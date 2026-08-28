package misc2

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// Direct unit tests for the literal → LLVM constant folds (T1418). These are the
// single source of truth shared by the scalar path (genIntLit/genFloatLit/…) and
// the static .rodata vector-literal fold (tryConstantExpr), so they are worth
// pinning at the function level rather than only through emitted IR — the bug
// was precisely that the two copies disagreed about a literal's value.

// TestStaticVectorLiteralMatchesScalar locks in the invariant the T1418 refactor
// exists to guarantee: the scalar literal path and the static .rodata vector
// fold produce the same constant for the same source text. Before the fix the
// scalar store held the correct full-precision value while the array initializer
// next to it held a sign-extended one.
func TestStaticVectorLiteralMatchesScalar(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() {
		u128 s = 18446744073709551617u128;
		u128[] v = [18446744073709551617u128];
	}`)
	codegentest.AssertContains(t, ir, "store i128 u0x10000000000000001")
	codegentest.AssertContains(t, ir, "[1 x i128] [i128 u0x10000000000000001]")
}

// TestStaticVectorLiteralRadixAndUnderscores covers the base-0 prefix handling
// in the shared int fold for narrow element types — tryConstantExpr's old
// strconv.ParseInt(raw, 0, 64) accepted the same prefixes, so this pins that
// behavior across the swap to big.Int.
func TestStaticVectorLiteralRadixAndUnderscores(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() {
		int[] v = [0xff, 0o17, 0b1010, 1_000_000];
	}`)
	codegentest.AssertContains(t, ir, "[4 x i64] [i64 255, i64 15, i64 10, i64 1000000]")
}

// TestStaticVectorLiteralCharEscapes pins the char fold through the vector path.
// tryConstantExpr used to carry its own copy of the escape table; it now shares
// constCharFromRaw with genCharLit, so a regression would hit both paths.
func TestStaticVectorLiteralCharEscapes(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() {
		char[] v = ['\n', '\r', '\t', '\b', '\\', '\'', '\0', 'é'];
	}`)
	codegentest.AssertContains(t, ir,
		"[8 x i32] [i32 10, i32 13, i32 9, i32 8, i32 92, i32 39, i32 0, i32 233]")
}

// TestStaticVectorLiteralF32 covers constFloatFromRaw's bitSize=32 branch
// through the vector fold: the element must be rounded to float32 precision, not
// stored as a truncated double.
func TestStaticVectorLiteralF32(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() {
		f32[] v = [0.1f32, -2.5f32];
	}`)
	// 0.1f32 == 0x3DCCCCCD; llir prints the float as its exact double widening.
	codegentest.AssertContains(t, ir, "[2 x float] [float 0x3FB99999A0000000, float -2.5]")
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
		ir := codegentest.GenerateIR(t, src)
		codegentest.AssertNotContains(t, ir, "@.arr.0 =")
		codegentest.AssertContains(t, ir, "@pal_alloc(")
	}
}

// TestTryConstantExprUnsuffixedWideElement covers the element form the suffixed
// tests miss: a literal with NO suffix whose type comes from the declared vector
// element type. Sema's validateIntRange only runs on suffixed literals, so
// nothing upstream would have caught a truncated fold here.
func TestTryConstantExprUnsuffixedWideElement(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() {
		u128[] a = [18446744073709551617, 340282366920938463463374607431768211455];
		i128[] b = [-170141183460469231731687303715884105728];
	}`)
	codegentest.AssertContains(t, ir,
		"[2 x i128] [i128 u0x10000000000000001, i128 u0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF]")
	codegentest.AssertContains(t, ir, "[1 x i128] [i128 -170141183460469231731687303715884105728]")
}
