package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/types"
)

// defineOSBodies adds LLVM IR function bodies to OS extern declarations
// from modules/os/os.pr. Each body bridges Promise types to raw PAL syscall wrappers.
//
// Must run after compileModules() so that os module externs are declared in c.module.Funcs.
func (c *Compiler) defineOSBodies() {
	// Build lookup by LLVM function name for declarations without bodies
	irFuncByName := make(map[string]*ir.Func)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			irFuncByName[fn.Name()] = fn
		}
	}

	if fn, ok := irFuncByName["promise_os_get_env"]; ok {
		c.defineGetEnvBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_get_cwd"]; ok {
		c.defineGetCwdBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_exit"]; ok {
		c.defineExitBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_get_args"]; ok {
		c.defineArgsBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_get_executable"]; ok {
		c.defineExecutableBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_spawn"]; ok {
		c.defineSpawnBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_spawn_stdout_fd"]; ok {
		c.defineSpawnStdoutFdBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_spawn_stderr_fd"]; ok {
		c.defineSpawnStderrFdBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_read_pipe"]; ok {
		c.defineReadPipeBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_wait_pid"]; ok {
		c.defineWaitPidBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_set_env"]; ok {
		c.defineSetEnvBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_unset_env"]; ok {
		c.defineUnsetEnvBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_set_cwd"]; ok {
		c.defineSetCwdBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_spawn_streaming"]; ok {
		c.defineSpawnStreamingBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_spawn_env"]; ok {
		c.defineSpawnEnvBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_spawn_streaming_env"]; ok {
		c.defineSpawnStreamingEnvBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_spawn_stdin_fd"]; ok {
		c.defineSpawnStdinFdBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_pipe_read_bytes"]; ok {
		c.definePipeReadBytesBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_pipe_write_bytes"]; ok {
		c.definePipeWriteBytesBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_pipe_close"]; ok {
		c.definePipeCloseBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_kill"]; ok {
		c.defineKillBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_exec_replace"]; ok {
		c.defineExecReplaceBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_get_environ"]; ok {
		c.defineGetEnvironBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_get_user_name"]; ok {
		c.defineGetUserNameBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_get_user_id"]; ok {
		c.defineGetUserIdBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_get_group_id"]; ok {
		c.defineGetGroupIdBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_get_home_dir"]; ok {
		c.defineGetHomeDirBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_get_hostname"]; ok {
		c.defineGetHostnameBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_signal_init"]; ok {
		c.defineSignalInitBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_signal_register"]; ok {
		c.defineSignalRegisterBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_signal_read"]; ok {
		c.defineSignalReadBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_send_signal"]; ok {
		c.defineSendSignalBody(fn)
	}
	if fn, ok := irFuncByName["promise_os_get_pid"]; ok {
		c.defineGetPidBody(fn)
	}
}

// defineGetEnvBody: void @promise_os_get_env(i8* sret, i8* name)
// Extracts name string → cstr, calls pal_getenv.
// Returns string? — present with value if set, none if not defined.
// Non-failable — getenv doesn't fail, just returns null for absent vars.
func (c *Compiler) defineGetEnvBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	nameParam := fn.Params[1]

	// Resolve the optional type: {i1, i8*} for string?
	optType := c.resolveType(types.NewOptional(types.TypString)).(*irtypes.StructType)

	// Convert Promise string name to null-terminated C string
	cstr := c.stringToCStr(entry, nameParam)

	// Call PAL: i8* @pal_getenv(i8* name)
	result := entry.NewCall(c.palGetEnv, cstr)

	// Free temporary C string (name copy — NOT the getenv result)
	entry.NewCall(c.palFree, cstr)

	// Check if result is null (variable not found)
	isNull := entry.NewICmp(enum.IPredEQ, result, constant.NewNull(irtypes.I8Ptr))
	foundBlk := fn.NewBlock(".found")
	notFoundBlk := fn.NewBlock(".not_found")
	entry.NewCondBr(isNull, notFoundBlk, foundBlk)

	// Found: strlen + promise_string_new → return optional present
	strlenFn := c.funcs["strlen"]
	valLen := foundBlk.NewCall(strlenFn, result)
	valStr := foundBlk.NewCall(c.funcs["promise_string_new"], result, valLen)
	c.storeOptionalSome(foundBlk, sret, valStr, optType)
	foundBlk.NewRet(nil)

	// Not found: return optional none
	c.storeOptionalNone(notFoundBlk, sret, optType)
	notFoundBlk.NewRet(nil)
}

// defineGetCwdBody: void @promise_os_get_cwd(i8* sret)
// Calls pal_getcwd with a 4096-byte buffer.
// This is a failable extern: sret points to {i1, i8*, i8*} (failable string result).
// On success: stores {false, string_instance_ptr, null}.
// On failure: stores {true, zero, error_instance_ptr}.
func (c *Compiler) defineGetCwdBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	// Compute the failable result type: {i1, i8*, i8*} for string!
	innerType := c.resolveType(types.TypString)
	resultType := computeResultType(innerType)

	// Allocate 4096-byte buffer for getcwd
	bufSize := constant.NewInt(irtypes.I64, 4096)
	buf := entry.NewCall(c.palAlloc, bufSize)

	// Call PAL: i8* @pal_getcwd(i8* buf, i64 len)
	c.emitEnterSyscall(entry)
	result := entry.NewCall(c.palGetCwd, buf, bufSize)
	c.emitExitSyscall(entry)

	// Check if result is null (failure)
	isNull := entry.NewICmp(enum.IPredEQ, result, constant.NewNull(irtypes.I8Ptr))
	successBlk := fn.NewBlock(".success")
	errorBlk := fn.NewBlock(".error")
	entry.NewCondBr(isNull, errorBlk, successBlk)

	// Success: strlen + promise_string_new → store failable success
	strlenFn := c.funcs["strlen"]
	pathLen := successBlk.NewCall(strlenFn, result)
	pathStr := successBlk.NewCall(c.funcs["promise_string_new"], result, pathLen)
	successBlk.NewCall(c.palFree, buf)
	c.storeFailableSuccess(successBlk, sret, pathStr, resultType)
	successBlk.NewRet(nil)

	// Error: construct error from a generic message, store failable error
	errorBlk.NewCall(c.palFree, buf)
	errInst := c.constructErrorFromGlobalStr(errorBlk, "failed to get working directory")
	c.storeFailableError(errorBlk, sret, errInst, resultType)
	errorBlk.NewRet(nil)
}

// defineExitBody: void @promise_os_exit(i8* code)
// Extracts int code → i32, calls pal_exit → unreachable.
func (c *Compiler) defineExitBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	codeParam := fn.Params[0]

	// Extract raw i64 from Promise int, truncate to i32
	codeRaw := c.extractRawInt(entry, codeParam)
	codeI32 := entry.NewTrunc(codeRaw, irtypes.I32)

	// Call PAL: void @pal_exit(i32 code) [noreturn]
	entry.NewCall(c.palExit, codeI32)
	entry.NewUnreachable()
}

// defineArgsBody: i8* @promise_os_get_args()
// Returns string[] (Vector[string]) from argv[1..argc-1], excluding the program name.
// Reads from the __promise_argc/__promise_argv globals populated in main's prologue.
func (c *Compiler) defineArgsBody(fn *ir.Func) {
	zero32 := constant.NewInt(irtypes.I32, 0)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one32 := constant.NewInt(irtypes.I32, 1)
	one64 := constant.NewInt(irtypes.I64, 1)
	ptrSize := constant.NewInt(irtypes.I64, int64(c.ptrSize()))
	headerSize := constant.NewInt(irtypes.I64, vectorHeaderSize)
	vectorHdrType := vectorHeaderType()
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)

	entry := fn.NewBlock(".entry")

	// Load argc and argv from globals
	argc := entry.NewLoad(irtypes.I32, c.argcGlobal)
	argv := entry.NewLoad(i8PtrPtrType, c.argvGlobal)

	// argsCount = max(0, argc - 1) — skip argv[0] (program name)
	rawCount := entry.NewSub(argc, one32)
	isNeg := entry.NewICmp(enum.IPredSLT, rawCount, zero32)
	argsCount32 := entry.NewSelect(isNeg, zero32, rawCount)
	argsCount := entry.NewZExt(argsCount32, irtypes.I64)

	// Allocate vector: header (16 bytes) + argsCount * ptrSize
	dataSize := entry.NewMul(argsCount, ptrSize)
	totalSize := entry.NewAdd(headerSize, dataSize)
	rawSlice := entry.NewCall(c.palAlloc, totalSize)

	// Store len and cap in vector header
	hdrPtr := entry.NewBitCast(rawSlice, irtypes.NewPointer(vectorHdrType))
	lenField := entry.NewGetElementPtr(vectorHdrType, hdrPtr, zero32, zero32)
	entry.NewStore(argsCount, lenField)
	capField := entry.NewGetElementPtr(vectorHdrType, hdrPtr, zero32, one32)
	entry.NewStore(argsCount, capField)

	// Loop: for i = 0; i < argsCount; i++
	hasArgs := entry.NewICmp(enum.IPredSGT, argsCount, zero64)
	loopHdr := fn.NewBlock(".loop_hdr")
	doneBlk := fn.NewBlock(".done")
	entry.NewCondBr(hasArgs, loopHdr, doneBlk)

	// loop_hdr: phi i, check i < argsCount
	loopBody := fn.NewBlock(".loop_body")
	iPhi := loopHdr.NewPhi(ir.NewIncoming(zero64, entry))
	cond := loopHdr.NewICmp(enum.IPredSLT, iPhi, argsCount)
	loopHdr.NewCondBr(cond, loopBody, doneBlk)

	// loop_body: argv[i+1] → strlen → promise_string_new → store in vector
	strlenFn := c.funcs["strlen"]
	argIdx := loopBody.NewAdd(iPhi, one64) // i+1 to skip argv[0]
	argvElemPtr := loopBody.NewGetElementPtr(irtypes.I8Ptr, argv, argIdx)
	argCStr := loopBody.NewLoad(irtypes.I8Ptr, argvElemPtr)
	argLen := loopBody.NewCall(strlenFn, argCStr)
	argStr := loopBody.NewCall(c.funcs["promise_string_new"], argCStr, argLen)

	// Store string at rawSlice + headerSize + i * ptrSize
	elemOff := loopBody.NewMul(iPhi, ptrSize)
	elemOff2 := loopBody.NewAdd(headerSize, elemOff)
	elemPtr := loopBody.NewGetElementPtr(irtypes.I8, rawSlice, elemOff2)
	elemPtrTyped := loopBody.NewBitCast(elemPtr, irtypes.NewPointer(irtypes.I8Ptr))
	loopBody.NewStore(argStr, elemPtrTyped)

	// i++
	iNext := loopBody.NewAdd(iPhi, one64)
	iPhi.Incs = append(iPhi.Incs, ir.NewIncoming(iNext, loopBody))
	loopBody.NewBr(loopHdr)

	// done: return vector pointer
	doneBlk.NewRet(rawSlice)
}

// stringInstanceToCStr creates a null-terminated C string from a Promise string instance pointer.
// Unlike stringToCStr which takes a value struct pointer, this takes the raw instance i8*.
// Allocates via palAlloc — caller must free with palFree after use.
func (c *Compiler) stringInstanceToCStr(block *ir.Block, instRaw value.Value) value.Value {
	dataPtr, dataLen := c.extractStringDataLenFromInstance(block, instRaw)
	allocSize := block.NewAdd(dataLen, constant.NewInt(irtypes.I64, 1))
	cstr := block.NewCall(c.palAlloc, allocSize)
	block.NewCall(c.funcs["llvm.memcpy"], cstr, dataPtr, dataLen, constant.False)
	nullPos := block.NewGetElementPtr(irtypes.I8, cstr, dataLen)
	block.NewStore(constant.NewInt(irtypes.I8, 0), nullPos)
	return cstr
}

// defineExecutableBody: void @promise_os_get_executable(i8* sret)
// Returns string from argv[0] (the program name / executable path).
// Reads from __promise_argv global. Returns empty string if argv is null.
func (c *Compiler) defineExecutableBody(fn *ir.Func) {
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)

	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	// Load argv from global
	argv := entry.NewLoad(i8PtrPtrType, c.argvGlobal)

	// Check if argv is null (WASM or no args)
	isNull := entry.NewICmp(enum.IPredEQ, argv, constant.NewNull(i8PtrPtrType))
	hasArgv := fn.NewBlock(".has_argv")
	noArgv := fn.NewBlock(".no_argv")
	entry.NewCondBr(isNull, noArgv, hasArgv)

	// has_argv: load argv[0], strlen → promise_string_new → store result
	strlenFn := c.funcs["strlen"]
	arg0Ptr := hasArgv.NewGetElementPtr(irtypes.I8Ptr, argv, constant.NewInt(irtypes.I64, 0))
	arg0 := hasArgv.NewLoad(irtypes.I8Ptr, arg0Ptr)
	arg0Len := hasArgv.NewCall(strlenFn, arg0)
	arg0Str := hasArgv.NewCall(c.funcs["promise_string_new"], arg0, arg0Len)
	c.storeStringResult(hasArgv, sret, arg0Str)
	hasArgv.NewRet(nil)

	// no_argv: return empty string
	emptyStr := noArgv.NewCall(c.funcs["promise_string_new"],
		constant.NewNull(irtypes.I8Ptr), constant.NewInt(irtypes.I64, 0))
	c.storeStringResult(noArgv, sret, emptyStr)
	noArgv.NewRet(nil)
}

// defineSpawnBody: void @promise_os_spawn(i8* sret, i8* program, i8* arguments)
// Converts program string and arguments vector to C argv, calls pal_spawn,
// caches stdout/stderr fds in TLS globals, returns int! (pid).
func (c *Compiler) defineSpawnBody(fn *ir.Func) {
	zero32 := constant.NewInt(irtypes.I32, 0)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	ptrSize := constant.NewInt(irtypes.I64, int64(c.ptrSize()))
	headerSize := constant.NewInt(irtypes.I64, vectorHeaderSize)
	vectorHdrType := vectorHeaderType()
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)

	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	programParam := fn.Params[1]
	argsParam := fn.Params[2] // string[] vector pointer (i8*)

	// Compute failable result type: {i1, intValueType, i8*} for int!
	innerType := c.resolveType(types.TypInt)
	resultType := computeResultType(innerType)

	// Convert program string to C string
	programCStr := c.stringToCStr(entry, programParam)

	// Load vector length from header (masked — bit 63 is static flag)
	hdrPtr := entry.NewBitCast(argsParam, irtypes.NewPointer(vectorHdrType))
	argsCount := loadVectorLen(entry, hdrPtr)

	// Allocate argv array: (1 + argsCount + 1) * ptrSize
	totalSlots := entry.NewAdd(argsCount, constant.NewInt(irtypes.I64, 2))
	argvSize := entry.NewMul(totalSlots, ptrSize)
	argvRaw := entry.NewCall(c.palAlloc, argvSize)
	argv := entry.NewBitCast(argvRaw, i8PtrPtrType)

	// argv[0] = program C string
	argv0Ptr := entry.NewGetElementPtr(irtypes.I8Ptr, argv, zero64)
	entry.NewStore(programCStr, argv0Ptr)

	// Loop: convert each vector element to C string and store in argv
	hasArgs := entry.NewICmp(enum.IPredSGT, argsCount, zero64)
	loopHdr := fn.NewBlock(".argv_loop_hdr")
	loopDone := fn.NewBlock(".argv_loop_done")
	entry.NewCondBr(hasArgs, loopHdr, loopDone)

	loopBody := fn.NewBlock(".argv_loop_body")
	iPhi := loopHdr.NewPhi(ir.NewIncoming(zero64, entry))
	cond := loopHdr.NewICmp(enum.IPredSLT, iPhi, argsCount)
	loopHdr.NewCondBr(cond, loopBody, loopDone)

	elemOff := loopBody.NewMul(iPhi, ptrSize)
	elemOff2 := loopBody.NewAdd(headerSize, elemOff)
	elemPtr := loopBody.NewGetElementPtr(irtypes.I8, argsParam, elemOff2)
	elemPtrTyped := loopBody.NewBitCast(elemPtr, irtypes.NewPointer(irtypes.I8Ptr))
	strInst := loopBody.NewLoad(irtypes.I8Ptr, elemPtrTyped)
	argCStr := c.stringInstanceToCStr(loopBody, strInst)

	argIdx := loopBody.NewAdd(iPhi, one64)
	argvSlotPtr := loopBody.NewGetElementPtr(irtypes.I8Ptr, argv, argIdx)
	loopBody.NewStore(argCStr, argvSlotPtr)

	iNext := loopBody.NewAdd(iPhi, one64)
	iPhi.Incs = append(iPhi.Incs, ir.NewIncoming(iNext, loopBody))
	loopBody.NewBr(loopHdr)

	// Null terminator at argv[argsCount + 1]
	nullIdx := loopDone.NewAdd(argsCount, one64)
	nullSlotPtr := loopDone.NewGetElementPtr(irtypes.I8Ptr, argv, nullIdx)
	loopDone.NewStore(constant.NewNull(irtypes.I8Ptr), nullSlotPtr)

	// Allocate output fd pointers on stack
	outStdoutFd := loopDone.NewAlloca(irtypes.I32)
	outStderrFd := loopDone.NewAlloca(irtypes.I32)

	// Call pal_spawn
	c.emitEnterSyscall(loopDone)
	pid := loopDone.NewCall(c.palSpawn, programCStr, argv, outStdoutFd, outStderrFd)
	c.emitExitSyscall(loopDone)

	// Cache fds in TLS globals
	stdoutFd := loopDone.NewLoad(irtypes.I32, outStdoutFd)
	stderrFd := loopDone.NewLoad(irtypes.I32, outStderrFd)
	loopDone.NewStore(stdoutFd, c.spawnStdoutFd)
	loopDone.NewStore(stderrFd, c.spawnStderrFd)

	// Free argv C strings: argv[1..argsCount]
	hasFreeArgs := loopDone.NewICmp(enum.IPredSGT, argsCount, zero64)
	freeLoopHdr := fn.NewBlock(".free_loop_hdr")
	freeDone := fn.NewBlock(".free_done")
	loopDone.NewCondBr(hasFreeArgs, freeLoopHdr, freeDone)

	freeLoopBody := fn.NewBlock(".free_loop_body")
	jPhi := freeLoopHdr.NewPhi(ir.NewIncoming(zero64, loopDone))
	freeCond := freeLoopHdr.NewICmp(enum.IPredSLT, jPhi, argsCount)
	freeLoopHdr.NewCondBr(freeCond, freeLoopBody, freeDone)

	freeIdx := freeLoopBody.NewAdd(jPhi, one64)
	freeSlotPtr := freeLoopBody.NewGetElementPtr(irtypes.I8Ptr, argv, freeIdx)
	freeStr := freeLoopBody.NewLoad(irtypes.I8Ptr, freeSlotPtr)
	freeLoopBody.NewCall(c.palFree, freeStr)
	jNext := freeLoopBody.NewAdd(jPhi, one64)
	jPhi.Incs = append(jPhi.Incs, ir.NewIncoming(jNext, freeLoopBody))
	freeLoopBody.NewBr(freeLoopHdr)

	// Free program C string and argv array
	freeDone.NewCall(c.palFree, programCStr)
	freeDone.NewCall(c.palFree, argvRaw)

	// Check result: -1 means error
	isErr := freeDone.NewICmp(enum.IPredSLT, pid, zero32)
	successBlk := fn.NewBlock(".success")
	errorBlk := fn.NewBlock(".error")
	freeDone.NewCondBr(isErr, errorBlk, successBlk)

	// Success: store pid as i64 failable success
	pidI64 := successBlk.NewSExt(pid, irtypes.I64)
	c.storeFailableSuccess(successBlk, sret, pidI64, resultType)
	successBlk.NewRet(nil)

	// Error: construct error, store failable error
	errInst := c.constructErrorFromGlobalStr(errorBlk, "failed to spawn process")
	c.storeFailableError(errorBlk, sret, errInst, resultType)
	errorBlk.NewRet(nil)
}

// defineExecReplaceBody: void @promise_os_exec_replace(i8* sret, i8* program, i8* arguments)
// Converts program + arguments to a C argv and calls pal_exec_replace, which on
// Unix replaces the process image (never returns) and on Windows runs the child,
// waits, and exits with its code (also never returns). Control only reaches the
// code after the call on failure, so this stores an int! error after freeing the
// argv C strings. The success path never returns, so it leaks nothing (T0770).
func (c *Compiler) defineExecReplaceBody(fn *ir.Func) {
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	ptrSize := constant.NewInt(irtypes.I64, int64(c.ptrSize()))
	headerSize := constant.NewInt(irtypes.I64, vectorHeaderSize)
	vectorHdrType := vectorHeaderType()
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)

	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	programParam := fn.Params[1]
	argsParam := fn.Params[2] // string[] vector pointer (i8*)

	// Failable result type: {i1, intValueType, i8*} for int!
	innerType := c.resolveType(types.TypInt)
	resultType := computeResultType(innerType)

	// Convert program string to C string.
	programCStr := c.stringToCStr(entry, programParam)

	// Load vector length from header (masked — bit 63 is static flag).
	hdrPtr := entry.NewBitCast(argsParam, irtypes.NewPointer(vectorHdrType))
	argsCount := loadVectorLen(entry, hdrPtr)

	// Allocate argv array: (1 + argsCount + 1) * ptrSize.
	totalSlots := entry.NewAdd(argsCount, constant.NewInt(irtypes.I64, 2))
	argvSize := entry.NewMul(totalSlots, ptrSize)
	argvRaw := entry.NewCall(c.palAlloc, argvSize)
	argv := entry.NewBitCast(argvRaw, i8PtrPtrType)

	// argv[0] = program C string.
	argv0Ptr := entry.NewGetElementPtr(irtypes.I8Ptr, argv, zero64)
	entry.NewStore(programCStr, argv0Ptr)

	// Loop: convert each vector element to C string and store in argv.
	hasArgs := entry.NewICmp(enum.IPredSGT, argsCount, zero64)
	loopHdr := fn.NewBlock(".argv_loop_hdr")
	loopDone := fn.NewBlock(".argv_loop_done")
	entry.NewCondBr(hasArgs, loopHdr, loopDone)

	loopBody := fn.NewBlock(".argv_loop_body")
	iPhi := loopHdr.NewPhi(ir.NewIncoming(zero64, entry))
	cond := loopHdr.NewICmp(enum.IPredSLT, iPhi, argsCount)
	loopHdr.NewCondBr(cond, loopBody, loopDone)

	elemOff := loopBody.NewMul(iPhi, ptrSize)
	elemOff2 := loopBody.NewAdd(headerSize, elemOff)
	elemPtr := loopBody.NewGetElementPtr(irtypes.I8, argsParam, elemOff2)
	elemPtrTyped := loopBody.NewBitCast(elemPtr, irtypes.NewPointer(irtypes.I8Ptr))
	strInst := loopBody.NewLoad(irtypes.I8Ptr, elemPtrTyped)
	argCStr := c.stringInstanceToCStr(loopBody, strInst)

	argIdx := loopBody.NewAdd(iPhi, one64)
	argvSlotPtr := loopBody.NewGetElementPtr(irtypes.I8Ptr, argv, argIdx)
	loopBody.NewStore(argCStr, argvSlotPtr)

	iNext := loopBody.NewAdd(iPhi, one64)
	iPhi.Incs = append(iPhi.Incs, ir.NewIncoming(iNext, loopBody))
	loopBody.NewBr(loopHdr)

	// Null terminator at argv[argsCount + 1].
	nullIdx := loopDone.NewAdd(argsCount, one64)
	nullSlotPtr := loopDone.NewGetElementPtr(irtypes.I8Ptr, argv, nullIdx)
	loopDone.NewStore(constant.NewNull(irtypes.I8Ptr), nullSlotPtr)

	// Call pal_exec_replace. On success it never returns; on failure it returns
	// -1 and control falls through to the cleanup + error path below.
	loopDone.NewCall(c.palExecReplace, programCStr, argv)

	// Free argv C strings: argv[1..argsCount].
	hasFreeArgs := loopDone.NewICmp(enum.IPredSGT, argsCount, zero64)
	freeLoopHdr := fn.NewBlock(".free_loop_hdr")
	freeDone := fn.NewBlock(".free_done")
	loopDone.NewCondBr(hasFreeArgs, freeLoopHdr, freeDone)

	freeLoopBody := fn.NewBlock(".free_loop_body")
	jPhi := freeLoopHdr.NewPhi(ir.NewIncoming(zero64, loopDone))
	freeCond := freeLoopHdr.NewICmp(enum.IPredSLT, jPhi, argsCount)
	freeLoopHdr.NewCondBr(freeCond, freeLoopBody, freeDone)

	freeIdx := freeLoopBody.NewAdd(jPhi, one64)
	freeSlotPtr := freeLoopBody.NewGetElementPtr(irtypes.I8Ptr, argv, freeIdx)
	freeStr := freeLoopBody.NewLoad(irtypes.I8Ptr, freeSlotPtr)
	freeLoopBody.NewCall(c.palFree, freeStr)
	jNext := freeLoopBody.NewAdd(jPhi, one64)
	jPhi.Incs = append(jPhi.Incs, ir.NewIncoming(jNext, freeLoopBody))
	freeLoopBody.NewBr(freeLoopHdr)

	// Free program C string and argv array.
	freeDone.NewCall(c.palFree, programCStr)
	freeDone.NewCall(c.palFree, argvRaw)

	// Failure: construct error, store failable error.
	errInst := c.constructErrorFromGlobalStr(freeDone, "failed to exec program")
	c.storeFailableError(freeDone, sret, errInst, resultType)
	freeDone.NewRet(nil)
}

// defineSpawnStdoutFdBody: void @promise_os_spawn_stdout_fd(i8* sret)
// Returns cached TLS stdout fd as int via sret. The sret points to a
// promise_int_v struct {i8* vtable, i8* rtti, i64 value} — store at field 2.
func (c *Compiler) defineSpawnStdoutFdBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	fd := entry.NewLoad(irtypes.I32, c.spawnStdoutFd)
	entry.NewStore(constant.NewInt(irtypes.I32, -1), c.spawnStdoutFd)
	fdI64 := entry.NewSExt(fd, irtypes.I64)
	c.storeIntResult(entry, sret, fdI64)
	entry.NewRet(nil)
}

// defineSpawnStderrFdBody: void @promise_os_spawn_stderr_fd(i8* sret)
// Returns cached TLS stderr fd as int via sret. Same struct layout as above.
func (c *Compiler) defineSpawnStderrFdBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	fd := entry.NewLoad(irtypes.I32, c.spawnStderrFd)
	entry.NewStore(constant.NewInt(irtypes.I32, -1), c.spawnStderrFd)
	fdI64 := entry.NewSExt(fd, irtypes.I64)
	c.storeIntResult(entry, sret, fdI64)
	entry.NewRet(nil)
}

// defineReadPipeBody: void @promise_os_read_pipe(i8* sret, i8* fd_value)
// Extracts int fd, calls pal_read_pipe with enter/exit_syscall, returns string.
func (c *Compiler) defineReadPipeBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	fdParam := fn.Params[1] // Promise int value

	// Extract raw i64 from Promise int, truncate to i32
	fdRaw := c.extractRawInt(entry, fdParam)
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	// Allocate output pointers on stack
	outBuf := entry.NewAlloca(irtypes.I8Ptr)
	outLen := entry.NewAlloca(irtypes.I64)

	// Call pal_read_pipe with syscall handoff
	c.emitEnterSyscall(entry)
	entry.NewCall(c.palReadPipe, fdI32, outBuf, outLen)
	c.emitExitSyscall(entry)

	// Load buffer and length
	buf := entry.NewLoad(irtypes.I8Ptr, outBuf)
	bufLen := entry.NewLoad(irtypes.I64, outLen)

	// Create Promise string from buffer
	isNull := entry.NewICmp(enum.IPredEQ, buf, constant.NewNull(irtypes.I8Ptr))
	hasData := fn.NewBlock(".has_data")
	noData := fn.NewBlock(".no_data")
	entry.NewCondBr(isNull, noData, hasData)

	str := hasData.NewCall(c.funcs["promise_string_new"], buf, bufLen)
	hasData.NewCall(c.palFree, buf)
	c.storeStringResult(hasData, sret, str)
	hasData.NewRet(nil)

	emptyStr := noData.NewCall(c.funcs["promise_string_new"],
		constant.NewNull(irtypes.I8Ptr), constant.NewInt(irtypes.I64, 0))
	c.storeStringResult(noData, sret, emptyStr)
	noData.NewRet(nil)
}

// defineWaitPidBody: void @promise_os_wait_pid(i8* sret, i8* pid_value)
// Extracts int pid, calls pal_wait_pid with enter/exit_syscall, returns int!.
func (c *Compiler) defineWaitPidBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	pidParam := fn.Params[1] // Promise int value

	innerType := c.resolveType(types.TypInt)
	resultType := computeResultType(innerType)

	// Extract raw i64 from Promise int, truncate to i32
	pidRaw := c.extractRawInt(entry, pidParam)
	pidI32 := entry.NewTrunc(pidRaw, irtypes.I32)

	// Call pal_wait_pid with syscall handoff
	c.emitEnterSyscall(entry)
	exitCode := entry.NewCall(c.palWaitPid, pidI32)
	c.emitExitSyscall(entry)

	// Check result: -1 means error
	isErr := entry.NewICmp(enum.IPredSLT, exitCode, constant.NewInt(irtypes.I32, 0))
	successBlk := fn.NewBlock(".success")
	errorBlk := fn.NewBlock(".error")
	entry.NewCondBr(isErr, errorBlk, successBlk)

	exitCodeI64 := successBlk.NewSExt(exitCode, irtypes.I64)
	c.storeFailableSuccess(successBlk, sret, exitCodeI64, resultType)
	successBlk.NewRet(nil)

	errInst := c.constructErrorFromGlobalStr(errorBlk, "failed to wait for process")
	c.storeFailableError(errorBlk, sret, errInst, resultType)
	errorBlk.NewRet(nil)
}

// defineSetEnvBody: void @promise_os_set_env(i8* name, i8* value)
// Converts Promise name and value strings to C strings, calls pal_setenv, frees temps.
// Non-failable — setenv failures (ENOMEM) are silently ignored.
func (c *Compiler) defineSetEnvBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	nameParam := fn.Params[0]
	valueParam := fn.Params[1]

	nameCStr := c.stringToCStr(entry, nameParam)
	valueCStr := c.stringToCStr(entry, valueParam)

	c.emitEnterSyscall(entry)
	entry.NewCall(c.palSetEnv, nameCStr, valueCStr)
	c.emitExitSyscall(entry)

	entry.NewCall(c.palFree, nameCStr)
	entry.NewCall(c.palFree, valueCStr)
	entry.NewRet(nil)
}

// defineUnsetEnvBody: void @promise_os_unset_env(i8* name)
// Converts Promise name string to C string, calls pal_unsetenv, frees temp.
// Non-failable — unsetenv failures are silently ignored.
func (c *Compiler) defineUnsetEnvBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	nameParam := fn.Params[0]

	nameCStr := c.stringToCStr(entry, nameParam)

	c.emitEnterSyscall(entry)
	entry.NewCall(c.palUnsetEnv, nameCStr)
	c.emitExitSyscall(entry)

	entry.NewCall(c.palFree, nameCStr)
	entry.NewRet(nil)
}

// defineSetCwdBody: void @promise_os_set_cwd(i8* sret, i8* path)
// Converts Promise path string to C string, calls pal_chdir.
// Returns int! (0 on success, raises error on failure).
func (c *Compiler) defineSetCwdBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	pathParam := fn.Params[1]

	// Compute the failable result type for int!: {i1, i64, i8*}
	innerType := c.resolveType(types.TypInt)
	resultType := computeResultType(innerType)

	pathCStr := c.stringToCStr(entry, pathParam)

	c.emitEnterSyscall(entry)
	ret := entry.NewCall(c.palChdir, pathCStr)
	c.emitExitSyscall(entry)

	entry.NewCall(c.palFree, pathCStr)

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	successBlk := fn.NewBlock(".success")
	errorBlk := fn.NewBlock(".error")
	entry.NewCondBr(isErr, errorBlk, successBlk)

	// Success: store 0 as failable success
	c.storeFailableSuccess(successBlk, sret, constant.NewInt(irtypes.I64, 0), resultType)
	successBlk.NewRet(nil)

	// Error: construct error, store failable error
	errInst := c.constructErrorFromGlobalStr(errorBlk, "failed to change working directory")
	c.storeFailableError(errorBlk, sret, errInst, resultType)
	errorBlk.NewRet(nil)
}

// defineSpawnStreamingBody: void @promise_os_spawn_streaming(i8* sret, i8* program, i8* arguments)
// Like defineSpawnBody but calls pal_spawn_streaming (3 pipes: stdin+stdout+stderr).
// Caches all 3 pipe fds in TLS globals, returns int! (pid).
func (c *Compiler) defineSpawnStreamingBody(fn *ir.Func) {
	zero32 := constant.NewInt(irtypes.I32, 0)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	ptrSize := constant.NewInt(irtypes.I64, int64(c.ptrSize()))
	headerSize := constant.NewInt(irtypes.I64, vectorHeaderSize)
	vectorHdrType := vectorHeaderType()
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)

	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	programParam := fn.Params[1]
	argsParam := fn.Params[2]

	innerType := c.resolveType(types.TypInt)
	resultType := computeResultType(innerType)

	// Convert program string to C string
	programCStr := c.stringToCStr(entry, programParam)

	// Load vector length from header (masked — bit 63 is static flag)
	hdrPtr := entry.NewBitCast(argsParam, irtypes.NewPointer(vectorHdrType))
	argsCount := loadVectorLen(entry, hdrPtr)

	// Allocate argv array: (1 + argsCount + 1) * ptrSize
	totalSlots := entry.NewAdd(argsCount, constant.NewInt(irtypes.I64, 2))
	argvSize := entry.NewMul(totalSlots, ptrSize)
	argvRaw := entry.NewCall(c.palAlloc, argvSize)
	argv := entry.NewBitCast(argvRaw, i8PtrPtrType)

	// argv[0] = program C string
	argv0Ptr := entry.NewGetElementPtr(irtypes.I8Ptr, argv, zero64)
	entry.NewStore(programCStr, argv0Ptr)

	// Loop: convert each vector element to C string and store in argv
	hasArgs := entry.NewICmp(enum.IPredSGT, argsCount, zero64)
	loopHdr := fn.NewBlock(".argv_loop_hdr")
	loopDone := fn.NewBlock(".argv_loop_done")
	entry.NewCondBr(hasArgs, loopHdr, loopDone)

	loopBody := fn.NewBlock(".argv_loop_body")
	iPhi := loopHdr.NewPhi(ir.NewIncoming(zero64, entry))
	cond := loopHdr.NewICmp(enum.IPredSLT, iPhi, argsCount)
	loopHdr.NewCondBr(cond, loopBody, loopDone)

	elemOff := loopBody.NewMul(iPhi, ptrSize)
	elemOff2 := loopBody.NewAdd(headerSize, elemOff)
	elemPtr := loopBody.NewGetElementPtr(irtypes.I8, argsParam, elemOff2)
	elemPtrTyped := loopBody.NewBitCast(elemPtr, irtypes.NewPointer(irtypes.I8Ptr))
	strInst := loopBody.NewLoad(irtypes.I8Ptr, elemPtrTyped)
	argCStr := c.stringInstanceToCStr(loopBody, strInst)

	argIdx := loopBody.NewAdd(iPhi, one64)
	argvSlotPtr := loopBody.NewGetElementPtr(irtypes.I8Ptr, argv, argIdx)
	loopBody.NewStore(argCStr, argvSlotPtr)

	iNext := loopBody.NewAdd(iPhi, one64)
	iPhi.Incs = append(iPhi.Incs, ir.NewIncoming(iNext, loopBody))
	loopBody.NewBr(loopHdr)

	// Null terminator at argv[argsCount + 1]
	nullIdx := loopDone.NewAdd(argsCount, one64)
	nullSlotPtr := loopDone.NewGetElementPtr(irtypes.I8Ptr, argv, nullIdx)
	loopDone.NewStore(constant.NewNull(irtypes.I8Ptr), nullSlotPtr)

	// Allocate output fd pointers on stack (3 fds: stdin, stdout, stderr)
	outStdinFd := loopDone.NewAlloca(irtypes.I32)
	outStdoutFd := loopDone.NewAlloca(irtypes.I32)
	outStderrFd := loopDone.NewAlloca(irtypes.I32)

	// Call pal_spawn_streaming
	c.emitEnterSyscall(loopDone)
	pid := loopDone.NewCall(c.palSpawnStreaming, programCStr, argv, outStdinFd, outStdoutFd, outStderrFd)
	c.emitExitSyscall(loopDone)

	// Cache all 3 fds in TLS globals
	stdinFd := loopDone.NewLoad(irtypes.I32, outStdinFd)
	stdoutFd := loopDone.NewLoad(irtypes.I32, outStdoutFd)
	stderrFd := loopDone.NewLoad(irtypes.I32, outStderrFd)
	loopDone.NewStore(stdinFd, c.spawnStdinFd)
	loopDone.NewStore(stdoutFd, c.spawnStdoutFd)
	loopDone.NewStore(stderrFd, c.spawnStderrFd)

	// Free argv C strings: argv[1..argsCount]
	hasFreeArgs := loopDone.NewICmp(enum.IPredSGT, argsCount, zero64)
	freeLoopHdr := fn.NewBlock(".free_loop_hdr")
	freeDone := fn.NewBlock(".free_done")
	loopDone.NewCondBr(hasFreeArgs, freeLoopHdr, freeDone)

	freeLoopBody := fn.NewBlock(".free_loop_body")
	jPhi := freeLoopHdr.NewPhi(ir.NewIncoming(zero64, loopDone))
	freeCond := freeLoopHdr.NewICmp(enum.IPredSLT, jPhi, argsCount)
	freeLoopHdr.NewCondBr(freeCond, freeLoopBody, freeDone)

	freeIdx := freeLoopBody.NewAdd(jPhi, one64)
	freeSlotPtr := freeLoopBody.NewGetElementPtr(irtypes.I8Ptr, argv, freeIdx)
	freeStr := freeLoopBody.NewLoad(irtypes.I8Ptr, freeSlotPtr)
	freeLoopBody.NewCall(c.palFree, freeStr)
	jNext := freeLoopBody.NewAdd(jPhi, one64)
	jPhi.Incs = append(jPhi.Incs, ir.NewIncoming(jNext, freeLoopBody))
	freeLoopBody.NewBr(freeLoopHdr)

	// Free program C string and argv array
	freeDone.NewCall(c.palFree, programCStr)
	freeDone.NewCall(c.palFree, argvRaw)

	// Check result: -1 means error
	isErr := freeDone.NewICmp(enum.IPredSLT, pid, zero32)
	successBlk := fn.NewBlock(".success")
	errorBlk := fn.NewBlock(".error")
	freeDone.NewCondBr(isErr, errorBlk, successBlk)

	// Success: store pid as i64 failable success
	pidI64 := successBlk.NewSExt(pid, irtypes.I64)
	c.storeFailableSuccess(successBlk, sret, pidI64, resultType)
	successBlk.NewRet(nil)

	// Error: construct error, store failable error
	errInst := c.constructErrorFromGlobalStr(errorBlk, "failed to spawn process")
	c.storeFailableError(errorBlk, sret, errInst, resultType)
	errorBlk.NewRet(nil)
}

// defineSpawnStdinFdBody: void @promise_os_spawn_stdin_fd(i8* sret)
// Returns cached TLS stdin fd as int via sret, then resets to -1.
func (c *Compiler) defineSpawnStdinFdBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	fd := entry.NewLoad(irtypes.I32, c.spawnStdinFd)
	entry.NewStore(constant.NewInt(irtypes.I32, -1), c.spawnStdinFd)
	fdI64 := entry.NewSExt(fd, irtypes.I64)
	c.storeIntResult(entry, sret, fdI64)
	entry.NewRet(nil)
}

// definePipeReadBytesBody: void @promise_os_pipe_read_bytes(i8* sret, i8* fd, i8* ~buf)
// Reads up to buf.len bytes from fd into the provided u8[] buffer.
// Returns bytes read as Promise int (negative = -errno on error, 0 = EOF).
func (c *Compiler) definePipeReadBytesBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	vecPtr := fn.Params[2]
	dataPtr, dataLen := extractVectorDataLen(entry, vecPtr)

	c.emitEnterSyscall(entry)
	n := entry.NewCall(c.palFileRead, fdI32, dataPtr, dataLen)
	c.emitExitSyscall(entry)

	c.storeIntResult(entry, sret, n)
	entry.NewRet(nil)
}

// definePipeWriteBytesBody: void @promise_os_pipe_write_bytes(i8* sret, i8* fd, i8* buf)
// Writes buf.len bytes from the u8[] buffer to fd.
// Returns bytes written as Promise int (negative = -errno).
func (c *Compiler) definePipeWriteBytesBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	vecPtr := fn.Params[2]
	dataPtr, dataLen := extractVectorDataLen(entry, vecPtr)

	c.emitEnterSyscall(entry)
	written := entry.NewCall(c.palFileWrite, fdI32, dataPtr, dataLen)
	c.emitExitSyscall(entry)

	c.storeIntResult(entry, sret, written)
	entry.NewRet(nil)
}

// definePipeCloseBody: void @promise_os_pipe_close(i8* sret, i8* fd)
// Closes a pipe fd. Returns 0 on success, negative on error.
func (c *Compiler) definePipeCloseBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	fdParam := fn.Params[1]

	fdRaw := c.extractRawInt(entry, fdParam)
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	c.emitEnterSyscall(entry)
	rc := entry.NewCall(c.palFileClose, fdI32)
	c.emitExitSyscall(entry)

	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, sret, rcI64)
	entry.NewRet(nil)
}

// defineKillBody: void @promise_os_kill(i8* sret, i8* pid)
// Sends SIGKILL (signal 9) to a process. Returns 0 on success, -1 on error.
func (c *Compiler) defineKillBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	pidParam := fn.Params[1]

	pidRaw := c.extractRawInt(entry, pidParam)
	pidI32 := entry.NewTrunc(pidRaw, irtypes.I32)

	rc := entry.NewCall(c.palKill, pidI32, constant.NewInt(irtypes.I32, 9))

	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, sret, rcI64)
	entry.NewRet(nil)
}

// OS info bridges

// defineGetEnvironBody: i8* @promise_os_get_environ()
// Returns string[] built from the C environ global (null-terminated array of "KEY=VALUE" strings).
func (c *Compiler) defineGetEnvironBody(fn *ir.Func) {
	zero32 := constant.NewInt(irtypes.I32, 0)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	one32 := constant.NewInt(irtypes.I32, 1)
	ptrSize := constant.NewInt(irtypes.I64, int64(c.ptrSize()))
	headerSize := constant.NewInt(irtypes.I64, vectorHeaderSize)
	vectorHdrType := vectorHeaderType()
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)

	entry := fn.NewBlock(".entry")

	// Get environ pointer
	environ := entry.NewCall(c.palGetEnviron)

	// Check for null environ
	isNull := entry.NewICmp(enum.IPredEQ, environ, constant.NewNull(i8PtrPtrType))
	countHdr := fn.NewBlock(".count_hdr")
	emptyBlk := fn.NewBlock(".empty")
	entry.NewCondBr(isNull, emptyBlk, countHdr)

	// Empty environ: return empty vector
	emptyVec := emptyBlk.NewCall(c.palAlloc, headerSize)
	emptyHdr := emptyBlk.NewBitCast(emptyVec, irtypes.NewPointer(vectorHdrType))
	emptyLenField := emptyBlk.NewGetElementPtr(vectorHdrType, emptyHdr, zero32, zero32)
	emptyBlk.NewStore(zero64, emptyLenField)
	emptyCapField := emptyBlk.NewGetElementPtr(vectorHdrType, emptyHdr, zero32, one32)
	emptyBlk.NewStore(zero64, emptyCapField)
	emptyBlk.NewRet(emptyVec)

	// Count loop: iterate until environ[i] == null
	countBody := fn.NewBlock(".count_body")
	countPhi := countHdr.NewPhi(ir.NewIncoming(zero64, entry))
	elemPtr := countHdr.NewGetElementPtr(irtypes.I8Ptr, environ, countPhi)
	elem := countHdr.NewLoad(irtypes.I8Ptr, elemPtr)
	elemIsNull := countHdr.NewICmp(enum.IPredEQ, elem, constant.NewNull(irtypes.I8Ptr))
	countDone := fn.NewBlock(".count_done")
	countHdr.NewCondBr(elemIsNull, countDone, countBody)

	countNext := countBody.NewAdd(countPhi, one64)
	countPhi.Incs = append(countPhi.Incs, ir.NewIncoming(countNext, countBody))
	countBody.NewBr(countHdr)

	// Allocate vector: header + count * ptrSize
	envCount := countDone.NewPhi(ir.NewIncoming(countPhi, countHdr))
	dataSize := countDone.NewMul(envCount, ptrSize)
	totalSize := countDone.NewAdd(headerSize, dataSize)
	rawSlice := countDone.NewCall(c.palAlloc, totalSize)

	// Store len and cap
	hdrPtr := countDone.NewBitCast(rawSlice, irtypes.NewPointer(vectorHdrType))
	lenField := countDone.NewGetElementPtr(vectorHdrType, hdrPtr, zero32, zero32)
	countDone.NewStore(envCount, lenField)
	capField := countDone.NewGetElementPtr(vectorHdrType, hdrPtr, zero32, one32)
	countDone.NewStore(envCount, capField)

	// Build loop: create strings from environ entries
	hasEntries := countDone.NewICmp(enum.IPredSGT, envCount, zero64)
	buildHdr := fn.NewBlock(".build_hdr")
	doneBlk := fn.NewBlock(".done")
	countDone.NewCondBr(hasEntries, buildHdr, doneBlk)

	buildBody := fn.NewBlock(".build_body")
	iPhi := buildHdr.NewPhi(ir.NewIncoming(zero64, countDone))
	buildCond := buildHdr.NewICmp(enum.IPredSLT, iPhi, envCount)
	buildHdr.NewCondBr(buildCond, buildBody, doneBlk)

	strlenFn := c.funcs["strlen"]
	envElemPtr := buildBody.NewGetElementPtr(irtypes.I8Ptr, environ, iPhi)
	envCStr := buildBody.NewLoad(irtypes.I8Ptr, envElemPtr)
	envLen := buildBody.NewCall(strlenFn, envCStr)
	envStr := buildBody.NewCall(c.funcs["promise_string_new"], envCStr, envLen)

	elemOff := buildBody.NewMul(iPhi, ptrSize)
	elemOff2 := buildBody.NewAdd(headerSize, elemOff)
	slotPtr := buildBody.NewGetElementPtr(irtypes.I8, rawSlice, elemOff2)
	slotPtrTyped := buildBody.NewBitCast(slotPtr, irtypes.NewPointer(irtypes.I8Ptr))
	buildBody.NewStore(envStr, slotPtrTyped)

	iNext := buildBody.NewAdd(iPhi, one64)
	iPhi.Incs = append(iPhi.Incs, ir.NewIncoming(iNext, buildBody))
	buildBody.NewBr(buildHdr)

	doneBlk.NewRet(rawSlice)
}

// defineGetUserNameBody: void @promise_os_get_user_name(i8* sret)
// Calls pal_get_user_info, extracts name, returns as Promise string.
func (c *Compiler) defineGetUserNameBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	outName := entry.NewAlloca(irtypes.I8Ptr)
	outDir := entry.NewAlloca(irtypes.I8Ptr)
	outUid := entry.NewAlloca(irtypes.I32)
	outGid := entry.NewAlloca(irtypes.I32)

	entry.NewCall(c.palGetUserInfo, outName, outDir, outUid, outGid)

	namePtr := entry.NewLoad(irtypes.I8Ptr, outName)
	isNull := entry.NewICmp(enum.IPredEQ, namePtr, constant.NewNull(irtypes.I8Ptr))
	foundBlk := fn.NewBlock(".found")
	emptyBlk := fn.NewBlock(".empty")
	entry.NewCondBr(isNull, emptyBlk, foundBlk)

	strlenFn := c.funcs["strlen"]
	nameLen := foundBlk.NewCall(strlenFn, namePtr)
	nameStr := foundBlk.NewCall(c.funcs["promise_string_new"], namePtr, nameLen)
	c.storeStringResult(foundBlk, sret, nameStr)
	foundBlk.NewRet(nil)

	emptyStr := emptyBlk.NewCall(c.funcs["promise_string_new"],
		constant.NewNull(irtypes.I8Ptr), constant.NewInt(irtypes.I64, 0))
	c.storeStringResult(emptyBlk, sret, emptyStr)
	emptyBlk.NewRet(nil)
}

// defineGetUserIdBody: void @promise_os_get_user_id(i8* sret)
// Calls pal_get_user_info, extracts uid, returns as Promise int.
// Uses ZExt (not SExt) because uid_t is unsigned.
func (c *Compiler) defineGetUserIdBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	outName := entry.NewAlloca(irtypes.I8Ptr)
	outDir := entry.NewAlloca(irtypes.I8Ptr)
	outUid := entry.NewAlloca(irtypes.I32)
	outGid := entry.NewAlloca(irtypes.I32)

	entry.NewCall(c.palGetUserInfo, outName, outDir, outUid, outGid)

	uid := entry.NewLoad(irtypes.I32, outUid)
	uidI64 := entry.NewZExt(uid, irtypes.I64)
	c.storeIntResult(entry, sret, uidI64)
	entry.NewRet(nil)
}

// defineGetGroupIdBody: void @promise_os_get_group_id(i8* sret)
// Calls pal_get_user_info, extracts gid, returns as Promise int.
// Uses ZExt (not SExt) because gid_t is unsigned.
func (c *Compiler) defineGetGroupIdBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	outName := entry.NewAlloca(irtypes.I8Ptr)
	outDir := entry.NewAlloca(irtypes.I8Ptr)
	outUid := entry.NewAlloca(irtypes.I32)
	outGid := entry.NewAlloca(irtypes.I32)

	entry.NewCall(c.palGetUserInfo, outName, outDir, outUid, outGid)

	gid := entry.NewLoad(irtypes.I32, outGid)
	gidI64 := entry.NewZExt(gid, irtypes.I64)
	c.storeIntResult(entry, sret, gidI64)
	entry.NewRet(nil)
}

// defineGetHomeDirBody: void @promise_os_get_home_dir(i8* sret)
// Calls pal_get_user_info, extracts home directory, returns as Promise string.
func (c *Compiler) defineGetHomeDirBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	outName := entry.NewAlloca(irtypes.I8Ptr)
	outDir := entry.NewAlloca(irtypes.I8Ptr)
	outUid := entry.NewAlloca(irtypes.I32)
	outGid := entry.NewAlloca(irtypes.I32)

	entry.NewCall(c.palGetUserInfo, outName, outDir, outUid, outGid)

	dirPtr := entry.NewLoad(irtypes.I8Ptr, outDir)
	isNull := entry.NewICmp(enum.IPredEQ, dirPtr, constant.NewNull(irtypes.I8Ptr))
	foundBlk := fn.NewBlock(".found")
	emptyBlk := fn.NewBlock(".empty")
	entry.NewCondBr(isNull, emptyBlk, foundBlk)

	strlenFn := c.funcs["strlen"]
	dirLen := foundBlk.NewCall(strlenFn, dirPtr)
	dirStr := foundBlk.NewCall(c.funcs["promise_string_new"], dirPtr, dirLen)
	c.storeStringResult(foundBlk, sret, dirStr)
	foundBlk.NewRet(nil)

	emptyStr := emptyBlk.NewCall(c.funcs["promise_string_new"],
		constant.NewNull(irtypes.I8Ptr), constant.NewInt(irtypes.I64, 0))
	c.storeStringResult(emptyBlk, sret, emptyStr)
	emptyBlk.NewRet(nil)
}

// defineGetHostnameBody: void @promise_os_get_hostname(i8* sret)
// Allocates a 256-byte buffer, calls pal_get_hostname, returns as Promise string.
func (c *Compiler) defineGetHostnameBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	bufSize := constant.NewInt(irtypes.I64, 256)
	buf := entry.NewCall(c.palAlloc, bufSize)

	result := entry.NewCall(c.palGetHostname, buf, bufSize)

	isNull := entry.NewICmp(enum.IPredEQ, result, constant.NewNull(irtypes.I8Ptr))
	successBlk := fn.NewBlock(".success")
	errorBlk := fn.NewBlock(".error")
	entry.NewCondBr(isNull, errorBlk, successBlk)

	strlenFn := c.funcs["strlen"]
	hostLen := successBlk.NewCall(strlenFn, result)
	hostStr := successBlk.NewCall(c.funcs["promise_string_new"], result, hostLen)
	successBlk.NewCall(c.palFree, buf)
	c.storeStringResult(successBlk, sret, hostStr)
	successBlk.NewRet(nil)

	errorBlk.NewCall(c.palFree, buf)
	emptyStr := errorBlk.NewCall(c.funcs["promise_string_new"],
		constant.NewNull(irtypes.I8Ptr), constant.NewInt(irtypes.I64, 0))
	c.storeStringResult(errorBlk, sret, emptyStr)
	errorBlk.NewRet(nil)
}

// Signal bridges

// defineSignalInitBody: void @promise_os_signal_init(i8* sret)
// Calls pal_signal_init(), stores rd_fd in global, returns rd_fd as Promise int.
// Idempotent: if already initialized (rd_fd >= 0), returns existing rd_fd.
func (c *Compiler) defineSignalInitBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	// Check if already initialized
	existingRdFd := entry.NewLoad(irtypes.I32, c.signalPipeRdFd)
	alreadyInit := entry.NewICmp(enum.IPredSGE, existingRdFd, constant.NewInt(irtypes.I32, 0))
	initBlk := fn.NewBlock(".init")
	doneBlk := fn.NewBlock(".done")
	entry.NewCondBr(alreadyInit, doneBlk, initBlk)

	// Already initialized: return existing rd_fd
	existingI64 := doneBlk.NewSExt(existingRdFd, irtypes.I64)
	c.storeIntResult(doneBlk, sret, existingI64)
	doneBlk.NewRet(nil)

	// Not initialized: call PAL
	rdFd := initBlk.NewCall(c.palSignalInit)
	initBlk.NewStore(rdFd, c.signalPipeRdFd)
	rdFdI64 := initBlk.NewSExt(rdFd, irtypes.I64)
	c.storeIntResult(initBlk, sret, rdFdI64)
	initBlk.NewRet(nil)
}

// defineSignalRegisterBody: void @promise_os_signal_register(i8* sret, i8* signum)
// Extracts signum, calls pal_signal_register, returns result as Promise int.
func (c *Compiler) defineSignalRegisterBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	sigRaw := c.extractRawInt(entry, fn.Params[1])
	sigI32 := entry.NewTrunc(sigRaw, irtypes.I32)

	rc := entry.NewCall(c.palSignalRegister, sigI32)
	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, sret, rcI64)
	entry.NewRet(nil)
}

// defineSignalReadBody: void @promise_os_signal_read(i8* sret, i8* ~buf)
// Reads from the signal pipe rd_fd into buf, with enter/exit_syscall.
// Returns bytes read as Promise int (0 = EOF/closed, negative = error).
func (c *Compiler) defineSignalReadBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	// Load the signal pipe rd_fd from global
	rdFd := entry.NewLoad(irtypes.I32, c.signalPipeRdFd)

	// Extract buf data pointer and length
	vecPtr := fn.Params[1]
	dataPtr, dataLen := extractVectorDataLen(entry, vecPtr)

	// Call pal_file_read with syscall handoff
	c.emitEnterSyscall(entry)
	n := entry.NewCall(c.palFileRead, rdFd, dataPtr, dataLen)
	c.emitExitSyscall(entry)

	c.storeIntResult(entry, sret, n)
	entry.NewRet(nil)
}

// defineSendSignalBody: void @promise_os_send_signal(i8* sret, i8* pid, i8* signum)
// Sends an arbitrary signal to a process. Returns 0 on success, -1 on error.
func (c *Compiler) defineSendSignalBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	pidRaw := c.extractRawInt(entry, fn.Params[1])
	pidI32 := entry.NewTrunc(pidRaw, irtypes.I32)

	sigRaw := c.extractRawInt(entry, fn.Params[2])
	sigI32 := entry.NewTrunc(sigRaw, irtypes.I32)

	rc := entry.NewCall(c.palKill, pidI32, sigI32)
	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, sret, rcI64)
	entry.NewRet(nil)
}

// defineGetPidBody: void @promise_os_get_pid(i8* sret)
// Returns the current process ID via getpid() (POSIX) or GetCurrentProcessId() (Windows).
func (c *Compiler) defineGetPidBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	// Windows uses GetCurrentProcessId (kernel32), POSIX uses getpid
	funcName := "getpid"
	if c.isWindows {
		funcName = "GetCurrentProcessId"
	}
	var getpidFn *ir.Func
	for _, f := range c.module.Funcs {
		if f.Name() == funcName {
			getpidFn = f
			break
		}
	}
	if getpidFn == nil {
		getpidFn = c.module.NewFunc(funcName, irtypes.I32)
		getpidFn.FuncAttrs = append(getpidFn.FuncAttrs, enum.FuncAttrNoUnwind)
	}
	pid := entry.NewCall(getpidFn)
	pidI64 := entry.NewSExt(pid, irtypes.I64)
	c.storeIntResult(entry, sret, pidI64)
	entry.NewRet(nil)
}

// Spawn with env/cwd bridges

// defineSpawnEnvBody: void @promise_os_spawn_env(i8* sret, i8* program, i8* args_vec, i8* env_entries_vec, i8* has_env_int, i8* cwd_str, i8* has_cwd_int)
// Like defineSpawnBody but with optional envp and cwd parameters.
// env_entries_vec is a string[] of "KEY=VALUE" entries. has_env is int (0=inherit, 1=use env_entries).
// cwd_str is a string path. has_cwd is int (0=inherit, 1=use cwd).
// Calls pal_spawn_env, caches stdout/stderr fds in TLS globals, returns int! (pid).
func (c *Compiler) defineSpawnEnvBody(fn *ir.Func) {
	zero32 := constant.NewInt(irtypes.I32, 0)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	ptrSize := constant.NewInt(irtypes.I64, int64(c.ptrSize()))
	headerSize := constant.NewInt(irtypes.I64, vectorHeaderSize)
	vectorHdrType := vectorHeaderType()
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)

	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	programParam := fn.Params[1]
	argsParam := fn.Params[2]       // string[] vector pointer (i8*)
	envEntriesParam := fn.Params[3] // string[] vector pointer (i8*)
	hasEnvParam := fn.Params[4]     // int value (0=inherit, 1=use env_entries)
	cwdParam := fn.Params[5]        // string value pointer (i8*)
	hasCwdParam := fn.Params[6]     // int value (0=inherit, 1=use cwd)

	// Compute failable result type: {i1, intValueType, i8*} for int!
	innerType := c.resolveType(types.TypInt)
	resultType := computeResultType(innerType)

	// Allocate storage for envp and cwd C pointers (use allocas to avoid complex phi merges)
	envpAlloca := entry.NewAlloca(i8PtrPtrType)
	envpRawAlloca := entry.NewAlloca(irtypes.I8Ptr) // for freeing the envp array
	cwdCStrAlloca := entry.NewAlloca(irtypes.I8Ptr)
	envCountAlloca := entry.NewAlloca(irtypes.I64) // for freeing env C strings
	entry.NewStore(constant.NewNull(i8PtrPtrType), envpAlloca)
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), envpRawAlloca)
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), cwdCStrAlloca)
	entry.NewStore(zero64, envCountAlloca)

	// Convert program string to C string
	programCStr := c.stringToCStr(entry, programParam)

	// Load vector length from header (masked — bit 63 is static flag)
	hdrPtr := entry.NewBitCast(argsParam, irtypes.NewPointer(vectorHdrType))
	argsCount := loadVectorLen(entry, hdrPtr)

	// Allocate argv array: (1 + argsCount + 1) * ptrSize
	totalSlots := entry.NewAdd(argsCount, constant.NewInt(irtypes.I64, 2))
	argvSize := entry.NewMul(totalSlots, ptrSize)
	argvRaw := entry.NewCall(c.palAlloc, argvSize)
	argv := entry.NewBitCast(argvRaw, i8PtrPtrType)

	// argv[0] = program C string
	argv0Ptr := entry.NewGetElementPtr(irtypes.I8Ptr, argv, zero64)
	entry.NewStore(programCStr, argv0Ptr)

	// Loop: convert each vector element to C string and store in argv
	hasArgs := entry.NewICmp(enum.IPredSGT, argsCount, zero64)
	loopHdr := fn.NewBlock(".argv_loop_hdr")
	loopDone := fn.NewBlock(".argv_loop_done")
	entry.NewCondBr(hasArgs, loopHdr, loopDone)

	loopBody := fn.NewBlock(".argv_loop_body")
	iPhi := loopHdr.NewPhi(ir.NewIncoming(zero64, entry))
	cond := loopHdr.NewICmp(enum.IPredSLT, iPhi, argsCount)
	loopHdr.NewCondBr(cond, loopBody, loopDone)

	elemOff := loopBody.NewMul(iPhi, ptrSize)
	elemOff2 := loopBody.NewAdd(headerSize, elemOff)
	elemPtr := loopBody.NewGetElementPtr(irtypes.I8, argsParam, elemOff2)
	elemPtrTyped := loopBody.NewBitCast(elemPtr, irtypes.NewPointer(irtypes.I8Ptr))
	strInst := loopBody.NewLoad(irtypes.I8Ptr, elemPtrTyped)
	argCStr := c.stringInstanceToCStr(loopBody, strInst)

	argIdx := loopBody.NewAdd(iPhi, one64)
	argvSlotPtr := loopBody.NewGetElementPtr(irtypes.I8Ptr, argv, argIdx)
	loopBody.NewStore(argCStr, argvSlotPtr)

	iNext := loopBody.NewAdd(iPhi, one64)
	iPhi.Incs = append(iPhi.Incs, ir.NewIncoming(iNext, loopBody))
	loopBody.NewBr(loopHdr)

	// Null terminator at argv[argsCount + 1]
	nullIdx := loopDone.NewAdd(argsCount, one64)
	nullSlotPtr := loopDone.NewGetElementPtr(irtypes.I8Ptr, argv, nullIdx)
	loopDone.NewStore(constant.NewNull(irtypes.I8Ptr), nullSlotPtr)

	// --- Build envp (if has_env != 0) ---
	hasEnvRaw := c.extractRawInt(loopDone, hasEnvParam)
	hasEnvBool := loopDone.NewICmp(enum.IPredNE, hasEnvRaw, zero64)
	buildEnvBlk := fn.NewBlock(".build_env")
	buildCwdBlk := fn.NewBlock(".build_cwd")
	loopDone.NewCondBr(hasEnvBool, buildEnvBlk, buildCwdBlk)

	// Build envp from env_entries vector
	envHdrPtr := buildEnvBlk.NewBitCast(envEntriesParam, irtypes.NewPointer(vectorHdrType))
	envCount := loadVectorLen(buildEnvBlk, envHdrPtr)
	buildEnvBlk.NewStore(envCount, envCountAlloca)
	envpSlots := buildEnvBlk.NewAdd(envCount, one64) // +1 for null terminator
	envpSize := buildEnvBlk.NewMul(envpSlots, ptrSize)
	envpRaw := buildEnvBlk.NewCall(c.palAlloc, envpSize)
	envp := buildEnvBlk.NewBitCast(envpRaw, i8PtrPtrType)
	buildEnvBlk.NewStore(envp, envpAlloca)
	buildEnvBlk.NewStore(envpRaw, envpRawAlloca)

	// Loop: convert env entries to C strings
	hasEnvEntries := buildEnvBlk.NewICmp(enum.IPredSGT, envCount, zero64)
	envLoopHdr := fn.NewBlock(".env_loop_hdr")
	envLoopDone := fn.NewBlock(".env_loop_done")
	buildEnvBlk.NewCondBr(hasEnvEntries, envLoopHdr, envLoopDone)

	envLoopBody := fn.NewBlock(".env_loop_body")
	envIPhi := envLoopHdr.NewPhi(ir.NewIncoming(zero64, buildEnvBlk))
	envCond := envLoopHdr.NewICmp(enum.IPredSLT, envIPhi, envCount)
	envLoopHdr.NewCondBr(envCond, envLoopBody, envLoopDone)

	envElemOff := envLoopBody.NewMul(envIPhi, ptrSize)
	envElemOff2 := envLoopBody.NewAdd(headerSize, envElemOff)
	envElemPtr := envLoopBody.NewGetElementPtr(irtypes.I8, envEntriesParam, envElemOff2)
	envElemPtrTyped := envLoopBody.NewBitCast(envElemPtr, irtypes.NewPointer(irtypes.I8Ptr))
	envStrInst := envLoopBody.NewLoad(irtypes.I8Ptr, envElemPtrTyped)
	envCStr := c.stringInstanceToCStr(envLoopBody, envStrInst)
	envSlotPtr := envLoopBody.NewGetElementPtr(irtypes.I8Ptr, envp, envIPhi)
	envLoopBody.NewStore(envCStr, envSlotPtr)

	envINext := envLoopBody.NewAdd(envIPhi, one64)
	envIPhi.Incs = append(envIPhi.Incs, ir.NewIncoming(envINext, envLoopBody))
	envLoopBody.NewBr(envLoopHdr)

	// Null terminator for envp
	envNullSlotPtr := envLoopDone.NewGetElementPtr(irtypes.I8Ptr, envp, envCount)
	envLoopDone.NewStore(constant.NewNull(irtypes.I8Ptr), envNullSlotPtr)
	envLoopDone.NewBr(buildCwdBlk)

	// --- Build cwd (if has_cwd != 0) ---
	hasCwdRaw := c.extractRawInt(buildCwdBlk, hasCwdParam)
	hasCwdBool := buildCwdBlk.NewICmp(enum.IPredNE, hasCwdRaw, zero64)
	buildCwdYes := fn.NewBlock(".build_cwd_yes")
	spawnBlk := fn.NewBlock(".spawn")
	buildCwdBlk.NewCondBr(hasCwdBool, buildCwdYes, spawnBlk)

	cwdCStr := c.stringToCStr(buildCwdYes, cwdParam)
	buildCwdYes.NewStore(cwdCStr, cwdCStrAlloca)
	buildCwdYes.NewBr(spawnBlk)

	// --- Spawn ---
	outStdoutFd := spawnBlk.NewAlloca(irtypes.I32)
	outStderrFd := spawnBlk.NewAlloca(irtypes.I32)
	finalEnvp := spawnBlk.NewLoad(i8PtrPtrType, envpAlloca)
	finalCwd := spawnBlk.NewLoad(irtypes.I8Ptr, cwdCStrAlloca)

	c.emitEnterSyscall(spawnBlk)
	spawnPid := spawnBlk.NewCall(c.palSpawnEnv, programCStr, argv, finalEnvp, finalCwd, outStdoutFd, outStderrFd)
	c.emitExitSyscall(spawnBlk)

	// Cache fds in TLS globals
	stdoutFd := spawnBlk.NewLoad(irtypes.I32, outStdoutFd)
	stderrFd := spawnBlk.NewLoad(irtypes.I32, outStderrFd)
	spawnBlk.NewStore(stdoutFd, c.spawnStdoutFd)
	spawnBlk.NewStore(stderrFd, c.spawnStderrFd)

	// --- Free env C strings (if envp was built) ---
	finalEnvp2 := spawnBlk.NewLoad(i8PtrPtrType, envpAlloca)
	envpIsNull := spawnBlk.NewICmp(enum.IPredEQ, finalEnvp2, constant.NewNull(i8PtrPtrType))
	freeEnvBlk := fn.NewBlock(".free_env")
	freeArgvBlk := fn.NewBlock(".free_argv")
	spawnBlk.NewCondBr(envpIsNull, freeArgvBlk, freeEnvBlk)

	// Free envp C strings loop
	savedEnvCount := freeEnvBlk.NewLoad(irtypes.I64, envCountAlloca)
	hasEnvFree := freeEnvBlk.NewICmp(enum.IPredSGT, savedEnvCount, zero64)
	envFreeLoopHdr := fn.NewBlock(".env_free_loop_hdr")
	envFreeDone := fn.NewBlock(".env_free_done")
	freeEnvBlk.NewCondBr(hasEnvFree, envFreeLoopHdr, envFreeDone)

	envFreeLoopBody := fn.NewBlock(".env_free_loop_body")
	envJPhi := envFreeLoopHdr.NewPhi(ir.NewIncoming(zero64, freeEnvBlk))
	envFreeCond := envFreeLoopHdr.NewICmp(enum.IPredSLT, envJPhi, savedEnvCount)
	envFreeLoopHdr.NewCondBr(envFreeCond, envFreeLoopBody, envFreeDone)

	envFreeSlotPtr := envFreeLoopBody.NewGetElementPtr(irtypes.I8Ptr, finalEnvp2, envJPhi)
	envFreeStr := envFreeLoopBody.NewLoad(irtypes.I8Ptr, envFreeSlotPtr)
	envFreeLoopBody.NewCall(c.palFree, envFreeStr)
	envJNext := envFreeLoopBody.NewAdd(envJPhi, one64)
	envJPhi.Incs = append(envJPhi.Incs, ir.NewIncoming(envJNext, envFreeLoopBody))
	envFreeLoopBody.NewBr(envFreeLoopHdr)

	// Free the envp array itself
	savedEnvpRaw := envFreeDone.NewLoad(irtypes.I8Ptr, envpRawAlloca)
	envFreeDone.NewCall(c.palFree, savedEnvpRaw)
	envFreeDone.NewBr(freeArgvBlk)

	// --- Free cwd C string (if built) ---
	finalCwd2 := freeArgvBlk.NewLoad(irtypes.I8Ptr, cwdCStrAlloca)
	cwdIsNull := freeArgvBlk.NewICmp(enum.IPredEQ, finalCwd2, constant.NewNull(irtypes.I8Ptr))
	freeCwdBlk := fn.NewBlock(".free_cwd")
	freeArgsBlk := fn.NewBlock(".free_args")
	freeArgvBlk.NewCondBr(cwdIsNull, freeArgsBlk, freeCwdBlk)

	freeCwdBlk.NewCall(c.palFree, finalCwd2)
	freeCwdBlk.NewBr(freeArgsBlk)

	// --- Free argv C strings: argv[1..argsCount] ---
	hasFreeArgs := freeArgsBlk.NewICmp(enum.IPredSGT, argsCount, zero64)
	freeLoopHdr := fn.NewBlock(".free_loop_hdr")
	freeDone := fn.NewBlock(".free_done")
	freeArgsBlk.NewCondBr(hasFreeArgs, freeLoopHdr, freeDone)

	freeLoopBody := fn.NewBlock(".free_loop_body")
	jPhi := freeLoopHdr.NewPhi(ir.NewIncoming(zero64, freeArgsBlk))
	freeCond := freeLoopHdr.NewICmp(enum.IPredSLT, jPhi, argsCount)
	freeLoopHdr.NewCondBr(freeCond, freeLoopBody, freeDone)

	freeIdx := freeLoopBody.NewAdd(jPhi, one64)
	freeSlotPtr := freeLoopBody.NewGetElementPtr(irtypes.I8Ptr, argv, freeIdx)
	freeStr := freeLoopBody.NewLoad(irtypes.I8Ptr, freeSlotPtr)
	freeLoopBody.NewCall(c.palFree, freeStr)
	jNext := freeLoopBody.NewAdd(jPhi, one64)
	jPhi.Incs = append(jPhi.Incs, ir.NewIncoming(jNext, freeLoopBody))
	freeLoopBody.NewBr(freeLoopHdr)

	// Free program C string and argv array
	freeDone.NewCall(c.palFree, programCStr)
	freeDone.NewCall(c.palFree, argvRaw)

	// Check result: -1 means error
	isErr := freeDone.NewICmp(enum.IPredSLT, spawnPid, zero32)
	successBlk := fn.NewBlock(".success")
	errorBlk := fn.NewBlock(".error")
	freeDone.NewCondBr(isErr, errorBlk, successBlk)

	// Success: store pid as i64 failable success
	pidI64 := successBlk.NewSExt(spawnPid, irtypes.I64)
	c.storeFailableSuccess(successBlk, sret, pidI64, resultType)
	successBlk.NewRet(nil)

	// Error: construct error, store failable error
	errInst := c.constructErrorFromGlobalStr(errorBlk, "failed to spawn process")
	c.storeFailableError(errorBlk, sret, errInst, resultType)
	errorBlk.NewRet(nil)
}

// defineSpawnStreamingEnvBody: void @promise_os_spawn_streaming_env(i8* sret, i8* program, i8* args_vec, i8* env_entries_vec, i8* has_env_int, i8* cwd_str, i8* has_cwd_int)
// Like defineSpawnStreamingBody but with optional envp and cwd parameters.
// env_entries_vec is a string[] of "KEY=VALUE" entries. has_env is int (0=inherit, 1=use env_entries).
// cwd_str is a string path. has_cwd is int (0=inherit, 1=use cwd).
// Calls pal_spawn_streaming_env, caches all 3 pipe fds in TLS globals, returns int! (pid).
func (c *Compiler) defineSpawnStreamingEnvBody(fn *ir.Func) {
	zero32 := constant.NewInt(irtypes.I32, 0)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	ptrSize := constant.NewInt(irtypes.I64, int64(c.ptrSize()))
	headerSize := constant.NewInt(irtypes.I64, vectorHeaderSize)
	vectorHdrType := vectorHeaderType()
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)

	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	programParam := fn.Params[1]
	argsParam := fn.Params[2]       // string[] vector pointer (i8*)
	envEntriesParam := fn.Params[3] // string[] vector pointer (i8*)
	hasEnvParam := fn.Params[4]     // int value (0=inherit, 1=use env_entries)
	cwdParam := fn.Params[5]        // string value pointer (i8*)
	hasCwdParam := fn.Params[6]     // int value (0=inherit, 1=use cwd)

	// Compute failable result type: {i1, intValueType, i8*} for int!
	innerType := c.resolveType(types.TypInt)
	resultType := computeResultType(innerType)

	// Allocate storage for envp and cwd C pointers (use allocas to avoid complex phi merges)
	envpAlloca := entry.NewAlloca(i8PtrPtrType)
	envpRawAlloca := entry.NewAlloca(irtypes.I8Ptr) // for freeing the envp array
	cwdCStrAlloca := entry.NewAlloca(irtypes.I8Ptr)
	envCountAlloca := entry.NewAlloca(irtypes.I64) // for freeing env C strings
	entry.NewStore(constant.NewNull(i8PtrPtrType), envpAlloca)
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), envpRawAlloca)
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), cwdCStrAlloca)
	entry.NewStore(zero64, envCountAlloca)

	// Convert program string to C string
	programCStr := c.stringToCStr(entry, programParam)

	// Load vector length from header (masked — bit 63 is static flag)
	hdrPtr := entry.NewBitCast(argsParam, irtypes.NewPointer(vectorHdrType))
	argsCount := loadVectorLen(entry, hdrPtr)

	// Allocate argv array: (1 + argsCount + 1) * ptrSize
	totalSlots := entry.NewAdd(argsCount, constant.NewInt(irtypes.I64, 2))
	argvSize := entry.NewMul(totalSlots, ptrSize)
	argvRaw := entry.NewCall(c.palAlloc, argvSize)
	argv := entry.NewBitCast(argvRaw, i8PtrPtrType)

	// argv[0] = program C string
	argv0Ptr := entry.NewGetElementPtr(irtypes.I8Ptr, argv, zero64)
	entry.NewStore(programCStr, argv0Ptr)

	// Loop: convert each vector element to C string and store in argv
	hasArgs := entry.NewICmp(enum.IPredSGT, argsCount, zero64)
	loopHdr := fn.NewBlock(".argv_loop_hdr")
	loopDone := fn.NewBlock(".argv_loop_done")
	entry.NewCondBr(hasArgs, loopHdr, loopDone)

	loopBody := fn.NewBlock(".argv_loop_body")
	iPhi := loopHdr.NewPhi(ir.NewIncoming(zero64, entry))
	cond := loopHdr.NewICmp(enum.IPredSLT, iPhi, argsCount)
	loopHdr.NewCondBr(cond, loopBody, loopDone)

	elemOff := loopBody.NewMul(iPhi, ptrSize)
	elemOff2 := loopBody.NewAdd(headerSize, elemOff)
	elemPtr := loopBody.NewGetElementPtr(irtypes.I8, argsParam, elemOff2)
	elemPtrTyped := loopBody.NewBitCast(elemPtr, irtypes.NewPointer(irtypes.I8Ptr))
	strInst := loopBody.NewLoad(irtypes.I8Ptr, elemPtrTyped)
	argCStr := c.stringInstanceToCStr(loopBody, strInst)

	argIdx := loopBody.NewAdd(iPhi, one64)
	argvSlotPtr := loopBody.NewGetElementPtr(irtypes.I8Ptr, argv, argIdx)
	loopBody.NewStore(argCStr, argvSlotPtr)

	iNext := loopBody.NewAdd(iPhi, one64)
	iPhi.Incs = append(iPhi.Incs, ir.NewIncoming(iNext, loopBody))
	loopBody.NewBr(loopHdr)

	// Null terminator at argv[argsCount + 1]
	nullIdx := loopDone.NewAdd(argsCount, one64)
	nullSlotPtr := loopDone.NewGetElementPtr(irtypes.I8Ptr, argv, nullIdx)
	loopDone.NewStore(constant.NewNull(irtypes.I8Ptr), nullSlotPtr)

	// --- Build envp (if has_env != 0) ---
	hasEnvRaw := c.extractRawInt(loopDone, hasEnvParam)
	hasEnvBool := loopDone.NewICmp(enum.IPredNE, hasEnvRaw, zero64)
	buildEnvBlk := fn.NewBlock(".build_env")
	buildCwdBlk := fn.NewBlock(".build_cwd")
	loopDone.NewCondBr(hasEnvBool, buildEnvBlk, buildCwdBlk)

	// Build envp from env_entries vector
	envHdrPtr := buildEnvBlk.NewBitCast(envEntriesParam, irtypes.NewPointer(vectorHdrType))
	envCount := loadVectorLen(buildEnvBlk, envHdrPtr)
	buildEnvBlk.NewStore(envCount, envCountAlloca)
	envpSlots := buildEnvBlk.NewAdd(envCount, one64) // +1 for null terminator
	envpSize := buildEnvBlk.NewMul(envpSlots, ptrSize)
	envpRaw := buildEnvBlk.NewCall(c.palAlloc, envpSize)
	envp := buildEnvBlk.NewBitCast(envpRaw, i8PtrPtrType)
	buildEnvBlk.NewStore(envp, envpAlloca)
	buildEnvBlk.NewStore(envpRaw, envpRawAlloca)

	// Loop: convert env entries to C strings
	hasEnvEntries := buildEnvBlk.NewICmp(enum.IPredSGT, envCount, zero64)
	envLoopHdr := fn.NewBlock(".env_loop_hdr")
	envLoopDone := fn.NewBlock(".env_loop_done")
	buildEnvBlk.NewCondBr(hasEnvEntries, envLoopHdr, envLoopDone)

	envLoopBody := fn.NewBlock(".env_loop_body")
	envIPhi := envLoopHdr.NewPhi(ir.NewIncoming(zero64, buildEnvBlk))
	envCond := envLoopHdr.NewICmp(enum.IPredSLT, envIPhi, envCount)
	envLoopHdr.NewCondBr(envCond, envLoopBody, envLoopDone)

	envElemOff := envLoopBody.NewMul(envIPhi, ptrSize)
	envElemOff2 := envLoopBody.NewAdd(headerSize, envElemOff)
	envElemPtr := envLoopBody.NewGetElementPtr(irtypes.I8, envEntriesParam, envElemOff2)
	envElemPtrTyped := envLoopBody.NewBitCast(envElemPtr, irtypes.NewPointer(irtypes.I8Ptr))
	envStrInst := envLoopBody.NewLoad(irtypes.I8Ptr, envElemPtrTyped)
	envCStr := c.stringInstanceToCStr(envLoopBody, envStrInst)
	envSlotPtr := envLoopBody.NewGetElementPtr(irtypes.I8Ptr, envp, envIPhi)
	envLoopBody.NewStore(envCStr, envSlotPtr)

	envINext := envLoopBody.NewAdd(envIPhi, one64)
	envIPhi.Incs = append(envIPhi.Incs, ir.NewIncoming(envINext, envLoopBody))
	envLoopBody.NewBr(envLoopHdr)

	// Null terminator for envp
	envNullSlotPtr := envLoopDone.NewGetElementPtr(irtypes.I8Ptr, envp, envCount)
	envLoopDone.NewStore(constant.NewNull(irtypes.I8Ptr), envNullSlotPtr)
	envLoopDone.NewBr(buildCwdBlk)

	// --- Build cwd (if has_cwd != 0) ---
	hasCwdRaw := c.extractRawInt(buildCwdBlk, hasCwdParam)
	hasCwdBool := buildCwdBlk.NewICmp(enum.IPredNE, hasCwdRaw, zero64)
	buildCwdYes := fn.NewBlock(".build_cwd_yes")
	spawnBlk := fn.NewBlock(".spawn")
	buildCwdBlk.NewCondBr(hasCwdBool, buildCwdYes, spawnBlk)

	cwdCStr := c.stringToCStr(buildCwdYes, cwdParam)
	buildCwdYes.NewStore(cwdCStr, cwdCStrAlloca)
	buildCwdYes.NewBr(spawnBlk)

	// --- Spawn ---
	outStdinFd := spawnBlk.NewAlloca(irtypes.I32)
	outStdoutFd := spawnBlk.NewAlloca(irtypes.I32)
	outStderrFd := spawnBlk.NewAlloca(irtypes.I32)
	finalEnvp := spawnBlk.NewLoad(i8PtrPtrType, envpAlloca)
	finalCwd := spawnBlk.NewLoad(irtypes.I8Ptr, cwdCStrAlloca)

	c.emitEnterSyscall(spawnBlk)
	spawnPid := spawnBlk.NewCall(c.palSpawnStreamingEnv, programCStr, argv, finalEnvp, finalCwd, outStdinFd, outStdoutFd, outStderrFd)
	c.emitExitSyscall(spawnBlk)

	// Cache all 3 fds in TLS globals
	stdinFd := spawnBlk.NewLoad(irtypes.I32, outStdinFd)
	stdoutFd := spawnBlk.NewLoad(irtypes.I32, outStdoutFd)
	stderrFd := spawnBlk.NewLoad(irtypes.I32, outStderrFd)
	spawnBlk.NewStore(stdinFd, c.spawnStdinFd)
	spawnBlk.NewStore(stdoutFd, c.spawnStdoutFd)
	spawnBlk.NewStore(stderrFd, c.spawnStderrFd)

	// --- Free env C strings (if envp was built) ---
	finalEnvp2 := spawnBlk.NewLoad(i8PtrPtrType, envpAlloca)
	envpIsNull := spawnBlk.NewICmp(enum.IPredEQ, finalEnvp2, constant.NewNull(i8PtrPtrType))
	freeEnvBlk := fn.NewBlock(".free_env")
	freeArgvBlk := fn.NewBlock(".free_argv")
	spawnBlk.NewCondBr(envpIsNull, freeArgvBlk, freeEnvBlk)

	// Free envp C strings loop
	savedEnvCount := freeEnvBlk.NewLoad(irtypes.I64, envCountAlloca)
	hasEnvFree := freeEnvBlk.NewICmp(enum.IPredSGT, savedEnvCount, zero64)
	envFreeLoopHdr := fn.NewBlock(".env_free_loop_hdr")
	envFreeDone := fn.NewBlock(".env_free_done")
	freeEnvBlk.NewCondBr(hasEnvFree, envFreeLoopHdr, envFreeDone)

	envFreeLoopBody := fn.NewBlock(".env_free_loop_body")
	envJPhi := envFreeLoopHdr.NewPhi(ir.NewIncoming(zero64, freeEnvBlk))
	envFreeCond := envFreeLoopHdr.NewICmp(enum.IPredSLT, envJPhi, savedEnvCount)
	envFreeLoopHdr.NewCondBr(envFreeCond, envFreeLoopBody, envFreeDone)

	envFreeSlotPtr := envFreeLoopBody.NewGetElementPtr(irtypes.I8Ptr, finalEnvp2, envJPhi)
	envFreeStr := envFreeLoopBody.NewLoad(irtypes.I8Ptr, envFreeSlotPtr)
	envFreeLoopBody.NewCall(c.palFree, envFreeStr)
	envJNext := envFreeLoopBody.NewAdd(envJPhi, one64)
	envJPhi.Incs = append(envJPhi.Incs, ir.NewIncoming(envJNext, envFreeLoopBody))
	envFreeLoopBody.NewBr(envFreeLoopHdr)

	// Free the envp array itself
	savedEnvpRaw := envFreeDone.NewLoad(irtypes.I8Ptr, envpRawAlloca)
	envFreeDone.NewCall(c.palFree, savedEnvpRaw)
	envFreeDone.NewBr(freeArgvBlk)

	// --- Free cwd C string (if built) ---
	finalCwd2 := freeArgvBlk.NewLoad(irtypes.I8Ptr, cwdCStrAlloca)
	cwdIsNull := freeArgvBlk.NewICmp(enum.IPredEQ, finalCwd2, constant.NewNull(irtypes.I8Ptr))
	freeCwdBlk := fn.NewBlock(".free_cwd")
	freeArgsBlk := fn.NewBlock(".free_args")
	freeArgvBlk.NewCondBr(cwdIsNull, freeArgsBlk, freeCwdBlk)

	freeCwdBlk.NewCall(c.palFree, finalCwd2)
	freeCwdBlk.NewBr(freeArgsBlk)

	// --- Free argv C strings: argv[1..argsCount] ---
	hasFreeArgs := freeArgsBlk.NewICmp(enum.IPredSGT, argsCount, zero64)
	freeLoopHdr := fn.NewBlock(".free_loop_hdr")
	freeDone := fn.NewBlock(".free_done")
	freeArgsBlk.NewCondBr(hasFreeArgs, freeLoopHdr, freeDone)

	freeLoopBody := fn.NewBlock(".free_loop_body")
	jPhi := freeLoopHdr.NewPhi(ir.NewIncoming(zero64, freeArgsBlk))
	freeCond := freeLoopHdr.NewICmp(enum.IPredSLT, jPhi, argsCount)
	freeLoopHdr.NewCondBr(freeCond, freeLoopBody, freeDone)

	freeIdx := freeLoopBody.NewAdd(jPhi, one64)
	freeSlotPtr := freeLoopBody.NewGetElementPtr(irtypes.I8Ptr, argv, freeIdx)
	freeStr := freeLoopBody.NewLoad(irtypes.I8Ptr, freeSlotPtr)
	freeLoopBody.NewCall(c.palFree, freeStr)
	jNext := freeLoopBody.NewAdd(jPhi, one64)
	jPhi.Incs = append(jPhi.Incs, ir.NewIncoming(jNext, freeLoopBody))
	freeLoopBody.NewBr(freeLoopHdr)

	// Free program C string and argv array
	freeDone.NewCall(c.palFree, programCStr)
	freeDone.NewCall(c.palFree, argvRaw)

	// Check result: -1 means error
	isErr := freeDone.NewICmp(enum.IPredSLT, spawnPid, zero32)
	successBlk := fn.NewBlock(".success")
	errorBlk := fn.NewBlock(".error")
	freeDone.NewCondBr(isErr, errorBlk, successBlk)

	// Success: store pid as i64 failable success
	pidI64 := successBlk.NewSExt(spawnPid, irtypes.I64)
	c.storeFailableSuccess(successBlk, sret, pidI64, resultType)
	successBlk.NewRet(nil)

	// Error: construct error, store failable error
	errInst := c.constructErrorFromGlobalStr(errorBlk, "failed to spawn process")
	c.storeFailableError(errorBlk, sret, errInst, resultType)
	errorBlk.NewRet(nil)
}
