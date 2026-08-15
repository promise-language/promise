package codegen

import (
	"strings"
	"testing"
)

// T1542: a field whose type is a value type imported from another module (here
// std's Duration) must use that type's wide value struct in the containing
// layout. Before the fix, computeAllTypeLayouts ran over the main file before
// compileModules, so the imported value type had no entry in c.layouts and
// instanceFieldLLVMType fell back to the generic {i8*, i8*} user-value struct —
// while the constructor produced %promise_Duration_v, panicking in codegen with
// "insertvalue elem type mismatch".

func TestT1542ImportedValueTypeAsValueField(t *testing.T) {
	ir := generateIR(t, `
		type W {
			Duration d `+"`"+`value;
		}
		main() {
			w := W(d: Duration.from_secs(2));
		}
	`)
	assertContains(t, ir, "%promise_Duration_v = type { i8*, i64 }")
	assertContains(t, ir, "%promise_W_v = type { i8*, %promise_Duration_v }")
}

func TestT1542ImportedValueTypeAsHeapField(t *testing.T) {
	ir := generateIR(t, `
		type H {
			Duration d;
			int x;
		}
		main() {
			h := H(d: Duration.from_secs(3), x: 7);
		}
	`)
	assertContains(t, ir, "%promise_H_i = type { %promise_H_m*, %promise_Duration_v, i64 }")
}

func TestT1542ImportedValueTypeAsOptionalField(t *testing.T) {
	ir := generateIR(t, `
		type Opt {
			Duration? d;
		}
		main() {
			o := Opt(d: Duration.from_secs(4));
		}
	`)
	assertContains(t, ir, "%promise_Opt_i = type { %promise_Opt_m*, { i1, %promise_Duration_v } }")
}

func TestT1542ImportedValueTypeAsArrayField(t *testing.T) {
	ir := generateIR(t, `
		type Arr {
			Duration[3] d;
		}
		main() {
			a := Arr(d: [Duration.from_secs(1), Duration.from_secs(2), Duration.from_secs(3)]);
		}
	`)
	assertContains(t, ir, "%promise_Arr_i = type { %promise_Arr_m*, [3 x %promise_Duration_v] }")
}

func TestT1542ImportedValueTypeAsTupleField(t *testing.T) {
	ir := generateIR(t, `
		type Tup {
			(Duration, int) d;
		}
		main() {
			tp := Tup(d: (Duration.from_secs(5), 9));
		}
	`)
	assertContains(t, ir, "%promise_Tup_i = type { %promise_Tup_m*, { %promise_Duration_v, i64 } }")
}

func TestT1542GenericUserTypeInstantiatedWithImportedValueType(t *testing.T) {
	ir := generateIR(t, `
		type Pair[T] {
			T a `+"`"+`value;
			T b `+"`"+`value;
		}
		main() {
			p := Pair[Duration](a: Duration.from_secs(6), b: Duration.from_secs(7));
		}
	`)
	assertContains(t, ir, `%"promise_Pair[Duration]_v" = type { i8*, %promise_Duration_v, %promise_Duration_v }`)
}

func TestT1542NestedValueTypeHoldingImportedValueType(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			Duration d `+"`"+`value;
		}
		type Outer {
			Inner inner `+"`"+`value;
		}
		main() {
			o := Outer(inner: Inner(d: Duration.from_secs(8)));
		}
	`)
	assertContains(t, ir, "%promise_Inner_v = type { i8*, %promise_Duration_v }")
	assertContains(t, ir, "%promise_Outer_v = type { i8*, %promise_Inner_v }")
}

// Regression guard: the pre-existing same-module and mono value-field paths
// (T0565) still go through the topological walker and produce their layouts.
func TestT1542SameModuleValueFieldsUnaffected(t *testing.T) {
	ir := generateIR(t, `
		type WithCoord {
			Coord pos;
			Point[int] pt;
		}
		type Coord {
			int x `+"`"+`value;
			int y `+"`"+`value;
		}
		main() {
			w := WithCoord(pos: Coord(x: 1, y: 2), pt: Point[int](x: 3, y: 4));
		}
	`)
	assertContains(t, ir, "%promise_Coord_v = type { i8*, i64, i64 }")
	assertContains(t, ir, `%"promise_Point[int]_v" = type { i8*, i64, i64 }`)
	assertContains(t, ir, `%promise_WithCoord_i = type { %promise_WithCoord_m*, %promise_Coord_v, %"promise_Point[int]_v" }`)
}

// A generic value type whose mono layout was already built by the mono-enum
// pre-pass (computeMonoEnumLayoutsOnly runs before the unified layout pass and
// lays out value-type variant payloads) is no longer in `pending` when a user
// type's field walk reaches it. That drives the field walker's non-pending
// branch for *types.Instance, where ensureValueTypeLayout must be a no-op and
// leave the single shared layout intact. (This shape also passed before the
// fix — the guard is that the on-demand build stays idempotent.)
func TestT1542MonoValueFieldAlreadyLaidOutByEnumPrepass(t *testing.T) {
	ir := generateIR(t, `
		enum Maybe[T] {
			None, Some(Point[T] p),
		}
		type Holder {
			Point[int] pos;
		}
		main() {
			m := Maybe[int].Some(Point[int](x: 1, y: 2));
			h := Holder(pos: Point[int](x: 3, y: 4));
		}
	`)
	assertContains(t, ir, `%"promise_Point[int]_v" = type { i8*, i64, i64 }`)
	assertContains(t, ir, `%promise_Holder_i = type { %promise_Holder_m*, %"promise_Point[int]_v" }`)
	// One layout, not two — the on-demand build must not re-emit the typedef.
	if n := strings.Count(ir, `%"promise_Point[int]_v" = type`); n != 1 {
		t.Fatalf("expected exactly 1 Point[int] value typedef, got %d", n)
	}
}

// The same gap for a value type imported from a non-std module: the importing
// file's layouts are computed before that module is compiled.
func TestT1542ModuleValueTypeAsField(t *testing.T) {
	ir := generateIRWithModule(t, "vmod",
		`
		type Meters `+"`"+`public {
			int amount `+"`"+`value;
		}
		`,
		`
		use vmod "./vmod";
		type Holder {
			vmod.Meters distance `+"`"+`value;
		}
		main() {
			h := Holder(distance: vmod.Meters(amount: 7));
		}
		`,
	)
	assertContains(t, ir, "%promise_Meters_v = type { i8*, i64 }")
	assertContains(t, ir, "%promise_Holder_v = type { i8*, %promise_Meters_v }")
}

// A module value type that itself nests a value type: the on-demand build must
// recurse into the inner field first, or the inner slot gets the generic
// {i8*, i8*} user-value struct and that wrong layout is cached permanently.
func TestT1542ModuleNestedValueTypeAsField(t *testing.T) {
	ir := generateIRWithModule(t, "vmod",
		`
		type Meters `+"`"+`public {
			int amount `+"`"+`value;
		}
		type Wrapped `+"`"+`public {
			Meters inner `+"`"+`value;
		}
		`,
		`
		use vmod "./vmod";
		type Holder {
			vmod.Wrapped w;
			int tag;
		}
		main() {
			h := Holder(w: vmod.Wrapped(inner: vmod.Meters(amount: 3)), tag: 1);
		}
		`,
	)
	assertContains(t, ir, "%promise_Meters_v = type { i8*, i64 }")
	assertContains(t, ir, "%promise_Wrapped_v = type { i8*, %promise_Meters_v }")
	assertContains(t, ir, "%promise_Holder_i = type { %promise_Holder_m*, %promise_Wrapped_v, i64 }")
}

// T0962/T0965: a local signature is the only mention of the module's value type,
// so resolveType's on-demand fallback (not the field walk) builds the layout.
// It must recurse into nested value-type fields too — otherwise `Wrapped`'s
// inner slot is cached as the generic {i8*, i8*} struct and the module-side
// constructor mismatches it.
func TestT1542ModuleNestedValueTypeInLocalSignatureOnly(t *testing.T) {
	ir := generateIRWithModule(t, "vmod",
		`
		type Meters `+"`"+`public {
			int amount `+"`"+`value;
		}
		type Wrapped `+"`"+`public {
			Meters inner `+"`"+`value;
		}
		make_wrapped(int amount) Wrapped `+"`"+`public {
			return Wrapped(inner: Meters(amount: amount));
		}
		`,
		`
		use vmod "./vmod";
		unwrap_locally(vmod.Wrapped w) int {
			return w.inner.amount;
		}
		main() {
			int n = unwrap_locally(vmod.make_wrapped(5));
		}
		`,
	)
	assertContains(t, ir, "%promise_Meters_v = type { i8*, i64 }")
	assertContains(t, ir, "%promise_Wrapped_v = type { i8*, %promise_Meters_v }")
	assertNotContains(t, ir, "%promise_Wrapped_v = type { i8*, { i8*, i8* } }")
}

// A generic value type imported from a module and instantiated in the importing
// file — the mono value layout must be built before the container's. The field
// is left un-annotated: “ `value “ on a generic value-type instance is
// rejected by sema today (T1553), though the layout is the same either way.
func TestT1542ModuleGenericValueTypeAsField(t *testing.T) {
	ir := generateIRWithModule(t, "vmod",
		`
		type Boxed[T] `+"`"+`public {
			T item `+"`"+`value;
		}
		`,
		`
		use vmod "./vmod";
		type Holder {
			vmod.Boxed[int] b;
		}
		main() {
			h := Holder(b: vmod.Boxed[int](item: 4));
		}
		`,
	)
	assertContains(t, ir, `%"promise_Boxed[int]_v" = type { i8*, i64 }`)
	assertContains(t, ir, `%promise_Holder_i = type { %promise_Holder_m*, %"promise_Boxed[int]_v" }`)
}

// A module-owned type with a field of a value type imported from *another*
// module (std's Duration): here the field walk runs during compileModule's own
// layout pass, not the main file's. std is compiled first, so this already
// worked — the test pins that module compilation order keeps holding.
func TestT1542ModuleTypeWithStdValueField(t *testing.T) {
	ir := generateIRWithModule(t, "vmod",
		`
		type Span `+"`"+`public {
			Duration d `+"`"+`value;
		}
		make_span(int secs) Span `+"`"+`public {
			return Span(d: Duration.from_secs(secs));
		}
		`,
		`
		use vmod "./vmod";
		main() {
			s := vmod.make_span(2);
		}
		`,
	)
	assertContains(t, ir, "%promise_Duration_v = type { i8*, i64 }")
	assertContains(t, ir, "%promise_Span_v = type { i8*, %promise_Duration_v }")
}

// AllFields() includes inherited fields, so a derived type's layout depends on
// the imported value type reached only through its parent.
func TestT1542InheritedImportedValueField(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			Duration d;
		}
		type Derived is Base {
			int x;
		}
		main() {
			d := Derived(d: Duration.from_secs(9), x: 1);
		}
	`)
	assertContains(t, ir, "%promise_Base_i = type { %promise_Base_m*, %promise_Duration_v }")
	assertContains(t, ir, "%promise_Derived_i = type { %promise_Derived_m*, %promise_Duration_v, i64 }")
}
