package codegen

import (
	"strings"
	"testing"
)

// T1188: A bare droppable `T` argument widened to an optional parameter must
// only transfer ownership to the callee for a `move` (RefMut) param. For a
// borrow param the caller retains the temp and drops it at scope exit — so no
// `heap.claim` ownership-release blocks are emitted in the caller. This mirrors
// the non-optional borrow arg `g(D(x:1))`, which never claims the temp.
func TestT1188OwnedOptionalParamBorrowRetainsTemp(t *testing.T) {
	borrowIR := generateIR(t, `
		type D { int x; }
		f0(D? o) int { return 0; }
		caller() int { return f0(D(x: 1)); }
	`)
	caller := extractFunc(borrowIR, "__user.caller")
	if caller == "" {
		t.Fatalf("caller function not found in IR:\n%s", borrowIR)
	}
	// Borrow: the temp stays tracked, so no ownership-transfer claim blocks.
	if strings.Contains(caller, "heap.claim") {
		t.Errorf("borrow optional param: caller must not claim (release) the heap temp\ngot:\n%s", caller)
	}
	// The caller still frees the retained temp at scope exit.
	assertContains(t, caller, "@pal_free")

	moveIR := generateIR(t, `
		type D { int x; }
		f0(D? move o) int { return 0; }
		caller() int { return f0(D(x: 1)); }
	`)
	callerMove := extractFunc(moveIR, "__user.caller")
	if callerMove == "" {
		t.Fatalf("caller function not found in IR:\n%s", moveIR)
	}
	// Move: ownership transfers to the callee, so the caller releases the temp.
	if !strings.Contains(callerMove, "heap.claim") {
		t.Errorf("move optional param: caller must claim (release) the heap temp\ngot:\n%s", callerMove)
	}

	// A `move`-ed NAMED local keeps `Arg.Value` an `IdentExpr` (with
	// `Arg.Move=true`), so the widening path additionally clears the local's
	// drop flag — the ownership-transfer branch the fresh-temp cases skip.
	// Ownership still moves to the callee (heap.claim present); the caller no
	// longer drops the local at scope exit.
	moveNamedIR := generateIR(t, `
		type D { int x; drop(~this){} }
		f0m(D? move o) int { return 0; }
		caller() int { d := D(x: 5); return f0m(move d); }
	`)
	callerNamed := extractFunc(moveNamedIR, "__user.caller")
	if callerNamed == "" {
		t.Fatalf("caller function not found in IR:\n%s", moveNamedIR)
	}
	if !strings.Contains(callerNamed, "heap.claim") {
		t.Errorf("move optional param (named local): caller must claim (release) the heap temp\ngot:\n%s", callerNamed)
	}
}
