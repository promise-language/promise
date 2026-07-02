package codegen

import "testing"

// T1181: A call returning a fixed-size array of a heap-allocating element type
// (string[N], Vector[T][N]) used INLINE (never bound) must have its elements
// dropped at statement end. The inline path lacked a "track returned fixed-array
// temp → element-wise drop" cleanup, so the heap elements leaked. This test
// asserts the discarded inline call emits the arrtmp element-walk drop.
func TestFixedArrayReturnTempDiscardDropsElements(t *testing.T) {
	ir := generateIR(t, `
		mk() string[2] { return ["a" + "1", "b" + "2"]; }
		main() { mk(); }
	`)
	// The array temp cleanup emits arrtmp.drop/arrtmp.skip blocks and drops each
	// string element via promise_string_drop.
	assertContains(t, ir, "arrtmp.drop")
	assertContains(t, ir, "arrtmp.skip")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T1181: When a failable callee raises while an inline array-returning call
// result is still live (passed to it by borrow), the caller's error-path cleanup
// must element-wise drop the temp — the arrtmp drop is emitted on the raise/error
// unwind path (emitStmtTempCleanupForErrorPath), not only at statement end.
func TestFixedArrayReturnTempErrorPathDropsElements(t *testing.T) {
	ir := generateIR(t, `
		mk() string[2] { return ["a" + "1", "b" + "2"]; }
		take_fail!(string[2] a) bool { raise error(message: "boom"); }
		main() { bool ok = take_fail(mk()) ? { true }; }
	`)
	// Element-wise array-temp drop present, and it must appear on the failure
	// (error-cleanup) unwind, so promise_string_drop is emitted for the elements.
	assertContains(t, ir, "arrtmp.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T1181: A fixed-array-returning call used inline inside a *generic* body is
// tracked under an active typeSubst; the element type is substituted before the
// per-element drop is emitted, so the monomorphized body still frees its elements.
func TestFixedArrayReturnTempGenericBodyDropsElements(t *testing.T) {
	ir := generateIR(t, `
		pair[T](T move a, T move b) T[2] { return [a, b]; }
		sink[T](T move a, T move b) { pair[T](move a, move b); }
		main() { sink[string]("a" + "1", "b" + "2"); }
	`)
	assertContains(t, ir, "arrtmp.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T1181: When the array-returning call result IS bound to a variable, the
// stmt-temp is claimed (drop flag cleared) so ownership transfers to the
// variable's bindingDropArray — the element drop happens once, at scope exit
// (arrdrop.exec), not twice.
func TestFixedArrayReturnTempBoundClaimsTemp(t *testing.T) {
	ir := generateIR(t, `
		mk() string[2] { return ["a" + "1", "b" + "2"]; }
		main() { string[2] r = mk(); }
	`)
	// Bound path drops through the scope-exit binding, not the stmt-temp.
	assertContains(t, ir, "arrdrop.exec")
	assertContains(t, ir, "call void @promise_string_drop")
}
