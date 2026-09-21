package codegen

import (
	"strings"
	"testing"
)

// Tests for the wasm32-web reactor top level (docs/wasm-web-callbacks.md §4,
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
	assertNotContains(t, ir, "promise_web_live_registrations")
	assertNotContains(t, ir, "promise_web_in_pump")
	assertNotContains(t, ir, "promise_web_pump")
	assertNotContains(t, ir, "promise_web_reactor_drain")
}

// `_initialize` must not unconditionally pal_exit — the liveness rule (§4.1)
// requires a live-registrations check to gate the exit, so a future
// Phase-2 registration can keep the instance alive. This is the one
// assertion in this file that would fail on the pre-Phase-1 behaviour
// (unconditional `call void @pal_exit(i32 %code)` followed by unreachable).
func TestInitializeGatesExitOnLiveness(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	initFn := extractDefine(ir, "_initialize")
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
	pumpFn := extractDefine(ir, "promise_web_pump")
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
	pumpFn := extractDefine(ir, "promise_web_pump")
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
