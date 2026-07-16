package codegen

import (
	"strings"
	"testing"
)

// T1292: Map[K, structural-interface] must drop its stored value boxes. A `Map`
// whose value type is a non-value structural interface stores each value as a
// heap-boxed {vtable, instance} view inside `Slot[K, V].Used`. Before the fix,
// `Slot[int, Showable]` never got a synthesized drop (monoTypeHasDroppable
// excluded structural interfaces), so `Map.drop` called plain `Vector.drop` and
// every boxed value leaked. Sibling of the Vector element-drop fix (T1283/T1284)
// for the Map value slot.

// The synthesized `Slot[int, Showable].drop` must exist and route its value slot
// through __promise_structural_drop (RTTI drop of the boxed heap instance).
func TestT1292SlotStructuralValueGetsDrop(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		show(int n) Showable { return Widget(id: n); }
		build() { m := Map[int, Showable](); m[1] = show(1); }
	`)
	drop := extractDefine(ir, "Slot[int, Showable].drop")
	if drop == "" {
		t.Fatalf("expected a synthesized @\"Slot[int, Showable].drop\" (monoTypeHasDroppable must classify a structural-interface variant field as droppable); not found in IR:\n%s", ir)
	}
	// The value slot's box is freed via the RTTI structural-drop routing.
	if !strings.Contains(drop, "__promise_structural_drop") {
		t.Fatalf("expected Slot[int, Showable].drop to route the value box through @__promise_structural_drop; got:\n%s", drop)
	}
}

// `Map[int, Showable].drop` must iterate its bucket vector and drop each Slot
// element (the element-drop loop), rather than calling a bare Vector.drop that
// leaks the boxes.
func TestT1292MapDropWalksStructuralElements(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		show(int n) Showable { return Widget(id: n); }
		build() { m := Map[int, Showable](); m[1] = show(1); }
	`)
	drop := extractDefine(ir, "Map[int, Showable].drop")
	if drop == "" {
		t.Fatalf("could not extract @\"Map[int, Showable].drop\" from IR:\n%s", ir)
	}
	// The bucket vector's element-drop loop calls the per-element Slot drop.
	if !strings.Contains(drop, `"Slot[int, Showable].drop"`) {
		t.Fatalf("expected Map[int, Showable].drop to call Slot[int, Showable].drop for each bucket element (element-drop loop); got:\n%s", drop)
	}
}
