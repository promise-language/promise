package codegen

import (
	"regexp"
	"testing"
)

// T1208: an OUTER match/if selecting an arm whose body is a nested MIXED
// owned/borrowed conditional (one owned inner arm — clone()/owned-local — and one
// BORROWED inner arm — a borrowed param/field) must thread the inner conditional's
// genuine PER-PATH ownership flag into the outer merge's flag phi. Before the fix
// the outer arm used a whole-arm CONSTANT `true` (the nested phi is a tracked owned
// temp), so on the borrowed inner path the outer statement-end cleanup freed the
// borrowed value — a use-after-free plus a double-free at the real owner's scope
// end. The fix (captureLiveTempFlag) loads the inner temp's live per-path flag phi
// in the arm block — BEFORE claimStringTemp neutralizes it — and threads it through
// matchArmInfo.ownedFlag into trackMergeResultTemp's flag-phi incoming.
//
// IR signature: the outer merge flag phi's nested-arm incoming is a loaded SSA
// value from the inner conditional's merge block (`phi i1 [ %NN, %if.merge.N ]`),
// NOT a constant (`phi i1 [ true, %if.merge.N ]`, the pre-fix bug shape).

var t1208BuggyFlag = regexp.MustCompile(`phi i1 \[ true, %(if\.merge|if\.end)\.`)
var t1208FixedFlag = regexp.MustCompile(`phi i1 \[ %\d+, %(if\.merge|if\.end)\.`)

// Outer MATCH, inner IF: the mixed inner conditional's borrowed arm (`param`) must
// not be dropped by the outer merge cleanup.
func TestNestedMixedBorrowMatchIfNoConstantFlag(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_len(string p) Holder { return Holder(n: p.len); }
		run(bool a, bool b, string param) {
			r := borrow_len(match a { true => if b { "x".clone() } else { param }, _ => "zz".clone() });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	if t1208BuggyFlag.MatchString(fn) {
		t.Fatalf("nested-arm flag incoming is a constant (pre-fix bug: borrowed value would be freed):\n%s", fn)
	}
	if !t1208FixedFlag.MatchString(fn) {
		t.Fatalf("expected outer flag phi to thread the inner per-path flag (a loaded SSA value):\n%s", fn)
	}
	// The owned inner path still drops its clone exactly once.
	assertContains(t, fn, "call void @promise_string_drop")
}

// Outer IF, inner IF: same shape via the genIfExpr / genIfStmtValue path.
func TestNestedMixedBorrowIfIfNoConstantFlag(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_len(string p) Holder { return Holder(n: p.len); }
		run(bool b, string param) {
			r := borrow_len(if true { if b { "x".clone() } else { param } } else { "zz".clone() });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	if t1208BuggyFlag.MatchString(fn) {
		t.Fatalf("nested-arm flag incoming is a constant (pre-fix bug: borrowed value would be freed):\n%s", fn)
	}
	if !t1208FixedFlag.MatchString(fn) {
		t.Fatalf("expected outer flag phi to thread the inner per-path flag (a loaded SSA value):\n%s", fn)
	}
	assertContains(t, fn, "call void @promise_string_drop")
}

// Outer REAL ENUM match (genEnumMatch, a distinct code path from the bool-subject
// genValueMatch used by the tests above), inner mixed IF. The enum arm is a block
// whose result is the nested if temp; genBlockValue must thread its per-path flag
// through matchArmInfo.ownedFlag so the borrowed inner path is not freed by the outer
// merge cleanup.
func TestNestedMixedBorrowEnumOuterNoConstantFlag(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		enum Choice { A, B }
		borrow_len(string p) Holder { return Holder(n: p.len); }
		run(bool b, string param, Choice c) {
			r := borrow_len(match c { A => if b { "x".clone() } else { param }, B => "zz".clone() });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	if t1208BuggyFlag.MatchString(fn) {
		t.Fatalf("enum-outer nested-arm flag incoming is a constant (pre-fix bug: borrowed value would be freed):\n%s", fn)
	}
	if !t1208FixedFlag.MatchString(fn) {
		t.Fatalf("expected outer flag phi to thread the inner per-path flag (a loaded SSA value):\n%s", fn)
	}
	assertContains(t, fn, "call void @promise_string_drop")
}

// DEPTH-3 nesting: outer match -> middle if -> inner match, borrowed innermost arm.
// The per-path flag must thread through TWO merge levels. A constant flag at either
// level would drop the borrowed value; the fixed IR has NO constant nested-arm flag
// incoming (neither `if.merge`/`if.end` nor `match.end`).
func TestNestedMixedBorrowDepth3NoConstantFlag(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_len(string p) Holder { return Holder(n: p.len); }
		run(bool a, bool b, bool d, string param) {
			r := borrow_len(match a {
				true => if b { match d { true => "q".clone(), _ => param } } else { "yy".clone() },
				_ => "zz".clone(),
			});
		}
	`)
	fn := extractFunction(ir, "__user.run")
	buggy := regexp.MustCompile(`phi i1 \[ true, %(if\.merge|if\.end|match\.end)\.`)
	if buggy.MatchString(fn) {
		t.Fatalf("depth-3 nested-arm flag incoming is a constant (pre-fix bug: borrowed value would be freed):\n%s", fn)
	}
	assertContains(t, fn, "call void @promise_string_drop")
}

// Outer MATCH, inner MATCH: the inner mixed conditional is itself a match.
func TestNestedMixedBorrowMatchMatchNoConstantFlag(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_len(string p) Holder { return Holder(n: p.len); }
		run(bool a, bool b, string param) {
			r := borrow_len(match a { true => match b { true => "x".clone(), _ => param }, _ => "zz".clone() });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	// The inner match's merge is a `match.end` block; its flag phi is threaded into
	// the outer flag phi as a loaded SSA value rather than a constant.
	buggy := regexp.MustCompile(`phi i1 \[ true, %match\.end\.`)
	if buggy.MatchString(fn) {
		t.Fatalf("nested-arm flag incoming is a constant (pre-fix bug: borrowed value would be freed):\n%s", fn)
	}
	fixed := regexp.MustCompile(`phi i1 \[ %\d+, %match\.end\.`)
	if !fixed.MatchString(fn) {
		t.Fatalf("expected outer flag phi to thread the inner per-path flag (a loaded SSA value):\n%s", fn)
	}
	assertContains(t, fn, "call void @promise_string_drop")
}
