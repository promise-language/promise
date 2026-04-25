package sema

import (
	"strconv"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
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
			result = c.resolveType(r.Return)
			if result == nil {
				return nil
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
		return types.NewSlice(elem)

	case *ast.ArrayTypeRef:
		elem := c.resolveType(r.Element)
		if elem == nil {
			return nil
		}
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
	obj := c.lookup(r.Name)
	if obj == nil {
		c.errorf(r.Pos(), "undefined type: %s", r.Name)
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

	// Special case: Map[K, V] → structural Map type for consistency with map literals
	if named, ok := typ.(*types.Named); ok && named == types.TypMap {
		return types.NewMap(typeArgs[0], typeArgs[1])
	}

	// Validate type argument constraints
	c.validateConstraints(r.Pos(), typ, typeArgs)

	inst := types.NewInstance(typ, typeArgs)
	c.info.Instances = append(c.info.Instances, inst)
	return inst
}

// validateConstraints checks that each type argument satisfies its type parameter's constraint.
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
		if tp.Constraint() == nil {
			continue
		}
		arg := typeArgs[i]
		constraint := tp.Constraint()
		// Check if the type arg satisfies the constraint
		if types.AssignableTo(arg, constraint) {
			continue
		}
		// Check interface implementation
		if cn, ok := constraint.(*types.Named); ok && types.Implements(arg, cn) {
			continue
		}
		c.errorf(pos, "type %s does not satisfy constraint %s for type parameter %s",
			arg, constraint, tp.Obj().Name())
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
