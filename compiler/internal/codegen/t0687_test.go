package codegen

import (
	"strings"
	"testing"
)

// T0687: On the WASM target (cooperative scheduler), awaiting `<-task` from a
// non-coroutine helper function deadlocked at runtime — the spin shape emitted
// by genReceiveTask used pal_usleep, which is a no-op on the single-threaded
// WASM scheduler, so the pending `go {…}` G never ran.
//
// Fix mirrors the T0668 pattern for Task[T].drop: on WASM, the non-coroutine
// receive spin pumps promise_sched_coop_step() instead of pal_usleep, with
// the same deadlock-detection terminal message. Host path is unchanged
// (another OS thread runs the awaited G).
//
// Runtime correctness + zero-leak is enforced by
// tests/concurrency/t0687_await_in_helper_test.pr (host AND --target wasm32-wasi).
// These tests lock the structural IR signature.

// TestT0687_WasmNonCoroutineTaskReceivePumpsCoopStep — on wasm32-wasi the
// `<-task` spin inside a non-coroutine helper must pump
// promise_sched_coop_step() (not the no-op pal_usleep) and terminate genuine
// deadlocks with the shared message.
func TestT0687_WasmNonCoroutineTaskReceivePumpsCoopStep(t *testing.T) {
	ir := generateIRForTarget(t, `
		worker() int { return 42; }
		helper() int { task[int] x = go worker(); return <-x; }
		main() { print_line(helper()); }
	`, "wasm32-wasi")

	helper := findDefinedFunc(ir, "@__user.helper(")
	if helper == "" {
		t.Fatalf("expected helper to be defined")
	}
	if !strings.Contains(helper, "@promise_sched_coop_step") {
		t.Errorf("WASM helper's `<-task` spin must call @promise_sched_coop_step "+
			"(pump the cooperative scheduler), not a no-op usleep:\n%s", helper)
	}
	if strings.Contains(helper, "usleep(") {
		t.Errorf("WASM helper's `<-task` spin must NOT use the no-op pal_usleep:\n%s", helper)
	}
	if !strings.Contains(helper, ".str.deadlock.taskdrop") {
		t.Errorf("WASM helper's `<-task` spin must emit the terminal deadlock "+
			"message when coop_step makes no progress and G is not done:\n%s", helper)
	}
}

// TestT0687_HostNonCoroutineTaskReceiveStillUsesUsleep — on the host target the
// `<-task` spin keeps the usleep busy-wait (another M runs the awaited G) and
// must NOT reference the WASM-only coop-step pump.
func TestT0687_HostNonCoroutineTaskReceiveStillUsesUsleep(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		helper() int { task[int] x = go worker(); return <-x; }
		main() { print_line(helper()); }
	`)

	helper := findDefinedFunc(ir, "@__user.helper(")
	if helper == "" {
		t.Fatalf("expected helper to be defined")
	}
	if !strings.Contains(helper, "usleep(") {
		t.Errorf("host helper's `<-task` spin must keep the usleep busy-wait:\n%s", helper)
	}
	if strings.Contains(helper, "@promise_sched_coop_step") {
		t.Errorf("host helper's `<-task` spin must NOT pump the WASM-only "+
			"promise_sched_coop_step:\n%s", helper)
	}
}

// TestT0687_BlockFormHelperPumpsCoopStep — variant where the helper uses the
// `go { body }` block form instead of the call form. Both forms reach the same
// receive site in genReceiveTask and must get the same WASM fix.
func TestT0687_BlockFormHelperPumpsCoopStep(t *testing.T) {
	ir := generateIRForTarget(t, `
		helper() int { y := 9; task[int] x = go { y }; return <-x; }
		main() { print_line(helper()); }
	`, "wasm32-wasi")

	helper := findDefinedFunc(ir, "@__user.helper(")
	if helper == "" {
		t.Fatalf("expected helper to be defined")
	}
	if !strings.Contains(helper, "@promise_sched_coop_step") {
		t.Errorf("WASM block-form helper's `<-task` spin must pump coop_step:\n%s", helper)
	}
	if strings.Contains(helper, "usleep(") {
		t.Errorf("WASM block-form helper's `<-task` spin must NOT use pal_usleep:\n%s", helper)
	}
}

// TestT0687_CoroutineTaskReceiveUnchanged — a `<-task` directly in a coroutine
// context (e.g. main wrapped by wrapMainWithScheduler as `.goroutine.main`) on
// WASM must keep the cooperative park-suspend join (task.park) and must NOT
// regress into the legacy pump-spin shape.
func TestT0687_CoroutineTaskReceiveUnchanged(t *testing.T) {
	ir := generateIRForTarget(t, `
		worker() int { return 42; }
		main() { task[int] x = go worker(); print_line(<-x); }
	`, "wasm32-wasi")

	mainCoro := findDefinedFunc(ir, "@.goroutine.main(")
	if mainCoro == "" {
		t.Fatalf("expected .goroutine.main coroutine body to be defined")
	}
	if !strings.Contains(mainCoro, "task.park") {
		t.Errorf("WASM coroutine `<-task` must use the cooperative park-suspend "+
			"join (task.park block), not the spin pump:\n%s", mainCoro)
	}
	if strings.Contains(mainCoro, "usleep(") {
		t.Errorf("WASM coroutine `<-task` must NOT use the legacy spin pump "+
			"(pal_usleep is host-only):\n%s", mainCoro)
	}
}
