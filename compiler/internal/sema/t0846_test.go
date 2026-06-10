package sema

import "testing"

// T0846: `drop` is the compiler-managed destructor; calling it explicitly is
// rejected uniformly in sema (previously: codegen panics for Vector/MutexGuard,
// silent double-free for heap user types).

func TestT0846ExplicitDropOnMutexGuardRejected(t *testing.T) {
	errs := checkErrs(t, `
		f() {
			m := Mutex[int](5);
			g := m.lock();
			g.drop();
		}
	`)
	expectError(t, errs, "cannot call 'drop' explicitly")
}

func TestT0846ExplicitDropOnVectorRejected(t *testing.T) {
	errs := checkErrs(t, `
		f() {
			v := [1, 2, 3];
			v.drop();
		}
	`)
	expectError(t, errs, "cannot call 'drop' explicitly")
}

func TestT0846ExplicitDropOnUserTypeRejected(t *testing.T) {
	errs := checkErrs(t, `
		type R { int id; drop(~this) {} }
		f() {
			r := R(id: 1);
			r.drop();
		}
	`)
	expectError(t, errs, "cannot call 'drop' explicitly")
}

// A type with no drop method keeps the normal "no member" diagnostic.
func TestT0846NoDropMethodKeepsNoMemberError(t *testing.T) {
	errs := checkErrs(t, `
		type P { int x; }
		f() {
			p := P(x: 1);
			p.drop();
		}
	`)
	expectError(t, errs, "has no field or method drop")
}

// The rejection is scoped to method calls (sig.Recv() != nil): a free function
// named `drop` is an ordinary call and must remain callable. Guards against the
// `mem.Field == "drop"` check over-rejecting plain function calls.
func TestT0846FreeFunctionDropAllowed(t *testing.T) {
	errs := checkErrs(t, `
		drop() {}
		f() { drop(); }
	`)
	if len(errs) != 0 {
		t.Fatalf("expected free function drop() to be callable, got: %v", errs)
	}
}
