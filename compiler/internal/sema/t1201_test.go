package sema

import "testing"

// T1201: residual sibling of T0666. When a `clone generic is instantiated with
// an UNBOUND TypeParam inside another generic body, validateCloneInstance's
// direct-site check defers (the substituted field still contains a TypeParam).
// If the outer generic is then called with a non-cloneable concrete arg, no
// site re-runs the T0666 predicate → codegen's AutoClone path bit-copies the
// field and double-frees the shared heap payload at drop. The deferred
// requirement now rides the T0616 recordCloneReq/propagateCloneReqs machinery
// and is validated at the concrete call edge.

func TestT1201NestedCloneGenericStructNonCloneable(t *testing.T) {
	// Box[T] instantiated inside outer[T]'s body; main calls outer[Heapy].
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		type Box[T] `+"`clone"+` { T val; }
		outer[T](T move v) {
			Box[T] b = Box[T](val: move v);
			Box[T] c = b.clone();
		}
		main() {
			outer[Heapy](Heapy.Word("x"));
		}
	`)
	expectError(t, errs, "not cloneable")
}

func TestT1201NestedCloneGenericEnumNonCloneable(t *testing.T) {
	// Enum-branch analogue: Wrap[T] instantiated inside outer[T].
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		enum Wrap[T] `+"`clone"+` { Plain(T v), Nothing }
		outer[T](T move v) {
			Wrap[T] w = Wrap[T].Plain(move v);
			Wrap[T] c = w.clone();
		}
		main() {
			outer[Heapy](Heapy.Word("x"));
		}
	`)
	expectError(t, errs, "not cloneable")
}

func TestT1201NestedCloneGenericTransitiveChain(t *testing.T) {
	// inner[T] instantiates Box[T]; mid[U] calls inner[U]; main calls mid[Heapy].
	// Exercises the fixed-point propagation across two generic call edges.
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		type Box[T] `+"`clone"+` { T val; }
		inner[T](T move v) {
			Box[T] b = Box[T](val: move v);
			Box[T] c = b.clone();
		}
		mid[U](U move v) {
			inner[U](move v);
		}
		main() {
			mid[Heapy](Heapy.Word("x"));
		}
	`)
	expectError(t, errs, "not cloneable")
}

func TestT1201NestedCloneGenericMethodReceiver(t *testing.T) {
	// A method body on a generic owner instantiates Box[T]; exercises
	// MethodCloneReqs + ownerSubstForMethodCall at the concrete method call.
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		type Box[T] `+"`clone"+` { T val; }
		type Holder[T] {
			int n;
			make(this, T move v) {
				Box[T] b = Box[T](val: move v);
				Box[T] c = b.clone();
			}
		}
		main() {
			Holder[Heapy] h = Holder[Heapy](n: 0);
			h.make(Heapy.Word("x"));
		}
	`)
	expectError(t, errs, "not cloneable")
}

// --- Negatives: nested cloneable args must NOT be rejected ---

func TestT1201NestedCloneGenericCopyArgOK(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Box[T] `+"`clone"+` { T val; }
		outer[T](T move v) {
			Box[T] b = Box[T](val: move v);
			Box[T] c = b.clone();
		}
		main() {
			outer[int](5);
		}
	`))
}

func TestT1201NestedCloneGenericUserCloneArgOK(t *testing.T) {
	// The concrete arg is itself a `clone user type owning a heap string — the
	// nested body's clone() deep-clones through it, so no error.
	expectNoErrors(t, checkErrs(t, `
		type Owned `+"`clone"+` { string s; }
		type Box[T] `+"`clone"+` { T val; }
		outer[T](T move v) {
			Box[T] b = Box[T](val: move v);
			Box[T] c = b.clone();
		}
		main() {
			outer[Owned](Owned(s: "x"));
		}
	`))
}

func TestT1201NestedCloneGenericStringArgOK(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Box[T] `+"`clone"+` { T val; }
		outer[T](T move v) {
			Box[T] b = Box[T](val: move v);
			Box[T] c = b.clone();
		}
		main() {
			outer[string]("x");
		}
	`))
}

// --- Mixed-field shapes: a concrete field alongside the deferred TypeParam
// field must be skipped by the nested-body scan (clone.go:180 / :209), while the
// generic field is still deferred and re-validated at the concrete call edge. ---

func TestT1201NestedCloneGenericMixedFieldStructNonCloneable(t *testing.T) {
	// Box[T] carries a concrete `int n` (skipped) plus the deferred `T val`.
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		type Box[T] `+"`clone"+` { int n; T val; }
		outer[T](T move v) {
			Box[T] b = Box[T](n: 1, val: move v);
			Box[T] c = b.clone();
		}
		main() {
			outer[Heapy](Heapy.Word("x"));
		}
	`)
	expectError(t, errs, "not cloneable")
}

func TestT1201NestedCloneGenericMixedFieldStructOK(t *testing.T) {
	// Same mixed-field shape but with a cloneable concrete arg — the concrete
	// `int n` field must not spuriously trip the check.
	expectNoErrors(t, checkErrs(t, `
		type Box[T] `+"`clone"+` { int n; T val; }
		outer[T](T move v) {
			Box[T] b = Box[T](n: 1, val: move v);
			Box[T] c = b.clone();
		}
		main() {
			outer[string]("x");
		}
	`))
}

func TestT1201NestedCloneGenericMixedFieldEnumNonCloneable(t *testing.T) {
	// Wrap[T] has a concrete-field variant Tag(int) (skipped) plus the deferred
	// Plain(T) variant.
	errs := checkErrs(t, `
		enum Heapy { Word(string s), Empty }
		enum Wrap[T] `+"`clone"+` { Plain(T v), Tag(int n), Nothing }
		outer[T](T move v) {
			Wrap[T] w = Wrap[T].Plain(move v);
			Wrap[T] c = w.clone();
		}
		main() {
			outer[Heapy](Heapy.Word("x"));
		}
	`)
	expectError(t, errs, "not cloneable")
}
