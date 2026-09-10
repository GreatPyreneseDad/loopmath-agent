# Findings contract — `loopmath.findings/v1`

The finding is the only artifact that crosses the customer's perimeter. It is
the membrane between the hot path (this agent, Go) and the cold path
(LoopMath, gitdr.ai, Python). Both sides build to this document.

## Envelope (sink POST)

```json
{ "schema": "loopmath.findings/v1", "findings": [ Finding, ... ] }
```

## Finding

| field | type | meaning |
|---|---|---|
| `schema` | string | always `loopmath.findings/v1` |
| `id` | string | unique per agent process (`f-<unix_ms>-<seq>`) |
| `at` | RFC3339 | time of the call that triggered the finding |
| `kind` | enum | see kinds below |
| `severity` | `info` \| `warn` \| `high` | |
| `loop_id` | string | prefixed by key kind: `h:` header, `b:` body, `f:` fingerprint |
| `loop_key_kind` | `header` \| `body` \| `fingerprint` | how the loop was identified |
| `provider` | string | `anthropic` \| `openai` \| extra-upstream name |
| `model` | string | most recent model in the loop |
| `calls` | int | calls in the loop so far |
| `loop_usd` | float | cumulative priced cost (0 for unknown models) |
| `loop_tokens` | int | input + output + cache_read + cache_write |
| `loop_seconds` | float | first call → this call |
| `evidence` | object<string,float> | kind-specific; keys are stable |
| `est_savings_usd` | float | conservative, see per-kind formula |
| `recommendation` | string | one sentence, fixed per kind |
| `agent` | string | `loopmath-agent/<version>` |

There is no field for prompt text, completion text, tool arguments, file
paths, or user identifiers other than the opaque `loop_id`. Consumers should
treat `loop_id` values under `b:` as potentially customer-meaningful
(they may be a session or user id the customer chose to send) and store
them accordingly.

## Kinds

### `context_growth`
Context (input + cache_read tokens) on the latest call is ≥ `growth_ratio`
(default 3) × the first call, over ≥ `growth_min_calls` (default 5).
Evidence: `first_context_tokens`, `last_context_tokens`, `growth_ratio`,
`tokens_per_call_slope`. Savings: tokens above the loop's median context ×
input price. Severity `high` at 3× the threshold. Fires once per loop.

### `low_cache_rate`
`cacheable = Σ min(ctx[i-1], ctx[i])` is the prefix that *could* have been
cached. Fires when `cacheable ≥ cache_min_tokens` (4096) and
`cache_read / cacheable < low_cache` (0.3). Evidence: `cacheable_tokens`,
`cache_read_tokens`, `cache_rate`. Savings: `(cacheable − cache_read) ×
(input_price − cache_read_price)`. Fires once per loop.

### `redundant_context`
Each call's shingle set is compared against the union of all earlier calls
in the loop; the mean overlap is `redundancy`. Fires at ≥ `redundancy`
(0.6) after 3 calls. Evidence: `redundancy`, `input_tokens`. Savings:
`redundancy × input_tokens × input_price × 0.5`. Fires once per loop.

### `runaway_loop`
`calls ≥ runaway_calls` (50) or `usd ≥ runaway_usd` (25). Re-fires each
time either doubles (`level` increments). Evidence: `calls`, `usd`,
`level`, `dominant_call_index`, `dominant_call_usd`. Severity `high` from
level 1.

### `retry_storm`
Identical canonical request body (stream flags removed) seen
≥ `retry_repeats` (3) times in one loop. Evidence: `identical_requests`,
`last_status`. Savings: `(n − 1) × cost of one call`. Re-fires every
`retry_repeats` further repeats.

### `error_burst`
≥ 3 upstream responses with status ≥ 400 in one loop. Evidence: `errors`,
`last_status`. Re-fires every 3.

### `unknown_price`
Model matched nothing in the price table. `loop_usd` is understated. Fires
once per loop. Severity `info`.

## Versioning

Additive changes (new kinds, new evidence keys) keep `v1`. Renaming or
removing a field bumps to `v2` and the agent emits both for one release.
