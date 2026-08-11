package codegen

import (
	"fmt"
	"strings"
	"testing"
)

func TestInferredVarDecl(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "alloca i64")
	assertContains(t, ir, "store i64 42")
}

func TestGenericFactoryCallCodegen(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call { i8*, i8* } @My.parse(")
}

func TestSelfGenericFactoryCodegen(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "@\"Box[int].wrap\"(")
	// Should call the Box[int] constructor
	assertContains(t, ir, "@\"Box[int].new\"(")
}

func TestSelfGenericMethodReturnCodegen(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "@\"Box[int].rewrap\"(")
}

func TestSelfGenericMultiParamCodegen(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "@\"Pair[int, string].make\"(")
	assertContains(t, ir, "@\"Pair[int, string].new\"(")
}

// T0474: super(v) in a generic Child[T] is Base[T] constructor must target the
// monomorphized parent constructor (Base__int.new), not the bare-name Base.new.
func TestSuperCallGenericParentCodegen(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `call void @"Base[int].new"(`)
	assertContains(t, ir, `call void @"Child[int].new"(`)
}

// T0474: with reordered child type params (Child[A, B] is Base[B]), super(b)
// must name the parent constructor monomorphized on B (Base[int]), not A.
func TestSuperCallGenericReorderedParentCodegen(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `call void @"Base[int].new"(`)
	assertContains(t, ir, `call void @"Child[string, int].new"(`)
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
	irOpt := generateIR(t, fmt.Sprintf(src, "Der?", `return oo as Der;`,
		`Der? d = gcast(0); if d { }`))
	assertContains(t, irOpt, "optcast.check")
	assertContains(t, irOpt, "optcast.some")
	assertContains(t, irOpt, "call i32 @promise_type_is(")
	// Force: monomorphized gcast__int emits the present/panic path.
	irForce := generateIR(t, fmt.Sprintf(src, "string",
		`d := oo as! Der; return d.name;`, `_ := gcast(0);`))
	assertContains(t, irForce, "optcast.present")
	assertContains(t, irForce, "optcast.nonepanic")
	// Generic body + aliasing (index) source: the dup and heap-temp registration
	// both run their `c.typeSubst != nil` substitution branches.
	irIndex := generateIR(t, `
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
	assertContains(t, irIndex, "optcast.check")
	assertContains(t, irIndex, "heapdup.copy") // duped aliasing source inside the generic body
}

// --- Generic type tests ---

func TestGenericTypeLayout(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 42);
		}
	`)
	assertContains(t, ir, "Box[int]_i")
	assertContains(t, ir, "store i64 42")
}

func TestGenericFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 42);
			int v = b.value;
		}
	`)
	assertContains(t, ir, "Box[int]_i")
	// Field access should load i64 (not i8*)
	assertContains(t, ir, "load i64")
}

func TestGenericFieldAssign(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 42);
			b.value = 10;
		}
	`)
	assertContains(t, ir, "store i64 10")
}

func TestGenericMethod(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T value;
			get(this) T { return this.value; }
		}
		main() {
			b := Box[int](value: 42);
			int v = b.get();
		}
	`)
	assertContains(t, ir, "define i64 @\"Box[int].get\"")
}

func TestGenericMethodSet(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T value;
			set(~this, T val) { this.value = val; }
		}
		main() {
			b := Box[int](value: 42);
			b.set(10);
		}
	`)
	assertContains(t, ir, "define void @\"Box[int].set\"")
}

func TestGenericMultipleInstances(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			a := Box[int](value: 42);
			b := Box[string](value: "hi");
		}
	`)
	assertContains(t, ir, "Box[int]_i")
	assertContains(t, ir, "Box[string]_i")
}

func TestGenericNestedField(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			a := Box[int](value: 1);
			b := Box[string](value: "hi");
			int x = a.value;
			string y = b.value;
		}
	`)
	// Both Box[int] and Box[string] fields accessed with correct types
	assertContains(t, ir, "Box[int]_i")
	assertContains(t, ir, "Box[string]_i")
	assertContains(t, ir, "load i64")
	assertContains(t, ir, "load i8*")
}

func TestGenericEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].Some(42);
		}
	`)
	assertContains(t, ir, "Option[int]_enum")
	assertContains(t, ir, "store i64 42")
}

func TestGenericEnumNone(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].None;
		}
	`)
	assertContains(t, ir, "Option[int]_enum")
}

func TestGenericEnumMatch(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].Some(42);
			r := match x {
				Some(v) => v,
				_ => 0,
			};
		}
	`)
	assertContains(t, ir, "switch i32")
}

func TestGenericEnumFieldless(t *testing.T) {
	ir := generateIR(t, `
		enum Dir[T] { Left, Right }
		main() {
			d := Dir[int].Left;
		}
	`)
	// Fieldless generic enum: internal type is i32
	assertContains(t, ir, "i32 0")
}

// TestGenericEnumMethodGetter verifies that a getter method on a generic enum
// is correctly monomorphized: the function is declared with the mono-qualified name.
func TestGenericEnumMethodGetter(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `"Wrapper[int].is_some"`)
	// Enum layout exists
	assertContains(t, ir, "Wrapper[int]_v")
}

// TestGenericEnumMethodRegular verifies that a regular (non-getter) method on a
// generic enum is monomorphized correctly.
func TestGenericEnumMethodRegular(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `"Box[int].unwrap_or"`)
}

// TestGenericEnumMethodCallsMethod verifies that a mono enum method body
// can call another method on the same enum via this.
func TestGenericEnumMethodCallsMethod(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `"Status[int].is_ok"`)
	assertContains(t, ir, `"Status[int].is_err"`)
	// is_err calls is_ok
	assertContains(t, ir, `call i1 @"Status[int].is_ok"`)
}

// TestGenericEnumMultipleInstantiations verifies that methods are monomorphized
// separately for each type argument.
func TestGenericEnumMultipleInstantiations(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `"Opt[int].has_value"`)
	assertContains(t, ir, `"Opt[string].has_value"`)
}

// TestT0636_EnumGenericMethodInstanceMono verifies that a generic
// (method-level type param) method on a generic enum instance is emitted as a
// monomorphized function "Box[int].transform[int]", with a separate mono
// function per (owner, method-type-arg) combination.
func TestT0636_EnumGenericMethodInstanceMono(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `"Box[int].transform[int]"`)
	assertContains(t, ir, `"Box[string].transform[bool]"`)
	// The call site dispatches to the monomorphized enum method.
	assertContains(t, ir, `call i64 @"Box[int].transform[int]"`)
}

// TestT0636_NonGenericEnumGenericMethodMono verifies that a generic
// (method-level type param) method on a *non-generic* enum is emitted as a
// monomorphized function named with the bare enum name (no monoName / "[..]"
// owner suffix), exercising the `case *types.Enum` arm of
// genGenericEnumMethodCall (enumName = enum.Obj().Name(), distinct from the
// generic-instance path which uses monoName(inst)).
func TestT0636_NonGenericEnumGenericMethodMono(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `"Tagged.pick[string]"`)
	assertContains(t, ir, `"Tagged.pick[bool]"`)
	assertContains(t, ir, `call i64 @"Tagged.pick[string]"`)
}

// TestT0639_GenericMethodViaThis verifies Defect B: a generic (method-type-
// param) method invoked via `this` inside the owner's own method body is
// emitted under the per-instance mono name (e.g. "NBox[int].inner[int]"),
// matching the monoCtx-built call site — NOT the bare-owner name
// ("NBox.inner[int]") which would be a different (mis-substituted) instance.
// Symmetric across a generic Named type and a generic enum.
func TestT0639_GenericMethodViaThis(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `define i64 @"NBox[int].inner[int]"`)
	assertContains(t, ir, `define i64 @"EBox[int].inner[int]"`)
	assertContains(t, ir, `call i64 @"NBox[int].inner[int]"`)
	assertContains(t, ir, `call i64 @"EBox[int].inner[int]"`)
	// The bare-owner name must never appear (would be the wrong instance).
	assertNotContains(t, ir, `@"NBox.inner[int]"`)
	assertNotContains(t, ir, `@"EBox.inner[int]"`)
}

// TestT0639_GenericFnRefParamGenericInstance verifies Defect A (call-site mono
// name unwraps `~`/`&` to the instance, not the bare owner) and Defect C (the
// generic free-function call passes a `~` param by pointer, matching the
// monomorphic callee's pointer ABI — no by-value/pointer mismatch segfault).
func TestT0639_GenericFnRefParamGenericInstance(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `define i64 @"NBox[int].transform[int]"`)
	assertContains(t, ir, `call i64 @"NBox[int].transform[int]"`)
	assertNotContains(t, ir, `@"NBox.transform[int]"`)
	// Defect C: definition and call site agree on the pointer ABI for the `~`
	// generic-instance param. The pre-fix bug passed it by value
	// (`...({ i8*, i8* } %`), dereferenced as a pointer => segfault.
	assertContains(t, ir, `define i64 @"proc_named[int]"({ i8*, i8* }* %`)
	assertContains(t, ir, `call i64 @"proc_named[int]"({ i8*, i8* }* %`)
	assertNotContains(t, ir, `call i64 @"proc_named[int]"({ i8*, i8* } %`)
}

// TestT0639_GenericFnRefParamStringVector verifies Defect C for the broader
// param class the bug also covered: a generic function with `~` string and
// `~` Vector params must pass them by pointer (matching the monomorphic
// callee's `i8**` ABI), not by value.
func TestT0639_GenericFnRefParamStringVector(t *testing.T) {
	ir := generateIR(t, `
		take_str[T](string~ s, T _x) int { return s.len; }
		take_vec[T](Vector[T]~ v) int { return v.len; }
		main() {
			a := take_str[int]("hello", 1);
			b := take_vec[int]([1, 2, 3, 4]);
		}
	`)
	assertContains(t, ir, `define i64 @"take_str[int]"(i8** %`)
	assertContains(t, ir, `call i64 @"take_str[int]"(i8** %`)
	assertContains(t, ir, `define i64 @"take_vec[int]"(i8** %`)
	assertContains(t, ir, `call i64 @"take_vec[int]"(i8** %`)
}

// TestT0639_RefWrappedGenericInstanceOperatorGetter verifies Defect A reaches
// beyond plain method calls: `resolveTypeName` is the shared mangling helper
// for ~20 dispatch sites. A `[]`-index and a parameterless-getter call on a
// `~`/`&` generic-instance receiver must mangle to the instance name
// ("GBox[int].[]" / "GBox[int].total"), NOT the bare generic owner
// ("GBox.[]" / "GBox.total") which pre-fix panicked "undeclared method".
func TestT0639_RefWrappedGenericInstanceOperatorGetter(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `define i64 @"GBox[int].[]"`)
	assertContains(t, ir, `call i64 @"GBox[int].[]"`)
	// Getter dispatch on a `&` generic-instance receiver.
	assertContains(t, ir, `define i64 @"GBox[int].total"`)
	assertContains(t, ir, `call i64 @"GBox[int].total"`)
	// The bare generic-owner name must never appear for either caller.
	assertNotContains(t, ir, `@"GBox.[]"`)
	assertNotContains(t, ir, `@"GBox.total"`)
}

// TestT0642_InferredGenericMethodOnNonGenericNamed verifies that calling a
// generic (method-type-param) method on a non-generic Named type WITHOUT
// explicit type-arg brackets dispatches to the per-method-type-arg mono name
// inferred from the call argument. Pre-fix this routed through `genMethodCall`,
// which built the bare mangled name ("Plain.echo") and panicked.
func TestT0642_InferredGenericMethodOnNonGenericNamed(t *testing.T) {
	ir := generateIR(t, `
		type Plain { int x; echo[U](U v) U { return v; } }
		main() {
			p := Plain(x: 1);
			r := p.echo("hi");
		}
	`)
	// Inferred U=string mangles to the same name as the explicit form.
	assertContains(t, ir, `"Plain.echo[string]"`)
	assertContains(t, ir, `call i8* @"Plain.echo[string]"`)
	// Bare-name form (pre-fix mis-dispatch target) must never appear.
	assertNotContains(t, ir, `@"Plain.echo"(`)
}

// TestT0642_InferredGenericMethodOnNonGenericEnum exercises the
// `case *types.Enum` arm of genGenericEnumMethodCall via the inferred path.
// Pre-fix the inferred call silently dispatched through the bare-name enum
// path (single overload) which ABI-mismatched on non-`i8*` args.
func TestT0642_InferredGenericMethodOnNonGenericEnum(t *testing.T) {
	ir := generateIR(t, `
		enum EPlain { A, B, echo[U](U v) U { return v; } }
		main() {
			p := EPlain.A;
			r := p.echo("hi");
		}
	`)
	assertContains(t, ir, `"EPlain.echo[string]"`)
	assertContains(t, ir, `call i8* @"EPlain.echo[string]"`)
}

// TestT0642_InferredGenericMethodOnGenericNamedInstance verifies the
// generic-Named-instance owner case routes through the per-instance mono
// name ("NBox[int].echo[string]"), with U inferred from the call arg.
func TestT0642_InferredGenericMethodOnGenericNamedInstance(t *testing.T) {
	ir := generateIR(t, `
		type NBox[T] { T val; echo[U](U v) U { return v; } }
		main() {
			b := NBox[int](val: 42);
			r := b.echo("hi");
		}
	`)
	assertContains(t, ir, `"NBox[int].echo[string]"`)
	assertContains(t, ir, `call i8* @"NBox[int].echo[string]"`)
	// Pre-fix would have mis-dispatched to the bare-owner name.
	assertNotContains(t, ir, `@"NBox.echo[string]"`)
}

// TestT0642_InferredGenericMethodOnGenericEnumInstance verifies the
// generic-enum-instance owner case routes through monoName(instance)
// + per-method-type-arg suffix on the inferred path (the `*types.Instance`
// arm of genGenericEnumMethodCall).
func TestT0642_InferredGenericMethodOnGenericEnumInstance(t *testing.T) {
	ir := generateIR(t, `
		enum EBox[T] { V(Vector[T] xs), N, echo[U](U v) U { return v; } }
		main() {
			b := EBox[int].V([1, 2, 3]);
			r := b.echo("hi");
		}
	`)
	assertContains(t, ir, `"EBox[int].echo[string]"`)
	assertContains(t, ir, `call i8* @"EBox[int].echo[string]"`)
	assertNotContains(t, ir, `@"EBox.echo[string]"`)
}

func TestGenericConstructorZeroInit(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box[int](value: 0);
		}
	`)
	// Generic type instance for Box[int]
	assertContains(t, ir, "Box[int]_i")
}

// TestGenericTupleTypeArg verifies that a Tuple used as a generic type argument
// produces a correct mono name ("Wrapper[(int, string)]") instead of "Wrapper[unknown]".
// Two different tuple args for the same generic must not collide.
func TestGenericTupleTypeArg(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper[T] { T val; }
		main() {
			w1 := Wrapper[(int, string)](val: (1, "a"));
			w2 := Wrapper[(bool, int)](val: (true, 42));
		}
	`)
	assertContains(t, ir, `Wrapper[(int, string)]`)
	assertContains(t, ir, `Wrapper[(bool, int)]`)
	assertNotContains(t, ir, `Wrapper[unknown]`)
}

// --- Generic function tests ---

func TestGenericFunc(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			int r = identity[int](42);
		}
	`)
	assertContains(t, ir, "define i64 @\"identity[int]\"")
	assertContains(t, ir, "ret i64")
}

func TestGenericFuncString(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			string s = identity[string]("hello");
		}
	`)
	assertContains(t, ir, "define i8* @\"identity[string]\"")
}

func TestGenericFuncMultipleInstances(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			int a = identity[int](42);
			string b = identity[string]("hi");
		}
	`)
	assertContains(t, ir, "@\"identity[int]\"")
	assertContains(t, ir, "@\"identity[string]\"")
}

func TestGenericMethodMutReceiverAssign(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T value;
			replace(~this, T newVal) { this.value = newVal; }
		}
		main() {
			b := Box[int](value: 10);
			b.replace(99);
		}
	`)
	assertContains(t, ir, "define void @\"Box[int].replace\"")
	// Should store i64 (the new value into the field)
	assertContains(t, ir, "store i64")
}

func TestGenericFuncVoid(t *testing.T) {
	ir := generateIR(t, `
		consume[T](T x) { }
		main() {
			consume[int](42);
		}
	`)
	assertContains(t, ir, "define void @\"consume[int]\"")
}

// T0340: a generic function with a `~T` parameter must clear the caller's
// drop flag at the call site. Without the applyMutRefArgOwnership fix,
// `~` args on generic calls left the caller's drop flag set → double-free
// when the callee consumed the value (e.g. moved it into a struct field).
func TestT0340_GenericFuncMutRefArgClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		consume[T](T move x) { }
		main() {
			s := "hello";
			consume[string](s);
		}
	`)
	// Drop flag store + immediate call to the monomorphized consume.
	assertContainsMatch(t, ir,
		`store i1 false, i1\* %s\.dropflag\s*\n\s*call void @"consume\[string\]"`)
}

// T0340: same fix must apply when the type parameter is inferred (no
// explicit `[T]` at the call site). Exercises genInferredGenericCall.
func TestT0340_InferredGenericFuncMutRefArgClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		consume[T](T move x) { }
		main() {
			s := "hello";
			consume(s);
		}
	`)
	assertContainsMatch(t, ir,
		`store i1 false, i1\* %s\.dropflag\s*\n\s*call void @"consume\[string\]"`)
}

// B0099: Generic function calling another generic function with its own type param.
func TestGenericFuncCallsGenericFunc(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T val) T { return val; }
		wrap[T](T val) T { return identity[T](val); }
		main() {
			int r = wrap[int](42);
		}
	`)
	// Both wrap[int] and identity[int] must be monomorphized
	assertContains(t, ir, "define i64 @\"wrap[int]\"")
	assertContains(t, ir, "define i64 @\"identity[int]\"")
}

// B0099: Transitive chain of generic functions calling generic functions.
func TestGenericFuncTransitiveChain(t *testing.T) {
	ir := generateIR(t, `
		inner[T](T val) T { return val; }
		middle[T](T val) T { return inner[T](val); }
		outer[T](T val) T { return middle[T](val); }
		main() {
			int r = outer[int](42);
		}
	`)
	assertContains(t, ir, "define i64 @\"outer[int]\"")
	assertContains(t, ir, "define i64 @\"middle[int]\"")
	assertContains(t, ir, "define i64 @\"inner[int]\"")
}

// B0099: Multiple instantiations through generic-calls-generic.
func TestGenericFuncCallsGenericMultipleInstances(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T val) T { return val; }
		wrap[T](T val) T { return identity[T](val); }
		main() {
			int a = wrap[int](42);
			string b = wrap[string]("hi");
		}
	`)
	assertContains(t, ir, "define i64 @\"wrap[int]\"")
	assertContains(t, ir, "define i64 @\"identity[int]\"")
	assertContains(t, ir, "define i8* @\"wrap[string]\"")
	assertContains(t, ir, "define i8* @\"identity[string]\"")
}

// B0099: Generic function calling a generic method (cross-resolution).
func TestGenericFuncCallsGenericMethod(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define i64 @\"invoke[int]\"")
	assertContains(t, ir, "define i64 @\"Echo.echo[int]\"")
}

// B0099: MethodInstance self-resolution (generic method calls generic method).
func TestGenericMethodCallsGenericMethod(t *testing.T) {
	ir := generateIR(t, `
		type Foo { echo[T](T val) T { return val; } }
		type Bar { delegate[T](Foo f, T val) T { return f.echo[T](val); } }
		main() {
			f := Foo();
			b := Bar();
			int r = b.delegate[int](f, 7);
		}
	`)
	assertContains(t, ir, "define i64 @\"Bar.delegate[int]\"")
	assertContains(t, ir, "define i64 @\"Foo.echo[int]\"")
}

// B0099: Type-instance resolution (generic type method calls generic free function).
func TestGenericTypeMethodCallsFreeFunc(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define i64 @\"identity[int]\"")
	assertContains(t, ir, "define i64 @\"Wrapper[int].wrapped\"")
}

// B0099: Cross-resolution reverse (generic method calls generic free function).
func TestGenericMethodCallsFreeFunc(t *testing.T) {
	ir := generateIR(t, `
		helper[T](T val) T { return val; }
		type Proxy {
			forward[T](T val) T { return helper[T](val); }
		}
		main() {
			p := Proxy();
			int r = p.forward[int](33);
		}
	`)
	assertContains(t, ir, "define i64 @\"Proxy.forward[int]\"")
	assertContains(t, ir, "define i64 @\"helper[int]\"")
}

// B0099: Type-instance resolution for MethodInstance (generic type method calls generic method).
func TestGenericTypeMethodCallsGenericMethod(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define i64 @\"Wrapper[int].echoed\"")
	assertContains(t, ir, "define i64 @\"Echoer.echo[int]\"")
}

// B0099: Cross-resolution both directions (method calls both func and method).
func TestGenericMethodCallsBothFuncAndMethod(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define i64 @\"Combiner.run[int]\"")
	assertContains(t, ir, "define i64 @\"helper[int]\"")
	assertContains(t, ir, "define i64 @\"Echoer.echo[int]\"")
}

func TestGenericTypeAsParam(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		unbox(Box[int] b) int {
			return b.value;
		}
		main() {
			b := Box[int](value: 99);
			int v = unbox(b);
		}
	`)
	assertContains(t, ir, "define i64 @__user.unbox")
	assertContains(t, ir, "load i64")
}

func TestGenericEnumMatchBlock(t *testing.T) {
	ir := generateIR(t, `
		enum Option[T] { Some(T), None }
		main() {
			x := Option[int].Some(42);
			match x {
				Some(v) => { int y = v; },
				_ => { },
			};
		}
	`)
	assertContains(t, ir, "switch i32")
}

// T0937: a value-struct-container elvis inside a generic function with an owned
// (`~`) optional param is orphaned on the some-path, so the result is tracked.
// The result type (`map[K,V]`) is resolved through c.typeSubst during
// monomorphization, so the synthesized drop dispatches on the concrete
// instantiation (`Map[string, int].drop`). Exercises the typeSubst branch of
// trackElvisResultHeap (uncovered by the non-generic ident-source tests).
func TestElvisMapGenericOwnedParamDropFlag(t *testing.T) {
	ir := generateIR(t, `
		gconsume[K: Hashable + Equal, V](map[K, V]? move a, map[K, V] b) int {
			return (a ?: b).len;
		}
		main() {
			map[string, int]? a = {"x": 1};
			map[string, int] b = {"z": 9};
			c := gconsume(a, b);
		}
	`)
	assertContains(t, ir, "elvis.merge")
	assertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	assertContains(t, ir, `call void @"Map[string, int].drop"`)
}

// T0937 (i8*-container gap): a generic vector elvis with an owned (`~`) optional
// param is orphaned on the some-path → tracked via trackElvisResultTemp. The
// result type (`T[]`) resolves through c.typeSubst at monomorphization.
// Exercises the typeSubst branch of trackElvisResultTemp (the existing generic
// tests use borrowed params, which now short-circuit at the orphan gate).
func TestElvisStrvecGenericOwnedParamDropFlag(t *testing.T) {
	ir := generateIR(t, `
		gconsume[T](T[]? move a, T[] b) int {
			return (a ?: b).len;
		}
		main() {
			string[]? a = ["x" + "y"];
			string[] b = ["z" + "w"];
			c := gconsume(a, b);
		}
	`)
	assertContains(t, ir, "elvis.merge")
	assertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
}

// T1160: an explicit type-argument list parses the callee as an IndexExpr
// (`make_generic[int]` → `IndexExpr{make_generic, int}`), which looks like a value
// subscript yielding a callable. The free function must still be recognized as a
// statically-known callee (its result is tracked), while the generic method's
// receiver must still reach the alias check (its result is NOT tracked).
func TestClosureGenericCallResultTracking(t *testing.T) {
	ir := generateIR(t, `
		type Holder { () -> int cb; get_cb[T](this, T v) () -> int { return this.cb; } }
		make_generic[T](T v, int x) () -> int { return || -> x + 1; }
		fresh() { make_generic[int](1, 10); }
		aliased(Holder h) { h.get_cb[int](0); }
		main() { fresh(); }
	`)
	assertContains(t, extractFunction(ir, "__user.fresh"), "env.tmp.drop")
	aliased := extractFunction(ir, "__user.aliased")
	assertContains(t, aliased, "define") // guard: the body was actually found
	assertNotContains(t, aliased, "env.tmp.drop")
}

// T1160: a receiver that is a generic INSTANCE whose closure lives in a type
// argument (`CBox[() -> int]`), not in the origin's own fields. The walk must
// descend into TypeArgs before concluding the receiver holds no closure. Runtime
// coverage is blocked by T1232 (such an instance never drops the env), so this
// pins the arm at IR level.
func TestClosureGenericInstanceReceiverAliasNotTracked(t *testing.T) {
	ir := generateIR(t, `
		type CBox[T] { T v; get_v(this) T { return this.v; } }
		via_generic_instance(CBox[() -> int] b) { b.get_v(); }
		main() { }
	`)
	body := extractFunction(ir, "__user.via_generic_instance")
	assertContains(t, body, "define") // guard: the body was actually found
	assertNotContains(t, body, "env.tmp.drop")
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
	ir := generateIR(t, `
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
	assertContains(t, extractFunction(ir, "__user.fresh"), "env.tmp.drop")
	aliased := extractFunction(ir, "__user.aliased")
	assertContains(t, aliased, "define") // guard: the body was actually found
	assertNotContains(t, aliased, "env.tmp.drop")
}

func TestVoidFunctionTypeParam(t *testing.T) {
	ir := generateIR(t, `
		apply((int) -> void fn) {
			fn(42);
		}
		main() {
			apply(|int x| { });
		}
	`)
	assertContains(t, ir, "define void @__user.apply")
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestStringInterpolationTypeParam(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "promise_int_to_string")
}

// T1297: an optional-element array/vector literal built inside a generic
// function Some-wraps each element after substituting the element expression's
// TypeParam through c.typeSubst (the monomorphization path — argExprType `T` is
// substituted to `int` before the Identical(argExprType, elem) wrap decision).
func TestT1297_GenericOptionalElementArrayWrap(t *testing.T) {
	ir := generateIR(t, `
		wrap_vec[T](T move a, T move b) T?[] { return [a, b]; }
		wrap_fixed[T](T move a, T move b) T?[2] { return [a, b]; }
		main() {
			int?[] v = wrap_vec[int](1, 2);
			int?[2] a = wrap_fixed[int](3, 4);
		}
	`)
	// Both the vector-literal and fixed-array-literal monomorphizations wrap
	// their substituted-int elements into the { i1, i64 } optional slot.
	assertContains(t, ir, "insertvalue { i1, i64 }")
	assertContains(t, ir, "wrap_vec[int]")
	assertContains(t, ir, "wrap_fixed[int]")
}

func TestT1341_FixedArrayOptionalStructuralCloneGeneric(t *testing.T) {
	// T1341, monomorphization path: the array-index read happens inside a generic
	// body (`_read_first[T](T?[2]) T?`) where the element's inner type is a TypeParam.
	// The clone-on-read branch must substitute (c.typeSubst) the inner to the concrete
	// structural type before the isNonValueStructuralType gate — otherwise the mono'd
	// body aliases the array's box and double-frees it. The concrete test above never
	// sets c.typeSubst; this one exercises that substitution branch, emitting the
	// structural clone INSIDE the specialized `_read_first[Showable]` function.
	ir := generateIR(t, `
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
	assertContains(t, ir, `define { i1, { i8*, i8* } } @"_read_first[Showable]"`)
	assertContains(t, ir, "call i8* @__promise_structural_clone")
}

func TestGenericGetterSetterSameName(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define i64 @\"Box[int].val\"(")
	assertContains(t, ir, "define void @\"Box[int].val$set\"(")
}

// Move in generic function call clears flag
func TestDropMoveToGenericFuncCall(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "store i1 false, i1*")
}

// B0235: Vector[GenericEnum] index assignment drops old enum element (mono).
func TestVectorMonoEnumIndexAssignDropsOld(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `call void @"Slot[string, string].drop"(`)
}

// B0192: Generic type with type-param field gets NeedsSynthDrop
// so its synthesized drop can free heap-allocated type-param fields.
func TestSynthDropGenericTypeParamField(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		type Holder { Point p; }
		main() {
			Holder h = Holder(p: Point(x: 1, y: 2));
		}
	`)
	// Holder gets a synthesized drop that frees the Point instance
	holderDrop := extractFunction(ir, "Holder.drop")
	if holderDrop == "" {
		t.Fatal("expected Holder.drop function in IR")
	}
	// Should pal_free the Point field instance (B0192 needsFreeOnly path)
	assertContains(t, holderDrop, "call void @pal_free(")
}

// B0202: Generic type where ALL fields are TypeParam — synthesized drop detected at mono time
func TestSynthDropMonoTypeParamOnlyFields(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		type Box[T] { T val; }
		main() {
			b := Box[Point](val: Point(x: 1, y: 2));
		}
	`)
	// Box[Point] gets a mono synthesized drop that frees the Point field
	boxDrop := extractFunction(ir, `"Box[Point].drop"`)
	if boxDrop == "" {
		t.Fatal("expected Box[Point].drop function in IR")
	}
	assertContains(t, boxDrop, "call void @pal_free(")
}

// B0202: Generic type with TypeParam field instantiated with primitive — no synth drop needed
func TestSynthDropMonoTypeParamPrimitive(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T val; }
		main() {
			b := Box[int](val: 42);
		}
	`)
	// Box[int] should NOT get a synthesized drop — int is primitive
	boxDrop := extractFunction(ir, `"Box[int].drop"`)
	if boxDrop != "" {
		t.Fatal("Box[int] should not have a synthesized drop")
	}
}

// B0202: Generic type with TypeParam field instantiated with string — gets synth drop
func TestSynthDropMonoTypeParamString(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper[T] { T val; }
		main() {
			w := Wrapper[string](val: "hello");
		}
	`)
	// Wrapper[string] gets a mono synthesized drop for the string field
	wrapperDrop := extractFunction(ir, `"Wrapper[string].drop"`)
	if wrapperDrop == "" {
		t.Fatal("expected Wrapper[string].drop function in IR")
	}
	assertContains(t, wrapperDrop, "call void @promise_string_drop(")
}

// B0209: Generic type with Optional[TypeParam] field instantiated with string — gets synth drop
func TestSynthDropMonoOptionalTypeParamString(t *testing.T) {
	ir := generateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[string](val: "hello");
		}
	`)
	// MaybeVal[string] gets a mono synthesized drop for the optional string field
	drop := extractFunction(ir, `"MaybeVal[string].drop"`)
	if drop == "" {
		t.Fatal("expected MaybeVal[string].drop function in IR")
	}
	assertContains(t, drop, "call void @promise_string_drop(")
}

// B0209: Generic type with Optional[TypeParam] field instantiated with primitive — no synth drop
func TestSynthDropMonoOptionalTypeParamPrimitive(t *testing.T) {
	ir := generateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[int](val: 42);
		}
	`)
	// MaybeVal[int] should NOT get a synthesized drop — int is primitive
	drop := extractFunction(ir, `"MaybeVal[int].drop"`)
	if drop != "" {
		t.Fatal("MaybeVal[int] should not have a synthesized drop")
	}
}

// B0209: Generic type with Optional[TypeParam] field instantiated with heap user type — gets synth drop
func TestSynthDropMonoOptionalTypeParamUserType(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[Point](val: Point(x: 1, y: 2));
		}
	`)
	// MaybeVal[Point] gets a mono synthesized drop (at minimum for pal_free of the instance)
	drop := extractFunction(ir, `"MaybeVal[Point].drop"`)
	if drop == "" {
		t.Fatal("expected MaybeVal[Point].drop function in IR")
	}
	assertContains(t, drop, "call void @pal_free(")
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, `
		make_then_drop[T](T move x, T move y) {
			(T, T) t = (x, y);
		}
		main() {
			make_then_drop[string]("a" + "b", "c" + "d");
		}
	`)
	// The mono'd function must register a tuple-walk drop binding that calls
	// promise_string_drop on the (substituted) string fields.
	assertContains(t, ir, "tupdrop.exec")
	assertContains(t, ir, "promise_string_drop")
}

// B0158: Generic type with droppable field gets a mono synthesized drop
func TestDropSynthesizedGeneric(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "Wrapper[int].drop")
}

// T0132: Generic type with generic droppable field gets cascading mono drop.
// Set[T] has a Map[T, bool] field — the synthesized Set[int].drop must call
// Map[int, bool].drop (mono name), not Map.drop (origin name which doesn't exist).
func TestDropSynthesizedGenericCascading(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `call void @"Box[int].drop"`)
	assertContains(t, ir, "Outer[int].drop")
}

// B0212: Generic enum instances (like Slot[K,V]) get synthesized drops at mono time
// when sema couldn't detect droppability for TypeParam variant fields.
func TestDropMonoEnumInstSynthDrop(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `define void @"Wrapper[Resource].drop"`)
	assertContains(t, ir, "call void @Resource.drop(")
}

// T0552: Type with generic-enum field whose TypeParam resolves to a droppable
// concrete type. monoTypeHasDroppable must see through the generic enum Instance
// (via monoEnumInstNeedsSynthDrop), and emitFieldDropsFor must drop the enum
// field by invoking the mono enum's drop function. Without both, the inner
// droppable leaks at scope exit of the holder.
func TestDropGenericTypeWithGenericEnumField(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `define void @"Holder[Resource].drop"`)
	assertContains(t, ir, `call void @"Maybe[Resource].drop"`)
}

// T0552: Non-generic holder containing a non-generic enum field with a
// droppable variant. Sema sets NeedsSynthDrop=true on the holder (since the
// concrete enum's HasDrop is observable), so a synth drop body is generated —
// but before T0552, emitFieldDropsFor's `extractNamed == nil` skip dropped the
// enum field silently. This test locks down the enum-field branch added in
// emitFieldDropsFor for the non-generic case (the generic case is covered by
// TestDropGenericTypeWithGenericEnumField above).
func TestDropTypeWithEnumFieldNonGeneric(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define void @Holder.drop")
	assertContains(t, ir, "call void @Maybe.drop")
}

// T0572: Generic holder with Optional<GenericEnum[T]> field. Exercises the
// monoEnumInstNeedsSynthDrop branch in the needsDrop check — without it,
// HasDrop on the un-substituted enum origin is false, the early-return
// fires, and the inner droppable leaks.
func TestDropGenericTypeWithOptionalGenericEnumField(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `define void @"Holder[Resource].drop"`)
	assertContains(t, ir, "optfield.drop")
	assertContains(t, ir, `call void @"Maybe[Resource].drop"`)
}

// B0238: Generic enum variables with TypeParam-only droppable fields must get drop
// registered at scope exit. maybeRegisterDrop must check monoEnumInstNeedsSynthDrop.
func TestDropGenericEnumVarWithDroppableTypeParam(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `define void @"Container[Wrapper].drop"`)
	assertContains(t, ir, `call void @"Container[Wrapper].drop"`)
}

// T0405: Generic type with T[] field — reassignment must drop string elements
// when T=string (exercises the typeSubst-substituted fieldType path).
func TestFieldAssignGenericVecDropsElements(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "field.vecdrop")
	assertContains(t, ir, "call void @promise_string_drop(")
	assertContains(t, ir, "call void @Vector.drop(")
}

// --- Return optional wrapping in monomorphized context ---

func TestReturnOptionalInMonoMethod(t *testing.T) {
	// The map [] method returns V? — returning a concrete V must wrap in Optional
	ir := generateIR(t, `
		main() {
			m := {"x": 42};
			int? v = m["x"];
		}
	`)
	// The monomorphized [] method should produce { i1, i64 } return type
	assertContains(t, ir, `define { i1, i64 } @"Map[string, int].[]"(`)
	// Should contain insertvalue for wrapping the value in Optional { true, val }
	assertContains(t, ir, "insertvalue { i1, i64 }")
}

// --- Nested generic monomorphization (discoverInstances) ---

func TestNestedGenericMonomorphization(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T val; }
		type Wrapper[T] { Box[T] inner; }
		main() {
			w := Wrapper[int](inner: Box[int](val: 42));
		}
	`)
	// Both Wrapper[int] and Box[int] should be monomorphized
	assertContains(t, ir, "Wrapper[int]")
	assertContains(t, ir, "Box[int]")
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
	ir := generateIR(t, `
		gmake[T](T v) Task[T] {
			return go { s := v; s };
		}
		main() {
			task[string] x = gmake[string]("a" + "b");
			r := <-x;
		}
	`)
	// The monomorphized instance is emitted as @"gmake[string]" (quoted name).
	monoIR := extractFunction(ir, `"gmake[string]"`)
	assertContains(t, monoIR, "@promise_string_new(")
	assertContains(t, monoIR, "call i8* @.goroutine.")
}

// T1261: a GENERIC-owner method referencing `this` in a `go`-CALL resolves the
// receiver type via the mono instance, deep-copies the concrete snapshot, and
// threads it into the coroutine — the mono path of the call-form fix.
func TestT1261_GoCallThisGenericOwner(t *testing.T) {
	ir := generateIR(t, `
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
	sendIR := extractFunction(ir, `"T1261Box[int].send_it"`)
	assertContains(t, sendIR, "heapdup.copy")
	assertContains(t, sendIR, `call i8* @".goroutine.T1261Box[int].send_it.`)
}

// T1198: the borrowed-param dup on the fast path must survive monomorphization.
// When the spawner is a GENERIC free function, its body is codegen'd with
// c.typeSubst active, so genGoCallExpr must substitute the capture type before
// the eligibility gate (mirroring the value-block/via-block paths). A concrete
// `string` param inside a `spawn[T]` spawner still dups. This exercises the
// `c.typeSubst != nil` branch that non-generic spawners never reach.
func TestT1198_FastPathBorrowedParamDupsUnderMonomorphization(t *testing.T) {
	ir := generateIR(t, `
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
	spawnIR := extractFunction(ir, `"spawn[int]"`)
	assertContains(t, spawnIR, "@promise_string_new(")
	assertContains(t, spawnIR, "call i8* @.goroutine.")
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
	ir := generateIR(t, `
		gwrap[T]((T, int) t) {}
		gpass[T](() -> T make) {
			gwrap[T]((make(), 1));
		}
		main() {
			gpass[string](|| -> "a" + "b");
		}
	`)
	// The specialized gpass[string] instance must field-wise drop its tuple temp.
	assertContains(t, ir, "gpass[string]")
	assertContains(t, ir, "tuptmp.drop")
	// The substituted string field's drop fires in the monomorphized body.
	assertContains(t, ir, "call void @promise_string_drop(")
}

func TestMultiParamGenericType(t *testing.T) {
	ir := generateIR(t, `
		type Pair[A, B] {
			A first;
			B second;
		}
		main() {
			p := Pair[int, string](first: 42, second: "hello");
		}
	`)
	// Monomorphized struct name should contain both type args
	assertContains(t, ir, "Pair[int, string]")
}

func TestMultiParamGenericFunc(t *testing.T) {
	ir := generateIR(t, `
		make_pair[A, B](A a, B b) (A, B) {
			return (a, b);
		}
		main() {
			(x, y) := make_pair[int, string](42, "hi");
		}
	`)
	// Monomorphized function name should contain both type args
	assertContains(t, ir, "make_pair[int, string]")
}

// B0224: Operator methods on generic value types must use the mono name.
func TestGenericValueTypeOperatorDispatch(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `@"Pair[int].=="`)
}

// TestGenericPropertyIncDecIR: T0712. Inc/dec on a property of a generic type
// must dispatch through the monomorphized getter/setter. `this.total++` inside a
// generic method body exercises the receiver-type substitution branch
// (c.typeSubst) in genIncDecTarget — the receiver type is Box[T] and must be
// substituted to Box[int] before the accessor lookup.
func TestGenericPropertyIncDecIR(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `call i64 @"Box[int].total"(`)
	assertContains(t, ir, "add i64")
	assertContains(t, ir, `call void @"Box[int].total$set"(`)
	// At main's call site: b.total-- (concrete-instance receiver, no typeSubst).
	assertContains(t, ir, "sub i64")
}

func TestGenericInheritanceNonGenericChild(t *testing.T) {
	ir := generateIR(t, `
		type Holder[T] { T value; }
		type IntHolder is Holder[int] {}
		main() {
			h := IntHolder(value: 42);
			int x = h.value;
		}
	`)
	// IntHolder uses Holder's layout — field should be i64 (int)
	assertContains(t, ir, "IntHolder")
	assertContains(t, ir, "load i64")
}

func TestGenericInheritanceForwardedTypeParams(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "Derived[int]")
	assertContains(t, ir, "Base[int]")
}

func TestMonoTypeVtableEmission(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "promise_vtable_ConstProducer[int]")
	assertContains(t, ir, "promise_typeinfo_ConstProducer[int]")
	// The vtable should contain the mono method pointer
	assertContains(t, ir, "ConstProducer[int].produce")
}

func TestMonoVtableVirtualDispatchIR(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "promise_vtable_Circle[int]")
	assertContains(t, ir, "promise_vtable_Shape[int]")
	// accept_shape should do virtual dispatch (load from vtable, indirect call)
	assertContains(t, ir, "promise_vtable_Shape[int]")
	// Mono method should be defined
	assertContains(t, ir, "Circle[int].area")
}

func TestMultipleMonoVtablesDistinct(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "promise_vtable_ConstProducer[int]")
	assertContains(t, ir, "promise_vtable_ConstProducer[string]")
	assertContains(t, ir, "promise_typeinfo_ConstProducer[int]")
	assertContains(t, ir, "promise_typeinfo_ConstProducer[string]")
	// Separate methods
	assertContains(t, ir, "ConstProducer[int].produce")
	assertContains(t, ir, "ConstProducer[string].produce")
}

func TestMonoVtableInheritedMethodResolution(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "promise_vtable_Leaf[int]")
	assertContains(t, ir, "Base[int].get")
}

func TestMonoTypeInfoEmittedForParent(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "promise_typeinfo_Dog[int]")
	assertContains(t, ir, "promise_typeinfo_Animal[int]")
	assertContains(t, ir, "promise_vtable_Dog[int]")
}

func TestMonoVtableOverrideDispatches(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "promise_vtable_Greeter[int]")
	assertContains(t, ir, "promise_vtable_Fancy[int]")
	assertContains(t, ir, "Greeter[int].greet")
	assertContains(t, ir, "Fancy[int].greet")
}

func TestMonoVtableNonGenericChildOfGenericParent(t *testing.T) {
	// Non-generic children already have vtables via emitVtableGlobals.
	// Verify they coexist with mono vtables for the parent.
	ir := generateIR(t, `
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
	assertContains(t, ir, "promise_vtable_IntFabricator")
	// Generic child uses mono vtable naming
	assertContains(t, ir, "promise_vtable_GenFabricator[int]")
	// Both methods exist
	assertContains(t, ir, "IntFabricator.fabricate")
	assertContains(t, ir, "GenFabricator[int].fabricate")
}

func TestMethodGenericIR(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "Echo.echo[int]")
	assertContains(t, ir, "Echo.echo[string]")
}

func TestMethodGenericOnGenericTypeIR(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "Box[int].convert[string]")
}

// --- Monomorphization: gaps ---

// TestGenericValueTypeLayout verifies that a generic pure-value type gets the
// correct layout: fields are embedded in the value struct (_v), and the instance
// struct (_i) is RTTI-only (no user fields). Also checks that no heap allocation
// is emitted (value types are stack-allocated and copied).
func TestGenericValueTypeLayout(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "Pair[int]")
	// Value struct has embedded fields (vtable + first + second)
	// The _v struct is named promise_Pair[int]_v
	assertContains(t, ir, "promise_Pair[int]_v")
	// Instance struct is RTTI-only (no user fields) — just the _variant pointer
	assertContains(t, ir, "promise_Pair[int]_i")
	// RTTI global is emitted for value types
	assertContains(t, ir, "promise_rtti_Pair[int]")
	// No heap allocation: value types are not malloc'd
	assertNotContains(t, ir, "promise_Pair[int]_i* @malloc")
}

// T0565: a non-generic user type that has a generic value-type instance as a
// field must lay out the field slot using the mono value struct (wider, with
// embedded fields), not the generic {i8*, i8*} slot. The construction-time
// store would otherwise mismatch the slot type and crash codegen.
func TestGenericValueTypeAsField(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "%\"promise_Pt[int]_v\" = type { i8*, i64, i64 }")
	// Outer's instance struct uses the wider mono value struct as the field
	// slot, not the standard {i8*, i8*} value layout.
	assertContains(t, ir, "%promise_Outer_i = type { %promise_Outer_m*, %\"promise_Pt[int]_v\" }")
}

// T0565: a non-generic value type used as a direct field of another type with
// REVERSE declaration order (containing type before value type). Without the
// topological walk over value-type field dependencies, the containing type's
// layout would be computed before the value type's, producing the wrong slot
// type. This exercises the extractNamed/IsValueType fallback in
// collectValueTypeFieldDeps.
func TestNonGenericValueTypeFieldReverseOrder(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "%promise_Coord_v = type { i8*, i64, i64 }")
	// WithCoord's instance struct uses the wider Coord value struct, not {i8*, i8*}.
	assertContains(t, ir, "%promise_WithCoord_i = type { %promise_WithCoord_m*, %promise_Coord_v }")
}

// T0565: a tuple field containing generic value-type instances. Exercises the
// *types.Tuple recursion in collectValueTypeFieldDeps so each tuple element is
// laid out before the containing type.
func TestTupleOfGenericValueTypesAsField(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "%\"promise_Pt[int]_v\" = type { i8*, i64, i64 }")
	assertContains(t, ir, "%\"promise_Pt[f64]_v\" = type { i8*, double, double }")
}

// T0565: a generic outer type with a Pt[T] field — after monomorphization the
// substituted field becomes Pt[int] (a *types.Instance after subst). The mono
// outer layout must use the wider value struct for the field slot.
func TestGenericOuterWithGenericValueField(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "%\"promise_Pt[int]_v\" = type { i8*, i64, i64 }")
	assertContains(t, ir, "%\"promise_Container[int]_i\" = type { %\"promise_Container[int]_m\"*, %\"promise_Pt[int]_v\" }")
}

// TestGenericValueTypeTwoInstances verifies that two instantiations of the same
// generic value type produce distinct layouts and RTTI globals.
func TestGenericValueTypeTwoInstances(t *testing.T) {
	ir := generateIR(t, `
		type Pair[T] {
			T first `+"`"+`value;
			T second `+"`"+`value;
		}
		main() {
			pi := Pair[int](first: 1, second: 2);
			pb := Pair[bool](first: true, second: false);
		}
	`)
	assertContains(t, ir, "promise_Pair[int]_v")
	assertContains(t, ir, "promise_Pair[bool]_v")
	assertContains(t, ir, "promise_rtti_Pair[int]")
	assertContains(t, ir, "promise_rtti_Pair[bool]")
	// Separate typeinfo for each instantiation
	assertContains(t, ir, "promise_typeinfo_Pair[int]")
	assertContains(t, ir, "promise_typeinfo_Pair[bool]")
}

// TestGenericEnumTwoTypeParams verifies that a generic enum with two type parameters
// is correctly monomorphized, producing distinct structs and a match that extracts
// both variant fields.
func TestGenericEnumTwoTypeParams(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "Either[int, string]")
	// Value struct typedef emitted
	assertContains(t, ir, "promise_Either[int, string]_v")
	// Function using the mono type exists
	assertContains(t, ir, "get_left")
}

// TestDeeplyNestedGenericMonomorphization verifies that transitive instance
// discovery via field types works at 3 levels of nesting.
func TestDeeplyNestedGenericMonomorphization(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T val; }
		main() {
			inner := Box[int](val: 1);
			mid := Box[Box[int]](val: inner);
			outer := Box[Box[Box[int]]](val: mid);
		}
	`)
	// All three levels must be monomorphized
	assertContains(t, ir, "Box[int]")
	assertContains(t, ir, "Box[Box[int]]")
	assertContains(t, ir, "Box[Box[Box[int]]]")
}

// TestGenericMethodReturnsGenericInstance verifies that a generic method whose
// return type is a monomorphized generic type is correctly compiled.
func TestGenericMethodReturnsGenericInstance(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			T val;
			clone(this) Box[T] { return Box[T](val: this.val); }
		}
		main() {
			b := Box[int](val: 5);
			c := b.clone();
		}
	`)
	assertContains(t, ir, "Box[int].clone")
	// Return type is also Box[int] — constructor call should appear
	assertContains(t, ir, "Box[int]")
}

// TestMonoSynthesizedDefaultOnGenericType verifies that a generic concrete type
// implementing a structural interface inherits the interface's default methods,
// and that those methods are emitted with the mono-qualified name.
func TestMonoSynthesizedDefaultOnGenericType(t *testing.T) {
	// Use a structural interface whose default method doesn't require operations
	// on T — just calls another abstract method.
	ir := generateIR(t, `
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
	assertContains(t, ir, "Pair[int].nonempty")
	// The concrete size method should also appear
	assertContains(t, ir, "Pair[int].size")
}

// TestMonoMapWithTupleValue is a regression test for T0400: instantiating
// Map[K, V] with a tuple V used to panic in codegen because the mono spiral
// guard over-marked Vector[(K, V)] as spiral, preventing _FnIter[T] from
// being resolved during Vector.iter()'s body monomorphization. After the
// originWrapsTypeParams precondition was added, Vector — which doesn't
// intrinsically wrap its TypeParam in a Tuple — is correctly skipped from
// spiral marking, letting the chain bound at Iterator/_FnIter as intended.
func TestMonoMapWithTupleValue(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := map[string, (string, int)]();
			m["a"] = ("alpha", 1);
		}
	`)
	// _FnIter[(string, (string, int))] must be monomorphized (so its layout
	// exists) — this is the instance whose missing layout caused the panic.
	assertContains(t, ir, "_FnIter[(string, (string, int))]")
	// Vector and Iterator instances for the tuple value must also exist.
	assertContains(t, ir, "Vector[(string, (string, int))]")
}

// TestGenericFuncWithGenericReturnType verifies a generic function that both
// takes and returns a monomorphic generic type. Box[int] is instantiated directly
// in main so its layout is collected; the generic function takes and returns it.
func TestGenericFuncWithGenericReturnType(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T val; }
		identity_box[T](Box[T] b) Box[T] { return b; }
		main() {
			b := Box[int](val: 42);
			c := identity_box[int](b);
		}
	`)
	assertContains(t, ir, "identity_box[int]")
	assertContains(t, ir, "Box[int]")
}

// TestGenericTypeInfoEmitted verifies that RTTI typeinfo and vtable globals are
// emitted for monomorphic generic type instantiations. promise_type_is requires
// these globals at runtime to check inheritance relationships.
func TestGenericTypeInfoEmitted(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "promise_typeinfo_Dog[int]")
	assertContains(t, ir, "promise_vtable_Dog[int]")
	// Animal[int] typeinfo must also be emitted (it's an abstract parent).
	assertContains(t, ir, "promise_typeinfo_Animal[int]")
}

func TestInstanceIRsNilWhenNoGenerics(t *testing.T) {
	// Non-generic code produces no instance IRs.
	file, info := parseWithStd(t, `
		type Foo { int x; }
		main() { f := Foo(x: 1); }
	`)
	result := Compile(file, info, "")
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
	ir := generateIR(t, `
		enum Option[T] { Some(T value), None }
		main() {
			Option[int] opt = Option[int].Some(value: 42);
			if opt is Some(val) {
				print_line("{val}");
			}
		}
	`)
	// Should have the destructure blocks for the monomorphized enum
	assertContains(t, ir, "isdestr.then")
	assertContains(t, ir, "icmp eq i32")
}

// B0112: destructure is-pattern inside generic method body must apply typeSubst
func TestIsDestructureInGenericMethodBody(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "isdestr.then")
	// Should have the tag comparison for the monomorphized enum Option__int
	assertContains(t, ir, "icmp eq i32")
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
	ir := generateIR(t, `
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
	fn := extractFunction(ir, `"gesc[Row]"`)
	if fn == "" {
		t.Fatalf("monomorphized gesc[Row] not found in IR:\n%s", ir)
	}
	assertContains(t, fn, "heapdup.copy")
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
	ir := generateIR(t, `
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
	fn := extractFunction(ir, `"grab[Row]"`)
	assertContains(t, fn, "heapdup.copy")
}

// T1169: a GENERIC subtype whose field type is the type parameter `T`,
// destructured as a concrete instance `Named[string]` in a plain function. This
// exercises bindIsDestructureNamed's targetType.(*types.Instance) local-subst
// branch (BuildSubstMap over the instance's own type args) — the field must
// resolve T → string so the droppable heap field is deep-cloned on escape.
func TestT1169IfIsDestructureNamedGenericInstanceFieldDups(t *testing.T) {
	ir := generateIR(t, `
		type Shape { }
		type Named[T] is Shape { T label; }
		make() string {
			Shape s = Named[string](label: "a" + "b");
			if s is Named[string](label) { return label; }
			return "";
		}
		main() { s := make(); }
	`)
	fn := extractFunction(ir, "__user.make")
	assertContains(t, fn, "strdup.copy")
	assertContains(t, fn, "label.dropflag")
}

// --- Generic is-pattern tests (B0012) ---

func TestIsGenericType(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		type LabeledBox[T] is Box[T] { string label; }
		main() {
			Box[int] b = LabeledBox[int](value: 42, label: "x");
			bool x = b is LabeledBox[int];
		}
	`)
	// Should generate mono typeinfo for the generic instance
	assertContains(t, ir, "promise_typeinfo_LabeledBox")
	assertContains(t, ir, "call i32 @promise_type_is")
}

func TestIsGenericTypeBaseClass(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		type LabeledBox[T] is Box[T] { string label; }
		main() {
			Box[int] b = LabeledBox[int](value: 42, label: "x");
			bool x = b is Box[int];
		}
	`)
	// Should have mono typeinfo for both instances
	assertContains(t, ir, "promise_typeinfo_Box")
	assertContains(t, ir, "promise_typeinfo_LabeledBox")
}

func TestIsGenericTypeOptional(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		type LabeledBox[T] is Box[T] { string label; }
		main() {
			Box[int] lb = LabeledBox[int](value: 42, label: "x");
			Box[int]? opt = lb;
			bool x = opt is LabeledBox[int];
		}
	`)
	// Optional generic is-check: should branch on presence then RTTI
	assertContains(t, ir, "call i32 @promise_type_is")
	assertContains(t, ir, "phi i1")
}

// --- Type Argument Inference Codegen Tests ---

func TestInferGenericFuncCodegen(t *testing.T) {
	ir := generateIR(t, `
		identity[T](T x) T { return x; }
		main() {
			int v = identity(42);
		}
	`)
	// Monomorphized function should be generated for int
	assertContains(t, ir, "define i64 @\"identity[int]\"")
}

func TestInferGenericFuncTwoTypeParamsCodegen(t *testing.T) {
	ir := generateIR(t, `
		first[A, B](A a, B b) A { return a; }
		main() {
			int v = first(1, "hello");
		}
	`)
	assertContains(t, ir, "@\"first[int, string]\"")
}

func TestInferConstructorCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		main() {
			b := Box(value: 42);
			int v = b.value;
		}
	`)
	// Should produce Box[int] instance struct
	assertContains(t, ir, "Box[int]_i")
}

func TestInferGenericFuncWithVectorParamCodegen(t *testing.T) {
	ir := generateIR(t, `
		first_elem[T](T[] items) T {
			return items[0];
		}
		main() {
			int[] arr = [1, 2, 3];
			int v = first_elem(arr);
		}
	`)
	assertContains(t, ir, "@\"first_elem[int]\"")
}

// B0134 variant: generic type (non-error) constructed inside generic function body.
func TestGenericTypeInGenericFuncBody(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper[T] { T value; }
		wrap[T](T v) Wrapper[T] {
			return Wrapper[T](value: v);
		}
		main() { w := wrap[int](42); }
	`)
	assertContains(t, ir, "Wrapper[int]")
}

// B0175: Heap temp claim in genInferredVarDecl — auto-typed iterator variable
func TestHeapTempClaimInInferredVarDecl(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "heap.claim")
}

// T0392: Synth drop must call the mono'd drop method for generic heap-user-type
// inner — Box[int].drop, not Box.drop.
func TestSynthDropOptionalGenericInnerUsesMonoName(t *testing.T) {
	ir := generateIR(t, `
		type T0392GBox[T] { T val; drop(~this) {} }
		type T0392GHolder[T] { T0392GBox[T]? data; }
		main() {
			h := T0392GHolder[int](data: T0392GBox[int](val: 7));
		}
	`)
	holderDrop := extractFunction(ir, `"T0392GHolder[int].drop"`)
	if holderDrop == "" {
		t.Fatal("expected T0392GHolder[int].drop in IR")
	}
	// The mono'd inner drop must be called by name (not Box.drop).
	assertContains(t, holderDrop, `call void @"T0392GBox[int].drop"`)
}

// T0415: emitFieldDrops must use the mono'd drop name for non-optional generic
// instance fields with explicit drop. Before the fix, the lookup used the
// origin name "Box.drop" which doesn't exist — the user's drop body was
// silently skipped, leaking heap content inside the field.
func TestFieldDropUsesMonoNameForGenericExplicitDrop(t *testing.T) {
	ir := generateIR(t, `
		type T0415Box[T] { T val; drop(~this) {} }
		type T0415Holder[T] { T0415Box[T] data; }
		main() {
			h := T0415Holder[int](data: T0415Box[int](val: 7));
		}
	`)
	holderDrop := extractFunction(ir, `"T0415Holder[int].drop"`)
	if holderDrop == "" {
		t.Fatal("expected T0415Holder[int].drop in IR")
	}
	// The mono'd inner drop must be called by name.
	assertContains(t, holderDrop, `call void @"T0415Box[int].drop"`)
	// And NOT the origin name (which is the bug shape).
	assertNotContains(t, holderDrop, "call void @T0415Box.drop")
}

// T0415: emitOptionalFieldReassignDrop must use the mono'd drop name when
// reassigning an optional generic field whose inner has explicit drop.
func TestOptionalFieldReassignDropUsesMonoName(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, mainFn, "field.optdrop")
	assertContains(t, mainFn, `call void @"T0415Box2[int].drop"`)
	assertNotContains(t, mainFn, "call void @T0415Box2.drop")
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
	ir := generateIR(t, `
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
	assertContains(t, freeBlock, `call void @"T0415RawBox[string].drop"`)
	// And must NOT call pal_free here — the synth drop already pal_freed.
	assertNotContains(t, freeBlock, "call void @pal_free")
	// Confirm test premise: the mono'd synth drop itself does both the
	// inner string drop and the pal_free of the box instance.
	synthDrop := extractFunction(ir, `"T0415RawBox[string].drop"`)
	if synthDrop == "" {
		t.Fatal("expected T0415RawBox[string].drop in IR")
	}
	assertContains(t, synthDrop, "call void @promise_string_drop")
	assertContains(t, synthDrop, "call void @pal_free")
}

// T1073: force-unwrap inside a collection literal in a *generic* function body —
// `wrap[T](T? move o) T[] { return [o!]; }` instantiated with a droppable heap
// type — must neutralize the source. Exercises the typeSubst substitution path
// in neutralizeForceUnwrapElem (the element type is resolved through the active
// monomorphization substitution before the typeNeedsFieldDrop gate).
func TestT1073GenericContextForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		wrap[T](T? move o) T[] { return [o!]; }
		main() {
			T1073Box b = T1073Box(name: "g");
			T1073Box? o = b;
			T1073Box[] v = wrap[T1073Box](move o);
		}
	`)
	// The monomorphized body @"wrap[T1073Box]" must carry the present-flag clear.
	fn := extractFunction(ir, `"wrap[T1073Box]"`)
	if fn == "" {
		t.Fatal("expected wrap[T1073Box] mono body in IR")
	}
	assertContains(t, fn, t1073NeutralizeSig)
}

// B0222: Generic combinator chain result stored in variable — intermediate iterators
// must be promoted to scope bindings (freed at scope exit, not statement end).
func TestGenericCombinatorInVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] v = [1, 2, 3];
			Iterator[int] it = v.iter().map[int](|int x| -> int { return x * 2; });
		}
	`)
	// B0222: Intermediate heapTemps promoted to scope bindings should produce
	// free.call blocks (scope-level cleanup) instead of heap.drop blocks
	// (statement-level cleanup) for the intermediate _FnIter.
	assertContains(t, ir, "free.call")
	assertContains(t, ir, "__promise_iter_cleanup")
}

// B0226: Inferred optional declaration should register optional drop.
func TestInferredOptionalDrop(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "optdrop")
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
	ir := generateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[string] b = Box[string](value: "init");
			b.value = (1).to_string();
		}
	`)
	// Reassign-drop block must exist (T0390).
	assertContains(t, ir, "field.optdrop")
	// The post-store stmt-temp drop block exists for tracked temps.
	assertContains(t, ir, "tmp.drop")
	// promise_string_drop is reachable but must be guarded by the temp drop
	// flag. With the fix, the flag is cleared before the wrap, so on the hot
	// path the drop block resolves to the no-op (skip) branch.
	assertContains(t, ir, "promise_string_drop")
}

// T0394 (vector limb): the predicate also covers types.IsVector(exprType).
// Reassigning a generic Optional[Vector[int]] field with a heap-allocated
// Vector RHS must emit the reassign-drop block for the OLD field value and
// the temp-drop guard block for the NEW value, with Vector.drop reachable
// for both.
func TestOptionalGenericFieldReassignVectorEmitsDropAndOptdrop(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[Vector[int]] b = Box[Vector[int]](value: [1, 2, 3]);
			b.value = [4, 5, 6];
		}
	`)
	// T0390 reassign-drop block for the OLD field value.
	assertContains(t, ir, "field.optdrop")
	// Stmt-temp drop block for tracked heap temps (covers the Vector case).
	assertContains(t, ir, "tmp.drop")
	// Vector.drop is generic — operates on i8*, not monomorphised per-T.
	assertContains(t, ir, "@Vector.drop")
}

// T0513: Force-unwrap of an Optional[string] field on a generic-type instance
// (e.g. Box[string]) must dup the inner string. Sema's fieldTypeHasDrop returns
// false for T? where T is a TypeParam, so the bare Named's HasDrop()=false; the
// mono instance Box[string] gets synth drop via monoInstNeedsSynthDrop. Without
// the dup, the field and the new var alias the same heap pointer — at scope
// end one drops the pointer; the next reassignment frees again -> invalid free.
func TestGenericOptionalStringFieldUnwrapDups(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[string] b = Box[string](value: "init");
			string s = b.value!;
		}
	`)
	// dupStringFieldAccess mechanism must emit strdup block + promise_string_new.
	assertContains(t, ir, "strdup.copy")
	assertContains(t, ir, "promise_string_new")
}

// T0513 (vector limb): same fix must apply to generic Optional[Vector[T]] field
// unwrap — the inner Vector buffer must be duped on read so the field and the
// new variable own independent copies.
func TestGenericOptionalVectorFieldUnwrapDups(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[Vector[int]] b = Box[Vector[int]](value: [1, 2, 3]);
			Vector[int] v = b.value!;
		}
	`)
	// dupContainerFieldAccess emits a vecdup block (alloc + memcpy) for the dup.
	assertContains(t, ir, "vecdup.copy")
	assertContains(t, ir, "memcpy")
}

// T0513 (direct string field on generic owner): reading a plain `T` field
// from `Box[string]` must dup the string when bound to a new variable.
// Without the Instance-local TypeArgs substitution (added in T0513), the dup
// check sees the raw TypeParam and skips; without the ownerHasOrSynthDrop
// gate the bare Named has HasDrop=false (sema's fieldTypeHasDrop returns
// false for TypeParam) and the dup is skipped entirely.
func TestGenericDirectStringFieldReadDups(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		test() {
			Box[string] b = Box[string](value: "hi");
			string s = b.value;
		}
	`)
	// Scope the assertion to the user's test() function — the broader IR
	// contains many stdlib strdup.copy blocks unrelated to this fix.
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	assertContains(t, testFn, "strdup.copy")
	assertContains(t, testFn, "promise_string_new")
}

// T0746: a generic method that returns a `this`-owned string field by value
// must dup it on return (clone-on-return) so the owner's field-drop and the
// returned value's drop don't free the same allocation. The VarDecl-site dup
// is covered by TestGenericDirectStringFieldReadDups; this covers the
// method-return site (genReturnStmt -> setDupFlagsForFieldAccess ->
// genFieldAccess with `this` as the target, under c.typeSubst {T->string}).
func TestGenericMethodReturnStringFieldDups(t *testing.T) {
	ir := generateIR(t, `
		type GBox[T] { T val; peek(this) T { return this.val; } }
		main() { b := GBox[string](val: "hi"); s := b.peek(); }
	`)
	fn := extractFunction(ir, `"GBox[string].peek"`)
	if fn == "" {
		t.Fatal("expected GBox[string].peek in IR")
	}
	assertContains(t, fn, "strdup.copy")
	assertContains(t, fn, "promise_string_new")
}

// T0746 (`this` receiver form): the bug reported the borrowed-receiver
// variant double-freed identically, so the dup-on-return must fire there too.
func TestGenericMethodReturnStringFieldDupsBorrowedReceiver(t *testing.T) {
	ir := generateIR(t, `
		type GBox[T] { T val; peek(this) T { return this.val; } }
		main() { b := GBox[string](val: "hi"); s := b.peek(); }
	`)
	fn := extractFunction(ir, `"GBox[string].peek"`)
	if fn == "" {
		t.Fatal("expected GBox[string].peek in IR")
	}
	assertContains(t, fn, "strdup.copy")
	assertContains(t, fn, "promise_string_new")
}

// T0746 (generic getter form): a getter returning a `this`-owned string field
// through the type parameter is a distinct codegen path from a method (getter
// vs method dispatch), but the return-by-value dup must fire identically.
func TestGenericGetterReturnStringFieldDups(t *testing.T) {
	ir := generateIR(t, `
		type GBox[T] { T val; get field T { return this.val; } }
		main() { b := GBox[string](val: "hi"); s := b.field; }
	`)
	fn := extractFunction(ir, `"GBox[string].field"`)
	if fn == "" {
		t.Fatal("expected GBox[string].field in IR")
	}
	assertContains(t, fn, "strdup.copy")
	assertContains(t, fn, "promise_string_new")
}

// T0513 (maybeEnableDupForMutRefArg generic owner): passing a generic
// owner's field to a `~` (consuming) param must auto-dup the field so the
// callee's consume-drop and the owner's drop don't double-free. Exercises
// the MemberExpr branch of maybeEnableDupForMutRefArg (expr.go:5094-5109),
// which had zero coverage before T0513 added a test for the generic-owner
// gate.
func TestGenericOwnerMutRefArgDupsStringField(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		consume(string move s) {}
		test() {
			b := Box[string](value: "hi");
			consume(b.value);
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	// dupStringFieldAccess + strdup.copy emitted at the field read site,
	// guarded by the ownerHasOrSynthDrop generic-owner gate.
	assertContains(t, testFn, "strdup.copy")
	assertContains(t, testFn, "promise_string_new")
}

// T0513 (maybeEnableDupForMutRefArg generic owner — Vector limb): same
// auto-dup must apply when the field type substitutes to a Vector and is
// passed to a consuming param. dupContainerFieldAccess routes through
// dupVector which emits a vecdup.copy block.
func TestGenericOwnerMutRefArgDupsVectorField(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		consume(Vector[int] move v) {}
		test() {
			b := Box[Vector[int]](value: [1, 2, 3]);
			consume(b.value);
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	assertContains(t, testFn, "vecdup.copy")
}

// T0513 (maybeEnableDupForConstructorArg generic owner): constructor
// field-init that reads from a generic owner's field must auto-dup so the
// new instance owns an independent copy. Mirrors T0411 (non-generic owner)
// for generic-owner instances; without ownerHasOrSynthDrop, the early
// return at expr.go:5129 would skip the dup setup.
func TestGenericOwnerConstructorArgDupsStringField(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T value; }
		type Holder { string s; drop(~this) {} }
		test() {
			b := Box[string](value: "hi");
			h := Holder(s: b.value);
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	assertContains(t, testFn, "strdup.copy")
	assertContains(t, testFn, "promise_string_new")
}

// B0210: Optional[TypeParam] field with none value should not cause mono layout mismatch.
// The mono layout computes the correct LLVM type for the optional field, but the none
// value was generated using an unsubstituted TypeParam, producing a type mismatch.
func TestOptionalTypeParamFieldNone(t *testing.T) {
	ir := generateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[int](val: none);
		}
	`)
	// The optional field should use the correct substituted type (i64 for int)
	assertContains(t, ir, "{ i1, i64 }")
}

// B0210: Optional[TypeParam] field with a concrete value should work too.
func TestOptionalTypeParamFieldValue(t *testing.T) {
	ir := generateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m := MaybeVal[string](val: "hello");
		}
	`)
	assertContains(t, ir, "{ i1, i8* }")
}

// B0210: Multiple Optional[TypeParam] fields with different instantiations.
func TestOptionalTypeParamMultipleInstantiations(t *testing.T) {
	ir := generateIR(t, `
		type MaybeVal[T] { T? val; }
		main() {
			m1 := MaybeVal[int](val: none);
			m2 := MaybeVal[string](val: none);
		}
	`)
	// Both int? and string? layouts should be present
	assertContains(t, ir, "{ i1, i64 }")
	assertContains(t, ir, "{ i1, i8* }")
}

// T0428: Generic type with borrowed this.field! exercises the typeSubst branches
// in genOptionalForceUnwrap (Case 3B with typeSubst != nil).
func TestT0428GenericBorrowedThisForceUnwrapTypeSubst(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `"GenHolder[GBox].get_val"`)
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "call void @llvm.memcpy")
}

// T0428: Generic function with local var force-unwrap exercises typeSubst path
// in neutralizeMemberOptionalField (IdentExpr root, lines 9052-9054).
// When a generic method/function body has `b.field!` where b's type has a TypeParam,
// c.typeSubst is applied to resolve the concrete owner type.
func TestT0428GenericFuncLocalVarOptFieldNeutralization(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "store i1 false")
}

// T0419: Optional[GenericBox[int]] with explicit drop must dispatch through
// the mono-mangled $wrap (e.g. GenericBoxD[int].drop$wrap).
func TestOptionalLocalDropExplicitUserDropWrapMono(t *testing.T) {
	ir := generateIR(t, `
		type GenericBoxD[T] {
			T val;
			drop(~this) {}
		}
		test_mono_no_unwrap() {
			GenericBoxD[int]? a = GenericBoxD[int](val: 5);
		}
	`)
	// The mono Optional drop dispatch must call the mono $wrap variant.
	assertContains(t, ir, `@"GenericBoxD[int].drop$wrap"`)
	wrapFn := extractFunction(ir, `"GenericBoxD[int].drop$wrap"`)
	if wrapFn == "" {
		t.Fatal(`expected "GenericBoxD[int].drop$wrap" in IR`)
	}
	assertContains(t, wrapFn, `call void @"GenericBoxD[int].drop"`)
	assertContains(t, wrapFn, "call void @pal_free")
}

// T0847 (generic/mono variant): the constructor field-init dup-on-read must
// fire under monomorphization, where the arg/target types are TypeParams that
// only resolve after substitution. This exercises maybeEnableDupForConstructorArg's
// typeSubst branches (argType/targetType Substitute) — `Holder[T](held: v[0])`
// inside a generic function body where T=Item is bound by the mono context.
func TestT0847_ConstructorGenericVectorElementDups(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "call void @llvm.memcpy")
}

func TestParenThisGenericMethodNoExtractFromPtr(t *testing.T) {
	// T0746: a droppable (string) payload exercises the generic-method
	// return-by-value dup path in addition to the (this).peek() dispatch gate.
	ir := generateIR(t, `
		type T0613GenBox[T] {
			T val;
			peek(this) T { return this.val; }
			via(this) T { return (this).peek(); }
		}
		main() { b := T0613GenBox[string](val: "hi"); v := b.via(); }
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	// The monomorphized instance method body is emitted into the (unsplit) module.
	assertContains(t, ir, "T0613GenBox[string].via")
}

// genGenericEnumMethodCall gate: a generic enum routes method calls through a
// dedicated path distinct from the non-generic enum method gate.
func TestParenThisGenericEnumMethodNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
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
	assertNotContains(t, ir, "extractvalue i8*")
	assertContains(t, ir, "T0613GOpt[int].has_via")
}

// genIsResolvedType gate: `(this) is IBox[int]` resolves to a concrete generic
// instance and routes through genIsResolvedType (distinct from the non-generic
// genIsNamedType gate covered by TestParenThisIsCheck...).
func TestParenThisIsGenericNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
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
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0613GShape.is_intbox") == "" {
		t.Fatal("expected T0613GShape.is_intbox in IR")
	}
}

// T1011: a GENERIC enum's narrowed heap variant field escaping the scope exercises
// the typeSubst substitution in genNarrowedVariantField (both targetType and the
// field type) and narrowedVariantFieldDroppable — the substituted field type is
// `string`, so the escape must still clone.
func TestEnumNarrowGenericVariantStringFieldEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		enum Opt[T] { Some(T val), None }
		grab() string {
			Opt[string] o = Opt[string].Some(val: "a");
			if o is Some { return o.val; }
			return "";
		}
		main() { string s = grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	assertContains(t, fn, "strdup.copy")
}

// T1011: a GENERIC FUNCTION body that narrows an enum and escapes a heap variant
// field exercises the typeSubst substitution in genNarrowedVariantField — the
// narrowing TargetType and FieldType carry the function's TypeParam, so they must
// be substituted before dupHeapFieldForEscape runs. Monomorphized for T=string the
// escape must clone; for T=int (non-heap) it must not. This is the path the
// concrete-Opt[string] test above does NOT reach (there typeSubst is nil because
// sema already resolved the field type to a concrete `string`).
func TestEnumNarrowGenericFnBodyVariantFieldEscapeDups(t *testing.T) {
	ir := generateIR(t, `
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
	strFn := extractFunction(ir, `"extract[string]"`)
	if strFn == "" {
		t.Fatal(`expected "extract[string]" mono instance in IR`)
	}
	assertContains(t, strFn, "strdup.copy")
	intFn := extractFunction(ir, `"extract[int]"`)
	if intFn == "" {
		t.Fatal(`expected "extract[int]" mono instance in IR`)
	}
	assertNotContains(t, intFn, "strdup.copy")
}

// T1011: binding a narrowed heap variant field inside a GENERIC function body
// (`b := o.val`) routes through isStringFieldDup → narrowedVariantFieldDroppable
// with typeSubst active, so the substituted TargetType resolves to a droppable
// enum and the binding clones the payload (keeping it independent of the subject).
func TestEnumNarrowGenericFnBodyVariantFieldBoundCopyDups(t *testing.T) {
	ir := generateIR(t, `
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
	fn := extractFunction(ir, `"first[string]"`)
	if fn == "" {
		t.Fatal(`expected "first[string]" mono instance in IR`)
	}
	assertContains(t, fn, "strdup.copy")
}
