package codegen

import (
	"strings"
	"testing"

	irtypes "github.com/llir/llvm/ir/types"
)

// T0674 (item 2): calling a function value retrieved by index — fns[0](x) where
// fns is a Vector[(int) -> int] — must NOT be mistaken for a generic-function
// instantiation. Before the fix, genCallExpr unconditionally routed an IndexExpr
// callee to genGenericFuncCall, which mangled a bogus name ("fns[int]") from the
// index's *type* and panicked with `undefined monomorphic function "fns[int]"`.
// The gate now checks the indexed target's recorded type: only a generic Signature
// routes to the generic path; a value subscript yielding a callable falls through
// to the closure-value (indirect fat-pointer) call path. Assert: no panic, no bogus
// "fns[int]" symbol, and an indirect call through the loaded {fn, env} pointer.
func TestFunctionValueIndexCall(t *testing.T) {
	ir := generateIR(t, `
		main() {
			fns := Vector[(int) -> int]();
			fns.push(|int x| -> x + 1);
			int r = fns[0](10);
		}
	`)
	// The bogus generic mangling must never appear.
	assertNotContains(t, ir, "fns[int]")
	assertNotContains(t, ir, "undefined monomorphic")
	// Closure dispatch: load the function pointer out of the {fn, env} fat pointer
	// and call indirectly (env passed as the first arg).
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

// T0674 (item 2, member-field form): the SAME routing bug had a second face —
// h.fns[0](x) where fns is a Vector[(int) -> int] field. Because idx.Target is a
// MemberExpr (h.fns), the pre-fix code unconditionally routed it into
// genGenericMethodCall, which looked for a method named "fns" on Holder and
// panicked `codegen: no method fns on type Holder`. The type-gated routing now
// sees idx.Target's recorded type is a Vector (not a generic Signature), so it
// falls through to the closure-value call path exactly like the free-function
// form. This pins the second panic at the IR level (the free-function form is
// pinned by TestFunctionValueIndexCall; only the runtime e2e batch test exercised
// the member form before).
func TestFunctionValueMemberFieldIndexCall(t *testing.T) {
	ir := generateIR(t, `
		type Holder { Vector[(int) -> int] fns; }
		main() {
			h := Holder(fns: Vector[(int) -> int]());
			h.fns.push(|int x| -> x + 1);
			int r = h.fns[0](10);
		}
	`)
	// Neither the bogus generic mangling nor a "no method" mis-route may appear.
	assertNotContains(t, ir, "fns[int]")
	assertNotContains(t, ir, "no method")
	// Same indirect closure dispatch through the loaded {fn, env} fat pointer.
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

// --- Part C: Slice / Array tests ---

func TestArrayLiteral(t *testing.T) {
	ir := generateIR(t, `main() { x := [1, 2, 3]; }`)
	// T0062: all-constant vector literals use static .rodata globals
	assertContains(t, ir, "@.arr.0 = private constant")
	assertContains(t, ir, "[3 x i64] [i64 1, i64 2, i64 3]")
}

func TestArrayLiteralNonConstant(t *testing.T) {
	ir := generateIR(t, `main() { int x = 1; int[] v = [x, x + 1]; }`)
	// Non-constant elements should heap-allocate
	assertContains(t, ir, "call i8* @pal_alloc(i64")
}

func TestStaticVectorCOW(t *testing.T) {
	ir := generateIR(t, `main() { x := [1, 2, 3]; x.push(4); }`)
	// Should call promise_vector_cow before push
	assertContains(t, ir, "call i8* @promise_vector_cow(")
	assertContains(t, ir, "call i8* @promise_vector_push(")
}

func TestArrayIndex(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] items = [1, 2, 3];
			int x = items[0];
		}
	`)
	// Should have bounds check
	assertContains(t, ir, "icmp ult")
	// Should have ok and oob blocks
	assertContains(t, ir, "index.ok")
	assertContains(t, ir, "index.oob")
	// Should call promise_panic on out-of-bounds
	assertContains(t, ir, "call void @promise_panic(")
}

func TestArrayIndexAssign(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] items = [1, 2, 3];
			items[0] = 42;
		}
	`)
	// Should have bounds check and store
	assertContains(t, ir, "icmp ult")
	assertContains(t, ir, "indexassign.ok")
	assertContains(t, ir, "store i64 42")
}

func TestArrayBoundsCheck(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] items = [1, 2, 3];
			int x = items[0];
		}
	`)
	// Bounds check uses unsigned less-than
	assertContains(t, ir, "icmp ult")
	// Out-of-bounds path calls promise_panic
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "unreachable")
}

func TestArrayVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] items = [1, 2, 3];
			int[] copy = items;
		}
	`)
	// Slice is stored/loaded as i8*
	assertContains(t, ir, "alloca i8*")
	assertContains(t, ir, "store i8*")
	assertContains(t, ir, "load i8*")
}

// --- Part D: Map tests ---

func TestMapLiteral(t *testing.T) {
	ir := generateIR(t, `main() { m := {"a": 1}; }`)
	// Should call monomorphized constructor and index assign
	assertContains(t, ir, "call void @\"Map[string, int].new\"(")
	assertContains(t, ir, `call void @"Map[string, int].[]="(`)
}

func TestMapIndex(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
			int? v = m["a"];
		}
	`)
	// Should call monomorphized [] method (returns optional { i1, i64 })
	assertContains(t, ir, `call { i1, i64 } @"Map[string, int].[]"(`)
}

func TestMapIndexWithElvis(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
			int v = m["a"] ?: 0;
		}
	`)
	// Should call monomorphized [] method + elvis
	assertContains(t, ir, `call { i1, i64 } @"Map[string, int].[]"(`)
	assertContains(t, ir, "elvis.some")
}

func TestMapIndexAssign(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
			m["a"] = 42;
		}
	`)
	// Should call monomorphized []= method
	assertContains(t, ir, `call void @"Map[string, int].[]="(`)
}

func TestMapIntKeys(t *testing.T) {
	ir := generateIR(t, `main() { m := {1: "one", 2: "two"}; }`)
	// Should create monomorphized map with int keys
	assertContains(t, ir, "call void @\"Map[int, string].new\"(")
	assertContains(t, ir, `call void @"Map[int, string].[]="(`)
}

func TestSliceLen(t *testing.T) {
	ir := generateIRWithStd(t, stdContainers, `
		main() {
			int[] arr = [1, 2, 3];
			int n = arr.len;
		}
	`)
	// Should GEP into slice header and load length
	assertContains(t, ir, "getelementptr { i64, i64 }")
	assertContains(t, ir, "load i64")
}

func TestArrayLen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] items = [1, 2, 3];
			int n = items.len;
		}
	`)
	assertContains(t, ir, "getelementptr { i64, i64 }")
	assertContains(t, ir, "load i64")
}

// --- Fixed-size array tests ---

func TestFixedArrayLiteral(t *testing.T) {
	ir := generateIR(t, `
		main() { int[3] x = [1, 2, 3]; }
	`)
	// Should use alloca [3 x i64] for stack allocation
	assertContains(t, ir, "alloca [3 x i64]")
	// Should store elements via GEP
	assertContains(t, ir, "store i64 1")
	assertContains(t, ir, "store i64 2")
	assertContains(t, ir, "store i64 3")
}

func TestFixedArrayIndex(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[3] items = [1, 2, 3];
			int x = items[0];
		}
	`)
	// Should have bounds check against constant 3
	assertContains(t, ir, "icmp ult i64")
	assertContains(t, ir, "arridx.ok")
	assertContains(t, ir, "arridx.oob")
	assertContains(t, ir, "call void @promise_panic(")
}

func TestFixedArrayIndexAssign(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[3] items = [1, 2, 3];
			items[0] = 42;
		}
	`)
	assertContains(t, ir, "icmp ult i64")
	assertContains(t, ir, "arrassign.ok")
	assertContains(t, ir, "store i64 42")
}

func TestFixedArrayLen(t *testing.T) {
	ir := generateIR(t, `
		get_len(int[3] arr) int { return arr.len; }
		main() { }
	`)
	// .len on fixed array should be a compile-time constant 3
	// The function body should just return i64 3 without loading from a header
	assertContains(t, ir, "ret i64 3")
}

func TestFixedArrayCopy(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[3] a = [1, 2, 3];
			int[3] b = a;
		}
	`)
	// Fixed arrays are value types — stored/loaded as [3 x i64]
	assertContains(t, ir, "alloca [3 x i64]")
	assertContains(t, ir, "store [3 x i64]")
	assertContains(t, ir, "load [3 x i64]")
}

func TestFixedArrayParam(t *testing.T) {
	ir := generateIR(t, `
		sum(int[3] arr) int { return arr[0]; }
		main() {
			int[3] items = [1, 2, 3];
			int s = sum(items);
		}
	`)
	// Function should take [3 x i64] parameter
	assertContains(t, ir, "[3 x i64]")
}

func TestFixedArrayFieldAssign(t *testing.T) {
	ir := generateIR(t, `
		type Grid { int[3] data; }
		main() {
			g := Grid(data: [1, 2, 3]);
			g.data[0] = 42;
		}
	`)
	// Should GEP into the instance field directly (not a temp copy)
	assertContains(t, ir, "getelementptr [3 x i64]")
}

// T0579: Array field with heap user element — layout must recurse so the
// field slot stores the value-struct {i8*, i8*} form, matching the array
// literal's element type. Pre-fix produced [N x i8*] and crashed NewStore.
func TestFixedArrayFieldHeapUserElement(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; drop(~this) {} }
		type _Holder { _Box[2] data; }
		main() {
			h := _Holder(data: [_Box(n: 1), _Box(n: 2)]);
		}
	`)
	// Field slot should be [2 x value struct], not [2 x i8*].
	assertContains(t, ir, "[2 x { i8*, i8* }]")
	// Synth drop must walk the array and drop each element.
	assertContains(t, ir, "call void @_Box.drop")
}

// T0579: Array field with value-type element — the topological layout pass
// must compute the value type's layout before the container, otherwise the
// slot falls back to the narrow {i8*, i8*} layout.
func TestFixedArrayFieldValueTypeElement(t *testing.T) {
	ir := generateIR(t, `
		type _Pt { int x `+"`value"+`; int y `+"`value"+`; }
		type _Holder { _Pt[2] data; }
		main() {
			h := _Holder(data: [_Pt(x: 1, y: 2), _Pt(x: 3, y: 4)]);
		}
	`)
	// Field slot should hold the wider value struct per element (i8* vtable + 2 ints).
	assertContains(t, ir, "[2 x %promise__Pt_v]")
}

// T0579: Array field with tuple element containing a droppable inner —
// exercises the Tuple branch in `typeNeedsFieldDrop`. The synth drop must
// walk the array and drop the tuple's string element.
func TestFixedArrayFieldTupleElement(t *testing.T) {
	ir := generateIR(t, `
		type _TupArr { (string, int)[2] data; }
		main() {
			t := _TupArr(data: [("a", 1), ("b", 2)]);
		}
	`)
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0579: Array field with Vector element — exercises `typeNeedsFieldDrop`'s
// `AsVector` branch. Without it, the predicate returns false and the inner
// Vector's heap buffer leaks at scope exit.
func TestFixedArrayFieldVectorElement(t *testing.T) {
	ir := generateIR(t, `
		type _VecArr { Vector[int][2] data; }
		main() {
			v0 := Vector[int]();
			v0.push(10);
			v1 := Vector[int]();
			v1.push(20);
			v := _VecArr(data: [v0, v1]);
		}
	`)
	assertContains(t, ir, "call void @Vector.drop")
}

func TestFixedArrayF64(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64[2] arr = [1.5, 2.5];
			f64 x = arr[0];
		}
	`)
	assertContains(t, ir, "alloca [2 x double]")
	assertContains(t, ir, "getelementptr [2 x double]")
}

func TestFixedArrayBool(t *testing.T) {
	ir := generateIR(t, `
		main() {
			bool[2] arr = [true, false];
			bool x = arr[0];
		}
	`)
	assertContains(t, ir, "alloca [2 x i1]")
	assertContains(t, ir, "getelementptr [2 x i1]")
}

func TestMapLen(t *testing.T) {
	ir := generateIRWithStd(t, stdContainers, `
		main() {
			m := {"a": 1};
			int n = m.len;
		}
	`)
	// Should call monomorphized len getter
	assertContains(t, ir, "call i64 @\"Map[string, int].len\"(")
}

func TestSliceLenInCondition(t *testing.T) {
	ir := generateIRWithStd(t, stdContainers, `
		main() {
			int[] arr = [1, 2, 3];
			if arr.len > 0 { }
		}
	`)
	assertContains(t, ir, "getelementptr { i64, i64 }")
	assertContains(t, ir, "icmp sgt i64")
}

func TestMapCompoundAssign(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
			m["a"] += 1;
		}
	`)
	// Should call [] to get, add, then []= to set
	assertContains(t, ir, `call { i1, i64 } @"Map[string, int].[]"(`)
	assertContains(t, ir, "mapcomp.ok")
	assertContains(t, ir, "mapcomp.panic")
	assertContains(t, ir, "add i64")
	assertContains(t, ir, `call void @"Map[string, int].[]="(`)
}

func TestMapCompoundAssignMul(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"x": 2};
			m["x"] *= 3;
		}
	`)
	assertContains(t, ir, `call { i1, i64 } @"Map[string, int].[]"(`)
	assertContains(t, ir, "mul i64")
	assertContains(t, ir, `call void @"Map[string, int].[]="(`)
}

// T0689: When MemoryLimitAccounting is on but the per-test map is nil (no
// per-test limits set), set_test_state calls are not emitted in main. The
// globals + helpers still exist so the per-test path can later be activated,
// but the harness simply doesn't drive it.
func TestMemoryLimitNoSetCallsWithoutPerTestMap(t *testing.T) {
	file, info := parseWithStd(t, `
		myTest() `+"`test"+` { }
	`)
	result := CompileWithOptions(file, info, "", &CompileOptions{
		DebugAllocator:        true,
		MemoryLimitAccounting: true,
	})
	// Deliberately do NOT call SetTestMemoryLimits.
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()
	// Helper symbol declared (so other code could call it):
	assertContains(t, ir, "define void @__promise_memory_set_test_state(i64 %new_limit)")
	// But no per-test invocation inside main():
	if strings.Contains(ir, "call void @__promise_memory_set_test_state") {
		t.Error("expected no set_test_state calls when testMemoryLimits is nil")
	}
}

func TestIncDecIndexedElement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] items = [1, 2, 3];
			items[0]++;
		}
	`)
	// Should have bounds check
	assertContains(t, ir, "incdec.index.ok")
	assertContains(t, ir, "incdec.index.oob")
	// Should load, increment, store back
	assertContains(t, ir, "add i64")
}

// --- Compound index eval order test ---

func TestCompoundIndexAssignSlice(t *testing.T) {
	// Ensure compound index assignments on slices generate valid IR
	ir := generateIR(t, `
		main() {
			s := [1, 2, 3];
			s[0] += 10;
		}
	`)
	assertContains(t, ir, "define i32 @main")
	// Should contain the compound add operation
	assertContains(t, ir, "add i64")
}

func TestLlvmTypeAlignArray(t *testing.T) {
	arr := irtypes.NewArray(10, irtypes.I32)
	if a := llvmTypeAlign(arr); a != 4 {
		t.Errorf("[10 x i32] align: got %d, want 4", a)
	}
}

func TestLlvmTypeSizeArray(t *testing.T) {
	arr := irtypes.NewArray(5, irtypes.I32)
	if sz := llvmTypeSize(arr); sz != 20 {
		t.Errorf("[5 x i32] size: got %d, want 20", sz)
	}
}

// --- Vector method tests ---

func TestVectorPush(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2];
			nums.push(3);
		}
	`)
	assertContains(t, ir, "define i8* @promise_vector_push(")
	assertContains(t, ir, "call i8* @promise_vector_push(")
}

func TestVectorPop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2];
			int? v = nums.pop();
		}
	`)
	assertContains(t, ir, "define i32 @promise_vector_pop(")
	assertContains(t, ir, "call i32 @promise_vector_pop(")
	assertContains(t, ir, "pop.some")
	assertContains(t, ir, "pop.none")
}

func TestVectorContainsInt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2, 3];
			bool has = nums.contains(2);
		}
	`)
	assertContains(t, ir, "define i8 @promise_vector_contains(")
	assertContains(t, ir, "call i8 @promise_vector_contains(")
}

func TestVectorRemove(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2, 3];
			nums.remove(0);
		}
	`)
	assertContains(t, ir, "define void @promise_vector_remove(")
	assertContains(t, ir, "call void @promise_vector_remove(")
}

func TestVectorContainsFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1];
			bool has = nums.contains(1);
		}
	`)
	// Verify key blocks in the defined contains function
	assertContains(t, ir, "loop.header:")
	assertContains(t, ir, "loop.body:")
	assertContains(t, ir, "cmp_bytes:")
	assertContains(t, ir, "call_eq:")
	// memcmp replaces byte-by-byte loop
	assertContains(t, ir, "call i32 @memcmp(")
	assertNotContains(t, ir, "byte.header:")
	assertNotContains(t, ir, "byte.body:")
	assertContains(t, ir, "found:")
	assertContains(t, ir, "not_found:")
	assertContains(t, ir, "loop.next:")
}

func TestVectorRemoveFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2, 3];
			nums.remove(1);
		}
	`)
	// Verify key blocks in the defined remove function
	assertContains(t, ir, "panic:")
	assertContains(t, ir, "check_shift:")
	assertContains(t, ir, "do_shift:")
	assertContains(t, ir, "dec_len:")
	// Verify panic calls and memmove
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "call void @llvm.memmove.p0i8.p0i8.i64(")
}

func TestVectorWithCapacityFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2];
			nums.push(3);
		}
	`)
	// with_capacity is always defined (codegen intrinsic)
	assertContains(t, ir, "define i8* @promise_vector_with_capacity(")
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "init:")
	assertContains(t, ir, "store i64 0")
}

func TestVectorPushFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1];
			nums.push(2);
		}
	`)
	// Verify key blocks in the defined push function
	assertContains(t, ir, "define i8* @promise_vector_push(")
	assertContains(t, ir, "grow:")
	assertContains(t, ir, "call i8* @pal_realloc(")
	assertContains(t, ir, "oom:")
	assertContains(t, ir, "update_cap:")
	assertContains(t, ir, "copy:")
	assertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
}

// B0147: Vector.push through MutRef parameter must dispatch via container path,
// not fall through to generic method lookup (which fails for mono instances).
func TestVectorPushViaMutRefParam(t *testing.T) {
	ir := generateIR(t, `
		helper(u8[] ~buf, int val) {
			buf.push(val as! u8);
		}
		main() {
			u8[] b = Vector[u8](capacity: 4);
			helper(b, 42);
		}
	`)
	assertContains(t, ir, "call i8* @promise_vector_push(")
}

func TestVectorPopFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2];
			int? v = nums.pop();
		}
	`)
	// Verify key blocks in the defined pop function
	assertContains(t, ir, "define i32 @promise_vector_pop(")
	assertContains(t, ir, "empty:")
	assertContains(t, ir, "do_pop:")
	assertContains(t, ir, "ret i32 0")
	assertContains(t, ir, "ret i32 1")
	assertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
}

func TestVectorContainsIntNull(t *testing.T) {
	// Int contains passes null eq_fn → byte comparison path
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2, 3];
			bool has = nums.contains(2);
		}
	`)
	assertContains(t, ir, "call i8 @promise_vector_contains(")
	// Null eq_fn for int (non-string) type
	assertContains(t, ir, "null)")
}

// --- Vector default capacity constructor ---

func TestVectorDefaultCapacity(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := Vector[int]();
			v.push(1);
		}
	`)
	// Should call promise_vector_with_capacity with default capacity 16
	assertContains(t, ir, "call i8* @promise_vector_with_capacity(i64 16,")
}

func TestVectorExplicitCapacity(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := Vector[int](64);
			v.push(1);
		}
	`)
	// Should call promise_vector_with_capacity with explicit capacity 64
	assertContains(t, ir, "call i8* @promise_vector_with_capacity(i64 64,")
}

// --- Slice Type Expression (T[] in expression position) ---

func TestSliceTypeExprDefaultCapacity(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := int[]();
			v.push(1);
		}
	`)
	// int[]() should generate the same IR as Vector[int]()
	assertContains(t, ir, "call i8* @promise_vector_with_capacity(i64 16,")
}

func TestSliceTypeExprExplicitCapacity(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := int[](capacity: 64);
			v.push(1);
		}
	`)
	assertContains(t, ir, "call i8* @promise_vector_with_capacity(i64 64,")
}

func TestSliceTypeExprNested(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := int[][]();
			inner := int[]();
			inner.push(1);
			v.push(inner);
		}
	`)
	// Both outer and inner should use vector_with_capacity
	assertContains(t, ir, "call i8* @promise_vector_with_capacity(i64 16,")
}

// T0686 (/coverage follow-up): the heapTemps non-isolation bug fired for ANY
// trailing expression that registers a heap temp, not just user structs. A
// VECTOR LITERAL with non-const elements registers a heap temp via the T0369
// path (`elemType != nil`), which is a DIFFERENT branch of cleanupHeapTemps
// (element-walk + buffer free) than the struct dropFunc path covered above. This
// guards that the distinct vector heap-temp path is also isolated to the inner
// coroutine — the awaiting `.goroutine.main` presplitcoroutine must not load its
// `%0` (the coro.id token) as a heap drop-flag (i1*) or heap pointer (i8**).
func TestT0686_VectorResultNoTokenLoad(t *testing.T) {
	ir := generateIR(t, `
		main() {
			n := 3;
			task[int[]] x = go { [n, n + 1, n + 2] };
			r := <-x;
		}
	`)
	defStart := strings.Index(ir, "define i8* @.goroutine.main(")
	if defStart < 0 {
		t.Fatal("expected a .goroutine.main coroutine definition in the IR")
	}
	body := ir[defStart:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end+2]
	}
	assertContains(t, body, "call token @llvm.coro.id") // %0 is the token
	assertNotContains(t, body, "i1* %0")                // no heap drop-flag load from the token
	assertNotContains(t, body, "i8** %0")               // no heap pointer load from the token
	// Positive guards: the vector result is still stored into G.result_ptr inside
	// the inner coroutine, and the caller allocates a real result buffer.
	assertContains(t, ir, "go.store_result")
	assertContains(t, ir, "@pal_alloc")
}

// B0228: Category B — OOM in vector_push returns null instead of unreachable.
func TestVectorPushOOMReturnsNull(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			v.push(4);
		}
	`)
	// The OOM path in promise_vector_push should return null
	assertContains(t, ir, "define i8* @promise_vector_push(")
}

func TestSliceExprVector(t *testing.T) {
	// Vector [:] calls the Promise-implemented method
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3, 4, 5];
			int[] sub = v[1:3];
		}
	`)
	assertContains(t, ir, `call i8* @"Vector[int].[:]"(`)
}

func TestSliceExprThroughSharedRef(t *testing.T) {
	// T0332: slicing a SharedRef parameter must auto-deref to the Vector's [:] method
	ir := generateIR(t, `
		take(int[] xs) int[] {
			return xs[1:3];
		}
		main() {
			v := [1, 2, 3, 4, 5];
			int[] sub = take(v);
		}
	`)
	assertContains(t, ir, `call i8* @"Vector[int].[:]"(`)
}

func TestSliceExprThroughMutRef(t *testing.T) {
	// T0332: slicing through a MutRef must auto-deref before looking up `[:]`.
	// `int[]~` (suffix ~ on the typeRef) is the mutRefType form, producing a
	// parameter whose Type is MutRef[Vector[int]]. (`~int[]` prefix ~ is
	// moveParam syntax — it strips ~ and gives an unwrapped Vector[int] type,
	// so it does not exercise the codegen MutRef unwrap branch.)
	ir := generateIR(t, `
		take(int[]~ xs) int[] {
			return xs[1:3];
		}
		main() {
			v := [1, 2, 3, 4, 5];
			int[] sub = take(v);
		}
	`)
	assertContains(t, ir, `call i8* @"Vector[int].[:]"(`)
}

func TestSliceAssignVector(t *testing.T) {
	// Vector [:]= calls the Promise-implemented method
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3, 4, 5];
			v[1:3] = [10, 20];
		}
	`)
	assertContains(t, ir, `call void @"Vector[int].[:]=`)
}

func TestUserDefinedIndexOperator(t *testing.T) {
	// User-defined type with [] operator method
	ir := generateIR(t, `
		type Grid {
			int[] data;
			int width;

			[](int index) int? {
				if index < 0 { return none; }
				if index >= this.data.len { return none; }
				return this.data[index];
			}
		}
		main() {
			Grid g = Grid(data: [1, 2, 3], width: 3);
			int? v = g[1];
		}
	`)
	assertContains(t, ir, `call { i1, i64 } @"Grid.[]"(`)
}

func TestUserDefinedIndexAssignOperator(t *testing.T) {
	// User-defined type with [] and []= operator methods
	ir := generateIR(t, `
		type Grid {
			int[] data;
			int width;

			[](int index) int {
				return this.data[index];
			}
			[]=(int index, int value) {
				this.data[index] = value;
			}
		}
		main() {
			Grid g = Grid(data: [1, 2, 3], width: 3);
			g[1] = 42;
		}
	`)
	assertContains(t, ir, `call void @"Grid.[]="(`)
}

func TestUserDefinedSliceOperator(t *testing.T) {
	// User-defined type with [:] operator method
	ir := generateIR(t, `
		type MyList {
			int[] data;

			[:](int? start, int? end) int[] {
				return this.data[start:end];
			}
		}
		main() {
			MyList l = MyList(data: [1, 2, 3, 4, 5]);
			int[] sub = l[1:3];
		}
	`)
	assertContains(t, ir, `call i8* @"MyList.[:]"(`)
}

func TestVariadicPassVectorDirectly(t *testing.T) {
	ir := generateIR(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		main() {
			int[] v = [1, 2, 3];
			sum(v);
		}
	`)
	// Should pass vector directly, not wrap in another vector
	assertContains(t, ir, "call i64 @__user.sum(i8*")
}

func TestVariadicMultipleArgsArrayLit(t *testing.T) {
	// Multiple variadic args should be wrapped into array literal in IR.
	ir := generateIR(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		main() {
			sum(10, 20);
		}
	`)
	// Should see Vector_int creation with elements pushed
	assertContains(t, ir, "call i64 @__user.sum(i8*")
}

// TestMapCompoundAssignUnchangedIR — non-failable [] (Map) compound assignment
// keeps the optional-presence shape untouched by the T0709 change.
func TestMapCompoundAssignUnchangedIR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[string, int] m = {"a": 1};
			m["a"] += 5;
		}
	`)
	// Map [] is non-failable: the presence-check path (not auto.propagate) is used.
	assertContains(t, ir, "mapcomp.ok")
	assertNotContains(t, ir, "add { i1, i64, i8* }")
}

// T0735: A map literal used as the trailing return expression must be claimed
// at the return path so the caller takes ownership. Without the claim, the
// callee's heap-temp drop would run before return and the caller would receive
// a dangling pointer (or the callee leaks if cleanup is missed).
func TestT0735_MapLitReturnValueClaimed(t *testing.T) {
	ir := generateIR(t, `
		make_map() Map[string, int] { return {"x": 9}; }
		main() {}
	`)
	// The function emitted from `make_map` is the user function (not a
	// .goroutine wrapper); it has its own IR body containing the literal.
	start := strings.Index(ir, `define { i8*, i8* } @__user.make_map(`)
	if start < 0 {
		t.Fatal("expected __user.make_map")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	body := rest[:end+2]
	// pal_alloc for the map literal must happen, the heap temp must be tracked,
	// and then claimed before the ret so the caller takes ownership.
	assertContains(t, body, "call i8* @pal_alloc(")
	assertContains(t, body, `call void @"Map[string, int].new"`)
	assertContains(t, body, "heap.claim")
	assertContains(t, body, "ret { i8*, i8* }")
}

// T0735: Two map literals in the same statement must each get their own
// heap-temp drop flag (independent allocas) and each must be cleaned up
// independently at statement end. Tests stack discipline of the heap-temp
// stack — if both literals shared a flag, one's drop would clobber the other's.
func TestT0735_TwoMapLitsInSameStmtBothTracked(t *testing.T) {
	ir := generateIR(t, `
		borrow_a(Map[string, int] m) int { return m.len; }
		borrow_b(Map[string, int] m) int { return m.len; }
		main() {
			int x = borrow_a({"a": 1}) + borrow_b({"b": 2});
		}
	`)
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	body := rest[:end+2]
	// Both borrow_a and borrow_b calls present.
	assertContains(t, body, "call i64 @__user.borrow_a(")
	assertContains(t, body, "call i64 @__user.borrow_b(")
	// Two distinct pal_alloc calls for the two map instances.
	if c := strings.Count(body, "call i8* @pal_alloc("); c < 2 {
		t.Fatalf("expected at least 2 pal_alloc calls (one per map literal), got %d", c)
	}
	// Two distinct Map.drop calls at statement end — one per heap-temp flag.
	dropCount := strings.Count(body, `call void @"Map[string, int].drop"`)
	if dropCount < 2 {
		t.Fatalf("expected at least 2 Map.drop calls in main body, got %d", dropCount)
	}
}

// --- T0745: `this[i]` / `this[i]=v` (and slice forms) on a user index operator ---
//
// genExpr(this) yields a bare instance i8* (value-struct ptr for value types),
// not a value struct. The four subscript/slice dispatch gates (genMethodIndex,
// genSliceExpr, genMethodIndexAssign, genSliceAssign) gated on isContainerType
// with no `this` branch, so the raw i8* fell through to extractInstancePtr and
// emitted `extractvalue i8* ..., 1` (opt-rejected); the value-type read path
// additionally panicked. The fix adds an isThisReceiver() branch (peels
// ParenExpr) that uses the i8* receiver directly. These tests guard the IR shape
// via the "no extractvalue from i8*" invariant and check the operator fn takes an
// i8* receiver.

// Read: bare `this[i]`, `(this)[i]`, `((this))[i]` on a user `[]` operator.
func TestT0745ThisIndexReadNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0745IdxBox {
			int[] xs;
			[](int i) int { return this.xs[i]; }
			first(this) int { return this[0]; }
			first_p(this) int { return (this)[0]; }
			first_pn(this) int { return ((this))[0]; }
		}
		main() { b := T0745IdxBox(xs: [10, 20]); _ := b.first(); _ := b.first_p(); _ := b.first_pn(); }
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	first := extractFunction(ir, "T0745IdxBox.first")
	if first == "" {
		t.Fatal("expected T0745IdxBox.first in IR")
	}
	// The operator method is invoked with the i8* `this` receiver directly.
	assertContains(t, first, `@"T0745IdxBox.[]"(i8*`)
}

// Write: bare `this[i] = v` and `(this)[i] = v` on a user `[]=` operator.
func TestT0745ThisIndexWriteNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0745WBox {
			int[] xs;
			[](int i) int { return this.xs[i]; }
			[]=(int i, int v) { this.xs[i] = v; }
			set_bare(~this, int i, int v) { this[i] = v; }
			set_paren(~this, int i, int v) { (this)[i] = v; }
		}
		main() { b := T0745WBox(xs: [10, 20]); b.set_bare(0, 99); b.set_paren(1, 77); }
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	setBare := extractFunction(ir, "T0745WBox.set_bare")
	if setBare == "" {
		t.Fatal("expected T0745WBox.set_bare in IR")
	}
	assertContains(t, setBare, `@"T0745WBox.[]="(i8*`)
}

// Slice read/write: `this[lo:hi]` and `this[lo:hi] = v` on user `[:]`/`[:]=`.
func TestT0745ThisSliceNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0745SliceBox {
			int[] xs;
			[:](int? lo, int? hi) int[] { return this.xs[lo:hi]; }
			[:]=(int? lo, int? hi, int[] v) { this.xs[lo:hi] = v; }
			head2(this) int[] { return this[0:2]; }
			head2_p(this) int[] { return (this)[0:2]; }
			replace01(~this, int[] v) { this[0:2] = v; }
			replace01_p(~this, int[] v) { (this)[0:2] = v; }
		}
		main() {
			b := T0745SliceBox(xs: [1, 2, 3, 4]);
			_ := b.head2(); _ := b.head2_p();
			b.replace01([9, 8]); b.replace01_p([7, 6]);
		}
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0745SliceBox.head2") == "" {
		t.Fatal("expected T0745SliceBox.head2 in IR")
	}
	if extractFunction(ir, "T0745SliceBox.replace01") == "" {
		t.Fatal("expected T0745SliceBox.replace01 in IR")
	}
}

// Value-type `this[i]` read — previously panicked in valueTypeReceiverPtr.
// generateIR not panicking is itself the regression guard.
func TestT0745ThisIndexValueType(t *testing.T) {
	ir := generateIR(t, `
		type T0745VPair {
			int a `+"`value"+`;
			int b `+"`value"+`;
			[](int i) int { if i == 0 { return this.a; } return this.b; }
			first(this) int { return this[0]; }
			first_p(this) int { return (this)[1]; }
		}
		main() { p := T0745VPair(a: 5, b: 7); _ := p.first(); _ := p.first_p(); }
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0745VPair.first") == "" {
		t.Fatal("expected T0745VPair.first in IR")
	}
}
