package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T0658: pushing a bare (non-optional) value into a Vector[T?] / T?[].
//
// Pre-fix, genVectorMethodCall's `push` case generated the argument with
// genCallArgExpr and stored the raw scalar/pointer straight into the
// {i1, T} element alloca, panicking inside generateIR:
//   "store operands are not compatible: src=i64; dst={ i1, i64 }*"
// `v.push(none)` was the same panic shape — c.targetType was never set so
// genNoneLit returned the i1 0 "void optional fallback" instead of a zero
// {i1,T} struct.
//
// The fix (push-side analog of T0615 / genVectorIndexAssign) sets
// c.targetType to the resolved Optional element type around argument
// generation and wraps a bare RHS into the Optional struct via
// wrapOptional, gated by the same predicate as T0615
// (argExprType != none && !Identical(argExprType, resolvedElem)).
//
// NOTE: these tests use extractFunc (not extractFunction): the user main
// is lowered to @.goroutine.main, whose first textual @.goroutine.main(
// occurrence is the *call site* inside the C @main wrapper. extractFunc
// skips call sites and walks to the `define`.

// TestT0658_PushBareIntoOptionalVectorWraps — `v.push(1)` must build a
// {i1,i64} aggregate (wrapOptional) and store THAT into the push arg
// alloca; the bare-i64-store panic shape must be gone. `v.push(none)`
// must lower to a zeroinitializer {i1,i64} store (genNoneLit via the
// newly-set c.targetType), not the i1 0 fallback.
func TestT0658_PushBareIntoOptionalVectorWraps(t *testing.T) {
	// Pre-fix this call panics inside generateIR at the unconditional
	// NewStore(argVal, argAlloca) in genVectorMethodCall's push case.
	ir := generateIR(t, `main() { int?[] v = []; v.push(1); v.push(none); }`)
	body := extractFunc(ir, ".goroutine.main")
	if body == "" {
		t.Fatalf("expected define @.goroutine.main in IR:\n%s", ir)
	}
	// `v.push(1)` → wrapOptional builds the {i1,i64} aggregate.
	if !strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("expected wrapOptional aggregate for bare-int push into int?[]:\n%s", body)
	}
	// The wrapped aggregate is stored into the push arg alloca.
	if !strings.Contains(body, "store { i1, i64 }") {
		t.Fatalf("expected a `store { i1, i64 }` into the push arg alloca:\n%s", body)
	}
	// Pre-fix corruption/panic shape: a bare i64 stored into a {i1,i64}*
	// slot. Must NOT appear (this was the panic the fix removes).
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("push arg alloca received a bare i64 store (pre-T0658 panic shape):\n%s", body)
	}
	// `v.push(none)` → genNoneLit under the newly-set c.targetType emits a
	// zeroinitializer {i1,i64} (NOT the `i1 0` void-optional fallback,
	// which would have produced an i1-typed value stored into {i1,i64}*).
	if !strings.Contains(body, "store { i1, i64 } zeroinitializer") {
		t.Errorf("expected `store { i1, i64 } zeroinitializer` for `v.push(none)` into int?[]:\n%s", body)
	}
}

// TestT0658_PushExplicitOptionalNoDoubleWrap — regression guard: pushing an
// already-typed `int?` must NOT re-wrap (skipped via the !types.Identical
// predicate). This is the only case that worked pre-fix; the new wrap must
// preserve it. `x` is a parameter (no literal→optional widening in the
// body) so ANY wrap aggregate would be an erroneous double-wrap of `x`.
func TestT0658_PushExplicitOptionalNoDoubleWrap(t *testing.T) {
	ir := generateIR(t, `
		f(int? x) { int?[] v = []; v.push(x); }
		main() { int? o = 5; f(o); }
	`)
	// extractFunction (not extractFunc): @__user.f's signature contains a
	// `{ i1, i64 }` struct param, which breaks extractFunc's brace-depth
	// walk. extractFunction terminates on `\n}\n` and the @__user.f
	// definition textually precedes its call site, so it is correct here.
	body := extractFunction(ir, "__user.f")
	if body == "" {
		t.Fatalf("expected define @__user.f in IR:\n%s", ir)
	}
	// x : int? is Identical to the int? element type → wrap must be skipped.
	if strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("already-optional push arg must NOT be re-wrapped (Identical guard):\n%s", body)
	}
	// The loaded {i1,i64} value is still stored into the push arg alloca.
	if !strings.Contains(body, "store { i1, i64 }") {
		t.Fatalf("expected a `store { i1, i64 }` into the push arg alloca:\n%s", body)
	}
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("push arg alloca received a bare i64 store (pre-T0658 panic shape):\n%s", body)
	}
}

// TestT0658_PushBareIntoGenericOptionalVector — the c.typeSubst path: inside
// the monomorphized `_push[int]` body, `v.push(x)` where x : T (= int) and
// the slot is T? (= int?) must wrap the bare arg under the substitution of
// resolvedElem / argExprType. Pre-fix this panicked in the mono'd body.
func TestT0658_PushBareIntoGenericOptionalVector(t *testing.T) {
	ir := generateIR(t, `
		_push[T](~T x) int { T?[] v = []; v.push(x); return v.len; }
		main() { int n = _push[int](7); }
	`)
	body := extractFunc(ir, `"_push[int]"`)
	if body == "" {
		t.Fatalf("expected monomorphized define @\"_push[int]\" in IR:\n%s", ir)
	}
	if !strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("expected wrapOptional aggregate for bare-T push into T?[] in mono'd body:\n%s", body)
	}
	if !strings.Contains(body, "store { i1, i64 }") {
		t.Fatalf("expected a `store { i1, i64 }` into the push arg alloca in mono'd body:\n%s", body)
	}
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("push arg alloca received a bare i64 store in mono'd body (pre-T0658 panic shape):\n%s", body)
	}
}
