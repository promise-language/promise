package codegen

import (
	"strings"
	"testing"
)

// T1706: Dropping a polymorphic value by its static type must dispatch through
// RTTI so subtype fields are reached — struct fields, Ref[T], Mutex[T].

func TestFieldDropPolymorphicUsesRtti(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			go_() int `+"`"+`abstract;
		}
		type Child is Base {
			string name;
			go_() int { return this.name.len; }
		}
		type Box {
			Base item;
		}
		main() {
			Box b = Box(item: Child(name: "x"));
		}
	`)
	boxDrop := extractDefine(ir, "Box.drop$synth")
	if boxDrop == "" {
		boxDrop = extractDefine(ir, "Box.drop")
	}
	if boxDrop == "" {
		t.Fatal("expected Box drop function in IR")
	}
	if !strings.Contains(boxDrop, "struct.drop.call") && !strings.Contains(boxDrop, "sfield.drop") {
		t.Errorf("Box drop should use RTTI dispatch for polymorphic field\nBox drop IR:\n%s", boxDrop)
	}
}

func TestArcInnerDropPolymorphicUsesRtti(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			go_() int `+"`"+`abstract;
		}
		type Child is Base {
			string name;
			go_() int { return this.name.len; }
		}
		main() {
			Ref[Base] r = Ref[Base](Child(name: "y"));
		}
	`)
	arcDrop := extractDefine(ir, "Ref[Base].drop")
	if arcDrop == "" {
		t.Fatal("expected Ref[Base].drop function in IR")
	}
	if !strings.Contains(arcDrop, "inner.rtti.drop") && !strings.Contains(arcDrop, "struct.drop.call") {
		t.Errorf("Ref[Base].drop should use RTTI dispatch for polymorphic element\nRef[Base].drop IR:\n%s", arcDrop)
	}
}

func TestMutexInnerDropPolymorphicUsesRtti(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			go_() int `+"`"+`abstract;
		}
		type Child is Base {
			string name;
			go_() int { return this.name.len; }
		}
		main() {
			Mutex[Base] m = Mutex[Base](Child(name: "z"));
		}
	`)
	mutexDrop := extractDefine(ir, "Mutex[Base].drop")
	if mutexDrop == "" {
		t.Fatal("expected Mutex[Base].drop function in IR")
	}
	if !strings.Contains(mutexDrop, "inner.rtti.drop") && !strings.Contains(mutexDrop, "struct.drop.call") {
		t.Errorf("Mutex[Base].drop should use RTTI dispatch for polymorphic element\nMutex[Base].drop IR:\n%s", mutexDrop)
	}
}

// T1706: Structural interface in Ref[T] must dispatch through RTTI in emitInnerDrop.
// Before the fix, emitInnerDrop had no RTTI branch — structural types fell through
// to the "heap user type without drop" case that just pal_free'd the instance.
func TestArcInnerDropStructuralUsesRtti(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural {
			show(this) string `+"`"+`abstract;
		}
		type Widget is Showable {
			string label;
			show(this) string { return this.label; }
		}
		main() {
			Ref[Showable] r = Ref[Showable](Widget(label: "w"));
		}
	`)
	arcDrop := extractDefine(ir, "Ref[Showable].drop")
	if arcDrop == "" {
		t.Fatal("expected Ref[Showable].drop function in IR")
	}
	if !strings.Contains(arcDrop, "inner.rtti.drop") && !strings.Contains(arcDrop, "__promise_structural_drop") {
		t.Errorf("Ref[Showable].drop should use RTTI dispatch for structural element\nRef[Showable].drop IR:\n%s", arcDrop)
	}
}

// T1706: Structural interface in Mutex[T] must dispatch through RTTI in emitInnerDrop.
func TestMutexInnerDropStructuralUsesRtti(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural {
			show(this) string `+"`"+`abstract;
		}
		type Widget is Showable {
			string label;
			show(this) string { return this.label; }
		}
		main() {
			Mutex[Showable] m = Mutex[Showable](Widget(label: "w"));
		}
	`)
	mutexDrop := extractDefine(ir, "Mutex[Showable].drop")
	if mutexDrop == "" {
		t.Fatal("expected Mutex[Showable].drop function in IR")
	}
	if !strings.Contains(mutexDrop, "inner.rtti.drop") && !strings.Contains(mutexDrop, "__promise_structural_drop") {
		t.Errorf("Mutex[Showable].drop should use RTTI dispatch for structural element\nMutex[Showable].drop IR:\n%s", mutexDrop)
	}
}

// T1706: Concrete (non-abstract) base with children in a struct field must use
// RTTI dispatch in the field drop — needsRttiDrop returns true because
// needsVtable is true (hasChildren). This is the vtable-only path (not structural).
func TestFieldDropConcreteBaseWithChildrenUsesRtti(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			speak(this) int { return 0; }
		}
		type Dog is Animal {
			string name;
			speak(this) int { return this.name.len; }
		}
		type Kennel {
			Animal pet;
		}
		main() {
			Kennel k = Kennel(pet: Dog(name: "rex"));
		}
	`)
	kennelDrop := extractDefine(ir, "Kennel.drop$synth")
	if kennelDrop == "" {
		kennelDrop = extractDefine(ir, "Kennel.drop")
	}
	if kennelDrop == "" {
		t.Fatal("expected Kennel drop function in IR")
	}
	if !strings.Contains(kennelDrop, "sfield.drop") {
		t.Errorf("Kennel drop should use RTTI dispatch (sfield.drop) for concrete base with children\nKennel drop IR:\n%s", kennelDrop)
	}
}

// T1706: Base type with no drop and no droppable fields, child adds all droppable
// fields. The "total leak" case — without RTTI dispatch the base's drop path is
// just pal_free, missing every field the child owns.
func TestFieldDropBaseNoDrop_ChildAddsFields_UsesRtti(t *testing.T) {
	ir := generateIR(t, `
		type Bare {
			act(this) int `+"`"+`abstract;
		}
		type Rich is Bare {
			string data;
			act(this) int { return this.data.len; }
		}
		type Holder {
			Bare item;
		}
		main() {
			Holder h = Holder(item: Rich(data: "abc"));
		}
	`)
	holderDrop := extractDefine(ir, "Holder.drop$synth")
	if holderDrop == "" {
		holderDrop = extractDefine(ir, "Holder.drop")
	}
	if holderDrop == "" {
		t.Fatal("expected Holder drop function in IR")
	}
	// Must use RTTI dispatch, not static drop lookup
	if !strings.Contains(holderDrop, "sfield.drop") {
		t.Errorf("Holder drop should use RTTI dispatch for abstract base field\nHolder drop IR:\n%s", holderDrop)
	}
}
