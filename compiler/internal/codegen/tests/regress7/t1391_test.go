package regress7

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1391: a use-binding whose failable close() fails on a bare `return` early-exit
// inside a `go! {}` body must route the close error into the goroutine's
// result aggregate (so it surfaces at `<-t`) instead of capturing it without a
// route — which orphans the error instance (leak).
//
// The bare-return goroutine branch of genReturnStmt (the c.coroutineReturnBlock
// path) calls emitScopeCleanup, which — since T1387 broadened the capture guard to
// fire when c.inFailableGoBlock — takes the CAPTURE path. Without a following
// emitCloseErrCheck the captured error is saved-but-never-freed. The fix mirrors
// the value-return path: capture the cap and call emitCloseErrCheck to route it.
//
// T1385/§17.2: a bare `return;` is legal only on a non-value path, so the body is
// `failable_task[Void]`. What this covers is the bare-return EXIT, not the result
// type — the close-error routing on that exit is unchanged. (The sibling
// explicit-return exit is covered by TestT1385_FailableExplicitReturnStoresOk.)
func TestT1391_GoBlockBareReturnUseCloseRoutesToSink(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
				if base + r.id > 0 { return; }
			};
		}
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	// The close-error CAPTURE path is active on the bare-return exit inside the
	// go-block coroutine body — capture allocas + the "save first error" block
	// only exist on the capture path (emitScopeCleanup allocated a cap because
	// inFailableGoBlock), proving we did not silently drop the error.
	if !strings.Contains(goro, "close.err.flag") {
		t.Errorf("expected close-error capture alloca on the bare-return exit (capture path taken):\n%s", goro)
	}
	if !strings.Contains(goro, "close.save") {
		t.Errorf("expected the capture-path close.save block (proves the error is not orphaned):\n%s", goro)
	}

	// emitCloseErrCheck routes the captured error to the failable-go-block sink:
	// a close.err.ret block storing the error aggregate (go.store_result) and
	// branching to the coroutine's final suspend — never a `ret` of the aggregate.
	if !strings.Contains(goro, "close.err.ret") {
		t.Errorf("expected a close.err.ret error-check block routing to the go-block sink:\n%s", goro)
	}
	if !strings.Contains(goro, "go.store_result") {
		t.Errorf("expected the close error to be stored into the goroutine result aggregate (go.store_result):\n%s", goro)
	}
	if !strings.Contains(goro, "final.suspend") {
		t.Errorf("expected the close-error path to branch to the coroutine final.suspend:\n%s", goro)
	}
	if strings.Contains(goro, "ret { i1, i8* }") {
		t.Errorf("failable go-block close-error path must not `ret` the aggregate from the coroutine ramp:\n%s", goro)
	}
}
