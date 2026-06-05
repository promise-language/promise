package sema

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
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
		c.checkExprStmtFailable(s)

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
		if !c.inGenerator {
			c.errorf(s.Pos(), "yield outside of generator function")
		} else if c.lambdaDepth > 0 {
			c.errorf(s.Pos(), "yield inside lambda/closure is not allowed")
		} else {
			c.yieldFound = true
			valType := c.checkExprWithHint(s.Value, c.generatorElemType)
			if valType != nil && c.generatorElemType != nil {
				if !types.AssignableTo(valType, c.generatorElemType) {
					c.errorf(s.Value.Pos(), "cannot yield %s in generator returning stream[%s]",
						valType, c.generatorElemType)
				}
			}
		}

	case *ast.YieldDelegateStmt:
		if !c.inGenerator {
			c.errorf(s.Pos(), "yield* outside of generator function")
		} else if c.lambdaDepth > 0 {
			c.errorf(s.Pos(), "yield* inside lambda/closure is not allowed")
		} else {
			c.yieldFound = true
			valType := c.checkExpr(s.Value)
			if valType != nil {
				var delegateElem types.Type
				if elem, ok := types.AsStream(valType); ok {
					delegateElem = elem
				} else if elem, ok := types.AsIterator(valType); ok {
					delegateElem = elem
				} else if arr, ok := valType.(*types.Array); ok {
					delegateElem = arr.Elem()
				} else if elem, ok := types.AsVector(valType); ok {
					delegateElem = elem
				} else if elem, ok := types.AsRange(valType); ok {
					delegateElem = elem
				} else if types.Identical(valType, types.TypString) {
					delegateElem = types.TypChar
				} else {
					c.errorf(s.Value.Pos(), "yield* requires an iterable type, got %s", valType)
				}
				if delegateElem != nil && c.generatorElemType != nil {
					if !types.AssignableTo(delegateElem, c.generatorElemType) {
						c.errorf(s.Value.Pos(), "yield* element type %s does not match generator element type %s",
							delegateElem, c.generatorElemType)
					}
				}
			}
		}

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

		// Auto-propagate failable calls in assignments within failable functions.
		c.checkVarDeclFailable(s.Value)

		// Error handler in value context must produce recovery value or diverge.
		c.checkErrorHandlerRecovery(s.Value, declType)
		// Update recorded type for optional recovery handlers
		if c.info.OptionalRecoveryHandlers[s.Value] {
			c.recordType(s.Value, declType)
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

	// T0682: A void-typed RHS produces no value, so there is nothing to bind.
	// This includes `r := void_func()` and `r := <-void_task`. Without this
	// check codegen would emit `alloca void` / `store nil` (LLVM error or a
	// nil-pointer panic). To await a void task or run a void call for effect,
	// use the expression as a statement (`<-task;` / `foo();`) instead.
	if types.Identical(valType, types.TypVoid) {
		c.errorf(s.Pos(), "cannot bind void to variable '%s': expression produces no value; use it as a statement instead", s.Name)
		return
	}

	// Track factory-created locals for `final field write restriction
	if c.inFactoryBody && s.Name != "_" && isConstructorCallExpr(s.Value) {
		c.factoryLocals[s.Name] = true
	}

	// Auto-propagate failable calls in assignments within failable functions.
	c.checkVarDeclFailable(s.Value)

	// Error handler in value context must produce recovery value or diverge.
	c.checkErrorHandlerRecovery(s.Value, nil)

	// Non-recovering error handler in inferred decl: wrap type as optional
	if c.info.OptionalRecoveryHandlers[s.Value] {
		valType = types.NewOptional(valType)
		c.recordType(s.Value, valType)
	}

	if s.Name != "_" {
		c.checkNoShadow(s.Name, s.Pos())
		c.insert(types.NewVar(tpos(s.Pos()), s.Name, valType))
	}
}

// checkVarDeclFailable handles naked failable calls in variable declarations.
// In failable functions, the error is auto-propagated to the caller.
// In non-failable functions, it is a compile error.
func (c *Checker) checkVarDeclFailable(expr ast.Expr) {
	if !c.info.FailableExprs[expr] {
		return
	}
	if c.curFunc != nil && c.curFunc.CanError() {
		c.info.AutoPropagateExprs[expr] = true
	} else {
		c.errorf(expr.Pos(), "failable call must be handled: use ?^ to propagate, ?! to panic on error, or ? { } for an inline handler")
	}
}

// checkSubExprFailable handles a failable call expression used as a
// sub-expression (binary/unary operand, etc.) in a context that expects
// its plain value type. In failable functions, the error is auto-propagated
// to the caller. In non-failable functions, it is a compile error.
func (c *Checker) checkSubExprFailable(expr ast.Expr) {
	if !c.info.FailableExprs[expr] {
		return
	}
	if c.curFunc != nil && c.curFunc.CanError() {
		c.info.AutoPropagateExprs[expr] = true
	} else {
		c.errorf(expr.Pos(), "failable call must be handled: use ?^ to propagate, ?! to panic on error, or ? { } for an inline handler")
	}
}

// checkErrorHandlerRecovery validates that an error handler used in a value
// context (variable declaration) either produces a recovery value or diverges.
// Without this, the variable would get a zero-initialized value, which is
// unsafe for types with drop methods (e.g., File with _fd=0 → closes stdin).
//
// declType is the declared type for typed declarations, or nil for inferred.
// When the handler doesn't recover, optional-typed or inferred declarations
// are allowed (variable becomes T?); non-optional typed declarations error.
func (c *Checker) checkErrorHandlerRecovery(expr ast.Expr, declType types.Type) {
	handler, ok := expr.(*ast.ErrorHandlerExpr)
	if !ok {
		return
	}

	// Check if a block either diverges or produces a non-void recovery value.
	blockRecovers := func(body *ast.Block) bool {
		if body == nil {
			return false
		}
		if c.blockReturns(body) {
			return true // diverges
		}
		if len(body.Stmts) > 0 {
			if es, ok := body.Stmts[len(body.Stmts)-1].(*ast.ExprStmt); ok {
				if typ := c.info.Types[es.Expr]; typ != nil && !types.Identical(typ, types.TypVoid) {
					return true // produces a non-void value
				}
			}
		}
		return false
	}

	// All reachable error paths must recover for the handler to be "fully recovering".
	// For typed handlers with else: both match body and else body are reachable.
	// For typed handlers with ! suffix: non-match panics, only match body matters.
	// For plain handlers: only the handler body matters.
	handlerRecovers := blockRecovers(handler.Body)
	if handlerRecovers {
		if handler.ElseBody != nil {
			// Typed handler with else: both bodies must recover.
			if blockRecovers(handler.ElseBody) {
				return // fully recovering
			}
		} else if handler.PanicOnNomatch || handler.TypeName == "" {
			return // fully recovering (no else path or panic on nomatch)
		} else {
			// Typed handler without else/! in failable function — nomatch propagates.
			return // fully recovering (nomatch auto-propagates)
		}
	}

	// At least one path doesn't produce a recovery value and doesn't diverge.
	// For optional-typed or inferred declarations, this is allowed — the
	// variable becomes T? (some on success, none on error).
	if declType != nil {
		if _, isOpt := declType.(*types.Optional); isOpt {
			c.info.OptionalRecoveryHandlers[expr] = true
			return
		}
	} else {
		// Inferred declaration — mark for optional wrapping
		c.info.OptionalRecoveryHandlers[expr] = true
		return
	}
	c.errorf(handler.Pos(), "error handler must produce a recovery value or diverge (return/raise) when used in an assignment")
}

func (c *Checker) checkDestructureVarDecl(s *ast.DestructureVarDecl) {
	valType := c.checkExpr(s.Value)
	if valType == nil {
		return
	}

	// Failable result capture: (val, err) := failableCall()
	if c.info.FailableExprs[s.Value] {
		if len(s.Names) != 2 {
			c.errorf(s.Pos(), "failable result destructuring requires exactly 2 bindings (value, error), got %d", len(s.Names))
			return
		}
		c.info.FailableDestructures[s] = true
		// First binding: success type (T from T!)
		if s.Names[0] != "_" {
			c.checkNoShadow(s.Names[0], s.Pos())
			c.insert(types.NewVar(tpos(s.Pos()), s.Names[0], valType))
		}
		// Second binding: error? (optional error)
		if s.Names[1] != "_" {
			c.checkNoShadow(s.Names[1], s.Pos())
			c.insert(types.NewVar(tpos(s.Pos()), s.Names[1], types.NewOptional(types.TypError)))
		}
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

	// Check for failable calls in the assigned value.
	c.checkVarDeclFailable(s.Value)

	// Validate setter exists when assigning to a getter property
	if me, ok := s.Target.(*ast.MemberExpr); ok {
		c.checkSetterAvailable(me)
	}

	// Same-file / glob-import getter assignment: counter = 10
	if ident, ok := s.Target.(*ast.IdentExpr); ok {
		if obj := c.lookup(ident.Name); obj != nil {
			if fn, ok := obj.(*types.Func); ok && fn.IsGetter() {
				setterObj := c.lookup(ident.Name + "$set")
				if setterObj == nil {
					c.errorf(ident.Pos(), "property '%s' has no setter", ident.Name)
				}
			}
		}
	}

	// Validate []= exists when assigning to an index target
	if idx, ok := s.Target.(*ast.IndexExpr); ok {
		c.checkIndexAssignAvailable(idx)
	}

	// Validate [:]= exists when assigning to a slice target
	if sl, ok := s.Target.(*ast.SliceExpr); ok {
		c.checkSliceAssignAvailable(sl)
	}

	// T0708: A failable setter / []= / [:]= silently dropped its error before;
	// reject the assignment unless the enclosing function is failable, in which
	// case codegen auto-propagates the call result via propagateIfFailable.
	if c.assignmentSetterCanError(s) && (c.curFunc == nil || !c.curFunc.CanError()) {
		c.errorf(s.Pos(), "failable setter assignment must be in a failable function: mark the enclosing function with `!`")
	} else if s.Op != ast.OpAssign && c.assignmentGetterCanError(s) &&
		(c.curFunc == nil || !c.curFunc.CanError()) {
		// T0709: a compound assignment reads the current value via a getter; a
		// failable read getter propagates, so require a failable scope. The
		// else-if guards against a duplicate diagnostic when the setter also errors.
		c.errorf(s.Pos(), "failable getter read in compound assignment must be in a failable function: mark the enclosing function with `!`")
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
	// Module-level setter check: mod.property = value
	if ident, ok := me.Target.(*ast.IdentExpr); ok {
		if obj := c.lookup(ident.Name); obj != nil {
			if mod, ok := obj.(*types.Module); ok {
				scope := mod.Scope()
				if scope == nil {
					return
				}
				// Check for a setter (stored as name$set)
				setterObj := scope.Lookup(me.Field + "$set")
				if setterObj == nil {
					// No setter — check if getter exists (read-only property)
					getterObj := scope.Lookup(me.Field)
					if getterObj != nil {
						if fn, ok := getterObj.(*types.Func); ok && fn.IsGetter() {
							c.errorf(me.Pos(), "property '%s' on module '%s' has no setter", me.Field, mod.Name())
						}
					}
				}
				return
			}
		}
	}

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
	// Unwrap MutRef/SharedRef for index assignment (auto-deref through borrows)
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
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

// assignmentSetterCanError reports whether the assignment statement's effective
// setter (property setter, module-level setter, []=, or [:]=) can raise. T0708.
func (c *Checker) assignmentSetterCanError(s *ast.AssignStmt) bool {
	switch tgt := s.Target.(type) {
	case *ast.MemberExpr:
		// Module-level setter: mod.prop = v → setter stored as "prop$set"
		if ident, ok := tgt.Target.(*ast.IdentExpr); ok {
			if obj := c.lookup(ident.Name); obj != nil {
				if mod, ok := obj.(*types.Module); ok {
					if scope := mod.Scope(); scope != nil {
						if setterObj := scope.Lookup(tgt.Field + "$set"); setterObj != nil {
							if fn, ok := setterObj.(*types.Func); ok {
								if sig, ok := fn.Type().(*types.Signature); ok {
									return sig.CanError()
								}
							}
						}
					}
					return false
				}
			}
		}
		// Instance setter
		targetType := c.info.Types[tgt.Target]
		if targetType == nil {
			return false
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
			return false
		}
		if setter := named.LookupSetter(tgt.Field); setter != nil {
			return setter.Sig().CanError()
		}
		return false

	case *ast.IdentExpr:
		// Same-file / glob-import getter assignment: counter = 10
		if obj := c.lookup(tgt.Name + "$set"); obj != nil {
			if fn, ok := obj.(*types.Func); ok {
				if sig, ok := fn.Type().(*types.Signature); ok {
					return sig.CanError()
				}
			}
		}
		return false

	case *ast.IndexExpr:
		targetType := c.info.Types[tgt.Target]
		if ref, ok := targetType.(*types.MutRef); ok {
			targetType = ref.Elem()
		}
		if ref, ok := targetType.(*types.SharedRef); ok {
			targetType = ref.Elem()
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
			return false
		}
		if m := named.LookupMethod("[]="); m != nil {
			return m.Sig().CanError()
		}
		return false

	case *ast.SliceExpr:
		targetType := c.info.Types[tgt.Target]
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
			return false
		}
		if m := named.LookupMethod("[:]="); m != nil {
			return m.Sig().CanError()
		}
		return false
	}
	return false
}

// assignmentGetterCanError reports whether a compound assignment's read getter
// (property getter, module-level getter, or []) can raise. A compound assignment
// reads the current value via this getter before applying the operator, so a
// failable read must propagate to the caller. T0709.
func (c *Checker) assignmentGetterCanError(s *ast.AssignStmt) bool {
	switch tgt := s.Target.(type) {
	case *ast.MemberExpr:
		// Module-level getter: mod.prop → getter stored under "prop"
		if ident, ok := tgt.Target.(*ast.IdentExpr); ok {
			if obj := c.lookup(ident.Name); obj != nil {
				if mod, ok := obj.(*types.Module); ok {
					if scope := mod.Scope(); scope != nil {
						if getterObj := scope.Lookup(tgt.Field); getterObj != nil {
							if fn, ok := getterObj.(*types.Func); ok && fn.IsGetter() {
								if sig, ok := fn.Type().(*types.Signature); ok {
									return sig.CanError()
								}
							}
						}
					}
					return false
				}
			}
		}
		// Instance getter
		targetType := c.info.Types[tgt.Target]
		if targetType == nil {
			return false
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
			return false
		}
		if getter := named.LookupGetter(tgt.Field); getter != nil {
			return getter.Sig().CanError()
		}
		return false

	case *ast.IdentExpr:
		// Same-file / glob-import getter: counter += 10
		if obj := c.lookup(tgt.Name); obj != nil {
			if fn, ok := obj.(*types.Func); ok && fn.IsGetter() {
				if sig, ok := fn.Type().(*types.Signature); ok {
					return sig.CanError()
				}
			}
		}
		return false

	case *ast.IndexExpr:
		return c.indexGetterCanError(tgt)
	}
	return false
}

// indexGetterCanError reports whether the [] read of an index target can raise.
// Shared by compound index assignment (m[k] += v) and inc/dec (m[k]++), both of
// which read the current value via [] before writing. T0709.
func (c *Checker) indexGetterCanError(target ast.Expr) bool {
	idx, ok := target.(*ast.IndexExpr)
	if !ok {
		return false
	}
	targetType := c.info.Types[idx.Target]
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
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
		return false
	}
	if m := named.LookupMethod("[]"); m != nil {
		return m.Sig().CanError()
	}
	return false
}

func (c *Checker) checkReturnStmt(s *ast.ReturnStmt) {
	if c.curFunc == nil {
		c.errorf(s.Pos(), "return outside of function")
		return
	}

	// In generator functions, bare return terminates the generator.
	// return-with-value is an error (use yield instead).
	if c.inGenerator {
		if s.Value != nil {
			c.errorf(s.Pos(), "cannot return a value from a generator function; use yield instead")
			c.checkExpr(s.Value) // still type-check for error recovery
		}
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
	valType := c.checkExpr(s.Value)
	if valType == nil {
		return
	}
	// Raised value must be an error type (error itself or a subtype).
	named, _ := valType.(*types.Named)
	if named == nil {
		if inst, ok := valType.(*types.Instance); ok {
			named, _ = inst.Origin().(*types.Named)
		}
	}
	if named == nil || !named.InheritsFrom(types.TypError) {
		c.errorf(s.Pos(), "raise requires an error type, got %s", valType)
	}
}

// checkExprStmtFailable validates failable calls used as expression statements.
// In failable functions, naked failable calls are auto-propagated.
// In non-failable functions, naked failable calls are a compile error.
func (c *Checker) checkExprStmtFailable(s *ast.ExprStmt) {
	if !c.info.FailableExprs[s.Expr] {
		return
	}
	// The expression is a failable call used as a statement (no ?^, ?!, or handler).
	if c.curFunc != nil && c.curFunc.CanError() {
		// Auto-propagate: codegen will emit tag-check + early return.
		c.info.AutoPropagateExprs[s.Expr] = true
	} else {
		c.errorf(s.Expr.Pos(), "failable call must be handled: use ?^ to propagate, ?! to panic on error, or ? { } for an inline handler")
	}
}

func (c *Checker) checkIfStmt(s *ast.IfStmt) {
	if s.Binding != "" {
		// If-unwrap: if val := expr { }
		c.openScope(s.Body, "if-unwrap")
		initType := c.checkExpr(s.Init)
		// T0770: a failable scrutinee (e.g. `if e := load()` where `load!() T?`)
		// auto-propagates its error in a failable function, leaving the `T?` to
		// unwrap; in a non-failable function it must be handled explicitly.
		c.checkVarDeclFailable(s.Init)
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
				// Negated (!cc, is absent): then-block has NO narrowing, else-block has narrowing
				c.openScope(s.Body, "if-then")
				c.checkBlock(s.Body)
				thenDiverges := c.blockReturns(s.Body)
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
				// Post-divergence narrowing: if the then-body always diverges
				// (return/raise), the variable must be present in all subsequent
				// code. Set pendingNarrowings for checkBlock to pick up.
				if thenDiverges && s.Else == nil {
					narrow.PostNarrow = true
					c.pendingNarrowings = narrow.Vars
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
		} else if destructNarrow := c.detectIsDestructureNarrowing(s.Cond); destructNarrow != nil {
			// Destructure is-pattern: insert bindings into then-scope
			c.info.IsDestructureNarrowings[s] = destructNarrow
			c.openScope(s.Body, "if-destructure")
			for _, b := range destructNarrow.Bindings {
				if b.VarName != "_" {
					c.insert(types.NewVar(tpos(s.Pos()), b.VarName, b.Type))
				}
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

// detectIsDestructureNarrowing checks if an if-condition is a destructure is-pattern
// (e.g., `if shape is Circle(r)` or `if animal is Dog(breed)`).
// Returns the narrowing info if detected, nil otherwise.
func (c *Checker) detectIsDestructureNarrowing(cond ast.Expr) *IsDestructureNarrowing {
	isExpr, ok := cond.(*ast.IsExpr)
	if !ok {
		return nil
	}
	dp, ok := isExpr.Pattern.(*ast.DestructureIsPattern)
	if !ok {
		return nil
	}

	subjectType := c.info.Types[isExpr.Expr]
	if subjectType == nil {
		return nil
	}

	// Check if it's an enum variant of the subject type
	var enum *types.Enum
	var subst map[*types.TypeParam]types.Type
	switch st := subjectType.Underlying().(type) {
	case *types.Enum:
		enum = st
	case *types.Instance:
		if e, ok := st.Origin().(*types.Enum); ok {
			enum = e
			subst = types.BuildSubstMap(e.TypeParams(), st.TypeArgs())
		}
	}

	if enum != nil {
		v := enum.LookupVariant(dp.TypeName)
		if v != nil {
			bindings := make([]IsDestructureBinding, 0, len(dp.Bindings))
			for i, name := range dp.Bindings {
				if i >= v.NumFields() {
					break
				}
				ft := v.Fields()[i].Type()
				if subst != nil {
					ft = types.Substitute(ft, subst)
				}
				bindings = append(bindings, IsDestructureBinding{VarName: name, Type: ft})
			}
			return &IsDestructureNarrowing{
				SubjectExpr: isExpr.Expr,
				Bindings:    bindings,
				IsEnum:      true,
				VariantName: dp.TypeName,
				TargetType:  subjectType,
			}
		}
	}

	// Generic type destructure: use resolved type from IsPatternTypes
	if resolved, ok := c.info.IsPatternTypes[dp]; ok {
		if inst, ok := resolved.(*types.Instance); ok {
			if named, ok := inst.Origin().(*types.Named); ok {
				instSubst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
				allFields := named.AllFields()
				bindings := make([]IsDestructureBinding, 0, len(dp.Bindings))
				for i, name := range dp.Bindings {
					if i >= len(allFields) {
						break
					}
					ft := types.Substitute(allFields[i].Type(), instSubst)
					bindings = append(bindings, IsDestructureBinding{VarName: name, Type: ft})
				}
				return &IsDestructureNarrowing{
					SubjectExpr: isExpr.Expr,
					Bindings:    bindings,
					IsEnum:      false,
					TargetType:  resolved,
				}
			}
		}
	}

	// Named type destructure
	obj := c.lookup(dp.TypeName)
	if obj == nil {
		return nil
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return nil
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return nil
	}
	allFields := named.AllFields()
	bindings := make([]IsDestructureBinding, 0, len(dp.Bindings))
	for i, name := range dp.Bindings {
		if i >= len(allFields) {
			break
		}
		bindings = append(bindings, IsDestructureBinding{VarName: name, Type: allFields[i].Type()})
	}
	return &IsDestructureNarrowing{
		SubjectExpr: isExpr.Expr,
		Bindings:    bindings,
		IsEnum:      false,
		TargetType:  named,
	}
}

// preDetectIfNarrowing detects compound (!cc, a && b) and negated (is absent)
// optional narrowing patterns BEFORE the condition is fully type-checked.
// Uses scope lookups to resolve types and manually records type info for the
// sub-expressions. Returns nil if no compound/negated pattern is found.
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

	// Case: x is absent (negated narrowing — equivalent to !x for optionals)
	if isExpr, ok := cond.(*ast.IsExpr); ok {
		if pat, ok := isExpr.Pattern.(*ast.IdentIsPattern); ok && pat.Name == "absent" {
			if ident, ok := isExpr.Expr.(*ast.IdentExpr); ok {
				obj := c.lookup(ident.Name)
				if obj != nil {
					if opt, ok := obj.Type().(*types.Optional); ok {
						c.recordType(isExpr.Expr, opt)
						c.recordObject(ident, obj)
						c.recordType(cond, types.TypBool)
						return &OptionalNarrowing{
							Vars:    []NarrowedVar{{VarName: ident.Name, InnerType: opt.Elem()}},
							Negated: true,
						}
					}
				}
			}
		}
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
	// T0770: a failable scrutinee auto-propagates its error (failable function)
	// or must be handled explicitly (non-failable), same as the if-unwrap form.
	c.checkVarDeclFailable(s.Value)

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
		var mapKeyType types.Type // non-nil when iterating a map with 2 bindings
		if arr, ok := iterType.(*types.Array); ok {
			elemType = arr.Elem()
		} else if elem, ok := types.AsVector(iterType); ok {
			elemType = elem
		} else if key, val, ok := types.AsMap(iterType); ok {
			if s.Index != "" {
				// for k, v in map: k = key type, v = value type
				elemType = val
				mapKeyType = key
			} else {
				// for entry in map: entry = (key, value) tuple
				elemType = types.NewTuple([]types.Type{key, val})
			}
		} else if elem, ok := types.AsRange(iterType); ok {
			elemType = elem
		} else if inst, ok := iterType.(*types.Instance); ok {
			origin := inst.Origin()
			if origin == types.TypIter {
				// Iterator[T] yields T via next() — record for duck-typed codegen
				if len(inst.TypeArgs()) > 0 {
					elemType = inst.TypeArgs()[0]
				} else {
					elemType = iterType
				}
				c.info.ForInKinds[s] = ForInNext
			} else if origin == types.TypStream {
				// Stream[T] yields T via iter() → next()
				if len(inst.TypeArgs()) > 0 {
					elemType = inst.TypeArgs()[0]
				} else {
					elemType = iterType
				}
				// Stream still uses genForInGenerator (raw coroutine path)
			} else if origin == types.TypChannel {
				// Channel[T] yields T via channel receive
				if len(inst.TypeArgs()) > 0 {
					elemType = inst.TypeArgs()[0]
				} else {
					elemType = iterType
				}
			} else if duckElem := c.checkDuckTypedForIn(s, iterType); duckElem != nil {
				elemType = duckElem
			} else {
				c.errorf(s.Iterable.Pos(), "cannot iterate over type %s", iterType)
				elemType = iterType
			}
		} else if types.Identical(iterType, types.TypString) {
			elemType = types.TypChar
		} else if duckElem := c.checkDuckTypedForIn(s, iterType); duckElem != nil {
			elemType = duckElem
		} else {
			c.errorf(s.Iterable.Pos(), "cannot iterate over type %s", iterType)
			elemType = iterType
		}

		if s.Binding != "_" {
			c.checkNoShadow(s.Binding, s.Pos())
			c.insert(types.NewVar(tpos(s.Pos()), s.Binding, elemType))
		}
		if s.Index != "" && s.Index != "_" {
			c.checkNoShadow(s.Index, s.Pos())
			var indexType types.Type = types.TypInt
			if mapKeyType != nil {
				indexType = mapKeyType
			}
			c.insert(types.NewVar(tpos(s.Pos()), s.Index, indexType))
		}
	}

	c.checkBlock(s.Body)
	c.closeScope()
	c.inLoop--
}

// checkDuckTypedForIn checks if a type supports iteration via duck-typing:
// 1. Has next() T? method → ForInNext, returns T
// 2. Has iter() returning type with next() T? → ForInIter, returns T
// Returns the element type, or nil if the type doesn't support iteration.
func (c *Checker) checkDuckTypedForIn(s *ast.ForInStmt, iterType types.Type) types.Type {
	// Get the Named type (handles both Named and Instance)
	var named *types.Named
	var subst map[*types.TypeParam]types.Type
	switch t := iterType.(type) {
	case *types.Named:
		named = t
	case *types.Instance:
		if origin, ok := t.Origin().(*types.Named); ok {
			named = origin
			if len(origin.TypeParams()) > 0 {
				subst = types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
			}
		}
	}
	if named == nil {
		return nil
	}

	// Check for next() T? method
	if nextMethod := named.LookupMethod("next"); nextMethod != nil {
		retType := nextMethod.Sig().Result()
		if subst != nil {
			retType = types.Substitute(retType, subst)
		}
		if opt, ok := retType.(*types.Optional); ok {
			c.info.ForInKinds[s] = ForInNext
			return opt.Elem()
		}
	}

	// Check for iter() returning type with next() T?
	if iterMethod := named.LookupMethod("iter"); iterMethod != nil {
		iterRetType := iterMethod.Sig().Result()
		if subst != nil {
			iterRetType = types.Substitute(iterRetType, subst)
		}
		// Resolve the iterator type returned by iter()
		var iterNamed *types.Named
		var iterSubst map[*types.TypeParam]types.Type
		switch t := iterRetType.(type) {
		case *types.Named:
			iterNamed = t
		case *types.Instance:
			if origin, ok := t.Origin().(*types.Named); ok {
				iterNamed = origin
				if len(origin.TypeParams()) > 0 {
					iterSubst = types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
				}
			}
		}
		if iterNamed != nil {
			if nextMethod := iterNamed.LookupMethod("next"); nextMethod != nil {
				nextRetType := nextMethod.Sig().Result()
				if iterSubst != nil {
					nextRetType = types.Substitute(nextRetType, iterSubst)
				}
				if opt, ok := nextRetType.(*types.Optional); ok {
					c.info.ForInKinds[s] = ForInIter
					return opt.Elem()
				}
			}
		}
	}

	return nil
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

	// T0709: inc/dec reads the current value via [] — a failable getter propagates.
	if c.indexGetterCanError(s.Target) && (c.curFunc == nil || !c.curFunc.CanError()) {
		c.errorf(s.Pos(), "failable index read in inc/dec must be in a failable function: mark the enclosing function with `!`")
	}

	// Inc/dec is a write: validate the target is assignable, mirroring
	// checkAssignStmt. Without this a read-only property / `final field / type
	// without []= reaches codegen and panics (or silently mutates a final). T0712.
	switch tgt := s.Target.(type) {
	case *ast.MemberExpr:
		c.checkSetterAvailable(tgt)
		// T0712: inc/dec on a property reads via the getter and writes via the
		// setter. If either accessor is failable, the read/write propagates, so
		// require a failable enclosing function (previously a hard codegen panic).
		if c.incDecPropertyCanError(tgt) && (c.curFunc == nil || !c.curFunc.CanError()) {
			c.errorf(s.Pos(), "failable property inc/dec must be in a failable function: mark the enclosing function with `!`")
		}
	case *ast.IndexExpr:
		c.checkIndexAssignAvailable(tgt)
	}
}

// incDecPropertyCanError reports whether inc/dec on a property reads or writes
// via a failable getter or setter. Mirrors assignmentSetterCanError's MemberExpr
// branch. T0712.
func (c *Checker) incDecPropertyCanError(me *ast.MemberExpr) bool {
	targetType := c.info.Types[me.Target]
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
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
		return false
	}
	setter := named.LookupSetter(me.Field)
	if setter == nil {
		return false // direct field — no accessor failability
	}
	if setter.Sig().CanError() {
		return true
	}
	getter := named.LookupGetter(me.Field)
	return getter != nil && getter.Sig().CanError()
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
					c.errorf(sc.SendValue.Pos(), "cannot send %s on Channel[%s]", valType, elemType)
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
	case *ast.ErrorPanicExpr:
		return isConstructorCallExpr(e.Expr)
	case *ast.OptionalUnwrapExpr:
		return isConstructorCallExpr(e.Expr)
	case *ast.AutoCloneExpr: // T0605: synth-only; inner is always this.field
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
