#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROMISE="$ROOT/bin/promise"
cd "$ROOT"

trap 'if [ $? -ne 0 ]; then echo "----------------------------------------------------"; echo "❌ Verify FAILED: tests did not pass"; echo "----------------------------------------------------"; fi' EXIT

MODE="${1:-host}"  # host (default), wasm, all

echo "Building..."
./build  # generates parser + embeds resources + compiles

echo "Running go tests..."
(cd compiler && go test ./...) || exit 1

if [ "$MODE" = "host" ] || [ "$MODE" = "all" ]; then
  echo ""
  echo "Running promise tests (host)..."
  "$PROMISE" test -timeout 10 tests/... || exit 1
fi

if [ "$MODE" = "wasm" ] || [ "$MODE" = "all" ]; then
  if ! command -v wasmtime &>/dev/null; then
    echo "ERROR: wasmtime not found. Install with: bin/install-prereqs.sh --wasm"
    exit 1
  fi
  echo ""
  echo "Running promise tests (wasm32-wasi)..."
  "$PROMISE" test -timeout 30 -target wasm32-wasi tests/... || exit 1
fi

echo ""
echo "✅ OK to Commit"
