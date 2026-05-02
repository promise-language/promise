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
