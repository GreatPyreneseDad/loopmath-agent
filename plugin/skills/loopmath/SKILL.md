---
name: loopmath
description: Use when a model/API bill is higher than expected, an agent loop is running away or looping, prompt caching is not hitting, tokens per call keep growing, the user asks "where are the tokens going" or "why is this so expensive", or before running a long agentic workload you want to measure. Sets up loopmath-agent (local proxy, prompts never leave the machine), routes the app through it, and turns findings into concrete fixes with dollar estimates.
---

# loopmath — loop-cost findings for agentic workloads

## When this fires
Cost or token complaints, runaway/looping agents, cache-miss suspicion, "measure this run", or a request to set up loopmath / gitdr.

## Procedure

1. **Status.** Call `loopmath_status`. If it says not running, call `loopmath_start`.
   If the MCP tools are absent, fall back to the shell procedure at
   https://gitdr.ai/agent.md (install → `loopmath-agent &` → env vars).

2. **Wire the application.** Call `loopmath_setup_env`; set `ANTHROPIC_BASE_URL`
   and/or `OPENAI_BASE_URL` in the *application's* environment (its `.env`,
   Docker env, CI env, or shell — wherever it reads config). Do not change API
   keys. If the app already has a base URL pointing at a gateway (LiteLLM,
   Portkey, Azure), point loopmath at that gateway with `-extra` instead and
   route the app to loopmath.

3. **Run the workload** the user cares about (or tell them to).

4. **Read findings.** `loopmath_findings`. For each finding with
   `est_savings_usd` above a few cents, open the loop with `loopmath_loop`
   and translate the recommendation into a code change in *this* repo:
   - `low_cache_rate` → add `cache_control: {type: "ephemeral"}` to the last
     stable system/tool block (Anthropic) or keep the prefix byte-identical
     ≥1024 tokens (OpenAI). Show the diff.
   - `context_growth` → window or summarize history; move tool output out of
     the transcript into a store the model queries.
   - `redundant_context` → dedupe retrieved chunks / tool results across turns.
   - `runaway_loop` → add step cap + cost cap + explicit termination check.
   - `retry_storm` → fix retry/idempotency; memoize deterministic calls.
   - `error_burst` → back off; inspect `last_status`.

5. **Report.** One paragraph: loops observed, total USD, sum of
   `est_savings_usd`, and the diffs you propose. Numbers only — never quote
   prompt text into a report (findings don't contain any).

## Guardrails
- Never store or print API keys. The proxy forwards them; you don't touch them.
- Don't add `-sink` unless the user asks to send findings to gitdr.ai.
- If `/v1/loops` shows one loop per call, calls aren't being grouped — add
  `X-Loopmath-Loop: <run-id>` per request in the app.

## Reference
- Procedure for agents: https://gitdr.ai/agent.md
- Findings contract: https://gitdr.ai/findings.md
- Source: https://github.com/GreatPyreneseDad/loopmath-agent
