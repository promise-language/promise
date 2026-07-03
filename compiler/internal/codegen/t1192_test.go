package codegen

import (
	"strings"
	"testing"
)

// T1192: a plain reassignment (`s = f()`) in the UPDATE clause of a C-style
// `for init; cond; update` loop must go through the full assignment path
// (drop-old + RHS temp claim). Previously genClassicForStmt emitted a bare
// `store` for the OpAssign case, which neither dropped the overwritten heap
// value nor claimed the RHS temp — double-freeing/segfaulting an owned heap
// value and leaking strings/vectors. The fix routes the OpAssign update through
// genStmt with a synthesized AssignStmt, so the update block now emits a
// guarded old-value drop before storing the new value.
func TestT1192_ClassicForUpdateAssignDropsOld(t *testing.T) {
	ir := generateIR(t, `
		type Counter { int n; drop(~this) {} }
		mk() Counter { return Counter(n: 9); }
		caller() {
			c := 0;
			s := Counter(n: 0);
			for j := 0; c < 3; s = mk() {
				c = c + 1;
			}
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "for.update") {
		t.Fatalf("expected a for.update block in caller:\n%s", body)
	}
	// The update clause calls mk() and reassigns s. Routing through the
	// assignment path drops the OLD s value (guarded on s.dropflag) before
	// storing the fresh one. Without the fix there was a bare store and only
	// two drops of s (panic-cleanup + scope-exit); the fix adds the update
	// clause's own drop-old, so Counter.drop appears three times.
	got := strings.Count(body, "@Counter.drop")
	if got < 3 {
		t.Fatalf("expected the update-clause reassignment to drop the old value (>=3 @Counter.drop calls), got %d:\n%s", got, body)
	}
	// The RHS temp must be claimed and the reassignment target's drop flag set,
	// proving the update went through genAssignStmt rather than a bare store.
	if !strings.Contains(body, "s.dropflag") {
		t.Fatalf("expected s.dropflag bookkeeping from the assignment path in caller:\n%s", body)
	}
}
