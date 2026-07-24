package codegen

import (
	"strings"
	"testing"
)

// T0595: push on a nested slice (arr[i].push / vov[i].push) used to panic in
// codegen at storeBackSlicePtr's IndexExpr case. The fix computes the element
// slot pointer (genIndexSlotPtr) and stores the grown inner Vector's pointer
// back into it. These tests assert IR generation no longer panics and emits the
// push + store-back-into-slot for both container kinds. The runtime correctness
// and no-leak guarantees are verified by the Promise e2e tests in
// tests/arrays/fixed_heap_field_test.pr.

// TestT0595_FixedArrayOfVectorsPush — arr[i].push on a Vector[int][2] (fixed
// array of Vectors) emits promise_vector_push and stores the result back into
// the array slot via a getelementptr.
func TestT0595_FixedArrayOfVectorsPush(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			Vector[int][2] arr = [Vector[int](), Vector[int]()];
			arr[1].push(99);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "@promise_vector_push(") {
		t.Errorf("expected call to @promise_vector_push in caller:\n%s", body)
	}
	// The grown pointer must be stored back into the array slot — a getelementptr
	// into the fixed array followed by a store i8* of the push result.
	if !strings.Contains(body, "getelementptr") || !strings.Contains(body, "store i8*") {
		t.Errorf("expected getelementptr + store i8* (store-back into array slot):\n%s", body)
	}
}

// TestT0595_VectorOfVectorsPush — vov[i].push on a Vector[int][] (Vector of
// Vectors) emits promise_vector_push and stores the result back into the heap
// element slot.
func TestT0595_VectorOfVectorsPush(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			Vector[int][] vov = [Vector[int](), Vector[int]()];
			vov[0].push(7);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "@promise_vector_push(") {
		t.Errorf("expected call to @promise_vector_push in caller:\n%s", body)
	}
	// Store-back into the outer vector's element slot is bounds-checked via the
	// shared emitIndexBoundsCheck helper (nestedpush.ok block) then a store i8*.
	if !strings.Contains(body, "nestedpush.ok") {
		t.Errorf("expected nestedpush.ok bounds-check block (vector slot store-back):\n%s", body)
	}
	if !strings.Contains(body, "store i8*") {
		t.Errorf("expected store i8* (store-back into vector slot):\n%s", body)
	}
}

// TestT0595_NestedPopRemoveNoPanic — pop/remove on a nested slice hit the same
// storeBackSlicePtr COW store-back path; verify they no longer panic and emit
// the slot store-back. (Bonus paths fixed by the same change.)
func TestT0595_NestedPopRemoveNoPanic(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			Vector[int][] vov = [Vector[int]()];
			vov[0].push(1);
			vov[0].pop();
			vov[0].remove(0);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "@promise_vector_pop(") {
		t.Errorf("expected call to @promise_vector_pop in caller:\n%s", body)
	}
	if !strings.Contains(body, "@promise_vector_remove(") {
		t.Errorf("expected call to @promise_vector_remove in caller:\n%s", body)
	}
}

// TestT0595_NestedIndexAssignStoreBack — index-assign into a nested slice
// (arr[i][j] = x) reaches storeBackSlicePtr's IndexExpr case via the assignment
// route (NOT the push-receiver slot route), then genIndexSlotPtr writes the
// COW'd inner-Vector pointer back into the outer slot. Pre-T0595 this panicked
// like push. Asserts IR generation succeeds and emits the COW + slot store-back.
func TestT0595_NestedIndexAssignStoreBack(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			Vector[int][] vov = [Vector[int]()];
			vov[0].push(1);
			vov[0][0] = 50;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "@promise_vector_cow(") {
		t.Errorf("expected @promise_vector_cow (index-assign COW) in caller:\n%s", body)
	}
	// genIndexSlotPtr bounds-checks the outer slot via the shared helper before
	// storing the COW'd pointer back into it.
	if !strings.Contains(body, "nestedpush.ok") {
		t.Errorf("expected nestedpush.ok bounds-check block (slot store-back):\n%s", body)
	}
	if !strings.Contains(body, "store i8*") {
		t.Errorf("expected store i8* (store-back into outer slot):\n%s", body)
	}
}

// TestT0595_GenericNestedPush — nested push from a monomorphized generic body.
// The receiver's outer-container type is substituted (c.typeSubst != nil) before
// genIndexSlotPtr / indexTargetIsArrayOrVector compute the slot, so this
// exercises the substitution prologue those helpers share with the read path.
func TestT0595_GenericNestedPush(t *testing.T) {
	ir := generateIR(t, `
		push_into[T](Vector[Vector[T]]~ vov, int i, T val) {
			vov[i].push(val);
		}
		caller() {
			Vector[Vector[int]] vov = [Vector[int]()];
			push_into[int](vov, 0, 7);
		}
		main() { caller(); }
	`)
	// The monomorphized instance is emitted as @"push_into[int]" (LLVM quotes
	// names containing brackets); the quotes are part of the @name( marker. Use
	// extractDefine — extractFunction would latch onto the call site in main and
	// walk back to the wrong define.
	body := extractDefine(ir, `"push_into[int]"`)
	if body == "" {
		t.Fatalf("expected monomorphized push_into[int] in IR")
	}
	if !strings.Contains(body, "@promise_vector_push(") {
		t.Errorf("expected @promise_vector_push in push_into[int]:\n%s", body)
	}
	if !strings.Contains(body, "store i8*") {
		t.Errorf("expected store i8* (slot store-back) in push_into[int]:\n%s", body)
	}
}
