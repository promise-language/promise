package codegen

import (
	"strings"
	"testing"
)

// T0562: maybeClearReceiverDropFlag (B0250 alias-clear) and the parallel
// maybeClearBindingDropFlagOnThisAlias (T0347) used too-loose shape checks on
// `val.Type()` and `recvAlloca`. Two failure modes:
//
//   - Native handle receiver + Optional[native] result (e.g.,
//     `Weak[int]? opt = a.downgrade()`): val became `{i1, i8*}`, recvAlloca was
//     `alloca i8*`. The struct check passed (any StructType), then
//     `load userValueType()` from an 8-byte slot read 16 bytes (UB). If the
//     trailing garbage matched the inner pointer, the receiver's drop flag was
//     wrongly cleared → Arc.drop never fired → leak.
//
//   - User-type receiver + Optional[user-type] result (e.g.,
//     `Box? opt = b.clone()`): val became `{i1, {i8*, i8*}}`. `extractValue(val, 1)`
//     returned `{i8*, i8*}` while `extractValue(recvVal, 1)` returned `i8*`,
//     so `icmp(struct, ptr)` panicked the IR builder at compile time.
//
// Fix: tighten the guards to bail when either side isn't exactly `{i8*, i8*}`.
// The B0250/T0347 alias-clear pattern only applies to bare user value structs.

// TestT0562_OptionalNativeReceiverNoAliasClear — `Weak[int]? opt = a.downgrade()`
// must NOT emit the return.this.clear/skip blocks. With the old code these blocks
// were emitted off an undefined-behavior load, and the icmp could spuriously match
// garbage, clearing `a`'s drop flag and leaking the Arc.
func TestT0562_OptionalNativeReceiverNoAliasClear(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			a := Arc[int](7);
			Weak[int]? opt = a.downgrade();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if strings.Contains(body, "return.this.clear") {
		t.Errorf("expected NO return.this.clear block (native handle recv + Optional result must skip alias-clear):\n%s", body)
	}
	if strings.Contains(body, "return.this.skip") {
		t.Errorf("expected NO return.this.skip block (native handle recv + Optional result must skip alias-clear):\n%s", body)
	}
}

// TestT0562_OptionalMutexGuardNoAliasClear — `MutexGuard[int]? opt = m.lock()`
// must compile and skip the alias-clear emission. Pre-fix: same UB pattern as
// the Weak case — Mutex alloca is `i8*` but recvAlloca was loaded as
// userValueType() (16 bytes), and the spurious match leaked the mutex chain
// (4 allocations: mutex + cond + mutex_internal + guard).
func TestT0562_OptionalMutexGuardNoAliasClear(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			m := Mutex[int](42);
			MutexGuard[int]? opt = m.lock();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if strings.Contains(body, "return.this.clear") {
		t.Errorf("expected NO return.this.clear block (Mutex recv + Optional[MutexGuard] result):\n%s", body)
	}
}

// TestT0562_OptionalUserTypeReceiverNoAliasClear — `Box? opt = b.clone()` must
// compile without crashing. Pre-fix: val was `{i1, {i8*, i8*}}`, the existing
// StructType check passed, then `extractValue(val, 1)` returned `{i8*, i8*}`
// and `icmp(struct, ptr)` panicked the LLVM IR builder.
func TestT0562_OptionalUserTypeReceiverNoAliasClear(t *testing.T) {
	ir := generateIR(t, `
		type Box `+"`"+`public {
			int x;
			clone(this) Box { return Box(x: this.x); }
		}
		caller() {
			b := Box(x: 1);
			Box? opt = b.clone();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// With the fix, the function bails before emitting the alias-clear
	// branches, so neither block should appear for this Optional-wrapped path.
	if strings.Contains(body, "return.this.clear") {
		t.Errorf("expected NO return.this.clear block for Optional-wrapped user-type result:\n%s", body)
	}
}

// TestT0562_OptionalUserTypeThisAliasNoCrash — `Box? r = this.clone()` inside a
// method must compile without crashing. Same root cause but exercised through
// maybeClearBindingDropFlagOnThisAlias (T0347 helper) rather than
// maybeClearReceiverDropFlag — val is `{i1, {i8*, i8*}}` so field 1 is a struct
// and the icmp would have panicked.
func TestT0562_OptionalUserTypeThisAliasNoCrash(t *testing.T) {
	ir := generateIR(t, `
		type Box `+"`"+`public {
			int x;
			clone(this) Box { return Box(x: this.x); }
			wrap(this) Box? {
				Box? r = this.clone();
				return r;
			}
		}
		caller() {
			b := Box(x: 1);
			Box? r = b.wrap();
		}
		main() { caller(); }
	`)
	// The presence of __user.Box.wrap in the IR proves codegen completed
	// without panicking on the `Box? r = this.clone()` line.
	if !strings.Contains(ir, "@Box.wrap(") {
		t.Errorf("expected @Box.wrap in IR (compilation of Optional this-alias must not panic)")
	}
	body := extractFunction(ir, "Box.wrap")
	if body == "" {
		t.Fatalf("expected Box.wrap in IR")
	}
	if strings.Contains(body, "this.alias.clear") {
		t.Errorf("expected NO this.alias.clear block for Optional-wrapped this-alias path:\n%s", body)
	}
}

// TestT0562_BareUserTypeAliasClearStillEmitted — sanity-check the supported
// case: `w2 := w.self()` (bare user type on both sides) must still emit the
// alias-clear blocks. The fix only narrows which shapes trigger emission; the
// supported shape is unchanged.
func TestT0562_BareUserTypeAliasClearStillEmitted(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int value; self() Wrapper { return this; } }
		main() { w := Wrapper(value: 42); w2 := w.self(); }
	`)
	if !strings.Contains(ir, "return.this.clear") {
		t.Errorf("expected return.this.clear block for bare user-type alias case:\n%s", ir)
	}
	if !strings.Contains(ir, "return.this.skip") {
		t.Errorf("expected return.this.skip block for bare user-type alias case:\n%s", ir)
	}
}

// TestT0562_TypedOptionalMutexGuardDeclEmitsBothDrops — this is the test that
// the T0561 deferred-coverage comment promised: `MutexGuard[int]? opt = m.lock()`
// (typed Optional decl from a native-handle method call) must claim the
// stmt-temp AND register the Optional's scope drop.
//
// Pre-T0562 fix, this declaration form leaked because maybeClearReceiverDropFlag
// wrongly cleared `m`'s drop flag (UB-load garbage match) AND the alias-clear
// path consumed the spot where the stmt-temp would have been claimed.
//
// After the fix, the alias-clear emission is skipped, so the normal Optional
// claim/registration path runs: tmp.drop block tracking the lock() result and
// optdrop.check block from the Optional scope binding.
func TestT0562_TypedOptionalMutexGuardDeclEmitsBothDrops(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			m := Mutex[int](42);
			MutexGuard[int]? opt = m.lock();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !hasMutexGuardTmpDropBlock(body) {
		t.Errorf("expected tmp.drop block calling @MutexGuard.drop (stmt-temp tracking for Optional decl):\n%s", body)
	}
	if !strings.Contains(body, "optdrop.check") {
		t.Errorf("expected optdrop.check block (Optional[MutexGuard] scope cleanup):\n%s", body)
	}
}
