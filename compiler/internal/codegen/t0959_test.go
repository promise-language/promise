package codegen

import (
	"strings"
	"testing"
)

// T0959: a non-native `++`/`--` on an `IdentExpr` target is `x = x.++()`, which
// drops the old value before storing the fresh operator result. That drop-old
// must be gated on the local's ownership: dropping it unconditionally by static
// type double-frees the caller's object for a non-owning binding (borrow-by-
// default heap param or a `T&`/`T~` reference-bound local). The fix mirrors
// genAssignStmt's OpAssign path — gate drop-old behind the runtime drop flag and
// re-arm it after the store.

// Borrow-by-default heap param: `bump(Counter c) { c++; }`. The caller owns the
// original, so the old value must NOT be freed at runtime — otherwise it is
// double-freed. Under the merged T1194 design the reassigned param DOES get a
// drop binding, but its flag is initialized to 0 (borrowed), so the drop-old is
// emitted behind a runtime flag gate (incdec.flagdrop) and never fires; the flag
// is armed to 1 only after the store so the fresh result is tracked (no leak).
// This supersedes the original T0959 "no drop machinery at all" assertion.
func TestT0959_BorrowParamIncDecNoDropOld(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int n;
			new(~this, int n) { this.n = n; }
			drop(~this) {}
			++(this) Counter { return Counter(this.n + 1); }
		}
		bump(Counter c) { c++; }
		main() { bump(Counter(5)); }
	`)
	body := extractFunction(ir, "__user.bump")
	if body == "" {
		t.Fatalf("expected @__user.bump in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@\"Counter.++\"(i8*") {
		t.Fatalf("expected operator dispatch `@\"Counter.++\"(i8* ...)` in bump:\n%s", body)
	}
	// The borrow-reassign binding initializes the drop flag to 0 (caller owns the
	// original), guaranteeing the drop-old branch is never taken at runtime.
	if !strings.Contains(body, "store i1 false, i1* %c.dropflag") {
		t.Fatalf("borrow-param inc/dec must init its drop flag to 0 (borrowed original):\n%s", body)
	}
	// The drop-old is present but gated behind the runtime flag (never fires here).
	if !strings.Contains(body, "incdec.flagdrop") {
		t.Fatalf("borrow-param inc/dec must gate the drop-old behind the runtime flag:\n%s", body)
	}
}

// Owned local: `c := Counter(...); c++`. The local owns its binding (drop flag
// initialized to 1), so drop-old fires — but now guarded behind the flag. Assert
// the flag-gated drop-old block AND the shared user-drop block are present
// (regression guard for TestT0880_HeapNamedIncDecDispatchAndDropOld).
func TestT0959_OwnedLocalIncDecGuardedDropOld(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int n;
			new(~this, int n) { this.n = n; }
			drop(~this) {}
			++(this) Counter { return Counter(this.n + 1); }
		}
		caller() {
			c := Counter(0);
			c++;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// Drop-old is now gated on the runtime drop flag (incdec.flagdrop).
	if !strings.Contains(body, "incdec.flagdrop") {
		t.Fatalf("expected flag-gated drop-old block `incdec.flagdrop` for an owned local:\n%s", body)
	}
	// The shared dropOldUserValueAtPtr user-drop block still appears (nested under
	// the flag-true path).
	if !strings.Contains(body, "incdec.userdrop") {
		t.Fatalf("expected user-drop block `incdec.userdrop` for an owned local:\n%s", body)
	}
	// The flag-gate continuation block, after which the flag is re-armed so the
	// fresh result is owned.
	if !strings.Contains(body, "incdec.flagdrop.done") {
		t.Fatalf("expected flag-gate continuation block `incdec.flagdrop.done`:\n%s", body)
	}
}

// Reference-bound local: `Counter &r = owner; r++`. `Counter &r = owner` moves
// ownership into `r` (flag=1), and `r++` reassigns it. targetType is the
// `Counter&` reference wrapper — the fix strips the reference so
// dropOldUserValueAtPtr recognizes the droppable underlying type and drops the
// old instance (guarded by the flag) instead of no-oping and leaking it.
func TestT0959_RefBoundLocalIncDecStripsRefAndDropsOld(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int n;
			new(~this, int n) { this.n = n; }
			drop(~this) {}
			++(this) Counter { return Counter(this.n + 1); }
		}
		caller() {
			owner := Counter(5);
			Counter &r = owner;
			r++;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// The reference wrapper is stripped, so drop-old actually fires (guarded).
	if !strings.Contains(body, "incdec.flagdrop") {
		t.Fatalf("expected flag-gated drop-old block `incdec.flagdrop` for a ref-bound local:\n%s", body)
	}
	if !strings.Contains(body, "incdec.userdrop") {
		t.Fatalf("expected user-drop block `incdec.userdrop` (ref stripped to underlying type):\n%s", body)
	}
}

// Mutable-reference-bound local: `Counter ~r = owner; r++`. Like the shared-ref
// case, `Counter ~r = owner` moves ownership into `r` (flag=1). targetType is
// the `Counter~` (MutRef) wrapper — this exercises the *MutRef* arm of the
// ref-stripping switch (the shared-ref `&`/SharedRef arm is covered above).
// Without stripping, dropOldUserValueAtPtr would no-op on the wrapper and leak
// the old instance.
func TestT0959_MutRefBoundLocalIncDecStripsRefAndDropsOld(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int n;
			new(~this, int n) { this.n = n; }
			drop(~this) {}
			++(this) Counter { return Counter(this.n + 1); }
		}
		caller() {
			owner := Counter(5);
			Counter ~r = owner;
			r++;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// The MutRef wrapper is stripped, so drop-old actually fires (guarded).
	if !strings.Contains(body, "incdec.flagdrop") {
		t.Fatalf("expected flag-gated drop-old block `incdec.flagdrop` for a mutref-bound local:\n%s", body)
	}
	if !strings.Contains(body, "incdec.userdrop") {
		t.Fatalf("expected user-drop block `incdec.userdrop` (MutRef stripped to underlying type):\n%s", body)
	}
}
