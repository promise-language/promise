package codegen

import "testing"

// T1065: Nested push/pop/remove and nested index-assign on a container whose
// user-defined `[]` operator returns a Vector BY VALUE must not panic in
// codegen. The returned Vector is an rvalue temporary, not an addressable
// element slot, so storeBackSlicePtr must skip the store-back (matching how a
// Vector returned from any other method behaves). Before the fix this panicked
// with "storeBackSlicePtr index target is not array/vector: VecBox".
func TestT1065UserIndexVectorPushNoPanic(t *testing.T) {
	ir := generateIR(t, `
		type VecBox {
		  Vector[int][] _data;
		  new(~this) { this._data = [Vector[int]()]; }
		  [](int i) Vector[int] => this._data[i];
		}
		caller() { b := VecBox(); b[0].push(99); }
		main() {}
	`)
	// The push still happens (onto the discarded temporary), so the runtime
	// push call must be emitted; the regression guard is that generateIR did
	// not panic.
	assertContains(t, ir, "@promise_vector_push")
}

func TestT1065UserIndexVectorPopNoPanic(t *testing.T) {
	ir := generateIR(t, `
		type VecBox {
		  Vector[int][] _data;
		  new(~this) { this._data = [Vector[int]()]; }
		  [](int i) Vector[int] => this._data[i];
		}
		caller() { b := VecBox(); b[0].pop(); }
		main() {}
	`)
	assertContains(t, ir, "@promise_vector_pop")
}

func TestT1065UserIndexNestedAssignNoPanic(t *testing.T) {
	// g[0][1] = 99 through a user-`[]` container: the outer index yields an
	// rvalue Vector temporary, so the nested index-assign store-back must be
	// skipped rather than panicking. Reaches storeBackSlicePtr via
	// genVectorIndexAssign (stmt.go).
	generateIR(t, `
		type VecBox {
		  Vector[int][] _data;
		  new(~this) { this._data = [[1, 2, 3]]; }
		  [](int i) Vector[int] => this._data[i];
		}
		caller() { g := VecBox(); g[0][1] = 99; }
		main() {}
	`)
}

// The remaining three storeBackSlicePtr callers in stmt.go — inc/dec, compound
// index-assign, and slice-assign — also reach the fixed guard when the outer
// index target is a user-`[]` container (they each COW-then-store-back through
// storeBackSlicePtr). All three panicked identically before the fix; these pin
// each distinct caller so a regression in any one is caught at IR level.

func TestT1065UserIndexNestedIncDecNoPanic(t *testing.T) {
	// g[0][1]++ — reaches storeBackSlicePtr via the inc/dec IndexExpr case.
	generateIR(t, `
		type VecBox {
		  Vector[int][] _data;
		  new(~this) { this._data = [[1, 2, 3]]; }
		  [](int i) Vector[int] => this._data[i];
		}
		caller() { g := VecBox(); g[0][1]++; }
		main() {}
	`)
}

func TestT1065UserIndexNestedCompoundAssignNoPanic(t *testing.T) {
	// g[0][1] += 5 — reaches storeBackSlicePtr via the compound-assign case.
	generateIR(t, `
		type VecBox {
		  Vector[int][] _data;
		  new(~this) { this._data = [[1, 2, 3]]; }
		  [](int i) Vector[int] => this._data[i];
		}
		caller() { g := VecBox(); g[0][1] += 5; }
		main() {}
	`)
}

func TestT1065UserIndexNestedSliceAssignNoPanic(t *testing.T) {
	// g[0][0:2] = [7, 8] — reaches storeBackSlicePtr via the slice-assign case.
	generateIR(t, `
		type VecBox {
		  Vector[int][] _data;
		  new(~this) { this._data = [[1, 2, 3]]; }
		  [](int i) Vector[int] => this._data[i];
		}
		caller() { g := VecBox(); g[0][0:2] = [7, 8]; }
		main() {}
	`)
}
