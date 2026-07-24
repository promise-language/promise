package codegen

import (
	"strings"
	"testing"
)

// T1305: a discarded / inline-consumed structural-interface return from a free
// function that ALSO takes a heap-typed argument must still be freed when the
// callee's return provably does NOT alias that argument. The T1294 fallback
// conservatively rejected any call with a heap-typed arg (leaking a genuinely
// fresh return); the sema return-alias fact (StructuralReturnAliasParams) now
// admits the provable cases: an owned index clone (`return v[0]`) through a
// borrow arg, and a fresh construction (`return Widget(...)`) through a moved arg.

const t1305Prelude = `
	type Showable ` + "`" + `structural { to_string() string ` + "`" + `abstract; }
	type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
	type Sink ` + "`" + `structural { emit(~this, int n) ` + "`" + `abstract; }
	type Counter {
		int total;
		emit(~this, int n) { this.total = this.total + n; }
	}
	first_of(Sink[] v) Sink { return v[0]; }
	make_vec() Sink[] {
		r := Vector[Sink]();
		r.push(Counter(total: 1));
		return r;
	}
	make_from(Widget ~w) Showable { return Widget(id: w.id); }
	get_widget() Widget { return Widget(id: 7); }
	pass_through(Sink s) Sink { return s; }
	mk_sink(int n) Sink { return Counter(total: n); }
	get_counter() Sink { return Counter(total: 3); }
`

// A discarded structural return that is an owned index clone (`first_of(make_vec())`)
// — the callee's `Sink[] v` borrow arg is a heap box, but the return is a fresh
// deep-cloned element that aliases nothing. It must be routed through
// __promise_structural_drop.
func TestT1305DiscardedIndexCloneDropsBox(t *testing.T) {
	ir := generateIR(t, t1305Prelude+`
		build() { first_of(make_vec()); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected discarded `first_of(make_vec());` fresh index-clone box to drop via @__promise_structural_drop; got:\n%s", fn)
	}
}

// A discarded structural return that is a fresh construction reached through a
// MOVED heap argument (`make_from(get_widget())`) — the moved arg is consumed by
// the callee, and the return is a fresh `Widget(...)`. Must be tracked.
func TestT1305DiscardedFreshFromMovedArgDropsBox(t *testing.T) {
	ir := generateIR(t, t1305Prelude+`
		build() { make_from(get_widget()); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected discarded `make_from(get_widget());` fresh box to drop via @__promise_structural_drop; got:\n%s", fn)
	}
}

// The T1294 no-double-free invariant must survive the T1305 change: a free
// function returning its structural ARGUMENT, where the argument is a fresh temp
// (`pass_through(mk_sink(1))`), must activate exactly ONE heap temp — the inner
// `mk_sink(1)` box. The return-alias fact records alias[0]==true for pass_through,
// so the outer result is NOT tracked (no second live flag → no double-free).
func TestT1305PassthroughOfFreshTempDropsOnce(t *testing.T) {
	ir := generateIR(t, t1305Prelude+`
		build() { pass_through(mk_sink(1)); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if n := strings.Count(fn, "store i1 true"); n != 1 {
		t.Fatalf("expected exactly 1 activated heap temp for `pass_through(mk_sink(1));`, got %d:\n%s", n, fn)
	}
}

// A discarded structural return from a passthrough of a NAMED local
// (`pass_through(c)`) must NOT be tracked — the local `c` is still owned by the
// caller and its own drop frees the box. alias[0]==true keeps the conservative
// reject. Counter has no drop of its own, so the only structural drop that could
// appear would be the erroneously-tracked passthrough temp.
func TestT1305PassthroughOfNamedLocalDoesNotDrop(t *testing.T) {
	ir := generateIR(t, t1305Prelude+`
		build() { c := Counter(total: 7); pass_through(c); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected discarded `pass_through(c);` NOT to drop the aliased local box; got:\n%s", fn)
	}
}
