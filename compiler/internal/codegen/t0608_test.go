package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T0608: Constructing an enum variant whose field is `T?` (Optional) by
// passing a bare non-optional value (implicit `T` -> `T?` widening) or a
// `none` literal previously panicked in genEnumVariantCallLayout because the
// arg value was stored directly into the variant field with no coercion to
// the declared `{i1, payload}` Optional type. The fix mirrors the
// struct-constructor Optional path: build the Optional aggregate (bare value)
// or zeroinitializer (none) before the variant-field store, and leave an
// explicit `T?` arg unwrapped (Identical types — the pre-T0608 passing path).

// TestT0608_BareValueWrapsOptionalBeforeStore — `H.Slot(n)` where `n` is a
// bare `int` and the variant field is `int?`. The `{i1,i64}` Optional
// aggregate must be built (insertvalue with the present flag) BEFORE the
// variant-field store, not stored as a bare i64.
func TestT0608_BareValueWrapsOptionalBeforeStore(t *testing.T) {
	ir := generateIR(t, `
		enum H { Slot(int? v), Empty, }
		mk_bare(int n) { H j = H.Slot(n); }
		main() {}
	`)
	body := extractFunction(ir, "__user.mk_bare")
	if body == "" {
		t.Fatalf("expected __user.mk_bare in IR")
	}
	wrapIdx := strings.Index(body, "insertvalue { i1, i64 } undef, i1 true, 0")
	if wrapIdx < 0 {
		t.Fatalf("expected Optional aggregate insertvalue (i1 true present flag) for bare int->int? widening:\n%s", body)
	}
	storeIdx := strings.Index(body, "store { i1, i64 }")
	if storeIdx < 0 {
		t.Fatalf("expected a `store { i1, i64 }` into the variant field:\n%s", body)
	}
	if wrapIdx > storeIdx {
		t.Errorf("expected the Optional aggregate to be built BEFORE the variant-field store (wrapIdx=%d, storeIdx=%d):\n%s", wrapIdx, storeIdx, body)
	}
	// Guard against the original panic shape: a bare i64 SSA value stored
	// directly into the `{ i1, i64 }*` variant slot (SSA-name-agnostic).
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("variant field received a bare i64 store (T0608 panic shape):\n%s", body)
	}
}

// TestT0608_NoneLiteralZeroInitBeforeStore — `H.Slot(none)` into an `int?`
// variant field must store a zeroinitializer of the concrete `{i1,i64}`
// layout type (the B0210-style direct-from-layout none value), not a bare i1.
func TestT0608_NoneLiteralZeroInitBeforeStore(t *testing.T) {
	ir := generateIR(t, `
		enum H { Slot(int? v), Empty, }
		mk_none() { H j = H.Slot(none); }
		main() {}
	`)
	body := extractFunction(ir, "__user.mk_none")
	if body == "" {
		t.Fatalf("expected __user.mk_none in IR")
	}
	if !strings.Contains(body, "store { i1, i64 } zeroinitializer, { i1, i64 }*") {
		t.Errorf("expected `store { i1, i64 } zeroinitializer` for none-literal into int? variant field:\n%s", body)
	}
	// No present-flag wrap for a none literal.
	if strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("none literal must not build a present Optional aggregate:\n%s", body)
	}
	// Guard against the original none panic shape: a bare i1 (constant or
	// SSA) stored directly into the `{ i1, i64 }*` variant slot.
	if regexp.MustCompile(`store i1 (false|true|%\w+), \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("variant field received a bare i1 store (T0608 none panic shape):\n%s", body)
	}
}

// TestT0608_ExplicitOptionalNoExtraWrap — `H.Slot(o)` where `o` is already an
// explicit `int?`. The arg type is Identical to the variant field type, so
// no extra wrap must be emitted (the value is loaded and stored directly).
// This is the path that already passed before T0608 — guard against
// double-wrapping it.
func TestT0608_ExplicitOptionalNoExtraWrap(t *testing.T) {
	ir := generateIR(t, `
		enum H { Slot(int? v), Empty, }
		mk_expl(int? o) { H j = H.Slot(o); }
		main() {}
	`)
	body := extractFunction(ir, "__user.mk_expl")
	if body == "" {
		t.Fatalf("expected __user.mk_expl in IR")
	}
	if !strings.Contains(body, "load { i1, i64 }, { i1, i64 }* %o.addr") {
		t.Fatalf("expected the explicit int? arg to be loaded as {i1,i64}:\n%s", body)
	}
	if !strings.Contains(body, "store { i1, i64 }") {
		t.Fatalf("expected a `store { i1, i64 }` into the variant field:\n%s", body)
	}
	// Identical types ⇒ no wrapOptional ⇒ no insertvalue at all in this body.
	if strings.Contains(body, "insertvalue { i1, i64 }") {
		t.Errorf("explicit int? arg must not be re-wrapped (Identical types — pre-T0608 passing path):\n%s", body)
	}
}

// TestT0608_TypeSubstBareWidenInGenericFnBody — constructing an Optional enum
// variant field from a BARE TypeParam arg INSIDE a generic function body
// (`make_box[T](T x) { Box[T].Full(x) }` → `make_box[int]`). This exercises
// the `c.typeSubst != nil` substitution branch of genEnumVariantCallLayout
// (expr.go ~5762-5764) plus the wrap-under-subst path — both had ZERO Go and
// e2e coverage before this T0608 coverage pass (the original T0608 tests all
// construct the variant at a non-generic call site). The mono'd body must
// still build the `{i1,i64}` Optional aggregate before the variant-field
// store, not store a bare i64.
//
// NOTE: the analogous EXPLICIT `T?` arg inside a generic body currently
// panics (asymmetric typeSubst: exprType concretized, vfType not) — tracked
// as T0630, deliberately NOT asserted here until that regression is fixed.
func TestT0608_TypeSubstBareWidenInGenericFnBody(t *testing.T) {
	ir := generateIR(t, `
		enum Box[T] { Full(T? v), Vacant, }
		make_box[T](T x) Box[T] { return Box[T].Full(x); }
		main() { Box[int] b = make_box(99); }
	`)
	body := extractFunction(ir, `"make_box[int]"`)
	if body == "" {
		t.Fatalf("expected monomorphized @\"make_box[int]\" in IR:\n%s", ir)
	}
	wrapIdx := strings.Index(body, "insertvalue { i1, i64 } undef, i1 true, 0")
	if wrapIdx < 0 {
		t.Fatalf("expected Optional aggregate insertvalue (i1 true present flag) for bare T->T? widening under typeSubst:\n%s", body)
	}
	storeIdx := strings.Index(body, "store { i1, i64 }")
	if storeIdx < 0 {
		t.Fatalf("expected a `store { i1, i64 }` into the variant field under typeSubst:\n%s", body)
	}
	if wrapIdx > storeIdx {
		t.Errorf("expected the Optional aggregate built BEFORE the variant-field store under typeSubst (wrapIdx=%d, storeIdx=%d):\n%s", wrapIdx, storeIdx, body)
	}
	// Guard against the original T0608 panic shape inside the mono'd body.
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("variant field received a bare i64 store inside generic-fn body (T0608 panic shape):\n%s", body)
	}
}
