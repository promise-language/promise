package regress3

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T2219: the synthesized clone of a generic child duplicated an INHERITED `T[]`
// buffer with the element size of an unbound type parameter — a pointer — because
// dupHeapValueFields substituted AllFields() with a child-only map, leaving the
// parent's T unresolved (the T1970 rule, missed on the codegen clone walk). The
// buffer copy must be sized by the bound element type.

const t2219Src = `
	type BaseV[T] { T[] items; }
	type DerV[T] is BaseV[T] { int n; }
	type WideDer is BaseV[(int, int)] { int n; }
	probe() {
		Vector[DerV[int]] vi = [];
		vi.push(DerV[int](items: [1, 2, 3], n: 7));
		di := vi[0];
		Vector[DerV[(int, int)]] vt = [];
		vt.push(DerV[(int, int)](items: [(1, 10)], n: 7));
		dt := vt[0];
		Vector[WideDer] vw = [];
		vw.push(WideDer(items: [(1, 10)], n: 7));
		dw := vw[0];
	}
	main() { probe(); }
`

// On wasm32 a pointer is 4 bytes and int is 8: the clone of DerV[int] must size
// the inherited buffer at 8 bytes per element.
func TestT2219_InheritedBufferCloneElementSizeWasm(t *testing.T) {
	ir := codegentest.GenerateIRForTarget(t, t2219Src, "wasm32-wasi")
	fn := codegentest.ExtractFunc(ir, `"DerV[int].__clone"`)
	if fn == "" {
		t.Fatalf("DerV[int].__clone not found in IR")
	}
	codegentest.AssertContainsMatch(t, fn, `mul i64 %\d+, 8\n`)
	codegentest.AssertNotContainsMatch(t, fn, `mul i64 %\d+, 4\n`)
}

// On a 64-bit host a (int, int) element is 16 bytes, twice the pointer size.
func TestT2219_InheritedBufferCloneElementSizeWide(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2219Src)
	fn := codegentest.ExtractFunc(ir, `"DerV[(int, int)].__clone"`)
	if fn == "" {
		t.Fatalf("DerV[(int, int)].__clone not found in IR")
	}
	codegentest.AssertContainsMatch(t, fn, `mul i64 %\d+, 16\n`)
	codegentest.AssertNotContainsMatch(t, fn, `mul i64 %\d+, 8\n`)
}

// A non-generic child of a generic parent resolves to a plain Named, not an
// Instance: the parent binding must still size the inherited buffer.
func TestT2219_NonGenericChildInheritedBufferCloneElementSize(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2219Src)
	fn := codegentest.ExtractFunc(ir, `WideDer.__clone`)
	if fn == "" {
		t.Fatalf("WideDer.__clone not found in IR")
	}
	codegentest.AssertContainsMatch(t, fn, `mul i64 %\d+, 16\n`)
	codegentest.AssertNotContainsMatch(t, fn, `mul i64 %\d+, 8\n`)
}
