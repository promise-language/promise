#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

$HOME/go/bin/agent_manager --config config.yaml "$@"
