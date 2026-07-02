package sema

import "testing"

// T1002: an Optional match subject supports only the `none` pattern plus a
// catch-all. Any other literal/expression arm has no lowering and must be a
// clean diagnostic (previously a codegen panic).

func TestMatchNonNoneLiteralOnOptionalRejected(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int? x = 5;
			r := match x { 5 => 1, _ => 2 };
		}
	`)
	expectError(t, errs, "match on optional type int? only supports the `none` pattern")
}

func TestMatchExpressionPatternOnOptionalRejected(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int y = 5;
			int? x = 5;
			r := match x { (y) => 1, _ => 2 };
		}
	`)
	expectError(t, errs, "match on optional type int? only supports the `none` pattern")
}

func TestMatchNoneAndCatchAllOnOptionalAccepted(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int? x = 5;
			r := match x { none => 1, _ => 2 };
		}
	`)
	expectNoErrors(t, errs)
}

// Borrowed-optional subject (T?& from Ref[T?].borrow) — the item's core repro.
// The rejection guard must strip the SharedRef before recognizing the Optional,
// so the same clean diagnostic fires (previously a codegen panic).

func TestMatchNonNoneLiteralOnBorrowedOptionalRejected(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int? init = none;
			a := Ref[int?](move init);
			r := match a.borrow { 5 => 1, _ => 2 };
		}
	`)
	expectError(t, errs, "match on optional type int?& only supports the `none` pattern")
}

func TestMatchExpressionPatternOnBorrowedOptionalRejected(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int y = 5;
			int? init = none;
			a := Ref[int?](move init);
			r := match a.borrow { (y) => 1, _ => 2 };
		}
	`)
	expectError(t, errs, "match on optional type int?& only supports the `none` pattern")
}

func TestMatchNoneOnBorrowedOptionalAccepted(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			int? init = none;
			a := Ref[int?](move init);
			r := match a.borrow { none => 1, _ => 2 };
		}
	`)
	expectNoErrors(t, errs)
}
