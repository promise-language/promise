#!/usr/bin/env bash
# Build the Promise compiler binary
set -euo pipefail

cd "$(dirname "$0")/../compiler"

# Copy resources for go:embed
mkdir -p cmd/promise/resources/std
cp ../std/*.pr cmd/promise/resources/std/

# Copy musl CRT on Linux
if [ "$(uname -s)" = "Linux" ]; then
  MUSL_CRT_SRC=/usr/lib/x86_64-linux-musl
  MUSL_CRT_DST=cmd/promise/resources/crt/x86_64-linux-musl
  if [ -f "$MUSL_CRT_SRC/libc.a" ]; then
    mkdir -p "$MUSL_CRT_DST"
    cp "$MUSL_CRT_SRC/crt1.o" "$MUSL_CRT_DST/"
    cp "$MUSL_CRT_SRC/crti.o" "$MUSL_CRT_DST/"
    cp "$MUSL_CRT_SRC/crtn.o" "$MUSL_CRT_DST/"
    cp "$MUSL_CRT_SRC/libc.a" "$MUSL_CRT_DST/"
  else
    echo "WARNING: musl-dev not installed, skipping CRT embed (install with: sudo apt install musl-dev)"
  fi
fi

# Build (skip ANTLR generate if parser already exists)
if ! go build -o promise ./cmd/promise; then
  echo "ERROR: build failed"
  exit 1
fi

echo "Built: compiler/promise"
