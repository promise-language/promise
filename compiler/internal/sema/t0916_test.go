package sema

import "testing"

// T0916: an overloaded operator method with a `move` value parameter crashes
// at runtime (operator dispatch does not move the operand the way a ~-param
// method call does → double-free/segfault). Reject it at compile time: operators
// borrow their operands and have no call-site move syntax.

func TestT0916_BinaryOperatorMoveParamRejected(t *testing.T) {
	expectError(t, checkErrs(t, `
		type S {
			string s;
			+(S move other) S { return other; }
		}
	`), "operator method S.+ cannot take a `move` parameter 'other'")
}

func TestT0916_OtherBinaryOperatorMoveParamRejected(t *testing.T) {
	// Any value-result operator dispatched from `a OP b` is rejected, not just +.
	// (Genuine unary operators carry no value param — the operand is the receiver —
	// so there is nothing to flag there.)
	expectError(t, checkErrs(t, `
		type S {
			string s;
			-(S move other) S { return other; }
		}
	`), "cannot take a `move` parameter")
}

func TestT0916_SetterOperatorMoveParamAllowed(t *testing.T) {
	// Setters ([]=, [:]=) are invoked via `lhs[i] = rhs`, an assignment that
	// genuinely moves the RHS into the call, so a ~ value/key param is correct and
	// must NOT be rejected. The stdlib relies on this (Map.[]=(~K key, ~V value)).
	expectNoErrors(t, checkErrs(t, `
		type S {
			string s;
			[]=(int i, S move v) { this.s = v.s; }
		}
	`))
}

func TestT0916_ComparisonOperatorMoveParamRejected(t *testing.T) {
	// Comparison operators (==, <, ...) are value-result and dispatched from
	// `a == b` with no call-site move syntax, so a ~ operand is rejected too.
	expectError(t, checkErrs(t, `
		type S {
			string s;
			==(S move other) bool { return true; }
		}
	`), "operator method S.== cannot take a `move` parameter 'other'")
}

func TestT0916_IndexGetterMoveParamRejected(t *testing.T) {
	// The index getter [] is value-result, dispatched from `a[i]` with no move
	// syntax — distinct from the []= setter, so its ~ index param is rejected.
	expectError(t, checkErrs(t, `
		type S {
			string s;
			[](int move i) string { return this.s; }
		}
	`), "operator method S.[] cannot take a `move` parameter 'i'")
}

func TestT0916_SliceSetterMoveParamAllowed(t *testing.T) {
	// The slice setter [:]= (the second setter form, isSetterOperatorName's other
	// branch) is invoked from `lhs[a:b] = rhs`, an assignment with a genuine
	// call-site move of the RHS, so a ~ value param is correct and not rejected.
	expectNoErrors(t, checkErrs(t, `
		type S {
			string s;
			[:]=(int a, int b, S move v) { this.s = v.s; }
		}
	`))
}

func TestT0916_OperatorMoveParamLaterPositionRejected(t *testing.T) {
	// The check scans every operand param, not just the first — a ~ on a later
	// param of a multi-arg operator (the slice getter [:]) is still flagged.
	expectError(t, checkErrs(t, `
		type S {
			string s;
			[:](int a, int move b) string { return this.s; }
		}
	`), "operator method S.[:] cannot take a `move` parameter 'b'")
}

func TestT0916_OperatorMutRefParamAllowed(t *testing.T) {
	// A mut-ref operand (suffix-~ type, `S~ other`) is a borrow, not a move — its
	// param ref modifier is RefNone (the ~ lives in the type, as a MutRef, not on
	// the param), so only the prefix-~ move modifier (RefMut) is rejected.
	expectNoErrors(t, checkErrs(t, `
		type S {
			string s;
			+(S~ other) string { return this.s; }
		}
	`))
}

func TestT0916_EnumOperatorMoveParamRejected(t *testing.T) {
	expectError(t, checkErrs(t, `
		enum E {
			a(string s),
			+(E move other) E { return other; }
		}
	`), "operator method E.+ cannot take a `move` parameter 'other'")
}

// TestT0916_IsOperatorMethodName pins the operator-vs-identifier classification
// the validation relies on: any name whose first rune is a letter or '_' is an
// identifier method; every symbol-led name is an operator. Covers the empty-name
// guard, which the sema paths never produce but the helper defends against.
func TestT0916_IsOperatorMethodName(t *testing.T) {
	operators := []string{"+", "-", "*", "/", "==", "<", "[]", "[:]", "[]=", "[:]="}
	for _, name := range operators {
		if !isOperatorMethodName(name) {
			t.Errorf("isOperatorMethodName(%q) = false, want true", name)
		}
	}
	identifiers := []string{"", "take", "_hidden", "to_string", "x"}
	for _, name := range identifiers {
		if isOperatorMethodName(name) {
			t.Errorf("isOperatorMethodName(%q) = true, want false", name)
		}
	}
}

// ===== Negatives — these constructs must remain accepted =====

func TestT0916_NonOperatorMoveParamAllowed(t *testing.T) {
	// A normal (non-operator) method may consume a ~ parameter — it has explicit
	// call-site move syntax (a.take(~b)).
	expectNoErrors(t, checkErrs(t, `
		type S {
			string s;
			take(S move other) string { return other.s; }
		}
	`))
}

func TestT0916_OperatorBorrowParamAllowed(t *testing.T) {
	// A plain borrow operand is unaffected — only the `move` modifier is
	// rejected. (A leading &-shared-ref operand is not expressible: the parser
	// reads a leading & as a receiver modifier, so it never reaches a param.)
	expectNoErrors(t, checkErrs(t, `
		type S {
			string s;
			+(S other) string { return this.s; }
		}
	`))
}

func TestT0916_OperatorMoveReceiverAllowed(t *testing.T) {
	// ~this on an operator (in-place consume of the receiver) is a different,
	// legitimate construct — only operand params are flagged, not the receiver.
	expectNoErrors(t, checkErrs(t, `
		type S {
			string s;
			+(~this, S other) string { return this.s; }
		}
	`))
}
