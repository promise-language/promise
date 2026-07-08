package ownership

import "testing"

// T1213: a generic function/method that directly returns a borrowed (non-`move`)
// param, instantiated with an Optional-wrapped single-owner handle
// (`Mutex[int]?`, `task[int]?`, `MutexGuard[int]?`), used to segfault. The param
// is a TypeParam during generic checking, so the T1102/T1138 concrete rejects
// cannot see the handle; at each concrete instantiation codegen routes the
// Optional-wrapped handle through dupOptionalVectorElem (no clone for
// single-owner handles) → aliases the one handle and double-frees. The fix
// defers the verdict to each concrete call site via GenericCallEdges. The
// BARE-handle instantiation stays valid (codegen's return-alias clears the
// source flag — t1102_generic_identity_mutex), as does the `move`-param form.

const t1213Identity = `identity[T](T x) T { return x; }
`

// --- Rejected: Optional-wrapped single-owner handle instantiations ---

func TestT1213_GenericReturnOptionalMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, t1213Identity+`
		test() {
			Mutex[int]? m = Mutex[int](5);
			Mutex[int]? m2 = identity(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
	expectOwnerError(t, errs, "returned as owned")
}

func TestT1213_GenericReturnOptionalTaskParamRejected(t *testing.T) {
	errs := ownerErrs(t, t1213Identity+`
		worker() int { return 1; }
		test() {
			task[int]? m = go worker();
			task[int]? m2 = identity(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Task[int]?")
}

func TestT1213_GenericReturnOptionalMutexGuardParamRejected(t *testing.T) {
	errs := ownerErrs(t, t1213Identity+`
		test() {
			Mutex[int] mm = Mutex[int](5);
			MutexGuard[int]? g = mm.lock();
			MutexGuard[int]? g2 = identity(g);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with MutexGuard[int]?")
}

func TestT1213_GenericReturnDoubleOptionalMutexParamRejected(t *testing.T) {
	// Deeper nesting `Mutex[int]??` — still an Optional-wrapped handle, rejected.
	errs := ownerErrs(t, t1213Identity+`
		test() {
			Mutex[int]? inner = Mutex[int](5);
			Mutex[int]?? m = inner;
			Mutex[int]?? m2 = identity(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]??")
}

func TestT1213_GenericMethodReturnOptionalMutexParamRejected(t *testing.T) {
	// Generic *method* form: forward[T] returns its borrowed param; the concrete
	// w.forward(m) instantiates with Mutex[int]? and triggers validation.
	errs := ownerErrs(t, `
		type Wrap { forward[T](T x) T { return x; } }
		test() {
			Wrap w = Wrap();
			Mutex[int]? m = Mutex[int](5);
			Mutex[int]? m2 = w.forward(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
}

func TestT1213_GenericReturnOptionalMutexTransitiveRejected(t *testing.T) {
	// Transitive chain: outer[V] returns mid[V](y); the concrete outer(m)
	// instantiation with Mutex[int]? is caught at the outer call site after the
	// requirement forwards from mid onto outer.
	errs := ownerErrs(t, `
		mid[U](U x) U { return x; }
		outer[V](V y) V { return mid(y); }
		test() {
			Mutex[int]? m = Mutex[int](5);
			Mutex[int]? m2 = outer(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
}

func TestT1213_GenericMethodReturnOptionalMutexTransitiveRejected(t *testing.T) {
	// Transitive chain whose caller is a generic *method*: Wrap.outer[V] returns
	// mid[V](y). mid's return-handle req substitutes to V (still generic), so it
	// forwards onto the method outer via addReturnHandleReq's method branch. The
	// concrete w.outer(m) with Mutex[int]? then triggers the rejection.
	errs := ownerErrs(t, `
		mid[U](U x) U { return x; }
		type Wrap { outer[V](V y) V { return mid(y); } }
		test() {
			Wrap w = Wrap();
			Mutex[int]? m = Mutex[int](5);
			Mutex[int]? m2 = w.outer(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
}

func TestT1213_GenericFuncMultipleReturnsOptionalMutexRejected(t *testing.T) {
	// A generic body with TWO returns of borrowed TypeParam params records a
	// return-handle req per return; recording the second walks the func's
	// existing-req dedup loop. Instantiated with Mutex[int]?, both are rejected.
	errs := ownerErrs(t, `
		pick[T](T a, T b, bool c) T {
			if c { return a; }
			return b;
		}
		test() {
			Mutex[int]? m1 = Mutex[int](5);
			Mutex[int]? m2 = Mutex[int](6);
			Mutex[int]? r = pick(m1, m2, true);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
}

func TestT1213_GenericMethodMultipleReturnsOptionalMutexRejected(t *testing.T) {
	// Method form of the multi-return case — exercises the method-path dedup
	// loop in recordReturnHandleReq.
	errs := ownerErrs(t, `
		type Chooser { pick[T](T a, T b, bool c) T { if c { return a; } return b; } }
		test() {
			Chooser ch = Chooser();
			Mutex[int]? m1 = Mutex[int](5);
			Mutex[int]? m2 = Mutex[int](6);
			Mutex[int]? r = ch.pick(m1, m2, true);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
}

// --- Accepted: no over-rejection ---

func TestT1213_GenericReturnBareMutexParamOK(t *testing.T) {
	// Bare-handle instantiation stays valid: codegen's return-alias check clears
	// the source drop flag, so only one drop happens. (t1102_generic_identity_mutex)
	ownerOK(t, t1213Identity+`
		test() {
			Mutex[int] m = Mutex[int](5);
			Mutex[int] m2 = identity(m);
		}
	`)
}

func TestT1213_GenericReturnMoveOptionalMutexParamOK(t *testing.T) {
	// `move` param transfers the single handle's ownership to the result.
	ownerOK(t, `
		identity[T](T move x) T { return x; }
		test() {
			Mutex[int]? m = Mutex[int](5);
			Mutex[int]? m2 = identity(move m);
		}
	`)
}

func TestT1213_GenericReturnIntParamOK(t *testing.T) {
	// Copy instantiation — freely returnable, no double-free.
	ownerOK(t, t1213Identity+`
		test() {
			int m = 5;
			int m2 = identity(m);
		}
	`)
}

func TestT1213_GenericReturnStringParamOK(t *testing.T) {
	// string return implicitly dups — freely returnable.
	ownerOK(t, t1213Identity+`
		test() {
			string s = "hi";
			string s2 = identity(s);
		}
	`)
}

func TestT1213_GenericReturnVectorParamOK(t *testing.T) {
	// Vector return dups its storage — freely returnable.
	ownerOK(t, t1213Identity+`
		test() {
			int[] v = int[]();
			int[] v2 = identity(v);
		}
	`)
}

func TestT1213_GenericReturnOptionalUserTypeParamOK(t *testing.T) {
	// Over-rejection guard: the reject is specific to Optional-wrapped SINGLE-OWNER
	// handles. An Optional-wrapped plain heap user type (Foo?) is freely returnable
	// and must NOT be rejected, even though it is also Optional-wrapped.
	ownerOK(t, `
		type Foo { int x; }
		identity[T](T x) T { return x; }
		test() {
			Foo? f = Foo(x: 5);
			Foo? f2 = identity(f);
		}
	`)
}
