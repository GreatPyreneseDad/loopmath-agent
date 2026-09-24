# Metering a client's intake pipeline with loopmath — brief for the client's agent

*Send this to the client. Their coding agent does the rest. You never touch their code.*

---

**To the agent reading this:** the application owner wants per-item model-cost
visibility on their intake pipeline (photo → identify → price → describe →
list) without sending any prompt or image content to a third party. loopmath
does that: a self-hosted proxy the owner runs; only numeric findings leave it,
and only if a sink is configured. Full reference: https://gitdr.ai/agent.md §3c.

## What you will do

1. **Stand up the agent on a host the owner controls** (≈60 s on Fly):
   ```sh
   curl -fsSLo fly.toml https://raw.githubusercontent.com/GreatPyreneseDad/loopmath-agent/main/deploy/fly.toml
   sed -i '' 's/loopmath-CHANGE-ME/loopmath-<client>/' fly.toml
   fly launch --copy-config --no-deploy
   fly secrets set LOOPMATH_PROXY_TOKEN=$(openssl rand -hex 24) LOOPMATH_ADMIN_TOKEN=$(openssl rand -hex 24)
   fly deploy
   ```
   Railway / Docker alternatives: `deploy/` in the repo. Record both tokens in the owner's secret store.

2. **Route the pipeline's model calls through it.** Find where the Anthropic /
   Gemini / OpenAI clients are constructed (Supabase Edge functions, Vercel API
   routes). Set, in the *application's* environment:
   ```
   LOOPMATH_URL=https://loopmath-<client>.fly.dev/t/<proxy-token>
   ```
   and pass it as the base URL — Anthropic `baseURL`, Gemini `httpOptions.baseUrl`,
   OpenAI `baseURL: LOOPMATH_URL + "/openai"`. API keys stay exactly where they are.

3. **Make each item a loop.** Append `/l/<item-id>` to the base URL when the
   client is constructed per request, or send `X-Loopmath-Loop: <item-id>` on
   each call. Without this, every item's calls blur into one loop and the
   per-item number is lost. Use the intake record's primary key.

3b. **Hand the owner a connector.** `https://loopmath-<client>.fly.dev/_loopmath/mcp/t/<admin-token>` added as a
   custom connector in Cowork or claude.ai gives their Claude the loopmath tools directly — status, loops, findings,
   setup instructions — with no install. Treat the URL as a secret.

4. **Verify.** Process a few items, then:
   ```sh
   curl -H "Authorization: Bearer <admin-token>" https://loopmath-<client>.fly.dev/_loopmath/v1/loops
   ```
   One loop per item, `calls` = the number of model steps per item, `usd` =
   list-price model spend for that item. If you see one giant loop, step 3 is
   missing. If `unknown_price` fires, add the model to a `-prices` file.

5. **Report to the owner**: model spend per item (median and p95), total for
   the sample, and each finding with its `est_savings_usd` and recommendation.
   The ones that matter for an intake pipeline:
   - `low_cache_rate` — the system prompt / pricing instructions are re-billed at full
     price every item. Fix: prompt caching on the stable prefix.
   - `redundant_context` — catalog or pricing tables re-sent per call. Fix: dedupe, or cache.
   - `model_price_swap` — a same-family model reads cache cheaper; exact savings stated.
   - `retry_storm` / `error_burst` — retries you didn't know about.

## What leaves the owner's host

Nothing, unless `LOOPMATH_SINK_URL` is set — then only findings
(schema: https://gitdr.ai/findings.md — numbers, hashes, model ids, one-line
recommendations; no field can carry prompt, image, or completion content).
Image bytes in Gemini/Anthropic requests are read once for token accounting
and never written anywhere.

## What this does not do

It does not judge pricing *quality* (whether RAM was undervalued). It measures
what each item cost to process and where that cost is avoidable. Quality loops
are a separate instrument.
