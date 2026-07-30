package ownership

import "testing"

// T1343: passing a borrowed (shared-borrow-parameter) non-Copy value into
// Vector.push whose receiver is a `~` mutable-borrow parameter must be rejected.
// Previously the consume-check for push's element was suppressed because the
// receiver's static type is a *MutRef wrapping the Vector, so IsVector was false
// and `storeNative` was never set — codegen still moved the element into the
// vector, so both the vector and the original owner dropped it (double-free).

func TestT1343PushBorrowedIntoMutBorrowVectorRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type R { int id; drop(~this) {} }
		leak_it(R r, R[]~ v) { v.push(r); }
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'r'")
}

// Owned vector target already rejected before T1343 — must stay rejected.
func TestT1343PushBorrowedIntoOwnedVectorRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type R { int id; drop(~this) {} }
		leak_it(R r) { R[] v = []; v.push(r); }
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'r'")
}

// Positive: pushing an owned local (moved) into a `~` vector is sound.
func TestT1343PushOwnedIntoMutBorrowVectorOK(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		fill(R[]~ v) { r := R(id: 1); v.push(move r); }
	`)
}

// Positive: consuming a `move`-declared parameter into a `~` vector is sound.
func TestT1343PushMoveParamIntoMutBorrowVectorOK(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		fill(R move r, R[]~ v) { v.push(move r); }
	`)
}

// Positive: an auto-dup element (string) borrowed as a plain param is duped by
// codegen at the push site, not moved — no double-free, must compile clean.
func TestT1343PushAutoDupIntoMutBorrowVectorOK(t *testing.T) {
	ownerOK(t, `
		fill(string s, string[]~ v) { v.push(s); }
	`)
}

// Reject (the owned-local flip side): before T1343, storeNative was false for a
// `~`-vector receiver, so pushing an OWNED local without `move` was silently
// accepted and the arg was modeled as a shared borrow — codegen still moved the
// element into the vector, so the still-"live" local and the vector both dropped
// it (double-free). With storeNative now true, enforceMoveMarker fires and the
// `move` marker is mandatory.
func TestT1343PushOwnedLocalIntoMutBorrowVectorNeedsMove(t *testing.T) {
	errs := ownerErrs(t, `
		type R { int id; drop(~this) {} }
		fill(R[]~ v) { r := R(id: 1); v.push(r); }
	`)
	expectOwnerError(t, errs, "consuming 'r' requires `move r`")
}

// Reject: an explicit `move` on a borrowed parameter must NOT bypass the
// consume-check. Pre-T1343 this configuration reported the misleading "this
// parameter borrows the argument — remove `move`" (it modeled push's elem as a
// borrow); the fix must instead reject the underlying move of a borrowed param.
func TestT1343MovedBorrowedParamIntoMutBorrowVectorRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type R { int id; drop(~this) {} }
		leak_it(R r, R[]~ v) { v.push(move r); }
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'r'")
}
