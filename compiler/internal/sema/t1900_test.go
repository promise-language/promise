package sema

import "testing"

// T1900: a bare failable call as a `match` scrutinee is an expression position
// (§7.2), so it must receive the same failable-handling check as any other bare
// call site — auto-propagate in a failable function, compile error in a
// non-failable one. Previously checkMatchExpr ran checkExpr on e.Subject and
// stopped, so the raw failable aggregate reached the match codegen: a codegen
// panic for int/enum subjects, a runtime segfault for string subjects, and a
// wrong answer for optional subjects (the none arm read the error flag).

// int scrutinee: accepted in a failable function (auto-propagates).
func TestT1900IntScrutineeFailableOK(t *testing.T) {
	checkOK(t, `
		mki!(int n) int { return n; }
		h!() int {
			return match mki(3) {
				3 => 1,
				_ => 0,
			};
		}
		main() {}
	`)
}

// int scrutinee: rejected in a non-failable function.
func TestT1900IntScrutineeNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		mki!(int n) int { return n; }
		h() int {
			return match mki(3) {
				3 => 1,
				_ => 0,
			};
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// string scrutinee: accepted in a failable function.
func TestT1900StringScrutineeFailableOK(t *testing.T) {
	checkOK(t, `
		mks!() string { return "aaa"; }
		h!() int {
			return match mks() {
				"aaa" => 1,
				_ => 0,
			};
		}
		main() {}
	`)
}

// string scrutinee: rejected in a non-failable function.
func TestT1900StringScrutineeNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		mks!() string { return "aaa"; }
		h() int {
			return match mks() {
				"aaa" => 1,
				_ => 0,
			};
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// enum scrutinee: accepted in a failable function.
func TestT1900EnumScrutineeFailableOK(t *testing.T) {
	checkOK(t, `
		enum Kind { alpha, beta, }
		mke!() Kind { return Kind.alpha; }
		h!() int {
			return match mke() {
				Kind.alpha => 1,
				Kind.beta => 0,
			};
		}
		main() {}
	`)
}

// enum scrutinee: rejected in a non-failable function.
func TestT1900EnumScrutineeNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		enum Kind { alpha, beta, }
		mke!() Kind { return Kind.alpha; }
		h() int {
			return match mke() {
				Kind.alpha => 1,
				Kind.beta => 0,
			};
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// optional scrutinee: accepted in a failable function.
func TestT1900OptionalScrutineeFailableOK(t *testing.T) {
	checkOK(t, `
		mko!() int? { return 3; }
		h!() int {
			return match mko() {
				none => -1,
				_ => 1,
			};
		}
		main() {}
	`)
}

// optional scrutinee: rejected in a non-failable function.
func TestT1900OptionalScrutineeNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		mko!() int? { return 3; }
		h() int {
			return match mko() {
				none => -1,
				_ => 1,
			};
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// Statement-position match (value discarded, checkMatchExpr report=false) must
// still reject a bare failable scrutinee in a non-failable function — the
// subject check runs before the report distinction applies to the arms.
func TestT1900StatementScrutineeNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		mki!(int n) int { return n; }
		h() {
			match mki(3) {
				3 => { },
				_ => { },
			}
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// Inside a plain `go {}` body the scope is non-failable (T1217) even when the
// enclosing function is failable — the diagnostic must still fire.
func TestT1900GoBlockScrutineeRejected(t *testing.T) {
	errs := checkErrs(t, `
		mki!(int n) int { return n; }
		main!() {
			go {
				n := match mki(3) {
					3 => 1,
					_ => 0,
				};
			};
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

// Explicit ?^ scrutinee is unaffected (an ErrorPropagateExpr, not a bare call).
func TestT1900ExplicitPropagateScrutineeOK(t *testing.T) {
	checkOK(t, `
		mki!(int n) int { return n; }
		h!() int {
			return match mki(3)?^ {
				3 => 1,
				_ => 0,
			};
		}
		main() {}
	`)
}
