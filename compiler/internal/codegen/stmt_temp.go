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

// cleanupStmtLevelTemps drains all statement-scoped temporaries at a statement
// boundary: unclaimed string/vector/channel stmtTemps (T0073), heap-instance
// temps (T0088), closure-env temps (T0100), and inline enum-constructor temps
// (B0267/B0269). Runs only when the current block is still open. Extracted from
// genStmt so non-genStmt statement boundaries — e.g. the C-style for-update
// clause (T0988) — can share the exact same cleanup.
func (c *Compiler) cleanupStmtLevelTemps() {
	if c.block == nil || c.block.Term != nil {
		return
	}
	// T1329: inside a block-value body the floors are non-zero, protecting the
	// enclosing expression's sibling temps (materialized before the body) from a
	// leading statement's mid-body drain. 0 elsewhere → full drain.
	c.cleanupStmtTempsFrom(c.blockTempFloorStmt)
	c.cleanupHeapTempsFrom(c.blockTempFloorHeap)
	c.cleanupEnvTempsFrom(c.blockTempFloorEnv) // T0100
	// B0267/B0269: Drop all inline enum constructor temps not consumed by a variable.
	c.drainEnumCtorTempsFrom(c.blockTempFloorEnum)
}

// drainEnumCtorTemps emits a flag-guarded drop for every tracked inline
// enum-constructor temp, then clears the list. Each temp's drop flag was set at
// construction and cleared at any move site that consumed it (var binding, moved
// call arg), so only temps still owned here are actually freed. Shared by the
// statement-boundary drain (cleanupStmtLevelTemps, B0267/B0269) and the
// return-path drain (genReturnStmt, T1317). The caller must ensure c.block is
// open (non-nil, no terminator).
func (c *Compiler) drainEnumCtorTemps() {
	c.drainEnumCtorTempsFrom(0)
}

// drainEnumCtorTempsFrom drops inline enum-constructor temps at or above index
// `floor`, leaving the prefix [0:floor) intact (T1329 — see cleanupStmtTempsFrom).
func (c *Compiler) drainEnumCtorTempsFrom(floor int) {
	for _, et := range c.enumCtorTemps[floor:] {
		flag := c.block.NewLoad(irtypes.I1, et.dropFlag)
		dropBlk := c.newBlock("enum.ctor.drop")
		skipBlk := c.newBlock("enum.ctor.skip")
		c.block.NewCondBr(flag, dropBlk, skipBlk)
		c.block = dropBlk
		ptr := c.block.NewLoad(irtypes.I8Ptr, et.alloca)
		c.block.NewCall(et.dropFunc, ptr)
		c.block.NewBr(skipBlk)
		c.block = skipBlk
	}
	c.enumCtorTemps = c.enumCtorTemps[:floor]
}

// clearMovedOutEnumCtorTemps neutralizes and untracks the inline enum-ctor temps
// created while evaluating a move-out RHS (an enum constructor, or a match/if
// producing the enum) that is bound/assigned/stored by the current statement.
// The clear is BOUNDED to temps at/above blockTempFloorEnum: inside a block-value
// arm the floor protects the enclosing call's still-live sibling ctor-arg prefix
// (T1329) — sweeping it wholesale zeroes the sibling's drop flag (leak, T1338/T1339)
// and truncates enumCtorTemps below the floor, so the following floor-bounded
// statement-boundary drain (drainEnumCtorTempsFrom) slices out of range (panic,
// T1340). Outside a block-value the floor is 0 → unchanged from the old [:0] clear.
// Caller must have already checked enumCtorTempMovesOut(s.Value) and that
// len(c.enumCtorTemps) > c.blockTempFloorEnum.
func (c *Compiler) clearMovedOutEnumCtorTemps() {
	floor := c.blockTempFloorEnum
	for i := floor; i < len(c.enumCtorTemps); i++ {
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[i].dropFlag)
	}
	c.enumCtorTemps = c.enumCtorTemps[:floor]
}

// drainNestedArmEnumCtorTemps drops inline enum-ctor temps created while evaluating
// a branch-arm result expression that is NOT itself a move-out enum constructor
// (T1326). Such temps are by-value call arguments the callee only borrows/dups
// (B0232) — the caller retains ownership, so they must be drained at the arm
// boundary. Otherwise the branch's statement-level clear (enumCtorTempMovesOut)
// sweeps them wholesale with the genuine arm-result ctors and orphans the payload.
// Move-out arm results (a direct ctor / nested move-out branch) are left tracked
// for the statement-level clear.
func (c *Compiler) drainNestedArmEnumCtorTemps(armExpr ast.Expr, snap int) {
	if len(c.enumCtorTemps) <= snap {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return // arm diverged; genReturnStmt/genRaiseStmt already drained the array
	}
	if c.enumCtorTempMovesOut(armExpr) {
		return // arm result IS moved out through the phi — keep for statement-level clear
	}
	c.drainEnumCtorTempsFrom(snap)
}

// drainNestedEnumCtorTemps drops inline enum-ctor temps created while evaluating
// a by-value call/ctor argument `sub` that the callee only borrows/dups (B0232),
// so the caller retains ownership and must free them. Called at the enum-ctor
// argument boundary (genEnumVariantCallLayout) with `snap` = len(enumCtorTemps)
// captured before `sub` was evaluated. T1338: without this, an inline ctor buried
// as a by-value arg INSIDE a moved-out ctor's own arguments (e.g.
// `Payload.Full(describe(Payload.Full(heap)))`) has its drop flag zeroed by the
// wholesale statement-level clear (which assumes every tracked temp is the
// moved-out result), orphaning its heap payload → leak.
//
// If `sub` is itself moved out (an enum ctor whose temp is the phi'd/payload
// result), its temp is kept tracked so the enclosing move-out site clears it
// (matching today's behavior — no double-free). A recursive genEnumVariantCallLayout
// for such a nested direct-ctor arg drains ITS own by-value arg temps.
func (c *Compiler) drainNestedEnumCtorTemps(sub ast.Expr, snap int) {
	if len(c.enumCtorTemps) <= snap {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return // diverged; genReturnStmt/genRaiseStmt already drained the array
	}
	if c.enumCtorTempMovesOut(sub) {
		return // sub IS moved out (into the payload / through the phi) — keep tracked
	}
	c.drainEnumCtorTempsFrom(snap)
}

// trackStringTemp registers a heap-allocated string temporary for cleanup at
// statement end (T0073). Entry-block allocas are initialized to null/false so
// temps created inside branches have defined values on all paths.
func (c *Compiler) trackStringTemp(val value.Value) {
	c.trackTempWithDrop(val, c.funcs["promise_string_drop"])
}

// trackVectorTemp registers a vector temporary for cleanup at statement end.
// B0219: Used for vector field-read dups from droppable types.
// T0109: Also used for vector-producing calls (e.g., split()) to drop string elements.
func (c *Compiler) trackVectorTemp(val value.Value) {
	c.trackTempWithDrop(val, c.funcs["Vector.drop"])
}

// trackVectorTempWithElemType registers a vector temporary with element type info.
// When elemType is non-nil and is string, the cleanup will also drop string elements
// before freeing the vector buffer. Delegates to trackTempWithDrop, then patches elemType.
func (c *Compiler) trackVectorTempWithElemType(val value.Value, elemType types.Type) {
	prevLen := len(c.stmtTemps)
	c.trackTempWithDrop(val, c.funcs["Vector.drop"])
	// If a new temp was actually added, set its element type.
	if len(c.stmtTemps) > prevLen {
		c.stmtTemps[len(c.stmtTemps)-1].elemType = elemType
	}
}

// trackArrayTemp registers a fixed-size array temporary (a call result of type
// T[N] used inline, never bound to a variable) for element-wise cleanup at
// statement end (T1181). Unlike trackTempWithDrop, the value is a `[N x T]` LLVM
// aggregate, not an i8* pointer — so it is stored into a dedicated [N x T]
// entry-block alloca and cleanupStmtTemps walks the elements via
// emitVariantFieldDrop (mirroring bindingDropArray / emitArrayDropCall). Claim
// (claimStringTemp) clears the drop flag when a binding takes ownership.
func (c *Compiler) trackArrayTemp(val value.Value, arr *types.Array) {
	if val == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if c.entryBlock == nil || !c.tempTrackingEnabled {
		return
	}
	if _, ok := c.stmtTempMap[val]; ok {
		return
	}
	llvmArrType := c.resolveType(arr)
	alloca := c.createEntryAlloca(llvmArrType)
	dropFlag := c.createEntryAlloca(irtypes.I1)
	c.entryBlock.NewStore(constant.NewZeroInitializer(llvmArrType), alloca)
	c.entryBlock.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)
	c.block.NewStore(val, alloca)
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	idx := len(c.stmtTemps)
	c.stmtTemps = append(c.stmtTemps, stmtTemp{alloca: alloca, dropFlag: dropFlag, arrType: arr})
	c.stmtTempMap[val] = idx
}

// registerTupleStmtTemp registers a droppable tuple temporary (a tuple literal
// or tuple-returning call result with no owning caller variable) for field-wise
// cleanup at statement end (T1233). The plan mirror of trackArrayTemp: the value
// is a tuple LLVM aggregate, not an i8* pointer, so it is stored into a dedicated
// alloca and cleanupStmtTemps walks the fields via emitVariantFieldDrop (the same
// walk emitTupleDropCall / bindingDropTuple use). Used where a plain (borrow)
// tuple param — which no longer drops its arg (T1233 superseded T0406's
// callee-drop) — receives a tuple temp, and for a discarded bare tuple statement.
// Not registered in stmtTempMap: these temps are always borrowed-by-callee /
// discarded, never transferred into a downstream binding, so no claim is needed.
func (c *Compiler) registerTupleStmtTemp(val value.Value, tup *types.Tuple) {
	if val == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if c.entryBlock == nil || !c.tempTrackingEnabled {
		return
	}
	llvmType := val.Type()
	alloca := c.createEntryAlloca(llvmType)
	dropFlag := c.createEntryAlloca(irtypes.I1)
	c.entryBlock.NewStore(constant.NewZeroInitializer(llvmType), alloca)
	c.entryBlock.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)
	c.block.NewStore(val, alloca)
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.stmtTemps = append(c.stmtTemps, stmtTemp{alloca: alloca, dropFlag: dropFlag, tupleType: tup})
}

// emitTupleTempDrop emits a conditional field-wise drop for a tuple statement
// temp (T1233). Mirrors emitArrayTempDrop: loads the tuple aggregate and walks
// its droppable fields via emitVariantFieldDrop, clearing the flag afterward so a
// temp reused across loop iterations isn't dropped twice.
func (c *Compiler) emitTupleTempDrop(temp stmtTemp) {
	typ := types.Type(temp.tupleType)
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	flag := c.block.NewLoad(irtypes.I1, temp.dropFlag)
	dropBlock := c.newBlock("tuptmp.drop")
	skipBlock := c.newBlock("tuptmp.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	tupVal := c.block.NewLoad(temp.alloca.ElemType, temp.alloca)
	c.emitVariantFieldDrop(tupVal, typ)
	c.block.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// tupleArgIsCallerOwnedTemp reports whether a tuple value produced by expr is a
// TEMPORARY the caller must drop (a literal or call result with no owning caller
// variable) rather than a borrow. An IdentExpr (an owned tuple variable drops via
// its own bindingDropTuple; a borrowed variable is owned elsewhere) and a plain
// container/field read (borrow) are NOT caller temps. Owned-return shapes ARE
// caller temps even behind Index/Member syntax: a getter returning a tuple by
// value (isGetterCallExpr) and a user-defined non-native `[]` returning a tuple
// (isUserIndexExpr) each produce a FRESH owned tuple the caller must free — a
// native container/array index instead aliases storage the container owns.
// Mirrors the owned-return exemptions in isClosureAggregateBorrow. T1233.
func (c *Compiler) tupleArgIsCallerOwnedTemp(expr ast.Expr) bool {
	e := unwrapDestructureParens(expr)
	switch e.(type) {
	case *ast.IndexExpr:
		return c.isUserIndexExpr(e) // user `[]` returns owned; native index borrows
	case *ast.MemberExpr:
		return c.isGetterCallExpr(e) // getter returns owned; plain field read borrows
	case *ast.IdentExpr:
		return false
	}
	return true
}

// trackChannelTempWithElemType registers a channel temporary for cleanup at
// statement end. B0219: used for channel field-read dups from droppable types.
// T0663: unlike trackVectorTempWithElemType (which patches stmtTemp.elemType so
// cleanupStmtTemps walks elements), the per-element-type Channel[T].drop already
// walks any un-received buffered items itself — so the element type only needs
// to select the right drop function. elemType is substituted here so callers
// can pass the raw channel element type.
func (c *Compiler) trackChannelTempWithElemType(val value.Value, elemType types.Type) {
	if elemType == nil {
		return
	}
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	c.trackTempWithDrop(val, c.getOrCreateChannelDrop(elemType))
}

// trackTempWithDrop registers a heap-allocated temporary (string/vector/channel)
// for cleanup at statement end using the specified drop function.
func (c *Compiler) trackTempWithDrop(val value.Value, dropFn *ir.Func) {
	if val == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if c.entryBlock == nil {
		return
	}
	// Only track values that are actually i8* (string/vector/channel pointers).
	// Failable calls return structs like {i1, i8*} — those are NOT temps.
	if val.Type() != irtypes.I8Ptr {
		return
	}
	// Only track when explicitly enabled for the current function (T0073).
	// Set to true in defineFunc for user-defined free functions only.
	if !c.tempTrackingEnabled {
		return
	}
	// Don't double-track the same SSA value
	if _, ok := c.stmtTempMap[val]; ok {
		return
	}

	// An ordinary temp is unconditionally live where it is created (flag = 1).
	c.appendStmtTemp(val, dropFn, nil, constant.NewInt(irtypes.I1, 1))
}

// appendStmtTemp records a statement temp for cleanup at statement end: an
// entry-block i8* alloca (init null) + i1 drop flag (init false) for defined
// values on untaken paths (B0168), then stores val + liveFlag in the CURRENT
// block. liveFlag is the i1 "this temp owns its value here" flag — a constant 1
// for ordinary temps (trackTempWithDrop), or a per-branch phi for elvis results
// whose per-path value is whether the result owns the selected buffer on that path
// (trackElvisResultTemp, T0936: own a transferred local/fresh operand, borrow a
// parameter/static one).
// elemType (vector element type, nil otherwise) drives the per-element drop loop
// in cleanupStmtTemps. Callers own the guards (tempTrackingEnabled, i8* type,
// terminated-block, double-track) before calling this.
func (c *Compiler) appendStmtTemp(val value.Value, dropFn *ir.Func, elemType types.Type, liveFlag value.Value) {
	// Create entry-block allocas via createEntryAlloca (handles coroutine layout).
	// The entry block's Insts list is separate from its Term, so appending stores
	// after allocas is safe.
	alloca := c.createEntryAlloca(irtypes.I8Ptr)
	dropFlag := c.createEntryAlloca(irtypes.I1)
	c.entryBlock.NewStore(constant.NewNull(irtypes.I8Ptr), alloca)
	c.entryBlock.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)

	// Store value and set flag in current block.
	c.block.NewStore(val, alloca)
	c.block.NewStore(liveFlag, dropFlag)

	// T1208: a non-constant liveFlag is a genuine per-path flag (an elvis/merge-result
	// flagPhi). Record it so an enclosing conditional threads this temp's live flag
	// instead of a whole-arm constant (which would drop a borrowed value on the
	// borrowed path). Ordinary temps (constant 1) keep perPathFlag=false.
	_, isConst := liveFlag.(constant.Constant)

	idx := len(c.stmtTemps)
	c.stmtTemps = append(c.stmtTemps, stmtTemp{alloca: alloca, dropFlag: dropFlag, dropFunc: dropFn, elemType: elemType, perPathFlag: !isConst})
	c.stmtTempMap[val] = idx
}

// claimStringTemp marks a tracked string temp as consumed (ownership transferred
// to a variable, constructor field, or container). Clears the drop flag so the
// temp won't be freed at statement end.
func (c *Compiler) claimStringTemp(val value.Value) {
	if val == nil {
		return
	}
	idx, ok := c.stmtTempMap[val]
	if !ok || idx < 0 {
		return
	}
	// Clear drop flag — ownership transferred
	c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.stmtTemps[idx].dropFlag)
	c.stmtTempMap[val] = -1
}

// cleanupStmtTemps drops all unclaimed string/vector/channel temps at statement end (T0073).
// For each temp: check flag → null-check ptr → call temp-specific drop function.
func (c *Compiler) cleanupStmtTemps() {
	c.cleanupStmtTempsFrom(0)
}

// cleanupStmtTempsFrom drops all unclaimed string/vector/channel temps at or
// above index `floor`, leaving the prefix [0:floor) intact. floor is 0 for every
// caller except cleanupStmtLevelTemps inside a block-value body (T1329), where it
// protects the enclosing expression's sibling temps from being freed mid-body.
func (c *Compiler) cleanupStmtTempsFrom(floor int) {
	// B0190: Clear per-statement flags that must not leak across statements.
	// Done before early returns so flags are always reset.
	c.optionalFieldString = false
	c.optionalFieldVector = false // T0354
	c.optionalStringDup = nil
	c.optionalContainerDup = nil // T0366
	c.optionalTupleDup = nil     // T0397
	c.optionalHeapDup = nil      // T0440
	if len(c.stmtTemps) <= floor {
		return
	}
	if c.block == nil || c.block.Term != nil {
		if floor == 0 {
			c.stmtTemps = c.stmtTemps[:0]
			c.stmtTempMap = make(map[value.Value]int)
			c.mergeBoundStructFlag = make(map[value.Value]*ir.InstAlloca) // T1211
		} else {
			c.stmtTemps = c.stmtTemps[:floor]
			pruneTempMapSuffix(c.stmtTempMap, floor)
		}
		return
	}

	for _, temp := range c.stmtTemps[floor:] {
		// T1181: fixed-array temp — element-wise drop, no i8* dropFunc.
		if temp.arrType != nil {
			c.emitArrayTempDrop(temp)
			continue
		}
		// T1233: tuple temp — field-wise drop via emitVariantFieldDrop, no i8* dropFunc.
		if temp.tupleType != nil {
			c.emitTupleTempDrop(temp)
			continue
		}
		// B0219: Each temp has its own drop function (string/vector/channel).
		if temp.dropFunc == nil {
			continue
		}
		flag := c.block.NewLoad(irtypes.I1, temp.dropFlag)
		dropBlock := c.newBlock("tmp.drop")
		skipBlock := c.newBlock("tmp.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		ptr := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
		isNull := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("tmp.exec")
		doneBlock := c.newBlock("tmp.done")
		c.block.NewCondBr(isNull, doneBlock, execBlock)

		c.block = execBlock
		// T0356: For vector temps, drop droppable elements (strings, enums with
		// droppable variants, heap user types, droppable tuples) before freeing
		// the vector buffer. Only set on sole-owner vector temps via
		// trackVectorTempWithElemType — shallow-dup field reads use trackVectorTemp
		// (no elemType) to avoid double-freeing shared elements.
		if temp.elemType != nil {
			c.emitVectorElementDropLoop(ptr, temp.elemType)
		}
		// T0668: a discarded Task statement-expr temp (e.g. `obj.task_getter;`
		// or `compute_task();`) reaches here un-awaited. In a coroutine body
		// route through the cooperative join so the single-threaded WASM
		// scheduler can run the pending goroutine; emitTaskJoinAndFreeByDropFn
		// returns false for non-Task temps (string/vector/channel) — those
		// keep the direct drop call.
		if !c.emitTaskJoinAndFreeByDropFn(ptr, temp.dropFunc) {
			c.block.NewCall(temp.dropFunc, ptr)
		}
		c.block.NewBr(doneBlock)

		c.block = doneBlock
		// B0172: Reset drop flag after dropping. Without this, in a loop where
		// a different match arm is taken on the next iteration, the stale flag=1
		// causes a double-free on the already-freed pointer.
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	}

	if floor == 0 {
		c.stmtTemps = c.stmtTemps[:0]
		c.stmtTempMap = make(map[value.Value]int)
		c.mergeBoundStructFlag = make(map[value.Value]*ir.InstAlloca) // T1211
	} else {
		// T1329: keep the sibling prefix [0:floor) and its map entries live; drain
		// only the suffix created within the block body. mergeBoundStructFlag is
		// keyed by value, so stale body entries are harmless — no reset needed.
		c.stmtTemps = c.stmtTemps[:floor]
		pruneTempMapSuffix(c.stmtTempMap, floor)
	}
}

// emitStmtTempCleanupForErrorPath emits cleanup IR for statement-level
// temps without resetting the tracking state (T0103). Used on error propagation
// paths where the error branch terminates (ret/unreachable) but the ok branch
// continues and still needs cleanup at statement end. The drop flags are stored
// in allocas, so each path independently checks and clears them at runtime.
func (c *Compiler) emitStmtTempCleanupForErrorPath() {
	if len(c.stmtTemps) == 0 {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return
	}

	for _, temp := range c.stmtTemps {
		// T1181: fixed-array temp — element-wise drop, no i8* dropFunc.
		if temp.arrType != nil {
			c.emitArrayTempDrop(temp)
			continue
		}
		// T1233: tuple temp — field-wise drop via emitVariantFieldDrop, no i8* dropFunc.
		if temp.tupleType != nil {
			c.emitTupleTempDrop(temp)
			continue
		}
		// B0219: Each temp has its own drop function (string/vector/channel).
		if temp.dropFunc == nil {
			continue
		}
		flag := c.block.NewLoad(irtypes.I1, temp.dropFlag)
		dropBlock := c.newBlock("err.tmp.drop")
		skipBlock := c.newBlock("err.tmp.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		ptr := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
		isNull := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("err.tmp.exec")
		doneBlock := c.newBlock("err.tmp.done")
		c.block.NewCondBr(isNull, doneBlock, execBlock)

		c.block = execBlock
		// T0356: Mirror cleanupStmtTemps — drop droppable vector elements
		// before freeing the vector buffer on the error-propagation path.
		if temp.elemType != nil {
			c.emitVectorElementDropLoop(ptr, temp.elemType)
		}
		// T0668: cooperative Task join (coroutine) — see cleanupStmtTemps.
		if !c.emitTaskJoinAndFreeByDropFn(ptr, temp.dropFunc) {
			c.block.NewCall(temp.dropFunc, ptr)
		}
		c.block.NewBr(doneBlock)

		c.block = doneBlock
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	}
}

// emitEnvTempCleanupForErrorPath emits cleanup IR for statement-level closure-env
// temps without resetting the tracking state. T1272: used at the error-handler
// split so the outer statement's env temps are dropped on both the error and ok
// paths (mirrors emitStmtTempCleanupForErrorPath / cleanupEnvTemps).
func (c *Compiler) emitEnvTempCleanupForErrorPath() {
	if len(c.envTemps) == 0 {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return
	}
	for _, temp := range c.envTemps {
		flag := c.block.NewLoad(irtypes.I1, temp.dropFlag)
		dropBlock := c.newBlock("err.env.drop")
		skipBlock := c.newBlock("err.env.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		ptr := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
		isNull := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("err.env.exec")
		doneBlock := c.newBlock("err.env.done")
		c.block.NewCondBr(isNull, doneBlock, execBlock)

		c.block = execBlock
		c.emitEnvDropOrFree(ptr) // B0221
		c.block.NewBr(doneBlock)

		c.block = doneBlock
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	}
}

// emitEnumCtorTempCleanupForErrorPath emits cleanup IR for inline enum-constructor
// temps without resetting the tracking state. T1272: used at the error-handler
// split so the outer statement's enum-ctor temps are dropped on both the error and
// ok paths (mirrors the enum loop in cleanupStmtLevelTemps).
func (c *Compiler) emitEnumCtorTempCleanupForErrorPath() {
	if len(c.enumCtorTemps) == 0 {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return
	}
	for _, et := range c.enumCtorTemps {
		flag := c.block.NewLoad(irtypes.I1, et.dropFlag)
		dropBlk := c.newBlock("err.enum.drop")
		skipBlk := c.newBlock("err.enum.skip")
		c.block.NewCondBr(flag, dropBlk, skipBlk)
		c.block = dropBlk
		ptr := c.block.NewLoad(irtypes.I8Ptr, et.alloca)
		c.block.NewCall(et.dropFunc, ptr)
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), et.dropFlag)
		c.block.NewBr(skipBlk)
		c.block = skipBlk
	}
}

// emitAllStmtTempCleanupForErrorPath emits flag-guarded cleanup IR for EVERY
// statement-level temp kind — string/vector/channel temps (stmtTemps), heap
// user-type instance temps (heapTemps), closure-env temps (envTemps), and inline
// enum-constructor temps (enumCtorTemps) — without resetting the tracking state.
// This is the single unwind-path cleanup for every error-in-flight site that
// abandons the current statement to propagate an error: bare-call auto-propagate,
// explicit `?^`, `raise`, the generator-factory error path, and the inline `? {}`
// handler split. T1272: previously each of those sites cleaned only
// stmtTemps+heapTemps, so a closure-env or inline-enum temp materialized in the
// abandoned statement (e.g. `take2(|x| -> x + cap, mayFail())`) leaked on the
// unwind — the env/enum kinds were freed only at the inline `? {}` handler split.
// Consolidating the four kinds behind one call keeps every site in lockstep so no
// future kind is dropped at one site and missed at another. Ordered
// stmt→heap→env→enum to match cleanupStmtLevelTemps.
//
// The TLS panic-flag-check unwind (emitPanicReturn, io.go) deliberately does NOT
// use this helper — see the comment there and T1318.
func (c *Compiler) emitAllStmtTempCleanupForErrorPath() {
	c.emitStmtTempCleanupForErrorPath()
	c.emitHeapTempCleanupForErrorPath()
	c.emitEnvTempCleanupForErrorPath()
	c.emitEnumCtorTempCleanupForErrorPath()
}

// pruneTempMapSuffix deletes value→index entries whose index points at or past
// `base` — the entries for temps that were truncated off the tail of a tracking
// slice. Entries below `base` (live siblings) and claimed entries (index -1) are
// left intact so downstream claim lookups stay valid and in bounds. T1272.
func pruneTempMapSuffix(m map[value.Value]int, base int) {
	for k, idx := range m {
		if idx >= base {
			delete(m, k)
		}
	}
}

// copyTempMap returns a shallow copy of a temp-tracking map (SSA value → index).
// Used by genBlockValue (T1329) to snapshot the sibling-temp prefix so it can be
// rebuilt if the block body diverges and drains the live map to a fresh one.
func copyTempMap(m map[value.Value]int) map[value.Value]int {
	out := make(map[value.Value]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// emitHeapTempCleanupForErrorPath emits cleanup IR for statement-level heap
// instance temps without resetting the tracking state (T0103). Same rationale
// as emitStmtTempCleanupForErrorPath.
func (c *Compiler) emitHeapTempCleanupForErrorPath() {
	if len(c.heapTemps) == 0 {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return
	}

	for _, temp := range c.heapTemps {
		flag := c.block.NewLoad(irtypes.I1, temp.dropFlag)
		dropBlock := c.newBlock("err.heap.drop")
		skipBlock := c.newBlock("err.heap.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		ptr := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
		isNull := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("err.heap.exec")
		doneBlock := c.newBlock("err.heap.done")
		c.block.NewCondBr(isNull, doneBlock, execBlock)

		c.block = execBlock
		// T0369: Mirror cleanupHeapTemps — walk droppable vector elements before
		// freeing the buffer on the error-propagation path.
		if temp.elemType != nil {
			c.emitVectorElementDropLoop(ptr, temp.elemType)
		}
		c.block.NewCall(temp.dropFunc, ptr)
		c.block.NewBr(doneBlock)

		c.block = doneBlock
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	}
}

// isHeapTempProducer returns true if expr produces a new unowned heap instance
// that must be tracked for cleanup (call results, error unwrap, auto-propagation).
// B0325: Expanded from CallExpr-only to cover ErrorPanicExpr, ErrorPropagateExpr,
// and auto-propagated expressions.
func (c *Compiler) isHeapTempProducer(expr ast.Expr) bool {
	switch expr.(type) {
	case *ast.CallExpr, *ast.ErrorPanicExpr, *ast.ErrorPropagateExpr:
		return true
	}
	return c.info.AutoPropagateExprs[expr]
}

// trackChainIntermediateReceiver tracks a method chain or field access intermediate
// receiver for cleanup at statement end (B0258, B0325). When the receiver of a
// method call or field access is itself a temporary (call result, error unwrap,
// auto-propagation), the intermediate heap-allocated value would leak without
// explicit tracking.
// receiverVal is the full value struct (for claiming existing constructor heapTemps).
// instancePtr is the extracted instance pointer (field 1 of receiverVal).
func (c *Compiler) trackChainIntermediateReceiver(memberTarget ast.Expr, receiverVal value.Value, instancePtr value.Value, named *types.Named, targetType types.Type) {
	if !c.tempTrackingEnabled || c.block == nil || c.block.Term != nil {
		return
	}
	// Only track when receiver is a temporary producer (B0325)
	if !c.isHeapTempProducer(memberTarget) {
		return
	}
	if named == nil {
		return
	}
	// Skip types already handled by other tracking systems
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return
	}
	if isContainerType(targetType) || named == types.TypString {
		return
	}
	// Bitcast typed instance pointer to i8* for heap temp tracking
	trackedPtr := instancePtr
	if trackedPtr.Type() != irtypes.I8Ptr {
		if _, isPtr := trackedPtr.Type().(*irtypes.PointerType); isPtr {
			trackedPtr = c.block.NewBitCast(trackedPtr, irtypes.I8Ptr)
		} else {
			return
		}
	}
	dropFunc := c.resolveDropFuncForTemp(named, targetType)
	if dropFunc != nil {
		// Claim any existing heapTemp for this receiver (e.g., from constructor
		// allocation tracking at T0135) to prevent double-free.
		c.claimHeapTemp(receiverVal)
		c.trackHeapTemp(trackedPtr, dropFunc)
	}
}

// trackVectorHeapTempWithElemType registers a vector heap buffer for cleanup at
// statement end with element-type info. When the vector literal is consumed as a
// transient (fn arg, for-in source, expression stmt), cleanupHeapTemps walks
// droppable elements via emitVectorElementDropLoop before freeing the buffer via
// Vector.drop. Mirrors trackVectorTempWithElemType for the heapTemp path. T0369.
//
// T0371: With genTupleLit now claiming heap-tracked field temps as the tuple
// takes ownership (string concats, heap user types, enum ctor temps), the
// element walk is safe even when the element type contains a droppable tuple —
// the buffer-walk is the unique drop site for those tuple fields. Returns true
// when the heap temp is configured to walk droppable elements at cleanup
// (elemType set). Callers gate ownership-transfer claims on this.
func (c *Compiler) trackVectorHeapTempWithElemType(rawPtr value.Value, elemType types.Type) bool {
	dropFn := c.funcs["Vector.drop"]
	if dropFn == nil {
		return false
	}
	prevLen := len(c.heapTemps)
	c.trackHeapTemp(rawPtr, dropFn)
	if len(c.heapTemps) > prevLen {
		c.heapTemps[len(c.heapTemps)-1].elemType = elemType
		return true
	}
	return false
}

// trackHeapTemp registers a heap-allocated droppable instance for cleanup at
// statement end (T0088). The instance pointer and drop function are stored so
// unclaimed temps can be dropped at statement end.
func (c *Compiler) trackHeapTemp(instancePtr value.Value, dropFunc *ir.Func) {
	c.trackHeapTempWithFlag(instancePtr, dropFunc, constant.NewInt(irtypes.I1, 1))
}

// trackHeapTempWithFlag is trackHeapTemp with a caller-supplied initial live-flag
// (an i1). Used by genElvis to register the elvis result with a per-branch flag
// (owned on the some path where the extracted inner is orphaned, not-owned on the
// none path where the default keeps its own owner). T0937.
func (c *Compiler) trackHeapTempWithFlag(instancePtr value.Value, dropFunc *ir.Func, flagVal value.Value) {
	if instancePtr == nil || dropFunc == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if c.entryBlock == nil || !c.tempTrackingEnabled {
		return
	}
	if instancePtr.Type() != irtypes.I8Ptr {
		return
	}
	if _, ok := c.heapTempMap[instancePtr]; ok {
		return // already tracked
	}

	alloca := c.createEntryAlloca(irtypes.I8Ptr)
	dropFlag := c.createEntryAlloca(irtypes.I1)

	c.entryBlock.NewStore(constant.NewNull(irtypes.I8Ptr), alloca)
	c.entryBlock.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)

	c.block.NewStore(instancePtr, alloca)
	c.block.NewStore(flagVal, dropFlag)

	idx := len(c.heapTemps)
	c.heapTemps = append(c.heapTemps, heapTemp{alloca: alloca, dropFlag: dropFlag, dropFunc: dropFunc})
	c.heapTempMap[instancePtr] = idx
}

// claimHeapTemp marks a tracked heap instance as consumed (ownership transferred
// to a variable). Clears the drop flag so the temp won't be dropped at statement end.
// Accepts either an i8* instance pointer or a value struct — extracts field 1
// (the instance pointer) at the LLVM level if needed.
func (c *Compiler) claimHeapTemp(val value.Value) {
	c.lastClaimedDropFunc = nil // T0127: reset before each claim attempt
	if val == nil || len(c.heapTemps) == 0 {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return
	}
	// Try direct match (i8* instance pointer)
	if idx, ok := c.heapTempMap[val]; ok && idx >= 0 {
		c.lastClaimedDropFunc = c.heapTemps[idx].dropFunc // T0127
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.heapTemps[idx].dropFlag)
		c.heapTempMap[val] = -1
		return
	}
	// For value structs ({vtable, instance}): extract field 1 and do a runtime
	// comparison against each tracked temp. This handles method call results
	// where maybeTrackIterTemp tracked the extractvalue but the caller has the
	// full value struct (different SSA value, same runtime pointer).
	if _, ok := val.Type().(*irtypes.StructType); ok {
		var instPtr value.Value = c.block.NewExtractValue(val, 1)
		// B0218: Bitcast typed instance pointers (e.g., promise_Point_i*) to i8*
		// so we can compare against tracked temps (which are always i8*).
		if instPtr.Type() != irtypes.I8Ptr {
			if _, isPtr := instPtr.Type().(*irtypes.PointerType); isPtr {
				instPtr = c.block.NewBitCast(instPtr, irtypes.I8Ptr)
			} else if innerSt, isStruct := instPtr.Type().(*irtypes.StructType); isStruct && len(innerSt.Fields) >= 2 {
				// B0233: Handle optional wrapping: {i1, {vtable, instance}} —
				// field 1 of the optional is a value struct, extract field 1 from it.
				instPtr = c.block.NewExtractValue(instPtr, 1)
				if instPtr.Type() != irtypes.I8Ptr {
					if _, isPtr2 := instPtr.Type().(*irtypes.PointerType); isPtr2 {
						instPtr = c.block.NewBitCast(instPtr, irtypes.I8Ptr)
					} else {
						return
					}
				}
			} else {
				return
			}
		}
		for _, temp := range c.heapTemps {
			if c.lastClaimedDropFunc == nil {
				c.lastClaimedDropFunc = temp.dropFunc // T0127: capture for scope binding
			}
			tracked := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
			isSame := c.block.NewICmp(enum.IPredEQ, instPtr, tracked)
			claimBlk := c.newBlock("heap.claim")
			skipBlk := c.newBlock("heap.claim.skip")
			c.block.NewCondBr(isSame, claimBlk, skipBlk)
			claimBlk.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
			claimBlk.NewBr(skipBlk)
			c.block = skipBlk
		}
	}
}

// clearMatchingStmtTemps clears the caller-side drop flag of whichever statement
// temp (string/vector) in temps matches val at runtime (T1106). Used for go-call
// argument transfer when the heap-string/vector root is a runtime phi over multiple
// owned temps (`if c { s1.clone() } else { s2.clone() }`): the phi value equals
// exactly one arm's temp pointer at runtime, so a runtime pointer comparison clears
// that arm's flag and leaves intermediates (e.g. the trim() result in
// `s.trim().clone()`) with the caller. Non-taken arms' temp allocas hold null and
// never match. Mirrors claimHeapTemp's per-temp comparison loop, but on i8* directly.
func (c *Compiler) clearMatchingStmtTemps(val value.Value, temps []stmtTemp) {
	if val == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if val.Type() != irtypes.I8Ptr {
		return
	}
	for _, temp := range temps {
		tracked := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
		isSame := c.block.NewICmp(enum.IPredEQ, val, tracked)
		claimBlk := c.newBlock("stmt.claim")
		skipBlk := c.newBlock("stmt.claim.skip")
		c.block.NewCondBr(isSame, claimBlk, skipBlk)
		claimBlk.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
		claimBlk.NewBr(skipBlk)
		c.block = skipBlk
	}
}

// cleanupHeapTemps drops all unclaimed heap instance temps at statement end (T0088).
// For each temp: check flag → null-check ptr → call drop(ptr).
func (c *Compiler) cleanupHeapTemps() {
	c.cleanupHeapTempsFrom(0)
}

// cleanupHeapTempsFrom drops all unclaimed heap instance temps at or above index
// `floor`, leaving the prefix [0:floor) intact (T1329 — see cleanupStmtTempsFrom).
func (c *Compiler) cleanupHeapTempsFrom(floor int) {
	if len(c.heapTemps) <= floor {
		return
	}
	if c.block == nil || c.block.Term != nil {
		if floor == 0 {
			c.heapTemps = c.heapTemps[:0]
			c.heapTempMap = make(map[value.Value]int)
		} else {
			c.heapTemps = c.heapTemps[:floor]
			pruneTempMapSuffix(c.heapTempMap, floor)
		}
		return
	}

	for _, temp := range c.heapTemps[floor:] {
		flag := c.block.NewLoad(irtypes.I1, temp.dropFlag)
		dropBlock := c.newBlock("heap.drop")
		skipBlock := c.newBlock("heap.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		ptr := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
		isNull := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("heap.exec")
		doneBlock := c.newBlock("heap.done")
		c.block.NewCondBr(isNull, doneBlock, execBlock)

		c.block = execBlock
		// T0369: For vector heap temps, walk droppable elements (strings, enums
		// with droppable variants, heap user types, droppable tuples) before
		// freeing the buffer via Vector.drop. Mirrors the stmtTemp path's T0356
		// fix. Only set on vector literals via trackVectorHeapTempWithElemType;
		// other heap temps (slice results, ctor allocations) leave elemType nil.
		if temp.elemType != nil {
			c.emitVectorElementDropLoop(ptr, temp.elemType)
		}
		c.block.NewCall(temp.dropFunc, ptr)
		c.block.NewBr(doneBlock)

		c.block = doneBlock
		// B0270/T1280: Reset the drop flag after dropping so a stale temp isn't
		// re-dropped on a later loop iteration or in a sibling match arm. The temp
		// alloca/flag are function-entry allocas that persist across iterations and
		// are only refreshed by the box-creation code in the *taken* branch. If arm A
		// registers+drops a box here (leaving flag=true) and a later iteration takes
		// arm B instead, this same emitted cleanup would re-load arm A's stale (freed)
		// pointer with flag still true → double free. emitHeapTempCleanupForErrorPath
		// already does this; cleanupHeapTemps was missing it.
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	}

	if floor == 0 {
		c.heapTemps = c.heapTemps[:0]
		c.heapTempMap = make(map[value.Value]int)
	} else {
		c.heapTemps = c.heapTemps[:floor] // T1329
		pruneTempMapSuffix(c.heapTempMap, floor)
	}
}

// promoteHeapTempsToScope converts remaining heapTemps into scope bindings (B0222).
// When a generic combinator chain result is stored in a variable, intermediate
// iterators must survive until scope exit — not be freed at statement end. Each
// heapTemp's existing drop flag is reused: the one claimed by the variable already
// has flag=0 (its scope binding won't fire), while unclaimed intermediates have
// flag=1 and will be freed at scope exit via emitFreeCall.
//
// T0369: temp.elemType is intentionally not propagated into the scopeBinding.
// This path only fires for structural-typed variables (e.g., Iterator). Vector
// literals have concrete Vector[T] declared types and never reach this code,
// so element drops via emitVectorElementDropLoop happen exclusively on the
// cleanupHeapTemps statement-end path. If a future callsite registers a vector
// heap temp that survives to a scope binding, emitFreeCall would lose element
// drops — extend scopeBinding/emitFreeCall at that point.
func (c *Compiler) promoteHeapTempsToScope() {
	if len(c.heapTemps) == 0 {
		return
	}
	for _, temp := range c.heapTemps {
		binding := scopeBinding{
			kind:     bindingFree,
			alloca:   temp.alloca,
			dropFlag: temp.dropFlag,
			dropFunc: temp.dropFunc,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
	}
	// Clear heapTemps to prevent cleanupHeapTemps from double-processing.
	c.heapTemps = c.heapTemps[:0]
	c.heapTempMap = make(map[value.Value]int)
}

// promoteHandleTempToScopeBinding promotes a tracked single-owner handle
// stmtTemp (the receiver of a borrowing method such as Mutex.lock()) into a
// scope binding so it outlives the derived guard. A single-owner Mutex *temp*
// receiver (`Mutex[int](7).lock()`, `mk_mtx().lock()`) would otherwise be
// dropped at statement end before the MutexGuard that borrows it, and
// MutexGuard.drop then unlocks/derefs freed Mutex memory → UAF/SEGV (T0655).
// Registering it as a scope binding before the guard's var-decl scope binding
// makes LIFO scope cleanup drop the guard (unlock) before the Mutex (free) —
// exactly mirroring the already-correct bound-receiver path.
//
// Returns false (no-op) when val is not a currently-tracked stmtTemp — e.g. a
// bound-variable receiver (`m := ...; m.lock()`), where mutexRaw is a fresh
// load and never a stmtTempMap key — so the must-stay-correct bound and
// consume-only cases are provably untouched.
func (c *Compiler) promoteHandleTempToScopeBinding(val value.Value, dropFunc *ir.Func, valType types.Type) bool {
	if val == nil || c.block == nil || c.block.Term != nil || c.entryBlock == nil {
		return false
	}
	idx, ok := c.stmtTempMap[val]
	if !ok || idx < 0 { // not a tracked temp → leave bound path untouched
		return false
	}
	// Coroutine-safe entry-block allocas (same primitive as trackTempWithDrop):
	// initialized to null/false in the entry block so a temp created inside a
	// branch has defined values on untaken paths.
	alloca := c.createEntryAlloca(irtypes.I8Ptr)
	dropFlag := c.createEntryAlloca(irtypes.I1)
	c.entryBlock.NewStore(constant.NewNull(irtypes.I8Ptr), alloca)
	c.entryBlock.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)
	c.block.NewStore(val, alloca)
	// T0951: preserve the temp's live per-branch ownership flag instead of
	// hardcoding 1. An ordinary handle temp (`Mutex[int](7).lock()`,
	// `mk_mtx().lock()`) carries flag=1, so this is identical to the prior
	// `store 1` for the T0655 case. But an inline elvis handle result
	// (`(a ?: b).lock()`) carries a per-path flag — owned (1) on the orphaned
	// some-path, borrowed (0) on the none-path where the default keeps its own
	// owner. Hardcoding 1 would force-drop the borrowed none-path default, which
	// its own scope binding also drops → double-free. Loaded before claimStringTemp
	// below clears the source temp's flag.
	curFlag := c.block.NewLoad(irtypes.I1, c.stmtTemps[idx].dropFlag)
	c.block.NewStore(curFlag, dropFlag)
	// bindingDropString: i8* alloca + void(i8*) drop — identical IR shape to the
	// known-good bound-Mutex scope binding (stmt.go ~2097). The Vector
	// static-flag branch in emitStringDropCall is inert for a Mutex valType.
	c.scopeBindings = append(c.scopeBindings, scopeBinding{
		kind:     bindingDropString,
		alloca:   alloca,
		dropFlag: dropFlag,
		dropFunc: dropFunc,
		valType:  valType,
	})
	// Neutralize the stmt-temp (clears its flag + maps it to -1) so it is not
	// also dropped at statement end. Keeps the T0555/T0561 binding-site claim
	// machinery intact.
	c.claimStringTemp(val)
	return true
}

// trackEnvTemp registers a heap-allocated closure env pointer for cleanup at
// statement end (T0100). Called from genLambdaExpr when the lambda has captures.
// If the lambda is later stored in a variable, claimEnvTemp prevents double-free.
func (c *Compiler) trackEnvTemp(envPtr value.Value) {
	if envPtr == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if c.entryBlock == nil || !c.tempTrackingEnabled {
		return
	}
	if envPtr.Type() != irtypes.I8Ptr {
		return
	}
	if _, ok := c.envTempMap[envPtr]; ok {
		return
	}

	alloca := c.createEntryAlloca(irtypes.I8Ptr)
	dropFlag := c.createEntryAlloca(irtypes.I1)
	c.entryBlock.NewStore(constant.NewNull(irtypes.I8Ptr), alloca)
	c.entryBlock.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)

	c.block.NewStore(envPtr, alloca)
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)

	idx := len(c.envTemps)
	c.envTemps = append(c.envTemps, envTemp{alloca: alloca, dropFlag: dropFlag})
	c.envTempMap[envPtr] = idx
}

// claimEnvTemp marks a tracked env temp as consumed (ownership transferred
// to a variable's scope binding via maybeRegisterEnvFree). Accepts either a
// raw i8* env pointer (direct SSA match) or a closure fat pointer {i8*, i8*}
// (extracts field 1 and compares at runtime).
func (c *Compiler) claimEnvTemp(val value.Value) {
	if val == nil || len(c.envTemps) == 0 {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return
	}
	// Try direct SSA match (rare — usually the env ptr is embedded in a fat pointer)
	if idx, ok := c.envTempMap[val]; ok && idx >= 0 {
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.envTemps[idx].dropFlag)
		c.envTempMap[val] = -1
		return
	}
	// For closure fat pointers {i8*, i8*}: extract env (field 1) and compare at runtime
	if st, ok := val.Type().(*irtypes.StructType); ok && len(st.Fields) == 2 {
		envPtr := c.block.NewExtractValue(val, 1)
		// T0814: Optional-wrapped closure {present, {fn,env}} — field 1 is the closure
		// fat pointer, not the bare env i8*. Recurse so the env temp is claimed
		// (otherwise cleanupEnvTemps frees it early → dangling env in the optional).
		if _, isStruct := envPtr.Type().(*irtypes.StructType); isStruct {
			c.claimEnvTemp(envPtr)
			return
		}
		if envPtr.Type() != irtypes.I8Ptr {
			return
		}
		for _, temp := range c.envTemps {
			tracked := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
			isSame := c.block.NewICmp(enum.IPredEQ, envPtr, tracked)
			claimBlk := c.newBlock("env.claim")
			skipBlk := c.newBlock("env.claim.skip")
			c.block.NewCondBr(isSame, claimBlk, skipBlk)
			claimBlk.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
			claimBlk.NewBr(skipBlk)
			c.block = skipBlk
		}
	}
}

// trackClosureOperatorResult registers the heap env of a closure returned by a
// user-defined operator (binary or unary) as a statement-scoped env temp, so a
// discarded operator result (`a + b;`, `-a;`) frees its env instead of leaking
// (T1229). Mirrors the direct-tracking pattern used for getter results (T1240/
// T1253) and error-handler results (T1235) rather than the defunct alias-filter.
//
// Soundness: an operator's `this` and argument are borrowed (sema rejects
// `move` operands outright — there is no call-site move syntax for `a + b`), so
// a returned `() -> …` closure is always a fresh, owned {fn,env} fat pointer.
// A closure aliasing a borrowed operand would require a move-out-of-borrow,
// which the ownership pass rejects, and an operand cannot itself be a moved-in
// closure. The defensive claimEnvTemp (runtime pointer match) is therefore
// unreachable today; it is kept purely to future-proof the sole-owner invariant
// — if operators ever gain move operands, it neutralizes any already-tracked
// aliasing temp before we register the result as sole owner (never a double
// free). The bound path stays single-free because a var-decl RHS already claims
// the value and hands ownership to the local's bindingFreeEnv.
func (c *Compiler) trackClosureOperatorResult(result value.Value) {
	if result == nil || c.block == nil || c.block.Term != nil {
		return
	}
	st, ok := result.Type().(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 {
		return
	}
	c.claimEnvTemp(result) // neutralize an aliasing moved-arg closure temp
	envPtr := c.block.NewExtractValue(result, 1)
	c.trackEnvTemp(envPtr)
}

// claimAllEnvTemps claims all active (unclaimed) env temps. Called when
// maybeTrackIterTemp registers a heap temp — the callee stored our closure env
// in the returned instance (e.g., _FnIter), so its cleanup handles the env.
func (c *Compiler) claimAllEnvTemps() {
	if c.block == nil || c.block.Term != nil {
		return
	}
	for key, idx := range c.envTempMap {
		if idx >= 0 {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.envTemps[idx].dropFlag)
			c.envTempMap[key] = -1
		}
	}
}

// cleanupEnvTemps frees all unclaimed closure env temps at statement end (T0100).
// For each temp: check flag → null-check ptr → call env drop fn or pal_free (B0221).
func (c *Compiler) cleanupEnvTemps() {
	c.cleanupEnvTempsFrom(0)
}

// cleanupEnvTempsFrom frees all unclaimed closure env temps at or above index
// `floor`, leaving the prefix [0:floor) intact (T1329 — see cleanupStmtTempsFrom).
func (c *Compiler) cleanupEnvTempsFrom(floor int) {
	if len(c.envTemps) <= floor {
		return
	}
	if c.block == nil || c.block.Term != nil {
		if floor == 0 {
			c.envTemps = c.envTemps[:0]
			c.envTempMap = make(map[value.Value]int)
		} else {
			c.envTemps = c.envTemps[:floor]
			pruneTempMapSuffix(c.envTempMap, floor)
		}
		return
	}

	for _, temp := range c.envTemps[floor:] {
		flag := c.block.NewLoad(irtypes.I1, temp.dropFlag)
		dropBlock := c.newBlock("env.tmp.drop")
		skipBlock := c.newBlock("env.tmp.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		ptr := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
		isNull := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("env.tmp.exec")
		doneBlock := c.newBlock("env.tmp.done")
		c.block.NewCondBr(isNull, doneBlock, execBlock)

		c.block = execBlock
		// B0221: Use emitEnvDropOrFree to properly drop captured values
		c.emitEnvDropOrFree(ptr)
		c.block.NewBr(doneBlock)

		c.block = doneBlock
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	}

	if floor == 0 {
		c.envTemps = c.envTemps[:0]
		c.envTempMap = make(map[value.Value]int)
	} else {
		c.envTemps = c.envTemps[:floor] // T1329
		pruneTempMapSuffix(c.envTempMap, floor)
	}
}

// maybeTrackIterTemp tracks the instance pointer from a method call result
// when the result type is a structural interface (T0088). At statement end,
// unclaimed temps are cleaned up. Iterator/Stream types use __promise_iter_cleanup
// (handles _FnIter parent chain + closure env). Other structural types use
// __promise_structural_drop (B0270: RTTI-based drop for arbitrary concrete types).
func (c *Compiler) maybeTrackIterTemp(e *ast.CallExpr, result value.Value) {
	if result == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if !c.tempTrackingEnabled {
		return
	}
	// Check if the result type is a structural interface (e.g., Iterator[T])
	resultType := c.resolvedExprType(e)
	resultNamed := extractNamed(resultType)
	if resultNamed == nil || !resultNamed.IsStructural() {
		return
	}
	// The result is a value struct {i8* vtable, i8* instance}. Extract instance ptr.
	if _, ok := result.Type().(*irtypes.StructType); !ok {
		return
	}
	instancePtr := c.block.NewExtractValue(result, 1)
	// Iterator[T] and Stream[T] use iterCleanup (handles _FnIter parent chain).
	// Other structural types use structuralDrop (B0270: RTTI-based, works for any type).
	_, isIter := types.AsIterator(resultType)
	_, isStream := types.AsStream(resultType)
	before := len(c.heapTemps)
	if (isIter || isStream) && c.iterCleanup != nil {
		c.trackHeapTemp(instancePtr, c.iterCleanup)
	} else if c.structuralDrop != nil {
		c.trackHeapTemp(instancePtr, c.structuralDrop)
	}
	// T1310: if the just-tracked temp aliases a live owned structural arg
	// (owned-arg passthrough, `f.make(s);` returning `s` by value), clear the
	// temp's drop flag so the caller's arg stays sole owner and we don't double
	// free the same box at scope exit. Fresh-constructing returns have a distinct
	// instance ptr → no spurious clear.
	if len(c.heapTemps) > before {
		c.maybeClearStructuralTempAliasArg(e, instancePtr, c.heapTemps[len(c.heapTemps)-1].dropFlag)
	}
	// T1311: also clear the result temp when it aliases a FRESH rvalue-temp
	// structural arg (e.g. `f.make(Counter(total:5));`). maybeClearStructuralTempAliasArg
	// only covers named-local (IdentExpr) args; the fresh-temp arg's instance ptr was
	// recorded by emitReturnAliasCheckSubst in pendingStructuralArgAliasPtrs. Clears
	// ONLY the result temp — the arg temp remains the sole owner and frees the box
	// once. Scoped to the discard/inline-in-discard context so the binding path
	// (T1304/T1308) is untouched. Fresh-return-with-arg-present (`return Counter(...)`)
	// is safe: the runtime ptr compare never fires, so both boxes free independently.
	// The pendingStructuralArgAliasCall == e check pins the recorded ptrs to THIS
	// call: a stale value left by an earlier free-function structural discard (which
	// records but never reaches maybeTrackIterTemp, and whose dispatch may not re-enter
	// emitReturnAliasCheckSubst before this call) can never be applied here.
	if c.discardedExpr != nil && len(c.pendingStructuralArgAliasPtrs) > 0 &&
		c.pendingStructuralArgAliasCall == ast.Expr(e) &&
		len(c.heapTemps) > before && instancePtr.Type() == irtypes.I8Ptr &&
		c.block != nil && c.block.Term == nil {
		resultFlag := c.heapTemps[len(c.heapTemps)-1].dropFlag
		for _, argPtr := range c.pendingStructuralArgAliasPtrs {
			same := c.block.NewICmp(enum.IPredEQ, instancePtr, argPtr)
			clearBlk := c.newBlock("struct.tmp.freshalias.clear")
			skipBlk := c.newBlock("struct.tmp.freshalias.skip")
			c.block.NewCondBr(same, clearBlk, skipBlk)
			clearBlk.NewStore(constant.False, resultFlag)
			clearBlk.NewBr(skipBlk)
			c.block = skipBlk
		}
	}
	c.pendingStructuralArgAliasPtrs = nil
	c.pendingStructuralArgAliasCall = nil
}

// isTrackedStringCall returns true if the call expression produces a NEW
// heap-allocated string (T0073, T0099, T0123). Tracks ALL calls returning
// string type. After B0255, string.to_string() allocates via clone(),
// so there are no known borrows left to exclude.
func (c *Compiler) isTrackedStringCall(_ *ast.CallExpr) bool {
	return true
}

// trackHeapUserTypeResult tracks an expression result that is a heap-allocated
// user type (Map, Set, regular user-defined heap types) returned as a value
// struct {i8*, i8*} (T0341, generalized in T0343). Used for direct CallExpr
// results and for ErrorPanicExpr / ErrorPropagateExpr / OptionalUnwrapExpr /
// ErrorHandlerExpr results, which all peel a failable/optional struct down to
// the bare value struct.
//
// Constructor calls already track via genConstructorCallMono (T0135) — skip
// them here to avoid double-tracking. Strings, vectors, structural interfaces,
// value types, copy types, and primitives have their own tracking paths and
// are skipped.
//
// Aliasing is handled via runtime pointer comparison:
//   - claimHeapTemp(result) clears any existing heapTemp whose runtime pointer
//     matches the result (e.g., method on a temp returning `this`).
//   - For method calls, an additional runtime check against the receiver's
//     instance pointer clears the new temp's drop flag if the call result
//     aliases a non-temp receiver (e.g., `c.iter()` where `c` is a local
//     variable whose own scope binding will free the allocation). The receiver
//     is found by peeling unwrap layers via findInnerCallExpr.
//
// trackHeapValueTemp registers a `{vtable, instance}` heap user-type value as an
// owned statement temp, keyed on its instance pointer, returning that pointer and
// the temp's drop flag (both nil if nothing was tracked — value/copy/primitive/
// structural type, container, string, or no drop function). It holds the single
// "is this a droppable heap user type, and if so track it" decision shared by
// trackHeapUserTypeResult (which layers AST-shape skips and receiver-alias clears
// on top) and the optional-handler ident-source phi path (T1085).
func (c *Compiler) trackHeapValueTemp(result value.Value, rt types.Type) (value.Value, value.Value) {
	if result == nil || c.block == nil || c.block.Term != nil || !c.tempTrackingEnabled {
		return nil, nil
	}
	st, ok := result.Type().(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 || st.Fields[0] != irtypes.I8Ptr || st.Fields[1] != irtypes.I8Ptr {
		return nil, nil
	}
	named := extractNamed(rt)
	if named == nil {
		return nil, nil
	}
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return nil, nil
	}
	if isContainerType(rt) || named == types.TypString {
		return nil, nil
	}
	dropFunc := c.resolveDropFuncForTemp(named, rt)
	if dropFunc == nil {
		return nil, nil
	}
	c.claimHeapTemp(result)
	instancePtr := c.block.NewExtractValue(result, 1)
	beforeLen := len(c.heapTemps)
	c.trackHeapTemp(instancePtr, dropFunc)
	if len(c.heapTemps) > beforeLen {
		return instancePtr, c.heapTemps[beforeLen].dropFlag
	}
	return nil, nil
}

func (c *Compiler) trackHeapUserTypeResult(expr ast.Expr, result value.Value) {
	if result == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if !c.tempTrackingEnabled {
		return
	}
	st, ok := result.Type().(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 || st.Fields[0] != irtypes.I8Ptr || st.Fields[1] != irtypes.I8Ptr {
		return
	}
	// B0287 / T0343: For optional unwrap on ident source, the optional's drop
	// binding owns the inner allocation. Tracking would double-free at scope exit.
	// isIdentOptionalUnwrapSource peels ParenExpr so `(o)!` skips like `o!`.
	if opt, isOpt := expr.(*ast.OptionalUnwrapExpr); isOpt {
		if isIdentOptionalUnwrapSource(opt.Expr) {
			return
		}
		// T0775: member source on an owner-with-drop (`owner.field!`) used as a
		// temporary. The extracted inner aliases the owned field; the owner's drop
		// frees it. Skip temp-tracking (mirrors the ident skip) — tracking would
		// double-free at scope exit. EXCLUDE borrowed-`this`: genOptionalForceUnwrap
		// (T0428 Case 3B) makes an INDEPENDENT dup there, which DOES need tracking.
		// T1299: EXCLUDE a structural getter source (`owner.getter!` where the getter
		// returns a non-value structural). The getter body deep-clones the field view
		// on escape, so the unwrapped box is an OWNED clone (not an owner-governed
		// alias); it must reach the structural-owned tracking below, exactly like the
		// index-operator case (`owner[i]!`, whose IndexExpr source already bypasses this
		// member guard). Direct field reads (`owner.field!`) still skip — the escape
		// there is unflagged and stays aliased.
		if c.isOwnerGovernedMemberOptionalUnwrapSource(opt.Expr) &&
			!c.isBorrowedThisMemberSource(opt.Expr) &&
			!c.isStructuralGetterMemberSource(opt.Expr) {
			return
		}
		// T1143: inline `container[k]!` (e.g. `m["k"]!.field`) extracts the
		// element by alias when no dup was made — the binding/return/arg dup
		// paths return early in genOptionalForceUnwrap. genOptionalForceUnwrap
		// sets optionalUnwrapContainerBorrow only on that plain no-dup path. The
		// container's drop frees the slot; registering the aliased inner as an
		// owned temp double-frees at scope exit. Consume the flag here so it can
		// never leak into a later unrelated call (the entry-reset in
		// genOptionalForceUnwrap is the belt-and-suspenders guard).
		if c.optionalUnwrapContainerBorrow {
			c.optionalUnwrapContainerBorrow = false
			return
		}
		// T1215: nested-Optional double-force (`r!!` with `r: T??`). After peeling
		// the force layers the source bottoms out in an owned ident / owner-governed
		// member whose recursive drop frees the innermost heap instance; the
		// extracted inner is an alias, so tracking it as an owned temp double-frees
		// (segfault) at scope exit alongside the owner's nested-optional drop.
		if c.isNestedOwnerGovernedUnwrapSource(opt.Expr) {
			return
		}
	}
	// T0753: Same for the optional-handler unwrap (`o? _ { ... }`) on an ident
	// source. The handler extracts the inner value as an aliasing extractvalue;
	// the source optional's own drop binding governs the inner allocation's
	// lifetime (frees it for an owned optional; deliberately does NOT free it for
	// a borrow-holding optional once isRttiCastBorrow clears o's drop flag).
	// Tracking the extracted heap value as an owned temp double-frees.
	if eh, isEH := expr.(*ast.ErrorHandlerExpr); isEH {
		if isIdentOptionalUnwrapSource(eh.Expr) {
			return
		}
	}
	// Skip constructor calls — those are tracked inside genConstructorCallMono.
	// Only applies when the outermost expression is itself a CallExpr; the
	// unwrap/propagate operators can't legally have a constructor as their
	// inner expression (constructors don't return failable/optional types).
	if call, isCall := expr.(*ast.CallExpr); isCall {
		calleeType := c.info.Types[call.Callee]
		if c.typeSubst != nil && calleeType != nil {
			calleeType = types.Substitute(calleeType, c.typeSubst)
		}
		switch calleeType.(type) {
		case *types.Named, *types.Instance:
			return
		}
	}
	rt := c.resolvedExprType(expr)
	// T1160: a call/getter returning a closure hands back a {i8*, i8*} fat pointer
	// (fn_ptr + heap env). extractNamed(*types.Signature) is nil, so the heap-value
	// path below never registers it — a discarded or inline-consumed result leaked
	// the env. Track the env ptr; consuming sinks claim it via claimEnvTemp,
	// otherwise cleanupEnvTemps frees it through the canonical B0221/T0739 path
	// (load env field-0 drop fn, else pal_free). Skip calls that may hand back a
	// closure they do not own (T1227) — freeing those double-frees the real owner.
	if _, isSig := rt.(*types.Signature); isSig {
		if !c.closureResultMayAliasCallInput(expr) {
			c.trackEnvTemp(c.block.NewExtractValue(result, 1))
		}
		return
	}
	// T1292: An owned non-value structural-interface temporary produced by a
	// container-index force-unwrap or handler — e.g. `m[k]!` when V is a structural
	// interface. `Map.[]` now deep-clones V internally (typeNeedsMatchDup(V) is
	// true), so the unwrapped view owns an independent heap box that must be freed
	// at statement end via RTTI drop dispatch (__promise_structural_drop honors the
	// concrete drop_fn). This is deliberately restricted to unwrap/handler sources:
	// the borrow guards above already returned for aliasing unwrap cases, so only a
	// genuine owned clone reaches here. A plain call returning a structural view
	// (e.g. `c.iter()` returning borrowed `this`) is NOT tracked — dropping a
	// borrowed/non-standard-RTTI view would double-free or crash.
	if rtNamed := extractNamed(rt); rtNamed != nil && rtNamed.IsStructural() && !rtNamed.IsValueType() && c.structuralDrop != nil {
		_, isUnwrap := expr.(*ast.OptionalUnwrapExpr)
		_, isHandler := expr.(*ast.ErrorHandlerExpr)
		// T1299: a getter (or its force-unwrap) returning a non-value structural hands
		// back an OWNED clone — the accessor body deep-cloned the field view on escape.
		// Track it via RTTI drop dispatch so an inline use (`owner.getter.emit(x)`,
		// `owner.getter!.emit(x)`) frees the box exactly once at statement end. A plain
		// call returning a borrowed structural view (e.g. `c.iter()` handing back `this`)
		// is still NOT tracked — it is neither an unwrap/handler nor a getter member.
		// T1321: a bare module getter (`stderr`, `heap_seven`) or qualified
		// module getter (`mod.prop`) of non-value structural type also hands back
		// a fresh, owned heap box — nothing else frees it. Module getters take no
		// receiver and construct fresh values, so tracking the box for statement-
		// end RTTI drop is never an alias/double-free hazard (mirrors the fresh-
		// owned instance-getter branch). A binding that claims this temp clears it
		// via claimHeapTemp — single owner either way.
		if isUnwrap || isHandler || c.isStructuralGetterMemberSource(expr) || c.isModuleGetterExpr(expr) {
			c.trackHeapTemp(c.block.NewExtractValue(result, 1), c.structuralDrop)
			return
		}
		// T1294: a plain free-function call returning a freshly-constructed non-value
		// structural view (`show(1);` discarded, or `show(3).to_string()` inline) owns
		// its heap box — nothing else frees it. isFreshOwnedStructuralCall admits only
		// the provably-fresh, non-aliasing shape (excludes method `this`/iter-adapter
		// returns and owned-arg passthrough), so routing the extracted instance ptr
		// through RTTI drop dispatch frees it exactly once at statement end. A later
		// binding that claims this temp clears it via claimHeapTemp — no double-free.
		if c.isFreshOwnedStructuralCall(expr, rt) {
			c.trackHeapTemp(c.block.NewExtractValue(result, 1), c.structuralDrop)
		}
		return
	}
	instancePtr, dropFlag := c.trackHeapValueTemp(result, rt)
	if instancePtr != nil {
		// T1029: when a heap-user-type value produced anywhere inside the discarded
		// statement (the outer result OR an inner sub-call result) aliases an
		// owned-local arg whose source-clear was suppressed, clear this temp so the
		// source local stays sole owner (freed once at scope exit). Using
		// discardedExpr != nil (not expr == discardedExpr) covers sibling sub-calls
		// whose result is not propagated to the discarded result — e.g.
		// `combine(a, ident(b))` returning `a`: the `ident(b)` temp aliases `b` but
		// is dropped by `combine`, so it must be neutralized here, not left armed.
		// emitDiscardAliasClears only clears on a runtime pointer match, so genuinely
		// fresh temps stay armed and are freed at statement end.
		if c.discardedExpr != nil && len(c.discardAliasArgPtrs) > 0 {
			c.emitDiscardAliasClears(instancePtr, dropFlag)
		}
		if innerCall := findInnerCallExpr(expr); innerCall != nil {
			c.emitReceiverAliasCheck(innerCall, instancePtr, dropFlag)
		} else if origin := operatorReceiverOrigin(expr); origin != nil {
			// T0958: operator dispatch (BinaryExpr/UnaryExpr) has no inner
			// CallExpr, so findInnerCallExpr returns nil. An operator body of
			// `return this` yields a result aliasing the left/unary operand;
			// without the alias-clear the temp and the operand's scope binding
			// both free the same instance (double-free). Mirrors the bound case
			// (operatorReceiverOrigin → pendingThisAliasClear /
			// maybeClearReceiverDropFlag, T0882).
			c.emitReceiverAliasCheckForTarget(origin, instancePtr, dropFlag)
		}
	}
}
