package sema

import (
	"strings"
	"testing"
)

// T0866: bare `{}` in value position is rejected with a guiding error pointing
// to the empty-collection literals `{:}` / `[]` and `Set[T]()` — instead of the
// raw ANTLR parse-failure cascade it used to produce.

func TestT0866EmptyBraceInVarDeclRejected(t *testing.T) {
	errs := checkErrs(t, `
		f() {
			map[string, int] m = {};
		}
	`)
	expectError(t, errs, "`{}` is not a valid empty-collection literal")
	expectError(t, errs, "empty map: `{:}`")
}

func TestT0866EmptyBraceInInferredDeclRejected(t *testing.T) {
	errs := checkErrs(t, `
		f() {
			x := {};
		}
	`)
	expectError(t, errs, "`{}` is not a valid empty-collection literal")
}

func TestT0866EmptyBraceInReturnRejected(t *testing.T) {
	errs := checkErrs(t, `
		f() map[string, int] {
			return {};
		}
	`)
	expectError(t, errs, "`{}` is not a valid empty-collection literal")
}

// The guiding error replaces the old ANTLR recovery cascade — there must be no
// "no viable alternative" / "mismatched input" noise alongside it.
func TestT0866NoAntlrCascade(t *testing.T) {
	errs := checkErrs(t, `
		f() {
			map[string, int] m = {};
		}
	`)
	for _, e := range errs {
		msg := e.Error()
		if strings.Contains(msg, "no viable alternative") || strings.Contains(msg, "mismatched input") {
			t.Errorf("unexpected ANTLR cascade error: %v", msg)
		}
	}
}

// `{:}` is the real empty-map literal and must keep type-checking cleanly.
func TestT0866EmptyMapLiteralStillValid(t *testing.T) {
	errs := checkErrs(t, `
		f() {
			map[string, int] m = {:};
		}
	`)
	expectNoErrors(t, errs)
}

// `[]` is the empty-vector literal and must keep type-checking cleanly.
func TestT0866EmptyVectorLiteralStillValid(t *testing.T) {
	errs := checkErrs(t, `
		f() {
			int[] v = [];
		}
	`)
	expectNoErrors(t, errs)
}

// An empty `{}` block as a match-arm body is a legitimate empty block, not an
// empty-collection literal — it must still parse and check (grammar prefers the
// block alternative over the new emptyBraceLiteral expression).
func TestT0866EmptyBlockInMatchArmStillValid(t *testing.T) {
	errs := checkErrs(t, `
		f(int n) {
			match n {
				1 => {},
				_ => {},
			}
		}
	`)
	expectNoErrors(t, errs)
}
