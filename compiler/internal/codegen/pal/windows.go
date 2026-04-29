package pal

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// WindowsPAL implements PAL for Windows using Win32 API (kernel32.dll).
type WindowsPAL struct{}

// EmitWrite declares Win32 GetStdHandle/WriteFile and defines @pal_write.
// Maps POSIX fd (0/1/2) to Windows HANDLE via GetStdHandle, then calls WriteFile.
// Signature: @pal_write(i32 %fd, i8* %buf, i64 %len) → i64
func (p *WindowsPAL) EmitWrite(module *ir.Module) *ir.Func {
	// declare i8* @GetStdHandle(i32)
	getStdHandle := module.NewFunc("GetStdHandle", irtypes.I8Ptr,
		ir.NewParam("nStdHandle", irtypes.I32))
	getStdHandle.FuncAttrs = append(getStdHandle.FuncAttrs, enum.FuncAttrNoUnwind)

	// declare i32 @WriteFile(i8*, i8*, i32, i32*, i8*)
	writeFile := module.NewFunc("WriteFile", irtypes.I32,
		ir.NewParam("hFile", irtypes.I8Ptr),
		ir.NewParam("lpBuffer", irtypes.I8Ptr),
		ir.NewParam("nNumberOfBytesToWrite", irtypes.I32),
		ir.NewParam("lpNumberOfBytesWritten", irtypes.NewPointer(irtypes.I32)),
		ir.NewParam("lpOverlapped", irtypes.I8Ptr))
	writeFile.FuncAttrs = append(writeFile.FuncAttrs, enum.FuncAttrNoUnwind)

	// define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)
	fn := module.NewFunc("pal_write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	entry := fn.NewBlock("entry")

	// Map fd to Windows handle constant: sub i32 -10, %fd
	// fd 0 → -10 (STD_INPUT_HANDLE)
	// fd 1 → -11 (STD_OUTPUT_HANDLE)
	// fd 2 → -12 (STD_ERROR_HANDLE)
	handleConst := entry.NewSub(constant.NewInt(irtypes.I32, -10), fn.Params[0])

	// Get the actual HANDLE
	handle := entry.NewCall(getStdHandle, handleConst)

	// Truncate len from i64 to i32 (WriteFile takes DWORD)
	len32 := entry.NewTrunc(fn.Params[2], irtypes.I32)

	// Alloca i32 for bytes written
	writtenPtr := entry.NewAlloca(irtypes.I32)

	// Call WriteFile(handle, buf, len32, &written, null)
	entry.NewCall(writeFile, handle, fn.Params[1], len32, writtenPtr,
		constant.NewNull(irtypes.I8Ptr))

	// Load written count, zero-extend to i64, return
	written := entry.NewLoad(irtypes.I32, writtenPtr)
	written64 := entry.NewZExt(written, irtypes.I64)
	entry.NewRet(written64)

	return fn
}

// EmitExit declares Win32 ExitProcess and defines @pal_exit as a noreturn wrapper.
// Signature: @pal_exit(i32 %code) → void [noreturn]
func (p *WindowsPAL) EmitExit(module *ir.Module) *ir.Func {
	// declare void @ExitProcess(i32) noreturn
	exitProcess := module.NewFunc("ExitProcess", irtypes.Void,
		ir.NewParam("uExitCode", irtypes.I32))
	exitProcess.FuncAttrs = append(exitProcess.FuncAttrs,
		enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind)

	// define void @pal_exit(i32 %code) noreturn
	fn := module.NewFunc("pal_exit", irtypes.Void,
		ir.NewParam("code", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewCall(exitProcess, fn.Params[0])
	entry.NewUnreachable()

	return fn
}

// Windows UCRT provides libc-compatible malloc/free/realloc.
func (p *WindowsPAL) EmitAlloc(module *ir.Module) *ir.Func   { return emitLibcAlloc(module) }
func (p *WindowsPAL) EmitFree(module *ir.Module) *ir.Func    { return emitLibcFree(module) }
func (p *WindowsPAL) EmitRealloc(module *ir.Module) *ir.Func { return emitLibcRealloc(module) }

// Windows threading stubs — run synchronously. Replace with CreateThread/CRITICAL_SECTION later.
func (p *WindowsPAL) EmitThreadCreate(module *ir.Module) *ir.Func {
	return emitStubThreadCreate(module)
}
func (p *WindowsPAL) EmitThreadJoin(module *ir.Module) *ir.Func  { return emitStubThreadJoin(module) }
func (p *WindowsPAL) EmitMutexInit(module *ir.Module) *ir.Func   { return emitStubMutexInit(module) }
func (p *WindowsPAL) EmitMutexLock(module *ir.Module) *ir.Func   { return emitStubMutexLock(module) }
func (p *WindowsPAL) EmitMutexUnlock(module *ir.Module) *ir.Func { return emitStubMutexUnlock(module) }
func (p *WindowsPAL) EmitMutexDestroy(module *ir.Module) *ir.Func {
	return emitStubMutexDestroy(module)
}
func (p *WindowsPAL) EmitCondInit(module *ir.Module) *ir.Func   { return emitStubCondInit(module) }
func (p *WindowsPAL) EmitCondWait(module *ir.Module) *ir.Func   { return emitStubCondWait(module) }
func (p *WindowsPAL) EmitCondSignal(module *ir.Module) *ir.Func { return emitStubCondSignal(module) }
func (p *WindowsPAL) EmitCondBroadcast(module *ir.Module) *ir.Func {
	return emitStubCondBroadcast(module)
}
func (p *WindowsPAL) EmitCondDestroy(module *ir.Module) *ir.Func { return emitStubCondDestroy(module) }
func (p *WindowsPAL) EmitNumCPUs(module *ir.Module) *ir.Func     { return emitStubNumCPUs(module) }
