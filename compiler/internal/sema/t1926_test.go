package sema

import (
	"fmt"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/types"
)

// T1926 — the container-element nesting rule is DERIVED from a type's fields,
// not recovered from its identity. `Map` and `Set` used to be named in three
// predicates in clone.go; the derivation that replaces them (ownsElementsByValue)
// reaches the same answer through the fields those types already have, and
// therefore covers a user's own container identically. These tests are written
// against user types on purpose: before T1926 every one of the rejections below
// was silently accepted.

// The headline fix: a user generic container that holds its elements in a
// Vector field is structurally what Map is, and is now judged the same way.
func TestT1926_UserVectorFieldContainerNestingRejected(t *testing.T) {
	errs := checkErrs(t, `
		type MyVec[T] { T[] items; }
		test() {
			Vector[MyVec[Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// The enum arm of the same walk: a generic enum whose variant holds a Vector
// owns its elements by value too. duplicatingContainerElemTypes accepts an
// *types.Enum origin, which the identity check it replaced could not.
func TestT1926_UserEnumVariantContainerNestingRejected(t *testing.T) {
	errs := checkErrs(t, `
		enum Bag[T] { Empty, Items(T[] xs) }
		test() {
			Vector[Bag[Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// Two hops: Outer holds Inner holds Vector. The property is transitive through
// fields, which is the whole basis for Map (Slot[K, V][]) and Set (Map[T, bool])
// needing no mention.
func TestT1926_TwoHopFieldWalkNestingRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Inner[T] { T[] xs; }
		type Outer[T] { Inner[T] i; }
		test() {
			Vector[Outer[Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// The item's own MyMap example, and the case that says the derivation is a
// derivation rather than a wider net: a type that merely *holds a T* does not
// own elements by value — duplicating it duplicates one field, not a buffer —
// so it is accepted, judged by exactly the rule that rejects Map.
func TestT1926_UserSingleFieldHolderNotAContainer(t *testing.T) {
	checkOK(t, `
		type MyBox[T] { T item; }
		test() {
			Vector[MyBox[Task[int]]] v = [];
		}
	`)
}

// ...and a type that owns a buffer of something ELSE is judged on where the type
// argument actually sits, not on whether the type owns a buffer at all. `Job`
// duplicates its tags, never its handle, so it is the `Box` case above with an
// unrelated vector attached, and stays a legal container element (T0508). Only
// the type args a by-value buffer HOLDS are reported.
func TestT1926_UnrelatedBufferDoesNotBlameTypeArg(t *testing.T) {
	checkOK(t, `
		type Job[T] { string[] tags; T handle; }
		test() {
			Vector[Job[Task[int]]] v = [];
		}
	`)
}

// The sibling-ordering guard for the buffer walk: `Two` reaches a buffer holding
// T only through its SECOND field, whose origin the FIRST field already walked
// at different type arguments. The walk's memo is keyed on the instantiation,
// not the origin, so the second field is judged on its own arguments.
func TestT1926_TwoFieldsSameOriginDifferentArgsRejected(t *testing.T) {
	errs := checkErrs(t, `
		type MyVec[T] { T[] items; }
		type Two[T] { MyVec[int] a; MyVec[T] b; }
		test() {
			Vector[Two[Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// The diagnostic names the handle the rule objects to. `MyVec3` holds `B` in a
// buffer and `A` as a plain field, so only `B` is a nesting; searching the whole
// instance would report `A` instead, because it comes first in type-argument
// order, and send the reader after a legal direct element. (T1926)
func TestT1926_DiagnosticNamesTheBufferedHandle(t *testing.T) {
	errs := checkErrs(t, `
		type MyVec3[A, B] { B[] xs; A other; }
		test() {
			Vector[MyVec3[Task[int], Mutex[int]]] v = [];
		}
	`)
	expectError(t, errs, "it transitively contains Mutex[int], a single-owner handle")
}

// The buffer may be one only the substitution produces: `Wrapper[T] { T inner; }`
// holds nothing by value as declared, but at T = Vector[Task[int]] the field IS
// the buffer and IS the type argument. Fields are therefore walked substituted.
func TestT1926_BufferProducedBySubstitutionRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Wrapper[T] { T inner; }
		test() {
			Vector[Wrapper[Vector[Task[int]]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// memory-model.md §3: a field held behind a handle is not held by value. Ref's
// dup is a refcount bump, so H[Task[int]] duplicates no task and the walk must
// stop at Ref — which it does because Ref is `native with no Promise-level
// fields, not because anything names it.
func TestT1926_HandleFieldIsNotByValue(t *testing.T) {
	checkOK(t, `
		type H[T] { Ref[T] r; }
		test() {
			Vector[H[Task[int]]] v = [];
		}
	`)
}

// The walk is cycle-guarded on the origin pointer, so a self-referential type
// that also owns a Vector terminates.
func TestT1926_RecursiveByValueContainerTerminates(t *testing.T) {
	checkOK(t, `
		type Node[T] { Node[T]? next; T[] xs; }
		test() {
			Vector[Node[int]] v = [];
		}
	`)
}

// Coverage held constant for the std containers the identity list used to name:
// both are still rejected, now without Map or Set appearing in the compiler.
func TestT1926_MapNestingStillRejectedWithoutIdentity(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			Vector[Map[string, Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

func TestT1926_SetNestingStillRejectedWithoutIdentity(t *testing.T) {
	// Task is neither Hashable nor Equal, so the constraint errors are expected
	// alongside; the nesting rule must still fire.
	errs := checkErrs(t, `
		test() {
			Vector[Set[Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// Nesting a plain Vector stays rejected — the base case, now reached through the
// `duplicates_elements annotation rather than a TypVector comparison.
func TestT1926_VectorNestingStillRejected(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			Vector[Vector[Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// The clone()/filled() gate reads the same derivation, so a user by-value
// container of handles can no longer escape it by supplying its own clone() —
// no hand-written clone can duplicate a Task.
func TestT1926_UserContainerCloneOfHandleRejected(t *testing.T) {
	errs := checkErrs(t, `
		type MyVec[T] { T[] items; clone() Self { return MyVec[T](items: []); } }
		worker() int { return 1; }
		test() {
			m := MyVec[Task[int]](items: [go worker()]);
			m2 := m.clone();
		}
	`)
	expectError(t, errs, "cannot be cloned")
}

// ...while the same container over a cloneable element still clones.
func TestT1926_UserContainerCloneOfPlainElementOK(t *testing.T) {
	checkOK(t, `
		type MyVec[T] { T[] items; clone() Self { return MyVec[T](items: []); } }
		test() {
			m := MyVec[int](items: [1, 2]);
			m2 := m.clone();
		}
	`)
}

// The generic-indirection deferral (T0616) reads the same derivation: inside a
// generic body the nested element is still a TypeParam, so the check is recorded
// against the enclosing function and re-run at each call site. isContainerWithTypeParam
// now reports the elements a buffer holds, so a USER container reaches that path
// exactly as Vector/Map/Set always did.
func TestT1926_UserContainerGenericIndirectionDeferred(t *testing.T) {
	errs := checkErrs(t, `
		type MyVec[T] { T[] items; }
		make_nested[T]() {
			Vector[MyVec[T]] v = [];
		}
		test() {
			make_nested[Task[int]]();
		}
	`)
	expectError(t, errs, "would duplicate it")
}

// ...and the same generic body instantiated over a harmless element compiles, so
// the deferral is not a blanket rejection of the body.
func TestT1926_UserContainerGenericIndirectionPlainElementOK(t *testing.T) {
	checkOK(t, `
		type MyVec[T] { T[] items; }
		make_nested[T]() {
			Vector[MyVec[T]] v = [];
		}
		test() {
			make_nested[int]();
		}
	`)
}

// --- Annotation plumbing (annotations.md §17 step 6) ---

// `duplicates_elements is an assertion, so it is `native-only: a type written in
// Promise derives the property from its fields instead.
func TestT1926_DuplicatesElementsRejectedOnUserType(t *testing.T) {
	errs := checkErrs(t, `
		type MyVec[T] `+"`duplicates_elements"+` { T[] items; }
	`)
	expectError(t, errs, "`duplicates_elements requires `native")
}

// The declaration is what is rejected, not some property of the body.
func TestT1926_DuplicatesElementsRejectedOnEmptyUserType(t *testing.T) {
	errs := checkErrs(t, `
		type Empty `+"`duplicates_elements"+` {}
	`)
	expectError(t, errs, "`duplicates_elements requires `native")
}

// An enum can never be `native, so the enum target is not registered at all.
func TestT1926_DuplicatesElementsRejectedOnEnum(t *testing.T) {
	errs := checkErrs(t, `
		enum Color `+"`duplicates_elements"+` { Red, Blue }
	`)
	expectError(t, errs, "cannot be applied to enum")
}

// Not a function target either.
func TestT1926_DuplicatesElementsRejectedOnFunction(t *testing.T) {
	errs := checkErrs(t, `
		foo() `+"`duplicates_elements"+` {}
	`)
	expectError(t, errs, "cannot be applied to function")
}

// It takes no parameters.
func TestT1926_DuplicatesElementsRejectsParameters(t *testing.T) {
	errs := checkErrs(t, `
		type Empty `+"`duplicates_elements(depth: 1)"+` {}
	`)
	expectError(t, errs, "`duplicates_elements takes no parameters")
}

// The annotation is accepted where it belongs: std's Vector, the one `native
// declaration that carries it. Everything above depends on this flag being set.
func TestT1926_VectorCarriesDuplicatesElements(t *testing.T) {
	checkOK(t, `test() {}`)
	if types.TypVector == nil {
		t.Fatal("TypVector not populated")
	}
	if !types.TypVector.DuplicatesElements() {
		t.Error("std Vector must carry `duplicates_elements — it is the base case ownsElementsByValue derives from")
	}
}

// The premise the whole derivation rests on (annotations.md §1 sanctions
// identity for exactly this: checking the compiler's own assumptions). Every
// `native universe type declares zero Promise-level fields, which is why the
// field walk stops at the behind-a-handle types without naming them, and why
// deleting isStdNativeContainerNamed's field-walk suppression was a no-op.
func TestT1926_NativeUniverseTypesHaveNoFields(t *testing.T) {
	checkOK(t, `test() {}`)
	natives := []struct {
		name string
		typ  *types.Named
	}{
		{"Vector", types.TypVector},
		{"string", types.TypString},
		{"Ref", types.TypArc},
		{"Weak", types.TypWeak},
		{"Channel", types.TypChannel},
		{"Task", types.TypTask},
		{"FailableTask", types.TypFailableTask},
		{"Mutex", types.TypMutex},
		{"MutexGuard", types.TypMutexGuard},
	}
	for _, n := range natives {
		if n.typ == nil {
			t.Errorf("%s: universe pointer not populated", n.name)
			continue
		}
		if got := len(n.typ.AllFields()); got != 0 {
			t.Errorf("%s: `native type must declare no Promise-level fields, got %d — "+
				"ownsElementsByValue and the field walks in clone.go rely on this", n.name, got)
		}
	}
}

// Map, by contrast, is an ordinary Promise type WITH fields — which is what makes
// it derivable, and made the old identity list's stated justification ("native
// types with no Promise-level fields") false for exactly the two types it was
// written to excuse. (Set is the same, reached through its Map[T, bool] field;
// it has no universe pointer left to assert on, which is the point of §6.)
func TestT1926_MapIsAnOrdinaryTypeWithFields(t *testing.T) {
	checkOK(t, `test() {}`)
	if types.TypMap == nil {
		t.Fatal("TypMap not populated")
	}
	if len(types.TypMap.AllFields()) == 0 {
		t.Error("Map must have Promise-level fields; the whole derivation depends on reaching Vector through them")
	}
}

// The buffer walk must visit each (origin, type args) pair once, not once per
// PATH that reaches it. `L0 { L1 a; L1 b; }` repeated 32 deep is 2^32 distinct
// paths and 33 distinct types: a walk that only guards the current path takes
// hours on this, and one that memoizes the instantiation takes a millisecond.
// No deadline is asserted — the package timeout is the assertion. (T1926)
func TestT1926_SharedSubtreesWalkedOncePerInstantiation(t *testing.T) {
	const depth = 32
	var b strings.Builder
	fmt.Fprintf(&b, "type L%d { int x; }\n", depth)
	for i := depth - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "type L%d { L%d a; L%d b; }\n", i, i+1, i+1)
	}
	b.WriteString("type Top[T] { L0 l; T item; }\n")
	b.WriteString("test() { Vector[Top[int]] v = []; }\n")
	checkOK(t, b.String())
}

// The same graph made CYCLIC, which is where memoizing only un-truncated answers
// is not enough: every level here is cut by `L0`, so an answer that refused to be
// reused because it was cut would put the 2^depth walk straight back. A memo
// entry records what cut it and is reused while that origin is still on the path.
// (T1926)
func TestT1926_CyclicSharedSubtreesStayLinear(t *testing.T) {
	const depth = 24
	var b strings.Builder
	fmt.Fprintf(&b, "type L%d { L0? back; }\n", depth)
	for i := depth - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "type L%d { L%d a; L%d b; }\n", i, i+1, i+1)
	}
	b.WriteString("type Top[T] { L0 l; T item; }\n")
	b.WriteString("test() { Vector[Top[int]] v = []; }\n")
	checkOK(t, b.String())
}

// The path guard is still load-bearing for termination: every level of this
// recursion is a NEW instantiation (`Rec[int]` → `Rec[Vector[int]]` → …), which
// sema accepts, so the memo alone would descend forever. (T1926)
func TestT1926_ArgGrowingRecursiveGenericTerminates(t *testing.T) {
	checkOK(t, `
		type Rec[T] { Rec[Vector[T]]? next; }
		test() { Vector[Rec[int]] v = []; }
	`)
}

// --- Buffer shapes the walk must recognise, and the type-argument occurrence
// check that decides which args a buffer actually holds (T1926) ---

// A fixed-size array is the other primitive that owns its elements by value
// (memory-model.md §2), so a user container built on one is judged exactly as a
// `T[]`-backed one is. This is the *field*-reached array, distinct from
// TestT0545_NestedFixedArrayInVectorError where the array IS the element.
func TestT1926_FixedArrayBufferInUserContainerRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Slots[T] { T[3] cells; }
		test() {
			Vector[Slots[Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// ...and the array arm is subject to the same "which arg does the buffer hold"
// question as the vector arm: an array of something ELSE leaves the type
// argument a legal direct element (T0508).
func TestT1926_UnrelatedFixedArrayDoesNotBlameTypeArg(t *testing.T) {
	checkOK(t, `
		type SlotJob[T] { int[3] cells; T handle; }
		test() {
			Vector[SlotJob[Task[int]]] v = [];
		}
	`)
}

// The type argument may sit one Optional deep inside the buffer. The occurrence
// check has to see through the Optional, or `T?[]` reads as a buffer holding
// `Task[int]?` — a type the argument is not identical to.
func TestT1926_OptionalElementInsideBufferRejected(t *testing.T) {
	errs := checkErrs(t, `
		type OptVec[T] { T?[] items; }
		test() {
			Vector[OptVec[Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// The same one Tuple deep. `(int, T)[]` duplicates the whole tuple, handle
// included, so the argument occurs in the buffer.
func TestT1926_TupleElementInsideBufferRejected(t *testing.T) {
	errs := checkErrs(t, `
		type TupVec[T] { (int, T)[] pairs; }
		test() {
			Vector[TupVec[Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// The buffer itself may be reached through a Tuple field rather than being one:
// `(int, T[])` holds a vector by value, so the field walk has to descend tuple
// elements the same way it descends Optionals.
func TestT1926_TupleFieldHoldingBufferRejected(t *testing.T) {
	errs := checkErrs(t, `
		type TupField[T] { (int, T[]) pair; }
		test() {
			Vector[TupField[Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// The derivation composes with std's own containers, which is the whole premise:
// `Wrapper` owns nothing by value on its face, but its `Map[string, T]` field
// reaches `Slot[string, T][]` and, through that, a Vector. Three hops, no type
// named anywhere.
func TestT1926_StdMapFieldReachedThroughUserTypeRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Wrapper[T] { Map[string, T] m; }
		test() {
			Vector[Wrapper[Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// The outer container need not be a Vector: a Map duplicates both K and V (both
// occur in `Slot[K, V][]`), so a user container of handles is rejected in the
// VALUE position...
func TestT1926_UserContainerAsMapValueRejected(t *testing.T) {
	errs := checkErrs(t, `
		type MyVec[T] { T[] items; }
		test() {
			Map[string, MyVec[Task[int]]] m = {:};
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// ...and in the KEY position. (The Hashable/Equal constraint errors come along
// for the ride; the nesting rule must fire regardless.)
func TestT1926_UserContainerAsMapKeyRejected(t *testing.T) {
	errs := checkErrs(t, `
		type MyVec[T] { T[] items; }
		test() {
			Map[MyVec[Task[int]], int] m = {:};
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// The filled() half of the clone()/filled() gate, on a user container. Both
// names resolve through the same derivation, so neither is Vector-specific any
// more.
func TestT1926_UserContainerFilledOfHandleRejected(t *testing.T) {
	errs := checkErrs(t, `
		type MyVec[T] { T[] items; }
		worker() int { return 1; }
		test() {
			m := MyVec[Task[int]](items: []);
			f := m.filled;
		}
	`)
	expectError(t, errs, "cannot be filled")
}

// The memo's dependency tracking, exercised end to end. `Y` is first reached
// under `X`, where the walk truncates the `X? back` edge and records Y's answer
// as resting on that truncation. `Root`'s second field reaches `Y` again with
// `X` off the path, so the recorded answer no longer applies and Y must be
// re-walked. The rule still has to fire — a memo reused out of context is a
// stale answer, and for the closure gate a stale `false is the direction that
// zeroes a live env. (T1926)
func TestT1926_MemoNotReusedOnceItsTruncationLeavesThePath(t *testing.T) {
	errs := checkErrs(t, `
		type Y1926[T] { X1926[T]? back; }
		type X1926[T] { Y1926[T]? y; T[] buf; }
		type Root1926[T] { X1926[T] x; Y1926[T] y; }
		test() {
			Vector[Root1926[Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// The same graph over a harmless element still compiles, so the re-walk is not a
// blanket rejection of the shape.
func TestT1926_MemoInvalidationShapePlainElementOK(t *testing.T) {
	checkOK(t, `
		type Y1926b[T] { X1926b[T]? back; }
		type X1926b[T] { Y1926b[T]? y; T[] buf; }
		type Root1926b[T] { X1926b[T] x; Y1926b[T] y; }
		test() {
			Vector[Root1926b[int]] v = [];
		}
	`)
}
