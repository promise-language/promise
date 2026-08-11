package codegen

import (
	"testing"
)

func TestAssignment(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 1;
			x = 2;
		}
	`)
	assertContains(t, ir, "store i64 1")
	assertContains(t, ir, "store i64 2")
}

func TestCompoundAssignment(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			x += 5;
		}
	`)
	assertContains(t, ir, "add i64")
}

// --- Function tests ---

func TestFunctionDeclaration(t *testing.T) {
	ir := generateIR(t, `
		add(int a, int b) int {
			return a + b;
		}
		main() { }
	`)
	assertContains(t, ir, "define i64 @__user.add(i64 %a, i64 %b)")
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "ret i64")
}

// --- Control flow tests ---

func TestIfStmt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			if true {
				int x = 1;
			}
		}
	`)
	assertContains(t, ir, "if.then")
	assertContains(t, ir, "if.end")
	assertContains(t, ir, "br i1 true")
}

func TestIfElseStmt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			if true {
				int x = 1;
			} else {
				int y = 2;
			}
		}
	`)
	assertContains(t, ir, "if.then")
	assertContains(t, ir, "if.else")
	assertContains(t, ir, "if.end")
}

func TestWhileLoop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 0;
			while x < 10 {
				x += 1;
			}
		}
	`)
	assertContains(t, ir, "while.header")
	assertContains(t, ir, "while.body")
	assertContains(t, ir, "while.exit")
	assertContains(t, ir, "icmp slt")
}

func TestInfiniteLoopWithBreak(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for {
				break;
			}
		}
	`)
	assertContains(t, ir, "loop.body")
	assertContains(t, ir, "loop.exit")
}

func TestReturnValue(t *testing.T) {
	ir := generateIR(t, `
		answer() int { return 42; }
		main() { }
	`)
	assertContains(t, ir, "ret i64 42")
}

func TestVoidReturn(t *testing.T) {
	ir := generateIR(t, `main() { return; }`)
	assertContains(t, ir, "ret void")
}

func TestLLVMMemcpyDeclared(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "declare void @llvm.memcpy.p0i8.p0i8.i64(")
}

func TestUserTypeCompoundAssign(t *testing.T) {
	ir := generateIR(t, `
		type Counter { int value; }
		main() {
			c := Counter(value: 0);
			c.value += 1;
		}
	`)
	// Should load, add, store
	assertContains(t, ir, "getelementptr %promise_Counter_i")
	assertContains(t, ir, "add i64")
}

func TestTupleReturn(t *testing.T) {
	ir := generateIR(t, `
		pair() (int, bool) { return (42, true); }
		main() { (a, b) := pair(); }
	`)
	assertContains(t, ir, "define { i64, i1 } @__user.pair()")
	assertContains(t, ir, "ret { i64, i1 }")
}

func TestFunctionTypeReturnFunction(t *testing.T) {
	ir := generateIR(t, `
		make_adder(int x) (int) -> int {
			return move |int y| -> x + y;
		}
		main() {
			(int) -> int add5 = make_adder(5);
			int r = add5(10);
		}
	`)
	// Should return a fat pointer
	assertContains(t, ir, "define { i8*, i8* } @__user.make_adder")
	// Main should do indirect call on the result
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

// --- Part E: Unsafe blocks ---

func TestUnsafeBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			unsafe {
				int x = 42;
			}
		}
	`)
	assertContains(t, ir, "store i64 42")
}

// --- Coverage gap tests ---

// genIfExpr: if-as-expression with phi merge
func TestIfExpression(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = if true { 1; } else { 2; };
		}
	`)
	assertContains(t, ir, "if.then")
	assertContains(t, ir, "if.else")
	assertContains(t, ir, "if.merge")
	assertContains(t, ir, "phi i64")
}

// genClassicForStmt: C-style for loop
func TestClassicFor(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for i := 0; i < 10; i += 1 {
				int x = i;
			}
		}
	`)
	assertContains(t, ir, "for.header")
	assertContains(t, ir, "for.body")
	assertContains(t, ir, "for.update")
	assertContains(t, ir, "for.exit")
	assertContains(t, ir, "icmp slt i64")
	assertContains(t, ir, "add i64")
}

// genContinueStmt
func TestContinueStmt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for i := 0; i < 10; i += 1 {
				if i < 5 {
					continue;
				}
			}
		}
	`)
	// continue should branch to for.update
	assertContains(t, ir, "br label %for.update")
}

// genContinueStmt in while loop
func TestContinueInWhile(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "br label %while.header")
}

func TestAsForcecast(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { }
		main() {
			Animal a = Dog(name: "Rex");
			Dog d = a as! Dog;
		}
	`)
	// Should have RTTI check, then cast.ok/cast.panic blocks
	assertContains(t, ir, "call i32 @promise_type_is")
	assertContains(t, ir, "cast.ok.")
	assertContains(t, ir, "cast.panic.")
	assertContains(t, ir, "call void @promise_panic")
}

func TestReverseOrderTypeDeclaration(t *testing.T) {
	ir := generateIR(t, `
		type Dog is Animal { string breed; }
		type Animal { string name; }
		main() {
			Dog d = Dog(name: "Rex", breed: "Lab");
			string n = d.name;
		}
	`)
	// Topological ordering should compute Animal layout before Dog
	// even though Dog is declared first in source
	assertContains(t, ir, "%promise_Dog_i = type { %promise_Dog_m*, i8*, i8* }")
	assertContains(t, ir, "%promise_Animal_i = type { %promise_Animal_m*, i8* }")
}

// T1029/T1031 non-regression: the assignment form `x = f(n)` binds the result into
// a NEW owner while the source local stays owned. Per T1031 the caller clones the
// aliased instance into the source's storage under a runtime guard (alias.dup) so
// both ends are independently owned — it must NEVER emit the discarded-statement
// temp-clear path (discard.alias.*).
func TestAssignedAliasDoesNotUseDiscardPath(t *testing.T) {
	ir := generateIR(t, `
		type Node { int v; }
		ident_node(Node n) Node { return n; }
		run() int { n := Node(v: 5); x := ident_node(n); return x.v; }
		main() { y := run(); }
	`)
	body := extractFunction(ir, "__user.run")
	assertContains(t, body, "alias.dup")
	assertNotContains(t, body, "discard.alias.clear")
}

// --- Operator Method Dispatch Tests ---

func TestIncDecVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			x := 0;
			x++;
			x--;
		}
	`)
	// ++ adds 1, -- subtracts 1
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "sub i64")
}

func TestClassicForWithIncDec(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for i := 0; i < 5; i++ {
				int x = i;
			}
		}
	`)
	// Should have for loop structure
	assertContains(t, ir, "for.header")
	assertContains(t, ir, "for.body")
	assertContains(t, ir, "for.update")
	// Update should use add i64
	assertContains(t, ir, "add i64")
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
		ir := generateIR(t, `
			identity(string zparam) string {
				return zparam;
			}
			main() {
				string v = "A".to_lower();
				string r = identity(v);
			}
		`)
		// Callee should NOT have a drop flag for its non-~ string param
		assertNotContains(t, ir, "zparam.dropflag")
		// Caller clones the aliased source under the runtime guard.
		assertContains(t, ir, "alias.dup")
		assertContains(t, ir, "strdup.copy")
	})
	t.Run("droppable_user_type", func(t *testing.T) {
		ir := generateIR(t, `
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
		assertNotContains(t, ir, "zparam.dropflag")
		// Caller clones the aliased source under the runtime guard.
		assertContains(t, ir, "alias.dup")
		assertContains(t, ir, "heapdup.copy")
	})
}

// Compound assignment on different typed variables exercises native operator dispatch
func TestCompoundAssignF64(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 x = 1.5;
			x += 2.5;
		}
	`)
	assertContains(t, ir, "fadd double")
}

func TestCompoundAssignI32(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			i32 val;
			work(~this, i32 delta) { this.val -= delta; }
		}
		main() { }
	`)
	assertContains(t, ir, "sub i32")
}

func TestCompoundAssignF32(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			f32 val;
			work(~this, f32 factor) { this.val *= factor; }
		}
		main() { }
	`)
	assertContains(t, ir, "fmul float")
}

func TestCompoundAssignI16(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			i16 val;
			work(~this, i16 delta) { this.val += delta; }
		}
		main() { }
	`)
	assertContains(t, ir, "add i16")
}

func TestCompoundAssignI8(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			i8 val;
			work(~this, i8 delta) { this.val += delta; }
		}
		main() { }
	`)
	assertContains(t, ir, "add i8")
}

func TestLLVMMemmoveDeclared(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2, 3];
			nums.remove(0);
		}
	`)
	assertContains(t, ir, "declare void @llvm.memmove.p0i8.p0i8.i64(")
}

func TestMemcmpDeclared(t *testing.T) {
	ir := generateIR(t, `main() { x := 1; }`)
	assertContains(t, ir, "declare i32 @memcmp(i8* nocapture noundef %s1, i8* nocapture noundef %s2, i64 noundef %n)")
	assertContains(t, ir, "mustprogress nounwind readonly willreturn argmemonly")
}

func TestReceiveExprWaitLoop(t *testing.T) {
	ir := generateIR(t, `
		compute() int { return 1; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Verify the task receive structure (thread-blocking mode in main)
	assertContains(t, ir, "task.done")
	assertContains(t, ir, "task.wait")
	assertContains(t, ir, "task.ready")
}

// T0858: an explicit `return;` in a void, non-failable main() must be lowered
// to branch to the coroutine's final-suspend block, NOT emit a bare `ret void`
// against `.goroutine.main`'s i8* result type (which fails LLVM verification).
func TestMainExplicitReturnNoVoidRet(t *testing.T) {
	ir := generateIR(t, `
		main() { return; }
	`)
	body := extractDefine(ir, ".goroutine.main")
	assertNotContains(t, body, "ret void")
	assertContains(t, body, "br label %final.suspend")
}

// T0858: a conditional early `return;` in main() must lower the same way.
func TestMainConditionalReturnNoVoidRet(t *testing.T) {
	ir := generateIR(t, `
		main() {
			if true { return; }
			print_line("x");
		}
	`)
	body := extractDefine(ir, ".goroutine.main")
	assertNotContains(t, body, "ret void")
	assertContains(t, body, "br label %final.suspend")
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
	ffSpawn := goNewToEnqueue(t, defBody(t, generateIR(t, `
		type W { make(this, int x) string { return "v{x}"; } }
		main() { W w = W(); go w.make(5); }
	`), "define i8* @.goroutine.main("))
	assertNotContains(t, ffSpawn, "pal_alloc") // no result buffer between g_new and enqueue

	taskSpawn := goNewToEnqueue(t, defBody(t, generateIR(t, `
		type W { make(this, int x) string { return "v{x}"; } }
		main() { W w = W(); t := go w.make(5); r := <-t; }
	`), "define i8* @.goroutine.main("))
	assertContains(t, taskSpawn, "pal_alloc") // result buffer allocated for the task
}

func TestFireAndForgetGoCallNoSentinel(t *testing.T) {
	// go void_func() as a statement (fire-and-forget) — should NOT set sentinel.
	ir := generateIR(t, `
		work() { }
		main() {
			go work();
		}
	`)
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestFireAndForgetNonVoidNoResultBuffer(t *testing.T) {
	// B0109 + T1159: go non_void_func() as fire-and-forget (result discarded) should
	// NOT allocate a result buffer — result_ptr stays null so goroutine_exit frees G.
	// T1159 further removes the now-dead runtime-null-checked store machinery for
	// fire-and-forget (result_ptr is statically null): the body drops the discarded
	// result instead (a no-op for a scalar `int`), so there is no store_result block.
	ffBody := defBody(t, generateIR(t, `
		compute() int { return 42; }
		main() {
			go compute();
		}
	`), "define i8* @.goroutine.0(")
	// Fire-and-forget body no longer emits the conditional result store…
	assertNotContains(t, ffBody, "store_result:")
	assertNotContains(t, ffBody, "after_store:")
	// …and the caller stores no sentinel (fire-and-forget, not a void task).
	assertNotContains(t, ffBody, "inttoptr i64 1 to i8*")

	// Contrast: the task-handle form DOES emit the store machinery (result received).
	taskBody := defBody(t, generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			r := <-t;
		}
	`), "define i8* @.goroutine.0(")
	assertContains(t, taskBody, "store_result:")
	assertContains(t, taskBody, "after_store:")
}

// --- Variadic Parameter Tests ---

func TestVariadicFunctionIR(t *testing.T) {
	// Variadic param becomes a T[] (i8*) parameter in IR.
	ir := generateIR(t, `
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
	assertContains(t, ir, "define i64 @__user.sum(i8* %nums)")
}

func TestVariadicEmptyCall(t *testing.T) {
	ir := generateIR(t, `
		count(...int nums) int {
			return nums.len;
		}
		main() {
			count();
		}
	`)
	// Should generate a call with an empty vector
	assertContains(t, ir, "call i64 @__user.count(i8*")
}

func TestVariadicWithFixedParams(t *testing.T) {
	ir := generateIR(t, `
		join(string sep, ...string items) string {
			return sep;
		}
		main() {
			join(",", "a", "b");
		}
	`)
	// Function takes (i8* sep, i8* items) — both are i8* (string and Vector)
	assertContains(t, ir, "define i8* @__user.join(i8* %sep, i8* %items)")
}

func TestVariadicNestedCallIR(t *testing.T) {
	// Variadic passing its param to another variadic — should pass T[] directly.
	ir := generateIR(t, `
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
	assertContains(t, ir, "define i64 @__user.doubleSum(i8* %nums)")
	// Inner call passes nums directly (T[] → T[])
	assertContains(t, ir, "call i64 @__user.sum(i8*")
}

func TestVariadicPassthroughStaticFlag(t *testing.T) {
	// B0203: Variadic passthrough sets bit 63 on the vector's len field
	// so the callee's scope-exit drop skips the free. The caller restores
	// the original len after the call. Static .rodata vectors are never modified.
	ir := generateIR(t, `
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
	assertContains(t, ir, "or i64")
	// The callee (sum) should check bit 63 at scope exit (vecdrop.nonstatic block)
	assertContains(t, ir, "vecdrop.nonstatic")
}

// B0135: if/else where both branches are void must not produce a phi void node.
func TestIfElseVoidBranchesNoPhi(t *testing.T) {
	ir := generateIR(t, `
		test(int n) {
			if n > 0 {
				print_line("pos");
			} else {
				print_line("neg");
			}
		}
		main() { }
	`)
	assertContains(t, ir, "if.then")
	assertContains(t, ir, "if.else")
	assertNotContains(t, ir, "phi void")
}

// T0157: Weak[T].upgrade() uses CAS loop on strong_count for thread safety.
func TestWeakUpgradeCASLoop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			w := a.downgrade();
			if upgraded := w.upgrade() {
				int x = upgraded.borrow;
			}
		}
	`)
	// Upgrade should produce CAS loop blocks (numeric suffix varies)
	assertContainsMatch(t, ir, `weak\.upgrade\.loop\.\d+:`)
	assertContainsMatch(t, ir, `weak\.upgrade\.none\.\d+:`)
	assertContainsMatch(t, ir, `weak\.upgrade\.some\.\d+:`)
	// Should use cmpxchg for atomic upgrade
	assertContains(t, ir, "cmpxchg")
}
