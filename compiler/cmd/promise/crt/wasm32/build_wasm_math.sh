#!/usr/bin/env bash
# build_wasm_math.sh — rebuild wasm_math.o.
#
# Compiles wasm_math.c plus the vendored musl libm sources under musl/ as
# separate translation units (necessary because musl files contain file-static
# helpers with overlapping names — `pio4`, `T[]`, `top12()`, etc. — which
# would collide in a single-TU amalgamation), then links them into a single
# relocatable wasm object via `wasm-ld --relocatable`.
#
# Run from this directory: ./build_wasm_math.sh
#
# Tools required: clang (with wasm32 target), wasm-ld (LLVM's WebAssembly
# linker, usually shipped alongside clang). Override CLANG / WASMLD via env
# if they aren't on PATH.

set -euo pipefail

CLANG=${CLANG:-clang}
WASMLD=${WASMLD:-wasm-ld}

# Resolve wasm-ld via the same toolchain as clang if not on PATH.
if ! command -v "$WASMLD" >/dev/null 2>&1; then
  CLANG_BIN_DIR=$(dirname "$(command -v "$CLANG")")
  if [ -x "$CLANG_BIN_DIR/wasm-ld" ]; then
    WASMLD="$CLANG_BIN_DIR/wasm-ld"
  fi
fi

cd "$(dirname "$0")"

CFLAGS=(--target=wasm32-unknown-wasi -O2 -nostdlib -nostdlibinc -ffreestanding -I musl)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

OBJS=()
for src in wasm_math.c musl/*.c; do
  base=$(basename "$src" .c)
  obj="$WORK/$base.o"
  "$CLANG" "${CFLAGS[@]}" -c "$src" -o "$obj"
  OBJS+=("$obj")
done

"$WASMLD" --relocatable -o wasm_math.o "${OBJS[@]}"

echo "wrote $(pwd)/wasm_math.o ($(stat -c%s wasm_math.o 2>/dev/null || stat -f%z wasm_math.o) bytes)"
