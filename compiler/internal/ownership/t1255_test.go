package ownership

import "testing"

// T1255 (residual to T1137): the T1137 reuse-after-alias verdict was a
// flow-INSENSITIVE textual heuristic. This left two in-scope cases wrong:
//
//  1. Loop reuse (FALSE NEGATIVE / silent UAF): the only textual use of the
//     source local is the aliasing call arg itself, so no "later use" flips the
//     candidate — yet iteration 2+ reuses the already-consumed handle. Fix B
//     (loop frames) flags a loop-body candidate whose source is not freshly
//     rebound at the top level of the body.
//  2. Mutually-exclusive branches (FALSE POSITIVE): a use of the source in a
//     sibling branch textually follows the aliasing call but is not
//     reachable-after it, so it must not flip the candidate. Fix A (branch-scoped
//     pendingAliasLocals) restores the pre-branch pending view for each
//     alternative and unions at the merge.

// --- Fix B: loop reuse now rejected (was a silent UAF) ---

func TestT1255_LoopReuseTaskRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			for i in 0..3 {
				int r = <-identity(t);
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't'")
}

func TestT1255_LoopReuseMutexRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			Mutex[int] m = Mutex[int](5);
			for i in 0..3 {
				Mutex[int] m2 = identity(m);
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse Mutex handle 'm'")
}

func TestT1255_InfiniteLoopReuseTaskRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			for {
				int r = <-identity(t);
				break;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't'")
}

func TestT1255_WhileLoopReuseTaskRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			bool c = true;
			while (c) {
				int r = <-identity(t);
				c = false;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't'")
}

func TestT1255_LoopReuseMutexGuardRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			Mutex[int] m = Mutex[int](5);
			MutexGuard[int] g = m.lock();
			for i in 0..3 {
				MutexGuard[int] g2 = identity(g);
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse MutexGuard handle 'g'")
}

// --- Fix B precision: a fresh top-level rebind per iteration is sound ---

// A `use g := …` guard freshly bound at the top level of the body dominates the
// back-edge, so aliasing it once per iteration is sound. Exercises the
// UseVarDecl arm of loopFreshBoundNames (the fresh-rebind shield for a
// `use`-declared MutexGuard).
func TestT1255_LoopUseBindGuardOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test() {
			Mutex[int] m = Mutex[int](5);
			for i in 0..3 {
				use g := m.lock();
				MutexGuard[int] g2 = identity(g);
			}
		}
	`)
}

// The item's explicitly-warned valid pattern: reassign a fresh handle at the top
// of each iteration, then alias it once. `t = go worker()` dominates the
// back-edge, so no reuse across iterations.
func TestT1255_LoopReassignFreshHandleOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			for i in 0..3 {
				t = go worker();
				int r = <-identity(t);
			}
		}
	`)
}

// A top-level reassign AFTER the aliasing call still dominates the back-edge:
// the handle consumed this iteration is replaced before the next alias, so no
// reuse. (Sound — accepted.)
func TestT1255_LoopReassignAfterCallOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			for i in 0..3 {
				int r = <-identity(t);
				t = go worker();
			}
		}
	`)
}

// A fresh declaration in the body binds a new handle each iteration — no reuse.
func TestT1255_LoopDeclaredInBodyOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test() {
			for i in 0..3 {
				task[int] t = go worker();
				int r = <-identity(t);
			}
		}
	`)
}

// --- Fix B soundness: a top-level rebind that a `continue` can SKIP does not
// dominate the back-edge, so it must not shield the candidate (else silent UAF) ---

// The rebind `t = go worker()` textually follows the aliasing call but sits
// after a `continue` that can bypass it: on the continue iteration the stale
// (already-consumed) handle survives to the next alias. Must be rejected.
func TestT1255_LoopRebindAfterContinueRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			bool skip = true;
			for i in 0..3 {
				int r = <-identity(t);
				if (skip) { continue; }
				t = go worker();
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't'")
}

// A top-level rebind BEFORE any continue barrier still dominates the back-edge
// (it runs every iteration before control can loop), so it stays sound.
func TestT1255_LoopRebindBeforeContinueOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			bool skip = true;
			for i in 0..3 {
				t = go worker();
				if (skip) { continue; }
				int r = <-identity(t);
			}
		}
	`)
}

// --- Fix A: mutually-exclusive branches no longer over-reject ---

func TestT1255_IfElseBranchReuseOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test(bool cond) {
			task[int] t = go worker();
			if (cond) {
				int r = <-identity(t);
			} else {
				int r2 = <-t;
			}
		}
	`)
}

func TestT1255_MatchArmReuseOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test(int n) {
			task[int] t = go worker();
			int r = match n {
				0 => <-identity(t),
				_ => <-t,
			};
		}
	`)
}

func TestT1255_SelectCaseReuseOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			channel[int] ch = channel[int]();
			channel[int] ch2 = channel[int]();
			select {
				v := <-ch:
					int r = <-identity(t);
				v2 := <-ch2:
					int r2 = <-t;
			}
		}
	`)
}

// --- Fix A precision: sequential reuse reachable-after is still rejected ---

// The aliasing call precedes the if; the branch `<-t` IS reachable after it, so
// the reuse must still be rejected (the pre-if pending carries the candidate
// into the branch).
func TestT1255_SequentialThenBranchReuseRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test(bool cond) {
			task[int] t = go worker();
			int r = <-identity(t);
			if (cond) {
				int r2 = <-t;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't'")
}

// Reuse in the fall-through after a match arm's aliasing call is still rejected:
// the union of arm pending carries the candidate to the post-match use.
func TestT1255_MatchThenFallthroughReuseRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test(int n) {
			task[int] t = go worker();
			int rr = match n {
				0 => <-identity(t),
				_ => 0,
			};
			int r2 = <-t;
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't'")
}

// --- Fix A precision: pick_first non-returned param in a branch stays sound ---

// pick_first returns `a`; reusing the NON-returned param (t2) in a sibling
// branch is sound and must not be rejected.
func TestT1255_BranchPickFirstNonReturnedOK(t *testing.T) {
	ownerOK(t, `
		worker() int { return 1; }
		pick_first[T](T a, T b) T { return a; }
		test(bool cond) {
			task[int] t1 = go worker();
			task[int] t2 = go worker();
			if (cond) {
				int r = <-pick_first(t1, t2);
			} else {
				int r2 = <-t2;
			}
		}
	`)
}
