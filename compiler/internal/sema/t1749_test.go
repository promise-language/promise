package sema

import "testing"

// T1749: `factory / `global / `mono are receiver-less placements. An operator is
// dispatched from `a OP b` (or `-a`, or `lhs[i] = rhs`) where the LEFT operand IS
// the receiver, and there is no call-site syntax that omits it — so a receiver-less
// operator silently discards the left operand. It is also undispatchable: a
// receiver-less member holds no vtable slot, so once the type gains a subtype the
// virtual-dispatch path has nothing to dispatch through.
//
// Same class of hidden effect as the `move`/mut-ref operand rejections in T0916 /
// T1070 / T1369, so it is rejected the same way — with a diagnostic, not a panic.

func TestT1749_GlobalOperatorRejected(t *testing.T) {
	expectError(t, checkErrs(t, `
		type S {
			int x;
			+(S other) S `+"`global"+` { return S(x: other.x); }
		}
	`), "operator method S.+ cannot be `global")
}

func TestT1749_MonoOperatorRejected(t *testing.T) {
	expectError(t, checkErrs(t, `
		type S[T] {
			T v;
			==(S[T] other) bool `+"`mono"+` { return true; }
		}
	`), "operator method S.== cannot be `mono")
}

func TestT1749_FactoryOperatorRejected(t *testing.T) {
	expectError(t, checkErrs(t, `
		type S {
			int x;
			*(S other) S `+"`factory"+` { return S(x: other.x); }
		}
	`), "operator method S.* cannot be `factory")
}

func TestT1749_FactorySetterOperatorRejected(t *testing.T) {
	// The index/slice setters are dispatched from `lhs[i] = rhs`, which also
	// supplies the receiver, so they are covered too.
	expectError(t, checkErrs(t, `
		type S {
			int x;
			[]=(int i, int v) `+"`factory"+` { }
		}
	`), "operator method S.[]= cannot be `factory")
}

func TestT1749_FactoryOperatorOnEnumRejected(t *testing.T) {
	expectError(t, checkErrs(t, `
		enum E {
			A,
			B,
			+(E other) E `+"`factory"+` { return E.A; }
		}
	`), "operator method E.+ cannot be `factory")
}

func TestT1749_NamedReceiverlessMembersStillAllowed(t *testing.T) {
	// Only operators are barred — named `factory / `global / `mono members on a
	// type that has a subtype are exactly what T1749 makes work.
	expectNoErrors(t, checkErrs(t, `
		type Base {
			int x;
			make(int v) Self `+"`factory"+` { return Base(x: v); }
			tag() string `+"`global"+` { return "t"; }
			get label string `+"`global"+` => "l";
		}
		type Kid is Base { int y; }
	`))
}

func TestT1749_UnaryOperatorPlacementRejected(t *testing.T) {
	// The unary form has no parameters at all, so a receiver-less `-()` would have
	// nothing to negate — `-a` supplies its operand as the receiver.
	expectError(t, checkErrs(t, `
		type S {
			int x;
			-() S `+"`factory"+` { return S(x: 0); }
		}
	`), "operator method S.- cannot be `factory")
}

func TestT1749_IndexGetterOperatorRejected(t *testing.T) {
	// `a[i]` also supplies its receiver positionally, so the index GETTER is
	// barred alongside the []= setter form.
	expectError(t, checkErrs(t, `
		type S {
			int x;
			[](int i) int `+"`global"+` { return i; }
		}
	`), "operator method S.[] cannot be `global")
}

func TestT1749_SliceSetterOperatorRejected(t *testing.T) {
	expectError(t, checkErrs(t, `
		type S {
			int x;
			[:]=(int? low, int? high, int v) `+"`global"+` { }
		}
	`), "operator method S.[:]= cannot be `global")
}

func TestT1749_InstanceOperatorStillAllowed(t *testing.T) {
	// The rejection keys off the placement, not the operator name: an ordinary
	// instance operator on a type that has a subtype is untouched, and still
	// takes its vtable slot.
	expectNoErrors(t, checkErrs(t, `
		type Base {
			int x;
			+(Base other) Base { return Base(x: this.x + other.x); }
			-() Base { return Base(x: -this.x); }
			[](int i) int { return this.x; }
		}
		type Kid is Base { int y; }
	`))
}
