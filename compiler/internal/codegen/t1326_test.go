package codegen

import (
	"strings"
	"testing"
)

// T1326: An inline enum-constructor temp used as a by-value CALL ARGUMENT inside a
// branch (`match`/`if`) arm leaked its heap payload. The branch's statement-level
// enum-ctor-temp clear (enumCtorTempMovesOut → true for any enum-result branch)
// swept EVERY tracked temp wholesale, assuming all are the arm phi result. The
// nested arg ctor is NOT the phi result — the callee only borrows/dups it (B0232),
// so the caller retains ownership and it must be drained at the arm boundary.
//
// The fix (drainNestedArmEnumCtorTemps) drops such nested arg temps in the arm's
// block before the merge, so the statement-level wholesale clear only sees genuine
// arm-result ctors. This must emit an enum.ctor.drop/skip guard for the nested arg,
// while NOT emitting one for a direct-ctor arm (which IS moved out through the phi).

// An `if`-expr arm whose value is a call taking a by-value enum-ctor arg drains that
// arg temp at the arm boundary — a guarded drop of the enum-ctor temp.
func TestT1326IfArmNestedCallArgDrains(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		wrap(Payload p) Payload {
			match p {
				Payload.Full(s) => { return Payload.Full(s.to_upper()); },
				Payload.Empty   => { return Payload.Empty; },
			}
		}
		len_of(Payload move p) int {
			match p { Payload.Full(s) => { return s.len; }, Payload.Empty => { return 0; }, }
		}
		branch_if(bool c) int {
			q := if c { wrap(Payload.Full("h".to_upper())) } else { Payload.Empty };
			return len_of(move q);
		}
	`)
	fn := extractFunction(ir, "__user.branch_if")
	if fn == "" {
		t.Fatalf("could not extract @__user.branch_if from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "enum.ctor.drop") || !strings.Contains(fn, "enum.ctor.skip") {
		t.Fatalf("expected enum-ctor drain guard (enum.ctor.drop/skip) for the nested by-value call-arg ctor in an if arm, got:\n%s", fn)
	}
}

// A `match` expression-form arm whose value is a call taking a by-value enum-ctor
// arg drains that arg temp at the arm boundary.
func TestT1326MatchExprArmNestedCallArgDrains(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		wrap(Payload p) Payload {
			match p {
				Payload.Full(s) => { return Payload.Full(s.to_upper()); },
				Payload.Empty   => { return Payload.Empty; },
			}
		}
		len_of(Payload move p) int {
			match p { Payload.Full(s) => { return s.len; }, Payload.Empty => { return 0; }, }
		}
		branch_match(bool c) int {
			q := match c { true => wrap(Payload.Full("h".to_upper())), _ => Payload.Empty, };
			return len_of(move q);
		}
	`)
	fn := extractFunction(ir, "__user.branch_match")
	if fn == "" {
		t.Fatalf("could not extract @__user.branch_match from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "enum.ctor.drop") || !strings.Contains(fn, "enum.ctor.skip") {
		t.Fatalf("expected enum-ctor drain guard (enum.ctor.drop/skip) for the nested by-value call-arg ctor in a match arm, got:\n%s", fn)
	}
}

// Regression guard for the T1317 wholesale-clear behavior: a branch whose arms are
// DIRECT enum ctors moves the arm-result temp out through the phi — its flag is
// cleared by the statement-level clear, and NO drain guard is emitted for it.
func TestT1326DirectCtorArmNoDrain(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		len_of(Payload move p) int {
			match p { Payload.Full(s) => { return s.len; }, Payload.Empty => { return 0; }, }
		}
		branch_direct(bool c) int {
			q := if c { Payload.Full("h".to_upper()) } else { Payload.Empty };
			return len_of(move q);
		}
	`)
	fn := extractFunction(ir, "__user.branch_direct")
	if fn == "" {
		t.Fatalf("could not extract @__user.branch_direct from IR:\n%s", ir)
	}
	assertNotContains(t, fn, "enum.ctor.drop")
}

// Divergence guard: when an arm tail creates a nested by-value enum-ctor temp and
// then DIVERGES (here via `break` out of the enclosing loop), the block terminator
// is already set and the temp array was drained by the break's cleanup path. The
// arm-boundary drain (drainNestedArmEnumCtorTemps) must early-return in that case
// rather than drain again — it detects the set terminator (c.block.Term != nil) and
// does nothing. This exercises that guard; the runtime counterpart (arm_break in
// enum_ctor_branch_arg_leak_test.pr) confirms no leak and no double-free.
func TestT1326ArmTailDivergesViaBreak(t *testing.T) {
	// Compiles cleanly (no codegen panic) and produces a body for the enclosing
	// function — the divergence guard prevents a wrong second drain of the arm's
	// nested ctor temp after the break has already terminated the block.
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		combine(Payload p, int k) Payload {
			match p { Payload.Full(s) => { return Payload.Full(s.to_upper()); }, Payload.Empty => { return Payload.Empty; }, }
		}
		sink(Payload move p) int { match p { Payload.Full(s) => { return s.len; }, Payload.Empty => { return 0; }, } }
		arm_break(bool c) int {
			total := 5;
			for {
				q := match c {
					true => combine(Payload.Full("hi".to_upper()), if c { break } else { break }),
					_ => Payload.Empty,
				};
				total = sink(move q);
				break;
			}
			return total;
		}
	`)
	if extractFunction(ir, "__user.arm_break") == "" {
		t.Fatalf("could not extract @__user.arm_break from IR:\n%s", ir)
	}
}
