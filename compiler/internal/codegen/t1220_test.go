package codegen

import (
	"strings"
	"testing"
)

// T1220 — a blocking `select` (no `default`) inside a NON-coroutine function
// (e.g. a named top-level function spawned via `go worker(a, b)`, whose body is
// compiled once as the plain `@__user.worker`) must, on WASM, pump the
// cooperative scheduler instead of silently falling through to the merge block.
// Sibling of the T1200 channel fix and T1218 mutex fix: on single-threaded WASM
// the try-chain branches straight to `mergeBlk` when no case is ready — the
// select completes having matched nothing, without ever yielding to let the
// peer G make the case ready.

const t1220SelectSrc = `
	worker(channel[int] a, channel[int] b) {
		select {
			v := <-a:
				if val := v { }
			v := <-b:
				if val := v { }
		}
	}
	main() {
		channel[int] a = channel[int]();
		channel[int] b = channel[int]();
		go worker(a, b);
		a.send(1);
	}
`

func TestT1220_WasmNonCoroutineSelectPumpsCoopStep(t *testing.T) {
	ir := generateIRForTarget(t, t1220SelectSrc, "wasm32-wasi")

	worker := findDefinedFunc(ir, "@__user.worker(")
	if worker == "" {
		t.Fatalf("expected @__user.worker to be defined")
	}
	// The blocking select must pump the cooperative scheduler.
	if !strings.Contains(worker, "@promise_sched_coop_step") {
		t.Errorf("WASM non-coroutine blocking select must pump promise_sched_coop_step:\n%s", worker)
	}
	// ...and must NOT silently fall through (the wait block must exist).
	if !strings.Contains(worker, "select.wasm.wait") {
		t.Errorf("WASM non-coroutine blocking select must have a coop-wait block:\n%s", worker)
	}
	// On coop_step==0 (no runnable G, no case ready) it takes the terminal deadlock exit(2).
	if !strings.Contains(worker, "@pal_exit") {
		t.Errorf("WASM select wait pump must have a terminal deadlock exit path:\n%s", worker)
	}
}

// When the non-coroutine select-waiting function returns a value, the pump's
// per-test-deadline (coop_step==2) early-return must emit a *typed zero* return
// matching the function signature, not a `ret void`. Locks the non-void branch
// through emitWasmCoopWaitPump reached from the select path.
func TestT1220_WasmNonVoidSelectReturnsTypedZero(t *testing.T) {
	src := `
		worker(channel[int] a, channel[int] b) int {
			select {
				v := <-a:
					if val := v { return val; }
				v := <-b:
					if val := v { return val; }
			}
			return -1;
		}
		main() {
			channel[int] a = channel[int]();
			channel[int] b = channel[int]();
			task[int] t = go worker(a, b);
			a.send(1);
		}
	`
	ir := generateIRForTarget(t, src, "wasm32-wasi")

	worker := findDefinedFunc(ir, "@__user.worker(")
	if worker == "" {
		t.Fatalf("expected @__user.worker to be defined")
	}
	if !strings.HasPrefix(strings.TrimSpace(worker), "define i64 @__user.worker(") {
		t.Fatalf("expected worker to return i64 (int):\n%s", strings.SplitN(worker, "\n", 2)[0])
	}
	// The deadline early-return must be typed (ret i64 0), never `ret void`.
	if !strings.Contains(worker, "ret i64 0") {
		t.Errorf("non-void select pump timeout must return a typed zero (ret i64 0):\n%s", worker)
	}
	if strings.Contains(worker, "ret void") {
		t.Errorf("non-void select fn must never emit ret void:\n%s", worker)
	}
	if !strings.Contains(worker, "@promise_sched_coop_step") {
		t.Errorf("non-void non-coroutine select must still pump coop_step:\n%s", worker)
	}
}

// Native builds are unaffected: the non-coroutine blocking select still uses the
// B0045 thread-blocking poll fallback (usleep + re-lock retry) — another M
// runs the peer G that makes a case ready — and there is no cooperative pump
// (coop_step is WASM-only).
func TestT1220_NativeSelectUnchanged(t *testing.T) {
	ir := generateIRForTarget(t, t1220SelectSrc, "x86_64-unknown-linux-gnu")

	worker := findDefinedFunc(ir, "@__user.worker(")
	if worker == "" {
		t.Fatalf("expected @__user.worker to be defined")
	}
	if !strings.Contains(worker, "@usleep") {
		t.Errorf("native non-coroutine blocking select must still use the usleep poll fallback:\n%s", worker)
	}
	if strings.Contains(worker, "@promise_sched_coop_step") {
		t.Errorf("native select must NOT pump the (WASM-only) coop_step:\n%s", worker)
	}
}
