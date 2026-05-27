package codegen

import (
	"strings"
	"testing"
)

// T0725: `this is Variant` inside an enum method/getter previously emitted
// malformed IR because genThisExpr returns the receiver as an i8* pointer while
// the is-check codegen assumed a by-value enum aggregate. The fix loads the enum
// value from the i8* `this` (mirroring genMatchExpr) before extracting the tag.

// Mixed-payload enum: a method using `this is Variant` must load the enum value
// (bitcast i8* -> %enum*, load) before extractvalue — never extractvalue on i8*.
func TestT0725_ThisIsVariantMethodMixedPayload(t *testing.T) {
	ir := generateIR(t, `
		enum E {
			Undefined,
			Bool(bool value),
			check_undef(this) bool { return this is Undefined; }
		}
		main() { e := E.Undefined; assert(e.check_undef(), "x"); }
	`)
	body := extractFunction(ir, "E.check_undef")
	if body == "" {
		t.Fatalf("expected E.check_undef in IR:\n%s", ir)
	}
	if strings.Contains(body, "extractvalue i8*") {
		t.Errorf("malformed: extractvalue on i8* receiver still present:\n%s", body)
	}
	if !strings.Contains(body, "bitcast i8*") {
		t.Errorf("expected bitcast of i8* `this` to enum pointer:\n%s", body)
	}
	if !strings.Contains(body, "load %promise_E_enum") {
		t.Errorf("expected load of enum value from `this`:\n%s", body)
	}
}

// Getter form: `get x bool { return this is Variant; }` on a mixed-payload enum.
func TestT0725_ThisIsVariantGetter(t *testing.T) {
	ir := generateIR(t, `
		enum E {
			Undefined,
			Bool(bool value),
			get is_undef bool { return this is Undefined; }
		}
		main() { e := E.Undefined; assert(e.is_undef, "x"); }
	`)
	body := extractFunction(ir, "E.is_undef")
	if body == "" {
		t.Fatalf("expected E.is_undef in IR:\n%s", ir)
	}
	if strings.Contains(body, "extractvalue i8*") {
		t.Errorf("malformed: extractvalue on i8* receiver still present:\n%s", body)
	}
	if !strings.Contains(body, "bitcast i8*") || !strings.Contains(body, "load %promise_E_enum") {
		t.Errorf("expected bitcast+load of enum value from `this`:\n%s", body)
	}
}

// Fieldless enum: the value IS the tag (i32). `this is Variant` must load the i32
// from the i8* `this` and compare — never `icmp ... i8* ... 0`.
func TestT0725_ThisIsVariantFieldlessEnum(t *testing.T) {
	ir := generateIR(t, `
		enum F {
			A,
			B,
			get is_a bool { return this is A; }
		}
		main() { f := F.A; assert(f.is_a, "x"); }
	`)
	body := extractFunction(ir, "F.is_a")
	if body == "" {
		t.Fatalf("expected F.is_a in IR:\n%s", ir)
	}
	if strings.Contains(body, "icmp eq i8*") {
		t.Errorf("malformed: icmp on i8* receiver still present:\n%s", body)
	}
	if !strings.Contains(body, "bitcast i8*") || !strings.Contains(body, "load i32") {
		t.Errorf("expected bitcast i8*->i32* + load i32 of the tag:\n%s", body)
	}
}

// Destructure-bind form: `if this is Variant(v) { ... }` inside an enum method.
// Previously panicked in codegen (storing i8* into the enum alloca). The loaded
// enum value must be stored into the variant alloca for field extraction.
func TestT0725_IfThisIsVariantBind(t *testing.T) {
	ir := generateIR(t, `
		enum E {
			Null,
			Bool(bool value),
			unwrap(this) bool {
				if this is Bool(v) { return v; }
				return false;
			}
		}
		main() { e := E.Bool(value: true); assert(e.unwrap(), "x"); }
	`)
	body := extractFunction(ir, "E.unwrap")
	if body == "" {
		t.Fatalf("expected E.unwrap in IR:\n%s", ir)
	}
	if strings.Contains(body, "extractvalue i8*") {
		t.Errorf("malformed: extractvalue on i8* receiver still present:\n%s", body)
	}
	if !strings.Contains(body, "bitcast i8*") || !strings.Contains(body, "load %promise_E_enum") {
		t.Errorf("expected bitcast+load of enum value from `this`:\n%s", body)
	}
}
