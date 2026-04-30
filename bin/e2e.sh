#!/usr/bin/env bash
set -euo pipefail

trap 'if [ $? -ne 0 ]; then echo "----------------------------------------------------"; echo "❌ Verify FAILED: tests did not pass"; echo "----------------------------------------------------"; fi' EXIT

cd "$(dirname "$0")/../compiler"

echo "Generating parser & resources..."
if [ "$(uname -s)" = "Linux" ]; then
  make generate resources musl-crt
else
  make generate resources
fi

echo "Building..."
go build -o promise ./cmd/promise 2>&1

echo "Running go tests..."
go test ./... || exit 1

echo "Running promise tests..."
./promise test -timeout 60 ../tests/... || exit 1

echo ""
echo "✅ OK to Commit"
