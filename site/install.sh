#!/bin/sh
# loopmath-agent installer: curl -fsSL https://gitdr.ai/install.sh | sh
set -eu
REPO="GreatPyreneseDad/loopmath-agent"
BIN="loopmath-agent"
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in x86_64|amd64) ARCH=amd64;; arm64|aarch64) ARCH=arm64;; *) echo "unsupported arch $ARCH" >&2; exit 1;; esac
case "$OS" in linux|darwin) ;; *) echo "unsupported OS $OS (use go install or docker)" >&2; exit 1;; esac
VER="${LOOPMATH_VERSION:-$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')}"
[ -n "$VER" ] || { echo "could not resolve latest version" >&2; exit 1; }
URL="https://github.com/$REPO/releases/download/$VER/${BIN}_${VER#v}_${OS}_${ARCH}.tar.gz"
DEST="${LOOPMATH_INSTALL_DIR:-/usr/local/bin}"
[ -w "$DEST" ] || DEST="$HOME/.local/bin"
mkdir -p "$DEST"
TMP=$(mktemp -d)
curl -fsSL "$URL" | tar -xz -C "$TMP" "$BIN"
install -m 0755 "$TMP/$BIN" "$DEST/$BIN"
rm -rf "$TMP"
echo "installed $DEST/$BIN ($VER)"
case ":$PATH:" in *":$DEST:"*) ;; *) echo "add $DEST to PATH";; esac
echo
echo "next:  $BIN &            # start proxy :8787, admin 127.0.0.1:8788"
echo "       export ANTHROPIC_BASE_URL=http://localhost:8787"
echo "       export OPENAI_BASE_URL=http://localhost:8787/openai"
echo "       docs: https://gitdr.ai/agent.md"
