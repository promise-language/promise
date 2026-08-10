package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T1427: a HEAP result value on a `go! {}` value exit is claimed away from the
// coroutine's temp cleanup for the store into G.result_ptr. A failing use-binding
// close() diverts that exit (emitCloseErrCheck → emitFailableGoBlockError), storing
// the {err} aggregate instead of the success value — so the divert path must DROP
// the orphaned result or it leaks (T0067). The fix arms goResultDivertVal/Type
// across the close check on the two value exits and drops it on the divert branch.
//
// Both value-exit styles route through the same `close.err.ret` divert block. The
// assertions below are scoped to that block: a bare module-wide substring check
// would pass even without the fix (heap drops appear all over normal cleanup), so
// each test isolates the divert block and requires the drop to live INSIDE it —
// that is exactly what the fix adds and what regresses if it is removed.

// closeErrRetBlock returns the body of the `close.err.ret*` divert basic block
// (from its label to the terminating blank line). Empty string if not present.
var closeErrRetLabelRE = regexp.MustCompile(`(?m)^\s*close\.err\.ret[\w.]*:`)

func closeErrRetBlock(t *testing.T, goro string) string {
	t.Helper()
	// Match the block LABEL definition (`close.err.ret.NN:`), not a `br` reference.
	loc := closeErrRetLabelRE.FindStringIndex(goro)
	if loc == nil {
		return ""
	}
	rest := goro[loc[0]:]
	// Blocks are separated by a blank line in the IR printer.
	if end := strings.Index(rest, "\n\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// Trailing-expression exit: `go! { s := make_str(3)?^; use r := ...; s + "!" }`.
func TestT1427_GoBlockTrailingHeapDivertDropsResult(t *testing.T) {
	ir := generateIR(t, `
		type CloseError is error { int code; }
		type FailResource {
			int id;
			close!(~this) void { raise CloseError(message: "cf", code: 42); }
		}
		make_str!(int x) string { if x < 0 { raise error("bad"); } return "v={x}"; }
		test() {
			t := go! {
				s := make_str(3)?^;
				use r := FailResource(id: 1);
				s + "!"
			};
		}
	`)
	goro := extractGoroutineBody(t, ir)

	block := closeErrRetBlock(t, goro)
	if block == "" {
		t.Fatalf("expected a close.err.ret divert block in the go-block coroutine:\n%s", goro)
	}
	if !strings.Contains(block, "promise_string_drop") {
		t.Errorf("expected the diverted heap (string) result to be dropped INSIDE the close.err.ret divert block:\n%s", block)
	}
}

// Trailing bare-identifier exit: `go! { s := make_str(4)?^; use r := ...; s }`.
// Distinct from the binary-expr case — the trailing value is a moved local with no
// fresh temp record, so the divert-drop must still reach it (arm in genBlockValue).
func TestT1427_GoBlockTrailingBareIdentHeapDivertDropsResult(t *testing.T) {
	ir := generateIR(t, `
		type CloseError is error { int code; }
		type FailResource {
			int id;
			close!(~this) void { raise CloseError(message: "cf", code: 42); }
		}
		make_str!(int x) string { if x < 0 { raise error("bad"); } return "v={x}"; }
		test() {
			t := go! {
				s := make_str(4)?^;
				use r := FailResource(id: 1);
				s
			};
		}
	`)
	goro := extractGoroutineBody(t, ir)

	block := closeErrRetBlock(t, goro)
	if block == "" {
		t.Fatalf("expected a close.err.ret divert block in the go-block coroutine:\n%s", goro)
	}
	if !strings.Contains(block, "promise_string_drop") {
		t.Errorf("expected the diverted bare-ident heap result to be dropped INSIDE the close.err.ret divert block:\n%s", block)
	}
}

// Non-string heap kind (Vector): the divert-drop routes through emitVariantFieldDrop,
// so a vector result must be dropped via Vector.drop inside the divert block, proving
// the fix is not string-specific.
func TestT1427_GoBlockHeapVectorDivertDropsResult(t *testing.T) {
	ir := generateIR(t, `
		type CloseError is error { int code; }
		type FailResource {
			int id;
			close!(~this) void { raise CloseError(message: "cf", code: 42); }
		}
		make_vec!(int x) int[] { if x < 0 { raise error("bad"); } return [x, x]; }
		test() {
			t := go! {
				v := make_vec(3)?^;
				use r := FailResource(id: 1);
				return v;
			};
		}
	`)
	goro := extractGoroutineBody(t, ir)

	block := closeErrRetBlock(t, goro)
	if block == "" {
		t.Fatalf("expected a close.err.ret divert block in the go-block coroutine:\n%s", goro)
	}
	if !strings.Contains(block, "Vector.drop") {
		t.Errorf("expected the diverted heap Vector result to be dropped (Vector.drop) INSIDE the close.err.ret divert block:\n%s", block)
	}
}

// Explicit-return exit (§17.2 / T1385): `go! { ...; use r := ...; return s + "!"; }`.
func TestT1427_GoBlockExplicitReturnHeapDivertDropsResult(t *testing.T) {
	ir := generateIR(t, `
		type CloseError is error { int code; }
		type FailResource {
			int id;
			close!(~this) void { raise CloseError(message: "cf", code: 42); }
		}
		make_str!(int x) string { if x < 0 { raise error("bad"); } return "v={x}"; }
		test() {
			t := go! {
				s := make_str(5)?^;
				use r := FailResource(id: 1);
				return s + "!";
			};
		}
	`)
	goro := extractGoroutineBody(t, ir)

	block := closeErrRetBlock(t, goro)
	if block == "" {
		t.Fatalf("expected a close.err.ret divert block in the go-block coroutine:\n%s", goro)
	}
	if !strings.Contains(block, "promise_string_drop") {
		t.Errorf("expected the diverted heap (string) return value to be dropped INSIDE the close.err.ret divert block:\n%s", block)
	}
}
