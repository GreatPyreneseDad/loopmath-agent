# loopmath-agent

A customer-held agent that sits beside your model gateway and emits
**loop-cost findings**. Prompts stay inside your perimeter. Findings leave.

One static binary. No runtime deps. Stdlib only.

```
                 base_url swap                    findings only
  your agents ───────────────▶ loopmath-agent ─────────────────▶ gitdr.ai / LoopMath / your SIEM
                                    │
                                    ▼ passthrough (streaming, unbuffered)
                          Anthropic / OpenAI / any OpenAI-compatible
```

## Install

```sh
go install github.com/GreatPyreneseDad/loopmath-agent/cmd/loopmath-agent@latest
loopmath-agent
```

Then point your apps at it:

```sh
export ANTHROPIC_BASE_URL=http://localhost:8787
export OPENAI_BASE_URL=http://localhost:8787/openai      # or /v1 auto-routes by path
```

That's the whole integration. Nothing else changes. Your API keys pass
through untouched in the `Authorization` / `x-api-key` headers; the agent
never stores them.

Docker:

```sh
docker run -p 8787:8787 -p 127.0.0.1:8788:8788 ghcr.io/greatpyrenesedad/loopmath-agent
```

## What it sees, what it keeps, what it sends

| Stage | Data | Retained? |
|---|---|---|
| Request body | read once to compute: model, message count, tool count, `sha256(system)`, `sha256(first user msg)`, `sha256(canonical body)`, and content-defined **shingle fingerprints** (64-bit hashes of 8-token windows) | **No.** Only the numbers and hashes. |
| Response body | streamed to the client unbuffered; a bounded copy is held until the stream ends, parsed for `usage`, then freed | **No.** |
| Per-loop state | token series, USD series, cache/redundancy ratios, up to 250k shingle hashes | In memory, LRU-bounded, never on disk |
| **Findings** | see below | JSONL on disk (optional), `GET /v1/findings`, batched `POST` to a sink (optional) |

A `Finding` has no field that can hold prompt text. That is enforced by the
type, not by a filter — see `internal/findings/findings.go`. The test suite
asserts no request text appears in any serialized call or finding.

## Findings

Schema `loopmath.findings/v1`. Full field reference in [`docs/findings.md`](docs/findings.md);
JSON Schema in [`docs/findings.schema.json`](docs/findings.schema.json).

| kind | fires when | evidence keys |
|---|---|---|
| `context_growth` | context tokens on the last call ≥ 3× the first, over ≥5 calls | `first_context_tokens`, `last_context_tokens`, `growth_ratio`, `tokens_per_call_slope` |
| `low_cache_rate` | ≥4096 tokens of repeated prefix across the loop, but `cache_read / cacheable < 0.3` | `cacheable_tokens`, `cache_read_tokens`, `cache_rate` |
| `redundant_context` | mean shingle overlap of each call vs. everything sent earlier in the loop ≥ 0.6 | `redundancy`, `input_tokens` |
| `runaway_loop` | calls ≥ 50 or USD ≥ 25; re-fires at each doubling | `calls`, `usd`, `level`, `dominant_call_index`, `dominant_call_usd` |
| `retry_storm` | the identical request body sent ≥3× in one loop | `identical_requests`, `last_status` |
| `error_burst` | ≥3 upstream 4xx/5xx in one loop | `errors`, `last_status` |
| `unknown_price` | model not in price table (USD is understated) | — |

Every finding carries `loop_id`, `loop_key_kind`, `provider`, `model`, `calls`,
`loop_usd`, `loop_tokens`, `loop_seconds`, `est_savings_usd`, and a one-line
`recommendation`. All thresholds are flags/env (`loopmath-agent -h`).

## How loops are keyed

Best available, in order:

1. **Header** — `X-Loopmath-Loop: <id>` (name configurable), `X-Session-Id`, or the trace id from W3C `traceparent`.
2. **Body** — `metadata.loop_id | session_id | trace_id | user_id` (Anthropic) or `user` (OpenAI).
3. **Fingerprint** — `provider + sha256(system) + sha256(first user message)`, closed after 10 min idle.

Agentic loops append turns, so the first user message is stable for the
whole run. That's why zero-config works. If your orchestrator runs many
parallel jobs off the same system prompt *and* the same opening message,
add the header.

## Admin API (`127.0.0.1:8788` by default)

```
GET /v1/findings?limit=200   newest first
GET /v1/loops?limit=100      most recent loops, summary
GET /v1/loops/{id}           one loop with per-call series
GET /metrics                 Prometheus text
GET /healthz
```

Bind it to localhost or behind your own auth. It exposes cost numbers, not
prompts, but it does expose what you spend.

## Sink (optional)

```sh
loopmath-agent -sink https://gitdr.ai/api/v1/findings -sink-token $GITDR_TOKEN
```

Batches every 30s: `{"schema":"loopmath.findings/v1","findings":[...]}` with
`Authorization: Bearer`. Requeues on network failure, drops on HTTP ≥300.
Without `-sink`, nothing leaves the host.

## Prices

Built-in table is a dated default. Override:

```sh
loopmath-agent -prices prices.json
```

```json
{"as_of":"2026-09-10","prices":{"claude-sonnet-4-5":{"input":3,"output":15,"cache_read":0.3,"cache_write":3.75}}}
```

USD per 1M tokens; keys match by longest prefix. Unknown models price at 0
and emit `unknown_price` rather than guessing.

## Notes for platform teams

- **OpenAI streams**: the agent injects `stream_options.include_usage=true`
  so the final SSE chunk carries token counts. Official SDKs handle the
  extra empty-choices chunk. Anthropic streams already carry usage; bodies
  are untouched.
- **Compression**: `Accept-Encoding` is stripped upstream so bodies are
  parseable. Cost: a few percent egress on the upstream hop.
- **Latency**: request body is read fully once (cap 32MB); response is
  streamed with `FlushInterval=-1`. Added latency is the JSON parse of the
  request, sub-millisecond for typical bodies.
- **Memory**: `-max-loops` (default 10k) bounds the LRU; each loop holds up
  to 250k shingle hashes (~2MB worst case, usually far less).
- **Multiple upstreams**: `-extra azure=https://x.openai.azure.com,vllm=http://vllm:8000`
  served at `/azure/...`, `/vllm/...`.
- **Custody**: this is the [TRUST.md](https://gitdr.ai) customer-held mode.
  Admin keys, PATs, and prompts never reach gitdr.ai. Only findings do, and
  only if you turn the sink on.

## Develop

```sh
make test      # go vet + go test -race
make build     # ./bin/loopmath-agent
make docker
```

## License

TBD — MacGregor Holding Company / ROSE Corp.
