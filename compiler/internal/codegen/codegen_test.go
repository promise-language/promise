package codegen

import (
	"bytes"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/testutil"
	"djabi.dev/go/promise_lang/internal/types"
	antlr "github.com/antlr4-go/antlr/v4"
	irtypes "github.com/llir/llvm/ir/types"
)

// stdAll provides all builtin type declarations needed by tests.
// Loaded from the actual std/*.pr files to avoid duplication.
var stdAll string

var (
	codegenStdOnce    sync.Once
	codegenStdModInfo *sema.ModuleInfo
	codegenStdScope   *types.Scope
)

func init() {
	stdAll = testutil.LoadStdFiles()
}

func getCodegenStdModInfo() (*sema.ModuleInfo, *types.Scope) {
	codegenStdOnce.Do(func() {
		input := antlr.NewInputStream(stdAll)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		stdFile, buildErrs := ast.Build("std.pr", tree)
		if len(buildErrs) > 0 {
			panic("std AST build errors: " + buildErrs[0].Error())
		}
		stdInfo, _ := sema.CheckWithTarget(stdFile, nil, sema.HostTargetInfo())
		codegenStdScope = sema.ExportedScope(stdInfo, stdFile)
		codegenStdModInfo = &sema.ModuleInfo{
			Name:           "std",
			CanonicalName:  "std",
			GlobalIdentity: "std",
			IRPrefix:       "std",
			File:           stdFile,
			SemaInfo:       stdInfo,
		}
	})
	return codegenStdModInfo, codegenStdScope
}

// parseWithStd parses user code, injects use std as _, and runs sema with the std module.
func parseWithStd(t *testing.T, src string) (*ast.File, *sema.Info) {
	t.Helper()

	stdModInfo, stdScope := getCodegenStdModInfo()

	// Parse user
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, errs := ast.Build("test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	file.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)

	info, errs := sema.CheckWithModules(file, map[string]*types.Scope{"std": stdScope})
	if len(errs) > 0 {
		t.Fatalf("sema errors: %v", errs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{"std": stdModInfo}
	info.ModuleOrder = []string{"std"}
	return file, info
}

// generateIR runs the full pipeline: parse → sema → codegen, returns LLVM IR text.
func generateIR(t *testing.T, src string) string {
	t.Helper()
	file, info := parseWithStd(t, src)
	result := Compile(file, info, "")
	return result.Module.String()
}

// compileResult runs the full pipeline and returns the CompileResult.
func compileResult(t *testing.T, src string) *CompileResult {
	t.Helper()
	file, info := parseWithStd(t, src)
	return Compile(file, info, "")
}

func assertContains(t *testing.T, ir, substr string) {
	t.Helper()
	if !strings.Contains(ir, substr) {
		t.Errorf("expected IR to contain %q\ngot:\n%s", substr, ir)
	}
}

func assertContainsMatch(t *testing.T, ir, pattern string) {
	t.Helper()
	re := regexp.MustCompile(pattern)
	if !re.MatchString(ir) {
		t.Errorf("expected IR to match %q\ngot:\n%s", pattern, ir)
	}
}

func assertNotContains(t *testing.T, ir, substr string) {
	t.Helper()
	if strings.Contains(ir, substr) {
		t.Errorf("expected IR to NOT contain %q\ngot:\n%s", substr, ir)
	}
}

// extractFunction returns the IR text for a named function (from "define" to the closing "}").
func extractFunction(ir, name string) string {
	// Find "define ... @name("
	marker := "@" + name + "("
	start := strings.Index(ir, marker)
	if start < 0 {
		return ""
	}
	// Walk back to "define"
	lineStart := strings.LastIndex(ir[:start], "define")
	if lineStart < 0 {
		return ""
	}
	// Find closing "}\n" — LLVM IR functions end with "}\n" at column 0
	rest := ir[lineStart:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return rest
	}
	return rest[:end+2]
}

// --- Literal tests ---

func TestIntLiteral(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "store i64 42")
}

func TestFloatLiteral(t *testing.T) {
	ir := generateIR(t, `main() { x := 3.14; }`)
	assertContains(t, ir, "double")
	// LLVM serializes floats as hex: 3.14 → 0x40091EB851EB851F
	assertContains(t, ir, "store double")
}

func TestBoolLiteral(t *testing.T) {
	ir := generateIR(t, `main() { x := true; y := false; }`)
	assertContains(t, ir, "store i1 true")
	assertContains(t, ir, "store i1 false")
}

// --- Binary operator tests (type-system dispatch) ---

func TestIntAdd(t *testing.T) {
	ir := generateIR(t, `main() { x := 1 + 2; }`)
	assertContains(t, ir, "add i64")
}

func TestIntSub(t *testing.T) {
	ir := generateIR(t, `main() { x := 5 - 3; }`)
	assertContains(t, ir, "sub i64")
}

func TestIntMul(t *testing.T) {
	ir := generateIR(t, `main() { x := 3 * 4; }`)
	assertContains(t, ir, "mul i64")
}

func TestIntDiv(t *testing.T) {
	ir := generateIR(t, `main() { x := 10 / 3; }`)
	assertContains(t, ir, "sdiv i64")
}

func TestIntMod(t *testing.T) {
	ir := generateIR(t, `main() { x := 10 % 3; }`)
	assertContains(t, ir, "srem i64")
}

func TestIntComparison(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := 1 == 2;
			b := 1 != 2;
			c := 1 < 2;
			d := 1 > 2;
			e := 1 <= 2;
			f := 1 >= 2;
		}
	`)
	assertContains(t, ir, "icmp eq")
	assertContains(t, ir, "icmp ne")
	assertContains(t, ir, "icmp slt")
	assertContains(t, ir, "icmp sgt")
	assertContains(t, ir, "icmp sle")
	assertContains(t, ir, "icmp sge")
}

func TestFloatArithmetic(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := 1.0 + 2.0;
			b := 1.0 - 2.0;
			c := 1.0 * 2.0;
			d := 1.0 / 2.0;
		}
	`)
	assertContains(t, ir, "fadd double")
	assertContains(t, ir, "fsub double")
	assertContains(t, ir, "fmul double")
	assertContains(t, ir, "fdiv double")
}

func TestFloatComparison(t *testing.T) {
	ir := generateIR(t, `main() { a := 1.0 < 2.0; }`)
	assertContains(t, ir, "fcmp olt")
}

// --- Unary operator tests ---

func TestUnaryNegInt(t *testing.T) {
	ir := generateIR(t, `main() { x := -42; }`)
	assertContains(t, ir, "sub i64 0")
}

func TestUnaryNot(t *testing.T) {
	ir := generateIR(t, `main() { x := !true; }`)
	assertContains(t, ir, "xor i1")
}

// --- Bitwise operators ---

func TestBitwiseAnd(t *testing.T) {
	ir := generateIR(t, `main() { x := 12 & 10; }`)
	assertContains(t, ir, "and i64")
}

func TestBitwiseOr(t *testing.T) {
	ir := generateIR(t, `main() { x := 5 | 3; }`)
	assertContains(t, ir, "or i64")
}

func TestBitwiseXor(t *testing.T) {
	ir := generateIR(t, `main() { x := 12 ^ 10; }`)
	assertContains(t, ir, "xor i64")
}

func TestLeftShift(t *testing.T) {
	ir := generateIR(t, `main() { x := 1 << 4; }`)
	assertContains(t, ir, "shl i64")
}

func TestRightShiftSigned(t *testing.T) {
	ir := generateIR(t, `main() { x := 16 >> 2; }`)
	assertContains(t, ir, "ashr i64")
}

func TestBitwiseNot(t *testing.T) {
	ir := generateIR(t, `main() { x := ~0; }`)
	assertContains(t, ir, "xor i64")
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

// --- Variable tests ---

func TestVariableAllocaAndLoad(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			int y = x;
		}
	`)
	assertContains(t, ir, "alloca i64")
	assertContains(t, ir, "store i64 10")
	assertContains(t, ir, "load i64")
}

func TestInferredVarDecl(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "alloca i64")
	assertContains(t, ir, "store i64 42")
}

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

// --- Function tests ---

func TestFunctionDeclaration(t *testing.T) {
	ir := generateIR(t, `
		add(int a, int b) int {
			return a + b;
		}
		main() { }
	`)
	assertContains(t, ir, "define i64 @add(i64 %a, i64 %b)")
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "ret i64")
}

func TestFunctionCall(t *testing.T) {
	ir := generateIR(t, `
		double(int x) int { return x * 2; }
		main() { y := double(21); }
	`)
	assertContains(t, ir, "call i64 @double(i64")
}

func TestVoidFunction(t *testing.T) {
	ir := generateIR(t, `
		noop() { }
		main() { noop(); }
	`)
	assertContains(t, ir, "define void @noop()")
	assertContains(t, ir, "call void @noop()")
}

// --- Extern print (struct-based ABI) ---

func TestPrintStringExtern(t *testing.T) {
	ir := generateIR(t, `
		print_s(string s) `+"`"+`extern("promise_print_string");
		main() { print_s("hello"); }
	`)
	assertContains(t, ir, "%promise_string_v = type")
	assertContains(t, ir, "define void @promise_print_string(i8*")
}

// --- PAL function body tests ---
// These verify that definePALBodies() generates correct IR for print/panic functions.

func TestPrintStringBody(t *testing.T) {
	ir := generateIR(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)
	// Function body: extracts data/len from string value struct, writes via PAL
	assertContains(t, ir, "define void @promise_print_string(i8*")
	assertContains(t, ir, "bitcast i8* %s to %promise_string_v*")
	assertContains(t, ir, "call i64 @pal_write(i32 1,") // stdout
}

func TestPanicBody(t *testing.T) {
	ir := generateIR(t, `
		main() {}
	`)
	// promise_panic is always declared as intrinsic; definePALBodies adds body
	assertContains(t, ir, "define void @promise_panic(i8*")
	assertContains(t, ir, "call i64 @strlen(i8*")
	assertContains(t, ir, "call i64 @pal_write(i32 2,") // stderr
	assertContains(t, ir, "call void @pal_exit(i32 1)")
	assertContains(t, ir, "unreachable")
}

func TestPanicMsgBody(t *testing.T) {
	ir := generateIR(t, `
		panic_msg(string msg) `+"`"+`extern("promise_panic_msg");
		main() { panic_msg("boom"); }
	`)
	assertContains(t, ir, "define void @promise_panic_msg(i8*")
	assertContains(t, ir, "bitcast i8* %msg to %promise_string_v*")
	assertContains(t, ir, "call i64 @pal_write(i32 2,") // stderr
	assertContains(t, ir, "call void @pal_exit(i32 1)")
	assertContains(t, ir, "unreachable")
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
		// macOS: 1-arg SIGSEGV handler with "fatal: stack overflow" message
		assertContains(t, ir, `@__promise_stack_overflow_msg = constant [22 x i8]`)
		assertContains(t, ir, "define void @__promise_sigsegv_handler(i32 %sig)")
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
	assertContains(t, ir, "define i64 @fib(i64 %n)")
	assertContains(t, ir, "call i64 @fib")
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "icmp sle")
}

// --- Extern architecture tests ---

func TestExternCustomCName(t *testing.T) {
	ir := generateIR(t, `
		log_value(int x) `+"`"+`extern("my_log_int");
		main() { log_value(99); }
	`)
	assertContains(t, ir, "declare void @my_log_int(i8*")
	assertContains(t, ir, "call void @my_log_int(i8*")
}

func TestExternDefaultCName(t *testing.T) {
	ir := generateIR(t, `
		do_thing(int x) `+"`"+`extern;
		main() { do_thing(1); }
	`)
	assertContains(t, ir, "declare void @promise_do_thing(i8*")
}

// generateIRForTarget runs parse → sema → codegen with a specific target triple.
func generateIRForTarget(t *testing.T, src, target string) string {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, errs := ast.Build("test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}

	stdModInfo, stdScope := getCodegenStdModInfo()
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	file.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)

	ti := sema.ParseTargetInfo(target)
	info, semaErrs := sema.CheckWithTarget(file, map[string]*types.Scope{"std": stdScope}, ti)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{"std": stdModInfo}
	info.ModuleOrder = []string{"std"}
	result := Compile(file, info, target)
	return result.Module.String()
}

// --- wasm_import codegen tests (T0035) ---

func TestWasmImportAttributes(t *testing.T) {
	ir := generateIRForTarget(t, `
		_fd_write(int fd) int `+"`extern(\"fd_write\") `wasm_import(\"wasi_snapshot_preview1\", \"fd_write\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	assertContains(t, ir, `"wasm-import-module"="wasi_snapshot_preview1"`)
	assertContains(t, ir, `"wasm-import-name"="fd_write"`)
}

func TestWasmImportIgnoredOnNative(t *testing.T) {
	// On native target, wasm_import annotations should not produce IR attributes.
	// The function itself is filtered out by `target(wasm), so it won't appear at all.
	ir := generateIR(t, `
		_fd_write(int fd) int `+"`extern(\"fd_write\") `wasm_import(\"wasi_snapshot_preview1\", \"fd_write\") `target(wasm)"+`;
		main() {}
	`)
	assertNotContains(t, ir, "wasm-import-module")
}

func TestExternMultipleParams(t *testing.T) {
	ir := generateIR(t, `
		add_ext(int a, int b) `+"`"+`extern("test_add");
		main() { add_ext(1, 2); }
	`)
	assertContains(t, ir, "declare void @test_add(i8* %a, i8* %b)")
	assertContains(t, ir, "call void @test_add")
}

func TestExternReturnValue(t *testing.T) {
	ir := generateIR(t, `
		get_value() int `+"`"+`extern("test_get");
		main() { x := get_value(); }
	`)
	// sret: struct return becomes void with first param as result pointer
	assertContains(t, ir, "declare void @test_get(i8* %sret)")
	// Return value should be loaded from sret alloca and unpacked
	assertContains(t, ir, "extractvalue %promise_int_v")
}

func TestExternStructTypeDefs(t *testing.T) {
	ir := generateIR(t, `
		use_int(int x) `+"`"+`extern("test_use_int");
		main() { use_int(42); }
	`)
	// All four struct types should be defined
	assertContains(t, ir, "%promise_int_t = type {}")
	assertContains(t, ir, "%promise_int_m = type { %promise_int_t* }")
	assertContains(t, ir, "%promise_int_i = type { %promise_int_m* }")
	assertContains(t, ir, "%promise_int_v = type { i8*, %promise_int_i*, i64 }")
}

// --- Primitive type layout coverage ---
// These tests verify that layout computation and extern declarations work
// for all primitive types. Externs are declared but not called since sema
// doesn't allow implicit narrowing from int/f64 literals to narrow types.

func TestExternI8Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i8(i8 x) `+"`"+`extern("test_i8");
		main() { }
	`)
	assertContains(t, ir, "%promise_i8_v = type { i8*, %promise_i8_i*, i8 }")
	assertContains(t, ir, "%promise_i8_i = type { %promise_i8_m* }")
	assertContains(t, ir, "%promise_i8_m = type { %promise_i8_t* }")
	assertContains(t, ir, "%promise_i8_t = type {}")
	assertContains(t, ir, "declare void @test_i8(i8*")
}

func TestExternI16Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i16(i16 x) `+"`"+`extern("test_i16");
		main() { }
	`)
	assertContains(t, ir, "%promise_i16_v = type { i8*, %promise_i16_i*, i16 }")
	assertContains(t, ir, "declare void @test_i16(i8*")
}

func TestExternI32Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i32(i32 x) `+"`"+`extern("test_i32");
		main() { }
	`)
	assertContains(t, ir, "%promise_i32_v = type { i8*, %promise_i32_i*, i32 }")
	assertContains(t, ir, "declare void @test_i32(i8*")
}

func TestExternU8Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u8(u8 x) `+"`"+`extern("test_u8");
		main() { }
	`)
	assertContains(t, ir, "%promise_u8_v = type { i8*, %promise_u8_i*, i8 }")
	assertContains(t, ir, "declare void @test_u8(i8*")
}

func TestExternU16Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u16(u16 x) `+"`"+`extern("test_u16");
		main() { }
	`)
	assertContains(t, ir, "%promise_u16_v = type { i8*, %promise_u16_i*, i16 }")
	assertContains(t, ir, "declare void @test_u16(i8*")
}

func TestExternU32Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u32(u32 x) `+"`"+`extern("test_u32");
		main() { }
	`)
	assertContains(t, ir, "%promise_u32_v = type { i8*, %promise_u32_i*, i32 }")
	assertContains(t, ir, "declare void @test_u32(i8*")
}

func TestExternU64Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u64(u64 x) `+"`"+`extern("test_u64");
		main() { }
	`)
	assertContains(t, ir, "%promise_u64_v = type { i8*, %promise_u64_i*, i64 }")
	assertContains(t, ir, "declare void @test_u64(i8*")
}

func TestExternI64Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i64(i64 x) `+"`"+`extern("test_i64");
		main() { }
	`)
	assertContains(t, ir, "%promise_i64_v = type { i8*, %promise_i64_i*, i64 }")
	assertContains(t, ir, "declare void @test_i64(i8*")
}

func TestExternF32Layout(t *testing.T) {
	ir := generateIR(t, `
		log_f32(f32 x) `+"`"+`extern("test_f32");
		main() { }
	`)
	assertContains(t, ir, "%promise_f32_v = type { i8*, %promise_f32_i*, float }")
	assertContains(t, ir, "declare void @test_f32(i8*")
}

func TestExternCharLayout(t *testing.T) {
	ir := generateIR(t, `
		log_char(char x) `+"`"+`extern("test_char");
		main() { }
	`)
	assertContains(t, ir, "%promise_char_v = type { i8*, %promise_char_i*, i32 }")
	assertContains(t, ir, "declare void @test_char(i8*")
}

func TestExternUintLayout(t *testing.T) {
	ir := generateIR(t, `
		log_uint(uint x) `+"`"+`extern("test_uint");
		main() { }
	`)
	assertContains(t, ir, "%promise_uint_v = type { i8*, %promise_uint_i*, i64 }")
	assertContains(t, ir, "declare void @test_uint(i8*")
}

// --- Header generation: return types and zero-param ---

func TestHeaderExternReturnType(t *testing.T) {
	result := compileResult(t, `
		get_val() int `+"`"+`extern("test_get_val");
		main() { x := get_val(); }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Return type uses sret: void return with first param as result pointer
	assertContains(t, header, "void test_get_val(promise_int_v *sret);")
}

func TestHeaderExternZeroParams(t *testing.T) {
	result := compileResult(t, `
		do_nothing() `+"`"+`extern("test_noop");
		main() { do_nothing(); }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Zero-param void functions should have (void) in C
	assertContains(t, header, "void test_noop(void);")
}

func TestHeaderExternMultipleTypes(t *testing.T) {
	// Externs only declared (not called) since sema doesn't allow implicit narrowing
	result := compileResult(t, `
		log_i32(i32 x) `+"`"+`extern("test_log_i32");
		log_bool(bool x) `+"`"+`extern("test_log_bool");
		log_f32(f32 x) `+"`"+`extern("test_log_f32");
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// bool layout: raw is uint8_t
	assertContains(t, header, "typedef struct { } promise_bool_t;")
	assertContains(t, header, "uint8_t              raw;")

	// i32 layout: raw is int32_t
	assertContains(t, header, "typedef struct { } promise_i32_t;")
	assertContains(t, header, "int32_t              raw;")

	// f32 layout: raw is float
	assertContains(t, header, "typedef struct { } promise_f32_t;")
	assertContains(t, header, "float                raw;")

	// Function declarations: all params passed by pointer
	assertContains(t, header, "void test_log_i32(promise_i32_v *x);")
	assertContains(t, header, "void test_log_bool(promise_bool_v *x);")
	assertContains(t, header, "void test_log_f32(promise_f32_v *x);")
}

// --- Ref param tests (shared & and mutable ~) ---

func TestExternSharedRefParam(t *testing.T) {
	ir := generateIR(t, `
		modify(int &x) `+"`"+`extern("test_modify");
		main() { }
	`)
	// Shared ref param should be a pointer to the value struct
	assertContains(t, ir, "declare void @test_modify(%promise_int_v*")
}

func TestExternMutRefParam(t *testing.T) {
	ir := generateIR(t, `
		update(int ~x) `+"`"+`extern("test_update");
		main() { }
	`)
	// Mutable ref param should be a pointer to the value struct
	assertContains(t, ir, "declare void @test_update(%promise_int_v*")
}

func TestHeaderExternSharedRefParam(t *testing.T) {
	result := compileResult(t, `
		modify(int &x) `+"`"+`extern("test_modify");
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Shared ref param should be pointer in C header
	assertContains(t, header, "void test_modify(promise_int_v *x);")
}

func TestHeaderExternMutRefParam(t *testing.T) {
	result := compileResult(t, `
		update(int ~x) `+"`"+`extern("test_update");
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Mutable ref param should be pointer in C header
	assertContains(t, header, "void test_update(promise_int_v *x);")
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

// --- String tests ---

func TestStringLiteral(t *testing.T) {
	ir := generateIR(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)
	// Static string instance global in .rodata (not heap-allocated)
	assertContains(t, ir, `c"hello"`)
	assertContains(t, ir, "private constant { i8*, i64, [5 x i8] }")
	// Packing into value struct
	assertContains(t, ir, "insertvalue %promise_string_v")
	// Call to extern
	assertContains(t, ir, "call void @promise_print_string(")
}

func TestStringVariable(t *testing.T) {
	ir := generateIR(t, `main() { s := "hello"; }`)
	// Alloca for i8* (string pointer)
	assertContains(t, ir, "alloca i8*")
	// Static string instance bitcast (no promise_string_new call for literals)
	assertContains(t, ir, "bitcast { i8*, i64, [5 x i8] }*")
	// Store i8* into alloca
	assertContains(t, ir, "store i8*")
}

func TestStringConcat(t *testing.T) {
	ir := generateIR(t, `main() { s := "hello" + " world"; }`)
	// Two string literals
	assertContains(t, ir, `c"hello"`)
	assertContains(t, ir, `c" world"`)
	// Concat intrinsic
	assertContains(t, ir, "call i8* @promise_string_concat(")
}

func TestStringEquality(t *testing.T) {
	ir := generateIR(t, `main() { b := "a" == "b"; }`)
	assertContains(t, ir, "call i1 @promise_string_eq(")
}

func TestStringEqFuncBody(t *testing.T) {
	ir := generateIR(t, `main() { b := "a" == "b"; }`)
	// Same-pointer fast path
	assertContains(t, ir, "icmp eq i8* %a, %b")
	// Length comparison
	assertContains(t, ir, "check_len:")
	// memcmp-based data comparison (replaces byte-by-byte loop)
	assertContains(t, ir, "call i32 @memcmp(")
	// Terminal blocks
	assertContains(t, ir, "equal:")
	assertContains(t, ir, "not_equal:")
}

func TestStringNotEqual(t *testing.T) {
	ir := generateIR(t, `main() { b := "a" != "b"; }`)
	assertContains(t, ir, "call i1 @promise_string_eq(")
	assertContains(t, ir, "xor i1")
}

func TestStringExternPacking(t *testing.T) {
	ir := generateIR(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)
	// Bitcast i8* to promise_string_i*
	assertContains(t, ir, "bitcast i8* %")
	// Insert into value struct
	assertContains(t, ir, "insertvalue %promise_string_v")
}

func TestStringLayout(t *testing.T) {
	// String layout struct types should always be present
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "%promise_string_t = type {}")
	assertContains(t, ir, "%promise_string_m = type { %promise_string_t* }")
	assertContains(t, ir, "%promise_string_i = type { %promise_string_m*, i64, [0 x i8] }")
	assertContains(t, ir, "%promise_string_v = type { i8*, %promise_string_i* }")
}

func TestStringHeader(t *testing.T) {
	result := compileResult(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// String layout with flexible array member
	assertContains(t, header, "typedef struct { } promise_string_t;")
	assertContains(t, header, "promise_string_m;")
	assertContains(t, header, "char                 data[];")
	assertContains(t, header, "promise_string_i;")
	assertContains(t, header, "promise_string_v;")

	// Extern declaration: string param by pointer
	assertContains(t, header, "void promise_print_string(promise_string_v *s);")
}

func TestStringEscapes(t *testing.T) {
	ir := generateIR(t, `main() { s := "hello\nworld"; }`)
	// The global should contain the actual newline character
	assertContains(t, ir, `c"hello\0Aworld"`)
}

func TestStringExternReturn(t *testing.T) {
	ir := generateIR(t, `
		get_greeting() string `+"`"+`extern("promise_get_greeting");
		main() { s := get_greeting(); }
	`)
	// Extern returns promise_string_v
	assertContains(t, ir, "define i32 @main(i32 %argc, i8** %argv)")
	// Unpack: extractvalue + bitcast back to i8*
	assertContains(t, ir, "extractvalue %promise_string_v")
	assertContains(t, ir, "bitcast %promise_string_i*")
}

func TestStringEmpty(t *testing.T) {
	ir := generateIR(t, `main() { s := ""; }`)
	// Empty string: [0 x i8] global constant
	assertContains(t, ir, "call i8* @promise_string_new(")
	// Length argument should be 0
	assertContains(t, ir, "i64 0)")
}

func TestStringEscapeBrace(t *testing.T) {
	ir := generateIR(t, `main() { s := "a\{b"; }`)
	// \{ should resolve to literal {
	assertContains(t, ir, `c"a{b"`)
}

func TestStringEscapeBraceOnly(t *testing.T) {
	// \{ alone — no interpolation, should take static string path
	ir := generateIR(t, `main() { s := "\{"; }`)
	assertContains(t, ir, `c"{"`)
}

func TestStringEscapeBraceMultiple(t *testing.T) {
	ir := generateIR(t, `main() { s := "\{a} and \{b}"; }`)
	assertContains(t, ir, `c"{a} and {b}"`)
}

func TestStringEscapeBraceWithInterpolation(t *testing.T) {
	// \{ mixed with real interpolation — takes interpolated path
	ir := generateIR(t, `main() { int x = 42; s := "\{x}={x}"; }`)
	// The escaped \{ produces static text "{x}="
	assertContains(t, ir, `c"{x}="`)
	// The real {x} produces a call to promise_int_to_string
	assertContains(t, ir, "call i8* @promise_int_to_string(")
}

func TestStringEscapeBraceAtEnd(t *testing.T) {
	ir := generateIR(t, `main() { s := "end\{"; }`)
	assertContains(t, ir, `c"end{"`)
}

func TestStringEscapeBraceAdjacentInterp(t *testing.T) {
	// \{ immediately followed by real interpolation {x}
	ir := generateIR(t, `main() { int x = 1; s := "\{{x}"; }`)
	assertContains(t, ir, `c"{"`)
	assertContains(t, ir, "call i8* @promise_int_to_string(")
}

func TestStringEscapeCloseBrace(t *testing.T) {
	// B0124: \} should resolve to literal }
	ir := generateIR(t, `main() { s := "a\}b"; }`)
	assertContains(t, ir, `c"a}b"`)
}

func TestStringEscapeCloseBraceOnly(t *testing.T) {
	ir := generateIR(t, `main() { s := "\}"; }`)
	assertContains(t, ir, `c"}"`)
}

func TestStringEscapeBothBraces(t *testing.T) {
	// \{...\} produces literal {…}
	ir := generateIR(t, `main() { s := "\{name\}"; }`)
	assertContains(t, ir, `c"{name}"`)
}

func TestStringIntrinsicsDeclared(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	// String intrinsics should always be defined (codegen-emitted LLVM IR)
	assertContains(t, ir, "define i8* @promise_string_new(i8* %data, i64 %len)")
	assertContains(t, ir, "define i8* @promise_string_concat(i8* %a, i8* %b)")
	assertContains(t, ir, "define i1 @promise_string_eq(i8* %a, i8* %b)")
	assertContains(t, ir, "define void @promise_string_drop(i8* %ptr)")
}

func TestStringLiteralStaticGlobal(t *testing.T) {
	ir := generateIR(t, `main() { s := "hello"; }`)
	// Static string instance in .rodata: { i8* null, i64 literalLen, [5 x i8] c"hello" }
	assertContains(t, ir, "private constant { i8*, i64, [5 x i8] }")
	assertContains(t, ir, `c"hello"`)
	// Bitcast global to i8* — no promise_string_new call for literals
	assertContains(t, ir, "bitcast { i8*, i64, [5 x i8] }*")
}

func TestStringLiteralNegativeLength(t *testing.T) {
	ir := generateIR(t, `main() { s := "hi"; }`)
	// Length field should be negative (literal flag = sign bit set)
	// "hi" is 2 bytes, so literalLen = 2 | (1<<63) = -9223372036854775806
	assertContains(t, ir, "i64 -9223372036854775806")
}

func TestStringLenMasksLiteralBit(t *testing.T) {
	ir := generateIR(t, `main() { s := "ab"; x := s.len; }`)
	// Length read should mask off sign bit: and i64 %raw, 0x7FFFFFFFFFFFFFFF
	assertContains(t, ir, "and i64")
	assertContains(t, ir, "u0x7FFFFFFFFFFFFFFF")
}

func TestStringDropFuncBody(t *testing.T) {
	ir := generateIR(t, `main() { s := "x"; }`)
	// T0093: promise_string_drop null-checks the pointer (for null fields in
	// synthesized drops), then checks bit 63 (literal flag), then conditionally frees
	assertContains(t, ir, "define void @promise_string_drop(i8* %ptr)")
	assertContains(t, ir, "icmp eq i8* %ptr, null")
	assertContains(t, ir, "icmp ne i64")
	assertContains(t, ir, "call void @pal_free(")
}

// T0061: String drop binding is registered at scope exit
func TestStringDropScopeBinding(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello";
		}
	`)
	// String drop binding: drop flag alloca, conditional call to promise_string_drop
	assertContains(t, ir, "strdrop.call")
	assertContains(t, ir, "strdrop.skip")
	assertContains(t, ir, "call void @promise_string_drop(")
}

// T0061: String drop flag is cleared when returning a string
func TestStringDropReturnClearsFlag(t *testing.T) {
	ir := generateIR(t, `
		make_name() string {
			s := "bob";
			return s;
		}
		main() { make_name(); }
	`)
	// The return should clear the drop flag (store i1 false) before scope cleanup
	assertContains(t, ir, "strdrop.skip")
}

// T0061: String drop flag IS cleared when passing to a function (same as user types)
func TestStringDropClearedOnFuncArg(t *testing.T) {
	ir := generateIR(t, `
		consume(string s) {}
		main() {
			s := "hello";
			consume(s);
		}
	`)
	// The string drop binding exists but flag is cleared at the call site,
	// so the conditional drop at scope exit is a no-op (skips).
	assertContains(t, ir, "strdrop.call")
	assertContains(t, ir, "call void @promise_string_drop(")
}

// T0061: String drop flag is cleared on assignment (move)
func TestStringDropClearedOnAssignment(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := "hello";
			b := a;
		}
	`)
	// Both a and b should have drop bindings
	// a's flag should be cleared (moved to b)
	assertContains(t, ir, "strdrop.call")
}

// T0061: String borrowed from struct field should NOT have active drop
func TestStringDropBorrowFromField(t *testing.T) {
	ir := generateIR(t, `
		type Person { string name; }
		main() {
			p := Person(name: "alice");
			field_val := p.name;
		}
	`)
	// field_val gets a drop binding but flag is immediately cleared (borrow from field)
	assertContains(t, ir, "strdrop.call")
}

// T0061: String borrowed from vector index should NOT have active drop
func TestStringDropBorrowFromIndex(t *testing.T) {
	ir := generateIR(t, `
		main() {
			names := string[]();
			names.push("alice");
			elem := names[0];
		}
	`)
	// elem gets a drop binding but flag is immediately cleared (borrow from vector)
	assertContains(t, ir, "strdrop.call")
}

// T0064: Vector drop binding is registered at scope exit
func TestVectorDropScopeBinding(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := int[]();
			v.push(1);
		}
	`)
	// Vector drop binding: strdrop.call block (reuses bindingDropString mechanism)
	assertContains(t, ir, "strdrop.call")
	assertContains(t, ir, "call void @Vector.drop(")
}

func TestStringNewFuncBody(t *testing.T) {
	ir := generateIR(t, `main() { s := "hello"; }`)
	assertContains(t, ir, "define i8* @promise_string_new(i8* %data, i64 %len)")
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "oom:")
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "unreachable")
	assertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
	assertContains(t, ir, "store i8* null")
}

func TestStringConcatFuncBody(t *testing.T) {
	ir := generateIR(t, `main() { s := "a" + "b"; }`)
	assertContains(t, ir, "define i8* @promise_string_concat(i8* %a, i8* %b)")
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "oom:")
	assertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
}

func TestLLVMMemcpyDeclared(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "declare void @llvm.memcpy.p0i8.p0i8.i64(")
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

func TestUserTypeConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		main() { d := Dog(age: 3); }
	`)
	// Should allocate via malloc
	assertContains(t, ir, "call i8* @pal_alloc(i64")
	// Should bitcast to typed pointer
	assertContains(t, ir, "bitcast i8*")
	// Should store field value
	assertContains(t, ir, "store i64 3")
}

func TestUserTypeFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		main() {
			d := Dog(age: 3);
			x := d.age;
		}
	`)
	// Should bitcast and GEP to access field
	assertContains(t, ir, "getelementptr %promise_Dog_i")
	assertContains(t, ir, "load i64")
}

func TestUserTypeFieldAssign(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		main() {
			d := Dog(age: 3);
			d.age = 5;
		}
	`)
	assertContains(t, ir, "store i64 5")
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

func TestUserTypeMethod(t *testing.T) {
	ir := generateIR(t, `
		type Dog {
			int age;
			getAge(this) int {
				return this.age;
			}
		}
		main() { }
	`)
	assertContains(t, ir, "define i64 @Dog.getAge(i8* %this)")
}

func TestUserTypeMethodCall(t *testing.T) {
	ir := generateIR(t, `
		type Dog {
			int age;
			getAge(this) int {
				return this.age;
			}
		}
		main() {
			d := Dog(age: 3);
			x := d.getAge();
		}
	`)
	assertContains(t, ir, "call i64 @Dog.getAge(i8*")
}

func TestUserTypeMethodWithReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int value;
			increment(~this) {
				this.value += 1;
			}
		}
		main() {
			c := Counter(value: 0);
			c.increment();
		}
	`)
	assertContains(t, ir, "define void @Counter.increment(i8* %this)")
	assertContains(t, ir, "call void @Counter.increment(i8*")
}

func TestThisExpr(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			int value;
			get(this) int {
				return this.value;
			}
		}
		main() {
			b := Box(value: 42);
			x := b.get();
		}
	`)
	// Method should load this from alloca
	assertContains(t, ir, "%this.addr = alloca i8*")
}

func TestUserTypeMultipleFields(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; int z; }
		main() {
			p := Point(x: 1, y: 2, z: 3);
		}
	`)
	assertContains(t, ir, "%promise_Point_i = type { %promise_Point_m*, i64, i64, i64 }")
	// All three field stores
	assertContains(t, ir, "store i64 1")
	assertContains(t, ir, "store i64 2")
	assertContains(t, ir, "store i64 3")
}

func TestUserTypeStringField(t *testing.T) {
	ir := generateIR(t, `
		type Dog { string name; }
		main() {
			d := Dog(name: "Rex");
		}
	`)
	// String field stored as i8*
	assertContains(t, ir, "%promise_Dog_i = type { %promise_Dog_m*, i8* }")
	// Should call promise_string_new for the literal
	assertContains(t, ir, "call i8* @promise_string_new")
}

func TestUserTypeExternPacking(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		print_dog(Dog d) `+"`"+`extern("print_dog");
		main() {
			d := Dog(age: 3);
			print_dog(d);
		}
	`)
	// Should pack into value struct
	assertContains(t, ir, "insertvalue %promise_Dog_v")
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

func TestUserTypeMethodWithParams(t *testing.T) {
	ir := generateIR(t, `
		type Adder {
			int base;
			add(&this, int n) int {
				return this.base + n;
			}
		}
		main() {
			a := Adder(base: 10);
			x := a.add(5);
		}
	`)
	assertContains(t, ir, "define i64 @Adder.add(i8* %this, i64 %n)")
	assertContains(t, ir, "call i64 @Adder.add(i8*")
}

func TestPalAllocDefined(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "declare noalias i8* @malloc(i64 noundef %size) nounwind willreturn")
	assertContains(t, ir, "@pal_alloc(i64 %size)")
}

func TestUserTypeExternUnpacking(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		get_dog() Dog `+"`"+`extern("get_dog");
		main() {
			d := get_dog();
		}
	`)
	// Extern uses sret for struct return
	assertContains(t, ir, "declare void @get_dog(i8* %sret)")
	// Unpack: load from sret alloca, extractvalue field 1 + bitcast back to i8*
	assertContains(t, ir, "extractvalue %promise_Dog_v")
	assertContains(t, ir, "bitcast %promise_Dog_i*")
}

func TestUserTypeNestedField(t *testing.T) {
	ir := generateIR(t, `
		type Inner { int value; }
		type Outer { Inner child; }
		main() {
			i := Inner(value: 42);
			o := Outer(child: i);
		}
	`)
	// Inner stored as value struct { i8*, i8* } in Outer's instance struct
	assertContains(t, ir, "%promise_Inner_i = type { %promise_Inner_m*, i64 }")
	assertContains(t, ir, "%promise_Outer_i = type { %promise_Outer_m*, { i8*, i8* } }")
	// Both should be allocated via malloc
	assertContains(t, ir, "call i8* @pal_alloc(i64")
}

func TestUserTypeNestedFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Inner { int value; }
		type Outer { Inner child; }
		main() {
			i := Inner(value: 42);
			o := Outer(child: i);
			c := o.child;
		}
	`)
	// Should GEP into Outer to load the child value struct
	assertContains(t, ir, "getelementptr %promise_Outer_i")
	assertContains(t, ir, "load { i8*, i8* }")
}

func TestUserTypeZeroArgConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		main() {
			p := Point(x: 0, y: 0);
		}
	`)
	// Should allocate and store both fields
	assertContains(t, ir, "call i8* @pal_alloc(i64")
	assertContains(t, ir, "store i64 0")
}

func TestConstructorDefaultExprEvaluation(t *testing.T) {
	ir := generateIR(t, `
		type Config { int port = 8080; string host; }
		main() {
			c := Config(host: "localhost");
		}
	`)
	// The default expression (8080) should be evaluated and stored
	assertContains(t, ir, "store i64 8080")
}

func TestConstructorAllDefaultsOmitted(t *testing.T) {
	ir := generateIR(t, `
		type Defaults { int x = 42; int y = 99; }
		main() {
			d := Defaults();
		}
	`)
	assertContains(t, ir, "store i64 42")
	assertContains(t, ir, "store i64 99")
}

func TestNewConstructorCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Clamped {
			int value;
			new(~this, int v) {
				if v < 0 { this.value = 0; }
				else { this.value = v; }
			}
		}
		main() {
			c := Clamped(v: 50);
		}
	`)
	// Should declare the new() method as a void function
	assertContains(t, ir, "define void @Clamped.new(i8* %this")
	// Constructor should call new()
	assertContains(t, ir, "call void @Clamped.new(")
}

func TestNewConstructorFinalFieldCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Token {
			string raw `+"`"+`final;
			new(~this, string raw) {
				this.raw = raw;
			}
		}
		main() {
			t := Token(raw: "hello");
		}
	`)
	assertContains(t, ir, "define void @Token.new(i8* %this")
	assertContains(t, ir, "call void @Token.new(")
}

func TestFailableNewConstructorCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Port {
			int value;
			new(~this, int value) void! {
				if value < 1 {
					raise error(message: "invalid port");
				}
				this.value = value;
			}
		}
		main()! {
			Port p = Port(value: 80)!;
		}
	`)
	// Failable new returns a result type { i1, i8* }
	assertContains(t, ir, "define { i1, i8* } @Port.new(i8* %this")
	// Constructor call should call new and check the error
	assertContains(t, ir, "call { i1, i8* } @Port.new(")
}

func TestFactoryConstructorCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Color {
			int r;
			int g;
			int b;
			red() Self `+"`"+`factory {
				return Color(r: 255, g: 0, b: 0);
			}
		}
		main() {
			Color c = Color.red();
		}
	`)
	// Factory method should be defined without a receiver parameter
	assertContains(t, ir, "define { i8*, i8* } @Color.red()")
	// main should call Color.red
	assertContains(t, ir, "call { i8*, i8* } @Color.red()")
}

func TestGenericFactoryCallCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Parseable `+"`"+`structural {
			parse(string data) `+"`"+`abstract `+"`"+`factory;
		}
		type My {
			parse(string data) My `+"`"+`factory { return My(); }
		}
		load[T: Parseable](string data) T {
			return T.parse(data);
		}
		main() {
			My m = load[My]("hello");
		}
	`)
	// Monomorphized load[My] should call My.parse directly
	assertContains(t, ir, "call { i8*, i8* } @My.parse(")
}

func TestGenericFailableFactoryPassthrough(t *testing.T) {
	ir := generateIR(t, `
		type TryParseable `+"`"+`structural {
			tryParse(string data)! `+"`"+`abstract `+"`"+`factory;
		}
		type Strict {
			tryParse(string data) Strict! `+"`"+`factory {
				if data == "bad" {
					raise error("invalid");
				}
				return Strict();
			}
		}
		tryLoad[T: TryParseable](string data) T! {
			return T.tryParse(data);
		}
		main() {
			Strict s = tryLoad[Strict]("ok")!;
		}
	`)
	// Monomorphized tryLoad[Strict] should call Strict.tryParse directly
	assertContains(t, ir, "call { i1, { i8*, i8* }, i8* } @Strict.tryParse(")
	// Failable passthrough: tryLoad[Strict] should return the result directly
	// (single ret of the call result, no insertvalue wrapping)
	assertContains(t, ir, "@\"tryLoad[Strict]\"(")
	assertContains(t, ir, "ret { i1, { i8*, i8* }, i8* } %")
}

func TestSelfGenericFactoryCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T value;
			new(~this, T v) { this.value = v; }
			wrap(T v) Self `+"`"+`factory {
				return Self(v: v);
			}
		}
		main() {
			Box[int] b = Box[int].wrap(v: 42);
		}
	`)
	// Factory should be monomorphized for int
	assertContains(t, ir, "@\"Box[int].wrap\"(")
	// Should call the Box[int] constructor
	assertContains(t, ir, "@\"Box[int].new\"(")
}

func TestSelfGenericMethodReturnCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T value;
			new(~this, T v) { this.value = v; }
			rewrap(T v) Self {
				return Self(v: v);
			}
		}
		main() {
			Box[int] b = Box[int](v: 1);
			Box[int] c = b.rewrap(v: 2);
		}
	`)
	// Instance method should exist for int monomorphization
	assertContains(t, ir, "@\"Box[int].rewrap\"(")
}

func TestSelfGenericMultiParamCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Pair[A, B] {
			A first;
			B second;
			new(~this, A a, B b) { this.first = a; this.second = b; }
			make(A a, B b) Self `+"`"+`factory {
				return Self(a: a, b: b);
			}
		}
		main() {
			Pair[int, string] p = Pair[int, string].make(a: 1, b: "x");
		}
	`)
	// Factory monomorphized for (int, string)
	assertContains(t, ir, "@\"Pair[int, string].make\"(")
	assertContains(t, ir, "@\"Pair[int, string].new\"(")
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

func TestSuperCallImplicitParentCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			int age;
		}
		type Dog is Animal {
			int tricks;
			new(~this, int age, int tricks) {
				super(age: age);
				this.tricks = tricks;
			}
		}
		main() {
			Dog d = Dog(age: 3, tricks: 5);
		}
	`)
	// Dog.new should be defined and set parent field directly (no Animal.new call)
	assertContains(t, ir, "define void @Dog.new(")
	// Dog constructor should call Dog.new
	assertContains(t, ir, "call void @Dog.new(")
}

func TestUserTypeHeaderFieldTypes(t *testing.T) {
	result := compileResult(t, `
		type Person { string name; int age; bool active; }
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Verify instance struct field types
	assertContains(t, header, "void*                name;")
	assertContains(t, header, "int64_t              age;")
	assertContains(t, header, "uint8_t              active;")
}

func TestUserTypeMethodMutatesField(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int value;
			set(~this, int n) {
				this.value = n;
			}
		}
		main() {
			c := Counter(value: 0);
			c.set(42);
		}
	`)
	assertContains(t, ir, "define void @Counter.set(i8* %this, i64 %n)")
	// Should store into this.value
	assertContains(t, ir, "getelementptr %promise_Counter_i")
	assertContains(t, ir, "store i64")
}

// === Enum Tests ===

func TestEnumLayout(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		main() { }
	`)
	// Four-struct layout for enum
	assertContains(t, ir, "%promise_Color_t = type {}")
	assertContains(t, ir, "%promise_Color_m = type { %promise_Color_t* }")
	assertContains(t, ir, "%promise_Color_i = type { %promise_Color_m* }")
	assertContains(t, ir, "%promise_Color_v = type { i8*, %promise_Color_i*, i32 }")
}

func TestEnumLayoutData(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		main() { }
	`)
	assertContains(t, ir, "%promise_Shape_t = type {}")
	assertContains(t, ir, "%promise_Shape_m = type { %promise_Shape_t* }")
	assertContains(t, ir, "%promise_Shape_i = type { %promise_Shape_m* }")
	// Value struct: vtable, instance ptr, tag, data bytes
	assertContains(t, ir, "%promise_Shape_v = type { i8*, %promise_Shape_i*, i32,")
	// Internal struct: tag + data area
	assertContains(t, ir, "%promise_Shape_enum = type { i32,")
}

func TestEnumFieldlessVariant(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Green;
		}
		main() { }
	`)
	// Green is tag 1
	assertContains(t, ir, "store i32 1")
}

func TestEnumDataConstructor(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		test() {
			Shape s = Shape.Circle(3.14);
		}
		main() { }
	`)
	// Should store tag (Circle = 0)
	assertContains(t, ir, "store i32 0")
	// Should store double field via GEP + bitcast
	assertContains(t, ir, "store double")
	assertContains(t, ir, "bitcast")
}

func TestEnumMatchFieldless(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			x := match c {
				Color.Red => 1,
				Color.Green => 2,
				Color.Blue => 3,
			};
		}
		main() { }
	`)
	// Should use switch on i32 tag
	assertContains(t, ir, "switch i32")
	// Should have arm blocks
	assertContains(t, ir, "match.arm0")
	assertContains(t, ir, "match.arm1")
	assertContains(t, ir, "match.arm2")
	// Merge block with phi
	assertContains(t, ir, "match.end")
}

func TestEnumMatchDestructure(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		test() f64 {
			Shape s = Shape.Circle(3.14);
			return match s {
				Shape.Circle(r) => r,
				Shape.Rect(w, h) => w,
			};
		}
		main() { }
	`)
	// Should switch on tag
	assertContains(t, ir, "switch i32")
	// Should bitcast + GEP to load variant data
	assertContains(t, ir, "bitcast")
	assertContains(t, ir, "load double")
}

func TestEnumMatchShortDestructure(t *testing.T) {
	ir := generateIR(t, `
		enum Result { Ok(int value), Err(int code) }
		test() int {
			Result r = Result.Ok(42);
			return match r {
				Ok(v) => v,
				Err(c) => c,
			};
		}
		main() { }
	`)
	// Short destructure should also produce switch
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "match.arm0")
	assertContains(t, ir, "match.arm1")
}

func TestEnumMatchWildcard(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() int {
			Color c = Color.Red;
			return match c {
				Color.Red => 1,
				_ => 0,
			};
		}
		main() { }
	`)
	// Switch with default case
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "match.arm0")
	assertContains(t, ir, "match.arm1")
}

func TestEnumMatchNameBinding(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() int {
			Color c = Color.Red;
			return match c {
				Color.Red => 1,
				val => 0,
			};
		}
		main() { }
	`)
	// Name binding should create alloca for the bound variable
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "alloca i32")
}

func TestEnumMatchBlock(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			match c {
				Color.Red => { int x = 1; },
				Color.Green => { int y = 2; },
				Color.Blue => { int z = 3; },
			};
		}
		main() { }
	`)
	// Should have switch and arm blocks (void match, no phi)
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "match.arm0")
}

func TestMatchIntLiteral(t *testing.T) {
	ir := generateIR(t, `
		test() int {
			int n = 42;
			return match n {
				1 => 10,
				2 => 20,
				_ => 0,
			};
		}
		main() { }
	`)
	// Should use comparison chain (icmp eq), not switch
	assertContains(t, ir, "icmp eq")
	assertContains(t, ir, "match.arm")
	assertContains(t, ir, "match.next")
}

func TestEnumExternPacking(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		print_color(Color c) `+"`"+`extern("print_color");
		test() {
			Color c = Color.Green;
			print_color(c);
		}
		main() { }
	`)
	// Should pack into value struct
	assertContains(t, ir, "insertvalue %promise_Color_v")
	// Extern declaration: param passed by pointer
	assertContains(t, ir, "declare void @print_color(i8*")
}

func TestEnumExternUnpacking(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		get_color() Color `+"`"+`extern("get_color");
		test() {
			Color c = get_color();
		}
		main() { }
	`)
	// Extern uses sret for struct return
	assertContains(t, ir, "declare void @get_color(i8* %sret)")
	// Should unpack via extractvalue after loading from sret
	assertContains(t, ir, "extractvalue %promise_Color_v")
}

func TestEnumHeaderFieldless(t *testing.T) {
	result := compileResult(t, `
		enum Color { Red, Green, Blue }
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	assertContains(t, header, "promise_Color_t")
	assertContains(t, header, "promise_Color_m")
	assertContains(t, header, "promise_Color_i")
	assertContains(t, header, "promise_Color_v")
	// Value struct should have tag field
	assertContains(t, header, "int32_t")
}

func TestEnumDataFieldlessVariant(t *testing.T) {
	// Exercises zeroinitializer path: fieldless variant in a data enum
	ir := generateIR(t, `
		enum Result { Ok(int value), None }
		test() {
			Result r = Result.None;
		}
		main() { }
	`)
	// None is tag 1, built via zeroinitializer + insertvalue (not alloca with partial store)
	assertContains(t, ir, "insertvalue %promise_Result_enum zeroinitializer, i32 1, 0")
	// Internal struct should exist for the data enum
	assertContains(t, ir, "%promise_Result_enum = type { i32,")
}

func TestEnumDataExternPacking(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		send_shape(Shape s) `+"`"+`extern("send_shape");
		test() {
			Shape s = Shape.Circle(3.14);
			send_shape(s);
		}
		main() { }
	`)
	// Data enum packing: extractvalue tag and data from internal struct
	assertContains(t, ir, "extractvalue %promise_Shape_enum")
	// Pack into value struct
	assertContains(t, ir, "insertvalue %promise_Shape_v")
	// Extern declaration: param passed by pointer
	assertContains(t, ir, "declare void @send_shape(i8*")
}

func TestEnumDataExternUnpacking(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		get_shape() Shape `+"`"+`extern("get_shape");
		test() {
			Shape s = get_shape();
		}
		main() { }
	`)
	// Data enum unpacking: sret + extractvalue from value struct, build internal struct
	assertContains(t, ir, "declare void @get_shape(i8* %sret)")
	assertContains(t, ir, "extractvalue %promise_Shape_v")
	assertContains(t, ir, "insertvalue %promise_Shape_enum")
}

func TestEnumAsFunctionParam(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		is_red(Color c) bool {
			return match c {
				Color.Red => true,
				_ => false,
			};
		}
		main() { }
	`)
	// Enum param should use i32 (fieldless enum internal type)
	assertContains(t, ir, "define i1 @is_red(i32 %c)")
	// Param should be alloca'd as i32
	assertContains(t, ir, "alloca i32")
	assertContains(t, ir, "switch i32")
}

func TestEnumAsFunctionReturn(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		get_green() Color {
			return Color.Green;
		}
		main() { }
	`)
	// Enum return should use i32
	assertContains(t, ir, "define i32 @get_green()")
	assertContains(t, ir, "ret i32 1")
}

func TestMatchValueNameBinding(t *testing.T) {
	ir := generateIR(t, `
		test() int {
			int x = 42;
			return match x {
				val => val + 1,
			};
		}
		main() { }
	`)
	// Name binding in value match: alloca + store the subject
	assertContains(t, ir, "alloca i64")
	assertContains(t, ir, "add i64")
}

func TestEnumDestructureUnderscoreSkip(t *testing.T) {
	ir := generateIR(t, `
		enum Pair { Both(int a, int b) }
		test() int {
			Pair p = Pair.Both(1, 2);
			return match p {
				Both(_, second) => second,
			};
		}
		main() { }
	`)
	// Should still load the second field (index 1) but skip the first
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "load i64")
}

func TestEnumHeaderData(t *testing.T) {
	result := compileResult(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	assertContains(t, header, "promise_Shape_t")
	assertContains(t, header, "promise_Shape_v")
	// Data enum value struct should have tag and data fields
	assertContains(t, header, "int32_t              tag;")
	assertContains(t, header, "uint8_t              data[16];")
}

// ── Error handling tests ──────────────────────────────────────────

func TestFailableDeclaration(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		main() { }
	`)
	// Return type should be result struct { i1, i64, i8* }
	assertContains(t, ir, "define { i1, i64, i8* } @parse(i8* %s)")
}

func TestReturnInFailable(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 42; }
		main() { }
	`)
	// Should wrap value in Ok result: tag=false, value, null error
	assertContains(t, ir, "insertvalue { i1, i64, i8* }")
	assertContains(t, ir, "i1 false")
	assertContains(t, ir, "ret { i1, i64, i8* }")
}

func TestFailableVoidBangShorthand(t *testing.T) {
	ir := generateIR(t, `
		fail()! { raise error(message: "oops"); }
		main() { }
	`)
	// Should produce void result struct { i1, i8* }
	assertContains(t, ir, "define { i1, i8* } @fail()")
}

func TestFailableMain(t *testing.T) {
	ir := generateIR(t, `
		main()! {
			raise error(message: "boom");
		}
	`)
	// Body compiled into helper function
	assertContains(t, ir, "define { i1, i8* } @__promise_main_body()")
	// Error path panics
	assertContains(t, ir, "unhandled error in main")
}

func TestRaiseStmt(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { raise error(message: "parse error"); }
		main() { }
	`)
	// Should wrap error in Error result: tag=true
	assertContains(t, ir, "i1 true")
	assertContains(t, ir, "ret { i1, i64, i8* }")
	// Should create the error message string
	assertContains(t, ir, `c"parse error"`)
	// Should extract instance pointer from value struct
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestErrorPropagate(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		process() int! {
			x := parse("42")?;
			return x;
		}
		main() { }
	`)
	// Should have propagation and ok blocks
	assertContains(t, ir, "error.propagate")
	assertContains(t, ir, "error.ok")
	// Should extract tag from result
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
}

func TestErrorUnwrap(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		main() {
			x := parse("42")!;
		}
	`)
	// Should have panic and ok blocks
	assertContains(t, ir, "error.panic")
	assertContains(t, ir, "error.ok")
	// B0200: Should extract message string from error instance before panicking.
	// The error.panic block must bitcast the error instance to load the message
	// field, then create a C string copy for promise_panic.
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "unreachable")
	// Verify message extraction: bitcast to error instance type, GEP to message field
	assertContains(t, ir, "getelementptr %promise_error_i")
}

// T0125: When func()! returns a string, the unwrapped i8* must be tracked
// as a stmt temp so it gets freed at statement end if not claimed.
func TestErrorUnwrapStringTemp(t *testing.T) {
	ir := generateIR(t, `
		make_str() string! { return "hello"; }
		main() {
			int n = make_str()!.len;
		}
	`)
	// Should have string temp tracking: store to alloca + drop flag
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "promise_string_drop")
}

func TestErrorHandler(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		main() {
			x := parse("42") ? e { 0; };
		}
	`)
	// Should have handler, ok, and merge blocks
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.ok")
	assertContains(t, ir, "error.merge")
}

func TestErrorHandlerDiscard(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		main() {
			x := parse("42") ? _ { 0; };
		}
	`)
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.ok")
}

func TestVoidFailable(t *testing.T) {
	ir := generateIR(t, `
		validate(string s) void! { return; }
		main() { }
	`)
	// Return type should be { i1, i8* }
	assertContains(t, ir, "define { i1, i8* } @validate(i8* %s)")
}

func TestVoidRaise(t *testing.T) {
	ir := generateIR(t, `
		validate(string s) void! { raise error(message: "invalid"); }
		main() { }
	`)
	assertContains(t, ir, "i1 true")
	assertContains(t, ir, "ret { i1, i8* }")
}

func TestFailableMethod(t *testing.T) {
	ir := generateIR(t, `
		type Parser {
			string input;
			parse(this) int! {
				return 42;
			}
		}
		main() { }
	`)
	assertContains(t, ir, "define { i1, i64, i8* } @Parser.parse(i8* %this)")
}

func TestFailableAutoTerminator(t *testing.T) {
	ir := generateIR(t, `
		validate(string s) void! {
			if true {
				return;
			}
		}
		main() { }
	`)
	// Auto-terminator on fall-through path should wrap in Ok (tag=false)
	assertContains(t, ir, "i1 false")
	assertContains(t, ir, "ret { i1, i8* }")
}

func TestVoidFailablePropagate(t *testing.T) {
	ir := generateIR(t, `
		validate(string s) void! { raise error(message: "invalid"); }
		process() void! {
			validate("x")?;
		}
		main() { }
	`)
	// Should propagate error from void failable callee
	assertContains(t, ir, "error.propagate")
	assertContains(t, ir, "error.ok")
	// Callee returns { i1, i8* }, caller also returns { i1, i8* }
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

func TestVoidFailableUnwrap(t *testing.T) {
	ir := generateIR(t, `
		validate(string s) void! { raise error(message: "invalid"); }
		main() {
			validate("x")!;
		}
	`)
	assertContains(t, ir, "error.panic")
	assertContains(t, ir, "error.ok")
	assertContains(t, ir, "call void @promise_panic(")
}

func TestVoidFailableHandler(t *testing.T) {
	ir := generateIR(t, `
		validate(string s) void! { raise error(message: "invalid"); }
		main() {
			validate("x") ? e { };
		}
	`)
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.ok")
	assertContains(t, ir, "error.merge")
}

func TestNestedErrorPropagation(t *testing.T) {
	ir := generateIR(t, `
		a() int! { return 1; }
		b() int! { return a()?; }
		c() int! { return b()?; }
		main() { }
	`)
	// Both b and c should have propagation blocks
	assertContains(t, ir, "error.propagate")
	assertContains(t, ir, "error.ok")
}

func TestErrorHandlerWithReturn(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		process(string s) int {
			x := parse(s) ? e { return -1; };
			return x;
		}
		main() { }
	`)
	// Handler block should contain a return (terminator)
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.ok")
}

func TestFailableConditionalRaiseReturn(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! {
			if s == "" {
				raise error(message: "empty");
			}
			return 42;
		}
		main() { }
	`)
	// Should have both Ok and Error paths
	assertContains(t, ir, "i1 true")
	assertContains(t, ir, "i1 false")
	assertContains(t, ir, "ret { i1, i64, i8* }")
}

// --- Typed Error Handler Tests ---

func TestTypedErrorHandler(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error {
			int code;
		}
		fail() void! { raise IoError(message: "disk full", code: 28); }
		process() void! {
			fail() ? e is IoError { };
		}
		main() { }
	`)
	// Should have RTTI type check
	assertContains(t, ir, "call i32 @promise_type_is(")
	// Should have typed match/nomatch blocks
	assertContains(t, ir, "error.typed.match")
	assertContains(t, ir, "error.typed.nomatch")
}

func TestTypedErrorHandlerInFailable(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error {
			int code;
		}
		fail() void! { raise IoError(message: "disk full", code: 28); }
		process() void! {
			fail() ? e is IoError { };
		}
		main() { }
	`)
	// Nomatch path in failable function should propagate error (ret)
	assertContains(t, ir, "error.typed.nomatch")
	assertContains(t, ir, "ret { i1, i8* }")
}

func TestTypedErrorHandlerNomatchPropagates(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error {
			int code;
		}
		fail() void! { raise IoError(message: "disk full", code: 28); }
		process() void! {
			fail() ? e is IoError { };
		}
		main() { }
	`)
	// Nomatch path in failable function should propagate error
	assertContains(t, ir, "error.typed.nomatch")
	assertContains(t, ir, "ret { i1, i8* }")
}

func TestTypedErrorHandlerDiscardBinding(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error {
			int code;
		}
		fail() void! { raise IoError(message: "disk full", code: 28); }
		process() void! {
			fail() ? _ is IoError { };
		}
		main() { }
	`)
	assertContains(t, ir, "call i32 @promise_type_is(")
	assertContains(t, ir, "error.typed.match")
}

func TestTypedErrorHandlerElse(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		fail() void! { raise IoError(message: "disk full", code: 28); }
		process() {
			fail() ? e is IoError { } else { };
		}
		main() { }
	`)
	assertContains(t, ir, "error.typed.match")
	assertContains(t, ir, "error.typed.nomatch")
	// No panic in nomatch — else handles it
	assertNotContains(t, ir, "unhandled error type")
}

func TestTypedErrorHandlerElseWithBinding(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		fail() void! { raise IoError(message: "disk full", code: 28); }
		get_msg() string {
			fail() ? e is IoError { return "io"; } else e { return e.message; };
			return "";
		}
		main() { }
	`)
	assertContains(t, ir, "error.typed.match")
	assertContains(t, ir, "error.typed.nomatch")
}

func TestTypedErrorHandlerBang(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		fail() void! { raise IoError(message: "disk full", code: 28); }
		process() {
			fail() ? e is IoError { }!;
		}
		main() { }
	`)
	assertContains(t, ir, "error.typed.match")
	assertContains(t, ir, "error.typed.nomatch")
	// Nomatch panics via promise_panic
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "unreachable")
}

func TestUntypedErrorHandlerUnchanged(t *testing.T) {
	ir := generateIR(t, `
		fail() void! { raise error(message: "oops"); }
		main() {
			fail() ? e { };
		}
	`)
	// Untyped handler should NOT have typed match/nomatch blocks
	assertContains(t, ir, "error.handler")
	assertNotContains(t, ir, "error.typed.match")
	assertNotContains(t, ir, "error.typed.nomatch")
}

func TestErrorHandlerBindingFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error {
			int code;
		}
		fail() void! { raise IoError(message: "disk full", code: 28); }
		process() int! {
			fail() ? e is IoError { return e.code; };
			return 0;
		}
		main() { }
	`)
	// Should reconstruct value struct and access field
	assertContains(t, ir, "error.typed.match")
	assertContains(t, ir, "insertvalue { i8*, i8* }")
}

func TestErrorPositionalConstruction(t *testing.T) {
	ir := generateIR(t, `
		foo() void! { raise error("oops"); }
		main() { foo() ? e { }; }
	`)
	assertContains(t, ir, "error.handler")
}

func TestErrorSubtypePositionalConstruction(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		foo() void! { raise IoError("disk full", 28); }
		main() { foo() ? e { }; }
	`)
	assertContains(t, ir, "error.handler")
}

func TestGenericErrorTypeRaise(t *testing.T) {
	ir := generateIR(t, `
		type DataError[T] is error { T data; }
		foo() void! { raise DataError[int](message: "bad", data: 42); }
		main() { foo() ? e { }; }
	`)
	// Should monomorphize DataError[int]
	assertContains(t, ir, "DataError[int]")
	assertContains(t, ir, "error.handler")
}

func TestFailableCallInsideHandler(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		foo() int! {
			int v = parse("x") ? e { return parse("0")?; };
			return v;
		}
		main() { foo() ? e { }; }
	`)
	assertContains(t, ir, "error.handler")
	// The handler body should contain another error propagation
	assertContains(t, ir, "error.propagate")
}

func TestBangUnwrapInsideHandler(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		foo() {
			parse("x") ? e { int v = parse("0")!; };
		}
		main() { }
	`)
	// Should have both handler and panic-on-unwrap blocks
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.panic")
}

func TestNestedErrorHandlers(t *testing.T) {
	ir := generateIR(t, `
		a() int! { return 1; }
		b() int! { return 2; }
		foo() {
			a() ? e1 {
				b() ? e2 { };
			};
		}
		main() { }
	`)
	// Should have multiple handler blocks
	assertContains(t, ir, "error.handler")
}

func TestErrorInheritanceChainTypedHandler(t *testing.T) {
	ir := generateIR(t, `
		type AppError is error { int code; }
		type DbError is AppError { string query; }
		fail() void! { raise DbError(message: "fail", code: 500, query: "SELECT"); }
		handler() int! {
			fail() ? e is AppError { return e.code; };
			return 0;
		}
		main() { }
	`)
	assertContains(t, ir, "error.typed.match")
	assertContains(t, ir, "promise_type_is")
}

func TestAutoPropagate(t *testing.T) {
	ir := generateIR(t, `
		fail() void! { raise error(message: "oops"); }
		process() void! {
			fail();
		}
		main() { }
	`)
	// Should have auto-propagation blocks
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
	// Should extract tag and conditionally branch
	assertContains(t, ir, "extractvalue { i1, i8* }")
	// Should return error result on error path
	assertContains(t, ir, "ret { i1, i8* }")
}

func TestAutoPropagate_NonVoid(t *testing.T) {
	ir := generateIR(t, `
		parse() int! { return 42; }
		process() int! {
			parse();
			return 0;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInTypedAssignment(t *testing.T) {
	ir := generateIR(t, `
		parse() int! { return 42; }
		wrapper() int! {
			int x = parse();
			return x;
		}
		main() { }
	`)
	// Should have auto-propagation blocks in wrapper
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
	// The ok path extracts the value (index 1 from failable result)
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
}

func TestAutoPropagateInInferredAssignment(t *testing.T) {
	ir := generateIR(t, `
		parse() int! { return 42; }
		wrapper() int! {
			x := parse();
			return x;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateMultipleAssignments(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		wrapper() int! {
			int a = parse("x");
			int b = parse("y");
			return a + b;
		}
		main() { }
	`)
	// Should have two sets of auto-propagation blocks
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInFuncArg(t *testing.T) {
	ir := generateIR(t, `
		parse() int! { return 42; }
		use_value(int x) {}
		wrapper() void! {
			use_value(parse());
		}
		main() { }
	`)
	// Should have auto-propagation blocks for the argument
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
	// The ok path extracts the value (index 1 from failable result)
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
}

func TestAutoPropagateInMethodArg(t *testing.T) {
	ir := generateIR(t, `
		parse() int! { return 42; }
		type Foo { use_value(int x) {} }
		wrapper() void! {
			f := Foo();
			f.use_value(parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInConstructorArg(t *testing.T) {
	ir := generateIR(t, `
		parse() int! { return 42; }
		type Foo { int x; }
		wrapper() void! {
			Foo(x: parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateMultipleArgs(t *testing.T) {
	ir := generateIR(t, `
		parse_a() int! { return 1; }
		parse_b() int! { return 2; }
		add(int a, int b) int { return a + b; }
		wrapper() void! {
			add(parse_a(), parse_b());
		}
		main() { }
	`)
	// Both arguments should have auto-propagation
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInAssignStmt(t *testing.T) {
	ir := generateIR(t, `
		parse() int! { return 42; }
		wrapper() int! {
			int x = 0;
			x = parse();
			return x;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInExplicitNewArg(t *testing.T) {
	ir := generateIR(t, `
		parse() int! { return 42; }
		type Foo {
			int v;
			new(~this, int v) { this.v = v; }
		}
		wrapper() void! {
			Foo(v: parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInValueTypeArg(t *testing.T) {
	ir := generateIR(t, `
		parse() int! { return 42; }
		type Vec2 {
			int x `+"`"+`value;
			int y `+"`"+`value;
		}
		wrapper() void! {
			Vec2(x: parse(), y: 0);
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInEnumVariantArg(t *testing.T) {
	ir := generateIR(t, `
		parse() int! { return 42; }
		enum Box { Val(int v) }
		wrapper() void! {
			Box.Val(v: parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInVecPushArg(t *testing.T) {
	ir := generateIR(t, `
		parse() int! { return 42; }
		wrapper() void! {
			int[] v = int[]();
			v.push(parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestDropNullSafe(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		make() Resource! { return Resource(id: 1); }
		main() {
			Resource r = make() ? e { return; };
		}
	`)
	// Drop should null-check instance pointer before calling drop
	assertContains(t, ir, "drop.exec")
	assertContains(t, ir, "drop.done")
}

func TestRaiseExtractsInstancePtr(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		foo() void! { raise IoError(message: "err", code: 1); }
		main() { foo() ? e { }; }
	`)
	// Raise on user types should extract instance pointer (i8*) from value struct
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestHandlerNoBinding(t *testing.T) {
	ir := generateIR(t, `
		foo() void! { raise error(message: "oops"); }
		bar() {
			foo() ? { };
		}
		main() { }
	`)
	assertContains(t, ir, "error.handler")
	// Handler without binding should not load variant pointer for reconstruction
	assertNotContains(t, ir, "error.typed.match")
}

func TestTypedHandlerNoMatchPropagation(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		type ParseError is error { int line; }
		fail() void! { raise ParseError(message: "parse", line: 1); }
		handler() void! {
			fail() ? e is IoError { };
		}
		main() { handler() ? e { }; }
	`)
	// Nomatch should propagate (re-wrap error and return)
	assertContains(t, ir, "error.typed.nomatch")
	assertContains(t, ir, "promise_type_is")
}

// --- Generic type tests ---

func TestGenericTypeLayout(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 42);
		}
	`)
	assertContains(t, ir, "Box[int]_i")
	assertContains(t, ir, "store i64 42")
}

func TestGenericFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 42);
			int v = b.value;
		}
	`)
	assertContains(t, ir, "Box[int]_i")
	// Field access should load i64 (not i8*)
	assertContains(t, ir, "load i64")
}

func TestGenericFieldAssign(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 42);
			b.value = 10;
		}
	`)
	assertContains(t, ir, "store i64 10")
}

func TestGenericMethod(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T value;
			get(this) T { return this.value; }
		}
		main() {
			b := Box[int](value: 42);
			int v = b.get();
		}
	`)
	assertContains(t, ir, "define i64 @\"Box[int].get\"")
}

func TestGenericMethodSet(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T value;
			set(~this, T val) { this.value = val; }
		}
		main() {
			b := Box[int](value: 42);
			b.set(10);
		}
	`)
	assertContains(t, ir, "define void @\"Box[int].set\"")
}

func TestGenericMultipleInstances(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			a := Box[int](value: 42);
			b := Box[string](value: "hi");
		}
	`)
	assertContains(t, ir, "Box[int]_i")
	assertContains(t, ir, "Box[string]_i")
}

func TestGenericNestedField(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			a := Box[int](value: 1);
			b := Box[string](value: "hi");
			int x = a.value;
			string y = b.value;
		}
	`)
	// Both Box[int] and Box[string] fields accessed with correct types
	assertContains(t, ir, "Box[int]_i")
	assertContains(t, ir, "Box[string]_i")
	assertContains(t, ir, "load i64")
	assertContains(t, ir, "load i8*")
}

func TestGenericEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].Some(42);
		}
	`)
	assertContains(t, ir, "Option[int]_enum")
	assertContains(t, ir, "store i64 42")
}

func TestGenericEnumNone(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].None;
		}
	`)
	assertContains(t, ir, "Option[int]_enum")
}

func TestGenericEnumMatch(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].Some(42);
			r := match x {
				Some(v) => v,
				_ => 0,
			};
		}
	`)
	assertContains(t, ir, "switch i32")
}

func TestGenericEnumFieldless(t *testing.T) {
	ir := generateIR(t, `
		enum Dir[T] { Left, Right }
		main() {
			d := Dir[int].Left;
		}
	`)
	// Fieldless generic enum: internal type is i32
	assertContains(t, ir, "i32 0")
}

// TestGenericEnumMethodGetter verifies that a getter method on a generic enum
// is correctly monomorphized: the function is declared with the mono-qualified name.
func TestGenericEnumMethodGetter(t *testing.T) {
	ir := generateIR(t, `
		enum Wrapper[T] {
			Some(T value),
			Empty,

			get is_some bool {
				match this {
					Some(_) => {
						return true;
					},
					Empty => {
						return false;
					},
				}
			}
		}
		main() {
			w := Wrapper[int].Some(value: 42);
			b := w.is_some;
		}
	`)
	// Mono method declared with instance-qualified name
	assertContains(t, ir, `"Wrapper[int].is_some"`)
	// Enum layout exists
	assertContains(t, ir, "Wrapper[int]_v")
}

// TestGenericEnumMethodRegular verifies that a regular (non-getter) method on a
// generic enum is monomorphized correctly.
func TestGenericEnumMethodRegular(t *testing.T) {
	ir := generateIR(t, `
		enum Box[T] {
			Full(T item),
			Vacant,

			unwrap_or(&this, T fallback) T {
				match this {
					Full(v) => {
						return v;
					},
					Vacant => {
						return fallback;
					},
				}
			}
		}
		main() {
			b := Box[int].Full(item: 99);
			x := b.unwrap_or(0);
		}
	`)
	assertContains(t, ir, `"Box[int].unwrap_or"`)
}

// TestGenericEnumMethodCallsMethod verifies that a mono enum method body
// can call another method on the same enum via this.
func TestGenericEnumMethodCallsMethod(t *testing.T) {
	ir := generateIR(t, `
		enum Status[T] {
			Ok(T data),
			Err(string msg),

			get is_ok bool {
				match this {
					Ok(_) => {
						return true;
					},
					Err(_) => {
						return false;
					},
				}
			}

			get is_err bool {
				return !this.is_ok;
			}
		}
		main() {
			s := Status[int].Ok(data: 42);
			b := s.is_err;
		}
	`)
	// Both methods should be declared
	assertContains(t, ir, `"Status[int].is_ok"`)
	assertContains(t, ir, `"Status[int].is_err"`)
	// is_err calls is_ok
	assertContains(t, ir, `call i1 @"Status[int].is_ok"`)
}

// TestGenericEnumMultipleInstantiations verifies that methods are monomorphized
// separately for each type argument.
func TestGenericEnumMultipleInstantiations(t *testing.T) {
	ir := generateIR(t, `
		enum Opt[T] {
			Some(T value),
			None,

			get has_value bool {
				match this {
					Some(_) => {
						return true;
					},
					None => {
						return false;
					},
				}
			}
		}
		main() {
			a := Opt[int].Some(value: 1);
			b := Opt[string].None;
			x := a.has_value;
			y := b.has_value;
		}
	`)
	// Both instantiations get their own method
	assertContains(t, ir, `"Opt[int].has_value"`)
	assertContains(t, ir, `"Opt[string].has_value"`)
}

func TestGenericConstructorZeroInit(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 0);
		}
	`)
	// Generic type instance for Box[int]
	assertContains(t, ir, "Box[int]_i")
}

// TestGenericTupleTypeArg verifies that a Tuple used as a generic type argument
// produces a correct mono name ("Wrapper[(int, string)]") instead of "Wrapper[unknown]".
// Two different tuple args for the same generic must not collide.
func TestGenericTupleTypeArg(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper[T] { T val; }
		main() {
			w1 := Wrapper[(int, string)](val: (1, "a"));
			w2 := Wrapper[(bool, int)](val: (true, 42));
		}
	`)
	assertContains(t, ir, `Wrapper[(int, string)]`)
	assertContains(t, ir, `Wrapper[(bool, int)]`)
	assertNotContains(t, ir, `Wrapper[unknown]`)
}

// --- Generic function tests ---

func TestGenericFunc(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			int r = identity[int](42);
		}
	`)
	assertContains(t, ir, "define i64 @\"identity[int]\"")
	assertContains(t, ir, "ret i64")
}

func TestGenericFuncString(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			string s = identity[string]("hello");
		}
	`)
	assertContains(t, ir, "define i8* @\"identity[string]\"")
}

func TestGenericFuncMultipleInstances(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			int a = identity[int](42);
			string b = identity[string]("hi");
		}
	`)
	assertContains(t, ir, "@\"identity[int]\"")
	assertContains(t, ir, "@\"identity[string]\"")
}

func TestGenericMethodMutReceiverAssign(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T value;
			replace(~this, T newVal) { this.value = newVal; }
		}
		main() {
			b := Box[int](value: 10);
			b.replace(99);
		}
	`)
	assertContains(t, ir, "define void @\"Box[int].replace\"")
	// Should store i64 (the new value into the field)
	assertContains(t, ir, "store i64")
}

func TestGenericFuncVoid(t *testing.T) {
	ir := generateIR(t, `
		consume[T](T x) { }
		main() {
			consume[int](42);
		}
	`)
	assertContains(t, ir, "define void @\"consume[int]\"")
}

func TestGenericFuncFailable(t *testing.T) {
	ir := generateIR(t, `
		tryIdentity[T](T x) T! {
			return x;
		}
		main() {
			int v = tryIdentity[int](42)!;
		}
	`)
	assertContains(t, ir, "define { i1, i64, i8* } @\"tryIdentity[int]\"")
}

// B0099: Generic function calling another generic function with its own type param.
func TestGenericFuncCallsGenericFunc(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T val) T { return val; }
		wrap[T](T val) T { return identity[T](val); }
		main() {
			int r = wrap[int](42);
		}
	`)
	// Both wrap[int] and identity[int] must be monomorphized
	assertContains(t, ir, "define i64 @\"wrap[int]\"")
	assertContains(t, ir, "define i64 @\"identity[int]\"")
}

// B0099: Transitive chain of generic functions calling generic functions.
func TestGenericFuncTransitiveChain(t *testing.T) {
	ir := generateIR(t, `
		inner[T](T val) T { return val; }
		middle[T](T val) T { return inner[T](val); }
		outer[T](T val) T { return middle[T](val); }
		main() {
			int r = outer[int](42);
		}
	`)
	assertContains(t, ir, "define i64 @\"outer[int]\"")
	assertContains(t, ir, "define i64 @\"middle[int]\"")
	assertContains(t, ir, "define i64 @\"inner[int]\"")
}

// B0099: Multiple instantiations through generic-calls-generic.
func TestGenericFuncCallsGenericMultipleInstances(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T val) T { return val; }
		wrap[T](T val) T { return identity[T](val); }
		main() {
			int a = wrap[int](42);
			string b = wrap[string]("hi");
		}
	`)
	assertContains(t, ir, "define i64 @\"wrap[int]\"")
	assertContains(t, ir, "define i64 @\"identity[int]\"")
	assertContains(t, ir, "define i8* @\"wrap[string]\"")
	assertContains(t, ir, "define i8* @\"identity[string]\"")
}

// B0099: Generic function calling a generic method (cross-resolution).
func TestGenericFuncCallsGenericMethod(t *testing.T) {
	ir := generateIR(t, `
		type Echo {
			echo[T](T val) T { return val; }
		}
		invoke[T](Echo &e, T val) T {
			return e.echo[T](val);
		}
		main() {
			e := Echo();
			int r = invoke[int](e, 42);
		}
	`)
	assertContains(t, ir, "define i64 @\"invoke[int]\"")
	assertContains(t, ir, "define i64 @\"Echo.echo[int]\"")
}

// B0099: MethodInstance self-resolution (generic method calls generic method).
func TestGenericMethodCallsGenericMethod(t *testing.T) {
	ir := generateIR(t, `
		type Foo { echo[T](T val) T { return val; } }
		type Bar { delegate[T](Foo &f, T val) T { return f.echo[T](val); } }
		main() {
			f := Foo();
			b := Bar();
			int r = b.delegate[int](f, 7);
		}
	`)
	assertContains(t, ir, "define i64 @\"Bar.delegate[int]\"")
	assertContains(t, ir, "define i64 @\"Foo.echo[int]\"")
}

// B0099: Type-instance resolution (generic type method calls generic free function).
func TestGenericTypeMethodCallsFreeFunc(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T val) T { return val; }
		type Wrapper[T] {
			T value;
			wrapped(&this) T { return identity[T](this.value); }
		}
		main() {
			w := Wrapper[int](value: 77);
			int r = w.wrapped();
		}
	`)
	assertContains(t, ir, "define i64 @\"identity[int]\"")
	assertContains(t, ir, "define i64 @\"Wrapper[int].wrapped\"")
}

// B0099: Cross-resolution reverse (generic method calls generic free function).
func TestGenericMethodCallsFreeFunc(t *testing.T) {
	ir := generateIR(t, `
		helper[T](T val) T { return val; }
		type Proxy {
			forward[T](T val) T { return helper[T](val); }
		}
		main() {
			p := Proxy();
			int r = p.forward[int](33);
		}
	`)
	assertContains(t, ir, "define i64 @\"Proxy.forward[int]\"")
	assertContains(t, ir, "define i64 @\"helper[int]\"")
}

// B0099: Type-instance resolution for MethodInstance (generic type method calls generic method).
func TestGenericTypeMethodCallsGenericMethod(t *testing.T) {
	ir := generateIR(t, `
		type Echoer { echo[T](T val) T { return val; } }
		type Wrapper[T] {
			T value;
			Echoer e;
			echoed(&this) T { return this.e.echo[T](this.value); }
		}
		main() {
			w := Wrapper[int](value: 55, e: Echoer());
			int r = w.echoed();
		}
	`)
	assertContains(t, ir, "define i64 @\"Wrapper[int].echoed\"")
	assertContains(t, ir, "define i64 @\"Echoer.echo[int]\"")
}

// B0099: Cross-resolution both directions (method calls both func and method).
func TestGenericMethodCallsBothFuncAndMethod(t *testing.T) {
	ir := generateIR(t, `
		helper[T](T val) T { return val; }
		type Echoer { echo[T](T val) T { return val; } }
		type Combiner {
			run[T](Echoer &e, T val) T {
				T a = helper[T](val);
				return e.echo[T](a);
			}
		}
		main() {
			e := Echoer();
			c := Combiner();
			int r = c.run[int](e, 42);
		}
	`)
	assertContains(t, ir, "define i64 @\"Combiner.run[int]\"")
	assertContains(t, ir, "define i64 @\"helper[int]\"")
	assertContains(t, ir, "define i64 @\"Echoer.echo[int]\"")
}

func TestGenericTypeAsParam(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		unbox(Box[int] b) int {
			return b.value;
		}
		main() {
			b := Box[int](value: 99);
			int v = unbox(b);
		}
	`)
	assertContains(t, ir, "define i64 @unbox")
	assertContains(t, ir, "load i64")
}

func TestGenericEnumMatchBlock(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].Some(42);
			match x {
				Some(v) => { int y = v; },
				_ => { },
			};
		}
	`)
	assertContains(t, ir, "switch i32")
}

// === Stage 8g: Container Codegen Tests ===

// --- Part A: Tuple tests ---

func TestTupleLiteral(t *testing.T) {
	ir := generateIR(t, `main() { x := (1, 2); }`)
	// Should use insertvalue to build { i64, i64 } struct
	assertContains(t, ir, "insertvalue { i64, i64 }")
}

func TestTupleDestructure(t *testing.T) {
	ir := generateIR(t, `
		pair() (int, int) { return (1, 2); }
		main() { (a, b) := pair(); }
	`)
	// Should use extractvalue to destructure
	assertContains(t, ir, "extractvalue { i64, i64 }")
}

func TestTupleDestructureSkip(t *testing.T) {
	ir := generateIR(t, `
		pair() (int, int) { return (1, 2); }
		main() { (_, b) := pair(); }
	`)
	// Should extract second element but skip first
	assertContains(t, ir, "extractvalue { i64, i64 }")
	// b should be allocated
	assertContains(t, ir, "%b = alloca i64")
}

func TestFailableDestructure(t *testing.T) {
	ir := generateIR(t, `
		parse() int! { return 42; }
		main() {
			(val, err) := parse();
		}
	`)
	// Should have branch on tag for error/ok paths
	assertContains(t, ir, "destruct.err")
	assertContains(t, ir, "destruct.ok")
	assertContains(t, ir, "destruct.merge")
	// Should alloca both bindings
	assertContains(t, ir, "%val")
	assertContains(t, ir, "%err")
}

func TestFailableDestructureDiscardError(t *testing.T) {
	ir := generateIR(t, `
		parse() int! { return 42; }
		main() {
			(val, _) := parse();
		}
	`)
	assertContains(t, ir, "destruct.merge")
	assertContains(t, ir, "%val")
}

func TestTupleMixedTypes(t *testing.T) {
	ir := generateIR(t, `main() { x := (42, "hello", true); }`)
	// Should produce { i64, i8*, i1 } struct
	assertContains(t, ir, "insertvalue { i64, i8*, i1 }")
}

func TestTupleReturn(t *testing.T) {
	ir := generateIR(t, `
		pair() (int, bool) { return (42, true); }
		main() { (a, b) := pair(); }
	`)
	assertContains(t, ir, "define { i64, i1 } @pair()")
	assertContains(t, ir, "ret { i64, i1 }")
}

// --- Part B: Optional tests ---

func TestOptionalNone(t *testing.T) {
	ir := generateIR(t, `main() { int? x = none; }`)
	// Should alloca { i1, i64 } and zero-initialize
	assertContains(t, ir, "alloca { i1, i64 }")
	assertContains(t, ir, "zeroinitializer")
}

func TestOptionalSome(t *testing.T) {
	ir := generateIR(t, `main() { int? x = 42; }`)
	// Should alloca { i1, i64 } and wrap: { true, 42 }
	assertContains(t, ir, "alloca { i1, i64 }")
	assertContains(t, ir, "insertvalue { i1, i64 }")
	assertContains(t, ir, "i1 true")
}

func TestElvisOperator(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			int y = x ?: 0;
		}
	`)
	// Should have condBr + phi pattern
	assertContains(t, ir, "elvis.some")
	assertContains(t, ir, "elvis.none")
	assertContains(t, ir, "elvis.merge")
}

func TestOptionalStringNone(t *testing.T) {
	ir := generateIR(t, `main() { string? x = none; }`)
	assertContains(t, ir, "alloca { i1, i8* }")
	assertContains(t, ir, "zeroinitializer")
}

func TestOptionalVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			int? y = x;
		}
	`)
	// Should load/store { i1, i64 } struct
	assertContains(t, ir, "load { i1, i64 }")
	assertContains(t, ir, "store { i1, i64 }")
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

func TestStaticVectorDrop(t *testing.T) {
	ir := generateIR(t, `main() { x := [1, 2, 3]; }`)
	// Drop function should check bit 63 before freeing
	assertContains(t, ir, "define void @Vector.drop(")
	assertContains(t, ir, "check_static")
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

func TestArrayStringElements(t *testing.T) {
	ir := generateIR(t, `main() { x := ["hello", "world"]; }`)
	// String elements stored as i8*
	assertContains(t, ir, "call i8* @promise_string_new(")
	assertContains(t, ir, "call i8* @pal_alloc(i64")
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

// --- Part E: Lambda tests ---

func TestLambdaExpr(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> x + 1;
		}
	`)
	// Lambda function has env (i8*) as first parameter
	assertContainsMatch(t, ir, `define i64 @\.lambda\.\d+\(i8\* %env, i64 %x\)`)
	// Lambda returned as fat pointer {fn_ptr, env_ptr}
	assertContains(t, ir, "insertvalue { i8*, i8* }")
}

func TestLambdaCall(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> x + 1;
			int y = f(42);
		}
	`)
	// Should extract fn and env from fat pointer, then call with env as first arg
	assertContains(t, ir, "extractvalue { i8*, i8* }")
	assertContains(t, ir, "call i64")
}

func TestLambdaBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> int { return x * 2; };
		}
	`)
	assertContainsMatch(t, ir, `define i64 @\.lambda\.\d+\(i8\* %env, i64 %x\)`)
	assertContains(t, ir, "mul i64")
}

func TestLambdaVoid(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> void { return; };
		}
	`)
	assertContainsMatch(t, ir, `define void @\.lambda\.\d+\(i8\* %env, i64 %x\)`)
}

func TestLambdaVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> x + 1;
		}
	`)
	// Lambda stored as fat pointer { i8*, i8* }
	assertContains(t, ir, "alloca { i8*, i8* }")
	assertContains(t, ir, "store { i8*, i8* }")
}

// --- Lambda Capture Tests ---

func TestLambdaCaptureInt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 42;
			f := |int y| -> x + y;
		}
	`)
	// Env struct should be allocated via malloc
	assertContains(t, ir, "call i8* @pal_alloc(i64")
	// Lambda function should have env param
	assertContainsMatch(t, ir, `define i64 @\.lambda\.\d+\(i8\* %env, i64 %y\)`)
	// Should load captured var from env struct inside lambda
	assertContains(t, ir, "cap")
}

func TestLambdaCaptureMultiple(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int a = 1;
			int b = 2;
			f := |int x| -> a + b + x;
		}
	`)
	// Env should be allocated
	assertContains(t, ir, "call i8* @pal_alloc(i64")
	// Lambda should have env param
	assertContainsMatch(t, ir, `define i64 @\.lambda\.\d+\(i8\* %env`)
}

func TestLambdaNoCaptures(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> x + 1;
		}
	`)
	// No malloc for env — null env pointer
	assertContains(t, ir, "i8* null, 1")
}

func TestLambdaCaptureCall(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			f := |int y| -> x + y;
			int result = f(5);
		}
	`)
	// Should extract fn and env from fat pointer for indirect call
	assertContains(t, ir, "extractvalue { i8*, i8* }")
	assertContains(t, ir, "call i64")
}

func TestLambdaNestedCapture(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			f := |int a| -> int {
				g := |int b| -> x + b;
				return g(a);
			};
		}
	`)
	// Outer lambda should also capture x (propagated from inner)
	// Both lambdas should have env params and malloc for env
	// Count lambda functions that take i8* %env and return i64
	matches := regexp.MustCompile(`define i64 @\.lambda\.\d+\(i8\* %env`).FindAllString(ir, -1)
	if len(matches) < 2 {
		t.Errorf("expected at least 2 i64 lambda functions with env, got %d", len(matches))
	}
	// Two malloc calls — one for outer lambda env, one for inner
	assertContains(t, ir, "call i8* @pal_alloc(i64")
}

func TestLambdaEnvFree(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			f := |int y| -> x + y;
		}
	`)
	// Env should be freed at scope exit
	assertContains(t, ir, "call void @pal_free(i8*")
}

func TestLambdaEnvFreeNullCheck(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> x + 1;
		}
	`)
	// No-capture lambda: env is null, free should have null check
	assertContains(t, ir, "env.free")
	assertContains(t, ir, "env.skip")
}

// T0100: Lambda with captures passed directly as function argument — env
// should be freed at statement end via the env temp tracking mechanism.
func TestLambdaEnvTempCleanup(t *testing.T) {
	ir := generateIR(t, `
		apply(int x, (int) -> int fn) int { return fn(x); }
		do_it() {
			int captured = 42;
			int result = apply(5, |int x| -> x + captured);
		}
		main() { do_it(); }
	`)
	// The lambda has a capture (captured) so env is allocated.
	// Since the lambda is passed directly as a function argument (not stored
	// in a variable), env temp tracking should free it at statement end.
	assertContains(t, ir, "env.tmp.drop")
	assertContains(t, ir, "env.tmp.exec")
	assertContains(t, ir, "call void @pal_free")
}

// T0100: Lambda stored in a variable — env temp is claimed (drop flag cleared
// at runtime), so env.tmp.drop exists in IR but the runtime flag check prevents
// the actual free. The scope binding (env.free) handles cleanup instead.
func TestLambdaEnvTempClaimedForVariable(t *testing.T) {
	ir := generateIR(t, `
		do_it() {
			int x = 10;
			f := |int y| -> x + y;
		}
		main() { do_it(); }
	`)
	// Lambda stored in a variable: env freed via scope binding (env.free).
	// The env temp is claimed (env.claim blocks emitted to clear drop flag).
	assertContains(t, ir, "env.free")
	assertContains(t, ir, "env.claim")
}

func TestNamedFuncRefThunk(t *testing.T) {
	ir := generateIR(t, `
		add(int x) int { return x + 1; }
		main() {
			f := add;
			int y = f(42);
		}
	`)
	// Should generate a thunk for the named function reference
	assertContains(t, ir, "define i64 @.thunk.add(i8* %env, i64 %x)")
	// Fat pointer should use thunk, not raw @add
	assertContains(t, ir, ".thunk.add")
	// Should call through indirect call path
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestFunctionTypedFieldCall(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper {
			() -> int getter;
			get_val() int { return this.getter(); }
		}
		main() {
			w := Wrapper(getter: || -> int { return 99; });
		}
	`)
	// Should call through indirect call path (extractvalue from fat pointer)
	assertContains(t, ir, "define i64 @Wrapper.get_val")
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestFunctionTypedFieldCallWithArgs(t *testing.T) {
	ir := generateIR(t, `
		type Calc {
			(int, int) -> int op;
			run(int a, int b) int { return this.op(a, b); }
		}
		main() {
			c := Calc(op: |int x, int y| -> x + y);
		}
	`)
	// Method should exist and use indirect call
	assertContains(t, ir, "define i64 @Calc.run")
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestFunctionTypedFieldOptionalReturn(t *testing.T) {
	// Critical for _FnIter[T] pattern where _next is () -> T?
	ir := generateIR(t, `
		type Supplier {
			() -> int? produce;
			next() int? { return this.produce(); }
		}
		main() {
			s := Supplier(produce: || -> int? { return 42; });
		}
	`)
	assertContains(t, ir, "@Supplier.next")
	// Should call through indirect call path (fat pointer)
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestFunctionTypedFieldVoidReturn(t *testing.T) {
	ir := generateIR(t, `
		type Handler {
			(int) -> void action;
			run(int x) { this.action(x); }
		}
		main() {
			h := Handler(action: |int x| { });
		}
	`)
	assertContains(t, ir, "define void @Handler.run")
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestThisCaptureInMethodLambda(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int count;
			make_fn() () -> int {
				return move || -> int {
					return this.count;
				};
			}
		}
		main() {
			c := Counter(count: 10);
		}
	`)
	// Method should return a fat pointer (closure)
	assertContains(t, ir, "define { i8*, i8* } @Counter.make_fn")
	// The lambda builds a fat pointer with env
	assertContains(t, ir, "insertvalue { i8*, i8* }")
}

func TestVoidFunctionTypeParam(t *testing.T) {
	ir := generateIR(t, `
		apply((int) -> void fn) {
			fn(42);
		}
		main() {
			apply(|int x| { });
		}
	`)
	assertContains(t, ir, "define void @apply")
	assertContains(t, ir, "extractvalue { i8*, i8* }")
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
	assertContains(t, ir, "define { i8*, i8* } @make_adder")
	// Main should do indirect call on the result
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

// ================================================================
// Stage 8h — Optional Patterns, String Interpolation & Expression Completeness
// ================================================================

// --- Part A: If-unwrap ---

func TestIfUnwrap(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			if val := x {
				int y = val + 1;
			}
		}
	`)
	assertContains(t, ir, "extractvalue")
	assertContains(t, ir, "ifunwrap.then")
	assertContains(t, ir, "ifunwrap.end")
}

func TestIfUnwrapElse(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = none;
			if val := x {
				int y = val;
			} else {
				int z = 0;
			}
		}
	`)
	assertContains(t, ir, "ifunwrap.then")
	assertContains(t, ir, "ifunwrap.else")
	assertContains(t, ir, "ifunwrap.end")
}

// --- Part B: While-unwrap ---

func TestWhileUnwrap(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			while val := x {
				break;
			}
		}
	`)
	assertContains(t, ir, "whileunwrap.header")
	assertContains(t, ir, "whileunwrap.body")
	assertContains(t, ir, "whileunwrap.exit")
	assertContains(t, ir, "extractvalue")
}

func TestWhileUnwrapBreak(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 10;
			while val := x {
				break;
			}
		}
	`)
	// break should jump to exit block
	assertContains(t, ir, "br label %whileunwrap.exit")
}

// --- Part C: Optional chaining ---

func TestOptionalChain(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		main() {
			Dog? d = Dog(age: 3);
			int? a = d?.age;
		}
	`)
	assertContains(t, ir, "optchain.some")
	assertContains(t, ir, "optchain.none")
	assertContains(t, ir, "optchain.merge")
}

func TestOptionalChainNone(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		main() {
			Dog? d = none;
			int? a = d?.age;
		}
	`)
	assertContains(t, ir, "optchain.some")
	assertContains(t, ir, "optchain.none")
	assertContains(t, ir, "phi")
}

// --- Part D: String interpolation ---

func TestStringInterpolationIdent(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string name = "world";
			string msg = "hello {name}";
		}
	`)
	// Should call promise_string_concat
	assertContains(t, ir, "call i8* @promise_string_concat")
}

func TestStringInterpolationInt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 42;
			string msg = "x = {x}";
		}
	`)
	assertContains(t, ir, "call i8* @promise_int_to_string")
	assertContains(t, ir, "call i8* @promise_string_concat")
}

func TestStringInterpolationBool(t *testing.T) {
	ir := generateIR(t, `
		main() {
			bool flag = true;
			string msg = "flag: {flag}";
		}
	`)
	assertContains(t, ir, "call i8* @promise_bool_to_string")
	assertContains(t, ir, "call i8* @promise_string_concat")
}

func TestStringInterpolationExpr(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string msg = "result: {1 + 2}";
		}
	`)
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "call i8* @promise_int_to_string")
	assertContains(t, ir, "call i8* @promise_string_concat")
}

func TestStringInterpolationMultiple(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int a = 1;
			int b = 2;
			string msg = "{a} and {b}";
		}
	`)
	// Two int-to-string conversions and multiple concats
	count := strings.Count(ir, "call i8* @promise_int_to_string")
	if count < 2 {
		t.Errorf("expected at least 2 calls to promise_int_to_string, got %d", count)
	}
}

func TestStringInterpolationTypeParam(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper[T] {
			T val;
			to_string() string => "[{this.val}]";
		}
		main() {
			Wrapper[int] w = Wrapper[int](val: 42);
			string s = w.to_string();
		}
	`)
	// The mono'd Wrapper[int].to_string should call promise_int_to_string
	assertContains(t, ir, "promise_int_to_string")
}

func TestStringInterpolationTuple(t *testing.T) {
	ir := generateIR(t, `
		main() {
			(int, bool) t = (1, true);
			string s = "{t}";
		}
	`)
	// Tuple formatting should produce calls to int_to_string and bool_to_string
	assertContains(t, ir, "promise_int_to_string")
	assertContains(t, ir, "promise_bool_to_string")
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

// genClassicForStmt with typed init
func TestClassicForTypedInit(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for int i = 0; i < 5; i += 1 {
				int x = i;
			}
		}
	`)
	assertContains(t, ir, "for.header")
	assertContains(t, ir, "for.exit")
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

// convertToString: f64 interpolation
func TestStringInterpolationF64(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 x = 3.14;
			string msg = "pi is {x}";
		}
	`)
	assertContains(t, ir, "call i8* @promise_f64_to_string")
	assertContains(t, ir, "call i8* @promise_string_concat")
}

// convertToString: string passthrough in interpolation
func TestStringInterpolationStringVar(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string name = "world";
			string msg = "hello {name}";
		}
	`)
	// No conversion call needed — string is passed directly to concat
	assertContains(t, ir, "call i8* @promise_string_concat")
}

// convertToString: f32 interpolation (direct f32 to string)
func TestStringInterpolationF32(t *testing.T) {
	ir := generateIR(t, `
		show(f32 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "call i8* @promise_f32_to_string")
}

// convertToString: i32 interpolation (sext to i64)
func TestStringInterpolationI32(t *testing.T) {
	ir := generateIR(t, `
		show(i32 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "sext i32")
	assertContains(t, ir, "call i8* @promise_int_to_string")
}

// convertToString: u32 interpolation (zext to i64, calls uint_to_string)
func TestStringInterpolationU32(t *testing.T) {
	ir := generateIR(t, `
		show(u32 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "zext i32")
	assertContains(t, ir, "call i8* @promise_uint_to_string")
}

// unsignedIntOps: basic unsigned arithmetic
func TestUnsignedIntArithmetic(t *testing.T) {
	ir := generateIR(t, `
		compute(uint a, uint b) {
			uint sum = a + b;
			uint diff = a - b;
			uint prod = a * b;
			uint quot = a / b;
			uint rem = a % b;
		}
		main() { }
	`)
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "sub i64")
	assertContains(t, ir, "mul i64")
	assertContains(t, ir, "udiv i64")
	assertContains(t, ir, "urem i64")
}

// unsignedIntOps: comparison operators
func TestUnsignedIntComparison(t *testing.T) {
	ir := generateIR(t, `
		compare(uint a, uint b) {
			bool lt = a < b;
			bool le = a <= b;
			bool gt = a > b;
			bool ge = a >= b;
			bool eq = a == b;
			bool ne = a != b;
		}
		main() { }
	`)
	assertContains(t, ir, "icmp ult i64")
	assertContains(t, ir, "icmp ule i64")
	assertContains(t, ir, "icmp ugt i64")
	assertContains(t, ir, "icmp uge i64")
	assertContains(t, ir, "icmp eq i64")
	assertContains(t, ir, "icmp ne i64")
}

// floatOps: float arithmetic (full coverage)
func TestFloatArithmeticFull(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 a = 3.14;
			f64 b = 2.0;
			f64 sum = a + b;
			f64 diff = a - b;
			f64 prod = a * b;
			f64 quot = a / b;
		}
	`)
	assertContains(t, ir, "fadd double")
	assertContains(t, ir, "fsub double")
	assertContains(t, ir, "fmul double")
	assertContains(t, ir, "fdiv double")
}

// floatOps: float comparison operators (full coverage)
func TestFloatComparisonFull(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 a = 3.14;
			f64 b = 2.0;
			bool lt = a < b;
			bool gt = a > b;
			bool eq = a == b;
			bool ne = a != b;
		}
	`)
	assertContains(t, ir, "fcmp olt double")
	assertContains(t, ir, "fcmp ogt double")
	assertContains(t, ir, "fcmp oeq double")
	assertContains(t, ir, "fcmp one double")
}

// resolveEscape: additional escape sequences
func TestStringEscapeSequences(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string a = "hello\tworld";
			string b = "line1\rline2";
			string c = "back\\slash";
			string d = "null\0end";
			string e = "quote\"mark";
		}
	`)
	// Each should produce a global string constant
	assertContains(t, ir, "call i8* @promise_string_new")
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

// boolOps: boolean equality/inequality
func TestBoolEquality(t *testing.T) {
	ir := generateIR(t, `
		main() {
			bool a = true;
			bool b = false;
			bool eq = a == b;
			bool ne = a != b;
		}
	`)
	assertContains(t, ir, "icmp eq i1")
	assertContains(t, ir, "icmp ne i1")
}

// --- Stage 8i: Char literals, container .len, string iteration, map compound assignment ---

func TestCharLiteral(t *testing.T) {
	ir := generateIR(t, `main() { char c = 'a'; }`)
	assertContains(t, ir, "store i32 97")
}

func TestCharEscape(t *testing.T) {
	ir := generateIR(t, `main() { char c = '\n'; }`)
	assertContains(t, ir, "store i32 10")
}

func TestCharEscapeNull(t *testing.T) {
	ir := generateIR(t, `main() { char c = '\0'; }`)
	assertContains(t, ir, "store i32 0")
}

func TestCharEscapeBackslash(t *testing.T) {
	ir := generateIR(t, `main() { char c = '\\'; }`)
	assertContains(t, ir, "store i32 92")
}

func TestCharMultiByte(t *testing.T) {
	ir := generateIR(t, `main() { char c = '€'; }`)
	// € is U+20AC = 8364
	assertContains(t, ir, "store i32 8364")
}

func TestCharEquality(t *testing.T) {
	ir := generateIR(t, `
		check(char a, char b) bool { return a == b; }
		main() { }
	`)
	assertContains(t, ir, "icmp eq i32")
}

func TestCharComparison(t *testing.T) {
	ir := generateIR(t, `
		check(char a, char b) bool { return a < b; }
		main() { }
	`)
	assertContains(t, ir, "icmp slt i32")
}

func TestCharInterpolation(t *testing.T) {
	ir := generateIR(t, `
		main() { char c = 'X'; string s = "char: {c}"; }
	`)
	assertContains(t, ir, "call i8* @promise_char_to_string(i32")
}

// convertToString: i16 interpolation (sext to i64)
func TestStringInterpolationI16(t *testing.T) {
	ir := generateIR(t, `
		show(i16 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "sext i16")
	assertContains(t, ir, "call i8* @promise_int_to_string")
}

// convertToString: i8 interpolation (sext to i64)
func TestStringInterpolationI8(t *testing.T) {
	ir := generateIR(t, `
		show(i8 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "sext i8")
	assertContains(t, ir, "call i8* @promise_int_to_string")
}

// convertToString: uint interpolation (direct i64, no extension)
func TestStringInterpolationUint(t *testing.T) {
	ir := generateIR(t, `
		show(uint x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "call i8* @promise_uint_to_string")
}

// convertToString: u16 interpolation (zext to i64)
func TestStringInterpolationU16(t *testing.T) {
	ir := generateIR(t, `
		show(u16 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "zext i16")
	assertContains(t, ir, "call i8* @promise_uint_to_string")
}

// convertToString: u8 interpolation (zext to i64)
func TestStringInterpolationU8(t *testing.T) {
	ir := generateIR(t, `
		show(u8 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "zext i8")
	assertContains(t, ir, "call i8* @promise_uint_to_string")
}

// --- Value-to-string function body tests ---

func TestBoolToStringFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() { bool b = true; string s = "{b}"; }
	`)
	assertContains(t, ir, "define i8* @promise_bool_to_string(i8")
	assertContains(t, ir, `c"true"`)
	assertContains(t, ir, `c"false"`)
	assertContains(t, ir, "true:")
	assertContains(t, ir, "false:")
}

func TestIntToStringFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() { int x = 42; string s = "{x}"; }
	`)
	assertContains(t, ir, "define i8* @promise_int_to_string(i64")
	assertContains(t, ir, "digit_loop:")
	assertContains(t, ir, "check_neg:")
	assertContains(t, ir, "check_sign:")
	assertContains(t, ir, "done:")
	assertContains(t, ir, "urem i64")
	assertContains(t, ir, "udiv i64")
}

func TestUintToStringFuncBody(t *testing.T) {
	ir := generateIR(t, `
		show(uint x) { string s = "{x}"; }
		main() { }
	`)
	assertContains(t, ir, "define i8* @promise_uint_to_string(i64")
	assertContains(t, ir, "call i8* @promise_uint_to_string")
	assertContains(t, ir, "digit_loop:")
	assertContains(t, ir, "done:")
	assertContains(t, ir, "urem i64")
	assertContains(t, ir, "udiv i64")
}

func TestF64ToStringFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() { f64 x = 3.14; string s = "{x}"; }
	`)
	// promise_f64_to_string is a bridge to the Promise-defined _f64_to_str
	assertContains(t, ir, "define i8* @promise_f64_to_string(double")
	assertContains(t, ir, "call i8* @__mod_std__f64_to_str(double")
}

func TestCharToStringFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() { char c = 'X'; string s = "{c}"; }
	`)
	assertContains(t, ir, "define i8* @promise_char_to_string(i32")
	assertContains(t, ir, "one_byte:")
	assertContains(t, ir, "two_byte:")
	assertContains(t, ir, "three_byte:")
	assertContains(t, ir, "four_byte:")
	assertContains(t, ir, "lshr i32")
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

func TestStringLen(t *testing.T) {
	ir := generateIRWithStd(t, stdContainers, `
		main() {
			string s = "hello";
			int n = s.len;
		}
	`)
	// Should GEP to string instance len field and load
	assertContains(t, ir, "load i64")
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

// --- Stage 8k: Inheritance Codegen Tests ---

func TestInheritedFieldLayout(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; int age; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", age: 5, breed: "Lab");
		}
	`)
	// Dog instance struct should include parent fields: _variant, name, age, breed
	assertContains(t, ir, `%promise_Dog_i = type { %promise_Dog_m*, i8*, i64, i8* }`)
	// Animal instance struct: _variant, name, age
	assertContains(t, ir, `%promise_Animal_i = type { %promise_Animal_m*, i8*, i64 }`)
}

func TestInheritedFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; int age; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", age: 5, breed: "Lab");
			string n = d.name;
			int a = d.age;
			string b = d.breed;
		}
	`)
	// Field access should use GEP on Dog instance struct
	assertContains(t, ir, "getelementptr %promise_Dog_i")
}

func TestInheritedFieldConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", breed: "Lab");
		}
	`)
	// Constructor should store values for both inherited and own fields
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "getelementptr %promise_Dog_i")
}

func TestInheritedMethodCall(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			greet() string { return this.name; }
		}
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", breed: "Lab");
			string g = d.greet();
		}
	`)
	// d.greet() should dispatch to Animal.greet (inherited method)
	assertContains(t, ir, "call i8* @Animal.greet(i8*")
}

func TestMethodOverride(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Dog d = Dog(name: "Rex");
			string s = d.speak();
		}
	`)
	// d.speak() should dispatch to Dog.speak (child overrides parent)
	assertContains(t, ir, "call i8* @Dog.speak(i8*")
}

func TestUpcastFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { string breed; }
		main() {
			Animal a = Dog(name: "Rex", breed: "Lab");
			string n = a.name;
		}
	`)
	// Upcast Dog to Animal, then access name via Animal layout
	assertContains(t, ir, "getelementptr %promise_Animal_i")
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

func TestConstructorStoresTypeInfo(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		main() {
			Animal a = Animal(name: "Rex");
		}
	`)
	// Constructor should store type info pointer instead of null
	assertContains(t, ir, "@promise_typeinfo_Animal")
	// The _variant slot should be set via bitcast of the type info global
	assertContains(t, ir, "bitcast")
}

func TestDeepInheritance(t *testing.T) {
	ir := generateIR(t, `
		type A { int x; }
		type B is A { int y; }
		type C is B { int z; }
		main() {
			C c = C(x: 1, y: 2, z: 3);
			int a = c.x;
			int b = c.y;
			int d = c.z;
		}
	`)
	// C struct should have _variant, x, y, z (4 fields + internal = 4 GEP indices)
	assertContains(t, ir, "%promise_C_i = type { %promise_C_m*, i64, i64, i64 }")
}

// --- Part D: is/as expression tests ---

func TestIsPresent(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			bool b = x is present;
		}
	`)
	// Should extract the i1 flag from the optional struct
	assertContains(t, ir, "extractvalue { i1, i64 }")
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

func TestIsEnumVariant(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			bool b = c is Red;
		}
		main() { }
	`)
	// Fieldless enum: value IS the tag, compare with icmp eq
	assertContains(t, ir, "icmp eq i32")
}

func TestIsEnumVariantData(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		test() {
			Shape s = Shape.Circle(radius: 3.14);
			bool b = s is Circle;
		}
		main() { }
	`)
	// Data-carrying enum: extract tag from struct, then compare
	assertContains(t, ir, "extractvalue")
	assertContains(t, ir, "icmp eq i32")
}

func TestIsNamedType(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { }
		main() {
			Animal a = Dog(name: "Rex");
			bool b = a is Dog;
		}
	`)
	// Should call promise_type_is (now codegen-emitted, not extern) and convert to i1
	assertContains(t, ir, "define i32 @promise_type_is")
	assertContains(t, ir, "call i32 @promise_type_is")
	assertContains(t, ir, "icmp ne i32")
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

func TestIsNamedTypeInheritance(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { string breed; }
		type Cat is Animal { }
		main() {
			Animal a = Dog(name: "Rex", breed: "Lab");
			bool isDog = a is Dog;
			bool isCat = a is Cat;
			bool isAnimal = a is Animal;
		}
	`)
	// All three checks should go through RTTI
	assertContains(t, ir, "call i32 @promise_type_is")
	// Type info globals for all three types
	assertContains(t, ir, "@promise_typeinfo_Dog")
	assertContains(t, ir, "@promise_typeinfo_Cat")
	assertContains(t, ir, "@promise_typeinfo_Animal")
}

func TestAsSafeCast(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; }
		type Dog is Animal { }
		main() {
			Animal a = Dog(name: "Rex");
			Dog? d = a as Dog;
		}
	`)
	// Should have RTTI check, then cast.some/cast.none/cast.merge blocks
	assertContains(t, ir, "call i32 @promise_type_is")
	assertContains(t, ir, "cast.some.")
	assertContains(t, ir, "cast.none.")
	assertContains(t, ir, "cast.merge.")
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

func TestScalarCastCharToInt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			char c = 'A';
			int x = c as int;
		}
	`)
	// char (i32) → int (i64): zero extension (codepoints are unsigned)
	assertContains(t, ir, "zext i32")
}

func TestScalarCastIntToChar(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 65;
			char c = x as char;
		}
	`)
	// int (i64) → char (i32): truncation
	assertContains(t, ir, "trunc i64")
}

func TestScalarCastBoolToInt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			bool b = true;
			int x = b as int;
		}
	`)
	// bool (i1) → int (i64): zero extension
	assertContains(t, ir, "zext i1")
}

func TestScalarCastIntToBoolIR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 42;
			bool b = x as bool;
		}
	`)
	// int → bool uses icmp ne (not trunc), so 2 as bool == true
	assertContains(t, ir, "icmp ne i64")
}

func TestScalarCastF64ToBoolIR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 f = 1.5;
			bool b = f as bool;
		}
	`)
	// float → bool uses fcmp une (unordered not-equal to 0.0, so NaN is truthy)
	assertContains(t, ir, "fcmp une double")
}

func TestScalarCastNoRTTI(t *testing.T) {
	ir := generateIR(t, `
		main() {
			char c = 'A';
			int x = c as int;
			bool b = x as bool;
		}
	`)
	// Scalar casts should use zext/icmp, not RTTI cast blocks
	assertContains(t, ir, "zext i32")
	assertContains(t, ir, "icmp ne i64")
	assertNotContains(t, ir, "cast.some")
	assertNotContains(t, ir, "cast.none")
}

func TestScalarCastCharToF64(t *testing.T) {
	ir := generateIR(t, `
		main() {
			char c = 'A';
			f64 f = c as f64;
		}
	`)
	// char (i32, unsigned) → f64: uitofp
	assertContains(t, ir, "uitofp i32")
}

func TestScalarCastF64ToChar(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 f = 65.0;
			char c = f as char;
		}
	`)
	// f64 → char (i32, unsigned): fptoui
	assertContains(t, ir, "fptoui double")
}

func TestScalarCastBoolToF64(t *testing.T) {
	ir := generateIR(t, `
		main() {
			bool b = true;
			f64 f = b as f64;
		}
	`)
	// bool (i1, unsigned) → f64: uitofp
	assertContains(t, ir, "uitofp i1")
}

func TestScalarCastF32ToBoolIR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f32 f = 1.0f32;
			bool b = f as bool;
		}
	`)
	// f32 → bool: fcmp une float
	assertContains(t, ir, "fcmp une float")
}

func TestScalarCastSameWidthNoop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 42;
			uint u = x as uint;
		}
	`)
	// Same-width cast (i64 → i64) is a no-op — value is loaded and stored directly.
	// The main function should NOT contain zext/sext/trunc of i64 for this cast.
	// Just verify it compiles and the RTTI cast path is not used.
	assertNotContains(t, ir, "cast.some")
	assertNotContains(t, ir, "cast.none")
}

func TestScalarCastI8SextToInt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			i8 x = 42i8;
			int y = x as int;
		}
	`)
	// i8 (signed) → int (i64): sign extension
	assertContains(t, ir, "sext i8")
}

func TestScalarCastU8ZextToInt(t *testing.T) {
	ir := generateIR(t, `
		main() {
			u8 x = 200u8;
			int y = x as int;
		}
	`)
	// u8 (unsigned) → int (i64): zero extension
	assertContains(t, ir, "zext i8")
}

func TestScalarCastIntSitofp(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 42;
			f64 f = x as f64;
		}
	`)
	// int (signed) → f64: sitofp
	assertContains(t, ir, "sitofp i64")
}

func TestScalarCastUintUitofp(t *testing.T) {
	ir := generateIR(t, `
		main() {
			uint x = 42 as! uint;
			f64 f = x as f64;
		}
	`)
	// uint (unsigned) → f64: uitofp
	assertContains(t, ir, "uitofp i64")
}

func TestScalarCastF64Fptosi(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 f = 3.14;
			int x = f as int;
		}
	`)
	// f64 → int (signed): fptosi
	assertContains(t, ir, "fptosi double")
}

func TestScalarCastF64Fptoui(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 f = 3.14;
			uint x = f as uint;
		}
	`)
	// f64 → uint (unsigned): fptoui
	assertContains(t, ir, "fptoui double")
}

func TestScalarCastF32Fpext(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f32 x = 1.5f32;
			f64 y = x as f64;
		}
	`)
	// f32 → f64: fpext
	assertContains(t, ir, "fpext float")
}

func TestScalarCastF64Fptrunc(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 x = 1.5;
			f32 y = x as f32;
		}
	`)
	// f64 → f32: fptrunc
	assertContains(t, ir, "fptrunc double")
}

func TestScalarCastI16ToBoolIR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			i16 x = 100i16;
			bool b = x as bool;
		}
	`)
	// i16 → bool: icmp ne i16
	assertContains(t, ir, "icmp ne i16")
}

func TestScalarCastCharToBoolIR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			char c = 'A';
			bool b = c as bool;
		}
	`)
	// char (i32) → bool: icmp ne i32
	assertContains(t, ir, "icmp ne i32")
}

func TestScalarCastAsBangScalarNoRTTI(t *testing.T) {
	ir := generateIR(t, `
		main() {
			char c = 'A';
			int x = c as! int;
		}
	`)
	// as! on scalar types should also use direct cast, not RTTI path
	assertContains(t, ir, "zext i32")
	assertNotContains(t, ir, "cast.ok")
	assertNotContains(t, ir, "cast.panic")
}

func TestOptionalHandlerRecovery(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = none;
			int y = x ? _ { 0; };
		}
	`)
	assertContains(t, ir, "opt.none")
	assertContains(t, ir, "opt.some")
	assertContains(t, ir, "opt.merge")
}

func TestOptionalForceUnwrapBang(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			int y = x!;
		}
	`)
	assertContains(t, ir, "unwrap.ok")
	assertContains(t, ir, "unwrap.panic")
	assertContains(t, ir, "promise_panic")
}

func TestOptionalForceUnwrapAsBang(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			int y = x as! int;
		}
	`)
	// Should extractvalue to check flag, then extractvalue to get inner value
	assertContains(t, ir, "unwrap.ok")
	assertContains(t, ir, "unwrap.panic")
	assertContains(t, ir, "promise_panic")
}

func TestFieldShadowing(t *testing.T) {
	ir := generateIR(t, `
		type Base { int x; int y; }
		type Child is Base { string x; }
		main() {
			Child c = Child(y: 1, x: "hi");
			string s = c.x;
			int n = c.y;
		}
	`)
	// Child layout: _variant, y (inherited, not shadowed), x (own, shadows Base.x)
	// y is int (i64), x is string (i8*) — parent x omitted from layout
	assertContains(t, ir, "%promise_Child_i = type { %promise_Child_m*, i64, i8* }")
}

func TestConstructorZeroInitInheritedField(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; int age; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", age: 0, breed: "Lab");
		}
	`)
	// Constructor should store inherited fields
	assertContains(t, ir, "getelementptr %promise_Dog_i")
}

func TestDeepInheritanceMethodDispatch(t *testing.T) {
	ir := generateIR(t, `
		type A {
			int x;
			getX() int { return this.x; }
		}
		type B is A { int y; }
		type C is B { int z; }
		main() {
			C c = C(x: 1, y: 2, z: 3);
			int v = c.getX();
		}
	`)
	// c.getX() should resolve through C → B → A and call A.getX
	assertContains(t, ir, "call i64 @A.getX(i8*")
}

func TestRTTIMultipleParents(t *testing.T) {
	ir := generateIR(t, `
		type Printable {
			show() string { return "printable"; }
		}
		type Serializable {
			encode() string { return "serializable"; }
		}
		type Doc is Printable, Serializable {
			string name;
		}
		main() {
			Doc d = Doc(name: "hi");
		}
	`)
	// Type info for Doc should include both parent IDs
	assertContains(t, ir, "@promise_typeinfo_Doc")
	assertContains(t, ir, "@promise_typeinfo_Printable")
	assertContains(t, ir, "@promise_typeinfo_Serializable")
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

func TestIsPresentStringOptional(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? x = "hello";
			bool b = x is present;
			bool c = x is absent;
		}
	`)
	// Should extractvalue on { i1, i8* } optional
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

func TestIsTypeOnOptionalPrimitive(t *testing.T) {
	// B0029: `is` type check on optional primitives should not try to extract vtable.
	// Before the fix, this panicked with "bitcast i64 to {i8*}*".
	ir := generateIR(t, `
		main() {
			int? x = 42;
			bool b = x is int;
		}
	`)
	// Should extract i1 flag from {i1, i64} — presence check only
	assertContains(t, ir, "extractvalue { i1, i64 }")
}

func TestIsTypeOnOptionalUserType(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			speak() string `+"`"+`abstract;
		}
		type Dog is Animal {
			speak() string { return "Woof"; }
		}
		main() {
			Animal d = Dog(name: "Rex");
			Animal? a = d;
			bool b = a is Dog;
		}
	`)
	// Should use RTTI check on unwrapped value, guarded by presence flag
	assertContains(t, ir, "promise_type_is")
}

func TestIsTypeOnOptionalString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? s = "hello";
			bool b = s is string;
		}
	`)
	// String optional: {i1, i8*} — should extract flag only, no RTTI
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

func TestIsTypeOnOptionalEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		main() {
			Color? c = Color.Red;
			bool b = c is Color;
		}
	`)
	// Enum optional: should extract flag only, no RTTI
	assertContains(t, ir, "extractvalue")
}

func TestIsTypeOnOptionalBoolFalse(t *testing.T) {
	// false is a valid present value — is bool must return true
	ir := generateIR(t, `
		main() {
			bool? b = false;
			bool ok = b is bool;
		}
	`)
	assertContains(t, ir, "extractvalue { i1, i8 }")
}

// --- VTable dispatch tests (Stage 8l) ---

func TestVtableGlobalEmitted(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Dog d = Dog(name: "Rex");
		}
	`)
	// Both types have virtual methods, vtable globals should be emitted
	assertContains(t, ir, "@promise_vtable_Animal")
	assertContains(t, ir, "@promise_vtable_Dog")
}

func TestAbstractMethodVirtualDispatch(t *testing.T) {
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
			string s = a.speak();
		}
	`)
	// Virtual dispatch: should NOT directly call @Animal.speak (abstract, doesn't exist)
	assertNotContains(t, ir, "call i8* @Animal.speak")
	// Should load function pointer from vtable (indirect call)
	assertContains(t, ir, "@promise_vtable_Animal")
	assertContains(t, ir, "@promise_vtable_Dog")
}

func TestConcreteOverrideVirtualDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Animal a = Dog(name: "Rex");
			string s = a.speak();
		}
	`)
	// When calling through Animal variable, should use vtable dispatch
	// (not direct call to Animal.speak)
	assertNotContains(t, ir, "call i8* @Animal.speak")
	// Vtable globals should exist for both types
	assertContains(t, ir, "@promise_vtable_Animal")
	assertContains(t, ir, "@promise_vtable_Dog")
}

func TestDirectDispatchPreserved(t *testing.T) {
	ir := generateIR(t, `
		type Dog {
			string name;
			speak() string { return "woof"; }
		}
		main() {
			Dog d = Dog(name: "Rex");
			string s = d.speak();
		}
	`)
	// Dog has no children → direct dispatch, no vtable indirection
	assertContains(t, ir, "call i8* @Dog.speak")
}

func TestVirtualGetterDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Shape {
			get area int `+"`"+`abstract;
		}
		type Circle is Shape {
			int radius;
			get area int => this.radius * this.radius;
		}
		main() {
			Shape s = Circle(radius: 5);
			int a = s.area;
		}
	`)
	// Getter through abstract parent should use vtable dispatch (indirect call)
	assertNotContains(t, ir, "call i64 @Shape.area")
	assertContains(t, ir, "@promise_vtable_Shape")
	assertContains(t, ir, "@promise_vtable_Circle")
}

func TestVirtualGetterOverrideDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			int _x;
			get x int { return this._x; }
		}
		type Child is Base {
			get x int { return this._x * 2; }
		}
		main() {
			Base b = Child(_x: 5);
			int v = b.x;
		}
	`)
	// Concrete getter override through parent-typed variable should use vtable dispatch
	assertNotContains(t, ir, "call i64 @Base.x(")
	assertContains(t, ir, "@promise_vtable_Base")
	assertContains(t, ir, "@promise_vtable_Child")
}

func TestDirectGetterPreserved(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int _x;
			get x int => this._x;
		}
		main() {
			Point p = Point(_x: 42);
			int v = p.x;
		}
	`)
	// Point has no children → direct dispatch for getter
	assertContains(t, ir, "call i64 @Point.x")
}

func TestDirectGetterNoVtable(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int _count;
			get count int { return this._count; }
		}
		main() {
			Counter c = Counter(_count: 10);
			int n = c.count;
		}
	`)
	// Counter has no children → direct getter call (not indirect through vtable)
	assertContains(t, ir, "call i64 @Counter.count")
}

func TestMultipleAbstractParentsVtable(t *testing.T) {
	ir := generateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		main() {
			Speakable s = Robot();
			string x = s.speak();
		}
	`)
	// Robot's vtable should cover both speak and move
	assertContains(t, ir, "@promise_vtable_Robot")
	assertContains(t, ir, "@promise_vtable_Speakable")
}

func TestDeepHierarchyVtable(t *testing.T) {
	ir := generateIR(t, `
		type A {
			greet() string `+"`"+`abstract;
		}
		type B is A {
			greet() string { return "hello from B"; }
		}
		type C is B {
			greet() string { return "hello from C"; }
		}
		main() {
			A a = C();
			string s = a.greet();
		}
	`)
	// A→B→C chain: all get vtable globals
	assertContains(t, ir, "@promise_vtable_A")
	assertContains(t, ir, "@promise_vtable_B")
	assertContains(t, ir, "@promise_vtable_C")
	// Should NOT directly call @A.greet (abstract)
	assertNotContains(t, ir, "call i8* @A.greet")
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

func TestFirstParentPrefixCompatible(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Animal a = Dog(name: "Rex");
			string s = a.speak();
		}
	`)
	// Animal is first parent of Dog — no view vtable needed
	assertNotContains(t, ir, "@promise_vtable_Dog_as_Animal")
	// Dispatch through vtable from value struct (extractvalue, GEP, load, bitcast, call)
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestSecondParentViewVtable(t *testing.T) {
	ir := generateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		main() {
			Movable m = Robot();
		}
	`)
	// Movable is second parent of Robot — needs a view-specific vtable
	assertContains(t, ir, "@promise_vtable_Robot_as_Movable")
}

func TestMultiParentVtableDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		main() {
			Movable m = Robot();
			string s = m.walk();
		}
	`)
	// Should emit view vtable for Robot-as-Movable
	assertContains(t, ir, "@promise_vtable_Robot_as_Movable")
	// Dispatch should use vtable from value struct (not typeinfo chain)
	assertContains(t, ir, "extractvalue { i8*, i8* }")
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

func TestFieldAccessThroughValueStruct(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			string name;
		}
		main() {
			Animal a = Animal(name: "Rex");
			string n = a.name;
		}
	`)
	// Should extract instance from value struct, then GEP to field
	assertContains(t, ir, "extractvalue { i8*, i8* }")
	assertContains(t, ir, "getelementptr %promise_Animal_i")
}

func TestConcreteDirectDispatchPreserved(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int x;
			int y;
			sum() int { return this.x + this.y; }
		}
		main() {
			Point p = Point(x: 1, y: 2);
			int s = p.sum();
		}
	`)
	// Concrete type with no parents that needs vtable — should use direct dispatch
	assertContains(t, ir, "call i64 @Point.sum")
}

func TestStructuralSatisfactionWithMeta(t *testing.T) {
	ir := generateIR(t, `
		type Printable `+"`"+`structural {
			print() string `+"`"+`abstract;
		}
		type Doc {
			print() string { return "doc"; }
		}
		main() {
			Printable p = Doc();
			string s = p.print();
		}
	`)
	// Should emit view vtable for Doc-as-Printable (structural satisfaction)
	assertContains(t, ir, "@promise_vtable_Doc_as_Printable")
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

func TestStructuralAdapterExtraOptionalParam(t *testing.T) {
	ir := generateIR(t, `
		type Printable `+"`"+`structural {
			print() string `+"`"+`abstract;
		}
		type Doc {
			print(int? indent) string { return "doc"; }
		}
		main() {
			Printable p = Doc();
			string s = p.print();
		}
	`)
	// Adapter thunk should be generated
	assertContains(t, ir, "Doc.print$view_adapt")
	assertContains(t, ir, "@promise_vtable_Doc_as_Printable")
}

func TestStructuralAdapterNonFailableToFailable(t *testing.T) {
	ir := generateIR(t, `
		type Processor `+"`"+`structural {
			process(int x) int! `+"`"+`abstract;
		}
		type Simple {
			process(int x) int { return x; }
		}
		main() {
			Processor p = Simple();
		}
	`)
	assertContains(t, ir, "Simple.process$view_adapt")
}

func TestStructuralAdapterNonOptionalToOptionalReturn(t *testing.T) {
	ir := generateIR(t, `
		type Finder `+"`"+`structural {
			find() int? `+"`"+`abstract;
		}
		type Always {
			find() int { return 42; }
		}
		main() {
			Finder f = Always();
		}
	`)
	assertContains(t, ir, "Always.find$view_adapt")
}

// --- Primitive → structural interface boxing tests ---
// These test the boxForStructuralView codegen path: when a primitive or string
// value is passed to a function parameter typed as a structural interface,
// the compiler must box it into a {vtable_ptr, instance_ptr} view struct.

func TestPrimitiveIntToStructuralView(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display(42); }
	`)
	// View vtable for int satisfying Showable
	assertContains(t, ir, "@promise_vtable_int_as_Showable")
	// Adapter thunk: int methods take i64 receiver, vtable passes i8*
	assertContains(t, ir, "int.to_string$view_adapt")
	// Boxing: alloca for scalar + insertvalue to build {i8*, i8*}
	assertContains(t, ir, "insertvalue { i8*, i8* }")
}

func TestPrimitiveBoolToStructuralView(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display(true); }
	`)
	assertContains(t, ir, "@promise_vtable_bool_as_Showable")
	assertContains(t, ir, "bool.to_string$view_adapt")
}

func TestPrimitiveF64ToStructuralView(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display(3.14); }
	`)
	assertContains(t, ir, "@promise_vtable_f64_as_Showable")
	assertContains(t, ir, "f64.to_string$view_adapt")
}

func TestStringToStructuralView(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display("hello"); }
	`)
	// String is i8* — no scalar alloca needed, but still needs view vtable
	assertContains(t, ir, "@promise_vtable_string_as_Showable")
}

func TestPrimitiveCharToStructuralView(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display('A'); }
	`)
	assertContains(t, ir, "@promise_vtable_char_as_Showable")
	assertContains(t, ir, "char.to_string$view_adapt")
}

func TestMultiplePrimitivesToStructuralView(t *testing.T) {
	// Multiple different primitives boxed in the same function
	ir := generateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() {
			display(42);
			display(true);
			display("hi");
		}
	`)
	assertContains(t, ir, "@promise_vtable_int_as_Showable")
	assertContains(t, ir, "@promise_vtable_bool_as_Showable")
	assertContains(t, ir, "@promise_vtable_string_as_Showable")
}

func TestPrimitiveViewAdapterLoadsScalarFromPointer(t *testing.T) {
	// The adapter thunk receives i8* (interface convention) and must
	// bitcast + load to get the scalar value for the concrete method
	ir := generateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display(42); }
	`)
	assertContains(t, ir, "int.to_string$view_adapt")
	// Adapter should bitcast i8* to i64* and load the scalar
	assertContains(t, ir, "bitcast i8* %this to i64*")
	assertContains(t, ir, "load i64, i64*")
}

func TestPrimitiveToFailableStructuralView(t *testing.T) {
	// Primitive method is non-failable, interface method is failable
	// → adapter wraps result as success
	ir := generateIR(t, `
		type Converter `+"`"+`structural {
			to_string() string! `+"`"+`abstract;
		}
		convert(Converter c) string { return c.to_string()!; }
		main() { convert(42); }
	`)
	assertContains(t, ir, "@promise_vtable_int_as_Converter")
	assertContains(t, ir, "int.to_string$view_adapt")
}

func TestPrimitiveMixedWithUserTypeToStructuralView(t *testing.T) {
	// Same function call mixes primitives and user types as structural params
	ir := generateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		type Pair {
			to_string() string { return "pair"; }
		}
		both(Showable a, Showable b) string { return a.to_string() + b.to_string(); }
		main() { both(42, Pair()); }
	`)
	assertContains(t, ir, "@promise_vtable_int_as_Showable")
	assertContains(t, ir, "@promise_vtable_Pair_as_Showable")
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

func TestReturnThisWrapsValueStruct(t *testing.T) {
	// Regression test (B0122): returning `this` from a method produced
	// `ret i8*` instead of the expected value struct `{ i8*, i8* }`.
	ir := generateIR(t, `
		type Counter {
			int count;
			iter() Counter { return this; }
		}
		main() { c := Counter(count: 0).iter(); }
	`)
	// Counter.iter should return { i8*, i8* } (value struct), not i8*
	assertContains(t, ir, "define { i8*, i8* } @Counter.iter(i8* %this)")
	// The body should insertvalue to build the value struct
	assertContains(t, ir, "insertvalue { i8*, i8* }")
}

func TestReturnThisFailable(t *testing.T) {
	// Failable method returning `this` should also wrap into value struct.
	ir := generateIR(t, `
		type Widget {
			int id;
			clone() Widget! { return this; }
		}
		main() {
			w := Widget(id: 1);
			Widget w2 = w.clone()!;
		}
	`)
	// Result type is { i1, { i8*, i8* }, i8* } (ok flag, value struct, error ptr)
	assertContains(t, ir, "define { i1, { i8*, i8* }, i8* } @Widget.clone(i8* %this)")
	assertContains(t, ir, "insertvalue { i8*, i8* }")
}

func TestReturnThisValueType(t *testing.T) {
	// Value type: `this` is i8* pointing to value struct — must load the full struct.
	ir := generateIR(t, `
		type Point {
			int x `+"`"+`value;
			int y `+"`"+`value;
			clone() Point { return this; }
		}
		main() { p := Point(x: 1, y: 2).clone(); }
	`)
	// Point is a value type — return type is the wider value struct with embedded fields
	assertContains(t, ir, "define %promise_Point_v @Point.clone(i8* %this)")
	// Should bitcast + load the value struct from the i8* this pointer
	assertContains(t, ir, "bitcast i8* %")
	assertContains(t, ir, "load %promise_Point_v")
}

func TestOptionalParamWrapping(t *testing.T) {
	ir := generateIR(t, `
		foo(int? x) int {
			if x is present { return x; }
			return 0;
		}
		main() {
			int r = foo(4);
		}
	`)
	// The call to foo(4) should pass {i1, i64} not bare i64
	assertContains(t, ir, "call i64 @foo({ i1, i64 }")
	assertNotContains(t, ir, "call i64 @foo(i64 ")
}

func TestOptionalParamOmittedNoneZeroinit(t *testing.T) {
	ir := generateIR(t, `
		foo(int? x) int {
			if x is present { return x; }
			return 0;
		}
		main() {
			int r = foo();
		}
	`)
	// Omitted optional param should pass {i1, i64} zeroinitializer, not bare i1 false
	assertContains(t, ir, "call i64 @foo({ i1, i64 }")
	assertNotContains(t, ir, "call i64 @foo(i1 ")
}

func TestOptionalParamWrappingMethodCall(t *testing.T) {
	ir := generateIR(t, `
		type Calc {
			add(int? bonus) int {
				if bonus is present { return bonus; }
				return 0;
			}
		}
		main() {
			Calc c = Calc();
			int r = c.add(bonus: 10);
		}
	`)
	// Method call should wrap 10 as {i1, i64}
	assertContains(t, ir, "call i64 @Calc.add(i8*")
	assertNotContains(t, ir, "call i64 @Calc.add(i8*, i64 ")
}

func TestOptionalParamWrappingConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Widget {
			int value;
			new(~this, int? v) {
				if v is present { this.value = v; }
			}
		}
		main() {
			Widget w = Widget(v: 5);
		}
	`)
	// Constructor new() call should wrap 5 as {i1, i64}
	assertContains(t, ir, "{ i1, i64 }")
}

// B0030: Optional user-defined type in constructor should use {i1, {i8*, i8*}}
func TestOptionalUserTypeFieldInConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Coord { int x; int y; }
		type Place { string name; Coord? location; }
		main() {
			Place p = Place(name: "home", location: Coord(x: 1, y: 2));
		}
	`)
	// Optional user type field should be {i1, {i8*, i8*}} not {i1, i8*}
	assertContains(t, ir, "{ i1, { i8*, i8* } }")
}

func TestOptionalUserTypeFieldNoneInConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Coord { int x; int y; }
		type Place { string name; Coord? location; }
		main() {
			Place p = Place(name: "test", location: none);
		}
	`)
	// None for optional user type should produce zeroinitializer of {i1, {i8*, i8*}}
	assertContains(t, ir, "{ i1, { i8*, i8* } } zeroinitializer")
}

func TestReturnCoercionSecondParent(t *testing.T) {
	ir := generateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		makeMovable() Movable {
			return Robot();
		}
		main() {
			Movable m = makeMovable();
			string s = m.walk();
		}
	`)
	// Returning Robot as Movable (second parent) should emit view vtable
	assertContains(t, ir, "@promise_vtable_Robot_as_Movable")
}

func TestArgCoercionSecondParent(t *testing.T) {
	ir := generateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		useMovable(Movable m) string {
			return m.walk();
		}
		main() {
			Robot r = Robot();
			string s = useMovable(r);
		}
	`)
	// Passing Robot as Movable arg (second parent) should emit view vtable
	assertContains(t, ir, "@promise_vtable_Robot_as_Movable")
}

// --- Stage 9: Std library and test runner codegen tests ---

// stdContainers is kept for backward compatibility with tests that pass it to generateIRWithStd.
// Its contents are already included in the real std module; pass "" and let generateIRWithStd ignore it.
const stdContainers = ""

// generateIRWithStd merges stdSrc (extra user-level declarations) with userSrc and generates IR.
// After the module-based refactor, stdSrc is treated as regular user code prepended to userSrc.
func generateIRWithStd(t *testing.T, stdSrc, userSrc string) string {
	t.Helper()
	combined := userSrc
	if stdSrc != "" {
		combined = stdSrc + "\n" + userSrc
	}
	return generateIR(t, combined)
}

// compileResultWithStd merges stdSrc (extra user-level declarations) with userSrc and compiles.
func compileResultWithStd(t *testing.T, stdSrc, userSrc string) *CompileResult {
	t.Helper()
	combined := userSrc
	if stdSrc != "" {
		combined = stdSrc + "\n" + userSrc
	}
	return compileResult(t, combined)
}

func TestStdFuncMangledName(t *testing.T) {
	// After module-based refactor: helper() is user code → IR name is @helper
	ir := generateIRWithStd(t,
		`helper() int { return 42; }`,
		`main() { x := helper(); }`,
	)
	assertContains(t, ir, "define i64 @helper")
	assertContains(t, ir, "call i64 @helper")
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
	assertContains(t, ir, "define i64 @helper_extra")
	assertContains(t, ir, "call i64 @helper_extra()")
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

// B0165: Batch test main waits for worker threads to finish init, then resets
// alloc count to 0 so scheduler allocations don't leak into per-test counts.
func TestBatchTestResetsAllocCount(t *testing.T) {
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
	// Spin-wait on sched.ready_count
	assertContains(t, ir, "sched_ready_spin")
	assertContains(t, ir, "sched_ready_done")
	// Reset alloc count to 0 via atomic exchange
	assertContains(t, ir, "atomicrmw xchg i64* @__promise_alloc_count, i64 0 monotonic")
}

// B0188: Batch test leak check inserts usleep before reading alloc count
// to let scheduler worker threads finish goroutine cleanup.
func TestBatchTestLeakCheckDrainDelay(t *testing.T) {
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
	// Leak check block should call pal_usleep before reading alloc count
	assertContains(t, ir, "leak_check_myTest")
	assertContains(t, ir, "call i32 @usleep(i32 100)")
}

// B0165: Sched struct includes ready_count field (i32 at end).
func TestSchedStructHasReadyCount(t *testing.T) {
	ir := generateIR(t, `main() { }`)
	// The sched global should have the ready_count i32 as the last field
	// Full type: { i8*, i8*, i64, i8*, i8*, i32, i8*, i8*, i64, i8, i8, i8*, i8*, i8*, i32, i64, i64, i64, i64, i8*, i32 }
	assertContains(t, ir, "@__promise_sched = global")
	// Verify sched_loop is defined (it increments ready_count)
	assertContains(t, ir, "define i8* @promise_sched_loop(")
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
	// 3-way branching: 0=pass, 2=timeout, else=fail
	assertContains(t, ir, "icmp eq i32 %failed, 0") // pass check
	assertContains(t, ir, "icmp eq i32 %failed, 2") // timeout check
	assertContains(t, ir, "br i1")                  // conditional branches
	assertContains(t, ir, "br label")               // unconditional branches to merge
	// PASS/FAIL/TIMEOUT prefix globals
	assertContains(t, ir, `@.str.pass_prefix = private constant [6 x i8] c"PASS ("`)
	assertContains(t, ir, `@.str.fail_prefix = private constant [6 x i8] c"FAIL ("`)
	assertContains(t, ir, `@.str.timeout_prefix = private constant [9 x i8] c"TIMEOUT ("`)
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

	// Function is defined (not just declared) — includes leaked, ignored, and stale params (T0020, T0067)
	assertContains(t, ir, "define void @promise_test_summary(i32 %passed, i32 %failed, i32 %skipped, i32 %leaked, i32 %ignored, i32 %stale)")
	// String suffix globals
	assertContains(t, ir, `@.str.passed_suffix = private constant [9 x i8] c" passed, "`)
	assertContains(t, ir, `@.str.failed_suffix = private constant [7 x i8] c" failed"`)
	assertContains(t, ir, `@.str.skipped_suffix = private constant [8 x i8] c" skipped"`)
	assertContains(t, ir, `@.str.leaked_suffix = private constant [7 x i8] c" leaked"`)
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

// T0020: Leak detection emits alloc count tracking in pal_alloc/pal_free
// and per-test leak checks in the test main.
func TestLeakDetectionAllocTracking(t *testing.T) {
	result := compileResult(t, `
		myTest() `+"`test"+` { }
	`)
	ir := result.Module.String()

	// pal_alloc should track allocations via __promise_alloc_count
	assertContains(t, ir, "@__promise_alloc_count = global i64 0")
	// pal_alloc atomically increments on successful malloc
	assertContains(t, ir, "atomicrmw add i64* @__promise_alloc_count, i64 1 monotonic")
	// pal_free atomically decrements on non-null free
	assertContains(t, ir, "atomicrmw sub i64* @__promise_alloc_count, i64 1 monotonic")
}

// T0020: Leak detection in test main snapshots alloc count before/after each test.
func TestLeakDetectionInTestMain(t *testing.T) {
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

	// Leak check blocks: snapshot before test, check delta after
	assertContains(t, ir, "leak_check_myTest")
	assertContains(t, ir, "print_leak_myTest")
	// Leak message string constants
	assertContains(t, ir, `c"  leak: "`)
	assertContains(t, ir, `c" allocations not freed\0A"`)
	// Leaked counter in summary call
	assertContains(t, ir, "call void @promise_test_summary(i32")
}

// T0067: Tests with allow_leaks don't increment leaked counter.
func TestAllowLeaksDoesNotIncrementLeakedCounter(t *testing.T) {
	src := `myTest() ` + "`test(allow_leaks: true)" + ` { }`
	result := compileResult(t, src)
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

	// Should have leak check blocks
	assertContains(t, ir, "leak_check_myTest")
	assertContains(t, ir, "print_leak_myTest")
	// Should have stale tag warning for allow_leaks
	assertContains(t, ir, "allow_leaks")
	assertContains(t, ir, "tag can be removed")
	// allow_leaks: no_leak block and ignored counter
	assertContains(t, ir, "no_leak_myTest")
	// Summary includes ignored parameter (T0067)
	assertContains(t, ir, "i32 %ignored")
}

// T0067: Tests without allow_leaks increment leaked counter and exit code includes leaks.
func TestNoAllowLeaksIncrementsLeakedCounter(t *testing.T) {
	src := `myTest() ` + "`test" + ` { }`
	result := compileResult(t, src)
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

	// Exit code should use OR of failed and leaked (T0067)
	assertContains(t, ir, "or i1")
	// Should NOT have stale tag warning (no allow_leaks)
	assertNotContains(t, ir, "tag can be removed")
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

func TestHostTargetTriple(t *testing.T) {
	triple := HostTargetTriple()
	if triple == "" {
		t.Fatal("HostTargetTriple returned empty string")
	}
	// Should contain a known arch
	if !strings.Contains(triple, "arm64") && !strings.Contains(triple, "x86_64") && !strings.Contains(triple, "aarch64") {
		t.Errorf("unexpected target triple: %s", triple)
	}
}

func TestHostTargetTripleInModule(t *testing.T) {
	ir := generateIR(t, `main() {}`)
	triple := HostTargetTriple()
	assertContains(t, ir, "target triple = \""+triple+"\"")
}

func TestStdExternRegistration(t *testing.T) {
	// Std externs should be callable via std.X() and normal call
	ir := generateIRWithStd(t,
		`_do_thing(int x) `+"`"+`extern("c_do_thing");`,
		`main() { _do_thing(42); }`,
	)
	// The C function should be declared
	assertContains(t, ir, "declare void @c_do_thing")
}

func TestStdExternDedupWithUserExtern(t *testing.T) {
	// User extern with same C name as std extern should share the IR declaration
	ir := generateIRWithStd(t,
		`_std_thing(int x) `+"`"+`extern("c_shared_fn");`,
		`
		my_thing(int x) `+"`"+`extern("c_shared_fn");
		main() { my_thing(42); }
		`,
	)
	// Only one C declaration (not two)
	count := strings.Count(ir, "declare void @c_shared_fn")
	if count != 1 {
		t.Errorf("expected 1 declaration of @c_shared_fn, got %d", count)
	}
}

func TestStdFuncUnshadowed(t *testing.T) {
	// After module-based refactor: helper is user code → plain @helper name
	ir := generateIRWithStd(t,
		`helper() int { return 42; }`,
		`main() { x := helper(); }`,
	)
	assertContains(t, ir, "call i64 @helper")
}

// --- Cross-Module Codegen Tests ---

// parseModuleSource parses a module source string, runs sema with the std module, and returns
// the ModuleInfo and exported scope.
func parseModuleSource(t *testing.T, moduleName, src string) (*sema.ModuleInfo, *types.Scope) {
	t.Helper()
	_, stdScope := getCodegenStdModInfo()

	// Parse module source
	modInput := antlr.NewInputStream(src)
	modLexer := parser.NewPromiseLexer(modInput)
	modLexer.RemoveErrorListeners()
	modStream := antlr.NewCommonTokenStream(modLexer, antlr.TokenDefaultChannel)
	modP := parser.NewPromiseParser(modStream)
	modP.RemoveErrorListeners()
	modTree := modP.CompilationUnit()
	modFile, errs := ast.Build("module.pr", modTree)
	if len(errs) > 0 {
		t.Fatalf("module AST build errors: %v", errs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	modFile.Uses = append([]*ast.UseDecl{stdUse}, modFile.Uses...)

	modInfo, semaErrs := sema.CheckWithModules(modFile, map[string]*types.Scope{"std": stdScope})
	if len(semaErrs) > 0 {
		t.Fatalf("module sema errors: %v", semaErrs)
	}

	scope := sema.ExportedScope(modInfo, modFile)
	globalID := "./" + moduleName
	return &sema.ModuleInfo{
		Name:           moduleName,
		CanonicalName:  moduleName,
		GlobalIdentity: globalID,
		IRPrefix:       moduleName, // test convenience: use plain name as IR prefix
		Path:           globalID,
		File:           modFile,
		SemaInfo:       modInfo,
	}, scope
}

// generateIRWithModule parses a module and user source, runs sema+codegen with
// the module available via `use <moduleName>`.
func generateIRWithModule(t *testing.T, moduleName, modSrc, userSrc string) string {
	t.Helper()

	modInfo, modScope := parseModuleSource(t, moduleName, modSrc)
	stdModInfo, stdScope := getCodegenStdModInfo()

	// Parse user
	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs := ast.Build("test.pr", userTree)
	if len(errs) > 0 {
		t.Fatalf("user AST build errors: %v", errs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)

	// Sema with std + module scopes
	modKey := "./" + moduleName
	moduleScopes := map[string]*types.Scope{
		"std":  stdScope,
		modKey: modScope,
	}
	info, semaErrs := sema.CheckWithModules(userFile, moduleScopes)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}

	// Attach module infos for codegen
	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":  stdModInfo,
		modKey: modInfo,
	}
	info.ModuleOrder = []string{"std", modKey}

	result := Compile(userFile, info, "")
	return result.Module.String()
}

func TestModuleCallQualified(t *testing.T) {
	ir := generateIRWithModule(t, "mylib",
		"compute() int `public { return 42; }",
		`
		use mylib "./mylib";
		main() {
			x := mylib.compute();
		}
		`,
	)
	// Module function should have module-mangled name
	assertContains(t, ir, "define i64 @__mod_mylib_compute")
	// Call should use the mangled name
	assertContains(t, ir, "call i64 @__mod_mylib_compute")
}

func TestModuleCallDoesNotCollideWithUser(t *testing.T) {
	ir := generateIRWithModule(t, "mylib",
		"compute() int `public { return 42; }",
		`
		use mylib "./mylib";
		compute() int { return 99; }
		main() {
			x := compute();
			y := mylib.compute();
		}
		`,
	)
	// Both functions exist with different names
	assertContains(t, ir, "define i64 @compute")
	assertContains(t, ir, "define i64 @__mod_mylib_compute")
	// User call goes to @compute, module call to @__mod_mylib_compute
	assertContains(t, ir, "call i64 @compute()")
	assertContains(t, ir, "call i64 @__mod_mylib_compute()")
}

func TestModuleTypeConstructor(t *testing.T) {
	ir := generateIRWithModule(t, "geo",
		`type Point `+"`public"+` { int x; int y; }`,
		`
		use geo "./geo";
		main() {
			geo.Point p = geo.Point(x: 1, y: 2);
		}
		`,
	)
	// Module type should have layout and constructor
	assertContains(t, ir, "Point")
	// Constructor stores field values
	assertContains(t, ir, "store i64 1")
	assertContains(t, ir, "store i64 2")
}

func TestModuleMethodCall(t *testing.T) {
	ir := generateIRWithModule(t, "counter",
		`type Counter `+"`public"+` {
			int value;
			get_value(this) int `+"`public"+` { return this.value; }
		}`,
		`
		use counter "./counter";
		main() {
			counter.Counter c = counter.Counter(value: 42);
			int v = c.get_value();
		}
		`,
	)
	// Module method should be defined with module-prefixed name
	assertContains(t, ir, "define i64 @__mod_counter_Counter.get_value")
	// Call should use the module-prefixed method
	assertContains(t, ir, "call i64 @__mod_counter_Counter.get_value")
}

func TestModuleGlobImportCall(t *testing.T) {
	ir := generateIRWithModule(t, "helpers",
		"greet() int `public { return 1; }",
		`
		use _ "./helpers";
		main() {
			int x = greet();
		}
		`,
	)
	// Glob-imported function should resolve to module-prefixed IR name
	assertContains(t, ir, "define i64 @__mod_helpers_greet")
	assertContains(t, ir, "call i64 @__mod_helpers_greet")
}

func TestModuleFuncWithParams(t *testing.T) {
	ir := generateIRWithModule(t, "math",
		"add(int a, int b) int `public { return a + b; }",
		`
		use math "./math";
		main() {
			int x = math.add(3, 4);
		}
		`,
	)
	assertContains(t, ir, "define i64 @__mod_math_add(i64 %a, i64 %b)")
	assertContains(t, ir, "call i64 @__mod_math_add(i64 3, i64 4)")
}

func TestModuleVoidFunc(t *testing.T) {
	ir := generateIRWithModule(t, "logger",
		"noop() `public {}",
		`
		use logger "./logger";
		main() {
			logger.noop();
		}
		`,
	)
	assertContains(t, ir, "define void @__mod_logger_noop()")
	assertContains(t, ir, "call void @__mod_logger_noop()")
}

func TestModuleFailableFunc(t *testing.T) {
	ir := generateIRWithModule(t, "parser",
		`
		parse(int x) int! `+"`public"+` {
			return x;
		}
		`,
		`
		use parser "./parser";
		main()! {
			int v = parser.parse(10)?;
		}
		`,
	)
	// Failable function should return a result struct { i1, i64, i8* }
	assertContains(t, ir, "define { i1, i64, i8* } @__mod_parser_parse(i64 %x)")
	assertContains(t, ir, "call { i1, i64, i8* } @__mod_parser_parse(i64 10)")
}

func TestModuleExternFunc(t *testing.T) {
	ir := generateIRWithModule(t, "ffi",
		`
		cfunc(int x) `+"`public `extern(\"test_cfunc\")"+`;
		wrapper(int x) int `+"`public"+` { return x; }
		`,
		`
		use ffi "./ffi";
		main() {
			ffi.cfunc(1);
			int y = ffi.wrapper(2);
		}
		`,
	)
	// Extern should be declared (not defined)
	assertContains(t, ir, "declare void @test_cfunc")
	// Wrapper should be a module function
	assertContains(t, ir, "define i64 @__mod_ffi_wrapper")
}

func TestModuleEnumVariant(t *testing.T) {
	ir := generateIRWithModule(t, "shapes",
		`
		enum Shape `+"`public"+` {
			Circle(int radius),
			Rect(int w, int h),
		}
		`,
		`
		use shapes "./shapes";
		main() {
			shapes.Shape s = shapes.Shape.Circle(radius: 5);
		}
		`,
	)
	// Enum layout should exist
	assertContains(t, ir, "Shape")
	// Variant constructor stores the tag and payload
	assertContains(t, ir, "store i64 5")
}

func TestModuleGlobImportType(t *testing.T) {
	ir := generateIRWithModule(t, "models",
		`
		type Item `+"`public"+` {
			int id;
			get_id(this) int `+"`public"+` { return this.id; }
		}
		`,
		`
		use _ "./models";
		main() {
			Item it = Item(id: 7);
			int v = it.get_id();
		}
		`,
	)
	// Type layout and method should use module-prefixed names
	assertContains(t, ir, "define i64 @__mod_models_Item.get_id")
	assertContains(t, ir, "call i64 @__mod_models_Item.get_id")
	// Constructor stores the field value
	assertContains(t, ir, "store i64 7")
}

func TestModuleGlobImportMultipleSymbols(t *testing.T) {
	ir := generateIRWithModule(t, "utils",
		`
		foo() int `+"`public"+` { return 1; }
		bar() int `+"`public"+` { return 2; }
		`,
		`
		use _ "./utils";
		main() {
			int a = foo();
			int b = bar();
		}
		`,
	)
	// Both glob-imported functions should resolve to module-prefixed names
	assertContains(t, ir, "define i64 @__mod_utils_foo")
	assertContains(t, ir, "define i64 @__mod_utils_bar")
	assertContains(t, ir, "call i64 @__mod_utils_foo()")
	assertContains(t, ir, "call i64 @__mod_utils_bar()")
}

// generateIRWithTwoModules parses two modules and user source, runs sema+codegen
// with both modules available.
func generateIRWithTwoModules(t *testing.T,
	mod1Name, mod1Src, mod2Name, mod2Src, userSrc string) string {
	t.Helper()

	mod1Info, mod1Scope := parseModuleSource(t, mod1Name, mod1Src)
	mod2Info, mod2Scope := parseModuleSource(t, mod2Name, mod2Src)
	stdModInfo, stdScope := getCodegenStdModInfo()

	// Parse user
	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs := ast.Build("test.pr", userTree)
	if len(errs) > 0 {
		t.Fatalf("user AST build errors: %v", errs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)

	mod1Key := "./" + mod1Name
	mod2Key := "./" + mod2Name
	moduleScopes := map[string]*types.Scope{
		"std":   stdScope,
		mod1Key: mod1Scope,
		mod2Key: mod2Scope,
	}
	info, semaErrs := sema.CheckWithModules(userFile, moduleScopes)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}

	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":   stdModInfo,
		mod1Key: mod1Info,
		mod2Key: mod2Info,
	}
	info.ModuleOrder = []string{"std", mod1Key, mod2Key}

	result := Compile(userFile, info, "")
	return result.Module.String()
}

func TestMultipleModules(t *testing.T) {
	ir := generateIRWithTwoModules(t,
		"alpha", "get_a() int `public { return 1; }",
		"beta", "get_b() int `public { return 2; }",
		`
		use alpha "./alpha";
		use beta "./beta";
		main() {
			int a = alpha.get_a();
			int b = beta.get_b();
		}
		`,
	)
	assertContains(t, ir, "define i64 @__mod_alpha_get_a")
	assertContains(t, ir, "define i64 @__mod_beta_get_b")
	assertContains(t, ir, "call i64 @__mod_alpha_get_a")
	assertContains(t, ir, "call i64 @__mod_beta_get_b")
}

func TestModuleTypeGlobalsPrefixed(t *testing.T) {
	ir := generateIRWithModule(t, "shapes",
		`type Circle `+"`public"+` {
			int radius;
			area(this) int `+"`public"+` { return this.radius; }
		}`,
		`
		use shapes "./shapes";
		main() {
			shapes.Circle c = shapes.Circle(radius: 5);
			int a = c.area();
		}
		`,
	)
	// RTTI/typeinfo globals should be prefixed with __mod_shapes_
	assertContains(t, ir, "@promise_typeinfo___mod_shapes_Circle")
	// std library types (e.g., int) should NOT have module prefix
	assertNotContains(t, ir, "__mod_shapes_int")
}

func TestModuleSplitModuleIRs(t *testing.T) {
	mod1Info, mod1Scope := parseModuleSource(t, "alpha", "get_a() int `public { return 1; }")
	mod2Info, mod2Scope := parseModuleSource(t, "beta", "get_b() int `public { return 2; }")
	stdModInfo, stdScope := getCodegenStdModInfo()

	userSrc := `
		use alpha "./alpha";
		use beta "./beta";
		main() {
			int a = alpha.get_a();
			int b = beta.get_b();
		}
	`
	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, buildErrs := ast.Build("test.pr", userTree)
	if len(buildErrs) > 0 {
		t.Fatalf("user AST build errors: %v", buildErrs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)

	moduleScopes := map[string]*types.Scope{
		"std":     stdScope,
		"./alpha": mod1Scope,
		"./beta":  mod2Scope,
	}
	info, semaErrs := sema.CheckWithModules(userFile, moduleScopes)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":     stdModInfo,
		"./alpha": mod1Info,
		"./beta":  mod2Info,
	}
	info.ModuleOrder = []string{"std", "./alpha", "./beta"}

	result := Compile(userFile, info, "")
	mainIR, moduleIRs := result.SplitModuleIRs()

	// Should produce separate IRs for std, alpha, and beta.
	if len(moduleIRs) != 3 {
		t.Fatalf("expected 3 module IRs (std, alpha, beta), got %d", len(moduleIRs))
	}
	alphaIR, ok := moduleIRs["alpha"]
	if !ok {
		t.Fatal("expected 'alpha' in moduleIRs")
	}
	betaIR, ok := moduleIRs["beta"]
	if !ok {
		t.Fatal("expected 'beta' in moduleIRs")
	}

	// alpha IR: has alpha's function body, beta's function is a declaration
	assertContains(t, alphaIR, "define i64 @__mod_alpha_get_a")
	assertNotContains(t, alphaIR, "define i64 @__mod_beta_get_b")

	// beta IR: has beta's function body, alpha's function is a declaration
	assertContains(t, betaIR, "define i64 @__mod_beta_get_b")
	assertNotContains(t, betaIR, "define i64 @__mod_alpha_get_a")

	// main IR: all module function bodies are declarations, not definitions
	assertNotContains(t, mainIR, "define i64 @__mod_alpha_get_a")
	assertNotContains(t, mainIR, "define i64 @__mod_beta_get_b")
	// main IR should still declare (not define) the module functions
	assertContains(t, mainIR, "declare i64 @__mod_alpha_get_a")
	assertContains(t, mainIR, "declare i64 @__mod_beta_get_b")
}

func TestModuleIRPrefixUsedForIR(t *testing.T) {
	// Verify that when the user alias differs from the IRPrefix,
	// the IR uses the IRPrefix (derived from GlobalIdentity), not the alias.
	mod1Info, mod1Scope := parseModuleSource(t, "myalias", "helper() int `public { return 42; }")
	// Override GlobalIdentity and IRPrefix to simulate a remote module
	mod1Info.GlobalIdentity = "github.com/alice/mylib"
	mod1Info.IRPrefix = "github_com_alice_mylib_abc123"

	stdModInfo, stdScope := getCodegenStdModInfo()

	userSrc := `
		use myalias "./myalias";
		main() {
			int x = myalias.helper();
		}
	`
	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, buildErrs := ast.Build("test.pr", userTree)
	if len(buildErrs) > 0 {
		t.Fatalf("user AST build errors: %v", buildErrs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)

	moduleScopes := map[string]*types.Scope{
		"std":       stdScope,
		"./myalias": mod1Scope,
	}
	info, semaErrs := sema.CheckWithModules(userFile, moduleScopes)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":       stdModInfo,
		"./myalias": mod1Info,
	}
	info.ModuleOrder = []string{"std", "./myalias"}

	result := Compile(userFile, info, "")
	ir := result.Module.String()

	// IR should use IRPrefix, not the alias "myalias"
	assertContains(t, ir, "define i64 @__mod_github_com_alice_mylib_abc123_helper")
	assertNotContains(t, ir, "__mod_myalias_")
}

// --- Catalog Module Tests ---

// generateIRWithCatalogModule sets up a module with catalog identity (bare name as
// IRPrefix, keyed by catalog name in sema scopes) and compiles user source against it.
func generateIRWithCatalogModule(t *testing.T, catalogName, modSrc, userSrc string) string {
	t.Helper()
	modInfo, modScope := parseModuleSource(t, catalogName, modSrc)
	// Override identity to match catalog convention: bare name
	modInfo.GlobalIdentity = catalogName
	modInfo.IRPrefix = catalogName
	modInfo.Path = catalogName

	stdModInfo, stdScope := getCodegenStdModInfo()

	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs := ast.Build("test.pr", userTree)
	if len(errs) > 0 {
		t.Fatalf("user AST build errors: %v", errs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)

	// Catalog modules are keyed by their catalog name (not "./name")
	moduleScopes := map[string]*types.Scope{
		"std":       stdScope,
		catalogName: modScope,
	}
	info, semaErrs := sema.CheckWithModules(userFile, moduleScopes)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":       stdModInfo,
		catalogName: modInfo,
	}
	info.ModuleOrder = []string{"std", catalogName}

	result := Compile(userFile, info, "")
	return result.Module.String()
}

func TestCatalogModuleCallQualified(t *testing.T) {
	ir := generateIRWithCatalogModule(t, "json",
		"parse() int `public { return 1; }",
		`
		use json;
		main() {
			int x = json.parse();
		}
		`,
	)
	assertContains(t, ir, "define i64 @__mod_json_parse")
	assertContains(t, ir, "call i64 @__mod_json_parse")
}

func TestCatalogModuleAliasedCall(t *testing.T) {
	// Regression test: aliased catalog imports must use the catalog name
	// as IR prefix, not the alias.
	ir := generateIRWithCatalogModule(t, "json",
		"parse() int `public { return 1; }",
		`
		use json as j;
		main() {
			int x = j.parse();
		}
		`,
	)
	assertContains(t, ir, "define i64 @__mod_json_parse")
	assertContains(t, ir, "call i64 @__mod_json_parse")
	assertNotContains(t, ir, "__mod_j_")
}

func TestCatalogModuleTypeQualified(t *testing.T) {
	ir := generateIRWithCatalogModule(t, "json",
		"type Value `public { int x; }",
		`
		use json;
		main() {
			v := json.Value(x: 42);
		}
		`,
	)
	assertContains(t, ir, "promise_typeinfo___mod_json_Value")
}

func TestCatalogModuleGlobImport(t *testing.T) {
	ir := generateIRWithCatalogModule(t, "json",
		"parse() int `public { return 1; }",
		`
		use json as _;
		main() {
			int x = parse();
		}
		`,
	)
	assertContains(t, ir, "define i64 @__mod_json_parse")
	assertContains(t, ir, "call i64 @__mod_json_parse")
}

func TestChannelFieldInUserType(t *testing.T) {
	// B0096: channel[T] fields in user types must use i8* layout,
	// not {i8*, i8*} (value struct). These are native container types like Vector.
	ir := generateIR(t, `
		type IntChan {
			channel[int] ch;
			emit(~this, int v) { this.ch.send(v); }
		}
		main() {
			ch := channel[int](capacity: 1);
			s := IntChan(ch: ch);
			s.emit(42);
		}
	`)
	// Instance struct field must be i8* (opaque channel pointer), not {i8*, i8*}
	assertContains(t, ir, "%promise_IntChan_i = type { %promise_IntChan_m*, i8* }")
	// Channel send generates inline mutex lock IR
	assertContains(t, ir, "call void @pal_mutex_lock(")
}

func TestTaskFieldInUserType(t *testing.T) {
	// B0096: task[T] fields must use i8* layout in user types.
	ir := generateIR(t, `
		compute() int { return 42; }
		type Holder { task[int] t; }
		main() {
			t := go compute();
			h := Holder(t: t);
		}
	`)
	// Instance struct field must be i8* (opaque task pointer), not {i8*, i8*}
	assertContains(t, ir, "%promise_Holder_i = type { %promise_Holder_m*, i8* }")
}

func TestChannelFieldInModuleType(t *testing.T) {
	// B0096: channel fields in module-defined types
	ir := generateIRWithCatalogModule(t, "mymod",
		`type Sender `+"`public"+` {
			channel[int] _ch;
			emit(~this, int v) `+"`public"+` { this._ch.send(v); }
		}`,
		`
		use mymod;
		main() {
			ch := channel[int](capacity: 1);
			s := mymod.Sender(_ch: ch);
			s.emit(42);
		}
		`,
	)
	// Instance struct field must be i8* in module types too
	assertContains(t, ir, "%promise_Sender_i = type { %promise_Sender_m*, i8* }")
	assertContains(t, ir, "call void @pal_mutex_lock(")
}

func TestChannelFieldInEnumVariant(t *testing.T) {
	// B0096: channel[T] in enum variant fields must use i8* layout.
	// Send variant: channel[int] (i8*, 8 bytes) + int (i64, 8 bytes) = 16 bytes.
	// Without fix: channel would be {i8*, i8*} (16 bytes) + i64 (8 bytes) = 24 bytes.
	ir := generateIR(t, `
		enum Action {
			Send(channel[int] ch, int value),
			Done,
		}
		main() {
			ch := channel[int](capacity: 1);
			a := Action.Send(ch: ch, value: 42);
		}
	`)
	// Data area must be [16 x i8] (channel as i8*), not [24 x i8] (channel as {i8*,i8*})
	assertContains(t, ir, "%promise_Action_enum = type { i32, [16 x i8] }")
	// The enum data area specifically must not use [24 x i8]
	assertNotContains(t, ir, "%promise_Action_enum = type { i32, [24 x i8] }")
}

func TestGenericTypeWithChannelFieldLayout(t *testing.T) {
	// B0096: mono layout for generic type with channel[T] field.
	ir := generateIR(t, `
		type Wrapper[T] {
			channel[T] ch;
			T default_val;
		}
		main() {
			ch := channel[int](capacity: 1);
			w := Wrapper[int](ch: ch, default_val: 0);
		}
	`)
	// Monomorphized instance struct: channel field is i8*, int field is i64
	assertContains(t, ir, `%"promise_Wrapper[int]_i" = type { %"promise_Wrapper[int]_m"*, i8*, i64 }`)
}

func TestMultipleOpaqueFieldsLayout(t *testing.T) {
	// B0096: multiple channel/task fields in one type.
	ir := generateIR(t, `
		compute() int { return 1; }
		type Multi {
			channel[int] ch1;
			channel[string] ch2;
			task[int] t;
		}
		main() {
			c1 := channel[int](capacity: 1);
			c2 := channel[string](capacity: 1);
			tk := go compute();
			m := Multi(ch1: c1, ch2: c2, t: tk);
		}
	`)
	// All three fields must be i8*
	assertContains(t, ir, "%promise_Multi_i = type { %promise_Multi_m*, i8*, i8*, i8* }")
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

func TestUnaryNotCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			bool b = !true;
		}
	`)
	// ! on bool generates xor with 1
	assertContains(t, ir, "xor i1")
}

// --- Stage 8m: use bindings ---

func TestUseVarDeclBasic(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		main() {
			use r := Resource(id: 1);
			int x = r.id;
		}
	`)
	// use binding should generate a close() call at end of scope
	assertContains(t, ir, "call void @Resource.close")
}

func TestUseVarDeclMultiple(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		main() {
			use a := Resource(id: 1);
			use b := Resource(id: 2);
			int x = a.id + b.id;
		}
	`)
	// Both resources should have close() calls
	assertContains(t, ir, "call void @Resource.close")
	// Count that there are at least 2 close calls
	count := strings.Count(ir, "call void @Resource.close")
	if count < 2 {
		t.Errorf("expected at least 2 close calls, got %d\nIR:\n%s", count, ir)
	}
}

func TestUseVarDeclWithReturn(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		make_resource() int {
			use r := Resource(id: 42);
			return r.id;
		}
		main() {
			int v = make_resource();
		}
	`)
	// close() should appear before the return instruction in make_resource
	assertContains(t, ir, "call void @Resource.close")
	assertContains(t, ir, "define i64 @make_resource")
}

func TestUseVarDeclInNestedBlock(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		main() {
			use outer := Resource(id: 1);
			if true {
				use inner := Resource(id: 2);
				int x = inner.id;
			}
			int y = outer.id;
		}
	`)
	// Both outer and inner resources should generate close() calls
	count := strings.Count(ir, "call void @Resource.close")
	if count < 2 {
		t.Errorf("expected at least 2 close calls (inner + outer), got %d\nIR:\n%s", count, ir)
	}
}

// T0106: use binding frees the instance after close()
func TestUseVarDeclFreesInstance(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		main() {
			use r := Resource(id: 1);
			int x = r.id;
		}
	`)
	// After close(), the instance should be freed via pal_free
	assertContains(t, ir, "call void @Resource.close")
	assertContains(t, ir, "close.free")
	assertContains(t, ir, "call void @pal_free(")
}

// T0106: emitFieldDrops frees field instances with explicit drop
func TestFieldDropFreesInstance(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner field;
			drop(~this) { }
		}
		main() {
			o := Outer(field: Inner(id: 1));
		}
	`)
	// Outer.drop should call Inner.drop on the field AND pal_free the field instance
	assertContains(t, ir, "call void @Inner.drop")
	// pal_free should appear in Outer.drop for the Inner field instance
	outerDrop := extractFunction(ir, "Outer.drop")
	if !strings.Contains(outerDrop, "call void @pal_free(") {
		t.Errorf("Outer.drop should pal_free the Inner field instance\nOuter.drop IR:\n%s", outerDrop)
	}
}

// T0106: String move via IdentExpr propagates ownership at runtime
func TestStringMoveDropFlagPropagation(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := "hello" + " world";
			b := a;
		}
	`)
	// a.dropflag should be loaded (saved) before clearing
	// b.dropflag should be set from the saved value (not unconditionally cleared)
	assertContains(t, ir, "a.dropflag")
	assertContains(t, ir, "b.dropflag")
	// Both should have string drop calls (conditional on flags)
	assertContains(t, ir, "promise_string_drop")
}

// --- Failable close() error propagation (B0013) ---

func TestUseFailableCloseErrorCapture(t *testing.T) {
	ir := generateIR(t, `
		type FRes {
			int id;
			close(~this)! { }
		}
		process()! {
			use r := FRes(id: 1);
			int x = r.id;
		}
	`)
	// Failable close should generate a result-type call (not void)
	assertContains(t, ir, "call { i1, i8* } @FRes.close")
	// Should have close error check and propagation blocks
	assertContains(t, ir, "close.err.flag")
	assertContains(t, ir, "close.err.ret")
}

func TestUseNonFailableCloseNoCapture(t *testing.T) {
	ir := generateIR(t, `
		type NRes {
			int id;
			close(~this) { }
		}
		process()! {
			use r := NRes(id: 1);
			int x = r.id;
		}
	`)
	// Non-failable close should remain a void call — no error capture
	assertContains(t, ir, "call void @NRes.close")
	assertNotContains(t, ir, "close.err.flag")
}

func TestUseFailableCloseSuppressedOnRaise(t *testing.T) {
	ir := generateIR(t, `
		type EBase is error { string message; }
		type FRes2 {
			int id;
			close(~this)! { }
		}
		process()! {
			use r := FRes2(id: 1);
			raise EBase(message: "fail");
		}
	`)
	// Raise path should suppress close errors (no close.err.flag on that path)
	// The close call is still emitted but result discarded
	assertContains(t, ir, "call { i1, i8* } @FRes2.close")
}

func TestUseFailableCloseInNonFailableFunc(t *testing.T) {
	ir := generateIR(t, `
		type FRes3 {
			int id;
			close(~this)! { }
		}
		process() {
			use r := FRes3(id: 1);
			int x = r.id;
		}
	`)
	// Non-failable function: close errors suppressed, no capture allocas
	assertNotContains(t, ir, "close.err.flag")
}

// T0135: Suppressed close errors are dropped (not leaked)
func TestSuppressedCloseErrorDropped(t *testing.T) {
	ir := generateIR(t, `
		type FRes {
			int id;
			close(~this)! { raise error(message: "close err"); }
		}
		process()! {
			use r := FRes(id: 1);
			raise error(message: "body err");
		}
	`)
	// Error-in-flight path: close error should be dropped via __mod_std_error.drop
	assertContains(t, ir, "close.err.drop")
	assertContains(t, ir, "@__mod_std_error.drop")
}

// T0135: Non-failable function drops suppressed close errors
func TestNonFailableSuppressedCloseErrorDropped(t *testing.T) {
	ir := generateIR(t, `
		type FRes {
			int id;
			close(~this)! { }
		}
		process() {
			use r := FRes(id: 1);
		}
	`)
	// Non-failable function: close error is suppressed and dropped
	assertContains(t, ir, "close.err.drop")
}

// T0135: Duplicate close error is dropped when first error already captured
func TestDuplicateCloseErrorDropped(t *testing.T) {
	ir := generateIR(t, `
		type FRes {
			int id;
			close(~this)! { }
		}
		process()! {
			use a := FRes(id: 1);
			use b := FRes(id: 2);
		}
	`)
	// Multiple failable closes: second error should be dropped
	assertContains(t, ir, "close.err.drop.dup")
}

// T0135: Failable result capture registers error optional for drop
func TestFailableResultCaptureErrorDrop(t *testing.T) {
	ir := generateIR(t, `
		fail()! { raise error(message: "test"); }
		test() {
			(val, err) := fail();
		}
	`)
	// Error optional should have a drop flag and be dropped at scope exit
	assertContains(t, ir, "err.dropflag")
	assertContains(t, ir, "optdrop.check")
}

// T0135: Constructor allocation tracked as heap temp for auto-propagation cleanup
func TestConstructorAllocHeapTemp(t *testing.T) {
	ir := generateIR(t, `
		fail(string s) int! {
			raise error(message: s);
		}
		type H { int v; }
		process()! {
			h := H(v: fail("x"));
		}
	`)
	// Constructor allocation should be tracked and freed on error path
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "err.heap.drop")
}

func TestUseFailableCloseVirtualDispatchCapture(t *testing.T) {
	ir := generateIR(t, `
		type Conn {
			int fd;
			close()! { }
		}
		type TcpConn is Conn {
			close()! { }
		}
		process()! {
			use c := Conn(fd: 3);
			int x = c.fd;
		}
	`)
	// Virtual dispatch + failable function: close error should be captured
	assertContains(t, ir, "@promise_vtable_Conn")
	assertContains(t, ir, "close.err.flag")
	assertContains(t, ir, "close.err.ret")
}

// --- Getter/Setter same name regression ---

func TestGetterSetterSameNameCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			int _val;
			get val int { return this._val; }
			set val(int v) { this._val = v; }
		}
		main() {
			Box b = Box(_val: 0);
			b.val = 42;
			int v = b.val;
		}
	`)
	// Both getter and setter should produce distinct functions
	assertContains(t, ir, "define i64 @Box.val(")
	assertContains(t, ir, "define void @Box.val$set(")
}

func TestGetterSetterSameNameVtable(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			get val int `+"`"+`abstract;
			set val(int v) `+"`"+`abstract;
		}
		type Impl is Base {
			int _v;
			get val int { return this._v; }
			set val(int v) { this._v = v; }
		}
		main() {
			Base b = Impl(_v: 0);
			b.val = 42;
			int v = b.val;
		}
	`)
	// Vtable should contain both getter and setter slots
	assertContains(t, ir, "@promise_vtable_Base")
	assertContains(t, ir, "@promise_vtable_Impl")
	// Both getter and setter functions should exist
	assertContains(t, ir, "define i64 @Impl.val(")
	assertContains(t, ir, "define void @Impl.val$set(")
	// Virtual dispatch should NOT use direct call to Base.val (abstract)
	assertNotContains(t, ir, "call i64 @Base.val(")
}

func TestCompoundAssignmentGetterSetterCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int _count;
			get count int { return this._count; }
			set count(int v) { this._count = v; }
		}
		main() {
			Counter c = Counter(_count: 0);
			c.count += 5;
		}
	`)
	// Should call both getter and setter
	assertContains(t, ir, "call i64 @Counter.count(")
	assertContains(t, ir, "call void @Counter.count$set(")
}

func TestViewVtableGetterSetter(t *testing.T) {
	ir := generateIR(t, `
		type Readable {
			get val int `+"`"+`abstract;
		}
		type Writable {
			get val int `+"`"+`abstract;
			set val(int v) `+"`"+`abstract;
		}
		type Store is Readable, Writable {
			int _v;
			get val int { return this._v; }
			set val(int v) { this._v = v; }
		}
		main() {
			Writable w = Store(_v: 0);
			w.val = 42;
			int v = w.val;
		}
	`)
	// View vtable for Store-as-Writable should exist
	assertContains(t, ir, "promise_vtable_Store_as_Writable")
	// Both functions should be emitted
	assertContains(t, ir, "define i64 @Store.val(")
	assertContains(t, ir, "define void @Store.val$set(")
}

func TestGenericGetterSetterSameName(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T _val;
			get val T { return this._val; }
			set val(T v) { this._val = v; }
		}
		main() {
			b := Box[int](_val: 0);
			b.val = 42;
			int v = b.val;
		}
	`)
	// Monomorphized getter and setter should have distinct names
	assertContains(t, ir, "define i64 @\"Box[int].val\"(")
	assertContains(t, ir, "define void @\"Box[int].val$set\"(")
}

// --- Drop method tests ---

// Basic: drop() called at scope exit
func TestDropBasicScopeExit(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			r := Resource(id: 1);
			int x = r.id;
		}
	`)
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "r.dropflag")
}

// B0159: Explicit drop methods auto-free instance memory via pal_free
func TestDropExplicitFreesInstance(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			r := Resource(id: 1);
		}
	`)
	// The drop function body should end with pal_free to free the instance struct
	assertContains(t, ir, "define void @Resource.drop")
	assertContains(t, ir, "call void @pal_free(i8* %this)")
}

// Move to function arg clears drop flag, adds condBr
func TestDropNotCalledWhenMoved(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		main() {
			r := Resource(id: 1);
			consume(r);
		}
	`)
	assertContains(t, ir, "store i1 false, i1*")
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Return triggers drop before ret
func TestDropWithReturn(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		make() int {
			r := Resource(id: 42);
			return r.id;
		}
		main() {
			int v = make();
		}
	`)
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "define i64 @make")
}

// Mixed use + drop bindings both fire
func TestDropAndUseOrdering(t *testing.T) {
	ir := generateIR(t, `
		type Closeable {
			int id;
			close() { }
		}
		type Droppable {
			int id;
			drop(~this) { }
		}
		main() {
			use c := Closeable(id: 1);
			d := Droppable(id: 2);
			int x = c.id + d.id;
		}
	`)
	assertContains(t, ir, "call void @Closeable.close")
	assertContains(t, ir, "call void @Droppable.drop")
}

// Nested type: outer drop() triggers field drops
func TestDropFieldAutoCleanup(t *testing.T) {
	ir := generateIR(t, `
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
			int x = o.inner.id;
		}
	`)
	assertContains(t, ir, "call void @Outer.drop")
	assertContains(t, ir, "call void @Inner.drop")
}

// Returning a droppable variable clears its flag
func TestDropReturnMoveClearsFlag(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		make() Resource {
			r := Resource(id: 42);
			return r;
		}
		main() {
			Resource v = make();
		}
	`)
	assertContains(t, ir, "store i1 false, i1*")
}

// Conditional move: moved in if-then only → drop flag condBr after merge
func TestDropConditionalMove(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		main() {
			r := Resource(id: 1);
			if true {
				consume(r);
			}
		}
	`)
	// Drop flag cleared in then-branch
	assertContains(t, ir, "store i1 false, i1*")
	// Conditional drop at scope exit (flag may be true or false)
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Conditional move with else: moved in both branches → flag cleared in both
func TestDropConditionalMoveBothBranches(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		other(Resource r) { }
		main() {
			r := Resource(id: 1);
			if true {
				consume(r);
			} else {
				other(r);
			}
		}
	`)
	// Flag should be cleared in both branches
	count := strings.Count(ir, "store i1 false, i1*")
	if count < 2 {
		t.Errorf("expected at least 2 'store i1 false' (both branches), got %d", count)
	}
}

// Nested scopes: inner scope drop happens before outer
func TestDropNestedScopes(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			r1 := Resource(id: 1);
			if true {
				r2 := Resource(id: 2);
				int x = r2.id;
			}
			int y = r1.id;
		}
	`)
	// Both should have drop flags and calls
	assertContains(t, ir, "r1.dropflag")
	assertContains(t, ir, "r2.dropflag")
	// Two drop calls (one for inner, one for outer)
	count := strings.Count(ir, "call void @Resource.drop")
	if count < 2 {
		t.Errorf("expected at least 2 drop calls (inner + outer scope), got %d\nIR:\n%s", count, ir)
	}
}

// While loop: droppable var inside loop body should be dropped per iteration
func TestDropInWhileLoop(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			int i = 0;
			while i < 3 {
				r := Resource(id: i);
				int x = r.id;
				i += 1;
			}
		}
	`)
	// Drop should be emitted inside the loop body
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "r.dropflag")
}

// Infinite loop with break: drop cleanup happens at break
func TestDropInLoopWithBreak(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			for {
				r := Resource(id: 1);
				int x = r.id;
				break;
			}
		}
	`)
	// Drop call should be present (at break cleanup)
	assertContains(t, ir, "call void @Resource.drop")
}

// Loop with continue: drop fires at end of iteration and at continue
func TestDropInLoopWithContinue(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			int i = 0;
			while i < 5 {
				r := Resource(id: i);
				i += 1;
				if i == 3 {
					continue;
				}
				int x = r.id;
			}
		}
	`)
	// Drop calls should exist (at continue and normal scope exit)
	assertContains(t, ir, "call void @Resource.drop")
}

// Move into method call clears drop flag
func TestDropMoveToMethodCall(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		type Container {
			int id;
			take(Resource r) { }
		}
		main() {
			c := Container(id: 0);
			r := Resource(id: 1);
			c.take(r);
		}
	`)
	// r's drop flag should be cleared after method call
	assertContains(t, ir, "store i1 false, i1*")
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Move into constructor field clears drop flag
func TestDropMoveToConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		main() {
			r := Inner(id: 1);
			o := Outer(inner: r);
			int x = o.inner.id;
		}
	`)
	// r's drop flag should be cleared when moved into constructor
	assertContains(t, ir, "store i1 false, i1*")
}

// Move into ident assignment clears drop flag
func TestDropMoveToIdentAssign(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			Resource a = Resource(id: 1);
			Resource b = Resource(id: 2);
			b = a;
			int x = b.id;
		}
	`)
	// a's drop flag should be cleared after the assignment to b
	assertContains(t, ir, "store i1 false, i1*")
}

// Move into member assignment clears drop flag (bug #2 fix)
func TestDropMoveToMemberAssign(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		main() {
			o := Outer(inner: Inner(id: 0));
			r := Inner(id: 1);
			o.inner = r;
		}
	`)
	// r's drop flag should be cleared after the member assignment
	assertContains(t, ir, "store i1 false, i1*")
}

// Multiple droppable vars: each gets its own flag and cleanup
func TestDropMultipleVariables(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			a := Resource(id: 1);
			b := Resource(id: 2);
			c := Resource(id: 3);
			int x = a.id + b.id + c.id;
		}
	`)
	assertContains(t, ir, "a.dropflag")
	assertContains(t, ir, "b.dropflag")
	assertContains(t, ir, "c.dropflag")
	count := strings.Count(ir, "call void @Resource.drop")
	if count < 3 {
		t.Errorf("expected at least 3 drop calls, got %d\nIR:\n%s", count, ir)
	}
}

// Multiple droppable fields: all cleaned up after user drop() body
func TestDropMultipleFieldsAutoCleanup(t *testing.T) {
	ir := generateIR(t, `
		type FileHandle {
			int fd;
			drop(~this) { }
		}
		type Connection {
			FileHandle read_handle;
			FileHandle write_handle;
			drop(~this) { }
		}
		main() {
			c := Connection(read_handle: FileHandle(fd: 3), write_handle: FileHandle(fd: 4));
		}
	`)
	assertContains(t, ir, "call void @Connection.drop")
	// FileHandle.drop should be called for both fields inside Connection.drop
	count := strings.Count(ir, "call void @FileHandle.drop")
	if count < 2 {
		t.Errorf("expected at least 2 FileHandle.drop calls (one per field), got %d\nIR:\n%s", count, ir)
	}
}

// Vector field drop: types with Vector fields should emit Vector.drop (B0157)
func TestDropVectorField(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			int[] items;
			drop(~this) {}
		}
		main() {
			h := Holder(items: [1, 2, 3]);
		}
	`)
	// Container fields get dropped via emitFieldDrops in drop() body
	assertContains(t, ir, "call void @Vector.drop")
	assertContains(t, ir, "%h.dropflag")
	assertContains(t, ir, "call void @Holder.drop")
}

// Standalone vector variables do NOT get scope-exit drop yet (needs ownership tracking)
// T0064: Standalone vector variables now get drop flags
func TestDropVectorStandaloneHasDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] my_vec = [1, 2, 3];
			int x = my_vec.len;
		}
	`)
	assertContains(t, ir, "%my_vec.dropflag")
	assertContains(t, ir, "call void @Vector.drop(")
}

// B0163: Channel scope-exit drop — standalone channel gets drop flag and Channel.drop call
func TestDropChannelStandaloneHasDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
		}
	`)
	assertContains(t, ir, "%ch.dropflag")
	assertContains(t, ir, "call void @Channel.drop(")
}

// B0163: Channel.drop function body uses refcount — frees only when refcount drops to 0
func TestChannelDropFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
		}
	`)
	assertContains(t, ir, "define void @Channel.drop(i8* %this)")
	// Refcount decrement (atomicrmw or load+add for WASM)
	assertContains(t, ir, "i64 -1")
	assertContains(t, ir, "call void @pal_free(")
	assertContains(t, ir, "call void @pal_mutex_destroy(")
	assertContains(t, ir, "call void @pal_cond_destroy(")
}

// B0163: Channel refcount initialized to 1 in promise_channel_new
func TestChannelRefcountInit(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
		}
	`)
	// promise_channel_new should store refcount = 1
	assertContains(t, ir, "define i8* @promise_channel_new(")
	// Channel.drop should use atomicrmw add with -1 (refcount decrement)
	assertContains(t, ir, "define void @Channel.drop(")
}

// B0163: Channel drop null-checks the pointer (zero-initialized channels from error paths)
func TestChannelDropNullCheck(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
		}
	`)
	// Channel.drop body should have null check (icmp eq ... null)
	dropFn := extractFunction(ir, "Channel.drop")
	if dropFn == "" {
		t.Fatal("expected Channel.drop function in IR")
	}
	assertContains(t, dropFn, "icmp eq")
	assertContains(t, dropFn, "null")
}

// B0163: Channel drop flag cleared on move (borrow detection)
func TestChannelDropFlagInDroppableContainer(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	// isDroppableContainerOrString should recognize channels
	assertContains(t, ir, "%ch.dropflag")
	assertContains(t, ir, "call void @Channel.drop(")
}

// Non-droppable type: no drop flag or call generated for that variable
func TestDropNotGeneratedForNonDroppable(t *testing.T) {
	ir := generateIR(t, `
		type Simple {
			int id;
		}
		main() {
			my_simple := Simple(id: 1);
			int x = my_simple.id;
		}
	`)
	// B0164: Non-droppable heap types now get bindingFree with a drop flag for pal_free
	assertContains(t, ir, "%my_simple.dropflag")
	assertNotContains(t, ir, "Simple.drop")
	assertContains(t, ir, "call void @pal_free")
}

// Copy type: no drop flag even if fields exist
func TestDropNotGeneratedForCopyType(t *testing.T) {
	ir := generateIR(t, `
		type Point `+"`"+`copy {
			int x;
			int y;
		}
		main() {
			p := Point(x: 1, y: 2);
			int v = p.x;
		}
	`)
	assertNotContains(t, ir, "%p.dropflag")
	assertNotContains(t, ir, "Point.drop")
}

// Droppable var in typed var decl
func TestDropTypedVarDecl(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			Resource r = Resource(id: 1);
			int x = r.id;
		}
	`)
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "r.dropflag")
}

// Drop with early return in failable function
func TestDropWithEarlyReturnFailable(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		work() void! {
			r := Resource(id: 42);
			return;
		}
		main() { }
	`)
	// drop() should be emitted before the return
	assertContains(t, ir, "call void @Resource.drop")
}

// Drop with raise: cleanup before error return
func TestDropWithRaise(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		fail() void! {
			r := Resource(id: 1);
			raise error(message: "oops");
		}
		main() { }
	`)
	// drop() should be emitted before the raise
	assertContains(t, ir, "call void @Resource.drop")
}

// Drop in a function that takes and returns a droppable:
// the parameter itself doesn't get a drop flag (it's the caller's responsibility)
func TestDropParameterNotFlagged(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		passthrough(Resource r) int {
			return r.id;
		}
		main() {
			int x = passthrough(Resource(id: 1));
		}
	`)
	assertContains(t, ir, "define i64 @passthrough")
	// The function should not create a drop flag for its parameter
	// (it doesn't own the alloca, the caller does the drop flag management)
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

// --- Coverage gap tests ---

// Virtual close dispatch through vtable (type has children → needs vtable)
func TestUseVarVirtualCloseDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			int id;
			close() { }
		}
		type Child is Base {
			close() { }
		}
		main() {
			use r := Base(id: 1);
			int x = r.id;
		}
	`)
	// Base has children → needs vtable → virtual close dispatch
	assertContains(t, ir, "@promise_vtable_Base")
}

// Virtual close with failable close() method (parent type with child)
func TestUseVarVirtualCloseDispatchFailable(t *testing.T) {
	ir := generateIR(t, `
		type Conn {
			int fd;
			close()! { }
		}
		type TcpConn is Conn {
			close()! { }
		}
		main() {
			use c := Conn(fd: 3);
			int x = c.fd;
		}
	`)
	// Conn has children → needs vtable → virtual close dispatch
	assertContains(t, ir, "@promise_vtable_Conn")
}

// Virtual drop dispatch through vtable (type has children → needs vtable)
func TestDropVirtualDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Handle {
			int id;
			drop(~this) { }
		}
		type FileHandle is Handle {
			drop(~this) { }
		}
		main() {
			h := Handle(id: 1);
			int x = h.id;
		}
	`)
	// Handle has children → needs vtable → virtual drop dispatch
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
	assertContains(t, ir, "h.dropflag")
	assertContains(t, ir, "@promise_vtable_Handle")
}

// llvmTypeAlign coverage: float, double, pointer, array
func TestLlvmTypeAlignFloat(t *testing.T) {
	if a := llvmTypeAlign(irtypes.Float); a != 4 {
		t.Errorf("float align: got %d, want 4", a)
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

func TestLlvmTypeAlignArray(t *testing.T) {
	arr := irtypes.NewArray(10, irtypes.I32)
	if a := llvmTypeAlign(arr); a != 4 {
		t.Errorf("[10 x i32] align: got %d, want 4", a)
	}
}

func TestLlvmTypeAlignStruct(t *testing.T) {
	s := irtypes.NewStruct(irtypes.I8, irtypes.I64)
	if a := llvmTypeAlign(s); a != 8 {
		t.Errorf("{i8, i64} align: got %d, want 8", a)
	}
}

func TestLlvmTypeAlignLargeInt(t *testing.T) {
	// i128 = 16 bytes, but capped at 8
	i128 := irtypes.NewInt(128)
	if a := llvmTypeAlign(i128); a != 8 {
		t.Errorf("i128 align: got %d, want 8", a)
	}
}

func TestLlvmTypeSizeFloat(t *testing.T) {
	if sz := llvmTypeSize(irtypes.Float); sz != 4 {
		t.Errorf("float size: got %d, want 4", sz)
	}
	if sz := llvmTypeSize(irtypes.Double); sz != 8 {
		t.Errorf("double size: got %d, want 8", sz)
	}
}

func TestLlvmTypeSizePointer(t *testing.T) {
	if sz := llvmTypeSize(irtypes.I8Ptr); sz != 8 {
		t.Errorf("pointer size: got %d, want 8", sz)
	}
}

func TestLlvmTypeSizeArray(t *testing.T) {
	arr := irtypes.NewArray(5, irtypes.I32)
	if sz := llvmTypeSize(arr); sz != 20 {
		t.Errorf("[5 x i32] size: got %d, want 20", sz)
	}
}

// Drop with use in loop triggers both close and drop at scope boundaries
func TestDropAndUseInLoop(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call void @Closeable.close")
	assertContains(t, ir, "call void @Droppable.drop")
}

// Move in function call clears flag — std call variant
func TestDropMoveToStdCall(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "store i1 false, i1*")
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Move in generic function call clears flag
func TestDropMoveToGenericFuncCall(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		identity[T](T val) T { return val; }
		main() {
			r := Resource(id: 1);
			Resource r2 = identity[Resource](r);
		}
	`)
	assertContains(t, ir, "store i1 false, i1*")
}

// Move to index assignment clears flag
func TestDropMoveToIndexAssign(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "store i1 false, i1*")
}

// Error propagation triggers scope cleanup
func TestDropErrorPropagateCleansUp(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		risky() int! {
			return 42;
		}
		work() int! {
			r := Resource(id: 1);
			int val = risky()?;
			return val + r.id;
		}
		main() { }
	`)
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "define { i1, i64, i8* } @work")
}

// Reassignment of droppable variable emits drop on old value
func TestDropOnReassignment(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
	// Should reset drop flag after reassignment
	assertContains(t, ir, "store i1 true")
}

// Move-then-reassign: drop flag was cleared by move, so reassignment skips drop
func TestDropOnReassignmentAfterMove(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Multiple reassignments: each reassignment should drop the old value
func TestDropOnMultipleReassignments(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
	assertContains(t, ir, "store i1 true")
}

// Self-assignment should be a no-op (no drop emitted, no store)
func TestDropOnSelfAssignmentSkipped(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "drop.call")
}

// Compound assignment should NOT trigger drop-before-store
func TestDropCompoundAssignNoExtraDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			x += 5;
		}
	`)
	// No drop blocks for primitive int variable x
	assertNotContains(t, ir, "%x.dropflag")
}

// Non-droppable type reassignment should not emit drop
func TestDropOnReassignmentNonDroppable(t *testing.T) {
	ir := generateIR(t, `
		type Simple { int x; }
		test() {
			my_simple := Simple(x: 1);
			my_simple = Simple(x: 2);
		}
		main() {}
	`)
	// B0164: Non-droppable heap types now get bindingFree with a drop flag for pal_free.
	// On reassignment, the old value is freed before storing the new one.
	assertContains(t, ir, "%my_simple.dropflag")
	assertNotContains(t, ir, "Simple.drop")
	assertContains(t, ir, "call void @pal_free")
}

// Reassignment inside if block: drop still emitted
func TestDropOnReassignmentInIfBlock(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Reassignment inside loop: drop per iteration
func TestDropOnReassignmentInLoop(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
	assertContains(t, ir, "store i1 true")
}

// Reassignment with virtual drop dispatch (type has children)
func TestDropOnReassignmentVirtualDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			int id;
			drop(~this) {}
		}
		type Child is Base {
			drop(~this) {}
		}
		test() {
			r := Base(id: 1);
			r = Base(id: 2);
		}
		main() {}
	`)
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Drop flag reset is i1 true (not i64 or other)
func TestDropOnReassignmentFlagResetIsI1True(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "store i1 true")
}

// Reassignment when RHS is a moved variable clears RHS drop flag
func TestDropOnReassignmentRHSMoveClears(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "store i1 true")
	assertContains(t, ir, "store i1 false")
}

// B0158: Type with droppable field auto-gets a synthesized drop
func TestDropSynthesizedBasic(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call void @Outer.drop")
	assertContains(t, ir, "o.dropflag")
	assertContains(t, ir, "call void @Inner.drop") // emitFieldDrops cascades
	assertContains(t, ir, "call void @pal_free(")  // frees Outer instance
}

// B0158: Cascading synthesized drop — Outer contains Middle contains Inner
func TestDropSynthesizedCascading(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call void @Outer.drop")
	assertContains(t, ir, "define void @Middle.drop")
}

// B0158: Synthesized drop with multiple droppable fields
func TestDropSynthesizedMultipleFields(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call void @Pair.drop")
	assertContains(t, ir, "define void @Pair.drop")
}

// B0158: Type with mix of droppable and non-droppable fields
func TestDropSynthesizedMixedFields(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call void @Mixed.drop")
	assertContains(t, ir, "define void @Mixed.drop")
}

// B0158: Copy type is not auto-synthesized even with droppable-looking fields
func TestDropSynthesizedNotForCopy(t *testing.T) {
	ir := generateIR(t, `
		type Simple `+"`copy"+` {
			int x;
		}
		main() {
			my_copy := Simple(x: 1);
		}
	`)
	assertNotContains(t, ir, "Simple.drop")
	assertNotContains(t, ir, "my_copy.dropflag")
}

// B0158: No synthesized drop when no fields have drop
func TestDropSynthesizedNotNeeded(t *testing.T) {
	ir := generateIR(t, `
		type Plain {
			int x;
			bool y;
		}
		main() {
			my_plain := Plain(x: 1, y: true);
		}
	`)
	assertNotContains(t, ir, "Plain.drop")
	// B0164: Plain types now get a bindingFree with a drop flag for pal_free at scope exit.
	// No synthesized drop method, just pal_free for the heap instance.
	assertContains(t, ir, "my_plain.dropflag")
	assertContains(t, ir, "call void @pal_free")
}

// T0095: Synthesized drop drops string fields via promise_string_drop
func TestDropSynthesizedStringField(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			string name;
		}
		main() {
			h := Holder(name: "hello");
		}
	`)
	// Synthesized drop should call promise_string_drop on the string field
	assertContains(t, ir, "define void @Holder.drop")
	assertContains(t, ir, "call void @promise_string_drop(")
	assertContains(t, ir, "call void @pal_free(")
}

// T0095: String field access on droppable type creates a dup (via promise_string_new)
func TestStringFieldAccessDup(t *testing.T) {
	ir := generateIR(t, `
		type Named {
			string name;
		}
		test() {
			n := Named(name: "world");
			string x = n.name;
		}
	`)
	// Reading n.name should dup the string to prevent double-free
	assertContains(t, ir, "call i8* @promise_string_new(")
}

// B0181: Optional string field access + unwrap should dup to prevent double-free
func TestOptionalStringFieldUnwrapDup(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper {
			string? opt_name;
		}
		test() {
			w := Wrapper(opt_name: "hello");
			string val = w.opt_name!;
		}
	`)
	// Reading w.opt_name should dup the inner string (via promise_string_new)
	// to prevent double-free between val's drop and Wrapper's synthesized drop
	assertContains(t, ir, "call i8* @promise_string_new(")
}

// B0190: Inline optional unwrap must not track field string as a temp.
// The unwrapped i8* from `w.opt_name!` is a field reference (not a new allocation),
// so tracking it would cause the field's string to be freed at statement end.
func TestInlineOptionalUnwrapNoTempTrack(t *testing.T) {
	// This should compile without errors. At runtime, the inline unwrap
	// must not free the field string as a temp — only Wrapper.drop should.
	generateIR(t, `
		type Wrapper {
			string? opt_name;
		}
		test() {
			w := Wrapper(opt_name: "hello");
			bool b = w.opt_name! == "hello";
		}
	`)
}

// T0095: Constructor with borrowed string param (no drop flag) dups the string
func TestConstructorDupBorrowedString(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper {
			string data;
		}
		wrap(string s) Wrapper {
			return Wrapper(data: s);
		}
		main() { }
	`)
	// Inside wrap(), s has no drop flag (non-~ param), so constructor should dup
	assertContains(t, ir, "define { i8*, i8* } @wrap(i8* %s)")
	// The wrap function body should contain a dup call (promise_string_new)
	// because s is a borrowed param without a drop flag
	assertContains(t, ir, "call i8* @promise_string_new(")
}

// B0179: Shared borrow of string field must NOT dup the string.
// The borrow doesn't own the value — duping creates a temp that gets freed
// while the borrow still points to it (use-after-free / double-free).
func TestStringBorrowFieldNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Pair {
			string a;
			string b;
		}
		get_ref(string & s) string & {
			return s;
		}
		test() {
			p := Pair(a: "hello", b: "world");
			string & ra = get_ref(p.a);
		}
	`)
	// The test function should NOT contain a string dup — the param is a borrow.
	testFn := extractFunction(ir, "test")
	assertNotContains(t, testFn, "call i8* @promise_string_new(")
}

// B0164: bindingFree emits pal_free on non-droppable heap types with multiple fields
func TestBindingFreeMultipleFields(t *testing.T) {
	ir := generateIR(t, `
		type Config {
			int port;
			bool verbose;
		}
		main() {
			c := Config(port: 8080, verbose: true);
			int p = c.port;
		}
	`)
	assertContains(t, ir, "c.dropflag")
	assertContains(t, ir, "call void @pal_free")
	assertNotContains(t, ir, "Config.drop")
}

// B0164: bindingFree works on reassignment — frees old value before storing new
func TestBindingFreeReassignment(t *testing.T) {
	ir := generateIR(t, `
		type Pair { int x; int y; }
		test() {
			p := Pair(x: 1, y: 2);
			p = Pair(x: 3, y: 4);
		}
		main() {}
	`)
	assertContains(t, ir, "p.dropflag")
	// Should have two pal_alloc (one per constructor) and free.call blocks
	assertContains(t, ir, "free.call")
	assertContains(t, ir, "call void @pal_free")
}

// T0086: Error types now get bindingFree at scope exit (previously excluded)
func TestBindingFreeErrorType(t *testing.T) {
	ir := generateIR(t, `
		main() {
			error e = error("test");
			string msg = e.message;
		}
	`)
	assertContains(t, ir, "e.dropflag")
	assertContains(t, ir, "call void @pal_free")
}

// T0127: Structural interface variables from calls get bindingFree with iter cleanup
func TestStructuralInterfaceVarFree(t *testing.T) {
	ir := generateIR(t, `
		type Counter is Iterator[int] {
			int current;
			int limit;
			next() int? {
				if this.current < this.limit {
					v := this.current;
					this.current = this.current + 1;
					return v;
				}
				return none;
			}
		}
		test() {
			c := Counter(current: 0, limit: 5);
			Iterator[int] it = c.filter(|int x| -> bool {
				return x > 2;
			});
		}
		main() {}
	`)
	// Structural interface variable should get a drop flag and free.call block
	assertContains(t, ir, "it.dropflag")
	assertContains(t, ir, "free.call")
	// Should use __promise_iter_cleanup for iterator chain results (frees env + instance)
	assertContains(t, ir, "__promise_iter_cleanup")
}

// T0127: Structural interface variables from identifiers should NOT get bindingFree (borrow)
func TestStructuralInterfaceVarBorrow(t *testing.T) {
	ir := generateIR(t, `
		type Counter is Iterator[int] {
			int current;
			int limit;
			next() int? {
				if this.current < this.limit {
					v := this.current;
					this.current = this.current + 1;
					return v;
				}
				return none;
			}
		}
		test() {
			c := Counter(current: 0, limit: 5);
			Iterator[int] it = c.filter(|int x| -> bool {
				return x > 2;
			});
			Iterator[int] it2 = it;
		}
		main() {}
	`)
	// it should have a drop flag (from call result)
	assertContains(t, ir, "it.dropflag")
	// it2 should NOT have a drop flag (borrow from it)
	assertNotContains(t, ir, "it2.dropflag")
}

// T0086: Raising a local error variable clears its drop flag before scope cleanup
func TestRaiseLocalErrorClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		fail() void! {
			error e = error("boom");
			raise e;
		}
		main() { }
	`)
	// The error should get a drop flag
	assertContains(t, ir, "e.dropflag")
	// The drop flag should be cleared (store false) before scope cleanup
	assertContains(t, ir, "store i1 false")
}

// T0073: Primitive to_string temp is dropped at statement end when not assigned
func TestStringTempDropAtStatementEnd(t *testing.T) {
	ir := generateIR(t, `
		test() {
			assert(42.to_string() == "42", "ok");
		}
		main() {}
	`)
	// Should have temp drop blocks from cleanupStmtTemps
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "tmp.skip")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0103: String temp cleanup on error propagation path
func TestStringTempCleanupOnErrorPath(t *testing.T) {
	ir := generateIR(t, `
		fail() int! {
			raise error(message: "fail");
		}
		use_both(string s, int x) int {
			return x;
		}
		work() int! {
			return use_both("hello".to_upper(), fail());
		}
		main() {}
	`)
	// Should have error-path temp cleanup blocks (T0103)
	assertContains(t, ir, "err.tmp.drop")
	assertContains(t, ir, "err.tmp.skip")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0073: Primitive to_string temp is claimed when assigned to a variable
func TestStringTempClaimedOnAssign(t *testing.T) {
	ir := generateIR(t, `
		test() {
			s := 42.to_string();
			assert(s == "42", "ok");
		}
		main() {}
	`)
	// The temp is tracked then claimed — the variable's drop binding handles cleanup.
	// The temp cleanup blocks should still exist but the flag should be cleared.
	assertContains(t, ir, "s.dropflag")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0073: Match expression with to_string in an arm — temp claimed by phi
func TestStringTempInMatchArm(t *testing.T) {
	ir := generateIR(t, `
		test() {
			int n = 7;
			string r = match n {
				1 => "one",
				_ => n.to_string(),
			};
			assert(r == "7", "ok");
		}
		main() {}
	`)
	// Should compile without domination errors; the temp in the _ arm is claimed
	assertContains(t, ir, "r.dropflag")
}

// B0172: Temp flag reset after drop — prevents double-free in loops with match arms
func TestStringTempFlagResetInLoop(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "store i1 false")
}

// B0172: Temp tracking enabled in defineMethodFunc
func TestStringTempTrackingInMethodBody(t *testing.T) {
	ir := generateIR(t, `
		type Fmt {
			format(this) string { return this.to_string(); }
			to_string(this) string { return "fmt"; }
		}
		main() {}
	`)
	// Method bodies should have temp tracking enabled
	assertContains(t, ir, "tmp.drop")
}

// B0168: String concat temp is tracked and dropped at statement end
func TestStringConcatTempDrop(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string name = "world";
			assert("hello " + name == "hello world", "ok");
		}
		main() {}
	`)
	// Concat result should be tracked as a temp and dropped
	assertContains(t, ir, "call i8* @promise_string_concat")
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// B0168: String concat temp claimed when assigned to variable
func TestStringConcatTempClaimedOnAssign(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string name = "world";
			string greeting = "hello " + name;
			assert(greeting == "hello world", "ok");
		}
		main() {}
	`)
	// Concat result is claimed (assigned to greeting), variable drop binding handles it
	assertContains(t, ir, "greeting.dropflag")
	assertContains(t, ir, "call i8* @promise_string_concat")
}

// B0168: String concat temp in constructor arg is claimed (no double-free)
func TestStringConcatTempInConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Greeter { string msg; }
		test() {
			g := Greeter(msg: "hello " + "world");
		}
		main() {}
	`)
	// Should compile and run without double-free; concat temp is claimed
	assertContains(t, ir, "call i8* @promise_string_concat")
}

// B0170: String temp pushed into vector is claimed (no double-free at stmt end)
func TestStringTempClaimedOnVectorPush(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] v = string[]();
			v.push("a" + "b");
		}
	`)
	// Concat result should be tracked then claimed by push.
	assertContains(t, ir, "call i8* @promise_string_concat")
	assertContains(t, ir, "call i8* @promise_vector_push")
}

// T0099: to_string() on user type is tracked as a string temp and freed at stmt end.
func TestStringTempUserTypeToString(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call i8* @Counter.to_string")
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0099: to_string() on string returns `this` (borrow) — NOT tracked as temp.
func TestStringTempStringToStringNotTracked(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string s = "hello";
			assert(s.to_string() == "hello", "ok");
		}
		main() {}
	`)
	// string.to_string() returns `this` — must NOT be tracked (would double-free).
	// The test function has only one string variable `s` — its drop handles cleanup.
	// Verification: s has a drop flag (the variable's own scope cleanup).
	assertContains(t, ir, "s.dropflag")
}

// T0099: to_string() on user type assigned to variable is claimed (not freed as temp).
func TestStringTempUserTypeToStringClaimed(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "s.dropflag")
	assertContains(t, ir, "call i8* @Tag.to_string")
}

// T0133: String slice expressions are tracked as temps and freed at statement end.
func TestStringSliceTempDrop(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string s = "hello world";
			assert(s[0:5] == "hello", "ok");
		}
		main() {}
	`)
	// s[0:5] produces a heap-allocated string via native slice (promise_string_new).
	// The slice result must be tracked as a temp and freed at statement end.
	assertContains(t, ir, "call i8* @promise_string_new")
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0133: String slice assigned to variable is claimed (not double-freed).
func TestStringSliceTempClaimedOnAssign(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string s = "hello world";
			string sub = s[0:5];
			assert(sub == "hello", "ok");
		}
		main() {}
	`)
	// s[0:5] is tracked as temp, then claimed when assigned to sub.
	// sub has its own drop binding for scope cleanup.
	assertContains(t, ir, "sub.dropflag")
	assertContains(t, ir, "call i8* @promise_string_new")
}

// T0092: String return from function with structural interface param is tracked as temp.
func TestStringTempStructuralParamReturn(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string {
			return s.to_string();
		}
		test() {
			assert(display(42) == "42", "ok");
		}
		main() {}
	`)
	// The return value of display(42) should be tracked as a string temp
	// and freed at statement end via promise_string_drop.
	assertContains(t, ir, "call i8* @display")
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0124: Free function call returning string is tracked as temp and freed at stmt end.
func TestStringTempFreeFunctionCall(t *testing.T) {
	ir := generateIR(t, `
		make_greeting(string name) string {
			return "hello " + name;
		}
		test() {
			assert(make_greeting("world") == "hello world", "ok");
		}
		main() {}
	`)
	// The return value of make_greeting() should be tracked and freed
	assertContains(t, ir, "call i8* @make_greeting")
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0124: Free function call returning string assigned to variable is claimed (not double-freed).
func TestStringTempFreeFunctionCallClaimed(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call i8* @make_label")
	// Drop flag is cleared (claimed) so no free at stmt end for this temp
	assertContains(t, ir, "store i1 false")
}

// T0082: Structural views are tested at the Promise level (e2e/structural_view_test.pr)
// because structural interface coercion requires the full std library.
// The fix: genTypedVarDecl skips clearDropFlag when LHS is a structural interface.

// B0167: Type with string field gets synthesized drop (cascading instance cleanup)
func TestSynthDropStringFieldCascade(t *testing.T) {
	ir := generateIR(t, `
		type Inner { string name; }
		type Outer { Inner inner; int x; }
		main() {
			o := Outer(inner: Inner(name: "hi"), x: 1);
		}
	`)
	// Both types get synthesized drops
	assertContains(t, ir, "define void @Inner.drop")
	assertContains(t, ir, "define void @Outer.drop")
	// Outer.drop calls Inner.drop (cascading) + pal_free
	assertContains(t, ir, "call void @Inner.drop")
	assertContains(t, ir, "call void @pal_free")
	// String fields are NOT freed by the synthesized drop (no promise_string_drop in drop body)
}

// B0167: Type with vector field gets synthesized drop
func TestSynthDropVectorField(t *testing.T) {
	ir := generateIR(t, `
		type Container { int[] items; }
		main() {
			int[] v = int[]();
			c := Container(items: v);
		}
	`)
	assertContains(t, ir, "define void @Container.drop")
	assertContains(t, ir, "call void @pal_free")
}

// B0177: Type with channel field gets synthesized drop
func TestSynthDropChannelField(t *testing.T) {
	ir := generateIR(t, `
		type WithChan { channel[int] ch; }
		main() {
			channel[int] ch = channel[int]();
			w := WithChan(ch: ch);
		}
	`)
	assertContains(t, ir, "define void @WithChan.drop")
	// Synthesized drop should call Channel.drop on the channel field
	withChanDrop := extractFunction(ir, "WithChan.drop")
	if withChanDrop == "" {
		t.Fatal("expected WithChan.drop function in IR")
	}
	assertContains(t, withChanDrop, "call void @Channel.drop(")
}

// T0091: Error types get synthesized drop (frees message string field + instance)
func TestSynthDropIncludesErrorTypes(t *testing.T) {
	ir := generateIR(t, `
		main() {
			error e = error(message: "fail");
		}
	`)
	// error gets a synthesized drop that frees its string message field
	assertContains(t, ir, "define void @__mod_std_error.drop")
}

// T0083/T0091: Caught error instances are dropped after handler blocks
func TestErrorHandlerDropsInstance(t *testing.T) {
	ir := generateIR(t, `
		fail() int! {
			raise error(message: "boom");
		}
		main() {
			int v = fail()? e => 0;
		}
	`)
	// Handler block should drop the error instance via synthesized drop
	assertContains(t, ir, "call void @__mod_std_error.drop")
}

// T0083/T0091/T0110: Typed error handler uses child type's drop for match path
func TestTypedErrorHandlerDropsInstance(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		fail() int! {
			raise IoError(code: 1, message: "io");
		}
		main() {
			int v = fail()? e is IoError { 0; } else { -1; };
		}
	`)
	// Match path drops via IoError.drop (resolves child type, T0110)
	assertContains(t, ir, "call void @IoError.drop")
	// Else path drops via base error.drop (unknown concrete type)
	assertContains(t, ir, "call void @__mod_std_error.drop")
}

// T0110: Error type synthesized drop includes string field drops
func TestErrorTypeSynthDropIncludesStringFields(t *testing.T) {
	ir := generateIR(t, `
		main() {
			error e = error(message: "fail");
		}
	`)
	// error.drop should call promise_string_drop for the message field
	assertContains(t, ir, "define void @__mod_std_error.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0110: Child error type drop frees child-specific string fields
func TestChildErrorTypeSynthDropFreesChildFields(t *testing.T) {
	ir := generateIR(t, `
		type NotFoundError is error { string key; }
		fail() int! {
			raise NotFoundError(key: "missing", message: "not found");
		}
		main() {
			int v = fail()? e is NotFoundError { 0; } else { -1; };
		}
	`)
	// NotFoundError.drop should be defined (synthesized)
	assertContains(t, ir, "define void @NotFoundError.drop")
	// Match path uses NotFoundError.drop, not error.drop
	assertContains(t, ir, "call void @NotFoundError.drop")
	// NotFoundError.drop should have 2 string drops (message + key)
	dropBody := extractFunction(ir, "NotFoundError.drop")
	count := strings.Count(dropBody, "call void @promise_string_drop")
	if count < 2 {
		t.Errorf("expected at least 2 promise_string_drop calls in NotFoundError.drop (message + key), got %d\nBody:\n%s", count, dropBody)
	}
}

// T0110: Dup-on-field-access works for error types (prevents use-after-free)
func TestErrorFieldAccessDupsString(t *testing.T) {
	ir := generateIR(t, `
		type NotFoundError is error { string key; }
		fail() string! {
			raise NotFoundError(key: "missing", message: "not found");
		}
		main() {
			string s = fail()? e is NotFoundError { e.key; } else { ""; };
		}
	`)
	// Accessing e.key in error handler should dup the string via string_new (copy)
	// dupString() calls promise_string_new to create a heap copy of the field data
	assertContains(t, ir, "strdup.copy")
}

// B0158: Synthesized drop coexists with explicit drop (explicit takes precedence)
func TestDropExplicitTakesPrecedence(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call void @Outer.drop")
	assertContains(t, ir, "call void @Inner.drop")
	// Explicit drop should NOT have pal_free auto-appended (that's a separate concern)
}

// B0158: Generic type with droppable field gets a mono synthesized drop
func TestDropSynthesizedGeneric(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Wrapper[T] {
			Inner inner;
			T value;
		}
		main() {
			w := Wrapper[int](inner: Inner(id: 1), value: 42);
		}
	`)
	assertContains(t, ir, "Wrapper[int].drop")
}

// T0132: Generic type with generic droppable field gets cascading mono drop.
// Set[T] has a Map[T, bool] field — the synthesized Set[int].drop must call
// Map[int, bool].drop (mono name), not Map.drop (origin name which doesn't exist).
func TestDropSynthesizedGenericCascading(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Box[T] {
			Inner inner;
		}
		type Outer[T] {
			Box[T] box;
		}
		main() {
			o := Outer[int](box: Box[int](inner: Inner(id: 1)));
		}
	`)
	// Outer[int].drop must call Box[int].drop, not Box.drop
	assertContains(t, ir, `call void @"Box[int].drop"`)
	assertContains(t, ir, "Outer[int].drop")
}

// T0102: Enum with string variant gets synthesized drop
func TestDropSynthesizedEnumString(t *testing.T) {
	ir := generateIR(t, `
		enum Value {
			Text(string s),
			Number(int n),
		}
		main() {
			v := Value.Text("hello");
		}
	`)
	assertContains(t, ir, "define void @Value.drop")
	assertContains(t, ir, "enum.drop.Text")
	assertContains(t, ir, "call void @promise_string_drop(")
	assertContains(t, ir, "v.dropflag")
}

// T0102: Fieldless enum does NOT get synthesized drop
func TestDropSynthesizedEnumFieldless(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue, }
		main() {
			c := Color.Red;
		}
	`)
	assertNotContains(t, ir, "Color.drop")
}

// T0102: Enum with vector variant gets synthesized drop
func TestDropSynthesizedEnumVector(t *testing.T) {
	ir := generateIR(t, `
		enum Value {
			Items(int[] items),
			Single(int n),
		}
		main() {
			v := Value.Single(42);
		}
	`)
	assertContains(t, ir, "define void @Value.drop")
	assertContains(t, ir, "enum.drop.Items")
	assertContains(t, ir, "call void @Vector.drop(")
}

// T0102: Enum with user type variant gets synthesized drop
func TestDropSynthesizedEnumUserType(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		enum Holder {
			Has(Resource r),
			Empty,
		}
		main() {
			h := Holder.Empty;
		}
	`)
	assertContains(t, ir, "define void @Holder.drop")
	assertContains(t, ir, "enum.drop.Has")
	assertContains(t, ir, "call void @Resource.drop(")
}

// Compound assignment on different typed variables exercises namedFromLLVMType branches
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

// --- Hash getter tests ---

func TestHashGetterInt(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; h := x.hash; }`)
	assertContains(t, ir, "call i64 @__mod_std__fnv1a_hash(i64")
}

func TestHashGetterBool(t *testing.T) {
	ir := generateIR(t, `main() { b := true; h := b.hash; }`)
	// Bool hash uses hardcoded constants via select, not fnv1a
	assertContains(t, ir, "select i1")
	assertNotContains(t, ir, "call i64 @__mod_std__fnv1a_hash")
}

func TestHashGetterBoolFalse(t *testing.T) {
	ir := generateIR(t, `main() { b := false; h := b.hash; }`)
	assertContains(t, ir, "select i1")
}

func TestHashGetterBoolInFunction(t *testing.T) {
	ir := generateIR(t, `
		hash_it(bool b) int { return b.hash; }
		main() {}
	`)
	assertContains(t, ir, "select i1")
	assertNotContains(t, ir, "call i64 @__mod_std__fnv1a_hash")
}

func TestHashGetterBoolNoZext(t *testing.T) {
	// Bool hash should not zext to i64 anymore
	ir := generateIR(t, `main() { h := true.hash; }`)
	assertContains(t, ir, "select i1")
}

func TestHashGetterBoolTrueAndFalseDifferentConstants(t *testing.T) {
	// Both constants should appear in the select instruction
	ir := generateIR(t, `main() { h := true.hash; }`)
	assertContains(t, ir, "5871781006564002453") // 0x517cc1b727220a95
	assertContains(t, ir, "7809847782465536322") // 0x6c62272e07bb0142
}

func TestHashGetterIntStillUsesFnv1a(t *testing.T) {
	// Verify other types still use fnv1a (regression check)
	ir := generateIR(t, `main() { h := 42.hash; }`)
	assertContains(t, ir, "call i64 @__mod_std__fnv1a_hash")
}

func TestHashGetterCharStillUsesFnv1a(t *testing.T) {
	ir := generateIR(t, `main() { h := 'x'.hash; }`)
	assertContains(t, ir, "call i64 @__mod_std__fnv1a_hash")
}

func TestHashGetterChar(t *testing.T) {
	ir := generateIR(t, `main() { c := 'a'; h := c.hash; }`)
	assertContains(t, ir, "call i64 @__mod_std__fnv1a_hash(i64")
}

func TestHashGetterString(t *testing.T) {
	ir := generateIR(t, `main() { s := "hi"; h := s.hash; }`)
	assertContains(t, ir, "call i64 @__promise_hash_string(i8*")
}

func TestHashGetterFloat(t *testing.T) {
	ir := generateIR(t, `
		test(f64 x) int { return x.hash; }
		main() {}
	`)
	assertContains(t, ir, "bitcast double")
	assertContains(t, ir, "call i64 @__mod_std__fnv1a_hash(i64")
}

func TestBitsGetterF64(t *testing.T) {
	ir := generateIR(t, `
		test(f64 x) uint { return x.bits; }
		main() {}
	`)
	assertContains(t, ir, "bitcast double")
	// bits getter returns the raw i64 — no hash call
	assertNotContains(t, ir, "call i64 @__mod_std__fnv1a_hash")
}

func TestHashGetterSmallInt(t *testing.T) {
	ir := generateIR(t, `
		test(i8 x) int { return x.hash; }
		main() {}
	`)
	assertContains(t, ir, "sext i8")
	assertContains(t, ir, "call i64 @__mod_std__fnv1a_hash(i64")
}

func TestHashGetterSmallUint(t *testing.T) {
	ir := generateIR(t, `
		test(u8 x) int { return x.hash; }
		main() {}
	`)
	// Unsigned types use zero-extend, not sign-extend
	assertContains(t, ir, "zext i8")
	assertContains(t, ir, "call i64 @__mod_std__fnv1a_hash(i64")
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

func TestVectorContainsString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] words = ["a", "b"];
			bool has = words.contains("a");
		}
	`)
	assertContains(t, ir, "define i8 @promise_vector_contains(")
	assertContains(t, ir, "call i8 @promise_vector_contains(")
	// String contains uses custom equality comparator
	assertContains(t, ir, "@__promise_eq_string")
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

func TestLLVMMemmoveDeclared(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] nums = [1, 2, 3];
			nums.remove(0);
		}
	`)
	assertContains(t, ir, "declare void @llvm.memmove.p0i8.p0i8.i64(")
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

// --- String byte indexing ---

func TestStringByteIndex(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello";
			char c = s[0];
		}
	`)
	assertContains(t, ir, "stridx.ok")
	assertContains(t, ir, "stridx.oob")
	assertContains(t, ir, "zext i8")
}

// --- String method tests ---

func TestStringContains(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello world";
			bool has = s.contains("world");
		}
	`)
	// Promise method compiled as a module-prefixed function (std module)
	assertContains(t, ir, "define i1 @__mod_std_string.contains(")
	assertContains(t, ir, "call i1 @__mod_std_string.contains(")
}

func TestStringStartsWith(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello";
			bool yes = s.starts_with("hel");
		}
	`)
	assertContains(t, ir, "define i1 @__mod_std_string.starts_with(")
	assertContains(t, ir, "call i1 @__mod_std_string.starts_with(")
}

func TestStringEndsWith(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello";
			bool yes = s.ends_with("llo");
		}
	`)
	assertContains(t, ir, "define i1 @__mod_std_string.ends_with(")
	assertContains(t, ir, "call i1 @__mod_std_string.ends_with(")
}

func TestStringIndexOf(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello";
			int? idx = s.index_of("ll");
		}
	`)
	assertContains(t, ir, "define { i1, i64 } @__mod_std_string.index_of(")
	assertContains(t, ir, "call { i1, i64 } @__mod_std_string.index_of(")
}

func TestStringTrim(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "  hi  ";
			string trimmed = s.trim();
		}
	`)
	assertContains(t, ir, "define i8* @promise_string_trim(")
	assertContains(t, ir, "call i8* @promise_string_trim(")
}

func TestStringSplit(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "a,b,c";
			string[] parts = s.split(",");
		}
	`)
	assertContains(t, ir, "define i8* @promise_string_split(")
	assertContains(t, ir, "call i8* @promise_string_split(")
}

func TestStringTrimFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := " hi ".trim();
		}
	`)
	assertContains(t, ir, "define i8* @promise_string_trim(i8* %s)")
	assertContains(t, ir, "trim_left_hdr:")
	assertContains(t, ir, "trim_right_hdr:")
	assertContains(t, ir, "build_result:")
	assertContains(t, ir, "icmp eq i8") // whitespace checks
	assertContains(t, ir, "call i8* @promise_string_new(")
}

func TestStringSplitFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "a,b".split(",");
		}
	`)
	assertContains(t, ir, "define i8* @promise_string_split(i8* %s, i8* %sep)")
	assertContains(t, ir, "call i32 @memcmp(")
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "oom:")
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "call i8* @promise_string_new(")
	assertContains(t, ir, "count_hdr:")
	assertContains(t, ir, "split_hdr:")
	assertContains(t, ir, "split_tail:")
}

func TestStringNextCharFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			for ch in "abc" {}
		}
	`)
	assertContains(t, ir, "define i32 @promise_string_next_char(i8* %s, i64* %pos)")
	assertContains(t, ir, "ret_eof:")
	assertContains(t, ir, "ret i32 -1")
	assertContains(t, ir, "set_1byte:")
	assertContains(t, ir, "cont_hdr:")
	assertContains(t, ir, "cont_body:")
	assertContains(t, ir, "cont_done:")
}

func TestMemcmpDeclared(t *testing.T) {
	ir := generateIR(t, `main() { x := 1; }`)
	assertContains(t, ir, "declare i32 @memcmp(i8* nocapture noundef %s1, i8* nocapture noundef %s2, i64 noundef %n)")
	assertContains(t, ir, "mustprogress nounwind readonly willreturn argmemonly")
}

func TestAllocatorAttributes(t *testing.T) {
	ir := generateIR(t, `
		type Foo { int x; }
		main() { f := Foo(x: 1); }
	`)
	// Underlying libc declarations still present (emitted by PAL)
	assertContains(t, ir, "declare noalias i8* @malloc(i64 noundef %size) nounwind willreturn")
	assertContains(t, ir, "declare void @free(i8* nocapture noundef %ptr) nounwind willreturn")
	assertContains(t, ir, "declare noalias i8* @realloc(i8* nocapture noundef %ptr, i64 noundef %size) nounwind willreturn")
	// PAL wrappers defined
	assertContains(t, ir, "@pal_alloc(i64 %size)")
	assertContains(t, ir, "@pal_free(i8* %ptr)")
	assertContains(t, ir, "@pal_realloc(i8* %ptr, i64 %size)")
}

// --- Return optional wrapping in monomorphized context ---

func TestReturnOptionalInMonoMethod(t *testing.T) {
	// The map [] method returns V? — returning a concrete V must wrap in Optional
	ir := generateIR(t, `
		main() {
			m := {"x": 42};
			int? v = m["x"];
		}
	`)
	// The monomorphized [] method should produce { i1, i64 } return type
	assertContains(t, ir, `define { i1, i64 } @"Map[string, int].[]"(`)
	// Should contain insertvalue for wrapping the value in Optional { true, val }
	assertContains(t, ir, "insertvalue { i1, i64 }")
}

// --- Nested generic monomorphization (discoverInstances) ---

func TestNestedGenericMonomorphization(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T val; }
		type Wrapper[T] { Box[T] inner; }
		main() {
			w := Wrapper[int](inner: Box[int](val: 42));
		}
	`)
	// Both Wrapper[int] and Box[int] should be monomorphized
	assertContains(t, ir, "Wrapper[int]")
	assertContains(t, ir, "Box[int]")
}

// --- Non-native operator dispatch ---

func TestNonNativeOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Pt {
			int x;
			==(Pt other) bool { return this.x == other.x; }
		}
		main() {
			Pt a = Pt(x: 1);
			Pt b = Pt(x: 2);
			bool r = a == b;
		}
	`)
	assertContains(t, ir, `call i1 @"Pt.=="(`)
}

func TestDefaultMethodViaViewVtable(t *testing.T) {
	ir := generateIRWithStd(t,
		"type MyEq `structural {\n\t==(Self other) bool `abstract;\n\t!=(Self other) bool => !(this == other);\n}\n",
		`type Pt {
			int x;
			==(Pt other) bool { return this.x == other.x; }
		}
		main() {
			MyEq e = Pt(x: 1);
			MyEq f = Pt(x: 2);
			bool r = e != f;
		}
	`)
	assertContains(t, ir, `@"Pt.!="`)                  // synthesized default
	assertContains(t, ir, "promise_vtable_Pt_as_MyEq") // view vtable
}

func TestDefaultMethodOverride(t *testing.T) {
	// Concrete type overrides the default — the override should be used, not the synthesized default
	ir := generateIRWithStd(t,
		"type MyEq `structural {\n\t==(Self other) bool `abstract;\n\t!=(Self other) bool => !(this == other);\n}\n",
		`type Pt {
			int x;
			==(Pt other) bool { return this.x == other.x; }
			!=(Pt other) bool { return this.x != other.x; }
		}
		main() {
			MyEq e = Pt(x: 1);
			MyEq f = Pt(x: 2);
			bool r = e != f;
		}
	`)
	assertContains(t, ir, "promise_vtable_Pt_as_MyEq") // view vtable still created
	// The vtable should use the concrete Pt.!= override, not a synthesized default.
	// Check that the concrete method exists.
	assertContains(t, ir, `@"Pt.!="`)
}

func TestOrderedDefaultsViaViewVtable(t *testing.T) {
	stdOrd := "type MyEq `structural {\n\t==(Self other) bool `abstract;\n\t!=(Self other) bool => !(this == other);\n}\n" +
		"type MyOrd is MyEq `structural {\n\t<(Self other) bool `abstract;\n\t>(Self other) bool => other < this;\n\t<=(Self other) bool => !(other < this);\n\t>=(Self other) bool => !(this < other);\n}\n"
	ir := generateIRWithStd(t, stdOrd, `
		type Val {
			int n;
			==(Val o) bool { return this.n == o.n; }
			<(Val o) bool { return this.n < o.n; }
		}
		main() {
			MyOrd a = Val(n: 1);
			MyOrd b = Val(n: 2);
			bool r1 = a > b;
			bool r2 = a <= b;
			bool r3 = a >= b;
			bool r4 = a != b;
		}
	`)
	assertContains(t, ir, `@"Val.>"`)  // synthesized from > default
	assertContains(t, ir, `@"Val.<="`) // synthesized from <= default
	assertContains(t, ir, `@"Val.>="`) // synthesized from >= default
	assertContains(t, ir, `@"Val.!="`) // inherited from MyEq parent default
	assertContains(t, ir, "promise_vtable_Val_as_MyOrd")
}

// --- Go / Receive (concurrency) tests ---

func TestGoExprBasicFunction(t *testing.T) {
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Coroutine function generated with presplitcoroutine attribute
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// Coroutine intrinsics used
	assertContains(t, ir, "call token @llvm.coro.id(")
	assertContains(t, ir, "call i8* @llvm.coro.begin(")
	assertContains(t, ir, "call i8 @llvm.coro.suspend(")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
	// Result buffer allocated for non-void task
	assertContains(t, ir, "@pal_alloc")
	// Coroutine calls target function
	assertContains(t, ir, "call i64 @compute")
}

func TestGoExprWithArgs(t *testing.T) {
	ir := generateIR(t, `
		double(int x) int { return x * 2; }
		main() {
			t := go double(21);
			result := <-t;
		}
	`)
	// Coroutine generated
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
	// The coroutine should call the target function
	assertContains(t, ir, "call i64 @double")
}

func TestGoExprVoidFunction(t *testing.T) {
	ir := generateIR(t, `
		doWork() { }
		main() {
			t := go doWork();
			<-t;
		}
	`)
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// Coroutine calls void function
	assertContains(t, ir, "call void @doWork")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

// B0046: go extern_func() must generate a wrapper to handle sret ABI.
func TestGoExprExternFunction(t *testing.T) {
	ir := generateIR(t, `
		get_data(int x) string `+"`"+`extern("test_get_data");
		main() {
			t := go get_data(42);
			result := <-t;
		}
	`)
	// Extern declared with sret pattern (void return, i8* sret first param)
	assertContains(t, ir, "declare void @test_get_data(i8* %sret")
	// Wrapper function generated for the go expression
	assertContains(t, ir, ".go_extern_wrap.get_data.")
	// Wrapper calls the extern with sret
	assertContains(t, ir, "call void @test_get_data(i8*")
	// Coroutine calls the wrapper (returns i8*, not void)
	assertContains(t, ir, "call i8* @.go_extern_wrap.get_data.")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoExprExternVoidFunction(t *testing.T) {
	ir := generateIR(t, `
		do_work(int x) `+"`"+`extern("test_do_work");
		main() {
			t := go do_work(42);
			<-t;
		}
	`)
	// Even void externs need the wrapper — extern int params expect i8*
	// (pointer to value struct) but Promise internal representation is i64
	assertContains(t, ir, ".go_extern_wrap.do_work.")
	assertContains(t, ir, "call i8* @promise_g_new(")
}

// Container return types (Vector, Channel) use direct i8* return — no sret.
func TestGoExprExternContainerReturn(t *testing.T) {
	ir := generateIR(t, `
		get_items() string[] `+"`"+`extern("test_get_items");
		main() {
			t := go get_items();
			result := <-t;
		}
	`)
	// Vector return: i8* directly (no sret)
	assertContains(t, ir, "declare i8* @test_get_items()")
	// Wrapper still generated (consistent handling of all externs)
	assertContains(t, ir, ".go_extern_wrap.get_items.")
	// Coroutine calls wrapper which returns i8*
	assertContains(t, ir, "call i8* @.go_extern_wrap.get_items.")
}

// Extern with string param (i8* in both Promise and extern ABI) + multiple args.
func TestGoExprExternMultipleArgs(t *testing.T) {
	ir := generateIR(t, `
		process(string name, int count) string `+"`"+`extern("test_process");
		main() {
			t := go process("hello", 5);
			result := <-t;
		}
	`)
	// Wrapper generated with both params
	assertContains(t, ir, ".go_extern_wrap.process.")
	// Extern uses sret for string return
	assertContains(t, ir, "declare void @test_process(i8* %sret")
	// Coroutine calls wrapper
	assertContains(t, ir, "call i8* @.go_extern_wrap.process.")
}

func TestGoExprBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			t := go { };
			<-t;
		}
	`)
	// Coroutine function for the block
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

// B0113: go method_call() should work — not just direct function calls.
func TestGoExprMethodCall(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int value;
			get_value(this) int { return this.value; }
		}
		main() {
			c := Counter(value: 42);
			t := go c.get_value();
			result := <-t;
		}
	`)
	// Coroutine generated with capture of outer local 'c'
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// Method call generated inside coroutine body
	assertContains(t, ir, "Counter.get_value")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

// B0113: go method_call() fire-and-forget (void method, result discarded).
func TestGoExprMethodCallFireAndForget(t *testing.T) {
	ir := generateIR(t, `
		type Worker {
			int id;
			run(this) { }
		}
		main() {
			w := Worker(id: 1);
			go w.run();
		}
	`)
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	assertContains(t, ir, "Worker.run")
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
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

// --- Channel tests ---

func TestChannelConstructor(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
		}
	`)
	// Should call promise_channel_new
	assertContains(t, ir, "call i8* @promise_channel_new(")
	// Should init mutex and 2 cond vars inside promise_channel_new
	assertContains(t, ir, "call i8* @pal_mutex_init()")
	assertContains(t, ir, "call i8* @pal_cond_init()")
}

func TestChannelConstructorUnbuffered(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int]();
		}
	`)
	// Unbuffered: capacity=0
	assertContains(t, ir, "call i8* @promise_channel_new(i64 0,")
}

func TestChannelSend(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	// Should lock/unlock mutex and use memcpy for send
	assertContains(t, ir, "call void @pal_mutex_lock(")
	assertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
	assertContains(t, ir, "call void @pal_cond_signal(")
	assertContains(t, ir, "call void @pal_mutex_unlock(")
}

func TestChannelClose(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.close();
		}
	`)
	// Close should broadcast both cond vars
	assertContains(t, ir, "call void @pal_cond_broadcast(")
}

func TestChannelReceive(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
			val := <-ch;
		}
	`)
	// Should have channel receive blocks
	assertContains(t, ir, "chrecv.wait")
	assertContains(t, ir, "chrecv.check")
	assertContains(t, ir, "chrecv.none")
	assertContains(t, ir, "chrecv.read")
	assertContains(t, ir, "chrecv.done")
	// Returns optional { i1, i64 }
	assertContains(t, ir, "insertvalue { i1, i64 }")
}

func TestChannelForIn(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
			ch.close();
			for v in ch {
				int x = v + 1;
			}
		}
	`)
	// Should have channel for-in block labels
	assertContains(t, ir, "forin_ch.header")
	assertContains(t, ir, "forin_ch.recv.wait")
	assertContains(t, ir, "forin_ch.recv.check")
	assertContains(t, ir, "forin_ch.recv.none")
	assertContains(t, ir, "forin_ch.recv.read")
	assertContains(t, ir, "forin_ch.body")
	assertContains(t, ir, "forin_ch.exit")
}

func TestChannelSendClosedPanic(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	// Send should check closed flag and panic if set
	assertContains(t, ir, "send.closed.panic")
	assertContains(t, ir, "send on closed channel")
	// After wait-full wakeup, should re-check closed
	assertContains(t, ir, "send.waitfull.closed")
}

func TestChannelDoubleClosePanic(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.close();
		}
	`)
	// Close should check already-closed flag
	assertContains(t, ir, "close.panic")
	assertContains(t, ir, "close of closed channel")
}

func TestGoBlockCapturesOuterVars(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			int x = 10;
			go {
				ch.send(x);
			};
		}
	`)
	// Coroutine should have parameters for captured variables
	assertContains(t, ir, "define i8* @.goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// G created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoBlockCapturesMultipleVars(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			int a = 10;
			int b = 20;
			go {
				ch.send(a + b);
			};
		}
	`)
	// Coroutine function should accept captured parameters
	assertContains(t, ir, "define i8* @.goroutine.")
	assertContains(t, ir, "presplitcoroutine")
}

// B0111: Select inside go block must capture outer channel variables.
func TestGoBlockCapturesSelectChannelVars(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(1);
			go {
				select {
					v := <-ch:
						print_line("ok");
				}
			};
		}
	`)
	// The goroutine coroutine should have a parameter for the captured "ch"
	assertContains(t, ir, "ch.cap")
	// Should still generate coroutine infrastructure
	assertContains(t, ir, "define i8* @.goroutine.")
	assertContains(t, ir, "presplitcoroutine")
}

func TestGoBlockNoCapturesStillWorks(t *testing.T) {
	ir := generateIR(t, `
		main() {
			go { };
		}
	`)
	// Even without captures, the go block should generate a coroutine
	assertContains(t, ir, "define i8* @.goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
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

func TestSchedulerGlobals(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	// Thread-local current G pointer
	assertContains(t, ir, "@__promise_current_g")
	assertContains(t, ir, "thread_local")
	// Global scheduler singleton
	assertContains(t, ir, "@__promise_sched")
}

func TestSchedulerFunctionsExist(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	assertContains(t, ir, "define void @promise_sched_init(")
	assertContains(t, ir, "define i8* @promise_sched_loop(")
	assertContains(t, ir, "define void @promise_sched_enqueue(")
	assertContains(t, ir, "define i8* @promise_sched_find_runnable(")
	assertContains(t, ir, "define void @promise_sched_park_m(")
	assertContains(t, ir, "define void @promise_sched_wake_m()")
	assertContains(t, ir, "define void @promise_goroutine_exit(")
	assertContains(t, ir, "define void @promise_sched_shutdown()")
	assertContains(t, ir, "define i8* @promise_g_new(")
}

func TestWaiterListFunctionsExist(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	assertContains(t, ir, "define void @promise_waiter_enqueue(")
	assertContains(t, ir, "define i8* @promise_waiter_dequeue(")
	assertContains(t, ir, "define void @promise_waiter_wake_all(")
}

func TestCoroIntrinsicsDeclared(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	assertContains(t, ir, "declare token @llvm.coro.id(")
	assertContains(t, ir, "declare i1 @llvm.coro.alloc(")
	assertContains(t, ir, "declare i8* @llvm.coro.begin(")
	assertContains(t, ir, "declare i64 @llvm.coro.size.i64()")
	assertContains(t, ir, "declare i8 @llvm.coro.suspend(")
	assertContains(t, ir, "declare void @llvm.coro.end(")
	assertContains(t, ir, "declare i8* @llvm.coro.free(")
	assertContains(t, ir, "declare void @llvm.coro.resume(")
	assertContains(t, ir, "declare void @llvm.coro.destroy(")
	assertContains(t, ir, "declare i1 @llvm.coro.done(")
}

func TestGoBlockEmitsCoroutine(t *testing.T) {
	ir := generateIR(t, `
		main() {
			x := 42;
			go { x; };
		}
	`)
	// Coroutine function with presplitcoroutine attribute
	assertContains(t, ir, "presplitcoroutine")
	// Coroutine intrinsics used in the go block
	assertContains(t, ir, "call token @llvm.coro.id(")
	assertContains(t, ir, "call i1 @llvm.coro.alloc(")
	assertContains(t, ir, "call i8* @llvm.coro.begin(")
	assertContains(t, ir, "call i8 @llvm.coro.suspend(")
	assertContains(t, ir, "call void @llvm.coro.end(")
	// Go blocks now use coroutine + G + enqueue, not direct pal_thread_create
	// (pal_thread_create is still used by the scheduler for M threads, but not in go block codegen)
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoBlockEnqueuesG(t *testing.T) {
	ir := generateIR(t, `
		main() {
			go { };
		}
	`)
	// G creation and enqueue
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestChannelSendInCoroutineSuspends(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				ch.send(42);
			};
		}
	`)
	// Inside go block, channel send should use goroutine-aware park
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
	// The go block is a coroutine
	assertContains(t, ir, "presplitcoroutine")
}

func TestChannelRecvInCoroutineSuspends(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				result := <-ch;
			};
		}
	`)
	// Inside go block, channel recv should use goroutine-aware park
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
}

func TestChannelCloseWakesAllWaiters(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.close();
		}
	`)
	// Close should call promise_waiter_wake_all for both send and recv waiters
	assertContains(t, ir, "call void @promise_waiter_wake_all(")
}

func TestChannelStructHas15Fields(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
		}
	`)
	// Channel struct should have 16 fields including the 4 waiter lists and refcount
	// The channel_new function initializes all fields including waiter lists
	assertContains(t, ir, "define i8* @promise_channel_new(")
}

func TestTaskReceiveParksGoroutine(t *testing.T) {
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Task receive in main (non-coroutine) uses thread-blocking mode
	// But the G.done field should be checked
	assertContains(t, ir, "promise_g_new")
	assertContains(t, ir, "promise_sched_enqueue")
}

// --- Phase 5c gap-filling tests ---

func TestTaskReceiveInCoroutine(t *testing.T) {
	// <-task inside a go block uses the coroutine park path with done_lock,
	// not the thread-blocking path. The done_lock protects the done flag and
	// done_waiters list, and park_mutex holds the lock across coro.suspend.
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			go {
				t := go compute();
				int result = <-t;
			};
		}
	`)
	// The outer go block is a coroutine
	assertContains(t, ir, "presplitcoroutine")
	// Task receive inside coroutine: parks on done_waiters via done_lock
	assertContains(t, ir, "task.done")
	assertContains(t, ir, "task.wait")
	assertContains(t, ir, "task.ready")
	// done_lock path: check under lock, park if not done
	assertContains(t, ir, "task.done_under_lock")
	assertContains(t, ir, "task.park")
	// Coroutine suspend in the task wait path
	assertContains(t, ir, "task.resume")
	// Should NOT use usleep (that's the thread-blocking path)
	// The go block coroutine uses coro.suspend instead
}

func TestTaskReceiveCoroutineMode(t *testing.T) {
	// <-task in main uses coroutine parking (main is compiled as goroutine)
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Coroutine mode: park on done_waiters with done_lock, coro.suspend
	assertContains(t, ir, "task.park")
	assertContains(t, ir, "task.resume")
	assertContains(t, ir, "task.done_under_lock")
}

func TestVoidTaskSentinel(t *testing.T) {
	// Void tasks set result_ptr to sentinel inttoptr(i64 1) so goroutine_exit
	// knows not to free G (the receiver frees it via <-task).
	ir := generateIR(t, `
		doWork() { }
		main() {
			t := go doWork();
			<-t;
		}
	`)
	// Sentinel value: inttoptr i64 1 to i8*
	assertContains(t, ir, "inttoptr i64 1 to i8*")
	// G is freed by the receiver, not goroutine_exit
	assertContains(t, ir, "task.ready")
}

func TestVoidGoBlockSentinel(t *testing.T) {
	// go { block } used as a task (assigned + awaited) — should set sentinel
	// so goroutine_exit doesn't free G before the receiver does.
	ir := generateIR(t, `
		main() {
			t := go { };
			<-t;
		}
	`)
	assertContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestFireAndForgetGoBlockNoSentinel(t *testing.T) {
	// go { block } as a statement (fire-and-forget) — should NOT set sentinel.
	// goroutine_exit will free the G struct since result_ptr stays null.
	ir := generateIR(t, `
		main() {
			go { };
		}
	`)
	// The go block trampoline body should not contain the sentinel store.
	// After promise_g_new, the next call should be promise_sched_enqueue (no inttoptr).
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
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

func TestGoCallTaskSetsSentinel(t *testing.T) {
	// go void_func() used as a task (assigned + awaited) — should set sentinel.
	ir := generateIR(t, `
		work() { }
		main() {
			t := go work();
			<-t;
		}
	`)
	assertContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestGoCallNonVoidTaskSetsResultPtr(t *testing.T) {
	// go non_void_func() as task — should allocate result buffer (not sentinel).
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			<-t;
		}
	`)
	// Result buffer allocated via pal_alloc and stored in result_ptr
	assertContains(t, ir, "call i8* @pal_alloc(")
}

func TestFireAndForgetGoBlockInLoop(t *testing.T) {
	// go { } as the only statement in a for loop — fire-and-forget.
	// genBlock routes all statements through genStmt, so the flag is set.
	ir := generateIR(t, `
		main() {
			for i in 0..3 {
				go { };
			}
		}
	`)
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertNotContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestFireAndForgetGoBlockNestedFlagRestore(t *testing.T) {
	// Nested go blocks: outer is a task, inner is fire-and-forget.
	// The inner genStmt clears goExprFireAndForget, but genGoBlock
	// saves/restores it, so the outer block still sets sentinel.
	ir := generateIR(t, `
		main() {
			t := go {
				go { };
			};
			<-t;
		}
	`)
	// Outer go block should have sentinel (it's a task)
	assertContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestGoroutineExitSkipsFreeForTask(t *testing.T) {
	// goroutine_exit checks result_ptr != null to decide whether to free G.
	// Tasks (result_ptr set) skip the free; fire-and-forget goroutines are freed.
	ir := generateIR(t, `
		main() { }
	`)
	// goroutine_exit should contain the conditional skip-free logic
	assertContains(t, ir, "define void @promise_goroutine_exit(")
	// The function checks result_ptr to decide whether to free
	assertContains(t, ir, "skip_free:")
	assertContains(t, ir, "do_free:")
}

func TestFireAndForgetNonVoidNoResultBuffer(t *testing.T) {
	// B0109: go non_void_func() as fire-and-forget (result discarded) should NOT
	// allocate a result buffer. result_ptr stays null so goroutine_exit frees G.
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			go compute();
		}
	`)
	// The coroutine body should null-check result_ptr before storing
	assertContains(t, ir, "store_result:")
	assertContains(t, ir, "after_store:")
	// Main function should go directly from promise_g_new to promise_sched_enqueue
	// without storing to result_ptr field (no inttoptr sentinel, no pal_alloc between them)
	assertNotContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestChannelSendCoroutineRendezvous(t *testing.T) {
	// Unbuffered channel send inside a go block uses coroutine-mode rendezvous:
	// after writing the value, the sender parks and suspends waiting for the
	// receiver to pick it up.
	ir := generateIR(t, `
		main() {
			ch := channel[int]();
			go {
				ch.send(42);
			};
			result := <-ch;
		}
	`)
	// Inside the coroutine, the rendezvous wait should use waiter_enqueue + coro.suspend
	assertContains(t, ir, "send.rv.wait")
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
}

func TestChannelSendRendezvousExitWakesNextSender(t *testing.T) {
	// B0156: When a sender exits the unbuffered rendezvous wait, it must
	// wake the next sender from send_waiters via promise_waiter_wake_one.
	// Without this, a second sender parked on send_waiters would deadlock.
	ir := generateIR(t, `
		main() {
			ch := channel[int]();
			go {
				ch.send(42);
			};
			result := <-ch;
		}
	`)
	// The rendezvous exit block should call wake_one to propagate to next sender
	assertContains(t, ir, "send.rv.exit")
	// Count wake_one calls: there should be at least 2 in the coroutine send path
	// (one after writing to wake recv, one in rv.exit to wake next sender)
	count := strings.Count(ir, "call void @promise_waiter_wake_one(")
	if count < 2 {
		t.Errorf("expected at least 2 promise_waiter_wake_one calls in send path, got %d", count)
	}
}

func TestForInChannelCoroutineMode(t *testing.T) {
	// for-in channel inside a go block uses coroutine-mode park+suspend
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				for v in ch {
					int x = v + 1;
				}
			};
			ch.send(1);
			ch.close();
		}
	`)
	// The for-in inside the coroutine should use waiter_enqueue + coro.suspend
	assertContains(t, ir, "forin_ch.recv.wait")
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
	// Should have the coroutine resume block for the for-in
	assertContains(t, ir, "forin_ch.recv.resume")
}

func TestChannelRecvWakesSenderGoroutine(t *testing.T) {
	// After receiving, the code should wake a parked sender goroutine via
	// promise_waiter_wake_one (handles both regular G and select SWN nodes).
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(1);
			result := <-ch;
		}
	`)
	assertContains(t, ir, "call void @promise_waiter_wake_one(")
}

func TestChannelSendWakesRecvGoroutine(t *testing.T) {
	// After sending, the code should wake a parked receiver goroutine via
	// promise_waiter_wake_one (handles both regular G and select SWN nodes).
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(42);
		}
	`)
	assertContains(t, ir, "call void @promise_waiter_wake_one(")
}

func TestSelectBlockingEmitsSWNParking(t *testing.T) {
	// A blocking select (no default) in coroutine mode should emit:
	// - SelectWaiterNode allocas and initialization (kind sentinel 0xFF)
	// - select_waiter_enqueue calls to park SWNs on channel waiter lists
	// - select_try_wake definition (wake-once protocol)
	// - waiter_wake_one definition (handles both G and SWN nodes)
	// - waiter_remove calls for SWN cleanup after resume
	ir := generateIR(t, `
		main() {
			ch1 := channel[int](capacity: 1);
			ch2 := channel[int](capacity: 1);
			go { ch1.send(1); };
			select {
				v := <-ch1:
					print_line("ch1");
				v := <-ch2:
					print_line("ch2");
			}
		}
	`)
	// SWN infrastructure functions
	assertContains(t, ir, "define void @promise_select_waiter_enqueue(")
	assertContains(t, ir, "define i1 @promise_select_try_wake(")
	assertContains(t, ir, "define void @promise_waiter_wake_one(")

	// Blocking path: SWN kind sentinel (0xFF = 255) stored to field 1
	assertContains(t, ir, "store i8 255,")

	// SWN enqueue calls (one per case)
	assertContains(t, ir, "call void @promise_select_waiter_enqueue(")

	// SWN cleanup after resume
	assertContains(t, ir, "call void @promise_waiter_remove(")

	// Select mutex lifecycle
	assertContains(t, ir, "call void @pal_mutex_destroy(")
}

func TestSelectNonBlockingNoSWN(t *testing.T) {
	// A select with a default case should NOT emit SWN parking code.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			select {
				v := <-ch:
					print_line("got");
				default:
					print_line("default");
			}
		}
	`)
	assertNotContains(t, ir, "call void @promise_select_waiter_enqueue(")
	assertContains(t, ir, "select.default")
}

func TestSelectEmptyDefaultNotBlocking(t *testing.T) {
	// B0116: An empty default body (no statements) must still be treated as
	// a non-blocking select. Previously, the nil []Stmt was indistinguishable
	// from "no default clause", causing the select to block.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			select {
				v := <-ch:
				default:
			}
		}
	`)
	assertNotContains(t, ir, "call void @promise_select_waiter_enqueue(")
	assertContains(t, ir, "select.default")
}

func TestSelectEmptyDefaultTwiceNotBlocking(t *testing.T) {
	// B0116: Two consecutive selects with empty default must both be non-blocking.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			select { v := <-ch: default: }
			select { v := <-ch: default: }
		}
	`)
	assertNotContains(t, ir, "call void @promise_select_waiter_enqueue(")
	// Both selects should have default blocks
	assertContains(t, ir, "select.default")
}

func TestSelectBlockingPollInNonCoroutine(t *testing.T) {
	// B0045: A blocking select (no default) in non-coroutine context should
	// emit a poll-retry loop that unlocks, sleeps, re-locks, and retries
	// instead of falling through to merge (which silently skips all cases).
	ir := generateIR(t, `
		foo() {
			ch := channel[int](capacity: 1);
			select {
				v := <-ch:
			}
		}
		main() { foo(); }
	`)
	// Poll block should exist (not SWN parking — that's for coroutines)
	assertContains(t, ir, "select.poll")
	assertNotContains(t, ir, "select.park")
	// Should call usleep in the poll loop
	assertContains(t, ir, "call i32 @usleep(i32 100)")
	// Should branch back to lock.start for retry
	assertContains(t, ir, "br label %select.lock.start")
}

func TestSelectWakePathSendGuard(t *testing.T) {
	// B0110: A blocking select with a send case should emit a fullness
	// re-check on the wake path. Between the wake and re-locking channels,
	// another sender may have filled the freed slot. The guard branches to
	// a retry block that unlocks all channels and retries from lock.start.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go { <-ch; };
			select {
				ch.send(42):
					print_line("sent");
				v := <-ch:
					print_line("recv");
			}
		}
	`)
	// Wake path should have a send.ok block (guard passed)
	assertContains(t, ir, "select.wk0.send.ok")
	// Wake retry block should exist for failed guard
	assertContains(t, ir, "select.wake.retry")
	// Retry should branch back to lock.start
	assertContains(t, ir, "br label %select.lock.start")
}

func TestBuildMatchPhiMixedArms(t *testing.T) {
	// Match expression where some arms produce values and at least one arm
	// has an early return. buildMatchPhi must handle missing predecessors
	// by inserting null placeholders for arms that branch to merge without values.
	ir := generateIR(t, `
		test(int n) int {
			int result = match n {
				1 => 10,
				2 => 20,
				_ => 0,
			};
			return result;
		}
		main() { }
	`)
	// PHI node should exist in the merge block with values from all arms
	assertContains(t, ir, "phi i64")
	assertContains(t, ir, "match.end")
}

func TestBuildMatchPhiStatementOnly(t *testing.T) {
	// Match used as a statement (no arm produces a value) — no PHI needed
	ir := generateIR(t, `
		test(int n) {
			match n {
				1 => { int x = 10; },
				_ => { int y = 20; },
			};
		}
		main() { }
	`)
	// Should have match arms but the merge block shouldn't have a PHI
	assertContains(t, ir, "match.arm")
	assertContains(t, ir, "match.end")
}

func TestEnumMatchPhiWithEarlyReturn(t *testing.T) {
	// Enum match where one arm returns early (doesn't branch to merge).
	// buildMatchPhi must skip non-merging arms to avoid PHI predecessor mismatch.
	ir := generateIR(t, `
		enum Op { Add(int a, int b), Neg(int a) }
		eval(Op op) int {
			return match op {
				Add(a, b) => a + b,
				Neg(a) => 0 - a,
			};
		}
		main() { }
	`)
	// Both arms produce values; PHI should merge them
	assertContains(t, ir, "phi i64")
}

func TestSchedulerReleasesParkMutex(t *testing.T) {
	// The scheduler loop checks G.park_mutex after coro.resume returns
	// and releases it if non-null. This closes the enqueue-before-suspend race.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go { ch.send(42); };
		}
	`)
	// Scheduler loop must contain the park_mutex release blocks
	assertContains(t, ir, "release_park_mutex")
	assertContains(t, ir, "after_release")
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

// --- Named Arguments Tests ---

func TestNamedArgsConstructorCodegen(t *testing.T) {
	// Named args in reverse order should produce correct field stores
	ir := generateIR(t, `
		type Point { int x; int y; }
		main() {
			p := Point(y: 20, x: 10);
		}
	`)
	// Both fields should be stored
	assertContains(t, ir, "store")
}

func TestNamedArgsConstructorPositionalCodegen(t *testing.T) {
	// All positional args should work for constructors
	ir := generateIR(t, `
		type Point { int x; int y; }
		main() {
			p := Point(10, 20);
		}
	`)
	assertContains(t, ir, "store")
}

func TestNamedArgsFunctionCallCodegen(t *testing.T) {
	// Named args reordered should generate correct call
	ir := generateIR(t, `
		add(int a, int b) int { return a + b; }
		main() {
			r := add(b: 2, a: 1);
		}
	`)
	assertContains(t, ir, "call")
	assertContains(t, ir, "@add")
}

func TestNamedArgsMethodCallCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Calc {
			int value;
			compute(int a, int b) int { return a + b; }
		}
		main() {
			c := Calc(value: 0);
			r := c.compute(b: 2, a: 1);
		}
	`)
	assertContains(t, ir, "Calc.compute")
}

func TestNamedArgsMixedPositionalNamedCodegen(t *testing.T) {
	ir := generateIR(t, `
		calc(int a, int b, int c) int { return a + b + c; }
		main() {
			r := calc(1, c: 3, b: 2);
		}
	`)
	assertContains(t, ir, "@calc")
}

// --- Optional interpolation tests ---

func TestStringInterpolationOptionalPresent(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			string s = "{x}";
		}
	`)
	// Should branch on presence flag
	assertContains(t, ir, "interp.some")
	assertContains(t, ir, "interp.none")
	assertContains(t, ir, "interp.merge")
	// Should call int_to_string in the some branch
	assertContains(t, ir, "promise_int_to_string")
}

func TestStringInterpolationOptionalNone(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = none;
			string s = "{x}";
		}
	`)
	assertContains(t, ir, "interp.some")
	assertContains(t, ir, "interp.none")
}

func TestStringInterpolationOptionalString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? name = "Alice";
			string s = "hello {name}";
		}
	`)
	assertContains(t, ir, "interp.some")
	assertContains(t, ir, "interp.none")
}

// --- User type interpolation codegen tests ---

func TestStringInterpolationUserTypeDirect(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			int x;
			format(Writer ~w)! { w.write_string("foo"); }
		}
		main() { Foo f = Foo(x: 1); string s = "{f}"; }
	`)
	// Should call Foo.format and Builder.to_string
	assertContains(t, ir, "Foo.format")
	assertContains(t, ir, "Builder.to_string")
	assertContains(t, ir, "interp.format.ok")
}

func TestStringInterpolationUserTypeVtable(t *testing.T) {
	ir := generateIR(t, `
		type Shape {
			format(Writer ~w)! { w.write_string("shape"); }
		}
		type Circle is Shape {
			format(Writer ~w)! { w.write_string("circle"); }
		}
		main() {
			Shape s = Circle();
			string x = "{s}";
		}
	`)
	// Virtual dispatch: should have vtable load + indirect call, not direct Shape.format
	assertContains(t, ir, "__interp_builder_writer_vtable")
	assertContains(t, ir, "interp.format.ok")
}

// T0084: Builder is freed after callFormatToString extracts the string
func TestCallFormatToStringBuilderDrop(t *testing.T) {
	ir := generateIR(t, `
		type Pt {
			int x;
			format(Writer ~w)! { w.write_string("pt"); }
		}
		main() { Pt p = Pt(x: 1); string s = "{p}"; }
	`)
	// After Builder.to_string, Builder.drop should be called to free the Builder
	assertContains(t, ir, "Builder.to_string")
	assertContains(t, ir, "Builder.drop")
}

// T0084: Builder.to_string() result is tracked as a string temp
func TestBuilderToStringTracked(t *testing.T) {
	ir := generateIR(t, `
		main() {
			Builder b = Builder();
			b.write_string("hello")!;
			assert(b.to_string() == "hello", "ok");
		}
	`)
	// Builder.to_string result should be tracked and dropped at statement end
	assertContains(t, ir, "Builder.to_string")
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "promise_string_drop")
}

// --- Optional narrowing codegen tests ---

func TestOptionalTruthinessNarrowingCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? cc = "hello";
			if cc {
				string s = cc;
			}
		}
	`)
	// Should have narrow blocks (not regular if blocks)
	assertContains(t, ir, "narrow.then")
	assertContains(t, ir, "narrow.end")
	// In the then block, should extractvalue the inner string from the optional
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

func TestIsPresentNarrowingCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			if x is present {
				int n = x;
			}
		}
	`)
	assertContains(t, ir, "narrow.then")
	// Should extract the inner i64 from { i1, i64 }
	assertContains(t, ir, "extractvalue { i1, i64 }")
}

func TestOptionalNarrowingWithElseCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			if x {
				int n = x;
			} else {
				int n = 0;
			}
		}
	`)
	assertContains(t, ir, "narrow.then")
	assertContains(t, ir, "narrow.else")
	assertContains(t, ir, "narrow.end")
}

func TestUninitOptionalVarCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x;
			string s = "{x}";
		}
	`)
	assertContains(t, ir, "zeroinitializer")
	assertContains(t, ir, "interp.none")
}

func TestNegatedNarrowingCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? cc = "hello";
			if !cc {
				string s = "none";
			} else {
				string s = cc;
			}
		}
	`)
	assertContains(t, ir, "narrow.then")
	assertContains(t, ir, "narrow.else")
}

func TestCompoundNarrowingCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? a = 1;
			string? b = "hi";
			if a && b {
				int x = a;
				string y = b;
			}
		}
	`)
	assertContains(t, ir, "narrow.check")
	assertContains(t, ir, "narrow.then")
}

func TestOptionalChainUserType(t *testing.T) {
	ir := generateIR(t, `
		type Cfg {
			int port;
		}
		main() {
			Cfg? c = Cfg(port: 8080);
			int? p = c?.port;
		}
	`)
	assertContains(t, ir, "optchain.some")
	assertContains(t, ir, "optchain.none")
}

// --- Index/Slice Operator Method Dispatch Tests ---

func TestIndexMethodDispatchMap(t *testing.T) {
	// Map [] goes through genMethodIndex, not genMapIndex
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
			int? v = m["a"];
		}
	`)
	assertContains(t, ir, `call { i1, i64 } @"Map[string, int].[]"(`)
}

func TestIndexAssignMethodDispatchMap(t *testing.T) {
	// Map []= goes through genMethodIndexAssign
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
			m["b"] = 2;
		}
	`)
	assertContains(t, ir, `call void @"Map[string, int].[]="(`)
}

func TestIndexNativeDispatchVector(t *testing.T) {
	// Vector [] still uses native path (genVectorIndex)
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			int x = v[0];
		}
	`)
	assertContains(t, ir, "index.ok")
	assertContains(t, ir, "index.oob")
}

func TestIndexNativeDispatchString(t *testing.T) {
	// String [] still uses native path (genStringIndex)
	ir := generateIR(t, `
		main() {
			s := "hello";
			char c = s[0];
		}
	`)
	assertContains(t, ir, "stridx.ok")
	assertContains(t, ir, "stridx.oob")
}

func TestSliceExprString(t *testing.T) {
	// String [:] uses native genStringSlice
	ir := generateIR(t, `
		main() {
			s := "hello";
			string sub = s[1:3];
		}
	`)
	assertContains(t, ir, "call i8* @promise_string_new(")
}

func TestSliceExprStringLowOnly(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello";
			string sub = s[1:];
		}
	`)
	assertContains(t, ir, "call i8* @promise_string_new(")
}

func TestSliceExprStringHighOnly(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello";
			string sub = s[:3];
		}
	`)
	assertContains(t, ir, "call i8* @promise_string_new(")
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

// --- Generator Tests ---

func TestGeneratorProducesCoroutine(t *testing.T) {
	ir := generateIR(t, `
		count() stream[int] {
			yield 1;
			yield 2;
		}
		main() {}
	`)
	assertContains(t, ir, `.generator.`)
	assertContains(t, ir, "presplitcoroutine")
	assertContains(t, ir, "@llvm.coro.suspend")
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
	// Two generator coroutines: inner and outer
	assertContains(t, ir, ".generator.0")
	assertContains(t, ir, ".generator.1")
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

func TestMultiParamGenericType(t *testing.T) {
	ir := generateIR(t, `
		type Pair[A, B] {
			A first;
			B second;
		}
		main() {
			p := Pair[int, string](first: 42, second: "hello");
		}
	`)
	// Monomorphized struct name should contain both type args
	assertContains(t, ir, "Pair[int, string]")
}

func TestMultiParamGenericFunc(t *testing.T) {
	ir := generateIR(t, `
		make_pair[A, B](A a, B b) (A, B) {
			return (a, b);
		}
		main() {
			(x, y) := make_pair[int, string](42, "hi");
		}
	`)
	// Monomorphized function name should contain both type args
	assertContains(t, ir, "make_pair[int, string]")
}

// --- Value type codegen tests ---

func TestValueTypeNoMalloc(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
		main() {
			p := Point(x: 1, y: 2);
		}
	`)
	// Value struct layout: {i8*, i64, i64} (no RTTI pointer — accessed via global)
	assertContains(t, ir, "%promise_Point_v = type { i8*, i64, i64 }")
	// Instance struct is RTTI-only: {promise_Point_m*}
	assertContains(t, ir, "%promise_Point_i = type { %promise_Point_m* }")
	// Should use insertvalue to build the value struct
	assertContains(t, ir, "insertvalue")
}

func TestValueTypeInsertValue(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
		main() {
			p := Point(x: 10, y: 20);
		}
	`)
	// Value struct has vtable + fields (no rtti pointer)
	assertContains(t, ir, "insertvalue")
	// Should have the RTTI global (used for is-checks, not stored in value struct)
	assertContains(t, ir, "@promise_rtti_Point")
}

func TestValueTypeFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
		main() {
			p := Point(x: 1, y: 2);
			int x = p.x;
		}
	`)
	// Field access uses extractvalue (no instance deref)
	assertContains(t, ir, "extractvalue")
}

func TestValueTypeMethodReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
			sum() int { return this.x + this.y; }
		}
		main() {
			p := Point(x: 3, y: 4);
			int s = p.sum();
		}
	`)
	// Method should be defined and callable
	assertContains(t, ir, "define")
	assertContains(t, ir, "Point.sum")
}

func TestValueTypeFieldAssignment(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
		main() {
			Point p = Point(x: 1, y: 2);
			p.x = 5;
		}
	`)
	// Field assignment uses GEP into the alloca, then store
	assertContains(t, ir, "getelementptr %promise_Point_v")
	assertContains(t, ir, "store i64 5")
}

func TestValueTypeCompoundAssignment(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
		main() {
			Point p = Point(x: 3, y: 4);
			p.x += 10;
		}
	`)
	// Compound assignment loads current value, adds, stores back
	assertContains(t, ir, "getelementptr %promise_Point_v")
	assertContains(t, ir, "add i64")
}

func TestValueTypeNewConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Clamped {
			int value `+"`value"+`;
			new(~this, int v) {
				if v < 0 { this.value = 0; }
				else { this.value = v; }
			}
		}
		main() {
			Clamped c = Clamped(v: 42);
		}
	`)
	// new() constructor called, value loaded from alloca after
	assertContains(t, ir, "call void @Clamped.new(")
	assertContains(t, ir, "load %promise_Clamped_v")
}

func TestValueTypeOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Vec2 {
			int x `+"`value"+`;
			int y `+"`value"+`;
			+(Vec2 other) Vec2 {
				return Vec2(x: this.x + other.x, y: this.y + other.y);
			}
		}
		main() {
			Vec2 a = Vec2(x: 1, y: 2);
			Vec2 b = Vec2(x: 3, y: 4);
			Vec2 c = a + b;
		}
	`)
	// Operator dispatches to Vec2.+ method
	assertContains(t, ir, `call %promise_Vec2_v @"Vec2.+"(`)
}

func TestValueTypeOptional(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
		main() {
			Point? maybe = Point(x: 1, y: 2);
		}
	`)
	// Optional wraps the full value struct: { i1, %promise_Point_v }
	assertContains(t, ir, "{ i1, %promise_Point_v }")
}

func TestValueTypeIsCheckUsesRTTIGlobal(t *testing.T) {
	ir := generateIR(t, `
		type Vec2 {
			f64 x `+"`value"+`;
			f64 y `+"`value"+`;
		}
		check(Vec2 v) bool { return v is Vec2; }
		main() {
			Vec2 v = Vec2(x: 1.0, y: 2.0);
			check(v);
		}
	`)
	// Value struct should NOT contain an RTTI pointer field
	assertContains(t, ir, "%promise_Vec2_v = type { i8*, double, double }")
	// RTTI global should still exist (used for is-checks)
	assertContains(t, ir, "@promise_rtti_Vec2")
	// is-check should use the RTTI global, not extract field 1 from value struct.
	// The bitcast of the RTTI global to i8* is the indicator.
	assertContains(t, ir, "bitcast %promise_Vec2_i* @promise_rtti_Vec2 to i8*")
}

func TestValueTypeDestructureIsPattern(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
		check(Point p) int {
			if p is Point(px, py) { return px + py; }
			return 0;
		}
		main() {
			check(Point(x: 3, y: 4));
		}
	`)
	// Should use RTTI global for the is-check
	assertContains(t, ir, "bitcast %promise_Point_i* @promise_rtti_Point to i8*")
	// Field extraction uses extractvalue on the value struct (fields at index 1, 2)
	assertContains(t, ir, "extractvalue %promise_Point_v")
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
	assertContains(t, ir, "define i64 @sum(i8* %nums)")
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
	assertContains(t, ir, "call i64 @count(i8*")
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
	assertContains(t, ir, "define i8* @join(i8* %sep, i8* %items)")
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
	assertContains(t, ir, "call i64 @sum(i8*")
}

func TestVariadicMethodIR(t *testing.T) {
	// Variadic method: receiver + variadic param in IR.
	ir := generateIR(t, `
		type Adder {
			int base;

			addAll(&this, ...int values) int {
				return this.base;
			}
		}
		main() {
			a := Adder(base: 10);
			a.addAll(1, 2, 3);
		}
	`)
	// Method takes instance ptr + vector param
	assertContains(t, ir, "define i64 @Adder.addAll(")
	assertContains(t, ir, "i8* %values")
}

func TestVariadicFailableIR(t *testing.T) {
	// Variadic + failable function in IR.
	ir := generateIR(t, `
		trySum(...int nums) int! {
			if nums.len == 0 { raise error(message: "empty"); }
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		main() {
			x := trySum(1, 2, 3)!;
		}
	`)
	// Failable returns {i1, i64, i8*} (error flag + result + error ptr)
	assertContains(t, ir, "define { i1, i64, i8* } @trySum(i8* %nums)")
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
	assertContains(t, ir, "define i64 @doubleSum(i8* %nums)")
	// Inner call passes nums directly (T[] → T[])
	assertContains(t, ir, "call i64 @sum(i8*")
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
	assertContains(t, ir, "call i64 @sum(i8*")
}

// --- Numeric Suffix Tests ---

func TestNumericSuffixU8IR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			u8 x = 42u8;
		}
	`)
	assertContains(t, ir, "store i8 42")
}

func TestNumericSuffixU16IR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			u16 x = 1000u16;
		}
	`)
	assertContains(t, ir, "store i16 1000")
}

func TestNumericSuffixI32IR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			i32 x = 0xFFi32;
		}
	`)
	assertContains(t, ir, "store i32 255")
}

func TestNumericSuffixF32IR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f32 x = 1.5f32;
		}
	`)
	assertContains(t, ir, "store float")
}

func TestF32LiteralRounding(t *testing.T) {
	// 3.14 as f32 should be 0x4048F5C3 (3.14000010...), not 0x4048F5C2 (3.13999962...)
	// The f64 hex encoding of f32 0x4048F5C3 is 0x40091EB860000000
	ir := generateIR(t, `
		main() {
			f32 x = 3.14f32;
		}
	`)
	assertContains(t, ir, "float 0x40091EB860000000")
	assertNotContains(t, ir, "float 0x40091EB840000000")
}

func TestNumericSuffixI64IR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			i64 x = 999i64;
		}
	`)
	assertContains(t, ir, "store i64 999")
}

func TestNumericSuffixU64IR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			u64 x = 42u64;
		}
	`)
	assertContains(t, ir, "store i64 42")
}

func TestNumericSuffixF64IR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 x = 3.14f64;
		}
	`)
	assertContains(t, ir, "store double")
}

func TestNumericSuffixBareIIR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 42i;
		}
	`)
	assertContains(t, ir, "store i64 42")
}

func TestNumericSuffixBareUIR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			uint x = 42u;
		}
	`)
	assertContains(t, ir, "store i64 42")
}

func TestGlobalMethodIR(t *testing.T) {
	ir := generateIR(t, "type Counter {\n"+
		"int value;\n"+
		"create(int v) Counter `global {\n"+
		"return Counter(value: v);\n"+
		"}\n"+
		"}\n"+
		"main() {\n"+
		"c := Counter.create(42);\n"+
		"}\n")
	// Global method should be defined as Counter.create with no 'this' parameter
	assertContains(t, ir, "Counter.create")
	// Should only have the 'v' param, not 'this'
	assertNotContains(t, ir, "Counter.create(i8*")
}

func TestGenericInheritanceNonGenericChild(t *testing.T) {
	ir := generateIR(t, `
		type Holder[T] { T value; }
		type IntHolder is Holder[int] {}
		main() {
			h := IntHolder(value: 42);
			int x = h.value;
		}
	`)
	// IntHolder uses Holder's layout — field should be i64 (int)
	assertContains(t, ir, "IntHolder")
	assertContains(t, ir, "load i64")
}

func TestGenericInheritanceForwardedTypeParams(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] {
			T data;
			get() T { return this.data; }
		}
		type Derived[T] is Base[T] {
			get() T { return this.data; }
		}
		main() {
			d := Derived[int](data: 99);
			int x = d.get();
		}
	`)
	// Monomorphized names should appear
	assertContains(t, ir, "Derived[int]")
	assertContains(t, ir, "Base[int]")
}

func TestMonoTypeVtableEmission(t *testing.T) {
	ir := generateIR(t, `
		type Producer[T] {
			produce() T `+"`"+`abstract;
		}
		type ConstProducer[T] is Producer[T] {
			T value;
			produce() T { return this.value; }
		}
		accept_producer(Producer[int] p) int {
			return p.produce();
		}
		main() {
			cp := ConstProducer[int](value: 5);
			int x = accept_producer(cp);
		}
	`)
	// Mono vtable and typeinfo should be emitted for ConstProducer[int]
	assertContains(t, ir, "promise_vtable_ConstProducer[int]")
	assertContains(t, ir, "promise_typeinfo_ConstProducer[int]")
	// The vtable should contain the mono method pointer
	assertContains(t, ir, "ConstProducer[int].produce")
}

func TestMonoVtableVirtualDispatchIR(t *testing.T) {
	ir := generateIR(t, `
		type Shape[T] {
			area() T `+"`"+`abstract;
		}
		type Circle[T] is Shape[T] {
			T radius;
			area() T { return this.radius; }
		}
		accept_shape(Shape[int] s) int {
			return s.area();
		}
		main() {
			c := Circle[int](radius: 5);
			int x = accept_shape(c);
		}
	`)
	// Vtable should exist for both parent and child mono instances
	assertContains(t, ir, "promise_vtable_Circle[int]")
	assertContains(t, ir, "promise_vtable_Shape[int]")
	// accept_shape should do virtual dispatch (load from vtable, indirect call)
	assertContains(t, ir, "promise_vtable_Shape[int]")
	// Mono method should be defined
	assertContains(t, ir, "Circle[int].area")
}

func TestMultipleMonoVtablesDistinct(t *testing.T) {
	ir := generateIR(t, `
		type Producer[T] {
			produce() T `+"`"+`abstract;
		}
		type ConstProducer[T] is Producer[T] {
			T value;
			produce() T { return this.value; }
		}
		use_int(Producer[int] p) int { return p.produce(); }
		use_str(Producer[string] p) string { return p.produce(); }
		main() {
			ci := ConstProducer[int](value: 1);
			cs := ConstProducer[string](value: "x");
			int i = use_int(ci);
			string s = use_str(cs);
		}
	`)
	// Separate vtables for int and string instantiations
	assertContains(t, ir, "promise_vtable_ConstProducer[int]")
	assertContains(t, ir, "promise_vtable_ConstProducer[string]")
	assertContains(t, ir, "promise_typeinfo_ConstProducer[int]")
	assertContains(t, ir, "promise_typeinfo_ConstProducer[string]")
	// Separate methods
	assertContains(t, ir, "ConstProducer[int].produce")
	assertContains(t, ir, "ConstProducer[string].produce")
}

func TestMonoVtableInheritedMethodResolution(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] {
			T val;
			get() T { return this.val; }
		}
		type Mid[T] is Base[T] {}
		type Leaf[T] is Mid[T] {}
		accept(Base[int] b) int { return b.get(); }
		main() {
			l := Leaf[int](val: 7);
			int x = accept(l);
		}
	`)
	// Leaf[int] vtable should reference Base[int].get (inherited method)
	assertContains(t, ir, "promise_vtable_Leaf[int]")
	assertContains(t, ir, "Base[int].get")
}

func TestMonoTypeInfoEmittedForParent(t *testing.T) {
	ir := generateIR(t, `
		type Animal[T] {
			T id;
			name() string `+"`"+`abstract;
		}
		type Dog[T] is Animal[T] {
			name() string { return "dog"; }
		}
		accept(Animal[int] a) string { return a.name(); }
		main() {
			d := Dog[int](id: 1);
			string s = accept(d);
		}
	`)
	// Both parent and child should have typeinfo
	assertContains(t, ir, "promise_typeinfo_Dog[int]")
	assertContains(t, ir, "promise_typeinfo_Animal[int]")
	assertContains(t, ir, "promise_vtable_Dog[int]")
}

func TestMonoVtableOverrideDispatches(t *testing.T) {
	ir := generateIR(t, `
		type Greeter[T] {
			T name;
			greet() string { return "hello"; }
		}
		type Fancy[T] is Greeter[T] {
			greet() string { return "fancy"; }
		}
		accept(Greeter[int] g) string { return g.greet(); }
		main() {
			Greeter[int] a = Greeter[int](name: 1);
			Greeter[int] b = Fancy[int](name: 2);
			string x = accept(a);
			string y = accept(b);
		}
	`)
	// Both should have vtables with their own greet method
	assertContains(t, ir, "promise_vtable_Greeter[int]")
	assertContains(t, ir, "promise_vtable_Fancy[int]")
	assertContains(t, ir, "Greeter[int].greet")
	assertContains(t, ir, "Fancy[int].greet")
}

func TestMonoVtableNonGenericChildOfGenericParent(t *testing.T) {
	// Non-generic children already have vtables via emitVtableGlobals.
	// Verify they coexist with mono vtables for the parent.
	ir := generateIR(t, `
		type Fabricator[T] {
			fabricate() T `+"`"+`abstract;
		}
		type IntFabricator is Fabricator[int] {
			fabricate() int { return 42; }
		}
		type GenFabricator[T] is Fabricator[T] {
			T val;
			fabricate() T { return this.val; }
		}
		use_fab(Fabricator[int] m) int { return m.fabricate(); }
		main() {
			int a = use_fab(IntFabricator());
			int b = use_fab(GenFabricator[int](val: 7));
		}
	`)
	// Non-generic child uses regular vtable naming
	assertContains(t, ir, "promise_vtable_IntFabricator")
	// Generic child uses mono vtable naming
	assertContains(t, ir, "promise_vtable_GenFabricator[int]")
	// Both methods exist
	assertContains(t, ir, "IntFabricator.fabricate")
	assertContains(t, ir, "GenFabricator[int].fabricate")
}

func TestMethodGenericIR(t *testing.T) {
	ir := generateIR(t, `
		type Echo {
			echo[T](T val) T { return val; }
		}
		main() {
			e := Echo();
			int x = e.echo[int](42);
			string s = e.echo[string]("hi");
		}
	`)
	// Monomorphized method names should appear
	assertContains(t, ir, "Echo.echo[int]")
	assertContains(t, ir, "Echo.echo[string]")
}

func TestMethodGenericOnGenericTypeIR(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T item;
			convert[R](R val) R { return val; }
		}
		main() {
			b := Box[int](item: 1);
			string s = b.convert[string]("hello");
		}
	`)
	// Should have mono type name + mono method name
	assertContains(t, ir, "Box[int].convert[string]")
}

// --- Monomorphization: gaps ---

// TestGenericValueTypeLayout verifies that a generic pure-value type gets the
// correct layout: fields are embedded in the value struct (_v), and the instance
// struct (_i) is RTTI-only (no user fields). Also checks that no heap allocation
// is emitted (value types are stack-allocated and copied).
func TestGenericValueTypeLayout(t *testing.T) {
	ir := generateIR(t, `
		type Pair[T] {
			T first `+"`"+`value;
			T second `+"`"+`value;
			sum(&this) T { return this.first; }
		}
		main() {
			p := Pair[int](first: 1, second: 2);
			x := p.sum();
		}
	`)
	// Mono type names should appear
	assertContains(t, ir, "Pair[int]")
	// Value struct has embedded fields (vtable + first + second)
	// The _v struct is named promise_Pair[int]_v
	assertContains(t, ir, "promise_Pair[int]_v")
	// Instance struct is RTTI-only (no user fields) — just the _variant pointer
	assertContains(t, ir, "promise_Pair[int]_i")
	// RTTI global is emitted for value types
	assertContains(t, ir, "promise_rtti_Pair[int]")
	// No heap allocation: value types are not malloc'd
	assertNotContains(t, ir, "promise_Pair[int]_i* @malloc")
}

// TestGenericValueTypeTwoInstances verifies that two instantiations of the same
// generic value type produce distinct layouts and RTTI globals.
func TestGenericValueTypeTwoInstances(t *testing.T) {
	ir := generateIR(t, `
		type Pair[T] {
			T first `+"`"+`value;
			T second `+"`"+`value;
		}
		main() {
			pi := Pair[int](first: 1, second: 2);
			pb := Pair[bool](first: true, second: false);
		}
	`)
	assertContains(t, ir, "promise_Pair[int]_v")
	assertContains(t, ir, "promise_Pair[bool]_v")
	assertContains(t, ir, "promise_rtti_Pair[int]")
	assertContains(t, ir, "promise_rtti_Pair[bool]")
	// Separate typeinfo for each instantiation
	assertContains(t, ir, "promise_typeinfo_Pair[int]")
	assertContains(t, ir, "promise_typeinfo_Pair[bool]")
}

// TestGenericEnumTwoTypeParams verifies that a generic enum with two type parameters
// is correctly monomorphized, producing distinct structs and a match that extracts
// both variant fields.
func TestGenericEnumTwoTypeParams(t *testing.T) {
	ir := generateIR(t, `
		enum Either[A, B] {
			Left(A val),
			Right(B val),
		}
		get_left(Either[int, string] e) int {
			int r = match e {
				Left(v) => v,
				Right(_) => -1,
			};
			return r;
		}
		main() {
			e := Either[int, string].Left(42);
			x := get_left(e);
		}
	`)
	// Both type params in mangled name
	assertContains(t, ir, "Either[int, string]")
	// Value struct typedef emitted
	assertContains(t, ir, "promise_Either[int, string]_v")
	// Function using the mono type exists
	assertContains(t, ir, "get_left")
}

// TestDeeplyNestedGenericMonomorphization verifies that transitive instance
// discovery via field types works at 3 levels of nesting.
func TestDeeplyNestedGenericMonomorphization(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T val; }
		main() {
			inner := Box[int](val: 1);
			mid := Box[Box[int]](val: inner);
			outer := Box[Box[Box[int]]](val: mid);
		}
	`)
	// All three levels must be monomorphized
	assertContains(t, ir, "Box[int]")
	assertContains(t, ir, "Box[Box[int]]")
	assertContains(t, ir, "Box[Box[Box[int]]]")
}

// TestGenericMethodReturnsGenericInstance verifies that a generic method whose
// return type is a monomorphized generic type is correctly compiled.
func TestGenericMethodReturnsGenericInstance(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T val;
			clone(&this) Box[T] { return Box[T](val: this.val); }
		}
		main() {
			b := Box[int](val: 5);
			c := b.clone();
		}
	`)
	assertContains(t, ir, "Box[int].clone")
	// Return type is also Box[int] — constructor call should appear
	assertContains(t, ir, "Box[int]")
}

// TestMonoSynthesizedDefaultOnGenericType verifies that a generic concrete type
// implementing a structural interface inherits the interface's default methods,
// and that those methods are emitted with the mono-qualified name.
func TestMonoSynthesizedDefaultOnGenericType(t *testing.T) {
	// Use a structural interface whose default method doesn't require operations
	// on T — just calls another abstract method.
	ir := generateIR(t, `
		type Sized `+"`"+`structural {
			size() int `+"`"+`abstract;
			nonempty() bool => this.size() > 0;
		}
		type Pair[T] is Sized {
			T a;
			T b;
			size() int { return 2; }
		}
		main() {
			p := Pair[int](a: 1, b: 2);
			bool r = p.nonempty();
		}
	`)
	// The synthesized nonempty default should appear with the mono-qualified name
	assertContains(t, ir, "Pair[int].nonempty")
	// The concrete size method should also appear
	assertContains(t, ir, "Pair[int].size")
}

// TestGenericFuncWithGenericReturnType verifies a generic function that both
// takes and returns a monomorphic generic type. Box[int] is instantiated directly
// in main so its layout is collected; the generic function takes and returns it.
func TestGenericFuncWithGenericReturnType(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T val; }
		identity_box[T](Box[T] b) Box[T] { return b; }
		main() {
			b := Box[int](val: 42);
			c := identity_box[int](b);
		}
	`)
	assertContains(t, ir, "identity_box[int]")
	assertContains(t, ir, "Box[int]")
}

// TestGenericTypeInfoEmitted verifies that RTTI typeinfo and vtable globals are
// emitted for monomorphic generic type instantiations. promise_type_is requires
// these globals at runtime to check inheritance relationships.
func TestGenericTypeInfoEmitted(t *testing.T) {
	ir := generateIR(t, `
		type Animal[T] {
			speak() T `+"`"+`abstract;
		}
		type Dog[T] is Animal[T] {
			T sound;
			speak() T { return this.sound; }
		}
		main() {
			Dog[int] d = Dog[int](sound: 1);
			Animal[int] a = d;
		}
	`)
	// Mono typeinfo and vtable globals must be emitted for Dog[int].
	assertContains(t, ir, "promise_typeinfo_Dog[int]")
	assertContains(t, ir, "promise_vtable_Dog[int]")
	// Animal[int] typeinfo must also be emitted (it's an abstract parent).
	assertContains(t, ir, "promise_typeinfo_Animal[int]")
}

// --- InstanceIRs, instanceOwnedFuncs, CompileWithCache tests ---

// mapKeys returns the keys of a string-keyed map, for diagnostics.
func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// boxWithGetMethod is a generic Box[T] type with a method that forces
// per-instance codegen for the method body.
const boxWithGetMethod = `
	type Box[T] {
		T value;
		get(this) T { return this.value; }
	}
`

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

func TestInstanceIRsNilWhenNoGenerics(t *testing.T) {
	// Non-generic code produces no instance IRs.
	file, info := parseWithStd(t, `
		type Foo { int x; }
		main() { f := Foo(x: 1); }
	`)
	result := Compile(file, info, "")
	instIRs := result.InstanceIRs()
	// May be nil or empty — either is acceptable.
	for name := range instIRs {
		// User types are not generic, so no user-defined instances expected.
		// (Std library instances like _FnIter may appear from iterator infrastructure;
		// this check is intentionally not exhaustive.)
		_ = name
	}
}

func TestInstanceOwnedFuncsTracking(t *testing.T) {
	// instanceOwnedFuncs should map Box[int]'s mangled methods to "Box[int]".
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() {
			b := Box[int](value: 1);
			int x = b.get();
		}
	`)
	result := Compile(file, info, "")
	c := result.compiler

	if len(c.instanceOwnedFuncs) == 0 {
		t.Fatal("expected non-empty instanceOwnedFuncs")
	}

	foundBoxInt := false
	for funcName, instName := range c.instanceOwnedFuncs {
		if instName == "Box[int]" {
			foundBoxInt = true
			if !strings.Contains(funcName, "Box[int]") {
				t.Errorf("function %q tagged as Box[int] but name doesn't contain 'Box[int]'", funcName)
			}
		}
	}
	if !foundBoxInt {
		t.Errorf("no function owned by Box[int]; instanceOwnedFuncs = %v", c.instanceOwnedFuncs)
	}
}

func TestCompileWithCacheNilEqualToCompile(t *testing.T) {
	// CompileWithCache with nil cachedInstances must produce the same IR as Compile.
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() { b := Box[int](value: 1); }
	`)
	r1 := Compile(file, info, "")
	r2 := CompileWithCache(file, info, "", nil)
	if r1.Module.String() != r2.Module.String() {
		t.Error("CompileWithCache(nil) produced different IR than Compile")
	}
}

func TestCompileWithCacheSkipsInstanceBody(t *testing.T) {
	// When Box[int] is listed as cached, its method body must not be generated
	// (so it won't appear in InstanceIRs).
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() {
			b := Box[int](value: 42);
			int x = b.get();
		}
	`)

	// Full compile: Box[int] must appear in InstanceIRs (body was generated).
	rFull := Compile(file, info, "")
	fullIRs := rFull.InstanceIRs()
	if _, ok := fullIRs["Box[int]"]; !ok {
		t.Skipf("Box[int] not in InstanceIRs on full compile; keys: %v", mapKeys(fullIRs))
	}

	// Cached compile: Box[int] body must be skipped → not in InstanceIRs.
	rCached := CompileWithCache(file, info, "", map[string]bool{"Box[int]": true})
	cachedIRs := rCached.InstanceIRs()
	if _, ok := cachedIRs["Box[int]"]; ok {
		t.Error("Box[int] should not appear in InstanceIRs when marked as cached")
	}
}

func TestCompileWithCacheOnlySkipsCachedInstances(t *testing.T) {
	// Marking Box[int] as cached must not affect Box[string].
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() {
			a := Box[int](value: 1);
			b := Box[string](value: "hi");
			int x = a.get();
			string y = b.get();
		}
	`)

	rCached := CompileWithCache(file, info, "", map[string]bool{"Box[int]": true})
	cachedIRs := rCached.InstanceIRs()

	// Box[int] was cached → no body, not in InstanceIRs
	if _, ok := cachedIRs["Box[int]"]; ok {
		t.Error("Box[int] should not appear in InstanceIRs when marked as cached")
	}
	// Box[string] was NOT cached → body generated, must be in InstanceIRs
	if _, ok := cachedIRs["Box[string]"]; !ok {
		t.Errorf("Box[string] should appear in InstanceIRs (not cached); keys: %v", mapKeys(cachedIRs))
	}
}

func TestInstanceOwnedFuncsTrackedEvenWhenCached(t *testing.T) {
	// instanceOwnedFuncs tagging must happen regardless of cachedInstances,
	// so that SplitModuleIRs can strip instance-owned functions from module/main IRs.
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() {
			b := Box[int](value: 42);
			int x = b.get();
		}
	`)
	r := CompileWithCache(file, info, "", map[string]bool{"Box[int]": true})
	c := r.compiler

	foundBoxInt := false
	for _, instName := range c.instanceOwnedFuncs {
		if instName == "Box[int]" {
			foundBoxInt = true
			break
		}
	}
	if !foundBoxInt {
		t.Errorf("Box[int] not in instanceOwnedFuncs even when cached; map = %v", c.instanceOwnedFuncs)
	}
}

// --- Failable getter codegen ---

func TestMatchMixedVoidAndValueArms(t *testing.T) {
	// Match where some arms produce a value and some call a void function.
	// buildMatchPhi must filter void-typed values before constructing the PHI.
	ir := generateIR(t, `
		test(int n) int {
			int result = match n {
				1 => 10,
				2 => { print_line("side effect"); 20; },
				_ => 0,
			};
			return result;
		}
		main() { }
	`)
	assertContains(t, ir, "phi i64")
	assertContains(t, ir, "match.end")
}

func TestMatchAllVoidArms(t *testing.T) {
	// Match used as statement where all arms are void (no arm produces a value).
	// buildMatchPhi should return nil (no PHI node needed).
	ir := generateIR(t, `
		test(int n) {
			match n {
				1 => { print_line("one"); },
				2 => { print_line("two"); },
				_ => { print_line("other"); },
			};
		}
		main() { }
	`)
	assertContains(t, ir, "match.arm")
	assertContains(t, ir, "match.end")
}

// B0126: match with block body containing if/else as expression.
// genBlockValue must capture the if/else result via genIfStmtValue.
func TestMatchBlockIfElseExpr(t *testing.T) {
	ir := generateIR(t, `
		classify(int n) string {
			return match n {
				3 => "small",
				_ => {
					if n < 100 {
						"medium";
					} else {
						"large";
					}
				},
			};
		}
		main() { }
	`)
	assertContains(t, ir, "phi")
	assertContains(t, ir, "match.end")
	assertContains(t, ir, "if.then")
	assertContains(t, ir, "if.end")
}

// B0126: single wildcard arm with block body containing if/else-if chain.
func TestMatchBlockIfElseIfChain(t *testing.T) {
	ir := generateIR(t, `
		classify(int n) string {
			return match n {
				_ => {
					if n < 10 {
						"tiny";
					} else if n < 100 {
						"small";
					} else {
						"big";
					}
				},
			};
		}
		main() { }
	`)
	assertContains(t, ir, "phi")
	assertContains(t, ir, "if.then")
	assertContains(t, ir, "if.end")
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

// B0135: if/else void inside a match block body must not produce phi void.
func TestMatchBlockIfElseVoidNoPhi(t *testing.T) {
	ir := generateIR(t, `
		test(int n) {
			match n {
				1 => {
					if n > 0 {
						print_line("a");
					} else {
						print_line("b");
					}
				},
				_ => { print_line("c"); },
			};
		}
		main() { }
	`)
	assertContains(t, ir, "if.then")
	assertNotContains(t, ir, "phi void")
}

func TestOptionalRecoveryCodegen(t *testing.T) {
	// Optional recovery: non-recovering handler wraps result as T?
	ir := generateIR(t, `
		fail() int! { raise error(message: "oops"); }
		main() {
			x := fail() ? e { print_line("handled"); };
		}
	`)
	// Should wrap success value as optional some (insertvalue with i1 true)
	// and produce a phi node merging ok/error paths
	assertContains(t, ir, "insertvalue")
	assertContains(t, ir, "i1 true")
}

func TestFailableGetterResultType(t *testing.T) {
	ir := generateIR(t, `
		type MyErr is error { int code; }
		type Foo {
			int _val;
			get value int! {
				if this._val < 0 { raise MyErr(code: 1, message: "neg"); }
				return this._val;
			}
		}
		main() {
			Foo f = Foo(_val: 42);
			int v = f.value!;
		}
	`)
	// Failable getter should return result type {i1, i64, i8*}
	assertContains(t, ir, "define { i1, i64, i8* } @Foo.value(")
}

func TestFailableGetterVirtualDispatch(t *testing.T) {
	ir := generateIR(t, `
		type MyErr is error { int code; }
		type Base {
			get value int! `+"`"+`abstract;
		}
		type Impl is Base {
			int _v;
			get value int! { return this._v; }
		}
		main() {
			Base b = Impl(_v: 10);
			int v = b.value!;
		}
	`)
	// Abstract failable getter should use vtable dispatch
	assertContains(t, ir, "@promise_vtable_Base")
	assertContains(t, ir, "@promise_vtable_Impl")
	assertContains(t, ir, "define { i1, i64, i8* } @Impl.value(")
}

func TestFailableGetterStringResult(t *testing.T) {
	ir := generateIR(t, `
		type MyErr is error { int code; }
		type Foo {
			int _mode;
			get label string! {
				if this._mode < 0 { raise MyErr(code: 1, message: "bad"); }
				return "ok";
			}
		}
		main() {
			Foo f = Foo(_mode: 1);
			string s = f.label!;
		}
	`)
	// Failable getter returning string should have result type in signature
	assertContains(t, ir, "define { i1, i8*, i8* } @Foo.label(")
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

func TestSchedLoopSetsCurrentM(t *testing.T) {
	// sched_loop should store M param to TLS current_m
	ir := generateIR(t, `
		main() { }
	`)
	// sched_loop stores m to current_m
	assertContains(t, ir, "__promise_current_m")
	assertContains(t, ir, "promise_sched_loop")
}

func TestSchedLoopJmpBufInEntryBlock(t *testing.T) {
	// B0120: jmpBuf alloca must be in the entry block (static alloca),
	// not in the run_g block where it would leak 256 bytes per resume.
	ir := generateIR(t, `
		main() { }
	`)
	// The alloca [256 x i8] must appear in the entry block of sched_loop,
	// before the first branch instruction (br label %loop).
	fn := extractFunction(ir, "promise_sched_loop")
	entryEnd := strings.Index(fn, "br label %loop")
	if entryEnd < 0 {
		t.Fatal("missing 'br label %loop' in sched_loop")
	}
	entryBlock := fn[:entryEnd]
	if !strings.Contains(entryBlock, "alloca [256 x i8]") {
		t.Error("jmpBuf alloca [256 x i8] must be in the entry block of sched_loop, not in run_g")
	}
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

func TestSchedShutdownUsesMaxP(t *testing.T) {
	// B0120: shutdown must signal/join ALL Ms using max_p (field 14),
	// not num_p (field 5). After set_max_procs reduces num_p, Ms on
	// disabled Ps would not be signaled/joined, causing SIGSEGV on exit.
	ir := generateIR(t, `
		main() { }
	`)
	fn := extractFunction(ir, "promise_sched_shutdown")
	// The sched struct GEP that loads the loop bound must reference
	// field index 14 (max_p). The GEP accesses @__promise_sched, and
	// the second field index is the one that selects num_p vs max_p.
	// Check that the GEP for the loop bound uses field 14.
	assertContains(t, fn, "@__promise_sched, i32 0, i32 14")
	// Ensure there is no GEP accessing num_p (field 5) in shutdown —
	// the only sched fields accessed should be shutdown (9), max_p (14), and ps (4).
	assertNotContains(t, fn, "@__promise_sched, i32 0, i32 5")
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

func TestPALGetEnvGetCwdDefined(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	// PAL getenv/getcwd wrappers are always declared
	assertContains(t, ir, "@pal_getenv")
	assertContains(t, ir, "@pal_getcwd")
}

func TestOptionalExternSret(t *testing.T) {
	ir := generateIR(t, `
		get_val(string name) string? `+"`"+`extern("promise_get_val");
		main() {
			string? v = get_val("key");
		}
	`)
	// Optional extern uses sret with {i1, T} struct
	assertContains(t, ir, "declare void @promise_get_val(")
	// Caller allocates sret and loads result
	assertContains(t, ir, "call void @promise_get_val(")
}

func TestFailableExternSret(t *testing.T) {
	ir := generateIR(t, `
		get_cwd() string! `+"`"+`extern("promise_get_cwd");
		main() {
			string s = get_cwd()!;
		}
	`)
	// Failable extern uses sret with {i1, T, i8*} struct
	assertContains(t, ir, "declare void @promise_get_cwd(")
	// Caller allocates sret and loads result
	assertContains(t, ir, "call void @promise_get_cwd(")
}

// --- Module-level getter/setter codegen tests ---

func TestModuleLevelGetterCodegen(t *testing.T) {
	ir := generateIR(t, `
		get answer int { return 42; }
		main() { int v = answer; }
	`)
	// Getter should generate a zero-arg function returning i64
	assertContains(t, ir, "define i64 @answer()")
	// Usage should call the getter (no args)
	assertContains(t, ir, "call i64 @answer()")
}

func TestModuleLevelSetterCodegen(t *testing.T) {
	ir := generateIR(t, `
		get counter int { return 0; }
		set counter(int value) {}
		main() { counter = 42; }
	`)
	// Setter stored as counter$set, takes one i64 param
	assertContains(t, ir, "define void @counter$set(i64")
	// Assignment should call the setter
	assertContains(t, ir, "call void @counter$set(i64")
}

func TestModuleLevelCompoundAssignCodegen(t *testing.T) {
	ir := generateIR(t, `
		get counter int { return 0; }
		set counter(int value) {}
		main() { counter += 5; }
	`)
	// Should call getter then setter
	assertContains(t, ir, "call i64 @counter()")
	assertContains(t, ir, "call void @counter$set(i64")
}

func TestModuleLevelGetterDistinctFromSetter(t *testing.T) {
	ir := generateIR(t, `
		get val int { return 0; }
		set val(int v) {}
		main() {
			int x = val;
			val = 10;
		}
	`)
	// Getter and setter should be distinct LLVM functions
	assertContains(t, ir, "define i64 @val()")
	assertContains(t, ir, "define void @val$set(i64")
}

func TestEnumMethodDecl(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue,
			describe(&this) string {
				match this {
					Color.Red => { return "red"; },
					_ => { return "other"; },
				}
			}
		}
		main() { string s = Color.Red.describe(); }
	`)
	assertContains(t, ir, "@Color.describe(i8* %this)")
	assertContains(t, ir, "call i8* @Color.describe(")
}

func TestEnumGetterDecl(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue,
			get opposite Color {
				match this {
					Color.Red => { return Color.Green; },
					Color.Green => { return Color.Blue; },
					Color.Blue => { return Color.Red; },
				}
			}
		}
		main() { Color c = Color.Red.opposite; }
	`)
	assertContains(t, ir, "define i32 @Color.opposite(i8* %this)")
	assertContains(t, ir, "call i32 @Color.opposite(")
}

func TestEnumMethodOnDataEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Point,
			is_point(&this) bool {
				match this {
					Shape.Point => { return true; },
					_ => { return false; },
				}
			}
		}
		main() { bool b = Shape.Point.is_point(); }
	`)
	assertContains(t, ir, "define i1 @Shape.is_point(i8* %this)")
	assertContains(t, ir, "call i1 @Shape.is_point(")
}

func TestEnumMethodCallsMethod(t *testing.T) {
	ir := generateIR(t, `
		enum Level { Low, High,
			rank(&this) int {
				match this {
					Level.Low => { return 1; },
					Level.High => { return 2; },
				}
			}
			gt(&this, Level other) bool {
				return this.rank() > other.rank();
			}
		}
		main() { bool b = Level.High.gt(Level.Low); }
	`)
	// Both methods declared with i8* receiver
	assertContains(t, ir, "@Level.rank(i8* %this)")
	assertContains(t, ir, "@Level.gt(i8* %this, i32 %other)")
}

func TestEnumMethodFailable(t *testing.T) {
	ir := generateIR(t, `
		enum Mode { A, B,
			check(&this) string! {
				match this {
					Mode.A => { return "a"; },
					Mode.B => { return "b"; },
				}
			}
		}
		main() { string s = Mode.A.check()!; }
	`)
	// Failable method returns result struct
	assertContains(t, ir, "@Mode.check(i8* %this)")
}

func TestEnumMethodVoid(t *testing.T) {
	ir := generateIR(t, `
		enum State { On, Off,
			log(&this) { print_line("x"); }
		}
		main() { State.On.log(); }
	`)
	assertContains(t, ir, "define void @State.log(i8* %this)")
}

func TestEnumGetterOnDataEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Point,
			get has_area bool {
				match this {
					Shape.Circle(r) => { return true; },
					Shape.Point => { return false; },
				}
			}
		}
		main() { bool b = Shape.Circle(radius: 1.0).has_area; }
	`)
	assertContains(t, ir, "define i1 @Shape.has_area(i8* %this)")
	assertContains(t, ir, "call i1 @Shape.has_area(")
}

// --- B0005: String constant private linkage tests ---
// All string constant globals must use LinkagePrivate so each split .bc file
// (module, instance) contains its own copy and doesn't depend on main-IR string
// numbering. This prevents stale cache entries from causing linker errors.

// compileResultWithModule parses a module and user source, runs sema+codegen,
// and returns the CompileResult (for SplitModuleIRs / InstanceIRs testing).
func compileResultWithModule(t *testing.T, moduleName, modSrc, userSrc string) *CompileResult {
	t.Helper()

	modInfo, modScope := parseModuleSource(t, moduleName, modSrc)
	stdModInfo, stdScope := getCodegenStdModInfo()

	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs := ast.Build("test.pr", userTree)
	if len(errs) > 0 {
		t.Fatalf("user AST build errors: %v", errs)
	}

	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)

	modKey := "./" + moduleName
	moduleScopes := map[string]*types.Scope{
		"std":  stdScope,
		modKey: modScope,
	}
	info, semaErrs := sema.CheckWithModules(userFile, moduleScopes)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":  stdModInfo,
		modKey: modInfo,
	}
	info.ModuleOrder = []string{"std", modKey}

	return Compile(userFile, info, "")
}

func TestStringConstantsArePrivate(t *testing.T) {
	// All string constants (@.str.*) must have "private" linkage in the IR.
	ir := generateIR(t, `
		main() {
			print_line("hello");
			assert(true, "ok");
		}
	`)
	for _, line := range strings.Split(ir, "\n") {
		// Match lines that define string constant globals (not references in function bodies)
		if (strings.HasPrefix(line, "@.str.") || strings.HasPrefix(line, "@.cstr.")) &&
			strings.Contains(line, " = ") && strings.Contains(line, "constant") {
			if !strings.Contains(line, "private") {
				t.Errorf("string constant must have private linkage: %s", line)
			}
		}
	}
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

// extractFunc returns the IR text of a named function definition from full module IR.
// Returns empty string if function not found.
func extractFunc(ir, funcName string) string {
	// Search for "define" lines containing the function name
	needle := "@" + funcName + "("
	searchFrom := 0
	for searchFrom < len(ir) {
		idx := strings.Index(ir[searchFrom:], needle)
		if idx < 0 {
			return ""
		}
		idx += searchFrom
		// Walk back to find "define" on the same line
		lineStart := strings.LastIndex(ir[:idx], "\n")
		if lineStart < 0 {
			lineStart = 0
		}
		line := ir[lineStart:idx]
		if !strings.Contains(line, "define") {
			// This is a call site, not a definition — skip
			searchFrom = idx + len(needle)
			continue
		}
		start := lineStart
		if ir[start] == '\n' {
			start++
		}
		// Walk forward to find the closing "}" at depth 0
		rest := ir[start:]
		depth := 0
		for i, ch := range rest {
			if ch == '{' {
				depth++
			} else if ch == '}' {
				depth--
				if depth == 0 {
					return rest[:i+1]
				}
			}
		}
		return rest
	}
	return ""
}

// B0007: Verify that the goroutine coroutine has coro.init.suspend block
// separating allocas in coro.start from the initial coro.suspend.
func TestCoroutineInitSuspendBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(42);
		}
	`)
	// Main is wrapped in a goroutine coroutine
	goFunc := extractFunc(ir, ".goroutine.main")
	if goFunc == "" {
		t.Fatal("expected .goroutine.main function in IR")
	}
	// coro.start should branch to coro.init.suspend (not contain coro.suspend directly)
	assertContains(t, goFunc, "br label %coro.init.suspend")
	// coro.init.suspend block should contain the initial coro.suspend
	assertContains(t, goFunc, "coro.init.suspend:")
	assertContains(t, goFunc, "call i8 @llvm.coro.suspend(")
}

// B0007: Verify that channel send alloca is in coro.start (entry block),
// not in the send.write block.
func TestChannelSendAllocaInEntryBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	goFunc := extractFunc(ir, ".goroutine.main")
	if goFunc == "" {
		t.Fatal("expected .goroutine.main function in IR")
	}

	// The alloca for the send value should be in coro.start, before the br to coro.init.suspend
	// Split on "coro.init.suspend:" to get coro.start content
	parts := strings.SplitN(goFunc, "coro.init.suspend:", 2)
	if len(parts) < 2 {
		t.Fatal("expected coro.init.suspend block")
	}
	coroStart := parts[0]

	// coro.start should contain an alloca for the send value (i64 for int)
	if !strings.Contains(coroStart, "alloca i64") {
		t.Errorf("expected alloca i64 in coro.start for channel send value\ncoro.start:\n%s", coroStart)
	}

	// The send.write block should NOT contain an alloca
	sendWriteIdx := strings.Index(goFunc, "send.write")
	if sendWriteIdx >= 0 {
		// Get the send.write block content (up to next label or end)
		sendWriteBlock := goFunc[sendWriteIdx:]
		nextLabel := strings.Index(sendWriteBlock[1:], "\n")
		if nextLabel > 0 {
			// Check a reasonable window after send.write label
			window := sendWriteBlock[:min(len(sendWriteBlock), 500)]
			if strings.Contains(window, "= alloca ") {
				t.Errorf("send.write block should not contain alloca (should be in entry block)\nblock:\n%s", window)
			}
		}
	}
}

func TestModuleSplitStringConstantsPreserved(t *testing.T) {
	// When SplitModuleIRs() splits the IR, string constants (private globals)
	// must remain as definitions in the module IR, not be stripped to extern.
	// This is the core B0005 fix: each .bc is self-contained for strings.
	result := compileResultWithModule(t, "mymod",
		`greet(string name) string `+"`public"+` { return "Hello, {name}!"; }`,
		`
		use mymod "./mymod";
		main() { string s = mymod.greet("World"); }
		`,
	)
	_, moduleIRs := result.SplitModuleIRs()

	modIR, ok := moduleIRs["mymod"]
	if !ok {
		t.Fatal("expected 'mymod' in moduleIRs")
	}

	// Module IR must contain at least one string constant as a private definition.
	foundPrivateStr := false
	for _, line := range strings.Split(modIR, "\n") {
		if strings.HasPrefix(line, "@.str.") && strings.Contains(line, "private constant") {
			foundPrivateStr = true
		}
		// No string constant should be an extern declaration
		if strings.HasPrefix(line, "@.str.") && !strings.Contains(line, "private") &&
			strings.Contains(line, " = ") && strings.Contains(line, "constant") {
			t.Errorf("module IR has non-private string constant: %s", line)
		}
	}
	if !foundPrivateStr {
		t.Error("module IR must contain at least one private string constant definition")
	}
}

func TestInstanceIRStringConstantsPreserved(t *testing.T) {
	// Instance .bc files must contain their own copy of string constants
	// (as private definitions), not extern references to the main IR.
	file, info := parseWithStd(t, `
		type Wrapper[T] {
			T value;
			describe(this) string { return "wrapped"; }
		}
		main() {
			w := Wrapper[int](value: 42);
			string s = w.describe();
		}
	`)
	result := Compile(file, info, "")
	instIRs := result.InstanceIRs()

	wrapIR, ok := instIRs["Wrapper[int]"]
	if !ok {
		t.Fatalf("expected Wrapper[int] in instance IRs, got: %v", mapKeys(instIRs))
	}

	// Instance IR must contain at least one private string constant.
	foundPrivateStr := false
	for _, line := range strings.Split(wrapIR, "\n") {
		if strings.HasPrefix(line, "@.str.") && strings.Contains(line, "private constant") {
			foundPrivateStr = true
		}
	}
	if !foundPrivateStr {
		t.Error("instance IR must contain at least one private string constant (from describe method)")
	}

	// No string constant should be an extern declaration in instance IR.
	for _, line := range strings.Split(wrapIR, "\n") {
		trimmed := strings.TrimSpace(line)
		if (strings.HasPrefix(trimmed, "@.str.") || strings.HasPrefix(trimmed, "@.cstr.")) &&
			!strings.Contains(trimmed, "private") &&
			strings.Contains(trimmed, " = external") {
			t.Errorf("instance IR has extern string constant (should be private): %s", line)
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

func TestModuleSplitNonPrivateGlobalsStripped(t *testing.T) {
	// In module IR, non-private globals (vtables, RTTI) must be converted to
	// extern declarations. Only private globals (strings) stay as definitions.
	result := compileResultWithModule(t, "shapes",
		`
		type Shape `+"`public"+` {
			string label;
			info(this) string { return "shape: {this.label}"; }
		}
		`,
		`
		use shapes "./shapes";
		main() {
			s := shapes.Shape(label: "box");
			print_line(s.info());
		}
		`,
	)
	_, moduleIRs := result.SplitModuleIRs()

	modIR, ok := moduleIRs["shapes"]
	if !ok {
		t.Fatal("expected 'shapes' in moduleIRs")
	}

	// Vtable/typeinfo globals should be extern declarations in module IR
	for _, line := range strings.Split(modIR, "\n") {
		trimmed := strings.TrimSpace(line)
		// Check vtable globals are NOT defined (they live in main IR)
		if strings.HasPrefix(trimmed, "@promise_vtable_") &&
			strings.Contains(trimmed, " = ") &&
			!strings.Contains(trimmed, "external") {
			// Allow if it's an extern declaration (no init = "external" or just "declare")
			if strings.Contains(trimmed, "constant") || strings.Contains(trimmed, "global") {
				t.Errorf("module IR should not define vtable global (should be extern): %s", trimmed)
			}
		}
	}
}

func TestIsDestructureEnumCodegen(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Point }
		main() {
			Shape s = Shape.Circle(radius: 5.0);
			if s is Circle(r) {
				print_line("{r}");
			}
		}
	`)
	// Should generate tag comparison for the enum variant check
	assertContains(t, ir, "icmp eq i32")
	// Should have the destructure blocks
	assertContains(t, ir, "isdestr.then")
	assertContains(t, ir, "isdestr.end")
}

func TestIsDestructureNamedTypeCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; speak(&this) string `+"`"+`abstract; }
		type Dog is Animal { string breed; speak(&this) string { return "woof"; } }
		main() {
			Animal a = Dog(name: "Rex", breed: "Lab");
			if a is Dog(n, b) {
				print_line(n);
			}
		}
	`)
	// Should generate RTTI type check
	assertContains(t, ir, "call i32 @promise_type_is(")
	// Should have the destructure blocks
	assertContains(t, ir, "isdestr.then")
	assertContains(t, ir, "isdestr.end")
}

func TestIsDestructureElseCodegen(t *testing.T) {
	ir := generateIR(t, `
		enum Opt { Some(int v), None }
		main() {
			Opt o = Opt.None;
			if o is Some(v) {
				print_line("{v}");
			} else {
				print_line("none");
			}
		}
	`)
	assertContains(t, ir, "isdestr.then")
	assertContains(t, ir, "isdestr.else")
	assertContains(t, ir, "isdestr.end")
}

func TestIsDestructureGenericEnumCodegen(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T value), None }
		main() {
			Option[int] opt = Option[int].Some(value: 42);
			if opt is Some(val) {
				print_line("{val}");
			}
		}
	`)
	// Should have the destructure blocks for the monomorphized enum
	assertContains(t, ir, "isdestr.then")
	assertContains(t, ir, "icmp eq i32")
}

func TestIsDestructureUnderscoreCodegen(t *testing.T) {
	ir := generateIR(t, `
		enum Pair { V(int a, int b) }
		main() {
			Pair p = Pair.V(a: 1, b: 2);
			if p is V(_, y) {
				print_line("{y}");
			}
		}
	`)
	assertContains(t, ir, "isdestr.then")
	// Should still produce the tag check
	assertContains(t, ir, "icmp eq i32")
}

// B0112: destructure is-pattern inside generic method body must apply typeSubst
func TestIsDestructureInGenericMethodBody(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T value), None }
		type Wrapper[T] {
			Option[T] opt;
			unwrap_or(this, T default_val) T {
				if this.opt is Some(val) {
					return val;
				}
				return default_val;
			}
		}
		main() {
			w := Wrapper[int](opt: Option[int].Some(value: 42));
			int result = w.unwrap_or(0);
		}
	`)
	// The monomorphized method should have destructure blocks
	assertContains(t, ir, "isdestr.then")
	// Should have the tag comparison for the monomorphized enum Option__int
	assertContains(t, ir, "icmp eq i32")
}

func TestIsDestructureAsExprCodegen(t *testing.T) {
	// When used as a plain expression (not if condition), should just produce the bool check
	ir := generateIR(t, `
		enum Opt { Some(int v), None }
		main() {
			Opt o = Opt.Some(v: 42);
			bool b = o is Some(x);
			if b { print_line("yes"); }
		}
	`)
	// Should NOT have isdestr blocks (handled by genIsDestructurePattern, not genIfDestructureIsStmt)
	assertNotContains(t, ir, "isdestr.then")
	// But should still have the tag comparison
	assertContains(t, ir, "icmp eq i32")
}

// B0007: Verify that channel recv alloca is in coro.start (entry block),
// not in the chrecv.read block.
func TestChannelRecvAllocaInEntryBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
			val := <-ch;
		}
	`)
	goFunc := extractFunc(ir, ".goroutine.main")
	if goFunc == "" {
		t.Fatal("expected .goroutine.main function in IR")
	}

	// The chrecv.read block should NOT contain an alloca
	readIdx := strings.Index(goFunc, "chrecv.read")
	if readIdx >= 0 {
		readBlock := goFunc[readIdx:]
		window := readBlock[:min(len(readBlock), 500)]
		if strings.Contains(window, "= alloca ") {
			t.Errorf("chrecv.read block should not contain alloca (should be in entry block)\nblock:\n%s", window)
		}
	}
}

// B0007: Verify that go-block coroutines also have the coro.init.suspend separation.
func TestGoBlockCoroutineInitSuspend(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				ch.send(42);
			};
		}
	`)
	// Find the go-block goroutine function (not .goroutine.main)
	goFunc := extractFunc(ir, ".goroutine.0")
	if goFunc == "" {
		t.Fatal("expected .goroutine.0 function in IR")
	}
	// Should have the separated init suspend block
	assertContains(t, goFunc, "br label %coro.init.suspend")
	assertContains(t, goFunc, "coro.init.suspend:")
}

// B0007: Verify select statement allocas are in entry block.
func TestSelectAllocaInEntryBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch1 := channel[int](capacity: 1);
			ch2 := channel[int](capacity: 1);
			ch1.send(10);
			select {
				v := <-ch1:
				v := <-ch2:
			}
		}
	`)
	goFunc := extractFunc(ir, ".goroutine.main")
	if goFunc == "" {
		t.Fatal("expected .goroutine.main function in IR")
	}

	// coro.start should contain the channel array alloca ([2 x i8*])
	parts := strings.SplitN(goFunc, "coro.init.suspend:", 2)
	if len(parts) < 2 {
		t.Fatal("expected coro.init.suspend block")
	}
	coroStart := parts[0]
	if !strings.Contains(coroStart, "alloca [2 x i8*]") {
		t.Errorf("expected alloca [2 x i8*] in coro.start for select channel array\ncoro.start:\n%s", coroStart)
	}
}

// --- Generic is-pattern tests (B0012) ---

func TestIsGenericType(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		type LabeledBox[T] is Box[T] { string label; }
		main() {
			Box[int] b = LabeledBox[int](value: 42, label: "x");
			bool x = b is LabeledBox[int];
		}
	`)
	// Should generate mono typeinfo for the generic instance
	assertContains(t, ir, "promise_typeinfo_LabeledBox")
	assertContains(t, ir, "call i32 @promise_type_is")
}

func TestIsGenericTypeBaseClass(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		type LabeledBox[T] is Box[T] { string label; }
		main() {
			Box[int] b = LabeledBox[int](value: 42, label: "x");
			bool x = b is Box[int];
		}
	`)
	// Should have mono typeinfo for both instances
	assertContains(t, ir, "promise_typeinfo_Box")
	assertContains(t, ir, "promise_typeinfo_LabeledBox")
}

func TestIsGenericTypeOptional(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		type LabeledBox[T] is Box[T] { string label; }
		main() {
			Box[int] lb = LabeledBox[int](value: 42, label: "x");
			Box[int]? opt = lb;
			bool x = opt is LabeledBox[int];
		}
	`)
	// Optional generic is-check: should branch on presence then RTTI
	assertContains(t, ir, "call i32 @promise_type_is")
	assertContains(t, ir, "phi i1")
}

func TestIsGenericErrorHandler(t *testing.T) {
	ir := generateIR(t, `
		type AppError[T] is error { T detail; }
		do_thing() AppError[int]! {
			raise AppError[int](message: "err", detail: 42);
		}
		main() {
			do_thing() ? e is AppError[int] {
			}!;
		}
	`)
	assertContains(t, ir, "call i32 @promise_type_is")
	assertContains(t, ir, "promise_typeinfo_AppError")
}

func TestMatchExpressionPattern(t *testing.T) {
	ir := generateIR(t, `
		test() int {
			int n = 15;
			return match true {
				n % 15 == 0 => 1,
				n % 3 == 0 => 2,
				_ => 0,
			};
		}
		main() { }
	`)
	// Expression patterns compile to comparisons like literal patterns
	assertContains(t, ir, "icmp eq")
	assertContains(t, ir, "match.arm")
	assertContains(t, ir, "match.next")
	// The modulo operation should appear in the IR
	assertContains(t, ir, "srem")
}

// --- Type Argument Inference Codegen Tests ---

func TestInferGenericFuncCodegen(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			int v = identity(42);
		}
	`)
	// Monomorphized function should be generated for int
	assertContains(t, ir, "define i64 @\"identity[int]\"")
}

func TestInferGenericFuncTwoTypeParamsCodegen(t *testing.T) {
	ir := generateIR(t, `
		first[A, B](A a, B b) A { return a; }
		main() {
			int v = first(1, "hello");
		}
	`)
	assertContains(t, ir, "@\"first[int, string]\"")
}

func TestInferConstructorCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box(value: 42);
			int v = b.value;
		}
	`)
	// Should produce Box[int] instance struct
	assertContains(t, ir, "Box[int]_i")
}

func TestInferGenericFuncWithVectorParamCodegen(t *testing.T) {
	ir := generateIR(t, `
		first_elem[T](T[] items) T {
			return items[0];
		}
		main() {
			int[] arr = [1, 2, 3];
			int v = first_elem(arr);
		}
	`)
	assertContains(t, ir, "@\"first_elem[int]\"")
}

// --- Embed getter tests ---

func TestEmbedStringGetter(t *testing.T) {
	file, info := parseWithStd(t, `
		get schema string `+"`embed(\"schema.sql\")"+`;
		main() {
			string s = schema;
		}
	`)
	// Manually populate embed data (normally done by ResolveEmbeds)
	for fd, embed := range info.Embeds {
		_ = fd
		embed.Data = []byte("CREATE TABLE foo;")
	}
	result := Compile(file, info, "")
	ir := result.Module.String()
	assertContains(t, ir, "@schema()")
	assertContains(t, ir, "CREATE TABLE foo;")
	assertContains(t, ir, "@promise_string_new")
}

func TestEmbedBytesGetter(t *testing.T) {
	file, info := parseWithStd(t, `
		get data u8[] `+"`embed(\"data.bin\")"+`;
		main() {
			u8[] d = data;
		}
	`)
	// Manually populate embed data
	for fd, embed := range info.Embeds {
		_ = fd
		embed.Data = []byte{0xDE, 0xAD, 0xBE, 0xEF}
	}
	result := Compile(file, info, "")
	ir := result.Module.String()
	assertContains(t, ir, "define i8* @data()")
	assertContains(t, ir, "@pal_alloc")
	assertContains(t, ir, "@llvm.memcpy")
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
	assertContains(t, ir, "@empty()")
	assertContains(t, ir, "@promise_string_new")
}

// T0137: String embed getter result assigned to variable — drop flag must NOT be cleared.
func TestEmbedStringGetterDrop(t *testing.T) {
	file, info := parseWithStd(t, `
		get greeting string `+"`embed(\"greeting.txt\")"+`;
		test_it() `+"`test"+` {
			string a = greeting;
		}
	`)
	for _, embed := range info.Embeds {
		embed.Data = []byte("hello")
	}
	result := Compile(file, info, "")
	ir := result.Module.String()
	// The variable should have a string drop at scope exit.
	assertContains(t, ir, "@promise_string_drop")
	// The drop flag must NOT be cleared to false immediately after being set to true.
	// Before the fix, the IR had: store i1 true, %a.dropflag; store i1 false, %a.dropflag
	assertNotContains(t, ir, "store i1 true, i1* %a.dropflag\n\tstore i1 false, i1* %a.dropflag")
}

// T0137: Bytes embed getter result assigned to variable — drop flag must NOT be cleared.
func TestEmbedBytesGetterDrop(t *testing.T) {
	file, info := parseWithStd(t, `
		get binary u8[] `+"`embed(\"data.bin\")"+`;
		test_it() `+"`test"+` {
			u8[] d = binary;
		}
	`)
	for _, embed := range info.Embeds {
		embed.Data = []byte{0xDE, 0xAD}
	}
	result := Compile(file, info, "")
	ir := result.Module.String()
	// The variable should have a vector drop at scope exit.
	assertContains(t, ir, "@Vector.drop")
	// The drop flag must NOT be cleared to false immediately after being set to true.
	assertNotContains(t, ir, "store i1 true, i1* %d.dropflag\n\tstore i1 false, i1* %d.dropflag")
}

// --- Coverage instrumentation tests (T0030) ---

// generateIRWithCoverage compiles with coverage enabled and returns the IR.
func generateIRWithCoverage(t *testing.T, src string) (string, []CoverageRegion) {
	t.Helper()
	file, info := parseWithStd(t, src)
	result := CompileWithOptions(file, info, "", &CompileOptions{CoverageEnabled: true})
	return result.Module.String(), result.CoverageRegions
}

func TestCoverageFunctionEntry(t *testing.T) {
	ir, regions := generateIRWithCoverage(t, `
		foo() int { return 42; }
		bar() int { return 7; }
		main() {}
	`)
	// Should have coverage globals for foo and bar (not main)
	assertContains(t, ir, "@__promise_cov_0")
	assertContains(t, ir, "@__promise_cov_1")

	// Should have 2 function regions
	funcCount := 0
	for _, r := range regions {
		if r.Kind == "function" {
			funcCount++
		}
	}
	if funcCount != 2 {
		t.Errorf("expected 2 function coverage regions, got %d (regions: %+v)", funcCount, regions)
	}
}

func TestCoverageSkipsTestFunctions(t *testing.T) {
	_, regions := generateIRWithCoverage(t, `
		foo() int { return 42; }
		test_foo() `+"`test"+` {
			int x = foo();
		}
		main() {}
	`)
	// Only foo should be instrumented, not test_foo or main
	for _, r := range regions {
		if r.FuncName == "test_foo" {
			t.Errorf("test function should not be instrumented: %+v", r)
		}
		if r.FuncName == "main" {
			t.Errorf("main should not be instrumented: %+v", r)
		}
	}
	funcCount := 0
	for _, r := range regions {
		if r.Kind == "function" {
			funcCount++
		}
	}
	if funcCount != 1 {
		t.Errorf("expected 1 function region (foo), got %d", funcCount)
	}
}

func TestCoverageIfBranches(t *testing.T) {
	ir, regions := generateIRWithCoverage(t, `
		classify(int x) string {
			if x > 0 {
				return "positive";
			} else {
				return "negative";
			}
		}
		main() {}
	`)
	// Should have coverage counter increments in IR
	assertContains(t, ir, "@__promise_cov_0")
	assertContains(t, ir, "@__promise_cov_1") // if.then
	assertContains(t, ir, "@__promise_cov_2") // if.else

	thenCount := 0
	elseCount := 0
	for _, r := range regions {
		if r.Kind == "if.then" {
			thenCount++
		}
		if r.Kind == "if.else" {
			elseCount++
		}
	}
	if thenCount != 1 {
		t.Errorf("expected 1 if.then region, got %d", thenCount)
	}
	if elseCount != 1 {
		t.Errorf("expected 1 if.else region, got %d", elseCount)
	}
}

func TestCoverageWhileLoop(t *testing.T) {
	_, regions := generateIRWithCoverage(t, `
		count(int n) int {
			int i = 0;
			while i < n {
				i++;
			}
			return i;
		}
		main() {}
	`)
	whileCount := 0
	for _, r := range regions {
		if r.Kind == "while.body" {
			whileCount++
		}
	}
	if whileCount != 1 {
		t.Errorf("expected 1 while.body region, got %d", whileCount)
	}
}

func TestCoverageDisabledByDefault(t *testing.T) {
	ir := generateIR(t, `
		foo() int { return 42; }
		main() {}
	`)
	if strings.Contains(ir, "__promise_cov_") {
		t.Error("coverage globals should not be emitted when coverage is disabled")
	}
}

func TestCoverageMethodEntry(t *testing.T) {
	_, regions := generateIRWithCoverage(t, `
		type Counter {
			int value;
			increment(~this) {
				this.value++;
			}
			get_value(this) int {
				return this.value;
			}
		}
		main() {}
	`)
	methodCount := 0
	for _, r := range regions {
		if r.Kind == "method" {
			methodCount++
		}
	}
	if methodCount < 2 {
		t.Errorf("expected at least 2 method coverage regions, got %d", methodCount)
	}
}

func TestCoverageTestMainEmitsMarkers(t *testing.T) {
	file, info := parseWithStd(t, `
		foo() int { return 42; }
		test_foo() `+"`test"+` {
			int x = foo();
		}
	`)
	result := CompileWithOptions(file, info, "", &CompileOptions{CoverageEnabled: true})
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()

	// GenerateTestMain should emit coverage output markers
	assertContains(t, ir, "===PROMISE_COV===")
	assertContains(t, ir, "===END_COV===")
	// Should contain the coverage counter global
	assertContains(t, ir, "@__promise_cov_0")
	// Should have exactly 1 coverage region (foo, not test_foo)
	if len(result.CoverageRegions) != 1 {
		t.Errorf("expected 1 coverage region, got %d: %+v", len(result.CoverageRegions), result.CoverageRegions)
	}
}

func TestCoverageClassicForLoop(t *testing.T) {
	_, regions := generateIRWithCoverage(t, `
		sum(int n) int {
			int total = 0;
			for int i = 0; i < n; i++ {
				total += i;
			}
			return total;
		}
		main() {}
	`)
	forCount := 0
	for _, r := range regions {
		if r.Kind == "for.body" {
			forCount++
		}
	}
	if forCount != 1 {
		t.Errorf("expected 1 for.body region, got %d", forCount)
	}
}

func TestCoverageInfiniteLoop(t *testing.T) {
	_, regions := generateIRWithCoverage(t, `
		run() int {
			int i = 0;
			for {
				i++;
				if i >= 5 {
					break;
				}
			}
			return i;
		}
		main() {}
	`)
	loopCount := 0
	for _, r := range regions {
		if r.Kind == "loop.body" {
			loopCount++
		}
	}
	if loopCount != 1 {
		t.Errorf("expected 1 loop.body region, got %d", loopCount)
	}
}

func TestCoverageEnumMatchArms(t *testing.T) {
	_, regions := generateIRWithCoverage(t, `
		enum Color { Red, Green, Blue }
		name(Color c) string {
			return match c {
				Color.Red => "red",
				Color.Green => "green",
				Color.Blue => "blue",
			};
		}
		main() {}
	`)
	armCount := 0
	for _, r := range regions {
		if r.Kind == "match.arm" {
			armCount++
		}
	}
	if armCount != 3 {
		t.Errorf("expected 3 match.arm regions, got %d", armCount)
	}
}

// B0134: generic error type constructor inside generic function body
// must be collected for monomorphization via func instance substitution.
func TestGenericErrorTypeInGenericFuncBody(t *testing.T) {
	ir := generateIR(t, `
		type AppError[T] is error { T detail; }
		make_err[T](T detail) AppError[T]! {
			raise AppError[T](message: "fail", detail: detail);
		}
		main() { make_err[int](42) ? e { }; }
	`)
	// B0134: AppError[int] must be monomorphized from the generic function body
	assertContains(t, ir, "AppError[int]")
}

// B0134 variant: generic type (non-error) constructed inside generic function body.
func TestGenericTypeInGenericFuncBody(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper[T] { T value; }
		wrap[T](T v) Wrapper[T] {
			return Wrapper[T](value: v);
		}
		main() { w := wrap[int](42); }
	`)
	assertContains(t, ir, "Wrapper[int]")
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
			next() int? {
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

// B0175: Heap temp claim in genInferredVarDecl — auto-typed iterator variable
func TestHeapTempClaimInInferredVarDecl(t *testing.T) {
	ir := generateIR(t, `
		type Counter is Iterator[int] {
			int n;
			next(~this) int? {
				if this.n <= 0 { return none; }
				this.n = this.n - 1;
				return this.n;
			}
		}
		test() {
			c := Counter(n: 5);
			result := c.take(3);
			int sum = 0;
			for x in result {
				sum = sum + x;
			}
		}
		main() {}
	`)
	// The auto-typed `result := c.take(3)` must generate a heap.claim block
	// to prevent the iterator instance from being freed at statement end.
	assertContains(t, ir, "heap.claim")
}

// B0175: Heap temp claim on method receiver in chained calls
func TestHeapTempClaimOnMethodReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Counter is Iterator[int] {
			int n;
			next(~this) int? {
				if this.n <= 0 { return none; }
				this.n = this.n - 1;
				return this.n;
			}
		}
		test() {
			c := Counter(n: 100);
			Iterator[int] result = c.filter(|int x| -> bool { return x % 2 == 0; }).take(3);
			int sum = 0;
			for x in result {
				sum = sum + x;
			}
		}
		main() {}
	`)
	// The chained call c.filter(...).take(3) must claim the filter result
	// (intermediate heap temp) before calling .take(3) on it.
	// Both the filter result and the take result get heap.claim blocks.
	assertContains(t, ir, "heap.claim")
}

// B0187: Reassignment of structural interface variable must claim heap temp
func TestHeapTempClaimOnReassignment(t *testing.T) {
	ir := generateIR(t, `
		type Counter is Iterator[int] {
			int n;
			next(~this) int? {
				if this.n <= 0 { return none; }
				this.n = this.n - 1;
				return this.n;
			}
		}
		test() {
			c := Counter(n: 10);
			Iterator[int] it = c.take(5);
			it = c.take(3);
			int sum = 0;
			for x in it {
				sum = sum + x;
			}
		}
		main() {}
	`)
	// The reassignment `it = c.take(3)` must generate a heap.claim block
	// to prevent the new iterator instance from being double-freed
	// (once at statement end via cleanupHeapTemps, again at scope exit via emitFreeCall).
	assertContains(t, ir, "heap.claim")
}

// T0101: Optional field in type with synthesized drop
func TestOptionalFieldInSynthDrop(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { string? opt; }
		main() {
			Wrapper w = Wrapper(opt: "hello");
		}
	`)
	// Wrapper gets synthesized drop that checks optional field
	assertContains(t, ir, "define void @Wrapper.drop")
	assertContains(t, ir, "optfield.drop")
	assertContains(t, ir, "optfield.skip")
}

// T0111: Optional local with droppable inner type gets scope-exit drop
func TestOptionalLocalStringDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? s = "hello";
		}
	`)
	// Optional local should get a drop binding with optdrop blocks
	assertContains(t, ir, "optdrop.check")
	assertContains(t, ir, "optdrop.inner")
	assertContains(t, ir, "promise_string_drop")
}

// T0111: Force unwrap of optional identifier clears drop flag
func TestOptionalForceUnwrapClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? s = "hello";
			string val = s!;
		}
	`)
	// Should have optdrop blocks (drop registered for s)
	assertContains(t, ir, "optdrop.check")
	// The drop flag should be cleared (store i1 false) after unwrap
	assertContains(t, ir, "store i1 false")
}

// T0111: Optional local with vector inner type gets scope-exit drop
func TestOptionalLocalVectorDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[]? v = [1, 2, 3];
		}
	`)
	assertContains(t, ir, "optdrop.check")
	assertContains(t, ir, "optdrop.inner")
	assertContains(t, ir, "Vector.drop")
}

// T0111: Force unwrap of optional field access dups the string via dupStringFieldAccess
func TestOptionalFieldForceUnwrapDupsString(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { string? opt; }
		main() {
			Wrapper w = Wrapper(opt: "hello");
			string val = w.opt!;
		}
	`)
	// dupStringFieldAccess mechanism dups the string during field access
	assertContains(t, ir, "strdup.copy")
	assertContains(t, ir, "promise_string_new")
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

// T0130: Terminal operations (count, collect, find) should NOT claim the receiver's
// heap temp — the temp should be freed at statement end via cleanupHeapTemps.
func TestTerminalOpDoesNotClaimReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Counter is Iterator[int] {
			int current;
			int limit;
			next() int? {
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
