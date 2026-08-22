package sema

import (
	"testing"
)

// T1634: `void` is the canonical (and only) spelling for "returns nothing", and
// it now has a single internal representation — `NewSignature` normalizes a
// TypVoid result to nil. Before that, a `(int) -> void` annotation resolved to a
// nil-result signature while an expression-body lambda inferred a TypVoid-result
// one, so the two compared unequal and the annotation was uninhabitable.

func TestVoidFunctionTypeAcceptsExprBodyLambda(t *testing.T) {
	checkOK(t, `
		apply(int x, (int) -> void fn) { fn(x); }
		main() { apply(3, |int y| -> print_line("{y}")); }
	`)
}

func TestVoidFunctionTypeAcceptsExplicitVoidBlockLambda(t *testing.T) {
	checkOK(t, `
		apply(int x, (int) -> void fn) { fn(x); }
		main() { apply(3, |int y| -> void { print_line("{y}"); }); }
	`)
}

func TestVoidFunctionTypeAcceptsArrowlessBlockLambda(t *testing.T) {
	checkOK(t, `
		apply(int x, (int) -> void fn) { fn(x); }
		main() { apply(3, |int y| { print_line("{y}"); }); }
	`)
}

// The defect was never lambda-only: a declared function with an explicit `void`
// return resolved to a TypVoid-result signature too, so `() -> void a = foo;`
// failed with "cannot assign () -> void to variable of type ()".
func TestVoidFunctionTypeAcceptsDeclaredVoidFunction(t *testing.T) {
	checkOK(t, `
		foo() void { print_line("foo"); }
		main() { () -> void a = foo; a(); }
	`)
}

// A function with no declared return type must satisfy the same annotation —
// both spellings produce the same nil-result signature.
func TestVoidFunctionTypeAcceptsBareFunction(t *testing.T) {
	checkOK(t, `
		foo() { print_line("foo"); }
		main() { () -> void a = foo; a(); }
	`)
}

func TestVoidFunctionTypeInEveryPosition(t *testing.T) {
	checkOK(t, `
		type Holder { (int) -> void cb; }
		make() (int) -> void { return |int y| -> print_line("{y}"); }
		main() {
			(int) -> void local = |int y| -> print_line("{y}");
			local(1);
			h := Holder(cb: |int y| -> print_line("{y}"));
			h.cb(2);
			m := make();
			m(3);
		}
	`)
}

// A value-returning lambda still must not satisfy a void annotation.
func TestVoidFunctionTypeRejectsValueLambda(t *testing.T) {
	errs := checkErrs(t, `
		apply(int x, (int) -> void fn) { fn(x); }
		main() { apply(3, |int y| -> y * 2); }
	`)
	expectError(t, errs, "(int) -> int")
}

// The diagnostic used to render `(int) -> void` as the bare `(int)`, which reads
// as a tuple and hid the real mismatch (T1634); `Signature.String()` now always
// emits the arrow and spells a nil result `void`.
func TestVoidFunctionTypeRendersInDiagnostic(t *testing.T) {
	errs := checkErrs(t, `
		apply(int x, (int) -> void fn) { fn(x); }
		main() { apply(3, 7); }
	`)
	expectError(t, errs, "(int) -> void")
}

// T1634 / D: `!(…) -> T` — the failable function-type notation from §9.6.
// Before this, no failable function type could be written at all (`missing ')'
// at '!'`), so no callback parameter could accept a failable function.

func TestFailableFunctionTypeAcceptsFailableFunction(t *testing.T) {
	checkOK(t, `
		boom!(int x) int { return x * 2; }
		apply!(!(int) -> int fn, int x) int { return fn(x)?^; }
		main!() { int r = apply(boom, 3)?^; }
	`)
}

func TestFailableFunctionTypeInEveryPosition(t *testing.T) {
	checkOK(t, `
		boom!(int x) int { return x * 2; }
		type Holder { !(int) -> int op; }
		maker() !(int) -> int { return boom; }
		main!() {
			!(int) -> int local = boom;
			int a = local(1)?^;
			h := Holder(op: boom);
			int b = h.op(2)?^;
			m := maker();
			int c = m(3)?^;
		}
	`)
}

// `!() -> void` — failable with nothing to return.
func TestFailableFunctionTypeVoidResult(t *testing.T) {
	checkOK(t, `
		vboom!() { }
		main!() {
			!() -> void v = vboom;
			v()?^;
		}
	`)
}

// Failability is part of signature identity in both directions.
func TestFailableFunctionTypeRejectsNonFailableLambda(t *testing.T) {
	errs := checkErrs(t, `
		main() { !(int) -> int f = |int x| -> x * 2; }
	`)
	expectError(t, errs, "cannot assign (int) -> int to variable of type !(int) -> int")
}

func TestNonFailableFunctionTypeRejectsFailableFunction(t *testing.T) {
	errs := checkErrs(t, `
		boom!(int x) int { return x; }
		main() { (int) -> int f = boom; }
	`)
	expectError(t, errs, "cannot assign !(int) -> int to variable of type (int) -> int")
}

// --- Error paths in function-type resolution ---
//
// `resolveType` used to short-circuit a `void` return before recursing, so the
// return-type branch was only ever reached for resolvable types. Now every
// return type resolves, and an unresolvable one must produce a diagnostic
// instead of a nil signature that later code dereferences.

func TestFunctionTypeUndefinedReturnType(t *testing.T) {
	errs := checkErrs(t, `
		apply((int) -> Unknown fn) { }
		main() { }
	`)
	expectError(t, errs, "undefined type: Unknown")
}

func TestFunctionTypeUndefinedParamType(t *testing.T) {
	errs := checkErrs(t, `
		apply((Unknown) -> int fn) { }
		main() { }
	`)
	expectError(t, errs, "undefined type: Unknown")
}

func TestFailableFunctionTypeUndefinedParamType(t *testing.T) {
	errs := checkErrs(t, `
		apply(!(Unknown) -> int fn) { }
		main() { }
	`)
	expectError(t, errs, "undefined type: Unknown")
}

func TestFailableFunctionTypeUndefinedReturnType(t *testing.T) {
	errs := checkErrs(t, `
		apply(!(int) -> Unknown fn) { }
		main() { }
	`)
	expectError(t, errs, "undefined type: Unknown")
}

// --- Nested and higher-order function types ---
//
// A function type is itself a type, so it may appear inside another one. The
// `!` prefix must bind to the inner producer, not leak outward.

func TestNestedVoidFunctionType(t *testing.T) {
	checkOK(t, `
		call_it((int) -> void f) { f(1); }
		apply_void(((int) -> void) -> void h) { h(|int y| -> print_line("{y}")); }
		main() { apply_void(call_it); }
	`)
}

func TestFailableFunctionTypeAsParamOfFunctionType(t *testing.T) {
	checkOK(t, `
		boom!(int x) int { return x * 2; }
		run_with(!(int) -> int fn, int x) int { return fn(x)? e { return -1; }; }
		higher((!(int) -> int, int) -> int h) int { return h(boom, 5); }
		main() { print_line("{higher(run_with)}"); }
	`)
}

// A void function type as a generic type argument — `Map[string, (int) -> void]`
// substitutes the signature through `types.Substitute`, another `NewSignature`
// construction site that must agree on the nil-result representation.
func TestVoidFunctionTypeAsGenericTypeArgument(t *testing.T) {
	checkOK(t, `
		main() {
			Map[string, (int) -> void] m = Map[string, (int) -> void]();
			m["a"] = |int y| -> print_line("{y}");
			m["a"]!(1);
		}
	`)
}

func TestFailableFunctionTypeAsGenericTypeArgument(t *testing.T) {
	checkOK(t, `
		boom!(int x) int { return x * 2; }
		main!() {
			(!(int) -> int)[] fns = [boom];
			int r = fns[0](3)?^;
		}
	`)
}

// --- Optionals of function types ---

func TestOptionalVoidFunctionType(t *testing.T) {
	checkOK(t, `
		main() {
			((int) -> void)? o = |int y| -> print_line("{y}");
			o!(3);
		}
	`)
}

func TestOptionalFailableFunctionType(t *testing.T) {
	checkOK(t, `
		boom!(int x) int { return x * 2; }
		main!() {
			(!(int) -> int)? o = boom;
			int r = o!(6)?^;
		}
	`)
}

// A failable function type is not satisfied by a non-failable *named function*
// either — the check is on the signature, not on how the value was written.
func TestFailableFunctionTypeRejectsNonFailableFunctionRef(t *testing.T) {
	errs := checkErrs(t, `
		plain(int x) int { return x; }
		main() { !(int) -> int f = plain; }
	`)
	expectError(t, errs, "cannot assign (int) -> int to variable of type !(int) -> int")
}

// The void annotation is not satisfied by a value-returning *named function*.
func TestVoidFunctionTypeRejectsValueFunctionRef(t *testing.T) {
	errs := checkErrs(t, `
		valued(int x) int { return x; }
		main() { (int) -> void f = valued; }
	`)
	expectError(t, errs, "(int) -> void")
}
