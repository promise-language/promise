package regress11

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1907: `promise_panic` sets a TLS flag and RETURNS, so every caller owes a flag
// check before it looks at the result. genExpr emits one after each `ast.CallExpr`
// (T0147), but a getter read, a setter write, an index assignment and a user-defined
// operator are calls that are not CallExprs — their callees' panics were swallowed,
// the caller computed on a zero return, and the next real panic reported
// `fatal: panic during panic recovery` from the wrong site.

// panicChecksIn counts the flag loads inside one function body.
func panicChecksIn(t *testing.T, ir, fn string) int {
	t.Helper()
	body := codegentest.FindDefinedFunc(ir, fn)
	if body == "" {
		t.Fatalf("no function %s in IR", fn)
	}
	return strings.Count(body, "@__promise_panic_flag")
}

func TestT1907_GetterReadIsFollowedByAPanicCheck(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Cell { int n; get val int => this.n; }
		read(Cell c) int { return c.val; }
		main() { int x = read(Cell(n: 1)); }
	`)
	if panicChecksIn(t, ir, "@__user.read(") == 0 {
		t.Fatalf("a getter read must be followed by a panic-flag check")
	}
}

func TestT1907_VirtualGetterReadIsFollowedByAPanicCheck(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type HasVal `+"`"+`structural { get val int `+"`"+`abstract; }
		type Cell { int n; get val int => this.n; }
		read(HasVal h) int { return h.val; }
		main() { int x = read(Cell(n: 1)); }
	`)
	body := codegentest.FindDefinedFunc(ir, "@__user.read(")
	if !strings.Contains(body, "@__promise_panic_flag") {
		t.Fatalf("a vtable getter read must be followed by a panic-flag check, got:\n%s", body)
	}
}

func TestT1907_UserOperatorIsFollowedByAPanicCheck(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Adder {
			int n;
			+(Adder other) Adder { return Adder(n: this.n + other.n); }
		}
		add(Adder a, Adder b) Adder { return a + b; }
		main() { Adder c = add(Adder(n: 1), Adder(n: 2)); }
	`)
	if panicChecksIn(t, ir, "@__user.add(") == 0 {
		t.Fatalf("a user-defined operator must be followed by a panic-flag check")
	}
}

func TestT1907_SetterWriteIsFollowedByAPanicCheck(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Cell { int n; get val int => this.n; set val(int v) { this.n = v; } }
		write(Cell ~c) { c.val = 9; }
		main() { write(Cell(n: 1)); }
	`)
	if panicChecksIn(t, ir, "@__user.write(") == 0 {
		t.Fatalf("a setter write must be followed by a panic-flag check")
	}
}

// A native operator has no callee — nothing can set the flag between the operands and
// the result — so it must NOT pay for a check.
func TestT1907_NativeOperatorGetsNoPanicCheck(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		add(int a, int b) int { return a + b; }
		main() { int x = add(1, 2); }
	`)
	if panicChecksIn(t, ir, "@__user.add(") != 0 {
		t.Fatalf("a native operator has no callee and must not emit a panic check")
	}
}

// The T1881 shape the item singles out: a slot nothing could fill panics, and the
// check at the call site is what lets that message be reported instead of being
// swallowed into a double panic.
func TestT1907_StubPanicReachesTheCallSiteCheck(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Trimmable `+"`"+`structural { trim() string `+"`"+`abstract; }
		call(Trimmable t) string { return t.trim(); }
		main() { string s = call("abc"); }
	`)
	if !strings.Contains(ir, "view_stub_as_Trimmable") {
		t.Fatalf("expected an unfillable slot to keep its panicking stub")
	}
	body := codegentest.FindDefinedFunc(ir, "@__user.call(")
	if !strings.Contains(body, "@__promise_panic_flag") {
		t.Fatalf("the stub's panic must be checked at the call site, got:\n%s", body)
	}
}
