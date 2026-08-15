package sema

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/types"
)

// T1527: a fieldless child of a pure value type is itself a pure value type —
// a layout-preserving newtype that may add methods but not fields. Before the
// fix, sema accepted the declaration and codegen panicked on the inherited
// `value field.

const vt = "`value"

// namedOf looks up a user type by name in the checked file's scope.
func namedOf(t *testing.T, info *Info, name string) *types.Named {
	t.Helper()
	obj := info.Scopes[findFile(t, info)].Lookup(name)
	if obj == nil {
		t.Fatalf("type %s not found", name)
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		t.Fatalf("%s is not a type", name)
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		t.Fatalf("%s is not a named type", name)
	}
	return named
}

func expectValueType(t *testing.T, info *Info, name string) {
	t.Helper()
	named := namedOf(t, info, name)
	if !named.IsValueType() {
		t.Errorf("expected %s to be classified as a value type", name)
	}
	if !named.IsCopy() {
		t.Errorf("expected %s to be copy (value types auto-enable copy)", name)
	}
}

func TestT1527ValueNewtypeIsValueType(t *testing.T) {
	info := checkOK(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId is Hash128 {
			get tag int => 7;
		}
		main() { e := EntityId(value: 1u128); print_line(e.value.to_string()); }
	`)
	expectValueType(t, info, "Hash128")
	expectValueType(t, info, "EntityId")
}

// The tracker's original repro shape: the value parent carries `clone.
// validateCloneTypes runs after the classification pass, so a newtype must be
// classified by the time clone validation walks its inherited `value field.
func TestT1527ValueNewtypeOfCloneAnnotatedParent(t *testing.T) {
	info := checkOK(t, `
		type Hash128 `+"`clone"+` { u128 value `+vt+`; }
		type EntityId is Hash128 {}
		main() { e := EntityId(value: 1u128); print_line(e.value.to_string()); }
	`)
	expectValueType(t, info, "EntityId")
}

func TestT1527ValueNewtypeDeclaredBeforeParent(t *testing.T) {
	info := checkOK(t, `
		type EntityId is Hash128 {}
		type Hash128 { u128 value `+vt+`; }
		main() { e := EntityId(value: 1u128); print_line(e.value.to_string()); }
	`)
	expectValueType(t, info, "EntityId")
}

func TestT1527ValueNewtypeChain(t *testing.T) {
	info := checkOK(t, `
		type A { int x `+vt+`; }
		type B is A {}
		type C is B {}
		main() { c := C(x: 1); A a = c; print_line(a.x.to_string()); }
	`)
	expectValueType(t, info, "A")
	expectValueType(t, info, "B")
	expectValueType(t, info, "C")
}

// A value newtype is Copy, so using it after a copy is not a move.
func TestT1527ValueNewtypeIsCopyAtUseSites(t *testing.T) {
	checkOK(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId is Hash128 {}
		take(Hash128 h) u128 => h.value;
		main() {
			e := EntityId(value: 1u128);
			Hash128 h = e;
			print_line(take(e).to_string());
			print_line(e.value.to_string());
			print_line(h.value.to_string());
		}
	`)
}

// A value newtype used as a `value field of another type: its classification
// happens after Define, so the containing type's copy-field check must wait for
// it rather than reject the field.
func TestT1527ValueNewtypeAsValueField(t *testing.T) {
	info := checkOK(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId is Hash128 {}
		type Edge { EntityId src `+vt+`; EntityId dst `+vt+`; }
		main() {
			e := Edge(src: EntityId(value: 1u128), dst: EntityId(value: 2u128));
			print_line(e.dst.value.to_string());
		}
	`)
	expectValueType(t, info, "Edge")
}

// Classification is transitive: X's `value field is a newtype, and Y's is X.
// Neither is copy at the point it is defined, so both must wait for the
// post-Define pass — deferring only on the newtype itself rejected Y.
func TestT1527ValueNewtypeAsNestedValueField(t *testing.T) {
	info := checkOK(t, `
		type A { int n `+vt+`; }
		type B is A {}
		type X { B b `+vt+`; }
		type Y { X x `+vt+`; }
		main() { y := Y(x: X(b: B(n: 1))); print_line(y.x.b.n.to_string()); }
	`)
	expectValueType(t, info, "X")
	expectValueType(t, info, "Y")
}

// A genuinely non-copy `value field is still rejected — the deferral must not
// swallow the diagnostic, only postpone it to the reporting pass.
func TestT1527NonCopyValueFieldStillRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Heap { int n; }
		type Bad { Heap h `+vt+`; }
		main() {}
	`)
	expectError(t, errs, "value field Bad.h must be a copy type")
}

func TestT1527ValueNewtypeCannotAddInstanceField(t *testing.T) {
	errs := checkErrs(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId is Hash128 { int tag; }
		main() {}
	`)
	expectError(t, errs, "type EntityId inherits `value fields from Hash128 and cannot declare fields of its own")
}

func TestT1527ValueNewtypeCannotAddValueField(t *testing.T) {
	errs := checkErrs(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId is Hash128 { int tag `+vt+`; }
		main() {}
	`)
	expectError(t, errs, "type EntityId inherits `value fields from Hash128 and cannot declare fields of its own")
}

// A type whose own fields are all `value but whose parent is an ordinary heap
// type keeps the pre-existing diagnostic (see TestValueTypeNoInheritance).
func TestT1527ValueFieldsWithHeapParent(t *testing.T) {
	errs := checkErrs(t, `
		type Base { int id; }
		type Child is Base { int x `+vt+`; }
		main() {}
	`)
	expectError(t, errs, "value type Child cannot have parent types")
}

func TestT1527ValueNewtypeMultipleParents(t *testing.T) {
	errs := checkErrs(t, `
		type Hash128 { u128 value `+vt+`; }
		type Marker {}
		type EntityId is Hash128, Marker {}
		main() {}
	`)
	expectError(t, errs, "type EntityId cannot inherit from value type Hash128 together with other parent types")
}

// Listing a structural interface alongside the value parent is the same
// multiple-parent case: the newtype shares exactly one struct. Satisfying a
// structural interface does not need the explicit `is` — see
// TestT1527ValueTypeIsStructuralStillAllowed and the runtime counterpart.
func TestT1527ValueNewtypeWithStructuralParentRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Tagged `+"`structural"+` { get tag int `+"`abstract"+`; }
		type Hash128 { u128 value `+vt+`; get tag int => 7; }
		type EntityId is Hash128, Tagged {}
		main() {}
	`)
	expectError(t, errs, "type EntityId cannot inherit from value type Hash128 together with other parent types")
}

func TestT1527ValueNewtypeGenericChildRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId[T] is Hash128 {}
		main() {}
	`)
	expectError(t, errs, "value-type inheritance is not supported for generic types: EntityId has type parameters, so it cannot inherit from the value type Hash128")
}

func TestT1527ValueNewtypeGenericParentRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Box[T] { T v `+vt+`; }
		type IdBox is Box[int] {}
		main() {}
	`)
	expectError(t, errs, "value-type inheritance is not supported for generic types: the value parent Box has type parameters")
}

// A value newtype may add methods, but not a drop() — the value-type rules keep
// applying to the late-classified type.
func TestT1527ValueNewtypeCannotHaveDrop(t *testing.T) {
	errs := checkErrs(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId is Hash128 { drop(~this) {} }
		main() {}
	`)
	expectError(t, errs, "value type EntityId cannot have a drop() method")
}

// The failable-new() rule moved into markValueType so it also covers types
// classified after Define; it must still fire for a plain value type.
func TestT1527ValueTypeFailableNewStillRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Pt { int x `+vt+`; new!(int v) { this.x = v; } }
		main() {}
	`)
	expectError(t, errs, "value type Pt cannot have a failable new() method")
}

// A value type dispatches statically, so an abstract method on one could never
// be resolved — codegen panicked with "undeclared getter" when a newtype tried
// to implement it. Rejected at the declaration instead.
func TestT1527ValueTypeCannotHaveAbstractMethod(t *testing.T) {
	errs := checkErrs(t, `
		type Shape { int side `+vt+`; get area int `+"`abstract"+`; }
		type Square is Shape { get area int => this.side * this.side; }
		main() {}
	`)
	expectError(t, errs, "value type Shape cannot have abstract methods: 'area' has no implementation")
}

// A value newtype redeclaring a parent method with the SAME signature is not an
// override — dispatch is static, so the parent's body would still run through a
// parent-typed variable. Rejected rather than silently diverging.
func TestT1527ValueNewtypeCannotOverride(t *testing.T) {
	errs := checkErrs(t, `
		type Shape { int side `+vt+`; get area int => this.side; }
		type Square is Shape { get area int => this.side * this.side; }
		main() {}
	`)
	expectError(t, errs, "value type Square cannot override 'area' from Shape")
}

// Overloading is not overriding: a child method whose signature differs resolves
// statically to exactly the declaration the argument types select, so it stays
// legal — this is how a newtype gets its own `==(Self)` to satisfy Equal.
func TestT1527ValueNewtypeOverloadStillAllowed(t *testing.T) {
	checkOK(t, `
		type Hash128 { u128 value `+vt+`; == (Hash128 other) bool => this.value == other.value; }
		type EntityId is Hash128 { == (EntityId other) bool => this.value == other.value; }
		main() {
			a := EntityId(value: 1u128);
			b := EntityId(value: 1u128);
			if a == b { print_line("eq"); }
		}
	`)
}

func TestT1527IsCheckBetweenValueTypesRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId is Hash128 {}
		main() {
			e := EntityId(value: 1u128);
			Hash128 h = e;
			if h is EntityId { print_line("yes"); }
		}
	`)
	expectError(t, errs, "cannot use 'is' type check between value types Hash128 and EntityId")
}

func TestT1527CastBetweenValueTypesRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId is Hash128 {}
		main() {
			h := Hash128(value: 1u128);
			x := h as EntityId;
		}
	`)
	expectError(t, errs, "cannot use 'as' cast between value types Hash128 and EntityId")
}

// A match type-pattern arm is the same RTTI dispatch as `is` in different
// syntax, so it must be rejected the same way — otherwise the arm silently
// never matches.
func TestT1527MatchTypePatternBetweenValueTypesRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Base { int n `+vt+`; }
		type Kid is Base {}
		main() {
			Base b = Kid(n: 1);
			match b {
				Kid k => print_line("kid"),
				_ => print_line("other"),
			}
		}
	`)
	expectError(t, errs, "cannot use 'match' type pattern between value types Base and Kid")
}

// The self-check forms answer correctly from the static type and stay legal —
// including the destructuring pattern, which is how value-type fields are bound.
func TestT1527SelfIsCheckStillAllowed(t *testing.T) {
	checkOK(t, `
		type Vec2 { f64 x `+vt+`; f64 y `+vt+`; }
		check(Vec2 v) bool => v is Vec2;
		sum(Vec2 v) f64 {
			if v is Vec2(a, b) { return a + b; }
			return 0.0;
		}
		main() { v := Vec2(x: 1.0, y: 2.0); check(v); sum(v); }
	`)
}

// A value type tested against a structural interface it satisfies is not a
// value-to-value check and must keep working (T1284).
func TestT1527ValueTypeIsStructuralStillAllowed(t *testing.T) {
	errs := checkErrs(t, `
		type Shape `+"`structural"+` { get area f64; }
		type Square { f64 side `+vt+`; get area f64 => this.side * this.side; }
		main() {
			Shape s = Square(side: 2.0);
			if s is Square { print_line("square"); }
		}
	`)
	expectNoErrorContaining(t, errs, "no runtime type identity")
}

// The destructuring form of `is` goes through a different pattern branch than
// the bare type name, so it needs its own rejection — otherwise the arm binds
// fields from a check that can never be answered at runtime.
func TestT1527DestructureIsBetweenValueTypesRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId is Hash128 {}
		main() {
			e := EntityId(value: 1u128);
			Hash128 h = e;
			if h is EntityId(v) { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "cannot use 'is' type check between value types Hash128 and EntityId")
}

// `as!` short-circuits to the target type without an optional, so the check has
// to sit before the Force branch to catch it.
func TestT1527ForceCastBetweenValueTypesRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId is Hash128 {}
		main() {
			h := Hash128(value: 1u128);
			x := h as! EntityId;
		}
	`)
	expectError(t, errs, "cannot use 'as' cast between value types Hash128 and EntityId")
}

// The upcast direction is rejected too: `as` on user types yields an optional
// from a runtime test, and there is no runtime identity to test either way. The
// widening that is actually wanted is a plain assignment, which stays a no-op.
func TestT1527UpcastAsBetweenValueTypesRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId is Hash128 {}
		main() {
			e := EntityId(value: 1u128);
			x := e as Hash128;
		}
	`)
	expectError(t, errs, "cannot use 'as' cast between value types EntityId and Hash128")
}

// A scalar cast on a value type's field is not a value-to-value check — the
// rejection must not swallow ordinary numeric conversions.
func TestT1527ScalarCastOnValueNewtypeStillAllowed(t *testing.T) {
	checkOK(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId is Hash128 {}
		main() {
			e := EntityId(value: 1u128);
			n := e.value as int;
			print_line(n.to_string());
		}
	`)
}

// The override rule covers plain methods, not just getters: static dispatch
// means the parent's body runs wherever the parent is the static type.
func TestT1527ValueNewtypeCannotOverrideMethod(t *testing.T) {
	errs := checkErrs(t, `
		type Base { int n `+vt+`; scaled(this, int k) int => this.n * k; }
		type Kid is Base { scaled(this, int k) int => this.n * k * 2; }
		main() {}
	`)
	expectError(t, errs, "value type Kid cannot override 'scaled' from Base")
}

// …and setters, which resolve through their own lookup namespace: a setter and
// a getter of the same name are distinct declarations.
func TestT1527ValueNewtypeCannotOverrideSetter(t *testing.T) {
	errs := checkErrs(t, `
		type Base { int n `+vt+`; set val(int v) { this.n = v; } }
		type Kid is Base { set val(int v) { this.n = v * 2; } }
		main() {}
	`)
	expectError(t, errs, "value type Kid cannot override 'val' from Base")
}

// A child getter that shadows a parent SETTER of the same name is not an
// override — different namespaces, so the lookup must be namespace-aware rather
// than name-only.
func TestT1527ValueNewtypeGetterOverSetterNameAllowed(t *testing.T) {
	info := checkOK(t, `
		type Base { int n `+vt+`; set val(int v) { this.n = v; } }
		type Kid is Base { get val int => this.n; }
		main() { k := Kid(n: 3); print_line(k.val.to_string()); }
	`)
	expectValueType(t, info, "Kid")
}

// new() is per-type, so a value newtype may declare its own constructor even
// with the parent's exact signature — the override rule exempts it.
func TestT1527ValueNewtypeOwnNewAllowed(t *testing.T) {
	info := checkOK(t, `
		type Hash128 { u128 value `+vt+`; new(~this, u128 v) { this.value = v; } }
		type EntityId is Hash128 { new(~this, u128 v) { this.value = v; } }
		main() { e := EntityId(1u128); print_line(e.value.to_string()); }
	`)
	expectValueType(t, info, "EntityId")
}

// A failable new() on a LATE-classified value type is the case the rule moved
// out of validateNewMethod for: at Define time EntityId is not yet known to be
// a value type, so the check has to run again once it is.
func TestT1527ValueNewtypeFailableNewRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Hash128 { u128 value `+vt+`; }
		type EntityId is Hash128 { new!(~this, u128 v) { this.value = v; } }
		main() {}
	`)
	expectError(t, errs, "value type EntityId cannot have a failable new() method")
}

// An abstract method on a value NEWTYPE (classified after Define) is rejected
// by the same rule that covers the parent — markValueType runs for both.
func TestT1527ValueNewtypeCannotHaveAbstractMethod(t *testing.T) {
	errs := checkErrs(t, `
		type Base { int n `+vt+`; }
		type Kid is Base { get extra int `+"`abstract"+`; }
		main() {}
	`)
	expectError(t, errs, "value type Kid cannot have abstract methods: 'extra' has no implementation")
}

// The hybrid own-fields case (T0994) is diagnosed once, at the offending field
// — the T1527 stray-field backstop must not pile a second error on top of it.
func TestT1527HybridOwnFieldsReportedOnce(t *testing.T) {
	errs := checkErrs(t, `
		type Mixed { int a `+vt+`; int b; }
		main() {}
	`)
	expectError(t, errs, "type Mixed mixes `value` and instance fields")
	if got := countErrorsContaining(errs, "mixes `value` and instance fields"); got != 1 {
		t.Errorf("expected exactly 1 hybrid diagnostic, got %d: %v", got, errs)
	}
}

// A heap child of a heap parent is deferred too (it has parents), and must come
// back out of the classification pass unmarked and undiagnosed.
func TestT1527HeapInheritanceUnaffected(t *testing.T) {
	info := checkOK(t, `
		type Base { int id; }
		type Kid is Base { int extra; }
		main() { k := Kid(id: 1, extra: 2); print_line(k.id.to_string()); }
	`)
	if namedOf(t, info, "Kid").IsValueType() {
		t.Errorf("heap child must not be classified as a value type")
	}
	if namedOf(t, info, "Base").IsValueType() {
		t.Errorf("heap parent must not be classified as a value type")
	}
}

// A `structural parent whose fields are all `value is itself a pure value type,
// so a fieldless child of it is an ordinary newtype — being structural is not a
// reason for the classification pass to skip it. (Upcasting to such a parent is
// separately broken in codegen — T1550 — but the classification here is right.)
func TestT1527ValueNewtypeOfStructuralValueParent(t *testing.T) {
	info := checkOK(t, `
		type Metric `+"`structural"+` { int raw `+vt+`; get raw_doubled int => this.raw * 2; }
		type Latency is Metric { get tag int => 7; }
		main() { l := Latency(raw: 6); print_line(l.raw_doubled.to_string()); }
	`)
	expectValueType(t, info, "Metric")
	expectValueType(t, info, "Latency")
}

// The own-field rejection reaches through a chain: the offending type's `value
// fields arrive via two intervening newtypes, and the diagnostic names the
// immediate parent it inherits them through.
func TestT1527ValueNewtypeChainCannotAddField(t *testing.T) {
	errs := checkErrs(t, `
		type V { int n `+vt+`; }
		type W is V {}
		type Z is W {}
		type Q is Z { int extra; }
		main() {}
	`)
	expectError(t, errs, "type Q inherits `value fields from Z and cannot declare fields of its own")
}

// Warnings share the error slice with real errors, so the stray-value-field
// backstop consults hasErrors rather than len(c.errors) — a warning must not
// short-circuit it. Valid code carrying one must still come out clean.
func TestT1527WarningDoesNotDisarmStrayValueFieldSweep(t *testing.T) {
	// The warning has to come from a DECLARATION-level rule: the sweep runs right
	// after Define, so a use-site warning (deprecation) is not recorded yet.
	info, errs := checkSource(t, `
		type Redundant `+"`copy `clone"+` { int q; }
		type V { int n `+vt+`; }
		type W is V {}
		main() {
			r := Redundant(q: 1);
			w := W(n: 2);
			print_line(r.q.to_string());
			print_line(w.n.to_string());
		}
	`)
	expectError(t, errs, "warning: `clone is redundant on `copy type Redundant")
	expectNoErrorContaining(t, errs, "mixes `value` and instance fields")
	for _, e := range errs {
		if !strings.Contains(e.Error(), "warning:") {
			t.Errorf("expected warnings only, got %v", errs)
			break
		}
	}
	expectValueType(t, info, "W")
}

// `is` on an OPTIONAL value newtype is the self-check form once the optional is
// unwrapped, so the value-identity rejection must see through the Optional
// rather than treating `W?` and `W` as two different value types.
func TestT1527OptionalValueNewtypeSelfIsAllowed(t *testing.T) {
	checkOK(t, `
		type V { int n `+vt+`; }
		type W is V {}
		main() {
			W? w = W(n: 1);
			if w is W { print_line("w"); }
			V? v = V(n: 2);
			if v is V { print_line("v"); }
		}
	`)
}

// An `is` whose target carries type ARGUMENTS resolves through a different
// pattern branch than a bare type name — the generic instance has to be unwrapped
// to its origin before the value-to-value rejection can see it.
func TestT1527IsAgainstGenericValueInstanceRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Box[T] { T v `+vt+`; }
		type Pt { int x `+vt+`; }
		main() {
			p := Pt(x: 1);
			if p is Box[int] { print_line("box"); }
		}
	`)
	expectError(t, errs, "cannot use 'is' type check between value types Pt and Box")
}

// …and the same target against a HEAP subject stays legal: the generic branch
// must reject only when both sides are value types, like the bare-name branch.
func TestT1527IsAgainstGenericValueInstanceFromHeapSubjectAllowed(t *testing.T) {
	errs := checkErrs(t, `
		type Box[T] { T v `+vt+`; }
		type Heap { int id; }
		main() {
			h := Heap(id: 1);
			if h is Box[int] { print_line("box"); }
		}
	`)
	expectNoErrorContaining(t, errs, "no runtime type identity")
}

// The rejection is keyed on BOTH sides being value types: a value type tested
// against an ordinary heap type is a different (pre-existing) situation and must
// not pick up the value-identity diagnostic.
func TestT1527ValueTypeIsHeapTypeNotRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Heap { int id; }
		type V { int n `+vt+`; }
		check(V v) bool => v is Heap;
		main() { check(V(n: 1)); }
	`)
	expectNoErrorContaining(t, errs, "no runtime type identity")
}

// countErrorsContaining reports how many diagnostics mention substr.
func countErrorsContaining(errs []error, substr string) int {
	n := 0
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			n++
		}
	}
	return n
}
