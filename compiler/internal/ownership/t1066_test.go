package ownership

import "testing"

// T1066: T0936's elvis move-at-ownership change (ownership/expr.go tryMove calls on
// both `?:` ident operands) hits tryMove's pre-existing use-bound and active-borrow
// rejections. These are sound under move-at-ownership, but were a new behavior
// corner with no dedicated coverage — lock them here so a future relaxation or
// regression is caught.

// A use-bound (pinned) variable used as the elvis default cannot be moved — it
// still needs close() at scope exit, so the elvis's move-out would leave it
// double-closed / used-after-close.
func TestT1066ElvisRejectsUseBoundDefault(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			close() {}
		}
		test() {
			Resource? optR = none;
			use g := Resource();
			x := optR ?: g;
		}
	`)
	expectOwnerError(t, errs, "cannot move use-bound variable 'g'")
}

// An actively-borrowed default cannot be moved into the elvis result — the
// borrower would outlive the value it points at.
func TestT1066ElvisRejectsBorrowedDefault(t *testing.T) {
	errs := ownerErrs(t, `
		getRef(int[] s) int[]& { return s; }
		test() {
			int[]? a = none;
			int[] b = []; b.push(9);
			int[] &r = getRef(b);
			m := a ?: b;
			k := r.len;
		}
	`)
	expectOwnerError(t, errs, "cannot move 'b' while it is borrowed")
}

// An actively-borrowed optional cannot be moved out of at the elvis's some-path
// either — the left operand goes through the same tryMove call as the default.
func TestT1066ElvisRejectsBorrowedOptional(t *testing.T) {
	errs := ownerErrs(t, `
		getRef(int[]? s) int[]?& { return s; }
		test() {
			int[] av = []; av.push(1);
			int[]? a = av;
			int[] b = []; b.push(9);
			int[]? &r = getRef(a);
			m := a ?: b;
			k := r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 'a' while it is borrowed")
}
