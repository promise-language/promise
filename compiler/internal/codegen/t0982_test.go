package codegen

import (
	"strings"
	"testing"
)

// T0982: `return a ?: b` whose result is a single-owner native handle (Arc, Channel,
// Weak, Mutex, MutexGuard, Task) or a droppable heap type (Map/Set/heap-user), where
// the none-path default `b` is an owned local, must neutralize `b`'s own scope-exit
// drop. The returned value escapes to the caller (who owns and drops it); without the
// neutralization the function's scope-exit cleanup ALSO frees the same allocation →
// double free (Mutex.drop's pal_mutex_destroy+pal_free is non-idempotent → SEGV; Arc a
// benign use-after-free; Map/heap-user a real double free).
//
// The gap: genElvis's none-path handle/heap neutralization branch was gated to
// boundResult only (T0952). A return sets no such flag, so `b` kept its owner. The fix
// adds a returnedResult signal (set by genReturnStmt when the return expression peels
// to an elvis) that reaches the SAME branch — clearing the owned-local default's drop
// flag in the elvis.none block ONLY (path-conditional; the some-path's unselected
// default still drops via its own binding). Vector/string returns were already correct
// (their neutralization at elvisResultDrop fires unconditionally, T0936). Unlike the
// bound case, the return path creates NO per-path elvisBoundDropFlag — the escaping
// result temp is claimed unconditionally in genReturnStmt.

// TestT0982MutexReturnClearsDefaultFlag locks the core fix: `return a ?: b` with an
// owned-local Mutex default clears `b`'s scope-exit drop flag in the elvis.none block.
func TestT0982MutexReturnClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		make_mutex(int seed) Mutex[int] {
			Mutex[int]? a = none;
			Mutex[int] b = Mutex[int](seed);
			return a ?: b;
		}
		demo() { make_mutex(9); }
	`)
	fn := extractFunction(ir, "__user.make_mutex")
	if fn == "" {
		t.Fatal("could not extract __user.make_mutex")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("returned none-path Mutex default must clear its drop flag in the elvis.none block (T0982)\n%s", none)
	}
}

// TestT0982ArcReturnClearsDefaultFlag covers the Arc arm — the latent use-after-free
// this fix also closes (Arc.drop merely survived the double-free).
func TestT0982ArcReturnClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		make_arc(int seed) Ref[int] {
			Ref[int]? a = none;
			Ref[int] b = Ref[int](seed);
			return a ?: b;
		}
		demo() { make_arc(9); }
	`)
	fn := extractFunction(ir, "__user.make_arc")
	if fn == "" {
		t.Fatal("could not extract __user.make_arc")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("returned none-path Arc default must clear its drop flag in the elvis.none block (T0982)\n%s", none)
	}
}

// TestT0982ChannelReturnClearsDefaultFlag covers the Channel arm.
func TestT0982ChannelReturnClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		make_channel() Channel[int] {
			Channel[int]? a = none;
			Channel[int] b = Channel[int](1);
			return a ?: b;
		}
		demo() { make_channel(); }
	`)
	fn := extractFunction(ir, "__user.make_channel")
	if fn == "" {
		t.Fatal("could not extract __user.make_channel")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("returned none-path Channel default must clear its drop flag in the elvis.none block (T0982)\n%s", none)
	}
}

// TestT0982HeapUserReturnClearsDefaultFlag covers the elvisResultHeapDrop arm — a
// droppable heap USER type. This is the arm that double-frees pre-fix.
func TestT0982HeapUserReturnClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		type Box { string name; drop(~this) {} }
		make_box() Box {
			Box? a = none;
			Box b = Box(name: "x");
			return a ?: b;
		}
		demo() { make_box(); }
	`)
	fn := extractFunction(ir, "__user.make_box")
	if fn == "" {
		t.Fatal("could not extract __user.make_box")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("returned none-path heap-user default must clear its drop flag in the elvis.none block (T0982)\n%s", none)
	}
}

// TestT0982MapReturnClearsDefaultFlag covers the Map (2-word value struct) arm — the
// same gated branch, so the single condition change fixes it too.
func TestT0982MapReturnClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		make_map() map[string, int] {
			map[string, int]? a = none;
			map[string, int] b = map[string, int]();
			b["k"] = 9;
			return a ?: b;
		}
		demo() { make_map(); }
	`)
	fn := extractFunction(ir, "__user.make_map")
	if fn == "" {
		t.Fatal("could not extract __user.make_map")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("returned none-path Map default must clear its drop flag in the elvis.none block (T0982)\n%s", none)
	}
}

// TestT0982ReturnParenDefaultClearsDefaultFlag confirms the none-path default operand
// peel: `return a ?: (b)` (parens around the DEFAULT) still clears the owned-local
// `b`'s flag (neutralizeElvisNoneDefault peels parens around e.Right).
func TestT0982ReturnParenDefaultClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		make_mutex(int seed) Mutex[int] {
			Mutex[int]? a = none;
			Mutex[int] b = Mutex[int](seed);
			return a ?: (b);
		}
		demo() { make_mutex(9); }
	`)
	fn := extractFunction(ir, "__user.make_mutex")
	if fn == "" {
		t.Fatal("could not extract __user.make_mutex")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("returned paren-default none-path Mutex default must clear its drop flag (T0982)\n%s", none)
	}
}

// TestT0982ReturnParenWholeClearsDefaultFlag confirms `return (a ?: b)` (parens around
// the whole elvis) still routes through the return signal — genReturnStmt peels the
// outer parens (unwrapDestructureParens) before checking for the elvis.
func TestT0982ReturnParenWholeClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		make_mutex(int seed) Mutex[int] {
			Mutex[int]? a = none;
			Mutex[int] b = Mutex[int](seed);
			return (a ?: b);
		}
		demo() { make_mutex(9); }
	`)
	fn := extractFunction(ir, "__user.make_mutex")
	if fn == "" {
		t.Fatal("could not extract __user.make_mutex")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("returned parenthesized-whole none-path Mutex default must clear its drop flag (T0982)\n%s", none)
	}
}

// TestT0982NestedReturnClearsInnermostDefaultFlag locks the nested composition:
// `return a ?: (b ?: c)` must clear the INNERMOST owned-local default `c` on the
// all-none path. The return signal propagates into the inner elvis (mirroring T0983's
// bound propagation) so IT neutralizes its own terminal default; without this the inner
// stays inline (borrow-on-none) and `c` escapes while its binding ALSO drops it → SEGV.
func TestT0982NestedReturnClearsInnermostDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		make_mutex(int seed) Mutex[int] {
			Mutex[int]? a = none;
			Mutex[int]? b = none;
			Mutex[int] c = Mutex[int](seed);
			return a ?: (b ?: c);
		}
		demo() { make_mutex(9); }
	`)
	fn := extractFunction(ir, "__user.make_mutex")
	if fn == "" {
		t.Fatal("could not extract __user.make_mutex")
	}
	if !strings.Contains(fn, "store i1 false, i1* %c.dropflag") {
		t.Errorf("nested returned elvis must clear the innermost owned-local Mutex default's drop flag (T0982)\n%s", fn)
	}
}

// TestT0982TripleNestedReturnClearsDeepestDefaultFlag confirms the propagation recurses
// through an extra level: `return a ?: (b ?: (c ?: d))` must clear the deepest `d`.
func TestT0982TripleNestedReturnClearsDeepestDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		make_arc(int seed) Ref[int] {
			Ref[int]? a = none;
			Ref[int]? b = none;
			Ref[int]? c = none;
			Ref[int] d = Ref[int](seed);
			return a ?: (b ?: (c ?: d));
		}
		demo() { make_arc(9); }
	`)
	fn := extractFunction(ir, "__user.make_arc")
	if fn == "" {
		t.Fatal("could not extract __user.make_arc")
	}
	if !strings.Contains(fn, "store i1 false, i1* %d.dropflag") {
		t.Errorf("triple-nested returned elvis must clear the deepest owned-local default's drop flag (T0982)\n%s", fn)
	}
}

// TestT0982InlineDiscardedUnchanged locks that the return fix does NOT perturb the
// INLINE discarded case (T0951): `(a ?: b).borrow;` in a non-return statement must
// still borrow-on-none (no flag clear). The returnedResult signal is scoped strictly to
// the return-expression eval site.
func TestT0982InlineDiscardedUnchanged(t *testing.T) {
	ir := generateIR(t, `
		sink(int n) { }
		demo() {
			Ref[int]? a = none;
			Ref[int] b = Ref[int](9);
			sink((a ?: b).borrow);
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	none := noneBlockOf(t, fn)
	if strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("inline discarded elvis must NOT clear the none-path default's drop flag — borrow-on-none preserved (T0951)\n%s", none)
	}
}
