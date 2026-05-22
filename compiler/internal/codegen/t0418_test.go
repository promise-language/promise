package codegen

import (
	"strings"
	"testing"
)

// T0418: wrapOptional panicked for generic call returning T?? of heap user type.
// Root cause: coerceCallArgs only applied c.typeSubst, not the call's own
// type-arg substitution. After T → _Box, paramType stayed as T? (TypeParam),
// resolveType(T?) fell through to {i1, i8*}, and wrapOptional panicked when
// asked to insertvalue a {i1, {i8*, i8*}} into an i8* slot.
//
// Fix: thread a per-call subst map (callSubst) into coerceCallArgs so paramType
// resolves correctly at the call site.

// TestT0418_GenericTOptArgWrappedToTDoubleOpt — the original repro: a generic
// function with `T? a` param returning `T??` should NOT panic at codegen and
// should produce a correct nested-Optional wrap inside the function body.
func TestT0418_GenericTOptArgWrappedToTDoubleOpt(t *testing.T) {
	ir := generateIR(t, `
		type _Box {
		  int n;
		  drop(~this) {}
		}

		_generic_double[T](T? a) T?? { return a; }

		main() {
		  _Box? a = _Box(n: 9);
		  _Box?? r = _generic_double[_Box](a);
		}
	`)
	body := extractFunction(ir, `"_generic_double[_Box]"`)
	if body == "" {
		t.Fatalf("_generic_double[_Box] not in IR\nfull IR:\n%s", ir)
	}
	// The function takes T? (after subst: { i1, { i8*, i8* } }) and returns T??
	// (after subst: { i1, { i1, { i8*, i8* } } }) — the inner Optional must be
	// the heap-user-type-shaped {i1, {i8*, i8*}}, NOT {i1, i8*}.
	if !strings.Contains(body, "{ i1, { i1, { i8*, i8* } } }") {
		t.Errorf("expected _generic_double[_Box] to return { i1, { i1, { i8*, i8* } } } (T??=_Box?? at heap-user-type shape)\nbody:\n%s", body)
	}
	// And the input parameter struct should be {i1, {i8*, i8*}} (T?=_Box?).
	if !strings.Contains(body, "{ i1, { i8*, i8* } } %a") {
		t.Errorf("expected _generic_double[_Box] to take { i1, { i8*, i8* } } %%a as the T? param\nbody:\n%s", body)
	}
}

// TestT0418_GenericOptionalArgNoSpuriousWrap — when the arg type and the
// substituted param type are identical (both _Box?), no wrapping should
// happen at the call site. This guards against a stray `insertvalue {i1, i8*}`
// caused by the param resolving to the wrong shape.
func TestT0418_GenericOptionalArgNoSpuriousWrap(t *testing.T) {
	ir := generateIR(t, `
		type _Box {
		  int n;
		  drop(~this) {}
		}

		_generic_passthrough[T](T? a) T? { return a; }

		main() {
		  _Box? a = _Box(n: 9);
		  _Box? r = _generic_passthrough[_Box](a);
		}
	`)
	// The function body wraps an i8* into the param Optional structure on
	// return — that's fine. The call site should NOT do any wrapping (no
	// insertvalue for the Optional param). The generated function shape
	// should match the substituted Optional type {i1, {i8*, i8*}}.
	body := extractFunction(ir, `"_generic_passthrough[_Box]"`)
	if body == "" {
		t.Fatalf("_generic_passthrough[_Box] not in IR\nfull IR:\n%s", ir)
	}
	// Param and return must both be {i1, {i8*, i8*}} (single Optional of heap user type).
	if !strings.Contains(body, "{ i1, { i8*, i8* } } @\"_generic_passthrough[_Box]\"({ i1, { i8*, i8* } } %a)") {
		t.Errorf("expected _generic_passthrough[_Box] signature ({i1, {i8*,i8*}}) -> ({i1, {i8*,i8*}})\nbody:\n%s", body)
	}
}

// TestT0418_GenericPrimitiveOptionalArg — same fix for primitive type params.
// Without the fix, `_generic_passthrough[int](a)` produced
// "expected i8*, got {i1, i64}" at codegen.
func TestT0418_GenericPrimitiveOptionalArg(t *testing.T) {
	ir := generateIR(t, `
		_generic_passthrough[T](T? a) T? { return a; }

		main() {
		  int? a = 42;
		  int? r = _generic_passthrough[int](a);
		}
	`)
	body := extractFunction(ir, `"_generic_passthrough[int]"`)
	if body == "" {
		t.Fatalf("_generic_passthrough[int] not in IR\nfull IR:\n%s", ir)
	}
	// Param and return must both be {i1, i64} (Optional of int).
	if !strings.Contains(body, "{ i1, i64 } @\"_generic_passthrough[int]\"({ i1, i64 } %a)") {
		t.Errorf("expected _generic_passthrough[int] signature ({i1, i64}) -> ({i1, i64})\nbody:\n%s", body)
	}
}
