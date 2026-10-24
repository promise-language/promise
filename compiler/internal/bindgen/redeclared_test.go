package bindgen

import (
	"strings"
	"testing"
)

// #682: the glue resolves an instance member on the object, through its own
// prototype chain. When an interface inheriting from the declaring one
// redeclares the member, that lookup reaches the redeclaration and its result
// crosses under the declaring interface's type, so such an import calls the
// declaring interface's own member instead (docs/wasm-bindings.md, "WebIDL to
// Promise", interface inheritance). Before the fix every import below resolved
// on the object.

// redeclaredFixture covers the item's three cases — an attribute and a named
// getter redeclared with another type, and an overload set a derived interface
// redeclares only in part — beside members nobody redeclares and a static that
// shares a redeclared identifier.
const redeclaredFixture = `interface Element { attribute DOMString className; attribute DOMString id; };
interface SVGAnimatedString {};
interface SVGElement : Element { readonly attribute SVGAnimatedString className; };
interface RadioNodeList {};
interface HTMLCollection { getter Element? namedItem(DOMString name); };
interface HTMLFormControlsCollection : HTMLCollection {
	getter (RadioNodeList or Element)? namedItem(DOMString name);
};
interface P {
	undefined f(long a);
	undefined f(DOMString s);
	static undefined g();
	undefined g(long a);
};
interface D : P { undefined f(long a); undefined g(long a); };`

// glueImport returns the body of the glue's import named key.
func glueImport(t *testing.T, js, key string) string {
	t.Helper()
	start := strings.Index(js, `"`+key+`"(`)
	if start < 0 {
		t.Fatalf("glue has no import %q:\n%s", key, js)
	}
	body := js[start:]
	end := strings.Index(body, "\n    },\n")
	if end < 0 {
		t.Fatalf("import %q is not closed:\n%s", key, body)
	}
	return body[:end]
}

func TestWebIdlRedeclaredGlue(t *testing.T) {
	js := GenerateJSGlue(overloadIR(t, redeclaredFixture))
	cases := []struct {
		key  string
		want string
	}{
		// Redeclared by a derived interface: the declaring interface's own member.
		{"Element.className.get", `_writeStringResult($retptr, Reflect.get(Element.prototype, "className", _refLoad($handle)));`},
		{"Element.className.set", `Reflect.set(Element.prototype, "className", _readString(value$ptr, value$len), _refLoad($handle));`},
		{"HTMLCollection.namedItem", `const $result = Reflect.apply(HTMLCollection.prototype.namedItem, _refLoad($handle), [_readString(name$ptr, name$len)]);`},
		// D redeclares only f(long), which replaces the whole set on its
		// prototype: the overload it leaves out must still reach P's f.
		{"P.f", `Reflect.apply(P.prototype.f, _refLoad($handle), [a]);`},
		{"P.f.2", `Reflect.apply(P.prototype.f, _refLoad($handle), [_readString(s$ptr, s$len)]);`},
		{"P.g.2", `Reflect.apply(P.prototype.g, _refLoad($handle), [a]);`},

		// Redeclared by no interface deriving from the declaring one: on the object.
		{"HTMLFormControlsCollection.namedItem.2", `const $result = _refLoad($handle).namedItem(_readString(name$ptr, name$len));`},
		{"SVGElement.className.get", `return _refStore(_refLoad($handle).className);`},
		{"D.f", `_refLoad($handle).f(a);`},
		{"D.g.2", `_refLoad($handle).g(a);`},
		{"Element.id.get", `_writeStringResult($retptr, _refLoad($handle).id);`},
		{"Element.id.set", `_refLoad($handle).id = _readString(value$ptr, value$len);`},
		// A static is reached through the interface object whatever its
		// identifier's instance members do.
		{"P.g", `P.g();`},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if body := glueImport(t, js, tc.key); !strings.Contains(body, tc.want) {
				t.Errorf("import %q:\n%s\nwant it to contain %s", tc.key, body, tc.want)
			}
		})
	}
}

// The mark is on the instance members alone: a static sharing the identifier
// of a redeclared operation is not one.
func TestWebIdlRedeclaredMarksInstanceMembers(t *testing.T) {
	p := overloadResource(t, overloadIR(t, redeclaredFixture), "P")
	want := map[string]bool{"P.f": true, "P.f.2": true, "P.g": false, "P.g.2": true}
	for _, m := range p.Methods {
		if m.ViaPrototype != want[m.ImportName] {
			t.Errorf("%s (%s) ViaPrototype = %v, want %v", m.ImportName, m.Name, m.ViaPrototype, want[m.ImportName])
		}
	}
}

// Redeclarations are taken from the whole input, before the keep list reduces
// it: a host object exists whether or not its interface is bound, so dropping
// the redeclaring interface, or only its redeclaring member, must not send the
// declaring interface's import back to the object.
func TestWebIdlRedeclaredSurvivesKeepList(t *testing.T) {
	cases := []struct {
		name    string
		list    string
		dropped []string // resources the list drops
		imports []string // imports that must still call the declaring interface's member
	}{
		{
			name:    "redeclaring interfaces dropped",
			list:    "Element\nElement.className\nHTMLCollection\nHTMLCollection.namedItem",
			dropped: []string{"SVGElement", "HTMLFormControlsCollection"},
			imports: []string{"Element.className.get", "HTMLCollection.namedItem"},
		},
		{
			name:    "redeclaring member dropped from a kept interface",
			list:    "Element\nHTMLFormControlsCollection\nHTMLCollection.namedItem",
			imports: []string{"HTMLCollection.namedItem"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := keepIR(t, redeclaredFixture, tc.list)
			for _, r := range m.Resources {
				for _, d := range tc.dropped {
					if r.Name == d {
						t.Errorf("the list keeps %s, want it dropped", d)
					}
				}
			}
			js := GenerateJSGlue([]*Module{m})
			assertNotContains(t, js, `"HTMLFormControlsCollection.namedItem`)
			for _, key := range tc.imports {
				if body := glueImport(t, js, key); !strings.Contains(body, "Reflect.") {
					t.Errorf("import %q resolves on the object:\n%s", key, body)
				}
			}
		})
	}
}
