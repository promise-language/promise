package regress11

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1952: a value crossing to a FIRST parent whose slot shapes it changed by a
// relaxed match must get a view vtable with an adapter, not the concrete's own
// vtable. `std.Closer` requires `close!(~this)`; a non-failable `close(~this)` is
// a documented relaxed match (language-design.md#variable-declarations), so the concrete's own slot holds a
// `void (i8*)*` while the Closer call site bitcasts to `{ i1, i8* } (i8*)*` and
// reads an error flag the callee never wrote.
//
// These are IR assertions on purpose: the runtime symptom is whatever happens to
// be in the result register, so a test that only checks exit status passes on a
// broken compiler.

// The same type WITHOUT an `is` clause — the structural path, which already
// adapted correctly. Pinned so the two crossings cannot drift apart.
func TestT1952StructuralCrossingAdaptsNonFailableClose(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type HA `+"`public"+` {
			int fd;
			new(~this, int f) { this.fd = f; }
			close(~this) {}
		}
		main() { HA h = HA(1); Closer c = h; c.close()?!; }
	`)
	codegentest.AssertContains(t, ir, "@HA.close$view_adapt_as_Closer")
	codegentest.AssertContainsMatch(t,
		ir, `@promise_vtable_HA_as_Closer = constant \[1 x i8\*\] \[i8\* bitcast \(\{ i1, i8\* \} \(i8\*\)\* @HA\.close\$view_adapt_as_Closer`)
}

// The bug. `is Closer` + a non-failable close: the concrete's own vtable keeps
// its own `void` shape, and the crossing installs an adapting view vtable.
func TestT1952ExplicitIsCrossingAdaptsNonFailableClose(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type HB is Closer `+"`public"+` {
			int fd;
			new(~this, int f) { this.fd = f; }
			close(~this) {}
		}
		main() { HB h = HB(1); Closer c = h; c.close()?!; }
	`)
	// The type's OWN vtable must still carry its own declared shape — a call
	// dispatched through HB as a static type reads `void` from that same slot.
	codegentest.AssertContains(t, ir, "define void @HB.close(i8* %this)")
	codegentest.AssertContainsMatch(t,
		ir, `@promise_vtable_HB = constant \[2 x i8\*\] \[i8\* bitcast \(void \(i8\*\)\* @HB\.close to i8\*\)`)
	// The crossing adapts.
	codegentest.AssertContainsMatch(t,
		ir, `@promise_vtable_HB_as_Closer = constant \[1 x i8\*\] \[i8\* bitcast \(\{ i1, i8\* \} \(i8\*\)\* @HB\.close\$view_adapt_as_Closer`)
	codegentest.AssertContains(t, ir, "define { i1, i8* } @HB.close$view_adapt_as_Closer")
	// The box installs the view vtable, not the concrete's own.
	codegentest.AssertContains(t, ir, "@promise_vtable_HB_as_Closer to i8*), 0")
}

// `is Closer` spelled exactly must stay free — no view vtable, no adapter. This
// is T1734's premise ("the `is` clause costs nothing at runtime") and the guard
// against making every explicit conformance pay for a vtable it does not need.
func TestT1952ExactCloseNeedsNoViewVtable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type HC is Closer `+"`public"+` {
			int fd;
			new(~this, int f) { this.fd = f; }
			close!(~this) {}
		}
		main() { HC h = HC(1); Closer c = h; c.close()?!; }
	`)
	codegentest.AssertNotContains(t, ir, "promise_vtable_HC_as_")
	codegentest.AssertNotContains(t, ir, "HC.close$view_adapt")
}

// Not specific to structural interfaces: plain `is` inheritance has the same
// hole. With `R is Q is P`, one slot serves two shapes — `q.close()` reads it as
// `void`, `p.close()?!` reads it as a failable struct — which is why the
// adaptation must live in the view and never be written into R's own vtable.
func TestT1952PlainInheritanceCrossingAdaptsAndOwnVtableIsUntouched(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type P `+"`public"+` { new(~this) {} close!(~this) {} }
		type Q is P `+"`public"+` { new(~this) {} close(~this) {} }
		type R is Q `+"`public"+` { new(~this) {} close(~this) {} }
		main() { Q q = R(); q.close(); P p = R(); p.close()?!; }
	`)
	// R's own vtable keeps the void shape Q's call site reads.
	codegentest.AssertContainsMatch(t,
		ir, `@promise_vtable_R = constant \[2 x i8\*\] \[i8\* bitcast \(void \(i8\*\)\* @R\.new to i8\*\), i8\* bitcast \(void \(i8\*\)\* @R\.close to i8\*\)\]`)
	// The P crossing adapts; the Q crossing (no shape change) does not.
	codegentest.AssertContainsMatch(t,
		ir, `@promise_vtable_R_as_P = constant \[2 x i8\*\] \[i8\* bitcast \(void \(i8\*\)\* @R\.new to i8\*\), i8\* bitcast \(\{ i1, i8\* \} \(i8\*\)\* @R\.close\$view_adapt_as_P to i8\*\)\]`)
	codegentest.AssertNotContains(t, ir, "promise_vtable_R_as_Q")
}

// A first-parent crossing with no shape difference must not pay for a view
// vtable — the other half of the guard in TestT1952ExactCloseNeedsNoViewVtable,
// on plain inheritance rather than a structural interface.
func TestT1952UnchangedSlotShapesReuseOwnVtable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base `+"`public"+` { new(~this) {} tick!(~this) int { return 1; } }
		type Leaf is Base `+"`public"+` { new(~this) {} tick!(~this) int { return 2; } }
		main() { Base b = Leaf(); b.tick()?!; }
	`)
	codegentest.AssertNotContains(t, ir, "promise_vtable_Leaf_as_Base")
	codegentest.AssertNotContains(t, ir, "Leaf.tick$view_adapt")
}

// The `T`-for-`T?` arm of slotShapeDiffers, on a first parent: the requirement
// returns an optional, the override returns the bare value, so the view slot
// must wrap it as `some`.
func TestT1952OptionalReturnRelaxationAdaptsOnFirstParent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Src `+"`public"+` { new(~this) {} peek(~this) int? { return 1; } }
		type Fixed is Src `+"`public"+` { new(~this) {} peek(~this) int { return 2; } }
		main() { Src s = Fixed(); s.peek(); }
	`)
	codegentest.AssertContains(t, ir, "@Fixed.peek$view_adapt_as_Src")
	codegentest.AssertContains(t, ir, "@promise_vtable_Fixed_as_Src")
}

// The extra-defaulted-param arm of slotShapeDiffers, on a first parent: the
// override takes a trailing parameter the requirement does not, so the view slot
// must supply its default.
func TestT1952ExtraDefaultedParamAdaptsOnFirstParent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Emit `+"`public"+` { new(~this) {} send(~this, int n) {} }
		type Wide is Emit `+"`public"+` { new(~this) {} send(~this, int n, int flags = 7) {} }
		main() { Emit e = Wide(); e.send(1); }
	`)
	codegentest.AssertContains(t, ir, "@Wide.send$view_adapt_as_Emit")
	codegentest.AssertContains(t, ir, "@promise_vtable_Wide_as_Emit")
}

// T1952: when the VIEW itself inherits and overrides a member, the view vtable's
// slot must carry the VIEW's shape — not the ancestor's.
//
// AllVirtualMethods() walks parents first, so for `Q is P` it hands back P's
// `close!` object even though Q overrides it non-failably. Resolving the slot's
// expected shape from that ancestor built `R_as_Q` with a `{ i1, i8* }` adapter
// while every call site with static type Q reads `void` — the same class of
// mismatch this item fixes, one level in. getOrEmitViewVtable therefore resolves
// the view's own member before deriving any shape from it.
//
// R relaxes Q by an extra defaulted param, which is what forces R_as_Q to exist
// at all; without that second relaxation the crossing reuses R's own vtable.
func TestT1952ViewVtableUsesTheViewsOwnShapeNotTheAncestors(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type P `+"`public"+` { new(~this) {} close!(~this) {} }
		type Q is P `+"`public"+` { new(~this) {} close(~this) {} }
		type R is Q `+"`public"+` { new(~this) {} close(~this, int x = 1) {} }
		main() { Q q = R(); q.close(); }
	`)
	// The adapter's own definition carries Q's void return, not P's { i1, i8* }.
	codegentest.AssertContains(t, ir, "define void @R.close$view_adapt_as_Q(i8* %this)")
	codegentest.AssertContainsMatch(t,
		ir, `@promise_vtable_R_as_Q = constant \[2 x i8\*\] \[i8\* bitcast \(void \(i8\*\)\* @R\.new to i8\*\), i8\* bitcast \(void \(i8\*\)\* @R\.close\$view_adapt_as_Q to i8\*\)\]`)
	// Guard the regression directly: a failable-shaped adapter in that slot is
	// exactly what resolving against the ancestor produced.
	codegentest.AssertNotContains(t, ir, "define { i1, i8* } @R.close$view_adapt_as_Q")
}

// T1952: slotShapeDiffers is substitution-free, so a generic parent whose slot is
// described by an unbound `T?` must not read as different from a concrete `int?`.
// If it did, every generic `is` would pay for a view vtable it does not need —
// the cost T1734 promises an `is` clause never has.
func TestT1952GenericParentExactMatchNeedsNoViewVtable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder[T] `+"`structural `public"+` { peek(~this) T? `+"`abstract"+`; }
		type IntBox is Holder[int] `+"`public"+` { int v; peek(~this) int? { return this.v; } }
		main() { Holder[int] a = IntBox(v: 7); a.peek(); }
	`)
	codegentest.AssertNotContains(t, ir, "IntBox_as_")
	codegentest.AssertNotContains(t, ir, "peek$view_adapt")
}

// T1952: the same generic parent WITH a relaxation. The adapter must be built
// against the view's SUBSTITUTED signature — `int?` is `{ i1, i64 }`, not the
// unbound `T?`'s i8* — which is what T1735's typeSubst in getOrEmitViewVtable
// provides. A raw `T?` here would emit a slot the call site cannot read.
func TestT1952GenericParentRelaxationAdaptsToTheSubstitutedShape(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder[T] `+"`structural `public"+` { peek(~this) T? `+"`abstract"+`; }
		type IntBox is Holder[int] `+"`public"+` { int v; peek(~this) int { return this.v; } }
		main() { Holder[int] a = IntBox(v: 7); a.peek(); }
	`)
	codegentest.AssertContains(t, ir, "define i64 @IntBox.peek(i8* %this)")
	codegentest.AssertContains(t, ir, "define { i1, i64 } @IntBox.peek$view_adapt_as_Holder(i8* %this)")
	codegentest.AssertContainsMatch(t,
		ir, `@"promise_vtable_IntBox_as_Holder\[int\]" = constant \[1 x i8\*\] \[i8\* bitcast \(\{ i1, i64 \} \(i8\*\)\* @IntBox\.peek\$view_adapt_as_Holder`)
}
