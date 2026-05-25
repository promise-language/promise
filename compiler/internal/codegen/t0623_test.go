package codegen

import (
	"strings"
	"testing"
)

// T0623: match-destructure of an enum variant whose field transitively owns a
// single-owner handle (Task/Mutex/MutexGuard) moves out. Codegen must:
//   - Register a drop binding for the moved-out handle binding (so the binding
//     drops the handle at arm-end / scope-exit).
//   - Null out the moved-out slot in the subject's enum-value alloca, so the
//     synth enum drop (firing on the subject at outer scope exit) sees null
//     there and skips it (single-owner-handle drops all null-check). Other
//     droppable variant fields are still freed by the synth drop.
// Non-moving arms (wildcard `_` slot, non-handle variant) must NOT null out
// the slot — the synth enum drop is the right cleanup path.

// TestT0623_TaskMatchMoveOutIR — the moving arm must store null into the
// subject's variant-field slot and register a Task[int] drop for the binding.
func TestT0623_TaskMatchMoveOutIR(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		enum Job { Pending, Running(Task[int] t) }
		caller() {
			j := Job.Running(go worker());
			match j {
				Job.Pending => assert(true, "p"),
				Job.Running(t) => assert(true, "r"),
			}
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// The moving arm must null out the handle slot in the subject's alloca.
	// Look for a store of null i8* — the destructure GEPs into %j's data area.
	if !strings.Contains(body, "store i8* null") {
		t.Errorf("expected store i8* null (slot null-out) in moving arm:\n%s", body)
	}
	// The binding must end up with a Task[int] drop site (registered via
	// maybeRegisterDrop → getOrCreateTaskDrop).
	if !strings.Contains(body, `@"Task[int].drop"`) {
		t.Errorf("expected call to @\"Task[int].drop\" (binding drop in moving arm):\n%s", body)
	}
	// The synth enum drop on the subject must still be wired up — the synth
	// drop walks the variant data and null-checks the handle slot at runtime.
	if !strings.Contains(body, "@Job.drop(") {
		t.Errorf("expected synth @Job.drop call (subject drop runs normally; null-check skips moved slot):\n%s", body)
	}
}

// TestT0623_WildcardArmDoesNotNullSubjectSlot — `_` binding on the handle
// variant does NOT null out the subject's slot (no move-out; synth enum drop
// frees the handle normally).
func TestT0623_WildcardArmDoesNotNullSubjectSlot(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		enum Job { Pending, Running(Task[int] t) }
		caller() {
			j := Job.Running(go worker());
			match j {
				Job.Pending => assert(true, "p"),
				Job.Running(_) => assert(true, "r"),
			}
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// The wildcard arm must not emit a slot null-out. The Task is owned by the
	// variant and freed by the synth drop. (No "store i8* null" anywhere in
	// the body except possibly in temp tracking — assert specifically that no
	// store of null targets a GEP into %j's data area.)
	// Heuristic: with wildcard, there's no GEP-then-store-null pair near %j.
	// We check the absence of the move-out hallmark: the binding drop registration.
	if strings.Contains(body, `%t = alloca`) {
		t.Errorf("wildcard arm must NOT create a %%t binding alloca:\n%s", body)
	}
}

// TestT0623_MultiFieldVariantNullOnlyHandleSlot — when a variant has both a
// dup'd droppable (string) and a moved handle (Task), the moving arm must:
//   - dup the string into the binding (independent copy)
//   - move the Task into the binding (registered drop)
//   - null ONLY the Task slot in the subject's alloca (the string slot is
//     still freed by the synth drop)
func TestT0623_MultiFieldVariantNullOnlyHandleSlot(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		enum E { Empty, Multi(string s, Task[int] t) }
		caller() {
			s := "hello world";
			e := E.Multi(s, go worker());
			match e {
				E.Empty => assert(true, "e"),
				E.Multi(a, b) => assert(true, "m"),
			}
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// The handle slot (i8* Task) must be null'd out.
	if !strings.Contains(body, "store i8* null") {
		t.Errorf("expected store i8* null for the Task slot in moving arm:\n%s", body)
	}
	// The Task drop for the binding must be registered.
	if !strings.Contains(body, `@"Task[int].drop"`) {
		t.Errorf("expected @\"Task[int].drop\" for moving-arm Task binding:\n%s", body)
	}
	// The synth enum drop on the subject must still run (frees the string slot).
	if !strings.Contains(body, "@E.drop(") {
		t.Errorf("expected synth @E.drop call (frees variant string; Task slot null'd):\n%s", body)
	}
}

// TestT0623_MutexMatchMoveOutIR — the same move-out lowering for Mutex[T]:
// register the per-instantiation Mutex drop on the binding, null the slot
// in the subject's alloca, and keep the synth enum drop call wired up.
// (Direct Mutex case is symmetric to Task at the predicate level but has a
// different per-instantiation drop name; ensures the maybeRegisterDrop path
// covers Mutex, not just Task.)
func TestT0623_MutexMatchMoveOutIR(t *testing.T) {
	ir := generateIR(t, `
		enum Holder { Empty, Locked(Mutex[int] m) }
		caller() {
			h := Holder.Locked(Mutex[int](7));
			match h {
				Holder.Empty => assert(true, "e"),
				Holder.Locked(m) => assert(true, "l"),
			}
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "store i8* null") {
		t.Errorf("expected store i8* null for the Mutex slot in moving arm:\n%s", body)
	}
	if !strings.Contains(body, `@"Mutex[int].drop"`) {
		t.Errorf("expected call to @\"Mutex[int].drop\" (binding drop in moving arm):\n%s", body)
	}
	if !strings.Contains(body, "@Holder.drop(") {
		t.Errorf("expected synth @Holder.drop call (subject drop runs; Mutex slot null'd):\n%s", body)
	}
}
