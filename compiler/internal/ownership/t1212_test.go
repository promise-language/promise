package ownership

import "testing"

// T1212: the Some-wrap coercion `Mutex[int]?? x = m` (m: Mutex[int]? borrowed,
// LHS wrap depth 2 > RHS depth 1) is legitimately allowed by the var-decl
// carve-out (it materializes a fresh outer Optional, so in-scope drop is sound —
// T1138 requires this to stay valid). But the inner handle it wraps is still
// borrowed, so ESCAPING the owned local `x` (return, call-arg consume, rebind
// then return, channel send) aliases the caller's live single-owner handle and
// double-frees (dupOptionalVectorElem has no clone). recordWrapCoercedBorrowedHandle
// tracks the provenance at the var-decl and rejectWrapCoercedHandleEscapeExpr
// rejects the escape while leaving the in-scope drop valid.

// --- Rejected: escapes of a wrap-coerced borrowed handle local ---

func TestT1212_WrapCoerceReturnMutexRejected(t *testing.T) {
	// The repro: return the wrap-coerced local.
	errs := ownerErrs(t, `
		wrapcoerce(Mutex[int]? m) Mutex[int]?? {
			Mutex[int]?? x = m;
			return x;
		}
	`)
	expectOwnerError(t, errs, "wrap-coerces the borrowed single-owner `Mutex` handle 'm'")
	expectOwnerError(t, errs, "Declare the parameter 'm' with `move`")
}

func TestT1212_WrapCoerceReturnTaskRejected(t *testing.T) {
	errs := ownerErrs(t, `
		wrapcoerce(task[int]? m) task[int]?? {
			task[int]?? x = m;
			return x;
		}
	`)
	expectOwnerError(t, errs, "wrap-coerces the borrowed single-owner `task` handle 'm'")
}

func TestT1212_WrapCoerceCallArgConsumeRejected(t *testing.T) {
	// Escape via a `~`-arg consume (push into a Vector).
	errs := ownerErrs(t, `
		wrapcoerce(Mutex[int]? m, Mutex[int]??[] v) {
			Mutex[int]?? x = m;
			v.push(x);
		}
	`)
	expectOwnerError(t, errs, "wrap-coerces the borrowed single-owner `Mutex` handle 'm'")
}

func TestT1212_WrapCoerceRebindThenReturnRejected(t *testing.T) {
	// Escape via rebind (`y := x` moves x) then return — the rebind is the move
	// site the reject fires on.
	errs := ownerErrs(t, `
		wrapcoerce(Mutex[int]? m) Mutex[int]?? {
			Mutex[int]?? x = m;
			y := x;
			return y;
		}
	`)
	expectOwnerError(t, errs, "wrap-coerces the borrowed single-owner `Mutex` handle 'm'")
}

func TestT1212_WrapCoerceReassignedFromBorrowedRejected(t *testing.T) {
	// Guards the delete()-on-reassign safety claim: untracking `x` when it is
	// reassigned must NOT open a hole. Here the RHS `y` is itself a tracked
	// borrowed wrap-coerce, so consuming it into `x` is caught at the assignment's
	// tryMoveConsume (on `y`) BEFORE the delete() untracks `x` — the escape is
	// rejected pointing at y's source, and the later `return x` is moot.
	errs := ownerErrs(t, `
		wrapcoerce(Mutex[int]? m, Mutex[int]? n) Mutex[int]?? {
			Mutex[int]?? x = m;
			Mutex[int]?? y = n;
			x = y;
			return x;
		}
	`)
	expectOwnerError(t, errs, "wrap-coerces the borrowed single-owner `Mutex` handle 'n'")
}

// --- Allowed: sound shapes that must not be over-rejected ---

func TestT1212_OwnedWrapCoerceReturnOK(t *testing.T) {
	// The RHS handle is OWNED (a fresh local), so `x = m` is a real move and the
	// single handle transfers cleanly to the caller — must stay valid. The reject
	// is strictly gated on Borrowed state.
	ownerOK(t, `
		wrapcoerce() Mutex[int]?? {
			Mutex[int]? m = Mutex[int](0);
			Mutex[int]?? x = m;
			return x;
		}
	`)
}

func TestT1212_WrapCoerceDropOnlyOK(t *testing.T) {
	// In-scope drop of a borrowed wrap-coerce is sound (codegen clears the inner
	// alias's drop flag) — no escape, so not rejected.
	ownerOK(t, `
		droponly(Mutex[int]? m) {
			Mutex[int]?? x = m;
		}
	`)
}

func TestT1212_WrapCoerceReassignedToOwnedReturnOK(t *testing.T) {
	// Reassigning the tracked local to a fresh OWNED wrap-coerce drops the old
	// borrowed-inner alias (sound) and makes `x` truly own its value — returning
	// it is safe. The stale provenance must be cleared on reassignment so this is
	// not a false positive. (A borrowed wrap-coerce RHS is instead rejected at the
	// assignment's tryMoveConsume, so untracking opens no hole.)
	ownerOK(t, `
		wrapcoerce(Mutex[int]? m) Mutex[int]?? {
			Mutex[int]?? x = m;
			Mutex[int]? owned = Mutex[int](5);
			Mutex[int]?? fresh = owned;
			x = fresh;
			return x;
		}
	`)
}

func TestT1212_MoveWrapCoerceReturnOK(t *testing.T) {
	// A `move` param makes `m` owned, so `x = m` is a real move — the fragile
	// launder+optional+handle combo must stay valid.
	ownerOK(t, `
		wrapcoerce(Mutex[int]? move m) Mutex[int]?? {
			Mutex[int]?? x = m;
			return x;
		}
	`)
}
