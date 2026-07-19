package codegen

import (
	"strings"
	"testing"
)

// T1329: A block-value body (if/match arm, `?` handler) used as a non-last call
// argument, whose body has a LEADING (non-last) statement, must NOT drain the
// enclosing expression's SIBLING heap temps at that statement's boundary. The
// old statement-boundary drain (cleanupStmtLevelTemps) emptied the WHOLE temp
// array, freeing the sibling mid-body; the enclosing call then re-read it after
// the merge → use-after-free. This is the general (leading-statement) form of
// T1325, and it affects if/match bodies too — not just error handlers.
//
// These lock the structural IR: the arm block that holds the leading statement
// must contain NO sibling drop; the sibling is dropped only at statement end
// (after the merge). Runtime zero-leak / correct-value coverage lives in
// tests/e2e/t1329_block_value_sibling_temp_test.pr.

// labeledBlockRegion extracts the basic block whose label starts with `prefix`
// (from its label up to the next blank-line-separated block) from a function body.
func labeledBlockRegion(body, prefix string) string {
	for _, block := range strings.Split(body, "\n\n") {
		trimmed := strings.TrimLeft(block, "\n")
		if strings.HasPrefix(trimmed, prefix) {
			return block
		}
	}
	return ""
}

func TestT1329_IfArmLeadingStmtDoesNotDropSibling(t *testing.T) {
	ir := generateIR(t, `
		make_s() string { return "abc".to_upper(); }
		combine(string s, int n) int { return s.len + n; }
		caller() int { return combine(make_s(), if true { int d = 7; d + 100 } else { 0 }); }
		main() { x := caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	// The `if.then` block holds the leading statement `int d = 7`. Before the fix,
	// that statement's boundary drained the whole temp array — emitting a
	// promise_string_drop for the sibling make_s() temp INSIDE this branch block,
	// freeing it before the enclosing combine() re-reads it at the merge.
	thenBlk := labeledBlockRegion(body, "if.then")
	if thenBlk == "" {
		t.Fatalf("expected an if.then block in @caller body:\n%s", body)
	}
	if strings.Contains(thenBlk, "promise_string_drop") || strings.Contains(thenBlk, "tmp.drop") {
		t.Errorf("if.then block (leading statement `int d = 7`) must NOT drop the "+
			"enclosing expression's sibling make_s() temp mid-body (T1329); it would be "+
			"freed before combine() re-reads it after the merge. if.then block:\n%s", thenBlk)
	}
	// Sanity: the sibling IS still dropped somewhere in the caller (at statement end,
	// after the merge — on both the panic-propagation and ok exit paths).
	if !strings.Contains(body, "@promise_string_drop") {
		t.Errorf("sibling make_s() temp must still be dropped at statement end (T1329); "+
			"no @promise_string_drop anywhere in @caller body:\n%s", body)
	}
}

func TestT1329_MatchArmLeadingStmtDoesNotDropSibling(t *testing.T) {
	ir := generateIR(t, `
		make_s() string { return "abc".to_upper(); }
		combine(string s, int n) int { return s.len + n; }
		caller(int k) int { return combine(make_s(), match k { 1 => { int d = 7; d + 100 }, _ => { 0 } }); }
		main() { x := caller(1); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	// The match arm's body block holds the leading statement. Find the block that
	// contains the arm computation (`add i64 ..., 100`) and assert it drops no sibling.
	var armBlk string
	for _, block := range strings.Split(body, "\n\n") {
		if strings.Contains(block, "add i64") && strings.Contains(block, "100") {
			armBlk = block
			break
		}
	}
	if armBlk == "" {
		t.Fatalf("expected a match-arm block computing `+ 100` in @caller body:\n%s", body)
	}
	if strings.Contains(armBlk, "promise_string_drop") {
		t.Errorf("match-arm block (leading statement) must NOT drop the sibling make_s() "+
			"temp mid-body (T1329). arm block:\n%s", armBlk)
	}
	if !strings.Contains(body, "@promise_string_drop") {
		t.Errorf("sibling make_s() temp must still be dropped at statement end (T1329); "+
			"no @promise_string_drop in @caller body:\n%s", body)
	}
}

// TestT1329_NestedIfStmtDoesNotDropHeapSibling guards the second fix front: a
// nested control-flow STATEMENT (here an if) as the leading statement of a
// block-value body reaches a mid-body merge whose OWN heap/env drain
// (genIfStmt: cleanupHeapTempsFrom/cleanupEnvTempsFrom) must be floor-aware.
// Before the fix genIfStmt drained from 0, freeing the enclosing use_box()'s
// droppable Box sibling inside the nested-if merge block — a use-after-free the
// enclosing call then re-read (segfault at runtime). The nested-if merge block
// (label starting `if.merge`) must NOT drop the sibling; the drop happens only at
// statement end after the enclosing merge.
func TestT1329_NestedIfStmtDoesNotDropHeapSibling(t *testing.T) {
	ir := generateIR(t, `
		type Box { string s; drop(~this) {} }
		make_box() Box { return Box(s: "abc".to_upper()); }
		use_box(Box b, int n) int { return b.s.len + n; }
		caller() int { return use_box(make_box(), if true { int d = 4; if 1 > 0 { d = d + 1; } d + 100 } else { 0 }); }
		main() { x := caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	// The inner if's merge block (the leading if-STATEMENT's merge) must not carry
	// a Box.drop for the sibling. Scan every if.merge-labelled block.
	for _, block := range strings.Split(body, "\n\n") {
		trimmed := strings.TrimLeft(block, "\n")
		if strings.HasPrefix(trimmed, "if.merge") && strings.Contains(block, "@Box.drop") {
			t.Errorf("nested if-stmt merge block must NOT drop the enclosing use_box() "+
				"sibling Box mid-body (T1329); it is re-read after the enclosing merge. "+
				"block:\n%s", block)
		}
	}
	// Sanity: the sibling Box IS still dropped somewhere (at statement end).
	if !strings.Contains(body, "@Box.drop") {
		t.Errorf("sibling Box temp must still be dropped at statement end (T1329); "+
			"no @Box.drop in @caller body:\n%s", body)
	}
}

// TestT1329_MatchArmDivergenceWellFormed guards Piece 2 (prefix rebuild on
// divergence): when one arm diverges (return) while a sibling arm reaches the
// merge, the block-value body drains the full temp array on the divergent path;
// the prefix must be rebuilt so the sibling still drops on the non-diverging path
// and the enclosing statement. Codegen must not panic and must produce a phi.
func TestT1329_MatchArmDivergenceWellFormed(t *testing.T) {
	ir := generateIR(t, `
		make_s() string { return "abc".to_upper(); }
		combine(string s, int n) int { return s.len + n; }
		helper!(int k) int { return combine(make_s(), match k { 0 => { int z = 1; return 999 }, _ => { 100 } }); }
		main() { x := helper(5)? e { 0 }; }
	`)
	body := extractFunction(ir, "__user.helper")
	if body == "" {
		t.Fatalf("expected @__user.helper in IR (codegen must not panic on arm divergence)")
	}
	// The non-diverging arm still reaches the merge, so the sibling make_s() temp
	// must be dropped (at statement end); the divergent arm's return path drains it
	// separately.
	if !strings.Contains(body, "@promise_string_drop") {
		t.Errorf("sibling make_s() temp must be dropped on the non-diverging path (T1329); "+
			"no @promise_string_drop in @helper body:\n%s", body)
	}
}
