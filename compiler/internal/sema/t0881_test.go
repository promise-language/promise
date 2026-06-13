package sema

import "testing"

// T0881: a user-defined prefix unary operator inherited from a parent type must
// be accepted by sema, the same way the binary equivalent already is.
// checkUnaryOperator previously scanned only the operand type's own methods,
// rejecting inherited operators with "operator - not defined on type Derived".
func TestT0881_InheritedUnaryOperatorAccepted(t *testing.T) {
	errs := checkErrs(t, `
		type Base { int v; -() Base { return Base(v: -this.v); } }
		type Derived is Base {}
		main() { d := Derived(v: 5); m := -d; }
	`)
	expectNoErrors(t, errs)
}

// Structural-interface default unary operator inherited by a concrete type.
func TestT0881_StructuralUnaryOperatorAccepted(t *testing.T) {
	errs := checkErrs(t, "\n"+`
		type Negatable `+"`structural"+` { int v; -() Negatable { return this; } }
		type Item is Negatable { int v; }
		main() { it := Item(v: 5); m := -it; }
	`)
	expectNoErrors(t, errs)
}

// Generic parent: the result type Base[T] must resolve to Base[int] through the
// inherited type-param mapping.
func TestT0881_InheritedGenericUnaryOperatorAccepted(t *testing.T) {
	errs := checkErrs(t, `
		type Base[T] { T v; -() Base[T] { return this; } }
		type Derived is Base[int] {}
		main() { d := Derived(v: 5); m := -d; }
	`)
	expectNoErrors(t, errs)
}

// Direct generic instance (not inherited): operand is Box[int], an *Instance.
// This populates `subst` (the instance's own type args) which the fix copies
// into fullSubst before substituting the result type Box[T] -> Box[int].
// The inherited cases above leave subst empty (Derived has no type params), so
// this is the only case exercising the fullSubst copy loop.
func TestT0881_DirectGenericInstanceUnaryOperatorAccepted(t *testing.T) {
	errs := checkErrs(t, `
		type Box[T] { T v; -() Box[T] { return Box[T](v: this.v); } }
		main() { b := Box[int](v: 7); m := -b; }
	`)
	expectNoErrors(t, errs)
}

// Negative guard: walking is-parents must NOT invent an operator the chain
// never declares. A Derived whose parent has no unary `-` is still rejected —
// the parent walk widens acceptance, it does not blanket-accept.
func TestT0881_InheritedUnaryOperatorAbsentStillRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Base { int v; }
		type Derived is Base {}
		main() { d := Derived(v: 5); m := -d; }
	`)
	expectError(t, errs, "operator - not defined on type Derived")
}
