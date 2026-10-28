package codegen

import (
	"sort"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// Coroutine variants of functions that can block (promise-language/promise#68).
//
// On WASM a blocking operation — a channel send/receive, `select`, `<-task`, a
// contended Mutex lock, an un-awaited Task join — parks the goroutine only when
// it is emitted inside a coroutine body (c.inCoroutine). Everywhere else it is a
// nested scheduler loop on the goroutine's own stack (emitWasmCoopWaitPump and
// its siblings): a goroutine that reaches one through a call cannot suspend, so
// on wasm32-web the reactor can never return to the host, and two such waits
// stacked on one stack falsely report a deadlock.
//
// The fix is the "manual CPS transform" docs/runtime-architecture.md names for
// WASM's stackful goroutines: on wasm32-web every function that can block is
// compiled a second time as a coroutine (`<name>$coro`), and a coroutine calls
// it by resuming it and suspending itself until it completes (emitDirectCall),
// so a park at any depth of a statically dispatched call chain suspends the
// whole goroutine. The plain function is unchanged and still serves
// non-coroutine callers.
//
// Which functions get a variant is read off codegen's own output rather than
// re-derived from the AST (that would need a second copy of call resolution):
// compile() runs an analysis pass, and suspendingFuncs takes every function that
// steps the scheduler from inside itself plus everything that reaches one
// through direct calls. The final pass then emits the variants and the awaits.

// coroVariantName is the symbol of plain's coroutine variant. A Promise
// identifier cannot contain `$`, and no other symbol ends in `$coro`, so it
// cannot collide.
func coroVariantName(plain string) string {
	return plain + "$coro"
}

// isWasmWebTriple reports whether target is the wasm32-web reactor target.
func isWasmWebTriple(target string) bool {
	return strings.Contains(target, "wasm") && strings.Contains(target, "web")
}

// suspendingFuncs returns the names of the variant-capable functions in m that
// can block: those that contain a direct call to coopStep (every WASM nested wait
// steps the scheduler) or a direct call to such a function, transitively.
// Blocking is propagated through every function, capable or not — a capable
// function that calls a non-capable blocking one (the synthesized Task[T].drop
// join) blocks too, and its variant parks where the plain body would have spun.
func suspendingFuncs(m *ir.Module, coopStep *ir.Func, capable map[string]bool) map[string]bool {
	if coopStep == nil {
		return nil
	}
	callers := make(map[*ir.Func][]*ir.Func)
	blocking := make(map[*ir.Func]bool)
	var work []*ir.Func
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			for _, inst := range b.Insts {
				call, ok := inst.(*ir.InstCall)
				if !ok {
					continue
				}
				callee := directCallee(call.Callee)
				if callee == nil {
					continue
				}
				if callee == coopStep {
					if !blocking[f] {
						blocking[f] = true
						work = append(work, f)
					}
					continue
				}
				callers[callee] = append(callers[callee], f)
			}
		}
	}
	for len(work) > 0 {
		f := work[len(work)-1]
		work = work[:len(work)-1]
		for _, caller := range callers[f] {
			if !blocking[caller] {
				blocking[caller] = true
				work = append(work, caller)
			}
		}
	}
	var out map[string]bool
	for f := range blocking {
		if !capable[f.Name()] {
			continue
		}
		if out == nil {
			out = make(map[string]bool)
		}
		out[f.Name()] = true
	}
	return out
}

// InstanceCoroVariants returns, for each mono instance whose split unit defines
// or calls a coroutine variant, the sorted names of those variants. They are
// part of the instance's object cache key (compileAndLinkSeparate): which
// variants exist depends on the bodies of the functions the instance reaches,
// outside its declaration, so an object cached before one of them started or
// stopped blocking defines or calls the wrong ones.
func (r *CompileResult) InstanceCoroVariants() map[string][]string {
	c := r.compiler
	isVariant := make(map[*ir.Func]bool, len(c.coroVariantFns))
	for _, v := range c.coroVariantFns {
		isVariant[v] = true
	}
	sets := make(map[string]map[string]bool)
	add := func(inst, name string) {
		if sets[inst] == nil {
			sets[inst] = make(map[string]bool)
		}
		sets[inst][name] = true
	}
	for _, fn := range c.module.Funcs {
		inst, owned := c.instanceOwnedFuncs[fn.Name()]
		if !owned || len(fn.Blocks) == 0 {
			continue
		}
		if isVariant[fn] {
			add(inst, fn.Name())
		}
		for _, b := range fn.Blocks {
			for _, in := range b.Insts {
				call, ok := in.(*ir.InstCall)
				if !ok {
					continue
				}
				if callee := directCallee(call.Callee); callee != nil && isVariant[callee] {
					add(inst, callee.Name())
				}
			}
		}
	}
	out := make(map[string][]string, len(sets))
	for inst, set := range sets {
		names := make([]string, 0, len(set))
		for name := range set {
			names = append(names, name)
		}
		sort.Strings(names)
		out[inst] = names
	}
	return out
}

// directCallee returns the function a call names directly, looking through a
// constant bitcast of it; nil for an indirect call.
func directCallee(callee value.Value) *ir.Func {
	switch v := callee.(type) {
	case *ir.Func:
		return v
	case *constant.ExprBitCast:
		if f, ok := v.From.(*ir.Func); ok {
			return f
		}
	}
	return nil
}

// coroVariantFor returns plain's coroutine variant, declaring it on first use.
// The variant takes plain's parameters followed by a pointer to the result slot
// the caller allocates (absent for a void function) and returns its handle.
func (c *Compiler) coroVariantFor(plain *ir.Func) *ir.Func {
	if v, ok := c.coroVariantFns[plain.Name()]; ok {
		return v
	}
	params := make([]*ir.Param, 0, len(plain.Params)+1)
	for _, p := range plain.Params {
		params = append(params, ir.NewParam(p.Name(), p.Typ))
	}
	if _, isVoid := plain.Sig.RetType.(*irtypes.VoidType); !isVoid {
		params = append(params, ir.NewParam("result.slot", irtypes.NewPointer(plain.Sig.RetType)))
	}
	v := c.module.NewFunc(coroVariantName(plain.Name()), irtypes.I8Ptr, params...)
	v.Linkage = plain.Linkage
	c.coroVariantFns[plain.Name()] = v
	return v
}

// coroVariantToDefine is called by each source-body emitter after it has
// emitted plain's body. It records plain as variant-capable — the analysis pass
// intersects the blocking functions with this set — and returns the variant the
// emitter must now define, or nil: outside the final wasm32-web pass, for a
// function that cannot block, and for one whose variant already has a body.
func (c *Compiler) coroVariantToDefine(plain *ir.Func) *ir.Func {
	c.coroVariantCapable[plain.Name()] = true
	if !c.coroVariants[plain.Name()] {
		return nil
	}
	v := c.coroVariantFor(plain)
	if len(v.Blocks) > 0 {
		return nil
	}
	return v
}

// coroVariantBody carries a variant body between beginSourceBody,
// enterCoroVariantBody and endCoroVariantBody.
type coroVariantBody struct {
	plain, variant *ir.Func
	coroID, hdl    value.Value

	body, finalSuspend, cleanup, done, panicExit *ir.Block

	// The enclosing body's coroutine and result-sink state, overridden for the
	// variant body by enterCoroVariantBody and restored by endCoroVariantBody.
	canError                    bool
	inCoroutine                 bool
	coroCleanupBlk              *ir.Block
	coroSuspendBlk              *ir.Block
	panicExitBlock              *ir.Block
	coroutineReturnBlock        *ir.Block
	inFailableGoBlock           bool
	failableGoBlockAggType      *irtypes.StructType
	failableGoBlockFinalSuspend *ir.Block
	goBlockValueResultLLVM      irtypes.Type
	coroResultSlot              value.Value
}

// beginSourceBody opens the body a source-body emitter is about to generate:
// plain's own entry block when variant is nil, otherwise variant's ramp
// preamble. Either way c.fn, c.block and c.entryBlock are left on the block the
// emitter binds parameters in, and c.fn.Params are the parameters to bind (a
// variant's leading parameters mirror plain's). Returns nil for a plain body.
func (c *Compiler) beginSourceBody(plain, variant *ir.Func) *coroVariantBody {
	if variant == nil {
		c.fn = plain
		c.block = plain.NewBlock(".entry")
		c.entryBlock = c.block
		return nil
	}

	variant.FuncAttrs = append(variant.FuncAttrs, rawFuncAttr("presplitcoroutine"))
	// T1222: the variant's body lives in the same split unit as plain's, so a
	// cached module/instance object carries both. Attributed here, not where the
	// variant is first declared: an await can declare it before plain's instance
	// ownership is recorded (defineMonoMethods records it at definition).
	c.attributeCoroToEnclosing(variant.Name(), plain)
	c.fn = variant
	vb := &coroVariantBody{plain: plain, variant: variant}

	// The genGoBlock preamble without its initial suspend: the caller runs the
	// ramp, which executes the body until it first parks (or completes).
	entry := variant.NewBlock(".entry")
	vb.coroID = entry.NewCall(c.coroId,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr))
	need := entry.NewCall(c.coroAlloc, vb.coroID)
	allocBlk := variant.NewBlock("coro.alloc")
	start := variant.NewBlock("coro.start")
	entry.NewCondBr(need, allocBlk, start)

	coroSize := allocBlk.NewCall(c.coroSize)
	var coroSizeArg value.Value = coroSize
	if c.isWasm {
		coroSizeArg = allocBlk.NewZExt(coroSize, irtypes.I64)
	}
	mem := allocBlk.NewCall(c.palAlloc, coroSizeArg)
	allocBlk.NewBr(start)

	phiMem := start.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entry),
		ir.NewIncoming(mem, allocBlk))
	vb.hdl = start.NewCall(c.coroBegin, vb.coroID, phiMem)

	vb.body = variant.NewBlock(coroBodyLabel)
	vb.cleanup = variant.NewBlock(coroCleanupLabel)
	vb.done = variant.NewBlock("coro.done")
	vb.finalSuspend = variant.NewBlock("final.suspend")
	vb.panicExit = variant.NewBlock("variant.panic_exit")

	// Parameters are bound in the start block (after coro.begin, so they are
	// part of the frame), and createEntryAlloca keeps appending allocas there.
	c.block = start
	c.entryBlock = start
	return vb
}

// enterCoroVariantBody switches a variant body from parameter binding to the
// source body. Called right before the emitter generates the body, once it has
// set c.canError from the signature. The body runs as a coroutine whose result
// goes through the `go! {}` result sink into the caller's slot: `return`, a
// failable function's errors and a panic exit all store there and branch to the
// final suspend instead of emitting the `ret` the coroutine ramp cannot have.
// No-op for a plain body.
func (c *Compiler) enterCoroVariantBody(vb *coroVariantBody) {
	if vb == nil {
		return
	}
	vb.canError = c.canError
	vb.inCoroutine = c.inCoroutine
	vb.coroCleanupBlk = c.coroCleanupBlk
	vb.coroSuspendBlk = c.coroSuspendBlk
	vb.panicExitBlock = c.panicExitBlock
	vb.coroutineReturnBlock = c.coroutineReturnBlock
	vb.inFailableGoBlock = c.inFailableGoBlock
	vb.failableGoBlockAggType = c.failableGoBlockAggType
	vb.failableGoBlockFinalSuspend = c.failableGoBlockFinalSuspend
	vb.goBlockValueResultLLVM = c.goBlockValueResultLLVM
	vb.coroResultSlot = c.coroResultSlot

	c.block.NewBr(vb.body)
	c.block = vb.body

	failable := c.canError
	c.canError = false
	c.inCoroutine = true
	c.coroCleanupBlk = vb.cleanup
	c.coroSuspendBlk = vb.done
	c.panicExitBlock = vb.panicExit
	c.coroutineReturnBlock = vb.finalSuspend
	c.inFailableGoBlock = false
	c.failableGoBlockAggType = nil
	c.failableGoBlockFinalSuspend = nil
	c.goBlockValueResultLLVM = nil
	c.coroResultSlot = nil

	retTy := vb.plain.Sig.RetType
	if _, isVoid := retTy.(*irtypes.VoidType); isVoid {
		return
	}
	c.coroResultSlot = vb.variant.Params[len(vb.variant.Params)-1]
	if failable {
		c.inFailableGoBlock = true
		c.failableGoBlockAggType = retTy.(*irtypes.StructType)
		c.failableGoBlockFinalSuspend = vb.finalSuspend
	} else {
		c.goBlockValueResultLLVM = retTy
	}
}

// endCoroVariantBody closes a variant body after the emitter has run its
// fall-through scope cleanup: a live fall-through stores the result default
// (plain's `ret zero`) and joins the final suspend, then the panic exit, final
// suspend, cleanup and done blocks are emitted and the enclosing body's state is
// restored. No-op for a plain body.
func (c *Compiler) endCoroVariantBody(vb *coroVariantBody) {
	if vb == nil {
		return
	}
	if c.block != nil && c.block.Term == nil {
		c.storeGoResultDefault()
		c.block.NewBr(vb.finalSuspend)
	}

	// Panic exit: emitPanicReturn has already run the scope cleanup. The TLS
	// panic flag stays set, so the caller's per-call panic check propagates the
	// panic exactly as it does after plain's `ret zero`.
	c.block = vb.panicExit
	c.storeGoResultDefault()
	c.block.NewBr(vb.finalSuspend)

	// Final suspend keeps the frame alive so the caller can see coro.done; the
	// caller destroys it.
	finalResult := vb.finalSuspend.NewCall(c.coroSuspend, constant.None, constant.True)
	vb.finalSuspend.NewSwitch(finalResult, vb.done,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), vb.done),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), vb.cleanup))

	coroMem := vb.cleanup.NewCall(c.coroFree, vb.coroID, vb.hdl)
	needFree := vb.cleanup.NewICmp(enum.IPredNE, coroMem, constant.NewNull(irtypes.I8Ptr))
	freeBlk := vb.variant.NewBlock("coro.free")
	vb.cleanup.NewCondBr(needFree, freeBlk, vb.done)
	freeBlk.NewCall(c.palFree, coroMem)
	freeBlk.NewBr(vb.done)

	vb.done.NewCall(c.coroEnd, vb.hdl, constant.False, constant.None)
	vb.done.NewRet(vb.hdl)

	c.canError = vb.canError
	c.inCoroutine = vb.inCoroutine
	c.coroCleanupBlk = vb.coroCleanupBlk
	c.coroSuspendBlk = vb.coroSuspendBlk
	c.panicExitBlock = vb.panicExitBlock
	c.coroutineReturnBlock = vb.coroutineReturnBlock
	c.inFailableGoBlock = vb.inFailableGoBlock
	c.failableGoBlockAggType = vb.failableGoBlockAggType
	c.failableGoBlockFinalSuspend = vb.failableGoBlockFinalSuspend
	c.goBlockValueResultLLVM = vb.goBlockValueResultLLVM
	c.coroResultSlot = vb.coroResultSlot
	c.block = nil
}

// emitDirectCall emits a statically dispatched call to fn. Inside a coroutine
// body, when fn can block and so has a coroutine variant, it instead runs the
// variant and suspends the calling coroutine until the variant completes:
//
//	h = fn$coro(args..., slot)
//	check:   if done(h) → done, else → suspend
//	suspend: coro.suspend → resume: resume(h), → check
//	                      → destroy: destroy(h), → the caller's cleanup
//	done:    destroy(h); result = load slot
//
// A park inside the variant has already recorded itself on the G (park_mutex),
// so the scheduler sees a parked goroutine once the whole chain has suspended,
// and resumes the outermost frame when it is woken. The resume/done/destroy
// calls go through the noinline generator wrappers so CoroElide cannot place the
// variant's frame on the caller's stack. Returns the call's result, or nil for a
// void fn.
func (c *Compiler) emitDirectCall(fn *ir.Func, args ...value.Value) value.Value {
	if !c.inCoroutine || c.coroSuspendBlk == nil || !c.coroVariants[fn.Name()] {
		return c.block.NewCall(fn, args...)
	}
	variant := c.coroVariantFor(fn)
	retTy := fn.Sig.RetType
	_, isVoid := retTy.(*irtypes.VoidType)

	callArgs := append([]value.Value(nil), args...)
	var slot *ir.InstAlloca
	if !isVoid {
		slot = c.createEntryAlloca(retTy)
		slot.SetName(c.uniqueLocalName("await.slot"))
		callArgs = append(callArgs, slot)
	}
	hdl := c.block.NewCall(variant, callArgs...)

	checkBlk := c.newBlock("await.check")
	suspendBlk := c.newBlock("await.suspend")
	resumeBlk := c.newBlock("await.resume")
	destroyBlk := c.newBlock("await.destroy")
	doneBlk := c.newBlock("await.done")
	c.block.NewBr(checkBlk)

	done := checkBlk.NewCall(c.genDone, hdl)
	checkBlk.NewCondBr(done, doneBlk, suspendBlk)

	suspResult := suspendBlk.NewCall(c.coroSuspend, constant.None, constant.False)
	suspendBlk.NewSwitch(suspResult, c.coroSuspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), destroyBlk))

	resumeBlk.NewCall(c.genResume, hdl)
	resumeBlk.NewBr(checkBlk)

	destroyBlk.NewCall(c.genDestroy, hdl)
	destroyBlk.NewBr(c.coroCleanupBlk)

	doneBlk.NewCall(c.genDestroy, hdl)
	c.block = doneBlk
	if isVoid {
		return nil
	}
	return doneBlk.NewLoad(retTy, slot)
}
