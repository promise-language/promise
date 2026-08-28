package regress6

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
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
	ir := codegentest.GenerateIR(t, `
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
	gen := codegentest.ExtractGeneratorBody(t, ir, "FailResource.close")

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
