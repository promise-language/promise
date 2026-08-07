package codegen

import (
	"strings"
	"testing"
)

// T1392: a bare `return` in a value-producing `go`/`go!` block is an early exit
// with no trailing value. The bare-return coroutine branch of genReturnStmt (the
// c.coroutineReturnBlock path) previously branched straight to the final suspend
// WITHOUT storing anything into the caller-allocated G.result_ptr buffer, so `<-t`
// read uninitialized memory (poison — a wasm crash, a host garbage int). Codegen
// now stores a DEFINED default on that exit: the result type's zero for a
// non-failable body, and {ok, zero, null} for a failable one.

// Non-failable value body: the bare-return exit must store the result type's zero
// into the goroutine result buffer (go.store_result) before the final suspend.
func TestT1392_NonFailableGoBlockBareReturnStoresZero(t *testing.T) {
	ir := generateIR(t, `
		test() {
			n := 5;
			t := go {
				if n > 0 { return; }
				n + 100
			};
			v := <-t;
		}
	`)
	goro := extractGoroutineBody(t, ir)

	// A defined store into G.result_ptr on the bare-return exit — without it the
	// receiver reads poison. storeGoResultAgg emits the go.store_result block.
	if !strings.Contains(goro, "go.store_result") {
		t.Errorf("expected a defined result store (go.store_result) on the non-failable bare-return exit:\n%s", goro)
	}
	if !strings.Contains(goro, "final.suspend") {
		t.Errorf("expected the bare-return exit to branch to the coroutine final.suspend:\n%s", goro)
	}
}

// Failable value body WITHOUT a use-binding: the bare-return exit must store a
// defined {ok, zero, null} aggregate (emitCloseErrCheck is a no-op with no cap, so
// the defined store is the only thing keeping `<-t` from reading poison).
func TestT1392_FailableGoBlockBareReturnStoresOkAggregate(t *testing.T) {
	ir := generateIR(t, `
		produce!(int x) int { if x < 0 { raise error("neg"); } return x; }
		test() {
			n := 5;
			t := go! {
				base := produce(1)?^;
				if n > 0 { return; }
				base + n
			};
		}
	`)
	goro := extractGoroutineBody(t, ir)

	if !strings.Contains(goro, "go.store_result") {
		t.Errorf("expected a defined {ok,zero,null} aggregate store (go.store_result) on the failable bare-return exit:\n%s", goro)
	}
	// The coroutine ramp returns i8*; the aggregate must be stored, never `ret`ed.
	if strings.Contains(goro, "ret { i1, i32, i8* }") {
		t.Errorf("failable go-block bare-return path must not `ret` the aggregate from the coroutine ramp:\n%s", goro)
	}
}
