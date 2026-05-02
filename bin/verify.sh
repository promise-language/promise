#!/usr/bin/env bash
# Pre-commit verification: format + vet + all tests.
#
# Usage: bin/verify.sh [--wasm] [--clean]
#   --wasm   Also run wasm32-wasi target tests
#   --clean  Clear caches first
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

trap 'if [ $? -ne 0 ]; then echo "----------------------------------------------------"; echo "❌ Verify FAILED: not safe to commit"; echo "----------------------------------------------------"; fi' EXIT

TEST_ARGS=()
for arg in "$@"; do
  case "$arg" in
    --wasm|--clean) TEST_ARGS+=("$arg") ;;
    *) echo "Usage: bin/verify.sh [--wasm] [--clean]"; exit 1 ;;
  esac
done

echo "Formatting..."
(cd compiler && gofmt -w .)

echo "Vetting..."
(cd compiler && go vet $(go list ./... | grep -v /internal/parser))

bin/test.sh all "${TEST_ARGS[@]+"${TEST_ARGS[@]}"}"

echo ""
echo "✅ OK to Commit"
