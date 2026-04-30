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

	case *ast.UseVarDecl:
		c.checkUseVarDecl(s)

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

	case *ast.SelectStmt:
		c.checkSelectStmt(s)

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

	// Uninitialized declaration: `T? x;` defaults to none
	if s.Value == nil {
		if _, ok := declType.(*types.Optional); !ok {
			c.errorf(s.Pos(), "uninitialized variable %s requires optional type, got %s", s.Name, declType)
		}
		if s.Name != "_" {
			c.checkNoShadow(s.Name, s.Pos())
			c.insert(types.NewVar(tpos(s.Pos()), s.Name, declType))
		}
		return
	}

	if s.Value != nil {
		// Special case: empty collection literals with typed declarations.
		// Infer the element type from the declared type instead of erroring.
		emptyLitHandled := false
		if arrLit, ok := s.Value.(*ast.ArrayLit); ok && len(arrLit.Elements) == 0 {
			if _, ok := types.AsVector(declType); ok {
				c.recordType(s.Value, declType)
				emptyLitHandled = true
			}
		}
		if mapLit, ok := s.Value.(*ast.MapLit); ok && len(mapLit.Entries) == 0 {
			if _, _, ok := types.AsMap(declType); ok {
				c.recordType(s.Value, declType)
				emptyLitHandled = true
			}
		}
		if !emptyLitHandled {
			valType := c.checkExprWithHint(s.Value, declType)
			if valType != nil && !types.AssignableTo(valType, declType) {
				c.errorf(s.Pos(), "cannot assign %s to variable of type %s", valType, declType)
			}
			// Track factory-created locals for `final field write restriction
			if c.inFactoryBody && s.Name != "_" && isConstructorCallExpr(s.Value) {
				c.factoryLocals[s.Name] = true
			}
		}
	}

	if s.Name != "_" {
		c.checkNoShadow(s.Name, s.Pos())
		c.insert(types.NewVar(tpos(s.Pos()), s.Name, declType))
	}
}

func (c *Checker) checkInferredVarDecl(s *ast.InferredVarDecl) {
	valType := c.checkExpr(s.Value)
	if valType == nil {
		return
	}

	// Track factory-created locals for `final field write restriction
	if c.inFactoryBody && s.Name != "_" && isConstructorCallExpr(s.Value) {
		c.factoryLocals[s.Name] = true
	}

	if s.Name != "_" {
		c.checkNoShadow(s.Name, s.Pos())
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
			c.checkNoShadow(name, s.Pos())
			c.insert(types.NewVar(tpos(s.Pos()), name, tup.Elems()[i]))
		}
	}
}

func (c *Checker) checkUseVarDecl(s *ast.UseVarDecl) {
	valType := c.checkExpr(s.Value)
	if valType == nil {
		return
	}

	// Verify the type has a close() method (structural Closer satisfaction)
	var named *types.Named
	switch t := valType.(type) {
	case *types.Named:
		named = t
	case *types.Instance:
		if n, ok := t.Origin().(*types.Named); ok {
			named = n
		}
	}
	if named == nil {
		c.errorf(s.Pos(), "use binding requires a type with close() method, got %s", valType)
		return
	}
	if named.LookupMethod("close") == nil {
		c.errorf(s.Pos(), "type %s has no close() method (required for use binding)", valType)
		return
	}

	if s.Name != "_" {
		c.checkNoShadow(s.Name, s.Pos())
		c.insert(types.NewVar(tpos(s.Pos()), s.Name, valType))
	}
}

func (c *Checker) checkAssignStmt(s *ast.AssignStmt) {
	targetType := c.checkExpr(s.Target)
	valType := c.checkExprWithHint(s.Value, targetType)

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
	// If the member is a field, check `final restriction
	if f := named.LookupField(me.Field); f != nil {
		if f.IsFinal() {
			if c.inNewBody {
				// OK — new() body can assign to this's final fields
			} else if c.inFactoryBody {
				// Only allow on locally-created instances
				if ident, ok := me.Target.(*ast.IdentExpr); !ok || !c.factoryLocals[ident.Name] {
					c.errorf(me.Pos(), "cannot assign to `final field '%s' (only allowed on locally-created instances in factory)", me.Field)
				}
			} else {
				c.errorf(me.Pos(), "cannot assign to `final field '%s'", me.Field)
			}
		}
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

	expected := c.curFunc.Result()

	valType := c.checkExprWithHint(s.Value, expected)
	if valType == nil {
		return
	}

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
			c.checkNoShadow(s.Binding, s.Pos())
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
		// Pre-detect compound/negated narrowing patterns before full type-check.
		// These patterns (!cc, a && b) would produce spurious type errors if
		// type-checked normally, so we intercept them first.
		narrow := c.preDetectIfNarrowing(s.Cond)
		var cond types.Type
		if narrow == nil {
			// Normal: type-check condition, then detect simple narrowing
			cond = c.checkExpr(s.Cond)
			narrow = c.detectOptionalNarrowing(s.Cond, cond)
		}
		if narrow != nil {
			// Optional narrowing: record and shadow variables in the appropriate branch
			c.info.OptionalNarrowings[s] = narrow
			if narrow.Negated {
				// Negated (!cc): then-block has NO narrowing, else-block has narrowing
				c.openScope(s.Body, "if-then")
				c.checkBlock(s.Body)
				c.closeScope()
				if s.Else != nil {
					// Insert narrowed vars into else scope
					switch elseNode := s.Else.(type) {
					case *ast.Block:
						c.openScope(elseNode, "if-narrow-else")
						for _, v := range narrow.Vars {
							c.insert(types.NewVar(tpos(s.Pos()), v.VarName, v.InnerType))
						}
						c.checkBlock(elseNode)
						c.closeScope()
					default:
						c.checkStmt(s.Else)
					}
				}
				return // else already handled
			}
			// Non-negated: then-block has narrowing
			c.openScope(s.Body, "if-narrow")
			for _, v := range narrow.Vars {
				c.insert(types.NewVar(tpos(s.Pos()), v.VarName, v.InnerType))
			}
			c.checkBlock(s.Body)
			c.closeScope()
		} else {
			if cond != nil && !types.Identical(cond, types.TypBool) {
				// Suppress for bool? identifiers (ambiguity error already reported above)
				isBoolOptIdent := false
				if _, isIdent := s.Cond.(*ast.IdentExpr); isIdent {
					if opt, isOpt := cond.(*types.Optional); isOpt {
						isBoolOptIdent = types.Identical(opt.Elem(), types.TypBool)
					}
				}
				if !isBoolOptIdent {
					c.errorf(s.Cond.Pos(), "if condition must be bool, got %s", cond)
				}
			}
			c.openScope(s.Body, "if-then")
			c.checkBlock(s.Body)
			c.closeScope()
		}
	}

	// Check else branch
	if s.Else != nil {
		c.checkStmt(s.Else)
	}
}

// detectOptionalNarrowing checks if an if-condition implies optional narrowing.
// Handles simple cases (single ident, `is present`) after the condition has been type-checked.
// Compound (!cc, a && b) patterns are handled by preDetectIfNarrowing instead.
func (c *Checker) detectOptionalNarrowing(cond ast.Expr, condType types.Type) *OptionalNarrowing {
	// Case 1: `if cc { ... }` where cc is T? (truthiness narrowing)
	if ident, ok := cond.(*ast.IdentExpr); ok {
		if opt, ok := condType.(*types.Optional); ok {
			inner := opt.Elem()
			// bool? is ambiguous for truthiness — require `is present` instead
			if types.Identical(inner, types.TypBool) {
				c.errorf(cond.Pos(), "bool? in if condition is ambiguous; use 'is present' instead")
				return nil
			}
			return &OptionalNarrowing{Vars: []NarrowedVar{{VarName: ident.Name, InnerType: inner}}}
		}
	}

	// Case 2: `if cc is present { ... }` — works for any T? including bool?
	if isExpr, ok := cond.(*ast.IsExpr); ok {
		if pat, ok := isExpr.Pattern.(*ast.IdentIsPattern); ok && pat.Name == "present" {
			if ident, ok := isExpr.Expr.(*ast.IdentExpr); ok {
				exprType := c.info.Types[isExpr.Expr]
				if opt, ok := exprType.(*types.Optional); ok {
					return &OptionalNarrowing{Vars: []NarrowedVar{{VarName: ident.Name, InnerType: opt.Elem()}}}
				}
			}
		}
	}

	return nil
}

// preDetectIfNarrowing detects compound (!cc, a && b) optional narrowing patterns
// BEFORE the condition is fully type-checked. Uses scope lookups to resolve types
// and manually records type info for the sub-expressions.
// Returns nil if no compound/negated pattern is found.
func (c *Checker) preDetectIfNarrowing(cond ast.Expr) *OptionalNarrowing {
	// Case: !cc (negated narrowing)
	if unary, ok := cond.(*ast.UnaryExpr); ok && unary.Op == ast.UnaryNot {
		vars := c.detectNarrowableExpr(unary.Operand)
		if vars != nil {
			c.recordType(cond, types.TypBool)
			return &OptionalNarrowing{Vars: vars, Negated: true}
		}
		return nil
	}

	// Case: a && b (compound narrowing)
	if bin, ok := cond.(*ast.BinaryExpr); ok && bin.Op == ast.BinAnd {
		leftVars := c.detectNarrowableExpr(bin.Left)
		rightVars := c.detectNarrowableExpr(bin.Right)
		if leftVars != nil && rightVars != nil {
			c.recordType(cond, types.TypBool)
			return &OptionalNarrowing{Vars: append(leftVars, rightVars...)}
		}
		return nil
	}

	return nil
}

// detectNarrowableExpr checks if an expression is a narrowable optional pattern
// using scope lookups (no checkExpr). Records types for matched expressions.
// Returns the narrowed variables or nil if not narrowable.
func (c *Checker) detectNarrowableExpr(expr ast.Expr) []NarrowedVar {
	// Simple identifier of type T?
	if ident, ok := expr.(*ast.IdentExpr); ok {
		obj := c.lookup(ident.Name)
		if obj == nil {
			return nil
		}
		opt, ok := obj.Type().(*types.Optional)
		if !ok {
			return nil
		}
		inner := opt.Elem()
		if types.Identical(inner, types.TypBool) {
			return nil // bool? can't use truthiness
		}
		c.recordType(expr, opt)
		c.recordObject(ident, obj)
		return []NarrowedVar{{VarName: ident.Name, InnerType: inner}}
	}

	// `x is present`
	if isExpr, ok := expr.(*ast.IsExpr); ok {
		if pat, ok := isExpr.Pattern.(*ast.IdentIsPattern); ok && pat.Name == "present" {
			if ident, ok := isExpr.Expr.(*ast.IdentExpr); ok {
				obj := c.lookup(ident.Name)
				if obj == nil {
					return nil
				}
				opt, ok := obj.Type().(*types.Optional)
				if !ok {
					return nil
				}
				c.recordType(isExpr.Expr, opt)
				c.recordObject(ident, obj)
				c.recordType(expr, types.TypBool)
				return []NarrowedVar{{VarName: ident.Name, InnerType: opt.Elem()}}
			}
		}
	}

	return nil
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
		c.checkNoShadow(s.Binding, s.Pos())
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
		if elem, ok := types.AsVector(iterType); ok {
			elemType = elem
		} else if key, val, ok := types.AsMap(iterType); ok {
			// Iterating a map yields (key, value) tuples
			elemType = types.NewTuple([]types.Type{key, val})
		} else if inst, ok := iterType.(*types.Instance); ok {
			// iter[T] yields T, stream[T] yields T, channel[T] yields T
			origin := inst.Origin()
			if origin == types.TypIter || origin == types.TypStream || origin == types.TypChannel {
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
			c.checkNoShadow(s.Binding, s.Pos())
			c.insert(types.NewVar(tpos(s.Pos()), s.Binding, elemType))
		}
		if s.Index != "" && s.Index != "_" {
			c.checkNoShadow(s.Index, s.Pos())
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
	c.checkNoShadow(s.InitName, s.Pos())
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

func (c *Checker) checkSelectStmt(s *ast.SelectStmt) {
	for _, sc := range s.Cases {
		chanType := c.checkExpr(sc.Channel)
		if chanType == nil {
			continue
		}

		// Verify it's a channel type
		inst, ok := chanType.(*types.Instance)
		if !ok || inst.Origin() != types.TypChannel {
			c.errorf(sc.Channel.Pos(), "select case requires channel type, got %s", chanType)
			continue
		}

		var elemType types.Type
		if len(inst.TypeArgs()) > 0 {
			elemType = inst.TypeArgs()[0]
		}

		if sc.IsSend {
			// Send case: verify method name is "send" (already parsed)
			// Check value type matches channel element type
			if sc.SendValue != nil && elemType != nil {
				valType := c.checkExprWithHint(sc.SendValue, elemType)
				if valType != nil && !types.AssignableTo(valType, elemType) {
					c.errorf(sc.SendValue.Pos(), "cannot send %s on channel[%s]", valType, elemType)
				}
			}
		} else {
			// Receive case: introduce binding as T? in case body scope
			// (same as regular receive: returns optional)
		}

		// Type-check body statements in a new scope
		c.openScope(s, "select-case")
		if !sc.IsSend && sc.Binding != "_" && elemType != nil {
			// Receive binding is T? (optional, like normal receive)
			c.checkNoShadow(sc.Binding, sc.Pos())
			optType := types.NewOptional(elemType)
			c.insert(types.NewVar(tpos(sc.Pos()), sc.Binding, optType))
		}
		for _, stmt := range sc.Body {
			c.checkStmt(stmt)
		}
		c.closeScope()
	}

	// Check default case body
	if s.Default != nil {
		c.openScope(s, "select-default")
		for _, stmt := range s.Default {
			c.checkStmt(stmt)
		}
		c.closeScope()
	}
}

// isConstructorCallExpr returns true if the expression is a constructor call
// (Type(...), Self(...)), possibly wrapped in error propagation (!)  or unwrap (!!).
func isConstructorCallExpr(expr ast.Expr) bool {
	// Unwrap error propagation: Foo(...)! or Foo(...)!!
	switch e := expr.(type) {
	case *ast.ErrorPropagateExpr:
		return isConstructorCallExpr(e.Expr)
	case *ast.ErrorUnwrapExpr:
		return isConstructorCallExpr(e.Expr)
	}
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch callee := call.Callee.(type) {
	case *ast.IdentExpr:
		// Constructor calls use type names (capitalized) or Self
		if len(callee.Name) > 0 && callee.Name[0] >= 'A' && callee.Name[0] <= 'Z' {
			return true
		}
	case *ast.IndexExpr:
		// Generic constructor: Type[T](...)
		if ident, ok := callee.Target.(*ast.IdentExpr); ok {
			if len(ident.Name) > 0 && ident.Name[0] >= 'A' && ident.Name[0] <= 'Z' {
				return true
			}
		}
	}
	return false
}
