package drop2

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// --- Variable tests ---

func TestDropVectorField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder {
			int[] items;
			drop(~this) {}
		}
		main() {
			h := Holder(items: [1, 2, 3]);
		}
	`)
	// Container fields get dropped via emitFieldDrops in drop() body
	codegentest.AssertContains(t, ir, "call void @Vector.drop")
	codegentest.AssertContains(t, ir, "%h.dropflag")
	codegentest.AssertContains(t, ir, "call void @Holder.drop")
}

// Standalone vector variables do NOT get scope-exit drop yet (needs ownership tracking)
// T0064: Standalone vector variables now get drop flags
func TestDropVectorStandaloneHasDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int[] my_vec = [1, 2, 3];
			int x = my_vec.len;
		}
	`)
	codegentest.AssertContains(t, ir, "%my_vec.dropflag")
	codegentest.AssertContains(t, ir, "call void @Vector.drop(")
}

// Non-droppable type: no drop flag or call generated for that variable
func TestDropNotGeneratedForNonDroppable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Simple {
			int id;
		}
		main() {
			my_simple := Simple(id: 1);
			int x = my_simple.id;
		}
	`)
	// B0164: Non-droppable heap types now get bindingFree with a drop flag for pal_free
	codegentest.AssertContains(t, ir, "%my_simple.dropflag")
	codegentest.AssertNotContains(t, ir, "Simple.drop")
	codegentest.AssertContains(t, ir, "call void @pal_free")
}

// Copy type: no drop flag even if fields exist
func TestDropNotGeneratedForCopyType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point `+"`"+`copy {
			int x;
			int y;
		}
		main() {
			p := Point(x: 1, y: 2);
			int v = p.x;
		}
	`)
	codegentest.AssertNotContains(t, ir, "%p.dropflag")
	codegentest.AssertNotContains(t, ir, "Point.drop")
}

// Droppable var in typed var decl
func TestDropTypedVarDecl(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			Resource r = Resource(id: 1);
			int x = r.id;
		}
	`)
	codegentest.AssertContains(t, ir, "call void @Resource.drop")
	codegentest.AssertContains(t, ir, "r.dropflag")
}

// Drop in a function that takes and returns a droppable:
// the parameter itself doesn't get a drop flag (it's the caller's responsibility)
func TestDropParameterNotFlagged(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		passthrough(Resource zres) int {
			return zres.id;
		}
		main() {
			int x = passthrough(Resource(id: 1));
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @__user.passthrough")
	// The callee does NOT create a drop flag for non-~ params.
	// The caller retains ownership and drops at scope exit.
	codegentest.AssertNotContains(t, ir, "zres.dropflag")
}

// Drop with use in loop triggers both close and drop at scope boundaries
func TestDropAndUseInLoop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Closeable {
			int id;
			close() { }
		}
		type Droppable {
			int id;
			drop(~this) { }
		}
		main() {
			d := Droppable(id: 1);
			int i = 0;
			while i < 3 {
				use c := Closeable(id: i);
				int x = c.id + d.id;
				i++;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "call void @Closeable.close")
	codegentest.AssertContains(t, ir, "call void @Droppable.drop")
}

// Move in function call clears flag — std call variant
func TestDropMoveToStdCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		take(Resource r) { }
		main() {
			r := Resource(id: 1);
			take(r);
		}
	`)
	codegentest.AssertContains(t, ir, "store i1 false, i1*")
	codegentest.AssertContains(t, ir, "drop.call")
	codegentest.AssertContains(t, ir, "drop.skip")
}

// Move to index assignment clears flag
func TestDropMoveToIndexAssign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			arr := [Resource(id: 0)];
			r := Resource(id: 1);
			arr[0] = r;
		}
	`)
	codegentest.AssertContains(t, ir, "store i1 false, i1*")
}

// B0195: Vector[string] index assign dups new value for independent ownership
func TestVectorStringIndexAssignDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string[] v = ["hello", "world"];
			v[0] = "replaced";
		}
	`)
	// Should dup the new value so vector owns an independent copy (like push)
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new")
}

// B0204: Vector[string] index assign drops old element before storing new value
func TestVectorStringIndexAssignDropsOld(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string[] v = ["hello", "world"];
			v[0] = "replaced";
		}
	`)
	// Should drop old string element before storing the new one
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// B0350: Map[K,string] index assign from borrow param dups value
func TestMapStringIndexAssignBorrowParamDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		store(map[string, string]~ m, string key, string value) {
			m[key] = value;
		}
		main() {
			map[string, string] m = {:};
			store(m, "k", "v");
		}
	`)
	// The borrow param 'value' should be duped before storing in map
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new")
}

// B0350: Owned local string assigned to map should NOT produce extra dup —
// clearDropFlag transfers ownership, so no dup is needed.
func TestMapStringIndexAssignOwnedNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_val() string { return "hello"; }
		store_owned(map[string, string]~ m) {
			string v = make_val();
			m["k"] = v;
		}
		main() {
			map[string, string] m = {:};
			store_owned(m);
		}
	`)
	// v is an owned local (has drop flag) — clearDropFlag transfers ownership.
	// B0350 dup must NOT fire for owned locals.
	fnIR := codegentest.ExtractFunction(ir, "store_owned")
	if strings.Contains(fnIR, "call i8* @promise_string_new") {
		t.Error("owned local string should not be duped in map index assign")
	}
}

// B0235: Map overwrite should drop old Slot enum element.
func TestMapOverwriteDropsOldSlot(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			map[string, string] m = {:};
			m["a"] = "one";
			m["a"] = "two";
		}
	`)
	// Map.[]= should call Slot.drop on old element before storing new.
	// The Slot enum has synthesized drop at mono time (Slot[string, string]).
	codegentest.AssertContains(t, ir, `call void @"Slot[string, string].drop"(`)
}

// B0204: Vector[string] index read dups when stored in variable
func TestVectorStringIndexReadDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string[] v = ["hello", "world"];
			string s = v[0];
		}
	`)
	// Should dup the string read from vector index (dup-on-read)
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new")
}

// T0383 / T0438: assigning `outer[0] = a.borrow` for non-Copy element T is
// rejected at sema (no implicit `T& → T` decay). The codegen dup-on-borrow
// path being tested here is unreachable under Option A; users must
// `.clone()` to obtain an owned independent copy.

// T0383: Vector[Vector[T]] index assign drops the old element before the
// store, preventing leak of the previously-pushed buffer.
func TestT0383VectorIndexAssignDropsOldHeapElement(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			outer := string[][]();
			inner := string[]();
			inner.push("init");
			outer.push(inner);

			v2 := string[]();
			v2.push("hello");
			outer[0] = v2;
		}
	`)
	// The drop-old path loads the old slot value and calls Vector.drop on it
	// inside the indexassign.ok block (before the new-value store).
	codegentest.AssertContains(t, ir, "call void @Vector.drop(")
}

// T0383: Vector[Vector[T]] index read dups when stored in a variable
// (mirror of B0204 for nested heap vectors). Without this, drop-on-write
// at vec[i] would create a use-after-free through the aliased local.
func TestT0383VectorIndexReadDupsHeapElement(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			outer := string[][]();
			inner := string[]();
			inner.push("init");
			outer.push(inner);
			t := outer[0];
		}
	`)
	// Dup-on-read emits a vecdup.copy block via dupVector for the read path.
	codegentest.AssertContains(t, ir, "vecdup.copy")
}

// T0388: push(h.containerField) where h is a droppable owned type must dup the
// field so that both h.drop and v's element walk own independent copies.
// genVectorMethodCall detects the MemberExpr arg and sets dupContainerFieldAccess;
// genFieldAccess then dups the vector when the owner has HasDrop() true.
func TestT0388PushVectorFieldFromDroppableOwnerDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Container {
			int[] data;
			drop(~this) {}
		}
		test() {
			c := Container(data: [1, 2, 3]);
			v := int[][]();
			v.push(c.data);
		}
	`)
	// genFieldAccess must emit vecdup.copy so v's element and Container.data are
	// independent — otherwise Container.drop and v's Vector.drop double-free the buffer.
	codegentest.AssertContains(t, ir, "vecdup.copy")
}

// T0398: `b := v[0]` where v is Vector[heap-user-type-with-drop] must deep-clone
// the element via cloneHeapElement so b holds an independent instance.
// Without the dup, b's drop binding and v's element walk double-free the same pointer.
// genInferredVarDecl sets dupHeapUserFieldAccess; genVectorIndex calls cloneHeapElement
// which falls back to dupHeapValue (pal_alloc + memcpy) when there is no clone method.
func TestT0398VectorHeapElementReadDupsOnVarDecl(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Item { int n; drop(~this) {} }
		test() {
			v := Item[]();
			v.push(Item(n: 1));
			b := v[0];
		}
	`)
	// cloneHeapElement → dupHeapValue: allocate a new instance and memcpy the data.
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc")
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy")
}

// T0898: `b := v[0]` where v is Vector[no-drop heap user type] must dup-on-read.
// These types lack drop()/synth-drop (so isDroppableHeapUserType excludes them)
// and lack clone(), so genVectorIndex dups via dupHeapValue (alloc+memcpy →
// heapdup.copy block), not cloneHeapElement. Without this the new drop-on-
// overwrite in genVectorIndexAssign would free a slot still aliased by b.
func TestT0898VectorNoDropHeapElementReadDupsOnVarDecl(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Bare { int n; dup() Bare { return this; } }
		probe() {
			v := Bare[]();
			v.push(Bare(n: 1));
			b := v[0];
		}
		main() { probe(); }
	`)
	// dupHeapValue emits a heapdup.copy block (alloc + memcpy). Scope to @probe:
	// the stdAll clone funcs also emit heapdup.copy.
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.probe"), "heapdup.copy")
}

// T0898: `v[i] = X` where v is Vector[no-drop heap user type] must drop the old
// element before storing. emitVariantFieldDrop's B0218 branch null-checks +
// pal_frees the old instance (varfield.free block). Without this the overwrite
// leaks the previous element.
func TestT0898VectorNoDropHeapElementOverwriteDrops(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Bare { int n; dup() Bare { return this; } }
		probe() {
			v := Bare[]();
			v.push(Bare(n: 1));
			v[0] = Bare(n: 2);
		}
		main() { probe(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.probe")
	codegentest.AssertContains(t, fn, "varfield.free")
	codegentest.AssertContains(t, fn, "call void @pal_free")
}

// T0898: `h.f = X` where f is a no-drop heap user-type field must drop the old
// field value before storing. The T0410/T0908 droppable-field branch (broadened
// to admit isHeapUserNoDropPalFree) emits the null + same-pointer guard
// (field.userdrop block) followed by emitVariantFieldDrop's B0218 pal_free.
// Without this the overwrite leaks the field's previous instance.
func TestT0898MemberNoDropHeapFieldOverwriteDrops(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Bare { int n; dup() Bare { return this; } }
		type Holder { Bare f; }
		probe() {
			h := Holder(f: Bare(n: 1));
			h.f = Bare(n: 2);
		}
		main() { probe(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.probe")
	codegentest.AssertContains(t, fn, "field.userdrop")
	codegentest.AssertContains(t, fn, "call void @pal_free")
}

// T0403: `f(v[0])` where v is Vector[heap-user-type-with-drop] and f takes a `~T`
// param must deep-clone the element via cloneHeapElement so the callee receives an
// independent instance. Without the dup, the callee's `~T` drop and v's element
// walk double-free the same pointer. maybeEnableDupForMutRefArg sets
// dupHeapUserFieldAccess for IndexExpr against Vector[heap-user-type] passed to ~T;
// genVectorIndex's existing consume-branch then clones via cloneHeapElement.
// Sibling of T0398 (var-decl-site).
func TestT0403VectorHeapElementCallArgDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Item { int n; drop(~this) {} }
		take(Item move b) {}
		test() {
			v := Item[]();
			v.push(Item(n: 1));
			take(v[0]);
		}
	`)
	// cloneHeapElement → dupHeapValue: allocate a new instance and memcpy the data.
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc")
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy")
}

// T0412: vec[i] = (...) for Vector[(droppable, ...)] must drop the old tuple
// element via emitVariantFieldDrop before storing the new value. Without this,
// the previous tuple's heap fields (string instance) leak.
func TestT0412VectorIndexAssignDropsOldTuple(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			outer := (string, int)[]();
			outer.push(("a" + "", 1));
			outer[0] = ("b" + "", 2);
		}
	`)
	// emitVariantFieldDrop's tuple branch walks fields via ExtractValue and
	// calls promise_string_drop on the string element. The drop must appear
	// inside the indexassign.ok block (not just at scope exit).
	codegentest.AssertContains(t, ir, "indexassign.ok")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0412: vec[i] = vec[j] for Vector[(droppable, ...)] must dup the RHS read
// via dupTupleValue so the new slot holds an independent clone. Without this,
// Part 1's drop-old would free heap fields still aliased by another slot.
func TestT0412VectorIndexAssignDupsTupleOnVecToVec(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			outer := (string, int)[]();
			outer.push(("a" + "", 1));
			outer.push(("b" + "", 2));
			outer[0] = outer[1];
		}
	`)
	// dupTupleValue emits promise_string_new to clone the string element on read.
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new")
}

// T0489: c.tup_field = (...) for a droppable tuple field must drop the old
// field's heap contents via emitVariantFieldDrop before storing the new value.
// Without this, the previous tuple's string instance leaks.
func TestT0489MemberAssignDropsOldTuple(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0489C { (string, int) f; drop(~this) {} }
		test() {
			c := T0489C(f: ("a" + "", 1));
			c.f = ("b" + "", 2);
		}
	`)
	// emitVariantFieldDrop's tuple branch walks fields via ExtractValue and
	// calls promise_string_drop on the string element. Drop must appear in the
	// function body (not just at scope exit T0489C.drop).
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0489: c.tup_field = vec[i] for a droppable tuple field must dup the RHS
// read via dupTupleValue before storing. Without this, the field and vec[i]
// alias the same heap contents, causing a silent double-free at scope exit.
func TestT0489MemberAssignDupsTupleOnVecToField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0489D { (string, int) f; drop(~this) {} }
		test() {
			v := (string, int)[]();
			v.push(("a" + "", 1));
			c := T0489D(f: ("first" + "", 1));
			c.f = v[0];
		}
	`)
	// dupTupleValue emits promise_string_new to clone the string element on read.
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new")
}

// Reassignment of droppable variable emits drop on old value
func TestDropOnReassignment(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		test() {
			r := Resource(id: 1);
			r = Resource(id: 2);
		}
		main() {}
	`)
	// Should have drop.call and drop.skip blocks for the reassignment drop
	codegentest.AssertContains(t, ir, "drop.call")
	codegentest.AssertContains(t, ir, "drop.skip")
	// Should reset drop flag after reassignment
	codegentest.AssertContains(t, ir, "store i1 true")
}

// Move-then-reassign: drop flag was cleared by move, so reassignment skips drop
func TestDropOnReassignmentAfterMove(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		consume(Resource r) {}
		test() {
			r := Resource(id: 1);
			consume(r);
			r = Resource(id: 2);
		}
		main() {}
	`)
	// The drop-before-reassign still emits condBr (checks flag), but flag is cleared
	codegentest.AssertContains(t, ir, "drop.call")
	codegentest.AssertContains(t, ir, "drop.skip")
}

// Multiple reassignments: each reassignment should drop the old value
func TestDropOnMultipleReassignments(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		test() {
			r := Resource(id: 1);
			r = Resource(id: 2);
			r = Resource(id: 3);
		}
		main() {}
	`)
	// At least two drop.call blocks (one per reassignment)
	codegentest.AssertContains(t, ir, "drop.call")
	codegentest.AssertContains(t, ir, "drop.skip")
	codegentest.AssertContains(t, ir, "store i1 true")
}

// Self-assignment should be a no-op (no drop emitted, no store)
func TestDropOnSelfAssignmentSkipped(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		test() {
			r := Resource(id: 1);
			r = r;
		}
		main() {}
	`)
	// The self-assignment is skipped entirely via return.
	// Scope exit should still emit ONE drop for r, so drop.call should exist.
	codegentest.AssertContains(t, ir, "drop.call")
}

// Compound assignment should NOT trigger drop-before-store
func TestDropCompoundAssignNoExtraDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int x = 10;
			x += 5;
		}
	`)
	// No drop blocks for primitive int variable x
	codegentest.AssertNotContains(t, ir, "%x.dropflag")
}

// Non-droppable type reassignment should not emit drop
func TestDropOnReassignmentNonDroppable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Simple { int x; }
		test() {
			my_simple := Simple(x: 1);
			my_simple = Simple(x: 2);
		}
		main() {}
	`)
	// B0164: Non-droppable heap types now get bindingFree with a drop flag for pal_free.
	// On reassignment, the old value is freed before storing the new one.
	codegentest.AssertContains(t, ir, "%my_simple.dropflag")
	codegentest.AssertNotContains(t, ir, "Simple.drop")
	codegentest.AssertContains(t, ir, "call void @pal_free")
}

// Reassignment inside if block: drop still emitted
func TestDropOnReassignmentInIfBlock(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		test() {
			r := Resource(id: 1);
			if true {
				r = Resource(id: 2);
			}
		}
		main() {}
	`)
	codegentest.AssertContains(t, ir, "drop.call")
	codegentest.AssertContains(t, ir, "drop.skip")
}

// Reassignment inside loop: drop per iteration
func TestDropOnReassignmentInLoop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		test() {
			r := Resource(id: 0);
			for int i = 0; i < 3; i++ {
				r = Resource(id: i);
			}
		}
		main() {}
	`)
	codegentest.AssertContains(t, ir, "drop.call")
	codegentest.AssertContains(t, ir, "drop.skip")
	codegentest.AssertContains(t, ir, "store i1 true")
}

// Drop flag reset is i1 true (not i64 or other)
func TestDropOnReassignmentFlagResetIsI1True(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type R {
			int v;
			drop(~this) {}
		}
		test() {
			r := R(v: 1);
			r = R(v: 2);
		}
		main() {}
	`)
	// After emitDropCall, the flag is reset to i1 true
	codegentest.AssertContains(t, ir, "store i1 true")
}

// Reassignment when RHS is a moved variable clears RHS drop flag
func TestDropOnReassignmentRHSMoveClears(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type R {
			int v;
			drop(~this) {}
		}
		test() {
			a := R(v: 1);
			b := R(v: 2);
			a = b;
		}
		main() {}
	`)
	// Drop old a, store b into a, clear b's drop flag
	codegentest.AssertContains(t, ir, "drop.call")
	codegentest.AssertContains(t, ir, "store i1 true")
	codegentest.AssertContains(t, ir, "store i1 false")
}

// B0158: Type with droppable field auto-gets a synthesized drop
func TestDropSynthesizedBasic(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		main() {
			o := Outer(inner: Inner(id: 1));
		}
	`)
	// Outer gets a synthesized drop that calls Inner.drop on its field + pal_free
	codegentest.AssertContains(t, ir, "call void @Outer.drop")
	codegentest.AssertContains(t, ir, "o.dropflag")
	codegentest.AssertContains(t, ir, "call void @Inner.drop") // emitFieldDrops cascades
	codegentest.AssertContains(t, ir, "call void @pal_free(")  // frees Outer instance
}

// B0158: Cascading synthesized drop — Outer contains Middle contains Inner
func TestDropSynthesizedCascading(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Middle {
			Inner inner;
		}
		type Outer {
			Middle mid;
		}
		main() {
			o := Outer(mid: Middle(inner: Inner(id: 1)));
		}
	`)
	// All types in the chain get synthesized drops
	codegentest.AssertContains(t, ir, "call void @Outer.drop")
	codegentest.AssertContains(t, ir, "define void @Middle.drop")
}

// B0158: Synthesized drop with multiple droppable fields
func TestDropSynthesizedMultipleFields(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int fd;
			drop(~this) { }
		}
		type Pair {
			Resource a;
			Resource b;
		}
		main() {
			p := Pair(a: Resource(fd: 1), b: Resource(fd: 2));
		}
	`)
	// Pair gets a synthesized drop function
	codegentest.AssertContains(t, ir, "call void @Pair.drop")
	codegentest.AssertContains(t, ir, "define void @Pair.drop")
}

// B0158: Type with mix of droppable and non-droppable fields
func TestDropSynthesizedMixedFields(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Mixed {
			int x;
			Inner inner;
			bool flag;
		}
		main() {
			m := Mixed(x: 1, inner: Inner(id: 2), flag: true);
		}
	`)
	codegentest.AssertContains(t, ir, "call void @Mixed.drop")
	codegentest.AssertContains(t, ir, "define void @Mixed.drop")
}

// B0158: Copy type is not auto-synthesized even with droppable-looking fields
func TestDropSynthesizedNotForCopy(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Simple `+"`copy"+` {
			int x;
		}
		main() {
			my_copy := Simple(x: 1);
		}
	`)
	codegentest.AssertNotContains(t, ir, "Simple.drop")
	codegentest.AssertNotContains(t, ir, "my_copy.dropflag")
}

// B0158: No synthesized drop when no fields have drop
func TestDropSynthesizedNotNeeded(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Plain {
			int x;
			bool y;
		}
		main() {
			my_plain := Plain(x: 1, y: true);
		}
	`)
	codegentest.AssertNotContains(t, ir, "Plain.drop")
	// B0164: Plain types now get a bindingFree with a drop flag for pal_free at scope exit.
	// No synthesized drop method, just pal_free for the heap instance.
	codegentest.AssertContains(t, ir, "my_plain.dropflag")
	codegentest.AssertContains(t, ir, "call void @pal_free")
}

// T0095: Synthesized drop drops string fields via promise_string_drop
func TestDropSynthesizedStringField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder {
			string name;
		}
		main() {
			h := Holder(name: "hello");
		}
	`)
	// Synthesized drop should call promise_string_drop on the string field
	codegentest.AssertContains(t, ir, "define void @Holder.drop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
	codegentest.AssertContains(t, ir, "call void @pal_free(")
}

// B0217: Type with multiple function fields — both env pointers freed
func TestDropSynthesizedMultipleFuncFields(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Transform {
			(int) -> int forward;
			(int) -> int backward;
		}
		main() {
			t := Transform(forward: |int x| -> x * 2, backward: |int x| -> x / 2);
		}
	`)
	codegentest.AssertContains(t, ir, "define void @Transform.drop")
	codegentest.AssertContains(t, ir, "funcfield.env.free")
}

// B0216: String field reassignment drops old value before storing new.
func TestStringFieldReassignDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box {
			string val;
		}
		main() {
			b := Box(val: "hello");
			b.val = "world";
		}
	`)
	// Field reassignment should emit old-value drop before store
	codegentest.AssertContains(t, ir, "field.strdrop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
}

// T0095: String field access on droppable type creates a dup (via promise_string_new)
func TestStringFieldAccessDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Named {
			string name;
		}
		test() {
			n := Named(name: "world");
			string x = n.name;
		}
	`)
	// Reading n.name should dup the string to prevent double-free
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
}

// B0220: NeedsSynthDrop types (no explicit drop) also get string field dups.
// HasDrop() is true for NeedsSynthDrop types (set together in sema), so the
// T0095 dup logic covers both explicit-drop and synthesized-drop types.
func TestStringFieldAccessDupNeedsSynthDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder {
			string value;
		}
		test() {
			h := Holder(value: "original");
			string saved = h.value;
		}
	`)
	// Holder has NeedsSynthDrop (string field, no explicit drop).
	// Reading h.value should still dup to prevent use-after-free on reassign.
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
}

// B0219: Vector field reassignment drops old value before storing new.
func TestVectorFieldReassignDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Container {
			int[] items;
		}
		main() {
			c := Container(items: []);
			c.items = [];
		}
	`)
	// Field reassignment should emit old-value drop before store
	codegentest.AssertContains(t, ir, "field.vecdrop")
	codegentest.AssertContains(t, ir, "call void @Vector.drop(")
}

// B0219: Vector field read from droppable type creates a dup (via vecdup).
func TestVectorFieldAccessDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder {
			int[] data;
		}
		test() {
			h := Holder(data: []);
			int[] x = h.data;
		}
	`)
	// Reading h.data should dup the vector to prevent double-free
	codegentest.AssertContains(t, ir, "vecdup.copy")
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")
}

// T0095: Constructor with borrowed string param (no drop flag) dups the string
func TestConstructorDupBorrowedString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper {
			string data;
		}
		wrap(string s) Wrapper {
			return Wrapper(data: s);
		}
		main() { }
	`)
	// Inside wrap(), s has no drop flag (non-~ param), so constructor should dup
	codegentest.AssertContains(t, ir, "define { i8*, i8* } @__user.wrap(i8* %s)")
	// The wrap function body should contain a dup call (promise_string_new)
	// because s is a borrowed param without a drop flag
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
}

// B0179: Shared borrow of string field must NOT dup the string.
// The borrow doesn't own the value — duping creates a temp that gets freed
// while the borrow still points to it (use-after-free / double-free).
func TestStringBorrowFieldNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pair {
			string a;
			string b;
		}
		get_ref(string s) string & {
			return s;
		}
		test() {
			p := Pair(a: "hello", b: "world");
			string & ra = get_ref(p.a);
		}
	`)
	// The test function should NOT contain a string dup — the param is a borrow.
	testFn := codegentest.ExtractFunction(ir, "__user.test")
	codegentest.AssertNotContains(t, testFn, "call i8* @promise_string_new(")
}

// B0164: bindingFree emits pal_free on non-droppable heap types with multiple fields
func TestBindingFreeMultipleFields(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Config {
			int port;
			bool verbose;
		}
		main() {
			c := Config(port: 8080, verbose: true);
			int p = c.port;
		}
	`)
	codegentest.AssertContains(t, ir, "c.dropflag")
	codegentest.AssertContains(t, ir, "call void @pal_free")
	codegentest.AssertNotContains(t, ir, "Config.drop")
}

// B0164: bindingFree works on reassignment — frees old value before storing new
func TestBindingFreeReassignment(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pair { int x; int y; }
		test() {
			p := Pair(x: 1, y: 2);
			p = Pair(x: 3, y: 4);
		}
		main() {}
	`)
	codegentest.AssertContains(t, ir, "p.dropflag")
	// Should have two pal_alloc (one per constructor) and free.call blocks
	codegentest.AssertContains(t, ir, "free.call")
	codegentest.AssertContains(t, ir, "call void @pal_free")
}

// B0199: Constructor call sites keep caller's drop flag for string-typed borrow
// parameters on types with HasDrop(). The constructor body strdups the string
// (genAssignment detects no drop flag on the param), so the caller must keep
// its drop flag to free the original string.
func TestConstructorBorrowParamKeepsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder {
			string data;
			new(~this, string s) {
				this.data = s;
			}
		}
		test() {
			string mystr_b0199 = "hello";
			h := Holder(s: mystr_b0199);
		}
		main() {}
	`)
	// mystr_b0199's drop flag should NOT be cleared (new() borrows, not moves)
	codegentest.AssertNotContains(t, ir, "store i1 false, i1* %mystr_b0199.dropflag")
}

// T0073: Primitive to_string temp is dropped at statement end when not assigned
func TestStringTempDropAtStatementEnd(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			assert(42.to_string() == "42", "ok");
		}
		main() {}
	`)
	// Should have temp drop blocks from cleanupStmtTemps
	codegentest.AssertContains(t, ir, "tmp.drop")
	codegentest.AssertContains(t, ir, "tmp.skip")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0073: Primitive to_string temp is claimed when assigned to a variable
func TestStringTempClaimedOnAssign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			s := 42.to_string();
			assert(s == "42", "ok");
		}
		main() {}
	`)
	// The temp is tracked then claimed — the variable's drop binding handles cleanup.
	// The temp cleanup blocks should still exist but the flag should be cleared.
	codegentest.AssertContains(t, ir, "s.dropflag")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// B0172: Temp flag reset after drop — prevents double-free in loops with match arms
func TestStringTempFlagResetInLoop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Builder {
			string result;
			add(~this, string s) { this.result = this.result + s; }
			process(~this, string s) {
				int i = 0;
				while i < s.len {
					char c = s[i];
					match c {
						'_' => { this.add("-"); },
						_ => { this.add(c.to_string()); },
					}
					i = i + 1;
				}
			}
		}
		main() {}
	`)
	// The cleanup code must reset the drop flag to 0 after dropping to prevent
	// double-free when a different match arm executes on the next loop iteration.
	codegentest.AssertContains(t, ir, "tmp.drop")
	codegentest.AssertContains(t, ir, "store i1 false")
}

// B0172: Temp tracking enabled in defineMethodFunc
func TestStringTempTrackingInMethodBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Fmt {
			format(this) string `+"`structural(protocol: false)"+` { return this.to_string(); }
			to_string(this) string { return "fmt"; }
		}
		main() {}
	`)
	// Method bodies should have temp tracking enabled
	codegentest.AssertContains(t, ir, "tmp.drop")
}

// B0168: String concat temp is tracked and dropped at statement end
func TestStringConcatTempDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			string name = "world";
			assert("hello " + name == "hello world", "ok");
		}
		main() {}
	`)
	// Concat result should be tracked as a temp and dropped
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat")
	codegentest.AssertContains(t, ir, "tmp.drop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// B0168: String concat temp claimed when assigned to variable
func TestStringConcatTempClaimedOnAssign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			string name = "world";
			string greeting = "hello " + name;
			assert(greeting == "hello world", "ok");
		}
		main() {}
	`)
	// Concat result is claimed (assigned to greeting), variable drop binding handles it
	codegentest.AssertContains(t, ir, "greeting.dropflag")
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat")
}

// B0168: String concat temp in constructor arg is claimed (no double-free)
func TestStringConcatTempInConstructor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Greeter { string msg; }
		test() {
			g := Greeter(msg: "hello " + "world");
		}
		main() {}
	`)
	// Should compile and run without double-free; concat temp is claimed
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat")
}

// B0170: String temp pushed into vector is claimed (no double-free at stmt end)
func TestStringTempClaimedOnVectorPush(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string[] v = string[]();
			v.push("a" + "b");
		}
	`)
	// Concat result should be tracked then claimed by push.
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat")
	codegentest.AssertContains(t, ir, "call i8* @promise_vector_push")
}

// T0099: to_string() on user type is tracked as a string temp and freed at stmt end.
func TestStringTempUserTypeToString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter {
			int n;
			to_string() string { return "count"; }
		}
		test() {
			Counter c = Counter(n: 1);
			assert(c.to_string() == "count", "ok");
		}
		main() {}
	`)
	// c.to_string() produces a temp that's freed at statement end
	codegentest.AssertContains(t, ir, "call i8* @Counter.to_string")
	codegentest.AssertContains(t, ir, "tmp.drop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0099: to_string() on string returns `this` (borrow) — NOT tracked as temp.
func TestStringTempStringToStringNotTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			string s = "hello";
			assert(s.to_string() == "hello", "ok");
		}
		main() {}
	`)
	// string.to_string() returns `this` — must NOT be tracked (would double-free).
	// The test function has only one string variable `s` — its drop handles cleanup.
	// Verification: s has a drop flag (the variable's own scope cleanup).
	codegentest.AssertContains(t, ir, "s.dropflag")
}

// T0099: to_string() on user type assigned to variable is claimed (not freed as temp).
func TestStringTempUserTypeToStringClaimed(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Tag {
			int id;
			to_string() string { return "tag"; }
		}
		test() {
			Tag t = Tag(id: 1);
			string s = t.to_string();
			assert(s == "tag", "ok");
		}
		main() {}
	`)
	// t.to_string() produces a temp that's tracked then claimed on assignment to s.
	// s has its own drop binding for cleanup.
	codegentest.AssertContains(t, ir, "s.dropflag")
	codegentest.AssertContains(t, ir, "call i8* @Tag.to_string")
}

// T0133: String slice expressions are tracked as temps and freed at statement end.
func TestStringSliceTempDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			string s = "hello world";
			assert(s[0:5] == "hello", "ok");
		}
		main() {}
	`)
	// s[0:5] produces a heap-allocated string via native slice (promise_string_new).
	// The slice result must be tracked as a temp and freed at statement end.
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new")
	codegentest.AssertContains(t, ir, "tmp.drop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0133: String slice assigned to variable is claimed (not double-freed).
func TestStringSliceTempClaimedOnAssign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			string s = "hello world";
			string sub = s[0:5];
			assert(sub == "hello", "ok");
		}
		main() {}
	`)
	// s[0:5] is tracked as temp, then claimed when assigned to sub.
	// sub has its own drop binding for scope cleanup.
	codegentest.AssertContains(t, ir, "sub.dropflag")
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new")
}

// T0124: Free function call returning string is tracked as temp and freed at stmt end.
func TestStringTempFreeFunctionCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_greeting(string name) string {
			return "hello " + name;
		}
		test() {
			assert(make_greeting("world") == "hello world", "ok");
		}
		main() {}
	`)
	// The return value of make_greeting() should be tracked and freed
	codegentest.AssertContains(t, ir, "call i8* @__user.make_greeting")
	codegentest.AssertContains(t, ir, "tmp.drop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0124: Free function call returning string assigned to variable is claimed (not double-freed).
func TestStringTempFreeFunctionCallClaimed(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_label(int n) string {
			return n.to_string();
		}
		test() {
			s := make_label(42);
			assert(s == "42", "ok");
		}
		main() {}
	`)
	// The call result should be tracked but claimed on assignment
	codegentest.AssertContains(t, ir, "call i8* @__user.make_label")
	// Drop flag is cleared (claimed) so no free at stmt end for this temp
	codegentest.AssertContains(t, ir, "store i1 false")
}

// B0198: String temps in if-condition must be cleaned up in the merge block.
// When the condition evaluates to false, the then-body never runs but its
// inner-statement cleanup cleared the Go tracking. The merge block must still
// emit flag-guarded cleanup IR.
func TestStringTempIfConditionFalsePath(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		check(string s) bool {
			if s.len >= 4 && s[0:4] == "true" {
				return true;
			}
			return false;
		}
		main() { check("no"); }
	`)
	// The merge block (if.end) must contain cleanup for the condition temp:
	// load drop flag → branch to tmp.drop or tmp.skip
	codegentest.AssertContains(t, ir, "tmp.drop")
	codegentest.AssertContains(t, ir, "tmp.skip")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0082: Structural views are tested at the Promise level (e2e/structural_view_test.pr)
// because structural interface coercion requires the full std library.
// The fix: genTypedVarDecl skips clearDropFlag when LHS is a structural interface.

// B0167: Type with string field gets synthesized drop (cascading instance cleanup)
func TestSynthDropStringFieldCascade(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Inner { string name; }
		type Outer { Inner inner; int x; }
		main() {
			o := Outer(inner: Inner(name: "hi"), x: 1);
		}
	`)
	// Both types get synthesized drops
	codegentest.AssertContains(t, ir, "define void @Inner.drop")
	codegentest.AssertContains(t, ir, "define void @Outer.drop")
	// Outer.drop calls Inner.drop (cascading) + pal_free
	codegentest.AssertContains(t, ir, "call void @Inner.drop")
	codegentest.AssertContains(t, ir, "call void @pal_free")
	// String fields are NOT freed by the synthesized drop (no promise_string_drop in drop body)
}

// B0167: Type with vector field gets synthesized drop
func TestSynthDropVectorField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Container { int[] items; }
		main() {
			int[] v = int[]();
			c := Container(items: v);
		}
	`)
	codegentest.AssertContains(t, ir, "define void @Container.drop")
	codegentest.AssertContains(t, ir, "call void @pal_free")
}

// B0192: Non-droppable heap user type fields inside synthesized drop get pal_free
func TestSynthDropFreesNonDroppableHeapField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; }
		type Wrapper { Point p; string name; }
		main() {
			w := Wrapper(p: Point(x: 1, y: 2), name: "test");
		}
	`)
	wrapperDrop := codegentest.ExtractFunction(ir, "Wrapper.drop")
	if wrapperDrop == "" {
		t.Fatal("expected Wrapper.drop function in IR")
	}
	// Synthesized drop should free the Point instance via pal_free
	codegentest.AssertContains(t, wrapperDrop, "call void @pal_free(")
	// And drop the string field
	codegentest.AssertContains(t, wrapperDrop, "call void @promise_string_drop(")
}

// B0264: Vector[(string, int)] must drop string elements inside tuples.
func TestVectorTupleElementStringDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			(string, int)[] v = [("hello", 1), ("world", 2)];
		}
	`)
	// The vector drop loop should extract tuple field 0 (string) and call promise_string_drop
	codegentest.AssertContains(t, ir, "vecdrop.body")
	codegentest.AssertContains(t, ir, "extractvalue")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0371: genTupleLit claims the heap-tracked string concat temp so the tuple
// is the unique owner of the concat result. Without this claim, the stmt-temp
// cleanup would free the string while the tuple still references it (UAF).
func TestT0371TupleLitClaimsHeapStringTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			(int, string) t = (1, "a" + "b");
		}
	`)
	// Concat result is tracked as a stmtTemp; genTupleLit clears its drop flag.
	codegentest.AssertContains(t, ir, "promise_string_concat")
	codegentest.AssertContains(t, ir, "store i1 false")
	// And the tuple variable t gets a tuple-walk drop binding at scope exit.
	codegentest.AssertContains(t, ir, "tupdrop.exec")
	codegentest.AssertContains(t, ir, "tupdrop.skip")
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

// T0371: genTupleLit claims the heap-tracked user type temp so the tuple is
// the unique owner. Without this claim, the heap temp would be freed at stmt
// end and dropped again via the tuple's scope-exit walk (double-free).
func TestT0371TupleLitClaimsHeapBoxTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box { int n; }
		main() {
			(int, Box) t = (1, Box(n: 5));
		}
	`)
	// Tuple variable t gets a tuple-walk drop binding.
	codegentest.AssertContains(t, ir, "tupdrop.exec")
	// The walk frees the heap Box via emitVariantFieldDrop's pal_free branch.
	codegentest.AssertContains(t, ir, "varfield.free")
	codegentest.AssertContains(t, ir, "@pal_free")
}

// T0371: A tuple variable with droppable fields registers a bindingDropTuple
// that walks the fields and drops each droppable one at scope exit. The drop
// flag is checked first so moves can suppress the walk.
func TestT0371TupleVarDropsFields(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "a" + "b";
			(int, string) t = (1, s);
		}
	`)
	// Tuple var has a drop flag and a tupdrop block at scope exit.
	codegentest.AssertContains(t, ir, "t.dropflag")
	codegentest.AssertContains(t, ir, "tupdrop.exec")
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

// T0481: `(_, n) := t` with an owned tuple source must register a drop binding
// for the discarded slot under a synthetic key. Without it, the source's drop
// flag is cleared (transfer to LHS) but the heap field at `_` is orphaned.
func TestT0481DiscardRegistersDropString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			t := ("a" + "b", 42);
			(_, n) := t;
		}
	`)
	// The synthetic discard binding must produce a drop flag and a string drop.
	codegentest.AssertContains(t, ir, "_destructure.discard")
	codegentest.AssertContains(t, ir, "_destructure.discard.dropflag")
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

// T0481: Borrow-source destructure with `_` must NOT register a drop binding
// for the discarded slot — borrowed elements are owned by the container, and
// adding a drop would double-free with the container's element walk.
func TestT0481DiscardBorrowSourceNoDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		sum_seconds((string, int)[] v) int {
			int total = 0;
			for tup in v {
				(_, n) := tup;
				total = total + n;
			}
			return total;
		}
		main() {
			int n = sum_seconds([("a" + "b", 5)]);
		}
	`)
	// No drop binding for the borrowed `_` slot inside the for-in loop.
	codegentest.AssertNotContains(t, ir, "_destructure.discard.dropflag")
}

// B0158: Synthesized drop coexists with explicit drop (explicit takes precedence)
func TestDropExplicitTakesPrecedence(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
			drop(~this) { }
		}
		main() {
			o := Outer(inner: Inner(id: 1));
		}
	`)
	codegentest.AssertContains(t, ir, "call void @Outer.drop")
	codegentest.AssertContains(t, ir, "call void @Inner.drop")
	// Explicit drop should NOT have pal_free auto-appended (that's a separate concern)
}

// T0357: local-var string compound must drop the old value before storing
// the new concat result, with a same-pointer guard mirroring OpAssign.
func TestCompoundAssignStringDropsOld(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string a = "hello " + "first ";
			a += "world";
		}
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat(")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
	codegentest.AssertContains(t, ir, "compound.diff")
	codegentest.AssertContains(t, ir, "compound.merge")
}

// T0405: Vector field reassignment must drop elements before freeing the buffer.
// Verifies that genMemberAssign emits a vector element drop loop (string drop)
// before calling Vector.drop for a string[] field.
func TestFieldAssignVecDropsElements(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder { string[] field; }
		main() {
			v1 := string[]();
			h := Holder(v1);
			v2 := string[]();
			h.field = v2;
		}
	`)
	// The field.vecdrop block must contain a string element drop loop
	codegentest.AssertContains(t, ir, "field.vecdrop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
	codegentest.AssertContains(t, ir, "call void @Vector.drop(")
}

// T0516: B0219 dup tracking must be per-receiver so reassigns on a DIFFERENT
// instance of the same type still emit element drops correctly. With per-type
// keying, a dup of h1.field would mark "Holder.field" and cause h2.field = w
// to skip its element drop loop, leaking h2's old elements.
func TestFieldAssignVecCrossInstanceDropsElements(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder { string[] field; }
		main() {
			h1 := Holder(string[]());
			h2 := Holder(string[]());
			v := h1.field;
			w := string[]();
			h2.field = w;
			x := string[]();
			h1.field = x;
		}
	`)
	// Both reassigns must emit Vector.drop on the buffer. The h2 reassign
	// must also emit a string element drop loop (h2 was not the duped receiver).
	codegentest.AssertContains(t, ir, "field.vecdrop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
	codegentest.AssertContains(t, ir, "call void @Vector.drop(")
}

// T0540: `v := h.field` for a Vector field with droppable elements on a droppable
// owner must emit a deep element-dup loop (not just a shallow buffer memcpy) so
// the dup owns independent copies. Without the loop, both v and h.field alias
// element pointers and scope-end drops cause a double-free.
func TestB0219FieldAccessVecDeepDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder { string[] field; }
		main() {
			v1 := string[]();
			h := Holder(v1);
			v := h.field;
		}
	`)
	// The shallow dup (vecdup.copy) must be followed by a per-element string
	// dup loop (vecdup_str.head + promise_string_new).
	codegentest.AssertContains(t, ir, "vecdup.copy")
	codegentest.AssertContains(t, ir, "vecdup_str.head")
	codegentest.AssertContains(t, ir, "promise_string_new")
}
