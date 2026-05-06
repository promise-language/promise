#!/usr/bin/env bash
# Local developer install — builds a release binary from source and installs it.
# Use this when iterating on the compiler itself.
#
# For end-user installation from a published release, use scripts/install.sh:
#   curl -sSf https://promise-lang.dev/install.sh | sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

echo "Building compiler (release)..."
./build --release

echo "Installing into epochs/dev/..."
bin/promise install --dev
