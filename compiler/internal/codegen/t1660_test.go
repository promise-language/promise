package codegen

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// T1660: rawABIExternSymbols names the raw-C-ABI helpers wasm_alloc.c defines,
// and a symbol missing from it silently reverts to the value-struct bridge ABI —
// which wasm-ld resolves to a trapping stub, or (when the shapes happen to
// coincide on wasm32) links wrong without a word. The registry's own comment
// says "adding a helper to wasm_alloc.c means adding its symbol here"; this test
// is what makes that true rather than merely asked for.
//
// The drift this guards against is not hypothetical: T0292 added
// cabi_vector_data/_len/_from to wasm_alloc.c and the prebuilt wasm_alloc.o was
// never rebuilt to match (T2128).
func TestRawABIExternSymbolsCoverWasmAllocC(t *testing.T) {
	path := findWasmAllocSource(t)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// Definitions only — a leading type, the name, an argument list, and an
	// opening brace on the same line, which is how every helper is written.
	defRe := regexp.MustCompile(`(?m)^[A-Za-z_][A-Za-z0-9_ *]*\b(cabi_[a-z0-9_]+)\s*\([^)]*\)\s*\{`)
	found := map[string]bool{}
	for _, m := range defRe.FindAllStringSubmatch(string(src), -1) {
		found[m[1]] = true
	}
	if len(found) == 0 {
		t.Fatalf("no cabi_* definitions found in %s — the pattern needs updating, not the registry", path)
	}

	for sym := range found {
		if !rawABIExternSymbols[sym] {
			t.Errorf("%s defines %s but rawABIExternSymbols does not list it — an `extern naming it "+
				"would lower to the value-struct bridge ABI and not match the C definition", path, sym)
		}
	}
	for sym := range rawABIExternSymbols {
		if !found[sym] {
			t.Errorf("rawABIExternSymbols lists %s but %s no longer defines it", sym, path)
		}
	}
}

// findWasmAllocSource walks up to the repo root to locate the canonical-ABI C
// source. Skips rather than fails if it is absent, so the test is inert in a
// stripped checkout.
func findWasmAllocSource(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		p := filepath.Join(dir, "compiler", "cmd", "promise", "crt", "wasm32", "wasm_alloc.c")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("wasm_alloc.c not found")
	return ""
}
