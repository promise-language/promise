package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// T1583: "is this target a structural interface represented as a {vtable, instance}
// fat pointer?" must be spelled exactly one way in this package — isStructuralView(n)
// (rtti.go), or isNonValueStructuralType(t) which delegates to it from a types.Type.
//
// T1550 is what happens when it is not. The predicate was spelled inline as
// `named.IsStructural() && !named.IsValueType()` at ~20 sites, a handful of which had
// dropped the `!IsValueType()` half; those took the boxing path for a flat value
// struct and panicked with "store operands are not compatible: src={ i8*, i8* };
// dst=%promise_Metric_v*", plus a silent-corruption variant on the parameter path.
// Consolidating the sites fixes the bug once; this guard is what stops it recurring,
// because a re-inlined spelling is exactly how the drift started.
//
// Note the deliberately narrow shapes below. The heap-user-type conjunction
//
//	!named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural()
//
// is a DIFFERENT predicate ("is this a droppable heap instance"), and its bare
// !IsStructural() is correct because the leading !IsValueType() already excludes a
// value-typed structural. It does not match any pattern here and must not be rewritten.
//
// Both conjunct orders are listed for each form. Re-inlining with the operands the
// other way round is the same predicate and the same bug, and is arguably the likelier
// slip — the site being edited usually already has an IsValueType() test in hand.
var t1583InlineSpellings = []struct {
	name string
	re   *regexp.Regexp
}{
	{
		// named.IsStructural() && !named.IsValueType()
		name: "positive",
		re:   regexp.MustCompile(`IsStructural\(\)\s*&&\s*!\w+\.IsValueType\(\)`),
	},
	{
		// !named.IsValueType() && named.IsStructural()
		name: "positive (reversed)",
		re:   regexp.MustCompile(`!\w+\.IsValueType\(\)\s*&&\s*\w+\.IsStructural\(\)`),
	},
	{
		// named == nil || !named.IsStructural() || named.IsValueType()
		name: "negated",
		re:   regexp.MustCompile(`!\w+\.IsStructural\(\)\s*\|\|\s*\w+\.IsValueType\(\)`),
	},
	{
		// named == nil || named.IsValueType() || !named.IsStructural()
		name: "negated (reversed)",
		re:   regexp.MustCompile(`\w+\.IsValueType\(\)\s*\|\|\s*!\w+\.IsStructural\(\)`),
	},
}

// The sole legitimate inline spelling: the body of isStructuralView itself, which is
// the definition every other site now routes through. Keyed by file and exact trimmed
// text, so a NEW inline spelling added to rtti.go is still caught.
var t1583Allowed = map[string]string{
	"rtti.go": "return n != nil && n.IsStructural() && !n.IsValueType()",
}

func TestT1583StructuralViewPredicateIsNotSpelledInline(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no .go sources found — is the test running outside the package directory?")
	}

	var offenders []string
	scanned := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if allowed, ok := t1583Allowed[file]; ok && trimmed == allowed {
				continue
			}
			for _, shape := range t1583InlineSpellings {
				if shape.re.MatchString(line) {
					offenders = append(offenders,
						fmt.Sprintf("%s:%d (%s): %s", file, i+1, shape.name, trimmed))
				}
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no non-test sources")
	}

	if len(offenders) > 0 {
		t.Errorf("the structural-view predicate is spelled inline at %d site(s); "+
			"call isStructuralView(n) (or isNonValueStructuralType(t) from a types.Type) "+
			"instead — see T1550/T1583:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// Guards the guard: the patterns above must actually match the shapes they describe,
// and must NOT match the heap-user-type conjunction they deliberately exclude. Without
// this, a typo'd regex would silently pass forever.
func TestT1583GuardPatternsMatchIntendedShapesOnly(t *testing.T) {
	shouldMatch := []string{
		`	if named.IsStructural() && !named.IsValueType() {`,
		`	case innerNamed != nil && innerNamed.IsStructural() && !innerNamed.IsValueType():`,
		`	} else if named != nil && named.IsStructural() && !named.IsValueType() {`,
		`	if en := extractNamed(elemType); en != nil && en.IsStructural() && !en.IsValueType() {`,
		`	if named == nil || !named.IsStructural() || named.IsValueType() {`,
		`	if innerNamed == nil || !innerNamed.IsStructural() || innerNamed.IsValueType() {`,
		// Same two predicates with the conjuncts the other way round.
		`	if !named.IsValueType() && named.IsStructural() {`,
		`	if named == nil || named.IsValueType() || !named.IsStructural() {`,
	}
	shouldNotMatch := []string{
		// The heap-user-type predicate — a different question, correct as written.
		`	if !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {`,
		// The consolidated spellings this item introduces.
		`	if isStructuralView(named) {`,
		`	if !isStructuralView(innerNamed) {`,
		`	case isStructuralView(innerNamed):`,
		// Interface-origin skips (mono.go / compiler_structdefault.go) stay as-is.
		`	if named.IsStructural() {`,
		`	if !pr.Named.IsStructural() {`,
	}

	matches := func(line string) bool {
		for _, shape := range t1583InlineSpellings {
			if shape.re.MatchString(line) {
				return true
			}
		}
		return false
	}
	for _, line := range shouldMatch {
		if !matches(line) {
			t.Errorf("guard failed to flag an inline spelling: %s", strings.TrimSpace(line))
		}
	}
	for _, line := range shouldNotMatch {
		if matches(line) {
			t.Errorf("guard wrongly flagged a line it must leave alone: %s", strings.TrimSpace(line))
		}
	}
}
