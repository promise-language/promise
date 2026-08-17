package codegen

import (
	"strings"
	"testing"
)

// T1559: a concrete type that inherits a *default getter* from a *generic*
// `structural` interface type-checked clean but panicked in codegen. genGetterCall
// resolved the inherited owner straight through resolveMonoParentName, which for a
// generic parent returns the interface instance name (`Holder[int]`), so it looked
// up `Holder[int].doubled_item` — a function that was never declared, since the
// generic-structural-default machinery synthesizes the default under the *concrete*
// implementor's name (`IntBox.doubled_item`). The fix makes genGetterCall detect a
// structural parent (via findStructuralOwnerBy + LookupGetter) and dispatch to the
// concrete's synthesized getter, mirroring genMethodCall.
//
// Iterator[T]'s 20 default combinators are all methods, not getters, so the stdlib
// never exercised this path.
func TestT1559GenericStructuralDefaultGetter(t *testing.T) {
	ir := generateIR(t, `
		type Holder[T] `+"`structural"+` {
			get item T `+"`abstract"+`;
			get doubled_item T => this.item;
		}
		type IntBox is Holder[int] {
			int raw;
			get item int => this.raw;
		}
		main() {
			b := IntBox(raw: 3);
			b.doubled_item.to_string();
		}
	`)

	// The default getter must be synthesized under the concrete implementor's name
	// and defined, not the (never-declared) generic interface-instance name.
	fn := extractDefine(ir, "IntBox.doubled_item")
	if fn == "" {
		t.Fatal("expected synthesized @IntBox.doubled_item to be defined")
	}
	assertContains(t, fn, "@IntBox.item")
	assertNotContains(t, ir, "Holder[int].doubled_item")
}

// T1559 setter side: a default *setter* (with a matching getter, so it reaches
// codegen) inherited from a generic `structural` interface hit the identical panic
// in genSetterCall. The fix uses findStructuralOwnerBy + LookupSetter and the $set
// slot key against the concrete type's name.
func TestT1559GenericStructuralDefaultSetter(t *testing.T) {
	ir := generateIR(t, `
		type Proxy[T] `+"`structural"+` {
			get raw T `+"`abstract"+`;
			set raw(T v) `+"`abstract"+`;
			get proxy T => this.raw;
			set proxy(T v) { this.raw = v; }
		}
		type IntCell is Proxy[int] {
			int stored;
			get raw int => this.stored;
			set raw(int v) { this.stored = v; }
		}
		main() {
			b := IntCell(stored: 0);
			b.proxy = 9;
			b.proxy.to_string();
		}
	`)

	if !strings.Contains(ir, "define void @IntCell.proxy$set(") {
		t.Fatal("expected synthesized @IntCell.proxy$set to be defined")
	}
	assertNotContains(t, ir, "Proxy[int].proxy$set")
}

// T1559 boundary case B: the non-generic structural default getter also flows
// through the per-concrete synthesis path after the fix and must stay correct.
func TestT1559NonGenericStructuralDefaultGetter(t *testing.T) {
	ir := generateIR(t, `
		type Holder `+"`structural"+` {
			get item int `+"`abstract"+`;
			get doubled_item int => this.item * 2;
		}
		type IntBox is Holder {
			int raw;
			get item int => this.raw;
		}
		main() {
			b := IntBox(raw: 3);
			b.doubled_item.to_string();
		}
	`)
	fn := extractDefine(ir, "IntBox.doubled_item")
	if fn == "" {
		t.Fatal("expected synthesized @IntBox.doubled_item (non-generic default getter) to be defined")
	}
	assertContains(t, fn, "@IntBox.item")
}

// T1559 regression guard: a getter that is *concretely* implemented on a
// non-structural intermediate class, while a structural ancestor only declares it
// `abstract, must dispatch to the intermediate's implementation — NOT be routed
// through the per-concrete synthesis path (which would mangle `Leaf.item`, a
// function that is never emitted, and panic "undeclared getter Leaf.item").
// The first cut of the T1559 fix treated any getter with a structural ancestor
// declaring it as a synthesized default, regressing this previously-working shape.
func TestT1559GetterConcretelyOverriddenOnIntermediate(t *testing.T) {
	ir := generateIR(t, `
		type OverrideBase `+"`structural"+` {
			get item int `+"`abstract"+`;
		}
		type OverrideMid is OverrideBase {
			int raw;
			get item int => this.raw;
		}
		type OverrideLeaf is OverrideMid {
		}
		main() {
			x := OverrideLeaf(raw: 5);
			x.item.to_string();
		}
	`)
	// The getter dispatches to the concrete intermediate's implementation...
	assertContains(t, ir, "call i64 @OverrideMid.item(")
	// ...and no per-concrete getter was synthesized under the leaf (would have
	// been the panicking, never-defined name).
	assertNotContains(t, ir, "OverrideLeaf.item")
}

// T1559: a default getter inherited from a *generic* structural interface has
// return type `T`. When the concrete implementor is a plain Named (not a generic
// Instance), the interface's TypeParam→concrete mapping lives in the implementor's
// parent ref, which none of the ambient getter-result substitutions carry — so the
// owned heap result (a returned string here) was never registered for drop and
// leaked. The fix falls back to sema's resolved member type. The IR must register
// the discarded getter result temp for cleanup (a drop flag store follows the call).
func TestT1559HeapReturningGenericStructuralDefaultGetterDropsResult(t *testing.T) {
	ir := generateIR(t, `
		type Holder[T] `+"`structural"+` {
			get val T `+"`abstract"+`;
			get echoed T => this.val;
		}
		type Entry is Holder[string] {
			string v;
			get val string => this.v;
		}
		main() {
			e := Entry(v: "hi");
			e.echoed;
		}
	`)
	// The synthesized getter is called and its owned string result is dropped —
	// the tracked temp is freed via promise_string_drop rather than leaked.
	assertContains(t, ir, "call i8* @Entry.echoed(")
	assertContains(t, ir, "call void @promise_string_drop(")
}

// T1559: a generic structural interface with *two* type parameters — both default
// getters must be synthesized per-concrete and substitute their respective params.
func TestT1559MultiParamGenericStructuralDefaultGetters(t *testing.T) {
	ir := generateIR(t, `
		type Pair[K, V] `+"`structural"+` {
			get key K `+"`abstract"+`;
			get val V `+"`abstract"+`;
			get first K => this.key;
			get second V => this.val;
		}
		type Entry is Pair[int, string] {
			int k;
			string v;
			get key int => this.k;
			get val string => this.v;
		}
		main() {
			e := Entry(k: 9, v: "hi");
			e.first.to_string();
			e.second.to_string();
		}
	`)
	if extractDefine(ir, "Entry.first") == "" {
		t.Fatal("expected synthesized @Entry.first (K=int default getter) to be defined")
	}
	if extractDefine(ir, "Entry.second") == "" {
		t.Fatal("expected synthesized @Entry.second (V=string default getter) to be defined")
	}
	assertNotContains(t, ir, "Pair[int__string].first")
}

// T1559: a default getter reached through a *non-structural* intermediate that
// merely inherits the default (does not override it) must still synthesize under
// the concrete implementor. This is the only shape that drives findStructuralOwnerBy's
// recursion INTO a non-structural parent (ownsConcreteMember false → recurse):
// the grandparent case recurses through a structural interface and returns at the
// first structural owner, and the override case stops via ownsConcreteMember.
func TestT1559DefaultGetterThroughNonStructuralIntermediate(t *testing.T) {
	ir := generateIR(t, `
		type RBase[T] `+"`structural"+` {
			get item T `+"`abstract"+`;
			get echoed T => this.item;
		}
		type RMid is RBase[int] {
			int raw;
			get item int => this.raw;
		}
		type RLeaf is RMid {
		}
		main() {
			x := RLeaf(raw: 9);
			x.echoed.to_string();
		}
	`)
	// The default getter is synthesized under the concrete leaf implementor, not
	// the non-structural intermediate or the generic interface instance.
	if extractDefine(ir, "RLeaf.echoed") == "" {
		t.Fatal("expected synthesized @RLeaf.echoed (default reached through non-structural intermediate) to be defined")
	}
	assertNotContains(t, ir, "RBase[int].echoed")
}

// T1559: a concrete type inheriting two structural interfaces, where the FIRST
// parent in declaration order does not declare the default member — findStructuralOwnerBy
// must skip it (lookup returns nil → continue) and find the owner in the second parent.
func TestT1559DefaultGetterSecondStructuralParent(t *testing.T) {
	ir := generateIR(t, `
		type HasTag `+"`structural"+` {
			get tag int `+"`abstract"+`;
		}
		type HasEcho[T] `+"`structural"+` {
			get item T `+"`abstract"+`;
			get echoed T => this.item;
		}
		type MultiBox is HasTag, HasEcho[int] {
			int raw;
			get item int => this.raw;
			get tag int => 7;
		}
		main() {
			b := MultiBox(raw: 9);
			b.echoed.to_string();
		}
	`)
	if extractDefine(ir, "MultiBox.echoed") == "" {
		t.Fatal("expected synthesized @MultiBox.echoed (owner in second structural parent) to be defined")
	}
	assertNotContains(t, ir, "HasEcho[int].echoed")
}

// T1559: a default getter living on a structural *grandparent*, reached through a
// second structural interface, must still synthesize under the concrete implementor
// (exercises findStructuralOwnerBy's recursion + transitive-parent substitution).
func TestT1559StructuralGrandparentDefaultGetter(t *testing.T) {
	ir := generateIR(t, `
		type ChainBase[T] `+"`structural"+` {
			get item T `+"`abstract"+`;
			get echoed T => this.item;
		}
		type ChainDerived[T] is ChainBase[T] `+"`structural"+` {
			get extra T `+"`abstract"+`;
		}
		type ChainBox is ChainDerived[int] {
			int raw;
			get item int => this.raw;
			get extra int => this.raw;
		}
		main() {
			b := ChainBox(raw: 7);
			b.echoed.to_string();
		}
	`)
	if extractDefine(ir, "ChainBox.echoed") == "" {
		t.Fatal("expected synthesized @ChainBox.echoed (grandparent default getter) to be defined")
	}
	assertContains(t, extractDefine(ir, "ChainBox.echoed"), "@ChainBox.item")
}
