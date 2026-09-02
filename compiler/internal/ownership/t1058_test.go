package ownership

import "testing"

// T1058: Pin the borrow-conflict behavior for a `~this` (mutable-receiver)
// method that is passed a non-Copy field of the SAME receiver as a plain
// (shared-borrow) argument in the same call — e.g. `o.mutate(o.field)` where
// `mutate` takes `~this`.
//
// T0964 made a plain-`T` call argument a *shared* borrow of its origin. So per
// call site the ownership pass runs the argument loop first: `o.field` as a
// plain non-Copy param registers a shared borrow {Origin: o, FieldPath:[field]}
// via createBorrowWithKind(..., BorrowShared, ...) in expr.go. Then
// checkReceiverBorrow maps the `~this` receiver to BorrowMut and calls
// createBorrowWithKind(o, BorrowMut, ...); its HasOverlappingBorrow("o", nil)
// check fires because the whole-object mutable borrow overlaps the existing
// `o.field` shared borrow → "cannot borrow 'o' as mutable — it is already
// borrowed".
//
// This is the intended, defensible aliasing rule and is Rust-consistent: the
// method could reassign/drop o.field while a live shared reference to it exists
// → dangling borrow. The outcome to pin is REJECT. No compiler change is made;
// these tests lock the behavior against regression and document it.
//
// The Copy exemption in createBorrowWithKind has two levels: (1) the ROOT variable
// (`o`) is Copy → skip entirely, and (2) the EXPRESSION's own resolved type is Copy
// → skip (T1378). So a Copy-typed field of a non-Copy receiver (e.g. `o.n` where
// `n int`) is exempted — the value is passed by-value with no aliasing hazard.

// Core reject: `~this` receiver + same-object non-Copy field arg.
func TestT1058_MutThisPlusSameFieldArgRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Field { string s; }
		type Obj {
			Field field;
			mutate(~this, Field f) { this.field.s = f.s; }
		}
		test() {
			Obj o = Obj(field: Field(s: "x"));
			o.mutate(o.field);
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 'o' as mutable")
}

// Contrast: a plain/shared `this` receiver + same-object field arg is fine —
// two coexisting shared borrows of `o` (receiver and o.field) do not conflict.
func TestT1058_SharedThisPlusSameFieldArgOK(t *testing.T) {
	ownerOK(t, `
		type Field { string s; }
		type Obj {
			Field field;
			read(this, Field f) string `+"`"+`structural(protocol: false) { return f.s; }
		}
		test() {
			Obj o = Obj(field: Field(s: "x"));
			string r = o.read(o.field);
		}
	`)
}

// T1378: A Copy-typed (int) field of a non-Copy receiver is now ACCEPTED —
// the expression's own resolved type is Copy, so no shared borrow is registered
// and the `~this` mutable borrow has nothing to overlap.
func TestT1058_MutThisPlusCopyFieldArgAccepted(t *testing.T) {
	ownerOK(t, `
		type Obj {
			int n;
			string s;
			mutate(~this, int f) { this.n = f; }
		}
		test() {
			Obj o = Obj(n: 1, s: "x");
			o.mutate(o.n);
		}
	`)
}

// Proves the reject is aliasing-specific, not a blanket "~this + non-Copy arg"
// ban: a `~this` receiver plus a DISTINCT object argument passes.
func TestT1058_MutThisPlusDistinctArgOK(t *testing.T) {
	ownerOK(t, `
		type Field { string s; }
		type Obj {
			Field field;
			mutate(~this, Field f) { this.field.s = f.s; }
		}
		test() {
			Obj o = Obj(field: Field(s: "x"));
			Field other = Field(s: "y");
			o.mutate(other);
		}
	`)
}

// Pins the OTHER side of the Copy exemption: when the WHOLE receiver is a Copy
// type, the same `o.mutate(o.field)` shape is accepted. The Copy early-return in
// createBorrowWithKind keys on the root variable `o` (here Copy `Obj`), so no
// shared borrow is registered for the arg and the `~this` receiver borrow has
// nothing to overlap — the call copies rather than borrows.
func TestT1058_CopyReceiverPlusSameFieldArgOK(t *testing.T) {
	ownerOK(t, `
		type Field `+"`copy"+` { int n; }
		type Obj `+"`copy"+` {
			Field field;
			mutate(~this, Field f) { this.field = f; }
		}
		test() {
			Obj o = Obj(field: Field(n: 1));
			o.mutate(o.field);
		}
	`)
}

// --- T1378: Copy-typed field arg relaxation tests ---

// T1378: non-Copy field of a non-Copy receiver is still rejected.
func TestT1378_MutThisPlusNonCopyFieldArgStillRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Obj {
			string name;
			mutate(~this, string s) { this.name = s; }
		}
		test() {
			Obj o = Obj(name: "x");
			o.mutate(o.name);
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 'o' as mutable")
}

// T1378: pure value type field (Copy) of a non-Copy receiver is accepted.
func TestT1378_MutThisPlusValueTypeFieldArgAccepted(t *testing.T) {
	ownerOK(t, `
		type Point `+"`copy"+` { int x; int y; }
		type Obj {
			Point pt;
			string s;
			mutate(~this, Point p) { this.pt = p; }
		}
		test() {
			Obj o = Obj(pt: Point(x: 1, y: 2), s: "x");
			o.mutate(o.pt);
		}
	`)
}

// T1378: bool field (Copy) of a non-Copy receiver is accepted.
func TestT1378_MutThisPlusBoolFieldArgAccepted(t *testing.T) {
	ownerOK(t, `
		type Obj {
			bool flag;
			string s;
			mutate(~this, bool f) { this.flag = f; }
		}
		test() {
			Obj o = Obj(flag: true, s: "x");
			o.mutate(o.flag);
		}
	`)
}

// T1378: multiple Copy-typed field args in the same call are accepted.
func TestT1378_MutThisPlusMultipleCopyFieldArgsAccepted(t *testing.T) {
	ownerOK(t, `
		type Obj {
			int x;
			int y;
			string s;
			mutate(~this, int a, int b) { this.x = a; this.y = b; }
		}
		test() {
			Obj o = Obj(x: 1, y: 2, s: "x");
			o.mutate(o.x, o.y);
		}
	`)
}

// T1378: mixed Copy + non-Copy field args — rejected because the non-Copy
// field still registers a shared borrow that overlaps the `~this` mutable borrow.
func TestT1378_MutThisPlusMixedCopyAndNonCopyFieldArgsRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Obj {
			int n;
			string s;
			mutate(~this, int a, string b) { this.n = a; this.s = b; }
		}
		test() {
			Obj o = Obj(n: 1, s: "x");
			o.mutate(o.n, o.s);
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 'o' as mutable")
}

// T1378: nested member access — o.inner.n where n is Copy but inner is non-Copy.
// The expression type of `o.inner.n` is int (Copy), so it should be accepted.
func TestT1378_MutThisPlusNestedCopyFieldAccepted(t *testing.T) {
	ownerOK(t, `
		type Inner { int n; string s; }
		type Outer {
			Inner inner;
			mutate(~this, int v) { this.inner.n = v; }
		}
		test() {
			Outer o = Outer(inner: Inner(n: 1, s: "x"));
			o.mutate(o.inner.n);
		}
	`)
}

// T1378: nested member access — o.inner.s where s is non-Copy. Still rejected.
func TestT1378_MutThisPlusNestedNonCopyFieldRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { string s; }
		type Outer {
			Inner inner;
			mutate(~this, string v) { this.inner.s = v; }
		}
		test() {
			Outer o = Outer(inner: Inner(s: "x"));
			o.mutate(o.inner.s);
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 'o' as mutable")
}

// T1378: f64 field (Copy) of a non-Copy receiver is accepted.
func TestT1378_MutThisPlusF64FieldArgAccepted(t *testing.T) {
	ownerOK(t, `
		type Obj {
			f64 val;
			string s;
			mutate(~this, f64 v) { this.val = v; }
		}
		test() {
			Obj o = Obj(val: 3.14, s: "x");
			o.mutate(o.val);
		}
	`)
}
