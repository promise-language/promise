package regress4

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1332: an `if`/`match` expression in value position where BOTH arms diverge
// (return/raise) previously produced zero phi incomings, so genIfExpr /
// buildMatchPhi returned nil — crashing the enclosing consumer with a
// "nil value for typed var decl" panic (var-decl / return position) or malformed
// nil-callee IR (operand / call-argument position). The merge block is genuinely
// unreachable; codegen now terminates it with `unreachable` and hands the
// consumer a typed poison value. generateIR prints the module, which validates
// well-formed IR — reaching that point without panicking is a core assertion.

func TestT1332IfBothArmsDivergeVarDecl(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		h(bool b) int { int r = if b { return 1 } else { return 2 }; return r; }
		main() {}
	`)
	body := codegentest.FuncBody(t, ir, "h")
	if !strings.Contains(body, "unreachable") {
		t.Errorf("expected an unreachable terminator for the dead merge in @h, got:\n%s", body)
	}
	// No phi in @h — both arms diverge, so the merge has no incoming value.
	if strings.Contains(body, "phi ") {
		t.Errorf("did not expect a phi for a fully-diverging if, got:\n%s", body)
	}
}

func TestT1332MatchBothArmsDivergeVarDecl(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		h(bool b) int { int r = match b { true => { return 1 }, _ => { return 2 } }; return r; }
		main() {}
	`)
	body := codegentest.FuncBody(t, ir, "h")
	if !strings.Contains(body, "unreachable") {
		t.Errorf("expected an unreachable terminator for the dead merge in @h, got:\n%s", body)
	}
	if strings.Contains(body, "phi ") {
		t.Errorf("did not expect a phi for a fully-diverging match, got:\n%s", body)
	}
}

func TestT1332IfBothArmsDivergeReturn(t *testing.T) {
	// Return position: `return if ...` — no panic reaching module print is the check.
	codegentest.GenerateIR(t, `
		h(bool b) int { return if b { return 1 } else { return 2 }; }
		main() {}
	`)
}

func TestT1332MatchBothArmsDivergeReturn(t *testing.T) {
	codegentest.GenerateIR(t, `
		h(bool b) int { return match b { true => { return 1 }, _ => { return 2 } }; }
		main() {}
	`)
}

func TestT1332IfBothArmsDivergeOperand(t *testing.T) {
	// Operand position (`10 + if ...`) — used to emit a nil-callee crash.
	codegentest.GenerateIR(t, `
		h(bool b) int { int r = 10 + if b { return 1 } else { return 2 }; return r; }
		main() {}
	`)
}

func TestT1332MatchBothArmsDivergeOperand(t *testing.T) {
	codegentest.GenerateIR(t, `
		h(bool b) int { int r = 10 + match b { true => { return 1 }, _ => { return 2 } }; return r; }
		main() {}
	`)
}

func TestT1332IfBothArmsDivergeCallArg(t *testing.T) {
	// Call-argument position (`combine(5, if ...)`).
	codegentest.GenerateIR(t, `
		combine(int m, int n) int { return m + n; }
		h(bool b) int { int r = combine(5, if b { return 1 } else { return 2 }); return r; }
		main() {}
	`)
}

func TestT1332MatchBothArmsDivergeCallArg(t *testing.T) {
	codegentest.GenerateIR(t, `
		combine(int m, int n) int { return m + n; }
		h(bool b) int { int r = combine(5, match b { true => { return 1 }, _ => { return 2 } }); return r; }
		main() {}
	`)
}

func TestT1332IfBothArmsDivergeRaise(t *testing.T) {
	// `raise` divergence in both arms.
	ir := codegentest.GenerateIR(t, `
		h!(bool b) int { int r = if b { raise error(message: "a") } else { raise error(message: "b") }; return r; }
		main() {}
	`)
	body := codegentest.FuncBody(t, ir, "h")
	if !strings.Contains(body, "unreachable") {
		t.Errorf("expected an unreachable terminator for the dead merge in @h, got:\n%s", body)
	}
}
