# loopmath in Cowork / Claude — one connector, one sentence

loopmath runs as a **remote MCP connector**. Whoever operates the agent (you,
or a client running their own instance) has a URL like:

```
https://loopmath-<name>.fly.dev/_loopmath/mcp/t/<admin-token>
```

That URL *is* the API call. Add it once; the agent gets six tools.

## For the person using Cowork / Claude

1. **Settings → Connectors → Add custom connector** → paste the URL above → Add.
   (No OAuth. The token is in the URL; treat the URL as a secret.)
2. Tell Claude:

   > **"Use my loopmath connector: check status, set up env for my app, and after I run it, tell me what it finds and what to change."**

   or, once it's wired:

   > **"Check loopmath and fix whatever it finds."**

Claude calls `loopmath_status`, `loopmath_setup_env` (returns the exact env
vars for *your* app — the hosted URL, never localhost), `loopmath_loops`,
`loopmath_findings`, and turns each `recommendation` into a change.

## For the person standing up the instance

```sh
curl -fsSLo fly.toml https://raw.githubusercontent.com/GreatPyreneseDad/loopmath-agent/main/deploy/fly.toml
sed -i '' 's/loopmath-CHANGE-ME/loopmath-<name>/' fly.toml
fly launch --copy-config --no-deploy
PROXY=$(openssl rand -hex 24); ADMIN=$(openssl rand -hex 24)
fly secrets set LOOPMATH_PROXY_TOKEN=$PROXY LOOPMATH_ADMIN_TOKEN=$ADMIN
fly deploy
echo "connector URL: https://loopmath-<name>.fly.dev/_loopmath/mcp/t/$ADMIN"
```

`FLY_APP_NAME` is detected, so `loopmath_setup_env` already knows its own
public URL. On Railway, `RAILWAY_PUBLIC_DOMAIN` is used; anywhere else set
`LOOPMATH_PUBLIC_URL=https://<host>`.

The **admin** token (in the connector URL) reads cost data and returns setup
instructions. The **proxy** token is what the application uses in its base
URL; `loopmath_setup_env` hands it to the agent so it can wire the app.
Rotate either by `fly secrets set` + redeploy.

## Local / desktop alternative (no hosting)

Cowork on the desktop can run a local MCP server. Add to the desktop app's
MCP config:

```json
{ "mcpServers": { "loopmath": { "command": "npx", "args": ["-y", "loopmath-agent", "mcp"] } } }
```
(or the path to a downloaded binary + `mcp`). `loopmath_start` will launch the
proxy on your machine and hand back `ANTHROPIC_BASE_URL=http://localhost:8787`.

## What the connector exposes, and what it cannot

Exposes: is the agent up; per-loop cost/tokens/cache rate/redundancy; findings
with recommendations and dollar estimates; the env vars to wire an app. Every
response is numbers, hashes, model ids, and fixed recommendation sentences.

Cannot: return any prompt, completion, image, or API key. There is no tool
for it and no field that can carry it. Findings contract: https://gitdr.ai/findings.md
