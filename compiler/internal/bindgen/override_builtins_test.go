package bindgen

import (
	"strings"
	"testing"
)

// #697: an object implementing an interface that carries
// [LegacyOverrideBuiltIns] has its supported named properties as its own, over
// every member of its prototype chain — a form holding <input name="action">
// answers form.action with that input. Such an object reaches the imports of
// every interface in that interface's inheritance chain, so each of their
// regular members calls its declaring interface's own member, as a redeclared
// one does (#682), but for a [LegacyUnforgeable] member, which lives on the
// object (docs/wasm-bindings.md, "WebIDL to Promise", interface inheritance).
// Before the fix every import below resolved on the object.

// overrideBuiltInsFixture has HTMLFormElement's chain, Document's attribute on
// a partial interface beside its unforgeable location, a descendant, a mixin
// included in and out of the chains, and interfaces outside every chain: a
// sibling of the form and one sharing only the root.
const overrideBuiltInsFixture = `interface EventTarget { undefined addEventListener(DOMString type); };
interface Node : EventTarget { readonly attribute DOMString nodeName; };
interface Element : Node { attribute DOMString id; };
interface HTMLElement : Element { attribute DOMString title; };
[Exposed=Window, LegacyOverrideBuiltIns, LegacyUnenumerableNamedProperties]
interface HTMLFormElement : HTMLElement {
	attribute DOMString action;
	undefined submit();
	getter Element (unsigned long index);
	getter Element? (DOMString name);
};
interface HTMLDivElement : HTMLElement { attribute DOMString align; };
interface Window : EventTarget { readonly attribute DOMString name; };
interface Location {};
interface Document : Node { attribute DOMString cookie; };
[LegacyOverrideBuiltIns]
partial interface Document {
	[PutForwards=href, LegacyUnforgeable] readonly attribute Location? location;
	getter Element? (DOMString name);
};
interface XMLDocument : Document { undefined load(DOMString url); };
interface DocumentFragment : Node {};
interface mixin ParentNode { Element? querySelector(DOMString selectors); };
Element includes ParentNode;
Document includes ParentNode;
DocumentFragment includes ParentNode;`

func TestWebIdlOverrideBuiltInsGlue(t *testing.T) {
	js := GenerateJSGlue(overloadIR(t, overrideBuiltInsFixture))
	cases := []struct {
		key  string
		want string
	}{
		// The interface carrying [LegacyOverrideBuiltIns].
		{"HTMLFormElement.action.get", `_writeStringResult($retptr, Reflect.get(HTMLFormElement.prototype, "action", _refLoad($handle)));`},
		{"HTMLFormElement.action.set", `Reflect.set(HTMLFormElement.prototype, "action", _readString(value$ptr, value$len), _refLoad($handle));`},
		{"HTMLFormElement.submit", `Reflect.apply(HTMLFormElement.prototype.submit, _refLoad($handle), []);`},
		// Its ancestors, up to the root: a form reaches them through a
		// parent-typed wrapper.
		{"HTMLElement.title.get", `_writeStringResult($retptr, Reflect.get(HTMLElement.prototype, "title", _refLoad($handle)));`},
		{"Element.id.get", `_writeStringResult($retptr, Reflect.get(Element.prototype, "id", _refLoad($handle)));`},
		{"Node.nodeName.get", `_writeStringResult($retptr, Reflect.get(Node.prototype, "nodeName", _refLoad($handle)));`},
		{"EventTarget.addEventListener", `Reflect.apply(EventTarget.prototype.addEventListener, _refLoad($handle), [_readString(_type$ptr, _type$len)]);`},
		// Carried on a partial interface; a mixin member of the chain; a
		// descendant, whose objects are Documents too.
		{"Document.cookie.get", `_writeStringResult($retptr, Reflect.get(Document.prototype, "cookie", _refLoad($handle)));`},
		{"Document.querySelector", `const result = Reflect.apply(Document.prototype.querySelector, _refLoad($handle), [_readString(selectors$ptr, selectors$len)]);`},
		{"XMLDocument.load", `Reflect.apply(XMLDocument.prototype.load, _refLoad($handle), [_readString(url$ptr, url$len)]);`},

		// An unforgeable member lives on the object, and no named property
		// hides it.
		{"Document.location.get", `const result = _refLoad($handle).location;`},
		// Outside every chain: a sibling of the form, an interface sharing only
		// the root, and a mixin member included elsewhere.
		{"HTMLDivElement.align.get", `_writeStringResult($retptr, _refLoad($handle).align);`},
		{"Window.name.get", `_writeStringResult($retptr, _refLoad($handle).name);`},
		{"DocumentFragment.querySelector", `const result = _refLoad($handle).querySelector(_readString(selectors$ptr, selectors$len));`},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if body := glueImport(t, js, tc.key); !strings.Contains(body, tc.want) {
				t.Errorf("import %q:\n%s\nwant it to contain %s", tc.key, body, tc.want)
			}
		})
	}
}

// The chains are taken from the whole input, before the keep list reduces it:
// a form exists in the page whether or not HTMLFormElement is bound, so
// dropping it must not send Element's imports back to the object.
func TestWebIdlOverrideBuiltInsSurvivesKeepList(t *testing.T) {
	m, _ := keepIR(t, overrideBuiltInsFixture, "Element\nElement.id")
	for _, r := range m.Resources {
		if r.Name == "HTMLFormElement" || r.Name == "HTMLElement" {
			t.Errorf("the list keeps %s, want it dropped", r.Name)
		}
	}
	js := GenerateJSGlue([]*Module{m})
	want := `Reflect.get(Element.prototype, "id", _refLoad($handle))`
	if body := glueImport(t, js, "Element.id.get"); !strings.Contains(body, want) {
		t.Errorf("import %q:\n%s\nwant it to contain %s", "Element.id.get", body, want)
	}
}
