#!/usr/bin/env bash
# Set up the local development environment for the Promise compiler repo.
# Run once after cloning. Safe to re-run (idempotent).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Enable tracked git hooks (.githooks/ directory)
git -C "$ROOT" config core.hooksPath .githooks
echo "Git hooks enabled (.githooks/)"
