package codegen

import (
	"strings"
	"testing"
)

// T0984: a plain binary/unary/inc-dec use of a failable user-defined operator
// previously emitted malformed IR — the operator's {i1, T, i8*} result struct
// was stored directly into a T-typed slot (panic "store operands are not
// compatible"). Sema now requires a failable enclosing scope and codegen
// auto-propagates the result (genAutoPropagateValue), symmetric with the T0715
// compound-assignment fix. These tests assert the auto-propagate branch is
// emitted and the tuple never reaches the store.

// Plain binary `a + b` with a failable value-type operator: the {ok, value, err}
// result is unwrapped via auto.propagate/auto.ok before the success value is
// stored into the var-decl slot.
func TestT0984_FailableBinaryOpAutoPropagates(t *testing.T) {
	ir := generateIR(t, `
		type Vec {
			int x `+"`value"+`;
			+!(Vec other) Vec { if other.x < 0 { raise error("neg"); } return Vec(x: this.x + other.x); }
		}
		caller!() {
			a := Vec(x: 10);
			b := Vec(x: 5);
			c := a + b;
		}
		main() {}
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Vec.+\"(i8*") {
		t.Fatalf("expected operator dispatch `@\"Vec.+\"(i8* ...)` in caller:\n%s", body)
	}
	if !strings.Contains(body, "auto.propagate") || !strings.Contains(body, "auto.ok") {
		t.Fatalf("expected auto-propagate of the failable operator result in caller:\n%s", body)
	}
}

// Prefix unary `-a` with a failable value-type operator auto-propagates too.
func TestT0984_FailableUnaryOpAutoPropagates(t *testing.T) {
	ir := generateIR(t, `
		type Vec {
			int x `+"`value"+`;
			-!() Vec { if this.x < 0 { raise error("neg"); } return Vec(x: -this.x); }
		}
		caller!() {
			a := Vec(x: 10);
			c := -a;
		}
		main() {}
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "auto.propagate") || !strings.Contains(body, "auto.ok") {
		t.Fatalf("expected auto-propagate of the failable unary operator result in caller:\n%s", body)
	}
}

// Inc/dec `a++` with a failable value-type operator: the store-back value is the
// unwrapped success value, so the auto-propagate branch precedes the store.
func TestT0984_FailableIncDecOpAutoPropagates(t *testing.T) {
	ir := generateIR(t, `
		type Vec {
			int x `+"`value"+`;
			++!() Vec { if this.x < 0 { raise error("neg"); } return Vec(x: this.x + 1); }
		}
		caller!() {
			a := Vec(x: 10);
			a++;
		}
		main() {}
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "auto.propagate") || !strings.Contains(body, "auto.ok") {
		t.Fatalf("expected auto-propagate of the failable inc/dec operator result in caller:\n%s", body)
	}
}
