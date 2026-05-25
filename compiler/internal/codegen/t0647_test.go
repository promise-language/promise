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
// here). The Mutex/Task/MutexGuard analogues of this branch are now reachable
// (T0650 added the ownership exemption) and are locked structurally by
// TestT0650_UserIndex{Mutex,Task,MutexGuard}HandleTracked; runtime 0-leak for
// Arc/Weak/Mutex/Task/MutexGuard is enforced by tests/e2e/
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

// === T0650: single-owner native handle (Mutex/Task/MutexGuard) returned by a
// user-defined non-native `[]` ===
//
// trackUserIndexResult's AsMutex/AsTask/AsMutexGuard branches (mirrored from
// the *ast.CallExpr T0555/T0561 path) were *unreachable dead code*: the
// ownership pass rejected every program with a user `[]` returning a
// single-owner handle before codegen ran (T0650). With the ownership exemption
// (isUserIndexExpr in ownership/expr.go) those branches are now live; these
// tests lock the IR signature. Runtime 0-leak / no-double-free is enforced by
// the batch tests in tests/e2e/user_index_heap_return_test.pr.

// Mutex[int] returned by a user `[]`, used as a temporary (fn-arg). The fresh
// Mutex must be registered as a tracked stmt-temp and dropped at statement end
// via @"Mutex[int].drop" (getOrCreateMutexDrop). MtxBox has no Mutex field, so
// in __user.caller @"Mutex[int].drop" uniquely denotes the tracked operator
// return (the @"MtxBox.[]" body is a separate function, never inlined here).
func TestT0650_UserIndexMutexHandleTracked(t *testing.T) {
	ir := generateIR(t, `
		type MtxBox { int n; [](int i) Mutex[int] { return Mutex[int](this.n + i); } }
		take_mtx(Mutex[int] m) {}
		caller() { b := MtxBox(n: 100); take_mtx(b[0]); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, `@"MtxBox.[]"`) {
		t.Fatalf("expected user-defined [] call @\"MtxBox.[]\" in caller:\n%s", body)
	}
	callIdx := strings.Index(body, `@"MtxBox.[]"`)
	post := body[callIdx:]
	if !strings.Contains(post, "tmp.exec") {
		t.Errorf("expected a tmp.exec stmt-temp cleanup block after the `[]` call "+
			"for the owned Mutex return (trackUserIndexResult AsMutex); none found "+
			"(pre-fix this branch was unreachable — ownership rejected first):\n%s", body)
	}
	if !strings.Contains(post, `call void @"Mutex[int].drop"(`) {
		t.Errorf("expected the tracked `[]` Mutex return to be dropped via "+
			"@\"Mutex[int].drop\" (getOrCreateMutexDrop) after the call; none found:\n%s", body)
	}
}

// Task[int] returned by a user `[]` (go-spawned). The fresh Task handle is a
// tracked stmt-temp dropped via @"Task[int].drop" (getOrCreateTaskDrop) at
// statement end. (TaskBox has no Task field — the only Task drop is the
// tracked operator return.)
func TestT0650_UserIndexTaskHandleTracked(t *testing.T) {
	ir := generateIR(t, `
		worker_t0650() int { return 42; }
		type TaskBox { [](int i) Task[int] { return go worker_t0650(); } }
		caller() { tb := TaskBox(); r := <-tb[0]; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, `@"TaskBox.[]"`) {
		t.Fatalf("expected user-defined [] call @\"TaskBox.[]\" in caller:\n%s", body)
	}
	callIdx := strings.Index(body, `@"TaskBox.[]"`)
	post := body[callIdx:]
	if !strings.Contains(post, "tmp.exec") {
		t.Errorf("expected a tmp.exec stmt-temp cleanup block after the `[]` call "+
			"for the owned Task return (trackUserIndexResult AsTask):\n%s", body)
	}
	if !strings.Contains(post, `call void @"Task[int].drop"(`) {
		t.Errorf("expected the tracked `[]` Task return to be dropped via "+
			"@\"Task[int].drop\" (getOrCreateTaskDrop) after the call; none found:\n%s", body)
	}
}

// MutexGuard[int] returned by a user `[]` (`this.m.lock()`), used as a
// temporary (`.borrow` getter read leaves the guard un-bound). The guard is a
// tracked stmt-temp dropped via the single @MutexGuard.drop symbol (T0561) at
// statement end. NOTE: @"Mutex[int].drop" appears *before* the @"MgBox.[]"
// call here (the MgBox-ctor `Mutex[int](42)` arg temp), so anchor strictly on
// the post-`[]`-call slice — there @MutexGuard.drop uniquely denotes the
// tracked operator return.
func TestT0650_UserIndexMutexGuardHandleTracked(t *testing.T) {
	ir := generateIR(t, `
		type MgBox { Mutex[int] m; [](int i) MutexGuard[int] { return this.m.lock(); } }
		caller() { mgb := MgBox(m: Mutex[int](42)); n := mgb[0].borrow; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, `@"MgBox.[]"`) {
		t.Fatalf("expected user-defined [] call @\"MgBox.[]\" in caller:\n%s", body)
	}
	callIdx := strings.Index(body, `@"MgBox.[]"`)
	post := body[callIdx:]
	if !strings.Contains(post, "tmp.exec") {
		t.Errorf("expected a tmp.exec stmt-temp cleanup block after the `[]` call "+
			"for the owned MutexGuard return (trackUserIndexResult AsMutexGuard):\n%s", body)
	}
	if !strings.Contains(post, "call void @MutexGuard.drop(") {
		t.Errorf("expected the tracked `[]` MutexGuard return to be dropped via "+
			"@MutexGuard.drop (T0561 single symbol) after the call; none found:\n%s", body)
	}
}

// Binding path: `m := b[1]` must claim the tracked Mutex temp into `m` and KEEP
// m's drop binding armed (m's scope-exit drop frees the owned Mutex exactly
// once). The pre-fix borrow-RHS over-clear (isStringBorrowExpr treats every
// IndexExpr as a container borrow) would arm then immediately clear
// %m.dropflag. The isUserIndexExpr exemption (codegen/stmt.go) removes that
// spurious clear; this guards the Mutex analogue of
// TestT0647_UserIndexStringBindKeepsDropFlag.
func TestT0650_UserIndexMutexBindKeepsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type MtxBox { int n; [](int i) Mutex[int] { return Mutex[int](this.n + i); } }
		caller() { b := MtxBox(n: 100); m := b[1]; use g := m.lock(); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, `@"MtxBox.[]"`) {
		t.Fatalf("expected user-defined [] call @\"MtxBox.[]\" in caller:\n%s", body)
	}
	if !strings.Contains(body, "store i1 true, i1* %m.dropflag") {
		t.Fatalf("expected m's drop flag to be armed (store i1 true, i1* %%m.dropflag):\n%s", body)
	}
	// The pre-fix spurious clear: m's flag armed then immediately cleared.
	// Its absence is the isUserIndexExpr exemption.
	spurious := "store i1 true, i1* %m.dropflag\n\tstore i1 false, i1* %m.dropflag"
	if strings.Contains(body, spurious) {
		t.Errorf("m's drop flag is armed then immediately cleared — the borrow-RHS "+
			"over-clear; owned `[]` Mutex return leaks:\n%s", body)
	}
	// Positive: m's scope-exit drop reads m.dropflag and drops the Mutex.
	if !strings.Contains(body, "load i1, i1* %m.dropflag") ||
		!strings.Contains(body, `call void @"Mutex[int].drop"(`) {
		t.Errorf("expected m's scope-exit Mutex drop wired (load %%m.dropflag + "+
			"@\"Mutex[int].drop\"):\n%s", body)
	}
}
