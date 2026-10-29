package bindgen

import "strings"

// jsValueName is the enum every WebIDL type with no Promise type of its own —
// `any`, `object`, a union, `Promise<T>`, `record<K, V>` — binds as
// (docs/wasm-bindings.md §"JsValue the Dynamic Value Enum").
const jsValueName = "JsValue"

// jsValueCase is one variant of the generated JsValue enum. Its index in
// jsValueCases is its tag: the number the glue's js_value_tag import answers
// for a JS value of that kind (emitJsValueRuntime), and the one
// _js_value_from_ref dispatches on (emitJsValueConversions).
//
// jsValueCases is the one definition of the variants: the enum, both
// conversion functions and their externs, and the glue's tag test, builders
// and getters are all rendered from it, so the two generated files cannot
// disagree on which tag is which variant.
type jsValueCase struct {
	name string // the variant
	// payload is the Promise type of the primitive the variant copies —
	// bool, f64 or string — and "" for none. A variant that holds no ref
	// crosses through a builder import into a temporary slot, and is lifted by
	// copying out of one.
	payload  string
	holdsRef bool   // the variant keeps its value's ref-table handle (`i32 _js_ref`)
	jsTest   string // the JS condition on `v` selecting the variant in js_value_tag; "" for the fallback
	jsStore  string // the JS value the builder stores, over its parameters (jsValueBuilderParams)
}

var jsValueCases = []jsValueCase{
	{name: "Undefined", jsTest: "v === undefined", jsStore: "undefined"},
	{name: "Null", jsTest: "v === null", jsStore: "null"},
	// A bool crosses as the i32 0 or 1; the builder stores a JS boolean so it
	// lifts back as Bool, never as Number.
	{name: "Bool", payload: "bool", jsTest: `typeof v === "boolean"`, jsStore: "v !== 0"},
	{name: "Number", payload: "f64", jsTest: `typeof v === "number"`, jsStore: "v"},
	{name: "Str", payload: "string", jsTest: `typeof v === "string"`, jsStore: "_readString(ptr, len)"},
	// Object is the fallback: any value no other test selects — a plain
	// object, but also a bigint or a symbol — keeps its handle.
	{name: "Object", holdsRef: true},
	{name: "Array", holdsRef: true, jsTest: "Array.isArray(v)"},
	{name: "Function", holdsRef: true, jsTest: `typeof v === "function"`},
}

// The JsValue helpers' names. Each import is linked under its own name and
// declared in Promise with a leading `_`. The prefix is toSnake(jsValueName).
// An interface's generated extern is `_<snake interface>_<member>`, so only an
// interface whose snake name and member spell one of these meets it —
// `JsValue.tag`, `Js.valueTag` — and bind's self-check then reports the
// redeclaration. An import name carries no `.`, so none meets a WebIDL import
// (`Iface.member`).
const (
	jsValueToRef         = "_js_value_to_ref"
	jsValueFromRef       = "_js_value_from_ref"
	jsValueTagImport     = "js_value_tag"
	jsValueReleaseImport = "js_value_release"
)

// jsValueBuilder returns the import that stores c's value in a temporary slot.
func jsValueBuilder(c jsValueCase) string { return "js_value_" + strings.ToLower(c.name) }

// jsValueGetter returns the import that reads the primitive of c's kind out of
// a slot.
func jsValueGetter(c jsValueCase) string { return "js_value_get_" + strings.ToLower(c.name) }

// jsValueBuilderParams returns the JS parameters of c's builder: the
// primitive it stores, a string crossing as its (ptr, len) pair.
func jsValueBuilderParams(c jsValueCase) string {
	switch c.payload {
	case "":
		return ""
	case "string":
		return "ptr, len"
	default:
		return "v"
	}
}
