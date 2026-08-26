package types

import "testing"

// AllVirtualMethods decides vtable *shape*, and every slot lookup
// (VirtualMethodIndex, VirtualUnaryMethodIndex, VirtualSlotIndexForMethod) is
// derived from it — so the exclusion list is what keeps a call site's slot index
// pointing at the method it named. It had no direct coverage in this package;
// codegen exercised it only through emitted IR.
//
// T1749 added the receiver-less exclusion: a `factory / `global / `mono member is
// a static call on the type name, so nothing can dispatch through a slot it
// holds — the slot's only effect was to shift every later method's index.

// instanceMethod builds a method with a receiver, i.e. a virtually dispatchable
// one. The receiver's type is irrelevant to slot assignment, only its presence.
func instanceMethod(owner *Named, name string) *Method {
	recv := NewParam("this", owner, RefNone)
	return NewMethod(Pos{}, name, NewSignature(recv, nil, TypString, false), PlaceInstance, false, false)
}

// receiverlessMethod builds a `factory / `global / `mono member: same shape, no
// receiver.
func receiverlessMethod(name string) *Method {
	return NewMethod(Pos{}, name, NewSignature(nil, nil, TypString, false), PlaceInstance, false, false)
}

func virtualNames(n *Named) []string {
	var names []string
	for _, m := range n.AllVirtualMethods() {
		names = append(names, methodSlotKey(m))
	}
	return names
}

func assertSlotKeys(t *testing.T, n *Named, want ...string) {
	t.Helper()
	got := virtualNames(n)
	if len(got) != len(want) {
		t.Fatalf("%s: got slots %v, want %v", n.Obj().Name(), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: got slots %v, want %v", n.Obj().Name(), got, want)
		}
	}
}

func TestT1749_AllVirtualMethodsExcludesReceiverless(t *testing.T) {
	n := makeNamed("Base")
	n.AddMethod(receiverlessMethod("make"))  // `factory
	n.AddMethod(receiverlessMethod("named")) // `global
	n.AddMethod(instanceMethod(n, "greet"))

	assertSlotKeys(t, n, "greet")
	assertEqual(t, n.VirtualMethodIndex("greet", false), 0)
	// A receiver-less member is not addressable by slot at all.
	assertEqual(t, n.VirtualMethodIndex("make", false), -1)
	assertEqual(t, n.VirtualMethodIndex("named", false), -1)
}

func TestT1749_ReceiverlessExclusionIsIndependentOfTheOthers(t *testing.T) {
	// The three exclusions are separate `continue`s over the same loop, so each
	// must drop its own method and leave the rest of the ordering intact.
	n := makeNamed("Mixed")

	native := instanceMethod(n, "nativeOp")
	n.AddMethod(NewMethod(Pos{}, "nativeOp", native.Sig(), PlaceInstance, false, true))

	generic := instanceMethod(n, "genericOp")
	tn := NewTypeName(Pos{}, "T", nil)
	generic.Sig().SetTypeParams([]*TypeParam{NewTypeParam(tn, nil, 0)})
	n.AddMethod(generic)

	n.AddMethod(receiverlessMethod("staticOp"))
	n.AddMethod(instanceMethod(n, "realOp"))

	assertSlotKeys(t, n, "realOp")
	assertEqual(t, n.VirtualMethodIndex("realOp", false), 0)
	assertEqual(t, n.VirtualMethodIndex("nativeOp", false), -1)
	assertEqual(t, n.VirtualMethodIndex("genericOp", false), -1)
	assertEqual(t, n.VirtualMethodIndex("staticOp", false), -1)
}

func TestT1749_ReceiverlessGetterAndSetterHoldNoSlots(t *testing.T) {
	// A getter and a setter of the same name are DISTINCT slot keys, so a
	// `global accessor pair used to consume two slots ahead of the real ones.
	n := makeNamed("Base")

	globalGet := receiverlessMethod("gate")
	globalGet.SetGetter(true)
	n.AddMethod(globalGet)

	globalSet := receiverlessMethod("gate")
	globalSet.SetSetter(true)
	n.AddMethod(globalSet)

	get := instanceMethod(n, "width")
	get.SetGetter(true)
	n.AddMethod(get)

	set := instanceMethod(n, "width")
	set.SetSetter(true)
	n.AddMethod(set)

	assertSlotKeys(t, n, "width", "width$set")
	assertEqual(t, n.VirtualMethodIndex("width", false), 0)
	assertEqual(t, n.VirtualMethodIndex("width", true), 1)
	assertEqual(t, n.VirtualMethodIndex("gate", false), -1)
	assertEqual(t, n.VirtualMethodIndex("gate", true), -1)
}

func TestT1749_ParentReceiverlessMembersDoNotShiftChildSlots(t *testing.T) {
	// Slot indices must agree between a parent and its child, since a call
	// through the parent's static type indexes the child's vtable. A parent's
	// receiver-less member is filtered at the parent, so it can never leak into
	// the child's inherited prefix.
	parent := makeNamed("Base")
	parent.AddMethod(receiverlessMethod("make"))
	parent.AddMethod(instanceMethod(parent, "greet"))

	child := makeNamed("Kid")
	child.AddParent(parent)
	child.AddMethod(instanceMethod(child, "greet")) // override — same slot
	child.AddMethod(receiverlessMethod("build"))
	child.AddMethod(instanceMethod(child, "extra"))

	assertSlotKeys(t, parent, "greet")
	assertSlotKeys(t, child, "greet", "extra")
	assertEqual(t, parent.VirtualMethodIndex("greet", false), child.VirtualMethodIndex("greet", false))

	// The slot is introduced at the parent's position and the child's override
	// reuses it rather than appending — which is exactly why a parent's
	// receiver-less member must not be able to claim one.
	assertEqual(t, child.AllVirtualMethods()[0], parent.LookupMethod("greet"))
}

func TestT1749_ReceiverlessOperatorHoldsNoSlot(t *testing.T) {
	// Sema now rejects this shape outright (validateOperatorPlacement), so this
	// pins the layer below: even if one were constructed, it holds no slot — the
	// second half of why the rejection is the right call rather than a silent
	// mis-dispatch. Unary and binary variants are distinct slot keys, so both
	// arms of methodSlotKey are exercised.
	n := makeNamed("S")

	binary := receiverlessMethod("+")
	n.AddMethod(binary)

	unary := receiverlessMethod("-")
	n.AddMethod(unary)

	n.AddMethod(instanceMethod(n, "greet"))

	assertSlotKeys(t, n, "greet")
	assertEqual(t, n.VirtualMethodIndex("+", false), -1)
	assertEqual(t, n.VirtualUnaryMethodIndex("-"), -1)
	assertEqual(t, n.VirtualSlotIndexForMethod(binary), -1)
	assertEqual(t, n.VirtualSlotIndexForMethod(unary), -1)
}
