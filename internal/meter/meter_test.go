package meter

import (
	"encoding/json"
	"testing"
)

func TestAnthropicRequestAndResponse(t *testing.T) {
	c := &Call{Provider: Anthropic}
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":100,"system":[{"type":"text","text":"You are terse."}],
	  "tools":[{"name":"a"},{"name":"b"}],
	  "messages":[{"role":"user","content":"hello world"},{"role":"assistant","content":[{"type":"text","text":"hi"}]}],
	  "metadata":{"user_id":"sess-1"}}`)
	out, rewritten := ParseRequest(c, body)
	if rewritten || string(out) != string(body) {
		t.Fatal("anthropic body must pass through untouched")
	}
	if c.Model != "claude-sonnet-4-5" || c.Tools != 2 || c.Messages != 2 || c.LoopHint != "sess-1" {
		t.Fatalf("bad parse: %+v", c)
	}
	if c.SystemHash == "" || c.FirstUserHash == "" || len(c.Shingles) == 0 {
		t.Fatal("hashes/shingles missing")
	}
	ParseResponse(c, []byte(`{"usage":{"input_tokens":1200,"output_tokens":40,"cache_read_input_tokens":1000,"cache_creation_input_tokens":0},"stop_reason":"end_turn"}`))
	if c.Usage.Input != 1200 || c.Usage.Output != 40 || c.Usage.CacheRead != 1000 || c.StopReason != "end_turn" {
		t.Fatalf("bad usage: %+v", c.Usage)
	}
}

func TestAnthropicSSE(t *testing.T) {
	c := &Call{Provider: Anthropic}
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-haiku-4-5\",\"usage\":{\"input_tokens\":500,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"x\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":77}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	ParseSSE(c, []byte(sse))
	if c.Usage.Input != 500 || c.Usage.Output != 77 || c.Model != "claude-haiku-4-5" || c.StopReason != "end_turn" {
		t.Fatalf("bad sse parse: %+v", c)
	}
}

func TestOpenAIStreamInjectsUsage(t *testing.T) {
	c := &Call{Provider: OpenAI}
	body := []byte(`{"model":"gpt-5-mini","stream":true,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"q"}],"user":"u-9"}`)
	out, rewritten := ParseRequest(c, body)
	if !rewritten {
		t.Fatal("openai stream should be rewritten")
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	so, _ := m["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("include_usage not injected: %s", out)
	}
	if c.LoopHint != "u-9" || !c.Stream {
		t.Fatalf("bad parse: %+v", c)
	}
	// request hash must not depend on stream flag
	c2 := &Call{Provider: OpenAI}
	ParseRequest(c2, []byte(`{"model":"gpt-5-mini","messages":[{"role":"system","content":"sys"},{"role":"user","content":"q"}],"user":"u-9"}`))
	if c.RequestHash != c2.RequestHash {
		t.Fatal("request hash should ignore stream flag")
	}
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":300,\"completion_tokens\":20,\"prompt_tokens_details\":{\"cached_tokens\":256}}}\n\n" +
		"data: [DONE]\n\n"
	ParseSSE(c, []byte(sse))
	if c.Usage.Input != 44 || c.Usage.CacheRead != 256 || c.Usage.Output != 20 || c.StopReason != "stop" {
		t.Fatalf("bad openai sse: %+v", c.Usage)
	}
}

func TestOpenAIResponsesAPI(t *testing.T) {
	c := &Call{Provider: OpenAI}
	ParseRequest(c, []byte(`{"model":"gpt-5","instructions":"be brief","input":"what is 2+2"}`))
	if c.Messages != 1 || c.Model != "gpt-5" {
		t.Fatalf("responses api parse: %+v", c)
	}
	ParseResponse(c, []byte(`{"usage":{"input_tokens":50,"output_tokens":5,"input_tokens_details":{"cached_tokens":0}}}`))
	if c.Usage.Input != 50 || c.Usage.Output != 5 {
		t.Fatalf("responses usage: %+v", c.Usage)
	}
}

func TestNoTextInSerializedCall(t *testing.T) {
	c := &Call{Provider: Anthropic}
	ParseRequest(c, []byte(`{"model":"m","system":"SECRET_SYSTEM_PROMPT","messages":[{"role":"user","content":"SECRET_USER_TEXT"}]}`))
	b, _ := json.Marshal(c)
	s := string(b)
	for _, leak := range []string{"SECRET_SYSTEM_PROMPT", "SECRET_USER_TEXT"} {
		if contains(s, leak) {
			t.Fatalf("serialized call leaks text: %s", s)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
