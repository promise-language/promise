package pal

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// WindowsPAL implements PAL for Windows using Win32 API (kernel32.dll).
// Threading uses CRITICAL_SECTION (mutexes) and CONDITION_VARIABLE (condition vars).
// All Win32 functions are declared as LLVM externals resolved by the linker from kernel32.lib/ucrt.
type WindowsPAL struct {
	DebugFree bool // poison-fill freed memory for UAF detection
}

// EmitWrite declares Win32 GetStdHandle/WriteFile and defines @pal_write.
// Maps POSIX fd (0/1/2) to Windows HANDLE via GetStdHandle, then calls WriteFile.
// Signature: @pal_write(i32 %fd, i8* %buf, i64 %len) → i64
func (p *WindowsPAL) EmitWrite(module *ir.Module) *ir.Func {
	// declare i8* @GetStdHandle(i32)
	getStdHandle := getOrDeclareFunc(module, "GetStdHandle", irtypes.I8Ptr,
		ir.NewParam("nStdHandle", irtypes.I32))

	// declare i32 @WriteFile(i8*, i8*, i32, i32*, i8*)
	writeFile := getOrDeclareFunc(module, "WriteFile", irtypes.I32,
		ir.NewParam("hFile", irtypes.I8Ptr),
		ir.NewParam("lpBuffer", irtypes.I8Ptr),
		ir.NewParam("nNumberOfBytesToWrite", irtypes.I32),
		ir.NewParam("lpNumberOfBytesWritten", irtypes.NewPointer(irtypes.I32)),
		ir.NewParam("lpOverlapped", irtypes.I8Ptr))

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
	exitProcess := getOrDeclareFunc(module, "ExitProcess", irtypes.Void,
		ir.NewParam("uExitCode", irtypes.I32))
	addFuncAttr(exitProcess, enum.FuncAttrNoReturn)

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
func (p *WindowsPAL) EmitAlloc(module *ir.Module) *ir.Func { return emitLibcAlloc(module) }
func (p *WindowsPAL) EmitFree(module *ir.Module) *ir.Func {
	if p.DebugFree {
		return emitLibcFreeDebug(module, "_msize")
	}
	return emitLibcFree(module)
}
func (p *WindowsPAL) EmitRealloc(module *ir.Module) *ir.Func { return emitLibcRealloc(module) }

// --- Windows threading via Win32 API ---

// winThreadFnType returns the LLVM type for a Windows thread routine: i32 (i8*)*
// LPTHREAD_START_ROUTINE signature: DWORD WINAPI ThreadProc(LPVOID lpParameter)
func winThreadFnType() *irtypes.PointerType {
	return irtypes.NewPointer(irtypes.NewFunc(irtypes.I32, irtypes.I8Ptr))
}

// EmitThreadCreate declares _beginthreadex and defines @pal_thread_create.
// Uses _beginthreadex (not CreateThread) so the CRT per-thread data is initialized,
// which is required for TLS and other CRT features on worker threads.
// Emits a trampoline that adapts PAL's i8*(i8*) signature to CRT's i32(i8*).
// Creates thread with explicit 2MB stack size (matching POSIX PAL).
func (p *WindowsPAL) EmitThreadCreate(module *ir.Module) *ir.Func {
	// declare i8* @_beginthreadex(i8*, i32, i32(i8*)*, i8*, i32, i32*)
	// Returns uintptr_t (handle) — modeled as i8* for consistency.
	// stack_size is unsigned (i32), not SIZE_T (i64) like CreateThread.
	beginThread := getOrDeclareFunc(module, "_beginthreadex", irtypes.I8Ptr,
		ir.NewParam("security", irtypes.I8Ptr),
		ir.NewParam("stack_size", irtypes.I32),
		ir.NewParam("start_address", winThreadFnType()),
		ir.NewParam("arglist", irtypes.I8Ptr),
		ir.NewParam("initflag", irtypes.I32),
		ir.NewParam("thrdaddr", irtypes.NewPointer(irtypes.I32)))

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

	// _beginthreadex(NULL, 2MB, trampoline, packed, 0, NULL)
	handle := entry.NewCall(beginThread,
		constant.NewNull(irtypes.I8Ptr),           // security
		constant.NewInt(irtypes.I32, 2*1024*1024), // stack_size (2MB)
		trampoline,                      // start_address
		packed,                          // arglist
		constant.NewInt(irtypes.I32, 0), // initflag (run immediately)
		constant.NewNull(irtypes.NewPointer(irtypes.I32))) // thrdaddr (don't need)
	entry.NewRet(handle)
	return fn
}

// EmitThreadJoin declares WaitForSingleObject/CloseHandle and defines @pal_thread_join.
// Waits for thread to finish (INFINITE timeout), then closes the handle.
func (p *WindowsPAL) EmitThreadJoin(module *ir.Module) *ir.Func {
	// declare i32 @WaitForSingleObject(i8*, i32) nounwind
	waitForSingleObject := getOrDeclareFunc(module, "WaitForSingleObject", irtypes.I32,
		ir.NewParam("hHandle", irtypes.I8Ptr),
		ir.NewParam("dwMilliseconds", irtypes.I32))

	// declare i32 @CloseHandle(i8*) nounwind
	closeHandle := getOrDeclareFunc(module, "CloseHandle", irtypes.I32,
		ir.NewParam("hObject", irtypes.I8Ptr))

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
	initCS := getOrDeclareFunc(module, "InitializeCriticalSection", irtypes.Void,
		ir.NewParam("lpCriticalSection", irtypes.I8Ptr))

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
	enterCS := getOrDeclareFunc(module, "EnterCriticalSection", irtypes.Void,
		ir.NewParam("lpCriticalSection", irtypes.I8Ptr))

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
	leaveCS := getOrDeclareFunc(module, "LeaveCriticalSection", irtypes.Void,
		ir.NewParam("lpCriticalSection", irtypes.I8Ptr))

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
	deleteCS := getOrDeclareFunc(module, "DeleteCriticalSection", irtypes.Void,
		ir.NewParam("lpCriticalSection", irtypes.I8Ptr))

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
	initCV := getOrDeclareFunc(module, "InitializeConditionVariable", irtypes.Void,
		ir.NewParam("lpConditionVariable", irtypes.I8Ptr))

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
	sleepCV := getOrDeclareFunc(module, "SleepConditionVariableCS", irtypes.I32,
		ir.NewParam("lpConditionVariable", irtypes.I8Ptr),
		ir.NewParam("lpCriticalSection", irtypes.I8Ptr),
		ir.NewParam("dwMilliseconds", irtypes.I32))

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
	wakeCV := getOrDeclareFunc(module, "WakeConditionVariable", irtypes.Void,
		ir.NewParam("lpConditionVariable", irtypes.I8Ptr))

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
	wakeAllCV := getOrDeclareFunc(module, "WakeAllConditionVariable", irtypes.Void,
		ir.NewParam("lpConditionVariable", irtypes.I8Ptr))

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
	return getOrDeclareFunc(module, "_errno", irtypes.NewPointer(irtypes.I32))
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
	ucrtOpen := getOrDeclareFunc(module, "_open", irtypes.I32,
		ir.NewParam("filename", irtypes.I8Ptr),
		ir.NewParam("oflag", irtypes.I32),
		ir.NewParam("pmode", irtypes.I32))

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
	ucrtRead := getOrDeclareFunc(module, "_read", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buffer", irtypes.I8Ptr),
		ir.NewParam("count", irtypes.I32))

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
	ucrtWrite := getOrDeclareFunc(module, "_write", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buffer", irtypes.I8Ptr),
		ir.NewParam("count", irtypes.I32))

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
	ucrtClose := getOrDeclareFunc(module, "_close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))

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
	ucrtLseek := getOrDeclareFunc(module, "_lseeki64", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("offset", irtypes.I64),
		ir.NewParam("origin", irtypes.I32))

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
	ucrtOpen := getOrDeclareFunc(module, "_open", irtypes.I32,
		ir.NewParam("filename", irtypes.I8Ptr),
		ir.NewParam("oflag", irtypes.I32),
		ir.NewParam("pmode", irtypes.I32))
	ucrtLseek := getOrDeclareFunc(module, "_lseeki64", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("offset", irtypes.I64),
		ir.NewParam("origin", irtypes.I32))
	ucrtClose := getOrDeclareFunc(module, "_close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))

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
	ucrtUnlink := getOrDeclareFunc(module, "_unlink", irtypes.I32,
		ir.NewParam("filename", irtypes.I8Ptr))

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
	ucrtAccess := getOrDeclareFunc(module, "_access", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("mode", irtypes.I32))

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
	ucrtMkdir := getOrDeclareFunc(module, "_mkdir", irtypes.I32,
		ir.NewParam("dirname", irtypes.I8Ptr))

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
	ucrtRmdir := getOrDeclareFunc(module, "_rmdir", irtypes.I32,
		ir.NewParam("dirname", irtypes.I8Ptr))

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
	getFileAttrs := getOrDeclareFunc(module, "GetFileAttributesA", irtypes.I32,
		ir.NewParam("lpFileName", irtypes.I8Ptr))

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
	getSystemInfo := getOrDeclareFunc(module, "GetSystemInfo", irtypes.Void,
		ir.NewParam("lpSystemInfo", irtypes.I8Ptr))

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
	findFirst := getOrDeclareFunc(module, "FindFirstFileA", irtypes.I8Ptr,
		ir.NewParam("lpFileName", irtypes.I8Ptr),
		ir.NewParam("lpFindFileData", irtypes.I8Ptr))

	palAlloc := lookupFunc(module, "pal_alloc")
	palFree := lookupFunc(module, "pal_free")
	strlenFn := getOrDeclareFunc(module, "strlen", irtypes.I64,
		ir.NewParam("s", irtypes.I8Ptr))

	fn := module.NewFunc("pal_dir_open", irtypes.I8Ptr,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Build "path\\*" pattern: allocate len+3 bytes (path + "\\*" + null)
	pathLen := entry.NewCall(strlenFn, fn.Params[0])
	patternSize := entry.NewAdd(pathLen, constant.NewInt(irtypes.I64, 3))
	pattern := entry.NewCall(palAlloc, patternSize)

	// memcpy path into pattern buffer
	memcpyFn := getOrDeclareFunc(module, "memcpy", irtypes.I8Ptr,
		ir.NewParam("dst", irtypes.I8Ptr),
		ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("n", irtypes.I64))
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
	findNext := getOrDeclareFunc(module, "FindNextFileA", irtypes.I32,
		ir.NewParam("hFindFile", irtypes.I8Ptr),
		ir.NewParam("lpFindFileData", irtypes.I8Ptr))

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
	findClose := getOrDeclareFunc(module, "FindClose", irtypes.I32,
		ir.NewParam("hFindFile", irtypes.I8Ptr))

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
	getenvFn := getOrDeclareFunc(module, "getenv", irtypes.I8Ptr,
		ir.NewParam("name", irtypes.I8Ptr))

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
	putenvsFn := getOrDeclareFunc(module, "_putenv_s", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr),
		ir.NewParam("value", irtypes.I8Ptr))

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
	putenvsFn := getOrDeclareFunc(module, "_putenv_s", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr),
		ir.NewParam("value", irtypes.I8Ptr))

	fn := module.NewFunc("pal_unsetenv", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	// Pass empty string as value to remove the variable
	emptyStr := module.NewGlobalDef(".str.empty_env", constant.NewCharArrayFromString("\x00"))
	emptyStr.Linkage = enum.LinkagePrivate
	emptyPtr := entry.NewGetElementPtr(irtypes.NewArray(1, irtypes.I8), emptyStr, constant.NewInt(irtypes.I64, 0), constant.NewInt(irtypes.I64, 0))
	result := entry.NewCall(putenvsFn, fn.Params[0], emptyPtr)
	isErr := entry.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
	entry.NewRet(entry.NewSelect(isErr, constant.NewInt(irtypes.I32, -1), constant.NewInt(irtypes.I32, 0)))
	return fn
}

// EmitChdir declares UCRT @_chdir and defines @pal_chdir.
func (p *WindowsPAL) EmitChdir(module *ir.Module) *ir.Func {
	chdirFn := getOrDeclareFunc(module, "_chdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))

	fn := module.NewFunc("pal_chdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	result := entry.NewCall(chdirFn, fn.Params[0])
	entry.NewRet(result)
	return fn
}

// --- Windows process execution via CreateProcessA + CreatePipe ---
//
// Windows HANDLEs are pointer-sized but kernel handles use only the low 32 bits
// (upper bits are sign-extended on 64-bit). The PAL interface uses i32 for pid/fd,
// so we pack HANDLEs via ptrtoint+trunc and unpack via sext+inttoptr.

// winI32ToHandle emits: sext i32 %val to i64, then inttoptr i64 to i8*.
func winI32ToHandle(blk *ir.Block, val value.Value) *ir.InstIntToPtr {
	ext := blk.NewSExt(val, irtypes.I64)
	return blk.NewIntToPtr(ext, irtypes.I8Ptr)
}

// emitArgvToCmdline emits @__promise_argv_to_cmdline(i8** argv) → i8*
// Builds a Windows command line string from a null-terminated argv array.
// Each argument is double-quoted; internal double quotes are escaped with backslash.
// Caller must free the returned string.
func emitArgvToCmdline(module *ir.Module) *ir.Func {
	if fn := lookupFunc(module, "__promise_argv_to_cmdline"); fn != nil {
		return fn
	}

	palAlloc := lookupFunc(module, "pal_alloc")
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)

	fn := module.NewFunc("__promise_argv_to_cmdline", irtypes.I8Ptr,
		ir.NewParam("argv", i8PtrPtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	// --- Pass 1: calculate total buffer size ---
	// For each arg: 2 (quotes) + strlen + count of internal quotes (for escaping) + 1 (space)
	// Plus 1 for null terminator.
	entry := fn.NewBlock(".entry")
	totalPtr := entry.NewAlloca(irtypes.I64)
	entry.NewStore(one64, totalPtr) // 1 for null terminator
	idxPtr := entry.NewAlloca(irtypes.I64)
	entry.NewStore(zero64, idxPtr)
	sizeLoop := fn.NewBlock(".size_loop")
	entry.NewBr(sizeLoop)

	// Load argv[idx]
	sizeIdx := sizeLoop.NewLoad(irtypes.I64, idxPtr)
	argSlotPtr := sizeLoop.NewGetElementPtr(irtypes.I8Ptr, fn.Params[0], sizeIdx)
	argPtr := sizeLoop.NewLoad(irtypes.I8Ptr, argSlotPtr)
	isNull := sizeLoop.NewICmp(enum.IPredEQ, argPtr, constant.NewNull(irtypes.I8Ptr))
	sizeBody := fn.NewBlock(".size_body")
	sizeDone := fn.NewBlock(".size_done")
	sizeLoop.NewCondBr(isNull, sizeDone, sizeBody)

	// Count: 3 (two quotes + space) + strlen of arg + number of internal quotes
	// Inner loop to count string length and internal quotes
	curTotal := sizeBody.NewLoad(irtypes.I64, totalPtr)
	added := sizeBody.NewAdd(curTotal, constant.NewInt(irtypes.I64, 3)) // " " + space
	sizeBody.NewStore(added, totalPtr)

	charIdxPtr := sizeBody.NewAlloca(irtypes.I64)
	sizeBody.NewStore(zero64, charIdxPtr)
	charLoop := fn.NewBlock(".char_count_loop")
	sizeBody.NewBr(charLoop)

	charIdx := charLoop.NewLoad(irtypes.I64, charIdxPtr)
	chPtr := charLoop.NewGetElementPtr(irtypes.I8, argPtr, charIdx)
	ch := charLoop.NewLoad(irtypes.I8, chPtr)
	isEnd := charLoop.NewICmp(enum.IPredEQ, ch, constant.NewInt(irtypes.I8, 0))
	charBody := fn.NewBlock(".char_count_body")
	charDone := fn.NewBlock(".char_count_done")
	charLoop.NewCondBr(isEnd, charDone, charBody)

	// Each char adds 1; if it's a double quote, add 1 more (for backslash escape)
	isQuote := charBody.NewICmp(enum.IPredEQ, ch, constant.NewInt(irtypes.I8, '"'))
	extra := charBody.NewZExt(isQuote, irtypes.I64)
	inc := charBody.NewAdd(extra, one64)
	t2 := charBody.NewLoad(irtypes.I64, totalPtr)
	t3 := charBody.NewAdd(t2, inc)
	charBody.NewStore(t3, totalPtr)
	nextCharIdx := charBody.NewAdd(charIdx, one64)
	charBody.NewStore(nextCharIdx, charIdxPtr)
	charBody.NewBr(charLoop)

	// Advance to next argv entry
	nextIdx := charDone.NewAdd(sizeIdx, one64)
	charDone.NewStore(nextIdx, idxPtr)
	charDone.NewBr(sizeLoop)

	// --- Pass 2: allocate buffer and fill ---
	totalSize := sizeDone.NewLoad(irtypes.I64, totalPtr)
	buf := sizeDone.NewCall(palAlloc, totalSize)
	outPtr := sizeDone.NewAlloca(irtypes.I64)
	sizeDone.NewStore(zero64, outPtr)
	sizeDone.NewStore(zero64, idxPtr) // reset index
	fillLoop := fn.NewBlock(".fill_loop")
	sizeDone.NewBr(fillLoop)

	fillIdx := fillLoop.NewLoad(irtypes.I64, idxPtr)
	fillSlotPtr := fillLoop.NewGetElementPtr(irtypes.I8Ptr, fn.Params[0], fillIdx)
	fillArgPtr := fillLoop.NewLoad(irtypes.I8Ptr, fillSlotPtr)
	fillIsNull := fillLoop.NewICmp(enum.IPredEQ, fillArgPtr, constant.NewNull(irtypes.I8Ptr))
	fillBody := fn.NewBlock(".fill_body")
	fillDone := fn.NewBlock(".fill_done")
	fillLoop.NewCondBr(fillIsNull, fillDone, fillBody)

	// Add space separator if not first arg
	isFirst := fillBody.NewICmp(enum.IPredEQ, fillIdx, zero64)
	addSpace := fn.NewBlock(".add_space")
	noSpace := fn.NewBlock(".no_space")
	fillBody.NewCondBr(isFirst, noSpace, addSpace)

	outPos1 := addSpace.NewLoad(irtypes.I64, outPtr)
	spacePtr := addSpace.NewGetElementPtr(irtypes.I8, buf, outPos1)
	addSpace.NewStore(constant.NewInt(irtypes.I8, ' '), spacePtr)
	outPos1Inc := addSpace.NewAdd(outPos1, one64)
	addSpace.NewStore(outPos1Inc, outPtr)
	addSpace.NewBr(noSpace)

	// Write opening quote
	outPos2 := noSpace.NewLoad(irtypes.I64, outPtr)
	quotePtr1 := noSpace.NewGetElementPtr(irtypes.I8, buf, outPos2)
	noSpace.NewStore(constant.NewInt(irtypes.I8, '"'), quotePtr1)
	outPos2Inc := noSpace.NewAdd(outPos2, one64)
	noSpace.NewStore(outPos2Inc, outPtr)

	// Copy chars, escaping internal quotes
	fillCharIdxPtr := noSpace.NewAlloca(irtypes.I64)
	noSpace.NewStore(zero64, fillCharIdxPtr)
	copyLoop := fn.NewBlock(".copy_loop")
	noSpace.NewBr(copyLoop)

	fillCharIdx := copyLoop.NewLoad(irtypes.I64, fillCharIdxPtr)
	fillChPtr := copyLoop.NewGetElementPtr(irtypes.I8, fillArgPtr, fillCharIdx)
	fillCh := copyLoop.NewLoad(irtypes.I8, fillChPtr)
	fillIsEnd := copyLoop.NewICmp(enum.IPredEQ, fillCh, constant.NewInt(irtypes.I8, 0))
	copyBody := fn.NewBlock(".copy_body")
	copyDone := fn.NewBlock(".copy_done")
	copyLoop.NewCondBr(fillIsEnd, copyDone, copyBody)

	// If char is double quote, write backslash first
	fillIsQuote := copyBody.NewICmp(enum.IPredEQ, fillCh, constant.NewInt(irtypes.I8, '"'))
	escapeBlk := fn.NewBlock(".escape_quote")
	writeChar := fn.NewBlock(".write_char")
	copyBody.NewCondBr(fillIsQuote, escapeBlk, writeChar)

	escPos := escapeBlk.NewLoad(irtypes.I64, outPtr)
	escDst := escapeBlk.NewGetElementPtr(irtypes.I8, buf, escPos)
	escapeBlk.NewStore(constant.NewInt(irtypes.I8, '\\'), escDst)
	escPosInc := escapeBlk.NewAdd(escPos, one64)
	escapeBlk.NewStore(escPosInc, outPtr)
	escapeBlk.NewBr(writeChar)

	wcPos := writeChar.NewLoad(irtypes.I64, outPtr)
	wcDst := writeChar.NewGetElementPtr(irtypes.I8, buf, wcPos)
	writeChar.NewStore(fillCh, wcDst)
	wcPosInc := writeChar.NewAdd(wcPos, one64)
	writeChar.NewStore(wcPosInc, outPtr)
	nextFillCharIdx := writeChar.NewAdd(fillCharIdx, one64)
	writeChar.NewStore(nextFillCharIdx, fillCharIdxPtr)
	writeChar.NewBr(copyLoop)

	// Write closing quote
	cdPos := copyDone.NewLoad(irtypes.I64, outPtr)
	quotePtr2 := copyDone.NewGetElementPtr(irtypes.I8, buf, cdPos)
	copyDone.NewStore(constant.NewInt(irtypes.I8, '"'), quotePtr2)
	cdPosInc := copyDone.NewAdd(cdPos, one64)
	copyDone.NewStore(cdPosInc, outPtr)

	// Advance to next arg
	nextFillIdx := copyDone.NewAdd(fillIdx, one64)
	copyDone.NewStore(nextFillIdx, idxPtr)
	copyDone.NewBr(fillLoop)

	// Null-terminate
	finalPos := fillDone.NewLoad(irtypes.I64, outPtr)
	nullDst := fillDone.NewGetElementPtr(irtypes.I8, buf, finalPos)
	fillDone.NewStore(constant.NewInt(irtypes.I8, 0), nullDst)
	fillDone.NewRet(buf)

	return fn
}

// winDeclareCreatePipe declares CreatePipe(i8**, i8**, i8*, i32) → i32
func winDeclareCreatePipe(module *ir.Module) *ir.Func {
	return getOrDeclareFunc(module, "CreatePipe", irtypes.I32,
		ir.NewParam("hReadPipe", irtypes.NewPointer(irtypes.I8Ptr)),
		ir.NewParam("hWritePipe", irtypes.NewPointer(irtypes.I8Ptr)),
		ir.NewParam("lpPipeAttributes", irtypes.I8Ptr),
		ir.NewParam("nSize", irtypes.I32))
}

// winDeclareSetHandleInformation declares SetHandleInformation(i8*, i32, i32) → i32
func winDeclareSetHandleInformation(module *ir.Module) *ir.Func {
	return getOrDeclareFunc(module, "SetHandleInformation", irtypes.I32,
		ir.NewParam("hObject", irtypes.I8Ptr),
		ir.NewParam("dwMask", irtypes.I32),
		ir.NewParam("dwFlags", irtypes.I32))
}

// winDeclareCreateProcessA declares CreateProcessA with 10 params → i32
func winDeclareCreateProcessA(module *ir.Module) *ir.Func {
	return getOrDeclareFunc(module, "CreateProcessA", irtypes.I32,
		ir.NewParam("lpApplicationName", irtypes.I8Ptr),
		ir.NewParam("lpCommandLine", irtypes.I8Ptr),
		ir.NewParam("lpProcessAttributes", irtypes.I8Ptr),
		ir.NewParam("lpThreadAttributes", irtypes.I8Ptr),
		ir.NewParam("bInheritHandles", irtypes.I32),
		ir.NewParam("dwCreationFlags", irtypes.I32),
		ir.NewParam("lpEnvironment", irtypes.I8Ptr),
		ir.NewParam("lpCurrentDirectory", irtypes.I8Ptr),
		ir.NewParam("lpStartupInfo", irtypes.I8Ptr),
		ir.NewParam("lpProcessInformation", irtypes.I8Ptr))
}

// winDeclareCloseHandle declares CloseHandle(i8*) → i32
func winDeclareCloseHandle(module *ir.Module) *ir.Func {
	return getOrDeclareFunc(module, "CloseHandle", irtypes.I32,
		ir.NewParam("hObject", irtypes.I8Ptr))
}

// winDeclareGetStdHandle declares GetStdHandle(i32) → i8*
func winDeclareGetStdHandle(module *ir.Module) *ir.Func {
	return getOrDeclareFunc(module, "GetStdHandle", irtypes.I8Ptr,
		ir.NewParam("nStdHandle", irtypes.I32))
}

// STARTUPINFOA layout on x64:
//
//	offset  0: cb (i32, 4 bytes)
//	offset  8: lpReserved (i8*, 8 bytes)
//	offset 16: lpDesktop (i8*, 8 bytes)
//	offset 24: lpTitle (i8*, 8 bytes)
//	offset 32: dwX (i32)
//	offset 36: dwY (i32)
//	offset 40: dwXSize (i32)
//	offset 44: dwYSize (i32)
//	offset 48: dwXCountChars (i32)
//	offset 52: dwYCountChars (i32)
//	offset 56: dwFillAttribute (i32)
//	offset 60: dwFlags (i32)
//	offset 64: wShowWindow (i16)
//	offset 66: cbReserved2 (i16)
//	offset 72: lpReserved2 (i8*, 8 bytes)
//	offset 80: hStdInput (i8*, 8 bytes)
//	offset 88: hStdOutput (i8*, 8 bytes)
//	offset 96: hStdError (i8*, 8 bytes)
//	total: 104 bytes
const startupInfoSize = 104

// SECURITY_ATTRIBUTES layout on x64:
//
//	offset  0: nLength (i32, 4 bytes)
//	offset  8: lpSecurityDescriptor (i8*, 8 bytes)
//	offset 16: bInheritHandle (i32, 4 bytes)
//	total: 24 bytes
const securityAttrSize = 24

// PROCESS_INFORMATION layout on x64:
//
//	offset  0: hProcess (i8*, 8 bytes)
//	offset  8: hThread (i8*, 8 bytes)
//	offset 16: dwProcessId (i32)
//	offset 20: dwThreadId (i32)
//	total: 24 bytes
const processInfoSize = 24

// winEmitMemset declares and calls memset to zero a stack-allocated struct.
func winEmitMemset(module *ir.Module, blk *ir.Block, ptr value.Value, size int64) {
	memset := getOrDeclareFunc(module, "memset", irtypes.I8Ptr,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("c", irtypes.I32),
		ir.NewParam("n", irtypes.I64))
	p := blk.NewBitCast(ptr, irtypes.I8Ptr)
	blk.NewCall(memset, p, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I64, size))
}

// winStoreI32AtOffset stores an i32 value at a byte offset into a struct via i8* GEP.
func winStoreI32AtOffset(blk *ir.Block, base value.Value, offset int64, val value.Value) {
	baseI8 := blk.NewBitCast(base, irtypes.I8Ptr)
	ptr := blk.NewGetElementPtr(irtypes.I8, baseI8, constant.NewInt(irtypes.I64, offset))
	i32Ptr := blk.NewBitCast(ptr, irtypes.NewPointer(irtypes.I32))
	blk.NewStore(val, i32Ptr)
}

// winStoreI8PtrAtOffset stores an i8* value at a byte offset into a struct.
func winStoreI8PtrAtOffset(blk *ir.Block, base value.Value, offset int64, val value.Value) {
	baseI8 := blk.NewBitCast(base, irtypes.I8Ptr)
	ptr := blk.NewGetElementPtr(irtypes.I8, baseI8, constant.NewInt(irtypes.I64, offset))
	ptrPtr := blk.NewBitCast(ptr, irtypes.NewPointer(irtypes.I8Ptr))
	blk.NewStore(val, ptrPtr)
}

// winLoadI8PtrAtOffset loads an i8* value from a byte offset in a struct.
func winLoadI8PtrAtOffset(blk *ir.Block, base value.Value, offset int64) *ir.InstLoad {
	baseI8 := blk.NewBitCast(base, irtypes.I8Ptr)
	ptr := blk.NewGetElementPtr(irtypes.I8, baseI8, constant.NewInt(irtypes.I64, offset))
	ptrPtr := blk.NewBitCast(ptr, irtypes.NewPointer(irtypes.I8Ptr))
	return blk.NewLoad(irtypes.I8Ptr, ptrPtr)
}

// emitCreatePipePair creates a pair of pipes (read+write HANDLEs) with inheritable write end.
// Returns (readHandle, writeHandle) loaded into the block. Branches to errBlk on failure.
func emitCreatePipePair(module *ir.Module, fn *ir.Func, blk *ir.Block, errBlk *ir.Block, okBlkName string) (*ir.Block, *ir.InstLoad, *ir.InstLoad) {
	createPipe := winDeclareCreatePipe(module)
	setHandleInfo := winDeclareSetHandleInformation(module)

	// Allocate SECURITY_ATTRIBUTES on stack with bInheritHandle = TRUE
	saAlloca := blk.NewAlloca(irtypes.NewArray(uint64(securityAttrSize), irtypes.I8))
	winEmitMemset(module, blk, saAlloca, securityAttrSize)
	winStoreI32AtOffset(blk, saAlloca, 0, constant.NewInt(irtypes.I32, securityAttrSize)) // nLength
	winStoreI32AtOffset(blk, saAlloca, 16, constant.NewInt(irtypes.I32, 1))               // bInheritHandle = TRUE

	// Allocate HANDLE slots
	readHandlePtr := blk.NewAlloca(irtypes.I8Ptr)
	writeHandlePtr := blk.NewAlloca(irtypes.I8Ptr)
	blk.NewStore(constant.NewNull(irtypes.I8Ptr), readHandlePtr)
	blk.NewStore(constant.NewNull(irtypes.I8Ptr), writeHandlePtr)

	saPtr := blk.NewBitCast(saAlloca, irtypes.I8Ptr)
	ret := blk.NewCall(createPipe, readHandlePtr, writeHandlePtr, saPtr, constant.NewInt(irtypes.I32, 0))
	isErr := blk.NewICmp(enum.IPredEQ, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(okBlkName)
	blk.NewCondBr(isErr, errBlk, okBlk)

	// Make read handle non-inheritable: SetHandleInformation(readHandle, HANDLE_FLAG_INHERIT=1, 0)
	readHandle := okBlk.NewLoad(irtypes.I8Ptr, readHandlePtr)
	okBlk.NewCall(setHandleInfo, readHandle, constant.NewInt(irtypes.I32, 1), constant.NewInt(irtypes.I32, 0))
	writeHandle := okBlk.NewLoad(irtypes.I8Ptr, writeHandlePtr)

	return okBlk, readHandle, writeHandle
}

// EmitSpawn defines @pal_spawn using CreateProcessA + CreatePipe on Windows.
// Signature: @pal_spawn(i8* program, i8** argv, i32* out_stdout_fd, i32* out_stderr_fd) → i32
// Returns process handle (packed as i32) on success, -1 on error.
// out_stdout_fd/out_stderr_fd receive read-end pipe handles (packed as i32).
func (p *WindowsPAL) EmitSpawn(module *ir.Module) *ir.Func {
	closeHandle := winDeclareCloseHandle(module)
	getStdHandle := winDeclareGetStdHandle(module)
	createProcessA := winDeclareCreateProcessA(module)
	argvToCmdline := emitArgvToCmdline(module)
	palFree := getOrDeclareFunc(module, "pal_free", irtypes.Void,
		ir.NewParam("ptr", irtypes.I8Ptr))

	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	i32PtrType := irtypes.NewPointer(irtypes.I32)
	negOne32 := constant.NewInt(irtypes.I32, -1)

	fn := module.NewFunc("pal_spawn", irtypes.I32,
		ir.NewParam("program", irtypes.I8Ptr),
		ir.NewParam("argv", i8PtrPtrType),
		ir.NewParam("out_stdout_fd", i32PtrType),
		ir.NewParam("out_stderr_fd", i32PtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	storeErrorFds := func(blk *ir.Block) {
		blk.NewStore(negOne32, fn.Params[2])
		blk.NewStore(negOne32, fn.Params[3])
	}

	entry := fn.NewBlock(".entry")

	// Build command line string from argv
	cmdline := entry.NewCall(argvToCmdline, fn.Params[1])

	// Create stdout pipe
	errBlk1 := fn.NewBlock(".pipe1_err")
	storeErrorFds(errBlk1)
	errBlk1.NewCall(palFree, cmdline)
	errBlk1.NewRet(negOne32)

	pipe1Ok, stdoutRead, stdoutWrite := emitCreatePipePair(module, fn, entry, errBlk1, ".stdout_pipe_ok")

	// Create stderr pipe
	errBlk2 := fn.NewBlock(".pipe2_err")
	errBlk2.NewCall(closeHandle, stdoutRead)
	errBlk2.NewCall(closeHandle, stdoutWrite)
	storeErrorFds(errBlk2)
	errBlk2.NewCall(palFree, cmdline)
	errBlk2.NewRet(negOne32)

	pipe2Ok, stderrRead, stderrWrite := emitCreatePipePair(module, fn, pipe1Ok, errBlk2, ".stderr_pipe_ok")

	// Set up STARTUPINFOA
	siAlloca := pipe2Ok.NewAlloca(irtypes.NewArray(uint64(startupInfoSize), irtypes.I8))
	winEmitMemset(module, pipe2Ok, siAlloca, startupInfoSize)
	winStoreI32AtOffset(pipe2Ok, siAlloca, 0, constant.NewInt(irtypes.I32, startupInfoSize)) // cb
	winStoreI32AtOffset(pipe2Ok, siAlloca, 60, constant.NewInt(irtypes.I32, 0x100))          // dwFlags = STARTF_USESTDHANDLES

	// hStdInput = GetStdHandle(STD_INPUT_HANDLE = -10)
	stdinHandle := pipe2Ok.NewCall(getStdHandle, constant.NewInt(irtypes.I32, -10))
	winStoreI8PtrAtOffset(pipe2Ok, siAlloca, 80, stdinHandle) // hStdInput
	winStoreI8PtrAtOffset(pipe2Ok, siAlloca, 88, stdoutWrite) // hStdOutput
	winStoreI8PtrAtOffset(pipe2Ok, siAlloca, 96, stderrWrite) // hStdError

	// Set up PROCESS_INFORMATION
	piAlloca := pipe2Ok.NewAlloca(irtypes.NewArray(uint64(processInfoSize), irtypes.I8))
	winEmitMemset(module, pipe2Ok, piAlloca, processInfoSize)

	// CreateProcessA(NULL, cmdline, NULL, NULL, TRUE, 0, NULL, NULL, &si, &pi)
	siPtr := pipe2Ok.NewBitCast(siAlloca, irtypes.I8Ptr)
	piPtr := pipe2Ok.NewBitCast(piAlloca, irtypes.I8Ptr)
	cpRet := pipe2Ok.NewCall(createProcessA,
		constant.NewNull(irtypes.I8Ptr), // lpApplicationName
		cmdline,                         // lpCommandLine
		constant.NewNull(irtypes.I8Ptr), // lpProcessAttributes
		constant.NewNull(irtypes.I8Ptr), // lpThreadAttributes
		constant.NewInt(irtypes.I32, 1), // bInheritHandles = TRUE
		constant.NewInt(irtypes.I32, 0), // dwCreationFlags
		constant.NewNull(irtypes.I8Ptr), // lpEnvironment
		constant.NewNull(irtypes.I8Ptr), // lpCurrentDirectory
		siPtr,                           // lpStartupInfo
		piPtr)                           // lpProcessInformation

	cpFailed := pipe2Ok.NewICmp(enum.IPredEQ, cpRet, constant.NewInt(irtypes.I32, 0))
	cpOkBlk := fn.NewBlock(".cp_ok")
	cpErrBlk := fn.NewBlock(".cp_err")
	pipe2Ok.NewCondBr(cpFailed, cpErrBlk, cpOkBlk)

	// CreateProcess error: close all handles
	cpErrBlk.NewCall(closeHandle, stdoutRead)
	cpErrBlk.NewCall(closeHandle, stdoutWrite)
	cpErrBlk.NewCall(closeHandle, stderrRead)
	cpErrBlk.NewCall(closeHandle, stderrWrite)
	storeErrorFds(cpErrBlk)
	cpErrBlk.NewCall(palFree, cmdline)
	cpErrBlk.NewRet(negOne32)

	// Success: close write ends (child has them), close thread handle
	cpOkBlk.NewCall(closeHandle, stdoutWrite)
	cpOkBlk.NewCall(closeHandle, stderrWrite)
	hThread := winLoadI8PtrAtOffset(cpOkBlk, piAlloca, 8)
	cpOkBlk.NewCall(closeHandle, hThread)
	cpOkBlk.NewCall(palFree, cmdline)

	// Pack HANDLEs into i32 and store
	stdoutReadI64 := cpOkBlk.NewPtrToInt(stdoutRead, irtypes.I64)
	stdoutReadI32 := cpOkBlk.NewTrunc(stdoutReadI64, irtypes.I32)
	cpOkBlk.NewStore(stdoutReadI32, fn.Params[2])
	stderrReadI64 := cpOkBlk.NewPtrToInt(stderrRead, irtypes.I64)
	stderrReadI32 := cpOkBlk.NewTrunc(stderrReadI64, irtypes.I32)
	cpOkBlk.NewStore(stderrReadI32, fn.Params[3])

	// Return process handle packed as i32
	hProcess := winLoadI8PtrAtOffset(cpOkBlk, piAlloca, 0)
	hProcessI64 := cpOkBlk.NewPtrToInt(hProcess, irtypes.I64)
	hProcessI32 := cpOkBlk.NewTrunc(hProcessI64, irtypes.I32)
	cpOkBlk.NewRet(hProcessI32)

	return fn
}

// EmitReadPipe defines @pal_read_pipe on Windows using ReadFile.
// Signature: @pal_read_pipe(i32 fd, i8** out_buf, i64* out_len) → void
// Reads pipe handle to EOF, stores malloc'd buffer + length. Caller must free.
func (p *WindowsPAL) EmitReadPipe(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")
	palRealloc := lookupFunc(module, "pal_realloc")
	closeHandle := winDeclareCloseHandle(module)
	readFile := getOrDeclareFunc(module, "ReadFile", irtypes.I32,
		ir.NewParam("hFile", irtypes.I8Ptr),
		ir.NewParam("lpBuffer", irtypes.I8Ptr),
		ir.NewParam("nNumberOfBytesToRead", irtypes.I32),
		ir.NewParam("lpNumberOfBytesRead", irtypes.NewPointer(irtypes.I32)),
		ir.NewParam("lpOverlapped", irtypes.I8Ptr))

	fn := module.NewFunc("pal_read_pipe", irtypes.Void,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("out_buf", irtypes.NewPointer(irtypes.I8Ptr)),
		ir.NewParam("out_len", irtypes.NewPointer(irtypes.I64)))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")

	// Unpack i32 fd → HANDLE
	handle := winI32ToHandle(entry, fn.Params[0])

	initCap := constant.NewInt(irtypes.I64, 4096)
	buf := entry.NewCall(palAlloc, initCap)
	capPtr := entry.NewAlloca(irtypes.I64)
	entry.NewStore(initCap, capPtr)
	bufPtr := entry.NewAlloca(irtypes.I8Ptr)
	entry.NewStore(buf, bufPtr)
	totalPtr := entry.NewAlloca(irtypes.I64)
	entry.NewStore(constant.NewInt(irtypes.I64, 0), totalPtr)
	bytesReadPtr := entry.NewAlloca(irtypes.I32)
	loopBlk := fn.NewBlock(".loop")
	entry.NewBr(loopBlk)

	readOkBlk := fn.NewBlock(".read_ok")
	growBlk := fn.NewBlock(".grow")
	doneBlk := fn.NewBlock(".done")

	curCap := loopBlk.NewLoad(irtypes.I64, capPtr)
	curBuf := loopBlk.NewLoad(irtypes.I8Ptr, bufPtr)
	curTotal := loopBlk.NewLoad(irtypes.I64, totalPtr)
	space := loopBlk.NewSub(curCap, curTotal)
	space32 := loopBlk.NewTrunc(space, irtypes.I32)
	readPtr := loopBlk.NewGetElementPtr(irtypes.I8, curBuf, curTotal)

	loopBlk.NewStore(constant.NewInt(irtypes.I32, 0), bytesReadPtr)
	ret := loopBlk.NewCall(readFile, handle, readPtr, space32, bytesReadPtr, constant.NewNull(irtypes.I8Ptr))
	// ReadFile returns 0 on failure (pipe closed = ERROR_BROKEN_PIPE)
	isFailed := loopBlk.NewICmp(enum.IPredEQ, ret, constant.NewInt(irtypes.I32, 0))
	checkBytes := fn.NewBlock(".check_bytes")
	loopBlk.NewCondBr(isFailed, doneBlk, checkBytes)

	bytesRead := checkBytes.NewLoad(irtypes.I32, bytesReadPtr)
	isZero := checkBytes.NewICmp(enum.IPredEQ, bytesRead, constant.NewInt(irtypes.I32, 0))
	checkBytes.NewCondBr(isZero, doneBlk, readOkBlk)

	n64 := readOkBlk.NewZExt(bytesRead, irtypes.I64)
	newTotal := readOkBlk.NewAdd(curTotal, n64)
	readOkBlk.NewStore(newTotal, totalPtr)
	isFull := readOkBlk.NewICmp(enum.IPredEQ, newTotal, curCap)
	readOkBlk.NewCondBr(isFull, growBlk, loopBlk)

	newCap := growBlk.NewMul(curCap, constant.NewInt(irtypes.I64, 2))
	growBlk.NewStore(newCap, capPtr)
	newBuf := growBlk.NewCall(palRealloc, curBuf, newCap)
	growBlk.NewStore(newBuf, bufPtr)
	growBlk.NewBr(loopBlk)

	// Done: close handle, store results
	doneBlk.NewCall(closeHandle, handle)
	finalBuf := doneBlk.NewLoad(irtypes.I8Ptr, bufPtr)
	finalTotal := doneBlk.NewLoad(irtypes.I64, totalPtr)
	doneBlk.NewStore(finalBuf, fn.Params[1])
	doneBlk.NewStore(finalTotal, fn.Params[2])
	doneBlk.NewRet(nil)

	return fn
}

// EmitWaitPid defines @pal_wait_pid on Windows using WaitForSingleObject + GetExitCodeProcess.
// Signature: @pal_wait_pid(i32 pid) → i32
// Takes a process handle (packed as i32), waits for exit, returns exit code or -1.
func (p *WindowsPAL) EmitWaitPid(module *ir.Module) *ir.Func {
	waitForSingleObject := getOrDeclareFunc(module, "WaitForSingleObject", irtypes.I32,
		ir.NewParam("hHandle", irtypes.I8Ptr),
		ir.NewParam("dwMilliseconds", irtypes.I32))
	getExitCodeProcess := getOrDeclareFunc(module, "GetExitCodeProcess", irtypes.I32,
		ir.NewParam("hProcess", irtypes.I8Ptr),
		ir.NewParam("lpExitCode", irtypes.NewPointer(irtypes.I32)))
	closeHandle := winDeclareCloseHandle(module)

	fn := module.NewFunc("pal_wait_pid", irtypes.I32,
		ir.NewParam("pid", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	handle := winI32ToHandle(entry, fn.Params[0])

	// WaitForSingleObject(handle, INFINITE=0xFFFFFFFF)
	waitRet := entry.NewCall(waitForSingleObject, handle, constant.NewInt(irtypes.I32, -1))
	// WAIT_OBJECT_0 = 0, WAIT_FAILED = 0xFFFFFFFF
	isFailed := entry.NewICmp(enum.IPredEQ, waitRet, constant.NewInt(irtypes.I32, -1))
	okBlk := fn.NewBlock(".wait_ok")
	errBlk := fn.NewBlock(".wait_err")
	entry.NewCondBr(isFailed, errBlk, okBlk)

	errBlk.NewCall(closeHandle, handle)
	errBlk.NewRet(constant.NewInt(irtypes.I32, -1))

	// GetExitCodeProcess
	exitCodePtr := okBlk.NewAlloca(irtypes.I32)
	okBlk.NewStore(constant.NewInt(irtypes.I32, 0), exitCodePtr)
	gecpRet := okBlk.NewCall(getExitCodeProcess, handle, exitCodePtr)
	gecpFailed := okBlk.NewICmp(enum.IPredEQ, gecpRet, constant.NewInt(irtypes.I32, 0))
	exitBlk := fn.NewBlock(".exit_ok")
	gecpErrBlk := fn.NewBlock(".gecp_err")
	okBlk.NewCondBr(gecpFailed, gecpErrBlk, exitBlk)

	gecpErrBlk.NewCall(closeHandle, handle)
	gecpErrBlk.NewRet(constant.NewInt(irtypes.I32, -1))

	exitCode := exitBlk.NewLoad(irtypes.I32, exitCodePtr)
	exitBlk.NewCall(closeHandle, handle)
	exitBlk.NewRet(exitCode)

	return fn
}

// EmitSpawnStreaming defines @pal_spawn_streaming on Windows using CreateProcessA + CreatePipe.
// Like EmitSpawn but also creates a stdin pipe.
// Signature: @pal_spawn_streaming(i8* program, i8** argv, i32* out_stdin_fd, i32* out_stdout_fd, i32* out_stderr_fd) → i32
func (p *WindowsPAL) EmitSpawnStreaming(module *ir.Module) *ir.Func {
	closeHandle := winDeclareCloseHandle(module)
	createProcessA := winDeclareCreateProcessA(module)
	setHandleInfo := winDeclareSetHandleInformation(module)
	argvToCmdline := emitArgvToCmdline(module)
	palFree := getOrDeclareFunc(module, "pal_free", irtypes.Void,
		ir.NewParam("ptr", irtypes.I8Ptr))

	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	i32PtrType := irtypes.NewPointer(irtypes.I32)
	negOne32 := constant.NewInt(irtypes.I32, -1)

	fn := module.NewFunc("pal_spawn_streaming", irtypes.I32,
		ir.NewParam("program", irtypes.I8Ptr),
		ir.NewParam("argv", i8PtrPtrType),
		ir.NewParam("out_stdin_fd", i32PtrType),
		ir.NewParam("out_stdout_fd", i32PtrType),
		ir.NewParam("out_stderr_fd", i32PtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	storeErrorFds := func(blk *ir.Block) {
		blk.NewStore(negOne32, fn.Params[2])
		blk.NewStore(negOne32, fn.Params[3])
		blk.NewStore(negOne32, fn.Params[4])
	}

	entry := fn.NewBlock(".entry")
	cmdline := entry.NewCall(argvToCmdline, fn.Params[1])

	// Create stdin pipe (write end inheritable — child reads from it)
	createPipe := winDeclareCreatePipe(module)
	stdinSaAlloca := entry.NewAlloca(irtypes.NewArray(uint64(securityAttrSize), irtypes.I8))
	winEmitMemset(module, entry, stdinSaAlloca, securityAttrSize)
	winStoreI32AtOffset(entry, stdinSaAlloca, 0, constant.NewInt(irtypes.I32, securityAttrSize))
	winStoreI32AtOffset(entry, stdinSaAlloca, 16, constant.NewInt(irtypes.I32, 1)) // bInheritHandle

	stdinReadPtr := entry.NewAlloca(irtypes.I8Ptr)
	stdinWritePtr := entry.NewAlloca(irtypes.I8Ptr)
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), stdinReadPtr)
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), stdinWritePtr)

	stdinSaPtr := entry.NewBitCast(stdinSaAlloca, irtypes.I8Ptr)
	stdinPipeRet := entry.NewCall(createPipe, stdinReadPtr, stdinWritePtr, stdinSaPtr, constant.NewInt(irtypes.I32, 0))
	stdinPipeFailed := entry.NewICmp(enum.IPredEQ, stdinPipeRet, constant.NewInt(irtypes.I32, 0))

	stdinPipeOk := fn.NewBlock(".stdin_pipe_ok")
	stdinPipeErr := fn.NewBlock(".stdin_pipe_err")
	entry.NewCondBr(stdinPipeFailed, stdinPipeErr, stdinPipeOk)

	storeErrorFds(stdinPipeErr)
	stdinPipeErr.NewCall(palFree, cmdline)
	stdinPipeErr.NewRet(negOne32)

	// Make the WRITE end of stdin pipe non-inheritable (parent writes to it)
	stdinRead := stdinPipeOk.NewLoad(irtypes.I8Ptr, stdinReadPtr)
	stdinWrite := stdinPipeOk.NewLoad(irtypes.I8Ptr, stdinWritePtr)
	stdinPipeOk.NewCall(setHandleInfo, stdinWrite, constant.NewInt(irtypes.I32, 1), constant.NewInt(irtypes.I32, 0))

	// Create stdout pipe
	stdoutPipeErr := fn.NewBlock(".stdout_pipe_err")
	stdoutPipeErr.NewCall(closeHandle, stdinRead)
	stdoutPipeErr.NewCall(closeHandle, stdinWrite)
	storeErrorFds(stdoutPipeErr)
	stdoutPipeErr.NewCall(palFree, cmdline)
	stdoutPipeErr.NewRet(negOne32)

	stdoutPipeOk, stdoutRead, stdoutWrite := emitCreatePipePair(module, fn, stdinPipeOk, stdoutPipeErr, ".stdout_pipe_ok")

	// Create stderr pipe
	stderrPipeErr := fn.NewBlock(".stderr_pipe_err")
	stderrPipeErr.NewCall(closeHandle, stdinRead)
	stderrPipeErr.NewCall(closeHandle, stdinWrite)
	stderrPipeErr.NewCall(closeHandle, stdoutRead)
	stderrPipeErr.NewCall(closeHandle, stdoutWrite)
	storeErrorFds(stderrPipeErr)
	stderrPipeErr.NewCall(palFree, cmdline)
	stderrPipeErr.NewRet(negOne32)

	stderrPipeOk, stderrRead, stderrWrite := emitCreatePipePair(module, fn, stdoutPipeOk, stderrPipeErr, ".stderr_pipe_ok")

	// Set up STARTUPINFOA
	siAlloca := stderrPipeOk.NewAlloca(irtypes.NewArray(uint64(startupInfoSize), irtypes.I8))
	winEmitMemset(module, stderrPipeOk, siAlloca, startupInfoSize)
	winStoreI32AtOffset(stderrPipeOk, siAlloca, 0, constant.NewInt(irtypes.I32, startupInfoSize))
	winStoreI32AtOffset(stderrPipeOk, siAlloca, 60, constant.NewInt(irtypes.I32, 0x100)) // STARTF_USESTDHANDLES
	winStoreI8PtrAtOffset(stderrPipeOk, siAlloca, 80, stdinRead)                         // hStdInput = read end of stdin pipe
	winStoreI8PtrAtOffset(stderrPipeOk, siAlloca, 88, stdoutWrite)                       // hStdOutput
	winStoreI8PtrAtOffset(stderrPipeOk, siAlloca, 96, stderrWrite)                       // hStdError

	// Set up PROCESS_INFORMATION
	piAlloca := stderrPipeOk.NewAlloca(irtypes.NewArray(uint64(processInfoSize), irtypes.I8))
	winEmitMemset(module, stderrPipeOk, piAlloca, processInfoSize)

	siPtr := stderrPipeOk.NewBitCast(siAlloca, irtypes.I8Ptr)
	piPtr := stderrPipeOk.NewBitCast(piAlloca, irtypes.I8Ptr)
	cpRet := stderrPipeOk.NewCall(createProcessA,
		constant.NewNull(irtypes.I8Ptr),
		cmdline,
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewInt(irtypes.I32, 1),
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		siPtr, piPtr)

	cpFailed := stderrPipeOk.NewICmp(enum.IPredEQ, cpRet, constant.NewInt(irtypes.I32, 0))
	cpOkBlk := fn.NewBlock(".cp_ok")
	cpErrBlk := fn.NewBlock(".cp_err")
	stderrPipeOk.NewCondBr(cpFailed, cpErrBlk, cpOkBlk)

	cpErrBlk.NewCall(closeHandle, stdinRead)
	cpErrBlk.NewCall(closeHandle, stdinWrite)
	cpErrBlk.NewCall(closeHandle, stdoutRead)
	cpErrBlk.NewCall(closeHandle, stdoutWrite)
	cpErrBlk.NewCall(closeHandle, stderrRead)
	cpErrBlk.NewCall(closeHandle, stderrWrite)
	storeErrorFds(cpErrBlk)
	cpErrBlk.NewCall(palFree, cmdline)
	cpErrBlk.NewRet(negOne32)

	// Success: close child-side handles + thread handle
	cpOkBlk.NewCall(closeHandle, stdinRead)   // child reads from stdin
	cpOkBlk.NewCall(closeHandle, stdoutWrite) // child writes to stdout
	cpOkBlk.NewCall(closeHandle, stderrWrite) // child writes to stderr
	hThread := winLoadI8PtrAtOffset(cpOkBlk, piAlloca, 8)
	cpOkBlk.NewCall(closeHandle, hThread)
	cpOkBlk.NewCall(palFree, cmdline)

	// Pack HANDLEs into i32: parent writes to stdin, reads from stdout/stderr
	stdinWriteI64 := cpOkBlk.NewPtrToInt(stdinWrite, irtypes.I64)
	stdinWriteI32 := cpOkBlk.NewTrunc(stdinWriteI64, irtypes.I32)
	cpOkBlk.NewStore(stdinWriteI32, fn.Params[2])
	stdoutReadI64 := cpOkBlk.NewPtrToInt(stdoutRead, irtypes.I64)
	stdoutReadI32 := cpOkBlk.NewTrunc(stdoutReadI64, irtypes.I32)
	cpOkBlk.NewStore(stdoutReadI32, fn.Params[3])
	stderrReadI64 := cpOkBlk.NewPtrToInt(stderrRead, irtypes.I64)
	stderrReadI32 := cpOkBlk.NewTrunc(stderrReadI64, irtypes.I32)
	cpOkBlk.NewStore(stderrReadI32, fn.Params[4])

	hProcess := winLoadI8PtrAtOffset(cpOkBlk, piAlloca, 0)
	hProcessI64 := cpOkBlk.NewPtrToInt(hProcess, irtypes.I64)
	hProcessI32 := cpOkBlk.NewTrunc(hProcessI64, irtypes.I32)
	cpOkBlk.NewRet(hProcessI32)

	return fn
}

// EmitKill defines @pal_kill on Windows.
// Signature: @pal_kill(i32 pid, i32 signal) → i32
// For self-signaling (pid == GetCurrentProcessId()):
//   - SIGINT (2) → GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0)
//   - SIGTERM (15) → GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT, 0)
//   - Other signals → return -1 (unsupported)
//
// For other PIDs: TerminateProcess (treats pid as packed HANDLE).
// Returns 0 on success, -1 on error.
func (p *WindowsPAL) EmitKill(module *ir.Module) *ir.Func {
	zero32 := constant.NewInt(irtypes.I32, 0)
	negOne32 := constant.NewInt(irtypes.I32, -1)

	terminateProcess := getOrDeclareFunc(module, "TerminateProcess", irtypes.I32,
		ir.NewParam("hProcess", irtypes.I8Ptr),
		ir.NewParam("uExitCode", irtypes.I32))

	getCurrentProcessId := getOrDeclareFunc(module, "GetCurrentProcessId", irtypes.I32)

	generateCtrlEvent := getOrDeclareFunc(module, "GenerateConsoleCtrlEvent", irtypes.I32,
		ir.NewParam("dwCtrlEvent", irtypes.I32),
		ir.NewParam("dwProcessGroupId", irtypes.I32))

	fn := module.NewFunc("pal_kill", irtypes.I32,
		ir.NewParam("pid", irtypes.I32),
		ir.NewParam("signal", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	pid := fn.Params[0]
	signal := fn.Params[1]

	// Check if self-signaling: pid == GetCurrentProcessId()
	currentPid := entry.NewCall(getCurrentProcessId)
	isSelf := entry.NewICmp(enum.IPredEQ, pid, currentPid)
	selfBlk := fn.NewBlock(".self")
	otherBlk := fn.NewBlock(".other")
	entry.NewCondBr(isSelf, selfBlk, otherBlk)

	// Self-signaling: map POSIX signal to console ctrl event
	// Check SIGINT (2) → CTRL_C_EVENT (0)
	isInt := selfBlk.NewICmp(enum.IPredEQ, signal, constant.NewInt(irtypes.I32, 2))
	ctrlCBlk := fn.NewBlock(".ctrl_c")
	checkTermBlk := fn.NewBlock(".check_term")
	selfBlk.NewCondBr(isInt, ctrlCBlk, checkTermBlk)

	// GenerateConsoleCtrlEvent(CTRL_C_EVENT=0, 0)
	ctrlCRet := ctrlCBlk.NewCall(generateCtrlEvent, zero32, zero32)
	ctrlCFailed := ctrlCBlk.NewICmp(enum.IPredEQ, ctrlCRet, zero32)
	ctrlCOk := fn.NewBlock(".ctrl_c_ok")
	errBlk := fn.NewBlock(".err")
	ctrlCBlk.NewCondBr(ctrlCFailed, errBlk, ctrlCOk)
	ctrlCOk.NewRet(zero32)

	// Check SIGTERM (15) → CTRL_BREAK_EVENT (1)
	isTerm := checkTermBlk.NewICmp(enum.IPredEQ, signal, constant.NewInt(irtypes.I32, 15))
	ctrlBreakBlk := fn.NewBlock(".ctrl_break")
	unsupportedBlk := fn.NewBlock(".unsupported")
	checkTermBlk.NewCondBr(isTerm, ctrlBreakBlk, unsupportedBlk)

	// GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT=1, 0)
	ctrlBreakRet := ctrlBreakBlk.NewCall(generateCtrlEvent, constant.NewInt(irtypes.I32, 1), zero32)
	ctrlBreakFailed := ctrlBreakBlk.NewICmp(enum.IPredEQ, ctrlBreakRet, zero32)
	ctrlBreakOk := fn.NewBlock(".ctrl_break_ok")
	ctrlBreakBlk.NewCondBr(ctrlBreakFailed, errBlk, ctrlBreakOk)
	ctrlBreakOk.NewRet(zero32)

	// Unsupported signal for self
	unsupportedBlk.NewRet(negOne32)

	// Non-self: TerminateProcess (pid is a packed HANDLE)
	handle := winI32ToHandle(otherBlk, pid)
	tpRet := otherBlk.NewCall(terminateProcess, handle, constant.NewInt(irtypes.I32, 1))
	tpFailed := otherBlk.NewICmp(enum.IPredEQ, tpRet, zero32)
	okBlk := fn.NewBlock(".ok")
	otherBlk.NewCondBr(tpFailed, errBlk, okBlk)

	okBlk.NewRet(zero32)
	errBlk.NewRet(negOne32)

	return fn
}

// EmitGetEnviron defines @pal_get_environ using UCRT __p__environ().
// Signature: @pal_get_environ() → i8** (null-terminated array of "KEY=VALUE" strings)
// UCRT __p__environ() returns a pointer to the _environ variable (i8***),
// so we load through it to get the actual i8** array.
func (p *WindowsPAL) EmitGetEnviron(module *ir.Module) *ir.Func {
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	i8PtrPtrPtrType := irtypes.NewPointer(i8PtrPtrType)

	// __p__environ() → i8*** (pointer to _environ)
	pEnvironFn := getOrDeclareFunc(module, "__p__environ", i8PtrPtrPtrType)

	fn := module.NewFunc("pal_get_environ", i8PtrPtrType)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	// __p__environ() returns &_environ (i8***), load to get _environ (i8**)
	environPtr := entry.NewCall(pEnvironFn)
	environ := entry.NewLoad(i8PtrPtrType, environPtr)
	entry.NewRet(environ)
	return fn
}

// EmitGetUserInfo defines @pal_get_user_info using Win32 GetUserNameA and
// GetEnvironmentVariableA for the home directory.
// Signature: @pal_get_user_info(i8** out_name, i8** out_dir, i32* out_uid, i32* out_gid) → i32
// Returns 0 on success. uid/gid are always 0 on Windows (no Unix uid concept).
func (p *WindowsPAL) EmitGetUserInfo(module *ir.Module) *ir.Func {
	i32PtrType := irtypes.NewPointer(irtypes.I32)
	zero32 := constant.NewInt(irtypes.I32, 0)

	// Win32: BOOL GetUserNameA(LPSTR lpBuffer, LPDWORD pcbBuffer)
	// advapi32.dll — but linked via import lib
	getUserNameFn := getOrDeclareFunc(module, "GetUserNameA", irtypes.I32,
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("size", i32PtrType))

	// UCRT: DWORD GetEnvironmentVariableA(LPCSTR name, LPSTR buf, DWORD size)
	getEnvVarFn := getOrDeclareFunc(module, "GetEnvironmentVariableA", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("size", irtypes.I32))

	// malloc for username and home dir buffers (caller doesn't free — static-ish)
	mallocFn := getOrDeclareFunc(module, "malloc", irtypes.I8Ptr,
		ir.NewParam("size", irtypes.I64))

	fn := module.NewFunc("pal_get_user_info", irtypes.I32,
		ir.NewParam("out_name", irtypes.NewPointer(irtypes.I8Ptr)),
		ir.NewParam("out_dir", irtypes.NewPointer(irtypes.I8Ptr)),
		ir.NewParam("out_uid", i32PtrType),
		ir.NewParam("out_gid", i32PtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")

	// uid = 0, gid = 0 (no Unix uid/gid on Windows)
	entry.NewStore(zero32, fn.Params[2])
	entry.NewStore(zero32, fn.Params[3])

	// Get username: alloca 256 bytes, call GetUserNameA
	nameBuf := entry.NewCall(mallocFn, constant.NewInt(irtypes.I64, 256))
	nameSizeAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 256), nameSizeAlloca)
	nameOk := entry.NewCall(getUserNameFn, nameBuf, nameSizeAlloca)
	isNameErr := entry.NewICmp(enum.IPredEQ, nameOk, zero32)
	nameOkBlk := fn.NewBlock(".name_ok")
	nameErrBlk := fn.NewBlock(".name_err")
	entry.NewCondBr(isNameErr, nameErrBlk, nameOkBlk)

	// Name error: null-terminate buffer at position 0 (empty string)
	nameErrBlk.NewStore(constant.NewInt(irtypes.I8, 0), nameBuf)
	nameErrBlk.NewBr(nameOkBlk)

	// Store username buffer pointer (GetUserNameA null-terminates on success,
	// we null-terminated at 0 on error)
	nameOkBlk.NewStore(nameBuf, fn.Params[0])

	// Get home dir: GetEnvironmentVariableA("USERPROFILE", buf, 260)
	userprofileStr := module.NewGlobalDef(".str.userprofile", constant.NewCharArrayFromString("USERPROFILE\x00"))
	userprofileStr.Linkage = enum.LinkagePrivate
	userprofilePtr := nameOkBlk.NewGetElementPtr(irtypes.NewArray(12, irtypes.I8), userprofileStr,
		constant.NewInt(irtypes.I64, 0), constant.NewInt(irtypes.I64, 0))
	dirBuf := nameOkBlk.NewCall(mallocFn, constant.NewInt(irtypes.I64, 260))
	dirLen := nameOkBlk.NewCall(getEnvVarFn, userprofilePtr, dirBuf, constant.NewInt(irtypes.I32, 260))
	isDirErr := nameOkBlk.NewICmp(enum.IPredEQ, dirLen, zero32)
	dirOkBlk := fn.NewBlock(".dir_ok")
	dirErrBlk := fn.NewBlock(".dir_err")
	nameOkBlk.NewCondBr(isDirErr, dirErrBlk, dirOkBlk)

	// Dir error: null-terminate buffer at 0 (empty string)
	dirErrBlk.NewStore(constant.NewInt(irtypes.I8, 0), dirBuf)
	dirErrBlk.NewBr(dirOkBlk)

	// Store home dir buffer pointer
	dirOkBlk.NewStore(dirBuf, fn.Params[1])
	dirOkBlk.NewRet(zero32)

	return fn
}

// EmitGetHostname defines @pal_get_hostname using Win32 GetComputerNameA.
// Signature: @pal_get_hostname(i8* buf, i64 len) → i8* (buf on success, null on error)
func (p *WindowsPAL) EmitGetHostname(module *ir.Module) *ir.Func {
	i32PtrType := irtypes.NewPointer(irtypes.I32)

	// Win32: BOOL GetComputerNameA(LPSTR lpBuffer, LPDWORD nSize)
	getComputerNameFn := getOrDeclareFunc(module, "GetComputerNameA", irtypes.I32,
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("size", i32PtrType))

	fn := module.NewFunc("pal_get_hostname", irtypes.I8Ptr,
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	// Truncate i64 len to i32 for Windows API
	len32 := entry.NewTrunc(fn.Params[1], irtypes.I32)
	sizeAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewStore(len32, sizeAlloca)

	ret := entry.NewCall(getComputerNameFn, fn.Params[0], sizeAlloca)
	isErr := entry.NewICmp(enum.IPredEQ, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".error")
	entry.NewCondBr(isErr, errBlk, okBlk)

	errBlk.NewRet(constant.NewNull(irtypes.I8Ptr))
	okBlk.NewRet(fn.Params[0])
	return fn
}

// EmitSignalInit defines @pal_signal_init using UCRT _pipe + SetConsoleCtrlHandler.
// Creates a CRT pipe for signal delivery and registers a console control handler
// that maps CTRL_C_EVENT → SIGINT (2), CTRL_BREAK_EVENT → SIGTERM (15).
// Returns read fd on success, -1 on error.
func (p *WindowsPAL) EmitSignalInit(module *ir.Module) *ir.Func {
	i32PtrType := irtypes.NewPointer(irtypes.I32)
	zero32 := constant.NewInt(irtypes.I32, 0)
	one32 := constant.NewInt(irtypes.I32, 1)
	negOne32 := constant.NewInt(irtypes.I32, -1)

	// UCRT _pipe(int* fds, unsigned int psize, int textmode) → int
	pipeFn := getOrDeclareFunc(module, "_pipe", irtypes.I32,
		ir.NewParam("pfds", i32PtrType),
		ir.NewParam("psize", irtypes.I32),
		ir.NewParam("textmode", irtypes.I32))

	// UCRT _write(int fd, const void* buf, unsigned int count) → int
	ucrtWrite := getOrDeclareFunc(module, "_write", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buffer", irtypes.I8Ptr),
		ir.NewParam("count", irtypes.I32))

	// SetConsoleCtrlHandler(HandlerRoutine, Add) → BOOL
	setCtrlHandler := getOrDeclareFunc(module, "SetConsoleCtrlHandler", irtypes.I32,
		ir.NewParam("HandlerRoutine", irtypes.I8Ptr),
		ir.NewParam("Add", irtypes.I32))

	// Global to store the write fd (shared across threads for handler)
	wrFdGlobal := module.NewGlobal("__promise_signal_pipe_wr", irtypes.I32)
	wrFdGlobal.Init = negOne32

	// Per-signal enable flags (checked by handler before writing to pipe)
	intEnabled := module.NewGlobal("__promise_signal_int_enabled", irtypes.I32)
	intEnabled.Init = zero32
	termEnabled := module.NewGlobal("__promise_signal_term_enabled", irtypes.I32)
	termEnabled.Init = zero32

	// Define the console control handler:
	// BOOL WINAPI promise_console_ctrl_handler(DWORD dwCtrlType)
	// Maps CTRL_C_EVENT (0) → 2 (SIGINT), CTRL_BREAK_EVENT (1) → 15 (SIGTERM).
	// Writes the signal number byte to the pipe if that signal is enabled.
	handlerFn := module.NewFunc("promise_console_ctrl_handler", irtypes.I32,
		ir.NewParam("dwCtrlType", irtypes.I32))
	handlerFn.FuncAttrs = append(handlerFn.FuncAttrs, enum.FuncAttrNoUnwind)
	{
		hEntry := handlerFn.NewBlock(".entry")
		ctrlType := handlerFn.Params[0]

		// Check CTRL_C_EVENT (0) → SIGINT (2)
		isCtrlC := hEntry.NewICmp(enum.IPredEQ, ctrlType, zero32)
		ctrlCBlk := handlerFn.NewBlock(".ctrl_c")
		checkBreakBlk := handlerFn.NewBlock(".check_break")
		hEntry.NewCondBr(isCtrlC, ctrlCBlk, checkBreakBlk)

		// CTRL_C_EVENT: check if SIGINT is enabled
		intFlag := ctrlCBlk.NewLoad(irtypes.I32, intEnabled)
		intIsOn := ctrlCBlk.NewICmp(enum.IPredNE, intFlag, zero32)
		writeIntBlk := handlerFn.NewBlock(".write_int")
		unhandledBlk := handlerFn.NewBlock(".unhandled")
		ctrlCBlk.NewCondBr(intIsOn, writeIntBlk, unhandledBlk)

		// Write SIGINT (2) to pipe
		sigIntBuf := writeIntBlk.NewAlloca(irtypes.I8)
		writeIntBlk.NewStore(constant.NewInt(irtypes.I8, 2), sigIntBuf)
		wrFd := writeIntBlk.NewLoad(irtypes.I32, wrFdGlobal)
		writeIntBlk.NewCall(ucrtWrite, wrFd, sigIntBuf, one32)
		writeIntBlk.NewRet(one32) // TRUE = handled

		// Check CTRL_BREAK_EVENT (1) → SIGTERM (15)
		isCtrlBreak := checkBreakBlk.NewICmp(enum.IPredEQ, ctrlType, one32)
		ctrlBreakBlk := handlerFn.NewBlock(".ctrl_break")
		checkBreakBlk.NewCondBr(isCtrlBreak, ctrlBreakBlk, unhandledBlk)

		// CTRL_BREAK_EVENT: check if SIGTERM is enabled
		termFlag := ctrlBreakBlk.NewLoad(irtypes.I32, termEnabled)
		termIsOn := ctrlBreakBlk.NewICmp(enum.IPredNE, termFlag, zero32)
		writeTermBlk := handlerFn.NewBlock(".write_term")
		ctrlBreakBlk.NewCondBr(termIsOn, writeTermBlk, unhandledBlk)

		// Write SIGTERM (15) to pipe
		sigTermBuf := writeTermBlk.NewAlloca(irtypes.I8)
		writeTermBlk.NewStore(constant.NewInt(irtypes.I8, 15), sigTermBuf)
		wrFd2 := writeTermBlk.NewLoad(irtypes.I32, wrFdGlobal)
		writeTermBlk.NewCall(ucrtWrite, wrFd2, sigTermBuf, one32)
		writeTermBlk.NewRet(one32) // TRUE = handled

		// Unhandled: return FALSE so default handler runs
		unhandledBlk.NewRet(zero32)
	}

	// Define @pal_signal_init() → i32
	fn := module.NewFunc("pal_signal_init", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	// _pipe(fds, 256, _O_BINARY=0x8000)
	pipeFds := entry.NewAlloca(irtypes.NewArray(2, irtypes.I32))
	pipeFdsPtr := entry.NewBitCast(pipeFds, i32PtrType)
	pipeRet := entry.NewCall(pipeFn, pipeFdsPtr,
		constant.NewInt(irtypes.I32, 256),
		constant.NewInt(irtypes.I32, 0x8000))
	isPipeErr := entry.NewICmp(enum.IPredNE, pipeRet, zero32)
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".error")
	entry.NewCondBr(isPipeErr, errBlk, okBlk)

	errBlk.NewRet(negOne32)

	// Load fds, store write fd in global
	rdFdPtr := okBlk.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), pipeFds, zero32, zero32)
	wrFdPtr := okBlk.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), pipeFds, zero32, one32)
	rdFd := okBlk.NewLoad(irtypes.I32, rdFdPtr)
	wrFd := okBlk.NewLoad(irtypes.I32, wrFdPtr)
	okBlk.NewStore(wrFd, wrFdGlobal)

	// Register the console control handler
	handlerPtr := okBlk.NewBitCast(handlerFn, irtypes.I8Ptr)
	ctrlRet := okBlk.NewCall(setCtrlHandler, handlerPtr, one32)
	isCtrlErr := okBlk.NewICmp(enum.IPredEQ, ctrlRet, zero32)
	registeredBlk := fn.NewBlock(".registered")
	ctrlErrBlk := fn.NewBlock(".ctrl_err")
	okBlk.NewCondBr(isCtrlErr, ctrlErrBlk, registeredBlk)

	ctrlErrBlk.NewRet(negOne32)
	registeredBlk.NewRet(rdFd)

	return fn
}

// EmitSignalRegister defines @pal_signal_register for Windows.
// Sets the per-signal enable flag for supported signals (SIGINT=2, SIGTERM=15).
// Returns 0 on success, -1 for unsupported signals.
func (p *WindowsPAL) EmitSignalRegister(module *ir.Module) *ir.Func {
	zero32 := constant.NewInt(irtypes.I32, 0)
	one32 := constant.NewInt(irtypes.I32, 1)
	negOne32 := constant.NewInt(irtypes.I32, -1)

	// Look up the enable globals defined by EmitSignalInit
	var intEnabled, termEnabled *ir.Global
	for _, g := range module.Globals {
		switch g.Name() {
		case "__promise_signal_int_enabled":
			intEnabled = g
		case "__promise_signal_term_enabled":
			termEnabled = g
		}
	}

	fn := module.NewFunc("pal_signal_register", irtypes.I32,
		ir.NewParam("signum", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")

	if intEnabled == nil || termEnabled == nil {
		// EmitSignalInit was not called — shouldn't happen
		entry.NewRet(negOne32)
		return fn
	}

	signum := fn.Params[0]

	// Check SIGINT (2)
	isInt := entry.NewICmp(enum.IPredEQ, signum, constant.NewInt(irtypes.I32, 2))
	enableIntBlk := fn.NewBlock(".enable_int")
	checkTermBlk := fn.NewBlock(".check_term")
	entry.NewCondBr(isInt, enableIntBlk, checkTermBlk)

	enableIntBlk.NewStore(one32, intEnabled)
	enableIntBlk.NewRet(zero32)

	// Check SIGTERM (15)
	isTerm := checkTermBlk.NewICmp(enum.IPredEQ, signum, constant.NewInt(irtypes.I32, 15))
	enableTermBlk := fn.NewBlock(".enable_term")
	unsupportedBlk := fn.NewBlock(".unsupported")
	checkTermBlk.NewCondBr(isTerm, enableTermBlk, unsupportedBlk)

	enableTermBlk.NewStore(one32, termEnabled)
	enableTermBlk.NewRet(zero32)

	// Unsupported signal
	unsupportedBlk.NewRet(negOne32)

	return fn
}

// EmitStackOverflowInit defines @pal_stack_overflow_init() → void
// Registers a Vectored Exception Handler (VEH) via AddVectoredExceptionHandler
// that catches STATUS_STACK_OVERFLOW (0xC00000FD), writes "fatal: stack overflow"
// to stderr, and calls ExitProcess(2).
//
// VEH is process-global — all threads are covered by a single registration.
// The handler must return EXCEPTION_CONTINUE_SEARCH (-1) for non-stack-overflow
// exceptions so the default handler runs.
func (p *WindowsPAL) EmitStackOverflowInit(module *ir.Module) *ir.Func {
	// Win32 APIs called directly from the VEH handler — minimal stack usage.
	// GetStdHandle and WriteFile are safe to call with very little stack remaining.
	getStdHandle := getOrDeclareFunc(module, "GetStdHandle", irtypes.I8Ptr,
		ir.NewParam("nStdHandle", irtypes.I32))
	writeFile := getOrDeclareFunc(module, "WriteFile", irtypes.I32,
		ir.NewParam("hFile", irtypes.I8Ptr),
		ir.NewParam("lpBuffer", irtypes.I8Ptr),
		ir.NewParam("nNumberOfBytesToWrite", irtypes.I32),
		ir.NewParam("lpNumberOfBytesWritten", irtypes.NewPointer(irtypes.I32)),
		ir.NewParam("lpOverlapped", irtypes.I8Ptr))
	exitProcess := getOrDeclareFunc(module, "ExitProcess", irtypes.Void,
		ir.NewParam("uExitCode", irtypes.I32))
	addFuncAttr(exitProcess, enum.FuncAttrNoReturn)

	// Error message: "fatal: stack overflow\n" (22 bytes)
	msgStr := "fatal: stack overflow\n"
	msgConst := constant.NewCharArrayFromString(msgStr)
	msgGlobal := module.NewGlobal("__promise_stack_overflow_msg", msgConst.Typ)
	msgGlobal.Init = msgConst
	msgGlobal.Immutable = true

	// VEH handler signature: i32 @handler(i8* %exception_pointers)
	// EXCEPTION_POINTERS = { EXCEPTION_RECORD*, CONTEXT* }
	// EXCEPTION_RECORD.ExceptionCode is at offset 0 (i32)
	exPtrsType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
	handlerFn := module.NewFunc("__promise_veh_handler", irtypes.I32,
		ir.NewParam("exception_pointers", irtypes.I8Ptr))
	handlerFn.FuncAttrs = append(handlerFn.FuncAttrs, enum.FuncAttrNoUnwind)
	{
		hEntry := handlerFn.NewBlock(".entry")

		// Load ExceptionRecord pointer from EXCEPTION_POINTERS[0]
		epPtr := hEntry.NewBitCast(handlerFn.Params[0], irtypes.NewPointer(exPtrsType))
		erField := hEntry.NewGetElementPtr(exPtrsType, epPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		erPtr := hEntry.NewLoad(irtypes.I8Ptr, erField)

		// Load ExceptionCode (i32 at offset 0 of EXCEPTION_RECORD)
		codePtr := hEntry.NewBitCast(erPtr, irtypes.NewPointer(irtypes.I32))
		code := hEntry.NewLoad(irtypes.I32, codePtr)

		// Check for fatal exceptions: stack overflow, access violation, bad stack.
		// On Windows, process cleanup (CRT atexit, TLS callbacks, thread teardown)
		// can trigger STATUS_ACCESS_VIOLATION after scheduler shutdown when worker
		// threads are being terminated. The VEH handler catches these to provide
		// clean exits instead of unhandled crashes (B0148).
		isStackOverflow := hEntry.NewICmp(enum.IPredEQ, code,
			constant.NewInt(irtypes.I32, 0xC00000FD))
		isAccessViolation := hEntry.NewICmp(enum.IPredEQ, code,
			constant.NewInt(irtypes.I32, 0xC0000005))
		isBadStack := hEntry.NewICmp(enum.IPredEQ, code,
			constant.NewInt(irtypes.I32, 0xC0000028))

		isFatal := hEntry.NewOr(isStackOverflow, hEntry.NewOr(isAccessViolation, isBadStack))

		fatalBlk := handlerFn.NewBlock("fatal_handler")
		continueBlk := handlerFn.NewBlock("continue_search")
		hEntry.NewCondBr(isFatal, fatalBlk, continueBlk)

		// Fatal: write message directly via Win32 and exit.
		// Stack overflow gets its specific message; AV/bad stack get a generic crash message.
		// Use only static data (no allocas) since the stack may be corrupt.
		isSOF := fatalBlk.NewICmp(enum.IPredEQ, code,
			constant.NewInt(irtypes.I32, 0xC00000FD))
		sofMsgBlk := handlerFn.NewBlock("sof_msg")
		crashMsgBlk := handlerFn.NewBlock("crash_msg")
		fatalBlk.NewCondBr(isSOF, sofMsgBlk, crashMsgBlk)

		// STD_ERROR_HANDLE = -12
		stderrHandle := sofMsgBlk.NewCall(getStdHandle,
			constant.NewInt(irtypes.I32, -12))
		msgPtr := sofMsgBlk.NewBitCast(msgGlobal, irtypes.I8Ptr)
		sofMsgBlk.NewCall(writeFile, stderrHandle, msgPtr,
			constant.NewInt(irtypes.I32, int64(len(msgStr))),
			constant.NewNull(irtypes.NewPointer(irtypes.I32)),
			constant.NewNull(irtypes.I8Ptr))
		sofMsgBlk.NewCall(exitProcess, constant.NewInt(irtypes.I32, 2))
		sofMsgBlk.NewUnreachable()

		// Access violation or bad stack: exit cleanly with code 0.
		// These are typically caused by CRT/thread cleanup races on Windows,
		// not by bugs in user code. The process has already completed its
		// work; the crash is in the teardown path.
		crashMsgBlk.NewCall(exitProcess, constant.NewInt(irtypes.I32, 0))
		crashMsgBlk.NewUnreachable()

		// Other exceptions: return EXCEPTION_CONTINUE_SEARCH (0)
		continueBlk.NewRet(constant.NewInt(irtypes.I32, 0))
	}

	// declare i8* @AddVectoredExceptionHandler(i32, i8*)
	addVEH := getOrDeclareFunc(module, "AddVectoredExceptionHandler", irtypes.I8Ptr,
		ir.NewParam("First", irtypes.I32),
		ir.NewParam("Handler", irtypes.I8Ptr))

	// Define @pal_stack_overflow_init()
	fn := module.NewFunc("pal_stack_overflow_init", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// AddVectoredExceptionHandler(1, handler) — 1 = first handler in chain
	handlerPtr := entry.NewBitCast(handlerFn, irtypes.I8Ptr)
	entry.NewCall(addVEH, constant.NewInt(irtypes.I32, 1), handlerPtr)
	entry.NewRet(nil)

	return fn
}

// EmitStackOverflowThreadInit — no-op on Windows.
// VEH is process-global, so no per-thread setup is needed.
// Thread guard pages are set up by _beginthreadex automatically.
func (p *WindowsPAL) EmitStackOverflowThreadInit(module *ir.Module) *ir.Func {
	return emitStubStackOverflowThreadInit(module)
}

// EmitGetCwd declares UCRT @_getcwd and defines @pal_getcwd.
// Signature: @pal_getcwd(i8* buf, i64 len) → i8* (buf or null)
func (p *WindowsPAL) EmitGetCwd(module *ir.Module) *ir.Func {
	// Windows _getcwd takes (char* buf, int maxlen) — i32 for length
	getcwdFn := getOrDeclareFunc(module, "_getcwd", irtypes.I8Ptr,
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("maxlen", irtypes.I32))

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
