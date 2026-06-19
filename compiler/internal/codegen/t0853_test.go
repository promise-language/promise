package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T0853: constructing an Ref[T?] / Mutex[T?] with a bare (non-optional) value
// of type T (or a bare `none`).
//
// Pre-fix, genArcConstructor / genMutexConstructor lowered the constructor
// argument with genCallArgExpr and stored the raw scalar/pointer straight into
// the {i1, T} element slot, panicking inside generateIR:
//   "store operands are not compatible: src={ i8*, i8* }; dst={ i1, { i8*, i8* } }*"
// `Ref[T?](none)` was the same panic shape — c.targetType was never set so
// genNoneLit returned the i1 0 "void optional fallback" instead of a zero
// {i1,T} struct.
//
// The fix (the Arc/Mutex-constructor analog of T0658 / genVectorMethodCall's
// push) sets c.targetType to the resolved Optional element type around argument
// generation and widens a bare arg via wrapReturnOptional (which no-ops for
// `none` and for an already-optional arg, else wrapOptional).

// TestT0853_ArcBareIntoOptionalWraps — `Ref[int?](5)` must build a {i1,i64}
// aggregate (wrapOptional) and store THAT into the Arc value slot; the
// bare-i64-store panic shape must be gone. `Ref[int?](none)` must lower to a
// zeroinitializer {i1,i64} store (genNoneLit via the newly-set c.targetType),
// not the i1 0 fallback.
func TestT0853_ArcBareIntoOptionalWraps(t *testing.T) {
	ir := generateIR(t, `main() { Ref[int?] a = Ref[int?](5); Ref[int?] b = Ref[int?](none); }`)
	body := extractFunc(ir, ".goroutine.main")
	if body == "" {
		t.Fatalf("expected define @.goroutine.main in IR:\n%s", ir)
	}
	// `Ref[int?](5)` → wrapOptional builds the {i1,i64} aggregate.
	if !strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("expected wrapOptional aggregate for bare-int Ref[int?](5):\n%s", body)
	}
	// The wrapped aggregate is stored into the Arc value slot.
	if !strings.Contains(body, "store { i1, i64 }") {
		t.Fatalf("expected a `store { i1, i64 }` into the Arc value slot:\n%s", body)
	}
	// Pre-fix panic shape: a bare i64 stored into a {i1,i64}* slot. Must NOT appear.
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("Arc value slot received a bare i64 store (pre-T0853 panic shape):\n%s", body)
	}
	// `Ref[int?](none)` → genNoneLit under the newly-set c.targetType emits a
	// zeroinitializer {i1,i64} (NOT the `i1 0` void-optional fallback).
	if !strings.Contains(body, "store { i1, i64 } zeroinitializer") {
		t.Errorf("expected `store { i1, i64 } zeroinitializer` for `Ref[int?](none)`:\n%s", body)
	}
}

// TestT0853_MutexBareIntoOptionalWraps — same as the Arc case for the
// genMutexConstructor path.
func TestT0853_MutexBareIntoOptionalWraps(t *testing.T) {
	ir := generateIR(t, `main() { Mutex[int?] a = Mutex[int?](5); Mutex[int?] b = Mutex[int?](none); }`)
	body := extractFunc(ir, ".goroutine.main")
	if body == "" {
		t.Fatalf("expected define @.goroutine.main in IR:\n%s", ir)
	}
	if !strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("expected wrapOptional aggregate for bare-int Mutex[int?](5):\n%s", body)
	}
	if !strings.Contains(body, "store { i1, i64 }") {
		t.Fatalf("expected a `store { i1, i64 }` into the Mutex value slot:\n%s", body)
	}
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("Mutex value slot received a bare i64 store (pre-T0853 panic shape):\n%s", body)
	}
	if !strings.Contains(body, "store { i1, i64 } zeroinitializer") {
		t.Errorf("expected `store { i1, i64 } zeroinitializer` for `Mutex[int?](none)`:\n%s", body)
	}
}

// TestT0853_ArcExplicitOptionalNoDoubleWrap — regression guard: constructing
// Ref[int?] from an already-typed `int?` must NOT re-wrap (skipped via the
// types.Identical short-circuit in wrapReturnOptional). `x` is a parameter (no
// literal→optional widening in the body) so ANY wrap aggregate would be an
// erroneous double-wrap of `x`.
func TestT0853_ArcExplicitOptionalNoDoubleWrap(t *testing.T) {
	ir := generateIR(t, `
		f(int? x) { Ref[int?] a = Ref[int?](x); }
		main() { int? o = 5; f(o); }
	`)
	body := extractFunction(ir, "__user.f")
	if body == "" {
		t.Fatalf("expected define @__user.f in IR:\n%s", ir)
	}
	// x : int? is Identical to the int? element type → wrap must be skipped.
	if strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("already-optional Arc arg must NOT be re-wrapped (Identical guard):\n%s", body)
	}
	if !strings.Contains(body, "store { i1, i64 }") {
		t.Fatalf("expected a `store { i1, i64 }` into the Arc value slot:\n%s", body)
	}
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("Arc value slot received a bare i64 store (pre-T0853 panic shape):\n%s", body)
	}
}

// TestT0853_MutexExplicitOptionalNoDoubleWrap — Mutex counterpart of the Arc
// no-double-wrap guard. The Mutex constructor reaches wrapReturnOptional through
// an independent call site, so its types.Identical short-circuit needs its own
// regression coverage: an already-typed `int?` arg must NOT be re-wrapped.
func TestT0853_MutexExplicitOptionalNoDoubleWrap(t *testing.T) {
	ir := generateIR(t, `
		f(int? x) { Mutex[int?] m = Mutex[int?](x); }
		main() { int? o = 5; f(o); }
	`)
	body := extractFunction(ir, "__user.f")
	if body == "" {
		t.Fatalf("expected define @__user.f in IR:\n%s", ir)
	}
	// x : int? is Identical to the int? element type → wrap must be skipped.
	if strings.Contains(body, "insertvalue { i1, i64 } undef, i1 true, 0") {
		t.Errorf("already-optional Mutex arg must NOT be re-wrapped (Identical guard):\n%s", body)
	}
	if !strings.Contains(body, "store { i1, i64 }") {
		t.Fatalf("expected a `store { i1, i64 }` into the Mutex value slot:\n%s", body)
	}
	if regexp.MustCompile(`store i64 %\w+, \{ i1, i64 \}\*`).MatchString(body) {
		t.Errorf("Mutex value slot received a bare i64 store (pre-T0853 panic shape):\n%s", body)
	}
}
