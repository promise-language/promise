package codegen

import (
	"strings"
	"testing"
)

// T1325: A `? {}` error handler that recovers-and-continues as a non-last call
// argument must NOT drop the enclosing expression's SIBLING heap temps at the
// handler-block entry. The old unconditional handler-block drain freed the
// sibling before the enclosing call re-read it on the recovery path →
// use-after-free. Error-path analogue of the T1272 OK-path fix.
//
// This locks the structural IR: with a failable call that has NO own temps, the
// `error.handler` block must contain NO string drop for the sibling `make_s()`
// temp — that temp is dropped only at statement end (after the merge). Runtime
// zero-leak / correct-value coverage lives in
// tests/e2e/t1325_handler_recover_sibling_test.pr.

// handlerBlockRegion extracts the `error.handler...` basic block text (from its
// label up to the next blank-line-separated block) from a function body.
func handlerBlockRegion(t *testing.T, body string) string {
	t.Helper()
	for _, block := range strings.Split(body, "\n\n") {
		trimmed := strings.TrimLeft(block, "\n")
		if strings.HasPrefix(trimmed, "error.handler") {
			return block
		}
	}
	return ""
}

func TestT1325_HandlerRecoverDoesNotDropSibling(t *testing.T) {
	ir := generateIR(t, `
		make_s() string { return "abc".to_upper(); }
		may_fail!(int x) int { if x < 0 { raise error(message: "neg"); } return x; }
		combine(string s, int n) int { return s.len + n; }
		caller() int { return combine(make_s(), may_fail(-1)? e { 100 }); }
		main() { x := caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	handler := handlerBlockRegion(t, body)
	if handler == "" {
		t.Fatalf("expected an error.handler block in @caller body:\n%s", body)
	}
	// The failable call `may_fail(-1)` has no own arg temps, so the handler block
	// must drain NO statement-level temps — it should go straight to building the
	// error binding, leaving the sibling make_s() temp live for the merge. The old
	// (buggy) IR branched from error.handler into an `err.tmp.drop`/`err.heap.drop`
	// cleanup chain that freed the sibling. (The `drop.call`/`drop.skip` branch for
	// the caught error binding itself is legitimate and NOT flagged here.)
	if strings.Contains(handler, "err.tmp.drop") || strings.Contains(handler, "err.heap.drop") {
		t.Errorf("handler block must NOT drain statement-level temps on the "+
			"recover-and-continue path (T1325); the sibling make_s() temp would be "+
			"freed before the enclosing combine() re-reads it. error.handler block:\n%s", handler)
	}
	// Sanity: the temp IS still dropped somewhere in the caller (statement end).
	if !strings.Contains(body, "@promise_string_drop") {
		t.Errorf("sibling make_s() temp must still be dropped at statement end "+
			"(T1325); no @promise_string_drop anywhere in @caller body:\n%s", body)
	}
}
