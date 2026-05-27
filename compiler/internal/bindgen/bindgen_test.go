package bindgen

import (
	"fmt"
	"strings"
	"testing"

	"djabi.dev/go/promise_lang/internal/webidl"
	"djabi.dev/go/promise_lang/internal/wit"
)

// --- Name conversion tests ---

func TestKebabToSnake(t *testing.T) {
	tests := []struct{ in, want string }{
		{"path-open", "path_open"},
		{"fd-read", "fd_read"},
		{"simple", "simple"},
		{"a-b-c", "a_b_c"},
	}
	for _, tt := range tests {
		got := kebabToSnake(tt.in)
		if got != tt.want {
			t.Errorf("kebabToSnake(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestKebabToPascal(t *testing.T) {
	tests := []struct{ in, want string }{
		{"descriptor-type", "DescriptorType"},
		{"open-flags", "OpenFlags"},
		{"simple", "Simple"},
		{"a-b-c", "ABC"},
	}
	for _, tt := range tests {
		got := kebabToPascal(tt.in)
		if got != tt.want {
			t.Errorf("kebabToPascal(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestToSnake(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Descriptor", "descriptor"},
		{"DescriptorStat", "descriptor_stat"},
		{"ABC", "abc"},
		{"DOMException", "dom_exception"},
		{"getURL", "get_url"},
		{"URLParser", "url_parser"},
	}
	for _, tt := range tests {
		got := toSnake(tt.in)
		if got != tt.want {
			t.Errorf("toSnake(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- WIT-to-IR conversion tests ---

func TestWitToIRRecord(t *testing.T) {
	src := `
interface types {
    record descriptor-stat {
        type: u32,
        size: u64,
    }
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	if len(modules) != 1 {
		t.Fatalf("expected 1 module, got %d", len(modules))
	}
	m := modules[0]
	if m.Name != "types" {
		t.Errorf("module name: got %q", m.Name)
	}
	if len(m.Types) != 1 {
		t.Fatalf("expected 1 type, got %d", len(m.Types))
	}
	ty := m.Types[0]
	if ty.Name != "DescriptorStat" {
		t.Errorf("type name: got %q, want %q", ty.Name, "DescriptorStat")
	}
	if ty.Kind != TypeRecord {
		t.Errorf("type kind: got %v, want Record", ty.Kind)
	}
	if len(ty.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(ty.Fields))
	}
	// WIT "type" is a keyword but also a valid field name
	if ty.Fields[0].Name != "type" {
		t.Errorf("field 0 name: got %q", ty.Fields[0].Name)
	}
	if ty.Fields[1].Name != "size" {
		t.Errorf("field 1 name: got %q", ty.Fields[1].Name)
	}
}

func TestWitToIREnum(t *testing.T) {
	src := `
interface types {
    enum color {
        red,
        green,
        blue,
    }
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	ty := modules[0].Types[0]
	if ty.Name != "Color" {
		t.Errorf("type name: got %q", ty.Name)
	}
	if ty.Kind != TypeEnum {
		t.Errorf("type kind: got %v, want Enum", ty.Kind)
	}
	if len(ty.Cases) != 3 {
		t.Fatalf("expected 3 cases, got %d", len(ty.Cases))
	}
	if ty.Cases[0].Name != "Red" {
		t.Errorf("case 0: got %q, want %q", ty.Cases[0].Name, "Red")
	}
}

func TestWitToIRVariant(t *testing.T) {
	src := `
interface types {
    variant shape {
        circle(f64),
        rectangle(u32),
        none,
    }
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	ty := modules[0].Types[0]
	if ty.Name != "Shape" {
		t.Errorf("type name: got %q", ty.Name)
	}
	if ty.Kind != TypeVariant {
		t.Errorf("type kind: got %v, want Variant", ty.Kind)
	}
	if len(ty.Cases) != 3 {
		t.Fatalf("expected 3 cases, got %d", len(ty.Cases))
	}
	if ty.Cases[0].Name != "Circle" || ty.Cases[0].Type == nil {
		t.Errorf("case 0 unexpected")
	}
	if ty.Cases[2].Name != "None" || ty.Cases[2].Type != nil {
		t.Errorf("case 2 unexpected")
	}
}

func TestWitToIRFlags(t *testing.T) {
	src := `
interface types {
    flags open-flags {
        read,
        write,
        append,
    }
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	ty := modules[0].Types[0]
	if ty.Name != "OpenFlags" {
		t.Errorf("type name: got %q", ty.Name)
	}
	if ty.Kind != TypeFlags {
		t.Errorf("type kind: got %v, want Flags", ty.Kind)
	}
	if len(ty.Fields) != 3 {
		t.Fatalf("expected 3 flags, got %d", len(ty.Fields))
	}
}

func TestWitToIRResource(t *testing.T) {
	src := `
interface fs {
    resource descriptor {
        read: func(len: u64) -> result<list<u8>, u32>;
    }
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	if len(modules[0].Resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(modules[0].Resources))
	}
	r := modules[0].Resources[0]
	if r.Name != "Descriptor" {
		t.Errorf("resource name: got %q", r.Name)
	}
	if !r.Drop {
		t.Error("expected Drop=true")
	}
	if len(r.Methods) != 1 {
		t.Fatalf("expected 1 method, got %d", len(r.Methods))
	}
	if r.Methods[0].Name != "read" {
		t.Errorf("method name: got %q", r.Methods[0].Name)
	}
	if r.Methods[0].Kind != FuncMethod {
		t.Errorf("method kind: got %v, want Method", r.Methods[0].Kind)
	}
}

func TestWitToIRFreeFunc(t *testing.T) {
	src := `
interface clocks {
    now: func() -> u64;
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	if len(modules[0].Functions) != 1 {
		t.Fatalf("expected 1 function, got %d", len(modules[0].Functions))
	}
	fn := modules[0].Functions[0]
	if fn.Name != "now" {
		t.Errorf("func name: got %q", fn.Name)
	}
	if fn.Kind != FuncFree {
		t.Errorf("func kind: got %v, want Free", fn.Kind)
	}
	if fn.ImportName != "now" {
		t.Errorf("import name: got %q", fn.ImportName)
	}
}

func TestWitToIRPackageImportModule(t *testing.T) {
	src := `
package wasi:filesystem@0.2.0;

interface types {
    type error-code = u32;
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	if modules[0].ImportModule != "wasi:filesystem/types" {
		t.Errorf("import module: got %q, want %q", modules[0].ImportModule, "wasi:filesystem/types")
	}
}

// --- Code generation tests ---

func TestCodegenRecord(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/types",
		Types: []Type{{
			Name: "Point",
			Kind: TypeRecord,
			Fields: []Field{
				{Name: "x", Type: TypeRef{Kind: BuiltinKind, Builtin: "f64"}},
				{Name: "y", Type: TypeRef{Kind: BuiltinKind, Builtin: "f64"}},
			},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "type Point `public `value {")
	assertContains(t, out, "f64 x `value;")
	assertContains(t, out, "f64 y `value;")
}

func TestCodegenEnum(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/types",
		Types: []Type{{
			Name: "Color",
			Kind: TypeEnum,
			Cases: []Case{
				{Name: "Red"},
				{Name: "Green"},
				{Name: "Blue"},
			},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "enum Color `public {")
	assertContains(t, out, "Red,")
	assertContains(t, out, "Green,")
	assertContains(t, out, "Blue,")
}

func TestCodegenVariant(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/types",
		Types: []Type{{
			Name: "Shape",
			Kind: TypeVariant,
			Cases: []Case{
				{Name: "Circle", Type: &TypeRef{Kind: BuiltinKind, Builtin: "f64"}},
				{Name: "None"},
			},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "enum Shape `public {")
	assertContains(t, out, "Circle(f64 value),")
	assertContains(t, out, "None,")
}

func TestCodegenFlags(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/types",
		Types: []Type{{
			Name: "OpenFlags",
			Kind: TypeFlags,
			Fields: []Field{
				{Name: "read"},
				{Name: "write"},
				{Name: "append"},
			},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "type OpenFlags `public `value {")
	assertContains(t, out, "int _bits `value;")
	assertContains(t, out, "get read OpenFlags `public `global {")
	assertContains(t, out, "return OpenFlags(_bits: 1);")
	assertContains(t, out, "return OpenFlags(_bits: 2);")
	assertContains(t, out, "return OpenFlags(_bits: 4);")
	assertContains(t, out, "has(this, OpenFlags flag) bool `public {")
}

func TestCodegenResource(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "Descriptor",
			Drop: true,
			Methods: []Func{{
				Name:       "read",
				Kind:       FuncMethod,
				Params:     []Param{{Name: "length", Type: TypeRef{Kind: BuiltinKind, Builtin: "u64"}}},
				Results:    []TypeRef{{Kind: ResultKind, Ok: &TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u8"}}}},
				ImportName: "[method]descriptor.read",
			}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "type Descriptor `public `target(wasi) {")
	assertContains(t, out, "i32 _handle;")
	assertContains(t, out, "read!(this, u64 length) u8[] `public {")
	assertContains(t, out, "return _descriptor_read(this._handle, length)^;")
	assertContains(t, out, "drop(~this) {")
	assertContains(t, out, "_descriptor_drop(this._handle);")
	assertContains(t, out, "`wasm_import(\"wasi:fs/types\", \"[method]descriptor.read\")")
	assertContains(t, out, "`wasm_import(\"wasi:fs/types\", \"[resource-drop]descriptor\")")
	assertContains(t, out, "`target(wasi);")
}

func TestCodegenResourceReturn(t *testing.T) {
	// Non-canonical: method returning a resource type must construct from i32 handle.
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Document",
			Drop: true,
			Methods: []Func{{
				Name:       "create_element",
				Kind:       FuncMethod,
				Params:     []Param{{Name: "tag", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
				Results:    []TypeRef{{Kind: NamedKind, Name: "Element"}},
				ImportName: "Document.createElement",
			}},
		}},
	}}
	out := GeneratePromise(modules, "web")
	// Wrapper constructs Element from handle
	assertContains(t, out, "handle := _document_create_element(this._handle, tag);\n")
	assertContains(t, out, "return Element(_handle: handle);")
	// Extern returns i32, not Element
	assertContains(t, out, "_document_create_element(i32 handle, string tag) i32")
}

func TestCodegenOptionalResourceReturn(t *testing.T) {
	// Non-canonical: method returning optional resource checks handle == 0.
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Document",
			Drop: true,
			Methods: []Func{{
				Name:       "get_element_by_id",
				Kind:       FuncMethod,
				Params:     []Param{{Name: "id", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
				Results:    []TypeRef{{Kind: OptionKind, Elem: &TypeRef{Kind: NamedKind, Name: "Element"}}},
				ImportName: "Document.getElementById",
			}},
		}},
	}}
	out := GeneratePromise(modules, "web")
	// Wrapper checks handle and constructs
	assertContains(t, out, "if handle == 0 { return none; }")
	assertContains(t, out, "return Element(_handle: handle);")
	// Extern returns i32
	assertContains(t, out, "_document_get_element_by_id(i32 handle, string id) i32")
}

func TestCodegenResourceParam(t *testing.T) {
	// Non-canonical: method taking a resource param passes ._handle to extern.
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Node",
			Drop: true,
			Methods: []Func{{
				Name:       "append_child",
				Kind:       FuncMethod,
				Params:     []Param{{Name: "child", Type: TypeRef{Kind: NamedKind, Name: "Node"}}},
				ImportName: "Node.appendChild",
			}},
		}},
	}}
	out := GeneratePromise(modules, "web")
	// Wrapper passes child._handle
	assertContains(t, out, "_node_append_child(this._handle, child._handle);")
	// Extern takes i32 for the resource param
	assertContains(t, out, "_node_append_child(i32 handle, i32 child)")
}

func TestPromiseExternType(t *testing.T) {
	tests := []struct {
		ref  TypeRef
		want string
	}{
		{TypeRef{Kind: BuiltinKind, Builtin: "u32"}, "u32"},
		{TypeRef{Kind: BuiltinKind, Builtin: "string"}, "string"},
		{TypeRef{Kind: NamedKind, Name: "Element"}, "i32"},
		{TypeRef{Kind: OptionKind, Elem: &TypeRef{Kind: NamedKind, Name: "Element"}}, "i32"},
		{TypeRef{Kind: OptionKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "string"}}, "string?"},
	}
	for _, tt := range tests {
		got := promiseExternType(tt.ref)
		if got != tt.want {
			t.Errorf("promiseExternType(%v) = %q, want %q", tt.ref.Kind, got, tt.want)
		}
	}
}

func TestCodegenFreeFunc(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:clocks/monotonic",
		Functions: []Func{{
			Name:       "now",
			Kind:       FuncFree,
			Results:    []TypeRef{{Kind: BuiltinKind, Builtin: "u64"}},
			ImportName: "now",
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "now() u64 `public `target(wasi) {")
	assertContains(t, out, "return _now();")
	assertContains(t, out, "`extern(\"now\")")
	assertContains(t, out, "`wasm_import(\"wasi:clocks/monotonic\", \"now\")")
}

func TestCodegenFailableResult(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "do_thing",
			Kind:       FuncFree,
			Results:    []TypeRef{{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}}},
			ImportName: "do-thing",
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "do_thing!() u32 `public `target(wasi) {")
	assertContains(t, out, "return _do_thing()^;")
}

func TestCodegenDoc(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "greet",
			Kind:       FuncFree,
			Params:     []Param{{Name: "name", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
			ImportName: "greet",
			Doc:        "Greet someone by name.",
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "`doc \"Greet someone by name.\"")
}

// --- Coverage gap tests ---

func TestCodegenConstructorWrapper(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "Descriptor",
			Drop: true,
			Methods: []Func{{
				Name:       "constructor",
				Kind:       FuncConstructor,
				Params:     []Param{{Name: "path", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
				Results:    []TypeRef{},
				ImportName: "[constructor]descriptor",
			}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "new(~this, string path) `public {")
	assertContains(t, out, "this._handle = _descriptor_constructor(path);")
	assertContains(t, out, "`wasm_import(\"wasi:fs/types\", \"[constructor]descriptor\")")
}

// TestCodegenConstructorWrapperNoParams covers the no-argument constructor path
// where the `thisParam` builder stays as bare `~this` (params == "").
// Without this, the empty-params branch is not exercised by the other constructor tests.
func TestCodegenConstructorWrapperNoParams(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "Descriptor",
			Drop: true,
			Methods: []Func{{
				Name:       "constructor",
				Kind:       FuncConstructor,
				ImportName: "[constructor]descriptor",
			}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "new(~this) `public {")
	assertContains(t, out, "this._handle = _descriptor_constructor();")
}

func TestCodegenConstructorWrapperFailable(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "Descriptor",
			Drop: true,
			Methods: []Func{{
				Name:       "constructor",
				Kind:       FuncConstructor,
				Params:     []Param{{Name: "path", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
				Results:    []TypeRef{{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}}},
				ImportName: "[constructor]descriptor",
			}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "new!(~this, string path) `public {")
	assertContains(t, out, "this._handle = _descriptor_constructor(path)^;")
}

func TestCodegenStaticWrapper(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "Descriptor",
			Drop: true,
			Methods: []Func{{
				Name:       "open",
				Kind:       FuncStatic,
				Params:     []Param{{Name: "path", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
				Results:    []TypeRef{{Kind: BuiltinKind, Builtin: "u32"}},
				ImportName: "[static]descriptor.open",
			}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "open(string path) u32 `public `global {")
	assertContains(t, out, "return _descriptor_open(path);")
	assertContains(t, out, "`wasm_import(\"wasi:fs/types\", \"[static]descriptor.open\")")
}

func TestCodegenStaticWrapperFailable(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "Descriptor",
			Drop: true,
			Methods: []Func{{
				Name:       "open",
				Kind:       FuncStatic,
				Params:     []Param{{Name: "path", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
				Results:    []TypeRef{{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}}},
				ImportName: "[static]descriptor.open",
			}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "open!(string path) u32 `public `global {")
	assertContains(t, out, "return _descriptor_open(path)^;")
}

func TestCodegenStaticWrapperVoid(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "Descriptor",
			Drop: true,
			Methods: []Func{{
				Name:       "reset",
				Kind:       FuncStatic,
				Results:    []TypeRef{},
				ImportName: "[static]descriptor.reset",
			}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "reset() `public `global {")
	assertContains(t, out, "_descriptor_reset();")
}

func TestPromiseTypeAllKinds(t *testing.T) {
	tests := []struct {
		ref  TypeRef
		want string
	}{
		{TypeRef{Kind: BuiltinKind, Builtin: "u32"}, "u32"},
		{TypeRef{Kind: NamedKind, Name: "MyType"}, "MyType"},
		{TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u8"}}, "u8[]"},
		{TypeRef{Kind: OptionKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "string"}}, "string?"},
		{TypeRef{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}}, "u32"},
		{TypeRef{Kind: ResultKind}, ""},
		{TypeRef{Kind: TupleKind, Elements: []TypeRef{
			{Kind: BuiltinKind, Builtin: "u32"},
			{Kind: BuiltinKind, Builtin: "string"},
		}}, "(u32, string)"},
		{TypeRef{Kind: OwnKind, Elem: &TypeRef{Kind: NamedKind, Name: "Res"}}, "~Res"},
		{TypeRef{Kind: BorrowKind, Elem: &TypeRef{Kind: NamedKind, Name: "Res"}}, "&Res"},
	}
	for _, tt := range tests {
		got := promiseType(tt.ref)
		if got != tt.want {
			t.Errorf("promiseType(%v) = %q, want %q", tt.ref.Kind, got, tt.want)
		}
	}
}

func TestWitBuiltinToPromiseAll(t *testing.T) {
	tests := []struct{ in, want string }{
		{"u8", "u8"}, {"u16", "u16"}, {"u32", "u32"}, {"u64", "u64"},
		{"s8", "i8"}, {"s16", "i16"}, {"s32", "i32"}, {"s64", "i64"},
		{"f32", "f32"}, {"f64", "f64"},
		{"bool", "bool"}, {"char", "char"}, {"string", "string"},
		{"unknown", "int"},
	}
	for _, tt := range tests {
		got := witBuiltinToPromise(tt.in)
		if got != tt.want {
			t.Errorf("witBuiltinToPromise(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatReturnTypeMultipleResults(t *testing.T) {
	g := &generator{}
	// Empty results
	if got := g.formatReturnType(nil); got != "" {
		t.Errorf("empty results: got %q", got)
	}
	// Multiple results → tuple
	results := []TypeRef{
		{Kind: BuiltinKind, Builtin: "u32"},
		{Kind: BuiltinKind, Builtin: "string"},
	}
	got := g.formatReturnType(results)
	if got != "(u32, string)" {
		t.Errorf("multiple results: got %q, want %q", got, "(u32, string)")
	}
	// Single non-result
	single := []TypeRef{{Kind: BuiltinKind, Builtin: "u64"}}
	got2 := g.formatReturnType(single)
	if got2 != "u64" {
		t.Errorf("single result: got %q, want %q", got2, "u64")
	}
	// Result with no Ok
	resultVoid := []TypeRef{{Kind: ResultKind}}
	got3 := g.formatReturnType(resultVoid)
	if got3 != "" {
		t.Errorf("result void: got %q, want empty", got3)
	}
}

func TestWitToIRResourceWithConstructorAndStatic(t *testing.T) {
	src := `
interface fs {
    resource descriptor {
        constructor(path: string);
        static open: func(path: string) -> result<descriptor, u32>;
        read: func(len: u64) -> list<u8>;
    }
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	r := modules[0].Resources[0]
	if len(r.Methods) != 3 {
		t.Fatalf("expected 3 methods, got %d", len(r.Methods))
	}
	// constructor
	if r.Methods[0].Kind != FuncConstructor {
		t.Errorf("method 0: got kind %v, want Constructor", r.Methods[0].Kind)
	}
	if r.Methods[0].ImportName != "[constructor]descriptor" {
		t.Errorf("constructor import name: got %q", r.Methods[0].ImportName)
	}
	// static
	if r.Methods[1].Kind != FuncStatic {
		t.Errorf("method 1: got kind %v, want Static", r.Methods[1].Kind)
	}
	if r.Methods[1].ImportName != "[static]descriptor.open" {
		t.Errorf("static import name: got %q", r.Methods[1].ImportName)
	}
	// method
	if r.Methods[2].Kind != FuncMethod {
		t.Errorf("method 2: got kind %v, want Method", r.Methods[2].Kind)
	}
}

func TestWitToIRConvertTypeRefAllKinds(t *testing.T) {
	src := `
interface types {
    f1: func(a: list<u8>) -> option<string>;
    f2: func() -> result<u32, string>;
    f3: func() -> tuple<u32, string, bool>;
    f4: func(a: own<my-res>, b: borrow<my-res>);
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	fns := modules[0].Functions

	// list param
	if fns[0].Params[0].Type.Kind != ListKind {
		t.Errorf("f1 param: got kind %v, want List", fns[0].Params[0].Type.Kind)
	}
	// option result
	if fns[0].Results[0].Kind != OptionKind {
		t.Errorf("f1 result: got kind %v, want Option", fns[0].Results[0].Kind)
	}
	// result with ok and err
	if fns[1].Results[0].Kind != ResultKind {
		t.Errorf("f2 result: got kind %v, want Result", fns[1].Results[0].Kind)
	}
	if fns[1].Results[0].Ok == nil || fns[1].Results[0].Ok.Builtin != "u32" {
		t.Error("f2 result: expected Ok=u32")
	}
	if fns[1].Results[0].Err == nil || fns[1].Results[0].Err.Builtin != "string" {
		t.Error("f2 result: expected Err=string")
	}
	// tuple
	if fns[2].Results[0].Kind != TupleKind {
		t.Errorf("f3 result: got kind %v, want Tuple", fns[2].Results[0].Kind)
	}
	if len(fns[2].Results[0].Elements) != 3 {
		t.Errorf("f3 tuple: expected 3 elements, got %d", len(fns[2].Results[0].Elements))
	}
	// own and borrow
	if fns[3].Params[0].Type.Kind != OwnKind {
		t.Errorf("f4 param 0: got kind %v, want Own", fns[3].Params[0].Type.Kind)
	}
	if fns[3].Params[1].Type.Kind != BorrowKind {
		t.Errorf("f4 param 1: got kind %v, want Borrow", fns[3].Params[1].Type.Kind)
	}
}

func TestWitToIRConvertNamedResults(t *testing.T) {
	src := `
interface api {
    get-pair: func() -> (first: u32, second: string);
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	fn := modules[0].Functions[0]
	if len(fn.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(fn.Results))
	}
	if fn.Results[0].Builtin != "u32" {
		t.Errorf("result 0: got %q, want u32", fn.Results[0].Builtin)
	}
	if fn.Results[1].Builtin != "string" {
		t.Errorf("result 1: got %q, want string", fn.Results[1].Builtin)
	}
}

func TestCodegenRecordDoc(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/types",
		Types: []Type{{
			Name: "Point",
			Kind: TypeRecord,
			Doc:  "A 2D point.",
			Fields: []Field{
				{Name: "x", Type: TypeRef{Kind: BuiltinKind, Builtin: "f64"}},
			},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "`doc \"A 2D point.\"")
	assertContains(t, out, "type Point `public `value {")
}

func TestCodegenEnumDoc(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/types",
		Types: []Type{{
			Name: "Color",
			Kind: TypeEnum,
			Doc:  "Basic colors.",
			Cases: []Case{
				{Name: "Red"},
			},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "`doc \"Basic colors.\"")
	assertContains(t, out, "enum Color `public {")
}

func TestCodegenVariantDoc(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/types",
		Types: []Type{{
			Name: "Shape",
			Kind: TypeVariant,
			Doc:  "A shape variant.",
			Cases: []Case{
				{Name: "Circle", Type: &TypeRef{Kind: BuiltinKind, Builtin: "f64"}},
			},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "`doc \"A shape variant.\"")
	assertContains(t, out, "enum Shape `public {")
}

func TestCodegenMethodWrapperVoid(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "Descriptor",
			Drop: true,
			Methods: []Func{{
				Name:       "sync",
				Kind:       FuncMethod,
				Results:    []TypeRef{},
				ImportName: "[method]descriptor.sync",
			}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "sync(this) `public {")
	assertContains(t, out, "_descriptor_sync(this._handle);")
}

func TestCodegenMethodWrapperVoidFailable(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "Descriptor",
			Drop: true,
			Methods: []Func{{
				Name:       "sync",
				Kind:       FuncMethod,
				Results:    []TypeRef{{Kind: ResultKind}},
				ImportName: "[method]descriptor.sync",
			}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "sync!(this) `public {")
}

func TestCodegenTypeAlias(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/types",
		Types: []Type{{
			Name:   "ErrorCode",
			Kind:   TypeAlias,
			Target: &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	// Aliases are typically inlined, but emitType should handle the alias kind
	_ = out
}

func TestCodegenResourceMethodDoc(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "File",
			Drop: true,
			Methods: []Func{{
				Name:       "read",
				Kind:       FuncMethod,
				Params:     []Param{{Name: "len", Type: TypeRef{Kind: BuiltinKind, Builtin: "u64"}}},
				Results:    []TypeRef{{Kind: BuiltinKind, Builtin: "u32"}},
				ImportName: "[method]file.read",
				Doc:        "Read bytes from the file.",
			}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "`doc \"Read bytes from the file.\"")
}

// --- Round-trip test ---

func TestRoundTripFullWit(t *testing.T) {
	src := `
package wasi:filesystem@0.2.0;

interface types {
    enum descriptor-type {
        unknown,
        directory,
        regular-file,
    }

    record descriptor-stat {
        type: descriptor-type,
        size: u64,
    }

    flags open-flags {
        create,
        exclusive,
        truncate,
    }

    resource descriptor {
        stat: func() -> result<descriptor-stat, u32>;
        read: func(length: u64) -> result<list<u8>, u32>;
    }

    open-at: func(path: string, flags: open-flags) -> result<descriptor, u32>;
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	out := GeneratePromise(modules, "wasi")

	// Verify key constructs are present
	assertContains(t, out, "enum DescriptorType `public {")
	assertContains(t, out, "type DescriptorStat `public `value {")
	assertContains(t, out, "type OpenFlags `public `value {")
	assertContains(t, out, "type Descriptor `public `target(wasi) {")
	assertContains(t, out, "drop(~this) {")
	assertContains(t, out, "open_at!(string path, OpenFlags flags) Descriptor `public `target(wasi) {")
	assertContains(t, out, "`wasm_import(\"wasi:filesystem/types\"")
	assertContains(t, out, "`target(wasi);")
}

func TestWitToIRUseResolution(t *testing.T) {
	src := `
interface types {
    record descriptor-stat {
        size: u64,
    }

    enum error-code {
        access,
        not-found,
    }
}

interface fs {
    use types.{descriptor-stat, error-code};

    get-stat: func() -> descriptor-stat;
    last-error: func() -> error-code;
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	if len(modules) != 2 {
		t.Fatalf("expected 2 modules, got %d", len(modules))
	}
	fsModule := modules[1]
	if fsModule.Name != "fs" {
		t.Fatalf("expected module name 'fs', got %q", fsModule.Name)
	}
	// The use-statement should have imported 2 types into the fs module.
	if len(fsModule.Types) != 2 {
		t.Fatalf("expected 2 types in fs module, got %d", len(fsModule.Types))
	}
	names := map[string]bool{}
	for _, ty := range fsModule.Types {
		names[ty.Name] = true
	}
	if !names["DescriptorStat"] {
		t.Error("expected DescriptorStat type in fs module")
	}
	if !names["ErrorCode"] {
		t.Error("expected ErrorCode type in fs module")
	}
}

func TestWitToIRUseResolutionWithRename(t *testing.T) {
	src := `
interface types {
    record descriptor-stat {
        size: u64,
    }
}

interface fs {
    use types.{descriptor-stat as stat};

    get-stat: func() -> stat;
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	fsModule := modules[1]
	if len(fsModule.Types) != 1 {
		t.Fatalf("expected 1 type in fs module, got %d", len(fsModule.Types))
	}
	if fsModule.Types[0].Name != "Stat" {
		t.Errorf("expected renamed type 'Stat', got %q", fsModule.Types[0].Name)
	}
	// Verify the type retains its structure.
	if fsModule.Types[0].Kind != TypeRecord {
		t.Errorf("expected Record kind, got %v", fsModule.Types[0].Kind)
	}
	if len(fsModule.Types[0].Fields) != 1 {
		t.Errorf("expected 1 field, got %d", len(fsModule.Types[0].Fields))
	}
}

func TestWitToIRUseMissingTypeSkipped(t *testing.T) {
	src := `
interface types {
    record real-type {
        x: u32,
    }
}

interface fs {
    use types.{nonexistent};

    get: func() -> u32;
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	fsModule := modules[1]
	// The nonexistent type should be silently skipped.
	if len(fsModule.Types) != 0 {
		t.Errorf("expected 0 types, got %d", len(fsModule.Types))
	}
}

func TestWitToIRUseMissingSourceSkipped(t *testing.T) {
	src := `
interface fs {
    use external-pkg.{some-type};

    get: func() -> u32;
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WitToIR(file)
	// External use should be silently skipped — no types added, no panic.
	if len(modules[0].Types) != 0 {
		t.Errorf("expected 0 types, got %d", len(modules[0].Types))
	}
	if len(modules[0].Functions) != 1 {
		t.Errorf("expected 1 function, got %d", len(modules[0].Functions))
	}
}

func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("output does not contain %q\n\nFull output:\n%s", substr, s)
	}
}

func assertNotContains(t *testing.T, s, substr string) {
	t.Helper()
	if strings.Contains(s, substr) {
		t.Errorf("output should NOT contain %q\n\nFull output:\n%s", substr, s)
	}
}

// --- Canonical ABI tests ---

func TestFlattenTypeScalars(t *testing.T) {
	tests := []struct {
		ref  TypeRef
		want []FlatType
	}{
		{TypeRef{Kind: BuiltinKind, Builtin: "u32"}, []FlatType{FlatI32}},
		{TypeRef{Kind: BuiltinKind, Builtin: "u64"}, []FlatType{FlatI64}},
		{TypeRef{Kind: BuiltinKind, Builtin: "f32"}, []FlatType{FlatF32}},
		{TypeRef{Kind: BuiltinKind, Builtin: "f64"}, []FlatType{FlatF64}},
		{TypeRef{Kind: BuiltinKind, Builtin: "bool"}, []FlatType{FlatI32}},
		{TypeRef{Kind: BuiltinKind, Builtin: "char"}, []FlatType{FlatI32}},
		{TypeRef{Kind: BuiltinKind, Builtin: "s8"}, []FlatType{FlatI32}},
		{TypeRef{Kind: BuiltinKind, Builtin: "s16"}, []FlatType{FlatI32}},
	}
	for _, tt := range tests {
		got := flattenType(tt.ref)
		if len(got) != len(tt.want) {
			t.Errorf("flattenType(%s): got %d flat types, want %d", tt.ref.Builtin, len(got), len(tt.want))
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("flattenType(%s)[%d]: got %d, want %d", tt.ref.Builtin, i, got[i], tt.want[i])
			}
		}
	}
}

func TestFlattenTypeString(t *testing.T) {
	ref := TypeRef{Kind: BuiltinKind, Builtin: "string"}
	got := flattenType(ref)
	if len(got) != 2 || got[0] != FlatI32 || got[1] != FlatI32 {
		t.Errorf("flattenType(string): got %v, want [i32, i32]", got)
	}
}

func TestFlattenTypeList(t *testing.T) {
	ref := TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u8"}}
	got := flattenType(ref)
	if len(got) != 2 || got[0] != FlatI32 || got[1] != FlatI32 {
		t.Errorf("flattenType(list<u8>): got %v, want [i32, i32]", got)
	}
}

func TestFlattenTypeOption(t *testing.T) {
	ref := TypeRef{Kind: OptionKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}}
	got := flattenType(ref)
	// option<u32> → i32 (discriminant) + i32 (payload) = 2
	if len(got) != 2 || got[0] != FlatI32 || got[1] != FlatI32 {
		t.Errorf("flattenType(option<u32>): got %v, want [i32, i32]", got)
	}
}

func TestFlattenTypeResult(t *testing.T) {
	ref := TypeRef{
		Kind: ResultKind,
		Ok:   &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
		Err:  &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
	}
	got := flattenType(ref)
	// result<u32, u32> → i32 (discriminant) + i32 (max of ok/err) = 2
	if len(got) != 2 || got[0] != FlatI32 || got[1] != FlatI32 {
		t.Errorf("flattenType(result<u32, u32>): got %v, want [i32, i32]", got)
	}
}

func TestFlattenTypeResultStringOk(t *testing.T) {
	ref := TypeRef{
		Kind: ResultKind,
		Ok:   &TypeRef{Kind: BuiltinKind, Builtin: "string"},
	}
	got := flattenType(ref)
	// result<string, void> → i32 (discriminant) + i32 + i32 (string ptr,len) = 3
	if len(got) != 3 {
		t.Errorf("flattenType(result<string>): got %d flat types, want 3", len(got))
	}
}

func TestFlattenTypeHandle(t *testing.T) {
	own := TypeRef{Kind: OwnKind, Elem: &TypeRef{Kind: NamedKind, Name: "Res"}}
	borrow := TypeRef{Kind: BorrowKind, Elem: &TypeRef{Kind: NamedKind, Name: "Res"}}
	if got := flattenType(own); len(got) != 1 || got[0] != FlatI32 {
		t.Errorf("flattenType(own<Res>): got %v, want [i32]", got)
	}
	if got := flattenType(borrow); len(got) != 1 || got[0] != FlatI32 {
		t.Errorf("flattenType(borrow<Res>): got %v, want [i32]", got)
	}
}

func TestFlattenTypeTuple(t *testing.T) {
	ref := TypeRef{
		Kind: TupleKind,
		Elements: []TypeRef{
			{Kind: BuiltinKind, Builtin: "u32"},
			{Kind: BuiltinKind, Builtin: "f64"},
			{Kind: BuiltinKind, Builtin: "bool"},
		},
	}
	got := flattenType(ref)
	// (u32, f64, bool) → i32 + f64 + i32 = 3
	if len(got) != 3 || got[0] != FlatI32 || got[1] != FlatF64 || got[2] != FlatI32 {
		t.Errorf("flattenType(tuple): got %v, want [i32, f64, i32]", got)
	}
}

func TestFlattenParams(t *testing.T) {
	params := []Param{
		{Name: "name", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}},
		{Name: "count", Type: TypeRef{Kind: BuiltinKind, Builtin: "u32"}},
	}
	flat, usePtr := flattenParams(params)
	if usePtr {
		t.Error("expected direct params, got usePtr=true")
	}
	if len(flat) != 3 {
		t.Fatalf("expected 3 flat params, got %d", len(flat))
	}
	// string → (name_ptr, name_len), u32 → count
	if flat[0].Name != "name_ptr" || flat[0].Type != FlatI32 {
		t.Errorf("flat[0]: got %v", flat[0])
	}
	if flat[1].Name != "name_len" || flat[1].Type != FlatI32 {
		t.Errorf("flat[1]: got %v", flat[1])
	}
	if flat[2].Name != "count" || flat[2].Type != FlatI32 {
		t.Errorf("flat[2]: got %v", flat[2])
	}
}

func TestFlattenParamsExceedsLimit(t *testing.T) {
	// Create > MaxFlatParams params
	var params []Param
	for i := 0; i < 9; i++ {
		params = append(params, Param{
			Name: fmt.Sprintf("s%d", i),
			Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}, // 2 flat each
		})
	}
	_, usePtr := flattenParams(params)
	if !usePtr {
		t.Error("expected usePtr=true for > 16 flat params")
	}
}

func TestFlattenResultsRetPtr(t *testing.T) {
	// string return → 2 flat results > MaxFlatResults (1) → retptr
	results := []TypeRef{{Kind: BuiltinKind, Builtin: "string"}}
	flat, useRetPtr := flattenResults(results)
	if !useRetPtr {
		t.Error("expected useRetPtr=true for string return")
	}
	if len(flat) != 2 {
		t.Errorf("expected 2 flat results, got %d", len(flat))
	}
}

func TestFlattenResultsDirect(t *testing.T) {
	// u32 return → 1 flat result ≤ MaxFlatResults → direct
	results := []TypeRef{{Kind: BuiltinKind, Builtin: "u32"}}
	flat, useRetPtr := flattenResults(results)
	if useRetPtr {
		t.Error("expected useRetPtr=false for u32 return")
	}
	if len(flat) != 1 || flat[0] != FlatI32 {
		t.Errorf("expected [i32], got %v", flat)
	}
}

func TestNeedsCanonicalLowering(t *testing.T) {
	tests := []struct {
		ref  TypeRef
		want bool
	}{
		{TypeRef{Kind: BuiltinKind, Builtin: "u32"}, false},
		{TypeRef{Kind: BuiltinKind, Builtin: "string"}, true},
		{TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u8"}}, true},
		{TypeRef{Kind: OwnKind, Elem: &TypeRef{Kind: NamedKind, Name: "R"}}, false},
		{TypeRef{Kind: BorrowKind, Elem: &TypeRef{Kind: NamedKind, Name: "R"}}, false},
		{TypeRef{Kind: OptionKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}}, true},
		{TypeRef{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}}, true},
	}
	for _, tt := range tests {
		got := needsCanonicalLowering(tt.ref)
		if got != tt.want {
			t.Errorf("needsCanonicalLowering(%v): got %v, want %v", tt.ref.Kind, got, tt.want)
		}
	}
}

// --- Canonical ABI codegen tests ---

func TestCodegenCanonicalABIStringParam(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "greet",
			Kind:       FuncFree,
			Params:     []Param{{Name: "name", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
			ImportName: "greet",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// Extern should have flat params: i32 name_ptr, i32 name_len
	assertContains(t, out, "_greet(i32 name_ptr, i32 name_len)")
	// Wrapper should lower string to flat values
	assertContains(t, out, "_cabi_string_data(name)")
	assertContains(t, out, "_cabi_string_len(name)")
	// Helper declarations should be present
	assertContains(t, out, "_cabi_string_data(string s) i32")
	assertContains(t, out, "_cabi_string_len(string s) i32")
}

func TestCodegenCanonicalABIStringReturn(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "get_name",
			Kind:       FuncFree,
			Results:    []TypeRef{{Kind: BuiltinKind, Builtin: "string"}},
			ImportName: "get-name",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// String return → retptr pattern
	assertContains(t, out, "i32 retptr)")
	// Wrapper should lift string from retptr
	assertContains(t, out, "_cabi_string_from(")
	assertContains(t, out, "_cabi_load_i32(")
}

func TestCodegenCanonicalABIScalarUnchanged(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "add",
			Kind:       FuncFree,
			Params:     []Param{{Name: "a", Type: TypeRef{Kind: BuiltinKind, Builtin: "u32"}}, {Name: "b", Type: TypeRef{Kind: BuiltinKind, Builtin: "u32"}}},
			Results:    []TypeRef{{Kind: BuiltinKind, Builtin: "u32"}},
			ImportName: "add",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// Scalar extern should use flat types
	assertContains(t, out, "_add(i32 a, i32 b) i32")
	// Wrapper should pass scalars directly
	assertContains(t, out, "return _add(a, b);")
}

func TestCodegenCanonicalABIResultRetPtr(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "do_thing",
			Kind:       FuncFree,
			Results:    []TypeRef{{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}}},
			ImportName: "do-thing",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// result<u32> → 2 flat results → retptr
	assertContains(t, out, "i32 retptr)")
	// Wrapper should check discriminant
	assertContains(t, out, "_cabi_load_i32(_cabi_retarea_ptr())")
}

func TestCodegenCanonicalABIResourceHandle(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "Descriptor",
			Drop: true,
			Methods: []Func{{
				Name:       "read",
				Kind:       FuncMethod,
				Params:     []Param{{Name: "length", Type: TypeRef{Kind: BuiltinKind, Builtin: "u64"}}},
				Results:    []TypeRef{{Kind: BuiltinKind, Builtin: "u32"}},
				ImportName: "[method]descriptor.read",
			}},
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// Resource method extern: handle is i32
	assertContains(t, out, "i32 handle")
	// u64 param stays i64
	assertContains(t, out, "i64 length")
}

func TestCodegenCanonicalABIHelpers(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "noop",
			Kind:       FuncFree,
			ImportName: "noop",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// All canonical ABI helpers should be declared
	assertContains(t, out, "_cabi_load_i32(i32 ptr) i32")
	assertContains(t, out, "_cabi_load_i64(i32 ptr) i64")
	assertContains(t, out, "_cabi_load_f32(i32 ptr) f32")
	assertContains(t, out, "_cabi_load_f64(i32 ptr) f64")
	assertContains(t, out, "_cabi_store_i32(i32 ptr, i32 val)")
	assertContains(t, out, "_cabi_store_i64(i32 ptr, i64 val)")
	assertContains(t, out, "_cabi_string_data(string s) i32")
	assertContains(t, out, "_cabi_string_from(i32 ptr, i32 len) string")
	assertContains(t, out, "_cabi_retarea_ptr() i32")
}

func TestCodegenNonCanonicalUnchanged(t *testing.T) {
	// Without canonical ABI, string params should NOT be flattened
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "greet",
			Kind:       FuncFree,
			Params:     []Param{{Name: "name", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
			ImportName: "greet",
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	// Should NOT flatten to ptr/len
	assertNotContains(t, out, "name_ptr")
	assertNotContains(t, out, "name_len")
	// Should use Promise string type
	assertContains(t, out, "string name")
}

func TestFlatParamName(t *testing.T) {
	tests := []struct {
		base  string
		index int
		total int
		want  string
	}{
		{"name", 0, 2, "name_ptr"},
		{"name", 1, 2, "name_len"},
		{"val", 0, 3, "val_tag"},
		{"val", 1, 3, "val_0"},
		{"val", 2, 3, "val_1"},
	}
	for _, tt := range tests {
		got := flatParamName(tt.base, tt.index, tt.total)
		if got != tt.want {
			t.Errorf("flatParamName(%q, %d, %d) = %q, want %q", tt.base, tt.index, tt.total, got, tt.want)
		}
	}
}

func TestFlatPromiseTypeAllCases(t *testing.T) {
	tests := []struct {
		ft   FlatType
		want string
	}{
		{FlatI32, "i32"},
		{FlatI64, "i64"},
		{FlatF32, "f32"},
		{FlatF64, "f64"},
		{FlatType(99), "i32"}, // default
	}
	for _, tt := range tests {
		got := flatPromiseType(tt.ft)
		if got != tt.want {
			t.Errorf("flatPromiseType(%d) = %q, want %q", tt.ft, got, tt.want)
		}
	}
}

func TestFlattenTypeNamed(t *testing.T) {
	ref := TypeRef{Kind: NamedKind, Name: "MyRecord"}
	got := flattenType(ref)
	if len(got) != 1 || got[0] != FlatI32 {
		t.Errorf("flattenType(named): got %v, want [i32]", got)
	}
}

func TestFlattenTypeDefaultKind(t *testing.T) {
	// Use a kind value that doesn't match any case
	ref := TypeRef{Kind: TypeRefKind(99)}
	got := flattenType(ref)
	if len(got) != 1 || got[0] != FlatI32 {
		t.Errorf("flattenType(unknown kind): got %v, want [i32]", got)
	}
}

func TestFlattenBuiltinDefault(t *testing.T) {
	got := flattenBuiltin("unknown_type")
	if len(got) != 1 || got[0] != FlatI32 {
		t.Errorf("flattenBuiltin(unknown): got %v, want [i32]", got)
	}
}

func TestFlattenResultErrWiderThanOk(t *testing.T) {
	// result<u32, string> — err (string → 2 flat) is wider than ok (u32 → 1 flat)
	ref := TypeRef{
		Kind: ResultKind,
		Ok:   &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
		Err:  &TypeRef{Kind: BuiltinKind, Builtin: "string"},
	}
	got := flattenType(ref)
	// discriminant (i32) + max(1, 2) = 3
	if len(got) != 3 {
		t.Errorf("flattenType(result<u32, string>): got %d flat types, want 3", len(got))
	}
}

func TestFlattenResultBothNil(t *testing.T) {
	// result<void, void>
	ref := TypeRef{Kind: ResultKind}
	got := flattenType(ref)
	// discriminant only
	if len(got) != 1 || got[0] != FlatI32 {
		t.Errorf("flattenType(result<void,void>): got %v, want [i32]", got)
	}
}

func TestCodegenMethodWrapperVoidClose(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "Descriptor",
			Drop: true,
			Methods: []Func{{
				Name:       "close",
				Kind:       FuncMethod,
				Results:    []TypeRef{},
				ImportName: "[method]descriptor.close",
			}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "close(this) `public {")
	assertContains(t, out, "_descriptor_close(this._handle);")
}

func TestCodegenMethodWrapperFailableVoid(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "Descriptor",
			Drop: true,
			Methods: []Func{{
				Name:       "sync",
				Kind:       FuncMethod,
				Results:    []TypeRef{{Kind: ResultKind}},
				ImportName: "[method]descriptor.sync",
			}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "sync!(this) `public {")
	assertContains(t, out, "_descriptor_sync(this._handle)^;")
}

func TestCodegenFreeFuncVoidFailableRetPtr(t *testing.T) {
	// result<void, E> with canonical ABI → retptr for discriminant check, no return value
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "do_action",
			Kind:       FuncFree,
			Results:    []TypeRef{{Kind: ResultKind}},
			ImportName: "do-action",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// Wrapper should be failable with no return type
	assertContains(t, out, "do_action!() `public `target(wasi) {")
}

func TestCodegenCanonicalABIStringReturnDirect(t *testing.T) {
	// Non-result string return → retptr (string is 2 flat > MaxFlatResults=1)
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "get_name",
			Kind:       FuncFree,
			Results:    []TypeRef{{Kind: BuiltinKind, Builtin: "string"}},
			ImportName: "get-name",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// Should use retptr and lift string from offset 0
	assertContains(t, out, "_cabi_string_from(_cabi_load_i32(_cabi_retarea_ptr()), _cabi_load_i32(_cabi_retarea_ptr() + 4))")
}

func TestCodegenCanonicalABIResultStringOk(t *testing.T) {
	// result<string, E> → retptr, string lifted from offset 4
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "get_value",
			Kind:       FuncFree,
			Results:    []TypeRef{{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
			ImportName: "get-value",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	assertContains(t, out, "_cabi_string_from(_cabi_load_i32(_cabi_retarea_ptr() + 4), _cabi_load_i32(_cabi_retarea_ptr() + 8))")
}

func TestCodegenCanonicalABIResultF64Ok(t *testing.T) {
	// result<f64, E> → retptr, f64 lifted via _cabi_load_f64
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "get_value",
			Kind:       FuncFree,
			Results:    []TypeRef{{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "f64"}}},
			ImportName: "get-value",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	assertContains(t, out, "_cabi_load_f64(_cabi_retarea_ptr() + 8)")
}

func TestCodegenCanonicalABIResultF32Ok(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "get_value",
			Kind:       FuncFree,
			Results:    []TypeRef{{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "f32"}}},
			ImportName: "get-value",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	assertContains(t, out, "_cabi_load_f32(_cabi_retarea_ptr() + 4)")
}

func TestCodegenCanonicalABIResultU64Ok(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "get_value",
			Kind:       FuncFree,
			Results:    []TypeRef{{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "u64"}}},
			ImportName: "get-value",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	assertContains(t, out, "_cabi_load_i64(_cabi_retarea_ptr() + 8)")
}

func TestCodegenCanonicalABIResultS64Ok(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "get_value",
			Kind:       FuncFree,
			Results:    []TypeRef{{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "s64"}}},
			ImportName: "get-value",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	assertContains(t, out, "_cabi_load_i64(_cabi_retarea_ptr() + 8)")
}

func TestCodegenCanonicalABIDirectScalarReturn(t *testing.T) {
	// u32 return with canonical ABI → direct return (1 flat ≤ MaxFlatResults)
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "get_count",
			Kind:       FuncFree,
			Results:    []TypeRef{{Kind: BuiltinKind, Builtin: "u32"}},
			ImportName: "get-count",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// Should NOT use retptr — direct return
	assertNotContains(t, out, "retptr")
	assertContains(t, out, "_get_count() i32")
}

func TestCodegenCanonicalABIDirectFailable(t *testing.T) {
	// result<void, E> with canonical ABI → discriminant is 1 flat → direct return
	// But result<void> flattens to just [i32] (discriminant) which is ≤ MaxFlatResults
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name:       "try_thing",
			Kind:       FuncFree,
			Results:    []TypeRef{{Kind: ResultKind}},
			ImportName: "try-thing",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// result<void,void> → 1 flat result → direct, not retptr
	assertContains(t, out, "_try_thing()")
}

func TestLowerParamToFlatList(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	p := Param{Name: "data", Type: TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u8"}}}
	got := g.lowerParamToFlat(p)
	if len(got) != 2 || got[0] != "_cabi_vector_data_u8(data)" || got[1] != "_cabi_vector_len_u8(data)" {
		t.Errorf("lowerParamToFlat(list<u8>): got %v, want [_cabi_vector_data_u8(data), _cabi_vector_len_u8(data)]", got)
	}
}

func TestLowerParamToFlatListS32(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	p := Param{Name: "ids", Type: TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "s32"}}}
	got := g.lowerParamToFlat(p)
	if len(got) != 2 || got[0] != "_cabi_vector_data_i32(ids)" || got[1] != "_cabi_vector_len_i32(ids)" {
		t.Errorf("lowerParamToFlat(list<s32>): got %v", got)
	}
}

func TestLowerParamToFlatDefault(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	p := Param{Name: "val", Type: TypeRef{Kind: NamedKind, Name: "MyRecord"}}
	got := g.lowerParamToFlat(p)
	if len(got) != 1 || got[0] != "val" {
		t.Errorf("lowerParamToFlat(named): got %v, want [val]", got)
	}
}

func TestNeedsCanonicalLoweringTuple(t *testing.T) {
	ref := TypeRef{Kind: TupleKind, Elements: []TypeRef{{Kind: BuiltinKind, Builtin: "u32"}}}
	if !needsCanonicalLowering(ref) {
		t.Error("expected needsCanonicalLowering(tuple) = true")
	}
}

func TestNeedsCanonicalLoweringNamed(t *testing.T) {
	ref := TypeRef{Kind: NamedKind, Name: "Foo"}
	if !needsCanonicalLowering(ref) {
		t.Error("expected needsCanonicalLowering(named) = true")
	}
}

func TestNeedsCanonicalLoweringDefaultKind(t *testing.T) {
	ref := TypeRef{Kind: TypeRefKind(99)}
	if needsCanonicalLowering(ref) {
		t.Error("expected needsCanonicalLowering(unknown) = false")
	}
}

func TestLiftReturnFromRetPtrEmpty(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	got := g.liftReturnFromRetPtr(nil)
	if got != "" {
		t.Errorf("liftReturnFromRetPtr(nil) = %q, want empty", got)
	}
}

func TestLiftScalarFromRetPtrOffset0(t *testing.T) {
	ref := TypeRef{Kind: BuiltinKind, Builtin: "u32"}
	got := liftScalarFromRetPtr(ref, 0)
	if got != "_cabi_load_i32(_cabi_retarea_ptr())" {
		t.Errorf("liftScalarFromRetPtr(u32, 0) = %q", got)
	}
}

func TestLiftScalarFromRetPtrNonBuiltin(t *testing.T) {
	ref := TypeRef{Kind: NamedKind, Name: "MyType"}
	got := liftScalarFromRetPtr(ref, 8)
	if got != "_cabi_load_i32(_cabi_retarea_ptr() + 8)" {
		t.Errorf("liftScalarFromRetPtr(named, 8) = %q", got)
	}
}

func TestFormatExternReturnSigCanonicalFailable(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	// result<u32> → 2 flat → retptr, failable
	results := []TypeRef{{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}}}
	got := g.formatExternReturnSig(results)
	// retptr + failable → void extern (retptr in params)
	if got != "" {
		t.Errorf("formatExternReturnSig(result<u32>) = %q, want empty (retptr pattern)", got)
	}
}

func TestFormatExternReturnSigCanonicalDirectFailable(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	// result<void> → [i32] (discriminant) → 1 flat → direct, failable
	results := []TypeRef{{Kind: ResultKind}}
	got := g.formatExternReturnSig(results)
	// Extern returns the discriminant as i32, marked failable
	if got != " i32!" {
		t.Errorf("formatExternReturnSig(result<void>) = %q, want \" i32!\"", got)
	}
}

func TestFormatExternReturnSigCanonicalDirectScalar(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	results := []TypeRef{{Kind: BuiltinKind, Builtin: "u32"}}
	got := g.formatExternReturnSig(results)
	if got != " i32" {
		t.Errorf("formatExternReturnSig(u32) = %q, want \" i32\"", got)
	}
}

func TestFormatExternReturnSigCanonicalEmpty(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	got := g.formatExternReturnSig(nil)
	if got != "" {
		t.Errorf("formatExternReturnSig(nil) = %q, want empty", got)
	}
}

func TestFormatReturnSigFailable(t *testing.T) {
	g := &generator{}
	// Failable with no Ok type → just "!"
	results := []TypeRef{{Kind: ResultKind}}
	got := g.formatReturnSig(results)
	if got != "!" {
		t.Errorf("formatReturnSig(result<void>) = %q, want \"!\"", got)
	}
}

func TestCodegenFlagsDoc(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/types",
		Types: []Type{{
			Name:   "Perms",
			Kind:   TypeFlags,
			Doc:    "Permission flags.",
			Fields: []Field{{Name: "read"}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "`doc \"Permission flags.\"")
}

func TestCodegenTypeAliasDoc(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/types",
		Types: []Type{{
			Name:   "ErrorCode",
			Kind:   TypeAlias,
			Doc:    "Error code type.",
			Target: &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "// ErrorCode: Error code type.")
}

func TestCodegenResourceDoc(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "File",
			Drop: true,
			Doc:  "A file handle.",
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "`doc \"A file handle.\"")
}

func TestCodegenResourceNoDrop(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{{
			Name: "Handle",
			Drop: false,
			Methods: []Func{{
				Name:       "value",
				Kind:       FuncMethod,
				Results:    []TypeRef{{Kind: BuiltinKind, Builtin: "u32"}},
				ImportName: "[method]handle.value",
			}},
		}},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "type Handle `public `target(wasi) {")
	assertNotContains(t, out, "drop(~this)")
	assertNotContains(t, out, "[resource-drop]")
}

func TestEscapeDocSpecialChars(t *testing.T) {
	got := escapeDoc("line1\nline2 with \"quotes\" and \\backslash")
	want := "line1 line2 with \\\"quotes\\\" and \\\\backslash"
	if got != want {
		t.Errorf("escapeDoc: got %q, want %q", got, want)
	}
}

func TestToKebab(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Descriptor", "descriptor"},
		{"DescriptorStat", "descriptor-stat"},
		{"simple", "simple"},
	}
	for _, tt := range tests {
		got := toKebab(tt.in)
		if got != tt.want {
			t.Errorf("toKebab(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatExternParamsWithRetPtrEmptyBase(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	// No params, but result needs retptr
	results := []TypeRef{{Kind: BuiltinKind, Builtin: "string"}}
	got := g.formatExternParamsWithRetPtr(nil, results)
	if got != "i32 retptr" {
		t.Errorf("formatExternParamsWithRetPtr(nil, string) = %q, want \"i32 retptr\"", got)
	}
}

func TestFormatExternParamsOverflow(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	// > MaxFlatParams → args_ptr
	var params []Param
	for i := 0; i < 9; i++ {
		params = append(params, Param{
			Name: fmt.Sprintf("s%d", i),
			Type: TypeRef{Kind: BuiltinKind, Builtin: "string"},
		})
	}
	got := g.formatExternParams(params)
	if got != "i32 args_ptr" {
		t.Errorf("formatExternParams(overflow) = %q, want \"i32 args_ptr\"", got)
	}
}

// --- Error payload extraction tests (T0294) ---

func TestCodegenCanonicalABIResultErrScalar(t *testing.T) {
	// result<u32, u32>: error branch should load i32 and call .to_string()
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name: "do_thing",
			Kind: FuncFree,
			Results: []TypeRef{{
				Kind: ResultKind,
				Ok:   &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
				Err:  &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
			}},
			ImportName: "do-thing",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	assertContains(t, out, `_cabi_load_i32(_cabi_retarea_ptr() + 4).to_string()`)
	assertContains(t, out, `"component error: "`)
	assertNotContains(t, out, `error("component error")`)
}

func TestCodegenCanonicalABIResultErrString(t *testing.T) {
	// result<u32, string>: error branch should lift string via _cabi_string_from
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name: "do_thing",
			Kind: FuncFree,
			Results: []TypeRef{{
				Kind: ResultKind,
				Ok:   &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
				Err:  &TypeRef{Kind: BuiltinKind, Builtin: "string"},
			}},
			ImportName: "do-thing",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// Error branch lifts string from retarea
	assertContains(t, out, `error(_cabi_string_from(_cabi_load_i32(_cabi_retarea_ptr() + 4), _cabi_load_i32(_cabi_retarea_ptr() + 8)))`)
}

func TestCodegenCanonicalABIResultErrVoidOk(t *testing.T) {
	// result<void, u32>: void return with retptr, error should extract payload
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name: "do_action",
			Kind: FuncFree,
			Results: []TypeRef{{
				Kind: ResultKind,
				Err:  &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
			}},
			ImportName: "do-action",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	assertContains(t, out, `_cabi_load_i32(_cabi_retarea_ptr() + 4).to_string()`)
	assertContains(t, out, `"component error: "`)
}

func TestCodegenCanonicalABIResultUnionAlignment(t *testing.T) {
	// result<u32, f64>: both Ok and Err should use offset 8 (f64 forces alignment)
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name: "get_value",
			Kind: FuncFree,
			Results: []TypeRef{{
				Kind: ResultKind,
				Ok:   &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
				Err:  &TypeRef{Kind: BuiltinKind, Builtin: "f64"},
			}},
			ImportName: "get-value",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// Ok value at offset 8 (not 4, because f64 err forces union alignment)
	assertContains(t, out, "_cabi_load_i32(_cabi_retarea_ptr() + 8)")
	// Err value at offset 8
	assertContains(t, out, "_cabi_load_f64(_cabi_retarea_ptr() + 8).to_string()")
}

func TestCodegenCanonicalABIResultStringOkWiderErr(t *testing.T) {
	// result<string, f64>: string Ok at offset 8 (f64 forces union alignment)
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name: "get_name",
			Kind: FuncFree,
			Results: []TypeRef{{
				Kind: ResultKind,
				Ok:   &TypeRef{Kind: BuiltinKind, Builtin: "string"},
				Err:  &TypeRef{Kind: BuiltinKind, Builtin: "f64"},
			}},
			ImportName: "get-name",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// String Ok lifted from offset 8 (not 4)
	assertContains(t, out, "_cabi_string_from(_cabi_load_i32(_cabi_retarea_ptr() + 8), _cabi_load_i32(_cabi_retarea_ptr() + 12))")
}

func TestLiftErrFromRetPtr(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}

	// Void error (Err == nil) → generic message
	got := g.liftErrFromRetPtr(TypeRef{Kind: ResultKind})
	if got != `"component error"` {
		t.Errorf("liftErrFromRetPtr(void): got %q", got)
	}

	// String error
	got = g.liftErrFromRetPtr(TypeRef{
		Kind: ResultKind,
		Ok:   &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
		Err:  &TypeRef{Kind: BuiltinKind, Builtin: "string"},
	})
	if !strings.Contains(got, "_cabi_string_from(") {
		t.Errorf("liftErrFromRetPtr(string): got %q, want _cabi_string_from", got)
	}

	// Scalar error (u32)
	got = g.liftErrFromRetPtr(TypeRef{
		Kind: ResultKind,
		Ok:   &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
		Err:  &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
	})
	if !strings.Contains(got, ".to_string()") {
		t.Errorf("liftErrFromRetPtr(u32): got %q, want .to_string()", got)
	}
	if !strings.Contains(got, `"component error: "`) {
		t.Errorf("liftErrFromRetPtr(u32): got %q, want component error prefix", got)
	}

	// u64 error — offset should be 8
	got = g.liftErrFromRetPtr(TypeRef{
		Kind: ResultKind,
		Ok:   &TypeRef{Kind: BuiltinKind, Builtin: "u32"},
		Err:  &TypeRef{Kind: BuiltinKind, Builtin: "u64"},
	})
	if !strings.Contains(got, "_cabi_load_i64(_cabi_retarea_ptr() + 8)") {
		t.Errorf("liftErrFromRetPtr(u64): got %q, want offset 8", got)
	}
}

func TestFormatExternParamsWithRetPtrNonEmptyBase(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	// Params + result that needs retptr → base params + ", i32 retptr"
	params := []Param{{Name: "id", Type: TypeRef{Kind: BuiltinKind, Builtin: "u32"}}}
	results := []TypeRef{{Kind: BuiltinKind, Builtin: "string"}} // string → 2 flat → retptr
	got := g.formatExternParamsWithRetPtr(params, results)
	if got != "i32 id, i32 retptr" {
		t.Errorf("formatExternParamsWithRetPtr(params+string) = %q, want \"i32 id, i32 retptr\"", got)
	}
}

func TestLiftReturnFromRetPtrNamedType(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	// Named type return → falls through to liftScalarFromRetPtr at offset 0
	results := []TypeRef{{Kind: NamedKind, Name: "MyRecord"}}
	got := g.liftReturnFromRetPtr(results)
	if got != "_cabi_load_i32(_cabi_retarea_ptr() + 0)" {
		t.Errorf("liftReturnFromRetPtr(named) = %q", got)
	}
}

func TestPromiseTypeUnknownKind(t *testing.T) {
	ref := TypeRef{Kind: TypeRefKind(99)}
	got := promiseType(ref)
	if got != "int" {
		t.Errorf("promiseType(unknown) = %q, want \"int\"", got)
	}
}

func TestKebabToPascalConsecutiveDashes(t *testing.T) {
	// Consecutive dashes produce empty parts that should be skipped
	got := kebabToPascal("a--b")
	if got != "AB" {
		t.Errorf("kebabToPascal(\"a--b\") = %q, want \"AB\"", got)
	}
}

func TestConvertTypeRefNil(t *testing.T) {
	got := convertTypeRef(nil)
	if got.Kind != BuiltinKind || got.Builtin != "void" {
		t.Errorf("convertTypeRef(nil) = {Kind:%v Builtin:%q}, want {BuiltinKind, \"void\"}", got.Kind, got.Builtin)
	}
}

func TestConvertTypeRefUnknownKind(t *testing.T) {
	ref := &wit.TypeRef{Kind: wit.TypeRefKind(99)}
	got := convertTypeRef(ref)
	if got.Kind != BuiltinKind || got.Builtin != "u32" {
		t.Errorf("convertTypeRef(unknown kind) = {Kind:%v Builtin:%q}, want {BuiltinKind, \"u32\"}", got.Kind, got.Builtin)
	}
}

func TestResultPayloadOffsetUnion(t *testing.T) {
	tests := []struct {
		name   string
		result TypeRef
		want   int
	}{
		{"u32/u32", TypeRef{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}, Err: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}}, 4},
		{"u32/f64", TypeRef{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}, Err: &TypeRef{Kind: BuiltinKind, Builtin: "f64"}}, 8},
		{"f64/u32", TypeRef{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "f64"}, Err: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}}, 8},
		{"f64/f64", TypeRef{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "f64"}, Err: &TypeRef{Kind: BuiltinKind, Builtin: "f64"}}, 8},
		{"string/u32", TypeRef{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "string"}, Err: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}}, 4},
		{"u32/u64", TypeRef{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}, Err: &TypeRef{Kind: BuiltinKind, Builtin: "u64"}}, 8},
		{"void/void", TypeRef{Kind: ResultKind}, 4},
		{"void/u32", TypeRef{Kind: ResultKind, Err: &TypeRef{Kind: BuiltinKind, Builtin: "u32"}}, 4},
		{"void/s64", TypeRef{Kind: ResultKind, Err: &TypeRef{Kind: BuiltinKind, Builtin: "s64"}}, 8},
	}
	for _, tt := range tests {
		got := resultPayloadOffset(tt.result)
		if got != tt.want {
			t.Errorf("resultPayloadOffset(%s): got %d, want %d", tt.name, got, tt.want)
		}
	}
}

// --- Vector/list canonical ABI tests (T0292) ---

func TestWitElemSize(t *testing.T) {
	tests := []struct {
		builtin string
		want    int
	}{
		{"u8", 1}, {"s8", 1}, {"bool", 1},
		{"u16", 2}, {"s16", 2},
		{"u32", 4}, {"s32", 4}, {"f32", 4}, {"char", 4},
		{"u64", 8}, {"s64", 8}, {"f64", 8},
		{"unknown", 4},
	}
	for _, tt := range tests {
		got := witElemSize(tt.builtin)
		if got != tt.want {
			t.Errorf("witElemSize(%q) = %d, want %d", tt.builtin, got, tt.want)
		}
	}
}

func TestVectorHelperSuffix(t *testing.T) {
	tests := []struct {
		elem TypeRef
		want string
	}{
		{TypeRef{Kind: BuiltinKind, Builtin: "u8"}, "u8"},
		{TypeRef{Kind: BuiltinKind, Builtin: "s32"}, "i32"},
		{TypeRef{Kind: BuiltinKind, Builtin: "f64"}, "f64"},
		{TypeRef{Kind: BuiltinKind, Builtin: "bool"}, "bool"},
	}
	for _, tt := range tests {
		got := vectorHelperSuffix(tt.elem)
		if got != tt.want {
			t.Errorf("vectorHelperSuffix(%q) = %q, want %q", tt.elem.Builtin, got, tt.want)
		}
	}
}

func TestCollectListElemTypes(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{
			{
				Name: "send",
				Kind: FuncFree,
				Params: []Param{
					{Name: "data", Type: TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u8"}}},
				},
				ImportName: "send",
			},
			{
				Name: "get_ids",
				Kind: FuncFree,
				Results: []TypeRef{{
					Kind: ResultKind,
					Ok:   &TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "s32"}},
				}},
				ImportName: "get-ids",
			},
			{
				Name: "dup",
				Kind: FuncFree,
				Params: []Param{
					// Duplicate u8 — should deduplicate
					{Name: "more", Type: TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u8"}}},
				},
				ImportName: "dup",
			},
		},
	}}
	got := collectListElemTypes(modules)
	if len(got) != 2 {
		t.Fatalf("collectListElemTypes: got %d types, want 2", len(got))
	}
	// Should have u8 and s32 (deduplicated)
	names := map[string]bool{}
	for _, elem := range got {
		names[promiseType(elem)] = true
	}
	if !names["u8"] {
		t.Error("missing u8 in collected list elem types")
	}
	if !names["i32"] {
		t.Error("missing i32 (from s32) in collected list elem types")
	}
}

func TestCollectListElemTypesNested(t *testing.T) {
	// list<u8> inside option<list<u8>> — should still find u8
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name: "maybe_data",
			Kind: FuncFree,
			Params: []Param{{
				Name: "data",
				Type: TypeRef{
					Kind: OptionKind,
					Elem: &TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u8"}},
				},
			}},
			ImportName: "maybe-data",
		}},
	}}
	got := collectListElemTypes(modules)
	if len(got) != 1 || promiseType(got[0]) != "u8" {
		t.Errorf("collectListElemTypes(option<list<u8>>): got %v", got)
	}
}

func TestCollectListElemTypesResource(t *testing.T) {
	// list param on a resource method
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Resources: []Resource{{
			Name: "Stream",
			Methods: []Func{{
				Name: "write",
				Kind: FuncMethod,
				Params: []Param{{
					Name: "buf",
					Type: TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u8"}},
				}},
				ImportName: "[method]stream.write",
			}},
		}},
	}}
	got := collectListElemTypes(modules)
	if len(got) != 1 || promiseType(got[0]) != "u8" {
		t.Errorf("collectListElemTypes(resource method): got %v", got)
	}
}

func TestCollectListElemTypesTuple(t *testing.T) {
	// list<s32> inside tuple<string, list<s32>> — should find s32
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name: "pair",
			Kind: FuncFree,
			Results: []TypeRef{{
				Kind: TupleKind,
				Elements: []TypeRef{
					{Kind: BuiltinKind, Builtin: "string"},
					{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "s32"}},
				},
			}},
			ImportName: "pair",
		}},
	}}
	got := collectListElemTypes(modules)
	if len(got) != 1 || promiseType(got[0]) != "i32" {
		t.Errorf("collectListElemTypes(tuple<string, list<s32>>): got %v", got)
	}
}

func TestLiftReturnFromRetPtrList(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	// Plain list<u8> return
	results := []TypeRef{{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u8"}}}
	got := g.liftReturnFromRetPtr(results)
	want := "_cabi_vector_from_u8(_cabi_load_i32(_cabi_retarea_ptr()), _cabi_load_i32(_cabi_retarea_ptr() + 4), 1)"
	if got != want {
		t.Errorf("liftReturnFromRetPtr(list<u8>) =\n  %q\nwant:\n  %q", got, want)
	}
}

func TestLiftReturnFromRetPtrResultList(t *testing.T) {
	g := &generator{canonicalABI: true, target: "wasi"}
	// result<list<u8>, string>
	results := []TypeRef{{
		Kind: ResultKind,
		Ok:   &TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u8"}},
		Err:  &TypeRef{Kind: BuiltinKind, Builtin: "string"},
	}}
	got := g.liftReturnFromRetPtr(results)
	want := "_cabi_vector_from_u8(_cabi_load_i32(_cabi_retarea_ptr() + 4), _cabi_load_i32(_cabi_retarea_ptr() + 8), 1)"
	if got != want {
		t.Errorf("liftReturnFromRetPtr(result<list<u8>, string>) =\n  %q\nwant:\n  %q", got, want)
	}
}

func TestCodegenCanonicalABIListParam(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name: "send_data",
			Kind: FuncFree,
			Params: []Param{{
				Name: "data",
				Type: TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u8"}},
			}},
			ImportName: "send-data",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// Vector helper declarations
	assertContains(t, out, `_cabi_vector_data_u8(u8[] v) i32 `+"`"+`extern("cabi_vector_data")`)
	assertContains(t, out, `_cabi_vector_len_u8(u8[] v) i32 `+"`"+`extern("cabi_vector_len")`)
	assertContains(t, out, `_cabi_vector_from_u8(i32 ptr, i32 len, i32 elem_size) u8[] `+"`"+`extern("cabi_vector_from")`)
	// Wrapper lowers list param
	assertContains(t, out, "_cabi_vector_data_u8(data), _cabi_vector_len_u8(data)")
}

func TestCodegenCanonicalABIListReturn(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "test:test/api",
		Functions: []Func{{
			Name: "get_data",
			Kind: FuncFree,
			Results: []TypeRef{{
				Kind: ResultKind,
				Ok:   &TypeRef{Kind: ListKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "u8"}},
				Err:  &TypeRef{Kind: BuiltinKind, Builtin: "string"},
			}},
			ImportName: "get-data",
		}},
	}}
	out := GeneratePromiseWithOptions(modules, "wasi", true)
	// Lifts list from retptr
	assertContains(t, out, "_cabi_vector_from_u8(_cabi_load_i32(_cabi_retarea_ptr() + 4), _cabi_load_i32(_cabi_retarea_ptr() + 8), 1)")
	// Error extraction
	assertContains(t, out, "raise error(_cabi_string_from(")
}

// --- WebIDL name conversion tests ---

func TestIdlToSnake(t *testing.T) {
	tests := []struct{ in, want string }{
		{"getElementById", "get_element_by_id"},
		{"setAttribute", "set_attribute"},
		{"innerHTML", "inner_html"},
		{"tagName", "tag_name"},
		{"URL", "url"},
		{"simple", "simple"},
	}
	for _, tt := range tests {
		got := idlToSnake(tt.in)
		if got != tt.want {
			t.Errorf("idlToSnake(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIdlEnumValueToPascal(t *testing.T) {
	tests := []struct{ in, want string }{
		{"read-write", "ReadWrite"},
		{"auto", "Auto"},
		{"same-origin", "SameOrigin"},
		{"no-cors", "NoCors"},
	}
	for _, tt := range tests {
		got := idlEnumValueToPascal(tt.in)
		if got != tt.want {
			t.Errorf("idlEnumValueToPascal(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIdlCamelCase(t *testing.T) {
	tests := []struct{ in, want string }{
		{"get_element_by_id", "getElementById"},
		{"set_attribute", "setAttribute"},
		{"simple", "simple"},
	}
	for _, tt := range tests {
		got := idlCamelCase(tt.in)
		if got != tt.want {
			t.Errorf("idlCamelCase(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIdlEnumValueToPascalEmpty(t *testing.T) {
	// Empty string after splitting → "Empty"
	got := idlEnumValueToPascal("")
	if got != "Empty" {
		t.Errorf("idlEnumValueToPascal(\"\") = %q, want \"Empty\"", got)
	}
	// Separator-only string → "Empty"
	got = idlEnumValueToPascal("--")
	if got != "Empty" {
		t.Errorf("idlEnumValueToPascal(\"--\") = %q, want \"Empty\"", got)
	}
}

func TestIdlToSnakeEmpty(t *testing.T) {
	got := idlToSnake("")
	if got != "" {
		t.Errorf("idlToSnake(\"\") = %q, want \"\"", got)
	}
}

func TestConvertOperationStatic(t *testing.T) {
	src := `interface Foo {
		static DOMString create(DOMString name);
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	found := false
	for _, r := range modules[0].Resources {
		for _, m := range r.Methods {
			if m.Name == "create" {
				if m.Kind != FuncStatic {
					t.Errorf("expected FuncStatic, got %d", m.Kind)
				}
				found = true
			}
		}
	}
	if !found {
		t.Error("static method 'create' not found")
	}
}

func TestConvertOperationSpecialNoName(t *testing.T) {
	src := `interface Collection {
		getter DOMString (unsigned long index);
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	found := false
	for _, r := range modules[0].Resources {
		for _, m := range r.Methods {
			if m.Name == "getter" {
				found = true
			}
		}
	}
	if !found {
		t.Error("unnamed getter operation should use special name 'getter'")
	}
}

// --- WebIDL-to-IR conversion tests ---

func TestWebIdlToIRInterface(t *testing.T) {
	src := `interface Element {
		readonly attribute DOMString tagName;
		Element? querySelector(DOMString selectors);
		void setAttribute(DOMString name, DOMString value);
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	if len(modules) != 1 {
		t.Fatalf("expected 1 module, got %d", len(modules))
	}
	m := modules[0]
	if m.ImportModule != "promise_env" {
		t.Errorf("expected import module 'promise_env', got %q", m.ImportModule)
	}
	if len(m.Resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(m.Resources))
	}
	res := m.Resources[0]
	if res.Name != "Element" {
		t.Errorf("expected resource name 'Element', got %q", res.Name)
	}
	if !res.Drop {
		t.Error("expected Drop=true")
	}
	// Readonly attribute → 1 getter. Operation → 1 method each. = 3 methods
	if len(res.Methods) != 3 {
		t.Errorf("expected 3 methods, got %d", len(res.Methods))
	}
	// Check getter
	getter := res.Methods[0]
	if getter.Name != "tag_name" {
		t.Errorf("expected getter 'tag_name', got %q", getter.Name)
	}
	if getter.Kind != FuncMethod {
		t.Errorf("expected FuncMethod kind, got %d", getter.Kind)
	}
	if len(getter.Results) != 1 || getter.Results[0].Builtin != "string" {
		t.Errorf("expected string result for getter")
	}
}

func TestWebIdlToIRDictionary(t *testing.T) {
	src := `dictionary RequestInit {
		required DOMString method;
		DOMString body;
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	m := modules[0]
	if len(m.Types) != 1 {
		t.Fatalf("expected 1 type, got %d", len(m.Types))
	}
	typ := m.Types[0]
	if typ.Name != "RequestInit" {
		t.Errorf("expected 'RequestInit', got %q", typ.Name)
	}
	if typ.Kind != TypeRecord {
		t.Errorf("expected TypeRecord, got %d", typ.Kind)
	}
	if len(typ.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(typ.Fields))
	}
	// First field: required → direct type
	if typ.Fields[0].Type.Kind != BuiltinKind {
		t.Errorf("expected required field to be BuiltinKind, got %d", typ.Fields[0].Type.Kind)
	}
	// Second field: optional → OptionKind wrapping
	if typ.Fields[1].Type.Kind != OptionKind {
		t.Errorf("expected optional field to be OptionKind, got %d", typ.Fields[1].Type.Kind)
	}
}

func TestWebIdlToIREnum(t *testing.T) {
	src := `enum ScrollBehavior {
		"auto",
		"instant",
		"smooth"
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	m := modules[0]
	if len(m.Types) != 1 {
		t.Fatalf("expected 1 type, got %d", len(m.Types))
	}
	typ := m.Types[0]
	if typ.Kind != TypeEnum {
		t.Errorf("expected TypeEnum, got %d", typ.Kind)
	}
	if len(typ.Cases) != 3 {
		t.Fatalf("expected 3 cases, got %d", len(typ.Cases))
	}
	if typ.Cases[0].Name != "Auto" {
		t.Errorf("expected 'Auto', got %q", typ.Cases[0].Name)
	}
}

func TestWebIdlToIRConstructor(t *testing.T) {
	src := `interface Image {
		constructor(unsigned long width, unsigned long height);
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	res := modules[0].Resources[0]
	if len(res.Methods) != 1 {
		t.Fatalf("expected 1 method, got %d", len(res.Methods))
	}
	ctor := res.Methods[0]
	if ctor.Kind != FuncConstructor {
		t.Errorf("expected FuncConstructor, got %d", ctor.Kind)
	}
	if ctor.Name != "create" {
		t.Errorf("expected 'create', got %q", ctor.Name)
	}
	if len(ctor.Params) != 2 {
		t.Errorf("expected 2 params, got %d", len(ctor.Params))
	}
}

func TestWebIdlToIRTypeMapping(t *testing.T) {
	// Test that WebIDL types map to the correct IR types
	tests := []struct {
		idlType  string
		expected TypeRef
	}{
		{"DOMString", TypeRef{Kind: BuiltinKind, Builtin: "string"}},
		{"boolean", TypeRef{Kind: BuiltinKind, Builtin: "bool"}},
		{"long", TypeRef{Kind: BuiltinKind, Builtin: "s32"}},
		{"unsigned long", TypeRef{Kind: BuiltinKind, Builtin: "u32"}},
		{"double", TypeRef{Kind: BuiltinKind, Builtin: "f64"}},
		{"float", TypeRef{Kind: BuiltinKind, Builtin: "f32"}},
		{"byte", TypeRef{Kind: BuiltinKind, Builtin: "s8"}},
		{"octet", TypeRef{Kind: BuiltinKind, Builtin: "u8"}},
		{"any", TypeRef{Kind: NamedKind, Name: "JsValue"}},
	}
	for _, tt := range tests {
		got := convertWebIdlBuiltin(tt.idlType)
		if got.Kind != tt.expected.Kind {
			t.Errorf("convertWebIdlBuiltin(%q): kind = %d, want %d", tt.idlType, got.Kind, tt.expected.Kind)
		}
		if got.Kind == BuiltinKind && got.Builtin != tt.expected.Builtin {
			t.Errorf("convertWebIdlBuiltin(%q): builtin = %q, want %q", tt.idlType, got.Builtin, tt.expected.Builtin)
		}
		if got.Kind == NamedKind && got.Name != tt.expected.Name {
			t.Errorf("convertWebIdlBuiltin(%q): name = %q, want %q", tt.idlType, got.Name, tt.expected.Name)
		}
	}
}

func TestWebIdlToIRReadWriteAttribute(t *testing.T) {
	src := `interface Foo {
		attribute DOMString name;
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	res := modules[0].Resources[0]
	// Read-write attribute should produce getter + setter = 2 methods
	if len(res.Methods) != 2 {
		t.Fatalf("expected 2 methods for read-write attribute, got %d", len(res.Methods))
	}
	if res.Methods[0].Name != "name" {
		t.Errorf("expected getter 'name', got %q", res.Methods[0].Name)
	}
	if res.Methods[1].Name != "set_name" {
		t.Errorf("expected setter 'set_name', got %q", res.Methods[1].Name)
	}
}

func TestWebIdlToIRNullableParam(t *testing.T) {
	src := `interface Foo {
		void bar(DOMString? name);
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	method := modules[0].Resources[0].Methods[0]
	if method.Params[0].Type.Kind != OptionKind {
		t.Errorf("expected OptionKind for nullable param, got %d", method.Params[0].Type.Kind)
	}
}

func TestWebIdlToIRMergedPartials(t *testing.T) {
	src := `interface Foo {
		readonly attribute DOMString name;
	};
	partial interface Foo {
		void doStuff();
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	webidl.Merge(file)
	modules := WebIdlToIR(file)
	res := modules[0].Resources[0]
	// 1 getter (name) + 1 operation (doStuff) = 2 methods
	if len(res.Methods) != 2 {
		t.Errorf("expected 2 methods after merge, got %d", len(res.Methods))
	}
}

// --- JS glue generation tests ---

func TestGenerateJSGlueRefTable(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, "function _refStore(obj)")
	assertContains(t, out, "function _refLoad(handle)")
	assertContains(t, out, "function _refRelease(handle)")
}

func TestGenerateJSGlueStringHelpers(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, "function _readString(ptr, len)")
	assertContains(t, out, "function _writeString(str)")
	assertContains(t, out, "TextEncoder")
	assertContains(t, out, "TextDecoder")
}

func TestGenerateJSGlueResourceDrop(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Element",
			Drop: true,
		}},
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, `"Element.drop"(handle)`)
	assertContains(t, out, "_refRelease(handle)")
}

func TestGenerateJSGlueMethodImport(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Element",
			Drop: true,
			Methods: []Func{{
				Name:       "set_attribute",
				Kind:       FuncMethod,
				OwnerType:  "Element",
				ImportName: "Element.setAttribute",
				Params: []Param{
					{Name: "name", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}},
					{Name: "value", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}},
				},
			}},
		}},
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, `"Element.setAttribute"`)
	assertContains(t, out, "_readString(name_ptr, name_len)")
	assertContains(t, out, "_refLoad(handle)")
}

func TestGenerateJSGlueConstructor(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Image",
			Drop: true,
			Methods: []Func{{
				Name:       "create",
				Kind:       FuncConstructor,
				OwnerType:  "Image",
				ImportName: "Image.constructor",
				Params: []Param{
					{Name: "width", Type: TypeRef{Kind: BuiltinKind, Builtin: "u32"}},
				},
			}},
		}},
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, "new Image(width)")
	assertContains(t, out, "_refStore(")
}

func TestGenerateJSGlueInstantiation(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, "WebAssembly.instantiateStreaming")
	assertContains(t, out, "export async function init(wasmPath)")
	assertContains(t, out, "_initialize")
}

func TestWebIdlEndToEnd(t *testing.T) {
	// Full pipeline: parse WebIDL → IR → Promise code + JS glue
	src := `
	interface Console {
		undefined log(DOMString message);
	};

	interface Document {
		Element? getElementById(DOMString elementId);
		Element createElement(DOMString localName);
		readonly attribute DOMString title;
	};

	enum ScrollBehavior {
		"auto",
		"smooth"
	};
	`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	webidl.Merge(file)
	modules := WebIdlToIR(file)

	// Generate Promise code
	prCode := GeneratePromise(modules, "web")
	if !strings.Contains(prCode, "Console") {
		t.Error("Promise code should contain Console resource")
	}
	if !strings.Contains(prCode, "Document") {
		t.Error("Promise code should contain Document resource")
	}
	if !strings.Contains(prCode, "ScrollBehavior") {
		t.Error("Promise code should contain ScrollBehavior enum")
	}
	if !strings.Contains(prCode, "`target(web)") {
		t.Error("Promise code should contain web target annotation")
	}

	// Generate JS glue
	jsCode := GenerateJSGlue(modules)
	if !strings.Contains(jsCode, "promise_env") {
		t.Error("JS glue should reference promise_env import module")
	}
	if !strings.Contains(jsCode, "Console.drop") {
		t.Error("JS glue should contain Console.drop")
	}
	if !strings.Contains(jsCode, "Document.drop") {
		t.Error("JS glue should contain Document.drop")
	}
}

// --- Coverage gap tests: WebIDL-to-IR ---

func TestWebIdlToIRTypedef(t *testing.T) {
	src := `typedef unsigned long long DOMTimeStamp;`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	m := modules[0]
	if len(m.Types) != 1 {
		t.Fatalf("expected 1 type, got %d", len(m.Types))
	}
	typ := m.Types[0]
	if typ.Kind != TypeAlias {
		t.Errorf("expected TypeAlias, got %d", typ.Kind)
	}
	if typ.Name != "DOMTimeStamp" {
		t.Errorf("expected 'DOMTimeStamp', got %q", typ.Name)
	}
	if typ.Target == nil {
		t.Fatal("expected non-nil target")
	}
	if typ.Target.Builtin != "u64" {
		t.Errorf("expected target u64, got %q", typ.Target.Builtin)
	}
}

func TestWebIdlToIRConstMember(t *testing.T) {
	src := `interface Node {
		const unsigned short ELEMENT_NODE = 1;
		const unsigned short TEXT_NODE = 3;
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	res := modules[0].Resources[0]
	if len(res.Methods) != 2 {
		t.Fatalf("expected 2 methods from consts, got %d", len(res.Methods))
	}
	f := res.Methods[0]
	if f.Kind != FuncStatic {
		t.Errorf("expected FuncStatic for const, got %d", f.Kind)
	}
	if f.Name != "element_node" {
		t.Errorf("expected 'element_node', got %q", f.Name)
	}
}

func TestWebIdlToIRSpecialOperations(t *testing.T) {
	src := `interface Storage {
		getter DOMString getItem(DOMString key);
		setter void setItem(DOMString key, DOMString value);
		deleter void removeItem(DOMString key);
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	res := modules[0].Resources[0]
	if len(res.Methods) != 3 {
		t.Fatalf("expected 3 methods, got %d", len(res.Methods))
	}
}

func TestWebIdlToIRStaticOperation(t *testing.T) {
	src := `interface Crypto {
		static DOMString randomUUID();
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	res := modules[0].Resources[0]
	if len(res.Methods) != 1 {
		t.Fatalf("expected 1 method, got %d", len(res.Methods))
	}
	f := res.Methods[0]
	if f.Kind != FuncStatic {
		t.Errorf("expected FuncStatic, got %d", f.Kind)
	}
}

func TestWebIdlToIROptionalConstructorParam(t *testing.T) {
	src := `interface Foo {
		constructor(optional DOMString name);
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	ctor := modules[0].Resources[0].Methods[0]
	if ctor.Params[0].Type.Kind != OptionKind {
		t.Errorf("expected OptionKind for optional ctor param, got %d", ctor.Params[0].Type.Kind)
	}
}

func TestWebIdlToIRMixinSkipped(t *testing.T) {
	// Mixin-tagged interfaces should be skipped as resources
	src := `interface mixin Mix {
		readonly attribute DOMString a;
	};
	interface Real {
		readonly attribute long x;
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	m := modules[0]
	// Only "Real" should appear as a resource, Mix is skipped
	if len(m.Resources) != 1 {
		t.Fatalf("expected 1 resource (mixin skipped), got %d", len(m.Resources))
	}
	if m.Resources[0].Name != "Real" {
		t.Errorf("expected 'Real', got %q", m.Resources[0].Name)
	}
	// Module name should come from first non-mixin interface
	if m.Name != "real" {
		t.Errorf("expected module name 'real', got %q", m.Name)
	}
}

func TestConvertWebIdlTypeRefComprehensive(t *testing.T) {
	tests := []struct {
		name   string
		ref    *webidl.TypeRef
		expect TypeRef
	}{
		{
			name:   "nil ref",
			ref:    nil,
			expect: TypeRef{Kind: BuiltinKind, Builtin: "void"},
		},
		{
			name:   "promise type",
			ref:    &webidl.TypeRef{Kind: webidl.PromiseType, Elem: &webidl.TypeRef{Kind: webidl.NamedType, Name: "Response"}},
			expect: TypeRef{Kind: NamedKind, Name: "JsValue"},
		},
		{
			name:   "record type",
			ref:    &webidl.TypeRef{Kind: webidl.RecordType, Key: &webidl.TypeRef{Kind: webidl.BuiltinType, Builtin: "DOMString"}, Value: &webidl.TypeRef{Kind: webidl.BuiltinType, Builtin: "long"}},
			expect: TypeRef{Kind: NamedKind, Name: "JsValue"},
		},
		{
			name:   "union type",
			ref:    &webidl.TypeRef{Kind: webidl.UnionType, Members: []*webidl.TypeRef{{Kind: webidl.BuiltinType, Builtin: "DOMString"}, {Kind: webidl.BuiltinType, Builtin: "long"}}},
			expect: TypeRef{Kind: NamedKind, Name: "JsValue"},
		},
		{
			name:   "observable array",
			ref:    &webidl.TypeRef{Kind: webidl.ObservableArrayType, Elem: &webidl.TypeRef{Kind: webidl.BuiltinType, Builtin: "long"}},
			expect: TypeRef{Kind: ListKind},
		},
		{
			name:   "frozen array",
			ref:    &webidl.TypeRef{Kind: webidl.FrozenArrayType, Elem: &webidl.TypeRef{Kind: webidl.BuiltinType, Builtin: "DOMString"}},
			expect: TypeRef{Kind: ListKind},
		},
		{
			name:   "nullable named",
			ref:    &webidl.TypeRef{Kind: webidl.NamedType, Name: "Element", Nullable: true},
			expect: TypeRef{Kind: OptionKind},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertWebIdlTypeRef(tt.ref)
			if got.Kind != tt.expect.Kind {
				t.Errorf("kind = %d, want %d", got.Kind, tt.expect.Kind)
			}
			if tt.expect.Builtin != "" && got.Builtin != tt.expect.Builtin {
				t.Errorf("builtin = %q, want %q", got.Builtin, tt.expect.Builtin)
			}
			if tt.expect.Name != "" && got.Name != tt.expect.Name {
				t.Errorf("name = %q, want %q", got.Name, tt.expect.Name)
			}
		})
	}
}

func TestConvertWebIdlBuiltinComprehensive(t *testing.T) {
	tests := []struct {
		builtin string
		kind    TypeRefKind
		value   string // Builtin or Name depending on kind
	}{
		{"short", BuiltinKind, "s16"},
		{"unsigned short", BuiltinKind, "u16"},
		{"long long", BuiltinKind, "s64"},
		{"unsigned long long", BuiltinKind, "u64"},
		{"unrestricted float", BuiltinKind, "f32"},
		{"unrestricted double", BuiltinKind, "f64"},
		{"void", BuiltinKind, "void"},
		{"undefined", BuiltinKind, "void"},
		{"object", NamedKind, "JsValue"},
		{"bigint", BuiltinKind, "s64"},
		{"symbol", NamedKind, "JsValue"},
		{"ArrayBuffer", NamedKind, "JsValue"},
		{"DataView", NamedKind, "JsValue"},
		{"Int8Array", NamedKind, "JsValue"},
		{"Int16Array", NamedKind, "JsValue"},
		{"Int32Array", NamedKind, "JsValue"},
		{"Uint8Array", NamedKind, "JsValue"},
		{"Uint16Array", NamedKind, "JsValue"},
		{"Uint32Array", NamedKind, "JsValue"},
		{"Uint8ClampedArray", NamedKind, "JsValue"},
		{"Float32Array", NamedKind, "JsValue"},
		{"Float64Array", NamedKind, "JsValue"},
		{"unknown_type", BuiltinKind, "u32"},
	}
	for _, tt := range tests {
		t.Run(tt.builtin, func(t *testing.T) {
			got := convertWebIdlBuiltin(tt.builtin)
			if got.Kind != tt.kind {
				t.Errorf("kind = %d, want %d", got.Kind, tt.kind)
			}
			if tt.kind == BuiltinKind && got.Builtin != tt.value {
				t.Errorf("builtin = %q, want %q", got.Builtin, tt.value)
			}
			if tt.kind == NamedKind && got.Name != tt.value {
				t.Errorf("name = %q, want %q", got.Name, tt.value)
			}
		})
	}
}

// --- Coverage gap tests: JS Glue ---

func TestGenerateJSGlueStaticMethod(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Math",
			Drop: true,
			Methods: []Func{{
				Name:       "random",
				Kind:       FuncStatic,
				OwnerType:  "Math",
				ImportName: "Math.random",
				Results:    []TypeRef{{Kind: BuiltinKind, Builtin: "f64"}},
			}},
		}},
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, `"Math.random"`)
	assertContains(t, out, "Math.random()")
}

func TestGenerateJSGlueFreeFunc(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Functions: []Func{{
			Name:       "alert",
			Kind:       FuncFree,
			ImportName: "alert",
			Params:     []Param{{Name: "message", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
		}},
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, `"alert"`)
	assertContains(t, out, "message_ptr")
	assertContains(t, out, "_readString")
}

func TestGenerateJSGlueRefReturnType(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Document",
			Drop: true,
			Methods: []Func{{
				Name:       "create_element",
				Kind:       FuncMethod,
				OwnerType:  "Document",
				ImportName: "Document.createElement",
				Params:     []Param{{Name: "tag", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
				Results:    []TypeRef{{Kind: NamedKind, Name: "Element"}},
			}},
		}},
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, "_refStore(")
}

func TestGenerateJSGlueAttributeSetter(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Element",
			Drop: true,
			Methods: []Func{
				{
					Name:       "text_content",
					Kind:       FuncMethod,
					OwnerType:  "Element",
					ImportName: "Element.textContent.get",
					Results:    []TypeRef{{Kind: BuiltinKind, Builtin: "string"}},
				},
				{
					Name:       "set_text_content",
					Kind:       FuncMethod,
					OwnerType:  "Element",
					ImportName: "Element.textContent.set",
					Params:     []Param{{Name: "value", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
				},
			},
		}},
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, "textContent.get")
	assertContains(t, out, "textContent.set")
	assertContains(t, out, "_refLoad(handle).textContent")
}

func TestGenerateJSGlueRefParam(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Node",
			Drop: true,
			Methods: []Func{{
				Name:       "append_child",
				Kind:       FuncMethod,
				OwnerType:  "Node",
				ImportName: "Node.appendChild",
				Params:     []Param{{Name: "child", Type: TypeRef{Kind: NamedKind, Name: "Node"}}},
				Results:    []TypeRef{{Kind: NamedKind, Name: "Node"}},
			}},
		}},
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, "_refLoad(child)")
	assertContains(t, out, "_refStore(")
}

func TestJsParamNameReservedWords(t *testing.T) {
	reserved := []string{"default", "class", "function", "var", "let", "const", "this", "new", "delete", "in", "typeof"}
	for _, word := range reserved {
		got := jsParamName(word)
		if got != word+"_" {
			t.Errorf("jsParamName(%q) = %q, want %q", word, got, word+"_")
		}
	}
	// Non-reserved should pass through
	if got := jsParamName("value"); got != "value" {
		t.Errorf("jsParamName(\"value\") = %q, want \"value\"", got)
	}
}

func TestGenerateJSGlueVoidReturnMethod(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Console",
			Drop: true,
			Methods: []Func{{
				Name:       "log",
				Kind:       FuncMethod,
				OwnerType:  "Console",
				ImportName: "Console.log",
				Params:     []Param{{Name: "msg", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
				// No results (void)
			}},
		}},
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, `"Console.log"`)
	assertContains(t, out, "_refLoad(handle).log(")
}

func TestGenerateJSGlueOptionalParam(t *testing.T) {
	elem := TypeRef{Kind: BuiltinKind, Builtin: "s32"}
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Foo",
			Drop: true,
			Methods: []Func{{
				Name:       "bar",
				Kind:       FuncMethod,
				OwnerType:  "Foo",
				ImportName: "Foo.bar",
				Params:     []Param{{Name: "x", Type: TypeRef{Kind: OptionKind, Elem: &elem}}},
			}},
		}},
	}}
	out := GenerateJSGlue(modules)
	// Optional s32 should unwrap to scalar param "x"
	assertContains(t, out, `"Foo.bar"(handle, x)`)
}

func TestGenerateJSGlueStringReturn(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Element",
			Drop: true,
			Methods: []Func{{
				Name:       "get_tag",
				Kind:       FuncMethod,
				OwnerType:  "Element",
				ImportName: "Element.getTag",
				Results:    []TypeRef{{Kind: BuiltinKind, Builtin: "string"}},
			}},
		}},
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, "_writeString(String(result))")
	assertContains(t, out, "cabi_retarea()")
}

func TestGenerateJSGlueMethodNoImportName(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Foo",
			Drop: true,
			Methods: []Func{{
				Name:      "bar",
				Kind:      FuncMethod,
				OwnerType: "Foo",
				// ImportName intentionally empty
			}},
		}},
	}}
	out := GenerateJSGlue(modules)
	// Should fall back to "Foo.bar" as import name
	assertContains(t, out, `"Foo.bar"`)
}

func TestGenerateJSGlueNoDrop(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "ReadOnly",
			Drop: false, // no drop function
		}},
	}}
	out := GenerateJSGlue(modules)
	if strings.Contains(out, "ReadOnly.drop") {
		t.Error("should not emit drop for Drop=false resource")
	}
}

func TestWebIdlToIRVoidReturn(t *testing.T) {
	src := `interface Foo {
		void doStuff();
		undefined doMore();
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	res := modules[0].Resources[0]
	for _, m := range res.Methods {
		if len(m.Results) != 0 {
			t.Errorf("method %q should have no results for void/undefined return, got %d", m.Name, len(m.Results))
		}
	}
}

func TestWebIdlToIRStaticAttribute(t *testing.T) {
	src := `interface Foo {
		static attribute DOMString name;
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	res := modules[0].Resources[0]
	// static read-write attribute → getter + setter, both FuncStatic
	if len(res.Methods) != 2 {
		t.Fatalf("expected 2 methods, got %d", len(res.Methods))
	}
	if res.Methods[0].Kind != FuncStatic {
		t.Errorf("expected static getter")
	}
	if res.Methods[1].Kind != FuncStatic {
		t.Errorf("expected static setter")
	}
}

func TestWebIdlToIRSequenceReturn(t *testing.T) {
	src := `interface Foo {
		sequence<DOMString> getNames();
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	res := modules[0].Resources[0]
	if len(res.Methods[0].Results) != 1 {
		t.Fatal("expected 1 result")
	}
	if res.Methods[0].Results[0].Kind != ListKind {
		t.Errorf("expected ListKind, got %d", res.Methods[0].Results[0].Kind)
	}
}

func TestWebIdlToIRNoInterfaces(t *testing.T) {
	// Module name should default to "web" when no non-mixin interfaces
	src := `enum Foo { "a", "b" };`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	modules := WebIdlToIR(file)
	if modules[0].Name != "web" {
		t.Errorf("expected default module name 'web', got %q", modules[0].Name)
	}
}

func TestWebIdlToIRMergedPartialDictionary(t *testing.T) {
	src := `dictionary Options {
		required DOMString name;
	};
	partial dictionary Options {
		long timeout;
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	webidl.Merge(file)
	modules := WebIdlToIR(file)
	m := modules[0]
	if len(m.Types) != 1 {
		t.Fatalf("expected 1 type, got %d", len(m.Types))
	}
	rec := m.Types[0]
	if rec.Kind != TypeRecord {
		t.Fatalf("expected TypeRecord, got %d", rec.Kind)
	}
	// name (required) + timeout (optional) = 2 fields
	if len(rec.Fields) != 2 {
		t.Fatalf("expected 2 fields after partial merge, got %d", len(rec.Fields))
	}
	if rec.Fields[0].Name != "name" {
		t.Errorf("expected field 'name', got %q", rec.Fields[0].Name)
	}
	if rec.Fields[1].Name != "timeout" {
		t.Errorf("expected field 'timeout', got %q", rec.Fields[1].Name)
	}
	// timeout is not required, so should be wrapped in OptionKind
	if rec.Fields[1].Type.Kind != OptionKind {
		t.Errorf("expected OptionKind for optional field, got %d", rec.Fields[1].Type.Kind)
	}
}

func TestJsValueEmission(t *testing.T) {
	// WebIDL with types that map to JsValue (any, union, Promise, record)
	src := `interface Foo {
		any getValue();
		void doStuff((DOMString or long) input);
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	webidl.Merge(file)
	modules := WebIdlToIR(file)
	if !modules[0].HasJsValue {
		t.Fatal("expected HasJsValue=true when types reference JsValue")
	}
	code := GeneratePromise(modules, "web")
	assertContains(t, code, "enum JsValue")
	assertContains(t, code, "Undefined,")
	assertContains(t, code, "Null,")
	assertContains(t, code, "Bool(bool value),")
	assertContains(t, code, "Number(f64 value),")
	assertContains(t, code, "Str(string value),")
	assertContains(t, code, "Object(int _js_ref),")
	assertContains(t, code, "Array(int _js_ref),")
	assertContains(t, code, "Function(int _js_ref),")
	assertContains(t, code, "get is_undefined bool")
	assertContains(t, code, "get is_null bool")
	assertContains(t, code, "as_bool(this) bool?")
	assertContains(t, code, "as_number(this) f64?")
	assertContains(t, code, "as_string(this) string?")
}

func TestJsValueDetectedInDictField(t *testing.T) {
	// JsValue detected via dictionary field of type 'any'
	src := `dictionary Options {
		any value;
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	webidl.Merge(file)
	modules := WebIdlToIR(file)
	if !modules[0].HasJsValue {
		t.Fatal("expected HasJsValue=true when dictionary field is 'any'")
	}
	code := GeneratePromise(modules, "web")
	assertContains(t, code, "enum JsValue")
}

func TestJsValueDetectedInTypedef(t *testing.T) {
	// JsValue detected via typedef target
	src := `typedef any JsRef;`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	webidl.Merge(file)
	modules := WebIdlToIR(file)
	if !modules[0].HasJsValue {
		t.Fatal("expected HasJsValue=true when typedef target is 'any'")
	}
}

func TestJsValueDetectedNested(t *testing.T) {
	// JsValue detected inside sequence<any> (nested Elem)
	src := `dictionary Batch {
		sequence<any> items;
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	webidl.Merge(file)
	modules := WebIdlToIR(file)
	if !modules[0].HasJsValue {
		t.Fatal("expected HasJsValue=true for sequence<any> in dict field")
	}
}

func TestJsValueNotEmittedWhenUnreferenced(t *testing.T) {
	// WebIDL with no types mapping to JsValue
	src := `interface Foo {
		DOMString getName();
		void setCount(long count);
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	webidl.Merge(file)
	modules := WebIdlToIR(file)
	if modules[0].HasJsValue {
		t.Fatal("expected HasJsValue=false when no types reference JsValue")
	}
	code := GeneratePromise(modules, "web")
	if strings.Contains(code, "enum JsValue") {
		t.Error("JsValue enum should not be emitted when unreferenced")
	}
}

// --- Coverage gap tests ---

func TestCodegenStaticWrapperResourceReturn(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{
			{Name: "File", Drop: true},
			{Name: "Dir", Drop: true, Methods: []Func{{
				Name:       "open_file",
				Kind:       FuncStatic,
				Params:     []Param{{Name: "path", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
				Results:    []TypeRef{{Kind: NamedKind, Name: "File"}},
				ImportName: "[static]dir.open-file",
			}}},
		},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "open_file(string path) File `public `global {")
	assertContains(t, out, "handle := _dir_open_file(path);")
	assertContains(t, out, "return File(_handle: handle);")
}

func TestCodegenStaticWrapperOptionalResourceReturn(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "wasi:fs/types",
		Resources: []Resource{
			{Name: "File", Drop: true},
			{Name: "Dir", Drop: true, Methods: []Func{{
				Name:       "find_file",
				Kind:       FuncStatic,
				Params:     []Param{{Name: "name", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
				Results:    []TypeRef{{Kind: OptionKind, Elem: &TypeRef{Kind: NamedKind, Name: "File"}}},
				ImportName: "[static]dir.find-file",
			}}},
		},
	}}
	out := GeneratePromise(modules, "wasi")
	assertContains(t, out, "find_file(string name) File? `public `global {")
	assertContains(t, out, "handle := _dir_find_file(name);")
	assertContains(t, out, "if handle == 0 { return none; }")
	assertContains(t, out, "return File(_handle: handle);")
}

func TestFormatReturnSig(t *testing.T) {
	g := &generator{}

	// No results → empty
	if got := g.formatReturnSig(nil); got != "" {
		t.Errorf("nil results: got %q, want empty", got)
	}

	// Plain type → " type"
	got := g.formatReturnSig([]TypeRef{{Kind: BuiltinKind, Builtin: "u32"}})
	if got != " u32" {
		t.Errorf("plain type: got %q, want %q", got, " u32")
	}

	// Failable with type → " type!"
	got = g.formatReturnSig([]TypeRef{{Kind: ResultKind, Ok: &TypeRef{Kind: BuiltinKind, Builtin: "string"}}})
	if got != " string!" {
		t.Errorf("failable+type: got %q, want %q", got, " string!")
	}

	// Failable void (Result with no Ok) → "!"
	got = g.formatReturnSig([]TypeRef{{Kind: ResultKind}})
	if got != "!" {
		t.Errorf("failable void: got %q, want %q", got, "!")
	}
}

func TestFormatExternReturnTypeMultiple(t *testing.T) {
	g := &generator{}
	results := []TypeRef{
		{Kind: BuiltinKind, Builtin: "u32"},
		{Kind: BuiltinKind, Builtin: "u64"},
	}
	got := g.formatExternReturnType(results)
	if got != "(u32, u64)" {
		t.Errorf("multi extern return: got %q, want %q", got, "(u32, u64)")
	}
}

func TestGenerateJSGlueFreeFuncNoImportName(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Functions: []Func{{
			Name: "do_thing",
			Kind: FuncFree,
			// ImportName intentionally empty — should fall back to Name
		}},
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, `"do_thing"`)
	assertContains(t, out, "doThing()")
}

func TestGenerateJSGlueNullableStringReturn(t *testing.T) {
	modules := []*Module{{
		Name:         "test",
		ImportModule: "promise_env",
		Resources: []Resource{{
			Name: "Storage",
			Drop: true,
			Methods: []Func{{
				Name:       "get_item",
				Kind:       FuncMethod,
				OwnerType:  "Storage",
				ImportName: "Storage.getItem",
				Params:     []Param{{Name: "key", Type: TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
				Results:    []TypeRef{{Kind: OptionKind, Elem: &TypeRef{Kind: BuiltinKind, Builtin: "string"}}},
			}},
		}},
	}}
	out := GenerateJSGlue(modules)
	assertContains(t, out, "if (result == null) return 0;")
	assertContains(t, out, "_writeString(String(result))")
}

func TestWebIdlOptionalParam(t *testing.T) {
	src := `interface Foo {
		void bar(optional DOMString name);
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	webidl.Merge(file)
	modules := WebIdlToIR(file)
	if len(modules) == 0 || len(modules[0].Resources) == 0 {
		t.Fatal("expected at least one resource")
	}
	// The optional parameter should be wrapped in OptionKind
	method := modules[0].Resources[0].Methods[0]
	if len(method.Params) != 1 {
		t.Fatalf("expected 1 param, got %d", len(method.Params))
	}
	p := method.Params[0]
	if p.Type.Kind != OptionKind {
		t.Errorf("expected optional param to have OptionKind, got %v", p.Type.Kind)
	}
}

func TestJsValueDetectedInFuncParam(t *testing.T) {
	m := &Module{
		Functions: []Func{{
			Name:   "do_stuff",
			Params: []Param{{Name: "val", Type: TypeRef{Kind: NamedKind, Name: "JsValue"}}},
		}},
	}
	if !moduleHasJsValue(m) {
		t.Error("expected HasJsValue=true when function param is JsValue")
	}
}

func TestJsValueDetectedInFuncResult(t *testing.T) {
	m := &Module{
		Functions: []Func{{
			Name:    "get_val",
			Results: []TypeRef{{Kind: NamedKind, Name: "JsValue"}},
		}},
	}
	if !moduleHasJsValue(m) {
		t.Error("expected HasJsValue=true when function result is JsValue")
	}
}

func TestJsValueDetectedInResourceMethodParam(t *testing.T) {
	m := &Module{
		Resources: []Resource{{
			Name: "Foo",
			Methods: []Func{{
				Name:   "set_val",
				Params: []Param{{Name: "v", Type: TypeRef{Kind: NamedKind, Name: "JsValue"}}},
			}},
		}},
	}
	if !moduleHasJsValue(m) {
		t.Error("expected HasJsValue=true when resource method param is JsValue")
	}
}

func TestJsValueDetectedInResourceMethodResult(t *testing.T) {
	m := &Module{
		Resources: []Resource{{
			Name: "Foo",
			Methods: []Func{{
				Name:    "get_val",
				Results: []TypeRef{{Kind: NamedKind, Name: "JsValue"}},
			}},
		}},
	}
	if !moduleHasJsValue(m) {
		t.Error("expected HasJsValue=true when resource method result is JsValue")
	}
}

func TestJsValueDetectedInVariantCase(t *testing.T) {
	m := &Module{
		Types: []Type{{
			Name: "MyVariant",
			Kind: TypeVariant,
			Cases: []Case{{
				Name: "dynamic",
				Type: &TypeRef{Kind: NamedKind, Name: "JsValue"},
			}},
		}},
	}
	if !moduleHasJsValue(m) {
		t.Error("expected HasJsValue=true when variant case type is JsValue")
	}
}

func TestJsValueDetectedInResultOk(t *testing.T) {
	m := &Module{
		Functions: []Func{{
			Name: "try_get",
			Results: []TypeRef{{
				Kind: ResultKind,
				Ok:   &TypeRef{Kind: NamedKind, Name: "JsValue"},
			}},
		}},
	}
	if !moduleHasJsValue(m) {
		t.Error("expected HasJsValue=true when Result.Ok is JsValue")
	}
}

func TestJsValueDetectedInResultErr(t *testing.T) {
	m := &Module{
		Functions: []Func{{
			Name: "try_get",
			Results: []TypeRef{{
				Kind: ResultKind,
				Err:  &TypeRef{Kind: NamedKind, Name: "JsValue"},
			}},
		}},
	}
	if !moduleHasJsValue(m) {
		t.Error("expected HasJsValue=true when Result.Err is JsValue")
	}
}

func TestJsValueDetectedInTupleElements(t *testing.T) {
	m := &Module{
		Functions: []Func{{
			Name: "get_pair",
			Results: []TypeRef{{
				Kind: TupleKind,
				Elements: []TypeRef{
					{Kind: BuiltinKind, Builtin: "u32"},
					{Kind: NamedKind, Name: "JsValue"},
				},
			}},
		}},
	}
	if !moduleHasJsValue(m) {
		t.Error("expected HasJsValue=true when tuple element is JsValue")
	}
}

func TestIdlCamelCaseConsecutiveUnderscores(t *testing.T) {
	got := idlCamelCase("a__b")
	if got != "aB" {
		t.Errorf("idlCamelCase(%q) = %q, want %q", "a__b", got, "aB")
	}
}

// TestWebIdlDOMExceptionGeneratesValidPromise — regression test for T0695.
// The DOMException interface exercises three things that used to produce invalid Promise:
//   - a `constructor(...)` member (used to emit `create(...) ... `public `static`,
//     which is doubly invalid: `static is not a Promise meta, and Promise
//     constructors are named `new` with a `~this` first parameter).
//   - a `const unsigned short` member (used to emit `... `public `static {`).
//   - the interface name `DOMException` itself, which the old non-acronym-aware
//     `toSnake` mangled into `d_o_m_exception_*`.
func TestWebIdlDOMExceptionGeneratesValidPromise(t *testing.T) {
	src := `interface DOMException {
		constructor(optional DOMString message = "", optional DOMString name = "Error");
		const unsigned short INDEX_SIZE_ERR = 1;
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse: %v", errs)
	}
	webidl.Merge(file)
	out := GeneratePromise(WebIdlToIR(file), "web")

	// No `static remains anywhere in the generated source.
	if strings.Contains(out, "`static") {
		t.Errorf("generated source still contains `static:\n%s", out)
	}
	// Constructor uses Promise's new(~this, ...) shape.
	assertContains(t, out, "new(~this, string? message, string? name) `public {")
	assertContains(t, out, "this._handle = _dom_exception_constructor(message, name);")
	// Const member is emitted as a receiver-less `global method.
	assertContains(t, out, "`public `global")
	// Snake-case is acronym-aware: DOMException -> dom_exception, not d_o_m_exception.
	assertContains(t, out, "_dom_exception_constructor")
	if strings.Contains(out, "_d_o_m_exception") {
		t.Errorf("snake_case is not acronym-aware:\n%s", out)
	}
	// The wasm import name preserves the original IDL identifier verbatim —
	// the snake-case change only affects internal Promise aliases.
	assertContains(t, out, "`wasm_import(\"promise_env\", \"DOMException.constructor\")")
}
