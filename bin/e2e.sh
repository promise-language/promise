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

# --- promise test: all-pass ---
test_name="test_assert (promise test)"
actual=$("$WORKDIR/promise" test "$TEST_DIR/test_assert.pr" 2>&1) || true
expected_text="PASS test_addition
PASS test_math
PASS test_strings

3 passed, 0 failed"
if [ "$actual" = "$expected_text" ]; then
  echo "PASS $test_name"
  PASS=$((PASS + 1))
else
  echo "FAIL $test_name"
  echo "  expected: $(echo "$expected_text" | head -3)"
  echo "  actual:   $(echo "$actual" | head -3)"
  FAIL=$((FAIL + 1))
fi

# --- promise test: mixed pass/fail ---
test_name="test_fail (promise test)"
exit_code=0
actual=$("$WORKDIR/promise" test "$TEST_DIR/test_fail.pr" 2>&1) || exit_code=$?
expected_text="PASS test_pass
panic: deliberate failure
FAIL test_fail

1 passed, 1 failed"
if [ "$actual" = "$expected_text" ] && [ "$exit_code" -ne 0 ]; then
  echo "PASS $test_name"
  PASS=$((PASS + 1))
else
  echo "FAIL $test_name"
  echo "  expected: $(echo "$expected_text" | head -3)"
  echo "  actual:   $(echo "$actual" | head -3)"
  if [ "$exit_code" -eq 0 ]; then
    echo "  (expected non-zero exit code, got 0)"
  fi
  FAIL=$((FAIL + 1))
fi

# --- promise test: no tests found ---
test_name="no_tests_found (promise test)"
actual=$("$WORKDIR/promise" test "$TEST_DIR/std_basics.pr" 2>&1) || true
if echo "$actual" | grep -q "no tests found"; then
  echo "PASS $test_name"
  PASS=$((PASS + 1))
else
  echo "FAIL $test_name"
  echo "  expected output containing 'no tests found'"
  echo "  actual:   $(echo "$actual" | head -3)"
  FAIL=$((FAIL + 1))
fi

# --- promise test: directory scanning (std tests) ---
test_name="std_tests (promise test dir)"
exit_code=0
actual=$("$WORKDIR/promise" test "$ROOT_DIR/tests/std/" 2>&1) || exit_code=$?
if [ "$exit_code" -eq 0 ] && echo "$actual" | grep -q "passed, 0 failed"; then
  echo "PASS $test_name"
  PASS=$((PASS + 1))
else
  echo "FAIL $test_name"
  echo "  actual:   $(echo "$actual" | tail -3)"
  FAIL=$((FAIL + 1))
fi

echo ""
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
