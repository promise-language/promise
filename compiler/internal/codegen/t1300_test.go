package codegen

import (
	"strings"
	"testing"
)

// T1300: Overwriting a populated Optional-structural-interface member field
// (`h.s = Counter(...)` when `s: Sink?` is already set) must drop the old
// {vtable, instance} view box before the store. emitOptionalFieldReassignDrop
// gained a structural branch that routes the old box through
// __promise_structural_drop (RTTI: typeinfo.drop_fn_ptr → concrete drop, else
// pal_free), mirroring the scope-exit path (emitOptionalValueDrop, T0460) and the
// vector/map overwrite drops (T1287/T1292). Without it the overwritten view box
// leaks. Reachable only on top of T1298 (widening a subtype to Optional-of-
// structural).
func TestT1300MemberReassignDropsOldStructuralBoxValue(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
		type Counter { int base `+"`"+`value; emit(this, int x) int { return this.base + x; } }
		type OptHolder { Sink? s; }
		build() { OptHolder h = OptHolder(s: Counter(base: 1)); h.s = Counter(base: 2); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected `h.s = Counter(...)` to drop the old structural view box via @__promise_structural_drop; got:\n%s", fn)
	}
}

// Same, with a heap-backed concrete (a field-owning, non-value type) — the old
// box plus its inner heap instance/string must route through the structural drop.
func TestT1300MemberReassignDropsOldStructuralBoxHeap(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
		type Heavy { string tag; emit(this, int x) int { return x + this.tag.len; } }
		type OptHolder { Sink? s; }
		build() { OptHolder h = OptHolder(s: Heavy(tag: "first")); h.s = Heavy(tag: "second"); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected `h.s = Heavy(...)` to drop the old structural view box via @__promise_structural_drop; got:\n%s", fn)
	}
}
