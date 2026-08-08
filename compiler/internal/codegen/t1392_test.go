package codegen

import (
	"strings"
	"testing"
)

// T1392: a bare `return` in a `go`/`go!` block is an early exit that carries no
// value. The bare-return coroutine branch of genReturnStmt (the
// c.coroutineReturnBlock path) previously branched straight to the final suspend
// WITHOUT storing anything into the caller-allocated G.result_ptr buffer, so `<-t`
// read uninitialized memory (poison — a host garbage int; on wasm the {ok,err}
// discriminant mis-reads as an error and dereferences a garbage pointer). Codegen
// now stores a DEFINED default on that exit: the result type's zero for a
// non-failable body, and {ok, zero, null} for a failable one.
//
// T1385/§17.2: a bare `return;` on a VALUE-producing path is now a compile error,
// so these bodies are `T = Void` — which is exactly the shape the defined-default
// store still backs. The value-producing exits (§17.2's explicit-return style) are
// covered by t1385_test.go.

// Non-failable void body: the bare-return exit must branch to the coroutine's
// final suspend rather than emit `ret void` (the ramp returns i8*). There is no
// result buffer at all here (goBlockValueResultLLVM is nil), so it must also
// store NOTHING.
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

	if strings.Contains(goro, "go.store_result") {
		t.Errorf("a void non-failable go block allocates no result buffer — the bare-return exit must not store into one:\n%s", goro)
	}
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
	// A bare `contains "go.store_result"` alone would pass even with the fix
	// reverted — the escaping-error sink and the fall-through success exit each
	// emit one too. Count the guarded stores (storeGoResultAgg emits exactly one
	// `br i1 …, label %go.store_result.N` per exit); this body has three:
	//   1. the `produce(1)?^` escaping-error sink  → {err}
	//   2. the bare-return early exit (T1392)      → {ok, null}
	//   3. the fall-through void success exit      → {ok, null}
	const wantStores = 3
	if got := strings.Count(goro, "label %go.store_result."); got != wantStores {
		t.Errorf("expected %d guarded result stores (error sink + bare-return exit + success exit), got %d — a missing bare-return store leaves G.result_ptr uninitialized:\n%s",
			wantStores, got, goro)
	}
	// The coroutine ramp returns i8*; the aggregate must be stored, never `ret`ed.
	if strings.Contains(goro, "ret { i1, i8* }") {
		t.Errorf("failable go-block bare-return path must not `ret` the aggregate from the coroutine ramp:\n%s", goro)
	}
}

// Fire-and-forget VOID block: sema rejects a bare return in a value-producing body
// whether or not the handle is received (§17.2), so the fire-and-forget shape that
// still reaches codegen is a void one. Codegen must take the void path here —
// useGoBlockValuePath is `!goIsVoid && !goExprFireAndForget`, so no result buffer is
// allocated. A store on this exit would write through a buffer that does not exist.
func TestT1392_FireAndForgetVoidGoBlockBareReturnStoresNothing(t *testing.T) {
	ir := generateIR(t, `
		score(int n) int { return n * 2; }
		test() {
			n := 5;
			go {
				if n > 0 { return; }
				ignored := score(n);
			};
		}
	`)
	goro := extractGoroutineBody(t, ir)

	if strings.Contains(goro, "go.store_result") {
		t.Errorf("a fire-and-forget go block allocates no result buffer — neither the bare-return exit nor the discarded trailing value may store into one:\n%s", goro)
	}
	if !strings.Contains(goro, "final.suspend") {
		t.Errorf("expected the bare-return exit to branch to the coroutine final.suspend:\n%s", goro)
	}
}
