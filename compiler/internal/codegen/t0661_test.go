package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T0661: contains with a bare (non-optional) value into a Vector[T?] / T?[].
//
// Pre-fix, genVectorMethodCall's `contains` case generated the argument with
// genCallArgExpr and stored the raw scalar straight into the {i1, T} element
// alloca, panicking:
//   "store operands are not compatible: src=i64; dst={ i1, i64 }*"
// `v.contains(none)` was the same panic shape — c.targetType was never set so
// genNoneLit returned the i1 0 "void optional fallback" instead of a zero
// {i1,T} struct.
//
// The fix mirrors the T0658 push-case fix: sets c.targetType to the resolved
// Optional element type around argument generation and wraps a bare RHS into
// the Optional struct via wrapOptional, gated by
// (argExprType != none && !Identical(argExprType, resolvedElem)).
// contains is read-only, so no claimStringTemp/claimHeapTemp/enumCtorTemps
// dance is needed.

// TestT0661_ContainsBareIntoOptionalVectorWraps — `v.contains(1)` must build a
// {i1,i64} aggregate (wrapOptional) and store THAT into the contains arg alloca;
// the bare-i64-store panic shape must be gone. `v.contains(none)` must lower to
// a zeroinitializer {i1,i64} store (genNoneLit via the newly-set c.targetType).
func TestT0661_ContainsBareIntoOptionalVectorWraps(t *testing.T) {
	ir := generateIR(t, `main() { int?[] v = []; v.push(1); bool b1 = v.contains(1); bool b2 = v.contains(none); }`)
	body := extractFunc(ir, ".goroutine.main")
	if body == "" {
		t.Fatalf("expected define @.goroutine.main in IR:\n%s", ir)
	}
	// `v.contains(1)` → wrapOptional builds the {i1,i64} aggregate.
	if !strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("expected wrapOptional aggregate for bare-int contains into int?[]:\n%s", body)
	}
	// The wrapped aggregate is stored into the contains arg alloca.
	if !strings.Contains(body, "store { i1, i64 }") {
		t.Fatalf("expected a `store { i1, i64 }` into the contains arg alloca:\n%s", body)
	}
	// Pre-fix corruption/panic shape: a bare i64 stored into a {i1,i64}* slot.
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("contains arg alloca received a bare i64 store (pre-T0661 panic shape):\n%s", body)
	}
	// `v.contains(none)` → genNoneLit under the newly-set c.targetType emits a
	// zeroinitializer {i1,i64} (NOT the `i1 0` void-optional fallback).
	if !strings.Contains(body, "store { i1, i64 } zeroinitializer") {
		t.Errorf("expected `store { i1, i64 } zeroinitializer` for `v.contains(none)` into int?[]:\n%s", body)
	}
}

// TestT0661_ContainsExplicitOptionalNoDoubleWrap — regression guard: passing an
// already-typed `int?` must NOT re-wrap (skipped via the !types.Identical
// predicate). This is the only case that worked pre-fix; the new wrap must
// preserve it.
func TestT0661_ContainsExplicitOptionalNoDoubleWrap(t *testing.T) {
	ir := generateIR(t, `
		f(int? x) bool { int?[] v = []; v.push(x); return v.contains(x); }
		main() { int? o = 5; _ := f(o); }
	`)
	body := extractFunction(ir, "__user.f")
	if body == "" {
		t.Fatalf("expected define @__user.f in IR:\n%s", ir)
	}
	// x : int? is Identical to the int? element type → wrap must be skipped.
	if strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("already-optional contains arg must NOT be re-wrapped (Identical guard):\n%s", body)
	}
	// The loaded {i1,i64} value is still stored into the contains arg alloca.
	if !strings.Contains(body, "store { i1, i64 }") {
		t.Fatalf("expected a `store { i1, i64 }` into the contains arg alloca:\n%s", body)
	}
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("contains arg alloca received a bare i64 store (pre-T0661 panic shape):\n%s", body)
	}
}

// TestT0661_ContainsBareIntoGenericOptionalVector — the c.typeSubst path: inside
// the monomorphized `_check[int]` body, `v.contains(x)` where x : T (= int) and
// the slot is T? (= int?) must wrap the bare arg under the substitution of
// resolvedElem / argExprType. Pre-fix this panicked in the mono'd body.
func TestT0661_ContainsBareIntoGenericOptionalVector(t *testing.T) {
	ir := generateIR(t, `
		_check[T](T move x) bool { T?[] v = []; v.push(x); return v.contains(x); }
		main() { _ := _check[int](7); }
	`)
	body := extractFunc(ir, `"_check[int]"`)
	if body == "" {
		t.Fatalf("expected monomorphized define @\"_check[int]\" in IR:\n%s", ir)
	}
	if !strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("expected wrapOptional aggregate for bare-T contains into T?[] in mono'd body:\n%s", body)
	}
	if !strings.Contains(body, "store { i1, i64 }") {
		t.Fatalf("expected a `store { i1, i64 }` into the contains arg alloca in mono'd body:\n%s", body)
	}
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("contains arg alloca received a bare i64 store in mono'd body (pre-T0661 panic shape):\n%s", body)
	}
}

// TestT0661_ContainsBareF64IntoOptionalVectorCustomEqFn — exercises the float
// (FCmp) path inside getOrEmitOptContainsEqFn. For f64?[], the inner LLVM type
// is `double` (FloatType), so the function is named __promise_opt_eq_double and
// uses `fcmp oeq double` instead of `icmp eq`. The test mirrors
// TestT0661_ContainsOptionalUsesCustomEqFn but for the float case.
func TestT0661_ContainsBareF64IntoOptionalVectorCustomEqFn(t *testing.T) {
	ir := generateIR(t, `main() { f64?[] v = []; v.push(3.5); bool b = v.contains(3.5); }`)

	if !strings.Contains(ir, "@__promise_opt_eq_double(") {
		t.Fatalf("expected @__promise_opt_eq_double in module IR:\n%s", ir)
	}
	eqBody := extractFunc(ir, "__promise_opt_eq_double")
	if eqBody == "" {
		t.Fatalf("expected define @__promise_opt_eq_double in IR:\n%s", ir)
	}
	// Flag comparison uses icmp eq i1 (same as int case).
	if !strings.Contains(eqBody, "load i1,") {
		t.Errorf("expected `load i1,` (presence flag) in __promise_opt_eq_double:\n%s", eqBody)
	}
	// Value comparison must use fcmp oeq for float (not icmp eq).
	if !strings.Contains(eqBody, "fcmp oeq double") {
		t.Errorf("expected `fcmp oeq double` (float comparison) in __promise_opt_eq_double:\n%s", eqBody)
	}
	// The call site must reference the float eq function.
	body := extractFunc(ir, ".goroutine.main")
	if !strings.Contains(body, "@__promise_opt_eq_double") {
		t.Errorf("expected @__promise_opt_eq_double passed to promise_vector_contains:\n%s", body)
	}
}

// TestT0661_ContainsOptionalUsesCustomEqFn — the fix for the cross-function WASM
// padding failure. Contains for Optional elements must pass __promise_opt_eq_<T>
// (not null) to promise_vector_contains. The generated function compares the i1
// presence flag and inner scalar value directly, bypassing memcmp — which would
// compare all 16 bytes of {i1, i64} including the 7 undefined padding bytes that
// LLVM O1 fails to zero when decomposing `store zeroinitializer` into per-field
// stores. Using a custom eq function makes contains padding-insensitive.
func TestT0661_ContainsOptionalUsesCustomEqFn(t *testing.T) {
	ir := generateIR(t, `main() { int?[] v = []; v.push(1); bool b = v.contains(1); }`)

	// The module must contain a custom Optional equality function for i64.
	if !strings.Contains(ir, "@__promise_opt_eq_i64(") {
		t.Fatalf("expected @__promise_opt_eq_i64 in module IR:\n%s", ir)
	}
	eqBody := extractFunc(ir, "__promise_opt_eq_i64")
	if eqBody == "" {
		t.Fatalf("expected define @__promise_opt_eq_i64 in IR:\n%s", ir)
	}
	// Field-by-field: loads the i1 flag and the i64 value separately.
	if !strings.Contains(eqBody, "load i1,") {
		t.Errorf("expected `load i1,` (presence flag) in __promise_opt_eq_i64:\n%s", eqBody)
	}
	if !strings.Contains(eqBody, "icmp eq i64") {
		t.Errorf("expected `icmp eq i64` (value comparison) in __promise_opt_eq_i64:\n%s", eqBody)
	}
	// The call site must reference the eq function (not i8* null).
	body := extractFunc(ir, ".goroutine.main")
	if !strings.Contains(body, "@__promise_opt_eq_i64") {
		t.Errorf("expected @__promise_opt_eq_i64 passed to promise_vector_contains:\n%s", body)
	}
}
