package codegen

import (
	"strings"
	"testing"
)

// T0655: a single-owner Mutex *temp* receiver of .lock() (constructor or
// plain-method return) must be promoted to a scope binding so it outlives the
// MutexGuard that borrows it. Pre-fix, the Mutex temp was dropped at statement
// end (via cleanupStmtTemps, a tmp.exec block) BEFORE the scope-lived
// MutexGuard, so MutexGuard.drop unlocked/dereferenced freed Mutex memory →
// UAF/SEGV. The fix (promoteHandleTempToScopeBinding) registers the Mutex temp
// as a bindingDropString scope binding, ordered before the guard's var-decl
// scope binding, so LIFO scope cleanup drops the guard (unlock) first and the
// Mutex (free) afterwards — mirroring the already-correct bound-receiver path.

// firstIdx / lastIdx: small helpers for ordering assertions within a function.
func t0655First(s, sub string) int { return strings.Index(s, sub) }
func t0655Last(s, sub string) int  { return strings.LastIndex(s, sub) }

const t0655Src = `
	mk_mtx() Mutex[int] { return Mutex[int](5); }
	take_mtx_i(Mutex[int] m) {}
	use_temp_ctor()  { use g := Mutex[int](7).lock(); }
	use_temp_meth()  { use g := mk_mtx().lock(); }
	use_temp_nouse() { g := Mutex[int](7).lock(); }
	use_bound()      { m := Mutex[int](7); use g := m.lock(); }
	use_consume()    { take_mtx_i(Mutex[int](1)); }
	main() { }
`

// The receiver-temp .lock() forms must promote the Mutex temp to a scope
// binding (emitted via emitStringDropCall → strdrop.exec block calling
// @"Mutex[int].drop") and the scope-exit Mutex free must come AFTER the
// MutexGuard.drop, not before it.
func TestT0655MutexTempReceiverPromotedToScopeBinding(t *testing.T) {
	ir := generateIR(t, t0655Src)

	for _, fnName := range []string{
		"__user.use_temp_ctor",
		"__user.use_temp_meth",
		"__user.use_temp_nouse",
	} {
		fn := extractFunction(ir, fnName)
		if fn == "" {
			t.Fatalf("%s: function not found in IR", fnName)
		}
		// Promotion: an int Mutex with no string/vector locals only produces a
		// strdrop.exec block when the Mutex temp is promoted to a
		// bindingDropString scope binding. Pre-fix there was none.
		if !strings.Contains(fn, "strdrop.exec") {
			t.Errorf("%s: expected promoted Mutex scope binding (strdrop.exec block); promotion did not fire\n%s", fnName, fn)
		}
		mtxDrop := `call void @"Mutex[int].drop"`
		guardDrop := `call void @MutexGuard.drop`
		if !strings.Contains(fn, mtxDrop) {
			t.Fatalf("%s: missing @\"Mutex[int].drop\" call\n%s", fnName, fn)
		}
		if !strings.Contains(fn, guardDrop) {
			t.Fatalf("%s: missing @MutexGuard.drop call\n%s", fnName, fn)
		}
		// Correct order: the scope-exit Mutex free (last Mutex.drop) must come
		// AFTER the guard's unlock. Pre-fix the Mutex temp dropped at statement
		// end (before the scope-lived guard) → lastIdx(mtx) < idx(guard).
		if t0655Last(fn, mtxDrop) <= t0655First(fn, guardDrop) {
			t.Errorf("%s: Mutex freed before MutexGuard unlocked (UAF ordering): lastIdx(Mutex.drop)=%d firstIdx(MutexGuard.drop)=%d\n%s",
				fnName, t0655Last(fn, mtxDrop), t0655First(fn, guardDrop), fn)
		}
	}
}

// Regression control: a bound-variable receiver (`m := ...; m.lock()`) must
// stay correct. Promotion is a no-op here (mutexRaw is a fresh load, not a
// tracked stmt-temp); the existing `m` scope binding already drops after the
// guard. Verify the correct ordering still holds.
func TestT0655BoundReceiverUnchanged(t *testing.T) {
	ir := generateIR(t, t0655Src)
	fn := extractFunction(ir, "__user.use_bound")
	if fn == "" {
		t.Fatal("__user.use_bound: function not found in IR")
	}
	mtxDrop := `call void @"Mutex[int].drop"`
	guardDrop := `call void @MutexGuard.drop`
	if !strings.Contains(fn, mtxDrop) || !strings.Contains(fn, guardDrop) {
		t.Fatalf("__user.use_bound: expected both Mutex.drop and MutexGuard.drop\n%s", fn)
	}
	if t0655Last(fn, mtxDrop) <= t0655First(fn, guardDrop) {
		t.Errorf("__user.use_bound: bound-receiver ordering regressed (Mutex freed before guard unlocked)\n%s", fn)
	}
}

// Generic / typeSubst path: a `Mutex[T]` temp receiver inside a generic body
// exercises the T0655-added `c.typeSubst != nil && mtxType != nil` substitution
// branch in genMutexMethodCall (expr.go) — not reached by the non-generic rows
// above. After monomorphization the substituted body must still promote the
// Mutex temp to a scope binding (strdrop.exec) using the identical
// @"Mutex[int].drop" symbol as the bound path, with the Mutex free ordered
// AFTER the guard unlock.
func TestT0655GenericMutexTempPromoted(t *testing.T) {
	ir := generateIR(t, `
		glk[T](T v) { use g := Mutex[T](v).lock(); }
		main() { glk[int](3); }
	`)
	// Monomorphized generic free functions are emitted with a quoted name
	// (@"glk[int]"); extractFunction matches @<name>( so pass the quoted form.
	fn := extractFunction(ir, `"glk[int]"`)
	if fn == "" {
		t.Fatalf("glk[int]: monomorphized generic function not found in IR")
	}
	if !strings.Contains(fn, "strdrop.exec") {
		t.Errorf("glk[int]: expected promoted Mutex scope binding (strdrop.exec) under typeSubst; promotion did not fire\n%s", fn)
	}
	mtxDrop := `call void @"Mutex[int].drop"`
	guardDrop := `call void @MutexGuard.drop`
	if !strings.Contains(fn, mtxDrop) {
		t.Fatalf("glk[int]: missing substituted @\"Mutex[int].drop\" (typeSubst did not resolve elemType)\n%s", fn)
	}
	if !strings.Contains(fn, guardDrop) {
		t.Fatalf("glk[int]: missing @MutexGuard.drop call\n%s", fn)
	}
	if t0655Last(fn, mtxDrop) <= t0655First(fn, guardDrop) {
		t.Errorf("glk[int]: Mutex freed before MutexGuard unlocked in generic body (UAF ordering): lastIdx(Mutex.drop)=%d firstIdx(MutexGuard.drop)=%d\n%s",
			t0655Last(fn, mtxDrop), t0655First(fn, guardDrop), fn)
	}
}

// Regression control: consume-only ctor temp (`take_mtx_i(Mutex[int](1));`)
// has no .lock(), so genMutexMethodCall and the promotion are never invoked.
// There must be no MutexGuard at all, and the Mutex must still be dropped.
func TestT0655ConsumeOnlyUnchanged(t *testing.T) {
	ir := generateIR(t, t0655Src)
	fn := extractFunction(ir, "__user.use_consume")
	if fn == "" {
		t.Fatal("__user.use_consume: function not found in IR")
	}
	if strings.Contains(fn, "@MutexGuard.drop") {
		t.Errorf("__user.use_consume: unexpected MutexGuard.drop (no .lock() in source)\n%s", fn)
	}
	if !strings.Contains(fn, `call void @"Mutex[int].drop"`) {
		t.Errorf("__user.use_consume: expected Mutex[int].drop for the consumed ctor temp\n%s", fn)
	}
}
