package codegen

import (
	"strings"
	"testing"
)

// T0647: a user-defined non-native `[](int i) T` returning a heap type leaked
// the owned operator return at the call site. genIndexExpr's `[]` read path
// (genMethodIndex) lacked the post-call heap-temp tracking the *ast.CallExpr
// genExpr case applies to ordinary method calls, so `s[i]` leaked where the
// IDENTICAL `s.at(i)` did not. Fix: trackUserIndexResult at the genMethodIndex
// tail (mirrors the CallExpr post-call tracking) + an isUserIndexExpr exemption
// from the borrow-RHS drop-flag clearing in var-decl ownership transfer.
//
// These tests lock the structural IR signature. Runtime zero-leak / no-double-
// free is enforced by the batch tests in
// tests/e2e/user_index_heap_return_test.pr and the T0647 case in
// tests/e2e/generic_method_parametric_receiver.pr.

// TestT0647_UserIndexStringTempTracked — `s[i]` used as a *temporary* (not
// bound) must register the owned `string` return as a tracked stmt-temp and
// drop it via promise_string_drop at statement end (the tmp.exec cleanup
// block). Pre-fix the `[]` result was never stored to a temp slot → 1 leaked
// allocation per call. The string `[]` body is IR-identical to a plain method
// returning the same; parity with the CallExpr path is the fix.
func TestT0647_UserIndexStringTempTracked(t *testing.T) {
	ir := generateIR(t, `
		type SBox { string[] d; [](int i) string { return this.d[i]; } }
		caller() { s := SBox(d: ["a", "bb"]); n := s[1].len; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, `@"SBox.[]"`) {
		t.Fatalf("expected user-defined [] call @\"SBox.[]\" in caller:\n%s", body)
	}
	// trackUserIndexResult → trackStringTemp emits a tmp.exec cleanup block
	// that drops the tracked operator-return temp at statement end.
	if !strings.Contains(body, "tmp.exec") {
		t.Errorf("expected a tmp.exec stmt-temp cleanup block for the owned `[]` "+
			"string return (trackUserIndexResult); none found (pre-fix leak):\n%s", body)
	}
	tmpExec := blockByPrefixT0638(body, "tmp.exec")
	if tmpExec == "" || !strings.Contains(tmpExec, "call void @promise_string_drop(") {
		t.Errorf("expected the tmp.exec block to call @promise_string_drop on the "+
			"tracked `[]` string temp:\n%s", body)
	}
	// Ordering: the `[]` call must precede its temp-drop cleanup.
	callIdx := strings.Index(body, `@"SBox.[]"`)
	dropIdx := strings.Index(body, "\ntmp.exec")
	if callIdx < 0 || dropIdx < 0 || callIdx > dropIdx {
		t.Errorf("expected @\"SBox.[]\" call (idx %d) before its tmp.exec drop (idx %d):\n%s",
			callIdx, dropIdx, body)
	}
}

// TestT0647_UserIndexStringBindKeepsDropFlag — `x := s[i]` must claim the
// tracked temp into `x` and KEEP x's drop binding armed (so x's scope-exit
// drop frees the owned string exactly once). The pre-fix bug: isStringBorrowExpr
// treats every IndexExpr as a container borrow, so var-decl ownership transfer
// cleared x's drop flag immediately after arming it
// (`store i1 true, i1* %x.dropflag` directly followed by
// `store i1 false, i1* %x.dropflag`) → x never freed → leak. The
// isUserIndexExpr exemption removes that spurious clear.
func TestT0647_UserIndexStringBindKeepsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type SBox { string[] d; [](int i) string { return this.d[i]; } }
		caller() { s := SBox(d: ["a", "bb"]); x := s[1]; n := x.len; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, `@"SBox.[]"`) {
		t.Fatalf("expected user-defined [] call @\"SBox.[]\" in caller:\n%s", body)
	}
	if !strings.Contains(body, "store i1 true, i1* %x.dropflag") {
		t.Fatalf("expected x's drop flag to be armed (store i1 true, i1* %%x.dropflag):\n%s", body)
	}
	// The pre-fix spurious clear: the arming store immediately followed by a
	// clearing store of the SAME flag. Its absence is the isUserIndexExpr fix.
	spurious := "store i1 true, i1* %x.dropflag\n\tstore i1 false, i1* %x.dropflag"
	if strings.Contains(body, spurious) {
		t.Errorf("x's drop flag is armed then immediately cleared — the T0647 "+
			"borrow-RHS over-clear; owned `[]` string return leaks:\n%s", body)
	}
	// Positive: x's scope-exit drop reads x.dropflag and drops the string.
	if !strings.Contains(body, "load i1, i1* %x.dropflag") ||
		!strings.Contains(body, "call void @promise_string_drop(") {
		t.Errorf("expected x's scope-exit string drop wired (load %%x.dropflag + "+
			"@promise_string_drop):\n%s", body)
	}
}

// TestT0647_NativeVectorIndexNotOverTracked — over-tracking guard. The fix is
// scoped to genMethodIndex (user-defined non-native `[]` only). Native
// `Vector[string]` indexing (genVectorIndex) returns a *borrowed* alias into
// the vector's buffer and must NOT be wrapped in a stmt-temp / dropped at the
// call site — doing so would double-free the element the vector still owns.
// The only promise_string_drop calls in the caller must be the scope-exit
// Vector element drop walk (vecdrop.*), never a stmt-temp (tmp.exec) for the
// index result.
func TestT0647_NativeVectorIndexNotOverTracked(t *testing.T) {
	ir := generateIR(t, `
		caller() { v := ["a", "bb"]; n := v[1].len; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if strings.Contains(body, "tmp.exec") || strings.Contains(body, "\ntmp.drop") {
		t.Errorf("native Vector[string] index result must NOT be tracked as a "+
			"stmt-temp (borrowed alias; over-tracking → double-free):\n%s", body)
	}
	// Sanity: native index goes through genVectorIndex (index.ok bounds block),
	// confirming this exercises the native path the fix must leave untouched.
	if !strings.Contains(body, "index.ok") {
		t.Fatalf("expected native vector index.ok bounds block in caller:\n%s", body)
	}
}

// TestT0647_GenericUserIndexStringTempTracked — the fix is independent of
// generics: a generic owner `GBox[T]` instantiated at `string` routes its
// `[](int i) T` read through the same genMethodIndex tail, so the owned
// string return is tracked identically to the non-generic case.
func TestT0647_GenericUserIndexStringTempTracked(t *testing.T) {
	ir := generateIR(t, `
		type GBox[T] { Vector[T] d; [](int i) T { return this.d[i]; } }
		caller() { g := GBox[string](d: ["a", "bb"]); n := g[1].len; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, `@"GBox[string].[]"`) {
		t.Fatalf("expected monomorphized user [] call @\"GBox[string].[]\":\n%s", body)
	}
	tmpExec := blockByPrefixT0638(body, "tmp.exec")
	if tmpExec == "" || !strings.Contains(tmpExec, "call void @promise_string_drop(") {
		t.Errorf("expected the owned generic `[]` string return to be tracked and "+
			"dropped at statement end (tmp.exec → @promise_string_drop):\n%s", body)
	}
}

// TestT0647_UserIndexArcHandleTracked — trackUserIndexResult's i8* native-
// handle dispatch (the AsArc branch → trackTempWithDrop(getOrCreateArcDrop)).
// The original T0647 suite only covered string/vector/heap-user/map; the
// Arc/Weak/Mutex/Task/MutexGuard branches it mirrored from the *ast.CallExpr
// path (T0555/T0561) had NO coverage. Arc/Weak ARE reachable (not single-owner
// handles); a user `[](int i) Arc[int]` returning an owned clone must register
// the result as a tracked stmt-temp and Arc-drop it at statement end via
// @"Arc[int].drop" (getOrCreateArcDrop) — pre-fix the `[]` result was never
// tracked → 1 leaked Arc cell per call. In __user.caller, @"Arc[int].drop"
// uniquely denotes the tracked operator-return temp (the ArcBox receiver's own
// Arc field is dropped inside @ArcBox.drop, a separate function — never inlined
// here). NOTE: the Mutex/Task/MutexGuard analogues of this branch are
// unreachable until T0650 (the ownership pass wrongly rejects a user `[]`
// returning a single-owner handle), so only Arc is locked structurally here;
// runtime 0-leak for Arc+Weak is enforced by tests/e2e/
// user_index_heap_return_test.pr.
func TestT0647_UserIndexArcHandleTracked(t *testing.T) {
	ir := generateIR(t, `
		type ArcBox { Arc[int] a; [](int i) Arc[int] { return this.a.clone(); } }
		caller() { b := ArcBox(a: Arc[int](7)); n := b[0].borrow; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, `@"ArcBox.[]"`) {
		t.Fatalf("expected user-defined [] call @\"ArcBox.[]\" in caller:\n%s", body)
	}
	// AsArc branch fired: the owned operator-return Arc is tracked and dropped
	// via @"Arc[int].drop" (getOrCreateArcDrop) in a stmt-temp cleanup block
	// *after* the `[]` call. Anchor on the post-call slice: the ArcBox-ctor
	// argument temp `Arc[int](7)` emits its own @"Arc[int].drop" in an
	// err.tmp.exec error-unwind block *before* the call (so a whole-body
	// ordering check is unreliable); after the call, @"Arc[int].drop" uniquely
	// denotes the tracked operator-return temp (the receiver `b` is dropped via
	// @ArcBox.drop — a separate function, never inlined here).
	callIdx := strings.Index(body, `@"ArcBox.[]"`)
	if callIdx < 0 {
		t.Fatalf("expected @\"ArcBox.[]\" call in caller:\n%s", body)
	}
	post := body[callIdx:]
	if !strings.Contains(post, "tmp.exec") {
		t.Errorf("expected a tmp.exec stmt-temp cleanup block after the `[]` call "+
			"for the owned Arc return (trackUserIndexResult AsArc); none found "+
			"(pre-fix leak):\n%s", body)
	}
	if !strings.Contains(post, `call void @"Arc[int].drop"(`) {
		t.Errorf("expected the tracked `[]` Arc return to be dropped via "+
			"@\"Arc[int].drop\" (getOrCreateArcDrop) after the call; none found "+
			"(pre-fix the `[]` result was never tracked → leaked Arc cell):\n%s", body)
	}
}
