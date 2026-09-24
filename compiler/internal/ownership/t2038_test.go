package ownership

import "testing"

// T2038: every value a generator yields is owned by the consumer, so `yield*`
// over a Vector/Array must hand out either an element moved out of a container it
// consumes (a local or a temporary) or a copy — and a closure cannot be copied.
// A delegation over closures held anywhere else is rejected, like the spelled-out
// `for f in this.fns { yield f; }` already is (T0978).

const t2038ClosureBag = `
	type Bag {
		(() -> int)[] fns;
		string[] names;
		(() -> int)[2] pair;
`

func TestT2038_YieldStarClosuresFromBorrowedFieldRejected(t *testing.T) {
	errs := ownerErrs(t, t2038ClosureBag+`
		all(this) stream[() -> int] { yield* this.fns; }
	}`)
	expectOwnerError(t, errs, "cannot `yield*` closures")
}

func TestT2038_YieldStarClosureArrayFromBorrowedFieldRejected(t *testing.T) {
	errs := ownerErrs(t, t2038ClosureBag+`
		both(this) stream[() -> int] { yield* this.pair; }
	}`)
	expectOwnerError(t, errs, "cannot `yield*` closures")
}

func TestT2038_YieldStarClosuresFromOwnedLocalAccepted(t *testing.T) {
	ownerOK(t, `
		make() (() -> int)[] {
			(() -> int)[] v = [|| -> 1];
			return v;
		}
		from_local() stream[() -> int] {
			(() -> int)[] v = make();
			yield* v;
		}
		from_temp() stream[() -> int] { yield* make(); }
		main() {}
	`)
}

// Copyable elements are copied out of a container the generator does not own, so
// the borrowed-field delegation stays legal for them.
func TestT2038_YieldStarCopyableFromBorrowedFieldAccepted(t *testing.T) {
	ownerOK(t, t2038ClosureBag+`
		all(this) stream[string] { yield* this.names; }
	}
	main() {}`)
}

// Parentheses do not change who owns the collection.
func TestT2038_YieldStarClosuresFromParenthesizedLocalAccepted(t *testing.T) {
	ownerOK(t, `
		from_local() stream[() -> int] {
			(() -> int)[] v = [|| -> 1];
			yield* (v);
		}
		main() {}
	`)
}
