package codegen

import (
	"strings"
	"testing"
)

// T0899: T0892 added the receiver-alias-clear to the variable-target branch of
// genAssignStmt (`m = expr`). The MemberExpr target (`obj.field = expr`) and
// IndexExpr target (`arr[i] = expr`) branches had no alias-clear, so reassigning
// a field/element from an operator/method result that returns the borrowed
// receiver (`return this`) aliased the operand and both the operand local and the
// field/element owner freed the same instance → double-free. These tests assert
// the field/index assignment paths now emit the same operand alias-clear.

// Field target with a binary operator returning `this` (`h.f = a + b`) must emit
// the receiver-alias-clear blocks (origin is the operand `a`).
func TestAssignFieldOperatorBinaryReturnThisEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; +(BB o) BB { return this; } drop(~this){} }
		type Holder { BB f; }
		main() { a := BB(v: 11); b := BB(v: 3); h := Holder(f: BB(v: 0)); h.f = a + b; }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// Field target with a method returning `this` (`h.f = d.dup()`).
func TestAssignFieldMethodReturnThisEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type DD { int v; dup() DD { return this; } drop(~this){} }
		type Holder { DD f; }
		main() { d := DD(v: 7); h := Holder(f: DD(v: 0)); h.f = d.dup(); }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// Index target with a binary operator returning `this` (`v[0] = a + b`).
func TestAssignIndexOperatorBinaryReturnThisEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; +(BB o) BB { return this; } drop(~this){} }
		main() { a := BB(v: 11); b := BB(v: 3); v := [BB(v: 0)]; v[0] = a + b; }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// Index target with a method returning `this` (`v[0] = d.dup()`).
func TestAssignIndexMethodReturnThisEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type DD { int v; dup() DD { return this; } drop(~this){} }
		main() { d := DD(v: 7); v := [DD(v: 0)]; v[0] = d.dup(); }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// Self-alias field (`h.f = h.f + b`): the operand origin is a MemberExpr, not an
// Ident, so the operand-clear must NOT fire. The drop-old guard inside
// genMemberAssign handles the aliasing.
func TestAssignFieldSelfAliasReturnThisSkipsOperandClear(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; +(BB o) BB { return this; } drop(~this){} }
		type Holder { BB f; }
		main() { b := BB(v: 3); h := Holder(f: BB(v: 21)); h.f = h.f + b; }
	`)
	if strings.Contains(ir, "return.this.clear") {
		t.Fatalf("self-alias field assignment must NOT emit return.this.clear (origin is MemberExpr):\n%s", ir)
	}
}

// Self-alias index (`v[0] = v[0] + b`): the operand origin is an IndexExpr, not
// an Ident, so the operand-clear must NOT fire — genVectorIndexAssign's drop-old
// guard handles the aliasing. Symmetric to the field self-alias case above.
func TestAssignIndexSelfAliasReturnThisSkipsOperandClear(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; +(BB o) BB { return this; } drop(~this){} }
		main() { b := BB(v: 3); v := [BB(v: 21)]; v[0] = v[0] + b; }
	`)
	if strings.Contains(ir, "return.this.clear") {
		t.Fatalf("self-alias index assignment must NOT emit return.this.clear (origin is IndexExpr):\n%s", ir)
	}
}

// Generic-function-body field target: the operator-return-this assignment lives
// inside `wrap[T]`, so during monomorphization c.typeSubst is active and the RHS
// type (GB[T]) is substituted before the alias-clear. This is the ONLY path that
// exercises clearOperandAliasForOwnedStore's types.Substitute branch — the other
// generic tests place the assignment in main(), where typeSubst is nil.
func TestAssignFieldOperatorReturnThisInGenericBodyEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type GB[T] { T v; +(GB[T] o) GB[T] { return this; } drop(~this){} }
		type GHolder[T] { GB[T] f; drop(~this){} }
		wrap[T](GHolder[T] move h, T move x, T move y) { a := GB[T](v: x); b := GB[T](v: y); h.f = a + b; }
		main() { h := GHolder[int](f: GB[int](v: 0)); wrap[int](h, 5, 9); }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// Generic-function-body index target: same as above but `v[0] = a + b` inside a
// generic body, exercising the IndexExpr branch under an active typeSubst.
func TestAssignIndexOperatorReturnThisInGenericBodyEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type GB[T] { T v; +(GB[T] o) GB[T] { return this; } drop(~this){} }
		wrap[T](T move x, T move y, T move z) { a := GB[T](v: x); b := GB[T](v: y); v := [GB[T](v: z)]; v[0] = a + b; }
		main() { wrap[int](5, 9, 1); }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}
