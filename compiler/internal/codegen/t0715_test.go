package codegen

import (
	"strings"
	"testing"
)

// T0715: a compound assignment (`+=`, `-=`, etc.) whose operator is a
// user-defined (non-native) method previously panicked codegen
// ("non-native compound op ... not yet implemented"). genCompoundOp now routes
// non-native operators through genNonNativeCompoundOp, which mirrors
// genBinaryExpr's method dispatch (direct / mono / structural / virtual) with
// both operands as plain values (neither is `this`). The result feeds the
// existing store/setter path; a failable operator auto-propagates.

// Value type local: the receiver is materialized via valueTypeReceiverPtr (i8*)
// and the operator returns the value struct by value, stored back into the
// alloca. No drop-old block (value types own no heap memory).
func TestT0715_ValueTypeCompoundOpDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Vec {
			int x `+"`value"+`;
			+(Vec other) Vec { return Vec(x: this.x + other.x); }
		}
		caller() {
			v := Vec(x: 10);
			v += Vec(x: 5);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Vec.+\"(i8*") {
		t.Fatalf("expected value-type compound dispatch `@\"Vec.+\"(i8* ...)` in caller:\n%s", body)
	}
	if strings.Contains(body, "userdrop") {
		t.Fatalf("did not expect a drop-old block for a value-type compound assignment:\n%s", body)
	}
}

// Value type via getter/setter property (the exact tracker repro shape): read
// the current value via the getter, apply the operator, write via the setter.
func TestT0715_ValueTypePropertyCompoundOpDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Vec {
			int x `+"`value"+`;
			+(Vec other) Vec { return Vec(x: this.x + other.x); }
		}
		type Holder {
			Vec v;
			get vec Vec { return this.v; }
			set vec(Vec nv) { this.v = nv; }
		}
		caller() {
			h := Holder(v: Vec(x: 10));
			h.vec += Vec(x: 5);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@Holder.vec(") {
		t.Fatalf("expected getter call `@Holder.vec` in caller:\n%s", body)
	}
	if !strings.Contains(body, "@Holder.vec$set(") {
		t.Fatalf("expected setter call `@Holder.vec$set` in caller:\n%s", body)
	}
	if !strings.Contains(body, "@\"Vec.+\"(i8*") {
		t.Fatalf("expected operator dispatch `@\"Vec.+\"(i8* ...)` in caller:\n%s", body)
	}
}

// Heap Named type local: the operator method receives the instance pointer, and
// the old instance is dropped (alias-guarded) before the new result is stored
// back into the alloca (zero-leak policy). Reuses dropOldUserValueAtPtr (shared
// with T0880 inc/dec), whose drop block is labeled `incdec.userdrop`.
func TestT0715_HeapNamedCompoundOpDropOld(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int n;
			+(Counter other) Counter { return Counter(n: this.n + other.n); }
			drop(~this) {}
		}
		caller() {
			c := Counter(n: 0);
			c += Counter(n: 1);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Counter.+\"(i8*") {
		t.Fatalf("expected heap Named compound dispatch `@\"Counter.+\"(i8* ...)` in caller:\n%s", body)
	}
	if !strings.Contains(body, "userdrop") {
		t.Fatalf("expected drop-old block for a droppable heap user-type compound:\n%s", body)
	}
}

// Heap Named type as a vector element: the operator returns a fresh value, so
// the old element must be dropped (alias-guarded) before the result overwrites
// the slot — otherwise the previous heap instance leaks (zero-leak policy).
// Same dropOldUserValueAtPtr helper as the local path (drop block `incdec.userdrop`).
func TestT0715_HeapNamedVectorElementCompoundDropOld(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int n;
			+(Counter other) Counter { return Counter(n: this.n + other.n); }
			drop(~this) {}
		}
		caller() {
			v := [Counter(n: 0), Counter(n: 1)];
			v[0] += Counter(n: 2);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Counter.+\"(i8*") {
		t.Fatalf("expected vector-element compound dispatch `@\"Counter.+\"(i8* ...)` in caller:\n%s", body)
	}
	if !strings.Contains(body, "userdrop") {
		t.Fatalf("expected drop-old block for a droppable heap user-type vector-element compound:\n%s", body)
	}
}

// Generic instance: the operator method is monomorphized and the call targets
// the mono-mangled name (Box[int].+), under the active typeSubst.
func TestT0715_GenericInstanceCompoundOpDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T v;
			+(Box[T] other) Box[T] { return Box[T](v: this.v); }
		}
		caller() {
			b := Box[int](v: 1);
			b += Box[int](v: 2);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Box[int].+\"(i8*") {
		t.Fatalf("expected mono compound dispatch `@\"Box[int].+\"(i8* ...)` in caller:\n%s", body)
	}
}

// Virtual dispatch: when the static type has a child (needs a vtable), the
// operator is called indirectly through a function pointer loaded from the
// vtable — not a direct named call.
func TestT0715_VirtualCompoundOpDispatch(t *testing.T) {
	ir := generateIR(t, `
		type VBase {
			int n;
			+(VBase other) VBase { return VBase(n: this.n + other.n); }
			drop(~this) {}
		}
		type VDerived is VBase {}
		caller() {
			a := VBase(n: 5);
			a += VBase(n: 1);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if strings.Contains(body, "@\"VBase.+\"(i8*") {
		t.Fatalf("expected indirect (vtable) dispatch, found a direct call in caller:\n%s", body)
	}
	// Indirect call through a function pointer bitcast loaded from the vtable.
	if !strings.Contains(body, "bitcast i8* %") {
		t.Fatalf("expected virtual compound dispatch via a vtable fn ptr in caller:\n%s", body)
	}
}

// Inherited (nominal-parent) operator: a leaf subtype with no vtable inherits
// the parent's operator. genNonNativeCompoundOp resolves the owner to the parent
// (resolveMonoParentName branch) so the call targets the PARENT's mangled name,
// not the leaf's. The droppable parent also exercises the drop-old path.
func TestT0715_InheritedOperatorCompoundDispatch(t *testing.T) {
	ir := generateIR(t, `
		type OpBase {
			int n;
			+(OpBase other) OpBase { return OpBase(n: this.n + other.n); }
			drop(~this) {}
		}
		type OpLeaf is OpBase {}
		caller() {
			d := OpLeaf(n: 5);
			d += OpLeaf(n: 3);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"OpBase.+\"(i8*") {
		t.Fatalf("expected inherited compound dispatch to the parent owner `@\"OpBase.+\"(i8* ...)` in caller:\n%s", body)
	}
	if !strings.Contains(body, "userdrop") {
		t.Fatalf("expected drop-old block for a droppable inherited-operator compound:\n%s", body)
	}
}

// Structural-default operator: a structural interface supplies a default `+`
// body; a concrete type that `is` it inherits the operator. genNonNativeCompoundOp
// synthesizes the default under the CONCRETE name (findStructuralOwner branch)
// and dispatches there.
func TestT0715_StructuralDefaultOperatorCompoundDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Summable `+"`structural"+` {
			get amount int `+"`abstract"+`;
			+(Summable other) Money { return Money(v: this.amount + other.amount); }
		}
		type Money is Summable {
			int v;
			get amount int { return this.v; }
		}
		caller() {
			m := Money(v: 4);
			m += Money(v: 3);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Money.+\"(i8*") {
		t.Fatalf("expected structural-default compound dispatch under the concrete name `@\"Money.+\"(i8* ...)` in caller:\n%s", body)
	}
}

// Failable operator in a failable scope: the operator returns {i1, T, i8*}, so
// the result is unwrapped and the error auto-propagated via genAutoPropagateValue
// (the `auto.propagate` block). Sema guarantees the enclosing scope is failable.
func TestT0715_FailableCompoundOpPropagates(t *testing.T) {
	ir := generateIR(t, `
		type Vec {
			int x `+"`value"+`;
			+!(Vec other) Vec { if other.x < 0 { raise error("neg"); } return Vec(x: this.x + other.x); }
		}
		caller!() {
			v := Vec(x: 10);
			v += Vec(x: 5);
		}
		main() { caller()? e {}; }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Vec.+\"(i8*") {
		t.Fatalf("expected operator dispatch `@\"Vec.+\"(i8* ...)` in caller:\n%s", body)
	}
	if !strings.Contains(body, "auto.propagate") {
		t.Fatalf("failable compound operator must emit an `auto.propagate` block:\n%s", body)
	}
}
