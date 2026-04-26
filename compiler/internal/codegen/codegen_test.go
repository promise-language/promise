package codegen

import (
	"bytes"
	"strings"
	"testing"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/sema"
	antlr "github.com/antlr4-go/antlr/v4"
)

// generateIR runs the full pipeline: parse → sema → codegen, returns LLVM IR text.
func generateIR(t *testing.T, src string) string {
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
	info, errs := sema.Check(file)
	if len(errs) > 0 {
		t.Fatalf("sema errors: %v", errs)
	}
	result := Compile(file, info)
	return result.Module.String()
}

// compileResult runs the full pipeline and returns the CompileResult.
func compileResult(t *testing.T, src string) *CompileResult {
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
	info, errs := sema.Check(file)
	if len(errs) > 0 {
		t.Fatalf("sema errors: %v", errs)
	}
	return Compile(file, info)
}

func assertContains(t *testing.T, ir, substr string) {
	t.Helper()
	if !strings.Contains(ir, substr) {
		t.Errorf("expected IR to contain %q\ngot:\n%s", substr, ir)
	}
}

func assertNotContains(t *testing.T, ir, substr string) {
	t.Helper()
	if strings.Contains(ir, substr) {
		t.Errorf("expected IR to NOT contain %q\ngot:\n%s", substr, ir)
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

func TestPrintInt(t *testing.T) {
	ir := generateIR(t, `
		print(int x) `+"`"+`extern("promise_print_int");
		main() { print(42); }
	`)
	// Struct type definition
	assertContains(t, ir, "%promise_int_v = type")
	// Extern declaration uses struct type
	assertContains(t, ir, "declare void @promise_print_int(%promise_int_v")
	// Struct packing via insertvalue
	assertContains(t, ir, "insertvalue %promise_int_v")
}

func TestPrintBool(t *testing.T) {
	ir := generateIR(t, `
		print(bool x) `+"`"+`extern("promise_print_bool");
		main() { print(true); }
	`)
	assertContains(t, ir, "%promise_bool_v = type")
	assertContains(t, ir, "declare void @promise_print_bool(%promise_bool_v")
	// Bool coercion: i1 → i8
	assertContains(t, ir, "zext i1 true to i8")
	assertContains(t, ir, "insertvalue %promise_bool_v")
}

func TestPrintF64(t *testing.T) {
	ir := generateIR(t, `
		print(f64 x) `+"`"+`extern("promise_print_f64");
		main() { print(3.14); }
	`)
	assertContains(t, ir, "%promise_f64_v = type")
	assertContains(t, ir, "declare void @promise_print_f64(%promise_f64_v")
	assertContains(t, ir, "insertvalue %promise_f64_v")
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
	assertContains(t, ir, "declare void @my_log_int(%promise_int_v")
	assertContains(t, ir, "call void @my_log_int(%promise_int_v")
}

func TestExternDefaultCName(t *testing.T) {
	ir := generateIR(t, `
		do_thing(int x) `+"`"+`extern;
		main() { do_thing(1); }
	`)
	assertContains(t, ir, "declare void @promise_do_thing(%promise_int_v")
}

func TestExternMultipleParams(t *testing.T) {
	ir := generateIR(t, `
		add_ext(int a, int b) `+"`"+`extern("test_add");
		main() { add_ext(1, 2); }
	`)
	assertContains(t, ir, "declare void @test_add(%promise_int_v %a, %promise_int_v %b)")
	assertContains(t, ir, "call void @test_add")
}

func TestExternReturnValue(t *testing.T) {
	ir := generateIR(t, `
		get_value() int `+"`"+`extern("test_get");
		main() { x := get_value(); }
	`)
	assertContains(t, ir, "declare %promise_int_v @test_get()")
	// Return value should be unpacked via extractvalue
	assertContains(t, ir, "extractvalue %promise_int_v")
}

func TestExternStructTypeDefs(t *testing.T) {
	ir := generateIR(t, `
		print(int x) `+"`"+`extern("promise_print_int");
		main() { print(42); }
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
	assertContains(t, ir, "declare void @test_i8(%promise_i8_v")
}

func TestExternI16Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i16(i16 x) `+"`"+`extern("test_i16");
		main() { }
	`)
	assertContains(t, ir, "%promise_i16_v = type { i8*, %promise_i16_i*, i16 }")
	assertContains(t, ir, "declare void @test_i16(%promise_i16_v")
}

func TestExternI32Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i32(i32 x) `+"`"+`extern("test_i32");
		main() { }
	`)
	assertContains(t, ir, "%promise_i32_v = type { i8*, %promise_i32_i*, i32 }")
	assertContains(t, ir, "declare void @test_i32(%promise_i32_v")
}

func TestExternU8Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u8(u8 x) `+"`"+`extern("test_u8");
		main() { }
	`)
	assertContains(t, ir, "%promise_u8_v = type { i8*, %promise_u8_i*, i8 }")
	assertContains(t, ir, "declare void @test_u8(%promise_u8_v")
}

func TestExternU16Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u16(u16 x) `+"`"+`extern("test_u16");
		main() { }
	`)
	assertContains(t, ir, "%promise_u16_v = type { i8*, %promise_u16_i*, i16 }")
	assertContains(t, ir, "declare void @test_u16(%promise_u16_v")
}

func TestExternU32Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u32(u32 x) `+"`"+`extern("test_u32");
		main() { }
	`)
	assertContains(t, ir, "%promise_u32_v = type { i8*, %promise_u32_i*, i32 }")
	assertContains(t, ir, "declare void @test_u32(%promise_u32_v")
}

func TestExternU64Layout(t *testing.T) {
	ir := generateIR(t, `
		log_u64(u64 x) `+"`"+`extern("test_u64");
		main() { }
	`)
	assertContains(t, ir, "%promise_u64_v = type { i8*, %promise_u64_i*, i64 }")
	assertContains(t, ir, "declare void @test_u64(%promise_u64_v")
}

func TestExternI64Layout(t *testing.T) {
	ir := generateIR(t, `
		log_i64(i64 x) `+"`"+`extern("test_i64");
		main() { }
	`)
	assertContains(t, ir, "%promise_i64_v = type { i8*, %promise_i64_i*, i64 }")
	assertContains(t, ir, "declare void @test_i64(%promise_i64_v")
}

func TestExternF32Layout(t *testing.T) {
	ir := generateIR(t, `
		log_f32(f32 x) `+"`"+`extern("test_f32");
		main() { }
	`)
	assertContains(t, ir, "%promise_f32_v = type { i8*, %promise_f32_i*, float }")
	assertContains(t, ir, "declare void @test_f32(%promise_f32_v")
}

func TestExternCharLayout(t *testing.T) {
	ir := generateIR(t, `
		log_char(char x) `+"`"+`extern("test_char");
		main() { }
	`)
	assertContains(t, ir, "%promise_char_v = type { i8*, %promise_char_i*, i32 }")
	assertContains(t, ir, "declare void @test_char(%promise_char_v")
}

func TestExternUintLayout(t *testing.T) {
	ir := generateIR(t, `
		log_uint(uint x) `+"`"+`extern("test_uint");
		main() { }
	`)
	assertContains(t, ir, "%promise_uint_v = type { i8*, %promise_uint_i*, i64 }")
	assertContains(t, ir, "declare void @test_uint(%promise_uint_v")
}

// --- Header generation: return types and zero-param ---

func TestHeaderExternReturnType(t *testing.T) {
	result := compileResult(t, `
		get_val() int `+"`"+`extern("test_get_val");
		main() { x := get_val(); }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Return type should be the value struct, not void
	assertContains(t, header, "promise_int_v test_get_val(void);")
}

func TestHeaderExternZeroParams(t *testing.T) {
	result := compileResult(t, `
		get_val() int `+"`"+`extern("test_get_val");
		main() { x := get_val(); }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Zero-param functions should have (void) in C
	assertContains(t, header, "(void);")
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
	if err := GenerateHeader(&buf, result.Layouts, result.Externs); err != nil {
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

	// Function declarations with correct struct types
	assertContains(t, header, "void test_log_i32(promise_i32_v x);")
	assertContains(t, header, "void test_log_bool(promise_bool_v x);")
	assertContains(t, header, "void test_log_f32(promise_f32_v x);")
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
	if err := GenerateHeader(&buf, result.Layouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Shared ref param should be pointer in C header
	assertContains(t, header, "void test_modify(promise_int_v* x);")
}

func TestHeaderExternMutRefParam(t *testing.T) {
	result := compileResult(t, `
		update(int ~x) `+"`"+`extern("test_update");
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Mutable ref param should be pointer in C header
	assertContains(t, header, "void test_update(promise_int_v* x);")
}

func TestHeaderGeneration(t *testing.T) {
	result := compileResult(t, `
		print(int x) `+"`"+`extern("promise_print_int");
		print_f(f64 x) `+"`"+`extern("promise_print_f64");
		main() { print(42); print_f(3.14); }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.Externs); err != nil {
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

	// Function declarations
	assertContains(t, header, "void promise_print_int(promise_int_v x);")
	assertContains(t, header, "void promise_print_f64(promise_f64_v x);")
}

// --- String tests ---

func TestStringLiteral(t *testing.T) {
	ir := generateIR(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)
	// Global string constant
	assertContains(t, ir, `c"hello"`)
	// Call to promise_string_new
	assertContains(t, ir, "call i8* @promise_string_new(")
	// Packing into value struct
	assertContains(t, ir, "insertvalue %promise_string_v")
	// Call to extern
	assertContains(t, ir, "call void @promise_print_string(")
}

func TestStringVariable(t *testing.T) {
	ir := generateIR(t, `main() { s := "hello"; }`)
	// Alloca for i8* (string pointer)
	assertContains(t, ir, "alloca i8*")
	// Call to promise_string_new
	assertContains(t, ir, "call i8* @promise_string_new(")
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
	if err := GenerateHeader(&buf, result.Layouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// String layout with flexible array member
	assertContains(t, header, "typedef struct { } promise_string_t;")
	assertContains(t, header, "promise_string_m;")
	assertContains(t, header, "char                 data[];")
	assertContains(t, header, "promise_string_i;")
	assertContains(t, header, "promise_string_v;")

	// Extern declaration using string value struct
	assertContains(t, header, "void promise_print_string(promise_string_v s);")
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
	assertContains(t, ir, "define i32 @main()")
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

func TestStringIntrinsicsDeclared(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	// String intrinsics should always be declared
	assertContains(t, ir, "declare i8* @promise_string_new(i8* %data, i64 %len)")
	assertContains(t, ir, "declare i8* @promise_string_concat(i8* %a, i8* %b)")
	assertContains(t, ir, "declare i1 @promise_string_eq(i8* %a, i8* %b)")
}
