package codegen

import (
	"strings"
	"testing"
)

// T1182: Inline force-unwrap of a fixed-array / Vector index optional on a
// heap-user element type followed by a member access (`arr[i]!.field` /
// `vec[i]!.field`) double-freed the element. genArrayIndex / genVectorIndex
// only deep-clone a slot when a sibling dup flag is set (binding/return/arg
// contexts); the inline-temp unwrap reaches the plain no-dup extractvalue path,
// so the extracted inner ALIASES the container's owned slot — yet
// trackHeapUserTypeResult still registered it as an independently-owned
// statement temp, so the temp drop AND the container's element-drop walk both
// freed the same instance (SIGSEGV at 0x0 when the element has a heap sub-field).
//
// Fix: isContainerIndexUnwrapSource now returns true for fixed-array
// (*types.Array) and Vector index sources, so genOptionalForceUnwrap sets
// optionalUnwrapContainerBorrow on the plain path and trackHeapUserTypeResult
// SKIPS the owned-temp registration for the aliased inner. The
// binding/return/arg dup paths return early in genOptionalForceUnwrap
// (optionalHeapDup), so this never touches them.
//
// Runtime zero-leak/double-free behavior is covered by the e2e batch tests in
// tests/arrays/fixed_heap_field_test.pr and tests/e2e/optional_vector_drop_test.pr
// under the zero-tolerance leak gate; these lock the IR shape.

const t1182Decls = `
	type Resource { string name; drop(~this) {} }
	make_resource() Resource { return Resource(name: "test!"); }
`

// TestT1182_ArrayInlineUnwrapDoesNotDoubleDrop — the inline `arr[i]!.name` form
// must NOT register the aliased unwrapped inner as an owned statement temp. The
// array-literal construction / optional-wrap / scope temps account for exactly
// nine Resource.drop$wrap emissions; the buggy build emitted a TENTH for the
// aliased unwrapped result (the double-free). This is the fix-sensitive assertion
// — it fails (count 10) on the pre-fix build. It also verifies the borrow path is
// reached (no `heapdup` clone of the element).
func TestT1182_ArrayInlineUnwrapDoesNotDoubleDrop(t *testing.T) {
	ir := generateIR(t, t1182Decls+`
		afu() int {
			Resource? a0 = make_resource();
			Resource? a1 = make_resource();
			Resource?[2] arr = [a0, a1];
			return arr[0]!.name.len;
		}
		main() { _ := afu(); }
	`)
	fn := extractFunction(ir, "__user.afu")
	if fn == "" {
		t.Fatalf("could not extract __user.afu from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "unwrap.ok") {
		t.Fatalf("expected an unwrap.ok block (the force-unwrap) in __user.afu:\n%s", fn)
	}
	// Borrow path: the inline unwrap must not clone the element.
	if n := strings.Count(fn, "heapdup"); n != 0 {
		t.Fatalf("expected NO heap dup for the inline array-index unwrap (it borrows "+
			"the slot), got %d heapdup markers:\n%s", n, fn)
	}
	// Owned-temp count: 9 legitimate construction/scope temps, and NOT a 10th for
	// the aliased unwrapped inner (that extra one was the double-free, pre-fix).
	if n := strings.Count(fn, "Resource.drop$wrap"); n != 9 {
		t.Fatalf("expected exactly 9 Resource.drop$wrap in __user.afu (construction/scope "+
			"temps only); got %d. A 10th means the aliased unwrapped inner was wrongly "+
			"registered as an owned temp → double-free:\n%s", n, fn)
	}
}

// TestT1182_VectorInlineUnwrapDoesNotDoubleDrop — Vector counterpart. A
// single-element `Resource?[]` gives four legitimate Resource.drop$wrap temps;
// the buggy build emitted a fifth for the aliased unwrapped result. Fix-sensitive.
func TestT1182_VectorInlineUnwrapDoesNotDoubleDrop(t *testing.T) {
	ir := generateIR(t, t1182Decls+`
		vfu() int {
			Resource? a0 = make_resource();
			Resource?[] v = [a0];
			return v[0]!.name.len;
		}
		main() { _ := vfu(); }
	`)
	fn := extractFunction(ir, "__user.vfu")
	if fn == "" {
		t.Fatalf("could not extract __user.vfu from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "unwrap.ok") {
		t.Fatalf("expected an unwrap.ok block (the force-unwrap) in __user.vfu:\n%s", fn)
	}
	if n := strings.Count(fn, "heapdup"); n != 0 {
		t.Fatalf("expected NO heap dup for the inline Vector-index unwrap (it borrows "+
			"the slot), got %d heapdup markers:\n%s", n, fn)
	}
	if n := strings.Count(fn, "Resource.drop$wrap"); n != 4 {
		t.Fatalf("expected exactly 4 Resource.drop$wrap in __user.vfu (construction/scope "+
			"temps only); got %d. A 5th means the aliased unwrapped inner was wrongly "+
			"registered as an owned temp → double-free:\n%s", n, fn)
	}
}

// TestT1182_ArrayBindingFormDups — locks the OTHER side of the dup/no-dup split:
// the var-binding form (`r := arr[i]!`) must deep-dup the inner so the new
// variable owns an independent copy (`heapdup` present). Unlike the inline
// borrow, the binding path is a pre-existing dup path unaffected by the fix — this
// guards against a future change that regresses the binding into an aliasing read
// (which would double-free, as the inline form did pre-fix).
func TestT1182_ArrayBindingFormDups(t *testing.T) {
	ir := generateIR(t, t1182Decls+`
		abind() int {
			Resource? a0 = make_resource();
			Resource? a1 = make_resource();
			Resource?[2] arr = [a0, a1];
			Resource r = arr[0]!;
			return r.name.len;
		}
		main() { _ := abind(); }
	`)
	fn := extractFunction(ir, "__user.abind")
	if fn == "" {
		t.Fatalf("could not extract __user.abind from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the binding-form array unwrap "+
			"(the bound variable must own an independent copy), got none:\n%s", fn)
	}
}

// TestT1182_VectorBindingFormDups — Vector counterpart of the binding-dup lock.
func TestT1182_VectorBindingFormDups(t *testing.T) {
	ir := generateIR(t, t1182Decls+`
		vbind() int {
			Resource? a0 = make_resource();
			Resource?[] v = [a0];
			Resource r = v[0]!;
			return r.name.len;
		}
		main() { _ := vbind(); }
	`)
	fn := extractFunction(ir, "__user.vbind")
	if fn == "" {
		t.Fatalf("could not extract __user.vbind from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the binding-form Vector unwrap "+
			"(the bound variable must own an independent copy), got none:\n%s", fn)
	}
}
