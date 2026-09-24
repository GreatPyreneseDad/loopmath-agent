#!/bin/sh
# Any VPS with Docker. Put a TLS terminator (Caddy/nginx/Cloudflare Tunnel) in front for serverless callers.
PROXY_TOKEN=${PROXY_TOKEN:-$(openssl rand -hex 24)}
ADMIN_TOKEN=${ADMIN_TOKEN:-$(openssl rand -hex 24)}
docker run -d --name loopmath --restart unless-stopped -p 8080:8080 \
  -e PORT=8080 -e LOOPMATH_PROXY_TOKEN="$PROXY_TOKEN" -e LOOPMATH_ADMIN_TOKEN="$ADMIN_TOKEN" \
  -v loopmath-data:/data -e LOOPMATH_FINDINGS_FILE=/data/findings.jsonl \
  ghcr.io/greatpyrenesedad/loopmath-agent:latest
echo "proxy token: $PROXY_TOKEN"
echo "admin token: $ADMIN_TOKEN"
echo "base_url:    https://<your-host>/t/$PROXY_TOKEN"
