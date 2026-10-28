package bindgen

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/wasmweb"
)

// #684: a derived wrapper is an `is` child of its parent, so an instance member
// an ancestor declares under a factory's name is a slot the child's `global
// factory of that name would take over as an override — and the module failed
// to compile with "cannot override inherited method 'create' from P". The
// factory falls back to its `_handle` name when the type has such a member, its
// own or inherited (hasMemberNamed), resolved through the module's resources.

func TestWebIdlInheritedMemberMovesFactory(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		want   []string // each must appear
		absent []string // none may appear
	}{
		{
			// The item's own case, with an overload: the ordinal goes on the
			// fallback stem. P has no constructor, so no owning factory of its
			// own, and neither declares nor inherits a `borrow`.
			name: "inherited create",
			src: `interface P { undefined create(DOMString s); };
			interface C : P { constructor(long x); constructor(DOMString s); };`,
			want: []string{
				createFactoryLine("C", "create_handle", "i32 x"),
				createFactoryLine("C", "create_handle_2", "string s"),
				borrowFactoryLine("P", "borrow"),
				borrowFactoryLine("C", "borrow"),
			},
			absent: []string{
				createFactoryLine("C", "create", "i32 x"),
				createFactoryLine("C", "create_2", "string s"),
			},
		},
		{
			// A grandparent's member reaches every interface below it; an
			// unrelated root keeps the default name.
			name: "grandparent borrow",
			src: `interface G { undefined borrow(DOMString s); };
			interface P : G {};
			interface C : P {};
			interface Free {};`,
			want: []string{
				borrowFactoryLine("G", "borrow_handle"),
				borrowFactoryLine("P", "borrow_handle"),
				borrowFactoryLine("C", "borrow_handle"),
				borrowFactoryLine("Free", "borrow"),
			},
			absent: []string{
				borrowFactoryLine("P", "borrow"),
				borrowFactoryLine("C", "borrow"),
				borrowFactoryLine("Free", "borrow_handle"),
			},
		},
		{
			// Inheritance runs one way: a member C declares moves C's
			// factories, never those of its parent P or its sibling S.
			name: "derived member stays below",
			src: `interface P { constructor(); };
			interface C : P {
				constructor(long x);
				undefined create(DOMString s);
				undefined borrow(DOMString s);
			};
			interface S : P { constructor(); };`,
			want: []string{
				createFactoryLine("P", "create", ""),
				borrowFactoryLine("P", "borrow"),
				createFactoryLine("C", "create_handle", "i32 x"),
				borrowFactoryLine("C", "borrow_handle"),
				createFactoryLine("S", "create", ""),
				borrowFactoryLine("S", "borrow"),
			},
			absent: []string{
				createFactoryLine("P", "create_handle", ""),
				borrowFactoryLine("P", "borrow_handle"),
				createFactoryLine("S", "create_handle", ""),
				borrowFactoryLine("S", "borrow_handle"),
			},
		},
		{
			// An inherited attribute is a getter slot like any instance member.
			name: "inherited attribute",
			src: `interface P { readonly attribute DOMString borrow; };
			interface C : P {};`,
			want: []string{
				borrowFactoryLine("C", "borrow_handle"),
			},
			absent: []string{
				borrowFactoryLine("C", "borrow"),
			},
		},
		{
			// A static and a constant are `global: the parent's own factories
			// move aside from them, but a child's `global factory hides them,
			// so the child keeps the default names.
			name: "inherited static and constant",
			src: `interface P {
				constructor();
				static undefined create(DOMString s);
				const long borrow = 1;
			};
			interface C : P { constructor(long x); };`,
			want: []string{
				createFactoryLine("P", "create_handle", ""),
				borrowFactoryLine("P", "borrow_handle"),
				createFactoryLine("C", "create", "i32 x"),
				borrowFactoryLine("C", "borrow"),
			},
			absent: []string{
				createFactoryLine("C", "create_handle", "i32 x"),
				borrowFactoryLine("C", "borrow_handle"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := GeneratePromise(overloadIR(t, tc.src), "web")
			for _, want := range tc.want {
				assertContains(t, out, want)
			}
			for _, bad := range tc.absent {
				assertNotContains(t, out, bad)
			}
		})
	}
}

// The chain is resolved among the module's resources, so it ends at a parent the
// module does not define — the `is` clause still names it and the module fails
// to compile with `undefined type` — and at an inheritance cycle, which reaches
// codegen from IDL (`interface A : B {}; interface B : A {};`) and must not make
// the walk loop forever.
func TestCodegenFactoryNameAncestorChainEnds(t *testing.T) {
	borrow := Func{Name: "borrow", Kind: FuncMethod, ImportName: "A.borrow"}
	cases := []struct {
		name      string
		resources []Resource
		want      []string
	}{
		{
			name:      "unbound parent",
			resources: []Resource{{Name: "C", Parent: "Missing", Drop: true}},
			want:      []string{borrowFactoryLine("C", "borrow")},
		},
		{
			// A declares borrow itself; B reaches it through the cycle's one edge.
			name: "cycle",
			resources: []Resource{
				{Name: "A", Parent: "B", Drop: true, Methods: []Func{borrow}},
				{Name: "B", Parent: "A", Drop: true},
			},
			want: []string{borrowFactoryLine("A", "borrow_handle"), borrowFactoryLine("B", "borrow_handle")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := GeneratePromise([]*Module{{Name: "test", ImportModule: wasmweb.ImportModule, Resources: tc.resources}}, "web")
			for _, want := range tc.want {
				assertContains(t, out, want)
			}
		})
	}
}
