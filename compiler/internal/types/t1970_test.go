package types

import "testing"

// T1970 — AllFieldTypes resolves an INHERITED field's declared type. AllFields
// prepends a parent's fields unsubstituted, so an inherited field is typed in the
// PARENT's *TypeParam — a different object from the child's, which a substitution
// built from the child's params alone leaves untouched. Every type-directed field
// walk in sema read inherited fields that way and missed whatever the type
// argument had put in them; `Vector[DerV[Mutex[int]]]` was accepted and
// segfaulted. These tests pin the primitive that closes the family.

// newNamedT is a Named with the given type params, named for the test.
func newNamedT(name string, tps []*TypeParam) *Named {
	return NewNamed(NewTypeName(Pos{}, name, nil), tps)
}

func addFieldT(n *Named, name string, typ Type) {
	n.AddField(NewField(Pos{}, name, typ, PlaceInstance, false, false))
}

// The headline shape: `BaseV[T] { T[] items; }`, `DerV[T] is BaseV[T] { int n; }`.
// The inherited buffer must resolve to the child's ARGUMENT, not stay expressed in
// the parent's parameter.
func TestT1970_AllFieldTypesResolvesInheritedField(t *testing.T) {
	baseT := makeTP("T", 0)
	base := newNamedT("BaseV", []*TypeParam{baseT})
	addFieldT(base, "items", NewVector(baseT))

	derT := makeTP("T", 0)
	der := newNamedT("DerV", []*TypeParam{derT})
	der.AddParent(base, []Type{derT})
	addFieldT(der, "n", TypInt)

	// AllFields alone reports the parent's field unsubstituted — the trap.
	fields := der.AllFields()
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(fields))
	}
	if elem, ok := AsVector(fields[0].Type()); !ok || elem != Type(baseT) {
		t.Fatalf("AllFields should report the parent's declared type Vector[BaseV.T], got %s", fields[0].Type())
	}

	got := AllFieldTypes(der, []Type{TypString})
	if len(got) != 2 {
		t.Fatalf("expected 2 field types, got %d", len(got))
	}
	elem, ok := AsVector(got[0])
	if !ok {
		t.Fatalf("expected the inherited field to stay a Vector, got %s", got[0])
	}
	if elem != TypString {
		t.Errorf("inherited field: expected Vector[string], got Vector[%s]", elem)
	}
	if got[1] != TypInt {
		t.Errorf("own field: expected int, got %s", got[1])
	}
}

// The child's parameter need not be spelled like the parent's — the binding comes
// from the ParentRef's type arguments. A name-matching fix would pass the test
// above and fail this one.
func TestT1970_AllFieldTypesResolvesRenamedParam(t *testing.T) {
	baseT := makeTP("T", 0)
	base := newNamedT("BaseV", []*TypeParam{baseT})
	addFieldT(base, "items", NewVector(baseT))

	derU := makeTP("U", 0)
	der := newNamedT("DerU", []*TypeParam{derU})
	der.AddParent(base, []Type{derU})

	got := AllFieldTypes(der, []Type{TypBool})
	if len(got) != 1 {
		t.Fatalf("expected 1 field type, got %d", len(got))
	}
	if elem, ok := AsVector(got[0]); !ok || elem != TypBool {
		t.Errorf("expected Vector[bool], got %s", got[0])
	}
}

// Two levels: the buffer is the grandparent's field, so the bindings must compose
// up the chain rather than one hop.
func TestT1970_AllFieldTypesResolvesTransitiveChain(t *testing.T) {
	baseT := makeTP("T", 0)
	base := newNamedT("BaseV", []*TypeParam{baseT})
	addFieldT(base, "items", NewVector(baseT))

	midT := makeTP("T", 0)
	mid := newNamedT("MidV", []*TypeParam{midT})
	mid.AddParent(base, []Type{midT})

	leafT := makeTP("T", 0)
	leaf := newNamedT("LeafV", []*TypeParam{leafT})
	leaf.AddParent(mid, []Type{leafT})

	got := AllFieldTypes(leaf, []Type{TypInt})
	if len(got) != 1 {
		t.Fatalf("expected 1 field type, got %d", len(got))
	}
	if elem, ok := AsVector(got[0]); !ok || elem != TypInt {
		t.Errorf("expected Vector[int], got %s", got[0])
	}
}

// A NON-generic child of a generic parent: there are no child params at all, so
// the whole substitution comes from the ParentRef. This is the shape that escaped
// every walk's bare-*Named branch, which applied no substitution whatsoever.
func TestT1970_AllFieldTypesResolvesNonGenericChild(t *testing.T) {
	baseT := makeTP("T", 0)
	base := newNamedT("BaseV", []*TypeParam{baseT})
	addFieldT(base, "items", NewVector(baseT))

	der := newNamedT("DerNG", nil)
	der.AddParent(base, []Type{TypString})
	addFieldT(der, "n", TypInt)

	got := AllFieldTypes(der, nil)
	if len(got) != 2 {
		t.Fatalf("expected 2 field types, got %d", len(got))
	}
	if elem, ok := AsVector(got[0]); !ok || elem != TypString {
		t.Errorf("expected Vector[string], got %s", got[0])
	}
}

// A non-generic parent must not break the walk, and its own parents are still
// followed — `Der is Mid is Base[T]` where Mid takes no arguments.
func TestT1970_MergeParentSubstSkipsNonGenericParent(t *testing.T) {
	baseT := makeTP("T", 0)
	base := newNamedT("BaseV", []*TypeParam{baseT})
	addFieldT(base, "items", NewVector(baseT))

	mid := newNamedT("MidNG", nil)
	mid.AddParent(base, []Type{TypInt})

	der := newNamedT("DerNG2", nil)
	der.AddParent(mid)

	subst := FieldSubstMap(der, nil)
	if got, ok := subst[baseT]; !ok || got != TypInt {
		t.Errorf("expected BaseV.T → int through the non-generic parent, got %v", subst[baseT])
	}
}

// FieldSubstMap must never return nil: BuildSubstMap returns nil on an arity
// mismatch (a generic type walked with unbound params passes nil typeArgs), and
// MergeParentSubst writes into the map.
func TestT1970_FieldSubstMapNeverNil(t *testing.T) {
	tp := makeTP("T", 0)
	n := newNamedT("Solo", []*TypeParam{tp})
	subst := FieldSubstMap(n, nil)
	if subst == nil {
		t.Fatal("FieldSubstMap returned a nil map for mismatched arity")
	}
	subst[tp] = TypInt // must not panic
}

// A type with no fields yields no field types — AllFieldTypes must not allocate a
// substitution for nothing, and must not return a one-element slice of nil.
func TestT1970_AllFieldTypesEmpty(t *testing.T) {
	n := newNamedT("Empty", nil)
	if got := AllFieldTypes(n, nil); len(got) != 0 {
		t.Errorf("expected no field types, got %v", got)
	}
}

// ContainsFailableTask shares the AllFields walk, so it shared the blind spot: an
// inherited `failable_task` read as still-generic let the goroutine's error be
// discarded, which language-design.md#failable-goroutines forbids — the error must reach
// exactly one receiver. These are the direct predicate tests; the end-to-end
// must-use diagnostic is TestT1970_InheritedFailableTaskMustBeReceived in
// ownership.

// Generic child, handle supplied as the child's type argument: reached through the
// Instance-origin *Named branch under the composed substitution.
func TestT1970_ContainsFailableTaskThroughInheritedGenericField(t *testing.T) {
	ft := NewInstance(TypFailableTask, []Type{TypInt})

	// type FtBase[T] { T t; }  /  type FtDer[T] is FtBase[T] { int n; }
	baseT := makeTP("T", 0)
	base := newNamedT("FtBase", []*TypeParam{baseT})
	addFieldT(base, "t", baseT)

	derT := makeTP("T", 0)
	der := newNamedT("FtDer", []*TypeParam{derT})
	der.AddParent(base, []Type{derT})
	addFieldT(der, "n", TypInt)

	assertTrue(t, ContainsFailableTask(NewInstance(der, []Type{ft})),
		"FtDer[failable_task[int]] through the inherited field")
	assertFalse(t, ContainsFailableTask(NewInstance(der, []Type{TypInt})),
		"FtDer[int] owns no failable task")
}

// Non-generic child, handle bound at the parent reference: reached through the bare
// *Named branch, which applied no substitution at all before this item. Neither the
// child's type arguments (it has none) nor the declared field type (an unbound
// parameter) reveals the handle — only the ParentRef does.
func TestT1970_ContainsFailableTaskThroughInheritedConcreteField(t *testing.T) {
	ft := NewInstance(TypFailableTask, []Type{TypInt})

	// type FtBase2[T] { T t; }  /  type FtDerNG is FtBase2[failable_task[int]] { int n; }
	baseT := makeTP("T", 0)
	base := newNamedT("FtBase2", []*TypeParam{baseT})
	addFieldT(base, "t", baseT)

	der := newNamedT("FtDerNG", nil)
	der.AddParent(base, []Type{ft})
	addFieldT(der, "n", TypInt)

	assertTrue(t, ContainsFailableTask(der),
		"FtDerNG is FtBase2[failable_task[int]] through the inherited field")

	// The same shape over a plain argument owns no task.
	plainDer := newNamedT("FtDerNGPlain", nil)
	plainDer.AddParent(base, []Type{TypInt})
	assertFalse(t, ContainsFailableTask(plainDer), "FtDerNGPlain is FtBase2[int]")
}

// The cycle guard in the Instance-origin *Named branch. AllFieldTypes allocates a
// substitution map and a field-type slice per call, so an unguarded cycle here is
// an infinite loop, not merely slow: `Rec[T] { Rec[T]? next; }` at Rec[int] must
// terminate and report no failable task.
func TestT1970_ContainsFailableTaskGenericNamedCycleTerminates(t *testing.T) {
	tp := makeTP("T", 0)
	rec := newNamedT("RecG", []*TypeParam{tp})
	addFieldT(rec, "next", NewOptional(NewInstance(rec, []Type{tp})))

	assertFalse(t, ContainsFailableTask(NewInstance(rec, []Type{TypInt})),
		"recursive generic RecG[int] terminates and owns no failable task")

	// The guard must not swallow a handle that IS present alongside the cycle.
	ft := NewInstance(TypFailableTask, []Type{TypInt})
	rec2 := newNamedT("RecG2", []*TypeParam{tp})
	addFieldT(rec2, "next", NewOptional(NewInstance(rec2, []Type{tp})))
	addFieldT(rec2, "t", ft)
	assertTrue(t, ContainsFailableTask(NewInstance(rec2, []Type{TypInt})),
		"recursive generic RecG2[int] still reports its failable task")
}

// The same cycle guard in the Instance-origin *Enum branch — a recursive generic
// cons list.
func TestT1970_ContainsFailableTaskGenericEnumCycleTerminates(t *testing.T) {
	tp := makeTP("T", 0)
	list := NewEnum(NewTypeName(Pos{}, "ListG", nil), []*TypeParam{tp})
	list.AddVariant(NewVariant("Cons", []*VarField{
		NewVarField("head", tp),
		NewVarField("tail", NewOptional(NewInstance(list, []Type{tp}))),
	}))
	list.AddVariant(NewVariant("Nil", nil))

	assertFalse(t, ContainsFailableTask(NewInstance(list, []Type{TypInt})),
		"recursive generic ListG[int] terminates and owns no failable task")
	assertTrue(t, ContainsFailableTask(NewInstance(list, []Type{NewInstance(TypFailableTask, []Type{TypInt})})),
		"ListG[failable_task[int]] reports the handle its payload carries")
}
