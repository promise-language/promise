#!/usr/bin/env bash
# Build the Promise compiler (release) and install it to ~/.promise/
set -euo pipefail

cd "$(dirname "$0")/../compiler"

# Copy std resources for go:embed
echo "Preparing resources..."
rm -fr cmd/promise/resources/std
mkdir -p cmd/promise/resources/std
cp ../std/*.pr cmd/promise/resources/std/

# Copy musl CRT on Linux
BUILD_TAGS=""
if [ "$(uname -s)" = "Linux" ]; then
  MUSL_CRT_SRC=/usr/lib/x86_64-linux-musl
  MUSL_CRT_DST=cmd/promise/resources/crt/x86_64-linux-musl
  if [ -f "$MUSL_CRT_SRC/libc.a" ]; then
    mkdir -p "$MUSL_CRT_DST"
    cp "$MUSL_CRT_SRC/crt1.o" "$MUSL_CRT_SRC/crti.o" "$MUSL_CRT_SRC/crtn.o" "$MUSL_CRT_SRC/libc.a" "$MUSL_CRT_DST/"
  else
    echo "WARNING: musl-dev not installed, skipping CRT embed"
  fi

  # Bundle LLVM tools for self-contained release binary
  echo "Bundling LLVM tools..."
  make llvm-bundle
  BUILD_TAGS="-tags embed_llvm"
fi

# Build
echo "Building compiler..."
if ! go build -buildvcs=false $BUILD_TAGS -o promise ./cmd/promise; then
  echo "ERROR: build failed"
  exit 1
fi

echo "Installing..."
./promise install
