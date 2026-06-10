package codegen

import (
	"strings"
	"testing"
)

// T0839: `MutexGuard[T].close()` had no codegen dispatch branch in
// genContainerMethodCall, so `g.close()` fell through to the generic
// monomorphized-method path and panicked with
// "codegen: undeclared method MutexGuard[int].close" (MutexGuard methods are
// codegen-emitted natively, not monomorphized). The fix routes close through
// the canonical unlock+free body (@MutexGuard.drop) AND clears the guard's
// scope-exit drop flag so the explicit close does not double-free/unlock.

// closeSiteSuppressesScopeDrop reports whether fn contains a MutexGuard.drop
// call immediately followed by a `store i1 false` to the guard's drop flag —
// the signature of the explicit close (genMutexGuardMethodCall: drop call +
// clearDropFlag). The eager flag clear is what prevents the scope-exit drop
// from running a second unlock/free on the same (now freed) guard.
func t0839CloseSuppressesScopeDrop(fn string) bool {
	const dropCall = "call void @MutexGuard.drop("
	idx := strings.Index(fn, dropCall)
	for idx >= 0 {
		// Look at the two statements following this drop call: the close site
		// emits the drop immediately followed by `store i1 false, i1* %g.dropflag`.
		window := fn[idx:]
		nl := strings.Index(window, "\n")
		if nl >= 0 {
			next := window[nl+1:]
			if strings.HasPrefix(strings.TrimSpace(next), "store i1 false, i1* %g.dropflag") {
				return true
			}
		}
		rel := strings.Index(fn[idx+len(dropCall):], dropCall)
		if rel < 0 {
			break
		}
		idx = idx + len(dropCall) + rel
	}
	return false
}

// Bound source: `g := m.lock(); g.close();`. The originally-panicking case in
// its simplest form (no optional unwrap needed to reproduce).
func TestT0839_MutexGuardCloseDispatched(t *testing.T) {
	ir := generateIR(t, `
		ub() {
			m := Mutex[int](5);
			g := m.lock();
			g.close();
		}
		main() {}
	`)
	fn := extractFunction(ir, "__user.ub")
	if fn == "" {
		t.Fatalf("__user.ub: function not found in IR")
	}
	if !strings.Contains(fn, "call void @MutexGuard.drop(") {
		t.Fatalf("__user.ub: missing @MutexGuard.drop call (close not dispatched)\n%s", fn)
	}
	if !t0839CloseSuppressesScopeDrop(fn) {
		t.Errorf("__user.ub: close did not clear the guard drop flag (would double-free at scope exit)\n%s", fn)
	}
}

// Optional-field source (the original ticket repro):
// `g := (h.mtx!).lock(); g.close();`. Guards against regression of the
// specific path reported in T0839.
func TestT0839_MutexGuardCloseOptionalField(t *testing.T) {
	ir := generateIR(t, `
		type MtxHolder { Mutex[int]? mtx; drop(~this) {} }
		uf() {
			h := MtxHolder(mtx: Mutex[int](5));
			g := (h.mtx!).lock();
			g.close();
		}
		main() {}
	`)
	fn := extractFunction(ir, "__user.uf")
	if fn == "" {
		t.Fatalf("__user.uf: function not found in IR")
	}
	if !strings.Contains(fn, "call void @MutexGuard.drop(") {
		t.Fatalf("__user.uf: missing @MutexGuard.drop call (close not dispatched)\n%s", fn)
	}
	if !t0839CloseSuppressesScopeDrop(fn) {
		t.Errorf("__user.uf: close did not clear the guard drop flag (would double-free at scope exit)\n%s", fn)
	}
}
