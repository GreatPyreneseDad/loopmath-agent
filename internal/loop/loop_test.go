package loop

import (
	"testing"
	"time"

	"github.com/GreatPyreneseDad/loopmath-agent/internal/config"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/cost"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/findings"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/meter"
)

// Regression: a Claude Code-style session writes the whole prefix to cache on
// call 1 (cache_write large, input tiny) and reads it back on every later
// call. Cache rate must land in [0,1] and be high, not 158.
func TestCacheRateWithCacheWriteFirst(t *testing.T) {
	cfg, _ := config.Load([]string{"-findings-file", ""})
	emit, _ := findings.NewEmitter("", "", "", time.Second, 100)
	e := New(cfg, cost.Default(), emit)
	now := time.Now()
	calls := []cost.Usage{
		{Input: 200, Output: 900, CacheWrite: 40000},
		{Input: 250, Output: 800, CacheRead: 40000, CacheWrite: 1200},
		{Input: 300, Output: 700, CacheRead: 41200, CacheWrite: 1000},
		{Input: 200, Output: 950, CacheRead: 42200},
	}
	var l *Loop
	for i, u := range calls {
		l = e.Observe(&meter.Call{ID: "c", At: now.Add(time.Duration(i) * time.Second), Provider: meter.Anthropic, Model: "claude-fable-5", Usage: u, SystemHash: "s", FirstUserHash: "u"}, "sess")
	}
	if l.CacheRate == nil || *l.CacheRate < 0.9 || *l.CacheRate > 1 {
		t.Fatalf("cache rate %v, want ~1", l.CacheRate)
	}
	if l.USD <= 0 {
		t.Fatalf("claude-fable-5 should be priced, got %f", l.USD)
	}
	if l.Context[0] != 40200 {
		t.Fatalf("first context should include cache_write: %d", l.Context[0])
	}
	for _, f := range emit.Recent(10) {
		if f.Kind == findings.LowCacheRate || f.Kind == findings.UnknownPrice {
			t.Fatalf("spurious finding %s", f.Kind)
		}
	}
}

// Christopher's real Claude Code loop, 2026-09-24: 15 calls on claude-fable-5.
// Expect model_price_swap to name claude-fable-5-1 and land near −30%.
func TestModelPriceSwapOnRealLoop(t *testing.T) {
	cfg, _ := config.Load([]string{"-findings-file", ""})
	emit, _ := findings.NewEmitter("", "", "", time.Second, 100)
	e := New(cfg, cost.Default(), emit)
	now := time.Now()
	total := cost.Usage{Input: 13553, Output: 8419, CacheRead: 2238029, CacheWrite: 222461}
	for i := 0; i < 15; i++ {
		u := cost.Usage{Input: total.Input / 15, Output: total.Output / 15, CacheRead: total.CacheRead / 15, CacheWrite: total.CacheWrite / 15}
		e.Observe(&meter.Call{ID: "c", At: now.Add(time.Duration(i) * time.Second), Provider: meter.Anthropic, Model: "claude-fable-5", Usage: u, SystemHash: "s", FirstUserHash: "u"}, "cc")
	}
	var got *findings.Finding
	for _, f := range emit.Recent(20) {
		if f.Kind == findings.ModelPriceSwap {
			ff := f
			got = &ff
		}
	}
	if got == nil {
		t.Fatalf("expected model_price_swap; got %v", emit.Recent(20))
	}
	if got.AltModel != "claude-fable-5-1" {
		t.Fatalf("alt model %q", got.AltModel)
	}
	if got.Evidence["saving_pct"] < 0.25 || got.Evidence["saving_pct"] > 0.35 {
		t.Fatalf("saving pct %.2f, expected ~0.30 (%+v)", got.Evidence["saving_pct"], got.Evidence)
	}
	if got.Billing != "api" {
		t.Fatalf("billing label %q", got.Billing)
	}
}
