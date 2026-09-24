// Package otlp is the second ingest path: an OTLP/HTTP receiver for
// OpenTelemetry GenAI spans. Nothing sits on the hot path. Whatever already
// emits spans — OpenLLMetry, Langfuse, LiteLLM, Portkey, the OTel Collector,
// a custom gateway — points its exporter here and loopmath sees the same
// loops and emits the same findings as the proxy.
//
//	POST /v1/traces   Content-Type: application/x-protobuf | application/json
//	                  Content-Encoding: gzip (optional)
//
// Degradation is honest: if spans carry usage but no message content,
// cost/growth/cache/runaway findings work and redundancy is skipped; if they
// carry no conversation id, loops are keyed by trace id.
package otlp

import (
	"compress/gzip"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GreatPyreneseDad/loopmath-agent/internal/loop"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/meter"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/shingle"
)

const maxBody = 64 << 20

type Receiver struct {
	token  string // optional: X-Loopmath-Token (set OTEL_EXPORTER_OTLP_HEADERS=X-Loopmath-Token=...)
	engine *loop.Engine
	mu     sync.Mutex
	spans  int
	calls  int
	drops  int
}

func New(engine *loop.Engine) *Receiver { return &Receiver{engine: engine} }

// WithToken requires X-Loopmath-Token on POST /v1/traces.
func (r *Receiver) WithToken(t string) *Receiver { r.token = t; return r }

func (r *Receiver) Stats() (spans, calls, dropped int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spans, r.calls, r.drops
}

func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/v1/traces") {
		if req.URL.Path == "/healthz" {
			w.Write([]byte("ok\n"))
			return
		}
		http.Error(w, "POST /v1/traces", http.StatusNotFound)
		return
	}
	if r.token != "" && subtle.ConstantTimeCompare([]byte(r.token), []byte(req.Header.Get("X-Loopmath-Token"))) != 1 {
		http.Error(w, "loopmath: token required (OTEL_EXPORTER_OTLP_HEADERS=X-Loopmath-Token=...)", http.StatusUnauthorized)
		return
	}
	var body io.Reader = io.LimitReader(req.Body, maxBody+1)
	if strings.EqualFold(req.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	}
	b, err := io.ReadAll(body)
	if err != nil || len(b) > maxBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var spans []Span
	ct := req.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "json"):
		spans, err = DecodeJSON(b)
	default: // application/x-protobuf and anything unlabeled
		spans, err = DecodeProto(b)
		if err != nil && len(b) > 0 && b[0] == '{' {
			spans, err = DecodeJSON(b)
		}
	}
	if err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	calls := 0
	for i := range spans {
		if c, hint, ok := ToCall(&spans[i]); ok {
			r.engine.Observe(c, hint)
			calls++
		}
	}
	r.mu.Lock()
	r.spans += len(spans)
	r.calls += calls
	r.drops += len(spans) - calls
	r.mu.Unlock()

	// ExportTraceServiceResponse: empty object == full success
	if strings.Contains(ct, "json") {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(200)
}

// ---- semconv mapping ----

func first(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

func atoi(s string) int {
	if s == "" {
		return 0
	}
	if i, err := strconv.Atoi(s); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f)
	}
	return 0
}

func short(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// isGenAI decides whether a span is a model call.
func isGenAI(s *Span) bool {
	a := s.Attrs
	if first(a, "gen_ai.operation.name") != "" {
		switch a["gen_ai.operation.name"] {
		case "chat", "text_completion", "generate_content", "create_agent", "invoke_agent", "execute_tool", "embeddings":
			return a["gen_ai.operation.name"] != "execute_tool" && a["gen_ai.operation.name"] != "create_agent"
		}
	}
	if first(a, "gen_ai.usage.input_tokens", "gen_ai.usage.prompt_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.completion_tokens",
		"llm.usage.total_tokens", "llm.token_count.prompt", "ai.usage.promptTokens") != "" {
		return true
	}
	if first(a, "gen_ai.request.model", "gen_ai.response.model", "llm.request.model", "ai.model.id") != "" {
		return true
	}
	return false
}

// ToCall maps a GenAI span to a meter.Call. hint is the loop key hint
// (conversation/session id, else trace id). ok=false for non-model spans.
func ToCall(s *Span) (c *meter.Call, hint string, ok bool) {
	if !isGenAI(s) {
		return nil, "", false
	}
	a := s.Attrs
	c = &meter.Call{ID: s.SpanID}
	if c.ID == "" {
		c.ID = short(s.TraceID + s.Name + strconv.FormatUint(s.StartNano, 10))
	}
	if s.StartNano > 0 {
		c.At = time.Unix(0, int64(s.StartNano))
	} else {
		c.At = time.Now()
	}
	if s.EndNano > s.StartNano {
		c.Latency = time.Duration(s.EndNano - s.StartNano)
	}

	// provider
	sys := strings.ToLower(first(a, "gen_ai.provider.name", "gen_ai.system", "llm.system", "llm.vendor", "ai.provider"))
	switch {
	case strings.Contains(sys, "anthropic"), strings.Contains(sys, "claude"):
		c.Provider = meter.Anthropic
	case strings.Contains(sys, "openai"), strings.Contains(sys, "azure"):
		c.Provider = meter.OpenAI
	case sys != "":
		c.Provider = meter.Provider(sys)
	default:
		c.Provider = meter.Unknown
	}

	c.Model = first(a, "gen_ai.response.model", "gen_ai.request.model", "llm.response.model", "llm.request.model", "ai.model.id", "model")
	if c.Provider == meter.Unknown {
		m := strings.ToLower(c.Model)
		switch {
		case strings.HasPrefix(m, "claude"):
			c.Provider = meter.Anthropic
		case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
			c.Provider = meter.OpenAI
		}
	}

	// usage — accept the current semconv, the older one, OpenLLMetry, Vercel AI SDK, and vendor-prefixed cache keys
	in := atoi(first(a, "gen_ai.usage.input_tokens", "gen_ai.usage.prompt_tokens", "llm.usage.prompt_tokens", "llm.token_count.prompt", "ai.usage.promptTokens"))
	out := atoi(first(a, "gen_ai.usage.output_tokens", "gen_ai.usage.completion_tokens", "llm.usage.completion_tokens", "llm.token_count.completion", "ai.usage.completionTokens"))
	cr := atoi(first(a, "gen_ai.usage.cache_read.input_tokens", "gen_ai.usage.cache_read_input_tokens", "gen_ai.usage.cached_tokens", "gen_ai.usage.cached_input_tokens",
		"anthropic.usage.cache_read_input_tokens", "openai.usage.cached_tokens", "llm.usage.cache_read_input_tokens", "ai.usage.cachedInputTokens"))
	cw := atoi(first(a, "gen_ai.usage.cache_creation.input_tokens", "gen_ai.usage.cache_creation_input_tokens", "anthropic.usage.cache_creation_input_tokens", "llm.usage.cache_creation_input_tokens"))
	// OpenAI-style: cached is a subset of prompt tokens. Anthropic-style: input excludes cache reads.
	// If the provider is OpenAI (or unknown) and cached <= in, treat as subset.
	if cr > 0 && c.Provider != meter.Anthropic && cr <= in {
		in -= cr
	}
	c.Usage.Input, c.Usage.Output, c.Usage.CacheRead, c.Usage.CacheWrite = in, out, cr, cw

	// status
	if code := atoi(first(a, "http.response.status_code", "http.status_code")); code > 0 {
		c.Status = code
	} else if s.Status == 2 {
		c.Status = 500
	} else {
		c.Status = 200
	}
	c.StopReason = first(a, "gen_ai.response.finish_reasons", "gen_ai.response.finish_reason", "llm.response.finish_reason")

	// content (optional): current semconv puts messages in attributes; older
	// semconv used events gen_ai.system.message / gen_ai.user.message /
	// gen_ai.content.prompt; OpenLLMetry uses gen_ai.prompt.N.content.
	var system, firstUser strings.Builder
	var all strings.Builder
	if v := first(a, "gen_ai.system_instructions", "gen_ai.request.system", "llm.system_prompt"); v != "" {
		system.WriteString(v)
	}
	if v := first(a, "gen_ai.input.messages", "gen_ai.prompt", "gen_ai.content.prompt", "llm.prompts", "input", "ai.prompt", "ai.prompt.messages"); v != "" {
		all.WriteString(v)
		extractRoles(v, &system, &firstUser)
	}
	// OpenLLMetry flattened: gen_ai.prompt.0.role / gen_ai.prompt.0.content
	for i := 0; i < 512; i++ {
		p := "gen_ai.prompt." + strconv.Itoa(i) + "."
		content, ok := a[p+"content"]
		if !ok {
			if i > 0 {
				break
			}
			continue
		}
		c.Messages++
		all.WriteString(content)
		all.WriteByte('\n')
		switch a[p+"role"] {
		case "system", "developer":
			system.WriteString(content)
		case "user":
			if firstUser.Len() == 0 {
				firstUser.WriteString(content)
			}
		}
	}
	for _, ev := range s.Events {
		content := first(ev.Attrs, "gen_ai.event.content", "content", "body")
		if content == "" {
			continue
		}
		all.WriteString(content)
		all.WriteByte('\n')
		switch ev.Name {
		case "gen_ai.system.message":
			system.WriteString(content)
		case "gen_ai.user.message":
			if firstUser.Len() == 0 {
				firstUser.WriteString(content)
			}
		case "gen_ai.content.prompt":
			extractRoles(content, &system, &firstUser)
		}
	}
	if all.Len() > 0 {
		c.Shingles = shingle.Of(system.String() + "\n" + all.String())
		c.ContentChars = all.Len()
		c.RequestHash = short(c.Model + "|" + all.String())
	} else {
		// no content: hash on what we have so retry_storm still needs identical spans, which won't happen — effectively disabled
		c.RequestHash = short(c.ID)
	}
	c.SystemHash = short(system.String())
	c.FirstUserHash = short(firstUser.String())
	if system.Len() > 0 {
		c.SystemPrefix = system.String()[:min(48, system.Len())]
	}
	if n := atoi(first(a, "gen_ai.request.tool_count")); n > 0 {
		c.Tools = n
	} else if v, ok := a["gen_ai.request.tools"]; ok {
		c.Tools = strings.Count(v, "name:") + strings.Count(v, `"name"`)
	}

	// loop hint: explicit conversation/session > agent id > trace
	hint = first(a, "gen_ai.conversation.id", "session.id", "langfuse.session.id", "langfuse.trace.session_id", "conversation_id", "session_id", "thread.id", "ai.telemetry.metadata.sessionId")
	if hint == "" {
		hint = first(s.Resource, "session.id")
	}
	if hint == "" && s.TraceID != "" {
		hint = "tp:" + s.TraceID
	}
	return c, hint, true
}

// extractRoles pulls system / first user text out of a JSON messages array
// when the attribute holds one; otherwise leaves the builders untouched.
func extractRoles(v string, system, firstUser *strings.Builder) {
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Parts   json.RawMessage `json:"parts"`
	}
	if json.Unmarshal([]byte(v), &msgs) != nil {
		return
	}
	for _, m := range msgs {
		raw := m.Content
		if len(raw) == 0 {
			raw = m.Parts
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			s = string(raw)
		}
		switch m.Role {
		case "system", "developer":
			system.WriteString(s)
		case "user":
			if firstUser.Len() == 0 {
				firstUser.WriteString(s)
			}
		}
	}
}
