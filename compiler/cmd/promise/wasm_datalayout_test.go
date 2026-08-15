package main

import (
	"strings"
	"testing"
)

// resetWasmDataLayoutCacheKey deletes one triple's entry from the process-wide
// probe cache so a test can force a fresh probe without disturbing other keys.
func resetWasmDataLayoutCacheKey(triple string) {
	wasmDataLayoutMu.Lock()
	defer wasmDataLayoutMu.Unlock()
	delete(wasmDataLayoutCached, triple)
}

// TestDetectWasmDataLayoutProbe verifies the T1544 probe returns the toolchain's
// real wasm32 DataLayout for a valid triple, caches it per-triple, and returns
// the identical string on a second (cache-hit) call. Skipped when no opt is
// available, since the probe execs the toolchain's own opt.
func TestDetectWasmDataLayoutProbe(t *testing.T) {
	if _, err := findLLVMTool("opt"); err != nil {
		t.Skipf("opt not available: %v", err)
	}

	const triple = "wasm32-wasi"
	resetWasmDataLayoutCacheKey(triple)

	got := detectWasmDataLayout(triple)
	if got == "" {
		t.Fatalf("detectWasmDataLayout(%q) returned empty; expected a real layout", triple)
	}
	// Every wasm32 DataLayout opt emits begins with the little-endian, 32-bit
	// pointer prefix. We deliberately do not assert an i128 spec: whether the
	// toolchain emits i128:128 depends on its LLVM version (T0764), and the whole
	// point of the probe is to use whatever it produces rather than guessing.
	if !strings.HasPrefix(got, "e-m:e-p:32:32") {
		t.Errorf("probed layout %q does not look like a wasm32 layout", got)
	}

	// Second call must hit the cache and return the same string.
	if again := detectWasmDataLayout(triple); again != got {
		t.Errorf("cache-hit call returned %q, first call returned %q", again, got)
	}

	// The value must be recorded under the triple key.
	wasmDataLayoutMu.Lock()
	cached, ok := wasmDataLayoutCached[triple]
	wasmDataLayoutMu.Unlock()
	if !ok || cached != got {
		t.Errorf("cache[%q] = (%q, %v), want (%q, true)", triple, cached, ok, got)
	}
}

// TestDetectWasmDataLayoutPerTripleCache verifies the probe caches independently
// per triple (it keys by triple rather than a single sync.Once), so probing the
// wasi triple does not populate or clobber the web triple's slot.
func TestDetectWasmDataLayoutPerTripleCache(t *testing.T) {
	if _, err := findLLVMTool("opt"); err != nil {
		t.Skipf("opt not available: %v", err)
	}

	const a = "wasm32-wasi"
	const b = "wasm32-unknown-wasi"
	resetWasmDataLayoutCacheKey(a)
	resetWasmDataLayoutCacheKey(b)

	la := detectWasmDataLayout(a)
	// Only a is probed; b must still be absent from the cache.
	wasmDataLayoutMu.Lock()
	_, hasB := wasmDataLayoutCached[b]
	wasmDataLayoutMu.Unlock()
	if hasB {
		t.Fatalf("probing %q unexpectedly populated cache for %q", a, b)
	}

	lb := detectWasmDataLayout(b)
	if la == "" || lb == "" {
		t.Fatalf("expected non-empty layouts, got a=%q b=%q", la, lb)
	}
	// Both keys must now be independently cached.
	wasmDataLayoutMu.Lock()
	_, okA := wasmDataLayoutCached[a]
	_, okB := wasmDataLayoutCached[b]
	wasmDataLayoutMu.Unlock()
	if !okA || !okB {
		t.Errorf("expected both triples cached; okA=%v okB=%v", okA, okB)
	}
}

// TestDetectWasmDataLayoutProbeFailureFallsBack verifies that when opt cannot be
// executed, the probe returns "" (which lets compile() fall back to the
// version-gated layout) rather than a bogus or partial string. Forced via a
// PROMISE_OPT override pointing at a nonexistent binary, so the exec fails.
func TestDetectWasmDataLayoutProbeFailureFallsBack(t *testing.T) {
	// A unique triple keeps this test's cached "" from poisoning the real
	// wasm32-wasi slot other tests rely on.
	const triple = "wasm32-t1544-probe-failure"
	resetWasmDataLayoutCacheKey(triple)

	t.Setenv("PROMISE_HOME", "")
	t.Setenv("PROMISE_OPT", "/nonexistent/definitely-not-opt")

	if got := detectWasmDataLayout(triple); got != "" {
		t.Errorf("probe with unusable opt = %q, want \"\"", got)
	}
	// The empty result must be cached so a repeat call does not re-exec.
	wasmDataLayoutMu.Lock()
	cached, ok := wasmDataLayoutCached[triple]
	wasmDataLayoutMu.Unlock()
	if !ok || cached != "" {
		t.Errorf("cache[%q] = (%q, %v), want (\"\", true)", triple, cached, ok)
	}
}

// TestWasmDataLayoutRegex verifies the datalayout-extraction regex the probe uses
// pulls the quoted layout out of opt's textual IR output, and reports no match
// when the module has no explicit datalayout line (the parse-failure branch that
// yields "").
func TestWasmDataLayoutRegex(t *testing.T) {
	const want = "e-m:e-p:32:32-i64:64-n32:64-S128"
	withLine := "; ModuleID = '<stdin>'\n" +
		"source_filename = \"<stdin>\"\n" +
		"target datalayout = \"" + want + "\"\n" +
		"target triple = \"wasm32-wasi\"\n"
	if m := wasmDataLayoutRe.FindStringSubmatch(withLine); m == nil {
		t.Errorf("regex failed to match a datalayout line")
	} else if m[1] != want {
		t.Errorf("regex captured %q, want %q", m[1], want)
	}

	noLine := "; ModuleID = '<stdin>'\ntarget triple = \"wasm32-wasi\"\n"
	if m := wasmDataLayoutRe.FindStringSubmatch(noLine); m != nil {
		t.Errorf("regex matched %q in output with no datalayout line", m[0])
	}
}
