package codegen

import (
	"strings"
	"testing"
)

// T1294: a discarded / inline-consumed expression statement whose value is a
// non-value structural-interface (`{vtable, instance}` fat view) returned by a
// fresh-constructing FREE-FUNCTION call must free its boxed heap instance at
// statement end via RTTI drop dispatch (__promise_structural_drop). Aliasing
// shapes — a method returning `this` (`c.get_self()`), an iterator adapter over
// the receiver (`v.iter()`), and an owned-arg passthrough (`pass_through(s)`) —
// must NOT be tracked, since their owner / iter-cleanup already frees the box.

const t1294Prelude = `
	type Showable ` + "`" + `structural { to_string() string ` + "`" + `abstract; }
	type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
	show(int n) Showable { return Widget(id: n); }
	type Sink ` + "`" + `structural { emit(int n) ` + "`" + `abstract; }
	type Counter {
		int total;
		emit(~this, int n) { this.total = this.total + n; }
		get_self() Sink { return this; }
	}
	pass_through(Sink s) Sink { return s; }
	mk_sink(int n) Sink { return Counter(total: n); }
	apply(() -> Sink fn) Sink { return fn(); }
`

// A discarded fresh-owned free-function structural return (`show(1);`) must be
// routed through __promise_structural_drop at statement end.
func TestT1294DiscardedFreshCallDropsBox(t *testing.T) {
	ir := generateIR(t, t1294Prelude+`
		build() { show(1); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected discarded `show(1);` to drop the owned structural box via @__promise_structural_drop; got:\n%s", fn)
	}
}

// An inline receiver on a fresh-owned free-function structural return
// (`show(3).to_string();`) must still drop the inner `show(3)` box.
func TestT1294InlineFreshCallDropsBox(t *testing.T) {
	ir := generateIR(t, t1294Prelude+`
		build() { show(3).to_string(); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected inline `show(3).to_string();` to drop the owned structural box via @__promise_structural_drop; got:\n%s", fn)
	}
}

// A free function returning its owned structural ARGUMENT (`pass_through(s);`)
// aliases the caller's still-owned `s`; it must NOT be tracked (owner frees it).
func TestT1294DiscardedPassthroughDoesNotDrop(t *testing.T) {
	ir := generateIR(t, t1294Prelude+`
		build() { c := Counter(total: 7); pass_through(c); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	// Counter has no drop of its own, so the only structural drop that could
	// appear would be the erroneously-tracked passthrough temp. Assert none.
	if strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected discarded `pass_through(c);` NOT to drop the aliased argument box; got:\n%s", fn)
	}
}

// A free function returning its structural argument, where that argument is a
// FRESH temp (`pass_through(mk_sink(1));`), aliases the inner temp's box. The
// inner `mk_sink(1)` is itself tracked as fresh-owned, so it frees the box once.
// The outer `pass_through(...)` must NOT be tracked — doing so would drop the SAME
// box a second time (T1294 double-free / segfault). Assert exactly one structural
// drop site (the inner temp), not two.
func TestT1294PassthroughOfFreshTempDropsOnce(t *testing.T) {
	ir := generateIR(t, t1294Prelude+`
		build() { pass_through(mk_sink(1)); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	// Exactly ONE heap temp is activated (the inner `mk_sink(1)` box). If the outer
	// `pass_through(...)` result were also tracked, a second temp's live flag would
	// be set — that is the double-free the fix prevents. (The drop itself may appear
	// on two control-flow paths — normal exit and panic cleanup — for the single
	// temp, so counting `@__promise_structural_drop` call sites is not the invariant;
	// counting activated temps is.)
	if n := strings.Count(fn, "store i1 true"); n != 1 {
		t.Fatalf("expected exactly 1 activated heap temp (inner fresh box only) for `pass_through(mk_sink(1));`, got %d:\n%s", n, fn)
	}
}

// A method returning `this` (`c.get_self();`) is a borrowed view. isFreshOwned-
// StructuralCall rejects it (callee is a MemberExpr), so T1294 adds no new drop.
// The pre-existing receiver-alias mechanism still emits a runtime-GATED
// structural_drop whose flag is cleared when the returned ptr equals the
// receiver's instance ptr — so `c.get_self()` frees nothing at runtime (the
// discard_self Promise e2e test asserts no double-free). This test pins that the
// tracked temp is guarded by a claim (the `heap.claim` alias-clear block), not
// dropped unconditionally.
func TestT1294DiscardedSelfMethodIsAliasGuarded(t *testing.T) {
	ir := generateIR(t, t1294Prelude+`
		build() { c := Counter(total: 5); c.get_self(); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heap.claim") {
		t.Fatalf("expected discarded `c.get_self();` structural temp to be receiver-alias guarded (heap.claim block); got:\n%s", fn)
	}
}

// A free function whose only argument is a LAMBDA (`apply(|| -> mk_sink(1));`)
// returns whatever the lambda produces — a fresh box, since the lambda constructs
// it inline. A function-typed argument carries no heap box of its own to alias
// (argMayAliasStructuralReturn extractNamed→nil→false), so it does NOT disqualify
// the call. isFreshOwnedStructuralCall admits it and routes the extracted instance
// ptr through __promise_structural_drop — freeing the box exactly once (no leak,
// no double-free, since nothing else owns it). Pins the named==nil arg branch and
// the fresh-owned true path reached via a non-heap (lambda) argument.
func TestT1294FreshCallWithLambdaArgDropsBox(t *testing.T) {
	ir := generateIR(t, t1294Prelude+`
		build() { apply(|| -> mk_sink(1)); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected `apply(|| -> mk_sink(1));` fresh structural box to drop via @__promise_structural_drop; got:\n%s", fn)
	}
}

// A discarded GENERATOR call (`gen(3);`, return type `stream[int]`) yields an
// iterator-adapter box whose non-standard-RTTI `_FnIter`-shaped layout would be
// double-dropped / crash if routed through __promise_structural_drop. isFresh-
// OwnedStructuralCall REJECTS it (isIteratorAdapterName true on the return type),
// so T1294 adds no structural drop. This intentionally leaves the box leaking when
// never iterated — a separate pre-existing bug tracked as T1306. This test pins the
// exclusion: the discarded generator must NOT be routed through structural drop
// (guarding against a regression that would turn the leak into a double-free).
func TestT1294DiscardedGeneratorNotStructuralDropped(t *testing.T) {
	ir := generateIR(t, `
		gen(int n) stream[int] { int i = 0; while i < n { yield i; i = i + 1; } }
		build() { gen(3); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected discarded generator `gen(3);` (iterator adapter) NOT to be routed through @__promise_structural_drop (T1306); got:\n%s", fn)
	}
}
