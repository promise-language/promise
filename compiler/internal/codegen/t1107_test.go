package codegen

import (
	"strings"
	"testing"
)

// T1107: an owned match/if-expression result (all arms yield owned i8* values —
// string / Vector[T] / native handle) passed to a BORROW parameter, or discarded,
// used to leak the selected value: the merge phi was never registered as an owned
// statement temp, so nothing freed it at the caller's statement end. The fix
// (trackMergeResultTemp) registers the phi with a per-path ownership flag phi —
// mirroring the elvis `?:` result precedent (trackElvisResultTemp). A bound /
// returned result claims the phi (flag zeroed) so there is no double free.

// A match whose arms both clone() → owned string passed to a borrow param. The
// merge block gets a `phi i1` ownership flag (both incomings `true`) and a
// flag-gated promise_string_drop at statement end.
func TestMatchOwnedStringResultTrackedForBorrow(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_len(string p) Holder { return Holder(n: p.len); }
		run(bool c) {
			r := borrow_len(match c { true => "abcdef".clone(), _ => "xy".clone() });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	// Ownership flag phi registered alongside the value phi (both arms owned).
	assertContains(t, fn, "phi i1 [ true,")
	// Statement-end cleanup can free the selected clone.
	assertContains(t, fn, "call void @promise_string_drop")
}

// A match whose arms both clone() a Vector[int] → owned vector result to a borrow
// param, freed via Vector.drop (which walks the element type + bit-63 static flag).
func TestMatchOwnedVectorResultTrackedForBorrow(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_vlen(int[] v) Holder { return Holder(n: v.len); }
		run(bool c) {
			r := borrow_vlen(match c { true => [1, 2, 3].clone(), _ => [9].clone() });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	assertContains(t, fn, "phi i1 [ true,")
	assertContains(t, fn, "call void @Vector.drop")
}

// The `if`-expression variant: both branches clone() → owned string to a borrow
// param. genBlockValue records the moved-out ownership (blockValueOwnedResult) so
// the if-merge phi is registered with an ownership flag phi.
func TestIfOwnedStringResultTrackedForBorrow(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_len(string p) Holder { return Holder(n: p.len); }
		run(bool c) {
			r := borrow_len(if c { "abcdef".clone() } else { "xy".clone() });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	assertContains(t, fn, "phi i1 [ true,")
	assertContains(t, fn, "call void @promise_string_drop")
}

// Negative: arms return BORROWED params (no owned temp, no drop flag) — the phi
// must NOT be registered as an owned temp (no ownership flag phi), so the real
// owner (the caller) frees the strings exactly once. Guards against a double free.
func TestMatchBorrowedResultNotTracked(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_len(string p) Holder { return Holder(n: p.len); }
		run(string a, string b, bool c) {
			r := borrow_len(match c { true => a, _ => b });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	if strings.Contains(fn, "phi i1") {
		t.Fatalf("expected no ownership flag phi for a borrowed-arm match result; got:\n%s", fn)
	}
}

// A match whose arms both construct a native handle (Ref[int]/Arc) → owned handle
// passed to a borrow param, freed via the per-instantiation Ref[int].drop
// (ownedI8PtrResultDrop's AsArc branch — not string/Vector). Confirms the
// non-string/Vector drop-function resolution is wired for the merge temp.
func TestMatchOwnedRefResultTrackedForBorrow(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_ref(Ref[int] a) Holder { return Holder(n: a.borrow); }
		run(bool c) {
			r := borrow_ref(match c { true => Ref[int](7), _ => Ref[int](9) });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	// Ownership flag phi registered (both arms construct an owned handle).
	assertContains(t, fn, "phi i1 [ true,")
	// Statement-end cleanup frees the selected handle via the Arc/Ref drop.
	assertContains(t, fn, `call void @"Ref[int].drop"`)
}

// Weak arms (arc.downgrade()) → owned Weak handle result to a borrow param, freed
// via the per-instantiation Weak.drop (ownedI8PtrResultDrop's AsWeak branch — the
// only native-handle drop branch not otherwise reached by these tests).
func TestMatchOwnedWeakResultTrackedForBorrow(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_weak(Weak[int] w) Holder { return Holder(n: 2); }
		run(bool c) {
			a := Ref[int](7);
			b := Ref[int](9);
			r := borrow_weak(match c { true => a.downgrade(), _ => b.downgrade() });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	assertContains(t, fn, "phi i1 [ true,")
	assertContains(t, fn, `call void @"Weak[int].drop"`)
}

// channel arms → owned Channel handle result to a borrow param, freed via the
// per-elem Channel.drop (ownedI8PtrResultDrop's AsChannel branch).
func TestMatchOwnedChannelResultTrackedForBorrow(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_c(channel[int] ch) Holder { return Holder(n: 5); }
		run(bool c) {
			r := borrow_c(match c { true => channel[int](1), _ => channel[int](2) });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	assertContains(t, fn, "phi i1 [ true,")
	assertContains(t, fn, `call void @"Channel[int].drop"`)
}

// Mutex arms → owned Mutex handle result to a borrow param, freed via the
// per-inner Mutex.drop (ownedI8PtrResultDrop's AsMutex branch).
func TestMatchOwnedMutexResultTrackedForBorrow(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_m(Mutex[int] m) Holder { return Holder(n: 1); }
		run(bool c) {
			r := borrow_m(match c { true => Mutex[int](7), _ => Mutex[int](9) });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	assertContains(t, fn, "phi i1 [ true,")
	assertContains(t, fn, `call void @"Mutex[int].drop"`)
}

// MutexGuard arms → owned guard result to a borrow param, freed via MutexGuard.drop
// (ownedI8PtrResultDrop's AsMutexGuard / TypMutexGuard branch). m.lock() yields an
// owned guard temp; the selected guard is released at the caller's statement end.
func TestMatchOwnedMutexGuardResultTrackedForBorrow(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_guard(MutexGuard[int] g) Holder { return Holder(n: g.borrow); }
		run(bool c) {
			m := Mutex[int](7);
			r := borrow_guard(match c { true => m.lock(), _ => m.lock() });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	assertContains(t, fn, "phi i1 [ true,")
	assertContains(t, fn, "call void @MutexGuard.drop")
}

// Task arms (`go compute(...)` → owned Task temp) → owned Task handle result to a
// borrow param, freed via the per-instantiation Task.drop (ownedI8PtrResultDrop's
// AsTask branch). Borrowed (not awaited), so the caller drops the selected task.
func TestMatchOwnedTaskResultTrackedForBorrow(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_task(task[int] t) Holder { return Holder(n: 42); }
		compute(int x) int { return x * 2; }
		run(bool c) {
			r := borrow_task(match c { true => go compute(3), _ => go compute(5) });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	assertContains(t, fn, "phi i1 [ true,")
	assertContains(t, fn, `call void @"Task[int].drop"`)
}

// Recursion coverage: an OUTER match arm whose body is itself a match whose leaf is
// an owned-local ident, evaluated as a `go`-call argument. Under
// suppressMergeResultTemp the inner match phi is NOT registered as a stmt temp, so
// matchArmTransfersOwnership falls through the val fast-path to
// resultTransfersOwnedFlag, which recurses into the nested MatchExpr arm
// (matchArmResultTransfersOwnedFlag → the owned-local IdentExpr). Exercises the
// recursive ownership-detection helper. Must compile without panic. (The
// mixed-ownership runtime variant of this shape was the T1206 leak, now fixed; this
// test only checks the caller-suppression IR mechanic.)
func TestNestedMatchArmOwnedLocalSuppressedGoArg(t *testing.T) {
	ir := generateIR(t, `
		proc(string p) { }
		run(bool a, bool b) {
			local := "hello".clone();
			go proc(match a { true => match b { true => local, _ => "xy".clone() }, _ => "zz".clone() });
		}
	`)
	assertContains(t, ir, "define")
	// Suppressed: no T1107 caller ownership-flag phi despite the owned-local arm.
	fn := extractFunction(ir, "__user.run")
	if strings.Contains(fn, "phi i1") {
		t.Fatalf("expected no T1107 ownership-flag phi under go-arg suppression; got:\n%s", fn)
	}
}

// Recursion coverage for the IfExpr / blockResultTransfersOwnedFlag branches: an
// outer match arm whose body is an `if` whose then-block's last statement is an
// owned-local ident, again under go-arg suppression so the recursive helper (not
// the stmtTempMap fast-path) resolves the ownership bit. (Runtime mixed-ownership
// variant of this shape was the T1206 leak, now fixed; IR-mechanic check only.)
func TestNestedIfArmOwnedLocalSuppressedGoArg(t *testing.T) {
	ir := generateIR(t, `
		proc(string p) { }
		run(bool a, bool b) {
			local := "hello".clone();
			go proc(match a { true => if b { local } else { "x".clone() }, _ => "zz".clone() });
		}
	`)
	assertContains(t, ir, "define")
}

// Recursion coverage for the BLOCK-form arm branch of the ownership-detection
// helper: an outer match arm whose body is a nested match with BLOCK arms (`{ ...
// }`), under go-arg suppression so the inner phi is not stmt-temp-registered and
// matchArmResultTransfersOwnedFlag must descend via blockResultTransfersOwnedFlag
// (arm.Block, last-statement ExprStmt) rather than arm.Body. Must compile; the
// owned-local reached through the block arm is detected without panic.
func TestNestedMatchBlockArmOwnedLocalSuppressedGoArg(t *testing.T) {
	ir := generateIR(t, `
		proc(string p) { }
		run(bool a, bool b) {
			local := "hello".clone();
			go proc(match a { true => match b { true => { local }, _ => { "xy".clone() } }, _ => "zz".clone() });
		}
	`)
	assertContains(t, ir, "define")
}

// A match owned result passed DIRECTLY as a `go`-call argument must NOT be
// registered as a caller statement-end temp (suppressMergeResultTemp): the go-arg
// machinery (T1106) transfers ownership into the goroutine frame, so a caller drop
// would race the goroutine's async read. The caller therefore has NO T1107
// ownership-flag phi — contrast with the synchronous-call variant above, which
// does. Guards the suppression wired in genGoCallExpr.
func TestGoCallMatchArgSuppressesCallerResultTemp(t *testing.T) {
	ir := generateIR(t, `
		proc_str(string p) { }
		run(bool c) {
			go proc_str(match c { true => "abcdef".clone(), _ => "xy".clone() });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	if strings.Contains(fn, "phi i1") {
		t.Fatalf("expected no T1107 ownership-flag phi for a `go`-call match arg (suppressed); got:\n%s", fn)
	}
}

// A BOUND result claims the phi: after the ownership flag phi is stored into the
// temp's flag alloca, the binding neutralizes it (store i1 false) and the variable
// takes ownership (its own drop flag set), so the value is freed exactly once at
// scope exit — no double free with the statement-end temp cleanup.
func TestMatchOwnedResultClaimedWhenBound(t *testing.T) {
	ir := generateIR(t, `
		run(bool c) {
			s := match c { true => "abcdef".clone(), _ => "xy".clone() };
		}
	`)
	fn := extractFunction(ir, "__user.run")
	assertContains(t, fn, "phi i1 [ true,")
	// The bound variable becomes the sole owner (drops at scope exit).
	assertContains(t, fn, "store i1 true, i1* %s.dropflag")
}
