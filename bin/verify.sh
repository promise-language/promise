#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../compiler"

echo "Running all tests..."
go test ./... || { echo "FAILED"; exit 1; }

echo ""
echo "OK to Commit"
