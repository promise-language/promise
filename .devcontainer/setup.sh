#!/usr/bin/env bash
# Dev container provisioning for Promise.
#
# Installs only what the image and devcontainer features do not already
# provide. Deliberately NOT installed (see devcontainer.json): musl-dev, LLVM,
# lld, java/antlr — bin/build fetches the LLVM + musl CRT prebuilts itself, and
# the generated parser is committed.
set -euo pipefail

# wasmtime — the runtime for `bin/test --wasm` / `bin/verify --wasm`, which is
# the documented pre-commit gate. CI installs it via the bytecode-alliance
# action; there is no devcontainer feature for it, so use the official script.
if ! command -v wasmtime >/dev/null 2>&1; then
  echo "==> installing wasmtime"
  curl -sSf https://wasmtime.dev/install.sh | bash
  # The installer drops it in ~/.wasmtime/bin and appends to the shell profile,
  # which a non-login container shell will not have sourced yet.
  echo 'export PATH="$HOME/.wasmtime/bin:$PATH"' >>"$HOME/.bashrc"
fi

echo "==> done. Next:"
echo "    bin/build         # fetches pinned LLVM + musl CRT on first run (~1 GB cache)"
echo "    bin/verify --wasm # format + vet + full suite"
