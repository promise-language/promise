package ownership

import "testing"

// T0936: elvis `?:` with a container/string result consumes BOTH operands
// (move-at-ownership). The some-path moves the optional's inner out and the
// none-path transfers the default into the result, so reusing either operand
// after the elvis is a use-after-move. These tests lock the ownership-pass side
// of that contract (codegen makes the result the sole owner on both paths).

// Reusing the optional after a bound elvis is a move error.
func TestT0936ReuseOptionalAfterBoundElvis(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			int[] av = []; av.push(1);
			int[]? a = av;
			int[] b = []; b.push(9);
			int[] c = []; c.push(7);
			m := a ?: b;
			n := a ?: c;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'a'")
}

// Reusing the default after a bound elvis is a move error.
func TestT0936ReuseDefaultAfterBoundElvis(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			int[]? a = none;
			int[] b = []; b.push(9);
			m := a ?: b;
			k := b.len;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'b'")
}

// The item's inline-reuse repro: `x := (a ?: b).len; w := (a ?: b).len;` — the
// second elvis re-reads both already-consumed operands.
func TestT0936InlineReuseRepro(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			int[] av = []; av.push(1); av.push(2); av.push(3);
			int[]? a = av;
			int[] b = []; b.push(9);
			x := (a ?: b).len;
			w := (a ?: b).len;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'a'")
	expectOwnerError(t, errs, "use of moved variable 'b'")
}

// An INLINE (unbound) elvis also consumes both operands: reusing the default with
// a plain member read after `(a ?: b).len` is a move error. This is the exact shape
// of the former runtime `t_reuse_none` use-after-free guard (tests/e2e/
// elvis_container_inline_test.pr), now turned into a compile-time rejection.
func TestT0936InlineThenReuseDefault(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			int[]? a = none;
			int[] b = []; b.push(9);
			x := (a ?: b).len;
			y := b.len;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'b'")
}

// Single-use elvis with an ident default and a fresh-variable bind must still
// compile — these are the shapes existing `?:` users rely on.
func TestT0936SingleUseBoundOK(t *testing.T) {
	ownerOK(t, `
		test() {
			int[]? a = none;
			int[] b = []; b.push(9);
			m := a ?: b;
			k := m.len;
		}
	`)
}

// Elvis with a literal (non-ident) default only consumes the optional; the
// literal carries no move state. The optional must not be reused, but this whole
// program is single-use and legal.
func TestT0936LiteralDefaultOK(t *testing.T) {
	ownerOK(t, `
		test() {
			string s = "h" + "i";
			string? a = s;
			string h = a ?: "lit";
			k := h.len;
		}
	`)
}

// Optional-of-Copy (`int?`) operands are Copy, so tryMove no-ops and reuse stays
// legal — the move restriction applies only to non-Copy (container/heap) operands.
func TestT0936CopyOptionalReuseOK(t *testing.T) {
	ownerOK(t, `
		test() {
			int? a = 5;
			int b = 9;
			x := a ?: b;
			y := a ?: b;
			k := x + y;
		}
	`)
}
