package codegen

import (
	"strings"
	"testing"
)

// TestT1103_MapIndexAssignClearsInlineEnumCtorTemp pins the codegen fix for
// T1103: storing an inline-constructed droppable enum into a container via
// index-assignment (`m[k] = Holder.Pair(...)`) must clear the enum constructor
// temporary's drop flag. The enum is moved into Map.[]=, transferring ownership
// of its heap payload (here a tuple carrying a Ref) to the map. Pre-fix the
// enum-ctor temp's drop flag stayed set, so cleanupStmtTemps dropped the temp at
// statement end — recursively freeing the variant's Ref the map now owns →
// use-after-free on later reads (the int field, stored inline, survived; only
// the Ref pointer was corrupted). The fix mirrors the var-decl (B0267) and
// field-assign (B0269) enumCtorTemps clears.
//
// We assert the caller body emits no direct drop of the moved enum temporary
// (the only drop is the map's own scope-exit drop, which legitimately frees the
// owned value once). The runtime no-UAF proof is in
// tests/e2e/map_enum_ctor_move_test.pr.
func TestT1103_MapIndexAssignClearsInlineEnumCtorTemp(t *testing.T) {
	ir := generateIR(t, `
		enum Holder[T] { Empty, Pair((Ref[T], int) pair), }
		caller() {
			m := Map[int, Holder[int]]();
			m[1] = Holder[int].Pair((Ref[int](9), 2));
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	// Pre-fix the inline enum-ctor temp was dropped at statement end via a
	// direct Holder[int].drop call inside the caller. Post-fix the only drop of
	// the enum value is transitive, through the map's scope-exit Map.drop — the
	// caller must not directly drop the moved temporary.
	if strings.Contains(body, `call void @"Holder[int].drop"`) {
		t.Errorf("T1103: caller directly drops the inline enum-ctor temp after "+
			"moving it into Map.[]= — the enumCtorTemps drop flag was not cleared, "+
			"so the map's owned Ref payload is freed early (use-after-free):\n%s", body)
	}
	// Sanity: the move into the container must actually occur.
	if !strings.Contains(body, `call void @"Map[int, Holder[int]].[]="`) {
		t.Fatalf("T1103: expected a Map.[]= move in caller body:\n%s", body)
	}
}
