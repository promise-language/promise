package codegen

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/wasmtest"
)

// T2128: what links into every wasm module is the prebuilt wasm_alloc.o, not
// wasm_alloc.c. The object is checked in because rebuilding it needs a WASM
// clang, so an edit to the source is inert until someone rebuilds — and T0292
// shipped cabi_vector_data/_len/_from source-only. Under wasm-ld's
// --allow-undefined a missing definition is not a link error: it becomes a host
// import, the build reports success, and the module refuses to instantiate.
//
// So the object is checked against its source here, at commit time — see
// diffWasmAllocObj for what counts as disagreeing.
func TestWasmAllocObjMatchesSource(t *testing.T) {
	cPath := findWasmAllocSource(t)
	objPath := filepath.Join(filepath.Dir(cPath), "wasm_alloc.o")
	defined := wasmAllocCabiDefinitions(t, cPath)

	obj, err := os.ReadFile(objPath)
	if err != nil {
		t.Fatalf("read %s: %v", objPath, err)
	}
	syms, err := wasmtest.Symbols(obj)
	if err != nil {
		t.Fatalf("%s: %v", objPath, err)
	}

	drift := diffWasmAllocObj(defined, syms)
	for _, name := range drift.imported {
		t.Errorf("%s imports function %q — under --allow-undefined every wasm module would carry it as a host import; "+
			"rebuild the object with a clang whose wasm32 defaults lower it (build_wasm_alloc.sh)", objPath, name)
	}
	for _, name := range drift.missing {
		t.Errorf("%s defines %s but %s does not — the object is stale; rebuild it with build_wasm_alloc.sh "+
			"(a missing helper links as an undefined host import and the module will not instantiate)", cPath, name, objPath)
	}
	for _, name := range drift.extra {
		t.Errorf("%s defines %s but %s does not — the object was not built from this source", objPath, name, cPath)
	}
}

// wasmAllocDrift is every way a wasm_alloc.o disagrees with its source, each
// list sorted.
type wasmAllocDrift struct {
	imported []string // functions the object imports
	missing  []string // cabi_* functions the source defines and the object does not
	extra    []string // global cabi_* functions the object defines and the source does not
}

// diffWasmAllocObj compares an object's symbols with the cabi_* functions its
// source defines:
//   - every one the source defines must be a defined, global function in the
//     object (the drift T0292 introduced);
//   - every global cabi_* function in the object must be defined by the source
//     (an object built from something else);
//   - the object must import no function at all. The source declares only
//     extern data (__heap_base, __promise_alloc_count), so an undefined function
//     — a memcpy libcall from a clang without bulk-memory, say — would reach
//     every module as the same silent host import.
func diffWasmAllocObj(defined map[string]bool, syms []wasmtest.Symbol) wasmAllocDrift {
	var drift wasmAllocDrift
	inObj := map[string]bool{}
	for _, s := range syms {
		switch {
		case s.Kind != wasmtest.SymbolFunction:
		case !s.Defined:
			drift.imported = append(drift.imported, s.Name)
		case !s.Local && strings.HasPrefix(s.Name, "cabi_"):
			inObj[s.Name] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(defined)) {
		if !inObj[name] {
			drift.missing = append(drift.missing, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(inObj)) {
		if !defined[name] {
			drift.extra = append(drift.extra, name)
		}
	}
	slices.Sort(drift.imported)
	return drift
}

// The real object agrees with its source, so TestWasmAllocObjMatchesSource never
// reaches a failing branch on its own — this is what shows each one fires, and
// that the symbols which are not helpers stay out of the comparison.
func TestDiffWasmAllocObjReportsEachDisagreement(t *testing.T) {
	defined := map[string]bool{"cabi_both": true, "cabi_source_only": true}
	syms := []wasmtest.Symbol{
		{Kind: wasmtest.SymbolFunction, Name: "memcpy"},
		{Kind: wasmtest.SymbolFunction, Name: "cabi_both", Defined: true},
		{Kind: wasmtest.SymbolFunction, Name: "cabi_object_only", Defined: true},
		// A file-static helper is not part of the linked surface.
		{Kind: wasmtest.SymbolFunction, Name: "cabi_static", Defined: true, Local: true},
		{Kind: wasmtest.SymbolFunction, Name: "malloc", Defined: true},
		// Undefined data is the extern data the source declares, not an import
		// anyone has to satisfy with code.
		{Kind: wasmtest.SymbolData, Name: "__heap_base"},
		{Kind: wasmtest.SymbolData, Name: "__cabi_retarea", Defined: true},
	}

	got := diffWasmAllocObj(defined, syms)
	want := wasmAllocDrift{
		imported: []string{"memcpy"},
		missing:  []string{"cabi_source_only"},
		extra:    []string{"cabi_object_only"},
	}
	if !slices.Equal(got.imported, want.imported) || !slices.Equal(got.missing, want.missing) || !slices.Equal(got.extra, want.extra) {
		t.Errorf("diffWasmAllocObj:\n got %+v\nwant %+v", got, want)
	}
}
