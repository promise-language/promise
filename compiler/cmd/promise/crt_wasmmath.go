package main

import _ "embed"

//go:embed crt/wasm32/wasm_math.o
var embeddedWasmMathObj []byte
