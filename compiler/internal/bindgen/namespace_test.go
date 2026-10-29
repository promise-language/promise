package bindgen

import (
	"reflect"
	"strings"
	"testing"
)

// #170: a WebIDL namespace binds as a type whose every member is `global`,
// named with the namespace's first letter upper-cased, each member converted as
// an interface's static is and imported under the namespace's IDL name, which
// the glue calls (docs/wasm-bindings.md, "WebIDL to Promise", namespaces).

// namespaceFixture holds every member kind a namespace may declare, and an
// overload set.
const namespaceFixture = `namespace console {
  undefined log(any... data);
  readonly attribute DOMString label;
  const unsigned short MAX = 4;
  undefined count();
  undefined count(DOMString label);
};`

func TestWebIdlToIRNamespace(t *testing.T) {
	modules := overloadIR(t, namespaceFixture)
	if len(modules[0].Resources) != 1 {
		t.Fatalf("want one resource, got %+v", modules[0].Resources)
	}
	r := modules[0].Resources[0]
	if r.Name != "Console" || r.Parent != "" || r.Drop || r.Doc != "`console` (WebIDL namespace)" {
		t.Errorf("resource = name %q, parent %q, drop %v, doc %q; want Console, no parent, no drop, the namespace's doc line",
			r.Name, r.Parent, r.Drop, r.Doc)
	}
	type method struct {
		name, imp string
		accessor  AccessorKind
		doc       string
	}
	want := []method{
		{"log", "console.log", AccessorNone, "`console.log` (WebIDL operation)"},
		{"label", "console.label.get", AccessorGetter, "`console.label` (WebIDL attribute)"},
		{"max", "console.MAX", AccessorNone, "`console.MAX` (WebIDL constant)"},
		{"count", "console.count", AccessorNone, "`console.count` (WebIDL operation)"},
		{"count_2", "console.count.2", AccessorNone, "`console.count` (WebIDL operation)"},
	}
	var got []method
	for _, f := range r.Methods {
		if f.Kind != FuncStatic {
			t.Errorf("%s is kind %v, want FuncStatic: every namespace member is `global`", f.Name, f.Kind)
		}
		got = append(got, method{f.Name, f.ImportName, f.Accessor, f.Doc})
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("methods\n got %+v\nwant %+v", got, want)
	}
}

// A partial namespace's members are the namespace's: one type carries both, and
// a namespace already upper-case keeps its name. The namespace's comment
// documents the type.
func TestWebIdlToIRPartialNamespace(t *testing.T) {
	modules := overloadIR(t, `// CSS utilities.
namespace CSS { CSSOMString escape(CSSOMString ident); };
partial namespace CSS { double px(double value); };`)
	r := overloadResource(t, modules, "CSS")
	if r.Doc != "CSS utilities." {
		t.Errorf("CSS doc = %q, want the namespace's comment", r.Doc)
	}
	var got []string
	for _, f := range r.Methods {
		got = append(got, f.Name+" "+f.ImportName)
	}
	if want := []string{"escape CSS.escape", "px CSS.px"}; !reflect.DeepEqual(got, want) {
		t.Errorf("CSS methods = %q, want %q", got, want)
	}
	if len(modules[0].Resources) != 1 {
		t.Errorf("want one resource, got %d", len(modules[0].Resources))
	}
}

// The type keeps the borrow factory a root over a seeded namespace object
// builds it with, and declares no drop: nothing owns a namespace object.
func TestCodegenNamespace(t *testing.T) {
	out := GeneratePromise(overloadIR(t, namespaceFixture), "web")
	for _, s := range []string{
		"type Console `structural(protocol: false) `public `target(web) `doc(\"`console` (WebIDL namespace)\") {",
		"    borrow(i32 handle) Console `public `global ",
		"    log(JsValue data) `public `global `doc(\"`console.log` (WebIDL operation)\") {\n        _console_log(data._to_ref());",
		"    get label string `public `global ",
		"    count_2(string label) `public `global ",
		"`wasm_import(\"promise_env\", \"console.count.2\")",
	} {
		assertContains(t, out, s)
	}
	assertNotContains(t, out, "drop(~this)")
	assertNotContains(t, out, "[resource-drop]")
}

// The glue calls the namespace object by its IDL name — the global `console`,
// never the type's Console, which no host defines.
func TestWebIdlNamespaceGlue(t *testing.T) {
	glue := GenerateJSGlue(overloadIR(t, namespaceFixture))
	assertContains(t, glue, "    \"console.log\"(data) {\n      console.log(_refLoad(data));\n")
	assertContains(t, glue, "    \"console.count.2\"(label$ptr, label$len) {\n      console.count(_readString(label$ptr, label$len));\n")
	assertNotContains(t, glue, "Console.")
	assertNotContains(t, glue, "[resource-drop]")
}

func TestWebIdlNamespaceErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "member names an undefined type",
			src:  `namespace ns { Missing find(); };`,
			want: []string{"dict.webidl:1:16: ns.find names Missing, which is not defined in the bound IDL files"},
		},
		{
			// A namespace is not a type: nothing is generated under its name.
			name: "a namespace used as a type",
			src: `namespace console { undefined log(DOMString m); };
interface A { attribute console c; };`,
			want: []string{"dict.webidl:2:15: A.c names console, which is not defined in the bound IDL files"},
		},
		{
			name: "type name of an interface",
			src: `namespace console { undefined log(DOMString m); };
interface Console { undefined clear(); };`,
			want: []string{"dict.webidl:1:1: namespace console: generates type Console, which interface Console also names"},
		},
		{
			name: "type name of a dictionary",
			src: `dictionary Probe { long n; };
namespace probe { undefined f(); };`,
			want: []string{"dict.webidl:2:1: namespace probe: generates type Probe, which dictionary Probe also names"},
		},
		{
			name: "type name of an enum",
			src: `enum Mode { "a" };
namespace mode { undefined f(); };`,
			want: []string{"dict.webidl:2:1: namespace mode: generates type Mode, which enum Mode also names"},
		},
		{
			name: "type name of an earlier namespace",
			src: `namespace Foo { undefined f(); };
namespace foo { undefined g(); };`,
			want: []string{"dict.webidl:2:1: namespace foo: generates type Foo, which namespace Foo also names"},
		},
		{
			// A typedef, a callback or a mixin declares no Promise type, so it
			// takes no name.
			name: "reported with the other failures",
			src: `namespace console { Missing f(); };
interface Console {};
typedef long Probe;
namespace probe { undefined g(); };
callback Handler = undefined ();
namespace handler { undefined h(); };
interface mixin Shared {};
namespace shared { undefined s(); };`,
			want: []string{
				"dict.webidl:1:21: console.f names Missing, which is not defined in the bound IDL files",
				"dict.webidl:1:1: namespace console: generates type Console, which interface Console also names",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			modules, err := WebIdlToIR(parseMerged(t, tc.src))
			if err == nil {
				t.Fatalf("WebIdlToIR succeeded with %d modules, want an error", len(modules))
			}
			if got := strings.Split(err.Error(), "\n"); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("error:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}
