package regress3

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1558: genArrayLit view-coerces every element to the vector's element type,
// not just Optional ones. A value-type element in a `structural[]` literal is a
// wide value struct that does not fit the {i8*, i8*} view slot; before the fix
// it was stored raw → NewStore panicked ("store operands are not compatible"),
// a check/build disagreement. These tests assert the box/vtable coercion reaches
// the store. Runtime + leak-freedom are covered by
// tests/e2e/T1558_array_lit_structural_view_test.pr.

// Value-type element in a structural-view vector literal boxes into the
// Seat→Tagged view before the store (the codegen panic before the fix).
func TestT1558ValueTypeElementBoxesIntoView(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Tagged `+"`"+`structural { get tag int `+"`"+`abstract; }
		type Seat { int row `+"`"+`value; get tag int => this.row * 2; }
		al() { Tagged[] v = [Seat(row: 1), Seat(row: 2)]; sink(v); }
		sink(Tagged[] v) {}
	`)
	fn := codegentest.ExtractFunction(ir, "__user.al")
	if fn == "" {
		t.Fatalf("could not extract @__user.al from IR:\n%s", ir)
	}
	// Each value-type element is boxed into the Seat→Tagged structural view
	// (boxValueTypeForStructuralView). Before the fix genArrayLit stored the
	// wide %promise_Seat_v value straight into the {i8*, i8*} slot, which panics
	// in NewStore inside generateIR — so reaching this assertion at all proves
	// the coercion ran; the vtable confirms it targeted the right view.
	codegentest.AssertContains(t, fn, "@promise_vtable_Seat_as_Tagged")
	// The boxed view stored into each element slot is the {i8*, i8*} struct.
	codegentest.AssertContains(t, fn, "store { i8*, i8* }")
}

// A heap-concrete element gets its view-specific vtable and the box drops via the
// RTTI structural-drop path — no leak of the boxed instance.
func TestT1558HeapElementUsesStructuralDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Tagged `+"`"+`structural { get tag int `+"`"+`abstract; }
		type Booth { int seat; get tag int => this.seat + 100; }
		al() { Tagged[] v = [Booth(seat: 7)]; sink(v); }
		sink(Tagged[] v) {}
	`)
	codegentest.AssertContains(t, ir, "@promise_vtable_Booth_as_Tagged")
	// The vector element drop routes the boxed view through the structural drop.
	codegentest.AssertContains(t, ir, "@__promise_structural_drop")
}

// A pre-boxed view ident moved into the literal has its source drop flag cleared
// so the vector is the sole owner (no double-free at scope exit).
func TestT1558PreboxedViewClearsSourceDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Tagged `+"`"+`structural { get tag int `+"`"+`abstract; }
		type Seat { int row `+"`"+`value; get tag int => this.row * 2; }
		al() { Tagged t = Seat(row: 1); Tagged[] v = [t]; sink(v); }
		sink(Tagged[] v) {}
	`)
	fn := codegentest.ExtractFunction(ir, "__user.al")
	if fn == "" {
		t.Fatalf("could not extract @__user.al from IR:\n%s", ir)
	}
	// The vector element drop walks structural views (element-drop loop enabled),
	// which is the branch that clears the source ident's drop flag.
	codegentest.AssertContains(t, ir, "@__promise_structural_drop")
}
