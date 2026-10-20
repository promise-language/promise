#!/usr/bin/env bash
# build_wasm_alloc.sh — rebuild wasm_alloc.o.
#
# wasm_alloc.o is a checked-in prebuilt: it is what the compiler //go:embed's
# and links into every wasm module, so an edit to wasm_alloc.c does nothing
# until this script is run and the object is committed with it. A helper
# defined in the source but missing from the object is not a link error —
# wasm-ld's --allow-undefined turns it into a host import and the module
# refuses to instantiate (T2128). TestWasmAllocObjMatchesSource
# (compiler/internal/codegen) fails when the two disagree.
#
# Usage: ./build_wasm_alloc.sh <clang>
#
# <clang> is the clang from the LLVM release pinned in
# tools/build/prebuilts.toml ([binaries.llvm]) — the release the compiler's own
# opt/llc/wasm-ld come from. A clang reporting any other version is refused:
# the object ships inside the compiler, so its bytes must be reproducible from
# the pin, not from whichever clang was at hand.

set -euo pipefail

if [ $# -ne 1 ]; then
  echo "usage: build_wasm_alloc.sh <clang>" >&2
  exit 2
fi
clang=$1
here=$(dirname "$0")
prebuilts=$(cd "$here/../../../../.." && pwd)/tools/build/prebuilts.toml

pinned=$(awk '/^\[/ { llvm = ($0 == "[binaries.llvm]") }
  llvm && /^version *=/ { gsub(/^[^"]*"|".*$/, ""); print; exit }' "$prebuilts")
if [ -z "$pinned" ]; then
  echo "build_wasm_alloc.sh: no [binaries.llvm] version in $prebuilts" >&2
  exit 1
fi
version=$("$clang" --version)
version=${version%%$'\n'*}
case $version in
  "clang version $pinned "*) ;;
  *)
    echo "build_wasm_alloc.sh: $clang is \"$version\", not the pinned LLVM $pinned ($prebuilts)" >&2
    exit 1
    ;;
esac

"$clang" --target=wasm32-wasip1 -O2 -nostdlib -c "$here/wasm_alloc.c" -o "$here/wasm_alloc.o"

echo "wrote $here/wasm_alloc.o ($(wc -c < "$here/wasm_alloc.o" | tr -d ' ') bytes)" >&2
