package regress8

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1551: string interpolation of a subtype that inherits format!() from its
// parent must dispatch to the type that DECLARES format(), not to a
// <Child>.format function that was never emitted.

func TestT1551InterpolationOfHeapChildCallsParentFormat(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base {
			int n;
			format!(Writer ~w) { w.write_string("B"); }
		}
		type Kid is Base {}
		main() {
			k := Kid(n: 3);
			s := "{k}";
		}
	`)
	codegentest.AssertContains(t, ir, "@Base.format(")
	codegentest.AssertNotContains(t, ir, "@Kid.format(")
}

func TestT1551InterpolationOfGenericParentChildUsesMonoName(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			T v;
			format!(Writer ~w) { w.write_string("box"); }
		}
		type IntBox is Box[int] {}
		main() {
			b := IntBox(v: 1);
			s := "{b}";
		}
	`)
	codegentest.AssertContains(t, ir, `call { i1, i8* } @"Box[int].format"(`)
	codegentest.AssertNotContains(t, ir, "@IntBox.format(")
}

// A parent that explicitly declares `is Format` and implements format() still
// owns the implementation — findStructuralOwner must not walk past it to the
// structural Format interface (whose format() is abstract, so nothing would
// ever be synthesized for the child).
func TestT1551InterpolationOfFormatInterfaceChildCallsParentFormat(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base is Format {
			int n;
			format!(Writer ~w) { w.write_string("B"); }
		}
		type Kid is Base {}
		main() {
			k := Kid(n: 3);
			s := "{k}";
		}
	`)
	codegentest.AssertContains(t, ir, "@Base.format(")
	codegentest.AssertNotContains(t, ir, "@Kid.format(")
}

// Same shape, reached through an explicit method call rather than
// interpolation — the genMethodCall path hit the identical findStructuralOwner
// defect before T1551.
func TestT1551ExplicitCallOnFormatInterfaceChildCallsParentFormat(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base is Format {
			int n;
			format!(Writer ~w) { w.write_string("B"); }
		}
		type Kid is Base {}
		main() {
			k := Kid(n: 3);
			Builder b = Builder(capacity: 8);
			k.format(b)?!;
		}
	`)
	codegentest.AssertContains(t, ir, "@Base.format(")
	codegentest.AssertNotContains(t, ir, "@Kid.format(")
}

// A genuine structural DEFAULT (interface method with a body) is still
// synthesized per-concrete, so the child keeps its own mangled name.
func TestT1551StructuralDefaultFormatStillSynthesizedPerConcrete(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Tagged `+"`structural"+` {
			format!(Writer ~w) { w.write_string("D"); }
		}
		type Base is Tagged { int n; }
		type Kid is Base {}
		main() {
			k := Kid(n: 3);
			s := "{k}";
		}
	`)
	codegentest.AssertContains(t, ir, "@Kid.format(")
}

// --- operator direct-dispatch paths -------------------------------------
//
// genBinaryExpr / genNonNativeCompoundOp / emitUnaryOpResult were folded onto
// the same resolveDirectDispatchOwner helper, so they inherit the T1551 fix.
// The pre-fix code walked past a concrete parent that declares the operator and
// landed on the structural interface it implements (Equal/Ordered), mangling
// <Child>.<op> — a function that is never emitted, because the interface's
// operator is abstract and therefore never synthesized per-concrete.

const t1551OrderedParent = `
	type P is Ordered {
		int n;
		==(Self other) bool => this.n == other.n;
		<(Self other) bool => this.n < other.n;
		+(Self other) P => P(n: this.n + other.n);
		-(this) P => P(n: 0 - this.n);
	}
	type K is P {}
`

func TestT1551InheritedBinaryOpCallsParentOperator(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1551OrderedParent+`
		main() {
			a := K(n: 1);
			b := K(n: 2);
			bool e = a == b;
			P s = a + b;
		}
	`)
	codegentest.AssertContains(t, ir, `@"P.=="(`)
	codegentest.AssertContains(t, ir, `@"P.+"(`)
	codegentest.AssertNotContains(t, ir, `@"K.=="(`)
	codegentest.AssertNotContains(t, ir, `@"K.+"(`)
}

func TestT1551InheritedCompoundOpCallsParentOperator(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1551OrderedParent+`
		main() {
			K a = K(n: 1);
			a += K(n: 2);
		}
	`)
	codegentest.AssertContains(t, ir, `@"P.+"(`)
	codegentest.AssertNotContains(t, ir, `@"K.+"(`)
}

func TestT1551InheritedUnaryOpCallsParentOperator(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1551OrderedParent+`
		main() {
			a := K(n: 5);
			P u = -a;
		}
	`)
	codegentest.AssertContains(t, ir, "@P.-$unary(")
	codegentest.AssertNotContains(t, ir, "@K.-$unary(")
}

// The mirror case: an operator the parent does NOT declare (`!=`, `>`) really
// is a structural default with a body, so it must still be synthesized under
// the concrete child's name. The T1551 guard only short-circuits on a parent
// that declares the method itself.
func TestT1551StructuralOperatorDefaultStillSynthesizedPerConcrete(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1551OrderedParent+`
		main() {
			a := K(n: 1);
			b := K(n: 2);
			bool x = a != b;
			bool y = a > b;
		}
	`)
	codegentest.AssertContains(t, ir, `@"K.!="(`)
	codegentest.AssertContains(t, ir, `@"K.>"(`)
}

// A plain method inherited from a generic parent instance must mangle to the
// parent's MONOMORPHIZED name — resolveDirectDispatchOwner's third branch
// (resolveMonoParentName). The operator equivalent of this case cannot be
// written today: sema rejects `IntBox == IntBox` when `==(Self)` is inherited
// from a generic parent (T1555), so the operator path is covered above with a
// non-generic parent only.
func TestT1551InheritedMethodFromGenericParentUsesMonoName(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] {
			T v;
			label(this) string { return "box"; }
		}
		type IntBox is Box[int] {}
		type Wrap[T] is Box[T] {}
		main() {
			b := IntBox(v: 1);
			string s = b.label();
			w := Wrap[string](v: "s");
			string t2 = w.label();
		}
	`)
	codegentest.AssertContains(t, ir, `@"Box[int].label"(`)
	codegentest.AssertContains(t, ir, `@"Box[string].label"(`)
	codegentest.AssertNotContains(t, ir, "@IntBox.label(")
	codegentest.AssertNotContains(t, ir, `@"Wrap[string].label"(`)
}

// Both findStructuralOwner outcomes through one plain method call: `speak` is
// the parent's implementation of a structural ABSTRACT method (dispatch to
// Animal.speak — Dog.speak is never emitted), while `loud` is a structural
// DEFAULT with a body and is still synthesized per-concrete as Dog.loud.
func TestT1551InheritedImplementationVersusStructuralDefault(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Speaker `+"`structural"+` {
			speak(this) string `+"`abstract"+`;
			loud(this) string => this.speak() + "!";
		}
		type Animal is Speaker {
			int age;
			speak(this) string { return "woof"; }
		}
		type Dog is Animal {}
		main() {
			d := Dog(age: 1);
			string a = d.speak();
			string b = d.loud();
		}
	`)
	codegentest.AssertContains(t, ir, "@Animal.speak(")
	codegentest.AssertContains(t, ir, "@Dog.loud(")
	codegentest.AssertNotContains(t, ir, "@Dog.speak(")
}

// The silent-miscompile variant: the structural method has a BODY, so the
// pre-fix walk did not panic — it synthesized Kid.greet from the interface
// default and quietly shadowed the parent's override. The parent declares
// greet() itself, so dispatch must land on Base.greet.
func TestT1551ParentOverrideOfStructuralDefaultWins(t *testing.T) {
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
			k := Kid(n: 1);
			string s = k.greet();
		}
	`)
	codegentest.AssertContains(t, ir, "@Base.greet(")
	codegentest.AssertNotContains(t, ir, "@Kid.greet(")
}

// Operator form of the same shape: the parent overrides Ordered's `!=`/`>`
// defaults, so the child must reach the parent's bodies rather than a
// per-concrete synthesis of the interface defaults.
func TestT1551ParentOverrideOfStructuralOperatorDefaultWins(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base is Ordered {
			int n;
			==(Self other) bool => this.n == other.n;
			<(Self other) bool => this.n < other.n;
			!=(Self other) bool => false;
			>(Self other) bool => false;
		}
		type Kid is Base {}
		main() {
			a := Kid(n: 1);
			b := Kid(n: 2);
			bool x = a != b;
			bool y = a > b;
		}
	`)
	codegentest.AssertContains(t, ir, `@"Base.!="(`)
	codegentest.AssertContains(t, ir, `@"Base.>"(`)
	codegentest.AssertNotContains(t, ir, `@"Kid.!="(`)
	codegentest.AssertNotContains(t, ir, `@"Kid.>"(`)
}

// Both post-fix branches at once: the guard must stop at the generic parent
// that declares format(), and resolveMonoParentName must then mangle the
// MONOMORPHIZED parent name — Box[int].format, never Box.format or IntBox.format.
func TestT1551GenericFormatInterfaceParentUsesMonoName(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] is Format {
			T v;
			format!(Writer ~w) { w.write_string("box"); }
		}
		type IntBox is Box[int] {}
		type Wrap[T] is Box[T] {}
		main() {
			b := IntBox(v: 1);
			s := "{b}";
			w := Wrap[string](v: "q");
			s2 := "{w}";
		}
	`)
	codegentest.AssertContains(t, ir, `call { i1, i8* } @"Box[int].format"(`)
	codegentest.AssertContains(t, ir, `call { i1, i8* } @"Box[string].format"(`)
	codegentest.AssertNotContains(t, ir, "@IntBox.format(")
	codegentest.AssertNotContains(t, ir, `@"Wrap[string].format"(`)
}

// An override at the MIDDLE of the chain owns the implementation for everything
// below it — resolveMethodOwner must stop at Mid, not walk on to Root.
func TestT1551MiddleLevelOverrideOwnsGrandchildDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Root {
			int n;
			format!(Writer ~w) { w.write_string("R"); }
			tag(this) string => "r";
		}
		type Mid is Root {
			format!(Writer ~w) { w.write_string("M"); }
			tag(this) string => "m";
		}
		type Grand is Mid {}
		main() {
			g := Grand(n: 1);
			s := "{g}";
			string t2 = g.tag();
		}
	`)
	codegentest.AssertContains(t, ir, "@Mid.format(")
	codegentest.AssertContains(t, ir, "@Mid.tag(")
	codegentest.AssertNotContains(t, ir, "@Grand.format(")
	codegentest.AssertNotContains(t, ir, "@Grand.tag(")
	codegentest.AssertNotContains(t, ir, "call void @Root.format(")
}
