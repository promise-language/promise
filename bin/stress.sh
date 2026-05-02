#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROMISE="$ROOT/bin/promise"
cd "$ROOT"

clear

echo "Building compiler..."
./build

echo "Running continuous stress test (Ctrl+C to stop)..."
exec "$PROMISE" test -timeout 5s -stress tests/...
