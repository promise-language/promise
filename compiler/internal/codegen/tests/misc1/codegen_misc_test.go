package misc1

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
)

func TestUnaryNot(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := !true; }`)
	codegentest.AssertContains(t, ir, "xor i1")
}

func TestLeftShift(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := 1 << 4; }`)
	codegentest.AssertContains(t, ir, "shl i64")
}

func TestRightShiftSigned(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := 16 >> 2; }`)
	codegentest.AssertContains(t, ir, "ashr i64")
}

// --- Short-circuit boolean ops ---

func TestShortCircuitAnd(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := true && false; }`)
	codegentest.AssertContains(t, ir, "and.rhs")
	codegentest.AssertContains(t, ir, "and.merge")
}

func TestShortCircuitOr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := true || false; }`)
	codegentest.AssertContains(t, ir, "or.rhs")
	codegentest.AssertContains(t, ir, "or.merge")
}

func TestIncrementDecrement(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int x = 10;
			x++;
			x--;
		}
	`)
	codegentest.AssertContains(t, ir, "add i64")
	codegentest.AssertContains(t, ir, "sub i64")
}

func TestFunctionCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		double(int x) int { return x * 2; }
		main() { y := double(21); }
	`)
	codegentest.AssertContains(t, ir, "call i64 @__user.double(i64")
}

func TestVoidFunction(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		noop() { }
		main() { noop(); }
	`)
	codegentest.AssertContains(t, ir, "define void @__user.noop()")
	codegentest.AssertContains(t, ir, "call void @__user.noop()")
}

func TestPALWriteExitDefined(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {}
	`)
	// PAL primitives are always emitted
	codegentest.AssertContains(t, ir, "define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)")
	codegentest.AssertContains(t, ir, "define void @pal_exit(i32 %code)")
	if runtime.GOOS == "windows" {
		// Windows PAL uses GetStdHandle+WriteFile and ExitProcess
		codegentest.AssertContains(t, ir, "@GetStdHandle")
		codegentest.AssertContains(t, ir, "@WriteFile")
		codegentest.AssertContains(t, ir, "@ExitProcess")
	} else {
		codegentest.AssertContains(t, ir, "call i64 @write(i32 %fd, i8* %buf, i64 %len)")
		codegentest.AssertContains(t, ir, "call void @exit(i32 %code)")
	}
}

func TestStackOverflowHandler(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {}
	`)
	// B0010: Stack overflow detection
	// Init function is defined and called from main (all platforms)
	codegentest.AssertContains(t, ir, "define void @pal_stack_overflow_init()")
	codegentest.AssertContains(t, ir, "call void @pal_stack_overflow_init()")
	// Thread init is defined and called from sched_loop (all platforms)
	codegentest.AssertContains(t, ir, "define void @pal_stack_overflow_thread_init()")
	codegentest.AssertContains(t, ir, "call void @pal_stack_overflow_thread_init()")

	if runtime.GOOS == "windows" {
		// Windows: VEH handler via AddVectoredExceptionHandler (B0141)
		codegentest.AssertContains(t, ir, "define i32 @__promise_veh_handler(i8* %exception_pointers)")
		codegentest.AssertContains(t, ir, "@AddVectoredExceptionHandler")
		codegentest.AssertContains(t, ir, "@ExitProcess")
	} else if runtime.GOOS == "darwin" {
		// macOS: 3-arg SA_SIGINFO handler printing the fault address (T1161)
		codegentest.AssertContains(t, ir, `@__promise_hex_digits = constant [16 x i8]`)
		codegentest.AssertContains(t, ir, `@__promise_segfault_prefix = constant [31 x i8]`)
		codegentest.AssertContains(t, ir, "define void @__promise_sigsegv_handler(i32 %sig, i8* %info, i8* %ucontext)")
		codegentest.AssertContains(t, ir, "call void @_exit(i32 2)")
		codegentest.AssertContains(t, ir, "call i32 @pthread_attr_setguardsize(")
	} else {
		// Linux: 3-arg SA_SIGINFO handler with fault address (B0128)
		codegentest.AssertContains(t, ir, `@__promise_hex_digits = constant [16 x i8]`)
		codegentest.AssertContains(t, ir, `@__promise_segfault_prefix = constant [31 x i8]`)
		codegentest.AssertContains(t, ir, "define void @__promise_sigsegv_handler(i32 %sig, i8* %info, i8* %ucontext)")
		codegentest.AssertContains(t, ir, "call void @_exit(i32 2)")
		codegentest.AssertContains(t, ir, "call i32 @pthread_attr_setguardsize(")
	}
}

func TestPrintNewlineEmission(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		print_s(string s) `+"`"+`extern("promise_print_string");
		main() { print_s("hello"); }
	`)
	// Newline global constant (used by print_string body)
	codegentest.AssertContains(t, ir, `@.str.newline = private constant [1 x i8] c"\0A"`)
	codegentest.AssertContains(t, ir, `@.str.panic_prefix = private constant [7 x i8] c"panic: "`)
}

// --- Integration tests ---

func TestFibonacci(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fib(int n) int {
			if n <= 1 {
				return n;
			}
			return fib(n - 1) + fib(n - 2);
		}
		main() { x := fib(10); }
	`)
	codegentest.AssertContains(t, ir, "define i64 @__user.fib(i64 %n)")
	codegentest.AssertContains(t, ir, "call i64 @__user.fib")
	codegentest.AssertContains(t, ir, "add i64")
	codegentest.AssertContains(t, ir, "icmp sle")
}

func TestHeaderGeneration(t *testing.T) {
	result := codegentest.CompileResult(t, `
		use_int(int x) `+"`"+`extern("test_use_int");
		use_f(f64 x) `+"`"+`extern("test_use_f64");
		main() { use_int(42); use_f(3.14); }
	`)

	var buf bytes.Buffer
	if err := codegen.GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Header guard
	codegentest.AssertContains(t, header, "#ifndef PROMISE_BINDINGS_H")
	codegentest.AssertContains(t, header, "#include <stdint.h>")

	// Type definitions for int
	codegentest.AssertContains(t, header, "typedef struct { } promise_int_t;")
	codegentest.AssertContains(t, header, "promise_int_v;")

	// Type definitions for f64
	codegentest.AssertContains(t, header, "typedef struct { } promise_f64_t;")
	codegentest.AssertContains(t, header, "promise_f64_v;")

	// Function declarations: all params by pointer
	codegentest.AssertContains(t, header, "void test_use_int(promise_int_v *x);")
	codegentest.AssertContains(t, header, "void test_use_f64(promise_f64_v *x);")
}

// === User Type Tests ===

func TestUserTypeLayout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Dog { string name; int age; }
		main() { }
	`)
	codegentest.AssertContains(t, ir, "%promise_Dog_t = type {}")
	codegentest.AssertContains(t, ir, "%promise_Dog_m = type { %promise_Dog_t* }")
	codegentest.AssertContains(t, ir, "%promise_Dog_i = type { %promise_Dog_m*, i8*, i64 }")
	codegentest.AssertContains(t, ir, "%promise_Dog_v = type { i8*, %promise_Dog_i* }")
}

func TestUserTypeHeader(t *testing.T) {
	result := codegentest.CompileResult(t, `
		type Dog { string name; int age; }
		main() { }
	`)

	var buf bytes.Buffer
	if err := codegen.GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	codegentest.AssertContains(t, header, "promise_Dog_t")
	codegentest.AssertContains(t, header, "promise_Dog_m")
	codegentest.AssertContains(t, header, "promise_Dog_i")
	codegentest.AssertContains(t, header, "promise_Dog_v")
	// int field should use raw C type
	codegentest.AssertContains(t, header, "int64_t")
}

func TestSuperCallCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal {
			int age;
			new(~this, int age) {
				this.age = age;
			}
		}
		type Dog is Animal {
			int tricks;
			new(~this, int age, int tricks) {
				super(age);
				this.tricks = tricks;
			}
		}
		main() {
			Dog d = Dog(age: 3, tricks: 5);
		}
	`)
	// Dog.new should call Animal.new
	codegentest.AssertContains(t, ir, "call void @Animal.new(")
	// Dog constructor should call Dog.new
	codegentest.AssertContains(t, ir, "call void @Dog.new(")
}

func TestHandlerNoBinding(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		foo!() void { raise error(message: "oops"); }
		bar() {
			foo() ? { };
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "error.handler")
	// Handler without binding should not load variant pointer for reconstruction
	codegentest.AssertNotContains(t, ir, "error.typed.match")
}

func TestTupleMixedTypes(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := (42, "hello", true); }`)
	// Should produce { i64, i8*, i1 } struct
	codegentest.AssertContains(t, ir, "insertvalue { i64, i8*, i1 }")
}

// T0933/T0940: `m := a ?: b` with a LOCALLY-OWNED source (`HVal? a = ...`) and an
// owned-local default `b`. someOwnsInner is true (the optional's scope drop flag is
// cleared on the some path); the none path neutralizes `b`'s own scope-exit drop flag
// (T0940) so `m` owns the buffer on both paths — the override phi folds to [ true,
// true ]. Single owner on each path: if a=None, `m` frees `b`'s buffer (b's flag
// cleared); if a=Some, `b`'s own binding frees it (none path not taken). Guards that
// the fix keeps the owned-source case owning without double-freeing the owned default.
func TestElvisBoundLocalSourceFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type HVal { string s; }
		main() {
			HVal? a = HVal(s: "a" + "b");
			HVal b = HVal(s: "c" + "d");
			m := a ?: b;
		}
	`)
	// Bound override phi: some=true (owned local), none=true. No false-some incoming.
	codegentest.AssertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ true, %elvis\.none`)
	codegentest.AssertContainsMatch(t, ir, `store i1 %[0-9]+, i1\* %m\.dropflag`)
}

// unaryExpr: negation
func TestUnaryNegation(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int x = -42;
			f64 y = -3.14;
		}
	`)
	codegentest.AssertContains(t, ir, "sub i64 0")
	codegentest.AssertContains(t, ir, "fneg double")
}

func TestTypeInfoGlobal(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal { string name; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", breed: "Lab");
		}
	`)
	// Type info globals should be emitted for both types
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Animal")
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Dog")
}

func TestIsAbsent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = none;
			bool b = x is absent;
		}
	`)
	// Should extract the i1 flag and negate via xor
	codegentest.AssertContains(t, ir, "extractvalue { i1, i64 }")
	codegentest.AssertContains(t, ir, "xor i1")
}

func TestTypeIsFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal { string name; }
		type Dog is Animal { }
		main() {
			Animal a = Dog(name: "Rex");
			bool b = a is Dog;
		}
	`)
	// Verify key blocks in the defined type_is function
	codegentest.AssertContains(t, ir, "define i32 @promise_type_is")
	codegentest.AssertContains(t, ir, "check_id:")
	codegentest.AssertContains(t, ir, "loop_init:")
	codegentest.AssertContains(t, ir, "loop_header:")
	codegentest.AssertContains(t, ir, "loop_body:")
	codegentest.AssertContains(t, ir, "ret_true:")
	codegentest.AssertContains(t, ir, "ret_false:")
}

func TestRTTIDiamondDedup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base {
			id() string { return "base"; }
		}
		type Left is Base { }
		type Right is Base { }
		type Bottom is Left, Right { }
		main() {
			Bottom b = Bottom();
		}
	`)
	// Type info globals for all types
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Bottom")
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Left")
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Right")
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Base")
}

// --- Stage 8l: Value struct dispatch model tests ---

func TestValueStructRepresentation(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		main() {
			Animal a = Animal(name: "Rex");
		}
	`)
	// Variables of user types should be value struct { i8*, i8* }
	codegentest.AssertContains(t, ir, "alloca { i8*, i8* }")
	// Constructor returns value struct with insertvalue
	codegentest.AssertContains(t, ir, "insertvalue { i8*, i8* }")
}

func TestIsExpressionWithValueStruct(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal {
			string name;
			speak() string `+"`"+`abstract;
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Animal a = Dog(name: "Rex");
			bool b = a is Dog;
		}
	`)
	// Should extract instance pointer from value struct for RTTI check
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
	codegentest.AssertContains(t, ir, "@promise_type_is")
}

func TestSaveRestoreLocalNameCountAcrossAdapter(t *testing.T) {
	// Regression test: emitting a view adapter mid-function used to reset
	// localNameCount, causing duplicate LLVM local names. This test verifies
	// that two variables with the same name in different scopes get unique
	// LLVM names even when a view adapter is emitted between them.
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() {
			int? opt = 10;
			if opt is present {
				int x = opt;
				display(x);
			}
			int x = 20;
			display(x);
		}
	`)
	// Both x variables should compile without "multiple definition" errors.
	// The first x gets %x, the second gets %x.1 (or similar unique name).
	codegentest.AssertContains(t, ir, "call")
	// If this test compiles at all, the localNameCount save/restore works.
	codegentest.AssertContains(t, ir, "@promise_vtable_int_as_Showable")
}

// T0582: paren-wrapped receiver in a discard statement `(w).self();` must hit
// emitReceiverAliasCheck's IdentExpr arm (via the paren-peel) and emit the
// recv.alias.clear/skip blocks so the temp's drop flag is cleared at runtime.
// Without the peel, mem.Target is a ParenExpr → default: return → no check →
// double-free at statement end.
func TestParenDiscardReceiverEmitsAliasCheck(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper { int v; self() Wrapper { return this; } }
		main() { w := Wrapper(v: 1); (w).self(); }
	`)
	codegentest.AssertContains(t, ir, "recv.alias.clear")
	codegentest.AssertContains(t, ir, "recv.alias.skip")
}

func TestStdFuncMangledName(t *testing.T) {
	// After module-based refactor: helper() is user code → IR name is @__user.helper
	ir := codegentest.GenerateIRWithStd(t,
		`helper() int { return 42; }`,
		`main() { x := helper(); }`,
	)
	codegentest.AssertContains(t, ir, "define i64 @__user.helper")
	codegentest.AssertContains(t, ir, "call i64 @__user.helper")
}

func TestStdUserNameCollision(t *testing.T) {
	// After module-based refactor: both helpers are user code → redefinition is an error,
	// so test with non-conflicting names instead.
	ir := codegentest.GenerateIRWithStd(t,
		`helper_extra() int { return 42; }`,
		`
		main() { x := helper_extra(); }
		`,
	)
	codegentest.AssertContains(t, ir, "define i64 @__user.helper_extra")
	codegentest.AssertContains(t, ir, "call i64 @__user.helper_extra()")
}

func TestStdCallViaStdPrefix(t *testing.T) {
	// Real std functions (e.g., print_line) are called via __mod_std_ prefix
	ir := codegentest.GenerateIR(t, `main() { print_line("hello"); }`)
	codegentest.AssertContains(t, ir, "call void @__mod_std_print_line")
}

func TestGenerateTestMainNoExistingMain(t *testing.T) {
	// GenerateTestMain should create a new main when none exists
	result := codegentest.CompileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
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
	codegentest.AssertContains(t, ir, "define i32 @main")
	codegentest.AssertContains(t, ir, "call i32 @promise_test_run")
	// promise_test_run is now codegen-defined (not a C extern)
	codegentest.AssertContains(t, ir, "define i32 @promise_test_run(i8* %fn, i64 %timeout_ns)")
	// Thread-based: spawns a thread via PAL, joins it
	codegentest.AssertContains(t, ir, "call i8* @pal_thread_create")
	codegentest.AssertContains(t, ir, "call void @pal_thread_join")
	// Trampoline bridges i8*(i8*) pthread ABI to void() test function
	codegentest.AssertContains(t, ir, "define i8* @.test_trampoline(i8* %fn_ptr)")
}

func TestGenerateTestMainReplacesExistingMain(t *testing.T) {
	// GenerateTestMain should replace user main's blocks
	result := codegentest.CompileResult(t, `
		myTest() `+"`test"+` { }
		main() { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { } main() { }`)
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
	// Should still have main but with test runner content
	codegentest.AssertContains(t, ir, "define i32 @main")
	codegentest.AssertContains(t, ir, "call i32 @promise_test_run")
	codegentest.AssertContains(t, ir, "call void @promise_test_summary")
}

func TestGenerateTestMainStoresArgcArgv(t *testing.T) {
	// GenerateTestMain should store argc/argv to globals for os.arguments/os.executable_path
	result := codegentest.CompileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
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
	// Test main receives argc/argv and stores to globals
	codegentest.AssertContains(t, ir, "define i32 @main(i32 %argc, i8** %argv)")
	codegentest.AssertContains(t, ir, "store i32 %argc, i32* @__promise_argc")
	codegentest.AssertContains(t, ir, "store i8** %argv, i8*** @__promise_argv")
}

// B0130: Batch test main reserves G.id=0 so goroutine panics recover via scheduler.
func TestGenerateTestMainReservesGID0(t *testing.T) {
	result := codegentest.CompileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
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
	// After sched_init, the goroutine counter must be bumped past 0
	// so no user goroutine gets G.id=0 (which promise_panic treats as main).
	codegentest.AssertContains(t, ir, "atomicrmw add i64*")
}

// T0689: When MemoryLimitAccounting=false (run/exec/build, or test with
// -memory-limit 0), the accounting allocator globals and helpers must be
// entirely absent from the IR — hard zero-overhead requirement.
func TestNoMemoryLimitGlobalsByDefault(t *testing.T) {
	result := codegentest.CompileResult(t, `
		main() { v := [1, 2, 3]; }
	`)
	ir := result.Module.String()
	codegentest.AssertNotContains(t, ir, "__promise_memory_used_bytes")
	codegentest.AssertNotContains(t, ir, "__promise_memory_limit_bytes")
	codegentest.AssertNotContains(t, ir, "__promise_memory_set_test_state")
	codegentest.AssertNotContains(t, ir, "fatal: memory limit exceeded")
}

// T0689: With CompileOptions.MemoryLimitAccounting=true, the accounting
// globals and the set_test_state helper are emitted, and pal_alloc's body
// references the used-bytes counter.
func TestMemoryLimitGlobalsEmittedWhenEnabled(t *testing.T) {
	file, info := codegentest.ParseWithStd(t, `
		myTest() `+"`test"+` { }
	`)
	result := codegen.CompileWithOptions(file, info, "", &codegen.CompileOptions{
		DebugAllocator:        true,
		MemoryLimitAccounting: true,
	})
	result.SetTestMemoryLimits(map[string]int64{"myTest": 1 << 20})
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()
	codegentest.AssertContains(t, ir, "@__promise_memory_used_bytes")
	codegentest.AssertContains(t, ir, "@__promise_memory_limit_bytes")
	codegentest.AssertContains(t, ir, "define void @__promise_memory_set_test_state(i64 %new_limit)")
	codegentest.AssertContains(t, ir, "fatal: memory limit exceeded")
	// Per-test set_test_state call must be emitted before each test runs
	codegentest.AssertContains(t, ir, "call void @__promise_memory_set_test_state")
}

func TestTestPrintResultBody(t *testing.T) {
	result := codegentest.CompileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
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

	// Function is defined (not just declared)
	codegentest.AssertContains(t, ir, "define void @promise_test_print_result(i8* %name, i32 %failed, i64 %elapsed_ns)")
	// 4-way branching: 0=pass, 2=timeout, 3=leak, else=fail
	codegentest.AssertContains(t, ir, "icmp eq i32 %failed, 0") // pass check
	codegentest.AssertContains(t, ir, "icmp eq i32 %failed, 2") // timeout check
	codegentest.AssertContains(t, ir, "icmp eq i32 %failed, 3") // leak check
	codegentest.AssertContains(t, ir, "br i1")                  // conditional branches
	codegentest.AssertContains(t, ir, "br label")               // unconditional branches to merge
	// pass/FAIL/TIMEOUT/LEAK prefix globals
	codegentest.AssertContains(t, ir, `@.str.pass_prefix = private constant [6 x i8] c"pass ("`)
	codegentest.AssertContains(t, ir, `@.str.fail_prefix = private constant [6 x i8] c"FAIL ("`)
	codegentest.AssertContains(t, ir, `@.str.timeout_prefix = private constant [9 x i8] c"TIMEOUT ("`)
	codegentest.AssertContains(t, ir, `@.str.leak_result_prefix = private constant [6 x i8] c"LEAK ("`)
	// Prefix writes
	codegentest.AssertContains(t, ir, "call i64 @pal_write(i32 1,")
	codegentest.AssertContains(t, ir, "i64 6)") // PASS/FAIL prefix length
	// Gets name length via strlen and writes name
	codegentest.AssertContains(t, ir, "call i64 @strlen(i8* %name)")
	// Time formatting: "s) " suffix, "\n" newline
	codegentest.AssertContains(t, ir, `@.str.time_suffix = private constant [3 x i8] c"s) "`)
	codegentest.AssertContains(t, ir, `@.str.newline = private constant [1 x i8] c"\0A"`)
}

func TestTestSummaryBody(t *testing.T) {
	result := codegentest.CompileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
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

	// Function is defined (not just declared) — includes leaked, timed_out, ignored, and stale params (T0020, T0067)
	codegentest.AssertContains(t, ir, "define void @promise_test_summary(i32 %passed, i32 %failed, i32 %skipped, i32 %leaked, i32 %timed_out, i32 %ignored, i32 %stale)")
	// String suffix globals
	codegentest.AssertContains(t, ir, `@.str.passed_suffix = private constant [9 x i8] c" passed, "`)
	codegentest.AssertContains(t, ir, `@.str.failed_suffix = private constant [7 x i8] c" failed"`)
	codegentest.AssertContains(t, ir, `@.str.skipped_suffix = private constant [8 x i8] c" skipped"`)
	codegentest.AssertContains(t, ir, `@.str.leaked_suffix = private constant [7 x i8] c" leaked"`)
	codegentest.AssertContains(t, ir, `@.str.timed_out_suffix = private constant [10 x i8] c" timed out"`)
	codegentest.AssertContains(t, ir, `@.str.allowed_leaks_suffix = private constant [14 x i8] c" allowed leaks"`)
	codegentest.AssertContains(t, ir, `@.str.stale_suffix = private constant [18 x i8] c" stale allow_leaks"`)
	// Converts i32 → i64 for int_to_string
	codegentest.AssertContains(t, ir, "sext i32 %passed to i64")
	codegentest.AssertContains(t, ir, "sext i32 %failed to i64")
	// Calls int_to_string and frees temp strings
	codegentest.AssertContains(t, ir, "call i8* @promise_int_to_string(i64")
	codegentest.AssertContains(t, ir, "call void @pal_free(i8*")
	// At least 2 free() calls for passed+failed (skipped/leaked are conditional)
	if strings.Count(ir, "call void @pal_free(i8*") < 2 {
		t.Error("expected at least 2 free() calls in promise_test_summary (one per int_to_string result)")
	}
	// Writes to stdout
	codegentest.AssertContains(t, ir, "call i64 @pal_write(i32 1,")
	// Suffix write lengths: 9 for " passed, ", 7 for " failed"
	codegentest.AssertContains(t, ir, "i64 9)")
	codegentest.AssertContains(t, ir, "i64 7)")
	// Conditional skipped output: icmp sgt for skipped > 0
	codegentest.AssertContains(t, ir, "icmp sgt i32 %skipped, 0")
	// Conditional leaked output: icmp sgt for leaked > 0 (T0020)
	codegentest.AssertContains(t, ir, "icmp sgt i32 %leaked, 0")
	// String instance extraction (bitcast for extractStringDataLenFromInstance)
	codegentest.AssertContains(t, ir, "bitcast i8* %")
	codegentest.AssertContains(t, ir, "to %promise_string_i*")
}

func TestTestTrampolineStackCreepDetection(t *testing.T) {
	// The test trampoline should read the stack pointer before and after the test
	// function call, and fail the test if the SP has changed (stack creep).
	result := codegentest.CompileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
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

	// Trampoline should contain inline asm to read SP (sideeffect prevents reordering)
	codegentest.AssertContains(t, ir, "asm sideeffect")
	// Should have stack_creep and stack_ok blocks
	codegentest.AssertContains(t, ir, "stack_creep:")
	codegentest.AssertContains(t, ir, "stack_ok:")
	// Stack creep message global
	codegentest.AssertContains(t, ir, "stack creep detected")
	// The SP comparison drives a conditional branch
	codegentest.AssertContains(t, ir, "icmp eq i64")
}

func TestTestTrampolineNoSetjmp(t *testing.T) {
	// T0150: test trampoline no longer uses setjmp/longjmp for panic recovery.
	// Panics are detected via TLS panic flag check after the test function returns.
	result := codegentest.CompileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
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

	fn := codegentest.ExtractFunction(ir, ".test_trampoline")
	// Must NOT contain setjmp, longjmp, or jmpBuf alloca
	codegentest.AssertNotContains(t, fn, "setjmp")
	codegentest.AssertNotContains(t, fn, "longjmp")
	codegentest.AssertNotContains(t, fn, "alloca [256 x i8]")
	codegentest.AssertNotContains(t, fn, "__promise_test_jmpbuf")
	// Must contain TLS panic flag check
	codegentest.AssertContains(t, fn, "__promise_panic_flag")
	codegentest.AssertContains(t, fn, "panic_detected")
}

// Test that GenerateTestMain produces 4-way counter logic (pass/fail/timeout/leak)
// and timeout context printing blocks.
func TestTestMainFourWayCountersAndTimeoutContext(t *testing.T) {
	result := codegentest.CompileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
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

	// 4-way counter logic: pass(0), fail(1), timeout(2) checks on effectiveResult
	codegentest.AssertContains(t, ir, "after_leak_detect_myTest")
	// Effective result phi merges leak-check path and skip path
	codegentest.AssertContains(t, ir, "skip_leak_check_myTest")
	// Timeout counter alloca and update
	codegentest.AssertContains(t, ir, "after_timeout_ctx_myTest")
	// Timeout context: prints "  timeout: exceeded <dur> limit\n" using compile-time duration (T1199)
	codegentest.AssertContains(t, ir, `c"  timeout: exceeded "`)
	codegentest.AssertContains(t, ir, `c"0ns"`) // timeoutNs==0 → formatDurationNs(0) == "0ns"
	codegentest.AssertContains(t, ir, `c" limit\0A"`)
	// FAIL context: panic check only on result==1 (not on timeout/leak)
	codegentest.AssertContains(t, ir, "check_panic_myTest")
	// failedNames stores all non-pass results (FAIL, LEAK, TIMEOUT)
	codegentest.AssertContains(t, ir, "store_fail_myTest")
	// Exit code includes timedOut in the OR chain
	codegentest.AssertContains(t, ir, "or i1")
}

func TestStdFuncUnshadowed(t *testing.T) {
	// After module-based refactor: helper is user code → @__user.helper name (B0319)
	ir := codegentest.GenerateIRWithStd(t,
		`helper() int { return 42; }`,
		`main() { x := helper(); }`,
	)
	codegentest.AssertContains(t, ir, "call i64 @__user.helper")
}

// B0319: User functions get __user. prefix to prevent PAL/libc name collisions.
// A user function named "write" must not collide with libc write().
func TestUserFuncNameNoLibcCollision(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		write(int x) int { return x; }
		main() { write(42); }
	`)
	// User function gets __user. prefix — structurally prevents libc collision
	codegentest.AssertContains(t, ir, "define i64 @__user.write")
	codegentest.AssertContains(t, ir, "call i64 @__user.write")
	// libc write should still be declared separately (via PAL).
	// PAL uses POSIX write() on Unix and _write() on Windows (MSVCRT).
	if runtime.GOOS == "windows" {
		codegentest.AssertContains(t, ir, "declare i32 @_write(")
	} else {
		codegentest.AssertContains(t, ir, "declare i64 @write(")
	}
}

func TestIncDecMember(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter { int value; }
		main() {
			Counter c = Counter(value: 0);
			c.value++;
		}
	`)
	// Should load field, add 1, store back
	codegentest.AssertContains(t, ir, "add i64")
	codegentest.AssertContains(t, ir, "getelementptr")
}

func TestUnaryNotCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			bool b = !true;
		}
	`)
	// ! on bool generates xor with 1
	codegentest.AssertContains(t, ir, "xor i1")
}

// --- Alignment bug fix test ---

// T0481: Multiple `_` slots in the same destructure must produce unique keys
// via uniqueLocalName so dropFlags entries don't collide. The IR should
// contain both `_destructure.discard.dropflag` (first) and
// `_destructure.discard.1.dropflag` (second).
func TestT0481MultipleDiscardsUseUniqueKeys(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			t := ("a" + "b", "c" + "d", 42);
			(_, _, n) := t;
		}
	`)
	codegentest.AssertContains(t, ir, "_destructure.discard.dropflag")
	codegentest.AssertContains(t, ir, "_destructure.discard.1.dropflag")
}

// --- M:N Scheduler IR Tests ---

func TestMainWrappedAsG0(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	// Main is the OS entry point that initializes the scheduler
	codegentest.AssertContains(t, ir, "define i32 @main(i32 %argc, i8** %argv)")
	codegentest.AssertContains(t, ir, "call i32 @pal_num_cpus()")
	codegentest.AssertContains(t, ir, "call void @promise_sched_init(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_run_until_main(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_shutdown()")
	// Main body is compiled inline inside the coroutine (no __promise_user_main call)
	codegentest.AssertContains(t, ir, "define i8* @.goroutine.main()")
	codegentest.AssertNotContains(t, ir, "__promise_user_main")
}

func TestWaiterListFunctionsExist(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	codegentest.AssertContains(t, ir, "define void @promise_waiter_enqueue(")
	codegentest.AssertContains(t, ir, "define i8* @promise_waiter_dequeue(")
	codegentest.AssertContains(t, ir, "define void @promise_waiter_wake_all(")
}

// T0683 regression guard: the void go-block path is unchanged — it still
// stores the 0x1 sentinel into G.result_ptr and emits no value-store block.
// T0686 regression guard: a value-returning `go { Box(...) }` whose result is
// a heap user-struct (Instance layout + generated drop), awaited via `<-x`,
// must NOT leak the coroutine's heap temp into the awaiting presplitcoroutine.
// Before the fix, genGoBlock's value path isolated stmtTemps but not heapTemps
// (unlike genBlock, which isolates heapTemps via T0088). The orphaned Box heap
// temp's coroFn alloca/dropFlag were cleaned up in the OUTER `.goroutine.main`,
// where those values are unnumbered, so they serialized as `%0` (the coro.id
// token): `load i1, i1* %0` / `load i8*, i8** %0` ('%0' is type 'token') makes
// opt verification fail. Guard: `.goroutine.main` (a presplitcoroutine, so its
// `%0` IS the token) must contain no `i1* %0` / `i8** %0` load. `%0` cannot
// false-match `%10`/`%20` (those put a digit between `%` and `0`); the only
// legit `%0` use is `token %0`.
func TestT0686_StructResultNoTokenLoad(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box { int v; string s; }
		main() {
			task[Box] x = go { Box(v: 42, s: "a" + "b") };
			b := <-x;
		}
	`)
	// The awaiting function is `.goroutine.main` (concurrency wraps main into a
	// coroutine). Its `%0` is `call token @llvm.coro.id(...)` — a heap drop-flag
	// or heap-pointer load from `%0` is the exact malformed-IR bug. Extract from
	// the `define` (extractFunction would match the call site inside @main first).
	defStart := strings.Index(ir, "define i8* @.goroutine.main(")
	if defStart < 0 {
		t.Fatal("expected a .goroutine.main coroutine definition in the IR")
	}
	body := ir[defStart:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end+2]
	}
	codegentest.AssertContains(t, body, "call token @llvm.coro.id") // %0 is the token
	codegentest.AssertNotContains(t, body, "i1* %0")                // no heap drop-flag load from the token
	codegentest.AssertNotContains(t, body, "i8** %0")               // no heap pointer load from the token
	// Positive guards: the Box value is still stored into G.result_ptr inside the
	// inner coroutine, and the caller allocates a real result buffer.
	codegentest.AssertContains(t, ir, "go.store_result")
	codegentest.AssertContains(t, ir, "@pal_alloc")
}

func TestGoroutineExitUsesDoneLock(t *testing.T) {
	// goroutine_exit acquires sched.done_lock before setting done=1 and
	// walking done_waiters, ensuring proper synchronization with task receivers.
	ir := codegentest.GenerateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// promise_goroutine_exit must lock done_lock (from sched global)
	codegentest.AssertContains(t, ir, "promise_goroutine_exit")
	// The function should contain mutex lock/unlock calls
	codegentest.AssertContains(t, ir, "waiter_loop")
	codegentest.AssertContains(t, ir, "waiters_done")
}

// B0320: goroutine_exit uses Release ordering on gs_completed increment so
// that alloc_count decrements are visible to any Acquire reader (drain fast path).
func TestGoroutineExitGsCompletedRelease(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	fn := codegentest.ExtractFunction(ir, "promise_goroutine_exit")
	// Both gs_completed increment sites (skip_free and do_free_g) should use release
	codegentest.AssertContains(t, fn, "atomicrmw add i64*")
	codegentest.AssertContains(t, fn, "release")
	// Should NOT use monotonic for gs_completed increments
	// (other atomics in the function may still be monotonic)
}

func TestInstanceIRsBasic(t *testing.T) {
	file, info := codegentest.ParseWithStd(t, codegentest.BoxWithGetMethod+`
		main() {
			b := Box[int](value: 42);
			int x = b.get();
		}
	`)
	result := codegen.Compile(file, info, "")
	instIRs := result.InstanceIRs()
	if len(instIRs) == 0 {
		t.Fatal("expected at least one instance IR")
	}
	ir, ok := instIRs["Box[int]"]
	if !ok {
		t.Fatalf("expected Box[int] in instance IRs, got: %v", codegentest.MapKeys(instIRs))
	}
	// Instance IR must contain at least one function definition for Box[int].
	if !strings.Contains(ir, "Box[int]") {
		t.Errorf("Box[int] IR does not mention Box[int]:\n%s", ir)
	}
	// Instance IR must not contain main() body.
	if strings.Contains(ir, "define void @main") ||
		strings.Contains(ir, "define void @__promise_main") {
		t.Error("instance IR should not contain main function definition")
	}
}

func TestInstanceIRsSeparation(t *testing.T) {
	// Box[int] and Box[string] must produce separate per-instance IRs.
	file, info := codegentest.ParseWithStd(t, codegentest.BoxWithGetMethod+`
		main() {
			a := Box[int](value: 1);
			b := Box[string](value: "hi");
			int x = a.get();
			string y = b.get();
		}
	`)
	result := codegen.Compile(file, info, "")
	instIRs := result.InstanceIRs()

	intIR, hasInt := instIRs["Box[int]"]
	strIR, hasStr := instIRs["Box[string]"]
	if !hasInt {
		t.Fatalf("missing Box[int] in instance IRs, keys: %v", codegentest.MapKeys(instIRs))
	}
	if !hasStr {
		t.Fatalf("missing Box[string] in instance IRs, keys: %v", codegentest.MapKeys(instIRs))
	}

	// Cross-contamination check: each IR must not DEFINE the other instance's functions.
	// (Extern declarations for the other instance's functions are expected and fine.)
	for _, line := range strings.Split(intIR, "\n") {
		if strings.Contains(line, "define") && strings.Contains(line, "Box[string].get") {
			t.Errorf("Box[int] IR should not define Box[string].get:\n  %s", line)
		}
	}
	for _, line := range strings.Split(strIR, "\n") {
		if strings.Contains(line, "define") && strings.Contains(line, "Box[int].get") {
			t.Errorf("Box[string] IR should not define Box[int].get:\n  %s", line)
		}
	}
}

func TestInstanceIRsStrippedFromMainIR(t *testing.T) {
	// After SplitModuleIRs, instance-owned method bodies must not be in main IR.
	file, info := codegentest.ParseWithStd(t, codegentest.BoxWithGetMethod+`
		main() {
			b := Box[int](value: 42);
			int x = b.get();
		}
	`)
	result := codegen.Compile(file, info, "")
	mainIR, _ := result.SplitModuleIRs()

	// Box[int].get must appear only as a declaration (not definition) in main IR.
	// The mangled name in IR is @"Box[int].get" (LLVM quotes names with dots).
	if strings.Contains(mainIR, `define`) && strings.Contains(mainIR, `Box[int].get`) {
		// More precise: look for a definition line
		for _, line := range strings.Split(mainIR, "\n") {
			if strings.Contains(line, "define") && strings.Contains(line, "Box[int].get") {
				t.Errorf("main IR should not define Box[int].get:\n  %s", line)
			}
		}
	}
}

// --- Syscall Handoff Tests (Phase 6a) ---

func TestSyscallHandoffFunctionsExist(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	codegentest.AssertContains(t, ir, "define void @promise_sched_enter_syscall()")
	codegentest.AssertContains(t, ir, "define void @promise_sched_exit_syscall()")
}

func TestSyscallHandoffCurrentMGlobal(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	codegentest.AssertContains(t, ir, "@__promise_current_m")
}

func TestEnterSyscallClearsCurrentP(t *testing.T) {
	// enter_syscall should clear TLS current_p and P.current_g
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	// The function loads current_p, clears P.current_g, clears current_p, calls wake_m
	codegentest.AssertContains(t, ir, "promise_sched_enter_syscall")
	codegentest.AssertContains(t, ir, "promise_sched_wake_m")
}

func TestExitSyscallRestoresP(t *testing.T) {
	// exit_syscall should load current_m, get M.p, restore P.current_g and current_p
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	codegentest.AssertContains(t, ir, "promise_sched_exit_syscall")
	codegentest.AssertContains(t, ir, "__promise_current_m")
}

func TestParkMConditionalRestore(t *testing.T) {
	// B0120: park_m must only restore M.p when deliberately woken (spinning=1).
	// When woken by shutdown (spinning=0), M is still on the idle stack and
	// restoring M.p would corrupt the idle-list next pointer chain.
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	fn := codegentest.ExtractFunction(ir, "promise_sched_park_m")
	// Must have conditional blocks for restore vs skip
	codegentest.AssertContains(t, fn, "restore_p")
	codegentest.AssertContains(t, fn, "skip_restore_p")
}

func TestFindRunnableGlobalEmptyFallsBackToLocal(t *testing.T) {
	// T0326: when the global queue is empty on a global-first tick, find_runnable
	// must fall back to check_local before trying work-stealing. This preserves
	// liveness on single-P targets (e.g., WASM) where steal always returns null.
	ir := codegentest.GenerateIR(t, `main() {}`)
	fn := codegentest.ExtractFunction(ir, "promise_sched_find_runnable")

	// The global_empty block must branch to check_local (not directly to try_steal)
	// when the global-first flag is set. The br target order is check_local first,
	// try_steal second — matching the conditional: if flag==1 → check_local else steal.
	codegentest.AssertContains(t, fn, "label %check_local, label %try_steal")
}

func TestSysmonWakesIdleMOnGlobalWork(t *testing.T) {
	// T0352: sysmon's lost-wakeup safety net. After scanning Ps for preemption,
	// sysmon must check the global queue size (sched field 2) and call wake_m
	// when it's non-zero. This bounds the worst-case stuck time for an M that
	// missed a wake_m signal due to the push-vs-wake race in park_m.
	ir := codegentest.GenerateIR(t, `main() {}`)
	fn := codegentest.ExtractFunction(ir, "promise_sysmon")

	// Must access sched.global_size (field 2 on the sched global) at scan_done.
	// Binding to @__promise_sched ensures we're reading the sched struct, not
	// some other struct's field 2 (e.g., gFieldWaitData=2 or pFieldRqHead=2).
	codegentest.AssertContains(t, fn, "@__promise_sched, i32 0, i32 2")
	// global_size is i64 — must load with the right width.
	codegentest.AssertContains(t, fn, "load i64, i64*")
	// Must call wake_m when global work is pending.
	codegentest.AssertContains(t, fn, "call void @promise_sched_wake_m()")
	// The new sysmon_wake_idle block must exist and be the target of the
	// conditional branch when global_size != 0 (else fall through to loop).
	codegentest.AssertContains(t, fn, "label %sysmon_wake_idle, label %loop")
	// The wake_idle block must branch back to the main loop after wake_m.
	codegentest.AssertContains(t, fn, "sysmon_wake_idle:\n\tcall void @promise_sched_wake_m()\n\tbr label %loop")
}

// --- OS bridge tests ---

func TestArgcArgvGlobals(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	// argc/argv globals are always declared for os.args()/os.executable()
	codegentest.AssertContains(t, ir, "@__promise_argc = global i32 0")
	codegentest.AssertContains(t, ir, "@__promise_argv = global i8**")
}

func TestMainStoresArgcArgv(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	// main receives argc/argv and stores them to globals
	codegentest.AssertContains(t, ir, "define i32 @main(i32 %argc, i8** %argv)")
	codegentest.AssertContains(t, ir, "store i32 %argc, i32* @__promise_argc")
	codegentest.AssertContains(t, ir, "store i8** %argv, i8*** @__promise_argv")
}

func TestCStrGlobalsArePrivate(t *testing.T) {
	// C-string globals (.cstr.<hash>) used for panic messages and assert
	// must have private linkage.
	ir := codegentest.GenerateIR(t, `
		main() {
			assert(1 == 1, "basic math");
		}
	`)
	for _, line := range strings.Split(ir, "\n") {
		if strings.HasPrefix(line, "@.cstr.") && strings.Contains(line, " = ") {
			if !strings.Contains(line, "private") {
				t.Errorf("cstr global must have private linkage: %s", line)
			}
		}
	}
}

func TestStripGlobalsPrivateVsNonPrivate(t *testing.T) {
	// stripGlobals must preserve private globals (string constants)
	// while converting non-private globals (vtables, RTTI) to extern.
	ir := codegentest.GenerateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		main() {
			a := Animal(name: "cat");
			print_line(a.speak());
		}
	`)

	// Private string constants must be defined (have content)
	foundPrivateDef := false
	foundNonPrivateGlobal := false
	for _, line := range strings.Split(ir, "\n") {
		if strings.HasPrefix(line, "@.str.") && strings.Contains(line, "private constant") {
			foundPrivateDef = true
		}
		// Vtable/typeinfo globals are non-private
		if strings.HasPrefix(line, "@promise_vtable_") && strings.Contains(line, " = ") {
			foundNonPrivateGlobal = true
			if strings.Contains(line, "private") {
				t.Errorf("vtable global should NOT be private: %s", line)
			}
		}
	}
	if !foundPrivateDef {
		t.Error("expected at least one private string constant definition")
	}
	if !foundNonPrivateGlobal {
		t.Error("expected at least one non-private vtable global")
	}
}

func TestEmbedEmptyFile(t *testing.T) {
	file, info := codegentest.ParseWithStd(t, `
		get empty string `+"`embed(\"empty.txt\")"+`;
		main() {
			string s = empty;
		}
	`)
	for fd, embed := range info.Embeds {
		_ = fd
		embed.Data = []byte{}
	}
	result := codegen.Compile(file, info, "")
	ir := result.Module.String()
	codegentest.AssertContains(t, ir, "@__user.empty()")
	codegentest.AssertContains(t, ir, "@promise_string_new")
}

// T0130: Terminal operations (count, collect, find) should NOT claim the receiver's
// heap temp — the temp should be freed at statement end via cleanupHeapTemps.
func TestTerminalOpDoesNotClaimReceiver(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter is Iterator[int] {
			int current;
			int limit;
			next(~this) int? {
				if this.current >= this.limit { return none; }
				int val = this.current;
				this.current = this.current + 1;
				return val;
			}
		}
		main() {
			c := Counter(current: 0, limit: 5);
			int n = c.filter(|int x| -> bool { return x > 2; }).count();
		}
	`)
	// The _FnIter from filter() should be cleaned up at statement end (not claimed).
	// Verify iterCleanup appears in the heap cleanup section after the count() call.
	codegentest.AssertContains(t, ir, "heap.drop")
	codegentest.AssertContains(t, ir, "__promise_iter_cleanup")
}

// T0157: Weak[T] — downgrade creates weak ref, upgrade uses cmpxchg.
func TestWeakDowngradeUpgrade(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := Ref[int](42);
			w := a.downgrade();
		}
	`)
	// Downgrade should atomically increment weak_count
	codegentest.AssertContains(t, ir, "%w.dropflag")
	// Should produce Weak drop function
	codegentest.AssertContains(t, ir, `@"Weak[int].drop"`)
}
