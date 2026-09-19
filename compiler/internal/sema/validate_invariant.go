package sema

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// validateInvariantMethods is the declaration-site half of `_validate!`
// (docs/language-design.md §5.7 → Validation, T1752). It runs after every type
// is defined, because "is this type validated?" walks the inheritance chain and
// a child may be declared before its parent.
//
// Two rules are enforced here:
//
//  1. The shape of a `_validate!` declaration (see validateValidateMethod).
//  2. Every construction path on a validated type is failable. `new` must be
//     `new!` and every “ `factory “ must carry `!` — even when the body cannot
//     itself fail, because the value it yields may still be rejected and a
//     signature that hid that would be lying to its caller.
func (c *Checker) validateInvariantMethods(file *ast.File) {
	for _, decl := range file.Decls {
		if c.info.FilteredDecls[decl] {
			continue
		}
		switch d := decl.(type) {
		case *ast.TypeDecl:
			named := c.namedFromDecl(d.Name)
			if named == nil {
				continue
			}
			c.rejectValidateNameReuse(d.Name, d.Methods)
			if own := named.OwnValidateMethod(); own != nil {
				c.validateValidateMethod(d.Name, own,
					methodDeclPos(d.Methods, types.ValidateMethodName, d.Pos()),
					named.IsStructural(), c.hasAnnotation(d.Annotations, "native"))
			}
			if !named.IsValidated() {
				continue
			}
			reason := validatedReason(named)
			// `new` is the construction path a type has besides its factories;
			// an enum has none.
			if nm := lookupOwnMethod(named, "new"); nm != nil && nm.Sig() != nil && !nm.Sig().CanError() {
				c.errorf(methodDeclPos(d.Methods, "new", d.Pos()),
					"new() on %s must be failable — write 'new!' — because %s: construction can raise, so the signature must say so",
					d.Name, reason)
			}
			c.requireFailableFactories(named.Methods(), d.Name, reason, d.Methods, d.Pos())

		case *ast.EnumDecl:
			enum := c.enumFromDecl(d.Name)
			if enum == nil {
				continue
			}
			c.rejectValidateNameReuse(d.Name, d.Methods)
			own := enum.OwnValidateMethod()
			if own == nil {
				continue
			}
			// Enums do not inherit, so the invariant is always the enum's own.
			c.validateValidateMethod(d.Name, own,
				methodDeclPos(d.Methods, types.ValidateMethodName, d.Pos()),
				false, c.hasAnnotation(d.Annotations, "native"))
			c.requireFailableFactories(enum.Methods(), d.Name,
				d.Name+" declares "+types.ValidateMethodName+"!", d.Methods, d.Pos())
		}
	}
}

// rejectValidateNameReuse keeps _validate reserved for the invariant (T1752).
// A getter or setter of that name would sit alongside a real _validate! and
// shadow it at every `x._validate` — and OwnValidateMethod is method-only, so
// nothing else would ever notice.
func (c *Checker) rejectValidateNameReuse(owner string, decls []*ast.MethodDecl) {
	for _, md := range decls {
		if md.Name != types.ValidateMethodName || !(md.IsGetter || md.IsSetter) {
			continue
		}
		kind := "getter"
		if md.IsSetter {
			kind = "setter"
		}
		c.errorf(md.Pos(), "'%s' is reserved for the type's invariant on %s — a %s may not take that name",
			types.ValidateMethodName, owner, kind)
	}
}

// requireFailableFactories reports every “ `factory “ on a validated type
// that is not declared `!`. §5.7: this holds even when the factory's body
// cannot itself fail — the value it yields may still be rejected, and a
// signature that hid that would be lying to its caller. (T1752)
func (c *Checker) requireFailableFactories(methods []*types.Method, owner, reason string, decls []*ast.MethodDecl, fallback ast.Pos) {
	for _, m := range methods {
		if !m.IsFactory() || m.Sig() == nil || m.Sig().CanError() {
			continue
		}
		c.errorf(methodDeclPos(decls, m.Name(), fallback),
			"factory '%s' on %s must be failable — write '%s!' — because %s: it can fail to yield a value even when its own body cannot fail",
			m.Name(), owner, m.Name(), reason)
	}
}

// markValidatedConstruction records that a construction expression yields a
// value whose _validate! chain must run (T1752), and makes the expression
// failable so the ordinary error-handling rules force ?/^/?! on every call
// site. See docs/language-design.md §5.7 -> Validation.
//
// A Self built inside a `factory on the same type is ALSO recorded as a
// deferral candidate, but is still marked here: only binding it to a local
// downgrades it (see trackDeferredValidateLocal, which runs before anything
// reads the marking). Marking eagerly is what keeps a construction with no
// fixup window — a branching `return if … { Self(…) } else { Self(…) }`, say —
// validated in place rather than silently skipped.
func (c *Checker) markValidatedConstruction(e ast.Expr, typ types.Type) {
	named := validatedNamed(typ)
	if named == nil {
		return
	}
	// §5.7: clone() does not validate — a clone is an identical copy of an
	// instance that was already valid. Scoped to the clone's OWN type: building
	// some other validated value inside a clone body still validates it.
	if c.cloneBodyOwner == types.Type(named) {
		return
	}
	if c.inFactoryBody && c.curType == named {
		c.factoryDeferredCtors[e] = true
	}
	c.info.FailableExprs[e] = true
	c.info.ValidateSites[e] = ValidateWrap
}

// markValidatedEnumConstruction is the enum analogue: a payload-carrying
// variant constructor yields an instance whose invariant must hold. A fieldless
// variant is a plain enumerated value with no state to validate, and is not a
// call expression at all, so it never reaches here.
func (c *Checker) markValidatedEnumConstruction(e ast.Expr, typ types.Type) {
	enum := extractEnumType(typ)
	if enum == nil || !enum.IsValidated() {
		return
	}
	if c.cloneBodyOwner == types.Type(enum) { // §5.7: clone() does not validate
		return
	}
	c.info.FailableExprs[e] = true
	c.info.ValidateSites[e] = ValidateWrap
}

// trackDeferredValidateLocal DEFERS a Self the enclosing `factory constructed,
// when it is bound to a local (T1752).
//
// Binding to a local is exactly what opens the post-construction `final fixup
// window §5.7 describes: only a named binding can be mutated after
// construction. A construction that is not bound has no such window — nothing
// can reach it to change it — so it stays validated in place.
//
// This runs after the initializer is checked but BEFORE checkFailableEscape, so
// undoing the eager marking here is invisible to every consumer of it.
func (c *Checker) trackDeferredValidateLocal(name string, value ast.Expr) {
	ctor := unwrapDeferredCtor(value)
	if !c.factoryDeferredCtors[ctor] {
		return
	}
	// An error operator on the binding is rejected rather than deferred. It
	// cannot mean what it says: the chain has not run yet at this point, so
	// there is no failure for `?!` / `?^` to handle — and honouring the operator
	// instead (validating here, then allowing the `final fixup below it) would
	// leave the FIXED value unvalidated, the exact hole §5.7's deferral closes.
	if value != ctor {
		c.errorf(value.Pos(), "remove the error operator: '%s' is a %s this factory is still constructing, "+
			"so its %s! runs at the factory's 'return' — after the `final fields are fixed — not here",
			name, c.curType, types.ValidateMethodName)
		return
	}
	delete(c.info.FailableExprs, ctor)
	delete(c.info.ValidateSites, ctor)
	c.factoryDeferredLocals[name] = true
}

// markDeferredValidateReturn runs the deferred `_validate!` chain at the point a
// “ `factory “-constructed Self leaves by `return` — after any
// post-construction “ `final “ fixups, so the chain never observes a
// placeholder. A `return` of anything else (another factory's result, a child
// built by an ordinary construction expression) was already validated where it
// was produced, and is not re-validated here. (T1752)
func (c *Checker) markDeferredValidateReturn(s *ast.ReturnStmt) {
	if !c.inFactoryBody || s.Value == nil {
		return
	}
	// Only the local-variable shape reaches here — it is the only one deferred
	// (see trackDeferredValidateLocal). Every other construction validated in
	// place, and the ordinary auto-propagation carries its error out.
	ident, ok := s.Value.(*ast.IdentExpr)
	if !ok || !c.factoryDeferredLocals[ident.Name] {
		return
	}
	c.info.ValidateSites[s.Value] = ValidatePropagate
	c.recordFailableEscape()
}

// rejectDeferredSelfEscape reports a deferred Self named anywhere the instance
// could escape the factory that built it. The permission is granted only by a
// member-access target and a `return` value (see deferredSelfAllowed), so every
// other position lands here. (T1752, docs/language-design.md §5.7 → Validation)
func (c *Checker) rejectDeferredSelfEscape(e *ast.IdentExpr) {
	if !c.inFactoryBody || c.deferredSelfAllowed || !c.factoryDeferredLocals[e.Name] {
		return
	}
	c.errorf(e.Pos(), "'%s' holds a %s the factory constructed, which must leave by 'return' — "+
		"it has not been validated yet, and %s! has no other point at which to run",
		e.Name, c.curType, types.ValidateMethodName)
}

// unwrapDeferredCtor peels the error operators a construction expression may
// carry, so trackDeferredValidateLocal can find the recorded CallExpr under
// one — and tell, by whether it peeled anything, that an operator was written.
//
// Only `?^` and `?!` are peeled. They are the two the rejection is about, and
// the only two that reach here: both var-decl call sites guard on
// isConstructorCallExpr, which recognizes nothing else that could wrap a
// constructor. Peeling more would be worse than useless — a ParenExpr arm, for
// instance, would report "remove the error operator" for `(Self(…))`.
func unwrapDeferredCtor(e ast.Expr) ast.Expr {
	for {
		switch x := e.(type) {
		case *ast.ErrorPropagateExpr:
			e = x.Expr
		case *ast.ErrorPanicExpr:
			e = x.Expr
		default:
			return e
		}
	}
}

// validatedNamed unwraps typ to the *types.Named being constructed, or nil when
// the construction needs no validation.
func validatedNamed(typ types.Type) *types.Named {
	var named *types.Named
	switch t := typ.(type) {
	case *types.Named:
		named = t
	case *types.Instance:
		named, _ = t.Origin().(*types.Named)
	}
	if named == nil || !named.IsValidated() {
		return nil
	}
	return named
}

// extractEnumType unwraps typ to the *types.Enum it names, or nil.
func extractEnumType(typ types.Type) *types.Enum {
	switch t := typ.(type) {
	case *types.Enum:
		return t
	case *types.Instance:
		e, _ := t.Origin().(*types.Enum)
		return e
	}
	return nil
}

// validatedReason names the type whose `_validate!` makes named validated —
// the type itself, or the nearest ancestor that declares one.
func validatedReason(named *types.Named) string {
	chain := named.ValidateChain()
	if len(chain) == 0 {
		return ""
	}
	owner := chain[len(chain)-1]
	if owner == named {
		return named.Obj().Name() + " declares " + types.ValidateMethodName + "!"
	}
	return named.Obj().Name() + " inherits " + types.ValidateMethodName + "! from " + owner.Obj().Name()
}

// methodDeclPos finds the source position of a method declaration by name,
// falling back to the type declaration's own position.
func methodDeclPos(methods []*ast.MethodDecl, name string, fallback ast.Pos) ast.Pos {
	for _, md := range methods {
		if md.Name == name {
			return md.Pos()
		}
	}
	return fallback
}

// namedFromDecl resolves a type declaration's name to its *types.Named.
func (c *Checker) namedFromDecl(name string) *types.Named {
	obj := c.declScope.Lookup(name)
	if obj == nil {
		return nil
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return nil
	}
	named, _ := tn.Type().(*types.Named)
	return named
}

// enumFromDecl resolves an enum declaration's name to its *types.Enum.
func (c *Checker) enumFromDecl(name string) *types.Enum {
	obj := c.declScope.Lookup(name)
	if obj == nil {
		return nil
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return nil
	}
	enum, _ := tn.Type().(*types.Enum)
	return enum
}
