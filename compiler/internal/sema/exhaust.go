package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// checkMatchExhaustiveness verifies that match expressions cover all cases.
// For enum subjects, all variants must be matched (or a wildcard must be present).
// For other subjects, a wildcard or binding is required.
func (c *Checker) checkMatchExhaustiveness(e *ast.MatchExpr, subjectType types.Type) {
	if len(e.Arms) == 0 {
		c.errorf(e.Pos(), "match expression has no arms")
		return
	}

	// Check for wildcard/catch-all arm
	if c.hasCatchAll(e.Arms) {
		return // always exhaustive
	}

	if subjectType == nil {
		return // can't check without type info
	}

	// For enum types, check variant coverage
	enum := extractEnum(subjectType)
	if enum == nil {
		// Non-enum match without wildcard — warn
		c.errorf(e.Pos(), "match on non-enum type %s must include a wildcard (_) or binding pattern", subjectType)
		return
	}

	// Collect covered variants
	covered := make(map[string]bool)
	for _, arm := range e.Arms {
		c.collectCoveredVariants(arm.Pattern, enum, covered)
	}

	// Find missing variants
	var missing []string
	for _, v := range enum.Variants() {
		if !covered[v.Name()] {
			missing = append(missing, v.Name())
		}
	}

	if len(missing) > 0 {
		if len(missing) == 1 {
			c.errorf(e.Pos(), "match is not exhaustive: missing variant %s.%s", enum, missing[0])
		} else {
			c.errorf(e.Pos(), "match is not exhaustive: missing %d variants of %s (%s, ...)",
				len(missing), enum, missing[0])
		}
	}
}

// hasCatchAll reports whether any arm in the list is a wildcard or binding pattern
// (without a guard, since guards make the arm conditional).
func (c *Checker) hasCatchAll(arms []*ast.MatchArm) bool {
	for _, arm := range arms {
		if arm.Guard != nil {
			continue // guarded arms don't count as catch-all
		}
		switch arm.Pattern.(type) {
		case *ast.WildcardMatchPattern:
			return true
		case *ast.NameMatchPattern:
			return true
		}
	}
	return false
}

// collectCoveredVariants records which enum variants are covered by a pattern.
func (c *Checker) collectCoveredVariants(pat ast.MatchPattern, enum *types.Enum, covered map[string]bool) {
	switch p := pat.(type) {
	case *ast.EnumVariantMatchPattern:
		if c.matchesEnumQualified(p.Module, p.Enum, enum) {
			covered[p.Variant] = true
		}
	case *ast.EnumDestructureMatchPattern:
		if c.matchesEnumQualified(p.Module, p.Enum, enum) {
			covered[p.Variant] = true
		}
	case *ast.ShortDestructureMatchPattern:
		// Short form: Name(bindings) — check if it's a variant of the subject enum
		if enum.LookupVariant(p.Name) != nil {
			covered[p.Name] = true
		}
	}
}

// matchIsExhaustive reports whether a match expression covers all cases.
// Used by missing-return analysis to determine if a match can fall through.
func (c *Checker) matchIsExhaustive(e *ast.MatchExpr, subjectType types.Type) bool {
	if len(e.Arms) == 0 {
		return false
	}
	if c.hasCatchAll(e.Arms) {
		return true
	}
	if subjectType == nil {
		return false
	}
	enum := extractEnum(subjectType)
	if enum == nil {
		return false
	}
	covered := make(map[string]bool)
	for _, arm := range e.Arms {
		c.collectCoveredVariants(arm.Pattern, enum, covered)
	}
	for _, v := range enum.Variants() {
		if !covered[v.Name()] {
			return false
		}
	}
	return true
}

// extractEnum returns the underlying Enum from a type, handling both
// direct *Enum and *Instance wrapping an Enum origin.
func extractEnum(typ types.Type) *types.Enum {
	switch t := typ.(type) {
	case *types.Enum:
		return t
	case *types.Instance:
		if e, ok := t.Origin().(*types.Enum); ok {
			return e
		}
	}
	return nil
}

// matchesEnumQualified checks if a pattern's enum name matches the given enum type,
// handling module-qualified names (e.g., "json", "JsonValue").
func (c *Checker) matchesEnumQualified(module, name string, enum *types.Enum) bool {
	var obj types.Object
	if module != "" {
		modObj := c.lookup(module)
		if modObj == nil {
			return false
		}
		mod, ok := modObj.(*types.Module)
		if !ok {
			return false
		}
		if mod.Scope() == nil {
			return false
		}
		obj = mod.Scope().Lookup(name)
	} else {
		obj = c.lookup(name)
	}
	if obj == nil {
		return false
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return false
	}
	return tn.Type() == enum
}
