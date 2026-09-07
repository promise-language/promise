package regress11

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1905 — a view adapter was named from the interface member's BARE name, which a
// getter and its setter share (the `$set` marker lives only in the method mangler).
// Both vtable slots resolved to one symbol, so codegen emitted two definitions
// under it and opt rejected the module with `invalid redefinition of function`.
const t1905GetterSetterSrc = `
	type Prop ` + "`" + `structural {
	  get val! int ` + "`" + `abstract;
	  set val!(int v) ` + "`" + `abstract;
	}
	type Holder {
	  int n;
	  get val int => this.n;
	  set val(int v) { this.n = v; }
	}
	main() { Holder h = Holder(n: 5); Prop p = h; }
`

// The interface pair is failable and the concrete pair is not, so both members need
// an adapter — which is what made the collision reachable. Their return types differ
// ({i1, i64, i8*} for the int getter, {i1, i8*} for the void setter), so the two
// definitions could never have shared one symbol.
const (
	t1905GetterDef = "define { i1, i64, i8* } @Holder.val$view_adapt_as_Prop("
	t1905SetterDef = "define { i1, i8* } @Holder.val$set$view_adapt_as_Prop("
)

func TestT1905_GetterAndSetterAdaptersHaveDistinctNames(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1905GetterSetterSrc)
	if n := strings.Count(ir, t1905GetterDef); n != 1 {
		t.Fatalf("expected exactly one getter adapter definition, got %d", n)
	}
	if !strings.Contains(ir, t1905SetterDef) {
		t.Fatalf("expected a distinctly named setter adapter: %s", t1905SetterDef)
	}
	slots := vtableSlots(t, ir, "promise_vtable_Holder_as_Prop")
	if !strings.Contains(slots, "Holder.val$view_adapt_as_Prop") ||
		!strings.Contains(slots, "Holder.val$set$view_adapt_as_Prop") {
		t.Fatalf("both slots must point at their own adapter, got: %s", slots)
	}
}
