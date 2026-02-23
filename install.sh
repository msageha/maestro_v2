#!/bin/bash
set -euo pipefail

# install.sh — Maestro bootstrap script
# Checks dependencies, builds the Go binary, and installs it.

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

err() { echo -e "${RED}error:${NC} $*" >&2; }
info() { echo -e "${GREEN}==>${NC} $*"; }
warn() { echo -e "${YELLOW}warning:${NC} $*"; }

# Step 1: Check dependencies
missing=0

if ! command -v tmux &>/dev/null; then
    err "tmux is not installed"
    echo "  Install via: brew install tmux"
    missing=1
fi

if ! command -v go &>/dev/null; then
    err "go is not installed"
    echo "  Install via: brew install go"
    echo "  Or visit: https://go.dev/dl/"
    missing=1
fi

if ! command -v claude &>/dev/null; then
    err "claude (Claude Code CLI) is not installed"
    echo "  Install via: npm install -g @anthropic-ai/claude-code"
    missing=1
fi

if [ "$missing" -ne 0 ]; then
    echo ""
    err "Missing dependencies. Please install them and re-run this script."
    exit 1
fi

info "All dependencies found."

# Step 2: Build
info "Building maestro..."
go build -o maestro ./cmd/maestro/

# Step 3: Install
if [ -n "${MAESTRO_INSTALL_DIR:-}" ]; then
    INSTALL_DIR="$MAESTRO_INSTALL_DIR"
elif [ -d "${HOME}/bin" ] || [ -w "${HOME}" ]; then
    INSTALL_DIR="${HOME}/bin"
elif [ -w /usr/local/bin ]; then
    INSTALL_DIR="/usr/local/bin"
else
    INSTALL_DIR="${HOME}/bin"
fi

mkdir -p "$INSTALL_DIR"
mv maestro "$INSTALL_DIR/maestro"
chmod +x "$INSTALL_DIR/maestro"

info "Installed maestro to ${INSTALL_DIR}/maestro"

# Step 4: PATH check
if ! echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
    warn "$INSTALL_DIR is not in your PATH."
    echo "  Add the following to your shell profile:"
    echo "    export PATH=\"${INSTALL_DIR}:\$PATH\""
fi

info "Done."
