package codegen

import (
	"regexp"
	"testing"
)

// T0496: `if cond { opt_field } else { none }` (and the match equivalent) used
// as a block-result expression produced a malformed phi: the field-read arm had
// the Optional struct type `{ i1, T }` but the `none` arm lowered to the bare
// `i1 0` void-optional fallback, so the two phi incomings had mismatched types
// and `opt` rejected the IR with a "constant expression type mismatch".
//
// The fix propagates the if/match result type as the contextual target type for
// each arm (c.targetType) so a bare `none` arm lowers to a zeroinitializer of
// the shared Optional result type via genNoneLit, matching the other arm.
//
// extractFunc (not extractFunction): the user main lowers to @.goroutine.main,
// whose first textual occurrence is the call site in the C @main wrapper;
// extractFunc skips call sites and walks to the `define`.

// TestT0496_IfExprNoneArmMatchesOptionalPhi — the `none` arm of an if-expression
// whose other arm yields `int?` must lower to a `{ i1, i64 } zeroinitializer`,
// and the merge phi must be typed `{ i1, i64 }` (both incomings agree).
func TestT0496_IfExprNoneArmMatchesOptionalPhi(t *testing.T) {
	ir := generateIR(t, `main() {
		flag := true;
		int? x = 5;
		vopt := if flag { x } else { none };
	}`)
	body := extractFunc(ir, ".goroutine.main")
	if body == "" {
		t.Fatalf("expected define @.goroutine.main in IR:\n%s", ir)
	}
	// The merge phi is typed as the Optional struct, with both incomings agreeing.
	phiRe := regexp.MustCompile(`phi \{ i1, i64 \} \[ [^]]+ \], \[ zeroinitializer, %if\.else`)
	if !phiRe.MatchString(body) {
		t.Errorf("expected an Optional-typed `phi { i1, i64 }` with a zeroinitializer none arm:\n%s", body)
	}
	// Pre-fix failure shape: a bare `i1 0` (or `false`) incoming from the else
	// arm into a `{ i1, i64 }` phi. Must not appear.
	if regexp.MustCompile(`phi \{ i1, i64 \}.*\[ (false|i1 0|0), %if\.else`).MatchString(body) {
		t.Errorf("none arm lowered to the bare i1 void fallback (pre-T0496 phi mismatch):\n%s", body)
	}
}

// TestT0496_MatchNoneArmMatchesOptionalPhi — the match equivalent: a `_ => none`
// arm whose sibling arm yields `int?` must lower to the Optional zero value, not
// the i1 fallback.
func TestT0496_MatchNoneArmMatchesOptionalPhi(t *testing.T) {
	ir := generateIR(t, `main() {
		flag := true;
		int? x = 5;
		vopt := match flag { true => x, _ => none };
	}`)
	body := extractFunc(ir, ".goroutine.main")
	if body == "" {
		t.Fatalf("expected define @.goroutine.main in IR:\n%s", ir)
	}
	// The match merge phi is typed as the Optional struct, with the `_ => none`
	// arm contributing a `zeroinitializer` incoming (folded directly into the
	// phi rather than a separate store).
	phiRe := regexp.MustCompile(`phi \{ i1, i64 \} \[[^]]+\], \[ zeroinitializer, %match`)
	if !phiRe.MatchString(body) {
		t.Errorf("expected an Optional-typed `phi { i1, i64 }` with a zeroinitializer none arm:\n%s", body)
	}
	// Pre-fix failure shape: a bare `i1 0`/`false` incoming into a `{ i1, i64 }`
	// phi from the none arm. Must not appear.
	if regexp.MustCompile(`phi \{ i1, i64 \}.*\[ (false|i1 0|0), %match`).MatchString(body) {
		t.Errorf("none arm lowered to the bare i1 void fallback (pre-T0496 phi mismatch):\n%s", body)
	}
}

// TestT0496_EnumMatchNoneArmMatchesOptionalPhi — a match on a real enum subject
// goes through genEnumMatch (the bool matches above are genValueMatch). A `_ =>
// none` arm whose sibling reads an `int?` field must still lower to the Optional
// zero value, exercising the matchResultType propagation in genEnumMatch.
func TestT0496_EnumMatchNoneArmMatchesOptionalPhi(t *testing.T) {
	ir := generateIR(t, `
		enum Tag { First, Second }
		type Box { int? value; }
		main() {
			b := Box(value: 7);
			e := Tag.First;
			opt := match e { Tag.First => b.value, _ => none };
		}`)
	body := extractFunc(ir, ".goroutine.main")
	if body == "" {
		t.Fatalf("expected define @.goroutine.main in IR:\n%s", ir)
	}
	// The enum-match merge phi is the Optional struct; the `_ => none` arm folds a
	// zeroinitializer directly into the phi from a `match.arm` predecessor.
	phiRe := regexp.MustCompile(`phi \{ i1, i64 \} \[[^]]+\], \[ zeroinitializer, %match`)
	if !phiRe.MatchString(body) {
		t.Errorf("expected an Optional-typed `phi { i1, i64 }` with a zeroinitializer none arm:\n%s", body)
	}
	// Pre-fix failure shape: a bare i1 0/false incoming into the Optional phi.
	if regexp.MustCompile(`phi \{ i1, i64 \}.*\[ (false|i1 0|0), %match`).MatchString(body) {
		t.Errorf("none arm lowered to the bare i1 void fallback (pre-T0496 phi mismatch):\n%s", body)
	}
}
