// Tests lifted back out of tests/misc2 (T1776): they reach into codegen's
// unexported internals, so they have to be compiled as part of package
// codegen. They use the private helper copies in codegen_helpers_test.go.
package codegen

import (
	"testing"

	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

func TestConstBool(t *testing.T) {
	if got := constBool(true); got.X.Int64() != 1 || got.Typ != irtypes.I1 {
		t.Errorf("constBool(true) = %v %v, want i1 1", got.Typ, got.X)
	}
	if got := constBool(false); got.X.Int64() != 0 || got.Typ != irtypes.I1 {
		t.Errorf("constBool(false) = %v %v, want i1 0", got.Typ, got.X)
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
