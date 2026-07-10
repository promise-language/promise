package ownership

import "testing"

// T1248: the tuple-destructure analogue of T1247. A tuple LITERAL whose element
// reads a capturing closure out of an aliasing aggregate (`(cb, n) := (m["k"]!, 7)`)
// copies the stored fat pointer {fn, env} by value — the destructured local aliases
// the aggregate's heap env (a Map's user `[]` returns the element by value; closures
// aren't Cloneable so no env dup happens on read). The destructured local must be
// marked Borrowed, not Owned — otherwise the local frees the env that the map's own
// drop also frees → double free / segfault, and escapes of the alias would not be
// rejected. Same-scope read-and-invoke stays valid; escaping the borrow as owned is
// rejected, mirroring the T1247 sibling.

// Returning a closure destructured out of a map value hands back an alias of the
// map's env, which the map still frees at scope exit → UAF on escape. Compile error.
func TestT1248ReturnTupleDestructuredClosureFromMapReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
extract() () -> int {
  x := 5;
  map[string, () -> int] m = map[string, () -> int]();
  m["k"] = move || -> x + 1;
  (cb, n) := (m["k"]!, 7);
  return cb;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

// Same-scope read-and-invoke of the tuple-destructured borrow is valid: the map
// remains sole owner and frees the env once at scope exit. Must be accepted.
func TestT1248ReadAndInvokeTupleDestructuredClosureFromMapAccepted(t *testing.T) {
	ownerOK(t, `
use_cb() {
  x := 5;
  map[string, () -> int] m = map[string, () -> int]();
  m["k"] = move || -> x + 1;
  (cb, n) := (m["k"]!, 7);
  y := cb();
  z := n;
}
main() {}
`)
}

// The vector-element read form must borrow identically: destructuring a capturing
// closure read out of a vector into a tuple and returning it escapes the alias.
func TestT1248ReturnTupleDestructuredClosureFromVectorReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
extract() () -> int {
  x := 5;
  v := [move || -> x + 1];
  (cb, n) := (v[0], 7);
  return cb;
}
main() {}
`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

// Symmetric with the map accepted case: same-scope read-and-invoke of a closure
// destructured out of a vector element remains valid — the vector stays sole owner
// of the env and frees it once at scope exit. Must be accepted.
func TestT1248ReadAndInvokeTupleDestructuredClosureFromVectorReadAccepted(t *testing.T) {
	ownerOK(t, `
use_cb() {
  x := 5;
  v := [move || -> x + 1];
  (cb, n) := (v[0], 7);
  y := cb();
  z := n;
}
main() {}
`)
}

// Discarding the aliasing closure element with `_` while binding the other element
// must not register a borrow or an owning free for the discarded slot (the map stays
// sole owner). The named non-closure element binds Owned normally. Exercises the
// `name == "_"` continue branch in the tuple-literal destructure ownership case.
func TestT1248UnderscoreDiscardsAliasingClosureElementAccepted(t *testing.T) {
	ownerOK(t, `
use_n() {
  x := 5;
  map[string, () -> int] m = map[string, () -> int]();
  m["k"] = move || -> x + 1;
  (_, n) := (m["k"]!, 7);
  z := n;
}
main() {}
`)
}

// A tuple whose element is a FRESH OWNED closure (a getter-built closure, not an
// aliasing container read) must NOT be marked Borrowed — the destructured local owns
// its env. Returning that owned closure is valid (no borrow escape). Mirrors the
// codegen owned-getter control; guards against the fix over-borrowing owned elements.
func TestT1248ReturnTupleDestructuredOwnedGetterClosureAccepted(t *testing.T) {
	ownerOK(t, `
type Factory {
  int base;
  get build() -> int {
    s := "fac" + "tory";
    return move || -> s.len;
  }
}
extract() () -> int {
  fac := Factory(base: 0);
  (cb, n) := (fac.build, 7);
  return cb;
}
main() {}
`)
}
