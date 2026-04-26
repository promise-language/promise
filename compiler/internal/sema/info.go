package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// FuncInstance records a concrete instantiation of a generic function.
type FuncInstance struct {
	Func     *types.Func      // the generic function
	TypeArgs []types.Type     // concrete type arguments
	Sig      *types.Signature // substituted signature (no TypeParams)
}

// Info holds the results of semantic analysis.
// All maps use AST nodes as keys — the AST itself is not modified.
type Info struct {
	// Types maps each expression to its resolved type.
	Types map[ast.Expr]types.Type

	// Objects maps each identifier expression to the object it refers to.
	Objects map[*ast.IdentExpr]types.Object

	// Scopes maps scope-creating AST nodes (File, Block, etc.) to their scope.
	Scopes map[ast.Node]*types.Scope

	// Instances records all concrete generic type instantiations for later monomorphization.
	Instances []*types.Instance

	// FuncInstances records all concrete generic function instantiations for later monomorphization.
	FuncInstances []*FuncInstance

	// Tests records functions annotated with `test.
	Tests []*types.Func
}

// recordType stores the resolved type for an expression.
func (c *Checker) recordType(expr ast.Expr, typ types.Type) {
	if typ != nil {
		c.info.Types[expr] = typ
	}
}

// recordObject stores the resolved object for an identifier expression.
func (c *Checker) recordObject(ident *ast.IdentExpr, obj types.Object) {
	if obj != nil {
		c.info.Objects[ident] = obj
	}
}

// recordInstance records a concrete generic instantiation for later monomorphization.
// Non-concrete instances (containing TypeParams from type definitions) are skipped.
func (c *Checker) recordInstance(inst *types.Instance) {
	for _, arg := range inst.TypeArgs() {
		if types.ContainsTypeParam(arg) {
			return
		}
	}
	c.info.Instances = append(c.info.Instances, inst)
}
