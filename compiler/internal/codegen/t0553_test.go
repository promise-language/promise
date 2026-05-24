package codegen

import (
	"strings"
	"testing"
)

// T0553: defineModuleTypeMethods did not auto-append field drops after a
// user-written drop() body for non-generic module-defined types, leaking
// heap fields. The fix mirrors defineMethodFunc's auto-append.

// TestT0553_ModuleUserDropAutoAppendsFieldDrops — a module-defined non-generic
// type with a user-written empty drop body and a heap (string) field. The
// emitted module-prefixed drop must call @promise_string_drop for the field.
func TestT0553_ModuleUserDropAutoAppendsFieldDrops(t *testing.T) {
	ir := generateIRWithModule(t, "modlib",
		`
			type _T0553ModBase `+"`"+`public {
			  string s;
			  drop(~this) {}
			}
		`,
		`
			use modlib "./modlib";
			main() {
			  s := "x";
			  b := modlib._T0553ModBase(s: s);
			}
		`,
	)

	body := extractPlainDefinition(ir, "__mod_modlib__T0553ModBase.drop")
	if body == "" {
		t.Fatalf("expected __mod_modlib__T0553ModBase.drop in IR")
	}
	if !strings.Contains(body, "@promise_string_drop") {
		t.Errorf("expected module-defined drop with empty user body to auto-append @promise_string_drop for heap field:\n%s", body)
	}
}

// TestT0553_ModuleUserDropNonEmptyBodyAutoAppends — a module-defined type
// with a *non-empty* user drop body and a heap field. The auto-append must
// still emit @promise_string_drop. Defends against a refactor that only
// fires the field-drop auto-append for empty bodies.
func TestT0553_ModuleUserDropNonEmptyBodyAutoAppends(t *testing.T) {
	ir := generateIRWithModule(t, "modlib",
		`
			type _T0553ModBaseBody `+"`"+`public {
			  string s;
			  int counter;
			  drop(~this) {
			    this.counter = this.counter + 1;
			  }
			}
		`,
		`
			use modlib "./modlib";
			main() {
			  s := "y";
			  b := modlib._T0553ModBaseBody(s: s, counter: 0);
			}
		`,
	)

	body := extractPlainDefinition(ir, "__mod_modlib__T0553ModBaseBody.drop")
	if body == "" {
		t.Fatalf("expected __mod_modlib__T0553ModBaseBody.drop in IR")
	}
	if !strings.Contains(body, "@promise_string_drop") {
		t.Errorf("expected module-defined drop with non-empty user body to auto-append @promise_string_drop for heap field:\n%s", body)
	}
}
