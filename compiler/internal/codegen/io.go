package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/types"
)

// definePALBodies adds LLVM IR function bodies to print/panic functions that were
// declared as extern or intrinsic declarations. This replaces the C runtime
// (runtime.c and runtime_string.c) with codegen-emitted LLVM IR using PAL
// primitives (pal_write, pal_exit).
//
// Functions are looked up by their LLVM name (C symbol name), not by Promise name,
// because declareExterns stores them in c.funcs under the Promise name (e.g. "_print_string")
// while the LLVM function is named by the C name (e.g. "promise_print_string").
func (c *Compiler) definePALBodies() {
	// Global constants shared by print/panic functions
	nlData := constant.NewCharArrayFromString("\n")
	c.newlineGlobal = c.module.NewGlobalDef(".str.newline", nlData)
	c.newlineGlobal.Immutable = true
	c.newlineGlobal.Linkage = enum.LinkagePrivate

	panicData := constant.NewCharArrayFromString("panic: ")
	c.panicPrefixGlobal = c.module.NewGlobalDef(".str.panic_prefix", panicData)
	c.panicPrefixGlobal.Immutable = true
	c.panicPrefixGlobal.Linkage = enum.LinkagePrivate

	// Build a lookup by LLVM function name for declarations without bodies
	irFuncByName := make(map[string]*ir.Func)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			irFuncByName[fn.Name()] = fn
		}
	}

	// Print function — declared by extern from std/io.pr
	if fn, ok := irFuncByName["promise_print_string"]; ok {
		c.definePrintStringBody(fn)
	}

	// Panic functions
	if fn, ok := irFuncByName["promise_panic"]; ok {
		c.definePanicBody(fn)
	}
	if fn, ok := irFuncByName["promise_panic_msg"]; ok {
		c.definePanicMsgBody(fn)
	}

	// Runtime functions
	if fn, ok := irFuncByName["promise_set_maxprocs"]; ok {
		c.defineSetMaxProcsBody(fn)
	}

	// Scheduler stat getters
	statGetters := []struct {
		name  string
		field int
	}{
		{"promise_sched_gs_created", schedFieldGsCreated},
		{"promise_sched_gs_completed", schedFieldGsCompleted},
		{"promise_sched_ctx_switches", schedFieldContextSwitches},
		{"promise_sched_steals", schedFieldSteals},
	}
	for _, sg := range statGetters {
		if fn, ok := irFuncByName[sg.name]; ok {
			c.defineSchedStatGetterBody(fn, sg.field)
		}
	}

	// Time functions — declared by extern from std/time.pr
	if fn, ok := irFuncByName["promise_nanotime"]; ok {
		c.buildNanotimeExternBody(fn)
	}
	if fn, ok := irFuncByName["promise_sleep_nanos"]; ok {
		c.buildSleepNanosExternBody(fn)
	}

}

// extractStringDataLen extracts the data pointer (i8*) and length (i64) from a
// string value struct pointer (i8* pointing to promise_string_v).
func (c *Compiler) extractStringDataLen(block *ir.Block, strParam value.Value) (dataPtr value.Value, dataLen value.Value) {
	strLayout := c.layouts[types.TypString]
	valType := strLayout.Value.LLVMType
	instType := strLayout.Instance.LLVMType
	instPtrType := irtypes.NewPointer(instType)

	// Bitcast i8* → %promise_string_v*
	valPtr := block.NewBitCast(strParam, irtypes.NewPointer(valType))

	// GEP to field 1 (_instance), load the instance pointer
	instPtrPtr := block.NewGetElementPtr(valType, valPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	instPtr := block.NewLoad(instPtrType, instPtrPtr)

	// Load string length (masking off the literal flag bit)
	dataLen = loadStringLen(block, instPtr, instType)

	// GEP to instance field 2 (data), index 0 → i8*
	dataPtr = block.NewGetElementPtr(instType, instPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	return dataPtr, dataLen
}

// extractStringDataLenFromInstance extracts data pointer and length from a string
// instance pointer (i8* pointing to promise_string_i, as returned by to-string funcs).
func (c *Compiler) extractStringDataLenFromInstance(block *ir.Block, instRaw value.Value) (dataPtr value.Value, dataLen value.Value) {
	strLayout := c.layouts[types.TypString]
	instType := strLayout.Instance.LLVMType

	// Bitcast i8* → %promise_string_i*
	instPtr := block.NewBitCast(instRaw, irtypes.NewPointer(instType))

	// Load string length (masking off the literal flag bit)
	dataLen = loadStringLen(block, instPtr, instType)

	// GEP to instance field 2 (data), index 0 → i8*
	dataPtr = block.NewGetElementPtr(instType, instPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	return dataPtr, dataLen
}

// emitWriteNewline emits a pal_write call that writes a newline character to the given fd.
func (c *Compiler) emitWriteNewline(block *ir.Block, fd value.Value) {
	nlPtr := block.NewGetElementPtr(c.newlineGlobal.ContentType, c.newlineGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewCall(c.palWrite, fd, nlPtr, constant.NewInt(irtypes.I64, 1))
}

// definePrintStringBody adds a function body to promise_print_string(i8* %s).
// Extracts data/len from the string value struct and writes via pal_write.
// Newline handling is done at the Promise level (print_line appends Platform.line_separator
// to the Builder before calling this function).
func (c *Compiler) definePrintStringBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	stdout := constant.NewInt(irtypes.I32, 1)

	dataPtr, dataLen := c.extractStringDataLen(entry, fn.Params[0])

	entry.NewCall(c.palWrite, stdout, dataPtr, dataLen)
	entry.NewRet(nil)
}

// emitErrorPanic extracts the message string from an error instance pointer
// and calls promise_panic with a null-terminated C string copy, then does
// panic return (cleanup + return zero).
// The errInstPtr is an i8* pointing to the error instance struct (field 0 = _variant,
// field 1 = message string instance pointer).
func (c *Compiler) emitErrorPanic(errInstPtr value.Value) {
	errorLayout := c.layouts[types.TypError]
	instType := errorLayout.Instance.LLVMType
	instPtrType := errorLayout.InstancePtrType

	// Cast error instance pointer to typed pointer
	typedPtr := c.block.NewBitCast(errInstPtr, instPtrType)

	// Load the message field (field 1 = string instance pointer, i8*)
	msgFieldPtr := c.block.NewGetElementPtr(instType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	msgInstPtr := c.block.NewLoad(irtypes.I8Ptr, msgFieldPtr)

	// Extract data pointer and length from the string instance
	dataPtr, dataLen := c.extractStringDataLenFromInstance(c.block, msgInstPtr)

	// Allocate a null-terminated C string copy: malloc(len+1), memcpy, null-terminate
	allocSize := c.block.NewAdd(dataLen, constant.NewInt(irtypes.I64, 1))
	cstr := c.block.NewCall(c.palAlloc, allocSize)
	c.block.NewCall(c.funcs["llvm.memcpy"], cstr, dataPtr, dataLen, constant.False)
	nullPos := c.block.NewGetElementPtr(irtypes.I8, cstr, dataLen)
	c.block.NewStore(constant.NewInt(irtypes.I8, 0), nullPos)

	c.block.NewCall(c.funcs["promise_panic"], cstr)
	c.emitPanicReturn()
}

// emitPanicCheck emits a TLS panic flag check after a call expression (T0146).
// Loads the panic flag, branches to a cleanup block if set (panic in flight),
// and sets c.block to the continuation block for normal execution.
// The cleanup block calls emitPanicReturn() to perform scope cleanup and return.
func (c *Compiler) emitPanicCheck() {
	if c.block == nil || c.block.Term != nil {
		return
	}
	flag := c.block.NewLoad(irtypes.I8, c.panicFlagGlobal)
	isPanic := c.block.NewICmp(enum.IPredNE, flag, constant.NewInt(irtypes.I8, 0))
	panicCleanup := c.newBlock("panic.cleanup")
	panicOk := c.newBlock("panic.ok")
	c.block.NewCondBr(isPanic, panicCleanup, panicOk)

	c.block = panicCleanup
	c.emitPanicReturn()

	c.block = panicOk
}

// emitPanicReturn emits cleanup and return-zero after a direct panic call site
// in user function codegen (Category A). Cleans up statement temps, heap temps,
// and scope bindings, then returns the zero value for the current function's
// return type. This is the inline panic equivalent of the error propagation
// return path. B0228: panic flag is set, caller will detect via T0147.
//
// In coroutine context (main body or go blocks), branches to panicExitBlock
// instead of ret — a ret from the middle of a coroutine body is invalid.
func (c *Compiler) emitPanicReturn() {
	if c.block == nil || c.block.Term != nil {
		return
	}
	c.emitStmtTempCleanupForErrorPath()
	c.emitHeapTempCleanupForErrorPath()
	if len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, true) // panic in flight — suppress close errors
	}
	if c.block == nil || c.block.Term != nil {
		return
	}
	// In coroutine context, branch to the panic exit block instead of ret.
	// A ret from the middle of a coroutine body bypasses coro.end/final.suspend.
	if c.panicExitBlock != nil {
		c.block.NewBr(c.panicExitBlock)
		return
	}
	retType := c.fn.Sig.RetType
	if _, isVoid := retType.(*irtypes.VoidType); isVoid {
		c.block.NewRet(nil)
	} else {
		c.block.NewRet(c.zeroValue(retType))
	}
}

// definePanicBody adds a function body to promise_panic(i8* %msg).
// msg is a null-terminated C string (may be .rodata or heap-allocated).
// Sets TLS panic flag + stores msg pointer + returns. The caller is responsible
// for checking the flag and propagating (T0147). Double-panic (flag already set)
// is fatal: writes to stderr and exits with code 134.
func (c *Compiler) definePanicBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	stderr := constant.NewInt(irtypes.I32, 2)

	// Double-panic check: if flag already set → fatal exit
	flag := entry.NewLoad(irtypes.I8, c.panicFlagGlobal)
	alreadyPanicking := entry.NewICmp(enum.IPredNE, flag, constant.NewInt(irtypes.I8, 0))
	doublePanicBlk := fn.NewBlock("double_panic")
	setPanicBlk := fn.NewBlock("set_panic")
	entry.NewCondBr(alreadyPanicking, doublePanicBlk, setPanicBlk)

	// double_panic: write fatal message and exit(134)
	dpMsg := constant.NewCharArrayFromString("fatal: panic during panic recovery\n")
	dpGlobal := c.module.NewGlobalDef(".str.double_panic", dpMsg)
	dpGlobal.Immutable = true
	dpGlobal.Linkage = enum.LinkagePrivate
	dpMsgPtr := doublePanicBlk.NewGetElementPtr(dpGlobal.ContentType, dpGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	doublePanicBlk.NewCall(c.palWrite, stderr, dpMsgPtr, constant.NewInt(irtypes.I64, 35))
	doublePanicBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 134))
	doublePanicBlk.NewUnreachable()

	// set_panic: store flag=1, msg, type=1 (.rodata), ret void
	setPanicBlk.NewStore(constant.NewInt(irtypes.I8, 1), c.panicFlagGlobal)
	setPanicBlk.NewStore(fn.Params[0], c.panicMsgTlsGlobal)
	setPanicBlk.NewStore(constant.NewInt(irtypes.I8, 1), c.panicTypeTlsGlobal) // 1 = .rodata
	setPanicBlk.NewRet(nil)
}

// definePanicMsgBody adds a function body to promise_panic_msg(i8* %s).
// s points to a promise_string_v.
// Extracts data/len from the Promise string, allocates a null-terminated C string
// copy, sets TLS panic flag + stores msg pointer + returns. Double-panic is fatal.
func (c *Compiler) definePanicMsgBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	stderr := constant.NewInt(irtypes.I32, 2)

	// Double-panic check: if flag already set → fatal exit
	flag := entry.NewLoad(irtypes.I8, c.panicFlagGlobal)
	alreadyPanicking := entry.NewICmp(enum.IPredNE, flag, constant.NewInt(irtypes.I8, 0))
	doublePanicBlk := fn.NewBlock("double_panic")
	setPanicBlk := fn.NewBlock("set_panic")
	entry.NewCondBr(alreadyPanicking, doublePanicBlk, setPanicBlk)

	// double_panic: write fatal message and exit(134)
	dpMsg := constant.NewCharArrayFromString("fatal: panic during panic recovery\n")
	dpGlobal := c.module.NewGlobalDef(".str.double_panic_msg", dpMsg)
	dpGlobal.Immutable = true
	dpGlobal.Linkage = enum.LinkagePrivate
	dpMsgPtr := doublePanicBlk.NewGetElementPtr(dpGlobal.ContentType, dpGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	doublePanicBlk.NewCall(c.palWrite, stderr, dpMsgPtr, constant.NewInt(irtypes.I64, 35))
	doublePanicBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 134))
	doublePanicBlk.NewUnreachable()

	// set_panic: extract string data, create C string copy, store flag+msg+type
	dataPtr, dataLen := c.extractStringDataLen(setPanicBlk, fn.Params[0])

	allocSize := setPanicBlk.NewAdd(dataLen, constant.NewInt(irtypes.I64, 1))
	cstr := setPanicBlk.NewCall(c.palAlloc, allocSize)
	setPanicBlk.NewCall(c.funcs["llvm.memcpy"], cstr, dataPtr, dataLen, constant.False)
	nullPos := setPanicBlk.NewGetElementPtr(irtypes.I8, cstr, dataLen)
	setPanicBlk.NewStore(constant.NewInt(irtypes.I8, 0), nullPos)

	setPanicBlk.NewStore(constant.NewInt(irtypes.I8, 1), c.panicFlagGlobal)
	setPanicBlk.NewStore(cstr, c.panicMsgTlsGlobal)
	setPanicBlk.NewStore(constant.NewInt(irtypes.I8, 2), c.panicTypeTlsGlobal) // 2 = heap-allocated
	setPanicBlk.NewRet(nil)
}

// defineSetMaxProcsBody adds a function body to promise_set_maxprocs(i8* %sret, i8* %n).
// Implements GOMAXPROCS: sets the number of active Ps (processors).
// If n <= 0, returns current value without changing. Otherwise clamps to [1, max_p].
// Returns the previous num_p value as a Promise int.
func (c *Compiler) defineSetMaxProcsBody(fn *ir.Func) {
	// fn signature: void @promise_set_maxprocs(i8* %sret, i8* %n)
	sretParam := fn.Params[0]
	nParam := fn.Params[1]

	schedTy := schedStructType()
	intLayout := c.layouts[types.TypInt]
	valType := intLayout.Value.LLVMType

	entry := fn.NewBlock(".entry")

	// Alloca must be in entry block for LLVM's mem2reg pass
	wakeIAlloca := entry.NewAlloca(irtypes.I32)

	// Extract raw i64 from n param (int value struct field 2)
	valPtr := entry.NewBitCast(nParam, irtypes.NewPointer(valType))
	rawPtr := entry.NewGetElementPtr(valType, valPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	rawN := entry.NewLoad(irtypes.I64, rawPtr)
	nI32 := entry.NewTrunc(rawN, irtypes.I32)

	// Load current num_p (this is the old value we'll return)
	numPField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldNumP)))
	oldNumP := entry.NewLoad(irtypes.I32, numPField)

	// If n <= 0, just return old value (query-only mode, like Go's GOMAXPROCS(0))
	isQuery := entry.NewICmp(enum.IPredSLE, nI32, constant.NewInt(irtypes.I32, 0))
	setBlk := fn.NewBlock("set")
	doneBlk := fn.NewBlock("done")
	entry.NewCondBr(isQuery, doneBlk, setBlk)

	// set: clamp n to [1, max_p], store new num_p
	maxPField := setBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMaxP)))
	maxP := setBlk.NewLoad(irtypes.I32, maxPField)

	// clamped = max(1, min(n, max_p))
	nGtMax := setBlk.NewICmp(enum.IPredSGT, nI32, maxP)
	clamped1 := setBlk.NewSelect(nGtMax, maxP, nI32)
	clamped1Lt1 := setBlk.NewICmp(enum.IPredSLT, clamped1, constant.NewInt(irtypes.I32, 1))
	clamped := setBlk.NewSelect(clamped1Lt1, constant.NewInt(irtypes.I32, 1), clamped1)

	setBlk.NewStore(clamped, numPField)

	// If increasing, wake idle Ms so they can pick up work
	isIncrease := setBlk.NewICmp(enum.IPredSGT, clamped, oldNumP)
	wakeBlk := fn.NewBlock("wake")
	setBlk.NewCondBr(isIncrease, wakeBlk, doneBlk)

	// wake: call wake_m for each additional P
	diff := wakeBlk.NewSub(clamped, oldNumP)
	wakeBlk.NewStore(constant.NewInt(irtypes.I32, 0), wakeIAlloca)
	wakeHeader := fn.NewBlock("wake_header")
	wakeBody := fn.NewBlock("wake_body")
	wakeBlk.NewBr(wakeHeader)

	iVal := wakeHeader.NewLoad(irtypes.I32, wakeIAlloca)
	wakeDone := wakeHeader.NewICmp(enum.IPredSGE, iVal, diff)
	wakeHeader.NewCondBr(wakeDone, doneBlk, wakeBody)

	wakeBody.NewCall(c.funcs["promise_sched_wake_m"])
	nextI := wakeBody.NewAdd(iVal, constant.NewInt(irtypes.I32, 1))
	wakeBody.NewStore(nextI, wakeIAlloca)
	wakeBody.NewBr(wakeHeader)

	// done: write old num_p as int value struct to sret
	oldNumP64 := doneBlk.NewSExt(oldNumP, irtypes.I64)
	instancePtrType := intLayout.Value.Fields[1].LLVMType.(*irtypes.PointerType)
	var agg value.Value = constant.NewUndef(valType)
	agg = doneBlk.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 0)
	agg = doneBlk.NewInsertValue(agg, constant.NewNull(instancePtrType), 1)
	agg = doneBlk.NewInsertValue(agg, oldNumP64, 2)

	sretTyped := doneBlk.NewBitCast(sretParam, irtypes.NewPointer(valType))
	doneBlk.NewStore(agg, sretTyped)
	doneBlk.NewRet(nil)
}

// defineSchedStatGetterBody adds a function body to a sched stat getter.
// Signature: void @promise_sched_X(i8* %sret) — reads an i64 counter from the
// sched struct and returns it as a Promise int value via sret.
func (c *Compiler) defineSchedStatGetterBody(fn *ir.Func, field int) {
	sretParam := fn.Params[0]
	schedTy := schedStructType()
	intLayout := c.layouts[types.TypInt]
	valType := intLayout.Value.LLVMType

	entry := fn.NewBlock(".entry")

	// Load the i64 counter from sched struct
	counterField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(field)))
	counterVal := entry.NewLoad(irtypes.I64, counterField)

	// Pack as int value struct and store via sret
	instancePtrType := intLayout.Value.Fields[1].LLVMType.(*irtypes.PointerType)
	var agg value.Value = constant.NewUndef(valType)
	agg = entry.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 0)
	agg = entry.NewInsertValue(agg, constant.NewNull(instancePtrType), 1)
	agg = entry.NewInsertValue(agg, counterVal, 2)

	sretTyped := entry.NewBitCast(sretParam, irtypes.NewPointer(valType))
	entry.NewStore(agg, sretTyped)
	entry.NewRet(nil)
}

// defineNanotimeFunc defines .promise_nanotime_raw() → i64 for the test runner.
// Returns raw i64 nanoseconds (not Promise int value struct).
// Uses a dot-prefixed name to avoid collision with the extern promise_nanotime.
// Idempotent: returns the cached function if already defined.
func (c *Compiler) defineNanotimeFunc() *ir.Func {
	if fn, ok := c.funcs[".promise_nanotime_raw"]; ok {
		return fn
	}
	fn := c.module.NewFunc(".promise_nanotime_raw", irtypes.I64)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	c.funcs[".promise_nanotime_raw"] = fn
	entry := fn.NewBlock(".entry")

	if c.isWasm {
		entry.NewRet(constant.NewInt(irtypes.I64, 0))
		return fn
	}

	if c.isWindows {
		c.buildWindowsNanotimeBody(entry)
		return fn
	}

	timespecType := irtypes.NewStruct(irtypes.I64, irtypes.I64)
	clockGettime := c.getOrDeclareFunc("clock_gettime", irtypes.I32,
		ir.NewParam("clk_id", irtypes.I32),
		ir.NewParam("tp", irtypes.NewPointer(timespecType)))

	clockMonotonic := int64(1)
	triple := c.module.TargetTriple
	if strings.Contains(triple, "darwin") || strings.Contains(triple, "apple") {
		clockMonotonic = 6
	}

	ts := entry.NewAlloca(timespecType)
	entry.NewCall(clockGettime, constant.NewInt(irtypes.I32, clockMonotonic), ts)

	secPtr := entry.NewGetElementPtr(timespecType, ts,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	nsecPtr := entry.NewGetElementPtr(timespecType, ts,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	sec := entry.NewLoad(irtypes.I64, secPtr)
	nsec := entry.NewLoad(irtypes.I64, nsecPtr)

	billion := constant.NewInt(irtypes.I64, 1_000_000_000)
	secNanos := entry.NewMul(sec, billion)
	totalNanos := entry.NewAdd(secNanos, nsec)
	entry.NewRet(totalNanos)
	return fn
}

// buildNanotimeExternBody adds a body to extern promise_nanotime(i8* %sret).
// Computes monotonic nanos via clock_gettime, packs into Promise int value struct.
func (c *Compiler) buildNanotimeExternBody(fn *ir.Func) {
	intLayout := c.layouts[types.TypInt]
	valType := intLayout.Value.LLVMType
	entry := fn.NewBlock(".entry")

	packNanosToSret := func(blk *ir.Block, nanos value.Value) {
		sretPtr := blk.NewBitCast(fn.Params[0], irtypes.NewPointer(valType))
		var agg value.Value = constant.NewUndef(valType)
		agg = blk.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 0)
		instancePtrType := intLayout.Value.Fields[1].LLVMType.(*irtypes.PointerType)
		agg = blk.NewInsertValue(agg, constant.NewNull(instancePtrType), 1)
		agg = blk.NewInsertValue(agg, nanos, 2)
		blk.NewStore(agg, sretPtr)
		blk.NewRet(nil)
	}

	if c.isWasm {
		packNanosToSret(entry, constant.NewInt(irtypes.I64, 0))
		return
	}

	if c.isWindows {
		nanos := c.emitWindowsQPCNanos(entry)
		packNanosToSret(entry, nanos)
		return
	}

	timespecType := irtypes.NewStruct(irtypes.I64, irtypes.I64)
	clockGettime := c.getOrDeclareFunc("clock_gettime", irtypes.I32,
		ir.NewParam("clk_id", irtypes.I32),
		ir.NewParam("tp", irtypes.NewPointer(timespecType)))

	clockMonotonic := int64(1)
	triple := c.module.TargetTriple
	if strings.Contains(triple, "darwin") || strings.Contains(triple, "apple") {
		clockMonotonic = 6
	}

	ts := entry.NewAlloca(timespecType)
	entry.NewCall(clockGettime, constant.NewInt(irtypes.I32, clockMonotonic), ts)

	secPtr := entry.NewGetElementPtr(timespecType, ts,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	nsecPtr := entry.NewGetElementPtr(timespecType, ts,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	sec := entry.NewLoad(irtypes.I64, secPtr)
	nsec := entry.NewLoad(irtypes.I64, nsecPtr)

	billion := constant.NewInt(irtypes.I64, 1_000_000_000)
	secNanos := entry.NewMul(sec, billion)
	totalNanos := entry.NewAdd(secNanos, nsec)
	packNanosToSret(entry, totalNanos)
}

// buildSleepNanosExternBody adds a body to extern promise_sleep_nanos(i8* %ns).
// Extracts raw i64 from Promise int param, calls nanosleep(2). No-op on WASM.
func (c *Compiler) buildSleepNanosExternBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")

	if c.isWasm {
		entry.NewRet(nil)
		return
	}

	// Extract raw i64 from the int value struct parameter
	intLayout := c.layouts[types.TypInt]
	valType := intLayout.Value.LLVMType
	valPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(valType))
	rawPtr := entry.NewGetElementPtr(valType, valPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	ns := entry.NewLoad(irtypes.I64, rawPtr)

	if c.isWindows {
		c.buildWindowsSleepNanosBody(entry, ns)
		return
	}

	timespecType := irtypes.NewStruct(irtypes.I64, irtypes.I64)
	nanosleepFn := c.getOrDeclareFunc("nanosleep", irtypes.I32,
		ir.NewParam("req", irtypes.NewPointer(timespecType)),
		ir.NewParam("rem", irtypes.NewPointer(timespecType)))

	// sec = ns / 1_000_000_000, nsec = ns % 1_000_000_000
	billion := constant.NewInt(irtypes.I64, 1_000_000_000)
	sec := entry.NewSDiv(ns, billion)
	nsec := entry.NewSRem(ns, billion)

	ts := entry.NewAlloca(timespecType)
	secPtr := entry.NewGetElementPtr(timespecType, ts,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	nsecPtr := entry.NewGetElementPtr(timespecType, ts,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	entry.NewStore(sec, secPtr)
	entry.NewStore(nsec, nsecPtr)

	entry.NewCall(nanosleepFn, ts, constant.NewNull(irtypes.NewPointer(timespecType)))
	entry.NewRet(nil)
}

// getOrDeclareFunc returns an existing function by name, or declares a new one.
func (c *Compiler) getOrDeclareFunc(name string, retType irtypes.Type, params ...*ir.Param) *ir.Func {
	for _, fn := range c.module.Funcs {
		if fn.Name() == name {
			return fn
		}
	}
	fn := c.module.NewFunc(name, retType, params...)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	return fn
}

// defineTestRunFunc defines a codegen-emitted promise_test_run(i8* %fn, i64 %timeout_ns) → i32.
// Runs each test in a thread via PAL. The trampoline uses TLS panic flag checking
// for per-test panic recovery — a panicking test sets the flag and returns through
// the call chain, and the trampoline detects this and returns 1 (fail) instead of
// killing the process, allowing subsequent tests to run.
// When timeout_ns > 0, the main thread polls @__promise_test_done with usleep(1ms)
// instead of blocking on pal_thread_join. If the test doesn't complete within
// timeout_ns nanoseconds, returns 2 (timeout) without joining.
// On WASM: panics still terminate the process, no timeout.
func (c *Compiler) defineTestRunFunc() *ir.Func {
	fn := c.module.NewFunc("promise_test_run", irtypes.I32,
		ir.NewParam("fn", irtypes.I8Ptr),
		ir.NewParam("timeout_ns", irtypes.I64))
	entry := fn.NewBlock(".entry")

	trampoline := c.defineTestTrampoline()
	trampolinePtr := entry.NewBitCast(trampoline, irtypes.I8Ptr)

	// Reset done flag before spawning the test thread
	entry.NewStore(constant.NewInt(irtypes.I32, 0), c.testDoneGlobal)

	// Spawn thread: pass the test function pointer as the arg
	handle := entry.NewCall(c.palThreadCreate, trampolinePtr, fn.Params[0])

	if c.isWasm {
		// WASM: no timeout, no recovery — just join and return 0
		entry.NewCall(c.palThreadJoin, handle)
		entry.NewRet(constant.NewInt(irtypes.I32, 0))
		return fn
	}

	// Check if timeout is enabled (timeout_ns > 0)
	nanotimeFn := c.defineNanotimeFunc()
	hasTimeout := entry.NewICmp(enum.IPredSGT, fn.Params[1], constant.NewInt(irtypes.I64, 0))
	pollBlk := fn.NewBlock("poll_start")
	noTimeoutBlk := fn.NewBlock("no_timeout")
	entry.NewCondBr(hasTimeout, pollBlk, noTimeoutBlk)

	// No-timeout path: just join and check result
	noTimeoutBlk.NewCall(c.palThreadJoin, handle)
	checkResultBlk := fn.NewBlock("check_result")
	noTimeoutBlk.NewBr(checkResultBlk)

	// Timeout path: compute deadline, poll done flag with usleep
	deadline := pollBlk.NewAdd(pollBlk.NewCall(nanotimeFn), fn.Params[1])

	pollLoopBlk := fn.NewBlock("poll_loop")
	pollBlk.NewBr(pollLoopBlk)

	// Poll loop: check done flag → check deadline → sleep → loop
	done := pollLoopBlk.NewLoad(irtypes.I32, c.testDoneGlobal)
	isDone := pollLoopBlk.NewICmp(enum.IPredEQ, done, constant.NewInt(irtypes.I32, 1))
	joinDoneBlk := fn.NewBlock("join_done")
	checkDeadlineBlk := fn.NewBlock("check_deadline")
	pollLoopBlk.NewCondBr(isDone, joinDoneBlk, checkDeadlineBlk)

	// Test completed: join thread (near-instant) and check result
	joinDoneBlk.NewCall(c.palThreadJoin, handle)
	joinDoneBlk.NewBr(checkResultBlk)

	// Check deadline
	now := checkDeadlineBlk.NewCall(nanotimeFn)
	pastDeadline := checkDeadlineBlk.NewICmp(enum.IPredSGT, now, deadline)
	timedOutBlk := fn.NewBlock("timed_out")
	sleepBlk := fn.NewBlock("poll_sleep")
	checkDeadlineBlk.NewCondBr(pastDeadline, timedOutBlk, sleepBlk)

	// Sleep 1ms and loop back
	sleepBlk.NewCall(c.palUsleep, constant.NewInt(irtypes.I32, 1000))
	sleepBlk.NewBr(pollLoopBlk)

	// Timed out: don't join (thread still running), return 2
	timedOutBlk.NewRet(constant.NewInt(irtypes.I32, 2))

	// Check result: inspect panic message
	panicMsg := checkResultBlk.NewLoad(irtypes.I8Ptr, c.testPanicMsgGlobal)
	hasPanic := checkResultBlk.NewICmp(enum.IPredNE, panicMsg, constant.NewNull(irtypes.I8Ptr))
	failBlk := fn.NewBlock("test_failed")
	passBlk := fn.NewBlock("test_passed")
	checkResultBlk.NewCondBr(hasPanic, failBlk, passBlk)

	failBlk.NewRet(constant.NewInt(irtypes.I32, 1))
	passBlk.NewRet(constant.NewInt(irtypes.I32, 0))

	return fn
}

// defineTestTrampoline generates a shared trampoline for test runner threads.
// Signature: i8*(i8* %fn_ptr) — casts fn_ptr to void()* and calls it.
// After the test function returns, checks the TLS panic flag to detect panics
// (panics set the flag and return through the call chain via T0144-T0148).
// On panic, stores the panic message and returns non-null to indicate failure.
// On non-WASM targets, also verifies that the stack pointer has not drifted
// (stack creep detection). If it has, stores a diagnostic message in
// @__promise_test_panic_msg and returns non-null to fail the test.
func (c *Compiler) defineTestTrampoline() *ir.Func {
	trampoline := c.module.NewFunc(".test_trampoline", irtypes.I8Ptr,
		ir.NewParam("fn_ptr", irtypes.I8Ptr))
	entry := trampoline.NewBlock(".entry")

	// Call the test function
	voidFnPtrType := irtypes.NewPointer(irtypes.NewFunc(irtypes.Void))
	typedFn := entry.NewBitCast(trampoline.Params[0], voidFnPtrType)

	spBefore := c.emitReadStackPointer(entry)
	entry.NewCall(typedFn)
	spAfter := c.emitReadStackPointer(entry)

	// Check TLS panic flag after test function returns.
	// With TLS flag propagation (T0144-T0148), panics set the flag and return
	// through the call chain. Check the flag here to detect panics.
	panicFlag := entry.NewLoad(irtypes.I8, c.panicFlagGlobal)
	panicked := entry.NewICmp(enum.IPredNE, panicFlag, constant.NewInt(irtypes.I8, 0))
	panicBlk := trampoline.NewBlock("panic_detected")
	checkSpBlk := trampoline.NewBlock("check_sp")
	entry.NewCondBr(panicked, panicBlk, checkSpBlk)

	// Panic detected: store msg, clear TLS state, signal done, return fail
	panicMsgVal := panicBlk.NewLoad(irtypes.I8Ptr, c.panicMsgTlsGlobal)
	panicBlk.NewStore(panicMsgVal, c.testPanicMsgGlobal)
	panicBlk.NewStore(constant.NewInt(irtypes.I8, 0), c.panicFlagGlobal)    // clear flag
	panicBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.panicMsgTlsGlobal) // clear msg
	panicBlk.NewStore(constant.NewInt(irtypes.I32, 1), c.testDoneGlobal)    // signal done
	failInd := panicBlk.NewIntToPtr(constant.NewInt(irtypes.I64, 1), irtypes.I8Ptr)
	panicBlk.NewRet(failInd)

	if spBefore != nil && spAfter != nil {
		// Compare stack pointers — any difference indicates stack creep
		spMatch := checkSpBlk.NewICmp(enum.IPredEQ, spBefore, spAfter)
		stackOkBlk := trampoline.NewBlock("stack_ok")
		stackCreepBlk := trampoline.NewBlock("stack_creep")
		checkSpBlk.NewCondBr(spMatch, stackOkBlk, stackCreepBlk)

		// Stack creep detected: signal done, store diagnostic message, return failure
		stackCreepBlk.NewStore(constant.NewInt(irtypes.I32, 1), c.testDoneGlobal) // signal done
		creepMsgData := constant.NewCharArrayFromString("stack creep detected\x00")
		creepMsgGlobal := c.module.NewGlobalDef(".str.stack_creep_msg", creepMsgData)
		creepMsgGlobal.Immutable = true
		creepMsgGlobal.Linkage = enum.LinkagePrivate
		creepMsgPtr := stackCreepBlk.NewGetElementPtr(creepMsgGlobal.ContentType, creepMsgGlobal,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		stackCreepBlk.NewStore(creepMsgPtr, c.testPanicMsgGlobal)
		creepFail := stackCreepBlk.NewIntToPtr(constant.NewInt(irtypes.I64, 1), irtypes.I8Ptr)
		stackCreepBlk.NewRet(creepFail)

		// Stack OK: signal done, return null (pass)
		stackOkBlk.NewStore(constant.NewInt(irtypes.I32, 1), c.testDoneGlobal) // signal done
		stackOkBlk.NewRet(constant.NewNull(irtypes.I8Ptr))
	} else {
		// SP check not available for this target: signal done, return null (pass)
		checkSpBlk.NewStore(constant.NewInt(irtypes.I32, 1), c.testDoneGlobal) // signal done
		checkSpBlk.NewRet(constant.NewNull(irtypes.I8Ptr))
	}

	return trampoline
}

// emitReadStackPointer emits inline assembly to read the current stack pointer.
// Returns the SP as an i64 value, or nil if the target architecture is not
// supported (e.g. WASM). Uses sideeffect to prevent LLVM from reordering or
// folding the reads across the test function call.
func (c *Compiler) emitReadStackPointer(block *ir.Block) value.Value {
	if c.isWasm {
		return nil
	}

	var asmStr string
	if strings.Contains(c.target, "x86_64") {
		asmStr = "movq %rsp, $0"
	} else if strings.Contains(c.target, "aarch64") || strings.Contains(c.target, "arm64") {
		asmStr = "mov $0, sp"
	} else {
		return nil
	}

	funcType := irtypes.NewFunc(irtypes.I64)
	inlineAsm := ir.NewInlineAsm(irtypes.NewPointer(funcType), asmStr, "=r")
	inlineAsm.SideEffect = true
	return block.NewCall(inlineAsm)
}

// defineTestPrintResultBody adds a function body to promise_test_print_result.
// Params: (i8* %name, i32 %result, i64 %elapsed_ns)
// result: 0=pass, 1=fail, 2=timeout
// Writes "PASS (X.XXXs) <name>\n", "FAIL (X.XXXs) <name>\n", or "TIMEOUT (X.XXXs) <name>\n".
func (c *Compiler) defineTestPrintResultBody(fn *ir.Func) {
	// Global constants for prefix/suffix strings
	passData := constant.NewCharArrayFromString("PASS (")
	passGlobal := c.module.NewGlobalDef(".str.pass_prefix", passData)
	passGlobal.Immutable = true
	passGlobal.Linkage = enum.LinkagePrivate

	failData := constant.NewCharArrayFromString("FAIL (")
	failGlobal := c.module.NewGlobalDef(".str.fail_prefix", failData)
	failGlobal.Immutable = true
	failGlobal.Linkage = enum.LinkagePrivate

	timeoutData := constant.NewCharArrayFromString("TIMEOUT (")
	timeoutGlobal := c.module.NewGlobalDef(".str.timeout_prefix", timeoutData)
	timeoutGlobal.Immutable = true
	timeoutGlobal.Linkage = enum.LinkagePrivate

	dotData := constant.NewCharArrayFromString(".")
	dotGlobal := c.module.NewGlobalDef(".str.dot", dotData)
	dotGlobal.Immutable = true
	dotGlobal.Linkage = enum.LinkagePrivate

	timeSuffixData := constant.NewCharArrayFromString("s) ")
	timeSuffixGlobal := c.module.NewGlobalDef(".str.time_suffix", timeSuffixData)
	timeSuffixGlobal.Immutable = true
	timeSuffixGlobal.Linkage = enum.LinkagePrivate

	stdout := constant.NewInt(irtypes.I32, 1)
	name := fn.Params[0]       // i8*
	resultCode := fn.Params[1] // i32 (0=pass, 1=fail, 2=timeout)
	elapsedNs := fn.Params[2]  // i64

	// Branch: 0 = pass, 2 = timeout, else = fail
	entry := fn.NewBlock(".entry")
	failBlock := fn.NewBlock("fail")
	passBlock := fn.NewBlock("pass")
	timeoutBlock := fn.NewBlock("timeout")
	mergeBlock := fn.NewBlock("merge")

	isPass := entry.NewICmp(enum.IPredEQ, resultCode, constant.NewInt(irtypes.I32, 0))
	notPassBlock := fn.NewBlock("not_pass")
	entry.NewCondBr(isPass, passBlock, notPassBlock)

	isTimeout := notPassBlock.NewICmp(enum.IPredEQ, resultCode, constant.NewInt(irtypes.I32, 2))
	notPassBlock.NewCondBr(isTimeout, timeoutBlock, failBlock)

	// "FAIL (" branch
	failPtr := failBlock.NewGetElementPtr(failGlobal.ContentType, failGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	failBlock.NewCall(c.palWrite, stdout, failPtr, constant.NewInt(irtypes.I64, 6))
	failBlock.NewBr(mergeBlock)

	// "TIMEOUT (" branch
	timeoutPtr := timeoutBlock.NewGetElementPtr(timeoutGlobal.ContentType, timeoutGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	timeoutBlock.NewCall(c.palWrite, stdout, timeoutPtr, constant.NewInt(irtypes.I64, 9))
	timeoutBlock.NewBr(mergeBlock)

	// "PASS (" branch
	passPtr := passBlock.NewGetElementPtr(passGlobal.ContentType, passGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	passBlock.NewCall(c.palWrite, stdout, passPtr, constant.NewInt(irtypes.I64, 6))
	passBlock.NewBr(mergeBlock)

	// Compute seconds and fractional milliseconds from elapsed_ns
	// elapsed_ms = elapsed_ns / 1_000_000
	elapsedMs := mergeBlock.NewSDiv(elapsedNs, constant.NewInt(irtypes.I64, 1_000_000))
	// seconds = elapsed_ms / 1000
	seconds := mergeBlock.NewSDiv(elapsedMs, constant.NewInt(irtypes.I64, 1000))
	// frac_ms = elapsed_ms % 1000
	fracMs := mergeBlock.NewSRem(elapsedMs, constant.NewInt(irtypes.I64, 1000))

	// Write seconds as string
	secStr := mergeBlock.NewCall(c.funcs["promise_int_to_string"], seconds)
	secDataPtr, secDataLen := c.extractStringDataLenFromInstance(mergeBlock, secStr)
	mergeBlock.NewCall(c.palWrite, stdout, secDataPtr, secDataLen)
	mergeBlock.NewCall(c.palFree, secStr)

	// Write "."
	dotPtr := mergeBlock.NewGetElementPtr(dotGlobal.ContentType, dotGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	mergeBlock.NewCall(c.palWrite, stdout, dotPtr, constant.NewInt(irtypes.I64, 1))

	// Zero-pad fractional ms: add 1000 then skip first char ("1005" → "005")
	paddedFrac := mergeBlock.NewAdd(fracMs, constant.NewInt(irtypes.I64, 1000))
	fracStr := mergeBlock.NewCall(c.funcs["promise_int_to_string"], paddedFrac)
	fracDataPtr, fracDataLen := c.extractStringDataLenFromInstance(mergeBlock, fracStr)
	// Skip first character: ptr+1, len-1
	fracDataPtrOffset := mergeBlock.NewGetElementPtr(irtypes.I8, fracDataPtr,
		constant.NewInt(irtypes.I64, 1))
	fracDataLenMinus1 := mergeBlock.NewSub(fracDataLen, constant.NewInt(irtypes.I64, 1))
	mergeBlock.NewCall(c.palWrite, stdout, fracDataPtrOffset, fracDataLenMinus1)
	mergeBlock.NewCall(c.palFree, fracStr)

	// Write "s) "
	tsPtr := mergeBlock.NewGetElementPtr(timeSuffixGlobal.ContentType, timeSuffixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	mergeBlock.NewCall(c.palWrite, stdout, tsPtr, constant.NewInt(irtypes.I64, 3))

	// Write name
	nameLen := mergeBlock.NewCall(c.funcs["strlen"], name)
	mergeBlock.NewCall(c.palWrite, stdout, name, nameLen)

	// Write "\n"
	nlPtr := mergeBlock.NewGetElementPtr(c.newlineGlobal.ContentType, c.newlineGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	mergeBlock.NewCall(c.palWrite, stdout, nlPtr, constant.NewInt(irtypes.I64, 1))

	mergeBlock.NewRet(nil)
}

// defineTestSummaryBody adds a function body to promise_test_summary(i32 %passed, i32 %failed, i32 %skipped, i32 %leaked, i32 %ignored, i32 %stale).
// Writes "<passed> passed, <failed> failed[, <skipped> skipped][, <leaked> leaked][, <ignored> ignored]\n" to stdout via pal_write.
func (c *Compiler) defineTestSummaryBody(fn *ir.Func) {
	// Global constants
	passedSuffixData := constant.NewCharArrayFromString(" passed, ")
	passedSuffixGlobal := c.module.NewGlobalDef(".str.passed_suffix", passedSuffixData)
	passedSuffixGlobal.Immutable = true
	passedSuffixGlobal.Linkage = enum.LinkagePrivate

	failedSuffixData := constant.NewCharArrayFromString(" failed")
	failedSuffixGlobal := c.module.NewGlobalDef(".str.failed_suffix", failedSuffixData)
	failedSuffixGlobal.Immutable = true
	failedSuffixGlobal.Linkage = enum.LinkagePrivate

	commaPrefixData := constant.NewCharArrayFromString(", ")
	commaPrefixGlobal := c.module.NewGlobalDef(".str.comma_prefix", commaPrefixData)
	commaPrefixGlobal.Immutable = true
	commaPrefixGlobal.Linkage = enum.LinkagePrivate

	skippedSuffixData := constant.NewCharArrayFromString(" skipped")
	skippedSuffixGlobal := c.module.NewGlobalDef(".str.skipped_suffix", skippedSuffixData)
	skippedSuffixGlobal.Immutable = true
	skippedSuffixGlobal.Linkage = enum.LinkagePrivate

	leakedSuffixData := constant.NewCharArrayFromString(" leaked")
	leakedSuffixGlobal := c.module.NewGlobalDef(".str.leaked_suffix", leakedSuffixData)
	leakedSuffixGlobal.Immutable = true
	leakedSuffixGlobal.Linkage = enum.LinkagePrivate

	ignoredSuffixData := constant.NewCharArrayFromString(" allowed leaks")
	ignoredSuffixGlobal := c.module.NewGlobalDef(".str.allowed_leaks_suffix", ignoredSuffixData)
	ignoredSuffixGlobal.Immutable = true
	ignoredSuffixGlobal.Linkage = enum.LinkagePrivate

	staleSuffixData := constant.NewCharArrayFromString(" stale allow_leaks")
	staleSuffixGlobal := c.module.NewGlobalDef(".str.stale_suffix", staleSuffixData)
	staleSuffixGlobal.Immutable = true
	staleSuffixGlobal.Linkage = enum.LinkagePrivate

	stdout := constant.NewInt(irtypes.I32, 1)
	passed := fn.Params[0]  // i32
	failed := fn.Params[1]  // i32
	skipped := fn.Params[2] // i32
	leaked := fn.Params[3]  // i32
	ignored := fn.Params[4] // i32
	stale := fn.Params[5]   // i32

	entry := fn.NewBlock(".entry")

	// Convert passed count to string: sext i32 → i64, call promise_int_to_string
	passedI64 := entry.NewSExt(passed, irtypes.I64)
	passedStr := entry.NewCall(c.funcs["promise_int_to_string"], passedI64)
	passedDataPtr, passedDataLen := c.extractStringDataLenFromInstance(entry, passedStr)
	entry.NewCall(c.palWrite, stdout, passedDataPtr, passedDataLen)
	entry.NewCall(c.palFree, passedStr)

	// Write " passed, "
	pSuffixPtr := entry.NewGetElementPtr(passedSuffixGlobal.ContentType, passedSuffixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	entry.NewCall(c.palWrite, stdout, pSuffixPtr, constant.NewInt(irtypes.I64, 9))

	// Convert failed count to string
	failedI64 := entry.NewSExt(failed, irtypes.I64)
	failedStr := entry.NewCall(c.funcs["promise_int_to_string"], failedI64)
	failedDataPtr, failedDataLen := c.extractStringDataLenFromInstance(entry, failedStr)
	entry.NewCall(c.palWrite, stdout, failedDataPtr, failedDataLen)
	entry.NewCall(c.palFree, failedStr)

	// Write " failed"
	fSuffixPtr := entry.NewGetElementPtr(failedSuffixGlobal.ContentType, failedSuffixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	entry.NewCall(c.palWrite, stdout, fSuffixPtr, constant.NewInt(irtypes.I64, 7))

	// Conditionally write ", <skipped> skipped" if skipped > 0
	hasSkipped := entry.NewICmp(enum.IPredSGT, skipped, constant.NewInt(irtypes.I32, 0))
	printSkipBlock := fn.NewBlock("print_skipped")
	afterSkipBlock := fn.NewBlock("after_skipped")
	entry.NewCondBr(hasSkipped, printSkipBlock, afterSkipBlock)

	// Write ", <skipped> skipped"
	commaPtr := printSkipBlock.NewGetElementPtr(commaPrefixGlobal.ContentType, commaPrefixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	printSkipBlock.NewCall(c.palWrite, stdout, commaPtr, constant.NewInt(irtypes.I64, 2))
	skippedI64 := printSkipBlock.NewSExt(skipped, irtypes.I64)
	skippedStr := printSkipBlock.NewCall(c.funcs["promise_int_to_string"], skippedI64)
	skippedDataPtr, skippedDataLen := c.extractStringDataLenFromInstance(printSkipBlock, skippedStr)
	printSkipBlock.NewCall(c.palWrite, stdout, skippedDataPtr, skippedDataLen)
	printSkipBlock.NewCall(c.palFree, skippedStr)
	sSuffixPtr := printSkipBlock.NewGetElementPtr(skippedSuffixGlobal.ContentType, skippedSuffixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	printSkipBlock.NewCall(c.palWrite, stdout, sSuffixPtr, constant.NewInt(irtypes.I64, 8))
	printSkipBlock.NewBr(afterSkipBlock)

	// Conditionally write ", <leaked> leaked" if leaked > 0 (T0020)
	hasLeaked := afterSkipBlock.NewICmp(enum.IPredSGT, leaked, constant.NewInt(irtypes.I32, 0))
	printLeakBlock := fn.NewBlock("print_leaked")
	afterLeakBlock := fn.NewBlock("after_leaked")
	afterSkipBlock.NewCondBr(hasLeaked, printLeakBlock, afterLeakBlock)

	// Write ", <leaked> leaked"
	commaPtr2 := printLeakBlock.NewGetElementPtr(commaPrefixGlobal.ContentType, commaPrefixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	printLeakBlock.NewCall(c.palWrite, stdout, commaPtr2, constant.NewInt(irtypes.I64, 2))
	leakedI64 := printLeakBlock.NewSExt(leaked, irtypes.I64)
	leakedStr := printLeakBlock.NewCall(c.funcs["promise_int_to_string"], leakedI64)
	leakedDataPtr, leakedDataLen := c.extractStringDataLenFromInstance(printLeakBlock, leakedStr)
	printLeakBlock.NewCall(c.palWrite, stdout, leakedDataPtr, leakedDataLen)
	printLeakBlock.NewCall(c.palFree, leakedStr)
	lSuffixPtr := printLeakBlock.NewGetElementPtr(leakedSuffixGlobal.ContentType, leakedSuffixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	printLeakBlock.NewCall(c.palWrite, stdout, lSuffixPtr, constant.NewInt(irtypes.I64, 7))
	printLeakBlock.NewBr(afterLeakBlock)

	// Conditionally write ", <ignored> ignored" if ignored > 0 (T0067)
	hasIgnored := afterLeakBlock.NewICmp(enum.IPredSGT, ignored, constant.NewInt(irtypes.I32, 0))
	printIgnoredBlock := fn.NewBlock("print_ignored")
	afterIgnoredBlock := fn.NewBlock("after_ignored")
	afterLeakBlock.NewCondBr(hasIgnored, printIgnoredBlock, afterIgnoredBlock)

	// Write ", <ignored> ignored"
	commaPtr3 := printIgnoredBlock.NewGetElementPtr(commaPrefixGlobal.ContentType, commaPrefixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	printIgnoredBlock.NewCall(c.palWrite, stdout, commaPtr3, constant.NewInt(irtypes.I64, 2))
	ignoredI64 := printIgnoredBlock.NewSExt(ignored, irtypes.I64)
	ignoredStr := printIgnoredBlock.NewCall(c.funcs["promise_int_to_string"], ignoredI64)
	ignoredDataPtr, ignoredDataLen := c.extractStringDataLenFromInstance(printIgnoredBlock, ignoredStr)
	printIgnoredBlock.NewCall(c.palWrite, stdout, ignoredDataPtr, ignoredDataLen)
	printIgnoredBlock.NewCall(c.palFree, ignoredStr)
	iSuffixPtr := printIgnoredBlock.NewGetElementPtr(ignoredSuffixGlobal.ContentType, ignoredSuffixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	printIgnoredBlock.NewCall(c.palWrite, stdout, iSuffixPtr, constant.NewInt(irtypes.I64, 14))
	printIgnoredBlock.NewBr(afterIgnoredBlock)

	// Conditionally write ", <stale> stale allow_leaks" if stale > 0
	hasStale := afterIgnoredBlock.NewICmp(enum.IPredSGT, stale, constant.NewInt(irtypes.I32, 0))
	printStaleBlock := fn.NewBlock("print_stale")
	afterStaleBlock := fn.NewBlock("after_stale")
	afterIgnoredBlock.NewCondBr(hasStale, printStaleBlock, afterStaleBlock)

	// Write ", <stale> stale allow_leaks"
	commaPtr4 := printStaleBlock.NewGetElementPtr(commaPrefixGlobal.ContentType, commaPrefixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	printStaleBlock.NewCall(c.palWrite, stdout, commaPtr4, constant.NewInt(irtypes.I64, 2))
	staleI64 := printStaleBlock.NewSExt(stale, irtypes.I64)
	staleStr := printStaleBlock.NewCall(c.funcs["promise_int_to_string"], staleI64)
	staleDataPtr, staleDataLen := c.extractStringDataLenFromInstance(printStaleBlock, staleStr)
	printStaleBlock.NewCall(c.palWrite, stdout, staleDataPtr, staleDataLen)
	printStaleBlock.NewCall(c.palFree, staleStr)
	sStaleSuffixPtr := printStaleBlock.NewGetElementPtr(staleSuffixGlobal.ContentType, staleSuffixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	printStaleBlock.NewCall(c.palWrite, stdout, sStaleSuffixPtr, constant.NewInt(irtypes.I64, 18))
	printStaleBlock.NewBr(afterStaleBlock)

	// Write "\n"
	nlPtr := afterStaleBlock.NewGetElementPtr(c.newlineGlobal.ContentType, c.newlineGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	afterStaleBlock.NewCall(c.palWrite, stdout, nlPtr, constant.NewInt(irtypes.I64, 1))

	afterStaleBlock.NewRet(nil)
}

// emitLeakMessage writes "  leak: <N> allocations not freed\n" to stdout (T0020).
// The block is NOT terminated — caller must add a terminator after this call.
// leakPrefixGlobal and leakSuffixGlobal must be pre-created module globals.
func (c *Compiler) emitLeakMessage(blk *ir.Block, delta value.Value, leakPrefixGlobal, leakSuffixGlobal *ir.Global) {
	stdout := constant.NewInt(irtypes.I32, 1)

	// Write "  leak: "
	prefixPtr := blk.NewGetElementPtr(leakPrefixGlobal.ContentType, leakPrefixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	blk.NewCall(c.palWrite, stdout, prefixPtr, constant.NewInt(irtypes.I64, 8))

	// Write delta as string
	deltaStr := blk.NewCall(c.funcs["promise_int_to_string"], delta)
	deltaDataPtr, deltaDataLen := c.extractStringDataLenFromInstance(blk, deltaStr)
	blk.NewCall(c.palWrite, stdout, deltaDataPtr, deltaDataLen)
	blk.NewCall(c.palFree, deltaStr)

	// Write " allocations not freed\n" (23 bytes)
	suffixPtr := blk.NewGetElementPtr(leakSuffixGlobal.ContentType, leakSuffixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	blk.NewCall(c.palWrite, stdout, suffixPtr, constant.NewInt(irtypes.I64, 23))
}

// emitStaleAllowLeaksWarning writes "  allow_leaks: no leaks detected (tag can be removed)\n"
// to stdout for tests that have allow_leaks but did not leak (T0067).
// The block is NOT terminated — caller must add a terminator after this call.
func (c *Compiler) emitStaleAllowLeaksWarning(blk *ir.Block, testName string) {
	msg := fmt.Sprintf("  allow_leaks: %s did not leak (tag can be removed)\n", testName)
	msgData := constant.NewCharArrayFromString(msg)
	msgGlobal := c.module.NewGlobalDef(fmt.Sprintf(".str.stale_allow_leaks_%s", testName), msgData)
	msgGlobal.Immutable = true
	msgGlobal.Linkage = enum.LinkagePrivate

	stdout := constant.NewInt(irtypes.I32, 1)
	msgPtr := blk.NewGetElementPtr(msgGlobal.ContentType, msgGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	blk.NewCall(c.palWrite, stdout, msgPtr, constant.NewInt(irtypes.I64, int64(len(msg))))
}
