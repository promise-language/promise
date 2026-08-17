package sema

import "testing"

// T1579: fixed-array repeat literal `[value; count]`.

func TestT1579RepeatLiteralOK(t *testing.T) {
	checkOK(t, `main() { u32[4] w = [0u32; 4]; }`)
}

func TestT1579RepeatLiteralNestedOK(t *testing.T) {
	checkOK(t, `main() { u8[4][3] grid = [[0u8; 4]; 3]; }`)
}

func TestT1579RepeatLiteralNonConstantCount(t *testing.T) {
	errs := checkErrs(t, `main() { n := 4; w := [0u32; n]; }`)
	expectError(t, errs, "array repeat count must be a constant integer literal")
}

func TestT1579RepeatLiteralNonCopyElement(t *testing.T) {
	errs := checkErrs(t, `main() { s := "hi"; x := [s; 3]; }`)
	expectError(t, errs, "must be `copy`")
}

// The `u32[64].filled(...)` shape parses as indexing (an inherent ambiguity);
// sema improves the diagnostic to point at the repeat literal.
func TestT1579FilledDiagnostic(t *testing.T) {
	errs := checkErrs(t, `main() { w := u32[64].filled(0u32, 64); }`)
	expectError(t, errs, "to build a sized array use a repeat literal: [<value>; 64]")
}

// A `T[N]` hint whose element differs from the value drives numeric/Some-wrapping
// widening: `u32? <- u32` widens the once-evaluated element to the hint type.
func TestT1579RepeatLiteralWidening(t *testing.T) {
	checkOK(t, `main() { u32?[4] w = [0u32; 4]; }`)
}

// An optional-array hint `T[N]?` derives the element hint from the inner array so
// the element still widens (exercises the `*types.Optional` hint branch).
func TestT1579RepeatLiteralOptionalArrayHint(t *testing.T) {
	checkOK(t, `main() { u32?[4]? w = [0u32; 4]; }`)
}

// A count literal that fits IntLit syntactically but overflows int64 is rejected
// by the non-negative/int64 guard (in addition to the element overflow check).
func TestT1579RepeatLiteralCountOverflow(t *testing.T) {
	errs := checkErrs(t, `main() { w := [0u32; 999999999999999999999999]; }`)
	expectError(t, errs, "array repeat count must be a non-negative constant integer literal")
}

// An undefined element expression yields a nil element type; the checker must
// bail cleanly (no panic) and surface the underlying error.
func TestT1579RepeatLiteralUndefinedElement(t *testing.T) {
	errs := checkErrs(t, `main() { w := [zzz; 4]; }`)
	expectError(t, errs, "undefined: zzz")
}
