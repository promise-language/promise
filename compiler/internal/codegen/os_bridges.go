package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/types"
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

	// Load vector length from header
	hdrPtr := entry.NewBitCast(argsParam, irtypes.NewPointer(vectorHdrType))
	lenField := entry.NewGetElementPtr(vectorHdrType, hdrPtr, zero32, zero32)
	argsCount := entry.NewLoad(irtypes.I64, lenField)

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
