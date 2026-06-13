package codegen

import (
	"testing"
)

// T0862: Coercing a non-generic concrete type that implements a *generic*
// `structural` interface instance (e.g. `is Box[int]`) into the interface view
// and then calling a *default* method through that view segfaulted at runtime
// ("fatal: segmentation fault at 0x0").
//
// A non-generic structural interface compiles its own default methods, which
// concrete implementors reference directly in their vtable. A *generic*
// interface instance does not: the mono pipeline skips structural instances, so
// no default body is ever emitted for the concrete implementor's vtable slot.
// The slot was left null and dispatch through the view jumped to address 0.
//
// The fix (declare/defineGenericStructuralDefaults) synthesizes the inherited
// defaults onto the non-generic concrete type *before* its vtable is built, so
// the per-concrete name (e.g. Counter.put_two) fills the slot.
func TestT0862GenericStructuralDefaultThroughView(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] `+"`"+`structural {
			put!(~this, T x) T `+"`"+`abstract;
			put_two!(~this, T x) T {
				return this.put(x);
			}
			put_three!(~this, T x) T {
				return this.put_two(x);
			}
		}
		type Counter is Box[int] {
			int n;
			put!(~this, int x) int {
				return x + this.n;
			}
		}
		main() {
			Counter ct = Counter(n: 0);
			Box[int] b = ct;
			b.put_three(5)?!;
		}
	`)
	// The inherited defaults must be synthesized on the non-generic concrete
	// type (with the correct int-substituted ABI), not on the skipped generic
	// instance Box__int.
	if extractFunction(ir, "Counter.put_two") == "" {
		t.Error("expected synthesized Counter.put_two to be emitted")
	}
	p3 := extractFunction(ir, "Counter.put_three")
	if p3 == "" {
		t.Fatal("expected synthesized Counter.put_three to be emitted")
	}
	// Chained default must dispatch to the sibling default by the concrete name.
	assertContains(t, p3, "@Counter.put_two")
	// The concrete type's vtable (used for dispatch through the Box[int] view)
	// must reference the synthesized defaults in its slots — the pre-fix bug left
	// these slots null, so dispatch through the view jumped to address 0.
	ctVtable := extractGlobal(ir, "promise_vtable_Counter")
	if ctVtable == "" {
		t.Fatal("expected @promise_vtable_Counter to be emitted")
	}
	assertContains(t, ctVtable, "@Counter.put_two")
	assertContains(t, ctVtable, "@Counter.put_three")
}

// T0862 (transitive interface inheritance): a generic structural interface that
// itself inherits another generic structural interface (Derived[T] is Base[T]).
// The default inherited transitively through the grandparent (Base.base_default)
// must have its type param resolved to the concrete arg (int). Before the
// mergeParentSubst fix in forEachConcreteGenericStructuralParent, the recursion
// passed Derived's substitution unchanged, leaving Base.T unsubstituted — the
// returned `T` resolved to the structural fat-pointer type and codegen crashed
// in wrapOk ("insertvalue elem type mismatch, expected i64, got {i1,i8*,i8*}").
func TestT0862TransitiveGenericInterfaceDefault(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] `+"`"+`structural {
			base_op!(~this, T x) T `+"`"+`abstract;
			base_default!(~this, T x) T { return this.base_op(x); }
		}
		type Derived[T] is Base[T] `+"`"+`structural {
			derived_default!(~this, T x) T { return this.base_default(x); }
		}
		type Impl is Derived[int] {
			int k;
			base_op!(~this, int x) int { return x + this.k; }
		}
		main() {
			Impl im = Impl(k: 3);
			Derived[int] d = im;
			d.derived_default(5)?!;
		}
	`)
	// Both the directly-inherited default and the transitively-inherited one
	// must be synthesized on the concrete type with the int-substituted ABI.
	dd := extractFunction(ir, "Impl.derived_default")
	if dd == "" {
		t.Fatal("expected synthesized Impl.derived_default to be emitted")
	}
	bd := extractFunction(ir, "Impl.base_default")
	if bd == "" {
		t.Fatal("expected synthesized Impl.base_default (transitive) to be emitted")
	}
	// base_default returns T = int — the substituted body must compute on i64,
	// not produce/return a structural fat pointer {i1, i8*, i8*}.
	assertContains(t, bd, "i64")
	assertNotContains(t, bd, "{ i1, i8*, i8* }")
	// derived_default chains to base_default by the concrete name.
	assertContains(t, dd, "@Impl.base_default")
	// The concrete vtable used for dispatch through the Derived[int] view must
	// reference both synthesized defaults.
	vt := extractGlobal(ir, "promise_vtable_Impl")
	if vt == "" {
		t.Fatal("expected @promise_vtable_Impl to be emitted")
	}
	assertContains(t, vt, "@Impl.derived_default")
	assertContains(t, vt, "@Impl.base_default")
}

// T0862 (override): when the concrete type defines its own copy of an inherited
// default, no synthesized default is emitted for it — the concrete method fills
// the vtable slot directly (exercises the hasOwnMethod skip).
func TestT0862ConcreteOverridesDefault(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] `+"`"+`structural {
			put!(~this, T x) T `+"`"+`abstract;
			put_two!(~this, T x) T { return this.put(x); }
		}
		type Override is Box[int] {
			int n;
			put!(~this, int x) int { return x; }
			put_two!(~this, int x) int { return x + 100; }
		}
		main() {
			Override o = Override(n: 0);
			Box[int] b = o;
			b.put_two(5)?!;
		}
	`)
	pt := extractFunction(ir, "Override.put_two")
	if pt == "" {
		t.Fatal("expected Override.put_two to be emitted")
	}
	// The body must be the user's override (x + 100), not the synthesized
	// default that dispatches to put().
	assertContains(t, pt, "add")
	vt := extractGlobal(ir, "promise_vtable_Override")
	if vt == "" {
		t.Fatal("expected @promise_vtable_Override to be emitted")
	}
	assertContains(t, vt, "@Override.put_two")
}

// T0862 (generic default method): a generic structural interface may have a
// default method with its own type param (wrap[U]). Such a method is not
// virtual and is skipped by the synthesis path (the md.TypeParams>0 guard); the
// regular non-generic defaults are still synthesized and fill the vtable.
func TestT0862GenericDefaultMethodSkipped(t *testing.T) {
	ir := generateIR(t, `
		type GBox[T] `+"`"+`structural {
			put!(~this, T x) T `+"`"+`abstract;
			put_two!(~this, T x) T { return this.put(x); }
			wrap[U](~this, U y) U { return y; }
		}
		type GCounter is GBox[int] {
			int n;
			put!(~this, int x) int { return x; }
		}
		main() {
			GCounter ct = GCounter(n: 0);
			GBox[int] b = ct;
			b.put_two(5)?!;
		}
	`)
	// The non-generic default is synthesized for the concrete type.
	if extractFunction(ir, "GCounter.put_two") == "" {
		t.Error("expected synthesized GCounter.put_two to be emitted")
	}
	// The generic default method must NOT be synthesized as a per-concrete stub
	// (it is monomorphized on demand at explicit call sites instead).
	if extractFunction(ir, "GCounter.wrap") != "" {
		t.Error("generic default method GCounter.wrap should not be synthesized")
	}
	vt := extractGlobal(ir, "promise_vtable_GCounter")
	if vt == "" {
		t.Fatal("expected @promise_vtable_GCounter to be emitted")
	}
	assertContains(t, vt, "@GCounter.put_two")
}
