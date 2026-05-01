package main

import _ "embed"

//go:embed crt/wasm32/wasm_alloc.o
var embeddedWasmAllocObj []byte
