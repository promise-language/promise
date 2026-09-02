package cast1

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1884: Structural interface downcast tests.

func TestStructuralDowncastPrimitiveForce(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Format f = 3;
			int x = f as! int;
		}
	`)
	// Should use vtable comparison (icmp eq), not promise_type_is.
	codegentest.AssertContains(t, ir, "icmp eq")
	codegentest.AssertContains(t, ir, "@promise_vtable_int_as_Format")
}

func TestStructuralDowncastStringForce(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Format f = "hello";
			string x = f as! string;
		}
	`)
	// Should produce a promise_string_new call (dupString) for the extracted string.
	codegentest.AssertContains(t, ir, "promise_string_new")
	codegentest.AssertContains(t, ir, "@promise_vtable_string_as_Format")
}

func TestStructuralDowncastOptional(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Format f = 3;
			int? x = f as int;
		}
	`)
	// The optional should wrap the scalar result.
	codegentest.AssertContains(t, ir, "icmp eq")
}

func TestStructuralDowncastMismatchOptional(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Format f = 3;
			string? x = f as string;
		}
	`)
	// Two different vtable comparisons (one for int boxing, one for string cast check).
	codegentest.AssertContains(t, ir, "icmp eq")
}

func TestStructuralDowncastSliceTarget(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Cloneable c = [1, 2, 3];
			int[] w = c as! int[];
		}
	`)
	// SliceTypeRef target should resolve correctly (no panic on *ast.SliceTypeRef).
	codegentest.AssertContains(t, ir, "icmp eq")
}

func TestStructuralDowncastUserTypeForce(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo {
			int x;
			== (this, Foo other) bool { return this.x == other.x; }
		}
		main() {
			Equal e = Foo(x: 42);
			Foo g = e as! Foo;
		}
	`)
	// Should reconstruct the value struct from the instance pointer.
	codegentest.AssertContains(t, ir, "icmp eq")
	codegentest.AssertContains(t, ir, "@promise_vtable_Foo_as_Equal")
}

func TestStructuralDowncastValueTypeForce(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pt {
			int x `+"`value"+`;
			int y `+"`value"+`;
			format!(Writer ~w) {
				w.write_string("pt")?^;
			}
		}
		main() {
			Format f = Pt(x: 1, y: 2);
			Pt p = f as! Pt;
		}
	`)
	// Value type unbox: load from box, restore vtable.
	codegentest.AssertContains(t, ir, "icmp eq")
	codegentest.AssertContains(t, ir, "@promise_vtable_Pt_as_Format")
}

func TestStructuralDowncastValueTypeOptional(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pt {
			int x `+"`value"+`;
			int y `+"`value"+`;
			format!(Writer ~w) {
				w.write_string("pt")?^;
			}
		}
		main() {
			Format f = Pt(x: 1, y: 2);
			Pt? p = f as Pt;
		}
	`)
	// Optional path for value type.
	codegentest.AssertContains(t, ir, "icmp eq")
}

func TestStructuralDowncastCharForce(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Format f = 'A';
			char x = f as! char;
		}
	`)
	// Char is a primitive scalar — same path as int.
	codegentest.AssertContains(t, ir, "icmp eq")
	codegentest.AssertContains(t, ir, "@promise_vtable_char_as_Format")
}

func TestStructuralDowncastBoolOptional(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Format f = true;
			bool? x = f as bool;
		}
	`)
	codegentest.AssertContains(t, ir, "icmp eq")
}

func TestStructuralDowncastSliceOptional(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Cloneable c = [1, 2, 3];
			int[]? w = c as int[];
		}
	`)
	// Optional path for opaque container (vector/slice).
	codegentest.AssertContains(t, ir, "icmp eq")
}

func TestStructuralDowncastUserTypeOptional(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Bar {
			int v;
			== (this, Bar other) bool { return this.v == other.v; }
		}
		main() {
			Equal e = Bar(v: 5);
			Bar? b = e as Bar;
		}
	`)
	// Optional path for heap user type.
	codegentest.AssertContains(t, ir, "icmp eq")
	codegentest.AssertContains(t, ir, "@promise_vtable_Bar_as_Equal")
}
