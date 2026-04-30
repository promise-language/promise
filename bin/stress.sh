#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../compiler"

if [ ! -f ./promise ]; then
    echo "Building compiler..."
    go build -o promise ./cmd/promise 2>&1
fi

echo "Running continuous stress test (Ctrl+C to stop)..."
exec ./promise test -stress ../tests/...
