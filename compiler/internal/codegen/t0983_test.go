package codegen

import (
	"strings"
	"testing"
)

// T0983: a BOUND droppable elvis whose default operand is ITSELF an elvis
// (`m := a ?: (b ?: c)`) must neutralize the INNERMOST terminal default's scope-exit
// drop when both optionals are none. T0952 handles the simple bound case, but its
// paren-peel does not reach through a nested elvis — the outer would value-identity
// claim the inner elvis's phi result (claiming nothing), leaving the innermost
// owned-local default with its scope-exit owner while the bound `m` ALSO took an
// owning drop → two Mutex.drop calls on the same pointer → SEGV.
//
// The fix propagates the bound obligation into the inner elvis so IT clears its own
// terminal default's flag and threads its per-path bound flag up as the outer's
// none-path ownership. The IR signature: the innermost owned-local default's drop
// flag is cleared (which pre-fix never happened, since the inner elvis was inline and
// inline elvis borrows-on-none).

// TestT0983NestedMutexClearsInnermostDefaultFlag locks the core fix: the innermost
// owned-local Mutex default `c` has its scope-exit drop flag cleared on the all-none
// path. noneBlockOf targets a single elvis block and cannot cleanly isolate the inner
// none block in a nested layout, so assert against the whole function body.
func TestT0983NestedMutexClearsInnermostDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Mutex[int]? a = none;
			Mutex[int]? b = none;
			Mutex[int] c = Mutex[int](9);
			m := a ?: (b ?: c);
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	if !strings.Contains(fn, "store i1 false, i1* %c.dropflag") {
		t.Errorf("nested bound elvis must clear the innermost owned-local Mutex default's drop flag (T0983)\n%s", fn)
	}
}

// TestT0983NestedArcClearsInnermostDefaultFlag covers the Arc arm — the latent
// use-after-free the fix also closes (Arc.drop merely survived the double-free).
func TestT0983NestedArcClearsInnermostDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Ref[int]? a = none;
			Ref[int]? b = none;
			Ref[int] c = Ref[int](9);
			m := a ?: (b ?: c);
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	if !strings.Contains(fn, "store i1 false, i1* %c.dropflag") {
		t.Errorf("nested bound elvis must clear the innermost owned-local Arc default's drop flag (T0983)\n%s", fn)
	}
}

// TestT0983TripleNestedClearsDeepestDefaultFlag confirms the neutralization recurses
// through an extra elvis level: `m := a ?: (b ?: (c ?: d))` must clear the deepest
// owned-local default `d`.
func TestT0983TripleNestedClearsDeepestDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Mutex[int]? a = none;
			Mutex[int]? b = none;
			Mutex[int]? c = none;
			Mutex[int] d = Mutex[int](11);
			m := a ?: (b ?: (c ?: d));
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	if !strings.Contains(fn, "store i1 false, i1* %d.dropflag") {
		t.Errorf("triple-nested bound elvis must clear the deepest owned-local default's drop flag (T0983)\n%s", fn)
	}
}

// TestT0983NestedHeapUserClearsInnermostDefaultFlag covers the elvisResultHeapDrop arm
// of the droppableRes gate — a droppable heap USER type (distinct from the native-handle
// arm the Mutex/Arc tests exercise). This is the arm that SEGVs pre-fix: without the
// nested neutralization the innermost owned-local `c` keeps its scope-exit drop while the
// bound `m` also owns it → double free of the instance.
func TestT0983NestedHeapUserClearsInnermostDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		type Box { string name; drop(~this) {} }
		demo() {
			Box? a = none;
			Box? b = none;
			Box c = Box(name: "x");
			m := a ?: (b ?: c);
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	if !strings.Contains(fn, "store i1 false, i1* %c.dropflag") {
		t.Errorf("nested bound elvis must clear the innermost heap-user default's drop flag (T0983)\n%s", fn)
	}
}

// TestT0983NestedMutexParenInnerDefault confirms the inner neutralization peels parens
// around the terminal default operand: `m := a ?: (b ?: (c))`.
func TestT0983NestedMutexParenInnerDefault(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Mutex[int]? a = none;
			Mutex[int]? b = none;
			Mutex[int] c = Mutex[int](9);
			m := a ?: (b ?: (c));
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	if !strings.Contains(fn, "store i1 false, i1* %c.dropflag") {
		t.Errorf("nested bound elvis must peel parens to clear the innermost default's drop flag (T0983)\n%s", fn)
	}
}
