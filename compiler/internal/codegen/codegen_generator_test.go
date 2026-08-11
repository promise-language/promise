package codegen

import (
	"regexp"
	"strings"
	"testing"
)

func TestForInRange(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int sum = 0;
			for i in 0..10 {
				sum += i;
			}
		}
	`)
	assertContains(t, ir, "forin.header")
	assertContains(t, ir, "forin.body")
	assertContains(t, ir, "forin.update")
	assertContains(t, ir, "forin.exit")
}

// T0971: a for-in over a borrowed vector parameter (int[]&) lowers through the
// normal vector for-in path (the borrow is stripped) and must NOT drop the
// borrowed buffer at loop exit (the buffer is owned by the caller).
func TestT0971_ForInBorrowedVectorNoBufferDrop(t *testing.T) {
	ir := generateIR(t, `
		sum(int[] data) int {
			total := 0;
			for x in data { total = total + x; }
			return total;
		}
		main() {}
	`)
	fn := extractFunction(ir, "__user.sum")
	if fn == "" {
		t.Fatalf("function @sum not found in IR")
	}
	// Lowering ran on the unwrapped buffer.
	assertContains(t, fn, "forin.header")
	assertContains(t, fn, "forin.body")
	assertContains(t, fn, "forin.exit")
	// No vector buffer drop is emitted inside @sum — the borrowed param has no
	// drop binding (the buffer is owned by the caller).
	assertNotContains(t, fn, "@Vector.drop")
}

// T0971: a borrowed string vector (string[]&) iterates with the per-iteration
// element clone/drop path (dupStrings), but still must not drop the borrowed
// buffer itself.
func TestT0971_ForInBorrowedStringVectorNoBufferDrop(t *testing.T) {
	ir := generateIR(t, `
		total_len(string[] data) int {
			n := 0;
			for x in data { n = n + x.len; }
			return n;
		}
		main() {}
	`)
	fn := extractFunction(ir, "__user.total_len")
	if fn == "" {
		t.Fatalf("function @total_len not found in IR")
	}
	assertContains(t, fn, "forin.header")
	assertContains(t, fn, "forin.body")
	// Per-iteration string clones are dropped (dupStrings path)...
	assertContains(t, fn, "@promise_string_drop")
	// ...but the borrowed vector buffer itself is never dropped inside the fn.
	assertNotContains(t, fn, "@Vector.drop")
}

// T0330: Failable call as range end operand must auto-propagate.
func TestAutoPropagateInRangeExpr(t *testing.T) {
	ir := generateIR(t, `
		get_end!() int { return 5; }
		wrapper!() {
			for i in 0..get_end() { }
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// T0330: Failable call as range start operand must auto-propagate.
func TestAutoPropagateInRangeStartExpr(t *testing.T) {
	ir := generateIR(t, `
		get_start!() int { return 0; }
		wrapper!() {
			for i in get_start()..5 { }
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestArrayForIn(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] items = [1, 2, 3];
			for x in items {
				int y = x;
			}
		}
	`)
	// Should have for-in loop blocks
	assertContains(t, ir, "forin.header")
	assertContains(t, ir, "forin.body")
	assertContains(t, ir, "forin.update")
	assertContains(t, ir, "forin.exit")
	// Should use unsigned comparison for counter < length
	assertContains(t, ir, "icmp ult")
}

func TestMapForIn(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1, "b": 2};
			for entry in m {
			}
		}
	`)
	// Should have for-in loop blocks
	assertContains(t, ir, "forin.header")
	assertContains(t, ir, "forin.body")
	assertContains(t, ir, "forin.exit")
}

// B0214: Map for-in drops temporary keys/values vectors after the loop.
func TestMapForInVectorCleanup(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1, "b": 2};
			for k, v in m {
			}
		}
	`)
	// Should call Vector.drop on both keys and values vectors in the exit block
	assertContains(t, ir, "forin.exit")
	assertContains(t, ir, "call void @Vector.drop(")
}

// B0214: Map for-in with string keys drops string elements before freeing keys vector.
func TestMapForInStringKeyElementDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1, "b": 2};
			for k, v in m {
			}
		}
	`)
	// String element drop loop for keys vector
	assertContains(t, ir, "vecdrop.head")
	assertContains(t, ir, "vecdrop.body")
}

// B0277: for-in over Vector[string] must dup elements to prevent aliasing.
func TestForInVectorStringDup(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string[] v = ["a", "b"];
			for elem in v {
			}
		}
	`)
	// String elements are dup'd via promise_string_new
	assertContains(t, ir, "strdup.copy")
	// Drop flag for binding
	assertContains(t, ir, "elem.dropflag")
	// Per-iteration drop of previous dup'd string
	assertContains(t, ir, "forin.str.drop")
	// Scope drop via promise_string_drop
	assertContains(t, ir, "call void @promise_string_drop(")
}

// B0279: for-in over fixed-size array of strings must dup elements to prevent aliasing.
func TestForInArrayStringDup(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string[2] arr = ["a", "b"];
			for elem in arr {
			}
		}
	`)
	// String elements are dup'd via promise_string_new
	assertContains(t, ir, "strdup.copy")
	// Drop flag for binding
	assertContains(t, ir, "elem.dropflag")
	// Per-iteration drop of previous dup'd string
	assertContains(t, ir, "forin.str.drop")
	// Scope drop via promise_string_drop
	assertContains(t, ir, "call void @promise_string_drop(")
}

func TestFixedArrayForIn(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[3] items = [1, 2, 3];
			for x in items {
				int y = x;
			}
		}
	`)
	assertContains(t, ir, "forin.header")
	assertContains(t, ir, "forin.body")
	assertContains(t, ir, "forin.update")
	assertContains(t, ir, "forin.exit")
	assertContains(t, ir, "icmp ult")
}

func TestForInString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for ch in "abc" { }
		}
	`)
	assertContains(t, ir, "call i32 @promise_string_next_char(")
	assertContains(t, ir, "forin.str.header")
	assertContains(t, ir, "forin.str.body")
	assertContains(t, ir, "forin.str.exit")
	// Should compare return value with -1
	assertContains(t, ir, "icmp eq i32")
}

func TestForInStringIndexed(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for i, ch in "abc" { }
		}
	`)
	assertContains(t, ir, "call i32 @promise_string_next_char(")
	// Index variable should be allocated and incremented
	assertContains(t, ir, "%i = alloca i64")
	assertContains(t, ir, "add i64")
}

func TestForInStringVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string s = "hello";
			for ch in s { }
		}
	`)
	assertContains(t, ir, "call i32 @promise_string_next_char(")
	assertContains(t, ir, "forin.str.header")
}

func TestForInStringEmpty(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for ch in "" { }
		}
	`)
	assertContains(t, ir, "call i32 @promise_string_next_char(")
	assertContains(t, ir, "forin.str.header")
}

func TestRangeExclusiveCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for i in 0..5 {
				int x = i;
			}
		}
	`)
	// Range loop compares counter < end
	assertContains(t, ir, "icmp slt")
	assertContains(t, ir, "forin.header")
}

func TestRangeInclusiveCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for i in 0..=5 {
				int x = i;
			}
		}
	`)
	// Inclusive range checks counter <= end
	assertContains(t, ir, "forin.header")
	assertContains(t, ir, "forin.body")
}

func TestGeneratorForIn(t *testing.T) {
	ir := generateIR(t, `
		count() stream[int] {
			yield 1;
			yield 2;
		}
		main() {
			int total = 0;
			for x in count() {
				total = total + x;
			}
		}
	`)
	assertContains(t, ir, "@llvm.coro.resume")
	assertContains(t, ir, "@llvm.coro.done")
	assertContains(t, ir, "@llvm.coro.destroy")
}

func TestGeneratorFactoryReturnsStruct(t *testing.T) {
	ir := generateIR(t, `
		nums() stream[int] {
			yield 42;
		}
		main() {}
	`)
	// The factory function should return {i8*, i8*}
	assertContains(t, ir, "insertvalue { i8*, i8* }")
	// Should allocate yield slot
	assertContains(t, ir, "@pal_alloc")
}

func TestYieldDelegateStream(t *testing.T) {
	ir := generateIR(t, `
		inner() stream[int] {
			yield 1;
		}
		outer() stream[int] {
			yield* inner();
		}
		main() {}
	`)
	// yield* over a stream should produce sub-generator resume/done/destroy
	assertContains(t, ir, "yieldstar.check")
	assertContains(t, ir, "yieldstar.yield")
	assertContains(t, ir, "yieldstar.exit")
	// Two generator coroutines: inner and outer. These are top-level (unowned)
	// functions, so their ramps keep bare `.generator.<n>` names (T1222 qualifies
	// only module/instance-owned ramps — e.g. std's Random generators become
	// `.generator.__mod_std_Random.ints.0`). Count the bare user generator defines
	// rather than hardcoding the numbers, which shift with the std generator count.
	userGenDefs := regexp.MustCompile(`define [^\n]*@\.generator\.\d+\(`).FindAllString(ir, -1)
	if len(userGenDefs) != 2 {
		t.Fatalf("expected 2 bare user generator coroutine defines (inner, outer), got %d:\n%v", len(userGenDefs), userGenDefs)
	}
}

func TestYieldDelegateRange(t *testing.T) {
	ir := generateIR(t, `
		nums() stream[int] {
			yield* 1..=3;
		}
		main() {}
	`)
	assertContains(t, ir, "yieldstar.range.header")
	assertContains(t, ir, "yieldstar.range.yield")
	assertContains(t, ir, "@llvm.coro.suspend")
}

func TestYieldDelegateArray(t *testing.T) {
	ir := generateIR(t, `
		nums() stream[int] {
			int[3] arr = [1, 2, 3];
			yield* arr;
		}
		main() {}
	`)
	assertContains(t, ir, "yieldstar.arr.header")
	assertContains(t, ir, "yieldstar.arr.yield")
}

func TestYieldDelegateMixed(t *testing.T) {
	ir := generateIR(t, `
		inner() stream[int] { yield 1; }
		outer() stream[int] {
			yield 0;
			yield* inner();
			yield* 5..=6;
		}
		main() {}
	`)
	// Should have both yield.resume (from regular yield) and yieldstar blocks
	assertContains(t, ir, "yield.resume")
	assertContains(t, ir, "yieldstar.check")
	assertContains(t, ir, "yieldstar.range.header")
}

func TestYieldDelegateVector(t *testing.T) {
	ir := generateIR(t, `
		gen(int[] v) stream[int] {
			yield* v;
		}
		main() {}
	`)
	assertContains(t, ir, "yieldstar.vec.header")
	assertContains(t, ir, "yieldstar.vec.yield")
}

func TestYieldDelegateString(t *testing.T) {
	ir := generateIR(t, `
		gen(string s) stream[char] {
			yield* s;
		}
		main() {}
	`)
	assertContains(t, ir, "yieldstar.str.header")
	assertContains(t, ir, "yieldstar.str.yield")
	assertContains(t, ir, "promise_string_next_char")
}

// --- Failable Generator Tests (B0023) ---

func TestFailableGeneratorFactoryReturnsFailable(t *testing.T) {
	ir := generateIR(t, `
		gen!() stream[int] {
			yield 1;
		}
		main() {}
	`)
	// Factory should return failable result containing {i8*, i8*, i8*}
	assertContains(t, ir, "insertvalue { i8*, i8*, i8* }")
	// Should allocate both yield slot and error slot
	assertContains(t, ir, "@pal_alloc")
	// Should have eager-start resume in factory
	assertContains(t, ir, "gen.factory.error")
	assertContains(t, ir, "gen.factory.ok")
}

func TestFailableGeneratorErrorPropagation(t *testing.T) {
	ir := generateIR(t, `
		helper!() int { raise error("boom"); }
		gen!() stream[int] {
			x := helper()?^;
			yield x;
		}
		main() {}
	`)
	// Error propagation in generator should store to error_slot (not ret wrapError)
	assertContains(t, ir, "error_slot.addr")
	// Should have final.suspend for error exit
	assertContains(t, ir, "final.suspend")
}

func TestFailableGeneratorRaise(t *testing.T) {
	ir := generateIR(t, `
		gen!() stream[int] {
			yield 1;
			raise error("mid-stream error");
		}
		main() {}
	`)
	// raise inside failable generator should store to error_slot
	assertContains(t, ir, "error_slot.addr")
	assertContains(t, ir, "final.suspend")
}

func TestFailableGeneratorYieldDelegateFailable(t *testing.T) {
	ir := generateIR(t, `
		sub!() stream[int] { yield 1; }
		outer!() stream[int] { yield* sub()?^; }
		main() {}
	`)
	// yield* from failable sub-generator should have error_slot handling
	assertContains(t, ir, "yieldstar.errslot")
	assertContains(t, ir, "yieldstar.error")
	assertContains(t, ir, "yieldstar.clean")
}

func TestFailableGeneratorForInInsideGenerator(t *testing.T) {
	ir := generateIR(t, `
		helper!() int { raise error("boom"); }
		inner!() stream[int] { x := helper()?^; yield x; }
		outer!() stream[int] {
			for v in inner()?^ {
				yield v;
			}
		}
		main() {}
	`)
	// For-in over failable generator inside failable generator should
	// propagate via emitGeneratorError (store to outer's error_slot)
	assertContains(t, ir, "gen.forin.error")
	assertContains(t, ir, "gen.forin.clean")
	assertContains(t, ir, "error_slot.addr")
}

func TestFailableGeneratorBreakCleanup(t *testing.T) {
	ir := generateIR(t, `
		gen!() stream[int] { yield 1; yield 2; }
		consume!() int {
			for x in gen()?^ {
				return x;
			}
			return 0;
		}
		main() {}
	`)
	// Return from inside for-in over failable generator should emit
	// generator cleanup (gen.cleanup block)
	assertContains(t, ir, "gen.cleanup")
	assertContains(t, ir, "gen.cleanup.skip")
}

// T0284: for-in over failable generator without explicit error handling
// should unwrap the failable result and produce gen.factory.err / gen.factory.ok blocks.
func TestFailableGeneratorForInUnwrap(t *testing.T) {
	ir := generateIR(t, `
		gen!() stream[int] {
			yield 1;
		}
		main() {
			for x in gen() {
			}
		}
	`)
	assertContains(t, ir, "gen.factory.err")
	assertContains(t, ir, "gen.factory.ok")
}

// T0284: for-in over failable generator in a failable function — error propagates via ret.
func TestFailableGeneratorForInUnwrapFailableFunc(t *testing.T) {
	ir := generateIR(t, `
		gen!() stream[int] {
			yield 1;
		}
		foo!() int {
			for x in gen() {
			}
			return 0;
		}
		main() {
			foo()?!;
		}
	`)
	assertContains(t, ir, "gen.factory.err")
	assertContains(t, ir, "gen.factory.ok")
}

// T0284: yield* from failable generator in a failable generator — error stored to generator error slot.
func TestFailableGeneratorYieldDelegateUnwrap(t *testing.T) {
	ir := generateIR(t, `
		gen!() stream[int] {
			yield 1;
		}
		outer!() stream[int] {
			yield * gen();
		}
		main() {
			for x in outer()?! {
			}
		}
	`)
	assertContains(t, ir, "gen.factory.err")
	assertContains(t, ir, "gen.factory.ok")
}

// T0479: Generator coroutine `~string` param must be dropped at coroutine end.
// Mirrors T0087 (regular function ~ param drop) but inside a generator coroutine,
// where the drop must happen at cleanupBlk (the universal destruction sink for
// natural completion, return, and mid-flight destroy).
func TestT0479GeneratorOwnedStringParamDrop(t *testing.T) {
	ir := generateIR(t, `
		gen(string move s) stream[int] {
			yield 1;
		}
		main() {
			for x in gen("hi".to_string()) {
				break;
			}
		}
	`)
	// Drop flag alloca for the param + conditional drop in the cleanup block.
	assertContains(t, ir, "%s.dropflag = alloca i1")
	assertContains(t, ir, "call void @promise_string_drop(")
	// Drop must happen in the cleanup block (the universal destroy sink), not
	// in the body or final.suspend.
	assertContains(t, ir, "strdrop.call")
}

// T1233: A plain tuple-by-value param BORROWS — the callee (here a generator)
// does NOT drop it (superseding T0406's callee-drop, which double-freed when the
// caller also owned the tuple). For a GENERATOR the param is copied into the
// coroutine frame and read lazily during iteration (a lifetime that outlives the
// call statement), so the caller drops the tuple-literal temp via a SCOPE-level
// owned drop (a synthetic `_gentuparg` local with a bindingDropTuple → the
// `tupdrop` block), NOT a statement-end temp — a statement-end drop would free
// the string before the generator reads it (use-after-free). This test verifies
// the caller-side scope drop and that the generator body has no callee-side drop.
func TestT0479GeneratorPlainTupleParamDrop(t *testing.T) {
	ir := generateIR(t, `
		gen((string, int) t) stream[int] {
			yield 1;
		}
		main() {
			for x in gen(("hi".to_string(), 1)) {
				break;
			}
		}
	`)
	// Caller-side scope-level tuple drop: the field-wise drop block walks the
	// tuple and the string field's drop fires there (not inside the generator).
	assertContains(t, ir, "tupdrop.exec")
	assertContains(t, ir, "_gentuparg")
	assertContains(t, ir, "call void @promise_string_drop(")
	// The generator no longer registers a callee-side tuple drop flag.
	genBody := ir[strings.Index(ir, "@__user.gen("):]
	if idx := strings.Index(genBody, "\ndefine "); idx >= 0 {
		genBody = genBody[:idx]
	}
	if strings.Contains(genBody, "t.dropflag") {
		t.Errorf("plain tuple param should borrow, but generator body registers a tuple drop flag:\n%s", genBody)
	}
}

// T0479: Variadic generator param (vector storage) must be dropped at coroutine end.
// Mirrors B0191 (regular function variadic drop) inside a generator.
func TestT0479GeneratorVariadicParamDrop(t *testing.T) {
	ir := generateIR(t, `
		gen(...string xs) stream[int] {
			yield 1;
		}
		main() {
			for x in gen("a".to_string(), "b".to_string()) {
				break;
			}
		}
	`)
	assertContains(t, ir, "%xs.dropflag = alloca i1")
	// Variadic vector storage drops via Vector.drop.
	assertContains(t, ir, "call void @Vector.drop(")
}

// T0479: Non-droppable generator params must NOT trigger any drop machinery.
// Verifies maybeRegisterDrop's early-return paths for `~int` (RefMut copy type)
// and plain `int` parameters keep paramDrops empty.
func TestT0479GeneratorNonDroppableParamSkipped(t *testing.T) {
	ir := generateIR(t, `
		gen(int move n, int m) stream[int] {
			yield n + m;
		}
		main() {
			for x in gen(1, 2) {
				break;
			}
		}
	`)
	// The generator coroutine should exist but not register any drop flag for
	// int params.
	assertContains(t, ir, ".generator.")
	assertNotContains(t, ir, "%n.dropflag")
	assertNotContains(t, ir, "%m.dropflag")
}

// T0504: Body-local string in a generator must drop on mid-flight destroy.
// emitYieldValue snapshots c.scopeBindings and emits a per-yield cleanup block
// that drops body locals before chaining to the generator's universal cleanup.
func TestT0504GeneratorBodyLocalYieldCleanup(t *testing.T) {
	ir := generateIR(t, `
		gen() stream[int] {
			string s = "x".to_string();
			yield 1;
			yield 2;
		}
		main() {
			for x in gen() {
				break;
			}
		}
	`)
	// A per-yield cleanup block must be emitted (numbered suffix from newBlock).
	assertContains(t, ir, "yield.cleanup")
	// The string drop call must fire from the per-yield cleanup path.
	assertContains(t, ir, "call void @promise_string_drop(")
	// Drop flag alloca for the body local.
	assertContains(t, ir, "%s.dropflag = alloca i1")
}

// T0504: When the generator body has no scope bindings at a yield, no
// per-yield cleanup block is emitted (the switch's tag=1 case targets
// c.generatorCleanup directly).
func TestT0504GeneratorNoBodyLocalsNoYieldCleanup(t *testing.T) {
	ir := generateIR(t, `
		gen() stream[int] {
			yield 1;
			yield 2;
		}
		main() {
			for x in gen() {
				break;
			}
		}
	`)
	// No body locals → no per-yield cleanup block.
	assertNotContains(t, ir, "yield.cleanup")
}

// T0494: Getter returning Vector[T] used in for-in iterable position must be
// promoted to a scope binding so the for-in body's stmt-end cleanup does not
// drop the cloned vector mid-loop.
func TestGetterVectorResultPromotedInForIn(t *testing.T) {
	ir := generateIR(t, `
		type Bag { string[] _tags;
			get tags string[] `+"`public"+` => this._tags.clone();
		}
		test() {
			Bag b = Bag(_tags: ["a", "b"]);
			for t in b.tags {}
		}
	`)
	// The forin promotion creates a scope-bound vector temp.
	assertContains(t, ir, "%__forin_vec_tmp")
	// Vector.drop must be called for the promoted scope binding.
	assertContains(t, ir, "Vector.drop")
}

// T0494: Getter returning Map[K,V] used in for-in iterable position must register
// a heap temp drop so the cloned map is freed once the loop exits.
func TestGetterMapResultTrackedInForIn(t *testing.T) {
	ir := generateIR(t, `
		type Holder { map[string, string] _data;
			get data map[string, string] `+"`public"+` => this._data.clone();
		}
		test() {
			Holder h = Holder(_data: map[string, string]());
			for k, v in h.data {}
		}
	`)
	// The cloned map's instance pointer must be tracked as a heap temp
	// and freed with the Map's drop function.
	assertContains(t, ir, "heap.drop")
	assertContains(t, ir, "Map[string, string].drop")
}

// T0494: Tracked string temp used as for-in iterable must be promoted to a
// scope binding so the body's stmt-end cleanup does not free the string
// mid-iteration. Covers both call results (latent bug pre-T0494) and getter
// results (the T0494-specific case).
func TestStringGetterResultPromotedInForIn(t *testing.T) {
	ir := generateIR(t, `
		type Box { string _content;
			get content string `+"`public"+` => this._content + "!";
		}
		test() {
			Box b = Box(_content: "abc");
			for c in b.content {}
		}
	`)
	// The for-in promotion creates a scope-bound string temp.
	assertContains(t, ir, "%__forin_str_tmp")
	// promise_string_drop must be wired up for the promoted scope binding.
	assertContains(t, ir, "promise_string_drop")
}

// T0494: Generic owner type with getter returning T[] called from inside a
// monomorphized method body exercises the typeSubst branch of
// trackGetterResult — the getter's return type T[] must be substituted to
// e.g. int[] before the Vector check fires, otherwise the result leaks.
func TestGenericGetterVectorResultPromotedInForIn(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T[] _items;
			get items T[] `+"`public"+` => this._items.clone();
			count() int `+"`public"+` {
				int n = 0;
				for x in this.items { n = n + 1; }
				return n;
			}
		}
		test() {
			Box[int] b = Box[int](_items: [1, 2, 3]);
			int n = b.count();
		}
	`)
	// The for-in promotion fires inside the monomorphized Box[int].count body.
	assertContains(t, ir, "%__forin_vec_tmp")
	// Vector.drop is the canonical drop path; substitution must succeed for it
	// to be wired up.
	assertContains(t, ir, "Vector.drop")
	// The monomorphized method must exist so the typeSubst path is exercised.
	assertContains(t, ir, `@"Box[int].count"`)
}

// T0494: Virtual dispatch path through genVirtualGetterCall with a Vector[T]
// return must also tracking-promote in for-in. Distinct from the direct path
// covered by TestGetterVectorResultPromotedInForIn — the virtual path resolves
// the function pointer through the vtable.
func TestVirtualGetterVectorResultPromotedInForIn(t *testing.T) {
	ir := generateIR(t, `
		type ItemSource {
			get items string[] `+"`public"+` `+"`abstract"+`;
		}
		type ItemImpl is ItemSource {
			string[] _items;
			get items string[] `+"`public"+` => this._items.clone();
		}
		test() {
			ItemSource src = ItemImpl(_items: ["a", "b"]);
			for x in src.items {}
		}
	`)
	// The vtable must exist for both abstract base and concrete impl.
	assertContains(t, ir, "@promise_vtable_ItemSource")
	assertContains(t, ir, "@promise_vtable_ItemImpl")
	// Promotion still fires when the call is made via vtable dispatch.
	assertContains(t, ir, "%__forin_vec_tmp")
	assertContains(t, ir, "Vector.drop")
}

// B0173: If-unwrap should clean up iterator temps at the merge block,
// not only in the else branch.
func TestIterCleanupIfUnwrapMergeBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] v = [10, 20, 30, 40];
			if val := v.iter().find(|int x| -> bool { return x > 25; }) {
				int y = val;
			}
		}
	`)
	// __promise_iter_cleanup must appear AFTER the ifunwrap.end merge label,
	// not inside a branch. Both then and else paths should reach the cleanup.
	assertContains(t, ir, "__promise_iter_cleanup")
	assertContains(t, ir, "ifunwrap.end")
}

// B0173: Stream for-in should free the iterator instance after the loop.
func TestStreamForInIterCleanup(t *testing.T) {
	ir := generateIR(t, `
		type NumberIter is Iterator[int] {
			int i;
			int n;
			next(~this) int? {
				if this.i >= this.n { return none; }
				int val = this.i;
				this.i = this.i + 1;
				return val;
			}
		}
		type NumberStream {
			int start;
			int count;
			iter() NumberIter {
				return NumberIter(i: this.start, n: this.count);
			}
		}
		main() {
			s := NumberStream(start: 0, count: 3);
			for x in s {
				int y = x;
			}
		}
	`)
	// The iterator instance from .iter() should be freed at loop exit.
	assertContains(t, ir, "call void @pal_free")
}

// T0128: __promise_iter_cleanup handles _parent field (i64) for chained cleanup
func TestIterCleanupHasParentHandling(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] v = [1, 2, 3];
			Iterator[int] it = v.iter();
		}
	`)
	// iterCleanup should load _parent (i64 field), check != 0, and recursively call itself
	assertContains(t, ir, "define void @__promise_iter_cleanup")
	assertContains(t, ir, "load i64")     // load _parent
	assertContains(t, ir, "clean.parent") // branch label for parent cleanup
	assertContains(t, ir, "inttoptr i64") // convert parent int to ptr
}

// T0128: __promise_iter_cleanup handles _parent field for chained iterator cleanup
func TestIterCleanupParentChain(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] v = [1, 2, 3];
			Iterator[int] it = v.iter().filter(|int x| -> bool { return x > 1; });
		}
	`)
	// iterCleanup should have parent chain handling (inttoptr + recursive call)
	assertContains(t, ir, "__promise_iter_cleanup")
	assertContains(t, ir, "inttoptr")
	assertContains(t, ir, "clean.parent")
}

// T0128: _parent is populated via ptrtoint(this) in structural default methods on _FnIter
func TestFnIterParentPopulated(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] v = [1, 2, 3];
			Iterator[int] it = v.iter().filter(|int x| -> bool { return x > 1; });
		}
	`)
	// The filter structural default on _FnIter should store ptrtoint(this) into _parent
	assertContains(t, ir, "ptrtoint")
}

// T0109: For-in over a call expression returning a vector registers a scope binding
// to drop the temporary vector on all exit paths (normal exit, early return).
func TestForInVectorCallExprScopeBinding(t *testing.T) {
	ir := generateIR(t, `
		type Bag {
			int[] items;
			to_list() int[] { return this.items; }
		}
		main() {
			b := Bag(items: [1, 2, 3]);
			for elem in b.to_list() {
			}
		}
	`)
	// The temp vector from to_list() should have a scope binding with Vector.drop.
	assertContains(t, ir, "__forin_vec_tmp")
	assertContains(t, ir, "call void @Vector.drop(")
}

// B0343: for-in over map[string, string] must dup key/value strings to prevent
// double-free when iteration variables are passed to methods.
func TestForInMapStringDup(t *testing.T) {
	ir := generateIR(t, `
		test() {
			map[string, string] m = map[string, string]();
			for k, v in m {
			}
		}
	`)
	// Key and value strings are dup'd via promise_string_new
	assertContains(t, ir, "strdup.copy")
	// Drop flags for key and value bindings
	assertContains(t, ir, "k.dropflag")
	assertContains(t, ir, "v.dropflag")
	// Per-iteration conditional drops
	assertContains(t, ir, "forin.key.drop")
	assertContains(t, ir, "forin.val.drop")
}

// T0436 Issue 2: a generator method with borrowed this following a ~this method
// on a different type must NOT clear the caller's Optional field present flag
// — it must dup instead. Before the fix, stale thisRecvIsOwned=true from the
// prior ~this method leaked into the generator coroutine, causing
// neutralizeMemberOptionalField to clear the caller's flag through the borrow.
func TestT0436BorrowedGeneratorThisAfterOwnedDups(t *testing.T) {
	ir := generateIR(t, `
		type T0436Box2 { int n; drop(~this) {} }
		type T0436Consumer { T0436Box2? data; consume(~this) { b := this.data!; } }
		type T0436Gen {
			T0436Box2? data;
			iter_n(this) stream[int] {
				b := this.data!;
				yield b.n;
			}
		}
		main() {
			h := T0436Gen(data: T0436Box2(n: 5));
			for n in h.iter_n() {}
		}
	`)
	// Find the iter_n wrapper, then the generator coroutine it calls. The
	// wrapper allocates the yield slot and calls @.generator.N — N is unique
	// per generator function.
	wrapper := extractFunction(ir, "T0436Gen.iter_n")
	if wrapper == "" {
		t.Fatal("expected T0436Gen.iter_n in IR")
	}
	callIdx := strings.Index(wrapper, "@.generator.")
	if callIdx < 0 {
		t.Fatal("expected iter_n to call a generator coroutine")
	}
	parenIdx := strings.Index(wrapper[callIdx:], "(")
	if parenIdx < 0 {
		t.Fatal("malformed coroutine call")
	}
	coroName := wrapper[callIdx+1 : callIdx+parenIdx] // ".generator.N"
	gen := strings.Index(ir, "define i8* @"+coroName+"(")
	if gen < 0 {
		t.Fatalf("expected coroutine %s in IR", coroName)
	}
	rest := ir[gen:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]
	// With the fix, the borrowed receiver dups the heap value via memcpy.
	assertContains(t, body, "call void @llvm.memcpy")
}
