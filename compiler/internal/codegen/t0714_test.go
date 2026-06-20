package codegen

import (
	"testing"
)

// T0714: a slice compound assignment (`v[a:b] += x`) previously dropped the
// operator and the current value — it lowered to a plain `[:]=` call with the
// raw RHS, silently behaving like `v[a:b] = x`. genSliceCompoundAssign now
// reads the current value via `[:]`, applies the operator, then writes via
// `[:]=` (mirrors the `[]` index compound and member compound paths).

// Basic read-modify-write: the IR must contain a `[:]` read, the arithmetic,
// and a `[:]=` write — the operator no longer vanishes.
func TestT0714_SliceCompoundReadModifyWrite(t *testing.T) {
	ir := generateIR(t, `
		type Span {
			int x;
			[:](int? low, int? high) int { return this.x; }
			[:]=(int? low, int? high, int v) { this.x = v; }
		}
		main() {
			s := Span(x: 10);
			s[0:1] += 5;
		}
	`)
	assertContains(t, ir, `call i64 @"Span.[:]"(`)
	assertContains(t, ir, `add i64`)
	assertContains(t, ir, `call void @"Span.[:]="(`)
}

// String operand: the getter returns a heap string, so the old value must be
// dropped before the new one is written (drop-old, T0363) to avoid a leak.
func TestT0714_SliceCompoundStringDropsOld(t *testing.T) {
	ir := generateIR(t, `
		type Span {
			string s;
			[:](int? low, int? high) string { return this.s; }
			[:]=(int? low, int? high, string v) { this.s = v; }
		}
		main() {
			x := Span(s: "a");
			x[0:1] += "b";
		}
	`)
	assertContains(t, ir, `call i8* @"Span.[:]"(`)
	assertContains(t, ir, `call void @"Span.[:]="(`)
	// Drop-old block guards against leaking the heap string the getter returned.
	assertContains(t, ir, "compound.strdrop")
	assertContains(t, ir, "@promise_string_drop")
}

// Heap user-type operand: the getter returns a freshly-allocated value that the
// `+` operator does not consume, so it must be dropped (alias-guarded) before the
// `[:]=` write — otherwise it leaks. The IR must contain the user drop-old block.
func TestT0714_SliceCompoundHeapUserTypeDropsOld(t *testing.T) {
	ir := generateIR(t, `
		type Money {
			int cents;
			+(Money other) Money { return Money(cents: this.cents + other.cents); }
			drop(~this) {}
		}
		type Account {
			Money balance;
			[:](int? low, int? high) Money { return Money(cents: this.balance.cents); }
			[:]=(int? low, int? high, ~Money v) { this.balance = v; }
		}
		main() {
			a := Account(balance: Money(cents: 10));
			a[0:1] += Money(cents: 5);
		}
	`)
	assertContains(t, ir, `call { i8*, i8* } @"Account.[:]"(`)
	// The fresh getter value is dropped (alias-guarded) before the [:]= write.
	assertContains(t, ir, "compound.userdrop")
	assertContains(t, ir, `call void @Money.drop(`)
}

// A failable `[:]` read must auto-propagate before the operator is applied; the
// arithmetic must operate on the extracted ok value, not the failable struct.
// T0709 reopened for slices by T0714.
func TestT0714_FailableSliceGetterCompoundPropagates(t *testing.T) {
	ir := generateIR(t, `
		type Span {
			int x;
			[:]!(int? low, int? high) int {
				if this.x < 0 { raise error("neg"); }
				return this.x;
			}
			[:]=(int? low, int? high, int v) { this.x = v; }
		}
		main!() {
			s := Span(x: 0);
			s[0:1] += 5;
		}
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "extractvalue { i1, i64, i8* }")
	// The arithmetic must NOT be applied to the raw failable struct.
	assertNotContains(t, ir, "add { i1, i64, i8* }")
}

// A failable RHS in a slice compound (`s[a:b] += f()` where `f` is failable)
// must auto-propagate the RHS before the operator runs — exercises the
// AutoPropagateExprs[valueExpr] branch of genSliceCompoundAssign. The slice
// getter is non-failable here so the propagation can only come from the RHS.
func TestT0714_SliceCompoundFailableRHSPropagates(t *testing.T) {
	ir := generateIR(t, `
		maybe!() int { return 5; }
		type Span {
			int x;
			[:](int? low, int? high) int { return this.x; }
			[:]=(int? low, int? high, int v) { this.x = v; }
		}
		main!() {
			s := Span(x: 10);
			s[0:1] += maybe();
		}
	`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, `call i64 @"Span.[:]"(`)
	assertContains(t, ir, "add i64")
	assertContains(t, ir, `call void @"Span.[:]="(`)
}

// A slice compound where the target is `this` inside a method — exercises the
// isThisReceiver branch of genSliceCompoundAssign (the receiver is already the
// i8* instance ptr, so no extractInstancePtr is emitted).
func TestT0714_SliceCompoundThisReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Span {
			int x;
			[:](int? low, int? high) int { return this.x; }
			[:]=(int? low, int? high, int v) { this.x = v; }
			bump(~this) { this[0:1] += 7; }
		}
		main() {
			s := Span(x: 1);
			s.bump();
		}
	`)
	assertContains(t, ir, `call i64 @"Span.[:]"(`)
	assertContains(t, ir, "add i64")
	assertContains(t, ir, `call void @"Span.[:]="(`)
}

// A slice compound inside a *generic type's* method body — exercises the
// typeSubst-substitution branches of genSliceCompoundAssign (targetType and
// operandType are substituted under the mono context). The monomorphized
// method must still read-modify-write.
func TestT0714_SliceCompoundGenericMethodTypeSubst(t *testing.T) {
	ir := generateIR(t, `
		type GSpan[T] {
			int x;
			T tag;
			[:](int? low, int? high) int { return this.x; }
			[:]=(int? low, int? high, int v) { this.x = v; }
			bump(~this) { this[0:1] += 3; }
		}
		main() {
			g := GSpan[bool](x: 10, tag: true);
			g.bump();
		}
	`)
	assertContains(t, ir, `call i64 @"GSpan[bool].[:]"(`)
	assertContains(t, ir, "add i64")
	assertContains(t, ir, `call void @"GSpan[bool].[:]="(`)
}
