package codegen

import (
	"strings"
	"testing"
)

// T1321: a temporary structural-interface method receiver produced by a bare
// module getter (`get sink Sink { ... }` accessed as `sink`) must be tracked in
// the CALLER via __promise_structural_drop so its freshly-constructed heap box is
// freed exactly once at statement end. Module getters take no receiver and always
// construct fresh owned values, so tracking is never an alias/double-free hazard.
// Sibling of T1299 (instance-getter receivers), on the bare-module-getter path
// through genIdentExpr → trackGetterResultByType → trackHeapUserTypeResult.

// A bare module getter used as a temporary method receiver (`sink.emit(2)`) must
// drop the caller-owned structural box via __promise_structural_drop.
func TestT1321ModuleGetterReceiverDropsBox(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(~this, int x) int `+"`"+`abstract; }
		type Counter { int base `+"`"+`value; emit(~this, int x) int { return this.base + x; } }
		get sink Sink { return Counter(base: 5); }
		build() { int r = sink.emit(2); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected the temp `sink.emit(2)` receiver to be dropped via @__promise_structural_drop; got:\n%s", fn)
	}
}

// A bare module getter used as a discarded expression statement (`sink;`) must
// also drop the caller-owned structural box at statement end.
func TestT1321ModuleGetterDiscardDropsBox(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(~this, int x) int `+"`"+`abstract; }
		type Counter { int base `+"`"+`value; emit(~this, int x) int { return this.base + x; } }
		get sink Sink { return Counter(base: 5); }
		build() { sink; }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected the discarded `sink;` temp to be dropped via @__promise_structural_drop; got:\n%s", fn)
	}
}

// T1321 generalized the bare-module-getter path (genIdentExpr) from string-only
// (T0137) to every heap-owning result kind via trackGetterResultByType. A bare
// module getter returning a heap Vector, used as a discarded temp, must drop the
// caller-owned vector at statement end.
func TestT1321ModuleGetterVectorDiscardDrops(t *testing.T) {
	ir := generateIR(t, `
		get heap_vec int[] { int[] v = []; v.push(1); return v; }
		build() { heap_vec; }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "@Vector.drop") {
		t.Fatalf("expected the discarded `heap_vec;` temp to be dropped via @Vector.drop; got:\n%s", fn)
	}
}

// T1240 branch: a bare module getter returning a function type yields an owned
// closure whose heap env must be freed. Discarding it registers the env (field 1)
// as an env temp — emitted as an env cleanup block at statement end.
func TestT1321ModuleGetterClosureDiscardFreesEnv(t *testing.T) {
	ir := generateIR(t, `
		get boxed_adder (int) -> int { int base = 10; return |int x| -> base + x; }
		build() { boxed_adder; }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "env.tmp.drop") {
		t.Fatalf("expected the discarded closure getter to free its env via an env.tmp.drop block; got:\n%s", fn)
	}
}
