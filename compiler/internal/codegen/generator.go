package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// generatorValueType returns the struct type used for all generator values: {i8*, i8*}.
// Field 0 is the coroutine handle, field 1 is the yield slot pointer.
func generatorValueType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
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
	c.buildGeneratorCoroutine(sig, fn, fd.Body, elemType, nil)
}

// defineGeneratorMethod compiles a generator method on a type.
func (c *Compiler) defineGeneratorMethod(md *ast.MethodDecl, m *types.Method, fn *ir.Func, elemType types.Type, ownerNamed *types.Named) {
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

	// 1. Create coroutine function: [this +] params + i8* yield_slot → i8*
	coroName := fmt.Sprintf(".generator.%d", c.generatorCounter)
	c.generatorCounter++

	var coroParams []*ir.Param
	if sig.Recv() != nil {
		coroParams = append(coroParams, ir.NewParam("this", irtypes.I8Ptr))
	}
	for _, p := range sig.Params() {
		coroParams = append(coroParams, ir.NewParam(p.Name(), c.resolveType(p.Type())))
	}
	coroParams = append(coroParams, ir.NewParam("yield_slot", irtypes.I8Ptr))

	coroFn := c.module.NewFunc(coroName, irtypes.I8Ptr, coroParams...)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("noinline"))

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
	savedDropBindings := c.dropBindings
	savedLoopScopeDepth := c.loopScopeDepth
	savedInCoroutine := c.inCoroutine
	savedCoroCleanup := c.coroCleanupBlk
	savedCoroSuspend := c.coroSuspendBlk
	savedInGenerator := c.inGenerator
	savedYieldSlot := c.generatorYieldSlot
	savedGenCoroId := c.generatorCoroId
	savedGenCleanup := c.generatorCleanup
	savedGenSuspend := c.generatorSuspend
	savedGenFinalSuspend := c.generatorFinalSuspend
	savedNamed := c.currentNamed

	c.fn = coroFn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = nil
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.dropBindings = make(map[string]scopeBinding)
	c.loopScopeDepth = 0
	c.inCoroutine = false
	c.inGenerator = true
	if ownerNamed != nil {
		c.currentNamed = ownerNamed
	}

	// Yield slot is the last parameter
	yieldSlotParam := coroFn.Params[len(coroFn.Params)-1]

	// 3. Build coroutine preamble with initial suspend
	entry := coroFn.NewBlock("entry")
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

	// Initial suspend: ramp returns handle immediately, body runs on first resume
	initSusp := startBlk.NewCall(c.coroSuspend, constant.None, constant.False)
	startBlk.NewSwitch(initSusp, doneBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), bodyBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// 4. Compile user body — runs on first resume
	c.block = bodyBlk
	c.entryBlock = bodyBlk
	c.genBlock(body)

	// If body falls through (no more yields), branch to final suspend
	if c.block != nil && c.block.Term == nil {
		c.block.NewBr(finalSuspBlk)
	}

	// 5. Cleanup: free coroutine memory (destroy path)
	coroMem := cleanupBlk.NewCall(c.coroFree, coroId, hdl)
	needFree := cleanupBlk.NewICmp(enum.IPredNE, coroMem, constant.NewNull(irtypes.I8Ptr))
	freeBlk := coroFn.NewBlock("coro.free")
	cleanupBlk.NewCondBr(needFree, freeBlk, doneBlk)

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
	c.dropBindings = savedDropBindings
	c.loopScopeDepth = savedLoopScopeDepth
	c.inCoroutine = savedInCoroutine
	c.coroCleanupBlk = savedCoroCleanup
	c.coroSuspendBlk = savedCoroSuspend
	c.inGenerator = savedInGenerator
	c.generatorYieldSlot = savedYieldSlot
	c.generatorCoroId = savedGenCoroId
	c.generatorCleanup = savedGenCleanup
	c.generatorSuspend = savedGenSuspend
	c.generatorFinalSuspend = savedGenFinalSuspend
	c.currentNamed = savedNamed

	// 7. Build factory body for original function:
	//    allocate yield slot, call coroutine ramp, return {handle, slot}
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.dropBindings = make(map[string]scopeBinding)
	c.blockCounter = 0

	factoryEntry := fn.NewBlock("entry")
	c.block = factoryEntry
	c.entryBlock = factoryEntry

	// Allocate yield slot: pal_alloc(sizeof(elemType))
	slotSize := constant.NewInt(irtypes.I64, int64(c.typeSize(elemLLVM)))
	slot := c.block.NewCall(c.palAlloc, slotSize)

	// Call coroutine ramp with original params + yield_slot
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

// genYieldStmt generates code for a yield statement inside a generator coroutine.
// Stores the yielded value into the yield slot, then suspends.
func (c *Compiler) genYieldStmt(s *ast.YieldStmt) {
	val := c.genExpr(s.Value)
	elemType := val.Type()

	// Load yield slot from alloca (preserved across suspends)
	slotPtr := c.block.NewLoad(irtypes.I8Ptr, c.generatorYieldSlot.(*ir.InstAlloca))

	// Store value to yield slot: bitcast i8* → T*, then store
	typedSlot := c.block.NewBitCast(slotPtr, irtypes.NewPointer(elemType))
	c.block.NewStore(val, typedSlot)

	// Suspend: coro.suspend(none, false) → switch(0=resume, 1=cleanup)
	suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
	resumeBlk := c.newBlock("yield.resume")
	c.block.NewSwitch(suspResult, c.generatorSuspend,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), c.generatorCleanup))

	c.block = resumeBlk
}

// genForInGenerator generates a for-in loop over a generator value {handle, slot}.
//
// Protocol (with initial suspend):
//
//	factory() → ramp returns handle immediately (body not started)
//	resume(handle) → body runs to first yield (stores value) or final suspend
//	loop: if done → exit; load value; body; resume; goto loop
func (c *Compiler) genForInGenerator(s *ast.ForInStmt, genVal value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)

	// Extract handle and yield_slot from generator value struct
	handle := c.block.NewExtractValue(genVal, 0)
	yieldSlot := c.block.NewExtractValue(genVal, 1)

	// Store into allocas for cleanup (break/return can destroy)
	handleAlloca := c.block.NewAlloca(irtypes.I8Ptr)
	handleAlloca.SetName(c.uniqueLocalName("gen.handle"))
	c.block.NewStore(handle, handleAlloca)

	slotAlloca := c.block.NewAlloca(irtypes.I8Ptr)
	slotAlloca.SetName(c.uniqueLocalName("gen.slot"))
	c.block.NewStore(yieldSlot, slotAlloca)

	// Register generator scope binding for cleanup on break/return
	c.scopeBindings = append(c.scopeBindings, scopeBinding{
		kind:            bindingGenerator,
		generatorHandle: handleAlloca,
		generatorSlot:   slotAlloca,
	})

	// Bind loop variable
	elemAlloca := c.block.NewAlloca(elemLLVM)
	elemAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.locals[s.Binding] = elemAlloca

	// Optional index variable
	if s.Index != "" {
		indexAlloca := c.block.NewAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	checkBlk := c.newBlock("gen.check")
	bodyBlk := c.newBlock("gen.body")
	resumeBlk := c.newBlock("gen.resume")
	exitBlk := c.newBlock("gen.exit")

	// Initial resume: start the generator body (runs to first yield or final suspend)
	initHandle := c.block.NewLoad(irtypes.I8Ptr, handleAlloca)
	c.block.NewCall(c.genResume, initHandle)
	c.block.NewBr(checkBlk)

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

	// Resume: update index, resume coroutine, then go back to check
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

	// Exit: destroy coroutine + free yield slot (use noinline wrapper)
	c.block = exitBlk
	exitHandle := c.block.NewLoad(irtypes.I8Ptr, handleAlloca)
	c.block.NewCall(c.genDestroy, exitHandle)
	exitSlot := c.block.NewLoad(irtypes.I8Ptr, slotAlloca)
	c.block.NewCall(c.palFree, exitSlot)

	// Null out handle so scope binding cleanup is a no-op
	c.block.NewStore(constant.NewNull(irtypes.I8Ptr), handleAlloca)

	// Remove the generator scope binding (it's been cleaned up)
	c.scopeBindings = c.scopeBindings[:len(c.scopeBindings)-1]
}

// emitGeneratorCleanup emits cleanup for a generator scope binding.
// Checks if handle is non-null; if so, destroys coroutine and frees yield slot.
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
	c.block.NewBr(skipBlk)

	c.block = skipBlk
}
