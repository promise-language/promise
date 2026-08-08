package codegen

import (
	"strings"
	"testing"
)

// T1392: a bare `return` in a `go`/`go!` block is an early exit that carries no
// value. The bare-return coroutine branch of genReturnStmt (the
// c.coroutineReturnBlock path) previously branched straight to the final suspend
// WITHOUT storing anything into the caller-allocated G.result_ptr buffer, so `<-t`
// read uninitialized memory (poison — a wasm crash, a host garbage int). Codegen
// now stores a DEFINED default on that exit: the result type's zero for a
// non-failable body, and {ok, zero, null} for a failable one.
//
// T1385/§17.2: a bare `return;` on a VALUE-producing path is now a compile error,
// so these bodies are `T = Void` — which is exactly the shape the defined-default
// store still backs. The value-producing exits are covered by t1385_test.go.

// Non-failable void body: the bare-return exit must branch to the coroutine's
// final suspend rather than emit `ret void` (the ramp returns i8*).
func TestT1392_NonFailableGoBlockBareReturnExitsViaFinalSuspend(t *testing.T) {
	ir := generateIR(t, `
		test() {
			n := 5;
			t := go {
				if n > 0 { return; }
				print_line("no");
			};
			<-t;
		}
	`)
	goro := extractGoroutineBody(t, ir)

	if !strings.Contains(goro, "final.suspend") {
		t.Errorf("expected the bare-return exit to branch to the coroutine final.suspend:\n%s", goro)
	}
	if strings.Contains(goro, "ret void") {
		t.Errorf("the coroutine ramp returns i8*; a bare return must not emit `ret void`:\n%s", goro)
	}
}

// Failable void body WITHOUT a use-binding: the bare-return exit must store a
// defined {ok, null} aggregate (emitCloseErrCheck is a no-op with no cap, so the
// defined store is the only thing keeping `<-t` from reading poison).
func TestT1392_FailableGoBlockBareReturnStoresOkAggregate(t *testing.T) {
	ir := generateIR(t, `
		produce!(int x) int { if x < 0 { raise error("neg"); } return x; }
		test() {
			n := 5;
			t := go! {
				base := produce(1)?^;
				if n > 0 { return; }
				print_line(base.to_string());
			};
		}
	`)
	goro := extractGoroutineBody(t, ir)

	if !strings.Contains(goro, "go.store_result") {
		t.Errorf("expected a defined {ok,null} aggregate store (go.store_result) on the failable bare-return exit:\n%s", goro)
	}
	// The coroutine ramp returns i8*; the aggregate must be stored, never `ret`ed.
	if strings.Contains(goro, "ret { i1, i8* }") {
		t.Errorf("failable go-block bare-return path must not `ret` the aggregate from the coroutine ramp:\n%s", goro)
	}
}
