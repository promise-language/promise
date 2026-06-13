package pal

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// WasmPAL implements PAL for WebAssembly using WASI (WebAssembly System Interface).
type WasmPAL struct {
	DebugAllocator        bool // scribble malloc'd (0xAA) + poison freed (0xDE) memory for UAF / uninit-read detection
	MemoryLimitAccounting bool // T0689: enable @__promise_memory_used_bytes accounting + memory-limit abort
	// WebTarget selects the abort-path imports for the debug allocator's
	// fatal-print sequence: false → WASI (wasm_snapshot_preview1.fd_write +
	// .proc_exit), true → JS-provided promise_env.write + .exit. Set by
	// WasmWebPAL when it delegates Alloc/Free/Realloc emission to WasmPAL.
	WebTarget bool
}

// EmitWrite declares WASI fd_write and defines @pal_write.
// Builds a ciovec {i8*, i32} on the stack and calls fd_write with a single iovec.
// Signature: @pal_write(i32 %fd, i8* %buf, i64 %len) → i64
func (p *WasmPAL) EmitWrite(module *ir.Module) *ir.Func {
	// ciovec struct type: {i8*, i32} (buffer pointer + buffer length)
	ciovecType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I32)

	// declare i32 @fd_write(i32, {i8*, i32}*, i32, i32*)
	fdWrite := getOrDeclareFunc(module, "fd_write", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("iovs", irtypes.NewPointer(ciovecType)),
		ir.NewParam("iovs_len", irtypes.I32),
		ir.NewParam("nwritten", irtypes.NewPointer(irtypes.I32)))
	fdWrite.FuncAttrs = append(fdWrite.FuncAttrs,
		ir.AttrPair{Key: "wasm-import-module", Value: "wasi_snapshot_preview1"},
		ir.AttrPair{Key: "wasm-import-name", Value: "fd_write"})

	// define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)
	fn := module.NewFunc("pal_write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	entry := fn.NewBlock(".entry")

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
	procExit := getOrDeclareFunc(module, "proc_exit", irtypes.Void,
		ir.NewParam("rval", irtypes.I32))
	addFuncAttr(procExit, enum.FuncAttrNoReturn)
	procExit.FuncAttrs = append(procExit.FuncAttrs,
		ir.AttrPair{Key: "wasm-import-module", Value: "wasi_snapshot_preview1"},
		ir.AttrPair{Key: "wasm-import-name", Value: "proc_exit"})

	// define void @pal_exit(i32 %code) noreturn
	fn := module.NewFunc("pal_exit", irtypes.Void,
		ir.NewParam("code", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
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
// Includes allocation count tracking for leak detection (T0020).
func (p *WasmPAL) EmitAlloc(module *ir.Module) *ir.Func {
	if p.DebugAllocator {
		return emitWasmAllocDebug(module, p.MemoryLimitAccounting, p.WebTarget)
	}

	// declare noalias i8* @malloc(i32 noundef) nounwind
	mallocSize := ir.NewParam("size", irtypes.I32)
	mallocSize.Attrs = append(mallocSize.Attrs, enum.ParamAttrNoUndef)
	mallocFn := getOrDeclareFunc(module, "malloc", irtypes.I8Ptr, mallocSize)
	mallocFn.ReturnAttrs = append(mallocFn.ReturnAttrs, enum.ReturnAttrNoAlias)
	addFuncAttr(mallocFn, enum.FuncAttrWillReturn)

	allocCount := getOrCreateAllocCountGlobal(module)

	// define noalias i8* @pal_alloc(i64 %size) nounwind
	fn := module.NewFunc("pal_alloc", irtypes.I8Ptr,
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")

	// Truncate i64 to i32 (wasm32 address space)
	size32 := entry.NewTrunc(fn.Params[0], irtypes.I32)
	ret := entry.NewCall(mallocFn, size32)

	// Track: if malloc returned non-null, increment alloc count (non-atomic, WASM is single-threaded)
	nonnull := entry.NewICmp(enum.IPredNE, ret, constant.NewNull(irtypes.I8Ptr))
	trackBlk := fn.NewBlock(".track")
	doneBlk := fn.NewBlock(".done")
	entry.NewCondBr(nonnull, trackBlk, doneBlk)

	old := trackBlk.NewLoad(irtypes.I64, allocCount)
	trackBlk.NewStore(trackBlk.NewAdd(old, constant.NewInt(irtypes.I64, 1)), allocCount)
	trackBlk.NewBr(doneBlk)

	doneBlk.NewRet(ret)

	return fn
}

// EmitWasmDebugAbort is the exported alias of emitWasmDebugAbortCall for
// wasm32-wasi (WASI imports). For wasm32-web, use EmitWasmWebDebugAbort.
func EmitWasmDebugAbort(module *ir.Module, blk *ir.Block, key, msg string) {
	emitWasmDebugAbortCall(module, blk, key, msg, false)
}

// EmitWasmWebDebugAbort is the wasm32-web variant — fatal-print + exit via the
// JS-provided promise_env.write / promise_env.exit imports instead of WASI.
// Suitable for use as a MemoryLimitConfig.EmitAbort callback on wasm-web.
func EmitWasmWebDebugAbort(module *ir.Module, blk *ir.Block, key, msg string) {
	emitWasmDebugAbortCall(module, blk, key, msg, true)
}

// emitWasmDebugAbortCall emits a write(2, msg) + exit(134) + unreachable
// sequence into blk for the WASM debug allocator (T0365). webTarget selects
// JS-provided promise_env imports (wasm32-web) over WASI imports
// (wasm32-wasi). Each call site uses its own private message global.
//
// On wasm32-wasi: writes via fd_write (ciovec built on stack), exits via
// proc_exit. Both imported from wasi_snapshot_preview1.
//
// On wasm32-web: writes via promise_env.write (raw fd/buf/len, same shape
// as pal_write), exits via promise_env.exit. The JS harness handles routing
// to console.error and process termination.
func emitWasmDebugAbortCall(module *ir.Module, blk *ir.Block, key, msg string, webTarget bool) {
	g := getOrCreateDebugMsgGlobal(module, "wasm_"+key, msg)
	msgPtr := blk.NewBitCast(g, irtypes.I8Ptr)
	stderrFd := constant.NewInt(irtypes.I32, 2)

	if webTarget {
		// promise_env.write(fd, buf, len) → i64 ; promise_env.exit(code) → !void
		envWrite := getOrDeclareFunc(module, "promise_env_write", irtypes.I64,
			ir.NewParam("fd", irtypes.I32),
			ir.NewParam("buf", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		envWrite.FuncAttrs = append(envWrite.FuncAttrs,
			ir.AttrPair{Key: "wasm-import-module", Value: "promise_env"},
			ir.AttrPair{Key: "wasm-import-name", Value: "write"})

		envExit := getOrDeclareFunc(module, "promise_env_exit", irtypes.Void,
			ir.NewParam("code", irtypes.I32))
		addFuncAttr(envExit, enum.FuncAttrNoReturn)
		envExit.FuncAttrs = append(envExit.FuncAttrs,
			ir.AttrPair{Key: "wasm-import-module", Value: "promise_env"},
			ir.AttrPair{Key: "wasm-import-name", Value: "exit"})

		blk.NewCall(envWrite, stderrFd, msgPtr, constant.NewInt(irtypes.I64, int64(len(msg))))
		blk.NewCall(envExit, constant.NewInt(irtypes.I32, 134))
		blk.NewUnreachable()
		return
	}

	// wasm32-wasi: fd_write with a ciovec, then proc_exit.
	ciovecType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I32)
	fdWrite := getOrDeclareFunc(module, "fd_write", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("iovs", irtypes.NewPointer(ciovecType)),
		ir.NewParam("iovs_len", irtypes.I32),
		ir.NewParam("nwritten", irtypes.NewPointer(irtypes.I32)))
	fdWrite.FuncAttrs = append(fdWrite.FuncAttrs,
		ir.AttrPair{Key: "wasm-import-module", Value: "wasi_snapshot_preview1"},
		ir.AttrPair{Key: "wasm-import-name", Value: "fd_write"})

	procExit := getOrDeclareFunc(module, "proc_exit", irtypes.Void,
		ir.NewParam("rval", irtypes.I32))
	addFuncAttr(procExit, enum.FuncAttrNoReturn)
	procExit.FuncAttrs = append(procExit.FuncAttrs,
		ir.AttrPair{Key: "wasm-import-module", Value: "wasi_snapshot_preview1"},
		ir.AttrPair{Key: "wasm-import-name", Value: "proc_exit"})

	iov := blk.NewAlloca(ciovecType)
	iovBufField := blk.NewGetElementPtr(ciovecType, iov,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	blk.NewStore(msgPtr, iovBufField)
	iovLenField := blk.NewGetElementPtr(ciovecType, iov,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	blk.NewStore(constant.NewInt(irtypes.I32, int64(len(msg))), iovLenField)

	nwrittenPtr := blk.NewAlloca(irtypes.I32)
	blk.NewCall(fdWrite, stderrFd, iov,
		constant.NewInt(irtypes.I32, 1), nwrittenPtr)
	blk.NewCall(procExit, constant.NewInt(irtypes.I32, 134))
	blk.NewUnreachable()
}

// emitHeaderValidationWasm validates the debug allocator header at userPtr on
// WASM (i32 internal sizes, i64 magic words). Mirrors emitHeaderValidationLibc
// — checks MAGIC_FREED at BOTH offset 0 and offset 8 to make double-free
// detection survive offset-0 reuse by the wasm allocator.
//
// webTarget=true selects promise_env.write+exit imports for the fatal-abort
// paths (wasm32-web); false uses WASI fd_write+proc_exit (wasm32-wasi).
func emitHeaderValidationWasm(
	module *ir.Module,
	fn *ir.Func, blk *ir.Block, userPtr value.Value,
	webTarget bool,
) (sizeVal value.Value, headerPtr value.Value, validatedBlk *ir.Block) {
	negHeader := constant.NewInt(irtypes.I32, -debugHeaderSize)
	hdrPtr := blk.NewGetElementPtr(irtypes.I8, userPtr, negHeader)

	magicSlot := blk.NewBitCast(hdrPtr, irtypes.NewPointer(irtypes.I64))
	magic := blk.NewLoad(irtypes.I64, magicSlot)
	sizeFieldPtr := blk.NewGetElementPtr(irtypes.I64, magicSlot, constant.NewInt(irtypes.I32, 1))
	sizeOrFreedTag := blk.NewLoad(irtypes.I64, sizeFieldPtr)

	freedMagic := constant.NewInt(irtypes.I64, debugMagicFreed)
	aliveMagic := constant.NewInt(irtypes.I64, debugMagicAlive)
	tailMagic := constant.NewInt(irtypes.I64, debugMagicTail)

	aliveBlk := fn.NewBlock(".dbg.header_alive_check")
	checkSecondaryFreedBlk := fn.NewBlock(".dbg.check_secondary_freed")
	freedAbortBlk := fn.NewBlock(".dbg.double_free")
	badMagicAbortBlk := fn.NewBlock(".dbg.bad_magic")
	tailOkBlk := fn.NewBlock(".dbg.tail_check")
	tailAbortBlk := fn.NewBlock(".dbg.tail_corrupt")
	postValidatedBlk := fn.NewBlock(".dbg.validated")

	isFreed := blk.NewICmp(enum.IPredEQ, magic, freedMagic)
	blk.NewCondBr(isFreed, freedAbortBlk, aliveBlk)

	isAlive := aliveBlk.NewICmp(enum.IPredEQ, magic, aliveMagic)
	aliveBlk.NewCondBr(isAlive, tailOkBlk, checkSecondaryFreedBlk)

	isSecondaryFreed := checkSecondaryFreedBlk.NewICmp(enum.IPredEQ, sizeOrFreedTag, freedMagic)
	checkSecondaryFreedBlk.NewCondBr(isSecondaryFreed, freedAbortBlk, badMagicAbortBlk)

	tailBytePtr := tailOkBlk.NewGetElementPtr(irtypes.I8, userPtr, sizeOrFreedTag)
	tailSlot := tailOkBlk.NewBitCast(tailBytePtr, irtypes.NewPointer(irtypes.I64))
	tailLoad := tailOkBlk.NewLoad(irtypes.I64, tailSlot)
	tailLoad.Align = 1
	tailOk := tailOkBlk.NewICmp(enum.IPredEQ, tailLoad, tailMagic)
	tailOkBlk.NewCondBr(tailOk, postValidatedBlk, tailAbortBlk)

	emitWasmDebugAbortCall(module, freedAbortBlk, "double_free",
		"fatal: double free\n", webTarget)
	emitWasmDebugAbortCall(module, badMagicAbortBlk, "bad_magic",
		"fatal: invalid free (bad header magic)\n", webTarget)
	emitWasmDebugAbortCall(module, tailAbortBlk, "tail_corrupt",
		"fatal: heap corruption (tail sentinel mismatch)\n", webTarget)

	return sizeOrFreedTag, hdrPtr, postValidatedBlk
}

// emitWasmAllocDebug defines @pal_alloc with the T0365 sentinel-header debug
// allocator for WASM. Adds a 16-byte header + 8-byte tail (24 bytes total
// overhead) per allocation. WASM uses i32 internal sizes; magic words remain
// i64 (8-byte aligned, fits within 8-byte WASM allocator alignment).
//
// Note on alignment: the WASM allocator (wasm_alloc.c) returns 8-byte aligned
// pointers for double. After a 16-byte header, the user pointer is still
// 8-byte aligned — sufficient for all current Promise types.
//
// When memoryLimitAccounting is true (T0689), additionally adds the requested
// size to @__promise_memory_used_bytes (plain load/store — both WASM targets
// are single-threaded) and aborts via fd_write+proc_exit (wasm32-wasi) or
// promise_env.write+exit (wasm32-web, when webTarget=true) on overflow.
func emitWasmAllocDebug(module *ir.Module, memoryLimitAccounting bool, webTarget bool) *ir.Func {
	mallocSize := ir.NewParam("size", irtypes.I32)
	mallocSize.Attrs = append(mallocSize.Attrs, enum.ParamAttrNoUndef)
	mallocFn := getOrDeclareFunc(module, "malloc", irtypes.I8Ptr, mallocSize)
	mallocFn.ReturnAttrs = append(mallocFn.ReturnAttrs, enum.ReturnAttrNoAlias)
	addFuncAttr(mallocFn, enum.FuncAttrWillReturn)

	memsetFn := getOrDeclareFunc(module, "memset", irtypes.I8Ptr,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("c", irtypes.I32),
		ir.NewParam("n", irtypes.I32))

	allocCount := getOrCreateAllocCountGlobal(module)

	fn := module.NewFunc("pal_alloc", irtypes.I8Ptr,
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")

	size32 := entry.NewTrunc(fn.Params[0], irtypes.I32)
	totalSize := entry.NewAdd(size32, constant.NewInt(irtypes.I32, debugSlackSize))
	internal := entry.NewCall(mallocFn, totalSize)

	nonnull := entry.NewICmp(enum.IPredNE, internal, constant.NewNull(irtypes.I8Ptr))
	headerBlk := fn.NewBlock(".dbg.write_header")
	doneBlk := fn.NewBlock(".done")
	entry.NewCondBr(nonnull, headerBlk, doneBlk)

	// magic_alive at internal + 0
	magicSlot := headerBlk.NewBitCast(internal, irtypes.NewPointer(irtypes.I64))
	headerBlk.NewStore(constant.NewInt(irtypes.I64, debugMagicAlive), magicSlot)

	// requested_size at internal + 8 (stored as i64 even though wasm uses i32 sizes)
	sizeSlot := headerBlk.NewGetElementPtr(irtypes.I64, magicSlot, constant.NewInt(irtypes.I32, 1))
	headerBlk.NewStore(fn.Params[0], sizeSlot)

	userPtr := headerBlk.NewGetElementPtr(irtypes.I8, internal, constant.NewInt(irtypes.I32, debugHeaderSize))

	tailBytePtr := headerBlk.NewGetElementPtr(irtypes.I8, userPtr, size32)
	tailSlot := headerBlk.NewBitCast(tailBytePtr, irtypes.NewPointer(irtypes.I64))
	tailStore := headerBlk.NewStore(constant.NewInt(irtypes.I64, debugMagicTail), tailSlot)
	tailStore.Align = 1

	headerBlk.NewCall(memsetFn, userPtr, constant.NewInt(irtypes.I32, 0xAA), size32)

	old := headerBlk.NewLoad(irtypes.I64, allocCount)
	headerBlk.NewStore(headerBlk.NewAdd(old, constant.NewInt(irtypes.I64, 1)), allocCount)

	// T0689: memory-limit accounting on WASM uses plain load/store + WASI
	// abort (single-threaded wasm32-wasi). Mirrors the libc-allocator path.
	resumeBlk := headerBlk
	if memoryLimitAccounting {
		used, _, _, _ := getOrCreateMemoryLimitGlobals(module)
		oldUsed := resumeBlk.NewLoad(irtypes.I64, used)
		newUsed := resumeBlk.NewAdd(oldUsed, fn.Params[0])
		resumeBlk.NewStore(newUsed, used)
		abort := EmitWasmDebugAbort
		if webTarget {
			abort = EmitWasmWebDebugAbort
		}
		cfg := MemoryLimitConfig{Atomic: false, EmitAbort: abort}
		resumeBlk = emitMemoryLimitCheck(module, fn, resumeBlk, newUsed, cfg)
	}
	resumeBlk.NewBr(doneBlk)

	retPhi := doneBlk.NewPhi(
		&ir.Incoming{X: constant.NewNull(irtypes.I8Ptr), Pred: entry},
		&ir.Incoming{X: userPtr, Pred: resumeBlk},
	)
	doneBlk.NewRet(retPhi)
	return fn
}

// EmitFree declares extern @free and defines @pal_free as a wrapper.
// Includes allocation count tracking for leak detection (T0020).
func (p *WasmPAL) EmitFree(module *ir.Module) *ir.Func {
	if p.DebugAllocator {
		return emitWasmFreeDebug(module, p.MemoryLimitAccounting, p.WebTarget)
	}

	// declare void @free(i8* nocapture noundef) nounwind
	freePtr := ir.NewParam("ptr", irtypes.I8Ptr)
	freePtr.Attrs = append(freePtr.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	freeFn := getOrDeclareFunc(module, "free", irtypes.Void, freePtr)
	addFuncAttr(freeFn, enum.FuncAttrWillReturn)

	allocCount := getOrCreateAllocCountGlobal(module)

	// define void @pal_free(i8* %ptr) nounwind willreturn
	fn := module.NewFunc("pal_free", irtypes.Void,
		ir.NewParam("ptr", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")

	// Track: if ptr is non-null, decrement alloc count and free (non-atomic, WASM is single-threaded)
	nonnull := entry.NewICmp(enum.IPredNE, fn.Params[0], constant.NewNull(irtypes.I8Ptr))
	trackBlk := fn.NewBlock(".track")
	doneBlk := fn.NewBlock(".done")
	entry.NewCondBr(nonnull, trackBlk, doneBlk)

	old := trackBlk.NewLoad(irtypes.I64, allocCount)
	trackBlk.NewStore(trackBlk.NewSub(old, constant.NewInt(irtypes.I64, 1)), allocCount)
	trackBlk.NewCall(freeFn, fn.Params[0])
	trackBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)

	return fn
}

// emitWasmFreeDebug defines @pal_free with header + tail validation. Detects
// double-free, free-of-invalid-pointer, and tail-sentinel corruption via the
// T0365 sentinel scheme on WASM. Mirrors emitLibcFreeDebug.
//
// When memoryLimitAccounting is true (T0689), subtracts the validated
// requested_size from @__promise_memory_used_bytes (plain load/store).
// webTarget selects the abort import set (WASI vs promise_env).
func emitWasmFreeDebug(module *ir.Module, memoryLimitAccounting bool, webTarget bool) *ir.Func {
	freePtr := ir.NewParam("ptr", irtypes.I8Ptr)
	freePtr.Attrs = append(freePtr.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	freeFn := getOrDeclareFunc(module, "free", irtypes.Void, freePtr)
	addFuncAttr(freeFn, enum.FuncAttrWillReturn)

	memsetFn := getOrDeclareFunc(module, "memset", irtypes.I8Ptr,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("c", irtypes.I32),
		ir.NewParam("n", irtypes.I32))

	allocCount := getOrCreateAllocCountGlobal(module)

	fn := module.NewFunc("pal_free", irtypes.Void,
		ir.NewParam("ptr", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")

	nonnull := entry.NewICmp(enum.IPredNE, fn.Params[0], constant.NewNull(irtypes.I8Ptr))
	checkBlk := fn.NewBlock(".dbg.check")
	doneBlk := fn.NewBlock(".done")
	entry.NewCondBr(nonnull, checkBlk, doneBlk)

	size, hdrPtr, validated := emitHeaderValidationWasm(module, fn, checkBlk, fn.Params[0], webTarget)

	// Poison-fill user region: memset(user_ptr, 0xDE, requested_size).
	// memset on WASM takes i32 size — truncate the i64 size from header.
	size32 := validated.NewTrunc(size, irtypes.I32)
	validated.NewCall(memsetFn, fn.Params[0], constant.NewInt(irtypes.I32, 0xDE), size32)

	// Mark BOTH header slots MAGIC_FREED — see emitHeaderValidationLibc for the
	// rationale (offset 0 may be overwritten by allocator free-list bookkeeping).
	freedSlot := validated.NewBitCast(hdrPtr, irtypes.NewPointer(irtypes.I64))
	validated.NewStore(constant.NewInt(irtypes.I64, debugMagicFreed), freedSlot)
	freedSlot2 := validated.NewGetElementPtr(irtypes.I64, freedSlot, constant.NewInt(irtypes.I32, 1))
	validated.NewStore(constant.NewInt(irtypes.I64, debugMagicFreed), freedSlot2)

	// Decrement count and free internal pointer.
	old := validated.NewLoad(irtypes.I64, allocCount)
	validated.NewStore(validated.NewSub(old, constant.NewInt(irtypes.I64, 1)), allocCount)
	// T0689: also subtract size from the memory-limit counter.
	if memoryLimitAccounting {
		used, _, _, _ := getOrCreateMemoryLimitGlobals(module)
		oldUsed := validated.NewLoad(irtypes.I64, used)
		validated.NewStore(validated.NewSub(oldUsed, size), used)
	}
	validated.NewCall(freeFn, hdrPtr)
	validated.NewBr(doneBlk)

	doneBlk.NewRet(nil)
	return fn
}

// EmitRealloc declares extern @realloc and defines @pal_realloc as a wrapper.
// Includes allocation count tracking for leak detection (T0066).
func (p *WasmPAL) EmitRealloc(module *ir.Module) *ir.Func {
	if p.DebugAllocator {
		return emitWasmReallocDebug(module, p.MemoryLimitAccounting, p.WebTarget)
	}

	// declare noalias i8* @realloc(i8* nocapture noundef, i32 noundef) nounwind
	reallocPtr := ir.NewParam("ptr", irtypes.I8Ptr)
	reallocPtr.Attrs = append(reallocPtr.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	reallocSize := ir.NewParam("size", irtypes.I32)
	reallocSize.Attrs = append(reallocSize.Attrs, enum.ParamAttrNoUndef)
	reallocFn := getOrDeclareFunc(module, "realloc", irtypes.I8Ptr, reallocPtr, reallocSize)
	reallocFn.ReturnAttrs = append(reallocFn.ReturnAttrs, enum.ReturnAttrNoAlias)
	addFuncAttr(reallocFn, enum.FuncAttrWillReturn)

	allocCount := getOrCreateAllocCountGlobal(module)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	nullPtr := constant.NewNull(irtypes.I8Ptr)

	// define noalias i8* @pal_realloc(i8* %ptr, i64 %size) nounwind
	fn := module.NewFunc("pal_realloc", irtypes.I8Ptr,
		ir.NewParam("ptr", irtypes.I8Ptr),
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")

	ptrIsNull := entry.NewICmp(enum.IPredEQ, fn.Params[0], nullPtr)
	sizeIsZero := entry.NewICmp(enum.IPredEQ, fn.Params[1], zero64)

	size32 := entry.NewTrunc(fn.Params[1], irtypes.I32)
	ret := entry.NewCall(reallocFn, fn.Params[0], size32)

	// realloc(NULL, size) with non-null result → new allocation
	newAllocBlk := fn.NewBlock(".new_alloc")
	checkFreeBlk := fn.NewBlock(".check_free")
	entry.NewCondBr(ptrIsNull, newAllocBlk, checkFreeBlk)

	retNonNull := newAllocBlk.NewICmp(enum.IPredNE, ret, nullPtr)
	incBlk := fn.NewBlock(".inc")
	doneBlk := fn.NewBlock(".done")
	newAllocBlk.NewCondBr(retNonNull, incBlk, doneBlk)
	incBlk.NewAtomicRMW(enum.AtomicOpAdd, allocCount, one64, enum.AtomicOrderingMonotonic)
	incBlk.NewBr(doneBlk)

	// realloc(ptr, 0) with non-null ptr → deallocation
	decBlk := fn.NewBlock(".dec")
	checkFreeBlk.NewCondBr(sizeIsZero, decBlk, doneBlk)
	decBlk.NewAtomicRMW(enum.AtomicOpSub, allocCount, one64, enum.AtomicOrderingMonotonic)
	decBlk.NewBr(doneBlk)

	doneBlk.NewRet(ret)

	return fn
}

// emitWasmReallocDebug defines @pal_realloc with header validation and
// re-construction (T0365). Mirrors emitLibcReallocDebug but uses i32 sizes
// for the WASM allocator. Delegates the malloc-like (NULL, n) and free-like
// (p, 0) cases to pal_alloc / pal_free for header consistency.
//
// When memoryLimitAccounting is true (T0689), adjusts the memory counter by
// (newSize - oldSize) and aborts on overflow when growing. webTarget selects
// the abort import set (WASI vs promise_env).
func emitWasmReallocDebug(module *ir.Module, memoryLimitAccounting bool, webTarget bool) *ir.Func {
	reallocPtr := ir.NewParam("ptr", irtypes.I8Ptr)
	reallocPtr.Attrs = append(reallocPtr.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	reallocSize := ir.NewParam("size", irtypes.I32)
	reallocSize.Attrs = append(reallocSize.Attrs, enum.ParamAttrNoUndef)
	reallocFn := getOrDeclareFunc(module, "realloc", irtypes.I8Ptr, reallocPtr, reallocSize)
	reallocFn.ReturnAttrs = append(reallocFn.ReturnAttrs, enum.ReturnAttrNoAlias)
	addFuncAttr(reallocFn, enum.FuncAttrWillReturn)

	memsetFn := getOrDeclareFunc(module, "memset", irtypes.I8Ptr,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("c", irtypes.I32),
		ir.NewParam("n", irtypes.I32))

	palAllocFn := lookupFunc(module, "pal_alloc")
	palFreeFn := lookupFunc(module, "pal_free")

	zero64 := constant.NewInt(irtypes.I64, 0)
	nullPtr := constant.NewNull(irtypes.I8Ptr)

	fn := module.NewFunc("pal_realloc", irtypes.I8Ptr,
		ir.NewParam("ptr", irtypes.I8Ptr),
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")

	ptrIsNull := entry.NewICmp(enum.IPredEQ, fn.Params[0], nullPtr)
	allocLikeBlk := fn.NewBlock(".dbg.alloc_like")
	checkFreeLikeBlk := fn.NewBlock(".dbg.check_free_like")
	entry.NewCondBr(ptrIsNull, allocLikeBlk, checkFreeLikeBlk)

	allocResult := allocLikeBlk.NewCall(palAllocFn, fn.Params[1])
	doneBlk := fn.NewBlock(".done")
	allocLikeBlk.NewBr(doneBlk)

	sizeIsZero := checkFreeLikeBlk.NewICmp(enum.IPredEQ, fn.Params[1], zero64)
	freeLikeBlk := fn.NewBlock(".dbg.free_like")
	resizeBlk := fn.NewBlock(".dbg.resize")
	checkFreeLikeBlk.NewCondBr(sizeIsZero, freeLikeBlk, resizeBlk)
	freeLikeBlk.NewCall(palFreeFn, fn.Params[0])
	freeLikeBlk.NewBr(doneBlk)

	oldSize, hdrPtr, validatedBlk := emitHeaderValidationWasm(module, fn, resizeBlk, fn.Params[0], webTarget)

	newSize32 := validatedBlk.NewTrunc(fn.Params[1], irtypes.I32)
	newTotal := validatedBlk.NewAdd(newSize32, constant.NewInt(irtypes.I32, debugSlackSize))
	newInternal := validatedBlk.NewCall(reallocFn, hdrPtr, newTotal)

	newNonNull := validatedBlk.NewICmp(enum.IPredNE, newInternal, nullPtr)
	resizeOkBlk := fn.NewBlock(".dbg.resize_ok")
	validatedBlk.NewCondBr(newNonNull, resizeOkBlk, doneBlk)

	newMagicSlot := resizeOkBlk.NewBitCast(newInternal, irtypes.NewPointer(irtypes.I64))
	newSizeSlot := resizeOkBlk.NewGetElementPtr(irtypes.I64, newMagicSlot, constant.NewInt(irtypes.I32, 1))
	resizeOkBlk.NewStore(fn.Params[1], newSizeSlot)

	newUserPtr := resizeOkBlk.NewGetElementPtr(irtypes.I8, newInternal, constant.NewInt(irtypes.I32, debugHeaderSize))
	newTailBytePtr := resizeOkBlk.NewGetElementPtr(irtypes.I8, newUserPtr, newSize32)
	newTailSlot := resizeOkBlk.NewBitCast(newTailBytePtr, irtypes.NewPointer(irtypes.I64))
	newTailStore := resizeOkBlk.NewStore(constant.NewInt(irtypes.I64, debugMagicTail), newTailSlot)
	newTailStore.Align = 1

	// T0689: adjust the memory-limit counter by (newSize - oldSize). Plain
	// load/store — wasm32-wasi is single-threaded. Only check the limit when
	// growing — shrinking can't trip.
	postAccBlk := resizeOkBlk
	if memoryLimitAccounting {
		used, _, _, _ := getOrCreateMemoryLimitGlobals(module)
		delta := postAccBlk.NewSub(fn.Params[1], oldSize)
		oldUsed := postAccBlk.NewLoad(irtypes.I64, used)
		newUsed := postAccBlk.NewAdd(oldUsed, delta)
		postAccBlk.NewStore(newUsed, used)
		grew := postAccBlk.NewICmp(enum.IPredSGT, delta, constant.NewInt(irtypes.I64, 0))
		checkBlk := fn.NewBlock(".dbg.resize_memlimit_check")
		skipCheckBlk := fn.NewBlock(".dbg.resize_memlimit_skip")
		postAccBlk.NewCondBr(grew, checkBlk, skipCheckBlk)
		abort := EmitWasmDebugAbort
		if webTarget {
			abort = EmitWasmWebDebugAbort
		}
		cfg := MemoryLimitConfig{Atomic: false, EmitAbort: abort}
		afterCheck := emitMemoryLimitCheck(module, fn, checkBlk, newUsed, cfg)
		afterCheck.NewBr(skipCheckBlk)
		postAccBlk = skipCheckBlk
	}

	hasGrown := postAccBlk.NewICmp(enum.IPredUGT, fn.Params[1], oldSize)
	scribbleBlk := fn.NewBlock(".dbg.scribble_grown")
	postAccBlk.NewCondBr(hasGrown, scribbleBlk, doneBlk)

	oldSize32 := scribbleBlk.NewTrunc(oldSize, irtypes.I32)
	scribbleStart := scribbleBlk.NewGetElementPtr(irtypes.I8, newUserPtr, oldSize32)
	scribbleLen := scribbleBlk.NewSub(newSize32, oldSize32)
	scribbleBlk.NewCall(memsetFn, scribbleStart, constant.NewInt(irtypes.I32, 0xAA), scribbleLen)
	scribbleBlk.NewBr(doneBlk)

	retPhi := doneBlk.NewPhi(
		&ir.Incoming{X: allocResult, Pred: allocLikeBlk},
		&ir.Incoming{X: nullPtr, Pred: freeLikeBlk},
		&ir.Incoming{X: nullPtr, Pred: validatedBlk},
		&ir.Incoming{X: newUserPtr, Pred: postAccBlk},
		&ir.Incoming{X: newUserPtr, Pred: scribbleBlk},
	)
	doneBlk.NewRet(retPhi)
	return fn
}

// WASM file I/O stubs — no file system access (Phase D).
func (p *WasmPAL) EmitFileOpen(module *ir.Module) *ir.Func     { return emitStubFileOpen(module) }
func (p *WasmPAL) EmitFileRead(module *ir.Module) *ir.Func     { return emitStubFileRead(module) }
func (p *WasmPAL) EmitFileWrite(module *ir.Module) *ir.Func    { return emitStubFileWrite(module) }
func (p *WasmPAL) EmitFileClose(module *ir.Module) *ir.Func    { return emitStubFileClose(module) }
func (p *WasmPAL) EmitPipeRead(module *ir.Module) *ir.Func     { return emitStubPipeRead(module) }
func (p *WasmPAL) EmitPipeWrite(module *ir.Module) *ir.Func    { return emitStubPipeWrite(module) }
func (p *WasmPAL) EmitPipeClose(module *ir.Module) *ir.Func    { return emitStubPipeClose(module) }
func (p *WasmPAL) EmitFileSeek(module *ir.Module) *ir.Func     { return emitStubFileSeek(module) }
func (p *WasmPAL) EmitFileStatSize(module *ir.Module) *ir.Func { return emitStubFileStatSize(module) }
func (p *WasmPAL) EmitFileStat(module *ir.Module) *ir.Func     { return emitStubFileStat(module) }
func (p *WasmPAL) EmitFileRemove(module *ir.Module) *ir.Func   { return emitStubFileRemove(module) }
func (p *WasmPAL) EmitFileExists(module *ir.Module) *ir.Func   { return emitStubFileExists(module) }
func (p *WasmPAL) EmitFileMkdir(module *ir.Module) *ir.Func    { return emitStubFileMkdir(module) }
func (p *WasmPAL) EmitDirRemove(module *ir.Module) *ir.Func    { return emitStubDirRemove(module) }
func (p *WasmPAL) EmitDirExists(module *ir.Module) *ir.Func    { return emitStubDirExists(module) }
func (p *WasmPAL) EmitErrno(module *ir.Module) *ir.Func        { return emitStubErrno(module) }

// WASM directory listing stubs — no filesystem access.
func (p *WasmPAL) EmitDirOpen(module *ir.Module) *ir.Func     { return emitStubDirOpen(module) }
func (p *WasmPAL) EmitDirNextName(module *ir.Module) *ir.Func { return emitStubDirNextName(module) }
func (p *WasmPAL) EmitDirClose(module *ir.Module) *ir.Func    { return emitStubDirClose(module) }
func (p *WasmPAL) EmitGetEnv(module *ir.Module) *ir.Func      { return emitStubGetEnv(module) }
func (p *WasmPAL) EmitGetCwd(module *ir.Module) *ir.Func      { return emitStubGetCwd(module) }
func (p *WasmPAL) EmitSetEnv(module *ir.Module) *ir.Func      { return emitStubSetEnv(module) }
func (p *WasmPAL) EmitUnsetEnv(module *ir.Module) *ir.Func    { return emitStubUnsetEnv(module) }
func (p *WasmPAL) EmitChdir(module *ir.Module) *ir.Func       { return emitStubChdir(module) }
func (p *WasmPAL) EmitSpawn(module *ir.Module) *ir.Func       { return emitStubSpawn(module) }
func (p *WasmPAL) EmitReadPipe(module *ir.Module) *ir.Func    { return emitStubReadPipe(module) }
func (p *WasmPAL) EmitWaitPid(module *ir.Module) *ir.Func     { return emitStubWaitPid(module) }
func (p *WasmPAL) EmitSpawnStreaming(module *ir.Module) *ir.Func {
	return emitStubSpawnStreaming(module)
}
func (p *WasmPAL) EmitSpawnEnv(module *ir.Module) *ir.Func {
	return emitStubSpawnEnv(module)
}
func (p *WasmPAL) EmitSpawnStreamingEnv(module *ir.Module) *ir.Func {
	return emitStubSpawnStreamingEnv(module)
}
func (p *WasmPAL) EmitKill(module *ir.Module) *ir.Func        { return emitStubKill(module) }
func (p *WasmPAL) EmitExecReplace(module *ir.Module) *ir.Func { return emitStubExecReplace(module) }
func (p *WasmPAL) EmitGetEnviron(module *ir.Module) *ir.Func  { return emitStubGetEnviron(module) }
func (p *WasmPAL) EmitGetUserInfo(module *ir.Module) *ir.Func { return emitStubGetUserInfo(module) }
func (p *WasmPAL) EmitGetHostname(module *ir.Module) *ir.Func { return emitStubGetHostname(module) }
func (p *WasmPAL) EmitSignalInit(module *ir.Module) *ir.Func  { return emitStubSignalInit(module) }
func (p *WasmPAL) EmitSignalRegister(module *ir.Module) *ir.Func {
	return emitStubSignalRegister(module)
}

// EmitStackOverflowInit stub — WASM has built-in stack overflow trapping.
func (p *WasmPAL) EmitStackOverflowInit(module *ir.Module) *ir.Func {
	return emitStubStackOverflowInit(module)
}

func (p *WasmPAL) EmitStackOverflowThreadInit(module *ir.Module) *ir.Func {
	return emitStubStackOverflowThreadInit(module)
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

// WASM socket stubs — no networking support (T0069).
func (p *WasmPAL) EmitSocketCreate(module *ir.Module) *ir.Func  { return emitStubSocketCreate(module) }
func (p *WasmPAL) EmitSocketBind(module *ir.Module) *ir.Func    { return emitStubSocketBind(module) }
func (p *WasmPAL) EmitSocketListen(module *ir.Module) *ir.Func  { return emitStubSocketListen(module) }
func (p *WasmPAL) EmitSocketAccept(module *ir.Module) *ir.Func  { return emitStubSocketAccept(module) }
func (p *WasmPAL) EmitSocketConnect(module *ir.Module) *ir.Func { return emitStubSocketConnect(module) }
func (p *WasmPAL) EmitSocketSend(module *ir.Module) *ir.Func    { return emitStubSocketSend(module) }
func (p *WasmPAL) EmitSocketRecv(module *ir.Module) *ir.Func    { return emitStubSocketRecv(module) }
func (p *WasmPAL) EmitSocketClose(module *ir.Module) *ir.Func   { return emitStubSocketClose(module) }
func (p *WasmPAL) EmitSocketSetOpt(module *ir.Module) *ir.Func  { return emitStubSocketSetOpt(module) }
func (p *WasmPAL) EmitSocketShutdown(module *ir.Module) *ir.Func {
	return emitStubSocketShutdown(module)
}
func (p *WasmPAL) EmitSocketSetNonBlock(module *ir.Module) *ir.Func {
	return emitStubSocketSetNonBlock(module)
}
func (p *WasmPAL) EmitSocketGetError(module *ir.Module) *ir.Func {
	return emitStubSocketGetError(module)
}
func (p *WasmPAL) EmitGetAddrInfo(module *ir.Module) *ir.Func  { return emitStubGetAddrInfo(module) }
func (p *WasmPAL) EmitFreeAddrInfo(module *ir.Module) *ir.Func { return emitStubFreeAddrInfo(module) }

// WASM reactor stubs — no reactor support (T0070).
func (p *WasmPAL) EmitReactorCreate(module *ir.Module) *ir.Func { return emitStubReactorCreate(module) }
func (p *WasmPAL) EmitReactorAdd(module *ir.Module) *ir.Func    { return emitStubReactorAdd(module) }
func (p *WasmPAL) EmitReactorRemove(module *ir.Module) *ir.Func { return emitStubReactorRemove(module) }
func (p *WasmPAL) EmitReactorPoll(module *ir.Module) *ir.Func   { return emitStubReactorPoll(module) }
func (p *WasmPAL) EmitReactorClose(module *ir.Module) *ir.Func  { return emitStubReactorClose(module) }

// WASM high-level socket address stubs — no networking (T0071).
func (p *WasmPAL) EmitSocketBindAddr(module *ir.Module) *ir.Func {
	return emitStubSocketBindAddr(module)
}
func (p *WasmPAL) EmitSocketConnectAddr(module *ir.Module) *ir.Func {
	return emitStubSocketConnectAddr(module)
}
func (p *WasmPAL) EmitSocketAcceptAddr(module *ir.Module) *ir.Func {
	return emitStubSocketAcceptAddr(module)
}
func (p *WasmPAL) EmitSocketGetLocalPort(module *ir.Module) *ir.Func {
	return emitStubSocketGetLocalPort(module)
}
