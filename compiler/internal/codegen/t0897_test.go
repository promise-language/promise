package codegen

import "testing"

// T0897: an overloaded operator borrows its operands (unlike a method value
// param, which is moved). A body that returns a borrowed operand unchanged
// (`return other`) would hand back a value aliasing the caller's still-live
// operand → both free the same heap instance (double-free). Codegen must clone
// the returned operand, mirroring the clone-on-`return this` fix (T0893). The
// clone shows up as a heapdup block in the operator body.
func TestOperatorReturnOtherClonesBorrowedOperand(t *testing.T) {
	ir := generateIR(t, `
		type S { int v; +(S other) S { return other; } }
		main() { a := S(v: 1); b := S(v: 2); m := a + b; }
	`)
	assertContains(t, extractFunction(ir, `"S.+"`), "heapdup")
}

// T0897: a comparison operator returns a bool (not an operand), so the operand
// is never aliased — the body must NOT clone. This guards against cloning when
// the return value is not a borrowed operand (which would break the borrow
// semantics of `==`/`<`/etc.).
func TestOperatorComparisonDoesNotClone(t *testing.T) {
	ir := generateIR(t, `
		type S { int v; ==(S other) bool { return this.v == other.v; } }
		main() { a := S(v: 1); b := S(v: 1); x := a == b; }
	`)
	assertNotContains(t, extractFunction(ir, `"S.=="`), "heapdup")
}

// T0897: an operator returning a freshly constructed value (not an operand)
// must NOT clone — the fresh value is already sole-owner, so cloning it would
// leak and is unnecessary.
func TestOperatorFreshResultDoesNotClone(t *testing.T) {
	ir := generateIR(t, `
		type S { int v; +(S other) S { return S(v: this.v + other.v); } }
		main() { a := S(v: 1); b := S(v: 2); m := a + b; }
	`)
	assertNotContains(t, extractFunction(ir, `"S.+"`), "heapdup")
}

// T0897: returning the receiver (`return this`) goes through the T0893 path, not
// the operand-clone path — but it still clones (the receiver is borrowed too).
// Returning the *other* operand and returning *this* should both clone; this
// confirms the operand path does not interfere with the existing this path.
func TestOperatorReturnThisStillClones(t *testing.T) {
	ir := generateIR(t, `
		type S { int v; |(S other) S { return this; } }
		main() { a := S(v: 1); b := S(v: 2); m := a | b; }
	`)
	assertContains(t, extractFunction(ir, `"S.|"`), "heapdup")
}

// T0897: a value-type operand has its data embedded directly in the value
// struct (no heap instance), so `return other` cannot alias a heap allocation —
// the body must NOT emit a heapdup clone.
func TestOperatorValueTypeOperandDoesNotClone(t *testing.T) {
	ir := generateIR(t, `
		type P { int x `+"`value"+`; +(P other) P { return other; } }
		main() { a := P(x: 1); b := P(x: 2); m := a + b; }
	`)
	assertNotContains(t, extractFunction(ir, `"P.+"`), "heapdup")
}

// T0897: an operator returning a borrow (`S&`) of an operand hands back a
// reference into existing storage, not an owned copy — it must NOT clone.
func TestOperatorBorrowReturnDoesNotClone(t *testing.T) {
	ir := generateIR(t, `
		type S { int v; &(S other) S& { return other; } }
		main() { a := S(v: 1); b := S(v: 2); }
	`)
	assertNotContains(t, extractFunction(ir, `"S.&"`), "heapdup")
}

// T0897: a generic operator (`Box[T]`) returning a borrowed operand clones in
// each monomorphized instance.
func TestOperatorGenericReturnOtherClones(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T v; +(Box[T] other) Box[T] { return other; } }
		main() { a := Box[int](v: 1); b := Box[int](v: 2); m := a + b; }
	`)
	assertContains(t, extractFunction(ir, `"Box[int].+"`), "heapdup")
}

// T0897: an operator whose declared return type is Optional (`S?`) but which
// returns a bare operand (`return other`) still aliases the live operand, so the
// payload must be cloned. wrapOperatorParamReturnValue unwraps the Optional to
// the element type before cloning; this guards that the Optional-return branch
// clones rather than handing back the aliasing value.
func TestOperatorOptionalReturnClonesOperand(t *testing.T) {
	ir := generateIR(t, `
		type S { int v; +(S other) S? { return other; } }
		main() { a := S(v: 1); b := S(v: 2); o := a + b; }
	`)
	assertContains(t, extractFunction(ir, `"S.+"`), "heapdup")
}

// T0897: an operand whose type is itself a copy type (a `string` operand) has no
// independent heap instance the result could double-free, so returning it must
// NOT clone. Exercises the string/Copy return branch of cloneOwnedReturnAlias.
func TestOperatorStringOperandDoesNotClone(t *testing.T) {
	ir := generateIR(t, `
		type S { string label; +(string other) string { return other; } }
		main() { a := S(label: "x"); s := a + "hi"; }
	`)
	assertNotContains(t, extractFunction(ir, `"S.+"`), "heapdup")
}

// T0897: a ref-typed operand (`S&`) is a borrow, not an owned value — operator
// dispatch never gives the body ownership of it, so it must not be registered as
// a clonable operand and `return other` must NOT clone. Exercises the
// MutRef/SharedRef param-type skip in setOperatorValueParams (distinct from
// TestOperatorBorrowReturnDoesNotClone, which has a value operand but a ref
// *return* type).
func TestOperatorRefTypedOperandDoesNotClone(t *testing.T) {
	ir := generateIR(t, `
		type S { int v; +(S other) S& { return other; } }
		main() { a := S(v: 1); b := S(v: 2); }
	`)
	assertNotContains(t, extractFunction(ir, `"S.+"`), "heapdup")
}

// T0897: an enum operand with no droppable variant payload is trivially copyable
// — returning it cannot double-free, so cloneOwnedReturnAlias must skip the clone
// (the `!enumInstanceHasDrop` early-out).
func TestOperatorNonDroppableEnumOperandDoesNotClone(t *testing.T) {
	ir := generateIR(t, `
		enum E { A(int n), B }
		type S { int v; +(E other) E { return other; } }
		main() { a := S(v: 1); e := E.A(1); m := a + e; }
	`)
	assertNotContains(t, extractFunction(ir, `"S.+"`), "enumdup")
}

// T0897: a droppable enum operand (variant carries a heap `string`) returned bare
// must be deep-cloned via the in-place enum-dup path, or the result and the live
// operand would both free the payload. Confirms the `dupEnumElementInPlace`
// branch of cloneOwnedReturnAlias is reached through the operator-operand route.
func TestOperatorDroppableEnumOperandClones(t *testing.T) {
	ir := generateIR(t, `
		enum E { A(string s), B }
		type S { int v; +(E other) E { return other; } }
		main() { a := S(v: 1); e := E.A("x"); m := a + e; }
	`)
	assertContains(t, extractFunction(ir, `"S.+"`), "enumdup")
}
