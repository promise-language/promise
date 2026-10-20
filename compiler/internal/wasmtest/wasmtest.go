// Package wasmtest reads the parts of a WebAssembly binary that tests assert on:
// the import section of a linked module, and the linking symbol table of a
// relocatable object. It decodes nothing else.
//
// Both exist because wasm-ld's --allow-undefined turns a missing definition into
// a host import rather than a link error (T2128), so "did this link" has to be
// answered by reading the artifact, not by the linker's exit status.
package wasmtest

import (
	"bytes"
	"errors"
	"fmt"
)

// ImportKind is the external kind of an import-section entry.
type ImportKind byte

// The import kinds of the WebAssembly binary format.
const (
	ImportFunction ImportKind = 0
	ImportTable    ImportKind = 1
	ImportMemory   ImportKind = 2
	ImportGlobal   ImportKind = 3
	ImportTag      ImportKind = 4
)

// Import is one entry of a module's import section.
type Import struct {
	Module string
	Name   string
	Kind   ImportKind
}

// SymbolKind is the kind of a linking symbol-table entry.
type SymbolKind byte

// The symbol kinds of the WebAssembly object-file linking convention
// (WebAssembly/tool-conventions, Linking.md).
const (
	SymbolFunction SymbolKind = 0
	SymbolData     SymbolKind = 1
	SymbolGlobal   SymbolKind = 2
	SymbolSection  SymbolKind = 3
	SymbolTag      SymbolKind = 4
	SymbolTable    SymbolKind = 5
)

// Symbol is one entry of a relocatable object's symbol table. An undefined
// symbol without an explicit name takes the field name of the import it refers
// to; a section symbol has no name.
type Symbol struct {
	Kind    SymbolKind
	Name    string
	Defined bool
	Local   bool
}

const (
	sectionCustom = 0
	sectionImport = 2

	linkingVersion         = 2
	subsectionSymbolTable  = 8
	symbolFlagBindingLocal = 0x02
	symbolFlagUndefined    = 0x10
	symbolFlagExplicitName = 0x40
)

// Imports returns the import section of a wasm module or object, in order. A
// binary with no import section has none.
func Imports(module []byte) ([]Import, error) {
	var imports []Import
	err := walkSections(module, func(id byte, sec *reader) error {
		if id == sectionImport {
			imports = sec.imports()
		}
		return nil
	})
	return imports, err
}

// Symbols returns the symbol table of a relocatable wasm object — the
// WASM_SYMBOL_TABLE subsection of its "linking" custom section. A binary without
// one is not a relocatable object, and is an error.
func Symbols(object []byte) ([]Symbol, error) {
	importNames := map[ImportKind][]string{}
	var syms []rawSymbol
	sawSymbolTable := false
	err := walkSections(object, func(id byte, sec *reader) error {
		switch id {
		case sectionImport:
			for _, imp := range sec.imports() {
				importNames[imp.Kind] = append(importNames[imp.Kind], imp.Name)
			}
		case sectionCustom:
			if sec.name() != "linking" {
				return nil
			}
			if v := sec.uleb(); sec.err == nil && v != linkingVersion {
				return fmt.Errorf("unsupported linking section version %d", v)
			}
			for sec.err == nil && sec.off < len(sec.b) {
				kind := sec.byte()
				sub := &reader{b: sec.bytes(int(sec.uleb()))}
				if sec.err != nil || kind != subsectionSymbolTable {
					continue
				}
				syms = sub.symbols()
				sawSymbolTable = true
				if sub.err != nil {
					return fmt.Errorf("symbol table: %w", sub.err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !sawSymbolTable {
		return nil, errors.New("no linking symbol table — not a relocatable wasm object")
	}

	out := make([]Symbol, len(syms))
	for i, s := range syms {
		out[i] = s.Symbol
		if s.Name != "" || s.Defined {
			continue
		}
		// An unnamed undefined symbol's index is its position among the imports
		// of its kind, which come first in that kind's index space.
		if kind, ok := s.Kind.importKind(); ok {
			if names := importNames[kind]; s.index < uint64(len(names)) {
				out[i].Name = names[s.index]
			}
		}
	}
	return out, nil
}

// importKind is the import kind an undefined symbol of this kind refers to; data
// and section symbols refer to no import.
func (k SymbolKind) importKind() (ImportKind, bool) {
	switch k {
	case SymbolFunction:
		return ImportFunction, true
	case SymbolGlobal:
		return ImportGlobal, true
	case SymbolTag:
		return ImportTag, true
	case SymbolTable:
		return ImportTable, true
	}
	return 0, false
}

// walkSections checks the module header, then hands each section's payload to
// visit. A section visit leaves malformed is reported as that section's error.
func walkSections(b []byte, visit func(id byte, sec *reader) error) error {
	r := &reader{b: b}
	if magic := r.bytes(4); r.err != nil || !bytes.Equal(magic, []byte("\x00asm")) {
		return errors.New("not a wasm binary (bad magic)")
	}
	if version := r.bytes(4); r.err != nil || !bytes.Equal(version, []byte{1, 0, 0, 0}) {
		return errors.New("unsupported wasm version")
	}
	for r.err == nil && r.off < len(r.b) {
		id := r.byte()
		sec := &reader{b: r.bytes(int(r.uleb()))}
		if r.err != nil {
			break
		}
		if err := visit(id, sec); err != nil {
			return fmt.Errorf("section %d: %w", id, err)
		}
		if sec.err != nil {
			return fmt.Errorf("section %d: %w", id, sec.err)
		}
	}
	return r.err
}

type rawSymbol struct {
	Symbol
	index uint64 // function/global/tag/table index; unused for data and section symbols
}

type reader struct {
	b   []byte
	off int
	err error
}

var errTruncated = errors.New("truncated")

func (r *reader) byte() byte {
	if r.err != nil || r.off >= len(r.b) {
		r.err = errTruncated
		return 0
	}
	c := r.b[r.off]
	r.off++
	return c
}

func (r *reader) bytes(n int) []byte {
	if r.err != nil || n < 0 || r.off+n > len(r.b) {
		r.err = errTruncated
		return nil
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s
}

func (r *reader) uleb() uint64 {
	var v uint64
	for shift := uint(0); shift < 64; shift += 7 {
		c := r.byte()
		if r.err != nil {
			return 0
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v
		}
	}
	r.err = errors.New("LEB128 overflow")
	return 0
}

func (r *reader) name() string { return string(r.bytes(int(r.uleb()))) }

func (r *reader) limits() {
	flags := r.byte()
	r.uleb() // min
	if flags&1 != 0 {
		r.uleb() // max
	}
}

func (r *reader) imports() []Import {
	var imports []Import
	for n := r.uleb(); n > 0 && r.err == nil; n-- {
		imp := Import{Module: r.name(), Name: r.name(), Kind: ImportKind(r.byte())}
		switch imp.Kind {
		case ImportFunction:
			r.uleb() // type index
		case ImportTable:
			r.byte() // reftype
			r.limits()
		case ImportMemory:
			r.limits()
		case ImportGlobal:
			r.byte() // valtype
			r.byte() // mutability
		case ImportTag:
			r.byte() // attribute
			r.uleb() // type index
		default:
			r.err = fmt.Errorf("unknown import kind %d", imp.Kind)
		}
		if r.err == nil {
			imports = append(imports, imp)
		}
	}
	return imports
}

func (r *reader) symbols() []rawSymbol {
	var syms []rawSymbol
	for n := r.uleb(); n > 0 && r.err == nil; n-- {
		kind := SymbolKind(r.byte())
		flags := r.uleb()
		s := rawSymbol{Symbol: Symbol{
			Kind:    kind,
			Defined: flags&symbolFlagUndefined == 0,
			Local:   flags&symbolFlagBindingLocal != 0,
		}}
		switch kind {
		case SymbolFunction, SymbolGlobal, SymbolTag, SymbolTable:
			s.index = r.uleb()
			if s.Defined || flags&symbolFlagExplicitName != 0 {
				s.Name = r.name()
			}
		case SymbolData:
			s.Name = r.name()
			if s.Defined {
				r.uleb() // segment index
				r.uleb() // offset
				r.uleb() // size
			}
		case SymbolSection:
			r.uleb() // section index
		default:
			r.err = fmt.Errorf("unknown symbol kind %d", kind)
		}
		syms = append(syms, s)
	}
	return syms
}
