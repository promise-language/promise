package ownership

import "testing"

// T1262: the BARE-container analogue of T1230. Reading a value-copying container
// that IS a Vector/Map-of-closures out of a Map value (`b := m[0]!` on a
// `Map[int, (() -> int)[]]`) aliases the map's stored heap env — the Map's `[]`
// returns the element by value, and a closure element is not deep-cloned on read
// (the captured frame is opaque, and dupVector's element-clone would zero it →
// SEGV). closureAggregateBorrowSource must mark the read a BORROW (Deep type gate +
// aliasing-container restriction), not an owned fresh copy — otherwise the local
// frees the env the map's own drop also frees → double free / segfault. Mirrors the
// T1230 struct-field sibling exactly.

// Same-scope read-and-invoke is a valid borrow: the map remains sole owner and frees
// the vector/env once at scope exit. Must be accepted.
func TestT1262ReadAndInvokeBareVecClosureFromMapAccepted(t *testing.T) {
	ownerOK(t, `
use_cb() {
  x := 5;
  m := Map[int, (() -> int)[]]();
  m[0] = [move || -> x + 1];
  b := m[0]!;
  y := b[0]();
}
main() {}
`)
}

// Returning the bare vector-of-closures read out of a map hands back an alias of the
// map's storage, which the map still frees at scope exit → UAF on escape. Compile
// error, mirroring the T1230 struct sibling.
func TestT1262ReturnBareVecClosureFromMapReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
extract(Map[int, (() -> int)[]] m) (() -> int)[] {
  b := m[0]!;
  return b;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

// Re-storing the borrowed vector-of-closures into a second, independently-owned map
// is another escape of the alias: both maps would free the same env/vector at scope
// exit → double free. The Borrowed marking must reject the move.
func TestT1262RestoreBareVecClosureFromMapReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
restore(Map[int, (() -> int)[]] m) {
  m2 := Map[int, (() -> int)[]]();
  b := m[0]!;
  m2[1] = b;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// Moving the borrowed vector-of-closures into a `~` (consuming) parameter escapes the
// alias — the callee would drop the vector/env the map still owns. Must be rejected.
func TestT1262MoveBareVecClosureToConsumingParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
sink((() -> int)[] ~f) {}
use_it(Map[int, (() -> int)[]] m) {
  b := m[0]!;
  sink(move b);
}
main() {}
`)
	expectOwnerError(t, errs, "borrows the argument")
}

// A bare Map-of-closures value (`Map[int, map[int, () -> int]]`) behaves identically —
// the read is a borrow and same-scope read-and-invoke of the inner closure is valid.
func TestT1262ReadAndInvokeBareMapClosureFromMapAccepted(t *testing.T) {
	ownerOK(t, `
use_cb() {
  x := 9;
  m := Map[int, map[int, () -> int]]();
  m[0] = { 0: move || -> x };
  b := m[0]!;
  y := b[0]!();
}
main() {}
`)
}

// Regression: a NON-closure bare vector read out of a map is deep-cloned to an OWNED
// copy (not a borrow) — returning it is valid. The Deep type gate reaches `int` (no
// closure) → nil → the read stays owned. Guards against the fix over-borrowing
// ordinary value-copying containers, which must keep their owning drop.
func TestT1262ReturnBareIntVecFromMapReadAccepted(t *testing.T) {
	ownerOK(t, `
extract(Map[int, int[]] m) int[] {
  b := m[0]!;
  return b;
}
main() {}
`)
}
