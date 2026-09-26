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
		obj := c.declScope.Lookup(td.Name)
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
			// A missing or still-abstract method means the type legitimately stays
			// abstract — instantiation is rejected elsewhere.
			override := named.LookupAbstractImpl(am.Method)
			if override == nil {
				continue
			}
			if !types.SatisfiesAbstract(override.Sig(), am.Method.Sig(), subst, am.Declarer, named) {
				c.reportOverrideMismatch(td, am, override, subst, abstractRequirement)
				continue
			}
			// T1486: a generator override with extra trailing params the view
			// adapter must default cannot be lowered — the adapter frees the
			// synthesized default before the coroutine reads it lazily. Reject at
			// the declaration (this fires for every explicit-`is` relationship,
			// whether or not the type is ever boxed into a view).
			if types.IsUnlowerableStreamAdapter(override.Sig(), am.Method.Sig()) {
				c.reportUnlowerableStreamAdapter(td, am, override)
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
				c.reportOverrideMismatch(td, am, override, subst, abstractRequirement)
			}
		}
	}
}

// validateInheritedOverrides checks that a method taking over the vtable slot of
// an inherited CONCRETE parent method satisfies that method's signature (T2184).
//
// validateAbstractOverrides above only ever examines abstract requirements, so a
// parent method with a BODY was never compared against the child that
// redeclared it: `tag(this) int` could be overridden by `tag(this) string`, and
// a call through the parent read the string instance pointer as an integer. It
// also accepted the one failability direction the relaxed rules forbid
// (language-design.md#structural-interface-satisfaction) — a child re-tightening
// a slot its parent had legally relaxed to non-failable, so the parent's call
// site read a void slot as a failable struct.
//
// The comparator was already right; it was simply never reached. This walks the
// nearest inherited declaration of each slot (Named.InheritedSlotDeclarations)
// and runs the same machinery, with types.SatisfiesInherited in place of
// SatisfiesAbstract to also admit a nominal covariant return.
//
// The T1486 stream check and the declarerSynthesizesDefaults return-shape
// restriction stay on the abstract path deliberately: both describe a structural
// view adapter standing in for an ABSTRACT requirement, and the synthesized
// default bodies that motivate them call the interface's abstract methods, never
// each other's relaxed returns. Applying them here would reject working code.
func (c *Checker) validateInheritedOverrides(file *ast.File) {
	for _, decl := range file.Decls {
		if c.info.FilteredDecls[decl] {
			continue
		}
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		obj := c.declScope.Lookup(td.Name)
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
		subst := c.buildParentSubstMap(named)
		for _, req := range named.InheritedSlotDeclarations() {
			if !inheritedSlotIsOverridable(req.Method) {
				continue
			}
			// The type's OWN declaration for that slot, keyed by slot so the
			// unary and binary variants of an operator never resolve to each
			// other. A type that declares nothing for the slot has nothing to
			// check — whichever ancestor declared it was checked at its own
			// declaration. An ABSTRACT own declaration is checked like any
			// other: it takes over the slot just the same, so `tag(this) string
			// `abstract` over a concrete `tag(this) int` launders exactly the
			// miscompile this pass exists to stop.
			override := named.LookupOwnSlotDeclaration(req.Method)
			if override == nil {
				continue
			}
			// Same carve-out and rationale as the abstract walk: the comparator
			// compares method-level TypeParams by pointer identity, so neither
			// side may carry its own.
			if len(override.Sig().TypeParams()) > 0 {
				continue
			}
			if !types.SatisfiesInherited(override.Sig(), req.Method.Sig(), subst, req.Declarer, named) {
				c.reportOverrideMismatch(td, req, override, subst, inheritedMethod)
			}
		}
	}
}

// inheritedSlotIsOverridable reports whether an inherited declaration is one a
// child can override — i.e. whether a child redeclaring it takes over a slot
// somebody else dispatches through. Everything it rejects is either handled by
// another pass or dispatched statically, so a signature difference there is not
// an override at all.
func inheritedSlotIsOverridable(m *types.Method) bool {
	switch {
	case m.IsAbstract():
		// validateAbstractOverrides owns these — checking them here too would
		// report the same mismatch twice.
		return false
	case m.IsNative():
		// Excluded from the vtable (AllVirtualMethods), so nothing dispatches
		// through the slot and the parent's call site binds statically.
		return false
	case len(m.Sig().TypeParams()) > 0:
		return false
	case m.Sig().Recv() == nil:
		// `factory / `global / `mono members are static calls on the type name,
		// with no instance to load a vtable from (T1749).
		return false
	case m.Name() == "new":
		// Constructors are per-type and their inheritance rules live in
		// validateConstructors — the same exemption, for the same reason, that
		// valueTypeOverride makes.
		return false
	case m.Name() == types.ValidateMethodName:
		// A child's _validate! ADDS to its parent's rather than overriding it —
		// both run, parent first — so the two never share a slot (T1752).
		return false
	}
	return true
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

// reportUnlowerableStreamAdapter emits the T1486 diagnostic for a generator
// override whose extra trailing parameters a view adapter would have to default.
// It anchors at the overriding MethodDecl when the type declares it directly,
// else at the type declaration (mirroring reportOverrideMismatch's anchoring).
func (c *Checker) reportUnlowerableStreamAdapter(td *ast.TypeDecl, am types.InheritedMethodInfo, override *types.Method) {
	pos := overridePos(td, override)
	name := am.Method.Name()
	c.errorf(pos, "type %s cannot be used as %s: its generator method '%s' takes "+
		"parameter(s) beyond %s.%s, and a stream view-adapter cannot forward a "+
		"synthesized default into the coroutine safely (T1486). Declare the "+
		"parameter in %s.%s, or remove the extra parameter from %s.%s.",
		td.Name, am.Declarer, name, am.Declarer, name,
		am.Declarer, name, td.Name, name)
}

// receiverSpelling renders a signature's receiver the way it is written in
// source — `this` for a shared borrow, `~this` for a mutable one — so a receiver
// mismatch can name both sides (T1952). Its one caller has already established
// that both receivers are present; "none" is the inert fallback for a
// receiver-less member (`factory / `global / `mono), which never reaches here.
func receiverSpelling(sig *types.Signature) string {
	if sig == nil || sig.Recv() == nil {
		return "none"
	}
	return sig.Recv().Ref().String() + "this"
}

// requirementKind selects the wording reportOverrideMismatch uses for the thing
// the override failed to live up to. The two walks reject on the identical
// comparator, so they share every arm of the diagnostic and differ only in what
// they call the requirement — an abstract method the type claims to satisfy, or
// a concrete parent method whose slot it takes over.
type requirementKind int

const (
	abstractRequirement requirementKind = iota
	inheritedMethod
)

// verb is how the message names what the type failed to do.
func (k requirementKind) verb() string {
	if k == inheritedMethod {
		return "override"
	}
	return "satisfy"
}

// noun is how the message names the requirement itself.
func (k requirementKind) noun() string {
	if k == inheritedMethod {
		return "inherited method"
	}
	return "abstract method"
}

// subject is how the message refers back to the requirement mid-sentence.
func (k requirementKind) subject() string {
	if k == inheritedMethod {
		return "the inherited method"
	}
	return "the requirement"
}

// nonFailable names what the override failed to stay compatible with, in the
// failability arm's "cannot X a non-failable Y" shape.
func (k requirementKind) nonFailable() string {
	if k == inheritedMethod {
		return "inherited method"
	}
	return "requirement"
}

// overridePos anchors a diagnostic at the OVERRIDING MethodDecl when the type
// declares it directly, else at the type declaration (for an override inherited
// from an intermediate type). It matches the override rather than the
// requirement because the override is the declaration at fault, and the two need
// not have the same kind: a getter and a plain method of the same name share one
// vtable slot, so a `get tag string` can take over an inherited `tag() int`.
// The arity comparison keeps an operator diagnostic off the wrong declaration: a
// type may declare both a unary and a binary `-`, in separate slots (T0883).
func overridePos(td *ast.TypeDecl, override *types.Method) ast.Pos {
	for _, md := range td.Methods {
		if md.Name == override.Name() &&
			md.IsGetter == override.IsGetter() &&
			md.IsSetter == override.IsSetter() &&
			(!types.IsUnaryOperatorName(md.Name) || len(md.Params) == len(override.Sig().Params())) {
			return md.Pos()
		}
	}
	return td.Pos()
}

// reportOverrideMismatch emits a diagnostic for a concrete method that does not
// satisfy the inherited requirement it overrides — an abstract one (T1376) or a
// concrete parent method whose slot it takes over (T2184).
func (c *Checker) reportOverrideMismatch(td *ast.TypeDecl, am types.InheritedMethodInfo, override *types.Method, subst map[*types.TypeParam]types.Type, kind requirementKind) {
	pos := overridePos(td, override)
	substAbstract := types.Substitute(am.Method.Sig(), subst).(*types.Signature)
	// A receiver mismatch needs its own wording: Signature.String() does not
	// print the receiver, so the generic "expected X, found Y" arm below would
	// print two identical signatures and name nothing (T1952). Ask types for the
	// verdict rather than restating it, so the message can never describe a
	// different condition than the one SatisfiesAbstract rejected on.
	if !types.ReceiverBorrowMatches(override.Sig(), substAbstract) {
		c.errorf(pos, "type %s cannot %s %s '%s' from %s: %s takes a %s receiver but %s.%s takes %s. An explicit `is` does not relax the receiver's borrow kind — declare %s with a %s receiver.",
			td.Name, kind.verb(), kind.noun(), am.Method.Name(), am.Declarer,
			kind.subject(), receiverSpelling(substAbstract), td.Name, am.Method.Name(),
			receiverSpelling(override.Sig()),
			am.Method.Name(), receiverSpelling(substAbstract))
		return
	}
	// Failability is the common case (T1376) — give it targeted wording.
	if override.Sig().CanError() && !substAbstract.CanError() {
		c.errorf(pos, "type %s cannot %s %s '%s' from %s: a failable method %s%s cannot %s a non-failable %s %s%s",
			td.Name, kind.verb(), kind.noun(), am.Method.Name(), am.Declarer,
			am.Method.Name(), override.Sig(),
			kind.verb(), kind.nonFailable(),
			am.Method.Name(), substAbstract)
		return
	}
	c.errorf(pos, "type %s cannot %s %s '%s' from %s: incompatible signature (expected %s%s, found %s%s)",
		td.Name, kind.verb(), kind.noun(), am.Method.Name(), am.Declarer,
		am.Method.Name(), substAbstract, am.Method.Name(), override.Sig())
}
