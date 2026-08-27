package sema

import (
	"strings"
	"testing"
)

// T1730: a pure value type may declare a `structural interface parent. The
// restriction it lifts was a layout rule — an interface declares no fields, so
// it cannot affect the value struct, and the `is` clause is a conformance claim
// the compiler checks rather than inherited state
// (docs/language-design.md §5.2). State-bearing parents are still rejected.

func TestT1730ValueTypeStructuralParentAccepted(t *testing.T) {
	checkOK(t, `
		type Dur is Format {
			int nanos `+vt+`;
			format!(Writer ~w) { w.write_string(this.nanos.to_string()); }
		}
		main() {
			Dur d = Dur(nanos: 5);
			Format f = d;
			Builder b = Builder(); f.format(b)?!;
		}
	`)
}

// The declaration is where the missing body must be caught: a value type never
// dispatches virtually, so an unimplemented requirement would leave a null slot
// in the view vtable that a box calls straight through.
func TestT1730ValueTypeStructuralParentMissingImplRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Tagged `+"`structural"+` { get tag int `+"`abstract"+`; }
		type Dur is Tagged { int nanos `+vt+`; }
		main() {}
	`)
	expectError(t, errs, "value type Dur does not implement 'tag' required by Tagged")
}

// A body with the wrong signature does not satisfy the requirement either —
// that one is override.go's diagnostic, unchanged by T1730.
func TestT1730ValueTypeStructuralParentSignatureMismatchRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Dur is Format {
			int nanos `+vt+`;
			format!(Writer ~w, int extra) { w.write_string(this.nanos.to_string()); }
		}
		main() {}
	`)
	expectError(t, errs, "cannot satisfy abstract method 'format' from Format")
}

// Only `structural interfaces are exempt: a non-structural parent is
// state-bearing (it has an instance struct), so the layout rule still applies.
func TestT1730ValueTypeNonStructuralParentStillRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Base { get tag int => 1; }
		type Dur is Base { int nanos `+vt+`; }
		main() {}
	`)
	expectError(t, errs, "value type Dur cannot inherit from Base")
}

func TestT1730ValueTypeAbstractParentStillRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Base `+"`abstract"+` { get tag int `+"`abstract"+`; }
		type Dur is Base { int nanos `+vt+`; get tag int => 1; }
		main() {}
	`)
	expectError(t, errs, "value type Dur cannot inherit from Base")
}

// A value newtype over a value parent may list a structural interface too: the
// interface contributes no value struct, so the one-state-parent limit holds.
// The newtype supplies the requirement itself — a method the value parent does
// not have, since redeclaring one it does have would be an illegal value-child
// override (T1527).
func TestT1730ValueNewtypeWithStructuralParentAccepted(t *testing.T) {
	info := checkOK(t, `
		type Tagged `+"`structural"+` { get tag int `+"`abstract"+`; }
		type Meters { f64 raw `+vt+`; }
		type Feet is Meters, Tagged { get tag int => 3; }
		main() { Feet f = Feet(raw: 1.0); Tagged v = f; print_line("{v.tag}"); }
	`)
	if n := namedOf(t, info, "Feet"); !n.IsValueType() {
		t.Errorf("Feet should still be classified a value type")
	}
}

// A requirement the newtype's value parent satisfies is accepted at the
// declaration, but the type is still reported abstract at instantiation — a
// pre-existing gap in Named.IsAbstract that is not specific to value types
// (an ordinary heap type with the same shape behaves identically). T1758.
func TestT1730ValueNewtypeStructuralParentSatisfiedByValueParent(t *testing.T) {
	info := checkOK(t, `
		type Tagged `+"`structural"+` { get tag int `+"`abstract"+`; }
		type Meters { f64 raw `+vt+`; get tag int => 7; }
		type Feet is Meters, Tagged {}
		main() {}
	`)
	if n := namedOf(t, info, "Feet"); !n.IsValueType() {
		t.Errorf("Feet should still be classified a value type")
	}
}

func TestT1730ValueTypeTwoStructuralParentsAccepted(t *testing.T) {
	checkOK(t, `
		type Dur is Format, Equal {
			int nanos `+vt+`;
			format!(Writer ~w) { w.write_string(this.nanos.to_string()); }
			== (Dur other) bool => this.nanos == other.nanos;
		}
		main() {
			Dur a = Dur(nanos: 1);
			assert(a == Dur(nanos: 1), "eq");
			assert(a != Dur(nanos: 2), "ne");
		}
	`)
}

func TestT1730GenericValueTypeStructuralParentAccepted(t *testing.T) {
	info := checkOK(t, `
		type Pt[T: Format] is Format {
			T x `+vt+`;
			format!(Writer ~w) { this.x.format(w)?^; }
		}
		main() {
			Pt[int] p = Pt[int](x: 3);
			Format f = p;
			Builder b = Builder(); f.format(b)?!;
		}
	`)
	if n := namedOf(t, info, "Pt"); !n.IsValueType() {
		t.Errorf("Pt should be classified a value type")
	}
}

// T1550 must still hold: a `structural *value* type is state-bearing, so it is
// not satisfied structurally and cannot declare `abstract methods.
func TestT1730StructuralValueTypeStillNotSatisfiedStructurally(t *testing.T) {
	errs := checkErrs(t, `
		type Length `+"`structural"+` { f64 raw `+vt+`; }
		type Other { f64 raw `+vt+`; }
		main() { Length l = Other(raw: 1.0); }
	`)
	expectError(t, errs, "cannot assign Other to variable of type Length")
}

func TestT1730StructuralValueTypeStillCannotDeclareAbstract(t *testing.T) {
	errs := checkErrs(t, `
		type Length `+"`structural"+` { f64 raw `+vt+`; get name string `+"`abstract"+`; }
		main() {}
	`)
	expectError(t, errs, "cannot have abstract methods")
}

// The rewritten diagnostic names the parent that actually broke the rule. With
// both an interface and a heap parent listed, the interface is not the mistake —
// pointing at it would send the reader to delete the conformance claim.
func TestT1730MixedParentsDiagnosticNamesStateBearingParent(t *testing.T) {
	errs := checkErrs(t, `
		type Base { get tag int => 1; }
		type Dur is Format, Base {
			int nanos `+vt+`;
			format!(Writer ~w) { w.write_string(this.nanos.to_string()); }
		}
		main() {}
	`)
	expectError(t, errs, "value type Dur cannot inherit from Base")
	for _, e := range errs {
		if strings.Contains(e.Error(), "cannot inherit from Format") {
			t.Errorf("diagnostic blamed the structural interface: %v", e)
		}
	}
}

// The motivating case from the task: `is Format` with no format! body. The
// requirement is a plain method, which selects LookupMethod inside
// Named.LookupAbstractImpl — the getter form is covered above.
func TestT1730ValueTypeStructuralParentMissingMethodRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Dur is Format { int nanos `+vt+`; }
		main() {}
	`)
	expectError(t, errs, "value type Dur does not implement 'format' required by Format")
}

// An abstract setter selects the third arm of Named.LookupAbstractImpl. A
// getter of the same name would not satisfy it, so the arms must not be
// collapsed.
func TestT1730ValueTypeStructuralParentMissingSetterRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Slot `+"`structural"+` { set knob(int v) `+"`abstract"+`; }
		type Dur is Slot { int nanos `+vt+`; get knob int => this.nanos; }
		main() {}
	`)
	expectError(t, errs, "value type Dur does not implement 'knob' required by Slot")
}

// The requirement may come from a grandparent interface: ParentAbstractMethods
// walks the whole chain, so the value type owes the transitively inherited body
// too, and the diagnostic names the interface that declared it (A), not the one
// the type listed (B).
func TestT1730TransitiveStructuralRequirementRejected(t *testing.T) {
	errs := checkErrs(t, `
		type A `+"`structural"+` { get tag int `+"`abstract"+`; }
		type B is A `+"`structural"+` { }
		type Dur is B { int nanos `+vt+`; }
		main() {}
	`)
	expectError(t, errs, "value type Dur does not implement 'tag' required by A")
}

func TestT1730TransitiveStructuralRequirementAccepted(t *testing.T) {
	info := checkOK(t, `
		type A `+"`structural"+` { get tag int `+"`abstract"+`; }
		type B is A `+"`structural"+` { }
		type Dur is B { int nanos `+vt+`; get tag int => 4; }
		main() { Dur d = Dur(nanos: 1); A a = d; print_line("{a.tag}"); }
	`)
	if n := namedOf(t, info, "Dur"); !n.IsValueType() {
		t.Errorf("Dur should still be classified a value type")
	}
}

// Fieldlessness is the test, not the `structural flag: a `structural type that
// declares an ordinary (non-`value) field is state-bearing, so the layout rule
// still rejects it. This is the case isStructuralInterfaceParent's AllFields
// check exists for.
func TestT1730StructuralParentWithFieldsStillRejected(t *testing.T) {
	errs := checkErrs(t, `
		type S `+"`structural"+` { string name; }
		type Dur is S { int nanos `+vt+`; }
		main() {}
	`)
	expectError(t, errs, "value type Dur cannot inherit from S")
}

// Declaring a structural parent does not buy the value type the right to
// declare `abstract methods of its own — there is still no vtable of its own to
// resolve them through.
func TestT1730ValueTypeWithStructuralParentStillCannotDeclareAbstract(t *testing.T) {
	errs := checkErrs(t, `
		type Dur is Format {
			int nanos `+vt+`;
			format!(Writer ~w) { w.write_string(this.nanos.to_string()); }
			get extra int `+"`abstract"+`;
		}
		main() {}
	`)
	expectError(t, errs, "value type Dur cannot have abstract methods: 'extra' has no implementation")
}

// The parent may be a generic interface instantiated at a concrete argument —
// still fieldless, still no contribution to the value struct.
func TestT1730GenericStructuralInterfaceParentAccepted(t *testing.T) {
	info := checkOK(t, `
		type Boxy[T] `+"`structural"+` { get item T `+"`abstract"+`; }
		type Cell is Boxy[int] { int raw `+vt+`; get item int => this.raw; }
		main() { Cell c = Cell(raw: 2); print_line("{c.item}"); }
	`)
	if n := namedOf(t, info, "Cell"); !n.IsValueType() {
		t.Errorf("Cell should still be classified a value type")
	}
}

// A `structural interface parent contributes no state, so the type it is
// declared on stays a pure value type: still `copy, still no drop().
func TestT1730StructuralParentKeepsValueTypeFlags(t *testing.T) {
	info := checkOK(t, `
		type Dur is Format {
			int nanos `+vt+`;
			format!(Writer ~w) { w.write_string(this.nanos.to_string()); }
		}
		main() {}
	`)
	n := namedOf(t, info, "Dur")
	if !n.IsValueType() {
		t.Fatalf("Dur should be classified a value type")
	}
	if !n.IsCopy() {
		t.Errorf("a pure value type is automatically `copy; `is Format` must not change that")
	}
}

// Filtering structural parents out of the override check must not exempt the
// value parent alongside them: a value child that redeclares a method its value
// parent already has is still an illegal override (T1527), even when a
// structural interface happens to require a method of that name. Dispatch is
// static, so the redeclaration would apply only where the child is the static
// type — the divergence T1527 exists to forbid.
func TestT1730ValueNewtypeStillCannotOverrideValueParentMethod(t *testing.T) {
	errs := checkErrs(t, `
		type Tagged `+"`structural"+` { get tag int `+"`abstract"+`; }
		type Base { int raw `+vt+`; get tag int => 1; }
		type Kid is Base, Tagged { get tag int => 2; }
		main() {}
	`)
	expectError(t, errs, "value type Kid cannot override 'tag' from Base")
}
