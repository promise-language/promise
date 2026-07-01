package codegen

import (
	"strings"
	"testing"
)

// T1166: an elvis `?:` bound to a MEMBER or INDEX target (`hold.h = a ?: b`,
// `v[i] = a ?: b`) stores into an OWNED field/element with no per-path drop flag —
// the container's synthesized drop frees it unconditionally. So on a borrowed
// some/none path the field/element cannot hold a borrow-alias (as a local can via
// its per-path drop flag, T1014); it must OWN an independent copy. genElvis
// force-clones the borrowed operand on each aliasing path (someOwnsInner/noneOwned
// forced true) via cloneResolvedValue, collapsing every path onto the already-working
// owned path that the member/index assign branch claims. Pre-fix the field/element
// aliased the caller-owned inner and double-freed it → SEGV.
//
// These tests lock that the force-clone (heapdup.copy) is emitted inside the elvis
// some/none blocks for a member/index target, and — critically — is NOT emitted for
// the plain bound var-decl form (`m := a ?: b`), which keeps the T0940/T1014
// borrow-on-both behavior (the fix is gated to member/index targets only).

// TestT1166MemberTargetBorrowedParamForceClones: `hold.h = a ?: b` with both operands
// borrowed params. Both the some-path (borrowed optional inner) and none-path
// (borrowed default) must be deep-cloned so the field owns an independent copy.
func TestT1166MemberTargetBorrowedParamForceClones(t *testing.T) {
	ir := generateIR(t, `
		type HeapBox { int[] data; }
		type Holder { HeapBox h; }
		pick(HeapBox? a, HeapBox b) {
			Holder hold = Holder(h: HeapBox(data: []));
			hold.h = a ?: b;
		}
		demo() {
			HeapBox x = HeapBox(data: []);
			HeapBox? a = none;
			pick(a, x);
		}
	`)
	fn := extractFunction(ir, "__user.pick")
	if fn == "" {
		t.Fatal("could not extract __user.pick")
	}
	// The borrowed operand is deep-cloned so the field owns it.
	assertContains(t, fn, "heapdup.copy")
	// The none-path borrowed default is cloned inside the elvis.none region.
	if !strings.Contains(noneBlockOf(t, fn), "heapdup") {
		t.Errorf("expected a force-clone (heapdup) inside the elvis.none block for a member target (T1166)\n%s", fn)
	}
}

// TestT1166IndexTargetBorrowedParamForceClones: `v[0] = a ?: b` — index-target arm of
// the same fix. The element must own a clone of the borrowed operand.
func TestT1166IndexTargetBorrowedParamForceClones(t *testing.T) {
	ir := generateIR(t, `
		type HeapBox { int[] data; }
		pick(HeapBox? a, HeapBox b) {
			HeapBox[] v = []; v.push(HeapBox(data: []));
			v[0] = a ?: b;
		}
		demo() {
			HeapBox x = HeapBox(data: []);
			HeapBox? a = none;
			pick(a, x);
		}
	`)
	fn := extractFunction(ir, "__user.pick")
	if fn == "" {
		t.Fatal("could not extract __user.pick")
	}
	assertContains(t, fn, "heapdup.copy")
}

// TestT1166StringFieldTargetForceClonesViaStrdup: the vecOrStr arm of the gate. A
// string field target must force-clone the borrowed operand via the dupString
// representation (strdup.copy), not heapdup — locks cloneResolvedValue picks the
// string path for a string-typed elvis result.
func TestT1166StringFieldTargetForceClonesViaStrdup(t *testing.T) {
	ir := generateIR(t, `
		type StrH { string s; }
		pick(string? a, string b) {
			StrH h = StrH(s: "");
			h.s = a ?: b;
		}
		demo() {
			string x = "";
			string? a = none;
			pick(a, x);
		}
	`)
	fn := extractFunction(ir, "__user.pick")
	if fn == "" {
		t.Fatal("could not extract __user.pick")
	}
	// String representation clones via promise_string_new inside a strdup.copy block.
	assertContains(t, fn, "strdup.copy")
	// The none-path borrowed default is cloned inside the elvis.none region.
	if !strings.Contains(noneBlockOf(t, fn), "strdup") {
		t.Errorf("expected a force-clone (strdup) inside the elvis.none block for a string field target (T1166)\n%s", fn)
	}
}

// TestT1166ScalarFieldTargetDoesNotForceClone: gate-false arm. A member target is
// signalled (elvisResultOwnsForced), but an int result is neither vecOrStr nor a
// heap-drop type, so forceOwnClone stays false and NO clone is emitted — a plain
// scalar phi select. Guards against the member/index signal over-triggering.
func TestT1166ScalarFieldTargetDoesNotForceClone(t *testing.T) {
	ir := generateIR(t, `
		type IntH { int n; }
		pick(int? a, int b) {
			IntH h = IntH(n: 0);
			h.n = a ?: b;
		}
		demo() { pick(none, 7); }
	`)
	fn := extractFunction(ir, "__user.pick")
	if fn == "" {
		t.Fatal("could not extract __user.pick")
	}
	for _, marker := range []string{"heapdup", "strdup.copy", "dupvector"} {
		if strings.Contains(strings.ToLower(fn), marker) {
			t.Errorf("scalar (int) field elvis target must NOT force-clone — gate is false for non-droppable results (T1166); found %q\n%s", marker, fn)
		}
	}
}

// TestT1166BoundVarDeclDoesNotForceClone locks the gating: the plain bound var-decl
// `m := a ?: b` (an IdentExpr target, handled by T1014's per-path flag) must NOT emit
// the T1166 force-clone — `m` borrows the caller-owned operand on both paths. If the
// force-clone leaked into this path it would deep-copy borrowed operands the local is
// supposed to alias (wasted work + changed semantics).
func TestT1166BoundVarDeclDoesNotForceClone(t *testing.T) {
	ir := generateIR(t, `
		type HeapBox { int[] data; }
		pick(HeapBox? a, HeapBox b) {
			HeapBox m = a ?: b;
		}
		demo() {
			HeapBox x = HeapBox(data: []);
			HeapBox? a = none;
			pick(a, x);
		}
	`)
	fn := extractFunction(ir, "__user.pick")
	if fn == "" {
		t.Fatal("could not extract __user.pick")
	}
	if strings.Contains(fn, "heapdup") {
		t.Errorf("bound var-decl elvis (IdentExpr target) must NOT force-clone — borrow-on-both preserved (T1166 gated to member/index)\n%s", fn)
	}
}
