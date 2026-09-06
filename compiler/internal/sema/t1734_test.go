package sema

import "testing"

// T1734 — every implementation of a published protocol interface in std and the
// catalog declares `is` explicitly. The value of the sweep is entirely in what
// the compiler does when a signature later drifts: an explicit `is` turns a
// silent loss of conformance into an error that names the exact method. These
// tests pin that behaviour on std-shaped declarations.

const t1734Tick = "`"

// --- The point of the sweep: `is` catches drift that structural matching would
// have swallowed silently. ---

func TestT1734DriftedFormatIsRejectedAtTheDeclaration(t *testing.T) {
	// std.Format requires format!(Writer ~w). A type that declares `is Format`
	// but spells format as the string-returning variant (the second spelling
	// T1734 removed from json.JsonValue) is rejected, and the error names the
	// method.
	errs := checkErrs(t, `
		type Report is Format {
			int n;
			format!(this) string { return "n"; }
		}
		main() {}
	`)
	expectError(t, errs, "abstract method 'format'")
	expectError(t, errs, "Format")
}

func TestT1734DriftedParseIsRejectedAtTheDeclaration(t *testing.T) {
	// std.Parse requires parse!(Reader ~r) Self. Parsing from a string — the
	// T1717 defect that modules/time/time.pr carried — is rejected once the type
	// declares the conformance.
	errs := checkErrs(t, `
		type Stamp is Parse {
			int _n `+t1734Tick+`value;
			parse!(string s) Stamp `+t1734Tick+`factory { return Stamp(_n: 0); }
		}
		main() {}
	`)
	expectError(t, errs, "abstract method 'parse'")
}

func TestT1734DriftedCloseIsRejectedAtTheDeclaration(t *testing.T) {
	// std.Closer requires close!(~this). A close that takes a required extra
	// argument cannot satisfy it, and the error names the method.
	//
	// Note this does NOT cover the receiver: `close(this)` against `close!(~this)`
	// is accepted today, because *Signature does not carry the receiver, so
	// nothing compares receiver ownership. That hole is T1952, filed from this
	// sweep — add a case here when it lands.
	errs := checkErrs(t, `
		type Handle is Closer {
			int fd;
			close!(~this, int mode) {}
		}
		main() {}
	`)
	expectError(t, errs, "abstract method 'close'")
}

// --- The conforming spellings the sweep moved std and the catalog onto. ---

func TestT1734ConformingFormatIsAccepted(t *testing.T) {
	checkOK(t, `
		type Report is Format {
			int n;
			format!(this, Writer ~w) { w.write_string("n"); }
		}
		main() {}
	`)
}

func TestT1734ConformingParseIsAccepted(t *testing.T) {
	// The shape modules/time/time.pr's DateTime/Date/Time now use.
	checkOK(t, `
		type Stamp is Parse {
			int _n `+t1734Tick+`value;
			parse!(Reader ~r) Stamp `+t1734Tick+`factory {
				int total = 0;
				while byte := r.read_byte() {
					total = total + 1;
				}
				return Stamp(_n: total);
			}
		}
		main() {}
	`)
}

func TestT1734ValueTypeMayDeclareSeveralProtocols(t *testing.T) {
	// The Duration / Date / Digest256 shape: a pure value type declaring the
	// full set it satisfies. Declaring `is` on a value type must not change what
	// it is allowed to contain.
	checkOK(t, `
		type Money is Ordered, Hashable, Format {
			int cents `+t1734Tick+`value;
			== (Money other) bool => this.cents == other.cents;
			< (Money other) bool => this.cents < other.cents;
			get hash int => this.cents;
			format!(Writer ~w) { w.write_string("$"); }
		}
		main() {}
	`)
}

func TestT1734RelaxedMatchIsStillAcceptedUnderExplicitIs(t *testing.T) {
	// The wide integers declare `is Parse` while their parse! carries an extra
	// defaulted `base` parameter. That is a documented relaxed match, and Parse
	// carries no default methods, so an explicit `is` accepts it.
	//
	// A non-failable close(~this) under `is Closer` is accepted by sema too, but
	// codegen then miscompiles the boxed call (T1952), which is why the sweep
	// leaves http.Client and std.MutexGuard[T] structurally conforming instead.
	checkOK(t, `
		type Word is Parse {
			int _n `+t1734Tick+`value;
			parse!(Reader ~r, int base = 16) Word `+t1734Tick+`factory { return Word(_n: base); }
		}
		main() {}
	`)
}

// --- The near-miss check still covers what `is` cannot reach. ---

func TestT1734ParseFromStringWithoutIsIsStillANearMiss(t *testing.T) {
	// Without the `is`, nothing verifies the claim — but the T1731 near-miss
	// check still flags a type that owns a protocol name it does not satisfy.
	// This is the gate that found the time.pr defect in the first place, and it
	// must keep working for types that (like enums) cannot declare `is`.
	errs := checkErrs(t, `
		type Stamp {
			int _n `+t1734Tick+`value;
			parse!(string s) Stamp `+t1734Tick+`factory { return Stamp(_n: 0); }
		}
		main() {}
	`)
	expectError(t, errs, "matching protocol Parse")
}

func TestT1734EnumSatisfyingFormatStructurallyIsAccepted(t *testing.T) {
	// json.JsonValue's situation: an enum cannot declare `is`, so it conforms to
	// Format by signature alone and the near-miss check accepts it. This is the
	// carve-out documented under "Protocol Conformance Is Declared, Not
	// Inferred" in docs/standard-library.md.
	checkOK(t, `
		enum Node {
			Leaf,
			Branch,
			format!(this, Writer ~w) { w.write_string("node"); }
		}
		main() {}
	`)
}

func TestT1734EnumDriftingFromFormatIsStillANearMiss(t *testing.T) {
	errs := checkErrs(t, `
		enum Node {
			Leaf,
			Branch,
			format!(this) string { return "node"; }
		}
		main() {}
	`)
	expectError(t, errs, "matching protocol Format")
}
