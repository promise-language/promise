package codegen

import (
	"strings"
	"testing"
)

// T1387: a use-binding whose failable close() fails on the SUCCESS exit of a
// `go! {}` value body must route the close error into the goroutine's result
// aggregate (so it surfaces at `<-t`) instead of taking emitCloseCall's suppress
// path — which drops the error via @__mod_std_error.drop and stores {ok, value},
// silently reporting success.
//
// The fix broadens emitScopeCleanup's close-error capture guard to fire inside a
// failable go-block (where c.canError is false because the coroutine ramp returns
// i8*), so emitCloseCall takes the CAPTURE path and emitCloseErrCheck's
// inFailableGoBlock branch (T1384) is reached.
func TestT1387_GoBlockUseCloseErrorRoutesToSink(t *testing.T) {
	ir := generateIR(t, `
		type CloseError is error { int code; }
		type FailResource {
			int id;
			close!(~this) void { raise CloseError(message: "cf", code: 42); }
		}
		produce!(int x) int { if x < 0 { raise error("neg"); } return x; }
		test() {
			t := go! {
				base := produce(1)?^;
				use r := FailResource(id: 1);
				base + r.id
			};
		}
	`)
	goro := extractGoroutineBody(t, ir)

	// The close-error CAPTURE path is active inside the go-block coroutine body.
	// The capture allocas and the "save first error" block only exist on the
	// capture path (emitScopeCleanup allocated a cap because inFailableGoBlock);
	// their presence proves we did NOT take emitCloseCall's suppress path (which
	// would drop the error and has neither).
	if !strings.Contains(goro, "close.err.flag") {
		t.Errorf("expected close-error capture alloca in the go-block coroutine body (capture path taken):\n%s", goro)
	}
	if !strings.Contains(goro, "close.save") {
		t.Errorf("expected the capture-path close.save block (proves the suppress path was NOT taken):\n%s", goro)
	}

	// emitCloseErrCheck routes the captured error to the failable-go-block sink:
	// a close.err.ret block that stores the error aggregate (go.store_result) and
	// branches to the coroutine's final suspend — never a `ret` of the aggregate
	// (invalid in the coroutine ramp).
	if !strings.Contains(goro, "close.err.ret") {
		t.Errorf("expected a close.err.ret error-check block routing to the go-block sink:\n%s", goro)
	}
	if !strings.Contains(goro, "go.store_result") {
		t.Errorf("expected the close error to be stored into the goroutine result aggregate (go.store_result):\n%s", goro)
	}
	if !strings.Contains(goro, "final.suspend") {
		t.Errorf("expected the close-error path to branch to the coroutine final.suspend:\n%s", goro)
	}
	if strings.Contains(goro, "ret { i1, i32, i8* }") {
		t.Errorf("failable go-block close-error path must not `ret` the aggregate from the coroutine ramp:\n%s", goro)
	}
}
