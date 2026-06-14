package codegen

import (
	"strings"
	"testing"
)

// T0908: The drop-old-value branches in genMemberAssign (T0410) and
// genVectorIndexAssign (T0398) previously freed the previous field/element only
// when the type was isDroppableHeapUserType (has an explicit/synthesized drop).
// Heap user types with NO drop method (isHeapUserNoDropPalFree) are still heap-
// allocated and need pal_free, but were skipped — so overwriting such a field/
// element leaked the old instance. The gates were widened to also cover the
// no-drop case, reusing emitVariantFieldDrop's B0218 pal_free path.

// Overwriting a no-drop heap user-type field emits the drop-old block
// (field.userdrop) and a pal_free of the old instance.
func TestAssignFieldNoDropHeapUserEmitsDropOldPalFree(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; }
		type Holder { BB f; }
		main() { h := Holder(f: BB(v: 0)); h.f = BB(v: 5); }
	`)
	mainFn := extractDefine(ir, ".goroutine.main")
	assertContains(t, mainFn, "field.userdrop")
	assertContains(t, mainFn, "@pal_free")
}

// Overwriting a no-drop heap user-type vector element emits a pal_free of the
// old element (via emitVariantFieldDrop's B0218 varfield.free branch).
func TestAssignIndexNoDropHeapUserEmitsDropOldPalFree(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; }
		main() { v := [BB(v: 0)]; v[0] = BB(v: 5); }
	`)
	mainFn := extractDefine(ir, ".goroutine.main")
	assertContains(t, mainFn, "varfield.free")
	assertContains(t, mainFn, "@pal_free")
}

// A no-drop heap user-type vector slot-to-slot assign (`v[0] = v[1]`) must
// dup-on-read the source element (heapdup.copy) so the destination owns an
// independent instance — without it the drop-old / element-drop walk would
// free the same instance twice. Mirrors genArrayIndex's T0590 no-drop branch.
func TestAssignIndexNoDropHeapUserSlotToSlotDupsSource(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; }
		main() { v := [BB(v: 1), BB(v: 2)]; v[0] = v[1]; }
	`)
	mainFn := extractDefine(ir, ".goroutine.main")
	assertContains(t, mainFn, "heapdup.copy")
}

// Negative control: a value-type field (no heap allocation) must NOT emit a
// drop-old block on overwrite — there is nothing to free.
func TestAssignFieldValueTypeNoDropOldBlock(t *testing.T) {
	ir := generateIR(t, `
		type VV { int v `+"`value"+`; }
		type Holder { VV f; }
		main() { h := Holder(f: VV(v: 0)); h.f = VV(v: 5); }
	`)
	mainFn := extractDefine(ir, ".goroutine.main")
	if strings.Contains(mainFn, "field.userdrop") {
		t.Fatalf("value-type field overwrite must NOT emit field.userdrop:\n%s", mainFn)
	}
}
