package codegen

import (
	"strings"
	"testing"
)

// T0876: a user-defined binary operator declared on an enum must dispatch.
// genBinaryExpr previously called extractNamed(leftType), which returns nil for
// enums, and panicked "cannot resolve Named type". The fix routes enum left
// operands to genEnumBinaryOp, which calls the mangled operator method with the
// i8* receiver convention used by genEnumMethodCall.
func TestT0876_EnumBinaryOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		enum ECmp {
			some(int x),
			< (ECmp other) bool {
				return this.unwrap() < other.unwrap();
			}
			unwrap(this) int { match this { some(x) => { return x; } } }
		}
		caller() bool {
			a := ECmp.some(2);
			b := ECmp.some(5);
			return a < b;
		}
		main() { x := caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// The operator must dispatch to the mangled enum operator method with an
	// i8* receiver (genEnumBinaryOp), not panic or fall through.
	if !strings.Contains(body, "@\"ECmp.<\"(i8*") {
		t.Fatalf("expected enum operator dispatch `@\"ECmp.<\"(i8* ...)` in caller:\n%s", body)
	}
}

// T0876: the generic-enum variant — the operator method is monomorphized
// (ECmp__int.<) and the call must target the mono-mangled name.
func TestT0876_GenericEnumBinaryOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		enum GCmp[T: Ordered] {
			one(T v),
			< (GCmp[T] other) bool {
				return this.unwrap() < other.unwrap();
			}
			unwrap(this) T { match this { one(v) => { return v; } } }
		}
		caller() bool {
			a := GCmp[int].one(2);
			b := GCmp[int].one(5);
			return a < b;
		}
		main() { x := caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"GCmp[int].<\"(i8*") {
		t.Fatalf("expected mono enum operator dispatch `@\"GCmp[int].<\"(i8* ...)` in caller:\n%s", body)
	}
}
