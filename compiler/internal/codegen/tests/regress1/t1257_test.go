package regress1

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1257: a single-owner / droppable value DECLARED IN THE CLASSIC-FOR INIT
// clause used to bypass the ownership bookkeeping that genTypedVarDecl performs.
// genClassicForStmt hand-rolled the init store, so it never claimed the RHS
// statement-temp (leaving its drop flag armed → a stray double-drop inside the
// loop body) and never registered a flag-guarded scope drop for the init
// variable (so a never-consumed owned init value leaked). The fix delegates the
// init clause to genTypedVarDecl/genInferredVarDecl.
//
// This test asserts an owned init value that is never consumed now gets a proper
// flag-guarded scope drop (fixing the latent leak): its drop flag bookkeeping is
// present and exactly one Counter.drop is emitted for it (scope-exit), with no
// stray extra drop from an unclaimed statement-temp.
func TestT1257_ClassicForInitRegistersDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter { int n; drop(~this) {} }
		mk() Counter { return Counter(n: 9); }
		caller() {
			c := 0;
			for it := mk(); c < 3; c = c + 1 {
				c = c + 1;
			}
		}
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// The init variable must get flag-guarded drop bookkeeping (proving the init
	// clause went through the var-decl path rather than a bare store).
	if !strings.Contains(body, "it.dropflag") {
		t.Fatalf("expected it.dropflag bookkeeping from the delegated var-decl path in caller:\n%s", body)
	}
	// The owned, never-consumed init value must be dropped (latent-leak fix). A
	// single flag-guarded drop binding materializes as exactly two @Counter.drop
	// call sites — the scope-exit path and the panic-cleanup path — both guarded
	// on it.dropflag so runtime performs one drop. The old hand-rolled path
	// registered no drop binding at all (0 call sites → leak); an unclaimed
	// statement-temp would add a stray third. Exactly two proves the fix.
	got := strings.Count(body, "@Counter.drop")
	if got != 2 {
		t.Fatalf("expected the init value to get one flag-guarded drop binding (2 @Counter.drop call sites: scope-exit + panic-cleanup), got %d:\n%s", got, body)
	}
}
