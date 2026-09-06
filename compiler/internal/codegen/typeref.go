package codegen

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// lookupLocalType resolves the declared type for a TypedVarDecl.
// It checks the TypeRef AST node to detect Optional declarations,
// then resolves the type by looking up the variable in sema scopes.
func (c *Compiler) lookupLocalType(s *ast.TypedVarDecl) types.Type {
	// Only need special handling for Optional declarations
	optRef, ok := s.Type.(*ast.OptionalTypeRef)
	if !ok {
		return nil // use expression type
	}
	// Always resolve the declared type from the AST so nested OptionalTypeRef
	// (T??, T???) preserves its full depth even when the value expr is itself
	// Optional (e.g. `T?? b = a` where a:T?). Using exprType here would collapse
	// the alloca to T?, mismatching sema and breaking unwraps.
	if t := c.resolveTypeRefToType(optRef); t != nil {
		return t
	}
	return c.lookupVarType(s.Name)
}

// resolveTypeRefToType returns the type of an AST TypeRef.
//
// It reads sema's recorded resolution (sema.Info.TypeRefs) rather than walking
// the type syntax a second time — before T1667 this was an independent copy of
// sema.resolveType that had to be edited in lockstep with it (T1634 had to make
// the same two edits in both, and a missed edit produces a sema type and a
// codegen type that disagree on failability: a wrong LLVM result shape at an
// indirect call, silent until codegen crashes). The recorded type is in terms of
// the declaring context, so it can still carry that context's type params or its
// interface-as-Self; the ambient mono/Self substitutions are applied here, in the
// same order and with the same meaning as at every other sema-type read site in
// codegen.
//
// c.info is the unit that type-checked the node: codegen already swaps it to a
// module's Info while compiling that module, and useDeclaringInfo (T1395) swaps
// it for a parameter-default subtree spliced in from the declaring unit. So the
// active Info is always the one holding the record — no cross-Info search.
//
// Returns nil when sema recorded nothing — the same contract the callers already
// handle.
func (c *Compiler) resolveTypeRefToType(ref ast.TypeRef) types.Type {
	if ref == nil {
		return nil
	}
	typ, ok := c.info.TypeRefs[ref]
	if !ok {
		return nil
	}
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	if c.selfSubst != nil {
		typ = types.SubstituteSelf(typ, c.selfSubst.iface, c.selfSubst.concrete)
	}
	return typ
}

// lookupVarType finds a variable's declared type by walking sema scopes.
func (c *Compiler) lookupVarType(name string) types.Type {
	for _, scope := range c.info.ScopeOrder {
		if obj := scope.Lookup(name); obj != nil {
			if v, ok := obj.(*types.Var); ok {
				typ := v.Type()
				if c.typeSubst != nil {
					typ = types.Substitute(typ, c.typeSubst)
				}
				return typ
			}
		}
	}
	return nil
}
