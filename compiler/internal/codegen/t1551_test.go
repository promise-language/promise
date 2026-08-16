package codegen

import "testing"

// T1551: string interpolation of a subtype that inherits format!() from its
// parent must dispatch to the type that DECLARES format(), not to a
// <Child>.format function that was never emitted.

func TestT1551InterpolationOfHeapChildCallsParentFormat(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "@Base.format(")
	assertNotContains(t, ir, "@Kid.format(")
}

func TestT1551InterpolationOfGenericParentChildUsesMonoName(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `call { i1, i8* } @"Box[int].format"(`)
	assertNotContains(t, ir, "@IntBox.format(")
}

// A parent that explicitly declares `is Format` and implements format() still
// owns the implementation — findStructuralOwner must not walk past it to the
// structural Format interface (whose format() is abstract, so nothing would
// ever be synthesized for the child).
func TestT1551InterpolationOfFormatInterfaceChildCallsParentFormat(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "@Base.format(")
	assertNotContains(t, ir, "@Kid.format(")
}

// Same shape, reached through an explicit method call rather than
// interpolation — the genMethodCall path hit the identical findStructuralOwner
// defect before T1551.
func TestT1551ExplicitCallOnFormatInterfaceChildCallsParentFormat(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "@Base.format(")
	assertNotContains(t, ir, "@Kid.format(")
}

// A genuine structural DEFAULT (interface method with a body) is still
// synthesized per-concrete, so the child keeps its own mangled name.
func TestT1551StructuralDefaultFormatStillSynthesizedPerConcrete(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "@Kid.format(")
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
	ir := generateIR(t, t1551OrderedParent+`
		main() {
			a := K(n: 1);
			b := K(n: 2);
			bool e = a == b;
			P s = a + b;
		}
	`)
	assertContains(t, ir, `@"P.=="(`)
	assertContains(t, ir, `@"P.+"(`)
	assertNotContains(t, ir, `@"K.=="(`)
	assertNotContains(t, ir, `@"K.+"(`)
}

func TestT1551InheritedCompoundOpCallsParentOperator(t *testing.T) {
	ir := generateIR(t, t1551OrderedParent+`
		main() {
			K a = K(n: 1);
			a += K(n: 2);
		}
	`)
	assertContains(t, ir, `@"P.+"(`)
	assertNotContains(t, ir, `@"K.+"(`)
}

func TestT1551InheritedUnaryOpCallsParentOperator(t *testing.T) {
	ir := generateIR(t, t1551OrderedParent+`
		main() {
			a := K(n: 5);
			P u = -a;
		}
	`)
	assertContains(t, ir, "@P.-$unary(")
	assertNotContains(t, ir, "@K.-$unary(")
}

// The mirror case: an operator the parent does NOT declare (`!=`, `>`) really
// is a structural default with a body, so it must still be synthesized under
// the concrete child's name. The T1551 guard only short-circuits on a parent
// that declares the method itself.
func TestT1551StructuralOperatorDefaultStillSynthesizedPerConcrete(t *testing.T) {
	ir := generateIR(t, t1551OrderedParent+`
		main() {
			a := K(n: 1);
			b := K(n: 2);
			bool x = a != b;
			bool y = a > b;
		}
	`)
	assertContains(t, ir, `@"K.!="(`)
	assertContains(t, ir, `@"K.>"(`)
}

// A plain method inherited from a generic parent instance must mangle to the
// parent's MONOMORPHIZED name — resolveDirectDispatchOwner's third branch
// (resolveMonoParentName). The operator equivalent of this case cannot be
// written today: sema rejects `IntBox == IntBox` when `==(Self)` is inherited
// from a generic parent (T1555), so the operator path is covered above with a
// non-generic parent only.
func TestT1551InheritedMethodFromGenericParentUsesMonoName(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `@"Box[int].label"(`)
	assertContains(t, ir, `@"Box[string].label"(`)
	assertNotContains(t, ir, "@IntBox.label(")
	assertNotContains(t, ir, `@"Wrap[string].label"(`)
}

// Both findStructuralOwner outcomes through one plain method call: `speak` is
// the parent's implementation of a structural ABSTRACT method (dispatch to
// Animal.speak — Dog.speak is never emitted), while `loud` is a structural
// DEFAULT with a body and is still synthesized per-concrete as Dog.loud.
func TestT1551InheritedImplementationVersusStructuralDefault(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "@Animal.speak(")
	assertContains(t, ir, "@Dog.loud(")
	assertNotContains(t, ir, "@Dog.speak(")
}

// The silent-miscompile variant: the structural method has a BODY, so the
// pre-fix walk did not panic — it synthesized Kid.greet from the interface
// default and quietly shadowed the parent's override. The parent declares
// greet() itself, so dispatch must land on Base.greet.
func TestT1551ParentOverrideOfStructuralDefaultWins(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "@Base.greet(")
	assertNotContains(t, ir, "@Kid.greet(")
}

// Operator form of the same shape: the parent overrides Ordered's `!=`/`>`
// defaults, so the child must reach the parent's bodies rather than a
// per-concrete synthesis of the interface defaults.
func TestT1551ParentOverrideOfStructuralOperatorDefaultWins(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `@"Base.!="(`)
	assertContains(t, ir, `@"Base.>"(`)
	assertNotContains(t, ir, `@"Kid.!="(`)
	assertNotContains(t, ir, `@"Kid.>"(`)
}

// Both post-fix branches at once: the guard must stop at the generic parent
// that declares format(), and resolveMonoParentName must then mangle the
// MONOMORPHIZED parent name — Box[int].format, never Box.format or IntBox.format.
func TestT1551GenericFormatInterfaceParentUsesMonoName(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, `call { i1, i8* } @"Box[int].format"(`)
	assertContains(t, ir, `call { i1, i8* } @"Box[string].format"(`)
	assertNotContains(t, ir, "@IntBox.format(")
	assertNotContains(t, ir, `@"Wrap[string].format"(`)
}

// An override at the MIDDLE of the chain owns the implementation for everything
// below it — resolveMethodOwner must stop at Mid, not walk on to Root.
func TestT1551MiddleLevelOverrideOwnsGrandchildDispatch(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "@Mid.format(")
	assertContains(t, ir, "@Mid.tag(")
	assertNotContains(t, ir, "@Grand.format(")
	assertNotContains(t, ir, "@Grand.tag(")
	assertNotContains(t, ir, "call void @Root.format(")
}
