package codegen

import (
	"strings"
	"testing"
)

// T0881: a user-defined (non-native) prefix unary operator inherited from a
// parent type must dispatch — sema previously rejected it (only the operand
// type's own methods were scanned, not its is-parents/structural parents),
// leaving the genUnaryExpr inherited-dispatch branch (added in T0878)
// unreachable. The fix walks parents in both sema (LookupUnaryMethod) and
// codegen, lighting up the existing direct-dispatch branch.

// (a) Derived inheriting a concrete parent's unary operator: the call targets
// the parent's operator method directly (resolveMonoParentName path).
func TestT0881_InheritedConcreteUnaryOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			int v;
			-() Base { return Base(v: -this.v); }
		}
		type Derived is Base {}
		caller() {
			d := Derived(v: 5);
			m := -d;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@Base.-$unary(i8*") {
		t.Fatalf("expected inherited unary dispatch `@Base.-$unary(i8* ...)` in caller:\n%s", body)
	}
}

// (b) Structural-interface default unary operator: the concrete type's operator
// method is synthesized (ensureDefaultMethodsSynthesized) and dispatched under
// the concrete name (findStructuralOwner path).
func TestT0881_StructuralDefaultUnaryOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Negatable `+"`structural"+` {
			int v;
			-() Negatable { return this; }
		}
		type Item is Negatable { int v; }
		caller() {
			it := Item(v: 5);
			m := -it;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@Item.-$unary(i8*") {
		t.Fatalf("expected synthesized structural unary dispatch `@Item.-$unary(i8* ...)` in caller:\n%s", body)
	}
	// The structural default method body must be synthesized on the concrete type.
	if extractFunction(ir, "Item.-$unary") == "" {
		t.Fatalf("expected synthesized @Item.- definition in IR:\n%s", ir)
	}
}

// (c) Derived inheriting a generic parent's unary operator: the operator method
// is monomorphized and the call targets the mono-mangled parent name. Unary
// operators have zero params, so only the result type needs substituting — the
// param-substitution failure mode that blocks the binary equivalent does not
// apply.
func TestT0881_InheritedGenericUnaryOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Base[T] {
			T v;
			-() Base[T] { return this; }
		}
		type Derived is Base[int] {}
		caller() {
			d := Derived(v: 5);
			m := -d;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Base[int].-$unary\"(i8*") {
		t.Fatalf("expected mono inherited unary dispatch `@\"Base[int].-$unary\"(i8* ...)` in caller:\n%s", body)
	}
}
