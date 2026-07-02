package codegen

import (
	"strings"
	"testing"
)

// T1184: a function that returns a *borrowed* (default read-only, non-`~`)
// fixed-array VALUE parameter by value (`echo(string[2] a) string[2] { return a; }`)
// handed back an [N x T] aggregate whose element pointers ALIAS the caller's heap
// allocations — the caller keeps ownership of the borrow, so both the returned copy
// and the caller freed the same elements → double-free / UAF. Fixed arrays lacked
// the string/vector return-alias-dup, and no ownership rejection covered the shape.
//
// The fix broadens dupBorrowedHeapUserPayload's entry gate to also recognize a
// borrowed fixed-array value param (borrowedArrayParamEscapeDup: borrowedValueParams
// membership + arrayElemNeedsEscapeDup), routing it through dupArrayValueForEscape so
// the escaped array element-wise deep-clones its elements. For a string[N] param each
// element lowers to a promise_string_new (via dupString) so the returned array owns
// independent strings; the caller keeps and drops its originals exactly once.
//
// Runtime zero-leak / no-double-free behavior is covered by the e2e batch tests in
// tests/arrays/fixed_return_borrow_test.pr under the zero-tolerance leak gate.

// TestT1184_BorrowedStringArrayReturnDeepClones — `return a` where `a` binds a
// borrowed string[2] param must element-wise deep-clone: N promise_string_new calls
// (one per array element) rebuilding the returned aggregate with insertvalue.
func TestT1184_BorrowedStringArrayReturnDeepClones(t *testing.T) {
	ir := generateIR(t, `
		echo(string[2] a) string[2] { return a; }
		main() { string[2] r = ["a" + "1", "b" + "2"]; string[2] c = echo(r); }
	`)
	fn := extractFunc(ir, "__user.echo")
	if n := strings.Count(fn, "@promise_string_new"); n < 2 {
		t.Fatalf("expected >=2 promise_string_new (one per string[2] element), got %d\n%s", n, fn)
	}
	if n := strings.Count(fn, "insertvalue [2 x i8*]"); n < 2 {
		t.Fatalf("expected >=2 insertvalue rebuilding the cloned aggregate, got %d\n%s", n, fn)
	}
}

// TestT1184_OwnedLocalArrayReturnNoDup — returning an OWNED local array (not a
// borrowed param) must NOT dup: the move-out clears the local's drop flag and the
// caller takes ownership, so no per-element clone is emitted on the return path.
func TestT1184_OwnedLocalArrayReturnNoDup(t *testing.T) {
	ir := generateIR(t, `
		mk() string[2] { return ["a" + "1", "b" + "2"]; }
		passthru() string[2] { string[2] tmp = mk(); return tmp; }
		main() { string[2] r = passthru(); }
	`)
	fn := extractFunc(ir, "__user.passthru")
	// The array-literal build in mk() dups; passthru only moves the owned local out,
	// so its own body emits no additional per-element clone for the `return tmp`.
	if strings.Contains(fn, "@promise_string_new") {
		t.Fatalf("owned-local array return must not deep-clone on return, but promise_string_new appears:\n%s", fn)
	}
}

// TestT1184_BorrowedNonHeapArrayReturnNoDup — a borrowed fixed-array param whose
// elements are plain value/copy types (int[2]) must NOT dup on return: the
// [N x T] aggregate is a bit-copy and both sides own it independently by
// construction. This exercises the arrayElemNeedsEscapeDup-false branch of
// borrowedArrayParamEscapeDup (borrowed param, but no heap-aliasing element), which
// must leave the value untouched — no per-element clone, no aggregate rebuild.
func TestT1184_BorrowedNonHeapArrayReturnNoDup(t *testing.T) {
	ir := generateIR(t, `
		echo(int[2] a) int[2] { return a; }
		main() { int[2] r = [1, 2]; int[2] c = echo(r); }
	`)
	fn := extractFunc(ir, "__user.echo")
	if strings.Contains(fn, "insertvalue [2 x i64]") {
		t.Fatalf("borrowed non-heap int[2] return must not rebuild the aggregate (no escape-dup), but insertvalue appears:\n%s", fn)
	}
}
