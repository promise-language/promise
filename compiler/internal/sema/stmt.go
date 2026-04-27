package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// checkStmt type-checks a statement.
func (c *Checker) checkStmt(stmt ast.Stmt) {
	if stmt == nil {
		return
	}

	switch s := stmt.(type) {
	case *ast.Block:
		c.openScope(s, "block")
		c.checkBlock(s)
		c.closeScope()

	case *ast.TypedVarDecl:
		c.checkTypedVarDecl(s)

	case *ast.InferredVarDecl:
		c.checkInferredVarDecl(s)

	case *ast.DestructureVarDecl:
		c.checkDestructureVarDecl(s)

	case *ast.AssignStmt:
		c.checkAssignStmt(s)

	case *ast.ReturnStmt:
		c.checkReturnStmt(s)

	case *ast.RaiseStmt:
		c.checkRaiseStmt(s)

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
		c.inLoop++
		c.openScope(s.Body, "loop")
		c.checkBlock(s.Body)
		c.closeScope()
		c.inLoop--

	case *ast.BreakStmt:
		if c.inLoop == 0 {
			c.errorf(s.Pos(), "break outside of loop")
		}

	case *ast.ContinueStmt:
		if c.inLoop == 0 {
			c.errorf(s.Pos(), "continue outside of loop")
		}

	case *ast.YieldStmt:
		c.checkExpr(s.Value)

	case *ast.YieldDelegateStmt:
		c.checkExpr(s.Value)

	case *ast.IncDecStmt:
		c.checkIncDecStmt(s)

	default:
		c.errorf(stmt.Pos(), "unsupported statement type")
	}
}

func (c *Checker) checkTypedVarDecl(s *ast.TypedVarDecl) {
	declType := c.resolveType(s.Type)
	if declType == nil {
		return
	}

	// Apply ref modifier wrapping
	switch s.RefMod {
	case ast.RefShared:
		declType = types.NewSharedRef(declType)
	case ast.RefMut:
		declType = types.NewMutRef(declType)
	}

	if s.Value != nil {
		valType := c.checkExpr(s.Value)
		if valType != nil && !types.AssignableTo(valType, declType) {
			c.errorf(s.Pos(), "cannot assign %s to variable of type %s", valType, declType)
		}
	}

	if s.Name != "_" {
		c.insert(types.NewVar(tpos(s.Pos()), s.Name, declType))
	}
}

func (c *Checker) checkInferredVarDecl(s *ast.InferredVarDecl) {
	valType := c.checkExpr(s.Value)
	if valType == nil {
		return
	}

	if s.Name != "_" {
		c.insert(types.NewVar(tpos(s.Pos()), s.Name, valType))
	}
}

func (c *Checker) checkDestructureVarDecl(s *ast.DestructureVarDecl) {
	valType := c.checkExpr(s.Value)
	if valType == nil {
		return
	}

	tup, ok := valType.(*types.Tuple)
	if !ok {
		c.errorf(s.Pos(), "destructuring requires tuple type, got %s", valType)
		return
	}

	if len(s.Names) != len(tup.Elems()) {
		c.errorf(s.Pos(), "destructuring expects %d values, tuple has %d elements",
			len(s.Names), len(tup.Elems()))
		return
	}

	for i, name := range s.Names {
		if name != "_" {
			c.insert(types.NewVar(tpos(s.Pos()), name, tup.Elems()[i]))
		}
	}
}

func (c *Checker) checkAssignStmt(s *ast.AssignStmt) {
	targetType := c.checkExpr(s.Target)
	valType := c.checkExpr(s.Value)

	if targetType == nil || valType == nil {
		return
	}

	// Validate setter exists when assigning to a getter property
	if me, ok := s.Target.(*ast.MemberExpr); ok {
		c.checkSetterAvailable(me)
	}

	// Validate []= exists when assigning to an index target
	if idx, ok := s.Target.(*ast.IndexExpr); ok {
		c.checkIndexAssignAvailable(idx)
	}

	// Validate [:]= exists when assigning to a slice target
	if sl, ok := s.Target.(*ast.SliceExpr); ok {
		c.checkSliceAssignAvailable(sl)
	}

	switch s.Op {
	case ast.OpAssign:
		if !types.AssignableTo(valType, targetType) {
			c.errorf(s.Pos(), "cannot assign %s to %s", valType, targetType)
		}

	case ast.OpAddAssign, ast.OpSubAssign, ast.OpMulAssign, ast.OpDivAssign, ast.OpModAssign:
		// Compound assignment: verify operator method exists
		var op string
		switch s.Op {
		case ast.OpAddAssign:
			op = "+"
		case ast.OpSubAssign:
			op = "-"
		case ast.OpMulAssign:
			op = "*"
		case ast.OpDivAssign:
			op = "/"
		case ast.OpModAssign:
			op = "%"
		}
		// Map index compound assignment: m["key"] += val operates on the
		// unwrapped value type, not the Optional returned by index access
		opTarget := targetType
		if idx, ok := s.Target.(*ast.IndexExpr); ok {
			idxTargetType := c.info.Types[idx.Target]
			if _, val, ok := types.AsMap(idxTargetType); ok {
				opTarget = val
			}
		}
		c.checkOperator(s.Pos(), opTarget, op, valType)
	}
}

// checkSetterAvailable validates that assignment to a getter property
// has a corresponding setter. Fields are always assignable.
func (c *Checker) checkSetterAvailable(me *ast.MemberExpr) {
	targetType := c.info.Types[me.Target]
	if targetType == nil {
		return
	}
	var named *types.Named
	switch t := targetType.(type) {
	case *types.Named:
		named = t
	case *types.Instance:
		if n, ok := t.Origin().(*types.Named); ok {
			named = n
		}
	}
	if named == nil {
		return
	}
	// If the member is a field, assignment is always OK
	if named.LookupField(me.Field) != nil {
		return
	}
	// If the member is a getter, check for a corresponding setter
	if g := named.LookupGetter(me.Field); g != nil {
		if named.LookupSetter(me.Field) == nil {
			c.errorf(me.Pos(), "property '%s' has no setter", me.Field)
		}
	}
}

// checkIndexAssignAvailable validates that the target type has a []= method
// when assigning to an index expression. For example, string has [] but not []=,
// so `str[0] = 'a'` should be rejected.
func (c *Checker) checkIndexAssignAvailable(idx *ast.IndexExpr) {
	targetType := c.info.Types[idx.Target]
	if targetType == nil {
		return
	}
	var named *types.Named
	switch t := targetType.(type) {
	case *types.Named:
		named = t
	case *types.Instance:
		if n, ok := t.Origin().(*types.Named); ok {
			named = n
		}
	}
	if named == nil {
		return
	}
	if named.LookupMethod("[]=") == nil {
		c.errorf(idx.Pos(), "type %s does not support index assignment", targetType)
	}
}

// checkSliceAssignAvailable validates that the target type has a [:]= method
// when assigning to a slice expression.
func (c *Checker) checkSliceAssignAvailable(sl *ast.SliceExpr) {
	targetType := c.info.Types[sl.Target]
	if targetType == nil {
		return
	}
	var named *types.Named
	switch t := targetType.(type) {
	case *types.Named:
		named = t
	case *types.Instance:
		if n, ok := t.Origin().(*types.Named); ok {
			named = n
		}
	}
	if named == nil {
		return
	}
	if named.LookupMethod("[:]=") == nil {
		c.errorf(sl.Pos(), "type %s does not support slice assignment", targetType)
	}
}

func (c *Checker) checkReturnStmt(s *ast.ReturnStmt) {
	if c.curFunc == nil {
		c.errorf(s.Pos(), "return outside of function")
		return
	}

	if s.Value == nil {
		// Bare return — function must return void
		if c.curFunc.Result() != nil && !types.Identical(c.curFunc.Result(), types.TypVoid) {
			c.errorf(s.Pos(), "missing return value (expected %s)", c.curFunc.Result())
		}
		return
	}

	valType := c.checkExpr(s.Value)
	if valType == nil {
		return
	}

	expected := c.curFunc.Result()
	if expected == nil {
		c.errorf(s.Pos(), "function does not return a value")
		return
	}

	if !types.AssignableTo(valType, expected) {
		c.errorf(s.Pos(), "cannot return %s from function returning %s", valType, expected)
	}
}

func (c *Checker) checkRaiseStmt(s *ast.RaiseStmt) {
	if c.curFunc == nil || !c.curFunc.CanError() {
		c.errorf(s.Pos(), "raise outside of failable function")
	}
	c.checkExpr(s.Value)
}

func (c *Checker) checkIfStmt(s *ast.IfStmt) {
	if s.Binding != "" {
		// If-unwrap: if val := expr { }
		c.openScope(s.Body, "if-unwrap")
		initType := c.checkExpr(s.Init)
		if initType != nil {
			opt, ok := initType.(*types.Optional)
			if !ok {
				c.errorf(s.Init.Pos(), "if-unwrap requires optional type, got %s", initType)
				c.insert(types.NewVar(tpos(s.Pos()), s.Binding, initType))
			} else {
				c.insert(types.NewVar(tpos(s.Pos()), s.Binding, opt.Elem()))
			}
		}
		c.checkBlock(s.Body)
		c.closeScope()
	} else {
		// Regular if
		cond := c.checkExpr(s.Cond)
		if cond != nil && !types.Identical(cond, types.TypBool) {
			c.errorf(s.Cond.Pos(), "if condition must be bool, got %s", cond)
		}
		c.openScope(s.Body, "if-then")
		c.checkBlock(s.Body)
		c.closeScope()
	}

	// Check else branch
	if s.Else != nil {
		c.checkStmt(s.Else)
	}
}

func (c *Checker) checkWhileStmt(s *ast.WhileStmt) {
	cond := c.checkExpr(s.Cond)
	if cond != nil && !types.Identical(cond, types.TypBool) {
		c.errorf(s.Cond.Pos(), "while condition must be bool, got %s", cond)
	}

	c.inLoop++
	c.openScope(s.Body, "while")
	c.checkBlock(s.Body)
	c.closeScope()
	c.inLoop--
}

func (c *Checker) checkWhileUnwrapStmt(s *ast.WhileUnwrapStmt) {
	valType := c.checkExpr(s.Value)

	c.inLoop++
	c.openScope(s.Body, "while-unwrap")

	if valType != nil {
		opt, ok := valType.(*types.Optional)
		if !ok {
			c.errorf(s.Value.Pos(), "while-unwrap requires optional type, got %s", valType)
			c.insert(types.NewVar(tpos(s.Pos()), s.Binding, valType))
		} else {
			c.insert(types.NewVar(tpos(s.Pos()), s.Binding, opt.Elem()))
		}
	}

	c.checkBlock(s.Body)
	c.closeScope()
	c.inLoop--
}

func (c *Checker) checkForInStmt(s *ast.ForInStmt) {
	iterType := c.checkExpr(s.Iterable)

	c.inLoop++
	c.openScope(s.Body, "for-in")

	if iterType != nil {
		// Determine element type from iterable
		var elemType types.Type
		if elem, ok := types.AsSlice(iterType); ok {
			elemType = elem
		} else if key, val, ok := types.AsMap(iterType); ok {
			// Iterating a map yields (key, value) tuples
			elemType = types.NewTuple([]types.Type{key, val})
		} else if inst, ok := iterType.(*types.Instance); ok {
			// iter[T] yields T, stream[T] yields T
			origin := inst.Origin()
			if origin == types.TypIter || origin == types.TypStream {
				if len(inst.TypeArgs()) > 0 {
					elemType = inst.TypeArgs()[0]
				} else {
					elemType = iterType
				}
			} else {
				c.errorf(s.Iterable.Pos(), "cannot iterate over type %s", iterType)
				elemType = iterType
			}
		} else {
			if types.Identical(iterType, types.TypRange) {
				elemType = types.TypInt
			} else if types.Identical(iterType, types.TypString) {
				elemType = types.TypChar
			} else {
				c.errorf(s.Iterable.Pos(), "cannot iterate over type %s", iterType)
				elemType = iterType
			}
		}

		if s.Binding != "_" {
			c.insert(types.NewVar(tpos(s.Pos()), s.Binding, elemType))
		}
		if s.Index != "" && s.Index != "_" {
			c.insert(types.NewVar(tpos(s.Pos()), s.Index, types.TypInt))
		}
	}

	c.checkBlock(s.Body)
	c.closeScope()
	c.inLoop--
}

func (c *Checker) checkClassicForStmt(s *ast.ClassicForStmt) {
	c.inLoop++
	c.openScope(s.Body, "for")

	// Init variable
	if s.InitType != nil {
		initType := c.resolveType(s.InitType)
		if initType != nil {
			valType := c.checkExpr(s.InitValue)
			if valType != nil && !types.AssignableTo(valType, initType) {
				c.errorf(s.Pos(), "cannot assign %s to variable of type %s", valType, initType)
			}
			c.insert(types.NewVar(tpos(s.Pos()), s.InitName, initType))
		}
	} else {
		// Inferred type
		valType := c.checkExpr(s.InitValue)
		if valType != nil {
			c.insert(types.NewVar(tpos(s.Pos()), s.InitName, valType))
		}
	}

	// Condition
	if s.Cond != nil {
		cond := c.checkExpr(s.Cond)
		if cond != nil && !types.Identical(cond, types.TypBool) {
			c.errorf(s.Cond.Pos(), "for condition must be bool, got %s", cond)
		}
	}

	// Update
	if s.UpdateIncDec {
		targetType := c.checkExpr(s.UpdateTarget)
		if targetType != nil {
			op := "++"
			if !s.UpdateIsInc {
				op = "--"
			}
			c.checkUnaryOperator(s.Pos(), targetType, op)
		}
	} else if s.UpdateTarget != nil {
		c.checkExpr(s.UpdateTarget)
		c.checkExpr(s.UpdateValue)
	} else if s.UpdateValue != nil {
		c.checkExpr(s.UpdateValue)
	}

	c.checkBlock(s.Body)
	c.closeScope()
	c.inLoop--
}

func (c *Checker) checkIncDecStmt(s *ast.IncDecStmt) {
	targetType := c.checkExpr(s.Target)
	if targetType == nil {
		return
	}
	op := "++"
	if !s.IsInc {
		op = "--"
	}
	c.checkUnaryOperator(s.Pos(), targetType, op)
}
