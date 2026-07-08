package codegen

import (
	"strings"
	"testing"
)

// T1200 — a channel blocking op inside a NON-coroutine function (e.g. a named
// top-level function spawned via `go worker(c)`, whose body is compiled once as
// the plain `@__user.worker`) must, on WASM, pump the cooperative scheduler
// instead of the no-op `pal_cond_wait`. On single-threaded WASM
// pal_cond_wait/pal_mutex_* are stubs, so the classic `cond_wait; recheck` loop
// busy-spins forever without yielding — the partner G never runs (livelock, zero
// progress) and coop_step (with its per-test deadline check) is never re-entered.

const t1200ChannelSrc = `
	worker(channel[int] c) { int i = 0; while true { c.send(i); i = i + 1; } }
	main() { channel[int] c = channel[int](); go worker(c); int? _v = <-c; }
`

func TestT1200_WasmNonCoroutineChannelWaitPumpsCoopStep(t *testing.T) {
	ir := generateIRForTarget(t, t1200ChannelSrc, "wasm32-wasi")

	worker := findDefinedFunc(ir, "@__user.worker(")
	if worker == "" {
		t.Fatalf("expected @__user.worker to be defined")
	}
	// The rendezvous wait must pump the cooperative scheduler.
	if !strings.Contains(worker, "@promise_sched_coop_step") {
		t.Errorf("WASM non-coroutine channel wait must pump promise_sched_coop_step:\n%s", worker)
	}
	// ...and must NOT fall back to the no-op pal_cond_wait busy-spin.
	if strings.Contains(worker, "@pal_cond_wait") {
		t.Errorf("WASM non-coroutine channel wait must NOT call the no-op pal_cond_wait:\n%s", worker)
	}
	// On coop_step==2 (per-test deadline) it early-returns; on coop_step==0 (no
	// runnable G) it takes the terminal deadlock exit(2).
	if !strings.Contains(worker, "ret i8 2") && !strings.Contains(worker, "icmp eq i8") {
		t.Errorf("WASM channel wait pump must compare coop_step's result:\n%s", worker)
	}
	if !strings.Contains(worker, "@pal_exit") {
		t.Errorf("WASM channel wait pump must have a terminal deadlock exit path:\n%s", worker)
	}
}

// A NON-coroutine channel *receiver* (a named fn spawned via `go`, so its
// `<-c` runs in the plain `@__user.consumer`) must also pump coop_step on WASM
// instead of the no-op pal_cond_wait recv-empty busy-spin. Locks the
// genReceiveChannel isWasm branch.
func TestT1200_WasmNonCoroutineReceiveWaitPumpsCoopStep(t *testing.T) {
	src := `
		consumer(channel[int] c) { while true { int? v = <-c; } }
		main() { channel[int] c = channel[int](); go consumer(c); c.send(1); }
	`
	ir := generateIRForTarget(t, src, "wasm32-wasi")

	consumer := findDefinedFunc(ir, "@__user.consumer(")
	if consumer == "" {
		t.Fatalf("expected @__user.consumer to be defined")
	}
	if !strings.Contains(consumer, "@promise_sched_coop_step") {
		t.Errorf("WASM non-coroutine recv wait must pump promise_sched_coop_step:\n%s", consumer)
	}
	if strings.Contains(consumer, "@pal_cond_wait") {
		t.Errorf("WASM non-coroutine recv wait must NOT call the no-op pal_cond_wait:\n%s", consumer)
	}
	if !strings.Contains(consumer, "@pal_exit") {
		t.Errorf("WASM recv wait pump must have a terminal deadlock exit path:\n%s", consumer)
	}
}

// A NON-coroutine `for v in ch` receiver (a named fn spawned via `go`) must pump
// coop_step on WASM for its recv-empty wait. Locks the genForInChannel isWasm
// branch (the same fix as `<-c`, in stmt.go).
func TestT1200_WasmNonCoroutineForInChannelWaitPumpsCoopStep(t *testing.T) {
	src := `
		consumer(channel[int] c) { for v in c { } }
		main() { channel[int] c = channel[int](); go consumer(c); c.send(1); c.close(); }
	`
	ir := generateIRForTarget(t, src, "wasm32-wasi")

	consumer := findDefinedFunc(ir, "@__user.consumer(")
	if consumer == "" {
		t.Fatalf("expected @__user.consumer to be defined")
	}
	if !strings.Contains(consumer, "@promise_sched_coop_step") {
		t.Errorf("WASM non-coroutine for-in channel wait must pump promise_sched_coop_step:\n%s", consumer)
	}
	if strings.Contains(consumer, "@pal_cond_wait") {
		t.Errorf("WASM non-coroutine for-in channel wait must NOT call the no-op pal_cond_wait:\n%s", consumer)
	}
}

// When the non-coroutine channel-waiting function returns a value (e.g. a named
// `worker(...) int` spawned via `go`), the pump's per-test-deadline (coop_step==2)
// early-return must emit a *typed zero* return matching the function signature,
// not a `ret void` — otherwise the IR verifier rejects it. Locks the non-void
// branch of emitWasmChannelWaitPump's timeout block.
func TestT1200_WasmNonVoidChannelWaitReturnsTypedZero(t *testing.T) {
	src := `
		worker(channel[int] c) int { c.send(0); return 5; }
		main() { channel[int] c = channel[int](); task[int] t = go worker(c); int? r = <-t; }
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
		t.Errorf("non-void channel-wait pump timeout must return a typed zero (ret i64 0):\n%s", worker)
	}
	if strings.Contains(worker, "ret void") {
		t.Errorf("non-void channel-wait fn must never emit ret void:\n%s", worker)
	}
	if !strings.Contains(worker, "@promise_sched_coop_step") {
		t.Errorf("non-void non-coroutine channel wait must still pump coop_step:\n%s", worker)
	}
}

// Native builds are unaffected: the non-coroutine channel wait still blocks the
// OS thread via the real pal_cond_wait (another M runs the partner), and there is
// no cooperative pump (coop_step is WASM-only).
func TestT1200_NativeChannelWaitUnchanged(t *testing.T) {
	ir := generateIRForTarget(t, t1200ChannelSrc, "x86_64-unknown-linux-gnu")

	worker := findDefinedFunc(ir, "@__user.worker(")
	if worker == "" {
		t.Fatalf("expected @__user.worker to be defined")
	}
	if !strings.Contains(worker, "@pal_cond_wait") {
		t.Errorf("native non-coroutine channel wait must still use pal_cond_wait:\n%s", worker)
	}
	if strings.Contains(worker, "@promise_sched_coop_step") {
		t.Errorf("native channel wait must NOT pump the (WASM-only) coop_step:\n%s", worker)
	}
}
