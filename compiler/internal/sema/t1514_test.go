package sema

import "testing"

// T1514/T1985/T1999: codegen's isRawGeneratorResult (codegen/generator.go) is a
// pure TYPE test — `types.AsStream(t)` — and four codegen sites now rest on it to
// decide that a CallExpr's `stream[T]` result is a raw coroutine `{handle, slot}`
// pair rather than a `{vtable, instance}` structural view. That is only sound
// because sema refuses to produce a Stream[T]-typed call result any other way.
//
// `stream[T]` and `Stream[T]` are the SAME type (the lowercase spelling is an
// alias installed by sema), so the guarantee has to hold for BOTH spellings.
// TestGeneratorNoYield / TestGeneratorMethodNoYield in sema_test.go pin only the
// lowercase one; a `Stream[T]`-spelled function that slipped through would give
// codegen a structural view whose field 0 is a vtable pointer, and
// dropDiscardedGenerator would hand that to __promise_gen_destroy.

func TestT1514CapitalizedStreamFunctionNoYield(t *testing.T) {
	errs := checkErrs(t, `
		view_of(int[] v) Stream[int] {
			return v;
		}
		main() {}
	`)
	expectError(t, errs, "returns stream[int] but contains no yield statements")
}

func TestT1514CapitalizedStreamMethodNoYield(t *testing.T) {
	errs := checkErrs(t, `
		type Holder {
			int[] v;
			view(this) Stream[int] {
				return this.v;
			}
		}
		main() {}
	`)
	expectError(t, errs, "returns stream[int] but contains no yield statements")
}

// The other half of the premise: a Stream[T] value that IS a structural view can
// only ever be reached through a non-call expression, because sema routes every
// CallExpr down the coroutine path (isStructuralStreamView). This is what makes
// isGeneratorFactoryCallExpr's syntactic call test a sufficient discriminator.
func TestT1514StructuralStreamViewIsNotACall(t *testing.T) {
	checkOK(t, `
		main() {
			int[] v = [1, 2, 3];
			Stream[int] s = v;
			int sum = 0;
			for x in s { sum = sum + x; }
		}
	`)
}
