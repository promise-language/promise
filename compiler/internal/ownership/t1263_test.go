package ownership

import "testing"

// T1263: two structural siblings of T1262 that alias a value-copying container of
// closures through DIFFERENT reads than the aliasing-container `[]`:
//   1. native container element read (`w := vv[0]` on Vector[Vector[() -> int]]) —
//      the native Vector `[]` returns the element by value, aliasing the buffer.
//   2. struct-field direct read (`f := h.fns` on H { (() -> int)[] fns; }) —
//      the field read aliases the struct's owned buffer.
// closureAggregateBorrowSource must mark both a BORROW so escapes are rejected;
// same-scope read-and-invoke stays valid. Mirrors T1262/T1230 exactly.

// --- Native vector-of-vector-of-closures index read ---

// Same-scope read-and-invoke is a valid borrow: the outer vector remains sole owner
// and frees the inner vector/envs once at scope exit. Must be accepted.
func TestT1263ReadAndInvokeNativeVecOfVecClosureAccepted(t *testing.T) {
	ownerOK(t, `
use_cb() {
  x := 5;
  vv := Vector[Vector[() -> int]]();
  vv.push([move || -> x + 1]);
  w := vv[0];
  y := w[0]();
}
main() {}
`)
}

// Returning the borrowed inner vector out of a native outer vector hands back an
// alias of storage the outer vector still frees at scope exit → UAF on escape.
func TestT1263ReturnNativeVecOfVecClosureReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
extract(Vector[Vector[() -> int]] vv) (() -> int)[] {
  w := vv[0];
  return w;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

// Re-storing the borrowed inner vector into a second, independently-owned vector is
// an escape of the alias: both would free the same env/vector → double free.
func TestT1263RestoreNativeVecOfVecClosureReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
restore(Vector[Vector[() -> int]] vv) {
  m2 := Map[int, (() -> int)[]]();
  w := vv[0];
  m2[1] = w;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// Moving the borrowed inner vector into a `~` (consuming) parameter escapes the
// alias — the callee would drop the vector/env the outer vector still owns.
func TestT1263MoveNativeVecOfVecClosureToConsumingParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
sink((() -> int)[] ~f) {}
use_it(Vector[Vector[() -> int]] vv) {
  w := vv[0];
  sink(move w);
}
main() {}
`)
	expectOwnerError(t, errs, "borrows the argument")
}

// --- Struct field vector-of-closures direct read ---

// Same-scope read-and-invoke is a valid borrow: the struct remains sole owner.
func TestT1263ReadAndInvokeStructFieldVecClosureAccepted(t *testing.T) {
	ownerOK(t, `
type H { (() -> int)[] fns; }
use_cb() {
  x := 5;
  h := H(fns: [move || -> x + 1]);
  f := h.fns;
  y := f[0]();
}
main() {}
`)
}

// Returning the borrowed field vector out of a `~`-consumed owner hands back an alias
// of storage the owner's drop frees → UAF on escape.
func TestT1263ReturnStructFieldVecClosureReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
type H { (() -> int)[] fns; }
extract(H ~h) (() -> int)[] {
  f := h.fns;
  return f;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

// Re-storing the borrowed field vector into an independently-owned vector → double free.
func TestT1263RestoreStructFieldVecClosureReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
type H { (() -> int)[] fns; }
restore(H h) {
  m2 := Map[int, (() -> int)[]]();
  f := h.fns;
  m2[1] = f;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// Moving the borrowed field vector into a `~` (consuming) parameter escapes the alias.
func TestT1263MoveStructFieldVecClosureToConsumingParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
type H { (() -> int)[] fns; }
sink((() -> int)[] ~f) {}
use_it(H h) {
  f := h.fns;
  sink(move f);
}
main() {}
`)
	expectOwnerError(t, errs, "borrows the argument")
}

// --- Regressions: non-closure value-copying containers stay OWNED (deep copy) ---

// A NON-closure inner vector read out of a native outer vector is deep-cloned to an
// OWNED copy — returning it is valid. The Deep type gate reaches `int` (no closure) →
// nil → the read stays owned. Guards against over-borrowing ordinary containers.
func TestT1263ReturnNativeVecOfIntVecReadAccepted(t *testing.T) {
	ownerOK(t, `
extract(Vector[int[]] vv) int[] {
  w := vv[0];
  return w;
}
main() {}
`)
}

// A NON-closure struct field vector is likewise deep-cloned to an OWNED copy —
// returning it out of a `~`-consumed owner is valid.
func TestT1263ReturnStructFieldIntVecReadAccepted(t *testing.T) {
	ownerOK(t, `
type IB { int[] xs; }
extract(IB ~h) int[] {
  f := h.xs;
  return f;
}
main() {}
`)
}

// A user-defined NON-aliasing `[]` returning a bare value-copying container of
// closures hands back a FRESH owned value (indexTargetIsAliasingContainer is false
// for a user Box target, so the IndexExpr arm falls through to owned). Returning it
// must be accepted — only native Vector/Map index reads alias storage the owner
// frees. Guards the fix against over-borrowing user-container index results.
func TestT1263ReturnUserNonAliasingIndexClosureVecAccepted(t *testing.T) {
	ownerOK(t, `
type Box {
  int n;
  [](int i) (() -> int)[] {
    return [move || -> 1];
  }
}
extract(Box b) (() -> int)[] {
  w := b[0];
  return w;
}
main() {}
`)
}
