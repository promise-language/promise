package codegen

import (
	"testing"
)

// T1267: a bare failable call in a `match` *expression* arm must auto-propagate
// like any other bare call site. Two manifestations were fixed:
//   A (void position): the error-union was discarded unbranched → silent swallow.
//   B (value position): the raw {i1,i64,i8*} union leaked into the arm's result
//      slot (typed as the arm value type) → wrapOk/store type-mismatch panic.
// genMatchArmValue now honors AutoPropagateExprs for arm.Body, emitting the
// auto.propagate/auto.ok branch and yielding the unwrapped success value.

// Void position: the ok-flag branch must be emitted (manifestation A).
func TestT1267VoidExprArmEmitsPropagateBranch(t *testing.T) {
	ir := generateIR(t, `
		boom!() { raise error(message: "bang"); }
		dispatch!(string c) {
			match c {
				"a" => boom(),
				_ => {},
			}
		}
		main() {}
	`)
	body := extractFunction(ir, "__user.dispatch")
	if body == "" {
		t.Fatalf("expected @__user.dispatch in IR:\n%s", ir)
	}
	// The auto-propagation branch (was missing → silent swallow before T1267).
	assertContains(t, body, "auto.propagate")
	assertContains(t, body, "auto.ok")
}

// Value position: compiles without panic and emits the propagate branch (B).
func TestT1267ValueExprArmCompilesAndPropagates(t *testing.T) {
	ir := generateIR(t, `
		boom_val!() int { raise error(message: "bang"); }
		f!(string c) int {
			return match c {
				"a" => boom_val(),
				_ => 0,
			};
		}
		main() {}
	`)
	body := extractFunction(ir, "__user.f")
	if body == "" {
		t.Fatalf("expected @__user.f in IR:\n%s", ir)
	}
	assertContains(t, body, "auto.propagate")
	assertContains(t, body, "auto.ok")
}

// Value position via a typed declaration — the store must not hit a type
// mismatch (manifestation B, the assign form).
func TestT1267ValueExprArmDeclCompiles(t *testing.T) {
	ir := generateIR(t, `
		boom_val!() int { raise error(message: "bang"); }
		f!(string c) int {
			int v = match c {
				"a" => boom_val(),
				_ => 0,
			};
			return v;
		}
		main() {}
	`)
	body := extractFunction(ir, "__user.f")
	if body == "" {
		t.Fatalf("expected @__user.f in IR:\n%s", ir)
	}
	assertContains(t, body, "auto.propagate")
}
