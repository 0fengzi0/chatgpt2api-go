#!/bin/sh
set -eu

APP_DIR="${APP_DIR:-/www/wwwroot/chatgpt2api}"
CONFIG="${CONFIG:-$APP_DIR/config.yaml}"
BIN="${BIN:-$APP_DIR/chatgpt2api}"

if [ ! -x "$BIN" ]; then
    echo "chatgpt2api binary not found: $BIN" >&2
    exit 1
fi

exec "$BIN" -config "$CONFIG" -cleanup-images
