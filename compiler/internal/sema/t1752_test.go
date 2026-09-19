package sema

import "testing"

// T1752 — `_validate!`, a type invariant every construction path must satisfy
// (docs/language-design.md §5.7 → Validation).

const t1752Port = "type Port {\n" +
	"  int value `final;\n" +
	"  _validate!(this) { if this.value < 1 { raise error(\"bad\"); } }\n" +
	"}\n"

// --- declaration shape -------------------------------------------------------

func TestT1752ValidateMethodShapeIsEnforced(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			name: "mutable receiver",
			src:  "type P { int v; _validate!(~this) {} }",
			want: "must take a shared 'this' receiver, not '~this'",
		},
		{
			name: "not failable",
			src:  "type P { int v; _validate(this) {} }",
			want: "must be failable — write '_validate!'",
		},
		{
			name: "takes parameters",
			src:  "type P { int v; _validate!(this, int x) {} }",
			want: "must take no parameters",
		},
		{
			name: "declares a return type",
			src:  "type P { int v; _validate!(this) bool { return true; } }",
			want: "must not declare a return type",
		},
		{
			name: "receiver-less (factory)",
			src:  "type P { int v; _validate!() Self `factory { return Self(v: 1); } }",
			want: "must be an instance method taking a shared 'this' receiver",
		},
		{
			name: "abstract",
			src:  "type P `abstract { int v; _validate!(this) `abstract; }",
			want: "must not be abstract",
		},
		{
			name: "on a structural interface",
			src:  "type P `structural { _validate!(this) {} }",
			want: "must not declare _validate!",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectError(t, checkErrs(t, tc.src+"\nmain() {}"), tc.want)
		})
	}
}

// The name is reserved for the invariant. A getter or setter of that name
// would shadow a real _validate! at every `x._validate` — and OwnValidateMethod
// is method-only, so nothing else would notice.
func TestT1752ValidateNameIsReserved(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			name: "getter on a type",
			src:  "type P { int v; get _validate int { return 1; } }",
			want: "a getter may not take that name",
		},
		{
			name: "setter on a type",
			src:  "type P { int v; set _validate(int x) { this.v = x; } }",
			want: "a setter may not take that name",
		},
		{
			name: "getter alongside a real invariant",
			src: "type P {\n" +
				"  int v;\n" +
				"  _validate!(this) { if this.v < 0 { raise error(\"x\"); } }\n" +
				"  get _validate int { return 1; }\n" +
				"}",
			want: "is reserved for the type's invariant",
		},
		{
			name: "getter on an enum",
			src:  "enum E { A, B, get _validate int { return 1; } }",
			want: "a getter may not take that name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectError(t, checkErrs(t, tc.src+"\nmain() {}"), tc.want)
		})
	}
}

// An invariant is Promise code by construction — it reads the type's own fields
// and raises. There is nothing for a runtime implementation to provide.
func TestT1752NativeValidateIsRejected(t *testing.T) {
	expectError(t, checkErrs(t, "type P { int v; _validate!(this) `native; }\nmain() {}"),
		"must not be native")
}

func TestT1752WellFormedValidateIsAccepted(t *testing.T) {
	checkOK(t, t1752Port+"main() { p := Port(value: 80)?!; }")
}

// --- every construction path on a validated type is failable -----------------

func TestT1752NewOnAValidatedTypeMustBeFailable(t *testing.T) {
	errs := checkErrs(t, "type P {\n"+
		"  int v `final;\n"+
		"  _validate!(this) { if this.v < 0 { raise error(\"bad\"); } }\n"+
		"  new(~this, int v) { this.v = v; }\n"+
		"}\nmain() {}")
	expectError(t, errs, "new() on P must be failable — write 'new!'")
	expectError(t, errs, "P declares _validate!")
}

func TestT1752FactoryOnAValidatedTypeMustBeFailable(t *testing.T) {
	errs := checkErrs(t, "type P {\n"+
		"  int v `final;\n"+
		"  _validate!(this) { if this.v < 0 { raise error(\"bad\"); } }\n"+
		"  make(int v) Self `factory { return Self(v: v); }\n"+
		"}\nmain() {}")
	expectError(t, errs, "factory 'make' on P must be failable — write 'make!'")
}

// The rule follows inheritance: a child inherits its parent's invariant, so the
// child's own construction paths must be failable too, and the diagnostic must
// name where the invariant came from.
func TestT1752InheritedValidateMakesTheChildsNewFailable(t *testing.T) {
	errs := checkErrs(t, "type B { int a `final; _validate!(this) { if this.a < 0 { raise error(\"a\"); } } }\n"+
		"type D is B { int b `final; new(~this, int a, int b) { super(a: a); this.b = b; } }\n"+
		"main() {}")
	expectError(t, errs, "D inherits _validate! from B")
}

// A factory whose body cannot itself fail STILL needs `!` — the value it yields
// may be rejected, and a signature that hid that would lie to its caller (§5.7).
func TestT1752NonFailableFactoryBodyStillNeedsTheMarker(t *testing.T) {
	errs := checkErrs(t, "type Color {\n"+
		"  int r `final;\n"+
		"  _validate!(this) { if this.r < 0 { raise error(\"r\"); } }\n"+
		"  red() Self `factory { return Self(r: 255); }\n"+
		"}\nmain() {}")
	expectError(t, errs, "it can fail to yield a value even when its own body cannot fail")
}

// --- construction call sites -------------------------------------------------

func TestT1752ConstructionCallSiteMustHandleTheFailure(t *testing.T) {
	expectError(t, checkErrs(t, t1752Port+"main() { p := Port(value: 80); }"),
		"failable call must be handled")
}

func TestT1752ConstructionCallSiteAcceptsEachErrorOperator(t *testing.T) {
	checkOK(t, t1752Port+"main() { p := Port(value: 80)?!; }")
	checkOK(t, t1752Port+"mk!() Port { return Port(value: 80)?^; }\nmain() {}")
	checkOK(t, t1752Port+"mk!() Port { return Port(value: 80); }\nmain() {}")
	checkOK(t, t1752Port+"main() { p := Port(value: 80) ? e { Port(value: 1)?! }; }")
}

// --- a Self built inside its own factory must leave by `return` --------------

const t1752Escaper = "type Port {\n" +
	"  int value `final;\n" +
	"  _validate!(this) { if this.value < 1 { raise error(\"bad\"); } }\n"

func TestT1752FactoryConstructedSelfMustLeaveByReturn(t *testing.T) {
	// Capturing one in a closure is already rejected upstream ("cannot capture
	// non-copy variable 'c' without move"), so it never reaches this rule.
	cases := []struct{ name, body string }{
		{"passed as an argument", "c := Self(value: v); sink(c); return c;"},
		{"pushed into a container", "c := Self(value: v); Port[] all = []; all.push(move c); return Self(value: v);"},
		{"rebound to another local", "c := Self(value: v); Port other = c; return other;"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := t1752Escaper +
				"  make!(int v) Self `factory { " + tc.body + " }\n" +
				"}\nsink(Port p) {}\nmain() {}"
			expectError(t, checkErrs(t, src), "must leave by 'return'")
		})
	}
}

// The rule is scoped to the deferred local. An unrelated local in the same
// factory, and a READ of the deferred one's fields, are both fine.
func TestT1752FactoryEscapeRuleDoesNotOverreach(t *testing.T) {
	checkOK(t, t1752Escaper+
		"  make!(int v) Self `factory {\n"+
		"    c := Self(value: 0);\n"+
		"    int doubled = c.value * 2;\n"+
		"    c.value = v + doubled;\n"+
		"    return c;\n"+
		"  }\n"+
		"}\nmain() { p := Port.make(80)?!; }")
}

// Deferral is what the `final-fixup window needs, and ONLY a local binding
// opens one. A construction not bound to a local — each arm of a branching
// return, say — has no window, so it must stay validated in place rather than
// be deferred to a `return` that cannot name it. Getting this wrong skips the
// invariant silently, which is why it is pinned here as well as in
// tests/e2e/validate_invariant_test.pr.
func TestT1752UnboundFactoryConstructionIsNotDeferred(t *testing.T) {
	src := t1752Escaper +
		"  pick!(bool hi) Self `factory { return if hi { Self(value: 443) } else { Self(value: -1) }; }\n" +
		"}\nmain() { p := Port.pick(true)?!; }"
	info := checkOK(t, src)
	wraps := 0
	for _, kind := range info.ValidateSites {
		if kind == ValidateWrap {
			wraps++
		}
	}
	// Both arms plus the call site's own handling: each arm's Self(…) is a wrap
	// site. Neither may be deferred.
	if wraps != 2 {
		t.Fatalf("expected both branching-return arms to be wrap sites, got %d", wraps)
	}
	for _, kind := range info.ValidateSites {
		if kind == ValidatePropagate {
			t.Fatal("an unbound construction must not be deferred to the factory's return")
		}
	}
}

// The `final-fixup shape §5.7 documents stays legal: reading and writing the
// local's fields does not let the instance escape.
func TestT1752FactoryFixupShapeIsAccepted(t *testing.T) {
	checkOK(t, t1752Escaper+
		"  make!(int v) Self `factory { c := Self(value: 0); c.value = v; return c; }\n"+
		"}\nmain() { p := Port.make(80)?!; }")
}

// An error operator on a deferred binding is rejected. It cannot mean what it
// says — the chain has not run yet — and honouring it instead would validate
// BEFORE the `final fixup, leaving the fixed value unvalidated. Without the
// rejection, codegen panicked ("nil value for inferred var decl"): sema accepted
// the operator against the pre-downgrade marking, then removed it.
func TestT1752ErrorOperatorOnADeferredBindingIsRejected(t *testing.T) {
	for _, op := range []string{"?!", "?^"} {
		t.Run(op, func(t *testing.T) {
			src := t1752Escaper +
				"  make!(int v) Self `factory { c := Self(value: 0)" + op + "; c.value = v; return c; }\n" +
				"}\nmain() {}"
			expectError(t, checkErrs(t, src), "remove the error operator")
		})
	}
	// The typed-binding form goes through the other tracking site.
	src := t1752Escaper +
		"  make!(int v) Self `factory { Port c = Self(value: 0)?!; c.value = v; return c; }\n" +
		"}\nmain() {}"
	expectError(t, checkErrs(t, src), "remove the error operator")
}

// The rejection is scoped to the factory's OWN Self. A DIFFERENT validated type
// bound to a local in the same factory is not a deferral candidate, so its
// error operator is required, not rejected.
func TestT1752ErrorOperatorOnAnotherValidatedTypeInAFactoryIsFine(t *testing.T) {
	checkOK(t, "type Inner {\n"+
		"  int n `final;\n"+
		"  _validate!(this) { if this.n < 0 { raise error(\"inner\"); } }\n"+
		"}\n"+
		"type Outer {\n"+
		"  int n `final;\n"+
		"  _validate!(this) { if this.n < 0 { raise error(\"outer\"); } }\n"+
		"  make!(int v) Self `factory {\n"+
		"    Inner probe = Inner(n: v)?!;\n"+
		"    c := Self(n: 0);\n"+
		"    c.n = probe.n;\n"+
		"    return c;\n"+
		"  }\n"+
		"}\nmain() {}")
}

// --- clone (§5.7: a clone copies an already-valid original) ------------------

// The exemption is scoped to the clone's OWN type. Building some OTHER
// validated value inside a clone body still validates it — otherwise a clone
// became a blanket opt-out for every invariant in reach.
func TestT1752CloneExemptionIsScopedToItsOwnType(t *testing.T) {
	errs := checkErrs(t, "type Inner {\n"+
		"  int n `final;\n"+
		"  _validate!(this) { if this.n < 0 { raise error(\"inner\"); } }\n"+
		"}\n"+
		"type Holder {\n"+
		"  int n `final;\n"+
		"  _validate!(this) { if this.n < 0 { raise error(\"holder\"); } }\n"+
		"  clone() Self { Inner probe = Inner(n: this.n); return Self(n: this.n); }\n"+
		"}\nmain() {}")
	expectError(t, errs, "failable call must be handled")
}

// The enum half of the same rule. Enums reach the exemption through a separate
// marking path, so it is pinned separately.
func TestT1752EnumCloneExemptionIsScopedToItsOwnEnum(t *testing.T) {
	src := "enum Inner {\n" +
		"  Value(int n),\n" +
		"  None,\n" +
		"  _validate!(this) { raise error(\"inner\"); }\n" +
		"}\n" +
		"enum Outer {\n" +
		"  Wrapped(int n),\n" +
		"  Empty,\n" +
		"  clone() Outer {\n" +
		"    Inner probe = Inner.Value(1);\n" +
		"    return match this { Outer.Wrapped(n) => Outer.Wrapped(n), Outer.Empty => Outer.Empty, };\n" +
		"  }\n" +
		"}\nmain() {}"
	expectError(t, checkErrs(t, src), "failable call must be handled")
}

// …and an enum's OWN variants stay exempt inside its clone.
func TestT1752EnumCloneExemptsItsOwnVariants(t *testing.T) {
	checkOK(t, "enum Reading {\n"+
		"  Value(int n),\n"+
		"  None,\n"+
		"  _validate!(this) { match this { Reading.Value(n) => { if n < 0 { raise error(\"neg\"); } }, _ => {}, } }\n"+
		"  clone() Reading {\n"+
		"    return match this { Reading.Value(n) => Reading.Value(n), Reading.None => Reading.None, };\n"+
		"  }\n"+
		"}\nmain() { r := Reading.Value(1)?!; c := r.clone(); }")
}

func TestT1752CloneBodyIsExemptFromValidation(t *testing.T) {
	// A hand-written clone must stay non-failable so a validated type can still
	// satisfy `Cloneable`, whose clone() Self carries no `!`.
	checkOK(t, "type Doc {\n"+
		"  string title `final;\n"+
		"  int pages `final;\n"+
		"  _validate!(this) { if this.pages < 1 { raise error(\"no pages\"); } }\n"+
		"  clone() Self { return Self(title: this.title.clone(), pages: this.pages); }\n"+
		"}\nmain() { d := Doc(title: \"t\", pages: 1)?!; c := d.clone(); }")
}

func TestT1752SynthesizedCloneIsExemptFromValidation(t *testing.T) {
	checkOK(t, "type Doc `clone {\n"+
		"  string title `final;\n"+
		"  int pages `final;\n"+
		"  _validate!(this) { if this.pages < 1 { raise error(\"no pages\"); } }\n"+
		"}\nmain() { d := Doc(title: \"t\", pages: 1)?!; c := d.clone(); }")
}

// --- enums -------------------------------------------------------------------

func TestT1752EnumPayloadVariantConstructionIsFailable(t *testing.T) {
	src := "enum Temp {\n" +
		"  Kelvin(int degrees),\n" +
		"  Unspecified,\n" +
		"  _validate!(this) { raise error(\"always\"); }\n" +
		"}\n"
	expectError(t, checkErrs(t, src+"main() { t := Temp.Kelvin(300); }"),
		"failable call must be handled")
	// A fieldless variant is a plain enumerated value with no state to
	// validate, and is not a construction expression at all.
	checkOK(t, src+"main() { t := Temp.Unspecified; }")
}
