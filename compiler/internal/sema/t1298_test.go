package sema

import "testing"

// T1298: a concrete type satisfying a `structural interface (or a child of a
// class) can be implicitly widened to Optional-of-that-interface/parent (`S?`),
// matching the already-supported bare-interface widening (`S = Concrete(...)`).
// Previously sema's AssignableTo Rule 2 only looked through an Optional target
// via Identical/self-instance, never applying the subtype/structural widening to
// the element — so `Sink? a = Counter(...)` was rejected. These tests lock the
// fix and its deliberate narrowing (ref decay into an optional stays rejected;
// codegen's optional view-box path does not box a borrow).

// The exact repro: value + heap concrete widened to `Sink?` in local-decl and
// constructor-field positions now type-checks.
func TestT1298StructuralWidenToOptionalTypeChecks(t *testing.T) {
	checkOK(t, `
		type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
		type Counter { int fd `+"`"+`value; emit(this, int x) int { return this.fd + x; } }
		type Heavy { string tag; emit(this, int x) int { return x; } }
		type OptHolder { Sink? s; }
		main() {
			Sink? a = Counter(fd: 5);
			Sink? b = Heavy(tag: "h");
			OptHolder h = OptHolder(s: Counter(fd: 7));
		}
	`)
}

// Return, call-argument, none-then-assign, and Vector-push positions all widen.
func TestT1298StructuralWidenToOptionalAllPositions(t *testing.T) {
	checkOK(t, `
		type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
		type Counter { int fd `+"`"+`value; emit(this, int x) int { return this.fd + x; } }
		make() Sink? { return Counter(fd: 1); }
		use(Sink? s) int { return s!.emit(2); }
		main() {
			Sink? b = none;
			b = Counter(fd: 3);
			use(Counter(fd: 4));
			Sink?[] v = [];
			v.push(Counter(fd: 5));
		}
	`)
}

// The class-inheritance sibling of the same gap: `Animal? a = Dog()`.
func TestT1298InheritanceWidenToOptionalTypeChecks(t *testing.T) {
	checkOK(t, `
		type Animal { speak(this) int `+"`"+`abstract; }
		type Dog is Animal { speak(this) int { return 1; } }
		type Zoo { Animal? pet; }
		make() Animal? { return Dog(); }
		main() {
			Animal? a = Dog();
			Zoo z = Zoo(pet: Dog());
		}
	`)
}

// Non-structural interface without `structural still requires explicit `is —
// the Optional target must not silently unlock non-structural widening.
func TestT1298NonStructuralOptionalStillRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Sink { emit(this, int x) int `+"`"+`abstract; }
		type Counter { int fd `+"`"+`value; emit(this, int x) int { return this.fd + x; } }
		main() {
			Sink? a = Counter(fd: 5);
		}
	`)
	expectError(t, errs, "cannot")
}
