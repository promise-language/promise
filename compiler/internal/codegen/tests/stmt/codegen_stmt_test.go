package stmt

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

func TestAssignment(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int x = 1;
			x = 2;
		}
	`)
	codegentest.AssertContains(t, ir, "store i64 1")
	codegentest.AssertContains(t, ir, "store i64 2")
}

func TestCompoundAssignment(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int x = 10;
			x += 5;
		}
	`)
	codegentest.AssertContains(t, ir, "add i64")
}

// --- Function tests ---

func TestFunctionDeclaration(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		add(int a, int b) int {
			return a + b;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "define i64 @__user.add(i64 %a, i64 %b)")
	codegentest.AssertContains(t, ir, "add i64")
	codegentest.AssertContains(t, ir, "ret i64")
}

// --- Control flow tests ---

func TestIfStmt(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			if true {
				int x = 1;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "if.then")
	codegentest.AssertContains(t, ir, "if.end")
	codegentest.AssertContains(t, ir, "br i1 true")
}

func TestIfElseStmt(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			if true {
				int x = 1;
			} else {
				int y = 2;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "if.then")
	codegentest.AssertContains(t, ir, "if.else")
	codegentest.AssertContains(t, ir, "if.end")
}

func TestWhileLoop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int x = 0;
			while x < 10 {
				x += 1;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "while.header")
	codegentest.AssertContains(t, ir, "while.body")
	codegentest.AssertContains(t, ir, "while.exit")
	codegentest.AssertContains(t, ir, "icmp slt")
}

func TestInfiniteLoopWithBreak(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			for {
				break;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "loop.body")
	codegentest.AssertContains(t, ir, "loop.exit")
}

func TestReturnValue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		answer() int { return 42; }
		main() { }
	`)
	codegentest.AssertContains(t, ir, "ret i64 42")
}

func TestVoidReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { return; }`)
	codegentest.AssertContains(t, ir, "ret void")
}

func TestLLVMMemcpyDeclared(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := 42; }`)
	codegentest.AssertContains(t, ir, "declare void @llvm.memcpy.p0i8.p0i8.i64(")
}

func TestUserTypeCompoundAssign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter { int value; }
		main() {
			c := Counter(value: 0);
			c.value += 1;
		}
	`)
	// Should load, add, store
	codegentest.AssertContains(t, ir, "getelementptr %promise_Counter_i")
	codegentest.AssertContains(t, ir, "add i64")
}

func TestTupleReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		pair() (int, bool) { return (42, true); }
		main() { (a, b) := pair(); }
	`)
	codegentest.AssertContains(t, ir, "define { i64, i1 } @__user.pair()")
	codegentest.AssertContains(t, ir, "ret { i64, i1 }")
}

func TestFunctionTypeReturnFunction(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_adder(int x) (int) -> int {
			return move |int y| -> x + y;
		}
		main() {
			(int) -> int add5 = make_adder(5);
			int r = add5(10);
		}
	`)
	// Should return a fat pointer
	codegentest.AssertContains(t, ir, "define { i8*, i8* } @__user.make_adder")
	// Main should do indirect call on the result
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
}

// --- Coverage gap tests ---

// genIfExpr: if-as-expression with phi merge
func TestIfExpression(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int x = if true { 1; } else { 2; };
		}
	`)
	codegentest.AssertContains(t, ir, "if.then")
	codegentest.AssertContains(t, ir, "if.else")
	codegentest.AssertContains(t, ir, "if.merge")
	codegentest.AssertContains(t, ir, "phi i64")
}

// genClassicForStmt: C-style for loop
func TestClassicFor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			for i := 0; i < 10; i += 1 {
				int x = i;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "for.header")
	codegentest.AssertContains(t, ir, "for.body")
	codegentest.AssertContains(t, ir, "for.update")
	codegentest.AssertContains(t, ir, "for.exit")
	codegentest.AssertContains(t, ir, "icmp slt i64")
	codegentest.AssertContains(t, ir, "add i64")
}

// genContinueStmt
func TestContinueStmt(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			for i := 0; i < 10; i += 1 {
				if i < 5 {
					continue;
				}
			}
		}
	`)
	// continue should branch to for.update
	codegentest.AssertContains(t, ir, "br label %for.update")
}

// genContinueStmt in while loop
func TestContinueInWhile(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int i = 0;
			while i < 10 {
				i += 1;
				if i < 5 {
					continue;
				}
			}
		}
	`)
	// continue should branch to while.header
	codegentest.AssertContains(t, ir, "br label %while.header")
}

func TestAsForcecast(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal { string name; }
		type Dog is Animal { }
		main() {
			Animal a = Dog(name: "Rex");
			Dog d = a as! Dog;
		}
	`)
	// Should have RTTI check, then cast.ok/cast.panic blocks
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is")
	codegentest.AssertContains(t, ir, "cast.ok.")
	codegentest.AssertContains(t, ir, "cast.panic.")
	codegentest.AssertContains(t, ir, "call void @promise_panic")
}

func TestReverseOrderTypeDeclaration(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Dog is Animal { string breed; }
		type Animal { string name; }
		main() {
			Dog d = Dog(name: "Rex", breed: "Lab");
			string n = d.name;
		}
	`)
	// Topological ordering should compute Animal layout before Dog
	// even though Dog is declared first in source
	codegentest.AssertContains(t, ir, "%promise_Dog_i = type { %promise_Dog_m*, i8*, i8* }")
	codegentest.AssertContains(t, ir, "%promise_Animal_i = type { %promise_Animal_m*, i8* }")
}

// T1029/T1031 non-regression: the assignment form `x = f(n)` binds the result into
// a NEW owner while the source local stays owned. Per T1031 the caller clones the
// aliased instance into the source's storage under a runtime guard (alias.dup) so
// both ends are independently owned — it must NEVER emit the discarded-statement
// temp-clear path (discard.alias.*).
func TestAssignedAliasDoesNotUseDiscardPath(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Node { int v; }
		ident_node(Node n) Node { return n; }
		run() int { n := Node(v: 5); x := ident_node(n); return x.v; }
		main() { y := run(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.run")
	codegentest.AssertContains(t, body, "alias.dup")
	codegentest.AssertNotContains(t, body, "discard.alias.clear")
}

// --- Operator Method Dispatch Tests ---

func TestIncDecVariable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			x := 0;
			x++;
			x--;
		}
	`)
	// ++ adds 1, -- subtracts 1
	codegentest.AssertContains(t, ir, "add i64")
	codegentest.AssertContains(t, ir, "sub i64")
}

func TestClassicForWithIncDec(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			for i := 0; i < 5; i++ {
				int x = i;
			}
		}
	`)
	// Should have for loop structure
	codegentest.AssertContains(t, ir, "for.header")
	codegentest.AssertContains(t, ir, "for.body")
	codegentest.AssertContains(t, ir, "for.update")
	// Update should use add i64
	codegentest.AssertContains(t, ir, "add i64")
}

func TestReturnAliasCheck(t *testing.T) {
	// B0345/T1031: When a function returns a non-Copy value that was passed as a
	// non-~ argument, the return pointer may alias the argument. Binding the
	// result into a NEW owner while the source local stays owned must NOT simply
	// transfer the source's flag (that frees the shared instance under the still-
	// owned source — the T1031 double-free/UAF). The caller instead clones into
	// the source's storage under a runtime alias guard, so both end up
	// independently owned. The callee itself returns the bare alias.
	t.Run("string_identity", func(t *testing.T) {
		ir := codegentest.GenerateIR(t, `
			identity(string zparam) string {
				return zparam;
			}
			main() {
				string v = "A".to_lower();
				string r = identity(v);
			}
		`)
		// Callee should NOT have a drop flag for its non-~ string param
		codegentest.AssertNotContains(t, ir, "zparam.dropflag")
		// Caller clones the aliased source under the runtime guard.
		codegentest.AssertContains(t, ir, "alias.dup")
		codegentest.AssertContains(t, ir, "strdup.copy")
	})
	t.Run("droppable_user_type", func(t *testing.T) {
		ir := codegentest.GenerateIR(t, `
			type Resource {
				int id;
				drop(~this) { }
			}
			identity(Resource zparam) Resource {
				return zparam;
			}
			main() {
				Resource v = Resource(id: 1);
				Resource w = identity(v);
			}
		`)
		// Callee should NOT have a drop flag for its non-~ param
		codegentest.AssertNotContains(t, ir, "zparam.dropflag")
		// Caller clones the aliased source under the runtime guard.
		codegentest.AssertContains(t, ir, "alias.dup")
		codegentest.AssertContains(t, ir, "heapdup.copy")
	})
}

// Compound assignment on different typed variables exercises native operator dispatch
func TestCompoundAssignF64(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			f64 x = 1.5;
			x += 2.5;
		}
	`)
	codegentest.AssertContains(t, ir, "fadd double")
}

func TestCompoundAssignI32(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box {
			i32 val;
			work(~this, i32 delta) { this.val -= delta; }
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "sub i32")
}

func TestCompoundAssignF32(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box {
			f32 val;
			work(~this, f32 factor) { this.val *= factor; }
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "fmul float")
}

func TestCompoundAssignI16(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box {
			i16 val;
			work(~this, i16 delta) { this.val += delta; }
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "add i16")
}

func TestCompoundAssignI8(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box {
			i8 val;
			work(~this, i8 delta) { this.val += delta; }
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "add i8")
}

func TestLLVMMemmoveDeclared(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int[] nums = [1, 2, 3];
			nums.remove(0);
		}
	`)
	codegentest.AssertContains(t, ir, "declare void @llvm.memmove.p0i8.p0i8.i64(")
}

func TestMemcmpDeclared(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := 1; }`)
	codegentest.AssertContains(t, ir, "declare i32 @memcmp(i8* nocapture noundef %s1, i8* nocapture noundef %s2, i64 noundef %n)")
	codegentest.AssertContains(t, ir, "mustprogress nounwind readonly willreturn argmemonly")
}

func TestReceiveExprWaitLoop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		compute() int { return 1; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Verify the task receive structure (thread-blocking mode in main)
	codegentest.AssertContains(t, ir, "task.done")
	codegentest.AssertContains(t, ir, "task.wait")
	codegentest.AssertContains(t, ir, "task.ready")
}

// T0858: an explicit `return;` in a void, non-failable main() must be lowered
// to branch to the coroutine's final-suspend block, NOT emit a bare `ret void`
// against `.goroutine.main`'s i8* result type (which fails LLVM verification).
func TestMainExplicitReturnNoVoidRet(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { return; }
	`)
	body := codegentest.ExtractDefine(ir, ".goroutine.main")
	codegentest.AssertNotContains(t, body, "ret void")
	codegentest.AssertContains(t, body, "br label %final.suspend")
}

// T0858: a conditional early `return;` in main() must lower the same way.
func TestMainConditionalReturnNoVoidRet(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			if true { return; }
			print_line("x");
		}
	`)
	body := codegentest.ExtractDefine(ir, ".goroutine.main")
	codegentest.AssertNotContains(t, body, "ret void")
	codegentest.AssertContains(t, body, "br label %final.suspend")
}

// T1159: fire-and-forget `go obj.method(...)` (via-block path) with a non-void
// result must NOT allocate a result buffer in the caller — the coroutine body
// drops the discarded result via cleanupStmtTemps. Contrast: the task-handle form
// allocates a buffer (`pal_alloc`) between `promise_g_new` and `promise_sched_enqueue`
// and stores it into G.result_ptr. Guards the via-block path (genGoCallExprViaBlock).
func TestT1159_ViaBlockFireAndForgetNoResultBuffer(t *testing.T) {
	// The user's `main()` is lowered into the `.goroutine.main` coroutine, whose
	// body holds the go-spawn site. Scope the g_new→enqueue slice to that body so
	// it isolates the user spawn from the runtime's own main-goroutine spawn.
	ffSpawn := codegentest.GoNewToEnqueue(t, codegentest.DefBody(t, codegentest.GenerateIR(t, `
		type W { make(this, int x) string { return "v{x}"; } }
		main() { W w = W(); go w.make(5); }
	`), "define i8* @.goroutine.main("))
	codegentest.AssertNotContains(t, ffSpawn, "pal_alloc") // no result buffer between g_new and enqueue

	taskSpawn := codegentest.GoNewToEnqueue(t, codegentest.DefBody(t, codegentest.GenerateIR(t, `
		type W { make(this, int x) string { return "v{x}"; } }
		main() { W w = W(); t := go w.make(5); r := <-t; }
	`), "define i8* @.goroutine.main("))
	codegentest.AssertContains(t, taskSpawn, "pal_alloc") // result buffer allocated for the task
}

func TestFireAndForgetGoCallNoSentinel(t *testing.T) {
	// go void_func() as a statement (fire-and-forget) — should NOT set sentinel.
	ir := codegentest.GenerateIR(t, `
		work() { }
		main() {
			go work();
		}
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestFireAndForgetNonVoidNoResultBuffer(t *testing.T) {
	// B0109 + T1159: go non_void_func() as fire-and-forget (result discarded) should
	// NOT allocate a result buffer — result_ptr stays null so goroutine_exit frees G.
	// T1159 further removes the now-dead runtime-null-checked store machinery for
	// fire-and-forget (result_ptr is statically null): the body drops the discarded
	// result instead (a no-op for a scalar `int`), so there is no store_result block.
	ffBody := codegentest.DefBody(t, codegentest.GenerateIR(t, `
		compute() int { return 42; }
		main() {
			go compute();
		}
	`), "define i8* @.goroutine.0(")
	// Fire-and-forget body no longer emits the conditional result store…
	codegentest.AssertNotContains(t, ffBody, "store_result:")
	codegentest.AssertNotContains(t, ffBody, "after_store:")
	// …and the caller stores no sentinel (fire-and-forget, not a void task).
	codegentest.AssertNotContains(t, ffBody, "inttoptr i64 1 to i8*")

	// Contrast: the task-handle form DOES emit the store machinery (result received).
	taskBody := codegentest.DefBody(t, codegentest.GenerateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			r := <-t;
		}
	`), "define i8* @.goroutine.0(")
	codegentest.AssertContains(t, taskBody, "store_result:")
	codegentest.AssertContains(t, taskBody, "after_store:")
}

// --- Variadic Parameter Tests ---

func TestVariadicFunctionIR(t *testing.T) {
	// Variadic param becomes a T[] (i8*) parameter in IR.
	ir := codegentest.GenerateIR(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		main() {
			sum(1, 2, 3);
		}
	`)
	// The function should take i8* (Vector) as its parameter
	codegentest.AssertContains(t, ir, "define i64 @__user.sum(i8* %nums)")
}

func TestVariadicEmptyCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		count(...int nums) int {
			return nums.len;
		}
		main() {
			count();
		}
	`)
	// Should generate a call with an empty vector
	codegentest.AssertContains(t, ir, "call i64 @__user.count(i8*")
}

func TestVariadicWithFixedParams(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		join(string sep, ...string items) string {
			return sep;
		}
		main() {
			join(",", "a", "b");
		}
	`)
	// Function takes (i8* sep, i8* items) — both are i8* (string and Vector)
	codegentest.AssertContains(t, ir, "define i8* @__user.join(i8* %sep, i8* %items)")
}

func TestVariadicNestedCallIR(t *testing.T) {
	// Variadic passing its param to another variadic — should pass T[] directly.
	ir := codegentest.GenerateIR(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		doubleSum(...int nums) int {
			return sum(nums) * 2;
		}
		main() {
			doubleSum(1, 2, 3);
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @__user.doubleSum(i8* %nums)")
	// Inner call passes nums directly (T[] → T[])
	codegentest.AssertContains(t, ir, "call i64 @__user.sum(i8*")
}

func TestVariadicPassthroughStaticFlag(t *testing.T) {
	// B0203: Variadic passthrough sets bit 63 on the vector's len field
	// so the callee's scope-exit drop skips the free. The caller restores
	// the original len after the call. Static .rodata vectors are never modified.
	ir := codegentest.GenerateIR(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		doubleSum(...int nums) int {
			return sum(nums) * 2;
		}
		main() {
			int x = doubleSum(1, 2, 3);
		}
	`)
	// doubleSum should set bit 63 before calling sum (passthrough)
	codegentest.AssertContains(t, ir, "or i64")
	// The callee (sum) should check bit 63 at scope exit (vecdrop.nonstatic block)
	codegentest.AssertContains(t, ir, "vecdrop.nonstatic")
}

// B0135: if/else where both branches are void must not produce a phi void node.
func TestIfElseVoidBranchesNoPhi(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test(int n) {
			if n > 0 {
				print_line("pos");
			} else {
				print_line("neg");
			}
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "if.then")
	codegentest.AssertContains(t, ir, "if.else")
	codegentest.AssertNotContains(t, ir, "phi void")
}

// T0157: Weak[T].upgrade() uses CAS loop on strong_count for thread safety.
func TestWeakUpgradeCASLoop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := Ref[int](42);
			w := a.downgrade();
			if upgraded := w.upgrade() {
				int x = upgraded.borrow;
			}
		}
	`)
	// Upgrade should produce CAS loop blocks (numeric suffix varies)
	codegentest.AssertContainsMatch(t, ir, `weak\.upgrade\.loop\.\d+:`)
	codegentest.AssertContainsMatch(t, ir, `weak\.upgrade\.none\.\d+:`)
	codegentest.AssertContainsMatch(t, ir, `weak\.upgrade\.some\.\d+:`)
	// Should use cmpxchg for atomic upgrade
	codegentest.AssertContains(t, ir, "cmpxchg")
}

// T1896: a bare failable call as a for-in iterable must be routed through the
// auto-propagate path. genForInStmt used to call genExpr directly, so the raw
// {i1, i8*, i8*} result struct reached genForInVector, which GEP'd it as a
// vector instance pointer and panicked codegen.
//
// Every assertion below is scoped to the user function under test with
// FuncBody: GenerateIR returns a module with std embedded, and std has plenty of
// auto-propagating calls and for-in-over-a-temp loops of its own, so a
// module-wide AssertContains for "auto.propagate" or "__forin_vec_tmp" would
// pass whether or not the change under test did anything.
func TestForInBareFailableVectorAutoPropagates(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		mkv!() u8[] {
			u8[] v = u8[]();
			v.push(1u8);
			return v;
		}
		run!() int {
			int n = 0;
			for b in mkv() { n += 1; }
			return n;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	// The result struct is branched on and the success value extracted...
	codegentest.AssertContains(t, body, "auto.propagate")
	codegentest.AssertContains(t, body, "auto.ok")
	// ...and the unwrapped vector is promoted to a scope binding, so it drops on
	// every exit path (T1883: the implicit path must register the same temp the
	// explicit `?^` path does, or the propagated vector leaks).
	codegentest.AssertContains(t, body, "__forin_vec_tmp")
	codegentest.AssertContains(t, body, "call void @Vector.drop")
}

// T1896: the bare form must emit what the explicit `?^` emits — §7.2 makes them
// the same expression. Comparing the two bodies for the same markers is the
// contract; asserting one spelling alone would not notice the two drifting.
func TestForInFailableVectorSpellingsAgree(t *testing.T) {
	for _, spelling := range []string{"mkv()", "mkv()?^"} {
		t.Run(spelling, func(t *testing.T) {
			ir := codegentest.GenerateIR(t, `
				mkv!() u8[] {
					u8[] v = u8[]();
					v.push(1u8);
					return v;
				}
				run!() int {
					int n = 0;
					for b in `+spelling+` { n += 1; }
					return n;
				}
				main() { }
			`)
			body := codegentest.FuncBody(t, ir, "run")
			codegentest.AssertContains(t, body, "__forin_vec_tmp")
			codegentest.AssertContains(t, body, "call void @Vector.drop")
		})
	}
}

// T1896: same contract for the string branch — unwrap, then promote the
// propagated string so the iteration outlives the enclosing statement.
func TestForInBareFailableStringAutoPropagates(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		mks!() string { return "a" + "bc"; }
		run!() int {
			int n = 0;
			for ch in mks() { n += 1; }
			return n;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "auto.propagate")
	codegentest.AssertContains(t, body, "auto.ok")
	codegentest.AssertContains(t, body, "__forin_str_tmp")
	codegentest.AssertContains(t, body, "call void @promise_string_drop")
}

// T1896 / T1417: the duck-typed branch (a plain type with next(~this) T?) took
// the same unpropagated genExpr path. It did not panic — it silently handed
// genForInCustomIter the result struct and iterated zero times.
func TestForInBareFailableDuckTypedAutoPropagates(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter {
			int n;
			next(~this) int? {
				if this.n >= 3 { return none; }
				int v = this.n;
				this.n = this.n + 1;
				return v;
			}
		}
		mkc!() Counter { return Counter(n: 0); }
		run!() int {
			int sum = 0;
			for x in mkc() { sum += x; }
			return sum;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "auto.propagate")
	codegentest.AssertContains(t, body, "auto.ok")
	// genForInCustomIter's loop blocks: the next()-driven path ran on the
	// unwrapped Counter, not on the raw result struct.
	codegentest.AssertContains(t, body, "iter.header")
}

// T1896: the raw generator branch keeps its own unwrap (T0284's
// unwrapFailableGeneratorResult, which emits gen.factory.ok/err). Sema's
// isRawGeneratorForIn withholds AutoPropagateExprs there so the two mechanisms
// cannot both fire on the same iterable. run() is deliberately non-failable:
// losing that guard turns the iterable into a "failable call must be handled"
// sema error, which fails this test at GenerateIR. Whether it *should* be that
// error is T1942 — until that is settled, this pins today's behavior.
func TestForInBareFailableGeneratorKeepsGeneratorUnwrap(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		gen!(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		run() int {
			int sum = 0;
			for x in gen(3) { sum += x; }
			return sum;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "gen.factory.ok")
	codegentest.AssertContains(t, body, "gen.factory.err")
	codegentest.AssertNotContains(t, body, "auto.propagate")
}

// T1896: genForInStmt dispatches on the iterable's type and each branch reads
// the value independently, so "the vector branch unwraps" says nothing about
// the rest. One test per remaining branch, each pinned to the same two markers:
// the auto-propagate preamble ran, and the branch's own loop was built on top of
// the unwrapped value rather than on the raw {i1, i8*, i8*} result struct.
func TestForInBareFailableMapAutoPropagates(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		mkm!() map[string, int] {
			map[string, int] m = {:};
			m["a"] = 1;
			return m;
		}
		run!() int {
			int n = 0;
			for k, v in mkm() { n += v; }
			return n;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "auto.propagate")
	codegentest.AssertContains(t, body, "auto.ok")
	// genForInMap's bucket walk — reached with a map instance pointer.
	codegentest.AssertContains(t, body, "forin.header")
	// T1941: the map branch has no scope-binding promotion, so there is
	// deliberately no __forin_*_tmp to assert here. Adding one is that item.
}

func TestForInBareFailableRangeAutoPropagates(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		mkr!(int n) Range[int] { return 0..n; }
		run!() int {
			int n = 0;
			for i in mkr(3) { n += 1; }
			return n;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "auto.propagate")
	codegentest.AssertContains(t, body, "auto.ok")
	codegentest.AssertContains(t, body, "forin.header")
}

// The channel branch is asserted here and not in tests/e2e/failable_forin.pr
// because running it leaks 5 allocations — T1940, a gap in
// trackUnwrappedFailableTemp's handling of native handles that the explicit
// `?^` spelling hits identically. The IR still shows what this item is about:
// the receive loop is built on the unwrapped channel.
func TestForInBareFailableChannelAutoPropagates(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		mkch!() channel[int] {
			ch := channel[int](capacity: 1);
			ch.close();
			return ch;
		}
		run!() int {
			int n = 0;
			for x in mkch() { n += 1; }
			return n;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "auto.propagate")
	codegentest.AssertContains(t, body, "auto.ok")
	codegentest.AssertContains(t, body, "forin_ch.header")
}

// The `iter()`-method shape lands on the ForInIter case of the duck-typed
// switch — a different call site from the ForInNext case the Counter test
// above covers, and the one place a failable iterable can reach
// genForInCustomStream (a Stream[T]-typed one is always a raw generator).
func TestForInBareFailableIterMethodAutoPropagates(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type NumsIter {
			int cur;
			int limit;
			next(~this) int? {
				if this.cur >= this.limit { return none; }
				int v = this.cur;
				this.cur = this.cur + 1;
				return v;
			}
		}
		type Nums {
			int limit;
			iter(~this) NumsIter { return NumsIter(cur: 0, limit: this.limit); }
		}
		mkn!(int limit) Nums { return Nums(limit: limit); }
		run!() int {
			int sum = 0;
			for x in mkn(4) { sum += x; }
			return sum;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "auto.propagate")
	codegentest.AssertContains(t, body, "auto.ok")
	codegentest.AssertContains(t, body, "iter.header")
}

// The iterable does not have to be a call. FailableExprs is also set on a
// failable getter (a MemberExpr) and on `<-t` for a failable_task[T] (a
// UnaryExpr, §17.2.1) — genForInStmt keys off AutoPropagateExprs, not off the
// node kind, and isRawGeneratorForIn's "is this a CallExpr" test must not send
// these anywhere unusual.
func TestForInBareFailableNonCallIterablesAutoPropagate(t *testing.T) {
	cases := map[string]string{
		"getter": `
			type Holder {
				get items! int[] {
					int[] v = [];
					v.push(1);
					return v;
				}
			}
			run!() int {
				Holder h = Holder();
				int n = 0;
				for x in h.items { n += x; }
				return n;
			}
			main() { }
		`,
		"awaited_task": `
			mkv!() int[] {
				int[] v = [];
				v.push(1);
				return v;
			}
			run!() int {
				t := go! { mkv()?^ };
				int n = 0;
				for x in <-t { n += x; }
				return n;
			}
			main() { }
		`,
		"parenthesized": `
			mkv!() int[] {
				int[] v = [];
				v.push(1);
				return v;
			}
			run!() int {
				int n = 0;
				for x in (mkv()) { n += x; }
				return n;
			}
			main() { }
		`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			body := codegentest.FuncBody(t, codegentest.GenerateIR(t, src), "run")
			codegentest.AssertContains(t, body, "auto.propagate")
			codegentest.AssertContains(t, body, "__forin_vec_tmp")
			codegentest.AssertContains(t, body, "call void @Vector.drop")
		})
	}
}

// The negative control for every assertion above: genExprAutoPropagate must
// stay a no-op when sema recorded nothing. Without this, an implementation that
// wrapped *every* iterable would satisfy the whole rest of this file.
func TestForInNonFailableIterableEmitsNoAutoPropagate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		mkv() int[] {
			int[] v = [];
			v.push(1);
			return v;
		}
		run() int {
			int n = 0;
			for x in mkv() { n += x; }
			for c in "ab" { n += 1; }
			for i in 0..3 { n += 1; }
			return n;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "forin.header")
	codegentest.AssertNotContains(t, body, "auto.propagate")
}

// The array branch reaches the iterable through genArrayBasePtr, which already
// consulted AutoPropagateExprs (B0323) — so it needed no codegen change and was
// nevertheless broken, because sema was never recording the iterable. It is the
// one branch where the whole fix is the sema half.
func TestForInBareFailableArrayAutoPropagates(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		mka!() int[3] {
			int[3] a = [1, 2, 3];
			return a;
		}
		run!() int {
			int total = 0;
			for x in mka() { total += x; }
			return total;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "auto.propagate")
	codegentest.AssertContains(t, body, "auto.ok")
	codegentest.AssertContains(t, body, "forin.header")
}

// T1735's shape, kept here because T1896 reordered the branch that chooses
// between it and the raw-generator path: a Stream[T] *variable* holds a
// structural view, so it must take iter()+next() and not the coroutine path.
// Nothing about it is failable, which is the point — the reorder must not have
// made the choice depend on the new failable route. The getter spelling of the
// same subject is T1943 (misread as a view, segfaults) and predates this item.
func TestForInStreamVariableStillUsesIterPath(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type NumsIter {
			int cur;
			int limit;
			next(~this) int? {
				if this.cur >= this.limit { return none; }
				int v = this.cur;
				this.cur = this.cur + 1;
				return v;
			}
		}
		type Nums {
			int limit;
			iter(~this) NumsIter { return NumsIter(cur: 0, limit: this.limit); }
		}
		run() int {
			Stream[int] s = Nums(limit: 3);
			int sum = 0;
			for x in s { sum += x; }
			return sum;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "iter.header")
	// Not the coroutine path, and nothing to unwrap.
	codegentest.AssertNotContains(t, body, "gen.factory.ok")
	codegentest.AssertNotContains(t, body, "auto.propagate")
}
