package codegen

import (
	"strings"
	"testing"
)

// T1183: a whole fixed-Array-VALUE escape of a match-borrowed variant payload
// whose element is Optional[string] / Optional[<container>] was NOT deep-cloned.
// T1173 broadened the escape recognizer (arrayElemNeedsEscapeDup -> pushElemNeedsDup)
// but the Optional branch delegated to optionalHeapDupElem, which ONLY matches
// Optional[heap-user]. So Optional[string][N] / Optional[Vector][N] whole-array
// escapes stayed aliased: the subject's synth enum drop freed the inner
// strings/buffers at scope exit while the escaped array still pointed at them → UAF.
//
// The fix introduces the broader push/escape recognizer optionalPushElemNeedsDup
// (string / Vector / Channel / Arc / Weak / tuple / nested-Optional inners) used by
// pushElemNeedsDup and maybeDupPushElement, which deep-clones via
// dupOptionalVectorElem (present/absent split -> per-inner dispatch). This fix is
// SHARED with the vector-push path, so Vector[Optional[string]].push of an aliasing
// element is fixed too.
//
// These lock the IR shape; runtime zero-leak/no-UAF behavior is covered by the e2e
// batch tests in tests/e2e/if_is_destructure_escape.pr (T1183 section) under the
// zero-tolerance leak gate.

const t1183Decls = `
	enum OptStrArr { Pair(string?[2] a), Empty, }
`

// TestT1183_IfIsOptStringArrayReturnDeepClones — `if h is Pair(a) { return a; }` for
// a string?[2] payload must element-wise deep-clone each Optional[string]: a
// present/absent split (optdup.dup block) whose present path calls dupString
// (promise_string_new) and rebuilds the aggregate. The synth enum drop must still be
// emitted to free the original payload once.
func TestT1183_IfIsOptStringArrayReturnDeepClones(t *testing.T) {
	ir := generateIR(t, t1183Decls+`
		esc() string?[2] {
			string? a0 = "x" + "1";
			string? a1 = "z" + "2";
			OptStrArr h = OptStrArr.Pair(a: [a0, a1]);
			if h is Pair(a) { return a; }
			string? y0 = none;
			string? y1 = none;
			return [y0, y1];
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	if fn == "" {
		t.Fatalf("could not extract __user.esc from IR:\n%s", ir)
	}
	// One present/absent split per escaping array element (2 for a 2-elem array).
	if n := strings.Count(fn, "optdup.dup"); n < 2 {
		t.Fatalf("expected >=2 optdup.dup present-split blocks (one per Optional[string] element), got %d\n%s", n, fn)
	}
	// The present path deep-clones the inner string via dupString -> promise_string_new.
	if n := strings.Count(fn, "@promise_string_new"); n < 2 {
		t.Fatalf("expected >=2 @promise_string_new (one per escaping Optional[string] element), got %d\n%s", n, fn)
	}
	// The enum's synth drop must still free the original payload once.
	if !strings.Contains(ir, "@OptStrArr.drop") {
		t.Fatalf("expected @OptStrArr.drop (synth enum drop) to still be emitted:\n%s", ir)
	}
}

// TestT1183_MatchOptStringArrayReturnDeepClones — the match-arm form clones too (both
// paths share matchBorrowedIdents + dupBorrowedHeapUserPayload -> dupArrayElemForEscape).
func TestT1183_MatchOptStringArrayReturnDeepClones(t *testing.T) {
	ir := generateIR(t, t1183Decls+`
		esc() string?[2] {
			string? a0 = "m" + "1";
			string? a1 = "m" + "2";
			OptStrArr h = OptStrArr.Pair(a: [a0, a1]);
			match h {
				Pair(a) => { return a; },
				Empty => {
					string? z0 = none;
					string? z1 = none;
					return [z0, z1];
				},
			}
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	if fn == "" {
		t.Fatalf("could not extract __user.esc from IR:\n%s", ir)
	}
	if n := strings.Count(fn, "optdup.dup"); n < 2 {
		t.Fatalf("expected >=2 optdup.dup present-split blocks (one per Optional[string] element), got %d\n%s", n, fn)
	}
	if n := strings.Count(fn, "@promise_string_new"); n < 2 {
		t.Fatalf("expected >=2 @promise_string_new (one per escaping Optional[string] element), got %d\n%s", n, fn)
	}
}

// TestT1183_VectorOptStringPushDeepClones — the SHARED push path. Pushing an
// index-read Optional[string] element (`dst.push(src[i])`) into a
// Vector[Optional[string]] must deep-clone the inner string (present-split +
// promise_string_new) so dst owns an independent copy — otherwise dst and src
// alias the same heap string and double-free / UAF at drop. maybeDupPushElement's
// new optionalPushElemNeedsDup branch (routed via dupOptionalVectorElem) drives this.
func TestT1183_VectorOptStringPushDeepClones(t *testing.T) {
	ir := generateIR(t, `
		build() {
			string?[] src = [];
			src.push("h" + "1");
			string?[] dst = [];
			dst.push(src[0]);
		}
		main() { build(); }
	`)
	fn := extractFunction(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract __user.build from IR:\n%s", ir)
	}
	// The push of src[0] into dst must go through the Optional present-split clone.
	if !strings.Contains(fn, "optdup.dup") {
		t.Fatalf("expected an optdup.dup present-split block for the pushed Optional[string] element:\n%s", fn)
	}
	// The present path clones the inner string via dupString -> promise_string_new.
	// (The src.push literal lowers to promise_string_concat, so the sole
	// promise_string_new here is the dst.push element clone.)
	if n := strings.Count(fn, "@promise_string_new"); n < 1 {
		t.Fatalf("expected >=1 @promise_string_new (pushed Optional[string] element clone), got %d\n%s", n, fn)
	}
}

// TestT1183_IfIsOptEnumArrayReturnDeepClones — Optional[droppable-enum][N] escape.
// optionalPushElemNeedsDup recognizes Optional[enum] (via pushElemNeedsDup's enum
// branch), so the escape routes into dupOptionalVectorElem. That switch originally
// had no enum case → the Some payload fell to the shallow pass-through and aliased
// the subject's variant fields → double-free once the subject's synth enum drop ran.
// The enum branch must deep-clone via an in-place variant-field dup (enumdup split),
// which re-clones the inner string (promise_string_new).
func TestT1183_IfIsOptEnumArrayReturnDeepClones(t *testing.T) {
	ir := generateIR(t, `
		enum Inner { Has(string s), Nothing, }
		enum OptEnumArr { Pair(Inner?[2] a), Empty, }
		esc() Inner?[2] {
			Inner? a0 = Inner.Has(s: "x" + "1");
			Inner? a1 = Inner.Has(s: "z" + "2");
			OptEnumArr h = OptEnumArr.Pair(a: [a0, a1]);
			if h is Pair(a) { return a; }
			Inner? y0 = none;
			Inner? y1 = none;
			return [y0, y1];
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	if fn == "" {
		t.Fatalf("could not extract __user.esc from IR:\n%s", ir)
	}
	// One present/absent split per escaping Optional[enum] element (2 for a 2-elem array).
	if n := strings.Count(fn, "optdup.dup"); n < 2 {
		t.Fatalf("expected >=2 optdup.dup present-split blocks (one per Optional[enum] element), got %d\n%s", n, fn)
	}
	// The present path deep-clones each enum via the in-place variant-field dup (enumdup),
	// NOT a shallow pass-through. Its absence means the aliasing UAF regressed.
	if n := strings.Count(fn, "enumdup"); n < 2 {
		t.Fatalf("expected >=2 enumdup blocks (deep-clone of each escaping Optional[enum] element), got %d\n%s", n, fn)
	}
	// The enum's synth drop must still free the original payload once.
	if !strings.Contains(ir, "@OptEnumArr.drop") {
		t.Fatalf("expected @OptEnumArr.drop (synth enum drop) to still be emitted:\n%s", ir)
	}
}

// TestT1183_IfIsOptCloneEnumArrayReturnUsesCloneFn — the OTHER sub-path of
// dupOptionalVectorElem's enum branch. When the inner enum has a synthesized
// clone (`clone), the Some payload must be deep-copied via the clone function
// (@Inner.clone), NOT the in-place variant-field dup (enumdup) used for
// drop-only enums. Locks the clone-fn routing so a regression to a shallow alias
// (or to the wrong dup strategy) is caught.
func TestT1183_IfIsOptCloneEnumArrayReturnUsesCloneFn(t *testing.T) {
	ir := generateIR(t, `
		enum Inner `+"`clone"+` { Has(string s), Nothing, }
		enum OptEnumArr { Pair(Inner?[2] a), Empty, }
		esc() Inner?[2] {
			Inner? a0 = Inner.Has(s: "x" + "1");
			Inner? a1 = Inner.Has(s: "z" + "2");
			OptEnumArr h = OptEnumArr.Pair(a: [a0, a1]);
			if h is Pair(a) { return a; }
			Inner? y0 = none;
			Inner? y1 = none;
			return [y0, y1];
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	if fn == "" {
		t.Fatalf("could not extract __user.esc from IR:\n%s", ir)
	}
	// One present/absent split per escaping Optional[enum] element (2 for a 2-elem array).
	if n := strings.Count(fn, "optdup.dup"); n < 2 {
		t.Fatalf("expected >=2 optdup.dup present-split blocks (one per Optional[enum] element), got %d\n%s", n, fn)
	}
	// The present path must route through the synthesized clone function, NOT the
	// in-place variant-field dup (enumdup).
	if n := strings.Count(fn, "@Inner.clone"); n < 2 {
		t.Fatalf("expected >=2 @Inner.clone calls (clone-fn deep-clone of each escaping Optional[cloneable-enum] element), got %d\n%s", n, fn)
	}
	if strings.Contains(fn, "enumdup") {
		t.Fatalf("cloneable enum must use the clone fn, not the in-place enumdup path:\n%s", fn)
	}
}
