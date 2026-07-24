package sema

import (
	"strings"
	"testing"
)

// T1337: binding a value-less `if`/`match` (void/empty arms, or all arms
// diverge with no contextual hint) to a var decl must emit a clear sema error
// rather than letting the nil result flow to codegen, which panicked with
// "nil value for inferred/typed var decl".

func TestT1337_InferredBothArmsVoid(t *testing.T) {
	errs := checkErrs(t, `foo(bool b) { r := if b {} else {}; } main() {}`)
	expectError(t, errs, "produces no value")
}

func TestT1337_InferredOneArmDivergesSiblingVoid(t *testing.T) {
	errs := checkErrs(t, `foo(bool b) int { r := if b { return 1 } else {}; return 0; } main() {}`)
	expectError(t, errs, "produces no value")
}

func TestT1337_InferredMatchMixed(t *testing.T) {
	errs := checkErrs(t, `foo(bool b) int { r := match b { true => { return }, _ => {} }; return 0; } main() {}`)
	// A bare `return` in an int function is itself an error; the key property is
	// that the compile halts cleanly (no codegen panic) with at least one error.
	if len(errs) == 0 {
		t.Fatalf("expected an error, got none")
	}
}

func TestT1337_InferredAllDivergeNoHint(t *testing.T) {
	errs := checkErrs(t, `foo(bool b) int { r := match b { true => { return 1 }, _ => { return 2 } }; return 0; } main() {}`)
	expectError(t, errs, "produces no value")
}

func TestT1337_TypedBothArmsVoid(t *testing.T) {
	errs := checkErrs(t, `foo(bool b) { int r = if b {} else {}; } main() {}`)
	expectError(t, errs, "produces no value")
}

// Assignment position (not a var decl) shares the same root cause: a value-less
// if/match RHS returns nil, and checkAssignStmt previously returned silently on
// nil, letting the nil flow to codegen's store → panic.

func TestT1337_AssignVarBothArmsVoid(t *testing.T) {
	errs := checkErrs(t, `foo(bool b) { r := 0; r = if b {} else {}; } main() {}`)
	expectError(t, errs, "produces no value")
}

func TestT1337_AssignFieldBothArmsVoid(t *testing.T) {
	errs := checkErrs(t, `type T { int x; } foo(bool b) { t := T(x: 0); t.x = if b {} else {}; } main() {}`)
	expectError(t, errs, "produces no value")
}

func TestT1337_AssignVarOK(t *testing.T) {
	checkOK(t, `foo(bool b) { r := 0; r = if b { 1 } else { 2 }; } main() {}`)
}

// Regressions — these must continue to compile without error.

func TestT1337_ValuedInferredOK(t *testing.T) {
	checkOK(t, `foo(bool b) int { r := if b { 1 } else { 2 }; return r; } main() {}`)
}

func TestT1337_BareMatchStatementVoidArmsOK(t *testing.T) {
	checkOK(t, `foo(bool b) { match b { true => {}, _ => {} }; } main() {}`)
}

func TestT1337_TypedAllDivergeWithHintOK(t *testing.T) {
	checkOK(t, `foo(bool b) int { int r = if b { return 1 } else { return 2 }; return r; } main() {}`)
}

// No-cascade guard: when an arm of the value-less if/match has its OWN error,
// checkIfExpr/checkMatchExpr also return nil — but the `len(c.errors) ==
// errsBefore` guard must suppress the T1337 diagnostic so the real (inner) error
// is reported alone, not doubled up with a spurious "produces no value". These
// exercise the false branch of that guard at each of the three call sites.

func containsProducesNoValue(errs []error) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), "produces no value") {
			return true
		}
	}
	return false
}

func TestT1337_InferredInnerErrorNoCascade(t *testing.T) {
	errs := checkErrs(t, `foo(bool b) { r := if b { missing } else {}; } main() {}`)
	expectError(t, errs, "undefined: missing")
	if containsProducesNoValue(errs) {
		t.Fatalf("inner-arm error should suppress the T1337 diagnostic (no cascade); got: %v", errs)
	}
}

func TestT1337_TypedInnerErrorNoCascade(t *testing.T) {
	errs := checkErrs(t, `foo(bool b) { int r = if b { missing } else {}; } main() {}`)
	expectError(t, errs, "undefined: missing")
	if containsProducesNoValue(errs) {
		t.Fatalf("inner-arm error should suppress the T1337 diagnostic (no cascade); got: %v", errs)
	}
}

func TestT1337_AssignInnerErrorNoCascade(t *testing.T) {
	errs := checkErrs(t, `foo(bool b) { r := 0; r = if b { missing } else {}; } main() {}`)
	expectError(t, errs, "undefined: missing")
	if containsProducesNoValue(errs) {
		t.Fatalf("inner-arm error should suppress the T1337 diagnostic (no cascade); got: %v", errs)
	}
}

// Assign with an invalid (nil-typed) target must not emit the T1337 diagnostic:
// the `targetType != nil` guard requires a valid LHS, so the undefined-target
// error is reported alone.
func TestT1337_AssignInvalidTargetNoCascade(t *testing.T) {
	errs := checkErrs(t, `foo(bool b) { missing_target = if b {} else {}; } main() {}`)
	expectError(t, errs, "undefined")
	if containsProducesNoValue(errs) {
		t.Fatalf("invalid target should suppress the T1337 diagnostic; got: %v", errs)
	}
}
