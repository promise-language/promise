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
// Note the Copy exemption in createBorrowWithKind keys on the ROOT variable
// (`o`), NOT the field: a Copy-typed field of a *non-Copy* receiver is still
// borrow-tracked and still rejected (see TestT1058_MutThisPlusCopyFieldArgRejected).
// That is a conservative over-approximation — a Copy field is passed by value with
// no real aliasing hazard — tracked as a possible future relaxation (T1378).

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

// Pins the over-approximation: a Copy-typed (int) field of a non-Copy receiver is
// ALSO rejected, because the Copy exemption in createBorrowWithKind checks the root
// variable `o` (non-Copy Obj), not the leaf field type. Semantically the int arg is
// a by-value copy with no aliasing hazard, so this is conservative; relaxing it is
// tracked as T1378. Pinning REJECT so any change to the shape is a deliberate choice.
func TestT1058_MutThisPlusCopyFieldArgRejected(t *testing.T) {
	errs := ownerErrs(t, `
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
	expectOwnerError(t, errs, "cannot borrow 'o' as mutable")
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
// nothing to overlap — the call copies rather than borrows. This is the exact
// "rule lifts only when the whole receiver is a Copy/value type" carve-out
// documented in docs/language-guide.md; pinning ACCEPT locks it against
// regression and balances the reject tests above.
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
