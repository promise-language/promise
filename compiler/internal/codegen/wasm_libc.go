package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// defineWasmMemcmp emits a byte-by-byte memcmp for WASM (no libc).
// Signature: @memcmp(i8* %s1, i8* %s2, i64 %n) → i32
func (c *Compiler) defineWasmMemcmp() *ir.Func {
	s1 := ir.NewParam("s1", irtypes.I8Ptr)
	s2 := ir.NewParam("s2", irtypes.I8Ptr)
	n := ir.NewParam("n", irtypes.I64)
	fn := c.module.NewFunc("memcmp", irtypes.I32, s1, s2, n)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	loopBlk := fn.NewBlock("loop")
	neqBlk := fn.NewBlock("not_equal")
	continueBlk := fn.NewBlock("continue")
	doneBlk := fn.NewBlock("done")

	// if n == 0, return 0
	isZero := entry.NewICmp(enum.IPredEQ, n, constant.NewInt(irtypes.I64, 0))
	entry.NewCondBr(isZero, doneBlk, loopBlk)

	// loop: compare byte by byte
	iPhi := loopBlk.NewPhi(ir.NewIncoming(constant.NewInt(irtypes.I64, 0), entry))
	b1Ptr := loopBlk.NewGetElementPtr(irtypes.I8, s1, iPhi)
	b1 := loopBlk.NewLoad(irtypes.I8, b1Ptr)
	b2Ptr := loopBlk.NewGetElementPtr(irtypes.I8, s2, iPhi)
	b2 := loopBlk.NewLoad(irtypes.I8, b2Ptr)

	bytesEqual := loopBlk.NewICmp(enum.IPredEQ, b1, b2)
	loopBlk.NewCondBr(bytesEqual, continueBlk, neqBlk)

	// continue: advance index, check if done
	iNext := continueBlk.NewAdd(iPhi, constant.NewInt(irtypes.I64, 1))
	iPhi.Incs = append(iPhi.Incs, ir.NewIncoming(iNext, continueBlk))
	loopDone := continueBlk.NewICmp(enum.IPredEQ, iNext, n)
	continueBlk.NewCondBr(loopDone, doneBlk, loopBlk)

	// not_equal: return (unsigned)b1 - (unsigned)b2
	b1ext := neqBlk.NewZExt(b1, irtypes.I32)
	b2ext := neqBlk.NewZExt(b2, irtypes.I32)
	diff := neqBlk.NewSub(b1ext, b2ext)
	neqBlk.NewRet(diff)

	// done: equal
	doneBlk.NewRet(constant.NewInt(irtypes.I32, 0))

	return fn
}

// defineWasmStrlen emits a null-terminator scan for WASM (no libc).
// Signature: @strlen(i8* %s) → i64
func (c *Compiler) defineWasmStrlen() *ir.Func {
	s := ir.NewParam("s", irtypes.I8Ptr)
	fn := c.module.NewFunc("strlen", irtypes.I64, s)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrReadOnly)

	entry := fn.NewBlock(".entry")
	loopBlk := fn.NewBlock("loop")
	doneBlk := fn.NewBlock("done")

	entry.NewBr(loopBlk)

	iPhi := loopBlk.NewPhi(ir.NewIncoming(constant.NewInt(irtypes.I64, 0), entry))
	charPtr := loopBlk.NewGetElementPtr(irtypes.I8, s, iPhi)
	ch := loopBlk.NewLoad(irtypes.I8, charPtr)
	isNull := loopBlk.NewICmp(enum.IPredEQ, ch, constant.NewInt(irtypes.I8, 0))

	iNext := loopBlk.NewAdd(iPhi, constant.NewInt(irtypes.I64, 1))
	iPhi.Incs = append(iPhi.Incs, ir.NewIncoming(iNext, loopBlk))

	loopBlk.NewCondBr(isNull, doneBlk, loopBlk)

	doneBlk.NewRet(iPhi)

	return fn
}

// defineWasmUsleep emits a no-op usleep for WASM.
func (c *Compiler) defineWasmUsleep() *ir.Func {
	usec := ir.NewParam("usec", irtypes.I32)
	fn := c.module.NewFunc("usleep", irtypes.I32, usec)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, 0))
	return fn
}

// emitWasmStart creates the WASM entry point. For wasm32-wasi this is `_start`
// (the WASI Command convention): the body calls @main and then @pal_exit with
// main's return code, unchanged from before this file existed.
//
// For wasm32-web it is `_initialize` (called from JS / the Node test harness —
// there is no WASI runtime to invoke `_start` automatically), and it applies
// the liveness rule of docs/wasm-web-callbacks.md §4.1: @main (via
// wrapMainWithScheduler's isWasmWeb branch, which runs the unbounded initial
// drain of defineWebReactorDrainFunc) returns 0 when the drain completed with
// zero live registrations — pal_exit exactly as today — or 1 when the
// instance is a reactor, in which case `_initialize` returns instead of
// exiting, leaving the heap, registrations, and parked goroutines intact for
// the host to call promise_web_pump into later. Every program that registers
// nothing (i.e. every program that doesn't `use web;` and subscribe —
// sched_wasm_web_delivery.go's promise_web_subscribe is the only writer of
// live registrations) takes the pal_exit branch, so this stays bit-for-bit
// unaffected for such programs.
func (c *Compiler) emitWasmStart(mainFn *ir.Func) {
	name := "_start"
	if c.isWasmWeb {
		name = "_initialize"
	}
	startFn := c.module.NewFunc(name, irtypes.Void)
	startFn.FuncAttrs = append(startFn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := startFn.NewBlock(".entry")

	// Call @main(argc=0, argv=null) — WASM has no command-line arguments
	result := entry.NewCall(mainFn,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.NewPointer(irtypes.I8Ptr)))

	if c.isWasmWeb {
		stayAlive := entry.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
		exitBlk := startFn.NewBlock("exit")
		returnBlk := startFn.NewBlock("return")
		entry.NewCondBr(stayAlive, returnBlk, exitBlk)

		exitBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 0))
		exitBlk.NewUnreachable()

		returnBlk.NewRet(nil)
		return
	}

	// wasm32-wasi: exit with main's return code, unconditionally.
	entry.NewCall(c.palExit, result)
	entry.NewUnreachable()
}
