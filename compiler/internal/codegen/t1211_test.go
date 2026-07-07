package codegen

import (
	"regexp"
	"testing"
)

// T1211: residual gap in the T1209 bug class. T1209 fixed the bound-to-local
// double-free for i8*-shaped owned results (string/Vector), but trackMergeResultTemp
// early-returned for any non-i8* phi — so VALUE-STRUCT merge results (a heap user type
// with `drop`, and `Map[K,V]`) never got a per-path ownership flag. captureLiveTempFlag
// then returned nil, applyBoundMergeFlag was a no-op, and the binding's drop flag stayed
// UNCONDITIONALLY armed (`store i1 true, i1* %s.dropflag`), so on the borrowed arm the
// bound local dropped a caller-owned value → double-free (segfault).
//
// trackMergeResultStructFlag now records a per-path i1 flag phi ([owned:1, borrowed:0])
// for these non-i8* merges in a parallel alloca (mergeBoundStructFlag); captureLiveTempFlag
// reads it and applyBoundMergeFlag stores it into the binding's drop flag.
//
// IR signature (same as T1209): the binding's drop flag is finally set from a loaded SSA
// value (`store i1 %NN, i1* %s.dropflag`), NOT left at the unconditional `store i1 true`,
// and a per-path ownership phi (`phi i1 [ true, … ], [ false, … ]`) feeds it.

var t1211FixedFlag = regexp.MustCompile(`store i1 %\d+, i1\* %s\.dropflag`)
var t1211PerPathPhi = regexp.MustCompile(`phi i1 \[ true, %.*\], \[ false, %`)

// Heap user type (`{i8*,i8*}` value struct with drop), inferred `:=`, outer IF,
// borrowed `else` arm (the exact repro).
func TestBoundMixedBorrowHeapUserTypeIfPerPathFlag(t *testing.T) {
	ir := generateIR(t, `
		type Res { string tag; drop(~this) {} }
		consume(bool b, Res param) {
			s := if b { Res(tag: "own".clone()) } else { param };
			foo(s.tag.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if !t1211PerPathPhi.MatchString(fn) {
		t.Fatalf("expected a per-path ownership flag phi [true/false] for the heap-user-type merge:\n%s", fn)
	}
	if !t1211FixedFlag.MatchString(fn) {
		t.Fatalf("expected the binding's drop flag to be set from the per-path flag (loaded SSA value), not left unconditionally armed:\n%s", fn)
	}
}

// Heap user type, outer MATCH, borrowed `_` arm.
func TestBoundMixedBorrowHeapUserTypeMatchPerPathFlag(t *testing.T) {
	ir := generateIR(t, `
		type Res { string tag; drop(~this) {} }
		consume(int k, Res param) {
			s := match k { 1 => Res(tag: "own".clone()), _ => param };
			foo(s.tag.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if !t1211FixedFlag.MatchString(fn) {
		t.Fatalf("expected the binding's drop flag to be set from the per-path flag (loaded SSA value):\n%s", fn)
	}
}

// Map[K,V], inferred `:=`, outer IF, borrowed `else` arm — the `.clone()` owned arm is a
// call-result value struct (field 1 tracked as extractvalue), exercising the
// resultIsFreshOwnedHeapTemp call-result branch.
func TestBoundMixedBorrowMapIfPerPathFlag(t *testing.T) {
	ir := generateIR(t, `
		consume(bool b, map[int, int] param) {
			s := if b { {1: 2}.clone() } else { param };
			foo(s.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if !t1211PerPathPhi.MatchString(fn) {
		t.Fatalf("expected a per-path ownership flag phi [true/false] for the Map merge:\n%s", fn)
	}
	if !t1211FixedFlag.MatchString(fn) {
		t.Fatalf("expected the binding's drop flag to be set from the per-path flag (loaded SSA value):\n%s", fn)
	}
}

// Outer-nested: outer MATCH selecting an arm whose body is a nested MIXED heap-user-type
// conditional, bound to a local. The composed per-path flag (threaded through two merge
// levels) must reach the binding's drop flag — a constant would drop the borrowed value.
func TestBoundOuterNestedMixedBorrowHeapUserTypePerPathFlag(t *testing.T) {
	ir := generateIR(t, `
		type Res { string tag; drop(~this) {} }
		consume(bool a, bool b, Res param) {
			s := match a { true => if b { Res(tag: "own".clone()) } else { param }, _ => Res(tag: "zz".clone()) };
			foo(s.tag.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if !t1211FixedFlag.MatchString(fn) {
		t.Fatalf("expected the binding's drop flag to be set from the composed per-path flag (loaded SSA value):\n%s", fn)
	}
}

// Recursive `else if`: an inner value-if (statement position) with an else-if chain —
// fresh owned Res middle arm, borrowed param final arm — nested inside an outer
// if-expression's block, bound to a local. The composed per-path flag must reach the
// binding's drop flag (genIfStmtValue's recursive `case *ast.IfStmt` threads the inner
// merge's mergeBoundStructFlag up); a constant would leak the owned middle arm or
// double-free the borrowed final arm.
func TestBoundNestedElseIfHeapUserTypePerPathFlag(t *testing.T) {
	ir := generateIR(t, `
		type Res { string tag; drop(~this) {} }
		consume(bool outer, int k, Res param) {
			s := if outer {
				if k == 0 { Res(tag: "zero".clone()) } else if k == 1 { Res(tag: "one".clone()) } else { param }
			} else {
				Res(tag: "outer".clone())
			};
			foo(s.tag.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if !t1211FixedFlag.MatchString(fn) {
		t.Fatalf("expected the binding's drop flag to be set from the composed per-path flag (loaded SSA value):\n%s", fn)
	}
}

// Both-arms-owned regression: no borrowed arm → the merge is unconditionally owned, so
// the per-path override must not fire (anyOwned gate builds the flag, but every incoming
// is `true`, so the binding still drops exactly once). Guards against a spurious borrowed
// path — the drop flag phi (if present) must never carry a `false` incoming here.
func TestBoundBothOwnedHeapUserTypeNoBorrowedPath(t *testing.T) {
	ir := generateIR(t, `
		type Res { string tag; drop(~this) {} }
		consume(bool b) {
			local := Res(tag: "loc".clone());
			s := if b { local } else { Res(tag: "w".clone()) };
			foo(s.tag.len);
		}
		foo(int n) {}
	`)
	fn := extractFunction(ir, "__user.consume")
	if t1211PerPathPhi.MatchString(fn) {
		t.Fatalf("both-owned merge must not build a mixed [true/false] per-path phi (no borrowed arm):\n%s", fn)
	}
}
