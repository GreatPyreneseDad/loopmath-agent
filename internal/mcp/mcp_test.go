package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
