package bindgen

import (
	"strings"
	"testing"
)

// #612: WebIDL lets a derived interface redeclare a member it inherits with a
// different signature, but a same-named instance member on a Promise `is` child
// is an override and must satisfy the inherited one. The overload set of a
// Promise name is therefore counted across the `is` chain (docs/wasm-bindings.md,
// "Shadowed Members"): a redeclaration that is exactly an inherited member
// overrides it and keeps its name; any other takes the next ordinal after the
// instance members it inherits under that name. Before the fix the child emitted
// the inherited name with its own signature, and the module failed with
// "cannot override inherited method".

func TestWebIdlShadowedMemberNames(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		resource string
		want     []overloadMethod
	}{
		{
			// The item's own case: the union lowers to JsValue, so the child's
			// result is not the parent's Element?. The import keeps the IDL
			// member, so the glue still calls namedItem.
			name: "different result",
			src: `interface Element {};
			interface RadioNodeList {};
			interface HTMLCollection {
				getter Element? namedItem(DOMString name);
			};
			interface HTMLFormControlsCollection : HTMLCollection {
				getter (RadioNodeList or Element)? namedItem(DOMString name);
			};`,
			resource: "HTMLFormControlsCollection",
			want: []overloadMethod{
				{"named_item_2", "HTMLFormControlsCollection.namedItem.2", FuncMethod, "string"},
			},
		},
		{
			name: "different parameters",
			src: `interface P { undefined m(DOMString a); };
			interface C : P { undefined m(long a); };`,
			resource: "C",
			want: []overloadMethod{
				{"m_2", "C.m.2", FuncMethod, "i32"},
			},
		},
		{
			// An attribute's getter and setter share the ordinal; its import is
			// unique already and names the accessor, so it keeps no ordinal.
			name: "attribute of a different type",
			src: `interface SVGAnimatedString {};
			interface Element { attribute DOMString className; };
			interface SVGElement : Element { attribute SVGAnimatedString className; };`,
			resource: "SVGElement",
			want: []overloadMethod{
				{"class_name_2", "SVGElement.className.get", FuncMethod, ""},
				{"class_name_2", "SVGElement.className.set", FuncMethod, "SVGAnimatedString"},
			},
		},
		{
			// DOMRect.x's shape, and HTMLOptionsCollection.length's: the exact getter
			// overrides, the setter is new.
			name: "exact attribute redeclaration overrides",
			src: `interface DOMRectReadOnly { readonly attribute double x; };
			interface DOMRect : DOMRectReadOnly { attribute double x; };`,
			resource: "DOMRect",
			want: []overloadMethod{
				{"x", "DOMRect.x.get", FuncMethod, ""},
				{"x", "DOMRect.x.set", FuncMethod, "f64"},
			},
		},
		{
			// A typedef is its target (#721), so the redeclaration is exactly
			// the inherited attribute and overrides it.
			name: "a typedef of the inherited type is exactly alike",
			src: `typedef DOMString Name;
			interface P { attribute DOMString label; };
			interface C : P { attribute Name label; };`,
			resource: "C",
			want: []overloadMethod{
				{"label", "C.label.get", FuncMethod, ""},
				{"label", "C.label.set", FuncMethod, "string"},
			},
		},
		{
			name: "exact redeclaration overrides, parameter names aside",
			src: `interface P { undefined m(DOMString name); };
			interface C : P { undefined m(DOMString key); };`,
			resource: "C",
			want: []overloadMethod{
				{"m", "C.m", FuncMethod, "string"},
			},
		},
		{
			// HTMLSelectElement's shape: remove() overrides the mixin's, and its
			// own second overload is numbered as before.
			name: "own overload after an override",
			src: `interface mixin ChildNode { undefined remove(); };
			interface Element {};
			Element includes ChildNode;
			interface HTMLSelectElement : Element {
				undefined remove();
				undefined remove(long index);
			};`,
			resource: "HTMLSelectElement",
			want: []overloadMethod{
				{"remove", "HTMLSelectElement.remove", FuncMethod, ""},
				{"remove_2", "HTMLSelectElement.remove.2", FuncMethod, "i32"},
			},
		},
		{
			// The child repeats the parent's second overload, so it overrides
			// m_2, and its other overload is numbered after both.
			name: "overload set split across levels",
			src: `interface P {
				undefined m(DOMString a);
				undefined m(long a);
			};
			interface C : P {
				undefined m(long a);
				undefined m(boolean a);
			};`,
			resource: "C",
			want: []overloadMethod{
				{"m_2", "C.m.2", FuncMethod, "i32"},
				{"m_3", "C.m.3", FuncMethod, "bool"},
			},
		},
		{
			// The kind is part of the signature: an operation of the getter's
			// shape is still a member of its own.
			name: "an operation never overrides an attribute",
			src: `interface P { readonly attribute long size; };
			interface C : P { long size(); };`,
			resource: "C",
			want: []overloadMethod{
				{"size_2", "C.size.2", FuncMethod, ""},
			},
		},
		{
			name: "grandchild overrides the child's shadow",
			src: `interface P { undefined m(DOMString a); };
			interface C : P { undefined m(long a); };
			interface G : C {
				undefined m(long a);
				undefined m(boolean a);
			};`,
			resource: "G",
			want: []overloadMethod{
				{"m_2", "G.m.2", FuncMethod, "i32"},
				{"m_3", "G.m.3", FuncMethod, "bool"},
			},
		},
		{
			// The child's renamed attribute is a slot of its own, so a third
			// type at the next level is numbered after both.
			name: "grandchild numbers after the child's renamed attribute",
			src: `interface P { attribute DOMString x; };
			interface C : P { attribute long x; };
			interface G : C { attribute boolean x; };`,
			resource: "G",
			want: []overloadMethod{
				{"x_3", "G.x.get", FuncMethod, ""},
				{"x_3", "G.x.set", FuncMethod, "bool"},
			},
		},
		{
			// Each sibling counts only its own ancestors.
			name: "sibling shadows are independent",
			src: `interface P { undefined m(DOMString a); };
			interface A : P { undefined m(long a); };
			interface B : P { undefined m(boolean a); };`,
			resource: "B",
			want: []overloadMethod{
				{"m_2", "B.m.2", FuncMethod, "bool"},
			},
		},
		{
			// DOMMatrix.fromMatrix's shape: statics are `global, never inherited
			// slots, so the child's hides the parent's under the same name.
			name: "static over static is not renamed",
			src: `interface P { static P make(long n); };
			interface C : P { static C make(long n); };`,
			resource: "C",
			want: []overloadMethod{
				{"make", "C.make", FuncStatic, "i32"},
			},
		},
		{
			// A static is `global`, never an override, so even one shaped
			// exactly like the inherited instance member is numbered after it.
			name: "static over an inherited instance member",
			src: `interface P { undefined foo(); };
			interface C : P { static undefined foo(); };`,
			resource: "C",
			want: []overloadMethod{
				{"foo_2", "C.foo.2", FuncStatic, ""},
			},
		},
		{
			// The same for a static attribute; its import keeps the accessor.
			name: "static attribute over an inherited instance attribute",
			src: `interface P { readonly attribute long q; };
			interface C : P { static readonly attribute long q; };`,
			resource: "C",
			want: []overloadMethod{
				{"q_2", "C.q.get", FuncStatic, ""},
			},
		},
		{
			// Constructors are `global factories, never inherited slots, so a
			// derived interface's are numbered on it alone — its factories are
			// create and create_2, not numbered after the parent's.
			name: "constructors are numbered on their own interface",
			src: `interface P { constructor(); constructor(long n); };
			interface C : P { constructor(); constructor(DOMString s); };`,
			resource: "C",
			want: []overloadMethod{
				{"constructor", "C.constructor", FuncConstructor, ""},
				{"constructor_2", "C.constructor.2", FuncConstructor, "string"},
			},
		},
		{
			// A constant is `global` and lowers to a getter-like member of its
			// snake_case name, so one sharing an inherited instance member's
			// Promise name is numbered after it, as a static operation is.
			name: "constant over an inherited instance member",
			src: `interface P { undefined reset(); };
			interface C : P { const long RESET = 1; };`,
			resource: "C",
			want: []overloadMethod{
				{"reset_2", "C.RESET.2", FuncStatic, ""},
			},
		},
		{
			// Constants are never inherited slots: the child's instance member
			// keeps its name beside the parent's constant.
			name: "instance member over an inherited constant",
			src: `interface P { const long RESET = 1; };
			interface C : P { undefined reset(long n); };`,
			resource: "C",
			want: []overloadMethod{
				{"reset", "C.reset", FuncMethod, "i32"},
			},
		},
		{
			// The decision pinned: identity, not the override rule's relaxed
			// matching. A covariant result and extra optional parameters are
			// overrides the compiler accepts, and are still new members.
			name: "compatible refinements are new members",
			src: `interface Track {
				Track clone();
				undefined start(optional double when);
			};
			interface CaptureTrack : Track {
				CaptureTrack clone();
				undefined start(optional double when, optional double offset);
			};`,
			resource: "CaptureTrack",
			want: []overloadMethod{
				{"clone_2", "CaptureTrack.clone.2", FuncMethod, ""},
				{"start_2", "CaptureTrack.start.2", FuncMethod, "f64?, f64?"},
			},
		},
		{
			name: "child declared before its parent",
			src: `interface C : P { undefined m(long a); };
			interface P { undefined m(DOMString a); };`,
			resource: "C",
			want: []overloadMethod{
				{"m_2", "C.m.2", FuncMethod, "i32"},
			},
		},
		{
			// Nothing to compare with: the module fails to compile with
			// `undefined type: Missing` (T1507), not with a renamed member.
			name:     "unbound parent",
			src:      `interface C : Missing { undefined m(long a); };`,
			resource: "C",
			want: []overloadMethod{
				{"m", "C.m", FuncMethod, "i32"},
			},
		},
		{
			// Malformed input: the cycle's back edge inherits nothing, so A,
			// converted first, sees B's m and B sees none of A's.
			name: "inheritance cycle terminates",
			src: `interface A : B { undefined m(long a); };
			interface B : A { undefined m(DOMString a); };`,
			resource: "A",
			want: []overloadMethod{
				{"m_2", "A.m.2", FuncMethod, "i32"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := overloadResource(t, overloadIR(t, tc.src), tc.resource)
			var got []overloadMethod
			for _, f := range r.Methods {
				got = append(got, describeMethod(f))
			}
			if len(got) != len(tc.want) {
				t.Fatalf("%s methods = %+v, want %+v", tc.resource, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("%s method %d = %+v, want %+v", tc.resource, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The parent is named as before a child shadowed it, and the resources keep
// the input order even though a parent is converted before its children.
func TestWebIdlShadowLeavesParentAndOrder(t *testing.T) {
	modules := overloadIR(t, `interface C : P { undefined m(long a); };
	interface P { undefined m(DOMString a); };`)
	var order []string
	for _, r := range modules[0].Resources {
		order = append(order, r.Name)
	}
	if got := strings.Join(order, ","); got != "C,P" {
		t.Errorf("resource order = %s, want the input order C,P", got)
	}
	p := overloadResource(t, modules, "P")
	if len(p.Methods) != 1 || describeMethod(p.Methods[0]) != (overloadMethod{"m", "P.m", FuncMethod, "string"}) {
		t.Errorf("P methods = %+v, want only m imported as P.m", p.Methods)
	}
}

// TestWebIdlShadowCodegen pins what the renamed members generate: every name
// derived from the member carries its ordinal, the parent's member is generated
// unchanged, and the glue implements each import by calling the IDL member —
// the parent's own, since the child redeclares it (#682), and the shadow's on
// the object.
func TestWebIdlShadowCodegen(t *testing.T) {
	modules := overloadIR(t, `interface Element { attribute DOMString className; };
	interface RadioNodeList {};
	interface SVGAnimatedString {};
	interface SVGElement : Element { readonly attribute SVGAnimatedString className; };
	interface HTMLCollection { getter Element? namedItem(DOMString name); };
	interface HTMLFormControlsCollection : HTMLCollection {
		getter (RadioNodeList or Element)? namedItem(DOMString name);
	};`)
	out := GeneratePromise(modules, "web")

	assertContains(t, out, "named_item(this, string name) Element? `public")
	assertContains(t, out, "named_item_2(this, string name) JsValue? `public")
	assertContains(t, out, "ref := _html_form_controls_collection_named_item_2(this._handle, name);")
	assertContains(t, out, "_html_form_controls_collection_named_item_2(i32 handle, string name) i32\n"+
		"    `extern(\"html_form_controls_collection_named_item_2\")\n"+
		"    `wasm_import(\"promise_env\", \"HTMLFormControlsCollection.namedItem.2\")")
	assertNotContains(t, out, "named_item(this, string name) JsValue?")

	assertContains(t, out, "get class_name string `public")
	assertContains(t, out, "set class_name(string value) `public")
	assertContains(t, out, "get class_name_2 SVGAnimatedString `public")
	assertContains(t, out, "_svg_element_class_name_2(i32 handle) i32\n"+
		"    `extern(\"svg_element_class_name_2\")\n"+
		"    `wasm_import(\"promise_env\", \"SVGElement.className.get\")")
	assertNotContains(t, out, "get class_name SVGAnimatedString")

	js := GenerateJSGlue(modules)
	assertContains(t, js, `"HTMLFormControlsCollection.namedItem.2"($handle, name$ptr, name$len) {`)
	assertContains(t, js, `"SVGElement.className.get"($handle) {`)
	assertContains(t, js, "_refStore(_refLoad($handle).className)")
	assertContains(t, js, `_writeStringResult($retptr, Reflect.get(Element.prototype, "className", _refLoad($handle)));`)
	assertContains(t, js, `Reflect.set(Element.prototype, "className", _readString(value$ptr, value$len), _refLoad($handle));`)
	assertContains(t, js, "Reflect.apply(HTMLCollection.prototype.namedItem, _refLoad($handle), [_readString(name$ptr, name$len)])")
	if n := strings.Count(js, "_refLoad($handle).namedItem(_readString(name$ptr, name$len))"); n != 1 {
		t.Errorf("namedItem resolved on the object by %d imports, want 1 (the shadow's)\n%s", n, js)
	}
}
