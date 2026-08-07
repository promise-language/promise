package codegen

import "testing"

// T1388: a failable use-binding close() that fails on a nested block's NORMAL
// (success) exit takes an early error-return (`close.err.ret`). Before T1388 that
// early exit ran `ret` immediately, skipping scope cleanup for still-live OUTER
// bindings (those below the block's floor) — leaking them. The fix cleans the
// outer prefix on the close-error path (errorInFlight=true) before returning.
//
// Scenario: an outer droppable `d` coexists with a nested block whose use-close
// fails. On the close-error early exit the outer `d` must be dropped.
func TestT1388_CloseErrEarlyExitCleansOuterPrefix(t *testing.T) {
	ir := generateIR(t, `
		type CErr is error { int code; }
		type FR { int id; close!(~this) void { raise CErr(message: "x", code: 1); } }
		type Drp { int id; drop(~this) { } }
		f!() int {
			d := Drp(id: 1);
			{ use r := FR(id: 2); }
			return d.id;
		}
	`)

	// The close-error early-exit block must FIRST clean the outer prefix — it
	// loads `d`'s drop flag and branches to a conditional drop, rather than
	// immediately loading the captured error and returning.
	assertContainsMatch(t, ir, `(?s)close\.err\.ret\.\d+:\s*%\d+ = load i1, i1\* %d\.dropflag`)

	// A Drp.drop call must be reachable from the close-error path (the outer
	// prefix cleanup), in addition to the normal success-exit drop.
	assertContains(t, ir, "call void @Drp.drop")
}

// Guards the exact shape the fix removed: the close-error return block must NOT
// jump straight to loading the captured error value (which would skip the outer
// prefix cleanup). With the fix the first instruction is the outer drop-flag load.
func TestT1388_CloseErrRetDoesNotImmediatelyReturn(t *testing.T) {
	ir := generateIR(t, `
		type CErr2 is error { int code; }
		type FR2 { int id; close!(~this) void { raise CErr2(message: "x", code: 1); } }
		type Drp2 { int id; drop(~this) { } }
		g!() int {
			d := Drp2(id: 1);
			{ use r := FR2(id: 2); }
			return d.id;
		}
	`)

	// Pre-fix shape: `close.err.ret.NN:` immediately loads close.err.val. The fix
	// interposes the outer-prefix cleanup, so this bare pattern must be absent.
	assertNotContainsMatch(t, ir, `(?s)close\.err\.ret\.\d+:\s*%\d+ = load i8\*, i8\*\* %close\.err\.val`)
}
