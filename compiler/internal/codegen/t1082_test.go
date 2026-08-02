package codegen

import (
	"strings"
	"testing"
)

// T1082 (under T0806): lock Fix A's branch behavior in genOptionalForceUnwrap
// at the IR level — the native-handle (Mutex[T]?/Task[T]?) statement-temp
// tracking is SKIPPED for an owner-governed member source (owner's drop frees
// the handle; tracking would double-free) but STILL FIRES for a no-owner call
// source (the extracted handle has no owner → must be freed at statement end).
// Runtime zero-leak/double-free is covered by
// tests/concurrency/t0806_optional_handle_field_unwrap_test.pr; this locks the
// IR shape so a codegen regression fails at the test layer, not by allocator luck.

const t1082Decls = `
	type MtxHolder { Mutex[int]? mtx; drop(~this) {} }
	type TskHolder { Task[int]? tsk; drop(~this) {} }
	worker_t1082() int { return 42; }
	make_opt_mutex() Mutex[int]? { return Mutex[int](11); }
	make_opt_task() Task[int]? { return go worker_t1082(); }
	take_task_t1082(Task[int] t) {}
`

// Owner-governed member source `(h.mtx!)` on a borrowed owner: the owner's drop
// governs the handle, so genOptionalForceUnwrap must NOT register a Mutex temp.
func TestT1082_ForceUnwrapMutexMemberSkipsTrack(t *testing.T) {
	ir := generateIR(t, t1082Decls+`
		fn_mtx(MtxHolder h) int { return (h.mtx!).lock().borrow; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.fn_mtx")
	if fn == "" {
		t.Fatalf("could not extract __user.fn_mtx:\n%s", ir)
	}
	if !strings.Contains(fn, "unwrap.ok") {
		t.Fatalf("expected an unwrap.ok block (force-unwrap actually ran):\n%s", fn)
	}
	if strings.Contains(fn, `call void @"Mutex[int].drop"(`) {
		t.Fatalf("owner-governed member force-unwrap must SKIP the statement-temp "+
			"track — no @\"Mutex[int].drop\" in the borrowing fn (owner frees it):\n%s", fn)
	}
}

// Same for a Task member source `(h.tsk!)`.
func TestT1082_ForceUnwrapTaskMemberSkipsTrack(t *testing.T) {
	ir := generateIR(t, t1082Decls+`
		fn_tsk(TskHolder h) int { return <-(h.tsk!); }
		main() {}
	`)
	fn := extractFunction(ir, "__user.fn_tsk")
	if fn == "" {
		t.Fatalf("could not extract __user.fn_tsk:\n%s", ir)
	}
	if !strings.Contains(fn, "unwrap.ok") {
		t.Fatalf("expected an unwrap.ok block:\n%s", fn)
	}
	if strings.Contains(fn, `call void @"Task[int].drop"(`) {
		t.Fatalf("owner-governed member force-unwrap must SKIP the Task temp track:\n%s", fn)
	}
}

// No-owner control: a free-function call source `(make_opt_mutex()!)` has no
// owner governing the handle, so the AsMutex tracking arm MUST fire.
func TestT1082_ForceUnwrapMutexCallSourceTracks(t *testing.T) {
	ir := generateIR(t, t1082Decls+`
		caller() int { return (make_opt_mutex()!).lock().borrow; }
		main() { _ := caller(); }
	`)
	fn := extractFunction(ir, "__user.caller")
	if fn == "" {
		t.Fatalf("could not extract __user.caller:\n%s", ir)
	}
	idx := strings.Index(fn, "unwrap.ok")
	if idx < 0 {
		t.Fatalf("expected unwrap.ok block:\n%s", fn)
	}
	post := fn[idx:]
	if !strings.Contains(post, "tmp.exec") ||
		!strings.Contains(post, `call void @"Mutex[int].drop"(`) {
		t.Fatalf("call-source unwrap must TRACK the handle (tmp.exec + "+
			"@\"Mutex[int].drop\" after the unwrap):\n%s", fn)
	}
}

// No-owner Task call source consumed as a call arg — AsTask tracking arm fires.
func TestT1082_ForceUnwrapTaskCallSourceTracks(t *testing.T) {
	ir := generateIR(t, t1082Decls+`
		caller() { take_task_t1082(make_opt_task()!); }
		main() { caller(); }
	`)
	fn := extractFunction(ir, "__user.caller")
	if fn == "" {
		t.Fatalf("could not extract __user.caller:\n%s", ir)
	}
	idx := strings.Index(fn, "unwrap.ok")
	if idx < 0 {
		t.Fatalf("expected unwrap.ok block:\n%s", fn)
	}
	post := fn[idx:]
	if !strings.Contains(post, "tmp.exec") ||
		!strings.Contains(post, `call void @"Task[int].drop"(`) {
		t.Fatalf("call-source Task unwrap must TRACK (tmp.exec + "+
			"@\"Task[int].drop\" after the unwrap):\n%s", fn)
	}
}

// Paren-peel guard: `((h.mtx))!` must be recognized like the bare member form,
// locking the ParenExpr peel loop in isOwnerGovernedMemberOptionalUnwrapSource.
func TestT1082_ParenWrappedMemberSkipsTrack(t *testing.T) {
	ir := generateIR(t, t1082Decls+`
		fn_mtx_paren(MtxHolder h) int { return ((h.mtx))!.lock().borrow; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.fn_mtx_paren")
	if fn == "" {
		t.Fatalf("could not extract __user.fn_mtx_paren:\n%s", ir)
	}
	if strings.Contains(fn, `call void @"Mutex[int].drop"(`) {
		t.Fatalf("paren-wrapped member force-unwrap must SKIP the track like the "+
			"bare form:\n%s", fn)
	}
}
