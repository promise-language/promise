#!/usr/bin/env bash
# Build the Promise compiler and install it to ~/.promise/
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR/compiler"

# Copy resources for go:embed
echo "Preparing resources..."
mkdir -p cmd/promise/resources/std cmd/promise/resources/runtime
cp ../std/*.pr cmd/promise/resources/std/
cp ../runtime/*.c ../runtime/*.h cmd/promise/resources/runtime/

# Build (skip ANTLR generate if parser already exists)
echo "Building compiler..."
if ! go build -o promise ./cmd/promise; then
  echo "ERROR: build failed"
  exit 1
fi

echo "Installing..."
./promise install
