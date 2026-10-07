#!/usr/bin/env bash
# Put a `jaca` command on PATH: a symlink to the `jacad` inside a Jaca.app. `jacad` picks its
# behaviour from the name it is run as (Sources/Daemon/main.swift). It can't be a second binary
# next to the app's own executable: that one is `Jaca`, and the filesystem is case-insensitive.
#
#   ./scripts/install-cli.sh [APP]              # APP defaults to /Applications/Jaca.app
#   JACA_BIN_DIR=~/bin ./scripts/install-cli.sh # choose where the link goes
#
# Without JACA_BIN_DIR the link goes to /usr/local/bin when it is writable, else to ~/.local/bin.
# `all.sh --install` runs this after copying the app.
set -euo pipefail

APP="${1:-/Applications/Jaca.app}"
JACAD="$APP/Contents/MacOS/jacad"
[ -x "$JACAD" ] || { echo "✗ jacad not found at $JACAD"; exit 1; }

if [ -n "${JACA_BIN_DIR:-}" ]; then
  BIN="$JACA_BIN_DIR"
elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
  BIN=/usr/local/bin
else
  BIN="$HOME/.local/bin"
fi
mkdir -p "$BIN"

# -n: replace an existing link instead of following it into the app bundle.
ln -sfn "$JACAD" "$BIN/jaca"
echo "✓ installed: $BIN/jaca"
