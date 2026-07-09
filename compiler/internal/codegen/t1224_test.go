package codegen

import (
	"testing"
)

// T1224 — `consume(move r!)` where r is an Optional single-owner handle must
// neutralize the source optional's present flag on the consume path. The
// force-unwrap moves the inner handle out and the callee's move param drops it,
// so leaving r's present flag set makes r's scope-exit drop re-free the same
// handle → double-free / segfault at 0x0. The fix mirrors the constructor arg
// paths by calling neutralizeForceUnwrapSource in the move-param arg branch.
// Without the fix, the `store i1 false` into r's present-flag slot is absent.
func TestT1224_ForceUnwrapMoveIntoSinkNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		consume_mutex(Mutex[int] move m) {}
		single_force_move_into_sink() {
			Mutex[int]? r = Mutex[int](5);
			consume_mutex(move r!);
		}
		main() { single_force_move_into_sink(); }
	`)
	// The neutralize stores i1 false into r's optional present-flag slot
	// (field 0 of the `{ i1, i8* }` optional) before the consume call. This
	// GEP into %r's present flag is present only when the source is neutralized.
	assertContains(t, ir, "getelementptr { i1, i8* }, { i1, i8* }* %r, i32 0, i32 0")
}
