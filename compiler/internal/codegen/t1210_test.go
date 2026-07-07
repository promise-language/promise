package codegen

import (
	"regexp"
	"testing"
)

// T1210: a mixed owned/borrowed match/if result (one owned arm — clone()/owned-local
// — and one BORROWED arm — a borrowed param/field) bound to a TYPED Optional local
// (`string? s = …`) is Optional-wrapped into the binding via maybeRegisterOptionalDrop,
// which arms the wrapped Optional's inner drop UNCONDITIONALLY (`store i1 true, i1*
// %s.dropflag`). On the borrowed path this frees the caller-owned value → double-free.
// This is the Optional-wrap sibling of T1209 (T1209's `!willWrap` gate deliberately
// skips this shape). The fix captures the merge temp's live per-path flag BEFORE the
// T0111 pre-wrap claim / wrapOptional neutralize it, then re-applies it to the wrapped
// Optional's drop flag AFTER maybeRegisterOptionalDrop (applyBoundMergeFlag).
//
// IR signature: the binding's drop flag is finally set from a loaded SSA value
// (`store i1 %NN, i1* %s.dropflag`), NOT left at the unconditional `store i1 true`.

var t1210FixedFlag = regexp.MustCompile(`store i1 %\d+, i1\* %s\.dropflag`)
var t1210PerPathPhi = regexp.MustCompile(`phi i1 \[ true, %.*\], \[ false, %`)

// Typed Optional decl, outer IF, borrowed `else` arm (the exact repro).
func TestOptWrapMixedBorrowIfPerPathFlag(t *testing.T) {
	ir := generateIR(t, `
		consume(string param) {
			b := false;
			string? s = if b { "x".clone() } else { param };
			foo(s!.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if !t1210PerPathPhi.MatchString(fn) {
		t.Fatalf("expected a per-path ownership flag phi [true/false]:\n%s", fn)
	}
	if !t1210FixedFlag.MatchString(fn) {
		t.Fatalf("expected the wrapped Optional's drop flag to be set from the per-path flag (loaded SSA value), not left unconditionally armed:\n%s", fn)
	}
	// The owned inner path still drops its clone exactly once.
	assertContains(t, fn, "call void @promise_string_drop")
}

// Typed Optional decl, outer MATCH, borrowed `_` arm.
func TestOptWrapMixedBorrowMatchPerPathFlag(t *testing.T) {
	ir := generateIR(t, `
		consume(int k, string param) {
			string? s = match k { 1 => "x".clone(), _ => param };
			foo(s!.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if !t1210FixedFlag.MatchString(fn) {
		t.Fatalf("expected the wrapped Optional's drop flag to be set from the per-path flag (loaded SSA value):\n%s", fn)
	}
	assertContains(t, fn, "call void @promise_string_drop")
}

// Outer-nested: outer MATCH selecting an arm whose body is a nested MIXED conditional,
// bound to a typed Optional local. The composed per-path flag (threaded through two
// merge levels by T1208) must reach the wrapped Optional's drop flag.
func TestOptWrapOuterNestedMixedBorrowPerPathFlag(t *testing.T) {
	ir := generateIR(t, `
		consume(bool a, bool b, string param) {
			string? s = match a { true => if b { "x".clone() } else { param }, _ => "zz".clone() };
			foo(s!.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if !t1210FixedFlag.MatchString(fn) {
		t.Fatalf("expected the wrapped Optional's drop flag to be set from the composed per-path flag (loaded SSA value):\n%s", fn)
	}
	assertContains(t, fn, "call void @promise_string_drop")
}

// Paren-wrapped RHS: isMixedMergeBindingRHS must unwrap the ParenExpr before the
// wrap-path capture (line ~1286) too, so `string? s = (if …)` still threads the merge
// temp's per-path flag into the wrapped Optional's drop flag. Without the unwrap the
// ParenExpr would fail the IfExpr/MatchExpr switch, mergeWrapFlag would stay nil, and
// the wrapped Optional's inner drop would be left unconditionally armed → double-free
// on the borrowed path.
func TestOptWrapParenMixedBorrowPerPathFlag(t *testing.T) {
	ir := generateIR(t, `
		consume(bool b, string param) {
			string? s = (if b { "x".clone() } else { param });
			foo(s!.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if !t1210FixedFlag.MatchString(fn) {
		t.Fatalf("paren-wrapped mixed merge into a typed Optional must still thread the per-path flag into the drop flag (loaded SSA value):\n%s", fn)
	}
	assertContains(t, fn, "call void @promise_string_drop")
}

// Negative guard (wrap-path, ErrorHandlerExpr): a member optional handler
// (`owner.field? _ { recovery }`) bound to a typed Optional local MOVES the present
// value out of the owner's field, so the wrapped binding owns it unconditionally.
// isMixedMergeBindingRHS excludes ErrorHandlerExpr, so mergeWrapFlag stays nil and the
// T1210 override is a no-op — the wrapped Optional's drop stays unconditionally armed.
// If the stale present=borrowed per-path flag were applied here it would suppress the
// sole drop and leak (mirror of the T1209 TestBoundMemberOptionalHandler guard, but on
// the Optional-wrap path this bug is about).
func TestOptWrapMemberOptionalHandlerKeepsUnconditionalDrop(t *testing.T) {
	ir := generateIR(t, `
		type Holder { string? f; }
		consume(Holder h) {
			string? s = h.f? _ { "recovered".clone() };
			foo(s!.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if t1210FixedFlag.MatchString(fn) {
		t.Fatalf("member optional handler wrapped into a typed Optional must NOT receive a per-path drop-flag override (would leak — T1162 interaction on the wrap path):\n%s", fn)
	}
	assertContains(t, fn, "store i1 true, i1* %s.dropflag")
}

// Negative guard: a plain owned RHS (`"lit".clone()`, not a match/if) is NOT a mixed
// merge binding — isMixedMergeBindingRHS returns false, so mergeWrapFlag stays nil and
// applyBoundMergeFlag is a no-op. The wrapped Optional owns its value unconditionally
// and its drop stays armed: the unconditional `store i1 true, i1* %s.dropflag` survives,
// never overwritten by a loaded per-path SSA value. This confirms the fix does not leak
// dead flag loads/stores into unrelated Optional-wrap shapes.
func TestOptWrapPlainCloneKeepsUnconditionalDrop(t *testing.T) {
	ir := generateIR(t, `
		consume() {
			string? s = "lit".clone();
			foo(s!.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if t1210FixedFlag.MatchString(fn) {
		t.Fatalf("plain owned-clone Optional binding must NOT receive a per-path drop-flag override (unconditional owning drop is correct):\n%s", fn)
	}
	assertContains(t, fn, "store i1 true, i1* %s.dropflag")
}
