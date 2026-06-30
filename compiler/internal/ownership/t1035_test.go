package ownership

import "testing"

// T1035: a generic for-in drain over a bare-TypeParam container element
// (`for x in v { sink.push(move x); }`) silently double-freed when instantiated
// with a non-Copy, non-string T — the for-in alias guard excludes bare
// TypeParams, and the generic body is only checked once with T unbound. The fix
// defers the verdict to each concrete instantiation via GenericCallEdges.

const t1035Drain = `
	type Item { int id; drop(~this) {} }
	drain[T](T[] v) T[] {
		sink := T[]();
		for x in v { sink.push(move x); }
		return sink;
	}
`

// --- Rejected: non-Copy, non-string instantiations ---

func TestT1035_PushMoveNonCopyRejected(t *testing.T) {
	errs := ownerErrs(t, t1035Drain+`
		go_drain() {
			items := Item[]();
			items.push(Item(id: 1));
			d := drain(items);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Item")
	expectOwnerError(t, errs, "for-in loop binding 'x'")
}

func TestT1035_ReturnMoveNonCopyRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Item { int id; drop(~this) {} }
		pick[T](T[] v) T? {
			for x in v { return x; }
			return none;
		}
		go_pick() {
			items := Item[]();
			items.push(Item(id: 1));
			p := pick(items);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Item")
}

func TestT1035_ConsumeParamNonCopyRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Item { int id; drop(~this) {} }
		sink_it[T](T move b) {}
		drain[T](T[] v) {
			for x in v { sink_it(move x); }
		}
		go_drain() {
			items := Item[]();
			items.push(Item(id: 1));
			drain(items);
		}
	`)
	// The consume-param drain is monomorphic-shaped but still generic in the
	// body; only when sink_it's param is exactly Item does the move consume.
	expectOwnerError(t, errs, "cannot instantiate generic with Item")
}

func TestT1035_TransitiveNonCopyRejected(t *testing.T) {
	errs := ownerErrs(t, t1035Drain+`
		outer[U](U[] v) U[] { return drain(v); }
		go_outer() {
			items := Item[]();
			items.push(Item(id: 1));
			d := outer(items);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Item")
}

func TestT1035_TransitiveThroughMethodNonCopyRejected(t *testing.T) {
	// A generic *method* whose body calls the generic `drain` func forwards the
	// drain requirement onto the method (addDrainReq's method branch); the
	// eventual concrete `b.via()` call instantiates the method with U=Item and
	// triggers validation at the inner drain[U] call site.
	errs := ownerErrs(t, t1035Drain+`
		type Box[U] {
			U[] items;
			via(this) U[] { return drain[U](this.items); }
		}
		go_box() {
			b := Box[Item](items: Item[]());
			d := b.via();
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Item")
	expectOwnerError(t, errs, "for-in loop binding 'x'")
}

func TestT1035_NestedDistinctBindingDrainRejected(t *testing.T) {
	// Two nested for-in loops over distinct bare-TypeParam containers with
	// distinct binding names (same name would be a shadow error) — the inner move
	// drains the inner container. Confirms per-binding TypeParam alias tracking
	// handles concurrently-live nested bindings; a non-Copy instantiation is
	// rejected at the move-out.
	errs := ownerErrs(t, `
		type Item { int id; drop(~this) {} }
		drain2[T](T[] a, T[] b) T[] {
			sink := T[]();
			for y in a {
				for x in b { sink.push(move x); }
			}
			return sink;
		}
		go_drain2() {
			a := Item[]();
			b := Item[]();
			d := drain2(a, b);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Item")
}

func TestT1035_MethodDrainNonCopyRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Item { int id; drop(~this) {} }
		type Box[T] {
			T[] items;
			drain(this) T[] {
				sink := T[]();
				for x in this.items { sink.push(move x); }
				return sink;
			}
		}
		go_method() {
			b := Box[Item](items: Item[]());
			d := b.drain();
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Item")
}

func TestT1035_InferredTypeArgNonCopyRejected(t *testing.T) {
	// `drain(items)` with no explicit [Item] — inferred type arg must still
	// trigger the deferred validation.
	errs := ownerErrs(t, t1035Drain+`
		go_drain() {
			items := Item[]();
			items.push(Item(id: 1));
			items.push(Item(id: 2));
			d := drain(items);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Item")
}

// --- Accepted: no over-rejection ---

func TestT1035_CopyInstantiationOK(t *testing.T) {
	ownerOK(t, t1035Drain+`
		go_drain() {
			items := int[]();
			items.push(1);
			d := drain(items);
		}
	`)
}

func TestT1035_StringInstantiationOK(t *testing.T) {
	ownerOK(t, t1035Drain+`
		go_drain() {
			items := string[]();
			items.push("a");
			d := drain(items);
		}
	`)
}

func TestT1035_ReadOnlyLoopOK(t *testing.T) {
	// No move out of the binding → no requirement recorded, so even a non-Copy
	// instantiation is fine.
	ownerOK(t, `
		type Item { int id; drop(~this) {} }
		count[T](T[] v) int {
			int n = 0;
			for x in v { n = n + 1; }
			return n;
		}
		go_count() {
			items := Item[]();
			items.push(Item(id: 1));
			c := count(items);
		}
	`)
}

func TestT1035_PopTakesOwnershipOK(t *testing.T) {
	// `.pop()` takes ownership of an element — no aliasing, freely movable even
	// for a non-Copy instantiation.
	ownerOK(t, `
		type Item { int id; drop(~this) {} }
		drain_pop[T](T[] v) T[] {
			sink := T[]();
			while v.len > 0 {
				if e := v.pop() { sink.push(move e); }
			}
			return sink;
		}
		go_drain() {
			items := Item[]();
			items.push(Item(id: 1));
			d := drain_pop(items);
		}
	`)
}
