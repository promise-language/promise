package ownership

import "testing"

// T1266: third structural sibling of T1262/T1263. A fixed-size array whose elements
// are value-copying containers of closures aliases the array's owned storage on an
// element read (`w := arr[0]`) through genArrayIndex. closureAggregateBorrowSource
// must mark it a BORROW so escapes are rejected; same-scope read-and-invoke stays
// valid. Mirrors T1263 exactly (indexTargetIsAliasingContainer now admits Array).

// Same-scope read-and-invoke is a valid borrow: the array remains sole owner and
// frees the inner vector/envs once at scope exit. Must be accepted.
func TestT1266ReadAndInvokeArrayOfVecClosureAccepted(t *testing.T) {
	ownerOK(t, `
use_cb() {
  x := 5;
  (() -> int)[][1] arr = [[move || -> x + 1]];
  w := arr[0];
  y := w[0]();
}
main() {}
`)
}

// Returning the borrowed inner vector out of a fixed array hands back an alias of
// storage the array still frees at scope exit → UAF on escape.
func TestT1266ReturnArrayOfVecClosureReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
extract((() -> int)[][1] arr) (() -> int)[] {
  w := arr[0];
  return w;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

// Re-storing the borrowed inner vector into a second, independently-owned container
// is an escape of the alias: both would free the same env/vector → double free.
func TestT1266RestoreArrayOfVecClosureReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
restore((() -> int)[][1] arr) {
  m2 := Map[int, (() -> int)[]]();
  w := arr[0];
  m2[1] = w;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// Moving the borrowed inner vector into a `~` (consuming) parameter escapes the
// alias — the callee would drop the vector/env the array still owns.
func TestT1266MoveArrayOfVecClosureToConsumingParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
sink((() -> int)[] ~f) {}
use_it((() -> int)[][1] arr) {
  w := arr[0];
  sink(move w);
}
main() {}
`)
	expectOwnerError(t, errs, "borrows the argument")
}

// Regression: a NON-closure inner vector read out of a fixed array is deep-cloned to
// an OWNED copy — returning it is valid. The Deep type gate reaches `int` (no closure)
// → nil → the read stays owned. Guards against over-borrowing ordinary containers.
func TestT1266ReturnArrayOfIntVecReadAccepted(t *testing.T) {
	ownerOK(t, `
extract(int[][1] arr) int[] {
  w := arr[0];
  return w;
}
main() {}
`)
}
