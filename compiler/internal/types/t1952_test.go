package types

import "testing"

// T1952: ReceiverBorrowMatches is the single definition of "may this concrete
// method's receiver fill this requirement's slot". SatisfiesAbstract rejects on
// it, and sema's reportOverrideMismatch asks the same question to pick its
// wording — so a drift here would make the diagnostic describe a different
// condition than the rejection.
//
// The behavioural coverage (which declarations are accepted or rejected, and the
// exact message) lives in internal/sema/t1734_test.go. These pin the predicate's
// own edges, including the nil arms its callers rely on but do not exercise.

func t1952SigWithRecv(ref RefMod) *Signature {
	return NewSignature(NewParam("this", nil, ref), nil, nil, false)
}

func TestT1952ReceiverBorrowMatchesSameKind(t *testing.T) {
	assertTrue(t, ReceiverBorrowMatches(t1952SigWithRecv(RefMut), t1952SigWithRecv(RefMut)), "~this satisfies ~this")
	assertTrue(t, ReceiverBorrowMatches(t1952SigWithRecv(RefNone), t1952SigWithRecv(RefNone)), "this satisfies this")
}

func TestT1952ReceiverBorrowMatchesRejectsEitherMismatch(t *testing.T) {
	// The reported drift: `close(this)` claiming `close!(~this)`. A shared
	// receiver cannot mutate, so it cannot release the resource the requirement
	// exists to release.
	assertFalse(t, ReceiverBorrowMatches(t1952SigWithRecv(RefNone), t1952SigWithRecv(RefMut)), "this must not claim ~this")
	// The other direction is the soundness half (T2185): a mutating method
	// filling a shared-receiver slot lets a read-only borrow of the view mutate
	// through it. An explicit `is` rejects both.
	assertFalse(t, ReceiverBorrowMatches(t1952SigWithRecv(RefMut), t1952SigWithRecv(RefNone)), "~this must not claim this")
}

func TestT1952ReceiverBorrowMatchesIgnoresReceiverlessMembers(t *testing.T) {
	// A `factory / `global / `mono member has no receiver at all. Factory-to-
	// factory matching is enforced elsewhere; this predicate must not reject the
	// pairing, or `is Parse` on a type whose parse! is a factory would break.
	noRecv := NewSignature(nil, nil, nil, false)
	assertTrue(t, ReceiverBorrowMatches(noRecv, t1952SigWithRecv(RefMut)), "receiver-less concrete defers to the arity check")
	assertTrue(t, ReceiverBorrowMatches(t1952SigWithRecv(RefMut), noRecv), "receiver-less requirement defers to the arity check")
	assertTrue(t, ReceiverBorrowMatches(noRecv, noRecv), "two receiver-less members")
}

func TestT1952ReceiverBorrowMatchesTolerateNilSignatures(t *testing.T) {
	// A signature that failed to resolve arrives as nil (see Identical's T1231
	// guard). Reporting a receiver mismatch for it would blame the receiver for
	// an unrelated earlier error.
	assertTrue(t, ReceiverBorrowMatches(nil, t1952SigWithRecv(RefMut)), "nil concrete")
	assertTrue(t, ReceiverBorrowMatches(t1952SigWithRecv(RefMut), nil), "nil abstract")
	assertTrue(t, ReceiverBorrowMatches(nil, nil), "both nil")
}
