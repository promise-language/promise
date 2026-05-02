#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROMISE="$ROOT/bin/promise"
cd "$ROOT"

trap 'if [ $? -ne 0 ]; then echo "----------------------------------------------------"; echo "❌ Verify FAILED: tests did not pass"; echo "----------------------------------------------------"; fi' EXIT

MODE="host"
CLEAN_CACHE=false

for arg in "$@"; do
  case "$arg" in
    host|wasm|all) MODE="$arg" ;;
    --clean) CLEAN_CACHE=true ;;
    *) echo "Usage: verify.sh [host|wasm|all] [--clean]"; exit 1 ;;
  esac
done

echo "Formatting..."
(cd compiler && gofmt -w .)

echo "Vetting..."
(cd compiler && go vet $(go list ./... | grep -v /internal/parser))

echo "Building..."
./build  # generates parser + embeds resources + compiles

if [ "$CLEAN_CACHE" = true ]; then
  echo "Clearing go test cache..."
  (cd compiler && go clean -testcache) || exit 1
  echo "Clearing promise test cache..."
  "$PROMISE" clean
fi

echo "Running go tests..."
(cd compiler && go test ./...) || exit 1

if [ "$MODE" = "host" ] || [ "$MODE" = "all" ]; then
  echo ""
  echo "Running promise tests (host)..."
  "$PROMISE" test -timeout 10 tests/... modules/... || exit 1
fi

if [ "$MODE" = "wasm" ] || [ "$MODE" = "all" ]; then
  if ! command -v wasmtime &>/dev/null; then
    echo "ERROR: wasmtime not found. Install with: bin/install-prereqs.sh --wasm"
    exit 1
  fi
  echo ""
  echo "Running promise tests (wasm32-wasi)..."
  "$PROMISE" test -timeout 10 -target wasm32-wasi tests/... modules/... || exit 1
fi

echo ""
echo "✅ OK to Commit"
