package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"

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
