package codegen

import (
	"strings"
	"testing"
)

// T1379: `go!` spawns a failable_task[T] whose result buffer holds the failable
// {ok,value,err} aggregate. An un-received failable task is dropped through a
// dedicated per-element-type FailableTask[T].drop (distinct from Task[T].drop)
// that discharges the buffered aggregate — dropping the success value or freeing
// the buffered error — so neither leaks.

func TestT1379_FailableTaskDropDistinctFromTask(t *testing.T) {
	ir := generateIR(t, `
		produce!(int x) int { return x; }
		test() {
			t := go! produce(5);
		}
	`)
	// The un-received failable task drops through FailableTask[int].drop, NOT the
	// plain Task[int].drop (which would leak the buffered error/value).
	if !strings.Contains(ir, `@"FailableTask[int].drop"`) {
		t.Errorf("expected a call to @\"FailableTask[int].drop\" for the un-received failable task:\n%s", ir)
	}
	assertContains(t, ir, `@"FailableTask[int].free_after_done"`)
}

// The failable receive surfaces the aggregate: `(<-t)?!` extracts the ok flag
// (field 0) of the loaded aggregate and panics on error, exactly like a failable
// call. Presence of the FailableTask drop confirms the failable spawn lowering.
func TestT1379_FailableReceiveHandled(t *testing.T) {
	ir := generateIR(t, `
		produce!(int x) int { return x; }
		test() {
			t := go! produce(5);
			v := (<-t)?!;
		}
	`)
	// The receive consumes the handle, so the scope-exit drop is cleared — the
	// FailableTask drop must NOT be called on the received handle (double-free).
	// But the failable spawn still declares the drop symbol; assert the receive
	// path emits the error-panic branch on the aggregate.
	assertContains(t, ir, "error.panic")
}

// On WASM the failable receive flows through the deadline-timeout merge (the
// single-threaded scheduler's cooperative-timeout path), not the host early
// return. The merged aggregate phi must still be surfaced raw so `?!`/`?^`/`? {}`
// consume it — assert the failable spawn/receive lowers there too (T1379).
func TestT1379_FailableReceiveWasmTimeoutMerge(t *testing.T) {
	ir := generateIRForTarget(t, `
		produce!(int x) int { return x; }
		test() {
			t := go! produce(5);
			v := (<-t)?!;
		}
	`, "wasm32-wasi")
	// The WASM receive builds the recv-merge block, and the failable spawn still
	// declares the per-element FailableTask drop for the leak-free path.
	assertContains(t, ir, "task.recv_merge")
	assertContains(t, ir, `@"FailableTask[int].free_after_done"`)
	assertContains(t, ir, "error.panic")
}

// `go! obj.method()` spawns through the ViaBlock lowering (full codegen context
// inside the coroutine body), distinct from the free-function fast path. The
// stored result is still the failable {ok,value,err} aggregate, so the same
// per-element FailableTask drop is synthesized (T1379).
func TestT1379_FailableMethodSpawnViaBlock(t *testing.T) {
	ir := generateIR(t, `
		type W {
			work!(this, int x) int { return x; }
		}
		test() {
			w := W();
			t := go! w.work(5);
			v := (<-t)?!;
		}
	`)
	assertContains(t, ir, `@"FailableTask[int].free_after_done"`)
	assertContains(t, ir, "error.panic")
}

// A non-failable `go f()` still uses the plain Task[T].drop — the failable path
// must not regress the non-failable spawn.
func TestT1379_PlainTaskUnaffected(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 1; }
		test() {
			t := go worker();
		}
	`)
	assertContains(t, ir, `@"Task[int].drop"`)
	if strings.Contains(ir, `@"FailableTask[int].drop"`) {
		t.Errorf("plain `go` must not emit a FailableTask drop:\n%s", ir)
	}
}
