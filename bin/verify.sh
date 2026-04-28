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
go build ./cmd/promise 2>&1

echo "Running all tests..."
go test ./... || exit 1

echo "Running e2e tests..."
bash ../bin/e2e.sh || exit 1

echo ""
echo "✅ OK to Commit"
