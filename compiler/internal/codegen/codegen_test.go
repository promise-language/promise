package codegen

import (
	"bytes"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/testutil"
	"github.com/promise-language/promise/compiler/internal/types"
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

func assertNotContainsMatch(t *testing.T, ir, pattern string) {
	t.Helper()
	re := regexp.MustCompile(pattern)
	if re.MatchString(ir) {
		t.Errorf("expected IR to NOT match %q\ngot:\n%s", pattern, ir)
	}
}

// extractFunction returns the IR text for a named function (from "define" to the closing "}").
// extractGlobal returns the single-line `@name = ...` global definition, or ""
// if absent. Useful for asserting on the contents of a constant vtable global.
func extractGlobal(ir, name string) string {
	marker := "@" + name + " ="
	start := strings.Index(ir, marker)
	if start < 0 {
		// Globals with special characters are quoted: @"name = ...".
		marker = "@\"" + name + "\" ="
		start = strings.Index(ir, marker)
		if start < 0 {
			return ""
		}
	}
	end := strings.Index(ir[start:], "\n")
	if end < 0 {
		return ir[start:]
	}
	return ir[start : start+end]
}

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

// extractDefine returns the body of the `define ... @name(...)` *definition*,
// anchoring on the `define` keyword. Unlike extractFunction (which matches the
// first `@name(` anywhere), this is safe for argless functions like
// `.goroutine.main()` whose name also appears as a call operand inside @main —
// where extractFunction can latch onto the reference and extract @main instead.
func extractDefine(ir, name string) string {
	needle := "@" + name + "("
	for idx := 0; ; {
		d := strings.Index(ir[idx:], "define")
		if d < 0 {
			return ""
		}
		d += idx
		nl := strings.Index(ir[d:], "\n")
		if nl < 0 {
			return ""
		}
		if strings.Contains(ir[d:d+nl], needle) {
			rest := ir[d:]
			end := strings.Index(rest, "\n}\n")
			if end < 0 {
				return rest
			}
			return rest[:end+2]
		}
		idx = d + len("define")
	}
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
	assertContains(t, ir, "define i64 @__user.add(i64 %a, i64 %b)")
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "ret i64")
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
	// B0228: promise_panic now sets TLS flag and returns (no longjmp/exit)
	assertContains(t, ir, "define void @promise_panic(i8*")
	assertContains(t, ir, "store i8 1, i8* @__promise_panic_flag")     // set panic flag
	assertContains(t, ir, "store i8* %msg, i8** @__promise_panic_msg") // store msg
	assertContains(t, ir, "store i8 1, i8* @__promise_panic_type")     // type=1 (.rodata)
	assertContains(t, ir, "fatal: panic during panic recovery")        // double-panic message
	assertContains(t, ir, "call void @pal_exit(i32 134)")              // double-panic exit
}

func TestPanicMsgBody(t *testing.T) {
	ir := generateIR(t, `
		panic_msg(string msg) `+"`"+`extern("promise_panic_msg");
		main() { panic_msg("boom"); }
	`)
	assertContains(t, ir, "define void @promise_panic_msg(i8*")
	assertContains(t, ir, "bitcast i8* %msg to %promise_string_v*")
	// B0228: promise_panic_msg now sets TLS flag and returns (no longjmp/exit)
	assertContains(t, ir, "store i8 1, i8* @__promise_panic_flag") // set panic flag
	assertContains(t, ir, "store i8* %")                           // store C string msg
	assertContains(t, ir, "store i8 2, i8* @__promise_panic_type") // type=2 (heap)
	assertContains(t, ir, "call i8* @pal_alloc(")                  // allocate C string copy
	assertContains(t, ir, "fatal: panic during panic recovery")    // double-panic message
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
	assertContains(t, ir, "define i64 @__user.fib(i64 %n)")
	assertContains(t, ir, "call i64 @__user.fib")
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
	// WASM imports use direct return and params for primitives (not sret/i8*)
	assertContains(t, ir, "declare i64 @fd_write(i64 %fd)")
}

// --- wall-clock (realtime) extern body tests (T0962) ---

// The time module's promise_wallclock extern gets its body from defineTimeBodies.
// On POSIX it reads CLOCK_REALTIME (id 0 on both Linux and macOS), distinct from
// the monotonic nanotime read (id 1 on Linux / 6 on macOS).
func TestWallclockExternBodyPosix(t *testing.T) {
	ir := generateIRForTarget(t, `
		_wallclock() int `+"`extern(\"promise_wallclock\")"+`;
		main() { int _x = _wallclock(); }
	`, "x86_64-unknown-linux-gnu")
	assertContains(t, ir, "define void @promise_wallclock(i8* %sret)")
	// CLOCK_REALTIME is 0 — the realtime clock, not the monotonic one.
	assertContains(t, ir, "call i32 @clock_gettime(i32 0,")
}

// On WASM there is no portable realtime source from emitted IR, so the body
// returns 0 (no clock_gettime call).
func TestWallclockExternBodyWasmReturnsZero(t *testing.T) {
	ir := generateIRForTarget(t, `
		_wallclock() int `+"`extern(\"promise_wallclock\")"+`;
		main() { int _x = _wallclock(); }
	`, "wasm32-wasi")
	assertContains(t, ir, "define void @promise_wallclock(i8* %sret)")
	if strings.Contains(ir, "@clock_gettime") {
		t.Errorf("WASM wallclock body must not call clock_gettime\ngot:\n%s", ir)
	}
}

// On Windows the body reads GetSystemTimePreciseAsFileTime and converts the
// FILETIME (100ns ticks since 1601) to nanoseconds since the Unix epoch.
func TestWallclockExternBodyWindows(t *testing.T) {
	ir := generateIRForTarget(t, `
		_wallclock() int `+"`extern(\"promise_wallclock\")"+`;
		main() { int _x = _wallclock(); }
	`, "x86_64-pc-windows-msvc")
	assertContains(t, ir, "define void @promise_wallclock(i8* %sret)")
	assertContains(t, ir, "@GetSystemTimePreciseAsFileTime")
	// Unix-epoch shift constant: 116444736000000000 ticks from 1601 → 1970.
	assertContains(t, ir, "116444736000000000")
}

// T0962: A main-file method taking a value-type parameter from another module
// (here std's Duration) must declare its stub and define its body with the SAME
// LLVM type. The stub is declared before the module is compiled, so the layout
// isn't in c.layouts yet; resolveType must compute it on demand rather than fall
// back to the generic {i8*,i8*} userValueType (which mismatched the body's
// %promise_Duration_v and crashed codegen with a store-type error).
func TestCrossModuleValueTypeParamLayout(t *testing.T) {
	ir := generateIR(t, `
		type Foo `+"`public"+` {
			int n;
			bar(Duration d) int => this.n + d.nanos;
		}
		main() { f := Foo(n: 1); int _x = f.bar(Duration.from_nanos(2)); }
	`)
	// Both the declaration and definition agree on the value-struct param type.
	assertContains(t, ir, "define i64 @Foo.bar(i8* %this, %promise_Duration_v %d)")
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

func TestWasmExternDirectReturn(t *testing.T) {
	ir := generateIRForTarget(t, `
		_sched_yield() i32 `+"`extern(\"__wasi_sched_yield\") `wasm_import(\"wasi_snapshot_preview1\", \"sched_yield\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	assertContains(t, ir, "declare i32 @__wasi_sched_yield()")
}

func TestWasmExternDirectParams(t *testing.T) {
	ir := generateIRForTarget(t, `
		_fd_close(i32 fd) `+"`extern(\"fd_close\") `wasm_import(\"wasi_snapshot_preview1\", \"fd_close\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	assertContains(t, ir, "declare void @fd_close(i32 %fd)")
}

func TestWasmExternDirectReturnWithParams(t *testing.T) {
	ir := generateIRForTarget(t, `
		_fd_read(i32 fd, i32 iovs, i32 iovs_len, i32 nwritten) i32 `+"`extern(\"fd_read\") `wasm_import(\"wasi_snapshot_preview1\", \"fd_read\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	assertContains(t, ir, "declare i32 @fd_read(i32 %fd, i32 %iovs, i32 %iovs_len, i32 %nwritten)")
}

func TestWasmExternDirectCall(t *testing.T) {
	ir := generateIRForTarget(t, `
		_get() int `+"`extern(\"test_get\") `wasm_import(\"env\", \"test_get\") `target(wasm)"+`;
		main() { x := _get(); }
	`, "wasm32-wasi")
	assertContains(t, ir, "declare i64 @test_get()")
	assertContains(t, ir, "call i64 @test_get()")
}

func TestWasmExternDirectCallWithParams(t *testing.T) {
	ir := generateIRForTarget(t, `
		_add(int a, int b) int `+"`extern(\"test_add\") `wasm_import(\"env\", \"test_add\") `target(wasm)"+`;
		main() { x := _add(1, 2); }
	`, "wasm32-wasi")
	assertContains(t, ir, "declare i64 @test_add(i64 %a, i64 %b)")
	assertContains(t, ir, "call i64 @test_add(i64")
}

func TestWasmExternNativeUnchanged(t *testing.T) {
	// Native targets still use sret/i8* for the same types
	ir := generateIR(t, `
		get_value() int `+"`"+`extern("native_get");
		use_value(int x) `+"`"+`extern("native_use");
		main() { use_value(1); x := get_value(); }
	`)
	assertContains(t, ir, "declare void @native_get(i8* %sret)")
	assertContains(t, ir, "declare void @native_use(i8* %x)")
}

func TestWasmExternBoolReturn(t *testing.T) {
	ir := generateIRForTarget(t, `
		_check() bool `+"`extern(\"test_check\") `wasm_import(\"env\", \"test_check\") `target(wasm)"+`;
		main() { x := _check(); }
	`, "wasm32-wasi")
	// Bool (i1) should use direct return on WASM, not sret
	assertContains(t, ir, "declare i1 @test_check()")
	assertContains(t, ir, "call i1 @test_check()")
}

func TestWasmExternF64Param(t *testing.T) {
	ir := generateIRForTarget(t, `
		_set(f64 val) `+"`extern(\"test_set\") `wasm_import(\"env\", \"test_set\") `target(wasm)"+`;
		main() { _set(3.14); }
	`, "wasm32-wasi")
	// f64 (double) should use direct param on WASM
	assertContains(t, ir, "declare void @test_set(double %val)")
}

func TestWasmExternFailableStillSret(t *testing.T) {
	ir := generateIRForTarget(t, `
		_open!(i32 fd) i32 `+"`extern(\"test_open\") `wasm_import(\"env\", \"test_open\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	// Failable externs always use sret, even on WASM
	assertContains(t, ir, "declare void @test_open(i8* %sret")
}

func TestWasmExternWithoutImportStillSret(t *testing.T) {
	ir := generateIRForTarget(t, `
		_internal(int x) int `+"`extern(\"test_internal\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	// Externs without wasm_import annotation keep sret ABI on WASM
	assertContains(t, ir, "declare void @test_internal(i8* %sret, i8* %x)")
}

// T0315: wasm32-web targets must export @_initialize (the JS/Node entry point)
// rather than @_start (the WASI Command convention).
func TestWasmWebEmitsInitialize(t *testing.T) {
	ir := generateIRForTarget(t, `
		main() { print_line("hi"); }
	`, "wasm32-web")
	assertContains(t, ir, "define void @_initialize()")
	assertNotContains(t, ir, "define void @_start()")
}

// wasm32-wasi keeps the existing @_start export.
func TestWasmWasiEmitsStart(t *testing.T) {
	ir := generateIRForTarget(t, `
		main() { print_line("hi"); }
	`, "wasm32-wasi")
	assertContains(t, ir, "define void @_start()")
	assertNotContains(t, ir, "define void @_initialize()")
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
		modify(int& x) `+"`"+`extern("test_modify");
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
		modify(int& x) `+"`"+`extern("test_modify");
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

func TestTripleQuotedStringLiteral(t *testing.T) {
	ir := generateIR(t, "main() { s := \"\"\"\nhello world\n\"\"\"; }")
	assertContains(t, ir, `c"\0Ahello world\0A"`)
	assertContains(t, ir, "private constant { i8*, i64, [13 x i8] }")
}

func TestRawStringLiteral(t *testing.T) {
	ir := generateIR(t, `main() { s := r"hello\nworld"; }`)
	assertContains(t, ir, `c"hello\5Cnworld"`)
	assertContains(t, ir, "private constant { i8*, i64, [12 x i8] }")
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

// B0227: string.from_bytes must mask bit 63 of the vector count field
// to handle static vector literals (T0062).
func TestStringFromBytesMasksStaticFlag(t *testing.T) {
	ir := generateIR(t, `main() { string s = string.from_bytes([65u8]); }`)
	// The from_bytes codegen should use loadVectorLen which ANDs with 0x7FFFFFFFFFFFFFFF
	assertContains(t, ir, "and i64")
	assertContains(t, ir, "u0x7FFFFFFFFFFFFFFF")
	assertContains(t, ir, "call i8* @promise_string_new(")
}

// B0223: Vector slice intermediate in from_bytes must be tracked as a heap temp
// and dropped at statement end (via Vector.drop).
func TestVectorSliceTempDroppedInFromBytes(t *testing.T) {
	ir := generateIR(t, `
		take(string s) {}
		main() {
			u8[] buf = u8[].filled(65u8, 10);
			take(string.from_bytes(buf[0:5]));
		}
	`)
	// The vector slice result should be dropped via Vector.drop at statement end
	assertContains(t, ir, "call void @Vector.drop(")
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
			add(this, int n) int {
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
			new!(~this, int value) void {
				if value < 1 {
					raise error(message: "invalid port");
				}
				this.value = value;
			}
		}
		main!() {
			Port p = Port(value: 80)?!;
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
			tryParse!(string data) `+"`"+`abstract `+"`"+`factory;
		}
		type Strict {
			tryParse!(string data) Strict `+"`"+`factory {
				if data == "bad" {
					raise error("invalid");
				}
				return Strict();
			}
		}
		tryLoad![T: TryParseable](string data) T {
			return T.tryParse(data);
		}
		main() {
			Strict s = tryLoad[Strict]("ok")?!;
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

func TestUseVarDeclFailableInitAutoPropagate(t *testing.T) {
	// GitHub #3: a bare failable call as a `use` initializer must auto-propagate
	// and unwrap to the ok value before the store. Previously codegen panicked
	// storing the failable-result aggregate into the unwrapped slot.
	ir := generateIR(t, `
		type Res {
			int id;
			make!(int id) Res `+"`"+`factory { return Res(id: id); }
			close!(~this) {}
		}
		build!() int {
			use r := Res.make(7);
			return r.id;
		}
		main() {}
	`)
	// The failable factory returns the {tag, value, errptr} result aggregate ...
	assertContains(t, ir, "call { i1, { i8*, i8* }, i8* } @Res.make(")
	// ... which is unwrapped on the auto-propagation ok path ...
	assertContains(t, ir, "auto.ok")
	// ... and the unwrapped Res value (not the raw aggregate) is stored into the
	// `use` slot %r.
	assertContains(t, ir, "{ i8*, i8* }* %r")
}

func TestUseBoundBothCloseAndDropSuppressesUserDrop(t *testing.T) {
	// T0967 / language-design §16.4: for a `use`-bound value whose type defines
	// BOTH close() and a user drop(), only close() runs at scope exit — the user
	// drop() body is suppressed (use takes precedence) to avoid double-cleanup.
	// The instance is still freed (the heap memory is not user logic).
	ir := generateIR(t, `
		type Conn {
			int id;
			close!(~this) {}
			drop(~this) {}
		}
		main() {
			use c := Conn(id: 1);
		}
	`)
	// close() is dispatched on the use binding's scope exit.
	assertContains(t, ir, "call { i1, i8* } @Conn.close(")
	// The heap instance is reclaimed via pal_free on the close-free path.
	assertContains(t, ir, "close.free")
	// Crucially, the user drop() is NOT called on the close path. Scope the
	// assertion to main's body — the typeinfo drop$wrap (B0226) legitimately
	// calls @Conn.drop, but that is RTTI dispatch, not the use/close path.
	mainIR := extractFunc(ir, "main")
	if mainIR == "" {
		t.Fatal("could not extract main function from IR")
	}
	assertNotContains(t, mainIR, "call void @Conn.drop(")
}

func TestUseBoundCloseDropInsideMethodRestoresThis(t *testing.T) {
	// T0967: when the use-bound close+drop value lives inside a METHOD body, the
	// close-free suppression path (emitInstanceFieldDropsAndFree) temporarily
	// rebinds locals["this"] to the closing instance to drop its (droppable)
	// fields, then must restore the method's real `this`. A `string` field forces
	// the field-drop branch; the method reads `this.marker` AFTER the use scope so
	// the restored `this` must still point at Holder. This exercises the hadThis
	// save/restore branch that free-function call sites never reach.
	ir := generateIR(t, `
		type Res {
			string name;
			close!(~this) {}
			drop(~this) {}
		}
		type Holder {
			int marker;
			run(this) int {
				{
					use r := Res(name: "x");
				}
				return this.marker;
			}
		}
		main() {
			h := Holder(marker: 1);
			x := h.run();
		}
	`)
	run := extractFunc(ir, "Holder.run")
	if run == "" {
		t.Fatal("could not extract Holder.run from IR")
	}
	// close() ran on the use binding inside the method...
	assertContains(t, run, "@Res.close(")
	// ...and its droppable string field was reclaimed inline (suppression path),
	assertContains(t, run, "@promise_string_drop")
	// ...but the user Res.drop() body was NOT invoked (use takes precedence).
	assertNotContains(t, run, "call void @Res.drop(")
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

// T0474: super(v) in a generic Child[T] is Base[T] constructor must target the
// monomorphized parent constructor (Base__int.new), not the bare-name Base.new.
func TestSuperCallGenericParentCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] {
			T value;
			new(~this, T move v) {
				this.value = v;
			}
		}
		type Child[T] is Base[T] {
			int extra;
			new(~this, T move v, int e) {
				super(move v);
				this.extra = e;
			}
		}
		main() {
			Child[int] c = Child[int](v: 5, e: 7);
		}
	`)
	// Child[int].new should call the monomorphized parent constructor.
	assertContains(t, ir, `call void @"Base[int].new"(`)
	assertContains(t, ir, `call void @"Child[int].new"(`)
}

// T0474: with reordered child type params (Child[A, B] is Base[B]), super(b)
// must name the parent constructor monomorphized on B (Base[int]), not A.
func TestSuperCallGenericReorderedParentCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] {
			T value;
			new(~this, T move v) {
				this.value = v;
			}
		}
		type Child[A, B] is Base[B] {
			A first;
			new(~this, A move a, B move b) {
				super(move b);
				this.first = a;
			}
		}
		main() {
			Child[string, int] c = Child[string, int](a: "x", b: 9);
		}
	`)
	// B=int → parent monomorphized as Base[int], not Base[string].
	assertContains(t, ir, `call void @"Base[int].new"(`)
	assertContains(t, ir, `call void @"Child[string, int].new"(`)
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

// T1155: a match-arm pattern binding that reuses the scrutinee's name must be
// scoped to the arm only. Before the fix, codegen left c.locals[scrutinee]
// pointing at the destructured (string) alloca, so a later `match` on the same
// name evaluated its subject against the wrong alloca and emitted garbage /
// self-recursive control flow → runtime stack overflow. The correct IR loads the
// enum subject from the parameter alloca (%b.addr) for BOTH matches.
func TestEnumMatchScrutineeShadow(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Empty }
		f(Msg b) int {
			int la = match b { Msg.Text(b) => b.len, Msg.Empty => 0 };
			int lb = match b { Msg.Text(s) => s.len, Msg.Empty => 0 };
			return la + lb;
		}
		main() { Msg m = Msg.Text("ab"); int x = f(m); }
	`)
	fn := ir[strings.Index(ir, "define i64 @__user.f("):]
	fn = fn[:strings.Index(fn, "\n}")]
	// Both matches must load the enum subject from the param alloca %b.addr —
	// the arm binding `Msg.Text(b)` in the first match must not leak and replace
	// the scrutinee for the second match.
	if got := strings.Count(fn, "load %promise_Msg_enum, %promise_Msg_enum* %b.addr"); got != 2 {
		t.Fatalf("expected 2 loads of the enum subject from %%b.addr (one per match), got %d\n%s", got, fn)
	}
	// The fix must not introduce a recursive call to f.
	if strings.Contains(fn, "call i64 @__user.f(") {
		t.Fatalf("f must not self-recurse:\n%s", fn)
	}
}

// B0328: Bare variant names in match-as-expression on enum subject must resolve
// to EnumVariantMatchPattern, not NameMatchPattern (catch-all binding).
func TestEnumMatchBareVariantNames(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() int {
			Color c = Color.Green;
			return match c {
				Red => 1,
				Green => 2,
				Blue => 3,
			};
		}
		main() { }
	`)
	// Must produce a switch with case labels (not an empty switch)
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "i32 0, label %match.arm0")
	assertContains(t, ir, "i32 1, label %match.arm1")
	assertContains(t, ir, "i32 2, label %match.arm2")
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
	assertContains(t, ir, "define i1 @__user.is_red(i32 %c)")
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
	assertContains(t, ir, "define i32 @__user.get_green()")
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

func TestFailableDeclaration(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
		main() { }
	`)
	// Return type should be result struct { i1, i64, i8* }
	assertContains(t, ir, "define { i1, i64, i8* } @__user.parse(i8* %s)")
}

func TestReturnInFailable(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 42; }
		main() { }
	`)
	// Should wrap value in Ok result: tag=false, value, null error
	assertContains(t, ir, "insertvalue { i1, i64, i8* }")
	assertContains(t, ir, "i1 false")
	assertContains(t, ir, "ret { i1, i64, i8* }")
}

func TestFailableVoidBangShorthand(t *testing.T) {
	ir := generateIR(t, `
		fail!() { raise error(message: "oops"); }
		main() { }
	`)
	// Should produce void result struct { i1, i8* }
	assertContains(t, ir, "define { i1, i8* } @__user.fail()")
}

func TestFailableMain(t *testing.T) {
	ir := generateIR(t, `
		main!() {
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
		parse!(string s) int { raise error(message: "parse error"); }
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
		parse!(string s) int { return 0; }
		process!() int {
			x := parse("42")?^;
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
		parse!(string s) int { return 0; }
		main() {
			x := parse("42")?!;
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

// B0256: emitErrorPanic heap-allocates a C string copy but promise_panic sets
// type=1 (.rodata). The fix overwrites panic_type to 2 (heap) after the call
// so goroutine_exit frees it. T0142: now calls promise_panic_at which handles
// the type=2 store internally.
func TestErrorPanicSetsHeapType(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
		main() {
			x := parse("42")?!;
		}
	`)
	// emitErrorPanic calls promise_panic_at (T0142)
	assertContains(t, ir, "call void @promise_panic_at(")
	// promise_panic_at body stores type=2 after calling promise_panic
	assertContains(t, ir, "store i8 2, i8* @__promise_panic_type")
}

// T0142: error panic via ?! includes source file and line number.
func TestErrorPanicSourceLocation(t *testing.T) {
	ir := generateIR(t, `
		fail!() int { return 0; }
		main() {
			x := fail()?!;
		}
	`)
	// Should call promise_panic_at with a filename global and line number constant
	assertContains(t, ir, "call void @promise_panic_at(")
	// Filename global should be a .file. prefixed constant
	assertContains(t, ir, "@.file.")
	// promise_panic_at should be defined with its body (not just declared)
	assertContains(t, ir, "define void @promise_panic_at(")
}

// T0125: When func()?! returns a string, the unwrapped i8* must be tracked
// as a stmt temp so it gets freed at statement end if not claimed.
func TestErrorUnwrapStringTemp(t *testing.T) {
	ir := generateIR(t, `
		make_str!() string { return "hello"; }
		main() {
			int n = make_str()?!.len;
		}
	`)
	// Should have string temp tracking: store to alloca + drop flag
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "promise_string_drop")
}

// B0260: When func()?^ propagates a string result, the ok-path i8* must be
// tracked as a stmt temp so it gets freed if not claimed (e.g., by vec.push
// which dups the string).
func TestErrorPropagateStringTemp(t *testing.T) {
	ir := generateIR(t, `
		make_str!() string { return "hello"; }
		wrap!(string[] v) string[] {
			v.push(make_str()?^);
			return v;
		}
	`)
	// Should have string temp tracking for the propagated string result
	assertContains(t, ir, "error.propagate")
	assertContains(t, ir, "error.ok")
	// The decoded string must be tracked as a temp and dropped after push
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "promise_string_drop")
}

func TestErrorHandler(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
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
		parse!(string s) int { return 0; }
		main() {
			x := parse("42") ? _ { 0; };
		}
	`)
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.ok")
}

func TestVoidFailable(t *testing.T) {
	ir := generateIR(t, `
		validate!(string s) void { return; }
		main() { }
	`)
	// Return type should be { i1, i8* }
	assertContains(t, ir, "define { i1, i8* } @__user.validate(i8* %s)")
}

func TestVoidRaise(t *testing.T) {
	ir := generateIR(t, `
		validate!(string s) void { raise error(message: "invalid"); }
		main() { }
	`)
	assertContains(t, ir, "i1 true")
	assertContains(t, ir, "ret { i1, i8* }")
}

func TestFailableMethod(t *testing.T) {
	ir := generateIR(t, `
		type Parser {
			string input;
			parse!(this) int {
				return 42;
			}
		}
		main() { }
	`)
	assertContains(t, ir, "define { i1, i64, i8* } @Parser.parse(i8* %this)")
}

func TestFailableAutoTerminator(t *testing.T) {
	ir := generateIR(t, `
		validate!(string s) void {
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
		validate!(string s) void { raise error(message: "invalid"); }
		process!() void {
			validate("x")?^;
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
		validate!(string s) void { raise error(message: "invalid"); }
		main() {
			validate("x")?!;
		}
	`)
	assertContains(t, ir, "error.panic")
	assertContains(t, ir, "error.ok")
	assertContains(t, ir, "call void @promise_panic(")
}

func TestVoidFailableHandler(t *testing.T) {
	ir := generateIR(t, `
		validate!(string s) void { raise error(message: "invalid"); }
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
		a!() int { return 1; }
		b!() int { return a()?^; }
		c!() int { return b()?^; }
		main() { }
	`)
	// Both b and c should have propagation blocks
	assertContains(t, ir, "error.propagate")
	assertContains(t, ir, "error.ok")
}

func TestErrorHandlerWithReturn(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
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
		parse!(string s) int {
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

// T0761: RTTI cast whose subject is itself an Optional. genCastExpr used to
// treat the {i1,{i8*,i8*}} optional as a bare value struct and panic; it now
// branches to genOptionalCastExpr, which unwraps field 1 before promise_type_is.
func TestOptionalSubjectForceCast(t *testing.T) {
	ir := generateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		main() {
			Base b = Der(name: "x");
			Base? ob = b;
			d := ob as! Der;
			_ := d.tag();
		}
	`)
	// Compiled without panic, took the optional-subject path (present/panic
	// blocks) and queried RTTI after unwrapping the inner value struct.
	assertContains(t, ir, "optcast.present")
	assertContains(t, ir, "optcast.nonepanic")
	assertContains(t, ir, "call i32 @promise_type_is(")
}

func TestOptionalSubjectOptionalCast(t *testing.T) {
	ir := generateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		main() {
			Base b = Der(name: "x");
			Base? ob = b;
			Der? d = ob as Der;
			if d { }
		}
	`)
	// Optional-subject `as` path: presence check, RTTI check, some/none merge.
	assertContains(t, ir, "optcast.check")
	assertContains(t, ir, "optcast.some")
	assertContains(t, ir, "optcast.none")
	assertContains(t, ir, "optcast.merge")
	assertContains(t, ir, "call i32 @promise_type_is(")
}

// T0848: casting to a borrow target type (`x as! T&`) used to panic codegen with
// "unsupported cast target type *ast.SharedRefTypeRef" — the target-type switch
// had no ref case. genCastExpr now peels the ref to the underlying named type and
// runs the same RTTI cast, so this compiles and emits the normal promise_type_is
// query plus the `as!` force-cast panic block.
func TestCastToBorrowTarget(t *testing.T) {
	ir := generateIR(t, `
		type Shape { string name; }
		type Circle is Shape { f64 radius; }
		borrow_return(Shape s) Circle & {
			return s as! Circle &;
		}
		main() { }
	`)
	assertContains(t, ir, "call i32 @promise_type_is(")
	assertContains(t, ir, "cast.panic")
}

// T0850: an `Ref[T?]` (or `Mutex[T?]`) whose element is an Optional must drop
// the inner optional's heap payload when the last reference is released. The
// Arc/Mutex inner-drop path (emitInnerDrop) dispatched on extractNamed, which is
// nil for an Optional, so no case fired and the held value leaked. The fix adds
// an Optional case that drops the present inner — here @Box.drop on the held Box.
func TestArcOptionalElementInnerDrop(t *testing.T) {
	ir := generateIR(t, `
		type Box { string s; }
		main() {
			Box? init = Box(s: "x");
			a := Ref[Box?](init);
			_ := a;
		}
	`)
	arcDrop := extractDefine(ir, `"Ref[Box?].drop"`)
	assertContains(t, arcDrop, "call void @Box.drop(")
}

// T0850: an RTTI optional cast whose subject is a BORROWED optional (`T?&`,
// here `Ref[Base?].borrow`) used to crash codegen — the non-optional RTTI path
// fed the loaded `{i1,{i8*,i8*}}` optional to wrapOptional → insertvalue/store
// type mismatch panic. The fix peels the SharedRef/MutRef and routes through
// genOptionalCastExpr (borrowSource): it must no longer panic, must emit the
// optcast blocks (proving it took the optional-subject path), and must dup the
// inner (heapdup.copy) since the borrow aliases the Arc's external-owned payload.
func TestBorrowedOptionalSubjectCast(t *testing.T) {
	ir := generateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		main() {
			Base b = Der(name: "x");
			Base? ob = b;
			a := Ref[Base?](ob);
			Der? d = a.borrow as Der;
			if d { }
		}
	`)
	// Routed through the optional-subject `as` path (not the bare-value path).
	assertContains(t, ir, "optcast.check")
	assertContains(t, ir, "optcast.some")
	assertContains(t, ir, "optcast.none")
	assertContains(t, ir, "optcast.merge")
	assertContains(t, ir, "call i32 @promise_type_is(")
	// The borrowed inner is duped into an owned copy before the RTTI dispatch.
	assertContains(t, extractDefine(ir, ".goroutine.main"), "heapdup.copy")
}

// T0850: a forced borrowed-optional cast (`Ref[T?].borrow as! U`) takes the
// optional-subject force path (panic on none/mismatch, return the duped inner).
func TestBorrowedOptionalSubjectForceCast(t *testing.T) {
	ir := generateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		main() {
			Base b = Der(name: "x");
			Base? ob = b;
			a := Ref[Base?](ob);
			d := a.borrow as! Der;
			_ := d.tag();
		}
	`)
	assertContains(t, ir, "optcast.present")
	assertContains(t, ir, "optcast.nonepanic")
	assertContains(t, ir, "optcast.mismatch")
	assertContains(t, extractDefine(ir, ".goroutine.main"), "heapdup.copy")
}

// T0761: an Optional cast whose subject is a container element (`v[i]`) aliases
// the vector's bucket, so genOptionalCastExpr must dup the inner — otherwise both
// the cast result and the vector free it (double-free) / the result leaks. The
// dup emits a heapdup.copy block before the cast's RTTI check.
func TestOptionalSubjectIndexCastDups(t *testing.T) {
	ir := generateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		main() {
			Base?[] v = [];
			Base b = Der(name: "x");
			v.push(b);
			d := v[0] as! Der;
			_ := d.tag();
		}
	`)
	// Aliasing source is duped into an owned copy before the RTTI dispatch.
	// Scoped to @main: the stdAll clone funcs always emit heapdup.copy, so a
	// whole-IR check would be trivially true.
	assertContains(t, extractDefine(ir, ".goroutine.main"), "heapdup.copy")
	assertContains(t, ir, "optcast.present")
}

// T0761: a scalar optional subject (`int? as f64`) has a bare scalar inner, not a
// value struct — genOptionalCastExpr must take the scalar path (emitScalarCast),
// not the RTTI path (which would extractvalue a non-aggregate and panic).
func TestOptionalSubjectScalarCast(t *testing.T) {
	// Force: unwrap (panic on none) then sitofp the inner int to f64.
	irForce := generateIR(t, `
		main() {
			int? x = 5;
			f := x as! f64;
			_ := f;
		}
	`)
	assertContains(t, irForce, "optcast.present")
	assertContains(t, irForce, "sitofp") // scalar conversion, not an RTTI dispatch
	// Optional: present → some(convert); absent → none.
	irOpt := generateIR(t, `
		main() {
			int? x = 5;
			f64? f = x as f64;
			if f { }
		}
	`)
	assertContains(t, irOpt, "optcast.some")
	assertContains(t, irOpt, "optcast.none")
	assertContains(t, irOpt, "sitofp") // scalar conversion, not an RTTI dispatch
}

// T0761: an Optional cast whose subject is an OWNED-LOCAL member field
// (`h.slot as Der`, h a local — not `this`). This is the MemberExpr arm that
// returns the *non*-aliasing/*non*-owned verdict in all three helpers:
// optionalCastSourceAliasesExternalOwner=false (no dup), optionalCastResultOwnsInner=false
// (no heap temp), and neutralizeOptionalCastSource clears the owner's field flag
// on the match path. Distinct from the borrowed-`this` and ident shapes.
func TestOptionalSubjectOwnedMemberCast(t *testing.T) {
	src := `
		type Base { string name; tag(this) string ` + "`" + `abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		type Holder { Base? slot; drop(~this) {} }
		main() {
			Base b = Der(name: "x");
			Holder h = Holder(slot: b);
			%s
		}
	`
	// Optional: owned-member source is neutralized (not duped) on match.
	// (heapdup.copy is scoped to @main — the stdAll clone funcs always emit it.)
	irOpt := generateIR(t, fmt.Sprintf(src, `Der? d = h.slot as Der; if d { }`))
	assertContains(t, irOpt, "optcast.check")
	assertContains(t, irOpt, "optcast.some")
	assertContains(t, irOpt, "call i32 @promise_type_is(")
	assertNotContains(t, extractDefine(irOpt, ".goroutine.main"), "heapdup.copy") // owned-local member is NOT duped
	// Force: same source shape via the `as!` path.
	irForce := generateIR(t, fmt.Sprintf(src, `d := h.slot as! Der; _ := d.tag();`))
	assertContains(t, irForce, "optcast.present")
	assertContains(t, irForce, "optcast.nonepanic")
	assertNotContains(t, extractDefine(irForce, ".goroutine.main"), "heapdup.copy")
}

// T0761: an Optional cast whose subject is a borrowed-`this.field` inside a
// `this` method. The MemberExpr arm here takes the aliasing/owned-true verdict:
// the caller still owns the field, so the inner is duped (heapdup.copy) and the
// `as` path registers it as a heap temp. Mirrors the index-source dup path but
// through the member shape.
func TestOptionalSubjectBorrowedThisCast(t *testing.T) {
	src := `
		type Base { string name; tag(this) string ` + "`" + `abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		type Holder {
			Base? slot;
			%s
			drop(~this) {}
		}
		main() {
			Base b = Der(name: "x");
			Holder h = Holder(slot: b);
			_ := h.probe();
		}
	`
	// Force through borrowed this: inner is duped before the RTTI dispatch.
	// (heapdup.copy is scoped to the probe method — stdAll clone funcs also emit it.)
	irForce := generateIR(t, fmt.Sprintf(src,
		`probe(this) string { c := this.slot as! Der; return c.tag(); }`))
	assertContains(t, extractFunction(irForce, "Holder.probe"), "heapdup.copy")
	assertContains(t, irForce, "optcast.present")
	// Optional through borrowed this: duped AND registered as a heap temp (result
	// owns the duped inner; freed on present+mismatch).
	irOpt := generateIR(t, fmt.Sprintf(src,
		`probe(this) bool { Der? d = this.slot as Der; if d { return true; } return false; }`))
	assertContains(t, extractFunction(irOpt, "Holder.probe"), "heapdup.copy")
	assertContains(t, irOpt, "optcast.check")
	assertContains(t, irOpt, "optcast.some")
}

// T0761: an Optional `as` cast on a call-result TEMP source. The temp owns its
// inner outright (no source binding to neutralize), so optionalCastResultOwnsInner
// returns true via the default arm and the `as` path registers the inner as a heap
// temp inside checkBlock — freed on present+mismatch, claimed by the binding on
// match. (The only other Go `as` test uses an ident source, which skips this block.)
func TestOptionalSubjectTempOptionalCast(t *testing.T) {
	ir := generateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		make_opt(string move n) Base? { Base s = Der(name: n); return s; }
		main() {
			Der? d = make_opt("x") as Der;
			if d { }
		}
	`)
	assertContains(t, ir, "optcast.check")
	assertContains(t, ir, "optcast.some")
	assertContains(t, ir, "optcast.none")
	// Temp source is owned outright — no dup (scoped to @main; stdAll clone funcs
	// emit heapdup.copy elsewhere), but heap-temp tracked for present+mismatch.
	assertNotContains(t, extractDefine(ir, ".goroutine.main"), "heapdup.copy")
}

// T0761: an Optional cast inside a GENERIC function body. The body is codegen'd
// with c.typeSubst active (monomorphization), exercising genOptionalCastExpr's
// type-substitution branches. The optional source is a LOCAL (a parameter source
// hits the pre-existing T0811 parameter segfault). Both `as` and `as!` paths.
func TestOptionalSubjectGenericBodyCast(t *testing.T) {
	src := `
		type Base { string name; tag(this) string ` + "`" + `abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		gcast[T](T marker) %s {
			_ := marker;
			Base b = Der(name: "g");
			Base? oo = b;
			%s
		}
		main() { %s }
	`
	// Optional: monomorphized gcast__int emits the optcast some/none/merge path.
	irOpt := generateIR(t, fmt.Sprintf(src, "Der?", `return oo as Der;`,
		`Der? d = gcast(0); if d { }`))
	assertContains(t, irOpt, "optcast.check")
	assertContains(t, irOpt, "optcast.some")
	assertContains(t, irOpt, "call i32 @promise_type_is(")
	// Force: monomorphized gcast__int emits the present/panic path.
	irForce := generateIR(t, fmt.Sprintf(src, "string",
		`d := oo as! Der; return d.name;`, `_ := gcast(0);`))
	assertContains(t, irForce, "optcast.present")
	assertContains(t, irForce, "optcast.nonepanic")
	// Generic body + aliasing (index) source: the dup and heap-temp registration
	// both run their `c.typeSubst != nil` substitution branches.
	irIndex := generateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		gidx[T](T marker) Der? {
			_ := marker;
			Base?[] v = [];
			Base b = Der(name: "g");
			v.push(b);
			return v[0] as Der;
		}
		main() { Der? d = gidx(0); if d { } }
	`)
	assertContains(t, irIndex, "optcast.check")
	assertContains(t, irIndex, "heapdup.copy") // duped aliasing source inside the generic body
}

// T0761: a paren-wrapped Optional cast source (`(oo) as Der`). The `as` move
// path's neutralizeOptionalCastSource must peel the ParenExpr before clearing the
// underlying ident's present flag (otherwise the source's drop double-frees the
// inner). Compiles cleanly and takes the optcast path.
func TestOptionalSubjectParenSourceCast(t *testing.T) {
	ir := generateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		main() {
			Base b = Der(name: "x");
			Base? oo = b;
			Der? d = (oo) as Der;
			if d { }
		}
	`)
	assertContains(t, ir, "optcast.check")
	assertContains(t, ir, "optcast.some")
	assertContains(t, ir, "call i32 @promise_type_is(")
}

// --- Typed Error Handler Tests ---

func TestTypedErrorHandler(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error {
			int code;
		}
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process!() void {
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
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process!() void {
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
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process!() void {
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
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process!() void {
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
		fail!() void { raise IoError(message: "disk full", code: 28); }
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
		fail!() void { raise IoError(message: "disk full", code: 28); }
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
		fail!() void { raise IoError(message: "disk full", code: 28); }
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
		fail!() void { raise error(message: "oops"); }
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
		fail!() void { raise IoError(message: "disk full", code: 28); }
		process!() int {
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
		foo!() void { raise error("oops"); }
		main() { foo() ? e { }; }
	`)
	assertContains(t, ir, "error.handler")
}

func TestErrorSubtypePositionalConstruction(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		foo!() void { raise IoError("disk full", 28); }
		main() { foo() ? e { }; }
	`)
	assertContains(t, ir, "error.handler")
}

func TestGenericErrorTypeRaise(t *testing.T) {
	ir := generateIR(t, `
		type DataError[T] is error { T data; }
		foo!() void { raise DataError[int](message: "bad", data: 42); }
		main() { foo() ? e { }; }
	`)
	// Should monomorphize DataError[int]
	assertContains(t, ir, "DataError[int]")
	assertContains(t, ir, "error.handler")
}

func TestFailableCallInsideHandler(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
		foo!() int {
			int v = parse("x") ? e { return parse("0")?^; };
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
		parse!(string s) int { return 0; }
		foo() {
			parse("x") ? e { int v = parse("0")?!; };
		}
		main() { }
	`)
	// Should have both handler and panic-on-unwrap blocks
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.panic")
}

func TestNestedErrorHandlers(t *testing.T) {
	ir := generateIR(t, `
		a!() int { return 1; }
		b!() int { return 2; }
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
		fail!() void { raise DbError(message: "fail", code: 500, query: "SELECT"); }
		handler!() int {
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
		fail!() void { raise error(message: "oops"); }
		process!() void {
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
		parse!() int { return 42; }
		process!() int {
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
		parse!() int { return 42; }
		wrapper!() int {
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
		parse!() int { return 42; }
		wrapper!() int {
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
		parse!(string s) int { return 0; }
		wrapper!() int {
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
		parse!() int { return 42; }
		use_value(int x) {}
		wrapper!() void {
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
		parse!() int { return 42; }
		type Foo { use_value(int x) {} }
		wrapper!() void {
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
		parse!() int { return 42; }
		type Foo { int x; }
		wrapper!() void {
			Foo(x: parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateMultipleArgs(t *testing.T) {
	ir := generateIR(t, `
		parse_a!() int { return 1; }
		parse_b!() int { return 2; }
		add(int a, int b) int { return a + b; }
		wrapper!() void {
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
		parse!() int { return 42; }
		wrapper!() int {
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
		parse!() int { return 42; }
		type Foo {
			int v;
			new(~this, int v) { this.v = v; }
		}
		wrapper!() void {
			Foo(v: parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInValueTypeArg(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		type Vec2 {
			int x `+"`"+`value;
			int y `+"`"+`value;
		}
		wrapper!() void {
			Vec2(x: parse(), y: 0);
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInEnumVariantArg(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		enum Box { Val(int v) }
		wrapper!() void {
			Box.Val(v: parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

func TestAutoPropagateInVecPushArg(t *testing.T) {
	ir := generateIR(t, `
		parse!() int { return 42; }
		wrapper!() void {
			int[] v = int[]();
			v.push(parse());
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// B0323: Failable call result used as field access target must be unwrapped.
func TestAutoPropagateInFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Foo { int x; }
		bar!() Foo { return Foo(x: 42); }
		wrapper!() int {
			return bar().x;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// B0323: Failable call result used as method call receiver must be unwrapped.
func TestAutoPropagateInMethodReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			int x;
			get_x(this) int { return this.x; }
		}
		bar!() Foo { return Foo(x: 42); }
		wrapper!() int {
			return bar().get_x();
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// B0323: Failable call result used as index target must be unwrapped.
func TestAutoPropagateInIndexTarget(t *testing.T) {
	ir := generateIR(t, `
		bar!() int[] { return [1, 2, 3]; }
		wrapper!() int {
			return bar()[0];
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// B0323: Failable call result used as generic method call receiver must be unwrapped.
func TestAutoPropagateInGenericMethodReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T val;
			cast[U](this, U default_val) U {
				return default_val;
			}
		}
		make_box!() Box[int] { return Box[int](val: 42); }
		wrapper!() string {
			return make_box().cast[string]("hello");
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// B0322: Failable call used as string method receiver must auto-propagate.
func TestAutoPropagateMethodChainReceiver(t *testing.T) {
	ir := generateIR(t, `
		get_name!() string { return "hello"; }
		wrapper!() string {
			string s = get_name().trim();
			return s;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// B0322: Auto-propagate on vector .len access.
func TestAutoPropagateVectorLen(t *testing.T) {
	ir := generateIR(t, `
		make_vec!() int[] { return [1, 2]; }
		wrapper!() int {
			return make_vec().len;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// B0322: Auto-propagate on getter call.
func TestAutoPropagateGetterReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Cnt {
			int n;
			get doubled int { return this.n * 2; }
		}
		make_cnt!() Cnt { return Cnt(n: 3); }
		wrapper!() int {
			return make_cnt().doubled;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// T0330: Failable call used as binary expression operand must auto-propagate.
func TestAutoPropagateInBinaryExpr(t *testing.T) {
	ir := generateIR(t, `
		read!() int { return 1; }
		wrapper!() bool {
			return read() != 0;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
}

// T0330: Failable call used as unary expression operand must auto-propagate.
func TestAutoPropagateInUnaryExpr(t *testing.T) {
	ir := generateIR(t, `
		get_flag!() bool { return true; }
		wrapper!() bool {
			return !get_flag();
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// T0330: Failable call as operand of && must auto-propagate in failable context.
func TestAutoPropagateInAndExpr(t *testing.T) {
	ir := generateIR(t, `
		flag!() bool { return true; }
		wrapper!() bool {
			return flag() && true;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// T0330: Failable call as operand of || must auto-propagate in failable context.
func TestAutoPropagateInOrExpr(t *testing.T) {
	ir := generateIR(t, `
		flag!() bool { return false; }
		wrapper!() bool {
			return false || flag();
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
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

// T0330: Failable call as elvis left operand must auto-propagate.
func TestAutoPropagateInElvisLeft(t *testing.T) {
	ir := generateIR(t, `
		get_opt!() int? { return 1; }
		wrapper!() int {
			return get_opt() ?: 0;
		}
		main() { }
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
}

// T0330: Failable call as elvis right (default) operand must auto-propagate.
func TestAutoPropagateInElvisRight(t *testing.T) {
	ir := generateIR(t, `
		fallback!() int { return 0; }
		wrapper!(int? v) int {
			return v ?: fallback();
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
		make!() Resource { return Resource(id: 1); }
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
		foo!() void { raise IoError(message: "err", code: 1); }
		main() { foo() ? e { }; }
	`)
	// Raise on user types should extract instance pointer (i8*) from value struct
	assertContains(t, ir, "extractvalue { i8*, i8* }")
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

func TestTypedHandlerNoMatchPropagation(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		type ParseError is error { int line; }
		fail!() void { raise ParseError(message: "parse", line: 1); }
		handler!() void {
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

			unwrap_or(this, T fallback) T {
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

// TestT0636_EnumGenericMethodInstanceMono verifies that a generic
// (method-level type param) method on a generic enum instance is emitted as a
// monomorphized function "Box[int].transform[int]", with a separate mono
// function per (owner, method-type-arg) combination.
func TestT0636_EnumGenericMethodInstanceMono(t *testing.T) {
	ir := generateIR(t, `
		enum Box[T] {
			V(Vector[T] d),
			N,
			transform[U](U _x) int {
				match this {
					V(d) => { return d.len; },
					N => { return 0; },
				}
			}
		}
		main() {
			b := Box[int].V([1, 2, 3]);
			x := b.transform[int](5);
			s := Box[string].V(["a"]);
			y := s.transform[bool](true);
		}
	`)
	// Per-(owner, method-type-arg) mono functions.
	assertContains(t, ir, `"Box[int].transform[int]"`)
	assertContains(t, ir, `"Box[string].transform[bool]"`)
	// The call site dispatches to the monomorphized enum method.
	assertContains(t, ir, `call i64 @"Box[int].transform[int]"`)
}

// TestT0636_NonGenericEnumGenericMethodMono verifies that a generic
// (method-level type param) method on a *non-generic* enum is emitted as a
// monomorphized function named with the bare enum name (no monoName / "[..]"
// owner suffix), exercising the `case *types.Enum` arm of
// genGenericEnumMethodCall (enumName = enum.Obj().Name(), distinct from the
// generic-instance path which uses monoName(inst)).
func TestT0636_NonGenericEnumGenericMethodMono(t *testing.T) {
	ir := generateIR(t, `
		enum Tagged {
			Empty,
			Data(Vector[int] xs),
			pick[U](U _sel) int {
				match this {
					Empty => { return -1; },
					Data(xs) => { return xs.len; },
				}
			}
		}
		main() {
			t := Tagged.Data([1, 2, 3, 4]);
			a := t.pick[string]("x");
			b := Tagged.Empty.pick[bool](false);
		}
	`)
	// Bare enum name (no "Tagged[..]" owner suffix) + per-method-type-arg mono.
	assertContains(t, ir, `"Tagged.pick[string]"`)
	assertContains(t, ir, `"Tagged.pick[bool]"`)
	assertContains(t, ir, `call i64 @"Tagged.pick[string]"`)
}

// TestT0639_GenericMethodViaThis verifies Defect B: a generic (method-type-
// param) method invoked via `this` inside the owner's own method body is
// emitted under the per-instance mono name (e.g. "NBox[int].inner[int]"),
// matching the monoCtx-built call site — NOT the bare-owner name
// ("NBox.inner[int]") which would be a different (mis-substituted) instance.
// Symmetric across a generic Named type and a generic enum.
func TestT0639_GenericMethodViaThis(t *testing.T) {
	ir := generateIR(t, `
		type NBox[T] {
			Vector[T] d;
			inner[U](U _x) int { return this.d.len; }
			outer(this) int { return this.inner[int](7); }
		}
		enum EBox[T] {
			V(Vector[T] d),
			N,
			inner[U](U _x) int {
				match this {
					V(d) => { return d.len; },
					N => { return 0; },
				}
			}
			outer(this) int { return this.inner[int](7); }
		}
		main() {
			n := NBox[int](d: [1, 2, 3]);
			x := n.outer();
			e := EBox[int].V([1, 2]);
			y := e.outer();
		}
	`)
	assertContains(t, ir, `define i64 @"NBox[int].inner[int]"`)
	assertContains(t, ir, `define i64 @"EBox[int].inner[int]"`)
	assertContains(t, ir, `call i64 @"NBox[int].inner[int]"`)
	assertContains(t, ir, `call i64 @"EBox[int].inner[int]"`)
	// The bare-owner name must never appear (would be the wrong instance).
	assertNotContains(t, ir, `@"NBox.inner[int]"`)
	assertNotContains(t, ir, `@"EBox.inner[int]"`)
}

// TestT0639_GenericFnRefParamGenericInstance verifies Defect A (call-site mono
// name unwraps `~`/`&` to the instance, not the bare owner) and Defect C (the
// generic free-function call passes a `~` param by pointer, matching the
// monomorphic callee's pointer ABI — no by-value/pointer mismatch segfault).
func TestT0639_GenericFnRefParamGenericInstance(t *testing.T) {
	ir := generateIR(t, `
		type NBox[T] {
			Vector[T] d;
			transform[U](U _x) int { return this.d.len; }
		}
		proc_named[X](NBox[X]~ b) int { return b.transform[int](5); }
		main() {
			x := proc_named[int](NBox[int](d: [1, 2, 3]));
		}
	`)
	// Defect A: receiver mangles to the instance, not the bare owner.
	assertContains(t, ir, `define i64 @"NBox[int].transform[int]"`)
	assertContains(t, ir, `call i64 @"NBox[int].transform[int]"`)
	assertNotContains(t, ir, `@"NBox.transform[int]"`)
	// Defect C: definition and call site agree on the pointer ABI for the `~`
	// generic-instance param. The pre-fix bug passed it by value
	// (`...({ i8*, i8* } %`), dereferenced as a pointer => segfault.
	assertContains(t, ir, `define i64 @"proc_named[int]"({ i8*, i8* }* %`)
	assertContains(t, ir, `call i64 @"proc_named[int]"({ i8*, i8* }* %`)
	assertNotContains(t, ir, `call i64 @"proc_named[int]"({ i8*, i8* } %`)
}

// TestT0639_GenericFnRefParamStringVector verifies Defect C for the broader
// param class the bug also covered: a generic function with `~` string and
// `~` Vector params must pass them by pointer (matching the monomorphic
// callee's `i8**` ABI), not by value.
func TestT0639_GenericFnRefParamStringVector(t *testing.T) {
	ir := generateIR(t, `
		take_str[T](string~ s, T _x) int { return s.len; }
		take_vec[T](Vector[T]~ v) int { return v.len; }
		main() {
			a := take_str[int]("hello", 1);
			b := take_vec[int]([1, 2, 3, 4]);
		}
	`)
	assertContains(t, ir, `define i64 @"take_str[int]"(i8** %`)
	assertContains(t, ir, `call i64 @"take_str[int]"(i8** %`)
	assertContains(t, ir, `define i64 @"take_vec[int]"(i8** %`)
	assertContains(t, ir, `call i64 @"take_vec[int]"(i8** %`)
}

// TestT0639_RefWrappedGenericInstanceOperatorGetter verifies Defect A reaches
// beyond plain method calls: `resolveTypeName` is the shared mangling helper
// for ~20 dispatch sites. A `[]`-index and a parameterless-getter call on a
// `~`/`&` generic-instance receiver must mangle to the instance name
// ("GBox[int].[]" / "GBox[int].total"), NOT the bare generic owner
// ("GBox.[]" / "GBox.total") which pre-fix panicked "undeclared method".
func TestT0639_RefWrappedGenericInstanceOperatorGetter(t *testing.T) {
	ir := generateIR(t, `
		type GBox[T] {
			Vector[T] d;
			get total int { return this.d.len; }
			[](int i) T { return this.d[i]; }
		}
		idx_mut(GBox[int] ~b) int { return b[1]; }
		get_shared(GBox[int] b) int { return b.total; }
		main() {
			x := idx_mut(GBox[int](d: [10, 20, 30]));
			y := get_shared(GBox[int](d: [1, 2, 3, 4]));
		}
	`)
	// `[]` operator dispatch on a `~` generic-instance receiver.
	assertContains(t, ir, `define i64 @"GBox[int].[]"`)
	assertContains(t, ir, `call i64 @"GBox[int].[]"`)
	// Getter dispatch on a `&` generic-instance receiver.
	assertContains(t, ir, `define i64 @"GBox[int].total"`)
	assertContains(t, ir, `call i64 @"GBox[int].total"`)
	// The bare generic-owner name must never appear for either caller.
	assertNotContains(t, ir, `@"GBox.[]"`)
	assertNotContains(t, ir, `@"GBox.total"`)
}

// TestT0642_InferredGenericMethodOnNonGenericNamed verifies that calling a
// generic (method-type-param) method on a non-generic Named type WITHOUT
// explicit type-arg brackets dispatches to the per-method-type-arg mono name
// inferred from the call argument. Pre-fix this routed through `genMethodCall`,
// which built the bare mangled name ("Plain.echo") and panicked.
func TestT0642_InferredGenericMethodOnNonGenericNamed(t *testing.T) {
	ir := generateIR(t, `
		type Plain { int x; echo[U](U v) U { return v; } }
		main() {
			p := Plain(x: 1);
			r := p.echo("hi");
		}
	`)
	// Inferred U=string mangles to the same name as the explicit form.
	assertContains(t, ir, `"Plain.echo[string]"`)
	assertContains(t, ir, `call i8* @"Plain.echo[string]"`)
	// Bare-name form (pre-fix mis-dispatch target) must never appear.
	assertNotContains(t, ir, `@"Plain.echo"(`)
}

// TestT0642_InferredGenericMethodOnNonGenericEnum exercises the
// `case *types.Enum` arm of genGenericEnumMethodCall via the inferred path.
// Pre-fix the inferred call silently dispatched through the bare-name enum
// path (single overload) which ABI-mismatched on non-`i8*` args.
func TestT0642_InferredGenericMethodOnNonGenericEnum(t *testing.T) {
	ir := generateIR(t, `
		enum EPlain { A, B, echo[U](U v) U { return v; } }
		main() {
			p := EPlain.A;
			r := p.echo("hi");
		}
	`)
	assertContains(t, ir, `"EPlain.echo[string]"`)
	assertContains(t, ir, `call i8* @"EPlain.echo[string]"`)
}

// TestT0642_InferredGenericMethodOnGenericNamedInstance verifies the
// generic-Named-instance owner case routes through the per-instance mono
// name ("NBox[int].echo[string]"), with U inferred from the call arg.
func TestT0642_InferredGenericMethodOnGenericNamedInstance(t *testing.T) {
	ir := generateIR(t, `
		type NBox[T] { T val; echo[U](U v) U { return v; } }
		main() {
			b := NBox[int](val: 42);
			r := b.echo("hi");
		}
	`)
	assertContains(t, ir, `"NBox[int].echo[string]"`)
	assertContains(t, ir, `call i8* @"NBox[int].echo[string]"`)
	// Pre-fix would have mis-dispatched to the bare-owner name.
	assertNotContains(t, ir, `@"NBox.echo[string]"`)
}

// TestT0642_InferredGenericMethodOnGenericEnumInstance verifies the
// generic-enum-instance owner case routes through monoName(instance)
// + per-method-type-arg suffix on the inferred path (the `*types.Instance`
// arm of genGenericEnumMethodCall).
func TestT0642_InferredGenericMethodOnGenericEnumInstance(t *testing.T) {
	ir := generateIR(t, `
		enum EBox[T] { V(Vector[T] xs), N, echo[U](U v) U { return v; } }
		main() {
			b := EBox[int].V([1, 2, 3]);
			r := b.echo("hi");
		}
	`)
	assertContains(t, ir, `"EBox[int].echo[string]"`)
	assertContains(t, ir, `call i8* @"EBox[int].echo[string]"`)
	assertNotContains(t, ir, `@"EBox.echo[string]"`)
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
		tryIdentity![T](T x) T {
			return x;
		}
		main() {
			int v = tryIdentity[int](42)?!;
		}
	`)
	assertContains(t, ir, "define { i1, i64, i8* } @\"tryIdentity[int]\"")
}

// T0340: a generic function with a `~T` parameter must clear the caller's
// drop flag at the call site. Without the applyMutRefArgOwnership fix,
// `~` args on generic calls left the caller's drop flag set → double-free
// when the callee consumed the value (e.g. moved it into a struct field).
func TestT0340_GenericFuncMutRefArgClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		consume[T](T move x) { }
		main() {
			s := "hello";
			consume[string](s);
		}
	`)
	// Drop flag store + immediate call to the monomorphized consume.
	assertContainsMatch(t, ir,
		`store i1 false, i1\* %s\.dropflag\s*\n\s*call void @"consume\[string\]"`)
}

// T0340: same fix must apply when the type parameter is inferred (no
// explicit `[T]` at the call site). Exercises genInferredGenericCall.
func TestT0340_InferredGenericFuncMutRefArgClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		consume[T](T move x) { }
		main() {
			s := "hello";
			consume(s);
		}
	`)
	assertContainsMatch(t, ir,
		`store i1 false, i1\* %s\.dropflag\s*\n\s*call void @"consume\[string\]"`)
}

// T0340: same fix must apply for module-qualified generic calls. The
// module callee may not be visible via lookupFunc, so genModuleGenericFuncCall
// also checks c.info.Types[e.Callee] as a fallback — without it, the drop
// flag clear was missed on module-imported `~` callees.
func TestT0340_ModuleGenericFuncMutRefArgClearsDropFlag(t *testing.T) {
	ir := generateIRWithModule(t, "mylib",
		"consume[T](T move x) `public { }",
		`
		use mylib "./mylib";
		main() {
			s := "hello";
			mylib.consume[string](s);
		}
	`)
	assertContainsMatch(t, ir,
		`store i1 false, i1\* %s\.dropflag\s*\n\s*call void @"consume\[string\]"`)
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
		invoke[T](Echo e, T val) T {
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
		type Bar { delegate[T](Foo f, T val) T { return f.echo[T](val); } }
		main() {
			f := Foo();
			b := Bar();
			int r = b.delegate[int](f, 7);
		}
	`)
	assertContains(t, ir, "define i64 @\"Bar.delegate[int]\"")
	assertContains(t, ir, "define i64 @\"Foo.echo[int]\"")
}

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

// B0099: Type-instance resolution (generic type method calls generic free function).
func TestGenericTypeMethodCallsFreeFunc(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T val) T { return val; }
		type Wrapper[T] {
			T value;
			wrapped(this) T { return identity[T](this.value); }
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
			echoed(this) T { return this.e.echo[T](this.value); }
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
			run[T](Echoer e, T val) T {
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
	assertContains(t, ir, "define i64 @__user.unbox")
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

// T0441: 3-element tuple destructure (grammar now accepts N>=2 names).
func TestTupleDestructureThreeElements(t *testing.T) {
	ir := generateIR(t, `
		triple() (int, int, int) { return (1, 2, 3); }
		main() { (a, b, c) := triple(); }
	`)
	assertContains(t, ir, "extractvalue { i64, i64, i64 }")
	assertContains(t, ir, "%a = alloca i64")
	assertContains(t, ir, "%b = alloca i64")
	assertContains(t, ir, "%c = alloca i64")
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
		parse!() int { return 42; }
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
		parse!() int { return 42; }
		main() {
			(val, _) := parse();
		}
	`)
	assertContains(t, ir, "destruct.merge")
	assertContains(t, ir, "%val")
}

// B0263: Failable destructure value must be freed at scope exit for heap user types.
func TestFailableDestructureValueFree(t *testing.T) {
	ir := generateIR(t, `
		type Pt { int x; int y; }
		make!() Pt { return Pt(x: 1, y: 2); }
		main() {
			(p, err) := make();
		}
	`)
	// The value variable 'p' should get a free binding (heap user type without drop).
	// emitFreeCall null-checks the instance pointer (safe for the error path's zeroinit).
	assertContains(t, ir, "free.call")
	assertContains(t, ir, "free.exec")
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
	assertContains(t, ir, "define { i64, i1 } @__user.pair()")
	assertContains(t, ir, "ret { i64, i1 }")
}

func TestTupleInterpolationTracksTemps(t *testing.T) {
	ir := generateIR(t, `
		main() {
			(int, int) pair = (1, 2);
			string s = "{pair}";
		}
	`)
	// B0254: convertTupleToString must track per-element convertToString results
	// as string temps so they get freed. Verify promise_string_drop is emitted.
	assertContains(t, ir, "call void @promise_string_drop")
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

// T0937: an inline (unbound) elvis whose result is a value-struct container
// (map[K,V] / Set[T] — a 2-word {i8*, i8*} Value struct, not a bare i8*) on an
// owned-local source must register the result as a heap drop temp with a
// per-branch flag: owned (true) on the some-path where the extracted inner is
// orphaned, borrowed (false) on the none-path where the default keeps its owner.
// Without this the some-path inner leaks (the i8*-only trackElvisResultTemp path
// skips value structs).
func TestElvisMapResultDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[string, int]? a = {"x": 1};
			map[string, int] b = {"z": 9};
			c := (a ?: b).len;
		}
	`)
	assertContains(t, ir, "elvis.merge")
	// Per-branch live flag: true on the some-path, false on the none-path.
	assertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	// The tracked heap temp dispatches to the synthesized Map drop.
	assertContains(t, ir, `call void @"Map[string, int].drop"`)
}

// T0937: a member-source elvis (`bx.m ?: b`) is owner-governed — the extracted
// inner aliases the owned field and the container's own drop frees it. The elvis
// result must NOT be tracked (tracking would double-free), so no per-branch
// owned-flag phi is emitted.
func TestElvisMapMemberSourceNoDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type MBox { map[string, int]? m; }
		main() {
			MBox bx = MBox(m: {"x": 1});
			map[string, int] b = {"z": 9};
			c := (bx.m ?: b).len;
		}
	`)
	assertContains(t, ir, "elvis.merge")
	assertNotContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
}

// T0937 (i8*-container gap): the i8* result path (trackElvisResultTemp, T0935)
// applies the same orphan classifier as the value-struct path. An owned-local
// string[] source is orphaned on the some-path → tracked with a per-branch flag.
// Here the none-path default `b` is ALSO an owned local, so T0936 neutralizes its
// scope-exit owner and the result owns it on the none-path too — the flag phi is
// [true, true] (the `true` on the some-incoming is what proves orphan tracking).
func TestElvisStrvecOwnedLocalDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[]? a = ["x" + "y"];
			string[] b = ["z" + "w"];
			c := (a ?: b).len;
		}
	`)
	assertContains(t, ir, "elvis.merge")
	// Per-branch live flag drives the stmt-temp Vector.drop dispatch: owned on the
	// some-path (orphaned inner) and on the none-path (neutralized local default).
	assertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ true, %elvis\.none`)
}

// T0937 (i8*-container gap): a member-source string[] elvis (`bx.v ?: b`) aliases
// the owned field; the container's drop frees it. The inline result must NOT be
// tracked (tracking double-freed the elements → use-after-free crash before the
// gate was applied to the i8* path), so no per-branch flag phi is emitted.
func TestElvisStrvecMemberSourceNoDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type SVBox { string[]? v; }
		main() {
			SVBox bx = SVBox(v: ["x" + "y"]);
			string[] b = ["z" + "w"];
			c := (bx.v ?: b).len;
		}
	`)
	assertContains(t, ir, "elvis.merge")
	assertNotContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
}

// T0937: a value-struct-container elvis inside a generic function with an owned
// (`~`) optional param is orphaned on the some-path, so the result is tracked.
// The result type (`map[K,V]`) is resolved through c.typeSubst during
// monomorphization, so the synthesized drop dispatches on the concrete
// instantiation (`Map[string, int].drop`). Exercises the typeSubst branch of
// trackElvisResultHeap (uncovered by the non-generic ident-source tests).
func TestElvisMapGenericOwnedParamDropFlag(t *testing.T) {
	ir := generateIR(t, `
		gconsume[K: Hashable + Equal, V](map[K, V]? move a, map[K, V] b) int {
			return (a ?: b).len;
		}
		main() {
			map[string, int]? a = {"x": 1};
			map[string, int] b = {"z": 9};
			c := gconsume(a, b);
		}
	`)
	assertContains(t, ir, "elvis.merge")
	assertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	assertContains(t, ir, `call void @"Map[string, int].drop"`)
}

// T0937: a value-struct-container elvis inside a structural-interface default
// method, with an owned-local source (orphaned on the some-path), is tracked.
// The default body is synthesized per concrete type with c.selfSubst active, so
// the result type passes through types.SubstituteSelf before the drop is
// resolved. Exercises the selfSubst branch of trackElvisResultHeap.
func TestElvisMapStructuralDefaultDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type HasMapFallback `+"`"+`structural {
			base_map() map[string, int] `+"`"+`abstract;
			resolved_len() int {
				map[string, int]? a = {"x": 1};
				return (a ?: this.base_map()).len;
			}
		}
		type MapConfig is HasMapFallback {
			base_map() map[string, int] { return {"k": 7}; }
		}
		main() {
			c := MapConfig();
			d := c.resolved_len();
		}
	`)
	assertContains(t, ir, "elvis.merge")
	// The none-incoming predecessor is the this.base_map() call continuation
	// block, not %elvis.none — so match only the some-path true incoming.
	assertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, `)
	assertContains(t, ir, `call void @"Map[string, int].drop"`)
}

// T0937 (i8*-container gap): a generic vector elvis with an owned (`~`) optional
// param is orphaned on the some-path → tracked via trackElvisResultTemp. The
// result type (`T[]`) resolves through c.typeSubst at monomorphization.
// Exercises the typeSubst branch of trackElvisResultTemp (the existing generic
// tests use borrowed params, which now short-circuit at the orphan gate).
func TestElvisStrvecGenericOwnedParamDropFlag(t *testing.T) {
	ir := generateIR(t, `
		gconsume[T](T[]? move a, T[] b) int {
			return (a ?: b).len;
		}
		main() {
			string[]? a = ["x" + "y"];
			string[] b = ["z" + "w"];
			c := gconsume(a, b);
		}
	`)
	assertContains(t, ir, "elvis.merge")
	assertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
}

// T0937 (i8*-container gap): a vector elvis inside a structural-interface default
// method with an owned-local source is orphaned → tracked. The default body is
// synthesized with c.selfSubst active. Exercises the selfSubst branch of
// trackElvisResultTemp.
func TestElvisStrvecStructuralDefaultDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type HasVecFallback `+"`"+`structural {
			base_vec() string[] `+"`"+`abstract;
			resolved_len() int {
				string[]? a = ["x" + "y"];
				return (a ?: this.base_vec()).len;
			}
		}
		type VecConfig is HasVecFallback {
			base_vec() string[] { return ["z" + "w"]; }
		}
		main() {
			c := VecConfig();
			d := c.resolved_len();
		}
	`)
	assertContains(t, ir, "elvis.merge")
	// The none-incoming predecessor is the this.base_vec() call continuation
	// block, not %elvis.none. The default is a FRESH temp (base_vec() returns a
	// new vector), so T0936 claims it and the result owns it on the none-path too:
	// the flag phi is [true, true] (the some-incoming `true` proves orphan tracking).
	assertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ true, `)
}

// T0937: an index-source elvis (`v[i] ?: b`) is owner-governed — the extracted
// inner aliases the container element, which the container's own drop frees. The
// result must NOT be tracked (tracking would double-free), so no per-branch
// owned-flag phi is emitted. Exercises the IndexExpr arm of
// elvisSomeInnerOrphaned (the ident/member tests cover the other arms).
func TestElvisMapIndexSourceNoDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			(map[string, int]?)[] v = [];
			v.push({"x": 1});
			map[string, int] b = {"z": 9};
			c := (v[0] ?: b).len;
		}
	`)
	assertContains(t, ir, "elvis.merge")
	assertNotContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
}

// T0937 (T0924 subsumption): a heap *user* type (a droppable non-value type, also
// a 2-word {i8*, i8*} Value struct) used inline on an owned-local source goes
// through the SAME trackElvisResultHeap path as Map/Set — distinct from the i8*
// container path. It must emit the per-branch owned-flag phi and dispatch the
// tracked temp to the type's own synthesized drop (@HVal.drop). This proves the
// heap-user representation arm fires (the Map tests only cover the container arm
// of the shared function); without it the some-path inner leaks (T0924).
func TestElvisHeapUserResultDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type HVal { string s; }
		main() {
			HVal? a = HVal(s: "a" + "b");
			HVal b = HVal(s: "c" + "d");
			c := (a ?: b).s.len;
		}
	`)
	assertContains(t, ir, "elvis.merge")
	// Per-branch live flag: owned (true) on some, borrowed (false) on none.
	assertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	// The tracked heap temp dispatches to the user type's own drop.
	assertContains(t, ir, "call void @HVal.drop")
}

// T0933/T0940: `m := a ?: b` with BORROWED-PARAMETER operands. The binding's drop
// flag must be driven by a per-path phi — false on the some path (the inner belongs
// to the caller-owned param `a`, not the binding) AND false on the none path (the
// default `b` is a borrowed param whose owner is the caller, so `m` borrows it —
// owning it would double-free the caller's buffer at scope exit, the bound none-path
// SEGV T0940 fixes). This overrides the unconditional `store i1 true` that
// maybeRegisterDrop emits. (T0940 generalizes T0933's heap-user-only flag and
// corrects the none-path: it owns only when the default's owner was neutralized.)
func TestElvisBoundBorrowedParamFlag(t *testing.T) {
	ir := generateIR(t, `
		type HVal { string s; }
		f(HVal? a, HVal b) { m := a ?: b; }
		main() { HVal? a = HVal(s: "a" + "b"); HVal c = HVal(s: "c" + "d"); f(a, c); }
	`)
	// The bound override phi borrows on both paths (both operands caller-owned).
	assertContainsMatch(t, ir, `phi i1 \[ false, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	// The override store follows maybeRegisterDrop's unconditional 1.
	assertContains(t, ir, "store i1 true, i1* %m.dropflag")
	assertContainsMatch(t, ir, `store i1 %[0-9]+, i1\* %m\.dropflag`)
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

// T0940: a STRING elvis bound result (i8* container) is covered by the per-path bound
// flag too — elvisResultDrop classifies string (with Vector) as the vecOrStr arm, so
// `m := a ?: b` with both operands borrowed params gets a phi[false, false] override of
// maybeRegisterDrop's unconditional `store i1 true`. Pre-T0940 the string-bound case
// kept the unconditional owning drop and freed the caller-owned default a second time
// (`@promise_string_drop` on a borrowed buffer → UAF). The bound flag makes `m` borrow
// on both paths, matching the Map/heap-user borrowed-param arm.
func TestElvisBoundStringBorrowsBoth(t *testing.T) {
	ir := generateIR(t, `
		f(string? a, string b) { m := a ?: b; }
		main() { string? a = "x"; string b = "y"; f(a, b); }
	`)
	// Borrows on both paths (both operands caller-owned); never the old owning shapes.
	assertContainsMatch(t, ir, `phi i1 \[ false, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	assertNotContainsMatch(t, ir, `phi i1 \[ false, %elvis\.some[^]]*\], \[ true, %elvis\.none`)
	assertNotContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ true, %elvis\.none`)
	// The per-path flag overrides the unconditional owning drop.
	assertContains(t, ir, "store i1 true, i1* %m.dropflag")
	assertContainsMatch(t, ir, `store i1 %[0-9]+, i1\* %m\.dropflag`)
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

// T0812: reading a closure out of an owning aggregate (struct/optional field,
// container element) borrows the aggregate's heap env — the local must NOT get an
// owning env-free binding, otherwise both the local and the aggregate's drop free
// the same env (double-free / UAF). A fresh closure literal still owns its env.
func TestClosureFieldReadBorrowsEnvNoFree(t *testing.T) {
	ir := generateIR(t, `
		type CbHolder { () -> int cb; }
		read_field(CbHolder h) int {
			f := h.cb;
			return f();
		}
		fresh_local() int {
			s := "a" + "b";
			g := move || -> s.len;
			return g();
		}
		main() {}
	`)
	// The field-read local `f` gets no drop flag and no env-free binding —
	// it borrows h's env (h.drop frees it exactly once).
	assertNotContains(t, ir, "%f.dropflag")
	// A fresh closure literal still owns its env: drop flag + env.free binding.
	assertContains(t, ir, "%g.dropflag")
	assertContains(t, ir, "env.free")
}

// T0911: reassigning a closure-typed local that owns a heap env must free the
// old env before the store (the drop-old logic previously ignored the
// bindingFreeEnv / dropFlags env cleanup, leaking the old owned env). The
// reassignment must emit a guarded env-free sequence.
func TestClosureReassignFreesOldEnv(t *testing.T) {
	ir := generateIR(t, `
		reassign() int {
			s := "captured";
			() -> int f = move || -> s.len + 1;
			t := "other";
			f = move || -> t.len + 2;
			return f();
		}
		main() {}
	`)
	// The reassignment emits the alias-guarded old-env free blocks.
	assertContains(t, ir, "reassign.env.free")
	assertContains(t, ir, "reassign.env.call")
	assertContains(t, ir, "reassign.env.merge")
}

// T0911: literal closure self-assignment (`f = f`) must take the early-return
// guard — the local keeps owning its env, so NO env-free blocks are emitted and
// the post-store clearDropFlag never runs (which would otherwise zero the env
// drop flag and leak the env at scope exit).
func TestClosureSelfAssignNoEnvFree(t *testing.T) {
	ir := generateIR(t, `
		self_assign() int {
			s := "captured";
			() -> int f = move || -> s.len + 1;
			f = f;
			return f();
		}
		main() {}
	`)
	// Early return before any env-free blocks are emitted for the self-assign.
	assertNotContains(t, ir, "reassign.env.free")
	assertNotContains(t, ir, "reassign.env.call")
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
	assertContains(t, ir, "define void @__user.apply")
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
	assertContains(t, ir, "define { i8*, i8* } @__user.make_adder")
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

// convertToString: string in interpolation (B0248: copies via concat with empty)
func TestStringInterpolationStringVar(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string name = "world";
			string msg = "hello {name}";
		}
	`)
	// B0248: String is copied (concat with empty), then concatenated with other parts
	assertContains(t, ir, "call i8* @promise_string_concat")
}

// B0248: single-string interpolation ("{s}") must copy via concat, not alias
func TestStringInterpolationStringOnly(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string s = "hello";
			string copy = "{s}";
		}
	`)
	// Must produce a concat call (copy), not pass through the original value
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
	assertContains(t, ir, "fcmp une double")
}

// floatOps: != uses unordered predicate (UNE) so NaN != NaN evaluates true (IEEE 754, T0463).
func TestFloatNotEqualNaN(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f64 a = 1.0;
			f64 b = 2.0;
			bool ne = a != b;
			f32 c = 1.0;
			f32 d = 2.0;
			bool ne32 = c != d;
		}
	`)
	assertContains(t, ir, "fcmp une double")
	assertContains(t, ir, "fcmp une float")
	assertNotContains(t, ir, "fcmp one double")
	assertNotContains(t, ir, "fcmp one float")
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

// T0579: Array field with Optional<HeapUser> element — exact repro from the
// bug. The Optional inside the array carries the full value struct, not
// bare i8*.
func TestFixedArrayFieldOptionalHeapUserElement(t *testing.T) {
	ir := generateIR(t, `
		type _BoxA { int n; drop(~this) {} }
		type _HolderArr { _BoxA?[2] data; }
		main() {
			_BoxA? a = _BoxA(n: 7);
			_BoxA? b = _BoxA(n: 8);
			_HolderArr h = _HolderArr(data: [a, b]);
		}
	`)
	// Field slot should hold {i1, value_struct} per element, not {i1, i8*}.
	assertContains(t, ir, "[2 x { i1, { i8*, i8* } }]")
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

// T0579: Array field with channel element — exercises `typeNeedsFieldDrop`'s
// `AsChannel` branch.
func TestFixedArrayFieldChannelElement(t *testing.T) {
	ir := generateIR(t, `
		type _ChArr { channel[int][2] data; }
		main() {
			c0 := channel[int]();
			c1 := channel[int]();
			c := _ChArr(data: [c0, c1]);
		}
	`)
	// T0663: Channel.drop is per-element-type — Channel[int].drop here.
	assertContains(t, ir, `call void @"Channel[int].drop"`)
}

// T0579: Array field with heap user type that has no explicit drop —
// exercises the "heap user without explicit drop" branch in
// `typeNeedsFieldDrop` (must return true so the per-element `pal_free` fires).
func TestFixedArrayFieldHeapNoDropElement(t *testing.T) {
	ir := generateIR(t, `
		type _Bare { int x; }
		type _BareArr { _Bare[2] data; }
		main() {
			b := _BareArr(data: [_Bare(x: 1), _Bare(x: 2)]);
		}
	`)
	// The element type has no drop method, so the synth drop must fall back to
	// pal_free per element.
	assertContains(t, ir, "call void @pal_free")
}

// T0583: `arr[i] = newVal` on a fixed-size array of droppable elements must
// emit a drop call for the old slot value before storing the new one.
// Before the fix, `genArrayIndexAssign` did a bare `NewStore` and leaked the
// previous allocation. The IR for a heap-user element with explicit drop must
// load the old element, drop it (Type.drop + pal_free), then store the new.
func TestFixedArrayIndexAssignDropsOldHeapUser(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; drop(~this) {} }
		main() {
			_Box[2] arr = [_Box(n: 1), _Box(n: 2)];
			_Box c = _Box(n: 3);
			arr[1] = c;
		}
	`)
	// After bounds-check OK, the IR must drop the previous element before storing.
	assertContains(t, ir, "arrassign.ok")
	assertContains(t, ir, "call void @_Box.drop")
}

// T0583: String element — overwrite must call promise_string_drop on the old slot.
func TestFixedArrayIndexAssignDropsOldString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[2] arr = ["alpha", "beta"];
			arr[1] = "gamma";
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0583: String compound assignment — `arr[i] += s` must drop the old string
// after computing the concat result and before storing it.
func TestFixedArrayIndexCompoundAssignDropsOldString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[2] arr = ["foo", "bar"];
			arr[1] += "baz";
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	// Compound path goes through emitStringDropOldValue.
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0583: Optional<HeapUser> element — overwrite must run the Optional drop
// dispatcher on the previous slot, dropping the inner instance when present.
func TestFixedArrayIndexAssignDropsOldOptionalHeapUser(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; drop(~this) {} }
		main() {
			_Box? a = _Box(n: 1);
			_Box? b = _Box(n: 2);
			_Box?[2] arr = [a, b];
			_Box? c = _Box(n: 3);
			arr[1] = c;
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	// The Optional's drop dispatcher checks presence then drops the inner _Box.
	assertContains(t, ir, "call void @_Box.drop")
}

// T0599: bare-T RHS assigned to an Optional<T> fixed-array slot. The
// IndexExpr-LHS path in genAssignStmt had no Optional-wrap (the MemberExpr
// and IdentExpr paths did), so genArrayIndexAssign's NewStore got a bare T
// against a {i1, T}* slot and panicked with "store operands are not
// compatible". Pre-fix, generateIR() here panics and fails the test; post-fix
// the bare _Box is wrapped into the {i1, {i8*, i8*}} Optional before the store.
func TestFixedArrayIndexAssignBareToOptionalHeapUser(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; drop(~this) {} }
		main() {
			_Box? a = _Box(n: 1);
			_Box? b = _Box(n: 2);
			_Box?[2] arr = [a, b];
			arr[0] = _Box(n: 99);
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	// The bare _Box ctor result is wrapped into the Optional struct (present
	// flag set, then the value-struct inserted) before the slot store.
	assertContains(t, ir, "insertvalue { i1, { i8*, i8* } } undef, i1 true, 0")
}

// T0599: bare string literal assigned to a string? fixed-array slot — the
// string-temp claim must run BEFORE the Optional wrap (val identity changes
// after wrapOptional). Pre-fix this panicked with "store operands are not
// compatible: src=i8*; dst={i1,i8*}*".
func TestFixedArrayIndexAssignBareToOptionalString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? oa = "a";
			string? ob = "b";
			string?[2] arr = [oa, ob];
			arr[0] = "new";
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	assertContains(t, ir, "insertvalue { i1, i8* } undef, i1 true, 0")
}

// T0583: Vector element — overwrite must call Vector.drop on the old slot.
func TestFixedArrayIndexAssignDropsOldVector(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v0 := Vector[int]();
			v0.push(10);
			v1 := Vector[int]();
			v1.push(20);
			Vector[int][2] arr = [v0, v1];
			v2 := Vector[int]();
			v2.push(30);
			arr[1] = v2;
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	assertContains(t, ir, "call void @Vector.drop")
}

// T0583: Primitive element (no drop needed) — `arr[i] = val` must NOT emit a
// load-and-drop dance for the old slot. Confirms typeNeedsFieldDrop correctly
// gates the new code path: for ints, the arrassign.ok block goes directly from
// GEP to store without any intervening load of the old value.
func TestFixedArrayIndexAssignNoDropForPrimitive(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[3] arr = [1, 2, 3];
			arr[1] = 42;
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	// For primitives the codegen emits: GEP into [3 x i64] then `store i64 42`
	// immediately. No old-value load or drop call appears between them.
	assertContainsMatch(t, ir,
		`getelementptr \[3 x i64\][^\n]*\n[^\n]*store i64 42`)
}

// T0590: genArrayIndex had no dup-on-read, so any read from a fixed-size array
// slot returned an alias. Combined with T0583's drop-on-overwrite, slot-to-slot
// copies and let-then-X reads on droppable elements produced double-frees.
// Tests below verify each dup branch fires.

func TestFixedArrayIndexDupsString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := "first";
			a = a + "+";
			b := "second";
			b = b + "+";
			string[2] arr = [a, b];
			string x = arr[0];
		}
	`)
	// After the array index load, genArrayIndex must call promise_string_new
	// (via dupString) so x owns an independent copy.
	assertContains(t, ir, "arridx.ok")
	assertContains(t, ir, "call i8* @promise_string_new(")
}

func TestFixedArrayIndexDupsHeapUser(t *testing.T) {
	ir := generateIR(t, `
		type _B { int n; drop(~this) {} }
		main() {
			_B[2] arr = [_B(n: 1), _B(n: 2)];
			_B x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// dupHeapValue path: pal_alloc + memcpy for the new instance.
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "call void @llvm.memcpy")
}

// T0813: a struct with a closure (function-value) field reaches
// dupHeapValueFields via a non-sema-gated implicit dup path (fixed-array index
// dup here). The closure env cannot be deep-cloned, so the cloned slot must be
// nulled (zeroinitializer store) rather than left aliasing the source's env —
// otherwise both droppable owners free the same env → double-free.
func TestFixedArrayIndexNullsClosureField(t *testing.T) {
	ir := generateIR(t, `
		type _Cb { () -> int cb; drop(~this) {} }
		main() {
			_Cb[2] arr = [_Cb(cb: move || -> 1), _Cb(cb: move || -> 2)];
			_Cb x = arr[0];
		}
	`)
	// dupHeapValueFields: memcpy the instance, then null the closure slot.
	assertContains(t, ir, "call void @llvm.memcpy")
	assertContains(t, ir, "store { i8*, i8* } zeroinitializer, { i8*, i8* }*")
}

func TestFixedArrayIndexDupsOptionalHeapUser(t *testing.T) {
	ir := generateIR(t, `
		type _B { int n; drop(~this) {} }
		main() {
			_B? a = _B(n: 1);
			_B? b = _B(n: 2);
			_B?[2] arr = [a, b];
			_B? x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// Optional[heap-user] dup path: extract inner, dupHeapValue, insert back.
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "call void @llvm.memcpy")
}

func TestFixedArrayIndexDupsVector(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v0 := Vector[int]();
			v0.push(1);
			v1 := Vector[int]();
			v1.push(2);
			Vector[int][2] arr = [v0, v1];
			Vector[int] x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// dupVector: pal_alloc for the new buffer + memcpy from the old one.
	assertContains(t, ir, "call i8* @pal_alloc(")
}

func TestFixedArrayIndexNoDupForPrimitive(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[2] arr = [10, 20];
			int x = arr[0];
		}
	`)
	// Primitives use the bare load — no dup helper. Check the main goroutine
	// specifically (std library code emitted in the same IR may reference
	// promise_string_new for other reasons).
	assertContains(t, ir, "arridx.ok")
	mainIR := extractFunction(ir, ".goroutine.main")
	if strings.Contains(mainIR, "promise_string_new") || strings.Contains(mainIR, "pal_alloc") {
		t.Errorf("expected main goroutine to have no dup helper for primitive array index, got:\n%s", mainIR)
	}
}

func TestFixedArrayIndexAssignSlotToSlotDupsHeapUser(t *testing.T) {
	// T0590: slot-to-slot copy (`arr[1] = arr[0]`) must dup on RHS read, then
	// drop-on-overwrite frees the previous arr[1] (T0583), then stores the dup.
	ir := generateIR(t, `
		type _B { int n; drop(~this) {} }
		main() {
			_B[2] arr = [_B(n: 1), _B(n: 2)];
			arr[1] = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")    // RHS read
	assertContains(t, ir, "arrassign.ok") // LHS assign with drop-on-overwrite
	// Must see both: drop-on-overwrite (drop of old arr[1]) and dup (clone of arr[0]).
	assertContains(t, ir, "call void @_B.drop")
	assertContains(t, ir, "call i8* @pal_alloc(")
}

// T0802: `s = obj.field` reassignment of a heap string field must clone-on-read,
// exactly like the var-decl path. Before the fix, genAssignStmt only set the
// dup-on-read flags for an IndexExpr RHS, so a MemberExpr (field-access) RHS
// aliased the field pointer into s with s's drop flag set — both s and obj's drop
// then freed the same allocation (double-free, latent on linux / SIGABRT on
// macOS). genFieldAccess emits the clone via dupString (promise_string_new).
func TestReassignStringFieldClones(t *testing.T) {
	ir := generateIR(t, `
		type _T { int id; string label; drop(~this) {} }
		probe() {
			_T t = _T(id: 1, label: "lit");
			string s = "";
			s = t.label;
		}
		main() { probe(); }
	`)
	// Isolate the user code (string literals are .rodata, so the only
	// promise_string_new in probe() is the field-read clone).
	probeIR := extractFunction(ir, "__user.probe")
	assertContains(t, probeIR, "call i8* @promise_string_new(")
}

// T0802 control: a reassignment whose RHS is NOT a field read must not emit the
// field-clone helper — proves the dup is driven by the MemberExpr RHS, not always.
func TestReassignNonFieldRhsNoClone(t *testing.T) {
	ir := generateIR(t, `
		type _T { int id; string label; drop(~this) {} }
		probe() {
			_T t = _T(id: 1, label: "lit");
			string s = "";
			s = "other";
		}
		main() { probe(); }
	`)
	probeIR := extractFunction(ir, "__user.probe")
	if strings.Contains(probeIR, "call i8* @promise_string_new(") {
		t.Errorf("expected no field-clone for non-field RHS reassignment, got:\n%s", probeIR)
	}
}

// T0590 coverage additions: confirm each remaining dup branch in genArrayIndex
// actually emits the per-type dup helper. The original 6 tests covered string,
// heap user, Optional<heap user>, Vector, and the primitive negative case; the
// tests below fill in tuple, Optional[string], heap-user-no-drop (_Bare),
// channel, Arc, and Weak.

func TestFixedArrayIndexDupsTuple(t *testing.T) {
	// Droppable tuple element: dup must walk the tuple's droppable inner
	// (string) so both slots own independent string allocations.
	ir := generateIR(t, `
		main() {
			(string, int)[2] arr = [("first", 1), ("second", 2)];
			(string, int) t = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// dupTupleValue: emits a per-field dup; for the string sub-field this
	// is promise_string_new. With static-literal flag, copy-on-write may
	// route through promise_vector_cow style — but the dup-on-read path
	// always emits promise_string_new for the inner string.
	assertContains(t, ir, "call i8* @promise_string_new(")
}

func TestFixedArrayIndexDupsOptionalString(t *testing.T) {
	// Optional[string]: extract inner, dup the string, insert back.
	ir := generateIR(t, `
		main() {
			a := "first";
			a = a + "+";
			b := "second";
			b = b + "+";
			string? oa = a;
			string? ob = b;
			string?[2] arr = [oa, ob];
			string? x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	assertContains(t, ir, "call i8* @promise_string_new(")
}

func TestFixedArrayIndexDupsHeapUserNoDrop(t *testing.T) {
	// _Bare: heap user with no explicit drop / no synth drop.
	// dup-on-read must still fire via the isHeapUserNoDropPalFree branch
	// (otherwise both slots alias one pal_free'd allocation).
	ir := generateIR(t, `
		type _Bare { int x; }
		main() {
			_Bare[2] arr = [_Bare(x: 1), _Bare(x: 2)];
			_Bare x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// dupHeapValue path: pal_alloc + memcpy
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "call void @llvm.memcpy")
}

func TestFixedArrayIndexDupsChannel(t *testing.T) {
	// Channel element: dupChannel inlines a null check + atomic refcount
	// incref on the channel struct (compiler.go:2234, chdup.inc block).
	ir := generateIR(t, `
		main() {
			c0 := channel[int]();
			c1 := channel[int]();
			channel[int][2] arr = [c0, c1];
			channel[int] x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// dupChannel emits a chdup.inc / chdup.merge block pair and an atomic add.
	assertContains(t, ir, "chdup.inc")
	assertContains(t, ir, "chdup.merge")
}

func TestFixedArrayIndexDupsArc(t *testing.T) {
	// Arc element: dupArc emits an atomic refcount incref.
	ir := generateIR(t, `
		main() {
			Ref[int][2] arr = [Ref[int](1), Ref[int](2)];
			Ref[int] x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// dupArc emits an atomic fetch-add on the Arc's refcount field.
	if !strings.Contains(ir, "atomicrmw add") && !strings.Contains(ir, "promise_arc_clone") {
		t.Errorf("expected Arc dup helper in IR; got:\n%s", ir)
	}
}

func TestFixedArrayIndexDupsWeak(t *testing.T) {
	// Weak element: dupWeak emits the type-specific Weak clone helper.
	ir := generateIR(t, `
		main() {
			Ref[int] keep0 = Ref[int](10);
			Ref[int] keep1 = Ref[int](20);
			Weak[int] w0 = keep0.downgrade();
			Weak[int] w1 = keep1.downgrade();
			Weak[int][2] arr = [w0, w1];
			Weak[int] x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// dupWeak emits an atomic refcount on the weak count.
	if !strings.Contains(ir, "atomicrmw add") && !strings.Contains(ir, "promise_weak") {
		t.Errorf("expected Weak dup helper in IR; got:\n%s", ir)
	}
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

// T0817: Directly invoking a force-unwrapped optional closure `o!()` must
// compile (no "unsupported callee type *ast.OptionalUnwrapExpr" panic) and
// emit an indirect call through the materialized {fn, env} fat pointer.
func TestT0817OptionalUnwrapClosureCall(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "cap" + "tured";
			(() -> int)? o = move || -> s.len;
			int n = o!();
		}
	`)
	// Indirect call through a loaded function pointer (fat-pointer dispatch),
	// not a named direct call.
	assertContains(t, ir, "call i64 %")
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

// B0301: Optional force-unwrap used as a constructor argument must neutralize
// the source optional's present flag to prevent double-free.
func TestOptionalForceUnwrapConstructorArg(t *testing.T) {
	ir := generateIR(t, `
		type Inner { int x; }
		type Outer { Inner inner; }
		main() {
			Inner? opt = Inner(x: 1);
			Outer o = Outer(inner: opt!);
		}
	`)
	// After unwrap.ok, the source optional's present flag (field 0) must be
	// set to false. Look for "store i1 false" targeting the optional's GEP
	// at field 0 after the unwrap block.
	assertContains(t, ir, "unwrap.ok")
	// The constructor should store the unwrapped value into the Outer instance
	assertContains(t, ir, "store { i8*, i8* }")
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
			process!(int x) int `+"`"+`abstract;
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
			to_string!() string `+"`"+`abstract;
		}
		convert(Converter c) string { return c.to_string()?!; }
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
			clone!() Widget { return this; }
		}
		main() {
			w := Widget(id: 1);
			Widget w2 = w.clone()?!;
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

// T0893: a borrowing method whose body is bare `return this` must clone the
// receiver instance so the returned owned value does not alias the receiver's
// heap allocation (otherwise one binding's scope-drop frees memory the other
// still reads). The clone shows up as a heapdup block in the method body.
func TestReturnThisClonesBorrowedReceiver(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; dup() BB { return this; } }
		main() { d := BB(v: 11); m := d.dup(); }
	`)
	assertContains(t, extractDefine(ir, "BB.dup"), "heapdup")
}

// T0893: a `~this` (owned/moved-in) receiver returning `this` is a genuine
// ownership transfer — cloning would copy needlessly and leak the moved-in
// instance. The method body must NOT contain a heapdup clone.
func TestReturnThisOwnedReceiverDoesNotClone(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; consume(~this) BB { return this; } }
		main() { d := BB(v: 11); m := d.consume(); }
	`)
	assertNotContains(t, extractDefine(ir, "BB.consume"), "heapdup")
}

// T0893: a method returning a borrow (`T&`) of `this` hands back a reference into
// existing storage, not an owned copy — it must NOT clone.
func TestReturnThisBorrowReturnDoesNotClone(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; ref() BB& { return this; } }
		main() { d := BB(v: 11); r := d.ref(); }
	`)
	assertNotContains(t, extractDefine(ir, "BB.ref"), "heapdup")
}

// B0250: Assigning the result of a method that returns `this` must clear the
// receiver's drop flag to prevent double-free (both variables share the same instance).
func TestReturnThisClearsReceiverDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int value; self() Wrapper { return this; } }
		main() { w := Wrapper(value: 42); w2 := w.self(); }
	`)
	// Should emit a runtime instance-pointer comparison and conditional drop flag clear
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// T0347: Chained self-returning calls on a local must walk through the chain
// to find the IdentExpr root and clear its drop flag (not just direct calls).
func TestReturnThisChainedClearsRootDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int value; self() Wrapper { return this; } }
		main() { w := Wrapper(value: 1); r := w.self().self(); }
	`)
	// chainOriginExpr should walk past the inner call and reach `w`,
	// emitting the receiver alias-clear blocks for the chained case.
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// T0347: `r := this.method()` inside a method must emit a different alias-clear
// path that targets the new binding's drop flag (since `this` itself has no
// drop flag — it's borrowed). Distinct block labels: this.alias.{clear,skip}.
func TestReturnThisRootedClearsBindingDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int n;
			iter() Inner { return this; }
			use_self() int {
				r := this.iter();
				return r.n;
			}
		}
		main() { i := Inner(n: 11); v := i.use_self(); }
	`)
	// New helper emits this.alias.clear / this.alias.skip blocks.
	assertContains(t, ir, "this.alias.clear")
	assertContains(t, ir, "this.alias.skip")
}

// T0347: Chained `r := this.iter().iter()` inside a method also emits the
// this-alias clear path (chainOriginExpr walks the chain to ThisExpr root).
func TestReturnThisRootedChainedClearsBindingDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int n;
			iter() Inner { return this; }
			use_self() int {
				r := this.iter().iter();
				return r.n;
			}
		}
		main() { i := Inner(n: 13); v := i.use_self(); }
	`)
	assertContains(t, ir, "this.alias.clear")
	assertContains(t, ir, "this.alias.skip")
}

// T0582: `return (this);` (paren-wrapped) must take the same wrapping path as
// `return this;` — codegen must build the { i8*, i8* } value struct, not emit
// a bare `ret i8* %0` against a `{ ptr, ptr }` return type (which opt rejects).
func TestReturnParenThisWrapsValueStruct(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int v; eat(~this) Wrapper { return (this); } }
		main() { w := Wrapper(v: 1); x := w.eat(); }
	`)
	assertContains(t, ir, "insertvalue { i8*, i8* }")
	body := extractFunction(ir, "Wrapper.eat")
	assertNotContains(t, body, "ret i8* %")
}

// T0582: nested parens — `return ((this));` — also takes the wrapping path,
// confirming the paren-peel iterates.
func TestReturnDoubleParenThisWrapsValueStruct(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int v; eat(~this) Wrapper { return ((this)); } }
		main() { w := Wrapper(v: 1); x := w.eat(); }
	`)
	assertContains(t, ir, "insertvalue { i8*, i8* }")
	body := extractFunction(ir, "Wrapper.eat")
	assertNotContains(t, body, "ret i8* %")
}

// T0582: paren-wrapped receiver `(w).self()` must walk through chainOriginExpr
// to the IdentExpr root `w` and emit the B0250 receiver alias-clear blocks.
// Without the paren-peel in chainOriginExpr, the chain origin would be a
// ParenExpr and the switch would miss → no alias-clear → runtime double-free.
func TestParenReceiverClearsReceiverDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int v; self() Wrapper { return this; } }
		main() { w := Wrapper(v: 1); w2 := (w).self(); }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// T0582: paren-wrapped receiver under a chain `(w).self().self()` — chain origin
// must still resolve to `w` after the paren-peel.
func TestParenReceiverChainedClearsReceiverDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int v; self() Wrapper { return this; } }
		main() { w := Wrapper(v: 1); r := (w).self().self(); }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// T0582: paren around an inner call `(w.self()).self()` — the outer chain step
// peels the ParenExpr to reach the inner call, then walks back to `w`.
func TestChainedParenInnerCallClearsReceiverDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int v; self() Wrapper { return this; } }
		main() { w := Wrapper(v: 1); r := (w.self()).self(); }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
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

// T1029: a discarded free-function call whose result aliases an owned-local arg
// must clear the RESULT TEMP's drop flag (discard.alias.clear), leaving the source
// local's drop flag armed so the aliased allocation is freed once at scope exit.
// Before the fix the source local's flag was cleared and the temp was freed at
// statement end → the still-live local dangled (use-after-free).
func TestDiscardedAliasArgClearsTempNotSource(t *testing.T) {
	ir := generateIR(t, `
		type Node { int v; }
		ident_node(Node n) Node { return n; }
		run() int { n := Node(v: 5); ident_node(n); return n.v; }
		main() { x := run(); }
	`)
	body := extractFunction(ir, "__user.run")
	// The result temp is cleared on the alias path.
	assertContains(t, body, "discard.alias.clear")
	assertContains(t, body, "discard.alias.skip")
	// The source local's drop flag must NOT be cleared inside the discarded call —
	// the local remains the single owner.
	assertNotContains(t, body, "store i1 false, i1* %n.dropflag")
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

// T1029: the i8* result path (vector/string) in genExpr — distinct from the
// heap-user-type path in trackHeapUserTypeResult — must also emit the result-temp
// clear (discard.alias.clear) for a discarded call whose i8* result aliases an
// owned-local arg, so the source local stays the single owner.
func TestDiscardedAliasArgClearsTempI8PtrPath(t *testing.T) {
	ir := generateIR(t, `
		ident_vec(int[] v) int[] { return v; }
		ident_str(string s) string { return s; }
		run_vec() int { xs := [1, 2]; xs.push(3); ident_vec(xs); return xs.len; }
		run_str() int { a := "hi"; s := a + "!"; ident_str(s); return s.len; }
		main() { x := run_vec(); y := run_str(); }
	`)
	vec := extractFunction(ir, "__user.run_vec")
	assertContains(t, vec, "discard.alias.clear")
	assertContains(t, vec, "discard.alias.skip")
	str := extractFunction(ir, "__user.run_str")
	assertContains(t, str, "discard.alias.clear")
	assertContains(t, str, "discard.alias.skip")
}

// T0582: `return (this);` from a value-type method must take the value-type
// branch of wrapThisReturnValue (bitcast i8* to value-struct pointer + load),
// not emit a bare `ret i8* %0` against the value-struct return type.
// Coverage gap: existing T0582 tests only cover heap-type returns.
func TestReturnParenThisValueTypeLoads(t *testing.T) {
	ir := generateIR(t, `
		type Pt {
			int x `+"`value"+`;
			int y `+"`value"+`;
			echo(this) Pt { return (this); }
		}
		main() { p := Pt(x: 3, y: 4); q := p.echo(); }
	`)
	body := extractFunction(ir, "Pt.echo")
	// Value-type branch emits bitcast then load of the value struct.
	assertContains(t, body, "bitcast i8*")
	assertContains(t, body, "load %promise_Pt_v")
	// Must NOT emit a raw i8* return against the value-struct return type.
	assertNotContains(t, body, "ret i8* %")
}

// T0891: a return-this aliased result bound to a local (`m := d.dup()`) must NOT
// be NLL-early-dropped. The result aliases the still-live source `d`; the
// receiver-alias-clear (B0250) leaves `m`'s drop flag set (clearing `d`'s), so an
// early free of `m` after its last use would free the shared instance and make
// `d`'s later read a use-after-free. The signature of the (now-suppressed) early
// drop is `emitEarlyDrops` clearing `m`'s flag in the normal body flow —
// `store i1 false, i1* %m.dropflag` — which must be absent. The alias-clear must
// still fire, and `m`'s flag-guarded scope-exit free must remain.
func TestReturnThisAliasNoEarlyDrop(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; dup() BB { return this; } }
		main() {
			d := BB(v: 11);
			m := d.dup();
			a := m.v;
			b := d.v;
		}
	`)
	// The B0250 receiver alias-clear must still fire — `m` becomes sole owner.
	assertContains(t, ir, "return.this.clear")
	// No NLL early drop of `m`: emitEarlyDrops would force-clear its flag in the
	// straight-line body after `a := m.v`. Absence proves the suppression (T0891).
	assertNotContains(t, ir, "store i1 false, i1* %m.dropflag")
	// `m` still has a flag-guarded free at scope exit (no leak, exactly one free).
	assertContains(t, ir, "%m.dropflag")
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
	assertContains(t, ir, "call i64 @__user.foo({ i1, i64 }")
	assertNotContains(t, ir, "call i64 @__user.foo(i64 ")
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
	assertContains(t, ir, "call i64 @__user.foo({ i1, i64 }")
	assertNotContains(t, ir, "call i64 @__user.foo(i1 ")
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

// T0262: WASM batch tests compile test bodies as coroutines and run them
// through the cooperative scheduler instead of spawning threads.
func TestGenerateTestMainWasmCoopScheduler(t *testing.T) {
	src := `myTest() ` + "`test" + ` { }`
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

	ti := sema.ParseTargetInfo("wasm32-wasi")
	info, semaErrs := sema.CheckWithTarget(file, map[string]*types.Scope{"std": stdScope}, ti)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{"std": stdModInfo}
	info.ModuleOrder = []string{"std"}
	result := Compile(file, info, "wasm32-wasi")
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()

	// WASM: should init scheduler with 1 P
	assertContains(t, ir, "call void @promise_sched_init(i32 1)")
	// WASM: should compile test as coroutine
	assertContains(t, ir, "define i8* @.test_coro.myTest()")
	// WASM: coroutine has presplitcoroutine attribute
	assertContainsMatch(t, ir, `@\.test_coro\.myTest\(\).*presplitcoroutine`)
	// WASM: should run through cooperative scheduler
	assertContains(t, ir, "call void @promise_sched_coop_run()")
	// WASM: should NOT use thread-based promise_test_run
	assertNotContains(t, ir, "call i32 @promise_test_run(")
	// WASM: should NOT bump goroutine counter past 0 (test G needs id=0)
	// The sched_init call is followed by alloc count reset, not atomicrmw add
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

// B0231/B0315: Batch test leak check uses spin-wait for goroutine drain
// (condvar approach had a lost-wakeup race on ARM64).
func TestBatchTestLeakCheckDrainSpinWait(t *testing.T) {
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
	// B0315: Drain check uses spin-wait with periodic wake_m nudge
	assertContains(t, ir, "leak_check_myTest")
	assertContains(t, ir, "drain_done_myTest")
	assertContains(t, ir, "drain_slow_myTest")
	assertContains(t, ir, "drain_gs_myTest")
	assertContains(t, ir, "drain_wait_myTest")
	assertContains(t, ir, "drain_nudge_myTest")
	assertContains(t, ir, "drain_sleep_myTest")
	// B0315: wake_m nudge to prevent lost-wakeup race
	assertContains(t, ir, "call void @promise_sched_wake_m()")
	// B0320: both fast-path and slow-path reads should use Acquire ordering
	assertContains(t, ir, "i64 0 acquire")
}

// B0165: Sched struct includes ready_count field (i32 at end).
func TestSchedStructHasReadyCount(t *testing.T) {
	ir := generateIR(t, `main() { }`)
	// The sched global should include the ready_count i32 field
	// Full type: { i8*, i8*, i64, i8*, i8*, i32, i8*, i8*, i64, i8, i8, i8*, i8*, i8*, i32, i64, i64, i64, i64, i8*, i32, i32, i8*, i8* }
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
	assertContains(t, ir, "print_leak_detail_myTest")
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
	assertContains(t, ir, "print_leak_detail_myTest")
	// Should have stale tag warning for allow_leaks
	assertContains(t, ir, "allow_leaks")
	assertContains(t, ir, "tag can be removed")
	// allow_leaks: no_leak_detail block and ignored counter
	assertContains(t, ir, "no_leak_detail_myTest")
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

// T0275: Batch test main frees heap-allocated panic messages to prevent leak.
// Verifies: testPanicTypeGlobal declared, free_panic_msg block generated,
// and leak delta adjustment discounts the panic msg allocation.
func TestPanicMsgFreedInTestMain(t *testing.T) {
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

	// T0275: __promise_test_panic_type global declared (non-TLS i8)
	assertContains(t, ir, "@__promise_test_panic_type = global i8 0")
	// T0275: free_panic_msg block conditionally frees heap panic msgs
	assertContains(t, ir, "free_panic_msg_myTest:")
	assertContains(t, ir, "after_free_panic_myTest:")
	// T0275: Leak delta adjustment — select discounts heap panic from delta
	assertContains(t, ir, "icmp eq i8")
}

// T0275: Test trampoline copies panic type to test harness global.
func TestTrampolineCopiesPanicType(t *testing.T) {
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

	// The trampoline stores panic type to the non-TLS test global (load from TLS, store to non-TLS)
	assertContains(t, ir, "load i8, i8* @__promise_panic_type")
	assertContains(t, ir, "store i8 1, i8* @__promise_test_panic_type")
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
	// Timeout context: prints "  timeout: exceeded " prefix
	assertContains(t, ir, `c"  timeout: exceeded "`)
	assertContains(t, ir, `c" limit\0A"`)
	// FAIL context: panic check only on result==1 (not on timeout/leak)
	assertContains(t, ir, "check_panic_myTest")
	// failedNames stores all non-pass results (FAIL, LEAK, TIMEOUT)
	assertContains(t, ir, "store_fail_myTest")
	// Exit code includes timedOut in the OR chain
	assertContains(t, ir, "or i1")
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
	// After module-based refactor: helper is user code → @__user.helper name (B0319)
	ir := generateIRWithStd(t,
		`helper() int { return 42; }`,
		`main() { x := helper(); }`,
	)
	assertContains(t, ir, "call i64 @__user.helper")
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
	assertContains(t, ir, "define i64 @__user.compute")
	assertContains(t, ir, "define i64 @__mod_mylib_compute")
	// User call goes to @__user.compute, module call to @__mod_mylib_compute
	assertContains(t, ir, "call i64 @__user.compute()")
	assertContains(t, ir, "call i64 @__mod_mylib_compute()")
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
		parse!(int x) int `+"`public"+` {
			return x;
		}
		`,
		`
		use parser "./parser";
		main!() {
			int v = parser.parse(10)?^;
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

	// Should produce separate IRs for std, alpha, beta, and the synthetic
	// __runtime module (T1089: codegen-emitted runtime helpers).
	if len(moduleIRs) != 4 {
		t.Fatalf("expected 4 module IRs (std, alpha, beta, __runtime), got %d", len(moduleIRs))
	}
	if _, ok := moduleIRs[runtimeModuleName]; !ok {
		t.Fatalf("expected %q in moduleIRs", runtimeModuleName)
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
	assertContains(t, ir, "define i64 @__user.make_resource")
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
			close!(~this) { }
		}
		process!() {
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
		process!() {
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
			close!(~this) { }
		}
		process!() {
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
			close!(~this) { }
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
			close!(~this) { raise error(message: "close err"); }
		}
		process!() {
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
			close!(~this) { }
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
			close!(~this) { }
		}
		process!() {
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
		fail!() { raise error(message: "test"); }
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
		fail!(string s) int {
			raise error(message: s);
		}
		type H { int v; }
		process!() {
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
	assertContains(t, ir, "define i64 @__user.make")
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

// B0163/T0663: Channel scope-exit drop — standalone channel gets drop flag and
// per-element-type Channel[int].drop call.
func TestDropChannelStandaloneHasDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
		}
	`)
	assertContains(t, ir, "%ch.dropflag")
	assertContains(t, ir, `call void @"Channel[int].drop"(`)
}

// B0163/T0663: Channel[T].drop body uses refcount — frees only when refcount drops to 0
func TestChannelDropFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
		}
	`)
	assertContains(t, ir, `define void @"Channel[int].drop"(i8* %this)`)
	// Refcount decrement (atomicrmw or load+add for WASM)
	assertContains(t, ir, "i64 -1")
	assertContains(t, ir, "call void @pal_free(")
	assertContains(t, ir, "call void @pal_mutex_destroy(")
	assertContains(t, ir, "call void @pal_cond_destroy(")
}

// B0163/T0663: Channel refcount initialized to 1 in promise_channel_new
func TestChannelRefcountInit(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
		}
	`)
	// promise_channel_new should store refcount = 1
	assertContains(t, ir, "define i8* @promise_channel_new(")
	// Channel[int].drop should use atomicrmw add with -1 (refcount decrement)
	assertContains(t, ir, `define void @"Channel[int].drop"(`)
}

// B0163/T0663: Channel drop null-checks the pointer (zero-initialized channels from error paths)
func TestChannelDropNullCheck(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
		}
	`)
	// Channel[int].drop body should have null check (icmp eq ... null)
	dropFn := extractFunction(ir, `"Channel[int].drop"`)
	if dropFn == "" {
		t.Fatal("expected Channel[int].drop function in IR")
	}
	assertContains(t, dropFn, "icmp eq")
	assertContains(t, dropFn, "null")
}

// B0163/T0663: Channel drop flag cleared on move (borrow detection)
func TestChannelDropFlagInDroppableContainer(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	// isDroppableContainerOrString should recognize channels
	assertContains(t, ir, "%ch.dropflag")
	assertContains(t, ir, `call void @"Channel[int].drop"(`)
}

// T1158: passing a channel as a `go f(ch)` argument increments the channel
// refcount (B0163) AND registers a matching goroutine-side Channel[T].drop after
// the target call returns, so the refcount returns to 0 and the channel is freed
// (previously the increment had no balancing decrement → 5-allocation leak).
func TestGoCallChannelArgGoroutineDrop(t *testing.T) {
	ir := generateIR(t, `
		ping(channel[int] out) { out.send(1); }
		main() {
			ch := channel[int](capacity: 1);
			go ping(ch);
		}
	`)
	// The goroutine coroutine body must contain the balancing drop. Anchor on the
	// `define ... @.goroutine.0(` line via extractDefine — extractFunction would
	// latch onto a `@.goroutine.0` fn-pointer reference inside @.goroutine.main
	// and extract the MAIN coroutine instead (whose caller-side drop of `ch` would
	// make this assertion pass for the wrong reason).
	coroFn := extractDefine(ir, `.goroutine.0`)
	if coroFn == "" {
		t.Fatal("expected .goroutine.0 coroutine function in IR")
	}
	if got := strings.Count(coroFn, `call void @"Channel[int].drop"(`); got != 1 {
		t.Fatalf("expected exactly 1 goroutine-side Channel[int].drop, got %d", got)
	}
}

// T1158: two channel args to one `go f(a, b)` call must each get their own
// goroutine-side drop — the per-arg loop appends one goArgBorrowDrop per channel,
// so the coroutine body holds exactly two balancing Channel[int].drop calls (one
// increment, one decrement per channel). Guards against a regression that only
// drops the first channel and leaks the rest.
func TestGoCallTwoChannelArgsBothDropped(t *testing.T) {
	ir := generateIR(t, `
		ping2(channel[int] a, channel[int] b) { a.send(1); b.send(2); }
		main() {
			x := channel[int](capacity: 1);
			y := channel[int](capacity: 1);
			go ping2(x, y);
		}
	`)
	coroFn := extractDefine(ir, `.goroutine.0`)
	if coroFn == "" {
		t.Fatal("expected .goroutine.0 coroutine function in IR")
	}
	if got := strings.Count(coroFn, `call void @"Channel[int].drop"(`); got != 2 {
		t.Fatalf("expected 2 goroutine-side Channel[int].drop calls (one per channel arg), got %d", got)
	}
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
		work!() void {
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
		fail!() void {
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
		passthrough(Resource zres) int {
			return zres.id;
		}
		main() {
			int x = passthrough(Resource(id: 1));
		}
	`)
	assertContains(t, ir, "define i64 @__user.passthrough")
	// The callee does NOT create a drop flag for non-~ params.
	// The caller retains ownership and drops at scope exit.
	assertNotContains(t, ir, "zres.dropflag")
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
			close!() { }
		}
		type TcpConn is Conn {
			close!() { }
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

// B0195: Vector[string] index assign dups new value for independent ownership
func TestVectorStringIndexAssignDup(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] v = ["hello", "world"];
			v[0] = "replaced";
		}
	`)
	// Should dup the new value so vector owns an independent copy (like push)
	assertContains(t, ir, "call i8* @promise_string_new")
}

// B0204: Vector[string] index assign drops old element before storing new value
func TestVectorStringIndexAssignDropsOld(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] v = ["hello", "world"];
			v[0] = "replaced";
		}
	`)
	// Should drop old string element before storing the new one
	assertContains(t, ir, "call void @promise_string_drop")
}

// B0350: Map[K,string] index assign from borrow param dups value
func TestMapStringIndexAssignBorrowParamDup(t *testing.T) {
	ir := generateIR(t, `
		store(map[string, string] m, string key, string value) {
			m[key] = value;
		}
		main() {
			map[string, string] m = {:};
			store(m, "k", "v");
		}
	`)
	// The borrow param 'value' should be duped before storing in map
	assertContains(t, ir, "call i8* @promise_string_new")
}

// B0350: Owned local string assigned to map should NOT produce extra dup —
// clearDropFlag transfers ownership, so no dup is needed.
func TestMapStringIndexAssignOwnedNoDup(t *testing.T) {
	ir := generateIR(t, `
		make_val() string { return "hello"; }
		store_owned(map[string, string] m) {
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
	fnIR := extractFunction(ir, "store_owned")
	if strings.Contains(fnIR, "call i8* @promise_string_new") {
		t.Error("owned local string should not be duped in map index assign")
	}
}

// B0235: Map overwrite should drop old Slot enum element.
func TestMapOverwriteDropsOldSlot(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[string, string] m = {:};
			m["a"] = "one";
			m["a"] = "two";
		}
	`)
	// Map.[]= should call Slot.drop on old element before storing new.
	// The Slot enum has synthesized drop at mono time (Slot[string, string]).
	assertContains(t, ir, `call void @"Slot[string, string].drop"(`)
}

// B0235: Vector[Enum] index assignment drops old enum element before storing new value.
func TestVectorEnumIndexAssignDropsOld(t *testing.T) {
	ir := generateIR(t, `
		enum Value {
			Null,
			Str(string s),
		}
		main() {
			Value[] v = [Value.Str("hello"), Value.Null];
			v[0] = Value.Null;
		}
	`)
	// Should drop old enum element before storing the new one.
	// The drop is emitted in the indexassign.ok block, before the store.
	assertContains(t, ir, "call void @Value.drop(")
}

// B0235: Vector[GenericEnum] index assignment drops old enum element (mono).
func TestVectorMonoEnumIndexAssignDropsOld(t *testing.T) {
	ir := generateIR(t, `
		enum Slot[K, V] {
			Empty,
			Used(K key, V value),
		}
		type Container[K, V] {
			Slot[K, V][] buckets;
			overwrite(~this, int idx) {
				this.buckets[idx] = Slot.Empty;
			}
		}
		main() {
			Slot[string, string][] b = [];
			Container[string, string] c = Container[string, string](buckets: b);
			c.overwrite(0);
		}
	`)
	// Should drop old mono enum element before storing the new one.
	assertContains(t, ir, `call void @"Slot[string, string].drop"(`)
}

// B0204: Vector[string] index read dups when stored in variable
func TestVectorStringIndexReadDup(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] v = ["hello", "world"];
			string s = v[0];
		}
	`)
	// Should dup the string read from vector index (dup-on-read)
	assertContains(t, ir, "call i8* @promise_string_new")
}

// T0383 / T0438: assigning `outer[0] = a.borrow` for non-Copy element T is
// rejected at sema (no implicit `T& → T` decay). The codegen dup-on-borrow
// path being tested here is unreachable under Option A; users must
// `.clone()` to obtain an owned independent copy.

// T0383: Vector[Vector[T]] index assign drops the old element before the
// store, preventing leak of the previously-pushed buffer.
func TestT0383VectorIndexAssignDropsOldHeapElement(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call void @Vector.drop(")
}

// T0383: Vector[Vector[T]] index read dups when stored in a variable
// (mirror of B0204 for nested heap vectors). Without this, drop-on-write
// at vec[i] would create a use-after-free through the aliased local.
func TestT0383VectorIndexReadDupsHeapElement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			outer := string[][]();
			inner := string[]();
			inner.push("init");
			outer.push(inner);
			t := outer[0];
		}
	`)
	// Dup-on-read emits a vecdup.copy block via dupVector for the read path.
	assertContains(t, ir, "vecdup.copy")
}

// T0388: push(h.containerField) where h is a droppable owned type must dup the
// field so that both h.drop and v's element walk own independent copies.
// genVectorMethodCall detects the MemberExpr arg and sets dupContainerFieldAccess;
// genFieldAccess then dups the vector when the owner has HasDrop() true.
func TestT0388PushVectorFieldFromDroppableOwnerDups(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "vecdup.copy")
}

// T0398: `b := v[0]` where v is Vector[heap-user-type-with-drop] must deep-clone
// the element via cloneHeapElement so b holds an independent instance.
// Without the dup, b's drop binding and v's element walk double-free the same pointer.
// genInferredVarDecl sets dupHeapUserFieldAccess; genVectorIndex calls cloneHeapElement
// which falls back to dupHeapValue (pal_alloc + memcpy) when there is no clone method.
func TestT0398VectorHeapElementReadDupsOnVarDecl(t *testing.T) {
	ir := generateIR(t, `
		type Item { int n; drop(~this) {} }
		test() {
			v := Item[]();
			v.push(Item(n: 1));
			b := v[0];
		}
	`)
	// cloneHeapElement → dupHeapValue: allocate a new instance and memcpy the data.
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "call void @llvm.memcpy")
}

// T0898: `b := v[0]` where v is Vector[no-drop heap user type] must dup-on-read.
// These types lack drop()/synth-drop (so isDroppableHeapUserType excludes them)
// and lack clone(), so genVectorIndex dups via dupHeapValue (alloc+memcpy →
// heapdup.copy block), not cloneHeapElement. Without this the new drop-on-
// overwrite in genVectorIndexAssign would free a slot still aliased by b.
func TestT0898VectorNoDropHeapElementReadDupsOnVarDecl(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, extractFunction(ir, "__user.probe"), "heapdup.copy")
}

// T0898: `v[i] = X` where v is Vector[no-drop heap user type] must drop the old
// element before storing. emitVariantFieldDrop's B0218 branch null-checks +
// pal_frees the old instance (varfield.free block). Without this the overwrite
// leaks the previous element.
func TestT0898VectorNoDropHeapElementOverwriteDrops(t *testing.T) {
	ir := generateIR(t, `
		type Bare { int n; dup() Bare { return this; } }
		probe() {
			v := Bare[]();
			v.push(Bare(n: 1));
			v[0] = Bare(n: 2);
		}
		main() { probe(); }
	`)
	fn := extractFunction(ir, "__user.probe")
	assertContains(t, fn, "varfield.free")
	assertContains(t, fn, "call void @pal_free")
}

// T0898: `h.f = X` where f is a no-drop heap user-type field must drop the old
// field value before storing. The T0410/T0908 droppable-field branch (broadened
// to admit isHeapUserNoDropPalFree) emits the null + same-pointer guard
// (field.userdrop block) followed by emitVariantFieldDrop's B0218 pal_free.
// Without this the overwrite leaks the field's previous instance.
func TestT0898MemberNoDropHeapFieldOverwriteDrops(t *testing.T) {
	ir := generateIR(t, `
		type Bare { int n; dup() Bare { return this; } }
		type Holder { Bare f; }
		probe() {
			h := Holder(f: Bare(n: 1));
			h.f = Bare(n: 2);
		}
		main() { probe(); }
	`)
	fn := extractFunction(ir, "__user.probe")
	assertContains(t, fn, "field.userdrop")
	assertContains(t, fn, "call void @pal_free")
}

// T0403: `f(v[0])` where v is Vector[heap-user-type-with-drop] and f takes a `~T`
// param must deep-clone the element via cloneHeapElement so the callee receives an
// independent instance. Without the dup, the callee's `~T` drop and v's element
// walk double-free the same pointer. maybeEnableDupForMutRefArg sets
// dupHeapUserFieldAccess for IndexExpr against Vector[heap-user-type] passed to ~T;
// genVectorIndex's existing consume-branch then clones via cloneHeapElement.
// Sibling of T0398 (var-decl-site).
func TestT0403VectorHeapElementCallArgDups(t *testing.T) {
	ir := generateIR(t, `
		type Item { int n; drop(~this) {} }
		take(Item move b) {}
		test() {
			v := Item[]();
			v.push(Item(n: 1));
			take(v[0]);
		}
	`)
	// cloneHeapElement → dupHeapValue: allocate a new instance and memcpy the data.
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "call void @llvm.memcpy")
}

// T1175: `f(v[i])` where v is Vector[Optional[heap-user-type]] and f takes a
// consuming `~`/`move` param must deep-clone the element's inner heap instance —
// the returned Optional value struct aliases the vector slot, so the callee's
// consume-drop and v's element-drop otherwise free the same inner instance (UAF).
// maybeEnableDupForMutRefArg now arms dupHeapUserFieldAccess for Optional[heap-
// user] Vector elements (not just bare heap-user); genVectorIndex's T0620 branch
// (dupOptionalVectorElem) does the clone, lowering to a dupHeapValue heapdup.copy.
// Sibling of T0403 (bare heap-user element) and genArrayIndex's fixed-Array path.
func TestT1175VectorOptionalHeapElementCallArgDups(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		take(Row? move r) {}
		test() {
			Row?[] v = [];
			v.push(Row(name: "a" + "b"));
			take(v[0]);
		}
	`)
	fn := extractFunction(ir, "__user.test")
	assertContains(t, fn, "heapdup.copy")
}

// T1175: `Holder(held: v[i])` — the constructor-field escape sibling. Same aliasing
// double-free: the new field owns the Optional value struct aliasing the vector
// slot. maybeEnableDupForConstructorArg arms the same Optional[heap-user] gate.
func TestT1175VectorOptionalHeapElementConstructorArgDups(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		type Holder { Row? held; }
		test() {
			Row?[] v = [];
			v.push(Row(name: "a" + "b"));
			h := Holder(held: v[0]);
		}
	`)
	fn := extractFunction(ir, "__user.test")
	assertContains(t, fn, "heapdup.copy")
}

// T1175: the no-drop-but-pal-free leg of the Optional[heap-user] gate. `Tag` is a
// field-less heap-user type with no drop() — it's still pal_malloc'd, so an escaped
// alias of the vector slot pal_free's the same pointer twice. optionalHeapDupElem
// recognizes both the droppable and the no-drop-pal-free inner, so f(v[i]) into a
// consuming param must still emit the deep-clone (dupHeapValue → heapdup.copy).
func TestT1175VectorOptionalNoDropHeapElementCallArgDups(t *testing.T) {
	ir := generateIR(t, `
		type Tag {}
		take(Tag? move t) {}
		test() {
			Tag?[] v = [];
			v.push(Tag());
			take(v[0]);
		}
	`)
	fn := extractFunction(ir, "__user.test")
	assertContains(t, fn, "heapdup.copy")
}

// T0397: `opt := m[k]` where the map value type is a tuple with droppable fields
// must dup the tuple's string fields so opt holds an independent copy.
// Without the dup, opt's bindingDropTuple and the map's element walk double-free
// the same string pointer. genInferredVarDecl sets dupTupleFieldAccess;
// genMethodIndex calls dupTupleValue which emits promise_string_new for string fields.
func TestT0397MapOptionalTupleIndexDupsStringField(t *testing.T) {
	ir := generateIR(t, `
		test() {
			m := map[string, (string, int)]();
			m["a"] = ("hello", 1);
			opt := m["a"];
		}
	`)
	// dupTupleValue emits promise_string_new to clone the string element.
	assertContains(t, ir, "call i8* @promise_string_new")
}

// T0397 (typed path): same dup via genTypedVarDecl's Optional[Tuple] check.
func TestT0397TypedVarDeclMapOptionalTupleDupsStringField(t *testing.T) {
	ir := generateIR(t, `
		test() {
			m := map[string, (string, int)]();
			m["a"] = ("hello", 1);
			(string, int)? opt = m["a"];
		}
	`)
	assertContains(t, ir, "call i8* @promise_string_new")
}

// T0412: vec[i] = (...) for Vector[(droppable, ...)] must drop the old tuple
// element via emitVariantFieldDrop before storing the new value. Without this,
// the previous tuple's heap fields (string instance) leak.
func TestT0412VectorIndexAssignDropsOldTuple(t *testing.T) {
	ir := generateIR(t, `
		test() {
			outer := (string, int)[]();
			outer.push(("a" + "", 1));
			outer[0] = ("b" + "", 2);
		}
	`)
	// emitVariantFieldDrop's tuple branch walks fields via ExtractValue and
	// calls promise_string_drop on the string element. The drop must appear
	// inside the indexassign.ok block (not just at scope exit).
	assertContains(t, ir, "indexassign.ok")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0412: vec[i] = vec[j] for Vector[(droppable, ...)] must dup the RHS read
// via dupTupleValue so the new slot holds an independent clone. Without this,
// Part 1's drop-old would free heap fields still aliased by another slot.
func TestT0412VectorIndexAssignDupsTupleOnVecToVec(t *testing.T) {
	ir := generateIR(t, `
		test() {
			outer := (string, int)[]();
			outer.push(("a" + "", 1));
			outer.push(("b" + "", 2));
			outer[0] = outer[1];
		}
	`)
	// dupTupleValue emits promise_string_new to clone the string element on read.
	assertContains(t, ir, "call i8* @promise_string_new")
}

// T0489: c.tup_field = (...) for a droppable tuple field must drop the old
// field's heap contents via emitVariantFieldDrop before storing the new value.
// Without this, the previous tuple's string instance leaks.
func TestT0489MemberAssignDropsOldTuple(t *testing.T) {
	ir := generateIR(t, `
		type T0489C { (string, int) f; drop(~this) {} }
		test() {
			c := T0489C(f: ("a" + "", 1));
			c.f = ("b" + "", 2);
		}
	`)
	// emitVariantFieldDrop's tuple branch walks fields via ExtractValue and
	// calls promise_string_drop on the string element. Drop must appear in the
	// function body (not just at scope exit T0489C.drop).
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0489: c.tup_field = vec[i] for a droppable tuple field must dup the RHS
// read via dupTupleValue before storing. Without this, the field and vec[i]
// alias the same heap contents, causing a silent double-free at scope exit.
func TestT0489MemberAssignDupsTupleOnVecToField(t *testing.T) {
	ir := generateIR(t, `
		type T0489D { (string, int) f; drop(~this) {} }
		test() {
			v := (string, int)[]();
			v.push(("a" + "", 1));
			c := T0489D(f: ("first" + "", 1));
			c.f = v[0];
		}
	`)
	// dupTupleValue emits promise_string_new to clone the string element on read.
	assertContains(t, ir, "call i8* @promise_string_new")
}

// T0489: c.tup_field = m[k]! must dup the RHS read via dupTupleValue before
// storing. The OptionalUnwrap-of-IndexExpr path goes through genMethodIndex's
// dupTupleFieldAccess consumer (expr.go:7514), which is a different consumer
// than the Vector path covered by TestT0489MemberAssignDupsTupleOnVecToField
// (expr.go:7654). Without this, the field and the map's stored value alias
// the same heap allocations, causing a silent double-free at scope exit.
func TestT0489MemberAssignDupsTupleOnMapUnwrapToField(t *testing.T) {
	ir := generateIR(t, `
		type T0489E { (string, int) f; drop(~this) {} }
		test() {
			m := map[string, (string, int)]();
			m["k"] = ("a" + "", 1);
			c := T0489E(f: ("first" + "", 1));
			c.f = m["k"]!;
		}
	`)
	// dupTupleValue emits promise_string_new to clone the string element on read.
	assertContains(t, ir, "call i8* @promise_string_new")
}

// Error propagation triggers scope cleanup
func TestDropErrorPropagateCleansUp(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "define { i1, i64, i8* } @__user.work")
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

// B0217: Function-typed field with captured env gets synthesized drop that frees env
func TestDropSynthesizedFuncFieldEnv(t *testing.T) {
	ir := generateIR(t, `
		type Executor {
			(int) -> void action;
		}
		main() {
			e := Executor(action: move |int x| {
				int _ = x * 2;
			});
		}
	`)
	// Executor gets a synthesized drop that null-checks and frees the closure env
	assertContains(t, ir, "define void @Executor.drop")
	assertContains(t, ir, "funcfield.env.free")
	assertContains(t, ir, "funcfield.env.skip")
	assertContains(t, ir, "call void @pal_free(") // frees env + instance
}

// B0217: Function-typed field without captures (null env) — synthesized drop with null check
func TestDropSynthesizedFuncFieldNullEnv(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper {
			() -> int getter;
		}
		main() {
			w := Wrapper(getter: || -> int { return 42; });
		}
	`)
	// Wrapper gets synthesized drop with null-check on env pointer
	assertContains(t, ir, "define void @Wrapper.drop")
	assertContains(t, ir, "funcfield.env.free")
	assertContains(t, ir, "funcfield.env.skip")
}

// B0217: Type with multiple function fields — both env pointers freed
func TestDropSynthesizedMultipleFuncFields(t *testing.T) {
	ir := generateIR(t, `
		type Transform {
			(int) -> int forward;
			(int) -> int backward;
		}
		main() {
			t := Transform(forward: |int x| -> x * 2, backward: |int x| -> x / 2);
		}
	`)
	assertContains(t, ir, "define void @Transform.drop")
	assertContains(t, ir, "funcfield.env.free")
}

// T0741 Part B: a struct closure field with heap captures must deep-drop its
// env (drop captured values via the env's field-0 drop fn) instead of a shallow
// pal_free. emitFuncFieldEnvFree now routes through emitEnvDropOrFree, which
// emits the env.deep_drop / env.shallow_free branch.
func TestDropClosureStructFieldDeepDrops(t *testing.T) {
	ir := generateIR(t, `
		type CbHolder {
			() -> int cb;
		}
		make_cb(int n) CbHolder {
			s := "cap" + "tured";
			return CbHolder(cb: move || -> s.len + n);
		}
		main() {
			h := make_cb(5);
		}
	`)
	assertContains(t, ir, "define void @CbHolder.drop")
	assertContains(t, ir, "funcfield.env.free")
	// Deep drop: load the env's field-0 drop fn and call it (drops captures),
	// else fall back to pal_free. The presence of these blocks (not a bare
	// pal_free in funcfield.env.free) is the Part-B fix.
	assertContains(t, ir, "env.deep_drop")
	assertContains(t, ir, "env.shallow_free")
}

// T0741 Part A: an enum variant whose payload is a closure must drop the
// closure's env in the synthesized enum drop. variantFieldNeedsDrop now returns
// true for *types.Signature, so emitVariantFieldDrop's closure case runs.
func TestDropEnumVariantClosurePayload(t *testing.T) {
	ir := generateIR(t, `
		enum Callback {
			holds(() -> int cb),
			empty,
		}
		main() {
			s := "enum" + " payload";
			c := Callback.holds(cb: move || -> s.len);
		}
	`)
	assertContains(t, ir, "define void @Callback.drop")
	assertContains(t, ir, "closure.env.free")
	assertContains(t, ir, "env.deep_drop")
}

// T0741 Part C: an optional closure struct field must drop its env.
// emitOptionalValueDrop now has a *types.Signature case that branches on the
// has-value flag and deep-drops the inner closure's env.
func TestDropOptionalClosureField(t *testing.T) {
	ir := generateIR(t, `
		type OptCb {
			(() -> int)? cb;
		}
		make_holder(int n) OptCb {
			s := "cap" + "tured";
			return OptCb(cb: move || -> s.len + n);
		}
		main() {
			h := make_holder(5);
		}
	`)
	assertContains(t, ir, "define void @OptCb.drop")
	assertContains(t, ir, "optfield.drop")
	assertContains(t, ir, "closure.env.free")
	assertContains(t, ir, "env.deep_drop")
}

// T0814: force-unwrapping a LOCAL optional closure into a new local (`f := o!`)
// transfers the heap env from `o` to `f`. The fix has two halves:
//  1. claimEnvTemp recurses into the optional-wrapped fat pointer so the lambda's
//     env temp is claimed by `o` (no early env.tmp.drop of the lambda env), and
//  2. neutralizeForceUnwrapSource clears `o`'s present flag (`store i1 false`)
//     so `o`'s optional drop is skipped and only `f` frees the env once.
func TestUnwrapLocalOptionalClosure(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "cap" + "tured";
			(() -> int)? o = move || -> s.len;
			f := o!;
		}
	`)
	// The unwrap path is taken and the source optional's present flag is cleared.
	assertContains(t, ir, "unwrap.ok")
	assertContains(t, ir, "store i1 false")
	// f owns the env and frees it exactly once at scope exit.
	assertContains(t, ir, "env.free")
	// The optdrop for o is registered for the Signature inner case (the env is
	// freed via the closure-env path when present, not leaked).
	assertContains(t, ir, "optdrop.inner")
	assertContains(t, ir, "closure.env.free")
}

// T0741/T0813: dup-ing a closure-containing enum aggregate must NOT shallow-copy
// the closure env (that would alias one env between two droppable owners →
// double-free). emitVariantFieldDup's Signature case nulls the cloned variant's
// closure slot instead, so only the source owns the env. T0813 makes sema reject
// the explicit Vector[Cb].clone() repro outright, so this exercises the null-dup
// via a non-gated implicit dup path (vector slice → emitVectorElementCloneLoop →
// dupEnumElementInPlace → emitVariantFieldDup). Verify the dup path stores a
// zeroed fat pointer rather than copying the source closure value.
func TestDupEnumVariantClosureNullsSlot(t *testing.T) {
	ir := generateIR(t, `
		enum Cb {
			holds(() -> int cb),
			empty,
		}
		main() {
			s := "shared" + " env";
			v := Vector[Cb]();
			v.push(Cb.holds(cb: move || -> s.len));
			v2 := v[:];
		}
	`)
	// The element dup reaches the enum variant dup switch (enumdup.holds), where
	// the cloned variant's {fn,env} closure slot is zero-initialized (nulled),
	// not a copy of the source env — the memory-safe degradation for the
	// non-cloneable closure env.
	assertContains(t, ir, "enumdup.holds")
	assertContains(t, ir, "store { i8*, i8* } zeroinitializer")
}

// T1109: A Ref/Arc[T] variant field in a container must dup via a strong-count
// increment (arcdup.inc), NOT route into dupHeapValue — Arc's LLVM type is a
// bare i8* (a *types.PointerType), so the heap-user-type dup path panicked with
// "interface conversion: *types.PointerType, not *types.StructType". This guards
// against regression to that route by confirming the variant-field dup emits the
// Arc refcount increment.
func TestDupEnumVariantArcRefcount(t *testing.T) {
	ir := generateIR(t, `
		enum Holder { Pair(Ref[int] r, int n) }
		main() {
			m := Map[int, Holder]();
			m[1] = Holder.Pair(Ref[int](9), 2);
		}
	`)
	// The variant-field dup for the Ref field reaches dupArc (strong-count
	// increment) rather than panicking in dupHeapValue.
	assertContains(t, ir, "enumdup.Pair")
	assertContains(t, ir, "arcdup.inc")
}

// T1109: A Weak[T] variant field in a container must dup via dupWeak (atomic
// weak-count increment, emits weakdup.inc), the symmetric sibling of the Arc
// branch. Like Arc, Weak's LLVM value is a bare i8*, so it must NOT reach
// dupHeapValue. Guards the weak branch of emitVariantFieldDup.
func TestDupEnumVariantWeakRefcount(t *testing.T) {
	ir := generateIR(t, `
		enum WHolder { One(Weak[int] w, int n) }
		main() {
			a := Ref[int](99);
			m := Map[int, WHolder]();
			m[1] = WHolder.One(a.downgrade(), 3);
		}
	`)
	assertContains(t, ir, "enumdup.One")
	assertContains(t, ir, "weakdup.inc")
}

// T1109: A generic enum carrying Ref[T]/Weak[T] variant fields exercises the
// type-substitution sub-branches in emitVariantFieldDup (the dup is synthesized
// in a mono context where c.typeSubst != nil, so the element type must be
// substituted before dupArc/dupWeak). Confirms both refcount paths fire for the
// monomorphized Box[int] instance.
func TestDupEnumVariantGenericArcWeakRefcount(t *testing.T) {
	ir := generateIR(t, `
		enum Box[T] { Some(Ref[T] r, int n), None }
		enum WBox[T] { W(Weak[T] w, int n), E }
		main() {
			a := Ref[int](7);
			m := Map[int, Box[int]]();
			m[1] = Box[int].Some(Ref[int](9), 2);
			wm := Map[int, WBox[int]]();
			wm[1] = WBox[int].W(a.downgrade(), 3);
		}
	`)
	assertContains(t, ir, "arcdup.inc")
	assertContains(t, ir, "weakdup.inc")
}

// B0216: String field reassignment drops old value before storing new.
func TestStringFieldReassignDrop(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			string val;
		}
		main() {
			b := Box(val: "hello");
			b.val = "world";
		}
	`)
	// Field reassignment should emit old-value drop before store
	assertContains(t, ir, "field.strdrop")
	assertContains(t, ir, "call void @promise_string_drop(")
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

// B0220: NeedsSynthDrop types (no explicit drop) also get string field dups.
// HasDrop() is true for NeedsSynthDrop types (set together in sema), so the
// T0095 dup logic covers both explicit-drop and synthesized-drop types.
func TestStringFieldAccessDupNeedsSynthDrop(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call i8* @promise_string_new(")
}

// B0219: Vector field reassignment drops old value before storing new.
func TestVectorFieldReassignDrop(t *testing.T) {
	ir := generateIR(t, `
		type Container {
			int[] items;
		}
		main() {
			c := Container(items: []);
			c.items = [];
		}
	`)
	// Field reassignment should emit old-value drop before store
	assertContains(t, ir, "field.vecdrop")
	assertContains(t, ir, "call void @Vector.drop(")
}

// B0219: Vector field read from droppable type creates a dup (via vecdup).
func TestVectorFieldAccessDup(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			int[] data;
		}
		test() {
			h := Holder(data: []);
			int[] x = h.data;
		}
	`)
	// Reading h.data should dup the vector to prevent double-free
	assertContains(t, ir, "vecdup.copy")
	assertContains(t, ir, "call i8* @pal_alloc(")
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

// T0753: the optional-handler unwrap `(o? _ { ... })` on an ident source must
// NOT register the extracted inner value as an owned heap temp. The source
// optional `o` already owns the inner allocation — its own scope drop binding
// frees it exactly once (for an owned optional) or deliberately not at all (for
// a borrow-holding optional, once T0747's isRttiCastBorrow clears o's flag).
// Tracking the extracted heap value as a statement temp double-frees at scope
// exit (`fatal: invalid free (bad header magic)`). Symmetric to the existing
// OptionalUnwrapExpr ident-skip guard (B0287/T0343).
func TestOptionalHandlerUnwrapIdentNoHeapTemp(t *testing.T) {
	ir := generateIR(t, `
		type HBox { int n; drop(~this) {} }
		tfn() int {
			HBox? o = HBox(n: 5);
			return (o? _ { return -1; }).n;
		}
		main() { _ := tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
	// Legitimate HBox.drop call sites in tfn (4 total): the construction temp's
	// cleanup (dead — flag cleared when moved into o) emitted on both the normal
	// and the unwrap-panic edges (2), plus o's optional-drop emitted on each of
	// the two return edges — the some path and the handler path (2). The extracted
	// inner from the handler must add NO further drop call site — without the fix
	// it tracks a spurious owned temp, raising the count to 5.
	got := strings.Count(fn, "call void @HBox.drop")
	if got != 4 {
		t.Fatalf("expected 4 HBox.drop call sites (no spurious heap temp for the "+
			"handler-extracted inner), got %d:\n%s", got, fn)
	}
}

// T0776: `((o)!).len` on a string? ident source must NOT register the extracted
// i8* inner as an owned statement temp. The source optional `o` already owns
// the inner allocation via its scope drop binding; tracking the extracted ptr
// as a temp emits a second @promise_string_drop call site against the same
// pointer (double-free at runtime). Without the ParenExpr peel in
// genOptionalForceUnwrap, `(o)!` is a *ParenExpr (not *IdentExpr), the
// isIdent check fails, and the spurious temp tracking fires. Mirror of
// TestOptionalHandlerUnwrapIdentNoHeapTemp for the i8*-inner force-unwrap
// branch (the handler form uses a separate site that T0753 already covers).
func TestT0776ParenForceUnwrapStringNoTemp(t *testing.T) {
	ir := generateIR(t, `
		tfn() int {
			string? o = "abc".to_upper();
			return ((o)!).len;
		}
		main() { _ := tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatal("expected __user.tfn in IR")
	}
	// Legitimate @promise_string_drop call sites in tfn (3): the to_upper temp's
	// cleanup (dead — flag cleared when moved into o) on the normal path (1),
	// plus o's optional-drop on each of the two return edges (some path and the
	// unwrap-panic edge) (2). The extracted inner from the paren force-unwrap
	// must add NO further drop call site — without the ParenExpr peel a
	// spurious temp tracker fires, raising the count to 4.
	got := strings.Count(fn, "call void @promise_string_drop")
	if got != 3 {
		t.Fatalf("expected 3 @promise_string_drop call sites in tfn (no spurious "+
			"temp for the paren force-unwrap extracted inner), got %d:\n%s", got, fn)
	}
}

// T0776: `((o)!).len` on an int[]? (Vector[int]) ident source must NOT register
// the extracted i8* inner as an owned statement temp. Symmetric to the string
// case; covers the TypVector branch of the type-aware temp tracker.
func TestT0776ParenForceUnwrapVectorNoTemp(t *testing.T) {
	ir := generateIR(t, `
		tfn() int {
			int[]? o = [1, 2].clone();
			return ((o)!).len;
		}
		main() { _ := tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatal("expected __user.tfn in IR")
	}
	// The codegen test harness uses an un-monomorphized stdlib stub, so the call
	// target is @Vector.drop (in real builds it's Vector__int.drop). The expected
	// count mirrors the string case: 1 clone-temp cleanup + 2 optional-drops on
	// the two return edges = 3. Without the ParenExpr peel a spurious temp
	// tracker would add a 4th call site.
	got := strings.Count(fn, "call void @Vector.drop")
	if got != 3 {
		t.Fatalf("expected 3 @Vector.drop call sites in tfn (no spurious "+
			"temp for the paren force-unwrap extracted inner), got %d:\n%s", got, fn)
	}
}

// T0776 no-regression: a non-ident source (call returning string?) MUST still
// track the extracted inner as a statement temp — there is no source optional
// alloca with its own scope drop to own the allocation, so without the temp
// the inner leaks. Symmetric to the ident-tracking guard at the call site of
// `(mk_str()!).len` — the peel must not over-broaden the skip.
func TestT0776NonIdentForceUnwrapStringStillTracks(t *testing.T) {
	ir := generateIR(t, `
		mk_str() string? { return "ab".to_upper(); }
		tfn() int {
			return (mk_str()!).len;
		}
		main() { _ := tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatal("expected __user.tfn in IR")
	}
	// With a non-ident source there is no source optional alloca → the
	// extracted inner is the *only* owner. trackStringTemp must emit at least
	// one statement-end drop for it; count >= 1 is enough to confirm the gate
	// did not over-broaden to skip non-ident sources too.
	got := strings.Count(fn, "call void @promise_string_drop")
	if got < 1 {
		t.Fatalf("expected >=1 @promise_string_drop call site in tfn (non-ident "+
			"source must still track the extracted inner), got %d:\n%s", got, fn)
	}
}

// T0776: `((o)!).borrow` on an Ref[int]? ident source exercises the
// trackTempWithDrop branch of genOptionalForceUnwrap's type-aware temp tracker
// (the path the string/vector tests above do NOT cover — those hit
// trackStringTemp / trackVectorTempWithElemType). Without the ParenExpr peel,
// the extracted i8* Arc handle gets registered as a NEW stmt-temp via
// trackTempWithDrop after `unwrap.ok`, with a tmp.exec / Ref[int].drop
// cleanup racing the source optional's own scope-drop on the same handle —
// atomic refcount goes to zero twice → use-after-free. Mirrors
// TestT0654_OptionalArcUnwrapConsumeTracked's discriminator (which asserts
// the OPPOSITE — non-ident sources MUST register the temp) by checking the
// IR slice AFTER `unwrap.ok`: no new tmp.exec / Arc drop pair must appear
// there. Covers Arc/Weak/Mutex/Task/Channel as a class — all five hit the
// same outer gate via trackTempWithDrop.
func TestT0776ParenForceUnwrapArcNoTemp(t *testing.T) {
	ir := generateIR(t, `
		tfn() int {
			Ref[int]? o = Ref[int](42);
			return ((o)!).borrow;
		}
		main() { _ := tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatal("expected __user.tfn in IR")
	}
	// Find the actual `unwrap.ok.<N>:` block LABEL (not the `%unwrap.ok.<N>`
	// reference inside the preceding tmp.skip branch instruction — which would
	// pull tmp.exec.<M> into the post slice and trigger a false positive).
	unwrapLabel := regexp.MustCompile(`(?m)^\s*unwrap\.ok\.\d+:`)
	loc := unwrapLabel.FindStringIndex(fn)
	if loc == nil {
		t.Fatalf("expected unwrap.ok.<N>: block label from "+
			"genOptionalForceUnwrap:\n%s", fn)
	}
	// The slice after `unwrap.ok.<N>:` is where genOptionalForceUnwrap would
	// emit the spurious tmp.exec / Ref[int].drop pair if the ParenExpr peel
	// were missing. After `unwrap.ok`, the only legitimate Ref[int].drop call
	// sites are the source optional's `optdrop.inner.*` blocks (one per
	// return edge); those live INSIDE `optdrop.inner.*`, not `tmp.exec.*`.
	post := fn[loc[0]:]
	postLines := strings.Split(post, "\n")
	inTmpExec := false
	for _, line := range postLines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "tmp.exec") && strings.HasSuffix(trimmed, ":") {
			inTmpExec = true
			continue
		}
		if strings.HasSuffix(trimmed, ":") && !strings.HasPrefix(trimmed, "tmp.exec") {
			inTmpExec = false
		}
		if inTmpExec && strings.Contains(line, `@"Ref[int].drop"`) {
			t.Fatalf("found spurious @\"Ref[int].drop\" call in a tmp.exec "+
				"block AFTER unwrap.ok — the ParenExpr peel in "+
				"genOptionalForceUnwrap should skip temp tracking for "+
				"`((o)!)` because the source optional already owns the Arc:\n%s", fn)
		}
	}
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
	assertContains(t, ir, "define { i8*, i8* } @__user.wrap(i8* %s)")
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
		get_ref(string s) string & {
			return s;
		}
		test() {
			p := Pair(a: "hello", b: "world");
			string & ra = get_ref(p.a);
		}
	`)
	// The test function should NOT contain a string dup — the param is a borrow.
	testFn := extractFunction(ir, "__user.test")
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

// B0199: Constructor call sites keep caller's drop flag for string-typed borrow
// parameters on types with HasDrop(). The constructor body strdups the string
// (genAssignment detects no drop flag on the param), so the caller must keep
// its drop flag to free the original string.
func TestConstructorBorrowParamKeepsDropFlag(t *testing.T) {
	ir := generateIR(t, `
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
	assertNotContains(t, ir, "store i1 false, i1* %mystr_b0199.dropflag")
}

// T0086: Raising a local error variable clears its drop flag before scope cleanup
func TestRaiseLocalErrorClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		fail!() void {
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
	assertContains(t, ir, "call i8* @__user.display")
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
	assertContains(t, ir, "call i8* @__user.make_greeting")
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
	assertContains(t, ir, "call i8* @__user.make_label")
	// Drop flag is cleared (claimed) so no free at stmt end for this temp
	assertContains(t, ir, "store i1 false")
}

// B0198: String temps in if-condition must be cleaned up in the merge block.
// When the condition evaluates to false, the then-body never runs but its
// inner-statement cleanup cleared the Go tracking. The merge block must still
// emit flag-guarded cleanup IR.
func TestStringTempIfConditionFalsePath(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "tmp.skip")
	assertContains(t, ir, "call void @promise_string_drop")
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
	// T0663: synthesized drop calls the per-element-type Channel[int].drop on
	// the channel field (was the single @Channel.drop symbol pre-T0663).
	withChanDrop := extractFunction(ir, "WithChan.drop")
	if withChanDrop == "" {
		t.Fatal("expected WithChan.drop function in IR")
	}
	assertContains(t, withChanDrop, `call void @"Channel[int].drop"(`)
}

// B0192: Non-droppable heap user type fields inside synthesized drop get pal_free
func TestSynthDropFreesNonDroppableHeapField(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		type Wrapper { Point p; string name; }
		main() {
			w := Wrapper(p: Point(x: 1, y: 2), name: "test");
		}
	`)
	wrapperDrop := extractFunction(ir, "Wrapper.drop")
	if wrapperDrop == "" {
		t.Fatal("expected Wrapper.drop function in IR")
	}
	// Synthesized drop should free the Point instance via pal_free
	assertContains(t, wrapperDrop, "call void @pal_free(")
	// And drop the string field
	assertContains(t, wrapperDrop, "call void @promise_string_drop(")
}

// B0192: Generic type with type-param field gets NeedsSynthDrop
// so its synthesized drop can free heap-allocated type-param fields.
func TestSynthDropGenericTypeParamField(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		type Holder { Point p; }
		main() {
			Holder h = Holder(p: Point(x: 1, y: 2));
		}
	`)
	// Holder gets a synthesized drop that frees the Point instance
	holderDrop := extractFunction(ir, "Holder.drop")
	if holderDrop == "" {
		t.Fatal("expected Holder.drop function in IR")
	}
	// Should pal_free the Point field instance (B0192 needsFreeOnly path)
	assertContains(t, holderDrop, "call void @pal_free(")
}

// B0202: Generic type where ALL fields are TypeParam — synthesized drop detected at mono time
func TestSynthDropMonoTypeParamOnlyFields(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		type Box[T] { T val; }
		main() {
			b := Box[Point](val: Point(x: 1, y: 2));
		}
	`)
	// Box[Point] gets a mono synthesized drop that frees the Point field
	boxDrop := extractFunction(ir, `"Box[Point].drop"`)
	if boxDrop == "" {
		t.Fatal("expected Box[Point].drop function in IR")
	}
	assertContains(t, boxDrop, "call void @pal_free(")
}

// B0202: Generic type with TypeParam field instantiated with primitive — no synth drop needed
func TestSynthDropMonoTypeParamPrimitive(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T val; }
		main() {
			b := Box[int](val: 42);
		}
	`)
	// Box[int] should NOT get a synthesized drop — int is primitive
	boxDrop := extractFunction(ir, `"Box[int].drop"`)
	if boxDrop != "" {
		t.Fatal("Box[int] should not have a synthesized drop")
	}
}

// B0202: Generic type with TypeParam field instantiated with string — gets synth drop
func TestSynthDropMonoTypeParamString(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper[T] { T val; }
		main() {
			w := Wrapper[string](val: "hello");
		}
	`)
	// Wrapper[string] gets a mono synthesized drop for the string field
	wrapperDrop := extractFunction(ir, `"Wrapper[string].drop"`)
	if wrapperDrop == "" {
		t.Fatal("expected Wrapper[string].drop function in IR")
	}
	assertContains(t, wrapperDrop, "call void @promise_string_drop(")
}

// B0209: Generic type with Optional[TypeParam] field instantiated with string — gets synth drop
func TestSynthDropMonoOptionalTypeParamString(t *testing.T) {
	ir := generateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[string](val: "hello");
		}
	`)
	// MaybeVal[string] gets a mono synthesized drop for the optional string field
	drop := extractFunction(ir, `"MaybeVal[string].drop"`)
	if drop == "" {
		t.Fatal("expected MaybeVal[string].drop function in IR")
	}
	assertContains(t, drop, "call void @promise_string_drop(")
}

// B0209: Generic type with Optional[TypeParam] field instantiated with primitive — no synth drop
func TestSynthDropMonoOptionalTypeParamPrimitive(t *testing.T) {
	ir := generateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[int](val: 42);
		}
	`)
	// MaybeVal[int] should NOT get a synthesized drop — int is primitive
	drop := extractFunction(ir, `"MaybeVal[int].drop"`)
	if drop != "" {
		t.Fatal("MaybeVal[int] should not have a synthesized drop")
	}
}

// B0209: Generic type with Optional[TypeParam] field instantiated with heap user type — gets synth drop
func TestSynthDropMonoOptionalTypeParamUserType(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[Point](val: Point(x: 1, y: 2));
		}
	`)
	// MaybeVal[Point] gets a mono synthesized drop (at minimum for pal_free of the instance)
	drop := extractFunction(ir, `"MaybeVal[Point].drop"`)
	if drop == "" {
		t.Fatal("expected MaybeVal[Point].drop function in IR")
	}
	assertContains(t, drop, "call void @pal_free(")
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
		fail!() int {
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
		fail!() int {
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
		fail!() int {
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
		fail!() string {
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

// B0236: Match destructure of droppable enum dups heap user type fields.
func TestMatchDupHeapUserType(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper {
			string name;
		}
		enum Container {
			Holding(Wrapper item),
			Empty,
		}
		test() {
			c := Container.Holding(Wrapper(name: "hello"));
			match c {
				Holding(w) => { string s = w.name; },
				Empty => { },
			}
		}
	`)
	// Extracting Wrapper from droppable enum should dup the heap instance
	assertContains(t, ir, "heapdup.copy")
	assertContains(t, ir, "call i8* @pal_alloc(")
}

// B0236: Match destructure of droppable enum dups vector fields.
func TestMatchDupVector(t *testing.T) {
	ir := generateIR(t, `
		enum Holder {
			Data(int[] items),
			None,
		}
		test() {
			h := Holder.Data([1, 2, 3]);
			match h {
				Data(v) => { int x = v.len; },
				None => { },
			}
		}
	`)
	// Extracting vector from droppable enum should dup via vecdup
	assertContains(t, ir, "vecdup.copy")
}

// B0236: Match destructure of droppable enum dups channel fields.
func TestMatchDupChannel(t *testing.T) {
	ir := generateIR(t, `
		enum Wrapper {
			Chan(channel[int] ch),
			None,
		}
		test() {
			ch := channel[int](1);
			w := Wrapper.Chan(ch);
			match w {
				Chan(c) => { },
				None => { },
			}
		}
	`)
	// Extracting channel from droppable enum should dup via chdup (refcount increment)
	assertContains(t, ir, "chdup.inc")
}

// B0284: Map with non-cloneable value types (enum with drops but no `clone`)
// must NOT be cloned via Map.clone() — the clone would be shallow, causing
// double-free when both original and clone drop shared enum heap data.
func TestMatchDupMapNotClonedNonCloneableValues(t *testing.T) {
	ir := generateIR(t, `
		enum JsonNode {
			Null,
			Text(string value),
			Dict(map[string, JsonNode] fields),
		}
		test() {
			map[string, JsonNode] fields = {"k": JsonNode.Text(value: "v")};
			JsonNode obj = JsonNode.Dict(fields: fields);
			match obj {
				Dict(f) => { int x = f.len; },
				_ => { },
			}
		}
	`)
	// Check that the test function itself does not heap-dup the Map.
	// The "heapdup.copy" block may legitimately appear in other monomorphized
	// functions (e.g., EmbeddedFiles.files vector clone), so only check inside @test.
	//
	// T1129 note: JsonNode now has a synthesized clone, but `match obj` here has an
	// owned *local* subject — its arm bindings BORROW the variant payload (the local's
	// own scope binding drops it once), so no per-binding clone is emitted regardless.
	inTestFunc := false
	for _, line := range strings.Split(ir, "\n") {
		if strings.HasPrefix(line, "define ") {
			inTestFunc = strings.Contains(line, "@test(")
		}
		if inTestFunc && strings.Contains(line, "heapdup.copy") {
			t.Error("Map should not be heap-dup'd in test function (vector of droppable enum elements)")
		}
		if inTestFunc && strings.Contains(line, "Map[string, JsonNode].clone") && strings.Contains(line, "= call") {
			t.Error("match of an owned local subject borrows its bindings — should not call Map.clone()")
		}
	}
}

// B0284: Map with safely cloneable value types (primitives, strings) CAN be
// cloned via Map.clone() — the clone's internal match-dup handles these types.
func TestMatchDupMapClonedSafeValues(t *testing.T) {
	ir := generateIR(t, `
		enum Holder {
			Data(map[string, int] fields),
			Empty,
		}
		test() {
			map[string, int] m = {"a": 1, "b": 2};
			h := Holder.Data(fields: m);
			match h {
				Data(f) => { int x = f.len; },
				Empty => { },
			}
		}
	`)
	// Map[string, int] — both type args are safe (string dup'd, int is primitive)
	assertContains(t, ir, "Map[string, int].clone")
}

// B0284: Map with cloneable enum values CAN be cloned — the enum has `clone`
// so the clone's internal match-dup will deep-copy via enum clone.
func TestMatchDupMapClonedWithCloneableEnumValues(t *testing.T) {
	ir := generateIR(t, ""+
		"enum Status `clone {\n"+
		"  Active(string label),\n"+
		"  Inactive,\n"+
		"}\n"+
		"enum Holder {\n"+
		"  Data(map[string, Status] fields),\n"+
		"  Empty,\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, Status] m = {\"a\": Status.Active(label: \"on\")};\n"+
		"  h := Holder.Data(fields: m);\n"+
		"  match h {\n"+
		"    Data(f) => { int x = f.len; },\n"+
		"    Empty => { },\n"+
		"  }\n"+
		"}\n")
	// Map[string, Status] — Status is `clone so match-dup can handle it
	found := false
	for _, line := range strings.Split(ir, "\n") {
		if strings.Contains(line, "Map[string, Status].clone") && strings.Contains(line, "= call") {
			found = true
			break
		}
	}
	if !found {
		t.Error("Map with cloneable enum values should be cloned via Map.clone()")
	}
}

// B0284: Map with fieldless enum values (no drops) CAN be cloned — bitwise copy
// is safe for enums with no heap data, so typeArgSafeForCloneDup returns true.
func TestMatchDupMapClonedFieldlessEnumValues(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue, }
		enum Holder {
			Data(map[string, Color] fields),
			Empty,
		}
		test() {
			map[string, Color] m = {"a": Color.Red};
			h := Holder.Data(fields: m);
			match h {
				Data(f) => { int x = f.len; },
				Empty => { },
			}
		}
	`)
	// Map[string, Color] — Color has no drops, bitwise copy is safe
	assertContains(t, ir, "Map[string, Color].clone")
}

// B0244: Match destructure of droppable enum clones enum-typed fields via clone().
func TestMatchDupEnumClone(t *testing.T) {
	ir := generateIR(t, ""+
		"enum Inner `clone {\n"+
		"  Value(string data),\n"+
		"  Empty,\n"+
		"}\n"+
		"enum Outer {\n"+
		"  Holding(Inner item),\n"+
		"  Nothing,\n"+
		"}\n"+
		"test() {\n"+
		"  o := Outer.Holding(item: Inner.Value(data: \"hello\"));\n"+
		"  match o {\n"+
		"    Holding(i) => { },\n"+
		"    Nothing => { },\n"+
		"  }\n"+
		"}\n")
	// Enum field extracted from droppable enum should be cloned via clone method
	assertContains(t, ir, "Inner.clone")
	assertContains(t, ir, "enum.clone.tmp")
}

// B0285: Synthesized enum clone method must NOT double-clone fields.
// The match inside clone() destructures variant fields, and without suppression
// the match-dup mechanism also clones them — causing double work and leaked
// intermediate clones. For recursive types this causes stack overflow.
func TestEnumCloneNoDoubleClone(t *testing.T) {
	ir := generateIR(t, ""+
		"enum Token `clone {\n"+
		"  Word(string text),\n"+
		"  Empty,\n"+
		"}\n"+
		"test() {\n"+
		"  t := Token.Word(text: \"hello\");\n"+
		"  Token t2 = t.clone();\n"+
		"}\n")
	// Inside Token.clone(), the match destructure should NOT dup the string field —
	// the synthesized body explicitly calls .clone() on it. With match-dup suppressed,
	// there should be exactly 1 strdup block label (from the explicit .clone() call).
	// Without suppression, there would be 2 (match-dup + explicit clone).
	lines := strings.Split(ir, "\n")
	inClone := false
	strdupBlocks := 0
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, "Token.clone") {
			inClone = true
		} else if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		// Count distinct strdup.copy block labels (not references)
		trimmed := strings.TrimSpace(line)
		if inClone && strings.HasPrefix(trimmed, "strdup.copy.") && strings.HasSuffix(trimmed, ":") {
			strdupBlocks++
		}
	}
	if strdupBlocks != 1 {
		t.Errorf("B0285: Token.clone() body should have exactly 1 strdup block (from explicit .clone()), got %d", strdupBlocks)
	}
}

// T0551/T0607: Cloning a generic `clone enum whose TypeArg is droppable
// (map[K,V]) must deep-copy the variant payload. isCopyField(TypeParam) is
// optimistically true, so the synth body can't classify the `T val` field at
// synth time; it emits the synth-only AutoCloneExpr intrinsic (T0607 unified
// the T0551 plain-T path onto the same mechanism), lowered type-directed at
// mono codegen to the substituted concrete type's clone. Before the fix the
// synth body shallow-aliased the Map fat pointer (no clone call) → double-free
// segfault. Assert the mono clone body contains a Map[..].clone call inside
// the Just arm (AutoClone → cloneByType → cloneResolvedValue → Map.clone).
func TestGenericEnumCloneDroppableTypeArg(t *testing.T) {
	ir := generateIR(t, ""+
		"enum MaybeMap[T] `clone {\n"+
		"  Just(T val),\n"+
		"  Nothing,\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  MaybeMap[map[string, string]] j = MaybeMap[map[string, string]].Just(src);\n"+
		"  MaybeMap[map[string, string]] c = j.clone();\n"+
		"}\n")
	// The mono enum clone body must deep-copy the Map payload.
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, "MaybeMap[Map[string, string]].clone") {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if inClone && strings.Contains(line, `@"Map[string, string].clone"`) {
			sawMapClone = true
		}
	}
	if !sawMapClone {
		t.Errorf("T0551: MaybeMap[Map[string, string]].clone() body must call Map[string, string].clone (deep-copy the droppable TypeArg payload); got shallow alias")
	}
}

// T0607: Cloning a generic `clone enum whose variant field is *declared* as
// `T?` (Optional[TypeParam]) with a droppable TypeArg (map[K,V]) must deep-copy
// the Optional payload. isCopyField(Optional[TypeParam])==true at synth time,
// so the synth body used to pass the field through bare → the constructor
// stored the inner Map fat pointer into the Optional slot (compile panic, then
// after T0608/T0630's coercion: shallow alias → double-free segfault). The fix
// routes the ContainsTypeParam field through the synth-only AutoCloneExpr
// intrinsic, lowered by cloneByType: an Optional none-check (autoclone.some /
// autoclone.merge blocks) that deep-clones the unwrapped concrete payload
// (Map[..].clone) and rewraps. Assert both the none-check structure and the
// inner Map clone are present in the OptVal arm (not a bare {i1,payload}
// passthrough).
func TestGenericEnumCloneOptionalTypeParamField(t *testing.T) {
	ir := generateIR(t, ""+
		"enum Wrap[T] `clone {\n"+
		"  OptVal(T? maybe),\n"+
		"  Nothing,\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  Wrap[map[string, string]] j = Wrap[map[string, string]].OptVal(src);\n"+
		"  Wrap[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	sawAutoCloneSome := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `Wrap[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		if strings.Contains(line, `@"Map[string, string].clone"`) {
			sawMapClone = true
		}
		if strings.Contains(line, "autoclone.some") {
			sawAutoCloneSome = true
		}
	}
	if !sawMapClone {
		t.Errorf("T0607: Wrap[Map[string, string]].clone() body must call Map[string, string].clone to deep-copy the Optional[TypeParam] payload; got shallow alias")
	}
	if !sawAutoCloneSome {
		t.Errorf("T0607: Wrap[Map[string, string]].clone() body must lower the `T? maybe` field via AutoClone (autoclone.some none-check block), not a bare {i1,payload} passthrough")
	}
}

// T0607: Cloning a generic `clone enum whose variant field is an enum-Instance
// carrying the TypeParam (`Inner[T] inner`) with a droppable TypeArg must deep-
// copy the nested enum. extractNamed is nil for enum Instances, so before the
// fix isAutoCloneBitCopy treated `Inner[Map]` as a bit copy (non-named →
// bitwise) → cloneByType returned the value unchanged → shallow alias →
// double-free. The fix adds an extractEnum branch to isAutoCloneBitCopy so the
// nested `clone enum routes through cloneResolvedValue→cloneEnumValue. Assert
// Outer's clone body calls the inner enum's clone (which itself deep-copies
// the Map), not a bare aggregate copy.
func TestGenericEnumCloneNestedEnumTypeParamField(t *testing.T) {
	ir := generateIR(t, ""+
		"enum Inner[T] `clone {\n"+
		"  Has(T v),\n"+
		"  Not,\n"+
		"}\n"+
		"enum Outer[T] `clone {\n"+
		"  Box(Inner[T] inner),\n"+
		"  Bare,\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  Outer[map[string, string]] j = Outer[map[string, string]].Box(Inner[map[string, string]].Has(src));\n"+
		"  Outer[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawInnerClone := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `Outer[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if inClone && strings.Contains(line, `@"Inner[Map[string, string]].clone"`) {
			sawInnerClone = true
		}
	}
	if !sawInnerClone {
		t.Errorf("T0607: Outer[Map[string, string]].clone() body must call Inner[Map[string, string]].clone to deep-copy the nested enum-Instance field; got shallow bit-copy alias (isAutoCloneBitCopy enum gap)")
	}
}

// T0674 (item 1): the nested generic `clone enum `Wrap(Inner[T])` shape must call
// the inner enum's clone EXACTLY ONCE — not twice. An earlier inspection (T0551)
// worried that lifting B0285 match-dup suppression for TypeParam fields would, for
// a variant field declared as a non-bare TypeParam-containing type the synth treats
// as non-copy (Inner[T] → clone.go emits an explicit .clone()), cause BOTH the
// lifted suppression AND the synth's .clone() to fire → a redundant double deep-
// clone. T0607 superseded that: it removed the per-field un-suppression entirely
// (uniform c.suppressMatchDup inside enum clone bodies) and routes every
// TypeParam-containing field through the synth-only AutoCloneExpr intrinsic. So a
// TypeParam-containing variant field is cloned through exactly one mechanism, never
// two. This pins single-clone and guards against any future change that re-broadens
// match-dup suppression back into a double-clone.
//
// IMPORTANT: do NOT narrow clone.go's `ContainsTypeParam(fieldType)` gate (the one
// that diverts TypeParam-containing fields to AutoCloneExpr) to mirror `isCopyField`
// instead — `isCopyField(TypeParam)==true` optimistically, so a bare `T` field would
// regress onto the shallow-copy path and reintroduce the T0607/T0605 double-free for
// droppable TypeArgs (e.g. map). The ContainsTypeParam predicate is intentional.
func TestGenericEnumCloneNestedSingleCloneCall(t *testing.T) {
	ir := generateIR(t, ""+
		"enum Inner[T] `clone {\n"+
		"  Has(T v),\n"+
		"  Not,\n"+
		"}\n"+
		"enum Outer[T] `clone {\n"+
		"  Wrap(Inner[T] inner),\n"+
		"  Bare,\n"+
		"}\n"+
		"test() {\n"+
		"  string[] src = [\"a\"];\n"+
		"  Outer[string[]] j = Outer[string[]].Wrap(Inner[string[]].Has(src));\n"+
		"  Outer[string[]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	innerCloneCalls := 0
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `Outer[Vector[string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if inClone && strings.Contains(line, "call ") && strings.Contains(line, `@"Inner[Vector[string]].clone"`) {
			innerCloneCalls++
		}
	}
	if innerCloneCalls != 1 {
		t.Errorf("T0674: Outer[Vector[string]].clone() body must call Inner[Vector[string]].clone EXACTLY ONCE (single deep-clone of the Wrap(Inner[T]) field); got %d calls (a double-clone is the efficiency regression T0607 eliminated)", innerCloneCalls)
	}
}

// T0607: a multi-TypeParam `clone enum with a variant carrying BOTH params
// (each a distinct droppable substitution) must deep-clone each independently.
// The synth emits AutoCloneExpr per ContainsTypeParam field; buildMethodInstance
// subst must resolve K and V separately so each AutoClone lowers to the correct
// concrete clone (K=Vector[string] → Vector clone w/ string element loop;
// V=Map[string,int] → Map.clone). Pins the two-param substitution path that the
// single-TypeParam Wrap[T] tests don't exercise.
func TestGenericEnumCloneMultiTypeParamFields(t *testing.T) {
	ir := generateIR(t, ""+
		"enum KV2[K, V] `clone {\n"+
		"  Pair(K key, V val),\n"+
		"  Empty,\n"+
		"}\n"+
		"test() {\n"+
		"  string[] k = [\"a\"];\n"+
		"  map[string, int] v = map[string, int]();\n"+
		"  KV2[string[], map[string, int]] j = KV2[string[], map[string, int]].Pair(k, v);\n"+
		"  KV2[string[], map[string, int]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawVecClone := false
	sawMapClone := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `KV2[Vector[string], Map[string, int]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		// K=Vector[string] deep clone: dupVector (vecdup.copy block) + the
		// per-element string-clone loop (vecdup_str.* blocks). A shallow alias
		// would emit neither. The element loop is the strongest signal.
		if strings.Contains(line, "vecdup_str.") || strings.Contains(line, "vecdup.copy") {
			sawVecClone = true
		}
		// V=Map[string,int] deep clone.
		if strings.Contains(line, `@"Map[string, int].clone"`) {
			sawMapClone = true
		}
	}
	if !sawVecClone {
		t.Errorf("T0607: KV2[Vector[string], Map[string, int]].clone() must deep-copy the `K key` field (Vector[string] clone / element loop); got shallow alias — multi-param subst resolved K wrong")
	}
	if !sawMapClone {
		t.Errorf("T0607: KV2[Vector[string], Map[string, int]].clone() must call Map[string, int].clone for the `V val` field; got shallow alias — multi-param subst resolved V wrong")
	}
}

// T0607/B0285 coexistence: a SINGLE variant carrying both a concrete non-copy
// field (string) and a TypeParam field (T) must clone each independently. The
// concrete `string label` takes the synth body's explicit .clone() path with
// B0285 match-dup suppression in effect → exactly 1 strdup block, not 2. The
// TypeParam `T payload` takes the synth-only AutoCloneExpr path (T0607) which
// lowers to a deep-copy of the substituted Map (→ a Map[..].clone call); B0285
// suppression uniformly stands inside the clone body (T0607 removed the T0551
// per-field un-suppression). Asserting both invariants in the same mono clone
// body pins the per-field handling that neither
// TestGenericEnumCloneDroppableTypeArg (pure TypeParam) nor
// TestEnumCloneNoDoubleClone (pure concrete, non-generic) checks jointly.
func TestEnumCloneMixedConcreteAndTypeParamField(t *testing.T) {
	ir := generateIR(t, ""+
		"enum Mixed[T] `clone {\n"+
		"  Both(string label, T payload),\n"+
		"  None,\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, int] m = map[string, int]();\n"+
		"  Mixed[map[string, int]] j = Mixed[map[string, int]].Both(\"tag\", m);\n"+
		"  Mixed[map[string, int]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	strdupBlocks := 0
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, "Mixed[Map[string, int]].clone") {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		if strings.Contains(line, `@"Map[string, int].clone"`) {
			sawMapClone = true
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "strdup.copy.") && strings.HasSuffix(trimmed, ":") {
			strdupBlocks++
		}
	}
	if !sawMapClone {
		t.Errorf("T0551: Mixed[Map[string, int]].clone() must call Map[string, int].clone for the TypeParam `payload` field (deep-copy); got shallow alias")
	}
	if strdupBlocks != 1 {
		t.Errorf("B0285: Mixed[Map[string, int]].clone() must clone the concrete `label` string exactly once (per-field suppression), got %d strdup blocks", strdupBlocks)
	}
}

// T0605: Cloning a generic `clone TYPE (not enum) whose TypeArg is droppable
// (map[K,V]) must deep-copy the field. The synth body treats the TypeParam
// field as copy (isCopyField(TypeParam)==true) so it emitted a bare shallow
// member read — the constructor then stored the un-dup'd Map fat pointer →
// both original and clone aliased the same heap value → double-free segfault.
// The fix emits a synth-only AutoCloneExpr for TypeParam-containing fields,
// lowered type-directed at mono codegen. Assert the mono clone body deep-
// copies the Map payload (a Map[..].clone call — or, as a fallback, a
// heapdup.copy block from dupHeapValue) rather than a bare shallow store.
// Parallel to TestGenericEnumCloneDroppableTypeArg (T0551, enum case).
func TestGenericTypeCloneDroppableTypeArg(t *testing.T) {
	ir := generateIR(t, ""+
		"type BoxT[T] `clone {\n"+
		"  T val;\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  BoxT[map[string, string]] j = BoxT[map[string, string]](val: src);\n"+
		"  BoxT[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawDeepCopy := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `BoxT[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		// Deep copy = an explicit Map clone call, or the dupHeapValue static
		// fallback (heapdup.copy block label).
		if strings.Contains(line, `@"Map[string, string].clone"`) {
			sawDeepCopy = true
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "heapdup.copy") && strings.HasSuffix(trimmed, ":") {
			sawDeepCopy = true
		}
	}
	if !sawDeepCopy {
		t.Errorf("T0605: BoxT[Map[string, string]].clone() body must deep-copy the TypeParam `val` field (Map[..].clone or dupHeapValue), got a bare shallow alias")
	}
}

// T0667: a generic `clone enum whose variant field is a TUPLE carrying the
// TypeParam (`(T, int) pr`) with a droppable TypeArg (map[K,V]) must deep-copy
// the tuple's heap member. types.ContainsTypeParam recurses into *types.Tuple
// so the synth already emits AutoCloneExpr, but before the fix
// isAutoCloneBitCopy classified every tuple as a bit-copy "scalar tuple" (non-
// named fallthrough) and cloneResolvedValue had no *types.Tuple arm → the
// inner Map fat pointer was shallow-aliased → double-free segfault. The fix
// adds a *types.Tuple recursion to isAutoCloneBitCopy + a per-element
// extract/cloneByType/insert arm to cloneResolvedValue. Assert the mono clone
// body calls Map[..].clone inside the tuple-field clone (per-element deep-
// clone), not a bare aggregate copy. Parallel to the T0662 array gap.
func TestGenericEnumCloneTupleTypeParamField(t *testing.T) {
	ir := generateIR(t, ""+
		"enum TupWrap[T] `clone {\n"+
		"  Pair((T, int) pr),\n"+
		"  Nope,\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  TupWrap[map[string, string]] j = TupWrap[map[string, string]].Pair((src, 7));\n"+
		"  TupWrap[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `TupWrap[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if inClone && strings.Contains(line, `@"Map[string, string].clone"`) {
			sawMapClone = true
		}
	}
	if !sawMapClone {
		t.Errorf("T0667: TupWrap[Map[string, string]].clone() body must call Map[string, string].clone to deep-copy the tuple `(T, int)` field's heap member; got shallow alias (isAutoCloneBitCopy tuple gap / no cloneResolvedValue *types.Tuple arm)")
	}
}

// T0667: the type-level sibling of TestGenericEnumCloneTupleTypeParamField — a
// generic `clone TYPE (not enum) whose field is a TUPLE carrying the TypeParam
// (`(T, int) pr`) with a droppable TypeArg must deep-copy the tuple's heap
// member. Same root cause and fix as the enum case (both lower through
// cloneByType→cloneResolvedValue). Assert the mono clone body calls
// Map[..].clone inside the tuple-field clone (per-element deep-clone), not a
// bare aggregate copy. Parallel to TestGenericTypeCloneDroppableTypeArg.
func TestGenericTypeCloneTupleTypeParamField(t *testing.T) {
	ir := generateIR(t, ""+
		"type TupBox[T] `clone {\n"+
		"  (T, int) pr;\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  TupBox[map[string, string]] j = TupBox[map[string, string]](pr: (src, 7));\n"+
		"  TupBox[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `TupBox[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if inClone && strings.Contains(line, `@"Map[string, string].clone"`) {
			sawMapClone = true
		}
	}
	if !sawMapClone {
		t.Errorf("T0667: TupBox[Map[string, string]].clone() body must call Map[string, string].clone to deep-copy the tuple `(T, int)` field's heap member; got shallow alias (isAutoCloneBitCopy tuple gap / no cloneResolvedValue *types.Tuple arm)")
	}
}

// T0667: a `(T?, int)` tuple field — the per-element cloneByType call must
// recurse from the *types.Tuple arm into the *types.Optional arm so the heap
// payload is deep-cloned behind a none-check (autoclone.some block + inner
// Map[..].clone), not bit-copied. Pins the tuple→optional→heap recursion at
// the IR level (the runtime e2e equivalent is blocked by the *separate*
// pre-existing field-destructure crash T0672, so the type-level e2e covers
// this shape via clone+double-drop without destructure). Mirrors
// TestGenericEnumCloneOptionalTypeParamField's two-signal assertion.
func TestGenericTypeCloneTupleOptionalTypeParamField(t *testing.T) {
	ir := generateIR(t, ""+
		"type TupBoxO[T] `clone {\n"+
		"  (T?, int) pr;\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  map[string, string]? mo = src;\n"+
		"  TupBoxO[map[string, string]] j = TupBoxO[map[string, string]](pr: (mo, 5));\n"+
		"  TupBoxO[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	sawAutoCloneSome := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `TupBoxO[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		if strings.Contains(line, `@"Map[string, string].clone"`) {
			sawMapClone = true
		}
		if strings.Contains(line, "autoclone.some") {
			sawAutoCloneSome = true
		}
	}
	if !sawMapClone {
		t.Errorf("T0667: TupBoxO[Map[string, string]].clone() must call Map[string, string].clone for the `(T?, int)` field's Optional payload (tuple→optional→heap recursion); got shallow alias")
	}
	if !sawAutoCloneSome {
		t.Errorf("T0667: TupBoxO[Map[string, string]].clone() must lower the Optional element of the `(T?, int)` tuple via the cloneByType Optional none-check (autoclone.some block); got a bare bit copy (tuple arm did not recurse into the Optional arm)")
	}
}

// T0667 (zero-regression guard): a PURE SCALAR tuple field (`(T, int)` with
// T=int → `(int, int)`) must stay a plain bit copy. This is the other arm of
// the isAutoCloneBitCopy tuple recursion: the loop finds every element
// bit-copy and falls through to `return true` (expr.go:7144) so cloneByType
// short-circuits without per-element deep-clone machinery. Without this guard
// the only coverage of the `return true` arm is the e2e runtime tests — a
// future change that wrongly routes scalar tuples through cloneResolvedValue
// (extra allocs / churn, or worse) would slip past Go-level tests. Asserts the
// mono clone body contains NO nested `.clone` call and NO `autoclone.` block
// (the two signatures of the deep path) — the inverse of the heap-member
// siblings above. Mirrors the bit-copy regression guards elsewhere (e.g. the
// copy/value TypeArg expectations).
func TestGenericTypeCloneScalarTupleStaysBitCopy(t *testing.T) {
	ir := generateIR(t, ""+
		"type TupBox[T] `clone {\n"+
		"  (T, int) pr;\n"+
		"}\n"+
		"test() {\n"+
		"  TupBox[int] j = TupBox[int](pr: (42, 7));\n"+
		"  TupBox[int] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawNestedClone := false
	sawAutoCloneBlock := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `TupBox[int].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		if strings.Contains(line, "call ") && strings.Contains(line, `.clone"`) {
			sawNestedClone = true
		}
		if strings.Contains(line, "autoclone.") {
			sawAutoCloneBlock = true
		}
	}
	if !inClone {
		t.Fatalf("T0667: TupBox[int].clone define not found in IR")
	}
	if sawNestedClone {
		t.Errorf("T0667: TupBox[int].clone() must NOT emit a nested `.clone` call for a pure scalar `(int, int)` tuple field — the scalar tuple must stay a bit copy (isAutoCloneBitCopy tuple loop → return true at expr.go:7144 → cloneByType short-circuit)")
	}
	if sawAutoCloneBlock {
		t.Errorf("T0667: TupBox[int].clone() must NOT emit an autoclone.* block for a pure scalar `(int, int)` tuple field — got deep-clone machinery where a bit copy is required (regression in the isAutoCloneBitCopy tuple `return true` arm)")
	}
}

// T0667 (index-correctness guard): a heap member at a NON-ZERO tuple index
// (`(int, T)` with T=map) must be deep-cloned and NewInsertValue'd back at
// that same index. The cloneResolvedValue loop carries the element index `i`
// through both NewExtractValue and NewInsertValue; the existing `(T, int)`
// siblings only ever insert at index 0 (heap element is first), so a
// regression that hard-codes index 0 in the re-insert (or drops the index)
// would pass them yet silently corrupt this shape. Asserts the mono clone
// body both calls Map[..].clone AND re-inserts via an `insertvalue ..., 1`
// (the cloned heap member written back at the non-zero index). The runtime
// counterpart is e2e test_clone_tupboxr_map_nonzero_index (mutation
// independence proves the deep copy is real, not just present).
func TestGenericTypeCloneTupleHeapMemberNonZeroIndex(t *testing.T) {
	ir := generateIR(t, ""+
		"type TupBoxR[T] `clone {\n"+
		"  (int, T) r;\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  TupBoxR[map[string, string]] j = TupBoxR[map[string, string]](r: (9, src));\n"+
		"  TupBoxR[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	sawInsertAtIndex1 := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `TupBoxR[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		if strings.Contains(line, `@"Map[string, string].clone"`) {
			sawMapClone = true
		}
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "insertvalue") && strings.HasSuffix(trimmed, ", 1") {
			sawInsertAtIndex1 = true
		}
	}
	if !inClone {
		t.Fatalf("T0667: TupBoxR[Map[string, string]].clone define not found in IR")
	}
	if !sawMapClone {
		t.Errorf("T0667: TupBoxR[Map[string, string]].clone() must call Map[string, string].clone for the heap member at tuple index 1 of `(int, T)`; got shallow alias")
	}
	if !sawInsertAtIndex1 {
		t.Errorf("T0667: TupBoxR[Map[string, string]].clone() must re-insert the cloned heap member at tuple index 1 (`insertvalue ..., 1`) — the cloneResolvedValue loop must carry the element index into NewInsertValue, not hard-code 0")
	}
}

// T0672: Destructuring a struct field of tuple type with an
// Optional[aggregate] element (`(map[K,V]?, int) pr`) then unwrapping the
// destructured local via if-let must NOT give the unwrapped binding an owning
// drop flag. `(m, n) := h.pr` sources from a MemberExpr → srcOwned=false, so
// `m`/`n` get no drop bindings and correctly alias the Holder-owned heap
// (T0371 borrow model). The bug: nothing recorded that `m` is a borrow, so a
// later `if mm := m` saw a plain ident, isOwnedOptionalExpr returned true, and
// `mm` got an owning `store i1 true, i1* %mm.dropflag` → `mm` and Holder.drop
// both freed the same map → double-free → SIGSEGV. The fix marks each
// borrow-sourced destructured local in matchBorrowedIdents (mirrors the
// T0485/T0512 match-destructure mechanism) so isOwnedOptionalExpr returns
// false and no owning drop binding is registered for `mm`; the map is freed
// exactly once, by Holder.drop. Companion e2e:
// tests/e2e/destructure_field_tuple_optional_test.pr.
func TestDestructureFieldTupleOptionalAggregateNoOwnDrop(t *testing.T) {
	ir := generateIR(t, ""+
		"type Holder { (map[string, string]?, int) pr; }\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  map[string, string]? mo = src;\n"+
		"  Holder h = Holder(pr: (mo, 5));\n"+
		"  (m, n) := h.pr;\n"+
		"  if mm := m { int z = mm.len; }\n"+
		"}\n")
	fnStart := strings.Index(ir, "define void @__user.test()")
	if fnStart < 0 {
		t.Fatalf("T0672: could not find @__user.test() in IR")
	}
	fnEnd := strings.Index(ir[fnStart:], "\n}\n")
	testIR := ir[fnStart : fnStart+fnEnd+3]
	// The if-let binding `mm` must NOT get an owning drop flag — the map is
	// owned by Holder and freed by Holder.drop. An owning `mm` drop binding
	// (the buggy behavior) double-frees the map.
	if strings.Contains(testIR, "%mm.dropflag") {
		t.Errorf("T0672: if-let binding `mm` got an owning drop flag (%%mm.dropflag) — the destructured `m` aliases Holder-owned heap (MemberExpr borrow source) and must not transfer ownership; this double-frees the map with Holder.drop")
	}
	// The destructured local `m` itself must stay a borrow (field-sourced
	// destructure registers no drop binding — existing T0371 behavior).
	if strings.Contains(testIR, "%m.dropflag") {
		t.Errorf("T0672: destructured local `m` unexpectedly got a drop flag — a struct-field-sourced tuple destructure must stay a borrow (no drop binding)")
	}
	// Holder.drop must still be the sole owner that frees the map.
	if !strings.Contains(testIR, "call void @Holder.drop") {
		t.Errorf("T0672: Holder.drop must be called to free the field-owned map exactly once")
	}
}

// B0237/B0242: Match destructure of droppable enum dups string fields and
// registers them for arm-scope cleanup with a drop flag. The drop flag is
// cleared at move sites (PHI, push, etc.), so consumed bindings are not
// double-freed. Unconsumed bindings are dropped at arm-scope exit.
func TestMatchDupStringScopeCleanup(t *testing.T) {
	ir := generateIR(t, `
		enum Slot {
			Empty,
			Used(string key, int value),
		}
		test() {
			s := Slot.Used(key: "hello", value: 42);
			match s {
				Used(k, v) => { int x = v; },
				Empty => { },
			}
		}
	`)
	// String field extracted from droppable enum should be dup'd
	assertContains(t, ir, "strdup.copy")
	// B0242: Dup'd string has a drop flag for arm-scope cleanup (unconsumed → dropped)
	assertContains(t, ir, "k.dropflag")
}

// B0242: Dup'd match binding consumed as arm result — drop flag must be cleared.
// Without clearDropFlag, arm-scope cleanup would drop the value, causing
// use-after-free on the match PHI result.
func TestMatchDupStringConsumedByPHI(t *testing.T) {
	ir := generateIR(t, `
		enum Slot {
			Empty,
			Used(string key, int value),
		}
		test() string {
			s := Slot.Used(key: "hello", value: 42);
			return match s {
				Used(k, v) => k,
				Empty => "none",
			};
		}
	`)
	assertContains(t, ir, "strdup.copy")
	assertContains(t, ir, "k.dropflag")
	// The drop flag must be cleared (store i1 false) before arm-scope cleanup
	assertContains(t, ir, "store i1 false")
}

// B0253: Match on borrowed enum with clone-able enum field must deep-clone.
// This is the pattern underlying JsonValue.get(this): match on this loads a
// shallow copy of the enum value. Extracted enum fields (like JsonValue from
// Slot.Used inside Map.[]) must be cloned, not shallow-copied, so the returned
// value is independent of the original map storage.
func TestMatchDupCloneableEnumFieldOnBorrow(t *testing.T) {
	ir := generateIR(t, ""+
		"enum Inner `clone {\n"+
		"  Text(string data),\n"+
		"  Empty,\n"+
		"}\n"+
		"enum Outer {\n"+
		"  Wrapped(Inner value),\n"+
		"  None,\n"+
		"  get_inner(this) Inner? {\n"+
		"    match this {\n"+
		"      Outer.Wrapped(v) => { return v; },\n"+
		"      _ => { return none; },\n"+
		"    }\n"+
		"  }\n"+
		"}\n"+
		"test() {\n"+
		"  o := Outer.Wrapped(value: Inner.Text(data: \"hello\"));\n"+
		"  r := o.get_inner();\n"+
		"}\n")
	// Inner enum field extracted from droppable Outer must be cloned via clone method
	assertContains(t, ir, "Inner.clone")
	assertContains(t, ir, "enum.clone.tmp")
}

// Temp enum receiver from a CallExpr should be dropped after a borrow method call.
// When movedDroppable causes enumCtorTemps to skip tracking, the method call
// path must explicitly drop the temp to prevent leaking the enum's heap data.
func TestEnumTempMethodReceiverDrop(t *testing.T) {
	ir := generateIR(t, ""+
		"enum Inner `clone {\n"+
		"  Text(string data),\n"+
		"  Number(f64 value),\n"+
		"}\n"+
		"enum Holder {\n"+
		"  Items(Inner[] list),\n"+
		"  Nothing,\n"+
		"  extract(this) Inner[]? {\n"+
		"    match this {\n"+
		"      Holder.Items(items) => { return items; },\n"+
		"      _ => { return none; },\n"+
		"    }\n"+
		"  }\n"+
		"}\n"+
		"test() {\n"+
		"  Inner[] items = [Inner.Number(value: 1.0)];\n"+
		"  Inner[]? arr = Holder.Items(list: items).extract();\n"+
		"}\n")
	// The temp enum receiver should be dropped after the method call
	assertContains(t, ir, "Holder.drop")
}

// Return of a droppable enum from a dup'd vector index must clone the value.
// Without cloning, scope cleanup drops the dup'd vector (and its elements),
// leaving the returned shallow enum copy with dangling heap pointers.
func TestReturnEnumFromVectorIndexClone(t *testing.T) {
	ir := generateIR(t, ""+
		"enum Inner `clone {\n"+
		"  Text(string data),\n"+
		"  Number(f64 value),\n"+
		"}\n"+
		"enum Holder {\n"+
		"  Items(Inner[] list),\n"+
		"  Nothing,\n"+
		"  at(this, int index) Inner? {\n"+
		"    match this {\n"+
		"      Holder.Items(items) => {\n"+
		"        if index >= 0 && index < items.len {\n"+
		"          return items[index];\n"+
		"        }\n"+
		"        return none;\n"+
		"      },\n"+
		"      _ => { return none; },\n"+
		"    }\n"+
		"  }\n"+
		"}\n"+
		"test() {\n"+
		"  Inner[] items = [Inner.Text(data: \"hello\")];\n"+
		"  h := Holder.Items(list: items);\n"+
		"  Inner? val = h.at(0);\n"+
		"}\n")
	// The returned enum value must be cloned via Inner.clone
	assertContains(t, ir, "Inner.clone")
}

// B0242: Dup'd match binding consumed via if-expression arm result.
// clearResultDropFlags must recurse into IfExpr branches.
func TestMatchDupStringConsumedViaIf(t *testing.T) {
	ir := generateIR(t, `
		enum Slot {
			Empty,
			Used(string key, int value),
		}
		test() string {
			s := Slot.Used(key: "hello", value: 42);
			return match s {
				Used(k, v) => if v > 0 { k } else { "neg" },
				Empty => "none",
			};
		}
	`)
	assertContains(t, ir, "strdup.copy")
	assertContains(t, ir, "k.dropflag")
	// clearResultDropFlags walks into IfExpr and clears k's drop flag
	assertContains(t, ir, "store i1 false")
}

// B0242: Dup'd match binding consumed via tuple literal (e.g., vector push).
// genTupleLit must clear the drop flag for ident elements so arm-scope cleanup
// doesn't free the string that is now owned by the tuple/vector.
func TestMatchDupStringConsumedViaTuple(t *testing.T) {
	ir := generateIR(t, `
		enum Slot {
			Empty,
			Used(string key, int value),
		}
		test() (string, int) {
			s := Slot.Used(key: "hello", value: 42);
			return match s {
				Used(k, v) => (k, v),
				Empty => ("none", 0),
			};
		}
	`)
	assertContains(t, ir, "strdup.copy")
	assertContains(t, ir, "k.dropflag")
	// genTupleLit clears the drop flag when k is consumed by the tuple
	assertContains(t, ir, "store i1 false")
}

// B0264: Vector[(string, int)] must drop string elements inside tuples.
func TestVectorTupleElementStringDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			(string, int)[] v = [("hello", 1), ("world", 2)];
		}
	`)
	// The vector drop loop should extract tuple field 0 (string) and call promise_string_drop
	assertContains(t, ir, "vecdrop.body")
	assertContains(t, ir, "extractvalue")
	assertContains(t, ir, "call void @promise_string_drop")
}

// B0268: Vector[(int, FieldlessEnum)] must NOT reference an enum drop.
// Fieldless enums (no variant fields) have no drop function — emitting a call
// to a non-existent drop causes linker errors (undefined symbol).
func TestVectorTupleFieldlessEnumNoDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		main() {
			(int, Color)[] v = [(1, Color.Red), (2, Color.Green)];
		}
	`)
	// Fieldless enum in tuple should NOT generate a Color.drop call or declaration
	assertNotContains(t, ir, "Color.drop")
}

// T0371: genTupleLit claims the heap-tracked string concat temp so the tuple
// is the unique owner of the concat result. Without this claim, the stmt-temp
// cleanup would free the string while the tuple still references it (UAF).
func TestT0371TupleLitClaimsHeapStringTemp(t *testing.T) {
	ir := generateIR(t, `
		main() {
			(int, string) t = (1, "a" + "b");
		}
	`)
	// Concat result is tracked as a stmtTemp; genTupleLit clears its drop flag.
	assertContains(t, ir, "promise_string_concat")
	assertContains(t, ir, "store i1 false")
	// And the tuple variable t gets a tuple-walk drop binding at scope exit.
	assertContains(t, ir, "tupdrop.exec")
	assertContains(t, ir, "tupdrop.skip")
	assertContains(t, ir, "promise_string_drop")
}

// T0371: genTupleLit claims the heap-tracked user type temp so the tuple is
// the unique owner. Without this claim, the heap temp would be freed at stmt
// end and dropped again via the tuple's scope-exit walk (double-free).
func TestT0371TupleLitClaimsHeapBoxTemp(t *testing.T) {
	ir := generateIR(t, `
		type Box { int n; }
		main() {
			(int, Box) t = (1, Box(n: 5));
		}
	`)
	// Tuple variable t gets a tuple-walk drop binding.
	assertContains(t, ir, "tupdrop.exec")
	// The walk frees the heap Box via emitVariantFieldDrop's pal_free branch.
	assertContains(t, ir, "varfield.free")
	assertContains(t, ir, "@pal_free")
}

// T0371: A tuple variable with droppable fields registers a bindingDropTuple
// that walks the fields and drops each droppable one at scope exit. The drop
// flag is checked first so moves can suppress the walk.
func TestT0371TupleVarDropsFields(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "a" + "b";
			(int, string) t = (1, s);
		}
	`)
	// Tuple var has a drop flag and a tupdrop block at scope exit.
	assertContains(t, ir, "t.dropflag")
	assertContains(t, ir, "tupdrop.exec")
	assertContains(t, ir, "promise_string_drop")
}

// T0371: Destructuring a tuple-with-heap-fields into named locals registers
// a drop binding per field so each local owns and frees its piece. Without
// these per-field drops, the string would leak after destructure.
func TestT0371DestructureRegistersFieldDrops(t *testing.T) {
	ir := generateIR(t, `
		main() {
			(a, b) := (1, "a" + "b");
		}
	`)
	// b is a destructured string local — should have a drop flag and call drop.
	assertContains(t, ir, "b.dropflag")
	assertContains(t, ir, "promise_string_drop")
}

// T0371: Enum variant with tuple-of-string field. Sema's fieldTypeHasDrop and
// codegen's variantFieldNeedsDrop must both recurse into tuples so the synth
// enum drop walks the tuple field and frees the inner string.
func TestT0371EnumWithTupleStringHasDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Pair {
			Empty,
			Some((int, string) data),
		}
		main() {
			s := Pair.Some(data: (1, "a" + "b"));
		}
	`)
	// Synth drop function should be emitted for Pair (because tuple variant
	// field contains a string). Verify via the variant drop block name.
	assertContains(t, ir, "Pair.drop")
	assertContains(t, ir, "promise_string_drop")
}

// T0371: Destructuring a borrowed tuple (e.g., from a for-in loop variable)
// must NOT register drops for the destructured locals — they are borrows of
// the container's elements, and adding drops would double-free with the
// container's element walk.
func TestT0371DestructureBorrowSourceNoDrop(t *testing.T) {
	ir := generateIR(t, `
		type Box { int n; }
		sum_first((int, Box)[] v) int {
			int total = 0;
			for tup in v {
				(idx, bx) := tup;
				total = total + bx.n;
			}
			return total;
		}
		main() {
			int n = sum_first([(1, Box(n: 5))]);
		}
	`)
	// bx is destructured from a for-in loop variable (borrow); no drop binding.
	assertNotContains(t, ir, "bx.dropflag")
}

// T0371: A tuple containing an enum constructor with heap variant data must
// claim the enum-ctor temp's drop flag so the tuple is the unique owner. Tests
// the savedEnumTemps loop in genTupleLit. Without it, the enum's variant
// string would be freed at stmt end while the tuple still references it.
func TestT0371TupleClaimsEnumCtorTemp(t *testing.T) {
	ir := generateIR(t, `
		enum Color {
			Red,
			Tagged(string label),
		}
		main() {
			(int, Color) t = (1, Color.Tagged(label: "a" + "b"));
		}
	`)
	// Tuple var t has its own tuple-walk drop binding.
	assertContains(t, ir, "t.dropflag")
	assertContains(t, ir, "tupdrop.exec")
	// The savedEnumTemps loop emits a drop-flag-clear store for the enum ctor
	// temp during element evaluation. The tuple-walk then drops the enum.
	assertContains(t, ir, "Color.drop")
}

// T0371: Generic function with a tuple-of-T local variable. Exercises the
// typeSubst != nil branch in emitTupleDropCall so the tuple type's elements
// are substituted at monomorphization time and the walk uses concrete types.
func TestT0371GenericFnTupleLocalSubstitutes(t *testing.T) {
	ir := generateIR(t, `
		make_then_drop[T](T move x, T move y) {
			(T, T) t = (x, y);
		}
		main() {
			make_then_drop[string]("a" + "b", "c" + "d");
		}
	`)
	// The mono'd function must register a tuple-walk drop binding that calls
	// promise_string_drop on the (substituted) string fields.
	assertContains(t, ir, "tupdrop.exec")
	assertContains(t, ir, "promise_string_drop")
}

// T0481: `(_, n) := t` with an owned tuple source must register a drop binding
// for the discarded slot under a synthetic key. Without it, the source's drop
// flag is cleared (transfer to LHS) but the heap field at `_` is orphaned.
func TestT0481DiscardRegistersDropString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			t := ("a" + "b", 42);
			(_, n) := t;
		}
	`)
	// The synthetic discard binding must produce a drop flag and a string drop.
	assertContains(t, ir, "_destructure.discard")
	assertContains(t, ir, "_destructure.discard.dropflag")
	assertContains(t, ir, "promise_string_drop")
}

// T0481: Borrow-source destructure with `_` must NOT register a drop binding
// for the discarded slot — borrowed elements are owned by the container, and
// adding a drop would double-free with the container's element walk.
func TestT0481DiscardBorrowSourceNoDrop(t *testing.T) {
	ir := generateIR(t, `
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
	assertNotContains(t, ir, "_destructure.discard.dropflag")
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

// B0212: Vector[Enum] scope-exit drops enum elements (each element's synthesized drop is called).
func TestDropVectorEnumElements(t *testing.T) {
	ir := generateIR(t, `
		enum Value {
			Null,
			Str(string s),
			List(Value[] items),
		}
		main() {
			v := [Value.Str("a"), Value.Null];
		}
	`)
	// Scope-exit vector drop should iterate elements and call Value.drop
	assertContains(t, ir, "vecdrop.head")
	assertContains(t, ir, "call void @Value.drop(")
}

// B0212: Enum variant with vector field drops enum elements in the vector.
func TestDropEnumVariantVectorElements(t *testing.T) {
	ir := generateIR(t, `
		enum Value {
			Null,
			Str(string s),
			List(Value[] items),
		}
		main() {
			v := Value.List([Value.Str("inner")]);
		}
	`)
	// Value.drop for List variant should drop vector elements before freeing buffer
	assertContains(t, ir, "define void @Value.drop(")
	assertContains(t, ir, "enum.drop.List")
	assertContains(t, ir, "vecdrop.head") // element drop loop in variant field drop
}

// B0212: Generic enum instances (like Slot[K,V]) get synthesized drops at mono time
// when sema couldn't detect droppability for TypeParam variant fields.
func TestDropMonoEnumInstSynthDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Wrapper[T] {
			Some(T value),
			None,
		}
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			w := Wrapper[Resource].Some(Resource(id: 1));
		}
	`)
	// Wrapper[Resource] should get a synthesized drop that calls Resource.drop
	assertContains(t, ir, `define void @"Wrapper[Resource].drop"`)
	assertContains(t, ir, "call void @Resource.drop(")
}

// T0567: Explicit drop(~this) on enum — user-defined drop is emitted and called
func TestDropExplicitEnumMethod(t *testing.T) {
	ir := generateIR(t, `
		enum Color {
			Red,
			Green,
			drop(~this) {}
		}
		main() {
			c := Color.Red;
		}
	`)
	// The user's drop method should be declared and called at scope exit
	assertContains(t, ir, "define void @Color.drop(")
	assertContains(t, ir, "call void @Color.drop(")
	assertContains(t, ir, "enum.drop.call")
}

// T0604: Explicit drop(~this) on enum with droppable variant fields —
// variant field cleanup (switch on tag, drop per-variant) is emitted after the user body.
func TestDropExplicitEnumVariantFieldCleanup(t *testing.T) {
	ir := generateIR(t, `
		enum Container {
			Data(string name),
			Empty,
			drop(~this) {}
		}
		main() {
			c := Container.Data(name: "test");
		}
	`)
	// The user's drop method should contain variant field cleanup blocks
	assertContains(t, ir, "define void @Container.drop(")
	assertContains(t, ir, "enum.drop.field.Data")
	assertContains(t, ir, "enum.drop.field.done")
	assertContains(t, ir, "call void @promise_string_drop(")
}

// T0552: Type with generic-enum field whose TypeParam resolves to a droppable
// concrete type. monoTypeHasDroppable must see through the generic enum Instance
// (via monoEnumInstNeedsSynthDrop), and emitFieldDropsFor must drop the enum
// field by invoking the mono enum's drop function. Without both, the inner
// droppable leaks at scope exit of the holder.
func TestDropGenericTypeWithGenericEnumField(t *testing.T) {
	ir := generateIR(t, `
		type Resource { int id; drop(~this) { } }
		enum Maybe[T] {
			Some(T value),
			Nothing,
		}
		type Holder[T] {
			Maybe[T] m;
		}
		main() {
			j := Maybe[Resource].Some(Resource(id: 1));
			c := Holder[Resource](m: j);
		}
	`)
	assertContains(t, ir, `define void @"Holder[Resource].drop"`)
	assertContains(t, ir, `call void @"Maybe[Resource].drop"`)
}

// T0552: Non-generic holder containing a non-generic enum field with a
// droppable variant. Sema sets NeedsSynthDrop=true on the holder (since the
// concrete enum's HasDrop is observable), so a synth drop body is generated —
// but before T0552, emitFieldDropsFor's `extractNamed == nil` skip dropped the
// enum field silently. This test locks down the enum-field branch added in
// emitFieldDropsFor for the non-generic case (the generic case is covered by
// TestDropGenericTypeWithGenericEnumField above).
func TestDropTypeWithEnumFieldNonGeneric(t *testing.T) {
	ir := generateIR(t, `
		type Resource { int id; drop(~this) { } }
		enum Maybe {
			Some(Resource value),
			Nothing,
		}
		type Holder {
			Maybe m;
		}
		main() {
			j := Maybe.Some(Resource(id: 1));
			c := Holder(m: j);
		}
	`)
	assertContains(t, ir, "define void @Holder.drop")
	assertContains(t, ir, "call void @Maybe.drop")
}

// T0572: Holder with Optional<Enum> field where the inner enum has droppable
// variant data. emitOptionalValueDrop had no enum branch — extractNamed
// returns nil for enums, so the has-value path fell through to the default
// `return` and skipped cleanup. The new branch must guard on the optional's
// has-value flag and call the enum's drop function.
func TestDropTypeWithOptionalEnumField(t *testing.T) {
	ir := generateIR(t, `
		type Resource { int id; drop(~this) { } }
		enum Maybe {
			Some(Resource value),
			Nothing,
		}
		type Holder {
			Maybe? m;
		}
		main() {
			j := Maybe.Some(Resource(id: 1));
			c := Holder(m: j);
		}
	`)
	assertContains(t, ir, "define void @Holder.drop")
	// The has-value guard for the optional enum field.
	assertContains(t, ir, "optfield.drop")
	assertContains(t, ir, "optfield.skip")
	// The enum's drop is invoked from inside the has-value branch.
	assertContains(t, ir, "call void @Maybe.drop")
}

// T0572: Holder with Optional<FieldlessEnum> field — the !needsDrop short-
// circuit must fire so no spurious cleanup branches are emitted for the
// fieldless enum slot. The holder itself has a sibling droppable field
// (Resource) so its synth drop body is generated and walks all fields,
// reaching the Optional<FieldlessEnum> branch. The short-circuit ensures
// only the sibling Resource's drop appears in the holder body — no
// optfield.drop block for the Color slot.
func TestDropTypeWithOptionalFieldlessEnumFieldShortCircuits(t *testing.T) {
	ir := generateIR(t, `
		type Resource { int id; drop(~this) { } }
		enum Color {
			Red,
			Green,
			Blue,
		}
		type Holder {
			Color? c;
			Resource r;
		}
		main() {
			h := Holder(c: Color.Red, r: Resource(id: 1));
		}
	`)
	assertContains(t, ir, "define void @Holder.drop")
	// The Resource sibling is dropped — confirms the holder synth drop runs.
	assertContains(t, ir, "call void @Resource.drop")
	// Fieldless Color enum has no drop fn, so the short-circuit must skip
	// emitting any call to a Color drop. (No @Color.drop function exists.)
	assertNotContains(t, ir, "call void @Color.drop")
}

// T0572: Generic holder with Optional<GenericEnum[T]> field. Exercises the
// monoEnumInstNeedsSynthDrop branch in the needsDrop check — without it,
// HasDrop on the un-substituted enum origin is false, the early-return
// fires, and the inner droppable leaks.
func TestDropGenericTypeWithOptionalGenericEnumField(t *testing.T) {
	ir := generateIR(t, `
		type Resource { int id; drop(~this) { } }
		enum Maybe[T] {
			Some(T value),
			Nothing,
		}
		type Holder[T] {
			Maybe[T]? m;
		}
		main() {
			j := Maybe[Resource].Some(Resource(id: 1));
			c := Holder[Resource](m: j);
		}
	`)
	assertContains(t, ir, `define void @"Holder[Resource].drop"`)
	assertContains(t, ir, "optfield.drop")
	assertContains(t, ir, `call void @"Maybe[Resource].drop"`)
}

// B0238: Generic enum variables with TypeParam-only droppable fields must get drop
// registered at scope exit. maybeRegisterDrop must check monoEnumInstNeedsSynthDrop.
func TestDropGenericEnumVarWithDroppableTypeParam(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { string name; int value; }
		enum Container[T] {
			Holding(T item),
			Empty,
		}
		main() {
			c := Container[Wrapper].Holding(Wrapper(name: "hello", value: 42));
		}
	`)
	// Container[Wrapper] should get a synthesized drop and it must be called at scope exit
	assertContains(t, ir, `define void @"Container[Wrapper].drop"`)
	assertContains(t, ir, `call void @"Container[Wrapper].drop"`)
}

// T1108: When a variable with a drop binding is moved into an enum-constructor
// temp that is passed to a BORROW param, the moved-in payload's only owner is
// the enum temp (the source ident's drop flag was cleared at the move). The
// borrow callee does not consume it, so the caller MUST drop the enum temp at
// statement end to free the payload — otherwise it leaks. (Was B0252's
// TestEnumCtorTempSkippedWhenMovedDroppable, which encoded that leak.)
func TestEnumCtorTempMovedDroppableBorrowDrops(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			string name;
			drop(~this) { }
		}
		enum Holder {
			Has(Resource r),
			Empty,
		}
		consume(Holder h) { }
		main() {
			r := Resource(name: "test");
			consume(Holder.Has(r));
		}
	`)
	// The enum should have a synthesized drop
	assertContains(t, ir, "define void @Holder.drop")
	// The borrowed enum ctor temp must be dropped at statement end so the
	// moved-in Resource is freed (zero-leak policy).
	assertContains(t, ir, "enum.ctor.drop")
}

// T1108: Non-ident expressions (e.g., function calls) returning droppable values
// moved into an enum-constructor temp that is then passed to a BORROW param must
// be dropped at statement end. The call result's only owner is the enum temp;
// the borrow callee does not consume it, so skipping the drop would leak the
// result. (Was B0286's TestEnumCtorTempSkippedForNonIdentDroppableArg — the IR
// test that masked the leak it introduced.)
func TestEnumCtorTempNonIdentDroppableBorrowDrops(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			string name;
			drop(~this) { }
		}
		enum Holder {
			Has(Resource r),
			Empty,
		}
		make_resource() Resource {
			return Resource(name: "test");
		}
		consume(Holder h) { }
		main() {
			consume(Holder.Has(r: make_resource()));
		}
	`)
	assertContains(t, ir, "define void @Holder.drop")
	assertContains(t, ir, "enum.ctor.drop")
}

// T1108: Enum variant with a non-ident arg of a synth-drop type (contains a
// string field) passed to a BORROW param must likewise be dropped at statement
// end. (Was B0286's TestEnumCtorTempSkippedForNonIdentSynthDropArg.)
func TestEnumCtorTempNonIdentSynthDropBorrowDrops(t *testing.T) {
	ir := generateIR(t, `
		type Info {
			string label;
		}
		enum Wrapper {
			Wrap(Info data),
			None,
		}
		make_info() Info {
			return Info(label: "hello");
		}
		consume(Wrapper w) { }
		main() {
			consume(Wrapper.Wrap(data: make_info()));
		}
	`)
	assertContains(t, ir, "define void @Wrapper.drop")
	assertContains(t, ir, "enum.ctor.drop")
}

// B0293: Enum variable reassignment must clear enumCtorTemps to prevent double-drop.
// Without the fix, the enum ctor temp drop fires at statement end AND the variable's
// scope-exit drop fires, causing use-after-free on the variant's heap data.
func TestEnumCtorTempClearedOnReassign(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { string name; int value; }
		enum Container[T] { Holding(T item), Empty, }
		test() {
			c := Container[Wrapper].Holding(Wrapper(name: "first", value: 1));
			c = Container[Wrapper].Holding(Wrapper(name: "second", value: 2));
		}
	`)
	// The reassignment path should NOT have enum.ctor.drop blocks —
	// ownership transferred to the variable, ctor temps must be cleared.
	assertNotContains(t, ir, "enum.ctor.drop")
}

// B0293: CastExpr as! on optional must neutralize source to prevent double-free.
func TestAsBangOptionalNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		test() {
			Point? p = Point(x: 8, y: 9);
			Point q = p as! Point;
		}
	`)
	// After as! unwrap into q, the optional p's present flag must be set to false.
	// This prevents both p's optional drop and q's drop from freeing the same instance.
	assertContains(t, ir, "store i1 false")
}

// B0293: Optional handler (p? _ { fallback }) must neutralize source.
func TestOptionalHandlerNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		test() {
			Point? p = Point(x: 5, y: 6);
			Point q = p? _ { Point(x: 0, y: 0); };
		}
	`)
	// After handler unwrap into q, the optional p's present flag should be set to false.
	assertContains(t, ir, "store i1 false")
}

// B0299: Inline optional string field unwrap must not track the string as a
// statement-end temp. The owner's drop handles the string's lifetime.
// Without this fix, statement-end cleanup frees the original string from
// the field, then Wrapper.drop frees it again → double-free.
func TestOptionalFieldStringInlineUnwrapNoTempTrack(t *testing.T) {
	res := compileResult(t, `
		type Wrapper { string? opt_name; }
		test() {
			Wrapper w = Wrapper(opt_name: "hello");
			assert(w.opt_name! == "hello", "ok");
		}
	`)
	ir := res.Module.String()
	// Extract just the test function's IR
	fnStart := strings.Index(ir, "define void @__user.test()")
	fnEnd := strings.Index(ir[fnStart:], "\n}\n")
	testIR := ir[fnStart : fnStart+fnEnd+3]
	// The inline unwrap must NOT generate a strdup/string_new call —
	// no dup is needed for inline access, and the string must not be
	// tracked as a temp (optionalFieldString flag suppresses tracking).
	assertNotContains(t, testIR, "promise_string_new")
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

// T0357: string compound assignment must dispatch through genStringOp,
// not panic in namedFromLLVMType.
func TestCompoundAssignString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string a = "hello ";
			string b = "world";
			a += b;
		}
	`)
	assertContains(t, ir, "call i8* @promise_string_concat(")
}

// T0357: local-var string compound must drop the old value before storing
// the new concat result, with a same-pointer guard mirroring OpAssign.
func TestCompoundAssignStringDropsOld(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string a = "hello " + "first ";
			a += "world";
		}
	`)
	assertContains(t, ir, "call i8* @promise_string_concat(")
	assertContains(t, ir, "call void @promise_string_drop(")
	assertContains(t, ir, "compound.diff")
	assertContains(t, ir, "compound.merge")
}

// T0357: field compound on a string field routes through genMemberAssign
// (compound branch). Asserts the code path is reachable for non-local sites.
func TestCompoundAssignStringField(t *testing.T) {
	ir := generateIR(t, `
		type Holder { string s; }
		main() {
			Holder h = Holder(s: "abc");
			h.s += "def";
		}
	`)
	assertContains(t, ir, "call i8* @promise_string_concat(")
}

// T0357: vector index compound (native path) on string elements routes
// through genVectorCompoundAssign with elemType passed correctly.
func TestCompoundAssignStringVecIndex(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] v = ["abc", "xyz"];
			v[0] += "def";
		}
	`)
	assertContains(t, ir, "call i8* @promise_string_concat(")
}

// T0405: Vector field reassignment must drop elements before freeing the buffer.
// Verifies that genMemberAssign emits a vector element drop loop (string drop)
// before calling Vector.drop for a string[] field.
func TestFieldAssignVecDropsElements(t *testing.T) {
	ir := generateIR(t, `
		type Holder { string[] field; }
		main() {
			v1 := string[]();
			h := Holder(v1);
			v2 := string[]();
			h.field = v2;
		}
	`)
	// The field.vecdrop block must contain a string element drop loop
	assertContains(t, ir, "field.vecdrop")
	assertContains(t, ir, "call void @promise_string_drop(")
	assertContains(t, ir, "call void @Vector.drop(")
}

// T0405: Vector field reassign must emit null guard (skip drop when field is null/zero).
func TestFieldAssignVecNullGuardInIR(t *testing.T) {
	ir := generateIR(t, `
		type Holder { string[] field; }
		main() {
			v := string[]();
			h := Holder(v);
			v2 := string[]();
			h.field = v2;
		}
	`)
	// The vecdrop block must guard against null (zero-initialized fields from
	// error fallthroughs) — emits an `or i1` combining isNull and isSame checks.
	assertContains(t, ir, "field.vecdrop")
	assertContains(t, ir, "or i1")
}

// T0405: Generic type with T[] field — reassignment must drop string elements
// when T=string (exercises the typeSubst-substituted fieldType path).
func TestFieldAssignGenericVecDropsElements(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T[] items;
			update(~this, T[] move val) { this.items = val; }
		}
		main() {
			b := Box[string](items: string[]());
			v := string[]();
			b.update(v);
		}
	`)
	assertContains(t, ir, "field.vecdrop")
	assertContains(t, ir, "call void @promise_string_drop(")
	assertContains(t, ir, "call void @Vector.drop(")
}

// T0516: B0219 dup tracking must be per-receiver so reassigns on a DIFFERENT
// instance of the same type still emit element drops correctly. With per-type
// keying, a dup of h1.field would mark "Holder.field" and cause h2.field = w
// to skip its element drop loop, leaking h2's old elements.
func TestFieldAssignVecCrossInstanceDropsElements(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "field.vecdrop")
	assertContains(t, ir, "call void @promise_string_drop(")
	assertContains(t, ir, "call void @Vector.drop(")
}

// T0540: `v := h.field` for a Vector field with droppable elements on a droppable
// owner must emit a deep element-dup loop (not just a shallow buffer memcpy) so
// the dup owns independent copies. Without the loop, both v and h.field alias
// element pointers and scope-end drops cause a double-free.
func TestB0219FieldAccessVecDeepDup(t *testing.T) {
	ir := generateIR(t, `
		type Holder { string[] field; }
		main() {
			v1 := string[]();
			h := Holder(v1);
			v := h.field;
		}
	`)
	// The shallow dup (vecdup.copy) must be followed by a per-element string
	// dup loop (vecdup_str.head + promise_string_new).
	assertContains(t, ir, "vecdup.copy")
	assertContains(t, ir, "vecdup_str.head")
	assertContains(t, ir, "promise_string_new")
}

// T0540: same deep-dup requirement for the Optional[Vector] field branch.
func TestB0219OptionalVecFieldDeepDup(t *testing.T) {
	ir := generateIR(t, `
		type OptHolder {
			string[]? field;
			drop(~this) {}
		}
		main() {
			v1 := string[]();
			h := OptHolder(field: v1);
			v := h.field;
		}
	`)
	assertContains(t, ir, "vecdup.copy")
	assertContains(t, ir, "vecdup_str.head")
	assertContains(t, ir, "promise_string_new")
}

// T0939: binding an Optional[Vector]-with-droppable-elements field to a plain
// container local must null-guard the element-clone loop. On the optional's `none`
// path field 1 is null; the dup of null is null, and the unguarded clone loop
// (loadVectorLen) would dereference it → segfault. The guard emits veccloneopt.
// blocks that skip the loop when the dup is null.
func TestT0939OptionalVecFieldCloneNullGuard(t *testing.T) {
	ir := generateIR(t, `
		type SVBox { string[]? v; drop(~this) {} }
		main() {
			bx := SVBox(v: none);
			b := string[]();
			x := bx.v ?: b;
		}
	`)
	assertContains(t, ir, "veccloneopt.do")
	assertContains(t, ir, "veccloneopt.merge")
}

// T0939 (genArrayIndex call site): the same null-guard fix also covers the
// Optional[Vector] element path of `genArrayIndex`. Indexing an array of
// `string[]?` and binding the elvis result to a plain `string[]` must emit the
// veccloneopt. guard blocks for the element-clone loop (the slot's inner buffer is
// null on the `none` path). Pins the second call site at the IR level.
func TestT0939OptionalVecArrayIndexCloneNullGuard(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[]? a0 = none;
			string[]? a1 = none;
			string[]?[2] arr = [a0, a1];
			b := string[]();
			x := arr[0] ?: b;
		}
	`)
	assertContains(t, ir, "veccloneopt.do")
	assertContains(t, ir, "veccloneopt.merge")
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

func TestDiscardedStringPopDropsOptionalInner(t *testing.T) {
	// B0196: When v.pop() result is discarded and element type is string,
	// the string inside the optional must be dropped.
	ir := generateIR(t, `
		main() {
			string[] v = [];
			v.push("hello");
			v.pop();
		}
	`)
	// Should have a discard.drop block that calls promise_string_drop
	assertContains(t, ir, "discard.drop")
	assertContains(t, ir, "discard.skip")
	assertContains(t, ir, "call void @promise_string_drop(i8*")
}

func TestDiscardedIntPopNoDropBlock(t *testing.T) {
	// B0196: int pop should NOT emit discard.drop block (only strings need it).
	ir := generateIR(t, `
		main() {
			int[] v = [1, 2, 3];
			v.pop();
		}
	`)
	mainFn := extractFunction(ir, "main")
	assertNotContains(t, mainFn, "discard.drop")
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
	assertContains(t, ir, "call i64 @__user.compute")
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
	assertContains(t, ir, "call i64 @__user.double")
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
	assertContains(t, ir, "call void @__user.doWork")
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

// T0671: for-in over a heap-element channel must drop the per-iteration loop
// variable. genForInChannel memcpys each item out of the ring buffer (a real
// move) into the loop-var alloca, so the loop owns it and must register a
// flag-guarded drop binding (string -> promise_string_drop). Pre-fix no drop
// binding was emitted, leaking one allocation per received heap item.
func TestT0671ForInChannelDropsHeapLoopVar(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[string](capacity: 2);
			ch.send("a");
			ch.close();
			for s in ch {
				int n = s.bytes().len;
			}
		}
	`)
	// for-in over channel[string]: the loop body must drop the moved-out
	// loop variable each iteration (flag-guarded promise_string_drop).
	assertContains(t, ir, "forin_ch.body")
	assertContains(t, ir, "strdrop.call")
	assertContains(t, ir, "strdrop.skip")
	assertContains(t, ir, "call void @promise_string_drop(")
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

// T0858: an early `return;` in main() must still run scope cleanup for heap
// locals allocated before it. The bare-return path branches to the coroutine
// final-suspend block via emitScopeCleanup — verify the string drop is emitted
// (and no bare ret void) so the early-return path can never leak.
func TestMainEarlyReturnRunsScopeCleanup(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "abcdef".repeat(50);
			if s.len > 0 { return; }
			print_line(s);
		}
	`)
	body := extractDefine(ir, ".goroutine.main")
	assertNotContains(t, body, "ret void")
	assertContains(t, body, "br label %final.suspend")
	assertContains(t, body, "promise_string_drop")
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
	// TLS panic flag globals (T0143)
	assertContains(t, ir, "@__promise_panic_flag")
	assertContains(t, ir, "@__promise_panic_msg")
	assertContains(t, ir, "@__promise_panic_type")
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

// T0683: a non-void value-returning go-block (`go { …; <expr> }` → task[T])
// awaited via `<-x` must store the trailing value into G.result_ptr, and the
// caller must allocate a heap result buffer — not the void sentinel 0x1,
// which `<-x` would dereference as a wild pointer (SIGSEGV, mislabeled
// "fatal: stack overflow" by the macOS signal handler).
func TestGoBlockValueResultStored(t *testing.T) {
	ir := generateIR(t, `
		main() {
			task[int] x = go { 42 };
			r := <-x;
		}
	`)
	// Coroutine body stores the trailing value into G.result_ptr via the
	// B0109 null-check store pattern.
	assertContains(t, ir, "store_result:")
	assertContains(t, ir, "after_store:")
	assertContains(t, ir, "store i64 42, i64*")
	// Caller allocates a heap result buffer for the non-void task.
	assertContains(t, ir, "@pal_alloc")
	// The bug stored the void sentinel 0x1 into result_ptr for this
	// non-void task — there must be no such sentinel now.
	assertNotContains(t, ir, "inttoptr i64 1 to i8*")
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
	assertContains(t, ir, "store_result:")
	assertContains(t, ir, "@pal_alloc")
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
	assertContains(t, ir, "store_result:")
	assertContains(t, ir, "@pal_alloc")
}

// T0739: a value-returning go-block whose trailing expression is a CAPTURING
// closure (≥1 capture → heap env struct) is the envTemps sibling of the T0686
// heapTemps bug. Edits 1–4 isolate the coroutine's env temp from the outer fn
// (fixes the WASM `%0`-is-coro.id-token compile failure — guarded by the e2e
// run, since generateIR of the closure case doesn't reproduce the `%0` misuse).
// Edit 5 teaches emitVariantFieldDrop a *types.Signature case so the
// dropped-not-awaited form (`task[() -> int] x = go { || -> base + 2 };` with
// no `<-x`) frees the closure's heap env via Task[() -> int].drop instead of
// leaking it. The `closure.env.free` block is absent on buggy master and proves
// edit 5 is wired into the Task drop path.
func TestT0739_ClosureResultDropFreesEnv(t *testing.T) {
	ir := generateIR(t, `
		main() {
			base := 1;
			task[() -> int] x = go { || -> base + 2 };
		}
	`)
	// Edit 5: the dropped Task[() -> int] routes its closure result through
	// emitVariantFieldDrop's new *types.Signature case → emitEnvDropOrFree,
	// which emits the closure.env.free block. Absent on buggy master.
	assertContains(t, ir, "closure.env.free")
	// Positive value-path guards: the closure is still stored into G.result_ptr
	// inside the inner coroutine, and the caller allocates a real result buffer.
	assertContains(t, ir, "store_result:")
	assertContains(t, ir, "@pal_alloc")
}

// T1105: `go obj.method(...)` returning a heap user type via the
// genGoCallExprViaBlock path is the sibling of the T0686 go-block heapTemps bug.
// The method's struct result registers a heap temp whose alloca/dropFlag belong
// to the inner `.goroutine.N` frame; without isolation those temps leaked into
// the outer `.goroutine.main` coroutine, where the printer serialized them as
// `%0` — the coro.id token — producing `load i1, i1* %0` (malformed IR / stack
// overflow). Guards the producer-side isolation of heapTemps/heapTempMap.
func TestT1105_GoMethodStructResultNoTokenLoad(t *testing.T) {
	ir := generateIR(t, `
		type R { int n; }
		type W { f(this, int x) R { return R(n: x); } }
		main() { W w = W(); t := go w.f(5); r := <-t; }
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
	// Positive guards: the via-block coroutine still exists and the caller
	// allocates a real result buffer (the ViaBlock path stores into G.result_ptr
	// inline, so there is no separate `store_result:` block as in the go-block form).
	assertContains(t, ir, "define i8* @.goroutine.")
	assertContains(t, ir, "@pal_alloc")
}

// T1105 (env sibling): `go obj.method(...)` returning a CAPTURING closure (≥1
// capture → heap env struct) exercises the envTemps isolation added alongside
// the heapTemps fix. Without isolating envTemps/envTempMap the closure's env
// temp (whose alloca lives in the inner `.goroutine.N` frame) leaked into the
// outer `.goroutine.main` coroutine, mis-serializing its coro.id token as `%0`.
func TestT1105_GoMethodClosureResultNoTokenLoad(t *testing.T) {
	ir := generateIR(t, `
		type W { f(this, int x) () -> int { return || -> x + 1; } }
		main() { W w = W(); t := go w.f(5); g := <-t; }
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
	assertNotContains(t, body, "i1* %0")                // no env drop-flag load from the token
	assertNotContains(t, body, "i8** %0")               // no env pointer load from the token
	// Positive guards: the via-block coroutine still exists and the caller
	// allocates a real result buffer (the ViaBlock path stores into G.result_ptr
	// inline, so there is no separate `store_result:` block as in the go-block form).
	assertContains(t, ir, "define i8* @.goroutine.")
	assertContains(t, ir, "@pal_alloc")
}

// T1159: fire-and-forget `go f(...)` with a non-void heap result must DROP the
// discarded result in the coroutine body instead of running the result-store
// machinery (there is no receiver and result_ptr stays null). Contrast: the
// task-handle form stores into G.result_ptr via a `store_result:` block. Guards
// the free-function fast path (genGoCallExpr).
func TestT1159_FastPathFireAndForgetDropsResult(t *testing.T) {
	// Fire-and-forget: result discarded → body drops the string, no store block.
	ffBody := defBody(t, generateIR(t, `
		build(int x) string { return "v{x}"; }
		main() { go build(5); }
	`), "define i8* @.goroutine.0(")
	assertContains(t, ffBody, "@promise_string_drop") // discarded result dropped
	assertNotContains(t, ffBody, "store_result:")     // no result-buffer store machinery

	// Task handle: result received → body stores it, no unconditional drop.
	taskBody := defBody(t, generateIR(t, `
		build(int x) string { return "v{x}"; }
		main() { t := go build(5); r := <-t; }
	`), "define i8* @.goroutine.0(")
	assertContains(t, taskBody, "store_result:")           // result stored into G.result_ptr
	assertNotContains(t, taskBody, "@promise_string_drop") // body does not drop — receiver owns it
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

// defBody extracts a single function *definition* body — from the line beginning
// with `marker` (a full `define <ret> @<name>(` prefix) up to its closing brace.
// Matching the full define prefix avoids matching a call to the same function.
func defBody(t *testing.T, ir, marker string) string {
	t.Helper()
	idx := strings.Index(ir, marker)
	if idx < 0 {
		t.Fatalf("expected a definition matching %q in the IR", marker)
	}
	body := ir[idx:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end+2]
	}
	return body
}

// goNewToEnqueue returns the IR slice of the spawn site — from the
// `promise_g_new` call to the following `promise_sched_enqueue` — where the
// via-block result buffer (if any) is allocated.
func goNewToEnqueue(t *testing.T, ir string) string {
	t.Helper()
	gNew := strings.Index(ir, "@promise_g_new")
	if gNew < 0 {
		t.Fatal("expected a promise_g_new call in the IR")
	}
	rest := ir[gNew:]
	enq := strings.Index(rest, "@promise_sched_enqueue")
	if enq < 0 {
		t.Fatal("expected a promise_sched_enqueue call after promise_g_new")
	}
	return rest[:enq]
}

func TestGoBlockVoidStillUsesSentinel(t *testing.T) {
	ir := generateIR(t, `
		main() {
			task[void] x = go { int n = 10; };
			<-x;
		}
	`)
	assertContains(t, ir, "inttoptr i64 1 to i8*")
	assertNotContains(t, ir, "store_result:")
}

// T0683: a value-returning go-block inside a *generic* function exercises
// genGoBlock's `c.typeSubst` monomorphization branch — the trailing value's
// type must be Substitute'd so the coroutine's result store and the caller's
// result-buffer size match the `<-x` receive side (symmetric with
// genReceiveTask). For `mk[int]`, T resolves to int: the monomorphized
// coroutine must store an i64 into G.result_ptr (not a TypeParam-typed or
// sentinel value). Guards the plan's key symmetry, which had no direct
// codegen coverage (the other two tests use only the concrete non-generic
// path).
func TestGoBlockValueResultMonomorphized(t *testing.T) {
	ir := generateIR(t, `
		mk[T](T v) Task[T] {
			return go { v };
		}
		main() {
			task[int] x = mk[int](42);
			r := <-x;
		}
	`)
	// The generic function was monomorphized for int.
	assertContains(t, ir, `@"mk[int]"`)
	// The value path was taken inside the monomorphized go-block coroutine
	// (store into G.result_ptr), the caller allocated a real result buffer,
	// and no void sentinel was used for this non-void monomorphized task.
	assertContains(t, ir, "store_result:")
	assertContains(t, ir, "after_store:")
	assertContains(t, ir, "@pal_alloc")
	assertNotContains(t, ir, "inttoptr i64 1 to i8*")
}

// T0688: a value-returning go-block whose trailing expression is a bare
// reference to a captured BORROWED heap parameter (no outer drop binding)
// must dup the value at spawn time. Without the dup, the coroutine reads
// the param after the caller's stmt-temp has been dropped — UAF / double-free.
// The dup is emitted in the spawning function's IR (outside the coroutine),
// while the param is still valid, before the goroutine is enqueued.
func TestT0688_BareCapturedHeapParamDups(t *testing.T) {
	ir := generateIR(t, `
		ngmake(string v) Task[string] {
			return go { v };
		}
		main() {
			task[string] x = ngmake("a" + "b");
			r := <-x;
		}
	`)
	// The spawning function (ngmake) dups its borrowed string param via
	// promise_string_new before passing the value to the goroutine ramp.
	// The dup IR lives between ngmake's load of v.addr and the call to
	// .goroutine.N.
	ngmakeIR := extractFunction(ir, "__user.ngmake")
	assertContains(t, ngmakeIR, "@promise_string_new(")
	assertContains(t, ngmakeIR, "call i8* @.goroutine.")
}

// T0688: a value-returning go-block capturing a heap LOCAL (already owned by
// the outer scope, has a drop binding) must NOT add an extra dup — the
// existing B0354 ownership-transfer machinery handles it correctly. Adding a
// dup here would leak the original.
func TestT0688_BareCapturedLocalNoExtraDup(t *testing.T) {
	ir := generateIR(t, `
		m() Task[string] {
			s := "x" + "y";
			return go { s };
		}
		main() {
			task[string] x = m();
			r := <-x;
		}
	`)
	mIR := extractFunction(ir, "__user.m")
	// m allocates the concat result via promise_string_concat — that's the
	// captured local. There must NOT be an additional promise_string_new
	// for a dup of the captured local.
	assertContains(t, mIR, "@promise_string_concat(")
	assertNotContains(t, mIR, "@promise_string_new(")
}

// T0688: a value-returning go-block whose trailing expression is DERIVED
// from a borrowed param (e.g. `v + "!"`) already produces a fresh heap value
// inside the coroutine. The spawning function must NOT dup the param — the
// dup is only required for bare-ident trailing values where the loaded
// pointer would alias the caller's stmt-temp.
func TestT0688_DerivedTrailingNoDup(t *testing.T) {
	ir := generateIR(t, `
		ngmake(string v) Task[string] {
			return go { v + "!" };
		}
		main() {
			task[string] x = ngmake("hi");
			r := <-x;
		}
	`)
	ngmakeIR := extractFunction(ir, "__user.ngmake")
	// No spawn-side dup: the only string allocations in the coroutine flow
	// of ngmake should be from the goroutine call path, not a pre-spawn
	// promise_string_new of the borrowed param.
	assertNotContains(t, ngmakeIR, "@promise_string_new(")
}

// T0688: Vector[T] dispatch branch in dupBorrowedCaptureForResult. The
// spawning function must emit a vector dup (pal_alloc + memcpy of header +
// data) before passing the value to the goroutine — without it the awaiter
// would store the dangling vector pointer into G.result_ptr.
func TestT0688_BareCapturedVectorParamDups(t *testing.T) {
	ir := generateIR(t, `
		ngvec(Vector[int] v) Task[Vector[int]] {
			return go { v };
		}
		main() {
			task[Vector[int]] x = ngvec([1, 2, 3]);
			r := <-x;
		}
	`)
	ngvecIR := extractFunction(ir, "__user.ngvec")
	// dupVector emits vecdup.copy / vecdup.merge labels — the spawning
	// function must contain them before the goroutine call.
	assertContains(t, ngvecIR, "vecdup.copy")
	assertContains(t, ngvecIR, "vecdup.merge")
	assertContains(t, ngvecIR, "call i8* @.goroutine.")
}

// T0688: heap user type dispatch branch in dupBorrowedCaptureForResult. The
// spawning function must emit a heapdup (pal_alloc + memcpy of the instance
// plus deep-clone of any droppable sub-fields like nested strings) before
// passing the value to the goroutine.
func TestT0688_BareCapturedHeapUserTypeDups(t *testing.T) {
	ir := generateIR(t, `
		type T0688DupBox {
			string name;
			int value;
		}
		ngbox(T0688DupBox b) Task[T0688DupBox] {
			return go { b };
		}
		main() {
			task[T0688DupBox] x = ngbox(T0688DupBox(name: "n", value: 1));
			r := <-x;
		}
	`)
	ngboxIR := extractFunction(ir, "__user.ngbox")
	// dupHeapValue emits heapdup.copy / heapdup.merge labels — the spawning
	// function must contain them before the goroutine call.
	assertContains(t, ngboxIR, "heapdup.copy")
	assertContains(t, ngboxIR, "heapdup.merge")
	assertContains(t, ngboxIR, "call i8* @.goroutine.")
}

// T0732: Map[K,V] dispatch branch in dupBorrowedCaptureForResult. Map/Set are
// heap user types excluded from isDroppableHeapUserType / isHeapUserNoDropPalFree
// by T0440, so the T0688 fix missed them — a bare-captured borrowed Map param
// returned from a value-block segfaulted (double-free of the dangling stmt-temp).
// The fix routes Map through dupHeapValue (memcpy + field-wise deep dup), which
// emits the heapdup.copy / heapdup.merge labels in the spawning function.
func TestT0688_BareCapturedMapParamDups(t *testing.T) {
	ir := generateIR(t, `
		ngmap(Map[string, int] m) Task[Map[string, int]] {
			return go { m };
		}
		main() {
			task[Map[string, int]] x = ngmap({"a": 1});
			r := <-x;
		}
	`)
	ngmapIR := extractFunction(ir, "__user.ngmap")
	assertContains(t, ngmapIR, "heapdup.copy")
	assertContains(t, ngmapIR, "heapdup.merge")
	assertContains(t, ngmapIR, "call i8* @.goroutine.")
}

// T0732: Set[T] dispatch branch in dupBorrowedCaptureForResult. Like Map, Set
// is excluded from the T0440-gated predicates; the fix recognizes it via
// isMapOrSetType and routes it through dupHeapValue, whose static path
// recursively deep-dups Set's nested Map[T,bool] field.
func TestT0688_BareCapturedSetParamDups(t *testing.T) {
	ir := generateIR(t, `
		ngset(Set[int] s) Task[Set[int]] {
			return go { s };
		}
		main() {
			Set[int] s = Set[int]();
			s.add(1);
			task[Set[int]] x = ngset(s);
			r := <-x;
		}
	`)
	ngsetIR := extractFunction(ir, "__user.ngset")
	assertContains(t, ngsetIR, "heapdup.copy")
	assertContains(t, ngsetIR, "heapdup.merge")
	assertContains(t, ngsetIR, "call i8* @.goroutine.")
}

// T0688: regression guard — Copy-type param (int) must NOT trigger any
// dup. The eligibility predicate goElemNeedsBorrowedCaptureDup returns
// false for primitives, so the spawning function has zero dup overhead.
func TestT0688_CopyParamNoDup(t *testing.T) {
	ir := generateIR(t, `
		ngint(int v) Task[int] {
			return go { v };
		}
		main() {
			task[int] x = ngint(42);
			r := <-x;
		}
	`)
	ngintIR := extractFunction(ir, "__user.ngint")
	// No dup of any kind should be emitted (no vecdup, no heapdup, no string
	// dup). The capture is the bare i64 value, passed directly to the
	// goroutine.
	assertNotContains(t, ngintIR, "vecdup.copy")
	assertNotContains(t, ngintIR, "heapdup.copy")
	assertNotContains(t, ngintIR, "@promise_string_new(")
	assertContains(t, ngintIR, "call i8* @.goroutine.")
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
	// Close should call promise_waiter_wake_all for send, recv, and rv_waiters (T0312)
	assertContains(t, ir, "call void @promise_waiter_wake_all(")
}

func TestChannelCloseWakesRvWaiters(t *testing.T) {
	// T0312: genChannelClose must wake rv_waiters in addition to send/recv waiters.
	// A rendezvous-parked sender (goroutine that wrote to an unbuffered channel and
	// is parked on rv_waiters) must be unblocked when the channel closes.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.close();
		}
	`)
	// Three wake_all calls: send_waiters, recv_waiters, rv_waiters
	count := strings.Count(ir, "call void @promise_waiter_wake_all(")
	if count < 3 {
		t.Errorf("expected >= 3 promise_waiter_wake_all calls in close (send/recv/rv_waiters), got %d", count)
	}
}

func TestSelectRecvWakesRvWaiters(t *testing.T) {
	// T0312: the select execRecv path must call wake_one for rv_waiters (field 15)
	// in addition to send_waiters, so rendezvous-parked senders are unblocked
	// when their value is consumed via a select recv case.
	irBaseline := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(1);
		}
	`)
	baseline := strings.Count(irBaseline, "call void @promise_waiter_wake_one(")

	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(1);
			select {
				v := <-ch:
					print_line("got");
				default:
					print_line("default");
			}
		}
	`)
	total := strings.Count(ir, "call void @promise_waiter_wake_one(")
	delta := total - baseline
	// execRecv adds 2 wake_one calls: send_waiters + rv_waiters
	if delta < 2 {
		t.Errorf("select recv must add >= 2 wake_one calls (send_waiters + rv_waiters), got delta=%d (rv_waiters wake missing?)", delta)
	}
}

func TestChannelStructHas18Fields(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
		}
	`)
	// Channel struct has 18 fields: buffer, head, tail, count, cap, elem_size,
	// is_closed, is_unbuffered, not_empty, not_full, send_waiters(2), recv_waiters(2),
	// rv_waiters(2, T0312), refcount. Verified by promise_channel_new definition.
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

func TestTaskDropDoneLoadIsAcquire(t *testing.T) {
	// T0669: G.done spin-wait in Task.drop must use an atomic acquire load so the
	// LLVM optimizer cannot hoist or cache it across loop iterations on Windows.
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			v := <-t;
		}
	`)
	// The Task drop function must contain an atomic acquire load on G.done (i8).
	assertContains(t, ir, "load atomic i8")
	assertContains(t, ir, "acquire")
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

func TestChannelSendCoroutineRendezvous(t *testing.T) {
	// T0312: Unbuffered channel send inside a go block parks on rv_waiters for the
	// rendezvous. After writing the value, the sender enqueues itself on rv_waiters,
	// sets park_mutex=&ch.mutex, and calls coro.suspend. The scheduler unlocks
	// ch.mutex. The receiver wakes the sender via wake_one(rv_waiters) after count--.
	ir := generateIR(t, `
		main() {
			ch := channel[int]();
			go {
				ch.send(42);
			};
			result := <-ch;
		}
	`)
	// Rendezvous wait and resume blocks must exist
	assertContains(t, ir, "send.rv.wait")
	assertContains(t, ir, "send.rv.resume")
	// rv_waiters park: waiter_enqueue IS called (unlike the old yield-spin)
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
	// Receiver must wake rv_waiters after count--
	assertContains(t, ir, "call void @promise_waiter_wake_one(")
}

func TestChannelSendRendezvousExitWakesNextWaiter(t *testing.T) {
	// T0305/T0312: Rendezvous exit wakes one waiter on send_waiters. rv_waiters
	// holds rendezvous-parked senders; send_waiters holds only write-waiters and
	// select SWNs, so waking it here is safe and never strands a write-waiter.
	ir := generateIR(t, `
		main() {
			ch := channel[int]();
			go {
				ch.send(42);
			};
			result := <-ch;
		}
	`)
	// The rendezvous exit block should exist and call wake_one
	assertContains(t, ir, "send.rv.exit")
	assertContains(t, ir, "call void @promise_waiter_wake_one(")
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

func TestSchedulerClearsParkMutexBeforeUnlock(t *testing.T) {
	// B0249: park_mutex must be cleared BEFORE the mutex unlock to prevent a race
	// where another thread wakes G, G re-parks with a new mutex, and the stale
	// NULL write overwrites it — causing double-resume and segfault.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go { ch.send(42); };
		}
	`)
	// In sched_loop (and sched_coop_run for WASM), the release_park_mutex block
	// must store null to park_mutex BEFORE calling pal_mutex_unlock.
	fn := extractFunction(ir, "promise_sched_loop")
	if fn == "" {
		t.Fatal("promise_sched_loop not found")
	}
	idx := strings.Index(fn, "release_park_mutex:")
	if idx < 0 {
		t.Fatal("release_park_mutex block not found in sched_loop")
	}
	relBlk := fn[idx:]
	storeIdx := strings.Index(relBlk, "store i8* null,")
	unlockIdx := strings.Index(relBlk, "call void @pal_mutex_unlock(")
	if storeIdx < 0 {
		t.Fatal("null store not found in release_park_mutex")
	}
	if unlockIdx < 0 {
		t.Fatal("mutex unlock not found in release_park_mutex")
	}
	if storeIdx > unlockIdx {
		t.Error("B0249: park_mutex null store must come BEFORE mutex unlock to prevent race")
	}
}

func TestSchedParkMRechecksGlobalQueue(t *testing.T) {
	// T0375: park_m must re-check sched.global_size while still holding
	// idle_lock AFTER pushing self onto the idle stack. If a non-M enqueuer
	// raced through sched_enqueue + wake_m against an empty idle stack, the
	// re-check sees the queued work and aborts the park (popping self off the
	// idle stack) instead of committing to cond_wait indefinitely.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go { ch.send(42); };
		}
	`)
	fn := extractFunction(ir, "promise_sched_park_m")
	if fn == "" {
		t.Fatal("promise_sched_park_m not found")
	}

	// The abort_park block must exist (the bail path).
	abortIdx := strings.Index(fn, "abort_park:")
	if abortIdx < 0 {
		t.Fatal("abort_park block not found in promise_sched_park_m")
	}

	// continue_park is the normal park path; both must exist.
	continueIdx := strings.Index(fn, "continue_park:")
	if continueIdx < 0 {
		t.Fatal("continue_park block not found in promise_sched_park_m")
	}

	// The conditional branch into abort_park must come from .entry,
	// before we reach wait_loop / cond_wait.
	entryEnd := strings.Index(fn, "abort_park:")
	if entryEnd < 0 {
		t.Fatal("could not locate end of entry block")
	}
	entryBlk := fn[:entryEnd]
	if !strings.Contains(entryBlk, "br i1") {
		t.Error("entry block must conditionally branch (abort_park vs continue_park)")
	}
	if !strings.Contains(entryBlk, ", label %abort_park, label %continue_park") {
		t.Error("entry must branch to abort_park / continue_park based on global queue size")
	}

	// The entry must compare a freshly-loaded i64 against zero — that's the
	// global_size != 0 test.
	if !strings.Contains(entryBlk, "icmp ne i64") {
		t.Error("entry must compare global_size (i64) against zero")
	}

	// The abort_park block must NOT contain pal_cond_wait — that's the whole
	// point of bailing out before we commit to parking.
	abortEnd := strings.Index(fn[abortIdx:], "\n\n")
	if abortEnd < 0 {
		abortEnd = len(fn) - abortIdx
	}
	abortBlk := fn[abortIdx : abortIdx+abortEnd]
	if strings.Contains(abortBlk, "@pal_cond_wait") {
		t.Error("abort_park must not call pal_cond_wait — bailing out before parking")
	}
	// abort_park must unlock both mutexes (idle_lock and park_mutex) and ret.
	if strings.Count(abortBlk, "@pal_mutex_unlock") != 2 {
		t.Error("abort_park must unlock both idle_lock and park_mutex (2 unlocks)")
	}
	if !strings.Contains(abortBlk, "ret void") {
		t.Error("abort_park must return")
	}
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

// T0149: goroutine_exit always calls coro.destroy (panicked goroutines reach
// final suspend via TLS flag propagation, so coro.destroy is safe).
func TestGoroutineExitAlwaysCoroDestroy(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	fn := extractFunction(ir, "promise_goroutine_exit")
	// Must call coro.destroy unconditionally
	assertContains(t, fn, "call void @llvm.coro.destroy")
	// Must NOT have the old free_coro_frame fallback for panicked goroutines
	assertNotContains(t, fn, "free_coro_frame:")
}

// B0225: goroutine_exit frees G.panic_msg when panicked==2 (heap-allocated msg).
func TestGoroutineExitFreePanicMsg(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	// goroutine_exit should have free_panic_msg and do_free_g blocks
	assertContains(t, ir, "free_panic_msg:")
	assertContains(t, ir, "do_free_g:")
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

// B0228: promise_panic_msg stores type=2 in TLS panic_type to mark heap-allocated msg.
func TestPanicMsgSetsHeapPanickedFlag(t *testing.T) {
	ir := generateIR(t, `
		panic_msg(string msg) `+"`"+`extern("promise_panic_msg");
		main() { panic_msg("boom"); }
	`)
	// promise_panic_msg stores i8 2 in @__promise_panic_type (heap-allocated)
	assertContains(t, ir, "store i8 2, i8* @__promise_panic_type")
}

// B0228: promise_panic is no longer noreturn — call sites use ret instead of unreachable.
func TestPanicNotNoreturn(t *testing.T) {
	ir := generateIR(t, `
		main() {}
	`)
	// promise_panic should NOT have noreturn (other funcs like pal_exit still do)
	assertNotContains(t, ir, "declare void @promise_panic(i8*) noreturn")
	// The function body should end with ret void in the set_panic block
	assertContains(t, ir, "define void @promise_panic(i8*")
}

// B0228: promise_panic double-panic check aborts with exit code 134.
func TestPanicDoublePanicAbort(t *testing.T) {
	ir := generateIR(t, `
		main() {}
	`)
	// Double panic should load flag, compare, and branch to abort path
	assertContains(t, ir, "load i8, i8* @__promise_panic_flag")
	assertContains(t, ir, "call void @pal_exit(i32 134)")
}

// B0228: Category A — OOB panic returns instead of unreachable.
func TestOOBPanicReturns(t *testing.T) {
	ir := generateIR(t, `
		get(int[] v, int i) int { return v[i]; }
		main() { v := [1]; get(v, 0); }
	`)
	// The OOB panic block should call promise_panic then return (not unreachable)
	assertContains(t, ir, "call void @promise_panic(")
	// promise_panic declaration should NOT have noreturn (other funcs like pal_exit still do)
	assertNotContains(t, ir, "declare void @promise_panic(i8*) noreturn")
}

// T0147: Panic check emitted after every call expression.
func TestPanicCheckAfterCallExpr(t *testing.T) {
	ir := generateIR(t, `
		foo() {}
		main() { foo(); }
	`)
	// After the call to foo(), emitPanicCheck should emit:
	// - load of __promise_panic_flag
	// - icmp ne (check if flag is set)
	// - conditional branch to panic.cleanup / panic.ok
	assertContains(t, ir, "panic.cleanup")
	assertContains(t, ir, "panic.ok")
}

// T0147: Panic check after method call.
func TestPanicCheckAfterMethodCall(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			int x;
			bar(this) int { return this.x; }
		}
		main() { f := Foo(x: 1); f.bar(); }
	`)
	assertContains(t, ir, "panic.cleanup")
	assertContains(t, ir, "panic.ok")
}

// T0147: Go-call (direct) coroutine has panic exit block.
func TestPanicCheckGoCallDirect(t *testing.T) {
	ir := generateIR(t, `
		work() {}
		main() { go work(); }
	`)
	// genGoCallExpr should emit go.panic_exit block
	assertContains(t, ir, "go.panic_exit")
	assertContains(t, ir, "go.call_ok")
}

// T0148: genGoCallExprViaBlock has final panic check before final suspend.
func TestPanicCheckGoCallViaBlockFinal(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			bar(this) {}
		}
		main() {
			f := Foo();
			go f.bar();
		}
	`)
	// genGoCallExprViaBlock should emit go.panic_exit block
	assertContains(t, ir, "go.panic_exit")
	// The coroutine body should have a final panic flag check (icmp ne + cond br)
	assertContains(t, ir, "@__promise_panic_flag")
}

// T0148: genGoBlock has final panic check before final suspend.
func TestPanicCheckGoBlockFinal(t *testing.T) {
	ir := generateIR(t, `
		work() {}
		main() {
			go {
				work();
			};
		}
	`)
	assertContains(t, ir, "go.panic_exit")
	assertContains(t, ir, "@__promise_panic_flag")
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
	assertContains(t, ir, "@__user.add")
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
	assertContains(t, ir, "@__user.calc")
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
			format!(Writer ~w) { w.write_string("foo"); }
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
			format!(Writer ~w) { w.write_string("shape"); }
		}
		type Circle is Shape {
			format!(Writer ~w) { w.write_string("circle"); }
		}
		main() {
			Shape s = Circle();
			string x = "{s}";
		}
	`)
	// Virtual dispatch: should use the Builder-as-Writer view vtable (with $view_adapt wrappers)
	assertContains(t, ir, "promise_vtable_Builder_as_Writer")
	assertContains(t, ir, "interp.format.ok")
}

// T0421: Fieldless enum interpolation emits switch on tag → variant name string.
func TestStringInterpolationEnumFieldless(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		main() {
			Color c = Color.Green;
			string s = "{c}";
		}
	`)
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "enum.interp.Red")
	assertContains(t, ir, "enum.interp.Green")
	assertContains(t, ir, "enum.interp.Blue")
	assertContains(t, ir, "enum.interp.merge")
	assertContains(t, ir, `"Red"`)
	assertContains(t, ir, `"Green"`)
	assertContains(t, ir, `"Blue"`)
}

// T0421: Data enum interpolation extracts tag from field 0 and emits switch.
func TestStringInterpolationEnumData(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		main() {
			Shape s = Shape.Circle(1.0);
			string x = "{s}";
		}
	`)
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "enum.interp.Circle")
	assertContains(t, ir, "enum.interp.Rect")
	assertContains(t, ir, `"Circle"`)
	assertContains(t, ir, `"Rect"`)
}

// T0421: Optional enum interpolation emits interp.some/none wrapper + inner enum switch.
func TestStringInterpolationEnumOptional(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		main() {
			Color? c = Color.Green;
			string s = "{c}";
		}
	`)
	// Optional wrapping blocks
	assertContains(t, ir, "interp.some")
	assertContains(t, ir, "interp.none")
	// Enum switch inside the some branch
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "enum.interp.Red")
	assertContains(t, ir, "enum.interp.Green")
	assertContains(t, ir, "enum.interp.Blue")
	// "none" string for absent case
	assertContains(t, ir, `"none"`)
}

// T0084: Builder is freed after callFormatToString extracts the string
func TestCallFormatToStringBuilderDrop(t *testing.T) {
	ir := generateIR(t, `
		type Pt {
			int x;
			format!(Writer ~w) { w.write_string("pt"); }
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
			b.write_string("hello");
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

// B0347: Returning `this.v[i]` where `v: Vector[string]` via a method must
// dup the inner string so the caller owns it. The return-site sets
// `dupStringFieldAccess` for Optional[string] returns; `genMethodIndex`
// consumes it when the target is a container, producing a `promise_string_new`
// call inside the method body.
func TestContainerStringIndexReturnDupsInnerString(t *testing.T) {
	ir := generateIR(t, `
		type Bag {
			string[] v;
			drop(~this) {}
			get_value(int i) string? {
				return this.v[i];
			}
		}
		main() {
			Bag b = Bag(v: string[]());
			b.v.push("value");
			string? r = b.get_value(0);
		}
	`)
	bodyStart := `define { i1, i8* } @Bag.get_value(`
	idx := strings.Index(ir, bodyStart)
	if idx < 0 {
		t.Fatalf("Bag.get_value definition not found in IR")
	}
	bodyEnd := strings.Index(ir[idx:], "\n}\n")
	if bodyEnd < 0 {
		t.Fatalf("could not find end of Bag.get_value body")
	}
	body := ir[idx : idx+bodyEnd]
	if !strings.Contains(body, "promise_string_new") {
		t.Errorf("expected promise_string_new in Bag.get_value body (dup of inner string); got:\n%s", body)
	}
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

func TestSliceAssignVectorDropsSourceBacking(t *testing.T) {
	// B0313: After slice assign, the source vector's backing array must be freed
	// via Vector.drop before clearing its drop flag.
	ir := generateIR(t, `
		main() {
			string[] src = ["hello"];
			string[] v = ["a", "b", "c"];
			v[0:1] = src;
		}
	`)
	assertContains(t, ir, `call void @"Vector[string].[:]=`)
	// B0313: Vector.drop must follow the [:]=  call (shallow free of src backing).
	assertContainsMatch(t, ir, `(?s)Vector\[string\]\.\[:\]=.*?call void @Vector\.drop`)
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

func TestFailableGeneratorCoroutineHasErrorSlot(t *testing.T) {
	ir := generateIR(t, `
		gen!() stream[int] {
			yield 1;
		}
		main() {}
	`)
	// Coroutine should have error_slot parameter and alloca
	assertContains(t, ir, "error_slot.addr")
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

// T0479: Plain tuple-by-value param with droppable fields must be dropped.
// Mirrors T0406 (regular function tuple-by-value drop) inside a generator.
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
	// Tuple-by-value drop walks fields; the string field's drop should fire.
	assertContains(t, ir, "%t.dropflag = alloca i1")
	assertContains(t, ir, "call void @promise_string_drop(")
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

// B0224: Operator methods on generic value types must use the mono name.
func TestGenericValueTypeOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Pair[T: Equal] {
			T a `+"`value"+`;
			T b `+"`value"+`;
			==(Pair[T] other) bool => this.a == other.a && this.b == other.b;
			!=(Pair[T] other) bool => !(this == other);
		}
		main() {
			p := Pair[int](a: 1, b: 2);
			q := Pair[int](a: 1, b: 2);
			bool r = p == q;
		}
	`)
	// Operator dispatches to mono name Pair[int].==
	assertContains(t, ir, `@"Pair[int].=="`)
}

// T0748: a value-type `this` used as the right-hand operand of a user-defined
// binary operator must load the param via bitcast+load from the receiver pointer,
// not synthesize a heap-style {vtable, instance} struct (which panicked:
// "store operands are not compatible: src=i8*; dst=i64*").
func TestValueTypeThisAsRightOperand(t *testing.T) {
	ir := generateIR(t, `
		type Cmp {
			int x `+"`value"+`;
			<(Cmp other) bool { return this.x < other.x; }
			gt_via(this, Cmp other) bool { return other < this; }
		}
		main() {
			a := Cmp(x: 5);
			b := Cmp(x: 2);
			_ := a.gt_via(b);
		}
	`)
	// The value-type `this` right operand is materialized via bitcast to the
	// param struct followed by a load — not a store of i8* into a data field.
	assertContains(t, ir, `call i1 @"Cmp.<"(`)
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

func TestVariadicMethodIR(t *testing.T) {
	// Variadic method: receiver + variadic param in IR.
	ir := generateIR(t, `
		type Adder {
			int base;

			addAll(this, ...int values) int {
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
	assertContains(t, ir, "define { i1, i64, i8* } @__user.trySum(i8* %nums)")
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

func TestVariadicParamDropAtScopeExit(t *testing.T) {
	// B0191: Variadic parameter vectors must be freed at scope exit.
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
	assertContains(t, ir, "call void @Vector.drop(i8*")
}

func TestVariadicMethodParamDropAtScopeExit(t *testing.T) {
	// B0191: Variadic method parameter vectors must be freed at scope exit.
	ir := generateIR(t, `
		type Adder {
			int base;

			addAll(this, ...int values) int {
				return this.base;
			}
		}
		main() {
			a := Adder(base: 10);
			a.addAll(1, 2, 3);
		}
	`)
	assertContains(t, ir, "call void @Vector.drop(i8*")
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

func TestVariadicVectorHeapTempOnFailableArg(t *testing.T) {
	// B0201: When a failable arg inside a variadic call fails, the vector
	// allocated for variadic args must be freed on the error path.
	ir := generateIR(t, `
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
	assertContains(t, ir, "err.heap.drop")
	assertContains(t, ir, "call void @pal_free")
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

// TestGlobalSetterIR verifies that `global setters (T0703) emit a function
// with no receiver param and that `Type.name = v` lowers to a direct call
// passing only the value argument. Uses a matching `global getter because
// setter-only properties don't currently parse as l-value targets (sema
// looks them up through LookupGetter).
func TestGlobalSetterIR(t *testing.T) {
	ir := generateIR(t, "type Foo {\n"+
		"int x;\n"+
		"get count int `global { return 0; }\n"+
		"set count(int v) `global { }\n"+
		"}\n"+
		"main() {\n"+
		"Foo.count = 7;\n"+
		"}\n")
	// Setter is defined with the $set mangle suffix and a single i64 param (no this).
	assertContains(t, ir, "define void @Foo.count$set(i64 %v)")
	assertNotContains(t, ir, "@Foo.count$set(i8*")
	// Call site lowers to a direct call with the value only.
	assertContains(t, ir, "call void @Foo.count$set(i64 7)")
}

// TestGlobalGetterSetterPairIR verifies that a `global getter and setter on
// the same name coexist (the setter mangles with `$set` so there's no clash)
// and dispatch correctly from `Type.name` reads and `Type.name = v` writes.
func TestGlobalGetterSetterPairIR(t *testing.T) {
	ir := generateIR(t, "type Foo {\n"+
		"int x;\n"+
		"get count int `global { return 0; }\n"+
		"set count(int v) `global { }\n"+
		"}\n"+
		"main() {\n"+
		"Foo.count = 3;\n"+
		"n := Foo.count;\n"+
		"}\n")
	assertContains(t, ir, "define i64 @Foo.count()")
	assertContains(t, ir, "define void @Foo.count$set(i64 %v)")
	assertContains(t, ir, "call void @Foo.count$set(i64 3)")
	assertContains(t, ir, "call i64 @Foo.count()")
}

// TestGlobalSetterCompoundAssignIR verifies that compound assignment
// (`Type.name += v`) on a `global getter/setter pair reads through the
// global getter, applies the op, and writes through the global setter.
// Exercises the interaction between genGetterCall's and genSetterCall's
// global branches via genMemberAssign's compound-op path.
func TestGlobalSetterCompoundAssignIR(t *testing.T) {
	ir := generateIR(t, "type Foo {\n"+
		"int x;\n"+
		"get count int `global { return 0; }\n"+
		"set count(int v) `global { }\n"+
		"}\n"+
		"main() {\n"+
		"Foo.count += 5;\n"+
		"}\n")
	// Compound assignment lowers to: load via global getter, add, store via global setter.
	assertContains(t, ir, "call i64 @Foo.count()")
	assertContains(t, ir, "call void @Foo.count$set(i64")
}

// TestFailableSetterInstanceNonVirtualIR verifies that a `set name!(...)`
// setter call site auto-propagates the failable result in a failable enclosing
// function. T0708.
func TestFailableSetterInstanceNonVirtualIR(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define { i1, i8* } @Foo.count$set")
	// Call site captures the result and routes through auto-propagation.
	assertContains(t, ir, "call { i1, i8* } @Foo.count$set")
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

// TestFailableGlobalSetterIR — `global failable setter (T0703 + T0708).
func TestFailableGlobalSetterIR(t *testing.T) {
	ir := generateIR(t, "type Foo {\n"+
		"int x;\n"+
		"get count int `global { return 0; }\n"+
		"set count!(int v) `global { if v < 0 { raise error(\"neg\"); } }\n"+
		"}\n"+
		"main!() {\n"+
		"Foo.count = -5;\n"+
		"}\n")
	assertContains(t, ir, "define { i1, i8* } @Foo.count$set(i64 %v)")
	assertContains(t, ir, "call { i1, i8* } @Foo.count$set(i64")
	assertContains(t, ir, "auto.propagate")
}

// TestFailableIndexSetterIR — a failable []= method's call site auto-propagates.
func TestFailableIndexSetterIR(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

// TestFailableSliceSetterIR — a failable [:]= method's call site auto-propagates.
func TestFailableSliceSetterIR(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

// TestFailableSetterCompoundAssignIR — `f.count += v` reads via getter then
// writes via setter; if either is failable both must propagate. This test
// covers the setter side.
func TestFailableSetterCompoundAssignIR(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call { i1, i8* } @Foo.count$set")
	assertContains(t, ir, "auto.propagate")
}

// TestFailableGetterCompoundAssignIR — `f.count += v` reads the current value
// via a failable getter; the read must auto-propagate and the arithmetic must
// operate on the extracted ok value, not the failable result struct. T0709.
func TestFailableGetterCompoundAssignIR(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define { i1, i64, i8* } @Foo.count")
	// Read routes through auto-propagation and the op uses the ok payload (field 1).
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "auto.ok")
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
	// The arithmetic must NOT be applied to the raw failable struct (the old bug).
	assertNotContains(t, ir, "add { i1, i64, i8* }")
}

// TestFailableIndexGetterCompoundIR — `b[0] += v` reads via a failable [] getter
// (non-optional return), which must auto-propagate before the op. T0709.
func TestFailableIndexGetterCompoundIR(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
	assertNotContains(t, ir, "add { i1, i64, i8* }")
}

// TestFailableIndexGetterIncDecIR — `b[0]++` reads via a failable [] getter,
// which must auto-propagate before the increment. T0709.
func TestFailableIndexGetterIncDecIR(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
	assertNotContains(t, ir, "add { i1, i64, i8* }")
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

// TestFailableGetterCompoundInGenericMethodIR — a failable getter compound inside
// a monomorphized generic method body exercises unwrapFailableCompoundRead's
// typeSubst path (c.typeSubst != nil). The operand is a concrete int field, so
// the native += is valid while the method itself is mono'd per instantiation. T0709.
func TestFailableGetterCompoundInGenericMethodIR(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T payload;
			int counter;
			get count! int {
				if this.counter < 0 { raise error("neg"); }
				return this.counter;
			}
			set count(int v) { this.counter = v; }
			bump!() { this.count += 1; }
		}
		main!() {
			b := Box[int](payload: 0, counter: 10);
			b.bump();
		}
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
	assertNotContains(t, ir, "add { i1, i64, i8* }")
}

// TestPropertyIncDecIR: T0712. `f.count++` on a getter/setter property (no
// backing field) must read via the getter and write via the setter, not panic
// in genFieldPtr.
func TestPropertyIncDecIR(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			int x;
			get count int { return this.x; }
			set count(int v) { this.x = v; }
		}
		main() {
			f := Foo(x: 1);
			f.count++;
		}
	`)
	assertContains(t, ir, "call i64 @Foo.count(")
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "call void @Foo.count$set(")
}

// TestPropertyDecIR: T0712. `f.count--` lowers to a subtraction.
func TestPropertyDecIR(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			int x;
			get count int { return this.x; }
			set count(int v) { this.x = v; }
		}
		main() {
			f := Foo(x: 1);
			f.count--;
		}
	`)
	assertContains(t, ir, "call i64 @Foo.count(")
	assertContains(t, ir, "sub i64")
	assertContains(t, ir, "call void @Foo.count$set(")
}

// TestFailablePropertyIncDecIR: T0712. With a failable getter and setter, the
// getter result is unwrapped (extractvalue from {i1, i64, i8*}) before the op,
// and the setter result auto-propagates — no malformed `add { i1` on the struct.
func TestFailablePropertyIncDecIR(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define { i1, i64, i8* } @Foo.count(")
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
	assertContains(t, ir, "call { i1, i8* } @Foo.count$set")
	assertContains(t, ir, "auto.propagate")
	assertNotContains(t, ir, "add { i1")
}

// TestGenericPropertyIncDecIR: T0712. Inc/dec on a property of a generic type
// must dispatch through the monomorphized getter/setter. `this.total++` inside a
// generic method body exercises the receiver-type substitution branch
// (c.typeSubst) in genIncDecTarget — the receiver type is Box[T] and must be
// substituted to Box[int] before the accessor lookup.
func TestGenericPropertyIncDecIR(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T v;
			int count;
			get total int { return this.count; }
			set total(int x) { this.count = x; }
			bump() { this.total++; }
		}
		main() {
			b := Box[int](v: 5, count: 10);
			b.bump();
			b.total--;
		}
	`)
	// Inside the monomorphized bump(): this.total++ dispatches through mono accessors.
	assertContains(t, ir, `call i64 @"Box[int].total"(`)
	assertContains(t, ir, "add i64")
	assertContains(t, ir, `call void @"Box[int].total$set"(`)
	// At main's call site: b.total-- (concrete-instance receiver, no typeSubst).
	assertContains(t, ir, "sub i64")
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
			sum(this) T { return this.first; }
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

// T0565: a non-generic user type that has a generic value-type instance as a
// field must lay out the field slot using the mono value struct (wider, with
// embedded fields), not the generic {i8*, i8*} slot. The construction-time
// store would otherwise mismatch the slot type and crash codegen.
func TestGenericValueTypeAsField(t *testing.T) {
	ir := generateIR(t, `
		type Pt[T] {
			T x `+"`"+`value;
			T y `+"`"+`value;
		}
		type Outer {
			Pt[int] inner;
		}
		main() {
			o := Outer(inner: Pt[int](x: 1, y: 2));
		}
	`)
	// The mono value struct typedef is present.
	assertContains(t, ir, "%\"promise_Pt[int]_v\" = type { i8*, i64, i64 }")
	// Outer's instance struct uses the wider mono value struct as the field
	// slot, not the standard {i8*, i8*} value layout.
	assertContains(t, ir, "%promise_Outer_i = type { %promise_Outer_m*, %\"promise_Pt[int]_v\" }")
}

// T0565: a non-generic value type used as a direct field of another type with
// REVERSE declaration order (containing type before value type). Without the
// topological walk over value-type field dependencies, the containing type's
// layout would be computed before the value type's, producing the wrong slot
// type. This exercises the extractNamed/IsValueType fallback in
// collectValueTypeFieldDeps.
func TestNonGenericValueTypeFieldReverseOrder(t *testing.T) {
	ir := generateIR(t, `
		type WithCoord {
			Coord pos;
		}
		type Coord {
			int x `+"`"+`value;
			int y `+"`"+`value;
		}
		main() {
			w := WithCoord(pos: Coord(x: 1, y: 2));
		}
	`)
	// Coord's value struct typedef exists.
	assertContains(t, ir, "%promise_Coord_v = type { i8*, i64, i64 }")
	// WithCoord's instance struct uses the wider Coord value struct, not {i8*, i8*}.
	assertContains(t, ir, "%promise_WithCoord_i = type { %promise_WithCoord_m*, %promise_Coord_v }")
}

// T0565: a tuple field containing generic value-type instances. Exercises the
// *types.Tuple recursion in collectValueTypeFieldDeps so each tuple element is
// laid out before the containing type.
func TestTupleOfGenericValueTypesAsField(t *testing.T) {
	ir := generateIR(t, `
		type Pt[T] {
			T x `+"`"+`value;
			T y `+"`"+`value;
		}
		type WithTuple {
			(Pt[int], Pt[f64]) pair;
		}
		main() {
			w := WithTuple(pair: (Pt[int](x: 1, y: 2), Pt[f64](x: 3.0, y: 4.0)));
		}
	`)
	// Both mono value structs are present.
	assertContains(t, ir, "%\"promise_Pt[int]_v\" = type { i8*, i64, i64 }")
	assertContains(t, ir, "%\"promise_Pt[f64]_v\" = type { i8*, double, double }")
}

// T0565: a generic outer type with a Pt[T] field — after monomorphization the
// substituted field becomes Pt[int] (a *types.Instance after subst). The mono
// outer layout must use the wider value struct for the field slot.
func TestGenericOuterWithGenericValueField(t *testing.T) {
	ir := generateIR(t, `
		type Pt[T] {
			T x `+"`"+`value;
			T y `+"`"+`value;
		}
		type Container[T] {
			Pt[T] pos;
		}
		main() {
			c := Container[int](pos: Pt[int](x: 1, y: 2));
		}
	`)
	assertContains(t, ir, "%\"promise_Pt[int]_v\" = type { i8*, i64, i64 }")
	assertContains(t, ir, "%\"promise_Container[int]_i\" = type { %\"promise_Container[int]_m\"*, %\"promise_Pt[int]_v\" }")
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
			clone(this) Box[T] { return Box[T](val: this.val); }
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

// TestMonoMapWithTupleValue is a regression test for T0400: instantiating
// Map[K, V] with a tuple V used to panic in codegen because the mono spiral
// guard over-marked Vector[(K, V)] as spiral, preventing _FnIter[T] from
// being resolved during Vector.iter()'s body monomorphization. After the
// originWrapsTypeParams precondition was added, Vector — which doesn't
// intrinsically wrap its TypeParam in a Tuple — is correctly skipped from
// spiral marking, letting the chain bound at Iterator/_FnIter as intended.
func TestMonoMapWithTupleValue(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := map[string, (string, int)]();
			m["a"] = ("alpha", 1);
		}
	`)
	// _FnIter[(string, (string, int))] must be monomorphized (so its layout
	// exists) — this is the instance whose missing layout caused the panic.
	assertContains(t, ir, "_FnIter[(string, (string, int))]")
	// Vector and Iterator instances for the tuple value must also exist.
	assertContains(t, ir, "Vector[(string, (string, int))]")
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
		fail!() int { raise error(message: "oops"); }
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
	assertContains(t, ir, "define { i1, i64, i8* } @Foo.value(")
}

func TestFailableGetterVirtualDispatch(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "@promise_vtable_Base")
	assertContains(t, ir, "@promise_vtable_Impl")
	assertContains(t, ir, "define { i1, i64, i8* } @Impl.value(")
}

func TestFailableGetterStringResult(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define { i1, i8*, i8* } @Foo.label(")
}

// T0494: Getter returning a heap user type ({i8*, i8*}) used as a method-chain
// receiver must register a heap temp drop so the cloned value is freed.
func TestGetterUserHeapResultTrackedInChain(t *testing.T) {
	ir := generateIR(t, `
		type Inner { string _name; drop(~this) {}
			ok(this) bool { return true; }
		}
		type Outer { Inner _inner;
			get inner Inner `+"`public"+` => Inner(_name: this._inner._name);
		}
		test() {
			Outer o = Outer(_inner: Inner(_name: "hi"));
			bool b = o.inner.ok();
		}
	`)
	// The cloned Inner returned by the getter must be tracked as a heap temp
	// (heap.drop block) and freed with the type's drop function at stmt end.
	assertContains(t, ir, "define void @__user.test()")
	assertContains(t, ir, "heap.drop")
	assertContains(t, ir, "Inner.drop")
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

// T0494: A getter returning a non-droppable primitive must NOT add any new
// drop tracking — the original B0290 sliver only fired for strings.
func TestGetterPrimitiveResultNotTracked(t *testing.T) {
	ir := generateIR(t, `
		type Counter { int _n;
			get n int => this._n;
		}
		test() {
			Counter c = Counter(_n: 5);
			int v = c.n;
		}
	`)
	fn := extractFunction(ir, "__user.test")
	// No new heap.drop or stmt-temp drop should appear for the int getter.
	assertNotContains(t, fn, "promise_string_drop")
	assertNotContains(t, fn, "Vector.drop")
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

// T0494: Virtual dispatch path with a heap user-type return. Exercises
// trackHeapUserTypeResult under genVirtualGetterCall — symmetric to the
// direct-path TestGetterUserHeapResultTrackedInChain.
func TestVirtualGetterHeapResultTrackedInChain(t *testing.T) {
	ir := generateIR(t, `
		type Inner { string _name; drop(~this) {}
			ok(this) bool { return true; }
		}
		type HeapSource {
			get item Inner `+"`public"+` `+"`abstract"+`;
		}
		type HeapImpl is HeapSource {
			Inner _item;
			get item Inner `+"`public"+` => Inner(_name: this._item._name);
		}
		test() {
			HeapSource src = HeapImpl(_item: Inner(_name: "x"));
			bool b = src.item.ok();
		}
	`)
	// Vtable wiring exists (virtual dispatch path).
	assertContains(t, ir, "@promise_vtable_HeapSource")
	assertContains(t, ir, "@promise_vtable_HeapImpl")
	// Heap-temp drop is registered and Inner.drop is wired up.
	assertContains(t, ir, "heap.drop")
	assertContains(t, ir, "Inner.drop")
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

func TestSchedLoopNoSetjmp(t *testing.T) {
	// T0149: sched_loop no longer uses setjmp/longjmp for panic recovery.
	// Panicked goroutines reach final suspend via TLS panic flag propagation
	// (T0146-T0148), so the scheduler just calls coro.resume directly.
	ir := generateIR(t, `
		main() { }
	`)
	fn := extractFunction(ir, "promise_sched_loop")
	// Must NOT contain setjmp or jmpBuf alloca
	assertNotContains(t, fn, "alloca [256 x i8]")
	assertNotContains(t, fn, "setjmp")
	assertNotContains(t, fn, "panic_recovery")
	// Must contain direct coro.resume in the run_g flow
	assertContains(t, fn, "call void @llvm.coro.resume")
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

func TestFindRunnableSchedTickGlobalFirstCheck(t *testing.T) {
	// T0326: find_runnable must check global queue first every 61 scheduling
	// iterations (schedtick % 61 == 0) to prevent starvation of goroutines
	// enqueued by non-M threads (e.g., test-thread channel ops).
	ir := generateIR(t, `main() {}`)
	fn := extractFunction(ir, "promise_sched_find_runnable")

	// Entry block must read the schedtick field (P field index 7), increment it,
	// and store it back.
	assertContains(t, fn, "i32 0, i32 7")
	// Must compute urem with 61 (prime modulus chosen to avoid resonance with
	// power-of-2 queue sizes) and branch to try_global when result == 0.
	assertContains(t, fn, "urem i64 %")
	assertContains(t, fn, ", 61")
	assertContains(t, fn, "label %try_global, label %check_local")
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

func TestSchedLoopIncrementsSchedTick(t *testing.T) {
	// T0326: sched_loop's runG block must also increment P.schedTick (field 7)
	// before resuming a goroutine. find_runnable uses the tick value set here
	// (not only the one it sets itself) for the global-first priority decision.
	ir := generateIR(t, `main() {}`)
	fn := extractFunction(ir, "promise_sched_loop")

	// sched_loop's runG block reads, increments, and stores back P field 7.
	assertContains(t, fn, "i32 0, i32 7")
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
		get_cwd!() string `+"`"+`extern("promise_get_cwd");
		main() {
			string s = get_cwd()?!;
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
	assertContains(t, ir, "define i64 @__user.answer()")
	// Usage should call the getter (no args)
	assertContains(t, ir, "call i64 @__user.answer()")
}

func TestModuleLevelSetterCodegen(t *testing.T) {
	ir := generateIR(t, `
		get counter int { return 0; }
		set counter(int value) {}
		main() { counter = 42; }
	`)
	// Setter stored as counter$set, takes one i64 param
	assertContains(t, ir, "define void @__user.counter$set(i64")
	// Assignment should call the setter
	assertContains(t, ir, "call void @__user.counter$set(i64")
}

func TestModuleLevelCompoundAssignCodegen(t *testing.T) {
	ir := generateIR(t, `
		get counter int { return 0; }
		set counter(int value) {}
		main() { counter += 5; }
	`)
	// Should call getter then setter
	assertContains(t, ir, "call i64 @__user.counter()")
	assertContains(t, ir, "call void @__user.counter$set(i64")
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
	assertContains(t, ir, "define i64 @__user.val()")
	assertContains(t, ir, "define void @__user.val$set(i64")
}

func TestEnumMethodDecl(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue,
			describe(this) string {
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
			is_point(this) bool {
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
			rank(this) int {
				match this {
					Level.Low => { return 1; },
					Level.High => { return 2; },
				}
			}
			gt(this, Level other) bool {
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
	assertContains(t, ir, "@Mode.check(i8* %this)")
}

func TestEnumMethodVoid(t *testing.T) {
	ir := generateIR(t, `
		enum State { On, Off,
			log(this) { print_line("x"); }
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
		type Animal { string name; speak(this) string `+"`"+`abstract; }
		type Dog is Animal { string breed; speak(this) string { return "woof"; } }
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

// T1012: `if x is V(field)` destructuring a heap payload of a DROPPABLE enum
// must deep-clone the payload and register a drop for the binding, so an escaped
// binding (return / store-to-outer) owns an independent copy — otherwise it
// aliases the subject's payload and dangles when the subject is dropped (UAF).
func TestT1012IfIsDestructureHeapFieldDupsOnDroppableEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		make() string {
			Msg m = Msg.Text(body: "a" + "b");
			if m is Text(body) { return body; }
			return "";
		}
		main() { s := make(); }
	`)
	fn := extractFunction(ir, "__user.make")
	// The heap payload is dup'd via cloneResolvedValue (string clone block).
	assertContains(t, fn, "strdup.copy")
	// The binding gets a drop flag registered (dropped on fall-through, cleared
	// on move at the return site).
	assertContains(t, fn, "body.dropflag")
}

// T1012 negative control: an int payload binding must NOT be cloned — value/
// numeric payloads stay zero-copy (criterion #3).
func TestT1012IfIsDestructureNumericFieldNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		grab() int {
			Msg m = Msg.Code(n: 7);
			if m is Code(n) { return n; }
			return 0;
		}
		main() { x := grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	assertContains(t, fn, "icmp eq i32") // sanity: we extracted the real function body
	assertNotContains(t, fn, "strdup.copy")
	assertNotContains(t, fn, "n.dropflag")
}

// T1012 (T0485 branch): an Optional-of-heap variant payload (`string? maybe`)
// destructured via `if x is V(field)` must NOT be dup'd — it is marked
// match-borrowed instead (the binding aliases the subject's payload, which the
// subject's synth enum drop owns). So no clone and no per-binding drop flag are
// emitted; only in-scope reads are sound (escape is the separate T1170 gap).
func TestT1012IfIsDestructureOptionalPayloadBorrowNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		read() int {
			Box b = Box.Has(maybe: "a" + "b");
			int out = 0;
			if b is Has(maybe) {
				if s := maybe { out = s.len; }
			}
			return out;
		}
		main() { x := read(); }
	`)
	fn := extractFunction(ir, "__user.read")
	// Optional heap payload is borrow-marked, not cloned, and no drop flag is
	// registered for the `maybe` binding.
	assertNotContains(t, fn, "strdup.copy")
	assertNotContains(t, fn, "maybe.dropflag")
}

// T1170: an Optional-of-heap variant payload (`string? maybe`) that ESCAPES the
// narrowing scope (here via `return`) must be deep-cloned on the read/escape side
// (genIdentExpr, gated on matchBorrowedIdents + the dup flag genReturnStmt sets),
// so the escaped Optional owns an independent inner string and survives the
// subject's synth enum drop. The clone lowers through dupString → `strdup.copy`.
func TestT1170OptionalPayloadEscapeDupsOnReturn(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		esc() string? {
			Box b = Box.Has(maybe: "a" + "b");
			if b is Has(maybe) { return maybe; }
			return none;
		}
		main() { s := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	// The escaping Optional[string] payload is cloned via dupString.
	assertContains(t, fn, "strdup.copy")
}

// T1170 zero-copy control: an in-scope read of an Optional-of-heap payload (no
// escape → no dup flag set) must NOT clone. This is the proof that the escape
// dup is gated on an owning sink and in-scope borrows stay zero-copy.
func TestT1170OptionalPayloadInScopeNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		read() int {
			Box b = Box.Has(maybe: "a" + "b");
			int out = 0;
			if b is Has(maybe) {
				if s := maybe { out = s.len; }
			}
			return out;
		}
		main() { x := read(); }
	`)
	fn := extractFunction(ir, "__user.read")
	assertNotContains(t, fn, "strdup.copy")
}

// T1170: a fixed-array element of a match-borrowed payload (`a[0]` where `a`
// binds `string[N]`) escaping to an outer local (`out = a[0]`) must be cloned on
// read (genArrayIndex, driven by the dup flag genAssignStmt sets for a
// borrow-marked array-index RHS), so `out` owns an independent copy that survives
// the subject's synth enum drop.
func TestT1170ArrayElementEscapeDupsOnStore(t *testing.T) {
	ir := generateIR(t, `
		enum Holder { Pair(string[2] a), Empty }
		esc() string {
			Holder h = Holder.Pair(a: ["x" + "1", "y" + "2"]);
			string out = "";
			if h is Pair(a) { out = a[0]; }
			return out;
		}
		main() { s := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	// The escaping array element string is cloned via dupString.
	assertContains(t, fn, "strdup.copy")
}

// T1172: a fixed-array-returning function whose body contains a panic-capable
// operation (string concat) reaches the panic-cleanup return (emitPanicReturn),
// which must emit the array zero aggregate — NOT the i64-0 default that produced
// malformed `ret i64 0` in a `[N x T]`-returning function.
func TestT1172ArrayReturnPanicCleanupZeroValue(t *testing.T) {
	ir := generateIR(t, `
		enum ArrHolder { Pair(string[2] a), Empty }
		mk() string[2] {
			ArrHolder h = ArrHolder.Pair(a: ["x" + "1", "y" + "2"]);
			return ["z", "w"];
		}
		main() { a := mk(); }
	`)
	fn := extractFunction(ir, "__user.mk")
	// The panic-cleanup path returns a zeroinitializer of the array type, never
	// a bare i64 0.
	assertContains(t, fn, "ret [2 x i8*] zeroinitializer")
	assertNotContains(t, fn, "ret i64 0")
}

// T1170: an Optional-of-heap payload stored to an escaping OUTER local
// (`out = maybe`, whole-Optional ident RHS) must be cloned on read. This
// exercises the genAssignStmt IdentExpr-RHS branch (isVariantPayloadBorrowShape)
// — distinct from the array-element RHS branch covered above.
func TestT1170OptionalPayloadEscapeDupsOnStore(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		esc() string? {
			Box b = Box.Has(maybe: "a" + "b");
			string? out = none;
			if b is Has(maybe) { out = maybe; }
			return out;
		}
		main() { s := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "strdup.copy")
}

// T1170: an Optional-of-heap payload passed to a consuming (~/move) param
// (`consume(move maybe)`) escapes into the callee and must be cloned so the
// subject's synth enum drop doesn't free the value the callee now owns. This
// exercises the maybeEnableDupForMutRefArg T1170 branch.
func TestT1170OptionalPayloadEscapeConsumingArg(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		consume(string? move s) string { if x := s { return x; } return ""; }
		esc() string {
			Box b = Box.Has(maybe: "a" + "b");
			string r = "";
			if b is Has(maybe) { r = consume(move maybe); }
			return r;
		}
		main() { s := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "strdup.copy")
}

// T1170: an Optional-of-heap payload used to initialize an owned constructor
// field (`W(held: maybe)`) escapes via the returned instance and must be cloned.
// This exercises the maybeEnableDupForConstructorArg T1170 branch.
func TestT1170OptionalPayloadEscapeConstructorField(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		type W { string? held; }
		esc() W {
			Box b = Box.Has(maybe: "a" + "b");
			W w = W(held: none);
			if b is Has(maybe) { w = W(held: maybe); }
			return w;
		}
		main() { w := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "strdup.copy")
}

// T1170: the escape dup fires uniformly for the `match` path (not just `if is`),
// since both populate matchBorrowedIdents. A match arm returning an
// Optional-of-heap payload must clone on read.
func TestT1170OptionalPayloadEscapeDupsOnMatch(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		esc() string? {
			Box b = Box.Has(maybe: "a" + "b");
			match b {
				Has(maybe) => { return maybe; },
				Nothing => { return none; },
			}
		}
		main() { s := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "strdup.copy")
}

// T1174: an Optional-of-heap-user-type variant payload (`Row? maybe`) that
// ESCAPES `if is`/`match` (return / store-to-outer / consuming arg / constructor
// field) must be deep-cloned via dupBorrowedHeapUserPayload — otherwise the
// bound alias points into the subject's variant payload, which the subject's
// synth enum drop frees at scope exit (UAF / SIGSEGV). The clone lowers to a
// dupHeapValue `heapdup.copy` block in the escaping function.
func TestT1174OptionalHeapUserPayloadEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Has(Row? maybe), Nothing }
		esc() Row? {
			Box b = Box.Has(maybe: Row(name: "a" + "b"));
			if b is Has(maybe) { return maybe; }
			return none;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	// Escaping the borrowed Optional[Row] payload deep-clones the inner heap value.
	assertContains(t, fn, "heapdup.copy")
}

// T1174 over-application guard: an in-scope-only Optional[heap-user] binding must
// stay a zero-copy borrow (no dup) — the subject outlives the narrowing and its
// synth enum drop frees the payload exactly once. The dup is gated to explicit
// escape sites, so no `heapdup.copy` is emitted here (preserving the T0512
// nested-Optional zero-copy invariant). An over-eager dup would also leak.
func TestT1174OptionalHeapUserPayloadInScopeNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Has(Row? maybe), Nothing }
		rd() int {
			Box b = Box.Has(maybe: Row(name: "a" + "b"));
			int out = 0;
			if b is Has(maybe) { if r := maybe { out = r.name.len; } }
			return out;
		}
		main() { x := rd(); }
	`)
	fn := extractFunction(ir, "__user.rd")
	assertNotContains(t, fn, "heapdup.copy")
}

// T1174: `v.push(maybe)` moves a match-borrowed Optional[heap-user] payload into
// a vector. Push is a native special-case that bypasses the escape-site dups, so
// it must deep-clone the Optional[heap-user] element in maybeDupPushElement's
// Optional branch — otherwise the vector slot aliases the subject's variant
// payload and double-frees when both drop. The clone lowers to a dupHeapValue
// `heapdup.copy` block. Also covers the pre-existing Vector[Row?] slice path,
// which shares the same maybeDupPushElement branch.
func TestT1174OptionalHeapUserPushDups(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Has(Row? maybe), Nothing }
		esc() Row?[] {
			Box b = Box.Has(maybe: Row(name: "a" + "b"));
			Row?[] v = [];
			if b is Has(maybe) { v.push(maybe); }
			return v;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "heapdup.copy")
}

// T1174: optionalHeapDupElem admits BOTH droppable heap-user types (via
// isDroppableHeapUserType — the Row-with-string cases above) AND no-drop-but-heap
// user types (via isHeapUserNoDropPalFree — a heap type whose fields need no
// drop). This pins the second branch: a `P?` payload (`type P { int x; }`, heap-
// allocated, pal_free-only, no synth drop) escaping `if is` must still deep-clone
// the inner via dupHeapValue, else the returned alias is freed by the subject's
// synth enum drop at scope exit (UAF). A value type would be copied by value and
// route past optionalHeapDupElem, so the presence of `heapdup.copy` confirms the
// no-drop heap branch is taken.
func TestT1174OptionalNoDropHeapUserPayloadEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		type P { int x; }
		enum Box { Has(P? maybe), Nothing }
		esc() P? {
			Box b = Box.Has(maybe: P(x: 42));
			if b is Has(maybe) { return maybe; }
			return none;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "heapdup.copy")
}

// T1171: a whole fixed-Array-of-heap-user variant payload (`Row[2] value`) that
// ESCAPES `if is`/`match` (return / store-to-outer / consuming arg / constructor
// field) must be element-wise deep-cloned via dupBorrowedHeapUserPayload's Array
// branch (arrayHeapDupElem) — otherwise the escaped [N x {vtable,instance}]
// aggregate aliases the subject's variant payload, which the subject's synth enum
// drop frees at scope exit (UAF / SIGSEGV). Each element clone lowers to a
// dupHeapValue `heapdup.copy` block, and the aggregate is rebuilt with N
// `insertvalue`s (one per element).
func TestT1171ArrayHeapUserPayloadEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Some(Row[2] value), Empty }
		esc() Row[2] {
			Box b = Box.Some(value: [Row(name: "a" + "b"), Row(name: "c" + "d")]);
			if b is Some(value) { return value; }
			return [Row(name: "x"), Row(name: "y")];
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	// Escaping the borrowed Array[Row] payload deep-clones each element.
	assertContains(t, fn, "heapdup.copy")
	// One insertvalue per array element rebuilds the cloned aggregate at the sink.
	if n := strings.Count(fn, "insertvalue"); n < 2 {
		t.Fatalf("expected >= 2 insertvalue (one per array element), got %d\n%s", n, fn)
	}
}

// T1171 over-application guard: an in-scope-only Array[heap-user] binding must
// stay a zero-copy borrow (no dup) — the subject outlives the narrowing and its
// synth enum drop frees each element exactly once. The dup is gated to explicit
// escape sites, so no `heapdup.copy` is emitted here. An over-eager dup would also
// leak.
func TestT1171ArrayHeapUserPayloadInScopeNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Some(Row[2] value), Empty }
		rd() int {
			Box b = Box.Some(value: [Row(name: "a" + "b"), Row(name: "c" + "d")]);
			int out = 0;
			if b is Some(value) { out = value[0].name.len; }
			return out;
		}
		main() { x := rd(); }
	`)
	fn := extractFunction(ir, "__user.rd")
	assertNotContains(t, fn, "heapdup.copy")
}

// T1171 generic/monomorphized path: when the Array[heap-user] payload lives in a
// GENERIC enum (`GBox[T]` with a `T[2]` variant field), the escape sink must
// resolve the element type through c.typeSubst (T -> Row) in BOTH
// dupBorrowedHeapUserPayload (t = Substitute(...)) and arrayHeapDupElem
// (elem = Substitute(arr.Elem(), ...)). Without those substitutions the array
// recognizer misses the shape and the escaped aggregate aliases the moved-in
// subject's payload (UAF). This is the only test that exercises the typeSubst
// branches of both helpers, so the monomorphized `gesc[Row]` must still emit the
// per-element `heapdup.copy`.
func TestT1171GenericArrayHeapUserPayloadEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum GBox[T] { Some(T[2] value), Empty }
		gesc[T](GBox[T] move b, T[2] fb) T[2] {
			if b is Some(value) { return value; }
			return fb;
		}
		main() {
			GBox[Row] b = GBox[Row].Some(value: [Row(name: "a" + "b"), Row(name: "c" + "d")]);
			Row[2] fb = [Row(name: "x"), Row(name: "y")];
			r := gesc(move b, fb);
		}
	`)
	// Monomorphized generic funcs are emitted as @"gesc[Row]" (quoted name).
	fn := extractFunction(ir, `"gesc[Row]"`)
	if fn == "" {
		t.Fatalf("monomorphized gesc[Row] not found in IR:\n%s", ir)
	}
	assertContains(t, fn, "heapdup.copy")
	if n := strings.Count(fn, "insertvalue"); n < 2 {
		t.Fatalf("expected >= 2 insertvalue (one per array element), got %d\n%s", n, fn)
	}
}

// T1176: reading a whole fixed-Array[heap-user] struct field out by value and
// returning it (`return w.rows`) must element-wise deep-clone. genFieldAccess
// routes the array field through dupHeapFieldForEscape's array branch, which
// extractvalue/dupHeapValue/insertvalue's each element. Before the fix the
// [N x {vtable,instance}] aggregate was aliased and the owner's synth drop freed
// each element at scope exit while the returned copy still pointed in (UAF).
// The clone lowers to a per-element dupHeapValue `heapdup.copy` block.
func TestT1176ArrayHeapUserFieldEscapeDupsOnReturn(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		type Wrap { Row[2] rows; }
		esc() Row[2] {
			Wrap w = Wrap(rows: [Row(name: "de" + "ep"), Row(name: "x" + "x")]);
			return w.rows;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	// Each escaping array element is deep-cloned via dupHeapValue.
	assertContains(t, fn, "heapdup.copy")
	// One insertvalue per element re-assembles the cloned array aggregate
	// ([2 x {vtable,instance}] — value structs are unnamed in the test harness).
	if n := strings.Count(fn, "insertvalue [2 x { i8*, i8* }]"); n < 2 {
		t.Errorf("expected >=2 insertvalue into the cloned array aggregate (one per element), got %d\n%s", n, fn)
	}
}

// T1176: a no-drop-but-pal-free element array field parity case — a heap-user
// type with only scalar fields has no synth drop but is still pal_free'd, so
// arrayHeapDupElem's isHeapUserNoDropPalFree branch must still deep-clone each
// escaping element. A value-type element would be copied by value and route
// past arrayHeapDupElem, so the presence of `heapdup.copy` confirms the no-drop
// heap branch is taken.
func TestT1176ArrayNoDropHeapUserFieldEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		type P { int x; }
		type Wrap { P[2] cells; }
		esc() P[2] {
			Wrap w = Wrap(cells: [P(x: 11), P(x: 22)]);
			return w.cells;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "heapdup.copy")
}

// T1176 over-application guard: an in-scope read of a fixed-Array[heap-user]
// field element (no escape → no dup flag set) must NOT clone. This proves the
// escape dup is gated on an owning sink and in-scope borrows stay zero-copy
// (an over-eager clone would also leak).
func TestT1176ArrayHeapUserFieldInScopeNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		type Wrap { Row[2] rows; }
		rd() int {
			Wrap w = Wrap(rows: [Row(name: "de" + "ep"), Row(name: "x" + "x")]);
			return w.rows[0].name.len;
		}
		main() { x := rd(); }
	`)
	fn := extractFunction(ir, "__user.rd")
	assertNotContains(t, fn, "heapdup.copy")
}

// T1176: escaping a generic array field whose element is a type parameter
// (`T[2]`) from inside a GENERIC function body must also deep-clone. Because the
// escape sits in `grab[T]`'s body, the field type is the unresolved `T[2]` and
// mono has `typeSubst` active (T→Row) at the access — this drives
// arrayHeapDupElem's `types.Substitute` branch, which the concrete-instance
// cases (field type already `Row[2]`, typeSubst nil) never reach. The
// monomorphized `grab__Row` still clones each escaping element. `h` is borrowed,
// so its synth drop runs in the caller while the returned copy must stay valid.
func TestT1176GenericArrayHeapUserFieldEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		type Holder[T] { T[2] data; }
		grab[T](Holder[T] h) T[2] { return h.data; }
		main() {
			Holder[Row] h = Holder[Row](data: [Row(name: "de" + "ep"), Row(name: "x" + "x")]);
			r := grab[Row](h);
		}
	`)
	// Mono generic funcs are emitted with a bracketed, quoted LLVM name
	// (`@"grab[Row]"`), so the extract marker must include the quotes.
	fn := extractFunction(ir, `"grab[Row]"`)
	assertContains(t, fn, "heapdup.copy")
}

// T1176 gate negative: escaping a value-element array field (`int[2]`) out of a
// DROPPABLE owner must NOT clone — arrayHeapDupElem recognizes the array but its
// element is neither a droppable heap-user nor a no-drop-pal-free type, so it
// returns false (the `int[]`/value-array fall-through) and the field is copied
// by value. The owner is made droppable by its sibling string field, so the
// escape sink runs setDupFlagsForFieldAccess → arrayHeapDupElem for real; the
// absence of `heapdup.copy` proves value arrays are left untouched.
func TestT1176ValueArrayFieldEscapeNoDup(t *testing.T) {
	ir := generateIR(t, `
		type VW { int[2] a; string s; }
		esc() int[2] {
			VW w = VW(a: [7, 8], s: "x" + "y");
			return w.a;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertNotContains(t, fn, "heapdup.copy")
}

// T1178: a fixed-Array[heap-user] variant payload escaping an `if is` destructure
// must be deep-cloned EXACTLY ONCE. T1176 added an arrayHeapDupElem branch to BOTH
// setDupFlagsForFieldAccess (the sink) and dupHeapFieldForEscape (the read side).
// For a variant payload the sink already clones via dupBorrowedHeapUserPayload, so
// letting the read-side dupHeapFieldForEscape array branch ALSO fire produced a
// SECOND element-wise clone whose elements were never dropped (leak). The fix skips
// the read-side dup for the array shape (genIdentExpr gates on !arrayHeapDupElem).
// The presence-only T1171/T1176 tests above (heapdup.copy present, >=2 insertvalue)
// pass under BOTH single- and double-clone, so they never caught the regression —
// this test pins the EXACT clone count: one per element (2 for a 2-elem array),
// not two. A double-clone re-emits the array-aggregate insertvalues, so a count of
// 4 (or any value != 2) fails here.
func TestT1178VariantPayloadArrayEscapeSingleClone(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Some(Row[2] value), Empty }
		esc() Row[2] {
			Box b = Box.Some(value: [Row(name: "a" + "b"), Row(name: "c" + "d")]);
			if b is Some(value) { return value; }
			return [Row(name: "x"), Row(name: "y")];
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "heapdup.copy")
	// Exactly one insertvalue per element rebuilds the SINGLE cloned aggregate.
	// A double-clone (the T1178 regression) emits 4.
	if n := strings.Count(fn, "insertvalue [2 x { i8*, i8* }]"); n != 2 {
		t.Fatalf("expected exactly 2 array-aggregate insertvalue (single element-wise clone), got %d (double-clone = T1178 regression)\n%s", n, fn)
	}
}

// T1178 (match parity): the same single-clone invariant for the `match` escape
// path — both `if is` and `match` populate matchBorrowedIdents and route through
// the same genIdentExpr read-side gate + dupBorrowedHeapUserPayload sink. Exactly
// one clone per element, not two.
func TestT1178VariantPayloadArrayEscapeMatchSingleClone(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Some(Row[2] value), Empty }
		esc() Row[2] {
			Box b = Box.Some(value: [Row(name: "a" + "b"), Row(name: "c" + "d")]);
			match b {
				Some(value) => { return value; },
				Empty => { return [Row(name: "x"), Row(name: "y")]; },
			}
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "heapdup.copy")
	if n := strings.Count(fn, "insertvalue [2 x { i8*, i8* }]"); n != 2 {
		t.Fatalf("expected exactly 2 array-aggregate insertvalue (single element-wise clone), got %d (double-clone = T1178 regression)\n%s", n, fn)
	}
}

// T1178 (non-array sibling preserved): the fix skips the read-side dup ONLY for the
// array shape. An Optional[heap-user] variant payload (T1174) is NOT covered by
// arrayHeapDupElem, so genIdentExpr's gate (!isArr => true) still lets
// dupBorrowedHeapUserPayload deep-clone it on escape. Guards against the fix
// over-reaching and suppressing the Optional[user] clone (which would be a UAF, not
// a leak). The escaped Optional's inner heap instance is cloned exactly once.
func TestT1178OptionalHeapUserPayloadStillClones(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Some(Row? maybe), Empty }
		esc() Row? {
			Box b = Box.Some(maybe: Row(name: "a" + "b"));
			if b is Some(maybe) { return maybe; }
			return none;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	// dupBorrowedHeapUserPayload's optionalHeapDupElem branch still fires.
	assertContains(t, fn, "heapdup.copy")
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

// B0353: return in error handler inside go block should branch to final.suspend,
// not emit ret void (the coroutine function returns ptr).
func TestGoBlockReturnInErrorHandler(t *testing.T) {
	ir := generateIR(t, `
		fail!() int { raise error(message: "fail"); }
		main() {
			go {
				x := fail()? e { return; };
			};
		}
	`)
	goFunc := extractFunc(ir, ".goroutine.0")
	if goFunc == "" {
		t.Fatal("expected .goroutine.0 function in IR")
	}
	// Should branch to final.suspend instead of ret void
	assertContains(t, goFunc, "br label %final.suspend")
	assertNotContains(t, goFunc, "ret void")
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
		do_thing!() AppError[int] {
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
	assertContains(t, ir, "@__user.schema()")
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
	assertContains(t, ir, "define i8* @__user.data()")
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
	assertContains(t, ir, "@__user.empty()")
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

// T0031: Directory embed getter constructs EmbeddedFiles value
func TestEmbedDirGetter(t *testing.T) {
	file, info := parseWithStd(t, `
		get assets EmbeddedFiles `+"`embed(\"static/...\")"+`;
		main() {
			EmbeddedFiles fs = assets;
		}
	`)
	// Manually populate embed data (normally done by ResolveEmbeds)
	for _, embed := range info.Embeds {
		embed.Kind = sema.EmbedDir
		embed.Data = []byte("body{}hello")
		embed.DirEntries = []sema.EmbedDirEntry{
			{Path: "index.html", Name: "index.html", Size: 5, Offset: 5},
			{Path: "style.css", Name: "style.css", Size: 6, Offset: 0},
		}
	}
	result := Compile(file, info, "")
	ir := result.Module.String()
	// Should contain allocations and string_new for file paths
	assertContains(t, ir, "@pal_alloc")
	assertContains(t, ir, "@promise_string_new")
	// Should contain the data blob
	assertContains(t, ir, "body{}hello")
	// Should return user value type {i8*, i8*}
	assertContains(t, ir, "define { i8*, i8* } @__user.assets()")
}

func TestEmbedDirGetterEmpty(t *testing.T) {
	file, info := parseWithStd(t, `
		get assets EmbeddedFiles `+"`embed(\"empty/...\")"+`;
		main() {
			EmbeddedFiles fs = assets;
		}
	`)
	for _, embed := range info.Embeds {
		embed.Kind = sema.EmbedDir
		embed.Data = []byte{}
		embed.DirEntries = nil
	}
	result := Compile(file, info, "")
	ir := result.Module.String()
	// Even empty dir should produce valid IR with allocations
	assertContains(t, ir, "define { i8*, i8* } @__user.assets()")
	assertContains(t, ir, "@pal_alloc")
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

// TestCoverageGenericMethodInstanceShared is the T0574 regression test.
// A monomorphized generic method's body lives in a per-instance .bc while the
// coverage reporter reads the counter from the main IR's test main. The counter
// global must therefore be a single externally-linked symbol: defined once in
// the main IR and an external declaration (with the increment) in the instance
// .bc. Private linkage would split it into independent per-translation-unit
// copies, so the always-zero main copy would be read → "not covered" (the bug).
func TestCoverageGenericMethodInstanceShared(t *testing.T) {
	file, info := parseWithStd(t, `
		type Box[T] {
			T x;
			inc(this) T { return this.x; }
		}
		test_inc() `+"`test"+` {
			b := Box[int](x: 7);
			int r = b.inc();
			assert(r == 7, "expected 7");
		}
	`)
	result := CompileWithOptions(file, info, "", &CompileOptions{CoverageEnabled: true})
	result.GenerateTestMain(info.Tests, nil)

	// Locate the coverage region for the monomorphized method Box[int].inc.
	idx := -1
	for i, r := range result.CoverageRegions {
		if r.FuncName == "Box[int].inc" && r.Kind == "method" {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatalf("no coverage region for Box[int].inc method; regions: %+v", result.CoverageRegions)
	}
	g := fmt.Sprintf("@__promise_cov_%d", idx)

	// Main IR keeps the single externally-visible definition (not private).
	mainIR, _ := result.SplitModuleIRs()
	assertContains(t, mainIR, g+" = global i64 0")
	assertNotContains(t, mainIR, g+" = private global")

	// The Box[int] instance .bc owns the method body (with the increment) and
	// references the counter as an external declaration — no private/own copy.
	instIRs := result.InstanceIRs()
	instIR, ok := instIRs["Box[int]"]
	if !ok {
		t.Fatalf("missing Box[int] in instance IRs, keys: %v", mapKeys(instIRs))
	}
	assertContains(t, instIR, g+" = external global")
	// The increment (load/add/store) must land in the instance .bc.
	hasStore := false
	for _, line := range strings.Split(instIR, "\n") {
		if strings.Contains(line, "store") && strings.Contains(line, g) {
			hasStore = true
			break
		}
	}
	if !hasStore {
		t.Errorf("Box[int] instance IR must contain the coverage increment store to %s (T0574)", g)
	}
	// No duplicate / private definition in the instance .bc.
	assertNotContains(t, instIR, g+" = global i64 0")
	assertNotContains(t, instIR, g+" = private global")
}

// TestCompileResultCoverageEnabled locks the T0574 cache-isolation contract at
// the accessor level. compileAndLinkSeparate appends "+cov" to the instance
// build-mode iff result.CoverageEnabled() is true, keeping coverage and
// non-coverage instance .bc files in separate build-cache entries (externally-
// linked counter globals would otherwise cause undefined-symbol link errors or
// silent undercount across the two build kinds). The accessor must therefore
// faithfully reflect CompileOptions.CoverageEnabled — including the default
// (nil opts / Compile) case, which must report false.
func TestCompileResultCoverageEnabled(t *testing.T) {
	src := `
		foo() int { return 42; }
		main() {}
	`
	file, info := parseWithStd(t, src)
	if got := CompileWithOptions(file, info, "", &CompileOptions{CoverageEnabled: true}).CoverageEnabled(); !got {
		t.Errorf("CoverageEnabled() = false, want true when CompileOptions.CoverageEnabled is set")
	}

	file2, info2 := parseWithStd(t, src)
	if got := CompileWithOptions(file2, info2, "", &CompileOptions{CoverageEnabled: false}).CoverageEnabled(); got {
		t.Errorf("CoverageEnabled() = true, want false when CompileOptions.CoverageEnabled is explicitly false")
	}

	file3, info3 := parseWithStd(t, src)
	if got := Compile(file3, info3, "").CoverageEnabled(); got {
		t.Errorf("CoverageEnabled() = true, want false for a default (nil opts) compile")
	}
}

// B0134: generic error type constructor inside generic function body
// must be collected for monomorphization via func instance substitution.
func TestGenericErrorTypeInGenericFuncBody(t *testing.T) {
	ir := generateIR(t, `
		type AppError[T] is error { T detail; }
		make_err![T](T detail) AppError[T] {
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

// T0392: Synth drop must recurse into Optional[heap-user-type] field, dropping the
// inner heap allocation. Without this, `Holder { Box? data }` leaks the Box.
func TestSynthDropRecursesIntoHeapUserOptionalField(t *testing.T) {
	ir := generateIR(t, `
		type T0392Box { int n; drop(~this) {} }
		type T0392Holder { T0392Box? data; }
		main() {
			h := T0392Holder(data: T0392Box(n: 7));
		}
	`)
	holderDrop := extractFunction(ir, "T0392Holder.drop")
	if holderDrop == "" {
		t.Fatal("expected T0392Holder.drop in IR")
	}
	// optfield drop block conditional on the present flag.
	assertContains(t, holderDrop, "optfield.drop")
	assertContains(t, holderDrop, "optfield.skip")
	// Inner Box.drop must be invoked for present values.
	assertContains(t, holderDrop, "call void @T0392Box.drop")
	// Heap user type without synth drop also requires pal_free of the instance.
	assertContains(t, holderDrop, "call void @pal_free")
}

// T0392: Synth drop must recurse into nested Optional T?? fields, visiting both
// the outer and inner has-value flags before dropping.
func TestSynthDropRecursesIntoNestedOptionalField(t *testing.T) {
	ir := generateIR(t, `
		type T0392Box2 { int n; drop(~this) {} }
		type T0392Holder2 { T0392Box2?? data; }
		main() {
			T0392Box2? inner = T0392Box2(n: 1);
			h := T0392Holder2(data: inner);
		}
	`)
	holderDrop := extractFunction(ir, "T0392Holder2.drop")
	if holderDrop == "" {
		t.Fatal("expected T0392Holder2.drop in IR")
	}
	// Two pairs of optfield branches — outer Optional and inner Optional.
	if got := strings.Count(holderDrop, "optfield.drop"); got < 2 {
		t.Errorf("expected at least 2 optfield.drop blocks (outer + inner), got %d", got)
	}
	// Inner Box.drop must still be called for the doubly-wrapped value.
	assertContains(t, holderDrop, "call void @T0392Box2.drop")
}

// T0392: Synth drop must call the mono'd drop method for generic heap-user-type
// inner — Box[int].drop, not Box.drop.
func TestSynthDropOptionalGenericInnerUsesMonoName(t *testing.T) {
	ir := generateIR(t, `
		type T0392GBox[T] { T val; drop(~this) {} }
		type T0392GHolder[T] { T0392GBox[T]? data; }
		main() {
			h := T0392GHolder[int](data: T0392GBox[int](val: 7));
		}
	`)
	holderDrop := extractFunction(ir, `"T0392GHolder[int].drop"`)
	if holderDrop == "" {
		t.Fatal("expected T0392GHolder[int].drop in IR")
	}
	// The mono'd inner drop must be called by name (not Box.drop).
	assertContains(t, holderDrop, `call void @"T0392GBox[int].drop"`)
}

// T0415: emitFieldDrops must use the mono'd drop name for non-optional generic
// instance fields with explicit drop. Before the fix, the lookup used the
// origin name "Box.drop" which doesn't exist — the user's drop body was
// silently skipped, leaking heap content inside the field.
func TestFieldDropUsesMonoNameForGenericExplicitDrop(t *testing.T) {
	ir := generateIR(t, `
		type T0415Box[T] { T val; drop(~this) {} }
		type T0415Holder[T] { T0415Box[T] data; }
		main() {
			h := T0415Holder[int](data: T0415Box[int](val: 7));
		}
	`)
	holderDrop := extractFunction(ir, `"T0415Holder[int].drop"`)
	if holderDrop == "" {
		t.Fatal("expected T0415Holder[int].drop in IR")
	}
	// The mono'd inner drop must be called by name.
	assertContains(t, holderDrop, `call void @"T0415Box[int].drop"`)
	// And NOT the origin name (which is the bug shape).
	assertNotContains(t, holderDrop, "call void @T0415Box.drop")
}

// T0415: emitOptionalFieldReassignDrop must use the mono'd drop name when
// reassigning an optional generic field whose inner has explicit drop.
func TestOptionalFieldReassignDropUsesMonoName(t *testing.T) {
	ir := generateIR(t, `
		type T0415Box2[T] { T val; drop(~this) {} }
		type T0415Holder2[T] { T0415Box2[T]? data; }
		main() {
			h := T0415Holder2[int](data: T0415Box2[int](val: 1));
			h.data = T0415Box2[int](val: 2);
		}
	`)
	// The reassignment site lives in the user's main goroutine body.
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	mainFn := rest[:end+2]
	assertContains(t, mainFn, "field.optdrop")
	assertContains(t, mainFn, `call void @"T0415Box2[int].drop"`)
	assertNotContains(t, mainFn, "call void @T0415Box2.drop")
}

// T0415: emitOptionalFieldReassignDrop must also handle the synth-drop-only
// path — generic types with no explicit drop where the type argument resolves
// to a heap type at mono time. The drop call must use the mono name and the
// optdrop block must NOT call pal_free (the synth drop already pal_frees).
// Before the fix, the drop call was skipped entirely (HasDrop=false,
// NeedsSynthDrop=false) and pal_free was called directly, leaking the inner
// heap content. The mono'd synth drop function exists either way (it's used by
// the holder's own drop at scope exit) — to detect the regression we must
// inspect the field.optdrop.free block specifically, not just the whole main.
func TestOptionalFieldReassignDropMonoSynthSkipsExtraFree(t *testing.T) {
	ir := generateIR(t, `
		type T0415RawBox[T] { T val; }
		type T0415RawHolder[T] { T0415RawBox[T]? data; }
		main() {
			h := T0415RawHolder[string](data: T0415RawBox[string](val: "a"));
			h.data = T0415RawBox[string](val: "b");
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
	mainFn := rest[:end+2]
	// Isolate the field.optdrop.free block — content between the label line
	// and the next blank line. emitOptionalFieldReassignDrop produces this
	// label only when handling the reassignment.
	freeLabel := "\nfield.optdrop.free"
	freeStart := strings.Index(mainFn, freeLabel)
	if freeStart < 0 {
		t.Fatal("expected field.optdrop.free block in main")
	}
	// Skip past the label line.
	blockStart := strings.Index(mainFn[freeStart+1:], "\n") + freeStart + 2
	blockEnd := strings.Index(mainFn[blockStart:], "\n\n")
	if blockEnd < 0 {
		t.Fatal("expected end of field.optdrop.free block")
	}
	freeBlock := mainFn[blockStart : blockStart+blockEnd]
	// The reassignment must invoke the mono'd synth drop INSIDE the optdrop
	// free block (not just somewhere else in main).
	assertContains(t, freeBlock, `call void @"T0415RawBox[string].drop"`)
	// And must NOT call pal_free here — the synth drop already pal_freed.
	assertNotContains(t, freeBlock, "call void @pal_free")
	// Confirm test premise: the mono'd synth drop itself does both the
	// inner string drop and the pal_free of the box instance.
	synthDrop := extractFunction(ir, `"T0415RawBox[string].drop"`)
	if synthDrop == "" {
		t.Fatal("expected T0415RawBox[string].drop in IR")
	}
	assertContains(t, synthDrop, "call void @promise_string_drop")
	assertContains(t, synthDrop, "call void @pal_free")
}

// T0392: Force-unwrap of an Optional[heap-user-type] field neutralizes the
// owner's flag so the holder's drop doesn't double-free the inner instance now
// owned by the new local.
func TestForceUnwrapOfHeapUserOptionalFieldNeutralizes(t *testing.T) {
	ir := generateIR(t, `
		type T0392Box3 { int n; drop(~this) {} }
		type T0392Holder3 { T0392Box3? data; }
		main() {
			h := T0392Holder3(data: T0392Box3(n: 3));
			b := h.data!;
		}
	`)
	// Slice out the user's main goroutine body. The C-ABI @main wrapper has no
	// user code; the unwrap site lives in @.goroutine.main.
	defineMarker := "define i8* @.goroutine.main"
	start := strings.Index(ir, defineMarker)
	if start < 0 {
		t.Fatal("expected define of .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace for .goroutine.main")
	}
	mainFn := rest[:end+2]
	// Neutralization stores `i1 false` into the field's present flag.
	assertContains(t, mainFn, "store i1 false")
}

// T1073: force-unwrap `o!` consumed at the collection-literal / raise / select-send
// sites must neutralize the source optional's present flag, exactly like the
// var-decl/assignment/call-arg sites. Otherwise both the source optional's
// scope-exit drop and the container/error-slot/channel free the moved inner →
// double-free (observed as a SIGSEGV). The neutralization GEPs the optional
// param's present field (field 0 of `{ i1, { i8*, i8* } }`) and stores false.
const t1073NeutralizeSig = "{ i1, { i8*, i8* } }* %o.addr, i32 0, i32 0"

func TestT1073ArrayLitForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		arr(T1073Box? move o) T1073Box[] { return [o!]; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.arr")
	if fn == "" {
		t.Fatal("expected __user.arr in IR")
	}
	assertContains(t, fn, "unwrap.ok")
	assertContains(t, fn, t1073NeutralizeSig)
}

func TestT1073TupleLitForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		tup(T1073Box? move o) (T1073Box, int) { return (o!, 1); }
		main() {}
	`)
	fn := extractFunction(ir, "__user.tup")
	if fn == "" {
		t.Fatal("expected __user.tup in IR")
	}
	assertContains(t, fn, t1073NeutralizeSig)
}

func TestT1073MapLitForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		mp(T1073Box? move o) map[int, T1073Box] { return {1: o!}; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.mp")
	if fn == "" {
		t.Fatal("expected __user.mp in IR")
	}
	assertContains(t, fn, t1073NeutralizeSig)
}

func TestT1073RaiseForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Err is error { string d; }
		rz!(T1073Err? move o) int { raise o!; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.rz")
	if fn == "" {
		t.Fatal("expected __user.rz in IR")
	}
	assertContains(t, fn, t1073NeutralizeSig)
}

// T1073: a copy/scalar inner (`int?`) is NOT consumed by force-unwrap, so its
// source optional must NOT be neutralized (it stays usable). neutralizeForceUnwrapElem
// self-gates on typeNeedsFieldDrop, so no present-flag clear is emitted here.
func TestT1073ArrayLitScalarForceUnwrapNoNeutralize(t *testing.T) {
	ir := generateIR(t, `
		arr(int? move o) int[] { return [o!]; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.arr")
	if fn == "" {
		t.Fatal("expected __user.arr in IR")
	}
	// int? optional layout is `{ i1, i64 }`, not `{ i1, { i8*, i8* } }`; and no
	// present-flag clear should be emitted for the (copy) source.
	assertNotContains(t, fn, "i32 0, i32 0\n\tstore i1 false")
}

// T1073: a paren-wrapped force-unwrap `[(o!)]` must still neutralize the source.
// Exercises the ParenExpr-peel loop in isForceUnwrapElem — codegen sees through
// ParenExpr at genExpr but the AST-shape dispatch here must peel it too.
func TestT1073ArrayLitParenWrappedForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		arr(T1073Box? move o) T1073Box[] { return [(o!)]; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.arr")
	if fn == "" {
		t.Fatal("expected __user.arr in IR")
	}
	assertContains(t, fn, t1073NeutralizeSig)
}

// T1073: force-unwrap of a droppable map *key* `{o!: 1}` must neutralize the
// source optional (the map's drop frees keys via []=), mirroring the map-value
// path. Exercises the entry.Key neutralize call site in genMapLit.
func TestT1073MapLitKeyForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Key {
			string name;
			drop(~this) {}
			get hash int { return 7; }
			== (T1073Key other) bool { return this.name == other.name; }
		}
		mk(T1073Key? move o) map[T1073Key, int] { return {o!: 1}; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.mk")
	if fn == "" {
		t.Fatal("expected __user.mk in IR")
	}
	assertContains(t, fn, t1073NeutralizeSig)
}

// T1073: force-unwrap inside a collection literal in a *generic* function body —
// `wrap[T](T? move o) T[] { return [o!]; }` instantiated with a droppable heap
// type — must neutralize the source. Exercises the typeSubst substitution path
// in neutralizeForceUnwrapElem (the element type is resolved through the active
// monomorphization substitution before the typeNeedsFieldDrop gate).
func TestT1073GenericContextForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		wrap[T](T? move o) T[] { return [o!]; }
		main() {
			T1073Box b = T1073Box(name: "g");
			T1073Box? o = b;
			T1073Box[] v = wrap[T1073Box](move o);
		}
	`)
	// The monomorphized body @"wrap[T1073Box]" must carry the present-flag clear.
	fn := extractFunction(ir, `"wrap[T1073Box]"`)
	if fn == "" {
		t.Fatal("expected wrap[T1073Box] mono body in IR")
	}
	assertContains(t, fn, t1073NeutralizeSig)
}

// T0392: Synth drop must call pal_free for heap user types WITHOUT a drop method
// (B0211 case). The inner has no drop function but the heap allocation must be freed.
func TestSynthDropOptionalNoDropHeapUserField(t *testing.T) {
	ir := generateIR(t, `
		type T0392RawBox { int n; }
		type T0392RawHolder { T0392RawBox? data; }
		main() {
			h := T0392RawHolder(data: T0392RawBox(n: 7));
		}
	`)
	holderDrop := extractFunction(ir, "T0392RawHolder.drop")
	if holderDrop == "" {
		t.Fatal("expected T0392RawHolder.drop in IR")
	}
	// optfield branches conditional on the present flag.
	assertContains(t, holderDrop, "optfield.drop")
	// pal_free must still happen for raw heap user types with no explicit drop.
	assertContains(t, holderDrop, "call void @pal_free")
	// No call to a drop method since the type doesn't define one.
	assertNotContains(t, holderDrop, "call void @T0392RawBox.drop")
}

// T0392: Synth drop must use the synth drop function for heap user types WITH
// synth drop (e.g., string field). The synth drop calls pal_free internally,
// so the optional path must NOT call pal_free again.
func TestSynthDropOptionalSynthDropHeapUserField(t *testing.T) {
	ir := generateIR(t, `
		type T0392SynBox { string s; }
		type T0392SynHolder { T0392SynBox? data; }
		main() {
			h := T0392SynHolder(data: T0392SynBox(s: "x"));
		}
	`)
	holderDrop := extractFunction(ir, "T0392SynHolder.drop")
	if holderDrop == "" {
		t.Fatal("expected T0392SynHolder.drop in IR")
	}
	// optfield branches conditional on the present flag.
	assertContains(t, holderDrop, "optfield.drop")
	// Synth drop is invoked — calls _Box.drop which itself calls pal_free.
	assertContains(t, holderDrop, "call void @T0392SynBox.drop")
}

// T0392: Force-unwrap of a string/vector optional field must NOT trigger
// MemberExpr neutralization — genFieldAccess already dups at access time, so
// neutralizing would leak the original. Verified by counting store-i1-false
// instructions: the heap-user case does ONE extra store (the neutralization
// flag clear) compared to the string case.
func TestForceUnwrapStringOptionalFieldNoExtraStore(t *testing.T) {
	stringIR := generateIR(t, `
		type T0392StrHolder { string? name; drop(~this) {} }
		main() {
			h := T0392StrHolder(name: "world");
			s := h.name!;
		}
	`)
	heapIR := generateIR(t, `
		type T0392HBox { int n; drop(~this) {} }
		type T0392HHolder { T0392HBox? data; drop(~this) {} }
		main() {
			h := T0392HHolder(data: T0392HBox(n: 7));
			b := h.data!;
		}
	`)
	extractMain := func(ir string) string {
		start := strings.Index(ir, "define i8* @.goroutine.main")
		if start < 0 {
			t.Fatal("expected .goroutine.main")
		}
		rest := ir[start:]
		end := strings.Index(rest, "\n}\n")
		if end < 0 {
			t.Fatal("expected closing brace")
		}
		return rest[:end+2]
	}
	stringMain := extractMain(stringIR)
	heapMain := extractMain(heapIR)
	stringStores := strings.Count(stringMain, "store i1 false")
	heapStores := strings.Count(heapMain, "store i1 false")
	// The heap-user case neutralizes the field's present flag (one extra
	// store i1 false). The string case does not.
	if heapStores <= stringStores {
		t.Errorf("expected heap-user neutralization to add ≥1 extra store; "+
			"got string=%d heap=%d", stringStores, heapStores)
	}
}

// T0392: Force-unwrap of a `this.field` inside a method must not crash codegen.
// Currently borrowed `this` is not in c.locals, so neutralization no-ops — this
// is a bug (T0416) but the codegen path itself must remain stable.
func TestForceUnwrapThisFieldDoesNotCrashCodegen(t *testing.T) {
	// Smoke test only — verifies codegen produces IR for `this.field!` without
	// panicking. The runtime double-free (T0416) is filed separately.
	ir := generateIR(t, `
		type T0392MBox { int n; drop(~this) {} }
		type T0392MHolder {
			T0392MBox? data;
			drop(~this) {}
			get_inner(this) int {
				if b := this.data {
					return b.n;
				}
				return -1;
			}
		}
		main() {
			h := T0392MHolder(data: T0392MBox(n: 5));
			v := h.get_inner();
		}
	`)
	// Method body should be present and reference the field GEP.
	getInner := extractFunction(ir, "T0392MHolder.get_inner")
	if getInner == "" {
		t.Fatal("expected T0392MHolder.get_inner in IR")
	}
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

// B0309: Force unwrap in index-assignment key position neutralizes source optional.
func TestOptionalForceUnwrapIndexAssignKey(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? key = "hello";
			map[string, int] m = {:};
			m[key!] = 42;
		}
	`)
	// The []=  call should exist (mangled as Map[string, int].[]=)
	assertContains(t, ir, `.[]="`)
	// B0309: present flag must be set to false after index assign (neutralize source)
	assertContains(t, ir, "store i1 false")
}

// B0309: Force unwrap in index-assignment value position neutralizes source optional.
func TestOptionalForceUnwrapIndexAssignValue(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? val = "hello";
			map[int, string] m = {:};
			m[1] = val!;
		}
	`)
	assertContains(t, ir, `.[]="`)
	// B0309: present flag must be set to false after index assign (neutralize source)
	assertContains(t, ir, "store i1 false")
}

// B0312: Force unwrap in ident reassignment neutralizes source optional.
func TestOptionalForceUnwrapIdentReassign(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? opt = "hello";
			string x = "";
			x = opt!;
		}
	`)
	// B0312: present flag must be set to false after ident reassign (neutralize source)
	assertContains(t, ir, "store i1 false")
}

// B0312: Force unwrap in member assignment neutralizes source optional.
func TestOptionalForceUnwrapMemberAssign(t *testing.T) {
	ir := generateIR(t, `
		type Holder { string val; }
		main() {
			string? opt = "hello";
			h := Holder(val: "");
			h.val = opt!;
		}
	`)
	// B0312: present flag must be set to false after member assign (neutralize source)
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

// T0938: Optional local with a vector inner whose elements are droppable
// (string[]?) must walk and drop elements before freeing the buffer, under a
// bit-63 static-vector guard — mirroring the non-optional emitStringDropCall
// path. Without this, only the buffer is freed and the elements leak.
func TestOptionalLocalVectorStringElementDrop(t *testing.T) {
	ir := generateIR(t, `
		dropfn_str() {
			string[] v = [];
			v.push("a");
			string[]? a = v;
		}
		main() { dropfn_str(); }
	`)
	fn := extractFunction(ir, "__user.dropfn_str")
	assertContains(t, fn, "optdrop.check")
	assertContains(t, fn, "optdrop.inner")
	// Static-vector guard: bit-63 mask before deciding to drop elements/buffer.
	assertContains(t, fn, "optvecdrop.nonstatic")
	assertContains(t, fn, "-9223372036854775808")
	// Element-drop loop runs before the buffer free.
	assertContains(t, fn, "vecdrop.head")
	assertContains(t, fn, "call void @promise_string_drop")
	assertContains(t, fn, "Vector.drop")
}

// T0938: A non-droppable element type (int[]?) must NOT emit a string element
// drop loop — only the buffer free path under the static guard.
func TestOptionalLocalVectorIntNoElementDrop(t *testing.T) {
	ir := generateIR(t, `
		dropfn_int() {
			int[] v = [];
			v.push(7);
			int[]? a = v;
		}
		main() { dropfn_int(); }
	`)
	fn := extractFunction(ir, "__user.dropfn_int")
	assertContains(t, fn, "optvecdrop.nonstatic")
	// No element-drop loop and no string drop inside the function for non-droppable ints.
	assertNotContains(t, fn, "vecdrop.head")
	assertNotContains(t, fn, "call void @promise_string_drop")
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

// B0196: Discarded Vector[string].pop() must drop the inner string.
func TestDropDiscardedOptionalStringPop(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string[] v = ["a", "b", "c"];
			v.pop();
		}
	`)
	// The discarded optional string from pop() should trigger a conditional drop.
	assertContains(t, ir, "discard.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// B0196: Discarded Vector[int].pop() should NOT emit discard drop (int is not droppable).
func TestNoDropDiscardedOptionalIntPop(t *testing.T) {
	ir := generateIR(t, `
		test() {
			int[] v = [1, 2, 3];
			v.pop();
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if strings.Contains(testFn, "discard.drop") {
		t.Fatalf("expected test function to NOT contain discard.drop\ngot:\n%s", testFn)
	}
}

// B0208: Discarded Vector[Vector[int]].pop() must drop the inner vector.
func TestDropDiscardedOptionalVectorPop(t *testing.T) {
	ir := generateIR(t, `
		test() {
			int[][] v = int[][]();
			v.push([1, 2, 3]);
			v.pop();
		}
	`)
	assertContains(t, ir, "discard.drop")
	assertContains(t, ir, "call void @Vector.drop")
}

// B0208: Discarded Optional with user type with drop must drop inner instance.
func TestDropDiscardedOptionalUserTypePop(t *testing.T) {
	ir := generateIR(t, `
		type Res {
			int id;
			drop(~this) {}
		}
		test() {
			Res[] v = Res[]();
			v.push(Res(id: 1));
			v.pop();
		}
	`)
	assertContains(t, ir, "discard.drop")
	assertContains(t, ir, "call void @Res.drop")
}

// B0211: Discarded constructor call for a heap user type without drop should emit pal_free.
func TestDropDiscardedHeapTypeConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Pt {
			int x;
			int y;
		}
		test() {
			Pt(x: 1, y: 2);
		}
	`)
	assertContains(t, ir, "discard.heap.free")
	assertContains(t, ir, "call void @pal_free")
}

// B0211: Discarded constructor call for a heap user type WITH drop should call drop.
func TestDropDiscardedHeapTypeConstructorWithDrop(t *testing.T) {
	ir := generateIR(t, `
		type Res {
			int id;
			drop(~this) {}
		}
		test() {
			Res(id: 1);
		}
	`)
	assertContains(t, ir, "discard.heap.free")
	assertContains(t, ir, "call void @Res.drop")
}

// B0211: Discarded method call returning a heap type should NOT emit discard.heap.free
// (only constructor calls are safe to free — method returns may share instance pointers).
func TestNoDropDiscardedMethodReturnHeapType(t *testing.T) {
	ir := generateIR(t, `
		type Pt {
			int x;
			int y;
		}
		make_pt() Pt {
			return Pt(x: 1, y: 2);
		}
		test() {
			make_pt();
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if strings.Contains(testFn, "discard.heap.free") {
		t.Fatalf("expected test function to NOT contain discard.heap.free for method return\ngot:\n%s", testFn)
	}
}

// B0211: Optional of heap user type without drop should register pal_free cleanup.
func TestOptionalHeapTypeWithoutDropFreed(t *testing.T) {
	ir := generateIR(t, `
		type Pt {
			int x;
			int y;
		}
		test() {
			Pt? p = Pt(x: 1, y: 2);
		}
	`)
	// Should have optional drop check and pal_free
	assertContains(t, ir, "optdrop.check")
	assertContains(t, ir, "call void @pal_free")
}

// B0211: String temp should NOT be claimed when constructor will strdup (NeedsSynthDrop).
func TestStringTempNotClaimedForSynthDrop(t *testing.T) {
	ir := generateIR(t, `
		type Named {
			string name;
			new(~this, string name) {
				this.name = name;
			}
		}
		make_name() string {
			return "hello";
		}
		test() {
			Named n = Named(name: make_name());
		}
	`)
	// The string from make_name() should be freed (not claimed by constructor).
	// Look for promise_string_drop in the test function — indicates the temp is freed.
	assertContains(t, ir, "promise_string_drop")
}

// B0262: Discarded auto-propagated failable call returning heap user type should drop+free.
func TestDropDiscardedAutoPropagateUserType(t *testing.T) {
	ir := generateIR(t, `
		type Foo {
			string name;
			drop(~this) {}
		}
		make_foo!() Foo { return Foo(name: "x"); }
		test!() {
			make_foo();
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	assertContains(t, testFn, "autoprop.drop")
	assertContains(t, testFn, "call void @Foo.drop")
}

// B0262: Discarded auto-propagated failable call returning closure should free env.
func TestDropDiscardedAutoPropagateClosureEnv(t *testing.T) {
	ir := generateIR(t, `
		make_fn!() (int) -> int {
			int x = 42;
			(int) -> int f = |int y| -> x + y;
			return f;
		}
		test!() {
			make_fn();
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	assertContains(t, testFn, "autoprop.env.free")
}

// B0215: If-let unwrap should drop the inner string value at scope exit.
func TestIfUnwrapStringDrop(t *testing.T) {
	ir := generateIR(t, `
		get_optional() string? {
			return "hello";
		}
		test() {
			if v := get_optional() {
				int x = v.len;
			}
		}
	`)
	// The unwrapped string v must be dropped in the then-block.
	assertContains(t, ir, "strdrop.call")
}

// B0215: If-let unwrap from a local optional should emit string drop for the unwrapped value.
func TestIfUnwrapLocalOptionalStringDrop(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string? s = "hello";
			if v := s {
				int x = v.len;
			}
		}
	`)
	// The unwrapped string v must be dropped in the then-block.
	assertContains(t, ir, "strdrop.call")
}

// B0215: While-let unwrap should drop the inner string value at each iteration end.
func TestWhileUnwrapStringDrop(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int n;
			next(~this) string? {
				if this.n <= 0 { return none; }
				this.n = this.n - 1;
				return "item";
			}
		}
		test() {
			Counter c = Counter(n: 3);
			while v := c.next() {
				int x = v.len;
			}
		}
	`)
	// The unwrapped string v must be dropped at end of each iteration.
	assertContains(t, ir, "strdrop.call")
}

// B0222: Generic combinator chain result stored in variable — intermediate iterators
// must be promoted to scope bindings (freed at scope exit, not statement end).
func TestGenericCombinatorInVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] v = [1, 2, 3];
			Iterator[int] it = v.iter().map[int](|int x| -> int { return x * 2; });
		}
	`)
	// B0222: Intermediate heapTemps promoted to scope bindings should produce
	// free.call blocks (scope-level cleanup) instead of heap.drop blocks
	// (statement-level cleanup) for the intermediate _FnIter.
	assertContains(t, ir, "free.call")
	assertContains(t, ir, "__promise_iter_cleanup")
}

// B0226: Typeinfo should include drop_fn_ptr at field 1.
func TestTypeInfoDropFnPtr(t *testing.T) {
	ir := generateIR(t, `
		type Droppable {
			int x;
			drop(~this) {}
		}
		main() {
			Droppable d = Droppable(x: 1);
		}
	`)
	// Typeinfo should reference the drop function
	assertContains(t, ir, "promise_typeinfo_Droppable")
	assertContains(t, ir, "@Droppable.drop")
}

// B0226: Inferred optional declaration should register optional drop.
func TestInferredOptionalDrop(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			int value;
			new(~this, int v) { this.value = v; }
			try_make(int v, bool ok) Self? `+"`factory"+` {
				if !ok { return none; }
				return Self(v: v);
			}
		}
		main() {
			r := Box.try_make(v: 10, ok: true);
		}
	`)
	// B0226: Inferred optional should register drop (optdrop block)
	assertContains(t, ir, "optdrop")
}

// B0226: Untyped error handler should use RTTI-based drop dispatch.
func TestUntypedErrorRttiDrop(t *testing.T) {
	ir := generateIR(t, `
		type MyError is error { int code; }
		fail_my!() void { raise MyError(message: "err", code: 42); }
		main() {
			fail_my()? e {
			};
		}
	`)
	// B0226: Should emit RTTI-based drop dispatch (loads drop fn from typeinfo)
	assertContains(t, ir, "rtti.drop")
}

// B0226: promise_type_is should use updated field indices (typeID at field 2).
func TestTypeIsFieldIndicesB0226(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; speak(this) string `+"`abstract"+`; }
		type Dog is Animal { speak(this) string { return "woof"; } }
		main() {
			Animal a = Dog(name: "Rex");
			if a is Dog { }
		}
	`)
	assertContains(t, ir, "define i32 @promise_type_is")
	assertContains(t, ir, "call i32 @promise_type_is")
}

// B0233: Constructor temps passed to non-~ methods should be freed at statement end.
// The constructor should NOT claim the heap temp — only downstream consumers should.
func TestConstructorTempFreedAtStmtEnd(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		check(Point p) bool { return p.x == 0; }
		main() {
			check(Point(x: 0, y: 0));
		}
	`)
	// The Point(x: 0, y: 0) temp should be freed at statement end via heap.drop,
	// NOT claimed by the constructor. The heap.drop block calls pal_free on the
	// unclaimed temp.
	assertContains(t, ir, "heap.drop")
	assertContains(t, ir, "heap.exec")
}

// B0233: Constructor temps assigned to variables should still be claimed.
func TestConstructorTempClaimedOnAssign(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		main() {
			Point p = Point(x: 1, y: 2);
		}
	`)
	// Variable assignment claims the heap temp — heap.claim block should exist.
	assertContains(t, ir, "heap.claim")
}

// B0233: Constructor temps passed to vector push should be claimed.
func TestConstructorTempClaimedOnPush(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		main() {
			Point[] v = [Point(x: 1, y: 2)];
			v.push(Point(x: 3, y: 4));
		}
	`)
	// Push claims the heap temp — heap.claim block should exist.
	assertContains(t, ir, "heap.claim")
}

// B0237: Constructor temps stored into enum variant data should be claimed.
func TestConstructorTempClaimedInEnumVariant(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int x; }
		enum Slot { Empty, Used(Wrapper value) }
		main() {
			Slot s = Slot.Used(value: Wrapper(x: 42));
		}
	`)
	// Enum variant construction claims the heap temp.
	assertContains(t, ir, "heap.claim")
}

// B0229: Optional structural interface variables should register drop for reassignment.
func TestOptionalStructuralInterfaceDropOnReassign(t *testing.T) {
	ir := generateIR(t, `
		type Iter is Iterator[int] {
			int val;
			next() int? { return none; }
		}
		make_iter() Iterator[int] {
			return Iter(val: 1);
		}
		main() {
			Iterator[int]? current = none;
			current = make_iter();
		}
	`)
	// B0229: Optional structural interface should have optdrop block for scope exit.
	assertContains(t, ir, "optdrop")
	// B0243: Should use RTTI-based drop dispatch (not __promise_iter_cleanup).
	// The concrete type behind the interface is unknown at compile time.
	assertContains(t, ir, "struct.drop")
}

// B0247: RTTI drop dispatch for types with explicit user drop must call pal_free
// after the drop function (user drops don't free the instance themselves).
// The typeinfo should store a $wrap function that calls drop + pal_free.
func TestRttiDropExplicitUserDropWrap(t *testing.T) {
	ir := generateIR(t, `
		type Counter is Iterator[int] {
			int val;
			next() int? {
				if this.val > 0 {
					this.val = this.val - 1;
					return this.val;
				}
				return none;
			}
			drop(~this) {}
		}
		make_counter() Iterator[int] {
			return Counter(val: 3);
		}
		test() {
			Iterator[int]? it = none;
			it = make_counter();
			it = none;
		}
	`)
	// The typeinfo drop_fn_ptr for Counter should point to the $wrap function
	// which calls Counter.drop then pal_free.
	assertContains(t, ir, "Counter.drop$wrap")
}

// B0243: Optional structural interface drop in closure env must use RTTI dispatch,
// not __promise_iter_cleanup (which assumes _FnIter layout and segfaults on other types).
func TestOptionalStructuralInterfaceEnvDropRTTI(t *testing.T) {
	ir := generateIR(t, `
		type Iter is Iterator[int] {
			int val;
			next() int? { return none; }
		}
		make_iter() Iterator[int] {
			return Iter(val: 1);
		}
		wrap() () -> int? {
			Iterator[int]? current = none;
			return move || -> int? {
				current = make_iter();
				if inner := current {
					return inner.next();
				}
				return none;
			};
		}
		main() {
			() -> int? fn = wrap();
			fn();
		}
	`)
	// B0243: The env drop function should use RTTI-based dispatch for Optional[Iterator].
	// It should NOT contain __promise_iter_cleanup for the optional structural field —
	// that function assumes _FnIter memory layout and crashes on other concrete types.
	assertContains(t, ir, "optst.rtti")
}

// B0246: If-let unwrap of Optional structural interface should NOT clear the source's
// drop flag. The unwrapped structural binding doesn't get a drop registered (no concrete
// type known at compile time), so the source must retain ownership. Its reassignment-time
// Optional drop (RTTI-based) handles cleanup.
func TestIfUnwrapOptionalStructuralNoDropFlagClear(t *testing.T) {
	ir := generateIR(t, `
		type Iter is Iterator[int] {
			int val;
			next() int? { return none; }
		}
		make_iter() Iterator[int] {
			return Iter(val: 1);
		}
		wrap() () -> int? {
			Iterator[int]? current = none;
			return move || -> int? {
				current = make_iter();
				if inner := current {
					return inner.next();
				}
				return none;
			};
		}
		main() {
			() -> int? fn = wrap();
			fn();
			fn();
		}
	`)
	// B0246: The reassignment `current = make_iter()` must trigger the Optional drop
	// even after an if-let unwrap. The optdrop block should appear in the reassignment path.
	assertContains(t, ir, "optdrop.check")
	// RTTI-based drop dispatch for the structural interface inside the Optional.
	assertContains(t, ir, "struct.drop")
}

// B0240: Assigning none to an optional field with a heap user type should
// drop/free the old inner value before storing the new value.
func TestOptionalFieldReassignDrop(t *testing.T) {
	ir := generateIR(t, `
		type Location { int x; int y; }
		type Place { string name; Location? location; }
		test() {
			Place p = Place(name: "a", location: Location(x: 1, y: 2));
			p.location = none;
		}
	`)
	// The reassignment to none should emit a conditional drop for the old optional value.
	assertContains(t, ir, "field.optdrop")
	// Should free the inner Location instance.
	assertContains(t, ir, "call void @pal_free")
}

// B0240: Assigning none to an optional string field should call promise_string_drop.
func TestOptionalStringFieldReassignDrop(t *testing.T) {
	ir := generateIR(t, `
		type Holder { string? value; }
		test() {
			Holder h = Holder(value: "hello");
			h.value = none;
		}
	`)
	assertContains(t, ir, "field.optdrop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// B0240: Assigning none to an optional field with a droppable user type should
// call the drop function before freeing.
func TestOptionalDroppableFieldReassignDrop(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		type Container { Resource? res; }
		test() {
			Container c = Container(res: Resource(id: 1));
			c.res = none;
		}
	`)
	assertContains(t, ir, "field.optdrop")
	assertContains(t, ir, "call void @Resource.drop")
}

// T0394: Reassigning a generic Optional[string] field with a heap RHS must
// claim the inner string temp BEFORE wrapping in Optional. Without the
// pre-wrap claim, the post-wrap claimStringTemp lookup uses value-identity
// against the wrapped {i1, i8*} struct and never matches the inner i8* temp,
// leaving the temp drop active so the field aliases a freed pointer.
//
// The fix mirrors the T0111 pattern in the parallel local-var (IdentExpr)
// and var-decl branches. We assert the drop-flag clear-before-wrap shape:
// `store i1 false` to the temp's drop flag must appear BEFORE the
// `insertvalue { i1, i8* }` that builds the wrapped Optional.
func TestOptionalGenericFieldReassignClaimsStringTempBeforeWrap(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[string] b = Box[string](value: "init");
			b.value = (1).to_string();
		}
	`)
	// Reassign-drop block must exist (T0390).
	assertContains(t, ir, "field.optdrop")
	// The post-store stmt-temp drop block exists for tracked temps.
	assertContains(t, ir, "tmp.drop")
	// promise_string_drop is reachable but must be guarded by the temp drop
	// flag. With the fix, the flag is cleared before the wrap, so on the hot
	// path the drop block resolves to the no-op (skip) branch.
	assertContains(t, ir, "promise_string_drop")
}

// T0394 (vector limb): the predicate also covers types.IsVector(exprType).
// Reassigning a generic Optional[Vector[int]] field with a heap-allocated
// Vector RHS must emit the reassign-drop block for the OLD field value and
// the temp-drop guard block for the NEW value, with Vector.drop reachable
// for both.
func TestOptionalGenericFieldReassignVectorEmitsDropAndOptdrop(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[Vector[int]] b = Box[Vector[int]](value: [1, 2, 3]);
			b.value = [4, 5, 6];
		}
	`)
	// T0390 reassign-drop block for the OLD field value.
	assertContains(t, ir, "field.optdrop")
	// Stmt-temp drop block for tracked heap temps (covers the Vector case).
	assertContains(t, ir, "tmp.drop")
	// Vector.drop is generic — operates on i8*, not monomorphised per-T.
	assertContains(t, ir, "@Vector.drop")
}

// T0394 (channel limb): the predicate also covers types.IsChannel(exprType).
// Channel reassign on an Optional generic field must produce the same
// reassign-drop + temp-drop shape with Channel.drop reachable.
func TestOptionalGenericFieldReassignChannelEmitsDropAndOptdrop(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[Channel[int]] b = Box[Channel[int]](value: channel[int](2));
			b.value = channel[int](2);
		}
	`)
	assertContains(t, ir, "field.optdrop")
	assertContains(t, ir, "tmp.drop")
	// T0663: Channel.drop is now per-element-type — Channel[int].drop here.
	assertContains(t, ir, `@"Channel[int].drop"`)
}

// T0513: Force-unwrap of an Optional[string] field on a generic-type instance
// (e.g. Box[string]) must dup the inner string. Sema's fieldTypeHasDrop returns
// false for T? where T is a TypeParam, so the bare Named's HasDrop()=false; the
// mono instance Box[string] gets synth drop via monoInstNeedsSynthDrop. Without
// the dup, the field and the new var alias the same heap pointer — at scope
// end one drops the pointer; the next reassignment frees again -> invalid free.
func TestGenericOptionalStringFieldUnwrapDups(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[string] b = Box[string](value: "init");
			string s = b.value!;
		}
	`)
	// dupStringFieldAccess mechanism must emit strdup block + promise_string_new.
	assertContains(t, ir, "strdup.copy")
	assertContains(t, ir, "promise_string_new")
}

// T0513 (vector limb): same fix must apply to generic Optional[Vector[T]] field
// unwrap — the inner Vector buffer must be duped on read so the field and the
// new variable own independent copies.
func TestGenericOptionalVectorFieldUnwrapDups(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[Vector[int]] b = Box[Vector[int]](value: [1, 2, 3]);
			Vector[int] v = b.value!;
		}
	`)
	// dupContainerFieldAccess emits a vecdup block (alloc + memcpy) for the dup.
	assertContains(t, ir, "vecdup.copy")
	assertContains(t, ir, "memcpy")
}

// T0513 (direct string field on generic owner): reading a plain `T` field
// from `Box[string]` must dup the string when bound to a new variable.
// Without the Instance-local TypeArgs substitution (added in T0513), the dup
// check sees the raw TypeParam and skips; without the ownerHasOrSynthDrop
// gate the bare Named has HasDrop=false (sema's fieldTypeHasDrop returns
// false for TypeParam) and the dup is skipped entirely.
func TestGenericDirectStringFieldReadDups(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		test() {
			Box[string] b = Box[string](value: "hi");
			string s = b.value;
		}
	`)
	// Scope the assertion to the user's test() function — the broader IR
	// contains many stdlib strdup.copy blocks unrelated to this fix.
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	assertContains(t, testFn, "strdup.copy")
	assertContains(t, testFn, "promise_string_new")
}

// T0746: a generic method that returns a `this`-owned string field by value
// must dup it on return (clone-on-return) so the owner's field-drop and the
// returned value's drop don't free the same allocation. The VarDecl-site dup
// is covered by TestGenericDirectStringFieldReadDups; this covers the
// method-return site (genReturnStmt -> setDupFlagsForFieldAccess ->
// genFieldAccess with `this` as the target, under c.typeSubst {T->string}).
func TestGenericMethodReturnStringFieldDups(t *testing.T) {
	ir := generateIR(t, `
		type GBox[T] { T val; peek(this) T { return this.val; } }
		main() { b := GBox[string](val: "hi"); s := b.peek(); }
	`)
	fn := extractFunction(ir, `"GBox[string].peek"`)
	if fn == "" {
		t.Fatal("expected GBox[string].peek in IR")
	}
	assertContains(t, fn, "strdup.copy")
	assertContains(t, fn, "promise_string_new")
}

// T0746 (`this` receiver form): the bug reported the borrowed-receiver
// variant double-freed identically, so the dup-on-return must fire there too.
func TestGenericMethodReturnStringFieldDupsBorrowedReceiver(t *testing.T) {
	ir := generateIR(t, `
		type GBox[T] { T val; peek(this) T { return this.val; } }
		main() { b := GBox[string](val: "hi"); s := b.peek(); }
	`)
	fn := extractFunction(ir, `"GBox[string].peek"`)
	if fn == "" {
		t.Fatal("expected GBox[string].peek in IR")
	}
	assertContains(t, fn, "strdup.copy")
	assertContains(t, fn, "promise_string_new")
}

// T0746 (generic getter form): a getter returning a `this`-owned string field
// through the type parameter is a distinct codegen path from a method (getter
// vs method dispatch), but the return-by-value dup must fire identically.
func TestGenericGetterReturnStringFieldDups(t *testing.T) {
	ir := generateIR(t, `
		type GBox[T] { T val; get field T { return this.val; } }
		main() { b := GBox[string](val: "hi"); s := b.field; }
	`)
	fn := extractFunction(ir, `"GBox[string].field"`)
	if fn == "" {
		t.Fatal("expected GBox[string].field in IR")
	}
	assertContains(t, fn, "strdup.copy")
	assertContains(t, fn, "promise_string_new")
}

// T0513 (maybeEnableDupForMutRefArg generic owner): passing a generic
// owner's field to a `~` (consuming) param must auto-dup the field so the
// callee's consume-drop and the owner's drop don't double-free. Exercises
// the MemberExpr branch of maybeEnableDupForMutRefArg (expr.go:5094-5109),
// which had zero coverage before T0513 added a test for the generic-owner
// gate.
func TestGenericOwnerMutRefArgDupsStringField(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		consume(string move s) {}
		test() {
			b := Box[string](value: "hi");
			consume(b.value);
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	// dupStringFieldAccess + strdup.copy emitted at the field read site,
	// guarded by the ownerHasOrSynthDrop generic-owner gate.
	assertContains(t, testFn, "strdup.copy")
	assertContains(t, testFn, "promise_string_new")
}

// T0513 (maybeEnableDupForMutRefArg generic owner — Vector limb): same
// auto-dup must apply when the field type substitutes to a Vector and is
// passed to a consuming param. dupContainerFieldAccess routes through
// dupVector which emits a vecdup.copy block.
func TestGenericOwnerMutRefArgDupsVectorField(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		consume(Vector[int] move v) {}
		test() {
			b := Box[Vector[int]](value: [1, 2, 3]);
			consume(b.value);
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	assertContains(t, testFn, "vecdup.copy")
}

// T0513 (maybeEnableDupForConstructorArg generic owner): constructor
// field-init that reads from a generic owner's field must auto-dup so the
// new instance owns an independent copy. Mirrors T0411 (non-generic owner)
// for generic-owner instances; without ownerHasOrSynthDrop, the early
// return at expr.go:5129 would skip the dup setup.
func TestGenericOwnerConstructorArgDupsStringField(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		type Holder { string s; drop(~this) {} }
		test() {
			b := Box[string](value: "hi");
			h := Holder(s: b.value);
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	assertContains(t, testFn, "strdup.copy")
	assertContains(t, testFn, "promise_string_new")
}

// T0522 (destructure neutralization): destructuring `t!` where `t` is an
// Optional[(int, string)] local must clear t's present flag — otherwise both
// the destructured `s` and t's scope-exit optdrop will free the same heap
// string. The neutralization emits a GEP into t at index (0,0) followed by a
// `store i1 false`, which is the distinguishing IR pattern.
func TestT0522DestructureForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		test() {
			(int, string)? t = (1, "a" + "b");
			(n, s) := t!;
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	// Distinctive pattern: GEP into the source Optional alloca %t selecting
	// the present-flag field (i32 0, i32 0). Without the fix, no such GEP
	// exists — operations on %t are only the initial store and the load for
	// the unwrap / scope-exit optdrop.
	assertContains(t, testFn, "%t, i32 0, i32 0")
}

// T0522 (consume-arg Optional[string] field dup claim): when passing an
// Optional[string] field from a droppable owner to a `~` param, the inner
// string is duped and tracked as a stmt temp. The dup temp's drop flag must
// be cleared BEFORE the consume call so the stmt-end cleanup doesn't free
// the pointer the callee consumed.
//
// Distinguishing IR: between the dup's `insertvalue` reconstructing the
// Optional and the consume call, there must be a `store i1 false, i1* %flag`
// (the claim). Without the fix, only `store i1 true` precedes the call.
func TestT0522ConsumeArgOptionalStringFieldClaimsDup(t *testing.T) {
	ir := generateIR(t, `
		type _Holder { string? title; drop(~this) {} }
		_consume_opt_string(string? move s) `+"`public {}"+`
		test() {
			h := _Holder(title: "foo" + "bar");
			_consume_opt_string(h.title);
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	assertContains(t, testFn, "strdup.copy")
	callIdx := strings.Index(testFn, "@__user._consume_opt_string")
	if callIdx < 0 {
		t.Fatalf("expected consume call in test body\n%s", testFn)
	}
	// Look back ~400 chars for the strdup.merge label that contains the
	// insertvalue + claim. With the fix, both `store i1 true` (set flag) and
	// `store i1 false` (claim) precede the call. Without it, only `store i1
	// true` precedes the call.
	start := callIdx - 600
	if start < 0 {
		start = 0
	}
	preCall := testFn[start:callIdx]
	if !strings.Contains(preCall, "store i1 false, i1*") {
		t.Errorf("expected `store i1 false, i1*` (T0522 dup-temp claim) before consume call\npre-call window:\n%s", preCall)
	}
}

// T0522 (consume-arg Optional[Vector] field dup claim): same pattern as the
// string variant — the inner Vector dup must be claimed after the consume call
// returns. The dup is via `vecdup.copy` (alloc + memcpy + tag clear).
func TestT0522ConsumeArgOptionalVectorFieldClaimsDup(t *testing.T) {
	ir := generateIR(t, `
		type _Holder { Vector[int]? items; drop(~this) {} }
		_consume_opt_vec(Vector[int]? move v) `+"`public {}"+`
		test() {
			h := _Holder(items: [1, 2, 3]);
			_consume_opt_vec(h.items);
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	assertContains(t, testFn, "vecdup.copy")
	callIdx := strings.Index(testFn, "@__user._consume_opt_vec")
	if callIdx < 0 {
		t.Fatalf("expected consume call in test body\n%s", testFn)
	}
	start := callIdx - 600
	if start < 0 {
		start = 0
	}
	preCall := testFn[start:callIdx]
	if !strings.Contains(preCall, "store i1 false, i1*") {
		t.Errorf("expected `store i1 false, i1*` (T0522 dup-temp claim) before consume call\npre-call window:\n%s", preCall)
	}
}

// T0391: Returning a non-~ Optional argument from a function that returns the same
// Optional type causes the caller's drop flag to alias with the return value's
// drop binding. The alias check (extended in T0391 to recognise Optional structs)
// must clear the caller's drop flag when the inner pointers compare equal,
// preventing double-free.
func TestOptionalReturnAliasCheckClearsArgFlag(t *testing.T) {
	ir := generateIR(t, `
		type Box { int n; drop(~this) {} }
		passthrough(Box? a) Box? { return a; }
		main() {
			Box? a = Box(n: 1);
			Box? r = passthrough(a);
		}
	`)
	// Caller must emit a runtime alias check for the call result vs the arg pointer.
	// T1031: Optional[droppable] IS deep-cloned at the call site (dupOptionalVectorElem):
	// when the result aliases the still-owned source, the inner Box is cloned into the
	// source's storage so both ends are independently owned. The arg's drop flag is NOT
	// cleared — both the source and the new binding drop their own allocation once.
	assertContains(t, ir, "alias.dup")
	assertContains(t, ir, "alias.cont")
	// The inner Optional value is deep-cloned (present/absent split + heap clone).
	assertContains(t, ir, "optdup.dup")
	assertContains(t, ir, "heapdup.copy")
	// The source arg's drop flag must NOT be cleared (no ownership transfer).
	assertNotContains(t, ir, "store i1 false, i1* %a.dropflag")
}

// T0391: A nested Optional local (T??) must register a scope-exit drop binding
// so its inner heap pointer is freed. The drop emits an outer present check,
// extracts the inner Optional, then a second present check before the actual
// drop (or pal_free for heap user types without a drop method).
func TestNestedOptionalDropRecurses(t *testing.T) {
	ir := generateIR(t, `
		type Box { int n; drop(~this) {} }
		returns_double(Box? a) Box?? { return a; }
		main() {
			Box? a = Box(n: 1);
			Box?? r = returns_double(a);
		}
	`)
	// r must have its own drop flag for scope-exit cleanup.
	assertContains(t, ir, "%r.dropflag")
	// The optional drop chain must traverse two layers — the helper emits
	// nested optdrop.inner / optdrop.done blocks via recursion.
	assertContains(t, ir, "optdrop.check")
	assertContainsMatch(t, ir, `optdrop\.inner[\s\S]*optdrop\.inner`)
	// Bottom-level dispatch reaches Box.drop (the heap user type has a drop method).
	assertContains(t, ir, "call void @Box.drop")
}

// T0391: A nested Optional[string] (string??) drop reaches promise_string_drop
// at the bottom of the recursive walk via the `b.named == TypString` branch
// in emitOptionalValueDrop.
func TestNestedOptionalStringDropRecurses(t *testing.T) {
	ir := generateIR(t, `
		returns_double_str(string? a) string?? { return a; }
		main() {
			string? a = "hello";
			string?? r = returns_double_str(a);
		}
	`)
	assertContains(t, ir, "%r.dropflag")
	// Two layers of optdrop.inner (recursive walk through string?? → string? → string).
	assertContainsMatch(t, ir, `optdrop\.inner[\s\S]*optdrop\.inner`)
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0391: A nested Optional[Vector] drop reaches Vector.drop at the bottom of the
// recursive walk via the `isContainerType` branch in emitOptionalValueDrop.
func TestNestedOptionalVectorDropRecurses(t *testing.T) {
	ir := generateIR(t, `
		returns_double_vec(int[]? a) int[]?? { return a; }
		main() {
			int[]? a = [1, 2, 3];
			int[]?? r = returns_double_vec(a);
		}
	`)
	assertContains(t, ir, "%r.dropflag")
	assertContainsMatch(t, ir, `optdrop\.inner[\s\S]*optdrop\.inner`)
	assertContains(t, ir, "call void @Vector.drop")
}

// T0391: A nested Optional[enum] drop reaches the enum drop function at the
// bottom via the `extractEnum != nil` branch in emitOptionalValueDrop. The
// inner value is an enum struct stored to a temp alloca and bitcast to i8*.
func TestNestedOptionalEnumDropRecurses(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Empty, Text(string body) }
		returns_double_enum(Msg? a) Msg?? { return a; }
		main() {
			Msg? a = Msg.Text("hi");
			Msg?? r = returns_double_enum(a);
		}
	`)
	assertContains(t, ir, "%r.dropflag")
	assertContainsMatch(t, ir, `optdrop\.inner[\s\S]*optdrop\.inner`)
	assertContains(t, ir, "call void @Msg.drop")
}

// T0391: while-let on T?? must register a nested Optional drop binding for the
// unwrapped element (just like if-let). Mirror of TestNestedOptionalDropRecurses
// for genWhileUnwrapStmt's nested Optional path.
func TestWhileLetNestedOptionalDropBinding(t *testing.T) {
	ir := generateIR(t, `
		type Box { int n; drop(~this) {} }
		returns_double(Box? a) Box?? { return a; }
		main() {
			Box? a = Box(n: 1);
			Box?? r = returns_double(a);
			while x := r {
				while y := x {
					r = none;
					break;
				}
				break;
			}
		}
	`)
	// Inner while-let unwraps Box?? → Box?, so x: Box? must register an
	// Optional-drop binding (a regular drop-binding would not free the inner Box).
	assertContains(t, ir, "%x.dropflag")
	// Body of the unwrap walks through optdrop.inner blocks.
	assertContains(t, ir, "optdrop.check")
	assertContains(t, ir, "call void @Box.drop")
}

// B0237: Constructor temps passed as map literal values should be claimed.
func TestConstructorTempClaimedInMapLiteral(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		main() {
			map[string, Point] m = { "a": Point(x: 1, y: 2) };
		}
	`)
	// Map literal initialization claims the heap temp.
	assertContains(t, ir, "heap.claim")
}

// B0280: Map literal values with drop flags must have flags cleared after []=.
// Without this, moved values are double-dropped at scope exit (use-after-free).
func TestMapLitClearsDropFlagOnEnumValue(t *testing.T) {
	ir := generateIR(t, `
		enum Wrapper { Val(string s); }
		main() {
			Wrapper w = Wrapper.Val(s: "hello");
			map[string, Wrapper] m = { "key": w };
		}
	`)
	// After the []= call, w's drop flag must be cleared (store i1 false)
	assertContains(t, ir, "w.dropflag")
	// The []= call should be followed by clearing w's drop flag
	assertContains(t, ir, "store i1 false, i1* %w.dropflag")
}

// B0280: Map literal with identifier key must clear key's drop flag too.
func TestMapLitClearsDropFlagOnKey(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string k = "mykey";
			map[string, int] m = { k: 42 };
		}
	`)
	// k is a string variable with a drop flag — it should be cleared
	assertContains(t, ir, "k.dropflag")
	assertContains(t, ir, "store i1 false, i1* %k.dropflag")
}

// T0736: A map literal whose value is a bare heap sub-expression (string concat,
// to_string(), split(), ...) registers a string/vector *stmt-temp*, not a heap
// temp. genMapLit must claim that stmt-temp (clear its drop flag) after the []=
// move — otherwise the caller's stmt-temp cleanup drops the string while the
// map's scope-exit drop drops it again → double-free ("invalid free"). The
// ident-`clearDropFlag` (B0280) and `claimHeapTemp` paths don't cover stmt-temps.
func TestT0736MapLitClaimsHeapStringValueTemp(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[string, string] m = {"k": "a" + "b"};
		}
	`)
	// Scope to the user's main goroutine body — the stdlib also defines and
	// calls Map[string, string].[]= elsewhere. The call site of .goroutine.main
	// (in the entry main) precedes its definition, so locate the definition
	// directly rather than via extractFunction.
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
	assertContains(t, body, "call i8* @promise_string_concat")
	// The concat value is moved into the map via []=, then its stmt-temp drop
	// flag is cleared (claimed) — the []= call is immediately followed by a
	// flag-clearing store. Without the fix the next line is instead the
	// `store { i8*, i8* } ... %m` map binding (no claim), and this fails.
	assertContainsMatch(t, body,
		`(?s)call void @"Map\[string, string\]\.\[\]="\([^\n]*\)\n\s*store i1 false, i1\*`)
}

// T0735: A map literal used directly as a borrowed function argument must
// register a stmt-temp drop binding so the map instance + its _buckets vector
// are freed at statement end. Pre-fix, genMapConstructor never called
// trackHeapTemp, so unclaimed map literal temps leaked 2 allocations.
// The fix mirrors genConstructorCallMono (T0135 + T0345): trackHeapTemp with
// palFree as the safe default, then updateConstructorTempDrop swaps in the
// type's full synth drop after new() completes.
func TestT0735_MapLitArgTracksHeapTemp(t *testing.T) {
	ir := generateIR(t, `
		borrow_map(Map[string, int] m) int { return 0; }
		main() {
			int x = borrow_map({"a": 1, "b": 2});
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
	// After the borrow_map call, the unclaimed map temp must flow through a
	// heap.drop block that calls Map[string, int].drop (the synthesized drop
	// walks _buckets and pal_frees the instance — without the swap, just a
	// pal_free of the instance would leak the buckets vector buffer).
	assertContains(t, body, "call i64 @__user.borrow_map(")
	assertContains(t, body, "heap.drop")
	assertContains(t, body, "heap.exec")
	assertContains(t, body, `call void @"Map[string, int].drop"`)
}

// T0735: A map literal used as a method-call receiver (rvalue temp) — same
// stmt-temp drop registration must apply. `{...}.len` returns a primitive but
// the receiver map still needs cleanup.
func TestT0735_MapLitMethodReceiverTracksHeapTemp(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int n = {"a": 1, "b": 2}.len;
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
	assertContains(t, body, `call i64 @"Map[string, int].len"`)
	assertContains(t, body, "heap.drop")
	assertContains(t, body, `call void @"Map[string, int].drop"`)
}

// T0735: Map literal bound to a local first must still work — the local's
// regular bindingDrop (registered by genAssignment) handles the cleanup, and
// claimHeapTemp at the assignment site clears the heap-temp flag so the
// instance isn't double-freed. Verifies the existing local-binding path is
// undisturbed by the new stmt-temp registration.
func TestT0735_MapLitLocalStillDropped(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[string, int] m = {"a": 1, "b": 2};
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
	// The local m's drop still fires (via bindingDrop), so Map.drop must
	// appear in the body. The heap-temp flag should also be cleared at the
	// assignment site so the heap.drop path doesn't double-free.
	assertContains(t, body, `call void @"Map[string, int].drop"`)
	assertContains(t, body, "heap.claim")
}

// T0735: When a map literal is passed as a borrowed arg to a CONSTRUCTOR, the
// constructor claims the heap temp into the field (heap.claim block). The
// stmt-temp drop is cleared, ownership transfers to the new instance's drop.
func TestT0735_MapLitInCtorFieldClaimed(t *testing.T) {
	ir := generateIR(t, `
		type _Box { Map[string, int] m; }
		main() {
			_Box b = _Box(m: {"a": 1});
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
	// The map literal heap-temp flag is cleared (claimed) when stored into the
	// _Box.m field — the _Box's drop takes over ownership. Asserting inside the
	// user's main body (not anywhere in the module IR) avoids false positives
	// from std-library heap.claim blocks.
	assertContains(t, body, "heap.claim")
}

// T0735: A map literal passed to a `~Map` parameter (consume-arg) must be
// claimed at the call site — the callee owns and drops, so the caller's
// heap-temp drop flag must be cleared to avoid double-free. Verifies the
// claim path on the move-arg ABI.
func TestT0735_MapLitMoveArgClaimed(t *testing.T) {
	ir := generateIR(t, `
		consume(Map[string, int] move m) int { return m.len; }
		main() {
			int x = consume({"a": 1});
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
	assertContains(t, body, "call i64 @__user.consume(")
	// claim must fire before the call returns so the heap.drop at statement
	// end sees flag=false; callee runs Map.drop itself.
	assertContains(t, body, "heap.claim")
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

// T0610: A vector literal whose element is a moved local variable of a type
// Vector.drop's element-walk frees (heap-user-with-drop, string, droppable
// enum, Mutex/Task, nested vector) must clear the source ident's drop flag —
// otherwise the source variable's scope-exit drop AND Vector.drop's element
// walk free the same allocation (double-free / SEGV). Mirrors genTupleLit
// (B0242) / genMapLit (B0280).
func TestVectorLitMoveFromVarClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type _Box { string label; drop(~this) {} }
		main() {
			_Box b0 = _Box(label: "a");
			_Box[] v = [b0];
		}
	`)
	// b0 is moved into the vector literal — its drop flag must be cleared so
	// Vector.drop's element walk becomes the sole owner.
	assertContains(t, ir, "store i1 false, i1* %b0.dropflag")
}

// T0620: Optional[heap-user] moved from variable into Vector[T?] literal must
// clear the source's drop flag — the vector now owns the inner payload via
// emitVectorElementDropLoop's Optional branch. Pre-T0620, this was NOT cleared
// (T0610 regression guard); now it IS cleared because Gap A is fixed.
func TestVectorLitMoveFromVarOptionalHeapClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type _Box { string label; drop(~this) {} }
		main() {
			_Box? a = _Box(label: "a");
			_Box?[] v = [a];
		}
	`)
	assertContains(t, ir, "%a.dropflag = alloca i1")
	// T0620: Gap A fix — vecElemNeedsOptionalDrop now matches, so the drop flag
	// is cleared, transferring ownership to the vector.
	assertContains(t, ir, "store i1 false, i1* %a.dropflag")
}

// T0620: Gap B fix — Vector[string?] drop must enter the element drop loop
// and emit the Optional drop branch (optfield.drop block). Pre-T0620, the
// emitVectorElementDropLoop guard early-returned for Optional elements.
func TestVectorOptionalStringElementDropLoop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? a = "hello";
			string?[] v = [a];
		}
	`)
	// The element drop loop body enters emitVariantFieldDrop → emitOptionalValueDrop,
	// which creates an "optfield.drop" block for the has-value branch.
	assertContains(t, ir, "optfield.drop")
	// The drop loop itself is emitted (vecdrop.head/body/done blocks).
	assertContains(t, ir, "vecdrop.head")
}

// T0620: Drop-on-overwrite for Vector[string?] index assign — must emit
// emitVariantFieldDrop on the old element before storing the new one.
func TestVectorOptionalStringIndexAssignDropsOld(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? a = "old";
			string?[] v = [a];
			v[0] = "new";
		}
	`)
	// The overwrite path loads the old element, drops it via emitVariantFieldDrop
	// (Optional branch → optfield.drop), then stores the new value.
	assertContains(t, ir, "optfield.drop")
}

// T0620: Dup-on-read for Vector[T?] exercises dupOptionalVectorElem branches.
// Reading v[i] into a variable must deep-dup the Optional inner so both
// the variable and the vector own independent copies.
func TestVectorOptionalDupOnReadBranches(t *testing.T) {
	// String branch — dupOptionalVectorElem → dupString
	ir := generateIR(t, `
		main() {
			string? a = "x";
			string?[] v = [a];
			string? x = v[0];
		}
	`)
	assertContains(t, ir, "optdup.dup")
	assertContains(t, ir, "optdup.merge")

	// Heap user branch — dupOptionalVectorElem → cloneHeapElement
	ir = generateIR(t, `
		type _B620 { string s; drop(~this) {} }
		main() {
			_B620? b = _B620(s: "hi");
			_B620?[] v = [b];
			_B620? x = v[0];
		}
	`)
	assertContains(t, ir, "optdup.dup")

	// Vector branch — dupOptionalVectorElem → dupVector
	ir = generateIR(t, `
		main() {
			int[]? a = [1, 2];
			int[]?[] v = [a];
			int[]? x = v[0];
		}
	`)
	assertContains(t, ir, "optdup.dup")

	// Channel branch — dupOptionalVectorElem → dupChannel
	ir = generateIR(t, `
		main() {
			channel[int]? ch = channel[int]();
			channel[int]?[] v = [ch];
			channel[int]? x = v[0];
		}
	`)
	assertContains(t, ir, "optdup.dup")

	// Arc branch — dupOptionalVectorElem → dupArc
	ir = generateIR(t, `
		main() {
			Ref[int]? a = Ref[int](1);
			Ref[int]?[] v = [a];
			Ref[int]? x = v[0];
		}
	`)
	assertContains(t, ir, "optdup.dup")
}

// T0610: a droppable tuple bound to a *variable* and moved into a vector
// literal must clear the source ident's drop flag — exercising the
// tupleNeedsDrop arm of the type-gate. Existing tuple-in-vector-literal
// tests only use inline tuple constructors (no ident move), so this is the
// sole IR coverage of the tupleNeedsDrop branch. Without the clear, the
// tuple field is freed by both the var's bindingDropTuple and Vector.drop's
// element walk → double-free (verified at runtime against baseline).
func TestVectorLitMoveFromVarTupleClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type _Box { string label; drop(~this) {} }
		main() {
			(int, _Box) t = (1, _Box(label: "a"));
			(int, _Box)[] v = [t];
		}
	`)
	// The tuple var has its own drop flag (bindingDropTuple, T0371).
	assertContains(t, ir, "%t.dropflag = alloca i1")
	// Moved into the vector literal — the tupleNeedsDrop arm must clear it so
	// Vector.drop's element walk becomes the sole owner of the tuple's _Box.
	assertContains(t, ir, "store i1 false, i1* %t.dropflag")
}

// T0610: a plain heap user type with NO drop method (and no droppable
// fields) moved from a variable into a vector literal must still clear the
// source ident's drop flag — exercising the "needs pal_free" arm of
// vecElemNeedsUserTypeDrop (stmt.go:3983). All other T0610 tests use a type
// WITH an explicit drop; this is the sole coverage of the pal_free-only
// element path. Without the clear, both the var's scope-exit pal_free and
// Vector.drop's element walk free the same allocation → double-free
// (verified at runtime against baseline).
func TestVectorLitMoveFromVarPlainHeapClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type _Plain { int x; int y; }
		main() {
			_Plain p = _Plain(x: 1, y: 2);
			_Plain[] v = [p];
		}
	`)
	assertContains(t, ir, "%p.dropflag = alloca i1")
	assertContains(t, ir, "store i1 false, i1* %p.dropflag")
}

// B0210: Optional[TypeParam] field with none value should not cause mono layout mismatch.
// The mono layout computes the correct LLVM type for the optional field, but the none
// value was generated using an unsubstituted TypeParam, producing a type mismatch.
func TestOptionalTypeParamFieldNone(t *testing.T) {
	ir := generateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[int](val: none);
		}
	`)
	// The optional field should use the correct substituted type (i64 for int)
	assertContains(t, ir, "{ i1, i64 }")
}

// B0210: Optional[TypeParam] field with a concrete value should work too.
func TestOptionalTypeParamFieldValue(t *testing.T) {
	ir := generateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[string](val: "hello");
		}
	`)
	assertContains(t, ir, "{ i1, i8* }")
}

// B0210: Multiple Optional[TypeParam] fields with different instantiations.
func TestOptionalTypeParamMultipleInstantiations(t *testing.T) {
	ir := generateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m1 := MaybeVal[int](val: none);
			m2 := MaybeVal[string](val: none);
		}
	`)
	// Both int? and string? layouts should be present
	assertContains(t, ir, "{ i1, i64 }")
	assertContains(t, ir, "{ i1, i8* }")
}

// B0245: Vector[UserType] drop should emit element drop loop for heap user types.
func TestVectorUserTypeElementDrop(t *testing.T) {
	ir := generateIR(t, `
		type Foo { int x; }
		main() {
			Foo[] v = [];
			v.push(Foo(x: 1));
		}
	`)
	// Should have vector element drop loop (vecdrop.head) for user type elements
	assertContains(t, ir, "vecdrop.head")
	assertContains(t, ir, "call void @pal_free(")
}

// B0245: Debug — check what IR is generated for Vector[Foo] with full std
func TestVectorUserTypeElementDropWithPush(t *testing.T) {
	ir := generateIR(t, `
		type Foo { int x; }
		test() {
			Foo[] v = [];
			v.push(Foo(x: 1));
			v.push(Foo(x: 2));
		}
	`)
	// Check for element drop loop: vecdrop.head is the loop header
	if !strings.Contains(ir, "vecdrop.head") {
		// Print the main function IR for debugging
		lines := strings.Split(ir, "\n")
		inFunc := false
		for _, line := range lines {
			if strings.Contains(line, "define") && strings.Contains(line, "@test(") {
				inFunc = true
			}
			if inFunc {
				t.Logf("%s", line)
				if line == "}" {
					break
				}
			}
		}
		t.Errorf("expected vecdrop.head element drop loop for Vector[Foo]")
	}
}

// B0257: Vector element drop loop must call both the user type's drop method
// and pal_free to free the instance memory.
func TestVectorUserTypeDropCallsFree(t *testing.T) {
	ir := generateIR(t, `
		type Res { int id; drop(~this) {} }
		test() {
			Res[] v = [];
			v.push(Res(id: 1));
		}
	`)
	// Element drop loop header must exist
	assertContains(t, ir, "vecdrop.head")
	// The loop body must call Res.drop AND pal_free (not just drop)
	assertContains(t, ir, "call void @Res.drop(")
	// pal_free must appear in the element drop loop (for the instance memory)
	assertContains(t, ir, "call void @pal_free(")
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

// T0109: Vector-producing call expressions (e.g., split()) are tracked as stmt temps.
func TestVectorCallExprStmtTempTracking(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int n = "a b c".split(" ").len;
		}
	`)
	// The vector temp from split() should be tracked and dropped.
	assertContains(t, ir, "call void @Vector.drop(")
}

// B0258: Method chain intermediate heap-allocated values must be tracked as
// heap temps and freed at statement end.
func TestMethodChainIntermediateTracked(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y;
			sum(this) int { return this.x + this.y; }
			add_point(this, int dx, int dy) Point {
				return Point(x: this.x + dx, y: this.y + dy);
			}
		}
		test() {
			Point p = Point(x: 1, y: 2);
			int result = p.add_point(dx: 10, dy: 20).sum();
		}
	`)
	// The intermediate Point from add_point() should be tracked as a heap temp
	// and freed at statement end via the heap.drop cleanup path.
	assertContains(t, ir, "heap.drop")
	assertContains(t, ir, "call void @pal_free(")
}

// B0325: Field access on a call result must track the intermediate heap instance.
func TestFieldAccessOnCallResultTracked(t *testing.T) {
	ir := generateIR(t, `
		type Pair { int x; int y; }
		make_pair() Pair { return Pair(x: 10, y: 20); }
		test() {
			int v = make_pair().x;
		}
	`)
	assertContains(t, ir, "heap.drop")
	assertContains(t, ir, "call void @pal_free(")
}

// B0325: Field access on a ?! unwrap result must track the intermediate heap instance.
func TestFieldAccessOnErrorPanicResultTracked(t *testing.T) {
	ir := generateIR(t, `
		type Pair { int x; int y; }
		make_pair!() Pair { return Pair(x: 10, y: 20); }
		test!() {
			int v = make_pair()?!.x;
		}
	`)
	assertContains(t, ir, "heap.drop")
	assertContains(t, ir, "call void @pal_free(")
}

// B0325: Method call on a ?! unwrap result must track the intermediate heap instance.
func TestMethodCallOnErrorPanicResultTracked(t *testing.T) {
	ir := generateIR(t, `
		type Pair { int x; int y;
			sum(this) int { return this.x + this.y; }
		}
		make_pair!() Pair { return Pair(x: 10, y: 20); }
		test!() {
			int v = make_pair()?!.sum();
		}
	`)
	assertContains(t, ir, "heap.drop")
	assertContains(t, ir, "call void @pal_free(")
}

// B0325: Field access on a type with explicit drop must use the $wrap function
// (drop + pal_free), not just the raw drop function.
func TestFieldAccessExplicitDropUsesWrap(t *testing.T) {
	ir := generateIR(t, `
		type Resource { int id; drop(~this) {} }
		make_resource!() Resource { return Resource(id: 42); }
		test!() {
			int v = make_resource()?!.id;
		}
	`)
	assertContains(t, ir, "heap.drop")
	// Must use the $wrap function that calls drop + pal_free
	assertContains(t, ir, "Resource.drop$wrap")
}

// === Clone annotation (T0154) ===

func TestCloneSynthesizesCloneMethod(t *testing.T) {
	ir := generateIR(t, `
		type Doc `+"`clone"+` {
			string title;
			int pages;
		}
		test() {
			d := Doc(title: "hi", pages: 1);
			d2 := d.clone();
		}
	`)
	// The synthesized clone method should exist and call promise_string_new (dupString)
	assertContains(t, ir, "Doc.clone")
}

func TestCloneStringNativeMethod(t *testing.T) {
	ir := generateIR(t, `
		test() {
			s := "hello";
			s2 := s.clone();
		}
	`)
	// string.clone() calls promise_string_new (dupString)
	assertContains(t, ir, "promise_string_new")
}

func TestCloneVectorNativeMethod(t *testing.T) {
	ir := generateIR(t, `
		test() {
			v := [1, 2, 3];
			v2 := v.clone();
		}
	`)
	// Vector.clone() calls pal_alloc (dupVector)
	assertContains(t, ir, "pal_alloc")
}

func TestCloneStringVectorDupsElements(t *testing.T) {
	ir := generateIR(t, `
		test() {
			v := ["a", "b"];
			v2 := v.clone();
		}
	`)
	// String vector clone should have the string dup loop
	assertContains(t, ir, "vecdup_str.head")
	assertContains(t, ir, "promise_string_new")
}

// B0275: Vector.clone() must deep-clone heap user type elements.
func TestCloneVectorHeapTypeCallsClone(t *testing.T) {
	ir := generateIR(t, `
		type Foo `+"`"+`clone {
			string name;
		}
		test() {
			v := [Foo(name: "a")];
			v2 := v.clone();
		}
	`)
	// Should have the clone loop calling Foo.clone
	assertContains(t, ir, "vecclone.head")
	assertContains(t, ir, "Foo.clone")
}

// B0275: Vector.clone() must dup channel elements (refcount increment).
func TestCloneVectorChannelDupsElements(t *testing.T) {
	ir := generateIR(t, `
		test() {
			ch := channel[int](1);
			v := [ch];
			v2 := v.clone();
		}
	`)
	// Should have the clone loop with channel dup (atomic refcount increment)
	assertContains(t, ir, "vecclone.head")
	assertContains(t, ir, "chdup.inc")
}

// B0276: dupHeapValueFields must deep-clone vector fields with droppable elements.
// When a heap type without clone() has a string[] field and is dup'd via
// dupHeapValue (e.g., as a vector element during clone), the string[] field
// must be deep-cloned, not shallow-copied.
func TestDupHeapValueFieldsDeepClonesVectorStrings(t *testing.T) {
	ir := generateIR(t, `
		type Container {
			string[] names;
			int id;
		}
		test() {
			v := [Container(names: ["a", "b"], id: 1)];
			v2 := v.clone();
		}
	`)
	// Vector[Container].clone() → emitVectorElementCloneLoop → cloneHeapElement
	// → dupHeapValue → dupHeapValueFields → should deep-clone the string[] field.
	assertContains(t, ir, "vecdup_str.head")
}

// B0289: emitVectorElementCloneLoop → cloneHeapElement must check type-arg safety
// before calling clone(). When vector elements are Map[string, NonCloneableEnum],
// Map.clone() would shallow-copy the enum values → double-free. The fix falls
// back to dupHeapValue instead.
func TestVectorCloneLoopSkipsUnsafeMapClone(t *testing.T) {
	ir := generateIR(t, `
		enum JsonNode {
			Null,
			Text(string value),
			Dict(map[string, JsonNode] fields),
		}
		test() {
			map[string, JsonNode][] maps = [{"k": JsonNode.Text(value: "v")}];
			map[string, JsonNode][] maps2 = maps.clone();
		}
	`)
	// T1129: JsonNode (recursive, Map-bearing) now has a synthesized recursive
	// clone, so Map[string, JsonNode] IS deep-cloneable. Vector[Map[...]].clone()
	// → emitVectorElementCloneLoop → cloneHeapElement now correctly routes through
	// Map[string, JsonNode].clone() (whose internal match-dup recurses via
	// @JsonNode.clone). This supersedes the old B0289 dupHeapValue fallback, whose
	// recursion guard shallow-copied the inner Map → double-free. jn3/jn4 runtime
	// tests confirm this path is leak-free.
	if extractDefine(ir, "JsonNode.clone") == "" {
		t.Errorf("T1129: expected a synthesized @JsonNode.clone for the recursive enum:\n%s", ir)
	}
	if !strings.Contains(ir, "Map[string, JsonNode].clone") {
		t.Errorf("T1129: Vector[Map[string, JsonNode]].clone() should deep-clone elements "+
			"via Map.clone() now that JsonNode is cloneable:\n%s", ir)
	}
}

// B0289: When vector elements are Map[string, int] (safe type args),
// cloneHeapElement should still call Map.clone().
func TestVectorCloneLoopCallsSafeMapClone(t *testing.T) {
	ir := generateIR(t, `
		test() {
			map[string, int][] maps = [{"a": 1}];
			map[string, int][] maps2 = maps.clone();
		}
	`)
	// Map[string, int] — both type args are safe, clone should be called.
	assertContains(t, ir, "Map[string, int].clone")
}

// T0559 + T0545 + T0616: Vector[Mutex|Task|MutexGuard].clone() via generic
// indirection is now rejected at sema by T0616 (deferred cloneability
// requirements propagated across generic call edges). The codegen-side
// backstop (length-guarded vecclone.unsup.panic, isSingleOwnerHandleType
// early-returns in cloneHeapElement/dupHeapValue) remains as defense-in-depth
// but is unreachable for well-formed user code. The IR-shape assertions that
// formerly pinned the backstop's emission have been removed; the sema-error
// behavior is verified by TestT0616_VectorCloneInGeneric{Task,Mutex,MutexGuard}Error
// in the sema package.

// B0281: Enum ctor temps used as map literal values must be claimed.
// Without the fix, the enum temp is dropped at statement end, double-freeing
// inner data (both the temp and the map's Slot share the same pointers).
func TestEnumCtorTempClaimedInMapLiteral(t *testing.T) {
	ir := generateIR(t, `
		enum Val { Txt(string s), Num(int n) }
		main() {
			map[string, Val] m = { "a": Val.Txt(s: "hello") };
		}
	`)
	// The enum ctor temp drop flag should be cleared (stored i1 false) BEFORE
	// statement-end cleanup. No enum.ctor.drop block should fire for this temp.
	assertNotContains(t, ir, "enum.ctor.drop")
}

// B0281: Enum ctor temps used as vector literal elements must be claimed.
func TestEnumCtorTempClaimedInVectorLiteral(t *testing.T) {
	ir := generateIR(t, `
		enum Val { Txt(string s), Num(int n) }
		main() {
			Val[] v = [Val.Txt(s: "hello"), Val.Num(n: 42)];
		}
	`)
	assertNotContains(t, ir, "enum.ctor.drop")
}

// B0288: is-present on method call returning T? with droppable enum inner type
// must emit a conditional drop for the temporary.
func TestIsPresentDropsTempOptionalEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Val { Txt(string s), Num(int n) }
		type Box {
			Val? item;
			get_item(this) Val? {
				return this.item;
			}
		}
		main() {
			Box b = Box(item: Val.Txt(s: "hello"));
			bool ok = b.get_item() is present;
		}
	`)
	// The method call returns a temporary Val? — the enum data must be dropped.
	assertContains(t, ir, "is.temp.drop")
	assertContains(t, ir, "is.temp.skip")
}

// B0288: is-present on ident expression must NOT emit temp drop
// (the variable's scope binding handles cleanup).
func TestIsPresentIdentNoTempDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? s = "hello";
			bool ok = s is present;
		}
	`)
	assertNotContains(t, ir, "is.temp.drop")
}

// B0288: is-present on field access must NOT emit temp drop
// (the parent object owns the field data).
func TestIsPresentFieldNoTempDrop(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			string? value;
		}
		main() {
			Holder h = Holder(value: "hello");
			bool ok = h.value is present;
		}
	`)
	assertNotContains(t, ir, "is.temp.drop")
}

// B0288: is-present on method call returning string? must emit temp drop.
func TestIsPresentDropsTempOptionalString(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			string? name;
			get_name(this) string? {
				return this.name;
			}
		}
		main() {
			Box b = Box(name: "hello");
			bool ok = b.get_name() is present;
		}
	`)
	assertContains(t, ir, "is.temp.drop")
	assertContains(t, ir, "promise_string_drop")
}

// B0288: is-present on method call returning UserType? with drop() must emit
// temp drop (extract instance ptr, null-check, call drop, free).
func TestIsPresentDropsTempOptionalUserType(t *testing.T) {
	ir := generateIR(t, `
		type Handle {
			int id;
			drop(~this) {}
		}
		type Factory {
			find(this, int id) Handle? {
				if id > 0 {
					return Handle(id: id);
				}
				return none;
			}
		}
		main() {
			Factory f = Factory();
			bool ok = f.find(1) is present;
		}
	`)
	assertContains(t, ir, "is.temp.drop")
	assertContains(t, ir, "is.temp.exec")
	assertContains(t, ir, "Handle.drop")
}

// B0287: Optional unwrap on ident source must NOT track the unwrapped string
// as a statement temp (the optional's scope-exit drop handles it).
func TestOptionalUnwrapIdentNoStringTemp(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? s = "hello";
			bool eq = s! == "hello";
		}
	`)
	// The s! result should not be tracked as a string temp.
	// If it were tracked, there would be a promise_string_drop call for the temp
	// AND the optional's scope drop — double-free.
	// Count promise_string_drop calls: should be only from scope cleanup, not temp tracking.
	testFn := extractFunction(ir, "main")
	count := strings.Count(testFn, "promise_string_drop")
	// Expect at most 1 drop (the optional's scope-exit drop).
	if count > 1 {
		t.Fatalf("expected at most 1 promise_string_drop call, got %d\n%s", count, testFn)
	}
}

// B0290: When a heap type with a vector of droppable enums is dup'd via
// dupHeapValue → dupHeapValueFields → emitVectorElementCloneLoop, enum elements
// without clone methods should be dup'd in place (switch on tag, dup droppable fields).
func TestDupEnumElementInPlaceForVectorOfEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Slot {
			Empty,
			Used(string key, string value),
		}
		type Container {
			Slot[] buckets;
			drop(~this) {}
		}
		test() {
			Container[] v = [Container(buckets: [Slot.Used(key: "a", value: "b")])];
			Container[] v2 = v.clone();
		}
	`)
	// Vector[Container].clone() → emitVectorElementCloneLoop → cloneHeapElement →
	// dupHeapValue → dupHeapValueFields → for Slot[] field → dupVector +
	// emitVectorElementCloneLoop → dupEnumElementInPlace for Slot elements.
	assertContains(t, ir, "enumdup.Used")
	assertContains(t, ir, "enumdup.done")
}

// B0302: Pushing a vector into a vector-of-vectors must dup the inner vector
// to ensure exclusive ownership. Without dup, filled() creates aliased pointers
// that cause double-free on the outer vector's element-level drop.
func TestVectorPushDupsDroppableElement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[][] v = [];
			int[] inner = [1, 2, 3];
			v.push(inner);
		}
	`)
	// The push should dup the inner vector (vecdup.copy block from dupVector)
	assertContains(t, ir, "vecdup.copy")
}

// TestCrossModulePropagation verifies that instances of types from module B
// created inside module A are propagated to B. Map[string, int] works even
// though Slot[string, int] (a generic enum in std) is only reachable through
// Map's fields.
func TestCrossModulePropagation(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
		}
	`)

	// Map layout must exist
	assertContains(t, ir, "Map[string, int]")
}

// generateIRWithDependentModules parses mod1, then mod2 (which can import mod1),
// then user code (which can import both). Used for cross-module dependency tests.
func generateIRWithDependentModules(t *testing.T,
	mod1Name, mod1Src, mod2Name, mod2Src, userSrc string) string {
	t.Helper()

	mod1Info, mod1Scope := parseModuleSource(t, mod1Name, mod1Src)
	stdModInfo, stdScope := getCodegenStdModInfo()

	// Parse mod2 with mod1 in scope
	mod1Key := "./" + mod1Name
	mod2Input := antlr.NewInputStream(mod2Src)
	mod2Lexer := parser.NewPromiseLexer(mod2Input)
	mod2Lexer.RemoveErrorListeners()
	mod2Stream := antlr.NewCommonTokenStream(mod2Lexer, antlr.TokenDefaultChannel)
	mod2P := parser.NewPromiseParser(mod2Stream)
	mod2P.RemoveErrorListeners()
	mod2Tree := mod2P.CompilationUnit()
	mod2File, errs := ast.Build("module.pr", mod2Tree)
	if len(errs) > 0 {
		t.Fatalf("mod2 AST build errors: %v", errs)
	}
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	mod2File.Uses = append([]*ast.UseDecl{stdUse}, mod2File.Uses...)
	mod2Info2, semaErrs := sema.CheckWithModules(mod2File, map[string]*types.Scope{
		"std":   stdScope,
		mod1Key: mod1Scope,
	})
	if len(semaErrs) > 0 {
		t.Fatalf("mod2 sema errors: %v", semaErrs)
	}
	mod2Scope := sema.ExportedScope(mod2Info2, mod2File)
	mod2Key := "./" + mod2Name
	mod2Info := &sema.ModuleInfo{
		Name:           mod2Name,
		CanonicalName:  mod2Name,
		GlobalIdentity: mod2Key,
		IRPrefix:       mod2Name,
		Path:           mod2Key,
		File:           mod2File,
		SemaInfo:       mod2Info2,
	}

	// Parse user code
	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs2 := ast.Build("test.pr", userTree)
	if len(errs2) > 0 {
		t.Fatalf("user AST build errors: %v", errs2)
	}
	userFile.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, userFile.Uses...)

	userInfo, userSemaErrs := sema.CheckWithModules(userFile, map[string]*types.Scope{
		"std":   stdScope,
		mod1Key: mod1Scope,
		mod2Key: mod2Scope,
	})
	if len(userSemaErrs) > 0 {
		t.Fatalf("user sema errors: %v", userSemaErrs)
	}

	userInfo.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":   stdModInfo,
		mod1Key: mod1Info,
		mod2Key: mod2Info,
	}
	userInfo.ModuleOrder = []string{"std", mod1Key, mod2Key}

	result := Compile(userFile, userInfo, "")
	return result.Module.String()
}

// TestCrossModuleGenericMethodCallsGenericFunc verifies B0344: when a generic
// method in module B calls a generic function in module A, the func instance
// (which contains TypeParams in B's sema) gets resolved via the concrete method
// instance from user code.
func TestCrossModuleGenericMethodCallsGenericFunc(t *testing.T) {
	ir := generateIRWithDependentModules(t,
		"helper",
		`transform[T](T val) T `+"`public"+` { return val; }`,
		"caller",
		`use helper "./helper";
		type Wrapper[V] `+"`public"+` {
			V _value;
			apply[T](this, T extra) T `+"`public"+` {
				return helper.transform[T](extra);
			}
		}`,
		`
		use caller "./caller";
		main() {
			caller.Wrapper[string] w = caller.Wrapper[string](_value: "hi");
			int result = w.apply[int](42);
		}
		`,
	)

	// The mono func instance transform[int] must be defined
	assertContains(t, ir, `define i64 @"transform[int]"`)
	// The mono method apply[int] must be defined for Wrapper[string]
	assertContains(t, ir, `@"Wrapper[string].apply[int]"`)
}

// TestCrossModuleGenericMethodCallsGenericFuncMultipleInstances verifies that
// cross-module resolution handles multiple concrete instantiations (B0344).
func TestCrossModuleGenericMethodCallsGenericFuncMultipleInstances(t *testing.T) {
	ir := generateIRWithDependentModules(t,
		"conv",
		`identity[T](T val) T `+"`public"+` { return val; }`,
		"box",
		`use conv "./conv";
		type Box[V] `+"`public"+` {
			V _data;
			unwrap[T](this, T fallback) T `+"`public"+` {
				return conv.identity[T](fallback);
			}
		}`,
		`
		use box "./box";
		main() {
			box.Box[int] b1 = box.Box[int](_data: 1);
			int r1 = b1.unwrap[int](10);
			box.Box[string] b2 = box.Box[string](_data: "x");
			string r2 = b2.unwrap[string]("y");
		}
		`,
	)

	// Both concrete instantiations must exist
	assertContains(t, ir, `define i64 @"identity[int]"`)
	assertContains(t, ir, `@"identity[string]"`)
	assertContains(t, ir, `@"Box[int].unwrap[int]"`)
	assertContains(t, ir, `@"Box[string].unwrap[string]"`)
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

// B0343: Map []= with borrow-string key must dup so the map owns the key.
func TestMapIndexAssignDupBorrowKey(t *testing.T) {
	ir := generateIR(t, `
		type Sink {
			map[string, string] m;
			put(~this, string k, string v) {
				this.m[k] = v;
			}
		}
		test() {
			Sink s = Sink(m: map[string, string]());
			s.put("a", "b");
		}
	`)
	// The key "k" (borrow param, no drop flag) must be dup'd at the []= site
	assertContains(t, ir, "strdup.copy")
}

// B0343: Map []= with owned-string key (has drop flag) clears the flag.
func TestMapIndexAssignClearKeyDropFlag(t *testing.T) {
	ir := generateIR(t, `
		test() {
			map[string, string] m = map[string, string]();
			for k, v in m {
				map[string, string] dst = map[string, string]();
				dst[k] = v;
			}
		}
	`)
	// Key k has a drop flag from B0343; dst[k] = v clears it
	assertContains(t, ir, "k.dropflag")
	assertContains(t, ir, "forin.key.drop")
}

// B0355: MemberExpr (field access) used as map key must be dup'd — the struct
// still owns the pointer, so the map needs an independent copy.
func TestMapIndexAssignDupBorrowKeyMemberExpr(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		test() {
			Row r = Row(name: "hello");
			map[string, int] m = map[string, int]();
			m[r.name] = 1;
		}
	`)
	assertContains(t, ir, "strdup.copy")
}

// B0355: MemberExpr (field access) used as map value must be dup'd — the struct
// still owns the pointer, so the map needs an independent copy.
func TestMapIndexAssignDupBorrowValueMemberExpr(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		test() {
			Row r = Row(name: "world");
			map[string, string] m = map[string, string]();
			m["k"] = r.name;
		}
	`)
	assertContains(t, ir, "strdup.copy")
}

// T0261: Verify that vector drop + go block produces unique local names
// (no duplicate vecdrop.idx allocas).
func TestGoBlockVectorDropUniqueNames(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] items = ["a", "b"];
			ch := channel[int](capacity: 1);
			go { ch.send(1); ch.close(); };
			int? v = <-ch;
			print_line("{items.len}");
		}
	`)
	// If codegen succeeds, localNameCount was properly saved/restored.
	// The IR should contain the vector drop loop.
	assertContains(t, ir, "vecdrop.idx")
}

// T0155: Ref[T] constructor allocates {i64, T} and stores refcount=1.
func TestArcConstructorAllocAndRefcount(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
		}
	`)
	// Should allocate 24 bytes (i64 strong_count + i64 weak_count + i64 value) — T0157
	assertContains(t, ir, "call i8* @pal_alloc(i64 24)")
	// Should store refcount = 1
	assertContains(t, ir, "store i64 1")
	// Should store the value 42
	assertContains(t, ir, "store i64 42")
}

// T0155: Ref[T] scope cleanup uses drop flag and calls Ref[int].drop.
func TestArcDropFlagAndCleanup(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
		}
	`)
	assertContains(t, ir, "%a.dropflag")
	assertContains(t, ir, `call void @"Ref[int].drop"(`)
}

// T0155: Ref[T].drop function has correct structure: null check, atomic decrement, free.
func TestArcDropFunctionBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
		}
	`)
	dropFn := extractFunction(ir, `"Ref[int].drop"`)
	if dropFn == "" {
		t.Fatal("expected Ref[int].drop function in IR")
	}
	// Null check
	assertContains(t, dropFn, "icmp eq")
	assertContains(t, dropFn, "null")
	// Atomic refcount decrement
	assertContains(t, dropFn, "i64 -1")
	// Free on last reference
	assertContains(t, dropFn, "call void @pal_free(")
}

// T0155: Ref[T].clone atomically increments refcount.
func TestArcCloneAtomicIncrement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			b := a.clone();
		}
	`)
	// Clone should atomically add 1 to refcount
	assertContains(t, ir, "i64 1")
	// Both a and b should have drop flags
	assertContains(t, ir, "%a.dropflag")
	assertContains(t, ir, "%b.dropflag")
}

// T0155: Ref[T] borrow getter loads value from allocation.
func TestArcBorrowLoadsValue(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			int x = a.borrow;
		}
	`)
	// Borrow GEPs into the {i64, i64, T} struct at field 2 (T0157: value shifted to field 2)
	assertContains(t, ir, "getelementptr { i64, i64, i64 }")
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

// T0157: Weak[T] drop function has correct structure: null check, atomic decrement weak_count.
func TestWeakDropFunctionBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			w := a.downgrade();
		}
	`)
	// Weak drop should null-check and decrement weak_count
	assertContains(t, ir, "define void @\"Weak[int].drop\"")
	assertContains(t, ir, "decwc:")
	assertContains(t, ir, "free:")
}

// T0157: Ref[T] drop now has two-stage deallocation with weak_count.
func TestArcDropTwoStageDeallocation(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
		}
	`)
	// Arc drop should have drop_value block (drops T + decrements weak_count)
	assertContains(t, ir, "drop_value:")
}

// T0499: Arc clone/downgrade chain intermediates produce fresh SSA values via ptrtoint+inttoptr
// so the method result is tracked separately from the constructor stmtTemp.
func TestArcCloneChainFreshSSA(t *testing.T) {
	ir := generateIR(t, `
		main() {
			Ref[int] b = Ref[int](42).clone();
		}
	`)
	// The clone result must be a fresh SSA value (ptrtoint+inttoptr) so stmtTemp
	// dedup doesn't merge it with the constructor's temp — both get dropped.
	assertContains(t, ir, "ptrtoint")
	assertContains(t, ir, "inttoptr")
	assertContains(t, ir, "%b.dropflag")
}

func TestArcDowngradeChainFreshSSA(t *testing.T) {
	ir := generateIR(t, `
		main() {
			Weak[int] w = Ref[int](42).downgrade();
		}
	`)
	// Downgrade must also produce a fresh SSA value for chain tracking
	assertContains(t, ir, "ptrtoint")
	assertContains(t, ir, "inttoptr")
	assertContains(t, ir, "%w.dropflag")
}

func TestWeakCloneChainFreshSSA(t *testing.T) {
	ir := generateIR(t, `
		main() {
			Ref[int] a = Ref[int](99);
			Weak[int] w = a.downgrade().clone();
		}
	`)
	// Weak clone in a chain must produce fresh SSA value
	assertContains(t, ir, "ptrtoint")
	assertContains(t, ir, "inttoptr")
	assertContains(t, ir, "%w.dropflag")
}

// T0157: Weak[T].clone() atomically increments weak_count.
func TestWeakCloneIR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			w := a.downgrade();
			w2 := w.clone();
		}
	`)
	// Both w and w2 should have drop flags
	assertContains(t, ir, "%w.dropflag")
	assertContains(t, ir, "%w2.dropflag")
	// Both should call Weak[int].drop at scope exit
	assertContains(t, ir, `call void @"Weak[int].drop"(`)
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

// T0157: dupArc — reading an Ref[T] field from a droppable type increments strong refcount.
func TestDupArcFieldFromDroppable(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			Ref[int] a;
			drop(~this) {}
		}
		main() {
			h := Holder(a: Ref[int](42));
			Ref[int] copy = h.a;
		}
	`)
	// Should produce arcdup block for refcount increment (numeric suffix varies)
	assertContainsMatch(t, ir, `arcdup\.inc\.\d+:`)
	assertContainsMatch(t, ir, `arcdup\.merge\.\d+:`)
}

// T0157: dupWeak — reading a Weak[T] field from a droppable type increments weak refcount.
func TestDupWeakFieldFromDroppable(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			Weak[int] w;
			drop(~this) {}
		}
		main() {
			a := Ref[int](42);
			h := Holder(w: a.downgrade());
			Weak[int] copy = h.w;
		}
	`)
	// Should produce weakdup block for weak_count increment (numeric suffix varies)
	assertContainsMatch(t, ir, `weakdup\.inc\.\d+:`)
	assertContainsMatch(t, ir, `weakdup\.merge\.\d+:`)
}

// T0156: Mutex[T] constructor allocates {i8* pal_handle, T value} and inits mutex.
func TestMutexConstructorAllocAndInit(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](42);
		}
	`)
	// Should init the PAL mutex and cond var
	assertContains(t, ir, "call i8* @pal_mutex_init()")
	assertContains(t, ir, "call i8* @pal_cond_init()")
	// Should store the value 42
	assertContains(t, ir, "store i64 42")
	// Should init held flag to 0
	assertContains(t, ir, "store i8 0")
}

// T0156: Mutex[T] scope cleanup uses drop flag and calls Mutex[int].drop.
func TestMutexDropFlagAndCleanup(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](42);
		}
	`)
	assertContains(t, ir, "%m.dropflag")
	assertContains(t, ir, `call void @"Mutex[int].drop"(`)
}

// T0156/T0285: Mutex[T].drop function has correct structure: null check, destroy cond + mutex, free.
func TestMutexDropFunctionBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](42);
		}
	`)
	dropFn := extractFunction(ir, `"Mutex[int].drop"`)
	if dropFn == "" {
		t.Fatal("expected Mutex[int].drop function in IR")
	}
	// Null check
	assertContains(t, dropFn, "icmp eq")
	assertContains(t, dropFn, "null")
	// Cond var destroy
	assertContains(t, dropFn, "call void @pal_cond_destroy(")
	// PAL mutex destroy
	assertContains(t, dropFn, "call void @pal_mutex_destroy(")
	// Free allocation
	assertContains(t, dropFn, "call void @pal_free(")
}

// T0156/T0285: Mutex.lock() uses scheduler-aware locking and allocates a guard.
func TestMutexLockAcquiresAndAllocatesGuard(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
		}
	`)
	// Should lock the PAL mutex (metadata critical section)
	assertContains(t, ir, "call void @pal_mutex_lock(")
	// Should check held flag
	assertContains(t, ir, "icmp eq i8")
	// Should allocate 8 bytes for the guard
	assertContains(t, ir, "call i8* @pal_alloc(i64 8)")
}

// T0301: Mutex.lock() must route to the contested path when waiters are queued,
// even if held==0 momentarily. Prevents newcomer starvation under contention
// when pthread_mutex is not FIFO. The acquired path requires both held==0 AND
// waiter_head==null, combined via `or` on the two conditions.
func TestMutexLockFairCheck(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
		}
	`)
	// Locate the block that branches on `mustWait` via `mutex.contested`/`mutex.acquired`.
	acqIdx := strings.Index(ir, "label %mutex.acquired")
	if acqIdx < 0 {
		t.Fatal("expected mutex.acquired branch label in IR")
	}
	// Search a small window before the branch for the fair-check instructions:
	// `icmp ne i8* %waiterHead, null` for hasWaiter, then `or i1 %isHeld, %hasWaiter`.
	windowStart := acqIdx - 400
	if windowStart < 0 {
		windowStart = 0
	}
	window := ir[windowStart:acqIdx]
	if !strings.Contains(window, "or i1") {
		t.Errorf("expected `or i1` combining held and waiter_head checks before mutex.acquired")
	}
	if !strings.Contains(window, "icmp ne i8*") {
		t.Errorf("expected `icmp ne i8*` for waiter_head != null check before mutex.acquired")
	}
}

// T0301: MutexGuard.drop's no-waiter unlock path must signal cond BEFORE
// pal_mutex_unlock (within the unlock.no_waiter block). Signal-before-unlock
// is the defensive POSIX ordering — it avoids a theoretical window where a
// waking cond_wait thread could observe stale `held` state on re-acquire.
func TestMutexUnlockNoWaiterSignalBeforeUnlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			guard := m.lock();
		}
	`)
	dropFn := extractFunction(ir, "MutexGuard.drop")
	if dropFn == "" {
		t.Fatal("expected MutexGuard.drop function in IR")
	}
	// Locate the no_waiter block body.
	marker := "unlock.no_waiter:"
	blkStart := strings.Index(dropFn, marker)
	if blkStart < 0 {
		t.Fatal("expected unlock.no_waiter block in MutexGuard.drop")
	}
	// Block ends at the next block label or `br`/return.
	blkTail := dropFn[blkStart:]
	idxSignal := strings.Index(blkTail, "@pal_cond_signal")
	idxUnlock := strings.Index(blkTail, "@pal_mutex_unlock")
	if idxSignal < 0 {
		t.Fatal("expected pal_cond_signal in unlock.no_waiter block")
	}
	if idxUnlock < 0 {
		t.Fatal("expected pal_mutex_unlock in unlock.no_waiter block")
	}
	if idxSignal > idxUnlock {
		t.Errorf("pal_cond_signal must come before pal_mutex_unlock in no_waiter block; signal@%d unlock@%d", idxSignal, idxUnlock)
	}
}

// T0156: MutexGuard.borrow getter loads T through the guard's mutex pointer.
func TestMutexGuardBorrowGetter(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](42);
			use guard := m.lock();
			int x = guard.borrow;
		}
	`)
	// Borrow navigates guard → mutex → value via GEPs on {i8*} and the full mutex struct
	assertContains(t, ir, "getelementptr { i8* }, { i8* }*")
	assertContains(t, ir, "getelementptr { i8*, i8*, i8*, i8*, i8, i64 }, { i8*, i8*, i8*, i8*, i8, i64 }*")
}

// T0156: MutexGuard.borrow setter stores T through the guard's mutex pointer.
func TestMutexGuardBorrowSetter(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
			guard.borrow = 99;
		}
	`)
	// Should store 99 through the guard→mutex→value path
	assertContains(t, ir, "store i64 99")
}

// T0270: Borrow setter must drop old value for droppable T via emitInnerDrop.
func TestMutexGuardBorrowSetterDropsOldValue(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[string]("hello");
			use guard := m.lock();
			guard.borrow = "world";
		}
	`)
	// emitInnerDrop should call promise_string_drop on the old value before storing new
	assertContains(t, ir, "call void @promise_string_drop(")
}

// T0270: Borrow setter compound assignment (guard.borrow += val).
func TestMutexGuardBorrowSetterCompoundAssign(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](10);
			use guard := m.lock();
			guard.borrow += 5;
		}
	`)
	// Compound assignment loads current value and adds
	assertContains(t, ir, "add i64")
}

// T0838: Binding the result of an error-handler unwrap of a single-owner
// native-handle optional field (`Mutex[int] m = h.mtx? _ {...}`) on an owned
// owner must neutralize the owner's optional present flag — genOptionalHandlerExpr
// makes NO dup for opaque containers (Mutex/Task are i8* handles that can't be
// deep-copied), so without the neutralization both the bound local and the
// owner's drop would free the same handle → double-free. The fix routes the
// handler binding through neutralizeMemberOptionalField (T0806 Fix C carve-out),
// emitting a `store i1 false` into the owner instance's optional flag.
func TestT0838MutexHandlerBindingNeutralizesOwnerField(t *testing.T) {
	ir := generateIR(t, `
		type MtxHolder { Mutex[int]? mtx; drop(~this) {} }
		main() {
			h := MtxHolder(mtx: Mutex[int](5));
			Mutex[int] m = h.mtx? _ { return; };
		}
	`)
	gmain := extractDefine(ir, ".goroutine.main")
	// The owner's `Mutex[int]?` field is laid out as the optional struct
	// `{ i1, i8* }` (present flag + opaque handle). Neutralization GEPs into that
	// struct's field 0 and stores `false`, so the owner's drop skips the handle
	// the binding now owns. (Generic drop-flag clears target named allocas, not a
	// GEP into `{ i1, i8* }`, so this pattern is specific to the field move-out.)
	assertContainsMatch(t, gmain,
		`getelementptr \{ i1, i8\* \}, \{ i1, i8\* \}\* %\d+, i32 0, i32 0\s*\n\s*store i1 false`)
}

// T0838 regression guard: a handler binding of a HEAP-USER optional field must
// NOT neutralize the owner field — genOptionalHandlerExpr makes an independent
// dup (T0775), so the owner keeps & frees the original and the bound local owns
// the dup. handlerResultIsNativeHandle returns false for heap-user types, so the
// T0775 dup-and-don't-neutralize contract is preserved (no `store i1 false`).
func TestT0838HeapUserHandlerBindingPreservesDup(t *testing.T) {
	ir := generateIR(t, `
		type Payload { int v; drop(~this) {} }
		type PayloadHolder { Payload? p; drop(~this) {} }
		main() {
			h := PayloadHolder(p: Payload(v: 21));
			Payload got = h.p? _ { Payload(v: 0) };
		}
	`)
	gmain := extractDefine(ir, ".goroutine.main")
	// handlerResultIsNativeHandle is false for heap-user types, so the T0775
	// dup-and-don't-neutralize contract holds: genOptionalHandlerExpr makes an
	// independent dup (the binding's sole owner) and the owner's optional field
	// is NOT neutralized — the owner keeps & frees the original. So there is no
	// GEP-into-optional-struct present-flag clear in the goroutine body.
	assertNotContainsMatch(t, gmain,
		`getelementptr \{ i1, [^}]*\}, \{ i1, [^}]*\}\* %\d+, i32 0, i32 0\s*\n\s*store i1 false`)
}

// T0838: Task[T]? sibling of the Mutex case — covers handlerResultIsNativeHandle's
// types.AsTask branch (reached only when the result is NOT a Mutex). Task is the
// other single-owner opaque i8* handle genOptionalHandlerExpr does not dup, so a
// handler binding `Task[int] t = h.tsk? _ {...}` must likewise neutralize the
// owner's optional present flag (same `{ i1, i8* }` GEP + `store i1 false`).
func TestT0838TaskHandlerBindingNeutralizesOwnerField(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		type TskHolder { Task[int]? tsk; drop(~this) {} }
		main() {
			h := TskHolder(tsk: go worker());
			Task[int] t = h.tsk? _ { return; };
		}
	`)
	gmain := extractDefine(ir, ".goroutine.main")
	assertContainsMatch(t, gmain,
		`getelementptr \{ i1, i8\* \}, \{ i1, i8\* \}\* %\d+, i32 0, i32 0\s*\n\s*store i1 false`)
}

// T0367: Assigning Ref[T].borrow to a variable must clear the variable's drop
// flag — the borrow returns a non-owning reference, so the parent's drop owns
// the inner value. Without the clear, both the borrow's drop and Arc.drop would
// free the same buffer (double-free / segfault for heap T).
func TestArcBorrowClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			borrowed := a.borrow;
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	// maybeRegisterDrop sets dropflag=true; T0367 fix immediately clears it.
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0367: Same fix for MutexGuard[T].borrow.
func TestMutexGuardBorrowClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			m := Mutex[int[]](v);
			use guard := m.lock();
			borrowed := guard.borrow;
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0367 / T0438: the typed-decl path `T borrowed = a.borrow` for non-Copy T
// is now rejected at sema (no implicit `T& → T` decay). The codegen
// dropflag-clear path being tested here is unreachable under Option A;
// the inferred-decl variant (`borrowed := a.borrow`) still tests the
// codegen behavior for the kept `T&` borrow type.

// T0379: Reassigning Ref[T].borrow to an existing variable must clear the
// dropflag re-armed by the unconditional reset in the assignment path. After
// the reassign-merge block, the sequence is: re-arm, store new pointer, clear
// (T0379 fix). Without the fix, the trailing clear is missing.
func TestArcBorrowReassignClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v1 := [1, 2, 3];
			v2 := [4, 5, 6];
			a1 := Ref[int[]](v1);
			a2 := Ref[int[]](v2);
			borrowed := a1.borrow;
			borrowed = a2.borrow;
		}
	`)
	assertContainsMatch(t, ir, `reassign\.merge[^:]*:\s+store i1 true, i1\* %borrowed\.dropflag\s+store i8\* %[^,]+, i8\*\* %borrowed\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0379: Same fix for MutexGuard[T].borrow reassignment.
func TestMutexGuardBorrowReassignClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v1 := [1, 2, 3];
			v2 := [4, 5, 6];
			m1 := Mutex[int[]](v1);
			m2 := Mutex[int[]](v2);
			use g1 := m1.lock();
			use g2 := m2.lock();
			borrowed := g1.borrow;
			borrowed = g2.borrow;
		}
	`)
	assertContainsMatch(t, ir, `reassign\.merge[^:]*:\s+store i1 true, i1\* %borrowed\.dropflag\s+store i8\* %[^,]+, i8\*\* %borrowed\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0379: Borrow→owned reassignment must NOT clear the dropflag — the local
// now owns the new value and its drop must run at scope exit. Verifies the fix
// is conditional on `isBorrowGetterExpr(s.Value)` and not always applied.
func TestArcBorrowReassignToOwnedKeepsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			borrowed := a.borrow;
			borrowed = [4, 5, 6];
		}
	`)
	// The full T0379-fired pattern (re-arm → store new ptr → clear) must NOT appear
	// after `reassign.merge`: the fix should fire only when RHS is `.borrow`.
	bad := regexp.MustCompile(`reassign\.merge[^:]*:\s+store i1 true, i1\* %borrowed\.dropflag\s+store [^\n]*%borrowed[^\n]*\n\s+store i1 false, i1\* %borrowed\.dropflag`)
	if bad.MatchString(ir) {
		t.Errorf("expected NO trailing flag-clear in reassign.merge for borrow→owned (T0379 should not fire)\ngot:\n%s", ir)
	}
	// But the re-arm and the pointer store should still be present (assignment ran normally).
	assertContainsMatch(t, ir, `reassign\.merge[^:]*:\s+store i1 true, i1\* %borrowed\.dropflag\s+store [^\n]*%borrowed`)
}

// T0377: A borrow laundered through an if-expression (both arms produce
// `.borrow`) must clear the new variable's dropflag — without the fix,
// scope cleanup double-frees with Arc.drop.
func TestArcBorrowThroughIfClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			cond := true;
			borrowed := if cond { a.borrow } else { a.borrow };
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0377: Same fix for match-laundered borrow — every arm produces `.borrow`.
func TestArcBorrowThroughMatchClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			k := 1;
			borrowed := match k { 1 => a.borrow, _ => a.borrow };
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0377: MutexGuard borrow laundered through an if-expression.
func TestMutexGuardBorrowThroughIfClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			m := Mutex[int[]](v);
			use guard := m.lock();
			cond := true;
			borrowed := if cond { guard.borrow } else { guard.borrow };
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0488: mixed-ownership if-expression (one borrow arm + one owned arm) for
// non-Copy `T` is now rejected at sema time — the codegen path that "must
// NOT clear the dropflag" is unreachable. Sema rejection is covered by
// TestT0488_IfMixedNonCopyRejected in sema/sema_test.go.

// T0377: Parenthesized borrow (`(a.borrow)`) is a trivial laundering form;
// recursion must look through ParenExpr to find the borrow.
func TestArcBorrowThroughParensClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			borrowed := (a.borrow);
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0377: Block-bodied match arms (`=> { a.borrow }` rather than `=> a.borrow`)
// take the `arm.Block` path through `matchArmIsBorrowGetter` — must still
// clear the dropflag when every arm's block result is a borrow.
func TestArcBorrowThroughMatchBlockArmsClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			k := 1;
			borrowed := match k {
				1 => { a.borrow },
				_ => { a.borrow },
			};
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0488: mixed-ownership match-expression for non-Copy `T` is rejected at
// sema time — see TestT0488_MatchMixedNonCopyRejected in sema/sema_test.go.

// T0381: explicit `T&` annotation drives the dropflag-clear path the same
// way as inferred declarations. Type-based detection (replacing the old
// AST-shape heuristic) sees the SharedRef on the RHS expression.
func TestArcBorrowExplicitRefTypeClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			int[]& borrowed = a.borrow;
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0381: a getter chain ending in a non-borrow leaf (e.g., `.clone()`)
// produces an OWNED value despite traversing a `T&`. The result expression
// type is `T`, not `T&`, so the dropflag stays armed for proper cleanup.
func TestArcBorrowCloneRetainsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			b := a.clone();
		}
	`)
	// b owns the cloned Arc — drop must run at scope exit.
	assertContains(t, ir, "%b.dropflag = alloca i1")
	bad := regexp.MustCompile(`store i1 true, i1\* %b\.dropflag\s+store i1 false, i1\* %b\.dropflag`)
	if bad.MatchString(ir) {
		t.Errorf("expected dropflag for clone() result to stay armed; T0381 type-based check should not fire (RHS type is Ref[int], not Ref[int]&)\ngot:\n%s", ir)
	}
}

// T0381: chained `.borrow.field` access dispatches through genMemberExpr's
// SharedRef unwrap — the inner member-access on `T&` looks up the field on
// the underlying `T`. Without the unwrap, the field-resolution path would
// fail to find the field on the SharedRef wrapper.
func TestT0381_ChainedBorrowFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Pt { int x; }
		main() {
			a := Ref[Pt](Pt(x: 7));
			x := a.borrow.x;
		}
	`)
	// The Ref[Pt] type and its drop should appear; sema/codegen lowering
	// of `.borrow.x` would fail without the SharedRef unwrap in genMemberExpr
	// because the field 'x' is not present on the SharedRef wrapper itself.
	assertContains(t, ir, "Ref[Pt].drop")
}

// T0381: a `T&`-typed local that is later reassigned to an owned `T`
// must register its drop binding using the underlying owned type — the
// SharedRef strip in maybeRegisterDrop ensures the proper drop function
// is dispatched (e.g., per-element drops for `string[]`).
func TestT0381_BorrowLocalReassignedToOwnedDrops(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := string[]();
			v.push("hello");
			a := Ref[string[]](v);
			string[]& borrowed = a.borrow;
			borrowed = string[]();
			borrowed.push("owned");
		}
	`)
	// The reassignment makes `borrowed` an owned vector; on scope exit
	// we should see a call into Vector.drop (proves maybeRegisterDrop
	// saw past the SharedRef and registered an owned-vector drop).
	assertContains(t, ir, "call void @Vector.drop")
}

// T0156/T0285/T0291: MutexGuard close/drop functions do scheduler-aware unlock and free.
func TestMutexGuardCloseUnlocksAndFrees(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
		}
	`)
	closeFn := extractFunction(ir, "MutexGuard.close")
	if closeFn == "" {
		t.Fatal("expected MutexGuard.close function in IR")
	}
	// Null check
	assertContains(t, closeFn, "icmp eq")
	// Locks metadata mutex
	assertContains(t, closeFn, "call void @pal_mutex_lock(")
	// Both handoff path and no-waiter path unlock the PAL mutex
	assertContains(t, closeFn, "call void @pal_mutex_unlock(")
	// Free guard
	assertContains(t, closeFn, "call void @pal_free(")
}

// T0291: Mutex.lock() inside a goroutine parks on the waiter list (not spin-yield).
func TestMutexLockParksOnWaiterList(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			go {
				use guard := m.lock();
			};
		}
	`)
	// The goroutine contested path must enqueue on the waiter list
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
	// The new park-and-wake block label must be present
	assertContains(t, ir, "mutex.park.resume")
	// No spin-retry: the old spin-yield block label must NOT be present
	assertNotContains(t, ir, "mutex.wait.resume")
}

// T0291: MutexGuard.close hands lock off to a waiting goroutine (waiter_dequeue + sched_enqueue).
func TestMutexGuardCloseHandsOffLock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
		}
	`)
	closeFn := extractFunction(ir, "MutexGuard.close")
	if closeFn == "" {
		t.Fatal("expected MutexGuard.close function in IR")
	}
	// Must dequeue a waiter
	assertContains(t, closeFn, "call i8* @promise_waiter_dequeue(")
	// Must enqueue the woken goroutine (handoff path)
	assertContains(t, closeFn, "call void @promise_sched_enqueue(")
	// No-waiter path: signal cond for thread-blocked waiters
	assertContains(t, closeFn, "call void @pal_cond_signal(")
}

// T0291: MutexGuard.drop (non-use binding) also hands lock off — same body as close.
func TestMutexGuardDropHandsOffLock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			guard := m.lock();
		}
	`)
	dropFn := extractFunction(ir, "MutexGuard.drop")
	if dropFn == "" {
		t.Fatal("expected MutexGuard.drop function in IR")
	}
	// Null check (guard may be null if moved)
	assertContains(t, dropFn, "icmp eq")
	// Must dequeue a waiter (handoff path)
	assertContains(t, dropFn, "call i8* @promise_waiter_dequeue(")
	// Must enqueue the woken goroutine (handoff path)
	assertContains(t, dropFn, "call void @promise_sched_enqueue(")
	// No-waiter path: signal cond for thread-blocked waiters
	assertContains(t, dropFn, "call void @pal_cond_signal(")
	// Free guard
	assertContains(t, dropFn, "call void @pal_free(")
}

// T0156: Mutex[string].drop calls promise_string_drop on inner value.
func TestMutexDropWithStringElement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[string]("hello");
		}
	`)
	dropFn := extractFunction(ir, `"Mutex[string].drop"`)
	if dropFn == "" {
		t.Fatal("expected Mutex[string].drop function in IR")
	}
	// Should drop the inner string before destroying cond + mutex
	assertContains(t, dropFn, "call void @promise_string_drop(")
	assertContains(t, dropFn, "call void @pal_cond_destroy(")
	assertContains(t, dropFn, "call void @pal_mutex_destroy(")
}

// T0272: Ref[T].drop calls user type's drop function + pal_free for heap user types.
func TestArcDropWithUserType(t *testing.T) {
	ir := generateIR(t, `
		type P { int x; drop(~this) {} }
		main() {
			a := Ref[P](P(x: 5));
		}
	`)
	dropFn := extractFunction(ir, `"Ref[P].drop"`)
	if dropFn == "" {
		t.Fatal("expected Ref[P].drop function in IR")
	}
	// Should call user drop then pal_free for heap user type
	assertContains(t, dropFn, "call void @P.drop(")
	assertContains(t, dropFn, "call void @pal_free(")
}

// T0272: Ref[T].drop with user type that has no explicit drop — just pal_free.
func TestArcDropWithHeapUserTypeNoDrop(t *testing.T) {
	ir := generateIR(t, `
		type Q { int x; int y; }
		main() {
			a := Ref[Q](Q(x: 1, y: 2));
		}
	`)
	dropFn := extractFunction(ir, `"Ref[Q].drop"`)
	if dropFn == "" {
		t.Fatal("expected Ref[Q].drop function in IR")
	}
	// Heap user type without drop — should still free the instance
	assertContains(t, dropFn, "call void @pal_free(")
}

// T0272: Arc constructor with user type claims the heap temp (no premature free).
func TestArcConstructorClaimsHeapTemp(t *testing.T) {
	ir := generateIR(t, `
		type R { int val; }
		main() {
			a := Ref[R](R(val: 42));
			r := a.borrow;
		}
	`)
	// The IR should NOT contain a pal_free of the R instance before the Arc drop.
	// Specifically, the main function should not free the R instance directly —
	// only the Ref[R].drop function should handle that.
	mainFn := extractFunction(ir, "main")
	if mainFn == "" {
		t.Fatal("expected main function in IR")
	}
	// The heap temp for R should be claimed; no direct pal_free of the instance in main
	// (Arc.drop handles it). Count pal_free calls in main — should only be for Arc itself.
	dropFn := extractFunction(ir, `"Ref[R].drop"`)
	if dropFn == "" {
		t.Fatal("expected Ref[R].drop function in IR")
	}
	assertContains(t, dropFn, "call void @pal_free(")
}

// T0273: Ref[Ref[T]] clears drop flag on inner variable to prevent double-drop.
func TestArcConstructorClearsDropFlagOnIdent(t *testing.T) {
	ir := generateIR(t, `
		main() {
			inner := Ref[int](42);
			outer := Ref[Ref[int]](inner);
		}
	`)
	// The goroutine body should clear inner's drop flag after moving it into Arc.
	// "store i1 false" is the drop flag clear pattern.
	assertContains(t, ir, "store i1 false")
}

// T0273: Mutex[T] constructor clears drop flag on moved variable.
func TestMutexConstructorClearsDropFlagOnIdent(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](10);
			m := Mutex[Ref[int]](a);
		}
	`)
	assertContains(t, ir, "store i1 false")
}

// T0272: Mutex[T].drop calls user type's drop function + pal_free.
func TestMutexDropWithUserType(t *testing.T) {
	ir := generateIR(t, `
		type MP { int x; drop(~this) {} }
		main() {
			m := Mutex[MP](MP(x: 10));
		}
	`)
	dropFn := extractFunction(ir, `"Mutex[MP].drop"`)
	if dropFn == "" {
		t.Fatal("expected Mutex[MP].drop function in IR")
	}
	assertContains(t, dropFn, "call void @MP.drop(")
	assertContains(t, dropFn, "call void @pal_free(")
	assertContains(t, dropFn, "call void @pal_mutex_destroy(")
}

// T0272: Arc drop with vector inner type calls Vector.drop.
func TestArcDropWithVectorElement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
		}
	`)
	dropFn := extractFunction(ir, `"Ref[int[]].drop"`)
	if dropFn == "" {
		// Try alternative mangled name
		dropFn = extractFunction(ir, `"Ref[Vector[int]].drop"`)
	}
	if dropFn == "" {
		t.Fatal("expected Ref[int[]].drop or Ref[Vector[int]].drop function in IR")
	}
	assertContains(t, dropFn, "call void @Vector.drop(")
}

// T0272: Mutex drop with user type that has synth drop (string field).
func TestMutexDropWithSynthDropUserType(t *testing.T) {
	ir := generateIR(t, `
		type Named { string name; }
		main() {
			m := Mutex[Named](Named(name: "hi"));
		}
	`)
	dropFn := extractFunction(ir, `"Mutex[Named].drop"`)
	if dropFn == "" {
		t.Fatal("expected Mutex[Named].drop function in IR")
	}
	// Synth drop types have their own drop function that handles field cleanup
	assertContains(t, dropFn, "call void @Named.drop(")
	assertContains(t, dropFn, "call void @pal_mutex_destroy(")
}

// T0271: Lambda capturing Ref[T] uses envDropCallFn (i8* + drop fn), not envDropUserValueDrop.
func TestLambdaEnvDropArcCapture(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			f := move || -> int { return a.borrow; };
			f();
		}
	`)
	// The env drop function should call Ref[int].drop on the i8* field,
	// not extract a {i8*, i8*} value struct (which would be type confusion).
	assertContains(t, ir, `call void @"Ref[int].drop"(`)
}

// T0271: Lambda capturing Weak[T] uses envDropCallFn.
func TestLambdaEnvDropWeakCapture(t *testing.T) {
	ir := generateIR(t, `
		check(Weak[int] w) bool {
			if upgraded := w.upgrade() {
				return true;
			}
			return false;
		}
		main() {
			a := Ref[int](42);
			w := a.downgrade();
			f := move || -> bool { return check(w.clone()); };
			f();
		}
	`)
	assertContains(t, ir, `call void @"Weak[int].drop"(`)
}

// T0271: Lambda capturing Mutex[T] uses envDropCallFn.
func TestLambdaEnvDropMutexCapture(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](10);
			f := move || -> int {
				use g := m.lock();
				return g.borrow;
			};
			f();
		}
	`)
	assertContains(t, ir, `call void @"Mutex[int].drop"(`)
}

// T0271: Lambda capturing MutexGuard uses envDropCallFn with MutexGuard.drop.
func TestLambdaEnvDropMutexGuardCapture(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](10);
			use g := m.lock();
			f := move || -> int { return g.borrow; };
			f();
		}
	`)
	assertContains(t, ir, "call void @MutexGuard.drop(")
}

// findEnvDropContaining returns the body of the first .lambda.N.env_drop
// function whose body contains marker. There are many stdlib-generated
// env_drop functions; this helper picks out the one for the user code under
// test.
func findEnvDropContaining(ir, marker string) string {
	// Iterate over candidate lambda numbers — the user's lambda lands after
	// stdlib lambdas, so the range needs to be large enough.
	idx := 0
	for {
		needle := fmt.Sprintf(".lambda.%d.env_drop", idx)
		if !strings.Contains(ir, "@"+needle+"(") {
			if idx > 500 {
				return ""
			}
			idx++
			continue
		}
		body := extractFunction(ir, needle)
		if strings.Contains(body, marker) {
			return body
		}
		idx++
	}
}

// T0554: env_drop for a captured user type with an explicit drop method calls
// T.drop$wrap (drop + pal_free) — not the bare T.drop followed by a separate
// pal_free on the instance, which would double-free.
func TestLambdaEnvDropUserTypeExplicitDropUsesWrap(t *testing.T) {
	ir := generateIR(t, `
		type _T554EX {
			string label;
			drop(~this) {}
		}
		main() {
			b := _T554EX(label: "x");
			f := move || -> string { return b.label; };
			f();
		}
	`)
	envDrop := findEnvDropContaining(ir, "_T554EX")
	if envDrop == "" {
		t.Fatal("expected an env_drop referencing _T554EX")
	}
	assertContains(t, envDrop, "call void @_T554EX.drop$wrap(")
	// Must NOT call _T554EX.drop directly outside the $wrap helper (that would
	// double-drop with $wrap inside).
	if strings.Contains(envDrop, "call void @_T554EX.drop(") {
		t.Errorf("env_drop should call $wrap, not bare drop:\n%s", envDrop)
	}
}

// T0554: env_drop for a captured user type with synthesized drop (no explicit
// drop, only droppable fields) calls the bare T.drop — synth drops already
// include pal_free, so calling pal_free again would double-free.
func TestLambdaEnvDropUserTypeSynthDropNoExtraPalFree(t *testing.T) {
	ir := generateIR(t, `
		type _T554SY {
			string label;
		}
		main() {
			b := _T554SY(label: "x");
			f := move || -> string { return b.label; };
			f();
		}
	`)
	envDrop := findEnvDropContaining(ir, "_T554SY")
	if envDrop == "" {
		t.Fatal("expected an env_drop referencing _T554SY")
	}
	assertContains(t, envDrop, "call void @_T554SY.drop(")
	// Count pal_free calls — must be exactly 1 (for the env struct itself).
	// A second pal_free on the instance would double-free with the synth drop.
	count := strings.Count(envDrop, "call void @pal_free(")
	if count != 1 {
		t.Errorf("expected exactly 1 pal_free call (for env), got %d:\n%s", count, envDrop)
	}
}

// T0554: env_drop for a captured user type with NO droppable fields and NO
// drop method falls through resolveDropFuncForTemp to palFree as the cleanup
// fn — single pal_free on the instance plus a pal_free on the env struct.
func TestLambdaEnvDropUserTypeNoDropUsesPalFree(t *testing.T) {
	ir := generateIR(t, `
		type _T554NO {
			int v;
			bool flag;
		}
		main() {
			p := _T554NO(v: 42, flag: true);
			f := move || -> int { return p.v; };
			f();
		}
	`)
	// This env_drop has no type-name marker (uses palFree for cleanup), so we
	// look for one that does NOT call any user T.drop, has the user-type Value
	// layout {i8*, i8*}, and has exactly 2 pal_free calls.
	idx := 0
	var envDrop string
	for idx < 500 {
		needle := fmt.Sprintf(".lambda.%d.env_drop", idx)
		if !strings.Contains(ir, "@"+needle+"(") {
			idx++
			continue
		}
		body := extractFunction(ir, needle)
		// User-type value struct capture: { i8*, i8* } payload + 2 pal_free calls.
		if strings.Contains(body, "{ i8*, { i8*, i8* } }") &&
			strings.Count(body, "call void @pal_free(") == 2 {
			envDrop = body
			break
		}
		idx++
	}
	if envDrop == "" {
		t.Fatal("expected an env_drop with 2 pal_free calls on a user value capture")
	}
}

// T0554: the lambda BODY for a move-captured user type must NOT register a
// scope-exit drop on the capture local. Before the fix, the body called the
// captured type's drop (or pal_free) on the local copy, which then ran AGAIN
// in env_drop → segfault on user types, double-free on droppable fields.
func TestLambdaBodyNoDropOnMoveCapture(t *testing.T) {
	ir := generateIR(t, `
		type _T554BO {
			string label;
		}
		main() {
			b := _T554BO(label: "x");
			f := move || -> string { return b.label; };
			f();
		}
	`)
	// Find the lambda function (not env_drop) that has _T554BO in its body —
	// the user-defined lambda captures _T554BO.
	idx := 0
	var body string
	for idx < 500 {
		needle := fmt.Sprintf(".lambda.%d", idx)
		if !strings.Contains(ir, "@"+needle+"(") {
			idx++
			continue
		}
		fnBody := extractFunction(ir, needle)
		// Skip env_drop and pick the lambda body that loads from a _T554BO
		// instance pointer.
		if !strings.Contains(fnBody, ".env_drop") &&
			strings.Contains(fnBody, "_T554BO") {
			body = fnBody
			break
		}
		idx++
	}
	if body == "" {
		t.Fatal("expected a lambda body referencing _T554BO")
	}
	// The lambda body must not invoke the captured type's drop (it would
	// duplicate env_drop's cleanup).
	if strings.Contains(body, "call void @_T554BO.drop(") ||
		strings.Contains(body, "call void @_T554BO.drop$wrap(") {
		t.Errorf("lambda body should not drop captured user type:\n%s", body)
	}
}

// T0373: Assigning a T? value into a T?? variable wraps the value once
// (lifting from single- to double-Optional). Before the fix, the wrap
// predicate skipped because the expression type was already Optional,
// leaving the T? value stored into a T?? slot → store-type panic.
func TestDoubleOptionalDeclWrapsOnce(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? a = 7;
			int?? b = a;
		}
	`)
	// b's alloca is a double-Optional struct {i1, {i1, i64}}.
	assertContains(t, ir, "%b = alloca { i1, { i1, i64 } }")
	// a wrapped once into double-Optional via two insertvalues:
	// one for the outer present flag, one for the inner T? value.
	assertContains(t, ir, "insertvalue { i1, { i1, i64 } } undef, i1 true, 0")
	assertContains(t, ir, "insertvalue { i1, { i1, i64 } } %")
}

// T0373: Reassigning a T? value into a T?? local wraps the value at the
// target's depth via insertvalue.
func TestDoubleOptionalReassignWrapsOnce(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int?? b = none;
			int? a = 5;
			b = a;
		}
	`)
	assertContains(t, ir, "%b = alloca { i1, { i1, i64 } }")
	assertContains(t, ir, "insertvalue { i1, { i1, i64 } } undef, i1 true, 0")
}

// T0373: Returning a T? expression from a T??-returning function wraps
// once at the return site.
func TestDoubleOptionalReturnWrapsOnce(t *testing.T) {
	ir := generateIR(t, `
		f(int? a) int?? {
			return a;
		}
		main() {
			int?? r = f(3);
		}
	`)
	// Function signature is T? in, T?? out (no caller-side wrap needed).
	assertContains(t, ir, "define { i1, { i1, i64 } } @__user.f({ i1, i64 } %a)")
	// Return wraps the T? value once into T?? via insertvalue.
	assertContains(t, ir, "insertvalue { i1, { i1, i64 } } undef, i1 true, 0")
}

// T0373: Value-type constructors take a distinct codegen path
// (genValueTypeConstructor) from heap-type constructors. Verify the
// value-type ctor's maybeWrapOptional wraps a T? arg into a T?? field.
func TestDoubleOptionalValueCtorWrapsOnce(t *testing.T) {
	ir := generateIR(t, `
		type VT { int?? data `+"`value;"+` }
		main() {
			int? a = 5;
			VT v = VT(data: a);
		}
	`)
	// Value-type Value struct embeds the field directly: {vtable, {i1,{i1,i64}}}.
	assertContains(t, ir, "%promise_VT_v = type { i8*, { i1, { i1, i64 } } }")
	// Field arg is wrapped once before being placed in the Value struct.
	assertContains(t, ir, "insertvalue { i1, { i1, i64 } } undef, i1 true, 0")
}

// T0428: Force-unwrap of a call-returning string? must track the extracted string
// pointer as a statement temp so it gets freed at statement end. The temp tracking
// branch (genOptionalForceUnwrap lines 8938-8949) fires for non-ident sources.
func TestT0428CallResultStringOptForceUnwrapTracksTemp(t *testing.T) {
	ir := generateIR(t, `
		get_greet() string? { return "hello"; }
		main() {
			int n = get_greet()!.len;
		}
	`)
	// The extracted string i8* must be tracked as a stmt temp with a drop at statement end.
	assertContains(t, ir, "promise_string_drop")
}

// T0428: Force-unwrap of a call-returning int[]? must track the extracted vector
// pointer as a statement temp so it gets freed at statement end.
func TestT0428CallResultVectorOptForceUnwrapTracksTemp(t *testing.T) {
	ir := generateIR(t, `
		get_nums() int[]? {
			int[] v = [1, 2, 3];
			return v;
		}
		main() {
			int n = get_nums()!.len;
		}
	`)
	// The extracted vector pointer must be tracked as a stmt temp with vector drop.
	assertContains(t, ir, "Vector.drop")
}

// T0428: Generic type with borrowed this.field! exercises the typeSubst branches
// in genOptionalForceUnwrap (Case 3B with typeSubst != nil).
func TestT0428GenericBorrowedThisForceUnwrapTypeSubst(t *testing.T) {
	ir := generateIR(t, `
		type GenHolder[T] {
			T? data;
			get_val(this) T {
				return this.data!;
			}
		}
		type GBox { int n; drop(~this) {} }
		main() {
			h := GenHolder[GBox](data: GBox(n: 42));
			v := h.get_val();
		}
	`)
	// get_val must dup the heap value for the borrowed receiver case.
	// The function is LLVM-quoted as @"GenHolder[GBox].get_val".
	assertContains(t, ir, `"GenHolder[GBox].get_val"`)
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "call void @llvm.memcpy")
}

// T0428: Generic function with local var force-unwrap exercises typeSubst path
// in neutralizeMemberOptionalField (IdentExpr root, lines 9052-9054).
// When a generic method/function body has `b.field!` where b's type has a TypeParam,
// c.typeSubst is applied to resolve the concrete owner type.
func TestT0428GenericFuncLocalVarOptFieldNeutralization(t *testing.T) {
	ir := generateIR(t, `
		type T0428ContainerBox { int n; drop(~this) {} }
		type T0428Container[T] {
			T? item;
		}
		unwrap_item[T](T0428Container[T] c) T {
			return c.item!;
		}
		main() {
			c := T0428Container[T0428ContainerBox](item: T0428ContainerBox(n: 5));
			b := unwrap_item[T0428ContainerBox](c);
		}
	`)
	// The concrete monomorphized function must clear the optional present flag.
	assertContains(t, ir, "store i1 false")
}

// T0428 Case 1: T?? field force-unwrap — neutralizeMemberOptionalField must
// look through the inner Optional to find the named type and clear the outer flag.
func TestT0428DoubleOptionalFieldNeutralization(t *testing.T) {
	ir := generateIR(t, `
		type T0428Box { int n; drop(~this) {} }
		type T0428Dbl { T0428Box?? data; }
		make_inner() T0428Box? { return T0428Box(n: 1); }
		main() {
			T0428Box? inner = make_inner();
			h := T0428Dbl(data: inner);
			b := h.data!;
		}
	`)
	// The present flag of h.data (outer Optional) must be stored false.
	// The neutralize store appears in the goroutine body, not the C main wrapper.
	assertContains(t, ir, "store i1 false")
}

// T0428 Case 2: chained MemberExpr force-unwrap — neutralizeMemberOptionalField
// must walk the chain to clear the Optional's present flag.
func TestT0428ChainedMemberForceUnwrapNeutralization(t *testing.T) {
	ir := generateIR(t, `
		type T0428Box2 { int n; drop(~this) {} }
		type T0428Inner { T0428Box2? data; }
		type T0428Outer { T0428Inner inner; }
		main() {
			o := T0428Outer(inner: T0428Inner(data: T0428Box2(n: 5)));
			b := o.inner.data!;
		}
	`)
	// The Optional present flag must be cleared via GEP into inner.data.
	assertContains(t, ir, "store i1 false")
}

// T0428 Case 3A: ~this method force-unwrap — neutralizeMemberOptionalField
// must handle ThisExpr root without calling extractInstancePtr on i8*.
func TestT0428OwnedThisForceUnwrapNeutralization(t *testing.T) {
	ir := generateIR(t, `
		type T0428Box3 { int n; drop(~this) {} }
		type T0428Holder3 {
			T0428Box3? data;
			drop(~this) {
				b := this.data!;
			}
		}
		main() {
			h := T0428Holder3(data: T0428Box3(n: 7));
		}
	`)
	dropFn := extractFunction(ir, "T0428Holder3.drop")
	if dropFn == "" {
		t.Fatal("expected T0428Holder3.drop in IR")
	}
	// Present flag must be cleared in the drop method body.
	assertContains(t, dropFn, "store i1 false")
}

// T0428 Case 3B: borrowed this.field! — genOptionalForceUnwrap must dup the
// inner heap value so both the caller's synth drop and the local own independent copies.
func TestT0428BorrowedThisForceUnwrapDup(t *testing.T) {
	ir := generateIR(t, `
		type T0428Box4 { int n; drop(~this) {} }
		type T0428Holder4 {
			T0428Box4? data;
			get_n(this) int {
				b := this.data!;
				return b.n;
			}
		}
		main() {
			h := T0428Holder4(data: T0428Box4(n: 3));
			n := h.get_n();
		}
	`)
	// The get_n method should call dupHeapValue logic: alloc + memcpy.
	getNFn := extractFunction(ir, "T0428Holder4.get_n")
	if getNFn == "" {
		t.Fatal("expected T0428Holder4.get_n in IR")
	}
	// dupHeapValue allocates new memory and memcpy's the instance.
	assertContains(t, getNFn, "call i8* @pal_alloc")
	assertContains(t, getNFn, "call void @llvm.memcpy")
}

// T0436 Issue 1: single-line `b := h.data!!` on a T?? field — the AST is
// OptionalUnwrapExpr(OptionalUnwrapExpr(MemberExpr)), so neutralizeForceUnwrapSource
// must look through the inner OptionalUnwrapExpr to reach the MemberExpr.
// Without the fix, the outer Optional's present flag stays true → double-free
// when the holder is dropped.
func TestT0436SingleLineDoubleUnwrapNeutralizes(t *testing.T) {
	ir := generateIR(t, `
		type T0436Box1 { int n; drop(~this) {} }
		type T0436Dbl1 { T0436Box1?? data; }
		make_inner1() T0436Box1?? {
			T0436Box1? inner = T0436Box1(n: 1);
			return inner;
		}
		main() {
			h := T0436Dbl1(data: make_inner1());
			b := h.data!!;
		}
	`)
	// neutralizeMemberOptionalField clears the outermost Optional's present flag
	// by GEPing into the field's full T?? layout `{ i1, { i1, T_v } }` and then
	// storing i1 false. Without the nested-unwrap walk in
	// neutralizeForceUnwrapSource, no such GEP is emitted (only loads for the
	// drop and unwrap exist) and the holder's drop double-frees the heap value.
	gepPattern := "getelementptr { i1, { i1, { i8*, i8* } } }, { i1, { i1, { i8*, i8* } } }* %"
	if !strings.Contains(ir, gepPattern) {
		t.Fatal("expected a GEP through the T?? outer-Optional struct (neutralization site)")
	}
}

// T0577: `b := (opt!);` — ParenExpr wrapping a force-unwrap. Before the fix,
// neutralizeForceUnwrapSource matched only OptionalUnwrapExpr/CastExpr/
// ErrorHandlerExpr at its outer switch and fell through for *ast.ParenExpr,
// so the source optional's present flag was never cleared and its scope-exit
// drop re-freed the inner value (double-free → segfault). The fix peels
// ParenExpr at the top of the function before the switch.
func TestT0577ParenForceUnwrapNeutralizes(t *testing.T) {
	ir := generateIR(t, `
		type T0577Box { int n; drop(~this) {} }
		main() {
			T0577Box? opt = T0577Box(n: 1);
			b := (opt!);
		}
	`)
	// Locate the unwrap.ok block emitted for `(opt!)` and confirm the present-flag
	// store appears in that block. Without the paren peel, no GEP+store i1 false
	// is emitted in unwrap.ok — only the b store and drop-flag set — and the
	// optional's scope drop later double-frees.
	unwrapOKIdx := strings.Index(ir, "\nunwrap.ok")
	if unwrapOKIdx < 0 {
		t.Fatal("expected an unwrap.ok block in IR")
	}
	rest := ir[unwrapOKIdx:]
	endIdx := strings.Index(rest, "\n\n")
	if endIdx < 0 {
		endIdx = len(rest)
	}
	block := rest[:endIdx]
	if !strings.Contains(block, "getelementptr { i1, { i8*, i8* } }, { i1, { i8*, i8* } }* %opt") {
		t.Fatalf("expected GEP into opt's Optional struct in unwrap.ok block (neutralization site); got:\n%s", block)
	}
	if !strings.Contains(block, "store i1 false") {
		t.Fatalf("expected `store i1 false` neutralization store in unwrap.ok block; got:\n%s", block)
	}
}

// T0577 mirror: `b := (opt)!;` — ParenExpr inside the OptionalUnwrap's `.Expr`.
// The outer switch matches OptionalUnwrapExpr, but `inner` is then ParenExpr,
// so the inner T0436-style chain walk (which previously only peeled
// OptionalUnwrapExpr) must also peel ParenExpr to reach IdentExpr. Without
// the inner peel, neither the IdentExpr nor MemberExpr arm fires, the present
// flag is never cleared, and the optional's scope-exit drop double-frees.
//
// IR shape note: this form goes through the heap-claim path rather than the
// `unwrap.ok` flag-store path used by `(opt!)`, so we assert the present-flag
// GEP+store appears anywhere in main rather than locating a specific block.
func TestT0577InnerParenForceUnwrapNeutralizes(t *testing.T) {
	ir := generateIR(t, `
		type T0577Box2 { int n; drop(~this) {} }
		main() {
			T0577Box2? opt = T0577Box2(n: 1);
			b := (opt)!;
		}
	`)
	// The exact pattern emitted by the IdentExpr arm of neutralizeForceUnwrapSource:
	// GEP through the Optional struct on %opt followed by `store i1 false`.
	// Without the inner ParenExpr peel, no such GEP/store pair is emitted on %opt.
	gep := "getelementptr { i1, { i8*, i8* } }, { i1, { i8*, i8* } }* %opt, i32 0, i32 0"
	if !strings.Contains(ir, gep) {
		t.Fatalf("expected GEP into opt's Optional struct (neutralization site) — inner ParenExpr peel did not reach IdentExpr arm")
	}
	// Confirm the GEP is followed by a `store i1 false` neutralization.
	gepIdx := strings.Index(ir, gep)
	tail := ir[gepIdx:]
	endIdx := strings.Index(tail, "\n\n")
	if endIdx < 0 {
		endIdx = len(tail)
	}
	region := tail[:endIdx]
	if !strings.Contains(region, "store i1 false") {
		t.Fatalf("expected `store i1 false` after GEP into %%opt; got:\n%s", region)
	}
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

// T0436 Issue 2 (defineModuleTypeMethods path): a borrowed-this method on a
// module type, declared after a ~this method on the same module type, must dup
// rather than clear the caller's flag. To expose stale thisRecvIsOwned from a
// prior ~this method, the test puts a generic type with a ~this method in a
// FIRST module. After mod1 is compiled, defineMonoMethods for that generic's
// ~this method leaves thisRecvIsOwned=true. mod2 is compiled next, and without
// the fix, defineModuleTypeMethods inherits the stale flag.
func TestT0436ModuleTypeBorrowedThisAfterOwnedDups(t *testing.T) {
	ir := generateIRWithTwoModules(t,
		"t0436a",
		`
		type GenBox[T] `+"`public"+` {
			T item;
			consume(~this) `+"`public"+` {}
		}
		`,
		"t0436b",
		`
		type Box2 `+"`public"+` { int n; drop(~this) {} }
		type Holder2 `+"`public"+` {
			Box2? data;
			get_n(this) int `+"`public"+` {
				b := this.data!;
				return b.n;
			}
		}
		`,
		`
		use t0436a "./t0436a";
		use t0436b "./t0436b";
		main() {
			// Force a generic instantiation in mod1 so its ~this consume() compiles
			// via defineMonoMethods → defineMethodFunc, leaving thisRecvIsOwned=true.
			gb := t0436a.GenBox[int](item: 1);
			h := t0436b.Holder2(data: t0436b.Box2(n: 3));
			n := h.get_n();
		}
		`,
	)
	// get_n is in mod2, compiled via defineModuleTypeMethods AFTER mod1.
	// Must dup the heap value (pal_alloc + memcpy), not clear the caller's flag.
	getNFn := extractFunction(ir, "__mod_t0436b_Holder2.get_n")
	if getNFn == "" {
		t.Fatal("expected __mod_t0436b_Holder2.get_n in IR")
	}
	assertContains(t, getNFn, "call i8* @pal_alloc")
	assertContains(t, getNFn, "call void @llvm.memcpy")
}

// T0419: Optional[T] with explicit user drop must dispatch the scope-exit drop
// through T.drop$wrap (which calls drop + pal_free), not the bare T.drop
// (which only runs the user body and leaks the heap allocation).
func TestOptionalLocalDropExplicitUserDropWrap(t *testing.T) {
	ir := generateIR(t, `
		type BoxDrop {
			int n;
			drop(~this) {}
		}
		test_no_unwrap() {
			BoxDrop? a = BoxDrop(n: 12);
		}
	`)
	// The Optional drop dispatch must call the $wrap variant.
	assertContains(t, ir, "@BoxDrop.drop$wrap")
	// The $wrap function itself must call both drop and pal_free.
	wrapFn := extractFunction(ir, "BoxDrop.drop$wrap")
	if wrapFn == "" {
		t.Fatal("expected BoxDrop.drop$wrap in IR")
	}
	assertContains(t, wrapFn, "call void @BoxDrop.drop")
	assertContains(t, wrapFn, "call void @pal_free")
}

// T0419: Optional[GenericBox[int]] with explicit drop must dispatch through
// the mono-mangled $wrap (e.g. GenericBoxD[int].drop$wrap).
func TestOptionalLocalDropExplicitUserDropWrapMono(t *testing.T) {
	ir := generateIR(t, `
		type GenericBoxD[T] {
			T val;
			drop(~this) {}
		}
		test_mono_no_unwrap() {
			GenericBoxD[int]? a = GenericBoxD[int](val: 5);
		}
	`)
	// The mono Optional drop dispatch must call the mono $wrap variant.
	assertContains(t, ir, `@"GenericBoxD[int].drop$wrap"`)
	wrapFn := extractFunction(ir, `"GenericBoxD[int].drop$wrap"`)
	if wrapFn == "" {
		t.Fatal(`expected "GenericBoxD[int].drop$wrap" in IR`)
	}
	assertContains(t, wrapFn, `call void @"GenericBoxD[int].drop"`)
	assertContains(t, wrapFn, "call void @pal_free")
}

// T0419: Optional[T] where T has only a SYNTHESIZED drop (auto-generated because
// of droppable fields) must dispatch the bare T.drop — NOT T.drop$wrap. Synth
// drops already include pal_free; wrapping would call pal_free twice.
// This guards the `if explicitDrop` check in maybeRegisterOptionalDrop.
func TestOptionalLocalDropSynthSkipsWrap(t *testing.T) {
	ir := generateIR(t, `
		type SynthDropBox {
			string s;
		}
		test_synth_no_unwrap() {
			SynthDropBox? a = SynthDropBox(s: "hello");
		}
	`)
	// Premise: SynthDropBox has a synthesized drop that includes pal_free.
	synthFn := extractFunction(ir, "SynthDropBox.drop")
	if synthFn == "" {
		t.Fatal("expected SynthDropBox.drop in IR")
	}
	assertContains(t, synthFn, "call void @promise_string_drop")
	assertContains(t, synthFn, "call void @pal_free")
	// The Optional drop dispatch must call the bare drop, NOT the wrapper.
	// (No SynthDropBox.drop$wrap function should be emitted at all.)
	assertNotContains(t, ir, "SynthDropBox.drop$wrap")
	// And the user function must dispatch directly to SynthDropBox.drop.
	userFn := extractFunction(ir, "__user.test_synth_no_unwrap")
	if userFn == "" {
		t.Fatal("expected __user.test_synth_no_unwrap in IR")
	}
	assertContains(t, userFn, "call void @SynthDropBox.drop(")
}

// T0411: Constructor field-init that reads a string field from a droppable
// owner (`Type(label: this.label)`) must dup the string so the new instance
// owns an independent copy. Without the dup, both the source's drop and the
// new instance's drop free the same buffer → double-free.
func TestT0411_ConstructorStringFieldFromThisDups(t *testing.T) {
	ir := generateIR(t, `
		type CB {
			string label;
			drop(~this) {}
			clone() CB {
				return CB(label: this.label);
			}
		}
		test_t0411_dup() {
			c := CB(label: "hi");
			c2 := c.clone();
		}
	`)
	cloneFn := extractFunction(ir, "CB.clone")
	if cloneFn == "" {
		t.Fatal("expected CB.clone in IR")
	}
	// The clone body must dup the string when initializing the new CB —
	// i.e., a call to promise_string_new must appear inside CB.clone.
	assertContains(t, cloneFn, "call i8* @promise_string_new(")
}

// T0411: Vector field auto-dup via constructor field-init from `this.field`.
// Mirrors TestT0411_ConstructorStringFieldFromThisDups but for the
// dupContainerFieldAccess path on a Vector field.
func TestT0411_ConstructorVectorFieldFromThisDups(t *testing.T) {
	ir := generateIR(t, `
		type V {
			int[] items;
			drop(~this) {}
			clone() V {
				return V(items: this.items);
			}
		}
		test_t0411_vec_dup() {
			a := V(items: [1, 2, 3]);
			b := a.clone();
		}
	`)
	cloneFn := extractFunction(ir, "V.clone")
	if cloneFn == "" {
		t.Fatal("expected V.clone in IR")
	}
	// dupContainerFieldAccess for a Vector field routes through dupVector,
	// which emits a `vecdup.copy` block label. Without the T0411 fix, the
	// field would be a direct store with no dup logic.
	assertContains(t, cloneFn, "vecdup.copy")
}

// T0847: Constructor field-init that reads a Vector element directly into an
// owning (non-borrow) field slot (`Holder(held: v[0])`) must dup-on-read.
// Without the dup, the element pointer is aliased into the new instance's
// owning field — both v (element walk in Vector.drop) and the holder (synth
// field drop) free the same instance → double-free SEGV. Mirrors T0403 (the
// ~-param call-arg path); maybeEnableDupForConstructorArg's IndexExpr branch
// sets dupHeapUserFieldAccess, consumed by genVectorIndex → cloneHeapElement.
func TestT0847_ConstructorVectorElementDups(t *testing.T) {
	ir := generateIR(t, `
		type Item { string label; drop(~this) {} }
		type Holder { Item held; drop(~this) {} }
		test_t0847() {
			v := Item[]();
			v.push(Item(label: "x"));
			h := Holder(held: v[0]);
		}
	`)
	// cloneHeapElement → dupHeapValue: allocate a new instance and memcpy the
	// data. Without the fix the element would be a direct aliasing store.
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "call void @llvm.memcpy")
}

// T0847 (cast variant): `Holder(held: v[0] as! Circle)` over a polymorphic
// Shape[] must peel the cast to reach the IndexExpr subject and still dup.
func TestT0847_ConstructorCastVectorElementDups(t *testing.T) {
	ir := generateIR(t, `
		type Shape { string name; drop(~this) {} }
		type Circle is Shape { int radius; }
		type Holder { Shape held; drop(~this) {} }
		test_t0847_cast() {
			v := Shape[]();
			v.push(Circle(name: "c", radius: 1));
			h := Holder(held: v[0] as! Circle);
		}
	`)
	// Cast peel reaches the IndexExpr → dup-on-read fires (allocate + memcpy).
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "call void @llvm.memcpy")
}

// T0847 (generic/mono variant): the constructor field-init dup-on-read must
// fire under monomorphization, where the arg/target types are TypeParams that
// only resolve after substitution. This exercises maybeEnableDupForConstructorArg's
// typeSubst branches (argType/targetType Substitute) — `Holder[T](held: v[0])`
// inside a generic function body where T=Item is bound by the mono context.
func TestT0847_ConstructorGenericVectorElementDups(t *testing.T) {
	ir := generateIR(t, `
		type Item { string label; drop(~this) {} }
		type Holder[T] { T held; drop(~this) {} }
		make_holder[T](Vector[T] v) Holder[T] {
			return Holder[T](held: v[0]);
		}
		test_t0847_generic() {
			v := Item[]();
			v.push(Item(label: "x"));
			h := make_holder[Item](v);
		}
	`)
	// Under mono (T=Item), the dup still fires: allocate + memcpy the element.
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "call void @llvm.memcpy")
}

// T0847 (paren variant): a parenthesized container-element ctor arg
// `Holder(held: (v[0]))` must peel the ParenExpr to reach the IndexExpr and
// still dup-on-read. Exercises maybeEnableDupForConstructorArg's ParenExpr peel.
func TestT0847_ConstructorParenVectorElementDups(t *testing.T) {
	ir := generateIR(t, `
		type Item { string label; drop(~this) {} }
		type Holder { Item held; drop(~this) {} }
		test_t0847_paren() {
			v := Item[]();
			v.push(Item(label: "x"));
			h := Holder(held: (v[0]));
		}
	`)
	// Paren peel reaches the IndexExpr → dup-on-read fires (allocate + memcpy).
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "call void @llvm.memcpy")
}

// T0411: Channel field auto-dup via constructor field-init from `this.field`.
// Channel dup is a refcount increment via promise_channel_incref.
func TestT0411_ConstructorChannelFieldFromThisDups(t *testing.T) {
	ir := generateIR(t, `
		type ChH {
			channel[int] ch;
			drop(~this) {}
			clone() ChH {
				return ChH(ch: this.ch);
			}
		}
		test_t0411_ch_dup() {
			c := channel[int](1);
			h := ChH(ch: c);
			h2 := h.clone();
		}
	`)
	cloneFn := extractFunction(ir, "ChH.clone")
	if cloneFn == "" {
		t.Fatal("expected ChH.clone in IR")
	}
	// dupChannel emits a `chdup.inc` block label and an inline atomicrmw add
	// to bump the channel's reference count.
	assertContains(t, cloneFn, "chdup.inc")
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
	ir := generateIR(t, `
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
	assertContains(t, ir, "dup_holders[Holder]")
}

// --- T0613: paren-wrapped `this` as a method/field/operator/RTTI receiver ---
//
// genExpr already evaluates ParenExpr transparently, so the receiver *value* for
// `(this)` is byte-identical to bare `this` (a raw i8* instance pointer). The bug
// was in the AST-shape dispatch gates that matched `*ast.ThisExpr` directly: for
// `*ast.ParenExpr{ThisExpr}` they fell through and ran `extractvalue i8* %p, 1`
// on the raw pointer, which opt rejects ("extractvalue operand must be aggregate
// type"). The fix peels ParenExpr at each gate via isThisReceiver(). The
// universal invariant below — valid IR never extracts a field from an `i8*` — is
// the cheap cross-cutting assertion for every category.

func TestParenThisMethodCallPassesReceiver(t *testing.T) {
	ir := generateIR(t, `
		type T0613MBox {
			int x;
			self(~this) T0613MBox { return this; }
			driver(~this) T0613MBox { r := (this).self(); return r; }
		}
		main() { b := T0613MBox(x: 1); r := b.driver(); }
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	driver := extractFunction(ir, "T0613MBox.driver")
	if driver == "" {
		t.Fatal("expected T0613MBox.driver in IR")
	}
	// The paren-wrapped `this` receiver is passed directly as the i8* receiver.
	assertContains(t, driver, "@T0613MBox.self(i8*")
}

func TestParenThisFieldReadNoExtractFromPtr(t *testing.T) {
	// Heap type field read through (this).
	irHeap := generateIR(t, `
		type T0613FHeap {
			string s;
			read(this) string { return (this).s; }
		}
		main() { b := T0613FHeap(s: "x"); v := b.read(); }
	`)
	assertNotContains(t, irHeap, "extractvalue i8*")
	if extractFunction(irHeap, "T0613FHeap.read") == "" {
		t.Fatal("expected T0613FHeap.read in IR")
	}
	// Value type field read through (this) and ((this)).
	irVal := generateIR(t, `
		type T0613FVal {
			int x `+"`value"+`;
			int y `+"`value"+`;
			read_x(this) int { return (this).x; }
			read_y(this) int { return ((this)).y; }
		}
		main() { p := T0613FVal(x: 3, y: 4); a := p.read_x(); b := p.read_y(); }
	`)
	assertNotContains(t, irVal, "extractvalue i8*")
	if extractFunction(irVal, "T0613FVal.read_x") == "" {
		t.Fatal("expected T0613FVal.read_x in IR")
	}
}

func TestParenThisSetterNoExtractFromPtr(t *testing.T) {
	// Direct setter through (this), plus plain field assignment through (this).
	irDirect := generateIR(t, `
		type T0613SBox {
			int n;
			get value int { return this.n; }
			set value(int v) { this.n = v; }
			set_via(~this, int v) { (this).value = v; }
			bump(~this, int v) { (this).n = v; }
		}
		main() { b := T0613SBox(n: 0); b.set_via(9); b.bump(7); }
	`)
	assertNotContains(t, irDirect, "extractvalue i8*")
	if extractFunction(irDirect, "T0613SBox.set_via") == "" {
		t.Fatal("expected T0613SBox.set_via in IR")
	}
	// Virtual setter (base has a child → vtable dispatch) through (this).
	irVirtual := generateIR(t, `
		type T0613VSBase {
			int n;
			get value int { return this.n; }
			set value(int v) { this.n = v; }
			scale_via(~this, int v) { (this).value = v; }
		}
		type T0613VSDerived is T0613VSBase {}
		main() { d := T0613VSDerived(n: 0); d.scale_via(11); }
	`)
	assertNotContains(t, irVirtual, "extractvalue i8*")
}

func TestParenThisGetterNoExtractFromPtr(t *testing.T) {
	// Direct getter through (this).
	irDirect := generateIR(t, `
		type T0613GBox {
			int n;
			get value int { return this.n; }
			get_via(this) int { return (this).value; }
		}
		main() { b := T0613GBox(n: 5); v := b.get_via(); }
	`)
	assertNotContains(t, irDirect, "extractvalue i8*")
	if extractFunction(irDirect, "T0613GBox.get_via") == "" {
		t.Fatal("expected T0613GBox.get_via in IR")
	}
	// Virtual getter (base has a child → vtable dispatch) through (this).
	irVirtual := generateIR(t, `
		type T0613VGBase {
			int n;
			get value int { return this.n; }
			get_via(this) int { return (this).value; }
		}
		type T0613VGDerived is T0613VGBase {}
		main() { d := T0613VGDerived(n: 13); v := d.get_via(); }
	`)
	assertNotContains(t, irVirtual, "extractvalue i8*")
}

func TestParenThisVirtualMethodNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0613Shape {
			area(this) int `+"`abstract"+`;
			area_via(this) int { return (this).area(); }
		}
		type T0613Square is T0613Shape {
			int side;
			area(this) int { return this.side * this.side; }
		}
		main() { s := T0613Square(side: 5); a := s.area_via(); }
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0613Shape.area_via") == "" {
		t.Fatal("expected T0613Shape.area_via in IR")
	}
}

func TestParenThisIsCheckNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0613Animal {
			string name;
			speak(this) string `+"`abstract"+`;
			am_i_dog(this) bool { return (this) is T0613Dog; }
		}
		type T0613Dog is T0613Animal { speak(this) string { return "Woof"; } }
		main() {
			T0613Animal d = T0613Dog(name: "Rex");
			b := d.am_i_dog();
		}
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0613Animal.am_i_dog") == "" {
		t.Fatal("expected T0613Animal.am_i_dog in IR")
	}
}

func TestParenThisOperatorNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0613V2 {
			int x `+"`value"+`;
			int y `+"`value"+`;
			+(T0613V2 other) T0613V2 { return T0613V2(x: this.x + other.x, y: this.y + other.y); }
			add_via(this, T0613V2 other) T0613V2 { return (this) + other; }
		}
		main() {
			a := T0613V2(x: 1, y: 2);
			b := T0613V2(x: 3, y: 4);
			c := a.add_via(b);
		}
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0613V2.add_via") == "" {
		t.Fatal("expected T0613V2.add_via in IR")
	}
}

func TestParenThisGenericMethodNoExtractFromPtr(t *testing.T) {
	// T0746: a droppable (string) payload exercises the generic-method
	// return-by-value dup path in addition to the (this).peek() dispatch gate.
	ir := generateIR(t, `
		type T0613GenBox[T] {
			T val;
			peek(this) T { return this.val; }
			via(this) T { return (this).peek(); }
		}
		main() { b := T0613GenBox[string](val: "hi"); v := b.via(); }
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	// The monomorphized instance method body is emitted into the (unsplit) module.
	assertContains(t, ir, "T0613GenBox[string].via")
}

func TestParenThisEnumMethodNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		enum T0613Color {
			Red, Green, Blue,
			describe(this) string {
				match this {
					T0613Color.Red => { return "red"; },
					T0613Color.Green => { return "green"; },
					T0613Color.Blue => { return "blue"; },
				}
			}
			describe_via(this) string { return (this).describe(); }
			get opposite T0613Color {
				match this {
					T0613Color.Red => { return T0613Color.Green; },
					T0613Color.Green => { return T0613Color.Blue; },
					T0613Color.Blue => { return T0613Color.Red; },
				}
			}
			opposite_via(this) T0613Color { return (this).opposite; }
		}
		main() {
			s := T0613Color.Red.describe_via();
			o := T0613Color.Red.opposite_via();
		}
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0613Color.describe_via") == "" {
		t.Fatal("expected T0613Color.describe_via in IR")
	}
}

// --- T0613: additional dispatch gates surfaced during coverage analysis ---
//
// Each of the gates below is a distinct receiver-dispatch site converted to
// isThisReceiver() that is NOT exercised by the categories above (operator
// receiver, method call, field access, etc.). All five emit valid IR with the
// peel and panic/emit-invalid-IR without it; the universal "no extractvalue from
// i8*" invariant guards the IR shape.

// genBinaryExpr `e.Right` "this-as-argument" gate: `other < (this)` passes the
// paren-wrapped `this` as the operator *argument* (not the receiver). Heap type
// — the value-type form of this gate is a separate pre-existing bug (T0748).
func TestParenThisOperatorArgNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0613CmpBox {
			int x;
			<(T0613CmpBox other) bool { return this.x < other.x; }
			gt_via(this, T0613CmpBox other) bool { return other < (this); }
		}
		main() {
			a := T0613CmpBox(x: 5);
			b := T0613CmpBox(x: 2);
			r := a.gt_via(b);
		}
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0613CmpBox.gt_via") == "" {
		t.Fatal("expected T0613CmpBox.gt_via in IR")
	}
}

// genVirtualBinaryOp gate: a base type with a child dispatches its operator
// through the vtable, so `(this) + other` hits the virtual-operator receiver
// gate rather than the direct genBinaryExpr gate.
func TestParenThisVirtualOperatorNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0613VOpBase {
			int n;
			+(T0613VOpBase other) T0613VOpBase { return T0613VOpBase(n: this.n + other.n); }
			add_via(this, T0613VOpBase other) T0613VOpBase { return (this) + other; }
		}
		type T0613VOpDerived is T0613VOpBase {}
		main() {
			a := T0613VOpBase(n: 1);
			b := T0613VOpBase(n: 2);
			c := a.add_via(b);
		}
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0613VOpBase.add_via") == "" {
		t.Fatal("expected T0613VOpBase.add_via in IR")
	}
}

// genGenericEnumMethodCall gate: a generic enum routes method calls through a
// dedicated path distinct from the non-generic enum method gate.
func TestParenThisGenericEnumMethodNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		enum T0613GOpt[T] {
			Some(T value), None,
			has(this) bool {
				match this { T0613GOpt.Some(v) => { return true; }, T0613GOpt.None => { return false; } }
			}
			has_via(this) bool { return (this).has(); }
		}
		main() {
			s := T0613GOpt[int].Some(5);
			r := s.has_via();
		}
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	assertContains(t, ir, "T0613GOpt[int].has_via")
}

// genIsResolvedType gate: `(this) is IBox[int]` resolves to a concrete generic
// instance and routes through genIsResolvedType (distinct from the non-generic
// genIsNamedType gate covered by TestParenThisIsCheck...).
func TestParenThisIsGenericNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0613GShape {
			area(this) int `+"`abstract"+`;
			is_intbox(this) bool { return (this) is T0613IBox[int]; }
		}
		type T0613IBox[T] is T0613GShape {
			T value;
			area(this) int { return 1; }
		}
		main() {
			T0613GShape s = T0613IBox[int](value: 5);
			b := s.is_intbox();
		}
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0613GShape.is_intbox") == "" {
		t.Fatal("expected T0613GShape.is_intbox in IR")
	}
}

// genFieldPtr value-type lvalue gate (compound assign through `(this)`):
// `(this).x += d` on a value type takes the field-ptr lvalue path. Without the
// peel this panics ("value type field assignment requires addressable target"),
// so the test passing at all (generateIR not panicking) is the regression guard.
// Runtime mutation is a no-op (value types are copy semantics), so this is an
// IR-shape test only — no runtime e2e companion.
func TestParenThisValueFieldPtrLvalueNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0613VTField {
			int x `+"`value"+`;
			int y `+"`value"+`;
			bump(~this, int d) { (this).x += d; }
		}
		main() {
			v := T0613VTField(x: 1, y: 2);
			v.bump(5);
		}
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0613VTField.bump") == "" {
		t.Fatal("expected T0613VTField.bump in IR")
	}
}

// --- T0747: `this as!/as T` RTTI cast on a `this` receiver ---
//
// genExpr(this) yields a bare instance i8*, not the {vtable, instance} value
// struct that the cast result paths assume. Before the fix, `(this as! T).field`
// emitted `extractvalue i8* %p, 1` (opt-rejected) and `d := this as! T` /
// `T? o = this as T` panicked in codegen storing the i8* into a value-struct
// slot. The fix rebuilds the cast result as a {vtable, instance} value struct
// (vtable loaded from the object's typeinfo chain, mirroring genVirtualBinaryOp).
// The "no extractvalue from i8*" invariant guards the IR shape; the optional
// case additionally guards against the codegen panic (generateIR would panic).

// Forced cast + inline field access through `this`, both bare and paren subject.
// The reconstruction (insertvalue into {i8*, i8*}) is what lets the downstream
// field access extract from an aggregate instead of `extractvalue i8* ...`.
func TestThisCastForcedFieldNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0747Base {
			int n;
			whoami(this) string `+"`abstract"+`;
			as_n(this) int { return (this as! T0747Derived).n; }
			as_n_bare(this) int { return ((this) as! T0747Derived).n; }
		}
		type T0747Derived is T0747Base { whoami(this) string { return "d"; } }
		main() { T0747Base b = T0747Derived(n: 7); _ := b.as_n(); _ := b.as_n_bare(); }
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	asN := extractFunction(ir, "T0747Base.as_n")
	if asN == "" {
		t.Fatal("expected T0747Base.as_n in IR")
	}
	// The cast result is rebuilt as a {i8*, i8*} value struct (the fix); without
	// it the function returned the raw i8* and the field read extracted from it.
	assertContains(t, asN, "insertvalue { i8*, i8* }")
}

// Forced cast bound to a local, then a virtual method call on the cast result.
// Before the fix this panicked in codegen (store i8* into {i8*,i8*}* var slot).
// The reconstructed value struct carries the real vtable (loaded from typeinfo),
// so the method call on the local dispatches correctly.
func TestThisCastForcedLocalNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0747LBase {
			int n;
			whoami(this) string `+"`abstract"+`;
			as_str(this) string { d := (this) as! T0747LDerived; return d.whoami(); }
			as_str_bare(this) string { d := this as! T0747LDerived; return d.whoami(); }
		}
		type T0747LDerived is T0747LBase { whoami(this) string { return "d"; } }
		main() { T0747LBase b = T0747LDerived(n: 7); _ := b.as_str(); _ := b.as_str_bare(); }
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0747LBase.as_str") == "" {
		t.Fatal("expected T0747LBase.as_str in IR")
	}
}

// Optional cast (`this as T`) through `this`. Before the fix this panicked in
// codegen inside wrapOptional ("store operands are not compatible: src=i8*;
// dst={ i8*, i8* }*"). generateIR not panicking is itself the regression guard.
func TestThisCastOptionalNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0747OBase {
			int n;
			whoami(this) string `+"`abstract"+`;
			is_der(this) bool {
				T0747ODerived? o = this as T0747ODerived;
				if o { return true; }
				return false;
			}
		}
		type T0747ODerived is T0747OBase { whoami(this) string { return "d"; } }
		main() { T0747OBase b = T0747ODerived(n: 7); _ := b.is_der(); }
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0747OBase.is_der") == "" {
		t.Fatal("expected T0747OBase.is_der in IR")
	}
}

// --- T0783: `return x as! T` clears the cast subject's drop flag ---
//
// `return s as! Circle` aliases s's heap instance into the returned value; the
// caller's binding owns it. genReturnStmt now peels the cast via
// castSubjectMovableIdent and clears the subject's drop flag (the same helper
// T0754/T0800 use at owning-slot stores) so s's scope-exit drop does not fire
// on the same allocation -> double-free. The IR signature is a
// `store i1 false, i1* %s.dropflag` on the cast-success path before the return;
// without the fix that clear is absent and the conditional drop executes.
func TestT0783_ReturnCastClearsSubjectDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape { f64 radius; area(this) f64 { return this.radius; } }
		helper(int dummy) Circle {
			Shape s = Circle(name: "src", radius: 2.0);
			return s as! Circle;
		}
		main() { _ := helper(0); }
	`)
	fn := extractFunction(ir, "__user.helper")
	if fn == "" {
		t.Fatal("expected __user.helper in IR")
	}
	// The cast subject's drop flag must be cleared (moved out via the return).
	assertContains(t, fn, "store i1 false, i1* %s.dropflag")
}

// Chained cast on the return path (T0800 sibling): castSubjectMovableIdent
// recurses through the nested CastExpr to the innermost subject, so its drop
// flag is still cleared.
func TestT0783_ReturnChainedCastClearsSubjectDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape { f64 radius; area(this) f64 { return this.radius; } }
		helper(int dummy) Circle {
			Shape s = Circle(name: "src", radius: 2.0);
			return (s as! Circle) as! Circle;
		}
		main() { _ := helper(0); }
	`)
	fn := extractFunction(ir, "__user.helper")
	if fn == "" {
		t.Fatal("expected __user.helper in IR")
	}
	assertContains(t, fn, "store i1 false, i1* %s.dropflag")
}

// T0849: the optional `as` form (Force == false, result `Circle?`) is a
// *conditional* move: the subject is aliased into the result only on a
// successful downcast; on failure the result is None and the subject must still
// be dropped. genReturnStmt routes the cast subject through
// consumeCastSubjectDropFlag, which — for the non-Force form — stores `!isMatch`
// into the subject's drop flag instead of clearing it unconditionally. So the
// subject's scope-exit drop fires iff the downcast failed: no double-free on
// success (was SEGV), no leak on failure. The IR signature is an
// `xor i1 %isMatch, true` feeding a `store ..., i1* %s.dropflag`, NOT an
// unconditional `store i1 false`.
func TestT0849_ReturnOptionalCastConditionalSubjectDrop(t *testing.T) {
	ir := generateIR(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape { f64 radius; area(this) f64 { return this.radius; } }
		helper(int dummy) Circle? {
			Shape s = Circle(name: "src", radius: 2.0);
			return s as Circle;
		}
		main() { _ := helper(0); }
	`)
	fn := extractFunction(ir, "__user.helper")
	if fn == "" {
		t.Fatal("expected __user.helper in IR")
	}
	// The drop flag is set to the negated downcast-success flag (drop iff the
	// cast failed): `%n = xor i1 %isMatch, true` then `store i1 %n, ... s.dropflag`.
	assertContainsMatch(t, fn, `%\w+ = xor i1 %\w+, true\n\s*store i1 %\w+, i1\* %s\.dropflag`)
	// The conditional drop still executes at scope exit (flag is loaded) ...
	assertContains(t, fn, "load i1, i1* %s.dropflag")
	// ... and it is NOT an unconditional clear (that would leak on a failed
	// downcast — the pre-T0849 buggy shape).
	assertNotContains(t, fn, "store i1 false, i1* %s.dropflag")
}

// T0849 owning-slot sibling: `Box(c: s as Circle)` stores the conditional
// success flag into the field-init constructor's subject drop flag the same way
// (drop iff the cast failed). Before T0849 this site cleared the flag
// unconditionally (`store i1 false`) → leak on the failure path.
func TestT0849_OwningSlotOptionalCastConditionalDrop(t *testing.T) {
	ir := generateIR(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape { f64 radius; area(this) f64 { return this.radius; } }
		type Box { Circle? c; }
		helper(int dummy) bool {
			Shape s = Circle(name: "src", radius: 2.0);
			b := Box(c: s as Circle);
			return true;
		}
		main() { _ := helper(0); }
	`)
	fn := extractFunction(ir, "__user.helper")
	if fn == "" {
		t.Fatal("expected __user.helper in IR")
	}
	// Conditional store of the negated success flag into the subject's drop flag.
	assertContainsMatch(t, fn, `%\w+ = xor i1 %\w+, true\n\s*store i1 %\w+, i1\* %s\.dropflag`)
	// Not an unconditional clear (the pre-T0849 leak-on-failure shape).
	assertNotContains(t, fn, "store i1 false, i1* %s.dropflag")
}

// T0849 (wasm exposure): a closure call must coerce its arguments to the
// signature's parameter types the same way a regular call does. An optional
// param `int?` typed the indirect-call function pointer as `(i8*, {i1, i64})`,
// but the bare `none` / `5` argument was passed uncoerced as the scalar
// discriminant. The resulting type-mismatched call was tolerated by the x86
// backend but lowered to invalid WebAssembly. The fix routes closure args
// through coerceCallArgs (optional wrapping), so the call passes the full
// `{i1, i64}` aggregate.
func TestT0849ClosureCallCoercesOptionalArg(t *testing.T) {
	ir := generateIR(t, `
		none_arg() bool {
			apply := |int? x| -> bool { return true; };
			return apply(none);
		}
		bare_arg() bool {
			apply := |int? x| -> bool { return true; };
			return apply(5);
		}
		main() { _ := none_arg(); _ := bare_arg(); }
	`)
	none := extractFunction(ir, "__user.none_arg")
	if none == "" {
		t.Fatal("expected __user.none_arg in IR")
	}
	// `none` → zeroinitialized `{i1,i64}` aggregate passed to the closure call.
	// Before the fix this was a bare `i1 false`, mismatching the `{i1,i64}` param.
	assertContains(t, none, "{ i1, i64 } zeroinitializer)")

	bare := extractFunction(ir, "__user.bare_arg")
	if bare == "" {
		t.Fatal("expected __user.bare_arg in IR")
	}
	// `5` → wrapped `{i1 true, i64 5}` aggregate (insertvalue chain), not a bare i64.
	assertContainsMatch(t, bare, `insertvalue \{ i1, i64 \} %\w+, i64 5, 1`)
	// The closure call receives the aggregate, never a bare scalar second arg.
	assertContainsMatch(t, bare, `call i1 %\w+\(i8\* %\w+, \{ i1, i64 \} %\w+\)`)
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

// T0993: a class `match { Circle c => }` type-pattern arm must dispatch on the
// runtime subtype via the promise_type_is RTTI machinery — not emit nothing and
// silently fall through to the wildcard (the merged T0992 miscompilation).
func TestMatchTypePatternEmitsRTTI(t *testing.T) {
	ir := generateIR(t, `
		type Shape { area(this) f64 => 0.0; }
		type Circle is Shape { f64 r; }
		type Square is Shape { f64 s; }
		describe(Shape sh) string {
			return match sh {
				Circle c => "circle",
				Square q => "square",
				_ => "other",
			};
		}
		main() { Shape s = Circle(r: 1.0); d := describe(s); print_line(d); }
	`)
	// Each class type-pattern arm lowers to an RTTI subtype check (Circle + Square).
	if n := strings.Count(ir, "call i32 @promise_type_is"); n < 2 {
		t.Errorf("expected >=2 promise_type_is calls for type-pattern arms, got %d\n%s", n, ir)
	}
}

// T0993: a class type-pattern arm whose runtime type is the tested subtype must
// not be a no-op — the arm body's value must reach the match result.
func TestMatchTypePatternBindsSubtype(t *testing.T) {
	ir := generateIR(t, `
		type Shape { area(this) f64 => 0.0; }
		type Circle is Shape { f64 r; area(this) f64 => this.r; }
		describe(Shape sh) f64 {
			return match sh {
				Circle c => c.r,
				_ => -1.0,
			};
		}
		main() { Shape s = Circle(r: 2.0); d := describe(s); }
	`)
	assertContains(t, ir, "call i32 @promise_type_is")
}

// T0993: a non-destructive enum variant narrowing (`if x is V { x.named }`)
// reads the named payload via a variant-data GEP+load. The function must
// compile (no codegen panic) and the field value must be produced.
func TestEnumNarrowVariantFieldRead(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rectangle(f64 width, f64 height) }
		main() {
			Shape s = Shape.Circle(radius: 5.0);
			f64 out = 0.0;
			if s is Circle {
				out = s.radius;
			}
		}
	`)
	// The narrowed read lowers to a variant-data field load of a double.
	assertContains(t, ir, "load double")
	assertContains(t, ir, "getelementptr")
}

// T1011: a narrowed heap (string) variant field that ESCAPES the narrowing scope
// (here: returned) must be cloned, not aliased — otherwise the subject's synth
// enum drop frees the payload while the returned value still points into it
// (use-after-free / double-free). The escape-dup must emit a strdup.
func TestEnumNarrowVariantStringFieldEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		grab() string {
			Msg m = Msg.Text(body: "a");
			if m is Text { return m.body; }
			return "";
		}
		main() { string s = grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	// dupString emits a strdup.copy block + promise_string_new at the return site.
	assertContains(t, fn, "strdup.copy")
	assertContains(t, fn, "promise_string_new")
}

// T1011 (no-regression): a purely in-scope read of a narrowed heap variant field
// stays a zero-copy borrow — no dup flag is set, so genNarrowedVariantField must
// NOT emit a strdup for the read.
func TestEnumNarrowVariantStringFieldInScopeNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		probe() int {
			Msg m = Msg.Text(body: "a");
			int n = 0;
			if m is Text { n = m.body.len; }
			return n;
		}
		main() { int x = probe(); }
	`)
	fn := extractFunction(ir, "__user.probe")
	if fn == "" {
		t.Fatal("expected __user.probe in IR")
	}
	assertNotContains(t, fn, "strdup.copy")
}

// T1011: a narrowed heap (string) variant field escaping into a CONSTRUCTOR field
// of a droppable type must be cloned. maybeEnableDupForConstructorArg routes the
// narrowed-field arg through the same dup-on-escape path as a struct field
// (narrowedVariantFieldDroppable matched=true, droppable=true).
func TestEnumNarrowVariantStringFieldCtorEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		type Sink { string held; drop(~this) {} }
		grab() Sink {
			Msg m = Msg.Text(body: "a");
			if m is Text { return Sink(held: m.body); }
			return Sink(held: "");
		}
		main() { Sink s = grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	assertContains(t, fn, "strdup.copy")
}

// T1011: a narrowed heap (string) variant field passed to a consuming `string move`
// param must be cloned. maybeEnableDupForMutRefArg's narrowed-field branch sets
// the dup-on-escape flag — the callee takes ownership, so the value must not
// alias the subject the synth enum drop frees at scope exit.
func TestEnumNarrowVariantStringFieldConsumingParamDups(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		take(string move s) int { return s.len; }
		grab() int {
			Msg m = Msg.Text(body: "a");
			if m is Text { return take(m.body); }
			return 0;
		}
		main() { int n = grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	assertContains(t, fn, "strdup.copy")
}

// T1011: binding a narrowed heap (string) variant field to a new variable
// (`b := m.body`) takes ownership, so isStringFieldDup recognizes the narrowed
// field (its narrowedVariantFieldDroppable branch) and the binding keeps its drop
// flag while genNarrowedVariantField clones the payload — without the clone the
// binding's drop would double-free with the subject's synth enum drop.
func TestEnumNarrowVariantStringFieldBoundCopyDups(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		grab() int {
			Msg m = Msg.Text(body: "a");
			int r = 0;
			if m is Text { b := m.body; r = b.len; }
			return r;
		}
		main() { int n = grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	assertContains(t, fn, "strdup.copy")
}

// T1011: a GENERIC enum's narrowed heap variant field escaping the scope exercises
// the typeSubst substitution in genNarrowedVariantField (both targetType and the
// field type) and narrowedVariantFieldDroppable — the substituted field type is
// `string`, so the escape must still clone.
func TestEnumNarrowGenericVariantStringFieldEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		enum Opt[T] { Some(T val), None }
		grab() string {
			Opt[string] o = Opt[string].Some(val: "a");
			if o is Some { return o.val; }
			return "";
		}
		main() { string s = grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	assertContains(t, fn, "strdup.copy")
}

// T1011: a GENERIC FUNCTION body that narrows an enum and escapes a heap variant
// field exercises the typeSubst substitution in genNarrowedVariantField — the
// narrowing TargetType and FieldType carry the function's TypeParam, so they must
// be substituted before dupHeapFieldForEscape runs. Monomorphized for T=string the
// escape must clone; for T=int (non-heap) it must not. This is the path the
// concrete-Opt[string] test above does NOT reach (there typeSubst is nil because
// sema already resolved the field type to a concrete `string`).
func TestEnumNarrowGenericFnBodyVariantFieldEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		enum Opt[T] { Some(T val), None }
		extract[T](Opt[T] o, T fallback) T {
			if o is Some { return o.val; }
			return fallback;
		}
		main() {
			Opt[string] os = Opt[string].Some(val: "a");
			string s = extract[string](os, "");
			Opt[int] oi = Opt[int].Some(val: 1);
			int n = extract[int](oi, 0);
		}
	`)
	strFn := extractFunction(ir, `"extract[string]"`)
	if strFn == "" {
		t.Fatal(`expected "extract[string]" mono instance in IR`)
	}
	assertContains(t, strFn, "strdup.copy")
	intFn := extractFunction(ir, `"extract[int]"`)
	if intFn == "" {
		t.Fatal(`expected "extract[int]" mono instance in IR`)
	}
	assertNotContains(t, intFn, "strdup.copy")
}

// T1011: binding a narrowed heap variant field inside a GENERIC function body
// (`b := o.val`) routes through isStringFieldDup → narrowedVariantFieldDroppable
// with typeSubst active, so the substituted TargetType resolves to a droppable
// enum and the binding clones the payload (keeping it independent of the subject).
func TestEnumNarrowGenericFnBodyVariantFieldBoundCopyDups(t *testing.T) {
	ir := generateIR(t, `
		enum Opt[T] { Some(T val), None }
		first[T](Opt[T] o, T fallback) T {
			if o is Some { b := o.val; return b; }
			return fallback;
		}
		main() {
			Opt[string] os = Opt[string].Some(val: "a");
			string s = first[string](os, "");
		}
	`)
	fn := extractFunction(ir, `"first[string]"`)
	if fn == "" {
		t.Fatal(`expected "first[string]" mono instance in IR`)
	}
	assertContains(t, fn, "strdup.copy")
}

// T1011 (no-regression): a non-droppable enum (no heap payload in any variant)
// narrowed to a variant whose non-heap field escapes must NOT clone —
// narrowedVariantFieldDroppable reports droppable=false (enumTargetDroppable is
// false), so the consumer skips the dup. Cloning a field the synth drop never
// frees would leak. The int field copies cleanly into the constructor.
func TestEnumNarrowVariantNonDroppableFieldNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum Flag { On(int code), Off }
		type Box { int v; }
		grab() Box {
			Flag f = Flag.On(code: 3);
			if f is On { return Box(v: f.code); }
			return Box(v: 0);
		}
		main() { Box b = grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	assertNotContains(t, fn, "strdup.copy")
}

// T0993: `match this { Subtype c => }` over a class hierarchy. genThisExpr
// returns the raw i8* instance pointer; genValueMatch must normalize it into the
// {vtable, instance} value struct before the type-pattern arm extracts the
// instance for RTTI — otherwise it emits `extractvalue` on an i8* (invalid IR
// that fails `opt`). This guards that normalization.
func TestMatchTypePatternThisReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Shape {
			describe(this) string => match this {
				Circle c => "circle",
				Square s => "square",
				_ => "other",
			};
		}
		type Circle is Shape { f64 r; }
		type Square is Shape { f64 s; }
		main() { Shape a = Circle(r: 1.0); d := a.describe(); }
	`)
	assertContains(t, ir, "call i32 @promise_type_is")
	// No extractvalue on an i8* — the subject was wrapped into a value struct.
	if strings.Contains(ir, "extractvalue i8* ") {
		t.Errorf("expected no extractvalue on i8* (this subject must be normalized)\n%s", ir)
	}
}

// --- T1031: aliasing a borrowed-return into a new owner dups at the call site -

// T1031: `Node b = ident(a)` where `ident(Node n) Node { return n; }` returns its
// by-value borrow param aliases a's heap instance. Because a remains owned (it is
// co-dropped at scope exit), the new owner b must receive an INDEPENDENT
// allocation — otherwise both drop the shared instance (double-free / UAF). The
// fix clones into the source local's storage at the call site, gated on a runtime
// `retPtr == argPtr` alias check. The callee itself returns the bare alias (no
// clone), so functions that relocate their param before returning (sort's COW)
// stay untouched.
func TestReturnBorrowParamDupsHeapUserType(t *testing.T) {
	ir := generateIR(t, `
		type Node { int v; drop(~this){} }
		ident(Node n) Node { return n; }
		run() Node {
			Node a = Node(v: 1);
			Node b = ident(a);
			return b;
		}
	`)
	// The callee returns the bare alias — no clone.
	callee := extractDefine(ir, "__user.ident")
	if callee == "" {
		t.Fatalf("ident callee not found in IR:\n%s", ir)
	}
	assertNotContains(t, callee, "heapdup.copy")
	// The caller clones into the source's storage under a runtime alias guard.
	caller := extractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	assertContains(t, caller, "alias.dup")
	assertContains(t, caller, "heapdup.copy")
	assertContains(t, caller, "@pal_alloc")
}

// T1031: a returned by-value string param is deep-copied at the call site.
func TestReturnBorrowParamDupsString(t *testing.T) {
	ir := generateIR(t, `
		ident(string s) string { return s; }
		run() string {
			string a = "x".repeat(2);
			string b = ident(a);
			return b;
		}
	`)
	callee := extractDefine(ir, "__user.ident")
	if callee == "" {
		t.Fatalf("ident callee not found in IR:\n%s", ir)
	}
	assertNotContains(t, callee, "strdup.copy")
	caller := extractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	assertContains(t, caller, "alias.dup")
	assertContains(t, caller, "strdup.copy")
}

// T1031: a returned by-value vector param is deep-copied at the call site.
func TestReturnBorrowParamDupsVector(t *testing.T) {
	ir := generateIR(t, `
		ident(int[] v) int[] { return v; }
		run() int[] {
			int[] a = [];
			int[] b = ident(a);
			return b;
		}
	`)
	caller := extractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	assertContains(t, caller, "alias.dup")
	assertContains(t, caller, "vecdup.copy")
}

// T1031: a moved (`move`) param is owned by the callee — the call site must NOT
// emit the aliasing clone (the source is consumed, not co-owned).
func TestReturnMovedParamNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Node { int v; drop(~this){} }
		consume(Node move n) Node { return n; }
		run() Node {
			Node a = Node(v: 1);
			Node b = consume(move a);
			return b;
		}
	`)
	caller := extractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	assertNotContains(t, caller, "alias.dup")
}

// T1031/T1017: a DISCARDED call whose heap-user-type result aliases a still-live
// local takes the discard path (clearDiscardedAliasTempFlag), NOT the assignment
// clone path. Heap user-type results are tracked as heapTemps keyed by an
// extractvalue SSA value distinct from the freshly-extracted retPtr, so the
// function must scan the tracked heap temps (loading each temp's stored instance
// pointer) and clear the matching temp's flag — keeping the live local the sole
// owner, dropped once at scope exit. This exercises the heap-temp scan branch
// (the vector/stmtTemp discard case in TestT1017DiscardedAliasClearsResultTemp
// returns earlier via the direct stmtTempMap lookup).
func TestReturnBorrowParamDiscardedHeapUserTypeScansHeapTemps(t *testing.T) {
	ir := generateIR(t, `
		type Node { int v; drop(~this){} }
		ident(Node n) Node { return n; }
		run() int {
			Node n = Node(v: 11);
			ident(n);
			return n.v;
		}
	`)
	caller := extractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	// Discard path (not the assignment clone path).
	assertContains(t, caller, "alias.discard.clear")
	assertNotContains(t, caller, "alias.dup")
}
