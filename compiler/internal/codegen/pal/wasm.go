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

// --- WASM allocator (linked from pre-compiled wasm_alloc.o) ---
//
// The allocator is a C free-list implementation compiled to WASM and linked
// via wasm-ld. It provides malloc/free/realloc with size-class buckets.
// See cmd/promise/crt/wasm32/wasm_alloc.c for the implementation.

// EmitAlloc declares extern @malloc and defines @pal_alloc as a wrapper.
// Signature: @pal_alloc(i64 %size) → i8*
func (p *WasmPAL) EmitAlloc(module *ir.Module) *ir.Func {
	// declare noalias i8* @malloc(i32 noundef) nounwind
	mallocSize := ir.NewParam("size", irtypes.I32)
	mallocSize.Attrs = append(mallocSize.Attrs, enum.ParamAttrNoUndef)
	mallocFn := module.NewFunc("malloc", irtypes.I8Ptr, mallocSize)
	mallocFn.ReturnAttrs = append(mallocFn.ReturnAttrs, enum.ReturnAttrNoAlias)
	mallocFn.FuncAttrs = append(mallocFn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)

	// define noalias i8* @pal_alloc(i64 %size) nounwind
	fn := module.NewFunc("pal_alloc", irtypes.I8Ptr,
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock("entry")

	// Truncate i64 to i32 (wasm32 address space)
	size32 := entry.NewTrunc(fn.Params[0], irtypes.I32)
	ret := entry.NewCall(mallocFn, size32)
	entry.NewRet(ret)

	return fn
}

// EmitFree declares extern @free and defines @pal_free as a wrapper.
func (p *WasmPAL) EmitFree(module *ir.Module) *ir.Func {
	// declare void @free(i8* nocapture noundef) nounwind
	freePtr := ir.NewParam("ptr", irtypes.I8Ptr)
	freePtr.Attrs = append(freePtr.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	freeFn := module.NewFunc("free", irtypes.Void, freePtr)
	freeFn.FuncAttrs = append(freeFn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)

	// define void @pal_free(i8* %ptr) nounwind willreturn
	fn := module.NewFunc("pal_free", irtypes.Void,
		ir.NewParam("ptr", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock("entry")
	entry.NewCall(freeFn, fn.Params[0])
	entry.NewRet(nil)

	return fn
}

// EmitRealloc declares extern @realloc and defines @pal_realloc as a wrapper.
func (p *WasmPAL) EmitRealloc(module *ir.Module) *ir.Func {
	// declare noalias i8* @realloc(i8* nocapture noundef, i32 noundef) nounwind
	reallocPtr := ir.NewParam("ptr", irtypes.I8Ptr)
	reallocPtr.Attrs = append(reallocPtr.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	reallocSize := ir.NewParam("size", irtypes.I32)
	reallocSize.Attrs = append(reallocSize.Attrs, enum.ParamAttrNoUndef)
	reallocFn := module.NewFunc("realloc", irtypes.I8Ptr, reallocPtr, reallocSize)
	reallocFn.ReturnAttrs = append(reallocFn.ReturnAttrs, enum.ReturnAttrNoAlias)
	reallocFn.FuncAttrs = append(reallocFn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)

	// define noalias i8* @pal_realloc(i8* %ptr, i64 %size) nounwind
	fn := module.NewFunc("pal_realloc", irtypes.I8Ptr,
		ir.NewParam("ptr", irtypes.I8Ptr),
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock("entry")

	size32 := entry.NewTrunc(fn.Params[1], irtypes.I32)
	ret := entry.NewCall(reallocFn, fn.Params[0], size32)
	entry.NewRet(ret)

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
