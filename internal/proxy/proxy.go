// Package proxy is the hot path: a transparent reverse proxy in front of
// model APIs that tees each exchange into the meter without holding the
// client's stream.
//
// Routing:
//
//	/anthropic/<path>  -> Anthropic upstream
//	/openai/<path>     -> OpenAI upstream
//	/<extra>/<path>    -> any -extra upstream
//	/<path>            -> guessed by path: /v1/messages => Anthropic, else OpenAI
//
// Streaming responses are flushed to the client as they arrive; a bounded
// copy is retained only until the response ends, parsed for usage, then
// freed. Nothing about the exchange is written to disk.
package proxy

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/GreatPyreneseDad/loopmath-agent/internal/config"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/loop"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/meter"
)

const (
	maxRequestBody  = 32 << 20 // 32MB
	maxResponseKeep = 64 << 20 // stop retaining after this; usage lives in head/tail anyway
)

type upstream struct {
	name string
	url  *url.URL
	prov meter.Provider
}

type Proxy struct {
	cfg    *config.Config
	engine *loop.Engine
	ups    map[string]*upstream // by prefix
	rp     *httputil.ReverseProxy

	mu       sync.Mutex
	inflight int
	served   int
}

func New(cfg *config.Config, engine *loop.Engine) (*Proxy, error) {
	p := &Proxy{cfg: cfg, engine: engine, ups: map[string]*upstream{}}
	add := func(name, raw string, prov meter.Provider) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		p.ups[name] = &upstream{name: name, url: u, prov: prov}
		return nil
	}
	if err := add("anthropic", cfg.AnthropicUpstream, meter.Anthropic); err != nil {
		return nil, err
	}
	if err := add("openai", cfg.OpenAIUpstream, meter.OpenAI); err != nil {
		return nil, err
	}
	if err := add("gemini", cfg.GeminiUpstream, meter.Gemini); err != nil {
		return nil, err
	}
	for k, v := range cfg.Extra {
		if err := add(k, v, meter.OpenAI); err != nil {
			return nil, err
		}
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute, // long generations
		DisableCompression:    true,             // we strip Accept-Encoding so bodies are parseable
	}
	p.rp = &httputil.ReverseProxy{
		Director:       p.director,
		Transport:      tr,
		FlushInterval:  -1, // flush immediately: SSE must not be buffered
		ModifyResponse: p.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if c, ok := r.Context().Value(ctxKey{}).(*exchange); ok {
				c.finish(http.StatusBadGateway, nil, false)
			}
			log.Printf("proxy: %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}
	return p, nil
}

type ctxKey struct{}

// exchange is the per-request state shared between Director, ModifyResponse
// and the tee body.
type exchange struct {
	p       *Proxy
	call    *meter.Call
	start   time.Time
	loopHdr string
	once    sync.Once
}

func (x *exchange) finish(status int, body []byte, sse bool) {
	x.once.Do(func() {
		c := x.call
		c.Status = status
		c.Latency = time.Since(x.start)
		if body != nil {
			if sse {
				meter.ParseSSE(c, body)
			} else {
				meter.ParseResponse(c, body)
			}
		}
		// only model calls become loop members; GET /v1/models, count_tokens,
		// files etc. pass through unmetered
		if c.Model != "" || c.Usage.Total() > 0 {
			x.p.engine.Observe(c, x.loopHdr)
		}
		x.p.mu.Lock()
		x.p.inflight--
		x.p.served++
		x.p.mu.Unlock()
	})
}

func newID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (p *Proxy) route(path string) (*upstream, string) {
	trimmed := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(trimmed, '/'); i > 0 {
		if u, ok := p.ups[trimmed[:i]]; ok {
			return u, trimmed[i:]
		}
	}
	switch meter.DetectProvider(path) {
	case meter.Anthropic:
		return p.ups["anthropic"], path
	case meter.Gemini:
		return p.ups["gemini"], path
	default:
		return p.ups["openai"], path
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(200)
		w.Write([]byte("ok\n"))
		return
	}
	path := r.URL.Path
	// /t/<token>/... — for SDKs that cannot set custom headers
	var pathTok string
	if strings.HasPrefix(path, "/t/") {
		rest := path[3:]
		if i := strings.IndexByte(rest, '/'); i > 0 {
			pathTok, path = rest[:i], rest[i:]
		}
	}
	if p.cfg.ProxyToken != "" {
		hdrTok := r.Header.Get("X-Loopmath-Token")
		if !tokenOK(p.cfg.ProxyToken, hdrTok) && !tokenOK(p.cfg.ProxyToken, pathTok) {
			w.Header().Set("WWW-Authenticate", "X-Loopmath-Token")
			http.Error(w, "loopmath: missing or invalid proxy token (X-Loopmath-Token header or /t/<token>/ path prefix)", http.StatusUnauthorized)
			return
		}
	}
	// /l/<loop-id>/... — per-item loop id in the base URL, also for header-less SDKs
	var pathLoop string
	if strings.HasPrefix(path, "/l/") {
		rest := path[3:]
		if i := strings.IndexByte(rest, '/'); i > 0 {
			pathLoop, path = rest[:i], rest[i:]
		}
	}
	up, rest := p.route(path)
	x := &exchange{p: p, start: time.Now(), loopHdr: r.Header.Get(p.cfg.LoopHeader)}
	if x.loopHdr == "" {
		x.loopHdr = pathLoop
	}
	x.call = &meter.Call{ID: newID(), At: x.start, Provider: up.prov}
	if up.prov == meter.Gemini {
		x.call.Model = geminiModelFromPath(rest)
	}
	if hint := r.Header.Get("X-Session-Id"); hint != "" && x.loopHdr == "" {
		x.loopHdr = hint
	}
	if tp := r.Header.Get("traceparent"); tp != "" && x.loopHdr == "" {
		// 00-<trace-id>-<span-id>-<flags>; trace id groups a run
		if parts := strings.Split(tp, "-"); len(parts) >= 2 {
			x.loopHdr = "tp:" + parts[1]
		}
	}

	// read + possibly rewrite body
	if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
		r.Body.Close()
		if err != nil || len(body) > maxRequestBody {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		nb, _ := meter.ParseRequest(x.call, body)
		r.Body = io.NopCloser(bytes.NewReader(nb))
		r.ContentLength = int64(len(nb))
		r.Header.Set("Content-Length", itoa(len(nb)))
	}

	p.mu.Lock()
	p.inflight++
	p.mu.Unlock()

	r = r.WithContext(withExchange(r.Context(), x))
	r.URL.Path = rest
	r.Header.Set("X-Loopmath-Upstream", up.name) // consumed by director
	p.rp.ServeHTTP(w, r)
}

// tokenOK is a constant-time compare so the proxy token can't be guessed a byte at a time.
func tokenOK(want, got string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// geminiModelFromPath: /v1beta/models/gemini-2.5-flash:generateContent -> gemini-2.5-flash
func geminiModelFromPath(path string) string {
	i := strings.Index(path, "/models/")
	if i < 0 {
		return ""
	}
	m := path[i+len("/models/"):]
	if j := strings.IndexAny(m, ":/?"); j >= 0 {
		m = m[:j]
	}
	return m
}

func itoa(n int) string {
	var b [20]byte
	i := len(b)
	if n == 0 {
		return "0"
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func (p *Proxy) director(r *http.Request) {
	up := p.ups[r.Header.Get("X-Loopmath-Upstream")]
	r.Header.Del("X-Loopmath-Upstream")
	r.Header.Del(p.cfg.LoopHeader)
	r.Header.Del("X-Loopmath-Token")
	r.Header.Del("Accept-Encoding") // identity so we can read usage
	r.URL.Scheme = up.url.Scheme
	r.URL.Host = up.url.Host
	if up.url.Path != "" && up.url.Path != "/" {
		r.URL.Path = strings.TrimSuffix(up.url.Path, "/") + r.URL.Path
	}
	r.Host = up.url.Host
	if _, ok := r.Header["User-Agent"]; !ok {
		r.Header.Set("User-Agent", "")
	}
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	x, ok := resp.Request.Context().Value(ctxKey{}).(*exchange)
	if !ok {
		return nil
	}
	sse := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	resp.Body = &teeBody{rc: resp.Body, x: x, status: resp.StatusCode, sse: sse}
	return nil
}

// teeBody streams upstream bytes to the client and keeps a bounded copy.
type teeBody struct {
	rc     io.ReadCloser
	x      *exchange
	status int
	sse    bool
	buf    bytes.Buffer
	full   bool
	done   bool
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 && !t.full {
		if t.buf.Len()+n > maxResponseKeep {
			t.full = true
		} else {
			t.buf.Write(p[:n])
		}
	}
	if err == io.EOF && !t.done {
		t.done = true
		t.x.finish(t.status, t.buf.Bytes(), t.sse)
	}
	return n, err
}

func (t *teeBody) Close() error {
	err := t.rc.Close()
	if !t.done {
		t.done = true
		// client disconnected or error: record what we have
		t.x.finish(t.status, t.buf.Bytes(), t.sse)
	}
	return err
}

// Stats for /metrics.
func (p *Proxy) Stats() (inflight, served int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight, p.served
}
