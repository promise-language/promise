package codegen

import (
	"testing"
)

// T0460: Storing a structural-interface as a field must emit drop dispatch
// through __promise_structural_drop in the wrapper's synthesized drop body.
// Before the fix, sema didn't mark the wrapper as needing a synthesized drop
// (fieldTypeHasDrop returned false for structural fields), and the codegen
// field walk silently skipped structural fields.

func TestT0460StructuralFieldSynthDrop(t *testing.T) {
	ir := generateIR(t, `
		type W `+"`"+`structural {
			write!(u8[] ~buf) int `+"`"+`abstract;
		}
		type ConcreteW {
			int n;
			write!(u8[] ~buf) int { return this.n; }
		}
		type Wrap {
			W _w;
			new(~this, W move w) { this._w = w; }
		}
		main() {
			Wrap mw = Wrap(ConcreteW(n: 0));
		}
	`)
	fn := extractFunction(ir, "Wrap.drop")
	if fn == "" {
		t.Fatal("expected synthesized Wrap.drop to be emitted")
	}
	assertContains(t, fn, "__promise_structural_drop")
}

func TestT0460StructuralFieldExplicitDrop(t *testing.T) {
	ir := generateIR(t, `
		type W `+"`"+`structural {
			write!(u8[] ~buf) int `+"`"+`abstract;
		}
		type ConcreteW {
			int n;
			write!(u8[] ~buf) int { return this.n; }
		}
		type Wrap {
			W _w;
			new(~this, W move w) { this._w = w; }
			drop(~this) {}
		}
		main() {
			Wrap mw = Wrap(ConcreteW(n: 0));
		}
	`)
	fn := extractFunction(ir, "Wrap.drop")
	if fn == "" {
		t.Fatal("expected Wrap.drop to be emitted")
	}
	// Post-body field walk must dispatch the structural field through RTTI.
	assertContains(t, fn, "__promise_structural_drop")
}

func TestT0460OptionalStructuralFieldSynthDrop(t *testing.T) {
	ir := generateIR(t, `
		type W `+"`"+`structural {
			write!(u8[] ~buf) int `+"`"+`abstract;
		}
		type ConcreteW {
			int n;
			write!(u8[] ~buf) int { return this.n; }
		}
		type Wrap {
			W? _w;
			new(~this, W? move w) { this._w = w; }
		}
		main() {
			W? maybe = none;
			Wrap mw = Wrap(maybe);
		}
	`)
	fn := extractFunction(ir, "Wrap.drop")
	if fn == "" {
		t.Fatal("expected synthesized Wrap.drop to be emitted for Optional[W] field")
	}
	assertContains(t, fn, "__promise_structural_drop")
}
