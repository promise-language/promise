package codegen

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/wasmweb"
)

// Tests for the wasm32-web reactor top level (docs/web-apps.md §4,
// §19 phase 1). These are the Go unit / IR-shape tests §17 requires: the
// exports exist with the right signatures, `_initialize` has no unconditional
// trailing pal_exit, the pump respects its step/time budget, and names come
// from internal/wasmweb. Runtime behaviour (the liveness rule actually
// keeping an instance alive) needs a real host and is exercised by the
// browser integration tests of §19 phase 6, not here — modules/web/web_test.pr
// drives it from Promise source at the module level (Go IR-shape tests plus
// Promise-level tests are the first two of §17's three required layers; a
// real browser is the third).

const trivialWasmWebProgram = `main() { print_line("hi"); }`

// The reactor globals exist with the names internal/wasmweb defines, so
// bindgen's .pr/.js generators (§14.1) can never invent a different name for
// the same thing.
func TestWasmWebReactorGlobalsExist(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	assertContains(t, ir, "@promise_web_live_registrations = global i32 0")
	assertContains(t, ir, "@promise_web_in_pump = global i8 0")
	assertContains(t, ir, "@promise_web_terminated = global i8 0")
}

// wasm32-wasi must not gain any of this — it is unaffected (§18 non-goal).
func TestWasmWebReactorGlobalsAbsentOnWasi(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-wasi")
	assertNotContains(t, ir, wasmweb.GlobalLiveRegistrations)
	assertNotContains(t, ir, wasmweb.GlobalInPump)
	assertNotContains(t, ir, wasmweb.ExportPump)
	assertNotContains(t, ir, "promise_web_reactor_drain")
}

// `_initialize` must not unconditionally pal_exit — the liveness rule (§4.1)
// requires a live-registrations check to gate the exit, so a future
// Phase-2 registration can keep the instance alive. This is the one
// assertion in this file that would fail on the pre-Phase-1 behaviour
// (unconditional `call void @pal_exit(i32 %code)` followed by unreachable).
func TestInitializeGatesExitOnLiveness(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	initFn := extractDefine(ir, wasmweb.ExportInitialize)
	if initFn == "" {
		t.Fatalf("expected @_initialize to be defined\ngot:\n%s", ir)
	}
	if !strings.Contains(initFn, "call i32 @main(") {
		t.Errorf("expected _initialize to call @main and inspect its result\ngot:\n%s", initFn)
	}
	if !strings.Contains(initFn, "icmp ne i32") {
		t.Errorf("expected _initialize to branch on main's result (the liveness gate) rather than exit unconditionally\ngot:\n%s", initFn)
	}
	if !strings.Contains(initFn, "ret void") {
		t.Errorf("expected _initialize to have a plain `ret void` path (stay alive) alongside the exit path\ngot:\n%s", initFn)
	}
	if !strings.Contains(initFn, "call void @pal_exit(i32 0)") {
		t.Errorf("expected _initialize to still exit(0) on the drained-clean path\ngot:\n%s", initFn)
	}
}

// wasm32-wasi's `_start` keeps its old unconditional shape: call @main, then
// exit with whatever it returned, no branch.
func TestStartExitsUnconditionallyOnWasi(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-wasi")
	startFn := extractDefine(ir, "_start")
	if startFn == "" {
		t.Fatalf("expected @_start to be defined\ngot:\n%s", ir)
	}
	if strings.Contains(startFn, "icmp") {
		t.Errorf("expected wasm32-wasi's _start to have no branch (unconditional exit), unlike wasm32-web's _initialize\ngot:\n%s", startFn)
	}
	if !strings.Contains(startFn, "call void @pal_exit(") {
		t.Errorf("expected _start to call pal_exit unconditionally\ngot:\n%s", startFn)
	}
}

// The exported pump exists with the documented signature (§14) and is
// actually passed to the linker via --export (checked separately in the
// cmd/promise package — this only checks the LLVM-level export exists).
func TestWebPumpExportSignature(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	assertContains(t, ir, "define i32 @promise_web_pump() nounwind")
}

// promise_sched_coop_run must still be emitted on wasm32-web — it is the
// unaffected driver for GenerateTestMain's per-test batches (tests are not
// reactors), even though ordinary program mains now go through
// promise_web_reactor_drain instead.
func TestCoopRunStillEmittedOnWasmWeb(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	assertContains(t, ir, "define void @promise_sched_coop_run() nounwind")
}

// @main's own body must call the new unbounded drain, not the old
// unconditional promise_sched_coop_run call, on wasm32-web.
func TestMainCallsReactorDrainOnWasmWeb(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	mainFn := extractDefine(ir, "main")
	if mainFn == "" {
		t.Fatalf("expected @main to be defined\ngot:\n%s", ir)
	}
	if !strings.Contains(mainFn, "call i8 @promise_web_reactor_drain()") {
		t.Errorf("expected @main to call promise_web_reactor_drain on wasm32-web\ngot:\n%s", mainFn)
	}
	if strings.Contains(mainFn, "call void @promise_sched_coop_run()") {
		t.Errorf("expected @main to NOT call promise_sched_coop_run directly on wasm32-web (that's GenerateTestMain's driver, not the program driver)\ngot:\n%s", mainFn)
	}
}

// @main's body must still call promise_sched_coop_run on wasm32-wasi,
// unchanged.
func TestMainCallsCoopRunOnWasi(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-wasi")
	mainFn := extractDefine(ir, "main")
	if mainFn == "" {
		t.Fatalf("expected @main to be defined\ngot:\n%s", ir)
	}
	if !strings.Contains(mainFn, "call void @promise_sched_coop_run()") {
		t.Errorf("expected @main to call promise_sched_coop_run on wasm32-wasi\ngot:\n%s", mainFn)
	}
	assertNotContains(t, ir, "promise_web_reactor_drain")
}

// The pump respects a step budget (§4.2): the loop body must compare the step
// counter against a budget and, separately, gate a wall-clock budget check
// on the step counter (amortized clock reads).
func TestWebPumpRespectsStepAndTimeBudget(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	pumpFn := extractDefine(ir, wasmweb.ExportPump)
	if pumpFn == "" {
		t.Fatalf("expected @promise_web_pump to be defined\ngot:\n%s", ir)
	}
	if !strings.Contains(pumpFn, "icmp sge i64") {
		t.Errorf("expected the pump to compare the step counter against a budget\ngot:\n%s", pumpFn)
	}
	if !strings.Contains(pumpFn, "srem i64") {
		t.Errorf("expected the pump to gate the wall-clock check on the step counter (amortized clock reads)\ngot:\n%s", pumpFn)
	}
	if !strings.Contains(pumpFn, "@promise_env.monotonic_nanos()") {
		t.Errorf("expected the pump to read the monotonic clock for its time budget\ngot:\n%s", pumpFn)
	}
	if !strings.Contains(pumpFn, "call void @promise_env_schedule_pump(i32 0, i32 0)") {
		t.Errorf("expected the pump to schedule a continuation (ASAP, delay 0) on budget exhaustion\ngot:\n%s", pumpFn)
	}
}

// schedule_pump is declared as a wasm import carrying the shared names from
// internal/wasmweb, not an ad hoc string (§14.1).
func TestSchedulePumpImportAttrs(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	if !strings.Contains(ir, "declare void @promise_env_schedule_pump(i32") {
		t.Errorf("expected a declared (imported) promise_env_schedule_pump(i32, i32)\ngot:\n%s", ir)
	}
	if !strings.Contains(ir, `"wasm-import-module"="promise_env"`) {
		t.Errorf("expected the schedule_pump import to carry wasm-import-module=promise_env")
	}
	if !strings.Contains(ir, `"wasm-import-name"="schedule_pump"`) {
		t.Errorf("expected the schedule_pump import to carry wasm-import-name=schedule_pump")
	}
}

// Re-entrancy guard (§5): the pump checks promise_web_in_pump before doing
// any work and checks promise_web_terminated before that (§10).
func TestWebPumpGuardsReentrancyAndFault(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	pumpFn := extractDefine(ir, wasmweb.ExportPump)
	if pumpFn == "" {
		t.Fatalf("expected @promise_web_pump to be defined\ngot:\n%s", ir)
	}
	if !strings.Contains(pumpFn, "load i8, i8* @promise_web_terminated") {
		t.Errorf("expected the pump to check promise_web_terminated first (§10)\ngot:\n%s", pumpFn)
	}
	if !strings.Contains(pumpFn, "load i8, i8* @promise_web_in_pump") {
		t.Errorf("expected the pump to check promise_web_in_pump (§5 — nesting refused, not supported)\ngot:\n%s", pumpFn)
	}
	if !strings.Contains(pumpFn, "ret i32 2") {
		t.Errorf("expected a PumpTerminated (2) return path\ngot:\n%s", pumpFn)
	}
}

// The generated batch-test main returns an exit code — 1 when a test failed —
// not a liveness verdict, so its `_initialize` exits with that code exactly as
// wasm32-wasi's `_start` does. Read as "stay alive", a failing batch returned
// from `_initialize` and the harness exited 0. A file with its own main already
// has the program's liveness-gated `_initialize` when the batch main replaces
// that main; it is rebuilt, not kept.
func TestTestMainInitializeExitsWithBatchResult(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"tests_only", "t_one() `test { int x = 1; }\n"},
		{"with_its_main", "main() { print_line(\"hi\"); }\nt_one() `test { int x = 1; }\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ir := generateTestMainIRForTarget(t, c.src, "wasm32-web")
			if n := strings.Count(ir, "define void @"+wasmweb.ExportInitialize+"("); n != 1 {
				t.Errorf("expected one definition of @%s, got %d", wasmweb.ExportInitialize, n)
			}
			initFn := extractDefine(ir, wasmweb.ExportInitialize)
			if initFn == "" {
				t.Fatalf("expected @_initialize to be defined\ngot:\n%s", ir)
			}
			if !strings.Contains(initFn, "%0 = call i32 @main(") || !strings.Contains(initFn, "call void @pal_exit(i32 %0)") {
				t.Errorf("expected _initialize to exit with the batch main's result\ngot:\n%s", initFn)
			}
			if strings.Contains(initFn, "ret void") {
				t.Errorf("a batch-test _initialize must never return to the host\ngot:\n%s", initFn)
			}
		})
	}
}

// _initialize's drain is the outermost pump (§5): it holds the re-entrancy
// guard from before its first step until it stops, so a host entry made while
// main runs does not nest a pump inside main's own step.
func TestReactorDrainHoldsInPump(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	drain := extractDefine(ir, "promise_web_reactor_drain")
	if drain == "" {
		t.Fatalf("expected @promise_web_reactor_drain to be defined\ngot:\n%s", ir)
	}
	set := strings.Index(drain, "store i8 1, i8* @promise_web_in_pump")
	step := strings.Index(drain, "call i8 @promise_sched_coop_step()")
	if set < 0 || step < 0 || set > step {
		t.Errorf("expected the drain to set promise_web_in_pump before its first step\ngot:\n%s", drain)
	}
	clear := strings.Index(drain, "store i8 0, i8* @promise_web_in_pump")
	idle := strings.Index(drain, "ret i8 1")
	done := strings.Index(drain, "ret i8 0")
	if clear < 0 || idle < 0 || done < 0 || clear > idle || clear > done {
		t.Errorf("expected the drain to clear promise_web_in_pump before both of its returns\ngot:\n%s", drain)
	}
}

// pal_exit is the one writer of promise_web_terminated, so every exit — not
// only a pump's clean drain — leaves the instance terminated, and an exited
// instance takes no further host entry (§10).
func TestPalExitMarksTerminated(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	exitFn := extractDefine(ir, "pal_exit")
	mark := strings.Index(exitFn, "store i8 1, i8* @"+wasmweb.GlobalTerminated)
	call := strings.Index(exitFn, "call void @promise_env_exit(")
	if mark < 0 || call < 0 || mark > call {
		t.Errorf("expected pal_exit to mark the instance terminated before calling out\ngot:\n%s", exitFn)
	}
	if n := strings.Count(ir, "@"+wasmweb.GlobalTerminated+" = "); n != 1 {
		t.Errorf("expected exactly one definition of @%s, got %d", wasmweb.GlobalTerminated, n)
	}
	if pump := extractDefine(ir, wasmweb.ExportPump); strings.Contains(pump, "store i8 1, i8* @"+wasmweb.GlobalTerminated) {
		t.Errorf("expected pal_exit to be the only writer of @%s\ngot:\n%s", wasmweb.GlobalTerminated, pump)
	}

	wasi := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-wasi")
	if exitFn := extractDefine(wasi, "pal_exit"); strings.Contains(exitFn, "store") {
		t.Errorf("wasm32-wasi's pal_exit must stay a bare proc_exit\ngot:\n%s", exitFn)
	}
}
