package codegen

import (
	"strings"
	"testing"
)

// T1218 — a contended Mutex[T].lock() inside a NON-coroutine function (e.g. a
// named top-level function spawned via `go grab(c)`, whose body is compiled once
// as the plain `@__user.grab`) must, on WASM, pump the cooperative scheduler
// instead of the no-op `pal_cond_wait`. Sibling of the T1200 channel fix: on
// single-threaded WASM pal_cond_wait/pal_mutex_* are stubs, so the contested-lock
// `cond_wait; recheck-held` loop busy-spins forever without yielding — the holder
// G never runs to release, and coop_step (with its per-test deadline) is never
// re-entered.

const t1218MutexSrc = `
	grab(Ref[Mutex[int]] c) { use guard := c.borrow.lock(); guard.borrow += 1; }
	main() {
		shared := Ref[Mutex[int]](Mutex[int](0));
		c := shared.clone();
		use held := shared.borrow.lock();
		go grab(c);
	}
`

func TestT1218_WasmNonCoroutineMutexLockPumpsCoopStep(t *testing.T) {
	ir := generateIRForTarget(t, t1218MutexSrc, "wasm32-wasi")

	grab := findDefinedFunc(ir, "@__user.grab(")
	if grab == "" {
		t.Fatalf("expected @__user.grab to be defined")
	}
	// The contested-lock wait must pump the cooperative scheduler.
	if !strings.Contains(grab, "@promise_sched_coop_step") {
		t.Errorf("WASM non-coroutine mutex lock must pump promise_sched_coop_step:\n%s", grab)
	}
	// ...and must NOT fall back to the no-op pal_cond_wait busy-spin.
	if strings.Contains(grab, "@pal_cond_wait") {
		t.Errorf("WASM non-coroutine mutex lock must NOT call the no-op pal_cond_wait:\n%s", grab)
	}
	// On coop_step==0 (no runnable G, still held) it takes the terminal deadlock exit(2).
	if !strings.Contains(grab, "@pal_exit") {
		t.Errorf("WASM mutex-lock wait pump must have a terminal deadlock exit path:\n%s", grab)
	}
}

// When the non-coroutine mutex-waiting function returns a value, the pump's
// per-test-deadline (coop_step==2) early-return must emit a *typed zero* return
// matching the function signature, not a `ret void`. Locks the non-void branch
// through emitWasmCoopWaitPump reached from the mutex path.
func TestT1218_WasmNonVoidMutexLockReturnsTypedZero(t *testing.T) {
	src := `
		grab(Ref[Mutex[int]] c) int { use guard := c.borrow.lock(); return guard.borrow; }
		main() {
			shared := Ref[Mutex[int]](Mutex[int](0));
			c := shared.clone();
			use held := shared.borrow.lock();
			task[int] t = go grab(c);
		}
	`
	ir := generateIRForTarget(t, src, "wasm32-wasi")

	grab := findDefinedFunc(ir, "@__user.grab(")
	if grab == "" {
		t.Fatalf("expected @__user.grab to be defined")
	}
	if !strings.HasPrefix(strings.TrimSpace(grab), "define i64 @__user.grab(") {
		t.Fatalf("expected grab to return i64 (int):\n%s", strings.SplitN(grab, "\n", 2)[0])
	}
	// The deadline early-return must be typed (ret i64 0), never `ret void`.
	if !strings.Contains(grab, "ret i64 0") {
		t.Errorf("non-void mutex-lock pump timeout must return a typed zero (ret i64 0):\n%s", grab)
	}
	if strings.Contains(grab, "ret void") {
		t.Errorf("non-void mutex-lock fn must never emit ret void:\n%s", grab)
	}
	if !strings.Contains(grab, "@promise_sched_coop_step") {
		t.Errorf("non-void non-coroutine mutex lock must still pump coop_step:\n%s", grab)
	}
}

// Native builds are unaffected: the non-coroutine mutex wait still blocks the OS
// thread via the real pal_cond_wait (another M runs the holder), and there is no
// cooperative pump (coop_step is WASM-only).
func TestT1218_NativeMutexLockUnchanged(t *testing.T) {
	ir := generateIRForTarget(t, t1218MutexSrc, "x86_64-unknown-linux-gnu")

	grab := findDefinedFunc(ir, "@__user.grab(")
	if grab == "" {
		t.Fatalf("expected @__user.grab to be defined")
	}
	if !strings.Contains(grab, "@pal_cond_wait") {
		t.Errorf("native non-coroutine mutex lock must still use pal_cond_wait:\n%s", grab)
	}
	if strings.Contains(grab, "@promise_sched_coop_step") {
		t.Errorf("native mutex lock must NOT pump the (WASM-only) coop_step:\n%s", grab)
	}
}
