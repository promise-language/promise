package ownership

import "testing"

// T1301: Returning a droppable generic type-param field (`V?`/`V`) out of a
// droppable owner double-frees when the concrete V is a drop-bearing heap-user
// type. The generic method/getter body is ownership-checked once with V
// unbound, so checkFieldMoveOwnership's `types.ContainsTypeParam(fieldType)`
// early-return used to allow the move unconditionally — codegen then emits an
// accessor that aliases the owner's box (no dupHeapFieldForEscape path for
// heap-user types), and both the escape sink and the owner's synth drop free it.
//
// Fix: an instantiation-aware check (checkGenericFieldMove) consults every
// recorded concrete instantiation of the owner and applies the concrete
// field-move verdict to the substituted field type. A drop-bearing heap-user
// instantiation (Box[_Res]) is rejected; Copy (Box[int]) and structural-view
// (Slot[Sink]) instantiations stay allowed.

// --- Getter escape, droppable instantiation → rejected ---

func TestT1301_GenericGetterDroppableFieldRejected(t *testing.T) {
	errs := ownerErrs(t, `
type _Res { int id; drop(~this) {} }
type Box[V] { V? _v; get val V? { return this._v; } }
make() {
    Box[_Res] b = Box[_Res](_v: none);
}
`)
	expectOwnerError(t, errs, "cannot move field '_v' out of 'Box[_Res]'")
}

// A non-optional generic field getter is equally unsafe.
func TestT1301_GenericGetterNonOptionalDroppableFieldRejected(t *testing.T) {
	errs := ownerErrs(t, `
type _Res { int id; drop(~this) {} }
type Box[V] { V _v; get val V { return this._v; } }
make() {
    Box[_Res] b = Box[_Res](_v: _Res(id: 1));
}
`)
	expectOwnerError(t, errs, "cannot move field '_v' out of 'Box[_Res]'")
}

// --- Non-getter escape contexts, droppable instantiation → rejected ---

// Constructor-field-init escape: `Holder[V](item: this._v)` aliases the owner's
// field box into the new object.
func TestT1301_GenericCtorFieldInitDroppableRejected(t *testing.T) {
	errs := ownerErrs(t, `
type _Res { int id; drop(~this) {} }
type Holder[V] { V _item; }
type Box[V] { V _v; wrap(this) Holder[V] { return Holder[V](_item: this._v); } }
make() {
    Box[_Res] b = Box[_Res](_v: _Res(id: 1));
}
`)
	expectOwnerError(t, errs, "cannot move field '_v' out of 'Box[_Res]'")
}

// Consuming `~`/move parameter escape: passing `this._v` to a move parameter
// hands the callee an alias of the owner's field box.
func TestT1301_GenericMoveArgDroppableRejected(t *testing.T) {
	errs := ownerErrs(t, `
type _Res { int id; drop(~this) {} }
take[V](V move r) {}
type Box[V] { V _v; give(this) { take[V](this._v); } }
make() {
    Box[_Res] b = Box[_Res](_v: _Res(id: 1));
}
`)
	expectOwnerError(t, errs, "cannot move field '_v' out of 'Box[_Res]'")
}

// --- Negative: Copy instantiation stays allowed ---

// Box[int] has a Copy field — the read copies, no double-free. The new
// per-instantiation path must not over-reject it.
func TestT1301_GenericGetterCopyInstantiationOK(t *testing.T) {
	ownerOK(t, `
type Box[V] { V? _v; get val V? { return this._v; } }
make() {
    Box[int] b = Box[int](_v: none);
}
`)
}

// --- Negative: structural-view instantiation stays allowed (T1299 shape) ---

// Slot[Sink] holds a structural-interface view `{vtable, instance}` box that
// codegen auto-clones on field-escape (T1299). isDroppableType returns false for
// a structural view, so fieldMoveVerdict allows it — the new path must not
// regress this.
func TestT1301_GenericGetterStructuralInstantiationOK(t *testing.T) {
	ownerOK(t, `
type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
type Counter { int base `+"`"+`value; emit(this, int x) int { return this.base + x; } }
type Slot[V] { V? _v; get val V? { return this._v; } }
make() {
    Slot[Sink] s = Slot[Sink](_v: Counter(base: 5));
}
`)
}

// --- Negative: no droppable instantiation ⇒ no error ---

// When only Box[int] is instantiated, the generic getter is sound for every live
// instance, so nothing is rejected — the getter body must still compile even
// though its field type contains an unbound V.
func TestT1301_GenericGetterNoDroppableInstanceOK(t *testing.T) {
	ownerOK(t, `
type Box[V] { V? _v; get val V? { return this._v; } }
make() {
    Box[int] a = Box[int](_v: none);
    Box[bool] c = Box[bool](_v: none);
}
`)
}

// --- Mixed: both a Copy and a droppable instance ⇒ still rejected ---

// A program using both Box[int] and Box[_Res] must be rejected — the getter is
// unsound for the Box[_Res] instantiation even though Box[int] is fine.
func TestT1301_GenericGetterMixedInstancesRejected(t *testing.T) {
	errs := ownerErrs(t, `
type _Res { int id; drop(~this) {} }
type Box[V] { V? _v; get val V? { return this._v; } }
make() {
    Box[int] a = Box[int](_v: none);
    Box[_Res] b = Box[_Res](_v: none);
}
`)
	expectOwnerError(t, errs, "cannot move field '_v' out of 'Box[_Res]'")
}

// --- Coverage: unrelated generic instance in the loop (named != owner) ---

// A second, unrelated generic type is also instantiated. The per-instantiation
// loop must skip its instances (origin != owner) and still reach the offending
// Box[_Res] to reject it.
func TestT1301_GenericUnrelatedInstanceSkipped(t *testing.T) {
	errs := ownerErrs(t, `
type _Res { int id; drop(~this) {} }
type Other[W] { W _o; }
type Box[V] { V? _v; get val V? { return this._v; } }
make() {
    Other[int] o = Other[int](_o: 1);
    Box[_Res] b = Box[_Res](_v: none);
}
`)
	expectOwnerError(t, errs, "cannot move field '_v' out of 'Box[_Res]'")
}

// --- Coverage: generic fn taking Box[T] must not shadow the concrete reject ---

// A generic function referencing Box[T] in its signature does not push a
// concrete Box instance; the per-instantiation loop still finds the concrete
// Box[_Res] from make() and rejects it. Guards against a regression where the
// generic-context reference would suppress the concrete diagnostic.
func TestT1301_GenericUnresolvedInstanceSkipped(t *testing.T) {
	errs := ownerErrs(t, `
type _Res { int id; drop(~this) {} }
type Box[V] { V? _v; get val V? { return this._v; } }
wrap[T](Box[T] move b) {}
make() {
    Box[_Res] b = Box[_Res](_v: none);
}
`)
	expectOwnerError(t, errs, "cannot move field '_v' out of 'Box[_Res]'")
}

// --- Coverage: getter-call target carve-out (T0591) in the generic path ---

// Reading through a getter (`this.peek`) whose return type contains a TypeParam
// is a getter call returning an owned value, not a field move — the LookupGetter
// carve-out must let it pass. Box[int] keeps every instantiation sound.
func TestT1301_GenericGetterCallTargetOK(t *testing.T) {
	ownerOK(t, `
type Box[V] {
    V? _v;
    get peek V? { return this._v; }
    scan(this) { V? x = this.peek; }
}
make() {
    Box[int] b = Box[int](_v: none);
}
`)
}
