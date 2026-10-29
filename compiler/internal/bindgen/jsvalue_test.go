package bindgen

import (
	"fmt"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/wasmweb"
)

// TestNamedKinds checks the kind registry classifies every name a module
// defines by what it is, and falls back to namedForeign — today's opaque-handle
// lowering — for a name it does not define (#173). Only a resource and a
// foreign name cross as a `_handle`: every other kind is off the handle path,
// and only JsValue converts through its ref.
func TestNamedKinds(t *testing.T) {
	alias := TypeRef{Kind: BuiltinKind, Builtin: "u64"}
	kinds := namedKinds([]*Module{{
		Name:      "web",
		Resources: []Resource{{Name: "Element"}},
		Types: []Type{
			{Name: "ShadowRootInit", Kind: TypeRecord},
			{Name: "ShadowRootMode", Kind: TypeEnum},
			{Name: "Shape", Kind: TypeVariant},
			{Name: "Mode", Kind: TypeFlags},
			{Name: "Size", Kind: TypeAlias, Target: &alias},
		},
	}})

	for _, c := range []struct {
		name   string
		want   namedKind
		handle bool
	}{
		{"Element", namedResource, true},
		{"ShadowRootInit", namedRecord, false},
		{"ShadowRootMode", namedEnum, false},
		{"Shape", namedVariant, false},
		{"Mode", namedFlags, false},
		// No reference names an alias (typedefs are inlined, WIT aliases by #726).
		{"Size", namedForeign, true},
		{"Unbound", namedForeign, true},
		// The lowering's own name, though no module here sets HasJsValue.
		{"JsValue", namedJsValue, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ref := TypeRef{Kind: NamedKind, Name: c.name}
			got, ok := kinds.kindOf(ref)
			if !ok || got != c.want {
				t.Errorf("kindOf(%s) = (%d, %v), want (%d, true)", c.name, got, ok, c.want)
			}
			if h := kinds.isHandle(ref); h != c.handle {
				t.Errorf("isHandle(%s) = %v, want %v", c.name, h, c.handle)
			}
			if j := kinds.isJsValue(ref); j != (c.want == namedJsValue) {
				t.Errorf("isJsValue(%s) = %v, want %v", c.name, j, c.want == namedJsValue)
			}
			// An option crosses as its handle or 0 exactly when the type
			// itself crosses as a handle (#163); the bare type is no option.
			opt := TypeRef{Kind: OptionKind, Elem: &ref}
			if h := kinds.isOptionalHandle(opt); h != c.handle {
				t.Errorf("isOptionalHandle(%s?) = %v, want %v", c.name, h, c.handle)
			}
			if kinds.isOptionalHandle(ref) {
				t.Errorf("isOptionalHandle(%s) = true, want false for a type that is not an option", c.name)
			}
			// A nested option (#756) has no single handle to pass.
			if kinds.isOptionalHandle(TypeRef{Kind: OptionKind, Elem: &opt}) {
				t.Errorf("isOptionalHandle(%s??) = true, want false", c.name)
			}
		})
	}

	for _, ref := range []TypeRef{
		{Kind: BuiltinKind, Builtin: "string"},
		{Kind: OptionKind, Elem: &TypeRef{Kind: NamedKind, Name: "Element"}},
		{Kind: ListKind, Elem: &TypeRef{Kind: NamedKind, Name: "JsValue"}},
	} {
		if got, ok := kinds.kindOf(ref); ok {
			t.Errorf("kindOf(%+v) = (%d, true), want ok=false for a non-NamedKind", ref, got)
		}
	}
}

// TestNamedKindsTakeNonResourcesOffTheHandlePath: a dictionary or enum is not
// a resource, so its parameter is not lowered to a `_handle` and its result is
// not constructed from one (#173), nor is a nullable one. Their marshalling is
// #174 and #175; until then they cross as the bare value. A resource beside
// them keeps its handle, nullable or not.
func TestNamedKindsTakeNonResourcesOffTheHandlePath(t *testing.T) {
	out := GeneratePromise(overloadIR(t, `enum ShadowRootMode { "open", "closed" };
dictionary ShadowRootInit {
	required ShadowRootMode mode;
};
interface ShadowRoot {};
interface Element {
	ShadowRoot attachShadow(ShadowRootInit init);
	attribute ShadowRootMode mode;
	readonly attribute ShadowRootMode? lastMode;
	readonly attribute ShadowRoot? shadowRoot;
};`), "web")

	assertContains(t, out, "handle := _element_attach_shadow(this._handle, init);")
	assertContains(t, out, "return ShadowRoot(_handle: handle, _owned: true);")
	assertContains(t, out, "_element_set_mode(this._handle, value);")
	// A nullable enum is not an optional handle; a nullable interface still is.
	assertContains(t, out, "get last_mode ShadowRootMode? `public `doc(\"`Element.lastMode` (WebIDL attribute)\") {\n        return _element_last_mode(this._handle);\n    }")
	assertContains(t, out, "handle := _element_shadow_root(this._handle);\n        if handle == 0 { return none; }\n        return ShadowRoot(_handle: handle, _owned: true);")
	assertNotContains(t, out, "init._handle")
	assertNotContains(t, out, "value._handle")
	assertNotContains(t, out, "ShadowRootMode(_handle:")
	// The extern ABI is unchanged: every named type crosses as an i32.
	assertContains(t, out, "_element_attach_shadow(i32 handle, i32 init) i32")
}

// TestJsValueConstructorParam covers the constructor path, whose arguments
// lower through formatExternCallArgs like a static's (#173).
func TestJsValueConstructorParam(t *testing.T) {
	out := GeneratePromise(overloadIR(t, `interface Thing {
	constructor(any init);
};`), "web")
	assertContains(t, out, "handle := _thing_constructor(_js_value_to_ref(init));")
	assertContains(t, out, "_thing_constructor(i32 init) i32")
}

// jsValueTag returns the tag of the JsValue variant named name: its index in
// jsValueCases.
func jsValueTag(t *testing.T, name string) int {
	t.Helper()
	for i, c := range jsValueCases {
		if c.name == name {
			return i
		}
	}
	t.Fatalf("no JsValue variant %q", name)
	return -1
}

// TestJsValueConversions pins the two functions every JsValue crosses the
// boundary through (#173). Lowering hands a ref-holding variant's own handle
// over and builds any other in the glue. Lifting is eager: each tag maps to its
// variant, a primitive or payload-less variant copies its value out and
// releases the slot — nothing else refers to it — and a ref-holding variant
// keeps the handle, unreleased, Object being the fallback.
func TestJsValueConversions(t *testing.T) {
	modules := []*Module{{Name: "web", ImportModule: wasmweb.ImportModule, HasJsValue: true}}
	out := GeneratePromise(modules, "web")

	assertContains(t, out, `_js_value_to_ref(JsValue value) i32 `+"`target(web)"+` {
    return match value {
        Undefined => _js_value_undefined(),
        Null => _js_value_null(),
        Bool(v) => _js_value_bool(v),
        Number(v) => _js_value_number(v),
        Str(v) => _js_value_str(v),
        Object(r) => r,
        Array(r) => r,
        Function(r) => r,
    };
}`)

	from := fmt.Sprintf(`_js_value_from_ref(i32 ref) JsValue `+"`target(web)"+` {
    tag := _js_value_tag(ref);
    if tag == %d {
        _js_value_release(ref);
        return JsValue.Undefined;
    }
    if tag == %d {
        _js_value_release(ref);
        return JsValue.Null;
    }
    if tag == %d {
        lifted := JsValue.Bool(value: _js_value_get_bool(ref));
        _js_value_release(ref);
        return lifted;
    }
    if tag == %d {
        lifted := JsValue.Number(value: _js_value_get_number(ref));
        _js_value_release(ref);
        return lifted;
    }
    if tag == %d {
        lifted := JsValue.Str(value: _js_value_get_str(ref));
        _js_value_release(ref);
        return lifted;
    }
    if tag == %d {
        return JsValue.Array(_js_ref: ref);
    }
    if tag == %d {
        return JsValue.Function(_js_ref: ref);
    }
    return JsValue.Object(_js_ref: ref);
}`, jsValueTag(t, "Undefined"), jsValueTag(t, "Null"), jsValueTag(t, "Bool"), jsValueTag(t, "Number"),
		jsValueTag(t, "Str"), jsValueTag(t, "Array"), jsValueTag(t, "Function"))
	assertContains(t, out, from)

	// The ref-holding variants' handles are i32 ref-table indices.
	for _, v := range []string{"Object", "Array", "Function"} {
		assertContains(t, out, "    "+v+"(i32 _js_ref),\n")
	}

	for _, extern := range []string{
		"_js_value_undefined() i32",
		"_js_value_null() i32",
		"_js_value_bool(bool value) i32",
		"_js_value_number(f64 value) i32",
		"_js_value_str(string value) i32",
		"_js_value_tag(i32 ref) i32",
		"_js_value_get_bool(i32 ref) bool",
		"_js_value_get_number(i32 ref) f64",
		"_js_value_get_str(i32 ref) string",
		"_js_value_release(i32 ref)",
	} {
		name := strings.TrimPrefix(extern[:strings.Index(extern, "(")], "_")
		assertContains(t, out, extern+"\n    `extern(\""+name+"\")\n    `wasm_import(\"promise_env\", \""+name+"\")\n    `target(web);")
	}
}

// TestJsValueConversionsFollowTarget: the conversions and their externs carry
// the target and import module the module is generated for, like every other
// generated host declaration, while the enum carries no target at all — it
// makes no host call, so it compiles on every target.
func TestJsValueConversionsFollowTarget(t *testing.T) {
	modules := []*Module{{Name: "web", ImportModule: "host_env", HasJsValue: true}}
	out := GeneratePromise(modules, "wasi")

	assertContains(t, out, "_js_value_to_ref(JsValue value) i32 `target(wasi) {")
	assertContains(t, out, "_js_value_from_ref(i32 ref) JsValue `target(wasi) {")
	assertContains(t, out, "`wasm_import(\"host_env\", \"js_value_tag\")\n    `target(wasi);")
	assertContains(t, out, "enum JsValue `public `doc(")
	assertNotContains(t, out, "`target(web)")
}

// TestGenerateJSGlueJsValue checks the glue's half (#173): every import the
// conversions declare, a tag test rendered from jsValueCases, and a JsValue
// argument read with _refTake (releasing a builder's temporary slot) while a
// resource argument keeps _refLoad. A nullable JsValue argument is read the same
// way: its `none` crosses as a built Undefined (#750), whose slot is temporary
// too.
func TestGenerateJSGlueJsValue(t *testing.T) {
	glue := GenerateJSGlue(overloadIR(t, `interface Node {};
interface Target {
	undefined take(any x, Node n);
	undefined maybe(object? y);
};`))

	for _, want := range []string{
		"    js_value_undefined() {\n      return _refStoreTemp(undefined);\n    },",
		"    js_value_null() {\n      return _refStoreTemp(null);\n    },",
		"    js_value_bool(v) {\n      return _refStoreTemp(v !== 0);\n    },",
		"    js_value_number(v) {\n      return _refStoreTemp(v);\n    },",
		"    js_value_str(ptr, len) {\n      return _refStoreTemp(_readString(ptr, len));\n    },",
		"    js_value_tag(handle) {\n      return _jsValueTag(_refLoad(handle));\n    },",
		"    js_value_get_bool(handle) {\n      return _refLoad(handle) ? 1 : 0;\n    },",
		"    js_value_get_number(handle) {\n      return _refLoad(handle);\n    },",
		"    js_value_get_str(handle, retptr) {\n      _writeStringResult(retptr, _refLoad(handle));\n    },",
		"    js_value_release(handle) {\n      _refRelease(handle);\n    },",
		"_refLoad($handle).take(_refTake(x), _refLoad(n));",
		"_refLoad($handle).maybe(_refTake(y));",
	} {
		assertContains(t, glue, want)
	}

	var tag strings.Builder
	tag.WriteString("function _jsValueTag(v) {\n")
	for _, c := range jsValueCases {
		if c.jsTest != "" {
			fmt.Fprintf(&tag, "  if (%s) return %d;\n", c.jsTest, jsValueTag(t, c.name))
		}
	}
	fmt.Fprintf(&tag, "  return %d;\n}", jsValueTag(t, "Object"))
	assertContains(t, glue, tag.String())
}
