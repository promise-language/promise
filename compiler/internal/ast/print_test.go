package ast

import (
	"strings"
	"testing"
)

// printFile renders a source file's AST through Print and returns the text.
func printFile(t *testing.T, src string) string {
	t.Helper()
	var b strings.Builder
	Print(&b, parseAndBuild(t, src))
	return b.String()
}

func assertPrints(t *testing.T, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Errorf("Print output missing %q; got:\n%s", want, out)
	}
}

// TestPrintFunctionTypeRef covers typeRefStr's FunctionTypeRef case, which
// renders the T1634 `!` prefix. The marker sits on the producer — `!(int) -> int`
// — so dropping it from the printer would make a failable and a non-failable
// callback print identically in `promise ast` output and in every AST dump used
// to diagnose one.
func TestPrintFunctionTypeRef(t *testing.T) {
	out := printFile(t, `
		apply(!(int) -> int fn, (int) -> void cb, () -> void n) {}
	`)
	assertPrints(t, out, "!(int) -> int fn")
	assertPrints(t, out, "(int) -> void cb")
	assertPrints(t, out, "() -> void n")
	// The non-failable forms must not pick up a stray `!`.
	if strings.Contains(out, "!(int) -> void") {
		t.Errorf("non-failable callback printed with a `!` prefix:\n%s", out)
	}
}

// A failable function type in return position: the `!` binds to the type, not to
// the declaration, so `maker() !(int) -> int` prints as a non-failable function
// whose *return type* carries the marker.
func TestPrintFunctionTypeRefInReturnPosition(t *testing.T) {
	out := printFile(t, `
		maker() !(int) -> int { return boom; }
	`)
	assertPrints(t, out, "Func maker()")
	assertPrints(t, out, "Returns !(int) -> int")
}

func TestPrintFunctionTypeRefInFieldPosition(t *testing.T) {
	out := printFile(t, `
		type Holder { !(string) -> int op; (int) -> void cb; }
	`)
	assertPrints(t, out, "Field !(string) -> int op")
	assertPrints(t, out, "Field (int) -> void cb")
}

// Nested function types round-trip through the printer: the inner type keeps its
// own marker, and multi-parameter lists stay comma-separated.
func TestPrintNestedFunctionTypeRef(t *testing.T) {
	out := printFile(t, `
		higher((!(int) -> int, int) -> int h, ((int) -> void) -> void v) {}
	`)
	assertPrints(t, out, "(!(int) -> int, int) -> int h")
	assertPrints(t, out, "((int) -> void) -> void v")
}
