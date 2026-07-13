package codegen

import "testing"

// T1263: two structural siblings of T1262 that SEGV through DIFFERENT codegen paths
// than the aliasing-container `[]` fixed there:
//   1. native container element read (`w := vv[0]` on Vector[Vector[() -> int]]) →
//      genVectorIndex → dupVector + emitVectorElementCloneLoop zeroes each closure env.
//   2. struct-field direct read (`f := h.fns` on H { (() -> int)[] fns; }) →
//      genFieldAccess → dupHeapFieldForEscape → same env-zeroing dupVector path.
// The fix guards both dup-decision points with FirstFieldNestedClosureDeep so the read
// stays ALIASED (a borrow, envs intact); the borrow gates suppress the owning drop
// binding and reject escapes.

// Native vector index of a value-copying container of closures: the probe body must
// NOT emit dupVector's env-zeroing element-clone loop (vecclonenull).
func TestT1263_NativeVecOfVecClosureReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		probe() {
			x := 7;
			vv := Vector[Vector[() -> int]]();
			vv.push([|| -> x]);
			w := vv[0];
			y := w[0]();
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("probe not found in IR")
	}
	// A deep-copy of the inner vector would zero each closure element's env slot.
	assertNotContains(t, fn, "vecclonenull")
}

// Struct field direct read of a value-copying container of closures: same — the
// probe body must NOT emit the env-zeroing element-clone loop.
func TestT1263_StructFieldVecClosureReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		type H { (() -> int)[] fns; }
		probe() {
			x := 7;
			h := H(fns: [|| -> x]);
			f := h.fns;
			y := f[0]();
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("probe not found in IR")
	}
	assertNotContains(t, fn, "vecclonenull")
}

// Control: a native vector index of a NON-closure value-copying container
// (`Vector[int[]]`) stays dup-safe — the read must still deep-copy the inner vector
// (vecdup.copy). Guards the fix against over-suppressing the dup, which would alias
// the container's stored buffer and break mutation isolation.
func TestT1263_NativeVecOfIntVecReadDups(t *testing.T) {
	ir := generateIR(t, `
		probe() {
			vv := Vector[int[]]();
			vv.push([1, 2, 3]);
			w := vv[0];
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("probe not found in IR")
	}
	assertContains(t, fn, "vecdup.copy")
}

// Control: a struct field of a NON-closure value-copying container (`int[]`) stays
// dup-safe — the field read still deep-copies (vecdup.copy).
func TestT1263_StructFieldIntVecReadDups(t *testing.T) {
	ir := generateIR(t, `
		type IB { int[] xs; }
		probe() {
			h := IB(xs: [1, 2, 3]);
			f := h.xs;
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("probe not found in IR")
	}
	assertContains(t, fn, "vecdup.copy")
}
