#!/usr/bin/env bash
# Run test coverage analysis for Go packages and/or Promise test files.
#
# Usage: bin/coverage.sh [go|promise|all] [paths...]
#   go       Go test coverage only
#   promise  Promise test coverage only
#   all      Both (default)
#   paths... Go package paths (e.g., ./internal/codegen/) or Promise test
#            files/directories (e.g., tests/e2e/)
#
# Examples:
#   bin/coverage.sh                              # Go + Promise coverage for recent changes
#   bin/coverage.sh go ./internal/codegen/       # Go coverage for codegen package
#   bin/coverage.sh promise tests/e2e/           # Promise coverage for e2e tests
#   bin/coverage.sh promise tests/std/           # Promise coverage for std tests
#   bin/coverage.sh all                          # Full Go + Promise coverage
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROMISE="$ROOT/bin/promise"
cd "$ROOT"

SUITE="all"
PATHS=()

for arg in "$@"; do
  case "$arg" in
    go|promise|all) SUITE="$arg" ;;
    *) PATHS+=("$arg") ;;
  esac
done

# --- Go coverage ---
run_go_coverage() {
  local pkg="${1:-./...}"
  echo "=== Go Coverage: $pkg ==="
  echo ""
  local covfile funcfile
  covfile=$(mktemp /tmp/promise_cov_XXXXXX.out)
  funcfile=$(mktemp /tmp/promise_func_XXXXXX.txt)
  # Run from compiler/ where go.mod lives
  (cd compiler && go test "$pkg" -coverprofile="$covfile" -count=1) || true
  echo ""
  if [ -s "$covfile" ]; then
    # Capture full output once to avoid running go tool cover twice
    (cd compiler && go tool cover -func="$covfile") > "$funcfile" 2>&1 || true
    tail -1 "$funcfile"
    echo ""
    echo "Functions below 70% coverage:"
    awk -F'\t' '
      NR > 0 && $NF != "100.0%" {
        pct = $NF + 0
        if (pct < 70 && pct >= 0 && $NF != "") { print $0; count++ }
        if (count >= 30) exit
      }
    ' "$funcfile"
    echo ""
    rm -f "$funcfile"
  fi
  rm -f "$covfile"
}

# --- Promise coverage ---
run_promise_coverage() {
  local target="$1"
  echo "=== Promise Coverage: $target ==="
  echo ""
  "$PROMISE" test -coverage -timeout 30 "$target" 2>&1 || true
}

# Determine Go packages and Promise targets from arguments or recent changes.
go_packages=()
promise_targets=()

if [ ${#PATHS[@]} -gt 0 ]; then
  for p in "${PATHS[@]}"; do
    if [[ "$p" == ./* ]] || [[ "$p" == */ ]]; then
      # Looks like a Go package path
      go_packages+=("$p")
    elif [[ "$p" == *.pr ]]; then
      promise_targets+=("$p")
    else
      # Directory — could be either
      if [ -d "compiler/$p" ]; then
        go_packages+=("$p")
      fi
      if [ -d "$p" ]; then
        promise_targets+=("$p")
      fi
    fi
  done
else
  # Default: use all packages / all test directories
  go_packages+=("./...")
  promise_targets+=("tests/..." "modules/...")
fi

if [ "$SUITE" = "go" ] || [ "$SUITE" = "all" ]; then
  for pkg in "${go_packages[@]}"; do
    run_go_coverage "$pkg"
  done
fi

if [ "$SUITE" = "promise" ] || [ "$SUITE" = "all" ]; then
  for target in "${promise_targets[@]}"; do
    run_promise_coverage "$target"
  done
fi
