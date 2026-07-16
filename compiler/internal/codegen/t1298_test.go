package codegen

import (
	"strings"
	"testing"
)

// T1298: a concrete type satisfying a `structural interface widens to
// Optional-of-that-interface (`Sink?`) in every assignment position. Codegen
// must view-box the concrete value into the interface view BEFORE the optional
// wrap; otherwise the raw value struct is insertvalue'd into the {i8*, i8*}
// optional payload → IR type mismatch (the pre-fix codegen panic). These tests
// assert the box-then-wrap ordering in the emitted IR. Runtime correctness and
// leak-freedom are covered by tests/e2e/optional_structural_widen_test.pr.

// var-decl: `Sink? a = Counter(...)` boxes the value type into the
// Counter→Sink view ({i8*, i8*}) and wraps THAT into the optional payload.
func TestT1298VarDeclValueBoxesBeforeOptionalWrap(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
		type Counter { int base `+"`"+`value; emit(this, int x) int { return this.base + x; } }
		vd() { Sink? a = Counter(base: 5); sink(a); }
		sink(Sink? s) {}
	`)
	fn := extractFunction(ir, "__user.vd")
	if fn == "" {
		t.Fatalf("could not extract @__user.vd from IR:\n%s", ir)
	}
	// The value is boxed into the Counter→Sink structural view before the wrap.
	assertContains(t, fn, "@promise_vtable_Counter_as_Sink")
	// The optional payload wrapped is the {i8*, i8*} view, not the raw value
	// struct — i.e. box-then-wrap, not the pre-fix raw-value insertvalue.
	assertContains(t, fn, "insertvalue { i1, { i8*, i8* } }")
	if strings.Contains(fn, "%promise_Counter_v %") && optionalInsertsRawValue(fn) {
		t.Fatalf("value struct must not be insertvalue'd directly into the optional payload:\n%s", fn)
	}
}

// constructor field: `OptHolder(s: Heavy(...))` where the field is `Sink?` and
// Heavy is a HEAP concrete type. The box must carry a real RTTI drop so the
// inner Heavy instance (and its string) is freed via __promise_structural_drop.
func TestT1298CtorFieldHeapBoxesWithStructuralDrop(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
		type Heavy { string tag; emit(this, int x) int { return x; } }
		type OptHolder { Sink? s; }
		cf() { OptHolder h = OptHolder(s: Heavy(tag: "hi")); sink(h.s); }
		sink(Sink? s) {}
	`)
	// The Heavy→Sink view vtable is emitted and used for the box.
	assertContains(t, ir, "@promise_vtable_Heavy_as_Sink")
	// The heap box drops through the RTTI structural-drop path so the inner
	// Heavy instance + string are freed (no leak).
	assertContains(t, ir, "@__promise_structural_drop")
	cf := extractFunction(ir, "__user.cf")
	if cf == "" {
		t.Fatalf("could not extract @__user.cf from IR:\n%s", ir)
	}
	assertContains(t, cf, "@promise_vtable_Heavy_as_Sink")
	assertContains(t, cf, "insertvalue { i1, { i8*, i8* } }")
}

// std-map index-assign: `m[k] = Counter(...)` where the map value type is a
// structural interface. The map `[]=` setter takes `V` (Sink); genMethodIndexAssign
// passes the value arg with no coercion, so the caller (genAssignStmt) must
// view-box the concrete into the Sink view BEFORE the setter call — otherwise the
// raw value struct is passed to `[]=` and read back through a bogus vtable (the
// pre-fix segfault). Assert the box view reaches the setter, not the raw value.
func TestT1298MapIndexAssignBoxesBeforeSetterCall(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
		type Counter { int base `+"`"+`value; emit(this, int x) int { return this.base + x; } }
		mv() { map[string, Sink] m = {:}; m["a"] = Counter(base: 5); }
	`)
	fn := extractFunction(ir, "__user.mv")
	if fn == "" {
		t.Fatalf("could not extract @__user.mv from IR:\n%s", ir)
	}
	// The concrete is boxed into the Counter→Sink structural view.
	assertContains(t, fn, "@promise_vtable_Counter_as_Sink")
	// The `[]=` setter is called with the {i8*, i8*} view as its value arg — the
	// raw %promise_Counter_v value struct must NOT be an argument to the setter.
	for _, line := range strings.Split(fn, "\n") {
		if strings.Contains(line, `.[]="(`) && strings.Contains(line, "%promise_Counter_v ") {
			t.Fatalf("raw Counter value struct passed to map []= setter (must be boxed first):\n%s", line)
		}
	}
}

// optionalInsertsRawValue reports whether the function body inserts a raw
// %promise_*_v value struct as the payload (field 1) of an optional { i1, ... }
// struct — the pre-fix bug shape. box-then-wrap never produces this.
func optionalInsertsRawValue(fn string) bool {
	for _, line := range strings.Split(fn, "\n") {
		if strings.Contains(line, "insertvalue { i1,") &&
			strings.Contains(line, "%promise_") && strings.Contains(line, "_v ") {
			return true
		}
	}
	return false
}
