package codegen

import (
	"fmt"
	"regexp"
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

// parkTimerProgram reaches std's wasm32-web sleep primitive through a helper,
// from main and from a goroutine. The cached std here is checked for the host,
// so the program declares the extern itself.
const parkTimerProgram = "_park(int ns) `extern(\"" + wasmweb.ParkTimerFunc + "\") `target(web);\n" + `
nap(int ns) {
  _park(ns);
}

main() {
  t := go { nap(5000000); };
  nap(1000);
  <-t;
}
`

// A call to the park extern is lowered inline, never called: in the helper's
// coroutine variant — the one main and the goroutine run — the goroutine links
// a frame-resident node into the timer list, hands sched.timer_lock to the
// scheduler as its park mutex and suspends; the plain helper, which cannot
// suspend, steps the scheduler until its deadline. A duration of zero or less
// arms nothing, and a deadline past the clock's range saturates.
func TestWebParkTimerLowering(t *testing.T) {
	ir := generateIRForTarget(t, parkTimerProgram, "wasm32-web")
	if strings.Contains(ir, "call void @"+wasmweb.ParkTimerFunc) {
		t.Errorf("expected every call to @%s to be lowered inline\ngot:\n%s", wasmweb.ParkTimerFunc, ir)
	}

	plain := extractDefine(ir, "__user.nap")
	if !strings.Contains(plain, "call void @promise_sched_timer_sleep(i64") || strings.Contains(plain, "llvm.coro.suspend") {
		t.Errorf("expected the plain helper to sleep through promise_sched_timer_sleep\ngot:\n%s", plain)
	}

	variant := extractDefine(ir, "__user.nap$coro")
	if variant == "" {
		t.Fatalf("expected the helper to get a coroutine variant: a sleep blocks\ngot:\n%s", ir)
	}
	for _, want := range []string{
		"alloca { i64, i8*, i8* }",                  // the frame-resident timer node
		"call void @pal_mutex_lock(",                // sched.timer_lock, held across the suspend
		"call void @promise_sched_timer_insert(i8*", // linked into the list
		"store i8 3, i8*",                           // G.status = waiting
		"call i8 @llvm.coro.suspend(token none, i1 false)",
		"i8 0, label %sleep.resume",
		"i8 1, label %coro.cleanup",
	} {
		if !strings.Contains(variant, want) {
			t.Errorf("expected the coroutine variant to contain %q\ngot:\n%s", want, variant)
		}
	}
	// The lock that was locked is the one handed over as the park mutex.
	lockRe := regexp.MustCompile(`(%\d+) = load i8\*, i8\*\* %\d+\n\tcall void @pal_mutex_lock\(i8\* (%\d+)\)`)
	m := lockRe.FindStringSubmatch(variant)
	if m == nil || m[1] != m[2] || !strings.Contains(variant, "store i8* "+m[1]+", i8** ") ||
		!strings.Contains(variant, "@__promise_sched, i32 0, i32 28") {
		t.Errorf("expected sched.timer_lock to be locked and stored as G.park_mutex\ngot:\n%s", variant)
	}

	for _, fn := range []string{plain, variant} {
		if !regexp.MustCompile(`icmp sgt i64 %\d+, 0\n\tbr i1 %\d+, label %sleep\.arm`).MatchString(fn) {
			t.Errorf("expected a duration of zero or less to arm nothing\ngot:\n%s", fn)
		}
		// One below math.MaxInt64, promise_sched_timer_idle's "no limit": a
		// sleep that never ends is still an armed timer to wait on.
		if !regexp.MustCompile(`select i1 %\d+, i64 u0x7FFFFFFFFFFFFFFE, i64 %\d+`).MatchString(fn) ||
			strings.Contains(fn, "u0x7FFFFFFFFFFFFFFF") {
			t.Errorf("expected the deadline to saturate one below the idle wait's no-limit sentinel\ngot:\n%s", fn)
		}
	}
}

// The timer runtime: an armed timer is a live registration (§4.1) — insert
// counts it, expire releases it as it makes the sleeper runnable — and the
// thread-owning wait honours the per-test deadline (T0680). The lock that
// guards the list is created at scheduler init on wasm32-web only.
func TestSchedTimerRuntime(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	insert := extractDefine(ir, "promise_sched_timer_insert")
	if !regexp.MustCompile(`add i32 %\d+, 1\n\tstore i32 %\d+, i32\* @` + wasmweb.GlobalLiveRegistrations).MatchString(insert) {
		t.Errorf("expected arming a timer to count a live registration\ngot:\n%s", insert)
	}
	if !strings.Contains(insert, "icmp sgt i64") {
		t.Errorf("expected insertion after every equal or earlier deadline\ngot:\n%s", insert)
	}
	expire := extractDefine(ir, "promise_sched_timer_expire")
	for _, want := range []string{"icmp sle i64", "call void @promise_sched_enqueue(i8*", "ret i64", "i64 -1"} {
		if !strings.Contains(expire, want) {
			t.Errorf("expected expire to contain %q\ngot:\n%s", want, expire)
		}
	}
	if !regexp.MustCompile(`add i32 %\d+, -1\n\tstore i32 %\d+, i32\* @` + wasmweb.GlobalLiveRegistrations).MatchString(expire) {
		t.Errorf("expected waking a sleeper to release its live registration\ngot:\n%s", expire)
	}
	idle := extractDefine(ir, "promise_sched_timer_idle")
	if !strings.Contains(idle, "load i64, i64* @__promise_test_deadline") ||
		!strings.Contains(idle, "store i8 1, i8* @__promise_test_timed_out") || !strings.Contains(idle, "ret i8 2") {
		t.Errorf("expected the timer wait to stop at the per-test deadline\ngot:\n%s", idle)
	}
	sleep := extractDefine(ir, "promise_sched_timer_sleep")
	if !strings.Contains(sleep, "call i8 @promise_sched_wait_step(i64 %deadline)") || strings.Contains(sleep, "promise_sched_coop_step") {
		t.Errorf("expected the non-coroutine sleep to take wait steps bounded by its own deadline\ngot:\n%s", sleep)
	}
	// The wait step runs a goroutine, and only when none ran waits for a timer.
	step := extractDefine(ir, "promise_sched_wait_step")
	coop := strings.Index(step, "call i8 @promise_sched_coop_step()")
	wait := strings.Index(step, "call i8 @promise_sched_timer_idle(i64 %limit)")
	if coop < 0 || wait < coop || !regexp.MustCompile(`icmp ne i8 %\d+, 0\n\tbr i1 %\d+, label %stepped, label %idle`).MatchString(step) {
		t.Errorf("expected the wait step to step the scheduler, then wait for a timer only when nothing ran\ngot:\n%s", step)
	}
	if init := extractDefine(ir, "promise_sched_init"); !strings.Contains(init, "@__promise_sched, i32 0, i32 28") {
		t.Errorf("expected scheduler init to create sched.timer_lock on wasm32-web\ngot:\n%s", init)
	}
	abandon := extractDefine(ir, "promise_sched_timer_abandon")
	if !regexp.MustCompile(`add i32 %\d+, -1\n\tstore i32 %\d+, i32\* @`+wasmweb.GlobalLiveRegistrations).MatchString(abandon) ||
		!strings.Contains(abandon, "@__promise_sched, i32 0, i32 27") || strings.Contains(abandon, "promise_sched_enqueue") {
		t.Errorf("expected abandon to unlink every node without waking its goroutine, releasing its live registration\ngot:\n%s", abandon)
	}

	wasi := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-wasi")
	if strings.Contains(wasi, "promise_sched_timer") {
		t.Error("wasm32-wasi must not carry the timer runtime")
	}
	if init := extractDefine(wasi, "promise_sched_init"); strings.Contains(init, "i32 0, i32 28") {
		t.Errorf("wasm32-wasi must not create sched.timer_lock\ngot:\n%s", init)
	}
}

// A drain that finds nothing runnable consults the timers before the liveness
// rule (§4.1, §4.2): a sleeper already due is stepped like any goroutine, and
// one still parked is a continuation the drain asks the host for — kind Timer,
// the delay rounded up to whole milliseconds and capped at a host timer's
// range — so the pump returns PumpMore and the initial drain stays alive.
func TestWebDrainsScheduleTimerContinuation(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	for _, name := range []string{wasmweb.ExportPump, "promise_web_reactor_drain"} {
		fn := extractDefine(ir, name)
		expire := strings.Index(fn, "call i64 @promise_sched_timer_expire()")
		live := strings.Index(fn, "load i32, i32* @"+wasmweb.GlobalLiveRegistrations)
		if expire < 0 || live < 0 || expire > live {
			t.Errorf("%s: expected the timers to be consulted before the liveness rule\ngot:\n%s", name, fn)
		}
		sched := fmt.Sprintf("call void @promise_env_schedule_pump(i32 %d, i32 %%", wasmweb.SchedulePumpTimer)
		if !strings.Contains(fn, sched) {
			t.Errorf("%s: expected a Timer continuation to be scheduled\ngot:\n%s", name, fn)
		}
		// u0x7FFFFFFF is math.MaxInt32, the longest delay a host timer takes.
		for _, want := range []string{"sdiv i64 %", "srem i64 %", "i64 u0x7FFFFFFF", "trunc i64"} {
			if !strings.Contains(fn, want) {
				t.Errorf("%s: expected the delay computation to contain %q\ngot:\n%s", name, want, fn)
			}
		}
	}
	pump := extractDefine(ir, wasmweb.ExportPump)
	scheduled := pump[strings.Index(pump, "timer_scheduled:"):]
	if !strings.HasPrefix(strings.SplitN(scheduled, "\n", 3)[1], fmt.Sprintf("\tret i32 %d", wasmweb.PumpMore)) {
		t.Errorf("expected the pump to return PumpMore once it scheduled a timer\ngot:\n%s", pump)
	}
	// A sleeper woken in the drain is stepped against the same budget.
	if !regexp.MustCompile(`phi i64 \[[^\n]*%drained \]`).MatchString(pump) {
		t.Errorf("expected the pump's step counter to carry over a wake in the drain\ngot:\n%s", pump)
	}
}

// The run-to-completion driver every batch test runs under owns the thread: on
// wasm32-web it waits for a parked sleeper before deciding the program is done
// or deadlocked. wasm32-wasi's driver is unchanged.
func TestCoopRunWaitsOnTimersOnWasmWeb(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	run := extractDefine(ir, "promise_sched_coop_run")
	idle := strings.Index(run, "call i8 @promise_sched_timer_idle(i64 u0x7FFFFFFFFFFFFFFF)") // math.MaxInt64: no limit
	check := strings.Index(run, "check_main_done:")
	if idle < 0 || check < 0 || idle > check {
		t.Errorf("expected coop_run to wait on the timers before checking main_done\ngot:\n%s", run)
	}
	wasi := extractDefine(generateIRForTarget(t, trivialWasmWebProgram, "wasm32-wasi"), "promise_sched_coop_run")
	if strings.Contains(wasi, "timer") {
		t.Errorf("wasm32-wasi's coop_run must not wait on timers\ngot:\n%s", wasi)
	}
}

// Before each test a batch binary drops the sleepers a timed-out earlier test
// abandoned, so coop_run waits only on the test's own timers.
func TestWasmWebTestMainAbandonsEarlierSleepers(t *testing.T) {
	const src = "test_one() `test {\n}\n\ntest_two() `test {\n}\n"
	main := extractDefine(generateTestMainIRForTarget(t, src, "wasm32-web"), "main")
	abandon := "call void @promise_sched_timer_abandon()"
	if strings.Count(main, abandon) != 2 {
		t.Fatalf("expected one %q per test\ngot:\n%s", abandon, main)
	}
	if strings.Index(main, abandon) > strings.Index(main, "call void @promise_sched_coop_run()") {
		t.Errorf("expected the abandon before the test's coop_run\ngot:\n%s", main)
	}
	if wasi := generateTestMainIRForTarget(t, src, "wasm32-wasi"); strings.Contains(wasi, "promise_sched_timer") {
		t.Error("wasm32-wasi's test main must not touch timers")
	}
}

// plainWaitProgram waits, in plain function bodies, in each of the three ways
// whose WASM wait steps the scheduler from the waiting function's own stack: a
// channel receive (the shared wait pump — send, select, for-in and Mutex.lock
// take it too), a task receive, and the join of a task dropped un-awaited
// (Task[int].drop).
const plainWaitProgram = `
recv_one(Channel[int] ch) int? {
  return <-ch;
}

await_one() int {
  t := go { 7 };
  return <-t;
}

drop_one() {
  t := go { 7 };
}

main() {
  ch := channel[int](1);
  ch.send(1);
  v := recv_one(ch);
  w := await_one();
  drop_one();
}
`

// On wasm32-web a goroutine a plain-body wait depends on may be parked in
// sleep, with nothing runnable until its timer is due, so every such wait
// steps through promise_sched_wait_step, which waits for the earliest timer
// before its "nothing runnable" can mean deadlock. wasm32-wasi arms no timers,
// and its waits step the scheduler directly.
func TestWasmWebPlainWaitsConsultTimers(t *testing.T) {
	web := generateIRForTarget(t, plainWaitProgram, "wasm32-web")
	wasi := generateIRForTarget(t, plainWaitProgram, "wasm32-wasi")
	waitStep := "call i8 @promise_sched_wait_step(i64 u0x7FFFFFFFFFFFFFFF)" // timerNoLimit
	coopStep := "call i8 @promise_sched_coop_step()"
	for _, fn := range []string{"__user.recv_one", "__user.await_one", "Task[int].drop"} {
		w := extractDefine(web, fn)
		if !strings.Contains(w, waitStep) || strings.Contains(w, coopStep) {
			t.Errorf("wasm32-web %s: expected its wait to step through the timers, never the bare scheduler\ngot:\n%s", fn, w)
		}
		if !strings.Contains(w, "call void @pal_exit(i32 2)") {
			t.Errorf("wasm32-web %s: expected a genuine deadlock still to abort\ngot:\n%s", fn, w)
		}
		x := extractDefine(wasi, fn)
		if !strings.Contains(x, coopStep) || strings.Contains(x, "promise_sched_wait_step") {
			t.Errorf("wasm32-wasi %s: expected its wait to step the scheduler directly\ngot:\n%s", fn, x)
		}
	}
}
