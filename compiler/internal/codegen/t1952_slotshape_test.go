package codegen

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/types"
)

// T1952: slotShapeDiffers is the predicate that decides whether a first-parent
// crossing may reuse the concrete's own vtable or must get an adapting view
// vtable. It is deliberately substitution-free — AllVirtualMethods() hands back
// the PARENT's method objects with type params unbound, so every arm has to be
// decidable on the raw signatures.
//
// The behavioural coverage lives in tests/regress11/t1952_test.go (IR shape) and
// tests/e2e/T1952_first_parent_view_adapter_test.pr (runtime). This exercises the
// predicate directly, because one arm — a result present on one side and absent
// on the other — is not reachable through a program sema accepts today: an
// explicit `is` requires both results present or both absent, and an override of
// a CONCRETE parent method is not signature-checked at all (T2184). When T2184
// lands, that arm becomes reachable from real code and must already be right.

func t1952Recv() *types.Param {
	return types.NewParam("this", nil, types.RefMut)
}

// sig builds a signature with the given params, result and failability. A nil
// result means "returns nothing" (NewSignature normalises TypVoid to nil, T1634).
func t1952Sig(result types.Type, canError bool, params ...*types.Param) *types.Signature {
	return types.NewSignature(t1952Recv(), params, result, canError)
}

func TestT1952SlotShapeIdenticalSignaturesDoNotDiffer(t *testing.T) {
	a := t1952Sig(types.TypInt, false)
	b := t1952Sig(types.TypInt, false)
	if slotShapeDiffers(a, b) {
		t.Fatal("identical signatures must not require an adapter — every explicit `is` would pay for a view vtable it does not need (T1734's premise)")
	}
}

func TestT1952SlotShapeVoidSignaturesDoNotDiffer(t *testing.T) {
	if slotShapeDiffers(t1952Sig(nil, false), t1952Sig(nil, false)) {
		t.Fatal("two nothing-returning signatures have the same LLVM shape")
	}
}

func TestT1952SlotShapeExtraParamDiffers(t *testing.T) {
	concrete := t1952Sig(nil, false, types.NewParam("n", types.TypInt, types.RefNone),
		types.NewParam("flags", types.TypInt, types.RefNone))
	abstract := t1952Sig(nil, false, types.NewParam("n", types.TypInt, types.RefNone))
	if !slotShapeDiffers(concrete, abstract) {
		t.Fatal("an override with an extra (defaulted) parameter needs an adapter to supply it")
	}
}

func TestT1952SlotShapeFailabilityDiffers(t *testing.T) {
	// The reported bug: close(~this) against close!(~this).
	if !slotShapeDiffers(t1952Sig(nil, false), t1952Sig(nil, true)) {
		t.Fatal("a non-failable override of a failable requirement changes the LLVM return shape")
	}
	// And the reverse pairing, which sema rejects but the predicate must still
	// classify — firstParentSlotShapesMatch walks slots sema never checked.
	if !slotShapeDiffers(t1952Sig(nil, true), t1952Sig(nil, false)) {
		t.Fatal("failable against non-failable is a shape difference in either direction")
	}
}

func TestT1952SlotShapeResultPresenceDiffers(t *testing.T) {
	// Not reachable through an explicit `is` today (SatisfiesAbstract requires
	// both results present or both absent), but firstParentSlotShapesMatch walks
	// every virtual slot, including overrides of CONCRETE parent methods that
	// nothing signature-checks (T2184). Returning false here would hand the
	// parent's call site a function that never writes a result.
	if !slotShapeDiffers(t1952Sig(nil, false), t1952Sig(types.TypInt, false)) {
		t.Fatal("an override returning nothing cannot fill a slot whose call site reads a value")
	}
	if !slotShapeDiffers(t1952Sig(types.TypInt, false), t1952Sig(nil, false)) {
		t.Fatal("an override returning a value cannot fill a slot whose call site reads none")
	}
}

func TestT1952SlotShapeOptionalReturnDiffers(t *testing.T) {
	bare := t1952Sig(types.TypInt, false)
	opt := t1952Sig(types.NewOptional(types.TypInt), false)
	if !slotShapeDiffers(bare, opt) {
		t.Fatal("T satisfying T? needs an adapter that wraps the result as `some`")
	}
	if !slotShapeDiffers(opt, bare) {
		t.Fatal("optional-ness is a shape difference in either direction")
	}
}

func TestT1952SlotShapeIgnoresUnboundTypeParams(t *testing.T) {
	// The reason the predicate is substitution-free. A generic parent's slot is
	// described by `T?` while the concrete instance returns `int?`; comparing the
	// element types would report a difference that does not exist and make every
	// generic `is` pay for a view vtable. Only the top-level optional-ness counts.
	tp := types.NewTypeParam(types.NewTypeName(types.Pos{}, "T", nil), nil, 0)
	generic := t1952Sig(types.NewOptional(tp), false)
	concrete := t1952Sig(types.NewOptional(types.TypInt), false)
	if slotShapeDiffers(concrete, generic) {
		t.Fatal("an unbound type param must not read as a shape difference — see TestT1952GenericParentExactMatchNeedsNoViewVtable")
	}
}

func TestT1952SlotShapeIgnoresCovariantReturn(t *testing.T) {
	// A covariant return (`U` for a structural `T`) is deliberately NOT a shape
	// difference: both lower to {i8*, i8*}, so the slot's ABI matches. Only the
	// returned value's own vtable pointer is at stake, which is T2182.
	u := types.NewNamed(types.NewTypeName(types.Pos{}, "Concrete", nil), nil)
	tt := types.NewNamed(types.NewTypeName(types.Pos{}, "Iface", nil), nil)
	if slotShapeDiffers(t1952Sig(u, false), t1952Sig(tt, false)) {
		t.Fatal("two named returns have the same LLVM shape; widening the predicate here would be T2182's fix, not this one's")
	}
}
