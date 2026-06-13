package codegen

import (
	"strings"
	"testing"
)

// T0892: The B0250/T0341/T0882 receiver-alias-clear lived only in the two var-decl
// paths (genTypedVarDecl / genInferredVarDecl). The assignment path genAssignStmt
// had no alias-clear, so reassigning an existing variable from an operator/method
// result that returns the borrowed receiver (`return this`) aliased the operand and
// both bindings freed the same instance at scope exit → double-free. These tests
// assert the assignment path now emits the same alias-clear the var-decl paths do.

// Binary operator on the assignment path (`m = a + b`) with a distinct Ident left
// operand must emit the receiver-alias-clear blocks (origin is the operand `a`).
func TestAssignOperatorBinaryReturnThisEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; +(BB o) BB { return this; } }
		main() { a := BB(v: 11); b := BB(v: 3); m := BB(v: 0); m = a + b; }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// Method call on the assignment path (`m = d.dup()` where dup is `return this`)
// must emit the alias-clear too — proving the pre-existing method-level gap (which
// predates and is independent of operators) is closed.
func TestAssignMethodReturnThisEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type DD { int v; dup() DD { return this; } }
		main() { d := DD(v: 7); m := DD(v: 0); m = d.dup(); }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// Self-alias assignment (`m = m + b` where + returns `this`) must guard the
// old-value drop at runtime (reassign.self.diff / reassign.self.merge) and must NOT
// emit the operand-clear — clearing the target's own flag post-store would leak.
func TestAssignSelfAliasReturnThisGuardsDropOld(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; +(BB o) BB { return this; } }
		main() { b := BB(v: 3); m := BB(v: 0); m = m + b; }
	`)
	assertContains(t, ir, "reassign.self.diff")
	assertContains(t, ir, "reassign.self.merge")
	if strings.Contains(ir, "return.this.clear") {
		t.Fatalf("self-alias assignment must NOT emit return.this.clear (would clear target's own flag → leak):\n%s", ir)
	}
}

// A chain rooted at `this` on the assignment path (`m = this.dup()` inside a method,
// dup returns `this`) must clear the new binding's drop flag against `this`
// (ThisExpr origin → this.alias.clear block).
func TestAssignMethodThisOriginReturnThisEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type DD { int v; dup() DD { return this; } refresh() { m := DD(v: 0); m = this.dup(); } }
		main() { d := DD(v: 7); d.refresh(); }
	`)
	assertContains(t, ir, "this.alias.clear")
	assertContains(t, ir, "this.alias.skip")
}

// Negative: a structural-typed target reassigned from a structural-default operator
// returning `this` must NOT emit the alias-clear — the structural binding never
// takes an owning drop, so clearing the operand's flag would leak the shared
// instance. Confirms the structural-target guard holds on the assignment path.
func TestAssignOperatorReturnThisStructuralSkipsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type Negatable `+"`structural"+` {
			int v;
			-() Negatable { return this; }
		}
		type Item is Negatable { int v; }
		main() { it := Item(v: 5); m := -it; m = -it; }
	`)
	if strings.Contains(ir, "return.this.clear") {
		t.Fatalf("structural-target reassignment must NOT emit return.this.clear (would leak):\n%s", ir)
	}
}
