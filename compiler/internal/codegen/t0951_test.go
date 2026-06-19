package codegen

import (
	"strings"
	"testing"
)

// T0951: an inline (used/discarded, not bound) elvis `?:` whose result is a
// single-owner native handle represented as a bare i8* — Ref[T], Channel[T],
// Mutex[T], Weak[T], MutexGuard[T], Task[T] — must drop the orphaned some-path
// handle via its native drop. These bypass both existing trackers: elvisResultDrop
// only resolves Vector/string, and trackElvisResultHeap requires a 2-word
// {i8*,i8*} value struct. So the moved-out handle had no owner → leak. The fix
// (elvisResultHandleDrop, wired into trackElvisResultTemp's !owned fall-through)
// registers the handle result with the SAME per-branch flag the other
// representations use: owned (true) on the orphaned some-path, borrowed (false) on
// the none-path where the default keeps its own owner.
//
// These tests lock the IR signature: (1) an owned-local handle source is tracked
// with the per-path flag phi and dropped via the handle's native drop; (2) a
// borrowed-param source is NOT tracked (orphan gate); (3) the Mutex `.lock()`
// receiver-temp promotion (T0655) threads the temp's *live* per-branch flag into
// the promoted scope binding instead of hardcoding 1 — otherwise the borrowed
// none-path default is force-dropped and then double-freed by its own binding.

// TestT0951ArcInlineElvisHandleTracked verifies that an inline Ref[int] elvis with
// an owned-local source carries the per-path drop flag phi ([true, false] — owns
// the moved inner on the some-path, borrows the default on the none-path) and is
// dropped via @"Ref[int].drop" at statement end. Pre-fix the bare-i8* Arc result
// matched neither tracker and leaked.
func TestT0951ArcInlineElvisHandleTracked(t *testing.T) {
	ir := generateIR(t, `
		sink(int n) { }
		demo() {
			Ref[int]? a = Ref[int](7);
			Ref[int] b = Ref[int](9);
			sink((a ?: b).borrow);
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	// Per-path flag: owns the some-path moved inner, borrows the none-path default.
	assertContainsMatch(t, fn, elvisPathFlag)
	// The orphaned some-path handle is freed exactly once via the native Arc drop.
	assertContains(t, fn, `call void @"Ref[int].drop"`)
}

// TestT0951ChannelInlineElvisHandleTracked locks the Channel arm of
// elvisResultHandleDrop. An inline Channel[int] elvis with an owned-local source
// carries the per-path flag phi and the orphaned some-path channel is freed via
// @"Channel[int].drop" (which walks any buffered items, then frees). Channel works
// single-threaded so no scheduler is needed.
func TestT0951ChannelInlineElvisHandleTracked(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Channel[int]? a = Channel[int](1);
			Channel[int] b = Channel[int](1);
			(a ?: b).close();
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	assertContainsMatch(t, fn, elvisPathFlag)
	assertContains(t, fn, `call void @"Channel[int].drop"`)
}

// TestT0951WeakInlineElvisHandleTracked locks the Weak arm. The inline elvis result
// is a Weak[int]; `.upgrade()` borrows it (returning Ref[int]?) so the moved-out
// Weak temp must still be dropped via @"Weak[int].drop". The outer `?: Ref[int](0)`
// is a second elvis that yields the Arc whose `.borrow` is read — its presence does
// not affect the Weak result's per-path tracking.
func TestT0951WeakInlineElvisHandleTracked(t *testing.T) {
	ir := generateIR(t, `
		sink(int n) { }
		demo() {
			Ref[int] keep = Ref[int](7);
			Weak[int]? a = keep.downgrade();
			Weak[int] b = keep.downgrade();
			sink(((a ?: b).upgrade() ?: Ref[int](0)).borrow);
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	assertContainsMatch(t, fn, elvisPathFlag)
	assertContains(t, fn, `call void @"Weak[int].drop"`)
}

// TestT0951MutexGuardInlineElvisHandleTracked locks the MutexGuard arm — the only
// handle class with no e2e and no Go coverage before this. A MutexGuard[int] held
// in an optional (from `.lock()`) and used inline via `.borrow` orphans the guard on
// the some-path; it must be released exactly once via @MutexGuard.drop (a single
// non-parameterized function — note: no quotes, unlike the generic handles).
func TestT0951MutexGuardInlineElvisHandleTracked(t *testing.T) {
	ir := generateIR(t, `
		sink(int n) { }
		demo() {
			Mutex[int] m1 = Mutex[int](7);
			Mutex[int] m2 = Mutex[int](9);
			MutexGuard[int]? a = m1.lock();
			MutexGuard[int] b = m2.lock();
			sink((a ?: b).borrow);
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	assertContainsMatch(t, fn, elvisPathFlag)
	assertContains(t, fn, `call void @MutexGuard.drop`)
}

// TestT0951TaskInlineElvisHandleTracked locks the Task arm. A discarded inline Task
// elvis (`(a ?: b);` — neither awaited nor bound) is the leak repro for tasks; the
// orphaned some-path handle must be freed via @"Task[int].drop", which
// cleanupStmtTemps routes through the cooperative join.
func TestT0951TaskInlineElvisHandleTracked(t *testing.T) {
	ir := generateIR(t, `
		get_42() int { return 42; }
		demo() {
			Task[int]? a = go get_42();
			Task[int] b = go get_42();
			(a ?: b);
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	assertContainsMatch(t, fn, elvisPathFlag)
	assertContains(t, fn, `call void @"Task[int].drop"`)
}

// TestT0951BorrowedHandleElvisNotTracked locks the orphan gate for the handle
// class: when the inline elvis source is a *borrowed* Arc param the caller owns and
// drops it, so the callee must NOT also drop the result. The body must therefore
// carry neither the per-path flag phi nor an Arc drop for the result.
func TestT0951BorrowedHandleElvisNotTracked(t *testing.T) {
	ir := generateIR(t, `
		sink(int n) { }
		borrowed_arc(Ref[int]? a, Ref[int] b) int {
			return (a ?: b).borrow;
		}
		demo() {
			Ref[int]? a = Ref[int](7);
			Ref[int] b = Ref[int](9);
			sink(borrowed_arc(a, b));
		}
	`)
	fn := extractFunction(ir, "__user.borrowed_arc")
	if fn == "" {
		t.Fatal("could not extract __user.borrowed_arc")
	}
	assertNotContainsMatch(t, fn, elvisPathFlag)
	if strings.Contains(fn, `call void @"Ref[int].drop"`) {
		t.Errorf("borrowed-param handle elvis must NOT drop its result — the caller owns it (T0951)\n%s", fn)
	}
}

// TestT0951GenericContextHandleElvisSubstituted locks the typeSubst arm of
// elvisResultHandleDrop. Inside a generic function the inline elvis result type is
// `Channel[T]` (with TypeParam T); elvisResultHandleDrop must substitute it to the
// concrete `Channel[int]` BEFORE types.AsChannel resolves the element, or the drop
// would resolve against an unbound TypeParam. The monomorphized body must carry the
// per-path flag and drop via @"Channel[int].drop". (`.close()` borrows the channel,
// leaving the orphaned some-path handle for the temp drop to free.)
func TestT0951GenericContextHandleElvisSubstituted(t *testing.T) {
	ir := generateIR(t, `
		pick_chan[T](T seed) {
			Channel[T]? a = Channel[T](1);
			Channel[T] b = Channel[T](1);
			(a ?: b).close();
		}
		demo() { pick_chan(7); }
	`)
	// extractDefine (anchors on `define`) — extractFunction would latch onto the
	// call site inside demo() since `@"pick_chan[int]"(` appears there first.
	fn := extractDefine(ir, `"pick_chan[int]"`)
	if fn == "" {
		t.Fatal("could not extract monomorphized pick_chan[int]")
	}
	assertContainsMatch(t, fn, elvisPathFlag)
	assertContains(t, fn, `call void @"Channel[int].drop"`)
}

// TestT0951MutexLockPromotionThreadsPerBranchFlag locks the second half of the
// fix (stmt.go promoteHandleTempToScopeBinding). `(a ?: b).lock()` on an inline
// Mutex elvis promotes the receiver temp to a scope binding (T0655). The promotion
// must store the temp's *loaded* per-branch flag into the promoted binding's
// dropflag, NOT a hardcoded `i1 1`. With the hardcode, the borrowed none-path
// default would be force-dropped by the promoted binding AND by its own scope
// binding → double-free SEGV.
//
// IR signature of the fix, in the elvis.merge block:
//
//	%P = phi i1 [ true, %elvis.some.N ], [ false, %elvis.none.N ]  ; per-branch flag
//	store i1 %P, i1* %tmpflag                                      ; appendStmtTemp
//	%L = load i1, i1* %tmpflag                                     ; promotion loads it
//	store i1 %L, i1* %bindingflag                                  ; promotion stores LOADED flag
//	store i1 false, i1* %tmpflag                                   ; claimStringTemp neutralizes temp
//
// Pre-fix the third line was `store i1 true, i1* %bindingflag` with no preceding
// load, so the load→store-register→store-false sequence is unique to the fix.
func TestT0951MutexLockPromotionThreadsPerBranchFlag(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Mutex[int]? a = Mutex[int](7);
			Mutex[int] b = Mutex[int](9);
			g := (a ?: b).lock();
			g.close();
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	// The Mutex result is now tracked with the per-path flag (owns some, borrows none).
	assertContainsMatch(t, fn, elvisPathFlag)
	// The receiver-temp promotion fired (a scope-binding Mutex drop, strdrop.exec).
	assertContains(t, fn, "strdrop.exec")
	assertContains(t, fn, `call void @"Mutex[int].drop"`)
	// The promotion threads the temp's LOADED per-branch flag into the promoted
	// binding (load i1; store i1 <reg>; store i1 false) — NOT a hardcoded store i1 1.
	assertContainsMatch(t, fn,
		`%\d+ = load i1, i1\* %\d+\s*\n\s*store i1 %\d+, i1\* %\d+\s*\n\s*store i1 false, i1\* %\d+`)
}
