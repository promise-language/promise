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

// generatorValueType returns the struct type used for non-failable generator values: {i8*, i8*}.
// Field 0 is the coroutine handle, field 1 is the yield slot pointer.
func generatorValueType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
}

// failableGeneratorValueType returns the struct type used for failable generator values: {i8*, i8*, i8*}.
// Field 0 is the coroutine handle, field 1 is the yield slot pointer, field 2 is the error slot pointer.
func failableGeneratorValueType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I8Ptr)
}

// defineGeneratorFunc compiles a top-level generator function.
func (c *Compiler) defineGeneratorFunc(fd *ast.FuncDecl, fn *ir.Func, elemType types.Type) {
	obj := c.lookupFunc(fd.Name)
	if obj == nil {
		return
	}
	sig, ok := obj.Type().(*types.Signature)
	if !ok {
		return
	}
	c.currentOpValueParams = nil // T0897: free functions are never operators
	c.buildGeneratorCoroutine(sig, fn, fd.Body, elemType, nil)
}

// defineGeneratorMethod compiles a generator method on a type.
func (c *Compiler) defineGeneratorMethod(md *ast.MethodDecl, m *types.Method, fn *ir.Func, elemType types.Type, ownerNamed *types.Named) {
	c.setOperatorValueParams(md.Name, m.Sig()) // T0897
	c.buildGeneratorCoroutine(m.Sig(), fn, md.Body, elemType, ownerNamed)
}

// buildGeneratorCoroutine is the shared core that compiles a generator into:
//  1. A coroutine `.generator.N(params..., i8* yield_slot) → i8*` with a standard
//     initial suspend. The ramp returns the coroutine handle immediately; the body
//     runs on first resume.
//  2. A factory body in `fn` that allocates the yield slot, calls the ramp, and
//     returns {handle, slot}.
//
// If sig.Recv() is non-nil, the coroutine gets a `this` (i8*) first param.
func (c *Compiler) buildGeneratorCoroutine(sig *types.Signature, fn *ir.Func, body *ast.Block, elemType types.Type, ownerNamed *types.Named) {
	elemLLVM := c.resolveType(elemType)
	isFailable := sig.CanError()

	// 1. Create coroutine function: [this +] params + i8* yield_slot [+ i8* error_slot] → i8*
	coroName := coroRampName("generator", c.coroEnclosingQualifier(fn), c.generatorCounter) // T1222: qualify by enclosing to keep symbol unique across split units
	c.generatorCounter++

	var coroParams []*ir.Param
	if sig.Recv() != nil {
		coroParams = append(coroParams, ir.NewParam("this", irtypes.I8Ptr))
	}
	for _, p := range sig.Params() {
		coroParams = append(coroParams, ir.NewParam(p.Name(), c.resolveType(p.Type())))
	}
	coroParams = append(coroParams, ir.NewParam("yield_slot", irtypes.I8Ptr))
	if isFailable {
		coroParams = append(coroParams, ir.NewParam("error_slot", irtypes.I8Ptr))
	}

	coroFn := c.module.NewFunc(coroName, irtypes.I8Ptr, coroParams...)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("noinline"))
	c.attributeCoroToEnclosing(coroName, fn) // T1222: same split unit as spawner

	// 2. Save compiler state
	savedFn := c.fn
	savedBlock := c.block
	savedEntryBlock := c.entryBlock
	savedLocals := c.locals
	savedCanError := c.canError
	savedRetType := c.currentRetType
	savedBlockCounter := c.blockCounter
	savedScopeBindings := c.scopeBindings
	savedDropFlags := c.dropFlags
	savedCastSubjectMatch := c.castSubjectMatch // T0849: function-scoped, like dropFlags
	savedDropBindings := c.dropBindings
	savedLoopScopeDepth := c.loopScopeDepth
	savedInCoroutine := c.inCoroutine
	savedCoroCleanup := c.coroCleanupBlk
	savedCoroSuspend := c.coroSuspendBlk
	savedInGenerator := c.inGenerator
	savedGenCanError := c.generatorCanError
	savedYieldSlot := c.generatorYieldSlot
	savedGenErrorSlot := c.generatorErrorSlot
	savedGenCoroId := c.generatorCoroId
	savedGenCleanup := c.generatorCleanup
	savedGenSuspend := c.generatorSuspend
	savedGenFinalSuspend := c.generatorFinalSuspend
	savedNamed := c.currentNamed
	savedLocalNameCount := c.localNameCount // T0261
	savedPanicExitBlock := c.panicExitBlock
	savedCoroutineReturnBlock := c.coroutineReturnBlock
	savedThisRecvIsOwned := c.thisRecvIsOwned    // T0436
	savedOpValueParams := c.currentOpValueParams // T0897: preserve caller-set value
	savedBorrowedParams := c.borrowedValueParams // T0945: preserve caller-set value

	c.fn = coroFn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false // keep false — error path handled via generatorCanError
	c.currentRetType = nil
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil // T0849: fresh per generated function body
	c.dropBindings = make(map[string]scopeBinding)
	c.loopScopeDepth = 0
	c.inCoroutine = false
	c.inGenerator = true
	c.generatorCanError = isFailable
	c.generatorErrorSlot = nil
	c.panicExitBlock = nil
	c.coroutineReturnBlock = nil
	if ownerNamed != nil {
		c.currentNamed = ownerNamed
	}
	// T0436: track whether the generator method's receiver is owned (~this).
	// Generator funcs (no receiver) get false. Without this, the flag leaks from
	// the previous defineMethodFunc call into the coroutine body.
	c.thisRecvIsOwned = sig.Recv() != nil && sig.Recv().Ref() == types.RefMut
	c.setBorrowedValueParams(sig) // T0945: generator body sees its own params

	// Yield slot is the second-to-last (or last for non-failable) parameter
	yieldSlotParam := coroFn.Params[len(coroFn.Params)-1]
	if isFailable {
		yieldSlotParam = coroFn.Params[len(coroFn.Params)-2]
	}

	// 3. Build coroutine preamble with initial suspend
	entry := coroFn.NewBlock(".entry")
	c.block = entry

	coroId := entry.NewCall(c.coroId,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr))
	c.generatorCoroId = coroId

	need := entry.NewCall(c.coroAlloc, coroId)
	allocBlk := coroFn.NewBlock("coro.alloc")
	startBlk := coroFn.NewBlock("coro.start")
	entry.NewCondBr(need, allocBlk, startBlk)

	coroSizeVal := allocBlk.NewCall(c.coroSize)
	var coroSizeArg value.Value = coroSizeVal
	if c.isWasm {
		coroSizeArg = allocBlk.NewZExt(coroSizeVal, irtypes.I64)
	}
	mem := allocBlk.NewCall(c.palAlloc, coroSizeArg)
	allocBlk.NewBr(startBlk)

	phiMem := startBlk.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entry),
		ir.NewIncoming(mem, allocBlk))
	hdl := startBlk.NewCall(c.coroBegin, coroId, phiMem)

	// Store yield_slot into alloca (part of coroutine frame, survives across suspends)
	yieldSlotAlloca := startBlk.NewAlloca(irtypes.I8Ptr)
	yieldSlotAlloca.SetName(c.uniqueLocalName("yield_slot.addr"))
	startBlk.NewStore(yieldSlotParam, yieldSlotAlloca)
	c.generatorYieldSlot = yieldSlotAlloca

	// B0023: Store error_slot into alloca for failable generators
	if isFailable {
		errorSlotParam := coroFn.Params[len(coroFn.Params)-1]
		errorSlotAlloca := startBlk.NewAlloca(irtypes.I8Ptr)
		errorSlotAlloca.SetName(c.uniqueLocalName("error_slot.addr"))
		startBlk.NewStore(errorSlotParam, errorSlotAlloca)
		c.generatorErrorSlot = errorSlotAlloca
	}

	// Bind params into allocas (after coro.begin → part of frame)
	paramIdx := 0
	if sig.Recv() != nil {
		alloca := startBlk.NewAlloca(irtypes.I8Ptr)
		alloca.SetName(c.uniqueLocalName("this.addr"))
		startBlk.NewStore(coroFn.Params[paramIdx], alloca)
		c.locals["this"] = alloca
		paramIdx++
	}
	for _, p := range sig.Params() {
		if p.Name() == "" || p.Name() == "_" {
			paramIdx++
			continue
		}
		alloca := startBlk.NewAlloca(c.resolveType(p.Type()))
		alloca.SetName(c.uniqueLocalName(p.Name() + ".addr"))
		startBlk.NewStore(coroFn.Params[paramIdx], alloca)
		c.locals[p.Name()] = alloca
		paramIdx++
	}

	// T0479: Register drop bindings for params that own heap data so they are
	// freed at coroutine end. Mirrors T0087/B0191/T0406 from defineFunc/defineMethodFunc.
	// We harvest the new bindings out of c.scopeBindings into a separate paramDrops
	// slice and emit them at cleanupBlk start (the universal destroy sink). Keeping
	// them out of c.scopeBindings prevents `return` mid-body from double-dropping
	// via emitScopeCleanup(0), since the existing emit*DropCall helpers don't clear
	// the drop flag after firing. The c.dropBindings map stays populated so
	// clearDropFlag still works for moves inside the body.
	prevBlock := c.block
	prevEntry := c.entryBlock
	c.block = startBlk
	c.entryBlock = startBlk

	var paramDrops []scopeBinding
	for _, p := range sig.Params() {
		if p.Name() == "" || p.Name() == "_" {
			continue
		}
		alloca := c.locals[p.Name()]
		if alloca == nil {
			continue
		}
		paramType := p.Type()
		if c.typeSubst != nil {
			paramType = types.Substitute(paramType, c.typeSubst)
		}
		before := len(c.scopeBindings)
		switch {
		case p.Ref() == types.RefMut:
			// T0087-equivalent: ~ ownership transfer.
			c.maybeRegisterDrop(p.Name(), alloca, paramType)
		case p.IsVariadic():
			// B0191-equivalent: variadic vector storage.
			c.maybeRegisterDrop(p.Name(), alloca, paramType)
		default:
			// T1233: plain tuple-by-value params borrow — the caller owns and
			// drops the tuple (see defineFunc). Supersedes T0406's callee-drop.
		}
		// T1194: borrow-by-default heap param reassigned to a fresh owned value
		// inside the generator body (no-op unless reassigned). Harvested into
		// paramDrops alongside the ~/variadic/tuple bindings below.
		c.maybeRegisterBorrowParamReassignDrop(p.Name(), alloca, paramType, p.Ref(), body)
		if len(c.scopeBindings) > before {
			paramDrops = append(paramDrops, c.scopeBindings[before:]...)
			c.scopeBindings = c.scopeBindings[:before]
		}
	}

	c.block = prevBlock
	c.entryBlock = prevEntry

	cleanupBlk := coroFn.NewBlock("cleanup")
	doneBlk := coroFn.NewBlock("coro.done")
	finalSuspBlk := coroFn.NewBlock("final.suspend")
	bodyBlk := coroFn.NewBlock("body")

	// doneBlk serves as both the suspend path (ramp return) and the done path.
	// coro.end is required for coro-split to generate proper ret void in the
	// resume function's default (suspend) switch case.
	c.generatorCleanup = cleanupBlk
	c.generatorSuspend = doneBlk
	c.generatorFinalSuspend = finalSuspBlk
	c.coroCleanupBlk = cleanupBlk
	c.coroSuspendBlk = doneBlk

	// Initial suspend — in a separate block so that createEntryAlloca can
	// append allocas to startBlk BEFORE the suspend point. coro-split needs
	// allocas to precede coro.suspend to properly spill them to the frame.
	initSuspBlk := coroFn.NewBlock("coro.init.suspend")
	startBlk.NewBr(initSuspBlk)

	initSusp := initSuspBlk.NewCall(c.coroSuspend, constant.None, constant.False)
	initSuspBlk.NewSwitch(initSusp, doneBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), bodyBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// 4. Compile user body — runs on first resume
	c.block = bodyBlk
	c.entryBlock = startBlk
	c.genBlock(body)

	// If body falls through (no more yields), branch to final suspend
	if c.block != nil && c.block.Term == nil {
		c.block.NewBr(finalSuspBlk)
	}

	// 5. Cleanup: drop owned params, then free coroutine memory (destroy path).
	// T0479: cleanupBlk is the universal destruction sink — reached on natural
	// completion (body fall-through → finalSuspBlk → consumer destroys → tag=1),
	// `return` mid-body (body locals dropped via emitScopeCleanup, branches to
	// finalSuspBlk → destroy → tag=1), and mid-flight destroy (yield's
	// coro.suspend tag=1 → cleanupBlk directly). Emitting param drops here fires
	// them exactly once per coroutine instance.
	c.block = cleanupBlk
	if len(paramDrops) > 0 {
		savedScope := c.scopeBindings
		c.scopeBindings = paramDrops
		c.emitScopeCleanup(0, false)
		c.scopeBindings = savedScope
	}
	coroMem := c.block.NewCall(c.coroFree, coroId, hdl)
	needFree := c.block.NewICmp(enum.IPredNE, coroMem, constant.NewNull(irtypes.I8Ptr))
	freeBlk := coroFn.NewBlock("coro.free")
	c.block.NewCondBr(needFree, freeBlk, doneBlk)

	freeBlk.NewCall(c.palFree, coroMem)
	freeBlk.NewBr(doneBlk)

	// Done: coro.end + ret
	doneBlk.NewCall(c.coroEnd, hdl, constant.False, constant.None)
	doneBlk.NewRet(hdl)

	// Final suspend: generator body finished, suspend so consumer sees coro.done()=true
	finalResult := finalSuspBlk.NewCall(c.coroSuspend, constant.None, constant.True)
	finalSuspBlk.NewSwitch(finalResult, doneBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), doneBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// 6. Restore compiler state
	c.fn = savedFn
	c.block = savedBlock
	c.entryBlock = savedEntryBlock
	c.locals = savedLocals
	c.canError = savedCanError
	c.currentRetType = savedRetType
	c.blockCounter = savedBlockCounter
	c.scopeBindings = savedScopeBindings
	c.dropFlags = savedDropFlags
	c.castSubjectMatch = savedCastSubjectMatch // T0849
	c.dropBindings = savedDropBindings
	c.loopScopeDepth = savedLoopScopeDepth
	c.inCoroutine = savedInCoroutine
	c.coroCleanupBlk = savedCoroCleanup
	c.coroSuspendBlk = savedCoroSuspend
	c.inGenerator = savedInGenerator
	c.generatorCanError = savedGenCanError
	c.generatorYieldSlot = savedYieldSlot
	c.generatorErrorSlot = savedGenErrorSlot
	c.generatorCoroId = savedGenCoroId
	c.generatorCleanup = savedGenCleanup
	c.generatorSuspend = savedGenSuspend
	c.generatorFinalSuspend = savedGenFinalSuspend
	c.currentNamed = savedNamed
	c.localNameCount = savedLocalNameCount // T0261
	c.panicExitBlock = savedPanicExitBlock
	c.coroutineReturnBlock = savedCoroutineReturnBlock
	c.thisRecvIsOwned = savedThisRecvIsOwned    // T0436
	c.currentOpValueParams = savedOpValueParams // T0897
	c.borrowedValueParams = savedBorrowedParams // T0945

	// 7. Build factory body for original function
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil // T0849: fresh per generated function body
	c.dropBindings = make(map[string]scopeBinding)
	c.blockCounter = 0

	factoryEntry := fn.NewBlock(".entry")
	c.block = factoryEntry
	c.entryBlock = factoryEntry

	// Allocate yield slot: pal_alloc(sizeof(elemType))
	slotSize := constant.NewInt(irtypes.I64, int64(c.typeSize(elemLLVM)))
	slot := c.block.NewCall(c.palAlloc, slotSize)

	if isFailable {
		// B0023: Failable generator factory with eager start
		// Allocate error slot: sizeof(i8*)
		ptrSize := int64(8)
		if c.isWasm {
			ptrSize = 4
		}
		errSlot := c.block.NewCall(c.palAlloc, constant.NewInt(irtypes.I64, ptrSize))
		// Initialize error_slot to null (no error)
		errSlotTyped := c.block.NewBitCast(errSlot, irtypes.NewPointer(irtypes.I8Ptr))
		c.block.NewStore(constant.NewNull(irtypes.I8Ptr), errSlotTyped)

		// Call coroutine ramp with original params + yield_slot + error_slot
		var rampArgs []value.Value
		for _, p := range fn.Params {
			rampArgs = append(rampArgs, p)
		}
		rampArgs = append(rampArgs, slot)
		rampArgs = append(rampArgs, errSlot)
		handle := c.block.NewCall(coroFn, rampArgs...)

		// Eager start: resume coroutine to run body to first yield or error
		c.block.NewCall(c.genResume, handle)

		// Check error_slot — non-null means error before first yield
		errPtr := c.block.NewLoad(irtypes.I8Ptr, errSlotTyped)
		isErr := c.block.NewICmp(enum.IPredNE, errPtr, constant.NewNull(irtypes.I8Ptr))

		errBlk := c.newBlock("gen.factory.error")
		okBlk := c.newBlock("gen.factory.ok")
		c.block.NewCondBr(isErr, errBlk, okBlk)

		// Error path: destroy coroutine, free slots, return error
		c.block = errBlk
		c.block.NewCall(c.genDestroy, handle)
		c.block.NewCall(c.palFree, slot)
		c.block.NewCall(c.palFree, errSlot)
		factoryResultType := fn.Sig.RetType.(*irtypes.StructType)
		c.block.NewRet(c.wrapError(errPtr, factoryResultType))

		// OK path: return wrapOk({handle, slot, error_slot})
		c.block = okBlk
		genVal := c.block.NewInsertValue(constant.NewUndef(failableGeneratorValueType()), handle, 0)
		genVal2 := c.block.NewInsertValue(genVal, slot, 1)
		genVal3 := c.block.NewInsertValue(genVal2, errSlot, 2)
		c.block.NewRet(c.wrapOk(genVal3, fn.Sig.RetType.(*irtypes.StructType)))
	} else {
		// Non-failable generator factory (existing behavior)
		var rampArgs []value.Value
		for _, p := range fn.Params {
			rampArgs = append(rampArgs, p)
		}
		rampArgs = append(rampArgs, slot)
		handle := c.block.NewCall(coroFn, rampArgs...)

		// Build return value: {handle, slot}
		genVal := c.block.NewInsertValue(constant.NewUndef(generatorValueType()), handle, 0)
		genVal2 := c.block.NewInsertValue(genVal, slot, 1)
		c.block.NewRet(genVal2)
	}
}

// genYieldStmt generates code for a yield statement inside a generator coroutine.
// Stores the yielded value into the yield slot, then suspends.
func (c *Compiler) genYieldStmt(s *ast.YieldStmt) {
	val := c.genExpr(s.Value)
	c.emitYieldValue(val)
}

// genForInGenerator generates a for-in loop over a generator value {handle, slot}
// or failable generator value {handle, slot, error_slot}.
//
// Non-failable protocol (with initial suspend):
//
//	factory() → ramp returns handle immediately (body not started)
//	resume(handle) → body runs to first yield (stores value) or final suspend
//	loop: if done → exit; load value; body; resume; goto loop
//
// Failable protocol (eager start — factory already resumed):
//
//	factory!() → ramp + resume; error_slot checked; returns {handle, slot, error_slot}
//	loop: if done → exit; load value; body; resume; if error → propagate; goto loop
//	exit: check error_slot → propagate or normal cleanup
func (c *Compiler) genForInGenerator(s *ast.ForInStmt, genVal value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)

	// Detect failable generator by struct field count (3 = failable, 2 = non-failable)
	genStruct := genVal.Type().(*irtypes.StructType)
	isFailable := len(genStruct.Fields) == 3

	// Extract handle and yield_slot from generator value struct
	handle := c.block.NewExtractValue(genVal, 0)
	yieldSlot := c.block.NewExtractValue(genVal, 1)

	// Store into allocas for cleanup (break/return can destroy)
	handleAlloca := c.createEntryAlloca(irtypes.I8Ptr)
	handleAlloca.SetName(c.uniqueLocalName("gen.handle"))
	c.block.NewStore(handle, handleAlloca)

	slotAlloca := c.createEntryAlloca(irtypes.I8Ptr)
	slotAlloca.SetName(c.uniqueLocalName("gen.slot"))
	c.block.NewStore(yieldSlot, slotAlloca)

	// B0023: For failable generators, also store error_slot
	var errSlotAlloca *ir.InstAlloca
	if isFailable {
		errSlot := c.block.NewExtractValue(genVal, 2)
		errSlotAlloca = c.createEntryAlloca(irtypes.I8Ptr)
		errSlotAlloca.SetName(c.uniqueLocalName("gen.errslot"))
		c.block.NewStore(errSlot, errSlotAlloca)
	}

	// Register generator scope binding for cleanup on break/return
	binding := scopeBinding{
		kind:            bindingGenerator,
		generatorHandle: handleAlloca,
		generatorSlot:   slotAlloca,
	}
	if isFailable {
		binding.generatorErrorSlot = errSlotAlloca
	}
	c.scopeBindings = append(c.scopeBindings, binding)

	// Bind loop variable
	elemAlloca := c.createEntryAlloca(elemLLVM)
	elemAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.locals[s.Binding] = elemAlloca

	// Optional index variable
	if s.Index != "" {
		indexAlloca := c.createEntryAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	checkBlk := c.newBlock("gen.check")
	bodyBlk := c.newBlock("gen.body")
	resumeBlk := c.newBlock("gen.resume")
	exitBlk := c.newBlock("gen.exit")

	if isFailable {
		// B0023: Failable generator — factory already did the initial resume (eager start).
		// Go directly to the done check.
		c.block.NewBr(checkBlk)
	} else {
		// Non-failable: initial resume starts the generator body
		initHandle := c.block.NewLoad(irtypes.I8Ptr, handleAlloca)
		c.block.NewCall(c.genResume, initHandle)
		c.block.NewBr(checkBlk)
	}

	// Check: is generator done? (use noinline wrapper to prevent coro-elide)
	c.block = checkBlk
	curHandle := c.block.NewLoad(irtypes.I8Ptr, handleAlloca)
	done := c.block.NewCall(c.genDone, curHandle)
	c.block.NewCondBr(done, exitBlk, bodyBlk)

	// Body: load yielded value from slot, bind to loop var, execute body
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopScopeDepth := c.loopScopeDepth
	c.breakTarget = exitBlk
	c.continueTarget = resumeBlk
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlk
	curSlot := c.block.NewLoad(irtypes.I8Ptr, slotAlloca)
	typedSlot := c.block.NewBitCast(curSlot, irtypes.NewPointer(elemLLVM))
	elemVal := c.block.NewLoad(elemLLVM, typedSlot)
	c.block.NewStore(elemVal, elemAlloca)

	c.genBlock(s.Body)

	// After body: branch to resume
	if c.block.Term == nil {
		c.block.NewBr(resumeBlk)
	}

	// Resume: update index, resume coroutine, then check for error (failable) or done
	c.block = resumeBlk
	if s.Index != "" {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}
	rHandle := c.block.NewLoad(irtypes.I8Ptr, handleAlloca)
	c.block.NewCall(c.genResume, rHandle)
	c.block.NewBr(checkBlk)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopScopeDepth

	// Exit: destroy coroutine + free slots
	c.block = exitBlk

	// Remove the generator scope binding (compile-time only — must happen
	// exactly once, before any condBr that splits into error/clean paths).
	c.scopeBindings = c.scopeBindings[:len(c.scopeBindings)-1]

	if isFailable {
		// B0023: Check error_slot — non-null means iteration error.
		// Error causes done=true via final.suspend, so we reach exit naturally.
		errSlotPtr := c.block.NewLoad(irtypes.I8Ptr, errSlotAlloca)
		errSlotTyped := c.block.NewBitCast(errSlotPtr, irtypes.NewPointer(irtypes.I8Ptr))
		errPtr := c.block.NewLoad(irtypes.I8Ptr, errSlotTyped)
		isErr := c.block.NewICmp(enum.IPredNE, errPtr, constant.NewNull(irtypes.I8Ptr))

		errBlk := c.newBlock("gen.forin.error")
		cleanBlk := c.newBlock("gen.forin.clean")
		c.block.NewCondBr(isErr, errBlk, cleanBlk)

		// Error path: destroy coro, free slots, propagate error to caller
		c.block = errBlk
		errHandle := c.block.NewLoad(irtypes.I8Ptr, handleAlloca)
		c.block.NewCall(c.genDestroy, errHandle)
		errYieldSlot := c.block.NewLoad(irtypes.I8Ptr, slotAlloca)
		c.block.NewCall(c.palFree, errYieldSlot)
		c.block.NewCall(c.palFree, errSlotPtr)
		c.block.NewStore(constant.NewNull(irtypes.I8Ptr), handleAlloca)
		// Propagate error to enclosing failable function/generator
		if c.inGenerator && c.generatorCanError {
			if len(c.scopeBindings) > 0 {
				c.emitScopeCleanup(0, true)
			}
			c.emitGeneratorError(errPtr)
		} else if c.canError {
			if len(c.scopeBindings) > 0 {
				c.emitScopeCleanup(0, true)
			}
			callerResultType := c.currentResultType()
			c.block.NewRet(c.wrapError(errPtr, callerResultType))
		} else {
			// Non-failable consumer — panic with the error
			c.emitErrorPanic(errPtr, s.Pos().File, s.Pos().Line)
		}

		// Normal cleanup path (no error)
		c.block = cleanBlk
		exitHandle := c.block.NewLoad(irtypes.I8Ptr, handleAlloca)
		c.block.NewCall(c.genDestroy, exitHandle)
		exitSlot := c.block.NewLoad(irtypes.I8Ptr, slotAlloca)
		c.block.NewCall(c.palFree, exitSlot)
		exitErrSlot := c.block.NewLoad(irtypes.I8Ptr, errSlotAlloca)
		c.block.NewCall(c.palFree, exitErrSlot)
		c.block.NewStore(constant.NewNull(irtypes.I8Ptr), handleAlloca)
	} else {
		exitHandle := c.block.NewLoad(irtypes.I8Ptr, handleAlloca)
		c.block.NewCall(c.genDestroy, exitHandle)
		exitSlot := c.block.NewLoad(irtypes.I8Ptr, slotAlloca)
		c.block.NewCall(c.palFree, exitSlot)
		c.block.NewStore(constant.NewNull(irtypes.I8Ptr), handleAlloca)
	}
}

// emitGeneratorCleanup emits cleanup for a generator scope binding.
// Checks if handle is non-null; if so, destroys coroutine and frees yield slot
// (and error slot for failable generators).
func (c *Compiler) emitGeneratorCleanup(b scopeBinding) {
	handle := c.block.NewLoad(irtypes.I8Ptr, b.generatorHandle)
	isNull := c.block.NewICmp(enum.IPredEQ, handle, constant.NewNull(irtypes.I8Ptr))

	cleanBlk := c.newBlock("gen.cleanup")
	skipBlk := c.newBlock("gen.cleanup.skip")
	c.block.NewCondBr(isNull, skipBlk, cleanBlk)

	c.block = cleanBlk
	c.block.NewCall(c.genDestroy, handle)
	slot := c.block.NewLoad(irtypes.I8Ptr, b.generatorSlot)
	c.block.NewCall(c.palFree, slot)
	// B0023: Free error_slot for failable generators
	if b.generatorErrorSlot != nil {
		errSlot := c.block.NewLoad(irtypes.I8Ptr, b.generatorErrorSlot)
		c.block.NewCall(c.palFree, errSlot)
	}
	c.block.NewBr(skipBlk)

	c.block = skipBlk
}

// emitGeneratorError stores an error into the generator's error slot and branches
// to the final suspend point. Used by ?^, raise, and auto-propagation inside
// failable generator bodies. The consumer (factory or for-in) detects the error
// by reading the error_slot after resume.
func (c *Compiler) emitGeneratorError(errVal value.Value) {
	errSlotAddr := c.block.NewLoad(irtypes.I8Ptr, c.generatorErrorSlot.(*ir.InstAlloca))
	errSlotTyped := c.block.NewBitCast(errSlotAddr, irtypes.NewPointer(irtypes.I8Ptr))
	c.block.NewStore(errVal, errSlotTyped)
	c.block.NewBr(c.generatorFinalSuspend)
}

// unwrapFailableGeneratorResult unwraps a failable result struct from a
// generator factory call. On error: panics (non-failable context), propagates
// (failable function), or stores to generator error slot (failable generator).
// On success: returns the inner generator value struct.
func (c *Compiler) unwrapFailableGeneratorResult(result value.Value, pos ast.Pos) value.Value {
	resultType := result.Type().(*irtypes.StructType)
	tag := c.block.NewExtractValue(result, 0)

	errBlk := c.newBlock("gen.factory.err")
	okBlk := c.newBlock("gen.factory.ok")
	c.block.NewCondBr(tag, errBlk, okBlk)

	c.block = errBlk
	errPtr := c.block.NewExtractValue(result, resultErrIdx(resultType))
	c.emitStmtTempCleanupForErrorPath()
	c.emitHeapTempCleanupForErrorPath()
	if c.inGenerator && c.generatorCanError {
		if len(c.scopeBindings) > 0 {
			c.emitScopeCleanup(0, true)
		}
		c.emitGeneratorError(errPtr)
	} else if c.canError {
		if len(c.scopeBindings) > 0 {
			c.emitScopeCleanup(0, true)
		}
		callerResultType := c.currentResultType()
		c.block.NewRet(c.wrapError(errPtr, callerResultType))
	} else {
		c.emitErrorPanic(errPtr, pos.File, pos.Line)
	}

	c.block = okBlk
	return c.block.NewExtractValue(result, 1)
}

// emitYieldValue stores a value into the generator's yield slot and suspends.
// On resume, execution continues in a new block (set as c.block).
//
// T0504: Mid-flight destroy (consumer breaks before the generator completes)
// resumes at coro.suspend with tag=1, which would jump straight to
// c.generatorCleanup — bypassing body scope cleanup. To prevent leaks of body
// locals alive at this suspend point, snapshot c.scopeBindings and emit a
// per-yield cleanup block that drops them before chaining to c.generatorCleanup
// (which still handles param drops via T0479's paramDrops slice).
func (c *Compiler) emitYieldValue(val value.Value) {
	slotPtr := c.block.NewLoad(irtypes.I8Ptr, c.generatorYieldSlot.(*ir.InstAlloca))
	typedSlot := c.block.NewBitCast(slotPtr, irtypes.NewPointer(val.Type()))
	c.block.NewStore(val, typedSlot)

	suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
	resumeBlk := c.newBlock("yield.resume")

	cleanupTarget := c.generatorCleanup
	if len(c.scopeBindings) > 0 {
		snapshot := make([]scopeBinding, len(c.scopeBindings))
		copy(snapshot, c.scopeBindings)

		yieldCleanupBlk := c.newBlock("yield.cleanup")
		savedBlock := c.block
		c.block = yieldCleanupBlk

		savedScope := c.scopeBindings
		c.scopeBindings = snapshot
		c.emitScopeCleanup(0, false)
		c.scopeBindings = savedScope

		c.block.NewBr(c.generatorCleanup)
		c.block = savedBlock
		cleanupTarget = yieldCleanupBlk
	}

	c.block.NewSwitch(suspResult, c.generatorSuspend,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupTarget))
	c.block = resumeBlk
}

// genYieldDelegateStmt generates code for a yield* statement inside a generator.
// Iterates over the delegate expression and yields each element to the consumer.
func (c *Compiler) genYieldDelegateStmt(s *ast.YieldDelegateStmt) {
	iterableType := c.info.Types[s.Value]
	if c.typeSubst != nil {
		iterableType = types.Substitute(iterableType, c.typeSubst)
	}

	if elem, ok := types.AsStream(iterableType); ok {
		genVal := c.genExpr(s.Value)
		// T0284: Failable sub-generator factory called without explicit error handling.
		if c.info.FailableExprs[s.Value] {
			genVal = c.unwrapFailableGeneratorResult(genVal, s.Pos())
		}
		c.genYieldDelegateGenerator(genVal, elem, s.Pos())
	} else if arr, ok := iterableType.(*types.Array); ok {
		c.genYieldDelegateArray(s.Value, arr)
	} else if elem, ok := types.AsVector(iterableType); ok {
		vecPtr := c.genExpr(s.Value)
		c.genYieldDelegateVector(vecPtr, elem)
	} else if elem, ok := types.AsRange(iterableType); ok {
		c.genYieldDelegateRange(s.Value, elem, iterableType)
	} else if types.Identical(extractNamed(iterableType), types.TypString) {
		strPtr := c.genExpr(s.Value)
		c.genYieldDelegateString(strPtr)
	} else if elem, ok := types.AsIterator(iterableType); ok {
		iterVal := c.genExpr(s.Value)
		c.genYieldDelegateIterator(iterVal, elem, iterableType)
	} else {
		panic(fmt.Sprintf("codegen: unsupported yield* iterable type %s", iterableType))
	}
}

// genYieldDelegateGenerator yields all values from a sub-generator (stream[T]).
// For failable sub-generators (3-field struct), errors are propagated to the
// outer generator's error_slot.
func (c *Compiler) genYieldDelegateGenerator(genVal value.Value, elemType types.Type, pos ast.Pos) {
	elemLLVM := c.resolveType(elemType)

	// Detect failable sub-generator by struct field count
	genStruct := genVal.Type().(*irtypes.StructType)
	isFailable := len(genStruct.Fields) == 3

	handle := c.block.NewExtractValue(genVal, 0)
	yieldSlot := c.block.NewExtractValue(genVal, 1)

	handleAlloca := c.createEntryAlloca(irtypes.I8Ptr)
	handleAlloca.SetName(c.uniqueLocalName("yieldstar.handle"))
	c.block.NewStore(handle, handleAlloca)

	slotAlloca := c.createEntryAlloca(irtypes.I8Ptr)
	slotAlloca.SetName(c.uniqueLocalName("yieldstar.slot"))
	c.block.NewStore(yieldSlot, slotAlloca)

	// B0023: For failable sub-generators, store error_slot
	var errSlotAlloca *ir.InstAlloca
	if isFailable {
		errSlot := c.block.NewExtractValue(genVal, 2)
		errSlotAlloca = c.createEntryAlloca(irtypes.I8Ptr)
		errSlotAlloca.SetName(c.uniqueLocalName("yieldstar.errslot"))
		c.block.NewStore(errSlot, errSlotAlloca)
	}

	// Register sub-generator for cleanup if our generator is destroyed mid-yield*
	binding := scopeBinding{
		kind:            bindingGenerator,
		generatorHandle: handleAlloca,
		generatorSlot:   slotAlloca,
	}
	if isFailable {
		binding.generatorErrorSlot = errSlotAlloca
	}
	c.scopeBindings = append(c.scopeBindings, binding)

	if isFailable {
		// B0023: Failable sub-generator — factory already did the initial resume
		// (eager start). Go directly to done check.
	} else {
		// Initial resume: start the sub-generator body
		c.block.NewCall(c.genResume, handle)
	}

	checkBlk := c.newBlock("yieldstar.check")
	yieldBlk := c.newBlock("yieldstar.yield")
	exitBlk := c.newBlock("yieldstar.exit")

	c.block.NewBr(checkBlk)

	// Check: is sub-generator done?
	c.block = checkBlk
	curHandle := c.block.NewLoad(irtypes.I8Ptr, handleAlloca)
	done := c.block.NewCall(c.genDone, curHandle)
	c.block.NewCondBr(done, exitBlk, yieldBlk)

	// Yield: load from sub-generator slot, yield to our consumer
	c.block = yieldBlk
	curSlot := c.block.NewLoad(irtypes.I8Ptr, slotAlloca)
	typedSlot := c.block.NewBitCast(curSlot, irtypes.NewPointer(elemLLVM))
	elem := c.block.NewLoad(elemLLVM, typedSlot)
	c.emitYieldValue(elem)

	// After resume: resume sub-generator for next value
	rHandle := c.block.NewLoad(irtypes.I8Ptr, handleAlloca)
	c.block.NewCall(c.genResume, rHandle)
	c.block.NewBr(checkBlk)

	// Exit: destroy sub-generator + free slots
	c.block = exitBlk

	// Remove scope binding (compile-time only — must happen exactly once,
	// before any condBr that splits into error/clean paths).
	c.scopeBindings = c.scopeBindings[:len(c.scopeBindings)-1]

	if isFailable {
		// B0023: Check sub-generator error_slot — propagate to outer generator
		subErrSlotPtr := c.block.NewLoad(irtypes.I8Ptr, errSlotAlloca)
		subErrSlotTyped := c.block.NewBitCast(subErrSlotPtr, irtypes.NewPointer(irtypes.I8Ptr))
		subErrPtr := c.block.NewLoad(irtypes.I8Ptr, subErrSlotTyped)
		isErr := c.block.NewICmp(enum.IPredNE, subErrPtr, constant.NewNull(irtypes.I8Ptr))

		errBlk := c.newBlock("yieldstar.error")
		cleanBlk := c.newBlock("yieldstar.clean")
		c.block.NewCondBr(isErr, errBlk, cleanBlk)

		// Error: destroy sub-generator, free slots, propagate to outer
		c.block = errBlk
		errH := c.block.NewLoad(irtypes.I8Ptr, handleAlloca)
		c.block.NewCall(c.genDestroy, errH)
		errS := c.block.NewLoad(irtypes.I8Ptr, slotAlloca)
		c.block.NewCall(c.palFree, errS)
		c.block.NewCall(c.palFree, subErrSlotPtr)
		c.block.NewStore(constant.NewNull(irtypes.I8Ptr), handleAlloca)
		// Propagate to outer generator or failable function
		if c.inGenerator && c.generatorCanError {
			if len(c.scopeBindings) > 0 {
				c.emitScopeCleanup(0, true)
			}
			c.emitGeneratorError(subErrPtr)
		} else if c.canError {
			if len(c.scopeBindings) > 0 {
				c.emitScopeCleanup(0, true)
			}
			callerResultType := c.currentResultType()
			c.block.NewRet(c.wrapError(subErrPtr, callerResultType))
		} else {
			c.emitErrorPanic(subErrPtr, pos.File, pos.Line)
		}

		// Normal cleanup
		c.block = cleanBlk
		cleanH := c.block.NewLoad(irtypes.I8Ptr, handleAlloca)
		c.block.NewCall(c.genDestroy, cleanH)
		cleanS := c.block.NewLoad(irtypes.I8Ptr, slotAlloca)
		c.block.NewCall(c.palFree, cleanS)
		cleanE := c.block.NewLoad(irtypes.I8Ptr, errSlotAlloca)
		c.block.NewCall(c.palFree, cleanE)
		c.block.NewStore(constant.NewNull(irtypes.I8Ptr), handleAlloca)
	} else {
		exitHandle := c.block.NewLoad(irtypes.I8Ptr, handleAlloca)
		c.block.NewCall(c.genDestroy, exitHandle)
		exitSlot := c.block.NewLoad(irtypes.I8Ptr, slotAlloca)
		c.block.NewCall(c.palFree, exitSlot)
		c.block.NewStore(constant.NewNull(irtypes.I8Ptr), handleAlloca)
	}
}

// genYieldDelegateRange yields all values from a Range.
func (c *Compiler) genYieldDelegateRange(expr ast.Expr, elemType types.Type, iterableType types.Type) {
	rangeVal := c.genExpr(expr)

	layout := c.lookupTypeLayout(iterableType)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for range type %s", iterableType))
	}

	start := c.block.NewExtractValue(rangeVal, uint64(layout.ValueFieldIndex["start"]))
	end := c.block.NewExtractValue(rangeVal, uint64(layout.ValueFieldIndex["end"]))
	inclusive := c.block.NewExtractValue(rangeVal, uint64(layout.ValueFieldIndex["inclusive"]))

	elemLLVM := c.resolveType(elemType)
	ltPred := enum.IPredSLT
	named := extractNamed(elemType)
	if named != nil && classify(named) == CatUnsignedInt {
		ltPred = enum.IPredULT
	}

	counterAlloca := c.createEntryAlloca(elemLLVM)
	c.block.NewStore(start, counterAlloca)

	headerBlk := c.newBlock("yieldstar.range.header")
	yieldBlk := c.newBlock("yieldstar.range.yield")
	updateBlk := c.newBlock("yieldstar.range.update")
	exitBlk := c.newBlock("yieldstar.range.exit")

	c.block.NewBr(headerBlk)

	// Header: counter < end || (counter == end && inclusive)
	c.block = headerBlk
	counter := c.block.NewLoad(elemLLVM, counterAlloca)
	ltCond := c.block.NewICmp(ltPred, counter, end)
	eqCond := c.block.NewICmp(enum.IPredEQ, counter, end)
	inclAndEq := c.block.NewAnd(inclusive, eqCond)
	cond := c.block.NewOr(ltCond, inclAndEq)
	c.block.NewCondBr(cond, yieldBlk, exitBlk)

	// Yield each element
	c.block = yieldBlk
	c.emitYieldValue(counter)
	c.block.NewBr(updateBlk)

	// Update: increment counter
	c.block = updateBlk
	cur := c.block.NewLoad(elemLLVM, counterAlloca)
	one := constant.NewInt(elemLLVM.(*irtypes.IntType), 1)
	next := c.block.NewAdd(cur, one)
	c.block.NewStore(next, counterAlloca)
	c.block.NewBr(headerBlk)

	c.block = exitBlk
}

// genYieldDelegateArray yields all elements from a fixed-size array.
func (c *Compiler) genYieldDelegateArray(expr ast.Expr, arr *types.Array) {
	basePtr := c.genArrayBasePtr(expr, arr)
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)
	length := constant.NewInt(irtypes.I64, arr.Size())

	counterAlloca := c.createEntryAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), counterAlloca)

	headerBlk := c.newBlock("yieldstar.arr.header")
	yieldBlk := c.newBlock("yieldstar.arr.yield")
	updateBlk := c.newBlock("yieldstar.arr.update")
	exitBlk := c.newBlock("yieldstar.arr.exit")

	c.block.NewBr(headerBlk)

	c.block = headerBlk
	counter := c.block.NewLoad(irtypes.I64, counterAlloca)
	cond := c.block.NewICmp(enum.IPredULT, counter, length)
	c.block.NewCondBr(cond, yieldBlk, exitBlk)

	c.block = yieldBlk
	curCounter := c.block.NewLoad(irtypes.I64, counterAlloca)
	elemPtr := c.block.NewGetElementPtr(arrType, basePtr,
		constant.NewInt(irtypes.I32, 0), curCounter)
	elem := c.block.NewLoad(elemLLVM, elemPtr)
	c.emitYieldValue(elem)
	c.block.NewBr(updateBlk)

	c.block = updateBlk
	cur := c.block.NewLoad(irtypes.I64, counterAlloca)
	next := c.block.NewAdd(cur, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(next, counterAlloca)
	c.block.NewBr(headerBlk)

	c.block = exitBlk
}

// genYieldDelegateVector yields all elements from a Vector.
func (c *Compiler) genYieldDelegateVector(vecPtr value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)

	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(vecPtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	counterAlloca := c.createEntryAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), counterAlloca)

	headerBlk := c.newBlock("yieldstar.vec.header")
	yieldBlk := c.newBlock("yieldstar.vec.yield")
	updateBlk := c.newBlock("yieldstar.vec.update")
	exitBlk := c.newBlock("yieldstar.vec.exit")

	c.block.NewBr(headerBlk)

	c.block = headerBlk
	counter := c.block.NewLoad(irtypes.I64, counterAlloca)
	cond := c.block.NewICmp(enum.IPredULT, counter, length)
	c.block.NewCondBr(cond, yieldBlk, exitBlk)

	c.block = yieldBlk
	curCounter := c.block.NewLoad(irtypes.I64, counterAlloca)
	dataBase := c.block.NewGetElementPtr(irtypes.I8, vecPtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, curCounter)
	elem := c.block.NewLoad(elemLLVM, elemPtr)
	c.emitYieldValue(elem)
	c.block.NewBr(updateBlk)

	c.block = updateBlk
	cur := c.block.NewLoad(irtypes.I64, counterAlloca)
	next := c.block.NewAdd(cur, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(next, counterAlloca)
	c.block.NewBr(headerBlk)

	c.block = exitBlk
}

// genYieldDelegateString yields all chars from a string.
func (c *Compiler) genYieldDelegateString(strPtr value.Value) {
	posAlloca := c.createEntryAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), posAlloca)

	headerBlk := c.newBlock("yieldstar.str.header")
	yieldBlk := c.newBlock("yieldstar.str.yield")
	exitBlk := c.newBlock("yieldstar.str.exit")

	c.block.NewBr(headerBlk)

	c.block = headerBlk
	cp := c.block.NewCall(c.funcs["promise_string_next_char"], strPtr, posAlloca)
	done := c.block.NewICmp(enum.IPredEQ, cp, constant.NewInt(irtypes.I32, -1))
	c.block.NewCondBr(done, exitBlk, yieldBlk)

	c.block = yieldBlk
	c.emitYieldValue(cp)
	c.block.NewBr(headerBlk)

	c.block = exitBlk
}

// genYieldDelegateIterator yields all values from an Iterator[T] (structural interface with next() T?).
func (c *Compiler) genYieldDelegateIterator(iterVal value.Value, _ types.Type, iterType types.Type) {
	named := extractNamed(iterType)
	if named == nil {
		panic(fmt.Sprintf("codegen: genYieldDelegateIterator on non-named type %s", iterType))
	}
	nextMethod := named.LookupMethod("next")
	if nextMethod == nil {
		panic(fmt.Sprintf("codegen: type %s has no next() method", named))
	}

	// Resolve optional return type
	retType := nextMethod.Sig().Result()
	if inst, ok := iterType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Named); ok && len(origin.TypeParams()) > 0 {
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			retType = types.Substitute(retType, subst)
		}
	}
	if c.typeSubst != nil {
		retType = types.Substitute(retType, c.typeSubst)
	}
	optLLVM := c.resolveType(retType)

	// Store iterator in alloca for repeated next() calls
	iterAlloca := c.createEntryAlloca(iterVal.Type())
	iterAlloca.SetName(c.uniqueLocalName("yieldstar.iter"))
	c.block.NewStore(iterVal, iterAlloca)

	headerBlk := c.newBlock("yieldstar.iter.header")
	yieldBlk := c.newBlock("yieldstar.iter.yield")
	exitBlk := c.newBlock("yieldstar.iter.exit")

	c.block.NewBr(headerBlk)

	// Header: call next(), check optional tag
	c.block = headerBlk
	curIter := c.block.NewLoad(iterVal.Type(), iterAlloca)
	nextResult := c.emitIterNext(curIter, iterType, named, nextMethod, optLLVM)
	tag := c.block.NewExtractValue(nextResult, 0)
	isNone := c.block.NewICmp(enum.IPredEQ, tag, constant.NewInt(irtypes.I1, 0))
	c.block.NewCondBr(isNone, exitBlk, yieldBlk)

	// Yield: extract value from optional, yield it
	c.block = yieldBlk
	val := c.block.NewExtractValue(nextResult, 1)
	c.emitYieldValue(val)
	c.block.NewBr(headerBlk)

	c.block = exitBlk
}
