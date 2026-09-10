package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GreatPyreneseDad/loopmath-agent/internal/config"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/cost"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/findings"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/loop"
)

// fakeAnthropic answers /v1/messages, streaming if asked, and bills input
// tokens ≈ content length / 4 with zero cache reads (the bad case).
func fakeAnthropic(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.Error(w, "wrong path: "+r.URL.Path, 404)
			return
		}
		if r.Header.Get("X-Loopmath-Loop") != "" || r.Header.Get("X-Loopmath-Upstream") != "" {
			http.Error(w, "internal headers leaked upstream", 500)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
		}
		json.Unmarshal(body, &req)
		in := len(body) / 4
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":%d,\"output_tokens\":1}}}\n\n", in)
			f.Flush()
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"tok\"}}\n\n")
				f.Flush()
				time.Sleep(2 * time.Millisecond)
			}
			fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":30}}\n\n")
			fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":%d,"output_tokens":30}}`, in)
	}))
}

func fakeOpenAI(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream        bool           `json:"stream"`
			StreamOptions map[string]any `json:"stream_options"`
		}
		json.Unmarshal(body, &req)
		if req.Stream && req.StreamOptions["include_usage"] != true {
			http.Error(w, "include_usage not injected", 500)
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":10}}\n\n", len(body)/4)
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"gpt-5-mini","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":10}}`, len(body)/4)
	}))
}

func newStack(t *testing.T, anth, oai string) (*httptest.Server, *loop.Engine, *findings.Emitter) {
	cfg, err := config.Load([]string{
		"-anthropic", anth, "-openai", oai,
		"-findings-file", "",
		"-growth-min-calls", "5", "-growth-ratio", "3",
		"-cache-min-tokens", "2000", "-low-cache", "0.3",
		"-runaway-calls", "8", "-runaway-usd", "0",
		"-retry-repeats", "3",
	})
	if err != nil {
		t.Fatal(err)
	}
	emit, _ := findings.NewEmitter("", "", "", time.Second, 1000)
	eng := loop.New(cfg, cost.Default(), emit)
	px, err := New(cfg, eng)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(px), eng, emit
}

func post(t *testing.T, url string, hdr map[string]string, body any) (int, []byte) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func kinds(fs []findings.Finding) map[findings.Kind]int {
	m := map[findings.Kind]int{}
	for _, f := range fs {
		m[f.Kind]++
	}
	return m
}

func waitObserved(eng *loop.Engine, n int) {
	for i := 0; i < 200; i++ {
		_, calls, _, _ := eng.Totals()
		if calls >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSyntheticAgentLoop(t *testing.T) {
	anth := fakeAnthropic(t)
	defer anth.Close()
	oai := fakeOpenAI(t)
	defer oai.Close()
	srv, eng, emit := newStack(t, anth.URL, oai.URL)
	defer srv.Close()

	// A runaway agent: every turn appends a big tool result, no caching,
	// same system prompt + first user message (so fingerprint keying works).
	system := strings.Repeat("You are an agent that fixes bugs. ", 200)
	msgs := []map[string]any{{"role": "user", "content": "Fix the failing test in repo X. " + strings.Repeat("ctx ", 300)}}
	n := 10
	for i := 0; i < n; i++ {
		stream := i%2 == 0
		body := map[string]any{"model": "claude-sonnet-4-5", "max_tokens": 200, "system": system, "messages": msgs, "stream": stream}
		st, out := post(t, srv.URL+"/v1/messages", nil, body)
		if st != 200 {
			t.Fatalf("turn %d: status %d: %s", i, st, out)
		}
		if stream && !strings.Contains(string(out), "message_stop") {
			t.Fatalf("stream not passed through: %s", out)
		}
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": "running tool"},
			map[string]any{"role": "user", "content": fmt.Sprintf("tool result %d: ", i) + strings.Repeat(fmt.Sprintf("line%d ", i), 400)},
		)
	}
	waitObserved(eng, n)

	loops := eng.Snapshot(10)
	if len(loops) != 1 {
		t.Fatalf("expected one fingerprint loop, got %d: %+v", len(loops), loops)
	}
	l := loops[0]
	if l.Calls != n || l.KeyKind != "fingerprint" || l.USD <= 0 {
		t.Fatalf("loop: %+v", l)
	}
	if l.Redundancy < 0.5 {
		t.Fatalf("appended-context loop should be highly redundant, got %.2f", l.Redundancy)
	}
	k := kinds(emit.Recent(100))
	for _, want := range []findings.Kind{findings.ContextGrowth, findings.LowCacheRate, findings.RedundantContext, findings.RunawayLoop} {
		if k[want] == 0 {
			t.Errorf("missing finding %s; got %v", want, k)
		}
	}
	// no text leaks in findings
	for _, f := range emit.Recent(100) {
		b, _ := json.Marshal(f)
		if strings.Contains(string(b), "fixes bugs") || strings.Contains(string(b), "tool result") {
			t.Fatalf("finding leaks prompt text: %s", b)
		}
		if f.Schema != findings.SchemaVersion {
			t.Fatalf("bad schema %q", f.Schema)
		}
	}
}

func TestRetryStormAndHeaderKeying(t *testing.T) {
	anth := fakeAnthropic(t)
	defer anth.Close()
	oai := fakeOpenAI(t)
	defer oai.Close()
	srv, eng, emit := newStack(t, anth.URL, oai.URL)
	defer srv.Close()

	body := map[string]any{"model": "gpt-5-mini", "messages": []map[string]any{{"role": "user", "content": "same thing"}}}
	for i := 0; i < 3; i++ {
		st, _ := post(t, srv.URL+"/openai/v1/chat/completions", map[string]string{"X-Loopmath-Loop": "job-42"}, body)
		if st != 200 {
			t.Fatalf("status %d", st)
		}
	}
	// streaming variant too, via bare path (auto-routes to openai)
	body["stream"] = true
	st, out := post(t, srv.URL+"/v1/chat/completions", map[string]string{"X-Loopmath-Loop": "job-42"}, body)
	if st != 200 || !strings.Contains(string(out), "[DONE]") {
		t.Fatalf("openai stream: %d %s", st, out)
	}
	waitObserved(eng, 4)
	l, ok := eng.Get("h:job-42")
	if !ok || l.Calls != 4 || l.KeyKind != "header" {
		t.Fatalf("header-keyed loop: %+v ok=%v", l, ok)
	}
	if kinds(emit.Recent(50))[findings.RetryStorm] == 0 {
		t.Fatalf("expected retry_storm, got %v", kinds(emit.Recent(50)))
	}
}

func TestUnknownModelAndUpstreamError(t *testing.T) {
	anth := fakeAnthropic(t)
	defer anth.Close()
	srv, eng, emit := newStack(t, anth.URL, "http://127.0.0.1:1") // openai upstream dead
	defer srv.Close()

	st, _ := post(t, srv.URL+"/v1/messages", nil, map[string]any{"model": "claude-future-9", "messages": []map[string]any{{"role": "user", "content": "x"}}})
	if st != 200 {
		t.Fatalf("status %d", st)
	}
	st, _ = post(t, srv.URL+"/v1/chat/completions", nil, map[string]any{"model": "gpt-5-mini", "messages": []map[string]any{{"role": "user", "content": "x"}}})
	if st != 502 {
		t.Fatalf("dead upstream should 502, got %d", st)
	}
	waitObserved(eng, 2)
	if kinds(emit.Recent(50))[findings.UnknownPrice] == 0 {
		t.Fatalf("expected unknown_price, got %v", kinds(emit.Recent(50)))
	}
	inflight, served := srv.Config.Handler.(*Proxy).Stats()
	if inflight != 0 || served != 2 {
		t.Fatalf("stats inflight=%d served=%d", inflight, served)
	}
}
