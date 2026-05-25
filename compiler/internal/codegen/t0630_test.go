package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T0630: Inside a generic function/method body, constructing an enum variant
// whose field is `T?` (Optional) by passing an EXPLICIT `T?` argument
// previously panicked in genEnumVariantCallLayout. The T0608 coercion
// concretized `exprType` via `c.typeSubst` but left `vfType` un-substituted
// (resolveMatchFieldType's Instance.TypeArgs() path produces an identity map
// `{T→T}` for unresolved instances, short-circuiting the c.typeSubst
// fallback). The asymmetric `Identical(int?, T?) = false` check spuriously
// called wrapOptional on an already-`{i1,payload}` aggregate, causing
// `insertvalue elem type mismatch, expected i64, got { i1, i64 }`.
//
// The fix applies `types.Substitute(vfType, c.typeSubst)` symmetrically with
// exprType before the Identical comparison. Verify:
//   - Explicit `T?` instance (`make_box_opt[int]`) emits NO Optional-aggregate
//     wrap (Identical types under symmetric subst → arg loaded & stored
//     directly). This is the previously-panicking path.
//   - Bare-widen instance (`make_box[int]`) STILL emits the wrap (T0608
//     behavior preserved: Identical(int, int?) = false → wrap correctly).

// TestT0630_ExplicitOptionalInGenericFnBodyNoWrap — the monomorphized
// `make_box_opt[int]` body must load the explicit `int?` arg and store it
// directly into the variant field with NO `insertvalue { i1, i64 } undef`
// wrap pattern. Locks the symmetric-subst invariant against regression.
func TestT0630_ExplicitOptionalInGenericFnBodyNoWrap(t *testing.T) {
	ir := generateIR(t, `
		enum Box[T] { Full(T? v), Vacant, }
		make_box_opt[T](T? x) Box[T] { return Box[T].Full(x); }
		main() { int? o = 7; Box[int] b = make_box_opt(o); }
	`)
	body := extractFunction(ir, `"make_box_opt[int]"`)
	if body == "" {
		t.Fatalf("expected monomorphized @\"make_box_opt[int]\" in IR:\n%s", ir)
	}
	// Identical Optional types ⇒ no extra wrap inside the mono'd body.
	// Before the fix, a spurious `insertvalue { i1, i64 } undef, ...` was
	// emitted and panicked on the value-element insertvalue.
	if strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("explicit `T?` arg inside generic-fn body must not be re-wrapped under typeSubst:\n%s", body)
	}
	// The variant field store must still happen with the matching Optional type.
	if !strings.Contains(body, "store { i1, i64 }") {
		t.Fatalf("expected a `store { i1, i64 }` into the variant field inside generic-fn body:\n%s", body)
	}
	// Guard against the old panic shape (bare i64 store into an Optional slot).
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("variant field received a bare i64 store inside generic-fn body (pre-T0630 corruption shape):\n%s", body)
	}
}

// TestT0630_BareWidenInGenericFnBodyStillWraps — symmetric guard: the bare-T
// path inside a generic body must STILL wrap (Identical(int, int?) = false
// → wrapOptional correctly applied). This ensures the T0630 fix did not
// accidentally suppress the legitimate T0608 wrap when types genuinely differ.
func TestT0630_BareWidenInGenericFnBodyStillWraps(t *testing.T) {
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
		t.Fatalf("expected Optional aggregate wrap for bare T->T? widening in generic-fn body (must not be regressed by T0630 fix):\n%s", body)
	}
	storeIdx := strings.Index(body, "store { i1, i64 }")
	if storeIdx < 0 {
		t.Fatalf("expected a `store { i1, i64 }` into the variant field:\n%s", body)
	}
	if wrapIdx > storeIdx {
		t.Errorf("Optional aggregate must be built BEFORE the variant-field store (wrapIdx=%d, storeIdx=%d):\n%s", wrapIdx, storeIdx, body)
	}
}

// TestT0630_NoneLiteralInGenericFnBodyZeroInits — the NoneLit branch of
// genEnumVariantCallLayout under c.typeSubst. The argument is `none`
// (*ast.NoneLit), so the NoneLit branch fires and emits `zeroValue(fieldLLVM)`
// → `store { i1, i64 } zeroinitializer, ...` into the variant field. NO
// wrapOptional, NO insertvalue chain. The T0630 symmetric substitution must
// preserve `vfType.(*types.Optional)` recognition under typeSubst so this
// branch continues to fire (its outer guard) for the full type matrix.
func TestT0630_NoneLiteralInGenericFnBodyZeroInits(t *testing.T) {
	ir := generateIR(t, `
		enum Box[T] { Full(T? v), Vacant, }
		make_box_none[T]() Box[T] { return Box[T].Full(none); }
		main() { Box[int] b = make_box_none[int](); }
	`)
	body := extractFunction(ir, `"make_box_none[int]"`)
	if body == "" {
		t.Fatalf("expected monomorphized @\"make_box_none[int]\" in IR:\n%s", ir)
	}
	// NoneLit branch must emit a zeroinitializer store into the Optional
	// variant field — no wrap pattern, no insertvalue chain.
	if !strings.Contains(body, "store { i1, i64 } zeroinitializer") {
		t.Fatalf("expected `store { i1, i64 } zeroinitializer` from NoneLit branch in generic-fn body:\n%s", body)
	}
	if strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("NoneLit branch must not emit a wrapOptional pattern under typeSubst:\n%s", body)
	}
	if strings.Contains(body, "insertvalue { i1, i64 } undef, i1 false, 0") {
		t.Errorf("NoneLit branch should use zeroinitializer directly, not insertvalue chains:\n%s", body)
	}
}

// TestT0630_ExplicitOptionalInGenericMethodBodyNoWrap — same invariant for a
// generic METHOD body (owner type generic). Exercises the typeSubst path
// inside method monomorphization.
func TestT0630_ExplicitOptionalInGenericMethodBodyNoWrap(t *testing.T) {
	ir := generateIR(t, `
		enum Box[T] { Full(T? v), Vacant, }
		type Wrap[T] {
			T seed;
			mk_opt(this, T? o) Box[T] { return Box[T].Full(o); }
		}
		main() {
			Wrap[int] w = Wrap[int](seed: 5);
			int? o = 8;
			Box[int] b = w.mk_opt(o);
		}
	`)
	body := extractFunction(ir, `"Wrap[int].mk_opt"`)
	if body == "" {
		t.Fatalf("expected monomorphized @\"Wrap[int].mk_opt\" in IR:\n%s", ir)
	}
	if strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("explicit `T?` arg inside generic-method body must not be re-wrapped under typeSubst:\n%s", body)
	}
	if !strings.Contains(body, "store { i1, i64 }") {
		t.Fatalf("expected a `store { i1, i64 }` into the variant field inside generic-method body:\n%s", body)
	}
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("variant field received a bare i64 store inside generic-method body (pre-T0630 corruption shape):\n%s", body)
	}
}
