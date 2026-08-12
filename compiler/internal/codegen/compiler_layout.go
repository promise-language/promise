package codegen

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// computeUserTypeLayouts computes layouts for all user-declared types in the file.
// Generic types (with TypeParams) are skipped — they're handled by computeMonoLayouts.
// Uses topological ordering to ensure parent layouts are computed before children.
// Equivalent to computeAllTypeLayouts(file, nil).
func (c *Compiler) computeUserTypeLayouts(file *ast.File) {
	c.computeAllTypeLayouts(file, nil)
}

// layoutPendingItem represents a type whose layout still needs to be computed.
// Exactly one of userNamed or monoInst is non-nil.
type layoutPendingItem struct {
	userNamed *types.Named    // non-generic user type from a TypeDecl
	monoInst  *types.Instance // monomorphic instance of a generic type
}

// computeAllTypeLayouts computes layouts for all user-declared types in the file
// and all monomorphic type instances in a single topological pass. Value-type
// field dependencies are resolved so that a user type with a generic value-type
// field (e.g., `Outer { Pt[int] inner; }`) has Pt[int]'s mono layout computed
// before Outer's layout is built. (T0565)
//
// Generic enum instances are computed first via the mono-enum dependency walker.
// Cycles among value types (which would mean an unsizable type) silently break
// recursion; the construction-time check will surface the resulting type error.
func (c *Compiler) computeAllTypeLayouts(file *ast.File, monoInstances []*types.Instance) {
	// 1. Mono enum layouts first (own dependency walker; sees enum-in-enum deps).
	c.computeMonoEnumLayoutsOnly(monoInstances)

	// 2. Build unified pending map for user types + mono type instances.
	pending := make(map[string]layoutPendingItem)
	var order []string

	if file != nil {
		for _, decl := range file.Decls {
			td, ok := decl.(*ast.TypeDecl)
			if !ok {
				continue
			}
			if c.info.FilteredDecls[decl] {
				continue // excluded by `target(cond) annotation for this build target
			}
			named := c.lookupNamedType(td.Name)
			if named == nil {
				continue
			}
			if _, exists := c.layouts[named]; exists {
				continue // skip built-in types with pre-computed layouts
			}
			if len(named.TypeParams()) > 0 {
				continue // generic — handled via monoInstances below
			}
			if isNativeTypeDecl(td) {
				continue // native types have special codegen layout handling
			}
			key := "user:" + td.Name
			if _, exists := pending[key]; exists {
				continue
			}
			pending[key] = layoutPendingItem{userNamed: named}
			order = append(order, key)
		}
	}

	for _, inst := range monoInstances {
		origin, ok := inst.Origin().(*types.Named)
		if !ok || len(origin.TypeParams()) == 0 {
			continue
		}
		name := monoName(inst)
		if _, exists := c.monoLayouts[name]; exists {
			continue
		}
		key := "mono:" + name
		if _, exists := pending[key]; exists {
			continue
		}
		pending[key] = layoutPendingItem{monoInst: inst}
		order = append(order, key)
	}

	// 3. Topological compute with cycle detection.
	computed := make(map[string]bool)
	inProgress := make(map[string]bool)
	var compute func(key string)
	compute = func(key string) {
		if computed[key] {
			return
		}
		if inProgress[key] {
			return // cycle break — recursive value types would be unsizable
		}
		item, ok := pending[key]
		if !ok {
			return
		}
		inProgress[key] = true

		// Compute dependencies (parents + value-type field targets).
		var named *types.Named
		var subst map[*types.TypeParam]types.Type
		if item.userNamed != nil {
			named = item.userNamed
			subst = buildParentFieldSubst(named)
		} else {
			inst := item.monoInst
			named = inst.Origin().(*types.Named)
			subst = types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
			mergeParentSubst(named, subst)
		}

		// Parent dependencies — for non-generic parents we depend on the user
		// layout entry; for generic parents (`is Foo[int]`) we depend on the
		// corresponding mono layout if pending.
		for _, pr := range named.Parents() {
			if len(pr.TypeArgs) == 0 {
				pkey := "user:" + pr.Named.Obj().Name()
				if _, ok := pending[pkey]; ok {
					compute(pkey)
				}
			}
			// Generic parents have their layouts created through monoInstances
			// (transitive expansion includes parent type instances). They're
			// covered by the mono pending entries; no extra lookup needed here.
		}

		// Value-type field dependencies (recurses into Optional/Tuple).
		for _, f := range named.AllFields() {
			fType := types.Substitute(f.Type(), subst)
			collectValueTypeFieldDeps(fType, pending, compute)
		}

		// Now compute this item.
		if item.userNamed != nil {
			if named.IsValueType() {
				c.layouts[named] = computeValueTypeLayout(c.module, named, c.layouts, c.ptrSize(), c.enumLayouts, c.monoEnumLayouts, c.monoLayouts)
			} else {
				c.layouts[named] = computeUserTypeLayout(c.module, named, c.layouts, c.ptrSize(), c.enumLayouts, c.monoEnumLayouts, c.monoLayouts)
			}
		} else {
			inst := item.monoInst
			name := monoName(inst)
			if named.IsValueType() {
				c.monoLayouts[name] = computeMonoValueTypeLayout(c.module, named, name, subst, c.layouts, c.ptrSize(), c.enumLayouts, c.monoEnumLayouts, c.monoLayouts)
			} else {
				c.monoLayouts[name] = computeMonoUserTypeLayout(c.module, named, name, subst, c.layouts, c.ptrSize(), c.enumLayouts, c.monoEnumLayouts, c.monoLayouts)
			}
		}
		delete(inProgress, key)
		computed[key] = true
	}

	for _, key := range order {
		compute(key)
	}
}

// collectValueTypeFieldDeps walks a field type and calls compute(key) for every
// value-type target (user value type or mono value-type instance) whose layout
// must exist before the containing item's layout is built. Recurses into
// Optional, Tuple, and Array inner types.
func collectValueTypeFieldDeps(typ types.Type, pending map[string]layoutPendingItem, compute func(string)) {
	switch t := typ.(type) {
	case *types.Optional:
		collectValueTypeFieldDeps(t.Elem(), pending, compute)
		return
	case *types.Tuple:
		for _, elem := range t.Elems() {
			collectValueTypeFieldDeps(elem, pending, compute)
		}
		return
	case *types.Array:
		// T0579: Fixed-size array fields can hold value-type elements; the
		// element layout must exist before the container's layout is built.
		collectValueTypeFieldDeps(t.Elem(), pending, compute)
		return
	case *types.Instance:
		if origin, ok := t.Origin().(*types.Named); ok && origin.IsValueType() {
			key := "mono:" + monoName(t)
			if _, ok := pending[key]; ok {
				compute(key)
			}
		}
		return
	}
	if n := extractNamed(typ); n != nil && n.IsValueType() {
		key := "user:" + n.Obj().Name()
		if _, ok := pending[key]; ok {
			compute(key)
		}
	}
}

// ensureValueTypeLayout computes the layout for a value type (and its value-type
// field deps) on demand, registering it in c.layouts/c.monoLayouts if absent.
// Enum variant data fields of value type need the embedded value struct, but
// enum layouts are built before the unified type-layout pass (both the
// non-generic enum pass and the mono-enum pass run before value-type layouts
// are computed). Value types only ever contain value/copy fields and no parents
// (enforced by sema), so the recursion terminates. Idempotent presence-guards
// prevent duplicate module.NewTypeDef; the later computeAllTypeLayouts pass skips
// anything already present. (T1016)
func (c *Compiler) ensureValueTypeLayout(typ types.Type) {
	switch t := typ.(type) {
	case *types.Optional:
		c.ensureValueTypeLayout(t.Elem())
		return
	case *types.Array:
		c.ensureValueTypeLayout(t.Elem())
		return
	case *types.Tuple:
		for _, elem := range t.Elems() {
			c.ensureValueTypeLayout(elem)
		}
		return
	case *types.MutRef:
		c.ensureValueTypeLayout(t.Elem())
		return
	case *types.SharedRef:
		c.ensureValueTypeLayout(t.Elem())
		return
	case *types.Instance:
		origin, ok := t.Origin().(*types.Named)
		if !ok || !origin.IsValueType() {
			return
		}
		name := monoName(t)
		if _, exists := c.monoLayouts[name]; exists {
			return
		}
		subst := types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
		// Recurse into field types first so nested value types are laid out.
		for _, f := range origin.AllFields() {
			c.ensureValueTypeLayout(types.Substitute(f.Type(), subst))
		}
		c.monoLayouts[name] = computeMonoValueTypeLayout(c.module, origin, name, subst, c.layouts, c.ptrSize(), c.enumLayouts, c.monoEnumLayouts, c.monoLayouts)
		return
	}
	if n := extractNamed(typ); n != nil && n.IsValueType() && len(n.TypeParams()) == 0 {
		if _, exists := c.layouts[n]; exists {
			return
		}
		// Recurse into field types first so nested value types are laid out.
		for _, f := range n.AllFields() {
			c.ensureValueTypeLayout(f.Type())
		}
		c.layouts[n] = computeValueTypeLayout(c.module, n, c.layouts, c.ptrSize(), c.enumLayouts, c.monoEnumLayouts, c.monoLayouts)
	}
}
