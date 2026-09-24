// Package mcp is a stdio Model Context Protocol server so a coding agent
// can install, start, and read loopmath-agent without a human in the loop.
//
//	claude mcp add loopmath -- loopmath-agent mcp
//
// It is a thin client over the admin API of a running agent, plus one tool
// that starts the agent if none is running. Stdlib only; JSON-RPC 2.0 over
// newline-delimited stdio, protocol version 2025-06-18.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const protocolVersion = "2025-06-18"

type Server struct {
	adminURL  string
	proxyAddr string
	adminAddr string
	self      string // path to this binary, for spawning
	version   string
	client    *http.Client
	in        io.Reader
	out       io.Writer
}

func New(adminURL, proxyAddr, adminAddr, self, version string) *Server {
	return &Server{adminURL: strings.TrimSuffix(adminURL, "/"), proxyAddr: proxyAddr, adminAddr: adminAddr, self: self, version: version,
		client: &http.Client{Timeout: 5 * time.Second}, in: os.Stdin, out: os.Stdout}
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func (s *Server) tools() []tool {
	return []tool{
		{Name: "loopmath_status", Description: "Check whether loopmath-agent (the local proxy that meters model calls and emits loop-cost findings) is running, and what it has observed so far: loops, calls, USD, tokens, findings. Call this first.", InputSchema: obj(map[string]any{})},
		{Name: "loopmath_start", Description: "Start loopmath-agent in the background if it is not running. Returns the environment variables the application must set (ANTHROPIC_BASE_URL, OPENAI_BASE_URL) so its model calls route through the proxy. Idempotent.", InputSchema: obj(map[string]any{
			"findings_file": map[string]any{"type": "string", "description": "JSONL path for findings (default loopmath-findings.jsonl in cwd)"},
		})},
		{Name: "loopmath_setup_env", Description: "Return the exact environment variables and code snippets (shell, Python, Node, Docker) to route Anthropic and OpenAI calls through loopmath-agent. Use after loopmath_start.", InputSchema: obj(map[string]any{})},
		{Name: "loopmath_findings", Description: "Recent loop-cost findings, newest first: context_growth, low_cache_rate, redundant_context, runaway_loop, retry_storm, error_burst, unknown_price. Each has numeric evidence, est_savings_usd, and a recommendation. No prompt text is ever included.", InputSchema: obj(map[string]any{
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "default": 50},
			"kind":  map[string]any{"type": "string", "description": "filter by finding kind"},
		})},
		{Name: "loopmath_loops", Description: "Loops (agent runs / conversations) the proxy has seen, most recent first, with calls, USD, tokens, cache rate, redundancy.", InputSchema: obj(map[string]any{
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "default": 25},
		})},
		{Name: "loopmath_loop", Description: "One loop in full: per-call context tokens, per-call USD, latency series, dominant call.", InputSchema: obj(map[string]any{
			"id": map[string]any{"type": "string"},
		}, "id")},
	}
}

func (s *Server) write(v any) {
	b, _ := json.Marshal(v)
	s.out.Write(append(b, '\n'))
}

func (s *Server) reply(id json.RawMessage, result any) {
	s.write(rpcResp{JSONRPC: "2.0", ID: id, Result: result})
}
func (s *Server) fail(id json.RawMessage, code int, msg string) {
	s.write(rpcResp{JSONRPC: "2.0", ID: id, Error: &rpcErr{Code: code, Message: msg}})
}

// Run serves until stdin closes.
func (s *Server) Run(ctx context.Context) error {
	sc := bufio.NewScanner(s.in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.fail(nil, -32700, "parse error")
			continue
		}
		s.handle(ctx, req)
	}
	return sc.Err()
}

func (s *Server) handle(ctx context.Context, req rpcReq) {
	switch req.Method {
	case "initialize":
		s.reply(req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "loopmath-agent", "version": s.version},
			"instructions": "loopmath-agent meters every Anthropic/OpenAI call your application makes through a local proxy and emits loop-cost findings (context growth, missed prompt caching, redundant context, runaway loops, retry storms). " +
				"Typical flow: loopmath_status → loopmath_start if not running → loopmath_setup_env and apply the env vars to the application → run the workload → loopmath_findings. " +
				"Findings contain numbers and hashes only; no prompt text leaves the machine. Docs: https://gitdr.ai/agent.md",
		})
	case "notifications/initialized", "notifications/cancelled":
		// no response to notifications
	case "ping":
		s.reply(req.ID, map[string]any{})
	case "tools/list":
		s.reply(req.ID, map[string]any{"tools": s.tools()})
	case "tools/call":
		var p struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
		text, isErr := s.call(ctx, p.Name, p.Args)
		s.reply(req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr})
	case "resources/list":
		s.reply(req.ID, map[string]any{"resources": []any{}})
	case "prompts/list":
		s.reply(req.ID, map[string]any{"prompts": []any{}})
	default:
		if req.ID != nil {
			s.fail(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (s *Server) get(path string) ([]byte, error) {
	resp, err := s.client.Get(s.adminURL + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("admin API %s: HTTP %d", path, resp.StatusCode)
	}
	return b, nil
}

func (s *Server) running() bool {
	_, err := s.get("/healthz")
	return err == nil
}

func otlpHost(proxyHost string) string {
	h := proxyHost
	if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h + ":4318"
}

func (s *Server) envBlock() string {
	host := s.proxyAddr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	base := "http://" + host
	return fmt.Sprintf(`Set these in the application's environment (not in this MCP server):

  ANTHROPIC_BASE_URL=%s
  OPENAI_BASE_URL=%s/openai
  # any OpenAI-compatible SDK: base_url=%s/openai/v1

Optional: send X-Loopmath-Loop: <run-id> on each request to group calls into loops explicitly.
Python (anthropic):  Anthropic(base_url="%s")
Python (openai):     OpenAI(base_url="%s/openai/v1")
Node (anthropic):    new Anthropic({ baseURL: "%s" })
Node (openai):       new OpenAI({ baseURL: "%s/openai/v1" })
Claude Code:         export ANTHROPIC_BASE_URL=%s   (then restart claude)
Docker:              use host.docker.internal instead of localhost

API keys are unchanged; the proxy forwards them and never stores them.

No-proxy alternative: if the app already emits OpenTelemetry GenAI spans (OpenLLMetry, Langfuse, LiteLLM, Portkey, Vercel AI SDK, OTel Collector), set
  OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://%s/v1/traces
instead of changing base URLs. Same loops, same findings.
Admin API: %s  (findings: /v1/findings, loops: /v1/loops, metrics: /metrics)`,
		base, base, base, base, base, base, base, base, otlpHost(host), s.adminURL)
}

func (s *Server) call(ctx context.Context, name string, args json.RawMessage) (string, bool) {
	var a map[string]any
	json.Unmarshal(args, &a)
	switch name {
	case "loopmath_status":
		if !s.running() {
			return fmt.Sprintf("loopmath-agent is NOT running (no answer at %s). Call loopmath_start.", s.adminURL), false
		}
		m, err := s.get("/metrics")
		if err != nil {
			return err.Error(), true
		}
		return "loopmath-agent is running at " + s.adminURL + "\n\n" + string(m), false

	case "loopmath_start":
		if s.running() {
			return "already running.\n\n" + s.envBlock(), false
		}
		if s.self == "" {
			return "cannot locate the loopmath-agent binary to spawn; start it manually: loopmath-agent", true
		}
		cmdArgs := []string{"-proxy", s.proxyAddr, "-admin", s.adminAddr}
		if ff, ok := a["findings_file"].(string); ok && ff != "" {
			cmdArgs = append(cmdArgs, "-findings-file", ff)
		}
		cmd := exec.Command(s.self, cmdArgs...)
		logf, _ := os.OpenFile("loopmath-agent.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.Stdin = nil
		detach(cmd)
		if err := cmd.Start(); err != nil {
			return "failed to start: " + err.Error(), true
		}
		go cmd.Wait()
		for i := 0; i < 40; i++ {
			if s.running() {
				return fmt.Sprintf("started loopmath-agent (pid %d, log: loopmath-agent.log).\n\n%s", cmd.Process.Pid, s.envBlock()), false
			}
			time.Sleep(50 * time.Millisecond)
		}
		return "started but admin API not answering yet; check loopmath-agent.log (port in use?)", true

	case "loopmath_setup_env":
		return s.envBlock(), false

	case "loopmath_findings":
		limit := 50
		if v, ok := a["limit"].(float64); ok {
			limit = int(v)
		}
		b, err := s.get(fmt.Sprintf("/v1/findings?limit=%d", limit))
		if err != nil {
			return s.notRunning(err), true
		}
		if kind, ok := a["kind"].(string); ok && kind != "" {
			var r struct {
				Findings []map[string]any `json:"findings"`
			}
			json.Unmarshal(b, &r)
			var out []map[string]any
			for _, f := range r.Findings {
				if f["kind"] == kind {
					out = append(out, f)
				}
			}
			fb, _ := json.MarshalIndent(map[string]any{"findings": out, "filtered_kind": kind}, "", "  ")
			return string(fb), false
		}
		return string(b), false

	case "loopmath_loops":
		limit := 25
		if v, ok := a["limit"].(float64); ok {
			limit = int(v)
		}
		b, err := s.get(fmt.Sprintf("/v1/loops?limit=%d", limit))
		if err != nil {
			return s.notRunning(err), true
		}
		return string(b), false

	case "loopmath_loop":
		id, _ := a["id"].(string)
		if id == "" {
			return "id required", true
		}
		b, err := s.get("/v1/loops/" + id)
		if err != nil {
			return s.notRunning(err), true
		}
		return string(b), false
	}
	return "unknown tool: " + name, true
}

func (s *Server) notRunning(err error) string {
	if _, ok := err.(net.Error); ok || strings.Contains(err.Error(), "connection refused") {
		return "loopmath-agent is not running. Call loopmath_start first. (" + err.Error() + ")"
	}
	return err.Error()
}
