package bindgen

import (
	"strings"
	"testing"

	"djabi.dev/go/promise_lang/internal/webidl"
)

// T0724 Pass 1: the WebIDL binder must (1) rewrite identifiers that collide with
// Promise keywords, and (2) emit dictionaries as plain types — not `value types,
// since dictionary members include non-copy types (string, sequences, JsValue?)
// and JsValue cannot be `copy.

// idlToSnake rewrites keyword-colliding names by prefixing "_"; non-keyword names
// are converted to snake_case unchanged.
func TestT0724IdlToSnakeKeywords(t *testing.T) {
	cases := map[string]string{
		"type":        "_type",
		"for":         "_for",
		"match":       "_match",
		"in":          "_in",
		"continue":    "_continue",
		"move":        "_move",
		"yield":       "_yield",
		"present":     "_present",
		"absent":      "_absent",
		"unsafe":      "_unsafe",
		"normal":      "normal",
		"tagName":     "tag_name",
		"getURL":      "get_url",
		"setAttrName": "set_attr_name",
	}
	for in, want := range cases {
		if got := idlToSnake(in); got != want {
			t.Errorf("idlToSnake(%q) = %q, want %q", in, got, want)
		}
	}
}

// A keyword parameter name (`type`, `for`) is sanitized consistently across the
// wrapper signature, the extern call, and the extern declaration so the generated
// source parses.
func TestT0724KeywordParamSanitized(t *testing.T) {
	src := `interface El {
		undefined doThing(DOMString type, long for);
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	out := GeneratePromise(WebIdlToIR(file), "web")

	// Wrapper signature uses the sanitized names.
	assertContains(t, out, "do_thing(this, string _type, i32 _for)")
	// Extern call passes the sanitized names.
	assertContains(t, out, "_el_do_thing(this._handle, _type, _for);")
	// Extern declaration uses the sanitized names.
	assertContains(t, out, "_el_do_thing(i32 handle, string _type, i32 _for)")
	// The bare keyword must not appear as a parameter declaration.
	assertNotContains(t, out, "string type,")
	assertNotContains(t, out, "i32 for)")
}

// A dictionary becomes a plain Promise type: no `value on the type declaration or
// any field. Required members map to a bare type; optional members map to T?.
func TestT0724DictionaryPlainType(t *testing.T) {
	src := `dictionary Opts {
		required DOMString name;
		boolean checkOpacity;
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	out := GeneratePromise(WebIdlToIR(file), "web")

	assertContains(t, out, "type Opts `public {")
	assertContains(t, out, "string name;")         // required → bare type
	assertContains(t, out, "bool? check_opacity;") // optional → T?
	// No `value annotation anywhere (the leading backtick distinguishes the
	// annotation from the variant field name `value` used inside JsValue).
	assertNotContains(t, out, "`value")
}

// The original issue #3 repro: a dictionary member typed `any` (→ JsValue?) must
// be a plain optional field. JsValue cannot be `copy (its Str(string) variant
// holds a non-copy string), so a value field would not compile.
func TestT0724DictionaryJsValueFieldPlain(t *testing.T) {
	src := `dictionary SetHTMLOptions {
		any sanitizer;
		boolean runScripts;
	};`
	file, errs := webidl.Parse(src, "test.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	out := GeneratePromise(WebIdlToIR(file), "web")

	assertContains(t, out, "type SetHTMLOptions `public {")
	assertContains(t, out, "JsValue? sanitizer;")
	assertContains(t, out, "bool? run_scripts;")
	assertNotContains(t, out, "`value")
	// JsValue stays a non-copy enum (must NOT be made `copy).
	if i := strings.Index(out, "enum JsValue"); i >= 0 {
		line := out[i : i+strings.IndexByte(out[i:], '\n')]
		if strings.Contains(line, "`copy") {
			t.Errorf("JsValue must not be `copy: %q", line)
		}
	}
}
