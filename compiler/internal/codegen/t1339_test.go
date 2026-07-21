package codegen

import (
	"regexp"
	"testing"
)

// T1339: A by-value inline enum-constructor call ARGUMENT followed by a LATER
// argument that diverges via `return` orphaned the earlier arg's heap payload. At
// the diverging `return Payload.Empty` (whose value is itself a move-out enum ctor,
// so enumCtorTempMovesOut == true), genReturnStmt's statement-level enum-ctor-temp
// clear swept EVERY tracked temp wholesale — including the sibling `Payload.Full(...)`
// temp still on the stack from the half-built enclosing `combine(...)` call. It
// unconditionally zeroed the sibling's drop flag, so the full drain dropped nothing
// and the payload leaked.
//
// The fix snapshots the temp count at return entry (enumCtorSnap) and bounds the
// clear to temps created while evaluating THIS return value. The sibling prefix
// survives for drainEnumCtorTemps to guard-drop on the divergent path.

// The divergent `return` inside a still-incomplete call must GUARD-drop the sibling
// enum-ctor arg temp on the divergent path — in addition to the non-divergent path's
// drain. So the sibling function contains TWO enum.ctor.drop guard blocks (one per
// path). Before the fix, the divergent path unconditionally zeroed the sibling flag
// and emitted NO guard, leaving only ONE enum.ctor.drop block (the non-divergent
// path) and leaking on the divergent path.
func TestT1339SiblingCtorDrainedOnDivergentReturn(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		combine(Payload p, int k) Payload {
			match p {
				Payload.Full(s) => { return Payload.Full(s.to_upper()); },
				Payload.Empty   => { return Payload.Empty; },
			}
		}
		sibling(bool d) Payload {
			q := combine(Payload.Full("h".to_upper()), if d { return Payload.Empty } else { 0 });
			return q;
		}
	`)
	fn := extractFunction(ir, "__user.sibling")
	if fn == "" {
		t.Fatalf("could not extract @__user.sibling from IR:\n%s", ir)
	}
	// Count enum-ctor drop guard BLOCK definitions (`enum.ctor.drop.N:`). The
	// divergent-return path and the non-divergent (call-completes) path each emit
	// one, so a correctly-bounded clear yields two. A single block means the
	// divergent path swept the sibling flag without a guard — the T1339 leak.
	blockRe := regexp.MustCompile(`enum\.ctor\.drop\.\d+:`)
	n := len(blockRe.FindAllString(fn, -1))
	if n < 2 {
		t.Fatalf("expected the sibling enum-ctor temp to be guard-dropped on BOTH the divergent and non-divergent paths (2 enum.ctor.drop blocks); found %d in:\n%s", n, fn)
	}
}

// Regression guard for the enumCtorSnap == 0 case: a plain `return Payload.Full(...)`
// with NO enclosing incomplete call. The ctor IS the returned value, moved out to
// the caller — its flag is cleared and NO drain guard is emitted (the T1317
// wholesale-clear-at-snap-0 behavior, unchanged by the T1339 bound).
func TestT1339DirectCtorReturnNoSiblingDrain(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		make_full() Payload { return Payload.Full("x".to_upper()); }
	`)
	fn := extractFunction(ir, "__user.make_full")
	if fn == "" {
		t.Fatalf("could not extract @__user.make_full from IR:\n%s", ir)
	}
	// No enclosing call → enumCtorSnap == 0 → the ctor temp is moved out, not drained.
	assertNotContains(t, fn, "enum.ctor.drop")
}
