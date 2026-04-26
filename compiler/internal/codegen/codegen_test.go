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

// stdContainers provides native type declarations for Slice, Map, and string.
const stdContainers = `
type string ` + "`" + `native {
	int len;
	contains(string sub) bool ` + "`" + `native;
	starts_with(string prefix) bool ` + "`" + `native;
	ends_with(string suffix) bool ` + "`" + `native;
	index_of(string sub) int? ` + "`" + `native;
	trim() string ` + "`" + `native;
	split(string sep) string[] ` + "`" + `native;
}
type Slice[T] ` + "`" + `native {
	int len;
	push(T elem) ` + "`" + `native;
	pop() T? ` + "`" + `native;
	contains(T elem) bool ` + "`" + `native;
	remove(int index) ` + "`" + `native;
}
type Map[K, V] ` + "`" + `native {
	int len;
	contains(K key) bool ` + "`" + `native;
	remove(K key) bool ` + "`" + `native;
	keys() K[] ` + "`" + `native;
	values() V[] ` + "`" + `native;
}
`

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
	// Extern declaration: all params passed as i8* (pointer) for C ABI
	assertContains(t, ir, "declare void @promise_print_int(i8*")
	// Struct packing via insertvalue
	assertContains(t, ir, "insertvalue %promise_int_v")
}

func TestPrintBool(t *testing.T) {
	ir := generateIR(t, `
		print(bool x) `+"`"+`extern("promise_print_bool");
		main() { print(true); }
	`)
	assertContains(t, ir, "%promise_bool_v = type")
	assertContains(t, ir, "declare void @promise_print_bool(i8*")
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
	assertContains(t, ir, "declare void @promise_print_f64(i8*")
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

	// Function declarations: all params by pointer
	assertContains(t, header, "void promise_print_int(promise_int_v *x);")
	assertContains(t, header, "void promise_print_f64(promise_f64_v *x);")
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
	// Should GEP into Outer to load the child value struct
	assertContains(t, ir, "getelementptr %promise_Outer_i")
	assertContains(t, ir, "load { i8*, i8* }")
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
	// Should call malloc for heap allocation
	assertContains(t, ir, "call i8* @malloc(i64")
	// Should store len and cap
	assertContains(t, ir, "store i64 3")
	// Should store elements
	assertContains(t, ir, "store i64 1")
	assertContains(t, ir, "store i64 2")
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
	assertContains(t, ir, "call i8* @malloc(i64")
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
	// Should call promise_map_new and promise_map_set
	assertContains(t, ir, "call i8* @promise_map_new(")
	assertContains(t, ir, "call void @promise_map_set(")
}

func TestMapIndex(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
			int? v = m["a"];
		}
	`)
	// Should call promise_map_get
	assertContains(t, ir, "call i8* @promise_map_get(")
	// Should check for NULL
	assertContains(t, ir, "icmp eq")
	// Should have found/notfound/merge blocks
	assertContains(t, ir, "map.found")
	assertContains(t, ir, "map.notfound")
	assertContains(t, ir, "map.merge")
}

func TestMapIndexWithElvis(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
			int v = m["a"] ?: 0;
		}
	`)
	// Should use map_get + elvis
	assertContains(t, ir, "call i8* @promise_map_get(")
	assertContains(t, ir, "elvis.some")
}

func TestMapIndexAssign(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
			m["a"] = 42;
		}
	`)
	// Should call promise_map_set
	assertContains(t, ir, "call void @promise_map_set(")
}

func TestMapIntKeys(t *testing.T) {
	ir := generateIR(t, `main() { m := {1: "one", 2: "two"}; }`)
	// Should use null hash/eq (byte-level for int keys)
	assertContains(t, ir, "call i8* @promise_map_new(i64 8, i64 8, i8* null, i8* null)")
}

func TestMapIntrinsics(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	// Map intrinsics should always be declared
	assertContains(t, ir, "declare i8* @promise_map_new(i64 %key_size, i64 %val_size, i8* %hash_fn, i8* %eq_fn)")
	assertContains(t, ir, "declare void @promise_map_set(i8* %m, i8* %key, i8* %val)")
	assertContains(t, ir, "declare i8* @promise_map_get(i8* %m, i8* %key)")
	assertContains(t, ir, "declare i64 @promise_map_len(i8* %m)")
}

func TestMapForIn(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1, "b": 2};
			for entry in m {
			}
		}
	`)
	// Should call promise_map_iter_next
	assertContains(t, ir, "call i32 @promise_map_iter_next(")
	// Should have loop blocks
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
	// Should create anonymous function
	assertContains(t, ir, "define i64 @.lambda.0(i64 %x)")
	// Lambda returned as i8* via bitcast
	assertContains(t, ir, "bitcast i64 (i64)* @.lambda.0 to i8*")
}

func TestLambdaCall(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> x + 1;
			int y = f(42);
		}
	`)
	// Should do indirect call via bitcast
	assertContains(t, ir, "bitcast i8*")
	assertContains(t, ir, "call i64")
}

func TestLambdaBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> int { return x * 2; };
		}
	`)
	assertContains(t, ir, "define i64 @.lambda.0(i64 %x)")
	assertContains(t, ir, "mul i64")
}

func TestLambdaVoid(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> void { return; };
		}
	`)
	assertContains(t, ir, "define void @.lambda.0(i64 %x)")
}

func TestLambdaVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			f := |int x| -> x + 1;
		}
	`)
	// Lambda stored as i8*
	assertContains(t, ir, "alloca i8*")
	assertContains(t, ir, "store i8*")
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
	assertNotContains(t, ir, "call i8* @promise_int_to_string")
	assertNotContains(t, ir, "call i8* @promise_f64_to_string")
	assertContains(t, ir, "call i8* @promise_string_concat")
}

// convertToString: f32 interpolation (fpext to double)
func TestStringInterpolationF32(t *testing.T) {
	ir := generateIR(t, `
		show(f32 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "fpext float")
	assertContains(t, ir, "call i8* @promise_f64_to_string")
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

// convertToString: u32 interpolation (zext to i64)
func TestStringInterpolationU32(t *testing.T) {
	ir := generateIR(t, `
		show(u32 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	assertContains(t, ir, "zext i32")
	assertContains(t, ir, "call i8* @promise_int_to_string")
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
	ir := generateIRWithStd(t, stdContainers, `
		check(int[3] arr) int { return arr.len; }
		main() { }
	`)
	assertContains(t, ir, "getelementptr { i64, i64 }")
	assertContains(t, ir, "load i64")
}

func TestMapLen(t *testing.T) {
	ir := generateIRWithStd(t, stdContainers, `
		main() {
			m := {"a": 1};
			int n = m.len;
		}
	`)
	assertContains(t, ir, "call i64 @promise_map_len(")
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
	// Should get, add, then set
	assertContains(t, ir, "call i8* @promise_map_get(")
	assertContains(t, ir, "mapcomp.ok")
	assertContains(t, ir, "mapcomp.panic")
	assertContains(t, ir, "add i64")
	assertContains(t, ir, "call void @promise_map_set(")
}

func TestMapCompoundAssignMul(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"x": 2};
			m["x"] *= 3;
		}
	`)
	assertContains(t, ir, "call i8* @promise_map_get(")
	assertContains(t, ir, "mul i64")
	assertContains(t, ir, "call void @promise_map_set(")
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
	assertContains(t, ir, "call i8* @malloc(")
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
	// Should call promise_type_is and convert to i1
	assertContains(t, ir, "call i32 @promise_type_is")
	assertContains(t, ir, "icmp ne i32")
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
			Dog d = Dog(breed: "Lab");
		}
	`)
	// Constructor should zero-init name (i8*) and age (i64) from inherited fields
	assertContains(t, ir, "getelementptr %promise_Dog_i")
	// Should have zeroinitializer or null stores for the unprovided fields
	assertContains(t, ir, "store i8* null")
	assertContains(t, ir, "store i64 0")
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

// generateIRWithStd compiles with std declarations (IsStd=true) merged before user code.
func generateIRWithStd(t *testing.T, stdSrc, userSrc string) string {
	t.Helper()
	// Parse std
	stdInput := antlr.NewInputStream(stdSrc)
	stdLexer := parser.NewPromiseLexer(stdInput)
	stdLexer.RemoveErrorListeners()
	stdStream := antlr.NewCommonTokenStream(stdLexer, antlr.TokenDefaultChannel)
	stdP := parser.NewPromiseParser(stdStream)
	stdP.RemoveErrorListeners()
	stdTree := stdP.CompilationUnit()
	stdFile, errs := ast.Build("std.pr", stdTree)
	if len(errs) > 0 {
		t.Fatalf("std AST build errors: %v", errs)
	}
	for _, d := range stdFile.Decls {
		switch dd := d.(type) {
		case *ast.FuncDecl:
			dd.IsStd = true
		case *ast.TypeDecl:
			dd.IsStd = true
		case *ast.EnumDecl:
			dd.IsStd = true
		}
	}

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

	// Merge
	merged := make([]ast.Decl, 0, len(stdFile.Decls)+len(userFile.Decls))
	merged = append(merged, stdFile.Decls...)
	merged = append(merged, userFile.Decls...)
	userFile.Decls = merged

	info, errs := sema.Check(userFile)
	if len(errs) > 0 {
		t.Fatalf("sema errors: %v", errs)
	}
	result := Compile(userFile, info)
	return result.Module.String()
}

// compileResultWithStd compiles with std declarations and returns the CompileResult.
func compileResultWithStd(t *testing.T, stdSrc, userSrc string) *CompileResult {
	t.Helper()
	stdInput := antlr.NewInputStream(stdSrc)
	stdLexer := parser.NewPromiseLexer(stdInput)
	stdLexer.RemoveErrorListeners()
	stdStream := antlr.NewCommonTokenStream(stdLexer, antlr.TokenDefaultChannel)
	stdP := parser.NewPromiseParser(stdStream)
	stdP.RemoveErrorListeners()
	stdTree := stdP.CompilationUnit()
	stdFile, errs := ast.Build("std.pr", stdTree)
	if len(errs) > 0 {
		t.Fatalf("std AST build errors: %v", errs)
	}
	for _, d := range stdFile.Decls {
		switch dd := d.(type) {
		case *ast.FuncDecl:
			dd.IsStd = true
		case *ast.TypeDecl:
			dd.IsStd = true
		case *ast.EnumDecl:
			dd.IsStd = true
		}
	}

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

	merged := make([]ast.Decl, 0, len(stdFile.Decls)+len(userFile.Decls))
	merged = append(merged, stdFile.Decls...)
	merged = append(merged, userFile.Decls...)
	userFile.Decls = merged

	info, errs := sema.Check(userFile)
	if len(errs) > 0 {
		t.Fatalf("sema errors: %v", errs)
	}
	return Compile(userFile, info)
}

func TestStdFuncMangledName(t *testing.T) {
	// Std functions should get __std_ prefix in LLVM IR
	ir := generateIRWithStd(t,
		`helper() int { return 42; }`,
		`main() { x := helper(); }`,
	)
	assertContains(t, ir, "define i64 @__std_helper")
	assertContains(t, ir, "call i64 @__std_helper")
}

func TestStdUserNameCollision(t *testing.T) {
	// When user defines same-name function, user version goes to funcs, std to stdFuncs
	ir := generateIRWithStd(t,
		`helper() int { return 42; }`,
		`
		helper() int { return 99; }
		main() { x := helper(); }
		`,
	)
	// Both functions should exist with different names
	assertContains(t, ir, "define i64 @__std_helper")
	assertContains(t, ir, "define i64 @helper")
	// main calls the user version (not the std version)
	assertContains(t, ir, "call i64 @helper()")
}

func TestStdCallViaStdPrefix(t *testing.T) {
	// std.X() should call the std-mangled version
	ir := generateIRWithStd(t,
		`helper() int { return 42; }`,
		`
		helper() int { return 99; }
		main() {
			x := helper();
			y := std.helper();
		}
		`,
	)
	// User call goes to @helper, std call goes to @__std_helper
	assertContains(t, ir, "call i64 @helper()")
	assertContains(t, ir, "call i64 @__std_helper()")
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
	result.GenerateTestMain(info.Tests)
	ir := result.Module.String()
	assertContains(t, ir, "define i32 @main")
	assertContains(t, ir, "call i32 @promise_test_run")
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
	result.GenerateTestMain(info.Tests)
	ir := result.Module.String()
	// Should still have main but with test runner content
	assertContains(t, ir, "define i32 @main")
	assertContains(t, ir, "call i32 @promise_test_run")
	assertContains(t, ir, "call void @promise_test_summary")
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
	// When no user function shadows, std function is accessible by plain name
	ir := generateIRWithStd(t,
		`helper() int { return 42; }`,
		`main() { x := helper(); }`,
	)
	// Call should go to the __std_helper function
	assertContains(t, ir, "call i64 @__std_helper")
}
