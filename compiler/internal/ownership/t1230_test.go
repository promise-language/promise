package ownership

import "testing"

// T1230: the struct-wrapped analogue of T1247. Reading a heap struct that HOLDS a
// capturing closure field out of a Map value (`fn := m[0]!` on a `Fn { () -> int f; }`
// value) aliases the map's stored heap env — the Map's `[]` returns the element by
// value, and a closure-nesting aggregate is not deep-cloned on read (the captured
// frame is opaque). The read must be treated as a BORROW, not an owned fresh copy —
// otherwise the local frees the env/instance that the map's own drop also frees →
// double free / segfault. Same-scope read-and-invoke stays valid; escaping the borrow
// as owned is rejected, mirroring the T1247 direct-closure sibling.

// Same-scope read-and-invoke is a valid borrow: the map remains sole owner and frees
// the instance/env once at scope exit. Must be accepted.
func TestT1230ReadAndInvokeStructClosureFromMapAccepted(t *testing.T) {
	ownerOK(t, `
type Fn { () -> int f; }
use_cb() {
  x := 5;
  m := Map[int, Fn]();
  m[0] = Fn(f: move || -> x + 1);
  fn := m[0]!;
  y := fn.f();
}
main() {}
`)
}

// Returning a struct-wrapped closure read out of a map hands back an alias of the
// map's env, which the map still frees at scope exit → UAF on escape. Compile error.
func TestT1230ReturnStructClosureFromMapReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
type Fn { () -> int f; }
extract(Map[int, Fn] m) Fn {
  fn := m[0]!;
  return fn;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

// Re-storing the borrowed struct-wrapped closure into a second, independently-owned
// map is another escape of the alias: both maps would free the same env at scope exit
// → double free. The Borrowed marking must reject the move.
func TestT1230RestoreStructClosureFromMapReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
type Fn { () -> int f; }
restore(Map[int, Fn] m) {
  m2 := Map[int, Fn]();
  fn := m[0]!;
  m2[1] = fn;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// Moving the borrowed struct-wrapped closure into a `~` (consuming) parameter escapes
// the alias — the callee would drop the env the map still owns. Must be rejected.
func TestT1230MoveStructClosureToConsumingParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
type Fn { () -> int f; }
sink(Fn ~f) {}
use_it(Map[int, Fn] m) {
  fn := m[0]!;
  sink(move fn);
}
main() {}
`)
	expectOwnerError(t, errs, "borrows the argument")
}

// A struct nesting the closure two levels deep behaves identically — the read is a
// borrow and same-scope read-and-invoke of the inner closure is valid.
func TestT1230ReadAndInvokeNestedStructClosureFromMapAccepted(t *testing.T) {
	ownerOK(t, `
type Fn { () -> int f; }
type Outer { Fn inner; }
use_cb() {
  x := 7;
  m := Map[int, Outer]();
  m[0] = Outer(inner: Fn(f: move || -> x));
  o := m[0]!;
  y := o.inner.f();
}
main() {}
`)
}

// A plain (non-closure) heap struct read out of a map is deep-cloned to an OWNED copy
// (not a borrow) — returning it is valid. Guards against the fix over-borrowing
// non-closure aggregates, which must keep their owning drop.
func TestT1230ReturnPlainStructFromMapReadAccepted(t *testing.T) {
	ownerOK(t, `
type Boxed { int[] xs; }
extract(Map[int, Boxed] m) Boxed {
  b := m[0]!;
  return b;
}
main() {}
`)
}
