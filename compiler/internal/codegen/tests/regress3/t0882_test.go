package regress3

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T0882: A user-defined operator method whose body is `return this` returns the
// borrowed receiver as an owned result, aliasing the operand. Operator dispatch
// has RHS BinaryExpr/UnaryExpr (not CallExpr), so the B0250/T0341 receiver-alias-
// clear was bypassed and both the operand binding and the result binding freed the
// same instance → double-free. operatorReceiverOrigin reaches the same alias-clear.

// Binary operator (`m := a + b`) with an Ident left operand must emit the
// receiver-alias-clear blocks (origin is the left operand `a`).
func TestOperatorBinaryReturnThisEmitsAliasClear(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type BB { int v; +(BB o) BB { return this; } }
		main() { a := BB(v: 11); b := BB(v: 3); m := a + b; }
	`)
	codegentest.AssertContains(t, ir, "return.this.clear")
	codegentest.AssertContains(t, ir, "return.this.skip")
}

// Binary operator with an explicit (typed) declaration takes the same path.
func TestOperatorBinaryReturnThisTypedEmitsAliasClear(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type BB { int v; +(BB o) BB { return this; } }
		main() { a := BB(v: 11); b := BB(v: 3); BB m = a + b; }
	`)
	codegentest.AssertContains(t, ir, "return.this.clear")
	codegentest.AssertContains(t, ir, "return.this.skip")
}

// Prefix unary operator (`m := -d`) with an Ident operand must emit the
// receiver-alias-clear blocks (origin is the operand `d`). Requires T0878.
func TestOperatorUnaryReturnThisEmitsAliasClear(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type NB { int v; -() NB { return this; } }
		main() { d := NB(v: 11); m := -d; }
	`)
	codegentest.AssertContains(t, ir, "return.this.clear")
	codegentest.AssertContains(t, ir, "return.this.skip")
}

// An operator invoked with `this` as its left operand inside a method body, whose
// result is bound to a local, must defer the alias-clear to the new binding's drop
// flag (ThisExpr origin → pendingThisAliasClear → alias.clear block).
func TestOperatorBinaryReturnThisFromMethodEmitsAliasClear(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type BB { int v; +(BB o) BB { return this; } combine(BB o) BB { m := this + o; return m; } }
		main() { a := BB(v: 11); b := BB(v: 3); r := a.combine(b); }
	`)
	codegentest.AssertContains(t, ir, "alias.clear")
	codegentest.AssertContains(t, ir, "alias.skip")
}

// Negative case for the isStructuralTarget guard in genInferredVarDecl: when a
// structural default operator `-() Negatable { return this; }` is synthesized onto
// a concrete type and the result is inferred to the structural interface type, the
// result binding never takes an owning drop, so the alias-clear MUST be skipped —
// clearing the operand's drop flag would leak the shared instance. Confirms the
// receiver-alias-clear blocks are NOT emitted on the structural path.
func TestOperatorUnaryReturnThisStructuralSkipsAliasClear(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Negatable `+"`structural"+` {
			int v;
			-() Negatable { return this; }
		}
		type Item is Negatable { int v; }
		main() { it := Item(v: 5); m := -it; }
	`)
	if strings.Contains(ir, "return.this.clear") {
		t.Fatalf("structural operator return-this must NOT emit return.this.clear (would leak):\n%s", ir)
	}
}
