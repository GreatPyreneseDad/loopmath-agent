// Package loop groups calls into loops and evaluates finding rules.
//
// A loop is a sequence of model calls that belong together: one agent run,
// one conversation, one job. The agent keys loops three ways, best first:
//
//  1. header  — X-Loopmath-Loop (or any header the customer configures)
//  2. body    — metadata.loop_id / session_id / trace_id / user_id, or OpenAI `user`
//  3. fingerprint — provider + hash(system prompt) + hash(first user message),
//     closed after an idle window. Agentic loops append turns, so the first
//     user message is stable across the whole run; this is what makes the
//     zero-config case work.
package loop

import (
	"container/list"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/GreatPyreneseDad/loopmath-agent/internal/config"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/cost"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/findings"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/meter"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/shingle"
)

const maxSeenShingles = 250_000 // per loop; ~2MB, then we stop growing

type Loop struct {
	ID           string     `json:"id"`
	KeyKind      string     `json:"key_kind"` // header|body|fingerprint
	Provider     string     `json:"provider"`
	Model        string     `json:"model"` // most recent
	FirstAt      time.Time  `json:"first_at"`
	LastAt       time.Time  `json:"last_at"`
	Calls        int        `json:"calls"`
	Errors       int        `json:"errors"`
	Usage        cost.Usage `json:"usage"`
	USD          float64    `json:"usd"`
	UnknownPrice bool       `json:"unknown_price"`

	// series (bounded)
	Context []int     `json:"context_tokens"` // input+cache_read+cache_write per call (full prompt size)
	CallUSD []float64 `json:"call_usd"`
	Latency []float64 `json:"latency_ms"`

	// derived
	CacheableTokens int     `json:"cacheable_tokens"` // sum over i>0 of min(ctx[i-1], ctx[i])
	CacheRate       float64 `json:"cache_rate"`
	Redundancy      float64 `json:"redundancy"` // mean overlap of call shingles vs seen
	redundancySum   float64
	DominantCall    int    `json:"dominant_call"` // index of costliest call
	DebugSystem     string `json:"system_prefix,omitempty"`

	seen      shingle.Set
	reqHashes map[string]int
	emitted   map[findings.Kind]int // kind -> threshold level emitted
	models    map[string]bool
	elem      *list.Element
}

type Engine struct {
	cfg   *config.Config
	price *cost.Table
	emit  *findings.Emitter

	mu     sync.Mutex
	loops  map[string]*Loop
	lru    *list.List // front = most recent
	seq    int
	fpLast map[string]string // fingerprint -> loop id (for idle-window rollover)
}

func New(cfg *config.Config, price *cost.Table, emit *findings.Emitter) *Engine {
	return &Engine{cfg: cfg, price: price, emit: emit, loops: map[string]*Loop{}, lru: list.New(), fpLast: map[string]string{}}
}

// Key resolves the loop id for a call.
func (e *Engine) key(c *meter.Call, headerHint string) (id, kind string) {
	if headerHint != "" {
		return "h:" + headerHint, "header"
	}
	if c.LoopHint != "" {
		return "b:" + c.LoopHint, "body"
	}
	fp := fmt.Sprintf("f:%s:%s:%s", c.Provider, c.SystemHash, c.FirstUserHash)
	// idle rollover: same fingerprint after the idle window starts a new loop
	if prev, ok := e.fpLast[fp]; ok {
		if l, ok := e.loops[prev]; ok && c.At.Sub(l.LastAt) <= e.cfg.LoopIdleWindow {
			return prev, "fingerprint"
		}
	}
	e.seq++
	id = fmt.Sprintf("%s:%d", fp, e.seq)
	e.fpLast[fp] = id
	return id, "fingerprint"
}

// Observe folds a completed call into its loop and runs the rules.
func (e *Engine) Observe(c *meter.Call, headerHint string) *Loop {
	e.mu.Lock()
	defer e.mu.Unlock()

	id, kind := e.key(c, headerHint)
	l, ok := e.loops[id]
	if !ok {
		l = &Loop{ID: id, KeyKind: kind, Provider: string(c.Provider), FirstAt: c.At,
			seen: shingle.Set{}, reqHashes: map[string]int{}, emitted: map[findings.Kind]int{}, models: map[string]bool{}}
		if e.cfg.Debug {
			l.DebugSystem = c.SystemPrefix
		}
		e.loops[id] = l
		l.elem = e.lru.PushFront(id)
		for e.lru.Len() > e.cfg.MaxLoops {
			old := e.lru.Back()
			e.lru.Remove(old)
			delete(e.loops, old.Value.(string))
		}
	} else {
		e.lru.MoveToFront(l.elem)
	}

	// price
	usd, known := e.price.USD(c.Model, c.Usage)
	c.USD, c.PriceKnown = usd, known
	if !known && c.Model != "" {
		l.UnknownPrice = true
	}

	// fold
	l.LastAt = c.At
	l.Calls++
	l.Model = c.Model
	l.models[c.Model] = true
	if c.Status >= 400 {
		l.Errors++
	}
	l.Usage.Input += c.Usage.Input
	l.Usage.Output += c.Usage.Output
	l.Usage.CacheRead += c.Usage.CacheRead
	l.Usage.CacheWrite += c.Usage.CacheWrite
	l.USD += usd
	ctx := c.Usage.Input + c.Usage.CacheRead + c.Usage.CacheWrite
	if n := len(l.Context); n > 0 {
		l.CacheableTokens += min(l.Context[n-1], ctx)
	}
	l.Context = appendBounded(l.Context, ctx, 2000)
	l.CallUSD = appendBounded(l.CallUSD, usd, 2000)
	l.Latency = appendBounded(l.Latency, float64(c.Latency.Milliseconds()), 2000)
	if l.CacheableTokens > 0 {
		l.CacheRate = min(1, float64(l.Usage.CacheRead)/float64(l.CacheableTokens))
	}
	if usd > l.CallUSD[l.DominantCall] {
		l.DominantCall = len(l.CallUSD) - 1
	}

	// redundancy: overlap of this call vs everything seen before it
	if l.Calls > 1 && len(c.Shingles) > 0 {
		l.redundancySum += shingle.Overlap(c.Shingles, l.seen)
		l.Redundancy = l.redundancySum / float64(l.Calls-1)
	}
	if len(l.seen) < maxSeenShingles {
		l.seen.Merge(c.Shingles)
	}
	l.reqHashes[c.RequestHash]++

	e.rules(l, c)
	return l
}

func appendBounded[T any](s []T, v T, max int) []T {
	s = append(s, v)
	if len(s) > max {
		// keep first (needed for growth ratio) and the tail
		copy(s[1:], s[len(s)-max+1:])
		s = s[:max]
	}
	return s
}

func (e *Engine) base(l *Loop, c *meter.Call, kind findings.Kind, sev findings.Severity) findings.Finding {
	e.seq++
	return findings.Finding{
		Schema: findings.SchemaVersion, ID: fmt.Sprintf("f-%d-%d", c.At.UnixMilli(), e.seq),
		At: c.At, Kind: kind, Severity: sev,
		LoopID: l.ID, LoopKey: l.KeyKind, Provider: l.Provider, Model: l.Model,
		Calls: l.Calls, LoopUSD: round(l.USD), LoopTokens: l.Usage.Total(),
		LoopDuration: l.LastAt.Sub(l.FirstAt).Seconds(),
		Evidence:     map[string]float64{}, Agent: "loopmath-agent/" + Version,
	}
}

var Version = "0.1.1"

func round(f float64) float64 { return math.Round(f*1e4) / 1e4 }

func (e *Engine) rules(l *Loop, c *meter.Call) {
	cfg := e.cfg
	price, _ := e.price.Lookup(c.Model)
	inPrice := price.Input / 1e6
	crPrice := price.CacheRead / 1e6
	if crPrice == 0 {
		crPrice = inPrice
	}

	// unknown_price: once per loop
	if l.UnknownPrice && l.emitted[findings.UnknownPrice] == 0 && !c.PriceKnown {
		f := e.base(l, c, findings.UnknownPrice, findings.Info)
		f.Recommendation = "loopmath: model not in price table; loop_usd is understated. Add it to the -prices file (see https://gitdr.ai/agent.md#prices)."
		l.emitted[findings.UnknownPrice] = 1
		e.emit.Emit(f)
	}

	// context_growth
	if l.Calls >= cfg.MinCallsForGrowth && l.emitted[findings.ContextGrowth] == 0 && l.Context[0] > 0 {
		first, last := float64(l.Context[0]), float64(l.Context[len(l.Context)-1])
		ratio := last / first
		if ratio >= cfg.ContextGrowthRatio {
			f := e.base(l, c, findings.ContextGrowth, findings.Warn)
			f.Evidence["first_context_tokens"] = first
			f.Evidence["last_context_tokens"] = last
			f.Evidence["growth_ratio"] = round(ratio)
			f.Evidence["tokens_per_call_slope"] = round((last - first) / float64(l.Calls-1))
			// savings if context were held at the median instead of growing
			med := median(l.Context)
			excess := 0.0
			for _, v := range l.Context {
				if float64(v) > med {
					excess += float64(v) - med
				}
			}
			f.EstSavingsUSD = round(excess * inPrice)
			f.Recommendation = "loopmath: context grows every call in this loop. Summarize or truncate history, or move the stable prefix into a cached block. Loop detail: loopmath_loop / https://gitdr.ai/loops"
			if ratio >= cfg.ContextGrowthRatio*3 {
				f.Severity = findings.High
			}
			l.emitted[findings.ContextGrowth] = 1
			e.emit.Emit(f)
		}
	}

	// low_cache_rate
	if l.Calls >= 3 && l.emitted[findings.LowCacheRate] == 0 && l.CacheableTokens >= cfg.MinCacheableTokens && l.CacheRate < cfg.LowCacheRate {
		f := e.base(l, c, findings.LowCacheRate, findings.Warn)
		f.Evidence["cacheable_tokens"] = float64(l.CacheableTokens)
		f.Evidence["cache_read_tokens"] = float64(l.Usage.CacheRead)
		f.Evidence["cache_rate"] = round(l.CacheRate)
		missed := float64(l.CacheableTokens - l.Usage.CacheRead)
		f.EstSavingsUSD = round(missed * (inPrice - crPrice))
		f.Recommendation = "loopmath: a repeated prefix is being re-billed at full input price. Enable prompt caching on the stable prefix (system + tools + early turns). https://gitdr.ai/fix/caching"
		if f.EstSavingsUSD > 1 {
			f.Severity = findings.High
		}
		l.emitted[findings.LowCacheRate] = 1
		e.emit.Emit(f)
	}

	// redundant_context
	if l.Calls >= 3 && l.emitted[findings.RedundantContext] == 0 && l.Redundancy >= cfg.RedundancyRatio {
		f := e.base(l, c, findings.RedundantContext, findings.Warn)
		f.Evidence["redundancy"] = round(l.Redundancy)
		f.Evidence["input_tokens"] = float64(l.Usage.Input)
		f.EstSavingsUSD = round(l.Redundancy * float64(l.Usage.Input) * inPrice * 0.5) // conservative: half of redundant content is removable
		f.Recommendation = "loopmath: most of each request was already sent earlier in this loop. Dedupe tool outputs / retrieved chunks, or cache the shared prefix. https://gitdr.ai/fix/redundancy"
		l.emitted[findings.RedundantContext] = 1
		e.emit.Emit(f)
	}

	// runaway_loop: emit at threshold, then at each doubling
	level := l.emitted[findings.RunawayLoop]
	callsHit := l.Calls >= cfg.RunawayCalls<<level
	usdHit := cfg.RunawayUSD > 0 && l.USD >= cfg.RunawayUSD*math.Pow(2, float64(level))
	if callsHit || usdHit {
		sev := findings.Warn
		if level >= 1 {
			sev = findings.High
		}
		f := e.base(l, c, findings.RunawayLoop, sev)
		f.Evidence["calls"] = float64(l.Calls)
		f.Evidence["usd"] = round(l.USD)
		f.Evidence["level"] = float64(level)
		f.Evidence["dominant_call_index"] = float64(l.DominantCall)
		f.Evidence["dominant_call_usd"] = round(l.CallUSD[l.DominantCall])
		f.Recommendation = "loopmath: this loop exceeded its budget bound. Add a step/cost cap and a termination check; inspect the dominant call (loopmath_loop). https://gitdr.ai/fix/runaway"
		l.emitted[findings.RunawayLoop] = level + 1
		e.emit.Emit(f)
	}

	// retry_storm
	if n := l.reqHashes[c.RequestHash]; n >= cfg.RetryStormRepeats && l.emitted[findings.RetryStorm] < n/cfg.RetryStormRepeats {
		f := e.base(l, c, findings.RetryStorm, findings.Warn)
		f.Evidence["identical_requests"] = float64(n)
		f.Evidence["last_status"] = float64(c.Status)
		f.EstSavingsUSD = round(float64(n-1) * c.USD)
		f.Recommendation = "loopmath: the same request body was sent repeatedly. Check retry policy / idempotency; memoize if the output is deterministic enough. https://gitdr.ai/fix/retries"
		l.emitted[findings.RetryStorm] = n / cfg.RetryStormRepeats
		e.emit.Emit(f)
	}

	// error_burst
	if l.Errors >= 3 && l.emitted[findings.ErrorBurst] < l.Errors/3 {
		f := e.base(l, c, findings.ErrorBurst, findings.Warn)
		f.Evidence["errors"] = float64(l.Errors)
		f.Evidence["last_status"] = float64(c.Status)
		f.Recommendation = "loopmath: upstream errors repeating inside one loop. Back off; check rate limits and request validity. https://gitdr.ai/fix/errors"
		l.emitted[findings.ErrorBurst] = l.Errors / 3
		e.emit.Emit(f)
	}
}

func median(xs []int) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := make([]int, len(xs))
	copy(c, xs)
	// insertion sort is fine at n<=2000? no — use sort
	sortInts(c)
	if len(c)%2 == 1 {
		return float64(c[len(c)/2])
	}
	return float64(c[len(c)/2-1]+c[len(c)/2]) / 2
}

// Snapshot returns loops, most recent first.
func (e *Engine) Snapshot(n int) []*Loop {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Loop, 0, n)
	for el := e.lru.Front(); el != nil && len(out) < n; el = el.Next() {
		l := e.loops[el.Value.(string)]
		cp := *l
		cp.Context, cp.CallUSD, cp.Latency = nil, nil, nil // keep /loops light
		out = append(out, &cp)
	}
	return out
}

// Get returns one loop with full series.
func (e *Engine) Get(id string) (*Loop, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.loops[id]
	if !ok {
		return nil, false
	}
	cp := *l
	return &cp, true
}

// Totals for /metrics.
func (e *Engine) Totals() (loops, calls int, usd float64, u cost.Usage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, l := range e.loops {
		loops++
		calls += l.Calls
		usd += l.USD
		u.Input += l.Usage.Input
		u.Output += l.Usage.Output
		u.CacheRead += l.Usage.CacheRead
		u.CacheWrite += l.Usage.CacheWrite
	}
	return
}
