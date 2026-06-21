package sema

import "testing"

// T0980: calling a `~this` (mutating) method through a shared (read-only) borrow
// must be rejected — the receiver analogue of the existing `~param` arg check.
// Scope is exactly RefMut receivers; RefNone implicit-`this` mutators stay
// permissive.

func TestT0980_MutMethodThroughSharedBorrowParam(t *testing.T) {
	errs := checkErrs(t, `
		type Counter { int n; bump(~this) { this.n = this.n + 1; } }
		via(Counter c) { c.bump(); }
	`)
	expectError(t, errs, "cannot call mutating method 'bump' through a shared (read-only) borrow")
}

func TestT0980_MutMethodThroughBorrowedFieldNested(t *testing.T) {
	// The borrowed place is a field of a shared-borrow parameter.
	errs := checkErrs(t, `
		type Counter { int n; bump(~this) { this.n = this.n + 1; } }
		type Box { Counter inner; }
		via(Box b) { b.inner.bump(); }
	`)
	expectError(t, errs, "cannot call mutating method 'bump' through a shared (read-only) borrow")
}

func TestT0980_MutMethodOnReadOnlyThis(t *testing.T) {
	// A plain (RefNone) `this` receiver is read-only; calling a `~this` method
	// on it would mutate through the shared borrow.
	errs := checkErrs(t, `
		type Counter {
			int n;
			bump(~this) { this.n = this.n + 1; }
			peek(this) { this.bump(); }
		}
	`)
	expectError(t, errs, "cannot call mutating method 'bump' through a shared (read-only) borrow")
}

func TestT0980_MutMethodOnOwnedLocalOK(t *testing.T) {
	checkOK(t, `
		type Counter { int n; bump(~this) { this.n = this.n + 1; } }
		main() { c := Counter(n: 1); c.bump(); }
	`)
}

func TestT0980_MutMethodThroughMutBorrowParamOK(t *testing.T) {
	checkOK(t, `
		type Counter { int n; bump(~this) { this.n = this.n + 1; } }
		via(Counter~ c) { c.bump(); }
	`)
}

func TestT0980_MutMethodThroughMoveParamOK(t *testing.T) {
	checkOK(t, `
		type Counter { int n; bump(~this) { this.n = this.n + 1; } }
		sink(Counter move c) { c.bump(); }
	`)
}

func TestT0980_MutMethodOnMutThisOK(t *testing.T) {
	// `~this` calling a `~this` method on `this` must stay allowed.
	checkOK(t, `
		type Counter {
			int n;
			bump(~this) { this.n = this.n + 1; }
			bump_twice(~this) { this.bump(); this.bump(); }
		}
	`)
}

func TestT0980_ImplicitThisMutatorThroughBorrowOK(t *testing.T) {
	// Scope guard: a RefNone implicit-`this` receiver (the Vector.push shape) is
	// NOT keyed on RefMut, so calling it through a shared borrow stays allowed.
	checkOK(t, `
		type Bag { int n; add(this, int x) { } }
		via(Bag b) { b.add(1); }
	`)
}

func TestT0980_MutMethodThroughIndexedSharedBorrow(t *testing.T) {
	// The receiver place is an element of a shared-borrow vector (IndexExpr
	// target → roots in a read-only borrow).
	errs := checkErrs(t, `
		type Counter { int n; bump(~this) { this.n = this.n + 1; } }
		via(Counter[] v) { v[0].bump(); }
	`)
	expectError(t, errs, "cannot call mutating method 'bump' through a shared (read-only) borrow")
}

func TestT0980_MutMethodThroughIndexedMutBorrowOK(t *testing.T) {
	// Same element access, but through a `~` mutable-borrow vector — allowed.
	checkOK(t, `
		type Counter { int n; bump(~this) { this.n = this.n + 1; } }
		via(Counter[]~ v) { v[0].bump(); }
	`)
}

func TestT0980_MutMethodThroughSlicedSharedBorrow(t *testing.T) {
	// The receiver place chains a SliceExpr under an IndexExpr; the whole place
	// still roots in the shared-borrow `v`, so the mutation is rejected.
	errs := checkErrs(t, `
		type Counter { int n; bump(~this) { this.n = this.n + 1; } }
		via(Counter[] v) { v[0:1][0].bump(); }
	`)
	expectError(t, errs, "cannot call mutating method 'bump' through a shared (read-only) borrow")
}

func TestT0980_MutMethodOnOwnedTempOK(t *testing.T) {
	// The receiver is the result of a call (an owned temporary, not a borrow), so
	// the place defaults to mutable and the `~this` call is allowed.
	checkOK(t, `
		type Counter { int n; bump(~this) { this.n = this.n + 1; } }
		make_counter() Counter { return Counter(n: 1); }
		via() { make_counter().bump(); }
	`)
}
