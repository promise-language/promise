package bindgen

import (
	"maps"
	"reflect"
	"strings"
	"testing"
)

// #722: some names webref's IDL uses as types are defined by specification
// prose or by an extended attribute rather than by an IDL definition —
// CSSOMString, WindowProxy, and each identifier a [LegacyWindowAlias] names.
// Each lowers to what it names (docs/wasm-bindings.md, "Names defined outside
// IDL"); before the fix each was emitted verbatim, an undefined type.

// outsideIDL binds Window and two interfaces carrying [LegacyWindowAlias] — one
// identifier, and a list — written as webref writes them, and uses each name
// defined outside IDL as an attribute type, a parameter, a nullable, a
// dictionary field, a typedef's target (and, through the typedef, an attribute
// type) and a constructor parameter.
const outsideIDL = `[Exposed=Window] interface Window {};
[Exposed=(Window,Worker),
 Serializable,
 LegacyWindowAlias=SVGPoint]
interface DOMPoint { attribute unrestricted double x; };
[Exposed=(Window,Worker),
 Serializable,
 LegacyWindowAlias=(SVGMatrix, WebKitCSSMatrix)]
interface DOMMatrix { readonly attribute unrestricted double a; };
dictionary Opts {
	CSSOMString text;
	required CSSOMString name;
	WindowProxy view;
	SVGMatrix transform;
};
dictionary Placement {
	required WindowProxy target;
	required SVGPoint origin;
};
typedef CSSOMString Label;
typedef WindowProxy View;
typedef SVGMatrix Transform;
[Exposed=Window]
interface A {
	constructor(SVGPoint origin, optional CSSOMString label);
	attribute CSSOMString text;
	attribute WindowProxy proxy;
	attribute SVGMatrix matrix;
	attribute Transform transform;
	readonly attribute WindowProxy? opener;
	undefined use(CSSOMString s, CSSOMString? ns, WindowProxy w, SVGPoint p, WebKitCSSMatrix k);
	SVGMatrix? getCTM();
	sequence<SVGPoint> points();
};`

// loweredSignatures maps each method of r, by import name, to its lowered
// parameter types and result: "(string, string?) -> Window?".
func loweredSignatures(r Resource) map[string]string {
	sigs := make(map[string]string)
	for _, f := range r.Methods {
		var params, results []string
		for _, p := range f.Params {
			params = append(params, promiseType(p.Type))
		}
		for _, res := range f.Results {
			results = append(results, promiseType(res))
		}
		sigs[f.ImportName] = "(" + strings.Join(params, ", ") + ") -> " + strings.Join(results, ", ")
	}
	return sigs
}

// loweredFields maps each field of the named type to its lowered type.
func loweredFields(t *testing.T, m *Module, name string) map[string]string {
	t.Helper()
	for _, ty := range m.Types {
		if ty.Name == name {
			fields := make(map[string]string)
			for _, f := range ty.Fields {
				fields[f.Name] = promiseType(f.Type)
			}
			return fields
		}
	}
	t.Fatalf("no type %q", name)
	return nil
}

func TestWebIdlNamesOutsideIDL(t *testing.T) {
	modules := mustWebIdlToIR(t, parseMerged(t, outsideIDL))

	wantA := map[string]string{
		"A.constructor": "(DOMPoint, string?) -> ",
		"A.text.get":    "() -> string",
		"A.text.set":    "(string) -> ",
		"A.proxy.get":   "() -> Window",
		"A.proxy.set":   "(Window) -> ",
		"A.matrix.get":  "() -> DOMMatrix",
		"A.matrix.set":  "(DOMMatrix) -> ",
		// A typedef inlines as its target, which lowers through the aliases.
		"A.transform.get": "() -> DOMMatrix",
		"A.transform.set": "(DOMMatrix) -> ",
		"A.opener.get":    "() -> Window?",
		"A.use":           "(string, string?, Window, DOMPoint, DOMMatrix) -> ",
		"A.getCTM":        "() -> DOMMatrix?",
		"A.points":        "() -> DOMPoint[]",
	}
	if got := loweredSignatures(overloadResource(t, modules, "A")); !maps.Equal(got, wantA) {
		t.Errorf("A lowered to\n%v\nwant\n%v", got, wantA)
	}

	wantFields := map[string]map[string]string{
		"Opts":      {"text": "string?", "name": "string", "view": "Window?", "transform": "DOMMatrix?"},
		"Placement": {"target": "Window", "origin": "DOMPoint"},
	}
	for name, want := range wantFields {
		if got := loweredFields(t, modules[0], name); !maps.Equal(got, want) {
			t.Errorf("%s fields lowered to %v, want %v", name, got, want)
		}
	}

	wantTypedefs := map[string]string{"Label": "string", "View": "Window", "Transform": "DOMMatrix"}
	gotTypedefs := make(map[string]string)
	for _, ty := range modules[0].Types {
		if ty.Kind == TypeAlias {
			gotTypedefs[ty.Name] = promiseType(*ty.Target)
		}
	}
	if !maps.Equal(gotTypedefs, wantTypedefs) {
		t.Errorf("typedefs lowered to %v, want %v", gotTypedefs, wantTypedefs)
	}
}

// An alias and its interface are one type, so a redeclaration that spells the
// other is exactly the inherited member: an override that keeps its name
// (docs/wasm-bindings.md, "Shadowed Members").
func TestWebIdlWindowAliasRedeclarationOverrides(t *testing.T) {
	modules := overloadIR(t, `[LegacyWindowAlias=SVGMatrix] interface DOMMatrix {};
	interface P { attribute DOMMatrix m; SVGMatrix f(DOMMatrix x); };
	interface C : P { attribute SVGMatrix m; DOMMatrix f(SVGMatrix x); };`)
	var got []string
	for _, f := range overloadResource(t, modules, "C").Methods {
		got = append(got, f.Name)
	}
	if want := []string{"m", "m", "f"}; !reflect.DeepEqual(got, want) {
		t.Errorf("C's members are named %q, want %q (overrides, no ordinal)", got, want)
	}
}

// A keep list that drops Window and the aliased interfaces drops what names
// them through a name defined outside IDL, reporting the definition it names.
// CSSOMString names no definition, so what uses it stays.
func TestKeepListNamesOutsideIDL(t *testing.T) {
	m, drops := keepIR(t, outsideIDL, "A\nA.text\nA.proxy\nA.matrix\nA.opener\nA.use\nA.getCTM\nA.points\n")

	wantDrops := []KeepDrop{
		{What: "typedef View", Ref: "Window"},
		{What: "typedef Transform", Ref: "DOMMatrix"},
		{What: "dictionary Placement", Ref: "Window"},
		{What: "Opts.view", Ref: "Window"},
		{What: "Opts.transform", Ref: "DOMMatrix"},
		{What: "A.proxy", Ref: "Window"},
		{What: "A.matrix", Ref: "DOMMatrix"},
		{What: "A.opener", Ref: "Window"},
		{What: "A.use", Ref: "Window"},
		{What: "A.getCTM", Ref: "DOMMatrix"},
		{What: "A.points", Ref: "DOMPoint"},
	}
	if !reflect.DeepEqual(drops, wantDrops) {
		t.Errorf("drops = %+v\nwant %+v", drops, wantDrops)
	}

	resources, _, types := keepShape(m)
	wantResources := map[string][]string{"A": {"A.text.get", "A.text.set"}}
	if !reflect.DeepEqual(resources, wantResources) {
		t.Errorf("resources = %v, want %v", resources, wantResources)
	}
	wantTypes := map[string][]string{"Opts": {"text", "name"}, "Label": nil}
	if !reflect.DeepEqual(types, wantTypes) {
		t.Errorf("types = %v, want %v", types, wantTypes)
	}
}

// A keep list that keeps the interfaces the names defined outside IDL name keeps
// what names them, and the members it keeps still lower to those interfaces:
// Merge recorded the aliases from the whole input, and reducing the file does
// not lose them.
func TestKeepListKeepsWhatNamesAKeptDefinition(t *testing.T) {
	m, drops := keepIR(t, outsideIDL, "A\nA.matrix\nA.getCTM\nA.points\nDOMMatrix\nDOMPoint\nWindow\n")
	if len(drops) != 0 {
		t.Errorf("drops = %+v, want none: every definition the input names is kept", drops)
	}

	want := map[string]string{
		"A.matrix.get": "() -> DOMMatrix",
		"A.matrix.set": "(DOMMatrix) -> ",
		"A.getCTM":     "() -> DOMMatrix?",
		"A.points":     "() -> DOMPoint[]",
	}
	if got := loweredSignatures(overloadResource(t, []*Module{m}, "A")); !maps.Equal(got, want) {
		t.Errorf("A lowered to %v, want %v", got, want)
	}
	if got, want := loweredFields(t, m, "Opts")["transform"], "DOMMatrix?"; got != want {
		t.Errorf("Opts.transform lowered to %s, want %s", got, want)
	}
}

// WindowProxy names Window whether or not Window is bound, so an unbound Window
// is the name left unresolved. An alias is known only from the bound IDL: with
// the interface declaring it unbound, the identifier is its own name.
func TestWebIdlNamesOutsideIDLUnbound(t *testing.T) {
	modules := mustWebIdlToIR(t, parseMerged(t, `[Exposed=Window] interface A {
		readonly attribute WindowProxy? opener;
		readonly attribute SVGMatrix m;
	};`))
	want := map[string]string{
		"A.opener.get": "() -> Window?",
		"A.m.get":      "() -> SVGMatrix",
	}
	if got := loweredSignatures(overloadResource(t, modules, "A")); !maps.Equal(got, want) {
		t.Errorf("A lowered to %v, want %v", got, want)
	}
}
