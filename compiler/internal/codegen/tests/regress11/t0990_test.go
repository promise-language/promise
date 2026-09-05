package regress11

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T0990 — a Vector-field mutation used to evaluate its field owner TWICE: once
// while reading the buffer (genFieldAccess) and again to recompute the
// write-back slot (storeBackSlicePtr → genFieldPtr → genExpr(target.Target)).
// For an impure owner the two evaluations produced DIFFERENT instances, so the
// relocated buffer was stored into the second one's field while the first kept
// the freed pre-mutation pointer (use-after-free + double free at drop).
//
// The fix captures the field slot once (vectorFieldSlot / genInstanceFieldSlot)
// and stores through it. These tests pin both halves of that in the IR: exactly
// one call to the impure owner, and a store of the mutating call's result into
// the getelementptr that was read from. Runtime correctness and leak-freedom are
// covered by tests/e2e/t0990_*.pr.

// t0990Source builds a program whose `caller` mutates `make_holder().items` with
// the given statement.
func t0990Source(stmt string) string {
	return `
		type Holder { Vector[int] items; }
		make_holder() Holder { return Holder(items: [1, 2, 3]); }
		caller() { ` + stmt + ` }
		main() { caller(); }
	`
}

var (
	t0990OwnerCall  = regexp.MustCompile(`@__user\.make_holder\(\)`)
	t0990FieldSlot  = regexp.MustCompile(`(%\d+) = getelementptr %promise_Holder_i, %promise_Holder_i\* %\d+, i32 0, i32 1\b`)
	t0990VectorCall = regexp.MustCompile(`(%\d+) = call i8\* @promise_vector_(\w+)\(`)
)

// assertSingleEvalStoreBack checks that `caller` evaluates the field owner
// exactly once and stores a promise_vector_* result back through the very
// getelementptr the buffer was read from.
func assertSingleEvalStoreBack(t *testing.T, stmt string) {
	t.Helper()
	ir := codegentest.GenerateIR(t, t0990Source(stmt))
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}

	if n := len(t0990OwnerCall.FindAllString(body, -1)); n != 1 {
		t.Errorf("expected exactly 1 call to @__user.make_holder(), got %d:\n%s", n, body)
	}

	slots := t0990FieldSlot.FindAllStringSubmatch(body, -1)
	if len(slots) != 1 {
		t.Fatalf("expected exactly 1 getelementptr to Holder.items, got %d:\n%s", len(slots), body)
	}
	slot := slots[0][1]

	// The write-back must store a promise_vector_* result into that same slot.
	stored := false
	for _, m := range t0990VectorCall.FindAllStringSubmatch(body, -1) {
		if strings.Contains(body, fmt.Sprintf("store i8* %s, i8** %s\n", m[1], slot)) {
			stored = true
			break
		}
	}
	if !stored {
		t.Errorf("expected a promise_vector_* result stored back into the read slot %s:\n%s", slot, body)
	}
}

func TestT0990_PushSingleEvalStoreBack(t *testing.T) {
	assertSingleEvalStoreBack(t, `make_holder().items.push(9);`)
}

func TestT0990_PopSingleEvalStoreBack(t *testing.T) {
	assertSingleEvalStoreBack(t, `make_holder().items.pop();`)
}

func TestT0990_RemoveSingleEvalStoreBack(t *testing.T) {
	assertSingleEvalStoreBack(t, `make_holder().items.remove(0);`)
}

func TestT0990_IndexAssignSingleEvalStoreBack(t *testing.T) {
	assertSingleEvalStoreBack(t, `make_holder().items[0] = 9;`)
}

func TestT0990_IndexCompoundAssignSingleEvalStoreBack(t *testing.T) {
	assertSingleEvalStoreBack(t, `make_holder().items[0] += 9;`)
}

func TestT0990_IndexIncrementSingleEvalStoreBack(t *testing.T) {
	assertSingleEvalStoreBack(t, `make_holder().items[0]++;`)
}

func TestT0990_SliceAssignSingleEvalStoreBack(t *testing.T) {
	assertSingleEvalStoreBack(t, `make_holder().items[0:1] = [9];`)
}

// TestT0990_GetterReceiverKeepsTempPath — a getter receiver is not a plain field
// slot, so vectorFieldSlot must reject it and leave the T1370 statement-temp
// path in charge. The push result is stored into a stack alloca (the temp's
// backing slot), never into a Holder instance GEP.
func TestT0990_GetterReceiverKeepsTempPath(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder {
			Vector[int] items;
			get view Vector[int] { return this.items.clone(); }
		}
		caller() {
			h := Holder(items: [1, 2, 3]);
			h.view.push(9);
		}
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	push := t0990VectorCall.FindStringSubmatch(body)
	if push == nil {
		t.Fatalf("expected a promise_vector_* call in caller:\n%s", body)
	}
	for _, m := range t0990FieldSlot.FindAllStringSubmatch(body, -1) {
		if strings.Contains(body, fmt.Sprintf("store i8* %s, i8** %s\n", push[1], m[1])) {
			t.Errorf("getter clone must not be stored back into the Holder field slot %s:\n%s", m[1], body)
		}
	}
}

// TestT0990_NestedElementReceiverSingleEval — `make_nested().rows[i].push(x)`
// reaches the store-back through evalVectorReceiver's IndexExpr branch, whose
// T1064 recompute re-addresses the outer container after argument evaluation.
// That recompute is sound only for an outer chain that re-addresses purely; a
// field of a call result does not, so it called the owner a second time and
// stored the grown inner buffer into a different instance's row. An rvalue outer
// takes the early-captured element slot instead (as `fresh_vov()[0].push`
// already did), so the owner is called exactly once.
func TestT0990_NestedElementReceiverSingleEval(t *testing.T) {
	nestedOwnerCall := regexp.MustCompile(`@__user\.make_nested\(\)`)
	for _, stmt := range []string{
		`make_nested().rows[0].push(9);`,
		`make_nested().rows[0].pop();`,
		`make_nested().rows[0].remove(0);`,
	} {
		ir := codegentest.GenerateIR(t, `
			type Nested { Vector[Vector[int]] rows; }
			make_nested() Nested { return Nested(rows: [[1, 2], [3, 4]]); }
			caller() { `+stmt+` }
			main() { caller(); }
		`)
		body := codegentest.ExtractFunction(ir, "__user.caller")
		if body == "" {
			t.Fatalf("%s: expected __user.caller in IR", stmt)
		}
		if n := len(nestedOwnerCall.FindAllString(body, -1)); n != 1 {
			t.Errorf("%s: expected exactly 1 call to @__user.make_nested(), got %d:\n%s", stmt, n, body)
		}
	}
}

// TestT0990_NestedElementLocalOwnerKeepsRecompute — the T1064 recompute must
// stay in place for an outer chain that DOES re-address purely (a local, and a
// field chain rooted at a local): the element slot is re-derived after argument
// evaluation, so an argument that reallocates the outer vector cannot leave it
// dangling. Pinned by a second load of the outer pointer after the argument call.
func TestT0990_NestedElementLocalOwnerKeepsRecompute(t *testing.T) {
	for _, subject := range []string{`n.rows`, `o.inner.rows`} {
		ir := codegentest.GenerateIR(t, `
			type Nested { Vector[Vector[int]] rows; }
			type Outer { Nested inner; }
			grow(int[][] ~vov) int { vov.push(Vector[int](capacity: 1)); return 7; }
			caller() {
				n := Nested(rows: [[1, 2]]);
				o := Outer(inner: Nested(rows: [[1, 2]]));
				`+subject+`[0].push(grow(`+subject+`));
			}
			main() { caller(); }
		`)
		body := codegentest.ExtractFunction(ir, "__user.caller")
		if body == "" {
			t.Fatalf("%s: expected __user.caller in IR", subject)
		}
		pushIdx := strings.Index(body, "@promise_vector_push")
		if pushIdx < 0 {
			t.Fatalf("%s: expected a promise_vector_push call:\n%s", subject, body)
		}
		// The store-back GEP is emitted after the push, from a freshly loaded outer
		// pointer — so a `getelementptr i8*` (the element-slot GEP) follows it.
		if !strings.Contains(body[pushIdx:], "getelementptr i8*") {
			t.Errorf("%s: expected the element slot to be recomputed after the push:\n%s", subject, body)
		}
	}
}
