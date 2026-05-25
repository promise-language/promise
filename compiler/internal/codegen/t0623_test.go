package codegen

import (
	"strings"
	"testing"
)

// T0623: match-destructure of an enum variant whose field transitively owns a
// single-owner handle (Task/Mutex/MutexGuard) moves out. Codegen must:
//   - Register a drop binding for the moved-out handle binding (so the binding
//     drops the handle at arm-end / scope-exit).
//   - Null out the moved-out slot in the subject's enum-value alloca, so the
//     synth enum drop (firing on the subject at outer scope exit) sees null
//     there and skips it (single-owner-handle drops all null-check). Other
//     droppable variant fields are still freed by the synth drop.
// Non-moving arms (wildcard `_` slot, non-handle variant) must NOT null out
// the slot — the synth enum drop is the right cleanup path.

// TestT0623_TaskMatchMoveOutIR — the moving arm must store null into the
// subject's variant-field slot and register a Task[int] drop for the binding.
func TestT0623_TaskMatchMoveOutIR(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		enum Job { Pending, Running(Task[int] t) }
		caller() {
			j := Job.Running(go worker());
			match j {
				Job.Pending => assert(true, "p"),
				Job.Running(t) => assert(true, "r"),
			}
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// The moving arm must null out the handle slot in the subject's alloca.
	// Look for a store of null i8* — the destructure GEPs into %j's data area.
	if !strings.Contains(body, "store i8* null") {
		t.Errorf("expected store i8* null (slot null-out) in moving arm:\n%s", body)
	}
	// The binding must end up with a Task[int] drop site (registered via
	// maybeRegisterDrop → getOrCreateTaskDrop).
	if !strings.Contains(body, `@"Task[int].drop"`) {
		t.Errorf("expected call to @\"Task[int].drop\" (binding drop in moving arm):\n%s", body)
	}
	// The synth enum drop on the subject must still be wired up — the synth
	// drop walks the variant data and null-checks the handle slot at runtime.
	if !strings.Contains(body, "@Job.drop(") {
		t.Errorf("expected synth @Job.drop call (subject drop runs normally; null-check skips moved slot):\n%s", body)
	}
}

// TestT0623_WildcardArmDoesNotNullSubjectSlot — `_` binding on the handle
// variant does NOT null out the subject's slot (no move-out; synth enum drop
// frees the handle normally).
func TestT0623_WildcardArmDoesNotNullSubjectSlot(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		enum Job { Pending, Running(Task[int] t) }
		caller() {
			j := Job.Running(go worker());
			match j {
				Job.Pending => assert(true, "p"),
				Job.Running(_) => assert(true, "r"),
			}
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// The wildcard arm must not emit a slot null-out. The Task is owned by the
	// variant and freed by the synth drop. (No "store i8* null" anywhere in
	// the body except possibly in temp tracking — assert specifically that no
	// store of null targets a GEP into %j's data area.)
	// Heuristic: with wildcard, there's no GEP-then-store-null pair near %j.
	// We check the absence of the move-out hallmark: the binding drop registration.
	if strings.Contains(body, `%t = alloca`) {
		t.Errorf("wildcard arm must NOT create a %%t binding alloca:\n%s", body)
	}
}

// TestT0623_MultiFieldVariantNullOnlyHandleSlot — when a variant has both a
// dup'd droppable (string) and a moved handle (Task), the moving arm must:
//   - dup the string into the binding (independent copy)
//   - move the Task into the binding (registered drop)
//   - null ONLY the Task slot in the subject's alloca (the string slot is
//     still freed by the synth drop)
func TestT0623_MultiFieldVariantNullOnlyHandleSlot(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		enum E { Empty, Multi(string s, Task[int] t) }
		caller() {
			s := "hello world";
			e := E.Multi(s, go worker());
			match e {
				E.Empty => assert(true, "e"),
				E.Multi(a, b) => assert(true, "m"),
			}
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// The handle slot (i8* Task) must be null'd out.
	if !strings.Contains(body, "store i8* null") {
		t.Errorf("expected store i8* null for the Task slot in moving arm:\n%s", body)
	}
	// The Task drop for the binding must be registered.
	if !strings.Contains(body, `@"Task[int].drop"`) {
		t.Errorf("expected @\"Task[int].drop\" for moving-arm Task binding:\n%s", body)
	}
	// The synth enum drop on the subject must still run (frees the string slot).
	if !strings.Contains(body, "@E.drop(") {
		t.Errorf("expected synth @E.drop call (frees variant string; Task slot null'd):\n%s", body)
	}
}

// TestT0623_MutexMatchMoveOutIR — the same move-out lowering for Mutex[T]:
// register the per-instantiation Mutex drop on the binding, null the slot
// in the subject's alloca, and keep the synth enum drop call wired up.
// (Direct Mutex case is symmetric to Task at the predicate level but has a
// different per-instantiation drop name; ensures the maybeRegisterDrop path
// covers Mutex, not just Task.)
func TestT0623_MutexMatchMoveOutIR(t *testing.T) {
	ir := generateIR(t, `
		enum Holder { Empty, Locked(Mutex[int] m) }
		caller() {
			h := Holder.Locked(Mutex[int](7));
			match h {
				Holder.Empty => assert(true, "e"),
				Holder.Locked(m) => assert(true, "l"),
			}
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "store i8* null") {
		t.Errorf("expected store i8* null for the Mutex slot in moving arm:\n%s", body)
	}
	if !strings.Contains(body, `@"Mutex[int].drop"`) {
		t.Errorf("expected call to @\"Mutex[int].drop\" (binding drop in moving arm):\n%s", body)
	}
	if !strings.Contains(body, "@Holder.drop(") {
		t.Errorf("expected synth @Holder.drop call (subject drop runs; Mutex slot null'd):\n%s", body)
	}
}

// TestT0633_NestedUserTypeWrapperMoveOutIR — the variant field is a user-type
// wrapper (TaskWrapper) that transitively owns a Task, not the Task directly.
// The wrapper field lowers to the {i8*,i8*} value struct, so:
//   - Part 1: the moved-out slot in the subject's alloca is zero-init'd via
//     `store { i8*, i8* } zeroinitializer` (NOT a panic, NOT a bare i8* null —
//     the prior code's `fieldType.(*irtypes.PointerType)` assertion panicked
//     here because fieldType is *StructType for a wrapper).
//   - Binding owns the wrapper → @TaskWrapper.drop registered for it.
//   - Subject's synth @Wrap.drop is still wired up.
//   - Part 2: inside @Wrap.drop, the wrapper-field drop dispatch is guarded by
//     an `icmp eq i8* … null` (varfield.drop/varfield.skip blocks) so the
//     zeroed slot's null instance ptr is skipped instead of segfaulting.
func TestT0633_NestedUserTypeWrapperMoveOutIR(t *testing.T) {
	// generateIR runs the full Compile; the pre-fix type assertion panic in
	// nullSubjectHandleSlot would crash this call (Defect 1 regression guard).
	ir := generateIR(t, `
		worker() int { return 42; }
		type TaskWrapper { Task[int] t; }
		enum Wrap { Empty, Inner(TaskWrapper w) }
		caller() {
			w := Wrap.Inner(TaskWrapper(t: go worker()));
			match w {
				Wrap.Empty => assert(true, "e"),
				Wrap.Inner(wrapper) => assert(true, "i"),
			}
		}
		main() { caller(); }
	`)

	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// Part 1: the moved-out wrapper slot is zero-init'd as a struct (not a
	// bare pointer null — that path panicked before the fix).
	if !strings.Contains(body, "store { i8*, i8* } zeroinitializer") {
		t.Errorf("expected `store { i8*, i8* } zeroinitializer` (zero-init moved-out wrapper slot):\n%s", body)
	}
	// The binding owns the wrapper — its drop (which drops the inner Task) is
	// registered for the move-out binding.
	if !strings.Contains(body, "@TaskWrapper.drop") {
		t.Errorf("expected @TaskWrapper.drop (move-out binding owns the wrapper):\n%s", body)
	}
	// The subject's synth enum drop is still wired up (it walks the variant
	// data; Part 2's null-check skips the zeroed slot at runtime).
	if !strings.Contains(body, "@Wrap.drop(") {
		t.Errorf("expected synth @Wrap.drop call (subject drop runs; wrapper slot null-guarded):\n%s", body)
	}

	// Part 2: the synth @Wrap.drop body must null-guard the wrapper-field drop
	// dispatch — without it, the zeroed moved-out slot segfaults.
	dropBody := extractFunction(ir, "Wrap.drop")
	if dropBody == "" {
		t.Fatalf("expected @Wrap.drop in IR")
	}
	if !strings.Contains(dropBody, "varfield.drop") || !strings.Contains(dropBody, "varfield.skip") {
		t.Errorf("expected varfield.drop/varfield.skip null-guard blocks in @Wrap.drop:\n%s", dropBody)
	}
	if !strings.Contains(dropBody, "icmp eq i8*") || !strings.Contains(dropBody, ", null") {
		t.Errorf("expected `icmp eq i8* … null` null-check guarding the wrapper drop:\n%s", dropBody)
	}
	if !strings.Contains(dropBody, "call void @TaskWrapper.drop(") {
		t.Errorf("expected guarded `call void @TaskWrapper.drop(` inside @Wrap.drop:\n%s", dropBody)
	}
}

// TestT0633_GenericWrapperMoveOutNullGuardIR — same defect for a generic
// wrapper (Holder[int]). A generic instance that NeedsSynthDrop flows through
// emitVariantFieldDrop's named/synth-drop branch (compiler.go:8685), which
// uses monoName(inst) for the owner when typ is a *types.Instance — so the
// mono drop @"Holder[int].drop" is reached there (NOT the later B0202
// mono-instance branch at 8708, whose guarded body is unreachable for a
// NeedsSynthDrop type — verified via coverage). What matters: the generic
// wrapper's drop dispatch is reached through the same T0633 `icmp eq i8* …
// null` guard.
func TestT0633_GenericWrapperMoveOutNullGuardIR(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		type Holder[T] { Task[T] handle; }
		enum GWrap { GEmpty, GInner(Holder[int] h) }
		caller() {
			g := GWrap.GInner(Holder[int](handle: go worker()));
			match g {
				GWrap.GEmpty => assert(true, "e"),
				GWrap.GInner(holder) => assert(true, "i"),
			}
		}
		main() { caller(); }
	`)

	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "store { i8*, i8* } zeroinitializer") {
		t.Errorf("expected `store { i8*, i8* } zeroinitializer` (zero-init moved-out generic-wrapper slot):\n%s", body)
	}

	dropBody := extractFunction(ir, "GWrap.drop")
	if dropBody == "" {
		t.Fatalf("expected @GWrap.drop in IR")
	}
	if !strings.Contains(dropBody, "varfield.drop") || !strings.Contains(dropBody, "varfield.skip") {
		t.Errorf("expected varfield.drop/varfield.skip null-guard blocks in @GWrap.drop:\n%s", dropBody)
	}
	if !strings.Contains(dropBody, "icmp eq i8*") || !strings.Contains(dropBody, ", null") {
		t.Errorf("expected `icmp eq i8* … null` null-check guarding the generic-wrapper drop:\n%s", dropBody)
	}
	if !strings.Contains(dropBody, `call void @"Holder[int].drop"(`) {
		t.Errorf("expected guarded `call void @\"Holder[int].drop\"(` inside @GWrap.drop:\n%s", dropBody)
	}
}

// TestT0633_ArrayOfHandleMoveOutIR — the predicate (FirstNestedSingleOwnerHandle)
// also matches a fixed array of handles, whose variant-field slot lowers to
// `[N x i8*]` (an *irtypes.ArrayType). c.zeroValue's default arm returns an
// `i64 0`, which is store-incompatible with an `[N x i8*]*` slot — so the slot
// null-out must emit `store [N x i8*] zeroinitializer` (not via c.zeroValue).
// The synth enum drop walks each element through the per-instantiation handle
// drop, which null-checks at entry, so the zeroed elements are safe no-ops.
func TestT0633_ArrayOfHandleMoveOutIR(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		enum TA { Empty, Arr(Task[int][2] ts) }
		caller() {
			a := TA.Arr([go worker(), go worker()]);
			match a {
				TA.Empty => assert(true, "e"),
				TA.Arr(ts) => assert(true, "a"),
			}
		}
		main() { caller(); }
	`)

	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// Part 1: the moved-out array slot is zero-init'd as an array aggregate
	// (NOT `i64 0` from c.zeroValue's default arm — that store is invalid IR).
	if !strings.Contains(body, "store [2 x i8*] zeroinitializer") {
		t.Errorf("expected `store [2 x i8*] zeroinitializer` (zero-init moved-out array-of-handle slot):\n%s", body)
	}
	if strings.Contains(body, "store i64 0, [2 x i8*]*") {
		t.Errorf("array slot must NOT receive an i64 0 (type-incompatible store):\n%s", body)
	}

	// The subject's synth drop walks each array element via the per-element
	// handle drop (which null-checks), so zeroed elements are safe no-ops.
	dropBody := extractFunction(ir, "TA.drop")
	if dropBody == "" {
		t.Fatalf("expected @TA.drop in IR")
	}
	if !strings.Contains(dropBody, `call void @"Task[int].drop"(`) {
		t.Errorf("expected per-element `call void @\"Task[int].drop\"(` in @TA.drop:\n%s", dropBody)
	}
}

// TestT0633_PolymorphicWrapperMoveOutNullGuardIR — the variant field is a
// POLYMORPHIC user-type wrapper (Base has a child Derived → needsVtable(Base)
// is true), so the subject's synth enum drop routes the wrapper-field drop
// through emitVariantFieldDrop's polymorphic branch (compiler.go:8671):
// emitStructuralInstanceDrop, which dispatches Base's drop indirectly through
// the typeinfo drop_fn_ptr (NOT a direct `call void @Base.drop`).
// emitStructuralInstanceDrop dereferences the instance ptr via loadVariantPtr
// with no null-check, so the Part-1 zero-init'd moved-out Base slot
// ({i8*,i8*} value struct → {null,null}) would segfault without the T0633
// null-guard. This branch had ZERO test coverage (neither the nested-wrapper
// test nor the generic-wrapper test reaches it — plain/generic wrappers take
// the named/mono synth-drop branches at 8685/8708, not the polymorphic one).
func TestT0633_PolymorphicWrapperMoveOutNullGuardIR(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		type Base { Task[int] t; describe() string { return "base"; } }
		type Derived is Base { describe() string { return "derived"; } }
		enum PolyWrap { Empty, Held(Base b) }
		caller() {
			w := PolyWrap.Held(Base(t: go worker()));
			match w {
				PolyWrap.Empty => assert(true, "e"),
				PolyWrap.Held(b) => assert(true, "h"),
			}
		}
		main() { caller(); }
	`)

	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// Part 1: the moved-out polymorphic wrapper slot is zero-init'd as a
	// struct (the {i8*,i8*} value struct), not a bare pointer null.
	if !strings.Contains(body, "store { i8*, i8* } zeroinitializer") {
		t.Errorf("expected `store { i8*, i8* } zeroinitializer` (zero-init moved-out polymorphic-wrapper slot):\n%s", body)
	}
	// The binding owns the Base wrapper — its drop is registered for the
	// move-out binding (direct named call, guarded by its own null-check).
	if !strings.Contains(body, "@Base.drop") {
		t.Errorf("expected @Base.drop (move-out binding owns the polymorphic wrapper):\n%s", body)
	}
	// The subject's synth enum drop is still wired up.
	if !strings.Contains(body, "@PolyWrap.drop(") {
		t.Errorf("expected synth @PolyWrap.drop call (subject drop runs; wrapper slot null-guarded):\n%s", body)
	}

	// Part 2: the synth @PolyWrap.drop body must null-guard the polymorphic
	// wrapper-field drop dispatch.
	dropBody := extractFunction(ir, "PolyWrap.drop")
	if dropBody == "" {
		t.Fatalf("expected @PolyWrap.drop in IR")
	}
	if !strings.Contains(dropBody, "varfield.drop") || !strings.Contains(dropBody, "varfield.skip") {
		t.Errorf("expected varfield.drop/varfield.skip null-guard blocks in @PolyWrap.drop:\n%s", dropBody)
	}
	if !strings.Contains(dropBody, "icmp eq i8*") || !strings.Contains(dropBody, ", null") {
		t.Errorf("expected `icmp eq i8* … null` null-check guarding the polymorphic-wrapper drop:\n%s", dropBody)
	}
	// Pins the POLYMORPHIC branch (8671) specifically: the structural instance
	// drop dispatches Base indirectly through the typeinfo drop_fn_ptr — the
	// named-drop branch (8685) would instead emit a direct `call void
	// @Base.drop` *inside* @PolyWrap.drop. Absence of that direct call proves
	// emitStructuralInstanceDrop (the guarded path) is what's exercised here.
	if strings.Contains(dropBody, "call void @Base.drop(") {
		t.Errorf("expected NO direct `call void @Base.drop(` inside @PolyWrap.drop (polymorphic branch dispatches via typeinfo drop_fn_ptr, not the named-drop branch):\n%s", dropBody)
	}
}

// TestT0633_TupleHandleMoveOutMixedAggregateIR — the variant field is a TUPLE
// (Task[int], int) that transitively owns a single-owner handle.
// firstNestedSingleOwnerHandle recurses into *types.Tuple (clone.go:274) so
// the T0623 move-out path fires. The tuple lowers to the MIXED pointer+scalar
// aggregate `{ i8*, i64 }` — structurally distinct from the {i8*,i8*}
// value-struct wrapper (TestT0633_NestedUserTypeWrapperMoveOutIR) and the
// [N x i8*] array (TestT0633_ArrayOfHandleMoveOutIR). nullSubjectHandleSlot's
// else-branch `NewZeroInitializer` must zero-init it as a typed aggregate; a
// bare-pointer `null` (the pre-T0633 code) or c.zeroValue's `i64 0` default
// would be a type-incompatible store for a `{ i8*, i64 }*` slot. The subject's
// synth enum drop walks the tuple element through the direct Task drop (which
// null-checks at entry), so the zeroed task ptr is a safe no-op.
func TestT0633_TupleHandleMoveOutMixedAggregateIR(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		enum TupHold { Empty, Pair((Task[int], int) p) }
		caller() {
			h := TupHold.Pair((go worker(), 7));
			match h {
				TupHold.Empty => assert(true, "e"),
				TupHold.Pair(p) => assert(true, "p"),
			}
		}
		main() { caller(); }
	`)

	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// Part 1: the moved-out tuple slot is zero-init'd as the mixed
	// pointer+scalar aggregate `{ i8*, i64 }` (NOT a bare pointer null and NOT
	// an `i64 0` — both type-incompatible with a `{ i8*, i64 }*` slot).
	if !strings.Contains(body, "store { i8*, i64 } zeroinitializer") {
		t.Errorf("expected `store { i8*, i64 } zeroinitializer` (zero-init moved-out tuple slot):\n%s", body)
	}
	if strings.Contains(body, "store i64 0, { i8*, i64 }*") {
		t.Errorf("tuple slot must NOT receive a bare `i64 0` (type-incompatible store):\n%s", body)
	}
	// The subject's synth enum drop is still wired up.
	if !strings.Contains(body, "@TupHold.drop(") {
		t.Errorf("expected synth @TupHold.drop call (subject drop runs; tuple task ptr zeroed):\n%s", body)
	}

	// The synth drop walks the tuple element via the direct per-handle Task
	// drop (which null-checks at entry — so the zeroed task ptr is safe).
	dropBody := extractFunction(ir, "TupHold.drop")
	if dropBody == "" {
		t.Fatalf("expected @TupHold.drop in IR")
	}
	if !strings.Contains(dropBody, `call void @"Task[int].drop"(`) {
		t.Errorf("expected `call void @\"Task[int].drop\"(` (tuple-element handle drop) in @TupHold.drop:\n%s", dropBody)
	}
}
