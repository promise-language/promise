package generic2

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

func TestDropSynthesizedGeneric(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Wrapper[T] {
			Inner inner;
			T value;
		}
		main() {
			w := Wrapper[int](inner: Inner(id: 1), value: 42);
		}
	`)
	codegentest.AssertContains(t, ir, "Wrapper[int].drop")
}

// T0132: Generic type with generic droppable field gets cascading mono drop.
// Set[T] has a Map[T, bool] field — the synthesized Set[int].drop must call
// Map[int, bool].drop (mono name), not Map.drop (origin name which doesn't exist).
func TestDropSynthesizedGenericCascading(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Box[T] {
			Inner inner;
		}
		type Outer[T] {
			Box[T] box;
		}
		main() {
			o := Outer[int](box: Box[int](inner: Inner(id: 1)));
		}
	`)
	// Outer[int].drop must call Box[int].drop, not Box.drop
	codegentest.AssertContains(t, ir, `call void @"Box[int].drop"`)
	codegentest.AssertContains(t, ir, "Outer[int].drop")
}

// B0212: Generic enum instances (like Slot[K,V]) get synthesized drops at mono time
// when sema couldn't detect droppability for TypeParam variant fields.
func TestDropMonoEnumInstSynthDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Wrapper[T] {
			Some(T value),
			None,
		}
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			w := Wrapper[Resource].Some(Resource(id: 1));
		}
	`)
	// Wrapper[Resource] should get a synthesized drop that calls Resource.drop
	codegentest.AssertContains(t, ir, `define void @"Wrapper[Resource].drop"`)
	codegentest.AssertContains(t, ir, "call void @Resource.drop(")
}

// T0552: Type with generic-enum field whose TypeParam resolves to a droppable
// concrete type. monoTypeHasDroppable must see through the generic enum Instance
// (via monoEnumInstNeedsSynthDrop), and emitFieldDropsFor must drop the enum
// field by invoking the mono enum's drop function. Without both, the inner
// droppable leaks at scope exit of the holder.
func TestDropGenericTypeWithGenericEnumField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource { int id; drop(~this) { } }
		enum Maybe[T] {
			Some(T value),
			Nothing,
		}
		type Holder[T] {
			Maybe[T] m;
		}
		main() {
			j := Maybe[Resource].Some(Resource(id: 1));
			c := Holder[Resource](m: j);
		}
	`)
	codegentest.AssertContains(t, ir, `define void @"Holder[Resource].drop"`)
	codegentest.AssertContains(t, ir, `call void @"Maybe[Resource].drop"`)
}

// T0552: Non-generic holder containing a non-generic enum field with a
// droppable variant. Sema sets NeedsSynthDrop=true on the holder (since the
// concrete enum's HasDrop is observable), so a synth drop body is generated —
// but before T0552, emitFieldDropsFor's `extractNamed == nil` skip dropped the
// enum field silently. This test locks down the enum-field branch added in
// emitFieldDropsFor for the non-generic case (the generic case is covered by
// TestDropGenericTypeWithGenericEnumField above).
func TestDropTypeWithEnumFieldNonGeneric(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource { int id; drop(~this) { } }
		enum Maybe {
			Some(Resource value),
			Nothing,
		}
		type Holder {
			Maybe m;
		}
		main() {
			j := Maybe.Some(Resource(id: 1));
			c := Holder(m: j);
		}
	`)
	codegentest.AssertContains(t, ir, "define void @Holder.drop")
	codegentest.AssertContains(t, ir, "call void @Maybe.drop")
}

// T0572: Generic holder with Optional<GenericEnum[T]> field. Exercises the
// monoEnumInstNeedsSynthDrop branch in the needsDrop check — without it,
// HasDrop on the un-substituted enum origin is false, the early-return
// fires, and the inner droppable leaks.
func TestDropGenericTypeWithOptionalGenericEnumField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource { int id; drop(~this) { } }
		enum Maybe[T] {
			Some(T value),
			Nothing,
		}
		type Holder[T] {
			Maybe[T]? m;
		}
		main() {
			j := Maybe[Resource].Some(Resource(id: 1));
			c := Holder[Resource](m: j);
		}
	`)
	codegentest.AssertContains(t, ir, `define void @"Holder[Resource].drop"`)
	codegentest.AssertContains(t, ir, "optfield.drop")
	codegentest.AssertContains(t, ir, `call void @"Maybe[Resource].drop"`)
}

// B0238: Generic enum variables with TypeParam-only droppable fields must get drop
// registered at scope exit. maybeRegisterDrop must check monoEnumInstNeedsSynthDrop.
func TestDropGenericEnumVarWithDroppableTypeParam(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper { string name; int value; }
		enum Container[T] {
			Holding(T item),
			Empty,
		}
		main() {
			c := Container[Wrapper].Holding(Wrapper(name: "hello", value: 42));
		}
	`)
	// Container[Wrapper] should get a synthesized drop and it must be called at scope exit
	codegentest.AssertContains(t, ir, `define void @"Container[Wrapper].drop"`)
	codegentest.AssertContains(t, ir, `call void @"Container[Wrapper].drop"`)
}

// T0405: Generic type with T[] field — reassignment must drop string elements
// when T=string (exercises the typeSubst-substituted fieldType path).
func TestFieldAssignGenericVecDropsElements(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			T[] items;
			update(~this, T[] move val) { this.items = val; }
		}
		main() {
			b := Box[string](items: string[]());
			v := string[]();
			b.update(v);
		}
	`)
	codegentest.AssertContains(t, ir, "field.vecdrop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
	codegentest.AssertContains(t, ir, "call void @Vector.drop(")
}

// --- Return optional wrapping in monomorphized context ---

func TestReturnOptionalInMonoMethod(t *testing.T) {
	// The map [] method returns V? — returning a concrete V must wrap in Optional
	ir := codegentest.GenerateIR(t, `
		main() {
			m := {"x": 42};
			int? v = m["x"];
		}
	`)
	// The monomorphized [] method should produce { i1, i64 } return type
	codegentest.AssertContains(t, ir, `define { i1, i64 } @"Map[string, int].[]"(`)
	// Should contain insertvalue for wrapping the value in Optional { true, val }
	codegentest.AssertContains(t, ir, "insertvalue { i1, i64 }")
}

// --- Nested generic monomorphization (discoverInstances) ---

func TestNestedGenericMonomorphization(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T val; }
		type Wrapper[T] { Box[T] inner; }
		main() {
			w := Wrapper[int](inner: Box[int](val: 42));
		}
	`)
	// Both Wrapper[int] and Box[int] should be monomorphized
	codegentest.AssertContains(t, ir, "Wrapper[int]")
	codegentest.AssertContains(t, ir, "Box[int]")
}

// T0731: the `c.typeSubst != nil` substitution branch of the spawn-side dup
// loop. When a GENERIC function (`gmake[T]`) captures a borrowed `T` param in a
// value-block, the capture's sema type resolved from `c.info.Types` is the raw
// TypeParam `T` (sema type-checks the generic body once with T unbound). Without
// `types.Substitute(capType, c.typeSubst)`, `goElemNeedsBorrowedCaptureDup(T)`
// returns false (a bare TypeParam is neither string/Vector/Map/heap-user) and no
// dup would be emitted — a UAF once T=string. The Substitute call resolves T to
// the concrete `string`, so the monomorphized `gmake[string]` DOES dup its
// borrowed heap param. The non-heap sibling (`t0683_mk_int[int]`, dup filtered)
// only exercises the Substitute-then-reject side; this locks the dup-emitting
// side.
func TestT0731_GenericHeapParamDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		gmake[T](T v) Task[T] {
			return go { s := v; s };
		}
		main() {
			task[string] x = gmake[string]("a" + "b");
			r := <-x;
		}
	`)
	// The monomorphized instance is emitted as @"gmake[string]" (quoted name).
	monoIR := codegentest.ExtractFunction(ir, `"gmake[string]"`)
	codegentest.AssertContains(t, monoIR, "@promise_string_new(")
	codegentest.AssertContains(t, monoIR, "call i8* @.goroutine.")
}

// T1261: a GENERIC-owner method referencing `this` in a `go`-CALL resolves the
// receiver type via the mono instance, deep-copies the concrete snapshot, and
// threads it into the coroutine — the mono path of the call-form fix.
func TestT1261_GoCallThisGenericOwner(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1261Box[T] {
			T value;
			send_it(this, Channel[T] out) {
				go out.send(this.value);
			}
		}
		main() {
			b := T1261Box[int](value: 9);
			d := channel[int](capacity: 1);
			b.send_it(d);
			r := <-d;
			d.close();
		}
	`)
	sendIR := codegentest.ExtractFunction(ir, `"T1261Box[int].send_it"`)
	codegentest.AssertContains(t, sendIR, "heapdup.copy")
	codegentest.AssertContains(t, sendIR, `call i8* @".goroutine.T1261Box[int].send_it.`)
}

// T1198: the borrowed-param dup on the fast path must survive monomorphization.
// When the spawner is a GENERIC free function, its body is codegen'd with
// c.typeSubst active, so genGoCallExpr must substitute the capture type before
// the eligibility gate (mirroring the value-block/via-block paths). A concrete
// `string` param inside a `spawn[T]` spawner still dups. This exercises the
// `c.typeSubst != nil` branch that non-generic spawners never reach.
func TestT1198_FastPathBorrowedParamDupsUnderMonomorphization(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		derive(string v, Channel[string] out) {
			out.send(v + "!");
		}
		spawn[T: Ordered](string v, Channel[string] out, T marker) {
			go derive(v, out);
		}
		main() {
			ch := channel[string](capacity: 1);
			spawn("a" + "b", ch, 0);
			r := <-ch;
		}
	`)
	// The generic spawner is monomorphized to `spawn[int]` (quoted in IR because
	// of the brackets). The borrowed string param `v` is still dup'd there.
	spawnIR := codegentest.ExtractFunction(ir, `"spawn[int]"`)
	codegentest.AssertContains(t, spawnIR, "@promise_string_new(")
	codegentest.AssertContains(t, spawnIR, "call i8* @.goroutine.")
}

// T1233: The caller-side tuple-temp drop must also fire inside a MONOMORPHIZED
// generic body. There the tuple type is `(T, int)` with a TypeParam field, so
// registerTupleStmtTemp / emitTupleTempDrop run with c.typeSubst active and must
// substitute T → the concrete droppable type before the field-wise drop walk
// (the substitution branch the non-generic tests above never reach). This builds
// a `(T, int)` literal temp (T from a maker closure) passed to a borrow param and
// instantiates T=string, asserting the drop block appears in the specialized
// instance with the concrete string field's drop.
func TestT1233GenericTupleTempDropMonomorphized(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		gwrap[T]((T, int) t) {}
		gpass[T](() -> T make) {
			gwrap[T]((make(), 1));
		}
		main() {
			gpass[string](|| -> "a" + "b");
		}
	`)
	// The specialized gpass[string] instance must field-wise drop its tuple temp.
	codegentest.AssertContains(t, ir, "gpass[string]")
	codegentest.AssertContains(t, ir, "tuptmp.drop")
	// The substituted string field's drop fires in the monomorphized body.
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
}

func TestMultiParamGenericType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pair[A, B] {
			A first;
			B second;
		}
		main() {
			p := Pair[int, string](first: 42, second: "hello");
		}
	`)
	// Monomorphized struct name should contain both type args
	codegentest.AssertContains(t, ir, "Pair[int, string]")
}

func TestMultiParamGenericFunc(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_pair[A, B](A a, B b) (A, B) {
			return (a, b);
		}
		main() {
			(x, y) := make_pair[int, string](42, "hi");
		}
	`)
	// Monomorphized function name should contain both type args
	codegentest.AssertContains(t, ir, "make_pair[int, string]")
}

// B0224: Operator methods on generic value types must use the mono name.
func TestGenericValueTypeOperatorDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pair[T: Equal] {
			T a `+"`value"+`;
			T b `+"`value"+`;
			==(Pair[T] other) bool => this.a == other.a && this.b == other.b;
			!=(Pair[T] other) bool => !(this == other);
		}
		main() {
			p := Pair[int](a: 1, b: 2);
			q := Pair[int](a: 1, b: 2);
			bool r = p == q;
		}
	`)
	// Operator dispatches to mono name Pair[int].==
	codegentest.AssertContains(t, ir, `@"Pair[int].=="`)
}

// TestGenericPropertyIncDecIR: T0712. Inc/dec on a property of a generic type
// must dispatch through the monomorphized getter/setter. `this.total++` inside a
// generic method body exercises the receiver-type substitution branch
// (c.typeSubst) in genIncDecTarget — the receiver type is Box[T] and must be
// substituted to Box[int] before the accessor lookup.
func TestGenericPropertyIncDecIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			T v;
			int count;
			get total int { return this.count; }
			set total(int x) { this.count = x; }
			bump(~this) { this.total++; }
		}
		main() {
			b := Box[int](v: 5, count: 10);
			b.bump();
			b.total--;
		}
	`)
	// Inside the monomorphized bump(): this.total++ dispatches through mono accessors.
	codegentest.AssertContains(t, ir, `call i64 @"Box[int].total"(`)
	codegentest.AssertContains(t, ir, "add i64")
	codegentest.AssertContains(t, ir, `call void @"Box[int].total$set"(`)
	// At main's call site: b.total-- (concrete-instance receiver, no typeSubst).
	codegentest.AssertContains(t, ir, "sub i64")
}

func TestGenericInheritanceNonGenericChild(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder[T] { T value; }
		type IntHolder is Holder[int] {}
		main() {
			h := IntHolder(value: 42);
			int x = h.value;
		}
	`)
	// IntHolder uses Holder's layout — field should be i64 (int)
	codegentest.AssertContains(t, ir, "IntHolder")
	codegentest.AssertContains(t, ir, "load i64")
}

func TestGenericInheritanceForwardedTypeParams(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base[T] {
			T data;
			get() T { return this.data; }
		}
		type Derived[T] is Base[T] {
			get() T { return this.data; }
		}
		main() {
			d := Derived[int](data: 99);
			int x = d.get();
		}
	`)
	// Monomorphized names should appear
	codegentest.AssertContains(t, ir, "Derived[int]")
	codegentest.AssertContains(t, ir, "Base[int]")
}

func TestMonoTypeVtableEmission(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Producer[T] {
			produce() T `+"`"+`abstract;
		}
		type ConstProducer[T] is Producer[T] {
			T value;
			produce() T { return this.value; }
		}
		accept_producer(Producer[int] p) int {
			return p.produce();
		}
		main() {
			cp := ConstProducer[int](value: 5);
			int x = accept_producer(cp);
		}
	`)
	// Mono vtable and typeinfo should be emitted for ConstProducer[int]
	codegentest.AssertContains(t, ir, "promise_vtable_ConstProducer[int]")
	codegentest.AssertContains(t, ir, "promise_typeinfo_ConstProducer[int]")
	// The vtable should contain the mono method pointer
	codegentest.AssertContains(t, ir, "ConstProducer[int].produce")
}

func TestMonoVtableVirtualDispatchIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shape[T] {
			area() T `+"`"+`abstract;
		}
		type Circle[T] is Shape[T] {
			T radius;
			area() T { return this.radius; }
		}
		accept_shape(Shape[int] s) int {
			return s.area();
		}
		main() {
			c := Circle[int](radius: 5);
			int x = accept_shape(c);
		}
	`)
	// Vtable should exist for both parent and child mono instances
	codegentest.AssertContains(t, ir, "promise_vtable_Circle[int]")
	codegentest.AssertContains(t, ir, "promise_vtable_Shape[int]")
	// accept_shape should do virtual dispatch (load from vtable, indirect call)
	codegentest.AssertContains(t, ir, "promise_vtable_Shape[int]")
	// Mono method should be defined
	codegentest.AssertContains(t, ir, "Circle[int].area")
}

func TestMultipleMonoVtablesDistinct(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Producer[T] {
			produce() T `+"`"+`abstract;
		}
		type ConstProducer[T] is Producer[T] {
			T value;
			produce() T { return this.value; }
		}
		use_int(Producer[int] p) int { return p.produce(); }
		use_str(Producer[string] p) string { return p.produce(); }
		main() {
			ci := ConstProducer[int](value: 1);
			cs := ConstProducer[string](value: "x");
			int i = use_int(ci);
			string s = use_str(cs);
		}
	`)
	// Separate vtables for int and string instantiations
	codegentest.AssertContains(t, ir, "promise_vtable_ConstProducer[int]")
	codegentest.AssertContains(t, ir, "promise_vtable_ConstProducer[string]")
	codegentest.AssertContains(t, ir, "promise_typeinfo_ConstProducer[int]")
	codegentest.AssertContains(t, ir, "promise_typeinfo_ConstProducer[string]")
	// Separate methods
	codegentest.AssertContains(t, ir, "ConstProducer[int].produce")
	codegentest.AssertContains(t, ir, "ConstProducer[string].produce")
}

func TestMonoVtableInheritedMethodResolution(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base[T] {
			T val;
			get() T { return this.val; }
		}
		type Mid[T] is Base[T] {}
		type Leaf[T] is Mid[T] {}
		accept(Base[int] b) int { return b.get(); }
		main() {
			l := Leaf[int](val: 7);
			int x = accept(l);
		}
	`)
	// Leaf[int] vtable should reference Base[int].get (inherited method)
	codegentest.AssertContains(t, ir, "promise_vtable_Leaf[int]")
	codegentest.AssertContains(t, ir, "Base[int].get")
}

func TestMonoTypeInfoEmittedForParent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal[T] {
			T id;
			name() string `+"`"+`abstract;
		}
		type Dog[T] is Animal[T] {
			name() string { return "dog"; }
		}
		accept(Animal[int] a) string { return a.name(); }
		main() {
			d := Dog[int](id: 1);
			string s = accept(d);
		}
	`)
	// Both parent and child should have typeinfo
	codegentest.AssertContains(t, ir, "promise_typeinfo_Dog[int]")
	codegentest.AssertContains(t, ir, "promise_typeinfo_Animal[int]")
	codegentest.AssertContains(t, ir, "promise_vtable_Dog[int]")
}

func TestMonoVtableOverrideDispatches(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Greeter[T] {
			T name;
			greet() string { return "hello"; }
		}
		type Fancy[T] is Greeter[T] {
			greet() string { return "fancy"; }
		}
		accept(Greeter[int] g) string { return g.greet(); }
		main() {
			Greeter[int] a = Greeter[int](name: 1);
			Greeter[int] b = Fancy[int](name: 2);
			string x = accept(a);
			string y = accept(b);
		}
	`)
	// Both should have vtables with their own greet method
	codegentest.AssertContains(t, ir, "promise_vtable_Greeter[int]")
	codegentest.AssertContains(t, ir, "promise_vtable_Fancy[int]")
	codegentest.AssertContains(t, ir, "Greeter[int].greet")
	codegentest.AssertContains(t, ir, "Fancy[int].greet")
}

func TestMonoVtableNonGenericChildOfGenericParent(t *testing.T) {
	// Non-generic children already have vtables via emitVtableGlobals.
	// Verify they coexist with mono vtables for the parent.
	ir := codegentest.GenerateIR(t, `
		type Fabricator[T] {
			fabricate() T `+"`"+`abstract;
		}
		type IntFabricator is Fabricator[int] {
			fabricate() int { return 42; }
		}
		type GenFabricator[T] is Fabricator[T] {
			T val;
			fabricate() T { return this.val; }
		}
		use_fab(Fabricator[int] m) int { return m.fabricate(); }
		main() {
			int a = use_fab(IntFabricator());
			int b = use_fab(GenFabricator[int](val: 7));
		}
	`)
	// Non-generic child uses regular vtable naming
	codegentest.AssertContains(t, ir, "promise_vtable_IntFabricator")
	// Generic child uses mono vtable naming
	codegentest.AssertContains(t, ir, "promise_vtable_GenFabricator[int]")
	// Both methods exist
	codegentest.AssertContains(t, ir, "IntFabricator.fabricate")
	codegentest.AssertContains(t, ir, "GenFabricator[int].fabricate")
}

func TestMethodGenericIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Echo {
			echo[T](T val) T { return val; }
		}
		main() {
			e := Echo();
			int x = e.echo[int](42);
			string s = e.echo[string]("hi");
		}
	`)
	// Monomorphized method names should appear
	codegentest.AssertContains(t, ir, "Echo.echo[int]")
	codegentest.AssertContains(t, ir, "Echo.echo[string]")
}

func TestMethodGenericOnGenericTypeIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			T item;
			convert[R](R val) R { return val; }
		}
		main() {
			b := Box[int](item: 1);
			string s = b.convert[string]("hello");
		}
	`)
	// Should have mono type name + mono method name
	codegentest.AssertContains(t, ir, "Box[int].convert[string]")
}

// --- Monomorphization: gaps ---

// TestGenericValueTypeLayout verifies that a generic pure-value type gets the
// correct layout: fields are embedded in the value struct (_v), and the instance
// struct (_i) is RTTI-only (no user fields). Also checks that no heap allocation
// is emitted (value types are stack-allocated and copied).
func TestGenericValueTypeLayout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pair[T] {
			T first `+"`"+`value;
			T second `+"`"+`value;
			sum(this) T { return this.first; }
		}
		main() {
			p := Pair[int](first: 1, second: 2);
			x := p.sum();
		}
	`)
	// Mono type names should appear
	codegentest.AssertContains(t, ir, "Pair[int]")
	// Value struct has embedded fields (vtable + first + second)
	// The _v struct is named promise_Pair[int]_v
	codegentest.AssertContains(t, ir, "promise_Pair[int]_v")
	// Instance struct is RTTI-only (no user fields) — just the _variant pointer
	codegentest.AssertContains(t, ir, "promise_Pair[int]_i")
	// RTTI global is emitted for value types
	codegentest.AssertContains(t, ir, "promise_rtti_Pair[int]")
	// No heap allocation: value types are not malloc'd
	codegentest.AssertNotContains(t, ir, "promise_Pair[int]_i* @malloc")
}

// T0565: a non-generic user type that has a generic value-type instance as a
// field must lay out the field slot using the mono value struct (wider, with
// embedded fields), not the generic {i8*, i8*} slot. The construction-time
// store would otherwise mismatch the slot type and crash codegen.
func TestGenericValueTypeAsField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pt[T] {
			T x `+"`"+`value;
			T y `+"`"+`value;
		}
		type Outer {
			Pt[int] inner;
		}
		main() {
			o := Outer(inner: Pt[int](x: 1, y: 2));
		}
	`)
	// The mono value struct typedef is present.
	codegentest.AssertContains(t, ir, "%\"promise_Pt[int]_v\" = type { i8*, i64, i64 }")
	// Outer's instance struct uses the wider mono value struct as the field
	// slot, not the standard {i8*, i8*} value layout.
	codegentest.AssertContains(t, ir, "%promise_Outer_i = type { %promise_Outer_m*, %\"promise_Pt[int]_v\" }")
}

// T0565: a non-generic value type used as a direct field of another type with
// REVERSE declaration order (containing type before value type). Without the
// topological walk over value-type field dependencies, the containing type's
// layout would be computed before the value type's, producing the wrong slot
// type. This exercises the extractNamed/IsValueType fallback in
// collectValueTypeFieldDeps.
func TestNonGenericValueTypeFieldReverseOrder(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type WithCoord {
			Coord pos;
		}
		type Coord {
			int x `+"`"+`value;
			int y `+"`"+`value;
		}
		main() {
			w := WithCoord(pos: Coord(x: 1, y: 2));
		}
	`)
	// Coord's value struct typedef exists.
	codegentest.AssertContains(t, ir, "%promise_Coord_v = type { i8*, i64, i64 }")
	// WithCoord's instance struct uses the wider Coord value struct, not {i8*, i8*}.
	codegentest.AssertContains(t, ir, "%promise_WithCoord_i = type { %promise_WithCoord_m*, %promise_Coord_v }")
}

// T0565: a tuple field containing generic value-type instances. Exercises the
// *types.Tuple recursion in collectValueTypeFieldDeps so each tuple element is
// laid out before the containing type.
func TestTupleOfGenericValueTypesAsField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pt[T] {
			T x `+"`"+`value;
			T y `+"`"+`value;
		}
		type WithTuple {
			(Pt[int], Pt[f64]) pair;
		}
		main() {
			w := WithTuple(pair: (Pt[int](x: 1, y: 2), Pt[f64](x: 3.0, y: 4.0)));
		}
	`)
	// Both mono value structs are present.
	codegentest.AssertContains(t, ir, "%\"promise_Pt[int]_v\" = type { i8*, i64, i64 }")
	codegentest.AssertContains(t, ir, "%\"promise_Pt[f64]_v\" = type { i8*, double, double }")
}

// T0565: a generic outer type with a Pt[T] field — after monomorphization the
// substituted field becomes Pt[int] (a *types.Instance after subst). The mono
// outer layout must use the wider value struct for the field slot.
func TestGenericOuterWithGenericValueField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pt[T] {
			T x `+"`"+`value;
			T y `+"`"+`value;
		}
		type Container[T] {
			Pt[T] pos;
		}
		main() {
			c := Container[int](pos: Pt[int](x: 1, y: 2));
		}
	`)
	codegentest.AssertContains(t, ir, "%\"promise_Pt[int]_v\" = type { i8*, i64, i64 }")
	codegentest.AssertContains(t, ir, "%\"promise_Container[int]_i\" = type { %\"promise_Container[int]_m\"*, %\"promise_Pt[int]_v\" }")
}

// TestGenericValueTypeTwoInstances verifies that two instantiations of the same
// generic value type produce distinct layouts and RTTI globals.
func TestGenericValueTypeTwoInstances(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pair[T] {
			T first `+"`"+`value;
			T second `+"`"+`value;
		}
		main() {
			pi := Pair[int](first: 1, second: 2);
			pb := Pair[bool](first: true, second: false);
		}
	`)
	codegentest.AssertContains(t, ir, "promise_Pair[int]_v")
	codegentest.AssertContains(t, ir, "promise_Pair[bool]_v")
	codegentest.AssertContains(t, ir, "promise_rtti_Pair[int]")
	codegentest.AssertContains(t, ir, "promise_rtti_Pair[bool]")
	// Separate typeinfo for each instantiation
	codegentest.AssertContains(t, ir, "promise_typeinfo_Pair[int]")
	codegentest.AssertContains(t, ir, "promise_typeinfo_Pair[bool]")
}

// TestGenericEnumTwoTypeParams verifies that a generic enum with two type parameters
// is correctly monomorphized, producing distinct structs and a match that extracts
// both variant fields.
func TestGenericEnumTwoTypeParams(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Either[A, B] {
			Left(A val),
			Right(B val),
		}
		get_left(Either[int, string] e) int {
			int r = match e {
				Left(v) => v,
				Right(_) => -1,
			};
			return r;
		}
		main() {
			e := Either[int, string].Left(42);
			x := get_left(e);
		}
	`)
	// Both type params in mangled name
	codegentest.AssertContains(t, ir, "Either[int, string]")
	// Value struct typedef emitted
	codegentest.AssertContains(t, ir, "promise_Either[int, string]_v")
	// Function using the mono type exists
	codegentest.AssertContains(t, ir, "get_left")
}

// TestDeeplyNestedGenericMonomorphization verifies that transitive instance
// discovery via field types works at 3 levels of nesting.
func TestDeeplyNestedGenericMonomorphization(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T val; }
		main() {
			inner := Box[int](val: 1);
			mid := Box[Box[int]](val: inner);
			outer := Box[Box[Box[int]]](val: mid);
		}
	`)
	// All three levels must be monomorphized
	codegentest.AssertContains(t, ir, "Box[int]")
	codegentest.AssertContains(t, ir, "Box[Box[int]]")
	codegentest.AssertContains(t, ir, "Box[Box[Box[int]]]")
}

// TestGenericMethodReturnsGenericInstance verifies that a generic method whose
// return type is a monomorphized generic type is correctly compiled.
func TestGenericMethodReturnsGenericInstance(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			T val;
			clone(this) Box[T] { return Box[T](val: this.val); }
		}
		main() {
			b := Box[int](val: 5);
			c := b.clone();
		}
	`)
	codegentest.AssertContains(t, ir, "Box[int].clone")
	// Return type is also Box[int] — constructor call should appear
	codegentest.AssertContains(t, ir, "Box[int]")
}

// TestMonoSynthesizedDefaultOnGenericType verifies that a generic concrete type
// implementing a structural interface inherits the interface's default methods,
// and that those methods are emitted with the mono-qualified name.
func TestMonoSynthesizedDefaultOnGenericType(t *testing.T) {
	// Use a structural interface whose default method doesn't require operations
	// on T — just calls another abstract method.
	ir := codegentest.GenerateIR(t, `
		type Sized `+"`"+`structural {
			size() int `+"`"+`abstract;
			nonempty() bool => this.size() > 0;
		}
		type Pair[T] is Sized {
			T a;
			T b;
			size() int { return 2; }
		}
		main() {
			p := Pair[int](a: 1, b: 2);
			bool r = p.nonempty();
		}
	`)
	// The synthesized nonempty default should appear with the mono-qualified name
	codegentest.AssertContains(t, ir, "Pair[int].nonempty")
	// The concrete size method should also appear
	codegentest.AssertContains(t, ir, "Pair[int].size")
}

// TestMonoMapWithTupleValue is a regression test for T0400: instantiating
// Map[K, V] with a tuple V used to panic in codegen because the mono spiral
// guard over-marked Vector[(K, V)] as spiral, preventing _FnIter[T] from
// being resolved during Vector.iter()'s body monomorphization. After the
// originWrapsTypeParams precondition was added, Vector — which doesn't
// intrinsically wrap its TypeParam in a Tuple — is correctly skipped from
// spiral marking, letting the chain bound at Iterator/_FnIter as intended.
func TestMonoMapWithTupleValue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := map[string, (string, int)]();
			m["a"] = ("alpha", 1);
		}
	`)
	// _FnIter[(string, (string, int))] must be monomorphized (so its layout
	// exists) — this is the instance whose missing layout caused the panic.
	codegentest.AssertContains(t, ir, "_FnIter[(string, (string, int))]")
	// Vector and Iterator instances for the tuple value must also exist.
	codegentest.AssertContains(t, ir, "Vector[(string, (string, int))]")
}

// TestGenericFuncWithGenericReturnType verifies a generic function that both
// takes and returns a monomorphic generic type. Box[int] is instantiated directly
// in main so its layout is collected; the generic function takes and returns it.
func TestGenericFuncWithGenericReturnType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T val; }
		identity_box[T](Box[T] b) Box[T] { return b; }
		main() {
			b := Box[int](val: 42);
			c := identity_box[int](b);
		}
	`)
	codegentest.AssertContains(t, ir, "identity_box[int]")
	codegentest.AssertContains(t, ir, "Box[int]")
}

// TestGenericTypeInfoEmitted verifies that RTTI typeinfo and vtable globals are
// emitted for monomorphic generic type instantiations. promise_type_is requires
// these globals at runtime to check inheritance relationships.
func TestGenericTypeInfoEmitted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal[T] {
			speak() T `+"`"+`abstract;
		}
		type Dog[T] is Animal[T] {
			T sound;
			speak() T { return this.sound; }
		}
		main() {
			Dog[int] d = Dog[int](sound: 1);
			Animal[int] a = d;
		}
	`)
	// Mono typeinfo and vtable globals must be emitted for Dog[int].
	codegentest.AssertContains(t, ir, "promise_typeinfo_Dog[int]")
	codegentest.AssertContains(t, ir, "promise_vtable_Dog[int]")
	// Animal[int] typeinfo must also be emitted (it's an abstract parent).
	codegentest.AssertContains(t, ir, "promise_typeinfo_Animal[int]")
}

func TestInstanceIRsNilWhenNoGenerics(t *testing.T) {
	// Non-generic code produces no instance IRs.
	file, info := codegentest.ParseWithStd(t, `
		type Foo { int x; }
		main() { f := Foo(x: 1); }
	`)
	result := codegen.Compile(file, info, "")
	instIRs := result.InstanceIRs()
	// May be nil or empty — either is acceptable.
	for name := range instIRs {
		// User types are not generic, so no user-defined instances expected.
		// (Std library instances like _FnIter may appear from iterator infrastructure;
		// this check is intentionally not exhaustive.)
		_ = name
	}
}

func TestIsDestructureGenericEnumCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Option[T] { Some(T value), None }
		main() {
			Option[int] opt = Option[int].Some(value: 42);
			if opt is Some(val) {
				print_line("{val}");
			}
		}
	`)
	// Should have the destructure blocks for the monomorphized enum
	codegentest.AssertContains(t, ir, "isdestr.then")
	codegentest.AssertContains(t, ir, "icmp eq i32")
}

// B0112: destructure is-pattern inside generic method body must apply typeSubst
func TestIsDestructureInGenericMethodBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Option[T] { Some(T value), None }
		type Wrapper[T] {
			Option[T] opt;
			unwrap_or(this, T default_val) T {
				if this.opt is Some(val) {
					return val;
				}
				return default_val;
			}
		}
		main() {
			w := Wrapper[int](opt: Option[int].Some(value: 42));
			int result = w.unwrap_or(0);
		}
	`)
	// The monomorphized method should have destructure blocks
	codegentest.AssertContains(t, ir, "isdestr.then")
	// Should have the tag comparison for the monomorphized enum Option__int
	codegentest.AssertContains(t, ir, "icmp eq i32")
}

// T1171 generic/monomorphized path: when the Array[heap-user] payload lives in a
// GENERIC enum (`GBox[T]` with a `T[2]` variant field), the escape sink must
// resolve the element type through c.typeSubst (T -> Row) in BOTH
// dupBorrowedHeapUserPayload (t = Substitute(...)) and arrayElemNeedsEscapeDup
// (elem = Substitute(arr.Elem(), ...)). Without those substitutions the array
// recognizer misses the shape and the escaped aggregate aliases the moved-in
// subject's payload (UAF). This is the only test that exercises the typeSubst
// branches of both helpers, so the monomorphized `gesc[Row]` must still emit the
// per-element `heapdup.copy`.
func TestT1171GenericArrayHeapUserPayloadEscapeDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Row { string name; }
		enum GBox[T] { Some(T[2] value), Empty }
		gesc[T](GBox[T] move b, T[2] fb) T[2] {
			if b is Some(value) { return value; }
			return fb;
		}
		main() {
			GBox[Row] b = GBox[Row].Some(value: [Row(name: "a" + "b"), Row(name: "c" + "d")]);
			Row[2] fb = [Row(name: "x"), Row(name: "y")];
			r := gesc(move b, fb);
		}
	`)
	// Monomorphized generic funcs are emitted as @"gesc[Row]" (quoted name).
	fn := codegentest.ExtractFunction(ir, `"gesc[Row]"`)
	if fn == "" {
		t.Fatalf("monomorphized gesc[Row] not found in IR:\n%s", ir)
	}
	codegentest.AssertContains(t, fn, "heapdup.copy")
	if n := strings.Count(fn, "insertvalue"); n < 2 {
		t.Fatalf("expected >= 2 insertvalue (one per array element), got %d\n%s", n, fn)
	}
}

// T1176: escaping a generic array field whose element is a type parameter
// (`T[2]`) from inside a GENERIC function body must also deep-clone. Because the
// escape sits in `grab[T]`'s body, the field type is the unresolved `T[2]` and
// mono has `typeSubst` active (T→Row) at the access — this drives
// arrayElemNeedsEscapeDup's `types.Substitute` branch, which the concrete-instance
// cases (field type already `Row[2]`, typeSubst nil) never reach. The
// monomorphized `grab__Row` still clones each escaping element. `h` is borrowed,
// so its synth drop runs in the caller while the returned copy must stay valid.
func TestT1176GenericArrayHeapUserFieldEscapeDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Row { string name; }
		type Holder[T] { T[2] data; }
		grab[T](Holder[T] h) T[2] { return h.data; }
		main() {
			Holder[Row] h = Holder[Row](data: [Row(name: "de" + "ep"), Row(name: "x" + "x")]);
			r := grab[Row](h);
		}
	`)
	// Mono generic funcs are emitted with a bracketed, quoted LLVM name
	// (`@"grab[Row]"`), so the extract marker must include the quotes.
	fn := codegentest.ExtractFunction(ir, `"grab[Row]"`)
	codegentest.AssertContains(t, fn, "heapdup.copy")
}

// T1169: a GENERIC subtype whose field type is the type parameter `T`,
// destructured as a concrete instance `Named[string]` in a plain function. This
// exercises bindIsDestructureNamed's targetType.(*types.Instance) local-subst
// branch (BuildSubstMap over the instance's own type args) — the field must
// resolve T → string so the droppable heap field is deep-cloned on escape.
func TestT1169IfIsDestructureNamedGenericInstanceFieldDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shape { }
		type Named[T] is Shape { T label; }
		make() string {
			Shape s = Named[string](label: "a" + "b");
			if s is Named[string](label) { return label; }
			return "";
		}
		main() { s := make(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.make")
	codegentest.AssertContains(t, fn, "strdup.copy")
	codegentest.AssertContains(t, fn, "label.dropflag")
}

// --- Generic is-pattern tests (B0012) ---

func TestIsGenericType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		type LabeledBox[T] is Box[T] { string label; }
		main() {
			Box[int] b = LabeledBox[int](value: 42, label: "x");
			bool x = b is LabeledBox[int];
		}
	`)
	// Should generate mono typeinfo for the generic instance
	codegentest.AssertContains(t, ir, "promise_typeinfo_LabeledBox")
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is")
}

func TestIsGenericTypeBaseClass(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		type LabeledBox[T] is Box[T] { string label; }
		main() {
			Box[int] b = LabeledBox[int](value: 42, label: "x");
			bool x = b is Box[int];
		}
	`)
	// Should have mono typeinfo for both instances
	codegentest.AssertContains(t, ir, "promise_typeinfo_Box")
	codegentest.AssertContains(t, ir, "promise_typeinfo_LabeledBox")
}

func TestIsGenericTypeOptional(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		type LabeledBox[T] is Box[T] { string label; }
		main() {
			Box[int] lb = LabeledBox[int](value: 42, label: "x");
			Box[int]? opt = lb;
			bool x = opt is LabeledBox[int];
		}
	`)
	// Optional generic is-check: should branch on presence then RTTI
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is")
	codegentest.AssertContains(t, ir, "phi i1")
}

// --- Type Argument Inference Codegen Tests ---

func TestInferGenericFuncCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			int v = identity(42);
		}
	`)
	// Monomorphized function should be generated for int
	codegentest.AssertContains(t, ir, "define i64 @\"identity[int]\"")
}

func TestInferGenericFuncTwoTypeParamsCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		first[A, B](A a, B b) A { return a; }
		main() {
			int v = first(1, "hello");
		}
	`)
	codegentest.AssertContains(t, ir, "@\"first[int, string]\"")
}

func TestInferConstructorCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box(value: 42);
			int v = b.value;
		}
	`)
	// Should produce Box[int] instance struct
	codegentest.AssertContains(t, ir, "Box[int]_i")
}

func TestInferGenericFuncWithVectorParamCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		first_elem[T](T[] items) T {
			return items[0];
		}
		main() {
			int[] arr = [1, 2, 3];
			int v = first_elem(arr);
		}
	`)
	codegentest.AssertContains(t, ir, "@\"first_elem[int]\"")
}

// B0134 variant: generic type (non-error) constructed inside generic function body.
func TestGenericTypeInGenericFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper[T] { T value; }
		wrap[T](T v) Wrapper[T] {
			return Wrapper[T](value: v);
		}
		main() { w := wrap[int](42); }
	`)
	codegentest.AssertContains(t, ir, "Wrapper[int]")
}

// B0175: Heap temp claim in genInferredVarDecl — auto-typed iterator variable
func TestHeapTempClaimInInferredVarDecl(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter is Iterator[int] {
			int n;
			next(~this) int? {
				if this.n <= 0 { return none; }
				this.n = this.n - 1;
				return this.n;
			}
		}
		test() {
			c := Counter(n: 5);
			result := c.take(3);
			int sum = 0;
			for x in result {
				sum = sum + x;
			}
		}
		main() {}
	`)
	// The auto-typed `result := c.take(3)` must generate a heap.claim block
	// to prevent the iterator instance from being freed at statement end.
	codegentest.AssertContains(t, ir, "heap.claim")
}

// T0392: Synth drop must call the mono'd drop method for generic heap-user-type
// inner — Box[int].drop, not Box.drop.
func TestSynthDropOptionalGenericInnerUsesMonoName(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0392GBox[T] { T val; drop(~this) {} }
		type T0392GHolder[T] { T0392GBox[T]? data; }
		main() {
			h := T0392GHolder[int](data: T0392GBox[int](val: 7));
		}
	`)
	holderDrop := codegentest.ExtractFunction(ir, `"T0392GHolder[int].drop"`)
	if holderDrop == "" {
		t.Fatal("expected T0392GHolder[int].drop in IR")
	}
	// The mono'd inner drop must be called by name (not Box.drop).
	codegentest.AssertContains(t, holderDrop, `call void @"T0392GBox[int].drop"`)
}

// T0415: emitFieldDrops must use the mono'd drop name for non-optional generic
// instance fields with explicit drop. Before the fix, the lookup used the
// origin name "Box.drop" which doesn't exist — the user's drop body was
// silently skipped, leaking heap content inside the field.
func TestFieldDropUsesMonoNameForGenericExplicitDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0415Box[T] { T val; drop(~this) {} }
		type T0415Holder[T] { T0415Box[T] data; }
		main() {
			h := T0415Holder[int](data: T0415Box[int](val: 7));
		}
	`)
	holderDrop := codegentest.ExtractFunction(ir, `"T0415Holder[int].drop"`)
	if holderDrop == "" {
		t.Fatal("expected T0415Holder[int].drop in IR")
	}
	// The mono'd inner drop must be called by name.
	codegentest.AssertContains(t, holderDrop, `call void @"T0415Box[int].drop"`)
	// And NOT the origin name (which is the bug shape).
	codegentest.AssertNotContains(t, holderDrop, "call void @T0415Box.drop")
}

// T0415: emitOptionalFieldReassignDrop must use the mono'd drop name when
// reassigning an optional generic field whose inner has explicit drop.
func TestOptionalFieldReassignDropUsesMonoName(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0415Box2[T] { T val; drop(~this) {} }
		type T0415Holder2[T] { T0415Box2[T]? data; }
		main() {
			h := T0415Holder2[int](data: T0415Box2[int](val: 1));
			h.data = T0415Box2[int](val: 2);
		}
	`)
	// The reassignment site lives in the user's main goroutine body.
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	mainFn := rest[:end+2]
	codegentest.AssertContains(t, mainFn, "field.optdrop")
	codegentest.AssertContains(t, mainFn, `call void @"T0415Box2[int].drop"`)
	codegentest.AssertNotContains(t, mainFn, "call void @T0415Box2.drop")
}

// T0415: emitOptionalFieldReassignDrop must also handle the synth-drop-only
// path — generic types with no explicit drop where the type argument resolves
// to a heap type at mono time. The drop call must use the mono name and the
// optdrop block must NOT call pal_free (the synth drop already pal_frees).
// Before the fix, the drop call was skipped entirely (HasDrop=false,
// NeedsSynthDrop=false) and pal_free was called directly, leaking the inner
// heap content. The mono'd synth drop function exists either way (it's used by
// the holder's own drop at scope exit) — to detect the regression we must
// inspect the field.optdrop.free block specifically, not just the whole main.
func TestOptionalFieldReassignDropMonoSynthSkipsExtraFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0415RawBox[T] { T val; }
		type T0415RawHolder[T] { T0415RawBox[T]? data; }
		main() {
			h := T0415RawHolder[string](data: T0415RawBox[string](val: "a"));
			h.data = T0415RawBox[string](val: "b");
		}
	`)
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	mainFn := rest[:end+2]
	// Isolate the field.optdrop.free block — content between the label line
	// and the next blank line. emitOptionalFieldReassignDrop produces this
	// label only when handling the reassignment.
	freeLabel := "\nfield.optdrop.free"
	freeStart := strings.Index(mainFn, freeLabel)
	if freeStart < 0 {
		t.Fatal("expected field.optdrop.free block in main")
	}
	// Skip past the label line.
	blockStart := strings.Index(mainFn[freeStart+1:], "\n") + freeStart + 2
	blockEnd := strings.Index(mainFn[blockStart:], "\n\n")
	if blockEnd < 0 {
		t.Fatal("expected end of field.optdrop.free block")
	}
	freeBlock := mainFn[blockStart : blockStart+blockEnd]
	// The reassignment must invoke the mono'd synth drop INSIDE the optdrop
	// free block (not just somewhere else in main).
	codegentest.AssertContains(t, freeBlock, `call void @"T0415RawBox[string].drop"`)
	// And must NOT call pal_free here — the synth drop already pal_freed.
	codegentest.AssertNotContains(t, freeBlock, "call void @pal_free")
	// Confirm test premise: the mono'd synth drop itself does both the
	// inner string drop and the pal_free of the box instance.
	synthDrop := codegentest.ExtractFunction(ir, `"T0415RawBox[string].drop"`)
	if synthDrop == "" {
		t.Fatal("expected T0415RawBox[string].drop in IR")
	}
	codegentest.AssertContains(t, synthDrop, "call void @promise_string_drop")
	codegentest.AssertContains(t, synthDrop, "call void @pal_free")
}

// T1073: force-unwrap inside a collection literal in a *generic* function body —
// `wrap[T](T? move o) T[] { return [o!]; }` instantiated with a droppable heap
// type — must neutralize the source. Exercises the typeSubst substitution path
// in neutralizeForceUnwrapElem (the element type is resolved through the active
// monomorphization substitution before the typeNeedsFieldDrop gate).
func TestT1073GenericContextForceUnwrapNeutralizesSource(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		wrap[T](T? move o) T[] { return [o!]; }
		main() {
			T1073Box b = T1073Box(name: "g");
			T1073Box? o = b;
			T1073Box[] v = wrap[T1073Box](move o);
		}
	`)
	// The monomorphized body @"wrap[T1073Box]" must carry the present-flag clear.
	fn := codegentest.ExtractFunction(ir, `"wrap[T1073Box]"`)
	if fn == "" {
		t.Fatal("expected wrap[T1073Box] mono body in IR")
	}
	codegentest.AssertContains(t, fn, codegentest.T1073NeutralizeSig)
}

// B0222: Generic combinator chain result stored in variable — intermediate iterators
// must be promoted to scope bindings (freed at scope exit, not statement end).
func TestGenericCombinatorInVariable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int[] v = [1, 2, 3];
			Iterator[int] it = v.iter().map[int](|int x| -> int { return x * 2; });
		}
	`)
	// B0222: Intermediate heapTemps promoted to scope bindings should produce
	// free.call blocks (scope-level cleanup) instead of heap.drop blocks
	// (statement-level cleanup) for the intermediate _FnIter.
	codegentest.AssertContains(t, ir, "free.call")
	codegentest.AssertContains(t, ir, "__promise_iter_cleanup")
}

// B0226: Inferred optional declaration should register optional drop.
func TestInferredOptionalDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box {
			int value;
			new(~this, int v) { this.value = v; }
			try_make(int v, bool ok) Self? `+"`factory"+` {
				if !ok { return none; }
				return Self(v: v);
			}
		}
		main() {
			r := Box.try_make(v: 10, ok: true);
		}
	`)
	// B0226: Inferred optional should register drop (optdrop block)
	codegentest.AssertContains(t, ir, "optdrop")
}

// T0394: Reassigning a generic Optional[string] field with a heap RHS must
// claim the inner string temp BEFORE wrapping in Optional. Without the
// pre-wrap claim, the post-wrap claimStringTemp lookup uses value-identity
// against the wrapped {i1, i8*} struct and never matches the inner i8* temp,
// leaving the temp drop active so the field aliases a freed pointer.
//
// The fix mirrors the T0111 pattern in the parallel local-var (IdentExpr)
// and var-decl branches. We assert the drop-flag clear-before-wrap shape:
// `store i1 false` to the temp's drop flag must appear BEFORE the
// `insertvalue { i1, i8* }` that builds the wrapped Optional.
func TestOptionalGenericFieldReassignClaimsStringTempBeforeWrap(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[string] b = Box[string](value: "init");
			b.value = (1).to_string();
		}
	`)
	// Reassign-drop block must exist (T0390).
	codegentest.AssertContains(t, ir, "field.optdrop")
	// The post-store stmt-temp drop block exists for tracked temps.
	codegentest.AssertContains(t, ir, "tmp.drop")
	// promise_string_drop is reachable but must be guarded by the temp drop
	// flag. With the fix, the flag is cleared before the wrap, so on the hot
	// path the drop block resolves to the no-op (skip) branch.
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

// T0394 (vector limb): the predicate also covers types.IsVector(exprType).
// Reassigning a generic Optional[Vector[int]] field with a heap-allocated
// Vector RHS must emit the reassign-drop block for the OLD field value and
// the temp-drop guard block for the NEW value, with Vector.drop reachable
// for both.
func TestOptionalGenericFieldReassignVectorEmitsDropAndOptdrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[Vector[int]] b = Box[Vector[int]](value: [1, 2, 3]);
			b.value = [4, 5, 6];
		}
	`)
	// T0390 reassign-drop block for the OLD field value.
	codegentest.AssertContains(t, ir, "field.optdrop")
	// Stmt-temp drop block for tracked heap temps (covers the Vector case).
	codegentest.AssertContains(t, ir, "tmp.drop")
	// Vector.drop is generic — operates on i8*, not monomorphised per-T.
	codegentest.AssertContains(t, ir, "@Vector.drop")
}

// T0513: Force-unwrap of an Optional[string] field on a generic-type instance
// (e.g. Box[string]) must dup the inner string. Sema's fieldTypeHasDrop returns
// false for T? where T is a TypeParam, so the bare Named's HasDrop()=false; the
// mono instance Box[string] gets synth drop via monoInstNeedsSynthDrop. Without
// the dup, the field and the new var alias the same heap pointer — at scope
// end one drops the pointer; the next reassignment frees again -> invalid free.
func TestGenericOptionalStringFieldUnwrapDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[string] b = Box[string](value: "init");
			string s = b.value!;
		}
	`)
	// dupStringFieldAccess mechanism must emit strdup block + promise_string_new.
	codegentest.AssertContains(t, ir, "strdup.copy")
	codegentest.AssertContains(t, ir, "promise_string_new")
}

// T0513 (vector limb): same fix must apply to generic Optional[Vector[T]] field
// unwrap — the inner Vector buffer must be duped on read so the field and the
// new variable own independent copies.
func TestGenericOptionalVectorFieldUnwrapDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[Vector[int]] b = Box[Vector[int]](value: [1, 2, 3]);
			Vector[int] v = b.value!;
		}
	`)
	// dupContainerFieldAccess emits a vecdup block (alloc + memcpy) for the dup.
	codegentest.AssertContains(t, ir, "vecdup.copy")
	codegentest.AssertContains(t, ir, "memcpy")
}

// T0513 (direct string field on generic owner): reading a plain `T` field
// from `Box[string]` must dup the string when bound to a new variable.
// Without the Instance-local TypeArgs substitution (added in T0513), the dup
// check sees the raw TypeParam and skips; without the ownerHasOrSynthDrop
// gate the bare Named has HasDrop=false (sema's fieldTypeHasDrop returns
// false for TypeParam) and the dup is skipped entirely.
func TestGenericDirectStringFieldReadDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		test() {
			Box[string] b = Box[string](value: "hi");
			string s = b.value;
		}
	`)
	// Scope the assertion to the user's test() function — the broader IR
	// contains many stdlib strdup.copy blocks unrelated to this fix.
	testFn := codegentest.ExtractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	codegentest.AssertContains(t, testFn, "strdup.copy")
	codegentest.AssertContains(t, testFn, "promise_string_new")
}

// T0746: a generic method that returns a `this`-owned string field by value
// must dup it on return (clone-on-return) so the owner's field-drop and the
// returned value's drop don't free the same allocation. The VarDecl-site dup
// is covered by TestGenericDirectStringFieldReadDups; this covers the
// method-return site (genReturnStmt -> setDupFlagsForFieldAccess ->
// genFieldAccess with `this` as the target, under c.typeSubst {T->string}).
func TestGenericMethodReturnStringFieldDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type GBox[T] { T val; peek(this) T { return this.val; } }
		main() { b := GBox[string](val: "hi"); s := b.peek(); }
	`)
	fn := codegentest.ExtractFunction(ir, `"GBox[string].peek"`)
	if fn == "" {
		t.Fatal("expected GBox[string].peek in IR")
	}
	codegentest.AssertContains(t, fn, "strdup.copy")
	codegentest.AssertContains(t, fn, "promise_string_new")
}

// T0746 (`this` receiver form): the bug reported the borrowed-receiver
// variant double-freed identically, so the dup-on-return must fire there too.
func TestGenericMethodReturnStringFieldDupsBorrowedReceiver(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type GBox[T] { T val; peek(this) T { return this.val; } }
		main() { b := GBox[string](val: "hi"); s := b.peek(); }
	`)
	fn := codegentest.ExtractFunction(ir, `"GBox[string].peek"`)
	if fn == "" {
		t.Fatal("expected GBox[string].peek in IR")
	}
	codegentest.AssertContains(t, fn, "strdup.copy")
	codegentest.AssertContains(t, fn, "promise_string_new")
}

// T0746 (generic getter form): a getter returning a `this`-owned string field
// through the type parameter is a distinct codegen path from a method (getter
// vs method dispatch), but the return-by-value dup must fire identically.
func TestGenericGetterReturnStringFieldDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type GBox[T] { T val; get field T { return this.val; } }
		main() { b := GBox[string](val: "hi"); s := b.field; }
	`)
	fn := codegentest.ExtractFunction(ir, `"GBox[string].field"`)
	if fn == "" {
		t.Fatal("expected GBox[string].field in IR")
	}
	codegentest.AssertContains(t, fn, "strdup.copy")
	codegentest.AssertContains(t, fn, "promise_string_new")
}

// T0513 (maybeEnableDupForMutRefArg generic owner): passing a generic
// owner's field to a `~` (consuming) param must auto-dup the field so the
// callee's consume-drop and the owner's drop don't double-free. Exercises
// the MemberExpr branch of maybeEnableDupForMutRefArg (expr.go:5094-5109),
// which had zero coverage before T0513 added a test for the generic-owner
// gate.
func TestGenericOwnerMutRefArgDupsStringField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		consume(string move s) {}
		test() {
			b := Box[string](value: "hi");
			consume(b.value);
		}
	`)
	testFn := codegentest.ExtractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	// dupStringFieldAccess + strdup.copy emitted at the field read site,
	// guarded by the ownerHasOrSynthDrop generic-owner gate.
	codegentest.AssertContains(t, testFn, "strdup.copy")
	codegentest.AssertContains(t, testFn, "promise_string_new")
}

// T0513 (maybeEnableDupForMutRefArg generic owner — Vector limb): same
// auto-dup must apply when the field type substitutes to a Vector and is
// passed to a consuming param. dupContainerFieldAccess routes through
// dupVector which emits a vecdup.copy block.
func TestGenericOwnerMutRefArgDupsVectorField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		consume(Vector[int] move v) {}
		test() {
			b := Box[Vector[int]](value: [1, 2, 3]);
			consume(b.value);
		}
	`)
	testFn := codegentest.ExtractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	codegentest.AssertContains(t, testFn, "vecdup.copy")
}

// T0513 (maybeEnableDupForConstructorArg generic owner): constructor
// field-init that reads from a generic owner's field must auto-dup so the
// new instance owns an independent copy. Mirrors T0411 (non-generic owner)
// for generic-owner instances; without ownerHasOrSynthDrop, the early
// return at expr.go:5129 would skip the dup setup.
func TestGenericOwnerConstructorArgDupsStringField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		type Holder { string s; drop(~this) {} }
		test() {
			b := Box[string](value: "hi");
			h := Holder(s: b.value);
		}
	`)
	testFn := codegentest.ExtractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	codegentest.AssertContains(t, testFn, "strdup.copy")
	codegentest.AssertContains(t, testFn, "promise_string_new")
}

// B0210: Optional[TypeParam] field with none value should not cause mono layout mismatch.
// The mono layout computes the correct LLVM type for the optional field, but the none
// value was generated using an unsubstituted TypeParam, producing a type mismatch.
func TestOptionalTypeParamFieldNone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[int](val: none);
		}
	`)
	// The optional field should use the correct substituted type (i64 for int)
	codegentest.AssertContains(t, ir, "{ i1, i64 }")
}

// B0210: Optional[TypeParam] field with a concrete value should work too.
func TestOptionalTypeParamFieldValue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[string](val: "hello");
		}
	`)
	codegentest.AssertContains(t, ir, "{ i1, i8* }")
}

// B0210: Multiple Optional[TypeParam] fields with different instantiations.
func TestOptionalTypeParamMultipleInstantiations(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m1 := MaybeVal[int](val: none);
			m2 := MaybeVal[string](val: none);
		}
	`)
	// Both int? and string? layouts should be present
	codegentest.AssertContains(t, ir, "{ i1, i64 }")
	codegentest.AssertContains(t, ir, "{ i1, i8* }")
}

// T0428: Generic type with borrowed this.field! exercises the typeSubst branches
// in genOptionalForceUnwrap (Case 3B with typeSubst != nil).
func TestT0428GenericBorrowedThisForceUnwrapTypeSubst(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type GenHolder[T] {
			T? data;
			get_val(this) T {
				return this.data!;
			}
		}
		type GBox { int n; drop(~this) {} }
		main() {
			h := GenHolder[GBox](data: GBox(n: 42));
			v := h.get_val();
		}
	`)
	// get_val must dup the heap value for the borrowed receiver case.
	// The function is LLVM-quoted as @"GenHolder[GBox].get_val".
	codegentest.AssertContains(t, ir, `"GenHolder[GBox].get_val"`)
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc")
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy")
}

// T0428: Generic function with local var force-unwrap exercises typeSubst path
// in neutralizeMemberOptionalField (IdentExpr root, lines 9052-9054).
// When a generic method/function body has `b.field!` where b's type has a TypeParam,
// c.typeSubst is applied to resolve the concrete owner type.
func TestT0428GenericFuncLocalVarOptFieldNeutralization(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0428ContainerBox { int n; drop(~this) {} }
		type T0428Container[T] {
			T? item;
		}
		unwrap_item[T](T0428Container[T] c) T {
			return c.item!;
		}
		main() {
			c := T0428Container[T0428ContainerBox](item: T0428ContainerBox(n: 5));
			b := unwrap_item[T0428ContainerBox](c);
		}
	`)
	// The concrete monomorphized function must clear the optional present flag.
	codegentest.AssertContains(t, ir, "store i1 false")
}

// T0419: Optional[GenericBox[int]] with explicit drop must dispatch through
// the mono-mangled $wrap (e.g. GenericBoxD[int].drop$wrap).
func TestOptionalLocalDropExplicitUserDropWrapMono(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type GenericBoxD[T] {
			T val;
			drop(~this) {}
		}
		test_mono_no_unwrap() {
			GenericBoxD[int]? a = GenericBoxD[int](val: 5);
		}
	`)
	// The mono Optional drop dispatch must call the mono $wrap variant.
	codegentest.AssertContains(t, ir, `@"GenericBoxD[int].drop$wrap"`)
	wrapFn := codegentest.ExtractFunction(ir, `"GenericBoxD[int].drop$wrap"`)
	if wrapFn == "" {
		t.Fatal(`expected "GenericBoxD[int].drop$wrap" in IR`)
	}
	codegentest.AssertContains(t, wrapFn, `call void @"GenericBoxD[int].drop"`)
	codegentest.AssertContains(t, wrapFn, "call void @pal_free")
}

// T0847 (generic/mono variant): the constructor field-init dup-on-read must
// fire under monomorphization, where the arg/target types are TypeParams that
// only resolve after substitution. This exercises maybeEnableDupForConstructorArg's
// typeSubst branches (argType/targetType Substitute) — `Holder[T](held: v[0])`
// inside a generic function body where T=Item is bound by the mono context.
func TestT0847_ConstructorGenericVectorElementDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Item { string label; drop(~this) {} }
		type Holder[T] { T held; drop(~this) {} }
		make_holder[T](Vector[T] v) Holder[T] {
			return Holder[T](held: v[0]);
		}
		test_t0847_generic() {
			v := Item[]();
			v.push(Item(label: "x"));
			h := make_holder[Item](v);
		}
	`)
	// Under mono (T=Item), the dup still fires: allocate + memcpy the element.
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc")
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy")
}

func TestParenThisGenericMethodNoExtractFromPtr(t *testing.T) {
	// T0746: a droppable (string) payload exercises the generic-method
	// return-by-value dup path in addition to the (this).peek() dispatch gate.
	ir := codegentest.GenerateIR(t, `
		type T0613GenBox[T] {
			T val;
			peek(this) T { return this.val; }
			via(this) T { return (this).peek(); }
		}
		main() { b := T0613GenBox[string](val: "hi"); v := b.via(); }
	`)
	codegentest.AssertNotContains(t, ir, "extractvalue i8*")
	// The monomorphized instance method body is emitted into the (unsplit) module.
	codegentest.AssertContains(t, ir, "T0613GenBox[string].via")
}

// genGenericEnumMethodCall gate: a generic enum routes method calls through a
// dedicated path distinct from the non-generic enum method gate.
func TestParenThisGenericEnumMethodNoExtractFromPtr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum T0613GOpt[T] {
			Some(T value), None,
			has(this) bool {
				match this { T0613GOpt.Some(v) => { return true; }, T0613GOpt.None => { return false; } }
			}
			has_via(this) bool { return (this).has(); }
		}
		main() {
			s := T0613GOpt[int].Some(5);
			r := s.has_via();
		}
	`)
	codegentest.AssertNotContains(t, ir, "extractvalue i8*")
	codegentest.AssertContains(t, ir, "T0613GOpt[int].has_via")
}

// genIsResolvedType gate: `(this) is IBox[int]` resolves to a concrete generic
// instance and routes through genIsResolvedType (distinct from the non-generic
// genIsNamedType gate covered by TestParenThisIsCheck...).
func TestParenThisIsGenericNoExtractFromPtr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0613GShape {
			area(this) int `+"`abstract"+`;
			is_intbox(this) bool { return (this) is T0613IBox[int]; }
		}
		type T0613IBox[T] is T0613GShape {
			T value;
			area(this) int { return 1; }
		}
		main() {
			T0613GShape s = T0613IBox[int](value: 5);
			b := s.is_intbox();
		}
	`)
	codegentest.AssertNotContains(t, ir, "extractvalue i8*")
	if codegentest.ExtractFunction(ir, "T0613GShape.is_intbox") == "" {
		t.Fatal("expected T0613GShape.is_intbox in IR")
	}
}

// T1011: a GENERIC enum's narrowed heap variant field escaping the scope exercises
// the typeSubst substitution in genNarrowedVariantField (both targetType and the
// field type) and narrowedVariantFieldDroppable — the substituted field type is
// `string`, so the escape must still clone.
func TestEnumNarrowGenericVariantStringFieldEscapeDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Opt[T] { Some(T val), None }
		grab() string {
			Opt[string] o = Opt[string].Some(val: "a");
			if o is Some { return o.val; }
			return "";
		}
		main() { string s = grab(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	codegentest.AssertContains(t, fn, "strdup.copy")
}

// T1011: a GENERIC FUNCTION body that narrows an enum and escapes a heap variant
// field exercises the typeSubst substitution in genNarrowedVariantField — the
// narrowing TargetType and FieldType carry the function's TypeParam, so they must
// be substituted before dupHeapFieldForEscape runs. Monomorphized for T=string the
// escape must clone; for T=int (non-heap) it must not. This is the path the
// concrete-Opt[string] test above does NOT reach (there typeSubst is nil because
// sema already resolved the field type to a concrete `string`).
func TestEnumNarrowGenericFnBodyVariantFieldEscapeDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Opt[T] { Some(T val), None }
		extract[T](Opt[T] o, T fallback) T {
			if o is Some { return o.val; }
			return fallback;
		}
		main() {
			Opt[string] os = Opt[string].Some(val: "a");
			string s = extract[string](os, "");
			Opt[int] oi = Opt[int].Some(val: 1);
			int n = extract[int](oi, 0);
		}
	`)
	strFn := codegentest.ExtractFunction(ir, `"extract[string]"`)
	if strFn == "" {
		t.Fatal(`expected "extract[string]" mono instance in IR`)
	}
	codegentest.AssertContains(t, strFn, "strdup.copy")
	intFn := codegentest.ExtractFunction(ir, `"extract[int]"`)
	if intFn == "" {
		t.Fatal(`expected "extract[int]" mono instance in IR`)
	}
	codegentest.AssertNotContains(t, intFn, "strdup.copy")
}

// T1011: binding a narrowed heap variant field inside a GENERIC function body
// (`b := o.val`) routes through isStringFieldDup → narrowedVariantFieldDroppable
// with typeSubst active, so the substituted TargetType resolves to a droppable
// enum and the binding clones the payload (keeping it independent of the subject).
func TestEnumNarrowGenericFnBodyVariantFieldBoundCopyDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Opt[T] { Some(T val), None }
		first[T](Opt[T] o, T fallback) T {
			if o is Some { b := o.val; return b; }
			return fallback;
		}
		main() {
			Opt[string] os = Opt[string].Some(val: "a");
			string s = first[string](os, "");
		}
	`)
	fn := codegentest.ExtractFunction(ir, `"first[string]"`)
	if fn == "" {
		t.Fatal(`expected "first[string]" mono instance in IR`)
	}
	codegentest.AssertContains(t, fn, "strdup.copy")
}
