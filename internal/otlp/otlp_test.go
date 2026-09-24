package otlp

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
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

// ---- tiny protobuf encoder for tests ----

func pvarint(v uint64) []byte {
	var b [10]byte
	n := binary.PutUvarint(b[:], v)
	return b[:n]
}
func pfield(num, wt int) []byte { return pvarint(uint64(num<<3 | wt)) }
func pbytes(num int, b []byte) []byte {
	return append(append(pfield(num, 2), pvarint(uint64(len(b)))...), b...)
}
func pstr(num int, s string) []byte { return pbytes(num, []byte(s)) }
func pfixed64(num int, v uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	return append(pfield(num, 1), b[:]...)
}
func pvar(num int, v uint64) []byte { return append(pfield(num, 0), pvarint(v)...) }

func anyStr(s string) []byte { return pstr(1, s) }
func anyInt(i int64) []byte  { return pvar(3, uint64(i)) }
func kv(k string, val []byte) []byte {
	return pbytes(1, append(pstr(1, k), pbytes(2, val)...))
}

// kvRaw builds a KeyValue message body (without outer tag) for field 9 embedding.
func kvRaw(k string, val []byte) []byte { return append(pstr(1, k), pbytes(2, val)...) }

func protoRequest(spans ...[]byte) []byte {
	var scope []byte
	scope = append(scope, pbytes(1, pstr(1, "test-instrumentation"))...)
	for _, s := range spans {
		scope = append(scope, pbytes(2, s)...)
	}
	res := pbytes(1, kvRaw("service.name", anyStr("agent-svc")))
	rs := append(pbytes(1, res), pbytes(2, scope)...)
	return pbytes(1, rs)
}

func mkSpan(traceID string, start time.Time, attrs map[string][]byte) []byte {
	var sp []byte
	sp = append(sp, pbytes(1, []byte(traceID))...)
	sp = append(sp, pbytes(2, []byte("spanid00"))...)
	sp = append(sp, pstr(5, "chat claude-sonnet-4-5")...)
	sp = append(sp, pfixed64(7, uint64(start.UnixNano()))...)
	sp = append(sp, pfixed64(8, uint64(start.Add(800*time.Millisecond).UnixNano()))...)
	for k, v := range attrs {
		sp = append(sp, pbytes(9, kvRaw(k, v))...)
	}
	sp = append(sp, pbytes(15, pvar(3, 1))...)
	return sp
}

func TestProtoDecode(t *testing.T) {
	start := time.Now()
	req := protoRequest(mkSpan("0123456789abcdef", start, map[string][]byte{
		"gen_ai.system":              anyStr("anthropic"),
		"gen_ai.request.model":       anyStr("claude-sonnet-4-5"),
		"gen_ai.usage.input_tokens":  anyInt(1200),
		"gen_ai.usage.output_tokens": anyInt(40),
		"gen_ai.conversation.id":     anyStr("conv-7"),
		"gen_ai.request.temperature": pfixed64(4, 0x3FE0000000000000), // AnyValue.double_value = 0.5
	}))
	spans, err := DecodeProto(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	s := spans[0]
	if s.Attrs["gen_ai.usage.input_tokens"] != "1200" || s.Attrs["gen_ai.system"] != "anthropic" || s.Resource["service.name"] != "agent-svc" || s.Scope != "test-instrumentation" {
		t.Fatalf("attrs: %+v", s)
	}
	if s.Attrs["gen_ai.request.temperature"] != "0.5" {
		t.Fatalf("double decode: %q", s.Attrs["gen_ai.request.temperature"])
	}
	c, hint, ok := ToCall(&s)
	if !ok || c.Usage.Input != 1200 || c.Usage.Output != 40 || hint != "conv-7" || c.Provider != "anthropic" || c.Latency < 700*time.Millisecond {
		t.Fatalf("call: %+v hint=%q", c, hint)
	}
}

func TestProtoTruncated(t *testing.T) {
	req := protoRequest(mkSpan("0123456789abcdef", time.Now(), map[string][]byte{"gen_ai.system": anyStr("openai")}))
	if _, err := DecodeProto(req[:len(req)-5]); err == nil {
		t.Fatal("truncated input should error")
	}
}

func jsonReq(spans ...map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   map[string]any{"attributes": []any{map[string]any{"key": "service.name", "value": map[string]any{"stringValue": "svc"}}}},
		"scopeSpans": []any{map[string]any{"scope": map[string]any{"name": "openllmetry"}, "spans": spans}},
	}}})
	return b
}

func attr(k string, v any) map[string]any {
	switch x := v.(type) {
	case string:
		return map[string]any{"key": k, "value": map[string]any{"stringValue": x}}
	case int:
		return map[string]any{"key": k, "value": map[string]any{"intValue": fmt.Sprint(x)}}
	}
	return nil
}

func TestJSONOpenAIWithCachedSubset(t *testing.T) {
	s := map[string]any{
		"traceId": "AAECAwQFBgcICQoLDA0ODw==", "spanId": "AAECAwQFBgc=", "name": "chat gpt-5-mini",
		"startTimeUnixNano": "1700000000000000000", "endTimeUnixNano": "1700000001000000000",
		"attributes": []any{
			attr("gen_ai.provider.name", "openai"), attr("gen_ai.request.model", "gpt-5-mini"),
			attr("gen_ai.usage.input_tokens", 3000), attr("gen_ai.usage.output_tokens", 50),
			attr("gen_ai.usage.cached_tokens", 2048), attr("session.id", "sess-1"),
		},
		"status": map[string]any{"code": "STATUS_CODE_OK"},
	}
	spans, err := DecodeJSON(jsonReq(s))
	if err != nil || len(spans) != 1 {
		t.Fatal(err, len(spans))
	}
	if spans[0].TraceID != "000102030405060708090a0b0c0d0e0f" {
		t.Fatalf("base64 trace id not converted: %s", spans[0].TraceID)
	}
	c, hint, ok := ToCall(&spans[0])
	if !ok || c.Usage.Input != 952 || c.Usage.CacheRead != 2048 || hint != "sess-1" || c.Provider != "openai" {
		t.Fatalf("openai cached subset: %+v hint=%s", c.Usage, hint)
	}
}

func TestNonGenAISpanIgnored(t *testing.T) {
	s := Span{Attrs: map[string]string{"http.method": "GET", "db.system": "postgres"}}
	if _, _, ok := ToCall(&s); ok {
		t.Fatal("db span should be ignored")
	}
	tool := Span{Attrs: map[string]string{"gen_ai.operation.name": "execute_tool"}}
	if _, _, ok := ToCall(&tool); ok {
		t.Fatal("tool span should be ignored")
	}
}

func newEngine(t *testing.T) (*loop.Engine, *findings.Emitter) {
	cfg, err := config.Load([]string{"-findings-file", "", "-growth-min-calls", "5", "-cache-min-tokens", "2000", "-runaway-calls", "8", "-runaway-usd", "0"})
	if err != nil {
		t.Fatal(err)
	}
	emit, _ := findings.NewEmitter("", "", "", time.Second, 1000)
	return loop.New(cfg, cost.Default(), emit), emit
}

// A runaway agent reported via OpenLLMetry-style spans with flattened prompt
// content, gzip + protobuf, through the HTTP receiver.
func TestReceiverEndToEnd(t *testing.T) {
	eng, emit := newEngine(t)
	srv := httptest.NewServer(New(eng))
	defer srv.Close()

	system := strings.Repeat("You are an agent that fixes bugs. ", 150)
	history := "Fix the failing test. " + strings.Repeat("ctx ", 300)
	start := time.Now().Add(-time.Minute)
	for i := 0; i < 10; i++ {
		ctxTokens := int64(len(system+history) / 4)
		attrs := map[string][]byte{
			"gen_ai.system":              anyStr("Anthropic"),
			"gen_ai.request.model":       anyStr("claude-sonnet-4-5"),
			"gen_ai.usage.input_tokens":  anyInt(ctxTokens),
			"gen_ai.usage.output_tokens": anyInt(30),
			"gen_ai.prompt.0.role":       anyStr("system"),
			"gen_ai.prompt.0.content":    anyStr(system),
			"gen_ai.prompt.1.role":       anyStr("user"),
			"gen_ai.prompt.1.content":    anyStr(history),
		}
		body := protoRequest(mkSpan("traceid-run-0001", start.Add(time.Duration(i)*time.Second), attrs))
		var gz bytes.Buffer
		w := gzip.NewWriter(&gz)
		w.Write(body)
		w.Close()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/traces", &gz)
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Content-Encoding", "gzip")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		history += fmt.Sprintf(" tool result %d: ", i) + strings.Repeat(fmt.Sprintf("line%d ", i), 400)
	}
	loops := eng.Snapshot(10)
	if len(loops) != 1 || loops[0].Calls != 10 {
		t.Fatalf("expected one loop of 10 calls: %+v", loops)
	}
	if loops[0].KeyKind != "header" { // trace-id hint arrives via the header slot
		t.Fatalf("key kind %s", loops[0].KeyKind)
	}
	if loops[0].Redundancy < 0.5 {
		t.Fatalf("redundancy %.2f", loops[0].Redundancy)
	}
	kinds := map[findings.Kind]bool{}
	for _, f := range emit.Recent(100) {
		kinds[f.Kind] = true
		b, _ := json.Marshal(f)
		if strings.Contains(string(b), "fixes bugs") {
			t.Fatalf("prompt text leaked: %s", b)
		}
	}
	for _, k := range []findings.Kind{findings.ContextGrowth, findings.LowCacheRate, findings.RedundantContext, findings.RunawayLoop} {
		if !kinds[k] {
			t.Errorf("missing %s; got %v", k, kinds)
		}
	}
	sp, cl, dr := srv.Config.Handler.(*Receiver).Stats()
	if sp != 10 || cl != 10 || dr != 0 {
		t.Fatalf("stats %d %d %d", sp, cl, dr)
	}
}

func TestReceiverJSONNoContentStillCosts(t *testing.T) {
	eng, _ := newEngine(t)
	srv := httptest.NewServer(New(eng))
	defer srv.Close()
	s := map[string]any{"traceId": "0123456789abcdef0123456789abcdef", "spanId": "0123456789abcdef", "name": "chat",
		"startTimeUnixNano": "1700000000000000000", "endTimeUnixNano": "1700000000500000000",
		"attributes": []any{attr("gen_ai.system", "openai"), attr("gen_ai.request.model", "gpt-5"), attr("gen_ai.usage.input_tokens", 10000), attr("gen_ai.usage.output_tokens", 100)}}
	resp, err := http.Post(srv.URL+"/v1/traces", "application/json", bytes.NewReader(jsonReq(s, s)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_, calls, usd, _ := eng.Totals()
	if calls != 2 || usd <= 0 {
		t.Fatalf("calls=%d usd=%f", calls, usd)
	}
}
