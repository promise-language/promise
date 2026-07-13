package ownership

import "testing"

// T1265: the store-native-push escape shape that the T1262/T1263 suites don't cover.
// A value-copying container of closures read as a BORROW (T1262 bare-container-from-Map
// read; T1263 struct-field read) can still escape by being auto-moved (implicit last
// use) into `Vector.push`'s by-value native consuming param. The ownership pass rejects
// index-assign / return / explicit-move-to-`~` but let the push slip through because the
// store-native surface used the narrower alias-safe predicate, which carves out
// Vector/Map/Set. Codegen's element auto-dup at the push site cannot dup a closure vector
// (dupVector zeroes the env → SEGV), so the borrow escaped and both owners freed the same
// env → double-free / SEGV. The fix re-includes closure aggregates via the Deep gate.

// Pushing a bare vector-of-closures borrowed out of a Map into a second, independently
// owned vector escapes the alias: both the map and the new vector free the same env at
// scope exit → double free. The auto-move (last use of `b`) must be rejected.
func TestT1265PushBareVecClosureFromMapReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
use_it(Map[int, (() -> int)[]] m) {
  b := m[0]!;
  vv := Vector[Vector[() -> int]]();
  vv.push(b);
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// The T1263 struct-field analogue: a vector-of-closures read directly off a struct field
// is a borrow; pushing it into an independently owned vector escapes the alias → double
// free. Must be rejected.
func TestT1265PushStructFieldVecClosureRejected(t *testing.T) {
	errs := ownerErrs(t, `
type H { (() -> int)[] fns; }
use_it(H h) {
  f := h.fns;
  vv := Vector[Vector[() -> int]]();
  vv.push(f);
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// Same-scope read-and-invoke (no push) is a valid borrow — the map remains sole owner and
// frees the env once at scope exit. Must be accepted (regression guard against over-
// rejection of the plain read).
func TestT1265ReadAndInvokeStillAccepted(t *testing.T) {
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

// Regression: pushing a FRESH owned vector-of-closures is fine — it is not borrowed, so
// the auto-move transfers sole ownership to the vector. Guards against the fix over-
// rejecting legitimate owned pushes.
func TestT1265OwnedClosureVecPushAccepted(t *testing.T) {
	ownerOK(t, `
use_it() {
  x := 7;
  vv := Vector[Vector[() -> int]]();
  vv.push([move || -> x]);
}
main() {}
`)
}

// Map-of-closures shape (T1262 sibling `Map[int, () -> int]`): a bare map-of-closures
// read out of an outer map is a borrow just like the vector shape. Pushing it into an
// independently-owned vector escapes the alias — both maps would free the same env at
// scope exit. The auto-move must be rejected. Exercises the Deep-gate branch through a
// Map (not Vector) top-level container.
func TestT1265PushBareMapClosureFromMapReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
use_it(Map[int, map[int, () -> int]] m) {
  b := m[0]!;
  vv := Vector[map[int, () -> int]]();
  vv.push(b);
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// Enum-with-closure-variant shape (T1259/T1264 family): an enum whose variant carries a
// vector-of-closures, read out of a map by value, is a borrow — the variant env aliases
// the map's storage. Pushing it into an independently-owned vector escapes the alias →
// double free. Must be rejected. Exercises the Deep gate recursing an enum variant field.
func TestT1265PushEnumClosureVariantFromMapReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
enum E { Wrap((() -> int)[]); Empty; }
use_it(Map[int, E] m) {
  b := m[0]!;
  vv := Vector[E]();
  vv.push(b);
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// Deep nesting (T1260 family): a struct field that is itself a struct holding a
// vector-of-closures. Reading the inner struct off the outer is a borrow; pushing it
// into an independently-owned vector escapes the alias. Guards that the Deep gate
// recurses through more than one struct field level.
func TestT1265PushNestedStructClosureRejected(t *testing.T) {
	errs := ownerErrs(t, `
type Inner { (() -> int)[] fns; }
type Outer { Inner inner; }
use_it(Outer o) {
  b := o.inner;
  vv := Vector[Inner]();
  vv.push(b);
}
main() {}
`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// Over-rejection guard for the Map shape: pushing a FRESH owned map-of-closures transfers
// sole ownership to the vector — not a borrow, so the auto-move is legitimate and must be
// accepted. Mirror of TestT1265OwnedClosureVecPushAccepted for the Map container.
func TestT1265OwnedClosureMapPushAccepted(t *testing.T) {
	ownerOK(t, `
use_it() {
  x := 3;
  vv := Vector[map[int, () -> int]]();
  vv.push({ 0: move || -> x });
}
main() {}
`)
}
