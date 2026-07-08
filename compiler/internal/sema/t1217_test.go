package sema

import "testing"

// T1217: a plain `go {}` block body is a non-failable scope (§17.2.1), even when
// the enclosing function is failable. Previously sema scoped failability to the
// enclosing fn, so a bare failable call auto-propagated and `raise` was allowed
// inside a goroutine body — both then panicked codegen (the goroutine's result
// slot is a coroutine/pointer type, not the failable {ok,value,err} aggregate).
// Sema must reject them with the same diagnostics as a non-failable enclosing fn.

func TestT1217GoBlockBareFailableCallRejected(t *testing.T) {
	// Repro 1: bare failable call inside `go {}` in a failable fn must NOT
	// auto-propagate — it must be handled locally.
	errs := checkErrs(t, `
		work!() int { return 1; }
		main!() {
			go {
				n := work();
			};
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1217GoBlockRaiseRejected(t *testing.T) {
	// Repro 2: `raise` inside `go {}` in a failable fn must be rejected.
	errs := checkErrs(t, `
		main!() {
			go { raise error(message: "x"); };
		}
	`)
	expectError(t, errs, "raise outside of failable function")
}

func TestT1217GoBlockErrorPropagateRejected(t *testing.T) {
	// `?^` inside `go {}` in a failable fn must be rejected.
	errs := checkErrs(t, `
		work!() int { return 1; }
		main!() {
			go { n := work()?^; };
		}
	`)
	expectError(t, errs, "used outside of failable function")
}

// --- Positive controls: local handling inside `go {}` is allowed ---

func TestT1217GoBlockPanicOnErrorOK(t *testing.T) {
	// `?!` handles the error locally — accepted even in a failable enclosing fn.
	checkOK(t, `
		work!() int { return 1; }
		main!() {
			go { n := work()?!; };
		}
	`)
}

func TestT1217GoBlockInlineHandlerOK(t *testing.T) {
	// An inline handler handles the error locally — accepted.
	checkOK(t, `
		work!() int { return 1; }
		main!() {
			go { n := work() ? e { 0 }; };
		}
	`)
}

// --- Nesting: a lambda defined inside a `go {}` carries its own signature ---

func TestT1217GoBlockNestedLambdaLocalHandlingOK(t *testing.T) {
	// A lambda inside a `go {}` is governed by its own (non-failable) signature,
	// so `?!` inside it is still accepted — the go-block override is reset for
	// the lambda body (T1217).
	checkOK(t, `
		work!() int { return 1; }
		main!() {
			go {
				f := || -> int { n := work()?!; n };
				f();
			};
		}
	`)
}

func TestT1217GoBlockNestedLambdaBareCallRejected(t *testing.T) {
	// A bare failable call inside the nested lambda must still be rejected — the
	// lambda is non-failable, so it cannot auto-propagate either.
	errs := checkErrs(t, `
		work!() int { return 1; }
		main!() {
			go {
				f := || -> int { n := work(); n };
				f();
			};
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

// --- Distinct gate sites: the non-failable scope must also fire for a bare
// failable call used as a statement (not a var-decl) and for typed-handler
// exhaustiveness — both read canPropagateError() at separate sites (T1217). ---

func TestT1217GoBlockBareCallStatementRejected(t *testing.T) {
	// `checkExprStmtFailable`: a failable call as a bare statement inside `go {}`
	// must be handled locally, not auto-propagated — distinct from the var-decl
	// path in TestT1217GoBlockBareFailableCallRejected.
	errs := checkErrs(t, `
		work!() int { return 1; }
		main!() {
			go { work(); };
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1217GoBlockTypedHandlerNeedsExhaustive(t *testing.T) {
	// `checkErrorHandlerExpr`: a typed handler with no else/`!` relies on the
	// enclosing scope being failable to absorb non-matching errors. Inside a
	// `go {}` the scope is non-failable, so it must be rejected even when the
	// enclosing fn is failable.
	errs := checkErrs(t, `
		type IoError is error { int code; }
		foo!() void { raise IoError(message: "fail", code: 1); }
		main!() {
			go { foo() ? _ is IoError { }; };
		}
	`)
	expectError(t, errs, "typed error handler in non-failable function")
}

func TestT1217GoBlockTypedHandlerElseOK(t *testing.T) {
	// The same typed handler made exhaustive with `else { }` is accepted inside
	// a `go {}` — it no longer needs a failable scope to absorb non-matches.
	checkOK(t, `
		type IoError is error { int code; }
		foo!() void { raise IoError(message: "fail", code: 1); }
		main!() {
			go { foo() ? e is IoError { } else { }; };
		}
	`)
}

// --- Unaffected control: `return` inside a `go {}` validates against the
// enclosing fn's result type (a separate mechanism), and must be untouched. ---

func TestT1217GoBlockReturnUnaffected(t *testing.T) {
	checkOK(t, `
		main!() int {
			go { return 5; };
			return 0;
		}
	`)
}
