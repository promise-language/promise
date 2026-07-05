package sema

import "testing"

// T0666: a `clone generic type/enum with a bare-TypeParam field, instantiated
// with a non-`clone / non-`copy / no-clone() heap type (e.g. an enum with heap
// variant data), must be a clean compile error at the instantiation site —
// otherwise codegen's AutoClone path bit-copies the field and double-frees the
// shared heap payload at drop.

func TestT0666CloneTypeArgNonCloneableEnum(t *testing.T) {
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		type Box[T] `+"`clone"+` { T val; }
		main() {
			Box[Heapy] j = Box[Heapy](val: Heapy.Word("x"));
			Box[Heapy] c = j.clone();
		}
	`)
	expectError(t, errs, "not cloneable")
}

func TestT0666CloneEnumArgNonCloneableEnum(t *testing.T) {
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		enum Wrap[T] `+"`clone"+` { Plain(T val), Nothing }
		main() {
			Wrap[Heapy] j = Wrap[Heapy].Plain(Heapy.Word("x"));
			Wrap[Heapy] c = j.clone();
		}
	`)
	expectError(t, errs, "not cloneable")
}

func TestT0666CloneTypeArgOptionalNonCloneable(t *testing.T) {
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		type OptBox[T] `+"`clone"+` { T? maybe; }
		main() {
			OptBox[Heapy] j = OptBox[Heapy](maybe: Heapy.Word("x"));
			OptBox[Heapy] c = j.clone();
		}
	`)
	expectError(t, errs, "not cloneable")
}

func TestT0666CloneTypeArgNonCloneableStruct(t *testing.T) {
	// A plain struct with a heap field and no clone() method is non-cloneable —
	// consistent with the concrete-field rule (a `clone type carrying such a
	// field errors identically).
	errs := checkErrs(t, `
		type Heap2 { string s; }
		type Box[T] `+"`clone"+` { T val; }
		main() {
			Box[Heap2] j = Box[Heap2](val: Heap2(s: "x"));
			Box[Heap2] c = j.clone();
		}
	`)
	expectError(t, errs, "not cloneable")
}

func TestT0666CloneTypeArgNonCloneableInheritedField(t *testing.T) {
	// The non-cloneable arg reaches a field INHERITED from a generic `clone
	// parent (`Sub[T] is Base[T]`). AllFields() surfaces it with the parent's
	// TypeParam, so validateCloneInstance must merge the parent-arg substitution
	// to re-check it — otherwise Sub[Heapy] slips the gate and leaks the shared
	// heap payload at drop (codegen sibling T1202 covers the cloneable-arg leak).
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		type Base[T] `+"`clone"+` { T val; }
		type Sub[T] is Base[T] `+"`clone"+` { }
		main() {
			Sub[Heapy] j = Sub[Heapy](val: Heapy.Word("x"));
			Sub[Heapy] c = j.clone();
		}
	`)
	expectError(t, errs, "not cloneable")
}

func TestT0666CloneTypeArgTupleNonCloneable(t *testing.T) {
	// A `(T, int)` tuple field with a non-cloneable T reaches isAutoCloneTypeArg's
	// *types.Tuple arm — every element must be cloneable, so Heapy makes the whole
	// tuple non-cloneable (mirrors the T0667 tuple codegen shape).
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		type TupBox[T] `+"`clone"+` { (T, int) pr; }
		main() {
			TupBox[Heapy] j = TupBox[Heapy](pr: (Heapy.Word("x"), 7));
			TupBox[Heapy] c = j.clone();
		}
	`)
	expectError(t, errs, "not cloneable")
}

func TestT0666CloneTypeArgArrayNonCloneable(t *testing.T) {
	// A fixed-size `T[3]` array field with a non-cloneable element reaches
	// isAutoCloneTypeArg's *types.Array arm (mirrors the T0662 array codegen shape).
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		type ArrBox[T] `+"`clone"+` { T[3] arr; }
		main() {
			ArrBox[Heapy] j = ArrBox[Heapy](arr: [Heapy.Word("x"), Heapy.Empty, Heapy.Empty]);
			ArrBox[Heapy] c = j.clone();
		}
	`)
	expectError(t, errs, "not cloneable")
}

func TestT0666CloneTypeArgMixedConcreteAndTypeParamField(t *testing.T) {
	// A `clone type with a leading CONCRETE field (`int n`) plus a TypeParam field
	// exercises validateCloneInstance's "concrete field already checked" continue
	// (types.ContainsTypeParam(f.Type()) == false) before it reaches the
	// non-cloneable T field that must still be rejected.
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		type Box[T] `+"`clone"+` { int n; T val; }
		main() {
			Box[Heapy] j = Box[Heapy](n: 1, val: Heapy.Word("x"));
			Box[Heapy] c = j.clone();
		}
	`)
	expectError(t, errs, "not cloneable")
}

func TestT0666CloneEnumArgMixedConcreteAndTypeParamField(t *testing.T) {
	// Enum-branch sibling: a variant carrying a concrete field (`int n`) alongside
	// the TypeParam field exercises the enum-branch concrete-field continue.
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		enum Wrap[T] `+"`clone"+` { Tagged(int n, T val), Nothing }
		main() {
			Wrap[Heapy] j = Wrap[Heapy].Tagged(1, Heapy.Word("x"));
			Wrap[Heapy] c = j.clone();
		}
	`)
	expectError(t, errs, "not cloneable")
}

func TestT0666CloneEnumArgTupleNonCloneable(t *testing.T) {
	// The enum-level branch of validateCloneInstance with a tuple variant field.
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		enum TupWrap[T] `+"`clone"+` { Pair((T, int) pr), Nope }
		main() {
			TupWrap[Heapy] j = TupWrap[Heapy].Pair((Heapy.Word("x"), 7));
			TupWrap[Heapy] c = j.clone();
		}
	`)
	expectError(t, errs, "not cloneable")
}

// --- Positive cases: must NOT be rejected (guard against over-rejection) ---

func TestT0666CloneTypeArgTupleOK(t *testing.T) {
	// The *types.Tuple arm's success path (return true): a `(T, int)` field with a
	// cloneable T (string) must NOT be rejected.
	expectNoErrors(t, checkErrs(t, `
		type TupBox[T] `+"`clone"+` { (T, int) pr; }
		main() {
			TupBox[string] b = TupBox[string](pr: ("x", 7));
			TupBox[string] c = b.clone();
		}
	`))
}

func TestT0666CloneTypeArgArrayOK(t *testing.T) {
	// The *types.Array arm's success path: a `T[3]` field of a copy element type.
	expectNoErrors(t, checkErrs(t, `
		type ArrBox[T] `+"`clone"+` { T[3] arr; }
		main() {
			ArrBox[int] b = ArrBox[int](arr: [1, 2, 3]);
			ArrBox[int] c = b.clone();
		}
	`))
}

func TestT0666CloneTypeArgStillGenericDeferred(t *testing.T) {
	// `Box[T]` referenced inside a generic function body: the substituted field
	// type is still a bare TypeParam, so validateCloneInstance must DEFER (the
	// `ContainsTypeParam(concrete)` continue) rather than falsely reject — the
	// concrete instantiation `Box[int]` is validated at its own use site.
	expectNoErrors(t, checkErrs(t, `
		type Box[T] `+"`clone"+` { T val; }
		wrap[T](T v) Box[T] { return Box[T](val: v); }
		main() {
			Box[int] b = wrap[int](5);
		}
	`))
}

func TestT0666CloneTypeArgCopyOK(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Box[T] `+"`clone"+` { T val; }
		main() {
			Box[int] b = Box[int](val: 5);
			Box[int] c = b.clone();
		}
	`))
}

func TestT0666CloneTypeArgStringOK(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Box[T] `+"`clone"+` { T val; }
		main() {
			Box[string] b = Box[string](val: "x");
			Box[string] c = b.clone();
		}
	`))
}

func TestT0666CloneTypeArgVectorOK(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Box[T] `+"`clone"+` { T val; }
		main() {
			Box[int[]] b = Box[int[]](val: [1, 2, 3]);
			Box[int[]] c = b.clone();
		}
	`))
}

func TestT0666CloneTypeArgOptionalStringOK(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type OptBox[T] `+"`clone"+` { T? maybe; }
		main() {
			OptBox[string] b = OptBox[string](maybe: "x");
			OptBox[string] c = b.clone();
		}
	`))
}

func TestT0666CloneTypeArgUserCloneTypeOK(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type UC `+"`clone"+` { string s; }
		type Box[T] `+"`clone"+` { T val; }
		main() {
			Box[UC] b = Box[UC](val: UC(s: "x"));
			Box[UC] c = b.clone();
		}
	`))
}

func TestT0666CloneVectorFieldOfHeapyOK(t *testing.T) {
	// A `clone type with a T[] field instantiated with a non-cloneable element
	// clones via the Vector element-clone path (T0607) — Vector itself has a
	// native clone(), so the field is cloneable regardless of element type.
	expectNoErrors(t, checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		type VecBox[T] `+"`clone"+` { T[] items; }
		main() {
			VecBox[Heapy] b = VecBox[Heapy](items: [Heapy.Word("x")]);
			VecBox[Heapy] c = b.clone();
		}
	`))
}
