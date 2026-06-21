package codegen

import (
	"strings"
	"testing"
)

// TestT1099_TupleElementVectorCloneDupsRef pins the codegen fix for T1099:
// Vector[(Ref[int], int)].clone() must deep-clone the Ref inside the tuple
// element. Pre-fix, emitVectorElementCloneLoop (stmt.go) had no tuple case and
// returned early for tuple element types, leaving the memcpy'd Ref shared
// (refcount unchanged) → double-free/UAF when both the source and clone drop at
// scope exit. Post-fix the clone loop walks the tuple's fields via dupTupleValue
// and emits an arc dup (arcdup.inc refcount bump) for the Ref field. We assert
// the clone loop runs (vecclone.*) AND the inner Ref is refcount-bumped
// (arcdup.inc) in the caller where Vector.clone() is inlined. The runtime
// no-double-free / independent-ownership proof is test_tuple_ref_* in
// tests/e2e/vector_single_owner_handle_clone_test.pr.
func TestT1099_TupleElementVectorCloneDupsRef(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			v := Vector[(Ref[int], int)]();
			v.push((Ref[int](7), 1));
			v2 := v.clone();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	// Pre-fix the tuple element fell through emitVectorElementCloneLoop's early
	// `return` — no per-element clone loop was emitted at all.
	if !strings.Contains(body, "vecclone.head") {
		t.Errorf("T1099: expected a per-element clone loop (vecclone.*) for "+
			"Vector[(Ref[int], int)].clone(); none found — emitVectorElementCloneLoop "+
			"returned early for the tuple element (the pre-fix shallow-copy gap):\n%s", body)
	}
	// The tuple's Ref field must be refcount-bumped so the clone owns it
	// independently; arcdup.inc is dupArc's increment block.
	if !strings.Contains(body, "arcdup.inc") {
		t.Errorf("T1099: expected an Arc refcount bump (arcdup.inc) for the Ref "+
			"inside the cloned tuple element; none found — the tuple's Ref was "+
			"shallow-copied (double-free/UAF at scope exit):\n%s", body)
	}
}

// TestT1099_TupleEnumVariantCloneDupsRef pins the compiler.go half of the fix:
// an enum variant carrying a tuple-of-Ref, when cloned inside a Vector, routes
// through dupEnumElementInPlace → emitVariantFieldDup. Pre-fix that function had
// no tuple case, so the variant's tuple field (and its Ref) was left shallow.
// The enum has no synthesized clone(), forcing the dup (not cloneEnumValue)
// path. We assert the variant clone runs and the inner Ref is refcount-bumped.
func TestT1099_TupleEnumVariantCloneDupsRef(t *testing.T) {
	ir := generateIR(t, `
		enum Holder[T] { Empty, Pair((Ref[T], int) pair), }
		caller() {
			v := Vector[Holder[int]]();
			v.push(Holder[int].Pair((Ref[int](9), 2)));
			v2 := v.clone();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	if !strings.Contains(body, "arcdup.inc") {
		t.Errorf("T1099: expected an Arc refcount bump (arcdup.inc) for the Ref "+
			"inside the cloned enum variant's tuple field; none found — "+
			"emitVariantFieldDup left the tuple field shallow (double-free at "+
			"scope exit):\n%s", body)
	}
}
