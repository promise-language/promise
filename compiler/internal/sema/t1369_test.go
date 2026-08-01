package sema

import "testing"

// T1369: an overloaded operator method with a mut-ref operand param (`S~ other`,
// a *types.MutRef param type) miscompiles — operator dispatch passes the operand
// BY VALUE, but a mut-ref param lowers to a pointer-taking signature, so the value
// struct is reinterpreted as a pointer → segfault at 0x8. Reject it at compile
// time (sibling of the T0916 move-param rejection): operators borrow their
// operands read-only and have no call-site syntax for a mutable borrow, so a
// mutating operand would be a hidden effect regardless of the ABI bug.

func TestT1369_BinaryOperatorMutRefParamRejected(t *testing.T) {
	// The exact repro that segfaulted at runtime.
	expectError(t, checkErrs(t, `
		type S {
			string s;
			+(S~ other) string { return this.s + other.s; }
		}
	`), "operator method S.+ cannot take a mut-ref parameter 'other'")
}

func TestT1369_ComparisonOperatorMutRefParamRejected(t *testing.T) {
	// Any value-result operator dispatched from `a OP b` is rejected, not just +.
	expectError(t, checkErrs(t, `
		type S {
			string s;
			==(S~ other) bool { return true; }
		}
	`), "operator method S.== cannot take a mut-ref parameter 'other'")
}

func TestT1369_IndexGetterMutRefParamRejected(t *testing.T) {
	// The index getter [] is value-result, dispatched from `a[i]` — a mut-ref
	// index param is rejected too.
	expectError(t, checkErrs(t, `
		type S {
			string s;
			[](S~ i) string { return this.s; }
		}
	`), "operator method S.[] cannot take a mut-ref parameter 'i'")
}

func TestT1369_SliceGetterLaterPositionMutRefParamRejected(t *testing.T) {
	// The check scans every operand param, not just the first — a mut-ref on a
	// later param of a multi-arg operator (the slice getter [:]) is still flagged.
	expectError(t, checkErrs(t, `
		type S {
			string s;
			[:](int a, S~ b) string { return this.s; }
		}
	`), "operator method S.[:] cannot take a mut-ref parameter 'b'")
}

func TestT1369_EnumOperatorMutRefParamRejected(t *testing.T) {
	// Covers the defineEnumMethod path.
	expectError(t, checkErrs(t, `
		enum E {
			a(string s),
			+(E~ other) E { return this; }
		}
	`), "operator method E.+ cannot take a mut-ref parameter 'other'")
}

func TestT1369_IndexSetterMutRefParamRejected(t *testing.T) {
	// Unlike the move (~) param check, the mut-ref check is NOT relaxed for
	// setters: a []= setter passes its value operand by value too, so a mut-ref
	// value param miscompiles there as well and is rejected.
	expectError(t, checkErrs(t, `
		type S {
			string s;
			[]=(int i, S~ v) { this.s = v.s; }
		}
	`), "operator method S.[]= cannot take a mut-ref parameter 'v'")
}

func TestT1369_SliceSetterMutRefParamRejected(t *testing.T) {
	// The other setter form ([:]=, isSetterOperatorName's second branch) also passes
	// its value operand by value, so a mut-ref value param is rejected there too —
	// unlike the move (~) param, which stays allowed on both setter forms.
	expectError(t, checkErrs(t, `
		type S {
			string s;
			[:]=(int a, int b, S~ v) { this.s = v.s; }
		}
	`), "operator method S.[:]= cannot take a mut-ref parameter 'v'")
}

// ===== Negatives — these constructs must remain accepted =====

func TestT1369_OperatorBorrowParamAllowed(t *testing.T) {
	// A plain borrow operand is unaffected — only the mut-ref (~) form is rejected.
	expectNoErrors(t, checkErrs(t, `
		type S {
			string s;
			+(S other) string { return this.s; }
		}
	`))
}

func TestT1369_OperatorMutRefReceiverAllowed(t *testing.T) {
	// A mut-ref receiver on an operator (in-place mutate of the visible LEFT
	// operand `a`) is a different, legitimate construct — the receiver is already
	// passed as a pointer. Only operand params are flagged, not the receiver.
	expectNoErrors(t, checkErrs(t, `
		type S {
			string s;
			+(~this, S other) string { this.s = other.s; return this.s; }
		}
	`))
}

func TestT1369_SetterMoveParamStillAllowed(t *testing.T) {
	// The move (~) param remains valid on setters (the stdlib Map.[]= pattern) —
	// a move param is RefMut, not a *types.MutRef, so the new mut-ref check does
	// not touch it. Guards against over-rejecting.
	expectNoErrors(t, checkErrs(t, `
		type S {
			string s;
			[]=(int i, S move v) { this.s = v.s; }
		}
	`))
}

func TestT1369_NonOperatorMutRefParamAllowed(t *testing.T) {
	// A mut-ref param is only barred on OPERATORS — a normal (non-operator) method
	// may take a Type~ mutable borrow (it has call-site syntax: a.take(~b)).
	expectNoErrors(t, checkErrs(t, `
		type S {
			string s;
			take(S~ other) string { other.s = this.s; return other.s; }
		}
	`))
}
