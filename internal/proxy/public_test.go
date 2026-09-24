package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GreatPyreneseDad/loopmath-agent/internal/api"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/config"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/cost"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/findings"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/loop"
)

// fakeGemini: /v1beta/models/<model>:generateContent (JSON) and
// :streamGenerateContent?alt=sse (SSE with cumulative usageMetadata).
func fakeGemini(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Loopmath-Token") != "" {
			http.Error(w, "loopmath token leaked upstream", 500)
			return
		}
		body, _ := io.ReadAll(r.Body)
		prompt := len(body) / 4
		if strings.Contains(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"a\"}]}}],\"usageMetadata\":{\"promptTokenCount\":%d,\"candidatesTokenCount\":3},\"modelVersion\":\"gemini-3.8-flash-001\"}\n\n", prompt)
			f.Flush()
			fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"b\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":%d,\"candidatesTokenCount\":9,\"cachedContentTokenCount\":%d},\"modelVersion\":\"gemini-3.8-flash-001\"}\n\n", prompt, prompt/2)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":12,"cachedContentTokenCount":%d,"thoughtsTokenCount":5},"modelVersion":"gemini-3.8-flash-001"}`, prompt, prompt/2)
	}))
}

func publicStack(t *testing.T, gem string) (*httptest.Server, *loop.Engine, *findings.Emitter) {
	cfg, err := config.Load([]string{
		"-gemini", gem, "-anthropic", "http://127.0.0.1:1", "-openai", "http://127.0.0.1:1",
		"-findings-file", "", "-proxy-token", "lm_secret", "-cache-min-tokens", "100", "-low-cache", "0.3",
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

func gemBody(item int) map[string]any {
	return map[string]any{
		"systemInstruction": map[string]any{"parts": []map[string]any{{"text": strings.Repeat("You price surplus IT hardware. ", 60)}}},
		"contents":          []map[string]any{{"role": "user", "parts": []map[string]any{{"text": fmt.Sprintf("Item %d label: Dell PowerEdge R740 2x Xeon Gold 6130 256GB", item)}}}},
	}
}

func TestProxyTokenRequired(t *testing.T) {
	gem := fakeGemini(t)
	defer gem.Close()
	srv, _, _ := publicStack(t, gem.URL)
	defer srv.Close()

	st, out := post(t, srv.URL+"/v1beta/models/gemini-3.8-flash:generateContent", nil, gemBody(1))
	if st != 401 || !strings.Contains(string(out), "X-Loopmath-Token") {
		t.Fatalf("want 401 with hint, got %d %s", st, out)
	}
	st, _ = post(t, srv.URL+"/v1beta/models/gemini-3.8-flash:generateContent", map[string]string{"X-Loopmath-Token": "wrong"}, gemBody(1))
	if st != 401 {
		t.Fatalf("wrong token should 401, got %d", st)
	}
	st, _ = post(t, srv.URL+"/v1beta/models/gemini-3.8-flash:generateContent", map[string]string{"X-Loopmath-Token": "lm_secret"}, gemBody(1))
	if st != 200 {
		t.Fatalf("header token should pass, got %d", st)
	}
	st, _ = post(t, srv.URL+"/t/lm_secret/v1beta/models/gemini-3.8-flash:generateContent", nil, gemBody(1))
	if st != 200 {
		t.Fatalf("path token should pass, got %d", st)
	}
	// healthz stays open for PaaS probes
	resp, _ := http.Get(srv.URL + "/healthz")
	if resp.StatusCode != 200 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
}

// Per-item loops via the base URL: /t/<token>/l/<item>/... — the pattern a
// serverless intake function uses when it constructs a client per request.
func TestGeminiPerItemLoopsViaPath(t *testing.T) {
	gem := fakeGemini(t)
	defer gem.Close()
	srv, eng, emit := publicStack(t, gem.URL)
	defer srv.Close()

	for item := 1; item <= 3; item++ {
		for step := 0; step < 4; step++ { // identify → price → describe → list
			path := ":generateContent"
			if step%2 == 1 {
				path = ":streamGenerateContent?alt=sse"
			}
			url := fmt.Sprintf("%s/t/lm_secret/l/item-%d/v1beta/models/gemini-3.8-flash%s", srv.URL, item, path)
			st, out := post(t, url, nil, gemBody(item))
			if st != 200 {
				t.Fatalf("item %d step %d: %d %s", item, step, st, out)
			}
			if step%2 == 1 && !strings.Contains(string(out), "STOP") {
				t.Fatalf("sse not passed through: %s", out)
			}
		}
	}
	waitObserved(eng, 12)
	loops := eng.Snapshot(10)
	if len(loops) != 3 {
		t.Fatalf("want 3 per-item loops, got %d", len(loops))
	}
	for _, l := range loops {
		if !strings.HasPrefix(l.ID, "h:item-") || l.Calls != 4 || l.Provider != "gemini" {
			t.Fatalf("loop %+v", l)
		}
		if l.Model != "gemini-3.8-flash-001" {
			t.Fatalf("model should come from modelVersion: %s", l.Model)
		}
		if l.Usage.CacheRead == 0 || l.Usage.Output <= 0 {
			t.Fatalf("usage not parsed: %+v", l.Usage)
		}
		if l.USD <= 0 {
			t.Fatalf("gemini should be priced: %f", l.USD)
		}
	}
	// same system prompt every item, 50% cached by the fake: cache_rate should be ~0.5 and low_cache_rate may fire
	for _, f := range emit.Recent(50) {
		b, _ := json.Marshal(f)
		if strings.Contains(string(b), "PowerEdge") || strings.Contains(string(b), "surplus IT") {
			t.Fatalf("finding leaks content: %s", b)
		}
	}
}

func TestSinglePortAdminAndToken(t *testing.T) {
	gem := fakeGemini(t)
	defer gem.Close()
	cfg, _ := config.Load([]string{"-gemini", gem.URL, "-findings-file", "", "-single", "-proxy-token", "p", "-admin-token", "a"})
	emit, _ := findings.NewEmitter("", "", "", time.Second, 10)
	eng := loop.New(cfg, cost.Default(), emit)
	px, _ := New(cfg, eng)
	// mirror main.go's single-port mux
	mux := http.NewServeMux()
	mux.Handle("/_loopmath/", http.StripPrefix("/_loopmath", adminForTest(eng, emit, px, "a")))
	mux.Handle("/", px)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post(t, srv.URL+"/t/p/v1beta/models/gemini-3.8-flash:generateContent", nil, gemBody(9))
	waitObserved(eng, 1)
	resp, _ := http.Get(srv.URL + "/_loopmath/v1/loops")
	if resp.StatusCode != 401 {
		t.Fatalf("admin without token should 401, got %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/_loopmath/v1/loops", nil)
	req.Header.Set("Authorization", "Bearer a")
	resp, _ = http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "gemini") {
		t.Fatalf("admin with token: %d %s", resp.StatusCode, b)
	}
}

func adminForTest(eng *loop.Engine, emit *findings.Emitter, px *Proxy, token string) http.Handler {
	return api.New(eng, emit, cost.Default(), px, nil, token)
}
