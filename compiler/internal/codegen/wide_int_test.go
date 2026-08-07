package codegen

import (
	"strings"
	"testing"
)

// Wide integer codegen: the native operator dispatcher emits width-polymorphic
// LLVM instructions on iN, so no per-width instruction code is needed — these
// tests lock in the emitted IR shape for the new widths.

func TestWideIntU128Arithmetic(t *testing.T) {
	ir := generateIR(t, `main() {
		u128 a = 100u128;
		u128 b = a + a;
		u128 q = b / a;
		u128 r = b % a;
	}`)
	assertContains(t, ir, "i128")
	assertContains(t, ir, "add i128")
	assertContains(t, ir, "udiv i128")
	assertContains(t, ir, "urem i128")
}

func TestWideIntI128SignedDiv(t *testing.T) {
	// Signed division must emit sdiv/srem (not udiv/urem).
	ir := generateIR(t, `main() {
		i128 a = -100i128;
		i128 b = 7i128;
		i128 q = a / b;
		i128 r = a % b;
	}`)
	assertContains(t, ir, "sdiv i128")
	assertContains(t, ir, "srem i128")
}

func TestWideIntI256AndI512Types(t *testing.T) {
	ir := generateIR(t, `main() {
		i256 a = 1i256;
		u256 b = 2u256;
		i512 c = 3i512;
		u512 d = 4u512;
		u256 e = b * b;
		u512 f = d + d;
	}`)
	assertContains(t, ir, "i256")
	assertContains(t, ir, "i512")
	assertContains(t, ir, "mul i256")
	assertContains(t, ir, "add i512")
}

func TestWideIntLargeConstant(t *testing.T) {
	// A >64-bit literal must be emitted losslessly (big.Int constant path),
	// not truncated to 64 bits.
	ir := generateIR(t, `main() {
		u128 max = 340282366920938463463374607431768211455u128;
	}`)
	// 2^128 - 1 in hex is 32 F's; llir renders large positive constants as u0x...
	assertContains(t, ir, "u0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF")
}

func TestWideIntCasts(t *testing.T) {
	ir := generateIR(t, `main() {
		i128 a = 42i128;
		i64 narrow = a as! i64;
		i256 wide = a as! i256;
		f64 f = a as! f64;
	}`)
	assertContains(t, ir, "trunc i128")  // 128 -> 64
	assertContains(t, ir, "sext i128")   // 128 -> 256 (signed)
	assertContains(t, ir, "sitofp i128") // 128 -> f64 (signed)
}

func TestWideIntDivBuiltins(t *testing.T) {
	// The 128-bit div/rem libcalls that i128/u128 `/` and `%` lower to must be
	// emitted in-IR on EVERY target (T0587, T1399). wasm32 has no compiler-rt,
	// and the default linux target statically links musl with no compiler-rt or
	// libgcc — so a missing definition is an undefined-symbol link error, not a
	// resolved external. The main-IR definitions use external linkage; on the
	// glibc dynamic path the strong definition simply satisfies the reference
	// before -lgcc's archive member is consulted, so there is no link conflict.
	for _, target := range []string{"wasm32-wasi", ""} {
		var ir string
		if target == "" {
			ir = generateIR(t, `main() {
				u128 a = 100u128;
				u128 b = 7u128;
				u128 q = a / b;
				u128 r = a % b;
			}`)
		} else {
			ir = generateIRForTarget(t, `main() {
				u128 a = 100u128;
				u128 b = 7u128;
				u128 q = a / b;
				u128 r = a % b;
			}`, target)
		}
		for _, sym := range []string{"__udivti3", "__umodti3", "__divti3", "__modti3", "__promise_udivmod128"} {
			if !strings.Contains(ir, "@"+sym+"(") {
				t.Errorf("target %q: IR must define %s:\n%s", target, sym, ir)
			}
		}
		// The helper must use only constant-amount i128 shifts (no variable-shift
		// libcalls __lshrti3/__ashlti3, which wasm also lacks).
		if strings.Contains(ir, "__lshrti3") || strings.Contains(ir, "__ashlti3") {
			t.Errorf("target %q: div helper must not require variable-shift libcalls:\n%s", target, ir)
		}
	}
}

func TestWideIntHashFold(t *testing.T) {
	// The native hash getter folds the wide value to i64 by XOR-ing 64-bit limbs.
	ir := generateIR(t, `main() {
		u128 a = 5u128;
		int h = a.hash;
	}`)
	assertContains(t, ir, "trunc i128")
	assertContains(t, ir, "lshr i128")
	assertContains(t, ir, "xor i64")
}
