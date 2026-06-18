package sema

import "testing"

// T0966: a bare auto-propagating failable call (`name!` written without an
// explicit `?^`/`?!`) used directly inside a string-interpolation slot must be
// treated like any other failable sub-expression: auto-propagated in a failable
// function, and rejected with "failable call must be handled" in a non-failable
// one. Previously sema skipped the failable check for interpolation operands,
// so the bare form silently slipped through to codegen and produced a wrong
// value (zero for int) or a runtime crash (stack overflow for string).

func TestBareFailableInInterpolationNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
twice!(int n) int { return n * 2; }
caller() string { return "v={twice(21)}"; }
main() {}
`)
	expectError(t, errs, "failable call must be handled")
}

func TestBareFailableStringInInterpolationNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
greet!(string n) string { return n; }
caller() string { return "v={greet("x")}"; }
main() {}
`)
	expectError(t, errs, "failable call must be handled")
}

func TestBareFailableInInterpolationFailableOK(t *testing.T) {
	errs := checkErrs(t, `
twice!(int n) int { return n * 2; }
caller!() string { return "v={twice(21)}"; }
main() {}
`)
	expectNoErrors(t, errs)
}

// A non-failable call in an interpolation slot inside a non-failable function
// must remain valid — the new failable sub-check must not over-trigger.
func TestNonFailableCallInInterpolationOK(t *testing.T) {
	errs := checkErrs(t, `
twice(int n) int { return n * 2; }
caller() string { return "v={twice(21)}"; }
main() {}
`)
	expectNoErrors(t, errs)
}
