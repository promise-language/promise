package member

import (
	"bytes"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
	"github.com/promise-language/promise/compiler/internal/sema"
)

func TestUserTypeConstructor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Dog { int age; }
		main() { d := Dog(age: 3); }
	`)
	// Should allocate via malloc
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(i64")
	// Should bitcast to typed pointer
	codegentest.AssertContains(t, ir, "bitcast i8*")
	// Should store field value
	codegentest.AssertContains(t, ir, "store i64 3")
}

func TestUserTypeFieldAccess(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Dog { int age; }
		main() {
			d := Dog(age: 3);
			x := d.age;
		}
	`)
	// Should bitcast and GEP to access field
	codegentest.AssertContains(t, ir, "getelementptr %promise_Dog_i")
	codegentest.AssertContains(t, ir, "load i64")
}

func TestUserTypeFieldAssign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Dog { int age; }
		main() {
			d := Dog(age: 3);
			d.age = 5;
		}
	`)
	codegentest.AssertContains(t, ir, "store i64 5")
}

func TestUserTypeMethod(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Dog {
			int age;
			getAge(this) int {
				return this.age;
			}
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "define i64 @Dog.getAge(i8* %this)")
}

func TestUserTypeMethodCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Dog {
			int age;
			getAge(this) int {
				return this.age;
			}
		}
		main() {
			d := Dog(age: 3);
			x := d.getAge();
		}
	`)
	codegentest.AssertContains(t, ir, "call i64 @Dog.getAge(i8*")
}

func TestUserTypeMethodWithReceiver(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter {
			int value;
			increment(~this) {
				this.value += 1;
			}
		}
		main() {
			c := Counter(value: 0);
			c.increment();
		}
	`)
	codegentest.AssertContains(t, ir, "define void @Counter.increment(i8* %this)")
	codegentest.AssertContains(t, ir, "call void @Counter.increment(i8*")
}

func TestThisExpr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box {
			int value;
			get(this) int {
				return this.value;
			}
		}
		main() {
			b := Box(value: 42);
			x := b.get();
		}
	`)
	// Method should load this from alloca
	codegentest.AssertContains(t, ir, "%this.addr = alloca i8*")
}

func TestUserTypeMultipleFields(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; int z; }
		main() {
			p := Point(x: 1, y: 2, z: 3);
		}
	`)
	codegentest.AssertContains(t, ir, "%promise_Point_i = type { %promise_Point_m*, i64, i64, i64 }")
	// All three field stores
	codegentest.AssertContains(t, ir, "store i64 1")
	codegentest.AssertContains(t, ir, "store i64 2")
	codegentest.AssertContains(t, ir, "store i64 3")
}

func TestUserTypeMethodWithParams(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Adder {
			int base;
			add(this, int n) int {
				return this.base + n;
			}
		}
		main() {
			a := Adder(base: 10);
			x := a.add(5);
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @Adder.add(i8* %this, i64 %n)")
	codegentest.AssertContains(t, ir, "call i64 @Adder.add(i8*")
}

func TestUserTypeNestedField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Inner { int value; }
		type Outer { Inner child; }
		main() {
			i := Inner(value: 42);
			o := Outer(child: i);
		}
	`)
	// Inner stored as value struct { i8*, i8* } in Outer's instance struct
	codegentest.AssertContains(t, ir, "%promise_Inner_i = type { %promise_Inner_m*, i64 }")
	codegentest.AssertContains(t, ir, "%promise_Outer_i = type { %promise_Outer_m*, { i8*, i8* } }")
	// Both should be allocated via malloc
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(i64")
}

func TestUserTypeNestedFieldAccess(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Inner { int value; }
		type Outer { Inner child; }
		main() {
			i := Inner(value: 42);
			o := Outer(child: i);
			c := o.child;
		}
	`)
	// Should GEP into Outer to load the child value struct
	codegentest.AssertContains(t, ir, "getelementptr %promise_Outer_i")
	codegentest.AssertContains(t, ir, "load { i8*, i8* }")
}

func TestUserTypeZeroArgConstructor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; }
		main() {
			p := Point(x: 0, y: 0);
		}
	`)
	// Should allocate and store both fields
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(i64")
	codegentest.AssertContains(t, ir, "store i64 0")
}

func TestConstructorDefaultExprEvaluation(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Config { int port = 8080; string host; }
		main() {
			c := Config(host: "localhost");
		}
	`)
	// The default expression (8080) should be evaluated and stored
	codegentest.AssertContains(t, ir, "store i64 8080")
}

func TestConstructorAllDefaultsOmitted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Defaults { int x = 42; int y = 99; }
		main() {
			d := Defaults();
		}
	`)
	codegentest.AssertContains(t, ir, "store i64 42")
	codegentest.AssertContains(t, ir, "store i64 99")
}

func TestNewConstructorCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Clamped {
			int value;
			new(~this, int v) {
				if v < 0 { this.value = 0; }
				else { this.value = v; }
			}
		}
		main() {
			c := Clamped(v: 50);
		}
	`)
	// Should declare the new() method as a void function
	codegentest.AssertContains(t, ir, "define void @Clamped.new(i8* %this")
	// Constructor should call new()
	codegentest.AssertContains(t, ir, "call void @Clamped.new(")
}

func TestNewConstructorFinalFieldCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Token {
			string raw `+"`"+`final;
			new(~this, string raw) {
				this.raw = raw;
			}
		}
		main() {
			t := Token(raw: "hello");
		}
	`)
	codegentest.AssertContains(t, ir, "define void @Token.new(i8* %this")
	codegentest.AssertContains(t, ir, "call void @Token.new(")
}

func TestFactoryConstructorCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Color {
			int r;
			int g;
			int b;
			red() Self `+"`"+`factory {
				return Color(r: 255, g: 0, b: 0);
			}
		}
		main() {
			Color c = Color.red();
		}
	`)
	// Factory method should be defined without a receiver parameter
	codegentest.AssertContains(t, ir, "define { i8*, i8* } @Color.red()")
	// main should call Color.red
	codegentest.AssertContains(t, ir, "call { i8*, i8* } @Color.red()")
}

func TestUserTypeHeaderFieldTypes(t *testing.T) {
	result := codegentest.CompileResult(t, `
		type Person { string name; int age; bool active; }
		main() { }
	`)

	var buf bytes.Buffer
	if err := codegen.GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	// Verify instance struct field types
	codegentest.AssertContains(t, header, "void*                name;")
	codegentest.AssertContains(t, header, "int64_t              age;")
	codegentest.AssertContains(t, header, "uint8_t              active;")
}

func TestUserTypeMethodMutatesField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter {
			int value;
			set(~this, int n) {
				this.value = n;
			}
		}
		main() {
			c := Counter(value: 0);
			c.set(42);
		}
	`)
	codegentest.AssertContains(t, ir, "define void @Counter.set(i8* %this, i64 %n)")
	// Should store into this.value
	codegentest.AssertContains(t, ir, "getelementptr %promise_Counter_i")
	codegentest.AssertContains(t, ir, "store i64")
}

func TestFunctionTypedFieldCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper {
			() -> int getter;
			get_val() int { return this.getter(); }
		}
		main() {
			w := Wrapper(getter: || -> int { return 99; });
		}
	`)
	// Should call through indirect call path (extractvalue from fat pointer)
	codegentest.AssertContains(t, ir, "define i64 @Wrapper.get_val")
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestFunctionTypedFieldCallWithArgs(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Calc {
			(int, int) -> int op;
			run(int a, int b) int { return this.op(a, b); }
		}
		main() {
			c := Calc(op: |int x, int y| -> x + y);
		}
	`)
	// Method should exist and use indirect call
	codegentest.AssertContains(t, ir, "define i64 @Calc.run")
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestFunctionTypedFieldVoidReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Handler {
			(int) -> void action;
			run(int x) { this.action(x); }
		}
		main() {
			h := Handler(action: |int x| { });
		}
	`)
	codegentest.AssertContains(t, ir, "define void @Handler.run")
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
}

// genClassicForStmt with typed init
func TestClassicForTypedInit(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			for int i = 0; i < 5; i += 1 {
				int x = i;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "for.header")
	codegentest.AssertContains(t, ir, "for.exit")
}

func TestMethodOverride(t *testing.T) {
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
			string s = d.speak();
		}
	`)
	// d.speak() should dispatch to Dog.speak (child overrides parent)
	codegentest.AssertContains(t, ir, "call i8* @Dog.speak(i8*")
}

func TestUpcastFieldAccess(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal { string name; }
		type Dog is Animal { string breed; }
		main() {
			Animal a = Dog(name: "Rex", breed: "Lab");
			string n = a.name;
		}
	`)
	// Upcast Dog to Animal, then access name via Animal layout
	codegentest.AssertContains(t, ir, "getelementptr %promise_Animal_i")
}

func TestConstructorStoresTypeInfo(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal { string name; }
		main() {
			Animal a = Animal(name: "Rex");
		}
	`)
	// Constructor should store type info pointer instead of null
	codegentest.AssertContains(t, ir, "@promise_typeinfo_Animal")
	// The _variant slot should be set via bitcast of the type info global
	codegentest.AssertContains(t, ir, "bitcast")
}

func TestIsNamedType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal { string name; }
		type Dog is Animal { }
		main() {
			Animal a = Dog(name: "Rex");
			bool b = a is Dog;
		}
	`)
	// Should call promise_type_is (now codegen-emitted, not extern) and convert to i1
	codegentest.AssertContains(t, ir, "define i32 @promise_type_is")
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is")
	codegentest.AssertContains(t, ir, "icmp ne i32")
}

func TestFieldShadowing(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base { int x; int y; }
		type Child is Base { string x; }
		main() {
			Child c = Child(y: 1, x: "hi");
			string s = c.x;
			int n = c.y;
		}
	`)
	// Child layout: _variant, y (inherited, not shadowed), x (own, shadows Base.x)
	// y is int (i64), x is string (i8*) — parent x omitted from layout
	codegentest.AssertContains(t, ir, "%promise_Child_i = type { %promise_Child_m*, i64, i8* }")
}

func TestDirectGetterPreserved(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point {
			int _x;
			get x int => this._x;
		}
		main() {
			Point p = Point(_x: 42);
			int v = p.x;
		}
	`)
	// Point has no children → direct dispatch for getter
	codegentest.AssertContains(t, ir, "call i64 @Point.x")
}

func TestFieldAccessThroughValueStruct(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal {
			string name;
		}
		main() {
			Animal a = Animal(name: "Rex");
			string n = a.name;
		}
	`)
	// Should extract instance from value struct, then GEP to field
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
	codegentest.AssertContains(t, ir, "getelementptr %promise_Animal_i")
}

func TestReturnThisWrapsValueStruct(t *testing.T) {
	// Regression test (B0122): returning `this` from a method produced
	// `ret i8*` instead of the expected value struct `{ i8*, i8* }`.
	ir := codegentest.GenerateIR(t, `
		type Counter {
			int count;
			iter() Counter { return this; }
		}
		main() { c := Counter(count: 0).iter(); }
	`)
	// Counter.iter should return { i8*, i8* } (value struct), not i8*
	codegentest.AssertContains(t, ir, "define { i8*, i8* } @Counter.iter(i8* %this)")
	// The body should insertvalue to build the value struct
	codegentest.AssertContains(t, ir, "insertvalue { i8*, i8* }")
}

// T0582: `return (this);` (paren-wrapped) must take the same wrapping path as
// `return this;` — codegen must build the { i8*, i8* } value struct, not emit
// a bare `ret i8* %0` against a `{ ptr, ptr }` return type (which opt rejects).
func TestReturnParenThisWrapsValueStruct(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper { int v; eat(~this) Wrapper { return (this); } }
		main() { w := Wrapper(v: 1); x := w.eat(); }
	`)
	codegentest.AssertContains(t, ir, "insertvalue { i8*, i8* }")
	body := codegentest.ExtractFunction(ir, "Wrapper.eat")
	codegentest.AssertNotContains(t, body, "ret i8* %")
}

// T0582: nested parens — `return ((this));` — also takes the wrapping path,
// confirming the paren-peel iterates.
func TestReturnDoubleParenThisWrapsValueStruct(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper { int v; eat(~this) Wrapper { return ((this)); } }
		main() { w := Wrapper(v: 1); x := w.eat(); }
	`)
	codegentest.AssertContains(t, ir, "insertvalue { i8*, i8* }")
	body := codegentest.ExtractFunction(ir, "Wrapper.eat")
	codegentest.AssertNotContains(t, body, "ret i8* %")
}

func TestMultipleOpaqueFieldsLayout(t *testing.T) {
	// B0096: multiple channel/task fields in one type.
	ir := codegentest.GenerateIR(t, `
		compute() int { return 1; }
		type Multi {
			channel[int] ch1;
			channel[string] ch2;
			task[int] t;
		}
		main() {
			c1 := channel[int](capacity: 1);
			c2 := channel[string](capacity: 1);
			tk := go compute();
			m := Multi(ch1: c1, ch2: c2, t: tk);
		}
	`)
	// All three fields must be i8*
	codegentest.AssertContains(t, ir, "%promise_Multi_i = type { %promise_Multi_m*, i8*, i8*, i8* }")
}

// --- Getter/Setter same name regression ---

func TestGetterSetterSameNameCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box {
			int _val;
			get val int { return this._val; }
			set val(int v) { this._val = v; }
		}
		main() {
			Box b = Box(_val: 0);
			b.val = 42;
			int v = b.val;
		}
	`)
	// Both getter and setter should produce distinct functions
	codegentest.AssertContains(t, ir, "define i64 @Box.val(")
	codegentest.AssertContains(t, ir, "define void @Box.val$set(")
}

func TestCompoundAssignmentGetterSetterCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter {
			int _count;
			get count int { return this._count; }
			set count(int v) { this._count = v; }
		}
		main() {
			Counter c = Counter(_count: 0);
			c.count += 5;
		}
	`)
	// Should call both getter and setter
	codegentest.AssertContains(t, ir, "call i64 @Counter.count(")
	codegentest.AssertContains(t, ir, "call void @Counter.count$set(")
}

// T0405: Vector field reassign must emit null guard (skip drop when field is null/zero).
func TestFieldAssignVecNullGuardInIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder { string[] field; }
		main() {
			v := string[]();
			h := Holder(v);
			v2 := string[]();
			h.field = v2;
		}
	`)
	// The vecdrop block must guard against null (zero-initialized fields from
	// error fallthroughs) — emits an `or i1` combining isNull and isSame checks.
	codegentest.AssertContains(t, ir, "field.vecdrop")
	codegentest.AssertContains(t, ir, "or i1")
}

// --- Hash getter tests ---

func TestHashGetterInt(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := 42; h := x.hash; }`)
	codegentest.AssertContains(t, ir, "call i64 @__mod_std__fnv1a_hash(i64")
}

func TestHashGetterBool(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { b := true; h := b.hash; }`)
	// Bool hash uses hardcoded constants via select, not fnv1a
	codegentest.AssertContains(t, ir, "select i1")
	codegentest.AssertNotContains(t, ir, "call i64 @__mod_std__fnv1a_hash")
}

func TestHashGetterBoolFalse(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { b := false; h := b.hash; }`)
	codegentest.AssertContains(t, ir, "select i1")
}

func TestHashGetterBoolInFunction(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		hash_it(bool b) int { return b.hash; }
		main() {}
	`)
	codegentest.AssertContains(t, ir, "select i1")
	codegentest.AssertNotContains(t, ir, "call i64 @__mod_std__fnv1a_hash")
}

func TestHashGetterBoolNoZext(t *testing.T) {
	// Bool hash should not zext to i64 anymore
	ir := codegentest.GenerateIR(t, `main() { h := true.hash; }`)
	codegentest.AssertContains(t, ir, "select i1")
}

func TestHashGetterBoolTrueAndFalseDifferentConstants(t *testing.T) {
	// Both constants should appear in the select instruction
	ir := codegentest.GenerateIR(t, `main() { h := true.hash; }`)
	codegentest.AssertContains(t, ir, "5871781006564002453") // 0x517cc1b727220a95
	codegentest.AssertContains(t, ir, "7809847782465536322") // 0x6c62272e07bb0142
}

func TestHashGetterIntStillUsesFnv1a(t *testing.T) {
	// Verify other types still use fnv1a (regression check)
	ir := codegentest.GenerateIR(t, `main() { h := 42.hash; }`)
	codegentest.AssertContains(t, ir, "call i64 @__mod_std__fnv1a_hash")
}

func TestHashGetterFloat(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test(f64 x) int { return x.hash; }
		main() {}
	`)
	codegentest.AssertContains(t, ir, "bitcast double")
	codegentest.AssertContains(t, ir, "call i64 @__mod_std__fnv1a_hash(i64")
}

func TestBitsGetterF64(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test(f64 x) uint { return x.bits; }
		main() {}
	`)
	codegentest.AssertContains(t, ir, "bitcast double")
	// bits getter returns the raw i64 — no hash call
	codegentest.AssertNotContains(t, ir, "call i64 @__mod_std__fnv1a_hash")
}

func TestHashGetterSmallInt(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test(i8 x) int { return x.hash; }
		main() {}
	`)
	codegentest.AssertContains(t, ir, "sext i8")
	codegentest.AssertContains(t, ir, "call i64 @__mod_std__fnv1a_hash(i64")
}

func TestHashGetterSmallUint(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test(u8 x) int { return x.hash; }
		main() {}
	`)
	// Unsigned types use zero-extend, not sign-extend
	codegentest.AssertContains(t, ir, "zext i8")
	codegentest.AssertContains(t, ir, "call i64 @__mod_std__fnv1a_hash(i64")
}

// T1105: `go obj.method(...)` returning a heap user type via the
// genGoCallExprViaBlock path is the sibling of the T0686 go-block heapTemps bug.
// The method's struct result registers a heap temp whose alloca/dropFlag belong
// to the inner `.goroutine.N` frame; without isolation those temps leaked into
// the outer `.goroutine.main` coroutine, where the printer serialized them as
// `%0` — the coro.id token — producing `load i1, i1* %0` (malformed IR / stack
// overflow). Guards the producer-side isolation of heapTemps/heapTempMap.
func TestT1105_GoMethodStructResultNoTokenLoad(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type R { int n; }
		type W { f(this, int x) R { return R(n: x); } }
		main() { W w = W(); t := go w.f(5); r := <-t; }
	`)
	defStart := strings.Index(ir, "define i8* @.goroutine.main(")
	if defStart < 0 {
		t.Fatal("expected a .goroutine.main coroutine definition in the IR")
	}
	body := ir[defStart:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end+2]
	}
	codegentest.AssertContains(t, body, "call token @llvm.coro.id") // %0 is the token
	codegentest.AssertNotContains(t, body, "i1* %0")                // no heap drop-flag load from the token
	codegentest.AssertNotContains(t, body, "i8** %0")               // no heap pointer load from the token
	// Positive guards: the via-block coroutine still exists and the caller
	// allocates a real result buffer (the ViaBlock path stores into G.result_ptr
	// inline, so there is no separate `store_result:` block as in the go-block form).
	codegentest.AssertContains(t, ir, "define i8* @.goroutine.")
	codegentest.AssertContains(t, ir, "@pal_alloc")
}

// T1261: a `go` expression whose operand is a METHOD/COMPLEX call capturing
// `this` (routed through genGoCallExprViaBlock — the CALL form, distinct from
// T1219's block form) used to load the borrowed `this` pointer and re-deref
// `this.field` inside the coroutine → UAF/garbage read. The fix mirrors T1219:
// the spawning method deep-copies the heap receiver (heapdup) before the ramp,
// the coroutine takes `this` as a `{ i8*, i8* }` value-struct capture param, and
// the coro body sets up the `this.val.addr`/`this.addr` snapshot allocas.
func TestT1261_GoCallThisHeapReceiverSnapshot(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1261C {
			int n;
			spawn(this, Channel[int] out) {
				go out.send(this.n);
			}
		}
		main() {
			c := T1261C(n: 5);
			d := channel[int](capacity: 1);
			c.spawn(d);
			r := <-d;
			d.close();
		}
	`)
	// The coroutine takes `this` as a value-struct capture param.
	codegentest.AssertContains(t, ir, "%this.cap")
	codegentest.AssertContains(t, ir, "{ i8*, i8* } %this.cap")
	// The coro body materializes the snapshot allocas (not a bare load of a
	// borrowed `this`).
	codegentest.AssertContains(t, ir, "%this.val.addr")
	codegentest.AssertContains(t, ir, "%this.addr")
	// The spawning method deep-copies the receiver before the goroutine ramp.
	spawnIR := codegentest.ExtractFunction(ir, "T1261C.spawn")
	codegentest.AssertContains(t, spawnIR, "heapdup.copy")
	codegentest.AssertContains(t, spawnIR, "call i8* @.goroutine.")
	dupIdx := strings.Index(spawnIR, "heapdup.copy")
	rampIdx := strings.Index(spawnIR, "call i8* @.goroutine.")
	if dupIdx < 0 || rampIdx < 0 || dupIdx > rampIdx {
		t.Fatalf("expected receiver heapdup before goroutine ramp in spawn:\n%s", spawnIR)
	}
}

// T1261: a VALUE-TYPE receiver captured by the `go`-CALL form is copied by value
// into the coroutine frame (Copy — no heap dup), mirroring T1219's value path.
func TestT1261_GoCallThisValueReceiverByValue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1261P {
			int x `+"`"+`value;
			int y `+"`"+`value;
			sum(this, Channel[int] out) {
				go out.send(this.x + this.y);
			}
		}
		main() {
			p := T1261P(x: 3, y: 4);
			d := channel[int](capacity: 1);
			p.sum(d);
			r := <-d;
			d.close();
		}
	`)
	codegentest.AssertContains(t, ir, "%this.cap")
	codegentest.AssertContains(t, ir, "%this.val.addr")
	sumIR := codegentest.ExtractFunction(ir, "T1261P.sum")
	codegentest.AssertContains(t, sumIR, "call i8* @.goroutine.")
	codegentest.AssertNotContains(t, sumIR, "heapdup.copy")
}

// --- Named Arguments Tests ---

func TestNamedArgsConstructorCodegen(t *testing.T) {
	// Named args in reverse order should produce correct field stores
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; }
		main() {
			p := Point(y: 20, x: 10);
		}
	`)
	// Both fields should be stored
	codegentest.AssertContains(t, ir, "store")
}

func TestNamedArgsConstructorPositionalCodegen(t *testing.T) {
	// All positional args should work for constructors
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; }
		main() {
			p := Point(10, 20);
		}
	`)
	codegentest.AssertContains(t, ir, "store")
}

func TestNamedArgsFunctionCallCodegen(t *testing.T) {
	// Named args reordered should generate correct call
	ir := codegentest.GenerateIR(t, `
		add(int a, int b) int { return a + b; }
		main() {
			r := add(b: 2, a: 1);
		}
	`)
	codegentest.AssertContains(t, ir, "call")
	codegentest.AssertContains(t, ir, "@__user.add")
}

func TestNamedArgsMethodCallCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Calc {
			int value;
			compute(int a, int b) int { return a + b; }
		}
		main() {
			c := Calc(value: 0);
			r := c.compute(b: 2, a: 1);
		}
	`)
	codegentest.AssertContains(t, ir, "Calc.compute")
}

func TestNamedArgsMixedPositionalNamedCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		calc(int a, int b, int c) int { return a + b + c; }
		main() {
			r := calc(1, c: 3, b: 2);
		}
	`)
	codegentest.AssertContains(t, ir, "@__user.calc")
}

func TestValueTypeNewConstructor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Clamped {
			int value `+"`value"+`;
			new(~this, int v) {
				if v < 0 { this.value = 0; }
				else { this.value = v; }
			}
		}
		main() {
			Clamped c = Clamped(v: 42);
		}
	`)
	// new() constructor called, value loaded from alloca after
	codegentest.AssertContains(t, ir, "call void @Clamped.new(")
	codegentest.AssertContains(t, ir, "load %promise_Clamped_v")
}

func TestVariadicMethodIR(t *testing.T) {
	// Variadic method: receiver + variadic param in IR.
	ir := codegentest.GenerateIR(t, `
		type Adder {
			int base;

			addAll(this, ...int values) int {
				return this.base;
			}
		}
		main() {
			a := Adder(base: 10);
			a.addAll(1, 2, 3);
		}
	`)
	// Method takes instance ptr + vector param
	codegentest.AssertContains(t, ir, "define i64 @Adder.addAll(")
	codegentest.AssertContains(t, ir, "i8* %values")
}

func TestGlobalMethodIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, "type Counter {\n"+
		"int value;\n"+
		"create(int v) Counter `global {\n"+
		"return Counter(value: v);\n"+
		"}\n"+
		"}\n"+
		"main() {\n"+
		"c := Counter.create(42);\n"+
		"}\n")
	// Global method should be defined as Counter.create with no 'this' parameter
	codegentest.AssertContains(t, ir, "Counter.create")
	// Should only have the 'v' param, not 'this'
	codegentest.AssertNotContains(t, ir, "Counter.create(i8*")
}

// TestGlobalSetterIR verifies that `global setters (T0703) emit a function
// with no receiver param and that `Type.name = v` lowers to a direct call
// passing only the value argument. Uses a matching `global getter because
// setter-only properties don't currently parse as l-value targets (sema
// looks them up through LookupGetter).
func TestGlobalSetterIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, "type Foo {\n"+
		"int x;\n"+
		"get count int `global { return 0; }\n"+
		"set count(int v) `global { }\n"+
		"}\n"+
		"main() {\n"+
		"Foo.count = 7;\n"+
		"}\n")
	// Setter is defined with the $set mangle suffix and a single i64 param (no this).
	codegentest.AssertContains(t, ir, "define void @Foo.count$set(i64 %v)")
	codegentest.AssertNotContains(t, ir, "@Foo.count$set(i8*")
	// Call site lowers to a direct call with the value only.
	codegentest.AssertContains(t, ir, "call void @Foo.count$set(i64 7)")
}

// TestGlobalGetterSetterPairIR verifies that a `global getter and setter on
// the same name coexist (the setter mangles with `$set` so there's no clash)
// and dispatch correctly from `Type.name` reads and `Type.name = v` writes.
func TestGlobalGetterSetterPairIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, "type Foo {\n"+
		"int x;\n"+
		"get count int `global { return 0; }\n"+
		"set count(int v) `global { }\n"+
		"}\n"+
		"main() {\n"+
		"Foo.count = 3;\n"+
		"n := Foo.count;\n"+
		"}\n")
	codegentest.AssertContains(t, ir, "define i64 @Foo.count()")
	codegentest.AssertContains(t, ir, "define void @Foo.count$set(i64 %v)")
	codegentest.AssertContains(t, ir, "call void @Foo.count$set(i64 3)")
	codegentest.AssertContains(t, ir, "call i64 @Foo.count()")
}

// TestGlobalSetterCompoundAssignIR verifies that compound assignment
// (`Type.name += v`) on a `global getter/setter pair reads through the
// global getter, applies the op, and writes through the global setter.
// Exercises the interaction between genGetterCall's and genSetterCall's
// global branches via genMemberAssign's compound-op path.
func TestGlobalSetterCompoundAssignIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, "type Foo {\n"+
		"int x;\n"+
		"get count int `global { return 0; }\n"+
		"set count(int v) `global { }\n"+
		"}\n"+
		"main() {\n"+
		"Foo.count += 5;\n"+
		"}\n")
	// Compound assignment lowers to: load via global getter, add, store via global setter.
	codegentest.AssertContains(t, ir, "call i64 @Foo.count()")
	codegentest.AssertContains(t, ir, "call void @Foo.count$set(i64")
}

// TestPropertyIncDecIR: T0712. `f.count++` on a getter/setter property (no
// backing field) must read via the getter and write via the setter, not panic
// in genFieldPtr.
func TestPropertyIncDecIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo {
			int x;
			get count int { return this.x; }
			set count(int v) { this.x = v; }
		}
		main() {
			f := Foo(x: 1);
			f.count++;
		}
	`)
	codegentest.AssertContains(t, ir, "call i64 @Foo.count(")
	codegentest.AssertContains(t, ir, "add i64")
	codegentest.AssertContains(t, ir, "call void @Foo.count$set(")
}

// TestPropertyDecIR: T0712. `f.count--` lowers to a subtraction.
func TestPropertyDecIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo {
			int x;
			get count int { return this.x; }
			set count(int v) { this.x = v; }
		}
		main() {
			f := Foo(x: 1);
			f.count--;
		}
	`)
	codegentest.AssertContains(t, ir, "call i64 @Foo.count(")
	codegentest.AssertContains(t, ir, "sub i64")
	codegentest.AssertContains(t, ir, "call void @Foo.count$set(")
}

// T0494: Getter returning a heap user type ({i8*, i8*}) used as a method-chain
// receiver must register a heap temp drop so the cloned value is freed.
func TestGetterUserHeapResultTrackedInChain(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Inner { string _name; drop(~this) {}
			ok(this) bool { return true; }
		}
		type Outer { Inner _inner;
			get inner Inner `+"`public"+` => Inner(_name: this._inner._name);
		}
		test() {
			Outer o = Outer(_inner: Inner(_name: "hi"));
			bool b = o.inner.ok();
		}
	`)
	// The cloned Inner returned by the getter must be tracked as a heap temp
	// (heap.drop block) and freed with the type's drop function at stmt end.
	codegentest.AssertContains(t, ir, "define void @__user.test()")
	codegentest.AssertContains(t, ir, "heap.drop")
	codegentest.AssertContains(t, ir, "Inner.drop")
}

// T0494: A getter returning a non-droppable primitive must NOT add any new
// drop tracking — the original B0290 sliver only fired for strings.
func TestGetterPrimitiveResultNotTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter { int _n;
			get n int => this._n;
		}
		test() {
			Counter c = Counter(_n: 5);
			int v = c.n;
		}
	`)
	fn := codegentest.ExtractFunction(ir, "__user.test")
	// No new heap.drop or stmt-temp drop should appear for the int getter.
	codegentest.AssertNotContains(t, fn, "promise_string_drop")
	codegentest.AssertNotContains(t, fn, "Vector.drop")
}

// T0031: Directory embed getter constructs EmbeddedFiles value
func TestEmbedDirGetter(t *testing.T) {
	file, info := codegentest.ParseWithStd(t, `
		get assets EmbeddedFiles `+"`embed(\"static/...\")"+`;
		main() {
			EmbeddedFiles fs = assets;
		}
	`)
	// Manually populate embed data (normally done by ResolveEmbeds)
	for _, embed := range info.Embeds {
		embed.Kind = sema.EmbedDir
		embed.Data = []byte("body{}hello")
		embed.DirEntries = []sema.EmbedDirEntry{
			{Path: "index.html", Name: "index.html", Size: 5, Offset: 5},
			{Path: "style.css", Name: "style.css", Size: 6, Offset: 0},
		}
	}
	result := codegen.Compile(file, info, "")
	ir := result.Module.String()
	// Should contain allocations and string_new for file paths
	codegentest.AssertContains(t, ir, "@pal_alloc")
	codegentest.AssertContains(t, ir, "@promise_string_new")
	// Should contain the data blob
	codegentest.AssertContains(t, ir, "body{}hello")
	// Should return user value type {i8*, i8*}
	codegentest.AssertContains(t, ir, "define { i8*, i8* } @__user.assets()")
}

func TestEmbedDirGetterEmpty(t *testing.T) {
	file, info := codegentest.ParseWithStd(t, `
		get assets EmbeddedFiles `+"`embed(\"empty/...\")"+`;
		main() {
			EmbeddedFiles fs = assets;
		}
	`)
	for _, embed := range info.Embeds {
		embed.Kind = sema.EmbedDir
		embed.Data = []byte{}
		embed.DirEntries = nil
	}
	result := codegen.Compile(file, info, "")
	ir := result.Module.String()
	// Even empty dir should produce valid IR with allocations
	codegentest.AssertContains(t, ir, "define { i8*, i8* } @__user.assets()")
	codegentest.AssertContains(t, ir, "@pal_alloc")
}

// B0226: promise_type_is should use updated field indices (typeID at field 2).
func TestTypeIsFieldIndicesB0226(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal { string name; speak(this) string `+"`abstract"+`; }
		type Dog is Animal { speak(this) string { return "woof"; } }
		main() {
			Animal a = Dog(name: "Rex");
			if a is Dog { }
		}
	`)
	codegentest.AssertContains(t, ir, "define i32 @promise_type_is")
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is")
}

// T0735: When a map literal is passed as a borrowed arg to a CONSTRUCTOR, the
// constructor claims the heap temp into the field (heap.claim block). The
// stmt-temp drop is cleared, ownership transfers to the new instance's drop.
func TestT0735_MapLitInCtorFieldClaimed(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _Box { Map[string, int] m; }
		main() {
			_Box b = _Box(m: {"a": 1});
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
	body := rest[:end+2]
	// The map literal heap-temp flag is cleared (claimed) when stored into the
	// _Box.m field — the _Box's drop takes over ownership. Asserting inside the
	// user's main body (not anywhere in the module IR) avoids false positives
	// from std-library heap.claim blocks.
	codegentest.AssertContains(t, body, "heap.claim")
}

// B0325: Field access on a call result must track the intermediate heap instance.
func TestFieldAccessOnCallResultTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pair { int x; int y; }
		make_pair() Pair { return Pair(x: 10, y: 20); }
		test() {
			int v = make_pair().x;
		}
	`)
	codegentest.AssertContains(t, ir, "heap.drop")
	codegentest.AssertContains(t, ir, "call void @pal_free(")
}

// --- T0613: paren-wrapped `this` as a method/field/operator/RTTI receiver ---
//
// genExpr already evaluates ParenExpr transparently, so the receiver *value* for
// `(this)` is byte-identical to bare `this` (a raw i8* instance pointer). The bug
// was in the AST-shape dispatch gates that matched `*ast.ThisExpr` directly: for
// `*ast.ParenExpr{ThisExpr}` they fell through and ran `extractvalue i8* %p, 1`
// on the raw pointer, which opt rejects ("extractvalue operand must be aggregate
// type"). The fix peels ParenExpr at each gate via isThisReceiver(). The
// universal invariant below — valid IR never extracts a field from an `i8*` — is
// the cheap cross-cutting assertion for every category.

func TestParenThisMethodCallPassesReceiver(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0613MBox {
			int x;
			self(~this) T0613MBox { return this; }
			driver(~this) T0613MBox { r := (this).self(); return r; }
		}
		main() { b := T0613MBox(x: 1); r := b.driver(); }
	`)
	codegentest.AssertNotContains(t, ir, "extractvalue i8*")
	driver := codegentest.ExtractFunction(ir, "T0613MBox.driver")
	if driver == "" {
		t.Fatal("expected T0613MBox.driver in IR")
	}
	// The paren-wrapped `this` receiver is passed directly as the i8* receiver.
	codegentest.AssertContains(t, driver, "@T0613MBox.self(i8*")
}

func TestParenThisFieldReadNoExtractFromPtr(t *testing.T) {
	// Heap type field read through (this).
	irHeap := codegentest.GenerateIR(t, `
		type T0613FHeap {
			string s;
			read(this) string `+"`structural(protocol: false)"+` { return (this).s; }
		}
		main() { b := T0613FHeap(s: "x"); v := b.read(); }
	`)
	codegentest.AssertNotContains(t, irHeap, "extractvalue i8*")
	if codegentest.ExtractFunction(irHeap, "T0613FHeap.read") == "" {
		t.Fatal("expected T0613FHeap.read in IR")
	}
	// Value type field read through (this) and ((this)).
	irVal := codegentest.GenerateIR(t, `
		type T0613FVal {
			int x `+"`value"+`;
			int y `+"`value"+`;
			read_x(this) int { return (this).x; }
			read_y(this) int { return ((this)).y; }
		}
		main() { p := T0613FVal(x: 3, y: 4); a := p.read_x(); b := p.read_y(); }
	`)
	codegentest.AssertNotContains(t, irVal, "extractvalue i8*")
	if codegentest.ExtractFunction(irVal, "T0613FVal.read_x") == "" {
		t.Fatal("expected T0613FVal.read_x in IR")
	}
}

func TestParenThisSetterNoExtractFromPtr(t *testing.T) {
	// Direct setter through (this), plus plain field assignment through (this).
	irDirect := codegentest.GenerateIR(t, `
		type T0613SBox {
			int n;
			get value int { return this.n; }
			set value(int v) { this.n = v; }
			set_via(~this, int v) { (this).value = v; }
			bump(~this, int v) { (this).n = v; }
		}
		main() { b := T0613SBox(n: 0); b.set_via(9); b.bump(7); }
	`)
	codegentest.AssertNotContains(t, irDirect, "extractvalue i8*")
	if codegentest.ExtractFunction(irDirect, "T0613SBox.set_via") == "" {
		t.Fatal("expected T0613SBox.set_via in IR")
	}
	// Virtual setter (base has a child → vtable dispatch) through (this).
	irVirtual := codegentest.GenerateIR(t, `
		type T0613VSBase {
			int n;
			get value int { return this.n; }
			set value(int v) { this.n = v; }
			scale_via(~this, int v) { (this).value = v; }
		}
		type T0613VSDerived is T0613VSBase {}
		main() { d := T0613VSDerived(n: 0); d.scale_via(11); }
	`)
	codegentest.AssertNotContains(t, irVirtual, "extractvalue i8*")
}

func TestParenThisGetterNoExtractFromPtr(t *testing.T) {
	// Direct getter through (this).
	irDirect := codegentest.GenerateIR(t, `
		type T0613GBox {
			int n;
			get value int { return this.n; }
			get_via(this) int { return (this).value; }
		}
		main() { b := T0613GBox(n: 5); v := b.get_via(); }
	`)
	codegentest.AssertNotContains(t, irDirect, "extractvalue i8*")
	if codegentest.ExtractFunction(irDirect, "T0613GBox.get_via") == "" {
		t.Fatal("expected T0613GBox.get_via in IR")
	}
	// Virtual getter (base has a child → vtable dispatch) through (this).
	irVirtual := codegentest.GenerateIR(t, `
		type T0613VGBase {
			int n;
			get value int { return this.n; }
			get_via(this) int { return (this).value; }
		}
		type T0613VGDerived is T0613VGBase {}
		main() { d := T0613VGDerived(n: 13); v := d.get_via(); }
	`)
	codegentest.AssertNotContains(t, irVirtual, "extractvalue i8*")
}

func TestParenThisIsCheckNoExtractFromPtr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0613Animal {
			string name;
			speak(this) string `+"`abstract"+`;
			am_i_dog(this) bool { return (this) is T0613Dog; }
		}
		type T0613Dog is T0613Animal { speak(this) string { return "Woof"; } }
		main() {
			T0613Animal d = T0613Dog(name: "Rex");
			b := d.am_i_dog();
		}
	`)
	codegentest.AssertNotContains(t, ir, "extractvalue i8*")
	if codegentest.ExtractFunction(ir, "T0613Animal.am_i_dog") == "" {
		t.Fatal("expected T0613Animal.am_i_dog in IR")
	}
}

// genFieldPtr value-type lvalue gate (compound assign through `(this)`):
// `(this).x += d` on a value type takes the field-ptr lvalue path. Without the
// peel this panics ("value type field assignment requires addressable target"),
// so the test passing at all (generateIR not panicking) is the regression guard.
// Runtime mutation is a no-op (value types are copy semantics), so this is an
// IR-shape test only — no runtime e2e companion.
func TestParenThisValueFieldPtrLvalueNoExtractFromPtr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0613VTField {
			int x `+"`value"+`;
			int y `+"`value"+`;
			bump(~this, int d) { (this).x += d; }
		}
		main() {
			v := T0613VTField(x: 1, y: 2);
			v.bump(5);
		}
	`)
	codegentest.AssertNotContains(t, ir, "extractvalue i8*")
	if codegentest.ExtractFunction(ir, "T0613VTField.bump") == "" {
		t.Fatal("expected T0613VTField.bump in IR")
	}
}

// T1901: Assigning a heap string through a user-defined setter must NOT dup
// at the call site — the setter body borrows the value and dups internally.
// The caller's copy is freed by normal cleanup (stmtTemp or drop binding).
func TestSetterStringAssignNoDupAtCallSite(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1901Box {
			string _v;
			get text string => this._v;
			set text(string s) { this._v = s; }
			drop(~this) {}
		}
		make_string() string { return "abc"; }
		main() {
			b := T1901Box(_v: "z");
			b.text = make_string();
		}
	`)
	// The setter must be called.
	codegentest.AssertContains(t, ir, "call void @T1901Box.text$set(")
	// The caller must NOT dup the string before passing to the setter —
	// the setter borrows and dups internally. A dup at the call site would leak.
	// Check that promise_string_dup does not appear near the setter call in the IR.
	// The setter body itself will dup, but the caller site should not.
	setterCallIdx := strings.Index(ir, "call void @T1901Box.text$set(")
	if setterCallIdx < 0 {
		t.Fatal("expected setter call in IR")
	}
	// Look at the 500 chars before the setter call for a dup — this covers
	// the RHS evaluation in the same basic block.
	start := setterCallIdx - 500
	if start < 0 {
		start = 0
	}
	callSiteRegion := ir[start:setterCallIdx]
	if strings.Contains(callSiteRegion, "promise_string_dup") {
		t.Errorf("T1901: caller should not dup string before setter call;\n%s", callSiteRegion)
	}
}

// T1901: Compound assignment (+=) through a string setter must track the
// concat result as a string temp so it is freed after the setter returns.
func TestSetterStringCompoundTrackTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1901Counter {
			string _v;
			get text string => this._v;
			set text(string s) { this._v = s; }
			drop(~this) {}
		}
		main() {
			c := T1901Counter(_v: "x");
			c.text += "y";
		}
	`)
	// Both getter and setter must be called for compound assignment.
	codegentest.AssertContains(t, ir, "call i8* @T1901Counter.text(")
	codegentest.AssertContains(t, ir, "call void @T1901Counter.text$set(")
	// The concat result must be tracked as a temp and dropped.
	// The string_drop call appears in the IR (either in the goroutine body
	// or cleanup blocks) to free the temporary concat buffer.
	codegentest.AssertContains(t, ir, "promise_string_drop")
}
