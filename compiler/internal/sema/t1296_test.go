package sema

import "testing"

// T1296: Sema must allow assigning a plain value into an optional-valued map
// (`m[k] = v` on map[K, Vector[T]?]) — the assignment target type is the `[]=`
// setter's value parameter (V), not the presence-optional `[]` getter return
// (V?). Member access on a map-index optional place (`m[k]!.push()`) is
// declined with a read-modify-write diagnostic, replacing the opaque
// "cannot access member on type int[]?".

func TestT1296MapOptionalValueAssignOK(t *testing.T) {
	errs := checkErrs(t, `
		f() {
			map[int, int[]?] m = {:};
			m[0] = [1, 2];
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1296MapOptionalValueReadModifyWriteOK(t *testing.T) {
	errs := checkErrs(t, `
		f() {
			map[int, int[]?] m = {:};
			m[0] = [1, 2];
			int[] inner = m[0]!!;
			inner.push(7);
			m[0] = inner;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1296MapOptionalInPlaceMutateRejected(t *testing.T) {
	errs := checkErrs(t, `
		f() {
			map[int, int[]?] m = {:};
			m[0] = [1, 2];
			m[0]!.push(7);
		}
	`)
	expectError(t, errs, "cannot mutate a map value in place")
	// Must NOT surface the old opaque diagnostic.
	expectNoErrorContaining(t, errs, "cannot access member on type")
}

func TestT1296PlainMapAssignStillOK(t *testing.T) {
	errs := checkErrs(t, `
		f() {
			map[int, int] m = {:};
			m[0] = 5;
		}
	`)
	expectNoErrors(t, errs)
}

// Correctness tightening: `m[k] = someOptional` on a NON-optional map is now
// rejected — previously the loose target type (V?) accepted it.
func TestT1296PlainMapAssignOptionalRejected(t *testing.T) {
	errs := checkErrs(t, `
		f(int? opt) {
			map[int, int] m = {:};
			m[0] = opt;
		}
	`)
	expectError(t, errs, "cannot assign")
}

// A non-map optional member access keeps the general "unwrap it first" hint.
func TestT1296NonMapOptionalMemberAccessHint(t *testing.T) {
	errs := checkErrs(t, `
		f() {
			int[]? x = [1, 2];
			x.push(1);
		}
	`)
	expectError(t, errs, "unwrap it first with")
	expectNoErrorContaining(t, errs, "mutate a map value in place")
}

// A member access on an unwrapped optional whose inner is NOT a map index
// (here a double-optional local `x!`) takes the general hint, not the map
// message — covers isMapIndexOptionalUnwrap's non-IndexExpr guard.
func TestT1296DoubleOptionalLocalMemberAccessHint(t *testing.T) {
	errs := checkErrs(t, `
		f() {
			int[]? inner = [1, 2];
			int[]?? x = inner;
			x!.push(3);
		}
	`)
	expectError(t, errs, "unwrap it first with")
	expectNoErrorContaining(t, errs, "mutate a map value in place")
}

// The map-in-place rejection also fires when the map is reached through a
// mutable borrow (`~`) — the index target type is a MutRef that
// isMapIndexOptionalUnwrap must unwrap before AsMap succeeds.
func TestT1296MapThroughMutBorrowInPlaceRejected(t *testing.T) {
	errs := checkErrs(t, `
		mutate(map[int, int[]?]~ m) {
			m[0]!.push(3);
		}
	`)
	expectError(t, errs, "cannot mutate a map value in place")
	expectNoErrorContaining(t, errs, "cannot access member on type")
}
