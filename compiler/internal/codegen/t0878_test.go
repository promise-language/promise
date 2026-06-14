package codegen

import (
	"strings"
	"testing"
)

// T0878: a user-defined (non-native) prefix unary operator must dispatch.
// genUnaryExpr previously hit an unimplemented path and panicked
// "non-native unary <Type>.<op> not yet implemented". The fix emits the
// operator method call with the correct receiver convention for Named/value
// types (mirroring genBinaryExpr) and enums (mirroring genEnumBinaryOp).

// Heap Named type: the operator method receives the instance pointer.
func TestT0878_NamedUnaryOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Neg {
			int v;
			-() Neg { return Neg(v: -this.v); }
		}
		caller() {
			n := Neg(v: 5);
			m := -n;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@Neg.-$unary(i8*") {
		t.Fatalf("expected Named unary dispatch `@Neg.-$unary(i8* ...)` in caller:\n%s", body)
	}
}

// Value type: the receiver is materialized via valueTypeReceiverPtr (i8*),
// and the operator returns the value struct by value.
func TestT0878_ValueTypeUnaryOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		type VNeg {
			int v `+"`value"+`;
			-() VNeg { return VNeg(v: -this.v); }
		}
		caller() {
			vn := VNeg(v: 5);
			vm := -vn;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@VNeg.-$unary(i8*") {
		t.Fatalf("expected value-type unary dispatch `@VNeg.-$unary(i8* ...)` in caller:\n%s", body)
	}
}

// Plain (non-generic) enum: dispatch through the mangled operator method with
// an i8* receiver, via a synthesized enum.this temp for a non-`this` operand.
func TestT0878_EnumUnaryOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		enum Sign {
			Positive,
			Negative,
			-() Sign {
				match this {
					Positive => { return Sign.Negative; },
					Negative => { return Sign.Positive; },
				}
			}
		}
		caller() {
			s := Sign.Negative;
			f := -s;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@Sign.-$unary(i8*") {
		t.Fatalf("expected enum unary dispatch `@Sign.-$unary(i8* ...)` in caller:\n%s", body)
	}
	if !strings.Contains(body, "%enum.this") {
		t.Fatalf("expected synthesized enum.this receiver temp in caller:\n%s", body)
	}
}

// Generic enum: the operator method is monomorphized and the call targets the
// mono-mangled name (Opt[int].-).
func TestT0878_GenericEnumUnaryOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		enum Opt[T] {
			nothing,
			some(T v),
			-() Opt[T] { return Opt[T].nothing; }
		}
		caller() {
			o := Opt[int].some(3);
			p := -o;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Opt[int].-$unary\"(i8*") {
		t.Fatalf("expected mono enum unary dispatch `@\"Opt[int].-$unary\"(i8* ...)` in caller:\n%s", body)
	}
}

// `this` operand inside a method body: the receiver pointer is passed directly,
// without a synthesized temp.
func TestT0878_UnaryOperatorOnThisReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Neg {
			int v;
			-() Neg { return Neg(v: -this.v); }
			twice() Neg {
				m := -this;
				return -m;
			}
		}
		main() {
			n := Neg(v: 5);
			t := n.twice();
		}
	`)
	body := extractFunction(ir, "Neg.twice")
	if body == "" {
		t.Fatalf("expected @Neg.twice in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@Neg.-$unary(i8*") {
		t.Fatalf("expected Named unary dispatch `@Neg.-$unary(i8* ...)` in twice:\n%s", body)
	}
}

// Virtual dispatch on a non-`this` operand: `-a` where `a`'s static type needs
// a vtable (it has a child) exercises genVirtualUnaryOp's else branch, which
// pulls the vtable and instance out of the operand value (extractVtablePtr /
// extractInstancePtr) rather than from `this`.
func TestT0878_VirtualUnaryOperatorDispatchNonThis(t *testing.T) {
	ir := generateIR(t, `
		type VOpBase {
			int n;
			-() VOpBase { return VOpBase(n: -this.n); }
		}
		type VOpDerived is VOpBase {}
		caller() {
			a := VOpBase(n: 5);
			b := -a;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// The operand is a value (not `this`): the fn ptr is bitcast to a
	// receiver-only signature and called indirectly through the vtable.
	if !strings.Contains(body, "to { i8*, i8* } (i8*)*") {
		t.Fatalf("expected virtual unary dispatch via receiver-only fn ptr in caller:\n%s", body)
	}
	// Must NOT be a direct named call — virtual dispatch only.
	if strings.Contains(body, "@VOpBase.-$unary(i8*") {
		t.Fatalf("expected indirect (vtable) dispatch, found direct call in caller:\n%s", body)
	}
}

// Unary operator applied to `this` inside a *generic enum* method body: the
// operator method is monomorphized and the receiver is the method's own `this`
// i8* (genEnumUnaryOp's monoCtx + isThisReceiver branches), so the call targets
// the mono-mangled name with no synthesized temp.
func TestT0878_EnumUnaryOperatorThisInGenericMethod(t *testing.T) {
	ir := generateIR(t, `
		enum Opt[T] {
			nothing,
			some(T v),
			-() Opt[T] { return Opt[T].nothing; }
			double_neg(this) Opt[T] { return -this; }
		}
		caller() {
			o := Opt[int].some(3);
			p := o.double_neg();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "\"Opt[int].double_neg\"")
	if body == "" {
		t.Fatalf("expected @\"Opt[int].double_neg\" in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Opt[int].-$unary\"(i8*") {
		t.Fatalf("expected mono enum unary dispatch `@\"Opt[int].-$unary\"(i8* ...)` in double_neg:\n%s", body)
	}
	// `this` is passed directly — no synthesized enum.this temp here.
	if strings.Contains(body, "%enum.this") {
		t.Fatalf("did not expect synthesized enum.this temp for a `this` operand:\n%s", body)
	}
}

// Base type with a child: the unary operator dispatches virtually through the
// vtable (genVirtualUnaryOp), so the call goes through a receiver-only function
// pointer loaded from the object's typeinfo, not a direct named call.
func TestT0878_VirtualUnaryOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
		type VOpBase {
			int n;
			-() VOpBase { return VOpBase(n: -this.n); }
			neg_via(this) VOpBase { return -this; }
		}
		type VOpDerived is VOpBase {}
		main() {
			a := VOpBase(n: 5);
			c := a.neg_via();
		}
	`)
	body := extractFunction(ir, "VOpBase.neg_via")
	if body == "" {
		t.Fatalf("expected @VOpBase.neg_via in IR:\n%s", ir)
	}
	// Virtual dispatch: the function pointer is bitcast to a receiver-only
	// signature `{ i8*, i8* } (i8*)*` (no second operand) and called indirectly.
	if !strings.Contains(body, "bitcast i8* %") || !strings.Contains(body, "to { i8*, i8* } (i8*)*") {
		t.Fatalf("expected virtual unary dispatch via receiver-only fn ptr in neg_via:\n%s", body)
	}
}
