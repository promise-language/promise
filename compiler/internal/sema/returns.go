package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// checkMissingReturn verifies that all non-void functions/methods return a value
// on every code path. Called after pass 3 type-checking is complete.
// Uses already-resolved signatures from pass 2 rather than re-resolving types.
func (c *Checker) checkMissingReturn(file *ast.File) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			obj := c.lookup(d.Name)
			if obj == nil {
				continue
			}
			fn, ok := obj.(*types.Func)
			if !ok {
				continue
			}
			sig, ok := fn.Type().(*types.Signature)
			if !ok || sig == nil {
				continue
			}
			if sig.Result() == nil || types.Identical(sig.Result(), types.TypVoid) {
				continue
			}
			if !c.blockReturns(d.Body) {
				c.errorf(d.End(), "function %s missing return statement", d.Name)
			}

		case *ast.TypeDecl:
			obj := c.lookup(d.Name)
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
			for _, md := range d.Methods {
				if md.Body == nil {
					continue
				}
				m := named.LookupMethod(md.Name)
				if m == nil || m.Sig() == nil {
					continue
				}
				if m.Sig().Result() == nil || types.Identical(m.Sig().Result(), types.TypVoid) {
					continue
				}
				if !c.blockReturns(md.Body) {
					c.errorf(md.End(), "method %s.%s missing return statement", d.Name, md.Name)
				}
			}
		}
	}
}

// blockReturns reports whether a block definitely returns on all paths.
func (c *Checker) blockReturns(block *ast.Block) bool {
	if block == nil || len(block.Stmts) == 0 {
		return false
	}
	return c.stmtReturns(block.Stmts[len(block.Stmts)-1])
}

// stmtReturns reports whether a statement definitely returns on all paths.
func (c *Checker) stmtReturns(stmt ast.Stmt) bool {
	switch s := stmt.(type) {
	case *ast.ReturnStmt:
		return true

	case *ast.RaiseStmt:
		return true

	case *ast.Block:
		return c.blockReturns(s)

	case *ast.IfStmt:
		if s.Else == nil {
			return false // if without else doesn't guarantee return
		}
		return c.blockReturns(s.Body) && c.stmtReturns(s.Else)

	case *ast.ExprStmt:
		return c.exprReturns(s.Expr)

	case *ast.InfiniteLoop:
		// An infinite for{} without break is considered to "return"
		// since it never falls through. Conservative: only if body has no break.
		return !c.blockHasBreak(s.Body)

	default:
		return false
	}
}

// exprReturns checks if an expression used as a statement guarantees a return.
// This covers match expressions where all arms return AND the match is exhaustive.
func (c *Checker) exprReturns(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.MatchExpr:
		if len(e.Arms) == 0 {
			return false
		}
		for _, arm := range e.Arms {
			if arm.Block != nil {
				if !c.blockReturns(arm.Block) {
					return false
				}
			} else {
				return false // expression body arms don't return
			}
		}
		// All arms return, but only counts if the match is exhaustive
		subjectType := c.info.Types[e.Subject]
		return c.matchIsExhaustive(e, subjectType)
	default:
		return false
	}
}

// blockHasBreak reports whether a block contains a break statement that
// would break out of an enclosing loop (not a nested loop's break).
func (c *Checker) blockHasBreak(block *ast.Block) bool {
	if block == nil {
		return false
	}
	for _, stmt := range block.Stmts {
		if c.stmtHasBreak(stmt) {
			return true
		}
	}
	return false
}

// stmtHasBreak reports whether a statement contains a break that applies
// to an enclosing loop. Does NOT recurse into nested loops since their
// breaks only apply to the inner loop.
func (c *Checker) stmtHasBreak(stmt ast.Stmt) bool {
	if stmt == nil {
		return false
	}
	switch s := stmt.(type) {
	case *ast.BreakStmt:
		return true
	case *ast.Block:
		return c.blockHasBreak(s)
	case *ast.IfStmt:
		if c.blockHasBreak(s.Body) {
			return true
		}
		return c.stmtHasBreak(s.Else)
	case *ast.ExprStmt:
		if me, ok := s.Expr.(*ast.MatchExpr); ok {
			for _, arm := range me.Arms {
				if arm.Block != nil && c.blockHasBreak(arm.Block) {
					return true
				}
			}
		}
		return false
	// Note: do NOT recurse into WhileStmt, ForInStmt, ClassicForStmt,
	// InfiniteLoop — break inside those only breaks the inner loop.
	default:
		return false
	}
}
