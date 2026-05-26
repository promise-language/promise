package codegen

import (
	"strings"
	"testing"
)

// T0668: Dropping a container/binding holding a live, never-awaited Task[T]
// handle deadlocked the single-threaded WASM cooperative scheduler — the old
// Task[T].drop busy-spun on G.done via a usleep that is a no-op on WASM, and
// the pending `go {…}` G never ran.
//
// Fix: split Task[T].drop into a spin shell + Task[T].free_after_done; route
// every coroutine-reachable drop site through a cooperative park-suspend join
// (mirroring `<-t`); and on WASM make the legacy callable Task[T].drop pump
// promise_sched_coop_step() (factored out of promise_sched_coop_run) instead of
// a no-op usleep, so genuinely non-coroutine drop bodies (synthesized
// struct/enum field drops, Map.drop) also make progress.
//
// Runtime correctness + zero-leak is enforced by
// tests/concurrency/task_drop_wasm_test.pr (host AND --target wasm32-wasi).
// These tests lock the structural IR signature.

// findDefinedFunc returns the IR text of a function whose `define` signature
// contains marker (handles quoted names like @"Task[int].drop").
func findDefinedFunc(ir, marker string) string {
	idx := strings.Index(ir, "define ")
	for idx >= 0 {
		lineEnd := strings.Index(ir[idx:], "\n")
		if lineEnd < 0 {
			return ""
		}
		sig := ir[idx : idx+lineEnd]
		if strings.Contains(sig, marker) {
			end := strings.Index(ir[idx:], "\n}\n")
			if end < 0 {
				return ir[idx:]
			}
			return ir[idx : idx+end]
		}
		next := strings.Index(ir[idx+1:], "\ndefine ")
		if next < 0 {
			return ""
		}
		idx = idx + 1 + next + 1
	}
	return ""
}

// funcsContaining returns the set of defined-function signatures whose body
// contains needle.
func funcsContaining(ir, needle string) []string {
	var out []string
	cur := ""
	for _, ln := range strings.Split(ir, "\n") {
		if strings.HasPrefix(ln, "define ") {
			cur = strings.Split(strings.SplitN(ln, "(", 2)[0], "define ")[1]
		}
		if strings.Contains(ln, needle) {
			out = append(out, cur)
		}
	}
	return out
}

// TestT0668_WasmTaskDropPumpsCoopStep — on wasm32-wasi the legacy callable
// Task[T].drop must pump promise_sched_coop_step() (not a no-op usleep) and
// terminate genuine deadlocks; it defers cleanup to Task[T].free_after_done.
func TestT0668_WasmTaskDropPumpsCoopStep(t *testing.T) {
	ir := generateIRForTarget(t, `
		worker() int { return 7; }
		type Box { Task[int] t; drop(~this) {} }
		main() { Box b = Box(t: go worker()); }
	`, "wasm32-wasi")

	drop := findDefinedFunc(ir, `@"Task[int].drop"`)
	if drop == "" {
		t.Fatalf("expected Task[int].drop to be defined")
	}
	if !strings.Contains(drop, "@promise_sched_coop_step") {
		t.Errorf("WASM Task[int].drop spin must call @promise_sched_coop_step "+
			"(pump the cooperative scheduler), not a no-op usleep:\n%s", drop)
	}
	if strings.Contains(drop, "usleep(") {
		t.Errorf("WASM Task[int].drop must NOT use the no-op usleep spin:\n%s", drop)
	}
	if !strings.Contains(drop, `@"Task[int].free_after_done"`) {
		t.Errorf("Task[int].drop must defer post-done cleanup to "+
			"Task[int].free_after_done:\n%s", drop)
	}
	if !strings.Contains(drop, ".str.deadlock.taskdrop") {
		t.Errorf("WASM Task[int].drop must emit the terminal deadlock message "+
			"when coop_step makes no progress and G is not done:\n%s", drop)
	}

	step := findDefinedFunc(ir, "@promise_sched_coop_step(")
	if step == "" {
		t.Fatalf("expected promise_sched_coop_step to be defined on WASM")
	}
	if !strings.HasPrefix(step, "define i8 @promise_sched_coop_step(") {
		t.Errorf("promise_sched_coop_step must return i8 (1=ran a G, 0=none):\n%s",
			strings.SplitN(step, "\n", 2)[0])
	}
	if !strings.Contains(step, "@promise_sched_find_runnable") {
		t.Errorf("promise_sched_coop_step must call find_runnable:\n%s", step)
	}

	run := findDefinedFunc(ir, "@promise_sched_coop_run(")
	if run == "" {
		t.Fatalf("expected promise_sched_coop_run to be defined on WASM")
	}
	if !strings.Contains(run, "call i8 @promise_sched_coop_step()") {
		t.Errorf("promise_sched_coop_run must drive the loop via "+
			"promise_sched_coop_step():\n%s", run)
	}
}

// TestT0668_HostTaskDropSpinsUsleepNoCoopStep — on the host (multi-M) target
// the legacy Task[T].drop keeps the usleep busy-spin (another M runs the G)
// and must NOT reference the WASM-only coop-step pump.
func TestT0668_HostTaskDropSpinsUsleepNoCoopStep(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 7; }
		type Box { Task[int] t; drop(~this) {} }
		main() { Box b = Box(t: go worker()); }
	`)

	drop := findDefinedFunc(ir, `@"Task[int].drop"`)
	if drop == "" {
		t.Fatalf("expected Task[int].drop to be defined")
	}
	if !strings.Contains(drop, "usleep(") {
		t.Errorf("host Task[int].drop spin must keep the usleep busy-wait:\n%s", drop)
	}
	if strings.Contains(drop, "@promise_sched_coop_step") {
		t.Errorf("host Task[int].drop must NOT pump the WASM-only "+
			"promise_sched_coop_step:\n%s", drop)
	}
	if !strings.Contains(drop, `@"Task[int].free_after_done"`) {
		t.Errorf("Task[int].drop must defer post-done cleanup to "+
			"Task[int].free_after_done (the split):\n%s", drop)
	}

	faf := findDefinedFunc(ir, `@"Task[int].free_after_done"`)
	if faf == "" {
		t.Fatalf("expected Task[int].free_after_done to be defined (split from drop)")
	}
	// free_after_done holds the post-done cleanup (frees the G struct) and must
	// NOT spin or pump the scheduler.
	if strings.Contains(faf, "pal_usleep") || strings.Contains(faf, "@promise_sched_coop_step") {
		t.Errorf("Task[int].free_after_done must be spin-free post-done cleanup:\n%s", faf)
	}
	if !strings.Contains(faf, "@pal_free") {
		t.Errorf("Task[int].free_after_done must free the G struct:\n%s", faf)
	}
}

// TestT0668_CoroutineSiteCooperativeJoin — a `go { task[int] inner = go w(); }`
// block body is a coroutine; its scope-exit drop of the un-awaited `inner`
// must emit the inline cooperative park-suspend join (parks on the target G's
// done_waiters, coro.suspend, then Task[int].free_after_done) and must NOT use
// the legacy spin (no usleep, no coop_step, no bare Task[int].drop call) inside
// any coroutine body.
func TestT0668_CoroutineSiteCooperativeJoin(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 7; }
		main() {
			task[void] outer = go {
				task[int] inner = go worker();
			};
			<-outer;
		}
	`)

	joinFns := funcsContaining(ir, "task.park")
	if len(joinFns) == 0 {
		t.Fatalf("expected at least one coroutine to emit the cooperative "+
			"park-suspend join (task.park block):\n%s", ir)
	}
	// Every coroutine that parks for the task join must also coro.suspend and
	// call free_after_done, and must not contain the legacy spin primitives.
	for _, sig := range joinFns {
		fn := findDefinedFunc(ir, strings.Fields(sig)[len(strings.Fields(sig))-1]+"(")
		if fn == "" {
			continue
		}
		if !strings.Contains(fn, "@llvm.coro.suspend") && !strings.Contains(fn, "call i8 @llvm.coro.suspend") {
			t.Errorf("cooperative join in %s must coro.suspend:\n%s", sig, fn)
		}
		if !strings.Contains(fn, ".free_after_done") {
			t.Errorf("cooperative join in %s must call Task[T].free_after_done "+
				"after the park-suspend:\n%s", sig, fn)
		}
		if strings.Contains(fn, "pal_usleep") {
			t.Errorf("cooperative join in %s must NOT use the usleep spin:\n%s", sig, fn)
		}
		if strings.Contains(fn, `call void @"Task[int].drop"`) ||
			strings.Contains(fn, `call void @"Task[void].drop"`) {
			t.Errorf("coroutine %s must use the cooperative join, NOT the "+
				"legacy callable Task[T].drop:\n%s", sig, fn)
		}
	}
}

// TestT0668_NonCoroutineStructFieldUsesLegacyDrop — a synthesized struct field
// drop is a plain (non-coroutine) function, so it must dispatch to the legacy
// callable Task[T].drop (which on WASM pumps coop_step), NOT emit an inline
// coro.suspend park (it has no coroutine context).
func TestT0668_NonCoroutineStructFieldUsesLegacyDrop(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 7; }
		type Box { Task[int] t; drop(~this) {} }
		main() { Box b = Box(t: go worker()); }
	`)

	boxDrop := findDefinedFunc(ir, "@Box.drop(")
	if boxDrop == "" {
		t.Fatalf("expected Box.drop to be defined")
	}
	if !strings.Contains(boxDrop, `@"Task[int].drop"`) {
		t.Errorf("non-coroutine Box.drop must dispatch the Task field to the "+
			"legacy callable Task[int].drop:\n%s", boxDrop)
	}
	if strings.Contains(boxDrop, "task.park") {
		t.Errorf("non-coroutine Box.drop must NOT emit the inline coroutine "+
			"park-suspend join (no coroutine context here):\n%s", boxDrop)
	}
}

// TestT0668_TrackedTempTaskInCoroutineUsesJoin — audit: a discarded Task
// statement-expression temp (a getter result) inside a `go {}` coroutine body
// reaches scope exit un-awaited via cleanupStmtTemps. That coroutine-reachable
// emission must route through the cooperative join (Task[int].free_after_done
// after a park-suspend), NOT the bare legacy Task[int].drop spin.
func TestT0668_TrackedTempTaskInCoroutineUsesJoin(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 7; }
		type B { get w task[int] `+"`public"+` => go worker(); drop(~this) {} }
		main() {
			go { B b = B(); b.w; };
			sleep(Duration.from_millis(1));
		}
	`)

	// The go-block body is a coroutine; the discarded `b.w` task temp must be
	// joined cooperatively there.
	joinFns := funcsContaining(ir, "task.park")
	if len(joinFns) == 0 {
		t.Fatalf("expected the go-block coroutine to cooperatively join the "+
			"discarded tracked Task getter temp (task.park):\n%s", ir)
	}
	joinedFn := ""
	for _, sig := range joinFns {
		fn := findDefinedFunc(ir, strings.Fields(sig)[len(strings.Fields(sig))-1]+"(")
		if strings.Contains(fn, `@"Task[int].free_after_done"`) {
			joinedFn = fn
			break
		}
	}
	if joinedFn == "" {
		t.Fatalf("expected a coroutine that parks AND calls "+
			"Task[int].free_after_done for the tracked getter temp:\n%s", ir)
	}
	if strings.Contains(joinedFn, `call void @"Task[int].drop"`) {
		t.Errorf("the coroutine join path must NOT also emit the bare legacy "+
			"Task[int].drop spin for the tracked temp:\n%s", joinedFn)
	}
}
