package pal

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// WasmPAL implements PAL for WebAssembly using WASI (WebAssembly System Interface).
type WasmPAL struct{}

// EmitWrite declares WASI fd_write and defines @pal_write.
// Builds a ciovec {i8*, i32} on the stack and calls fd_write with a single iovec.
// Signature: @pal_write(i32 %fd, i8* %buf, i64 %len) → i64
func (p *WasmPAL) EmitWrite(module *ir.Module) *ir.Func {
	// ciovec struct type: {i8*, i32} (buffer pointer + buffer length)
	ciovecType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I32)

	// declare i32 @fd_write(i32, {i8*, i32}*, i32, i32*)
	fdWrite := module.NewFunc("fd_write", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("iovs", irtypes.NewPointer(ciovecType)),
		ir.NewParam("iovs_len", irtypes.I32),
		ir.NewParam("nwritten", irtypes.NewPointer(irtypes.I32)))
	fdWrite.FuncAttrs = append(fdWrite.FuncAttrs, enum.FuncAttrNoUnwind,
		ir.AttrPair{Key: "wasm-import-module", Value: "wasi_snapshot_preview1"},
		ir.AttrPair{Key: "wasm-import-name", Value: "fd_write"})

	// define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)
	fn := module.NewFunc("pal_write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	entry := fn.NewBlock("entry")

	// Alloca ciovec {i8*, i32} on the stack
	iov := entry.NewAlloca(ciovecType)

	// Store buf into iovec field 0 (buffer pointer)
	bufPtr := entry.NewGetElementPtr(ciovecType, iov,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	entry.NewStore(fn.Params[1], bufPtr)

	// Truncate len from i64 to i32, store into iovec field 1 (buffer length)
	len32 := entry.NewTrunc(fn.Params[2], irtypes.I32)
	lenPtr := entry.NewGetElementPtr(ciovecType, iov,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	entry.NewStore(len32, lenPtr)

	// Alloca i32 for nwritten
	nwrittenPtr := entry.NewAlloca(irtypes.I32)

	// Call fd_write(fd, &iov, 1, &nwritten)
	entry.NewCall(fdWrite, fn.Params[0], iov,
		constant.NewInt(irtypes.I32, 1), nwrittenPtr)

	// Load nwritten, zero-extend to i64, return
	nwritten := entry.NewLoad(irtypes.I32, nwrittenPtr)
	nwritten64 := entry.NewZExt(nwritten, irtypes.I64)
	entry.NewRet(nwritten64)

	return fn
}

// EmitExit declares WASI proc_exit and defines @pal_exit as a noreturn wrapper.
// Signature: @pal_exit(i32 %code) → void [noreturn]
func (p *WasmPAL) EmitExit(module *ir.Module) *ir.Func {
	// declare void @proc_exit(i32) noreturn
	procExit := module.NewFunc("proc_exit", irtypes.Void,
		ir.NewParam("rval", irtypes.I32))
	procExit.FuncAttrs = append(procExit.FuncAttrs,
		enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind,
		ir.AttrPair{Key: "wasm-import-module", Value: "wasi_snapshot_preview1"},
		ir.AttrPair{Key: "wasm-import-name", Value: "proc_exit"})

	// define void @pal_exit(i32 %code) noreturn
	fn := module.NewFunc("pal_exit", irtypes.Void,
		ir.NewParam("code", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewCall(procExit, fn.Params[0])
	entry.NewUnreachable()

	return fn
}

// --- Custom bump allocator (Phase 4b) ---
//
// Uses WASM linear memory via memory.grow/memory.size.
// Each allocation has an 8-byte header storing the block size (for realloc).
// free() is a no-op (bump only). Memory grows but never shrinks.
//
// Globals:
//   @__promise_heap_ptr  — current bump pointer (i32)
//   @__promise_heap_end  — end of committed memory (i32)
//   @__heap_base         — provided by wasm-ld, start of heap after stack/data

// emitWasmAllocGlobals declares the heap state globals and the __heap_base external.
func emitWasmAllocGlobals(module *ir.Module) (heapPtr, heapEnd, heapBase *ir.Global) {
	heapPtr = module.NewGlobal("__promise_heap_ptr", irtypes.I32)
	heapPtr.Init = constant.NewInt(irtypes.I32, 0)

	heapEnd = module.NewGlobal("__promise_heap_end", irtypes.I32)
	heapEnd.Init = constant.NewInt(irtypes.I32, 0)

	// __heap_base is provided by wasm-ld
	heapBase = module.NewGlobal("__heap_base", irtypes.I32)
	heapBase.Linkage = enum.LinkageExternal
	heapBase.Immutable = true

	return
}

// emitWasmInitHeap emits @__promise_init_heap which sets heap_ptr and heap_end
// from __heap_base and current memory.size. Must be called once at _start.
func emitWasmInitHeap(module *ir.Module, heapPtr, heapEnd, heapBase *ir.Global) *ir.Func {
	memorySize := module.NewFunc("llvm.wasm.memory.size.i32", irtypes.I32,
		ir.NewParam("mem", irtypes.I32))
	memorySize.FuncAttrs = append(memorySize.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("__promise_init_heap", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")

	// Align __heap_base up to 8 bytes
	base := entry.NewLoad(irtypes.I32, heapBase)
	basePlus7 := entry.NewAdd(base, constant.NewInt(irtypes.I32, 7))
	aligned := entry.NewAnd(basePlus7, constant.NewInt(irtypes.I32, -8)) // & ~7
	entry.NewStore(aligned, heapPtr)

	// heap_end = memory.size(0) * 65536
	pages := entry.NewCall(memorySize, constant.NewInt(irtypes.I32, 0))
	end := entry.NewMul(pages, constant.NewInt(irtypes.I32, 65536))
	entry.NewStore(end, heapEnd)

	entry.NewRet(nil)
	return fn
}

// EmitAlloc defines @pal_alloc using a bump allocator on WASM linear memory.
// Signature: @pal_alloc(i64 %size) → i8*
func (p *WasmPAL) EmitAlloc(module *ir.Module) *ir.Func {
	heapPtr, heapEnd, heapBase := emitWasmAllocGlobals(module)
	emitWasmInitHeap(module, heapPtr, heapEnd, heapBase)

	memoryGrow := module.NewFunc("llvm.wasm.memory.grow.i32", irtypes.I32,
		ir.NewParam("mem", irtypes.I32),
		ir.NewParam("pages", irtypes.I32))
	memoryGrow.FuncAttrs = append(memoryGrow.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_alloc", irtypes.I8Ptr,
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock("entry")
	growBlk := fn.NewBlock("grow")
	oomBlk := fn.NewBlock("oom")
	doneBlk := fn.NewBlock("done")

	// Truncate i64 size to i32 (wasm32 address space)
	size32 := entry.NewTrunc(fn.Params[0], irtypes.I32)

	// total = align_up(size32, 8) + 8 (header)
	sizePlus7 := entry.NewAdd(size32, constant.NewInt(irtypes.I32, 7))
	sizeAligned := entry.NewAnd(sizePlus7, constant.NewInt(irtypes.I32, -8))
	total := entry.NewAdd(sizeAligned, constant.NewInt(irtypes.I32, 8)) // 8-byte header

	// Load current heap state
	curPtr := entry.NewLoad(irtypes.I32, heapPtr)
	curEnd := entry.NewLoad(irtypes.I32, heapEnd)

	// newPtr = curPtr + total
	newPtr := entry.NewAdd(curPtr, total)

	// if newPtr > curEnd: grow
	needGrow := entry.NewICmp(enum.IPredUGT, newPtr, curEnd)
	entry.NewCondBr(needGrow, growBlk, doneBlk)

	// grow block: call memory.grow with enough pages
	deficit := growBlk.NewSub(newPtr, curEnd)
	deficitRounded := growBlk.NewAdd(deficit, constant.NewInt(irtypes.I32, 65535))
	pagesNeeded := growBlk.NewUDiv(deficitRounded, constant.NewInt(irtypes.I32, 65536))
	growResult := growBlk.NewCall(memoryGrow, constant.NewInt(irtypes.I32, 0), pagesNeeded)
	// memory.grow returns -1 on failure
	growFailed := growBlk.NewICmp(enum.IPredEQ, growResult, constant.NewInt(irtypes.I32, -1))
	growOkBlk := fn.NewBlock("grow_ok")
	growBlk.NewCondBr(growFailed, oomBlk, growOkBlk)

	// OOM: trap (WASM runtime will report the error)
	oomBlk.NewUnreachable()

	// grow_ok: update heap_end after successful grow
	newEnd := growOkBlk.NewAdd(curEnd, growOkBlk.NewMul(pagesNeeded, constant.NewInt(irtypes.I32, 65536)))
	growOkBlk.NewStore(newEnd, heapEnd)
	growOkBlk.NewBr(doneBlk)

	// done block: store size in header, advance heap_ptr, return ptr+8
	// Header at curPtr: store the allocation size (for realloc)
	headerPtr := doneBlk.NewIntToPtr(curPtr, irtypes.NewPointer(irtypes.I32))
	doneBlk.NewStore(sizeAligned, headerPtr)

	// User pointer = curPtr + 8
	userAddr := doneBlk.NewAdd(curPtr, constant.NewInt(irtypes.I32, 8))
	userPtr := doneBlk.NewIntToPtr(userAddr, irtypes.I8Ptr)

	// Advance heap_ptr
	doneBlk.NewStore(newPtr, heapPtr)

	doneBlk.NewRet(userPtr)
	return fn
}

// EmitFree defines @pal_free as a no-op (bump allocator doesn't reclaim).
func (p *WasmPAL) EmitFree(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_free", irtypes.Void,
		ir.NewParam("ptr", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock("entry")
	entry.NewRet(nil)
	return fn
}

// EmitRealloc defines @pal_realloc: allocate new, copy, return new (old is leaked).
func (p *WasmPAL) EmitRealloc(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")
	memcpyFn := emitWasmMemcpy(module)

	fn := module.NewFunc("pal_realloc", irtypes.I8Ptr,
		ir.NewParam("ptr", irtypes.I8Ptr),
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock("entry")
	nullBlk := fn.NewBlock("null_ptr")
	copyBlk := fn.NewBlock("copy")

	// If ptr is null, just allocate
	isNull := entry.NewICmp(enum.IPredEQ, fn.Params[0], constant.NewNull(irtypes.I8Ptr))
	entry.NewCondBr(isNull, nullBlk, copyBlk)

	// null case: return alloc(size)
	newAlloc := nullBlk.NewCall(palAlloc, fn.Params[1])
	nullBlk.NewRet(newAlloc)

	// copy case: read old size from header (ptr - 8), alloc new, memcpy, return new
	ptrInt := copyBlk.NewPtrToInt(fn.Params[0], irtypes.I32)
	headerAddr := copyBlk.NewSub(ptrInt, constant.NewInt(irtypes.I32, 8))
	headerP := copyBlk.NewIntToPtr(headerAddr, irtypes.NewPointer(irtypes.I32))
	oldSize := copyBlk.NewLoad(irtypes.I32, headerP)

	newSize32 := copyBlk.NewTrunc(fn.Params[1], irtypes.I32)
	// copyLen = min(oldSize, newSize32)
	useOld := copyBlk.NewICmp(enum.IPredULT, oldSize, newSize32)
	copyLen := copyBlk.NewSelect(useOld, oldSize, newSize32)

	newPtr := copyBlk.NewCall(palAlloc, fn.Params[1])
	// Copy bytes
	copyLen64 := copyBlk.NewZExt(copyLen, irtypes.I64)
	copyBlk.NewCall(memcpyFn, newPtr, fn.Params[0], copyLen64)

	copyBlk.NewRet(newPtr)
	return fn
}

// emitWasmMemcpy emits a simple byte-by-byte memcpy for WASM (no libc).
// Signature: @__promise_memcpy(i8* %dst, i8* %src, i64 %n) → void
func emitWasmMemcpy(module *ir.Module) *ir.Func {
	// Check if already defined
	if f := lookupFunc(module, "__promise_memcpy"); f != nil {
		return f
	}

	fn := module.NewFunc("__promise_memcpy", irtypes.Void,
		ir.NewParam("dst", irtypes.I8Ptr),
		ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("n", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock("entry")
	loopBlk := fn.NewBlock("loop")
	doneBlk := fn.NewBlock("done")

	// if n == 0, skip
	isZero := entry.NewICmp(enum.IPredEQ, fn.Params[2], constant.NewInt(irtypes.I64, 0))
	entry.NewCondBr(isZero, doneBlk, loopBlk)

	// loop: i = phi(0, i+1); copy byte; if i+1 == n goto done
	iPhi := loopBlk.NewPhi(
		ir.NewIncoming(constant.NewInt(irtypes.I64, 0), entry))
	srcElem := loopBlk.NewGetElementPtr(irtypes.I8, fn.Params[1], iPhi)
	b := loopBlk.NewLoad(irtypes.I8, srcElem)
	dstElem := loopBlk.NewGetElementPtr(irtypes.I8, fn.Params[0], iPhi)
	loopBlk.NewStore(b, dstElem)

	iNext := loopBlk.NewAdd(iPhi, constant.NewInt(irtypes.I64, 1))
	iPhi.Incs = append(iPhi.Incs, ir.NewIncoming(iNext, loopBlk))

	loopDone := loopBlk.NewICmp(enum.IPredEQ, iNext, fn.Params[2])
	loopBlk.NewCondBr(loopDone, doneBlk, loopBlk)

	doneBlk.NewRet(nil)
	return fn
}

// WASM threading stubs — run synchronously. WASM has no threads (Phase 5d: cooperative scheduler).
func (p *WasmPAL) EmitThreadCreate(module *ir.Module) *ir.Func  { return emitStubThreadCreate(module) }
func (p *WasmPAL) EmitThreadJoin(module *ir.Module) *ir.Func    { return emitStubThreadJoin(module) }
func (p *WasmPAL) EmitMutexInit(module *ir.Module) *ir.Func     { return emitStubMutexInit(module) }
func (p *WasmPAL) EmitMutexLock(module *ir.Module) *ir.Func     { return emitStubMutexLock(module) }
func (p *WasmPAL) EmitMutexUnlock(module *ir.Module) *ir.Func   { return emitStubMutexUnlock(module) }
func (p *WasmPAL) EmitMutexDestroy(module *ir.Module) *ir.Func  { return emitStubMutexDestroy(module) }
func (p *WasmPAL) EmitCondInit(module *ir.Module) *ir.Func      { return emitStubCondInit(module) }
func (p *WasmPAL) EmitCondWait(module *ir.Module) *ir.Func      { return emitStubCondWait(module) }
func (p *WasmPAL) EmitCondSignal(module *ir.Module) *ir.Func    { return emitStubCondSignal(module) }
func (p *WasmPAL) EmitCondBroadcast(module *ir.Module) *ir.Func { return emitStubCondBroadcast(module) }
func (p *WasmPAL) EmitCondDestroy(module *ir.Module) *ir.Func   { return emitStubCondDestroy(module) }
func (p *WasmPAL) EmitNumCPUs(module *ir.Module) *ir.Func       { return emitStubNumCPUs(module) }
