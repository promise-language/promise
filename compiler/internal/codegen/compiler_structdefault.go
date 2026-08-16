package codegen

import (
	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// declareGenericStructuralDefaults declares (without bodies) the default methods
// that a non-generic concrete type inherits from a *generic* structural interface
// instance (e.g. `type Counter is Box[int]` inheriting Box[T]'s default
// `put_two`). The bodies are generated later by defineGenericStructuralDefaults.
//
// A non-generic structural interface compiles its own default methods (e.g.
// IBox.put_two), which concrete implementors reference directly in their vtable.
// A *generic* interface instance does not: the mono pipeline skips structural
// instances (declareMonoMethods / declareMonoSynthesizedDefaults), so no
// Box__int.put_two is ever emitted. Without this, the concrete implementor's
// vtable slot for the inherited default is left null, and dispatching it through
// the interface view jumps to address 0 (T0862).
//
// Declaration must happen before emitVtableGlobals so the concrete vtable can
// reference the stub; body generation is deferred (see
// defineGenericStructuralDefaults) until after compileModules, because some
// default bodies (e.g. Iterator combinators) reference mono instances/layouts
// that are only available then. This mirrors the declare/define split the mono
// pipeline uses for generic concrete types (declare/defineMonoSynthesizedDefaults).
func (c *Compiler) declareGenericStructuralDefaults(file *ast.File) {
	c.forEachConcreteGenericStructuralParent(file, func(named, iface *types.Named, subst map[*types.TypeParam]types.Type) {
		// The interface may live in a catalog/std module (e.g. std.Iterator[T]).
		// declareStructuralDefaultStubs now self-resolves the interface's declaring
		// file and swaps c.info per recursion level (T1377), so the direct interface
		// as well as any transitively-inherited grandparent interface that crosses
		// files/modules resolve correctly. Its name uses the concrete type's plain
		// name (no instance suffix) and it does not tag moduleOwnedFuncs/
		// instanceOwnedFuncs, so the stub stays in the main IR. Using main-file-only
		// lookup here (the T0862 fix's original form) found no TypeDecl for a module
		// interface, so no stub was declared and the concrete type's vtable slot for
		// the inherited default was left null → segfault when dispatched (T1374).
		c.declareStructuralDefaultStubs(named.Obj().Name(), named, iface, subst)
	})
}

// defineGenericStructuralDefaults generates the bodies for the stubs declared by
// declareGenericStructuralDefaults. Runs after compileModules so default bodies
// that depend on mono layouts resolve correctly.
func (c *Compiler) defineGenericStructuralDefaults(file *ast.File) {
	c.forEachConcreteGenericStructuralParent(file, func(named, iface *types.Named, subst map[*types.TypeParam]types.Type) {
		c.defineConcreteStructuralDefaultBodies(file, named, iface, subst)
	})
}

// forEachConcreteGenericStructuralParent invokes fn for every (concrete, iface)
// pair where concrete is a non-generic user type in file that inherits default
// methods from the generic structural interface instance iface, passing the
// type-arg substitution derived from the parent reference.
func (c *Compiler) forEachConcreteGenericStructuralParent(file *ast.File, fn func(named, iface *types.Named, subst map[*types.TypeParam]types.Type)) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if named == nil || len(named.TypeParams()) > 0 || named.IsStructural() {
			continue
		}
		for _, pr := range named.Parents() {
			// Only generic structural parents need this — non-generic interfaces
			// compile their own default methods, which the vtable references.
			if pr.Named.IsStructural() && len(pr.TypeArgs) > 0 {
				subst := types.BuildSubstMap(pr.Named.TypeParams(), pr.TypeArgs)
				// Augment with the parent interface chain's own type params (e.g.
				// `Derived[int] is Base[T]` adds Base.T → int) so default bodies
				// inherited transitively through grandparent interfaces resolve
				// their type params instead of leaving them as fat-pointer
				// placeholders (which crashes wrapOk / method ABI).
				mergeParentSubst(pr.Named, subst)
				fn(named, pr.Named, subst)
			}
		}
	}
}

// defineConcreteStructuralDefaultBodies generates method bodies for the default
// methods a non-generic concrete type inherits from a generic structural
// interface. Mirrors defineStructuralDefaultBodies but without the mono-instance
// context (no monoCtx, no instanceOwnedFuncs tag) — the concrete type is a plain
// non-generic type whose functions live in the main IR.
func (c *Compiler) defineConcreteStructuralDefaultBodies(file *ast.File, concrete, iface *types.Named, subst map[*types.TypeParam]types.Type) {
	ifaceTD, ifaceModInfo := c.findTypeDeclAnyFile(iface.Obj().Name())
	if ifaceTD == nil {
		return
	}
	if ifaceModInfo != nil {
		savedInfo := c.info
		c.info = ifaceModInfo
		defer func() { c.info = savedInfo }()
	}
	for _, md := range ifaceTD.Methods {
		if md.Body == nil {
			continue
		}
		m := c.lookupMethodForDecl(iface, md)
		if m == nil || m.IsAbstract() {
			continue
		}
		if hasOwnMethod(concrete, md.Name) {
			continue // concrete type overrides the default
		}
		if len(md.TypeParams) > 0 {
			continue // generic methods require explicit type args at call sites
		}
		mangledName := mangleMethodDeclName(concrete.Obj().Name(), md)
		fn, ok := c.funcs[mangledName]
		if !ok || len(fn.Blocks) > 0 {
			continue // not declared, or already defined
		}
		saved := c.saveState()
		c.selfSubst = &selfSubstInfo{iface: iface, concrete: concrete}
		c.typeSubst = subst
		c.defineMethodFunc(md, m, fn, concrete)
		c.restoreState(saved)
	}
	// Recurse into parent interfaces (e.g. Ordered inherits != from Equal).
	for _, pr := range iface.Parents() {
		if pr.Named.IsStructural() {
			c.defineConcreteStructuralDefaultBodies(file, concrete, pr.Named, subst)
		}
	}
}

// ensureDefaultMethodsSynthesized triggers synthesis of default methods from a
// structural parent for a concrete type. For generic interfaces (e.g., Iterator[T]),
// also sets typeSubst from the parent ref's concrete type args.
func (c *Compiler) ensureDefaultMethodsSynthesized(concrete, iface *types.Named) {
	// Build typeSubst from the concrete type's parent ref to the interface.
	var typeSubst map[*types.TypeParam]types.Type
	for _, pr := range concrete.Parents() {
		if pr.Named == iface && len(pr.TypeArgs) > 0 {
			typeSubst = types.BuildSubstMap(iface.TypeParams(), pr.TypeArgs)
			break
		}
	}
	// If not a direct parent, walk transitively
	if typeSubst == nil {
		typeSubst = c.buildTransitiveParentSubst(concrete, iface)
	}

	savedSubst := c.typeSubst
	if typeSubst != nil {
		c.typeSubst = typeSubst
	}
	c.synthesizeDefaultMethods(concrete, iface)
	c.typeSubst = savedSubst
}

// buildTransitiveParentSubst builds a type substitution map for a structural
// interface that is a transitive (not direct) parent of concrete.
func (c *Compiler) buildTransitiveParentSubst(concrete, iface *types.Named) map[*types.TypeParam]types.Type {
	for _, pr := range concrete.Parents() {
		if pr.Named == iface {
			if len(pr.TypeArgs) > 0 {
				return types.BuildSubstMap(iface.TypeParams(), pr.TypeArgs)
			}
			return nil
		}
		// Recurse: build subst through intermediate parent
		if sub := c.buildTransitiveParentSubst(pr.Named, iface); sub != nil {
			return sub
		}
	}
	return nil
}

// resolveMonoParentName resolves the monomorphized name of a parent type that owns
// a method inherited by `named`. If the parent is generic and the child is accessed
// through a concrete type (Instance or Named with generic parents), the parent's
// mono name (e.g., Container[int]) is returned instead of the raw name (Container).
// resolveMonoParentName resolves the monomorphized name for an inherited method's
// owner type. E.g., for Wrapper[int].get() inherited from Container[T], returns
// "Container[int]". Builds a full substitution map from the child's concrete
// type args through the entire parent chain to resolve all type params.
func (c *Compiler) resolveMonoParentName(named *types.Named, targetType types.Type, ownerName string) string {
	// Build a full substitution map from targetType through the parent chain.
	subst := make(map[*types.TypeParam]types.Type)
	// T0637: Seed from the active mono substitution first so that inherited
	// dispatch inside a mono method body (where `this` is typed as the Named
	// origin, not an Instance) still resolves parent type args correctly.
	if c.typeSubst != nil {
		for k, v := range c.typeSubst {
			subst[k] = v
		}
	}
	if inst, ok := targetType.(*types.Instance); ok {
		origin, _ := inst.Origin().(*types.Named)
		if origin != nil {
			for k, v := range types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs()) {
				subst[k] = v
			}
		}
	}
	// Merge parent type param mappings transitively.
	mergeParentSubst(named, subst)

	// Now find the parent ref that matches ownerName by walking the chain.
	return findMonoParentName(named, ownerName, subst)
}

// resolveDirectDispatchOwner returns the type name that a direct (non-virtual)
// call to methodName on `named` — reached through `targetType` — must be mangled
// against. Three cases, in order:
//
//   - `named` declares the method itself → its own name, monomorphized when
//     `targetType` is a generic instance (e.g. "Pair[int]").
//   - inherited from a structural interface default → still the concrete type's
//     name, because defaults are synthesized per-concrete; the synthesis is
//     triggered here so the callee exists.
//   - inherited from a non-structural parent → the parent's monomorphized name
//     (e.g. `IntBox is Box[int]` inheriting format() → "Box[int]"), since no
//     <child>.<method> function is ever emitted for a plain inherited method
//     (T1551).
//
// Shared by method calls, binary/unary/compound operators, and string
// interpolation's format() dispatch so every direct-dispatch site agrees on the
// callee.
func (c *Compiler) resolveDirectDispatchOwner(named *types.Named, targetType types.Type, methodName string) string {
	ownerName := c.resolveMethodOwner(named, methodName)
	if ownerName == named.Obj().Name() {
		return c.resolveTypeName(targetType)
	}
	if structParent := c.findStructuralOwner(named, methodName); structParent != nil {
		c.ensureDefaultMethodsSynthesized(named, structParent)
		return c.resolveTypeName(targetType)
	}
	return c.resolveMonoParentName(named, targetType, ownerName)
}

// findMonoParentName walks the parent chain to find ownerName and build
// the monomorphized Instance name from the already-computed substitution map.
func findMonoParentName(named *types.Named, ownerName string, subst map[*types.TypeParam]types.Type) string {
	for _, pr := range named.Parents() {
		if pr.Named.Obj().Name() == ownerName && len(pr.TypeArgs) > 0 {
			resolvedArgs := make([]types.Type, len(pr.TypeArgs))
			for i, ta := range pr.TypeArgs {
				resolvedArgs[i] = types.Substitute(ta, subst)
			}
			parentInst := types.NewInstance(pr.Named, resolvedArgs)
			return monoName(parentInst)
		}
		if pr.Named.Obj().Name() != ownerName {
			result := findMonoParentName(pr.Named, ownerName, subst)
			if result != ownerName {
				return result
			}
		}
	}
	return ownerName
}

// synthesizeDefaultMethods generates LLVM functions for default methods from
// a structural interface that a concrete type does not override.
// Called lazily when a view vtable is needed for (concrete, iface).
func (c *Compiler) synthesizeDefaultMethods(concrete, iface *types.Named) {
	// Generic types (with unresolved TypeParams) must not be synthesized here.
	// Each concrete instantiation gets its own synthesized defaults via
	// defineMonoSynthesizedDefaults / defineStructuralDefaultBodies.
	if len(concrete.TypeParams()) > 0 {
		return
	}
	// Find the interface's AST TypeDecl to get method bodies.
	// The interface may be defined in a module file (e.g., std.Iterator[T]),
	// so search all available files, not just c.file.
	ifaceTD, ifaceModInfo := c.findTypeDeclAnyFile(iface.Obj().Name())
	if ifaceTD == nil {
		return
	}
	// If the interface lives in a module, use the module's sema info for type
	// lookups inside method bodies (e.g., _FnIter[T] type args in Iterator[T]).
	if ifaceModInfo != nil {
		savedInfo := c.info
		c.info = ifaceModInfo
		defer func() { c.info = savedInfo }()
	}

	concreteName := concrete.Obj().Name()

	for _, md := range ifaceTD.Methods {
		if md.Body == nil {
			continue // abstract method — skip
		}

		// Check if concrete already has this method (override)
		ifaceMethod := c.lookupMethodForDecl(iface, md)
		if ifaceMethod == nil || ifaceMethod.IsAbstract() {
			continue
		}
		// Only skip if the concrete type has its OWN implementation (not inherited).
		// LookupMethod traverses parents, so we check own methods directly.
		if hasOwnMethod(concrete, md.Name) {
			continue // concrete type overrides the default
		}

		// Method-level generic methods (e.g. map[R], zip[U]) require explicit type args.
		// They are handled by declareMonoMethodInstances at each call site, not here.
		if len(md.TypeParams) > 0 {
			continue
		}

		mangledName := mangleMethodDeclName(concreteName, md)
		if _, exists := c.funcs[mangledName]; exists {
			continue // already synthesized
		}

		sig := ifaceMethod.Sig()

		// Save all compiler state — we're in the middle of codegen for another function
		saved := c.saveState()
		c.selfSubst = &selfSubstInfo{iface: iface, concrete: concrete}

		var params []*ir.Param
		if sig.Recv() != nil {
			params = append(params, ir.NewParam("this", irtypes.I8Ptr))
		}
		for _, p := range sig.Params() {
			params = append(params, ir.NewParam(p.Name(), c.resolveParamType(p)))
		}

		retType := irtypes.Type(irtypes.Void)
		if sig.Result() != nil {
			retType = c.resolveType(sig.Result())
		}
		if sig.CanError() {
			retType = computeResultType(retType)
		}

		fn := c.module.NewFunc(mangledName, retType, params...)
		c.funcs[mangledName] = fn // register BEFORE body generation (prevents recursion)

		// Generate the body with selfSubst active
		c.defineMethodFunc(md, ifaceMethod, fn, concrete)

		// Restore compiler state
		c.restoreState(saved)
	}

	// Recurse into parent interfaces (e.g., Ordered inherits != default from Equal)
	for _, pr := range iface.Parents() {
		if pr.Named.IsStructural() {
			c.synthesizeDefaultMethods(concrete, pr.Named)
		}
	}
}
