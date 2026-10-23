package bindgen

import (
	"strings"
	"testing"
)

// #696: an anonymous getter, setter or deleter has no JS member — the host
// exposes it only as property access on the object, keyed by its first argument
// (docs/wasm-bindings.md, "Supported subset"). Before the fix the glue called
// its keyword as a method (`_refLoad(handle).getter(index)`), which no host
// object has, so every call threw a TypeError.

// specialOpsFixture declares every anonymous special operation, with an indexed
// (unsigned long) and a named (DOMString) key, beside a named getter, which is
// also a regular operation; StringMap has DOMStringMap's shape, and
// ReportingMap's deleter declares a result. The Node runtime test
// (TestGenerateJSGlueAnonymousSpecialReachesHost) calls Target's imports.
const specialOpsFixture = `interface Node {};
interface Target {
  getter Node? (unsigned long index);
  getter Node? (DOMString name);
  setter undefined (unsigned long index, Node? node);
  setter undefined (DOMString name, Node? node);
  deleter undefined (DOMString name);
  getter Node? item(unsigned long index);
};
interface StringMap {
  getter DOMString (DOMString name);
  setter undefined (DOMString name, DOMString value);
  deleter undefined (DOMString name);
};
interface ReportingMap {
  deleter boolean (DOMString name);
};`

// Each import's body is asserted directly after its header line, so a form
// cannot match under the wrong import.
func TestWebIdlAnonymousSpecialGlue(t *testing.T) {
	glue := GenerateJSGlue(overloadIR(t, specialOpsFixture))

	for _, tc := range []struct {
		name   string
		header string
		body   string
	}{
		{"indexed getter", `"Target.getter"(handle, index) {`, "const result = _refLoad(handle)[index >>> 0];"},
		{"named getter", `"Target.getter.2"(handle, name_ptr, name_len) {`, "const result = _refLoad(handle)[_readString(name_ptr, name_len)];"},
		{"indexed setter", `"Target.setter"(handle, index, node) {`, "_refLoad(handle)[index >>> 0] = _refLoad(node);"},
		{"named setter", `"Target.setter.2"(handle, name_ptr, name_len, node) {`, "_refLoad(handle)[_readString(name_ptr, name_len)] = _refLoad(node);"},
		{"deleter", `"Target.deleter"(handle, name_ptr, name_len) {`, "delete _refLoad(handle)[_readString(name_ptr, name_len)];"},
		// A named special operation is a regular operation: still a method call.
		{"named special operation", `"Target.item"(handle, index) {`, "const result = _refLoad(handle).item(index);"},
		{"string result", `"StringMap.getter"(handle, name_ptr, name_len, retptr) {`, "_writeStringResult(retptr, _refLoad(handle)[_readString(name_ptr, name_len)]);"},
		{"string value", `"StringMap.setter"(handle, name_ptr, name_len, value_ptr, value_len) {`, "_refLoad(handle)[_readString(name_ptr, name_len)] = _readString(value_ptr, value_len);"},
		// Only a setter's result is rejected: a deleter declaring one binds, and
		// receives what the delete yields.
		{"deleter result", `"ReportingMap.deleter"(handle, name_ptr, name_len) {`, "return delete _refLoad(handle)[_readString(name_ptr, name_len)];"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertContains(t, glue, "    "+tc.header+"\n      "+tc.body+"\n")
		})
	}
	for _, call := range []string{".getter(", ".setter(", ".deleter("} {
		assertNotContains(t, glue, call)
	}
}

// Property access passes exactly a key — and a setter, a value — and returns no
// setter result, so an anonymous special operation of any other shape cannot be
// bound: each is an error naming it and its position, and nothing is converted.
func TestWebIdlToIRAnonymousSpecialErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string // each must appear in the error
	}{
		{
			name: "getter without a key",
			src:  `interface T { getter long (); };`,
			want: []string{"anonymous getter of T takes 0 arguments, but property access passes exactly one: the key"},
		},
		{
			name: "getter with two arguments",
			src:  `interface T { getter long (unsigned long a, unsigned long b); };`,
			want: []string{"anonymous getter of T takes 2 arguments, but property access passes exactly one: the key"},
		},
		{
			name: "setter without a value",
			src:  `interface T { setter undefined (unsigned long i); };`,
			want: []string{"anonymous setter of T takes 1 argument, but property assignment passes exactly two: the key and the value"},
		},
		{
			name: "deleter with two arguments",
			src:  `interface T { deleter undefined (DOMString a, DOMString b); };`,
			want: []string{"anonymous deleter of T takes 2 arguments, but property access passes exactly one: the key"},
		},
		{
			name: "setter declaring a result",
			src:  `interface T { setter long (unsigned long i, long v); };`,
			want: []string{"anonymous setter of T declares a result, but property assignment returns none"},
		},
		{
			name: "position",
			src:  "interface T {\n  getter long ();\n};",
			want: []string{"dict.webidl:2:"},
		},
		{
			// Interfaces are converted, and reported, alongside a dictionary
			// that fails.
			name: "every malformed operation and dictionary is reported",
			src: `interface A { getter long (); };
			interface B { deleter undefined (); };
			dictionary D : Missing { long x; };`,
			want: []string{
				"anonymous getter of A",
				"anonymous deleter of B",
				"dictionary D inherits from Missing",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modules, err := WebIdlToIR(parseMerged(t, tc.src))
			if err == nil {
				t.Fatalf("WebIdlToIR succeeded, want an error containing %q", tc.want)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not contain %q:\n%v", w, err)
				}
			}
			if modules != nil {
				t.Errorf("WebIdlToIR returned modules alongside its error")
			}
		})
	}
}

// Only an anonymous special operation is held to property access's shape: a
// named one is called as a method, whatever its arguments, and a setter
// declaring `void` declares no result.
func TestWebIdlToIRSpecialShapesThatBind(t *testing.T) {
	for _, src := range []string{
		`interface Node {}; interface T { getter Node? item(unsigned long index, long extra); };`,
		`interface T { setter void (unsigned long i, long v); };`,
	} {
		if _, err := WebIdlToIR(parseMerged(t, src)); err != nil {
			t.Errorf("WebIdlToIR(%s): %v", src, err)
		}
	}
}
