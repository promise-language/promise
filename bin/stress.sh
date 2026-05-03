#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROMISE="$ROOT/bin/promise"
cd "$ROOT"

# Always use local cache/temp (equivalent to verify.sh --local)
export PROMISE_HOME="$ROOT/.promise-home"
export TMPDIR="$ROOT/.promise-home/tmp"
mkdir -p "$TMPDIR"

clear

echo "Building compiler..."
./build

echo "Running continuous stress test (Ctrl+C to stop)..."
exec "$PROMISE" test -timeout 5s -stress tests/... modules/...
