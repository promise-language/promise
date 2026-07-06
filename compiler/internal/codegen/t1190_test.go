package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T1190: a `match`/`if` whose arms are ALL bare `none` has expression type
// `none` (TypNone) — there is no value-arm sibling to unify against. Binding it
// to an inferred local and using that local in a typed Optional context used to
// panic in codegen: resolveType(TypNone) returned `void`, so the local's alloca
// and the merge phi carried a bare `i1`/`i64` that mismatched the `{ i1, T }`
// store slot ("store operands are not compatible: src=i64; dst=void*").
//
// The fix lowers a none-typed value to the void-optional `i1` (resolveType now
// returns i1 for TypNone) and coerces it to the concrete Optional[T] zero at the
// return/typed-store consumption sites. These tests assert the merge is an `i1`
// phi (never a bare `i64` phi), the local's alloca is `i1`, and the function
// materializes a `{ i1, i64 }` zero at return — the exact regression shape.

// TestT1190_BothNoneMatchLowersToI1 — `x := match k { 0 => none, _ => none }`
// bound to an int?-returning local: merge phi is `i1`, alloca is `i1`, and the
// return materializes a `{ i1, i64 }` zeroinitializer. No bare-i64 phi/store.
func TestT1190_BothNoneMatchLowersToI1(t *testing.T) {
	ir := generateIR(t, `
		_both_none(int k) int? {
		  x := match k { 0 => none, _ => none };
		  return x;
		}
		main() { int n = _both_none(0) ?: 5; }
	`)
	body := extractFunction(ir, "__user._both_none")
	if body == "" {
		t.Fatalf("expected define @__user._both_none in IR:\n%s", ir)
	}
	// The none-typed local's alloca is i1 (resolveType(TypNone) → i1), not void*.
	if !strings.Contains(body, "= alloca i1") {
		t.Errorf("expected the none-typed local alloca to be `i1`:\n%s", body)
	}
	// The all-`none` merge lowers to an `i1` phi (both arms `false`).
	if !regexp.MustCompile(`phi i1 \[ false,`).MatchString(body) {
		t.Errorf("expected an `i1` merge phi for the all-none match:\n%s", body)
	}
	// The return materializes the concrete Optional[int] zero.
	if !strings.Contains(body, "ret { i1, i64 } zeroinitializer") {
		t.Errorf("expected `ret { i1, i64 } zeroinitializer` for the empty optional:\n%s", body)
	}
	// Pre-fix panic shapes: a bare `i64` phi/merge or a store into a `void*`
	// slot must NOT appear.
	if strings.Contains(body, "phi i64") {
		t.Errorf("unexpected bare `i64` phi (pre-T1190 regression shape):\n%s", body)
	}
	if regexp.MustCompile(`store i64 %?\w+, void\*`).MatchString(body) ||
		strings.Contains(body, "= alloca void") {
		t.Errorf("unexpected `void*` alloca/store (pre-T1190 regression shape):\n%s", body)
	}
}

// TestT1190_BothNoneIfLowersToI1 — the `if`-expr form crashes identically
// pre-fix; assert the same `i1`-phi / `{ i1, i64 }`-return shape.
func TestT1190_BothNoneIfLowersToI1(t *testing.T) {
	ir := generateIR(t, `
		_both_none_if(int k) int? {
		  x := if k == 0 { none } else { none };
		  return x;
		}
		main() { int n = _both_none_if(0) ?: 5; }
	`)
	body := extractFunction(ir, "__user._both_none_if")
	if body == "" {
		t.Fatalf("expected define @__user._both_none_if in IR:\n%s", ir)
	}
	if !strings.Contains(body, "= alloca i1") {
		t.Errorf("expected the none-typed local alloca to be `i1`:\n%s", body)
	}
	if !regexp.MustCompile(`phi i1 \[ false,`).MatchString(body) {
		t.Errorf("expected an `i1` merge phi for the all-none if-expr:\n%s", body)
	}
	if !strings.Contains(body, "ret { i1, i64 } zeroinitializer") {
		t.Errorf("expected `ret { i1, i64 } zeroinitializer` for the empty optional:\n%s", body)
	}
	if strings.Contains(body, "phi i64") {
		t.Errorf("unexpected bare `i64` phi (pre-T1190 regression shape):\n%s", body)
	}
}

// TestT1190_BothNoneGenericMonoLowersToI1 — the same all-`none` match bound in a
// GENERIC function returning `T?`. The concrete Optional shape is only known
// after monomorphization, so the return coercion runs through
// coerceNoneToOptional's `c.typeSubst != nil` branch (substituting the none-typed
// expr type under the active mono subst). Assert the `Box[int]`-specialized body
// still lowers to an `i1` merge and materializes the `{ i1, i64 }` zero — no bare
// `i64` phi survives the substitution path.
func TestT1190_BothNoneGenericMonoLowersToI1(t *testing.T) {
	ir := generateIR(t, `
		_gen_both_none[T](int k) T? {
		  x := match k { 0 => none, _ => none };
		  return x;
		}
		main() { int n = _gen_both_none[int](0) ?: 5; }
	`)
	// Mono name is quoted in IR: @"_gen_both_none[int]".
	body := extractFunction(ir, `"_gen_both_none[int]"`)
	if body == "" {
		t.Fatalf("expected define @\"_gen_both_none[int]\" in IR:\n%s", ir)
	}
	if !strings.Contains(body, "= alloca i1") {
		t.Errorf("expected the none-typed local alloca to be `i1`:\n%s", body)
	}
	if !regexp.MustCompile(`phi i1 \[ false,`).MatchString(body) {
		t.Errorf("expected an `i1` merge phi for the generic all-none match:\n%s", body)
	}
	if !strings.Contains(body, "ret { i1, i64 } zeroinitializer") {
		t.Errorf("expected `ret { i1, i64 } zeroinitializer` for the empty optional:\n%s", body)
	}
	if strings.Contains(body, "phi i64") {
		t.Errorf("unexpected bare `i64` phi (pre-T1190 regression shape):\n%s", body)
	}
}
