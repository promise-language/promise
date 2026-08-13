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

// T1466: A fixed-array LITERAL passed directly as a call argument to a plain
// (borrow) array-by-value param is copied as an [N x T] aggregate and borrowed
// by the callee — nothing frees its heap-allocating elements. genCallArgs must
// register it as an element-wise-drop statement temp (trackArrayTemp) so the
// caller frees the elements after the call. This drives trackArrayTemp from the
// argument-position path (T1181 only drove it from the call-result path).
func TestT1466ArrayLiteralArgBorrowParamDropsElements(t *testing.T) {
	ir := generateIR(t, `
		take(string[2] xs) int => xs[0].len;
		main() { take(["a" + "b", "c" + "d"]); }
	`)
	// Caller drops the array-literal temp element-wise at statement end.
	assertContains(t, ir, "arrtmp.drop")
	assertContains(t, ir, "arrtmp.skip")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T1466: A non-droppable element type (int[N]) has no heap elements, so the
// array-literal arg must NOT register any element-wise temp drop — the
// variantFieldNeedsDrop gate excludes it.
func TestT1466IntArrayLiteralArgNoDrop(t *testing.T) {
	ir := generateIR(t, `
		take(int[2] xs) int => xs[0];
		main() { take([1 + 2, 3 + 4]); }
	`)
	assertNotContains(t, ir, "arrtmp.drop")
}

// T1466: An owned fixed-array VARIABLE passed to a borrow param is a borrow at
// the call site (IdentExpr → tupleArgIsCallerOwnedTemp false): it drops via its
// own bindingDropArray (arrdrop.exec), NOT a caller statement temp — so no
// arrtmp.drop is registered for the arg (guards against a double-free).
func TestT1466OwnedArrayVariableArgNoStmtTemp(t *testing.T) {
	ir := generateIR(t, `
		take(string[2] xs) int => xs[0].len;
		main() { string[2] v = ["a" + "b", "c" + "d"]; take(v); }
	`)
	assertContains(t, ir, "arrdrop.exec")
	assertNotContains(t, ir, "arrtmp.drop")
}

// T1466: A generator borrows its array param but reads it LAZILY (the frame
// outlives the call statement), so a statement-end drop is too early (UAF). The
// array-literal arg must instead get a scope-level owned drop (a synthetic
// `_genarrarg` local) — never an arrtmp statement temp.
func TestT1466GeneratorArrayLiteralArgScopeDrop(t *testing.T) {
	ir := generateIR(t, `
		lens(string[2] xs) stream[int] { yield xs[0].len; yield xs[1].len; }
		main() { for n in lens(["a" + "b", "c" + "d"]) {} }
	`)
	// Scope-level owned drop (bindingDropArray), not a statement temp.
	assertContains(t, ir, "_genarrarg")
	assertContains(t, ir, "call void @promise_string_drop")
	assertNotContains(t, ir, "arrtmp.drop")
}

// T1466: The same generator fed a CALL-RESULT array (already registered as a
// statement temp by the T1181 CallExpr path) still routes through the generator
// branch, which emits the scope-level `_genarrarg` drop AND claims the pre-
// existing statement temp so its too-early drop is neutralized (correctness is
// runtime-verified in tests/arrays/array_arg_drop_test.pr — no UAF, no double-
// free). Here we assert the scope-level drop is present for the call-result arg.
func TestT1466GeneratorCallResultArrayArgScopeDrop(t *testing.T) {
	ir := generateIR(t, `
		mk() string[2] => ["a" + "b", "c" + "d"];
		lens(string[2] xs) stream[int] { yield xs[0].len; yield xs[1].len; }
		main() { for n in lens(mk()) {} }
	`)
	assertContains(t, ir, "_genarrarg")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T1466: A HEAP-USER element type (not string, not vector) drives the
// variantFieldNeedsDrop heap-user branch of the arg-position registration and
// the emitVariantFieldDrop heap-user cleanup path — the caller frees each
// element via its drop function + pal_free inside the arrtmp.drop block. This
// covers the branch the string/int Go tests don't reach.
func TestT1466HeapUserArrayLiteralArgDropsElements(t *testing.T) {
	ir := generateIR(t, `
		type Boxed { int v; drop(~this) {} }
		take(Boxed[2] xs) int => xs[0].v;
		main() { take([Boxed(v: 1), Boxed(v: 2)]); }
	`)
	// Caller drops each heap-user element in the array-temp drop block.
	assertContains(t, ir, "arrtmp.drop")
	assertContains(t, ir, "call void @Boxed.drop")
	assertContains(t, ir, "call void @pal_free")
}
