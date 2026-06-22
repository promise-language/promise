package codegen

import (
	"strings"
	"testing"
)

// extractGoroutineCoro returns the IR body of the `.goroutine.0` coroutine
// function. extractFunction can't be used here: `@.goroutine.0(` also appears at
// the ramp call site (before the definition), so anchoring on the bare name
// captures the wrong function. Anchor on the `define ... @.goroutine.0(` line.
func extractGoroutineCoro(t *testing.T, ir string) string {
	t.Helper()
	marker := "@.goroutine.0("
	for i := 0; i < len(ir); {
		idx := strings.Index(ir[i:], marker)
		if idx < 0 {
			break
		}
		pos := i + idx
		lineStart := strings.LastIndex(ir[:pos], "\n") + 1
		if strings.HasPrefix(ir[lineStart:], "define ") {
			rest := ir[lineStart:]
			if end := strings.Index(rest, "\n}\n"); end >= 0 {
				return rest[:end+2]
			}
			return rest
		}
		i = pos + len(marker)
	}
	t.Fatalf("expected a define of .goroutine.0 coroutine function in IR\n%s", ir)
	return ""
}

// T1098: the `t := go f(expr)` task-handle form must transfer ownership of a
// heap argument temporary into the goroutine frame. For borrow params the
// goroutine frame drops the temporary after the call; for move params the callee
// consumes it, so the goroutine emits no drop.

func TestT1098GoCallStringBorrowDropsInCoro(t *testing.T) {
	ir := generateIR(t, `
		type R { int n; }
		f(string p) R { return R(n: p.len); }
		main() {
			string s = "alpha";
			t := go f(s.clone());
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Borrow param: the goroutine frame owns the cloned string and must free it.
	assertContains(t, coro, "@promise_string_drop")
}

func TestT1098GoCallStringMoveNoCoroDrop(t *testing.T) {
	ir := generateIR(t, `
		type R { int n; }
		f(string move p) R { return R(n: p.len); }
		main() {
			string s = "alpha";
			t := go f(s.clone());
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Move param: the callee consumes and drops the string at its own scope exit,
	// so the goroutine body must NOT drop it (that would be a double-free).
	assertNotContains(t, coro, "@promise_string_drop")
}

func TestT1098GoCallUserTypeBorrowDropsInCoro(t *testing.T) {
	ir := generateIR(t, `
		type R { int n; }
		type Box { string s; }
		f(Box b) R { return R(n: b.s.len); }
		main() {
			string s = "alpha";
			t := go f(Box(s: s.clone()));
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Borrow param: the goroutine frame owns the heap Box and drops it.
	assertContains(t, coro, "@Box.drop")
}

func TestT1098GoCallUserTypeMoveNoCoroDrop(t *testing.T) {
	ir := generateIR(t, `
		type R { int n; }
		type Box { string s; }
		f(Box move b) R { return R(n: b.s.len); }
		main() {
			string s = "alpha";
			t := go f(Box(s: s.clone()));
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Move param: callee consumes the Box, so no goroutine-side drop.
	assertNotContains(t, coro, "@Box.drop")
}

func TestT1098GoCallVectorBorrowDropsElementsInCoro(t *testing.T) {
	ir := generateIR(t, `
		type R { int n; }
		f(string[] v) R { return R(n: v.len); }
		main() {
			string[] xs = ["aa", "bbb"];
			t := go f(xs.clone());
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Borrow param: the goroutine frame owns the cloned vector and must drop its
	// droppable (string) elements before freeing the buffer. The element-drop
	// loop calls promise_string_drop on each element, then Vector.drop frees it.
	assertContains(t, coro, "@promise_string_drop")
	assertContains(t, coro, "@Vector.drop")
}

func TestT1098GoCallTwoBorrowArgsBothDropInCoro(t *testing.T) {
	ir := generateIR(t, `
		type R { int n; }
		f(string a, string b) R { return R(n: a.len + b.len); }
		main() {
			string s = "abc";
			string u = "wxyz";
			t := go f(s.clone(), u.clone());
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Both heap argument temporaries are owned by the goroutine frame → exactly
	// two string drops in the coroutine body.
	if got := strings.Count(coro, "@promise_string_drop"); got != 2 {
		t.Fatalf("expected 2 goroutine-side string drops for two heap borrow args, got %d\n%s", got, coro)
	}
}

func TestT1098GoCallMixedMoveBorrowOneCoroDrop(t *testing.T) {
	ir := generateIR(t, `
		type R { int n; }
		f(string move a, string b) R { return R(n: a.len + b.len); }
		main() {
			string s = "abc";
			string u = "wxyz";
			t := go f(s.clone(), u.clone());
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Arg 0 is consumed by the callee (no goroutine drop); arg 1 is a borrow the
	// goroutine frame drops → exactly one string drop in the coroutine body.
	if got := strings.Count(coro, "@promise_string_drop"); got != 1 {
		t.Fatalf("expected 1 goroutine-side string drop for mixed move+borrow args, got %d\n%s", got, coro)
	}
}
