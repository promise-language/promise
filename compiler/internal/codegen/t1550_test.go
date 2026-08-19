package codegen

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/types"
)

// T1550: a `structural type whose fields are all `value is a pure VALUE TYPE, not an
// interface view. Its LLVM representation is the flat value struct (%promise_T_v), so
// nothing crossing to it may be boxed into a {i8*, i8*} fat pointer — codegen used to
// select the view branch on IsStructural() alone and panic with "store operands are not
// compatible: src={ i8*, i8* }; dst=%promise_Metric_v*" (or, on the parameter path,
// silently hand the callee a pointer where an i64 field was expected).
//
// isStructuralView(n) — n.IsStructural() && !n.IsValueType() — is now the predicate
// every view-construction site selects on. Crossing to such a type is a plain copy of
// the shared value struct (T1527 makes the newtype share its parent's layout verbatim).

// A value newtype of a `structural value parent, exercised across every crossing
// codegen has a distinct path for.
const t1550NewtypeSrc = `
	type Metric ` + "`structural" + ` {
		int raw ` + "`value" + `;
		get raw_doubled int => this.raw * 2;
	}
	type Latency is Metric {
		get tag int => 7;
	}
	show(Metric m) int => m.raw_doubled;
	make() Metric { return Latency(raw: 6); }
	main() {
		l := Latency(raw: 6);
		Metric m = l;
		show(l);
		make();
		m.raw_doubled;
		l.tag;
	}
`

func TestT1550NewtypeUpcastToStructuralValueParentIsPlainCopy(t *testing.T) {
	ir := generateIR(t, t1550NewtypeSrc)
	// The parent-typed local is the shared flat value struct, never a fat pointer.
	assertContains(t, ir, "%promise_Metric_v = type { i8*, i64 }")
	assertContains(t, ir, "alloca %promise_Metric_v")
	assertNotContains(t, ir, "%promise_Latency_v = type")
	// No view was constructed for the crossing: no view vtable, no view adapter.
	assertNotContainsMatch(t, ir, `promise_vtable_Latency_as_`)
	assertNotContainsMatch(t, ir, `promise_vtable_Metric_as_`)
	assertNotContainsMatch(t, ir, `Latency\.\w+\$view_adapt_as_`)
}

func TestT1550NewtypeAsStructuralValueParamPassesValueStruct(t *testing.T) {
	ir := generateIR(t, t1550NewtypeSrc)
	// The callee takes the value struct by value — not {i8*, i8*}. This is the
	// silent-corruption variant: a boxed pointer read back as the i64 field.
	assertContainsMatch(t, ir, `define\s+i64\s+@__user\.show\(%promise_Metric_v `)
	assertNotContainsMatch(t, ir, `define\s+i64\s+@__user\.show\(\{ i8\*, i8\* \}`)
}

func TestT1550NewtypeReturnedAsStructuralValueParentNotHeapBoxed(t *testing.T) {
	ir := generateIR(t, t1550NewtypeSrc)
	// coerceReturnToView's escape path (boxValueTypeForStructuralViewHeap) must not
	// fire: the return is the flat struct, built inline with no allocation.
	assertContainsMatch(t, ir, `define\s+%promise_Metric_v\s+@__user\.make\(\)`)
	assertContains(t, ir, "insertvalue %promise_Metric_v")
	assertNotContainsMatch(t, ir, `promise_vtable_Latency_as_`)
}

func TestT1550NewtypeInOptionalAndVectorOfParentNotBoxed(t *testing.T) {
	ir := generateIR(t, `
type Metric `+"`structural"+` {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
type Latency is Metric {
  get tag int => 7;
}
main() {
  Metric? o = Latency(raw: 6);
  Metric[] v = [Latency(raw: 6)];
  if p := o { p.raw_doubled; }
  v[0].raw_doubled;
}
`)
	assertContains(t, ir, "%promise_Metric_v")
	assertNotContainsMatch(t, ir, `promise_vtable_Latency_as_`)
	assertNotContainsMatch(t, ir, `promise_vtable_Metric_as_`)
}

// A concrete `structural value type used on its own (no newtype involved) is equally
// flat — pins the shape the fix must not regress.
func TestT1550ConcreteStructuralValueTypeIsFlat(t *testing.T) {
	ir := generateIR(t, `
type Metric `+"`structural"+` {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
show(Metric m) int => m.raw_doubled;
main() {
  Metric m = Metric(raw: 6);
  show(m);
  m.raw_doubled;
}
`)
	assertContainsMatch(t, ir, `define\s+i64\s+@__user\.show\(%promise_Metric_v `)
	assertNotContainsMatch(t, ir, `promise_vtable_Metric_as_`)
}

// Regression guard for T1284: a value type crossing into an abstract-only structural
// interface (which is NOT itself a value type) must STILL be heap-boxed into a
// {vtable, box} fat pointer. isStructuralView must not over-reject that shape.
func TestT1550ValueTypeIntoAbstractStructuralInterfaceStillBoxes(t *testing.T) {
	ir := generateIR(t, `
type Tagged `+"`structural"+` {
  get tag int `+"`abstract"+`;
}
type Seat {
  int row `+"`value"+`;
  get tag int => this.row * 2;
}
main() {
  Tagged t = Seat(row: 2);
  print_line(t.tag.to_string());
}
`)
	assertContains(t, ir, "@promise_vtable_Seat_as_Tagged")
	assertContains(t, ir, "call i8* @pal_alloc")
}

// A `structural VALUE type can still structurally satisfy a REAL abstract-only
// interface, so it reaches the T1284 box path exactly like any other value type —
// and its typeinfo therefore needs a clone_fn. maybeSynthesizeCloneFn skipped every
// IsStructural() origin, which for this shape is the concrete type itself, so the
// clone_fn stayed null: __promise_structural_clone fell back to a shallow alias and
// the clone and the original both freed the same box (the test process died at scope
// exit with no result reported). Runtime coverage:
// tests/value_types/structural_value_type_test.pr.
func TestT1550StructuralValueTypeBoxedBehindInterfaceGetsCloneFn(t *testing.T) {
	ir := generateIR(t, `
type Doubler `+"`structural"+` {
  get raw_doubled int `+"`abstract"+`;
}
type Metric `+"`structural"+` {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
main() {
  Doubler[] v = [];
  v.push(Metric(raw: 1));
  w := v.clone();
  print_line(w[0].raw_doubled.to_string());
}
`)
	// It really is boxed behind the interface (so the clone_fn is load-bearing)…
	assertContains(t, ir, "@promise_vtable_Metric_as_Doubler")
	// …and the concrete typeinfo the box carries has a non-null clone_fn (field 2).
	assertContainsMatch(t, ir,
		`@promise_typeinfo_Metric = constant [^\n]*@__promise_flat_box_clone_`)
}

// Control for the test above: the same shape without `structural on the value type
// always had its clone_fn — the fix must not have changed this path.
func TestT1550PlainValueTypeBoxedBehindInterfaceKeepsCloneFn(t *testing.T) {
	ir := generateIR(t, `
type Doubler `+"`structural"+` {
  get raw_doubled int `+"`abstract"+`;
}
type Plain {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
main() {
  Doubler[] v = [];
  v.push(Plain(raw: 1));
  w := v.clone();
  print_line(w[0].raw_doubled.to_string());
}
`)
	assertContainsMatch(t, ir,
		`@promise_typeinfo_Plain = constant [^\n]*@__promise_flat_box_clone_`)
}

// A genuine structural INTERFACE still gets no clone_fn: it is never the concrete
// type inside a box, so synthesizing one for it would be dead weight. Pins that the
// fix widened the branch by exactly the value-type case and no further.
func TestT1550StructuralInterfaceStillGetsNoCloneFn(t *testing.T) {
	ir := generateIR(t, `
type Doubler `+"`structural"+` {
  get raw_doubled int `+"`abstract"+`;
}
type Plain {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
main() {
  Doubler d = Plain(raw: 1);
  print_line(d.raw_doubled.to_string());
}
`)
	assertNotContainsMatch(t, ir,
		`@promise_typeinfo_Doubler = constant [^\n]*@__promise_flat_box_clone_`)
}

// The two halves of the fix meeting: a value NEWTYPE of a `structural value parent,
// boxed behind a REAL abstract-only interface. The target is a view (so it boxes) while
// the source is a newtype sharing its parent's struct (so the box is sized and cloned
// off the shared layout). Both had to be decided independently — keying either one on
// the `structural annotation alone gets this shape wrong in a different direction.
func TestT1550NewtypeBoxedBehindInterfaceCarriesChildTypeinfo(t *testing.T) {
	ir := generateIR(t, `
type Doubler `+"`structural"+` {
  get raw_doubled int `+"`abstract"+`;
}
type Metric `+"`structural"+` {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
type Latency is Metric {
  get tag int => 7;
}
main() {
  Doubler[] v = [];
  v.push(Latency(raw: 3));
  w := v.clone();
  print_line(w[0].raw_doubled.to_string());
}
`)
	// The box is built for the CHILD, and its one slot resolves to the parent's body
	// (a value child adds methods, it never overrides them — T1527).
	assertContains(t, ir,
		"@promise_vtable_Latency_as_Doubler = constant [1 x i8*] [i8* bitcast (i64 (i8*)* @Metric.raw_doubled to i8*)]")
	// The child's own typeinfo travels in the box and needs its own clone_fn, or
	// __promise_structural_clone aliases the box and both copies free it.
	assertContainsMatch(t, ir,
		`@promise_typeinfo_Latency = constant [^\n]*@__promise_flat_box_clone_`)
}

// The assignment STATEMENT path (genAssignStmt) is separate from the typed-declaration
// path both other tests exercise, and has its own structural-target gate.
func TestT1550NewtypeAssignedToParentTypedVariableIsPlainCopy(t *testing.T) {
	ir := generateIR(t, `
type Metric `+"`structural"+` {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
type Latency is Metric {
  get tag int => 7;
}
main() {
  Metric m = Metric(raw: 1);
  l := Latency(raw: 6);
  m = l;
  m = Latency(raw: 7);
  print_line(m.raw_doubled.to_string());
}
`)
	assertNotContainsMatch(t, ir, `promise_vtable_Latency_as_`)
	assertNotContainsMatch(t, ir, `promise_vtable_Metric_as_`)
	// Both assignments are stores of the shared flat struct into the same slot.
	assertContainsMatch(t, ir, `store %promise_Metric_v [^\n]*%m`)
}

// A NON-value structural parent (instance fields) keeps its view representation —
// the fix is scoped to value types only.
func TestT1550NonValueStructuralParentStillUsesView(t *testing.T) {
	ir := generateIR(t, `
type Metric `+"`structural"+` {
  int raw;
  get raw_doubled int => this.raw * 2;
}
type Latency is Metric {
  get tag int => 7;
}
main() {
  l := Latency(raw: 6);
  Metric m = l;
  print_line(m.raw_doubled.to_string());
}
`)
	// The heap structural parent lowers to the {i8*, i8*} value struct as before.
	assertContains(t, ir, "%promise_Metric_v = type { i8*, %promise_Metric_i* }")
	assertContains(t, ir, "%promise_Latency_i* ")
}

// A `~` (mutable borrow) param typed as the value parent, given a newtype child arg:
// the caller's alloca must be passed straight through. The child shares the parent's
// LLVM struct (T1527), so the differing static types used to select the coercion
// branch of genCallArgsWithMutRef — store into a fresh temp, pass the temp — and the
// callee's writes went to the temp and were discarded on return. Silent corruption:
// `check` said OK and the program just lost the mutation.
func TestT1550NewtypeThroughValueParentMutRefPassesCallerAlloca(t *testing.T) {
	ir := generateIR(t, `
type Ticket {
  int n `+"`value"+`;
  get doubled int => this.n * 2;
}
type Seat is Ticket {
  get tag int => 7;
}
bump(Ticket ~t) { t.n = 42; }
main() {
  s := Seat(n: 1);
  bump(s);
  print_line(s.n.to_string());
}
`)
	// Exactly one %promise_Ticket_v alloca in the goroutine body — the caller's `s`.
	// A second one would be the discarded coercion temp.
	if got := strings.Count(ir, "alloca %promise_Ticket_v"); got != 1 {
		t.Errorf("expected exactly 1 %%promise_Ticket_v alloca (the caller's local), got %d\n%s", got, ir)
	}
	// And it is that alloca that reaches the call, not a copy of it.
	assertContainsMatch(t, ir, `call void @__user\.bump\(%promise_Ticket_v\* %s`)
}

// The same shape with a `structural value parent — the T1550 report's variant.
func TestT1550NewtypeThroughStructuralValueParentMutRefPassesCallerAlloca(t *testing.T) {
	ir := generateIR(t, `
type Metric `+"`structural"+` {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
type Latency is Metric {
  get tag int => 7;
}
bump(Metric ~m) { m.raw = 21; }
main() {
  l := Latency(raw: 1);
  bump(l);
  print_line(l.raw.to_string());
}
`)
	if got := strings.Count(ir, "alloca %promise_Metric_v"); got != 1 {
		t.Errorf("expected exactly 1 %%promise_Metric_v alloca (the caller's local), got %d\n%s", got, ir)
	}
	assertContainsMatch(t, ir, `call void @__user\.bump\(%promise_Metric_v\* %l`)
}

// Monomorphization skipped method declaration AND definition for every `structural
// origin, on the assumption that a structural type's methods are synthesized per
// implementor. A `structural type that is itself a value type has no implementors,
// so its own methods simply went missing: `check` passed and codegen panicked with
// "codegen: undeclared getter Slot[int].first". Field-only instantiations worked,
// which is what kept the non-generic cases above from catching it.
func TestT1550GenericStructuralValueTypeDeclaresItsOwnMonoMethods(t *testing.T) {
	ir := generateIR(t, `
type Slot[T] `+"`structural"+` {
  T a `+"`value"+`;
  T b `+"`value"+`;
  get first T => this.a;
  swapped(this) Slot[T] { return Slot[T](a: this.b, b: this.a); }
}
main() {
  s := Slot[int](a: 3, b: 4);
  print_line(s.first.to_string());
  print_line(s.swapped().first.to_string());
}
`)
	// Both bodies exist for the instantiation — a call with no definition is the
	// "undeclared getter" panic, and a stub with no body is a dangling declare.
	assertContains(t, ir, `define i64 @"Slot[int].first"(`)
	assertContains(t, ir, `define %"promise_Slot[int]_v" @"Slot[int].swapped"(`)
	// It is the flat value struct throughout, not a view fat pointer.
	assertContains(t, ir, `%"promise_Slot[int]_v" = type { i8*, i64, i64 }`)
	assertNotContainsMatch(t, ir, `promise_vtable_Slot\[int\]_as_`)
}

// A generic structural INTERFACE keeps the skip: its default methods belong to each
// concrete implementor, not to the interface instantiation itself. (Spelled with a
// default METHOD rather than a getter — a default GETTER on a generic structural
// interface is separately broken, T1559.)
func TestT1550GenericStructuralInterfaceStillSkipsMonoMethods(t *testing.T) {
	ir := generateIR(t, `
type Holder[T] `+"`structural"+` {
  get item T `+"`abstract"+`;
  doubled_item(this) T { return this.item; }
}
type IntBox is Holder[int] {
  int raw;
  get item int => this.raw;
}
main() {
  b := IntBox(raw: 3);
  print_line(b.doubled_item().to_string());
}
`)
	// The default body is synthesized onto the concrete implementor…
	assertContainsMatch(t, ir, `define\s+i64\s+@IntBox\.doubled_item\(`)
	// …and never onto the interface instantiation.
	assertNotContains(t, ir, `@"Holder[int].doubled_item"(`)
}

// Negative: an UNRELATED coercion (concrete → abstract-only structural interface)
// must still materialize a temp — sharesValueStruct must not widen the direct path.
func TestT1550UnrelatedMutRefCoercionStillMaterializesTemp(t *testing.T) {
	ir := generateIR(t, `
type Tagged `+"`structural"+` {
  get tag int `+"`abstract"+`;
}
type Seat {
  int row `+"`value"+`;
  get tag int => this.row * 2;
}
bump(Tagged ~t) { print_line(t.tag.to_string()); }
main() {
  s := Seat(row: 2);
  bump(s);
}
`)
	// The view fat pointer is built into its own temp; the callee receives that.
	assertContainsMatch(t, ir, `call void @__user\.bump\(\{ i8\*, i8\* \}\*`)
}

// coerceToView's own value-type → interface box (boxValueTypeForStructuralView) is
// the NON-escaping crossing: a borrow argument, whose box is freed at statement end
// rather than transferred to the caller. It is a separate function from the escape
// path (…Heap, exercised by the return/decl tests above) and the T1550 fix had to
// teach both that a `structural TARGET which is a value type is not a view — so both
// need a test that actually enters them. This one had none.
func TestT1550ValueTypeIntoInterfaceAsBorrowParamBoxes(t *testing.T) {
	ir := generateIR(t, `
type Tagged `+"`structural"+` {
  get tag int `+"`abstract"+`;
}
type Seat {
  int row `+"`value"+`;
  get tag int => this.row * 2;
}
use_tag(Tagged t) int => t.tag;
main() {
  m := Seat(row: 2);
  print_line(use_tag(t: m).to_string());
}
`)
	// The value type is boxed behind the real interface: a view vtable is emitted and
	// the callee takes the fat pointer by value (a borrow, not a `~` temp pointer).
	assertContains(t, ir, "@promise_vtable_Seat_as_Tagged")
	assertContainsMatch(t, ir, `define\s+i64\s+@__user\.use_tag\(\{ i8\*, i8\* \} `)
	assertContains(t, ir, "call i8* @pal_alloc")
}

// The same borrow-arg crossing for a value NEWTYPE of a `structural value parent —
// the shape where both halves of T1550 meet. The child is not a view (the plain-copy
// rule), but the interface it is passed to IS one, so this must still box, and the
// box must carry the CHILD's typeinfo rather than the shared parent's.
func TestT1550NewtypeIntoInterfaceAsBorrowParamBoxesWithChildTypeinfo(t *testing.T) {
	ir := generateIR(t, `
type Metric `+"`structural"+` {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
type Latency is Metric {
  get tag int => 7;
}
type Doubling `+"`structural"+` {
  get raw_doubled int `+"`abstract"+`;
}
use_it(Doubling d) int => d.raw_doubled;
main() {
  l := Latency(raw: 6);
  print_line(use_it(d: l).to_string());
}
`)
	// Boxed behind the REAL interface…
	assertContains(t, ir, "@promise_vtable_Latency_as_Doubling")
	assertContainsMatch(t, ir, `define\s+i64\s+@__user\.use_it\(\{ i8\*, i8\* \} `)
	// …carrying the child's own typeinfo, so drop/clone dispatch resolves to Latency.
	assertContains(t, ir, "@promise_typeinfo_Latency")
	// …while the parent-typed crossing itself stays a flat value struct.
	assertContains(t, ir, "%promise_Metric_v = type { i8*, i64 }")
	assertNotContainsMatch(t, ir, `promise_vtable_Latency_as_Metric`)
}

// Returning an owned LOCAL as the parent type, rather than a constructor call.
// genReturnStmt has its own structural-target gate (retBoxSrcOwned) that only fires
// for an IdentExpr operand, so the literal-return test above never enters it. If that
// gate had kept selecting on `structural alone it would have marked the flat struct as
// an owned box and handed it to the escape boxer.
func TestT1550OwnedLocalReturnedAsStructuralValueParentIsPlainCopy(t *testing.T) {
	ir := generateIR(t, `
type Metric `+"`structural"+` {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
type Latency is Metric {
  get tag int => 7;
}
make_from_local() Metric {
  l := Latency(raw: 6);
  return l;
}
main() { print_line(make_from_local().raw_doubled.to_string()); }
`)
	assertContainsMatch(t, ir, `define\s+%promise_Metric_v\s+@__user\.make_from_local\(\)`)
	assertNotContainsMatch(t, ir, `promise_vtable_Latency_as_`)
	assertNotContainsMatch(t, ir, `promise_vtable_Metric_as_`)
}

// A `structural value type nested as a `value FIELD of another `structural value type.
// The outer struct embeds the inner's value struct by value, so a boxing decision at
// either level would show up as a pointer-shaped field rather than a store panic —
// and the outer's newtype upcast has to stay a copy of that whole nested struct.
func TestT1550NestedStructuralValueTypeEmbedsInnerValueStruct(t *testing.T) {
	ir := generateIR(t, `
type Metric `+"`structural"+` {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
type Latency is Metric {
  get tag int => 7;
}
type Window `+"`structural"+` {
  Metric lo `+"`value"+`;
  Metric hi `+"`value"+`;
  get span int => this.hi.raw - this.lo.raw;
}
type Bucket is Window {
  get label int => 3;
}
span_of(Window w) int => w.span;
main() {
  b := Bucket(lo: Latency(raw: 2), hi: Latency(raw: 9));
  Window w = b;
  print_line(span_of(w: b).to_string());
  print_line(w.lo.raw_doubled.to_string());
}
`)
	// The inner value struct is embedded by value at both field slots — not {i8*, i8*}.
	assertContains(t, ir, "%promise_Metric_v = type { i8*, i64 }")
	assertContains(t, ir, "%promise_Window_v = type { i8*, %promise_Metric_v, %promise_Metric_v }")
	// The outer newtype shares the outer struct verbatim (T1527) — no struct of its own.
	assertNotContains(t, ir, "%promise_Bucket_v = type")
	// And the parameter carries that nested struct by value.
	assertContainsMatch(t, ir, `define\s+i64\s+@__user\.span_of\(%promise_Window_v `)
	assertNotContainsMatch(t, ir, `promise_vtable_Bucket_as_`)
	assertNotContainsMatch(t, ir, `promise_vtable_Window_as_`)
}

// sharesValueStruct is the predicate that lets genCallArgsWithMutRef hand a `~` param
// the caller's own alloca instead of a coercion temp — i.e. it decides when a write
// through the callee is visible to the caller. Widening it by accident is a silent
// aliasing bug, so its guards are pinned directly: the IR tests above only ever reach
// the true case, and sema rejects the shapes that would reach the rest from source.
func TestT1550SharesValueStructGuards(t *testing.T) {
	c := &Compiler{}
	mk := func(name string, valueType bool) *types.Named {
		n := types.NewNamed(types.NewTypeName(types.Pos{}, name, nil), nil)
		n.SetIsValueType(valueType)
		return n
	}

	valueA, valueB := mk("ValueA", true), mk("ValueB", true)
	heap := mk("Heap", false)

	// No Named on either side (a primitive/function-typed slot) — nothing to share.
	if c.sharesValueStruct(nil, nil) {
		t.Error("nil/nil types must not share a value struct")
	}
	if c.sharesValueStruct(valueA, nil) {
		t.Error("a nil target must not share a value struct")
	}
	// Same type: the caller already took the types.Identical fast path, and answering
	// true here would make this predicate the one deciding an unrelated case.
	if c.sharesValueStruct(valueA, valueA) {
		t.Error("a type must not report sharing with itself")
	}
	// A heap type never shares a value struct, in either direction.
	if c.sharesValueStruct(valueA, heap) || c.sharesValueStruct(heap, valueA) {
		t.Error("a heap type must not share a value struct")
	}
	// Two unrelated value types: same shape, no inheritance link, so passing one where
	// the other is expected must still materialize a coercion temp.
	if c.sharesValueStruct(valueA, valueB) {
		t.Error("unrelated value types must not share a value struct")
	}
}

// --- T1550, second wave: crossings with a distinct LLVM shape to pin ---------------

// Source exercising the crossings the first wave did not reach. Every slot typed at
// the `structural value parent must hold the flat %promise_Metric_v, and no crossing
// may synthesize a view vtable or adapter for it.
const t1550SecondWaveSrc = `
	type Metric ` + "`structural" + ` {
		int raw ` + "`value" + `;
		get raw_doubled int => this.raw * 2;
		mapped[U](this, (int) -> U f) U { int x = this.raw; return f(x); }
	}
	type Latency is Metric {
		get tag int => 7;
	}
	type Crate[T] { T item; get held T => this.item; }
	capture() int { Metric c = Latency(raw: 6); f := || -> c.raw_doubled; return f(); }
	checked!(int n) Metric { if n < 0 { raise error("neg"); } return Latency(raw: n); }
	produce() stream[Metric] { yield Latency(raw: 1); }
	main() {
		c := Crate[Metric](item: Latency(raw: 5));
		print_line(c.held.raw_doubled.to_string());
		m := Metric(raw: 3);
		print_line(m.mapped[int](f: |int x| -> x * 2).to_string());
		print_line(capture().to_string());
		print_line(checked(n: 1)?!.raw_doubled.to_string());
		for g in produce() { print_line(g.raw_doubled.to_string()); }
	}
`

// A generic type whose FIELD is typed at the `structural value parent, constructed with
// a newtype argument. genConstructorCallMono decides per argument whether the field slot
// is a view to box into; it selected that on the `structural annotation alone, so the
// child would have been boxed into a fat pointer and stored into a flat field slot.
func TestT1550NewtypeIntoGenericFieldOfStructuralValueParentStaysFlat(t *testing.T) {
	ir := generateIR(t, t1550SecondWaveSrc)
	// The instance struct embeds the parent's flat value struct inline after the RTTI
	// pointer — a boxed field would be `{ i8*, i8* }` here instead.
	assertContains(t, ir, `%"promise_Crate[Metric]_i" = type { %"promise_Crate[Metric]_m"*, %promise_Metric_v }`)
	// …and the generic getter hands it back by value.
	assertContains(t, ir, `define %promise_Metric_v @"Crate[Metric].held"(`)
	assertNotContainsMatch(t, ir, `define \{ i8\*, i8\* \} @"Crate\[Metric\]\.held"\(`)
}

// A method with its OWN type parameters on a `structural value type. This is
// declareMonoMethodInstances / defineMonoMethodInstances, a different mono entry than
// the declareMonoMethods one the fix repaired — the owner is non-generic here, so only
// the method-instance path can produce this body.
func TestT1550GenericMethodInstanceOnStructuralValueTypeIsDefined(t *testing.T) {
	ir := generateIR(t, t1550SecondWaveSrc)
	// The instance exists (a missing body is the "undeclared method" codegen panic) and
	// takes the receiver as the flat instance pointer plus the closure fat pointer —
	// the `{ i8*, i8* }` is the lambda, not a boxed receiver.
	assertContains(t, ir, `define i64 @"Metric.mapped[int]"(i8* %this, { i8*, i8* } %f)`)
}

// Returned as the parent from a FAILABLE function: the success value rides in the
// failable result triple, a return slot the plain path does not cover.
func TestT1550StructuralValueParentInFailableResultIsFlat(t *testing.T) {
	ir := generateIR(t, t1550SecondWaveSrc)
	// { ok_flag, value, error } — the middle slot is the flat struct, not a fat pointer.
	assertContains(t, ir, `define { i1, %promise_Metric_v, i8* } @__user.checked(i64 %n)`)
	assertNotContainsMatch(t, ir, `define \{ i1, \{ i8\*, i8\* \}, i8\* \} @__user\.checked\(`)
}

// Captured into a closure ENV at the parent type. The env struct is
// `{ i8* env_drop_fn, capture0, … }`; a value capture is stored inline and the drop-fn
// slot stays null. A boxed capture would need a free at env drop, which a pure value
// type must never get — that is a leak or a double free depending on which side wins.
func TestT1550StructuralValueParentCapturedInlineWithNoEnvDrop(t *testing.T) {
	ir := generateIR(t, t1550SecondWaveSrc)
	// The env struct embeds the flat value struct after the drop-fn slot.
	assertContains(t, ir, `{ i8*, %promise_Metric_v }`)
	// And nothing was synthesized to drop it.
	assertNotContainsMatch(t, ir, `@\.lambda\.\d+\.envdrop`)
}

// No crossing anywhere in the second-wave program built a view for the parent or the
// newtype: no view vtable, no view adapter thunk. This is the single assertion that
// would have failed for every shape above before the fix.
func TestT1550SecondWaveBuildsNoViewForValueParent(t *testing.T) {
	ir := generateIR(t, t1550SecondWaveSrc)
	assertNotContainsMatch(t, ir, `promise_vtable_Metric_as_`)
	assertNotContainsMatch(t, ir, `promise_vtable_Latency_as_`)
	assertNotContainsMatch(t, ir, `Metric\.\w+\$view_adapt_as_`)
	assertNotContainsMatch(t, ir, `Latency\.\w+\$view_adapt_as_`)
	// The newtype never gets a value struct of its own — it shares the parent's (T1527).
	assertNotContains(t, ir, `%promise_Latency_v = type`)
}

// hasStructuralParam gates T0092's "this call returns a fresh string, free it at
// statement end" tracking on a parameter being `structural. For a `structural value
// type that parameter is a flat value struct the callee reads fields out of, not an
// interface it formats through — so the gate now selects on isStructuralView and this
// shape takes the ordinary owned-string path instead. Pins that the parameter is passed
// as the value struct and that the string result is still freed exactly once.
func TestT1550StringReturningFnWithStructuralValueParamTakesValueStruct(t *testing.T) {
	ir := generateIR(t, `
type Gauge `+"`structural"+` {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
describe(Gauge g) string { return "g=" + g.raw_doubled.to_string(); }
main() {
  g := Gauge(raw: 6);
  print_line(describe(g: g));
}
`)
	// The param crosses as the flat struct — a fat pointer here is the T1550 defect.
	assertContainsMatch(t, ir, `define\s+i8\*\s+@__user\.describe\(%promise_Gauge_v `)
	assertNotContainsMatch(t, ir, `define\s+i8\*\s+@__user\.describe\(\{ i8\*, i8\* \}`)
	// No view machinery was built for the argument.
	assertNotContainsMatch(t, ir, `promise_vtable_Gauge_as_`)
	// The concatenated result is still freed — the statement-end drop of the temp.
	assertContains(t, ir, "@promise_string_drop")
}

// The receiver-alias check answered "assume the result aliases storage the receiver
// owns" for every `structural receiver. A `structural value type owns no heap storage,
// so the gate now selects on isStructuralView and a closure returned from one of its
// methods is tracked like any other fresh env. Pins that the env is heap-allocated and
// freed rather than treated as borrowed from the receiver.
func TestT1550ClosureReturnedFromStructuralValueMethodOwnsItsEnv(t *testing.T) {
	ir := generateIR(t, `
type Meter `+"`structural"+` {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
  adder(this) (int) -> int {
    int base = this.raw;
    return |int x| -> x + base;
  }
}
main() {
  m := Meter(raw: 6);
  f := m.adder();
  print_line(f(4).to_string());
}
`)
	// The method is emitted on the concrete type itself — not synthesized per
	// implementor as an interface default would be — and returns the closure fat
	// pointer directly rather than through a view adapter.
	assertContainsMatch(t, ir, `define\s+\{ i8\*, i8\* \}\s+@Meter\.adder\(`)
	assertNotContainsMatch(t, ir, `Meter\.adder\$view_adapt_as_`)
	// The returned closure is a fat pointer over a heap env that gets freed.
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "@pal_free")
}

// A generic type parameter CONSTRAINED by a `structural value type. The instantiation
// monomorphizes the body against the flat value struct: the mono layout is the parent's
// own struct and no view is built at the constraint boundary.
func TestT1550GenericConstrainedByStructuralValueTypeMonomorphizesFlat(t *testing.T) {
	ir := generateIR(t, `
type Ledger `+"`structural"+` {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
doubled_of[T: Ledger](T x) int {
  T copy = x;
  return copy.raw_doubled;
}
main() {
  print_line(doubled_of[Ledger](x: Ledger(raw: 5)).to_string());
}
`)
	// The instantiation takes the value struct by value, not a view fat pointer.
	assertContainsMatch(t, ir, `define\s+i64\s+@"doubled_of\[Ledger\]"\(%promise_Ledger_v `)
	assertNotContainsMatch(t, ir, `define\s+i64\s+@"doubled_of\[Ledger\]"\(\{ i8\*, i8\* \}`)
	assertNotContainsMatch(t, ir, `promise_vtable_Ledger_as_`)
}

// The newtype half of the same three shapes: the child is passed where the `structural
// value PARENT is expected, so each crossing is a copy of the struct they share.
func TestT1550NewtypeThroughStringReturningAndGenericParentBoundaries(t *testing.T) {
	ir := generateIR(t, `
type Rate `+"`structural"+` {
  int raw `+"`value"+`;
  get raw_doubled int => this.raw * 2;
}
type Bitrate is Rate {
  get tag int => 9;
}
describe(Rate r) string { return "r=" + r.raw_doubled.to_string(); }
doubled_of[T: Rate](T x) int => x.raw_doubled;
main() {
  b := Bitrate(raw: 6);
  print_line(describe(r: b));
  print_line(doubled_of[Rate](x: b).to_string());
  print_line(doubled_of[Bitrate](x: b).to_string());
}
`)
	// The child never gets a value struct of its own — it reuses the parent's (T1527).
	assertNotContains(t, ir, "%promise_Bitrate_v = type")
	assertContainsMatch(t, ir, `define\s+i8\*\s+@__user\.describe\(%promise_Rate_v `)
	// Both instantiations lower to the same flat struct, neither to a view.
	assertContainsMatch(t, ir, `define\s+i64\s+@"doubled_of\[Rate\]"\(%promise_Rate_v `)
	assertContainsMatch(t, ir, `define\s+i64\s+@"doubled_of\[Bitrate\]"\(%promise_Rate_v `)
	assertNotContainsMatch(t, ir, `promise_vtable_Bitrate_as_`)
}
