package codegen

import (
	"fmt"
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1880 added verifyNoNullVtableSlots as the build-time guard that a slot for a
// default inherited from a structural interface is never emitted null — a null
// there turns a virtual call into a jump to address 0, which is the bug itself.
//
// No program in the tree reaches its detection logic any more (that is the
// point: after the fix no such slot is ever emitted null), so the guard is
// exercised here directly. A guard that cannot fire protects nothing, and the
// silent-skip branches are exactly where it would stop firing by accident.
//
// The behaviour of the fix is pinned in
// compiler/internal/codegen/tests/regress11/t1880_test.go (IR shape) and in
// tests/e2e/structural_explicit_is_default_test.pr (runtime).

// vtableWithSlots builds a vtable global whose slots are null wherever nullAt
// says so and a distinct non-null bitcast elsewhere, mirroring what
// emitVtableGlobal produces.
func vtableWithSlots(m *ir.Module, name string, nullAt ...bool) *ir.Global {
	entries := make([]constant.Constant, len(nullAt))
	for i, isNull := range nullAt {
		if isNull {
			entries[i] = constant.NewNull(irtypes.I8Ptr)
			continue
		}
		fn := m.NewFunc(name+".slot", irtypes.Void)
		entries[i] = constant.NewBitCast(fn, irtypes.I8Ptr)
	}
	arrayType := irtypes.NewArray(uint64(len(entries)), irtypes.I8Ptr)
	g := m.NewGlobalDef(name, constant.NewArray(arrayType, entries...))
	g.Immutable = true
	return g
}

// namedMethod builds a bare *types.Method usable as a slot record's method — the
// verifier only ever reads its name, for the diagnostic.
func namedMethod(name string) *types.Method {
	sig := types.NewSignature(nil, nil, nil, false)
	return types.NewMethod(types.Pos{}, name, sig, types.PlaceInstance, false, false)
}

// recoverPanic runs fn and returns the panic value rendered as a string, or ""
// if fn returned normally.
func recoverPanic(fn func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	fn()
	return ""
}

// A structural-default slot still null when every body has been emitted is a
// codegen bug and must abort the build, naming the type and the method so the
// reader is not left with a segfault at run time.
func TestVerifyNoNullVtableSlotsPanicsOnStructuralDefault(t *testing.T) {
	m := ir.NewModule()
	c := &Compiler{}
	g := vtableWithSlots(m, "promise_vtable_Sink", false, true)
	c.recordNullVtableSlots(g, []nullVtableSlot{{
		slot: 1, typeName: "Sink", method: namedMethod("write_string"), structuralDefault: true,
	}})

	msg := recoverPanic(c.verifyNoNullVtableSlots)
	if msg == "" {
		t.Fatal("expected verifyNoNullVtableSlots to panic on a null structural-default slot")
	}
	for _, want := range []string{"slot 1", "promise_vtable_Sink", "Sink.write_string", "T1880"} {
		if !strings.Contains(msg, want) {
			t.Errorf("panic message missing %q, got: %s", want, msg)
		}
	}
}

// The two other kinds of null slot codegen emits today are deliberately out of
// scope: an interface's own vtable (nothing dispatches through it) and a
// main-file type whose module parent is not declared yet (T1893). Both are
// recorded with structuralDefault=false and must not abort the build.
func TestVerifyNoNullVtableSlotsIgnoresNonStructuralDefault(t *testing.T) {
	m := ir.NewModule()
	c := &Compiler{}
	g := vtableWithSlots(m, "promise_vtable_Writer", true, true)
	c.recordNullVtableSlots(g, []nullVtableSlot{
		{slot: 0, typeName: "Writer", method: namedMethod("write"), structuralDefault: false},
		{slot: 1, typeName: "Writer", method: namedMethod("write_line"), structuralDefault: false},
	})

	if msg := recoverPanic(c.verifyNoNullVtableSlots); msg != "" {
		t.Fatalf("expected no panic for non-structural-default nulls, got: %s", msg)
	}
}

// A mono vtable is emitted twice: the first pass leaves slots null for functions
// that are not declared yet, and the second patches the global's Init. The
// verifier must judge the FINAL contents — re-reading Init rather than the
// initializer captured at record time is the whole reason nullVtableSlot keeps
// the global. Recording the initializer instead would fail the build on every
// module-owned generic type.
func TestVerifyNoNullVtableSlotsRereadsPatchedInitializer(t *testing.T) {
	m := ir.NewModule()
	c := &Compiler{}
	g := vtableWithSlots(m, "promise_vtable_Box__int", true)
	c.recordNullVtableSlots(g, []nullVtableSlot{{
		slot: 0, typeName: "Box__int", method: namedMethod("put_two"), structuralDefault: true,
	}})

	// Second emission fills the slot, exactly as emitMonoVtableGlobals does.
	fn := m.NewFunc("Box__int.put_two", irtypes.Void)
	arrayType := irtypes.NewArray(1, irtypes.I8Ptr)
	g.Init = constant.NewArray(arrayType, constant.NewBitCast(fn, irtypes.I8Ptr))

	if msg := recoverPanic(c.verifyNoNullVtableSlots); msg != "" {
		t.Fatalf("expected no panic once the second emission patched the slot, got: %s", msg)
	}
}

// recordNullVtableSlots copies each record by value and stamps the global onto
// the copy. A record that kept a pointer into the caller's slice, or that was
// appended before the global was attached, would leave global==nil and make the
// verifier nil-dereference instead of reporting the bug.
func TestRecordNullVtableSlotsStampsTheGlobalOnEveryRecord(t *testing.T) {
	m := ir.NewModule()
	c := &Compiler{}
	first := vtableWithSlots(m, "promise_vtable_A", true)
	second := vtableWithSlots(m, "promise_vtable_B", true)
	c.recordNullVtableSlots(first, []nullVtableSlot{{slot: 0, typeName: "A", method: namedMethod("m")}})
	c.recordNullVtableSlots(second, []nullVtableSlot{{slot: 0, typeName: "B", method: namedMethod("m")}})

	if len(c.nullVtableSlots) != 2 {
		t.Fatalf("expected 2 recorded slots, got %d", len(c.nullVtableSlots))
	}
	if c.nullVtableSlots[0].global != first || c.nullVtableSlots[1].global != second {
		t.Errorf("each record must carry the global it was emitted into, got %v and %v",
			c.nullVtableSlots[0].global, c.nullVtableSlots[1].global)
	}
}

// A record whose slot index is past the end of the (possibly re-emitted) array
// is skipped rather than panicking with an index-out-of-range, so a shrinking
// re-emission reports nothing instead of crashing the compiler with an
// unrelated error.
func TestVerifyNoNullVtableSlotsSkipsOutOfRangeSlot(t *testing.T) {
	m := ir.NewModule()
	c := &Compiler{}
	g := vtableWithSlots(m, "promise_vtable_Shrunk", true, true)
	c.recordNullVtableSlots(g, []nullVtableSlot{{
		slot: 1, typeName: "Shrunk", method: namedMethod("gone"), structuralDefault: true,
	}})
	arrayType := irtypes.NewArray(1, irtypes.I8Ptr)
	g.Init = constant.NewArray(arrayType, constant.NewNull(irtypes.I8Ptr))

	if msg := recoverPanic(c.verifyNoNullVtableSlots); msg != "" {
		t.Fatalf("expected out-of-range slot to be skipped, got: %s", msg)
	}
}
