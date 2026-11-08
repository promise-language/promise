package bindgen

import (
	"fmt"
	"testing"

	"github.com/promise-language/promise/compiler/internal/wit"
)

// dictionaryLine is the declaration line of the WebIDL dictionary name, with
// the module's target when target is not empty.
func dictionaryLine(name, target string) string {
	annot := ""
	if target != "" {
		annot = " `target(" + target + ")"
	}
	return fmt.Sprintf("type %s `structural(protocol: false) `public%s `doc(\"`%s` (WebIDL dictionary)\") {", name, annot, name)
}

// TestTargetedDictionaries: a dictionary carries the module's target exactly
// when a member names a type that exists only there — an interface wrapper, or
// another dictionary that carries the target — directly or inside an optional
// or a sequence, inherited or not, in any declaration order and through a
// cycle. Every other dictionary carries none, so it compiles on every target
// (docs/wasm-bindings.md §"Generated Module Contents", #810).
func TestTargetedDictionaries(t *testing.T) {
	cases := []struct {
		name     string
		idl      string
		targeted []string // dictionaries whose line carries `target(web)
		plain    []string // dictionaries whose line carries no target
	}{
		{
			name:     "a required interface member",
			idl:      "interface Node {};\ndictionary D { required Node n; };",
			targeted: []string{"D"},
		},
		{
			name:     "an optional interface member",
			idl:      "interface Node {};\ndictionary D { Node n; };",
			targeted: []string{"D"},
		},
		{
			name:     "a nullable interface member",
			idl:      "interface Node {};\ndictionary D { Node? n; };",
			targeted: []string{"D"},
		},
		{
			name:     "a sequence of an interface",
			idl:      "interface Node {};\ndictionary D { sequence<Node> ns; };",
			targeted: []string{"D"},
		},
		{
			name:     "a FrozenArray of an interface",
			idl:      "interface Node {};\ndictionary D { FrozenArray<Node> ns; };",
			targeted: []string{"D"},
		},
		{
			name:     "a typedef of an interface",
			idl:      "interface Node {};\ntypedef Node NodeAlias;\ndictionary D { NodeAlias n; };",
			targeted: []string{"D"},
		},
		{
			name:     "a dictionary naming a targeted dictionary declared after it",
			idl:      "interface Node {};\ndictionary Outer { Inner i; };\ndictionary Inner { Node n; };",
			targeted: []string{"Outer", "Inner"},
		},
		{
			name:     "a dictionary naming a targeted dictionary declared before it",
			idl:      "interface Node {};\ndictionary Inner { Node n; };\ndictionary Outer { Inner i; };",
			targeted: []string{"Outer", "Inner"},
		},
		{
			name:     "a dictionary inheriting an interface member",
			idl:      "interface Node {};\ndictionary Base { Node n; };\ndictionary Derived : Base { long x; };",
			targeted: []string{"Base", "Derived"},
		},
		{
			name:     "a cycle through a dictionary naming an interface",
			idl:      "interface Node {};\ndictionary A { B b; };\ndictionary B { A a; Node n; };",
			targeted: []string{"A", "B"},
		},
		{
			name:  "a cycle naming no interface",
			idl:   "interface Node {};\ndictionary A { B b; };\ndictionary B { A a; };",
			plain: []string{"A", "B"},
		},
		{
			name:  "members naming nothing bound to the target",
			idl:   "interface Node {};\nenum Mode { \"open\", \"closed\" };\ndictionary P { long x; DOMString s; Mode m; any v; (Node or long) u; };",
			plain: []string{"P"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := GeneratePromise(mustWebIdlToIR(t, parseMerged(t, tc.idl)), "web")
			for _, name := range tc.targeted {
				assertContains(t, out, dictionaryLine(name, "web"))
			}
			for _, name := range tc.plain {
				assertContains(t, out, dictionaryLine(name, ""))
			}
		})
	}
}

// TestTargetedNeverEnums: an enum names no type, so neither a WebIDL enum nor
// the JsValue enum carries a target, beside a dictionary that does.
func TestTargetedNeverEnums(t *testing.T) {
	idl := "interface Node {};\nenum Mode { \"open\" };\ndictionary D { Node n; Mode m; any v; };"
	out := GeneratePromise(mustWebIdlToIR(t, parseMerged(t, idl)), "web")
	assertContains(t, out, dictionaryLine("D", "web"))
	assertContains(t, out, "enum Mode `public `doc(\"`Mode` (WebIDL enum)\") {")
	assertContains(t, out, "enum JsValue `public `doc(\"Represents a dynamic JavaScript value.\") {")
}

// TestTargetedWitTypes: the rule is the shared generator's, so a WIT record or
// variant naming a resource carries the module's target — whatever `-target`
// names — and a record naming none carries no target.
func TestTargetedWitTypes(t *testing.T) {
	src := `package test:things;

interface api {
    resource descriptor {
        size: func() -> u64;
    }

    record holder {
        d: descriptor,
        n: u32,
    }

    record nested {
        h: option<holder>,
    }

    variant pick {
        one(descriptor),
        empty,
    }

    record plain {
        x: u32,
        y: u32,
    }
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	out := GeneratePromise(WitToIR(file), "wasi")
	assertContains(t, out, "type Descriptor `structural(protocol: false) `public `target(wasi) {")
	assertContains(t, out, "type Holder `structural(protocol: false) `public `target(wasi) {")
	assertContains(t, out, "type Nested `structural(protocol: false) `public `target(wasi) {")
	assertContains(t, out, "enum Pick `public `target(wasi) {")
	assertContains(t, out, "type Plain `structural(protocol: false) `public {")
}

// witRecordLine and witVariantLine are the declaration lines of a WIT record
// and variant bound for wasi, with the target when targeted.
func witRecordLine(name string, targeted bool) string {
	if targeted {
		return "type " + name + " `structural(protocol: false) `public `target(wasi) {"
	}
	return "type " + name + " `structural(protocol: false) `public {"
}

func witVariantLine(name string, targeted bool) string {
	if targeted {
		return "enum " + name + " `public `target(wasi) {"
	}
	return "enum " + name + " `public {"
}

// TestTargetedWitShapes: a WIT type naming a resource carries the target
// whatever shape names it — a list, either side of a result, a tuple or an
// owned handle — and so does a variant whose payload is a record carrying
// it, and a record naming such a variant. A variant naming no resource carries
// none (docs/wasm-bindings.md §"Generated Module Contents").
func TestTargetedWitShapes(t *testing.T) {
	cases := []struct {
		name  string
		items string   // WIT items declared beside resource descriptor
		lines []string // declaration lines the module must hold
	}{
		{
			name:  "a list of a resource",
			items: "record r { ds: list<descriptor> }",
			lines: []string{witRecordLine("R", true)},
		},
		{
			name:  "a result whose ok names a resource",
			items: "record r { x: result<descriptor, u32> }",
			lines: []string{witRecordLine("R", true)},
		},
		{
			name:  "a result whose error names a resource",
			items: "record r { x: result<u32, descriptor> }",
			lines: []string{witRecordLine("R", true)},
		},
		{
			name:  "a tuple naming a resource",
			items: "record r { x: tuple<u32, descriptor> }",
			lines: []string{witRecordLine("R", true)},
		},
		{
			name:  "an owned handle",
			items: "record r { x: own<descriptor> }",
			lines: []string{witRecordLine("R", true)},
		},
		{
			name:  "a variant carrying a targeted record",
			items: "variant v { h(holder), empty }\n    record holder { d: descriptor }",
			lines: []string{witVariantLine("V", true), witRecordLine("Holder", true)},
		},
		{
			name:  "a record naming a targeted variant",
			items: "record r { pick: v }\n    variant v { one(descriptor), empty }",
			lines: []string{witRecordLine("R", true), witVariantLine("V", true)},
		},
		{
			name:  "a variant naming no resource",
			items: "variant v { n(u32), s(string), empty }",
			lines: []string{witVariantLine("V", false)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "package test:things;\n\ninterface api {\n    resource descriptor {\n        size: func() -> u64;\n    }\n\n    " +
				tc.items + "\n}\n"
			file, errs := wit.Parse(src, "test.wit")
			if len(errs) > 0 {
				t.Fatalf("parse errors: %v", errs)
			}
			out := GeneratePromise(WitToIR(file), "wasi")
			for _, line := range tc.lines {
				assertContains(t, out, line)
			}
		})
	}
}

// TestTargetedAcrossWitInterfaces: a record naming a resource another
// interface defines, through `use`, carries the target — the rule spans every
// module generated into the one file, not only the interface declaring it.
func TestTargetedAcrossWitInterfaces(t *testing.T) {
	src := `package test:things;

interface files {
    resource descriptor {
        size: func() -> u64;
    }
}

interface api {
    use files.{descriptor};

    record holder {
        d: descriptor,
    }

    record plain {
        x: u32,
    }
}
`
	file, errs := wit.Parse(src, "test.wit")
	if len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	out := GeneratePromise(WitToIR(file), "wasi")
	assertContains(t, out, witRecordLine("Holder", true))
	assertContains(t, out, witRecordLine("Plain", false))
}
