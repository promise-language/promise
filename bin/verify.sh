#!/usr/bin/env bash
set -euo pipefail

trap 'if [ $? -ne 0 ]; then echo "----------------------------------------------------"; echo "❌ Verify FAILED: tests did not pass"; echo "----------------------------------------------------"; fi' EXIT

MODE="${1:-host}"  # host (default), wasm, all

cd "$(dirname "$0")/../compiler"

echo "Generating parser & resources..."
if [ "$(uname -s)" = "Linux" ]; then
  make generate resources musl-crt
else
  make generate resources
fi

echo "Formatting..."
gofmt -w .

echo "Vetting..."
go vet $(go list ./... | grep -v /internal/parser)

echo "Building..."
go build -o promise ./cmd/promise 2>&1

echo "Running go tests..."
go test ./... || exit 1

if [ "$MODE" = "host" ] || [ "$MODE" = "all" ]; then
  echo ""
  echo "Running promise tests (host)..."
  ./promise test -timeout 10 ../tests/... || exit 1
fi

if [ "$MODE" = "wasm" ] || [ "$MODE" = "all" ]; then
  if ! command -v wasmtime &>/dev/null; then
    echo "ERROR: wasmtime not found. Install with: bin/install-prereqs.sh --wasm"
    exit 1
  fi
  echo ""
  echo "Running promise tests (wasm32-wasi)..."
  ./promise test -timeout 30 -target wasm32-wasi ../tests/... || exit 1
fi

echo ""
echo "✅ OK to Commit"
