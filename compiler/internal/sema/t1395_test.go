package sema

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1395: parameter defaults are type-checked at their declaration, not only at
// call sites that omit the argument. Codegen needs a recorded type for every
// default it may have to materialize — including a default that no call site
// ever omits, which is exactly the shape produced when a concrete method
// satisfies a structural interface through EXTRA defaulted params.

// TestT1395_ParamDefaultTypeRecordedWithoutCallSite — a default that is never
// omitted by any call site must still end up in Info.Types, or codegen panics
// when the generic structural dispatch fills it in.
func TestT1395_ParamDefaultTypeRecordedWithoutCallSite(t *testing.T) {
	info := checkOK(t, `
		widen(int x, int extra = 2 * 3) int { return x + extra; }
		main() { widen(1, 2); }
	`)
	var found bool
	for param, expr := range info.ParamDefaults {
		if param.Name() != "extra" {
			continue
		}
		found = true
		if typ := info.Types[expr]; typ == nil {
			t.Errorf("default expression for 'extra' has no recorded type")
		} else if typ.String() != "int" {
			t.Errorf("default expression for 'extra' typed %s, want int", typ)
		}
	}
	if !found {
		t.Fatalf("no ParamDefaults entry for 'extra'")
	}
}

// TestT1395_ParamDefaultTypeMismatchReported — a default whose type does not fit
// its parameter is reported at the declaration, even when no call omits it.
// Before defaults were checked at their declaration this went unreported.
func TestT1395_ParamDefaultTypeMismatchReported(t *testing.T) {
	errs := checkErrs(t, `
		bad(int x = "str") int { return x; }
		main() { bad(1); }
	`)
	expectError(t, errs, "cannot use default value of type string for parameter 'x' of type int")
}

// TestT1395_ContextSensitiveDefaultNotRejectedAtDeclaration — a default is
// evaluated in the CALLER's context, so context-sensitive diagnostics belong to
// the call site. `mk()?^` propagates from the failable caller that omits the
// argument; the declaration pre-pass must not reject it.
func TestT1395_ContextSensitiveDefaultNotRejectedAtDeclaration(t *testing.T) {
	errs := checkErrs(t, `
		mk!() int { return 5; }
		f!(int x = mk()?^) int { return x; }
		g!() int { return f(); }
		main() { }
	`)
	expectNoErrors(t, errs)
}

// TestT1395_GenericParamDefaultSkipped — a plain (non-type-param-typed) default
// on a GENERIC function is still checked normally at its declaration.
func TestT1395_GenericParamDefaultSkipped(t *testing.T) {
	errs := checkErrs(t, `
		pick[T](T a, int n = 1) T { return a; }
		main() { pick[int](3); }
	`)
	expectNoErrors(t, errs)
}

// TestT1395_TypeParamTypedDefaultSkipped — a default whose PARAMETER TYPE
// mentions a type param cannot be checked in file scope (the type param is not
// in scope there), so the declaration pre-pass skips it rather than reporting a
// bogus error. This is the `ContainsTypeParam` guard in checkParamDefaults;
// TestT1395_GenericParamDefaultSkipped above does not reach it, since `int n`
// contains no type param.
func TestT1395_TypeParamTypedDefaultSkipped(t *testing.T) {
	errs := checkErrs(t, `
		pick[T](T a, T? alt = none) T { return a; }
		main() { pick[int](3); }
	`)
	expectNoErrors(t, errs)
}

// TestT1395_MethodParamDefaultTypeMismatchReported — method signatures record
// their defaults through the same recordParamDefault path as free functions
// (resolveMethodSignature), so a bad default on a METHOD is reported at its
// declaration too.
func TestT1395_MethodParamDefaultTypeMismatchReported(t *testing.T) {
	errs := checkErrs(t, `
		type Widget {
		  int v;
		  bump(this, int by = "str") int { return this.v + by; }
		}
		main() { }
	`)
	expectError(t, errs, "cannot use default value of type string for parameter 'by' of type int")
}

// TestT1395_EnumMethodParamDefaultTypeMismatchReported — same for enum methods
// (resolveEnumMethodSignature), the third recordParamDefault call site.
func TestT1395_EnumMethodParamDefaultTypeMismatchReported(t *testing.T) {
	errs := checkErrs(t, `
		enum Level {
		  Low, High,

		  rank(this, int base = "str") int { return base; }
		}
		main() { }
	`)
	expectError(t, errs, "cannot use default value of type string for parameter 'base' of type int")
}

// TestT1395_MethodParamDefaultTypeRecordedWithoutCallSite — the codegen-facing
// half for methods: a method default that NO call site ever omits must still be
// typed, because the only place it is materialized may be a synthesized
// dispatch (a generic structural call or a view adapter) with no AST args.
func TestT1395_MethodParamDefaultTypeRecordedWithoutCallSite(t *testing.T) {
	info := checkOK(t, `
		type Widget {
		  int v;
		  bump(this, int by = 2 * 3) int { return this.v + by; }
		}
		main() { Widget w = Widget(v: 1); w.bump(1); }
	`)
	var found bool
	for param, expr := range info.ParamDefaults {
		if param.Name() != "by" {
			continue
		}
		found = true
		if typ := info.Types[expr]; typ == nil {
			t.Errorf("default expression for 'by' has no recorded type")
		} else if typ.String() != "int" {
			t.Errorf("default expression for 'by' typed %s, want int", typ)
		}
	}
	if !found {
		t.Fatalf("no ParamDefaults entry for 'by'")
	}
}

// TestT1395_DefaultReferencingAnotherParamStillErrorsAtCallSite — the
// declaration pre-pass discards its own diagnostics (a default is evaluated in
// the CALLER's context), which must not swallow a genuinely bad default. `a` is
// not in file scope, so the call site that omits `b` still reports it.
func TestT1395_DefaultReferencingAnotherParamStillErrorsAtCallSite(t *testing.T) {
	errs := checkErrs(t, `
		f(int a, int b = a) int { return a + b; }
		main() { f(1); }
	`)
	if len(errs) == 0 {
		t.Fatalf("expected an error for a default referencing another parameter")
	}
}

// TestT1395_DefaultArgExprNilWithoutDefault — DefaultArgExpr is the single
// lookup shared by sema's call-arg resolution and codegen's default filling, and
// codegen calls it unguarded on every trailing param. A param with no default
// must yield nil rather than a stale/absent map hit.
func TestT1395_DefaultArgExprNilWithoutDefault(t *testing.T) {
	info := &Info{ParamDefaults: map[*types.Param]ast.Expr{}}
	p := types.NewParam("x", types.TypInt, types.RefNone)
	if got := info.DefaultArgExpr(p); got != nil {
		t.Errorf("DefaultArgExpr on a param without a default = %v, want nil", got)
	}

	// With a default recorded only on the param (the cross-module shape — the
	// declaring module's ParamDefaults is not this Info's map), the fallback
	// must still find it.
	lit := &ast.IntLit{Raw: "7"}
	p.SetHasDefault(true)
	p.SetDefaultExpr(lit)
	if got := info.DefaultArgExpr(p); got != ast.Expr(lit) {
		t.Errorf("DefaultArgExpr cross-module fallback = %v, want the recorded literal", got)
	}
}
