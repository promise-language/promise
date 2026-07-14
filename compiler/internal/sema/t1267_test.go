package sema

import "testing"

// T1267: a bare failable call written as a `match` *expression* arm (`=> call(),`)
// must receive the same failable-handling check as any other bare call site.
// Previously checkMatchExpr only ran checkExpr on arm.Body and stopped, so the
// call was neither auto-propagated (failable context) nor rejected (non-failable
// context) — silently swallowed. Block arms already routed through checkBlock's
// statement path.

// In a non-failable function, a bare failable expr arm must be a compile error.
func TestT1267BareFailableExprArmNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		boom!() { raise error(message: "bang"); }
		dispatch(string c) {
			match c {
				"a" => boom(),
				_ => {},
			}
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// The same call in a failable function is accepted (auto-propagates).
func TestT1267BareFailableExprArmFailableOK(t *testing.T) {
	checkOK(t, `
		boom!() { raise error(message: "bang"); }
		dispatch!(string c) {
			match c {
				"a" => boom(),
				_ => {},
			}
		}
		main() {}
	`)
}

// Value position: a value-failable expr arm in a failable function is accepted.
func TestT1267ValueFailableExprArmFailableOK(t *testing.T) {
	checkOK(t, `
		boom_val!() int { raise error(message: "bang"); }
		f!(string c) int {
			return match c {
				"a" => boom_val(),
				_ => 0,
			};
		}
		main() {}
	`)
}

// Value position in a non-failable function is a compile error, not a swallow.
func TestT1267ValueFailableExprArmNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		boom_val!() int { raise error(message: "bang"); }
		f(string c) int {
			return match c {
				"a" => boom_val(),
				_ => 0,
			};
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// Explicit ?^ arm is unaffected (not a bare failable call — an ErrorPropagateExpr).
func TestT1267ExplicitPropagateExprArmOK(t *testing.T) {
	checkOK(t, `
		boom_val!() int { raise error(message: "bang"); }
		f!(string c) int {
			return match c {
				"a" => boom_val()?^,
				_ => 0,
			};
		}
		main() {}
	`)
}
