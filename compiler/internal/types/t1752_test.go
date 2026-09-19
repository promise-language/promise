package types

import "testing"

// T1752 — ValidateChain is what codegen walks to emit a type's `_validate!`
// links. Its two properties are not observable from a passing program (a
// mis-ordered or duplicated chain still *runs* the invariants), so they are
// pinned here: ROOT FIRST, and each declaring type exactly once.

// addValidate gives n a well-formed `_validate!`: shared `this`, no params, no
// result, failable — the shape validateValidateMethod enforces in sema.
func addValidate(n *Named) {
	recv := NewParam("this", n, RefNone)
	n.AddMethod(NewMethod(Pos{}, ValidateMethodName,
		NewSignature(recv, nil, nil, true), PlaceInstance, false, false))
}

func chainNames(n *Named) []string {
	var out []string
	for _, l := range n.ValidateChain() {
		out = append(out, l.Obj().Name())
	}
	return out
}

func assertChain(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain = %v, want %v", got, want)
		}
	}
}

func TestT1752ValidateChainEmptyWhenNothingDeclaresOne(t *testing.T) {
	n := makeNamed("Plain")
	if got := n.ValidateChain(); len(got) != 0 {
		t.Errorf("expected an empty chain, got %v", chainNames(n))
	}
	if n.IsValidated() {
		t.Error("a type with no _validate! must not report IsValidated")
	}
	if n.OwnValidateMethod() != nil {
		t.Error("OwnValidateMethod must be nil when none is declared")
	}
}

func TestT1752ValidateChainOwnOnly(t *testing.T) {
	n := makeNamed("Port")
	addValidate(n)
	assertChain(t, chainNames(n), "Port")
	if !n.IsValidated() {
		t.Error("a type declaring _validate! must report IsValidated")
	}
}

// A child INHERITS the obligation without declaring its own.
func TestT1752ValidateChainInheritedWithoutOwn(t *testing.T) {
	base := makeNamed("Base")
	addValidate(base)
	child := makeNamed("Child")
	child.AddParent(base)

	assertChain(t, chainNames(child), "Base")
	if !child.IsValidated() {
		t.Error("a child inheriting _validate! must report IsValidated")
	}
	if child.OwnValidateMethod() != nil {
		t.Error("OwnValidateMethod must see only the type's OWN declaration")
	}
}

// §5.7: the parent's runs first, and a child's _validate! ADDS to it rather
// than overriding — so both appear, root first.
func TestT1752ValidateChainIsRootFirst(t *testing.T) {
	base := makeNamed("Base")
	addValidate(base)
	mid := makeNamed("Mid")
	mid.AddParent(base)
	addValidate(mid)
	leaf := makeNamed("Leaf")
	leaf.AddParent(mid)
	addValidate(leaf)

	assertChain(t, chainNames(leaf), "Base", "Mid", "Leaf")
}

// An ancestor reachable by two paths is emitted ONCE. Emitting it twice would
// run the same invariant twice per construction — "runs exactly once" is the
// guarantee §5.7 makes.
func TestT1752ValidateChainDeduplicatesADiamond(t *testing.T) {
	top := makeNamed("Top")
	addValidate(top)
	left := makeNamed("Left")
	left.AddParent(top)
	addValidate(left)
	right := makeNamed("Right")
	right.AddParent(top)
	bottom := makeNamed("Bottom")
	bottom.AddParent(left)
	bottom.AddParent(right)
	addValidate(bottom)

	assertChain(t, chainNames(bottom), "Top", "Left", "Bottom")
}

// A parent that declares none is skipped, but its own ancestors are not.
func TestT1752ValidateChainSkipsUndeclaringAncestors(t *testing.T) {
	top := makeNamed("Top")
	addValidate(top)
	mid := makeNamed("Mid") // declares none
	mid.AddParent(top)
	leaf := makeNamed("Leaf")
	leaf.AddParent(mid)
	addValidate(leaf)

	assertChain(t, chainNames(leaf), "Top", "Leaf")
}

// _validate! is excluded from vtable slot assignment: a child's ADDS to its
// parent's rather than overriding it, so the two must never share a slot.
// Nothing dispatches it virtually — the chain is emitted statically at the
// construction expression, where the concrete type is exactly known.
func TestT1752ValidateIsNotAVirtualMethod(t *testing.T) {
	base := makeNamed("Base")
	addValidate(base)
	base.AddMethod(NewMethod(Pos{}, "speak",
		NewSignature(NewParam("this", base, RefNone), nil, TypInt, false),
		PlaceInstance, false, false))

	for _, m := range base.AllVirtualMethods() {
		if m.Name() == ValidateMethodName {
			t.Fatal("_validate must not occupy a vtable slot")
		}
	}
	if base.VirtualMethodIndex(ValidateMethodName, false) != -1 {
		t.Error("_validate must have no vtable slot index")
	}
	// An ordinary method alongside it still gets one.
	if base.VirtualMethodIndex("speak", false) < 0 {
		t.Error("an ordinary method must still get a vtable slot")
	}
}

// Enums do not inherit, so their chain is always their own single link.
func TestT1752EnumValidateIsItsOwn(t *testing.T) {
	tn := NewTypeName(Pos{}, "Temp", nil)
	e := NewEnum(tn, nil)
	if e.IsValidated() {
		t.Error("an enum with no _validate! must not report IsValidated")
	}
	e.AddMethod(NewMethod(Pos{}, ValidateMethodName,
		NewSignature(NewParam("this", e, RefNone), nil, nil, true),
		PlaceInstance, false, false))
	if !e.IsValidated() {
		t.Error("an enum declaring _validate! must report IsValidated")
	}
	if e.OwnValidateMethod() == nil {
		t.Error("OwnValidateMethod must find the enum's own declaration")
	}
}
