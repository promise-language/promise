#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../compiler"

echo "Formatting..."
gofmt -w .

echo "Vetting..."
go vet $(go list ./... | grep -v /internal/parser)

echo ""
echo "OK to Accept"
