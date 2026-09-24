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
	// Google Gemini — ai.google.dev/gemini-api/docs/pricing, 2026-09-24 (paid tier; ≤200k tier for pro)
	"gemini-3.8-flash":      {Input: 0.75, Output: 3.75, CacheRead: 0.075},
	"gemini-3.7-flash":      {Input: 0.75, Output: 3.75, CacheRead: 0.075},
	"gemini-3.6-flash":      {Input: 0.75, Output: 3.75, CacheRead: 0.075},
	"gemini-3.5-flash-lite": {Input: 0.30, Output: 2.5, CacheRead: 0.03},
	"gemini-3.5-flash":      {Input: 1.5, Output: 9, CacheRead: 0.15},
	"gemini-3.1-flash-lite": {Input: 0.25, Output: 1.5, CacheRead: 0.025},
	"gemini-3.1-pro":        {Input: 2, Output: 12, CacheRead: 0.2},
	// xAI — docs.x.ai/docs/models, 2026-09-24 (no cached-input price published)
	"grok-4.7": {Input: 2, Output: 6},
	"grok-4":   {Input: 3, Output: 15, CacheRead: 0.75}, // older, unverified
}

// Family groups a model id into a line whose members are interchangeable
// enough that suggesting a swap is reasonable. Returns "" if unknown.
func Family(model string) string {
	m := strings.ToLower(model)
	for _, f := range []string{"claude-fable", "claude-mythos", "claude-opus", "claude-sonnet", "claude-haiku", "gpt-5", "gpt-4", "gemini-3.8-flash", "gemini-3.5-flash", "gemini-3.1-pro", "grok-4"} {
		if strings.HasPrefix(m, f) {
			return f
		}
	}
	return ""
}

// CheaperCacheRead returns the same-family model with the lowest cache-read
// price that is strictly cheaper than the given model's. ok=false if none.
func (t *Table) CheaperCacheRead(model string) (alt string, altPrice Price, ok bool) {
	fam := Family(model)
	cur, known := t.Lookup(model)
	if fam == "" || !known {
		return "", Price{}, false
	}
	curCR := cur.CacheRead
	if curCR == 0 {
		curCR = cur.Input
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	best := curCR
	for k, p := range t.prices {
		if Family(k) != fam || strings.HasPrefix(strings.ToLower(model), k) {
			continue
		}
		cr := p.CacheRead
		if cr == 0 {
			cr = p.Input
		}
		// same family, cheaper cache read, and not more expensive on output (avoid suggesting a downgrade that costs more elsewhere)
		if cr < best && p.Output <= cur.Output && p.Input <= cur.Input {
			best, alt, altPrice, ok = cr, k, p, true
		}
	}
	return
}

func Default() *Table {
	t := &Table{prices: map[string]Price{}, AsOf: "2026-09-24 built-in defaults (Anthropic, Gemini, xAI grok-4.7 verified; OpenAI placeholders)"}
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
