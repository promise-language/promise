package regress10

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1377: sibling to T1374. When a user defines their own generic `structural`
// interface that extends a *module* one (e.g. `MyIter[T] is Iterator[T]`) and a
// concrete type implements the user interface, the inherited-default vtable slots
// were left null. T1374 fixed the *directly*-inherited module interface, but the
// stub-declaration recursion in declareStructuralDefaultStubs reused the file it
// was handed: recursing from MyIter (found in the main file) into its parent
// Iterator (which lives in std) resolved Iterator against the *main* file, found
// nothing, and declared no `UpTo.count`/`UpTo.filter`/… stub → the concrete's
// vtable slot for that inherited default stayed `i8* null` → dispatch through the
// view segfaulted at address 0.
//
// The fix makes both the declare (declareStructuralDefaultStubs) and define
// (defineStructuralDefaultBodies) recursions re-resolve each interface's declaring
// file and swap c.info per level, symmetric across the mono and non-mono families.
func TestT1377TransitiveModuleIteratorVtableHasNoNullSlots(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MyIter[T] is Iterator[T] `+"`structural"+` {
		}
		type UpTo is MyIter[int] {
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
			MyIter[int] it = u;
			it.count();
		}
	`)

	vtable := codegentest.ExtractGlobal(ir, "promise_vtable_UpTo")
	if vtable == "" {
		t.Fatal("expected @promise_vtable_UpTo to be emitted")
	}
	// The pre-fix bug left the transitively-inherited combinator slots null.
	if strings.Contains(vtable, "i8* null") {
		t.Errorf("promise_vtable_UpTo has a null slot (T1377 regression):\n%s", vtable)
	}
	// Every combinator inherited through MyIter[int] → Iterator[int] must be
	// synthesized under the concrete name (UpTo.*), not the skipped generic
	// instance name.
	for _, m := range []string{
		"@UpTo.next",
		"@UpTo.filter",
		"@UpTo.take",
		"@UpTo.count",
		"@UpTo.collect",
		"@UpTo.find",
		"@UpTo.first",
	} {
		codegentest.AssertContains(t, vtable, m)
	}
	codegentest.AssertNotContains(t, vtable, "@Iterator__int.count")
	codegentest.AssertNotContains(t, vtable, "@MyIter__int.count")
}

// T1377 (define side): the transitively-inherited default must be both declared
// and *defined*. This is what catches a declared-but-undefined stub if only the
// declare recursion were made file-aware.
func TestT1377TransitiveDefaultSynthesized(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MyIter[T] is Iterator[T] `+"`structural"+` {
		}
		type UpTo is MyIter[int] {
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
			MyIter[int] it = UpTo(cur: 0, limit: 5);
			it.count();
		}
	`)
	countFn := codegentest.ExtractFunction(ir, "UpTo.count")
	if countFn == "" {
		t.Fatal("expected synthesized UpTo.count (transitive module Iterator default) to be emitted")
	}
	// count() iterates via this.next(); the synthesized body must dispatch to the
	// concrete's own next.
	codegentest.AssertContains(t, countFn, "@UpTo.next")
}

// T1377 (mono symmetry): a *generic* concrete type whose structural-parent chain
// crosses files (MyBox[T] is MyIter[T] is Iterator[T]) instantiated as MyBox[int]
// and dispatched through the view. The mono declare AND define recursions must
// both re-resolve the grandparent Iterator's file, else the slot is null (declare
// broken) or the stub is declared-but-undefined (only declare fixed).
//
// In the single-module generateIR path, mono instance names keep bracket form
// (`MyBox[int]`) and are quoted in IR; extractGlobal handles the quoting.
func TestT1377MonoTransitiveDefaultDefined(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MyIter[T] is Iterator[T] `+"`structural"+` {
		}
		type MyBox[T] is MyIter[T] {
			T val;
			int remaining;
			next(~this) T? {
				if this.remaining <= 0 { return none; }
				this.remaining = this.remaining - 1;
				return this.val;
			}
		}
		main() {
			MyBox[int] b = MyBox[int](val: 7, remaining: 3);
			MyIter[int] it = b;
			it.count();
		}
	`)

	vtable := codegentest.ExtractGlobal(ir, "promise_vtable_MyBox[int]")
	if vtable == "" {
		t.Fatal("expected @promise_vtable_MyBox[int] to be emitted")
	}
	if strings.Contains(vtable, "i8* null") {
		t.Errorf("promise_vtable_MyBox[int] has a null slot (T1377 mono regression):\n%s", vtable)
	}
	codegentest.AssertContains(t, vtable, `@"MyBox[int].count"`)

	// The declared stub must also be *defined* — a declared-but-undefined function
	// (empty body) would be worse than the symmetric no-op. Its body must dispatch
	// to the concrete's own next.
	defIdx := strings.Index(ir, `define i64 @"MyBox[int].count"(`)
	if defIdx < 0 {
		t.Fatal("expected MyBox[int].count to be defined, not just declared")
	}
	body := ir[defIdx:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	codegentest.AssertContains(t, body, `@"MyBox[int].next"`)
}
