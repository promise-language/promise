package codegen

import (
	"strings"
	"testing"
)

// T0792: a failable `T&`-return consumed via a handler `? e { ... }` whose
// recovery body reads an i8* borrow source (`r.d[0]`) leaked 1 alloc.
//
// genBlockValue decides whether to dup the block's last expression from the
// inner expr's natural sema type. The whole handler expr is `string&`
// (borrow), but the last expr `r.d[0]` is `string` (owned) — so
// setDupFlagsForFieldAccess dup'd the element via promise_string_new. The
// merged phi is typed `string&`, so the `string & x = …` bind site takes no
// ownership → the dup is owned by nobody → leak.
//
// Fix: genErrorHandlerExpr sets c.borrowBlockResult when the recovery result
// type is a ref (`T&`/`T~`); genBlockValue then reads the last expr as a pure
// alias — no dup, no owned-temp tracking.
//
// These tests lock the IR shape (no spurious dup in the borrow case, a real
// dup in the owned case) so the gate is precise and borrow-targeted, failing
// closed on regression. Runtime zero-leak behavior is covered by the e2e
// batch tests in tests/e2e/ref_return_handler_test.pr under the
// zero-tolerance leak gate.

const t0792BorrowSource = `
	type RefRet {
	  string[] d;
	  at!(int i) string & {
	    if i < 0 { raise error("bad"); }
	    return this.d[i];
	  }
	}
	tfn() {
	  r := RefRet(d: ["aa" + "x", "bbb" + "y"]);
	  string & x = r.at(-1)? e { r.d[0] };
	}
	main() { tfn(); }
`

const t0792OwnedSource = `
	type RefRet {
	  string[] d;
	  at!(int i) string {
	    if i < 0 { raise error("bad"); }
	    return this.d[i];
	  }
	}
	tfn() {
	  r := RefRet(d: ["aa" + "x", "bbb" + "y"]);
	  string x = r.at(-1)? e { r.d[0] };
	}
	main() { tfn(); }
`

// TestT0792_HandlerBorrowRecoveryNoDup — the core fix. A `string&` failable
// return whose handler recovery body reads `r.d[0]` must NOT emit a
// promise_string_new in tfn: the borrow bind site takes no ownership, so a dup
// would leak.
func TestT0792_HandlerBorrowRecoveryNoDup(t *testing.T) {
	ir := generateIR(t, t0792BorrowSource)
	fn := extractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatalf("could not extract __user.tfn from IR:\n%s", ir)
	}
	if strings.Contains(fn, "@promise_string_new") {
		t.Fatalf("expected NO `@promise_string_new` in tfn (borrow recovery "+
			"body must alias, not dup), got one:\n%s", fn)
	}
}

// TestT0792_HandlerOwnedRecoveryDups — the contrast / gate. An owned `string`
// return with the same handler recovery body DOES dup (the bind site takes
// ownership), proving the suppression is borrow-targeted and not a blanket
// "never dup in handlers".
func TestT0792_HandlerOwnedRecoveryDups(t *testing.T) {
	ir := generateIR(t, t0792OwnedSource)
	fn := extractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatalf("could not extract __user.tfn from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "@promise_string_new") {
		t.Fatalf("expected `@promise_string_new` in tfn (owned recovery body "+
			"dups so the bind site owns a copy), got none:\n%s", fn)
	}
}

// t0792ElseBorrowSource — a typed handler with an else clause whose else body
// yields a borrow (`r.d[1]`). The raised base `error` never matches `MyErr`, so
// the else body is the live recovery path. The else body's genBlockValue call is
// a SEPARATE code path from the main handler body (genErrorHandlerExpr sets
// borrowBlockResult independently at expr.go:7701), so it needs its own gate.
const t0792ElseBorrowSource = `
	type MyErr is error { int code; }
	type RefRet {
	  string[] d;
	  at!(int i) string & {
	    if i < 0 { raise error("bad"); }
	    return this.d[i];
	  }
	}
	tfn() {
	  r := RefRet(d: ["aa" + "x", "bbb" + "y"]);
	  string & x = r.at(-1)? e is MyErr { r.d[0] } else { r.d[1] };
	}
	main() { tfn(); }
`

// TestT0792_TypedHandlerElseBorrowNoDup — the else-clause borrow body must also
// alias, not dup. The else path is its own borrowBlockResult assignment, so a
// regression there would not be caught by the main-body gate above. Verified
// independently: neutralizing only the else-path assignment makes the matching
// e2e test (at_qhandler_else_borrow_bind) leak 1 alloc.
func TestT0792_TypedHandlerElseBorrowNoDup(t *testing.T) {
	ir := generateIR(t, t0792ElseBorrowSource)
	fn := extractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatalf("could not extract __user.tfn from IR:\n%s", ir)
	}
	if strings.Contains(fn, "@promise_string_new") {
		t.Fatalf("expected NO `@promise_string_new` in tfn (typed-handler else "+
			"borrow body must alias, not dup), got one:\n%s", fn)
	}
}
