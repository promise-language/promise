package ownership

import "testing"

// T1214: the LAUNDER-inside-body form of T1213 — a generic function/method that
// aliases a borrowed (non-`move`) param into an owned local
// (`identity[T](T x) T { T y = x; ... }`). `T y = x` is already rejected outright
// for a concrete handle ("cannot move borrowed parameter"), but the generic body
// is checked once with `T` unbound so no inline reject fires. At each concrete
// instantiation with an Optional-wrapped single-owner handle (`Mutex[int]?`,
// `task[int]?`) the owned alias double-frees the caller's live handle at its
// scope-exit drop (or when consumed) — INDEPENDENT of whether it is returned.
// The fix records a deferred requirement at the launder BINDING site (not the
// return), validated per concrete instantiation via the shared T1213
// GenericCallEdges machinery. Bare-handle launders (codegen return-alias clears
// the source flag) and `move`-param launders (ownership transfers) stay valid.

const t1214Launder = `identity[T](T x) T { T y = x; return y; }
`

// --- Rejected: Optional-wrapped single-owner handle instantiations ---

func TestT1214_LaunderReturnOptionalMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, t1214Launder+`
		test() {
			Mutex[int]? m = Mutex[int](5);
			Mutex[int]? m2 = identity(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
	expectOwnerError(t, errs, "aliased into an owned local")
}

func TestT1214_LaunderReturnOptionalTaskParamRejected(t *testing.T) {
	errs := ownerErrs(t, t1214Launder+`
		worker() int { return 1; }
		test() {
			task[int]? m = go worker();
			task[int]? m2 = identity(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Task[int]?")
}

func TestT1214_LaunderReturnOptionalMutexGuardParamRejected(t *testing.T) {
	errs := ownerErrs(t, t1214Launder+`
		test() {
			Mutex[int] mm = Mutex[int](5);
			MutexGuard[int]? g = mm.lock();
			MutexGuard[int]? g2 = identity(g);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with MutexGuard[int]?")
}

func TestT1214_LaunderReturnDoubleOptionalMutexParamRejected(t *testing.T) {
	// Deeper nesting `Mutex[int]??` — still an Optional-wrapped handle, rejected.
	errs := ownerErrs(t, t1214Launder+`
		test() {
			Mutex[int]? inner = Mutex[int](5);
			Mutex[int]?? m = inner;
			Mutex[int]?? m2 = identity(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]??")
}

func TestT1214_LaunderInferredReturnOptionalMutexParamRejected(t *testing.T) {
	// Inferred-launder shape (`y := x`) records at checkInferredVarDecl.
	errs := ownerErrs(t, `
		identity[T](T x) T { y := x; return y; }
		test() {
			Mutex[int]? m = Mutex[int](5);
			Mutex[int]? m2 = identity(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
}

func TestT1214_LaunderChainedReturnOptionalMutexParamRejected(t *testing.T) {
	// Chained launder (`T y = x; T z = y; return z;`): the first param→local hop
	// records the req, so the instantiation is rejected without tracking the chain.
	errs := ownerErrs(t, `
		identity[T](T x) T { T y = x; T z = y; return z; }
		test() {
			Mutex[int]? m = Mutex[int](5);
			Mutex[int]? m2 = identity(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
}

func TestT1214_LaunderDiscardOptionalMutexParamRejected(t *testing.T) {
	// The alias double-frees at scope exit even when NOT returned — a launder that
	// returns a plain int is still unsound and must be rejected.
	errs := ownerErrs(t, `
		identity[T](T x) int { T y = x; return 1; }
		test() {
			Mutex[int]? m = Mutex[int](5);
			int r = identity(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
	expectOwnerError(t, errs, "aliased into an owned local")
}

func TestT1214_LaunderVoidOptionalMutexParamRejected(t *testing.T) {
	// No return at all — the owned alias still double-frees at scope exit.
	errs := ownerErrs(t, `
		consume[T](T x) { T y = x; }
		test() {
			Mutex[int]? m = Mutex[int](5);
			consume(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
}

func TestT1214_LaunderAssignBorrowedParamRejectedInline(t *testing.T) {
	// The assignment launder (`y = x` into an existing owned local) is NOT a
	// var-decl and does not need the deferred check: the assignment path consumes
	// its RHS via tryMoveConsume, which already rejects a borrowed param inline
	// ("cannot move borrowed parameter") even for a generic TypeParam. This guards
	// that the var-decl recorder is not the sole line of defense for the assign form.
	errs := ownerErrs(t, `
		pick[T](T x, T move w) T { T y = w; y = x; return y; }
		test() {
			Mutex[int]? a = Mutex[int](5);
			Mutex[int]? b = Mutex[int](9);
			Mutex[int]? r = pick(a, move b);
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'x'")
}

func TestT1214_LaunderGenericMethodReturnOptionalMutexParamRejected(t *testing.T) {
	// Generic *method* launder form: forward[T] launders its borrowed param through
	// a local; the concrete w.forward(m) instantiates with Mutex[int]?.
	errs := ownerErrs(t, `
		type Wrap { forward[T](T x) T { T y = x; return y; } }
		test() {
			Wrap w = Wrap();
			Mutex[int]? m = Mutex[int](5);
			Mutex[int]? m2 = w.forward(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
}

func TestT1214_LaunderReturnOptionalMutexTransitiveRejected(t *testing.T) {
	// Transitive chain whose inner body launders: mid[U] launders y before
	// returning; outer[V] forwards it; the concrete outer(m) with Mutex[int]? is
	// caught at the outer call site after the requirement forwards from mid.
	errs := ownerErrs(t, `
		mid[U](U x) U { U y = x; return y; }
		outer[V](V y) V { return mid(y); }
		test() {
			Mutex[int]? m = Mutex[int](5);
			Mutex[int]? m2 = outer(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
}

// --- Accepted: no over-rejection ---

func TestT1214_LaunderReturnBareMutexParamOK(t *testing.T) {
	// Bare-handle launder stays valid: codegen's return-alias check clears the
	// source drop flag (optionalWrappedSingleOwnerHandle returns "" at depth 0).
	ownerOK(t, t1214Launder+`
		test() {
			Mutex[int] m = Mutex[int](5);
			Mutex[int] m2 = identity(m);
		}
	`)
}

func TestT1214_LaunderDiscardBareMutexParamOK(t *testing.T) {
	// Bare-handle launder-and-discard is safe at runtime (verified) — must not be
	// over-rejected.
	ownerOK(t, `
		identity[T](T x) int { T y = x; return 1; }
		test() {
			Mutex[int] m = Mutex[int](5);
			int r = identity(m);
		}
	`)
}

func TestT1214_LaunderReturnMoveOptionalMutexParamOK(t *testing.T) {
	// `move` param launder: `x` is Owned (consuming), not Borrowed, so
	// recordLaunderedHandleReq never records — the single handle's ownership
	// transfers to the result.
	ownerOK(t, `
		identity[T](T move x) T { T y = x; return y; }
		test() {
			Mutex[int]? m = Mutex[int](5);
			Mutex[int]? m2 = identity(move m);
		}
	`)
}

func TestT1214_LaunderReturnIntParamOK(t *testing.T) {
	// Copy instantiation — freely returnable.
	ownerOK(t, t1214Launder+`
		test() {
			int m = 5;
			int m2 = identity(m);
		}
	`)
}

func TestT1214_LaunderReturnStringParamOK(t *testing.T) {
	// string return implicitly dups — freely returnable.
	ownerOK(t, t1214Launder+`
		test() {
			string s = "hi";
			string s2 = identity(s);
		}
	`)
}

func TestT1214_LaunderReturnVectorParamOK(t *testing.T) {
	// Vector launder — freely returnable, no single-owner handle.
	ownerOK(t, t1214Launder+`
		test() {
			int[] v = int[]();
			int[] v2 = identity(v);
		}
	`)
}

func TestT1214_LaunderReturnOptionalUserTypeParamOK(t *testing.T) {
	// Over-rejection guard: an Optional-wrapped plain heap user type (Foo?) is
	// freely returnable and must NOT be rejected via the launder path.
	ownerOK(t, `
		type Foo { int x; }
		identity[T](T x) T { T y = x; return y; }
		test() {
			Foo? f = Foo(x: 5);
			Foo? f2 = identity(f);
		}
	`)
}
