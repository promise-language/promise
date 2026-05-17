package main

// embeddedWasiAdapter holds the WASI P1→P2 adapter (wasi_snapshot_preview1.command.wasm).
// When non-empty, it's automatically used by -component to wrap core wasm into a
// component that translates WASI Preview 1 imports to Preview 2.
//
// To embed the adapter:
// 1. Download from wasmtime releases (wasi_snapshot_preview1.command.wasm)
// 2. Place in crt/wasm32/wasi_snapshot_preview1.command.wasm
// 3. Uncomment the go:embed directive below
//
// Without an embedded adapter, users can provide their own via -adapt <path>.

// Uncomment when adapter is available:
// //go:embed crt/wasm32/wasi_snapshot_preview1.command.wasm
// var embeddedWasiAdapter []byte

var embeddedWasiAdapter []byte
