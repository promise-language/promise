package regress11

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1759: getOrEmitViewVtable takes the view's FULL type as a trailing argument and
// substitutes with it, but three of its five callers — the box helpers — never passed
// it. So a value type boxed into a generic interface built its adapter against the
// UNSUBSTITUTED interface signature: `T?` resolved to `{ i1, ptr }` while the concrete
// returned `{ i1, i64 }`, and `opt` rejected the module. The same omission collapsed
// the vtable cache key and the adapter name, so one concrete boxed into two
// instantiations of one generic view would have shared a vtable and emitted two
// definitions under one symbol.

func TestT1759_ValueTypeBoxedIntoGenericViewSubstitutesTheAdapterSignature(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Countdown is Iterator[int] {
			int n `+"`"+`value;
			next(~this) int? {
				if (this.n <= 0) { return none; }
				this.n = this.n - 1;
				return this.n + 1;
			}
		}
		main() {
			Iterator[int] it = Countdown(n: 2);
			int c = it.count();
		}
	`)
	// The adapter's LLVM return type is the SUBSTITUTED `int?`, not the unbound `T?`.
	if !strings.Contains(ir, `define { i1, i64 } @"Countdown.next$view_adapt_as_Iterator[int]"`) {
		t.Fatalf("adapter must be built against the substituted interface signature; got:\n%s",
			codegentest.FindDefinedFunc(ir, `view_adapt_as_Iterator`))
	}
	// The view's type args reach the vtable global's name too.
	if !strings.Contains(ir, `@"promise_vtable_Countdown_as_Iterator[int]"`) {
		t.Fatalf("view vtable must be keyed by the view's instantiation")
	}
}

// A heap concrete reaching a generic view through a second `is` parent already worked
// (coerceToView's general arm passed the view type) — keep it working.
func TestT1759_HeapConcreteSecondParentGenericView(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base { get id int => 1; }
		type Producer[T] `+"`"+`structural {
			produce(this) T `+"`"+`abstract;
		}
		type Countdown is Base, Producer[int] {
			int n;
			produce(this) int { return this.n; }
		}
		main() {
			Producer[int] it = Countdown(n: 2);
			int v = it.produce();
		}
	`)
	if !strings.Contains(ir, `define i64 @"Countdown.produce$view_adapt_as_Producer[int]"`) {
		t.Fatalf("adapter must return the substituted i64; got:\n%s",
			codegentest.FindDefinedFunc(ir, `view_adapt_as_Producer`))
	}
}

// Two instantiations of one generic view over one concrete must get two vtables and
// two adapters. A view type parameter that appears in no requirement ("phantom") is
// the shape that makes this reachable: the concrete satisfies every instantiation.
// The requirement is failable and the concrete's method is not, so both slots need a
// real adapter — which is what makes the name collision reachable.
func TestT1759_TwoViewInstantiationsGetDistinctVtablesAndAdapters(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Marker[T] `+"`"+`structural {
			tag!(this) int `+"`"+`abstract;
		}
		type Thing {
			int n `+"`"+`value;
			tag(this) int => this.n;
		}
		main() {
			Marker[int] a = Thing(n: 1);
			Marker[string] b = Thing(n: 2);
			int x = a.tag()?!;
			int y = b.tag()?!;
		}
	`)
	for _, want := range []string{
		`@"promise_vtable_Thing_as_Marker[int]"`,
		`@"promise_vtable_Thing_as_Marker[string]"`,
	} {
		if !strings.Contains(ir, want) {
			t.Fatalf("expected a distinct view vtable %s", want)
		}
	}
	// The synthesized per-slot symbols must be distinct too, or the second
	// instantiation redefines the first's.
	for _, want := range []string{
		`$view_adapt_as_Marker[int]"`,
		`$view_adapt_as_Marker[string]"`,
	} {
		if !strings.Contains(ir, want) {
			t.Fatalf("expected a distinct adapter symbol ending %s", want)
		}
	}
}

// A non-generic view must keep its existing spelling — the cache key is the bare
// interface name there, so no symbol churn.
func TestT1759_NonGenericViewNamesAreUnchanged(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display(42); }
	`)
	if !strings.Contains(ir, "@promise_vtable_int_as_Showable") {
		t.Fatalf("non-generic view vtable name changed")
	}
	if !strings.Contains(ir, "int.to_string$view_adapt_as_Showable") {
		t.Fatalf("non-generic view adapter name changed")
	}
}
