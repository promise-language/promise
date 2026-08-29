package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- If expressions ---

func (c *Compiler) genIfExpr(e *ast.IfExpr) value.Value {
	cond := c.genExpr(e.Cond)

	thenBlock := c.newBlock("if.then")
	elseBlock := c.newBlock("if.else")
	mergeBlock := c.newBlock("if.merge")

	c.block.NewCondBr(cond, thenBlock, elseBlock)

	// T0496: Propagate the if-expression's result type as the contextual target
	// type for each arm so a bare `none` arm lowers to a zero value of the shared
	// result type (e.g. an Optional struct) rather than the `i1 0` void fallback,
	// which would produce a phi-type mismatch. T1189: sema's joinBranchTypes unifies
	// a `none` arm with a value arm into `T?`, but a bare value arm still lowers to
	// the inner `T`; wrapArmValueOptional (applied before the phi below) rewraps it
	// so both incomings share the `{ i1, T }` shape.
	ifResultType := c.info.Types[e]
	if c.typeSubst != nil && ifResultType != nil {
		ifResultType = types.Substitute(ifResultType, c.typeSubst)
	}

	// Then branch
	c.block = thenBlock
	savedTarget := c.targetType
	c.targetType = ifResultType
	thenVal := c.genBlockValue(e.Then)
	c.targetType = savedTarget
	thenOwned := c.blockValueOwnedResult   // T1107: genBlockValue recorded ownership
	thenOwnedFlag := c.blockValueOwnedFlag // T1208: live per-path flag (nested tracked temp)
	c.claimStringTemp(thenVal)             // T0073
	thenEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else branch
	c.block = elseBlock
	// Re-set per-arm (not once for both): a nested expression inside the then arm
	// can clear c.targetType, which would otherwise leak into the else `none`.
	c.targetType = ifResultType
	elseVal := c.genBlockValue(e.Else)
	c.targetType = savedTarget
	elseOwned := c.blockValueOwnedResult   // T1107: genBlockValue recorded ownership
	elseOwnedFlag := c.blockValueOwnedFlag // T1208: live per-path flag (nested tracked temp)
	c.claimStringTemp(elseVal)             // T0073
	elseEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	c.block = mergeBlock

	// Filter void-typed values — they cannot participate in phi nodes.
	if thenVal != nil {
		if _, isVoid := thenVal.Type().(*irtypes.VoidType); isVoid {
			thenVal = nil
		}
	}
	if elseVal != nil {
		if _, isVoid := elseVal.Type().(*irtypes.VoidType); isVoid {
			elseVal = nil
		}
	}

	// T1189: rewrap a bare inner value arm to the shared Optional shape so both
	// phi incomings agree (a `none` arm already produced `{ i1, T }`). The
	// insertvalue must land in each arm's own end block (before its br to merge) so
	// it dominates the phi incoming edge — temporarily retarget c.block per arm.
	if thenVal != nil {
		c.block = thenEnd
		thenVal = c.wrapArmValueOptional(thenVal, ifResultType)
	}
	if elseVal != nil {
		c.block = elseEnd
		elseVal = c.wrapArmValueOptional(elseVal, ifResultType)
	}
	c.block = mergeBlock

	// Build the phi from whichever branches reach mergeBlock. One arm may diverge
	// (end in return/raise), leaving only the other to contribute its value —
	// mirror genIfStmtValue / buildMatchPhi's single-incoming handling rather than
	// requiring both arms and returning nil (T1330).
	//
	// T1336: in statement / no-hint position one arm may yield a value while the
	// other is a reachable void arm (`if b { 1 } else {}`). Both arms branch to
	// mergeBlock, so a phi built from only the value arm has fewer incomings than
	// mergeBlock has predecessors → malformed IR ("PHINode should have one entry
	// for each predecessor"). Mirror buildMatchPhi: every predecessor that branches
	// to mergeBlock must contribute an incoming, zero-filling a reachable void arm
	// with the value arm's type. The merged value is discarded in statement position
	// and typed as the value arm's type in a bare `:=` (sema's joinBranchTypes
	// returns the value arm's type), matching how `match` lowers the same shape.
	var valType irtypes.Type
	if thenVal != nil {
		valType = thenVal.Type()
	} else if elseVal != nil {
		valType = elseVal.Type()
	}
	var incomings []*ir.Incoming
	if thenEnd.Term != nil && isBrTo(thenEnd.Term, mergeBlock) {
		v := thenVal
		if v == nil && valType != nil {
			v = constant.NewZeroInitializer(valType)
		}
		if v != nil {
			incomings = append(incomings, &ir.Incoming{X: v, Pred: thenEnd})
		}
	}
	if elseEnd.Term != nil && isBrTo(elseEnd.Term, mergeBlock) {
		v := elseVal
		if v == nil && valType != nil {
			v = constant.NewZeroInitializer(valType)
		}
		if v != nil {
			incomings = append(incomings, &ir.Incoming{X: v, Pred: elseEnd})
		}
	}
	if len(incomings) > 0 {
		phi := mergeBlock.NewPhi(incomings...)
		// T1107: register an owned i8* phi (string/vector/native handle) as a
		// statement temp so a borrow/discard consumer frees it exactly once. Reuses
		// the match path via two synthetic arm records (only .end and .owned are
		// consulted). A bound/return consumer claims the phi and neutralizes the flag.
		// trackMergeResultTemp itself filters to arms that branch to mergeBlock, so a
		// diverging arm's synthetic record is safely ignored.
		c.trackMergeResultTemp(phi, ifResultType, []matchArmInfo{
			{end: thenEnd, owned: thenOwned, ownedFlag: thenOwnedFlag},
			{end: elseEnd, owned: elseOwned, ownedFlag: elseOwnedFlag},
		})
		return phi
	}

	// T1332: both arms diverged (return/raise) → no incoming reaches mergeBlock,
	// so it is dead code. Terminate it and hand the consumer a typed poison value.
	// Guard against a reachable-but-void merge (arms branch to merge with no value,
	// e.g. statement-position void arms): only the truly unreachable case gets the
	// `unreachable` terminator; the reachable case keeps returning nil.
	thenReaches := thenEnd.Term != nil && isBrTo(thenEnd.Term, mergeBlock)
	elseReaches := elseEnd.Term != nil && isBrTo(elseEnd.Term, mergeBlock)
	if thenReaches || elseReaches {
		return nil
	}
	return c.emitDivergedMergeValue(mergeBlock, ifResultType)
}

// isBrTo reports whether an unconditional branch terminator targets block b.
func isBrTo(term ir.Terminator, b *ir.Block) bool {
	if br, ok := term.(*ir.TermBr); ok {
		return br.Target == b
	}
	return false
}

// --- Error handling expressions ---

// genErrorPropagateExpr generates the `expr^` operator.
// Evaluates the inner failable call, checks the tag, propagates the error
// to the caller on error, or extracts the Ok value on success.
func (c *Compiler) genErrorPropagateExpr(e *ast.ErrorPropagateExpr) value.Value {
	result := c.genExpr(e.Expr)
	calleeResultType := result.Type().(*irtypes.StructType)

	tag := c.block.NewExtractValue(result, 0)

	propagateBlock := c.newBlock("error.propagate")
	okBlock := c.newBlock("error.ok")
	c.block.NewCondBr(tag, propagateBlock, okBlock)

	// Error path: cleanup stmt temps + scope bindings, extract error, propagate
	c.block = propagateBlock
	c.emitAllStmtTempCleanupForErrorPath() // T0103/T1272: free all temp kinds before returning
	if len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, true) // error in flight — suppress close errors
	}
	errVal := c.block.NewExtractValue(result, resultErrIdx(calleeResultType))
	if c.inFailableGoBlock {
		// T1384: store the error into the goroutine's result aggregate and branch
		// to the coroutine's final suspend (a `ret` is invalid in the coro ramp).
		c.emitFailableGoBlockError(errVal)
	} else if c.inGenerator && c.generatorCanError {
		// B0023: store error to generator error_slot and branch to final suspend
		c.emitGeneratorError(errVal)
	} else {
		callerResultType := c.currentResultType()
		c.block.NewRet(c.wrapError(errVal, callerResultType))
	}

	// Ok path: extract value
	c.block = okBlock
	if !isVoidResult(calleeResultType) {
		return c.block.NewExtractValue(result, 1)
	}
	return nil
}

// genErrorPanicExpr generates the `expr?!` operator.
// Evaluates the inner failable call, panics on error, or extracts the Ok value.
func (c *Compiler) genErrorPanicExpr(e *ast.ErrorPanicExpr) value.Value {
	result := c.genExpr(e.Expr)
	resultType := result.Type().(*irtypes.StructType)

	tag := c.block.NewExtractValue(result, 0)

	panicBlock := c.newBlock("error.panic")
	okBlock := c.newBlock("error.ok")
	c.block.NewCondBr(tag, panicBlock, okBlock)

	// Error: extract message from error instance, panic with it (T0142: include source location)
	c.block = panicBlock
	errMsg := c.block.NewExtractValue(result, resultErrIdx(resultType))
	c.emitErrorPanic(errMsg, e.Pos().File, e.Pos().Line)

	// Ok: extract value
	c.block = okBlock
	if !isVoidResult(resultType) {
		return c.block.NewExtractValue(result, 1)
	}
	return nil
}

// genErrorHandlerExpr generates the `expr ? binding { body }` operator.
// Evaluates the inner failable call, runs the handler on error (with optional
// error binding), or extracts the Ok value on success. Merges with phi if
// both branches produce values.
//
// For typed handlers (`? e is IoError { ... }`), an RTTI check is performed on
// the error instance. If the check fails, the error is propagated (in failable
// functions) or causes a panic (in non-failable functions).
func (c *Compiler) genErrorHandlerExpr(e *ast.ErrorHandlerExpr) value.Value {
	// Optional handler: T? ? { recovery } → T
	if c.info.OptionalHandlers[e] {
		return c.genOptionalHandlerExpr(e)
	}

	// T1272: snapshot the temp-tracking depth BEFORE evaluating the failable
	// subexpression, so the OK-path drain below touches only the temps THIS call
	// materializes (the suffix at/after the snapshot) — never sibling temps from
	// an enclosing expression (e.g. the `make_s()` arg in
	// `combine(make_s(), may_fail()? e {})`), which must outlive the handler and
	// be consumed/dropped by the enclosing statement. Draining a sibling here
	// frees it before the enclosing call reads it → use-after-free.
	preStmt := len(c.stmtTemps)
	preHeap := len(c.heapTemps)
	preEnv := len(c.envTemps)
	preEnum := len(c.enumCtorTemps)

	result := c.genExpr(e.Expr)
	resultType := result.Type().(*irtypes.StructType)

	tag := c.block.NewExtractValue(result, 0)

	handlerBlock := c.newBlock("error.handler")
	okBlock := c.newBlock("error.ok")
	mergeBlock := c.newBlock("error.merge")
	c.block.NewCondBr(tag, handlerBlock, okBlock)

	// T1325: The handler-block temp drain is now scoped to the failable call's OWN
	// temps (emitted inside the narrowing block below), NOT the whole tracking array.
	// The old unconditional `emitAllStmtTempCleanupForErrorPath()` here dropped
	// enclosing SIBLING temps too — but when the handler recovers-and-continues
	// (falls through to the merge instead of returning/raising), control re-enters the
	// enclosing call, which then reads a freed sibling → use-after-free. Siblings are
	// left for the enclosing statement to drop; divergence (return/raise inside the
	// handler) still drains the full arrays via genReturnStmt/genRaiseStmt.
	c.block = handlerBlock

	// T1272: The outer statement's argument temporaries (call results, constructor
	// results, string/closure/enum-ctor temps materialized for THIS failable call)
	// are still tracked. The error path dropped them above; the OK path relied on
	// statement-end cleanup, but the handler body's own statement boundaries drain
	// the shared tracking first — dropping them only on the handler path and
	// leaving the OK path un-dropped → leak. Emit the OK-path drops now and clear
	// the tracking so the handler body starts hermetic and statement-end doesn't
	// double-handle these temps.
	okContBlock := okBlock
	if len(c.stmtTemps) > preStmt || len(c.heapTemps) > preHeap || len(c.envTemps) > preEnv || len(c.enumCtorTemps) > preEnum {
		savedHandlerBlock := c.block
		c.block = okBlock
		// T1272: The failable expression's OWN result value is being produced on the
		// OK path (e.g. a constructor's caller-allocated instance, tracked as a
		// heapTemp BEFORE the call). Its ownership passes to the enclosing binding /
		// statement (which re-tracks and drops it after the merge), so it must NOT be
		// dropped by the arg-temp drain below — dropping it frees the value the caller
		// is about to consume (use-after-free + double-free). Claim it out of the
		// drain, leaving only the genuine argument temporaries to be freed. Only heap
		// user-type results are caller-pre-allocated into the pre-merge tracking;
		// string/vector/method results are materialized after the extract, so a
		// heap-temp claim is sufficient here. (The claim runs against the full arrays,
		// before they are narrowed below, so its runtime-pointer struct-match sees
		// every tracked temp.)
		if !isVoidResult(resultType) {
			okResult := c.block.NewExtractValue(result, 1)
			c.claimHeapTemp(okResult)
		}
		// T1272: Narrow each tracking slice to the failable call's OWN temps (the
		// suffix past the pre-eval snapshot) so the OK-path drain drops only those.
		// The sibling prefix is spliced back below, untouched.
		fullStmt, fullHeap := c.stmtTemps, c.heapTemps
		fullEnv, fullEnum := c.envTemps, c.enumCtorTemps
		c.stmtTemps, c.heapTemps = fullStmt[preStmt:], fullHeap[preHeap:]
		c.envTemps, c.enumCtorTemps = fullEnv[preEnv:], fullEnum[preEnum:]
		c.emitAllStmtTempCleanupForErrorPath()
		okContBlock = c.block
		c.block = savedHandlerBlock
		// T1325: Drain the failable call's OWN temps on the handler path too. The
		// tracking slices are still narrowed to the own-temps suffix here, so this
		// drops ONLY those (flags are runtime-guarded and the OK/handler blocks are
		// mutually exclusive → no double-free). Sibling temps stay tracked and are
		// dropped by the enclosing statement after the merge.
		c.emitAllStmtTempCleanupForErrorPath()
		// T1272: Drop the drained own-temps from tracking (truncate to the snapshot)
		// but KEEP the sibling prefix — and prune only the map entries that now point
		// past the truncated slices so downstream claims stay in bounds. Claimed
		// entries (index -1, from claimStringTemp/claimHeapTemp) and live sibling
		// entries (index < snapshot) are preserved.
		c.stmtTemps, c.heapTemps = fullStmt[:preStmt], fullHeap[:preHeap]
		c.envTemps, c.enumCtorTemps = fullEnv[:preEnv], fullEnum[:preEnum]
		pruneTempMapSuffix(c.stmtTempMap, preStmt)
		pruneTempMapSuffix(c.heapTempMap, preHeap)
		pruneTempMapSuffix(c.envTempMap, preEnv)
	}
	errVal := c.block.NewExtractValue(result, resultErrIdx(resultType))

	// T0770: For the regular (non-optional-wrapping) recovery path, the recovery
	// body yields the recovered type directly. Expose it as the target type so a
	// bare `none` recovery (`expr? e { none }` on a `T?`-typed failable) lowers to
	// the full optional struct, not a bare i1. The optional-recovery path (which
	// wraps the body's inner T) must not see this, so it is left nil there.
	var recoveredTargetType types.Type
	if !c.info.OptionalRecoveryHandlers[e] {
		rt := c.info.Types[e]
		if rt != nil && c.typeSubst != nil {
			rt = types.Substitute(rt, c.typeSubst)
		}
		recoveredTargetType = rt
	}

	// T0792: when the recovery/else body's result is consumed as a borrow
	// (`T&`/`T~`), genBlockValue must read the last expr as a pure alias — no
	// dup, no owned-temp tracking. Otherwise the inner expr's natural owned type
	// (e.g. `r.d[0]` → `string`) sets dupStringFieldAccess and allocates a copy
	// that the borrow bind site never takes ownership of → leak.
	borrowRecovery := recoveredTargetType != nil && isRefType(recoveredTargetType)

	var noMatchVal value.Value
	var noMatchEnd *ir.Block

	// For typed handlers, perform RTTI check before entering the handler body
	if e.TypeName != "" {
		var targetID int32
		if resolved := c.info.ErrorHandlerTypes[e]; resolved != nil {
			// Generic typed handler (e.g., DataError[string])
			var ok bool
			targetID, ok = c.resolveTypeID(resolved)
			if !ok {
				panic(fmt.Sprintf("codegen: cannot resolve type ID for %s in error handler", e.TypeName))
			}
		} else {
			// Non-generic typed handler
			targetNamed := c.lookupNamedType(e.TypeName)
			if targetNamed == nil {
				panic(fmt.Sprintf("codegen: undefined type %s in error handler", e.TypeName))
			}
			targetID = c.assignTypeID(targetNamed)
		}

		variantPtr := c.loadVariantPtr(errVal)
		rttiResult := c.block.NewCall(c.funcs["promise_type_is"],
			variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
		typeMatch := c.block.NewICmp(enum.IPredNE, rttiResult, constant.NewInt(irtypes.I32, 0))

		matchBlock := c.newBlock("error.typed.match")
		noMatchBlock := c.newBlock("error.typed.nomatch")
		c.block.NewCondBr(typeMatch, matchBlock, noMatchBlock)

		// No-match path: else body, panic (!), or propagate
		c.block = noMatchBlock
		if e.ElseBody != nil {
			// else clause: bind error and run else body (T0091: register for drop)
			savedElseScope := len(c.scopeBindings)
			prevElseLocal, hadPrevElseLocal := c.locals[e.ElseBinding]
			if e.ElseBinding != "" && e.ElseBinding != "_" {
				elseValStruct := c.reconstructErrorValue(errVal)
				alloca := c.createEntryAlloca(userValueType())
				alloca.SetName(c.uniqueLocalName(e.ElseBinding))
				c.block.NewStore(elseValStruct, alloca)
				c.locals[e.ElseBinding] = alloca
				c.registerErrorDrop(e.ElseBinding, alloca, types.TypError)
			} else {
				// No else binding — temporary for drop
				alloca := c.createEntryAlloca(userValueType())
				alloca.SetName(c.uniqueLocalName("_else_err_tmp"))
				elseValStruct := c.reconstructErrorValue(errVal)
				c.block.NewStore(elseValStruct, alloca)
				c.registerErrorDrop("_else_err_tmp", alloca, types.TypError)
			}
			savedTarget := c.targetType
			if recoveredTargetType != nil {
				c.targetType = recoveredTargetType
			}
			savedBorrow := c.borrowBlockResult
			c.borrowBlockResult = borrowRecovery
			noMatchVal = c.genBlockValue(e.ElseBody)
			c.borrowBlockResult = savedBorrow
			c.targetType = savedTarget
			elseDiverged := c.block.Term != nil
			if !elseDiverged {
				if len(c.scopeBindings) > savedElseScope {
					c.emitScopeCleanup(savedElseScope, false)
				}
				noMatchEnd = c.block
				c.block.NewBr(mergeBlock)
			}
			c.scopeBindings = c.scopeBindings[:savedElseScope]
			// T1605: remove the else binding from c.locals so it doesn't leak
			// into subsequent code (e.g., a go block that reuses the name).
			if e.ElseBinding != "" && e.ElseBinding != "_" {
				if hadPrevElseLocal {
					c.locals[e.ElseBinding] = prevElseLocal
				} else {
					delete(c.locals, e.ElseBinding)
				}
			}
		} else if e.PanicOnNomatch {
			// Explicit ! suffix: panic on non-matching error (T0142: include source location)
			c.emitErrorPanic(errVal, e.Pos().File, e.Pos().Line)
		} else if c.canError || (c.inGenerator && c.generatorCanError) || c.inFailableGoBlock {
			if len(c.scopeBindings) > 0 {
				c.emitScopeCleanup(0, true) // error in flight — suppress close errors
			}
			if c.inFailableGoBlock {
				// T1384: store the error into the goroutine's result aggregate and
				// branch to the coroutine's final suspend.
				c.emitFailableGoBlockError(errVal)
			} else if c.inGenerator && c.generatorCanError {
				// B0023: store error to generator error_slot and branch to final suspend
				c.emitGeneratorError(errVal)
			} else {
				callerResultType := c.currentResultType()
				c.block.NewRet(c.wrapError(errVal, callerResultType))
			}
		} else {
			// Should not be reached — sema rejects typed handlers in
			// non-failable functions without else or !
			panicMsg := c.makeGlobalString("unhandled error type")
			c.block.NewCall(c.funcs["promise_panic"], panicMsg)
			c.emitPanicReturn()
		}

		// Match path: continue to bind and run handler body
		c.block = matchBlock
	}

	// T0091/T0110: Register error binding for drop so the error instance (and its
	// string fields) are freed at handler scope exit. For typed catches, resolve
	// the concrete type's drop to free child-specific string fields. For re-raise
	// paths, genRaiseStmt clears the drop flag (T0086) before scope cleanup.
	savedHandlerScope := len(c.scopeBindings)

	// T0110: Resolve concrete error type for drop dispatch.
	// Typed catches use the child type's drop; untyped catches use base error.drop.
	// For generic error types (e.g., AppError[int]), pass the Instance type so
	// registerErrorDrop can use the monomorphized drop name.
	var errorDropType types.Type = types.TypError
	if e.TypeName != "" {
		if resolved := c.info.ErrorHandlerTypes[e]; resolved != nil {
			errorDropType = resolved
		} else if n := c.lookupNamedType(e.TypeName); n != nil {
			errorDropType = n
		}
	}

	prevHandlerLocal, hadPrevHandlerLocal := c.locals[e.Binding]
	if e.Binding != "" && e.Binding != "_" {
		valStruct := c.reconstructErrorValue(errVal)
		alloca := c.createEntryAlloca(userValueType())
		alloca.SetName(c.uniqueLocalName(e.Binding))
		c.block.NewStore(valStruct, alloca)
		c.locals[e.Binding] = alloca
		c.registerErrorDrop(e.Binding, alloca, errorDropType)
	} else {
		// No binding — create a temporary alloca so drop machinery can free it.
		alloca := c.createEntryAlloca(userValueType())
		alloca.SetName(c.uniqueLocalName("_err_tmp"))
		valStruct := c.reconstructErrorValue(errVal)
		c.block.NewStore(valStruct, alloca)
		c.registerErrorDrop("_err_tmp", alloca, errorDropType)
	}
	savedHandlerTarget := c.targetType
	if recoveredTargetType != nil {
		c.targetType = recoveredTargetType
	}
	savedHandlerBorrow := c.borrowBlockResult
	c.borrowBlockResult = borrowRecovery
	handlerVal := c.genBlockValue(e.Body)
	c.borrowBlockResult = savedHandlerBorrow
	c.targetType = savedHandlerTarget
	// Emit drop for the error binding after handler body (scope cleanup).
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedHandlerScope {
		c.emitScopeCleanup(savedHandlerScope, false)
	}
	c.scopeBindings = c.scopeBindings[:savedHandlerScope]
	// T1605: remove the handler binding from c.locals so it doesn't leak
	// into subsequent code (e.g., a go block that reuses the name).
	if e.Binding != "" && e.Binding != "_" {
		if hadPrevHandlerLocal {
			c.locals[e.Binding] = prevHandlerLocal
		} else {
			delete(c.locals, e.Binding)
		}
	}
	handlerEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Ok path: extract value (continue after any T1272 ok-path temp drops)
	c.block = okContBlock
	var okVal value.Value
	if !isVoidResult(resultType) {
		okVal = c.block.NewExtractValue(result, 1)
	}

	// Optional recovery: wrap ok value as some(T), non-recovering paths produce none.
	if c.info.OptionalRecoveryHandlers[e] {
		semaType := c.info.Types[e]
		if c.typeSubst != nil {
			semaType = types.Substitute(semaType, c.typeSubst)
		}
		optLLVM := c.resolveType(semaType)
		optStructType, _ := optLLVM.(*irtypes.StructType)

		// Wrap ok value as some(T) in the ok block.
		if optStructType != nil && okVal != nil {
			okVal = c.wrapOptional(okVal, optStructType)
		}
		c.block.NewBr(mergeBlock)
		okEnd := c.block

		noneVal := c.zeroValue(optLLVM)

		// Wrap handler value in its block (before its br to merge).
		var handlerOptVal value.Value = noneVal
		handlerReachesMerge := false
		// B0353: Only consider handler as reaching merge if its br targets mergeBlock.
		if handlerEnd.Term != nil {
			if br, isBr := handlerEnd.Term.(*ir.TermBr); isBr && br.Target == mergeBlock {
				handlerReachesMerge = true
				if handlerVal != nil {
					if _, isVoid := handlerVal.Type().(*irtypes.VoidType); !isVoid {
						// Insert wrapOptional before the existing br terminator.
						savedBlock := c.block
						c.block = handlerEnd
						handlerEnd.Term = nil // remove br temporarily
						handlerOptVal = c.wrapOptional(handlerVal, optStructType)
						c.block.NewBr(mergeBlock) // re-add br
						c.block = savedBlock
					}
				}
			}
		}

		// Wrap noMatch value in its block.
		var noMatchOptVal value.Value = noneVal
		noMatchReachesMerge := false
		if noMatchEnd != nil {
			noMatchReachesMerge = true
			if noMatchVal != nil {
				if _, isVoid := noMatchVal.Type().(*irtypes.VoidType); !isVoid {
					savedBlock := c.block
					c.block = noMatchEnd
					noMatchEnd.Term = nil
					noMatchOptVal = c.wrapOptional(noMatchVal, optStructType)
					c.block.NewBr(mergeBlock)
					c.block = savedBlock
				}
			}
		}

		c.block = mergeBlock
		var incomings []*ir.Incoming
		incomings = append(incomings, &ir.Incoming{X: okVal, Pred: okEnd})
		if handlerReachesMerge {
			incomings = append(incomings, &ir.Incoming{X: handlerOptVal, Pred: handlerEnd})
		}
		if noMatchReachesMerge {
			incomings = append(incomings, &ir.Incoming{X: noMatchOptVal, Pred: noMatchEnd})
		}

		if len(incomings) > 1 {
			return mergeBlock.NewPhi(incomings...)
		}
		return okVal
	}

	c.block.NewBr(mergeBlock)
	okEnd := c.block

	// Merge with phi if both paths produce compatible values.
	// Treat void-typed values as nil (void call results cannot participate in phi).
	c.block = mergeBlock
	if handlerVal != nil {
		if _, isVoid := handlerVal.Type().(*irtypes.VoidType); isVoid {
			handlerVal = nil
		}
	}
	if noMatchVal != nil {
		if _, isVoid := noMatchVal.Type().(*irtypes.VoidType); isVoid {
			noMatchVal = nil
		}
	}
	if okVal != nil && handlerVal != nil {
		incomings := []*ir.Incoming{
			{X: okVal, Pred: okEnd},
			{X: handlerVal, Pred: handlerEnd},
		}
		if noMatchEnd != nil && noMatchVal != nil {
			incomings = append(incomings, &ir.Incoming{X: noMatchVal, Pred: noMatchEnd})
		}
		return mergeBlock.NewPhi(incomings...)
	}
	// okVal defined in okBlock doesn't dominate mergeBlock when handler also
	// reaches mergeBlock. Use a phi with a zero default from the handler path.
	if okVal != nil && handlerEnd.Term != nil {
		// B0353: Only add handler PHI entry if it actually branches to mergeBlock.
		// A return inside the handler (e.g., goroutine return) may branch elsewhere.
		if br, isBr := handlerEnd.Term.(*ir.TermBr); isBr && br.Target == mergeBlock {
			zeroVal := c.zeroValue(okVal.Type())
			incomings := []*ir.Incoming{
				{X: okVal, Pred: okEnd},
				{X: zeroVal, Pred: handlerEnd},
			}
			if noMatchEnd != nil {
				noMatchZero := c.zeroValue(okVal.Type())
				incomings = append(incomings, &ir.Incoming{X: noMatchZero, Pred: noMatchEnd})
			}
			return mergeBlock.NewPhi(incomings...)
		}
	}
	return okVal
}

// reconstructErrorValue builds a value struct {vtable_ptr, instance_ptr} from a raw i8* error pointer.
func (c *Compiler) reconstructErrorValue(errPtr value.Value) value.Value {
	vtablePtr := c.loadVtablePtrFromInstance(errPtr)
	var valStruct value.Value = constant.NewZeroInitializer(userValueType())
	valStruct = c.block.NewInsertValue(valStruct, vtablePtr, 0)
	valStruct = c.block.NewInsertValue(valStruct, errPtr, 1)
	return valStruct
}
