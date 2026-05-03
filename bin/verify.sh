#!/usr/bin/env bash
# Pre-commit verification: format + vet + all tests.
#
# Usage: bin/verify.sh [--wasm] [--clean] [--local]
#   --wasm   Also run wasm32-wasi target tests
#   --clean  Clear caches first
#   --local  Use ./.promise-home as PROMISE_HOME (avoid polluting ~/.promise)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

trap 'if [ $? -ne 0 ]; then echo "----------------------------------------------------"; echo "❌ Verify FAILED: not safe to commit"; echo "----------------------------------------------------"; fi' EXIT

TEST_ARGS=()
LOCAL=false
CLEAN=false
for arg in "$@"; do
  case "$arg" in
    --wasm) TEST_ARGS+=("$arg") ;;
    --clean) TEST_ARGS+=("$arg"); CLEAN=true ;;
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

echo "Formatting go..."
(cd compiler && gofmt -w .)

echo "Vetting go..."
(cd compiler && go vet $(go list ./... | grep -v /internal/parser))

echo "Formatting promise..."
find . -name '*.pr' -not -path './.git/*' -not -path './compiler/*' -not -path './.promise-home/*' | xargs ./bin/promise format
echo ""

bin/test.sh all "${TEST_ARGS[@]+"${TEST_ARGS[@]}"}"

echo ""
echo "✅ OK to Commit"
