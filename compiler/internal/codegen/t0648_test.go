package codegen

import (
	"strings"
	"testing"
)

// T0648: returning a `Vector[T]` element of a `Vector[Vector[T]]` field by
// value from a method/`[]` operator crashed (panic for int / SIGSEGV for
// string). genReturnStmt / var-decl set c.dupContainerFieldAccess for a
// Vector result; genVectorIndex then evaluated the index *target* (the
// `this.rows` field read) with that flag still set, so genFieldAccess
// consumed it to deep-clone the ENTIRE outer container and track the whole
// clone as a stmt-temp. The index read one element out of that clone,
// statement-end cleanup dropped the whole clone (incl. that element), and
// the returned inner pointer dangled. Fix: T0500-style save/suppress/restore
// of dupContainerFieldAccess around the e.Target eval in genVectorIndex, so
// the flag survives to the element-level dup (the T0383 branch) which makes
// the correct owned copy of just the indexed inner vector.
//
// These tests lock the structural IR signature. Runtime zero-leak / no-
// crash / deep-copy independence is enforced by the batch tests in
// tests/e2e/nested_vector_element_return_test.pr.

// TestT0648_NestedVectorIntElementDupsElementNotWholeOuter — `return
// this.rows[i]` where `rows: Vector[Vector[int]]` on an owner-droppable
// type must dup ONLY the indexed inner `Vector[int]` (the element-level
// dupVector — vecdup.copy/vecdup.init, the T0383 branch at expr.go:8759),
// NOT deep-clone the whole outer container. The pre-fix bug consumed
// dupContainerFieldAccess at the `this.rows` field read and deep-cloned the
// entire outer `Vector[Vector[int]]`, whose `Vector[int]` elements each
// need a clone loop — emitting a `vecclone.*` loop. Post-fix the only dup
// is the single inner-vector element dup (int elements are primitive, no
// clone loop), so `vecclone.` must be ABSENT. `this.rows` is borrowed by
// the method (never dropped here), so no `vecdrop.` walk either — its
// presence would mean a whole-outer clone got tracked-and-dropped in-body.
func TestT0648_NestedVectorIntElementDupsElementNotWholeOuter(t *testing.T) {
	ir := generateIR(t, `
		type VI { Vector[int][] rows; at(int i) Vector[int] { return this.rows[i]; } }
		caller() { v := VI(rows: [[1,2],[3]]); n := v.at(0).len; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "VI.at")
	if body == "" {
		t.Fatalf("expected @VI.at in IR")
	}
	// Element-level dup (the T0383 branch at expr.go:8759) fired: a single
	// inner-vector dupVector. Its presence proves the flag survived the
	// target eval (the fix) instead of being consumed by the field read.
	if !strings.Contains(body, "vecdup.copy") || !strings.Contains(body, "vecdup.init") {
		t.Errorf("expected element-level dupVector (vecdup.copy/vecdup.init) for the "+
			"indexed inner Vector[int] (T0383 branch); none found — the "+
			"dupContainerFieldAccess flag was consumed by the field read (pre-fix):\n%s", body)
	}
	// No whole-outer deep-clone: the pre-fix bug deep-cloned the whole
	// `Vector[Vector[int]]`, whose `Vector[int]` elements each get an
	// emitVectorElementCloneLoop (`vecclone.*`). Post-fix only the inner
	// int-vector is dup'd (primitive elements → no clone loop).
	if strings.Contains(body, "vecclone.") {
		t.Errorf("found a whole-outer deep-clone loop (vecclone.*) in @VI.at — the "+
			"dupContainerFieldAccess flag was wrongly consumed by the `this.rows` "+
			"field read, deep-cloning the entire outer Vector[Vector[int]] (T0648 "+
			"pre-fix crash):\n%s", body)
	}
	// `this.rows` is borrowed by the method — never dropped in-body. A
	// `vecdrop.*` walk here would mean a whole-outer clone was tracked and
	// dropped inside @VI.at (the pre-fix UAF: drop the clone incl. the
	// returned element).
	if strings.Contains(body, "vecdrop.") {
		t.Errorf("found a vector element-drop walk (vecdrop.*) in @VI.at — `this.rows` "+
			"is borrowed and must not be dropped in-body; its presence means a "+
			"tracked whole-outer clone (T0648 pre-fix UAF):\n%s", body)
	}
}

// TestT0648_NestedVectorStringElementDeepDups — string variant. `return
// this.rows[i]` where `rows: Vector[Vector[string]]` must DEEP-dup the
// single indexed inner `Vector[string]`: the element-level dupVector
// (vecdup.copy) PLUS a per-string dup loop (vecdup_str.*/strdup.*) so the
// returned inner vector owns independent string copies (no double-free
// between the caller's temp and the outer container's element walk). And
// still NO whole-outer deep-clone (`vecclone.` absent) — the pre-fix bug
// (SIGSEGV for the string element type) deep-cloned the whole outer.
func TestT0648_NestedVectorStringElementDeepDups(t *testing.T) {
	ir := generateIR(t, `
		type VB { Vector[string][] rows; at(int i) Vector[string] { return this.rows[i]; } }
		caller() { v := VB(rows: [["a","b"],["c"]]); n := v.at(0).len; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "VB.at")
	if body == "" {
		t.Fatalf("expected @VB.at in IR")
	}
	if !strings.Contains(body, "vecdup.copy") {
		t.Errorf("expected element-level dupVector (vecdup.copy) for the indexed "+
			"inner Vector[string] (T0383 branch); none found (pre-fix flag "+
			"consumed by field read):\n%s", body)
	}
	// Deep dup: each string in the indexed inner vector is itself dup'd
	// (emitVectorStringDupLoop). This is what makes the returned inner
	// vector a fully independent owned copy.
	if !strings.Contains(body, "vecdup_str.") || !strings.Contains(body, "strdup.") {
		t.Errorf("expected a per-string dup loop (vecdup_str.*/strdup.*) deep-cloning "+
			"the indexed inner Vector[string]'s strings; none found — returned "+
			"inner vector would alias the container's strings (T0648 SIGSEGV):\n%s", body)
	}
	if strings.Contains(body, "vecclone.") {
		t.Errorf("found a whole-outer deep-clone loop (vecclone.*) in @VB.at — the "+
			"dupContainerFieldAccess flag was wrongly consumed by the `this.rows` "+
			"field read (T0648 pre-fix SIGSEGV):\n%s", body)
	}
	if strings.Contains(body, "vecdrop.") {
		t.Errorf("found a vector element-drop walk (vecdrop.*) in @VB.at — `this.rows` "+
			"is borrowed; its presence means a tracked whole-outer clone (T0648 "+
			"pre-fix UAF):\n%s", body)
	}
}

// TestT0648_OperatorIndexNestedVectorElementDupsElement — `[]` operator
// parity. The reported shape includes a user `[](int i) Vector[T]` body
// that does `return this.rows[i]`. That operator body is compiled through
// genVectorIndex identically to a plain method, so the same element-level
// dup (not whole-outer clone) must hold inside the operator function.
func TestT0648_OperatorIndexNestedVectorElementDupsElement(t *testing.T) {
	ir := generateIR(t, `
		type VOP { Vector[string][] rows; [](int i) Vector[string] { return this.rows[i]; } }
		caller() { v := VOP(rows: [["a","b"],["c"]]); n := v[0].len; }
		main() { caller(); }
	`)
	body := extractFunction(ir, `"VOP.[]"`)
	if body == "" {
		t.Fatalf("expected @\"VOP.[]\" operator function in IR")
	}
	if !strings.Contains(body, "vecdup.copy") {
		t.Errorf("expected element-level dupVector (vecdup.copy) in the `[]` operator "+
			"body for the indexed inner Vector[string]; none found (pre-fix):\n%s", body)
	}
	if strings.Contains(body, "vecclone.") {
		t.Errorf("found a whole-outer deep-clone loop (vecclone.*) in @\"VOP.[]\" — "+
			"dupContainerFieldAccess consumed by the `this.rows` field read inside "+
			"the operator body (T0648 pre-fix):\n%s", body)
	}
	if strings.Contains(body, "vecdrop.") {
		t.Errorf("found a vector element-drop walk (vecdrop.*) in @\"VOP.[]\" — "+
			"`this.rows` is borrowed; its presence means a tracked whole-outer "+
			"clone (T0648 pre-fix UAF):\n%s", body)
	}
}

// TestT0648_OwnedLocalNestedIndexUnchanged — over-suppression guard. The
// fix suppresses dupContainerFieldAccess only across the index *target*
// eval, then restores it. For an owned-local nested vector
// (`rows := [[..]]; return rows[i];`) the target is an IdentExpr (never a
// field read), so the flag always survived to the element-level dup and
// that path was already correct. After the fix it must be IDENTICAL — the
// same element-level dupVector (vecdup.copy) and no whole-outer
// `vecclone.*`. The local source `rows` IS owned by the function, so a
// scope-exit `vecdrop.*` walk dropping the local IS expected here (it is
// not a tracked clone — contrast the borrowed-field cases above which must
// have NO vecdrop).
func TestT0648_OwnedLocalNestedIndexUnchanged(t *testing.T) {
	ir := generateIR(t, `
		at(int i) Vector[int] { rows := [[1,2],[3]]; return rows[i]; }
		caller() { n := at(0).len; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.at")
	if body == "" {
		t.Fatalf("expected @__user.at in IR")
	}
	if !strings.Contains(body, "vecdup.copy") || !strings.Contains(body, "vecdup.init") {
		t.Errorf("expected element-level dupVector (vecdup.copy/vecdup.init) for the "+
			"owned-local nested index (the always-correct path); none found — the "+
			"fix over-suppressed the restored flag:\n%s", body)
	}
	if strings.Contains(body, "vecclone.") {
		t.Errorf("owned-local nested index emitted a whole-outer deep-clone loop "+
			"(vecclone.*) — a vector index must dup exactly one element, never the "+
			"whole container:\n%s", body)
	}
}

// TestT0648_ArcElementDupsElementNotWholeOuter — element-type-agnostic guard.
// The fix suppresses dupContainerFieldAccess across the index *target* eval
// regardless of the element type, so it protects the Arc element branch
// (expr.go:8793) identically to the Vector branch. `return this.rows[i]`
// where `rows: Vector[Arc[int]]` on an owner-droppable type must dup ONLY the
// single indexed Arc (an `arcdup.inc` refcount bump), NOT deep-clone the
// whole outer `Vector[Arc[int]]`. Pre-fix the `this.rows` field read consumed
// the flag and deep-cloned the entire outer container — and since Arc
// elements are droppable, emitVectorElementCloneLoop would emit a
// `vecclone.*` clone loop, then track-and-drop the whole clone (`vecdrop.*`)
// in-body, dangling the returned Arc (T0648 pre-fix UAF). This locks the
// fix at the IR level for a NON-Vector element type (the existing IR tests
// only cover Vector[int]/Vector[string]); the runtime 0-leak/no-double-free
// proof is test_t0648_arc_element_* in the e2e file.
func TestT0648_ArcElementDupsElementNotWholeOuter(t *testing.T) {
	ir := generateIR(t, `
		type ArcRows { Vector[Arc[int]] rows; at(int i) Arc[int] { return this.rows[i]; } }
		caller() { v := ArcRows(rows: [Arc[int](7), Arc[int](9)]); a := v.at(0); n := a.borrow; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "ArcRows.at")
	if body == "" {
		t.Fatalf("expected @ArcRows.at in IR")
	}
	// Element-level Arc dup (the T0383/8793 branch) fired: a single arcdup
	// refcount bump for the indexed Arc. Its presence proves the flag
	// survived the target eval (the fix) instead of being consumed by the
	// `this.rows` field read.
	if !strings.Contains(body, "arcdup.inc") {
		t.Errorf("expected element-level Arc dup (arcdup.inc) for the indexed Arc "+
			"(expr.go:8793 branch); none found — the dupContainerFieldAccess flag "+
			"was consumed by the `this.rows` field read (T0648 pre-fix):\n%s", body)
	}
	// No whole-outer deep-clone: pre-fix the whole `Vector[Arc[int]]` was
	// deep-cloned; Arc elements are droppable so emitVectorElementCloneLoop
	// emits a `vecclone.*` loop. Post-fix only the single Arc is dup'd.
	if strings.Contains(body, "vecclone.") {
		t.Errorf("found a whole-outer deep-clone loop (vecclone.*) in @ArcRows.at — "+
			"the dupContainerFieldAccess flag was wrongly consumed by the `this.rows` "+
			"field read, deep-cloning the entire outer Vector[Arc[int]] (T0648 "+
			"pre-fix UAF, element-type-agnostic):\n%s", body)
	}
	// `this.rows` is borrowed by the method — never dropped in-body. A
	// `vecdrop.*` walk here would mean a whole-outer clone was tracked and
	// dropped inside @ArcRows.at (the pre-fix UAF).
	if strings.Contains(body, "vecdrop.") {
		t.Errorf("found a vector element-drop walk (vecdrop.*) in @ArcRows.at — "+
			"`this.rows` is borrowed; its presence means a tracked whole-outer "+
			"clone (T0648 pre-fix UAF):\n%s", body)
	}
}
