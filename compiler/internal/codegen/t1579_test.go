package codegen

import (
	"strings"
	"testing"
)

// T1579: the repeat array literal `[value; count]` is pure sugar over the
// N-element fixed-array literal — same stack alloca, same per-slot stores, no
// new allocation path.

func TestT1579RepeatLiteralShape(t *testing.T) {
	ir := generateIR(t, `main() { u32[4] w = [9u32; 4]; }`)
	// Stack-allocated [4 x i32], one store per slot with the single value.
	assertContains(t, ir, "alloca [4 x i32]")
	assertContains(t, ir, "getelementptr [4 x i32]")
	if n := strings.Count(ir, "store i32 9,"); n != 4 {
		t.Fatalf("expected 4 stores of the repeated value, got %d\n%s", n, ir)
	}
}

// The repeat literal expands to exactly the same array-store IR as the
// equivalent hand-written N-element literal (comparing the store shape).
func TestT1579RepeatMatchesHandwritten(t *testing.T) {
	repeat := extractArrayStores(generateIR(t, `main() { u32[4] w = [9u32; 4]; }`))
	manual := extractArrayStores(generateIR(t, `main() { u32[4] w = [9u32, 9u32, 9u32, 9u32]; }`))
	if repeat != manual {
		t.Fatalf("repeat literal IR shape differs from hand-written literal:\nrepeat:\n%s\nmanual:\n%s", repeat, manual)
	}
}

// A repeat literal with a non-`copy` element type is a sema error, so codegen is
// never reached — no need for a codegen negative here.

// An Optional element hint (Some-wrapping) must wrap the once-evaluated value
// into the {i1,T} element struct before storing — sema widens `u32 -> u32?`, so
// storing the raw i32 into an optional slot would produce malformed IR. Guards
// against the codegen panic regression fixed in T1579.
func TestT1579RepeatOptionalElement(t *testing.T) {
	ir := generateIR(t, `main() { u32?[4] w = [0u32; 4]; }`)
	assertContains(t, ir, "alloca [4 x { i1, i32 }]")
	// The stored value is the wrapped optional struct, not a bare i32.
	assertContains(t, ir, "store { i1, i32 }")
}

// Nested repeat literals produce nested fixed arrays.
func TestT1579NestedRepeat(t *testing.T) {
	ir := generateIR(t, `main() { u8[4][3] grid = [[1u8; 4]; 3]; }`)
	assertContains(t, ir, "alloca [3 x [4 x i8]]")
	assertContains(t, ir, "alloca [4 x i8]")
}

// Inside a monomorphized generic function, c.typeSubst is non-nil while the
// repeat literal is generated. A concrete-element repeat must still lower to the
// same [N x T] store loop — exercises the typeSubst-substitution branches.
func TestT1579RepeatInGenericContext(t *testing.T) {
	ir := generateIR(t, `
		gbox[T]() u32[4] { return [7u32; 4]; }
		main() { b := gbox[int](); }
	`)
	assertContains(t, ir, "alloca [4 x i32]")
	if n := strings.Count(ir, "store i32 7,"); n != 4 {
		t.Fatalf("expected 4 stores in the monomorphized body, got %d\n%s", n, ir)
	}
}

// extractArrayStores returns the [N x T] GEP/store lines so two IR outputs can
// be compared on array-construction shape alone (ignoring alloca temp numbers).
func extractArrayStores(ir string) string {
	var b strings.Builder
	for _, line := range strings.Split(ir, "\n") {
		l := strings.TrimSpace(line)
		if strings.Contains(l, "[4 x i32]") || strings.HasPrefix(l, "store i32 9,") {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	return b.String()
}
