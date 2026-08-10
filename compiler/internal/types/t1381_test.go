package types

import "testing"

// T1381: ContainsFailableTask reports whether a type transitively owns a
// failable_task[T] — the predicate that drives must-use linearity.
func TestContainsFailableTask(t *testing.T) {
	ft := NewInstance(TypFailableTask, []Type{TypInt}) // failable_task[int]

	// A nil type is not must-use (guards the ownership recordMustUse path).
	assertFalse(t, ContainsFailableTask(nil), "nil type")

	// Direct handle.
	assertTrue(t, ContainsFailableTask(ft), "failable_task[int]")

	// Not a failable task.
	assertFalse(t, ContainsFailableTask(TypInt), "int")
	assertFalse(t, ContainsFailableTask(NewInstance(TypTask, []Type{TypInt})), "plain task[int]")
	assertFalse(t, ContainsFailableTask(NewInstance(TypChannel, []Type{TypInt})), "channel[int]")

	// Containers over a failable task (via TypeArgs).
	assertTrue(t, ContainsFailableTask(NewVector(ft)), "failable_task[int][]")
	assertTrue(t, ContainsFailableTask(NewMap(TypString, ft)), "map[string, failable_task[int]]")
	assertTrue(t, ContainsFailableTask(NewOptional(ft)), "failable_task[int]?")
	assertTrue(t, ContainsFailableTask(NewArray(ft, 3)), "failable_task[int][3]")
	assertTrue(t, ContainsFailableTask(NewTuple([]Type{TypInt, ft})), "(int, failable_task[int])")

	// Container of a non-failable-task element is not must-use.
	assertFalse(t, ContainsFailableTask(NewVector(TypInt)), "int[]")

	// A bare TypeParam is not must-use (generic bodies are checked unbound; each
	// concrete instantiation is re-validated at its own site).
	tp := NewTypeParam(NewTypeName(Pos{}, "T", nil), nil, 0)
	assertFalse(t, ContainsFailableTask(tp), "bare TypeParam")
	assertFalse(t, ContainsFailableTask(NewVector(tp)), "T[]")
}

// A user type that transitively owns a failable_task is must-use.
func TestContainsFailableTaskUserTypes(t *testing.T) {
	ft := NewInstance(TypFailableTask, []Type{TypInt})

	// type Holder { failable_task[int] t; }
	holder := NewNamed(NewTypeName(Pos{}, "Holder", nil), nil)
	holder.AddField(NewField(Pos{}, "t", ft, PlaceInstance, false, false))
	assertTrue(t, ContainsFailableTask(holder), "Holder{ failable_task[int] t }")

	// A holder with only plain fields is not must-use.
	plain := NewNamed(NewTypeName(Pos{}, "Plain", nil), nil)
	plain.AddField(NewField(Pos{}, "n", TypInt, PlaceInstance, false, false))
	assertFalse(t, ContainsFailableTask(plain), "Plain{ int n }")

	// A self-referential type terminates (cycle guard) and, holding no failable
	// task, is not must-use.
	node := NewNamed(NewTypeName(Pos{}, "Node", nil), nil)
	node.AddField(NewField(Pos{}, "next", NewOptional(node), PlaceInstance, false, false))
	assertFalse(t, ContainsFailableTask(node), "recursive Node")
}

// An enum variant payload carrying a failable_task makes the enum must-use;
// the transitivity descends into variant fields (both the bare *Enum case and
// the generic-Instance-over-Enum case).
func TestContainsFailableTaskEnumPayload(t *testing.T) {
	ft := NewInstance(TypFailableTask, []Type{TypInt})

	// enum E { Some(failable_task[int]); None; }
	e := NewEnum(NewTypeName(Pos{}, "E", nil), nil)
	e.AddVariant(NewVariant("Some", []*VarField{NewVarField("", ft)}))
	e.AddVariant(NewVariant("None", nil))
	assertTrue(t, ContainsFailableTask(e), "enum with failable_task payload")

	// An enum whose variants carry only plain payloads is not must-use, and its
	// self-referential variant (a cons list) terminates via the cycle guard.
	list := NewEnum(NewTypeName(Pos{}, "List", nil), nil)
	list.AddVariant(NewVariant("Cons", []*VarField{
		NewVarField("head", TypInt),
		NewVarField("tail", NewOptional(list)),
	}))
	list.AddVariant(NewVariant("Nil", nil))
	assertFalse(t, ContainsFailableTask(list), "recursive plain enum")
}

// A generic user type/enum instance reaches a failable_task only after the
// type-arg substitution is applied to its fields/variants — e.g. Box[failable_task]
// whose field is `T value`. Covers the Instance→Named and Instance→Enum origin
// recursion (distinct from the by-TypeArgs path).
func TestContainsFailableTaskGenericInstanceSubst(t *testing.T) {
	// type Box[T] { failable_task[T] handle; } instantiated as Box[int]. The
	// failable_task is BUILT INSIDE the field, so it is not directly a type arg —
	// it is only reachable by substituting T→int into `failable_task[T]`, forcing
	// the Instance→Named field-recursion branch (not the by-TypeArgs shortcut).
	tpT := NewTypeParam(NewTypeName(Pos{}, "T", nil), nil, 0)
	box := NewNamed(NewTypeName(Pos{}, "Box", nil), []*TypeParam{tpT})
	box.AddField(NewField(Pos{}, "handle", NewInstance(TypFailableTask, []Type{tpT}), PlaceInstance, false, false))
	assertTrue(t, ContainsFailableTask(NewInstance(box, []Type{TypInt})), "Box[int]{ failable_task[T] } via field subst")

	// type Plain[T] { T value; } instantiated as Plain[int] — no failable task.
	plainG := NewNamed(NewTypeName(Pos{}, "PlainG", nil), []*TypeParam{tpT})
	plainG.AddField(NewField(Pos{}, "value", tpT, PlaceInstance, false, false))
	assertFalse(t, ContainsFailableTask(NewInstance(plainG, []Type{TypInt})), "PlainG[int]")

	// type Wrap[T] { Payload(failable_task[T]) } enum instantiated as Wrap[int]:
	// the payload `failable_task[T]` becomes concrete only under substitution,
	// forcing the Instance→Enum variant-recursion branch.
	tpU := NewTypeParam(NewTypeName(Pos{}, "U", nil), nil, 0)
	we := NewEnum(NewTypeName(Pos{}, "Wrap", nil), []*TypeParam{tpU})
	we.AddVariant(NewVariant("Payload", []*VarField{NewVarField("", NewInstance(TypFailableTask, []Type{tpU}))}))
	assertTrue(t, ContainsFailableTask(NewInstance(we, []Type{TypInt})), "Wrap[int]{ Payload(failable_task[U]) } via variant subst")

	// enum WrapPlain[T] { Payload(T) } instantiated as WrapPlain[int] — no task.
	weP := NewEnum(NewTypeName(Pos{}, "WrapPlain", nil), []*TypeParam{tpU})
	weP.AddVariant(NewVariant("Payload", []*VarField{NewVarField("", tpU)}))
	assertFalse(t, ContainsFailableTask(NewInstance(weP, []Type{TypInt})), "WrapPlain[int]")
}
