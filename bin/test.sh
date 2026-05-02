#!/usr/bin/env bash
# Run tests. Builds first, then runs the requested test suites.
#
# Usage: bin/test.sh [go|promise|all] [--wasm] [--clean]
#   go       Go unit tests only (go test ./...)
#   promise  Promise tests only (bin/promise test tests/... modules/...)
#   all      Both (default)
#   --wasm   Also run wasm32-wasi target for Promise tests
#   --clean  Clear go test cache + promise build cache first
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROMISE="$ROOT/bin/promise"
cd "$ROOT"

trap 'if [ $? -ne 0 ]; then echo "----------------------------------------------------"; echo "❌ Tests FAILED"; echo "----------------------------------------------------"; fi' EXIT

SUITE="all"
WASM=false
CLEAN=false

for arg in "$@"; do
  case "$arg" in
    go|promise|all) SUITE="$arg" ;;
    --wasm) WASM=true ;;
    --clean) CLEAN=true ;;
    *) echo "Usage: bin/test.sh [go|promise|all] [--wasm] [--clean]"; exit 1 ;;
  esac
done

echo "Building..."
./build

if [ "$CLEAN" = true ]; then
  echo "Clearing go test cache..."
  (cd compiler && go clean -testcache)
  echo "Clearing promise test cache..."
  "$PROMISE" clean
fi

if [ "$SUITE" = "go" ] || [ "$SUITE" = "all" ]; then
  echo ""
  echo "Running go tests..."
  (cd compiler && go test ./...) || exit 1
fi

if [ "$SUITE" = "promise" ] || [ "$SUITE" = "all" ]; then
  echo ""
  echo "Running promise tests (host)..."
  "$PROMISE" test -timeout 10 tests/... modules/... || exit 1

  if [ "$WASM" = true ]; then
    if ! command -v wasmtime &>/dev/null; then
      echo "ERROR: wasmtime not found. Install with: bin/install-prereqs.sh --wasm"
      exit 1
    fi
    echo ""
    echo "Running promise tests (wasm32-wasi)..."
    "$PROMISE" test -timeout 30 -target wasm32-wasi tests/... modules/... || exit 1
  fi
fi

echo ""
echo "✅ All tests passed"
