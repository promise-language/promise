#!/usr/bin/env bash
set -euo pipefail

trap 'if [ $? -ne 0 ]; then echo "----------------------------------------------------"; echo "❌ Verify FAILED: tests did not pass"; echo "----------------------------------------------------"; fi' EXIT

cd "$(dirname "$0")/../compiler"

echo "Running all tests..."
go test ./... || exit 1

echo ""
echo "✅ OK to Commit"
