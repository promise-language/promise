package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- While loop ---

func (c *Compiler) genWhileStmt(s *ast.WhileStmt) {
	headerBlock := c.newBlock("while.header")
	bodyBlock := c.newBlock("while.body")
	exitBlock := c.newBlock("while.exit")

	c.block.NewBr(headerBlock)

	// Header: evaluate condition
	c.block = headerBlock
	cond := c.genExprAutoPropagate(s.Cond) // T1873
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock
	c.loopScopeDepth = len(c.scopeBindings)
	savedLoopTempFloor := c.enterLoopTempFloor() // T1331

	c.block = bodyBlock
	if c.shouldInstrument() {
		pos := s.Body.Pos()
		end := s.Body.End()
		idx := c.addCoverageRegion(pos.File, pos.Line, end.Line, c.currentCoverageFuncName(), "while.body")
		c.emitCoverageIncrement(idx)
	}
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(headerBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.loopTempFloor = savedLoopTempFloor // T1331
	c.block = exitBlock
}

// genWhileUnwrapStmt handles while-unwrap: while val := optExpr { }
// Each iteration evaluates the optional; loop continues while present.
func (c *Compiler) genWhileUnwrapStmt(s *ast.WhileUnwrapStmt) {
	headerBlock := c.newBlock("whileunwrap.header")
	bodyBlock := c.newBlock("whileunwrap.body")
	exitBlock := c.newBlock("whileunwrap.exit")

	c.block.NewBr(headerBlock)

	// Header: evaluate optional, check flag
	c.block = headerBlock
	// T0397: Same dup-on-read pattern as genIfUnwrapStmt — when iterating
	// over `Map[K, (droppable,...)]` indices, the inner tuple aliases bucket
	// data; without dupping, the binding's per-iteration drop would free the
	// map's storage.
	dupValType := c.info.Types[s.Value]
	if c.typeSubst != nil {
		dupValType = types.Substitute(dupValType, c.typeSubst)
	}
	if opt, ok := dupValType.(*types.Optional); ok {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		if _, isTup := elem.(*types.Tuple); isTup && c.tupleNeedsDrop(elem) {
			c.dupTupleFieldAccess = true
		}
		// T0440: Same dup-on-read for Optional[heap-user-type] in while-let.
		if isDroppableHeapUserType(elem) {
			c.dupHeapUserFieldAccess = true
		}
	}
	optVal := c.genExpr(s.Value)
	c.dupTupleFieldAccess = false
	c.dupHeapUserFieldAccess = false
	// T0770: auto-propagate a failable scrutinee so optVal is the unwrapped `T?`
	// optional, not the raw failable result struct (mirrors genIfUnwrapStmt).
	if c.info.AutoPropagateExprs[s.Value] {
		optVal = c.genAutoPropagateValue(optVal)
	}
	flag := c.block.NewExtractValue(optVal, 0)
	c.block.NewCondBr(flag, bodyBlock, exitBlock)

	// Body: unwrap value, bind to local
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock
	c.loopScopeDepth = len(c.scopeBindings)
	savedLoopTempFloor := c.enterLoopTempFloor() // T1331

	c.block = bodyBlock
	innerVal := c.block.NewExtractValue(optVal, 1)
	innerType := innerVal.Type()
	alloca := c.createEntryAlloca(innerType) // B0153: must be in entry block
	alloca.SetName(c.uniqueLocalName(s.Binding))
	c.block.NewStore(innerVal, alloca)
	prev, hadPrev := c.locals[s.Binding]
	c.locals[s.Binding] = alloca

	// B0215: Register drop binding for the unwrapped inner value. Each iteration
	// gets a new value; the drop flag is set to 1 in the body block (via
	// maybeRegisterDrop's store in c.block) so it resets correctly per iteration.
	// The binding is above loopScopeDepth, so break/continue paths include it.
	unwrapScopeLen := len(c.scopeBindings)
	savedDropFlag, hadDropFlag := c.dropFlags[s.Binding]
	savedDropBinding, hadDropBinding := c.dropBindings[s.Binding]
	// T0512: Snapshot the match-borrow marker (see genIfUnwrapStmt). Reverted
	// at body end so the marker does not leak past the loop. Safe on nil map.
	savedBorrowMark, hadBorrowMark := c.matchBorrowedIdents[s.Binding]
	valType := c.info.Types[s.Value]
	if c.typeSubst != nil {
		valType = types.Substitute(valType, c.typeSubst)
	}
	if opt, ok := valType.(*types.Optional); ok && c.isOwnedOptionalExpr(s.Value) {
		// T0391: When unwrapping a nested Optional (T?? → T?), the element type is
		// itself Optional and needs an Optional drop binding so its inner heap value
		// is freed at scope exit (or transferred ownership to a further unwrap).
		elemType := opt.Elem()
		if c.typeSubst != nil {
			elemType = types.Substitute(elemType, c.typeSubst)
		}
		// T0585: Load source's drop flag value before maybeRegister* / clearDropFlag
		// so we can mirror the source's ownership state into the binding.
		var srcFlagVal value.Value
		if ident, isIdent := s.Value.(*ast.IdentExpr); isIdent {
			if srcFlag, has := c.dropFlags[ident.Name]; has {
				srcFlagVal = c.block.NewLoad(irtypes.I1, srcFlag)
			}
		}
		if innerOpt, ok := elemType.(*types.Optional); ok {
			c.maybeRegisterOptionalDrop(s.Binding, alloca, innerOpt)
		} else {
			c.maybeRegisterDrop(s.Binding, alloca, elemType)
		}
		// Only transfer ownership if the unwrapped binding got a drop registered.
		// B0246: Structural interfaces don't get drops via maybeRegisterDrop — the
		// source must retain ownership for its Optional drop (RTTI-based) to handle cleanup.
		if _, innerHasDrop := c.dropBindings[s.Binding]; innerHasDrop {
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				// T0585: Propagate source's pre-clear drop flag into the binding's
				// drop flag only when the source had a flag. See genIfUnwrapStmt
				// for the rationale — no-flag source is ambiguous at the callee.
				if srcFlagVal != nil {
					if bindingFlag, has := c.dropFlags[s.Binding]; has {
						c.block.NewStore(srcFlagVal, bindingFlag)
					}
				}
				c.clearDropFlag(ident.Name)
			}
		}

		// T1288: Mirror of the if-let fix — a non-value structural inner unwrapped
		// from an owned optional temp (call/operator result) is skipped by
		// maybeRegisterDrop and gets no ownership transfer, so the box leaks.
		// Register an RTTI-dispatched structural free, gated to fresh-owned-temp
		// sources so an IdentExpr/field-borrow MemberExpr source (which keeps its
		// own owner) is not double-freed. T1289: a getter-call MemberExpr and a
		// user-defined `[]` IndexExpr are now correctly recognized as fresh-owned.
		// The binding is above unwrapScopeLen, so the per-iteration
		// emitScopeCleanup frees it each iteration.
		if _, already := c.dropBindings[s.Binding]; !already {
			if en := extractNamed(elemType); en != nil && en.IsStructural() && !en.IsValueType() {
				if c.isFreshOwnedStructuralRHS(s.Value) {
					c.maybeRegisterStructuralParamFree(s.Binding, alloca, elemType)
				}
			}
		}
	}

	// T0512: A match-borrowed source means the unwrapped binding still
	// aliases variant-owned memory; mark it borrowed so a further
	// if-let/while-let on this binding does not transfer ownership.
	if ident, isIdent := s.Value.(*ast.IdentExpr); isIdent &&
		c.matchBorrowedIdents != nil && c.matchBorrowedIdents[ident.Name] {
		c.matchBorrowedIdents[s.Binding] = true
	}

	c.genBlock(s.Body)

	// B0215: Emit drop for the unwrapped value at iteration end (fall-through).
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > unwrapScopeLen {
		cap := c.emitScopeCleanup(unwrapScopeLen, false)
		c.emitCloseErrCheck(cap, unwrapScopeLen)
	}
	c.scopeBindings = c.scopeBindings[:unwrapScopeLen]

	if c.block.Term == nil {
		c.block.NewBr(headerBlock)
	}

	// Remove binding from scope (it's only visible in the loop body)
	if hadPrev {
		c.locals[s.Binding] = prev
	} else {
		delete(c.locals, s.Binding)
	}
	// B0215: Restore drop flag/binding state.
	if hadDropFlag {
		c.dropFlags[s.Binding] = savedDropFlag
	} else {
		delete(c.dropFlags, s.Binding)
	}
	if hadDropBinding {
		c.dropBindings[s.Binding] = savedDropBinding
	} else {
		delete(c.dropBindings, s.Binding)
	}
	// T0512: Revert the borrow marker propagated for this binding.
	if hadBorrowMark {
		c.matchBorrowedIdents[s.Binding] = savedBorrowMark
	} else if c.matchBorrowedIdents != nil {
		delete(c.matchBorrowedIdents, s.Binding)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.loopTempFloor = savedLoopTempFloor // T1331
	c.block = exitBlock
}

// --- Classic for loop ---

func (c *Compiler) genClassicForStmt(s *ast.ClassicForStmt) {
	// Init: declare the loop variable.
	// T1257: delegate to the normal var-decl codegen (same technique T1192 uses
	// for the update clause) instead of hand-rolling the store. The hand-rolled
	// path bypassed claimStringTemp (leaving the RHS statement-temp drop flag
	// armed → stray double-drop of an owned handle inside the loop body ⇒
	// segfault/hang) and maybeRegisterDrop (no flag-guarded scope drop → a value
	// consumed in the re-evaluated condition had no flag to clear, and a
	// never-consumed owned init value leaked). The sema Types/AutoPropagate maps
	// are keyed on the reused InitValue expr node and InitName/InitType by value,
	// so a reconstructed decl node resolves identically.
	if s.InitValue != nil {
		if s.InitType != nil {
			c.genTypedVarDecl(&ast.TypedVarDecl{Type: s.InitType, Name: s.InitName, Value: s.InitValue})
		} else {
			c.genInferredVarDecl(&ast.InferredVarDecl{Name: s.InitName, Value: s.InitValue})
		}
		// T1257: the init clause is a statement boundary but is codegen'd inline
		// here rather than via genStmt, so it must run the same statement-temp
		// cleanup genStmt performs after every statement. Without this, an
		// unclaimed init-RHS temp (e.g. `for int n = makeVec().len; ...` — the
		// owned Vector is discarded by `.len`) is drained by the trailing
		// cleanupStmtLevelTemps in the update block instead. A zero-iteration loop
		// never reaches the update block, so that temp leaks. Draining it here
		// (right after the decl, exactly as genStmt would) fixes the leak; the
		// init var's own value is already claimed into its scope-drop binding, so
		// this only frees genuinely-unclaimed intermediates.
		c.cleanupStmtLevelTemps()
	}

	headerBlock := c.newBlock("for.header")
	bodyBlock := c.newBlock("for.body")
	updateBlock := c.newBlock("for.update")
	exitBlock := c.newBlock("for.exit")

	c.block.NewBr(headerBlock)

	// Header: evaluate condition
	c.block = headerBlock
	cond := c.genExprAutoPropagate(s.Cond) // T1873
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)
	savedLoopTempFloor := c.enterLoopTempFloor() // T1331

	c.block = bodyBlock
	if c.shouldInstrument() {
		pos := s.Body.Pos()
		end := s.Body.End()
		idx := c.addCoverageRegion(pos.File, pos.Line, end.Line, c.currentCoverageFuncName(), "for.body")
		c.emitCoverageIncrement(idx)
	}
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update
	c.block = updateBlock
	if s.UpdateIncDec {
		// Inc/dec update: target++ or target--
		c.genIncDecTarget(s.UpdateTarget, s.UpdateIsInc)
	} else if s.UpdateTarget != nil {
		if s.UpdateOp == ast.OpAssign {
			// T1192: a plain reassignment in the update clause must go through the
			// full assignment path (drop-old + RHS temp claim + move/drop-flag
			// bookkeeping). A raw store double-frees/segfaults an owned heap value
			// and leaks strings/vectors. The sema Types/AutoPropagate maps are keyed
			// on the original UpdateTarget/UpdateValue expr nodes, which are reused
			// verbatim, so they resolve unchanged. genStmt also emits the
			// per-iteration statement-temp cleanup in the update block.
			c.genStmt(&ast.AssignStmt{
				Target: s.UpdateTarget,
				Op:     s.UpdateOp,
				Value:  s.UpdateValue,
			})
		} else {
			// Compound update: target op= value
			updateVal := c.genExpr(s.UpdateValue)
			ident, ok := s.UpdateTarget.(*ast.IdentExpr)
			if ok {
				alloca, ok := c.locals[ident.Name]
				if ok {
					current := c.block.NewLoad(alloca.ElemType, alloca)
					result := c.genCompoundOp(s.UpdateOp, c.info.Types[s.UpdateTarget], current, updateVal)
					// T0715: a non-native operator returns a FRESH value; drop the
					// old heap user-type / droppable-enum value (alias-guarded;
					// no-op for value types/scalars) to preserve the zero-leak policy.
					// (The RHS operator-argument temp is drained by the trailing
					// cleanupStmtLevelTemps call below — T0988.)
					// T1194: flag-aware so a borrow-by-default heap param reassigned in
					// a compound for-update is not double-freed and its fresh value is
					// tracked for drop at scope exit.
					c.dropOldUserValueAtIdentSlot(ident.Name, alloca, c.info.Types[s.UpdateTarget], result)
					c.block.NewStore(result, alloca)
				}
			}
		}
	} else if s.UpdateValue != nil {
		// Expression-only update
		c.genExpr(s.UpdateValue)
	}
	// T0988: the update clause is a statement boundary but is codegen'd inline
	// here rather than via genStmt, so it must run the same statement-temp
	// cleanup — otherwise any heap temporary produced by the update expression
	// (or the RHS of a compound update) leaks once per iteration. The compound
	// store-back result is not tracked as a temp (genCompoundOp deliberately
	// omits result tracking), so this only drains the unclaimed argument temps.
	c.cleanupStmtLevelTemps()
	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.loopTempFloor = savedLoopTempFloor // T1331
	c.block = exitBlock
}

// --- Infinite loop ---

func (c *Compiler) genInfiniteLoop(s *ast.InfiniteLoop) {
	bodyBlock := c.newBlock("loop.body")
	exitBlock := c.newBlock("loop.exit")

	c.block.NewBr(bodyBlock)

	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = bodyBlock
	c.loopScopeDepth = len(c.scopeBindings)
	savedLoopTempFloor := c.enterLoopTempFloor() // T1331

	c.block = bodyBlock
	if c.shouldInstrument() {
		pos := s.Body.Pos()
		end := s.Body.End()
		idx := c.addCoverageRegion(pos.File, pos.Line, end.Line, c.currentCoverageFuncName(), "loop.body")
		c.emitCoverageIncrement(idx)
	}
	c.genBlock(s.Body)
	if c.block != nil && c.block.Term == nil {
		c.emitYieldCheck()
		c.block.NewBr(bodyBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.loopTempFloor = savedLoopTempFloor // T1331
	c.block = exitBlock
}

// --- Cooperative preemption yield check ---

// emitYieldCheck emits an inline cooperative yield check at loop back-edges.
// Only active when c.inCoroutine is true (inside goroutine/main coroutine).
// Checks G.preempt flag set by sysmon; if set, clears it, re-enqueues self,
// and calls coro.suspend to yield to the scheduler.
func (c *Compiler) emitYieldCheck() {
	if !c.inCoroutine {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return
	}

	gTy := goroutineStructType()

	// Load current G
	curG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
	gPtr := c.block.NewBitCast(curG, irtypes.NewPointer(gTy))

	// Load G.preempt
	preemptField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPreempt)))
	preemptVal := c.block.NewLoad(irtypes.I8, preemptField)
	shouldYield := c.block.NewICmp(enum.IPredNE, preemptVal, constant.NewInt(irtypes.I8, 0))

	yieldBlk := c.newBlock("yield")
	continueBlk := c.newBlock("yield.cont")
	c.block.NewCondBr(shouldYield, yieldBlk, continueBlk)

	// yield: clear preempt, coro.suspend (scheduler re-enqueues after suspend)
	//
	// IMPORTANT: We must NOT enqueue self before coro.suspend. If we did,
	// another M could pick up G from the run queue and call coro.resume
	// before our coro.suspend completes — that's UB in LLVM's coroutine model.
	// Instead, we just suspend. The scheduler detects a yield (park_mutex==null)
	// and re-enqueues the goroutine after coro.suspend has fully completed.
	c.block = yieldBlk
	c.block.NewStore(constant.NewInt(irtypes.I8, 0), preemptField)

	// Suspend — scheduler detects yield (null park_mutex) and re-enqueues us
	suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
	c.block.NewSwitch(suspResult, c.coroSuspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), continueBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

	// yield.cont: continue with the loop
	c.block = continueBlk
}

// --- Break / Continue ---

func (c *Compiler) genBreakStmt() {
	if c.breakTarget != nil {
		c.drainLoopExitTemps() // T1331
		// Close use bindings added within the loop body
		if len(c.scopeBindings) > c.loopScopeDepth {
			cap := c.emitScopeCleanup(c.loopScopeDepth, false)
			c.emitCloseErrCheck(cap, c.loopScopeDepth)
		}
		c.block.NewBr(c.breakTarget)
	}
}

func (c *Compiler) genContinueStmt() {
	if c.continueTarget != nil {
		c.drainLoopExitTemps() // T1331
		// Close use bindings added within the loop body
		if len(c.scopeBindings) > c.loopScopeDepth {
			cap := c.emitScopeCleanup(c.loopScopeDepth, false)
			c.emitCloseErrCheck(cap, c.loopScopeDepth)
		}
		c.block.NewBr(c.continueTarget)
	}
}

// enterLoopTempFloor records the current temp-array depths as the innermost
// loop's temp floor (T1331) and returns the previous floor for restoration at
// loop exit. Called at each loop's body entry alongside c.loopScopeDepth.
func (c *Compiler) enterLoopTempFloor() [4]int {
	saved := c.loopTempFloor
	c.loopTempFloor = [4]int{len(c.stmtTemps), len(c.heapTemps), len(c.envTemps), len(c.enumCtorTemps)}
	return saved
}

// drainLoopExitTemps drops unclaimed temps created inside the current loop body
// before a break/continue abandons its remainder mid-evaluation (T1331). Drains
// down to c.loopTempFloor (the temp-array depths at the loop's entry), NOT from
// 0: a break/continue only abandons the enclosing statement up to the target
// loop. Temps below the floor belong to expressions that enclose the loop (e.g.
// a sibling call argument when the loop is nested inside a block-value arg) and
// must survive — draining them would free a value the post-loop code still uses.
// For the T1331 case (loop OUTSIDE the enclosing call) the sibling temp is
// created inside the loop body, so it sits at/above the floor and is dropped —
// closing the leak. Drops are flag-guarded (B0198), and genBlockValue's deferred
// prefix rebuild restores tracking for the non-divergent path, so the divergent
// and non-divergent paths never double-free.
func (c *Compiler) drainLoopExitTemps() {
	if c.block == nil || c.block.Term != nil {
		return
	}
	c.cleanupStmtTempsFrom(c.loopTempFloor[0])
	c.cleanupHeapTempsFrom(c.loopTempFloor[1])
	c.cleanupEnvTempsFrom(c.loopTempFloor[2])
	c.drainEnumCtorTempsFrom(c.loopTempFloor[3])
}
