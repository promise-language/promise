package regress1

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1384: the `go! { }` block form lowers the coroutine body as a failable scope.
// A trailing success value is wrapped as {ok=false, value, null} and stored into
// G.result_ptr; an escaping error (bare failable call, `?^`, `raise`) is wrapped
// as {ok=true, _, err}, stored into G.result_ptr, and branches to the coroutine's
// final suspend — never a `ret` of the aggregate (invalid in the coroutine ramp).

// A trailing bare failable call: the auto-propagated success value is wrapped and
// stored into the goroutine's result buffer, and the un-received task drops
// through the per-element FailableTask drop (same aggregate as the call form).
func TestT1384_BlockFormSuccessWrappedAndStored(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		produce!(int x) int { return x; }
		test() {
			t := go! { produce(5) };
		}
	`)
	// The block form shares the failable-task lowering: per-element drop symbol.
	codegentest.AssertContains(t, ir, `@"FailableTask[int].free_after_done"`)
	// The success value is stored through G.result_ptr via the go-block store path.
	codegentest.AssertContains(t, ir, "go.store_result")
	codegentest.AssertContains(t, ir, "go.after_store")
	// The stored value is the {i1, i32, i8*} failable aggregate, not a bare int.
	codegentest.AssertContains(t, ir, "{ i1, i32, i8* }")
}

// An escaping error inside the block routes through the failable-go-block sink:
// it stores the error aggregate and branches to the coroutine's final suspend
// rather than emitting `ret { i1, i32, i8* }` (which the coroutine ramp forbids).
func TestT1384_BlockFormErrorBranchesToFinalSuspend(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		produce!(int x) int {
			if x < 0 { raise error("neg"); }
			return x;
		}
		test() {
			t := go! { produce(-1) };
		}
	`)
	goroutine := codegentest.ExtractGoroutineBody(t, ir)
	// The error path branches to the coroutine's final suspend...
	if !strings.Contains(goroutine, "final.suspend") {
		t.Errorf("expected the failable go-block error path to branch to final.suspend:\n%s", goroutine)
	}
	// ...and must NOT `ret` the failable aggregate (invalid in the coroutine ramp).
	if strings.Contains(goroutine, "ret { i1, i32, i8* }") {
		t.Errorf("failable go-block must not `ret` the aggregate from the coroutine ramp:\n%s", goroutine)
	}
}

// A void-failable block (`go! { effect(); }`, T==void) still allocates a real
// {i1,i8*} aggregate buffer on the caller side (never the 0x1 void sentinel),
// because `<-t` expects and frees an aggregate — matching the call form.
func TestT1384_BlockFormVoidAllocatesAggregateBuffer(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		effect!(int x) { if x < 0 { raise error("bad"); } }
		test() {
			t := go! { effect(1); };
		}
	`)
	// The void-failable task uses the FailableTask (not plain Task) drop path...
	codegentest.AssertContains(t, ir, `@"FailableTask[void].free_after_done"`)
	// ...and its coroutine stores a void aggregate {i1, i8*} into G.result_ptr.
	codegentest.AssertContains(t, ir, "go.store_result")
	codegentest.AssertContains(t, ir, "{ i1, i8* }")
}

// Explicit `?^` inside the block is symmetric with the bare auto-propagate form —
// both route escaping errors through the go-block sink to final suspend.
func TestT1384_BlockFormExplicitPropagateSymmetry(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		produce!(int x) int {
			if x < 0 { raise error("neg"); }
			return x;
		}
		test() {
			t := go! { produce(7)?^ };
		}
	`)
	codegentest.AssertContains(t, ir, `@"FailableTask[int].free_after_done"`)
	codegentest.AssertContains(t, ir, "go.store_result")
}
