package codegen

import (
	"strings"
	"testing"
)

// T0906: a method whose return type is an optional of its own receiver type
// (`OB?`) with body `return this` crashed codegen with
// "panic: insertvalue elem type mismatch, expected { i8*, i8* }, got i8*".
// wrapThisReturnValue bailed because extractNamed does not peel Optional, so the
// bare i8* instance pointer flowed into wrapOptional's {i8*,i8*} insertvalue. The
// fix unwraps the optional's element type, builds the value-struct payload (and
// the T0893 clone), and lets wrapReturnOptional wrap it.

// Heap type: the body must build the { i8*, i8* } value struct, clone it
// (dupHeapValue — the receiver is borrowed `this`, not `~this`), and wrap the
// result into the optional { i1, { i8*, i8* } } — not feed a bare i8* into the
// optional payload (which previously panicked).
func TestT0906OptionalReturnThisHeap(t *testing.T) {
	ir := generateIR(t, `
		type OB { int v; dup() OB? { return this; } }
		main() { d := OB(v: 1); m := d.dup()!; }
	`)
	// Optional return type is the optional-of-value-struct shape.
	assertContains(t, ir, "define { i1, { i8*, i8* } } @OB.dup")
	// The optional wrap (present flag + value-struct payload) is emitted.
	assertContains(t, ir, "insertvalue { i1, { i8*, i8* } }")
	// The T0893 clone fired (independent instance): heap dup copies the instance.
	assertContains(t, ir, "heapdup.copy")
	// Guard against the regression: the value struct, not a bare i8*, is wrapped.
	if strings.Contains(ir, "insertvalue { i1, { i8*, i8* } } undef, i8*") {
		t.Fatalf("optional payload must be the value struct, not a bare i8*:\n%s", ir)
	}
}

// Pure value type: wrapThisReturnValue must load the full value struct from the
// `this` pointer via the optional's element type, then wrap it into the optional.
func TestT0906OptionalReturnThisValueType(t *testing.T) {
	ir := generateIR(t, `
		type VT { int x `+"`value"+`; int y `+"`value"+`; dup() VT? { return this; } }
		main() { d := VT(x: 3, y: 4); m := d.dup()!; }
	`)
	// Value-type optional return wraps the loaded value struct (no panic).
	assertContains(t, ir, "@VT.dup")
	assertContains(t, ir, "insertvalue { i1,")
}

// Enum receiver: `this` is an i8* pointer but the method returns the enum value
// struct ({i32, [N x i8]}). wrapThisReturnValue must load the value via
// enumThisSubject rather than feeding the bare i8* into the optional wrap (which
// previously panicked: "insertvalue elem type mismatch, ... got i8*"). The
// droppable string payload also exercises the T0893-analog deep clone.
func TestT0906OptionalReturnThisEnum(t *testing.T) {
	ir := generateIR(t, `
		enum OE { None, Some(string s), dup() OE? { return this; } }
		main() { d := OE.Some("hi"); m := d.dup()!; }
	`)
	// No bare i8* wrapped into the optional payload (the regression shape).
	if strings.Contains(ir, "insertvalue { i1, %promise_OE_enum } undef, i8*") {
		t.Fatalf("enum optional payload must be the loaded value struct, not a bare i8*:\n%s", ir)
	}
	// The optional wrap over the enum value struct is emitted.
	assertContains(t, ir, "@OE.dup")
}

// Enum with an EXPLICIT `clone` method: the droppable-payload deep clone in
// wrapThisReturnValue must route through cloneEnumValue (call @CE.clone) rather
// than the in-place strdup fallback exercised by TestT0906OptionalReturnThisEnum.
// Covers the cloned-branch of the enum optional return-this path.
func TestT0906OptionalReturnThisEnumCloneMethod(t *testing.T) {
	ir := generateIR(t, `
		enum CE {
			None, Some(string s),
			clone(this) CE { return match this { CE.Some(s) => CE.Some(s.clone()), CE.None => CE.None, }; }
			dup() CE? { return this; }
		}
		main() { d := CE.Some("hi"); m := d.dup()!; }
	`)
	// The clone-method branch fires inside CE.dup: temp alloca + call to CE.clone.
	assertContains(t, ir, "@CE.dup")
	assertContains(t, ir, "enum.clone.tmp")
	assertContains(t, ir, "call %promise_CE_enum @CE.clone")
}
