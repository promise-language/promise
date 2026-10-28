package bindgen

import (
	"reflect"
	"strings"
	"testing"
)

// #723: a type naming a definition no bound IDL file defines is a bind error
// naming the declaration, its position and the name (docs/wasm-bindings.md,
// "Supported subset"); before the fix the name was emitted verbatim and the
// module failed to compile with `undefined type`. A keep list drops what names
// it instead (TestApplyKeepList). Typedefs, CSSOMString, WindowProxy and
// [LegacyWindowAlias] resolve first, so only a name still unresolved after them
// is reported (TestWebIdlNamesOutsideIDLUnbound covers the last two).

func TestWebIdlUnboundReference(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "optional dictionary field",
			src:  `dictionary D { Missing m; long n; };`,
			want: []string{"dict.webidl:1:16: D.m names Missing, which is not defined in the bound IDL files"},
		},
		{
			name: "required dictionary field",
			src:  `dictionary D { required Missing m; };`,
			want: []string{"dict.webidl:1:16: D.m names Missing, which is not defined in the bound IDL files"},
		},
		{
			name: "parameter",
			src:  `interface A { undefined use(Missing m); };`,
			want: []string{"dict.webidl:1:15: A.use names Missing, which is not defined in the bound IDL files"},
		},
		{
			name: "return type",
			src:  `interface A { Missing find(); };`,
			want: []string{"dict.webidl:1:15: A.find names Missing, which is not defined in the bound IDL files"},
		},
		{
			name: "attribute",
			src:  `interface A { attribute Missing a; };`,
			want: []string{"dict.webidl:1:15: A.a names Missing, which is not defined in the bound IDL files"},
		},
		{
			name: "constructor parameter",
			src:  `interface A { constructor(Missing m); };`,
			want: []string{"dict.webidl:1:15: A.constructor names Missing, which is not defined in the bound IDL files"},
		},
		{
			name: "iterable value type",
			src:  `interface A { iterable<Missing>; };`,
			want: []string{"dict.webidl:1:15: A.iterable names Missing, which is not defined in the bound IDL files"},
		},
		{
			// The lowered type carries the name inside a sequence and a nullable.
			name: "sequence and nullable",
			src: `interface A {
  undefined each(sequence<Missing> s);
  Other? maybe();
};`,
			want: []string{
				"dict.webidl:2:3: A.each names Missing, which is not defined in the bound IDL files",
				"dict.webidl:3:3: A.maybe names Other, which is not defined in the bound IDL files",
			},
		},
		{
			// A declaration is reported once, by the first name it is missing.
			name: "first missing name of a declaration",
			src:  `interface A { undefined f(Missing a, Other b); };`,
			want: []string{"dict.webidl:1:15: A.f names Missing, which is not defined in the bound IDL files"},
		},
		{
			// A use of a typedef names the typedef, which is defined: only the
			// typedef is reported, at the cause.
			name: "a typedef, not its uses",
			src: `typedef Missing M;
interface A { attribute M m; };`,
			want: []string{"dict.webidl:1:1: typedef M names Missing, which is not defined in the bound IDL files"},
		},
		{
			// A mixin is not a type: nothing is generated under its name.
			name: "a mixin used as a type",
			src: `interface mixin M {};
interface A { attribute M m; };`,
			want: []string{"dict.webidl:2:15: A.m names M, which is not defined in the bound IDL files"},
		},
		{
			name: "every declaration is reported",
			src: `typedef sequence<Gone> G;
dictionary D { Missing a; };
interface A {
  attribute Other b;
  undefined c(long x, Third t);
};`,
			want: []string{
				"dict.webidl:1:1: typedef G names Gone, which is not defined in the bound IDL files",
				"dict.webidl:2:16: D.a names Missing, which is not defined in the bound IDL files",
				"dict.webidl:4:3: A.b names Other, which is not defined in the bound IDL files",
				"dict.webidl:5:3: A.c names Third, which is not defined in the bound IDL files",
			},
		},
		{
			name: "reported with the other failures",
			src: `typedef A A;
dictionary D : Gone {};
interface I { Missing m(); };`,
			want: []string{
				"dict.webidl:1:1: typedef A: cycle through A",
				"dict.webidl:3:15: I.m names Missing, which is not defined in the bound IDL files",
				"dictionary D inherits from Gone, which is not a dictionary in the bound IDL files",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			modules, err := WebIdlToIR(parseMerged(t, tc.src))
			if err == nil {
				t.Fatalf("WebIdlToIR succeeded with %d modules, want the unbound reference reported", len(modules))
			}
			if got := strings.Split(err.Error(), "\n"); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("error:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// What a reference does not reach, or reaches defined, converts.
func TestWebIdlUnboundReferenceNotReported(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			// A union lowers to JsValue, so a name inside one is no reference.
			name: "union member",
			src:  `interface A { attribute (Missing or DOMString) u; };`,
		},
		{
			name: "Promise and record",
			src: `interface A {
  Promise<Missing> later();
  undefined put(record<DOMString, Missing> table);
};`,
		},
		{
			name: "CSSOMString",
			src:  `interface A { attribute CSSOMString text; };`,
		},
		{
			// WindowProxy with Window bound, and aliases with the interfaces
			// declaring them bound.
			name: "names defined outside IDL, resolved",
			src:  outsideIDL,
		},
		{
			// A callback is defined; lowering it is #595's.
			name: "typedef, enum, callback and dictionary",
			src: `typedef DOMString Name;
enum Mode { "a", "b" };
callback CB = undefined (DOMString s);
dictionary Opts { Name label; Mode mode; };
interface A {
  attribute Name n;
  attribute Mode m;
  attribute CB? cb;
  undefined go(optional Opts o = {});
};`,
		},
		{
			// A parent is not a type reference: an unbound one is named in the
			// `is` clause and fails to compile (§"WebIDL to Promise").
			name: "unbound interface parent",
			src:  `interface C : Missing { undefined m(long a); };`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustWebIdlToIR(t, parseMerged(t, tc.src))
		})
	}
}
