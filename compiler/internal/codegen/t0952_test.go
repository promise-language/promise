package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T0952: a BOUND elvis `?:` whose result is a single-owner native handle (Arc,
// Channel, Weak, Mutex, MutexGuard, Task), where the source optional is none, must
// neutralize the none-path default's own scope-exit drop. The bound variable claims
// the elvis result temp and takes an UNCONDITIONAL owning drop (maybeRegisterDrop);
// on the none-path it aliases the default, so without neutralizing the default's
// owner the same handle is freed twice — Mutex.drop (pal_mutex_destroy + pal_free)
// is non-idempotent → SEGV; Arc/Channel survive a benign use-after-free.
//
// The fix sets c.elvisResultBound at the three RHS-eval sites (genInferredVarDecl,
// genTypedVarDecl, genAssignStmt) when the RHS is a (paren-peeled) elvis; genElvis
// then clears the none-path default's drop flag (owned-local default) or claims its
// stmt-temp (fresh-temp default) inside the elvis.none block ONLY — path-conditional
// so the unselected default on the some-path still drops via its own binding. The
// INLINE case (T0951) is deliberately left borrow-on-none ([true,false]) — gated to
// the bound case so the inline IR and all its tests stay byte-identical.

// elvisNoneBlock extracts the text of the `elvis.none.N:` basic block — from its
// label to its terminating `br label %elvis.merge`. RE2 has no lookahead, so the
// non-greedy `(?s).*?` stops at the FIRST merge branch, which is exactly this
// block's terminator (each elvis emits one none→merge edge). Tests with a single
// elvis get the one block.
var elvisNoneBlock = regexp.MustCompile(`(?s)elvis\.none\.\d+:.*?br label %elvis\.merge`)

func noneBlockOf(t *testing.T, fn string) string {
	t.Helper()
	blk := elvisNoneBlock.FindString(fn)
	if blk == "" {
		t.Fatalf("could not locate elvis.none block in function:\n%s", fn)
	}
	return blk
}

// TestT0952MutexBoundNoneClearsDefaultFlag locks the core fix: `m := a ?: b` with
// an owned-local Mutex default neutralizes `b`'s scope-exit drop flag in the
// elvis.none block. Pre-fix `b` kept its owner AND the bound `m` owned the aliased
// mutex → two Mutex.drop calls on the same pointer → SEGV.
func TestT0952MutexBoundNoneClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Mutex[int]? a = none;
			Mutex[int] b = Mutex[int](9);
			m := a ?: b;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("bound none-path Mutex default must clear its drop flag in the elvis.none block (T0952)\n%s", none)
	}
}

// TestT0952ArcBoundNoneClearsDefaultFlag covers the Arc arm — the latent
// use-after-free this fix also closes (Arc.drop merely survived the double-free).
func TestT0952ArcBoundNoneClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Ref[int]? a = none;
			Ref[int] b = Ref[int](9);
			m := a ?: b;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("bound none-path Arc default must clear its drop flag in the elvis.none block (T0952)\n%s", none)
	}
}

// TestT0952ChannelBoundNoneClearsDefaultFlag covers the Channel arm.
func TestT0952ChannelBoundNoneClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Channel[int]? a = none;
			Channel[int] b = Channel[int](1);
			m := a ?: b;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("bound none-path Channel default must clear its drop flag in the elvis.none block (T0952)\n%s", none)
	}
}

// TestT0952TypedDeclBoundNoneClearsDefaultFlag locks the genTypedVarDecl RHS-eval
// site: `Mutex[int] m = a ?: b` must clear the default's flag identically to the
// inferred form.
func TestT0952TypedDeclBoundNoneClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Mutex[int]? a = none;
			Mutex[int] b = Mutex[int](9);
			Mutex[int] m = a ?: b;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("typed-decl bound none-path Mutex default must clear its drop flag in the elvis.none block (T0952)\n%s", none)
	}
}

// TestT0952AssignBoundNoneClearsDefaultFlag locks the genAssignStmt RHS-eval site:
// `m = a ?: b` (re-assignment to an existing Mutex local) must clear the default's
// flag. `m` is initialized from a third mutex so the prior value is dropped before
// the elvis; the elvis none-path then transfers `b` into `m`.
func TestT0952AssignBoundNoneClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Mutex[int]? a = none;
			Mutex[int] b = Mutex[int](9);
			Mutex[int] m = Mutex[int](1);
			m = a ?: b;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("assignment bound none-path Mutex default must clear its drop flag in the elvis.none block (T0952)\n%s", none)
	}
}

// TestT0952ParenBoundNoneClearsDefaultFlag confirms the paren-peel: `m := (a ?: b)`
// still routes through the bound transfer (unwrapDestructureParens peels the parens
// at the RHS-eval site).
func TestT0952ParenBoundNoneClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Mutex[int]? a = none;
			Mutex[int] b = Mutex[int](9);
			m := (a ?: b);
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("parenthesized bound none-path Mutex default must clear its drop flag in the elvis.none block (T0952)\n%s", none)
	}
}

// TestT0952ParenDefaultBoundNoneClearsDefaultFlag confirms the none-path default
// operand peel: `m := a ?: (b)` (parens around the DEFAULT, not the whole elvis)
// must still clear the owned-local `b`'s flag. Without peeling e.Right this falls to
// the fresh-temp claim, which cannot reach a local's scope-exit drop flag → SEGV.
func TestT0952ParenDefaultBoundNoneClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Mutex[int]? a = none;
			Mutex[int] b = Mutex[int](9);
			m := a ?: (b);
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("paren-default bound none-path Mutex default must clear its drop flag in the elvis.none block (T0952)\n%s", none)
	}
}

// TestT0952GenericContextBoundNoneClearsDefaultFlag locks the typeSubst arm of
// elvisResultHandleDrop. Inside a generic function the elvis result type is
// `Mutex[T]` (with TypeParam T); elvisResultHandleDrop substitutes it to the
// concrete `Mutex[int]` so the handle is recognized and the bound none-path default
// is neutralized in the monomorphized body.
func TestT0952GenericContextBoundNoneClearsDefaultFlag(t *testing.T) {
	ir := generateIR(t, `
		pick[T](T seed) {
			Mutex[T]? a = none;
			Mutex[T] b = Mutex[T](seed);
			m := a ?: b;
		}
		demo() { pick(7); }
	`)
	// extractDefine anchors on `define` — extractFunction would latch onto the call
	// site inside demo() since `@"pick[int]"(` appears there first.
	fn := extractDefine(ir, `"pick[int]"`)
	if fn == "" {
		t.Fatal("could not extract monomorphized pick[int]")
	}
	none := noneBlockOf(t, fn)
	if !strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("generic-context bound none-path Mutex default must clear its drop flag in the elvis.none block (T0952)\n%s", none)
	}
}

// TestT0952InlineUnchanged locks "inline IR unchanged": the inline form
// `(a ?: b).borrow;` (discarded, not bound) must NOT clear the none-path default's
// flag and must still carry the per-path flag phi ([true,false] — owns the some-path
// inner, borrows the none-path default). This is T0951's deliberate borrow-on-none.
func TestT0952InlineUnchanged(t *testing.T) {
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
		t.Errorf("inline (non-bound) elvis must NOT clear the none-path default's drop flag — borrow-on-none is preserved (T0951)\n%s", none)
	}
	// The per-path flag phi survives unchanged (some owns, none borrows).
	assertContainsMatch(t, fn, elvisPathFlag)
}

// TestT0952BorrowedParamDefaultNoClear locks the no-op path: when the bound elvis
// default is a borrowed handle parameter (no scope-exit drop flag in the callee),
// the none-path clear must no-op — no spurious `store i1 false` against a flag that
// does not exist. (The bound borrowed-param default has its own orthogonal,
// pre-existing double-free — filed as T0981; the bound variable still takes an
// unconditional owning drop of the caller-owned handle. This test only asserts the
// T0952 default-neutralization is a safe no-op here and does not make T0981 worse.)
func TestT0952BorrowedParamDefaultNoClear(t *testing.T) {
	ir := generateIR(t, `
		pick(Mutex[int]? a, Mutex[int] b) {
			m := a ?: b;
		}
		demo() {
			Mutex[int] x = Mutex[int](9);
			Mutex[int]? a = none;
			pick(a, x);
		}
	`)
	fn := extractFunction(ir, "__user.pick")
	if fn == "" {
		t.Fatal("could not extract __user.pick")
	}
	none := noneBlockOf(t, fn)
	if strings.Contains(none, "store i1 false, i1* %b.dropflag") {
		t.Errorf("borrowed-param default has no drop flag — the bound none-path clear must no-op (T0952)\n%s", none)
	}
}
