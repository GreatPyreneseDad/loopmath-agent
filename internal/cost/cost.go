// Package cost prices token usage. Prices change; the built-in table is a
// dated default and customers override it with a JSON file. Unknown models
// price at zero and are reported as such in findings (unknown_price=true),
// never silently guessed.
package cost

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// Price is USD per 1M tokens.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`  // 0 => same as input
	CacheWrite float64 `json:"cache_write"` // 0 => same as input
}

type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}

func (u Usage) Total() int { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

type Table struct {
	mu     sync.RWMutex
	prices map[string]Price // exact model id or prefix (longest prefix wins)
	AsOf   string
}

// Defaults as of 2026-09. Treat as placeholders; override with -prices.
var defaults = map[string]Price{
	// Anthropic — platform.claude.com/docs/en/about-claude/pricing, 2026-09-24
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-fable-5":    {Input: 10, Output: 50, CacheRead: 1.0, CacheWrite: 12.5},
	"claude-mythos-5-1": {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5},
	"claude-opus-5":     {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4-8":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4-7":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4-6":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4-5":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4":     {Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75}, // 4, 4.1 legacy
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
	"claude-sonnet-4":   {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}, // 4, 4.5, 4.6
	"claude-haiku-4":    {Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
	"claude-3-5-haiku":  {Input: 0.8, Output: 4, CacheRead: 0.08, CacheWrite: 1},
	// OpenAI — placeholders, verify before relying on them
	"gpt-5-mini": {Input: 0.25, Output: 2, CacheRead: 0.025},
	"gpt-5-nano": {Input: 0.05, Output: 0.4, CacheRead: 0.005},
	"gpt-5":      {Input: 1.25, Output: 10, CacheRead: 0.125},
	"gpt-4.1":    {Input: 2, Output: 8, CacheRead: 0.5},
	"gpt-4o":     {Input: 2.5, Output: 10, CacheRead: 1.25},
	"o3":         {Input: 2, Output: 8, CacheRead: 0.5},
	"o4-mini":    {Input: 1.1, Output: 4.4, CacheRead: 0.275},
	// xAI — placeholders
	"grok-4": {Input: 3, Output: 15, CacheRead: 0.75},
	"grok-3": {Input: 3, Output: 15, CacheRead: 0.75},
}

func Default() *Table {
	t := &Table{prices: map[string]Price{}, AsOf: "2026-09-24 built-in defaults (Anthropic verified; OpenAI/xAI placeholders)"}
	for k, v := range defaults {
		t.prices[k] = v
	}
	return t
}

// Load reads a JSON file: {"as_of": "...", "prices": {"model": {...}}}.
// Entries merge over defaults.
func Load(path string) (*Table, error) {
	t := Default()
	if path == "" {
		return t, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		AsOf   string           `json:"as_of"`
		Prices map[string]Price `json:"prices"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range f.Prices {
		t.prices[k] = v
	}
	if f.AsOf != "" {
		t.AsOf = f.AsOf
	}
	return t, nil
}

// Lookup returns the price for a model by longest matching prefix.
func (t *Table) Lookup(model string) (Price, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	m := strings.ToLower(model)
	best, bestLen := Price{}, -1
	for k, p := range t.prices {
		if strings.HasPrefix(m, k) && len(k) > bestLen {
			best, bestLen = p, len(k)
		}
	}
	return best, bestLen >= 0
}

// USD prices a usage. ok=false means the model was unknown and cost is 0.
func (t *Table) USD(model string, u Usage) (usd float64, ok bool) {
	p, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	cr, cw := p.CacheRead, p.CacheWrite
	if cr == 0 {
		cr = p.Input
	}
	if cw == 0 {
		cw = p.Input
	}
	usd = (float64(u.Input)*p.Input + float64(u.Output)*p.Output +
		float64(u.CacheRead)*cr + float64(u.CacheWrite)*cw) / 1e6
	return usd, true
}
