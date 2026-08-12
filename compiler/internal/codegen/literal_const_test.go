package codegen

import (
	"testing"

	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// Direct unit tests for the literal → LLVM constant folds (T1418). These are the
// single source of truth shared by the scalar path (genIntLit/genFloatLit/…) and
// the static .rodata vector-literal fold (tryConstantExpr), so they are worth
// pinning at the function level rather than only through emitted IR — the bug
// was precisely that the two copies disagreed about a literal's value.

func TestConstIntFromRaw(t *testing.T) {
	i128 := irtypes.NewInt(128)
	i256 := irtypes.NewInt(256)
	tests := []struct {
		name string
		typ  *irtypes.IntType
		raw  string
		want string // decimal
	}{
		{"decimal", irtypes.I64, "42", "42"},
		{"zero", irtypes.I64, "0", "0"},
		{"hex", irtypes.I64, "0xff", "255"},
		{"octal", irtypes.I64, "0o17", "15"},
		{"binary", irtypes.I64, "0b1010", "10"},
		{"underscores", irtypes.I64, "1_000_000", "1000000"},
		{"int64_max", irtypes.I64, "9223372036854775807", "9223372036854775807"},
		// u64.max does not fit int64 — the old strconv.ParseInt path range-errored
		// here and fell through to ParseUint + an int64 round-trip.
		{"uint64_max", irtypes.I64, "18446744073709551615", "18446744073709551615"},
		// 2^63: first value strconv.ParseInt rejects. Was sign-extended to all-ones
		// high bits in a wide element.
		{"two_pow_63_wide", i128, "9223372036854775808", "9223372036854775808"},
		// 2^64-1: last value strconv.ParseUint accepts. Same sign-extension bug.
		{"two_pow_64_minus_1_wide", i128, "18446744073709551615", "18446744073709551615"},
		// >= 2^64: ParseUint range-errored too and the discarded error left
		// MaxUint64 behind, which became -1 at any width.
		{"two_pow_64_plus_1_wide", i128, "18446744073709551617", "18446744073709551617"},
		{
			"i128_max", i128,
			"170141183460469231731687303715884105727",
			"170141183460469231731687303715884105727",
		},
		{
			"u128_max", i128,
			"340282366920938463463374607431768211455",
			"340282366920938463463374607431768211455",
		},
		{
			"u256_max", i256,
			"115792089237316195423570985008687907853269984665640564039457584007913129639935",
			"115792089237316195423570985008687907853269984665640564039457584007913129639935",
		},
		// Radix + underscore handling must survive above 64 bits too.
		{"wide_hex", i128, "0x1_0000_0000_0000_0000", "18446744073709551616"},
		{
			"wide_binary", i128,
			"0b1_0000000000000000000000000000000000000000000000000000000000000000",
			"18446744073709551616",
		},
		{"wide_octal", i128, "0o2000000000000000000000", "18446744073709551616"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cv, ok := constIntFromRaw(tt.typ, tt.raw)
			if !ok {
				t.Fatalf("constIntFromRaw(%q) reported unparseable", tt.raw)
			}
			if got := cv.X.String(); got != tt.want {
				t.Errorf("constIntFromRaw(%q) = %s, want %s", tt.raw, got, tt.want)
			}
			if cv.Typ != tt.typ {
				t.Errorf("constIntFromRaw(%q) type = %v, want %v", tt.raw, cv.Typ, tt.typ)
			}
		})
	}
}

func TestConstIntFromRawUnparseable(t *testing.T) {
	// Unreachable post-sema (validateIntRange rejects these first), but the
	// helper must report failure rather than silently folding to a wrong value —
	// the discarded strconv.ParseUint error is what turned an out-of-range
	// literal into MaxUint64/-1. Callers fall back: genIntLit emits 0, and
	// tryConstantExpr returns nil so the vector takes the heap path.
	for _, raw := range []string{"", "0x", "0b", "12abc", "08", "1.5", "-", "0o9"} {
		if cv, ok := constIntFromRaw(irtypes.I64, raw); ok {
			t.Errorf("constIntFromRaw(%q) = %s, want unparseable", raw, cv.X)
		}
	}
}

func TestConstFloatFromRaw(t *testing.T) {
	tests := []struct {
		name string
		typ  *irtypes.FloatType
		raw  string
		want float64
	}{
		{"f64", irtypes.Double, "1.5", 1.5},
		{"f64_underscores", irtypes.Double, "1_000.5", 1000.5},
		{"f64_exponent", irtypes.Double, "1.5e3", 1500},
		// ParseFloat(s, 32) rounds to the nearest float32 before widening, so the
		// stored double is exactly the float32 value LLVM will emit.
		{"f32_rounds_to_single", irtypes.Float, "0.1", float64(float32(0.1))},
		{"f32_exact", irtypes.Float, "2.5", 2.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cv := constFloatFromRaw(tt.typ, tt.raw)
			got, _ := cv.X.Float64()
			if got != tt.want {
				t.Errorf("constFloatFromRaw(%q) = %v, want %v", tt.raw, got, tt.want)
			}
			if cv.Typ != tt.typ {
				t.Errorf("constFloatFromRaw(%q) type = %v, want %v", tt.raw, cv.Typ, tt.typ)
			}
		})
	}
	// The f32 fold must actually narrow: 0.1 as float32 differs from 0.1 as float64.
	single, _ := constFloatFromRaw(irtypes.Float, "0.1").X.Float64()
	double, _ := constFloatFromRaw(irtypes.Double, "0.1").X.Float64()
	if single == double {
		t.Errorf("f32 fold did not narrow: %v == %v", single, double)
	}
}

func TestConstCharFromRaw(t *testing.T) {
	// The escape set is exactly what PromiseLexer.g4's CHAR_LITERAL admits:
	// ~['\\] | '\\' [nrtb\\'0].
	tests := []struct {
		raw  string
		want int64
	}{
		{`'a'`, 'a'},
		{`'\n'`, '\n'},
		{`'\r'`, '\r'},
		{`'\t'`, '\t'},
		{`'\b'`, '\b'},
		{`'\\'`, '\\'},
		{`'\''`, '\''},
		{`'\0'`, 0},
		{`' '`, ' '},
		{`'é'`, 233},    // two-byte rune → utf8.DecodeRuneInString path
		{`'€'`, 8364},   // three-byte rune
		{`'😀'`, 128512}, // four-byte rune (outside the BMP)
		// Escape the lexer never produces: the fold's default branch takes the
		// escaped byte verbatim rather than dropping the backslash silently.
		{`'\q'`, 'q'},
	}
	for _, tt := range tests {
		cv := constCharFromRaw(tt.raw)
		if got := cv.X.Int64(); got != tt.want {
			t.Errorf("constCharFromRaw(%s) = %d, want %d", tt.raw, got, tt.want)
		}
		if cv.Typ != irtypes.I32 {
			t.Errorf("constCharFromRaw(%s) type = %v, want i32", tt.raw, cv.Typ)
		}
	}
}

func TestConstBool(t *testing.T) {
	if got := constBool(true); got.X.Int64() != 1 || got.Typ != irtypes.I1 {
		t.Errorf("constBool(true) = %v %v, want i1 1", got.Typ, got.X)
	}
	if got := constBool(false); got.X.Int64() != 0 || got.Typ != irtypes.I1 {
		t.Errorf("constBool(false) = %v %v, want i1 0", got.Typ, got.X)
	}
}

// TestStaticVectorLiteralMatchesScalar locks in the invariant the T1418 refactor
// exists to guarantee: the scalar literal path and the static .rodata vector
// fold produce the same constant for the same source text. Before the fix the
// scalar store held the correct full-precision value while the array initializer
// next to it held a sign-extended one.
func TestStaticVectorLiteralMatchesScalar(t *testing.T) {
	ir := generateIR(t, `main() {
		u128 s = 18446744073709551617u128;
		u128[] v = [18446744073709551617u128];
	}`)
	assertContains(t, ir, "store i128 u0x10000000000000001")
	assertContains(t, ir, "[1 x i128] [i128 u0x10000000000000001]")
}

// TestStaticVectorLiteralRadixAndUnderscores covers the base-0 prefix handling
// in the shared int fold for narrow element types — tryConstantExpr's old
// strconv.ParseInt(raw, 0, 64) accepted the same prefixes, so this pins that
// behavior across the swap to big.Int.
func TestStaticVectorLiteralRadixAndUnderscores(t *testing.T) {
	ir := generateIR(t, `main() {
		int[] v = [0xff, 0o17, 0b1010, 1_000_000];
	}`)
	assertContains(t, ir, "[4 x i64] [i64 255, i64 15, i64 10, i64 1000000]")
}

// TestStaticVectorLiteralCharEscapes pins the char fold through the vector path.
// tryConstantExpr used to carry its own copy of the escape table; it now shares
// constCharFromRaw with genCharLit, so a regression would hit both paths.
func TestStaticVectorLiteralCharEscapes(t *testing.T) {
	ir := generateIR(t, `main() {
		char[] v = ['\n', '\r', '\t', '\b', '\\', '\'', '\0', 'é'];
	}`)
	assertContains(t, ir,
		"[8 x i32] [i32 10, i32 13, i32 9, i32 8, i32 92, i32 39, i32 0, i32 233]")
}

// TestStaticVectorLiteralF32 covers constFloatFromRaw's bitSize=32 branch
// through the vector fold: the element must be rounded to float32 precision, not
// stored as a truncated double.
func TestStaticVectorLiteralF32(t *testing.T) {
	ir := generateIR(t, `main() {
		f32[] v = [0.1f32, -2.5f32];
	}`)
	// 0.1f32 == 0x3DCCCCCD; llir prints the float as its exact double widening.
	assertContains(t, ir, "[2 x float] [float 0x3FB99999A0000000, float -2.5]")
}

// TestStaticVectorLiteralNonConstantFallback covers tryConstantExpr's nil
// returns: a non-literal operand under unary minus, a non-negation unary op, and
// an element kind the fold does not handle. Each must fall back to the heap
// path rather than emitting a bogus .rodata initializer.
func TestStaticVectorLiteralNonConstantFallback(t *testing.T) {
	for _, src := range []string{
		`main() { int x = 1; int[] v = [-x]; }`,
		`main() { bool[] v = [!true]; }`,
		`main() { string[] v = ["a"]; }`,
	} {
		ir := generateIR(t, src)
		assertNotContains(t, ir, "@.arr.0 =")
		assertContains(t, ir, "@pal_alloc(")
	}
}

// TestTryConstantExprUnsuffixedWideElement covers the element form the suffixed
// tests miss: a literal with NO suffix whose type comes from the declared vector
// element type. Sema's validateIntRange only runs on suffixed literals, so
// nothing upstream would have caught a truncated fold here.
func TestTryConstantExprUnsuffixedWideElement(t *testing.T) {
	ir := generateIR(t, `main() {
		u128[] a = [18446744073709551617, 340282366920938463463374607431768211455];
		i128[] b = [-170141183460469231731687303715884105728];
	}`)
	assertContains(t, ir,
		"[2 x i128] [i128 u0x10000000000000001, i128 u0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF]")
	assertContains(t, ir, "[1 x i128] [i128 -170141183460469231731687303715884105728]")
}

// TestTryConstantExprElemTypeMismatch covers the defensive `elemLLVM` fallbacks:
// an int literal folded against a non-integer element type defaults to i64, and
// a float literal against a non-float element type defaults to double. Not
// reachable from well-typed source (sema checks each element against the
// declared element type), so drive the fold directly.
func TestTryConstantExprElemTypeMismatch(t *testing.T) {
	c := &Compiler{}
	notAnInt := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)

	cv := c.tryConstantExpr(&ast.IntLit{Raw: "42"}, nil, notAnInt)
	iv, ok := cv.(*constant.Int)
	if !ok {
		t.Fatalf("int literal with struct elem type = %T, want *constant.Int", cv)
	}
	if iv.Typ != irtypes.I64 || iv.X.String() != "42" {
		t.Errorf("int fallback = %v %s, want i64 42", iv.Typ, iv.X)
	}

	fv, ok := c.tryConstantExpr(&ast.FloatLit{Raw: "1.5"}, nil, irtypes.I64).(*constant.Float)
	if !ok {
		t.Fatalf("float literal with int elem type did not fold to *constant.Float")
	}
	if got, _ := fv.X.Float64(); fv.Typ != irtypes.Double || got != 1.5 {
		t.Errorf("float fallback = %v %v, want double 1.5", fv.Typ, got)
	}
}

// TestTryConstantExprUnparseableIntFallsBackToHeap pins the T1418 failure mode
// at the fold level: an integer literal the parser cannot read must abandon the
// static path (nil) rather than fold to a silently wrong constant, both on its
// own and as the operand of a unary minus. The old code discarded the
// strconv.ParseUint error and folded such a literal to MaxUint64 → -1.
func TestTryConstantExprUnparseableIntFallsBackToHeap(t *testing.T) {
	c := &Compiler{}
	bad := &ast.IntLit{Raw: "0x"}
	if cv := c.tryConstantExpr(bad, nil, irtypes.NewInt(128)); cv != nil {
		t.Errorf("unparseable int folded to %v, want nil", cv)
	}
	neg := &ast.UnaryExpr{Op: ast.UnaryNeg, Operand: bad}
	if cv := c.tryConstantExpr(neg, nil, irtypes.NewInt(128)); cv != nil {
		t.Errorf("negated unparseable int folded to %v, want nil", cv)
	}
}

// TestTryConstantExprNegateWide pins the negation arm at full precision. The
// pre-fix `-v.X.Int64()` truncated the magnitude first, so -i128.min folded to
// +1 and any operand above int64.max negated to garbage.
func TestTryConstantExprNegateWide(t *testing.T) {
	c := &Compiler{}
	i128 := irtypes.NewInt(128)
	tests := []struct{ raw, want string }{
		{"170141183460469231731687303715884105728", "-170141183460469231731687303715884105728"}, // i128.min
		{"18446744073709551617", "-18446744073709551617"},                                       // 2^64+1
		{"9223372036854775808", "-9223372036854775808"},                                         // 2^63
		{"5", "-5"},
	}
	for _, tt := range tests {
		neg := &ast.UnaryExpr{Op: ast.UnaryNeg, Operand: &ast.IntLit{Raw: tt.raw}}
		cv, ok := c.tryConstantExpr(neg, nil, i128).(*constant.Int)
		if !ok {
			t.Fatalf("negate %s did not fold to *constant.Int", tt.raw)
		}
		if got := cv.X.String(); got != tt.want {
			t.Errorf("negate %s = %s, want %s", tt.raw, got, tt.want)
		}
		if cv.Typ != i128 {
			t.Errorf("negate %s type = %v, want i128", tt.raw, cv.Typ)
		}
	}

	// The float arm of the same switch.
	fv, ok := c.tryConstantExpr(
		&ast.UnaryExpr{Op: ast.UnaryNeg, Operand: &ast.FloatLit{Raw: "2.5"}},
		nil, irtypes.Double,
	).(*constant.Float)
	if !ok {
		t.Fatalf("negated float did not fold to *constant.Float")
	}
	if got, _ := fv.X.Float64(); got != -2.5 {
		t.Errorf("negate 2.5 = %v, want -2.5", got)
	}
}

// TestGenLitUnparseableFallback covers the scalar path's counterpart to the
// heap-path fallback above: genIntLit emits 0 for a literal it cannot parse
// instead of a wrong value, and genFloatLit emits 0.0. Both are unreachable
// post-sema, and both are the reason the shared fold reports failure rather
// than returning a bogus constant.
func TestGenLitUnparseableFallback(t *testing.T) {
	c := &Compiler{info: &sema.Info{Types: map[ast.Expr]types.Type{}}}

	iv, ok := c.genIntLit(&ast.IntLit{Raw: "0b"}).(*constant.Int)
	if !ok {
		t.Fatalf("genIntLit did not return *constant.Int")
	}
	if iv.Typ != irtypes.I64 || iv.X.Sign() != 0 {
		t.Errorf("genIntLit(unparseable) = %v %s, want i64 0", iv.Typ, iv.X)
	}

	fv, ok := c.genFloatLit(&ast.FloatLit{Raw: "not-a-float"}).(*constant.Float)
	if !ok {
		t.Fatalf("genFloatLit did not return *constant.Float")
	}
	if got, _ := fv.X.Float64(); fv.Typ != irtypes.Double || got != 0 {
		t.Errorf("genFloatLit(unparseable) = %v %v, want double 0", fv.Typ, got)
	}
}
