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

// defineWasmSetjmp emits a stub _setjmp that always returns 0 (no panic recovery on WASM).
func (c *Compiler) defineWasmSetjmp() *ir.Func {
	env := ir.NewParam("env", irtypes.I8Ptr)
	fn := c.module.NewFunc("_setjmp", irtypes.I32, env)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, 0))
	return fn
}

// defineWasmLongjmp emits a stub _longjmp that is unreachable.
// On WASM, promise_panic always exits — longjmp is never called.
func (c *Compiler) defineWasmLongjmp() *ir.Func {
	env := ir.NewParam("env", irtypes.I8Ptr)
	val := ir.NewParam("val", irtypes.I32)
	fn := c.module.NewFunc("_longjmp", irtypes.Void, env, val)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewUnreachable()
	return fn
}

// emitWasmStart creates the @_start WASI entry point.
// _start calls @main (which has scheduler code). The allocator self-initializes.
func (c *Compiler) emitWasmStart(mainFn *ir.Func) {
	startFn := c.module.NewFunc("_start", irtypes.Void)
	startFn.FuncAttrs = append(startFn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := startFn.NewBlock(".entry")

	// Call @main(argc=0, argv=null) — WASM has no command-line arguments
	exitCode := entry.NewCall(mainFn,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.NewPointer(irtypes.I8Ptr)))

	// Exit with main's return code
	entry.NewCall(c.palExit, exitCode)
	entry.NewUnreachable()
}
