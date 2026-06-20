package codegen

import (
	"strings"
	"testing"
)

// T0958: An inline (unbound) result of a user-defined operator whose body is
// `return this` on a MOVE receiver (`~this`) aliases the operand's heap
// allocation. trackOperatorResult (T0918) registers the result as an owned heap
// drop-temp, but — unlike a method call — BinaryExpr/UnaryExpr have no inner
// CallExpr, so findInnerCallExpr returns nil and the T0341/T0882 receiver-alias-
// clear was bypassed. The temp's drop flag and the operand's own scope-binding
// drop flag were both left set → the same instance was freed twice (double-free,
// SEGV at runtime; silent under leak-only detection). operatorReceiverOrigin now
// reaches the same clear via emitReceiverAliasCheckForTarget.
//
// The test type carries a `string` field so it is a heap (droppable) type — a
// pure value type (`{ int v; }`) is skipped by trackHeapUserTypeResult's value-
// type guard and never reaches the alias-clear path.

// Inline binary operator discard with an Ident left operand must emit the
// receiver-alias-clear blocks (origin is the left operand `a`).
func TestT0958InlineBinaryEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type BB { string name; int v; +(~this, BB o) BB { return this; } drop(~this) {} }
		main() { a := BB(name: "a", v: 11); b := BB(name: "b", v: 3); a + b; }
	`)
	assertContains(t, ir, "recv.alias.clear")
	assertContains(t, ir, "recv.alias.skip")
}

// Inline prefix-unary operator discard with an Ident operand must emit the
// receiver-alias-clear blocks (origin is the operand `d`).
func TestT0958InlineUnaryEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type NB { string name; int v; -(~this) NB { return this; } drop(~this) {} }
		main() { d := NB(name: "d", v: 11); -d; }
	`)
	assertContains(t, ir, "recv.alias.clear")
	assertContains(t, ir, "recv.alias.skip")
}

// Inline operator result with a field access (`x := (a + b).v`) — the concern's
// exact example — must still emit the alias-clear: the temp aliases `a` and is
// dropped at statement end while `a`'s scope binding also frees it.
func TestT0958InlineOperatorFieldAccessEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type BB { string name; int v; +(~this, BB o) BB { return this; } drop(~this) {} }
		main() { a := BB(name: "a", v: 11); b := BB(name: "b", v: 3); x := (a + b).v; }
	`)
	assertContains(t, ir, "recv.alias.clear")
	assertContains(t, ir, "recv.alias.skip")
}

// Inline operator invoked with `this` as the left operand inside a method body
// (discarded) must reach the clear through the ThisExpr origin path.
func TestT0958InlineOperatorThisOperandEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type BB {
			string name; int v;
			+(~this, BB o) BB { return this; }
			combine(~this, BB o) { this + o; }
			drop(~this) {}
		}
		main() { a := BB(name: "a", v: 11); b := BB(name: "b", v: 3); a.combine(b); }
	`)
	assertContains(t, ir, "recv.alias.clear")
	assertContains(t, ir, "recv.alias.skip")
}

// Negative: a `structural` operator whose result is inferred to the interface
// type is skipped by trackHeapUserTypeResult's IsStructural() guard — the result
// binding never takes an owning drop, so the alias-clear MUST NOT be emitted
// (clearing would leak the shared instance). Mirrors
// TestOperatorUnaryReturnThisStructuralSkipsAliasClear for the inline path.
func TestT0958InlineStructuralOperatorSkipsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type Negatable `+"`structural"+` {
			string name;
			-(~this) Negatable { return this; }
		}
		type Item is Negatable { string name; int v; }
		main() { it := Item(name: "x", v: 5); -it; }
	`)
	if strings.Contains(ir, "recv.alias.clear") {
		t.Fatalf("structural inline operator return-this must NOT emit recv.alias.clear (would leak):\n%s", ir)
	}
}

// Chained return-this operators (`a + b + c`): the outer operator's left operand
// is the inner `a + b` BinaryExpr, so operatorReceiverOrigin must descend through
// it to reach `a`. Without the descent the alias-clear is skipped (origin is a
// BinaryExpr, not Ident/This) → `a`'s binding and the chained result double-free.
// Inline form: the descent feeds emitReceiverAliasCheckForTarget on the outer temp.
func TestT0958ChainedReturnThisInlineEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type BB { string name; int v; +(~this, BB o) BB { return this; } drop(~this) {} }
		main() { a := BB(name: "a", v: 1); b := BB(name: "b", v: 2); c := BB(name: "c", v: 3); (a + b + c).v; }
	`)
	// Two operator dispatches → two alias-clear sites (inner temp + outer temp),
	// both reachable only because the origin descends to the `a` leaf.
	if got := strings.Count(ir, "recv.alias.clear"); got < 2 {
		t.Fatalf("chained inline operator must emit an alias-clear per level (want >=2 recv.alias.clear, got %d):\n%s", got, ir)
	}
}

// Chained return-this operators bound to a local (`m := a + b + c`): the outer
// origin must descend to `a` so maybeClearReceiverDropFlag clears `a`'s binding
// (transferring ownership to `m`); otherwise `m` and `a` double-free.
func TestT0958ChainedReturnThisBoundClearsReceiver(t *testing.T) {
	ir := generateIR(t, `
		type BB { string name; int v; +(~this, BB o) BB { return this; } drop(~this) {} }
		main() { a := BB(name: "a", v: 1); b := BB(name: "b", v: 2); c := BB(name: "c", v: 3); m := a + b + c; }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// Negative: an operator returning a FRESH instance (not `this`) used inline must
// still drop its owned temp — the fix only inserts a runtime pointer icmp, never
// a static drop removal, so the icmp is harmless (pointers differ) and the temp's
// drop is preserved. Confirms the fix does not over-suppress genuine owned drops.
func TestT0958InlineFreshInstanceKeepsTempDrop(t *testing.T) {
	ir := generateIR(t, `
		type BB { string name; int v; +(BB o) BB { return BB(name: "n", v: 0); } drop(~this) {} }
		main() { a := BB(name: "a", v: 11); b := BB(name: "b", v: 3); a + b; }
	`)
	assertContains(t, ir, "call void @BB.drop")
}

// A GENERIC return-this operator used inline (with typeSubst active during mono)
// must still reach the alias-clear: the direct-dispatch tests above never set
// typeSubst, so this guards the substituted-instance path through
// trackHeapUserTypeResult → operatorReceiverOrigin. T=int alone is a value type
// (skipped); the heap `tag` field makes GHeap[int] droppable.
func TestT0958InlineGenericReturnThisEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type GHeap[T] { string tag; T v; +(~this, GHeap[T] o) GHeap[T] { return this; } drop(~this) {} }
		main() { a := GHeap[int](tag: "a", v: 1); b := GHeap[int](tag: "b", v: 2); (a + b).v; }
	`)
	assertContains(t, ir, "recv.alias.clear")
	assertContains(t, ir, "recv.alias.skip")
}

// A VIRTUAL (inherited) return-this operator used inline on a child instance must
// reach the alias-clear via the operand origin — dispatch is virtual but the
// origin is still the `a` local. Guards the inheritance path, which the T0918
// virtual tests cover only with fresh (non-aliasing) results.
func TestT0958InlineVirtualReturnThisEmitsAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type PHeap { string tag; int v; +(~this, PHeap o) PHeap { return this; } drop(~this) {} }
		type CHeap is PHeap { string tag; int v; }
		main() { PHeap a = CHeap(tag: "a", v: 1); PHeap b = CHeap(tag: "b", v: 2); (a + b).v; }
	`)
	assertContains(t, ir, "recv.alias.clear")
	assertContains(t, ir, "recv.alias.skip")
}
