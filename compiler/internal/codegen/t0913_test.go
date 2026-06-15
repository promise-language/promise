package codegen

import (
	"strings"
	"testing"
)

// T0913: Reassigning a heap-capturing closure local must free the old env struct
// before overwriting the fat pointer. Closure locals register bindingFreeEnv (not
// dropBindings), so the old drop-old path never fired for them — the env was leaked.
// The fix adds a T0913 branch in genAssignStmt that calls emitEnvFree on the
// matching bindingFreeEnv when the LHS is a Signature type.

// emitEnvFree emits: env.free (flag check) → env.free.call (null check) →
// env.deep_drop / env.shallow_free → env.skip.
// We probe for these block labels in the function's IR.

func fnBodyT0913(t *testing.T, ir, fn string) string {
	t.Helper()
	// Functions compiled without a goroutine context use @__user.<name>.
	defMark := "define void @__user." + fn + "("
	defStart := strings.Index(ir, defMark)
	if defStart < 0 {
		t.Fatalf("function __user.%s not in IR\nfull IR:\n%s", fn, ir)
	}
	defEnd := strings.Index(ir[defStart:], "\n}\n")
	if defEnd < 0 {
		t.Fatalf("could not find end of __user.%s\n", fn)
	}
	return ir[defStart : defStart+defEnd+2]
}

// TestT0913ReassignEmitsEnvFree: reassigning a heap-capturing closure must emit
// the env.free / env.free.call / env.deep_drop / env.shallow_free blocks.
func TestT0913ReassignEmitsEnvFree(t *testing.T) {
	ir := generateIR(t, `
		reassign_heap_t0913() {
			s1 := "captured_one";
			() -> int f = move || -> s1.len;
			s2 := "two";
			f = move || -> s2.len;
		}
	`)
	body := fnBodyT0913(t, ir, "reassign_heap_t0913")
	if !strings.Contains(body, "env.free") {
		t.Errorf("expected env.free block for closure reassignment\nbody:\n%s", body)
	}
	if !strings.Contains(body, "env.free.call") {
		t.Errorf("expected env.free.call null-guard block for closure reassignment\nbody:\n%s", body)
	}
	// env.deep_drop (has captured drop fn) or env.shallow_free (pal_free) must appear
	if !strings.Contains(body, "env.deep_drop") && !strings.Contains(body, "env.shallow_free") {
		t.Errorf("expected env.deep_drop or env.shallow_free for closure env release\nbody:\n%s", body)
	}
}

// TestT0913SelfAssignSingleEnvFree: self-assignment (f = f) must NOT emit an extra
// reassignment-site env.free — only the scope-exit env.free should be present.
// If T0913 fires for self-assign it would emit a second env.free.call block,
// free the env, then store a dangling pointer → UAF on the next call.
// The Promise test reassign_self() would crash, but this IR test catches it
// structurally: exactly one env.free.call block (scope-exit), not two.
func TestT0913SelfAssignSingleEnvFree(t *testing.T) {
	ir := generateIR(t, `
		self_assign_t0913() {
			s := "hello";
			() -> int f = move || -> s.len;
			f = f;
		}
	`)
	body := fnBodyT0913(t, ir, "self_assign_t0913")
	// Count env.free.call block *definitions* (label ending in ':').
	// Self-assign returns early in genAssignStmt, so only the scope-exit
	// cleanup emits env.free.call — exactly one definition.
	count := strings.Count(body, "env.free.call.")
	// Each block appears twice in IR text: once as a branch target and once as
	// the label definition. So "1 block" → count == 2.
	if count != 2 {
		t.Errorf("expected exactly 1 env.free.call block (scope-exit only, appears twice in IR), got count=%d\nbody:\n%s", count, body)
	}
}

// TestT0913NonCapturingReassignNullGuard: reassigning a non-capturing closure
// (null env) must not crash. emitEnvFree emits a null guard that branches to
// env.skip when the env pointer is null — the env.free.call block is still emitted
// but the null check prevents any actual free. Verify both the null check and the
// env.skip branch are present.
func TestT0913NonCapturingReassignNullGuard(t *testing.T) {
	ir := generateIR(t, `
		no_capture_reassign_t0913() {
			() -> int f = || -> 1;
			f = || -> 2;
		}
	`)
	body := fnBodyT0913(t, ir, "no_capture_reassign_t0913")
	// The reassignment T0913 path fires (bindingFreeEnv registered for f even
	// for non-capturing closures), emitting env.free + null guard → env.skip.
	if !strings.Contains(body, "env.free") {
		t.Errorf("expected env.free block even for non-capturing closure reassignment\nbody:\n%s", body)
	}
	// The null guard must redirect to env.skip (not free null).
	if !strings.Contains(body, "env.skip") {
		t.Errorf("expected env.skip block (null guard) for non-capturing closure reassignment\nbody:\n%s", body)
	}
}
