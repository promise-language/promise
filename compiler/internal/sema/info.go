package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// Info holds the results of semantic analysis.
// All maps use AST nodes as keys — the AST itself is not modified.
type Info struct {
	// Types maps each expression to its resolved type.
	Types map[ast.Expr]types.Type

	// Objects maps each identifier expression to the object it refers to.
	Objects map[*ast.IdentExpr]types.Object

	// Scopes maps scope-creating AST nodes (File, Block, etc.) to their scope.
	Scopes map[ast.Node]*types.Scope

	// Instances records all concrete generic instantiations for later monomorphization.
	Instances []*types.Instance
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
