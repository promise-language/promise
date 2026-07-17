package sema

import (
	"strconv"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// resolveType converts an ast.TypeRef to a types.Type using the current scope.
// Returns nil and reports an error if the type cannot be resolved.
func (c *Checker) resolveType(ref ast.TypeRef) types.Type {
	if ref == nil {
		return nil
	}

	switch r := ref.(type) {
	case *ast.NamedTypeRef:
		return c.resolveNamedType(r)

	case *ast.QualifiedTypeRef:
		return c.resolveQualifiedType(r)

	case *ast.TupleTypeRef:
		elems := make([]types.Type, len(r.Elements))
		for i, e := range r.Elements {
			elems[i] = c.resolveType(e)
			if elems[i] == nil {
				return nil
			}
		}
		return types.NewTuple(elems)

	case *ast.FunctionTypeRef:
		params := make([]*types.Param, len(r.Params))
		for i, p := range r.Params {
			pt := c.resolveType(p)
			if pt == nil {
				return nil
			}
			params[i] = types.NewParam("", pt, types.RefNone)
		}
		var result types.Type
		if r.Return != nil {
			// "void" in return position means no return value
			if named, ok := r.Return.(*ast.NamedTypeRef); ok && named.Name == "void" && len(named.TypeArgs) == 0 {
				// result stays nil
			} else {
				result = c.resolveType(r.Return)
				if result == nil {
					return nil
				}
			}
		}
		return types.NewSignature(nil, params, result, false)

	case *ast.SharedRefTypeRef:
		inner := c.resolveType(r.Inner)
		if inner == nil {
			return nil
		}
		return types.NewSharedRef(inner)

	case *ast.MutRefTypeRef:
		inner := c.resolveType(r.Inner)
		if inner == nil {
			return nil
		}
		return types.NewMutRef(inner)

	case *ast.PointerTypeRef:
		inner := c.resolveType(r.Inner)
		if inner == nil {
			return nil
		}
		return types.NewPointer(inner)

	case *ast.OptionalTypeRef:
		inner := c.resolveType(r.Inner)
		if inner == nil {
			return nil
		}
		return types.NewOptional(inner)

	case *ast.SliceTypeRef:
		elem := c.resolveType(r.Element)
		if elem == nil {
			return nil
		}
		// T1315: `stream[int][]` — the vector sugar bypasses resolveInstance, so
		// reject a non-storable generator Stream element here.
		c.rejectStreamTypeArg(r.Pos(), []types.Type{elem})
		inst := types.NewVector(elem)
		c.recordInstance(inst)
		return inst

	case *ast.ArrayTypeRef:
		elem := c.resolveType(r.Element)
		if elem == nil {
			return nil
		}
		// T1315: `stream[int][N]` — reject a non-storable generator Stream element.
		c.rejectStreamTypeArg(r.Pos(), []types.Type{elem})
		size, err := strconv.ParseInt(r.Size, 10, 64)
		if err != nil {
			c.errorf(r.Pos(), "invalid array size: %s", r.Size)
			return nil
		}
		return types.NewArray(elem, size)

	default:
		c.errorf(ref.Pos(), "unknown type reference kind")
		return nil
	}
}

// resolveNamedType resolves a named type reference, handling generic instantiation.
func (c *Checker) resolveNamedType(r *ast.NamedTypeRef) types.Type {
	// Self resolves to the enclosing type
	if r.Name == "Self" {
		if c.curType == nil {
			c.errorf(r.Pos(), "Self can only be used inside a type body")
			return nil
		}
		if len(r.TypeArgs) > 0 {
			c.errorf(r.Pos(), "Self does not take type arguments")
			return nil
		}
		return c.selfType()
	}

	obj := c.lookup(r.Name)
	if obj == nil {
		c.errorf(r.Pos(), "undefined type: %s", r.Name)
		c.suggestForUndefinedType(r.Pos(), r.Name)
		return nil
	}

	tn, ok := obj.(*types.TypeName)
	if !ok {
		c.errorf(r.Pos(), "%s is not a type", r.Name)
		return nil
	}

	typ := tn.Type()

	// No type arguments — return the type directly
	if len(r.TypeArgs) == 0 {
		return typ
	}

	// Generic instantiation: resolve type arguments
	typeArgs := make([]types.Type, len(r.TypeArgs))
	for i, ta := range r.TypeArgs {
		typeArgs[i] = c.resolveType(ta)
		if typeArgs[i] == nil {
			return nil
		}
	}

	// Validate arity against type parameter count
	switch t := typ.(type) {
	case *types.Named:
		if len(t.TypeParams()) != len(typeArgs) {
			c.errorf(r.Pos(), "type %s expects %d type arguments, got %d",
				r.Name, len(t.TypeParams()), len(typeArgs))
			return nil
		}
	case *types.Enum:
		if len(t.TypeParams()) != len(typeArgs) {
			c.errorf(r.Pos(), "type %s expects %d type arguments, got %d",
				r.Name, len(t.TypeParams()), len(typeArgs))
			return nil
		}
	default:
		c.errorf(r.Pos(), "type %s is not generic", r.Name)
		return nil
	}

	// Validate type argument constraints
	c.validateConstraints(r.Pos(), typ, typeArgs)
	c.validateSendableInstance(r.Pos(), typ, typeArgs)
	c.validateSingleOwnerContainerInstance(r.Pos(), typ, typeArgs)
	c.rejectStreamTypeArg(r.Pos(), typeArgs)
	c.validateCloneInstance(r.Pos(), typ, typeArgs)

	inst := types.NewInstance(typ, typeArgs)
	c.recordInstance(inst)
	return inst
}

// resolveQualifiedType resolves a module-qualified type reference like mod.Type or mod.Type[T].
func (c *Checker) resolveQualifiedType(r *ast.QualifiedTypeRef) types.Type {
	// Look up the module object
	var scope *types.Scope
	obj := c.lookup(r.Module)
	if obj != nil {
		mod, ok := obj.(*types.Module)
		if !ok {
			c.errorf(r.Pos(), "%s is not a module", r.Module)
			return nil
		}
		scope = mod.Scope()
		if scope == nil {
			c.errorf(r.Pos(), "module '%s' has no loaded scope", r.Module)
			return nil
		}
	} else {
		c.errorf(r.Pos(), "undefined module: %s", r.Module)
		c.suggestForUndefinedModule(r.Pos(), r.Module)
		return nil
	}

	// Look up the type name in the module's scope
	member := scope.Lookup(r.Name)
	if member == nil {
		c.errorf(r.Pos(), "module '%s' has no exported member '%s'", r.Module, r.Name)
		return nil
	}

	// Check visibility
	if !isObjectExported(member) {
		c.errorf(r.Pos(), "'%s' is private to module '%s'", r.Name, r.Module)
		return nil
	}

	tn, ok := member.(*types.TypeName)
	if !ok {
		c.errorf(r.Pos(), "%s.%s is not a type", r.Module, r.Name)
		return nil
	}

	typ := tn.Type()

	// No type arguments — return directly
	if len(r.TypeArgs) == 0 {
		return typ
	}

	// Generic instantiation
	typeArgs := make([]types.Type, len(r.TypeArgs))
	for i, ta := range r.TypeArgs {
		typeArgs[i] = c.resolveType(ta)
		if typeArgs[i] == nil {
			return nil
		}
	}

	switch t := typ.(type) {
	case *types.Named:
		if len(t.TypeParams()) != len(typeArgs) {
			c.errorf(r.Pos(), "type %s.%s expects %d type arguments, got %d",
				r.Module, r.Name, len(t.TypeParams()), len(typeArgs))
			return nil
		}
	case *types.Enum:
		if len(t.TypeParams()) != len(typeArgs) {
			c.errorf(r.Pos(), "type %s.%s expects %d type arguments, got %d",
				r.Module, r.Name, len(t.TypeParams()), len(typeArgs))
			return nil
		}
	default:
		c.errorf(r.Pos(), "type %s.%s is not generic", r.Module, r.Name)
		return nil
	}

	c.validateConstraints(r.Pos(), typ, typeArgs)
	c.validateSendableInstance(r.Pos(), typ, typeArgs)
	c.validateSingleOwnerContainerInstance(r.Pos(), typ, typeArgs)
	c.rejectStreamTypeArg(r.Pos(), typeArgs)
	c.validateCloneInstance(r.Pos(), typ, typeArgs)
	inst := types.NewInstance(typ, typeArgs)
	c.recordInstance(inst)
	return inst
}

// validateConstraints checks that each type argument satisfies all of its type parameter's constraints.
func (c *Checker) validateConstraints(pos ast.Pos, origin types.Type, typeArgs []types.Type) {
	var tparams []*types.TypeParam
	switch t := origin.(type) {
	case *types.Named:
		tparams = t.TypeParams()
	case *types.Enum:
		tparams = t.TypeParams()
	default:
		return
	}
	for i, tp := range tparams {
		if len(tp.Constraints()) == 0 {
			continue
		}
		arg := typeArgs[i]
		for _, constraint := range tp.Constraints() {
			if types.AssignableTo(arg, constraint) {
				continue
			}
			if cn, ok := constraint.(*types.Named); ok && types.Implements(arg, cn) {
				continue
			}
			c.errorf(pos, "type %s does not satisfy constraint %s for type parameter %s",
				arg, constraint, tp.Obj().Name())
		}
	}
}

// resolveRefMod converts an ast.RefModifier to a types.RefMod.
func resolveRefMod(rm ast.RefModifier) types.RefMod {
	switch rm {
	case ast.RefShared:
		return types.RefShared
	case ast.RefMut:
		return types.RefMut
	default:
		return types.RefNone
	}
}

// reborrowAssignable reports whether an already-borrowed reference value
// (`T&`/`T~`) may bind to a non-reference borrow parameter of type paramType
// (T0998). A bare parameter borrows its argument, so passing an existing
// reference reborrows it — e.g. `f(v[i])` where indexing yields `T&` and `f`
// takes a bare `T`. Only applies when paramType is itself not a reference type
// (a `T~` mutable-borrow parameter still requires an exact reference match).
func reborrowAssignable(argType, paramType types.Type) bool {
	switch paramType.(type) {
	case *types.SharedRef, *types.MutRef:
		return false
	}
	var elem types.Type
	switch r := argType.(type) {
	case *types.SharedRef:
		elem = r.Elem()
	case *types.MutRef:
		elem = r.Elem()
	default:
		return false
	}
	return types.AssignableTo(elem, paramType)
}

// rejectSharedRefParam reports the removed `&` parameter form (T0998). The
// shared (read-only) borrow is the unmarked default, so `T& name` / `T &name`
// is redundant — a bare `T name` already borrows. Mutable-borrow parameters keep
// the `T~ name` spelling, so only `*types.SharedRef` param types are rejected.
func (c *Checker) rejectSharedRefParam(pos ast.Pos, name string, pt types.Type) {
	if _, ok := pt.(*types.SharedRef); ok {
		c.errorf(pos, "`&` is not a parameter marker; a plain `Type %s` parameter is already a shared (read-only) borrow — remove the `&`", name)
	}
}
