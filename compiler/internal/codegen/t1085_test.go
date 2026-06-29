package codegen

import (
	"strings"
	"testing"
)

// T1085: the non-diverging optional-handler unwrap (`o? _ { recovery }`) on an
// ident source leaked the absent-runtime recovery for inner kinds beyond the
// string/vector that T0778 fixed:
//   - opaque i8*-backed containers: Channel[T], Arc/Ref[T], Weak/Mutex/Task/
//     MutexGuard (tracked via the type-aware temp tracker).
//   - heap user types: Map[K,V], Set[T] (tracked via trackHeapValueTemp).
//
// Fix (genOptionalHandlerExpr): when the handler is non-diverging AND the source
// is an ident AND the inner is one of these kinds, neutralize the source
// optional's present flag in the some-block and track the merged phi as an owned
// statement temp at the merge block. These tests lock the IR shape so a future
// regression fails at the test layer, not only at the zero-tolerance leak gate.
// Runtime zero-leak behavior is covered by the e2e batch tests in
// tests/e2e/optional_handler_unwrap_test.pr.

// The neutralize-source GEP for an i8*-inner optional ({i1, i8*} alloca).
const t1085NeutralizeI8Sig = "getelementptr { i1, i8* }, { i1, i8* }* %o"

// TestT1085_HandlerNonDivergingIdentChannelTracked — Channel[int] inner. The
// some-block must neutralize the source's present flag, and the per-element-type
// Channel[int].drop must be reachable from the tracked-temp cleanup so the
// absent-runtime recovery channel is freed.
func TestT1085_HandlerNonDivergingIdentChannelTracked(t *testing.T) {
	ir := generateIR(t, `
		tfn() {
			Channel[int]? o = channel[int](capacity: 1);
			(o? _ { channel[int](capacity: 1) }).close();
		}
		main() { tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatalf("could not extract __user.tfn from IR:\n%s", ir)
	}
	if !strings.Contains(fn, t1085NeutralizeI8Sig) {
		t.Fatalf("expected neutralize-source GEP %q in tfn (some-block clears "+
			"the Channel optional's present flag), got none:\n%s",
			t1085NeutralizeI8Sig, fn)
	}
	if !strings.Contains(fn, `call void @"Channel[int].drop"`) {
		t.Fatalf("expected `call void @\"Channel[int].drop\"` (tracked phi freed "+
			"at statement end), got none:\n%s", fn)
	}
}

// TestT1085_HandlerNonDivergingIdentArcTracked — Arc/Ref[int] inner. Same shape
// as the channel case via trackTempWithDrop(getOrCreateArcDrop).
func TestT1085_HandlerNonDivergingIdentArcTracked(t *testing.T) {
	ir := generateIR(t, `
		tfn() int {
			Ref[int]? o = Ref[int](7);
			return (o? _ { Ref[int](99) }).borrow;
		}
		main() { _ := tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatalf("could not extract __user.tfn from IR:\n%s", ir)
	}
	if !strings.Contains(fn, t1085NeutralizeI8Sig) {
		t.Fatalf("expected neutralize-source GEP %q in tfn (some-block clears "+
			"the Arc optional's present flag), got none:\n%s",
			t1085NeutralizeI8Sig, fn)
	}
	if !strings.Contains(fn, `call void @"Ref[int].drop"`) {
		t.Fatalf("expected `call void @\"Ref[int].drop\"` (tracked phi freed at "+
			"statement end), got none:\n%s", fn)
	}
}

// Heap user-type optional has a {vtable, instance} value struct, so the source
// alloca is {i1, {i8*, i8*}}.
const t1085HeapNeutralizeSig = "getelementptr { i1, { i8*, i8* } }, { i1, { i8*, i8* } }* %o"

// TestT1085_HandlerNonDivergingIdentMapOwnedTracked — heap user-type inner (Map)
// from an OWNED optional ident source. The recovery is a block returning a
// moved-out local (`o? { mk := ...; mk }`), which genBlockValue claims (drop flag
// cleared in noneBlock) — so without phi-tracking the absent-runtime recovery
// leaks. Neutralizing the OWNED source transfers the present inner's ownership to
// the merged phi, which trackHeapValueTemp drops once at statement end.
func TestT1085_HandlerNonDivergingIdentMapOwnedTracked(t *testing.T) {
	ir := generateIR(t, `
		tfn() int {
			map[int, int]? o = none;
			return (o? { mk := map[int, int](); mk[9] = 9; mk }).len;
		}
		main() { _ := tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatalf("could not extract __user.tfn from IR:\n%s", ir)
	}
	if !strings.Contains(fn, t1085HeapNeutralizeSig) {
		t.Fatalf("expected neutralize-source GEP %q in tfn (some-block clears the "+
			"owned Map optional's present flag), got none:\n%s", t1085HeapNeutralizeSig, fn)
	}
	if !strings.Contains(fn, `call void @"Map[int, int].drop"`) {
		t.Fatalf("expected `call void @\"Map[int, int].drop\"` (tracked phi freed "+
			"at statement end), got none:\n%s", fn)
	}
}

// TestT1085_HandlerNonDivergingIdentBorrowNotTracked — the gate. A heap user-type
// optional bound from an RTTI downcast (`Sub? o = this as Sub`) is borrow-holding:
// the present arm aliases `this`'s instance, owned by the caller. The T1085
// neutralize+track must NOT fire (it would double-free the borrow — the
// paren_this csq_unwrap_sides_nondiverging regression). trackHeapUserTypeResult's
// T0753 ident-skip governs it; the bare-constructor recovery is tracked at its own
// construction site. Locks that the some-block does NOT clear o's present flag.
func TestT1085_HandlerNonDivergingIdentBorrowNotTracked(t *testing.T) {
	ir := generateIR(t, `
		type Base { int x; kind(this) string { return "base"; } }
		type Sub is Base { int y; kind(this) string { return "sub"; } }
		fn(Base b) int {
			Sub? o = b as Sub;
			return (o? _ { Sub(x: -1, y: -1) }).y;
		}
		main() { _ := fn(Sub(x: 1, y: 2)); }
	`)
	fn := extractFunction(ir, "__user.fn")
	if fn == "" {
		t.Fatalf("could not extract __user.fn from IR:\n%s", ir)
	}
	if strings.Contains(fn, t1085HeapNeutralizeSig) {
		t.Fatalf("did NOT expect neutralize-source GEP %q in fn (borrow-holding "+
			"optional from RTTI downcast — the present arm aliases an external "+
			"owner and must not be tracked), got:\n%s", t1085HeapNeutralizeSig, fn)
	}
}

// TestT1085_HandlerDivergingIdentChannelSkipped — the gate. A diverging handler
// degenerates the phi to okVal aliasing the source's owned inner; the existing
// T0753 ident-skip leaves it untracked (the optional's drop binding governs the
// lifetime). The `!handlerDiverged` gate must suppress the neutralize so the
// source isn't orphaned and the inner isn't double-freed.
func TestT1085_HandlerDivergingIdentChannelSkipped(t *testing.T) {
	ir := generateIR(t, `
		tfn() {
			Channel[int]? o = channel[int](capacity: 1);
			(o? _ { return }).close();
		}
		main() { tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatalf("could not extract __user.tfn from IR:\n%s", ir)
	}
	if strings.Contains(fn, t1085NeutralizeI8Sig) {
		t.Fatalf("did NOT expect neutralize-source GEP %q in tfn (diverging "+
			"handler must skip the T1085 neutralize), got:\n%s",
			t1085NeutralizeI8Sig, fn)
	}
}
