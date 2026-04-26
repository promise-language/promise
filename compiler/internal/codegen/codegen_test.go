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
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
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
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
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
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
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
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
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
	assertContains(t, ir, "call i8* @malloc(i64")
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

func TestMallocDeclared(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "declare i8* @malloc(i64 %size)")
}

func TestUserTypeExternUnpacking(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		get_dog() Dog `+"`"+`extern("get_dog");
		main() {
			d := get_dog();
		}
	`)
	// Extern returns promise_Dog_v
	assertContains(t, ir, "declare %promise_Dog_v @get_dog()")
	// Unpack: extractvalue field 1 + bitcast back to i8*
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
	// Inner stored as i8* in Outer's instance struct
	assertContains(t, ir, "%promise_Inner_i = type { %promise_Inner_m*, i64 }")
	assertContains(t, ir, "%promise_Outer_i = type { %promise_Outer_m*, i8* }")
	// Both should be allocated via malloc
	assertContains(t, ir, "call i8* @malloc(i64")
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
	// Should GEP into Outer to load the child i8*
	assertContains(t, ir, "getelementptr %promise_Outer_i")
	assertContains(t, ir, "load i8*")
}

func TestUserTypeZeroArgConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		main() {
			p := Point();
		}
	`)
	// Should allocate and zero-initialize both fields
	assertContains(t, ir, "call i8* @malloc(i64")
	// Both fields should get zero-initialized (store i64 0)
	assertContains(t, ir, "store i64 0")
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
	// Extern declaration uses value struct
	assertContains(t, ir, "declare void @print_color(%promise_Color_v")
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
	// Extern returns value struct
	assertContains(t, ir, "declare %promise_Color_v @get_color()")
	// Should unpack via extractvalue
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
	// Extern declaration uses value struct
	assertContains(t, ir, "declare void @send_shape(%promise_Shape_v")
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
	// Data enum unpacking: extractvalue from value struct, build internal struct
	assertContains(t, ir, "declare %promise_Shape_v @get_shape()")
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

func TestRaiseStmt(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { raise "parse error"; }
		main() { }
	`)
	// Should wrap error in Error result: tag=true
	assertContains(t, ir, "i1 true")
	assertContains(t, ir, "ret { i1, i64, i8* }")
	// Should create the error string
	assertContains(t, ir, `c"parse error"`)
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
	// Should call promise_panic
	assertContains(t, ir, "call void @promise_panic(")
	assertContains(t, ir, "unreachable")
}

func TestErrorHandler(t *testing.T) {
	ir := generateIR(t, `
		parse(string s) int! { return 0; }
		main() {
			x := parse("42") ? e { 0 };
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
			x := parse("42") ? _ { 0 };
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
		validate(string s) void! { raise "invalid"; }
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
		validate(string s) void! { raise "invalid"; }
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
		validate(string s) void! { raise "invalid"; }
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
		validate(string s) void! { raise "invalid"; }
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
				raise "empty";
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

// --- Generic type tests ---

func TestGenericTypeLayout(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 42);
		}
	`)
	assertContains(t, ir, "Box__int_i")
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
	assertContains(t, ir, "Box__int_i")
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
	assertContains(t, ir, "define i64 @Box__int.get")
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
	assertContains(t, ir, "define void @Box__int.set")
}

func TestGenericMultipleInstances(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			a := Box[int](value: 42);
			b := Box[string](value: "hi");
		}
	`)
	assertContains(t, ir, "Box__int_i")
	assertContains(t, ir, "Box__string_i")
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
	assertContains(t, ir, "Box__int_i")
	assertContains(t, ir, "Box__string_i")
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
	assertContains(t, ir, "Option__int_enum")
	assertContains(t, ir, "store i64 42")
}

func TestGenericEnumNone(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].None;
		}
	`)
	assertContains(t, ir, "Option__int_enum")
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

func TestGenericConstructorZeroInit(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int]();
		}
	`)
	// Zero-init with i64 0 for the int field
	assertContains(t, ir, "Box__int_i")
}

// --- Generic function tests ---

func TestGenericFunc(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			int r = identity[int](42);
		}
	`)
	assertContains(t, ir, "define i64 @identity__int")
	assertContains(t, ir, "ret i64")
}

func TestGenericFuncString(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			string s = identity[string]("hello");
		}
	`)
	assertContains(t, ir, "define i8* @identity__string")
}

func TestGenericFuncMultipleInstances(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			int a = identity[int](42);
			string b = identity[string]("hi");
		}
	`)
	assertContains(t, ir, "@identity__int")
	assertContains(t, ir, "@identity__string")
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
	assertContains(t, ir, "define void @Box__int.replace")
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
	assertContains(t, ir, "define void @consume__int")
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
	assertContains(t, ir, "define { i1, i64, i8* } @tryIdentity__int")
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
