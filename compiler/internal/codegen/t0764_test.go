package codegen

import (
	"strings"
	"testing"
)

// T0764: the wasm32 DataLayout must match the wasm-ld LTO linker's LLVM major
// version. LLVM 23 added reference-type address spaces (p10/p20), i128:128, and
// a non-integral pointer spec (ni:1:10:20); its LTO backend aborts with
// "Target-incompatible DataLayout" if the module lacks them. LLVM 22 and earlier
// use (and require) the narrower layout. The two cannot share one string.
func TestWasmDataLayoutVersionGate(t *testing.T) {
	const llvm22 = "e-m:e-p:32:32-i64:64-n32:64-S128"
	const llvm23 = "e-m:e-p:32:32-p10:8:8-p20:8:8-i64:64-i128:128-n32:64-S128-ni:1:10:20"

	cases := []struct {
		major int
		want  string
	}{
		{0, llvm22},  // unknown → long-standing pre-23 layout
		{21, llvm22}, // below floor still maps to the narrow layout
		{22, llvm22},
		{23, llvm23},
		{24, llvm23}, // future versions keep the reference-type layout
	}
	for _, c := range cases {
		if got := wasmDataLayout(c.major); got != c.want {
			t.Errorf("wasmDataLayout(%d) = %q, want %q", c.major, got, c.want)
		}
	}
}

// TestWasmDataLayoutResolverWiring verifies compile() routes the wasm32
// DataLayout through the WasmLinkerMajorVersion hook so the CLI-detected linker
// version drives the emitted layout.
func TestWasmDataLayoutResolverWiring(t *testing.T) {
	saved := WasmLinkerMajorVersion
	defer func() { WasmLinkerMajorVersion = saved }()

	src := `main() { }`

	WasmLinkerMajorVersion = func() int { return 22 }
	ir22 := generateIRForTarget(t, src, "wasm32-wasi")
	if !strings.Contains(ir22, `target datalayout = "e-m:e-p:32:32-i64:64-n32:64-S128"`) {
		t.Errorf("LLVM 22 wasm IR missing narrow datalayout:\n%s", firstLines(ir22, 3))
	}

	WasmLinkerMajorVersion = func() int { return 23 }
	ir23 := generateIRForTarget(t, src, "wasm32-wasi")
	if !strings.Contains(ir23, `target datalayout = "e-m:e-p:32:32-p10:8:8-p20:8:8-i64:64-i128:128-n32:64-S128-ni:1:10:20"`) {
		t.Errorf("LLVM 23 wasm IR missing reference-type datalayout:\n%s", firstLines(ir23, 3))
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
