package sema

import "testing"

// T0883: a type declaring BOTH the prefix-unary and binary variant of the same
// operator symbol must type-check. Two sema failure modes are covered:
//   (2) binary lookup was arity-blind (checkOperator used LookupMethod, returning
//       the 0-param unary variant when declared first → "invalid signature").
//   (3) method-body resolution was name-only (lookupMethodByKind), so the binary
//       body was checked against the unary signature → "undefined: o".

func TestT0883_BinaryFirstTypeChecks(t *testing.T) {
	errs := checkErrs(t, `
		type Base {
			int v;
			-(Base o) Base { return Base(v: this.v - o.v); }
			-() Base { return Base(v: -this.v); }
		}
		use_both() {
			a := Base(v: 5);
			b := Base(v: 3);
			u := -a;
			d := a - b;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT0883_UnaryFirstTypeChecks(t *testing.T) {
	errs := checkErrs(t, `
		type Base {
			int v;
			-() Base { return Base(v: -this.v); }
			-(Base o) Base { return Base(v: this.v - o.v); }
		}
		use_both() {
			a := Base(v: 5);
			b := Base(v: 3);
			u := -a;
			d := a - b;
		}
	`)
	expectNoErrors(t, errs)
}

// A type declaring ONLY the unary (0-param) `-` but using it as a binary
// `a - b` must still report "invalid signature" — the binary lookup falls back
// to the name-only LookupMethod, finds the 0-param variant, and rejects it on
// arity. This guards the lookupBinaryOperatorMethod fallback (T0883).
func TestT0883_OnlyUnaryUsedAsBinaryInvalidSignature(t *testing.T) {
	errs := checkErrs(t, `
		type OnlyUnary {
			int v;
			-() OnlyUnary { return OnlyUnary(v: -this.v); }
		}
		bad() {
			a := OnlyUnary(v: 5);
			b := OnlyUnary(v: 3);
			d := a - b;
		}
	`)
	expectError(t, errs, "operator - has invalid signature")
}

// Symmetric enum case: only the unary `-` variant declared, used as binary →
// "invalid signature" via checkEnumOperator's LookupBinaryMethod fallback (T0883).
func TestT0883_EnumOnlyUnaryUsedAsBinaryInvalidSignature(t *testing.T) {
	errs := checkErrs(t, `
		enum E {
			Val(int n),
			-() E { return match this { Val(a) => E.Val(-a), }; }
		}
		bad() {
			x := E.Val(5);
			y := E.Val(3);
			d := x - y;
		}
	`)
	expectError(t, errs, "operator - has invalid signature")
}

// Enum with both variants of `-` also type-checks (arity-aware lookup on *Enum).
func TestT0883_EnumBothVariantsTypeCheck(t *testing.T) {
	errs := checkErrs(t, `
		enum E {
			Val(int n),
			-(E o) E {
				return match this {
					Val(a) => match o { Val(b) => E.Val(a - b), },
				};
			}
			-() E {
				return match this { Val(a) => E.Val(-a), };
			}
		}
		use_enum() {
			x := E.Val(7);
			y := E.Val(2);
			d := x - y;
			u := -x;
		}
	`)
	expectNoErrors(t, errs)
}
