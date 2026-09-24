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
	if l.CacheRate < 0.9 || l.CacheRate > 1 {
		t.Fatalf("cache rate %.3f, want ~1", l.CacheRate)
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
