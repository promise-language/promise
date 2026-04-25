package ownership

import "djabi.dev/go/promise_lang/internal/ast"

// checkBlock walks all statements in a block sequentially.
func (c *Checker) checkBlock(block *ast.Block) {
	if block == nil {
		return
	}
	for _, stmt := range block.Stmts {
		c.checkStmt(stmt)
	}
}

// checkStmt dispatches ownership analysis on a single statement.
func (c *Checker) checkStmt(stmt ast.Stmt) {
	if stmt == nil {
		return
	}
	switch s := stmt.(type) {
	case *ast.Block:
		c.checkBlock(s)

	case *ast.TypedVarDecl:
		c.checkTypedVarDecl(s)

	case *ast.InferredVarDecl:
		c.checkInferredVarDecl(s)

	case *ast.DestructureVarDecl:
		c.checkDestructureVarDecl(s)

	case *ast.AssignStmt:
		c.checkAssignStmt(s)

	case *ast.ReturnStmt:
		if s.Value != nil {
			c.checkExpr(s.Value)
			c.tryMove(s.Value)
		}

	case *ast.RaiseStmt:
		c.checkExpr(s.Value)
		c.tryMove(s.Value)

	case *ast.ExprStmt:
		c.checkExpr(s.Expr)

	case *ast.IfStmt:
		c.checkIfStmt(s)

	case *ast.WhileStmt:
		c.checkWhileStmt(s)

	case *ast.WhileUnwrapStmt:
		c.checkWhileUnwrapStmt(s)

	case *ast.ForInStmt:
		c.checkForInStmt(s)

	case *ast.ClassicForStmt:
		c.checkClassicForStmt(s)

	case *ast.InfiniteLoop:
		c.checkInfiniteLoop(s)

	case *ast.YieldStmt:
		c.checkExpr(s.Value)
		c.tryMove(s.Value)

	case *ast.YieldDelegateStmt:
		c.checkExpr(s.Value)
		c.tryMove(s.Value)

	case *ast.BreakStmt, *ast.ContinueStmt:
		// no ownership effect
	}
}

func (c *Checker) checkTypedVarDecl(s *ast.TypedVarDecl) {
	if s.Value != nil {
		c.checkExpr(s.Value)
		c.tryMove(s.Value)
	}
	if s.Name != "_" {
		c.state[s.Name] = Owned
	}
	// Raw pointer types are only allowed inside unsafe blocks.
	if c.inUnsafe == 0 && isPointerTypeRef(s.Type) {
		c.errorf(s.Pos(), "raw pointer type used outside of unsafe block")
	}
}

// isPointerTypeRef checks whether a type reference is a raw pointer type.
func isPointerTypeRef(tr ast.TypeRef) bool {
	_, ok := tr.(*ast.PointerTypeRef)
	return ok
}

func (c *Checker) checkInferredVarDecl(s *ast.InferredVarDecl) {
	c.checkExpr(s.Value)
	c.tryMove(s.Value)
	if s.Name != "_" {
		c.state[s.Name] = Owned
	}
}

func (c *Checker) checkDestructureVarDecl(s *ast.DestructureVarDecl) {
	c.checkExpr(s.Value)
	c.tryMove(s.Value)
	for _, name := range s.Names {
		if name != "_" {
			c.state[name] = Owned
		}
	}
}

func (c *Checker) checkAssignStmt(s *ast.AssignStmt) {
	if s.Op != ast.OpAssign {
		// Compound assignment (+=, -=, etc.) reads the target.
		c.checkExpr(s.Target)
	} else {
		// Simple assignment: check sub-expressions of the target (member/index
		// receivers) but not the target variable itself since we're writing to it.
		c.checkAssignTarget(s.Target)
	}

	c.checkExpr(s.Value)
	c.tryMove(s.Value)

	// Simple assignment resurrects the target variable.
	if s.Op == ast.OpAssign {
		if ident, ok := s.Target.(*ast.IdentExpr); ok {
			if _, tracked := c.state[ident.Name]; tracked {
				c.state[ident.Name] = Owned
			}
		}
	}
}

// checkAssignTarget checks sub-expressions of an assignment target for
// use-after-move, without checking the final target variable itself.
func (c *Checker) checkAssignTarget(target ast.Expr) {
	switch e := target.(type) {
	case *ast.IdentExpr:
		// Don't check — we're assigning TO this variable.
	case *ast.MemberExpr:
		c.checkExpr(e.Target)
	case *ast.IndexExpr:
		c.checkExpr(e.Target)
		c.checkExpr(e.Index)
	}
}

// --- Control flow statements ---

func (c *Checker) checkIfStmt(s *ast.IfStmt) {
	if s.Binding != "" {
		// if-unwrap: if val := expr { }
		c.checkExpr(s.Init)
		c.tryMove(s.Init)
	} else {
		c.checkExpr(s.Cond)
	}

	saved := c.state.clone()
	if s.Binding != "" && s.Binding != "_" {
		c.state[s.Binding] = Owned
	}
	c.checkBlock(s.Body)
	thenState := c.state

	if s.Else != nil {
		c.state = saved.clone()
		c.checkStmt(s.Else)
		elseState := c.state
		c.state = merge(thenState, elseState)
	} else {
		// No else: conservative merge with pre-if state.
		c.state = merge(saved, thenState)
	}
}

func (c *Checker) checkWhileStmt(s *ast.WhileStmt) {
	c.checkExpr(s.Cond)
	saved := c.state.clone()
	c.checkBlock(s.Body)
	c.state = merge(saved, c.state)
}

func (c *Checker) checkWhileUnwrapStmt(s *ast.WhileUnwrapStmt) {
	c.checkExpr(s.Value)
	if s.Binding != "" && s.Binding != "_" {
		c.state[s.Binding] = Owned
	}
	saved := c.state.clone()
	c.checkBlock(s.Body)
	c.state = merge(saved, c.state)
}

func (c *Checker) checkForInStmt(s *ast.ForInStmt) {
	c.checkExpr(s.Iterable)
	c.tryMove(s.Iterable)
	if s.Binding != "_" {
		c.state[s.Binding] = Owned
	}
	if s.Index != "" && s.Index != "_" {
		c.state[s.Index] = Owned
	}
	saved := c.state.clone()
	c.checkBlock(s.Body)
	c.state = merge(saved, c.state)
}

func (c *Checker) checkClassicForStmt(s *ast.ClassicForStmt) {
	if s.InitValue != nil {
		c.checkExpr(s.InitValue)
		c.tryMove(s.InitValue)
	}
	if s.InitName != "" && s.InitName != "_" {
		c.state[s.InitName] = Owned
	}

	saved := c.state.clone()
	if s.Cond != nil {
		c.checkExpr(s.Cond)
	}
	c.checkBlock(s.Body)
	if s.UpdateValue != nil {
		c.checkExpr(s.UpdateValue)
	}
	c.state = merge(saved, c.state)
}

func (c *Checker) checkInfiniteLoop(s *ast.InfiniteLoop) {
	saved := c.state.clone()
	c.checkBlock(s.Body)
	c.state = merge(saved, c.state)
}
