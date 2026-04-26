#!/bin/bash
# E2E test runner: compiles and runs .pr files, checks output against .expected files
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
COMPILER="${SCRIPT_DIR}/../../compiler/cmd/promise"
TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

# Build the compiler
cd "${SCRIPT_DIR}/../../compiler"
go build -o "$TMPDIR/promise" ./cmd/promise 2>/dev/null

PASS=0
FAIL=0

for prfile in "$SCRIPT_DIR"/*.pr; do
  [ -f "$prfile" ] || continue
  name=$(basename "$prfile" .pr)
  expected="$SCRIPT_DIR/${name}.expected"

  if [ ! -f "$expected" ]; then
    echo "SKIP $name (no .expected file)"
    continue
  fi

  # Compile
  if ! "$TMPDIR/promise" build "$prfile" -o "$TMPDIR/$name" 2>/dev/null; then
    echo "FAIL $name (compilation failed)"
    FAIL=$((FAIL + 1))
    continue
  fi

  # Run and capture output
  actual=$("$TMPDIR/$name" 2>&1) || true
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
[ $FAIL -eq 0 ]
