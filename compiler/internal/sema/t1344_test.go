package sema

// T1344: mutating a method through a shared borrow of a `structural` interface
// value must be rejected, the same as through the concrete receiver. The prior
// hole: the abstract method's receiver ref (RefNone) was what
// checkReceiverMutability read, so upcasting a concrete `~this` mutator to its
// interface bypassed the T1053 shared-borrow check. Marking the abstract
// mutator `~this` closes it — both the concrete and interface-typed calls are
// now rejected.

import (
	"strings"
	"testing"
)

// Iterator[int].next() is now `next(~this)`, so calling it through a shared
// (bare) borrow of an Iterator[int] value is rejected.
func TestT1344_IteratorNextThroughSharedBorrowRejected(t *testing.T) {
	errs := checkErrs(t, `
		advance(Iterator[int] it) int { if v := it.next() { return v; } return -1; }
	`)
	expectError(t, errs, "cannot call mutating method 'next' through a shared (read-only) borrow")
}

// A mutating combinator (which internally advances `this`) is likewise rejected
// through a shared borrow of the interface.
func TestT1344_IteratorCollectThroughSharedBorrowRejected(t *testing.T) {
	errs := checkErrs(t, `
		take_all(Iterator[int] it) int[] { return it.collect(); }
	`)
	expectError(t, errs, "cannot call mutating method 'collect' through a shared (read-only) borrow")
}

// A user-defined `structural` interface with a `~this` abstract mutator is
// rejected through a shared borrow of the interface type (the fix generalizes
// beyond the stdlib Iterator).
func TestT1344_UserStructuralMutatorThroughSharedBorrowRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Sink[T] `+"`structural"+` {
			push(~this, T v) bool `+"`abstract"+`;
		}
		feed(Sink[int] s) { s.push(1); }
	`)
	expectError(t, errs, "cannot call mutating method 'push' through a shared (read-only) borrow")
}

// The same mutator IS allowed through a `~` mutable borrow of the interface.
func TestT1344_IteratorNextThroughMutableBorrowAllowed(t *testing.T) {
	errs := checkErrs(t, `
		advance(Iterator[int]~ it) int { if v := it.next() { return v; } return -1; }
	`)
	expectNoErrorContaining(t, errs, "through a shared")
}

// And allowed through an owned (`move`) interface value — the receiver is fully
// owned, so the `~this` mutator is legal.
func TestT1344_IteratorNextThroughOwnedValueAllowed(t *testing.T) {
	errs := checkErrs(t, `
		advance(Iterator[int] move it) int { if v := it.next() { return v; } return -1; }
	`)
	expectNoErrorContaining(t, errs, "through a shared")
}

// Consistency (the core of the bug): the SAME logical mutation is rejected on
// both the concrete receiver AND the interface. Before T1344 the concrete call
// was rejected but upcasting to the structural interface bypassed the check.
func TestT1344_ConcreteAndInterfaceBothRejected(t *testing.T) {
	src := `
		type C is Iterator[int] {
			int cur; int lim;
			next(~this) int? {
				if this.cur < this.lim { v := this.cur; this.cur = this.cur + 1; return v; }
				return none;
			}
		}
		call_concrete(C c) int { if v := c.next() { return v; } return -1; }
		call_iface(Iterator[int] it) int { if v := it.next() { return v; } return -1; }
	`
	errs := checkErrs(t, src)
	// Two distinct rejections: one for the concrete `c`, one for the interface `it`.
	n := 0
	for _, e := range errs {
		if strings.Contains(e.Error(), "cannot call mutating method 'next' through a shared (read-only) borrow") {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("expected both the concrete and interface call to be rejected (>=2 shared-borrow errors), got %d:\n%v", n, errs)
	}
}
