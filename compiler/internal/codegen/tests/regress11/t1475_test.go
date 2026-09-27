package regress11

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1475: a vector literal whose element type is a structural interface must not take
// the all-constant .rodata path — its elements have to be BOXED, and only the heap
// path can box. tryConstantExpr used to substitute I64/Double when the element slot's
// LLVM type was not an integer/float (and never checked it at all for bool/char), so
// the folded constants were written into a `[N x { i8*, i8* }]` initializer and `opt`
// rejected the module with `constant expression type mismatch`.

func TestT1475_ConstantVectorLiteralWithBoxedElementTakesHeapPath(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Format[] v = [1, 2];
		}
	`)
	// No .rodata vector global for this literal: every array global in the IR must
	// hold scalars, never the view pair.
	if strings.Contains(ir, `[2 x { i8*, i8* }] [i64`) {
		t.Fatalf("vector literal with a structural element type folded to a .rodata global:\n%s",
			codegentest.ExtractFunction(ir, "__user.main"))
	}
	// The heap path boxes each element: a per-concrete flat header goes into field 0.
	if !strings.Contains(ir, `@promise_typeinfo_box$int$flat`) {
		t.Fatalf("expected each constant element to be boxed on the heap path")
	}
}

func TestT1475_NonConstantVectorLiteralWithBoxedElementStillBoxes(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int q = 1;
			Format[] v = [q];
		}
	`)
	if !strings.Contains(ir, `@promise_typeinfo_box$int$flat`) {
		t.Fatalf("expected the non-constant element to be boxed")
	}
}

// An ordinary scalar element type must still fold to .rodata — the guard rejects a
// mismatched kind, it does not disable constant folding.
func TestT1475_ScalarVectorLiteralStillFoldsToRodata(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int[] v = [1, 2, 3];
		}
	`)
	if !strings.Contains(ir, `[3 x i64] [i64 1, i64 2, i64 3]`) {
		t.Fatalf("a plain int vector literal must still take the static .rodata path")
	}
}

// bool and char literals never consulted the element slot's LLVM type at all, so they
// are the arms most likely to regress: they must fold for their own element type and
// bail for a boxed one.
func TestT1475_BoolAndCharLiteralsRespectTheElementKind(t *testing.T) {
	scalars := codegentest.GenerateIR(t, `
		main() {
			bool[] b = [true, false];
			char[] c = ['a', 'b'];
		}
	`)
	if !strings.Contains(scalars, `[2 x i1] [i1 true, i1 false]`) {
		t.Fatalf("a bool vector literal must still fold to .rodata")
	}
	if !strings.Contains(scalars, `[2 x i32] [i32 97, i32 98]`) {
		t.Fatalf("a char vector literal must still fold to .rodata")
	}

	boxed := codegentest.GenerateIR(t, `
		main() {
			Format[] b = [true, false];
		}
	`)
	if strings.Contains(boxed, `[2 x { i8*, i8* }] [i1`) {
		t.Fatalf("bool literals folded into a boxed element slot:\n%s",
			codegentest.ExtractFunction(boxed, "__user.main"))
	}
	if !strings.Contains(boxed, `@promise_typeinfo_box$bool$flat`) {
		t.Fatalf("expected boxed bool elements on the heap path")
	}
}
