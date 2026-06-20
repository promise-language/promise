package codegen

import (
	"testing"
)

// T0861: An owned (~) structural-interface-view value parameter must be dropped
// at the callee's exit. Previously such a parameter got no drop binding — the
// caller cleared its source drop flag at the move site, so the backing concrete
// instance leaked. The fix registers an RTTI-dispatched free binding (the same
// __promise_structural_drop path used for structural fields), so the callee
// drops the backing instance at scope exit.

func TestT0861OwnedStructuralParamDrop(t *testing.T) {
	ir := generateIR(t, `
		type W `+"`"+`structural {
			write!(u8[] ~buf) int `+"`"+`abstract;
		}
		type ConcreteW {
			int n;
			write!(u8[] ~buf) int { return this.n; }
		}
		sink(W move w) {}
		main() {
			sink(ConcreteW(n: 0));
		}
	`)
	fn := extractFunction(ir, "__user.sink")
	if fn == "" {
		t.Fatal("expected sink to be emitted")
	}
	// The owned ~W param must be dropped at callee exit via inlined RTTI dispatch
	// (emitStructuralInstanceDrop reads typeinfo.drop_fn_ptr, falling back to
	// pal_free) — the struct.drop.* blocks are emitted only by that path.
	assertContains(t, fn, "struct.drop.call")
	assertContains(t, fn, "struct.drop.free")
}

// Control: a borrow (non-owned) structural param must NOT emit a drop — the
// caller retains ownership.
func TestT0861BorrowStructuralParamNoDrop(t *testing.T) {
	ir := generateIR(t, `
		type W `+"`"+`structural {
			write!(u8[] ~buf) int `+"`"+`abstract;
		}
		type ConcreteW {
			int n;
			write!(u8[] ~buf) int { return this.n; }
		}
		sink(W w) {}
		main() {
			ConcreteW c = ConcreteW(n: 0);
			sink(c);
		}
	`)
	fn := extractFunction(ir, "__user.sink")
	if fn == "" {
		t.Fatal("expected sink to be emitted")
	}
	// A borrow param does not own the backing instance — no drop in the callee.
	assertNotContains(t, fn, "struct.drop.call")
}
