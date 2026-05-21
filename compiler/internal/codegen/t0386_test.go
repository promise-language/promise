package codegen

import (
	"strings"
	"testing"
)

// T0386: Slice assignment `vec[i:j] = src` for `string[]` element type must
// dup each string element inside the monomorphized `Vector[T].[:]=` body
// (Patch 1) and must NOT call `Vector.drop` on the source vector at the call
// site (Patch 2). Without Patch 1, B0195 didn't fire inside the generic body
// because `c.info.Types[ThisExpr]` returned the bare Named owner without
// TypeArgs bound; `types.AsVector` then returned false on the bare Named and
// per-element string-dup was skipped — element pointers were aliased between
// source and target, causing double-free at scope exit.

// TestT0386_SliceAssignDupsStringElementsInsideGenericMethod verifies Patch 1:
// the body of `Vector[string].[:]=` must contain `promise_string_new` (the
// dupString call) inside its per-iteration block. Without Patch 1, the body
// would contain only a raw load+store of the source pointer with no dup.
func TestT0386_SliceAssignDupsStringElementsInsideGenericMethod(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := string[]();
			s.push("a" + "");
			v := string[]();
			v.push("x" + "");
			v[0:1] = s;
		}
	`)
	body := extractFunction(ir, `"Vector[string].[:]="`)
	if body == "" {
		t.Fatalf("Vector[string].[:]= not in IR\nfull IR:\n%s", ir)
	}
	if !strings.Contains(body, "promise_string_new") {
		t.Errorf("expected Vector[string].[:]= to call promise_string_new (per-element dup, B0195/T0386 Patch 1)\nbody:\n%s", body)
	}
}

// TestT0386_HeapStringSliceAssignDoesNotForceFreeSource verifies Patch 2:
// for a `string[]` IdentExpr RHS, the call site of `[:]=` must NOT call
// `Vector.drop` on the source pointer right after the [:]= call (which
// would orphan the source's per-element strings, leaking them). Patch 1
// makes the body dup string elements, so the source retains independent
// ownership and must run normal scope cleanup.
func TestT0386_HeapStringSliceAssignDoesNotForceFreeSource(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := string[]();
			s.push("a" + "");
			v := string[]();
			v.push("x" + "");
			v[0:1] = s;
		}
	`)
	// User main() is wrapped as the goroutine coroutine `.goroutine.main`.
	// extractFunction finds the first definition by walking back from the
	// `@name(` marker — which matches the call site first; instead splice
	// out the body manually starting from "define i8* @.goroutine.main()".
	defStart := strings.Index(ir, "define i8* @.goroutine.main()")
	if defStart < 0 {
		t.Fatalf(".goroutine.main definition not in IR\nfull IR:\n%s", ir)
	}
	defEnd := strings.Index(ir[defStart:], "\n}\n")
	if defEnd < 0 {
		t.Fatalf("could not find end of .goroutine.main\nfrom defStart:\n%s", ir[defStart:])
	}
	mainFn := ir[defStart : defStart+defEnd+2]
	callIdx := strings.Index(mainFn, `call void @"Vector[string].[:]="`)
	if callIdx < 0 {
		t.Fatalf("expected .goroutine.main to call Vector[string].[:]=\n%s", mainFn)
	}
	// Patch 2 must skip the explicit B0313 destructive Vector.drop on the
	// source — but the source still goes through normal scope cleanup at end
	// of main. Find the rest of main after the [:]= call and confirm the
	// source's drop flag (s.dropflag) is still read for cleanup, i.e. it
	// hasn't been disarmed by clearDropFlag.
	rest := mainFn[callIdx:]
	if !strings.Contains(rest, "%s.dropflag") {
		t.Errorf("expected source vector s to still be subject to scope cleanup (s.dropflag read after [:]= call) — Patch 2 must not disarm source drop for string[] RHS\nmain after [:]=:\n%s", rest)
	}
}

// TestT0386_NonStringSliceAssignStillFreesSourceBacking is the negative-of-Patch-2
// counterpart: for non-string element types (int[]) the source is still aliased
// inside the [:]= body, so Patch 2's `skipB0313` branch must NOT fire. The call
// site must still emit the destructive `Vector.drop(srcPtr)` after the [:]= call
// (free source backing array) and the source's drop flag must be cleared so its
// normal scope cleanup is disarmed (avoids double-free of the freed backing array).
func TestT0386_NonStringSliceAssignStillFreesSourceBacking(t *testing.T) {
	ir := generateIR(t, `
		main() {
			src := int[]();
			src.push(1);
			v := int[]();
			v.push(99);
			v[0:1] = src;
		}
	`)
	defStart := strings.Index(ir, "define i8* @.goroutine.main()")
	if defStart < 0 {
		t.Fatalf(".goroutine.main definition not in IR\nfull IR:\n%s", ir)
	}
	defEnd := strings.Index(ir[defStart:], "\n}\n")
	if defEnd < 0 {
		t.Fatalf("could not find end of .goroutine.main\nfrom defStart:\n%s", ir[defStart:])
	}
	mainFn := ir[defStart : defStart+defEnd+2]
	callIdx := strings.Index(mainFn, `call void @"Vector[int].[:]="`)
	if callIdx < 0 {
		t.Fatalf("expected .goroutine.main to call Vector[int].[:]=\n%s", mainFn)
	}
	// Patch 2's non-string branch: emit Vector.drop(src) at the call site to
	// free the source's backing array (since clearDropFlag below disarms its
	// normal scope cleanup).
	rest := mainFn[callIdx:]
	if !strings.Contains(rest, "call void @Vector.drop") {
		t.Errorf("expected destructive Vector.drop call after Vector[int].[:]= — Patch 2 must keep B0313 for non-string element types\nmain after [:]=:\n%s", rest)
	}
}
