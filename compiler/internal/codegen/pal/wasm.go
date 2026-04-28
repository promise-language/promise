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
	fdWrite.FuncAttrs = append(fdWrite.FuncAttrs, enum.FuncAttrNoUnwind)

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
		enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind)

	// define void @pal_exit(i32 %code) noreturn
	fn := module.NewFunc("pal_exit", irtypes.Void,
		ir.NewParam("code", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewCall(procExit, fn.Params[0])
	entry.NewUnreachable()

	return fn
}

// WASI libc provides malloc/free/realloc. Phase 4b replaces with memory.grow bump allocator.
func (p *WasmPAL) EmitAlloc(module *ir.Module) *ir.Func   { return emitLibcAlloc(module) }
func (p *WasmPAL) EmitFree(module *ir.Module) *ir.Func    { return emitLibcFree(module) }
func (p *WasmPAL) EmitRealloc(module *ir.Module) *ir.Func { return emitLibcRealloc(module) }

// WASM threading stubs — run synchronously. WASM has no threads (Phase 5b deferred).
func (p *WasmPAL) EmitThreadCreate(module *ir.Module) *ir.Func { return emitStubThreadCreate(module) }
func (p *WasmPAL) EmitThreadJoin(module *ir.Module) *ir.Func   { return emitStubThreadJoin(module) }
func (p *WasmPAL) EmitMutexInit(module *ir.Module) *ir.Func    { return emitStubMutexInit(module) }
func (p *WasmPAL) EmitMutexLock(module *ir.Module) *ir.Func    { return emitStubMutexLock(module) }
func (p *WasmPAL) EmitMutexUnlock(module *ir.Module) *ir.Func  { return emitStubMutexUnlock(module) }
func (p *WasmPAL) EmitMutexDestroy(module *ir.Module) *ir.Func { return emitStubMutexDestroy(module) }
func (p *WasmPAL) EmitCondInit(module *ir.Module) *ir.Func     { return emitStubCondInit(module) }
func (p *WasmPAL) EmitCondWait(module *ir.Module) *ir.Func     { return emitStubCondWait(module) }
func (p *WasmPAL) EmitCondSignal(module *ir.Module) *ir.Func   { return emitStubCondSignal(module) }
func (p *WasmPAL) EmitCondDestroy(module *ir.Module) *ir.Func  { return emitStubCondDestroy(module) }
