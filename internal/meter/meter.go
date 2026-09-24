// Package meter extracts a Call record from a request/response pair.
//
// A Call carries numbers and hashes only. The text of the request is read
// once to compute shingles and a system-prompt hash, then dropped.
package meter

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/GreatPyreneseDad/loopmath-agent/internal/cost"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/shingle"
)

type Provider string

const (
	Anthropic Provider = "anthropic"
	OpenAI    Provider = "openai"
	Gemini    Provider = "gemini"
	Unknown   Provider = "unknown"
)

// Call is what the agent remembers about one model request.
type Call struct {
	ID         string        `json:"id"`
	At         time.Time     `json:"at"`
	Provider   Provider      `json:"provider"`
	Model      string        `json:"model"`
	Stream     bool          `json:"stream"`
	Status     int           `json:"status"`
	Latency    time.Duration `json:"latency_ns"`
	Usage      cost.Usage    `json:"usage"`
	USD        float64       `json:"usd"`
	PriceKnown bool          `json:"price_known"`

	// Shape of the request, no content.
	Messages      int    `json:"messages"`
	Tools         int    `json:"tools"`
	SystemHash    string `json:"system_hash"` // sha256 of system prompt (first 16 hex)
	FirstUserHash string `json:"first_user_hash"`
	RequestHash   string `json:"request_hash"` // sha256 of canonical body sans stream flag
	ContentChars  int    `json:"content_chars"`
	StopReason    string `json:"stop_reason,omitempty"`

	// Loop routing hints pulled from headers/body.
	LoopHint string `json:"loop_hint,omitempty"`

	// Not serialized: fingerprints for redundancy analysis.
	Shingles shingle.Set `json:"-"`
	// Not serialized, debug only.
	SystemPrefix string `json:"-"`
}

// DetectProvider by path.
func DetectProvider(path string) Provider {
	switch {
	case strings.Contains(path, ":generateContent"), strings.Contains(path, ":streamGenerateContent"), strings.Contains(path, "/models/gemini"):
		return Gemini
	case strings.Contains(path, "/v1/messages"):
		return Anthropic
	case strings.Contains(path, "/chat/completions"), strings.Contains(path, "/v1/responses"), strings.Contains(path, "/completions"):
		return OpenAI
	}
	return Unknown
}

func short(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:8])
}

// ---- request parsing ----

type anyMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// flatten pulls text out of a content field that may be a string or an
// array of blocks ({type:text,text} / {type:input_text,text} / tool blocks).
func flatten(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		for _, key := range []string{"text", "input", "content", "partial_json"} {
			if v, ok := b[key]; ok {
				var t string
				if json.Unmarshal(v, &t) == nil {
					sb.WriteString(t)
					sb.WriteByte('\n')
				} else {
					sb.Write(v)
					sb.WriteByte('\n')
				}
			}
		}
	}
	return sb.String()
}

// ParseRequest reads a JSON body and fills the request-side fields of c.
// It returns the (possibly rewritten) body: for OpenAI streams we inject
// stream_options.include_usage so the final chunk carries token counts.
func ParseRequest(c *Call, body []byte) (out []byte, rewritten bool) {
	out = body
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		c.RequestHash = short(body)
		return out, false
	}
	if m, ok := req["model"]; ok {
		json.Unmarshal(m, &c.Model)
	}
	if s, ok := req["stream"]; ok {
		json.Unmarshal(s, &c.Stream)
	}
	if t, ok := req["tools"]; ok {
		var arr []json.RawMessage
		if json.Unmarshal(t, &arr) == nil {
			c.Tools = len(arr)
		}
	}
	// Gemini: {contents:[{role,parts:[{text}]}], systemInstruction:{parts:[{text}]}}
	if c.Provider == Gemini {
		return parseGeminiRequest(c, body, req), false
	}
	// system: Anthropic top-level (string or blocks); OpenAI role=system/developer
	var systemText string
	if s, ok := req["system"]; ok {
		systemText = flatten(s)
	}
	if inst, ok := req["instructions"]; ok { // OpenAI Responses API
		systemText += flatten(inst)
	}
	var allText strings.Builder
	allText.WriteString(systemText)
	var msgs []anyMsg
	msgKey := "messages"
	if _, ok := req["input"]; ok && !hasKey(req, "messages") {
		msgKey = "input" // Responses API
	}
	if m, ok := req[msgKey]; ok {
		if json.Unmarshal(m, &msgs) != nil {
			// Responses API input may be a plain string
			var s string
			if json.Unmarshal(m, &s) == nil {
				msgs = []anyMsg{{Role: "user", Content: m}}
			}
		}
	}
	c.Messages = len(msgs)
	firstUser := ""
	for _, m := range msgs {
		txt := flatten(m.Content)
		switch m.Role {
		case "system", "developer":
			systemText += txt
		case "user":
			if firstUser == "" {
				firstUser = txt
			}
		}
		allText.WriteString(txt)
		allText.WriteByte('\n')
	}
	c.SystemHash = short([]byte(systemText))
	c.FirstUserHash = short([]byte(firstUser))
	c.ContentChars = allText.Len()
	c.Shingles = shingle.Of(allText.String())
	if len(systemText) > 0 {
		c.SystemPrefix = systemText[:min(len(systemText), 48)]
	}
	// request hash ignores stream + stream_options so retries match across modes
	delete(req, "stream")
	delete(req, "stream_options")
	if canon, err := json.Marshal(req); err == nil {
		c.RequestHash = short(canon)
	} else {
		c.RequestHash = short(body)
	}
	// loop hint from body metadata (Anthropic metadata.user_id, OpenAI user)
	if md, ok := req["metadata"]; ok {
		var m map[string]any
		if json.Unmarshal(md, &m) == nil {
			for _, k := range []string{"loop_id", "session_id", "trace_id", "user_id"} {
				if v, ok := m[k].(string); ok && v != "" {
					c.LoopHint = v
					break
				}
			}
		}
	}
	if c.LoopHint == "" {
		if u, ok := req["user"]; ok {
			json.Unmarshal(u, &c.LoopHint)
		}
	}
	// inject include_usage for OpenAI streams
	if c.Stream && c.Provider == OpenAI {
		var full map[string]json.RawMessage
		if json.Unmarshal(body, &full) == nil {
			so := map[string]any{"include_usage": true}
			if existing, ok := full["stream_options"]; ok {
				var e map[string]any
				if json.Unmarshal(existing, &e) == nil {
					for k, v := range e {
						so[k] = v
					}
					so["include_usage"] = true
				}
			}
			if b, err := json.Marshal(so); err == nil {
				full["stream_options"] = b
				if nb, err := json.Marshal(full); err == nil {
					return nb, true
				}
			}
		}
	}
	return out, false
}

func hasKey(m map[string]json.RawMessage, k string) bool { _, ok := m[k]; return ok }

type geminiContent struct {
	Role  string `json:"role"`
	Parts []struct {
		Text string `json:"text"`
	} `json:"parts"`
}

func (g geminiContent) text() string {
	var sb strings.Builder
	for _, p := range g.Parts {
		sb.WriteString(p.Text)
		sb.WriteByte('\n')
	}
	return sb.String()
}

func parseGeminiRequest(c *Call, body []byte, req map[string]json.RawMessage) []byte {
	var systemText, firstUser string
	var all strings.Builder
	if si, ok := req["systemInstruction"]; ok {
		var g geminiContent
		if json.Unmarshal(si, &g) == nil {
			systemText = g.text()
		}
	} else if si, ok := req["system_instruction"]; ok {
		var g geminiContent
		if json.Unmarshal(si, &g) == nil {
			systemText = g.text()
		}
	}
	all.WriteString(systemText)
	var contents []geminiContent
	if cs, ok := req["contents"]; ok {
		if json.Unmarshal(cs, &contents) != nil {
			var one geminiContent
			if json.Unmarshal(cs, &one) == nil {
				contents = []geminiContent{one}
			}
		}
	}
	c.Messages = len(contents)
	for _, g := range contents {
		t := g.text()
		if (g.Role == "user" || g.Role == "") && firstUser == "" {
			firstUser = t
		}
		all.WriteString(t)
	}
	if t, ok := req["tools"]; ok {
		var arr []json.RawMessage
		if json.Unmarshal(t, &arr) == nil {
			c.Tools = len(arr)
		}
	}
	c.SystemHash = short([]byte(systemText))
	c.FirstUserHash = short([]byte(firstUser))
	c.ContentChars = all.Len()
	c.Shingles = shingle.Of(all.String())
	if len(systemText) > 0 {
		c.SystemPrefix = systemText[:min(len(systemText), 48)]
	}
	c.RequestHash = short(append([]byte(c.Model+"|"), body...))
	return body
}

type geminiUsage struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
}

func applyGemini(c *Call, u geminiUsage) {
	if u.PromptTokenCount == 0 && u.CandidatesTokenCount == 0 {
		return
	}
	// cached is a subset of prompt; thoughts bill as output
	c.Usage.Input = u.PromptTokenCount - u.CachedContentTokenCount
	c.Usage.CacheRead = u.CachedContentTokenCount
	c.Usage.Output = u.CandidatesTokenCount + u.ThoughtsTokenCount
}

type geminiResp struct {
	UsageMetadata geminiUsage `json:"usageMetadata"`
	ModelVersion  string      `json:"modelVersion"`
	Candidates    []struct {
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
}

func applyGeminiResp(c *Call, r geminiResp) {
	applyGemini(c, r.UsageMetadata)
	if len(r.Candidates) > 0 && r.Candidates[0].FinishReason != "" {
		c.StopReason = r.Candidates[0].FinishReason
	}
	if r.ModelVersion != "" {
		c.Model = r.ModelVersion // exact version beats the alias in the path
	}
}

// ---- response parsing ----

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	InputTokens      int `json:"input_tokens"`  // Responses API
	OutputTokens     int `json:"output_tokens"` // Responses API
	PromptDetails    struct {
		Cached int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputDetails struct {
		Cached int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func applyAnthropic(c *Call, u anthropicUsage) {
	if u.InputTokens > 0 {
		c.Usage.Input = u.InputTokens
	}
	if u.OutputTokens > 0 {
		c.Usage.Output = u.OutputTokens
	}
	if u.CacheReadInputTokens > 0 {
		c.Usage.CacheRead = u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens > 0 {
		c.Usage.CacheWrite = u.CacheCreationInputTokens
	}
}

func applyOpenAI(c *Call, u openaiUsage) {
	in := u.PromptTokens
	if in == 0 {
		in = u.InputTokens
	}
	out := u.CompletionTokens
	if out == 0 {
		out = u.OutputTokens
	}
	cached := u.PromptDetails.Cached
	if cached == 0 {
		cached = u.InputDetails.Cached
	}
	// OpenAI reports cached as a subset of prompt tokens; split so Input is
	// the uncached remainder, matching Anthropic's accounting.
	c.Usage.Input = in - cached
	c.Usage.CacheRead = cached
	c.Usage.Output = out
}

// ParseResponse consumes a complete (non-stream) body.
func ParseResponse(c *Call, body []byte) {
	switch c.Provider {
	case Gemini:
		var one geminiResp
		if json.Unmarshal(body, &one) == nil && (one.UsageMetadata.PromptTokenCount > 0 || len(one.Candidates) > 0) {
			applyGeminiResp(c, one)
			return
		}
		var arr []geminiResp // streamGenerateContent without alt=sse returns a JSON array
		if json.Unmarshal(body, &arr) == nil {
			for _, r := range arr {
				applyGeminiResp(c, r) // usage is cumulative; last wins
			}
		}
	case Anthropic:
		var r struct {
			Usage      anthropicUsage `json:"usage"`
			StopReason string         `json:"stop_reason"`
			Model      string         `json:"model"`
		}
		if json.Unmarshal(body, &r) == nil {
			applyAnthropic(c, r.Usage)
			c.StopReason = r.StopReason
			if c.Model == "" {
				c.Model = r.Model
			}
		}
	default:
		var r struct {
			Usage   openaiUsage `json:"usage"`
			Model   string      `json:"model"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(body, &r) == nil {
			applyOpenAI(c, r.Usage)
			if len(r.Choices) > 0 {
				c.StopReason = r.Choices[0].FinishReason
			}
			if c.Model == "" {
				c.Model = r.Model
			}
		}
	}
}

// ParseSSE consumes a complete SSE stream body (already forwarded to the
// client) and extracts usage. It only looks at `data:` lines.
func ParseSSE(c *Call, body []byte) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		switch c.Provider {
		case Gemini:
			var r geminiResp
			if json.Unmarshal(data, &r) == nil {
				applyGeminiResp(c, r)
			}
		case Anthropic:
			var ev struct {
				Type    string `json:"type"`
				Message struct {
					Model string         `json:"model"`
					Usage anthropicUsage `json:"usage"`
				} `json:"message"`
				Usage anthropicUsage `json:"usage"`
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			switch ev.Type {
			case "message_start":
				applyAnthropic(c, ev.Message.Usage)
				if c.Model == "" {
					c.Model = ev.Message.Model
				}
			case "message_delta":
				applyAnthropic(c, ev.Usage)
				if ev.Delta.StopReason != "" {
					c.StopReason = ev.Delta.StopReason
				}
			}
		default:
			var ev struct {
				Usage    *openaiUsage `json:"usage"`
				Model    string       `json:"model"`
				Response *struct {
					Usage *openaiUsage `json:"usage"`
				} `json:"response"`
				Choices []struct {
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
			}
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			if ev.Usage != nil {
				applyOpenAI(c, *ev.Usage)
			}
			if ev.Response != nil && ev.Response.Usage != nil {
				applyOpenAI(c, *ev.Response.Usage)
			}
			if len(ev.Choices) > 0 && ev.Choices[0].FinishReason != "" {
				c.StopReason = ev.Choices[0].FinishReason
			}
			if c.Model == "" && ev.Model != "" {
				c.Model = ev.Model
			}
		}
	}
}
