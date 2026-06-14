package codegen

import (
	"strings"
	"testing"
)

// T0880: a user-defined (non-native) `++`/`--` operator must dispatch through
// genIncDecTarget. Previously genIncDecTarget unconditionally called
// emitNativeOp, which panicked ("native method ++ on non-primitive type ...")
// for any non-native ++/--. The fix routes inc/dec through emitUnaryOpResult
// (shared with the prefix-unary path T0878) and drops the old heap-owned value
// before storing the operator's result back into the lvalue (zero-leak policy).

// Value type: the receiver is materialized via valueTypeReceiverPtr (i8*) and
// the operator returns the value struct by value. No drop-old (value types own
// no heap memory).
func TestT0880_ValueTypeIncDecDispatch(t *testing.T) {
	ir := generateIR(t, `
		type VCounter {
			int n `+"`value"+`;
			++() VCounter { return VCounter(n: this.n + 1); }
		}
		caller() {
			c := VCounter(n: 0);
			c++;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"VCounter.++\"(i8*") {
		t.Fatalf("expected value-type inc/dec dispatch `@\"VCounter.++\"(i8* ...)` in caller:\n%s", body)
	}
	if strings.Contains(body, "incdec.userdrop") {
		t.Fatalf("did not expect a drop-old block for a value-type inc/dec:\n%s", body)
	}
}

// Heap Named type: the operator method receives the instance pointer, and the
// old (no-drop) instance is freed before the new result is stored back.
func TestT0880_HeapNamedIncDecDispatchAndDropOld(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int n;
			++() Counter { return Counter(n: this.n + 1); }
		}
		caller() {
			c := Counter(n: 0);
			c++;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Counter.++\"(i8*") {
		t.Fatalf("expected heap Named inc/dec dispatch `@\"Counter.++\"(i8* ...)` in caller:\n%s", body)
	}
	// Drop-old: the previous instance is freed (guarded by null/alias check) so
	// the new instance returned by ++ does not leak the old one.
	if !strings.Contains(body, "incdec.userdrop") {
		t.Fatalf("expected drop-old block `incdec.userdrop` in caller:\n%s", body)
	}
}

// Generic instance: the operator method is monomorphized and the call targets
// the mono-mangled name (Box[int].++), under the active typeSubst.
func TestT0880_GenericInstanceIncDecDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T v;
			++() Box[T] { return Box[T](v: this.v); }
		}
		caller() {
			b := Box[int](v: 1);
			b++;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Box[int].++\"(i8*") {
		t.Fatalf("expected mono inc/dec dispatch `@\"Box[int].++\"(i8* ...)` in caller:\n%s", body)
	}
}

// Virtual dispatch: when the static type has a child (needs a vtable), the
// operator is called indirectly through a receiver-only function pointer loaded
// from the vtable — not a direct named call.
func TestT0880_VirtualIncDecDispatch(t *testing.T) {
	ir := generateIR(t, `
		type VBase {
			int n;
			++() VBase { return VBase(n: this.n + 1); }
		}
		type VDerived is VBase {}
		caller() {
			a := VBase(n: 5);
			a++;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "to { i8*, i8* } (i8*)*") {
		t.Fatalf("expected virtual inc/dec dispatch via receiver-only fn ptr in caller:\n%s", body)
	}
	if strings.Contains(body, "@\"VBase.++\"(i8*") {
		t.Fatalf("expected indirect (vtable) dispatch, found direct call in caller:\n%s", body)
	}
}

// Plain (non-generic) enum: dispatch through the mangled operator method with an
// i8* receiver, via a synthesized enum.this temp for a non-`this` operand.
func TestT0880_EnumIncDecDispatch(t *testing.T) {
	ir := generateIR(t, `
		enum Dir {
			North,
			East,
			++() Dir {
				match this {
					North => { return Dir.East; },
					East => { return Dir.North; },
				}
			}
		}
		caller() {
			d := Dir.North;
			d++;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Dir.++\"(i8*") {
		t.Fatalf("expected enum inc/dec dispatch `@\"Dir.++\"(i8* ...)` in caller:\n%s", body)
	}
	if !strings.Contains(body, "%enum.this") {
		t.Fatalf("expected synthesized enum.this receiver temp in caller:\n%s", body)
	}
}

// Generic enum: the operator method is monomorphized and the call targets the
// mono-mangled name (Opt[int].++).
func TestT0880_GenericEnumIncDecDispatch(t *testing.T) {
	ir := generateIR(t, `
		enum Opt[T] {
			nothing,
			some(T v),
			++() Opt[T] { return Opt[T].nothing; }
		}
		caller() {
			o := Opt[int].some(3);
			o++;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Opt[int].++\"(i8*") {
		t.Fatalf("expected mono enum inc/dec dispatch `@\"Opt[int].++\"(i8* ...)` in caller:\n%s", body)
	}
}

// Non-native indexed container: `s[i]++` on a type whose `[]`/`[]=` are
// user-defined (non-native) and whose `[]` returns the element directly drives
// genIncDecTarget's non-native index branch — read via `[]`, apply the operator,
// write via `[]=` (the `$set` variant). Verifies the getter, the operator, and
// the setter are all emitted (vs the native vector add-i64 path).
func TestT0880_NonNativeIndexIncDecDispatch(t *testing.T) {
	ir := generateIR(t, `
		type VBox {
			int n `+"`value"+`;
			++() VBox { return VBox(n: this.n + 1); }
		}
		type VSlot {
			VBox held;
			[](int i) VBox { return this.held; }
			[]=(int i, VBox b) { this.held = b; }
		}
		caller() {
			s := VSlot(held: VBox(n: 0));
			s[0]++;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"VSlot.[]\"(") {
		t.Fatalf("expected non-native index read `@\"VSlot.[]\"` in caller:\n%s", body)
	}
	if !strings.Contains(body, "@\"VSlot.[]=\"(") {
		t.Fatalf("expected non-native index write `@\"VSlot.[]=\"` in caller:\n%s", body)
	}
	if !strings.Contains(body, "@\"VBox.++\"(i8*") {
		t.Fatalf("expected operator dispatch `@\"VBox.++\"(i8* ...)` in caller:\n%s", body)
	}
}

// For-loop update on a primitive stays on the native path (emitNativeOp): no
// operator method call, just an integer add.
func TestT0880_ForLoopNativeIncDecUnchanged(t *testing.T) {
	ir := generateIR(t, `
		counter() int {
			sum := 0;
			for i := 0; i < 3; i++ {
				sum = sum + i;
			}
			return sum;
		}
		main() { x := counter(); }
	`)
	body := extractFunction(ir, "__user.counter")
	if body == "" {
		t.Fatalf("expected @__user.counter in IR:\n%s", ir)
	}
	if strings.Contains(body, "++(i8*") {
		t.Fatalf("did not expect an operator-method call for native i++:\n%s", body)
	}
	if !strings.Contains(body, "add i64") {
		t.Fatalf("expected native integer increment `add i64` in counter:\n%s", body)
	}
}
