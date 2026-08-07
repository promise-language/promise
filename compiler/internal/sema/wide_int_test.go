package sema

import "testing"

// Wide integer literal range checks (i128/u128/i256/u256/i512/u512).

func TestWideIntLiteralInRange(t *testing.T) {
	// Max values of each signed/unsigned wide type must type-check cleanly.
	src := `main() {
		u128 a = 340282366920938463463374607431768211455u128;
		i128 b = 170141183460469231731687303715884105727i128;
		u256 c = ~0u256;
		i256 d = 57896044618658097711785492504343953926634992332820282019728792003956564819967i256;
		u512 e = 0u512;
	}`
	if errs := checkErrs(t, src); len(errs) > 0 {
		t.Fatalf("unexpected errors for in-range wide literals: %v", errs)
	}
}

func TestWideIntLiteralOverflowU128(t *testing.T) {
	// 2^128 overflows u128 (max is 2^128 - 1).
	errs := checkErrs(t, `main() {
		u128 x = 340282366920938463463374607431768211456u128;
	}`)
	expectError(t, errs, "overflows u128")
}

func TestWideIntLiteralOverflowI128(t *testing.T) {
	// 2^127 overflows i128 positive max (2^127 - 1).
	errs := checkErrs(t, `main() {
		i128 x = 170141183460469231731687303715884105728i128;
	}`)
	expectError(t, errs, "overflows i128")
}

func TestWideIntSignedMinInRange(t *testing.T) {
	// i128.min = -(2^127) must be representable through the negated-literal path.
	src := `main() {
		i128 x = -170141183460469231731687303715884105728i128;
	}`
	if errs := checkErrs(t, src); len(errs) > 0 {
		t.Fatalf("unexpected errors for i128 min literal: %v", errs)
	}
}

func TestWideIntNegatedOverflow(t *testing.T) {
	// -(2^127 + 1) is one below i128.min (-2^127) → the negated-overflow branch.
	errs := checkErrs(t, `main() {
		i128 x = -170141183460469231731687303715884105729i128;
	}`)
	expectError(t, errs, "overflows i128 (min -170141183460469231731687303715884105728)")
}

func TestWideIntOverflow256And512(t *testing.T) {
	// Exercise the unsigned (u256) and signed (i512) overflow paths at the
	// widest ladder rungs to lock in the arbitrary-precision range check.
	// u256.max = 2^256 - 1; the literal below is 2^256.
	errs := checkErrs(t, `main() {
		u256 x = 115792089237316195423570985008687907853269984665640564039457584007913129639936u256;
	}`)
	expectError(t, errs, "overflows u256")

	// i512.max = 2^511 - 1; the literal below is 2^511 (one past the positive max).
	errs = checkErrs(t, `main() {
		i512 y = 6703903964971298549787012499102923063739682910296196688861780721860882015036773488400937149083451713845015929093243025426876941405973284973216824503042048i512;
	}`)
	expectError(t, errs, "overflows i512")
}

func TestWideIntArithmeticTypeChecks(t *testing.T) {
	// Full operator surface resolves for the wide types.
	src := `main() {
		u128 a = 10u128;
		u128 b = a + a * a - a / a % a;
		u128 c = (a & a) | (a ^ a);
		bool ok = (a << 2u128) >> 2u128 == a && a < b;
		i256 s = -5i256;
		i256 t = -s + 1i256;
		u512 w = ~0u512;
	}`
	if errs := checkErrs(t, src); len(errs) > 0 {
		t.Fatalf("unexpected errors for wide arithmetic: %v", errs)
	}
}
