package sema

import (
	"testing"
)

// aliasFactFor returns the recorded StructuralReturnAliasParams slice for the
// free function named fn, and whether the fact was recorded at all.
func aliasFactFor(info *Info, fn string) ([]bool, bool) {
	for f, alias := range info.StructuralReturnAliasParams {
		if f.Name() == fn {
			return alias, true
		}
	}
	return nil, false
}

func assertAliasFact(t *testing.T, info *Info, fn string, want []bool) {
	t.Helper()
	got, ok := aliasFactFor(info, fn)
	if !ok {
		t.Fatalf("no StructuralReturnAliasParams fact recorded for %s", fn)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: alias len = %d, want %d (got %v)", fn, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: alias = %v, want %v", fn, got, want)
		}
	}
}

// T1305: the per-parameter structural-return-alias fact must distinguish a fresh
// construction / owned index clone (no alias) from a returned argument (alias).
func TestStructuralReturnAliasFacts(t *testing.T) {
	info := checkOK(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(int n) { this.total = this.total + n; } }
		type Widget { int id; emit(int n) {} }

		// Owned index clone through a borrow arg — does NOT alias v.
		first_of(Sink[] v) Sink { return v[0]; }

		// Fresh construction through a moved arg — aliases nothing.
		make_from(Counter ~c) Sink { return Widget(id: c.total); }

		// Returns its argument directly — aliases s.
		pass_through(Sink s) Sink { return s; }

		// Branch mix: one branch returns the param, the other constructs fresh.
		pick(Sink s, bool b) Sink { return if b { s } else { Widget(id: 0) }; }

		// Match mix: one arm returns the param, the other constructs fresh.
		pick_match(Sink s, int n) Sink {
			return match n { 0 => s, _ => Widget(id: n) };
		}
	`)

	assertAliasFact(t, info, "first_of", []bool{false})
	assertAliasFact(t, info, "make_from", []bool{false})
	assertAliasFact(t, info, "pass_through", []bool{true})
	// pick: param s (index 0) may alias; bool b (index 1) never.
	assertAliasFact(t, info, "pick", []bool{true, false})
	// pick_match: param s (index 0) may alias; int n (index 1) never.
	assertAliasFact(t, info, "pick_match", []bool{true, false})
}

// T1305: conservative and root-marking classifier paths. Each function exercises
// a distinct return shape that the base-case test does not:
//   - returning a LOCAL (markAll) — no provenance analysis, mark every param.
//   - returning a non-constructor CALL result (markAll).
//   - returning a structural MEMBER read (markRoot) — marks the owner param.
//   - returning a member of a LOCAL (markRoot → markAll) — non-param root.
//   - a nested return inside an `if` STATEMENT (walkStmt/walkBlock recursion).
//   - a nested return inside an `if`-EXPRESSION in a var decl (walkExpr recursion).
//   - an `if`-expression branch that DIVERGES (markBranch/lastStmtDiverges) — the
//     diverging branch yields no value and contributes no alias.
func TestStructuralReturnAliasConservativePaths(t *testing.T) {
	info := checkOK(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Widget { int id; emit(int n) {} }
		type Holder { Sink inner; }
		pass_through(Sink s) Sink { return s; }

		// Returns a local — no provenance, conservatively marks all params.
		via_local(Sink s) Sink { x := s; return x; }

		// Returns a non-constructor call result — conservatively marks all params.
		via_call(Sink s) Sink { return pass_through(s); }

		// Returns a structural member of a PARAM — markRoot marks the owner.
		via_member(Holder ~h) Sink { return h.inner; }

		// Returns a member of a LOCAL — markRoot bottoms out at a non-param, marks all.
		via_member_local(Sink s) Sink { h := Holder(inner: s); return h.inner; }

		// Nested return inside an if-STATEMENT — the walker must find it and mark s.
		via_if_stmt(Sink s, bool b) Sink {
			if b { return s; }
			return Widget(id: 0);
		}

		// Nested return inside a WHILE loop — a missed walker arm here would leave the
		// aliasing return unclassified and let codegen double-free the passthrough.
		via_while(Sink s, bool b) Sink {
			while b { return s; }
			return Widget(id: 0);
		}

		// Nested return inside a FOR-IN loop — same guard for the loop walker arm.
		via_for(Sink s, int[] xs) Sink {
			for x in xs { return s; }
			return Widget(id: 0);
		}

		// Nested return inside an if-EXPRESSION (var-decl value) — walkExpr recursion.
		via_nested(Sink s, bool b) Sink {
			x := if b { return s; } else { 0 };
			return Widget(id: x);
		}

		// if-expression whose else branch DIVERGES — only the then-branch (s) aliases;
		// the diverging else yields no value.
		pick_diverge(Sink s, bool b) Sink {
			return if b { s } else { return Widget(id: 0); };
		}
	`)

	assertAliasFact(t, info, "via_local", []bool{true})
	assertAliasFact(t, info, "via_call", []bool{true})
	assertAliasFact(t, info, "via_member", []bool{true})
	assertAliasFact(t, info, "via_member_local", []bool{true})
	assertAliasFact(t, info, "via_if_stmt", []bool{true, false})
	assertAliasFact(t, info, "via_while", []bool{true, false})
	assertAliasFact(t, info, "via_for", []bool{true, false})
	assertAliasFact(t, info, "via_nested", []bool{true, false})
	assertAliasFact(t, info, "pick_diverge", []bool{true, false})
}

// A function whose result is not a non-value structural interface, or which takes
// no parameters, records no fact (the analysis is gated to those shapes so codegen
// falls back to its conservative default).
func TestStructuralReturnAliasGatedToStructural(t *testing.T) {
	info := checkOK(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Widget { int id; emit(int n) {} }

		// Non-structural result: gated out.
		plain(Widget w) int { return w.id; }

		// Structural result but ZERO params: nothing to index, gated out.
		no_params() Sink { return Widget(id: 0); }
	`)
	if _, ok := aliasFactFor(info, "plain"); ok {
		t.Fatalf("expected no StructuralReturnAliasParams fact for non-structural-returning function")
	}
	if _, ok := aliasFactFor(info, "no_params"); ok {
		t.Fatalf("expected no StructuralReturnAliasParams fact for a zero-parameter function")
	}
}
