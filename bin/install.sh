#!/usr/bin/env bash
# Build the Promise compiler (release) and install it to ~/.promise/
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

echo "Building compiler (release)..."
./build --release

echo "Installing..."
bin/promise install
