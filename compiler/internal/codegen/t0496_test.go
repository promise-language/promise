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

// T1189: when the sibling arm of a `none` arm is a BARE non-optional value (not an
// already-Optional field read, as T0496 covered), sema now unifies both arms to
// `T?` and codegen must wrap the bare value arm into `{ i1, T }` so the merge phi
// is Optional-typed. Pre-fix, `joinBranchTypes` returned whichever arm came first:
// a `none`-first match returned `none` → the phi was typed `i64` (void-optional
// zero) and `opt` rejected the module; a `none`-second match returned bare `T` and
// silently miscompiled the none arm to `Some(zero)`. These tests assert the phi is
// the Optional struct in both orderings across value-match, enum-match, and if-expr.

// assertOptionalScalarPhi checks a function body has an Optional-typed `{ i1, i64 }`
// merge phi and NO malformed bare-`i64` phi (the pre-fix void-optional shape).
func assertOptionalScalarPhi(t *testing.T, body, predPrefix string) {
	t.Helper()
	if !regexp.MustCompile(`phi \{ i1, i64 \}`).MatchString(body) {
		t.Errorf("expected an Optional-typed `phi { i1, i64 }` merge phi:\n%s", body)
	}
	// Pre-fix bug: the merge phi was typed bare `i64` (the void-optional zero from
	// the un-unified `none` arm). Must not appear.
	if regexp.MustCompile(`phi i64 `).MatchString(body) {
		t.Errorf("merge phi typed bare i64 — none arm not unified to Optional (T1189 regression):\n%s", predPrefix+body)
	}
}

func TestT1189_ValueMatchBareScalarNoneFirst(t *testing.T) {
	ir := generateIR(t, `main() {
		k := 0;
		vopt := match k { 0 => none, _ => 5 };
	}`)
	body := extractFunc(ir, ".goroutine.main")
	if body == "" {
		t.Fatalf("expected define @.goroutine.main in IR:\n%s", ir)
	}
	assertOptionalScalarPhi(t, body, "")
}

func TestT1189_ValueMatchBareScalarNoneSecond(t *testing.T) {
	ir := generateIR(t, `main() {
		k := 0;
		vopt := match k { 0 => 5, _ => none };
	}`)
	body := extractFunc(ir, ".goroutine.main")
	if body == "" {
		t.Fatalf("expected define @.goroutine.main in IR:\n%s", ir)
	}
	assertOptionalScalarPhi(t, body, "")
}

func TestT1189_IfExprBareScalarBothOrderings(t *testing.T) {
	for _, expr := range []string{
		`if k == 0 { none } else { 5 }`,
		`if k == 0 { 5 } else { none }`,
	} {
		ir := generateIR(t, "main() {\n\tk := 0;\n\tvopt := "+expr+";\n}")
		body := extractFunc(ir, ".goroutine.main")
		if body == "" {
			t.Fatalf("expected define @.goroutine.main in IR for %q:\n%s", expr, ir)
		}
		assertOptionalScalarPhi(t, body, expr+"\n")
	}
}

func TestT1189_EnumMatchBareScalarNoneFirst(t *testing.T) {
	ir := generateIR(t, `
		enum Tag { First, Second }
		main() {
			e := Tag.First;
			vopt := match e { Tag.First => none, _ => 5 };
		}`)
	body := extractFunc(ir, ".goroutine.main")
	if body == "" {
		t.Fatalf("expected define @.goroutine.main in IR:\n%s", ir)
	}
	assertOptionalScalarPhi(t, body, "")
}

// TestT1189_ValueMatchBareUserType — the reported case: a bare user (heap) type arm
// `_ => D(x:1)` sibling of a `none` arm. The merge phi must be the heap Optional
// struct `{ i1, { i8*, i8* } }`, and the value arm must be wrapped (no bare `i64`
// phi, which is the exact shape from the item that `opt` rejected).
func TestT1189_ValueMatchBareUserType(t *testing.T) {
	ir := generateIR(t, `
		type D { int x; }
		main() {
			k := 0;
			vopt := match k { 0 => none, _ => D(x: 1) };
		}`)
	body := extractFunc(ir, ".goroutine.main")
	if body == "" {
		t.Fatalf("expected define @.goroutine.main in IR:\n%s", ir)
	}
	if !regexp.MustCompile(`phi \{ i1, \{ i8\*, i8\* \} \}`).MatchString(body) {
		t.Errorf("expected an Optional-typed `phi { i1, { i8*, i8* } }` merge phi:\n%s", body)
	}
	if regexp.MustCompile(`phi i64 `).MatchString(body) {
		t.Errorf("merge phi typed bare i64 — the exact malformed shape from T1189:\n%s", body)
	}
}

// TestT1189_NoneArmWithOptionalSibling — the sibling of a `none` arm is itself an
// ALREADY-Optional value (`int?`), not a bare `int`. joinBranchTypes must return
// the sibling's `int?` unchanged (`none + T? → T?`, the isOpt branch) rather than
// re-wrapping to `int??`, and wrapArmValueOptional must pass the optional arm
// through untouched. The merge phi must be single-level `{ i1, i64 }` — a
// double-optional `{ i1, { i1, i64 } }` would mean the sibling was wrapped twice.
func TestT1189_NoneArmWithOptionalSibling(t *testing.T) {
	for name, expr := range map[string]string{
		"value_match": `match k { 0 => none, _ => e }`,
		"if_expr":     `if k == 0 { none } else { e }`,
	} {
		t.Run(name, func(t *testing.T) {
			ir := generateIR(t, "_optval() int? { return 3; }\nmain() {\n\tk := 0;\n\te := _optval();\n\tvopt := "+expr+";\n}")
			body := extractFunc(ir, ".goroutine.main")
			if body == "" {
				t.Fatalf("expected define @.goroutine.main in IR:\n%s", ir)
			}
			if !regexp.MustCompile(`phi \{ i1, i64 \}`).MatchString(body) {
				t.Errorf("expected single-level Optional `phi { i1, i64 }`:\n%s", body)
			}
			// A double-wrap (none + T? mis-unified to T??) would show this shape.
			if regexp.MustCompile(`phi \{ i1, \{ i1, i64 \} \}`).MatchString(body) {
				t.Errorf("merge phi is double-optional { i1, { i1, i64 } } — sibling wrapped twice (T1189 isOpt branch):\n%s", body)
			}
		})
	}
}
