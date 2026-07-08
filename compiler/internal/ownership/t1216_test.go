package ownership

import "testing"

// T1216: the UNDERSCORE-DISCARD launder form of T1214 — a generic function that
// aliases a borrowed (non-`move`) param into a `_` discard binding
// (`consume[T](T x) { T _ = x; }` or `_ := x`). T1214 fixed the NAMED launder
// (`T y = x`) but its recorder call sites lived inside `if s.Name != "_"`, so a
// `_` discard slipped through. Codegen still materializes a droppable owned temp
// for the discard, so at each concrete Optional-wrapped single-owner handle
// instantiation the alias double-frees the caller's live handle at scope exit —
// exactly the T1214 crash. The fix moves recordLaunderedHandleReq out of the
// name guard at both var-decl sites so `_` bindings record too. Bare-handle and
// `move`-param discards stay valid (same safety split as T1214).

// --- Rejected: Optional-wrapped single-owner handle instantiations ---

func TestT1216_UnderscoreTypedLaunderOptionalMutexRejected(t *testing.T) {
	// Typed discard `T _ = x` over Mutex[int]? — segfaults at runtime pre-fix.
	errs := ownerErrs(t, `
		consume[T](T x) { T _ = x; }
		test() {
			Mutex[int]? m = Mutex[int](5);
			consume(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
	expectOwnerError(t, errs, "aliased into an owned local")
}

func TestT1216_UnderscoreInferredLaunderOptionalMutexRejected(t *testing.T) {
	// Inferred discard `_ := x` over Mutex[int]? — segfaults at runtime pre-fix.
	errs := ownerErrs(t, `
		consume[T](T x) { _ := x; }
		test() {
			Mutex[int]? m = Mutex[int](5);
			consume(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Mutex[int]?")
	expectOwnerError(t, errs, "aliased into an owned local")
}

func TestT1216_UnderscoreLaunderOptionalTaskRejected(t *testing.T) {
	// Parity over task[int]? — the other single-owner handle kind.
	errs := ownerErrs(t, `
		consume[T](T x) { T _ = x; }
		worker() int { return 1; }
		test() {
			task[int]? m = go worker();
			consume(m);
		}
	`)
	expectOwnerError(t, errs, "cannot instantiate generic with Task[int]?")
}

// --- Accepted: no over-rejection ---

func TestT1216_UnderscoreLaunderBareMutexOK(t *testing.T) {
	// Bare-handle discard `T _ = x` over Mutex[int] is safe (depth-0 bare, the
	// return-alias clear applies) — must not be over-rejected.
	ownerOK(t, `
		consume[T](T x) { T _ = x; }
		test() {
			Mutex[int] m = Mutex[int](5);
			consume(m);
		}
	`)
}

func TestT1216_UnderscoreLaunderMoveOptionalMutexOK(t *testing.T) {
	// `move`-param discard: `x` is Owned (consuming), not Borrowed, so
	// recordLaunderedHandleReq never records — ownership transfers into the temp.
	ownerOK(t, `
		consume[T](T move x) { T _ = x; }
		test() {
			Mutex[int]? m = Mutex[int](5);
			consume(move m);
		}
	`)
}
