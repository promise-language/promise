package sema

import "testing"

// T0716: a direct field store (field assignment or field inc/dec) through a
// shared (read-only) borrow must be rejected — without it, mutating a field of a
// borrowed parameter silently mutates the caller's value. T1053 later generalized
// the rule to every mutation form (method calls, index/slice writes, property
// setters) and closed the self-mutation carve-out; those cases live in
// t1053_test.go. The tests below pin the original direct-field-store behavior.

func TestT0716_FieldAssignThroughSharedBorrow(t *testing.T) {
	// The reported repro: writing a field of a borrowed parameter.
	errs := checkErrs(t, `
		type Counter { int n; }
		mutate_via_shared(Counter c) { c.n = 999; }
	`)
	expectError(t, errs, "cannot mutate field 'n' through a shared (read-only) borrow")
}

func TestT0716_FieldIncDecThroughSharedBorrow(t *testing.T) {
	errs := checkErrs(t, `
		type Counter { int n; }
		via(Counter c) { c.n++; }
	`)
	expectError(t, errs, "cannot mutate field 'n' through a shared (read-only) borrow")
}

func TestT0716_NestedFieldThroughSharedBorrow(t *testing.T) {
	// The written field belongs to a field of the shared-borrow parameter; the
	// place still roots in the read-only borrow.
	errs := checkErrs(t, `
		type Inner { int n; }
		type Box { Inner inner; }
		via(Box b) { b.inner.n = 5; }
	`)
	expectError(t, errs, "cannot mutate field 'n' through a shared (read-only) borrow")
}

func TestT0716_ForUpdateIncDecThroughSharedBorrow(t *testing.T) {
	// The classic-for update clause is a write position too.
	errs := checkErrs(t, `
		type Counter { int n; }
		via(Counter c) { for i := 0; i < 3; c.n++ { } }
	`)
	expectError(t, errs, "cannot mutate field 'n' through a shared (read-only) borrow")
}

func TestT0716_ForUpdateAssignThroughSharedBorrow(t *testing.T) {
	errs := checkErrs(t, `
		type Counter { int n; }
		via(Counter c) { for i := 0; i < 3; c.n = i { } }
	`)
	expectError(t, errs, "cannot mutate field 'n' through a shared (read-only) borrow")
}

func TestT0716_FieldAssignThroughMutBorrowOK(t *testing.T) {
	checkOK(t, `
		type Counter { int n; }
		via(Counter~ c) { c.n = 5; }
	`)
}

func TestT0716_FieldAssignThroughMoveParamOK(t *testing.T) {
	checkOK(t, `
		type Counter { int n; }
		sink(Counter move c) { c.n = 5; }
	`)
}

func TestT0716_FieldAssignOnOwnedLocalOK(t *testing.T) {
	checkOK(t, `
		type Counter { int n; }
		main() { c := Counter(n: 1); c.n = 9; c.n++; }
	`)
}

func TestT1053_FieldStoreThroughPlainThisRejected(t *testing.T) {
	// T1053 closes the deferred RefNone-receiver hole: a plain-`this` method that
	// writes `this.field` is now rejected — the author must annotate `~this`.
	// The `~this` form stays OK.
	errs := checkErrs(t, `
		type Counter {
			int n;
			set_plain(this) { this.n = 5; }
			set_mut(~this) { this.n = 7; }
		}
	`)
	expectError(t, errs, "cannot mutate field 'n' through a shared (read-only) borrow")
}

func TestT1053_FieldStoreThroughMutThisOK(t *testing.T) {
	// A `~this` receiver is a mutable borrow, so self-mutation stays permitted.
	checkOK(t, `
		type Counter {
			int n;
			set_mut(~this) { this.n = 7; }
		}
	`)
}

func TestT1053_SetterThroughSharedBorrowRejected(t *testing.T) {
	// T1053: a property setter is a `~this` mutating method (setters default to a
	// mutable-borrow receiver), so writing through a shared borrow is now rejected.
	errs := checkErrs(t, `
		type Counter {
			int n;
			get value int { return this.n; }
			set value(int v) { this.n = v; }
		}
		via(Counter c) { c.value = 5; }
	`)
	expectError(t, errs, "cannot mutate field 'value' through a shared (read-only) borrow")
}

func TestT1053_IndexAssignThroughSharedBorrowRejected(t *testing.T) {
	// T1053: `vec[i] = x` dispatches to Vector.[]= (`~this` setter), so an index
	// write through a shared borrow is now rejected — take a `~` mutable borrow.
	errs := checkErrs(t, `
		via(int[] v) { v[0] = 9; }
	`)
	expectError(t, errs, "cannot mutate element through a shared (read-only) borrow")
}
