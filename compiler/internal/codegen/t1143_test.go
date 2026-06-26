package codegen

import (
	"strings"
	"testing"
)

// T1143: Inline force-unwrap of a Map-index optional on a droppable element type
// followed by a member access (`m["k"]!.field`) double-freed the element. The
// synthesized `Map.[]` getter returns Optional[V] by ALIAS when V has an
// explicit/synth drop but no usable clone (its match-destructure does not dup V —
// typeNeedsMatchDup(V) is false, T0440). The inline unwrap reaching the plain
// no-dup extractvalue path therefore borrows the bucket's slot, yet
// trackHeapUserTypeResult still registered it as an independently-owned statement
// temp — so the temp drop AND the Map's own drop both freed the same instance
// (SIGSEGV at 0x0).
//
// Fix: genOptionalForceUnwrap sets optionalUnwrapContainerBorrow on the plain
// path when the source is such a Map index; trackHeapUserTypeResult consumes it
// and SKIPS owned-temp registration. The binding/return/arg dup paths return
// early in genOptionalForceUnwrap (optionalHeapDup), so this never touches them.
//
// These lock the IR shape; runtime zero-leak/double-free behavior is covered by
// the e2e batch tests in tests/e2e/map_index_unwrap_temp_test.pr under the
// zero-tolerance leak gate.

const t1143Decls = `
	type Resource { string name; drop(~this) {} }
	make_resource() Resource { return Resource(name: "test!"); }
`

// TestT1143_InlineUnwrapDoesNotDoubleDrop — the inline `m["k"]!.name` form must
// NOT register the aliased inner as an owned statement temp. The only
// Resource.drop$wrap (the statement-temp drop wrapper) in __user.mfu is the one
// for the map-literal construction temp; the buggy version emitted a SECOND
// drop$wrap for the unwrapped result (the double-free). Assert exactly one.
func TestT1143_InlineUnwrapDoesNotDoubleDrop(t *testing.T) {
	ir := generateIR(t, t1143Decls+`
		mfu() int {
			map[string, Resource] m = {"k": make_resource()};
			return m["k"]!.name.len;
		}
		main() { _ := mfu(); }
	`)
	fn := extractFunction(ir, "__user.mfu")
	if fn == "" {
		t.Fatalf("could not extract __user.mfu from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "unwrap.ok") {
		t.Fatalf("expected an unwrap.ok block (the force-unwrap) in __user.mfu:\n%s", fn)
	}
	// The map-literal construction temp accounts for exactly one drop$wrap. The
	// aliased unwrapped inner must NOT add a second (that was the double-free).
	if n := strings.Count(fn, "Resource.drop$wrap"); n != 1 {
		t.Fatalf("expected exactly 1 Resource.drop$wrap in __user.mfu (the map-literal "+
			"construction temp); got %d. A second drop$wrap means the aliased "+
			"unwrapped inner was wrongly registered as an owned temp → double-free:\n%s", n, fn)
	}
}

// TestT1143_BindingFormStillDups — the var-binding form (`r := m["k"]!`) must
// still deep-dup the inner via the T0440 dupHeapUserFieldAccess path
// (genMethodIndex → optionalHeapDup), so the new variable owns an independent
// copy. dupHeapValue emits `heapdup` blocks. This locks the other side of the
// dup/no-dup split — the no-dup skip must not regress the dup path.
func TestT1143_BindingFormStillDups(t *testing.T) {
	ir := generateIR(t, t1143Decls+`
		mbind() int {
			map[string, Resource] m = {"k": make_resource()};
			r := m["k"]!;
			return r.name.len;
		}
		main() { _ := mbind(); }
	`)
	fn := extractFunction(ir, "__user.mbind")
	if fn == "" {
		t.Fatalf("could not extract __user.mbind from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the binding-form unwrap "+
			"(the bound variable must own an independent copy), got none:\n%s", fn)
	}
}

// TestT1143_CloneBearingInlineStaysTracked — when V has a clone() method the
// `Map.[]` body dups V internally (typeNeedsMatchDup(V) is true), so the inline
// unwrap result is OWNED and must stay tracked as a statement temp (else a leak).
// The borrow skip must NOT fire here: the inline `m["k"]!.label` form keeps its
// owned-temp drop wrapper. Token has both clone and drop, so its $wrap temp drop
// must appear for the unwrapped result in addition to the map-literal temp.
func TestT1143_CloneBearingInlineStaysTracked(t *testing.T) {
	ir := generateIR(t, `
		type Token {
			string label;
			clone(this) Token { return Token(label: this.label); }
			drop(~this) {}
		}
		make_token() Token { return Token(label: "tok"); }
		mtok() int {
			map[string, Token] m = {"k": make_token()};
			return m["k"]!.label.len;
		}
		main() { _ := mtok(); }
	`)
	fn := extractFunction(ir, "__user.mtok")
	if fn == "" {
		t.Fatalf("could not extract __user.mtok from IR:\n%s", ir)
	}
	// Clone-bearing V is dup'd by the `[]` body → owned result → still tracked.
	// The map-literal temp plus the owned unwrapped-result temp give >= 2 wraps.
	if n := strings.Count(fn, "Token.drop$wrap"); n < 2 {
		t.Fatalf("expected the clone-bearing inline unwrap result to stay tracked as "+
			"an owned temp (>= 2 Token.drop$wrap: map-literal temp + owned result); "+
			"got %d — the borrow skip wrongly fired for a dup'd element:\n%s", n, fn)
	}
}
