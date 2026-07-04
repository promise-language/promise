package codegen

import (
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T0680 Part 1 — WASM monotonic clock. defineNanotimeFunc/buildNanotimeExternBody
// used to hardcode `ret i64 0` / `store i64 0` on WASM, freezing all time at 0.
// They now read a real monotonic source: wasi_snapshot_preview1.clock_time_get
// (CLOCKID_MONOTONIC = 1) on wasm32-wasi, promise_env.monotonic_nanos on
// wasm32-web.

func TestT0680_WasiNanotimeImportsClockTimeGet(t *testing.T) {
	ir := generateIRForTarget(t, `
		_nanotime() int `+"`extern(\"promise_nanotime\")"+`;
		main() { int _x = _nanotime(); }
	`, "wasm32-wasi")

	assertContains(t, ir, `"wasm-import-module"="wasi_snapshot_preview1"`)
	assertContains(t, ir, `"wasm-import-name"="clock_time_get"`)
	// The monotonic clockid (1) is passed as the first argument.
	assertContains(t, ir, "call i32 @clock_time_get(i32 1,")

	body := findDefinedFunc(ir, "@promise_nanotime(")
	if body == "" {
		t.Fatalf("expected promise_nanotime body to be defined")
	}
	// The monotonic nanos must flow from clock_time_get, not a hardcoded 0.
	if !strings.Contains(body, "@clock_time_get") {
		t.Errorf("WASM promise_nanotime must read clock_time_get, not return 0:\n%s", body)
	}
}

func TestT0680_WasmWebNanotimeImportsHostMonotonic(t *testing.T) {
	ir := generateIRForTarget(t, `
		_nanotime() int `+"`extern(\"promise_nanotime\")"+`;
		main() { int _x = _nanotime(); }
	`, "wasm32-web")

	// wasm32-web sources monotonic time from the Node harness, not WASI.
	assertContains(t, ir, `"wasm-import-module"="promise_env"`)
	assertContains(t, ir, `"wasm-import-name"="monotonic_nanos"`)
	assertContains(t, ir, "call i64 @promise_env.monotonic_nanos()")
	if strings.Contains(ir, "@clock_time_get") {
		t.Errorf("wasm32-web must NOT import WASI clock_time_get:\n%s", ir)
	}
}

// Native builds are unaffected — nanotime still reads clock_gettime and never
// imports the WASM clock.
func TestT0680_NativeNanotimeUnchanged(t *testing.T) {
	ir := generateIRForTarget(t, `
		_nanotime() int `+"`extern(\"promise_nanotime\")"+`;
		main() { int _x = _nanotime(); }
	`, "x86_64-unknown-linux-gnu")
	assertContains(t, ir, "@clock_gettime")
	if strings.Contains(ir, "@clock_time_get") {
		t.Errorf("native nanotime must not import the WASM clock_time_get:\n%s", ir)
	}
}

// T0680 Part 2 — the cooperative scheduler enforces the per-test deadline.
// promise_sched_coop_step returns i8 2 (stop/timeout) when the armed deadline is
// reached; promise_sched_coop_run stops the loop on 2.

func TestT0680_CoopStepEnforcesDeadline(t *testing.T) {
	ir := generateIRForTarget(t, `
		worker() int { return 7; }
		main() { Task[int] t = go worker(); int _v = <-t; }
	`, "wasm32-wasi")

	// The two enforcement globals exist.
	assertContains(t, ir, "@__promise_test_deadline = global i64 0")
	assertContains(t, ir, "@__promise_test_timed_out = global i8 0")

	step := findDefinedFunc(ir, "@promise_sched_coop_step(")
	if step == "" {
		t.Fatalf("expected promise_sched_coop_step to be defined on WASM")
	}
	// It reads the deadline, the monotonic clock, sets timed_out, and can ret 2.
	if !strings.Contains(step, "@__promise_test_deadline") {
		t.Errorf("coop_step must load the per-test deadline:\n%s", step)
	}
	if !strings.Contains(step, "@clock_time_get") {
		t.Errorf("coop_step must read the monotonic clock to compare the deadline:\n%s", step)
	}
	if !strings.Contains(step, "store i8 1, i8* @__promise_test_timed_out") {
		t.Errorf("coop_step must set __promise_test_timed_out on deadline:\n%s", step)
	}
	if !strings.Contains(step, "ret i8 2") {
		t.Errorf("coop_step must have a `ret i8 2` (stop/timeout) path:\n%s", step)
	}

	run := findDefinedFunc(ir, "@promise_sched_coop_run(")
	if run == "" {
		t.Fatalf("expected promise_sched_coop_run to be defined on WASM")
	}
	if !strings.Contains(run, "icmp eq i8") || !strings.Contains(run, ", 2") {
		t.Errorf("coop_run must branch to its done block when coop_step returns 2:\n%s", run)
	}
}

// buildWasmTestMainIR mirrors generateIRForTarget but drives the WASM test-main
// path (GenerateTestMain) with an explicit per-test timeout map so the deadline
// prologue and timeout-aware result computation are emitted.
func buildWasmTestMainIR(t *testing.T, src string, timeouts map[string]int64) string {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, errs := ast.Build("test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}

	stdModInfo, stdScope := getCodegenStdModInfo()
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	file.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)

	ti := sema.ParseTargetInfo("wasm32-wasi")
	info, semaErrs := sema.CheckWithTarget(file, map[string]*types.Scope{"std": stdScope}, ti)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{"std": stdModInfo}
	info.ModuleOrder = []string{"std"}
	result := Compile(file, info, "wasm32-wasi")
	result.GenerateTestMain(info.Tests, timeouts)
	return result.Module.String()
}

func TestT0680_WasmTestMainArmsDeadlineAndReadsTimedOut(t *testing.T) {
	ir := buildWasmTestMainIR(t,
		`myTest() `+"`test"+` { }`,
		map[string]int64{"myTest": 2_000_000_000})

	// main arms the deadline (stores into __promise_test_deadline) and clears the
	// flag before running the cooperative scheduler.
	if !strings.Contains(ir, "store i64 %") || !strings.Contains(ir, "@__promise_test_deadline") {
		t.Errorf("WASM test main must arm __promise_test_deadline")
	}
	if !strings.Contains(ir, "store i8 0, i8* @__promise_test_timed_out") {
		t.Errorf("WASM test main must clear __promise_test_timed_out before each test")
	}
	// After the run it loads the flag to compute the result (timeout=2).
	if !strings.Contains(ir, "load i8, i8* @__promise_test_timed_out") {
		t.Errorf("WASM test main must load __promise_test_timed_out to compute the result")
	}
	assertContains(t, ir, "call void @promise_sched_coop_run()")
}

// With no per-test timeout the deadline is stored as 0 (disabled) — enforcement
// is skipped and the process backstop remains the only limit.
func TestT0680_WasmTestMainDisablesDeadlineWhenNoTimeout(t *testing.T) {
	ir := buildWasmTestMainIR(t, `myTest() `+"`test"+` { }`, nil)
	if !strings.Contains(ir, "store i64 0, i64* @__promise_test_deadline") {
		t.Errorf("WASM test main must disable the deadline (store 0) when no timeout is set")
	}
}

// TestT0680_ClockTimeGetDeclaredOnce — getOrDeclareWasmImport dedups by name.
// clock_time_get is imported by BOTH .promise_nanotime_raw (defineNanotimeFunc)
// and promise_sched_coop_step's deadline check, yet only one declaration may
// exist — a duplicate declare would produce invalid IR / a linker conflict.
func TestT0680_ClockTimeGetDeclaredOnce(t *testing.T) {
	ir := generateIRForTarget(t, `
		worker() int { return 7; }
		main() { Task[int] t = go worker(); int _v = <-t; }
	`, "wasm32-wasi")
	if n := strings.Count(ir, "declare i32 @clock_time_get"); n != 1 {
		t.Errorf("clock_time_get must be declared exactly once (dedup), got %d:\n%s",
			n, ir)
	}
}

// TestT0680_CoopRunStopsOnTimeout — promise_sched_coop_run must branch to its
// done block (not keep looping) when promise_sched_coop_step returns 2, and keep
// looping only on the non-timeout progress path (loop_continue).
func TestT0680_CoopRunStopsOnTimeout(t *testing.T) {
	ir := generateIRForTarget(t, `
		worker() int { return 7; }
		main() { Task[int] t = go worker(); int _v = <-t; }
	`, "wasm32-wasi")
	run := findDefinedFunc(ir, "@promise_sched_coop_run(")
	if run == "" {
		t.Fatalf("expected promise_sched_coop_run to be defined on WASM")
	}
	// The run loop calls coop_step, tests the result against 2, and has a
	// dedicated loop_continue block for the non-timeout progress branch.
	if !strings.Contains(run, "call i8 @promise_sched_coop_step()") {
		t.Errorf("coop_run must drive coop_step:\n%s", run)
	}
	if !strings.Contains(run, "loop_continue") {
		t.Errorf("coop_run must split out a loop_continue block for the "+
			"non-timeout branch:\n%s", run)
	}
	if !strings.Contains(run, "icmp eq i8") || !strings.Contains(run, ", 2") {
		t.Errorf("coop_run must compare coop_step's result to 2 (timeout):\n%s", run)
	}
}

// TestT0680_WasmTaskReceiveHasTimeoutMerge — a non-coroutine `<-t` on WASM spins
// promise_sched_coop_step; when it returns 2 (deadline) the spin must break to a
// task.timed_out block and the result must be produced by a phi that merges the
// normal load path with a zeroinitializer (the timed-out value is discarded).
// The G is deliberately NOT freed on the timeout path (tolerated teardown leak;
// result==2 skips the leak check).
func TestT0680_WasmTaskReceiveHasTimeoutMerge(t *testing.T) {
	ir := generateIRForTarget(t, `
		worker() int { return 7; }
		foo() int { Task[int] t = go worker(); int v = <-t; return v; }
		main() { int _x = foo(); }
	`, "wasm32-wasi")
	foo := findDefinedFunc(ir, "@__user.foo(")
	if foo == "" {
		t.Fatalf("expected __user.foo to be defined")
	}
	if !strings.Contains(foo, "task.timed_out") {
		t.Errorf("WASM task receive must break the spin to a task.timed_out "+
			"block on coop_step==2:\n%s", foo)
	}
	if !strings.Contains(foo, "task.recv_merge") {
		t.Errorf("WASM task receive must merge the load and timeout paths:\n%s", foo)
	}
	if !strings.Contains(foo, "phi i64") || !strings.Contains(foo, "zeroinitializer") {
		t.Errorf("WASM task receive result must be a phi merging the loaded "+
			"value with a zeroinitializer timeout value:\n%s", foo)
	}
	// The spin breaks specifically on stepR == 2.
	if !strings.Contains(foo, "call i8 @promise_sched_coop_step()") ||
		!strings.Contains(foo, "icmp eq i8") {
		t.Errorf("WASM task receive must compare coop_step's result to 2:\n%s", foo)
	}
}

// TestT0680_WasmVoidTaskReceiveHasTimeoutMerge — the void variant still emits the
// timeout break + merge block, but with no phi (nothing to merge — the merge
// falls through to `ret void`). Exercises the isVoid branch after the merge.
func TestT0680_WasmVoidTaskReceiveHasTimeoutMerge(t *testing.T) {
	ir := generateIRForTarget(t, `
		worker() { }
		foo() { Task[void] t = go worker(); <-t; }
		main() { foo(); }
	`, "wasm32-wasi")
	foo := findDefinedFunc(ir, "@__user.foo(")
	if foo == "" {
		t.Fatalf("expected __user.foo to be defined")
	}
	if !strings.Contains(foo, "task.timed_out") || !strings.Contains(foo, "task.recv_merge") {
		t.Errorf("WASM void task receive must still emit the timeout break + "+
			"merge block:\n%s", foo)
	}
	if strings.Contains(foo, "phi i64") {
		t.Errorf("WASM void task receive must NOT emit a result phi (void):\n%s", foo)
	}
}

// TestT0680_NativeTaskReceiveHasNoTimeoutMerge — native `<-t` uses the usleep
// busy-wait (another M runs the G), so the WASM-only timeout break/merge blocks
// must never appear. Locks the WASM-gating of the Part 2 receive change.
func TestT0680_NativeTaskReceiveHasNoTimeoutMerge(t *testing.T) {
	ir := generateIRForTarget(t, `
		worker() int { return 7; }
		foo() int { Task[int] t = go worker(); int v = <-t; return v; }
		main() { int _x = foo(); }
	`, "x86_64-unknown-linux-gnu")
	if strings.Contains(ir, "task.timed_out") || strings.Contains(ir, "task.recv_merge") {
		t.Errorf("native task receive must NOT emit the WASM-only timeout "+
			"break/merge blocks:\n%s", ir)
	}
}

// TestT0680_WasmTaskDropBreaksSpinOnTimeout — the legacy callable Task[T].drop
// spin on WASM must break to `done` (skipping free_after_done) when coop_step
// returns 2, and keep its progress/deadlock handling under a task.drop.progress
// block. Prevents a livelock nested under a drop-join from spinning forever.
func TestT0680_WasmTaskDropBreaksSpinOnTimeout(t *testing.T) {
	ir := generateIRForTarget(t, `
		worker() int { return 7; }
		type Box { Task[int] t; drop(~this) {} }
		main() { Box b = Box(t: go worker()); }
	`, "wasm32-wasi")
	drop := findDefinedFunc(ir, `@"Task[int].drop"`)
	if drop == "" {
		t.Fatalf("expected Task[int].drop to be defined on WASM")
	}
	if !strings.Contains(drop, "task.drop.progress") {
		t.Errorf("WASM Task[int].drop must split out a task.drop.progress block "+
			"for the non-timeout branch:\n%s", drop)
	}
	if !strings.Contains(drop, "call i8 @promise_sched_coop_step()") ||
		!strings.Contains(drop, "icmp eq i8") {
		t.Errorf("WASM Task[int].drop must compare coop_step's result to 2:\n%s", drop)
	}
	// The timeout (==2) branch must jump straight to done, NOT free_after_done.
	if !strings.Contains(drop, "label %done, label %task.drop.progress") {
		t.Errorf("WASM Task[int].drop timeout branch must go to done (skipping "+
			"free_after_done), else fall through to task.drop.progress:\n%s", drop)
	}
}

// TestT0680_NativeTaskDropHasNoTimeoutBranch — native Task[T].drop keeps the
// usleep busy-spin (no coop_step, no timeout branch). Locks WASM-gating of the
// Part 2 drop change.
func TestT0680_NativeTaskDropHasNoTimeoutBranch(t *testing.T) {
	ir := generateIRForTarget(t, `
		worker() int { return 7; }
		type Box { Task[int] t; drop(~this) {} }
		main() { Box b = Box(t: go worker()); }
	`, "x86_64-unknown-linux-gnu")
	drop := findDefinedFunc(ir, `@"Task[int].drop"`)
	if drop == "" {
		t.Fatalf("expected Task[int].drop to be defined")
	}
	if strings.Contains(drop, "task.drop.progress") {
		t.Errorf("native Task[int].drop must NOT emit the WASM-only "+
			"task.drop.progress timeout block:\n%s", drop)
	}
}
