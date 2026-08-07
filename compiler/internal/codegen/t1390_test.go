package codegen

import (
	"strings"
	"testing"
)

// T1390: a use-binding whose failable close() fails on the SUCCESS exit of a
// failable generator body must route the close error into the generator's error
// slot (so it surfaces at the failable consumer) instead of taking
// emitCloseCall's suppress path — which drops the error via @__mod_std_error.drop
// and reports success.
//
// The fix broadens emitScopeCleanup's close-error capture guard to fire inside a
// failable generator (where c.canError is false because the generator ramp
// returns i8*, but c.generatorCanError is true), so emitCloseCall takes the
// CAPTURE path and emitCloseErrCheck's `c.inGenerator && c.generatorCanError`
// branch (B0023 emitGeneratorError) is reached instead of being dead code.
func TestT1390_GeneratorUseCloseErrorRoutesToErrorSlot(t *testing.T) {
	ir := generateIR(t, `
		type CloseError is error { int code; }
		type FailResource {
			int id;
			close!(~this) void { raise CloseError(message: "cf", code: 42); }
		}
		gen!() stream[int] {
			use r := FailResource(id: 1);
			yield r.id;
		}
		main() { }
	`)
	gen := extractGeneratorBody(t, ir, "FailResource.close")

	// The close-error CAPTURE path is active inside the generator coroutine body.
	// The capture allocas and the "save first error" block only exist on the
	// capture path (emitScopeCleanup allocated a cap because
	// c.inGenerator && c.generatorCanError); their presence proves we did NOT take
	// emitCloseCall's suppress path (which would drop the error and has neither).
	if !strings.Contains(gen, "close.err.flag") {
		t.Errorf("expected close-error capture alloca in the generator coroutine body (capture path taken):\n%s", gen)
	}
	if !strings.Contains(gen, "close.save") {
		t.Errorf("expected the capture-path close.save block (proves the suppress path was NOT taken):\n%s", gen)
	}

	// emitCloseErrCheck routes the captured error into the generator error slot:
	// a close.err.ret block that stores the error into error_slot.addr and
	// branches to the generator's final suspend.
	if !strings.Contains(gen, "close.err.ret") {
		t.Errorf("expected a close.err.ret error-check block routing to the generator error slot:\n%s", gen)
	}
	if !strings.Contains(gen, "error_slot.addr") {
		t.Errorf("expected the close error to be stored into the generator error slot (error_slot.addr):\n%s", gen)
	}
	if !strings.Contains(gen, "final.suspend") {
		t.Errorf("expected the close-error path to branch to the generator final.suspend:\n%s", gen)
	}
}

// extractGeneratorBody returns the IR text of the `@.generator.*` coroutine
// definition that contains `marker`, so assertions are scoped to the user
// generator's ramp (not the enclosing factory or std generators).
func extractGeneratorBody(t *testing.T, ir, marker string) string {
	t.Helper()
	const prefix = "define i8* @.generator."
	search := ir
	base := 0
	for {
		rel := strings.Index(search, prefix)
		if rel < 0 {
			break
		}
		defStart := base + rel
		rest := ir[defStart:]
		end := strings.Index(rest, "\n}\n")
		body := rest
		if end >= 0 {
			body = rest[:end]
		}
		if strings.Contains(body, marker) {
			return body
		}
		base = defStart + len(prefix)
		search = ir[base:]
	}
	t.Fatalf("no generator coroutine definition containing %q found in IR:\n%s", marker, ir)
	return ""
}
