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
	errs := checkErrs(t, `
		type Handle is Closer {
			int fd;
			close!(~this, int mode) {}
		}
		main() {}
	`)
	expectError(t, errs, "abstract method 'close'")
}

func TestT1952ReceiverBorrowKindMustMatchUnderExplicitIs(t *testing.T) {
	// T1952: std.Closer requires close!(~this). A shared `this receiver is not
	// among language-design.md#variable-declarations's relaxations, so an explicit `is` rejects it — and the message
	// names the receiver, because Signature.String() does not print it and the
	// generic "expected X, found Y" arm would print two identical signatures.
	errs := checkErrs(t, `
		type Handle is Closer {
			int fd;
			close(this) {}
		}
		main() {}
	`)
	expectError(t, errs, "the requirement takes a ~this receiver but Handle.close takes this")
}

func TestT1952MutatingReceiverCannotClaimASharedRequirement(t *testing.T) {
	// The other direction of the same rule. A requirement that promises a shared
	// (read-only) borrow may not be implemented by a method that mutates — that
	// would let a caller holding a `& borrow of the view mutate through it.
	errs := checkErrs(t, `
		type Touchable `+t1734Tick+`structural {
			touch!(this) `+t1734Tick+`abstract;
		}
		type Counter is Touchable {
			int n;
			touch!(~this) { this.n = 1; }
		}
		main() {}
	`)
	expectError(t, errs, "the requirement takes a this receiver but Counter.touch takes ~this")
}

func TestT1952StructuralSatisfactionDoesNotYetCompareReceivers(t *testing.T) {
	// Pins a known GAP, not a blessing: implicit structural satisfaction compares
	// params, failability and result but never the receiver, so a `~this method
	// satisfies a shared-receiver requirement and `s.emit(1)` below mutates
	// through what language-design.md#borrowing-and-moving calls a read-only borrow. That is T2185; language-design.md#variable-declarations states the
	// end-state rule (a concrete receiver may be less demanding than the
	// requirement's, never more). This test exists so closing T2185 is a
	// deliberate, visible change here rather than a silent one — the idiomatic
	// requirement is written with no receiver at all, so the fix is a sweep that
	// spells `emit(~this, int n)` across the seven affected test units.
	checkOK(t, `
		type Sink `+t1734Tick+`structural {
			emit(int n) `+t1734Tick+`abstract;
		}
		type Counter {
			int total;
			emit(~this, int n) { this.total = this.total + n; }
		}
		main() { Sink s = Counter(total: 0); s.emit(1); }
	`)
}

func TestT1952NonFailableCloseIsStillAcceptedUnderExplicitIs(t *testing.T) {
	// The relaxation T1952 restored on http.Client and std.MutexGuard[T]: a
	// non-failable close(~this) satisfies Closer's close!(~this). Sema accepts it
	// (Closer carries no default methods, so the T1376 exactness gate does not
	// fire) and codegen now adapts it at the crossing rather than miscompiling.
	checkOK(t, `
		type Handle is Closer {
			int fd;
			close(~this) {}
		}
		main() {}
	`)
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

// --- Two properties the sweep relies on that the cases above do not isolate. ---

func TestT1734MultipleProtocolsAreCheckedIndependently(t *testing.T) {
	// Most of the sweep declares several protocols at once (`type int is
	// Ordered, Hashable, Format, Parse, Encodable, Decodable`). Each must be
	// checked on its own: the Format half here is correct and the Parse half is
	// not, so exactly the Parse method is named and Format is left alone.
	errs := checkErrs(t, `
		type Doc is Format, Parse {
			int n;
			format!(this, Writer ~w) { w.write_string("x"); }
			parse!(string s) Doc `+t1734Tick+`factory { return Doc(n: 1); }
		}
		main() {}
	`)
	expectError(t, errs, "abstract method 'parse'")
	expectNoErrorContaining(t, errs, "abstract method 'format'")
}

func TestT1734ProtocolOptOutSuppressesTheNearMiss(t *testing.T) {
	// `structural(protocol: false)` is the escape hatch for a method that owns a
	// reserved name without implementing the protocol. std.Channel's close() is
	// the worked example the sweep deliberately kept rather than fixed, so the
	// opt-out must actually silence the near-miss check.
	errs := checkErrs(t, `
		type Gate {
			int n;
			close() `+t1734Tick+`structural(protocol: false) { }
		}
		main() {}
	`)
	expectNoErrorContaining(t, errs, "matching protocol Closer")
}
