package codegen

import (
	"strings"
	"testing"
)

// T1338: An inline enum-ctor temp buried as a by-value CALL ARGUMENT inside a
// moved-out enum ctor's OWN arguments (`E.Full(describe(E.Full(heap)))`) leaked
// its payload. The moved-out outer ctor drives a wholesale enumCtorTemps clear at
// the statement move-out site (var-decl/return/assignment/container store), which
// zeroed the drop flag of EVERY tracked temp — including the inner arg ctor that
// `describe` only borrows/dups (B0232). The fix drains the nested by-value arg
// temp at the enum-ctor-argument boundary (genEnumVariantCallLayout), so a
// flag-guarded drop frees it before the wholesale clear runs.

// The nested inner ctor buried inside a call argument of a moved-out outer ctor
// gets a drain guard (enum.ctor.drop/skip) — the inner temp is freed, not orphaned.
func TestT1338NestedCtorArgInMovedOutCtorDrains(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		describe(Payload p) string {
			match p {
				Payload.Full(s) => { return s.to_upper(); },
				Payload.Empty   => { return "e"; },
			}
		}
		bind_it() int {
			q := Payload.Full(describe(Payload.Full("x".to_upper())));
			match q {
				Payload.Full(s) => { return s.len; },
				Payload.Empty   => { return 0; },
			}
		}
	`)
	fn := extractFunction(ir, "__user.bind_it")
	if fn == "" {
		t.Fatalf("could not extract @__user.bind_it from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "enum.ctor.drop") || !strings.Contains(fn, "enum.ctor.skip") {
		t.Fatalf("expected a nested-arg drain guard (enum.ctor.drop/skip) for the inner ctor buried in the moved-out ctor's arg, got:\n%s", fn)
	}
}

// Control: a moved-out ctor whose only arg is a plain heap-string call (no inner
// enum ctor buried in a by-value call arg) — the outer ctor temp is flag-cleared
// by the wholesale move-out clear, NOT dropped, and no nested-arg drain guard is
// emitted. This discriminates drain-present (inner ctor nested in a call arg) from
// drain-absent (no such nested ctor).
func TestT1338DirectCtorArgNoDrain(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		bind_it() int {
			q := Payload.Full("x".to_upper());
			match q {
				Payload.Full(s) => { return s.len; },
				Payload.Empty   => { return 0; },
			}
		}
	`)
	fn := extractFunction(ir, "__user.bind_it")
	if fn == "" {
		t.Fatalf("could not extract @__user.bind_it from IR:\n%s", ir)
	}
	assertNotContains(t, fn, "enum.ctor.drop")
}

// Skip path (enumCtorTempMovesOut): the outer ctor's arg is ITSELF a direct nested
// ctor (`Outer.V(Inner.W(heap))`), which is moved into the payload. drainNestedEnumCtorTemps
// must NOT drain it — it stays tracked so the enclosing wholesale move-out clear
// zeroes its flag (the value now lives inside `q`, dropped when `q` is). No nested-arg
// drain guard is emitted for the direct-ctor arg.
func TestT1338NestedDirectCtorArgKeptNotDrained(t *testing.T) {
	ir := generateIR(t, `
		enum Inner { W(string s), None, }
		enum Outer { V(Inner i), Empty, }
		bind_it() int {
			q := Outer.V(Inner.W("x".to_upper()));
			match q {
				Outer.V(i) => { return 1; },
				Outer.Empty => { return 0; },
			}
		}
	`)
	fn := extractFunction(ir, "__user.bind_it")
	if fn == "" {
		t.Fatalf("could not extract @__user.bind_it from IR:\n%s", ir)
	}
	// The direct nested ctor arg is moved into the payload — no drain guard for it.
	assertNotContains(t, fn, "enum.ctor.drop")
}

// Per-iteration snapshot: a multi-arg variant mixing a DIRECT nested-ctor arg
// (arg0, kept in the payload) with a call whose own arg is an inline ctor (arg1,
// borrowed → drained). The drain guard IS emitted (for arg1) but must floor at
// arg1's own start — a pre-loop snapshot would sweep arg0's kept temp. Presence of
// the drain guard alongside the direct-ctor arg confirms the per-iteration snapshot.
func TestT1338MultiArgPerIterationSnapshot(t *testing.T) {
	ir := generateIR(t, `
		enum Inner { W(string s), None, }
		enum Outer { V(Inner first, string second), Empty, }
		describe(Inner p) string {
			match p {
				Inner.W(s) => { return s.to_upper(); },
				Inner.None => { return "n"; },
			}
		}
		bind_it() int {
			q := Outer.V(Inner.W("a".to_upper()), describe(Inner.W("b".to_upper())));
			match q {
				Outer.V(i, s) => { return s.len; },
				Outer.Empty => { return 0; },
			}
		}
	`)
	fn := extractFunction(ir, "__user.bind_it")
	if fn == "" {
		t.Fatalf("could not extract @__user.bind_it from IR:\n%s", ir)
	}
	// arg1's nested-in-call ctor is drained → guard present.
	if !strings.Contains(fn, "enum.ctor.drop") || !strings.Contains(fn, "enum.ctor.skip") {
		t.Fatalf("expected a nested-arg drain guard for the call-arg ctor in a multi-arg variant, got:\n%s", fn)
	}
}
