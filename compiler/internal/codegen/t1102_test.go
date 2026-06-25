package codegen

import "testing"

// T1102: Task[T] is a single-owner native handle (opaque G-struct ptr) with a
// registered drop, so it must be treated as droppable by the return-alias check
// (isTypeDroppable). A generic identity instantiated over a task returns its
// borrowed param unchanged, so the caller's result aliases its still-live source
// local — the alias check must fire and clear the source's drop flag (the
// alias.dup/alias.cont runtime guard), otherwise both ends drop the one handle
// (double-free / segfault). Before the fix, isTypeDroppable returned false for
// Task, the check returned early, and no alias guard was emitted.
func TestT1102GenericIdentityTaskEmitsAliasGuard(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		identity[T](T x) T { return x; }
		main() {
			task[int] t = go worker();
			r := <-identity(t);
		}
	`)
	// The user `main` body compiles into the `.goroutine.main` coroutine; the
	// return-alias check for the task-returning call lives there. alias.dup is
	// unique to this aliasing call site, so assert on the whole module IR.
	assertContains(t, ir, "alias.dup")
	assertContains(t, ir, "alias.cont")
}
