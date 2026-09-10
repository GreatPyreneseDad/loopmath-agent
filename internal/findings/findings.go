// Package findings defines the one thing that leaves the customer's
// perimeter: a Finding. Numbers, hashes, ids, a recommendation. No text
// from any prompt or completion, by construction — there is no field for it.
package findings

import (
	"time"
)

const SchemaVersion = "loopmath.findings/v1"

type Kind string

const (
	ContextGrowth    Kind = "context_growth"    // context balloons across the loop
	LowCacheRate     Kind = "low_cache_rate"    // repeated prefix, little cache read
	RedundantContext Kind = "redundant_context" // same content re-sent each call
	RunawayLoop      Kind = "runaway_loop"      // calls or dollars past a bound
	RetryStorm       Kind = "retry_storm"       // identical request repeated
	UnknownPrice     Kind = "unknown_price"     // model not in price table
	ErrorBurst       Kind = "error_burst"       // upstream 4xx/5xx repeating
)

type Severity string

const (
	Info Severity = "info"
	Warn Severity = "warn"
	High Severity = "high"
)

type Finding struct {
	Schema   string    `json:"schema"`
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Kind     Kind      `json:"kind"`
	Severity Severity  `json:"severity"`

	LoopID   string `json:"loop_id"`
	LoopKey  string `json:"loop_key_kind"` // header | body | fingerprint
	Provider string `json:"provider"`
	Model    string `json:"model"`

	Calls        int     `json:"calls"`
	LoopUSD      float64 `json:"loop_usd"`
	LoopTokens   int     `json:"loop_tokens"`
	LoopDuration float64 `json:"loop_seconds"`

	// Kind-specific numbers. Keys are stable per kind; see docs/findings.md.
	Evidence map[string]float64 `json:"evidence"`

	EstSavingsUSD  float64 `json:"est_savings_usd"`
	Recommendation string  `json:"recommendation"`
	Agent          string  `json:"agent"`
}
