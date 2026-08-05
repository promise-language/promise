package sema

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// validateAbstractOverrides checks that every concrete method a type provides for
// an inherited abstract requirement actually satisfies that requirement's
// signature. Sema's abstract/concrete bookkeeping (Named.IsAbstract) matches an
// override to a requirement purely by slot key (name/kind), never comparing
// failability or the full signature. Without this pass a failable `next!(~this)
// int?` counts as overriding the non-failable `next(~this) int?` from
// Iterator[int], the type is treated as concrete, and codegen synthesizes the
// inherited default combinators against the wrong optional shape — panicking in
// wrapOptional (T1376). Rejecting the incompatible override here turns that
// compiler panic into a clean diagnostic. It reuses the exact relaxed comparator
// the structural-satisfaction path uses (types.SatisfiesAbstract), so a valid
// non-failable override of a failable requirement, or `T` satisfying `T?`, still
// passes for a pure-abstract requirement. When the declaring interface also
// contributes synthesized default method bodies (e.g. Iterator's combinators),
// those relaxations are additionally shape-checked — see the inner comment on
// declarerSynthesizesDefaults. Runs after Define (all parent methods resolved).
// Enums cannot have abstract methods, so only TypeDecls are checked.
func (c *Checker) validateAbstractOverrides(file *ast.File) {
	for _, decl := range file.Decls {
		if c.info.FilteredDecls[decl] {
			continue
		}
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		obj := c.fileScope.Lookup(td.Name)
		if obj == nil {
			continue
		}
		tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		// Substitution mapping each parent's generic type params to the concrete
		// args used in the `is` clause (e.g. Iterator's T -> int), transitively.
		subst := c.buildParentSubstMap(named)
		for _, am := range named.ParentAbstractMethods() {
			// Skip abstract methods with their own type params: the relaxed
			// comparator compares method-level TypeParams by pointer identity, so
			// the abstract's and the override's params would never match even for
			// a correct override. Validating these would need method-param
			// pairing; it's out of scope for T1376 (next has no type params) and
			// stays as uncovered as it was before this pass existed.
			if len(am.Method.Sig().TypeParams()) > 0 {
				continue
			}
			// Look up the satisfying concrete method by kind, mirroring
			// types.Implements. A missing or still-abstract method means the type
			// legitimately stays abstract — instantiation is rejected elsewhere.
			var override *types.Method
			switch {
			case am.Method.IsGetter():
				override = named.LookupGetter(am.Method.Name())
			case am.Method.IsSetter():
				override = named.LookupSetter(am.Method.Name())
			default:
				override = named.LookupMethod(am.Method.Name())
			}
			if override == nil || override.IsAbstract() {
				continue
			}
			if !types.SatisfiesAbstract(override.Sig(), am.Method.Sig(), subst, am.Declarer, named) {
				c.reportOverrideMismatch(td, am, override, subst)
				continue
			}
			// A structurally-valid override can still break codegen: when the
			// declaring interface contributes synthesized default method bodies
			// (e.g. Iterator's combinators), those bodies call this override
			// directly and assume it returns the abstract's exact substituted
			// shape. A relaxed match — `T` for `T?`, non-failable for failable —
			// changes the LLVM return shape and panics synthesis (T1376). Require
			// an exact return shape in that case; the relaxed rules still hold for
			// pure-abstract requirements with no default bodies.
			if declarerSynthesizesDefaults(am.Declarer) &&
				!types.ReturnShapeMatchesAbstract(override.Sig(), am.Method.Sig(), subst, am.Declarer, named) {
				c.reportOverrideMismatch(td, am, override, subst)
			}
		}
	}
}

// declarerSynthesizesDefaults reports whether an interface contributes
// synthesized default method bodies to its concrete implementors — i.e. it is
// structural and declares at least one non-abstract (default) method. Codegen
// copies those default bodies onto the concrete type, and they call the
// abstract methods directly, so the concrete override must match the abstract
// return shape exactly.
func declarerSynthesizesDefaults(d *types.Named) bool {
	if !d.IsStructural() {
		return false
	}
	for _, m := range d.Methods() {
		if !m.IsAbstract() {
			return true
		}
	}
	return false
}

// reportOverrideMismatch emits a diagnostic for a concrete method that does not
// satisfy the inherited abstract requirement it overrides. It anchors the error
// at the overriding MethodDecl when the type declares it directly, else at the
// type declaration (for an override inherited from an intermediate type).
func (c *Checker) reportOverrideMismatch(td *ast.TypeDecl, am types.AbstractMethodInfo, override *types.Method, subst map[*types.TypeParam]types.Type) {
	pos := td.Pos()
	for _, md := range td.Methods {
		if md.Name == am.Method.Name() &&
			md.IsGetter == am.Method.IsGetter() &&
			md.IsSetter == am.Method.IsSetter() {
			pos = md.Pos()
			break
		}
	}
	substAbstract := types.Substitute(am.Method.Sig(), subst).(*types.Signature)
	// Failability is the common case (T1376) — give it targeted wording.
	if override.Sig().CanError() && !substAbstract.CanError() {
		c.errorf(pos, "type %s cannot satisfy abstract method '%s' from %s: a failable method %s%s cannot satisfy a non-failable requirement %s%s",
			td.Name, am.Method.Name(), am.Declarer,
			am.Method.Name(), override.Sig(),
			am.Method.Name(), substAbstract)
		return
	}
	c.errorf(pos, "type %s cannot satisfy abstract method '%s' from %s: incompatible signature (expected %s%s, found %s%s)",
		td.Name, am.Method.Name(), am.Declarer,
		am.Method.Name(), substAbstract, am.Method.Name(), override.Sig())
}
