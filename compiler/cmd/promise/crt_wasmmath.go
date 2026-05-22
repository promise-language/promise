package main

import _ "embed"

// embeddedWasmMathObj holds the precompiled wasm32 math runtime, built from
// crt/wasm32/wasm_math.c (the user-facing sqrt/fabs/floor/ceil/round wrappers
// that delegate to __builtin_*) plus the vendored musl libm sources under
// crt/wasm32/musl/ (sin/cos/tan/exp/log/pow + f32 variants and their kernel
// helpers — full IEEE 754, ~1 ULP accuracy, NaN/Inf/0 special cases handled).
//
// To rebuild: run crt/wasm32/build_wasm_math.sh. The result is committed and
// embedded here so end users don't need clang+wasm-ld at compile time.
//
//go:embed crt/wasm32/wasm_math.o
var embeddedWasmMathObj []byte
