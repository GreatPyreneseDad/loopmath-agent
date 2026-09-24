// Package api is the cold path: a small read-only HTTP surface for humans,
// LoopMath, and gitdr.ai to pull what the agent knows. Bind it to localhost
// or behind your own auth; it exposes cost numbers, not prompts.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/GreatPyreneseDad/loopmath-agent/internal/cost"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/findings"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/loop"
)

type Stats interface{ Stats() (inflight, served int) }

type OTLPStats interface {
	Stats() (spans, calls, dropped int)
}

type Server struct {
	engine *loop.Engine
	emit   *findings.Emitter
	price  *cost.Table
	proxy  Stats
	otlp   OTLPStats
	mux    *http.ServeMux
}

func New(engine *loop.Engine, emit *findings.Emitter, price *cost.Table, proxy Stats, otlp OTLPStats) *Server {
	s := &Server{engine: engine, emit: emit, price: price, proxy: proxy, otlp: otlp, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	s.mux.HandleFunc("GET /v1/findings", s.findings)
	s.mux.HandleFunc("GET /findings", s.findings)
	s.mux.HandleFunc("GET /v1/loops", s.loops)
	s.mux.HandleFunc("GET /loops", s.loops)
	s.mux.HandleFunc("GET /v1/loops/{id...}", s.loop)
	s.mux.HandleFunc("GET /metrics", s.metrics)
	s.mux.HandleFunc("GET /", s.index)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func limit(r *http.Request, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 10000 {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func (s *Server) findings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"schema":   findings.SchemaVersion,
		"total":    s.emit.Total(),
		"findings": s.emit.Recent(limit(r, 200)),
	})
}

func (s *Server) loops(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"loops": s.engine.Snapshot(limit(r, 100))})
}

func (s *Server) loop(w http.ResponseWriter, r *http.Request) {
	l, ok := s.engine.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, l)
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	loops, calls, usd, u := s.engine.Totals()
	inflight, served := s.proxy.Stats()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# TYPE loopmath_loops gauge\nloopmath_loops %d\n", loops)
	fmt.Fprintf(w, "# TYPE loopmath_calls_total counter\nloopmath_calls_total %d\n", calls)
	fmt.Fprintf(w, "# TYPE loopmath_usd_total counter\nloopmath_usd_total %.6f\n", usd)
	fmt.Fprintf(w, "# TYPE loopmath_tokens_total counter\n")
	fmt.Fprintf(w, "loopmath_tokens_total{kind=\"input\"} %d\n", u.Input)
	fmt.Fprintf(w, "loopmath_tokens_total{kind=\"output\"} %d\n", u.Output)
	fmt.Fprintf(w, "loopmath_tokens_total{kind=\"cache_read\"} %d\n", u.CacheRead)
	fmt.Fprintf(w, "loopmath_tokens_total{kind=\"cache_write\"} %d\n", u.CacheWrite)
	fmt.Fprintf(w, "# TYPE loopmath_findings_total counter\nloopmath_findings_total %d\n", s.emit.Total())
	fmt.Fprintf(w, "# TYPE loopmath_proxy_inflight gauge\nloopmath_proxy_inflight %d\n", inflight)
	fmt.Fprintf(w, "# TYPE loopmath_proxy_served_total counter\nloopmath_proxy_served_total %d\n", served)
	if s.otlp != nil {
		sp, cl, dr := s.otlp.Stats()
		fmt.Fprintf(w, "# TYPE loopmath_otlp_spans_total counter\nloopmath_otlp_spans_total %d\n", sp)
		fmt.Fprintf(w, "# TYPE loopmath_otlp_calls_total counter\nloopmath_otlp_calls_total %d\n", cl)
		fmt.Fprintf(w, "# TYPE loopmath_otlp_ignored_spans_total counter\nloopmath_otlp_ignored_spans_total %d\n", dr)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, map[string]any{
		"agent":        "loopmath-agent/" + loop.Version,
		"prices_as_of": s.price.AsOf,
		"endpoints":    []string{"/v1/findings", "/v1/loops", "/v1/loops/{id}", "/metrics", "/healthz"},
		"ingest":       []string{"proxy: swap ANTHROPIC_BASE_URL / OPENAI_BASE_URL", "otlp: POST :4318/v1/traces (GenAI spans)"},
	})
}
