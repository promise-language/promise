package generic1

import (
	"fmt"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

func TestInferredVarDecl(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := 42; }`)
	codegentest.AssertContains(t, ir, "alloca i64")
	codegentest.AssertContains(t, ir, "store i64 42")
}

func TestGenericFactoryCallCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Parseable `+"`"+`structural {
			parse(string data) `+"`"+`abstract `+"`"+`factory;
		}
		type My {
			parse(string data) My `+"`"+`factory { return My(); }
		}
		load[T: Parseable](string data) T {
			return T.parse(data);
		}
		main() {
			My m = load[My]("hello");
		}
	`)
	// Monomorphized load[My] should call My.parse directly
	codegentest.AssertContains(t, ir, "call { i8*, i8* } @My.parse(")
}

func TestSelfGenericFactoryCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			T value;
			new(~this, T v) { this.value = v; }
			wrap(T v) Self `+"`"+`factory {
				return Self(v: v);
			}
		}
		main() {
			Box[int] b = Box[int].wrap(v: 42);
		}
	`)
	// Factory should be monomorphized for int
	codegentest.AssertContains(t, ir, "@\"Box[int].wrap\"(")
	// Should call the Box[int] constructor
	codegentest.AssertContains(t, ir, "@\"Box[int].new\"(")
}

func TestSelfGenericMethodReturnCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			T value;
			new(~this, T v) { this.value = v; }
			rewrap(T v) Self {
				return Self(v: v);
			}
		}
		main() {
			Box[int] b = Box[int](v: 1);
			Box[int] c = b.rewrap(v: 2);
		}
	`)
	// Instance method should exist for int monomorphization
	codegentest.AssertContains(t, ir, "@\"Box[int].rewrap\"(")
}

func TestSelfGenericMultiParamCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pair[A, B] {
			A first;
			B second;
			new(~this, A a, B b) { this.first = a; this.second = b; }
			make(A a, B b) Self `+"`"+`factory {
				return Self(a: a, b: b);
			}
		}
		main() {
			Pair[int, string] p = Pair[int, string].make(a: 1, b: "x");
		}
	`)
	// Factory monomorphized for (int, string)
	codegentest.AssertContains(t, ir, "@\"Pair[int, string].make\"(")
	codegentest.AssertContains(t, ir, "@\"Pair[int, string].new\"(")
}

// T0474: super(v) in a generic Child[T] is Base[T] constructor must target the
// monomorphized parent constructor (Base__int.new), not the bare-name Base.new.
func TestSuperCallGenericParentCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base[T] {
			T value;
			new(~this, T move v) {
				this.value = v;
			}
		}
		type Child[T] is Base[T] {
			int extra;
			new(~this, T move v, int e) {
				super(move v);
				this.extra = e;
			}
		}
		main() {
			Child[int] c = Child[int](v: 5, e: 7);
		}
	`)
	// Child[int].new should call the monomorphized parent constructor.
	codegentest.AssertContains(t, ir, `call void @"Base[int].new"(`)
	codegentest.AssertContains(t, ir, `call void @"Child[int].new"(`)
}

// T0474: with reordered child type params (Child[A, B] is Base[B]), super(b)
// must name the parent constructor monomorphized on B (Base[int]), not A.
func TestSuperCallGenericReorderedParentCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base[T] {
			T value;
			new(~this, T move v) {
				this.value = v;
			}
		}
		type Child[A, B] is Base[B] {
			A first;
			new(~this, A move a, B move b) {
				super(move b);
				this.first = a;
			}
		}
		main() {
			Child[string, int] c = Child[string, int](a: "x", b: 9);
		}
	`)
	// B=int → parent monomorphized as Base[int], not Base[string].
	codegentest.AssertContains(t, ir, `call void @"Base[int].new"(`)
	codegentest.AssertContains(t, ir, `call void @"Child[string, int].new"(`)
}

// T0761: an Optional cast inside a GENERIC function body. The body is codegen'd
// with c.typeSubst active (monomorphization), exercising genOptionalCastExpr's
// type-substitution branches. The optional source is a LOCAL (a parameter source
// hits the pre-existing T0811 parameter segfault). Both `as` and `as!` paths.
func TestOptionalSubjectGenericBodyCast(t *testing.T) {
	src := `
		type Base { string name; tag(this) string ` + "`" + `abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		gcast[T](T marker) %s {
			_ := marker;
			Base b = Der(name: "g");
			Base? oo = b;
			%s
		}
		main() { %s }
	`
	// Optional: monomorphized gcast__int emits the optcast some/none/merge path.
	irOpt := codegentest.GenerateIR(t, fmt.Sprintf(src, "Der?", `return oo as Der;`,
		`Der? d = gcast(0); if d { }`))
	codegentest.AssertContains(t, irOpt, "optcast.check")
	codegentest.AssertContains(t, irOpt, "optcast.some")
	codegentest.AssertContains(t, irOpt, "call i32 @promise_type_is(")
	// Force: monomorphized gcast__int emits the present/panic path.
	irForce := codegentest.GenerateIR(t, fmt.Sprintf(src, "string",
		`d := oo as! Der; return d.name;`, `_ := gcast(0);`))
	codegentest.AssertContains(t, irForce, "optcast.present")
	codegentest.AssertContains(t, irForce, "optcast.nonepanic")
	// Generic body + aliasing (index) source: the dup and heap-temp registration
	// both run their `c.typeSubst != nil` substitution branches.
	irIndex := codegentest.GenerateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		gidx[T](T marker) Der? {
			_ := marker;
			Base?[] v = [];
			Base b = Der(name: "g");
			v.push(b);
			return v[0] as Der;
		}
		main() { Der? d = gidx(0); if d { } }
	`)
	codegentest.AssertContains(t, irIndex, "optcast.check")
	codegentest.AssertContains(t, irIndex, "heapdup.copy") // duped aliasing source inside the generic body
}

// --- Generic type tests ---

func TestGenericTypeLayout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 42);
		}
	`)
	codegentest.AssertContains(t, ir, "Box[int]_i")
	codegentest.AssertContains(t, ir, "store i64 42")
}

func TestGenericFieldAccess(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 42);
			int v = b.value;
		}
	`)
	codegentest.AssertContains(t, ir, "Box[int]_i")
	// Field access should load i64 (not i8*)
	codegentest.AssertContains(t, ir, "load i64")
}

func TestGenericFieldAssign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 42);
			b.value = 10;
		}
	`)
	codegentest.AssertContains(t, ir, "store i64 10")
}

func TestGenericMethod(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			T value;
			get(this) T { return this.value; }
		}
		main() {
			b := Box[int](value: 42);
			int v = b.get();
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @\"Box[int].get\"")
}

func TestGenericMethodSet(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			T value;
			set(~this, T val) { this.value = val; }
		}
		main() {
			b := Box[int](value: 42);
			b.set(10);
		}
	`)
	codegentest.AssertContains(t, ir, "define void @\"Box[int].set\"")
}

func TestGenericMultipleInstances(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		main() {
			a := Box[int](value: 42);
			b := Box[string](value: "hi");
		}
	`)
	codegentest.AssertContains(t, ir, "Box[int]_i")
	codegentest.AssertContains(t, ir, "Box[string]_i")
}

func TestGenericNestedField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		main() {
			a := Box[int](value: 1);
			b := Box[string](value: "hi");
			int x = a.value;
			string y = b.value;
		}
	`)
	// Both Box[int] and Box[string] fields accessed with correct types
	codegentest.AssertContains(t, ir, "Box[int]_i")
	codegentest.AssertContains(t, ir, "Box[string]_i")
	codegentest.AssertContains(t, ir, "load i64")
	codegentest.AssertContains(t, ir, "load i8*")
}

func TestGenericEnum(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].Some(42);
		}
	`)
	codegentest.AssertContains(t, ir, "Option[int]_enum")
	codegentest.AssertContains(t, ir, "store i64 42")
}

func TestGenericEnumNone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].None;
		}
	`)
	codegentest.AssertContains(t, ir, "Option[int]_enum")
}

func TestGenericEnumMatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].Some(42);
			r := match x {
				Some(v) => v,
				_ => 0,
			};
		}
	`)
	codegentest.AssertContains(t, ir, "switch i32")
}

func TestGenericEnumFieldless(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Dir[T] { Left, Right }
		main() {
			d := Dir[int].Left;
		}
	`)
	// Fieldless generic enum: internal type is i32
	codegentest.AssertContains(t, ir, "i32 0")
}

// TestGenericEnumMethodGetter verifies that a getter method on a generic enum
// is correctly monomorphized: the function is declared with the mono-qualified name.
func TestGenericEnumMethodGetter(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Wrapper[T] {
			Some(T value),
			Empty,

			get is_some bool {
				match this {
					Some(_) => {
						return true;
					},
					Empty => {
						return false;
					},
				}
			}
		}
		main() {
			w := Wrapper[int].Some(value: 42);
			b := w.is_some;
		}
	`)
	// Mono method declared with instance-qualified name
	codegentest.AssertContains(t, ir, `"Wrapper[int].is_some"`)
	// Enum layout exists
	codegentest.AssertContains(t, ir, "Wrapper[int]_v")
}

// TestGenericEnumMethodRegular verifies that a regular (non-getter) method on a
// generic enum is monomorphized correctly.
func TestGenericEnumMethodRegular(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Box[T] {
			Full(T item),
			Vacant,

			unwrap_or(this, T fallback) T {
				match this {
					Full(v) => {
						return v;
					},
					Vacant => {
						return fallback;
					},
				}
			}
		}
		main() {
			b := Box[int].Full(item: 99);
			x := b.unwrap_or(0);
		}
	`)
	codegentest.AssertContains(t, ir, `"Box[int].unwrap_or"`)
}

// TestGenericEnumMethodCallsMethod verifies that a mono enum method body
// can call another method on the same enum via this.
func TestGenericEnumMethodCallsMethod(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Status[T] {
			Ok(T data),
			Err(string msg),

			get is_ok bool {
				match this {
					Ok(_) => {
						return true;
					},
					Err(_) => {
						return false;
					},
				}
			}

			get is_err bool {
				return !this.is_ok;
			}
		}
		main() {
			s := Status[int].Ok(data: 42);
			b := s.is_err;
		}
	`)
	// Both methods should be declared
	codegentest.AssertContains(t, ir, `"Status[int].is_ok"`)
	codegentest.AssertContains(t, ir, `"Status[int].is_err"`)
	// is_err calls is_ok
	codegentest.AssertContains(t, ir, `call i1 @"Status[int].is_ok"`)
}

// TestGenericEnumMultipleInstantiations verifies that methods are monomorphized
// separately for each type argument.
func TestGenericEnumMultipleInstantiations(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Opt[T] {
			Some(T value),
			None,

			get has_value bool {
				match this {
					Some(_) => {
						return true;
					},
					None => {
						return false;
					},
				}
			}
		}
		main() {
			a := Opt[int].Some(value: 1);
			b := Opt[string].None;
			x := a.has_value;
			y := b.has_value;
		}
	`)
	// Both instantiations get their own method
	codegentest.AssertContains(t, ir, `"Opt[int].has_value"`)
	codegentest.AssertContains(t, ir, `"Opt[string].has_value"`)
}

// TestT0636_EnumGenericMethodInstanceMono verifies that a generic
// (method-level type param) method on a generic enum instance is emitted as a
// monomorphized function "Box[int].transform[int]", with a separate mono
// function per (owner, method-type-arg) combination.
func TestT0636_EnumGenericMethodInstanceMono(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Box[T] {
			V(Vector[T] d),
			N,
			transform[U](U _x) int {
				match this {
					V(d) => { return d.len; },
					N => { return 0; },
				}
			}
		}
		main() {
			b := Box[int].V([1, 2, 3]);
			x := b.transform[int](5);
			s := Box[string].V(["a"]);
			y := s.transform[bool](true);
		}
	`)
	// Per-(owner, method-type-arg) mono functions.
	codegentest.AssertContains(t, ir, `"Box[int].transform[int]"`)
	codegentest.AssertContains(t, ir, `"Box[string].transform[bool]"`)
	// The call site dispatches to the monomorphized enum method.
	codegentest.AssertContains(t, ir, `call i64 @"Box[int].transform[int]"`)
}

// TestT0636_NonGenericEnumGenericMethodMono verifies that a generic
// (method-level type param) method on a *non-generic* enum is emitted as a
// monomorphized function named with the bare enum name (no monoName / "[..]"
// owner suffix), exercising the `case *types.Enum` arm of
// genGenericEnumMethodCall (enumName = enum.Obj().Name(), distinct from the
// generic-instance path which uses monoName(inst)).
func TestT0636_NonGenericEnumGenericMethodMono(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Tagged {
			Empty,
			Data(Vector[int] xs),
			pick[U](U _sel) int {
				match this {
					Empty => { return -1; },
					Data(xs) => { return xs.len; },
				}
			}
		}
		main() {
			t := Tagged.Data([1, 2, 3, 4]);
			a := t.pick[string]("x");
			b := Tagged.Empty.pick[bool](false);
		}
	`)
	// Bare enum name (no "Tagged[..]" owner suffix) + per-method-type-arg mono.
	codegentest.AssertContains(t, ir, `"Tagged.pick[string]"`)
	codegentest.AssertContains(t, ir, `"Tagged.pick[bool]"`)
	codegentest.AssertContains(t, ir, `call i64 @"Tagged.pick[string]"`)
}

// TestT0639_GenericMethodViaThis verifies Defect B: a generic (method-type-
// param) method invoked via `this` inside the owner's own method body is
// emitted under the per-instance mono name (e.g. "NBox[int].inner[int]"),
// matching the monoCtx-built call site — NOT the bare-owner name
// ("NBox.inner[int]") which would be a different (mis-substituted) instance.
// Symmetric across a generic Named type and a generic enum.
func TestT0639_GenericMethodViaThis(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type NBox[T] {
			Vector[T] d;
			inner[U](U _x) int { return this.d.len; }
			outer(this) int { return this.inner[int](7); }
		}
		enum EBox[T] {
			V(Vector[T] d),
			N,
			inner[U](U _x) int {
				match this {
					V(d) => { return d.len; },
					N => { return 0; },
				}
			}
			outer(this) int { return this.inner[int](7); }
		}
		main() {
			n := NBox[int](d: [1, 2, 3]);
			x := n.outer();
			e := EBox[int].V([1, 2]);
			y := e.outer();
		}
	`)
	codegentest.AssertContains(t, ir, `define i64 @"NBox[int].inner[int]"`)
	codegentest.AssertContains(t, ir, `define i64 @"EBox[int].inner[int]"`)
	codegentest.AssertContains(t, ir, `call i64 @"NBox[int].inner[int]"`)
	codegentest.AssertContains(t, ir, `call i64 @"EBox[int].inner[int]"`)
	// The bare-owner name must never appear (would be the wrong instance).
	codegentest.AssertNotContains(t, ir, `@"NBox.inner[int]"`)
	codegentest.AssertNotContains(t, ir, `@"EBox.inner[int]"`)
}

// TestT0639_GenericFnRefParamGenericInstance verifies Defect A (call-site mono
// name unwraps `~`/`&` to the instance, not the bare owner) and Defect C (the
// generic free-function call passes a `~` param by pointer, matching the
// monomorphic callee's pointer ABI — no by-value/pointer mismatch segfault).
func TestT0639_GenericFnRefParamGenericInstance(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type NBox[T] {
			Vector[T] d;
			transform[U](U _x) int { return this.d.len; }
		}
		proc_named[X](NBox[X]~ b) int { return b.transform[int](5); }
		main() {
			x := proc_named[int](NBox[int](d: [1, 2, 3]));
		}
	`)
	// Defect A: receiver mangles to the instance, not the bare owner.
	codegentest.AssertContains(t, ir, `define i64 @"NBox[int].transform[int]"`)
	codegentest.AssertContains(t, ir, `call i64 @"NBox[int].transform[int]"`)
	codegentest.AssertNotContains(t, ir, `@"NBox.transform[int]"`)
	// Defect C: definition and call site agree on the pointer ABI for the `~`
	// generic-instance param. The pre-fix bug passed it by value
	// (`...({ i8*, i8* } %`), dereferenced as a pointer => segfault.
	codegentest.AssertContains(t, ir, `define i64 @"proc_named[int]"({ i8*, i8* }* %`)
	codegentest.AssertContains(t, ir, `call i64 @"proc_named[int]"({ i8*, i8* }* %`)
	codegentest.AssertNotContains(t, ir, `call i64 @"proc_named[int]"({ i8*, i8* } %`)
}

// TestT0639_GenericFnRefParamStringVector verifies Defect C for the broader
// param class the bug also covered: a generic function with `~` string and
// `~` Vector params must pass them by pointer (matching the monomorphic
// callee's `i8**` ABI), not by value.
func TestT0639_GenericFnRefParamStringVector(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		take_str[T](string~ s, T _x) int { return s.len; }
		take_vec[T](Vector[T]~ v) int { return v.len; }
		main() {
			a := take_str[int]("hello", 1);
			b := take_vec[int]([1, 2, 3, 4]);
		}
	`)
	codegentest.AssertContains(t, ir, `define i64 @"take_str[int]"(i8** %`)
	codegentest.AssertContains(t, ir, `call i64 @"take_str[int]"(i8** %`)
	codegentest.AssertContains(t, ir, `define i64 @"take_vec[int]"(i8** %`)
	codegentest.AssertContains(t, ir, `call i64 @"take_vec[int]"(i8** %`)
}

// TestT0639_RefWrappedGenericInstanceOperatorGetter verifies Defect A reaches
// beyond plain method calls: `resolveTypeName` is the shared mangling helper
// for ~20 dispatch sites. A `[]`-index and a parameterless-getter call on a
// `~`/`&` generic-instance receiver must mangle to the instance name
// ("GBox[int].[]" / "GBox[int].total"), NOT the bare generic owner
// ("GBox.[]" / "GBox.total") which pre-fix panicked "undeclared method".
func TestT0639_RefWrappedGenericInstanceOperatorGetter(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type GBox[T] {
			Vector[T] d;
			get total int { return this.d.len; }
			[](int i) T { return this.d[i]; }
		}
		idx_mut(GBox[int] ~b) int { return b[1]; }
		get_shared(GBox[int] b) int { return b.total; }
		main() {
			x := idx_mut(GBox[int](d: [10, 20, 30]));
			y := get_shared(GBox[int](d: [1, 2, 3, 4]));
		}
	`)
	// `[]` operator dispatch on a `~` generic-instance receiver.
	codegentest.AssertContains(t, ir, `define i64 @"GBox[int].[]"`)
	codegentest.AssertContains(t, ir, `call i64 @"GBox[int].[]"`)
	// Getter dispatch on a `&` generic-instance receiver.
	codegentest.AssertContains(t, ir, `define i64 @"GBox[int].total"`)
	codegentest.AssertContains(t, ir, `call i64 @"GBox[int].total"`)
	// The bare generic-owner name must never appear for either caller.
	codegentest.AssertNotContains(t, ir, `@"GBox.[]"`)
	codegentest.AssertNotContains(t, ir, `@"GBox.total"`)
}

// TestT0642_InferredGenericMethodOnNonGenericNamed verifies that calling a
// generic (method-type-param) method on a non-generic Named type WITHOUT
// explicit type-arg brackets dispatches to the per-method-type-arg mono name
// inferred from the call argument. Pre-fix this routed through `genMethodCall`,
// which built the bare mangled name ("Plain.echo") and panicked.
func TestT0642_InferredGenericMethodOnNonGenericNamed(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Plain { int x; echo[U](U v) U { return v; } }
		main() {
			p := Plain(x: 1);
			r := p.echo("hi");
		}
	`)
	// Inferred U=string mangles to the same name as the explicit form.
	codegentest.AssertContains(t, ir, `"Plain.echo[string]"`)
	codegentest.AssertContains(t, ir, `call i8* @"Plain.echo[string]"`)
	// Bare-name form (pre-fix mis-dispatch target) must never appear.
	codegentest.AssertNotContains(t, ir, `@"Plain.echo"(`)
}

// TestT0642_InferredGenericMethodOnNonGenericEnum exercises the
// `case *types.Enum` arm of genGenericEnumMethodCall via the inferred path.
// Pre-fix the inferred call silently dispatched through the bare-name enum
// path (single overload) which ABI-mismatched on non-`i8*` args.
func TestT0642_InferredGenericMethodOnNonGenericEnum(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum EPlain { A, B, echo[U](U v) U { return v; } }
		main() {
			p := EPlain.A;
			r := p.echo("hi");
		}
	`)
	codegentest.AssertContains(t, ir, `"EPlain.echo[string]"`)
	codegentest.AssertContains(t, ir, `call i8* @"EPlain.echo[string]"`)
}

// TestT0642_InferredGenericMethodOnGenericNamedInstance verifies the
// generic-Named-instance owner case routes through the per-instance mono
// name ("NBox[int].echo[string]"), with U inferred from the call arg.
func TestT0642_InferredGenericMethodOnGenericNamedInstance(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type NBox[T] { T val; echo[U](U v) U { return v; } }
		main() {
			b := NBox[int](val: 42);
			r := b.echo("hi");
		}
	`)
	codegentest.AssertContains(t, ir, `"NBox[int].echo[string]"`)
	codegentest.AssertContains(t, ir, `call i8* @"NBox[int].echo[string]"`)
	// Pre-fix would have mis-dispatched to the bare-owner name.
	codegentest.AssertNotContains(t, ir, `@"NBox.echo[string]"`)
}

// TestT0642_InferredGenericMethodOnGenericEnumInstance verifies the
// generic-enum-instance owner case routes through monoName(instance)
// + per-method-type-arg suffix on the inferred path (the `*types.Instance`
// arm of genGenericEnumMethodCall).
func TestT0642_InferredGenericMethodOnGenericEnumInstance(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum EBox[T] { V(Vector[T] xs), N, echo[U](U v) U { return v; } }
		main() {
			b := EBox[int].V([1, 2, 3]);
			r := b.echo("hi");
		}
	`)
	codegentest.AssertContains(t, ir, `"EBox[int].echo[string]"`)
	codegentest.AssertContains(t, ir, `call i8* @"EBox[int].echo[string]"`)
	codegentest.AssertNotContains(t, ir, `@"EBox.echo[string]"`)
}

func TestGenericConstructorZeroInit(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 0);
		}
	`)
	// Generic type instance for Box[int]
	codegentest.AssertContains(t, ir, "Box[int]_i")
}

// TestGenericTupleTypeArg verifies that a Tuple used as a generic type argument
// produces a correct mono name ("Wrapper[(int, string)]") instead of "Wrapper[unknown]".
// Two different tuple args for the same generic must not collide.
func TestGenericTupleTypeArg(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper[T] { T val; }
		main() {
			w1 := Wrapper[(int, string)](val: (1, "a"));
			w2 := Wrapper[(bool, int)](val: (true, 42));
		}
	`)
	codegentest.AssertContains(t, ir, `Wrapper[(int, string)]`)
	codegentest.AssertContains(t, ir, `Wrapper[(bool, int)]`)
	codegentest.AssertNotContains(t, ir, `Wrapper[unknown]`)
}

// --- Generic function tests ---

func TestGenericFunc(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			int r = identity[int](42);
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @\"identity[int]\"")
	codegentest.AssertContains(t, ir, "ret i64")
}

func TestGenericFuncString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			string s = identity[string]("hello");
		}
	`)
	codegentest.AssertContains(t, ir, "define i8* @\"identity[string]\"")
}

func TestGenericFuncMultipleInstances(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			int a = identity[int](42);
			string b = identity[string]("hi");
		}
	`)
	codegentest.AssertContains(t, ir, "@\"identity[int]\"")
	codegentest.AssertContains(t, ir, "@\"identity[string]\"")
}

func TestGenericMethodMutReceiverAssign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			T value;
			replace(~this, T newVal) { this.value = newVal; }
		}
		main() {
			b := Box[int](value: 10);
			b.replace(99);
		}
	`)
	codegentest.AssertContains(t, ir, "define void @\"Box[int].replace\"")
	// Should store i64 (the new value into the field)
	codegentest.AssertContains(t, ir, "store i64")
}

func TestGenericFuncVoid(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		consume[T](T x) { }
		main() {
			consume[int](42);
		}
	`)
	codegentest.AssertContains(t, ir, "define void @\"consume[int]\"")
}

// T0340: a generic function with a `~T` parameter must clear the caller's
// drop flag at the call site. Without the applyMutRefArgOwnership fix,
// `~` args on generic calls left the caller's drop flag set → double-free
// when the callee consumed the value (e.g. moved it into a struct field).
func TestT0340_GenericFuncMutRefArgClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		consume[T](T move x) { }
		main() {
			s := "hello";
			consume[string](s);
		}
	`)
	// Drop flag store + immediate call to the monomorphized consume.
	codegentest.AssertContainsMatch(t, ir,
		`store i1 false, i1\* %s\.dropflag\s*\n\s*call void @"consume\[string\]"`)
}

// T0340: same fix must apply when the type parameter is inferred (no
// explicit `[T]` at the call site). Exercises genInferredGenericCall.
func TestT0340_InferredGenericFuncMutRefArgClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		consume[T](T move x) { }
		main() {
			s := "hello";
			consume(s);
		}
	`)
	codegentest.AssertContainsMatch(t, ir,
		`store i1 false, i1\* %s\.dropflag\s*\n\s*call void @"consume\[string\]"`)
}

// B0099: Generic function calling another generic function with its own type param.
func TestGenericFuncCallsGenericFunc(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		identity[T](T val) T { return val; }
		wrap[T](T val) T { return identity[T](val); }
		main() {
			int r = wrap[int](42);
		}
	`)
	// Both wrap[int] and identity[int] must be monomorphized
	codegentest.AssertContains(t, ir, "define i64 @\"wrap[int]\"")
	codegentest.AssertContains(t, ir, "define i64 @\"identity[int]\"")
}

// B0099: Transitive chain of generic functions calling generic functions.
func TestGenericFuncTransitiveChain(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		inner[T](T val) T { return val; }
		middle[T](T val) T { return inner[T](val); }
		outer[T](T val) T { return middle[T](val); }
		main() {
			int r = outer[int](42);
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @\"outer[int]\"")
	codegentest.AssertContains(t, ir, "define i64 @\"middle[int]\"")
	codegentest.AssertContains(t, ir, "define i64 @\"inner[int]\"")
}

// B0099: Multiple instantiations through generic-calls-generic.
func TestGenericFuncCallsGenericMultipleInstances(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		identity[T](T val) T { return val; }
		wrap[T](T val) T { return identity[T](val); }
		main() {
			int a = wrap[int](42);
			string b = wrap[string]("hi");
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @\"wrap[int]\"")
	codegentest.AssertContains(t, ir, "define i64 @\"identity[int]\"")
	codegentest.AssertContains(t, ir, "define i8* @\"wrap[string]\"")
	codegentest.AssertContains(t, ir, "define i8* @\"identity[string]\"")
}

// B0099: Generic function calling a generic method (cross-resolution).
func TestGenericFuncCallsGenericMethod(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Echo {
			echo[T](T val) T { return val; }
		}
		invoke[T](Echo e, T val) T {
			return e.echo[T](val);
		}
		main() {
			e := Echo();
			int r = invoke[int](e, 42);
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @\"invoke[int]\"")
	codegentest.AssertContains(t, ir, "define i64 @\"Echo.echo[int]\"")
}

// B0099: MethodInstance self-resolution (generic method calls generic method).
func TestGenericMethodCallsGenericMethod(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo { echo[T](T val) T { return val; } }
		type Bar { delegate[T](Foo f, T val) T { return f.echo[T](val); } }
		main() {
			f := Foo();
			b := Bar();
			int r = b.delegate[int](f, 7);
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @\"Bar.delegate[int]\"")
	codegentest.AssertContains(t, ir, "define i64 @\"Foo.echo[int]\"")
}

// B0099: Type-instance resolution (generic type method calls generic free function).
func TestGenericTypeMethodCallsFreeFunc(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		identity[T](T val) T { return val; }
		type Wrapper[T] {
			T value;
			wrapped(this) T { return identity[T](this.value); }
		}
		main() {
			w := Wrapper[int](value: 77);
			int r = w.wrapped();
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @\"identity[int]\"")
	codegentest.AssertContains(t, ir, "define i64 @\"Wrapper[int].wrapped\"")
}

// B0099: Cross-resolution reverse (generic method calls generic free function).
func TestGenericMethodCallsFreeFunc(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		helper[T](T val) T { return val; }
		type Proxy {
			forward[T](T val) T { return helper[T](val); }
		}
		main() {
			p := Proxy();
			int r = p.forward[int](33);
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @\"Proxy.forward[int]\"")
	codegentest.AssertContains(t, ir, "define i64 @\"helper[int]\"")
}

// B0099: Type-instance resolution for MethodInstance (generic type method calls generic method).
func TestGenericTypeMethodCallsGenericMethod(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Echoer { echo[T](T val) T { return val; } }
		type Wrapper[T] {
			T value;
			Echoer e;
			echoed(this) T { return this.e.echo[T](this.value); }
		}
		main() {
			w := Wrapper[int](value: 55, e: Echoer());
			int r = w.echoed();
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @\"Wrapper[int].echoed\"")
	codegentest.AssertContains(t, ir, "define i64 @\"Echoer.echo[int]\"")
}

// B0099: Cross-resolution both directions (method calls both func and method).
func TestGenericMethodCallsBothFuncAndMethod(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		helper[T](T val) T { return val; }
		type Echoer { echo[T](T val) T { return val; } }
		type Combiner {
			run[T](Echoer e, T val) T {
				T a = helper[T](val);
				return e.echo[T](a);
			}
		}
		main() {
			e := Echoer();
			c := Combiner();
			int r = c.run[int](e, 42);
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @\"Combiner.run[int]\"")
	codegentest.AssertContains(t, ir, "define i64 @\"helper[int]\"")
	codegentest.AssertContains(t, ir, "define i64 @\"Echoer.echo[int]\"")
}

func TestGenericTypeAsParam(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T value; }
		unbox(Box[int] b) int {
			return b.value;
		}
		main() {
			b := Box[int](value: 99);
			int v = unbox(b);
		}
	`)
	codegentest.AssertContains(t, ir, "define i64 @__user.unbox")
	codegentest.AssertContains(t, ir, "load i64")
}

func TestGenericEnumMatchBlock(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].Some(42);
			match x {
				Some(v) => { int y = v; },
				_ => { },
			};
		}
	`)
	codegentest.AssertContains(t, ir, "switch i32")
}

// T0937: a value-struct-container elvis inside a generic function with an owned
// (`~`) optional param is orphaned on the some-path, so the result is tracked.
// The result type (`map[K,V]`) is resolved through c.typeSubst during
// monomorphization, so the synthesized drop dispatches on the concrete
// instantiation (`Map[string, int].drop`). Exercises the typeSubst branch of
// trackElvisResultHeap (uncovered by the non-generic ident-source tests).
func TestElvisMapGenericOwnedParamDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		gconsume[K: Hashable + Equal, V](map[K, V]? move a, map[K, V] b) int {
			return (a ?: b).len;
		}
		main() {
			map[string, int]? a = {"x": 1};
			map[string, int] b = {"z": 9};
			c := gconsume(a, b);
		}
	`)
	codegentest.AssertContains(t, ir, "elvis.merge")
	codegentest.AssertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	codegentest.AssertContains(t, ir, `call void @"Map[string, int].drop"`)
}

// T0937 (i8*-container gap): a generic vector elvis with an owned (`~`) optional
// param is orphaned on the some-path → tracked via trackElvisResultTemp. The
// result type (`T[]`) resolves through c.typeSubst at monomorphization.
// Exercises the typeSubst branch of trackElvisResultTemp (the existing generic
// tests use borrowed params, which now short-circuit at the orphan gate).
func TestElvisStrvecGenericOwnedParamDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		gconsume[T](T[]? move a, T[] b) int {
			return (a ?: b).len;
		}
		main() {
			string[]? a = ["x" + "y"];
			string[] b = ["z" + "w"];
			c := gconsume(a, b);
		}
	`)
	codegentest.AssertContains(t, ir, "elvis.merge")
	codegentest.AssertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
}

// T1160: an explicit type-argument list parses the callee as an IndexExpr
// (`make_generic[int]` → `IndexExpr{make_generic, int}`), which looks like a value
// subscript yielding a callable. The free function must still be recognized as a
// statically-known callee (its result is tracked), while the generic method's
// receiver must still reach the alias check (its result is NOT tracked).
func TestClosureGenericCallResultTracking(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder { () -> int cb; get_cb[T](this, T v) () -> int { return this.cb; } }
		make_generic[T](T v, int x) () -> int { return || -> x + 1; }
		fresh() { make_generic[int](1, 10); }
		aliased(Holder h) { h.get_cb[int](0); }
		main() { fresh(); }
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.fresh"), "env.tmp.drop")
	aliased := codegentest.ExtractFunction(ir, "__user.aliased")
	codegentest.AssertContains(t, aliased, "define") // guard: the body was actually found
	codegentest.AssertNotContains(t, aliased, "env.tmp.drop")
}

// T1160: a receiver that is a generic INSTANCE whose closure lives in a type
// argument (`CBox[() -> int]`), not in the origin's own fields. The walk must
// descend into TypeArgs before concluding the receiver holds no closure. Runtime
// coverage is blocked by T1232 (such an instance never drops the env), so this
// pins the arm at IR level.
func TestClosureGenericInstanceReceiverAliasNotTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type CBox[T] { T v; get_v(this) T { return this.v; } }
		via_generic_instance(CBox[() -> int] b) { b.get_v(); }
		main() { }
	`)
	body := codegentest.ExtractFunction(ir, "__user.via_generic_instance")
	codegentest.AssertContains(t, body, "define") // guard: the body was actually found
	codegentest.AssertNotContains(t, body, "env.tmp.drop")
}

// T1160: a self-referential GENERIC type. TestClosureCallResultRecursiveTypeGuard's
// `Node` is non-generic, so its cycle trips the `seen` guard on the Named arm
// directly. Here the cycle runs through the Instance arm (`GNode[int]` → origin
// `GNode` → field `GNode[T]?` → Instance again), which adds nothing to `seen` itself
// and depends on the origin Named's guard to break the loop — without it the compiler
// hangs. Having terminated, the walk must still report "no closure" so the fresh
// result is tracked; the mirror type's `cb` must still be found past the cycle so the
// borrowed result is not.
func TestClosureRecursiveGenericReceiverGuard(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type GNode[T] {
			T v;
			GNode[T]? next;
			mk(this, int x) () -> int { return move || -> x + 1; }
		}
		type GCbNode[T] {
			T v;
			GCbNode[T]? next;
			() -> int cb;
			get_cb(this) () -> int { return this.cb; }
		}
		fresh(GNode[int] n) { n.mk(1); }
		aliased(GCbNode[int] h) { h.get_cb(); }
		main() {}
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.fresh"), "env.tmp.drop")
	aliased := codegentest.ExtractFunction(ir, "__user.aliased")
	codegentest.AssertContains(t, aliased, "define") // guard: the body was actually found
	codegentest.AssertNotContains(t, aliased, "env.tmp.drop")
}

func TestVoidFunctionTypeParam(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		apply((int) -> void fn) {
			fn(42);
		}
		main() {
			apply(|int x| { });
		}
	`)
	codegentest.AssertContains(t, ir, "define void @__user.apply")
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestStringInterpolationTypeParam(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper[T] {
			T val;
			to_string() string => "[{this.val}]";
		}
		main() {
			Wrapper[int] w = Wrapper[int](val: 42);
			string s = w.to_string();
		}
	`)
	// The mono'd Wrapper[int].to_string should call promise_int_to_string
	codegentest.AssertContains(t, ir, "promise_int_to_string")
}

// T1297: an optional-element array/vector literal built inside a generic
// function Some-wraps each element after substituting the element expression's
// TypeParam through c.typeSubst (the monomorphization path — argExprType `T` is
// substituted to `int` before the Identical(argExprType, elem) wrap decision).
func TestT1297_GenericOptionalElementArrayWrap(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		wrap_vec[T](T move a, T move b) T?[] { return [a, b]; }
		wrap_fixed[T](T move a, T move b) T?[2] { return [a, b]; }
		main() {
			int?[] v = wrap_vec[int](1, 2);
			int?[2] a = wrap_fixed[int](3, 4);
		}
	`)
	// Both the vector-literal and fixed-array-literal monomorphizations wrap
	// their substituted-int elements into the { i1, i64 } optional slot.
	codegentest.AssertContains(t, ir, "insertvalue { i1, i64 }")
	codegentest.AssertContains(t, ir, "wrap_vec[int]")
	codegentest.AssertContains(t, ir, "wrap_fixed[int]")
}

func TestT1341_FixedArrayOptionalStructuralCloneGeneric(t *testing.T) {
	// T1341, monomorphization path: the array-index read happens inside a generic
	// body (`_read_first[T](T?[2]) T?`) where the element's inner type is a TypeParam.
	// The clone-on-read branch must substitute (c.typeSubst) the inner to the concrete
	// structural type before the isNonValueStructuralType gate — otherwise the mono'd
	// body aliases the array's box and double-frees it. The concrete test above never
	// sets c.typeSubst; this one exercises that substitution branch, emitting the
	// structural clone INSIDE the specialized `_read_first[Showable]` function.
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			tag() string `+"`"+`abstract;
		}
		type Widget {
			int id;
			tag() string { return "w"; }
		}
		_read_first[T](T?[2] move a) T? {
			return a[0];
		}
		main() {
			Showable?[2] a = [Widget(id: 1), Widget(id: 2)];
			Showable? g = _read_first[Showable](move a);
		}
	`)
	codegentest.AssertContains(t, ir, `define { i1, { i8*, i8* } } @"_read_first[Showable]"`)
	codegentest.AssertContains(t, ir, "call i8* @__promise_structural_clone")
}

func TestGenericGetterSetterSameName(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			T _val;
			get val T { return this._val; }
			set val(T v) { this._val = v; }
		}
		main() {
			b := Box[int](_val: 0);
			b.val = 42;
			int v = b.val;
		}
	`)
	// Monomorphized getter and setter should have distinct names
	codegentest.AssertContains(t, ir, "define i64 @\"Box[int].val\"(")
	codegentest.AssertContains(t, ir, "define void @\"Box[int].val$set\"(")
}

// Move in generic function call clears flag
func TestDropMoveToGenericFuncCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		identity[T](T val) T { return val; }
		main() {
			r := Resource(id: 1);
			Resource r2 = identity[Resource](r);
		}
	`)
	codegentest.AssertContains(t, ir, "store i1 false, i1*")
}

// B0235: Vector[GenericEnum] index assignment drops old enum element (mono).
func TestVectorMonoEnumIndexAssignDropsOld(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Slot[K, V] {
			Empty,
			Used(K key, V value),
		}
		type Container[K, V] {
			Slot[K, V][] buckets;
			overwrite(~this, int idx) {
				this.buckets[idx] = Slot.Empty;
			}
		}
		main() {
			Slot[string, string][] b = [];
			Container[string, string] c = Container[string, string](buckets: b);
			c.overwrite(0);
		}
	`)
	// Should drop old mono enum element before storing the new one.
	codegentest.AssertContains(t, ir, `call void @"Slot[string, string].drop"(`)
}

// B0192: Generic type with type-param field gets NeedsSynthDrop
// so its synthesized drop can free heap-allocated type-param fields.
func TestSynthDropGenericTypeParamField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; }
		type Holder { Point p; }
		main() {
			Holder h = Holder(p: Point(x: 1, y: 2));
		}
	`)
	// Holder gets a synthesized drop that frees the Point instance
	holderDrop := codegentest.ExtractFunction(ir, "Holder.drop")
	if holderDrop == "" {
		t.Fatal("expected Holder.drop function in IR")
	}
	// Should pal_free the Point field instance (B0192 needsFreeOnly path)
	codegentest.AssertContains(t, holderDrop, "call void @pal_free(")
}

// B0202: Generic type where ALL fields are TypeParam — synthesized drop detected at mono time
func TestSynthDropMonoTypeParamOnlyFields(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; }
		type Box[T] { T val; }
		main() {
			b := Box[Point](val: Point(x: 1, y: 2));
		}
	`)
	// Box[Point] gets a mono synthesized drop that frees the Point field
	boxDrop := codegentest.ExtractFunction(ir, `"Box[Point].drop"`)
	if boxDrop == "" {
		t.Fatal("expected Box[Point].drop function in IR")
	}
	codegentest.AssertContains(t, boxDrop, "call void @pal_free(")
}

// B0202: Generic type with TypeParam field instantiated with primitive — no synth drop needed
func TestSynthDropMonoTypeParamPrimitive(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T val; }
		main() {
			b := Box[int](val: 42);
		}
	`)
	// Box[int] should NOT get a synthesized drop — int is primitive
	boxDrop := codegentest.ExtractFunction(ir, `"Box[int].drop"`)
	if boxDrop != "" {
		t.Fatal("Box[int] should not have a synthesized drop")
	}
}

// B0202: Generic type with TypeParam field instantiated with string — gets synth drop
func TestSynthDropMonoTypeParamString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper[T] { T val; }
		main() {
			w := Wrapper[string](val: "hello");
		}
	`)
	// Wrapper[string] gets a mono synthesized drop for the string field
	wrapperDrop := codegentest.ExtractFunction(ir, `"Wrapper[string].drop"`)
	if wrapperDrop == "" {
		t.Fatal("expected Wrapper[string].drop function in IR")
	}
	codegentest.AssertContains(t, wrapperDrop, "call void @promise_string_drop(")
}

// B0209: Generic type with Optional[TypeParam] field instantiated with string — gets synth drop
func TestSynthDropMonoOptionalTypeParamString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[string](val: "hello");
		}
	`)
	// MaybeVal[string] gets a mono synthesized drop for the optional string field
	drop := codegentest.ExtractFunction(ir, `"MaybeVal[string].drop"`)
	if drop == "" {
		t.Fatal("expected MaybeVal[string].drop function in IR")
	}
	codegentest.AssertContains(t, drop, "call void @promise_string_drop(")
}

// B0209: Generic type with Optional[TypeParam] field instantiated with primitive — no synth drop
func TestSynthDropMonoOptionalTypeParamPrimitive(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[int](val: 42);
		}
	`)
	// MaybeVal[int] should NOT get a synthesized drop — int is primitive
	drop := codegentest.ExtractFunction(ir, `"MaybeVal[int].drop"`)
	if drop != "" {
		t.Fatal("MaybeVal[int] should not have a synthesized drop")
	}
}

// B0209: Generic type with Optional[TypeParam] field instantiated with heap user type — gets synth drop
func TestSynthDropMonoOptionalTypeParamUserType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; }
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[Point](val: Point(x: 1, y: 2));
		}
	`)
	// MaybeVal[Point] gets a mono synthesized drop (at minimum for pal_free of the instance)
	drop := codegentest.ExtractFunction(ir, `"MaybeVal[Point].drop"`)
	if drop == "" {
		t.Fatal("expected MaybeVal[Point].drop function in IR")
	}
	codegentest.AssertContains(t, drop, "call void @pal_free(")
}

// T0551/T0607: Cloning a generic `clone enum whose TypeArg is droppable
// (map[K,V]) must deep-copy the variant payload. isCopyField(TypeParam) is
// optimistically true, so the synth body can't classify the `T val` field at
// synth time; it emits the synth-only AutoCloneExpr intrinsic (T0607 unified
// the T0551 plain-T path onto the same mechanism), lowered type-directed at
// mono codegen to the substituted concrete type's clone. Before the fix the
// synth body shallow-aliased the Map fat pointer (no clone call) → double-free
// segfault. Assert the mono clone body contains a Map[..].clone call inside
// the Just arm (AutoClone → cloneByType → cloneResolvedValue → Map.clone).
func TestGenericEnumCloneDroppableTypeArg(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum MaybeMap[T] `clone {\n"+
		"  Just(T val),\n"+
		"  Nothing,\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  MaybeMap[map[string, string]] j = MaybeMap[map[string, string]].Just(src);\n"+
		"  MaybeMap[map[string, string]] c = j.clone();\n"+
		"}\n")
	// The mono enum clone body must deep-copy the Map payload.
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, "MaybeMap[Map[string, string]].clone") {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if inClone && strings.Contains(line, `@"Map[string, string].clone"`) {
			sawMapClone = true
		}
	}
	if !sawMapClone {
		t.Errorf("T0551: MaybeMap[Map[string, string]].clone() body must call Map[string, string].clone (deep-copy the droppable TypeArg payload); got shallow alias")
	}
}

// T0607: Cloning a generic `clone enum whose variant field is *declared* as
// `T?` (Optional[TypeParam]) with a droppable TypeArg (map[K,V]) must deep-copy
// the Optional payload. isCopyField(Optional[TypeParam])==true at synth time,
// so the synth body used to pass the field through bare → the constructor
// stored the inner Map fat pointer into the Optional slot (compile panic, then
// after T0608/T0630's coercion: shallow alias → double-free segfault). The fix
// routes the ContainsTypeParam field through the synth-only AutoCloneExpr
// intrinsic, lowered by cloneByType: an Optional none-check (autoclone.some /
// autoclone.merge blocks) that deep-clones the unwrapped concrete payload
// (Map[..].clone) and rewraps. Assert both the none-check structure and the
// inner Map clone are present in the OptVal arm (not a bare {i1,payload}
// passthrough).
func TestGenericEnumCloneOptionalTypeParamField(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum Wrap[T] `clone {\n"+
		"  OptVal(T? maybe),\n"+
		"  Nothing,\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  Wrap[map[string, string]] j = Wrap[map[string, string]].OptVal(src);\n"+
		"  Wrap[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	sawAutoCloneSome := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `Wrap[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		if strings.Contains(line, `@"Map[string, string].clone"`) {
			sawMapClone = true
		}
		if strings.Contains(line, "autoclone.some") {
			sawAutoCloneSome = true
		}
	}
	if !sawMapClone {
		t.Errorf("T0607: Wrap[Map[string, string]].clone() body must call Map[string, string].clone to deep-copy the Optional[TypeParam] payload; got shallow alias")
	}
	if !sawAutoCloneSome {
		t.Errorf("T0607: Wrap[Map[string, string]].clone() body must lower the `T? maybe` field via AutoClone (autoclone.some none-check block), not a bare {i1,payload} passthrough")
	}
}

// T0607: Cloning a generic `clone enum whose variant field is an enum-Instance
// carrying the TypeParam (`Inner[T] inner`) with a droppable TypeArg must deep-
// copy the nested enum. extractNamed is nil for enum Instances, so before the
// fix isAutoCloneBitCopy treated `Inner[Map]` as a bit copy (non-named →
// bitwise) → cloneByType returned the value unchanged → shallow alias →
// double-free. The fix adds an extractEnum branch to isAutoCloneBitCopy so the
// nested `clone enum routes through cloneResolvedValue→cloneEnumValue. Assert
// Outer's clone body calls the inner enum's clone (which itself deep-copies
// the Map), not a bare aggregate copy.
func TestGenericEnumCloneNestedEnumTypeParamField(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum Inner[T] `clone {\n"+
		"  Has(T v),\n"+
		"  Not,\n"+
		"}\n"+
		"enum Outer[T] `clone {\n"+
		"  Box(Inner[T] inner),\n"+
		"  Bare,\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  Outer[map[string, string]] j = Outer[map[string, string]].Box(Inner[map[string, string]].Has(src));\n"+
		"  Outer[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawInnerClone := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `Outer[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if inClone && strings.Contains(line, `@"Inner[Map[string, string]].clone"`) {
			sawInnerClone = true
		}
	}
	if !sawInnerClone {
		t.Errorf("T0607: Outer[Map[string, string]].clone() body must call Inner[Map[string, string]].clone to deep-copy the nested enum-Instance field; got shallow bit-copy alias (isAutoCloneBitCopy enum gap)")
	}
}

// T0674 (item 1): the nested generic `clone enum `Wrap(Inner[T])` shape must call
// the inner enum's clone EXACTLY ONCE — not twice. An earlier inspection (T0551)
// worried that lifting B0285 match-dup suppression for TypeParam fields would, for
// a variant field declared as a non-bare TypeParam-containing type the synth treats
// as non-copy (Inner[T] → clone.go emits an explicit .clone()), cause BOTH the
// lifted suppression AND the synth's .clone() to fire → a redundant double deep-
// clone. T0607 superseded that: it removed the per-field un-suppression entirely
// (uniform c.suppressMatchDup inside enum clone bodies) and routes every
// TypeParam-containing field through the synth-only AutoCloneExpr intrinsic. So a
// TypeParam-containing variant field is cloned through exactly one mechanism, never
// two. This pins single-clone and guards against any future change that re-broadens
// match-dup suppression back into a double-clone.
//
// IMPORTANT: do NOT narrow clone.go's `ContainsTypeParam(fieldType)` gate (the one
// that diverts TypeParam-containing fields to AutoCloneExpr) to mirror `isCopyField`
// instead — `isCopyField(TypeParam)==true` optimistically, so a bare `T` field would
// regress onto the shallow-copy path and reintroduce the T0607/T0605 double-free for
// droppable TypeArgs (e.g. map). The ContainsTypeParam predicate is intentional.
func TestGenericEnumCloneNestedSingleCloneCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum Inner[T] `clone {\n"+
		"  Has(T v),\n"+
		"  Not,\n"+
		"}\n"+
		"enum Outer[T] `clone {\n"+
		"  Wrap(Inner[T] inner),\n"+
		"  Bare,\n"+
		"}\n"+
		"test() {\n"+
		"  string[] src = [\"a\"];\n"+
		"  Outer[string[]] j = Outer[string[]].Wrap(Inner[string[]].Has(src));\n"+
		"  Outer[string[]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	innerCloneCalls := 0
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `Outer[Vector[string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if inClone && strings.Contains(line, "call ") && strings.Contains(line, `@"Inner[Vector[string]].clone"`) {
			innerCloneCalls++
		}
	}
	if innerCloneCalls != 1 {
		t.Errorf("T0674: Outer[Vector[string]].clone() body must call Inner[Vector[string]].clone EXACTLY ONCE (single deep-clone of the Wrap(Inner[T]) field); got %d calls (a double-clone is the efficiency regression T0607 eliminated)", innerCloneCalls)
	}
}

// T0607: a multi-TypeParam `clone enum with a variant carrying BOTH params
// (each a distinct droppable substitution) must deep-clone each independently.
// The synth emits AutoCloneExpr per ContainsTypeParam field; buildMethodInstance
// subst must resolve K and V separately so each AutoClone lowers to the correct
// concrete clone (K=Vector[string] → Vector clone w/ string element loop;
// V=Map[string,int] → Map.clone). Pins the two-param substitution path that the
// single-TypeParam Wrap[T] tests don't exercise.
func TestGenericEnumCloneMultiTypeParamFields(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum KV2[K, V] `clone {\n"+
		"  Pair(K key, V val),\n"+
		"  Empty,\n"+
		"}\n"+
		"test() {\n"+
		"  string[] k = [\"a\"];\n"+
		"  map[string, int] v = map[string, int]();\n"+
		"  KV2[string[], map[string, int]] j = KV2[string[], map[string, int]].Pair(k, v);\n"+
		"  KV2[string[], map[string, int]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawVecClone := false
	sawMapClone := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `KV2[Vector[string], Map[string, int]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		// K=Vector[string] deep clone: dupVector (vecdup.copy block) + the
		// per-element string-clone loop (vecdup_str.* blocks). A shallow alias
		// would emit neither. The element loop is the strongest signal.
		if strings.Contains(line, "vecdup_str.") || strings.Contains(line, "vecdup.copy") {
			sawVecClone = true
		}
		// V=Map[string,int] deep clone.
		if strings.Contains(line, `@"Map[string, int].clone"`) {
			sawMapClone = true
		}
	}
	if !sawVecClone {
		t.Errorf("T0607: KV2[Vector[string], Map[string, int]].clone() must deep-copy the `K key` field (Vector[string] clone / element loop); got shallow alias — multi-param subst resolved K wrong")
	}
	if !sawMapClone {
		t.Errorf("T0607: KV2[Vector[string], Map[string, int]].clone() must call Map[string, int].clone for the `V val` field; got shallow alias — multi-param subst resolved V wrong")
	}
}

// T0607/B0285 coexistence: a SINGLE variant carrying both a concrete non-copy
// field (string) and a TypeParam field (T) must clone each independently. The
// concrete `string label` takes the synth body's explicit .clone() path with
// B0285 match-dup suppression in effect → exactly 1 strdup block, not 2. The
// TypeParam `T payload` takes the synth-only AutoCloneExpr path (T0607) which
// lowers to a deep-copy of the substituted Map (→ a Map[..].clone call); B0285
// suppression uniformly stands inside the clone body (T0607 removed the T0551
// per-field un-suppression). Asserting both invariants in the same mono clone
// body pins the per-field handling that neither
// TestGenericEnumCloneDroppableTypeArg (pure TypeParam) nor
// TestEnumCloneNoDoubleClone (pure concrete, non-generic) checks jointly.
func TestEnumCloneMixedConcreteAndTypeParamField(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum Mixed[T] `clone {\n"+
		"  Both(string label, T payload),\n"+
		"  None,\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, int] m = map[string, int]();\n"+
		"  Mixed[map[string, int]] j = Mixed[map[string, int]].Both(\"tag\", m);\n"+
		"  Mixed[map[string, int]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	strdupBlocks := 0
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, "Mixed[Map[string, int]].clone") {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		if strings.Contains(line, `@"Map[string, int].clone"`) {
			sawMapClone = true
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "strdup.copy.") && strings.HasSuffix(trimmed, ":") {
			strdupBlocks++
		}
	}
	if !sawMapClone {
		t.Errorf("T0551: Mixed[Map[string, int]].clone() must call Map[string, int].clone for the TypeParam `payload` field (deep-copy); got shallow alias")
	}
	if strdupBlocks != 1 {
		t.Errorf("B0285: Mixed[Map[string, int]].clone() must clone the concrete `label` string exactly once (per-field suppression), got %d strdup blocks", strdupBlocks)
	}
}

// T0605: Cloning a generic `clone TYPE (not enum) whose TypeArg is droppable
// (map[K,V]) must deep-copy the field. The synth body treats the TypeParam
// field as copy (isCopyField(TypeParam)==true) so it emitted a bare shallow
// member read — the constructor then stored the un-dup'd Map fat pointer →
// both original and clone aliased the same heap value → double-free segfault.
// The fix emits a synth-only AutoCloneExpr for TypeParam-containing fields,
// lowered type-directed at mono codegen. Assert the mono clone body deep-
// copies the Map payload (a Map[..].clone call — or, as a fallback, a
// heapdup.copy block from dupHeapValue) rather than a bare shallow store.
// Parallel to TestGenericEnumCloneDroppableTypeArg (T0551, enum case).
func TestGenericTypeCloneDroppableTypeArg(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"type BoxT[T] `clone {\n"+
		"  T val;\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  BoxT[map[string, string]] j = BoxT[map[string, string]](val: src);\n"+
		"  BoxT[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawDeepCopy := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `BoxT[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		// Deep copy = an explicit Map clone call, or the dupHeapValue static
		// fallback (heapdup.copy block label).
		if strings.Contains(line, `@"Map[string, string].clone"`) {
			sawDeepCopy = true
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "heapdup.copy") && strings.HasSuffix(trimmed, ":") {
			sawDeepCopy = true
		}
	}
	if !sawDeepCopy {
		t.Errorf("T0605: BoxT[Map[string, string]].clone() body must deep-copy the TypeParam `val` field (Map[..].clone or dupHeapValue), got a bare shallow alias")
	}
}

// T1202: Cloning a generic `clone TYPE that INHERITS a bare-TypeParam field
// from a generic `clone parent (`Sub[T] is Base[T]`, `Base[T]` has `T val`)
// must synthesize a drop for the child instance. The gate
// monoInstNeedsSynthDrop built its subst only from the child's own TypeParams,
// so AllFields()'s inherited `val` (typed in Base's T) stayed unresolved →
// gate returned false → no Sub[..].drop emitted → the inherited heap field
// leaked. The fix merges parent substitutions (mergeParentSubst) in the gate.
// Assert both: the clone body deep-clones the inherited field, AND a
// Sub[CloneHeapy].drop function is emitted (guards the gate against silently
// dropping the synth-drop again).
func TestT1202InheritedGenericCloneFieldDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum CloneHeapy `clone {\n"+
		"  Word(string s),\n"+
		"  Empty,\n"+
		"}\n"+
		"type Base[T] `clone {\n"+
		"  T val;\n"+
		"}\n"+
		"type Sub[T] is Base[T] `clone {\n"+
		"}\n"+
		"test() {\n"+
		"  Sub[CloneHeapy] j = Sub[CloneHeapy](val: CloneHeapy.Word(\"abc\"));\n"+
		"  Sub[CloneHeapy] c = j.clone();\n"+
		"}\n")
	// The inherited field must be deep-cloned via CloneHeapy.clone.
	if !strings.Contains(ir, `@CloneHeapy.clone(`) {
		t.Errorf("T1202: Sub[CloneHeapy].clone() must deep-clone the inherited `val` field via CloneHeapy.clone; got shallow alias")
	}
	// A synth drop for the child instance must be emitted, else the inherited
	// heap field leaks (bare pal_free at scope exit).
	if !strings.Contains(ir, `define void @"Sub[CloneHeapy].drop"`) {
		t.Errorf("T1202: Sub[CloneHeapy].drop must be emitted so the inherited droppable `val` field is reclaimed; monoInstNeedsSynthDrop missed the parent-substituted field")
	}
}

// T0667: a generic `clone enum whose variant field is a TUPLE carrying the
// TypeParam (`(T, int) pr`) with a droppable TypeArg (map[K,V]) must deep-copy
// the tuple's heap member. types.ContainsTypeParam recurses into *types.Tuple
// so the synth already emits AutoCloneExpr, but before the fix
// isAutoCloneBitCopy classified every tuple as a bit-copy "scalar tuple" (non-
// named fallthrough) and cloneResolvedValue had no *types.Tuple arm → the
// inner Map fat pointer was shallow-aliased → double-free segfault. The fix
// adds a *types.Tuple recursion to isAutoCloneBitCopy + a per-element
// extract/cloneByType/insert arm to cloneResolvedValue. Assert the mono clone
// body calls Map[..].clone inside the tuple-field clone (per-element deep-
// clone), not a bare aggregate copy. Parallel to the T0662 array gap.
func TestGenericEnumCloneTupleTypeParamField(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum TupWrap[T] `clone {\n"+
		"  Pair((T, int) pr),\n"+
		"  Nope,\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  TupWrap[map[string, string]] j = TupWrap[map[string, string]].Pair((src, 7));\n"+
		"  TupWrap[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `TupWrap[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if inClone && strings.Contains(line, `@"Map[string, string].clone"`) {
			sawMapClone = true
		}
	}
	if !sawMapClone {
		t.Errorf("T0667: TupWrap[Map[string, string]].clone() body must call Map[string, string].clone to deep-copy the tuple `(T, int)` field's heap member; got shallow alias (isAutoCloneBitCopy tuple gap / no cloneResolvedValue *types.Tuple arm)")
	}
}

// T0667: the type-level sibling of TestGenericEnumCloneTupleTypeParamField — a
// generic `clone TYPE (not enum) whose field is a TUPLE carrying the TypeParam
// (`(T, int) pr`) with a droppable TypeArg must deep-copy the tuple's heap
// member. Same root cause and fix as the enum case (both lower through
// cloneByType→cloneResolvedValue). Assert the mono clone body calls
// Map[..].clone inside the tuple-field clone (per-element deep-clone), not a
// bare aggregate copy. Parallel to TestGenericTypeCloneDroppableTypeArg.
func TestGenericTypeCloneTupleTypeParamField(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"type TupBox[T] `clone {\n"+
		"  (T, int) pr;\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  TupBox[map[string, string]] j = TupBox[map[string, string]](pr: (src, 7));\n"+
		"  TupBox[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `TupBox[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if inClone && strings.Contains(line, `@"Map[string, string].clone"`) {
			sawMapClone = true
		}
	}
	if !sawMapClone {
		t.Errorf("T0667: TupBox[Map[string, string]].clone() body must call Map[string, string].clone to deep-copy the tuple `(T, int)` field's heap member; got shallow alias (isAutoCloneBitCopy tuple gap / no cloneResolvedValue *types.Tuple arm)")
	}
}

// T0667: a `(T?, int)` tuple field — the per-element cloneByType call must
// recurse from the *types.Tuple arm into the *types.Optional arm so the heap
// payload is deep-cloned behind a none-check (autoclone.some block + inner
// Map[..].clone), not bit-copied. Pins the tuple→optional→heap recursion at
// the IR level (the runtime e2e equivalent is blocked by the *separate*
// pre-existing field-destructure crash T0672, so the type-level e2e covers
// this shape via clone+double-drop without destructure). Mirrors
// TestGenericEnumCloneOptionalTypeParamField's two-signal assertion.
func TestGenericTypeCloneTupleOptionalTypeParamField(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"type TupBoxO[T] `clone {\n"+
		"  (T?, int) pr;\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  map[string, string]? mo = src;\n"+
		"  TupBoxO[map[string, string]] j = TupBoxO[map[string, string]](pr: (mo, 5));\n"+
		"  TupBoxO[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	sawAutoCloneSome := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `TupBoxO[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		if strings.Contains(line, `@"Map[string, string].clone"`) {
			sawMapClone = true
		}
		if strings.Contains(line, "autoclone.some") {
			sawAutoCloneSome = true
		}
	}
	if !sawMapClone {
		t.Errorf("T0667: TupBoxO[Map[string, string]].clone() must call Map[string, string].clone for the `(T?, int)` field's Optional payload (tuple→optional→heap recursion); got shallow alias")
	}
	if !sawAutoCloneSome {
		t.Errorf("T0667: TupBoxO[Map[string, string]].clone() must lower the Optional element of the `(T?, int)` tuple via the cloneByType Optional none-check (autoclone.some block); got a bare bit copy (tuple arm did not recurse into the Optional arm)")
	}
}

// T0667 (zero-regression guard): a PURE SCALAR tuple field (`(T, int)` with
// T=int → `(int, int)`) must stay a plain bit copy. This is the other arm of
// the isAutoCloneBitCopy tuple recursion: the loop finds every element
// bit-copy and falls through to `return true` (expr.go:7144) so cloneByType
// short-circuits without per-element deep-clone machinery. Without this guard
// the only coverage of the `return true` arm is the e2e runtime tests — a
// future change that wrongly routes scalar tuples through cloneResolvedValue
// (extra allocs / churn, or worse) would slip past Go-level tests. Asserts the
// mono clone body contains NO nested `.clone` call and NO `autoclone.` block
// (the two signatures of the deep path) — the inverse of the heap-member
// siblings above. Mirrors the bit-copy regression guards elsewhere (e.g. the
// copy/value TypeArg expectations).
func TestGenericTypeCloneScalarTupleStaysBitCopy(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"type TupBox[T] `clone {\n"+
		"  (T, int) pr;\n"+
		"}\n"+
		"test() {\n"+
		"  TupBox[int] j = TupBox[int](pr: (42, 7));\n"+
		"  TupBox[int] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawNestedClone := false
	sawAutoCloneBlock := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `TupBox[int].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		if strings.Contains(line, "call ") && strings.Contains(line, `.clone"`) {
			sawNestedClone = true
		}
		if strings.Contains(line, "autoclone.") {
			sawAutoCloneBlock = true
		}
	}
	if !inClone {
		t.Fatalf("T0667: TupBox[int].clone define not found in IR")
	}
	if sawNestedClone {
		t.Errorf("T0667: TupBox[int].clone() must NOT emit a nested `.clone` call for a pure scalar `(int, int)` tuple field — the scalar tuple must stay a bit copy (isAutoCloneBitCopy tuple loop → return true at expr.go:7144 → cloneByType short-circuit)")
	}
	if sawAutoCloneBlock {
		t.Errorf("T0667: TupBox[int].clone() must NOT emit an autoclone.* block for a pure scalar `(int, int)` tuple field — got deep-clone machinery where a bit copy is required (regression in the isAutoCloneBitCopy tuple `return true` arm)")
	}
}

// T0667 (index-correctness guard): a heap member at a NON-ZERO tuple index
// (`(int, T)` with T=map) must be deep-cloned and NewInsertValue'd back at
// that same index. The cloneResolvedValue loop carries the element index `i`
// through both NewExtractValue and NewInsertValue; the existing `(T, int)`
// siblings only ever insert at index 0 (heap element is first), so a
// regression that hard-codes index 0 in the re-insert (or drops the index)
// would pass them yet silently corrupt this shape. Asserts the mono clone
// body both calls Map[..].clone AND re-inserts via an `insertvalue ..., 1`
// (the cloned heap member written back at the non-zero index). The runtime
// counterpart is e2e test_clone_tupboxr_map_nonzero_index (mutation
// independence proves the deep copy is real, not just present).
func TestGenericTypeCloneTupleHeapMemberNonZeroIndex(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"type TupBoxR[T] `clone {\n"+
		"  (int, T) r;\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  TupBoxR[map[string, string]] j = TupBoxR[map[string, string]](r: (9, src));\n"+
		"  TupBoxR[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	sawInsertAtIndex1 := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `TupBoxR[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		if strings.Contains(line, `@"Map[string, string].clone"`) {
			sawMapClone = true
		}
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "insertvalue") && strings.HasSuffix(trimmed, ", 1") {
			sawInsertAtIndex1 = true
		}
	}
	if !inClone {
		t.Fatalf("T0667: TupBoxR[Map[string, string]].clone define not found in IR")
	}
	if !sawMapClone {
		t.Errorf("T0667: TupBoxR[Map[string, string]].clone() must call Map[string, string].clone for the heap member at tuple index 1 of `(int, T)`; got shallow alias")
	}
	if !sawInsertAtIndex1 {
		t.Errorf("T0667: TupBoxR[Map[string, string]].clone() must re-insert the cloned heap member at tuple index 1 (`insertvalue ..., 1`) — the cloneResolvedValue loop must carry the element index into NewInsertValue, not hard-code 0")
	}
}

// T0662: a fixed-array field `T[N]` (`*types.Array`) instantiated with a
// droppable element must be deep-cloned per element, not bit-copied. Before the
// fix isAutoCloneBitCopy classified every array as a bit copy (non-named
// fallthrough) and cloneResolvedValue had no *types.Array arm, so the synth
// AutoClone emitted a bare aggregate copy that aliased each element's heap
// pointer → double-free. Asserts the mono clone body emits a per-element clone
// loop: it calls Map[..].clone AND re-inserts the cloned element via
// `insertvalue` (the cloneResolvedValue array arm's extract/clone/insert).
// Runtime counterpart: e2e test_clone_arrbox_map / test_clone_arrbox_map_independence.
func TestGenericTypeCloneArrayHeapElement(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"type ArrBox[T] `clone {\n"+
		"  T[2] pair;\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, string] m1 = map[string, string]();\n"+
		"  map[string, string] m2 = map[string, string]();\n"+
		"  ArrBox[map[string, string]] j = ArrBox[map[string, string]](pair: [m1, m2]);\n"+
		"  ArrBox[map[string, string]] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawMapClone := false
	sawInsertValue := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `ArrBox[Map[string, string]].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		if strings.Contains(line, `@"Map[string, string].clone"`) {
			sawMapClone = true
		}
		if strings.Contains(strings.TrimSpace(line), "insertvalue") {
			sawInsertValue = true
		}
	}
	if !inClone {
		t.Fatalf("T0662: ArrBox[Map[string, string]].clone define not found in IR")
	}
	if !sawMapClone {
		t.Errorf("T0662: ArrBox[Map[string, string]].clone() must call Map[string, string].clone to deep-copy the fixed-array `T[N]` field's heap elements; got a bare aggregate bit copy (isAutoCloneBitCopy array gap / no cloneResolvedValue *types.Array arm)")
	}
	if !sawInsertValue {
		t.Errorf("T0662: ArrBox[Map[string, string]].clone() must re-insert each cloned element via `insertvalue` (the cloneResolvedValue array extract/clone/insert loop); got no per-element insert")
	}
}

// T0662 (zero-regression guard): a PURE SCALAR fixed-array field (`T[N]` with
// T=int → `int[2]`) must stay a plain bit copy — the isAutoCloneBitCopy array
// recursion returns true when the element is bit-copy, so cloneByType
// short-circuits without per-element clone machinery. Asserts the mono clone
// body contains NO nested `.clone` call and NO `autoclone.` block. Mirrors
// TestGenericTypeCloneScalarTupleStaysBitCopy for the array arm.
func TestGenericTypeCloneScalarArrayStaysBitCopy(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"type ArrBox[T] `clone {\n"+
		"  T[2] pair;\n"+
		"}\n"+
		"test() {\n"+
		"  ArrBox[int] j = ArrBox[int](pair: [42, 7]);\n"+
		"  ArrBox[int] c = j.clone();\n"+
		"}\n")
	lines := strings.Split(ir, "\n")
	inClone := false
	sawNestedClone := false
	sawAutoCloneBlock := false
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, `ArrBox[int].clone`) {
			inClone = true
			continue
		}
		if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		if !inClone {
			continue
		}
		if strings.Contains(line, "call ") && strings.Contains(line, `.clone"`) {
			sawNestedClone = true
		}
		if strings.Contains(line, "autoclone.") {
			sawAutoCloneBlock = true
		}
	}
	if !inClone {
		t.Fatalf("T0662: ArrBox[int].clone define not found in IR")
	}
	if sawNestedClone {
		t.Errorf("T0662: ArrBox[int].clone() must NOT emit a nested `.clone` call for a pure scalar `int[2]` array field — the scalar array must stay a bit copy (isAutoCloneBitCopy array recursion → true → cloneByType short-circuit)")
	}
	if sawAutoCloneBlock {
		t.Errorf("T0662: ArrBox[int].clone() must NOT emit an autoclone.* block for a pure scalar `int[2]` array field — got deep-clone machinery where a bit copy is required (regression in the isAutoCloneBitCopy array recursion)")
	}
}

// T0371: Generic function with a tuple-of-T local variable. Exercises the
// typeSubst != nil branch in emitTupleDropCall so the tuple type's elements
// are substituted at monomorphization time and the walk uses concrete types.
func TestT0371GenericFnTupleLocalSubstitutes(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_then_drop[T](T move x, T move y) {
			(T, T) t = (x, y);
		}
		main() {
			make_then_drop[string]("a" + "b", "c" + "d");
		}
	`)
	// The mono'd function must register a tuple-walk drop binding that calls
	// promise_string_drop on the (substituted) string fields.
	codegentest.AssertContains(t, ir, "tupdrop.exec")
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

// B0158: Generic type with droppable field gets a mono synthesized drop
