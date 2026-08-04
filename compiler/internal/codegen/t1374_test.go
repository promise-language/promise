package codegen

import (
	"strings"
	"testing"
)

// T1374: a non-generic concrete type that implements a generic `structural`
// interface defined in a *module* (std's Iterator[int]) and is dispatched through
// the interface view had every inherited-default vtable slot left null. The
// mono pipeline skips structural instances, and the T0862 declare path only
// searched the *main* file for the interface's method decls — so for a module
// interface no per-concrete default stub was declared before the vtable was
// built, and each combinator slot became `i8* null`. Calling e.g. `it.count()`
// through the view then loaded the null slot and jumped to address 0.
//
// The fix makes declareGenericStructuralDefaults resolve the interface's decls
// from whichever file (main or module) defines it, mirroring the already
// module-aware define path. This IR-shape test locks the vtable down: every slot
// of the concrete type's vtable must reference a synthesized `<Concrete>.<method>`
// and none may be null (closes T1060, the requested IR-shape regression).
func TestT1374ModuleIteratorVtableHasNoNullSlots(t *testing.T) {
	ir := generateIR(t, `
		type UpTo is Iterator[int] {
			int cur;
			int limit;
			next(~this) int? {
				if this.cur >= this.limit { return none; }
				int v = this.cur;
				this.cur = this.cur + 1;
				return v;
			}
		}
		main() {
			UpTo u = UpTo(cur: 0, limit: 5);
			Iterator[int] it = u;
			it.count();
		}
	`)

	vtable := extractGlobal(ir, "promise_vtable_UpTo")
	if vtable == "" {
		t.Fatal("expected @promise_vtable_UpTo to be emitted")
	}
	// The pre-fix bug left inherited-default combinator slots null; dispatch
	// through the Iterator[int] view then jumped to address 0.
	if strings.Contains(vtable, "i8* null") {
		t.Errorf("promise_vtable_UpTo has a null slot (T1374 regression):\n%s", vtable)
	}
	// The concrete's own abstract impl plus every synthesized module default must
	// be referenced by the concrete name (UpTo.*), not a skipped generic instance
	// name (Iterator__int.*) and not left null.
	for _, m := range []string{
		"@UpTo.next",   // user-provided abstract impl
		"@UpTo.filter", // closure-returning intermediate combinator
		"@UpTo.take",
		"@UpTo.count",
		"@UpTo.collect",
		"@UpTo.find",  // T?-returning terminal
		"@UpTo.first", // T?-returning terminal
	} {
		assertContains(t, vtable, m)
	}
	// The generic instance methods must NOT be what the vtable points at — the
	// mono pipeline never emits them for a structural instance.
	assertNotContains(t, vtable, "@Iterator__int.count")
}

// T1374 (direct + view parity): the same synthesized default must be callable
// both directly on the concrete and through the interface view. Emitting the
// concrete default function is what fills the vtable slot the view dispatches on.
func TestT1374ModuleIteratorDefaultSynthesized(t *testing.T) {
	ir := generateIR(t, `
		type UpTo is Iterator[int] {
			int cur;
			int limit;
			next(~this) int? {
				if this.cur >= this.limit { return none; }
				int v = this.cur;
				this.cur = this.cur + 1;
				return v;
			}
		}
		main() {
			Iterator[int] it = UpTo(cur: 0, limit: 5);
			it.count();
		}
	`)
	if extractFunction(ir, "UpTo.count") == "" {
		t.Error("expected synthesized UpTo.count (module Iterator default) to be emitted")
	}
	// count() iterates via this.next(); the synthesized body must dispatch to the
	// concrete's own next (correct int? = { i1, i64 } shape), not a generic stub.
	countFn := extractFunction(ir, "UpTo.count")
	assertContains(t, countFn, "@UpTo.next")
}
