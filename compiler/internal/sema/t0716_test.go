package sema

import "testing"

// T0716: a direct field store (field assignment or field inc/dec) through a
// shared (read-only) borrow must be rejected — without it, mutating a field of a
// borrowed parameter silently mutates the caller's value. Scope is deliberately
// narrow: direct *field* stores only. Method-mediated writes (property setters,
// `vec[i] = x`, slice assignment) go through RefNone-receiver methods and remain
// a permitted interior-mutability escape hatch (T0980 / deferred T1053), as does
// self-mutation through `this`.

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

func TestT0716_FieldStoreThroughThisOK(t *testing.T) {
	// Self-mutation through a (plain, RefNone) `this` receiver stays permitted —
	// the deferred RefNone-receiver decision, not this bug. Every stdlib container
	// mutator relies on it.
	checkOK(t, `
		type Counter {
			int n;
			set_plain(this) { this.n = 5; }
			set_mut(~this) { this.n = 7; }
		}
	`)
}

func TestT0716_SetterThroughSharedBorrowOK(t *testing.T) {
	// A property setter is a RefNone-receiver method call — the interior-mutability
	// escape hatch, not a direct field store. Left permissive (T0980 / T1053).
	checkOK(t, `
		type Counter {
			int n;
			get value int { return this.n; }
			set value(int v) { this.n = v; }
		}
		via(Counter c) { c.value = 5; }
	`)
}

func TestT0716_IndexAssignThroughSharedBorrowOK(t *testing.T) {
	// `vec[i] = x` dispatches to Vector.[]= (RefNone receiver) — the same escape
	// hatch sort/shuffle rely on. Not a direct field store; left permissive.
	checkOK(t, `
		via(int[] v) { v[0] = 9; }
	`)
}
