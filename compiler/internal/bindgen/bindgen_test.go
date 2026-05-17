package bindgen

import (
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
	assertContains(t, out, "type Descriptor `public {")
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
	assertContains(t, out, "now() u64 `public {")
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
	assertContains(t, out, "do_thing!() u32 `public {")
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
	assertContains(t, out, "type Descriptor `public {")
	assertContains(t, out, "drop(~this) {")
	assertContains(t, out, "open_at!(string path, OpenFlags flags) Descriptor `public {")
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
