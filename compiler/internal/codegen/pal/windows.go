package pal

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// WindowsPAL implements PAL for Windows using Win32 API (kernel32.dll).
// Threading uses CRITICAL_SECTION (mutexes) and CONDITION_VARIABLE (condition vars).
// All Win32 functions are declared as LLVM externals resolved by the linker from kernel32.lib/ucrt.
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
	entry := fn.NewBlock(".entry")

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
	entry := fn.NewBlock(".entry")
	entry.NewCall(exitProcess, fn.Params[0])
	entry.NewUnreachable()

	return fn
}

// Windows UCRT provides libc-compatible malloc/free/realloc.
func (p *WindowsPAL) EmitAlloc(module *ir.Module) *ir.Func   { return emitLibcAlloc(module) }
func (p *WindowsPAL) EmitFree(module *ir.Module) *ir.Func    { return emitLibcFree(module) }
func (p *WindowsPAL) EmitRealloc(module *ir.Module) *ir.Func { return emitLibcRealloc(module) }

// --- Windows threading via Win32 API ---

// winThreadFnType returns the LLVM type for a Windows thread routine: i32 (i8*)*
// LPTHREAD_START_ROUTINE signature: DWORD WINAPI ThreadProc(LPVOID lpParameter)
func winThreadFnType() *irtypes.PointerType {
	return irtypes.NewPointer(irtypes.NewFunc(irtypes.I32, irtypes.I8Ptr))
}

// EmitThreadCreate declares CreateThread and defines @pal_thread_create.
// Emits a trampoline that adapts PAL's i8*(i8*) signature to Win32's i32(i8*).
// Creates thread with explicit 2MB stack size (matching POSIX PAL).
func (p *WindowsPAL) EmitThreadCreate(module *ir.Module) *ir.Func {
	// declare i8* @CreateThread(i8*, i64, i32(i8*)*, i8*, i32, i32*)
	createThread := module.NewFunc("CreateThread", irtypes.I8Ptr,
		ir.NewParam("lpThreadAttributes", irtypes.I8Ptr),
		ir.NewParam("dwStackSize", irtypes.I64),
		ir.NewParam("lpStartAddress", winThreadFnType()),
		ir.NewParam("lpParameter", irtypes.I8Ptr),
		ir.NewParam("dwCreationFlags", irtypes.I32),
		ir.NewParam("lpThreadId", irtypes.NewPointer(irtypes.I32)))
	createThread.FuncAttrs = append(createThread.FuncAttrs, enum.FuncAttrNoUnwind)

	// Emit trampoline: @__pal_thread_trampoline(i8* %arg) → i32
	// The arg is a 2-pointer struct: {fn_ptr, real_arg}.
	// Calls fn_ptr(real_arg), discards return, returns 0.
	trampoline := module.NewFunc("__pal_thread_trampoline", irtypes.I32,
		ir.NewParam("packed", irtypes.I8Ptr))
	trampoline.FuncAttrs = append(trampoline.FuncAttrs, enum.FuncAttrNoUnwind)
	{
		entry := trampoline.NewBlock(".entry")
		// Cast packed arg to {i8*, i8*}*
		pairPtrTy := irtypes.NewPointer(irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr))
		pairPtr := entry.NewBitCast(trampoline.Params[0], pairPtrTy)
		// Load fn pointer (field 0)
		fnField := entry.NewGetElementPtr(
			irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr),
			pairPtr,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, 0))
		fnRaw := entry.NewLoad(irtypes.I8Ptr, fnField)
		fnPtr := entry.NewBitCast(fnRaw, threadFnPtrType())
		// Load real arg (field 1)
		argField := entry.NewGetElementPtr(
			irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr),
			pairPtr,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, 1))
		realArg := entry.NewLoad(irtypes.I8Ptr, argField)
		// Call the real thread function
		entry.NewCall(fnPtr, realArg)
		// Free the packed struct
		palFree := lookupFunc(module, "pal_free")
		entry.NewCall(palFree, trampoline.Params[0])
		// Return 0 (DWORD success)
		entry.NewRet(constant.NewInt(irtypes.I32, 0))
	}

	// define i8* @pal_thread_create(i8* %fn, i8* %arg) nounwind
	fn := module.NewFunc("pal_thread_create", irtypes.I8Ptr,
		ir.NewParam("fn", irtypes.I8Ptr),
		ir.NewParam("arg", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Allocate {i8*, i8*} struct to pass both fn and arg to the trampoline
	palAlloc := lookupFunc(module, "pal_alloc")
	packed := entry.NewCall(palAlloc, constant.NewInt(irtypes.I64, 16))
	pairPtrTy := irtypes.NewPointer(irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr))
	pairPtr := entry.NewBitCast(packed, pairPtrTy)

	// Store fn pointer at field 0
	fnField := entry.NewGetElementPtr(
		irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr),
		pairPtr,
		constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I32, 0))
	entry.NewStore(fn.Params[0], fnField)

	// Store arg at field 1
	argField := entry.NewGetElementPtr(
		irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr),
		pairPtr,
		constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I32, 1))
	entry.NewStore(fn.Params[1], argField)

	// CreateThread(NULL, 2MB, trampoline, packed, 0, NULL)
	handle := entry.NewCall(createThread,
		constant.NewNull(irtypes.I8Ptr),           // lpThreadAttributes
		constant.NewInt(irtypes.I64, 2*1024*1024), // dwStackSize (2MB)
		trampoline,                      // lpStartAddress
		packed,                          // lpParameter
		constant.NewInt(irtypes.I32, 0), // dwCreationFlags (run immediately)
		constant.NewNull(irtypes.NewPointer(irtypes.I32))) // lpThreadId (don't need)
	entry.NewRet(handle)
	return fn
}

// EmitThreadJoin declares WaitForSingleObject/CloseHandle and defines @pal_thread_join.
// Waits for thread to finish (INFINITE timeout), then closes the handle.
func (p *WindowsPAL) EmitThreadJoin(module *ir.Module) *ir.Func {
	// declare i32 @WaitForSingleObject(i8*, i32) nounwind
	waitForSingleObject := module.NewFunc("WaitForSingleObject", irtypes.I32,
		ir.NewParam("hHandle", irtypes.I8Ptr),
		ir.NewParam("dwMilliseconds", irtypes.I32))
	waitForSingleObject.FuncAttrs = append(waitForSingleObject.FuncAttrs, enum.FuncAttrNoUnwind)

	// declare i32 @CloseHandle(i8*) nounwind
	closeHandle := module.NewFunc("CloseHandle", irtypes.I32,
		ir.NewParam("hObject", irtypes.I8Ptr))
	closeHandle.FuncAttrs = append(closeHandle.FuncAttrs, enum.FuncAttrNoUnwind)

	// define void @pal_thread_join(i8* %handle) nounwind
	fn := module.NewFunc("pal_thread_join", irtypes.Void,
		ir.NewParam("handle", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// WaitForSingleObject(handle, INFINITE=0xFFFFFFFF)
	entry.NewCall(waitForSingleObject, fn.Params[0],
		constant.NewInt(irtypes.I32, -1)) // 0xFFFFFFFF = INFINITE

	// CloseHandle(handle)
	entry.NewCall(closeHandle, fn.Params[0])

	entry.NewRet(nil)
	return fn
}

// EmitMutexInit defines @pal_mutex_init using CRITICAL_SECTION (40 bytes on x64).
func (p *WindowsPAL) EmitMutexInit(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")

	// declare void @InitializeCriticalSection(i8*) nounwind
	initCS := module.NewFunc("InitializeCriticalSection", irtypes.Void,
		ir.NewParam("lpCriticalSection", irtypes.I8Ptr))
	initCS.FuncAttrs = append(initCS.FuncAttrs, enum.FuncAttrNoUnwind)

	// define i8* @pal_mutex_init() nounwind
	fn := module.NewFunc("pal_mutex_init", irtypes.I8Ptr)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Allocate 40 bytes for CRITICAL_SECTION
	buf := entry.NewCall(palAlloc, constant.NewInt(irtypes.I64, 40))
	entry.NewCall(initCS, buf)
	entry.NewRet(buf)
	return fn
}

// EmitMutexLock defines @pal_mutex_lock using EnterCriticalSection.
func (p *WindowsPAL) EmitMutexLock(module *ir.Module) *ir.Func {
	// declare void @EnterCriticalSection(i8*) nounwind
	enterCS := module.NewFunc("EnterCriticalSection", irtypes.Void,
		ir.NewParam("lpCriticalSection", irtypes.I8Ptr))
	enterCS.FuncAttrs = append(enterCS.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_mutex_lock", irtypes.Void,
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewCall(enterCS, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// EmitMutexUnlock defines @pal_mutex_unlock using LeaveCriticalSection.
func (p *WindowsPAL) EmitMutexUnlock(module *ir.Module) *ir.Func {
	// declare void @LeaveCriticalSection(i8*) nounwind
	leaveCS := module.NewFunc("LeaveCriticalSection", irtypes.Void,
		ir.NewParam("lpCriticalSection", irtypes.I8Ptr))
	leaveCS.FuncAttrs = append(leaveCS.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_mutex_unlock", irtypes.Void,
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewCall(leaveCS, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// EmitMutexDestroy defines @pal_mutex_destroy: DeleteCriticalSection + free.
func (p *WindowsPAL) EmitMutexDestroy(module *ir.Module) *ir.Func {
	palFree := lookupFunc(module, "pal_free")

	// declare void @DeleteCriticalSection(i8*) nounwind
	deleteCS := module.NewFunc("DeleteCriticalSection", irtypes.Void,
		ir.NewParam("lpCriticalSection", irtypes.I8Ptr))
	deleteCS.FuncAttrs = append(deleteCS.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_mutex_destroy", irtypes.Void,
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewCall(deleteCS, fn.Params[0])
	entry.NewCall(palFree, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// EmitCondInit defines @pal_cond_init using CONDITION_VARIABLE (8 bytes on x64).
func (p *WindowsPAL) EmitCondInit(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")

	// declare void @InitializeConditionVariable(i8*) nounwind
	initCV := module.NewFunc("InitializeConditionVariable", irtypes.Void,
		ir.NewParam("lpConditionVariable", irtypes.I8Ptr))
	initCV.FuncAttrs = append(initCV.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_cond_init", irtypes.I8Ptr)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Allocate 8 bytes for CONDITION_VARIABLE
	buf := entry.NewCall(palAlloc, constant.NewInt(irtypes.I64, 8))
	entry.NewCall(initCV, buf)
	entry.NewRet(buf)
	return fn
}

// EmitCondWait defines @pal_cond_wait using SleepConditionVariableCS.
// INFINITE timeout — blocks until signaled.
func (p *WindowsPAL) EmitCondWait(module *ir.Module) *ir.Func {
	// declare i32 @SleepConditionVariableCS(i8*, i8*, i32) nounwind
	sleepCV := module.NewFunc("SleepConditionVariableCS", irtypes.I32,
		ir.NewParam("lpConditionVariable", irtypes.I8Ptr),
		ir.NewParam("lpCriticalSection", irtypes.I8Ptr),
		ir.NewParam("dwMilliseconds", irtypes.I32))
	sleepCV.FuncAttrs = append(sleepCV.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_cond_wait", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr),
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// SleepConditionVariableCS(cond, mutex, INFINITE=0xFFFFFFFF)
	entry.NewCall(sleepCV, fn.Params[0], fn.Params[1],
		constant.NewInt(irtypes.I32, -1)) // INFINITE

	entry.NewRet(nil)
	return fn
}

// EmitCondSignal defines @pal_cond_signal using WakeConditionVariable.
func (p *WindowsPAL) EmitCondSignal(module *ir.Module) *ir.Func {
	// declare void @WakeConditionVariable(i8*) nounwind
	wakeCV := module.NewFunc("WakeConditionVariable", irtypes.Void,
		ir.NewParam("lpConditionVariable", irtypes.I8Ptr))
	wakeCV.FuncAttrs = append(wakeCV.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_cond_signal", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewCall(wakeCV, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// EmitCondBroadcast defines @pal_cond_broadcast using WakeAllConditionVariable.
func (p *WindowsPAL) EmitCondBroadcast(module *ir.Module) *ir.Func {
	// declare void @WakeAllConditionVariable(i8*) nounwind
	wakeAllCV := module.NewFunc("WakeAllConditionVariable", irtypes.Void,
		ir.NewParam("lpConditionVariable", irtypes.I8Ptr))
	wakeAllCV.FuncAttrs = append(wakeAllCV.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_cond_broadcast", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewCall(wakeAllCV, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// EmitCondDestroy defines @pal_cond_destroy. Windows CONDITION_VARIABLE has no
// destroy function — just free the allocated memory.
func (p *WindowsPAL) EmitCondDestroy(module *ir.Module) *ir.Func {
	palFree := lookupFunc(module, "pal_free")

	fn := module.NewFunc("pal_cond_destroy", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewCall(palFree, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// --- Windows file I/O via UCRT (Phase D) ---

// getOrDeclareErrnoFn returns (or declares) the UCRT _errno function.
// Returns a function with signature () -> i32*.
func (p *WindowsPAL) getOrDeclareErrnoFn(module *ir.Module) *ir.Func {
	for _, fn := range module.Funcs {
		if fn.Name() == "_errno" {
			return fn
		}
	}
	fn := module.NewFunc("_errno", irtypes.NewPointer(irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	return fn
}

// emitNegErrnoReturnI32 emits a block that reads errno and returns -errno (i32).
func (p *WindowsPAL) emitNegErrnoReturnI32(errBlk *ir.Block, errnoFn *ir.Func) {
	errnoPtr := errBlk.NewCall(errnoFn)
	errnoVal := errBlk.NewLoad(irtypes.I32, errnoPtr)
	negErrno := errBlk.NewSub(constant.NewInt(irtypes.I32, 0), errnoVal)
	errBlk.NewRet(negErrno)
}

// emitNegErrnoReturnI64 emits a block that reads errno and returns -errno as i64.
func (p *WindowsPAL) emitNegErrnoReturnI64(errBlk *ir.Block, errnoFn *ir.Func) {
	errnoPtr := errBlk.NewCall(errnoFn)
	errnoVal := errBlk.NewLoad(irtypes.I32, errnoPtr)
	errnoI64 := errBlk.NewSExt(errnoVal, irtypes.I64)
	negErrno := errBlk.NewSub(constant.NewInt(irtypes.I64, 0), errnoI64)
	errBlk.NewRet(negErrno)
}

// EmitFileOpen declares UCRT @_open and defines @pal_file_open.
// Maps mode (0=open-rw, 1=read, 2=create, 3=append) to _O_* flags.
func (p *WindowsPAL) EmitFileOpen(module *ir.Module) *ir.Func {
	// declare i32 @_open(i8*, i32, i32) nounwind
	ucrtOpen := module.NewFunc("_open", irtypes.I32,
		ir.NewParam("filename", irtypes.I8Ptr),
		ir.NewParam("oflag", irtypes.I32),
		ir.NewParam("pmode", irtypes.I32))
	ucrtOpen.FuncAttrs = append(ucrtOpen.FuncAttrs, enum.FuncAttrNoUnwind)

	// Windows UCRT flags: _O_RDONLY=0, _O_RDWR=2, _O_CREAT=0x100,
	// _O_TRUNC=0x200, _O_APPEND=0x8, _O_BINARY=0x8000
	const (
		oBinary       = 0x8000
		oRDWR         = 2 | oBinary
		oRDONLY       = 0 | oBinary
		oCreateTrunc  = 2 | 0x100 | 0x200 | oBinary
		oCreateAppend = 2 | 0x100 | 0x8 | oBinary
	)

	fn := module.NewFunc("pal_file_open", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("mode", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	isRead := entry.NewICmp(enum.IPredEQ, fn.Params[1], constant.NewInt(irtypes.I32, 1))
	isCreate := entry.NewICmp(enum.IPredEQ, fn.Params[1], constant.NewInt(irtypes.I32, 2))
	isAppend := entry.NewICmp(enum.IPredEQ, fn.Params[1], constant.NewInt(irtypes.I32, 3))

	f1 := entry.NewSelect(isRead, constant.NewInt(irtypes.I32, oRDONLY), constant.NewInt(irtypes.I32, oRDWR))
	f2 := entry.NewSelect(isCreate, constant.NewInt(irtypes.I32, oCreateTrunc), f1)
	flags := entry.NewSelect(isAppend, constant.NewInt(irtypes.I32, oCreateAppend), f2)

	// _open(path, flags, _S_IREAD|_S_IWRITE=0x180)
	fd := entry.NewCall(ucrtOpen, fn.Params[0], flags, constant.NewInt(irtypes.I32, 0x180))

	isErr := entry.NewICmp(enum.IPredSLT, fd, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoFn(module))
	okBlk.NewRet(fd)
	return fn
}

// EmitFileRead declares UCRT @_read and defines @pal_file_read.
func (p *WindowsPAL) EmitFileRead(module *ir.Module) *ir.Func {
	// declare i32 @_read(i32, i8*, i32) nounwind
	ucrtRead := module.NewFunc("_read", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buffer", irtypes.I8Ptr),
		ir.NewParam("count", irtypes.I32))
	ucrtRead.FuncAttrs = append(ucrtRead.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_file_read", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Truncate i64 len to i32 (UCRT _read takes unsigned int)
	len32 := entry.NewTrunc(fn.Params[2], irtypes.I32)
	ret32 := entry.NewCall(ucrtRead, fn.Params[0], fn.Params[1], len32)
	ret64 := entry.NewSExt(ret32, irtypes.I64)

	isErr := entry.NewICmp(enum.IPredSLT, ret64, constant.NewInt(irtypes.I64, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI64(errBlk, p.getOrDeclareErrnoFn(module))
	okBlk.NewRet(ret64)
	return fn
}

// EmitFileWrite declares UCRT @_write and defines @pal_file_write.
func (p *WindowsPAL) EmitFileWrite(module *ir.Module) *ir.Func {
	// declare i32 @_write(i32, i8*, i32) nounwind
	ucrtWrite := module.NewFunc("_write", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buffer", irtypes.I8Ptr),
		ir.NewParam("count", irtypes.I32))
	ucrtWrite.FuncAttrs = append(ucrtWrite.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_file_write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	len32 := entry.NewTrunc(fn.Params[2], irtypes.I32)
	ret32 := entry.NewCall(ucrtWrite, fn.Params[0], fn.Params[1], len32)
	ret64 := entry.NewSExt(ret32, irtypes.I64)

	isErr := entry.NewICmp(enum.IPredSLT, ret64, constant.NewInt(irtypes.I64, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI64(errBlk, p.getOrDeclareErrnoFn(module))
	okBlk.NewRet(ret64)
	return fn
}

// EmitFileClose declares UCRT @_close and defines @pal_file_close.
func (p *WindowsPAL) EmitFileClose(module *ir.Module) *ir.Func {
	ucrtClose := module.NewFunc("_close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	ucrtClose.FuncAttrs = append(ucrtClose.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_file_close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(ucrtClose, fn.Params[0])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitFileSeek declares UCRT @_lseeki64 and defines @pal_file_seek.
func (p *WindowsPAL) EmitFileSeek(module *ir.Module) *ir.Func {
	ucrtLseek := module.NewFunc("_lseeki64", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("offset", irtypes.I64),
		ir.NewParam("origin", irtypes.I32))
	ucrtLseek.FuncAttrs = append(ucrtLseek.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_file_seek", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("offset", irtypes.I64),
		ir.NewParam("whence", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(ucrtLseek, fn.Params[0], fn.Params[1], fn.Params[2])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I64, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI64(errBlk, p.getOrDeclareErrnoFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitFileStatSize defines @pal_file_stat_size using _open+_lseeki64+_close.
func (p *WindowsPAL) EmitFileStatSize(module *ir.Module) *ir.Func {
	ucrtOpen := lookupFunc(module, "_open")
	ucrtLseek := lookupFunc(module, "_lseeki64")
	ucrtClose := lookupFunc(module, "_close")

	fn := module.NewFunc("pal_file_stat_size", irtypes.I64,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	failBlk := fn.NewBlock(".fail")
	gotFdBlk := fn.NewBlock(".got_fd")

	// _open(path, _O_RDONLY|_O_BINARY=0x8000, 0)
	fd := entry.NewCall(ucrtOpen, fn.Params[0], constant.NewInt(irtypes.I32, 0x8000), constant.NewInt(irtypes.I32, 0))
	isNeg := entry.NewICmp(enum.IPredSLT, fd, constant.NewInt(irtypes.I32, 0))
	entry.NewCondBr(isNeg, failBlk, gotFdBlk)

	size := gotFdBlk.NewCall(ucrtLseek, fd, constant.NewInt(irtypes.I64, 0), constant.NewInt(irtypes.I32, 2))
	gotFdBlk.NewCall(ucrtClose, fd)
	gotFdBlk.NewRet(size)

	p.emitNegErrnoReturnI64(failBlk, p.getOrDeclareErrnoFn(module))
	return fn
}

// EmitFileRemove declares UCRT @_unlink and defines @pal_file_remove.
func (p *WindowsPAL) EmitFileRemove(module *ir.Module) *ir.Func {
	ucrtUnlink := module.NewFunc("_unlink", irtypes.I32,
		ir.NewParam("filename", irtypes.I8Ptr))
	ucrtUnlink.FuncAttrs = append(ucrtUnlink.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_file_remove", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(ucrtUnlink, fn.Params[0])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitFileExists declares UCRT @_access and defines @pal_file_exists.
func (p *WindowsPAL) EmitFileExists(module *ir.Module) *ir.Func {
	ucrtAccess := module.NewFunc("_access", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("mode", irtypes.I32))
	ucrtAccess.FuncAttrs = append(ucrtAccess.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_file_exists", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	ret := entry.NewCall(ucrtAccess, fn.Params[0], constant.NewInt(irtypes.I32, 0))
	isZero := entry.NewICmp(enum.IPredEQ, ret, constant.NewInt(irtypes.I32, 0))
	result := entry.NewSelect(isZero, constant.NewInt(irtypes.I32, 1), constant.NewInt(irtypes.I32, 0))
	entry.NewRet(result)
	return fn
}

// EmitFileMkdir declares UCRT @_mkdir and defines @pal_file_mkdir.
func (p *WindowsPAL) EmitFileMkdir(module *ir.Module) *ir.Func {
	// Windows _mkdir takes only path (no mode parameter)
	ucrtMkdir := module.NewFunc("_mkdir", irtypes.I32,
		ir.NewParam("dirname", irtypes.I8Ptr))
	ucrtMkdir.FuncAttrs = append(ucrtMkdir.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_file_mkdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(ucrtMkdir, fn.Params[0])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitDirRemove declares UCRT @_rmdir and defines @pal_dir_remove.
func (p *WindowsPAL) EmitDirRemove(module *ir.Module) *ir.Func {
	ucrtRmdir := module.NewFunc("_rmdir", irtypes.I32,
		ir.NewParam("dirname", irtypes.I8Ptr))
	ucrtRmdir.FuncAttrs = append(ucrtRmdir.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_dir_remove", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(ucrtRmdir, fn.Params[0])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitDirExists declares Win32 @GetFileAttributesA and defines @pal_dir_exists.
// Checks FILE_ATTRIBUTE_DIRECTORY (0x10).
func (p *WindowsPAL) EmitDirExists(module *ir.Module) *ir.Func {
	getFileAttrs := module.NewFunc("GetFileAttributesA", irtypes.I32,
		ir.NewParam("lpFileName", irtypes.I8Ptr))
	getFileAttrs.FuncAttrs = append(getFileAttrs.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_dir_exists", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	attrs := entry.NewCall(getFileAttrs, fn.Params[0])
	// INVALID_FILE_ATTRIBUTES = -1
	isInvalid := entry.NewICmp(enum.IPredEQ, attrs, constant.NewInt(irtypes.I32, -1))
	// FILE_ATTRIBUTE_DIRECTORY = 0x10
	dirBit := entry.NewAnd(attrs, constant.NewInt(irtypes.I32, 0x10))
	isDir := entry.NewICmp(enum.IPredNE, dirBit, constant.NewInt(irtypes.I32, 0))
	// Return 1 only if valid AND directory
	validAndDir := entry.NewSelect(isInvalid, constant.False, isDir)
	result := entry.NewZExt(validAndDir, irtypes.I32)
	entry.NewRet(result)
	return fn
}

// EmitErrno declares UCRT @_errno and defines @pal_errno.
func (p *WindowsPAL) EmitErrno(module *ir.Module) *ir.Func {
	ucrtErrno := p.getOrDeclareErrnoFn(module)

	fn := module.NewFunc("pal_errno", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	ptr := entry.NewCall(ucrtErrno)
	val := entry.NewLoad(irtypes.I32, ptr)
	entry.NewRet(val)
	return fn
}

// EmitNumCPUs defines @pal_num_cpus using GetSystemInfo.
// Reads dwNumberOfProcessors from SYSTEM_INFO struct (offset 32 on x64).
func (p *WindowsPAL) EmitNumCPUs(module *ir.Module) *ir.Func {
	// declare void @GetSystemInfo(i8*) nounwind
	getSystemInfo := module.NewFunc("GetSystemInfo", irtypes.Void,
		ir.NewParam("lpSystemInfo", irtypes.I8Ptr))
	getSystemInfo.FuncAttrs = append(getSystemInfo.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_num_cpus", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Stack-allocate 48 bytes for SYSTEM_INFO
	sysInfoBuf := entry.NewAlloca(irtypes.NewArray(48, irtypes.I8))
	sysInfoPtr := entry.NewBitCast(sysInfoBuf, irtypes.I8Ptr)

	// GetSystemInfo(&sysInfo)
	entry.NewCall(getSystemInfo, sysInfoPtr)

	// Read dwNumberOfProcessors at byte offset 32 (i32)
	numCPUPtr := entry.NewGetElementPtr(irtypes.I8, sysInfoPtr,
		constant.NewInt(irtypes.I64, 32))
	numCPUPtrI32 := entry.NewBitCast(numCPUPtr, irtypes.NewPointer(irtypes.I32))
	numCPU := entry.NewLoad(irtypes.I32, numCPUPtrI32)

	// Clamp to at least 1
	isLess := entry.NewICmp(enum.IPredSLT, numCPU, constant.NewInt(irtypes.I32, 1))
	clamped := entry.NewSelect(isLess, constant.NewInt(irtypes.I32, 1), numCPU)
	entry.NewRet(clamped)
	return fn
}

// --- Windows directory listing (Phase D) ---
//
// Windows uses FindFirstFileA/FindNextFileA/FindClose.
// State struct layout (heap-allocated, returned as i8* handle):
//   offset 0:  i8*  hFind (HANDLE, 8 bytes)
//   offset 8:  i32  first (flag: 1 = first entry already in findData, 0 = need FindNextFileA)
//   offset 12: i32  padding
//   offset 16: [328 x i8] WIN32_FIND_DATAA
// cFileName is at offset 44 within WIN32_FIND_DATAA → offset 60 within state struct.
// Total state struct size: 344 bytes.

const winDirStateSize = 344
const winDirFindDataOffset = 16
const winDirCFileNameOffset = 60 // 16 (findData start) + 44 (cFileName within WIN32_FIND_DATAA)

// EmitDirOpen declares Win32 FindFirstFileA and defines @pal_dir_open.
// Appends "\\*" to the path, calls FindFirstFileA, allocates state struct.
// Returns i8* handle (state struct) or null on error.
func (p *WindowsPAL) EmitDirOpen(module *ir.Module) *ir.Func {
	// declare i8* @FindFirstFileA(i8*, i8*) nounwind
	findFirst := module.NewFunc("FindFirstFileA", irtypes.I8Ptr,
		ir.NewParam("lpFileName", irtypes.I8Ptr),
		ir.NewParam("lpFindFileData", irtypes.I8Ptr))
	findFirst.FuncAttrs = append(findFirst.FuncAttrs, enum.FuncAttrNoUnwind)

	palAlloc := lookupFunc(module, "pal_alloc")
	palFree := lookupFunc(module, "pal_free")
	strlenFn := lookupFunc(module, "strlen")
	if strlenFn == nil {
		strlenFn = module.NewFunc("strlen", irtypes.I64,
			ir.NewParam("s", irtypes.I8Ptr))
		strlenFn.FuncAttrs = append(strlenFn.FuncAttrs, enum.FuncAttrNoUnwind)
	}

	fn := module.NewFunc("pal_dir_open", irtypes.I8Ptr,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Build "path\\*" pattern: allocate len+3 bytes (path + "\\*" + null)
	pathLen := entry.NewCall(strlenFn, fn.Params[0])
	patternSize := entry.NewAdd(pathLen, constant.NewInt(irtypes.I64, 3))
	pattern := entry.NewCall(palAlloc, patternSize)

	// memcpy path into pattern buffer
	memcpyFn := lookupFunc(module, "memcpy")
	if memcpyFn == nil {
		memcpyFn = module.NewFunc("memcpy", irtypes.I8Ptr,
			ir.NewParam("dst", irtypes.I8Ptr),
			ir.NewParam("src", irtypes.I8Ptr),
			ir.NewParam("n", irtypes.I64))
		memcpyFn.FuncAttrs = append(memcpyFn.FuncAttrs, enum.FuncAttrNoUnwind)
	}
	entry.NewCall(memcpyFn, pattern, fn.Params[0], pathLen)

	// Append "\\*\0"
	slashPos := entry.NewGetElementPtr(irtypes.I8, pattern, pathLen)
	entry.NewStore(constant.NewInt(irtypes.I8, '\\'), slashPos)
	starPos := entry.NewGetElementPtr(irtypes.I8, pattern,
		entry.NewAdd(pathLen, constant.NewInt(irtypes.I64, 1)))
	entry.NewStore(constant.NewInt(irtypes.I8, '*'), starPos)
	nullPos := entry.NewGetElementPtr(irtypes.I8, pattern,
		entry.NewAdd(pathLen, constant.NewInt(irtypes.I64, 2)))
	entry.NewStore(constant.NewInt(irtypes.I8, 0), nullPos)

	// Allocate state struct
	state := entry.NewCall(palAlloc, constant.NewInt(irtypes.I64, winDirStateSize))

	// FindFirstFileA(pattern, &state[findDataOffset])
	findDataPtr := entry.NewGetElementPtr(irtypes.I8, state,
		constant.NewInt(irtypes.I64, winDirFindDataOffset))
	hFind := entry.NewCall(findFirst, pattern, findDataPtr)

	// Free pattern
	entry.NewCall(palFree, pattern)

	// Check INVALID_HANDLE_VALUE (-1 as i8*)
	hFindInt := entry.NewPtrToInt(hFind, irtypes.I64)
	isInvalid := entry.NewICmp(enum.IPredEQ, hFindInt, constant.NewInt(irtypes.I64, -1))
	okBlk := fn.NewBlock(".ok")
	failBlk := fn.NewBlock(".fail")
	entry.NewCondBr(isInvalid, failBlk, okBlk)

	// Failure: free state, return null
	failBlk.NewCall(palFree, state)
	failBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	// Success: store hFind at offset 0, set first=1 at offset 8
	hFindPtr := okBlk.NewBitCast(state, irtypes.NewPointer(irtypes.I8Ptr))
	okBlk.NewStore(hFind, hFindPtr)
	firstFlagPtr := okBlk.NewGetElementPtr(irtypes.I8, state, constant.NewInt(irtypes.I64, 8))
	firstFlagPtrI32 := okBlk.NewBitCast(firstFlagPtr, irtypes.NewPointer(irtypes.I32))
	okBlk.NewStore(constant.NewInt(irtypes.I32, 1), firstFlagPtrI32)

	okBlk.NewRet(state)
	return fn
}

// EmitDirNextName defines @pal_dir_next_name for Windows.
// If first flag is set, returns cFileName from current findData, clears first.
// Otherwise calls FindNextFileA; returns cFileName or null when done.
func (p *WindowsPAL) EmitDirNextName(module *ir.Module) *ir.Func {
	// declare i32 @FindNextFileA(i8*, i8*) nounwind
	findNext := module.NewFunc("FindNextFileA", irtypes.I32,
		ir.NewParam("hFindFile", irtypes.I8Ptr),
		ir.NewParam("lpFindFileData", irtypes.I8Ptr))
	findNext.FuncAttrs = append(findNext.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_dir_next_name", irtypes.I8Ptr,
		ir.NewParam("handle", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Load first flag from offset 8
	firstFlagPtr := entry.NewGetElementPtr(irtypes.I8, fn.Params[0],
		constant.NewInt(irtypes.I64, 8))
	firstFlagPtrI32 := entry.NewBitCast(firstFlagPtr, irtypes.NewPointer(irtypes.I32))
	firstFlag := entry.NewLoad(irtypes.I32, firstFlagPtrI32)
	isFirst := entry.NewICmp(enum.IPredNE, firstFlag, constant.NewInt(irtypes.I32, 0))

	firstBlk := fn.NewBlock(".first")
	nextBlk := fn.NewBlock(".next")
	entry.NewCondBr(isFirst, firstBlk, nextBlk)

	// First entry: clear flag, return cFileName pointer
	firstBlk.NewStore(constant.NewInt(irtypes.I32, 0), firstFlagPtrI32)
	firstName := firstBlk.NewGetElementPtr(irtypes.I8, fn.Params[0],
		constant.NewInt(irtypes.I64, winDirCFileNameOffset))
	firstBlk.NewRet(firstName)

	// Subsequent entries: call FindNextFileA
	hFindPtr := nextBlk.NewBitCast(fn.Params[0], irtypes.NewPointer(irtypes.I8Ptr))
	hFind := nextBlk.NewLoad(irtypes.I8Ptr, hFindPtr)
	findDataPtr := nextBlk.NewGetElementPtr(irtypes.I8, fn.Params[0],
		constant.NewInt(irtypes.I64, winDirFindDataOffset))
	rc := nextBlk.NewCall(findNext, hFind, findDataPtr)

	isZero := nextBlk.NewICmp(enum.IPredEQ, rc, constant.NewInt(irtypes.I32, 0))
	gotBlk := fn.NewBlock(".got")
	doneBlk := fn.NewBlock(".done")
	nextBlk.NewCondBr(isZero, doneBlk, gotBlk)

	// Got entry: return cFileName
	gotName := gotBlk.NewGetElementPtr(irtypes.I8, fn.Params[0],
		constant.NewInt(irtypes.I64, winDirCFileNameOffset))
	gotBlk.NewRet(gotName)

	// Done: return null
	doneBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	return fn
}

// EmitDirClose defines @pal_dir_close for Windows.
// Calls FindClose on the stored HANDLE, then frees the state struct.
func (p *WindowsPAL) EmitDirClose(module *ir.Module) *ir.Func {
	// declare i32 @FindClose(i8*) nounwind
	findClose := module.NewFunc("FindClose", irtypes.I32,
		ir.NewParam("hFindFile", irtypes.I8Ptr))
	findClose.FuncAttrs = append(findClose.FuncAttrs, enum.FuncAttrNoUnwind)

	palFree := lookupFunc(module, "pal_free")

	fn := module.NewFunc("pal_dir_close", irtypes.Void,
		ir.NewParam("handle", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Load HANDLE from offset 0
	hFindPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(irtypes.I8Ptr))
	hFind := entry.NewLoad(irtypes.I8Ptr, hFindPtr)
	entry.NewCall(findClose, hFind)

	// Free state struct
	entry.NewCall(palFree, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// EmitGetEnv declares UCRT @getenv and defines @pal_getenv.
// Signature: @pal_getenv(i8* name) → i8* (value or null)
func (p *WindowsPAL) EmitGetEnv(module *ir.Module) *ir.Func {
	getenvFn := module.NewFunc("getenv", irtypes.I8Ptr,
		ir.NewParam("name", irtypes.I8Ptr))
	getenvFn.FuncAttrs = append(getenvFn.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_getenv", irtypes.I8Ptr,
		ir.NewParam("name", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	result := entry.NewCall(getenvFn, fn.Params[0])
	entry.NewRet(result)
	return fn
}

// EmitSetEnv declares UCRT @_putenv_s and defines @pal_setenv.
func (p *WindowsPAL) EmitSetEnv(module *ir.Module) *ir.Func {
	// Windows _putenv_s(name, value) returns 0 on success, errno on error
	putenvsFn := module.NewFunc("_putenv_s", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr),
		ir.NewParam("value", irtypes.I8Ptr))
	putenvsFn.FuncAttrs = append(putenvsFn.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_setenv", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr),
		ir.NewParam("value", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	result := entry.NewCall(putenvsFn, fn.Params[0], fn.Params[1])
	// _putenv_s returns 0 on success; convert non-zero to -1
	isErr := entry.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
	entry.NewRet(entry.NewSelect(isErr, constant.NewInt(irtypes.I32, -1), constant.NewInt(irtypes.I32, 0)))
	return fn
}

// EmitUnsetEnv uses _putenv_s with empty string to unset on Windows.
func (p *WindowsPAL) EmitUnsetEnv(module *ir.Module) *ir.Func {
	// On Windows, _putenv_s(name, "") removes the variable
	putenvsFn := lookupFunc(module, "_putenv_s")
	if putenvsFn == nil {
		putenvsFn = module.NewFunc("_putenv_s", irtypes.I32,
			ir.NewParam("name", irtypes.I8Ptr),
			ir.NewParam("value", irtypes.I8Ptr))
		putenvsFn.FuncAttrs = append(putenvsFn.FuncAttrs, enum.FuncAttrNoUnwind)
	}

	fn := module.NewFunc("pal_unsetenv", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	// Pass empty string as value to remove the variable
	emptyStr := module.NewGlobalDef(".str.empty_env", constant.NewCharArrayFromString("\x00"))
	emptyPtr := entry.NewGetElementPtr(irtypes.NewArray(1, irtypes.I8), emptyStr, constant.NewInt(irtypes.I64, 0), constant.NewInt(irtypes.I64, 0))
	result := entry.NewCall(putenvsFn, fn.Params[0], emptyPtr)
	isErr := entry.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
	entry.NewRet(entry.NewSelect(isErr, constant.NewInt(irtypes.I32, -1), constant.NewInt(irtypes.I32, 0)))
	return fn
}

// EmitChdir declares UCRT @_chdir and defines @pal_chdir.
func (p *WindowsPAL) EmitChdir(module *ir.Module) *ir.Func {
	chdirFn := module.NewFunc("_chdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	chdirFn.FuncAttrs = append(chdirFn.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_chdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	result := entry.NewCall(chdirFn, fn.Params[0])
	entry.NewRet(result)
	return fn
}

// EmitExecute returns -1 stub (not yet implemented on Windows).
func (p *WindowsPAL) EmitExecute(module *ir.Module) *ir.Func {
	return emitStubExecute(module)
}

// EmitGetCwd declares UCRT @_getcwd and defines @pal_getcwd.
// Signature: @pal_getcwd(i8* buf, i64 len) → i8* (buf or null)
func (p *WindowsPAL) EmitGetCwd(module *ir.Module) *ir.Func {
	// Windows _getcwd takes (char* buf, int maxlen) — i32 for length
	getcwdFn := module.NewFunc("_getcwd", irtypes.I8Ptr,
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("maxlen", irtypes.I32))
	getcwdFn.FuncAttrs = append(getcwdFn.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_getcwd", irtypes.I8Ptr,
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	// Truncate i64 len to i32 for Windows API
	lenI32 := entry.NewTrunc(fn.Params[1], irtypes.I32)
	result := entry.NewCall(getcwdFn, fn.Params[0], lenI32)
	entry.NewRet(result)
	return fn
}
