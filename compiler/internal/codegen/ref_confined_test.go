package codegen

import (
	"strings"
	"testing"
)

// T0995: a `confined element type makes Ref[T]'s refcount non-atomic. The drop
// function for a confined Ref must use a plain load/add/store, while a default
// (unmarked) Ref keeps atomicrmw so it can be shared across goroutines.

func TestConfinedRefDropIsNonAtomic(t *testing.T) {
	ir := generateIR(t, `
		type Data `+"`confined"+` { int x; }
		main() {
			r := Ref[Data](Data(x: 1));
		}
	`)
	dropFn := extractFunction(ir, `"Ref[Data].drop"`)
	if dropFn == "" {
		t.Fatalf("expected a Ref[Data].drop function in IR:\n%s", ir)
	}
	if strings.Contains(dropFn, "atomicrmw") {
		t.Errorf("confined Ref[Data].drop must NOT use atomicrmw (non-atomic counter):\n%s", dropFn)
	}
	// The non-atomic path is a plain load/add/store of the i64 refcount.
	if !strings.Contains(dropFn, "load i64") || !strings.Contains(dropFn, "store i64") {
		t.Errorf("confined Ref[Data].drop should load/add/store the counter:\n%s", dropFn)
	}
}

func TestDefaultRefDropIsAtomic(t *testing.T) {
	ir := generateIR(t, `
		type Data { int x; }
		main() {
			r := Ref[Data](Data(x: 1));
		}
	`)
	dropFn := extractFunction(ir, `"Ref[Data].drop"`)
	if dropFn == "" {
		t.Fatalf("expected a Ref[Data].drop function in IR:\n%s", ir)
	}
	if !strings.Contains(dropFn, "atomicrmw") {
		t.Errorf("default Ref[Data].drop must use atomicrmw (atomic counter):\n%s", dropFn)
	}
}

func TestConfinedRefCloneIsNonAtomic(t *testing.T) {
	ir := generateIR(t, `
		type Data `+"`confined"+` { int x; }
		main() {
			r := Ref[Data](Data(x: 1));
			r2 := r.clone();
		}
	`)
	// The clone() increment runs in main; locate the strong-count add. A confined
	// Ref must not emit any atomicrmw for its refcount operations.
	dropFn := extractFunction(ir, `"Ref[Data].drop"`)
	if strings.Contains(dropFn, "atomicrmw") {
		t.Errorf("confined Ref[Data].drop must NOT use atomicrmw:\n%s", dropFn)
	}
}

// The strong-count's twin counter (weak count, manipulated by downgrade/Weak
// drop) must follow the same atomicity decision: confined → plain load/store.
func TestConfinedWeakDropIsNonAtomic(t *testing.T) {
	ir := generateIR(t, `
		type Data `+"`confined"+` { int x; }
		main() {
			r := Ref[Data](Data(x: 1));
			w := r.downgrade();
		}
	`)
	weakDrop := extractFunction(ir, `"Weak[Data].drop"`)
	if weakDrop == "" {
		t.Fatalf("expected a Weak[Data].drop function in IR:\n%s", ir)
	}
	if strings.Contains(weakDrop, "atomicrmw") {
		t.Errorf("confined Weak[Data].drop must NOT use atomicrmw (non-atomic counter):\n%s", weakDrop)
	}
}

func TestDefaultWeakDropIsAtomic(t *testing.T) {
	ir := generateIR(t, `
		type Data { int x; }
		main() {
			r := Ref[Data](Data(x: 1));
			w := r.downgrade();
		}
	`)
	weakDrop := extractFunction(ir, `"Weak[Data].drop"`)
	if weakDrop == "" {
		t.Fatalf("expected a Weak[Data].drop function in IR:\n%s", ir)
	}
	if !strings.Contains(weakDrop, "atomicrmw") {
		t.Errorf("default Weak[Data].drop must use atomicrmw (atomic counter):\n%s", weakDrop)
	}
}

// `confined applies to enums too: IsConfined unwraps *Enum, so a Ref over a
// confined enum gets the non-atomic counter.
func TestConfinedEnumRefIsNonAtomic(t *testing.T) {
	ir := generateIR(t, `
		enum Color `+"`confined"+` { Red, Green, Value(int v) }
		main() {
			r := Ref[Color](Color.Red);
		}
	`)
	dropFn := extractFunction(ir, `"Ref[Color].drop"`)
	if dropFn == "" {
		t.Fatalf("expected a Ref[Color].drop function in IR:\n%s", ir)
	}
	if strings.Contains(dropFn, "atomicrmw") {
		t.Errorf("confined-enum Ref[Color].drop must NOT use atomicrmw:\n%s", dropFn)
	}
}

// IsConfined unwraps Optional: a Ref over a `confined?` optional element still
// gets the non-atomic counter.
func TestConfinedOptionalElementRefIsNonAtomic(t *testing.T) {
	ir := generateIR(t, `
		type Data `+"`confined"+` { int x; }
		main() {
			r := Ref[Data?](Data(x: 1));
		}
	`)
	dropFn := extractFunction(ir, `"Ref[Data?].drop"`)
	if dropFn == "" {
		t.Fatalf("expected a Ref[Data?].drop function in IR:\n%s", ir)
	}
	if strings.Contains(dropFn, "atomicrmw") {
		t.Errorf("confined-optional Ref[Data?].drop must NOT use atomicrmw:\n%s", dropFn)
	}
}
