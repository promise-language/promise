package sema

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/types"
)

// T1053: mutation through a shared (read-only) borrow is rejected for every
// mutation form — a mutating method call (`v.push(x)`), an index/slice write
// (`v[i] = x`, `v[:] = …`), a property-setter write (`c.value = 5`), and a direct
// field store (covered by T0716). Every stdlib container mutator now takes a
// `~this` receiver, so the single `checkReceiverMutability` / write check blocks
// them. The sole exception is a receiver whose underlying type is marked
// `interior` (Channel / Mutex / MutexGuard), which opt into interior mutability.

// --- Reject: container mutators through a shared borrow ---

func TestT1053_VectorPushThroughSharedBorrowRejected(t *testing.T) {
	// The reachable memory-safety hole: `v.push(x)` through a shared borrow could
	// realloc the heap vector and leave the caller with a dangling pointer.
	errs := checkErrs(t, `
		grow(int[] v) { v.push(7); }
	`)
	expectError(t, errs, "cannot call mutating method 'push' through a shared (read-only) borrow")
}

func TestT1053_VectorIndexWriteThroughSharedBorrowRejected(t *testing.T) {
	errs := checkErrs(t, `
		set0(int[] v) { v[0] = 7; }
	`)
	expectError(t, errs, "cannot mutate element through a shared (read-only) borrow")
}

func TestT1053_VectorSliceWriteThroughSharedBorrowRejected(t *testing.T) {
	errs := checkErrs(t, `
		fill(int[] v) { v[:] = [1, 2, 3]; }
	`)
	expectError(t, errs, "cannot mutate slice through a shared (read-only) borrow")
}

func TestT1053_MapIndexWriteThroughSharedBorrowRejected(t *testing.T) {
	errs := checkErrs(t, `
		put(Map[int, int] m) { m[1] = 2; }
	`)
	expectError(t, errs, "cannot mutate element through a shared (read-only) borrow")
}

func TestT1053_MapRemoveThroughSharedBorrowRejected(t *testing.T) {
	errs := checkErrs(t, `
		drop_key(Map[int, int] m) { m.remove(1); }
	`)
	expectError(t, errs, "cannot call mutating method 'remove' through a shared (read-only) borrow")
}

func TestT1053_SetAddThroughSharedBorrowRejected(t *testing.T) {
	errs := checkErrs(t, `
		insert(Set[int] s) { s.add(7); }
	`)
	expectError(t, errs, "cannot call mutating method 'add' through a shared (read-only) borrow")
}

// --- Reject: user types ---

func TestT1053_UserMutMethodThroughSharedBorrowRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Counter { int n; bump(~this) { this.n = this.n + 1; } }
		via(Counter c) { c.bump(); }
	`)
	expectError(t, errs, "cannot call mutating method 'bump' through a shared (read-only) borrow")
}

func TestT1053_UserPlainThisSelfMutationRejected(t *testing.T) {
	// A plain-`this` method that writes `this.field` is now an error — the author
	// must annotate `~this`. This closes the user-type slice of the hole.
	errs := checkErrs(t, `
		type Counter { int n; set_plain(this) { this.n = 5; } }
	`)
	expectError(t, errs, "cannot mutate field 'n' through a shared (read-only) borrow")
}

// --- Allow: owned / mutable-borrow / move receivers ---

func TestT1053_VectorPushOnOwnedLocalOK(t *testing.T) {
	checkOK(t, `
		grow() { v := [1, 2, 3]; v.push(7); }
	`)
}

func TestT1053_VectorPushThroughMutBorrowOK(t *testing.T) {
	checkOK(t, `
		grow(int[]~ v) { v.push(7); }
	`)
}

func TestT1053_VectorPushThroughMoveParamOK(t *testing.T) {
	checkOK(t, `
		sink(int[] move v) { v.push(7); }
	`)
}

func TestT1053_MutThisSelfMutationOK(t *testing.T) {
	checkOK(t, `
		type Counter { int n; bump(~this) { this.n = this.n + 1; } }
	`)
}

// --- Allow: interior-mutable types through a shared borrow (the escape hatch) ---

func TestT1053_ChannelSendThroughSharedBorrowOK(t *testing.T) {
	// Channel is `interior: send takes a shared `this receiver (interior mutability
	// through internal synchronization), so it is callable through a shared borrow —
	// essential for a channel shared across goroutines.
	checkOK(t, `
		emit(Channel[int] ch) { ch.send(7); }
	`)
}

func TestT1053_ChannelCloseThroughSharedBorrowOK(t *testing.T) {
	checkOK(t, `
		shut(Channel[int] ch) { ch.close(); }
	`)
}

func TestT1053_MutexLockThroughSharedBorrowOK(t *testing.T) {
	// Mutex is `interior: lock() takes a shared `this receiver (interior mutability),
	// so it is callable through a shared borrow — essential for a shared Mutex.
	checkOK(t, `
		use_it(Mutex[int] m) { g := m.lock(); g.close(); }
	`)
}

func TestT1053_MutexGuardSetThroughSharedBorrowOK(t *testing.T) {
	// MutexGuard is `interior: its `set borrow` setter keeps a shared `this receiver,
	// so writing the guarded value through a shared borrow of the guard is permitted
	// (the write goes through the held lock). This exercises the interior exemption
	// in checkWriteThroughSharedBorrow.
	checkOK(t, `
		store(MutexGuard[int] g) { g.borrow = 5; }
	`)
}

// --- The `interior flag flows from the stdlib decl through to instance origins ---

// originIsInterior scans the recorded types for an Instance whose origin Named
// has the given name, returning whether that origin is marked `interior. Returns
// (false, false) if no such instance was found.
func originIsInterior(info *Info, name string) (interior bool, found bool) {
	for _, typ := range info.Types {
		inst, ok := typ.(*types.Instance)
		if !ok {
			continue
		}
		named, ok := inst.Origin().(*types.Named)
		if !ok || named.String() != name {
			continue
		}
		return named.IsInterior(), true
	}
	return false, false
}

func TestT1053_ChannelOriginIsInterior(t *testing.T) {
	// The `interior marker on the Channel stdlib decl reaches the concrete
	// instance's origin Named — this is what recvIsInterior reads.
	info := checkOK(t, `
		test() { ch := channel[int](capacity: 1); }
	`)
	interior, found := originIsInterior(info, "Channel")
	if !found {
		t.Fatal("no Channel instance recorded")
	}
	if !interior {
		t.Error("Channel origin should report IsInterior()")
	}
}

func TestT1053_VectorOriginNotInterior(t *testing.T) {
	// A container that is NOT marked `interior must not report interior mutability
	// — this is what makes push/index writes through a shared borrow a hard error.
	info := checkOK(t, `
		test() { v := [1, 2, 3]; }
	`)
	interior, found := originIsInterior(info, "Vector")
	if !found {
		t.Fatal("no Vector instance recorded")
	}
	if interior {
		t.Error("Vector origin should not report IsInterior()")
	}
}

// --- User-defined `interior types: the codified escape hatch ---
//
// `interior is a user-facing annotation (docs/language-design.md §6.2). These
// pin the non-generic-Named path of recvIsInterior / the write check, which the
// generic stdlib primitives (Channel/Mutex, all *Instance) never exercise.

func TestT1053_UserInteriorTypeMutMethodThroughSharedBorrowOK(t *testing.T) {
	// A user type marked `interior may have a mutating method invoked through a
	// shared borrow — recvIsInterior resolves the bare *types.Named receiver
	// (`c`, a non-generic interior type) to IsInterior() == true.
	checkOK(t, `
		type Cell `+"`interior"+` { int n; poke(this) { this.n = 5; } }
		via(Cell c) { c.poke(); }
	`)
}

func TestT1053_UserInteriorTypeFieldStoreThroughSharedBorrowOK(t *testing.T) {
	// A direct field store through a shared borrow of an interior type is the
	// exemption path in checkWriteThroughSharedBorrow (recvPlace is the bare
	// *types.Named `c`).
	checkOK(t, `
		type Cell `+"`interior"+` { int n; }
		via(Cell c) { c.n = 5; }
	`)
}

func TestT1053_UserInteriorTypeSelfMutationPlainThisOK(t *testing.T) {
	// Inside a plain-`this method of an interior type, writing `this.field is
	// allowed — recvIsInterior(this) short-circuits the self-mutation check that
	// otherwise forces `~this on a non-interior type.
	checkOK(t, `
		type Cell `+"`interior"+` { int n; set_plain(this) { this.n = 5; } }
	`)
}

func TestT1053_NonInteriorUserTypeStillRejectedForContrast(t *testing.T) {
	// The same type WITHOUT `interior is rejected — proving the exemption is what
	// makes the interior cases above pass, not some unrelated permissiveness.
	errs := checkErrs(t, `
		type Cell { int n; poke(this) { this.n = 5; } }
		via(Cell c) { c.poke(); }
	`)
	expectError(t, errs, "cannot mutate field 'n' through a shared (read-only) borrow")
}

// --- `interior on enums: recvIsInterior's *types.Enum branch ---

func TestT1053_InteriorEnumMutMethodThroughSharedBorrowOK(t *testing.T) {
	// A `~this method on an `interior enum is callable through a shared borrow —
	// recvIsInterior resolves the bare *types.Enum receiver to IsInterior() == true.
	checkOK(t, `
		enum Flag `+"`interior"+` { On, Off, reset(~this) { this = Flag.Off; } }
		via(Flag f) { f.reset(); }
	`)
}

func TestT1053_GenericInteriorEnumMutMethodThroughSharedBorrowOK(t *testing.T) {
	// A generic `interior enum: the receiver `b is a *types.Instance whose origin
	// is the interior *types.Enum — recvIsInterior resolves through the instance
	// origin (the Enum-origin arm), distinct from the non-generic bare-Enum arm.
	checkOK(t, `
		enum Box[T] `+"`interior"+` { None, Some(T v), reset(~this) { this = Box[T].None; } }
		via(Box[int] b) { b.reset(); }
	`)
}

func TestT1053_NonInteriorEnumMutMethodThroughSharedBorrowRejected(t *testing.T) {
	// Without `interior, the same enum `~this method through a shared borrow is
	// rejected — recvIsInterior returns false for the non-interior *types.Enum.
	errs := checkErrs(t, `
		enum Flag { On, Off, reset(~this) { this = Flag.Off; } }
		via(Flag f) { f.reset(); }
	`)
	expectError(t, errs, "cannot call mutating method 'reset' through a shared (read-only) borrow")
}

// --- Setter receiver defaults (resolveMethodSignature / resolveEnumMethodSignature) ---

// setterRecvRef returns the receiver RefMod of the named type's `[]= index
// setter, resolved from the file scope. Fails the test if absent.
func setterRecvRef(t *testing.T, info *Info, typeName string) types.RefMod {
	t.Helper()
	scope := info.Scopes[findFile(t, info)]
	tn, ok := scope.Lookup(typeName).(*types.TypeName)
	if !ok {
		t.Fatalf("%s is not a type name", typeName)
	}
	var methods []*types.Method
	switch typ := tn.Type().(type) {
	case *types.Named:
		methods = typ.Methods()
	case *types.Enum:
		methods = typ.Methods()
	default:
		t.Fatalf("%s is neither Named nor Enum", typeName)
	}
	for _, m := range methods {
		if m.Name() == "[]=" {
			return m.Sig().Recv().Ref()
		}
	}
	t.Fatalf("no []= setter on %s", typeName)
	return types.RefNone
}

func TestT1053_EnumIndexSetterDefaultsToMutThisReceiver(t *testing.T) {
	// A setter inherently mutates its receiver, so an enum `[]= operator setter
	// with no explicit receiver defaults to a `~this mutable borrow (T1053) — the
	// resolveEnumMethodSignature setter branch. This is what makes an index write
	// through a shared borrow a mutation subject to the shared-borrow check.
	//
	// Note: the `interior arm of that branch (setter keeps RefNone) is currently
	// unreachable — the `interior flag is set after method resolution for enums, so
	// even an `interior enum's setter resolves to RefMut. Tracked as T1345; benign
	// because the call-site check (recvIsInterior) resolves interior correctly.
	info := checkOK(t, `
		enum Cell {
			Empty, Full(int n),
			[](int i) int { return 0; }
			[]=(int i, int v) { this = Cell.Full(v); }
		}
	`)
	if ref := setterRecvRef(t, info, "Cell"); ref != types.RefMut {
		t.Errorf("enum setter should default to ~this (RefMut), got %v", ref)
	}
}

func TestT1053_NamedIndexSetterDefaultsToMutThisReceiver(t *testing.T) {
	// The type analogue: a `[]= setter on a plain (non-interior) type defaults to
	// a `~this receiver — resolveMethodSignature setter branch.
	info := checkOK(t, `
		type Bag { int slot; [](int i) int { return this.slot; } []=(int i, int v) { this.slot = v; } }
	`)
	if ref := setterRecvRef(t, info, "Bag"); ref != types.RefMut {
		t.Errorf("named setter should default to ~this (RefMut), got %v", ref)
	}
}

// --- Meta validation: `interior only applies to types and enums ---

func TestT1053_InteriorMetaRejectedOnFunction(t *testing.T) {
	// `interior is registered for TargetType/TargetEnum only; applying it to a
	// function is a meta-target error (meta.go).
	errs := checkErrs(t, `
		foo() `+"`interior"+` {}
	`)
	expectError(t, errs, "meta `interior cannot be applied to function")
}
