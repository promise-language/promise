package regress11

import (
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1885 — a `native` requirement reached through a boxed structural view.
//
// Defect 1: a `native` method has no LLVM body (it is open-coded at each AST call
// site), so the view vtable's function lookup missed and the slot was emitted as a
// null pointer — `Cloneable c = "x"; c.clone()` jumped to address 0.
//
// Defect 2: an opaque container/handle coerced to a structural view was boxed as the
// RAW payload pointer with no RTTI header, so __promise_structural_drop read the
// payload's own field 0 (a vector's `len|bit63`, an Arc's strong count) as a typeinfo
// pointer and dereferenced it.

// vtableSlots returns the initializer element list of the named vtable global.
func vtableSlots(t *testing.T, ir, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^@"?` + regexp.QuoteMeta(name) + `"? = constant \[\d+ x i8\*\] (.*)$`)
	m := re.FindStringSubmatch(ir)
	if m == nil {
		t.Fatalf("vtable global @%s not found in IR", name)
	}
	return m[1]
}

const t1885StringSrc = `
	main() { Cloneable c = "x"; Cloneable d = c.clone(); }
`

func TestT1885_StringCloneViewSlotIsNotNull(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1885StringSrc)
	slots := vtableSlots(t, ir, "promise_vtable_string_as_Cloneable")
	if strings.Contains(slots, "i8* null") {
		t.Fatalf("string→Cloneable vtable still has a null slot: %s", slots)
	}
	if !strings.Contains(slots, "string.clone$view_adapt_as_Cloneable") {
		t.Fatalf("expected the clone slot to point at the view adapter, got: %s", slots)
	}
	// The adapter forwards to the synthesized concrete-signature shim for the
	// `native` clone, which has no body of its own.
	if !strings.Contains(ir, `define i8* @string.clone$native(i8* %this)`) {
		t.Fatalf("expected a synthesized @string.clone$native shim")
	}
}

const t1885VectorSrc = `
	main() { int[] v = [1, 2, 3]; Cloneable c = v.clone(); Cloneable d = c.clone(); }
`

func TestT1885_VectorCloneViewSlotIsNotNull(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1885VectorSrc)
	slots := vtableSlots(t, ir, "promise_vtable_Vector[int]_as_Cloneable")
	if strings.Contains(slots, "i8* null") {
		t.Fatalf("Vector[int]→Cloneable vtable still has a null slot: %s", slots)
	}
	if !strings.Contains(ir, `define i8* @"Vector[int].clone$native"(i8* %this)`) {
		t.Fatalf("expected a synthesized @\"Vector[int].clone$native\" shim")
	}
	// The shim must be per-instance: keying it on the unbound Vector[T] would give
	// Vector[int] and Vector[string] one shared (wrong) element-clone loop.
	if strings.Contains(ir, `@"Vector[T].clone$native"`) {
		t.Fatalf("shim was emitted under the unbound generic name Vector[T]")
	}
}

const t1885RefSrc = `
	type Node { int v; }
	main() {
	  Ref[Node] r = Ref[Node](Node(v: 1));
	  Weak[Node] w = r.downgrade();
	  Cloneable a = r.clone();
	  Cloneable b = w.clone();
	}
`

func TestT1885_RefAndWeakCloneViewSlotsAreNotNull(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1885RefSrc)
	for _, vt := range []string{
		"promise_vtable_Ref[Node]_as_Cloneable",
		"promise_vtable_Weak[Node]_as_Cloneable",
	} {
		if slots := vtableSlots(t, ir, vt); strings.Contains(slots, "i8* null") {
			t.Fatalf("@%s still has a null slot: %s", vt, slots)
		}
	}
	for _, shim := range []string{`@"Ref[Node].clone$native"`, `@"Weak[Node].clone$native"`} {
		if !strings.Contains(ir, "define i8* "+shim+"(i8* %this)") {
			t.Fatalf("expected a synthesized %s shim", shim)
		}
	}
}

func TestT1885_OpaqueContainerBoxCarriesTypeInfoHeader(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1885VectorSrc)
	// The box header exists, with a real drop_fn and clone_fn (not the raw-pointer
	// shape this replaces, which had no header at all).
	if !strings.Contains(ir, `@"promise_typeinfo_containerbox$Vector[int]" = constant`) {
		t.Fatalf("expected a per-concrete box typeinfo for Vector[int]")
	}
	if !strings.Contains(ir, `define void @"__promise_container_box_drop$Vector[int]"(i8* %box)`) {
		t.Fatalf("expected a box drop function for Vector[int]")
	}
	if !strings.Contains(ir, `define i8* @"__promise_container_box_clone$Vector[int]"(i8* %box)`) {
		t.Fatalf("expected a box clone function for Vector[int]")
	}
	// The header must actually be stored into field 0 of the box at the coercion site.
	if !strings.Contains(ir, `store i8* bitcast ({ i8*, i8*, i8*, i32, i32 }* @"promise_typeinfo_containerbox$Vector[int]" to i8*)`) {
		t.Fatalf("box typeinfo header is never stored into the box")
	}
	// The drop must free the payload and then the box itself.
	drop := codegentest.FindDefinedFunc(ir, `@"__promise_container_box_drop$Vector[int]"(`)
	if !strings.Contains(drop, "@Vector.drop") || !strings.Contains(drop, "@pal_free") {
		t.Fatalf("box drop must drop the payload and free the box, got:\n%s", drop)
	}
}

// A view vtable slot that nothing can fill must become a panicking stub, not a null
// that jumps to address 0. `Vector[T].len` is a `native GETTER — outside this fix
// (T1881) — so it is the live example.
const t1885UnfillableSrc = `
	type Counted ` + "`" + `structural { clone() Self ` + "`" + `abstract; get len int ` + "`" + `abstract; }
	main() { int[] v = [1, 2, 3]; Counted c = v; }
`

func TestT1885_UnfillableViewSlotPanicsInsteadOfNull(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1885UnfillableSrc)
	slots := vtableSlots(t, ir, "promise_vtable_Vector[int]_as_Counted")
	if strings.Contains(slots, "i8* null") {
		t.Fatalf("unfillable slot was left null: %s", slots)
	}
	if !strings.Contains(slots, "Vector[int].len$view_stub_as_Counted") {
		t.Fatalf("expected a panicking stub in the unfillable slot, got: %s", slots)
	}
	stub := codegentest.FindDefinedFunc(ir, `@"Vector[int].len$view_stub_as_Counted"(`)
	if !strings.Contains(stub, "@promise_panic") {
		t.Fatalf("stub must panic, got:\n%s", stub)
	}
	if !strings.Contains(ir, "no implementation of Counted.len for Vector[int]") {
		t.Fatalf("stub panic message must name the view, method and concrete type")
	}
}

// --- emitUnimplementedViewStub: the signature shapes a stub has to reproduce -----
//
// The stub stands in for a slot nothing could fill, so its LLVM signature must be
// the INTERFACE method's, not a fixed one — a mismatched return type is undefined
// behaviour at the indirect call exactly as the null it replaces was. The three
// shapes reachable from source are void, plain-value and failable.

// A void-returning slot is the dangerous one the item singles out: a null there did
// not fault reliably, it sometimes just exited 0, so a truncated `promise test` batch
// read as a pass. `Channel[T].close` is `native` and is the one native non-clone
// method std opts into structural satisfaction, so it is the live example.
const t1885VoidStubSrc = `
	type Closer2 ` + "`" + `structural { close() ` + "`" + `abstract; }
	main() { channel[int] ch = channel[int](2); Closer2 c = ch; }
`

func TestT1885_VoidReturningUnfillableSlotGetsAVoidStub(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1885VoidStubSrc)
	slots := vtableSlots(t, ir, "promise_vtable_Channel[int]_as_Closer2")
	if strings.Contains(slots, "i8* null") {
		t.Fatalf("void slot was left null — the silent-exit-0 shape: %s", slots)
	}
	stub := codegentest.FindDefinedFunc(ir, `@"Channel[int].close$view_stub_as_Closer2"(`)
	if !strings.Contains(stub, `define void @"Channel[int].close$view_stub_as_Closer2"(i8* %this)`) {
		t.Fatalf("stub must have the interface method's void signature, got:\n%s", stub)
	}
	if !strings.Contains(stub, "@promise_panic") || !strings.Contains(stub, "ret void") {
		t.Fatalf("void stub must panic and then return void, got:\n%s", stub)
	}
	if !strings.Contains(ir, "no implementation of Closer2.close for Channel[int]") {
		t.Fatalf("stub panic message must name the view, method and concrete type")
	}
}

// A failable requirement satisfied by a non-failable native concrete: the stub must
// return the failable TUPLE, not the bare value. Both a void and a value result are
// checked because computeResultType shapes them differently ({i1, i8*} vs
// {i1, i64, i8*}), and either one emitted at the other's width corrupts the caller's
// unpack.
func TestT1885_FailableUnfillableSlotStubReturnsTheFailableTuple(t *testing.T) {
	voidIR := codegentest.GenerateIR(t, `
		type CloserF `+"`"+`structural { close!() `+"`"+`abstract; }
		main() { channel[int] ch = channel[int](2); CloserF c = ch; }
	`)
	stub := codegentest.FindDefinedFunc(voidIR, `@"Channel[int].close$view_stub_as_CloserF"(`)
	if !strings.Contains(stub, `define { i1, i8* } @"Channel[int].close$view_stub_as_CloserF"(i8* %this)`) {
		t.Fatalf("failable void stub must return {i1, i8*}, got:\n%s", stub)
	}

	valueIR := codegentest.GenerateIR(t, `
		type LenF `+"`"+`structural { get len! int `+"`"+`abstract; }
		main() { int[] v = [1]; LenF p = v; }
	`)
	stub = codegentest.FindDefinedFunc(valueIR, `@"Vector[int].len$view_stub_as_LenF"(`)
	if !strings.Contains(stub, `define { i1, i64, i8* } @"Vector[int].len$view_stub_as_LenF"(i8* %this)`) {
		t.Fatalf("failable int-getter stub must return {i1, i64, i8*}, got:\n%s", stub)
	}
	if !strings.Contains(stub, "@promise_panic") {
		t.Fatalf("failable stub must still panic, got:\n%s", stub)
	}
}

// --- per-instance keying ---------------------------------------------------------

// Boxing happens inside a MONOMORPHIZED body here, so fromType is the unbound
// Vector[T] until the active substitution is applied. The box's drop_fn and clone_fn
// are generated FROM the payload type, so sharing one key across instantiations would
// give Vector[string] the int element walk: its elements would never be freed.
const t1885GenericBoxSrc = `
	type ShowClone ` + "`" + `structural { clone() Self ` + "`" + `abstract; to_string() string ` + "`" + `abstract; }
	box_clone[T](Vector[T] v) string {
	  ShowClone c = v.clone();
	  ShowClone d = c.clone();
	  return d.to_string();
	}
	main() {
	  int[] a = [1, 2];
	  string[] b = ["p"];
	  print_line(box_clone(a));
	  print_line(box_clone(b));
	}
`

func TestT1885_GenericBodyBoxesPerInstanceNotPerGeneric(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1885GenericBoxSrc)
	for _, name := range []string{
		`@"promise_typeinfo_containerbox$Vector[int]" = constant`,
		`@"promise_typeinfo_containerbox$Vector[string]" = constant`,
		`define i8* @"Vector[int].clone$native"(i8* %this)`,
		`define i8* @"Vector[string].clone$native"(i8* %this)`,
	} {
		if !strings.Contains(ir, name) {
			t.Fatalf("expected a per-instance %q emitted from the generic body", name)
		}
	}
	// Nothing may be keyed on the unbound type parameter — that is the shared-and-wrong
	// pair the substitution exists to prevent.
	for _, unbound := range []string{
		`promise_typeinfo_containerbox$Vector[T]`,
		`Vector[T].clone$native`,
		`promise_vtable_Vector[T]_as_ShowClone`,
	} {
		if strings.Contains(ir, unbound) {
			t.Fatalf("%q was emitted under the unbound generic name", unbound)
		}
	}
	// The two box drops must differ in exactly the way that matters: the string one
	// frees its elements, the int one has no element loop to run.
	stringDrop := codegentest.FindDefinedFunc(ir, `@"__promise_container_box_drop$Vector[string]"(`)
	if !strings.Contains(stringDrop, "@promise_string_drop") {
		t.Fatalf("Vector[string] box drop must free its string elements, got:\n%s", stringDrop)
	}
	intDrop := codegentest.FindDefinedFunc(ir, `@"__promise_container_box_drop$Vector[int]"(`)
	if strings.Contains(intDrop, "@promise_string_drop") {
		t.Fatalf("Vector[int] box drop ran the string element walk:\n%s", intDrop)
	}
}

// The `native clone shim is cached on the concrete, not on the (concrete, view) pair:
// two views over one concrete share a single definition, and each gets its own
// adapter around it. A second definition under the same LLVM name is invalid IR.
const t1885TwoViewsSrc = `
	type OnlyClone ` + "`" + `structural { clone() Self ` + "`" + `abstract; }
	type CloneAndShow ` + "`" + `structural { clone() Self ` + "`" + `abstract; to_string() string ` + "`" + `abstract; }
	main() {
	  string s = "x";
	  OnlyClone a = s;
	  CloneAndShow b = s;
	}
`

func TestT1885_NativeCloneShimIsEmittedOncePerConcrete(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1885TwoViewsSrc)
	if n := strings.Count(ir, "define i8* @string.clone$native(i8* %this)"); n != 1 {
		t.Fatalf("expected exactly one @string.clone$native definition, got %d", n)
	}
	for _, view := range []string{"OnlyClone", "CloneAndShow"} {
		slots := vtableSlots(t, ir, "promise_vtable_string_as_"+view)
		if strings.Contains(slots, "i8* null") {
			t.Fatalf("string→%s vtable has a null slot: %s", view, slots)
		}
		if !strings.Contains(slots, "string.clone$view_adapt_as_"+view) {
			t.Fatalf("expected a per-view adapter for %s, got: %s", view, slots)
		}
	}
}

// The box's clone_fn is what __promise_structural_clone dispatches to, so it must
// produce an independently-owned payload: an aliasing copy would be double-freed by
// the two boxes' drops. Asserted on the IR because the runtime routes that reach it
// (a box stored as a vector element) leak the ORIGINAL box's payload for unrelated
// reasons — T1909, pre-existing and reproducible with a plain heap string.
func TestT1885_BoxCloneDeepCopiesThePayload(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1885GenericBoxSrc)

	stringClone := codegentest.FindDefinedFunc(ir, `@"__promise_container_box_clone$Vector[string]"(`)
	// A fresh box (its own pal_alloc), a fresh buffer (the vector dup's pal_alloc),
	// and a fresh string per element — nothing shared with the source box.
	if n := strings.Count(stringClone, "@pal_alloc"); n < 2 {
		t.Fatalf("box clone must allocate both a new box and a new buffer, got %d pal_alloc:\n%s", n, stringClone)
	}
	if !strings.Contains(stringClone, "@promise_string_new") {
		t.Fatalf("Vector[string] box clone must deep-copy its elements, got:\n%s", stringClone)
	}

	// The int instance has no droppable element, so it must NOT run an element loop —
	// the same assertion from the other side, and the one a shared key would break.
	intClone := codegentest.FindDefinedFunc(ir, `@"__promise_container_box_clone$Vector[int]"(`)
	if strings.Contains(intClone, "@promise_string_new") {
		t.Fatalf("Vector[int] box clone ran the string element dup:\n%s", intClone)
	}

	// The clone_fn must actually be published in the typeinfo header, or
	// __promise_structural_clone falls back to the shallow copy this replaces.
	if !strings.Contains(ir, `@"promise_typeinfo_containerbox$Vector[string]" = constant`) ||
		!strings.Contains(ir, `@"__promise_container_box_clone$Vector[string]" to i8*`) {
		t.Fatalf("box clone_fn is never referenced from the box typeinfo")
	}
}

// --- viewMemberTag: a bare method name is not unique within an interface ----------

// A getter/setter pair is T1905's case; a unary operator is the other one. The
// collision itself is unreachable (T1911), so this pins the naming and the dispatch
// for a lone unary requirement — an adapter named without the marker would collide
// the moment T1911 is fixed.
const t1885UnaryAdapterSrc = `
	type Flip ` + "`" + `structural { !() bool? ` + "`" + `abstract; }
	type Flag { bool b; !() bool => !this.b; }
	main() { Flag f = Flag(b: true); Flip v = f; if r := !v { print_line("{r}"); } }
`

func TestT1885_UnaryOperatorAdapterCarriesTheArityMarker(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1885UnaryAdapterSrc)
	slots := vtableSlots(t, ir, "promise_vtable_Flag_as_Flip")
	if !strings.Contains(slots, `Flag.!$unary$view_adapt_as_Flip`) {
		t.Fatalf("unary adapter must carry the $unary arity marker, got: %s", slots)
	}
	adapter := codegentest.FindDefinedFunc(ir, `@"Flag.!$unary$view_adapt_as_Flip"(`)
	// The interface wraps the concrete's bare bool into an optional, so the adapter
	// returns {i1, i1} and forwards to the concrete unary method.
	if !strings.Contains(adapter, `define { i1, i1 } @"Flag.!$unary$view_adapt_as_Flip"(i8* %this)`) {
		t.Fatalf("unary adapter must have the interface's optional-returning signature, got:\n%s", adapter)
	}
	if !strings.Contains(adapter, `@"Flag.!$unary"(`) {
		t.Fatalf("unary adapter must forward to the concrete unary method, got:\n%s", adapter)
	}
}

// --- the shared dup dispatch must still fall through for everything else ----------

// emitVariantFieldDup's five hand-written branches (string / Vector / Channel / Ref /
// Weak) were collapsed into one emitNativeDupValue call. That call reports false for a
// type it has no dup semantics for, and the walk must then continue to the ordinary
// heap-value dup — a fallthrough that stops working turns a nested user-type field
// into an ALIAS, which the two owners then double-free.
const t1885MixedFieldsSrc = `
	type Inner { string s; int n; }
	type Outer {
	  Inner i;
	  string tag;
	  int[] nums;
	  channel[int] ch;
	  Ref[int] r;
	}
	main() {
	  channel[int] c = channel[int](1);
	  Outer[] v = [Outer(i: Inner(s: "deep", n: 1), tag: "t", nums: [1], ch: move c, r: Ref[int](2))];
	  Outer[] copy = v.clone();
	  print_line(copy[0].tag);
	}
`

func TestT1885_FieldDupWalkHandlesNativeAndNonNativeFields(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1885MixedFieldsSrc)
	dup := codegentest.FindDefinedFunc(ir, "@Outer.__clone(")
	if dup == "" {
		t.Fatalf("no @Outer.__clone in the IR")
	}
	// One field of each kind the collapsed dispatch has to tell apart, each with the
	// block label its own emitter produces. A branch lost in the rewrite shows up here
	// as a missing label, and at runtime as an aliased field two owners double-free.
	for what, label := range map[string]string{
		"nested user-type field (the fallthrough)": "heapdup.copy",
		"string field":  "strdup.copy",
		"vector field":  "vecdup.copy",
		"channel field": "chdup.inc",
		"Ref field":     "arcdup.inc",
	} {
		if !strings.Contains(dup, label) {
			t.Fatalf("%s did not dup — no %q block in @Outer.__clone:\n%s", what, label, dup)
		}
	}
	// The two refcount bumps are increments of the live handle, not copies of it —
	// a memcpy there would give two owners one allocation to free twice.
	if n := strings.Count(dup, "atomicrmw add i64*"); n != 2 {
		t.Fatalf("expected exactly two refcount increments (channel + Ref), got %d:\n%s", n, dup)
	}
	// The nested Inner is deep-copied through its own allocation, and its string with it.
	if n := strings.Count(dup, "@promise_string_new"); n != 2 {
		t.Fatalf("expected two string dups (Outer.tag and the nested Inner.s), got %d", n)
	}
}
