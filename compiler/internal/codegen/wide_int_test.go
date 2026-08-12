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

func TestWideIntDivBuiltinsWin64ABI(t *testing.T) {
	// x86-64 Windows lowers 128-bit div/rem libcalls with operands passed
	// indirectly by pointer and the result returned as <2 x i64> in xmm0. A
	// by-value `i128` definition silently reads the POINTERS as operands and
	// returns into registers the caller never reads — every non-constant-divisor
	// i128/u128 `/` and `%` produced garbage (T1414). ARM64 Windows and every
	// other target pass i128 by value, so they keep the plain signature.
	src := `main() {
		u128 a = 100u128;
		u128 b = 7u128;
		u128 q = a / b;
		u128 r = a % b;
	}`
	syms := []string{"__udivti3", "__umodti3", "__divti3", "__modti3"}

	winIR := generateIRForTarget(t, src, "x86_64-pc-windows-msvc")
	for _, sym := range syms {
		assertContains(t, winIR, "define <2 x i64> @"+sym+"(i128* %pa, i128* %pb)")
		// The signature alone is not enough: a wrapper that returns <2 x i64>
		// but never dereferences its pointer operands would still match. Pin the
		// body — both operands loaded, result bitcast back out through xmm0.
		body := extractFunction(winIR, sym)
		if body == "" {
			t.Fatalf("%s: no body extracted from win64 IR:\n%s", sym, winIR)
		}
		for _, want := range []string{
			"load i128, i128* %pa",
			"load i128, i128* %pb",
			"bitcast i128 %",
			"to <2 x i64>",
			"ret <2 x i64> %",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("win64 %s body must contain %q:\n%s", sym, want, body)
			}
		}
		// The by-value shape must be gone entirely — a function is defined once,
		// so a leftover `define i128 @sym` means the gate did not apply.
		if strings.Contains(winIR, "define i128 @"+sym+"(") {
			t.Errorf("win64 %s must not also be defined with the by-value i128 ABI", sym)
		}
	}

	// Sign correction still has to happen inside the indirect wrappers — the
	// signed pair is the easiest thing to lose when refactoring the shared emit
	// closure, and `__divti3`/`__modti3` differ only in which sign they apply.
	if body := extractFunction(winIR, "__divti3"); !strings.Contains(body, "ashr i128 %0, 127") ||
		!strings.Contains(body, "ashr i128 %1, 127") {
		t.Errorf("win64 __divti3 must derive both operand signs:\n%s", body)
	}
	if body := extractFunction(winIR, "__modti3"); !strings.Contains(body, "ashr i128 %0, 127") {
		t.Errorf("win64 __modti3 must derive the dividend sign:\n%s", body)
	}

	// Only the four PUBLIC wrappers change shape. The internal helper is called
	// directly from the wrappers (not through the backend's libcall lowering),
	// so it must keep the by-value signature on every target — including win64.
	assertContains(t, winIR, "define { i128, i128 } @__promise_udivmod128(i128 %a, i128 %b)")

	// Every other target passes i128 by value in register pairs and returns it
	// the same way, so the plain signature is the correct one there. macOS
	// x86_64 discriminates an arch-only gate; aarch64 Windows an OS-only one.
	for _, target := range []string{
		"x86_64-unknown-linux-musl",
		"x86_64-unknown-linux-gnu",
		"x86_64-apple-macosx14.0.0",
		"aarch64-apple-macosx14.0.0",
		"aarch64-unknown-linux-gnu",
		"aarch64-pc-windows-msvc",
		"wasm32-wasi",
		"wasm32-web",
	} {
		ir := generateIRForTarget(t, src, target)
		for _, sym := range syms {
			assertContains(t, ir, "define i128 @"+sym+"(i128 %a, i128 %b)")
			if strings.Contains(ir, "define <2 x i64> @"+sym+"(") {
				t.Errorf("target %q: %s must use the by-value i128 ABI", target, sym)
			}
		}
		assertContains(t, ir, "define { i128, i128 } @__promise_udivmod128(i128 %a, i128 %b)")
	}
}

func TestWin64IndirectI128LibcallGate(t *testing.T) {
	// The ABI switch is a pure function of the target triple, so pin it directly
	// rather than only through emitted IR — the discriminating cases are "windows
	// but not x86_64" and "x86_64 but not windows", which an OS-only or arch-only
	// check would each get wrong in one direction (T1414).
	for _, tc := range []struct {
		target string
		want   bool
	}{
		{"x86_64-pc-windows-msvc", true},
		{"x86_64-pc-windows-gnu", true},
		{"aarch64-pc-windows-msvc", false},
		{"arm64-pc-windows-msvc", false},
		{"x86_64-unknown-linux-musl", false},
		{"x86_64-unknown-linux-gnu", false},
		{"x86_64-apple-macosx14.0.0", false},
		{"x86_64-apple-darwin", false},
		{"aarch64-apple-macosx14.0.0", false},
		{"aarch64-unknown-linux-gnu", false},
		{"wasm32-wasi", false},
		{"wasm32-web", false},
		// An empty triple never reaches codegen — compile() substitutes
		// HostTargetTriple() before constructing the Compiler — so the
		// conservative by-value answer here is unreachable, not a Windows hole.
		{"", false},
	} {
		c := &Compiler{target: tc.target}
		if got := c.win64IndirectI128Libcall(); got != tc.want {
			t.Errorf("win64IndirectI128Libcall(%q) = %v, want %v", tc.target, got, tc.want)
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

func TestWideIntStaticVectorLiteral(t *testing.T) {
	// T1418: an all-constant vector literal takes the T0062 static .rodata path,
	// which folded each element through strconv's 64-bit parsers and then
	// sign-extended the int64 into the wide element type. Every element above
	// int64.max was silently wrong: 2^63..2^64-1 gained all-ones high bits, and
	// >= 2^64 became -1 (the discarded ParseUint range error yields MaxUint64).
	ir := generateIR(t, `main() {
		u128[] a = [9223372036854775808u128];
		u128[] b = [18446744073709551615u128];
		i128[] c = [170141183460469231731687303715884105727i128];
		i128[] d = [-170141183460469231731687303715884105728i128];
		i256[] e = [18446744073709551617i256];
	}`)
	// llir renders wide constants that exceed int64 as unsigned hex.
	assertContains(t, ir, "[1 x i128] [i128 u0x8000000000000000]")
	assertContains(t, ir, "[1 x i128] [i128 u0xFFFFFFFFFFFFFFFF]")
	assertContains(t, ir, "[1 x i128] [i128 u0x7FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF]")
	assertContains(t, ir, "[1 x i128] [i128 -170141183460469231731687303715884105728]")
	assertContains(t, ir, "[1 x i256] [i256 u0x10000000000000001]")
	// The pre-fix folds: sign-extended int64 and the MaxUint64/-1 clamp.
	assertNotContains(t, ir, "[i128 -1]")
	assertNotContains(t, ir, "[i256 -1]")
	assertNotContains(t, ir, "[i128 1]")
	assertNotContains(t, ir, "[i128 -9223372036854775808]")
}

func TestWideIntStaticVectorLiteralNarrowUnchanged(t *testing.T) {
	// The full-precision parse must not disturb the narrow element types that
	// already worked (int/i8/u64/char/bool/f64 vector literals).
	ir := generateIR(t, `main() {
		int[] a = [1, -2, 3];
		i8[] b = [-128i8, 127i8];
		u64[] c = [18446744073709551615u64];
		char[] d = ['a', '\n'];
		bool[] e = [true, false];
		f64[] f = [1.5, -2.5];
	}`)
	assertContains(t, ir, "[3 x i64] [i64 1, i64 -2, i64 3]")
	assertContains(t, ir, "[2 x i8] [i8 -128, i8 127]")
	// u64.max no longer folds through int64: it renders as unsigned hex, the
	// same bit pattern (and the same rendering) the scalar genIntLit path emits.
	assertContains(t, ir, "[1 x i64] [i64 u0xFFFFFFFFFFFFFFFF]")
	assertContains(t, ir, "[2 x i32] [i32 97, i32 10]")
	assertContains(t, ir, "[2 x i1] [i1 true, i1 false]")
	assertContains(t, ir, "[2 x double] [double 1.5, double -2.5]")
}
