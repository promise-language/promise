package ownership

import "testing"

// T1227: a closure (function value) field owns a heap-allocated env struct freed
// by the owner's synthesized drop. Moving it out of its owner in a consuming
// context (return, `~`/move sink, owning-slot store) aliases that env while the
// owner still frees it → double-free. Closures are move-only single-owner values
// with no clone, so this must be rejected — exactly like the native single-owner
// handles (Mutex/Task).

func TestT1227ReturnClosureFieldFromBorrowRejected(t *testing.T) {
	errs := ownerErrs(t, `
type Holder {
  () -> int cb;
  get_cb(this) () -> int { return this.cb; }
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move closure field")
}

func TestT1227ReturnClosureFieldFromGetterRejected(t *testing.T) {
	errs := ownerErrs(t, `
type Holder {
  () -> int cb;
  get cb_getter () -> int { return this.cb; }
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move closure field")
}

// The item suggests "require ~this", but ~this + `return this.cb` also double-frees
// (no codegen path moves a closure field out of an owned receiver, exactly like
// Mutex). Lock in the reject-not-lie decision: it is rejected even with ~this.
func TestT1227ReturnClosureFieldFromConsumingReceiverRejected(t *testing.T) {
	errs := ownerErrs(t, `
type Holder {
  () -> int cb;
  take_cb(~this) () -> int { return this.cb; }
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move closure field")
}

// Optional[() -> int] field is exactly as unsafe to move out as the bare closure —
// the helper peels the Optional layer.
func TestT1227ReturnOptionalClosureFieldRejected(t *testing.T) {
	errs := ownerErrs(t, `
type Holder {
  (() -> int)? cb;
  get_cb(this) (() -> int)? { return this.cb; }
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move closure field")
}

// Moving out of the *owner is a local variable*, not `this` (return h.cb from a
// borrowed `Holder` param) — exercises the IdentExpr value-target branch of the
// gate, not just the ThisExpr branch the method/getter tests cover.
func TestT1227ReturnClosureFieldFromOwnedLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
type Holder {
  () -> int cb;
}
extract(Holder h) () -> int { return h.cb; }
main() {}
`)
	expectOwnerError(t, errs, "cannot move closure field")
}

// Consuming context via a move-sink (`move` param), not a return — this reaches
// checkFieldMoveOwnership through tryMoveConsume (the second call site), covering
// the comment's "`~`/move sink" claim that the return tests do not.
func TestT1227PassClosureFieldToMoveSinkRejected(t *testing.T) {
	errs := ownerErrs(t, `
type Holder {
  () -> int cb;
  give(this) { consume(this.cb); }
}
consume((() -> int) move f) int { return f(); }
main() {}
`)
	expectOwnerError(t, errs, "cannot move closure field")
}

// Consuming context via an owning-slot store (storing `this.cb` into a fresh
// constructor field) — the third context named in the fix comment.
func TestT1227StoreClosureFieldIntoOwningSlotRejected(t *testing.T) {
	errs := ownerErrs(t, `
type Holder {
  () -> int cb;
  wrap(this) Holder { return Holder(cb: this.cb); }
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move closure field")
}

// Nested Optional[Optional[closure]] — exercises the multi-iteration peel loop in
// isClosureFieldType (the single-Optional test only runs it once).
func TestT1227ReturnDoubleOptionalClosureFieldRejected(t *testing.T) {
	errs := ownerErrs(t, `
type Holder {
  ((() -> int)?)? cb;
  get_cb(this) ((() -> int)?)? { return this.cb; }
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move closure field")
}

// Safe borrow shapes must stay accepted: binding the field to a local (`f := h.cb`),
// calling it through the receiver (`this.cb()`), and passing it as a non-`~` arg.
func TestT1227ClosureFieldBorrowShapesAccepted(t *testing.T) {
	ownerOK(t, `
type Holder {
  () -> int cb;
  invoke(this) int { return this.cb(); }
}
apply(() -> int f) int { return f(); }
main() {
  h := Holder(cb: || -> 1);
  f := h.cb;
  x := apply(h.cb);
  y := h.invoke();
}
`)
}
