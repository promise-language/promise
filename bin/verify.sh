#!/usr/bin/env bash
set -euo pipefail

trap 'if [ $? -ne 0 ]; then echo "----------------------------------------------------"; echo "❌ Verify FAILED: tests did not pass"; echo "----------------------------------------------------"; fi' EXIT

cd "$(dirname "$0")/../compiler"

echo "Formatting..."
gofmt -w .

echo "Vetting..."
go vet $(go list ./... | grep -v /internal/parser)

echo "Building all packages..."
go build ./... 2>&1

echo "Running all tests..."
go test ./... || exit 1

echo ""
echo "✅ OK to Commit"
