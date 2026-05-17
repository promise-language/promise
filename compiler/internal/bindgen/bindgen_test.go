package bindgen

import (
	"fmt"
	"strings"
	"testing"

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
		{"ABC", "a_b_c"},
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
	assertContains(t, out, "get read OpenFlags `public `static {")
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
	assertContains(t, out, "int _handle;")
	assertContains(t, out, "read!(this, u64 length) u8[] `public {")
	assertContains(t, out, "return _descriptor_read(this._handle, length)^;")
	assertContains(t, out, "drop(~this) {")
	assertContains(t, out, "_descriptor_drop(this._handle);")
	assertContains(t, out, "`wasm_import(\"wasi:fs/types\", \"[method]descriptor.read\")")
	assertContains(t, out, "`wasm_import(\"wasi:fs/types\", \"[resource-drop]descriptor\")")
	assertContains(t, out, "`target(wasi);")
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
	assertContains(t, out, "create(string path) Descriptor `public `static {")
	assertContains(t, out, "handle := _descriptor_constructor(path);")
	assertContains(t, out, "return Descriptor(_handle: handle);")
	assertContains(t, out, "`wasm_import(\"wasi:fs/types\", \"[constructor]descriptor\")")
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
	assertContains(t, out, "create!(string path) Descriptor `public `static {")
	assertContains(t, out, "handle := _descriptor_constructor(path)^;")
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
	assertContains(t, out, "open(string path) u32 `public `static {")
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
	assertContains(t, out, "open!(string path) u32 `public `static {")
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
	assertContains(t, out, "reset() `public `static {")
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
