package codegen

import (
	"strings"
	"testing"
)

// T1173: a whole fixed-Array-VALUE escape of a match-borrowed variant payload
// whose element aliases heap — string[N] (and Vector[T][N]) — was NOT deep-cloned.
// `return a` / `x = a` copied the [N x T] aggregate by value, aliasing the
// subject's variant-payload element pointers; the subject's synth enum drop frees
// them at scope exit → UAF (string) / double-free (container). The element
// recognizer (arrayHeapDupElem, renamed arrayElemNeedsEscapeDup) only matched
// heap-user elements, so string/container-element arrays fell through to a no-op.
//
// The fix broadens arrayElemNeedsEscapeDup to any heap-aliasing element (reusing
// pushElemNeedsDup + the bare-string case) and routes both escape sinks through
// dupArrayValueForEscape, which element-wise deep-clones the aggregate. For a
// string[N] payload each element lowers to a promise_string_dup (via dupString's
// promise_string_new) so the escaped array owns independent strings; the subject's
// synth enum drop still frees the originals exactly once.
//
// These lock the IR shape; runtime zero-leak/no-UAF behavior is covered by the
// e2e batch tests in tests/e2e/if_is_destructure_escape.pr (T1173 section) under
// the zero-tolerance leak gate.

const t1173Decls = `
	enum StrArr { Pair(string[2] a), Empty, }
`

// TestT1173_IfIsStringArrayReturnDeepClones — `if h is Pair(a) { return a; }` for a
// string[2] payload must element-wise deep-clone: N promise_string_new calls (one
// per array element) and >=2 insertvalue's rebuilding the cloned aggregate. The
// enum's synth drop must still free the original payload once.
func TestT1173_IfIsStringArrayReturnDeepClones(t *testing.T) {
	ir := generateIR(t, t1173Decls+`
		esc() string[2] {
			StrArr h = StrArr.Pair(a: ["x" + "1", "y" + "2"]);
			if h is Pair(a) { return a; }
			return ["", ""];
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	if fn == "" {
		t.Fatalf("could not extract __user.esc from IR:\n%s", ir)
	}
	// dupString allocates each escaping element via promise_string_new — one per
	// array element (2 for a 2-elem string array).
	if n := strings.Count(fn, "@promise_string_new"); n < 2 {
		t.Fatalf("expected >=2 @promise_string_new (one per escaping string[2] element), got %d\n%s", n, fn)
	}
	// One insertvalue per element rebuilds the cloned array aggregate.
	if n := strings.Count(fn, "insertvalue [2 x i8*]"); n < 2 {
		t.Fatalf("expected >=2 insertvalue into the cloned string-array aggregate, got %d\n%s", n, fn)
	}
	// The enum's synth drop must still free the original payload once.
	if !strings.Contains(ir, "@StrArr.drop") {
		t.Fatalf("expected @StrArr.drop (synth enum drop) to still be emitted:\n%s", ir)
	}
}

// TestT1173_MatchStringArrayReturnDeepClones — the match-arm form clones too (both
// paths share matchBorrowedIdents + dupBorrowedHeapUserPayload).
func TestT1173_MatchStringArrayReturnDeepClones(t *testing.T) {
	ir := generateIR(t, t1173Decls+`
		esc() string[2] {
			StrArr h = StrArr.Pair(a: ["m" + "1", "m" + "2"]);
			match h {
				Pair(a) => { return a; },
				Empty => { return ["", ""]; },
			}
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	if fn == "" {
		t.Fatalf("could not extract __user.esc from IR:\n%s", ir)
	}
	if n := strings.Count(fn, "@promise_string_new"); n < 2 {
		t.Fatalf("expected >=2 @promise_string_new (one per escaping string[2] element), got %d\n%s", n, fn)
	}
}

// TestT1173_VectorArrayReturnDeepClones — the container-element branch of
// dupArrayElemForEscape (maybeDupPushElement, NOT the bare-string dupString
// branch). A `int[][2]` (Vector[int][2]) match-borrowed payload escaping via
// `return a` must element-wise shallow-copy each Vector (pal_alloc + memcpy of the
// buffer) and rebuild the [2 x i8*] aggregate — otherwise the escaped array aliases
// the subject's payload buffers, which the synth enum drop frees at scope exit
// (double-free). The string test above only exercises the dupString branch; this
// pins the pushElemNeedsDup/maybeDupPushElement branch that the container/double-
// free half of T1173 depends on. The two insertvalue rebuilds happen ONLY on the
// clone path (a no-op would extract-and-return without reinserting).
func TestT1173_VectorArrayReturnDeepClones(t *testing.T) {
	ir := generateIR(t, `
		enum T1173VIR { Two(int[][2] a), Nada, }
		esc() int[][2] {
			int[] p0 = [1, 2];
			int[] p1 = [3, 4];
			T1173VIR h = T1173VIR.Two(a: [p0, p1]);
			if h is Two(a) { return a; }
			int[] e = [];
			return [e.clone(), e.clone()];
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	if fn == "" {
		t.Fatalf("could not extract __user.esc from IR:\n%s", ir)
	}
	// Two insertvalue's into the [2 x i8*] aggregate rebuild the cloned array — the
	// rebuild only occurs on the clone path (a no-op escape would not reinsert).
	if n := strings.Count(fn, "insertvalue [2 x i8*]"); n < 2 {
		t.Fatalf("expected >=2 insertvalue into the cloned Vector-array aggregate, got %d\n%s", n, fn)
	}
	// The synth enum drop must still free the original payload once.
	if !strings.Contains(ir, "@T1173VIR.drop") {
		t.Fatalf("expected @T1173VIR.drop (synth enum drop) to still be emitted:\n%s", ir)
	}
	// The escaped elements must not be the bare-string dupString path — this is the
	// Vector (container) element branch, so no per-element promise_string_new.
	if strings.Contains(fn, "@promise_string_new") {
		t.Fatalf("did not expect @promise_string_new for a Vector-element array escape "+
			"(that is the string branch, not the container branch):\n%s", fn)
	}
}

// TestT1173_OwnedStringArrayReturnNoClone — an ordinary owned-local string[2]
// returned (NOT a match-borrow) must MOVE, not clone. The escape dup is gated on
// matchBorrowedIdents membership; a plain local built from a literal is not a
// borrow, so no per-element promise_string_new should appear for the return move.
func TestT1173_OwnedStringArrayReturnNoClone(t *testing.T) {
	ir := generateIR(t, `
		esc() string[2] {
			string[2] a = ["x" + "1", "y" + "2"];
			return a;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	if fn == "" {
		t.Fatalf("could not extract __user.esc from IR:\n%s", ir)
	}
	// The only promise_string_new's are the two literal concatenations building the
	// array; the return must not add a per-element clone. A whole-array escape clone
	// would re-run promise_string_new inside a strdup.copy block — assert none.
	if strings.Contains(fn, "strdup.copy") {
		t.Fatalf("did not expect a per-element string clone (strdup.copy) for a plain "+
			"owned-array return move — the T1173 escape dup must be gated on "+
			"match-borrowed bindings only:\n%s", fn)
	}
}
