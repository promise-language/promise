package regress9

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1356: mutating a value-type field/property through a non-Ident addressable
// receiver — a nested value-type member (o.inner) or a container element
// (vs[0]). Direct field assign previously panicked genFieldPtr ("value type
// field assignment requires addressable target"); the setter silently no-op'd
// (genSetterCall spilled a loaded copy). Both now take the in-place address of
// the receiver l-value via genValueTypeReceiverAddr.

// Direct field assign through a nested value-type member GEPs into the parent's
// own storage (%o) and stores in place — no panic, no spill temp.
func TestT1356_NestedMemberFieldAssignInPlace(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Vec2 { int x `+"`value"+`; int y `+"`value"+`; }
		type Outer { Vec2 inner `+"`value"+`; int tag `+"`value"+`; }
		caller() {
			o := Outer(inner: Vec2(x: 3, y: 4), tag: 9);
			o.inner.x = 17;
		}
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// GEP into the Outer alloca (%o), then into the embedded Vec2, then store.
	if !strings.Contains(body, "getelementptr %promise_Outer_v") {
		t.Fatalf("expected a GEP into the Outer storage:\n%s", body)
	}
	if !strings.Contains(body, "getelementptr %promise_Vec2_v") {
		t.Fatalf("expected a GEP into the embedded Vec2 field:\n%s", body)
	}
	if !strings.Contains(body, "store i64 17") {
		t.Fatalf("expected an in-place store of 17:\n%s", body)
	}
	// The Vec2 is embedded in %o — there must be no separate Vec2 spill alloca.
	if n := strings.Count(body, "alloca %promise_Vec2_v"); n != 0 {
		t.Fatalf("expected 0 `alloca %%promise_Vec2_v` (no spill temp), got %d:\n%s", n, body)
	}
}

// Property setter through a nested value-type member passes a bitcast of an
// in-place GEP (into %o) to i8* — no second value-struct alloca acting as spill.
func TestT1356_NestedMemberSetterInPlace(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Vec2 { int x `+"`value"+`; int y `+"`value"+`;
			get sum int { return this.x + this.y; }
			set sum(int v) { this.x = v; this.y = 0; } }
		type Outer { Vec2 inner `+"`value"+`; int tag `+"`value"+`; }
		caller() {
			o := Outer(inner: Vec2(x: 3, y: 4), tag: 9);
			o.inner.sum = 17;
		}
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "getelementptr %promise_Outer_v") {
		t.Fatalf("expected a GEP into the Outer storage for the setter receiver:\n%s", body)
	}
	if !strings.Contains(body, "@Vec2.sum$set(i8*") {
		t.Fatalf("expected value-type setter call `@Vec2.sum$set(i8* ...)`:\n%s", body)
	}
	// The receiver is the embedded Vec2 inside %o — no spill alloca.
	if n := strings.Count(body, "alloca %promise_Vec2_v"); n != 0 {
		t.Fatalf("expected 0 `alloca %%promise_Vec2_v` (no spill temp), got %d:\n%s", n, body)
	}
}

// Property setter through a vector element passes a pointer derived from the
// bounds-checked element GEP, not a spill temp.
func TestT1356_VectorElementSetterInPlace(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Vec2 { int x `+"`value"+`; int y `+"`value"+`;
			get sum int { return this.x + this.y; }
			set sum(int v) { this.x = v; this.y = 0; } }
		caller() {
			vs := [Vec2(x: 3, y: 4)];
			vs[0].sum = 17;
		}
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@Vec2.sum$set(i8*") {
		t.Fatalf("expected value-type setter call `@Vec2.sum$set(i8* ...)`:\n%s", body)
	}
	// The element is reached through a bounds-checked index into the vector
	// buffer (genIndexSlotPtr's "nestedpush" bounds check), not spilled to a
	// fresh alloca.
	if !strings.Contains(body, "nestedpush.oob") {
		t.Fatalf("expected a bounds check for the vector element receiver:\n%s", body)
	}
	if n := strings.Count(body, "alloca %promise_Vec2_v"); n != 0 {
		t.Fatalf("expected 0 `alloca %%promise_Vec2_v` (no spill temp), got %d:\n%s", n, body)
	}
}
