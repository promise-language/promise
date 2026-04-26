#!/usr/bin/env bash
# E2E test runner: compiles and runs .pr files, checks output against .expected files
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$ROOT_DIR/tests/e2e"
WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

# Build the compiler
echo "Building compiler..."
cd "$ROOT_DIR/compiler"
if ! go build -o "$WORKDIR/promise" ./cmd/promise; then
  echo "ERROR: compiler build failed"
  exit 1
fi

PASS=0
FAIL=0

for prfile in "$TEST_DIR"/*.pr; do
  [ -f "$prfile" ] || continue
  name=$(basename "$prfile" .pr)
  expected="$TEST_DIR/${name}.expected"

  if [ ! -f "$expected" ]; then
    echo "SKIP $name (no .expected file)"
    continue
  fi

  # Compile (suppress "Compiled..." message, show errors on failure)
  compile_out=$("$WORKDIR/promise" build "$prfile" -o "$WORKDIR/$name" 2>&1)
  if [ $? -ne 0 ]; then
    echo "FAIL $name (compilation failed)"
    echo "$compile_out" | grep -v "^Compiled " | head -5
    FAIL=$((FAIL + 1))
    continue
  fi

  # Run and capture output
  actual=$("$WORKDIR/$name" 2>&1) || true
  expected_text=$(cat "$expected")

  if [ "$actual" = "$expected_text" ]; then
    echo "PASS $name"
    PASS=$((PASS + 1))
  else
    echo "FAIL $name"
    echo "  expected: $(head -3 "$expected")"
    echo "  actual:   $(echo "$actual" | head -3)"
    FAIL=$((FAIL + 1))
  fi
done

echo ""
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
