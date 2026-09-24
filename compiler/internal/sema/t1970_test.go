package sema

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/types"
)

// T1970 — the container-element nesting rule and its sibling dup-safety walks
// survive INHERITANCE. T1926 derived the rule from a type's fields, but
// Named.AllFields() reports a parent's fields UNSUBSTITUTED: an inherited field is
// typed in the PARENT's *TypeParam, which a substitution built from the child's
// params leaves untouched. Every walk therefore read an inherited field as
// still-generic and missed whatever the type argument put there —
// `Vector[DerV[Mutex[int]]]` was accepted and segfaulted, while the identical
// shape without inheritance was rejected. types.AllFieldTypes folds the parent
// chain's bindings into the substitution, which closes the whole family at once.

// The headline: the buffer is reached through the parent's field, and the handle
// the outer Vector would duplicate is the child's type argument.
func TestT1970_InheritedBufferNestingRejected(t *testing.T) {
	errs := checkErrs(t, `
		type BaseV[T] { T[] items; }
		type DerV[T] is BaseV[T] { int n; }
		test() {
			Vector[DerV[Mutex[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// The same over Task, the other single-owner handle. Nothing in the walk is
// specific to Mutex; this pins that the escape was the substitution, not the
// handle.
func TestT1970_InheritedBufferNestingRejectedForTask(t *testing.T) {
	errs := checkErrs(t, `
		type BaseV2[T] { T[] items; }
		type DerV2[T] is BaseV2[T] { int n; }
		test() {
			Vector[DerV2[Task[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// The child's parameter need not be SPELLED like the parent's — the binding comes
// from the ParentRef's type arguments, not from a name match. A fix that compared
// parameter names instead of resolving the ParentRef would pass the test above and
// fail this one.
func TestT1970_InheritedBufferRenamedParamRejected(t *testing.T) {
	errs := checkErrs(t, `
		type BaseV3[T] { T[] items; }
		type DerU[U] is BaseV3[U] { int n; }
		test() {
			Vector[DerU[Mutex[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// Two levels of inheritance: the buffer is the grandparent's field, so the
// bindings have to compose up the chain rather than one hop.
func TestT1970_TwoLevelInheritedBufferRejected(t *testing.T) {
	errs := checkErrs(t, `
		type BaseV4[T] { T[] items; }
		type MidV[T] is BaseV4[T] { int n; }
		type LeafV[T] is MidV[T] { int m; }
		test() {
			Vector[LeafV[Mutex[int]]] v = [];
		}
	`)
	expectError(t, errs, "cannot be a container element")
}

// The BOUNDARY between this item and T1950, pinned so the line stays visible.
//
// When the handle is bound at the PARENT REFERENCE rather than supplied as the
// child's type argument, resolving the inherited field is not enough: the rule is
// phrased as "which of the outer container's TYPE ARGUMENTS does a by-value buffer
// hold", and here the buffer holds a handle that is no argument of anything —
// `int` is the only argument, and the buffer does not hold it. Reporting it needs
// T1950's fused walk (find a by-value path, then look for a handle at its leaf),
// which is a different defect from this item's substitution gap. The inherited
// field DOES now resolve to Vector[Mutex[int]] (TestT1970_AllFieldTypes* in the
// types package pins that), so T1970's half of the work is what lands here.
//
// !! This shape SEGFAULTS at run time — measured, `fatal: segmentation fault at
// 0x0` on the duplicating read. checkOK here asserts what the compiler does today,
// NOT that the program is acceptable. It is pinned so the boundary between the two
// items cannot move unnoticed: when T1950 lands, this test must flip to
// expectError, and its failure is the reminder to do so.
func TestT1970_ParentBoundHandleStillAcceptedPendingT1950(t *testing.T) {
	checkOK(t, `
		type BaseV5[T] { T[] items; }
		type DerFixed[U] is BaseV5[Mutex[int]] { U other; }
		test() {
			Vector[DerFixed[int]] v = [];
		}
	`)
}

// The substitution has to be exact, not positional-by-accident. `E[A, B] is
// BaseV[B]` passes its SECOND parameter to the parent, so the inherited buffer
// holds B and never A. Both directions are asserted in one test because either
// alone would pass under a wrong-but-plausible mapping: a fix that bound the
// parent's params to the child's args in declaration order would reject
// E[Mutex[int], int] (a false positive on a legal move-only collection) and accept
// E[int, Mutex[int]] (the escape, still open).
func TestT1970_InheritedBufferHonoursParameterOrder(t *testing.T) {
	checkOK(t, `
		type BaseVE[T] { T[] items; }
		type E1[A, B] is BaseVE[B] { A other; }
		test() {
			Vector[E1[Mutex[int], int]] v = [];
		}
	`)

	errs := checkErrs(t, `
		type BaseVE2[T] { T[] items; }
		type E2[A, B] is BaseVE2[B] { A other; }
		test() {
			Vector[E2[int, Mutex[int]]] v = [];
		}
	`)
	expectError(t, errs, "it transitively contains Mutex[int], a single-owner handle")
}

// The T1926 `Job` invariant, inherited: the buffer holds the tags, never the
// handle, so duplicating the element duplicates strings. Resolving inherited
// fields must not widen the rule into "any type arg of any type that owns a
// buffer" — this stays a legal T0508 move-only collection.
func TestT1970_InheritedUnrelatedBufferDoesNotBlameTypeArg(t *testing.T) {
	checkOK(t, `
		type JobBase[T] { string[] tags; T handle; }
		type DerJob[T] is JobBase[T] { int id; }
		test() {
			Vector[DerJob[Task[int]]] v = [];
		}
	`)
}

// memory-model.md §3, inherited: a field held behind a handle is not held by
// value. Ref's dup is a refcount bump, so nothing is duplicated and the walk must
// still stop at Ref once the inherited field resolves to Ref[Mutex[int]].
func TestT1970_InheritedHandleFieldIsNotByValue(t *testing.T) {
	checkOK(t, `
		type RefBase[T] { Ref[T] r; }
		type DerRef[T] is RefBase[T] { int n; }
		test() {
			Vector[DerRef[Mutex[int]]] v = [];
		}
	`)
}

// A plain element through the same inherited-buffer shape is an ordinary
// container and must stay accepted — the shape itself is not what the rule
// objects to.
func TestT1970_InheritedBufferPlainElementAccepted(t *testing.T) {
	checkOK(t, `
		type BaseV6[T] { T[] items; }
		type DerV6[T] is BaseV6[T] { int n; }
		test() {
			Vector[DerV6[int]] v = [];
		}
	`)
}

// The invariant the whole fix is for: an inherited buffer gets the SAME answer as
// a declared one. A handle inside a buffer is rejected whether the buffer is the
// type's own field or its parent's — pinned as one test so the two answers cannot
// drift apart again. (Both are rejected because firstSingleOwnerHandle recurses
// Instance TypeArgs; that policy is T0545's and is not what this item changes —
// what matters here is that inheritance no longer alters it.)
func TestT1970_InheritedBufferMatchesDeclaredBuffer(t *testing.T) {
	declared := checkErrs(t, `
		type MyVecR[T] { T[] items; }
		test() {
			Vector[MyVecR[Ref[Mutex[int]]]] v = [];
		}
	`)
	expectError(t, declared, "cannot be a container element")

	inherited := checkErrs(t, `
		type BaseVR[T] { T[] items; }
		type DerVR[T] is BaseVR[T] { int n; }
		test() {
			Vector[DerVR[Ref[Mutex[int]]]] v = [];
		}
	`)
	expectError(t, inherited, "cannot be a container element")
}

// The clone gate, reached through the inherited field rather than through
// TypeArgs: cloning a container of a type that transitively owns a handle is
// rejected (language-design.md §17.2).
func TestT1970_InheritedHandleContainerNotCloneable(t *testing.T) {
	errs := checkErrs(t, `
		type HBase[T] { T m; }
		type HDer is HBase[Mutex[int]] { int n; }
		test() {
			Vector[HDer] v = [];
			w := v.clone();
		}
	`)
	expectError(t, errs, "single-owner handle")
}

// The three gates below are reached ONLY through the inherited field — the handle
// or closure is bound at the parent reference, so it is no type argument of the
// child and the TypeArgs recursion that saves the ordinary shapes never sees it.
// All three were silently accepted before this item, and the first was measured
// SEGFAULTING at run time (`fatal: segmentation fault at 0x0` — the clone
// shallow-copied the handle and both copies were dropped; not even the
// emitVectorElementCloneLoop backstop fired). They are the "latent blind spot"
// half of the item's blast-radius list, and they are latent no longer.

// firstNestedSingleOwnerHandle, Instance origin: `HDerFixed[U] is HBase[Mutex[int]]`
// has type argument `int` and an inherited `Mutex[int]` field. Cloning the
// container would duplicate the handle. (language-design.md §17.2: every context
// that would structurally copy a handle-owning value is a compile error.)
func TestT1970_InheritedFieldOnlyHandleNotCloneable(t *testing.T) {
	errs := checkErrs(t, `
		type HBaseC[T] { T m; }
		type HDerFixedC[U] is HBaseC[Mutex[int]] { U other; }
		test() {
			Vector[HDerFixedC[int]] v = [];
			w := v.clone();
		}
	`)
	expectError(t, errs, "cannot be cloned: it contains Mutex[int], a single-owner handle")
}

// The sibling call site: slicing duplicates elements exactly as clone() does, and
// consults the same predicate through a different entry point.
func TestT1970_InheritedFieldOnlyHandleNotSliceable(t *testing.T) {
	errs := checkErrs(t, `
		type HBaseS[T] { T m; }
		type HDerFixedS[U] is HBaseS[Mutex[int]] { U other; }
		test() {
			Vector[HDerFixedS[int]] v = [];
			s := v[0:1];
		}
	`)
	expectError(t, errs, "cannot be sliced: it contains Mutex[int], a single-owner handle")
}

// firstNestedClosure, Instance origin: a closure env is opaque and cannot be
// deep-copied, so a container of a type that transitively owns one is not
// cloneable. Reached only through the inherited field.
func TestT1970_InheritedFieldOnlyClosureNotCloneable(t *testing.T) {
	errs := checkErrs(t, `
		type FBaseC[T] { T f; }
		type FDerFixedC[U] is FBaseC[() -> int] { U other; }
		test() {
			Vector[FDerFixedC[int]] v = [];
			w := v.clone();
		}
	`)
	expectError(t, errs, "it contains a closure field (() -> int)")
}

// firstNestedClosure, bare *types.Named: a NON-generic child has no type params at
// all, so this branch applied no substitution whatsoever and the inherited closure
// field read as an unbound parameter.
func TestT1970_InheritedClosureNonGenericChildNotCloneable(t *testing.T) {
	errs := checkErrs(t, `
		type FBaseN[T] { T f; }
		type FDerNGC is FBaseN[() -> int] { int n; }
		test() {
			Vector[FDerNGC] v = [];
			w := v.clone();
		}
	`)
	expectError(t, errs, "it contains a closure field (() -> int)")
}

// The matching negative for all four: a closure behind a Ref is still reached by
// firstNestedClosure (it mirrors the handle predicate and recurses TypeArgs
// deliberately), but a type whose inherited field is a PLAIN value must stay
// cloneable — resolving inherited fields must not make every inheriting container
// non-cloneable.
func TestT1970_InheritedPlainFieldStaysCloneable(t *testing.T) {
	checkOK(t, `
		type PBase[T] { T v; }
		type PDerFixed[U] is PBase[int] { U other; }
		test() {
			Vector[PDerFixed[int]] v = [];
			w := v.clone();
		}
	`)
}

// The nil guards on the exported entry points of the walk family T1970 changed.
// Each is consulted from codegen and ownership on a type looked up in a map, so a
// nil reaches them whenever a type was never recorded — these guards are what turns
// that into "no handle, no closure" instead of a panic mid-compile. Mirrors the
// ContainsFailableTask(nil) case in types/t1381_test.go, which exists for the same
// reason on the same kind of predicate.
func TestT1970_WalkFamilyHandlesNilType(t *testing.T) {
	if off := FirstNestedSingleOwnerHandle(nil); off != nil {
		t.Errorf("FirstNestedSingleOwnerHandle(nil) = %s, want nil", off)
	}
	if off := FirstFieldNestedSingleOwnerHandle(nil); off != nil {
		t.Errorf("FirstFieldNestedSingleOwnerHandle(nil) = %s, want nil", off)
	}
	if sig := FirstFieldNestedClosure(nil); sig != nil {
		t.Errorf("FirstFieldNestedClosure(nil) = %s, want nil", sig)
	}
	if sig := FirstFieldNestedClosureDeep(nil); sig != nil {
		t.Errorf("FirstFieldNestedClosureDeep(nil) = %s, want nil", sig)
	}
	// firstNestedClosure has no exported entry, so its guard is asserted directly.
	if sig := firstNestedClosure(nil, nil); sig != nil {
		t.Errorf("firstNestedClosure(nil) = %s, want nil", sig)
	}
}

// collectByValueBuffers accepts only a *types.Named or *types.Enum origin — every
// caller reaches it through one, since an origin is what a declaration produces —
// and answers "no buffer" for anything else rather than walking a type that has no
// fields to walk. Asserted directly because no public surface can pass another
// origin kind. Note the primitives are themselves *types.Named, so the type used
// here has to be a structural one. (T1926/T1970)
func TestT1970_CollectByValueBuffersRejectsNonUserOrigin(t *testing.T) {
	for _, notAnOrigin := range []types.Type{
		types.NewOptional(types.TypInt),
		types.NewTuple([]types.Type{types.TypInt, types.TypString}),
		types.NewArray(types.TypInt, 2),
	} {
		var buffers []types.Type
		if collectByValueBuffers(notAnOrigin, nil, newByValueWalk(&buffers)) {
			t.Errorf("collectByValueBuffers(%s) reported a by-value buffer", notAnOrigin)
		}
		if len(buffers) != 0 {
			t.Errorf("collectByValueBuffers(%s) recorded %v, want none", notAnOrigin, buffers)
		}
	}
}
