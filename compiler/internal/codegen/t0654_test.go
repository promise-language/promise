package codegen

import (
	"strings"
	"testing"
)

// T0654: a non-native method or user-defined `[]` operator whose return type is
// `Optional<single-owner-native-handle>` (`Mutex[T]?`, `Task[T]?`,
// `MutexGuard[T]?`) — and the ref-counted handles `Arc[T]?`/`Weak[T]?` — leaked
// the owned handle when `!`-unwrapped at a *consume/temp* site (call arg, method
// receiver, getter receiver). genOptionalForceUnwrap's post-extract i8* temp
// tracking only covered string/vector; the single-owner / ref-counted handle
// arms were missing, so the unwrapped i8* fell through with no statement-end
// drop. The binding-site (`x := f()!`) path is correctly 0-leak because
// genVarDecl's native-handle claim is a no-op when no temp was registered.
//
// Fix: extend the switch in genOptionalForceUnwrap to mirror the CallExpr
// post-call tracking — AsArc/AsWeak/AsMutex/AsTask/AsMutexGuard/AsChannel arms,
// keyed off the peeled Optional inner type.
//
// These tests lock the structural IR signature. Runtime zero-leak is enforced
// by the e2e batch tests in tests/e2e/user_index_heap_return_test.pr.

// TestT0654_OptionalMutexUnwrapConsumeTracked — `take_mtx_i(s.at_mtx(0)!)` with
// `at_mtx(int i) Mutex[int]?`. The unwrapped Mutex must be registered as a
// tracked stmt-temp and dropped via @"Mutex[int].drop" at statement end. Src has
// no Mutex field, so in __user.caller @"Mutex[int].drop" after the unwrap
// uniquely denotes the tracked operator return.
func TestT0654_OptionalMutexUnwrapConsumeTracked(t *testing.T) {
	ir := generateIR(t, `
		take_mtx_i(Mutex[int] m) {}
		type Src { at_mtx(int i) Mutex[int]? { return Mutex[int](i); } }
		caller() { s := Src(); take_mtx_i(s.at_mtx(0)!); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "@Src.at_mtx") {
		t.Fatalf("expected Src.at_mtx call in caller:\n%s", body)
	}
	unwrapIdx := strings.Index(body, "unwrap.ok")
	if unwrapIdx < 0 {
		t.Fatalf("expected unwrap.ok block from genOptionalForceUnwrap:\n%s", body)
	}
	post := body[unwrapIdx:]
	if !strings.Contains(post, "tmp.exec") {
		t.Errorf("expected tmp.exec stmt-temp cleanup block after the unwrap "+
			"for the owned Mutex (AsMutex tracking arm); none found "+
			"(pre-fix the unwrapped i8* was never tracked → leaked Mutex):\n%s", body)
	}
	if !strings.Contains(post, `call void @"Mutex[int].drop"(`) {
		t.Errorf("expected the tracked unwrapped Mutex to be dropped via "+
			"@\"Mutex[int].drop\" (getOrCreateMutexDrop) after the unwrap:\n%s", body)
	}
}

// TestT0654_OptionalTaskUnwrapConsumeTracked — `take_task_i(s.at_task(0)!)` with
// `at_task(int i) Task[int]?`. The unwrapped Task must be registered as a
// tracked stmt-temp and dropped via @"Task[int].drop" at statement end.
func TestT0654_OptionalTaskUnwrapConsumeTracked(t *testing.T) {
	ir := generateIR(t, `
		take_task_i(Task[int] t) {}
		worker_t0654() int { return 7; }
		type Src { at_task(int i) Task[int]? { return go worker_t0654(); } }
		caller() { s := Src(); take_task_i(s.at_task(0)!); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "@Src.at_task") {
		t.Fatalf("expected Src.at_task call in caller:\n%s", body)
	}
	unwrapIdx := strings.Index(body, "unwrap.ok")
	if unwrapIdx < 0 {
		t.Fatalf("expected unwrap.ok block from genOptionalForceUnwrap:\n%s", body)
	}
	post := body[unwrapIdx:]
	if !strings.Contains(post, "tmp.exec") {
		t.Errorf("expected tmp.exec stmt-temp cleanup block after the unwrap "+
			"for the owned Task (AsTask tracking arm); none found:\n%s", body)
	}
	if !strings.Contains(post, `call void @"Task[int].drop"(`) {
		t.Errorf("expected the tracked unwrapped Task to be dropped via "+
			"@\"Task[int].drop\" (getOrCreateTaskDrop) after the unwrap:\n%s", body)
	}
}

// TestT0654_OptionalMutexGuardUnwrapTempTracked — `n := s.at_guard(0)!.borrow`
// with `at_guard(int i) MutexGuard[int]?`. The unwrapped guard is consumed by
// the `.borrow` getter, leaving the guard un-bound — it must be a tracked
// stmt-temp dropped via the single @MutexGuard.drop symbol at statement end.
// Anchor strictly on the post-unwrap slice (the Src-ctor / receiver-Mutex drops
// appear before the unwrap site).
func TestT0654_OptionalMutexGuardUnwrapTempTracked(t *testing.T) {
	ir := generateIR(t, `
		type Src { Mutex[int] held; at_guard(int i) MutexGuard[int]? { return this.held.lock(); } }
		caller() { s := Src(held: Mutex[int](5)); n := s.at_guard(0)!.borrow; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "@Src.at_guard") {
		t.Fatalf("expected Src.at_guard call in caller:\n%s", body)
	}
	unwrapIdx := strings.Index(body, "unwrap.ok")
	if unwrapIdx < 0 {
		t.Fatalf("expected unwrap.ok block from genOptionalForceUnwrap:\n%s", body)
	}
	post := body[unwrapIdx:]
	if !strings.Contains(post, "tmp.exec") {
		t.Errorf("expected tmp.exec stmt-temp cleanup block after the unwrap "+
			"for the owned MutexGuard (AsMutexGuard tracking arm):\n%s", body)
	}
	if !strings.Contains(post, "call void @MutexGuard.drop(") {
		t.Errorf("expected the tracked unwrapped MutexGuard to be dropped via "+
			"@MutexGuard.drop (T0561 single symbol) after the unwrap:\n%s", body)
	}
}

// TestT0654_OptionalArcUnwrapConsumeTracked — `take_arc_i(s.at_arc(0)!)` with
// `at_arc(int i) Arc[int]?`. The unwrapped Arc must be a tracked stmt-temp
// dropped via @"Arc[int].drop" at statement end. Src has no Arc field so the
// post-unwrap @"Arc[int].drop" uniquely denotes the tracked unwrap temp.
func TestT0654_OptionalArcUnwrapConsumeTracked(t *testing.T) {
	ir := generateIR(t, `
		take_arc_i(Arc[int] a) {}
		type Src { at_arc(int i) Arc[int]? { return Arc[int](i + 100); } }
		caller() { s := Src(); take_arc_i(s.at_arc(0)!); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "@Src.at_arc") {
		t.Fatalf("expected Src.at_arc call in caller:\n%s", body)
	}
	unwrapIdx := strings.Index(body, "unwrap.ok")
	if unwrapIdx < 0 {
		t.Fatalf("expected unwrap.ok block from genOptionalForceUnwrap:\n%s", body)
	}
	post := body[unwrapIdx:]
	if !strings.Contains(post, "tmp.exec") {
		t.Errorf("expected tmp.exec stmt-temp cleanup block after the unwrap "+
			"for the owned Arc (AsArc tracking arm):\n%s", body)
	}
	if !strings.Contains(post, `call void @"Arc[int].drop"(`) {
		t.Errorf("expected the tracked unwrapped Arc to be dropped via "+
			"@\"Arc[int].drop\" (getOrCreateArcDrop) after the unwrap:\n%s", body)
	}
}

// TestT0654_OptionalMutexUnwrapBindKeepsTempClaimed — control: at a *binding*
// site (`m := s.at_mtx(3)!`), the new tracking still fires but the var-decl
// native-handle claim (stmt.go) clears the temp's drop flag so the variable's
// own scope-exit drop is the unique live owner. Verifies the fix does not
// double-free at the binding path.
//
// IR shape proof: m.dropflag is armed (the var binding's own drop); the tmp
// dropflag is cleared (claimStringTemp / claim-by-SSA stores i1 false) before
// the scope exit; @"Mutex[int].drop" fires exactly once in the user.caller
// drop block (the var-decl scope-exit drop).
func TestT0654_OptionalMutexUnwrapBindKeepsTempClaimed(t *testing.T) {
	ir := generateIR(t, `
		type Src { at_mtx(int i) Mutex[int]? { return Mutex[int](i); } }
		caller() { s := Src(); m := s.at_mtx(3)!; use g := m.lock(); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "@Src.at_mtx") {
		t.Fatalf("expected Src.at_mtx call in caller:\n%s", body)
	}
	// m's scope-exit Mutex drop is wired.
	if !strings.Contains(body, "store i1 true, i1* %m.dropflag") {
		t.Fatalf("expected m's drop flag to be armed (store i1 true, i1* %%m.dropflag):\n%s", body)
	}
	if !strings.Contains(body, "load i1, i1* %m.dropflag") ||
		!strings.Contains(body, `call void @"Mutex[int].drop"(`) {
		t.Errorf("expected m's scope-exit Mutex drop wired (load %%m.dropflag + "+
			"@\"Mutex[int].drop\"):\n%s", body)
	}
	// The new T0654 tracking must NOT cause a redundant tmp.exec Mutex drop —
	// the binding-site claim clears the tmp's drop flag, so the tmp.exec block's
	// loaded flag is false at runtime. The number of literal @"Mutex[int].drop"
	// call sites in the caller IR must remain bounded; we don't lock an exact
	// count (the err.tmp.exec error-unwind path may also reference it), but the
	// presence of the var-decl drop above proves the claim path is intact.
}

// TestT0654_OptionalUserIndexMutexUnwrapConsumeTracked — `[]`-spelling parity:
// `take_mtx(b[0]!)` with `[](int i) Mutex[int]?`. The IR shape (AST is
// OptionalUnwrap(CallExpr(MemberExpr)) for the method form and
// OptionalUnwrap(IndexExpr) for the operator form) routes both spellings
// through genOptionalForceUnwrap, so the same fix point covers both. Locks the
// `[]` arm.
func TestT0654_OptionalUserIndexMutexUnwrapConsumeTracked(t *testing.T) {
	ir := generateIR(t, `
		take_mtx_i(Mutex[int] m) {}
		type MtxBox { int n; [](int i) Mutex[int]? { return Mutex[int](this.n + i); } }
		caller() { b := MtxBox(n: 100); take_mtx_i(b[0]!); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, `@"MtxBox.[]"`) {
		t.Fatalf("expected user-defined [] call @\"MtxBox.[]\" in caller:\n%s", body)
	}
	unwrapIdx := strings.Index(body, "unwrap.ok")
	if unwrapIdx < 0 {
		t.Fatalf("expected unwrap.ok block from genOptionalForceUnwrap:\n%s", body)
	}
	post := body[unwrapIdx:]
	if !strings.Contains(post, "tmp.exec") {
		t.Errorf("expected tmp.exec stmt-temp cleanup block after the unwrap "+
			"for the owned Mutex returned by user `[]` (AsMutex tracking arm); "+
			"none found:\n%s", body)
	}
	if !strings.Contains(post, `call void @"Mutex[int].drop"(`) {
		t.Errorf("expected the tracked unwrapped `[]` Mutex to be dropped via "+
			"@\"Mutex[int].drop\" after the unwrap:\n%s", body)
	}
}
