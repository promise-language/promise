package sema

import "testing"

// T1550: a `structural type whose fields are all `value is a pure VALUE TYPE, not an
// interface view — it lowers to the flat value struct, never to a {vtable, instance}
// fat pointer. Two sema rules keep that shape closed, so `check` and `build` can never
// disagree about it:
//
//  1. It may not declare abstract methods (the rule lives in markValueType, added by
//     T1527 and asserted there for the nominal form). Without it, `check` accepted
//     `Shape s = Square(...)` — structural satisfaction into a value type — and codegen
//     panicked with "store operands are not compatible: src={ i8*, i8* };
//     dst=%promise_Shape_v*". With it, nothing can ever satisfy such a type structurally.
//  2. Consequently the only types assignable to it are its own value newtypes.
//
// These cases pin the `structural spelling specifically, plus the negatives that must
// NOT be swept up: an abstract-only structural interface implemented by a value type
// (T1284) and a plain concrete structural value type.

// The `structural spelling of the abstract-method rule — the exact shape whose
// structural satisfaction used to reach codegen and panic.
func TestT1550StructuralValueTypeAbstractMethodRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Shape `+"`structural"+` {
		  int side `+"`value"+`;
		  get area int `+"`abstract"+`;
		}
		main() {}
	`)
	expectError(t, errs, "value type Shape cannot have abstract methods")
}

// …and therefore an unrelated value type can never structurally satisfy it: the
// program that used to panic is now a clean, source-located diagnostic on both
// `check` and `build`.
func TestT1550StructuralValueTypeCannotBeSatisfiedStructurally(t *testing.T) {
	errs := checkErrs(t, `
		type Shape `+"`structural"+` {
		  int side `+"`value"+`;
		  get area int `+"`abstract"+`;
		}
		type Square {
		  int side `+"`value"+`;
		  get area int => this.side * this.side;
		}
		main() {
		  Shape s = Square(side: 3);
		  print_line(s.area.to_string());
		}
	`)
	expectError(t, errs, "value type Shape cannot have abstract methods")
}

// Negative — T1284's shape must keep working: an abstract-only structural interface
// (no fields, so NOT a value type) satisfied structurally by a value type.
func TestT1550AbstractOnlyStructuralInterfaceWithValueImplOK(t *testing.T) {
	errs := checkErrs(t, `
		type Tagged `+"`structural"+` {
		  get tag int `+"`abstract"+`;
		}
		type Seat {
		  int row `+"`value"+`;
		  get tag int => this.row * 2;
		}
		main() {
		  Tagged t = Seat(row: 2);
		  print_line(t.tag.to_string());
		}
	`)
	expectNoErrors(t, errs)
}

// Negative — a concrete `structural value type (all fields `value, no abstract
// methods) is a perfectly good value type, used directly and across every crossing.
func TestT1550ConcreteStructuralValueTypeOK(t *testing.T) {
	errs := checkErrs(t, `
		type Metric `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		}
		show(Metric m) { print_line(m.raw_doubled.to_string()); }
		make() Metric { return Metric(raw: 6); }
		main() {
		  Metric m = make();
		  show(m: m);
		  Metric? o = m;
		  Metric[] v = [m];
		  print_line(v[0].raw_doubled.to_string());
		}
	`)
	expectNoErrors(t, errs)
}

// The GENERIC spelling of the abstract-method rule. A generic type reaches the
// classification through a different entry than a plain one (its `value fields are
// TypeParams, so the copy check defers), and the rule has to hold there too — a
// generic `structural value type is monomorphized into flat value structs with its own
// method bodies, so an abstract method still has no vtable slot to resolve through.
func TestT1550GenericStructuralValueTypeAbstractMethodRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Slot[T] `+"`structural"+` {
		  T a `+"`value"+`;
		  get first T `+"`abstract"+`;
		}
		main() {}
	`)
	expectError(t, errs, "value type Slot cannot have abstract methods")
}

// Negative — the same generic type with every method implemented is a perfectly good
// generic value type, instantiated and used across crossings.
func TestT1550GenericStructuralValueTypeOK(t *testing.T) {
	errs := checkErrs(t, `
		type Slot[T] `+"`structural"+` {
		  T a `+"`value"+`;
		  T b `+"`value"+`;
		  get first T => this.a;
		  swapped(this) Slot[T] { return Slot[T](a: this.b, b: this.a); }
		}
		first_of(Slot[int] s) int { return s.first; }
		main() {
		  s := Slot[int](a: 3, b: 4);
		  print_line(first_of(s: s).to_string());
		  print_line(s.swapped().first.to_string());
		}
	`)
	expectNoErrors(t, errs)
}

// Negative — an unrelated value type assigned to a concrete structural value target
// is a plain type mismatch, not a view coercion.
func TestT1550UnrelatedValueTypeToStructuralValueTargetRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Metric `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		}
		type Other {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 3;
		}
		main() {
		  Metric m = Other(raw: 6);
		  print_line(m.raw_doubled.to_string());
		}
	`)
	expectError(t, errs, "cannot assign Other to variable of type Metric")
}

// The T1527 value-newtype rules are spelled for the NOMINAL form of a value parent.
// T1550's whole shape is `structural re-opening a decision that annotation should not
// touch, so each rule needs the `structural spelling pinned too — a rule that quietly
// stopped applying here is exactly how `check` and `build` drift apart again.

// A value child may add methods, not fields — the child shares the parent's struct
// verbatim, and `structural on the parent does not change that.
func TestT1550NewtypeOfStructuralValueParentCannotAddField(t *testing.T) {
	errs := checkErrs(t, `
		type Metric `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		}
		type Latency is Metric {
		  int extra `+"`value"+`;
		}
		main() {}
	`)
	expectError(t, errs, "type Latency inherits `value fields from Metric and cannot declare fields of its own")
}

// A generic CHILD of a `structural value parent is rejected: monomorphization would
// have to produce a distinct shared struct per instantiation.
func TestT1550GenericChildOfStructuralValueParentRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Metric `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		}
		type Latency[T] is Metric {}
		main() {}
	`)
	expectError(t, errs, "value-type inheritance is not supported for generic types: Latency has type parameters, so it cannot inherit from the value type Metric")
}

// …and so is a child of a generic `structural value parent INSTANTIATION. This is the
// one that could plausibly have slipped through: a generic `structural type reaches
// value classification on the mono path, where the parent is an Instance rather than
// the Named the check reads.
func TestT1550ChildOfGenericStructuralValueParentRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Slot[T] `+"`structural"+` {
		  T a `+"`value"+`;
		  get first T => this.a;
		}
		type IntSlot is Slot[int] {}
		main() {}
	`)
	expectError(t, errs, "value-type inheritance is not supported for generic types: the value parent Slot has type parameters")
}

// `is` between a `structural value parent and its newtype is rejected, not silently
// answered. A `structural target normally IS is-checkable (RTTI through the view), so
// the value-type rule has to win here — otherwise codegen would be asked to read a
// type id out of a flat struct that has no RTTI pointer at all.
func TestT1550IsCheckBetweenStructuralValueParentAndNewtypeRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Metric `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		}
		type Latency is Metric {
		  get tag int => 7;
		}
		main() {
		  Metric m = Latency(raw: 6);
		  if m is Latency { print_line("yes"); }
		}
	`)
	expectError(t, errs, "cannot use 'is' type check between value types Metric and Latency")
}

// …and the downcast likewise: there is nothing at runtime to check it against.
func TestT1550AsCastBetweenStructuralValueParentAndNewtypeRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Metric `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		}
		type Latency is Metric {
		  get tag int => 7;
		}
		main() {
		  Metric m = Latency(raw: 6);
		  l := m as Latency;
		  print_line(l.tag.to_string());
		}
	`)
	expectError(t, errs, "cannot use 'as' cast between value types Metric and Latency")
}

// Negative — a `structural value type nested as a `value field of another one is a
// perfectly good value type all the way down, and a newtype of the outer still works.
func TestT1550NestedStructuralValueTypesOK(t *testing.T) {
	errs := checkErrs(t, `
		type Metric `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		}
		type Latency is Metric {
		  get tag int => 7;
		}
		type Window `+"`structural"+` {
		  Metric lo `+"`value"+`;
		  Metric hi `+"`value"+`;
		  get span int => this.hi.raw - this.lo.raw;
		}
		type Bucket is Window {
		  get label int => 3;
		}
		span_of(Window w) int { return w.span; }
		main() {
		  b := Bucket(lo: Latency(raw: 2), hi: Latency(raw: 9));
		  Window w = b;
		  print_line(span_of(w: b).to_string());
		  print_line(w.lo.raw_doubled.to_string());
		}
	`)
	expectNoErrors(t, errs)
}

// --- T1550, second wave: the remaining rules that keep the shape closed ------------
// The value-type rules T1527 introduced all have a `structural spelling, and each one
// is what stops a shape reaching codegen that has no lowering. The cases above pin the
// abstract-method rule; these pin the rest, plus the positives on the other side of
// each boundary.

// The abstract-method rule applies to the NEWTYPE as well as to the parent. A value
// child is classified through its parent's `value fields rather than its own (it has
// none), so it reaches the rule by a different route — and if it slipped through, the
// child would be structurally satisfiable while its parent was not, which is exactly
// the {i8*, i8*}-into-a-flat-struct crossing again.
func TestT1550NewtypeOfStructuralValueParentAbstractMethodRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Metric `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		}
		type Latency is Metric {
		  get tag int `+"`abstract"+`;
		}
		main() {}
	`)
	expectError(t, errs, "value type Latency cannot have abstract methods")
}

// A `structural value type may not declare drop(). It is register-resident and Copy —
// every crossing above is a bit copy, so a drop would run once per copy. The rule has
// to see through the `structural annotation to reach this type at all.
func TestT1550StructuralValueTypeDropRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Metric `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		  drop(~this) {}
		}
		main() {}
	`)
	expectError(t, errs, "value type Metric cannot have a drop() method")
}

// A value newtype may ADD methods but not OVERRIDE inherited ones: it shares the
// parent's struct and there is no vtable to resolve an override through, so the call
// site would silently pick whichever of the two the static type named. `structural on
// the parent must not buy the child virtual dispatch it cannot have.
func TestT1550NewtypeOfStructuralValueParentCannotOverride(t *testing.T) {
	errs := checkErrs(t, `
		type Metric `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		}
		type Latency is Metric {
		  get raw_doubled int => this.raw * 3;
		}
		main() {}
	`)
	expectError(t, errs, "value type Latency cannot override 'raw_doubled' from Metric")
}

// A `structural type with a MIX of `value and plain fields is NOT a value type — one
// heap field is enough to make it an ordinary interface view. Inheriting its `value
// fields is therefore rejected rather than silently producing a child that shares a
// layout its parent does not have. This is the boundary the whole item turns on:
// "all fields `value" is the classifier, not the `structural annotation.
func TestT1550MixedFieldStructuralParentIsNotAValueType(t *testing.T) {
	errs := checkErrs(t, `
		type Mixed `+"`structural"+` {
		  int raw `+"`value"+`;
		  string label;
		  get raw_doubled int => this.raw * 2;
		}
		type Sub is Mixed {
		  get tag int => 7;
		}
		main() {}
	`)
	expectError(t, errs, "type Sub inherits `value fields from Mixed, which is not a valid value type")
}

// Negative — a genuine non-value `structural parent keeps every view capability the
// value-typed one loses: the child is assignable to it AND `is`-checkable against it,
// because the fat pointer carries RTTI. The mirror of
// TestT1550IsCheckBetweenStructuralValueParentAndNewtypeRejected; together they pin
// that the new rules fire on exactly the value-typed shape and no wider.
func TestT1550NonValueStructuralParentKeepsViewCapabilitiesOK(t *testing.T) {
	errs := checkErrs(t, `
		type Shape `+"`structural"+` {
		  int side;
		  get area int => this.side * this.side;
		}
		type Square is Shape {
		  get tag int => 7;
		}
		main() {
		  s := Square(side: 3);
		  Shape sh = s;
		  if sh is Square { print_line("yes"); }
		  print_line(sh.area.to_string());
		}
	`)
	expectNoErrors(t, errs)
}

// Negative — the two halves meeting on the accepting side: a value NEWTYPE of a
// `structural value parent still structurally satisfies a REAL abstract-only interface,
// both directly and after being upcast to its parent. The parent's concrete getter is
// what satisfies the interface, so the child inherits the satisfaction; rejecting this
// would be the over-correction that trades the panic for a false diagnostic.
func TestT1550NewtypeOfStructuralValueParentSatisfiesAbstractInterfaceOK(t *testing.T) {
	errs := checkErrs(t, `
		type Tagged `+"`structural"+` {
		  get tag int `+"`abstract"+`;
		}
		type Metric `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		  get tag int => this.raw;
		}
		type Latency is Metric {
		  get label int => 7;
		}
		main() {
		  Tagged t = Latency(raw: 6);
		  print_line(t.tag.to_string());
		  Metric m = Latency(raw: 6);
		  Tagged t2 = m;
		  print_line(t2.tag.to_string());
		  print_line(Latency(raw: 6).label.to_string());
		}
	`)
	expectNoErrors(t, errs)
}

// A `structural value type used as a generic CONSTRAINT bound. The bound is a concrete
// value type rather than an interface, so the constraint check must accept both the
// type itself and its value newtypes without ever asking for a view. Pins the sema half
// of the shape whose codegen half is TestT1550GenericConstrainedByStructuralValueType-
// MonomorphizesFlat.
func TestT1550StructuralValueTypeAsGenericConstraintOK(t *testing.T) {
	errs := checkErrs(t, `
		type Metric `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		}
		type Latency is Metric {
		  get tag int => 7;
		}
		doubled_of[T: Metric](T x) int { return x.raw_doubled; }
		main() {
		  print_line(doubled_of[Metric](x: Metric(raw: 5)).to_string());
		  print_line(doubled_of[Latency](x: Latency(raw: 6)).to_string());
		}
	`)
	expectNoErrors(t, errs)
}

// A free function taking the `structural value type as a parameter and returning a
// string, called with the type itself and with its newtype. Both crossings are plain
// copies of the shared struct — sema must not require (or reject) a view coercion at
// either. Pairs with TestT1550StringReturningFnWithStructuralValueParamTakesValueStruct
// in codegen, which pins that the parameter is lowered as the flat struct.
func TestT1550StringReturningFnWithStructuralValueParamOK(t *testing.T) {
	errs := checkErrs(t, `
		type Metric `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		}
		type Latency is Metric {
		  get tag int => 7;
		}
		describe(Metric m) string { return "m=" + m.raw_doubled.to_string(); }
		main() {
		  print_line(describe(m: Metric(raw: 6)));
		  print_line(describe(m: Latency(raw: 7)));
		}
	`)
	expectNoErrors(t, errs)
}

// A method on the `structural value type that returns a CLOSURE, inherited by its
// newtype and called through a parent-typed local. The receiver is a flat value struct
// on every one of those routes, and the closure's captured `value field is copied into
// the env rather than borrowed from a box.
func TestT1550ClosureReturningMethodOnStructuralValueTypeOK(t *testing.T) {
	errs := checkErrs(t, `
		type Meter `+"`structural"+` {
		  int raw `+"`value"+`;
		  get raw_doubled int => this.raw * 2;
		  adder(this) (int) -> int {
		    int base = this.raw;
		    return |int x| -> x + base;
		  }
		}
		type Ammeter is Meter {
		  get tag int => 4;
		}
		main() {
		  a := Ammeter(raw: 6);
		  f := a.adder();
		  print_line(f(4).to_string());
		  Meter up = a;
		  print_line(up.adder()(1).to_string());
		}
	`)
	expectNoErrors(t, errs)
}
