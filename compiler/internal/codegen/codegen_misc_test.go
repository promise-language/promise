package codegen

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
)

func TestUnaryNot(t *testing.T) {
	ir := generateIR(t, `main() { x := !true; }`)
	assertContains(t, ir, "xor i1")
}

func TestLeftShift(t *testing.T) {
	ir := generateIR(t, `main() { x := 1 << 4; }`)
	assertContains(t, ir, "shl i64")
}

func TestRightShiftSigned(t *testing.T) {
	ir := generateIR(t, `main() { x := 16 >> 2; }`)
	assertContains(t, ir, "ashr i64")
}

// --- Short-circuit boolean ops ---

func TestShortCircuitAnd(t *testing.T) {
	ir := generateIR(t, `main() { x := true && false; }`)
	assertContains(t, ir, "and.rhs")
	assertContains(t, ir, "and.merge")
}

func TestShortCircuitOr(t *testing.T) {
	ir := generateIR(t, `main() { x := true || false; }`)
	assertContains(t, ir, "or.rhs")
	assertContains(t, ir, "or.merge")
}

func TestIncrementDecrement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			x++;
			x--;
		}
	`)
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "sub i64")
}

func TestFunctionCall(t *testing.T) {
	ir := generateIR(t, `
		double(int x) int { return x * 2; }
		main() { y := double(21); }
	`)
	assertContains(t, ir, "call i64 @__user.double(i64")
}

func TestVoidFunction(t *testing.T) {
	ir := generateIR(t, `
		noop() { }
		main() { noop(); }
	`)
	assertContains(t, ir, "define void @__user.noop()")
	assertContains(t, ir, "call void @__user.noop()")
}

func TestPALWriteExitDefined(t *testing.T) {
	ir := generateIR(t, `
		main() {}
	`)
	// PAL primitives are always emitted
	assertContains(t, ir, "define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)")
	assertContains(t, ir, "define void @pal_exit(i32 %code)")
	if runtime.GOOS == "windows" {
		// Windows PAL uses GetStdHandle+WriteFile and ExitProcess
		assertContains(t, ir, "@GetStdHandle")
		assertContains(t, ir, "@WriteFile")
		assertContains(t, ir, "@ExitProcess")
	} else {
		assertContains(t, ir, "call i64 @write(i32 %fd, i8* %buf, i64 %len)")
		assertContains(t, ir, "call void @exit(i32 %code)")
	}
}

func TestStackOverflowHandler(t *testing.T) {
	ir := generateIR(t, `
		main() {}
	`)
	// B0010: Stack overflow detection
	// Init function is defined and called from main (all platforms)
	assertContains(t, ir, "define void @pal_stack_overflow_init()")
	assertContains(t, ir, "call void @pal_stack_overflow_init()")
	// Thread init is defined and called from sched_loop (all platforms)
	assertContains(t, ir, "define void @pal_stack_overflow_thread_init()")
	assertContains(t, ir, "call void @pal_stack_overflow_thread_init()")

	if runtime.GOOS == "windows" {
		// Windows: VEH handler via AddVectoredExceptionHandler (B0141)
		assertContains(t, ir, "define i32 @__promise_veh_handler(i8* %exception_pointers)")
		assertContains(t, ir, "@AddVectoredExceptionHandler")
		assertContains(t, ir, "@ExitProcess")
	} else if runtime.GOOS == "darwin" {
		// macOS: 3-arg SA_SIGINFO handler printing the fault address (T1161)
		assertContains(t, ir, `@__promise_hex_digits = constant [16 x i8]`)
		assertContains(t, ir, `@__promise_segfault_prefix = constant [31 x i8]`)
		assertContains(t, ir, "define void @__promise_sigsegv_handler(i32 %sig, i8* %info, i8* %ucontext)")
		assertContains(t, ir, "call void @_exit(i32 2)")
		assertContains(t, ir, "call i32 @pthread_attr_setguardsize(")
	} else {
		// Linux: 3-arg SA_SIGINFO handler with fault address (B0128)
		assertContains(t, ir, `@__promise_hex_digits = constant [16 x i8]`)
		assertContains(t, ir, `@__promise_segfault_prefix = constant [31 x i8]`)
		assertContains(t, ir, "define void @__promise_sigsegv_handler(i32 %sig, i8* %info, i8* %ucontext)")
		assertContains(t, ir, "call void @_exit(i32 2)")
		assertContains(t, ir, "call i32 @pthread_attr_setguardsize(")
	}
}

func TestPrintNewlineEmission(t *testing.T) {
	ir := generateIR(t, `
		print_s(string s) `+"`"+`extern("promise_print_string");
		main() { print_s("hello"); }
	`)
	// Newline global constant (used by print_string body)
	assertContains(t, ir, `@.str.newline = private constant [1 x i8] c"\0A"`)
	assertContains(t, ir, `@.str.panic_prefix = private constant [7 x i8] c"panic: "`)
}

// --- Integration tests ---

func TestFibonacci(t *testing.T) {
	ir := generateIR(t, `
		fib(int n) int {
			if n <= 1 {
				return n;
			}
			return fib(n - 1) + fib(n - 2);
		}
		main() { x := fib(10); }
	`)
	assertContains(t, ir, "define i64 @__user.fib(i64 %n)")
	assertContains(t, ir, "call i64 @__user.fib")
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "icmp sle")
}

func TestHeaderGeneration(t *testing.T) {
	result := compileResult(t, `
		use_int(int x) `+"`"+`extern("test_use_int");
		use_f(f64 x) `+"`"+`extern("test_use_f64");
		main() { use_int(42); use_f(3.14); }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Header guard
	assertContains(t, header, "#ifndef PROMISE_BINDINGS_H")
	assertContains(t, header, "#include <stdint.h>")

	// Type definitions for int
	assertContains(t, header, "typedef struct { } promise_int_t;")
	assertContains(t, header, "promise_int_v;")

	// Type definitions for f64
	assertContains(t, header, "typedef struct { } promise_f64_t;")
	assertContains(t, header, "promise_f64_v;")

	// Function declarations: all params by pointer
	assertContains(t, header, "void test_use_int(promise_int_v *x);")
	assertContains(t, header, "void test_use_f64(promise_f64_v *x);")
}

// === User Type Tests ===

func TestUserTypeLayout(t *testing.T) {
	ir := generateIR(t, `
		type Dog { string name; int age; }
		main() { }
	`)
	assertContains(t, ir, "%promise_Dog_t = type {}")
	assertContains(t, ir, "%promise_Dog_m = type { %promise_Dog_t* }")
	assertContains(t, ir, "%promise_Dog_i = type { %promise_Dog_m*, i8*, i64 }")
	assertContains(t, ir, "%promise_Dog_v = type { i8*, %promise_Dog_i* }")
}

func TestUserTypeHeader(t *testing.T) {
	result := compileResult(t, `
		type Dog { string name; int age; }
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	assertContains(t, header, "promise_Dog_t")
	assertContains(t, header, "promise_Dog_m")
	assertContains(t, header, "promise_Dog_i")
	assertContains(t, header, "promise_Dog_v")
	// int field should use raw C type
	assertContains(t, header, "int64_t")
}

func TestSuperCallCodegen(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call void @Animal.new(")
	// Dog constructor should call Dog.new
	assertContains(t, ir, "call void @Dog.new(")
}

func TestHandlerNoBinding(t *testing.T) {
	ir := generateIR(t, `
		foo!() void { raise error(message: "oops"); }
		bar() {
			foo() ? { };
		}
		main() { }
	`)
	assertContains(t, ir, "error.handler")
	// Handler without binding should not load variant pointer for reconstruction
	assertNotContains(t, ir, "error.typed.match")
}

func TestTupleMixedTypes(t *testing.T) {
	ir := generateIR(t, `main() { x := (42, "hello", true); }`)
	// Should produce { i64, i8*, i1 } struct
	assertContains(t, ir, "insertvalue { i64, i8*, i1 }")
}

// T0933/T0940: `m := a ?: b` with a LOCALLY-OWNED source (`HVal? a = ...`) and an
// owned-local default `b`. someOwnsInner is true (the optional's scope drop flag is
// cleared on the some path); the none path neutralizes `b`'s own scope-exit drop flag
// (T0940) so `m` owns the buffer on both paths — the override phi folds to [ true,
// true ]. Single owner on each path: if a=None, `m` frees `b`'s buffer (b's flag
// cleared); if a=Some, `b`'s own binding frees it (none path not taken). Guards that
// the fix keeps the owned-source case owning without double-freeing the owned default.
func TestElvisBoundLocalSourceFlag(t *testing.T) {
	ir := generateIR(t, `
		type HVal { string s; }
		main() {
			HVal? a = HVal(s: "a" + "b");
			HVal b = HVal(s: "c" + "d");
			m := a ?: b;
		}
	`)
	// Bound override phi: some=true (owned local), none=true. No false-some incoming.
	assertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ true, %elvis\.none`)
	assertContainsMatch(t, ir, `store i1 %[0-9]+, i1\* %m\.dropflag`)
}

// unaryExpr: negation
func TestUnaryNegation(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = -42;
			f64 y = -3.14;
		}
	`)
	assertContains(t, ir, "sub i64 0")
	assertContains(t, ir, "fneg double")
}

func TestTypeInfoGlobal(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", breed: "Lab");
		}
	`)
	// Type info globals should be emitted for both types
	assertContains(t, ir, "@promise_typeinfo_Animal")
	assertContains(t, ir, "@promise_typeinfo_Dog")
}

func TestIsAbsent(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = none;
			bool b = x is absent;
		}
	`)
	// Should extract the i1 flag and negate via xor
	assertContains(t, ir, "extractvalue { i1, i64 }")
	assertContains(t, ir, "xor i1")
}

func TestTypeIsFuncBody(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { }
		main() {
			Animal a = Dog(name: "Rex");
			bool b = a is Dog;
		}
	`)
	// Verify key blocks in the defined type_is function
	assertContains(t, ir, "define i32 @promise_type_is")
	assertContains(t, ir, "check_id:")
	assertContains(t, ir, "loop_init:")
	assertContains(t, ir, "loop_header:")
	assertContains(t, ir, "loop_body:")
	assertContains(t, ir, "ret_true:")
	assertContains(t, ir, "ret_false:")
}

func TestRTTIDiamondDedup(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "@promise_typeinfo_Bottom")
	assertContains(t, ir, "@promise_typeinfo_Left")
	assertContains(t, ir, "@promise_typeinfo_Right")
	assertContains(t, ir, "@promise_typeinfo_Base")
}

// --- Stage 8l: Value struct dispatch model tests ---

func TestValueStructRepresentation(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		main() {
			Animal a = Animal(name: "Rex");
		}
	`)
	// Variables of user types should be value struct { i8*, i8* }
	assertContains(t, ir, "alloca { i8*, i8* }")
	// Constructor returns value struct with insertvalue
	assertContains(t, ir, "insertvalue { i8*, i8* }")
}

func TestIsExpressionWithValueStruct(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "extractvalue { i8*, i8* }")
	assertContains(t, ir, "@promise_type_is")
}

func TestSaveRestoreLocalNameCountAcrossAdapter(t *testing.T) {
	// Regression test: emitting a view adapter mid-function used to reset
	// localNameCount, causing duplicate LLVM local names. This test verifies
	// that two variables with the same name in different scopes get unique
	// LLVM names even when a view adapter is emitted between them.
	ir := generateIR(t, `
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
	assertContains(t, ir, "call")
	// If this test compiles at all, the localNameCount save/restore works.
	assertContains(t, ir, "@promise_vtable_int_as_Showable")
}

// T0582: paren-wrapped receiver in a discard statement `(w).self();` must hit
// emitReceiverAliasCheck's IdentExpr arm (via the paren-peel) and emit the
// recv.alias.clear/skip blocks so the temp's drop flag is cleared at runtime.
// Without the peel, mem.Target is a ParenExpr → default: return → no check →
// double-free at statement end.
func TestParenDiscardReceiverEmitsAliasCheck(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int v; self() Wrapper { return this; } }
		main() { w := Wrapper(v: 1); (w).self(); }
	`)
	assertContains(t, ir, "recv.alias.clear")
	assertContains(t, ir, "recv.alias.skip")
}

func TestStdFuncMangledName(t *testing.T) {
	// After module-based refactor: helper() is user code → IR name is @__user.helper
	ir := generateIRWithStd(t,
		`helper() int { return 42; }`,
		`main() { x := helper(); }`,
	)
	assertContains(t, ir, "define i64 @__user.helper")
	assertContains(t, ir, "call i64 @__user.helper")
}

func TestStdUserNameCollision(t *testing.T) {
	// After module-based refactor: both helpers are user code → redefinition is an error,
	// so test with non-conflicting names instead.
	ir := generateIRWithStd(t,
		`helper_extra() int { return 42; }`,
		`
		main() { x := helper_extra(); }
		`,
	)
	assertContains(t, ir, "define i64 @__user.helper_extra")
	assertContains(t, ir, "call i64 @__user.helper_extra()")
}

func TestStdCallViaStdPrefix(t *testing.T) {
	// Real std functions (e.g., print_line) are called via __mod_std_ prefix
	ir := generateIR(t, `main() { print_line("hello"); }`)
	assertContains(t, ir, "call void @__mod_std_print_line")
}

func TestGenerateTestMainNoExistingMain(t *testing.T) {
	// GenerateTestMain should create a new main when none exists
	result := compileResult(t, `
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
	assertContains(t, ir, "define i32 @main")
	assertContains(t, ir, "call i32 @promise_test_run")
	// promise_test_run is now codegen-defined (not a C extern)
	assertContains(t, ir, "define i32 @promise_test_run(i8* %fn, i64 %timeout_ns)")
	// Thread-based: spawns a thread via PAL, joins it
	assertContains(t, ir, "call i8* @pal_thread_create")
	assertContains(t, ir, "call void @pal_thread_join")
	// Trampoline bridges i8*(i8*) pthread ABI to void() test function
	assertContains(t, ir, "define i8* @.test_trampoline(i8* %fn_ptr)")
}

func TestGenerateTestMainReplacesExistingMain(t *testing.T) {
	// GenerateTestMain should replace user main's blocks
	result := compileResult(t, `
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
	assertContains(t, ir, "define i32 @main")
	assertContains(t, ir, "call i32 @promise_test_run")
	assertContains(t, ir, "call void @promise_test_summary")
}

func TestGenerateTestMainStoresArgcArgv(t *testing.T) {
	// GenerateTestMain should store argc/argv to globals for os.arguments/os.executable_path
	result := compileResult(t, `
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
	assertContains(t, ir, "define i32 @main(i32 %argc, i8** %argv)")
	assertContains(t, ir, "store i32 %argc, i32* @__promise_argc")
	assertContains(t, ir, "store i8** %argv, i8*** @__promise_argv")
}

// B0130: Batch test main reserves G.id=0 so goroutine panics recover via scheduler.
func TestGenerateTestMainReservesGID0(t *testing.T) {
	result := compileResult(t, `
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
	assertContains(t, ir, "atomicrmw add i64*")
}

// T0689: When MemoryLimitAccounting=false (run/exec/build, or test with
// -memory-limit 0), the accounting allocator globals and helpers must be
// entirely absent from the IR — hard zero-overhead requirement.
func TestNoMemoryLimitGlobalsByDefault(t *testing.T) {
	result := compileResult(t, `
		main() { v := [1, 2, 3]; }
	`)
	ir := result.Module.String()
	assertNotContains(t, ir, "__promise_memory_used_bytes")
	assertNotContains(t, ir, "__promise_memory_limit_bytes")
	assertNotContains(t, ir, "__promise_memory_set_test_state")
	assertNotContains(t, ir, "fatal: memory limit exceeded")
}

// T0689: With CompileOptions.MemoryLimitAccounting=true, the accounting
// globals and the set_test_state helper are emitted, and pal_alloc's body
// references the used-bytes counter.
func TestMemoryLimitGlobalsEmittedWhenEnabled(t *testing.T) {
	file, info := parseWithStd(t, `
		myTest() `+"`test"+` { }
	`)
	result := CompileWithOptions(file, info, "", &CompileOptions{
		DebugAllocator:        true,
		MemoryLimitAccounting: true,
	})
	result.SetTestMemoryLimits(map[string]int64{"myTest": 1 << 20})
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()
	assertContains(t, ir, "@__promise_memory_used_bytes")
	assertContains(t, ir, "@__promise_memory_limit_bytes")
	assertContains(t, ir, "define void @__promise_memory_set_test_state(i64 %new_limit)")
	assertContains(t, ir, "fatal: memory limit exceeded")
	// Per-test set_test_state call must be emitted before each test runs
	assertContains(t, ir, "call void @__promise_memory_set_test_state")
}

func TestTestPrintResultBody(t *testing.T) {
	result := compileResult(t, `
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
	assertContains(t, ir, "define void @promise_test_print_result(i8* %name, i32 %failed, i64 %elapsed_ns)")
	// 4-way branching: 0=pass, 2=timeout, 3=leak, else=fail
	assertContains(t, ir, "icmp eq i32 %failed, 0") // pass check
	assertContains(t, ir, "icmp eq i32 %failed, 2") // timeout check
	assertContains(t, ir, "icmp eq i32 %failed, 3") // leak check
	assertContains(t, ir, "br i1")                  // conditional branches
	assertContains(t, ir, "br label")               // unconditional branches to merge
	// pass/FAIL/TIMEOUT/LEAK prefix globals
	assertContains(t, ir, `@.str.pass_prefix = private constant [6 x i8] c"pass ("`)
	assertContains(t, ir, `@.str.fail_prefix = private constant [6 x i8] c"FAIL ("`)
	assertContains(t, ir, `@.str.timeout_prefix = private constant [9 x i8] c"TIMEOUT ("`)
	assertContains(t, ir, `@.str.leak_result_prefix = private constant [6 x i8] c"LEAK ("`)
	// Prefix writes
	assertContains(t, ir, "call i64 @pal_write(i32 1,")
	assertContains(t, ir, "i64 6)") // PASS/FAIL prefix length
	// Gets name length via strlen and writes name
	assertContains(t, ir, "call i64 @strlen(i8* %name)")
	// Time formatting: "s) " suffix, "\n" newline
	assertContains(t, ir, `@.str.time_suffix = private constant [3 x i8] c"s) "`)
	assertContains(t, ir, `@.str.newline = private constant [1 x i8] c"\0A"`)
}

func TestTestSummaryBody(t *testing.T) {
	result := compileResult(t, `
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
	assertContains(t, ir, "define void @promise_test_summary(i32 %passed, i32 %failed, i32 %skipped, i32 %leaked, i32 %timed_out, i32 %ignored, i32 %stale)")
	// String suffix globals
	assertContains(t, ir, `@.str.passed_suffix = private constant [9 x i8] c" passed, "`)
	assertContains(t, ir, `@.str.failed_suffix = private constant [7 x i8] c" failed"`)
	assertContains(t, ir, `@.str.skipped_suffix = private constant [8 x i8] c" skipped"`)
	assertContains(t, ir, `@.str.leaked_suffix = private constant [7 x i8] c" leaked"`)
	assertContains(t, ir, `@.str.timed_out_suffix = private constant [10 x i8] c" timed out"`)
	assertContains(t, ir, `@.str.allowed_leaks_suffix = private constant [14 x i8] c" allowed leaks"`)
	assertContains(t, ir, `@.str.stale_suffix = private constant [18 x i8] c" stale allow_leaks"`)
	// Converts i32 → i64 for int_to_string
	assertContains(t, ir, "sext i32 %passed to i64")
	assertContains(t, ir, "sext i32 %failed to i64")
	// Calls int_to_string and frees temp strings
	assertContains(t, ir, "call i8* @promise_int_to_string(i64")
	assertContains(t, ir, "call void @pal_free(i8*")
	// At least 2 free() calls for passed+failed (skipped/leaked are conditional)
	if strings.Count(ir, "call void @pal_free(i8*") < 2 {
		t.Error("expected at least 2 free() calls in promise_test_summary (one per int_to_string result)")
	}
	// Writes to stdout
	assertContains(t, ir, "call i64 @pal_write(i32 1,")
	// Suffix write lengths: 9 for " passed, ", 7 for " failed"
	assertContains(t, ir, "i64 9)")
	assertContains(t, ir, "i64 7)")
	// Conditional skipped output: icmp sgt for skipped > 0
	assertContains(t, ir, "icmp sgt i32 %skipped, 0")
	// Conditional leaked output: icmp sgt for leaked > 0 (T0020)
	assertContains(t, ir, "icmp sgt i32 %leaked, 0")
	// String instance extraction (bitcast for extractStringDataLenFromInstance)
	assertContains(t, ir, "bitcast i8* %")
	assertContains(t, ir, "to %promise_string_i*")
}

func TestTestTrampolineStackCreepDetection(t *testing.T) {
	// The test trampoline should read the stack pointer before and after the test
	// function call, and fail the test if the SP has changed (stack creep).
	result := compileResult(t, `
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
	assertContains(t, ir, "asm sideeffect")
	// Should have stack_creep and stack_ok blocks
	assertContains(t, ir, "stack_creep:")
	assertContains(t, ir, "stack_ok:")
	// Stack creep message global
	assertContains(t, ir, "stack creep detected")
	// The SP comparison drives a conditional branch
	assertContains(t, ir, "icmp eq i64")
}

func TestTestTrampolineNoSetjmp(t *testing.T) {
	// T0150: test trampoline no longer uses setjmp/longjmp for panic recovery.
	// Panics are detected via TLS panic flag check after the test function returns.
	result := compileResult(t, `
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

	fn := extractFunction(ir, ".test_trampoline")
	// Must NOT contain setjmp, longjmp, or jmpBuf alloca
	assertNotContains(t, fn, "setjmp")
	assertNotContains(t, fn, "longjmp")
	assertNotContains(t, fn, "alloca [256 x i8]")
	assertNotContains(t, fn, "__promise_test_jmpbuf")
	// Must contain TLS panic flag check
	assertContains(t, fn, "__promise_panic_flag")
	assertContains(t, fn, "panic_detected")
}

// Test that GenerateTestMain produces 4-way counter logic (pass/fail/timeout/leak)
// and timeout context printing blocks.
func TestTestMainFourWayCountersAndTimeoutContext(t *testing.T) {
	result := compileResult(t, `
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
	assertContains(t, ir, "after_leak_detect_myTest")
	// Effective result phi merges leak-check path and skip path
	assertContains(t, ir, "skip_leak_check_myTest")
	// Timeout counter alloca and update
	assertContains(t, ir, "after_timeout_ctx_myTest")
	// Timeout context: prints "  timeout: exceeded <dur> limit\n" using compile-time duration (T1199)
	assertContains(t, ir, `c"  timeout: exceeded "`)
	assertContains(t, ir, `c"0ns"`) // timeoutNs==0 → formatDurationNs(0) == "0ns"
	assertContains(t, ir, `c" limit\0A"`)
	// FAIL context: panic check only on result==1 (not on timeout/leak)
	assertContains(t, ir, "check_panic_myTest")
	// failedNames stores all non-pass results (FAIL, LEAK, TIMEOUT)
	assertContains(t, ir, "store_fail_myTest")
	// Exit code includes timedOut in the OR chain
	assertContains(t, ir, "or i1")
}

func TestStdFuncUnshadowed(t *testing.T) {
	// After module-based refactor: helper is user code → @__user.helper name (B0319)
	ir := generateIRWithStd(t,
		`helper() int { return 42; }`,
		`main() { x := helper(); }`,
	)
	assertContains(t, ir, "call i64 @__user.helper")
}

// B0319: User functions get __user. prefix to prevent PAL/libc name collisions.
// A user function named "write" must not collide with libc write().
func TestUserFuncNameNoLibcCollision(t *testing.T) {
	ir := generateIR(t, `
		write(int x) int { return x; }
		main() { write(42); }
	`)
	// User function gets __user. prefix — structurally prevents libc collision
	assertContains(t, ir, "define i64 @__user.write")
	assertContains(t, ir, "call i64 @__user.write")
	// libc write should still be declared separately (via PAL).
	// PAL uses POSIX write() on Unix and _write() on Windows (MSVCRT).
	if runtime.GOOS == "windows" {
		assertContains(t, ir, "declare i32 @_write(")
	} else {
		assertContains(t, ir, "declare i64 @write(")
	}
}

func TestIncDecMember(t *testing.T) {
	ir := generateIR(t, `
		type Counter { int value; }
		main() {
			Counter c = Counter(value: 0);
			c.value++;
		}
	`)
	// Should load field, add 1, store back
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "getelementptr")
}

func TestUnaryNotCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			bool b = !true;
		}
	`)
	// ! on bool generates xor with 1
	assertContains(t, ir, "xor i1")
}

// --- Alignment bug fix test ---

func TestLlvmTypeSizeAlignment(t *testing.T) {
	// Test that struct sizes account for alignment padding
	// {i1, i64} should be 16 (1 byte + 7 padding + 8 bytes), not 9
	s1 := irtypes.NewStruct(irtypes.I1, irtypes.I64)
	if sz := llvmTypeSize(s1); sz != 16 {
		t.Errorf("{i1, i64} size: got %d, want 16", sz)
	}

	// {i64, i1} should be 16 (8 bytes + 1 byte + 7 tail padding)
	s2 := irtypes.NewStruct(irtypes.I64, irtypes.I1)
	if sz := llvmTypeSize(s2); sz != 16 {
		t.Errorf("{i64, i1} size: got %d, want 16", sz)
	}

	// {i32, i32} should be 8 (no padding needed)
	s3 := irtypes.NewStruct(irtypes.I32, irtypes.I32)
	if sz := llvmTypeSize(s3); sz != 8 {
		t.Errorf("{i32, i32} size: got %d, want 8", sz)
	}

	// {i8, i32, i8} should be 12 (1 + 3pad + 4 + 1 + 3pad)
	s4 := irtypes.NewStruct(irtypes.I8, irtypes.I32, irtypes.I8)
	if sz := llvmTypeSize(s4); sz != 12 {
		t.Errorf("{i8, i32, i8} size: got %d, want 12", sz)
	}
}

func TestLlvmTypeAlignDouble(t *testing.T) {
	if a := llvmTypeAlign(irtypes.Double); a != 8 {
		t.Errorf("double align: got %d, want 8", a)
	}
}

func TestLlvmTypeAlignPointer(t *testing.T) {
	if a := llvmTypeAlign(irtypes.I8Ptr); a != 8 {
		t.Errorf("pointer align: got %d, want 8", a)
	}
}

func TestLlvmTypeAlignStruct(t *testing.T) {
	s := irtypes.NewStruct(irtypes.I8, irtypes.I64)
	if a := llvmTypeAlign(s); a != 8 {
		t.Errorf("{i8, i64} align: got %d, want 8", a)
	}
}

func TestLlvmTypeSizePointer(t *testing.T) {
	if sz := llvmTypeSize(irtypes.I8Ptr); sz != 8 {
		t.Errorf("pointer size: got %d, want 8", sz)
	}
}

// T0481: Multiple `_` slots in the same destructure must produce unique keys
// via uniqueLocalName so dropFlags entries don't collide. The IR should
// contain both `_destructure.discard.dropflag` (first) and
// `_destructure.discard.1.dropflag` (second).
func TestT0481MultipleDiscardsUseUniqueKeys(t *testing.T) {
	ir := generateIR(t, `
		main() {
			t := ("a" + "b", "c" + "d", 42);
			(_, _, n) := t;
		}
	`)
	assertContains(t, ir, "_destructure.discard.dropflag")
	assertContains(t, ir, "_destructure.discard.1.dropflag")
}

// --- M:N Scheduler IR Tests ---

func TestMainWrappedAsG0(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	// Main is the OS entry point that initializes the scheduler
	assertContains(t, ir, "define i32 @main(i32 %argc, i8** %argv)")
	assertContains(t, ir, "call i32 @pal_num_cpus()")
	assertContains(t, ir, "call void @promise_sched_init(")
	assertContains(t, ir, "call void @promise_sched_run_until_main(")
	assertContains(t, ir, "call void @promise_sched_shutdown()")
	// Main body is compiled inline inside the coroutine (no __promise_user_main call)
	assertContains(t, ir, "define i8* @.goroutine.main()")
	assertNotContains(t, ir, "__promise_user_main")
}

func TestWaiterListFunctionsExist(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	assertContains(t, ir, "define void @promise_waiter_enqueue(")
	assertContains(t, ir, "define i8* @promise_waiter_dequeue(")
	assertContains(t, ir, "define void @promise_waiter_wake_all(")
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
	ir := generateIR(t, `
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
	assertContains(t, body, "call token @llvm.coro.id") // %0 is the token
	assertNotContains(t, body, "i1* %0")                // no heap drop-flag load from the token
	assertNotContains(t, body, "i8** %0")               // no heap pointer load from the token
	// Positive guards: the Box value is still stored into G.result_ptr inside the
	// inner coroutine, and the caller allocates a real result buffer.
	assertContains(t, ir, "go.store_result")
	assertContains(t, ir, "@pal_alloc")
}

func TestGoroutineExitUsesDoneLock(t *testing.T) {
	// goroutine_exit acquires sched.done_lock before setting done=1 and
	// walking done_waiters, ensuring proper synchronization with task receivers.
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// promise_goroutine_exit must lock done_lock (from sched global)
	assertContains(t, ir, "promise_goroutine_exit")
	// The function should contain mutex lock/unlock calls
	assertContains(t, ir, "waiter_loop")
	assertContains(t, ir, "waiters_done")
}

// B0320: goroutine_exit uses Release ordering on gs_completed increment so
// that alloc_count decrements are visible to any Acquire reader (drain fast path).
func TestGoroutineExitGsCompletedRelease(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	fn := extractFunction(ir, "promise_goroutine_exit")
	// Both gs_completed increment sites (skip_free and do_free_g) should use release
	assertContains(t, fn, "atomicrmw add i64*")
	assertContains(t, fn, "release")
	// Should NOT use monotonic for gs_completed increments
	// (other atomics in the function may still be monotonic)
}

func TestInstanceIRsBasic(t *testing.T) {
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() {
			b := Box[int](value: 42);
			int x = b.get();
		}
	`)
	result := Compile(file, info, "")
	instIRs := result.InstanceIRs()
	if len(instIRs) == 0 {
		t.Fatal("expected at least one instance IR")
	}
	ir, ok := instIRs["Box[int]"]
	if !ok {
		t.Fatalf("expected Box[int] in instance IRs, got: %v", mapKeys(instIRs))
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
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() {
			a := Box[int](value: 1);
			b := Box[string](value: "hi");
			int x = a.get();
			string y = b.get();
		}
	`)
	result := Compile(file, info, "")
	instIRs := result.InstanceIRs()

	intIR, hasInt := instIRs["Box[int]"]
	strIR, hasStr := instIRs["Box[string]"]
	if !hasInt {
		t.Fatalf("missing Box[int] in instance IRs, keys: %v", mapKeys(instIRs))
	}
	if !hasStr {
		t.Fatalf("missing Box[string] in instance IRs, keys: %v", mapKeys(instIRs))
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
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() {
			b := Box[int](value: 42);
			int x = b.get();
		}
	`)
	result := Compile(file, info, "")
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
	ir := generateIR(t, `
		main() { }
	`)
	assertContains(t, ir, "define void @promise_sched_enter_syscall()")
	assertContains(t, ir, "define void @promise_sched_exit_syscall()")
}

func TestSyscallHandoffCurrentMGlobal(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	assertContains(t, ir, "@__promise_current_m")
}

func TestEnterSyscallClearsCurrentP(t *testing.T) {
	// enter_syscall should clear TLS current_p and P.current_g
	ir := generateIR(t, `
		main() { }
	`)
	// The function loads current_p, clears P.current_g, clears current_p, calls wake_m
	assertContains(t, ir, "promise_sched_enter_syscall")
	assertContains(t, ir, "promise_sched_wake_m")
}

func TestExitSyscallRestoresP(t *testing.T) {
	// exit_syscall should load current_m, get M.p, restore P.current_g and current_p
	ir := generateIR(t, `
		main() { }
	`)
	assertContains(t, ir, "promise_sched_exit_syscall")
	assertContains(t, ir, "__promise_current_m")
}

func TestParkMConditionalRestore(t *testing.T) {
	// B0120: park_m must only restore M.p when deliberately woken (spinning=1).
	// When woken by shutdown (spinning=0), M is still on the idle stack and
	// restoring M.p would corrupt the idle-list next pointer chain.
	ir := generateIR(t, `
		main() { }
	`)
	fn := extractFunction(ir, "promise_sched_park_m")
	// Must have conditional blocks for restore vs skip
	assertContains(t, fn, "restore_p")
	assertContains(t, fn, "skip_restore_p")
}

func TestFindRunnableGlobalEmptyFallsBackToLocal(t *testing.T) {
	// T0326: when the global queue is empty on a global-first tick, find_runnable
	// must fall back to check_local before trying work-stealing. This preserves
	// liveness on single-P targets (e.g., WASM) where steal always returns null.
	ir := generateIR(t, `main() {}`)
	fn := extractFunction(ir, "promise_sched_find_runnable")

	// The global_empty block must branch to check_local (not directly to try_steal)
	// when the global-first flag is set. The br target order is check_local first,
	// try_steal second — matching the conditional: if flag==1 → check_local else steal.
	assertContains(t, fn, "label %check_local, label %try_steal")
}

func TestSysmonWakesIdleMOnGlobalWork(t *testing.T) {
	// T0352: sysmon's lost-wakeup safety net. After scanning Ps for preemption,
	// sysmon must check the global queue size (sched field 2) and call wake_m
	// when it's non-zero. This bounds the worst-case stuck time for an M that
	// missed a wake_m signal due to the push-vs-wake race in park_m.
	ir := generateIR(t, `main() {}`)
	fn := extractFunction(ir, "promise_sysmon")

	// Must access sched.global_size (field 2 on the sched global) at scan_done.
	// Binding to @__promise_sched ensures we're reading the sched struct, not
	// some other struct's field 2 (e.g., gFieldWaitData=2 or pFieldRqHead=2).
	assertContains(t, fn, "@__promise_sched, i32 0, i32 2")
	// global_size is i64 — must load with the right width.
	assertContains(t, fn, "load i64, i64*")
	// Must call wake_m when global work is pending.
	assertContains(t, fn, "call void @promise_sched_wake_m()")
	// The new sysmon_wake_idle block must exist and be the target of the
	// conditional branch when global_size != 0 (else fall through to loop).
	assertContains(t, fn, "label %sysmon_wake_idle, label %loop")
	// The wake_idle block must branch back to the main loop after wake_m.
	assertContains(t, fn, "sysmon_wake_idle:\n\tcall void @promise_sched_wake_m()\n\tbr label %loop")
}

// --- OS bridge tests ---

func TestArgcArgvGlobals(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	// argc/argv globals are always declared for os.args()/os.executable()
	assertContains(t, ir, "@__promise_argc = global i32 0")
	assertContains(t, ir, "@__promise_argv = global i8**")
}

func TestMainStoresArgcArgv(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	// main receives argc/argv and stores them to globals
	assertContains(t, ir, "define i32 @main(i32 %argc, i8** %argv)")
	assertContains(t, ir, "store i32 %argc, i32* @__promise_argc")
	assertContains(t, ir, "store i8** %argv, i8*** @__promise_argv")
}

func TestCStrGlobalsArePrivate(t *testing.T) {
	// C-string globals (.cstr.<hash>) used for panic messages and assert
	// must have private linkage.
	ir := generateIR(t, `
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
	ir := generateIR(t, `
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
	file, info := parseWithStd(t, `
		get empty string `+"`embed(\"empty.txt\")"+`;
		main() {
			string s = empty;
		}
	`)
	for fd, embed := range info.Embeds {
		_ = fd
		embed.Data = []byte{}
	}
	result := Compile(file, info, "")
	ir := result.Module.String()
	assertContains(t, ir, "@__user.empty()")
	assertContains(t, ir, "@promise_string_new")
}

// T0130: Terminal operations (count, collect, find) should NOT claim the receiver's
// heap temp — the temp should be freed at statement end via cleanupHeapTemps.
func TestTerminalOpDoesNotClaimReceiver(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "heap.drop")
	assertContains(t, ir, "__promise_iter_cleanup")
}

// T0157: Weak[T] — downgrade creates weak ref, upgrade uses cmpxchg.
func TestWeakDowngradeUpgrade(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			w := a.downgrade();
		}
	`)
	// Downgrade should atomically increment weak_count
	assertContains(t, ir, "%w.dropflag")
	// Should produce Weak drop function
	assertContains(t, ir, `@"Weak[int].drop"`)
}
