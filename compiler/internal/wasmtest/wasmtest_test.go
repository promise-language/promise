package wasmtest

import (
	"slices"
	"testing"
)

func leb(n int) []byte {
	var out []byte
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(out, c)
		}
		out = append(out, c|0x80)
	}
}

func str(s string) []byte { return append(leb(len(s)), s...) }

func cat(parts ...[]byte) []byte { return slices.Concat(parts...) }

func section(id byte, payload ...[]byte) []byte {
	p := cat(payload...)
	return cat([]byte{id}, leb(len(p)), p)
}

func binary(sections ...[]byte) []byte {
	return cat([]byte("\x00asm"), []byte{1, 0, 0, 0}, cat(sections...))
}

// The shapes a clang-built object actually carries: memory, table and global
// imports interleaved with the function imports, so a function symbol's index
// counts function imports only.
func objectImports() []byte {
	return section(sectionImport, leb(5),
		str("env"), str("__linear_memory"), []byte{byte(ImportMemory), 0}, leb(1),
		str("env"), str("memcpy"), []byte{byte(ImportFunction)}, leb(0),
		str("env"), str("__stack_pointer"), []byte{byte(ImportGlobal), 0x7f, 1},
		str("env"), str("__indirect_function_table"), []byte{byte(ImportTable), 0x70, 0}, leb(0),
		str("env"), str("host_fn"), []byte{byte(ImportFunction)}, leb(0),
	)
}

func symbolTable(count int) []byte {
	entries := cat(
		[]byte{byte(SymbolFunction)}, leb(symbolFlagUndefined), leb(0),
		[]byte{byte(SymbolFunction)}, leb(symbolFlagUndefined|symbolFlagExplicitName), leb(1), str("renamed"),
		[]byte{byte(SymbolGlobal)}, leb(symbolFlagUndefined), leb(0),
		[]byte{byte(SymbolFunction)}, leb(0), leb(2), str("cabi_global"),
		[]byte{byte(SymbolFunction)}, leb(symbolFlagBindingLocal), leb(3), str("cabi_local"),
		[]byte{byte(SymbolData)}, leb(symbolFlagUndefined), str("__heap_base"),
		[]byte{byte(SymbolData)}, leb(0), str("__cabi_retarea"), leb(0), leb(0), leb(128),
		[]byte{byte(SymbolSection)}, leb(symbolFlagBindingLocal), leb(5),
	)
	segmentInfo := cat(leb(1), str(".data"), leb(2), leb(0))
	return section(sectionCustom, str("linking"), leb(linkingVersion),
		[]byte{5}, leb(len(segmentInfo)), segmentInfo,
		[]byte{subsectionSymbolTable}, leb(len(entries)+len(leb(count))), leb(count), entries,
	)
}

func TestSymbolsNamesUndefinedSymbolsFromTheirImports(t *testing.T) {
	obj := binary(
		section(1, leb(0)),
		objectImports(),
		section(sectionCustom, str("producers"), leb(0)),
		symbolTable(8),
	)
	got, err := Symbols(obj)
	if err != nil {
		t.Fatal(err)
	}
	want := []Symbol{
		{Kind: SymbolFunction, Name: "memcpy"},
		{Kind: SymbolFunction, Name: "renamed"},
		{Kind: SymbolGlobal, Name: "__stack_pointer"},
		{Kind: SymbolFunction, Name: "cabi_global", Defined: true},
		{Kind: SymbolFunction, Name: "cabi_local", Defined: true, Local: true},
		{Kind: SymbolData, Name: "__heap_base"},
		{Kind: SymbolData, Name: "__cabi_retarea", Defined: true},
		{Kind: SymbolSection, Defined: true, Local: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Symbols:\n got %+v\nwant %+v", got, want)
	}
}

// A table that claims more entries than it holds is an error, never the prefix
// that decoded — a dropped trailing import would read as "imports nothing".
func TestSymbolsRejectsATruncatedSymbolTable(t *testing.T) {
	if syms, err := Symbols(binary(objectImports(), symbolTable(9))); err == nil {
		t.Errorf("Symbols accepted a symbol table one entry short, returning %+v", syms)
	}
}

func TestImportsDecodesEveryKind(t *testing.T) {
	module := binary(
		section(1, leb(0)),
		section(sectionImport, leb(5),
			str("wasi_snapshot_preview1"), str("fd_write"), []byte{byte(ImportFunction)}, leb(3),
			str("env"), str("table"), []byte{byte(ImportTable), 0x70, 1}, leb(1), leb(4),
			str("env"), str("memory"), []byte{byte(ImportMemory), 1}, leb(1), leb(2),
			str("env"), str("g"), []byte{byte(ImportGlobal), 0x7e, 0},
			str("env"), str("exn"), []byte{byte(ImportTag), 0}, leb(0),
		),
		section(sectionCustom, str("name"), leb(0)),
	)
	got, err := Imports(module)
	if err != nil {
		t.Fatal(err)
	}
	want := []Import{
		{Module: "wasi_snapshot_preview1", Name: "fd_write", Kind: ImportFunction},
		{Module: "env", Name: "table", Kind: ImportTable},
		{Module: "env", Name: "memory", Kind: ImportMemory},
		{Module: "env", Name: "g", Kind: ImportGlobal},
		{Module: "env", Name: "exn", Kind: ImportTag},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Imports:\n got %+v\nwant %+v", got, want)
	}
}

func TestImportsRejectsATruncatedImportSection(t *testing.T) {
	module := binary(section(sectionImport, leb(2),
		str("env"), str("cabi_vector_data"), []byte{byte(ImportFunction)}, leb(0),
	))
	if imports, err := Imports(module); err == nil {
		t.Errorf("Imports accepted an import section one entry short, returning %+v", imports)
	}
}
