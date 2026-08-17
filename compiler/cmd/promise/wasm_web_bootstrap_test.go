package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmitWebBootstrapJSIncludesBasePALGlue verifies the auto-generated
// wasm32-web bootstrap loader implements the base PAL runtime imports
// (promise_env.write/exit/monotonic_nanos) that every wasm32-web binary
// unconditionally imports — WebIDL bindings or not. Before this fix, the
// bootstrap only did WebAssembly.instantiateStreaming with an empty default
// importObject, leaving these unresolved for any program not paired with
// hand-written or WebIDL-bindgen-generated glue.
func TestEmitWebBootstrapJSIncludesBasePALGlue(t *testing.T) {
	dir := t.TempDir()
	wasmFile := filepath.Join(dir, "frontend.wasm")

	emitWebBootstrapJS(wasmFile)

	jsFile := filepath.Join(dir, "frontend.js")
	data, err := os.ReadFile(jsFile)
	if err != nil {
		t.Fatalf("expected bootstrap JS at %s: %v", jsFile, err)
	}
	content := string(data)

	for _, want := range []string{"write(", "exit(", "monotonic_nanos("} {
		if !strings.Contains(content, want) {
			t.Errorf("bootstrap JS missing base PAL import %q", want)
		}
	}
}

// TestEmitWebBootstrapJSMergesImportObject verifies init() deep-merges a
// caller-supplied importObject.promise_env on top of the base PAL defaults
// instead of replacing them outright — so a project can pass in WebIDL
// bindgen's generated importObject and get both halves (base glue + WebIDL
// bindings) working together.
func TestEmitWebBootstrapJSMergesImportObject(t *testing.T) {
	dir := t.TempDir()
	wasmFile := filepath.Join(dir, "frontend.wasm")

	emitWebBootstrapJS(wasmFile)

	data, err := os.ReadFile(filepath.Join(dir, "frontend.js"))
	if err != nil {
		t.Fatalf("read bootstrap JS: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "...basePromiseEnv") || !strings.Contains(content, "...(importObject.promise_env || {})") {
		t.Error("init() should merge importObject.promise_env on top of basePromiseEnv, not replace it")
	}
	if !strings.Contains(content, "...importObject") {
		t.Error("init() should spread the caller's importObject rather than only accepting {}")
	}
}

// TestEmitWebBootstrapJSCatchesExitUnwind verifies that init() catches the
// exit-unwind sentinel around _initialize(): once base PAL glue provides a
// real (throwing) exit() implementation, a normal successful main() return —
// which lowers to pal_exit(0) — must not surface as an uncaught JS exception.
func TestEmitWebBootstrapJSCatchesExitUnwind(t *testing.T) {
	dir := t.TempDir()
	wasmFile := filepath.Join(dir, "frontend.wasm")

	emitWebBootstrapJS(wasmFile)

	data, err := os.ReadFile(filepath.Join(dir, "frontend.js"))
	if err != nil {
		t.Fatalf("read bootstrap JS: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "try {") || !strings.Contains(content, "promise_env.exit") {
		t.Error("init() should try/catch _initialize() and swallow the promise_env.exit sentinel")
	}
}
