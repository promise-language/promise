package codegen

import (
	"strings"
	"testing"
)

// T1179: A plain var-decl binding a whole match-borrowed Array[heap-user] or
// Optional[heap-user] enum-variant payload to a NEW owned local segfaulted (UAF /
// double-free). `if…is`/`match` bind such a payload as a shallow borrow (no drop):
// its inner heap value is still owned by the enum's synthesized drop. A plain
// var-decl (`Row[2] copy = value;` / `Row? copy = value;`) instead gives the new
// local an OWNING drop, so without a deep-clone both the local's drop and the
// synth enum drop freed the same instances.
//
// The fix deep-clones the borrowed payload exactly once at the var-decl site
// (cloneBorrowedWholePayloadVarDecl → cloneByType), gated on matchBorrowedIdents
// membership + matchBindingIsBorrow. The `if…is` path now also marks such bindings
// as match-borrowed (parity with match arms), and isAutoCloneBitCopy/
// cloneResolvedValue gained the missing *types.Array case.
//
// These lock the IR shape (a `heapdup` deep-clone block must appear in the escape
// function, and the enum's synth drop must still be emitted); runtime
// zero-leak/no-segfault behavior is covered by the e2e batch tests in
// tests/e2e/if_is_destructure_escape.pr under the zero-tolerance leak gate.

const t1179Decls = `
	type Row { string name; }
	enum Box { Some(Row[2] value), Empty, }
	enum Opt { Present(Row? value), Absent, }
`

// TestT1179_IfIsArrayVarDeclDeepClones — `if w is Some(value) { Row[2] copy = value; }`
// must deep-clone the whole array payload so `copy` owns independent Row instances.
func TestT1179_IfIsArrayVarDeclDeepClones(t *testing.T) {
	ir := generateIR(t, t1179Decls+`
		escape() int {
			Box w = Box.Some(value: [Row(name: "a"), Row(name: "b")]);
			if w is Some(value) {
				Row[2] copy = value;
				return copy[0].name.len;
			}
			return 0;
		}
		main() { _ := escape(); }
	`)
	fn := extractFunction(ir, "__user.escape")
	if fn == "" {
		t.Fatalf("could not extract __user.escape from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the match-borrowed array "+
			"var-decl (copy must own independent Row instances), got none:\n%s", fn)
	}
	// The enum's synth drop must still free the original payload once.
	if !strings.Contains(ir, "@Box.drop") {
		t.Fatalf("expected @Box.drop (synth enum drop) to still be emitted:\n%s", ir)
	}
}

// TestT1179_MatchArrayVarDeclDeepClones — the match-arm form deep-clones too.
func TestT1179_MatchArrayVarDeclDeepClones(t *testing.T) {
	ir := generateIR(t, t1179Decls+`
		escape() int {
			Box w = Box.Some(value: [Row(name: "a"), Row(name: "b")]);
			match w {
				Some(value) => {
					copy := value;
					return copy[1].name.len;
				},
				Empty => { return 0; },
			}
		}
		main() { _ := escape(); }
	`)
	fn := extractFunction(ir, "__user.escape")
	if fn == "" {
		t.Fatalf("could not extract __user.escape from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the match-arm array "+
			"var-decl, got none:\n%s", fn)
	}
}

// TestT1179_IfIsOptionalVarDeclDeepClones — the Optional[heap-user] payload analog.
func TestT1179_IfIsOptionalVarDeclDeepClones(t *testing.T) {
	ir := generateIR(t, t1179Decls+`
		escape() int {
			Opt o = Opt.Present(value: Row(name: "a"));
			if o is Present(value) {
				Row? copy = value;
				return copy!.name.len;
			}
			return 0;
		}
		main() { _ := escape(); }
	`)
	fn := extractFunction(ir, "__user.escape")
	if fn == "" {
		t.Fatalf("could not extract __user.escape from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the match-borrowed optional "+
			"var-decl (copy must own an independent Row), got none:\n%s", fn)
	}
}

// TestT1179_OwnedArrayVarDeclNoOverClone — an ordinary owned-local move of a whole
// array (NOT a match-borrow) must NOT be routed through the T1179 clone. The gate
// is matchBorrowedIdents membership + matchBindingIsBorrow; a plain local built
// from a literal is neither, so no extra deep-clone should appear for the move.
func TestT1179_OwnedArrayVarDeclNoOverClone(t *testing.T) {
	ir := generateIR(t, t1179Decls+`
		owned() int {
			Row[2] a = [Row(name: "a"), Row(name: "b")];
			Row[2] b = a;
			return b[0].name.len;
		}
		main() { _ := owned(); }
	`)
	fn := extractFunction(ir, "__user.owned")
	if fn == "" {
		t.Fatalf("could not extract __user.owned from IR:\n%s", ir)
	}
	if strings.Contains(fn, "heapdup") {
		t.Fatalf("did not expect a heap dup for a plain owned-array move (`b = a`) — the "+
			"T1179 clone must be gated on match-borrowed bindings only:\n%s", fn)
	}
}
