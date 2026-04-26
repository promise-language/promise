#!/usr/bin/env bash
set -euo pipefail

trap 'if [ $? -ne 0 ]; then echo "----------------------------------------------------"; echo "❌ Accept FAILED: formatting or vetting errors"; echo "----------------------------------------------------"; fi' EXIT

cd "$(dirname "$0")/../compiler"

echo "Formatting..."
gofmt -w .

echo "Vetting..."
go vet $(go list ./... | grep -v /internal/parser)

echo ""
echo "✅ OK to Accept"
