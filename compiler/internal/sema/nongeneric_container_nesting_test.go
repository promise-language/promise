package sema

import (
	"strings"
	"testing"
)

// #404 — the container-element nesting rule covers a handle the element type
// holds in a by-value buffer through its FIELDS, not only one of its type
// arguments. T1926 phrased the rule as "which type arguments does a buffer hold",
// so a non-generic container (`TaskBag { Task[int][] items; }`, no type arguments
// at all) and a generic one whose buffer holds a concrete handle
// (`Mixed[U] { Task[int][] items; U other; }`) had nothing to blame and were
// accepted; the duplicating read then hit the runtime clone backstop. The fused
// walk (bufferedSingleOwnerHandle) asks the by-value buffers directly. The `T[]`
// and `T[N]` sugar is routed through the same rule, since it never reached
// validateSingleOwnerContainerInstance.

// The item's headline: a non-generic container whose field is a Vector of tasks.
func TestNongenericContainerNesting_TaskBagRejected(t *testing.T) {
	errs := checkErrs(t, `
		type TaskBag { Task[int][] items; }
		test() {
			Vector[TaskBag] v = [];
		}
	`)
	expectError(t, errs, "TaskBag cannot be a container element: it transitively contains Task[int], a single-owner handle")
}

// The other two single-owner handles. Nothing in the walk is specific to Task.
func TestNongenericContainerNesting_MutexAndGuardBagsRejected(t *testing.T) {
	errs := checkErrs(t, `
		type MutexBag { Mutex[int][] items; }
		test() {
			Vector[MutexBag] v = [];
		}
	`)
	expectError(t, errs, "it transitively contains Mutex[int], a single-owner handle")

	errs = checkErrs(t, `
		type GuardBag { MutexGuard[int][] items; }
		test() {
			Vector[GuardBag] v = [];
		}
	`)
	expectError(t, errs, "it transitively contains MutexGuard[int], a single-owner handle")
}

// Two hops through non-generic fields: Outer holds Inner holds the buffer.
func TestNongenericContainerNesting_TwoHopsRejected(t *testing.T) {
	errs := checkErrs(t, `
		type InnerNG { Task[int][] xs; }
		type OuterNG { InnerNG i; }
		test() {
			Vector[OuterNG] v = [];
		}
	`)
	expectError(t, errs, "OuterNG cannot be a container element")
}

// The enum arm of the walk: a non-generic enum whose variant holds the buffer.
func TestNongenericContainerNesting_EnumVariantBufferRejected(t *testing.T) {
	errs := checkErrs(t, `
		enum BagE { Empty, Items(Task[int][] xs) }
		test() {
			Vector[BagE] v = [];
		}
	`)
	expectError(t, errs, "BagE cannot be a container element")
}

// A fixed-size array is a by-value buffer too, with no annotation needed.
func TestNongenericContainerNesting_FixedArrayBufferRejected(t *testing.T) {
	errs := checkErrs(t, `
		type ArrBag { Task[int][2] items; }
		test() {
			Vector[ArrBag] v = [];
		}
	`)
	expectError(t, errs, "ArrBag cannot be a container element")
}

// A map duplicates its values (and keys) through its bucket buffer, both in an
// annotation and in a map literal.
func TestNongenericContainerNesting_MapValueRejected(t *testing.T) {
	errs := checkErrs(t, `
		type TaskBagM { Task[int][] items; }
		test() {
			map[string, TaskBagM] m = {:};
		}
	`)
	expectError(t, errs, "TaskBagM cannot be a container element")

	errs = checkErrs(t, `
		type TaskBagML { Task[int][] items; }
		test() {
			m := {"k": TaskBagML(items: [])};
		}
	`)
	expectError(t, errs, "TaskBagML cannot be a container element")
}

// A vector literal whose element type is inferred from its elements, not from an
// annotation.
func TestNongenericContainerNesting_InferredVectorLiteralRejected(t *testing.T) {
	errs := checkErrs(t, `
		type TaskBagL { Task[int][] items; }
		test() {
			xs := [TaskBagL(items: [])];
		}
	`)
	expectError(t, errs, "TaskBagL cannot be a container element")
}

// A GENERIC owner whose buffer holds a concrete handle that is none of its type
// arguments: the type-argument half reports nothing (U = int is not in the
// buffer), so only the fused walk can.
func TestNongenericContainerNesting_GenericOwnerConcreteHandleRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Mixed[U] { Task[int][] items; U other; }
		test() {
			Vector[Mixed[int]] v = [];
		}
	`)
	expectError(t, errs, "it transitively contains Task[int], a single-owner handle")
}

// Every buffer is judged, not the first one found: a handle-free buffer declared
// ahead of the one holding tasks must not settle the answer. That holds for a
// non-generic type and for a generic one whose type argument is not the handle.
func TestNongenericContainerNesting_LaterBufferRejected(t *testing.T) {
	errs := checkErrs(t, `
		type TagsThenTasks { string[] tags; Task[int][] tasks; }
		test() {
			Vector[TagsThenTasks] v = [];
		}
	`)
	expectError(t, errs, "TagsThenTasks cannot be a container element: it transitively contains Task[int]")

	errs = checkErrs(t, `
		type MixedLate[U] { U[] others; Task[int][] items; }
		test() {
			Vector[MixedLate[int]] v = [];
		}
	`)
	expectError(t, errs, "MixedLate[int] cannot be a container element: it transitively contains Task[int]")
}

// A non-generic child whose INHERITED buffer is bound to a handle at the parent
// reference. T1970 resolves the inherited field; the fused walk reports it.
func TestNongenericContainerNesting_InheritedParentBoundHandleRejected(t *testing.T) {
	errs := checkErrs(t, `
		type BaseVN[T] { T[] items; }
		type DerNG is BaseVN[Mutex[int]] { int n; }
		test() {
			Vector[DerNG] v = [];
		}
	`)
	expectError(t, errs, "DerNG cannot be a container element: it transitively contains Mutex[int]")
}

// The diagnostic names the handle in the BUFFER, not a handle the type merely
// holds in a plain field — that one is a legal direct element and not why the
// type is rejected.
func TestNongenericContainerNesting_NamesTheBufferedHandle(t *testing.T) {
	errs := checkErrs(t, `
		type MyVec3NG { Mutex[int][] xs; Task[int] other; }
		test() {
			Vector[MyVec3NG] v = [];
		}
	`)
	expectError(t, errs, "MyVec3NG cannot be a container element: it transitively contains Mutex[int]")
	for _, e := range errs {
		if strings.Contains(e.Error(), "contains Task[int]") {
			t.Errorf("diagnostic blames the plain-field handle: %v", e)
		}
	}
}

// A buffer OF Refs gets the same answer as the generic spelling
// `Vector[Vector[Ref[Task[int]]]]`, which firstSingleOwnerHandle already rejects
// by recursing into Ref's type argument. Whether that recursion is right is the
// leaf-predicate policy of #627; this pins that the non-generic spelling does not
// disagree with the generic one in the meantime.
func TestNongenericContainerNesting_BufferOfRefsMatchesGenericSpelling(t *testing.T) {
	errs := checkErrs(t, `
		type RefBag { Ref[Task[int]][] rs; }
		test() {
			Vector[RefBag] v = [];
		}
	`)
	expectError(t, errs, "RefBag cannot be a container element")
}

// The item's TITLE spelling: the `T[]` sugar builds the vector without going
// through validateSingleOwnerContainerInstance, so it has to run the rule itself.
// Every annotation position the sugar reaches — a local, a field, a parameter, a
// return type — and the `T[N]` array sugar.
func TestNongenericContainerNesting_SugarSpellingsRejected(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"local", `type TaskBagS1 { Task[int][] items; }
		test() {
			TaskBagS1[] v = [];
		}`},
		{"field", `type TaskBagS2 { Task[int][] items; }
		type OwnS2 { TaskBagS2[] bags; }`},
		{"param", `type TaskBagS3 { Task[int][] items; }
		take(TaskBagS3[] bags) int { return bags.len; }`},
		{"return", `type TaskBagS4 { Task[int][] items; }
		make() TaskBagS4[] { return []; }`},
		{"array", `type TaskBagS5 { Task[int][] items; }
		type OwnS5 { TaskBagS5[2] bags; }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := checkErrs(t, tc.src)
			expectError(t, errs, "cannot be a container element: it transitively contains Task[int]")
		})
	}
}

// The generic shapes T0545/T1926 already reject in the `Vector[...]` spelling,
// written with the sugar: the hole was never specific to non-generic containers.
func TestNongenericContainerNesting_SugarGenericShapesRejected(t *testing.T) {
	errs := checkErrs(t, `
		test() {
			Task[int][][] v = [];
		}
	`)
	expectError(t, errs, "Task[int][] cannot be a container element")

	errs = checkErrs(t, `
		type MyVecS[T] { T[] items; }
		test() {
			MyVecS[Task[int]][] v = [];
		}
	`)
	expectError(t, errs, "MyVecS[Task[int]] cannot be a container element")
}

// In a generic body the sugar takes the same T0616 call-site deferral as the
// `Vector[...]` spelling: `T[][]`, `MyVec[T][]` and `T[][2]` instantiated with a
// handle are rejected at the call, exactly as `Vector[Vector[T]]` is. Running only
// the direct half of the rule from the sugar would leave every one accepted.
func TestNongenericContainerNesting_SugarInGenericBodyDeferred(t *testing.T) {
	for _, tc := range []struct{ name, decl, call, want string }{
		{"vector-of-vectors", `wrap[T](Vector[T] v) { T[][] inner = []; }`,
			`wrap[Task[int]](v);`, "cannot instantiate generic with Task[int][]"},
		{"vector-of-user-container", `type MyVecGS[T] { T[] items; }
			wrap[T](Vector[T] v) { MyVecGS[T][] inner = []; }`,
			`wrap[Task[int]](v);`, "cannot instantiate generic with MyVecGS[Task[int]]"},
		{"array-of-vectors", `wrap[T](Vector[T] v) { T[][2] inner = [[], []]; }`,
			`wrap[Task[int]](v);`, "cannot instantiate generic with Task[int][]"},
		{"method-body", `type HolderGS[T] { T[] xs;
				mk() { T[][] inner = []; }
			}`,
			`h := HolderGS[Task[int]](xs: []); h.mk();`, "cannot instantiate generic with Task[int][]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := checkErrs(t, `
				worker() int { return 42; }
				`+tc.decl+`
				test_task() {
					v := Vector[Task[int]]();
					v.push(go worker());
					`+tc.call+`
				}
			`)
			expectError(t, errs, tc.want)
			expectError(t, errs, "nested container element")
		})
	}

	// The deferral fires on the instantiation, not the definition: a plain T is fine.
	checkOK(t, `
		wrap[T](Vector[T] v) { T[][] inner = []; T[][2] arr = [[], []]; }
		test_plain() {
			v := Vector[int]();
			wrap[int](v);
		}
	`)
}

// The sugar must not widen the rule: a DIRECT handle element is the T0508
// move-only collection, not a nesting, an Optional or Tuple around a handle is
// the T0558 carve-out, and an ordinary nested collection is fine.
func TestNongenericContainerNesting_SugarDirectElementsAccepted(t *testing.T) {
	checkOK(t, `
		type ArrDirect { Task[int][2] c; int[][2] e; }
		test() {
			Task[int][] a = [];
			Mutex[int][] b = [];
			string[][] d = [];
			Task[int]?[] f = [];
			(Task[int], int)[] g = [];
		}
	`)
}

// memory-model.md#by-value-versus-behind-a-handle with a whole BAG behind the
// handle: Ref, Weak and Channel duplicate by a refcount bump, so none of them
// copies the tasks the bag holds by value, however it is spelled. The buffer
// walk has to stop AT the handle. RefFieldAccepted below cannot show that,
// because a bare Task has no buffer even if the walk looked through the Ref.
func TestNongenericContainerNesting_BagBehindAHandleAccepted(t *testing.T) {
	checkOK(t, `
		type TaskBagBH { Task[int][] items; }
		type RefBagHolder { Ref[TaskBagBH] r; string[] names; }
		type GenHolderBH[T] { Ref[T] r; string[] names; }
		test() {
			Vector[Ref[TaskBagBH]] a = [];
			Ref[TaskBagBH][] b = [];
			Vector[Weak[TaskBagBH]] c = [];
			Vector[Channel[TaskBagBH]] d = [];
			Vector[RefBagHolder] e = [];
			Vector[GenHolderBH[TaskBagBH]] f = [];
		}
	`)
}

// The walk now also starts from a type with no type arguments, so it meets
// recursion there. A self- or mutually-recursive type terminates; it is accepted
// when no buffer on any path holds a handle, and rejected when one does, even
// when the handle is reachable only by going round the cycle.
func TestNongenericContainerNesting_RecursiveTypes(t *testing.T) {
	checkOK(t, `
		type TreeNG { TreeNG[] kids; int v; }
		type ListNG { ListNG? next; string[] tags; }
		test() {
			Vector[TreeNG] a = [];
			Vector[ListNG] b = [];
		}
	`)

	errs := checkErrs(t, `
		type RecA { RecB? b; int n; }
		type RecB { RecA? a; Task[int][] ts; }
		test() {
			Vector[RecA] v = [];
		}
	`)
	expectError(t, errs, "RecA cannot be a container element: it transitively contains Task[int]")
}

// docs/memory-model.md#by-value-versus-behind-a-handle: a handle held behind a
// Ref FIELD is not held by value. Ref's dup is a refcount bump, so the buffer
// walk stops at it and only the string buffer is found.
func TestNongenericContainerNesting_RefFieldAccepted(t *testing.T) {
	checkOK(t, `
		type Holder { Ref[Task[int]] r; string[] names; }
		test() {
			Vector[Holder] v = [];
		}
	`)
}

// A handle in a plain field is the T0508 direct element: the outer container
// moves the Box, never duplicates it, and the indexed read is what is rejected.
func TestNongenericContainerNesting_DirectHandleFieldAccepted(t *testing.T) {
	checkOK(t, `
		type Box { Task[int] handle; }
		test() {
			Vector[Box] v = [];
		}
	`)
}

// T1926's `Job` invariant without generics: the buffer holds the tags, not the
// handle, so duplicating a Job duplicates strings.
func TestNongenericContainerNesting_UnrelatedBufferAccepted(t *testing.T) {
	checkOK(t, `
		type Job { string[] tags; Task[int] handle; }
		test() {
			Vector[Job] v = [];
		}
	`)
}

// Channel is refcounted, so a buffer of channels duplicates no payload.
func TestNongenericContainerNesting_ChannelBufferAccepted(t *testing.T) {
	checkOK(t, `
		type ChBag { Channel[int][] chans; }
		test() {
			Vector[ChBag] v = [];
		}
	`)
}

// The rule is about nesting, not about the type: a TaskBag on its own is fine.
func TestNongenericContainerNesting_NoOuterContainerAccepted(t *testing.T) {
	checkOK(t, `
		type TaskBagSolo { Task[int][] items; }
		test() {
			b := TaskBagSolo(items: []);
		}
	`)
}

// Inside a generic body the buffer holds an unbound T, which the fused walk must
// not report: the definition is accepted, and the existing T0616 deferral still
// catches the instantiation that puts a handle there.
func TestNongenericContainerNesting_GenericBodyDeferred(t *testing.T) {
	checkOK(t, `
		type MyVecG[T] { T[] items; }
		wrap[T](Vector[T] v) {
			inner := Vector[MyVecG[T]]();
		}
		test_plain() {
			v := Vector[int]();
			wrap[int](v);
		}
	`)

	errs := checkErrs(t, `
		type MyVecG2[T] { T[] items; }
		worker() int { return 42; }
		wrap[T](Vector[T] v) {
			inner := Vector[MyVecG2[T]]();
		}
		test_task() {
			v := Vector[Task[int]]();
			v.push(go worker());
			wrap[Task[int]](v);
		}
	`)
	expectError(t, errs, "nested container element")
}
