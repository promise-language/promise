package types

import (
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────

func assertEqual(t *testing.T, got, want interface{}) {
	t.Helper()
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func assertNil(t *testing.T, v interface{}) {
	t.Helper()
	if v != nil {
		t.Errorf("expected nil, got %v", v)
	}
}

func assertTrue(t *testing.T, v bool, msg string) {
	t.Helper()
	if !v {
		t.Errorf("expected true: %s", msg)
	}
}

func assertFalse(t *testing.T, v bool, msg string) {
	t.Helper()
	if v {
		t.Errorf("expected false: %s", msg)
	}
}

// makeNamed is a helper to create a named type with a given name.
func makeNamed(name string) *Named {
	tn := NewTypeName(Pos{}, name, nil)
	return NewNamed(tn, nil)
}

// ── Named types ─────────────────────────────────────────────────────

func TestNamed(t *testing.T) {
	tests := []struct {
		name  string
		check func(t *testing.T)
	}{
		{
			name: "create_simple",
			check: func(t *testing.T) {
				n := makeNamed("Dog")
				assertEqual(t, n.String(), "Dog")
				assertEqual(t, n.Obj().Name(), "Dog")
				assertEqual(t, n.NumFields(), 0)
				assertEqual(t, n.NumMethods(), 0)
			},
		},
		{
			name: "add_field",
			check: func(t *testing.T) {
				n := makeNamed("Player")
				f := NewField(Pos{}, "name", TypString, PlaceInstance, false, false)
				n.AddField(f)
				assertEqual(t, n.NumFields(), 1)
				assertEqual(t, n.Fields()[0].Name(), "name")
				assertEqual(t, n.Fields()[0].Type(), Type(TypString))
			},
		},
		{
			name: "add_method",
			check: func(t *testing.T) {
				n := makeNamed("Dog")
				sig := NewSignature(nil, nil, TypString, false)
				m := NewMethod(Pos{}, "speak", sig, PlaceInstance, false, false)
				n.AddMethod(m)
				assertEqual(t, n.NumMethods(), 1)
				assertEqual(t, n.Methods()[0].Name(), "speak")
			},
		},
		{
			name: "lookup_field_direct",
			check: func(t *testing.T) {
				n := makeNamed("Player")
				n.AddField(NewField(Pos{}, "health", TypInt, PlaceInstance, false, false))
				n.AddField(NewField(Pos{}, "name", TypString, PlaceInstance, false, false))

				f := n.LookupField("health")
				assertEqual(t, f.Name(), "health")

				f2 := n.LookupField("name")
				assertEqual(t, f2.Name(), "name")

				f3 := n.LookupField("missing")
				if f3 != nil {
					t.Errorf("expected nil for missing field")
				}
			},
		},
		{
			name: "lookup_field_inherited",
			check: func(t *testing.T) {
				animal := makeNamed("Animal")
				animal.AddField(NewField(Pos{}, "name", TypString, PlaceInstance, false, false))

				dog := makeNamed("Dog")
				dog.AddParent(animal)
				dog.AddField(NewField(Pos{}, "breed", TypString, PlaceInstance, false, false))

				// Own field
				assertEqual(t, dog.LookupField("breed").Name(), "breed")
				// Inherited field
				assertEqual(t, dog.LookupField("name").Name(), "name")
				// Missing
				if dog.LookupField("missing") != nil {
					t.Errorf("expected nil for missing field")
				}
			},
		},
		{
			name: "lookup_method_direct",
			check: func(t *testing.T) {
				n := makeNamed("Dog")
				sig := NewSignature(nil, nil, TypString, false)
				n.AddMethod(NewMethod(Pos{}, "speak", sig, PlaceInstance, false, false))

				m := n.LookupMethod("speak")
				assertEqual(t, m.Name(), "speak")

				if n.LookupMethod("missing") != nil {
					t.Errorf("expected nil for missing method")
				}
			},
		},
		{
			name: "lookup_method_inherited",
			check: func(t *testing.T) {
				animal := makeNamed("Animal")
				sig := NewSignature(nil, nil, TypString, false)
				animal.AddMethod(NewMethod(Pos{}, "eat", sig, PlaceInstance, false, false))

				dog := makeNamed("Dog")
				dog.AddParent(animal)
				dogSig := NewSignature(nil, nil, TypString, false)
				dog.AddMethod(NewMethod(Pos{}, "fetch", dogSig, PlaceInstance, false, false))

				// Own method
				assertEqual(t, dog.LookupMethod("fetch").Name(), "fetch")
				// Inherited method
				assertEqual(t, dog.LookupMethod("eat").Name(), "eat")
			},
		},
		{
			name: "method_override",
			check: func(t *testing.T) {
				animal := makeNamed("Animal")
				aSig := NewSignature(nil, nil, TypString, false)
				animal.AddMethod(NewMethod(Pos{}, "speak", aSig, PlaceInstance, true, false))

				dog := makeNamed("Dog")
				dog.AddParent(animal)
				dSig := NewSignature(nil, nil, TypString, false)
				dog.AddMethod(NewMethod(Pos{}, "speak", dSig, PlaceInstance, false, false))

				// Dog's speak overrides Animal's
				m := dog.LookupMethod("speak")
				assertFalse(t, m.IsAbstract(), "Dog.speak should not be abstract")
			},
		},
		{
			name: "is_abstract_all_abstract",
			check: func(t *testing.T) {
				shape := makeNamed("Shape")
				sig := NewSignature(nil, nil, TypF64, false)
				shape.AddMethod(NewMethod(Pos{}, "area", sig, PlaceInstance, true, false))

				assertTrue(t, shape.IsAbstract(), "Shape with abstract method should be abstract")
			},
		},
		{
			name: "is_abstract_concrete",
			check: func(t *testing.T) {
				dog := makeNamed("Dog")
				sig := NewSignature(nil, nil, TypString, false)
				dog.AddMethod(NewMethod(Pos{}, "speak", sig, PlaceInstance, false, false))

				assertFalse(t, dog.IsAbstract(), "Dog with concrete method should not be abstract")
			},
		},
		{
			name: "is_abstract_inherited_abstract",
			check: func(t *testing.T) {
				shape := makeNamed("Shape")
				sig := NewSignature(nil, nil, TypF64, false)
				shape.AddMethod(NewMethod(Pos{}, "area", sig, PlaceInstance, true, false))

				// Circle extends Shape but does NOT override area
				circle := makeNamed("Circle")
				circle.AddParent(shape)

				assertTrue(t, circle.IsAbstract(), "Circle without area override should be abstract")
			},
		},
		{
			name: "is_abstract_overridden",
			check: func(t *testing.T) {
				shape := makeNamed("Shape")
				aSig := NewSignature(nil, nil, TypF64, false)
				shape.AddMethod(NewMethod(Pos{}, "area", aSig, PlaceInstance, true, false))

				// Circle extends Shape and overrides area
				circle := makeNamed("Circle")
				circle.AddParent(shape)
				cSig := NewSignature(nil, nil, TypF64, false)
				circle.AddMethod(NewMethod(Pos{}, "area", cSig, PlaceInstance, false, false))

				assertFalse(t, circle.IsAbstract(), "Circle with area override should not be abstract")
			},
		},
		{
			name: "multiple_inheritance",
			check: func(t *testing.T) {
				named := makeNamed("Named")
				named.AddField(NewField(Pos{}, "name", TypString, PlaceInstance, false, false))

				audible := makeNamed("Audible")
				sig := NewSignature(nil, nil, TypString, false)
				audible.AddMethod(NewMethod(Pos{}, "speak", sig, PlaceInstance, true, false))

				dog := makeNamed("Dog")
				dog.AddParent(named)
				dog.AddParent(audible)

				// Inherited from Named
				assertEqual(t, dog.LookupField("name").Name(), "name")
				// Inherited from Audible
				assertEqual(t, dog.LookupMethod("speak").Name(), "speak")
			},
		},
		{
			name: "primitives_are_named",
			check: func(t *testing.T) {
				// Verify that built-in types are Named
				assertEqual(t, TypInt.String(), "int")
				assertEqual(t, TypBool.String(), "bool")
				assertEqual(t, TypString.String(), "string")
				assertEqual(t, TypF64.String(), "f64")
				assertEqual(t, TypVoid.String(), "void")
				assertEqual(t, TypNone.String(), "none")
			},
		},
		{
			name: "field_placement",
			check: func(t *testing.T) {
				n := makeNamed("Player")
				n.AddField(NewField(Pos{}, "x", TypF64, PlaceValue, false, false))
				n.AddField(NewField(Pos{}, "name", TypString, PlaceInstance, false, false))
				n.AddField(NewField(Pos{}, "sprite", TypString, PlaceVariant, false, false))
				n.AddField(NewField(Pos{}, "typeName", TypString, PlaceType, false, false))

				assertEqual(t, n.LookupField("x").Placement(), PlaceValue)
				assertEqual(t, n.LookupField("name").Placement(), PlaceInstance)
				assertEqual(t, n.LookupField("sprite").Placement(), PlaceVariant)
				assertEqual(t, n.LookupField("typeName").Placement(), PlaceType)
			},
		},
		{
			name: "raw_field",
			check: func(t *testing.T) {
				n := makeNamed("int")
				n.AddField(NewField(Pos{}, "value", nil, PlaceValue, true, false))

				f := n.LookupField("value")
				assertTrue(t, f.IsRaw(), "field should be raw")
				assertEqual(t, f.Placement(), PlaceValue)
			},
		},
		{
			name: "type_params",
			check: func(t *testing.T) {
				tn := NewTypeName(Pos{}, "List", nil)
				tpObj := NewTypeName(Pos{}, "T", nil)
				tp := NewTypeParam(tpObj, nil, 0)
				n := NewNamed(tn, []*TypeParam{tp})

				assertEqual(t, len(n.TypeParams()), 1)
				assertEqual(t, n.TypeParams()[0].Obj().Name(), "T")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.check)
	}
}

// ── Enum types ──────────────────────────────────────────────────────

func TestEnum(t *testing.T) {
	tests := []struct {
		name  string
		check func(t *testing.T)
	}{
		{
			name: "create_simple",
			check: func(t *testing.T) {
				tn := NewTypeName(Pos{}, "Color", nil)
				e := NewEnum(tn, nil)
				assertEqual(t, e.String(), "Color")
			},
		},
		{
			name: "add_variants",
			check: func(t *testing.T) {
				tn := NewTypeName(Pos{}, "Color", nil)
				e := NewEnum(tn, nil)
				e.AddVariant(NewVariant("Red", nil))
				e.AddVariant(NewVariant("Green", nil))
				e.AddVariant(NewVariant("Blue", nil))

				assertEqual(t, len(e.Variants()), 3)
				assertEqual(t, e.Variants()[0].Name(), "Red")
			},
		},
		{
			name: "lookup_variant",
			check: func(t *testing.T) {
				tn := NewTypeName(Pos{}, "Shape", nil)
				e := NewEnum(tn, nil)
				e.AddVariant(NewVariant("Circle", []*VarField{
					NewVarField("radius", TypF64),
				}))
				e.AddVariant(NewVariant("Rect", []*VarField{
					NewVarField("w", TypF64),
					NewVarField("h", TypF64),
				}))

				v := e.LookupVariant("Circle")
				assertEqual(t, v.Name(), "Circle")
				assertEqual(t, v.NumFields(), 1)
				assertEqual(t, v.Fields()[0].Name(), "radius")

				v2 := e.LookupVariant("Rect")
				assertEqual(t, v2.NumFields(), 2)

				if e.LookupVariant("Missing") != nil {
					t.Errorf("expected nil for missing variant")
				}
			},
		},
		{
			name: "variant_positional_fields",
			check: func(t *testing.T) {
				tn := NewTypeName(Pos{}, "Result", nil)
				e := NewEnum(tn, nil)
				e.AddVariant(NewVariant("Ok", []*VarField{
					NewVarField("", TypInt),
				}))

				v := e.LookupVariant("Ok")
				assertEqual(t, v.Fields()[0].Name(), "")
				assertEqual(t, v.Fields()[0].Type(), Type(TypInt))
			},
		},
		{
			name: "enum_method",
			check: func(t *testing.T) {
				tn := NewTypeName(Pos{}, "Color", nil)
				e := NewEnum(tn, nil)
				sig := NewSignature(nil, nil, TypString, false)
				e.AddMethod(NewMethod(Pos{}, "name", sig, PlaceInstance, false, false))

				m := e.LookupMethod("name")
				assertEqual(t, m.Name(), "name")
				if e.LookupMethod("missing") != nil {
					t.Errorf("expected nil for missing method")
				}
			},
		},
		{
			name: "variant_string",
			check: func(t *testing.T) {
				v1 := NewVariant("None", nil)
				assertEqual(t, v1.String(), "None")

				v2 := NewVariant("Some", []*VarField{
					NewVarField("", TypInt),
				})
				assertEqual(t, v2.String(), "Some(int)")

				v3 := NewVariant("Pair", []*VarField{
					NewVarField("first", TypInt),
					NewVarField("second", TypString),
				})
				assertEqual(t, v3.String(), "Pair(int first, string second)")
			},
		},
		{
			name: "generic_enum",
			check: func(t *testing.T) {
				tn := NewTypeName(Pos{}, "Option", nil)
				tpObj := NewTypeName(Pos{}, "T", nil)
				tp := NewTypeParam(tpObj, nil, 0)
				e := NewEnum(tn, []*TypeParam{tp})

				assertEqual(t, len(e.TypeParams()), 1)
				assertEqual(t, e.TypeParams()[0].Obj().Name(), "T")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.check)
	}
}

// ── Signature ───────────────────────────────────────────────────────

func TestSignature(t *testing.T) {
	tests := []struct {
		name  string
		check func(t *testing.T)
	}{
		{
			name: "no_params_no_return",
			check: func(t *testing.T) {
				sig := NewSignature(nil, nil, nil, false)
				assertEqual(t, sig.String(), "()")
				assertFalse(t, sig.CanError(), "should not have error")
			},
		},
		{
			name: "single_param",
			check: func(t *testing.T) {
				params := []*Param{NewParam("x", TypInt, RefNone)}
				sig := NewSignature(nil, params, TypBool, false)
				assertEqual(t, sig.String(), "(int) -> bool")
			},
		},
		{
			name: "multiple_params",
			check: func(t *testing.T) {
				params := []*Param{
					NewParam("a", TypInt, RefNone),
					NewParam("b", TypString, RefNone),
				}
				sig := NewSignature(nil, params, TypBool, false)
				assertEqual(t, sig.String(), "(int, string) -> bool")
			},
		},
		{
			name: "ref_params",
			check: func(t *testing.T) {
				params := []*Param{
					NewParam("s", TypString, RefShared),
					NewParam("arr", NewVector(TypInt), RefMut),
				}
				sig := NewSignature(nil, params, nil, false)
				assertEqual(t, sig.String(), "(string&, int[]~)")
			},
		},
		{
			name: "can_error",
			check: func(t *testing.T) {
				sig := NewSignature(nil, nil, TypString, true)
				assertTrue(t, sig.CanError(), "should have error")
				assertEqual(t, sig.String(), "() -> string!")
			},
		},
		{
			name: "with_receiver",
			check: func(t *testing.T) {
				dog := makeNamed("Dog")
				recv := NewParam("this", dog, RefNone)
				sig := NewSignature(recv, nil, TypString, false)
				assertEqual(t, sig.Recv().Name(), "this")
				assertEqual(t, sig.Recv().Type(), Type(dog))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.check)
	}
}

// ── Container types ─────────────────────────────────────────────────

func TestContainers(t *testing.T) {
	tests := []struct {
		name  string
		check func(t *testing.T)
	}{
		{
			name: "tuple",
			check: func(t *testing.T) {
				tup := NewTuple([]Type{TypInt, TypString})
				assertEqual(t, tup.String(), "(int, string)")
				assertEqual(t, len(tup.Elems()), 2)
			},
		},
		{
			name: "tuple_single",
			check: func(t *testing.T) {
				tup := NewTuple([]Type{TypBool})
				assertEqual(t, tup.String(), "(bool)")
			},
		},
		{
			name: "array",
			check: func(t *testing.T) {
				arr := NewArray(TypInt, 10)
				assertEqual(t, arr.String(), "int[10]")
				assertEqual(t, arr.Elem(), Type(TypInt))
				assertEqual(t, arr.Size(), int64(10))
			},
		},
		{
			name: "slice",
			check: func(t *testing.T) {
				sl := NewVector(TypString)
				assertEqual(t, sl.String(), "string[]")
				elem, ok := AsVector(sl)
				if !ok {
					t.Fatal("expected Slice instance")
				}
				assertEqual(t, elem, Type(TypString))
			},
		},
		{
			name: "nested",
			check: func(t *testing.T) {
				// int[][]
				inner := NewVector(TypInt)
				outer := NewVector(inner)
				assertEqual(t, outer.String(), "int[][]")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.check)
	}
}

// ── Reference types ─────────────────────────────────────────────────

func TestRefs(t *testing.T) {
	tests := []struct {
		name  string
		check func(t *testing.T)
	}{
		{
			name: "optional",
			check: func(t *testing.T) {
				opt := NewOptional(TypInt)
				assertEqual(t, opt.String(), "int?")
				assertEqual(t, opt.Elem(), Type(TypInt))
			},
		},
		{
			name: "shared_ref",
			check: func(t *testing.T) {
				r := NewSharedRef(TypString)
				assertEqual(t, r.String(), "string&")
			},
		},
		{
			name: "mut_ref",
			check: func(t *testing.T) {
				r := NewMutRef(NewVector(TypInt))
				assertEqual(t, r.String(), "int[]~")
			},
		},
		{
			name: "pointer",
			check: func(t *testing.T) {
				p := NewPointer(TypInt)
				assertEqual(t, p.String(), "int*")
			},
		},
		{
			name: "nested_optional",
			check: func(t *testing.T) {
				// int&?  (SharedRef of int, then Optional of that)
				opt := NewOptional(NewSharedRef(TypInt))
				assertEqual(t, opt.String(), "int&?")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.check)
	}
}

// ── TypeParam & Instance ────────────────────────────────────────────

func TestTypeParam(t *testing.T) {
	tests := []struct {
		name  string
		check func(t *testing.T)
	}{
		{
			name: "unconstrained",
			check: func(t *testing.T) {
				tn := NewTypeName(Pos{}, "T", nil)
				tp := NewTypeParam(tn, nil, 0)
				assertEqual(t, tp.String(), "T")
				assertEqual(t, tp.Index(), 0)
				if tp.Constraint() != nil {
					t.Errorf("expected nil constraint")
				}
			},
		},
		{
			name: "constrained",
			check: func(t *testing.T) {
				hashable := makeNamed("Hashable")
				tn := NewTypeName(Pos{}, "K", nil)
				tp := NewTypeParam(tn, hashable, 0)
				assertEqual(t, tp.String(), "K")
				assertEqual(t, tp.Constraint(), Type(hashable))
			},
		},
		{
			name: "instance",
			check: func(t *testing.T) {
				list := makeNamed("List")
				inst := NewInstance(list, []Type{TypInt})
				assertEqual(t, inst.String(), "List[int]")
				assertEqual(t, inst.Origin(), Type(list))
				assertEqual(t, len(inst.TypeArgs()), 1)
			},
		},
		{
			name: "instance_multi_args",
			check: func(t *testing.T) {
				mp := makeNamed("map")
				inst := NewInstance(mp, []Type{TypString, TypInt})
				assertEqual(t, inst.String(), "map[string, int]")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.check)
	}
}

// ── Scope ───────────────────────────────────────────────────────────

func TestScope(t *testing.T) {
	tests := []struct {
		name  string
		check func(t *testing.T)
	}{
		{
			name: "insert_and_lookup",
			check: func(t *testing.T) {
				s := NewScope(nil, Pos{}, Pos{}, "test")
				v := NewVar(Pos{}, "x", TypInt)
				existing := s.Insert(v)
				if existing != nil {
					t.Errorf("expected nil on first insert")
				}
				found := s.Lookup("x")
				assertEqual(t, found.Name(), "x")
			},
		},
		{
			name: "lookup_missing",
			check: func(t *testing.T) {
				s := NewScope(nil, Pos{}, Pos{}, "test")
				found := s.Lookup("missing")
				if found != nil {
					t.Errorf("expected nil for missing name")
				}
			},
		},
		{
			name: "insert_conflict",
			check: func(t *testing.T) {
				s := NewScope(nil, Pos{}, Pos{}, "test")
				v1 := NewVar(Pos{}, "x", TypInt)
				v2 := NewVar(Pos{}, "x", TypString)
				s.Insert(v1)
				existing := s.Insert(v2)
				assertEqual(t, existing.Name(), "x")
				// The scope still has the original
				assertEqual(t, s.Lookup("x").(*Var).Type(), Type(TypInt))
			},
		},
		{
			name: "lookup_parent",
			check: func(t *testing.T) {
				outer := NewScope(nil, Pos{}, Pos{}, "outer")
				outer.Insert(NewVar(Pos{}, "x", TypInt))

				inner := NewScope(outer, Pos{}, Pos{}, "inner")
				inner.Insert(NewVar(Pos{}, "y", TypString))

				// Find in current scope
				obj, scope := inner.LookupParent("y")
				assertEqual(t, obj.Name(), "y")
				assertEqual(t, scope.Comment(), "inner")

				// Find in parent scope
				obj, scope = inner.LookupParent("x")
				assertEqual(t, obj.Name(), "x")
				assertEqual(t, scope.Comment(), "outer")

				// Not found
				obj, scope = inner.LookupParent("missing")
				if obj != nil {
					t.Errorf("expected nil for missing name")
				}
			},
		},
		{
			name: "shadowing",
			check: func(t *testing.T) {
				outer := NewScope(nil, Pos{}, Pos{}, "outer")
				outer.Insert(NewVar(Pos{}, "x", TypInt))

				inner := NewScope(outer, Pos{}, Pos{}, "inner")
				inner.Insert(NewVar(Pos{}, "x", TypString))

				// Inner scope shadows outer
				obj, scope := inner.LookupParent("x")
				assertEqual(t, obj.(*Var).Type(), Type(TypString))
				assertEqual(t, scope.Comment(), "inner")
			},
		},
		{
			name: "names_sorted",
			check: func(t *testing.T) {
				s := NewScope(nil, Pos{}, Pos{}, "test")
				s.Insert(NewVar(Pos{}, "c", TypInt))
				s.Insert(NewVar(Pos{}, "a", TypInt))
				s.Insert(NewVar(Pos{}, "b", TypInt))

				names := s.Names()
				assertEqual(t, len(names), 3)
				assertEqual(t, names[0], "a")
				assertEqual(t, names[1], "b")
				assertEqual(t, names[2], "c")
			},
		},
		{
			name: "parent_child_relationship",
			check: func(t *testing.T) {
				parent := NewScope(nil, Pos{}, Pos{}, "parent")
				child := NewScope(parent, Pos{}, Pos{}, "child")

				assertEqual(t, child.Parent().Comment(), "parent")
				assertEqual(t, len(parent.Children()), 1)
				assertEqual(t, parent.Children()[0].Comment(), "child")
			},
		},
		{
			name: "set_parent_on_insert",
			check: func(t *testing.T) {
				s := NewScope(nil, Pos{}, Pos{}, "test")
				v := NewVar(Pos{}, "x", TypInt)
				if v.Parent() != nil {
					t.Errorf("expected nil parent before insert")
				}
				s.Insert(v)
				assertEqual(t, v.Parent().Comment(), "test")
			},
		},
		{
			name: "len",
			check: func(t *testing.T) {
				s := NewScope(nil, Pos{}, Pos{}, "test")
				assertEqual(t, s.Len(), 0)
				s.Insert(NewVar(Pos{}, "x", TypInt))
				assertEqual(t, s.Len(), 1)
				s.Insert(NewVar(Pos{}, "y", TypInt))
				assertEqual(t, s.Len(), 2)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.check)
	}
}

// ── Universe ────────────────────────────────────────────────────────

func TestUniverse(t *testing.T) {
	builtins := []string{
		"int", "i8", "i16", "i32", "i64",
		"uint", "u8", "u16", "u32", "u64",
		"f32", "f64",
		"bool", "char", "string",
		"void", "none",
	}

	for _, name := range builtins {
		t.Run(name, func(t *testing.T) {
			obj := Universe.Lookup(name)
			if obj == nil {
				t.Fatalf("built-in type %q not found in universe", name)
			}
			tn, ok := obj.(*TypeName)
			if !ok {
				t.Fatalf("expected *TypeName, got %T", obj)
			}
			named, ok := tn.Type().(*Named)
			if !ok {
				t.Fatalf("expected *Named, got %T", tn.Type())
			}
			assertEqual(t, named.String(), name)
		})
	}

	t.Run("universe_is_root", func(t *testing.T) {
		if Universe.Parent() != nil {
			t.Errorf("Universe should have nil parent")
		}
	})

	t.Run("predeclared_vars", func(t *testing.T) {
		assertEqual(t, TypInt.String(), "int")
		assertEqual(t, TypBool.String(), "bool")
		assertEqual(t, TypString.String(), "string")
		assertEqual(t, TypF32.String(), "f32")
		assertEqual(t, TypF64.String(), "f64")
		assertEqual(t, TypVoid.String(), "void")
		assertEqual(t, TypNone.String(), "none")
		// TypError is nil at init — populated by sema from std module
		if TypError != nil {
			t.Errorf("TypError should be nil at init, got %v", TypError)
		}
	})

	t.Run("no_basic_type", func(t *testing.T) {
		// All built-in types are *Named, not any special Basic type
		for _, name := range builtins {
			obj := Universe.Lookup(name)
			tn := obj.(*TypeName)
			if _, ok := tn.Type().(*Named); !ok {
				t.Errorf("type %q should be *Named, got %T", name, tn.Type())
			}
		}
	})
}

// ── Identical ───────────────────────────────────────────────────────

func TestIdentical(t *testing.T) {
	tests := []struct {
		name string
		x, y Type
		want bool
	}{
		// Same pointer = identical
		{"same_named", TypInt, TypInt, true},
		{"same_named_bool", TypBool, TypBool, true},

		// Different Named = not identical (nominal)
		{"different_named", TypInt, TypI32, false},
		{"different_named_2", TypInt, TypString, false},

		// Structural types
		{"same_slice", NewVector(TypInt), NewVector(TypInt), true},
		{"diff_slice", NewVector(TypInt), NewVector(TypString), false},
		{"same_array", NewArray(TypInt, 5), NewArray(TypInt, 5), true},
		{"diff_array_size", NewArray(TypInt, 5), NewArray(TypInt, 10), false},
		{"diff_array_elem", NewArray(TypInt, 5), NewArray(TypString, 5), false},
		{"same_optional", NewOptional(TypInt), NewOptional(TypInt), true},
		{"diff_optional", NewOptional(TypInt), NewOptional(TypString), false},
		{"same_shared_ref", NewSharedRef(TypInt), NewSharedRef(TypInt), true},
		{"diff_shared_ref", NewSharedRef(TypInt), NewSharedRef(TypString), false},
		{"same_mut_ref", NewMutRef(TypInt), NewMutRef(TypInt), true},
		{"diff_mut_ref", NewMutRef(TypInt), NewMutRef(TypString), false},
		{"same_pointer", NewPointer(TypInt), NewPointer(TypInt), true},
		{"diff_pointer", NewPointer(TypInt), NewPointer(TypString), false},
		{"same_tuple", NewTuple([]Type{TypInt, TypString}), NewTuple([]Type{TypInt, TypString}), true},
		{"diff_tuple", NewTuple([]Type{TypInt, TypString}), NewTuple([]Type{TypString, TypInt}), false},
		{"diff_tuple_len", NewTuple([]Type{TypInt}), NewTuple([]Type{TypInt, TypString}), false},

		// Signatures
		{
			"same_sig",
			NewSignature(nil, []*Param{NewParam("x", TypInt, RefNone)}, TypBool, false),
			NewSignature(nil, []*Param{NewParam("y", TypInt, RefNone)}, TypBool, false),
			true,
		},
		{
			"diff_sig_params",
			NewSignature(nil, []*Param{NewParam("x", TypInt, RefNone)}, TypBool, false),
			NewSignature(nil, []*Param{NewParam("x", TypString, RefNone)}, TypBool, false),
			false,
		},
		{
			"diff_sig_result",
			NewSignature(nil, nil, TypInt, false),
			NewSignature(nil, nil, TypBool, false),
			false,
		},
		{
			"diff_sig_error",
			NewSignature(nil, nil, TypInt, false),
			NewSignature(nil, nil, TypInt, true),
			false,
		},
		{
			"diff_sig_ref",
			NewSignature(nil, []*Param{NewParam("x", TypInt, RefShared)}, nil, false),
			NewSignature(nil, []*Param{NewParam("x", TypInt, RefMut)}, nil, false),
			false,
		},

		// Instance (generic instantiation)
		{
			"same_instance",
			NewInstance(makeNamed("List"), []Type{TypInt}),
			NewInstance(makeNamed("List"), []Type{TypInt}),
			false, // different List *Named pointers
		},

		// nil handling
		{"nil_nil", nil, nil, true},
		{"nil_named", nil, TypInt, false},
		{"named_nil", TypInt, nil, false},

		// Cross-kind
		{"named_vs_slice", TypInt, NewVector(TypInt), false},
		{"optional_vs_named", NewOptional(TypInt), TypInt, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Identical(tt.x, tt.y)
			if got != tt.want {
				t.Errorf("Identical(%v, %v) = %v, want %v", tt.x, tt.y, got, tt.want)
			}
		})
	}
}

// ── Assignability ───────────────────────────────────────────────────

func TestAssignableTo(t *testing.T) {
	// Set up inheritance: Dog is Animal
	animal := makeNamed("Animal")
	dog := makeNamed("Dog")
	dog.AddParent(animal)

	// Set up deep inheritance: Puppy is Dog is Animal
	puppy := makeNamed("Puppy")
	puppy.AddParent(dog)

	tests := []struct {
		name string
		x, y Type
		want bool
	}{
		// Rule 1: identical types
		{"identical_int", TypInt, TypInt, true},
		{"identical_string", TypString, TypString, true},

		// Rule 2: T assignable to T?
		{"int_to_optional_int", TypInt, NewOptional(TypInt), true},
		{"string_to_optional_string", TypString, NewOptional(TypString), true},
		{"int_to_optional_string", TypInt, NewOptional(TypString), false},

		// Rule 3: none to T?
		{"none_to_optional_int", TypNone, NewOptional(TypInt), true},
		{"none_to_optional_string", TypNone, NewOptional(TypString), true},

		// Rule 4: child to parent
		{"dog_to_animal", dog, animal, true},
		{"puppy_to_animal", puppy, animal, true},
		{"puppy_to_dog", puppy, dog, true},
		{"animal_to_dog", animal, dog, false},

		// Rule 5: TypeParam to constraint
		{
			"typeparam_to_constraint",
			func() Type {
				tn := NewTypeName(Pos{}, "T", nil)
				return NewTypeParam(tn, animal, 0)
			}(),
			animal,
			true,
		},

		// Rule 6: T assignable to T& (implicit shared borrow)
		{"string_to_shared_ref", TypString, NewSharedRef(TypString), true},
		{"int_to_shared_ref", TypInt, NewSharedRef(TypInt), true},
		{"dog_to_shared_ref_animal", dog, NewSharedRef(animal), true},
		{"int_to_shared_ref_string", TypInt, NewSharedRef(TypString), false},

		// Rule 7: T assignable to T~ (implicit mutable borrow)
		{"string_to_mut_ref", TypString, NewMutRef(TypString), true},
		{"int_to_mut_ref", TypInt, NewMutRef(TypInt), true},
		{"int_to_mut_ref_string", TypInt, NewMutRef(TypString), false},

		// Rule 8: T~ assignable to T& (mut ref coerces to shared ref)
		{"mut_ref_to_shared_ref", NewMutRef(TypString), NewSharedRef(TypString), true},
		{"mut_ref_to_shared_ref_mismatch", NewMutRef(TypInt), NewSharedRef(TypString), false},

		// Rule 8b (T0381): T& assignable to T (implicit decay)
		{"shared_ref_string_to_string", NewSharedRef(TypString), TypString, true},
		{"shared_ref_int_to_int", NewSharedRef(TypInt), TypInt, true},
		{"shared_ref_dog_to_animal", NewSharedRef(dog), animal, true},
		{"shared_ref_int_to_string", NewSharedRef(TypInt), TypString, false},

		// Rule 8c (T0381): T~ assignable to T (implicit decay)
		{"mut_ref_string_to_string", NewMutRef(TypString), TypString, true},
		{"mut_ref_int_to_int", NewMutRef(TypInt), TypInt, true},
		{"mut_ref_dog_to_animal", NewMutRef(dog), animal, true},
		{"mut_ref_int_to_string", NewMutRef(TypInt), TypString, false},

		// Not assignable
		{"int_to_string", TypInt, TypString, false},
		{"unrelated_types", makeNamed("Cat"), makeNamed("Fish"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AssignableTo(tt.x, tt.y)
			if got != tt.want {
				t.Errorf("AssignableTo(%v, %v) = %v, want %v", tt.x, tt.y, got, tt.want)
			}
		})
	}
}

// ── Implements ──────────────────────────────────────────────────────

func TestImplements(t *testing.T) {
	// Interface: all abstract
	drawable := makeNamed("Drawable")
	drawSig := NewSignature(nil, nil, nil, false)
	drawable.AddMethod(NewMethod(Pos{}, "draw", drawSig, PlaceInstance, true, false))

	// Concrete type implementing it
	circle := makeNamed("Circle")
	circle.AddMethod(NewMethod(Pos{}, "draw", drawSig, PlaceInstance, false, false))

	// Concrete type NOT implementing it
	square := makeNamed("Square")

	t.Run("implements", func(t *testing.T) {
		assertTrue(t, Implements(circle, drawable), "Circle should implement Drawable")
	})

	t.Run("not_implements", func(t *testing.T) {
		assertFalse(t, Implements(square, drawable), "Square should not implement Drawable")
	})

	t.Run("not_interface", func(t *testing.T) {
		concrete := makeNamed("Concrete")
		concrete.AddMethod(NewMethod(Pos{}, "foo", drawSig, PlaceInstance, false, false))
		assertFalse(t, Implements(circle, concrete), "non-interface should return false")
	})
}

// ── Format ──────────────────────────────────────────────────────────

func TestFormat(t *testing.T) {
	t.Run("type_string_nil", func(t *testing.T) {
		assertEqual(t, TypeString(nil), "<nil>")
	})

	t.Run("type_string_named", func(t *testing.T) {
		assertEqual(t, TypeString(TypInt), "int")
	})

	t.Run("object_string_var", func(t *testing.T) {
		v := NewVar(Pos{}, "x", TypInt)
		assertEqual(t, ObjectString(v), "var x int")
	})

	t.Run("object_string_func", func(t *testing.T) {
		sig := NewSignature(nil, []*Param{NewParam("x", TypInt, RefNone)}, TypBool, false)
		f := NewFunc(Pos{}, "check", sig)
		assertEqual(t, ObjectString(f), "func check(int) -> bool")
	})

	t.Run("object_string_typename", func(t *testing.T) {
		tn := NewTypeName(Pos{}, "Dog", nil)
		assertEqual(t, ObjectString(tn), "type Dog")
	})

	t.Run("object_string_label", func(t *testing.T) {
		l := NewLabel(Pos{}, "loop")
		assertEqual(t, ObjectString(l), "label loop")
	})

	t.Run("object_string_nil", func(t *testing.T) {
		assertEqual(t, ObjectString(nil), "<nil>")
	})

	t.Run("placement_strings", func(t *testing.T) {
		assertEqual(t, PlaceInstance.String(), "instance")
		assertEqual(t, PlaceValue.String(), "value")
		assertEqual(t, PlaceVariant.String(), "variant")
		assertEqual(t, PlaceType.String(), "type")
	})

	t.Run("refmod_strings", func(t *testing.T) {
		assertEqual(t, RefNone.String(), "")
		assertEqual(t, RefShared.String(), "&")
		assertEqual(t, RefMut.String(), "~")
	})
}

// ── Pos ─────────────────────────────────────────────────────────────

func TestPos(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		p := Pos{File: "test.pr", Line: 10, Column: 5}
		assertTrue(t, p.IsValid(), "should be valid")
		assertEqual(t, p.String(), "test.pr:10:5")
	})

	t.Run("no_file", func(t *testing.T) {
		p := Pos{Line: 1, Column: 0}
		assertEqual(t, p.String(), "1:0")
	})

	t.Run("invalid", func(t *testing.T) {
		p := Pos{}
		assertFalse(t, p.IsValid(), "zero Pos should be invalid")
	})
}
