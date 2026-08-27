package codegen

import (
	"strings"
	"testing"
)

// T1730: declaring a `structural interface parent on a pure value type is a
// conformance claim, not inherited state — an interface contributes no fields,
// so the value struct is unchanged and dispatch stays static. These tests pin
// that representational freedom: the same type with and without `is Format`
// must produce the same layout and the same direct call.

const t1730Src = `
	type Dur is Format {
		int nanos ` + "`value" + `;
		format!(Writer ~w) { w.write_string(this.nanos.to_string()); }
	}
	main() {
		d := Dur(nanos: 5);
		Builder b = Builder();
		d.format(b)?!;
	}
`

func TestT1730StructuralParentKeepsFlatValueStruct(t *testing.T) {
	ir := generateIR(t, t1730Src)
	// Fields stay embedded in the value struct — no instance pointer, no
	// heap allocation introduced by the `is` clause.
	assertContains(t, ir, "%promise_Dur_v = type { i8*, i64 }")
	assertNotContains(t, ir, "%promise_Dur_i*")
}

func TestT1730StructuralParentDispatchesStatically(t *testing.T) {
	ir := generateIR(t, t1730Src)
	// The direct call resolves to the concrete body, not through a vtable slot.
	assertContains(t, ir, "@Dur.format(")
	assertNotContains(t, ir, "@promise_vtable_Dur_as_Format")
}

// The `is` clause changes exactly one thing in the IR: the typeinfo records
// Format as a parent, so a boxed value can answer an `is` check. Everything
// else — the value struct and the function bodies — is byte-identical to the
// same type without the clause.
func TestT1730StructuralParentOnlyAddsParentID(t *testing.T) {
	without := generateIR(t, `
		type Dur {
			int nanos `+"`value"+`;
			format!(Writer ~w) { w.write_string(this.nanos.to_string()); }
		}
		main() {
			d := Dur(nanos: 5);
			Builder b = Builder();
			d.format(b)?!;
		}
	`)
	assertContains(t, without, "%promise_Dur_v = type { i8*, i64 }")

	with := generateIR(t, t1730Src)

	// The one difference: the typeinfo grows a parent-ID array (numParents 1),
	// which is what lets an `is` check on a boxed Dur answer true for Format.
	// The trailing `[N x i32]` is present only when the type has parents, so
	// the struct type alone distinguishes the two.
	assertContains(t, without, "@promise_typeinfo_Dur = constant { i8*, i8*, i8*, i32, i32 }")
	assertContains(t, with, "@promise_typeinfo_Dur = constant { i8*, i8*, i8*, i32, i32, [1 x i32] }")

	a, b := extractDefine(with, "Dur.format"), extractDefine(without, "Dur.format")
	if a == "" || b == "" {
		t.Fatalf("Dur.format not found in IR")
	}
	if a != b {
		t.Errorf("`is Format` changed the body of Dur.format")
	}
}

// Boxing the value type into the interface builds a view vtable for the
// (concrete, view) pair. Its slots are filled from the value type's own
// methods — a value type has no virtual dispatch to fall back on, so a
// requirement left without a body would sit here as a null that the box calls
// straight through. That is exactly what markValueType's "does not implement"
// check prevents at the declaration; this pins the shape it protects.
func TestT1730BoxedViewVtableSlotHoldsConcreteBody(t *testing.T) {
	ir := generateIR(t, `
		type Dur is Format {
			int nanos `+"`value"+`;
			format!(Writer ~w) { w.write_string(this.nanos.to_string()); }
		}
		main() {
			d := Dur(nanos: 5);
			Format f = d;
			Builder b = Builder();
			f.format(b)?!;
		}
	`)
	line := globalLine(ir, "@promise_vtable_Dur_as_Format")
	if line == "" {
		t.Fatalf("no view vtable emitted for the boxed value type")
	}
	if !strings.Contains(line, "@Dur.format") {
		t.Errorf("view vtable slot does not point at the concrete body: %s", line)
	}
	if strings.Contains(line, "null") {
		t.Errorf("view vtable has an unfilled slot: %s", line)
	}
	// Taking a box does not un-flatten the value struct — the fields stay
	// embedded and the direct path is unaffected.
	assertContains(t, ir, "%promise_Dur_v = type { i8*, i64 }")
}

// globalLine returns the single IR line defining the named global, or "".
func globalLine(ir, name string) string {
	for _, l := range strings.Split(ir, "\n") {
		if strings.HasPrefix(l, name+" =") {
			return l
		}
	}
	return ""
}

// A value newtype that lists a structural interface alongside its value parent
// is still the layout-preserving newtype of T1527: it gets no value struct of
// its own — it reuses the parent's — and the requirement it supplies is a
// direct call. The interface contributed to neither.
func TestT1730ValueNewtypeWithStructuralParentSharesParentValueStruct(t *testing.T) {
	ir := generateIR(t, `
		type Tagged `+"`structural"+` { get tag int `+"`abstract"+`; }
		type Millis { int raw `+"`value"+`; get scaled int => this.raw * 1000; }
		type Seconds is Millis, Tagged { get tag int => 7; }
		main() {
			Seconds s = Seconds(raw: 3);
			print_line("{s.scaled}{s.tag}");
			Millis m = s;
			print_line("{m.scaled}");
		}
	`)
	assertContains(t, ir, "%promise_Millis_v = type { i8*, i64 }")
	assertNotContains(t, ir, "%promise_Seconds_v")
	assertContains(t, ir, "@Seconds.tag(")
}
