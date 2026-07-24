package codegen

import (
	"regexp"
	"testing"
)

// T1340: A block-value arm (an `if`/`match` used as a call argument) whose LEADING
// statement is a var-decl (or assignment / container store) binding a moved-out enum
// constructor, while a sibling by-value enum-ctor arg temp of the ENCLOSING call is
// still live, panicked codegen with `slice bounds out of range`.
//
// genBlockValue sets blockTempFloorEnum = len(enumCtorTemps) on arm entry so the
// arm's own leading statements drain only their own temps, leaving the enclosing
// call's sibling prefix [0:floor) intact. But the four move-out clear sites did an
// unbounded `enumCtorTemps = enumCtorTemps[:0]`, which (a) zeroed the sibling's drop
// flag (the T1338/T1339 over-clear leak) and (b) truncated below the floor, so the
// following floor-bounded drainEnumCtorTempsFrom(floor) sliced out of range.
//
// The fix bounds each clear to temps at/above blockTempFloorEnum
// (clearMovedOutEnumCtorTemps), leaving the sibling prefix for the statement-boundary
// drain to guard-drop.

// The `:=` form (genInferredVarDecl) — the exact site that panics in the item repro.
// After the fix the enclosing function emits exactly ONE enum.ctor.drop guard block:
// the surviving sibling `Payload.Full("hi"...)` temp, drained at the `q := combine(...)`
// statement boundary (the inner `r := Payload.Full("zz"...)` temp is moved into `r`, so
// it is cleared, not drained).
func TestT1340BlockValueArmMoveOutVarDeclNoPanic(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		combine(Payload p, int k) int {
			match p {
				Payload.Full(s) => { return s.len; },
				Payload.Empty   => { return 0; },
			}
		}
		direct(bool c) int {
			q := combine(Payload.Full("hi".to_upper()), if c { r := Payload.Full("zz".to_upper()); combine(r, 0) } else { 0 });
			return q;
		}
	`)
	fn := extractFunction(ir, "__user.direct")
	if fn == "" {
		t.Fatalf("could not extract @__user.direct from IR:\n%s", ir)
	}
	// Exactly one guard block for the surviving sibling temp. The inner move-out
	// var-decl's temp is moved into `r` (cleared, floor-bounded); the sibling is
	// preserved and drained at the enclosing statement boundary.
	blockRe := regexp.MustCompile(`enum\.ctor\.drop\.\d+:`)
	n := len(blockRe.FindAllString(fn, -1))
	if n != 1 {
		t.Fatalf("expected exactly ONE enum.ctor.drop guard block (the preserved sibling temp); found %d in:\n%s", n, fn)
	}
}

// The typed var-decl form (`Payload r = ...`, genTypedVarDecl) — a DISTINCT clear site
// from the `:=` inferred form above. Same floor-bounded clear; must not panic and must
// preserve the enclosing call's sibling temp (one guard block in the enclosing fn).
func TestT1340BlockValueArmMoveOutTypedVarDeclNoPanic(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		combine(Payload p, int k) int {
			match p {
				Payload.Full(s) => { return s.len; },
				Payload.Empty   => { return 0; },
			}
		}
		direct(bool c) int {
			q := combine(Payload.Full("hi".to_upper()), if c { Payload r = Payload.Full("zz".to_upper()); combine(r, 0) } else { 0 });
			return q;
		}
	`)
	fn := extractFunction(ir, "__user.direct")
	if fn == "" {
		t.Fatalf("could not extract @__user.direct from IR:\n%s", ir)
	}
	blockRe := regexp.MustCompile(`enum\.ctor\.drop\.\d+:`)
	n := len(blockRe.FindAllString(fn, -1))
	if n != 1 {
		t.Fatalf("expected exactly ONE enum.ctor.drop guard block (the preserved sibling temp); found %d in:\n%s", n, fn)
	}
}

// The container-element store form (`v[i] = ctor`, the container move-out clear site) —
// the fourth clear site, runtime-exercised in the e2e test but asserted here at the IR
// level for the "exactly one preserved sibling guard block" shape.
func TestT1340BlockValueArmMoveOutContainerStoreNoPanic(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		combine(Payload p, int k) int {
			match p {
				Payload.Full(s) => { return s.len; },
				Payload.Empty   => { return 0; },
			}
		}
		direct(bool c) int {
			q := combine(Payload.Full("hi".to_upper()), if c { v := [Payload.Empty]; v[0] = Payload.Full("zz".to_upper()); combine(v[0], 0) } else { 0 });
			return q;
		}
	`)
	fn := extractFunction(ir, "__user.direct")
	if fn == "" {
		t.Fatalf("could not extract @__user.direct from IR:\n%s", ir)
	}
	blockRe := regexp.MustCompile(`enum\.ctor\.drop\.\d+:`)
	n := len(blockRe.FindAllString(fn, -1))
	if n != 1 {
		t.Fatalf("expected exactly ONE enum.ctor.drop guard block (the preserved sibling temp); found %d in:\n%s", n, fn)
	}
}

// The `=` assignment form (the assignment move-out clear site). Same shape, but the
// arm's leading statement is a reassignment of a moved-out enum ctor. Must not panic
// and must still preserve the sibling temp (one guard block in the enclosing fn).
func TestT1340BlockValueArmMoveOutAssignNoPanic(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		combine(Payload p, int k) int {
			match p {
				Payload.Full(s) => { return s.len; },
				Payload.Empty   => { return 0; },
			}
		}
		direct(bool c) int {
			q := 0;
			q = combine(Payload.Full("hi".to_upper()), if c { r := Payload.Empty; r = Payload.Full("zz".to_upper()); combine(r, 0) } else { 0 });
			return q;
		}
	`)
	fn := extractFunction(ir, "__user.direct")
	if fn == "" {
		t.Fatalf("could not extract @__user.direct from IR:\n%s", ir)
	}
	blockRe := regexp.MustCompile(`enum\.ctor\.drop\.\d+:`)
	n := len(blockRe.FindAllString(fn, -1))
	if n != 1 {
		t.Fatalf("expected exactly ONE enum.ctor.drop guard block (the preserved sibling temp); found %d in:\n%s", n, fn)
	}
}

// Regression guard for the floor == 0 case: a plain top-level `x := Payload.Full(...)`
// var-decl with NO enclosing incomplete call. The ctor IS the moved-out value bound to
// x — its flag is cleared and NO drain guard is emitted (the wholesale-clear-at-floor-0
// behavior, unchanged by the T1340 bound).
func TestT1340TopLevelMoveOutUnchanged(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		use_it(Payload move p) int {
			match p { Payload.Full(s) => { return s.len; }, Payload.Empty => { return 0; }, }
		}
		top() int {
			x := Payload.Full("hi".to_upper());
			return use_it(move x);
		}
	`)
	fn := extractFunction(ir, "__user.top")
	if fn == "" {
		t.Fatalf("could not extract @__user.top from IR:\n%s", ir)
	}
	// floor == 0 → the moved-out ctor temp is cleared, not drained.
	assertNotContains(t, fn, "enum.ctor.drop")
}
