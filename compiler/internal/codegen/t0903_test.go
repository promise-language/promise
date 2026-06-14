package codegen

import (
	"strings"
	"testing"
)

// T0903: Field/index assignment must drop the overwritten slot's old value for
// *no-drop* heap user types (a `type` with no `drop()`/synth-drop), not only for
// the drop-bearing types isDroppableHeapUserType admits. Without this,
// `h.field = X` / `vec[i] = X` for a plain heap user type leaks the old
// instance — the field/index analog of the var-LHS bindingFree/pal_free
// drop-old. The fix broadens the genMemberAssign (T0410) and
// genVectorIndexAssign (T0398) drop-old predicates to also admit
// isHeapUserNoDropPalFree, reusing emitVariantFieldDrop's B0218 null-guarded
// pal_free.

func mainBodyT0903(t *testing.T, ir string) string {
	t.Helper()
	defStart := strings.Index(ir, "define i8* @.goroutine.main()")
	if defStart < 0 {
		t.Fatalf(".goroutine.main definition not in IR\nfull IR:\n%s", ir)
	}
	defEnd := strings.Index(ir[defStart:], "\n}\n")
	if defEnd < 0 {
		t.Fatalf("could not find end of .goroutine.main\nfrom defStart:\n%s", ir[defStart:])
	}
	return ir[defStart : defStart+defEnd+2]
}

func TestT0903MemberAssignNoDropDropsOld(t *testing.T) {
	ir := generateIR(t, `
		type BB_t0903 { int v; }
		type Holder_t0903 { BB_t0903 field; }
		main() {
			h := Holder_t0903(field: BB_t0903(v: 0));
			h.field = BB_t0903(v: 11);
		}
	`)
	mainFn := mainBodyT0903(t, ir)
	// The T0410/T0903 drop-old branch emits a uniquely-named null+same-pointer
	// guard block (`field.userdrop`) followed by emitVariantFieldDrop's B0218
	// pal_free (`varfield.free`). Neither appears for a no-drop field LHS
	// unless the broadened predicate fires.
	if !strings.Contains(mainFn, "field.userdrop") {
		t.Errorf("expected field.userdrop guard block for no-drop heap user field reassignment (drop-old)\nmain:\n%s", mainFn)
	}
	if !strings.Contains(mainFn, "varfield.free") {
		t.Errorf("expected varfield.free (B0218 pal_free) for overwritten no-drop heap user field\nmain:\n%s", mainFn)
	}
}

func TestT0903VectorIndexAssignNoDropDropsOld(t *testing.T) {
	ir := generateIR(t, `
		type BB_t0903 { int v; }
		main() {
			arr := [BB_t0903(v: 0)];
			arr[0] = BB_t0903(v: 11);
		}
	`)
	mainFn := mainBodyT0903(t, ir)
	// The drop-old runs in the in-bounds index-assign block. Scope to the region
	// after `indexassign.ok` and before the next basic block so we don't pick up
	// the unrelated array scope-exit drop loop.
	okIdx := strings.Index(mainFn, "\nindexassign.ok")
	if okIdx < 0 {
		t.Fatalf("expected indexassign.ok block in main\nmain:\n%s", mainFn)
	}
	region := mainFn[okIdx+1:]
	// Truncate at the out-of-bounds sibling block, which follows the ok block.
	if oob := strings.Index(region, "\nindexassign.oob"); oob > 0 {
		region = region[:oob]
	}
	if !strings.Contains(region, "varfield.free") {
		t.Errorf("expected varfield.free (B0218 pal_free) drop-old for overwritten no-drop heap user element\nindexassign.ok region:\n%s", region)
	}
}

// T0903: genVectorIndex must dup-on-read a no-drop heap user element (`t := vec[i]`)
// so the bound variable owns an independent instance — otherwise it aliases the
// vector's element and both free it at scope exit (double free). Mirrors
// genArrayIndex's T0590 no-drop dup-on-read; the dup goes via dupHeapValue
// (pal_alloc + memcpy in a heapdup block).
func TestT0903VectorIndexReadNoDropDups(t *testing.T) {
	ir := generateIR(t, `
		type BB_t0903 { int v; }
		main() {
			src := [BB_t0903(v: 5)];
			t := src[0];
		}
	`)
	mainFn := mainBodyT0903(t, ir)
	okIdx := strings.Index(mainFn, "\nindex.ok")
	if okIdx < 0 {
		t.Fatalf("expected index.ok block in main\nmain:\n%s", mainFn)
	}
	region := mainFn[okIdx+1:]
	if oob := strings.Index(region, "\nindex.oob"); oob > 0 {
		region = region[:oob]
	}
	// The no-drop dup branch reads the element then conditionally clones it via
	// dupHeapValue (pal_alloc + memcpy in a heapdup block). Without the fix the
	// element is stored straight into the binding with no heapdup.
	if !strings.Contains(region, "heapdup") {
		t.Errorf("expected heapdup (dupHeapValue) dup-on-read for no-drop heap user vector element\nindex.ok region:\n%s", region)
	}
}
