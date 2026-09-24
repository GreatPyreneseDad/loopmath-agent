package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func run(t *testing.T, s *Server, msgs ...string) []map[string]any {
	var in bytes.Buffer
	for _, m := range msgs {
		in.WriteString(m + "\n")
	}
	var out bytes.Buffer
	s.in, s.out = &in, &out
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad json line %q: %v", line, err)
		}
		resps = append(resps, m)
	}
	return resps
}

func text(r map[string]any) string {
	res := r["result"].(map[string]any)
	c := res["content"].([]any)[0].(map[string]any)
	return c["text"].(string)
}

func TestHandshakeAndTools(t *testing.T) {
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Write([]byte("ok"))
		case "/metrics":
			w.Write([]byte("loopmath_loops 3\nloopmath_usd_total 1.25\n"))
		case "/v1/findings":
			w.Write([]byte(`{"findings":[{"kind":"runaway_loop","loop_usd":9},{"kind":"low_cache_rate","loop_usd":2}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer admin.Close()
	s := New(admin.URL, ":8787", "127.0.0.1:8788", "", "test")

	resps := run(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"loopmath_status","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"loopmath_findings","arguments":{"kind":"runaway_loop"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"loopmath_setup_env","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"nope"}`,
	)
	if len(resps) != 6 { // notification gets no reply
		t.Fatalf("want 6 responses, got %d", len(resps))
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != protocolVersion || !strings.Contains(init["instructions"].(string), "gitdr.ai/agent.md") {
		t.Fatalf("bad initialize: %v", init)
	}
	tools := resps[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 6 {
		t.Fatalf("want 6 tools, got %d", len(tools))
	}
	if !strings.Contains(text(resps[2]), "loopmath_usd_total 1.25") {
		t.Fatalf("status: %s", text(resps[2]))
	}
	f := text(resps[3])
	if !strings.Contains(f, "runaway_loop") || strings.Contains(f, "low_cache_rate") {
		t.Fatalf("kind filter failed: %s", f)
	}
	if !strings.Contains(text(resps[4]), "ANTHROPIC_BASE_URL=http://localhost:8787") {
		t.Fatalf("env block: %s", text(resps[4]))
	}
	if resps[5]["error"] == nil {
		t.Fatal("unknown method should error")
	}
}

func TestNotRunningGuidesToStart(t *testing.T) {
	s := New("http://127.0.0.1:1", ":8787", "127.0.0.1:8788", "", "test")
	resps := run(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"loopmath_findings","arguments":{}}}`)
	if !strings.Contains(text(resps[0]), "loopmath_start") {
		t.Fatalf("should point at loopmath_start: %s", text(resps[0]))
	}
}

func TestHTTPTransport(t *testing.T) {
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Loopmath-Token") != "adm" {
			http.Error(w, "no token", 401)
			return
		}
		switch r.URL.Path {
		case "/healthz":
			w.Write([]byte("ok"))
		case "/metrics":
			w.Write([]byte("loopmath_loops 2\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer admin.Close()
	os.Setenv("LOOPMATH_ADMIN_TOKEN", "adm")
	defer os.Unsetenv("LOOPMATH_ADMIN_TOKEN")
	s := New(admin.URL, ":8080", ":8080", "", "test").Hosted("https://loopmath-x.fly.dev", "ptok")
	h := httptest.NewServer(HTTPHandler{S: s, Token: "adm"})
	defer h.Close()

	post := func(url, body string, hdr map[string]string) (int, string, http.Header) {
		req, _ := http.NewRequest("POST", url, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	if st, _, _ := post(h.URL+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, nil); st != 401 {
		t.Fatalf("no token should 401, got %d", st)
	}
	st, body, hdr := post(h.URL+"/mcp/t/adm", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil)
	if st != 200 || hdr.Get("Mcp-Session-Id") == "" || !strings.Contains(body, protocolVersion) {
		t.Fatalf("initialize via path token: %d %s %v", st, body, hdr)
	}
	if st, _, _ := post(h.URL+"/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, map[string]string{"Authorization": "Bearer adm"}); st != 202 {
		t.Fatalf("notification should 202, got %d", st)
	}
	_, body, _ = post(h.URL+"/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"loopmath_setup_env","arguments":{}}}`, map[string]string{"X-Loopmath-Token": "adm"})
	if !strings.Contains(body, "https://loopmath-x.fly.dev/t/ptok") || strings.Contains(body, "localhost") {
		t.Fatalf("hosted env block wrong: %s", body)
	}
	_, body, _ = post(h.URL+"/mcp/t/adm", `[{"jsonrpc":"2.0","id":3,"method":"ping"},{"jsonrpc":"2.0","id":4,"method":"tools/list"}]`, nil)
	var arr []any
	if json.Unmarshal([]byte(body), &arr) != nil || len(arr) != 2 {
		t.Fatalf("batch: %s", body)
	}
	if st, _, _ := post(h.URL+"/mcp/t/adm", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"loopmath_status","arguments":{}}}`, nil); st != 200 {
		t.Fatalf("status call %d", st)
	}
}
