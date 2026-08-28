package string

import (
	"bytes"
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
)

// --- PAL function body tests ---
// These verify that definePALBodies() generates correct IR for print/panic functions.

func TestPrintStringBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)
	// Function body: extracts data/len from string value struct, writes via PAL
	codegentest.AssertContains(t, ir, "define void @promise_print_string(i8*")
	codegentest.AssertContains(t, ir, "bitcast i8* %s to %promise_string_v*")
	codegentest.AssertContains(t, ir, "call i64 @pal_write(i32 1,") // stdout
}

// --- String tests ---

func TestStringLiteral(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)
	// Static string instance global in .rodata (not heap-allocated)
	codegentest.AssertContains(t, ir, `c"hello"`)
	codegentest.AssertContains(t, ir, "private constant { i8*, i64, [5 x i8] }")
	// Packing into value struct
	codegentest.AssertContains(t, ir, "insertvalue %promise_string_v")
	// Call to extern
	codegentest.AssertContains(t, ir, "call void @promise_print_string(")
}

func TestStringVariable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "hello"; }`)
	// Alloca for i8* (string pointer)
	codegentest.AssertContains(t, ir, "alloca i8*")
	// Static string instance bitcast (no promise_string_new call for literals)
	codegentest.AssertContains(t, ir, "bitcast { i8*, i64, [5 x i8] }*")
	// Store i8* into alloca
	codegentest.AssertContains(t, ir, "store i8*")
}

func TestStringConcat(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "hello" + " world"; }`)
	// Two string literals
	codegentest.AssertContains(t, ir, `c"hello"`)
	codegentest.AssertContains(t, ir, `c" world"`)
	// Concat intrinsic
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat(")
}

func TestStringEquality(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { b := "a" == "b"; }`)
	codegentest.AssertContains(t, ir, "call i1 @promise_string_eq(")
}

func TestStringEqFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { b := "a" == "b"; }`)
	// Same-pointer fast path
	codegentest.AssertContains(t, ir, "icmp eq i8* %a, %b")
	// Length comparison
	codegentest.AssertContains(t, ir, "check_len:")
	// memcmp-based data comparison (replaces byte-by-byte loop)
	codegentest.AssertContains(t, ir, "call i32 @memcmp(")
	// Terminal blocks
	codegentest.AssertContains(t, ir, "equal:")
	codegentest.AssertContains(t, ir, "not_equal:")
}

func TestStringNotEqual(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { b := "a" != "b"; }`)
	codegentest.AssertContains(t, ir, "call i1 @promise_string_eq(")
	codegentest.AssertContains(t, ir, "xor i1")
}

func TestStringLayout(t *testing.T) {
	// String layout struct types should always be present
	ir := codegentest.GenerateIR(t, `main() { x := 42; }`)
	codegentest.AssertContains(t, ir, "%promise_string_t = type {}")
	codegentest.AssertContains(t, ir, "%promise_string_m = type { %promise_string_t* }")
	codegentest.AssertContains(t, ir, "%promise_string_i = type { %promise_string_m*, i64, [0 x i8] }")
	codegentest.AssertContains(t, ir, "%promise_string_v = type { i8*, %promise_string_i* }")
}

func TestStringHeader(t *testing.T) {
	result := codegentest.CompileResult(t, `
		print_string(string s) `+"`"+`extern("promise_print_string");
		main() { print_string("hello"); }
	`)

	var buf bytes.Buffer
	if err := codegen.GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// String layout with flexible array member
	codegentest.AssertContains(t, header, "typedef struct { } promise_string_t;")
	codegentest.AssertContains(t, header, "promise_string_m;")
	codegentest.AssertContains(t, header, "char                 data[];")
	codegentest.AssertContains(t, header, "promise_string_i;")
	codegentest.AssertContains(t, header, "promise_string_v;")

	// Extern declaration: string param by pointer
	codegentest.AssertContains(t, header, "void promise_print_string(promise_string_v *s);")
}

func TestStringEscapes(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "hello\nworld"; }`)
	// The global should contain the actual newline character
	codegentest.AssertContains(t, ir, `c"hello\0Aworld"`)
}

func TestStringEmpty(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := ""; }`)
	// Empty string: [0 x i8] global constant
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
	// Length argument should be 0
	codegentest.AssertContains(t, ir, "i64 0)")
}

func TestTripleQuotedStringLiteral(t *testing.T) {
	ir := codegentest.GenerateIR(t, "main() { s := \"\"\"\nhello world\n\"\"\"; }")
	codegentest.AssertContains(t, ir, `c"\0Ahello world\0A"`)
	codegentest.AssertContains(t, ir, "private constant { i8*, i64, [13 x i8] }")
}

func TestRawStringLiteral(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := r"hello\nworld"; }`)
	codegentest.AssertContains(t, ir, `c"hello\5Cnworld"`)
	codegentest.AssertContains(t, ir, "private constant { i8*, i64, [12 x i8] }")
}

func TestStringEscapeBrace(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "a\{b"; }`)
	// \{ should resolve to literal {
	codegentest.AssertContains(t, ir, `c"a{b"`)
}

func TestStringEscapeBraceOnly(t *testing.T) {
	// \{ alone — no interpolation, should take static string path
	ir := codegentest.GenerateIR(t, `main() { s := "\{"; }`)
	codegentest.AssertContains(t, ir, `c"{"`)
}

func TestStringEscapeBraceMultiple(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "\{a} and \{b}"; }`)
	codegentest.AssertContains(t, ir, `c"{a} and {b}"`)
}

func TestStringEscapeBraceWithInterpolation(t *testing.T) {
	// \{ mixed with real interpolation — takes interpolated path
	ir := codegentest.GenerateIR(t, `main() { int x = 42; s := "\{x}={x}"; }`)
	// The escaped \{ produces static text "{x}="
	codegentest.AssertContains(t, ir, `c"{x}="`)
	// The real {x} produces a call to promise_int_to_string
	codegentest.AssertContains(t, ir, "call i8* @promise_int_to_string(")
}

func TestStringEscapeBraceAtEnd(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "end\{"; }`)
	codegentest.AssertContains(t, ir, `c"end{"`)
}

func TestStringEscapeBraceAdjacentInterp(t *testing.T) {
	// \{ immediately followed by real interpolation {x}
	ir := codegentest.GenerateIR(t, `main() { int x = 1; s := "\{{x}"; }`)
	codegentest.AssertContains(t, ir, `c"{"`)
	codegentest.AssertContains(t, ir, "call i8* @promise_int_to_string(")
}

func TestStringEscapeBothBraces(t *testing.T) {
	// \{...\} produces literal {…}
	ir := codegentest.GenerateIR(t, `main() { s := "\{name\}"; }`)
	codegentest.AssertContains(t, ir, `c"{name}"`)
}

func TestStringIntrinsicsDeclared(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := 42; }`)
	// String intrinsics should always be defined (codegen-emitted LLVM IR)
	codegentest.AssertContains(t, ir, "define i8* @promise_string_new(i8* %data, i64 %len)")
	codegentest.AssertContains(t, ir, "define i8* @promise_string_concat(i8* %a, i8* %b)")
	codegentest.AssertContains(t, ir, "define i1 @promise_string_eq(i8* %a, i8* %b)")
	codegentest.AssertContains(t, ir, "define void @promise_string_drop(i8* %ptr)")
}

func TestStringLiteralStaticGlobal(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "hello"; }`)
	// Static string instance in .rodata: { i8* null, i64 literalLen, [5 x i8] c"hello" }
	codegentest.AssertContains(t, ir, "private constant { i8*, i64, [5 x i8] }")
	codegentest.AssertContains(t, ir, `c"hello"`)
	// Bitcast global to i8* — no promise_string_new call for literals
	codegentest.AssertContains(t, ir, "bitcast { i8*, i64, [5 x i8] }*")
}

func TestStringLiteralNegativeLength(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "hi"; }`)
	// Length field should be negative (literal flag = sign bit set)
	// "hi" is 2 bytes, so literalLen = 2 | (1<<63) = -9223372036854775806
	codegentest.AssertContains(t, ir, "i64 -9223372036854775806")
}

func TestStringLenMasksLiteralBit(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "ab"; x := s.len; }`)
	// Length read should mask off sign bit: and i64 %raw, 0x7FFFFFFFFFFFFFFF
	codegentest.AssertContains(t, ir, "and i64")
	codegentest.AssertContains(t, ir, "u0x7FFFFFFFFFFFFFFF")
}

func TestStringNewFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "hello"; }`)
	codegentest.AssertContains(t, ir, "define i8* @promise_string_new(i8* %data, i64 %len)")
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")
	codegentest.AssertContains(t, ir, "oom:")
	codegentest.AssertContains(t, ir, "call void @promise_panic(")
	codegentest.AssertContains(t, ir, "unreachable")
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
	codegentest.AssertContains(t, ir, "store i8* null")
}

func TestStringConcatFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "a" + "b"; }`)
	codegentest.AssertContains(t, ir, "define i8* @promise_string_concat(i8* %a, i8* %b)")
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")
	codegentest.AssertContains(t, ir, "oom:")
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
}

// B0227: string.from_bytes must mask bit 63 of the vector count field
// to handle static vector literals (T0062).
func TestStringFromBytesMasksStaticFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { string s = string.from_bytes([65u8]); }`)
	// The from_bytes codegen should use loadVectorLen which ANDs with 0x7FFFFFFFFFFFFFFF
	codegentest.AssertContains(t, ir, "and i64")
	codegentest.AssertContains(t, ir, "u0x7FFFFFFFFFFFFFFF")
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
}

func TestUserTypeStringField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Dog { string name; }
		main() {
			d := Dog(name: "Rex");
		}
	`)
	// String field stored as i8*
	codegentest.AssertContains(t, ir, "%promise_Dog_i = type { %promise_Dog_m*, i8* }")
	// Should call promise_string_new for the literal
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new")
}

func TestArrayStringElements(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := ["hello", "world"]; }`)
	// String elements stored as i8*
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(i64")
}

// --- Part D: String interpolation ---

func TestStringInterpolationIdent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string name = "world";
			string msg = "hello {name}";
		}
	`)
	// Should call promise_string_concat
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat")
}

func TestStringInterpolationInt(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int x = 42;
			string msg = "x = {x}";
		}
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_int_to_string")
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat")
}

func TestStringInterpolationBool(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			bool flag = true;
			string msg = "flag: {flag}";
		}
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_bool_to_string")
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat")
}

func TestStringInterpolationExpr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string msg = "result: {1 + 2}";
		}
	`)
	codegentest.AssertContains(t, ir, "add i64")
	codegentest.AssertContains(t, ir, "call i8* @promise_int_to_string")
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat")
}

func TestStringInterpolationMultiple(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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

func TestStringInterpolationTuple(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			(int, bool) t = (1, true);
			string s = "{t}";
		}
	`)
	// Tuple formatting should produce calls to int_to_string and bool_to_string
	codegentest.AssertContains(t, ir, "promise_int_to_string")
	codegentest.AssertContains(t, ir, "promise_bool_to_string")
}

// convertToString: f64 interpolation
func TestStringInterpolationF64(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			f64 x = 3.14;
			string msg = "pi is {x}";
		}
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_f64_to_string")
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat")
}

// convertToString: string in interpolation (B0248: copies via concat with empty)
func TestStringInterpolationStringVar(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string name = "world";
			string msg = "hello {name}";
		}
	`)
	// B0248: String is copied (concat with empty), then concatenated with other parts
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat")
}

// B0248: single-string interpolation ("{s}") must copy via concat, not alias
func TestStringInterpolationStringOnly(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string s = "hello";
			string copy = "{s}";
		}
	`)
	// Must produce a concat call (copy), not pass through the original value
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat")
}

// convertToString: f32 interpolation (direct f32 to string)
func TestStringInterpolationF32(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		show(f32 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_f32_to_string")
}

// convertToString: i32 interpolation (sext to i64)
func TestStringInterpolationI32(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		show(i32 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "sext i32")
	codegentest.AssertContains(t, ir, "call i8* @promise_int_to_string")
}

// convertToString: u32 interpolation (zext to i64, calls uint_to_string)
func TestStringInterpolationU32(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		show(u32 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "zext i32")
	codegentest.AssertContains(t, ir, "call i8* @promise_uint_to_string")
}

// resolveEscape: additional escape sequences
func TestStringEscapeSequences(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string a = "hello\tworld";
			string b = "line1\rline2";
			string c = "back\\slash";
			string d = "null\0end";
			string e = "quote\"mark";
		}
	`)
	// Each should produce a global string constant
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new")
}

// --- Stage 8i: Char literals, container .len, string iteration, map compound assignment ---

func TestCharLiteral(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { char c = 'a'; }`)
	codegentest.AssertContains(t, ir, "store i32 97")
}

func TestCharEscape(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { char c = '\n'; }`)
	codegentest.AssertContains(t, ir, "store i32 10")
}

func TestCharEscapeNull(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { char c = '\0'; }`)
	codegentest.AssertContains(t, ir, "store i32 0")
}

func TestCharEscapeBackslash(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { char c = '\\'; }`)
	codegentest.AssertContains(t, ir, "store i32 92")
}

func TestCharMultiByte(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { char c = '€'; }`)
	// € is U+20AC = 8364
	codegentest.AssertContains(t, ir, "store i32 8364")
}

func TestCharEquality(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		check(char a, char b) bool { return a == b; }
		main() { }
	`)
	codegentest.AssertContains(t, ir, "icmp eq i32")
}

func TestCharComparison(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		check(char a, char b) bool { return a < b; }
		main() { }
	`)
	codegentest.AssertContains(t, ir, "icmp slt i32")
}

func TestCharInterpolation(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { char c = 'X'; string s = "char: {c}"; }
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_char_to_string(i32")
}

// convertToString: i16 interpolation (sext to i64)
func TestStringInterpolationI16(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		show(i16 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "sext i16")
	codegentest.AssertContains(t, ir, "call i8* @promise_int_to_string")
}

// convertToString: i8 interpolation (sext to i64)
func TestStringInterpolationI8(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		show(i8 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "sext i8")
	codegentest.AssertContains(t, ir, "call i8* @promise_int_to_string")
}

// convertToString: uint interpolation (direct i64, no extension)
func TestStringInterpolationUint(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		show(uint x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_uint_to_string")
}

// convertToString: u16 interpolation (zext to i64)
func TestStringInterpolationU16(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		show(u16 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "zext i16")
	codegentest.AssertContains(t, ir, "call i8* @promise_uint_to_string")
}

// convertToString: u8 interpolation (zext to i64)
func TestStringInterpolationU8(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		show(u8 x) {
			string msg = "val: {x}";
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "zext i8")
	codegentest.AssertContains(t, ir, "call i8* @promise_uint_to_string")
}

// --- Value-to-string function body tests ---

func TestBoolToStringFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { bool b = true; string s = "{b}"; }
	`)
	codegentest.AssertContains(t, ir, "define i8* @promise_bool_to_string(i8")
	codegentest.AssertContains(t, ir, `c"true"`)
	codegentest.AssertContains(t, ir, `c"false"`)
	codegentest.AssertContains(t, ir, "true:")
	codegentest.AssertContains(t, ir, "false:")
}

func TestIntToStringFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { int x = 42; string s = "{x}"; }
	`)
	codegentest.AssertContains(t, ir, "define i8* @promise_int_to_string(i64")
	codegentest.AssertContains(t, ir, "digit_loop:")
	codegentest.AssertContains(t, ir, "check_neg:")
	codegentest.AssertContains(t, ir, "check_sign:")
	codegentest.AssertContains(t, ir, "done:")
	codegentest.AssertContains(t, ir, "urem i64")
	codegentest.AssertContains(t, ir, "udiv i64")
}

func TestUintToStringFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		show(uint x) { string s = "{x}"; }
		main() { }
	`)
	codegentest.AssertContains(t, ir, "define i8* @promise_uint_to_string(i64")
	codegentest.AssertContains(t, ir, "call i8* @promise_uint_to_string")
	codegentest.AssertContains(t, ir, "digit_loop:")
	codegentest.AssertContains(t, ir, "done:")
	codegentest.AssertContains(t, ir, "urem i64")
	codegentest.AssertContains(t, ir, "udiv i64")
}

func TestF64ToStringFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { f64 x = 3.14; string s = "{x}"; }
	`)
	// promise_f64_to_string is a bridge to the Promise-defined _f64_to_str
	codegentest.AssertContains(t, ir, "define i8* @promise_f64_to_string(double")
	codegentest.AssertContains(t, ir, "call i8* @__mod_std__f64_to_str(double")
}

func TestCharToStringFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { char c = 'X'; string s = "{c}"; }
	`)
	codegentest.AssertContains(t, ir, "define i8* @promise_char_to_string(i32")
	codegentest.AssertContains(t, ir, "one_byte:")
	codegentest.AssertContains(t, ir, "two_byte:")
	codegentest.AssertContains(t, ir, "three_byte:")
	codegentest.AssertContains(t, ir, "four_byte:")
	codegentest.AssertContains(t, ir, "lshr i32")
}

func TestStringLen(t *testing.T) {
	ir := codegentest.GenerateIRWithStd(t, codegentest.StdContainers, `
		main() {
			string s = "hello";
			int n = s.len;
		}
	`)
	// Should GEP to string instance len field and load
	codegentest.AssertContains(t, ir, "load i64")
}

func TestStringViewAdapterLoadsPointerFromBox(t *testing.T) {
	// T1280: the string box is { i8* typeinfo, i8* string_ptr } — the adapter thunk
	// receives the box as i8* (interface convention) and must bitcast + GEP + load
	// field 1 to recover the string pointer receiver for the concrete method.
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display("hi"); }
	`)
	codegentest.AssertContains(t, ir, "string.to_string$view_adapt")
	codegentest.AssertContains(t, ir, "bitcast i8* %this to { i8*, i8* }*")
	codegentest.AssertContains(t, ir, "getelementptr { i8*, i8* }, { i8*, i8* }* %0, i32 0, i32 1")
}

func TestReturnedStringBoxHasStringBoxTypeInfo(t *testing.T) {
	// T1280: when a string is boxed and RETURNED (escaping), the box's field 0 must be
	// the stringbox typeinfo so __promise_structural_drop finds a non-null drop_fn
	// (@__promise_string_box_drop) and frees the cloned string + box instead of
	// misreading the string instance's _variant pointer as a typeinfo header.
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		show_str(string s) Showable { return s; }
		main() { Showable a = show_str("hi"); }
	`)
	// The typeinfo global exists and carries the box drop wrapper as its drop_fn.
	codegentest.AssertContains(t, ir, "@promise_typeinfo_stringbox")
	codegentest.AssertContains(t, ir, "@__promise_string_box_drop")
	// The box stores the stringbox typeinfo into field 0.
	codegentest.AssertContains(t, ir, "bitcast ({ i8*, i8*, i8*, i32, i32 }* @promise_typeinfo_stringbox to i8*)")
}

// T0357: string compound assignment must dispatch through genStringOp,
// not panic in namedFromLLVMType.
func TestCompoundAssignString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string a = "hello ";
			string b = "world";
			a += b;
		}
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat(")
}

// T0357: field compound on a string field routes through genMemberAssign
// (compound branch). Asserts the code path is reachable for non-local sites.
func TestCompoundAssignStringField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder { string s; }
		main() {
			Holder h = Holder(s: "abc");
			h.s += "def";
		}
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat(")
}

// T0357: vector index compound (native path) on string elements routes
// through genVectorCompoundAssign with elemType passed correctly.
func TestCompoundAssignStringVecIndex(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string[] v = ["abc", "xyz"];
			v[0] += "def";
		}
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_string_concat(")
}

func TestHashGetterCharStillUsesFnv1a(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { h := 'x'.hash; }`)
	codegentest.AssertContains(t, ir, "call i64 @__mod_std__fnv1a_hash")
}

func TestHashGetterChar(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { c := 'a'; h := c.hash; }`)
	codegentest.AssertContains(t, ir, "call i64 @__mod_std__fnv1a_hash(i64")
}

func TestHashGetterString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "hi"; h := s.hash; }`)
	codegentest.AssertContains(t, ir, "call i64 @__promise_hash_string(i8*")
}

func TestVectorContainsString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string[] words = ["a", "b"];
			bool has = words.contains("a");
		}
	`)
	codegentest.AssertContains(t, ir, "define i8 @promise_vector_contains(")
	codegentest.AssertContains(t, ir, "call i8 @promise_vector_contains(")
	// String contains uses custom equality comparator
	codegentest.AssertContains(t, ir, "@__promise_eq_string")
}

// --- String byte indexing ---

func TestStringByteIndex(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "hello";
			char c = s[0];
		}
	`)
	codegentest.AssertContains(t, ir, "stridx.ok")
	codegentest.AssertContains(t, ir, "stridx.oob")
	codegentest.AssertContains(t, ir, "zext i8")
}

// --- String method tests ---

func TestStringContains(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "hello world";
			bool has = s.contains("world");
		}
	`)
	// Promise method compiled as a module-prefixed function (std module)
	codegentest.AssertContains(t, ir, "define i1 @__mod_std_string.contains(")
	codegentest.AssertContains(t, ir, "call i1 @__mod_std_string.contains(")
}

func TestStringStartsWith(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "hello";
			bool yes = s.starts_with("hel");
		}
	`)
	codegentest.AssertContains(t, ir, "define i1 @__mod_std_string.starts_with(")
	codegentest.AssertContains(t, ir, "call i1 @__mod_std_string.starts_with(")
}

func TestStringEndsWith(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "hello";
			bool yes = s.ends_with("llo");
		}
	`)
	codegentest.AssertContains(t, ir, "define i1 @__mod_std_string.ends_with(")
	codegentest.AssertContains(t, ir, "call i1 @__mod_std_string.ends_with(")
}

func TestStringIndexOf(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "hello";
			int? idx = s.index_of("ll");
		}
	`)
	codegentest.AssertContains(t, ir, "define { i1, i64 } @__mod_std_string.index_of(")
	codegentest.AssertContains(t, ir, "call { i1, i64 } @__mod_std_string.index_of(")
}

func TestStringTrim(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "  hi  ";
			string trimmed = s.trim();
		}
	`)
	codegentest.AssertContains(t, ir, "define i8* @promise_string_trim(")
	codegentest.AssertContains(t, ir, "call i8* @promise_string_trim(")
}

func TestStringSplit(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "a,b,c";
			string[] parts = s.split(",");
		}
	`)
	codegentest.AssertContains(t, ir, "define i8* @promise_string_split(")
	codegentest.AssertContains(t, ir, "call i8* @promise_string_split(")
}

func TestStringTrimFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := " hi ".trim();
		}
	`)
	codegentest.AssertContains(t, ir, "define i8* @promise_string_trim(i8* %s)")
	codegentest.AssertContains(t, ir, "trim_left_hdr:")
	codegentest.AssertContains(t, ir, "trim_right_hdr:")
	codegentest.AssertContains(t, ir, "build_result:")
	codegentest.AssertContains(t, ir, "icmp eq i8") // whitespace checks
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
}

func TestStringSplitFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "a,b".split(",");
		}
	`)
	codegentest.AssertContains(t, ir, "define i8* @promise_string_split(i8* %s, i8* %sep)")
	codegentest.AssertContains(t, ir, "call i32 @memcmp(")
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")
	codegentest.AssertContains(t, ir, "oom:")
	codegentest.AssertContains(t, ir, "call void @promise_panic(")
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
	codegentest.AssertContains(t, ir, "count_hdr:")
	codegentest.AssertContains(t, ir, "split_hdr:")
	codegentest.AssertContains(t, ir, "split_tail:")
}

func TestStringNextCharFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			for ch in "abc" {}
		}
	`)
	codegentest.AssertContains(t, ir, "define i32 @promise_string_next_char(i8* %s, i64* %pos)")
	codegentest.AssertContains(t, ir, "ret_eof:")
	codegentest.AssertContains(t, ir, "ret i32 -1")
	codegentest.AssertContains(t, ir, "set_1byte:")
	codegentest.AssertContains(t, ir, "cont_hdr:")
	codegentest.AssertContains(t, ir, "cont_body:")
	codegentest.AssertContains(t, ir, "cont_done:")
}

// --- User type interpolation codegen tests ---

func TestStringInterpolationUserTypeDirect(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo {
			int x;
			format!(Writer ~w) { w.write_string("foo"); }
		}
		main() { Foo f = Foo(x: 1); string s = "{f}"; }
	`)
	// Should call Foo.format and Builder.to_string
	codegentest.AssertContains(t, ir, "Foo.format")
	codegentest.AssertContains(t, ir, "Builder.to_string")
	codegentest.AssertContains(t, ir, "interp.format.ok")
}

// T0084: Builder.to_string() result is tracked as a string temp
func TestBuilderToStringTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Builder b = Builder();
			b.write_string("hello");
			assert(b.to_string() == "hello", "ok");
		}
	`)
	// Builder.to_string result should be tracked and dropped at statement end
	codegentest.AssertContains(t, ir, "Builder.to_string")
	codegentest.AssertContains(t, ir, "tmp.drop")
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

func TestSliceExprString(t *testing.T) {
	// String [:] uses native genStringSlice
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "hello";
			string sub = s[1:3];
		}
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
}

func TestSliceExprStringLowOnly(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "hello";
			string sub = s[1:];
		}
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
}

func TestSliceExprStringHighOnly(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "hello";
			string sub = s[:3];
		}
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
}

func TestStringConstantsArePrivate(t *testing.T) {
	// All string constants (@.str.*) must have "private" linkage in the IR.
	ir := codegentest.GenerateIR(t, `
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

func TestInstanceIRStringConstantsPreserved(t *testing.T) {
	// Instance .bc files must contain their own copy of string constants
	// (as private definitions), not extern references to the main IR.
	file, info := codegentest.ParseWithStd(t, `
		type Wrapper[T] {
			T value;
			describe(this) string { return "wrapped"; }
		}
		main() {
			w := Wrapper[int](value: 42);
			string s = w.describe();
		}
	`)
	result := codegen.Compile(file, info, "")
	instIRs := result.InstanceIRs()

	wrapIR, ok := instIRs["Wrapper[int]"]
	if !ok {
		t.Fatalf("expected Wrapper[int] in instance IRs, got: %v", codegentest.MapKeys(instIRs))
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

// --- Embed getter tests ---

func TestEmbedStringGetter(t *testing.T) {
	file, info := codegentest.ParseWithStd(t, `
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
	result := codegen.Compile(file, info, "")
	ir := result.Module.String()
	codegentest.AssertContains(t, ir, "@__user.schema()")
	codegentest.AssertContains(t, ir, "CREATE TABLE foo;")
	codegentest.AssertContains(t, ir, "@promise_string_new")
}

func TestEmbedBytesGetter(t *testing.T) {
	file, info := codegentest.ParseWithStd(t, `
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
	result := codegen.Compile(file, info, "")
	ir := result.Module.String()
	codegentest.AssertContains(t, ir, "define i8* @__user.data()")
	codegentest.AssertContains(t, ir, "@pal_alloc")
	codegentest.AssertContains(t, ir, "@llvm.memcpy")
}

// TestTimeoutContextEmitsFormattedDuration verifies that GenerateTestMain emits
// a compile-time-formatted duration string (e.g. "2s", "500ms") in the TIMEOUT
// context block rather than a bare integer (T1199).
func TestTimeoutContextEmitsFormattedDuration(t *testing.T) {
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
	// 2s timeout = 2_000_000_000 ns → "2s"
	result.GenerateTestMain(info.Tests, map[string]int64{"myTest": 2_000_000_000})
	ir := result.Module.String()
	// Must contain the compile-time-formatted duration string global (T1199)
	codegentest.AssertContains(t, ir, `@.str.timeout_dur_myTest = private constant [2 x i8] c"2s"`)
	// The print_timeout_ctx block must reference the duration global, not sdiv
	codegentest.AssertContains(t, ir, "print_timeout_ctx_myTest")
	codegentest.AssertContains(t, ir, ".str.timeout_dur_myTest")
	// No runtime sdiv in the context print block — check within the context block
	ctxIdx := strings.Index(ir, "print_timeout_ctx_myTest:")
	afterIdx := strings.Index(ir[ctxIdx:], "after_timeout_ctx_myTest")
	ctxBlock := ir[ctxIdx : ctxIdx+afterIdx]
	if strings.Contains(ctxBlock, "sdiv") {
		t.Errorf("print_timeout_ctx block should not contain sdiv:\n%s", ctxBlock)
	}

	// Sub-second: 500ms = 500_000_000 ns → "500ms"
	result2 := codegentest.CompileResult(t, `
		fastTest() `+"`test"+` { }
	`)
	info2, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`fastTest() ` + "`test" + ` { }`)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result2.GenerateTestMain(info2.Tests, map[string]int64{"fastTest": 500_000_000})
	ir2 := result2.Module.String()
	codegentest.AssertContains(t, ir2, `@.str.timeout_dur_fastTest = private constant [5 x i8] c"500ms"`)
	ctxIdx2 := strings.Index(ir2, "print_timeout_ctx_fastTest:")
	afterIdx2 := strings.Index(ir2[ctxIdx2:], "after_timeout_ctx_fastTest")
	ctxBlock2 := ir2[ctxIdx2 : ctxIdx2+afterIdx2]
	if strings.Contains(ctxBlock2, "sdiv") {
		t.Errorf("print_timeout_ctx block should not contain sdiv:\n%s", ctxBlock2)
	}
}
