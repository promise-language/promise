package sema

import "testing"

// T2184: a method that takes over the vtable slot of an inherited CONCRETE
// parent method must satisfy that method's signature. validateAbstractOverrides
// only ever examined ABSTRACT requirements, so a parent method with a body was
// never compared against the child that redeclared it — `tag(this) int` could be
// overridden by `tag(this) string` and the parent's call site read the string
// instance pointer as an integer, silently, with `promise check` reporting ok.
func TestConcreteOverrideSignatureMismatch(t *testing.T) {
	// The reported repro: an incompatible return type.
	errs := checkErrs(t, `
		type P { new(~this) {} tag(this) int { return 1; } }
		type R is P { new(~this) {} tag(this) string { return "x"; } }`)
	expectError(t, errs, "cannot override inherited method 'tag' from P")
	expectError(t, errs, "expected tag() -> int, found tag() -> string")

	// The second repro: a child RE-TIGHTENING a slot its parent had legally
	// relaxed. R must be measured against Q (void), not against P's failable
	// close! that Q already relaxed — measuring against P would accept this.
	errs = checkErrs(t, `
		type P { new(~this) {} close!(~this) {} }
		type Q is P { new(~this) {} close(~this) {} }
		type R is Q { new(~this) {} close!(~this) {} }`)
	expectError(t, errs, "cannot override inherited method 'close' from Q")
	expectError(t, errs, "a failable method close!() -> void cannot override a non-failable inherited method close() -> void")

	// The receiver's borrow kind does not relax under an explicit `is` (T1952).
	errs = checkErrs(t, `
		type P { int n; ping(this) {} }
		type R is P { ping(~this) {} }`)
	expectError(t, errs, "cannot override inherited method 'ping' from P")
	expectError(t, errs, "takes a this receiver but R.ping takes ~this")

	// A parameter type is part of the slot's shape too.
	errs = checkErrs(t, `
		type P { int n; tag(this, int a) int { return a; } }
		type R is P { tag(this, string a) int { return 1; } }`)
	expectError(t, errs, "expected tag(int) -> int, found tag(string) -> int")

	// Getters occupy their own slot and are checked the same way.
	errs = checkErrs(t, `
		type P { int n; get label string { return "p"; } }
		type R is P { get label int { return 3; } }`)
	expectError(t, errs, "cannot override inherited method 'label' from P")
	expectError(t, errs, "expected label() -> string, found label() -> int")

	// A CONCRETE default method of a generic `structural interface, overridden
	// across a module boundary — exercises the parent substitution (Iterator's
	// T -> int) on the concrete-requirement path.
	errs = checkErrs(t, `
		type Src is Iterator[int] {
			int n;
			next(~this) int? { if this.n <= 0 { return none; } this.n = this.n - 1; return this.n; }
			count(~this) string { return "nope"; }
		}`)
	expectError(t, errs, "cannot override inherited method 'count' from Iterator")
	expectError(t, errs, "expected count() -> int, found count() -> string")
}

// TestConcreteOverrideCannotBeLaunderedThroughAnAbstractRedeclaration pins the
// hole an earlier revision of this check left open: resolving the overriding
// declaration through PARENTS (own-then-inherited) let an intermediate type
// re-declare a concrete parent's slot as `abstract` with a different signature,
// and neither walk compared it against anything. The abstract walk saw only the
// intermediate's abstract `tag` — which the grandchild satisfies exactly — while
// the concrete walk skipped the intermediate as "not an override". The result
// was the reported T2184 miscompile spelled in three types instead of two:
// `P p = S(...); p.tag()` printed the string instance pointer as an integer.
//
// The intermediate is where the drift enters, so that is where it is reported.
func TestConcreteOverrideCannotBeLaunderedThroughAnAbstractRedeclaration(t *testing.T) {
	errs := checkErrs(t, `
		type P { int n; tag(this) int { return 1; } }
		type R is P { tag(this) string `+"`abstract;"+` }
		type S is R { tag(this) string { return "x"; } }`)
	expectError(t, errs, "type R cannot override inherited method 'tag' from P")
	expectError(t, errs, "expected tag() -> int, found tag() -> string")

	// An abstract redeclaration that keeps the parent's signature is the
	// legitimate use — forcing grandchildren to supply their own body — and must
	// still be accepted.
	checkOK(t, `
		type P { int n; tag(this) int { return 1; } }
		type R is P { tag(this) int `+"`abstract;"+` }
		type S is R { tag(this) int { return 2; } }`)
}

// TestConcreteOverrideAcrossGetterAndMethodSlots pins that a getter and a plain
// method of the same name share ONE vtable slot — methodSlotKey discriminates
// setters and unary operators, not getters — so `get tag string` really does take
// over an inherited `tag() int`, and a parent-typed call site reads the string
// instance pointer as an integer. The diagnostic must anchor at the getter, not
// fall back to the type declaration: the two declarations differ in kind, so
// matching the AST declaration against the REQUIREMENT's kind finds nothing.
func TestConcreteOverrideAcrossGetterAndMethodSlots(t *testing.T) {
	errs := checkErrs(t, `
		type P { int n; tag(this) int { return 1; } }
		type R is P { get tag string { return "x"; } }`)
	expectError(t, errs, "type R cannot override inherited method 'tag' from P")
	expectError(t, errs, "expected tag() -> int, found tag() -> string")
}

// TestConcreteOverrideKeepsSlotKindsApart pins the discriminators methodSlotKey
// applies. Both walks key off the slot, so a suffix that stopped being appended
// would silently start comparing one declaration against an unrelated one — and
// the resulting diagnostic would name a method the author never touched.
func TestConcreteOverrideKeepsSlotKindsApart(t *testing.T) {
	// `$set`: a getter and a setter of the same name are separate slots, so
	// overriding only the setter must not be measured against the getter (whose
	// return type differs by construction — a setter returns void).
	checkOK(t, `
		type P { int n; get label string { return "p"; } set label(string v) { } }
		type R is P { set label(string v) { } }`)

	// ...and the setter slot is still checked on its own terms.
	errs := checkErrs(t, `
		type P { int n; get label string { return "p"; } set label(string v) { } }
		type R is P { set label(int v) { } }`)
	expectError(t, errs, "type R cannot override inherited method 'label' from P")
	expectError(t, errs, "expected label(string) -> void, found label(int) -> void")
	// The getter slot is untouched and must not be dragged in.
	expectNoErrorContaining(t, errs, "label() -> string")

	// `$unary`: a child declaring BOTH variants must have each measured against
	// the parent's matching one. Dropping the suffix would pair the child's
	// unary `-` with the parent's binary one and report a correct declaration.
	checkOK(t, `
		type Vec {
			int v;
			-(Vec o) Vec { return Vec(v: this.v - o.v); }
			-() Vec { return Vec(v: -this.v); }
		}
		type Vec2 is Vec {
			-(Vec o) Vec { return Vec(v: 0); }
			-() Vec { return Vec(v: 1); }
		}`)

	// And a wrong UNARY override is caught without the binary one masking it.
	errs = checkErrs(t, `
		type Vec {
			int v;
			-(Vec o) Vec { return Vec(v: this.v - o.v); }
			-() Vec { return Vec(v: -this.v); }
		}
		type Vec2 is Vec { -() int { return 1; } }`)
	expectError(t, errs, "expected -() -> Vec, found -() -> int")
}

// TestConcreteOverrideOnAValueTypeDoesNotDoubleReport pins the boundary with
// meta.go's valueTypeOverride, which rejects a value child that redeclares an
// inherited method with an IDENTICAL signature (value types dispatch statically,
// so an override could never take effect). The two rules partition cleanly:
// identical signatures are its business, differing ones are this pass's, and
// neither declaration should draw both diagnostics.
func TestConcreteOverrideOnAValueTypeDoesNotDoubleReport(t *testing.T) {
	// Differing signature — this pass reports, valueTypeOverride stays quiet.
	errs := checkErrs(t, `
		type P { int x `+"`value"+`; tag(this) int { return this.x; } }
		type R is P { tag(this) string { return "x"; } }`)
	expectError(t, errs, "type R cannot override inherited method 'tag' from P")
	expectNoErrorContaining(t, errs, "value types dispatch statically")

	// Identical signature — valueTypeOverride reports, this pass stays quiet.
	errs = checkErrs(t, `
		type P { int x `+"`value"+`; tag(this) int { return this.x; } }
		type R is P { tag(this) int { return 2; } }`)
	expectError(t, errs, "value types dispatch statically")
	expectNoErrorContaining(t, errs, "cannot override inherited method")
}

// TestConcreteOverrideUnderMultipleInheritance pins that a child is measured
// against EVERY parent that declares the slot, not just the first. Each parent
// crossing reads the slot through its own view vtable, so satisfying one parent
// is not licence to break another's call sites — which is why
// InheritedSlotDeclarations gives each direct parent its own `seen` map.
func TestConcreteOverrideUnderMultipleInheritance(t *testing.T) {
	errs := checkErrs(t, `
		type A { int n; step(this) int { return 1; } }
		type B `+"`structural"+` { step(this) string { return "b"; } }
		type C is A, B { step(this) int { return 2; } }`)
	expectError(t, errs, "type C cannot override inherited method 'step' from B")
	expectError(t, errs, "expected step() -> string, found step() -> int")
	// A matches exactly, so only B is reported.
	expectNoErrorContaining(t, errs, "'step' from A")
}

// TestConcreteOverrideRelaxationBoundaries pins the far side of each relaxation.
// A rule that says "T satisfies T?" is only half a rule; the half that does the
// work is that the reverse does not. Each case here is one documented relaxation
// driven backwards, and each has a distinct LLVM return or parameter shape, so
// accepting it would be the reported miscompile in a different costume.
func TestConcreteOverrideRelaxationBoundaries(t *testing.T) {
	// `T` for `T?` is legal; `T?` for `T` is not — the caller reads a bare int
	// out of a slot the override filled with an optional.
	errs := checkErrs(t, `
		type P { int n; peek(this) int { return 1; } }
		type R is P { peek(this) int? { return none; } }`)
	expectError(t, errs, "expected peek() -> int, found peek() -> int?")

	// Extra parameters are legal only when the adapter can synthesize them.
	// A required extra has no value to supply.
	errs = checkErrs(t, `
		type P { int n; emit(this, int a) { } }
		type R is P { emit(this, int a, int b) { } }`)
	expectError(t, errs, "expected emit(int) -> void, found emit(int, int) -> void")

	// Dropping a parameter the parent's call sites still pass is never legal.
	errs = checkErrs(t, `
		type P { int n; emit(this, int a) { } }
		type R is P { emit(this) { } }`)
	expectError(t, errs, "expected emit(int) -> void, found emit() -> void")

	// A void override of a value-returning parent leaves the caller reading a
	// result the callee never wrote.
	errs = checkErrs(t, `
		type P { int n; tag(this) int { return 1; } }
		type R is P { tag(this) { } }`)
	expectError(t, errs, "expected tag() -> int, found tag() -> void")

	// And the reverse: a value-returning override of a void parent changes the
	// slot's LLVM function type.
	errs = checkErrs(t, `
		type P { int n; ping(this) { } }
		type R is P { ping(this) int { return 1; } }`)
	expectError(t, errs, "expected ping() -> void, found ping() -> int")
}

// TestConcreteOverrideCovariantReturnIsNotABlanketEscape pins that
// SatisfiesInherited's covariant-return arm is gated on the SUBTYPE relation and
// nothing wider. The arm exists so `Sub.self() Sub` may override
// `Base.self() Base`; it must not become a hole through which any return type
// walks. subtypeWidens — not AssignableTo — is what draws that line: it admits
// exactly the widenings codegen's view-box path can realize, and excludes the
// reference decay and optional wrapping whose representations differ from the
// slot's. Swapping in AssignableTo would reopen the reported bug from this side.
func TestConcreteOverrideCovariantReturnIsNotABlanketEscape(t *testing.T) {
	// An unrelated Named return is not covariant, however much it looks like
	// the `Sub for Base` case structurally.
	errs := checkErrs(t, `
		type Base { int v; self(this) Base { return Base(v: 1); } }
		type Other { int w; }
		type Sub is Base { self(this) Other { return Other(w: 1); } }`)
	expectError(t, errs, "type Sub cannot override inherited method 'self' from Base")
	expectError(t, errs, "expected self() -> Base, found self() -> Other")

	// Covariance runs child-for-parent, never parent-for-child: returning the
	// PARENT where the slot promises the CHILD is a widening the caller cannot
	// absorb.
	errs = checkErrs(t, `
		type Base { int v; }
		type Mid is Base { self(this) Mid { return Mid(v: 1); } }
		type Leaf is Mid { self(this) Base { return Base(v: 1); } }`)
	expectError(t, errs, "type Leaf cannot override inherited method 'self' from Mid")
	expectError(t, errs, "expected self() -> Mid, found self() -> Base")

	// A borrow of the right type is not the right type: `Base&` and `Base` are
	// different representations, and AssignableTo would have accepted this.
	errs = checkErrs(t, `
		type Base { int v; self(this) Base { return Base(v: 1); } }
		type Sub is Base { self(this) Base & { return this; } }`)
	expectError(t, errs, "type Sub cannot override inherited method 'self' from Base")
}

// TestConcreteOverrideSkipsWhatDoesNotShareASlot pins the three
// inheritedSlotIsOverridable arms whose skip is not observable through a
// diagnostic, so nothing else would notice if one stopped firing.
func TestConcreteOverrideSkipsWhatDoesNotShareASlot(t *testing.T) {
	// A GENERIC override shares the parent's slot key but cannot be compared:
	// the relaxed comparator matches method-level TypeParams by pointer
	// identity, so even a correct generic override would be reported. Same
	// carve-out the abstract walk has made since T1376.
	checkOK(t, `
		type P { int n; wrap(this, int x) int { return x; } }
		type R is P { wrap[T](this, T x) int { return 1; } }`)

	// A NATIVE parent method is excluded from the vtable (AllVirtualMethods),
	// so nothing dispatches through its slot and a mismatching child cannot
	// produce the reported miscompile. Such a program cannot be built at all —
	// calling it panics codegen with `undeclared method P.blip` (T2222,
	// pre-existing and unrelated to inheritance) — so the skip costs nothing.
	checkOK(t, `
		type P { int n; blip(this) int `+"`native;"+` }
		type R is P { blip(this) string { return "x"; } }`)

	// A receiver-less member (`+"`"+`factory / `+"`"+`global / `+"`"+`mono) is a static call on the
	// type name, with no instance to load a vtable from (T1749).
	checkOK(t, `
		type P { int n; make(int v) P `+"`factory"+` { return P(n: v); } }
		type R is P { make(int v, int w) R `+"`factory"+` { return R(n: v + w); } }`)
}

// TestOverrideDiagnosticAnchorsAtTheTypeWhenNotDeclaredThere pins overridePos's
// fallback arm. The abstract walk resolves an implementation through parents, so
// a type can be reported for an override it does not itself declare — here R
// inherits P's `+"`"+`probe`+"`"+` and that is what fails Iface's requirement. There is no
// MethodDecl in R to point at, so the diagnostic must land on the type.
func TestOverrideDiagnosticAnchorsAtTheTypeWhenNotDeclaredThere(t *testing.T) {
	errs := checkErrs(t, `
		type Iface { probe() int `+"`abstract;"+` }
		type P { int n; probe(this) string { return "p"; } }
		type R is P, Iface { }`)
	expectError(t, errs, "type R cannot satisfy abstract method 'probe' from Iface")
	expectError(t, errs, "expected probe() -> int, found probe() -> string")
}

// TestConcreteOverrideSignatureMatch pins what the new check must NOT reject.
// Every relaxation the language grants an abstract requirement is equally legal
// over a concrete parent method, and three kinds of declaration are not
// overrides at all despite sharing a name with the parent's.
func TestConcreteOverrideSignatureMatch(t *testing.T) {
	// The four documented relaxations, over concrete parent methods.
	checkOK(t, `
		type P { int mark; close!(~this) { this.mark = 1; } }
		type Q is P { close(~this) { this.mark = 2; } }`)
	checkOK(t, `
		type Src { peek(~this) int? { return none; } }
		type Fixed is Src { peek(~this) int { return 7; } }`)
	checkOK(t, `
		type Emit { int last; send(~this, int n) { this.last = n; } }
		type Wide is Emit { send(~this, int n, int flags = 7) { this.last = n + flags; } }`)

	// Covariant return under NOMINAL inheritance: the child's method returns the
	// child type where the parent's returns the parent type. This is the shape
	// `clone` takes on every `clone child (tests/e2e/clone_inherited_generic_test.pr),
	// and SatisfiesAbstract alone rejects it — it admits a covariant return only
	// for a structural interface.
	checkOK(t, `
		type Base { int v; self(this) Base { return Base(v: this.v); } }
		type Sub is Base { self(this) Sub { return Sub(v: this.v); } }`)
	checkOK(t, `
		type GBase[T] { T v; self(this) GBase[T] { return GBase[T](v: this.v); } }
		type GSub[T] is GBase[T] { self(this) GSub[T] { return GSub[T](v: this.v); } }
		main() { GSub[int] s = GSub[int](v: 1); }`)

	// A type may declare both a unary and a binary `-`; they occupy separate
	// slots (T0883). Resolving the inherited binary `-` by NAME would find the
	// child's unary one and report a mismatch on a child that declares nothing.
	checkOK(t, `
		type Vec {
			int v;
			-(Vec o) Vec { return Vec(v: this.v - o.v); }
			-() Vec { return Vec(v: -this.v); }
		}
		type Vec2 is Vec {}`)

	// Constructors are per-type — validateConstructors owns their rules — so a
	// child's new() need not match its parent's arity.
	checkOK(t, `
		type Base { int a; new(~this, int a) { this.a = a; } }
		type Child is Base { new(~this, int a, int b) { this.a = a + b; } }`)

	// A child's _validate! ADDS to its parent's rather than overriding it, so
	// the two never share a slot (T1752).
	checkOK(t, `
		type Base { int a; _validate!(this) { if this.a < 0 { raise error(message: "neg"); } } }
		type Child is Base { _validate!(this) { if this.a > 9 { raise error(message: "big"); } } }`)

	// An abstract requirement still reports through the abstract walk only —
	// the two passes must not both fire on the same declaration.
	errs := checkErrs(t, `
		type Iface { work() int `+"`abstract;"+` }
		type Impl is Iface { work() string { return "x"; } }`)
	expectNoErrorContaining(t, errs, "cannot override inherited method")
	expectError(t, errs, "cannot satisfy abstract method 'work'")
}
