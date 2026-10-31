package bindgen

import (
	"reflect"
	"strings"
	"testing"
)

// #721: a typedef names no type. Each use of one lowers as its target written in
// its place (docs/wasm-bindings.md, "WebIDL to Promise", typedefs); before the
// fix the use kept the typedef's name, and the module failed to compile with
// `undefined type`.

// inlinedShape is what a module binds once its typedefs are inlined: its
// resources and every type but the aliases, which only document a typedef.
func inlinedShape(m *Module) ([]Resource, []Type) {
	var types []Type
	for _, t := range m.Types {
		if t.Kind != TypeAlias {
			types = append(types, t)
		}
	}
	return m.Resources, types
}

func TestWebIdlTypedefInlinedAsTarget(t *testing.T) {
	cases := []struct {
		name     string
		typedefs string // IDL using typedefs
		inline   string // the same IDL with each typedef's target written in its place
	}{
		{
			name: "primitive typedef as an attribute, a parameter, a return type and a constant",
			typedefs: `typedef DOMString Name;
			typedef long Count;
			interface A {
				constructor(Name first);
				attribute Name label;
				Name rename(Name to);
				const Count LIMIT = 3;
			};`,
			inline: `interface A {
				constructor(DOMString first);
				attribute DOMString label;
				DOMString rename(DOMString to);
				const long LIMIT = 3;
			};`,
		},
		{
			name: "union typedef as a parameter",
			typedefs: `typedef (DOMString or long) NameOrIndex;
			interface A { undefined pick(NameOrIndex which); };`,
			inline: `interface A { undefined pick((DOMString or long) which); };`,
		},
		{
			name: "typedef of a typedef",
			typedefs: `typedef long Index;
			typedef Index Position;
			interface A { attribute Position at; };`,
			inline: `interface A { attribute long at; };`,
		},
		{
			name: "nullable use",
			typedefs: `typedef DOMString Name;
			interface A { attribute Name? label; };`,
			inline: `interface A { attribute DOMString? label; };`,
		},
		{
			name: "typedef of a nullable type",
			typedefs: `typedef DOMString? MaybeName;
			interface A { attribute MaybeName label; };`,
			inline: `interface A { attribute DOMString? label; };`,
		},
		{
			// WebIDL forbids a nullable of a nullable type, so the use's `?`
			// adds nothing to a target that is nullable already, however many
			// typedefs away.
			name: "nullable use of a nullable typedef stays nullable once",
			typedefs: `typedef DOMString? MaybeName;
			typedef MaybeName AlsoMaybeName;
			interface A {
				attribute MaybeName? label;
				attribute AlsoMaybeName? other;
			};`,
			inline: `interface A {
				attribute DOMString? label;
				attribute DOMString? other;
			};`,
		},
		{
			name: "inside optional, sequence, FrozenArray and ObservableArray",
			typedefs: `typedef DOMString Name;
			interface A {
				undefined take(optional Name n);
				sequence<Name> names();
				FrozenArray<Name> frozen();
				attribute ObservableArray<Name> observed;
			};`,
			inline: `interface A {
				undefined take(optional DOMString n);
				sequence<DOMString> names();
				FrozenArray<DOMString> frozen();
				attribute ObservableArray<DOMString> observed;
			};`,
		},
		{
			name: "dictionary fields",
			typedefs: `typedef DOMString Name;
			typedef (DOMString or long) NameOrIndex;
			dictionary D {
				required NameOrIndex which;
				Name label;
			};`,
			inline: `dictionary D {
				required (DOMString or long) which;
				DOMString label;
			};`,
		},
		{
			name: "interface typedef as a return type",
			typedefs: `interface Event {};
			typedef Event EventAlias;
			interface A {
				EventAlias get();
				EventAlias? find();
			};`,
			inline: `interface Event {};
			interface A {
				Event get();
				Event? find();
			};`,
		},
		{
			name: "typedef declared after its use",
			typedefs: `interface A { attribute Name label; };
			typedef DOMString Name;`,
			inline: `interface A { attribute DOMString label; };`,
		},
		{
			// A name inside a union is not followed — the union lowers to
			// JsValue — so this is no cycle.
			name: "typedef inside its own union",
			typedefs: `typedef (U or long) U;
			interface A { attribute U u; };`,
			inline: `interface A { attribute (DOMString or long) u; };`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotResources, gotTypes := inlinedShape(mustWebIdlToIR(t, parseMerged(t, tc.typedefs))[0])
			wantResources, wantTypes := inlinedShape(mustWebIdlToIR(t, parseMerged(t, tc.inline))[0])
			if !reflect.DeepEqual(gotResources, wantResources) {
				t.Errorf("resources:\n got %+v\nwant %+v", gotResources, wantResources)
			}
			if !reflect.DeepEqual(gotTypes, wantTypes) {
				t.Errorf("types:\n got %+v\nwant %+v", gotTypes, wantTypes)
			}
		})
	}
}

// A typedef that refers to itself cannot be inlined: each typedef on the cycle
// is a bind error at its position, reported with every other failure, and the
// conversion ends.
func TestWebIdlTypedefCycle(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string // the error's lines, in order
	}{
		{
			name: "self through a sequence",
			src: `typedef sequence<A> A;
interface I { attribute A a; };`,
			want: []string{"dict.webidl:1:1: typedef A: cycle through A"},
		},
		{
			name: "two typedefs",
			src: `typedef B A;
typedef A? B;
interface I { attribute A a; };`,
			want: []string{
				"dict.webidl:1:1: typedef A: cycle through B",
				"dict.webidl:2:1: typedef B: cycle through A",
			},
		},
		{
			// C names the cycle without being on it, so only A and B are
			// reported; C's use ends at the name A.
			name: "a typedef naming a cycle is not on it",
			src: `typedef B A;
typedef A B;
typedef sequence<A> C;
interface I { attribute C c; };`,
			want: []string{
				"dict.webidl:1:1: typedef A: cycle through B",
				"dict.webidl:2:1: typedef B: cycle through A",
			},
		},
		{
			name: "reported with a broken dictionary",
			src: `dictionary D : Missing {};
typedef A A;`,
			want: []string{
				"dict.webidl:2:1: typedef A: cycle through A",
				"dictionary D inherits from Missing, which is not a dictionary in the bound IDL files",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			modules, err := WebIdlToIR(parseMerged(t, tc.src))
			if err == nil {
				t.Fatalf("WebIdlToIR succeeded with %d modules, want the cycle error", len(modules))
			}
			if got := strings.Split(err.Error(), "\n"); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("error:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}
