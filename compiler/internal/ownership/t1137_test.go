package ownership

import (
	"strings"
	"testing"
)

// T1137: a generic function/method that returns its borrowed (non-`move`) param,
// instantiated over a BARE single-owner handle (task/Mutex/MutexGuard), makes the
// call result an alias of the caller's source local. A SINGLE use of the result
// is sound (codegen clears the source's drop flag), but REUSING the source local
// after the aliasing call is a use-after-free — the handle was already consumed.
// The residual T1102/T1213 hole: inside the generic body the param is a bare
// TypeParam, so the concrete rejects cannot see it is a single-owner handle, and
// the bare-handle instantiation is deliberately kept valid for the single-use
// form. These tests pin the reuse-after rejection (precise per returned param)
// and confirm the single-use / non-returned-param / reassign forms stay valid.

const t1137Identity = `identity[T](T x) T { return x; }
worker() int { return 1; }
`

// --- Rejected: reuse-after-alias ---

func TestT1137_FreeFuncReuseTaskRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			int r = <-identity(t);
			int r2 = <-t;
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't'")
}

func TestT1137_MethodReuseTaskRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 1; }
		type Wrap {
			int id;
			forward[T](T x) T { return x; }
		}
		test() {
			Wrap w = Wrap(id: 1);
			task[int] t = go worker();
			int r = <-w.forward(t);
			int r2 = <-t;
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't'")
}

func TestT1137_MutexReuseRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			Mutex[int] m = Mutex[int](5);
			Mutex[int] m2 = identity(m);
			use g := m.lock();
		}
	`)
	expectOwnerError(t, errs, "cannot reuse Mutex handle 'm'")
}

func TestT1137_MutexGuardReuseRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			Mutex[int] mm = Mutex[int](5);
			MutexGuard[int] g = mm.lock();
			MutexGuard[int] g2 = identity(g);
			int v = g.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot reuse MutexGuard handle 'g'")
}

// Precision: pick_first returns `a`; reusing the RETURNED source local (t1) is
// a UAF and must be rejected.
func TestT1137_PickFirstReuseReturnedRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 1; }
		pick_first[T](T a, T b) T { return a; }
		test() {
			task[int] t1 = go worker();
			task[int] t2 = go worker();
			int r = <-pick_first(t1, t2);
			int r1 = <-t1;
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't1'")
}

// Precision: a `move` param consumes the arg, so `move t; <-t` is already a
// moved-variable use — the T1137 reuse rejection must NOT fire (it would be a
// confusing duplicate). Confirms the `arg.Move` carve-out in
// recordAliasHandleReuseCandidates.
func TestT1137_MoveArgFallsBackToMovedVariable(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 1; }
		identity_move[T](T move x) T { return x; }
		test() {
			task[int] t = go worker();
			int r = <-identity_move(move t);
			int r2 = <-t;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 't'")
	expectNoOwnerError(t, errs, "cannot reuse task handle")
}

// A generic callee with TWO return sites binding the same param produces two
// matching returnHandleReqs; the reuse must be reported exactly once
// (emitted-key dedup in propagateReturnHandleReqs).
func TestT1137_MultiReturnSiteSingleError(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 1; }
		dup_return[T](T x, bool c) T { if (c) { return x; } return x; }
		test() {
			task[int] t = go worker();
			int r = <-dup_return(t, true);
			int r2 = <-t;
		}
	`)
	n := 0
	for _, e := range errs {
		if strings.Contains(e.Error(), "cannot reuse task handle 't'") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one reuse error, got %d: %v", n, errs)
	}
}

// --- Allowed: single-use / non-returned param / reassign-then-use ---

func TestT1137_SingleUseTaskOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			int r = <-identity(t);
		}
	`)
}

// Precision: pick_first returns `a`; reusing the NON-returned source local (t2)
// is sound — t2 was never aliased into the result, so it must NOT be rejected.
func TestT1137_PickFirstReuseNonReturnedOK(t *testing.T) {
	ownerOK(t, `
		worker() int { return 1; }
		pick_first[T](T a, T b) T { return a; }
		test() {
			task[int] t1 = go worker();
			task[int] t2 = go worker();
			int r = <-pick_first(t1, t2);
			int r2 = <-t2;
		}
	`)
}

// Reassigning the source local before the later use binds a FRESH handle, so the
// later use is not a reuse of the aliased one — must NOT be rejected.
func TestT1137_ReassignThenUseOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			int r = <-identity(t);
			t = go worker();
			int r2 = <-t;
		}
	`)
}
