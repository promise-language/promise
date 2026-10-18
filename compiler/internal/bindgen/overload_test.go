package bindgen

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/webidl"
)

// #263: a WebIDL overload set — operations of one interface that generate the
// same Promise name — becomes distinct members, because Promise has no
// overloading. The first keeps its name; the k-th is `<name>_<k>`, imported as
// `<Interface>.<member>.<k>`. Before the fix every overload emitted the same
// extern, which the compiler rejected as "redeclared in this scope".

// overloadIR runs the path `promise bind webidl` takes: parse, merge partials
// and mixins, convert.
func overloadIR(t *testing.T, src string) []*Module {
	t.Helper()
	file, errs := webidl.Parse(src, "overload.webidl")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	webidl.Merge(file)
	return WebIdlToIR(file)
}

// overloadResource returns the named resource of the converted module.
func overloadResource(t *testing.T, modules []*Module, name string) Resource {
	t.Helper()
	for _, r := range modules[0].Resources {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no resource %q in %+v", name, modules[0].Resources)
	return Resource{}
}

// overloadMethod is what the IR must say about one member: the name every
// generated Promise name derives from, the import the glue implements, its
// kind, and its parameter types (proving each overload kept its own signature).
type overloadMethod struct {
	name       string
	importName string
	kind       FuncKind
	params     string
}

func describeMethod(f Func) overloadMethod {
	var params []string
	for _, p := range f.Params {
		params = append(params, promiseType(p.Type))
	}
	return overloadMethod{name: f.Name, importName: f.ImportName, kind: f.Kind, params: strings.Join(params, ", ")}
}

func TestWebIdlOverloadSetNames(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		resource string
		want     []overloadMethod
	}{
		{
			// The item's own example: both overloads have arity 2, so an arity
			// suffix could not tell them apart.
			name: "same arity, different types",
			src: `interface TrustedHTML {};
			interface Element {
				undefined setAttribute(DOMString name, DOMString value);
				undefined setAttribute(DOMString name, TrustedHTML value);
			};`,
			resource: "Element",
			want: []overloadMethod{
				{"set_attribute", "Element.setAttribute", FuncMethod, "string, string"},
				{"set_attribute_2", "Element.setAttribute.2", FuncMethod, "string, TrustedHTML"},
			},
		},
		{
			name: "three-way arity set",
			src: `interface Img {};
			interface Ctx {
				undefined drawImage(Img image, double dx, double dy);
				undefined drawImage(Img image, double dx, double dy, double dw, double dh);
				undefined drawImage(Img image, double sx, double sy, double sw, double sh, double dx, double dy, double dw, double dh);
			};`,
			resource: "Ctx",
			want: []overloadMethod{
				{"draw_image", "Ctx.drawImage", FuncMethod, "Img, f64, f64"},
				{"draw_image_2", "Ctx.drawImage.2", FuncMethod, "Img, f64, f64, f64, f64"},
				{"draw_image_3", "Ctx.drawImage.3", FuncMethod, "Img, f64, f64, f64, f64, f64, f64, f64, f64"},
			},
		},
		{
			name: "static set",
			src: `interface Element {
				static Element make(long n);
				static Element make(DOMString tag);
			};`,
			resource: "Element",
			want: []overloadMethod{
				{"make", "Element.make", FuncStatic, "i32"},
				{"make_2", "Element.make.2", FuncStatic, "string"},
			},
		},
		{
			// The ordinal follows the merged member order: the interface's own
			// members, then partials, then included mixins.
			name: "set split across partial and mixin",
			src: `interface Element { undefined foo(long a); };
			partial interface Element { undefined foo(DOMString a); };
			interface mixin Extra { undefined foo(boolean a); };
			Element includes Extra;`,
			resource: "Element",
			want: []overloadMethod{
				{"foo", "Element.foo", FuncMethod, "i32"},
				{"foo_2", "Element.foo.2", FuncMethod, "string"},
				{"foo_3", "Element.foo.3", FuncMethod, "bool"},
			},
		},
		{
			// Anonymous special operations are emitted under their keyword, and
			// the import names that keyword too — never an empty member.
			name: "anonymous special operations",
			src: `interface Element {};
			interface Form {
				getter Element (unsigned long index);
				getter Element (DOMString name);
			};`,
			resource: "Form",
			want: []overloadMethod{
				{"getter", "Form.getter", FuncMethod, "u32"},
				{"getter_2", "Form.getter.2", FuncMethod, "string"},
			},
		},
		{
			// Counters are per interface: B's close is not B's second close.
			name: "same name on two interfaces",
			src: `interface A { undefined close(); };
			interface B { undefined close(); };`,
			resource: "B",
			want: []overloadMethod{
				{"close", "B.close", FuncMethod, ""},
			},
		},
		{
			// A read-write attribute's getter and setter legitimately share a
			// name (the accessor tells them apart); only operations are counted.
			name: "attribute beside an overload set",
			src: `interface Element {
				attribute DOMString foo;
				undefined bar(long a);
				undefined bar(DOMString a);
			};`,
			resource: "Element",
			want: []overloadMethod{
				{"foo", "Element.foo.get", FuncMethod, ""},
				{"foo", "Element.foo.set", FuncMethod, "string"},
				{"bar", "Element.bar", FuncMethod, "i32"},
				{"bar_2", "Element.bar.2", FuncMethod, "string"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := overloadResource(t, overloadIR(t, tc.src), tc.resource)
			var got []overloadMethod
			for _, m := range res.Methods {
				got = append(got, describeMethod(m))
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d methods %+v, want %d %+v", len(got), got, len(tc.want), tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("method %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// overloadFixture has an instance overload set of equal arity and a static one.
const overloadFixture = `interface TrustedHTML {};
interface Element {
	undefined setAttribute(DOMString name, DOMString value);
	undefined setAttribute(DOMString name, TrustedHTML value);
	static Element make(long n);
	static Element make(DOMString tag);
};`

// Every overload gets its own wrapper, extern and import, each declared exactly
// once. The counts are the regression check: before #263 the module declared
// `_element_set_attribute` and `_element_make` twice each.
func TestWebIdlOverloadCodegenDistinctMembers(t *testing.T) {
	out := GeneratePromise(overloadIR(t, overloadFixture), "web")

	assertContains(t, out, "set_attribute(this, string name, string value) `public {")
	assertContains(t, out, "_element_set_attribute(this._handle, name, value);")
	assertContains(t, out, "set_attribute_2(this, string name, TrustedHTML value) `public {")
	assertContains(t, out, "_element_set_attribute_2(this._handle, name, value._handle);")
	assertContains(t, out, "make(i32 n) Element `public `global {")
	assertContains(t, out, "make_2(string tag) Element `public `global {")
	assertContains(t, out, "handle := _element_make_2(tag);")

	assertContains(t, out, "_element_set_attribute_2(i32 handle, string name, i32 value)\n")
	assertContains(t, out, "`extern(\"element_set_attribute_2\")")
	assertContains(t, out, "`wasm_import(\"promise_env\", \"Element.setAttribute.2\")")
	assertContains(t, out, "_element_make_2(string tag) i32\n")
	assertContains(t, out, "`extern(\"element_make_2\")")
	assertContains(t, out, "`wasm_import(\"promise_env\", \"Element.make.2\")")

	for _, decl := range []string{
		"\n_element_set_attribute(", "\n_element_set_attribute_2(",
		"\n_element_make(", "\n_element_make_2(",
		"    set_attribute(this", "    set_attribute_2(this",
		"    make(", "    make_2(",
		`"Element.setAttribute")`, `"Element.setAttribute.2")`,
	} {
		if n := strings.Count(out, decl); n != 1 {
			t.Errorf("%q appears %d times, want exactly once:\n%s", decl, n, out)
		}
	}
}

// Each overload's import calls the same JS member with that overload's own
// argument conversions, so the host's overload resolution picks the overload.
// A JS object literal silently keeps the last of two equal keys, so each key
// must appear exactly once.
func TestWebIdlOverloadJSGlue(t *testing.T) {
	glue := GenerateJSGlue(overloadIR(t, overloadFixture))

	assertContains(t, glue, `"Element.setAttribute"(handle, name_ptr, name_len, value_ptr, value_len) {`)
	assertContains(t, glue, "_refLoad(handle).setAttribute(_readString(name_ptr, name_len), _readString(value_ptr, value_len))")
	assertContains(t, glue, `"Element.setAttribute.2"(handle, name_ptr, name_len, value) {`)
	assertContains(t, glue, "_refLoad(handle).setAttribute(_readString(name_ptr, name_len), _refLoad(value))")
	assertContains(t, glue, `"Element.make"(n) {`)
	assertContains(t, glue, "Element.make(n)")
	assertContains(t, glue, `"Element.make.2"(tag_ptr, tag_len) {`)
	assertContains(t, glue, "Element.make(_readString(tag_ptr, tag_len))")
	// The member is never rebuilt from the suffixed Promise name.
	assertNotContains(t, glue, "setAttribute2")
	assertNotContains(t, glue, "make2")

	for _, key := range []string{`"Element.setAttribute"(`, `"Element.setAttribute.2"(`, `"Element.make"(`, `"Element.make.2"(`} {
		if n := strings.Count(glue, key); n != 1 {
			t.Errorf("import key %q appears %d times, want exactly once:\n%s", key, n, glue)
		}
	}
}

// The glue reads the JS member from the import name, where the front end wrote
// the IDL identifier verbatim — so an identifier that does not survive the
// snake_case round trip (an acronym) is still called by its own name.
func TestWebIdlJSGlueMemberIsVerbatimIdentifier(t *testing.T) {
	glue := GenerateJSGlue(overloadIR(t, `interface Doc {
		DOMString toJSON();
		static Doc fromURL(DOMString url);
		readonly attribute DOMString baseURI;
	};`))

	assertContains(t, glue, "_refLoad(handle).toJSON()")
	assertContains(t, glue, "Doc.fromURL(_readString(url_ptr, url_len))")
	assertContains(t, glue, "_refLoad(handle).baseURI")
	assertNotContains(t, glue, "toJson")
	assertNotContains(t, glue, "fromUrl")
}

// anonymousGetterFixture is an overload set of anonymous special operations,
// the shape HTMLFormElement has in real IDL.
const anonymousGetterFixture = `interface Element {};
interface Form {
	getter Element (unsigned long index);
	getter Element (DOMString name);
};`

// An anonymous special operation's import names its keyword, so the glue calls
// a member rather than emitting `obj.(...)`, which would not parse.
func TestWebIdlOverloadAnonymousSpecialGlue(t *testing.T) {
	glue := GenerateJSGlue(overloadIR(t, anonymousGetterFixture))

	assertContains(t, glue, `"Form.getter"(handle, index) {`)
	assertContains(t, glue, "_refLoad(handle).getter(index)")
	assertContains(t, glue, `"Form.getter.2"(handle, name_ptr, name_len) {`)
	assertContains(t, glue, "_refLoad(handle).getter(_readString(name_ptr, name_len))")
	assertNotContains(t, glue, ").(")
}
