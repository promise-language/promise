package codegen

import (
	"testing"
)

// T1148: `go f(move x)` where x is a NAMED local/loop variable bound to a heap
// value must clear the variable's caller-side drop flag — the move param's
// callee consumes and drops it. Without the fix the caller's scope/loop teardown
// frees the same allocation the callee already freed → double free
// (`fatal: invalid free (bad header magic)`).
//
// T1098 only covered heap *temporary* arg roots (`s.clone()`, `Box(s: ...)`);
// the named-variable move root (no temporary to transfer) was the uncovered
// case this test guards.

func TestT1148GoCallMoveNamedVarNoCoroDrop(t *testing.T) {
	ir := generateIR(t, `
		type R { int n; }
		f(string move p) R { return R(n: p.len); }
		main() {
			string s = "alpha".clone();
			t := go f(move s);
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Move param: the callee consumes and drops the moved string at its own scope
	// exit, so the goroutine body must NOT drop it (that would be a double-free).
	assertNotContains(t, coro, "@promise_string_drop")
}

func TestT1148GoCallMoveNamedVarClearsCallerDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type R { int n; }
		f(string move p) R { return R(n: p.len); }
		main() {
			string s = "alpha".clone();
			t := go f(move s);
			r := <-t;
		}
	`)
	// The caller's body (the `.goroutine.main` coroutine — user `main()` is
	// compiled into a goroutine) must clear s's drop flag (store i1 false) so its
	// scope teardown does not also free the now-consumed allocation.
	mainCoro := extractDefine(ir, ".goroutine.main")
	assertContains(t, mainCoro, "store i1 false")
}
