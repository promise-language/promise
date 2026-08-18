package sema

import "testing"

// T1585: a force-unwrapped container element (`m[k]!`) passed to a `~`
// (mutable-borrow) parameter must be rejected — `Map.[]` returns Optional[V] and
// `!` unwraps a genuine copy, so the callee's mutation would be silently
// discarded. This is the sema half of the fix (the codegen half writes back a
// Vector/array element slot; the map case has no stable address to write to, so
// it becomes a compile error instead of a silent no-op).

func TestT1585_MapUnwrapToMutRefParamRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Plain { int n `+"`value"+`; }
		bump(Plain ~p) { p.n = 21; }
		probe() {
			map[string, Plain] m = map[string, Plain]();
			m["a"] = Plain(n: 1);
			bump(m["a"]!);
		}
	`)
	expectError(t, errs, "cannot pass a force-unwrapped container element to mutable-borrow parameter")
}

// A parenthesized force-unwrap `(m[k])!` must be rejected too — isUnwrappedContainerIndexArg
// peels the ParenExpr before matching the inner IndexExpr, so wrapping in parens
// does not smuggle a copy past the mut-ref gate.
func TestT1585_ParenMapUnwrapToMutRefParamRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Plain { int n `+"`value"+`; }
		bump(Plain ~p) { p.n = 21; }
		probe() {
			map[string, Plain] m = map[string, Plain]();
			m["a"] = Plain(n: 1);
			bump((m["a"])!);
		}
	`)
	expectError(t, errs, "cannot pass a force-unwrapped container element to mutable-borrow parameter")
}

// A `container[k]!` argument to a `move` (consuming) param is a legitimate
// consume of a copy — it must NOT trigger the mut-ref rejection.
func TestT1585_MapUnwrapToMoveParamAllowed(t *testing.T) {
	errs := checkErrs(t, `
		type Plain { int n `+"`value"+`; }
		consume(Plain move p) Plain { return p; }
		probe() {
			map[string, Plain] m = map[string, Plain]();
			m["a"] = Plain(n: 1);
			x := consume(m["a"]!);
		}
	`)
	expectNoErrorContaining(t, errs, "cannot pass a force-unwrapped container element to mutable-borrow parameter")
}

// T1585 (soundness gate): passing a container element `v[0]` to a `~` param now
// writes back to the element (genMutRefArg). If the container is only a shared
// (read-only) borrow, that write-back would mutate through the borrow — the same
// hole checkReceiverMutability/checkMutation reject for `v[0].m()` / `v[0].f = x`.
// It must be rejected, not silently escape through the borrow.
func TestT1585_VectorElementToMutRefThroughSharedBorrowRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Plain { int n `+"`value"+`; }
		bump(Plain ~p) { p.n = 21; }
		observe(Plain[] v) { bump(v[0]); }
	`)
	expectError(t, errs, "cannot pass a container element through a shared (read-only) borrow to mutable-borrow parameter")
}

// The same call through a `~` (mutable) borrow of the container is legitimate —
// the write-back reaches the caller's element, so it must NOT be rejected.
func TestT1585_VectorElementToMutRefThroughMutBorrowAllowed(t *testing.T) {
	errs := checkErrs(t, `
		type Plain { int n `+"`value"+`; }
		bump(Plain ~p) { p.n = 21; }
		observe(Plain[] ~v) { bump(v[0]); }
	`)
	expectNoErrorContaining(t, errs, "shared (read-only) borrow")
}

// An owned local container is a mutable place — `bump(v[0])` must compile.
func TestT1585_VectorElementToMutRefFromOwnedLocalAllowed(t *testing.T) {
	errs := checkErrs(t, `
		type Plain { int n `+"`value"+`; }
		bump(Plain ~p) { p.n = 21; }
		probe() {
			Plain[] v = [Plain(n: 1)];
			bump(v[0]);
		}
	`)
	expectNoErrorContaining(t, errs, "shared (read-only) borrow")
}
