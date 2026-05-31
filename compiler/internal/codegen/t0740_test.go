package codegen

import (
	"strings"
	"testing"
)

// T0740: a value-returning `go { ... }` block whose trailing value is a
// capturing closure read any outer variable referenced ONLY inside that closure
// as zero. collectBlockIdents (which gathers the outer locals a go-block must
// thread into its coroutine arg pack) skipped lambda bodies, so a var used only
// inside the nested lambda was never passed to the coroutine — inside the
// coroutine its slot was zero-initialized and genLambdaExpr captured 0.
//
// The fix consults sema's per-lambda capture set in collectBlockIdents's
// LambdaExpr case and adds the outer-local captures to the coroutine's args.
//
// Runtime correctness + zero-leak is enforced by the t0740_* batch tests in
// tests/concurrency/t0683_go_block_value_test.pr (host AND --target wasm32-wasi).
// This test locks the structural IR signature: the coroutine receives the
// closure-only outer var as a parameter and the caller loads it before spawning.

// TestT0740_LambdaOnlyCaptureThreadedIntoCoroutine — `base`, referenced only
// inside the trailing closure of a `go { }` block, must become a parameter of
// the go-block coroutine and be loaded + passed by the caller. Before the fix
// the coroutine took no parameters and the captured value was zero.
func TestT0740_LambdaOnlyCaptureThreadedIntoCoroutine(t *testing.T) {
	ir := generateIR(t, `
		wrap() task[() -> int] {
			base := 40;
			task[() -> int] x = go { || -> base + 2 };
			return x;
		}
		main() { }
	`)

	// The go-block coroutine must take `base` as a captured parameter.
	coro := findDefinedFunc(ir, "@.goroutine.0(")
	if coro == "" {
		t.Fatalf("expected go-block coroutine @.goroutine.0 to be defined")
	}
	sig := coro
	if nl := strings.Index(coro, "\n"); nl >= 0 {
		sig = coro[:nl]
	}
	if !strings.Contains(sig, "%base.cap") {
		t.Errorf("go-block coroutine must receive the closure-only outer var "+
			"`base` as a captured parameter (a var used only inside the nested "+
			"lambda must still be threaded into the coroutine):\n%s", sig)
	}

	// The caller must load `base` and pass it to the coroutine spawn — proving
	// the real value (not a zero) is what reaches the closure's env.
	caller := findDefinedFunc(ir, "@__user.wrap(")
	if caller == "" {
		t.Fatalf("expected caller @__user.wrap to be defined")
	}
	if !strings.Contains(caller, "load i64, i64* %base") {
		t.Errorf("caller must load the real value of `base` before spawning the "+
			"go-block coroutine:\n%s", caller)
	}
	if !strings.Contains(caller, "@.goroutine.0(i64 ") {
		t.Errorf("caller must pass the loaded `base` value to the go-block "+
			"coroutine spawn:\n%s", caller)
	}
}

// TestT0740_BlockLocalNotThreadedIntoCoroutine — when the trailing closure
// captures BOTH an outer-function local (`base`) and a block-local declared
// inside the `go { }` (`inner`), only the outer local is threaded into the
// coroutine arg pack. The block-local is already a coroutine local (declared in
// the block, which becomes the coroutine body), so it must NOT also appear as a
// captured parameter. This locks the `outerLocals` filter on the lambda path:
// without it, every lambda capture (including block-locals) would be added to
// captureNames and the caller would try to load a local that does not exist in
// its scope (confirmed: removing only the filter crashes codegen).
func TestT0740_BlockLocalNotThreadedIntoCoroutine(t *testing.T) {
	ir := generateIR(t, `
		wrap() task[() -> int] {
			base := 40;
			task[() -> int] x = go {
				inner := 100;
				|| -> base + inner
			};
			return x;
		}
		main() { }
	`)

	coro := findDefinedFunc(ir, "@.goroutine.0(")
	if coro == "" {
		t.Fatalf("expected go-block coroutine @.goroutine.0 to be defined")
	}
	sig := coro
	if nl := strings.Index(coro, "\n"); nl >= 0 {
		sig = coro[:nl]
	}
	// Signature line is e.g. `define i8* @.goroutine.0(i64 %base.cap) ...`.
	paramList := sig
	if open := strings.Index(sig, "("); open >= 0 {
		if close := strings.Index(sig[open:], ")"); close >= 0 {
			paramList = sig[open : open+close+1]
		}
	}
	// The outer local must be a captured parameter.
	if !strings.Contains(paramList, "%base.cap") {
		t.Errorf("outer-function local `base` must be threaded into the "+
			"coroutine arg pack:\n%s", sig)
	}
	// The block-local must NOT be threaded (it is a coroutine local, in scope
	// inside the coroutine body — threading it would duplicate the name and
	// make the caller load a nonexistent local).
	if strings.Contains(paramList, "inner") {
		t.Errorf("block-local `inner` must NOT be threaded into the coroutine "+
			"arg pack (the outerLocals filter must exclude it):\n%s", sig)
	}
}
