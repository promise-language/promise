#!/usr/bin/env bash
# Pre-commit verification: format + vet + all tests.
#
# Usage: bin/verify.sh [--wasm] [--clean] [--local]
#   --wasm   Also run wasm32-wasi target tests
#   --clean  Clear caches first
#   --local  Use ./.promise-home as PROMISE_HOME (avoid polluting ~/.promise)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROMISE="$ROOT/bin/promise"
cd "$ROOT"

trap 'if [ $? -ne 0 ]; then echo "----------------------------------------------------"; echo "❌ Verify FAILED: not safe to commit"; echo "----------------------------------------------------"; fi' EXIT

WASM=false
LOCAL=false
CLEAN=false
for arg in "$@"; do
  case "$arg" in
    --wasm) WASM=true ;;
    --clean) CLEAN=true ;;
    --local) LOCAL=true ;;
    *) echo "Usage: bin/verify.sh [--wasm] [--clean] [--local]"; exit 1 ;;
  esac
done

if [ "$LOCAL" = true ]; then
  export PROMISE_HOME="$ROOT/.promise-home"
  export TMPDIR="$ROOT/.promise-home/tmp"
  if [ "$CLEAN" = true ]; then
    rm -rf "$TMPDIR"
  fi
  mkdir -p "$TMPDIR"
fi

VERIFY_START=$SECONDS

echo "Formatting go..."
(cd compiler && gofmt -w .)

# Format .pr files before building so that embedded resources include formatted source.
# Requires an existing bin/promise binary (from a prior build); first build bootstraps without formatting.
if [ -x "$PROMISE" ]; then
  echo "Formatting promise..."
  find . -name '*.pr' -not -path './.git/*' -not -path './compiler/*' -not -path './.promise-home/*' | xargs "$PROMISE" format
  echo ""
fi

echo "Building compiler..."
./build

echo "Vetting go..."
(cd compiler && go vet $(go list ./... | grep -v /internal/parser))

if [ "$CLEAN" = true ]; then
  echo "Clearing go test cache..."
  (cd compiler && go clean -testcache)
  echo "Clearing promise test cache..."
  "$PROMISE" clean
fi

# --- Go tests ---
echo "Running go tests..."
(cd compiler && go test ./...) || exit 1

# --- Promise tests (host) ---
echo ""
echo "Running promise tests (host)..."
HOST_LOG=$(mktemp)
"$PROMISE" test -timeout 10 tests/... modules/... examples/... 2>&1 | tee "$HOST_LOG" || exit 1
HOST_SUMMARY=$(grep -E '^[0-9]+ passed, [0-9]+ failed' "$HOST_LOG" | tail -1)
rm -f "$HOST_LOG"

# --- Promise tests (wasm) ---
WASM_SUMMARY=""
if [ "$WASM" = true ]; then
  if ! command -v wasmtime &>/dev/null; then
    echo "ERROR: wasmtime not found. Install with: bin/install-prereqs.sh --wasm"
    exit 1
  fi
  echo ""
  echo "Running promise tests (wasm32-wasi)..."
  WASM_LOG=$(mktemp)
  "$PROMISE" test -timeout 10 -target wasm32-wasi tests/... modules/... examples/... 2>&1 | tee "$WASM_LOG" || exit 1
  WASM_SUMMARY=$(grep -E '^[0-9]+ passed, [0-9]+ failed' "$WASM_LOG" | tail -1)
  rm -f "$WASM_LOG"
fi

# --- Summary ---
VERIFY_ELAPSED=$(( SECONDS - VERIFY_START ))
VERIFY_MIN=$(( VERIFY_ELAPSED / 60 ))
VERIFY_SEC=$(( VERIFY_ELAPSED % 60 ))
HOST_TARGET=$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m)

echo ""
echo "===================================================="
echo "  Verify Summary"
echo "----------------------------------------------------"
printf "  Host target:  %s\n" "$HOST_TARGET"
printf "  Host tests:   %s\n" "${HOST_SUMMARY:-all passed}"
if [ "$WASM" = true ]; then
  printf "  WASM tests:   %s\n" "${WASM_SUMMARY:-all passed}"
fi
printf "  Total time:   %dm%02ds\n" "$VERIFY_MIN" "$VERIFY_SEC"
echo "===================================================="
echo "✅ OK to Commit"
