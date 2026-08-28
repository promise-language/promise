package regress6

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1336: a statement / no-hint `if`-expression with one value-producing arm and
// one reachable void arm (`if b { 1 } else {}`) previously built a phi with an
// incoming only from the value arm, leaving mergeBlock's void-arm predecessor
// without an entry → malformed IR ("PHINode should have one entry for each
// predecessor"). genIfExpr must now zero-fill the reachable void arm so every
// predecessor of mergeBlock contributes a phi incoming, mirroring buildMatchPhi.
//
// generateIR prints the module but does not run `opt`, so these tests document
// the two-incoming phi shape; the real malformed-IR guard (which invokes `opt`)
// is tests/e2e/if_void_arm_statement.pr.

func TestT1336IfVoidArmBareStatement(t *testing.T) {
	// Bare-statement `if b { 1 } else {}` used for effect — value discarded.
	ir := codegentest.GenerateIR(t, `
		m(bool b) { if b { 1 } else {}; }
		main() { m(true); }
	`)
	if !strings.Contains(ir, "phi i64") {
		t.Errorf("expected a phi for the value arm, got:\n%s", ir)
	}
	// The void arm must contribute a zero-filled incoming so the phi has one
	// entry per predecessor.
	if !strings.Contains(ir, "zeroinitializer") {
		t.Errorf("expected the void arm to be zero-filled into the phi, got:\n%s", ir)
	}
}

func TestT1336IfVoidArmInferredDecl(t *testing.T) {
	// Inferred `:=` decl — no expected-type hint, so the T1335 sema gate does
	// not fire; codegen must still emit well-formed IR.
	ir := codegentest.GenerateIR(t, `
		m(bool b) { r := if b { 1 } else {}; }
		main() { m(true); }
	`)
	if !strings.Contains(ir, "phi i64") {
		t.Errorf("expected a phi for the value arm, got:\n%s", ir)
	}
	if !strings.Contains(ir, "zeroinitializer") {
		t.Errorf("expected the void arm to be zero-filled into the phi, got:\n%s", ir)
	}
}

func TestT1336IfVoidThenArm(t *testing.T) {
	// Symmetric shape: the then arm is void, the else arm supplies the value.
	ir := codegentest.GenerateIR(t, `
		m(bool b) { if b {} else { 1 }; }
		main() { m(true); m(false); }
	`)
	if !strings.Contains(ir, "phi i64") {
		t.Errorf("expected a phi for the value arm, got:\n%s", ir)
	}
	if !strings.Contains(ir, "zeroinitializer") {
		t.Errorf("expected the void arm to be zero-filled into the phi, got:\n%s", ir)
	}
}
