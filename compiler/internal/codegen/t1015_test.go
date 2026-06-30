package codegen

import (
	"testing"
)

// T1015: a compound assignment (`+=`, `-=`, etc.) whose operand TYPE is an enum
// with a user-defined operator previously panicked codegen
// ("cannot resolve Named type from <Enum> for compound assignment +="). The enum
// analogue of T0715 (Named) / T0876 (plain `a + b`): genCompoundOp resolved the
// operand via extractNamed (nil for enums) before any dispatch ran. It now falls
// back to genNonNativeEnumCompoundOp, mirroring genBinaryExpr's enum path. Because
// the panic fired first, the enum branches of dropOldUserValueAtPtr (member/[]/ident)
// and emitDropOldCompoundValue ([:] getter sites) were dead — these tests confirm
// they are now live (the old enum value drops via @Tag.drop, no leak).

// Member target: `h.t += enumval`. The operator dispatches to the enum's `+`
// method (i8* receiver), and the old field value is dropped before the result is
// stored back (drop-old via dropOldUserValueAtPtr's enum branch → @Tag.drop).
func TestT1015_EnumMemberCompoundOpDispatch(t *testing.T) {
	ir := generateIR(t, `
		enum Tag {
			Named(string name),
			Empty,
			+(Tag other) Tag { return Tag.Named(name: "merged"); }
			drop(~this) {}
		}
		type Holder { Tag t; }
		caller() {
			h := Holder(t: Tag.Named(name: "a"));
			h.t += Tag.Empty;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// Enum operator dispatch (i8* receiver) — no panic.
	assertContains(t, body, `@"Tag.+"(i8*`)
	// Drop-old enum branch is now reachable: the old field value drops before the
	// new result overwrites it (zero-leak), via @Tag.drop.
	assertContains(t, body, "@Tag.drop(i8*")
}

// Index target: `arr[i] += enumval`. The enum vector element is read, the operator
// applied, and the old element dropped before the slot is overwritten.
func TestT1015_EnumIndexCompoundOpDispatch(t *testing.T) {
	ir := generateIR(t, `
		enum Tag {
			Named(string name),
			Empty,
			+(Tag other) Tag { return Tag.Named(name: "merged"); }
			drop(~this) {}
		}
		caller() {
			arr := [Tag.Named(name: "a"), Tag.Empty];
			arr[0] += Tag.Empty;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	assertContains(t, body, `@"Tag.+"(i8*`)
	assertContains(t, body, "@Tag.drop(i8*")
}

// Slice target: `obj[a:b] += enumval` (T0714's slice-compound path). The `[:]`
// getter returns a fresh enum value; the operator runs; the old getter-returned
// value is dropped (emitDropOldCompoundValue's enum branch → @Tag.drop, now live)
// before the `[:]=` write.
func TestT1015_EnumSliceCompoundOpDispatch(t *testing.T) {
	ir := generateIR(t, `
		enum Tag {
			Named(string name),
			Empty,
			+(Tag other) Tag { return Tag.Named(name: "merged"); }
			drop(~this) {}
		}
		type Box {
			Tag t;
			[:](int? low, int? high) Tag { return Tag.Named(name: "g"); }
			[:]=(int? low, int? high, Tag move v) { this.t = v; }
		}
		caller() {
			b := Box(t: Tag.Empty);
			b[0:1] += Tag.Empty;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	assertContains(t, body, `@"Box.[:]"(i8*`)
	assertContains(t, body, `@"Tag.+"(i8*`)
	// emitDropOldCompoundValue's enum branch drops the fresh getter value.
	assertContains(t, body, "@Tag.drop(i8*")
	assertContains(t, body, `@"Box.[:]="(i8*`)
}

// Generic enum operand: `b += Box[int].Full(...)` where the enum is a generic
// instance. Exercises genNonNativeEnumCompoundOp's `*types.Instance` branch —
// the operator must dispatch through the MONO name (`Box[int].+`), not the bare
// origin name (`Box.+`), and the old value drops via the mono'd `Box[int].drop`.
func TestT1015_GenericEnumCompoundOpDispatch(t *testing.T) {
	ir := generateIR(t, `
		enum Box[T] {
			Full(T value),
			Empty,
			+(Box[T] other) Box[T] { return Box.Full(value: other.unwrap()); }
			unwrap(this) T { return match this { Full(v) => v, Empty => zero(), }; }
			drop(~this) {}
		}
		zero() int { return 0; }
		caller() {
			b := Box[int].Full(value: 1);
			b += Box[int].Full(value: 7);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// Mono-name dispatch (Instance branch), NOT the bare origin name.
	assertContains(t, body, `@"Box[int].+"(i8*`)
	assertNotContains(t, body, `@"Box.+"(i8*`)
	// Drop-old fires through the mono'd drop.
	assertContains(t, body, `@"Box[int].drop"(i8*`)
}

// Generic enum compound assignment INSIDE the generic enum's own method body
// (`acc += x` where acc is Box[T]). Monomorphized to Box[int], this exercises the
// name-resolution path active under c.monoCtx (the method is being defined with a
// concrete substitution) — the operator still dispatches to the mono'd `Box[int].+`.
func TestT1015_GenericEnumCompoundOpInGenericMethod(t *testing.T) {
	ir := generateIR(t, `
		enum Box[T] {
			Full(T value),
			Empty,
			+(Box[T] other) Box[T] { return Box.Full(value: other.unwrap()); }
			unwrap(this) T { return match this { Full(v) => v, Empty => zero(), }; }
			combine(this, Box[T] move x) Box[T] {
				acc := Box[T].Full(value: this.unwrap());
				acc += x;
				return acc;
			}
			drop(~this) {}
		}
		zero() int { return 0; }
		caller() {
			a := Box[int].Full(value: 1);
			r := a.combine(Box[int].Full(value: 9));
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, `"Box[int].combine"`)
	if body == "" {
		t.Fatalf("expected @Box[int].combine in IR:\n%s", ir)
	}
	// The compound `acc += x` dispatches the mono'd operator (a real call, not the
	// definition) from inside the monomorphized method body.
	assertContains(t, body, `call %"promise_Box[int]_enum" @"Box[int].+"(i8*`)
}

// Failable enum operator (`+!`) in a failable scope: the operator returns
// {i1, T, i8*}; genNonNativeEnumCompoundOp unwraps it and auto-propagates the
// error (sema guarantees the enclosing scope is failable via
// compoundOperatorCanError).
func TestT1015_FailableEnumCompoundOpPropagates(t *testing.T) {
	ir := generateIR(t, `
		enum Tag {
			Named(string name, int v),
			Empty,
			+!(Tag other) Tag {
				if other.value() < 0 { raise error("neg"); }
				return Tag.Named(name: "m", v: 1);
			}
			value(this) int {
				return match this {
					Named(n, v) => v,
					Empty => 0,
				};
			}
			drop(~this) {}
		}
		type Holder { Tag t; }
		caller!() {
			h := Holder(t: Tag.Named(name: "a", v: 2));
			h.t += Tag.Named(name: "b", v: 3);
		}
		main() { caller()? e {}; }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// Failable enum operator returns the {i1, enum, i8*} result struct.
	assertContains(t, body, `@"Tag.+"(i8*`)
	assertContains(t, body, "auto.propagate")
}
