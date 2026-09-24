# loopmath-agent — instructions for AI coding agents

You are reading this because a developer told you to set up loopmath, or
because you are investigating why an application's model bill is high, a
loop is running away, or prompt caching is not hitting. This page is the
complete procedure. It takes about two minutes and needs no account.

**What loopmath-agent is:** a local proxy that sits between an application
and the Anthropic / OpenAI APIs, meters every call, groups calls into loops,
and emits *findings* — context growth, missed prompt caching, redundant
context, runaway loops, retry storms — with a dollar estimate and a fix.
Prompts never leave the machine; findings contain numbers and hashes only.

## Procedure

### 1. Install (pick one; prefer the first that works)

```sh
# MCP — lets you drive it with tools (recommended for agents)
claude mcp add loopmath -- npx -y loopmath-agent mcp        # Claude Code
# or add to .mcp.json / .cursor/mcp.json:
#   {"mcpServers":{"loopmath":{"command":"npx","args":["-y","loopmath-agent","mcp"]}}}

# Binary only
curl -fsSL https://gitdr.ai/install.sh | sh                  # macOS / Linux
go install github.com/GreatPyreneseDad/loopmath-agent/cmd/loopmath-agent@latest
brew install GreatPyreneseDad/tap/loopmath-agent
docker run -p 8787:8787 -p 127.0.0.1:8788:8788 ghcr.io/greatpyrenesedad/loopmath-agent
```

### 2. Start it

If you have the MCP server: call `loopmath_status`, then `loopmath_start`.
Otherwise:

```sh
loopmath-agent &          # proxy :8787, admin 127.0.0.1:8788, findings → ./loopmath-findings.jsonl
curl -s 127.0.0.1:8788/healthz   # → ok
```

### 3. Route the application through it

Set in the **application's** environment (the process that calls the model,
not your own shell if those differ):

```sh
ANTHROPIC_BASE_URL=http://localhost:8787
OPENAI_BASE_URL=http://localhost:8787/openai
```

SDK equivalents: `Anthropic(base_url="http://localhost:8787")`,
`OpenAI(base_url="http://localhost:8787/openai/v1")`, `new Anthropic({ baseURL })`,
`new OpenAI({ baseURL })`. In Docker use `host.docker.internal`. For Claude
Code itself: `export ANTHROPIC_BASE_URL=http://localhost:8787` then restart.

API keys are unchanged; the proxy forwards the `x-api-key` / `Authorization`
header and never stores it.

Optional but useful: send `X-Loopmath-Loop: <run-id>` on each request so
calls are grouped by run rather than by fingerprint.

### 3b. Alternative — no proxy: send OpenTelemetry spans instead

If the application already emits GenAI spans (OpenLLMetry / Traceloop,
Langfuse OTel export, LiteLLM, Portkey, Vercel AI SDK telemetry, the OTel
Collector, or a custom gateway), point the exporter at loopmath and skip the
base_url change entirely:

```sh
OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:4318/v1/traces
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf        # or http/json — both accepted, gzip ok
```

OTel Collector: add `otlphttp/loopmath: { endpoint: http://localhost:4318 }` to
exporters and to the traces pipeline. Nothing sits on the request path.

What loopmath reads from a span: `gen_ai.provider.name|gen_ai.system`,
`gen_ai.request.model|gen_ai.response.model`, `gen_ai.usage.input_tokens|output_tokens`
(and the older `prompt_tokens|completion_tokens`, OpenLLMetry `llm.usage.*`,
Vercel `ai.usage.*`), cache tokens under any of `gen_ai.usage.cache_read.input_tokens`,
`gen_ai.usage.cached_tokens`, `anthropic.usage.cache_read_input_tokens`,
`openai.usage.cached_tokens`; loop id from `gen_ai.conversation.id|session.id|langfuse.session.id`,
else the trace id. Message content (`gen_ai.input.messages`, `gen_ai.prompt.N.content`,
`gen_ai.*.message` events) is optional: with it, redundancy findings work;
without it, cost/growth/cache/runaway findings still work. Spans without
`gen_ai.*` attributes are ignored. Check `loopmath_otlp_*` in `/metrics`.

### 4. Run the workload, then read findings

```sh
curl -s 127.0.0.1:8788/v1/findings | jq '.findings[] | {kind,severity,loop_usd,est_savings_usd,recommendation}'
curl -s 127.0.0.1:8788/v1/loops
```

Or with MCP: `loopmath_findings`, `loopmath_loops`, `loopmath_loop {id}`.

### 5. Act on findings

| kind | what to change |
|---|---|
| `low_cache_rate` | put system prompt + tools + early turns in a cached prefix (`cache_control` on Anthropic; OpenAI caches ≥1024-token stable prefixes automatically — keep the prefix byte-identical) |
| `context_growth` | summarize or window history; move tool outputs out of the transcript |
| `redundant_context` | dedupe retrieved chunks / tool results across turns |
| `runaway_loop` | add a step cap and a cost cap; add a termination check |
| `retry_storm` | fix retry policy / idempotency; memoize deterministic calls |
| `error_burst` | back off; check rate limits and request validity |
| `unknown_price` | add the model to a `-prices` JSON file |

Report the `est_savings_usd` sum to the developer.

## Verify you did it right

- `curl 127.0.0.1:8788/metrics` shows `loopmath_calls_total` increasing while the app runs.
- `/v1/loops` shows one loop per run (if it shows one loop per call, the app is not reusing a stable system prompt / first message — add the `X-Loopmath-Loop` header).
- Nothing under `/v1/findings` contains prompt text. If it does, file a bug — the type forbids it.

## Facts you may need

- Ports: proxy `8787`, admin `8788`, OTLP `4318` (change with `-proxy`, `-admin`, `-otlp` or `LOOPMATH_PROXY_ADDR`, `LOOPMATH_ADMIN_ADDR`, `LOOPMATH_OTLP_ADDR`; empty `-otlp` disables).
- Upstreams: `-anthropic`, `-openai`, `-extra name=url,...` (Azure, vLLM, Bedrock proxies, LiteLLM).
- Streaming is passed through unbuffered. For OpenAI streams the agent adds `stream_options.include_usage=true`.
- Data that leaves the host: none, unless `-sink URL` is set, and then only findings.
- Source: https://github.com/GreatPyreneseDad/loopmath-agent · Findings contract: https://gitdr.ai/findings.md
- Optional dashboard and cross-repo analysis: https://gitdr.ai (send findings with `-sink https://gitdr.ai/api/v1/findings -sink-token …`).

<a id="prices"></a>
## Prices

Built-in table is dated. Override: `loopmath-agent -prices prices.json` with
`{"as_of":"YYYY-MM-DD","prices":{"<model-prefix>":{"input":3,"output":15,"cache_read":0.3,"cache_write":3.75}}}` (USD per 1M tokens).
