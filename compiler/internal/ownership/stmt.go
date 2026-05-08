package ownership

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// checkBlock walks all statements in a block sequentially.
// After each statement, call-scoped borrows are expired, and NLL borrow
// narrowing (T0164) expires variable-scoped borrows whose borrower's last
// use was this statement.
func (c *Checker) checkBlock(block *ast.Block) {
	if block == nil {
		return
	}
	for _, stmt := range block.Stmts {
		c.checkStmt(stmt)
		if c.borrows != nil {
			c.borrows.ExpireCallScoped()
			// T0164: NLL borrow narrowing — expire borrows whose borrower
			// variable's last use was this statement.
			if names, ok := c.refLastUses[stmt]; ok {
				for _, name := range names {
					c.borrows.ExpireBorrower(name)
				}
			}
		}
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

	case *ast.UseVarDecl:
		c.checkExpr(s.Value)
		c.tryMove(s.Value)
		if s.Name != "_" {
			c.state[s.Name] = Owned
			c.pinned[s.Name] = true
			if typ := c.info.Types[s.Value]; typ != nil {
				c.trackDeclOrder(s.Name, typ)
			}
		}

	case *ast.AssignStmt:
		c.checkAssignStmt(s)

	case *ast.ReturnStmt:
		if s.Value != nil {
			c.checkExpr(s.Value)
			c.tryMove(s.Value)
			c.checkReturnRefSafety(s)
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

	case *ast.IncDecStmt:
		c.checkExpr(s.Target)

	case *ast.SelectStmt:
		c.checkSelectStmt(s)

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
		c.promoteCallBorrows(s.Name, s.Value)
		if typ := c.info.Types[s.Value]; typ != nil {
			c.trackDeclOrder(s.Name, typ)
		}
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
		c.promoteCallBorrows(s.Name, s.Value)
		if typ := c.info.Types[s.Value]; typ != nil {
			c.trackDeclOrder(s.Name, typ)
		}
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

// promoteCallBorrows promotes pending call-scoped borrows to variable-scoped
// when a function returning a reference type stores its result in a variable.
func (c *Checker) promoteCallBorrows(varName string, value ast.Expr) {
	if value == nil || c.borrows == nil {
		return
	}
	typ := c.info.Types[value]
	if !isRefType(typ) {
		return
	}
	for _, b := range c.borrows.borrows {
		if b.Borrower == "" {
			b.Borrower = varName
		}
	}
}

// isRefType returns true if the type is a reference type (SharedRef or MutRef).
func isRefType(t types.Type) bool {
	if t == nil {
		return false
	}
	switch t.(type) {
	case *types.SharedRef, *types.MutRef:
		return true
	}
	return false
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
			// Cannot reassign a variable that is actively borrowed by others
			if c.borrows != nil && c.borrows.HasAnyBorrow(ident.Name) {
				c.errorf(s.Pos(), "cannot assign to '%s' while it is borrowed", ident.Name)
			}
			// Expire borrows where this variable is the borrower
			if c.borrows != nil {
				c.borrows.ExpireBorrower(ident.Name)
			}
			if _, tracked := c.state[ident.Name]; tracked {
				c.state[ident.Name] = Owned
			}
			// Promote call-scoped borrows if the RHS returns a ref type
			c.promoteCallBorrows(ident.Name, s.Value)
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
	case *ast.SliceExpr:
		c.checkExpr(e.Target)
		if e.Low != nil {
			c.checkExpr(e.Low)
		}
		if e.High != nil {
			c.checkExpr(e.High)
		}
	}
}

// checkReturnRefSafety validates that returned references don't point to locals.
func (c *Checker) checkReturnRefSafety(s *ast.ReturnStmt) {
	if c.curSig == nil || c.curSig.Result() == nil {
		return
	}
	if !isRefType(c.curSig.Result()) {
		return
	}
	ident, ok := s.Value.(*ast.IdentExpr)
	if !ok {
		return
	}
	// A variable is local if it's not a parameter of the current function.
	if c.params != nil && !c.params[ident.Name] {
		c.errorf(s.Pos(), "cannot return reference to local variable '%s'", ident.Name)
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
	// Expire call-scoped borrows from the condition/init expression so the
	// then/else branches can re-borrow the same variables.
	if c.borrows != nil {
		c.borrows.ExpireCallScoped()
	}

	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	if s.Binding != "" && s.Binding != "_" {
		c.state[s.Binding] = Owned
	}
	c.checkBlock(s.Body)
	thenState := c.state
	thenBorrows := c.borrows

	if s.Else != nil {
		c.state = savedState.clone()
		c.borrows = savedBorrows.Clone()
		c.checkStmt(s.Else)
		elseState := c.state
		elseBorrows := c.borrows
		c.state = merge(thenState, elseState)
		c.borrows = MergeBorrowSets(thenBorrows, elseBorrows)
	} else {
		// No else: conservative merge with pre-if state.
		c.state = merge(savedState, thenState)
		c.borrows = MergeBorrowSets(savedBorrows, thenBorrows)
	}
}

func (c *Checker) checkWhileStmt(s *ast.WhileStmt) {
	c.checkExpr(s.Cond)
	// Expire call-scoped borrows from the condition so the loop body can
	// re-borrow the same variables.
	if c.borrows != nil {
		c.borrows.ExpireCallScoped()
	}
	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	c.checkBlock(s.Body)
	c.state = merge(savedState, c.state)
	c.borrows = MergeBorrowSets(savedBorrows, c.borrows)
}

func (c *Checker) checkWhileUnwrapStmt(s *ast.WhileUnwrapStmt) {
	c.checkExpr(s.Value)
	// Expire call-scoped borrows from the condition expression so the loop
	// body can re-borrow the same variables (B0004).
	if c.borrows != nil {
		c.borrows.ExpireCallScoped()
	}
	if s.Binding != "" && s.Binding != "_" {
		c.state[s.Binding] = Owned
	}
	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	c.checkBlock(s.Body)
	c.state = merge(savedState, c.state)
	c.borrows = MergeBorrowSets(savedBorrows, c.borrows)
}

func (c *Checker) checkForInStmt(s *ast.ForInStmt) {
	c.checkExpr(s.Iterable)
	c.tryMove(s.Iterable)
	// Expire call-scoped borrows from the iterable expression so the loop
	// body can re-borrow the same variables.
	if c.borrows != nil {
		c.borrows.ExpireCallScoped()
	}
	if s.Binding != "_" {
		c.state[s.Binding] = Owned
	}
	if s.Index != "" && s.Index != "_" {
		c.state[s.Index] = Owned
	}
	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	c.checkBlock(s.Body)
	c.state = merge(savedState, c.state)
	c.borrows = MergeBorrowSets(savedBorrows, c.borrows)
}

func (c *Checker) checkClassicForStmt(s *ast.ClassicForStmt) {
	if s.InitValue != nil {
		c.checkExpr(s.InitValue)
		c.tryMove(s.InitValue)
	}
	if s.InitName != "" && s.InitName != "_" {
		c.state[s.InitName] = Owned
	}
	// Expire call-scoped borrows from the init expression.
	if c.borrows != nil {
		c.borrows.ExpireCallScoped()
	}

	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	if s.Cond != nil {
		c.checkExpr(s.Cond)
	}
	// Expire call-scoped borrows from the condition so the loop body can
	// re-borrow the same variables.
	if c.borrows != nil {
		c.borrows.ExpireCallScoped()
	}
	c.checkBlock(s.Body)
	if s.UpdateIncDec {
		if s.UpdateTarget != nil {
			c.checkExpr(s.UpdateTarget)
		}
	} else if s.UpdateValue != nil {
		c.checkExpr(s.UpdateValue)
	}
	c.state = merge(savedState, c.state)
	c.borrows = MergeBorrowSets(savedBorrows, c.borrows)
}

func (c *Checker) checkSelectStmt(s *ast.SelectStmt) {
	// Each case channel expression is checked; at most one case executes.
	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()

	var states []StateMap
	var borrowSets []*BorrowSet

	for _, sc := range s.Cases {
		c.state = savedState.clone()
		c.borrows = savedBorrows.Clone()
		c.checkExpr(sc.Channel)
		if sc.IsSend && sc.SendValue != nil {
			c.checkExpr(sc.SendValue)
			c.tryMove(sc.SendValue)
		}
		// Expire call-scoped borrows from channel/send expressions so the case
		// body can re-borrow the same variables (B0103).
		if c.borrows != nil {
			c.borrows.ExpireCallScoped()
		}
		if !sc.IsSend && sc.Binding != "_" {
			c.state[sc.Binding] = Owned
		}
		for _, stmt := range sc.Body {
			c.checkStmt(stmt)
			if c.borrows != nil {
				c.borrows.ExpireCallScoped()
			}
		}
		states = append(states, c.state)
		borrowSets = append(borrowSets, c.borrows)
	}

	if s.Default != nil {
		c.state = savedState.clone()
		c.borrows = savedBorrows.Clone()
		for _, stmt := range s.Default {
			c.checkStmt(stmt)
			if c.borrows != nil {
				c.borrows.ExpireCallScoped()
			}
		}
		states = append(states, c.state)
		borrowSets = append(borrowSets, c.borrows)
	}

	// Merge all branches
	if len(states) > 0 {
		merged := states[0]
		mergedBorrows := borrowSets[0]
		for i := 1; i < len(states); i++ {
			merged = merge(merged, states[i])
			mergedBorrows = MergeBorrowSets(mergedBorrows, borrowSets[i])
		}
		// Also merge with pre-select state (select might not execute any case if no default)
		if s.Default == nil {
			merged = merge(savedState, merged)
			mergedBorrows = MergeBorrowSets(savedBorrows, mergedBorrows)
		}
		c.state = merged
		c.borrows = mergedBorrows
	} else {
		c.state = savedState
		c.borrows = savedBorrows
	}
}

func (c *Checker) checkInfiniteLoop(s *ast.InfiniteLoop) {
	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	c.checkBlock(s.Body)
	c.state = merge(savedState, c.state)
	c.borrows = MergeBorrowSets(savedBorrows, c.borrows)
}
