package codegen

import (
	"regexp"
	"testing"
)

// T1209: a mixed owned/borrowed match/if result (one owned arm — clone()/owned-local
// — and one BORROWED arm — a borrowed param/field) BOUND to a local (`s := …`) owns
// its value only on the paths that selected an owned arm. trackMergeResultTemp builds
// a correct PER-PATH ownership flag phi (`[owned:1, borrowed:0]`), but when that phi
// is claimed by the binding, maybeRegisterDrop arms `s`'s drop flag UNCONDITIONALLY
// (`store i1 true, i1* %s.dropflag`), so on the borrowed path `s`'s scope-end drop
// frees the caller-owned value — a double-free / use-after-free. The fix
// (captureLiveTempFlag + applyBoundMergeFlag) loads the merge temp's live per-path
// flag BEFORE claimStringTemp neutralizes it and re-stores it into the binding's drop
// flag, overriding the unconditional owning drop.
//
// IR signature: the binding's drop flag is finally set from a loaded SSA value
// (`store i1 %NN, i1* %s.dropflag`), NOT left at the unconditional `store i1 true`.

var t1209FixedFlag = regexp.MustCompile(`store i1 %\d+, i1\* %s\.dropflag`)
var t1209PerPathPhi = regexp.MustCompile(`phi i1 \[ true, %.*\], \[ false, %`)

// Inferred `:=` binding, outer IF, borrowed `else` arm (the exact repro).
func TestBoundMixedBorrowIfPerPathFlag(t *testing.T) {
	ir := generateIR(t, `
		consume(string param) {
			b := false;
			s := if b { "x".clone() } else { param };
			foo(s.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if !t1209PerPathPhi.MatchString(fn) {
		t.Fatalf("expected a per-path ownership flag phi [true/false]:\n%s", fn)
	}
	if !t1209FixedFlag.MatchString(fn) {
		t.Fatalf("expected the binding's drop flag to be set from the per-path flag (loaded SSA value), not left unconditionally armed:\n%s", fn)
	}
	// The owned inner path still drops its clone exactly once.
	assertContains(t, fn, "call void @promise_string_drop")
}

// Paren-wrapped RHS: isMixedMergeBindingRHS must unwrap the ParenExpr and still
// thread the per-path flag into the binding's drop flag (a bare `(...)` wrapper is
// not a distinct construct and must not hide the mixed merge temp).
func TestBoundMixedBorrowParenPerPathFlag(t *testing.T) {
	ir := generateIR(t, `
		consume(bool b, string param) {
			s := (if b { "x".clone() } else { param });
			foo(s.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if !t1209FixedFlag.MatchString(fn) {
		t.Fatalf("expected the paren-wrapped binding's drop flag to be set from the per-path flag (loaded SSA value):\n%s", fn)
	}
	assertContains(t, fn, "call void @promise_string_drop")
}

// Inferred `:=` binding, outer MATCH, borrowed `_` arm.
func TestBoundMixedBorrowMatchPerPathFlag(t *testing.T) {
	ir := generateIR(t, `
		consume(int k, string param) {
			s := match k { 1 => "x".clone(), _ => param };
			foo(s.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if !t1209FixedFlag.MatchString(fn) {
		t.Fatalf("expected the binding's drop flag to be set from the per-path flag (loaded SSA value):\n%s", fn)
	}
	assertContains(t, fn, "call void @promise_string_drop")
}

// Outer-nested: outer MATCH selecting an arm whose body is a nested MIXED conditional,
// bound to a local. The composed per-path flag (threaded through two merge levels by
// T1208) must reach the binding's drop flag — a constant would drop the borrowed value.
func TestBoundOuterNestedMixedBorrowPerPathFlag(t *testing.T) {
	ir := generateIR(t, `
		consume(bool a, bool b, string param) {
			s := match a { true => if b { "x".clone() } else { param }, _ => "zz".clone() };
			foo(s.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if !t1209FixedFlag.MatchString(fn) {
		t.Fatalf("expected the binding's drop flag to be set from the composed per-path flag (loaded SSA value):\n%s", fn)
	}
	assertContains(t, fn, "call void @promise_string_drop")
}

// Regression guard for the T1162 interaction: a member optional handler
// (`owner.field? _ { recovery }`, an ErrorHandlerExpr) ALSO registers a per-path
// flag temp, but its binding MOVES the present value out of the owner's field, so
// the binding owns it unconditionally. The T1209 per-path override must NOT apply
// here (isMixedMergeBindingRHS excludes ErrorHandlerExpr) — otherwise the stale
// present=borrowed flag suppresses the sole drop and leaks. The binding's drop flag
// must stay unconditionally armed (`store i1 true`), never overwritten by a loaded
// per-path SSA value.
func TestBoundMemberOptionalHandlerKeepsUnconditionalDrop(t *testing.T) {
	ir := generateIR(t, `
		type MtxBox { Mutex[string]? f; }
		consume(MtxBox b) {
			m := b.f? _ { Mutex[string]("recovered") };
			g := m.lock();
			g.close();
		}
	`)
	fn := extractFunction(ir, "__user.consume")
	overridden := regexp.MustCompile(`store i1 %\d+, i1\* %m\.dropflag`)
	if overridden.MatchString(fn) {
		t.Fatalf("member optional handler binding must NOT receive a per-path drop-flag override (would leak — T1162 regression):\n%s", fn)
	}
	assertContains(t, fn, "store i1 true, i1* %m.dropflag")
}
