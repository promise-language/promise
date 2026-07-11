package ownership

import "testing"

// T1256 (residual to T1255): T1255's Fix B (aliasLoopFrame) flags loop-back-edge
// reuse of a bare single-owner handle only for aliasing calls in the loop BODY.
// Positions that are checked once textually yet RE-EVALUATED every iteration were
// not covered because they were checked BEFORE enterLoopBody raised the frame:
//
//   - the while / while-unwrap CONDITION/VALUE,
//   - the classic-for CONDITION.
//
// The fix pushes the aliasLoopFrame BEFORE checking these per-iteration
// expressions, so recordAliasHandleReuseCandidates lands their candidates on the
// loop frame and exitLoopBody evaluates them for back-edge reuse — turning the
// silent UAF into a compile error. Once-eval positions (classic-for INIT, for-in
// ITERABLE) stay OUTSIDE the frame so a single sound alias is not misflagged.

// --- Rejected: reuse in a per-iteration condition/value (was a silent UAF) ---

func TestT1256_WhileCondReuseTaskRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			int i = 0;
			while (i < 3 && <-identity(t) == 1) {
				i = i + 1;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't'")
}

// The alias lives in the while-unwrap VALUE expression itself (re-evaluated every
// iteration), NOT the body — this is the position T1256 newly covers. A body-only
// alias was already caught by T1255's body frame, so it would pass without the fix.
func TestT1256_WhileUnwrapValueReuseTaskRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		make_opt(int v) int? { return v; }
		test() {
			task[int] t = go worker();
			while v := make_opt(<-identity(t)) {
				break;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't'")
}

func TestT1256_ClassicForCondReuseTaskRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			for int i = 0; i < 3 && <-identity(t) == 1; i = i + 1 {
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't'")
}

func TestT1256_ClassicForCondReuseMutexGuardRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			Mutex[int] m = Mutex[int](5);
			MutexGuard[int] g = m.lock();
			for int i = 0; i < 3; i = i + 1 {
				MutexGuard[int] g2 = identity(g);
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse MutexGuard handle 'g'")
}

// Guard: the classic-for UPDATE clause was already checked INSIDE the frame
// before T1256 — reuse there must stay rejected (locks in prior behavior).
func TestT1256_ClassicForUpdateReuseTaskRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			int acc = 0;
			for int i = 0; i < 3; acc = acc + <-identity(t) {
				i = i + 1;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't'")
}

// --- Accepted: no false positives ---

// The for-in ITERABLE is evaluated ONCE (before the first iteration), so aliasing
// a handle there is a single sound use — it must NOT be flagged. This is why the
// frame is pushed AFTER checking the iterable.
func TestT1256_ForInIterableAliasOnceOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			for x in [<-identity(t)] {
				int y = x;
			}
		}
	`)
}

// The classic-for INIT is evaluated once — aliasing a handle there is sound and
// must not be flagged (the frame is pushed after the init).
func TestT1256_ClassicForInitAliasOnceOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			for int r = <-identity(t); r < 0; r = r + 1 {
			}
		}
	`)
}

// A fresh handle rebound at the top of each body iteration dominates the
// back-edge, so aliasing it in the condition of the NEXT iteration is sound. The
// freshBound shield (computed from the body's top-level rebinds) covers the
// condition alias.
func TestT1256_WhileCondFreshRebindInBodyOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			int i = 0;
			while (<-identity(t) == 1 && i < 3) {
				i = i + 1;
				t = go worker();
			}
		}
	`)
}

// Accept guard for the newly-framed while-unwrap VALUE position: a fresh handle
// rebound at the top of each body iteration dominates the back-edge, so aliasing
// it in the NEXT iteration's unwrapped value is sound and must not be flagged.
func TestT1256_WhileUnwrapValueFreshRebindOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		make_opt(int v) int? { return v; }
		test() {
			task[int] t = go worker();
			int n = 0;
			while v := make_opt(<-identity(t)) {
				n = n + 1;
				t = go worker();
				if n >= 3 { break; }
			}
		}
	`)
}

// Accept guard for the newly-framed classic-for CONDITION position: a fresh
// handle rebound in the body dominates the back-edge, so the next iteration's
// condition alias is sound.
func TestT1256_ClassicForCondFreshRebindOK(t *testing.T) {
	ownerOK(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			for int i = 0; <-identity(t) == 1 && i < 3; i = i + 1 {
				t = go worker();
			}
		}
	`)
}

// Nested loops: reuse in the INNER while condition must be rejected. The fix
// pushes the inner frame before checking the inner condition, so the candidate
// lands on the innermost (correct) frame rather than the enclosing outer one.
func TestT1256_NestedInnerWhileCondReuseRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		test() {
			task[int] t = go worker();
			int j = 0;
			while (j < 2) {
				j = j + 1;
				int i = 0;
				while (i < 3 && <-identity(t) == 1) {
					i = i + 1;
				}
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse task handle 't'")
}

// Kind × position guard: a Mutex handle (not task) reused in the classic-for
// UPDATE clause stays rejected — locks in the update-position behavior across
// the single-owner-handle kinds (task covered above, Mutex here).
func TestT1256_ClassicForUpdateReuseMutexRejected(t *testing.T) {
	errs := ownerErrs(t, t1137Identity+`
		sink(Mutex[int] m) {}
		test() {
			Mutex[int] mx = Mutex[int](5);
			for int i = 0; i < 3; sink(identity(mx)) {
				i = i + 1;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot reuse Mutex handle 'mx'")
}
