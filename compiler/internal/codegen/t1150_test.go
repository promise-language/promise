package codegen

import (
	"strings"
	"testing"
)

// T1150: Inline-consumed task-handle receive (`<-t`, `t : task[T]`) must
// register the received heap result as a droppable statement temp so it is
// freed at statement end when the value is consumed inline (member access,
// operator, method call) rather than bound to a named variable. Without the
// fix the received temp has no owner and leaks.
//
// trackReceivedTaskResult dispatches on the (already-substituted) task element
// type. These tests pin the IR shape per element kind: each consuming function
// must drop the received temp inside a `tmp.drop` block (the statement-temp
// teardown), proving it was tracked rather than left dangling. The runtime
// leak/no-double-free behavior is covered end-to-end by
// tests/concurrency/t1150_inline_receive_temp_leak_test.pr.

// receiveDropProbe builds a program whose `consume` function receives a
// task[T] result and consumes it inline, then asserts the consuming function
// drops that temp in a tmp.drop block (and, when given, calls dropSym).
func receiveDropProbe(t *testing.T, src, dropSym string) {
	t.Helper()
	ir := generateIR(t, src)
	body := extractFunction(ir, "__user.consume")
	if body == "" {
		t.Fatalf("expected __user.consume in IR")
	}
	if !strings.Contains(body, "tmp.drop") {
		t.Errorf("expected a tmp.drop block in __user.consume (received temp not tracked):\n%s", body)
	}
	if dropSym != "" && !strings.Contains(body, dropSym) {
		t.Errorf("expected %s in __user.consume:\n%s", dropSym, body)
	}
}

// String element — `(<-t).len` reads the received string without claiming it,
// so the string temp must be dropped at statement end.
func TestT1150_StringReceiveInlineTracked(t *testing.T) {
	receiveDropProbe(t, `
		make_str() string { return "hi".clone(); }
		consume() { t := go make_str(); int n = (<-t).len; }
		main() { consume(); }
	`, "@promise_string_drop")
}

// Heap vector element — `(<-t).len` on a task[int[]] result. The received
// vector temp is dropped via @Vector.drop in a tmp.drop block. (The task
// handle's own teardown produces an unrelated tmp.drop, so the @Vector.drop
// symbol — absent without the fix — is what pins the received-temp tracking.)
func TestT1150_VectorReceiveInlineTracked(t *testing.T) {
	receiveDropProbe(t, `
		make_vec() int[] { int[] v = []; v.push(1); return v; }
		consume() { t := go make_vec(); int n = (<-t).len; }
		main() { consume(); }
	`, "@Vector.drop")
}

// Channel element — `(<-t).close()` calls a method on the received channel
// without consuming it, so the channel struct temp must be dropped.
func TestT1150_ChannelReceiveInlineTracked(t *testing.T) {
	receiveDropProbe(t, `
		make_chan() channel[int] { c := channel[int](1); return c; }
		consume() { t := go make_chan(); (<-t).close(); }
		main() { consume(); }
	`, `@"Channel[int].drop"`)
}

// Arc (Ref[T]) element — `(<-t).borrow` reads the inner value through a borrow,
// so the received Ref temp must be dropped (decref / free at last reference).
func TestT1150_ArcReceiveInlineTracked(t *testing.T) {
	receiveDropProbe(t, `
		make_ref() Ref[int] { return Ref[int](7); }
		consume() { t := go make_ref(); int x = (<-t).borrow; }
		main() { consume(); }
	`, `@"Ref[int].drop"`)
}

// Weak element — a task[Weak[int]] result consumed via `.upgrade()`. The weak
// temp must be dropped (decref the weak count).
func TestT1150_WeakReceiveInlineTracked(t *testing.T) {
	receiveDropProbe(t, `
		make_weak() Weak[int] { r := Ref[int](7); w := r.downgrade(); return w; }
		consume() { t := go make_weak(); Ref[int]? u = (<-t).upgrade(); }
		main() { consume(); }
	`, `@"Weak[int].drop"`)
}

// Mutex element — `(<-t).lock()` borrows the received mutex to lock it, so the
// Mutex temp itself must be dropped at statement end.
func TestT1150_MutexReceiveInlineTracked(t *testing.T) {
	receiveDropProbe(t, `
		make_mutex() Mutex[int] { return Mutex[int](5); }
		consume() { t := go make_mutex(); int x = (<-t).lock().borrow; }
		main() { consume(); }
	`, `@"Mutex[int].drop"`)
}

// Task element — a task[task[int]] result. The inner `<-t` yields a task[int]
// temp that is received-from (not bound), so it must be dropped.
func TestT1150_TaskReceiveInlineTracked(t *testing.T) {
	receiveDropProbe(t, `
		inner_int() int { return 99; }
		make_task() task[int] { return go inner_int(); }
		consume() { t := go make_task(); int x = <-(<-t); }
		main() { consume(); }
	`, `@"Task[int].drop"`)
}

// Heap user type element — the `{i8*, i8*}` value-struct branch. `(<-t).x`
// reads a field without claiming the value, so the Box instance temp must be
// dropped via its synthesized drop function.
func TestT1150_HeapUserTypeReceiveInlineTracked(t *testing.T) {
	receiveDropProbe(t, `
		type Box { int x; drop(~this) {} }
		make_box() Box { return Box(x: 42); }
		consume() { t := go make_box(); int x = (<-t).x; }
		main() { consume(); }
	`, "@Box.drop")
}

// T1150 (WASM regression): dropping a `task[task[int]]` inside a coroutine body
// must NOT bleed the enclosing coroutine's suspend/cleanup context into the
// synthesized `Task[Task[int]].free_after_done`. That helper is a plain
// (non-coroutine) function; its result is a `task[int]` whose drop routes
// through emitVariantFieldDrop → emitTaskJoinAndFree. Before the fix,
// free_after_done was generated lazily while c.inCoroutine was still true, so the
// nested task-field drop emitted an `llvm.coro.suspend` switching to the OUTER
// coroutine's `%cleanup`/`%coro.done` blocks — which don't exist in the plain
// function ("use of undefined value '%cleanup'" on the WASM cooperative path).
// The drop body must instead call the legacy callable `Task[int].drop`.
func TestT1150_NestedTaskDropInCoroutineNoCoroSuspend(t *testing.T) {
	// The `go {}` block compiles to a coroutine; `t : task[task[int]]` is created
	// and dropped inside it, forcing Task[Task[int]].free_after_done to be
	// generated while inside the coroutine. WASM is the cooperative-scheduler
	// target where the stale suspend path produced malformed IR.
	ir := generateIRForTarget(t, `
		inner_int() int { return 99; }
		make_task() task[int] { return go inner_int(); }
		spawn() { go { t := go make_task(); }; }
		main() { spawn(); }
	`, "wasm32-wasi")

	// The IR name is quoted because it contains brackets:
	// @"Task[Task[int]].free_after_done". extractDefine anchors on the `define`
	// keyword (extractFunction would latch onto a call site and walk back to the
	// wrong enclosing function — which is itself a coroutine).
	faf := extractDefine(ir, `"Task[Task[int]].free_after_done"`)
	if faf == "" {
		t.Fatalf("expected Task[Task[int]].free_after_done to be defined in IR")
	}
	// A plain function must never contain a coroutine suspend — that only makes
	// sense inside a presplitcoroutine and references blocks (%cleanup) absent here.
	if strings.Contains(faf, "llvm.coro.suspend") {
		t.Errorf("Task[Task[int]].free_after_done must not emit a coro.suspend "+
			"(stale coroutine context leaked into a plain drop helper):\n%s", faf)
	}
	// The nested task-field drop must route through the legacy callable drop shell.
	if !strings.Contains(faf, `@"Task[int].drop"`) {
		t.Errorf("expected Task[Task[int]].free_after_done to call Task[int].drop "+
			"(legacy callable join) for the inner task result:\n%s", faf)
	}
}

// Void receive: `<-t` on a task[void] result returns nil — there is nothing to
// track, so trackReceivedTaskResult must be a no-op (no spurious drop).
func TestT1150_VoidReceiveNotTracked(t *testing.T) {
	ir := generateIR(t, `
		make_void() {}
		consume() { t := go make_void(); <-t; }
		main() { consume(); }
	`)
	body := extractFunction(ir, "__user.consume")
	if body == "" {
		t.Fatalf("expected __user.consume in IR")
	}
	// A void receive yields no value; the only drop site is for the task handle
	// `t` itself, never a received-result temp.
	if strings.Contains(body, "promise_string_drop") {
		t.Errorf("void receive must not track a string temp:\n%s", body)
	}
}
