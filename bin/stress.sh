#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../compiler"

clear

echo "Building compiler..."
go build -o promise ./cmd/promise 2>&1

echo "Running continuous stress test (Ctrl+C to stop)..."
exec ./promise test -timeout 5s -stress ../tests/...
