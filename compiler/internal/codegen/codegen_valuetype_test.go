package codegen

import (
	"testing"

	irtypes "github.com/llir/llvm/ir/types"
)

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

func TestBitwiseNot(t *testing.T) {
	ir := generateIR(t, `main() { x := ~0; }`)
	assertContains(t, ir, "xor i64")
}

// === Stage 8g: Container Codegen Tests ===

// --- Part A: Tuple tests ---

func TestTupleLiteral(t *testing.T) {
	ir := generateIR(t, `main() { x := (1, 2); }`)
	// Should use insertvalue to build { i64, i64 } struct
	assertContains(t, ir, "insertvalue { i64, i64 }")
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

func TestPrimitiveViewAdapterLoadsScalarFromPointer(t *testing.T) {
	// The adapter thunk receives i8* (interface convention) and must
	// bitcast + GEP + load to get the scalar value for the concrete method.
	ir := generateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display(42); }
	`)
	assertContains(t, ir, "int.to_string$view_adapt")
	// T1276: the box is { i8* typeinfo, i64 scalar } — the adapter bitcasts i8* to
	// the box struct and loads the scalar from field 1 (not offset 0).
	assertContains(t, ir, "bitcast i8* %this to { i8*, i64 }*")
	assertContains(t, ir, "getelementptr { i8*, i64 }, { i8*, i64 }* %0, i32 0, i32 1")
	assertContains(t, ir, "load i64, i64*")
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

// llvmTypeAlign coverage: float, double, pointer, array
func TestLlvmTypeAlignFloat(t *testing.T) {
	if a := llvmTypeAlign(irtypes.Float); a != 4 {
		t.Errorf("float align: got %d, want 4", a)
	}
}

func TestLlvmTypeAlignLargeInt(t *testing.T) {
	// LLVM's default x86_64/aarch64 datalayouts specify no ABI alignment wider
	// than i128, so anything beyond that still aligns to 16 bytes (verified
	// against real `opt`-computed struct offsets, not just LangRef reading).
	// Capping at 8 (the pre-T1419 bug) mis-sized enum variant data blobs and
	// truncated wide-int fields at non-zero offsets.
	if a := llvmTypeAlign(irtypes.NewInt(128)); a != 16 {
		t.Errorf("i128 align: got %d, want 16", a)
	}
	if a := llvmTypeAlign(irtypes.NewInt(256)); a != 16 {
		t.Errorf("i256 align: got %d, want 16", a)
	}
	if a := llvmTypeAlign(irtypes.NewInt(512)); a != 16 {
		t.Errorf("i512 align: got %d, want 16", a)
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
