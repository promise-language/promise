package ownership

import "testing"

// T1247: reading a capturing closure out of a Map value aliases the map's stored
// heap env (a Map's user `[]` returns the element by value; closures aren't
// Cloneable so no env dup happens on read). The read must be treated as a BORROW,
// not an owned fresh closure — otherwise the local frees the env that the map's own
// drop also frees → double free / segfault. This mirrors the T1113 aliasing-container
// carve-out and the T1227 borrowed-closure escape rejections: same-scope
// read-and-invoke stays valid, but escaping the borrow as owned is rejected.

// Returning a closure read out of a map hands back an alias of the map's env, which
// the map still frees at scope exit → UAF on escape. Must be a compile error.
func TestT1247ReturnClosureFromMapReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
extract() () -> int {
  x := 5;
  map[string, () -> int] m = map[string, () -> int]();
  m["k"] = move || -> x + 1;
  g := m["k"]!;
  return g;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

// Same-scope read-and-invoke is a valid borrow: the map remains sole owner and
// frees the env once at scope exit. Must be accepted.
func TestT1247ReadAndInvokeClosureFromMapAccepted(t *testing.T) {
	ownerOK(t, `
use_cb() {
  x := 5;
  map[string, () -> int] m = map[string, () -> int]();
  m["k"] = move || -> x + 1;
  g := m["k"]!;
  y := g();
}
main() {}
`)
}

// Re-storing the borrowed map-read closure into a second, independently-owned
// aggregate is another escape of the alias: both maps would free the same env at
// scope exit → double free. The Borrowed marking must reject the move — same
// invariant as the return case, via a different escape path.
func TestT1247RestoreClosureFromMapReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
restore() {
  x := 5;
  map[string, () -> int] m = map[string, () -> int]();
  m["k"] = move || -> x + 1;
  g := m["k"]!;
  map[string, () -> int] m2 = map[string, () -> int]();
  m2["j"] = g;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// The map literal construction form of the read must borrow identically to the
// index-assign form: reading out and returning the closure is still an escape of
// the map's env alias and must be rejected.
func TestT1247ReturnClosureFromMapLiteralReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
extract() () -> int {
  x := 5;
  f := move || -> x + 1;
  map[string, () -> int] m = { "k": f };
  g := m["k"]!;
  return g;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}
