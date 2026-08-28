package regress4

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T0765: An enum variant holding a structural-interface field must dispatch
// drop through __promise_structural_drop in the synthesized enum drop body.
// Before the fix, the per-variant gate (variantFieldNeedsDrop) excluded
// structural types and emitVariantFieldDrop had no structural branch, so the
// underlying allocation leaked. Enum-variant analog of T0460.
func TestT0765EnumVariantStructuralFieldDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type W `+"`"+`structural {
			write!(u8[] ~buf) int `+"`"+`abstract;
		}
		type ConcreteW {
			int n;
			write!(u8[] ~buf) int { return this.n; }
		}
		enum StructuralHolder {
			WithWriter(W w),
			Empty,
		}
		main() {
			StructuralHolder h = StructuralHolder.WithWriter(ConcreteW(n: 0));
		}
	`)
	fn := codegentest.ExtractFunction(ir, "StructuralHolder.drop")
	if fn == "" {
		t.Fatal("expected synthesized StructuralHolder.drop to be emitted")
	}
	codegentest.AssertContains(t, fn, "__promise_structural_drop")
}
