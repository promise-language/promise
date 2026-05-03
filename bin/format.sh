#!/usr/bin/env bash
# Format all Promise source files in the project.
#
# Usage: bin/format.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROMISE="$ROOT/bin/promise"
cd "$ROOT"

trap 'if [ $? -ne 0 ]; then echo "----------------------------------------------------"; echo "❌ FAILED: format"; echo "----------------------------------------------------"; fi' EXIT

# Build the compiler first
./build

find . -name '*.pr' -not -path './.git/*' -not -path './compiler/*' -not -path './.promise-home/*' | xargs "$PROMISE" format

echo "✅ OK"
