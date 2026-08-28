package inherit

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

func TestSuperCallImplicitParentCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal {
			int age;
		}
		type Dog is Animal {
			int tricks;
			new(~this, int age, int tricks) {
				super(age: age);
				this.tricks = tricks;
			}
		}
		main() {
			Dog d = Dog(age: 3, tricks: 5);
		}
	`)
	// Dog.new should be defined and set parent field directly (no Animal.new call)
	codegentest.AssertContains(t, ir, "define void @Dog.new(")
	// Dog constructor should call Dog.new
	codegentest.AssertContains(t, ir, "call void @Dog.new(")
}

// T0937: a value-struct-container elvis inside a structural-interface default
// method, with an owned-local source (orphaned on the some-path), is tracked.
// The default body is synthesized per concrete type with c.selfSubst active, so
// the result type passes through types.SubstituteSelf before the drop is
// resolved. Exercises the selfSubst branch of trackElvisResultHeap.
func TestElvisMapStructuralDefaultDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type HasMapFallback `+"`"+`structural {
			base_map() map[string, int] `+"`"+`abstract;
			resolved_len() int {
				map[string, int]? a = {"x": 1};
				return (a ?: this.base_map()).len;
			}
		}
		type MapConfig is HasMapFallback {
			base_map() map[string, int] { return {"k": 7}; }
		}
		main() {
			c := MapConfig();
			d := c.resolved_len();
		}
	`)
	codegentest.AssertContains(t, ir, "elvis.merge")
	// The none-incoming predecessor is the this.base_map() call continuation
	// block, not %elvis.none — so match only the some-path true incoming.
	codegentest.AssertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, `)
	codegentest.AssertContains(t, ir, `call void @"Map[string, int].drop"`)
}

// T0937 (i8*-container gap): a vector elvis inside a structural-interface default
// method with an owned-local source is orphaned → tracked. The default body is
// synthesized with c.selfSubst active. Exercises the selfSubst branch of
// trackElvisResultTemp.
func TestElvisStrvecStructuralDefaultDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type HasVecFallback `+"`"+`structural {
			base_vec() string[] `+"`"+`abstract;
			resolved_len() int {
				string[]? a = ["x" + "y"];
				return (a ?: this.base_vec()).len;
			}
		}
		type VecConfig is HasVecFallback {
			base_vec() string[] { return ["z" + "w"]; }
		}
		main() {
			c := VecConfig();
			d := c.resolved_len();
		}
	`)
	codegentest.AssertContains(t, ir, "elvis.merge")
	// The none-incoming predecessor is the this.base_vec() call continuation
	// block, not %elvis.none. The default is a FRESH temp (base_vec() returns a
	// new vector), so T0936 claims it and the result owns it on the none-path too:
	// the flag phi is [true, true] (the some-incoming `true` proves orphan tracking).
	codegentest.AssertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ true, `)
}

// --- Stage 8k: Inheritance Codegen Tests ---

func TestInheritedFieldLayout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal { string name; int age; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", age: 5, breed: "Lab");
		}
	`)
	// Dog instance struct should include parent fields: _variant, name, age, breed
	codegentest.AssertContains(t, ir, `%promise_Dog_i = type { %promise_Dog_m*, i8*, i64, i8* }`)
	// Animal instance struct: _variant, name, age
	codegentest.AssertContains(t, ir, `%promise_Animal_i = type { %promise_Animal_m*, i8*, i64 }`)
}

func TestInheritedFieldAccess(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal { string name; int age; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", age: 5, breed: "Lab");
			string n = d.name;
			int a = d.age;
			string b = d.breed;
		}
	`)
	// Field access should use GEP on Dog instance struct
	codegentest.AssertContains(t, ir, "getelementptr %promise_Dog_i")
}

func TestInheritedFieldConstructor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal { string name; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", breed: "Lab");
		}
	`)
	// Constructor should store values for both inherited and own fields
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")
	codegentest.AssertContains(t, ir, "getelementptr %promise_Dog_i")
}

func TestInheritedMethodCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal {
			string name;
			greet() string { return this.name; }
		}
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", breed: "Lab");
			string g = d.greet();
		}
	`)
	// d.greet() should dispatch to Animal.greet (inherited method)
	codegentest.AssertContains(t, ir, "call i8* @Animal.greet(i8*")
}

func TestDeepInheritance(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type A { int x; }
		type B is A { int y; }
		type C is B { int z; }
		main() {
			C c = C(x: 1, y: 2, z: 3);
			int a = c.x;
			int b = c.y;
			int d = c.z;
		}
	`)
	// C struct should have _variant, x, y, z (4 fields + internal = 4 GEP indices)
	codegentest.AssertContains(t, ir, "%promise_C_i = type { %promise_C_m*, i64, i64, i64 }")
}

func TestIsNamedTypeInheritance(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal { string name; }
		type Dog is Animal { string breed; }
		type Cat is Animal { }
		main() {
			Animal a = Dog(name: "Rex", breed: "Lab");
			bool isDog = a is Dog;
			bool isCat = a is Cat;
			bool isAnimal = a is Animal;
		}
	`)
	// All three checks should go through RTTI
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is")
	// Type info globals for all three types
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Dog")
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Cat")
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Animal")
}

func TestAsSafeCast(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal { string name; }
		type Dog is Animal { }
		main() {
			Animal a = Dog(name: "Rex");
			Dog? d = a as Dog;
		}
	`)
	// Should have RTTI check, then cast.some/cast.none/cast.merge blocks
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is")
	codegentest.AssertContains(t, ir, "cast.some.")
	codegentest.AssertContains(t, ir, "cast.none.")
	codegentest.AssertContains(t, ir, "cast.merge.")
}

func TestConstructorZeroInitInheritedField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal { string name; int age; }
		type Dog is Animal { string breed; }
		main() {
			Dog d = Dog(name: "Rex", age: 0, breed: "Lab");
		}
	`)
	// Constructor should store inherited fields
	codegentest.AssertContains(t, ir, "getelementptr %promise_Dog_i")
}

func TestDeepInheritanceMethodDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type A {
			int x;
			getX() int { return this.x; }
		}
		type B is A { int y; }
		type C is B { int z; }
		main() {
			C c = C(x: 1, y: 2, z: 3);
			int v = c.getX();
		}
	`)
	// c.getX() should resolve through C → B → A and call A.getX
	codegentest.AssertContains(t, ir, "call i64 @A.getX(i8*")
}

func TestRTTIMultipleParents(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Printable {
			show() string { return "printable"; }
		}
		type Serializable {
			encode() string `+"`structural(protocol: false)"+` { return "serializable"; }
		}
		type Doc is Printable, Serializable {
			string name;
		}
		main() {
			Doc d = Doc(name: "hi");
		}
	`)
	// Type info for Doc should include both parent IDs
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Doc")
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Printable")
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Serializable")
}

// --- VTable dispatch tests (Stage 8l) ---

func TestVtableGlobalEmitted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Dog d = Dog(name: "Rex");
		}
	`)
	// Both types have virtual methods, vtable globals should be emitted
	codegentest.AssertContains(t, ir, "@promise_vtable_Animal")
	codegentest.AssertContains(t, ir, "@promise_vtable_Dog")
}

func TestAbstractMethodVirtualDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal {
			string name;
			speak() string `+"`"+`abstract;
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Animal a = Dog(name: "Rex");
			string s = a.speak();
		}
	`)
	// Virtual dispatch: should NOT directly call @Animal.speak (abstract, doesn't exist)
	codegentest.AssertNotContains(t, ir, "call i8* @Animal.speak")
	// Should load function pointer from vtable (indirect call)
	codegentest.AssertContains(t, ir, "@promise_vtable_Animal")
	codegentest.AssertContains(t, ir, "@promise_vtable_Dog")
}

func TestConcreteOverrideVirtualDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Animal a = Dog(name: "Rex");
			string s = a.speak();
		}
	`)
	// When calling through Animal variable, should use vtable dispatch
	// (not direct call to Animal.speak)
	codegentest.AssertNotContains(t, ir, "call i8* @Animal.speak")
	// Vtable globals should exist for both types
	codegentest.AssertContains(t, ir, "@promise_vtable_Animal")
	codegentest.AssertContains(t, ir, "@promise_vtable_Dog")
}

func TestDirectDispatchPreserved(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Dog {
			string name;
			speak() string { return "woof"; }
		}
		main() {
			Dog d = Dog(name: "Rex");
			string s = d.speak();
		}
	`)
	// Dog has no children → direct dispatch, no vtable indirection
	codegentest.AssertContains(t, ir, "call i8* @Dog.speak")
}

func TestVirtualGetterDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shape {
			get area int `+"`"+`abstract;
		}
		type Circle is Shape {
			int radius;
			get area int => this.radius * this.radius;
		}
		main() {
			Shape s = Circle(radius: 5);
			int a = s.area;
		}
	`)
	// Getter through abstract parent should use vtable dispatch (indirect call)
	codegentest.AssertNotContains(t, ir, "call i64 @Shape.area")
	codegentest.AssertContains(t, ir, "@promise_vtable_Shape")
	codegentest.AssertContains(t, ir, "@promise_vtable_Circle")
}

func TestVirtualGetterOverrideDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base {
			int _x;
			get x int { return this._x; }
		}
		type Child is Base {
			get x int { return this._x * 2; }
		}
		main() {
			Base b = Child(_x: 5);
			int v = b.x;
		}
	`)
	// Concrete getter override through parent-typed variable should use vtable dispatch
	codegentest.AssertNotContains(t, ir, "call i64 @Base.x(")
	codegentest.AssertContains(t, ir, "@promise_vtable_Base")
	codegentest.AssertContains(t, ir, "@promise_vtable_Child")
}

func TestDirectGetterNoVtable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter {
			int _count;
			get count int { return this._count; }
		}
		main() {
			Counter c = Counter(_count: 10);
			int n = c.count;
		}
	`)
	// Counter has no children → direct getter call (not indirect through vtable)
	codegentest.AssertContains(t, ir, "call i64 @Counter.count")
}

func TestMultipleAbstractParentsVtable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		main() {
			Speakable s = Robot();
			string x = s.speak();
		}
	`)
	// Robot's vtable should cover both speak and move
	codegentest.AssertContains(t, ir, "@promise_vtable_Robot")
	codegentest.AssertContains(t, ir, "@promise_vtable_Speakable")
}

func TestDeepHierarchyVtable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type A {
			greet() string `+"`"+`abstract;
		}
		type B is A {
			greet() string { return "hello from B"; }
		}
		type C is B {
			greet() string { return "hello from C"; }
		}
		main() {
			A a = C();
			string s = a.greet();
		}
	`)
	// A→B→C chain: all get vtable globals
	codegentest.AssertContains(t, ir, "@promise_vtable_A")
	codegentest.AssertContains(t, ir, "@promise_vtable_B")
	codegentest.AssertContains(t, ir, "@promise_vtable_C")
	// Should NOT directly call @A.greet (abstract)
	codegentest.AssertNotContains(t, ir, "call i8* @A.greet")
}

func TestFirstParentPrefixCompatible(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal {
			string name;
			speak() string { return "..."; }
		}
		type Dog is Animal {
			speak() string { return "woof"; }
		}
		main() {
			Animal a = Dog(name: "Rex");
			string s = a.speak();
		}
	`)
	// Animal is first parent of Dog — no view vtable needed
	codegentest.AssertNotContains(t, ir, "@promise_vtable_Dog_as_Animal")
	// Dispatch through vtable from value struct (extractvalue, GEP, load, bitcast, call)
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestSecondParentViewVtable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		main() {
			Movable m = Robot();
		}
	`)
	// Movable is second parent of Robot — needs a view-specific vtable
	codegentest.AssertContains(t, ir, "@promise_vtable_Robot_as_Movable")
}

func TestMultiParentVtableDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		main() {
			Movable m = Robot();
			string s = m.walk();
		}
	`)
	// Should emit view vtable for Robot-as-Movable
	codegentest.AssertContains(t, ir, "@promise_vtable_Robot_as_Movable")
	// Dispatch should use vtable from value struct (not typeinfo chain)
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestConcreteDirectDispatchPreserved(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point {
			int x;
			int y;
			sum() int { return this.x + this.y; }
		}
		main() {
			Point p = Point(x: 1, y: 2);
			int s = p.sum();
		}
	`)
	// Concrete type with no parents that needs vtable — should use direct dispatch
	codegentest.AssertContains(t, ir, "call i64 @Point.sum")
}

func TestStructuralSatisfactionWithMeta(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Printable `+"`"+`structural {
			print() string `+"`"+`abstract;
		}
		type Doc {
			print() string { return "doc"; }
		}
		main() {
			Printable p = Doc();
			string s = p.print();
		}
	`)
	// Should emit view vtable for Doc-as-Printable (structural satisfaction)
	codegentest.AssertContains(t, ir, "@promise_vtable_Doc_as_Printable")
}

// --- Primitive → structural interface boxing tests ---
// These test the boxForStructuralView codegen path: when a primitive or string
// value is passed to a function parameter typed as a structural interface,
// the compiler must box it into a {vtable_ptr, instance_ptr} view struct.

func TestPrimitiveIntToStructuralView(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display(42); }
	`)
	// View vtable for int satisfying Showable
	codegentest.AssertContains(t, ir, "@promise_vtable_int_as_Showable")
	// Adapter thunk: int methods take i64 receiver, vtable passes i8*
	codegentest.AssertContains(t, ir, "int.to_string$view_adapt")
	// Boxing: insertvalue to build the {i8*, i8*} view struct
	codegentest.AssertContains(t, ir, "insertvalue { i8*, i8* }")
	// T1276: the primitive box is HEAP-allocated { i8* typeinfo, scalar } (not a
	// stack alloca) so the escaping interface fat pointer stays valid; field 0
	// carries a null-drop typeinfo header.
	// T1284: the header is a per-size flat-box typeinfo whose clone_fn (field 2) is
	// a flat malloc+memcpy clone — so a Vector[Showable] holding this box clones/
	// slices to an independently-owned box (drop_fn stays null → pal_free on drop).
	codegentest.AssertContains(t, ir, "@promise_typeinfo_flatbox_16")
	codegentest.AssertContains(t, ir, "bitcast ({ i8*, i8*, i8*, i32, i32 }* @promise_typeinfo_flatbox_16 to i8*)")
	codegentest.AssertContains(t, ir, "@__promise_flat_box_clone_16")
}

func TestPrimitiveBoolToStructuralView(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display(true); }
	`)
	codegentest.AssertContains(t, ir, "@promise_vtable_bool_as_Showable")
	codegentest.AssertContains(t, ir, "bool.to_string$view_adapt")
}

func TestPrimitiveF64ToStructuralView(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display(3.14); }
	`)
	codegentest.AssertContains(t, ir, "@promise_vtable_f64_as_Showable")
	codegentest.AssertContains(t, ir, "f64.to_string$view_adapt")
}

func TestStringToStructuralView(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display("hello"); }
	`)
	codegentest.AssertContains(t, ir, "@promise_vtable_string_as_Showable")
	// T1280: a string coerced to a structural interface is heap-boxed as
	// { i8* typeinfo, i8* string_ptr }, carries a dedicated typeinfo whose drop_fn
	// frees the cloned string + box, and dispatches through an adapter thunk.
	codegentest.AssertContains(t, ir, "string.to_string$view_adapt")
	codegentest.AssertContains(t, ir, "@promise_typeinfo_stringbox")
	codegentest.AssertContains(t, ir, "@__promise_string_box_drop")
}

func TestPrimitiveCharToStructuralView(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() { display('A'); }
	`)
	codegentest.AssertContains(t, ir, "@promise_vtable_char_as_Showable")
	codegentest.AssertContains(t, ir, "char.to_string$view_adapt")
}

func TestMultiplePrimitivesToStructuralView(t *testing.T) {
	// Multiple different primitives boxed in the same function
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string { return s.to_string(); }
		main() {
			display(42);
			display(true);
			display("hi");
		}
	`)
	codegentest.AssertContains(t, ir, "@promise_vtable_int_as_Showable")
	codegentest.AssertContains(t, ir, "@promise_vtable_bool_as_Showable")
	codegentest.AssertContains(t, ir, "@promise_vtable_string_as_Showable")
}

func TestVectorStructuralElementDropAndClone(t *testing.T) {
	// T1284: a Vector[structural-interface] must (a) drop each heap-boxed element
	// through __promise_structural_drop at vector drop, and (b) deep-clone the boxed
	// instances on clone via __promise_structural_clone (RTTI dispatch on the
	// instance typeinfo's clone_fn) so the clone owns independent boxes — otherwise
	// the now-active element drop double-frees the aliased boxes.
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		type Widget {
			int id;
			to_string() string { return this.id.to_string(); }
		}
		main() {
			Showable[] v = [];
			v.push(Widget(id: 1));
			Showable[] c = v.clone();
		}
	`)
	// The RTTI structural drop/clone helpers are emitted and referenced.
	codegentest.AssertContains(t, ir, "@__promise_structural_drop")
	codegentest.AssertContains(t, ir, "@__promise_structural_clone")
	// The clone helper dispatches through the typeinfo clone_fn (field 2).
	codegentest.AssertContains(t, ir, "call i8* @__promise_structural_clone")
}

func TestValueTypeToStructuralViewHeapBoxes(t *testing.T) {
	// T1276: a pure value type coerced to a structural interface must be boxed on
	// the HEAP (pal_alloc), not a stack alloca, so the interface can escape its
	// defining frame (return) without dangling. Field 0 of the box is overwritten
	// with the concrete typeinfo pointer so __promise_structural_drop reads a null
	// drop_fn and pal_free's the box.
	ir := codegentest.GenerateIR(t, `
		type Sink `+"`"+`structural {
			emit(~this, int x) int `+"`"+`abstract;
		}
		type Counter {
			int fd `+"`"+`value;
			emit(~this, int x) int { return this.fd + x; }
		}
		make_sink(int fd) Sink { return Counter(fd: fd); }
		main() { Sink s = make_sink(7); s.emit(1); }
	`)
	codegentest.AssertContains(t, ir, "@promise_vtable_Counter_as_Sink")
	// Heap box: pal_alloc of the value struct, then typeinfo stored into field 0.
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc")
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Counter")
	// No stack alloca of the Counter value struct for the box — it must be heap.
	// (The value struct is the named type %promise_Counter_v, so a reverted
	// stack box would emit `alloca %promise_Counter_v`; assert that's absent.)
	codegentest.AssertNotContains(t, ir, "alloca %promise_Counter_v")
}

func TestPrimitiveMixedWithUserTypeToStructuralView(t *testing.T) {
	// Same function call mixes primitives and user types as structural params
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		type Pair {
			to_string() string { return "pair"; }
		}
		both(Showable a, Showable b) string { return a.to_string() + b.to_string(); }
		main() { both(42, Pair()); }
	`)
	codegentest.AssertContains(t, ir, "@promise_vtable_int_as_Showable")
	codegentest.AssertContains(t, ir, "@promise_vtable_Pair_as_Showable")
}

func TestReturnCoercionSecondParent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		makeMovable() Movable {
			return Robot();
		}
		main() {
			Movable m = makeMovable();
			string s = m.walk();
		}
	`)
	// Returning Robot as Movable (second parent) should emit view vtable
	codegentest.AssertContains(t, ir, "@promise_vtable_Robot_as_Movable")
}

func TestArgCoercionSecondParent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Speakable {
			speak() string `+"`"+`abstract;
		}
		type Movable {
			walk() string `+"`"+`abstract;
		}
		type Robot is Speakable, Movable {
			speak() string { return "beep"; }
			walk() string { return "roll"; }
		}
		useMovable(Movable m) string {
			return m.walk();
		}
		main() {
			Robot r = Robot();
			string s = useMovable(r);
		}
	`)
	// Passing Robot as Movable arg (second parent) should emit view vtable
	codegentest.AssertContains(t, ir, "@promise_vtable_Robot_as_Movable")
}

func TestGetterSetterSameNameVtable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base {
			get val int `+"`"+`abstract;
			set val(int v) `+"`"+`abstract;
		}
		type Impl is Base {
			int _v;
			get val int { return this._v; }
			set val(int v) { this._v = v; }
		}
		main() {
			Base b = Impl(_v: 0);
			b.val = 42;
			int v = b.val;
		}
	`)
	// Vtable should contain both getter and setter slots
	codegentest.AssertContains(t, ir, "@promise_vtable_Base")
	codegentest.AssertContains(t, ir, "@promise_vtable_Impl")
	// Both getter and setter functions should exist
	codegentest.AssertContains(t, ir, "define i64 @Impl.val(")
	codegentest.AssertContains(t, ir, "define void @Impl.val$set(")
	// Virtual dispatch should NOT use direct call to Base.val (abstract)
	codegentest.AssertNotContains(t, ir, "call i64 @Base.val(")
}

func TestViewVtableGetterSetter(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Readable {
			get val int `+"`"+`abstract;
		}
		type Writable {
			get val int `+"`"+`abstract;
			set val(int v) `+"`"+`abstract;
		}
		type Store is Readable, Writable {
			int _v;
			get val int { return this._v; }
			set val(int v) { this._v = v; }
		}
		main() {
			Writable w = Store(_v: 0);
			w.val = 42;
			int v = w.val;
		}
	`)
	// View vtable for Store-as-Writable should exist
	codegentest.AssertContains(t, ir, "promise_vtable_Store_as_Writable")
	// Both functions should be emitted
	codegentest.AssertContains(t, ir, "define i64 @Store.val(")
	codegentest.AssertContains(t, ir, "define void @Store.val$set(")
}

// Virtual drop dispatch through vtable (type has children → needs vtable)
func TestDropVirtualDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Handle {
			int id;
			drop(~this) { }
		}
		type FileHandle is Handle {
			drop(~this) { }
		}
		main() {
			h := Handle(id: 1);
			int x = h.id;
		}
	`)
	// Handle has children → needs vtable → virtual drop dispatch
	codegentest.AssertContains(t, ir, "drop.call")
	codegentest.AssertContains(t, ir, "drop.skip")
	codegentest.AssertContains(t, ir, "h.dropflag")
	codegentest.AssertContains(t, ir, "@promise_vtable_Handle")
}

// Reassignment with virtual drop dispatch (type has children)
func TestDropOnReassignmentVirtualDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base {
			int id;
			drop(~this) {}
		}
		type Child is Base {
			drop(~this) {}
		}
		test() {
			r := Base(id: 1);
			r = Base(id: 2);
		}
		main() {}
	`)
	codegentest.AssertContains(t, ir, "drop.call")
	codegentest.AssertContains(t, ir, "drop.skip")
}

// T0127: Structural interface variables from calls get bindingFree with iter cleanup
func TestStructuralInterfaceVarFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter is Iterator[int] {
			int current;
			int limit;
			next(~this) int? {
				if this.current < this.limit {
					v := this.current;
					this.current = this.current + 1;
					return v;
				}
				return none;
			}
		}
		test() {
			c := Counter(current: 0, limit: 5);
			Iterator[int] it = c.filter(|int x| -> bool {
				return x > 2;
			});
		}
		main() {}
	`)
	// Structural interface variable should get a drop flag and free.call block
	codegentest.AssertContains(t, ir, "it.dropflag")
	codegentest.AssertContains(t, ir, "free.call")
	// Should use __promise_iter_cleanup for iterator chain results (frees env + instance)
	codegentest.AssertContains(t, ir, "__promise_iter_cleanup")
}

// T0127: Structural interface variables from identifiers should NOT get bindingFree (borrow)
func TestStructuralInterfaceVarBorrow(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter is Iterator[int] {
			int current;
			int limit;
			next(~this) int? {
				if this.current < this.limit {
					v := this.current;
					this.current = this.current + 1;
					return v;
				}
				return none;
			}
		}
		test() {
			c := Counter(current: 0, limit: 5);
			Iterator[int] it = c.filter(|int x| -> bool {
				return x > 2;
			});
			Iterator[int] it2 = it;
		}
		main() {}
	`)
	// it should have a drop flag (from call result)
	codegentest.AssertContains(t, ir, "it.dropflag")
	// it2 should NOT have a drop flag (borrow from it)
	codegentest.AssertNotContains(t, ir, "it2.dropflag")
}

// T0092: String return from function with structural interface param is tracked as temp.
func TestStringTempStructuralParamReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		display(Showable s) string {
			return s.to_string();
		}
		test() {
			assert(display(42) == "42", "ok");
		}
		main() {}
	`)
	// The return value of display(42) should be tracked as a string temp
	// and freed at statement end via promise_string_drop.
	codegentest.AssertContains(t, ir, "call i8* @__user.display")
	codegentest.AssertContains(t, ir, "tmp.drop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// --- Non-native operator dispatch ---

func TestNonNativeOperatorDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pt {
			int x;
			==(Pt other) bool { return this.x == other.x; }
		}
		main() {
			Pt a = Pt(x: 1);
			Pt b = Pt(x: 2);
			bool r = a == b;
		}
	`)
	codegentest.AssertContains(t, ir, `call i1 @"Pt.=="(`)
}

func TestDefaultMethodViaViewVtable(t *testing.T) {
	ir := codegentest.GenerateIRWithStd(t,
		"type MyEq `structural {\n\t==(Self other) bool `abstract;\n\t!=(Self other) bool => !(this == other);\n}\n",
		`type Pt {
			int x;
			==(Pt other) bool { return this.x == other.x; }
		}
		main() {
			MyEq e = Pt(x: 1);
			MyEq f = Pt(x: 2);
			bool r = e != f;
		}
	`)
	codegentest.AssertContains(t, ir, `@"Pt.!="`)                  // synthesized default
	codegentest.AssertContains(t, ir, "promise_vtable_Pt_as_MyEq") // view vtable
}

func TestDefaultMethodOverride(t *testing.T) {
	// Concrete type overrides the default — the override should be used, not the synthesized default
	ir := codegentest.GenerateIRWithStd(t,
		"type MyEq `structural {\n\t==(Self other) bool `abstract;\n\t!=(Self other) bool => !(this == other);\n}\n",
		`type Pt {
			int x;
			==(Pt other) bool { return this.x == other.x; }
			!=(Pt other) bool { return this.x != other.x; }
		}
		main() {
			MyEq e = Pt(x: 1);
			MyEq f = Pt(x: 2);
			bool r = e != f;
		}
	`)
	codegentest.AssertContains(t, ir, "promise_vtable_Pt_as_MyEq") // view vtable still created
	// The vtable should use the concrete Pt.!= override, not a synthesized default.
	// Check that the concrete method exists.
	codegentest.AssertContains(t, ir, `@"Pt.!="`)
}

func TestOrderedDefaultsViaViewVtable(t *testing.T) {
	stdOrd := "type MyEq `structural {\n\t==(Self other) bool `abstract;\n\t!=(Self other) bool => !(this == other);\n}\n" +
		"type MyOrd is MyEq `structural {\n\t<(Self other) bool `abstract;\n\t>(Self other) bool => other < this;\n\t<=(Self other) bool => !(other < this);\n\t>=(Self other) bool => !(this < other);\n}\n"
	ir := codegentest.GenerateIRWithStd(t, stdOrd, `
		type Val {
			int n;
			==(Val o) bool { return this.n == o.n; }
			<(Val o) bool { return this.n < o.n; }
		}
		main() {
			MyOrd a = Val(n: 1);
			MyOrd b = Val(n: 2);
			bool r1 = a > b;
			bool r2 = a <= b;
			bool r3 = a >= b;
			bool r4 = a != b;
		}
	`)
	codegentest.AssertContains(t, ir, `@"Val.>"`)  // synthesized from > default
	codegentest.AssertContains(t, ir, `@"Val.<="`) // synthesized from <= default
	codegentest.AssertContains(t, ir, `@"Val.>="`) // synthesized from >= default
	codegentest.AssertContains(t, ir, `@"Val.!="`) // inherited from MyEq parent default
	codegentest.AssertContains(t, ir, "promise_vtable_Val_as_MyOrd")
}

func TestStringInterpolationUserTypeVtable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shape {
			format!(Writer ~w) { w.write_string("shape"); }
		}
		type Circle is Shape {
			format!(Writer ~w) { w.write_string("circle"); }
		}
		main() {
			Shape s = Circle();
			string x = "{s}";
		}
	`)
	// Virtual dispatch: should use the Builder-as-Writer view vtable (with $view_adapt wrappers)
	codegentest.AssertContains(t, ir, "promise_vtable_Builder_as_Writer")
	codegentest.AssertContains(t, ir, "interp.format.ok")
}

// --- Index/Slice Operator Method Dispatch Tests ---

func TestIndexMethodDispatchMap(t *testing.T) {
	// Map [] goes through genMethodIndex, not genMapIndex
	ir := codegentest.GenerateIR(t, `
		main() {
			m := {"a": 1};
			int? v = m["a"];
		}
	`)
	codegentest.AssertContains(t, ir, `call { i1, i64 } @"Map[string, int].[]"(`)
}

func TestIndexAssignMethodDispatchMap(t *testing.T) {
	// Map []= goes through genMethodIndexAssign
	ir := codegentest.GenerateIR(t, `
		main() {
			m := {"a": 1};
			m["b"] = 2;
		}
	`)
	codegentest.AssertContains(t, ir, `call void @"Map[string, int].[]="(`)
}

func TestIndexNativeDispatchVector(t *testing.T) {
	// Vector [] still uses native path (genVectorIndex)
	ir := codegentest.GenerateIR(t, `
		main() {
			v := [1, 2, 3];
			int x = v[0];
		}
	`)
	codegentest.AssertContains(t, ir, "index.ok")
	codegentest.AssertContains(t, ir, "index.oob")
}

func TestIndexNativeDispatchString(t *testing.T) {
	// String [] still uses native path (genStringIndex)
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "hello";
			char c = s[0];
		}
	`)
	codegentest.AssertContains(t, ir, "stridx.ok")
	codegentest.AssertContains(t, ir, "stridx.oob")
}

func TestValueTypeOperatorDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Vec2 {
			int x `+"`value"+`;
			int y `+"`value"+`;
			+(Vec2 other) Vec2 {
				return Vec2(x: this.x + other.x, y: this.y + other.y);
			}
		}
		main() {
			Vec2 a = Vec2(x: 1, y: 2);
			Vec2 b = Vec2(x: 3, y: 4);
			Vec2 c = a + b;
		}
	`)
	// Operator dispatches to Vec2.+ method
	codegentest.AssertContains(t, ir, `call %promise_Vec2_v @"Vec2.+"(`)
}

// T0494: Virtual dispatch path with a heap user-type return. Exercises
// trackHeapUserTypeResult under genVirtualGetterCall — symmetric to the
// direct-path TestGetterUserHeapResultTrackedInChain.
func TestVirtualGetterHeapResultTrackedInChain(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Inner { string _name; drop(~this) {}
			ok(this) bool { return true; }
		}
		type HeapSource {
			get item Inner `+"`public"+` `+"`abstract"+`;
		}
		type HeapImpl is HeapSource {
			Inner _item;
			get item Inner `+"`public"+` => Inner(_name: this._item._name);
		}
		test() {
			HeapSource src = HeapImpl(_item: Inner(_name: "x"));
			bool b = src.item.ok();
		}
	`)
	// Vtable wiring exists (virtual dispatch path).
	codegentest.AssertContains(t, ir, "@promise_vtable_HeapSource")
	codegentest.AssertContains(t, ir, "@promise_vtable_HeapImpl")
	// Heap-temp drop is registered and Inner.drop is wired up.
	codegentest.AssertContains(t, ir, "heap.drop")
	codegentest.AssertContains(t, ir, "Inner.drop")
}

// B0247: RTTI drop dispatch for types with explicit user drop must call pal_free
// after the drop function (user drops don't free the instance themselves).
// The typeinfo should store a $wrap function that calls drop + pal_free.
func TestRttiDropExplicitUserDropWrap(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter is Iterator[int] {
			int val;
			next(~this) int? {
				if this.val > 0 {
					this.val = this.val - 1;
					return this.val;
				}
				return none;
			}
			drop(~this) {}
		}
		make_counter() Iterator[int] {
			return Counter(val: 3);
		}
		test() {
			Iterator[int]? it = none;
			it = make_counter();
			it = none;
		}
	`)
	// The typeinfo drop_fn_ptr for Counter should point to the $wrap function
	// which calls Counter.drop then pal_free.
	codegentest.AssertContains(t, ir, "Counter.drop$wrap")
}

// T0847 (cast variant): `Holder(held: v[0] as! Circle)` over a polymorphic
// Shape[] must peel the cast to reach the IndexExpr subject and still dup.
func TestT0847_ConstructorCastVectorElementDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shape { string name; drop(~this) {} }
		type Circle is Shape { int radius; }
		type Holder { Shape held; drop(~this) {} }
		test_t0847_cast() {
			v := Shape[]();
			v.push(Circle(name: "c", radius: 1));
			h := Holder(held: v[0] as! Circle);
		}
	`)
	// Cast peel reaches the IndexExpr → dup-on-read fires (allocate + memcpy).
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc")
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy")
}

func TestParenThisVirtualMethodNoExtractFromPtr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0613Shape {
			area(this) int `+"`abstract"+`;
			area_via(this) int { return (this).area(); }
		}
		type T0613Square is T0613Shape {
			int side;
			area(this) int { return this.side * this.side; }
		}
		main() { s := T0613Square(side: 5); a := s.area_via(); }
	`)
	codegentest.AssertNotContains(t, ir, "extractvalue i8*")
	if codegentest.ExtractFunction(ir, "T0613Shape.area_via") == "" {
		t.Fatal("expected T0613Shape.area_via in IR")
	}
}

// genVirtualBinaryOp gate: a base type with a child dispatches its operator
// through the vtable, so `(this) + other` hits the virtual-operator receiver
// gate rather than the direct genBinaryExpr gate.
func TestParenThisVirtualOperatorNoExtractFromPtr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0613VOpBase {
			int n;
			+(T0613VOpBase other) T0613VOpBase { return T0613VOpBase(n: this.n + other.n); }
			add_via(this, T0613VOpBase other) T0613VOpBase { return (this) + other; }
		}
		type T0613VOpDerived is T0613VOpBase {}
		main() {
			a := T0613VOpBase(n: 1);
			b := T0613VOpBase(n: 2);
			c := a.add_via(b);
		}
	`)
	codegentest.AssertNotContains(t, ir, "extractvalue i8*")
	if codegentest.ExtractFunction(ir, "T0613VOpBase.add_via") == "" {
		t.Fatal("expected T0613VOpBase.add_via in IR")
	}
}

// --- T0747: `this as!/as T` RTTI cast on a `this` receiver ---
//
// genExpr(this) yields a bare instance i8*, not the {vtable, instance} value
// struct that the cast result paths assume. Before the fix, `(this as! T).field`
// emitted `extractvalue i8* %p, 1` (opt-rejected) and `d := this as! T` /
// `T? o = this as T` panicked in codegen storing the i8* into a value-struct
// slot. The fix rebuilds the cast result as a {vtable, instance} value struct
// (vtable loaded from the object's typeinfo chain, mirroring genVirtualBinaryOp).
// The "no extractvalue from i8*" invariant guards the IR shape; the optional
// case additionally guards against the codegen panic (generateIR would panic).

// Forced cast + inline field access through `this`, both bare and paren subject.
// The reconstruction (insertvalue into {i8*, i8*}) is what lets the downstream
// field access extract from an aggregate instead of `extractvalue i8* ...`.
func TestThisCastForcedFieldNoExtractFromPtr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0747Base {
			int n;
			whoami(this) string `+"`abstract"+`;
			as_n(this) int { return (this as! T0747Derived).n; }
			as_n_bare(this) int { return ((this) as! T0747Derived).n; }
		}
		type T0747Derived is T0747Base { whoami(this) string { return "d"; } }
		main() { T0747Base b = T0747Derived(n: 7); _ := b.as_n(); _ := b.as_n_bare(); }
	`)
	codegentest.AssertNotContains(t, ir, "extractvalue i8*")
	asN := codegentest.ExtractFunction(ir, "T0747Base.as_n")
	if asN == "" {
		t.Fatal("expected T0747Base.as_n in IR")
	}
	// The cast result is rebuilt as a {i8*, i8*} value struct (the fix); without
	// it the function returned the raw i8* and the field read extracted from it.
	codegentest.AssertContains(t, asN, "insertvalue { i8*, i8* }")
}

// Forced cast bound to a local, then a virtual method call on the cast result.
// Before the fix this panicked in codegen (store i8* into {i8*,i8*}* var slot).
// The reconstructed value struct carries the real vtable (loaded from typeinfo),
// so the method call on the local dispatches correctly.
func TestThisCastForcedLocalNoExtractFromPtr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0747LBase {
			int n;
			whoami(this) string `+"`abstract"+`;
			as_str(this) string { d := (this) as! T0747LDerived; return d.whoami(); }
			as_str_bare(this) string { d := this as! T0747LDerived; return d.whoami(); }
		}
		type T0747LDerived is T0747LBase { whoami(this) string { return "d"; } }
		main() { T0747LBase b = T0747LDerived(n: 7); _ := b.as_str(); _ := b.as_str_bare(); }
	`)
	codegentest.AssertNotContains(t, ir, "extractvalue i8*")
	if codegentest.ExtractFunction(ir, "T0747LBase.as_str") == "" {
		t.Fatal("expected T0747LBase.as_str in IR")
	}
}

// --- T0783: `return x as! T` clears the cast subject's drop flag ---
//
// `return s as! Circle` aliases s's heap instance into the returned value; the
// caller's binding owns it. genReturnStmt now peels the cast via
// castSubjectMovableIdent and clears the subject's drop flag (the same helper
// T0754/T0800 use at owning-slot stores) so s's scope-exit drop does not fire
// on the same allocation -> double-free. The IR signature is a
// `store i1 false, i1* %s.dropflag` on the cast-success path before the return;
// without the fix that clear is absent and the conditional drop executes.
func TestT0783_ReturnCastClearsSubjectDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape { f64 radius; area(this) f64 { return this.radius; } }
		helper(int dummy) Circle {
			Shape s = Circle(name: "src", radius: 2.0);
			return s as! Circle;
		}
		main() { _ := helper(0); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.helper")
	if fn == "" {
		t.Fatal("expected __user.helper in IR")
	}
	// The cast subject's drop flag must be cleared (moved out via the return).
	codegentest.AssertContains(t, fn, "store i1 false, i1* %s.dropflag")
}

// Chained cast on the return path (T0800 sibling): castSubjectMovableIdent
// recurses through the nested CastExpr to the innermost subject, so its drop
// flag is still cleared.
func TestT0783_ReturnChainedCastClearsSubjectDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape { f64 radius; area(this) f64 { return this.radius; } }
		helper(int dummy) Circle {
			Shape s = Circle(name: "src", radius: 2.0);
			return (s as! Circle) as! Circle;
		}
		main() { _ := helper(0); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.helper")
	if fn == "" {
		t.Fatal("expected __user.helper in IR")
	}
	codegentest.AssertContains(t, fn, "store i1 false, i1* %s.dropflag")
}
