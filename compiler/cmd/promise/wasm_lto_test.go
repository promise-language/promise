package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/wasmweb"
)

// TestWasmCRTObjectsLandUnderPromiseCache: the allocator and math objects every
// wasm32 link pulls in are written from the binary's embedded copies, so they
// are derived caches and PROMISE_CACHE relocates them (#99). Each lands under
// the cache root's crt/wasm32/, and nothing is created under the home's cache.
func TestWasmCRTObjectsLandUnderPromiseCache(t *testing.T) {
	for _, tc := range []struct {
		name    string
		extract func() (string, error)
	}{
		{"wasm_alloc.o", ensureWasmAllocObj},
		{"wasm_math.o", ensureWasmMathObj},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cache := filepath.Join(t.TempDir(), "derived")
			t.Setenv("PROMISE_HOME", home)
			t.Setenv("PROMISE_CACHE", cache)

			got, err := tc.extract()
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if want := filepath.Join(cache, "crt", "wasm32", tc.name); got != want {
				t.Errorf("%s extracted to %q, want %q under PROMISE_CACHE", tc.name, got, want)
			}
			if _, err := os.Stat(filepath.Join(home, "cache")); !os.IsNotExist(err) {
				t.Errorf("<home>/cache was created (stat err %v); the object belongs under PROMISE_CACHE", err)
			}
		})
	}
}

// TestWasmLinkUsesLtoO1 verifies that WASM linking uses --lto-O1, not --lto-O2.
// T0333: --lto-O2 + LLVM 23 miscompiles `icmp samesign ult` in loop exit
// comparisons, causing OOB index reads. Switching to --lto-O1 avoids the buggy
// late LTO pass. Keeping this test ensures we don't accidentally regress to
// --lto-O2 (which restores the original miscompile).
func TestWasmLinkUsesLtoO1(t *testing.T) {
	t.Parallel()
	args, err := buildWasmLinkArgs([]string{"dummy.o"}, "wasm32-wasi", "out.wasm", true /* useLTO */)
	if err != nil {
		t.Fatalf("buildWasmLinkArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--lto-O2") {
		t.Errorf("WASM link uses --lto-O2 which miscompiles icmp samesign (T0333). Args: %v", args)
	}
	if !strings.Contains(joined, "--lto-O1") {
		t.Errorf("WASM link does not use --lto-O1. Args: %v", args)
	}
}

// TestWasmLinkIncludesMathRuntime verifies that WASM linking pulls in the
// embedded math runtime (wasm_math.o). T0333: --lto-O1 doesn't constant-fold
// sin/cos/etc., so unresolved libcall imports would appear without this object.
func TestWasmLinkIncludesMathRuntime(t *testing.T) {
	t.Parallel()
	args, err := buildWasmLinkArgs([]string{"dummy.o"}, "wasm32-wasi", "out.wasm", true)
	if err != nil {
		t.Fatalf("buildWasmLinkArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "wasm_math.o") {
		t.Errorf("WASM link does not include wasm_math.o (T0333). Args: %v", args)
	}
	if !strings.Contains(joined, "wasm_alloc.o") {
		t.Errorf("WASM link does not include wasm_alloc.o. Args: %v", args)
	}
}

// TestWasmLinkNonLTOUsesGcSections verifies the non-LTO path uses --gc-sections
// for DCE on object files. Sanity check on the alternative branch.
func TestWasmLinkNonLTOUsesGcSections(t *testing.T) {
	t.Parallel()
	args, err := buildWasmLinkArgs([]string{"dummy.o"}, "wasm32-wasi", "out.wasm", false /* useLTO */)
	if err != nil {
		t.Fatalf("buildWasmLinkArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--gc-sections") {
		t.Errorf("non-LTO WASM link does not use --gc-sections. Args: %v", args)
	}
	if strings.Contains(joined, "--lto-O1") || strings.Contains(joined, "--lto-O2") {
		t.Errorf("non-LTO WASM link should not include --lto-O*. Args: %v", args)
	}
}

// TestWasmLinkWebTargetExportsInitialize verifies that the wasm32-web target
// exports _initialize and memory (instead of _start) for browser bootstrapping.
func TestWasmLinkWebTargetExportsInitialize(t *testing.T) {
	t.Parallel()
	args, err := buildWasmLinkArgs([]string{"dummy.o"}, "wasm32-web", "out.wasm", true)
	if err != nil {
		t.Fatalf("buildWasmLinkArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--export=_initialize") {
		t.Errorf("wasm32-web link does not export _initialize. Args: %v", args)
	}
	if !strings.Contains(joined, "--export-memory") {
		t.Errorf("wasm32-web link does not export memory. Args: %v", args)
	}
	if strings.Contains(joined, "--export=_start") {
		t.Errorf("wasm32-web link should not export _start (only _initialize). Args: %v", args)
	}
}

// TestWasmLinkWebTargetExportsWebPump verifies that the wasm32-web target
// exports promise_web_pump — the bounded pump every JS callback calls after
// delivering an event (docs/web-apps.md §14) — and that wasm32-wasi
// (unaffected by the reactor design, §18) does not.
func TestWasmLinkWebTargetExportsWebPump(t *testing.T) {
	webArgs, err := buildWasmLinkArgs([]string{"dummy.o"}, "wasm32-web", "out.wasm", true)
	if err != nil {
		t.Fatalf("buildWasmLinkArgs: %v", err)
	}
	if !strings.Contains(strings.Join(webArgs, " "), "--export="+wasmweb.ExportPump) {
		t.Errorf("wasm32-web link does not export %s. Args: %v", wasmweb.ExportPump, webArgs)
	}

	wasiArgs, err := buildWasmLinkArgs([]string{"dummy.o"}, "wasm32-wasi", "out.wasm", true)
	if err != nil {
		t.Fatalf("buildWasmLinkArgs: %v", err)
	}
	if strings.Contains(strings.Join(wasiArgs, " "), wasmweb.ExportPump) {
		t.Errorf("wasm32-wasi link should not reference %s. Args: %v", wasmweb.ExportPump, wasiArgs)
	}
}

// TestWasmLinkWebTargetExportsWebEnqueue verifies that the wasm32-web target
// exports promise_web_enqueue — the entry point a real addEventListener
// callback calls to push one event into a subscription's channel (§8, §14)
// — and that wasm32-wasi does not. wasmweb/names.go's own comment on
// ExportEnqueue warns that declaring the name there is not enough on its
// own; this regression-tests the --export= flag that actually makes it
// reachable from JS (without it, defineWebEnqueueFunc's body is defined but
// wasm-ld strips it as unreferenced, and no event can ever be delivered).
func TestWasmLinkWebTargetExportsWebEnqueue(t *testing.T) {
	webArgs, err := buildWasmLinkArgs([]string{"dummy.o"}, "wasm32-web", "out.wasm", true)
	if err != nil {
		t.Fatalf("buildWasmLinkArgs: %v", err)
	}
	if !strings.Contains(strings.Join(webArgs, " "), "--export="+wasmweb.ExportEnqueue) {
		t.Errorf("wasm32-web link does not export %s. Args: %v", wasmweb.ExportEnqueue, webArgs)
	}

	wasiArgs, err := buildWasmLinkArgs([]string{"dummy.o"}, "wasm32-wasi", "out.wasm", true)
	if err != nil {
		t.Fatalf("buildWasmLinkArgs: %v", err)
	}
	if strings.Contains(strings.Join(wasiArgs, " "), wasmweb.ExportEnqueue) {
		t.Errorf("wasm32-wasi link should not reference %s. Args: %v", wasmweb.ExportEnqueue, wasiArgs)
	}
}
