package bindgen

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/wit"
)

// #699: a generated wrapper binds its extern's result to a local (`handle`,
// `ref`, or the canonical-ABI `tag`). An argument of the same name made the
// wrapper redeclare its own parameter, so the module did not compile. Each
// emitter that writes such a local now takes its name from newWrapperLocals,
// which appends "_" while a parameter already has it.
func TestWrapperLocalsAvoidParams(t *testing.T) {
	// webidl generates a module holding interface Y with the one member given.
	webidl := func(member string) func(t *testing.T) string {
		return func(t *testing.T) string {
			src := "interface Node {};\ninterface Y {\n  " + member + "\n};\n"
			return GeneratePromise(overloadIR(t, src), "web")
		}
	}
	// canonicalWit generates the canonical-ABI module of a WIT interface holding
	// the one function given.
	canonicalWit := func(fn string) func(t *testing.T) string {
		return func(t *testing.T) string {
			file, errs := wit.Parse("package test:api;\n\ninterface greet {\n  "+fn+"\n}\n", "test.wit")
			if len(errs) > 0 {
				t.Fatalf("parse errors: %v", errs)
			}
			return GeneratePromiseWithOptions(WitToIR(file), "wasi", true)
		}
	}
	// freeFunc generates a free function `f(i32 ref)` returning result. No front
	// end produces one today — WebIDL fills no Module.Functions and WIT has no
	// JsValue — so the IR is built directly.
	freeFunc := func(result TypeRef) func(t *testing.T) string {
		return func(t *testing.T) string {
			return GeneratePromise([]*Module{{
				Name:         "test",
				ImportModule: "test",
				HasJsValue:   true,
				Functions: []Func{{
					Name:       "f",
					Kind:       FuncFree,
					ImportName: "f",
					Params:     []Param{{Name: "ref", Type: TypeRef{Kind: BuiltinKind, Builtin: "s32"}}},
					Results:    []TypeRef{result},
				}},
			}}, "web")
		}
	}
	jsValue := TypeRef{Kind: NamedKind, Name: "JsValue"}

	cases := []struct {
		name     string
		generate func(t *testing.T) string
		want     []string
	}{
		{
			name:     "constructor",
			generate: webidl("constructor(long handle);"),
			want: []string{
				"handle_ := _y_constructor(handle);",
				"return Y(_handle: handle_, _owned: true);",
			},
		},
		{
			name:     "method returning an interface",
			generate: webidl("Node p(long handle);"),
			want: []string{
				"handle_ := _y_p(this._handle, handle);",
				"return Node(_handle: handle_, _owned: true);",
			},
		},
		{
			name:     "method returning a nullable interface",
			generate: webidl("Node? m(long handle);"),
			want: []string{
				"handle_ := _y_m(this._handle, handle);",
				"if handle_ == 0 { return none; }",
				"return Node(_handle: handle_, _owned: true);",
			},
		},
		{
			name:     "method returning any",
			generate: webidl("any n(long ref);"),
			want: []string{
				"ref_ := _y_n(this._handle, ref);",
				"return JsValue.Object(_js_ref: ref_ as int);",
			},
		},
		{
			name:     "method returning a nullable object",
			generate: webidl("object? o(long ref);"),
			want: []string{
				"ref_ := _y_o(this._handle, ref);",
				"if ref_ == 0 { return none; }",
				"return JsValue.Object(_js_ref: ref_ as int);",
			},
		},
		{
			name:     "static returning an interface",
			generate: webidl("static Node s(long handle);"),
			want: []string{
				"handle_ := _y_s(handle);",
				"return Node(_handle: handle_, _owned: true);",
			},
		},
		{
			name:     "static returning a nullable object",
			generate: webidl("static object? t(long ref);"),
			want: []string{
				"ref_ := _y_t(ref);",
				"if ref_ == 0 { return none; }",
				"return JsValue.Object(_js_ref: ref_ as int);",
			},
		},
		{
			name:     "suffixed name also taken",
			generate: webidl("Node q(long handle, long handle_);"),
			want: []string{
				"handle__ := _y_q(this._handle, handle, handle_);",
				"return Node(_handle: handle__, _owned: true);",
			},
		},
		{
			name:     "canonical result with a value",
			generate: canonicalWit("label: func(tag: u32) -> result<string, string>;"),
			want: []string{
				"tag_ := _cabi_load_i32(_cabi_retarea_ptr());",
				"if tag_ != 0 { raise error(",
			},
		},
		{
			name:     "canonical result without a value",
			generate: canonicalWit("touch: func(tag: u32) -> result<_, string>;"),
			want: []string{
				"tag_ := _cabi_load_i32(_cabi_retarea_ptr());",
				"if tag_ != 0 { raise error(",
			},
		},
		{
			name:     "free function returning JsValue",
			generate: freeFunc(jsValue),
			want: []string{
				"ref_ := _f(ref);",
				"return JsValue.Object(_js_ref: ref_ as int);",
			},
		},
		{
			name:     "free function returning a nullable JsValue",
			generate: freeFunc(TypeRef{Kind: OptionKind, Elem: &jsValue}),
			want: []string{
				"ref_ := _f(ref);",
				"if ref_ == 0 { return none; }",
				"return JsValue.Object(_js_ref: ref_ as int);",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := c.generate(t)
			for _, want := range c.want {
				assertContains(t, out, want)
			}
		})
	}
}
