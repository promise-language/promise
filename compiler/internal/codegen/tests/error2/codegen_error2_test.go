package error2

import (
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
)

func TestFailableDestructureDiscardError(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!() int { return 42; }
		main() {
			(val, _) := parse();
		}
	`)
	codegentest.AssertContains(t, ir, "destruct.merge")
	codegentest.AssertContains(t, ir, "%val")
}

// B0263: Failable destructure value must be freed at scope exit for heap user types.
func TestFailableDestructureValueFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pt { int x; int y; }
		make!() Pt { return Pt(x: 1, y: 2); }
		main() {
			(p, err) := make();
		}
	`)
	// The value variable 'p' should get a free binding (heap user type without drop).
	// emitFreeCall null-checks the instance pointer (safe for the error path's zeroinit).
	codegentest.AssertContains(t, ir, "free.call")
	codegentest.AssertContains(t, ir, "free.exec")
}

// T1160: the failable-unwrap layers (`?!`, `?^`) only extract the inner call's
// success value, so a discarded closure result must still be tracked through them.
func TestClosureFailableCallResultTrackedAsEnvTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_adder!(int x) () -> int { return || -> x + 1; }
		do_it() { make_adder(10)?!; }
		main() { do_it(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.do_it")
	codegentest.AssertContains(t, body, "env.tmp.drop")
	codegentest.AssertContains(t, body, "env.tmp.exec")
}

// T1242: a failable getter discarded with `?!` (or `?^`) reaches
// closureResultMayAliasCallInput's MemberExpr arm. The outer genExpr(ErrorPanicExpr)
// calls trackUnwrappedFailableTemp with the extracted {fn,env} fat pointer; inside
// closureResultMayAliasCallInput the peel loop strips the ErrorPanicExpr, landing on
// the MemberExpr whose Target is the alias receiver.  T1227 ensures fresh getter
// results (no borrowed field returned), so the receiver has no closure field and the
// filter returns false — the env is registered and freed at statement end.
func TestClosureFailableGetterResultTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Maker { int base; get adder! () -> int { b := this.base; return move || -> b + 1; } }
		discard_failable() { m := Maker(base: 1); m.adder?!; }
		propagate_failable!() { m := Maker(base: 1); m.adder?^; }
		main() {}
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.discard_failable"), "env.tmp.drop")
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.propagate_failable"), "env.tmp.drop")
}

// T1160: the filter's ErrorPropagateExpr peel arm. `?^` yields the inner call's
// success value, so a discarded closure it produced is owned here and must be
// freed; the sibling `?!` arm is pinned by TestClosureFailableCallResultTrackedAsEnvTemp.
// Without the peel, `?^` lands in `default: return true` and the env leaks.
func TestClosurePropagatedFailableCallResultTrackedAsEnvTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_adder!(int x) () -> int { return || -> x + 1; }
		via_propagate!() { make_adder(10)?^; }
		main() { via_propagate()?!; }
	`)
	body := codegentest.ExtractFunction(ir, "__user.via_propagate")
	codegentest.AssertContains(t, body, "env.tmp.drop")
	codegentest.AssertContains(t, body, "env.tmp.exec")
}

// A discard leaving its scope through an early `return` or a `raise` unwind must be
// cleaned on those exit edges, not only on block fallthrough.
func TestClosureCallResultDroppedOnReturnAndRaisePaths(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_adder(int x) () -> int { return move || -> x + 1; }
		type Unwind is error { int code; }
		early(bool b) int {
			if b { make_adder(1); return 1; }
			return 0;
		}
		unwinding!(bool b) {
			make_adder(1);
			if b { raise Unwind(code: 1, message: "boom"); }
		}
		main() {}
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.early"), "env.tmp.drop")
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.unwinding"), "env.tmp.drop")
}

func TestStructuralSatisfactionWithoutMetaFails(t *testing.T) {
	// Without `structural meta, explicit `is is required
	src := `
		type Printable {
			print() string ` + "`" + `abstract;
		}
		type Doc {
			print() string { return "doc"; }
		}
		main() {
			Printable p = Doc();
		}
	`
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	pr := parser.NewPromiseParser(stream)
	pr.RemoveErrorListeners()
	tree := pr.CompilationUnit()
	file, errs := ast.Build("test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}
	_, errs = sema.Check(file)
	if len(errs) == 0 {
		t.Error("expected sema error for assigning Doc to Printable without `structural, got none")
	}
}

func TestStructuralAdapterNonFailableToFailable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Processor `+"`"+`structural {
			process!(int x) int `+"`"+`abstract;
		}
		type Simple {
			process(int x) int { return x; }
		}
		main() {
			Processor p = Simple();
		}
	`)
	codegentest.AssertContains(t, ir, "Simple.process$view_adapt")
}

func TestPrimitiveToFailableStructuralView(t *testing.T) {
	// Primitive method is non-failable, interface method is failable
	// → adapter wraps result as success
	ir := codegentest.GenerateIR(t, `
		type Converter `+"`"+`structural {
			to_string!() string `+"`"+`abstract;
		}
		convert(Converter c) string { return c.to_string()?!; }
		main() { convert(42); }
	`)
	codegentest.AssertContains(t, ir, "@promise_vtable_int_as_Converter")
	codegentest.AssertContains(t, ir, "int.to_string$view_adapt")
}

func TestReturnThisFailable(t *testing.T) {
	// Failable method returning `this` should also wrap into value struct.
	ir := codegentest.GenerateIR(t, `
		type Widget {
			int id;
			clone!() Widget `+"`structural(protocol: false)"+` { return this; }
		}
		main() {
			w := Widget(id: 1);
			Widget w2 = w.clone()?!;
		}
	`)
	// Result type is { i1, { i8*, i8* }, i8* } (ok flag, value struct, error ptr)
	codegentest.AssertContains(t, ir, "define { i1, { i8*, i8* }, i8* } @Widget.clone(i8* %this)")
	codegentest.AssertContains(t, ir, "insertvalue { i8*, i8* }")
}

// T0275: Batch test main frees heap-allocated panic messages to prevent leak.
// Verifies: testPanicTypeGlobal declared, free_panic_msg block generated,
// and leak delta adjustment discounts the panic msg allocation.
func TestPanicMsgFreedInTestMain(t *testing.T) {
	src := `myTest() ` + "`test" + ` { }`
	result := codegentest.CompileResult(t, src)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(src)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()

	// T0275: __promise_test_panic_type global declared (non-TLS i8)
	codegentest.AssertContains(t, ir, "@__promise_test_panic_type = global i8 0")
	// T0275: free_panic_msg block conditionally frees heap panic msgs
	codegentest.AssertContains(t, ir, "free_panic_msg_myTest:")
	codegentest.AssertContains(t, ir, "after_free_panic_myTest:")
	// T0275: Leak delta adjustment — select discounts heap panic from delta
	codegentest.AssertContains(t, ir, "icmp eq i8")
}

// T0275: Test trampoline copies panic type to test harness global.
func TestTrampolineCopiesPanicType(t *testing.T) {
	src := `myTest() ` + "`test" + ` { }`
	result := codegentest.CompileResult(t, src)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(src)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()

	// The trampoline stores panic type to the non-TLS test global (load from TLS, store to non-TLS)
	codegentest.AssertContains(t, ir, "load i8, i8* @__promise_panic_type")
	codegentest.AssertContains(t, ir, "store i8 1, i8* @__promise_test_panic_type")
}

// --- Failable close() error propagation (B0013) ---

func TestUseFailableCloseErrorCapture(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type FRes {
			int id;
			close!(~this) { }
		}
		process!() {
			use r := FRes(id: 1);
			int x = r.id;
		}
	`)
	// Failable close should generate a result-type call (not void)
	codegentest.AssertContains(t, ir, "call { i1, i8* } @FRes.close")
	// Should have close error check and propagation blocks
	codegentest.AssertContains(t, ir, "close.err.flag")
	codegentest.AssertContains(t, ir, "close.err.ret")
}

func TestUseNonFailableCloseNoCapture(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type NRes {
			int id;
			close(~this) { }
		}
		process!() {
			use r := NRes(id: 1);
			int x = r.id;
		}
	`)
	// Non-failable close should remain a void call — no error capture
	codegentest.AssertContains(t, ir, "call void @NRes.close")
	codegentest.AssertNotContains(t, ir, "close.err.flag")
}

func TestUseFailableCloseSuppressedOnRaise(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type EBase is error { string message; }
		type FRes2 {
			int id;
			close!(~this) { }
		}
		process!() {
			use r := FRes2(id: 1);
			raise EBase(message: "fail");
		}
	`)
	// Raise path should suppress close errors (no close.err.flag on that path)
	// The close call is still emitted but result discarded
	codegentest.AssertContains(t, ir, "call { i1, i8* } @FRes2.close")
}

func TestUseFailableCloseInNonFailableFunc(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type FRes3 {
			int id;
			close!(~this) { }
		}
		process() {
			use r := FRes3(id: 1);
			int x = r.id;
		}
	`)
	// Non-failable function: close errors suppressed, no capture allocas
	codegentest.AssertNotContains(t, ir, "close.err.flag")
}

// T0135: Suppressed close errors are dropped (not leaked)
func TestSuppressedCloseErrorDropped(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type FRes {
			int id;
			close!(~this) { raise error(message: "close err"); }
		}
		process!() {
			use r := FRes(id: 1);
			raise error(message: "body err");
		}
	`)
	// Error-in-flight path: close error should be dropped via __mod_std_error.drop
	codegentest.AssertContains(t, ir, "close.err.drop")
	codegentest.AssertContains(t, ir, "@__mod_std_error.drop")
}

// T0135: Non-failable function drops suppressed close errors
func TestNonFailableSuppressedCloseErrorDropped(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type FRes {
			int id;
			close!(~this) { }
		}
		process() {
			use r := FRes(id: 1);
		}
	`)
	// Non-failable function: close error is suppressed and dropped
	codegentest.AssertContains(t, ir, "close.err.drop")
}

// T0135: Duplicate close error is dropped when first error already captured
func TestDuplicateCloseErrorDropped(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type FRes {
			int id;
			close!(~this) { }
		}
		process!() {
			use a := FRes(id: 1);
			use b := FRes(id: 2);
		}
	`)
	// Multiple failable closes: second error should be dropped
	codegentest.AssertContains(t, ir, "close.err.drop.dup")
}

// T0135: Failable result capture registers error optional for drop
func TestFailableResultCaptureErrorDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fail!() { raise error(message: "test"); }
		test() {
			(val, err) := fail();
		}
	`)
	// Error optional should have a drop flag and be dropped at scope exit
	codegentest.AssertContains(t, ir, "err.dropflag")
	codegentest.AssertContains(t, ir, "optdrop.check")
}

func TestUseFailableCloseVirtualDispatchCapture(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Conn {
			int fd;
			close!() { }
		}
		type TcpConn is Conn {
			close!() { }
		}
		process!() {
			use c := Conn(fd: 3);
			int x = c.fd;
		}
	`)
	// Virtual dispatch + failable function: close error should be captured
	codegentest.AssertContains(t, ir, "@promise_vtable_Conn")
	codegentest.AssertContains(t, ir, "close.err.flag")
	codegentest.AssertContains(t, ir, "close.err.ret")
}

// Drop with early return in failable function
func TestDropWithEarlyReturnFailable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		work!() void {
			r := Resource(id: 42);
			return;
		}
		main() { }
	`)
	// drop() should be emitted before the return
	codegentest.AssertContains(t, ir, "call void @Resource.drop")
}

// Drop with raise: cleanup before error return
func TestDropWithRaise(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		fail!() void {
			r := Resource(id: 1);
			raise error(message: "oops");
		}
		main() { }
	`)
	// drop() should be emitted before the raise
	codegentest.AssertContains(t, ir, "call void @Resource.drop")
}

// Error propagation triggers scope cleanup
func TestDropErrorPropagateCleansUp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		risky!() int {
			return 42;
		}
		work!() int {
			r := Resource(id: 1);
			int val = risky()?^;
			return val + r.id;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "call void @Resource.drop")
	codegentest.AssertContains(t, ir, "define { i1, i64, i8* } @__user.work")
}

// T0086: Error types now get bindingFree at scope exit (previously excluded)
func TestBindingFreeErrorType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			error e = error("test");
			string msg = e.message;
		}
	`)
	codegentest.AssertContains(t, ir, "e.dropflag")
	codegentest.AssertContains(t, ir, "call void @pal_free")
}

// T0086: Raising a local error variable clears its drop flag before scope cleanup
func TestRaiseLocalErrorClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fail!() void {
			error e = error("boom");
			raise e;
		}
		main() { }
	`)
	// The error should get a drop flag
	codegentest.AssertContains(t, ir, "e.dropflag")
	// The drop flag should be cleared (store false) before scope cleanup
	codegentest.AssertContains(t, ir, "store i1 false")
}

// T0103: String temp cleanup on error propagation path
func TestStringTempCleanupOnErrorPath(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fail!() int {
			raise error(message: "fail");
		}
		use_both(string s, int x) int {
			return x;
		}
		work!() int {
			return use_both("hello".to_upper(), fail());
		}
		main() {}
	`)
	// Should have error-path temp cleanup blocks (T0103)
	codegentest.AssertContains(t, ir, "err.tmp.drop")
	codegentest.AssertContains(t, ir, "err.tmp.skip")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0091: Error types get synthesized drop (frees message string field + instance)
func TestSynthDropIncludesErrorTypes(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			error e = error(message: "fail");
		}
	`)
	// error gets a synthesized drop that frees its string message field
	codegentest.AssertContains(t, ir, "define void @__mod_std_error.drop")
}

// T0083/T0091: Caught error instances are dropped after handler blocks
func TestErrorHandlerDropsInstance(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fail!() int {
			raise error(message: "boom");
		}
		main() {
			int v = fail()? e => 0;
		}
	`)
	// Handler block should drop the error instance via synthesized drop
	codegentest.AssertContains(t, ir, "call void @__mod_std_error.drop")
}

// T0083/T0091/T0110: Typed error handler uses child type's drop for match path
func TestTypedErrorHandlerDropsInstance(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type IoError is error { int code; }
		fail!() int {
			raise IoError(code: 1, message: "io");
		}
		main() {
			int v = fail()? e is IoError { 0; } else { -1; };
		}
	`)
	// Match path drops via IoError.drop (resolves child type, T0110)
	codegentest.AssertContains(t, ir, "call void @IoError.drop")
	// Else path drops via base error.drop (unknown concrete type)
	codegentest.AssertContains(t, ir, "call void @__mod_std_error.drop")
}

// T0110: Error type synthesized drop includes string field drops
func TestErrorTypeSynthDropIncludesStringFields(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			error e = error(message: "fail");
		}
	`)
	// error.drop should call promise_string_drop for the message field
	codegentest.AssertContains(t, ir, "define void @__mod_std_error.drop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0110: Child error type drop frees child-specific string fields
func TestChildErrorTypeSynthDropFreesChildFields(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type NotFoundError is error { string key; }
		fail!() int {
			raise NotFoundError(key: "missing", message: "not found");
		}
		main() {
			int v = fail()? e is NotFoundError { 0; } else { -1; };
		}
	`)
	// NotFoundError.drop should be defined (synthesized)
	codegentest.AssertContains(t, ir, "define void @NotFoundError.drop")
	// Match path uses NotFoundError.drop, not error.drop
	codegentest.AssertContains(t, ir, "call void @NotFoundError.drop")
	// NotFoundError.drop should have 2 string drops (message + key)
	dropBody := codegentest.ExtractFunction(ir, "NotFoundError.drop")
	count := strings.Count(dropBody, "call void @promise_string_drop")
	if count < 2 {
		t.Errorf("expected at least 2 promise_string_drop calls in NotFoundError.drop (message + key), got %d\nBody:\n%s", count, dropBody)
	}
}

// T0110: Dup-on-field-access works for error types (prevents use-after-free)
func TestErrorFieldAccessDupsString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type NotFoundError is error { string key; }
		fail!() string {
			raise NotFoundError(key: "missing", message: "not found");
		}
		main() {
			string s = fail()? e is NotFoundError { e.key; } else { ""; };
		}
	`)
	// Accessing e.key in error handler should dup the string via string_new (copy)
	// dupString() calls promise_string_new to create a heap copy of the field data
	codegentest.AssertContains(t, ir, "strdup.copy")
}

// B0225: goroutine_exit frees G.panic_msg when panicked==2 (heap-allocated msg).
func TestGoroutineExitFreePanicMsg(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	// goroutine_exit should have free_panic_msg and do_free_g blocks
	codegentest.AssertContains(t, ir, "free_panic_msg:")
	codegentest.AssertContains(t, ir, "do_free_g:")
}

// B0228: promise_panic_msg stores type=2 in TLS panic_type to mark heap-allocated msg.
func TestPanicMsgSetsHeapPanickedFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		panic_msg(string msg) `+"`"+`extern("promise_panic_msg");
		main() { panic_msg("boom"); }
	`)
	// promise_panic_msg stores i8 2 in @__promise_panic_type (heap-allocated)
	codegentest.AssertContains(t, ir, "store i8 2, i8* @__promise_panic_type")
}

// B0228: promise_panic is no longer noreturn — call sites use ret instead of unreachable.
func TestPanicNotNoreturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {}
	`)
	// promise_panic should NOT have noreturn (other funcs like pal_exit still do)
	codegentest.AssertNotContains(t, ir, "declare void @promise_panic(i8*) noreturn")
	// The function body should end with ret void in the set_panic block
	codegentest.AssertContains(t, ir, "define void @promise_panic(i8*")
}

// B0228: promise_panic double-panic check aborts with exit code 134.
func TestPanicDoublePanicAbort(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {}
	`)
	// Double panic should load flag, compare, and branch to abort path
	codegentest.AssertContains(t, ir, "load i8, i8* @__promise_panic_flag")
	codegentest.AssertContains(t, ir, "call void @pal_exit(i32 134)")
}

// B0228: Category A — OOB panic returns instead of unreachable.
func TestOOBPanicReturns(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get(int[] v, int i) int { return v[i]; }
		main() { v := [1]; get(v, 0); }
	`)
	// The OOB panic block should call promise_panic then return (not unreachable)
	codegentest.AssertContains(t, ir, "call void @promise_panic(")
	// promise_panic declaration should NOT have noreturn (other funcs like pal_exit still do)
	codegentest.AssertNotContains(t, ir, "declare void @promise_panic(i8*) noreturn")
}

// T0147: Panic check emitted after every call expression.
func TestPanicCheckAfterCallExpr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		foo() {}
		main() { foo(); }
	`)
	// After the call to foo(), emitPanicCheck should emit:
	// - load of __promise_panic_flag
	// - icmp ne (check if flag is set)
	// - conditional branch to panic.cleanup / panic.ok
	codegentest.AssertContains(t, ir, "panic.cleanup")
	codegentest.AssertContains(t, ir, "panic.ok")
}

// T0147: Panic check after method call.
func TestPanicCheckAfterMethodCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo {
			int x;
			bar(this) int { return this.x; }
		}
		main() { f := Foo(x: 1); f.bar(); }
	`)
	codegentest.AssertContains(t, ir, "panic.cleanup")
	codegentest.AssertContains(t, ir, "panic.ok")
}

// T0147: Go-call (direct) coroutine has panic exit block.
func TestPanicCheckGoCallDirect(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		work() {}
		main() { go work(); }
	`)
	// genGoCallExpr should emit go.panic_exit block
	codegentest.AssertContains(t, ir, "go.panic_exit")
	codegentest.AssertContains(t, ir, "go.call_ok")
}

// T0148: genGoCallExprViaBlock has final panic check before final suspend.
func TestPanicCheckGoCallViaBlockFinal(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo {
			bar(this) {}
		}
		main() {
			f := Foo();
			go f.bar();
		}
	`)
	// genGoCallExprViaBlock should emit go.panic_exit block
	codegentest.AssertContains(t, ir, "go.panic_exit")
	// The coroutine body should have a final panic flag check (icmp ne + cond br)
	codegentest.AssertContains(t, ir, "@__promise_panic_flag")
}

func TestVariadicFailableIR(t *testing.T) {
	// Variadic + failable function in IR.
	ir := codegentest.GenerateIR(t, `
		trySum!(...int nums) int {
			if nums.len == 0 { raise error(message: "empty"); }
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		main() {
			x := trySum(1, 2, 3)?!;
		}
	`)
	// Failable returns {i1, i64, i8*} (error flag + result + error ptr)
	codegentest.AssertContains(t, ir, "define { i1, i64, i8* } @__user.trySum(i8* %nums)")
}

func TestVariadicVectorHeapTempOnFailableArg(t *testing.T) {
	// B0201: When a failable arg inside a variadic call fails, the vector
	// allocated for variadic args must be freed on the error path.
	ir := codegentest.GenerateIR(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		parse!(string s) int {
			raise error(message: s);
		}
		foo!() int {
			return sum(parse("a"), parse("b"));
		}
		main() { foo()?!; }
	`)
	// The variadic vector should be tracked as a heap temp (pal_alloc + store to alloca)
	// and freed on the error propagation path (err.heap.drop block calls pal_free)
	codegentest.AssertContains(t, ir, "err.heap.drop")
	codegentest.AssertContains(t, ir, "call void @pal_free")
}

// TestFailableSetterInstanceNonVirtualIR verifies that a `set name!(...)`
// setter call site auto-propagates the failable result in a failable enclosing
// function. T0708.
func TestFailableSetterInstanceNonVirtualIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo {
			int x;
			get count int { return this.x; }
			set count!(int v) {
				if v < 0 { raise error("negative"); }
				this.x = v;
			}
		}
		main!() {
			f := Foo(x: 0);
			f.count = -5;
		}
	`)
	// The setter is defined with a failable result type.
	codegentest.AssertContains(t, ir, "define { i1, i8* } @Foo.count$set")
	// Call site captures the result and routes through auto-propagation.
	codegentest.AssertContains(t, ir, "call { i1, i8* } @Foo.count$set")
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
	codegentest.AssertContains(t, ir, "extractvalue { i1, i8* }")
}

// TestFailableGlobalSetterIR — `global failable setter (T0703 + T0708).
func TestFailableGlobalSetterIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, "type Foo {\n"+
		"int x;\n"+
		"get count int `global { return 0; }\n"+
		"set count!(int v) `global { if v < 0 { raise error(\"neg\"); } }\n"+
		"}\n"+
		"main!() {\n"+
		"Foo.count = -5;\n"+
		"}\n")
	codegentest.AssertContains(t, ir, "define { i1, i8* } @Foo.count$set(i64 %v)")
	codegentest.AssertContains(t, ir, "call { i1, i8* } @Foo.count$set(i64")
	codegentest.AssertContains(t, ir, "auto.propagate")
}

// TestFailableIndexSetterIR — a failable []= method's call site auto-propagates.
func TestFailableIndexSetterIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box {
			int x;
			[](int k) int { return this.x; }
			[]=!(int k, int v) { if v < 0 { raise error("neg"); } this.x = v; }
		}
		main!() {
			b := Box(x: 0);
			b[0] = -5;
		}
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "extractvalue { i1, i8* }")
}

// TestFailableSliceSetterIR — a failable [:]= method's call site auto-propagates.
func TestFailableSliceSetterIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box {
			int x;
			[:](int? low, int? high) int { return this.x; }
			[:]=!(int? low, int? high, int v) { if v < 0 { raise error("neg"); } this.x = v; }
		}
		main!() {
			b := Box(x: 0);
			b[1:2] = -5;
		}
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "extractvalue { i1, i8* }")
}

// TestFailableSetterCompoundAssignIR — `f.count += v` reads via getter then
// writes via setter; if either is failable both must propagate. This test
// covers the setter side.
func TestFailableSetterCompoundAssignIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo {
			int x;
			get count int { return this.x; }
			set count!(int v) {
				if v < 0 { raise error("neg"); }
				this.x = v;
			}
		}
		main!() {
			f := Foo(x: 0);
			f.count += 5;
		}
	`)
	codegentest.AssertContains(t, ir, "call { i1, i8* } @Foo.count$set")
	codegentest.AssertContains(t, ir, "auto.propagate")
}

// TestFailableGetterCompoundAssignIR — `f.count += v` reads the current value
// via a failable getter; the read must auto-propagate and the arithmetic must
// operate on the extracted ok value, not the failable result struct. T0709.
func TestFailableGetterCompoundAssignIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo {
			int x;
			get count! int {
				if this.x < 0 { raise error("neg"); }
				return this.x;
			}
			set count(int v) { this.x = v; }
		}
		main!() {
			f := Foo(x: 0);
			f.count += 5;
		}
	`)
	// Getter is defined with a failable result type.
	codegentest.AssertContains(t, ir, "define { i1, i64, i8* } @Foo.count")
	// Read routes through auto-propagation and the op uses the ok payload (field 1).
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "auto.ok")
	codegentest.AssertContains(t, ir, "extractvalue { i1, i64, i8* }")
	// The arithmetic must NOT be applied to the raw failable struct (the old bug).
	codegentest.AssertNotContains(t, ir, "add { i1, i64, i8* }")
}

// TestFailableIndexGetterCompoundIR — `b[0] += v` reads via a failable [] getter
// (non-optional return), which must auto-propagate before the op. T0709.
func TestFailableIndexGetterCompoundIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box {
			int x;
			[]!(int k) int {
				if this.x < 0 { raise error("neg"); }
				return this.x;
			}
			[]=(int k, int v) { this.x = v; }
		}
		main!() {
			b := Box(x: 0);
			b[0] += 5;
		}
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "extractvalue { i1, i64, i8* }")
	codegentest.AssertNotContains(t, ir, "add { i1, i64, i8* }")
}

// TestFailableIndexGetterIncDecIR — `b[0]++` reads via a failable [] getter,
// which must auto-propagate before the increment. T0709.
func TestFailableIndexGetterIncDecIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box {
			int x;
			[]!(int k) int {
				if this.x < 0 { raise error("neg"); }
				return this.x;
			}
			[]=(int k, int v) { this.x = v; }
		}
		main!() {
			b := Box(x: 0);
			b[0]++;
		}
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "extractvalue { i1, i64, i8* }")
	codegentest.AssertNotContains(t, ir, "add { i1, i64, i8* }")
}

// TestFailableGetterCompoundInGenericMethodIR — a failable getter compound inside
// a monomorphized generic method body exercises unwrapFailableCompoundRead's
// typeSubst path (c.typeSubst != nil). The operand is a concrete int field, so
// the native += is valid while the method itself is mono'd per instantiation. T0709.
func TestFailableGetterCompoundInGenericMethodIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			T payload;
			int counter;
			get count! int {
				if this.counter < 0 { raise error("neg"); }
				return this.counter;
			}
			set count(int v) { this.counter = v; }
			bump!(~this) { this.count += 1; }
		}
		main!() {
			b := Box[int](payload: 0, counter: 10);
			b.bump();
		}
	`)
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "extractvalue { i1, i64, i8* }")
	codegentest.AssertNotContains(t, ir, "add { i1, i64, i8* }")
}

// TestFailablePropertyIncDecIR: T0712. With a failable getter and setter, the
// getter result is unwrapped (extractvalue from {i1, i64, i8*}) before the op,
// and the setter result auto-propagates — no malformed `add { i1` on the struct.
func TestFailablePropertyIncDecIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo {
			int x;
			get count! int { if this.x < 0 { raise error("neg"); } return this.x; }
			set count!(int v) { if v < 0 { raise error("neg"); } this.x = v; }
		}
		main!() {
			f := Foo(x: 0);
			f.count++;
		}
	`)
	codegentest.AssertContains(t, ir, "define { i1, i64, i8* } @Foo.count(")
	codegentest.AssertContains(t, ir, "extractvalue { i1, i64, i8* }")
	codegentest.AssertContains(t, ir, "call { i1, i8* } @Foo.count$set")
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertNotContains(t, ir, "add { i1")
}

func TestFailableGetterResultType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MyErr is error { int code; }
		type Foo {
			int _val;
			get value! int {
				if this._val < 0 { raise MyErr(code: 1, message: "neg"); }
				return this._val;
			}
		}
		main() {
			Foo f = Foo(_val: 42);
			int v = f.value?!;
		}
	`)
	// Failable getter should return result type {i1, i64, i8*}
	codegentest.AssertContains(t, ir, "define { i1, i64, i8* } @Foo.value(")
}

func TestFailableGetterVirtualDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MyErr is error { int code; }
		type Base {
			get value! int `+"`"+`abstract;
		}
		type Impl is Base {
			int _v;
			get value! int { return this._v; }
		}
		main() {
			Base b = Impl(_v: 10);
			int v = b.value?!;
		}
	`)
	// Abstract failable getter should use vtable dispatch
	codegentest.AssertContains(t, ir, "@promise_vtable_Base")
	codegentest.AssertContains(t, ir, "@promise_vtable_Impl")
	codegentest.AssertContains(t, ir, "define { i1, i64, i8* } @Impl.value(")
}

func TestFailableGetterStringResult(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MyErr is error { int code; }
		type Foo {
			int _mode;
			get label! string {
				if this._mode < 0 { raise MyErr(code: 1, message: "bad"); }
				return "ok";
			}
		}
		main() {
			Foo f = Foo(_mode: 1);
			string s = f.label?!;
		}
	`)
	// Failable getter returning string should have result type in signature
	codegentest.AssertContains(t, ir, "define { i1, i8*, i8* } @Foo.label(")
}

func TestEnumMethodFailable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Mode { A, B,
			check!(this) string {
				match this {
					Mode.A => { return "a"; },
					Mode.B => { return "b"; },
				}
			}
		}
		main() { string s = Mode.A.check()?!; }
	`)
	// Failable method returns result struct
	codegentest.AssertContains(t, ir, "@Mode.check(i8* %this)")
}

// T1172: a fixed-array-returning function whose body contains a panic-capable
// operation (string concat) reaches the panic-cleanup return (emitPanicReturn),
// which must emit the array zero aggregate — NOT the i64-0 default that produced
// malformed `ret i64 0` in a `[N x T]`-returning function.
func TestT1172ArrayReturnPanicCleanupZeroValue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum ArrHolder { Pair(string[2] a), Empty }
		mk() string[2] {
			ArrHolder h = ArrHolder.Pair(a: ["x" + "1", "y" + "2"]);
			return ["z", "w"];
		}
		main() { a := mk(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.mk")
	// The panic-cleanup path returns a zeroinitializer of the array type, never
	// a bare i64 0.
	codegentest.AssertContains(t, fn, "ret [2 x i8*] zeroinitializer")
	codegentest.AssertNotContains(t, fn, "ret i64 0")
}

func TestIsGenericErrorHandler(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type AppError[T] is error { T detail; }
		do_thing!() AppError[int] {
			raise AppError[int](message: "err", detail: 42);
		}
		main() {
			do_thing() ? e is AppError[int] {
			}!;
		}
	`)
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is")
	codegentest.AssertContains(t, ir, "promise_typeinfo_AppError")
}

// B0134: generic error type constructor inside generic function body
// must be collected for monomorphization via func instance substitution.
func TestGenericErrorTypeInGenericFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type AppError[T] is error { T detail; }
		make_err![T](T detail) AppError[T] {
			raise AppError[T](message: "fail", detail: detail);
		}
		main() { make_err[int](42) ? e { }; }
	`)
	// B0134: AppError[int] must be monomorphized from the generic function body
	codegentest.AssertContains(t, ir, "AppError[int]")
}

func TestT1073RaiseForceUnwrapNeutralizesSource(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1073Err is error { string d; }
		rz!(T1073Err? move o) int { raise o!; }
		main() {}
	`)
	fn := codegentest.ExtractFunction(ir, "__user.rz")
	if fn == "" {
		t.Fatal("expected __user.rz in IR")
	}
	codegentest.AssertContains(t, fn, codegentest.T1073NeutralizeSig)
}

// B0262: Discarded auto-propagated failable call returning heap user type should drop+free.
func TestDropDiscardedAutoPropagateUserType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo {
			string name;
			drop(~this) {}
		}
		make_foo!() Foo { return Foo(name: "x"); }
		test!() {
			make_foo();
		}
	`)
	testFn := codegentest.ExtractFunction(ir, "__user.test")
	codegentest.AssertContains(t, testFn, "autoprop.drop")
	codegentest.AssertContains(t, testFn, "call void @Foo.drop")
}

// B0262: Discarded auto-propagated failable call returning closure should free env.
func TestDropDiscardedAutoPropagateClosureEnv(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_fn!() (int) -> int {
			int x = 42;
			(int) -> int f = |int y| -> x + y;
			return f;
		}
		test!() {
			make_fn();
		}
	`)
	testFn := codegentest.ExtractFunction(ir, "__user.test")
	codegentest.AssertContains(t, testFn, "autoprop.env.free")
}

// B0226: Untyped error handler should use RTTI-based drop dispatch.
func TestUntypedErrorRttiDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MyError is error { int code; }
		fail_my!() void { raise MyError(message: "err", code: 42); }
		main() {
			fail_my()? e {
			};
		}
	`)
	// B0226: Should emit RTTI-based drop dispatch (loads drop fn from typeinfo)
	codegentest.AssertContains(t, ir, "rtti.drop")
}

// B0325: Field access on a ?! unwrap result must track the intermediate heap instance.
func TestFieldAccessOnErrorPanicResultTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pair { int x; int y; }
		make_pair!() Pair { return Pair(x: 10, y: 20); }
		test!() {
			int v = make_pair()?!.x;
		}
	`)
	codegentest.AssertContains(t, ir, "heap.drop")
	codegentest.AssertContains(t, ir, "call void @pal_free(")
}

// B0325: Method call on a ?! unwrap result must track the intermediate heap instance.
func TestMethodCallOnErrorPanicResultTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pair { int x; int y;
			sum(this) int { return this.x + this.y; }
		}
		make_pair!() Pair { return Pair(x: 10, y: 20); }
		test!() {
			int v = make_pair()?!.sum();
		}
	`)
	codegentest.AssertContains(t, ir, "heap.drop")
	codegentest.AssertContains(t, ir, "call void @pal_free(")
}

// TestT0545_GenericIndirectionBackstopNoPanic was retired: T0616 now rejects
// generic-indirection clone of a single-owner handle at the call site (sema),
// so the codegen backstop path the test exercised is unreachable for
// well-formed user code. The compile-fail behavior is verified by
// TestT0616_VectorCloneInGenericTaskError in the sema package.

// TestT0482_NestedHandleBackstopNoPanic pins the codegen backstop for the
// nested-Named-handle shape behind generic indirection: a generic function
// clones a Vector[T] and is instantiated with a user type that owns a Task
// through a field. Direct user code is now gated by the T0482 sema predicate
// (firstNestedSingleOwnerHandle), but a generic body is checked with unbound
// T, so the concrete Holder reaches dupHeapValueFields at codegen. The
// isOpaqueContainerType skip (compiler.go:dupHeapValueFields) must degrade the
// nested Task field to a shallow copy without a Go panic (the recursive
// dupHeapValue would otherwise bitcast an i8* handle to a struct type and
// crash). The residual runtime double-free behind generic indirection is the
// separate, tracked T0616 — this test only generates IR, never runs it.
func TestT0482_NestedHandleBackstopNoPanic(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		worker_int() int { return 42; }
		type Holder { Task[int] t; }
		dup_holders[T](Vector[T] v) Vector[T] { return v.clone(); }
		test_nh() {
			v := Vector[Holder]();
			v.push(Holder(t: go worker_int()));
			v2 := dup_holders[Holder](v);
		}
	`)
	// The Holder-instantiated generic dup function must be emitted (proves the
	// nested-handle backstop path was exercised in codegen, not skipped).
	codegentest.AssertContains(t, ir, "dup_holders[Holder]")
}

// T1634: an indirect (closure) call to a *failable* function type must type the
// function pointer with the callee's result struct — `{ i1, T, i8* }` — not the
// bare `T` from sig.Result(). The thunk copies the real function's RetType, so
// building the pointer from sig.Result() alone made the two disagree: the call
// was typed to return `i64` while the callee returned `{ i1, i64, i8* }`, and
// unwrapping it (`?^`) panicked codegen with "types.Type is *types.IntType,
// not *types.StructType".
func TestT1634_FailableFunctionTypeIndirectCallResultShape(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		boom!(int x) int { return x * 2; }
		test_indirect!() {
			!(int) -> int f = boom;
			int r = f(3)?^;
		}
	`)
	// The function pointer the indirect call is bitcast to must carry the
	// failable result struct, with the env pointer as the first parameter.
	codegentest.AssertContains(t, ir, "{ i1, i64, i8* } (i8*, i64)*")
}

// T1634: `!() -> void` — a failable function with nothing to return uses the
// two-field result struct `{ i1, i8* }`.
func TestT1634_FailableVoidFunctionTypeIndirectCallResultShape(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		vboom!() { }
		test_indirect_void!() {
			!() -> void v = vboom;
			v()?^;
		}
	`)
	codegentest.AssertContains(t, ir, "{ i1, i8* } (i8*)*")
}

// T1634: a non-failable function type is unaffected — its indirect call still
// returns the bare result type.
func TestT1634_NonFailableFunctionTypeIndirectCallResultShape(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test_plain() {
			(int) -> int f = |int x| -> x * 2;
			int r = f(3);
		}
	`)
	codegentest.AssertContains(t, ir, "i64 (i8*, i64)*")
	codegentest.AssertNotContains(t, ir, "{ i1, i64, i8* } (i8*, i64)*")
}
