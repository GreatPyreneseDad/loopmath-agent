// Package config loads agent configuration from flags and environment.
//
// Design rule: a customer should be able to run the agent with zero config
// and swap one env var in their app (base_url). Everything else has a default.
package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Listeners
	ProxyAddr string // hot path: where apps point base_url
	AdminAddr string // cold path: /findings, /loops, /metrics, /healthz
	OTLPAddr  string // second ingest: OTLP/HTTP GenAI spans on /v1/traces ("" disables)

	// Upstreams
	AnthropicUpstream string
	OpenAIUpstream    string
	// Extra upstreams: prefix=url, e.g. "azure=https://x.openai.azure.com"
	Extra map[string]string

	// Loop grouping
	LoopHeader     string        // explicit loop id header, default X-Loopmath-Loop
	LoopIdleWindow time.Duration // gap that closes a loop with no explicit id
	MaxLoops       int           // LRU bound on tracked loops

	// Findings
	FindingsFile string // JSONL append; "" disables
	SinkURL      string // POST findings here; "" disables
	SinkToken    string // bearer for sink
	SinkFlush    time.Duration

	// Cost
	PriceFile string // JSON override of the built-in price table

	// Thresholds
	ContextGrowthRatio float64 // input tokens last/first over a loop
	MinCallsForGrowth  int
	LowCacheRate       float64 // cache_read / cacheable input
	MinCacheableTokens int
	RedundancyRatio    float64 // shingles already seen in loop
	RunawayCalls       int
	RunawayUSD         float64
	RetryStormRepeats  int

	// Privacy
	// Never set true in production. When true, loop fingerprints include a
	// short prefix of the system prompt in /loops for debugging. Findings
	// never carry text regardless.
	Debug bool
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envF(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envI(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func envD(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// Load parses flags (which override env, which override defaults).
func Load(args []string) (*Config, error) {
	c := &Config{Extra: map[string]string{}}
	fs := flag.NewFlagSet("loopmath-agent", flag.ContinueOnError)

	fs.StringVar(&c.ProxyAddr, "proxy", env("LOOPMATH_PROXY_ADDR", ":8787"), "proxy listen address (apps point base_url here)")
	fs.StringVar(&c.AdminAddr, "admin", env("LOOPMATH_ADMIN_ADDR", "127.0.0.1:8788"), "admin listen address (/findings /loops /metrics)")
	fs.StringVar(&c.OTLPAddr, "otlp", env("LOOPMATH_OTLP_ADDR", ":4318"), "OTLP/HTTP receiver address for GenAI spans (empty disables)")
	fs.StringVar(&c.AnthropicUpstream, "anthropic", env("LOOPMATH_ANTHROPIC_UPSTREAM", "https://api.anthropic.com"), "Anthropic upstream")
	fs.StringVar(&c.OpenAIUpstream, "openai", env("LOOPMATH_OPENAI_UPSTREAM", "https://api.openai.com"), "OpenAI-compatible upstream")
	var extra string
	fs.StringVar(&extra, "extra", env("LOOPMATH_EXTRA_UPSTREAMS", ""), "extra upstreams: name=url,name=url (served at /name/...)")

	fs.StringVar(&c.LoopHeader, "loop-header", env("LOOPMATH_LOOP_HEADER", "X-Loopmath-Loop"), "header carrying an explicit loop id")
	fs.DurationVar(&c.LoopIdleWindow, "loop-idle", envD("LOOPMATH_LOOP_IDLE", 10*time.Minute), "idle gap that closes an implicit loop")
	fs.IntVar(&c.MaxLoops, "max-loops", envI("LOOPMATH_MAX_LOOPS", 10000), "max tracked loops in memory")

	fs.StringVar(&c.FindingsFile, "findings-file", env("LOOPMATH_FINDINGS_FILE", "loopmath-findings.jsonl"), "append findings JSONL here (empty disables)")
	fs.StringVar(&c.SinkURL, "sink", env("LOOPMATH_SINK_URL", ""), "POST findings to this URL (e.g. https://gitdr.ai/api/v1/findings)")
	fs.StringVar(&c.SinkToken, "sink-token", env("LOOPMATH_SINK_TOKEN", ""), "bearer token for sink")
	fs.DurationVar(&c.SinkFlush, "sink-flush", envD("LOOPMATH_SINK_FLUSH", 30*time.Second), "sink batch interval")

	fs.StringVar(&c.PriceFile, "prices", env("LOOPMATH_PRICES", ""), "JSON price table override")

	fs.Float64Var(&c.ContextGrowthRatio, "growth-ratio", envF("LOOPMATH_GROWTH_RATIO", 3.0), "context_growth: input tokens last/first")
	fs.IntVar(&c.MinCallsForGrowth, "growth-min-calls", envI("LOOPMATH_GROWTH_MIN_CALLS", 5), "context_growth: min calls in loop")
	fs.Float64Var(&c.LowCacheRate, "low-cache", envF("LOOPMATH_LOW_CACHE", 0.3), "low_cache_rate: threshold")
	fs.IntVar(&c.MinCacheableTokens, "cache-min-tokens", envI("LOOPMATH_CACHE_MIN_TOKENS", 4096), "low_cache_rate: min repeated prefix tokens")
	fs.Float64Var(&c.RedundancyRatio, "redundancy", envF("LOOPMATH_REDUNDANCY", 0.6), "redundant_context: threshold")
	fs.IntVar(&c.RunawayCalls, "runaway-calls", envI("LOOPMATH_RUNAWAY_CALLS", 50), "runaway_loop: calls")
	fs.Float64Var(&c.RunawayUSD, "runaway-usd", envF("LOOPMATH_RUNAWAY_USD", 25), "runaway_loop: USD")
	fs.IntVar(&c.RetryStormRepeats, "retry-repeats", envI("LOOPMATH_RETRY_REPEATS", 3), "retry_storm: identical requests")
	fs.BoolVar(&c.Debug, "debug", os.Getenv("LOOPMATH_DEBUG") == "1", "debug mode (see docs — not for production)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if extra != "" {
		for _, kv := range strings.Split(extra, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if !ok || k == "" || v == "" {
				return nil, fmt.Errorf("bad -extra entry %q (want name=url)", kv)
			}
			c.Extra[k] = v
		}
	}
	return c, nil
}
