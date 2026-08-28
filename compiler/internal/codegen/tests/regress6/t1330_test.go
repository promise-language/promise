package regress6

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1330: an `if`-expression in value position with one arm diverging
// (return/raise) previously returned nil from genIfExpr, producing either a
// "nil value for typed var decl" panic (direct value position) or a nil-callee
// malformed IR / SIGSEGV during module print (operand / call-argument position).
// It must now lower to a single-incoming phi, mirroring genIfStmtValue /
// buildMatchPhi.

func TestT1330IfDivergeVarDecl(t *testing.T) {
	// Direct var-decl value position, then arm diverges via return.
	ir := codegentest.GenerateIR(t, `
		h!(bool b) int { int r = if b { return 999 } else { 100 }; return r; }
		main() {}
	`)
	// A phi must be emitted for the non-diverging arm's value.
	if !strings.Contains(ir, "phi i64") {
		t.Errorf("expected a phi for the non-diverging arm, got:\n%s", ir)
	}
}

func TestT1330IfDivergeElseArm(t *testing.T) {
	// Else arm diverges — the then arm supplies the single phi incoming.
	ir := codegentest.GenerateIR(t, `
		h!(bool b) int { int r = if b { 100 } else { return 999 }; return r; }
		main() {}
	`)
	if !strings.Contains(ir, "phi i64") {
		t.Errorf("expected a phi for the non-diverging arm, got:\n%s", ir)
	}
}

func TestT1330IfDivergeOperand(t *testing.T) {
	// Operand position (`1 + if ...`) — used to emit a nil-callee call that
	// crashed during module print. Reaching Module.String() without panic here
	// (generateIR prints the module) is the assertion.
	ir := codegentest.GenerateIR(t, `
		h!(bool b) int { int r = 1 + if b { return 999 } else { 100 }; return r; }
		main() {}
	`)
	if !strings.Contains(ir, "phi i64") {
		t.Errorf("expected a phi for the non-diverging arm, got:\n%s", ir)
	}
}

func TestT1330IfDivergeCallArg(t *testing.T) {
	// Call-argument position (`combine(5, if ...)`) — the other nil-callee shape.
	ir := codegentest.GenerateIR(t, `
		combine(int m, int n) int { return m + n; }
		h!(bool b) int { int r = combine(5, if b { return 999 } else { 100 }); return r; }
		main() {}
	`)
	if !strings.Contains(ir, "phi i64") {
		t.Errorf("expected a phi for the non-diverging arm, got:\n%s", ir)
	}
}

func TestT1330IfDivergeRaise(t *testing.T) {
	// `raise` divergence must be handled identically to `return`.
	ir := codegentest.GenerateIR(t, `
		h!(bool b) int { int r = if b { raise error(message: "x") } else { 100 }; return r; }
		main() {}
	`)
	if !strings.Contains(ir, "phi i64") {
		t.Errorf("expected a phi for the non-diverging arm, got:\n%s", ir)
	}
}
