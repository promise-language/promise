package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/types"
)

// defineFileIOBodies adds LLVM IR function bodies to file I/O extern declarations
// from modules/io/io.pr. Each body bridges Promise types to raw PAL syscall wrappers.
//
// Must run after compileModules() so that io module externs are declared in c.module.Funcs.
func (c *Compiler) defineFileIOBodies() {
	// Build lookup by LLVM function name for declarations without bodies
	irFuncByName := make(map[string]*ir.Func)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			irFuncByName[fn.Name()] = fn
		}
	}

	// File operations — each extracts Promise types, calls PAL, wraps result
	if fn, ok := irFuncByName["promise_io_file_open"]; ok {
		c.defineFileOpenBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_file_close"]; ok {
		c.defineFileCloseBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_file_write_string"]; ok {
		c.defineFileWriteStringBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_file_read_all"]; ok {
		c.defineFileReadAllBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_file_seek"]; ok {
		c.defineFileSeekBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_file_stat_size"]; ok {
		c.defineFileStatSizeBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_file_remove"]; ok {
		c.defineFileRemoveBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_file_exists"]; ok {
		c.defineFileExistsBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_file_mkdir"]; ok {
		c.defineFileMkdirBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_dir_remove"]; ok {
		c.defineDirRemoveBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_dir_exists"]; ok {
		c.defineDirExistsBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_errno"]; ok {
		c.defineErrnoBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_file_read_line"]; ok {
		c.defineFileReadLineBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_dir_open"]; ok {
		c.defineDirOpenBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_dir_next_name"]; ok {
		c.defineDirNextNameBody(fn)
	}
	if fn, ok := irFuncByName["promise_io_dir_close_handle"]; ok {
		c.defineDirCloseHandleBody(fn)
	}
}

// ── Syscall handoff helpers ──────────────────────────────────────────────────

// emitEnterSyscall emits a call to promise_sched_enter_syscall before a blocking PAL call.
// Releases P so other goroutines can run while this M blocks in the kernel.
func (c *Compiler) emitEnterSyscall(block *ir.Block) {
	block.NewCall(c.funcs["promise_sched_enter_syscall"])
}

// emitExitSyscall emits a call to promise_sched_exit_syscall after a blocking PAL call returns.
// Reattaches P to this M so the goroutine can continue running.
func (c *Compiler) emitExitSyscall(block *ir.Block) {
	block.NewCall(c.funcs["promise_sched_exit_syscall"])
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// extractRawInt extracts the raw i64 value from a Promise int value struct pointer.
// The int value struct layout is {i8* vtable, instance_ptr, i64 raw} — raw is at index 2.
func (c *Compiler) extractRawInt(block *ir.Block, param value.Value) value.Value {
	intLayout := c.layouts[types.TypInt]
	valType := intLayout.Value.LLVMType
	valPtr := block.NewBitCast(param, irtypes.NewPointer(valType))
	rawPtr := block.NewGetElementPtr(valType, valPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	return block.NewLoad(irtypes.I64, rawPtr)
}

// storeIntResult packs a raw i64 into a Promise int value struct and stores via sret.
func (c *Compiler) storeIntResult(block *ir.Block, sretParam value.Value, rawI64 value.Value) {
	intLayout := c.layouts[types.TypInt]
	valType := intLayout.Value.LLVMType
	instancePtrType := intLayout.Value.Fields[1].LLVMType.(*irtypes.PointerType)

	var agg value.Value = constant.NewUndef(valType)
	agg = block.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 0)
	agg = block.NewInsertValue(agg, constant.NewNull(instancePtrType), 1)
	agg = block.NewInsertValue(agg, rawI64, 2)

	sretTyped := block.NewBitCast(sretParam, irtypes.NewPointer(valType))
	block.NewStore(agg, sretTyped)
}

// storeStringResult packs a string instance pointer (i8*) into a string value struct
// and stores it via sret. String value struct layout: {i8* vtable, promise_string_i* instance}.
func (c *Compiler) storeStringResult(block *ir.Block, sretParam value.Value, strInst value.Value) {
	strLayout := c.layouts[types.TypString]
	valType := strLayout.Value.LLVMType
	instancePtrType := strLayout.Value.Fields[1].LLVMType.(*irtypes.PointerType)

	var agg value.Value = constant.NewUndef(valType)
	agg = block.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 0)
	inst := block.NewBitCast(strInst, instancePtrType)
	agg = block.NewInsertValue(agg, inst, 1)

	sretTyped := block.NewBitCast(sretParam, irtypes.NewPointer(valType))
	block.NewStore(agg, sretTyped)
}

// stringToCStr creates a null-terminated C string from a Promise string.
// Allocates via palAlloc — caller must free with palFree after use.
// Returns the i8* C string pointer.
func (c *Compiler) stringToCStr(block *ir.Block, strParam value.Value) value.Value {
	dataPtr, dataLen := c.extractStringDataLen(block, strParam)

	// Allocate len+1 bytes for null-terminated copy
	allocSize := block.NewAdd(dataLen, constant.NewInt(irtypes.I64, 1))
	cstr := block.NewCall(c.palAlloc, allocSize)
	block.NewCall(c.funcs["llvm.memcpy"], cstr, dataPtr, dataLen, constant.False)

	// Write null terminator
	nullPos := block.NewGetElementPtr(irtypes.I8, cstr, dataLen)
	block.NewStore(constant.NewInt(irtypes.I8, 0), nullPos)

	return cstr
}

// ── File open/close/seek ─────────────────────────────────────────────────────

// defineFileOpenBody: void @promise_io_file_open(i8* sret, i8* path, i8* mode)
// Extracts path string → cstr, mode int → i32, calls pal_file_open, wraps i32 fd as int.
func (c *Compiler) defineFileOpenBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	pathParam := fn.Params[1]
	modeParam := fn.Params[2]

	// Convert Promise string path to null-terminated C string
	cstr := c.stringToCStr(entry, pathParam)

	// Extract raw mode i32 from Promise int
	modeRaw := c.extractRawInt(entry, modeParam)
	modeI32 := entry.NewTrunc(modeRaw, irtypes.I32)

	// Call PAL: i32 @pal_file_open(i8* path, i32 mode)
	c.emitEnterSyscall(entry)
	fd := entry.NewCall(c.palFileOpen, cstr, modeI32)
	c.emitExitSyscall(entry)

	// Free temporary C string
	entry.NewCall(c.palFree, cstr)

	// Sign-extend i32 fd to i64 and store as Promise int
	fdI64 := entry.NewSExt(fd, irtypes.I64)
	c.storeIntResult(entry, sret, fdI64)
	entry.NewRet(nil)
}

// defineFileCloseBody: void @promise_io_file_close(i8* sret, i8* fd)
func (c *Compiler) defineFileCloseBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	fdParam := fn.Params[1]

	fdRaw := c.extractRawInt(entry, fdParam)
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	// Call PAL: i32 @pal_file_close(i32 fd)
	c.emitEnterSyscall(entry)
	rc := entry.NewCall(c.palFileClose, fdI32)
	c.emitExitSyscall(entry)

	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, sret, rcI64)
	entry.NewRet(nil)
}

// defineFileSeekBody: void @promise_io_file_seek(i8* sret, i8* fd, i8* offset, i8* whence)
func (c *Compiler) defineFileSeekBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	offsetRaw := c.extractRawInt(entry, fn.Params[2])

	whenceRaw := c.extractRawInt(entry, fn.Params[3])
	whenceI32 := entry.NewTrunc(whenceRaw, irtypes.I32)

	// Call PAL: i64 @pal_file_seek(i32 fd, i64 offset, i32 whence)
	c.emitEnterSyscall(entry)
	pos := entry.NewCall(c.palFileSeek, fdI32, offsetRaw, whenceI32)
	c.emitExitSyscall(entry)

	c.storeIntResult(entry, sret, pos)
	entry.NewRet(nil)
}

// ── File read/write ──────────────────────────────────────────────────────────

// defineFileWriteStringBody: void @promise_io_file_write_string(i8* sret, i8* fd, i8* data)
// Extracts fd and string data/len, calls pal_file_write, returns bytes written.
func (c *Compiler) defineFileWriteStringBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]
	fdParam := fn.Params[1]
	dataParam := fn.Params[2]

	fdRaw := c.extractRawInt(entry, fdParam)
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	dataPtr, dataLen := c.extractStringDataLen(entry, dataParam)

	// Call PAL: i64 @pal_file_write(i32 fd, i8* buf, i64 len)
	c.emitEnterSyscall(entry)
	written := entry.NewCall(c.palFileWrite, fdI32, dataPtr, dataLen)
	c.emitExitSyscall(entry)

	c.storeIntResult(entry, sret, written)
	entry.NewRet(nil)
}

// defineFileReadAllBody: void @promise_io_file_read_all(i8* sret, i8* fd)
// Reads all data from fd into a growing buffer, stores string result via sret.
// pal_file_read now returns -errno on failure (errno folded into return).
// On success: clears errno to 0 so the caller's _io_errno() check sees no error.
// On error: sets errno to the error code so the caller's _io_errno() check sees it.
func (c *Compiler) defineFileReadAllBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	// Find the errno location function to clear/set errno for the caller
	errnoLocFn := c.findErrnoLocationFn()

	// Allocas for loop state (LLVM mem2reg will optimize to SSA)
	bufAlloca := entry.NewAlloca(irtypes.I8Ptr)
	totalAlloca := entry.NewAlloca(irtypes.I64)
	capAlloca := entry.NewAlloca(irtypes.I64)

	// Initial buffer: 4096 bytes
	initCap := constant.NewInt(irtypes.I64, 4096)
	initBuf := entry.NewCall(c.palAlloc, initCap)
	entry.NewStore(initBuf, bufAlloca)
	entry.NewStore(constant.NewInt(irtypes.I64, 0), totalAlloca)
	entry.NewStore(initCap, capAlloca)

	loopBlk := fn.NewBlock("read_loop")
	entry.NewBr(loopBlk)

	// ── Read loop ──
	buf := loopBlk.NewLoad(irtypes.I8Ptr, bufAlloca)
	total := loopBlk.NewLoad(irtypes.I64, totalAlloca)
	cap_ := loopBlk.NewLoad(irtypes.I64, capAlloca)

	remaining := loopBlk.NewSub(cap_, total)
	readPtr := loopBlk.NewGetElementPtr(irtypes.I8, buf, total)

	// Call PAL: i64 @pal_file_read(i32 fd, i8* buf, i64 len)
	// Returns bytes read on success, -errno on failure
	c.emitEnterSyscall(loopBlk)
	n := loopBlk.NewCall(c.palFileRead, fdI32, readPtr, remaining)
	c.emitExitSyscall(loopBlk)

	// Check error (n < 0 means -errno)
	isErr := loopBlk.NewICmp(enum.IPredSLT, n, constant.NewInt(irtypes.I64, 0))
	checkEOF := fn.NewBlock("check_eof")
	errorBlk := fn.NewBlock("read_error")
	loopBlk.NewCondBr(isErr, errorBlk, checkEOF)

	// Check EOF (n == 0)
	isEOF := checkEOF.NewICmp(enum.IPredEQ, n, constant.NewInt(irtypes.I64, 0))
	doneBlk := fn.NewBlock("read_done")
	afterRead := fn.NewBlock("after_read")
	checkEOF.NewCondBr(isEOF, doneBlk, afterRead)

	// Update total, check if buffer needs growing
	newTotal := afterRead.NewAdd(total, n)
	afterRead.NewStore(newTotal, totalAlloca)
	isFull := afterRead.NewICmp(enum.IPredEQ, newTotal, cap_)
	growBlk := fn.NewBlock("grow_buf")
	afterRead.NewCondBr(isFull, growBlk, loopBlk)

	// Grow buffer: double capacity, realloc
	newCap := growBlk.NewMul(cap_, constant.NewInt(irtypes.I64, 2))
	newBuf := growBlk.NewCall(c.palRealloc, buf, newCap)
	growBlk.NewStore(newBuf, bufAlloca)
	growBlk.NewStore(newCap, capAlloca)
	growBlk.NewBr(loopBlk)

	// ── Done: create string from buffer, store via sret ──
	doneBuf := doneBlk.NewLoad(irtypes.I8Ptr, bufAlloca)
	doneTotal := doneBlk.NewLoad(irtypes.I64, totalAlloca)
	str := doneBlk.NewCall(c.funcs["promise_string_new"], doneBuf, doneTotal)
	doneBlk.NewCall(c.palFree, doneBuf)
	// Clear errno to 0 on success so the caller's _io_errno() check sees no error
	if errnoLocFn != nil {
		errnoPtr := doneBlk.NewCall(errnoLocFn)
		doneBlk.NewStore(constant.NewInt(irtypes.I32, 0), errnoPtr)
	}
	c.storeStringResult(doneBlk, sret, str)
	doneBlk.NewRet(nil)

	// ── Error: n is -errno. Free buffer, set errno, return empty string ──
	// Extract errno code from -n (pal_file_read returns -errno on failure)
	negN := errorBlk.NewSub(constant.NewInt(irtypes.I64, 0), n)
	errCode := errorBlk.NewTrunc(negN, irtypes.I32)
	errBuf := errorBlk.NewLoad(irtypes.I8Ptr, bufAlloca)
	errorBlk.NewCall(c.palFree, errBuf)
	emptyStr := errorBlk.NewCall(c.funcs["promise_string_new"],
		constant.NewNull(irtypes.I8Ptr), constant.NewInt(irtypes.I64, 0))
	// Set errno so the caller's _io_errno() check sees the error code
	if errnoLocFn != nil {
		errnoPtr := errorBlk.NewCall(errnoLocFn)
		errorBlk.NewStore(errCode, errnoPtr)
	}
	c.storeStringResult(errorBlk, sret, emptyStr)
	errorBlk.NewRet(nil)
}

// findErrnoLocationFn finds the platform-specific errno location function
// (__error on macOS, __errno_location on Linux, _errno on Windows)
// already declared in the module by the PAL layer.
// Returns nil if not found (e.g., on WASM).
func (c *Compiler) findErrnoLocationFn() *ir.Func {
	for _, fn := range c.module.Funcs {
		name := fn.Name()
		if name == "__error" || name == "__errno_location" || name == "_errno" {
			return fn
		}
	}
	return nil
}

// ── Path-based operations (string path → cstr → PAL → int result) ───────────

// defineFileStatSizeBody: void @promise_io_file_stat_size(i8* sret, i8* path)
func (c *Compiler) defineFileStatSizeBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	cstr := c.stringToCStr(entry, fn.Params[1])
	c.emitEnterSyscall(entry)
	result := entry.NewCall(c.palFileStatSize, cstr)
	c.emitExitSyscall(entry)
	entry.NewCall(c.palFree, cstr)
	c.storeIntResult(entry, fn.Params[0], result)
	entry.NewRet(nil)
}

// defineFileRemoveBody: void @promise_io_file_remove(i8* sret, i8* path)
func (c *Compiler) defineFileRemoveBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	cstr := c.stringToCStr(entry, fn.Params[1])
	c.emitEnterSyscall(entry)
	rc := entry.NewCall(c.palFileRemove, cstr)
	c.emitExitSyscall(entry)
	entry.NewCall(c.palFree, cstr)
	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, fn.Params[0], rcI64)
	entry.NewRet(nil)
}

// defineFileExistsBody: void @promise_io_file_exists(i8* sret, i8* path)
func (c *Compiler) defineFileExistsBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	cstr := c.stringToCStr(entry, fn.Params[1])
	c.emitEnterSyscall(entry)
	rc := entry.NewCall(c.palFileExists, cstr)
	c.emitExitSyscall(entry)
	entry.NewCall(c.palFree, cstr)
	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, fn.Params[0], rcI64)
	entry.NewRet(nil)
}

// defineFileMkdirBody: void @promise_io_file_mkdir(i8* sret, i8* path)
func (c *Compiler) defineFileMkdirBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	cstr := c.stringToCStr(entry, fn.Params[1])
	c.emitEnterSyscall(entry)
	rc := entry.NewCall(c.palFileMkdir, cstr)
	c.emitExitSyscall(entry)
	entry.NewCall(c.palFree, cstr)
	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, fn.Params[0], rcI64)
	entry.NewRet(nil)
}

// defineDirRemoveBody: void @promise_io_dir_remove(i8* sret, i8* path)
func (c *Compiler) defineDirRemoveBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	cstr := c.stringToCStr(entry, fn.Params[1])
	c.emitEnterSyscall(entry)
	rc := entry.NewCall(c.palDirRemove, cstr)
	c.emitExitSyscall(entry)
	entry.NewCall(c.palFree, cstr)
	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, fn.Params[0], rcI64)
	entry.NewRet(nil)
}

// defineDirExistsBody: void @promise_io_dir_exists(i8* sret, i8* path)
func (c *Compiler) defineDirExistsBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	cstr := c.stringToCStr(entry, fn.Params[1])
	c.emitEnterSyscall(entry)
	rc := entry.NewCall(c.palDirExists, cstr)
	c.emitExitSyscall(entry)
	entry.NewCall(c.palFree, cstr)
	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, fn.Params[0], rcI64)
	entry.NewRet(nil)
}

// ── Errno ────────────────────────────────────────────────────────────────────

// defineErrnoBody: void @promise_io_errno(i8* sret)
func (c *Compiler) defineErrnoBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")

	// Call PAL: i32 @pal_errno()
	errnoVal := entry.NewCall(c.palErrno)

	// Sign-extend to i64 and store as Promise int
	errnoI64 := entry.NewSExt(errnoVal, irtypes.I64)
	c.storeIntResult(entry, fn.Params[0], errnoI64)
	entry.NewRet(nil)
}

// ── Read line ────────────────────────────────────────────────────────────────

// defineFileReadLineBody: void @promise_io_file_read_line(i8* sret, i8* fd)
// Reads one line from fd by calling pal_file_read one byte at a time.
// Strips trailing \r\n or \n. On EOF with no data, sets errno to 0x7FFF0001.
// On error, sets errno to the error code.
func (c *Compiler) defineFileReadLineBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	errnoLocFn := c.findErrnoLocationFn()

	// Allocas for loop state
	bufAlloca := entry.NewAlloca(irtypes.I8Ptr)
	lenAlloca := entry.NewAlloca(irtypes.I64)
	capAlloca := entry.NewAlloca(irtypes.I64)

	// Initial buffer: 256 bytes
	initCap := constant.NewInt(irtypes.I64, 256)
	initBuf := entry.NewCall(c.palAlloc, initCap)
	entry.NewStore(initBuf, bufAlloca)
	entry.NewStore(constant.NewInt(irtypes.I64, 0), lenAlloca)
	entry.NewStore(initCap, capAlloca)

	// Stack-allocate a single byte for reading
	oneByte := entry.NewAlloca(irtypes.I8)

	loopBlk := fn.NewBlock("read_loop")
	entry.NewBr(loopBlk)

	// ── Read loop: read one byte at a time ──
	c.emitEnterSyscall(loopBlk)
	n := loopBlk.NewCall(c.palFileRead, fdI32,
		loopBlk.NewBitCast(oneByte, irtypes.I8Ptr),
		constant.NewInt(irtypes.I64, 1))
	c.emitExitSyscall(loopBlk)

	// Check error (n < 0)
	isErr := loopBlk.NewICmp(enum.IPredSLT, n, constant.NewInt(irtypes.I64, 0))
	checkEOF := fn.NewBlock("check_eof")
	errorBlk := fn.NewBlock("read_error")
	loopBlk.NewCondBr(isErr, errorBlk, checkEOF)

	// Check EOF (n == 0)
	isEOF := checkEOF.NewICmp(enum.IPredEQ, n, constant.NewInt(irtypes.I64, 0))
	eofBlk := fn.NewBlock("eof")
	gotByte := fn.NewBlock("got_byte")
	checkEOF.NewCondBr(isEOF, eofBlk, gotByte)

	// ── Got a byte: check for newline ──
	ch := gotByte.NewLoad(irtypes.I8, oneByte)
	isNewline := gotByte.NewICmp(enum.IPredEQ, ch, constant.NewInt(irtypes.I8, '\n'))
	doneBlk := fn.NewBlock("line_done")
	appendBlk := fn.NewBlock("append_byte")
	gotByte.NewCondBr(isNewline, doneBlk, appendBlk)

	// ── Append byte to buffer ──
	curLen := appendBlk.NewLoad(irtypes.I64, lenAlloca)
	curCap := appendBlk.NewLoad(irtypes.I64, capAlloca)
	curBuf := appendBlk.NewLoad(irtypes.I8Ptr, bufAlloca)

	// Check if buffer needs growing
	isFull := appendBlk.NewICmp(enum.IPredEQ, curLen, curCap)
	growBlk := fn.NewBlock("grow")
	storeBlk := fn.NewBlock("store_byte")
	appendBlk.NewCondBr(isFull, growBlk, storeBlk)

	// Grow buffer
	newCap := growBlk.NewMul(curCap, constant.NewInt(irtypes.I64, 2))
	newBuf := growBlk.NewCall(c.palRealloc, curBuf, newCap)
	growBlk.NewStore(newBuf, bufAlloca)
	growBlk.NewStore(newCap, capAlloca)
	growBlk.NewBr(storeBlk)

	// Store byte (phi between grow and no-grow paths)
	storeBuf := storeBlk.NewLoad(irtypes.I8Ptr, bufAlloca)
	storeLen := storeBlk.NewLoad(irtypes.I64, lenAlloca)
	bytePos := storeBlk.NewGetElementPtr(irtypes.I8, storeBuf, storeLen)
	storeBlk.NewStore(ch, bytePos)
	newLen := storeBlk.NewAdd(storeLen, constant.NewInt(irtypes.I64, 1))
	storeBlk.NewStore(newLen, lenAlloca)
	storeBlk.NewBr(loopBlk)

	// ── Line done (hit \n): strip trailing \r if present, clear errno, return ──
	doneLen := doneBlk.NewLoad(irtypes.I64, lenAlloca)
	doneBuf := doneBlk.NewLoad(irtypes.I8Ptr, bufAlloca)

	// Check if last byte is \r (for \r\n line endings) and subtract 1 if so.
	// Guard against empty buffer (len==0) by using select for safe index.
	hasData := doneBlk.NewICmp(enum.IPredUGT, doneLen, constant.NewInt(irtypes.I64, 0))
	lastIdx := doneBlk.NewSub(doneLen, constant.NewInt(irtypes.I64, 1))
	safeIdx := doneBlk.NewSelect(hasData, lastIdx, constant.NewInt(irtypes.I64, 0))
	lastPtr := doneBlk.NewGetElementPtr(irtypes.I8, doneBuf, safeIdx)
	lastCh := doneBlk.NewLoad(irtypes.I8, lastPtr)
	isCR := doneBlk.NewICmp(enum.IPredEQ, lastCh, constant.NewInt(irtypes.I8, '\r'))
	shouldStrip := doneBlk.NewAnd(hasData, isCR)
	stripAmt := doneBlk.NewSelect(shouldStrip,
		constant.NewInt(irtypes.I64, 1), constant.NewInt(irtypes.I64, 0))
	finalLen := doneBlk.NewSub(doneLen, stripAmt)

	str := doneBlk.NewCall(c.funcs["promise_string_new"], doneBuf, finalLen)
	doneBlk.NewCall(c.palFree, doneBuf)
	if errnoLocFn != nil {
		errnoPtr := doneBlk.NewCall(errnoLocFn)
		doneBlk.NewStore(constant.NewInt(irtypes.I32, 0), errnoPtr)
	}
	c.storeStringResult(doneBlk, sret, str)
	doneBlk.NewRet(nil)

	// ── EOF: check if we have accumulated data ──
	eofLen := eofBlk.NewLoad(irtypes.I64, lenAlloca)
	eofBuf := eofBlk.NewLoad(irtypes.I8Ptr, bufAlloca)
	hasAccum := eofBlk.NewICmp(enum.IPredUGT, eofLen, constant.NewInt(irtypes.I64, 0))
	eofWithData := fn.NewBlock("eof_data")
	eofNoData := fn.NewBlock("eof_empty")
	eofBlk.NewCondBr(hasAccum, eofWithData, eofNoData)

	// EOF with data: return accumulated data as final line, clear errno
	eofStr := eofWithData.NewCall(c.funcs["promise_string_new"], eofBuf, eofLen)
	eofWithData.NewCall(c.palFree, eofBuf)
	if errnoLocFn != nil {
		errnoPtr := eofWithData.NewCall(errnoLocFn)
		eofWithData.NewStore(constant.NewInt(irtypes.I32, 0), errnoPtr)
	}
	c.storeStringResult(eofWithData, sret, eofStr)
	eofWithData.NewRet(nil)

	// EOF with no data: set errno to EOF sentinel (0x7FFF0001 = 2147418113)
	eofNoData.NewCall(c.palFree, eofBuf)
	emptyStr := eofNoData.NewCall(c.funcs["promise_string_new"],
		constant.NewNull(irtypes.I8Ptr), constant.NewInt(irtypes.I64, 0))
	if errnoLocFn != nil {
		errnoPtr := eofNoData.NewCall(errnoLocFn)
		eofNoData.NewStore(constant.NewInt(irtypes.I32, 0x7FFF0001), errnoPtr)
	}
	c.storeStringResult(eofNoData, sret, emptyStr)
	eofNoData.NewRet(nil)

	// ── Error: set errno, return empty string ──
	negN := errorBlk.NewSub(constant.NewInt(irtypes.I64, 0), n)
	errCode := errorBlk.NewTrunc(negN, irtypes.I32)
	errBuf := errorBlk.NewLoad(irtypes.I8Ptr, bufAlloca)
	errorBlk.NewCall(c.palFree, errBuf)
	errEmpty := errorBlk.NewCall(c.funcs["promise_string_new"],
		constant.NewNull(irtypes.I8Ptr), constant.NewInt(irtypes.I64, 0))
	if errnoLocFn != nil {
		errnoPtr := errorBlk.NewCall(errnoLocFn)
		errorBlk.NewStore(errCode, errnoPtr)
	}
	c.storeStringResult(errorBlk, sret, errEmpty)
	errorBlk.NewRet(nil)
}

// ── Directory listing bridges ────────────────────────────────────────────────

// defineDirOpenBody: void @promise_io_dir_open(i8* sret, i8* path)
// Converts Promise string path to C string, calls pal_dir_open, returns handle as int.
// Returns -errno on failure (null from PAL → check errno).
func (c *Compiler) defineDirOpenBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	cstr := c.stringToCStr(entry, fn.Params[1])
	c.emitEnterSyscall(entry)
	handle := entry.NewCall(c.palDirOpen, cstr)
	c.emitExitSyscall(entry)
	entry.NewCall(c.palFree, cstr)

	// Check if null (error)
	isNull := entry.NewICmp(enum.IPredEQ, handle, constant.NewNull(irtypes.I8Ptr))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isNull, errBlk, okBlk)

	// Success: cast i8* to i64 (handle as integer)
	handleI64 := okBlk.NewPtrToInt(handle, irtypes.I64)
	c.storeIntResult(okBlk, sret, handleI64)
	okBlk.NewRet(nil)

	// Error: return -errno
	errnoLocFn := c.findErrnoLocationFn()
	if errnoLocFn != nil {
		errnoPtr := errBlk.NewCall(errnoLocFn)
		errnoVal := errBlk.NewLoad(irtypes.I32, errnoPtr)
		errnoI64 := errBlk.NewSExt(errnoVal, irtypes.I64)
		negErrno := errBlk.NewSub(constant.NewInt(irtypes.I64, 0), errnoI64)
		c.storeIntResult(errBlk, sret, negErrno)
	} else {
		c.storeIntResult(errBlk, sret, constant.NewInt(irtypes.I64, -1))
	}
	errBlk.NewRet(nil)
}

// defineDirNextNameBody: void @promise_io_dir_next_name(i8* sret, i8* handle)
// Casts int handle to i8*, calls pal_dir_next_name, creates Promise string from result.
// Returns empty string when done. Sets errno on error (PAL clears errno before readdir).
func (c *Compiler) defineDirNextNameBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	// Extract int handle and cast to i8*
	handleI64 := c.extractRawInt(entry, fn.Params[1])
	handlePtr := entry.NewIntToPtr(handleI64, irtypes.I8Ptr)

	c.emitEnterSyscall(entry)
	namePtr := entry.NewCall(c.palDirNextName, handlePtr)
	c.emitExitSyscall(entry)

	isNull := entry.NewICmp(enum.IPredEQ, namePtr, constant.NewNull(irtypes.I8Ptr))
	gotName := fn.NewBlock(".got_name")
	nullBlk := fn.NewBlock(".null")
	entry.NewCondBr(isNull, nullBlk, gotName)

	// Got a name: strlen + promise_string_new
	strlenFn := c.funcs["strlen"]
	nameLen := gotName.NewCall(strlenFn, namePtr)
	str := gotName.NewCall(c.funcs["promise_string_new"], namePtr, nameLen)
	c.storeStringResult(gotName, sret, str)
	gotName.NewRet(nil)

	// Null: end-of-directory or error (errno already set by PAL)
	// Return empty string — caller checks errno to distinguish error from done
	emptyStr := nullBlk.NewCall(c.funcs["promise_string_new"],
		constant.NewNull(irtypes.I8Ptr), constant.NewInt(irtypes.I64, 0))
	c.storeStringResult(nullBlk, sret, emptyStr)
	nullBlk.NewRet(nil)
}

// defineDirCloseHandleBody: void @promise_io_dir_close_handle(i8* sret, i8* handle)
// Casts int handle to i8*, calls pal_dir_close, returns 0.
func (c *Compiler) defineDirCloseHandleBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	handleI64 := c.extractRawInt(entry, fn.Params[1])
	handlePtr := entry.NewIntToPtr(handleI64, irtypes.I8Ptr)

	c.emitEnterSyscall(entry)
	entry.NewCall(c.palDirClose, handlePtr)
	c.emitExitSyscall(entry)

	c.storeIntResult(entry, sret, constant.NewInt(irtypes.I64, 0))
	entry.NewRet(nil)
}
