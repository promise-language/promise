package codegen

import (
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

	// GEP to instance field 1 (len), load i64
	lenPtr := block.NewGetElementPtr(instType, instPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dataLen = block.NewLoad(irtypes.I64, lenPtr)

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

	// GEP to instance field 1 (len), load i64
	lenPtr := block.NewGetElementPtr(instType, instPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dataLen = block.NewLoad(irtypes.I64, lenPtr)

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

// definePanicBody adds a function body to promise_panic(i8* %msg).
// msg is a null-terminated C string.
// Recovery priority: (1) goroutine recovery via scheduler longjmp, (2) test recovery
// via test trampoline longjmp, (3) write to stderr and exit.
// On WASM: always exits (no longjmp recovery — single-threaded, no goroutine isolation).
func (c *Compiler) definePanicBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	stderr := constant.NewInt(irtypes.I32, 2)

	if c.isWasm {
		// WASM: all panics write to stderr and exit
		prefixPtr := entry.NewGetElementPtr(c.panicPrefixGlobal.ContentType, c.panicPrefixGlobal,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		entry.NewCall(c.palWrite, stderr, prefixPtr, constant.NewInt(irtypes.I64, 7))
		msgLen := entry.NewCall(c.funcs["strlen"], fn.Params[0])
		entry.NewCall(c.palWrite, stderr, fn.Params[0], msgLen)
		c.emitWriteNewline(entry, stderr)
		entry.NewCall(c.palExit, constant.NewInt(irtypes.I32, 1))
		entry.NewUnreachable()
		return
	}

	gTy := goroutineStructType()

	// Check if we're in a non-main goroutine that can recover
	currentG := entry.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
	hasG := entry.NewICmp(enum.IPredNE, currentG, constant.NewNull(irtypes.I8Ptr))

	checkIdBlk := fn.NewBlock("check_id")
	checkTestBlk := fn.NewBlock("check_test_jmpbuf")

	entry.NewCondBr(hasG, checkIdBlk, checkTestBlk)

	// checkId: if G.id != 0 (not main goroutine), use longjmp recovery to scheduler
	gPtr := checkIdBlk.NewBitCast(currentG, irtypes.NewPointer(gTy))
	idField := checkIdBlk.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldID)))
	gID := checkIdBlk.NewLoad(irtypes.I64, idField)
	isMain := checkIdBlk.NewICmp(enum.IPredEQ, gID, constant.NewInt(irtypes.I64, 0))

	recoverBlk := fn.NewBlock("do_recover")
	checkIdBlk.NewCondBr(isMain, checkTestBlk, recoverBlk)

	// do_recover: set G.panicked=1, G.panic_msg=msg, longjmp back to scheduler
	// No stderr output — recovered goroutines are silent.
	panickedField := recoverBlk.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
	recoverBlk.NewStore(constant.NewInt(irtypes.I8, 1), panickedField)

	panicMsgField := recoverBlk.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
	recoverBlk.NewStore(fn.Params[0], panicMsgField)

	// Load jmp_buf from TLS and longjmp
	jmpBuf := recoverBlk.NewLoad(irtypes.I8Ptr, c.panicJmpBufGlobal)
	recoverBlk.NewCall(c.funcs["longjmp"], jmpBuf, constant.NewInt(irtypes.I32, 1))
	recoverBlk.NewUnreachable()

	// checkTestJmpbuf: check if a test trampoline set a jmpbuf for recovery
	testJmpBuf := checkTestBlk.NewLoad(irtypes.I8Ptr, c.testJmpBufGlobal)
	hasTestJmpBuf := checkTestBlk.NewICmp(enum.IPredNE, testJmpBuf, constant.NewNull(irtypes.I8Ptr))
	testRecoverBlk := fn.NewBlock("test_recover")
	exitBlk := fn.NewBlock("do_exit")
	checkTestBlk.NewCondBr(hasTestJmpBuf, testRecoverBlk, exitBlk)

	// test_recover: store panic msg for the test runner, longjmp back to trampoline
	testRecoverBlk.NewStore(fn.Params[0], c.testPanicMsgGlobal)
	testRecoverBlk.NewCall(c.funcs["longjmp"], testJmpBuf, constant.NewInt(irtypes.I32, 1))
	testRecoverBlk.NewUnreachable()

	// do_exit: no recovery available — write panic message and exit
	prefixPtr := exitBlk.NewGetElementPtr(c.panicPrefixGlobal.ContentType, c.panicPrefixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	exitBlk.NewCall(c.palWrite, stderr, prefixPtr, constant.NewInt(irtypes.I64, 7))
	msgLen := exitBlk.NewCall(c.funcs["strlen"], fn.Params[0])
	exitBlk.NewCall(c.palWrite, stderr, fn.Params[0], msgLen)
	c.emitWriteNewline(exitBlk, stderr)
	exitBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 1))
	exitBlk.NewUnreachable()
	// noreturn already set on promise_panic in declareIntrinsics
}

// definePanicMsgBody adds a function body to promise_panic_msg(i8* %s).
// s points to a promise_string_v.
// Recovery priority: (1) goroutine recovery via scheduler longjmp, (2) test recovery
// via test trampoline longjmp, (3) write to stderr and exit.
// On WASM: always exits (no longjmp recovery).
func (c *Compiler) definePanicMsgBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	stderr := constant.NewInt(irtypes.I32, 2)

	// Extract data/len from string value struct (needed by both paths)
	dataPtr, dataLen := c.extractStringDataLen(entry, fn.Params[0])

	if c.isWasm {
		// WASM: all panics write to stderr and exit
		prefixPtr := entry.NewGetElementPtr(c.panicPrefixGlobal.ContentType, c.panicPrefixGlobal,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		entry.NewCall(c.palWrite, stderr, prefixPtr, constant.NewInt(irtypes.I64, 7))
		entry.NewCall(c.palWrite, stderr, dataPtr, dataLen)
		c.emitWriteNewline(entry, stderr)
		entry.NewCall(c.palExit, constant.NewInt(irtypes.I32, 1))
		entry.NewUnreachable()
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoReturn)
		return
	}

	gTy := goroutineStructType()

	// Check if we're in a non-main goroutine that can recover
	currentG := entry.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
	hasG := entry.NewICmp(enum.IPredNE, currentG, constant.NewNull(irtypes.I8Ptr))

	checkIdBlk := fn.NewBlock("check_id")
	checkTestBlk := fn.NewBlock("check_test_jmpbuf")

	entry.NewCondBr(hasG, checkIdBlk, checkTestBlk)

	// checkId: if G.id != 0 (not main goroutine), use longjmp recovery to scheduler
	gPtr := checkIdBlk.NewBitCast(currentG, irtypes.NewPointer(gTy))
	idField := checkIdBlk.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldID)))
	gID := checkIdBlk.NewLoad(irtypes.I64, idField)
	isMain := checkIdBlk.NewICmp(enum.IPredEQ, gID, constant.NewInt(irtypes.I64, 0))

	recoverBlk := fn.NewBlock("do_recover")
	checkIdBlk.NewCondBr(isMain, checkTestBlk, recoverBlk)

	// do_recover: set G.panicked=1, G.panic_msg=cstr, longjmp back to scheduler
	// No stderr output — recovered goroutines are silent.
	panickedField := recoverBlk.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
	recoverBlk.NewStore(constant.NewInt(irtypes.I8, 1), panickedField)

	// Create a null-terminated C string copy from Promise string data for G.panic_msg.
	allocSize := recoverBlk.NewAdd(dataLen, constant.NewInt(irtypes.I64, 1))
	cstr := recoverBlk.NewCall(c.palAlloc, allocSize)
	recoverBlk.NewCall(c.funcs["llvm.memcpy"], cstr, dataPtr, dataLen, constant.False)
	nullPos := recoverBlk.NewGetElementPtr(irtypes.I8, cstr, dataLen)
	recoverBlk.NewStore(constant.NewInt(irtypes.I8, 0), nullPos)

	panicMsgField := recoverBlk.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
	recoverBlk.NewStore(cstr, panicMsgField)

	// Load jmp_buf from TLS and longjmp
	jmpBuf := recoverBlk.NewLoad(irtypes.I8Ptr, c.panicJmpBufGlobal)
	recoverBlk.NewCall(c.funcs["longjmp"], jmpBuf, constant.NewInt(irtypes.I32, 1))
	recoverBlk.NewUnreachable()

	// checkTestJmpbuf: check if a test trampoline set a jmpbuf for recovery
	testJmpBuf := checkTestBlk.NewLoad(irtypes.I8Ptr, c.testJmpBufGlobal)
	hasTestJmpBuf := checkTestBlk.NewICmp(enum.IPredNE, testJmpBuf, constant.NewNull(irtypes.I8Ptr))
	testRecoverBlk := fn.NewBlock("test_recover")
	exitBlk := fn.NewBlock("do_exit")
	checkTestBlk.NewCondBr(hasTestJmpBuf, testRecoverBlk, exitBlk)

	// test_recover: create C string copy, store for test runner, longjmp back to trampoline
	testAllocSize := testRecoverBlk.NewAdd(dataLen, constant.NewInt(irtypes.I64, 1))
	testCstr := testRecoverBlk.NewCall(c.palAlloc, testAllocSize)
	testRecoverBlk.NewCall(c.funcs["llvm.memcpy"], testCstr, dataPtr, dataLen, constant.False)
	testNullPos := testRecoverBlk.NewGetElementPtr(irtypes.I8, testCstr, dataLen)
	testRecoverBlk.NewStore(constant.NewInt(irtypes.I8, 0), testNullPos)
	testRecoverBlk.NewStore(testCstr, c.testPanicMsgGlobal)
	testRecoverBlk.NewCall(c.funcs["longjmp"], testJmpBuf, constant.NewInt(irtypes.I32, 1))
	testRecoverBlk.NewUnreachable()

	// do_exit: no recovery available — write panic message and exit
	prefixPtr := exitBlk.NewGetElementPtr(c.panicPrefixGlobal.ContentType, c.panicPrefixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	exitBlk.NewCall(c.palWrite, stderr, prefixPtr, constant.NewInt(irtypes.I64, 7))
	exitBlk.NewCall(c.palWrite, stderr, dataPtr, dataLen)
	c.emitWriteNewline(exitBlk, stderr)
	exitBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 1))
	exitBlk.NewUnreachable()

	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoReturn)
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
func (c *Compiler) defineNanotimeFunc() *ir.Func {
	fn := c.module.NewFunc(".promise_nanotime_raw", irtypes.I64)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	if c.isWasm {
		entry.NewRet(constant.NewInt(irtypes.I64, 0))
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

	if c.isWasm {
		sretPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(valType))
		var agg value.Value = constant.NewUndef(valType)
		agg = entry.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 0)
		instancePtrType := intLayout.Value.Fields[1].LLVMType.(*irtypes.PointerType)
		agg = entry.NewInsertValue(agg, constant.NewNull(instancePtrType), 1)
		agg = entry.NewInsertValue(agg, constant.NewInt(irtypes.I64, 0), 2)
		entry.NewStore(agg, sretPtr)
		entry.NewRet(nil)
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

	// Pack into int value struct {vtable, instance_ptr, raw_i64} and store to sret
	sretPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(valType))
	var agg value.Value = constant.NewUndef(valType)
	agg = entry.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 0)
	instancePtrType := intLayout.Value.Fields[1].LLVMType.(*irtypes.PointerType)
	agg = entry.NewInsertValue(agg, constant.NewNull(instancePtrType), 1)
	agg = entry.NewInsertValue(agg, totalNanos, 2)
	entry.NewStore(agg, sretPtr)
	entry.NewRet(nil)
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

// defineTestRunFunc defines a codegen-emitted promise_test_run(i8* %fn) → i32.
// Runs each test in a thread via PAL. On non-WASM targets, the trampoline uses
// setjmp/longjmp for per-test panic recovery — a panicking test returns 1 (fail)
// instead of killing the process, allowing subsequent tests to run.
// On WASM: panics still terminate the process (no longjmp support).
func (c *Compiler) defineTestRunFunc() *ir.Func {
	fn := c.module.NewFunc("promise_test_run", irtypes.I32,
		ir.NewParam("fn", irtypes.I8Ptr))
	entry := fn.NewBlock(".entry")

	trampoline := c.defineTestTrampoline()
	trampolinePtr := entry.NewBitCast(trampoline, irtypes.I8Ptr)

	// Spawn thread: pass the test function pointer as the arg
	handle := entry.NewCall(c.palThreadCreate, trampolinePtr, fn.Params[0])

	// Join thread (waits for completion)
	entry.NewCall(c.palThreadJoin, handle)

	if c.isWasm {
		// WASM: no recovery — if we get here, the test passed
		entry.NewRet(constant.NewInt(irtypes.I32, 0))
	} else {
		// Check @__promise_test_panic_msg: non-null means test panicked
		panicMsg := entry.NewLoad(irtypes.I8Ptr, c.testPanicMsgGlobal)
		hasPanic := entry.NewICmp(enum.IPredNE, panicMsg, constant.NewNull(irtypes.I8Ptr))
		failBlk := fn.NewBlock("test_failed")
		passBlk := fn.NewBlock("test_passed")
		entry.NewCondBr(hasPanic, failBlk, passBlk)

		failBlk.NewRet(constant.NewInt(irtypes.I32, 1))
		passBlk.NewRet(constant.NewInt(irtypes.I32, 0))
	}
	return fn
}

// defineTestTrampoline generates a shared trampoline for test runner threads.
// Signature: i8*(i8* %fn_ptr) — casts fn_ptr to void()* and calls it.
// On non-WASM: uses setjmp/longjmp for panic recovery. The trampoline stores a
// jmp_buf in the TLS @__promise_test_jmpbuf so promise_panic can longjmp back
// instead of exiting. On panic, returns non-null to indicate failure.
// After the test function returns normally, verifies that the stack pointer has
// not drifted (stack creep detection). If it has, stores a diagnostic message
// in @__promise_test_panic_msg and returns non-null to fail the test.
func (c *Compiler) defineTestTrampoline() *ir.Func {
	trampoline := c.module.NewFunc(".test_trampoline", irtypes.I8Ptr,
		ir.NewParam("fn_ptr", irtypes.I8Ptr))
	entry := trampoline.NewBlock(".entry")

	if c.isWasm {
		// WASM: no setjmp recovery — just call the test function directly
		voidFnPtrType := irtypes.NewPointer(irtypes.NewFunc(irtypes.Void))
		typedFn := entry.NewBitCast(trampoline.Params[0], voidFnPtrType)
		entry.NewCall(typedFn)
		entry.NewRet(constant.NewNull(irtypes.I8Ptr))
		return trampoline
	}

	// Allocate 256-byte jmp_buf on stack (same as sched.go panic recovery)
	jmpBufType := irtypes.NewArray(256, irtypes.I8)
	jmpBufAlloca := entry.NewAlloca(jmpBufType)
	jmpBufPtr := entry.NewBitCast(jmpBufAlloca, irtypes.I8Ptr)

	// Store jmp_buf pointer in dedicated test TLS so promise_panic can find it
	entry.NewStore(jmpBufPtr, c.testJmpBufGlobal)

	// setjmp returns 0 on initial call, non-zero on longjmp return
	setjmpResult := entry.NewCall(c.funcs["setjmp"], jmpBufPtr)
	isPanicReturn := entry.NewICmp(enum.IPredNE, setjmpResult, constant.NewInt(irtypes.I32, 0))

	normalBlk := trampoline.NewBlock("normal")
	panicBlk := trampoline.NewBlock("panic_recovered")
	entry.NewCondBr(isPanicReturn, panicBlk, normalBlk)

	// Normal path: read SP, call test function, read SP again, check for drift
	voidFnPtrType := irtypes.NewPointer(irtypes.NewFunc(irtypes.Void))
	typedFn := normalBlk.NewBitCast(trampoline.Params[0], voidFnPtrType)

	spBefore := c.emitReadStackPointer(normalBlk)
	normalBlk.NewCall(typedFn)
	spAfter := c.emitReadStackPointer(normalBlk)

	if spBefore != nil && spAfter != nil {
		// Compare stack pointers — any difference indicates stack creep
		spMatch := normalBlk.NewICmp(enum.IPredEQ, spBefore, spAfter)
		stackOkBlk := trampoline.NewBlock("stack_ok")
		stackCreepBlk := trampoline.NewBlock("stack_creep")
		normalBlk.NewCondBr(spMatch, stackOkBlk, stackCreepBlk)

		// Stack creep detected: store diagnostic message, return failure
		creepMsgData := constant.NewCharArrayFromString("stack creep detected\x00")
		creepMsgGlobal := c.module.NewGlobalDef(".str.stack_creep_msg", creepMsgData)
		creepMsgGlobal.Immutable = true
		creepMsgGlobal.Linkage = enum.LinkagePrivate
		creepMsgPtr := stackCreepBlk.NewGetElementPtr(creepMsgGlobal.ContentType, creepMsgGlobal,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		stackCreepBlk.NewStore(creepMsgPtr, c.testPanicMsgGlobal)
		stackCreepBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.testJmpBufGlobal)
		creepFail := stackCreepBlk.NewIntToPtr(constant.NewInt(irtypes.I64, 1), irtypes.I8Ptr)
		stackCreepBlk.NewRet(creepFail)

		// Stack OK: clear jmpbuf, return null (pass)
		stackOkBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.testJmpBufGlobal)
		stackOkBlk.NewRet(constant.NewNull(irtypes.I8Ptr))
	} else {
		// SP check not available for this target: proceed without check
		normalBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.testJmpBufGlobal)
		normalBlk.NewRet(constant.NewNull(irtypes.I8Ptr))
	}

	// Panic recovery path: clear jmpbuf, return non-null (fail indicator)
	panicBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.testJmpBufGlobal)
	failIndicator := panicBlk.NewIntToPtr(constant.NewInt(irtypes.I64, 1), irtypes.I8Ptr)
	panicBlk.NewRet(failIndicator)

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
// Params: (i8* %name, i32 %failed, i64 %elapsed_ns)
// Writes "PASS (X.XXXs) <name>\n" or "FAIL (X.XXXs) <name>\n" to stdout.
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

	dotData := constant.NewCharArrayFromString(".")
	dotGlobal := c.module.NewGlobalDef(".str.dot", dotData)
	dotGlobal.Immutable = true
	dotGlobal.Linkage = enum.LinkagePrivate

	timeSuffixData := constant.NewCharArrayFromString("s) ")
	timeSuffixGlobal := c.module.NewGlobalDef(".str.time_suffix", timeSuffixData)
	timeSuffixGlobal.Immutable = true
	timeSuffixGlobal.Linkage = enum.LinkagePrivate

	stdout := constant.NewInt(irtypes.I32, 1)
	name := fn.Params[0]      // i8*
	failed := fn.Params[1]    // i32
	elapsedNs := fn.Params[2] // i64

	// Branch on failed != 0
	entry := fn.NewBlock(".entry")
	thenBlock := fn.NewBlock("fail")
	elseBlock := fn.NewBlock("pass")
	mergeBlock := fn.NewBlock("merge")

	isFailed := entry.NewICmp(enum.IPredNE, failed, constant.NewInt(irtypes.I32, 0))
	entry.NewCondBr(isFailed, thenBlock, elseBlock)

	// "FAIL (" branch
	failPtr := thenBlock.NewGetElementPtr(failGlobal.ContentType, failGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	thenBlock.NewCall(c.palWrite, stdout, failPtr, constant.NewInt(irtypes.I64, 6))
	thenBlock.NewBr(mergeBlock)

	// "PASS (" branch
	passPtr := elseBlock.NewGetElementPtr(passGlobal.ContentType, passGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	elseBlock.NewCall(c.palWrite, stdout, passPtr, constant.NewInt(irtypes.I64, 6))
	elseBlock.NewBr(mergeBlock)

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

// defineTestSummaryBody adds a function body to promise_test_summary(i32 %passed, i32 %failed, i32 %skipped).
// Writes "<passed> passed, <failed> failed[, <skipped> skipped]\n" to stdout via pal_write.
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

	skippedPrefixData := constant.NewCharArrayFromString(", ")
	skippedPrefixGlobal := c.module.NewGlobalDef(".str.skipped_prefix", skippedPrefixData)
	skippedPrefixGlobal.Immutable = true
	skippedPrefixGlobal.Linkage = enum.LinkagePrivate

	skippedSuffixData := constant.NewCharArrayFromString(" skipped")
	skippedSuffixGlobal := c.module.NewGlobalDef(".str.skipped_suffix", skippedSuffixData)
	skippedSuffixGlobal.Immutable = true
	skippedSuffixGlobal.Linkage = enum.LinkagePrivate

	stdout := constant.NewInt(irtypes.I32, 1)
	passed := fn.Params[0]  // i32
	failed := fn.Params[1]  // i32
	skipped := fn.Params[2] // i32

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

	// Write ", "
	sPrefixPtr := printSkipBlock.NewGetElementPtr(skippedPrefixGlobal.ContentType, skippedPrefixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	printSkipBlock.NewCall(c.palWrite, stdout, sPrefixPtr, constant.NewInt(irtypes.I64, 2))

	// Convert skipped count to string
	skippedI64 := printSkipBlock.NewSExt(skipped, irtypes.I64)
	skippedStr := printSkipBlock.NewCall(c.funcs["promise_int_to_string"], skippedI64)
	skippedDataPtr, skippedDataLen := c.extractStringDataLenFromInstance(printSkipBlock, skippedStr)
	printSkipBlock.NewCall(c.palWrite, stdout, skippedDataPtr, skippedDataLen)
	printSkipBlock.NewCall(c.palFree, skippedStr)

	// Write " skipped"
	sSuffixPtr := printSkipBlock.NewGetElementPtr(skippedSuffixGlobal.ContentType, skippedSuffixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	printSkipBlock.NewCall(c.palWrite, stdout, sSuffixPtr, constant.NewInt(irtypes.I64, 8))
	printSkipBlock.NewBr(afterSkipBlock)

	// Write "\n"
	nlPtr := afterSkipBlock.NewGetElementPtr(c.newlineGlobal.ContentType, c.newlineGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	afterSkipBlock.NewCall(c.palWrite, stdout, nlPtr, constant.NewInt(irtypes.I64, 1))

	afterSkipBlock.NewRet(nil)
}
