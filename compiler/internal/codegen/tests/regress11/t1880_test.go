package regress11

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1880 — a non-generic type that declares `is <structural interface>` must get
// its OWN vtable slots for the interface's default methods filled with a
// per-concrete synthesis. The explicit `is` makes the type a nominal child, so
// boxing takes the prefix-compatible path and reuses the concrete type's own
// vtable rather than building a view vtable; before the fix that vtable reserved
// the inherited defaults' slots and left them null (or, when the interface's own
// body happened to be declared, filled them with that single shared body, which
// dispatches its abstract requirements by INTERFACE-relative slot index and so
// calls the wrong method whenever the interface is not the first parent).
//
// Runtime behaviour is pinned in tests/e2e/structural_explicit_is_default_test.pr
// and tests/modules/module_structural_explicit_is_test.pr.

// assertNoNullVtableSlot fails if the extracted vtable global has any null entry.
func assertNoNullVtableSlot(t *testing.T, vtable, name string) {
	t.Helper()
	if strings.Contains(vtable, "i8* null") {
		t.Errorf("expected no null slot in @%s, got:\n%s", name, vtable)
	}
}

// 1. The item's own repro: `is Writer`, whose defaults are write_string /
// write_line. Both must be emitted under Sink's name and land in Sink's vtable.
func TestT1880ExplicitIsWriterFillsOwnVtableSlots(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Sink is Writer {
		  u8[] buf;
		  new(~this) { this.buf = []; }
		  write!(~this, u8[] ~b) int {
		    int i = 0;
		    while i < b.len { this.buf.push(b[i]); i = i + 1; }
		    return b.len;
		  }
		}
		main() {
		  Writer w = Sink();
		  w.write_string("hi")?!;
		}
	`)
	if codegentest.ExtractFunction(ir, "Sink.write_string") == "" {
		t.Error("expected synthesized @Sink.write_string to be emitted")
	}
	if codegentest.ExtractFunction(ir, "Sink.write_line") == "" {
		t.Error("expected synthesized @Sink.write_line to be emitted")
	}
	vt := codegentest.ExtractGlobal(ir, "promise_vtable_Sink")
	if vt == "" {
		t.Fatal("expected @promise_vtable_Sink to be emitted")
	}
	codegentest.AssertContains(t, vt, "@Sink.write_string")
	codegentest.AssertContains(t, vt, "@Sink.write_line")
	assertNoNullVtableSlot(t, vt, "promise_vtable_Sink")
}

// 2. Two structural parents. The interface's shared default body resolves its
// abstract requirement by the INTERFACE's slot index, so slot 0 of Greeter is
// `name` while slot 0 of Bob is `word`: filling Bob's slot with @Greeter.greet
// silently ran shout()'s requirement instead of greet()'s. Every slot must name
// Bob's own synthesis.
func TestT1880TwoStructuralParentsUsePerConcreteDefaults(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Speaker `+"`structural"+` {
		  word(this) string `+"`abstract"+`;
		  shout(this) string => this.word() + "!";
		}
		type Greeter `+"`structural"+` {
		  name(this) string `+"`abstract"+`;
		  greet(this) string => "hello " + this.name();
		}
		type Bob is Speaker, Greeter {
		  new(~this) {}
		  word(this) string => "hi";
		  name(this) string => "bob";
		}
		type Rob is Bob { new(~this) {} }
		main() {
		  Bob b = Rob();
		  string s = b.greet();
		}
	`)
	vt := codegentest.ExtractGlobal(ir, "promise_vtable_Bob")
	if vt == "" {
		t.Fatal("expected @promise_vtable_Bob to be emitted")
	}
	codegentest.AssertContains(t, vt, "@Bob.greet")
	codegentest.AssertContains(t, vt, "@Bob.shout")
	codegentest.AssertNotContains(t, vt, "@Greeter.greet")
	codegentest.AssertNotContains(t, vt, "@Speaker.shout")
	assertNoNullVtableSlot(t, vt, "promise_vtable_Bob")
}

// 3. Transitive: Fee reaches Ordered through the NON-structural parent Amount, so
// a pass that only looked at direct structural parents would leave Fee's slots
// null. Nothing here boxes into an interface — this is ordinary nominal
// polymorphism through a heap type that happens to have a structural ancestor.
func TestT1880TransitiveStructuralAncestorFillsChildVtable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Amount is Ordered {
		  int cents;
		  ==(Self other) bool => this.cents == other.cents;
		  <(Self other) bool => this.cents < other.cents;
		}
		type Fee is Amount {}
		main() {
		  Amount a = Fee(cents: 5);
		  Amount b = Fee(cents: 2);
		  bool x = a > b;
		}
	`)
	vt := codegentest.ExtractGlobal(ir, "promise_vtable_Fee")
	if vt == "" {
		t.Fatal("expected @promise_vtable_Fee to be emitted")
	}
	codegentest.AssertContains(t, vt, `@"Fee.>"`)
	codegentest.AssertContains(t, vt, `@"Fee.!="`)
	codegentest.AssertContains(t, vt, `@"Fee.<="`)
	codegentest.AssertContains(t, vt, `@"Fee.>="`)
	assertNoNullVtableSlot(t, vt, "promise_vtable_Fee")
	// The abstract requirements stay on the parent that implements them.
	codegentest.AssertContains(t, vt, `@"Amount.<"`)
	codegentest.AssertContains(t, vt, `@"Amount.=="`)
}

// 4. The operator form of case 1: `is Ordered` on a plain heap type. `<` is the
// abstract requirement (it worked before the fix); `>`, `<=`, `>=` are Ordered's
// defaults and `!=` is inherited transitively from Equal — all four were null.
func TestT1880ExplicitIsOrderedFillsInheritedOperatorSlots(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Money is Ordered {
		  int c;
		  ==(Money other) bool => this.c == other.c;
		  <(Money other) bool => this.c < other.c;
		}
		main() {
		  Ordered o = Money(c: 3);
		  bool b = o >= Money(c: 4);
		}
	`)
	vt := codegentest.ExtractGlobal(ir, "promise_vtable_Money")
	if vt == "" {
		t.Fatal("expected @promise_vtable_Money to be emitted")
	}
	codegentest.AssertContains(t, vt, `@"Money.>"`)
	codegentest.AssertContains(t, vt, `@"Money.<="`)
	codegentest.AssertContains(t, vt, `@"Money.>="`)
	codegentest.AssertContains(t, vt, `@"Money.!="`)
	assertNoNullVtableSlot(t, vt, "promise_vtable_Money")
}

// 5. The counter-case the fix must NOT break (T1551): when a non-structural
// ancestor concretely DECLARES the method, its implementation wins over the
// interface default and no per-concrete synthesis is emitted — the eager declare
// pass applies the same findStructuralOwner gate the lazy call-site synthesis
// does, so the vtable slot and direct dispatch keep naming the same function.
func TestT1880ParentOverrideStillWinsOverInterfaceDefault(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Greeter `+"`structural"+` {
		  greet(this) string => "default";
		}
		type Base is Greeter {
		  int n;
		  greet(this) string => "base";
		}
		type Kid is Base {}
		main() {
		  Base b = Kid(n: 1);
		  string s = b.greet();
		}
	`)
	codegentest.AssertNotContains(t, ir, "@Kid.greet(")
	vt := codegentest.ExtractGlobal(ir, "promise_vtable_Kid")
	if vt == "" {
		t.Fatal("expected @promise_vtable_Kid to be emitted")
	}
	codegentest.AssertContains(t, vt, "@Base.greet")
}

// 6. The generic form of case 3. `declareMonoSynthesizedDefaults` used to look
// only at an instance origin's DIRECT structural parents, so `Fee[T] is Amount[T]`
// — whose own parent list holds nothing structural — got no stubs and every
// inherited-default slot in `Fee[int]`'s vtable was null. Both passes now share
// forEachNearestStructuralAncestor with the non-generic pass.
func TestT1880GenericTransitiveStructuralAncestorFillsMonoVtable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Amount[T] is Ordered {
		  int cents;
		  ==(Self other) bool => this.cents == other.cents;
		  <(Self other) bool => this.cents < other.cents;
		}
		type Fee[T] is Amount[T] {}
		main() {
		  Amount[int] a = Fee[int](cents: 5);
		  Amount[int] b = Fee[int](cents: 2);
		  bool x = a > b;
		}
	`)
	vt := codegentest.ExtractGlobal(ir, "promise_vtable_Fee[int]")
	if vt == "" {
		t.Fatal(`expected @"promise_vtable_Fee[int]" to be emitted`)
	}
	codegentest.AssertContains(t, vt, `@"Fee[int].>"`)
	codegentest.AssertContains(t, vt, `@"Fee[int].!="`)
	codegentest.AssertContains(t, vt, `@"Fee[int].<="`)
	codegentest.AssertContains(t, vt, `@"Fee[int].>="`)
	assertNoNullVtableSlot(t, vt, "promise_vtable_Fee[int]")
	// The abstract requirements stay on the instance that implements them.
	codegentest.AssertContains(t, vt, `@"Amount[int].<"`)
	codegentest.AssertContains(t, vt, `@"Amount[int].=="`)
}

// 7. Case 5 reached through a VIEW vtable. When the interface is not the concrete
// type's first parent, boxing builds `_as_<Interface>` instead of reusing the
// type's own vtable, and that path synthesizes defaults lazily. It used to do so
// without asking whether a non-structural ancestor already declared the method,
// so it emitted @Retagged.label from Labelled's default body and the view vtable
// preferred it — the boxed value answered "default" while every other dispatch
// path answered "tagged".
func TestT1880ViewVtableDoesNotShadowParentOverride(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counted `+"`structural"+` {
		  count(this) int `+"`abstract"+`;
		}
		type Labelled `+"`structural"+` {
		  label(this) string => "default";
		}
		type Tagged is Counted, Labelled {
		  int n;
		  count(this) int => this.n;
		  label(this) string => "tagged";
		}
		type Retagged is Tagged {}
		main() {
		  Labelled l = Retagged(n: 1);
		  string s = l.label();
		}
	`)
	codegentest.AssertNotContains(t, ir, "@Retagged.label(")
	vt := codegentest.ExtractGlobal(ir, "promise_vtable_Retagged_as_Labelled")
	if vt == "" {
		t.Fatal("expected @promise_vtable_Retagged_as_Labelled to be emitted")
	}
	codegentest.AssertContains(t, vt, "@Tagged.label")
	assertNoNullVtableSlot(t, vt, "promise_vtable_Retagged_as_Labelled")
}

// 8. The property form: a NON-GENERIC structural interface supplying a default
// getter AND setter. Before the fix the eager per-concrete declare pass ran only
// for *generic* structural interfaces, so this shape reached it for the first
// time — and getters/setters are invisible to LookupMethod, so a walk driven by
// a method value has to select its lookup by member kind or answer "no
// structural owner" and leave the slot null.
func TestT1880ExplicitIsFillsInheritedDefaultPropertySlots(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Gauge `+"`structural"+` {
		  get raw int `+"`abstract"+`;
		  set raw(int v) `+"`abstract"+`;
		  get scaled int => this.raw * 2;
		  set scaled(int v) { this.raw = v / 2; }
		}
		type Dial is Gauge {
		  int stored;
		  get raw int => this.stored;
		  set raw(int v) { this.stored = v; }
		}
		main() {
		  Gauge g = Dial(stored: 4);
		  int n = g.scaled;
		  g.scaled = 20;
		}
	`)
	if codegentest.ExtractFunction(ir, "Dial.scaled") == "" {
		t.Error("expected synthesized @Dial.scaled getter to be emitted")
	}
	if codegentest.ExtractFunction(ir, "Dial.scaled$set") == "" {
		t.Error("expected synthesized @Dial.scaled$set setter to be emitted")
	}
	vt := codegentest.ExtractGlobal(ir, "promise_vtable_Dial")
	if vt == "" {
		t.Fatal("expected @promise_vtable_Dial to be emitted")
	}
	codegentest.AssertContains(t, vt, "@Dial.scaled ")
	codegentest.AssertContains(t, vt, "@Dial.scaled$set")
	assertNoNullVtableSlot(t, vt, "promise_vtable_Dial")
}

// 9. One interface reached by two nominal paths. The ancestor walk descends
// through non-structural parents, so `BothStamps is LeftStamp, RightStamp` sees
// Stamped twice — and `BothKin`, whose two parents share a grandparent, sees
// that grandparent twice. Both visits must be deduplicated: a second pass over
// the same (concrete, interface) pair would append a second body to the same
// function and produce malformed IR.
func TestT1880DiamondSynthesizesEachDefaultOnce(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Stamped `+"`structural"+` {
		  mark(this) int `+"`abstract"+`;
		  stamp(this) string => "mark={this.mark()}";
		}
		type LeftStamp is Stamped { mark(this) int => 1; }
		type RightStamp is Stamped { mark(this) int => 2; }
		type BothStamps is LeftStamp, RightStamp { mark(this) int => 7; }

		type SharedBase is Stamped { mark(this) int => 3; }
		type LeftKin is SharedBase {}
		type RightKin is SharedBase {}
		type BothKin is LeftKin, RightKin {}

		main() {
		  Stamped a = BothStamps();
		  Stamped b = BothKin();
		  string s = a.stamp() + b.stamp();
		}
	`)
	for _, name := range []string{"BothStamps.stamp", "BothKin.stamp"} {
		if n := strings.Count(ir, "define i8* @"+name+"(i8* %this)"); n != 1 {
			t.Errorf("expected exactly one definition of @%s, got %d", name, n)
		}
	}
	for _, ty := range []string{"BothStamps", "BothKin"} {
		vt := codegentest.ExtractGlobal(ir, "promise_vtable_"+ty)
		if vt == "" {
			t.Fatalf("expected @promise_vtable_%s to be emitted", ty)
		}
		codegentest.AssertContains(t, vt, "@"+ty+".stamp")
		assertNoNullVtableSlot(t, vt, "promise_vtable_"+ty)
	}
}

// 10. The pure-value-type form. A value type's `this` points at the value struct
// rather than an instance, so Ordered's `> (Self other) => other < this` has to
// marshal `this`-as-argument differently — and both dispatch paths must do it
// the same way, or the direct call and the vtable slot disagree.
func TestT1880ValueTypeExplicitIsFillsOwnVtableSlots(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Cents is Ordered {
		  int v `+"`value"+`;
		  ==(Self other) bool => this.v == other.v;
		  <(Self other) bool => this.v < other.v;
		}
		main() {
		  Ordered o = Cents(v: 3);
		  bool b = o >= Cents(v: 1);
		}
	`)
	vt := codegentest.ExtractGlobal(ir, "promise_vtable_Cents")
	if vt == "" {
		t.Fatal("expected @promise_vtable_Cents to be emitted")
	}
	for _, op := range []string{">", "<=", ">=", "!="} {
		codegentest.AssertContains(t, vt, `@"Cents.`+op+`"`)
	}
	assertNoNullVtableSlot(t, vt, "promise_vtable_Cents")
}

// 11. The module form. compileModule builds its types' vtables before the main
// file is compiled, so it needs its own per-concrete declare/define pass — the
// main file's pass never sees a module's declarations. This is the arrangement
// T1734 needs, since every non-generic heap I/O type that will declare
// `is Reader` / `is Writer` lives in a catalog module.
func TestT1880ModuleTypeExplicitIsFillsOwnVtableSlots(t *testing.T) {
	ir := codegentest.GenerateIRWithModule(t, "sinkmod", `
		type ModSink is Writer `+"`public"+` {
		  int n `+"`public"+`;
		  write!(~this, u8[] ~b) int `+"`public"+` { return b.len; }
		}
	`, `
		use sinkmod "./sinkmod";
		main() {
		  Writer w = sinkmod.ModSink(n: 0);
		  w.write_string("hi")?!;
		}
	`)
	if codegentest.ExtractFunction(ir, "ModSink.write_string") == "" {
		t.Error("expected synthesized @ModSink.write_string to be emitted for the module type")
	}
	if codegentest.ExtractFunction(ir, "ModSink.write_line") == "" {
		t.Error("expected synthesized @ModSink.write_line to be emitted for the module type")
	}
	// The module type's own methods are name-prefixed with the module; the
	// synthesized defaults are not — they are declared under the concrete type's
	// plain name and stay in the main IR (declareStructuralDefaultStubs does not
	// register them in moduleOwnedFuncs). The vtable has to bridge the two.
	vt := codegentest.ExtractGlobal(ir, "promise_vtable___mod_sinkmod_ModSink")
	if vt == "" {
		t.Fatal("expected @promise_vtable___mod_sinkmod_ModSink to be emitted")
	}
	codegentest.AssertContains(t, vt, "@__mod_sinkmod_ModSink.write ")
	codegentest.AssertContains(t, vt, "@ModSink.write_string")
	codegentest.AssertContains(t, vt, "@ModSink.write_line")
	assertNoNullVtableSlot(t, vt, "promise_vtable___mod_sinkmod_ModSink")
}

// 12. The assertion's scope, from the other side: an abstract class between the
// interface and the implementation re-declares the requirement as abstract, so
// its own vtable slot is null BY DESIGN and no per-concrete synthesis could ever
// fill it. Only the default it also inherits is the assertion's business — an
// over-eager guard would refuse to compile every abstract class under a
// structural interface.
func TestT1880AbstractSlotUnderStructuralParentStaysNull(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Drawable `+"`structural"+` {
		  draw(this) string `+"`abstract"+`;
		  render(this) string => "<{this.draw()}>";
		}
		type Shape is Drawable {
		  int id;
		  draw(this) string `+"`abstract"+`;
		}
		type Circle is Shape {
		  draw(this) string => "o";
		}
		main() {
		  Shape s = Circle(id: 1);
		  string r = s.render();
		}
	`)
	vt := codegentest.ExtractGlobal(ir, "promise_vtable_Shape")
	if vt == "" {
		t.Fatal("expected @promise_vtable_Shape to be emitted")
	}
	// The abstract requirement's slot is null and the compile still succeeded.
	codegentest.AssertContains(t, vt, "i8* null")
	// The inherited default's slot is not — it names the abstract class's own
	// synthesis, whose body dispatches draw() virtually to the concrete child.
	codegentest.AssertContains(t, vt, "@Shape.render")

	concrete := codegentest.ExtractGlobal(ir, "promise_vtable_Circle")
	if concrete == "" {
		t.Fatal("expected @promise_vtable_Circle to be emitted")
	}
	codegentest.AssertContains(t, concrete, "@Circle.draw")
	codegentest.AssertContains(t, concrete, "@Circle.render")
	assertNoNullVtableSlot(t, concrete, "promise_vtable_Circle")
}

// 13. Declaring an interface AND the interface it already inherits. `Ordered is
// Equal`, so the ancestor walk stops at Ordered and recurses into Equal for
// `!=`, then visits Equal directly and reaches the same method again. Exactly
// one @"Rank.!=" may be emitted — a second declaration would append a second
// body to the same function and produce malformed IR.
func TestT1880InterfaceAndItsOwnParentDeclareTheDefaultOnce(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Rank is Ordered, Equal {
		  int n;
		  ==(Self other) bool => this.n == other.n;
		  <(Self other) bool => this.n < other.n;
		}
		main() {
		  Ordered o = Rank(n: 2);
		  bool b = o != Rank(n: 1);
		}
	`)
	if n := strings.Count(ir, `define i1 @"Rank.!="`); n != 1 {
		t.Errorf(`expected exactly one definition of @"Rank.!=", got %d`, n)
	}
	vt := codegentest.ExtractGlobal(ir, "promise_vtable_Rank")
	if vt == "" {
		t.Fatal("expected @promise_vtable_Rank to be emitted")
	}
	for _, op := range []string{"!=", ">", "<=", ">="} {
		codegentest.AssertContains(t, vt, `@"Rank.`+op+`"`)
	}
	assertNoNullVtableSlot(t, vt, "promise_vtable_Rank")
}
