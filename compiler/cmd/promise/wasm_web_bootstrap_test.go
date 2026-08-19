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
// It also checks for the typed _ExitSignal sentinel (rather than matching a
// bare Error's .message string, which would swallow any unrelated JS error
// carrying the same text) and that a non-zero code is checked, not just the
// sentinel's identity — see TestEmitWebBootstrapJSSmokeSurfacesNonZeroExit
// for the corresponding behavioral proof that exit(1) actually propagates.
func TestEmitWebBootstrapJSCatchesExitUnwind(t *testing.T) {
	dir := t.TempDir()
	wasmFile := filepath.Join(dir, "frontend.wasm")

	emitWebBootstrapJS(wasmFile)

	data, err := os.ReadFile(filepath.Join(dir, "frontend.js"))
	if err != nil {
		t.Fatalf("read bootstrap JS: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "class _ExitSignal") {
		t.Error("expected a typed _ExitSignal sentinel class instead of bare Error message matching")
	}
	if !strings.Contains(content, "try {") || !strings.Contains(content, "instanceof _ExitSignal") {
		t.Error("init() should try/catch _initialize() and check instanceof _ExitSignal")
	}
	if !strings.Contains(content, "e.code !== 0") {
		t.Error("init() should distinguish exit(0) from a non-zero exit code, not swallow every exit unconditionally")
	}
}
