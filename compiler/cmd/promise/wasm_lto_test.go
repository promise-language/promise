package main

import (
	"strings"
	"testing"
)

// TestWasmLinkUsesLtoO1 verifies that WASM linking uses --lto-O1, not --lto-O2.
// T0333: --lto-O2 + LLVM 23 miscompiles `icmp samesign ult` in loop exit
// comparisons, causing OOB index reads. Switching to --lto-O1 avoids the buggy
// late LTO pass. Keeping this test ensures we don't accidentally regress to
// --lto-O2 (which restores the original miscompile).
func TestWasmLinkUsesLtoO1(t *testing.T) {
	args := buildWasmLinkArgs([]string{"dummy.o"}, "wasm32-wasi", "out.wasm", true /* useLTO */)
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
	args := buildWasmLinkArgs([]string{"dummy.o"}, "wasm32-wasi", "out.wasm", true)
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
	args := buildWasmLinkArgs([]string{"dummy.o"}, "wasm32-wasi", "out.wasm", false /* useLTO */)
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
	args := buildWasmLinkArgs([]string{"dummy.o"}, "wasm32-web", "out.wasm", true)
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
