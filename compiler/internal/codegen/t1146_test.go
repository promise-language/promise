package codegen

import (
	"strings"
	"testing"
)

// T1146: Inline force-unwrap of a Map-index optional on a droppable-no-clone
// element type that ESCAPES into an owning sink — a move (~) param arg
// (`consume(m["k"]!)`), a constructor field-init (`Holder(res: m["k"]!)`), or a
// return (`return m["k"]!`) — double-freed the element (SIGSEGV at 0x0 / invalid
// free). The synthesized `Map.[]` getter returns Optional[V] by ALIAS when V has
// an explicit/synth drop but no usable clone (typeNeedsMatchDup(V) is false,
// T0440). The unwrapped inner aliased the bucket slot; the owning sink dropped it
// while the Map ALSO dropped the same slot at scope exit.
//
// Sibling of T1143 (member-access form). That fix scoped a borrow-SKIP to the
// plain no-dup path. These owning sinks instead need a DUP — the value genuinely
// must become independent (the map keeps and drops its slot). The fix sets
// dupHeapUserFieldAccess in the three sinks, routing them through genMethodIndex's
// deep-dup exactly like the already-working var-binding form (stmt.go:1133).
//
// These lock the IR shape (a `heapdup` block must appear in each owning sink,
// where the buggy version emitted none); runtime zero-leak/double-free behavior is
// covered by the e2e batch tests in tests/e2e/map_index_unwrap_temp_test.pr under
// the zero-tolerance leak gate.

const t1146Decls = `
	type Resource { string name; drop(~this) {} }
	make_resource() Resource { return Resource(name: "test!"); }
	consume(Resource move r) int { return r.name.len; }
	type Holder { Resource res; drop(~this) {} }
`

// TestT1146_MoveArgDups — `consume(m["k"]!)` must dup the unwrapped inner so the
// callee's consume-drop and the map's slot drop don't free the same instance.
func TestT1146_MoveArgDups(t *testing.T) {
	ir := generateIR(t, t1146Decls+`
		arg_consume() int {
			map[string, Resource] m = {"k": make_resource()};
			return consume(m["k"]!);
		}
		main() { _ := arg_consume(); }
	`)
	fn := extractFunction(ir, "__user.arg_consume")
	if fn == "" {
		t.Fatalf("could not extract __user.arg_consume from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the inline move-arg unwrap "+
			"(the moved-in arg must own an independent copy), got none:\n%s", fn)
	}
}

// TestT1146_ConstructorArgDups — `Holder(res: m["k"]!)` field-init must dup the
// unwrapped inner so the new instance's field-drop and the map's slot drop don't
// free the same instance.
func TestT1146_ConstructorArgDups(t *testing.T) {
	ir := generateIR(t, t1146Decls+`
		ctor_move() int {
			map[string, Resource] m = {"k": make_resource()};
			h := Holder(res: m["k"]!);
			return h.res.name.len;
		}
		main() { _ := ctor_move(); }
	`)
	fn := extractFunction(ir, "__user.ctor_move")
	if fn == "" {
		t.Fatalf("could not extract __user.ctor_move from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the inline constructor-arg "+
			"unwrap (the new field must own an independent copy), got none:\n%s", fn)
	}
}

// TestT1146_ReturnDups — `return m["k"]!` must dup the unwrapped inner so the
// caller's drop of the returned value and the map's slot drop at function exit
// don't free the same instance.
func TestT1146_ReturnDups(t *testing.T) {
	ir := generateIR(t, t1146Decls+`
		relay() Resource {
			map[string, Resource] m = {"k": make_resource()};
			return m["k"]!;
		}
		main() { r := relay(); _ := r.name.len; }
	`)
	fn := extractFunction(ir, "__user.relay")
	if fn == "" {
		t.Fatalf("could not extract __user.relay from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the inline return unwrap "+
			"(the caller must own an independent copy), got none:\n%s", fn)
	}
}

// TestT1146_SynthDropMoveArgDups — a synth-drop-only V whose fields aren't
// shallow-dup-safe (a droppable-element vector field) is ALSO returned by alias
// from Map.[] (typeNeedsMatchDup is false). The pre-existing dup gate keyed on an
// *explicit* drop method (LookupMethod("drop") != nil) wrongly excluded it, so it
// aliased the bucket and double-freed even in the var-binding form. Widening the
// gate to `!typeNeedsMatchDup && clone == nil` fixes it for every owning sink. The
// move-arg form must now emit a `heapdup` block.
func TestT1146_SynthDropMoveArgDups(t *testing.T) {
	ir := generateIR(t, `
		type Bag { string[] items; }
		make_bag() Bag { return Bag(items: ["a", "b", "c"]); }
		consume_bag(Bag move b) int { return b.items.len; }
		arg_consume() int {
			map[string, Bag] m = {"k": make_bag()};
			return consume_bag(m["k"]!);
		}
		main() { _ := arg_consume(); }
	`)
	fn := extractFunction(ir, "__user.arg_consume")
	if fn == "" {
		t.Fatalf("could not extract __user.arg_consume from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the synth-drop-only move-arg "+
			"unwrap (Map.[] aliases it; the moved-in arg must own an independent copy), "+
			"got none:\n%s", fn)
	}
}

// TestT1146_SynthDropBindDups — locks the widened gate on the already-existing
// var-binding path: `b := m["k"]!` for a synth-drop-only Bag must dup. This is the
// pre-existing form the gate-widening also repairs (it double-freed before).
func TestT1146_SynthDropBindDups(t *testing.T) {
	ir := generateIR(t, `
		type Bag { string[] items; }
		make_bag() Bag { return Bag(items: ["a", "b", "c"]); }
		bag_bind() int {
			map[string, Bag] m = {"k": make_bag()};
			b := m["k"]!;
			return b.items.len;
		}
		main() { _ := bag_bind(); }
	`)
	fn := extractFunction(ir, "__user.bag_bind")
	if fn == "" {
		t.Fatalf("could not extract __user.bag_bind from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the synth-drop-only bind "+
			"unwrap (the bound variable must own an independent copy), got none:\n%s", fn)
	}
}

// TestT1146_ParenSourceMoveArgDups — `consume((m["k"])!)` wraps the index in a
// ParenExpr before the unwrap. isUnwrappedContainerIndex must peel the paren(s) to
// find the IndexExpr, so the dup still fires. Locks the paren-peeling loop body.
func TestT1146_ParenSourceMoveArgDups(t *testing.T) {
	ir := generateIR(t, t1146Decls+`
		arg_consume() int {
			map[string, Resource] m = {"k": make_resource()};
			return consume((m["k"])!);
		}
		main() { _ := arg_consume(); }
	`)
	fn := extractFunction(ir, "__user.arg_consume")
	if fn == "" {
		t.Fatalf("could not extract __user.arg_consume from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the parenthesized inline "+
			"move-arg unwrap `(m[\"k\"])!` (paren must be peeled to find the IndexExpr), "+
			"got none:\n%s", fn)
	}
}

// TestT1146_ParenSourceReturnDups — `return (m["k"])!` — the return-sink path also
// routes through isUnwrappedContainerIndex; locks paren-peeling there too.
func TestT1146_ParenSourceReturnDups(t *testing.T) {
	ir := generateIR(t, t1146Decls+`
		relay() Resource {
			map[string, Resource] m = {"k": make_resource()};
			return (m["k"])!;
		}
		main() { r := relay(); _ := r.name.len; }
	`)
	fn := extractFunction(ir, "__user.relay")
	if fn == "" {
		t.Fatalf("could not extract __user.relay from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the parenthesized inline "+
			"return unwrap `(m[\"k\"])!`, got none:\n%s", fn)
	}
}

// TestT1146_NonIndexUnwrapNoDup — a force-unwrap whose source is NOT a container
// index (`f()!` where f returns Optional[Resource]) must NOT trip the new dup flag:
// isUnwrappedContainerIndex returns false (inner is a CallExpr, not IndexExpr), so
// the owning-sink dup path is skipped. Guards against the predicate over-firing.
func TestT1146_NonIndexUnwrapNoDup(t *testing.T) {
	ir := generateIR(t, t1146Decls+`
		maybe() Resource? { return make_resource(); }
		arg_consume() int {
			return consume(maybe()!);
		}
		main() { _ := arg_consume(); }
	`)
	fn := extractFunction(ir, "__user.arg_consume")
	if fn == "" {
		t.Fatalf("could not extract __user.arg_consume from IR:\n%s", ir)
	}
	if strings.Contains(fn, "heapdup") {
		t.Fatalf("a non-index unwrap source (`f()!`) must NOT route through the container "+
			"index dup path; expected no `heapdup` block:\n%s", fn)
	}
}

// TestT1146_CloneBearingMoveArgNoDoubleDup — when V has a clone() method the
// `Map.[]` body dups V internally (typeNeedsMatchDup(V) is true), so the inline
// unwrap result is already OWNED. genMethodIndex's dup branch requires clone ==
// nil, so the new flag must NOT fire a SECOND dup inside the move-arg body —
// otherwise the extra copy leaks. Token has both clone and drop; the only
// `heapdup` in arg_consume must come from cloning that crosses no extra path here,
// so we assert there is no `heapdup` block in the move-arg body itself.
func TestT1146_CloneBearingMoveArgNoDoubleDup(t *testing.T) {
	ir := generateIR(t, `
		type Token {
			string label;
			clone(this) Token { return Token(label: this.label); }
			drop(~this) {}
		}
		make_token() Token { return Token(label: "tok"); }
		consume_token(Token move t) int { return t.label.len; }
		arg_consume() int {
			map[string, Token] m = {"k": make_token()};
			return consume_token(m["k"]!);
		}
		main() { _ := arg_consume(); }
	`)
	fn := extractFunction(ir, "__user.arg_consume")
	if fn == "" {
		t.Fatalf("could not extract __user.arg_consume from IR:\n%s", ir)
	}
	if strings.Contains(fn, "heapdup") {
		t.Fatalf("clone-bearing V is dup'd by the `[]` body → already owned; the inline "+
			"move-arg must NOT emit a redundant `heapdup` (that copy would leak):\n%s", fn)
	}
}
