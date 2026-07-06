package codegen

// T1207 — codegen drop-ordering coverage for the hidden-lock-escape fix.
//
// The fix lives in ownership's NLL last-use analysis (lastuse.go): a Mutex passed
// as a bare-borrow arg to a helper that also takes a `~`/`&` container-of-MutexGuard
// param must NOT be early-dropped at the call, because the callee can lock it and
// push the guard into the escaping container that outlives the Mutex.
//
// These tests exercise the effect at the IR level. Crucially they run the real
// `ownership.Check` pass first (unlike the default `generateIR` harness, which
// skips ownership and therefore never populates `info.EarlyDrops` — so the NLL
// force-clear can never appear there and force-clear assertions on it are vacuous).
// Running ownership here makes the positive/negative pair genuinely distinguishing:
// without the fix the negative and positive would BOTH emit the early force-clear;
// with it, only the negative (non-guard container) does.

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/ownership"
)

// generateIROwned runs parse → sema → ownership → codegen and returns the IR.
// Unlike generateIR it runs ownership.Check, which populates info.EarlyDrops —
// the input codegen consults to emit NLL early drops (and their `store i1 false`
// drop-flag force-clears). Required to observe the T1207 suppression at IR level.
func generateIROwned(t *testing.T, src string) string {
	t.Helper()
	file, info := parseWithStd(t, src)
	if errs := ownership.Check(file, info); len(errs) > 0 {
		t.Fatalf("unexpected ownership errors: %v", errs)
	}
	return Compile(file, info, "").Module.String()
}

// TestT1207HiddenLockEscapeVecNoEarlyDrop — positive control. The guard escapes
// via a `~vec` container param, so `m`'s NLL early drop at its last use
// (`push_g(v, m)`) MUST be suppressed and deferred to scope exit (after `v`).
// The tell-tale is the ABSENCE of an early force-clear on `m`'s drop flag.
func TestT1207HiddenLockEscapeVecNoEarlyDrop(t *testing.T) {
	ir := generateIROwned(t, `
		push_g(Vector[MutexGuard[int]] ~vec, Mutex[int] m) { vec.push(m.lock()); }
		main() {
			m := Mutex[int](17);
			v := Vector[MutexGuard[int]]();
			push_g(v, m);
			int n = v.len;
		}
	`)
	// Suppressed: no NLL early drop of `m` (the force-clear that precedes an
	// emitted early drop must be absent — present without the fix).
	assertNotContains(t, ir, "store i1 false, i1* %m.dropflag")
	// `m` still has its single flag-guarded scope-exit drop (no leak, no UAF).
	assertContains(t, ir, "%m.dropflag")
	assertContains(t, ir, `call void @"Mutex[int].drop"`)
}

// TestT1207HiddenLockEscapeMapNoEarlyDrop — the Map store family the issue calls
// out (`mp[k] = m.lock()` into a `~mp` param). Also exercises the "scan ALL type
// args" behavior of typeHasMutexGuardElem: the guard is Map's SECOND type arg, so
// the first (int) is skipped before the container is recognized as guard-bearing.
func TestT1207HiddenLockEscapeMapNoEarlyDrop(t *testing.T) {
	ir := generateIROwned(t, `
		put_g(Map[int, MutexGuard[int]] ~mp, Mutex[int] m) { mp[1] = m.lock(); }
		main() {
			m := Mutex[int](19);
			mp := Map[int, MutexGuard[int]]();
			put_g(mp, m);
			int n = mp.len;
		}
	`)
	assertNotContains(t, ir, "store i1 false, i1* %m.dropflag")
	assertContains(t, ir, "%m.dropflag")
	assertContains(t, ir, `call void @"Mutex[int].drop"`)
}

// TestT1207NonGuardContainerStillEarlyDrops — negative control. Identical shape to
// the positive Vec test except the escaping `~vec` param holds `int`, not
// `MutexGuard[int]`. No guard can escape, so the suppression must NOT fire and
// `m`'s NLL early drop at `push_i(v, m)` MUST still happen — the force-clear stays.
// Pins typeHasMutexGuardElem returning false for a non-guard container (the
// Instance type-arg scan finds no MutexGuard) and guards against over-suppression.
func TestT1207NonGuardContainerStillEarlyDrops(t *testing.T) {
	ir := generateIROwned(t, `
		push_i(Vector[int] ~vec, Mutex[int] m) { vec.push(0); }
		main() {
			m := Mutex[int](17);
			v := Vector[int]();
			push_i(v, m);
			int n = v.len;
		}
	`)
	// Early drop still fires: the force-clear precedes the NLL drop of `m`.
	assertContains(t, ir, "store i1 false, i1* %m.dropflag")
	assertContains(t, ir, `call void @"Mutex[int].drop"`)
}

// TestT1207NoGuardContainerParamStillEarlyDrops — second negative control: the
// helper takes a bare `Mutex[int]` arg but exposes NO container parameter at all,
// so callMayEscapeMutexGuardFromMutexArg finds a Mutex arg yet no escaping
// guard-container param and returns false. `m`'s early drop must still fire.
func TestT1207NoGuardContainerParamStillEarlyDrops(t *testing.T) {
	ir := generateIROwned(t, `
		sink(Mutex[int] m) { int x = 0; }
		main() {
			m := Mutex[int](17);
			sink(m);
			int n = 5;
		}
	`)
	assertContains(t, ir, "store i1 false, i1* %m.dropflag")
	assertContains(t, ir, `call void @"Mutex[int].drop"`)
}
