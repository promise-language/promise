#!/usr/bin/env bash
set -euo pipefail

trap 'if [ $? -ne 0 ]; then echo "----------------------------------------------------"; echo "❌ Verify FAILED: tests did not pass"; echo "----------------------------------------------------"; fi' EXIT

cd "$(dirname "$0")/../compiler"

echo "Generating parser & resources..."
make generate resources

echo "Formatting..."
gofmt -w .

echo "Vetting..."
go vet $(go list ./... | grep -v /internal/parser)

echo "Building..."
go build -o promise ./cmd/promise 2>&1

echo "Running go tests..."
go test ./... || exit 1

echo "Running promise tests..."
./promise test -timeout 15 ../tests/... || exit 1

echo ""
echo "✅ OK to Commit"
