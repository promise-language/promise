package codegen

import (
	"strings"
	"testing"
)

// T1106: `t := go f(arg)` must transfer ownership of a conditional/polymorphic
// heap-arg root — a runtime phi over match/if arms (possibly different concrete
// types) or a nested constructor — into the goroutine frame. For borrow params
// the goroutine-side drop dispatches at runtime through the value's typeinfo
// drop_fn_ptr via __promise_structural_drop (so heterogeneous arms drop the right
// concrete type); the caller clears exactly the live arm's drop flag by runtime
// pointer comparison. For move params the callee consumes the value (no goroutine
// drop). The simple single-root path (T1098) must keep emitting the concrete drop.

func TestT1106GoCallMatchPolyBorrowStructuralDropInCoro(t *testing.T) {
	ir := generateIR(t, `
		type Base { get name string { return "base"; } }
		type Box is Base { string s; get name string { return this.s; } }
		type Widget is Base { string w; string extra; get name string { return this.w; } }
		f(Base b) int { return b.name.len; }
		main() {
			t := go f(match true { true => Box(s: "hi".clone()), _ => Widget(w: "a".clone(), extra: "b".clone()) });
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Heterogeneous concrete types behind Base → the goroutine frame drops the
	// selected value via runtime structural dispatch, never a single static
	// concrete drop (which would run the wrong arm's drop).
	assertContains(t, coro, "@__promise_structural_drop")
	assertNotContains(t, coro, "@Box.drop")
	assertNotContains(t, coro, "@Widget.drop")
}

func TestT1106GoCallMatchPolyMoveNoCoroDrop(t *testing.T) {
	ir := generateIR(t, `
		type Base { get name string { return "base"; } }
		type Box is Base { string s; get name string { return this.s; } }
		type Widget is Base { string w; string extra; get name string { return this.w; } }
		f(Base move b) int { return b.name.len; }
		main() {
			t := go f(match true { true => Box(s: "hi".clone()), _ => Widget(w: "a".clone(), extra: "b".clone()) });
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Move param: the callee consumes and drops the value via its own structural
	// dispatch, so the goroutine body must NOT drop it (would be a double-free).
	assertNotContains(t, coro, "@__promise_structural_drop")
}

func TestT1106GoCallMatchPolyClaimsLiveArmInCaller(t *testing.T) {
	ir := generateIR(t, `
		type Base { get name string { return "base"; } }
		type Box is Base { string s; get name string { return this.s; } }
		type Widget is Base { string w; string extra; get name string { return this.w; } }
		f(Base b) int { return b.name.len; }
		main() {
			t := go f(match true { true => Box(s: "hi".clone()), _ => Widget(w: "a".clone(), extra: "b".clone()) });
			r := <-t;
		}
	`)
	// The caller clears exactly the live arm's heap-temp flag by runtime pointer
	// comparison (claimHeapTemp emits a heap.claim/heap.claim.skip pair per temp).
	assertContains(t, ir, "heap.claim")
}

func TestT1106GoCallNestedCtorBorrowStructuralDropInCoro(t *testing.T) {
	ir := generateIR(t, `
		type Inner { string s; }
		type Outer { Inner inner; }
		f(Outer o) int { return o.inner.s.len; }
		main() {
			t := go f(Outer(inner: Inner(s: "x".clone())));
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Nested constructor leaves multiple live heap temps; the outer value's drop
	// dispatches through structural drop (which recurses into the inner field).
	assertContains(t, coro, "@__promise_structural_drop")
}

func TestT1106GoCallStringPhiBorrowDropsInCoro(t *testing.T) {
	ir := generateIR(t, `
		type R { int n; }
		f(string p) R { return R(n: p.len); }
		main() {
			string a = "abc";
			string b = "wxyz";
			c := a.len > 0;
			t := go f(if c { a.clone() } else { b.clone() });
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// String phi over two owned clones is homogeneous (both promise_string_drop) —
	// the goroutine frame drops the selected clone once.
	assertContains(t, coro, "@promise_string_drop")
	// The caller clears the live arm's stmt-temp flag by runtime comparison.
	assertContains(t, ir, "stmt.claim")
}

func TestT1106GoCallVectorPhiBorrowDropsInCoro(t *testing.T) {
	ir := generateIR(t, `
		f(string[] v) int { return v.len; }
		main() {
			string[] a = ["aa", "bb"];
			string[] b = ["ccc", "ddd", "eee"];
			c := a.len > 0;
			t := go f(if c { a.clone() } else { b.clone() });
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Vector phi over two owned clones is homogeneous (both Vector.drop) — the
	// goroutine frame drops the selected clone once. Homogeneous element type means
	// a single static dropFunc suffices; no runtime structural dispatch needed.
	assertContains(t, coro, "@Vector.drop")
	assertNotContains(t, coro, "@__promise_structural_drop")
	// The caller clears the live arm's stmt-temp flag by runtime comparison.
	assertContains(t, ir, "stmt.claim")
}

func TestT1106GoCallStringPhiMoveNoCoroDrop(t *testing.T) {
	ir := generateIR(t, `
		f(string move p) int { return p.len; }
		main() {
			string a = "abc";
			string b = "wxyz";
			c := a.len > 0;
			t := go f(if c { a.clone() } else { b.clone() });
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Move param: the callee consumes and drops the selected clone, so the goroutine
	// body must NOT drop it (would be a double-free).
	assertNotContains(t, coro, "@promise_string_drop")
	// The caller still clears the live arm's stmt-temp flag (the callee owns it now);
	// without this the caller's scope teardown would double-free.
	assertContains(t, ir, "stmt.claim")
}

func TestT1106GoCallVectorPhiMoveNoCoroDrop(t *testing.T) {
	ir := generateIR(t, `
		f(string[] move v) int { return v.len; }
		main() {
			string[] a = ["aa", "bb"];
			string[] b = ["ccc", "ddd", "eee"];
			c := a.len > 0;
			t := go f(if c { a.clone() } else { b.clone() });
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	// Move param: the callee consumes and drops the selected vector clone, so the
	// goroutine body must NOT drop it.
	assertNotContains(t, coro, "@Vector.drop")
	assertContains(t, ir, "stmt.claim")
}

func TestT1106GoCallIfPolyBorrowStructuralDropInCoro(t *testing.T) {
	// if-expression (not match) over heterogeneous concrete types behind a borrow
	// param — same Case A structural-dispatch path as match, exercised via if.
	ir := generateIR(t, `
		type Base { get name string { return "base"; } }
		type Box is Base { string s; get name string { return this.s; } }
		type Widget is Base { string w; string extra; get name string { return this.w; } }
		f(Base b) int { return b.name.len; }
		main() {
			c := true;
			t := go f(if c { Box(s: "hi".clone()) } else { Widget(w: "a".clone(), extra: "b".clone()) });
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	assertContains(t, coro, "@__promise_structural_drop")
	assertNotContains(t, coro, "@Box.drop")
	assertNotContains(t, coro, "@Widget.drop")
	assertContains(t, ir, "heap.claim")
}

func TestT1106GoCallSingleCtorStillConcreteDrop(t *testing.T) {
	// Regression: a single statically-typed heap root (T1098 simple path) must keep
	// emitting the concrete drop, NOT route through runtime structural dispatch.
	ir := generateIR(t, `
		type R { int n; }
		type Box { string s; }
		f(Box b) R { return R(n: b.s.len); }
		main() {
			string s = "alpha";
			t := go f(Box(s: s.clone()));
			r := <-t;
		}
	`)
	coro := extractGoroutineCoro(t, ir)
	assertContains(t, coro, "@Box.drop")
	if strings.Contains(coro, "@__promise_structural_drop") {
		t.Fatalf("single-root path must use concrete @Box.drop, not structural dispatch\n%s", coro)
	}
}
