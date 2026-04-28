package codegen

import (
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
// because declareExterns stores them in c.funcs under the Promise name (e.g. "_print_int")
// while the LLVM function is named by the C name (e.g. "promise_print_int").
func (c *Compiler) definePALBodies() {
	// Global constants shared by print/panic functions
	nlData := constant.NewCharArrayFromString("\n")
	c.newlineGlobal = c.module.NewGlobalDef(".str.newline", nlData)
	c.newlineGlobal.Immutable = true

	panicData := constant.NewCharArrayFromString("panic: ")
	c.panicPrefixGlobal = c.module.NewGlobalDef(".str.panic_prefix", panicData)
	c.panicPrefixGlobal.Immutable = true

	// Build a lookup by LLVM function name for declarations without bodies
	irFuncByName := make(map[string]*ir.Func)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			irFuncByName[fn.Name()] = fn
		}
	}

	// Print functions — declared by extern from std/io.pr
	if fn, ok := irFuncByName["promise_print_string"]; ok {
		c.definePrintStringBody(fn)
	}
	if fn, ok := irFuncByName["promise_print_int"]; ok {
		c.definePrintIntBody(fn)
	}
	if fn, ok := irFuncByName["promise_print_f64"]; ok {
		c.definePrintF64Body(fn)
	}
	if fn, ok := irFuncByName["promise_print_bool"]; ok {
		c.definePrintBoolBody(fn)
	}

	// Panic functions
	if fn, ok := irFuncByName["promise_panic"]; ok {
		c.definePanicBody(fn)
	}
	if fn, ok := irFuncByName["promise_panic_msg"]; ok {
		c.definePanicMsgBody(fn)
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
func (c *Compiler) definePrintStringBody(fn *ir.Func) {
	entry := fn.NewBlock("entry")
	stdout := constant.NewInt(irtypes.I32, 1)

	dataPtr, dataLen := c.extractStringDataLen(entry, fn.Params[0])

	// Write string data
	entry.NewCall(c.palWrite, stdout, dataPtr, dataLen)
	// Write newline
	c.emitWriteNewline(entry, stdout)
	entry.NewRet(nil)
}

// definePrintIntBody adds a function body to promise_print_int(i8* %x).
// Extracts raw i64, converts to string via promise_int_to_string, writes via pal_write.
func (c *Compiler) definePrintIntBody(fn *ir.Func) {
	entry := fn.NewBlock("entry")
	stdout := constant.NewInt(irtypes.I32, 1)

	intLayout := c.layouts[types.TypInt]
	valType := intLayout.Value.LLVMType

	// Bitcast i8* → %promise_int_v*
	valPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(valType))

	// GEP to field 2 (raw), load i64
	rawPtr := entry.NewGetElementPtr(valType, valPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	raw := entry.NewLoad(irtypes.I64, rawPtr)

	// Convert to string instance
	strInst := entry.NewCall(c.funcs["promise_int_to_string"], raw)

	// Extract data/len from string instance
	dataPtr, dataLen := c.extractStringDataLenFromInstance(entry, strInst)

	entry.NewCall(c.palWrite, stdout, dataPtr, dataLen)
	c.emitWriteNewline(entry, stdout)
	// Free the temporary string instance allocated by promise_int_to_string
	entry.NewCall(c.palFree, strInst)
	entry.NewRet(nil)
}

// definePrintF64Body adds a function body to promise_print_f64(i8* %x).
// Extracts raw double, converts to string via promise_f64_to_string, writes via pal_write.
func (c *Compiler) definePrintF64Body(fn *ir.Func) {
	entry := fn.NewBlock("entry")
	stdout := constant.NewInt(irtypes.I32, 1)

	f64Layout := c.layouts[types.TypF64]
	valType := f64Layout.Value.LLVMType

	// Bitcast i8* → %promise_f64_v*
	valPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(valType))

	// GEP to field 2 (raw), load double
	rawPtr := entry.NewGetElementPtr(valType, valPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	raw := entry.NewLoad(irtypes.Double, rawPtr)

	// Convert to string instance
	strInst := entry.NewCall(c.funcs["promise_f64_to_string"], raw)

	// Extract data/len from string instance
	dataPtr, dataLen := c.extractStringDataLenFromInstance(entry, strInst)

	entry.NewCall(c.palWrite, stdout, dataPtr, dataLen)
	c.emitWriteNewline(entry, stdout)
	// Free the temporary string instance allocated by promise_f64_to_string
	entry.NewCall(c.palFree, strInst)
	entry.NewRet(nil)
}

// definePrintBoolBody adds a function body to promise_print_bool(i8* %x).
// Extracts raw i8, converts to string via promise_bool_to_string, writes via pal_write.
func (c *Compiler) definePrintBoolBody(fn *ir.Func) {
	entry := fn.NewBlock("entry")
	stdout := constant.NewInt(irtypes.I32, 1)

	boolLayout := c.layouts[types.TypBool]
	valType := boolLayout.Value.LLVMType

	// Bitcast i8* → %promise_bool_v*
	valPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(valType))

	// GEP to field 2 (raw), load i8
	rawPtr := entry.NewGetElementPtr(valType, valPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	raw := entry.NewLoad(irtypes.I8, rawPtr)

	// Convert to string instance
	strInst := entry.NewCall(c.funcs["promise_bool_to_string"], raw)

	// Extract data/len from string instance
	dataPtr, dataLen := c.extractStringDataLenFromInstance(entry, strInst)

	entry.NewCall(c.palWrite, stdout, dataPtr, dataLen)
	c.emitWriteNewline(entry, stdout)
	// Free the temporary string instance allocated by promise_bool_to_string
	entry.NewCall(c.palFree, strInst)
	entry.NewRet(nil)
}

// definePanicBody adds a function body to promise_panic(i8* %msg).
// msg is a null-terminated C string. Writes "panic: <msg>\n" to stderr and exits.
func (c *Compiler) definePanicBody(fn *ir.Func) {
	entry := fn.NewBlock("entry")
	stderr := constant.NewInt(irtypes.I32, 2)

	// Write "panic: " prefix
	prefixPtr := entry.NewGetElementPtr(c.panicPrefixGlobal.ContentType, c.panicPrefixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	entry.NewCall(c.palWrite, stderr, prefixPtr, constant.NewInt(irtypes.I64, 7))

	// Get message length via strlen
	msgLen := entry.NewCall(c.funcs["strlen"], fn.Params[0])

	// Write message
	entry.NewCall(c.palWrite, stderr, fn.Params[0], msgLen)

	// Write newline
	c.emitWriteNewline(entry, stderr)

	// Exit with code 1
	entry.NewCall(c.palExit, constant.NewInt(irtypes.I32, 1))
	entry.NewUnreachable()
	// noreturn already set on promise_panic in declareIntrinsics
}

// definePanicMsgBody adds a function body to promise_panic_msg(i8* %s).
// s points to a promise_string_v. Writes "panic: <msg>\n" to stderr and exits.
func (c *Compiler) definePanicMsgBody(fn *ir.Func) {
	entry := fn.NewBlock("entry")
	stderr := constant.NewInt(irtypes.I32, 2)

	// Write "panic: " prefix
	prefixPtr := entry.NewGetElementPtr(c.panicPrefixGlobal.ContentType, c.panicPrefixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	entry.NewCall(c.palWrite, stderr, prefixPtr, constant.NewInt(irtypes.I64, 7))

	// Extract data/len from string value struct
	dataPtr, dataLen := c.extractStringDataLen(entry, fn.Params[0])

	// Write message
	entry.NewCall(c.palWrite, stderr, dataPtr, dataLen)

	// Write newline
	c.emitWriteNewline(entry, stderr)

	// Exit with code 1
	entry.NewCall(c.palExit, constant.NewInt(irtypes.I32, 1))
	entry.NewUnreachable()

	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoReturn)
}

// defineTestRunFunc defines a codegen-emitted promise_test_run(i8* %fn) → i32.
// Replaces the C extern (fork/waitpid) with thread-based execution via PAL.
// The test function runs on a separate thread. If it completes normally, returns 0.
// If it panics, pal_exit terminates the process (no fork isolation — same as Go's testing).
func (c *Compiler) defineTestRunFunc() *ir.Func {
	fn := c.module.NewFunc("promise_test_run", irtypes.I32,
		ir.NewParam("fn", irtypes.I8Ptr))
	entry := fn.NewBlock("entry")

	// The test function is void(). We need a trampoline with i8*(i8*) signature
	// that calls the void function and returns null.
	// Generate a single shared trampoline: .test_trampoline(i8* %fn_ptr) → i8*
	// It bitcasts fn_ptr to void()* and calls it.
	trampoline := c.defineTestTrampoline()
	trampolinePtr := entry.NewBitCast(trampoline, irtypes.I8Ptr)

	// Spawn thread: pass the test function pointer as the arg
	handle := entry.NewCall(c.palThreadCreate, trampolinePtr, fn.Params[0])

	// Join thread (waits for completion)
	entry.NewCall(c.palThreadJoin, handle)

	// If we get here, the test passed (panics terminate the process)
	entry.NewRet(constant.NewInt(irtypes.I32, 0))
	return fn
}

// defineTestTrampoline generates a shared trampoline for test runner threads.
// Signature: i8*(i8* %fn_ptr) — casts fn_ptr to void()* and calls it.
func (c *Compiler) defineTestTrampoline() *ir.Func {
	trampoline := c.module.NewFunc(".test_trampoline", irtypes.I8Ptr,
		ir.NewParam("fn_ptr", irtypes.I8Ptr))
	entry := trampoline.NewBlock("entry")

	// Bitcast i8* → void()*
	voidFnPtrType := irtypes.NewPointer(irtypes.NewFunc(irtypes.Void))
	typedFn := entry.NewBitCast(trampoline.Params[0], voidFnPtrType)

	// Call the test function
	entry.NewCall(typedFn)

	// Return null (pthread expects i8* return)
	entry.NewRet(constant.NewNull(irtypes.I8Ptr))
	return trampoline
}

// defineTestPrintResultBody adds a function body to promise_test_print_result(i8* %name, i32 %failed).
// Writes "PASS <name>\n" or "FAIL <name>\n" to stdout via pal_write.
func (c *Compiler) defineTestPrintResultBody(fn *ir.Func) {
	// Global constants for prefix strings
	passData := constant.NewCharArrayFromString("PASS ")
	passGlobal := c.module.NewGlobalDef(".str.pass_prefix", passData)
	passGlobal.Immutable = true

	failData := constant.NewCharArrayFromString("FAIL ")
	failGlobal := c.module.NewGlobalDef(".str.fail_prefix", failData)
	failGlobal.Immutable = true

	stdout := constant.NewInt(irtypes.I32, 1)
	name := fn.Params[0]   // i8* (C string, null-terminated)
	failed := fn.Params[1] // i32

	// Branch on failed != 0
	entry := fn.NewBlock("entry")
	thenBlock := fn.NewBlock("fail")
	elseBlock := fn.NewBlock("pass")
	mergeBlock := fn.NewBlock("merge")

	isFailed := entry.NewICmp(enum.IPredNE, failed, constant.NewInt(irtypes.I32, 0))
	entry.NewCondBr(isFailed, thenBlock, elseBlock)

	// "FAIL " branch
	failPtr := thenBlock.NewGetElementPtr(failGlobal.ContentType, failGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	thenBlock.NewCall(c.palWrite, stdout, failPtr, constant.NewInt(irtypes.I64, 5))
	thenBlock.NewBr(mergeBlock)

	// "PASS " branch
	passPtr := elseBlock.NewGetElementPtr(passGlobal.ContentType, passGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	elseBlock.NewCall(c.palWrite, stdout, passPtr, constant.NewInt(irtypes.I64, 5))
	elseBlock.NewBr(mergeBlock)

	// Write name + newline
	nameLen := mergeBlock.NewCall(c.funcs["strlen"], name)
	mergeBlock.NewCall(c.palWrite, stdout, name, nameLen)
	c.emitWriteNewline(mergeBlock, stdout)
	mergeBlock.NewRet(nil)
}

// defineTestSummaryBody adds a function body to promise_test_summary(i32 %passed, i32 %failed).
// Writes "\n<passed> passed, <failed> failed\n" to stdout via pal_write.
func (c *Compiler) defineTestSummaryBody(fn *ir.Func) {
	// Global constants
	passedSuffixData := constant.NewCharArrayFromString(" passed, ")
	passedSuffixGlobal := c.module.NewGlobalDef(".str.passed_suffix", passedSuffixData)
	passedSuffixGlobal.Immutable = true

	failedSuffixData := constant.NewCharArrayFromString(" failed\n")
	failedSuffixGlobal := c.module.NewGlobalDef(".str.failed_suffix", failedSuffixData)
	failedSuffixGlobal.Immutable = true

	stdout := constant.NewInt(irtypes.I32, 1)
	passed := fn.Params[0] // i32
	failed := fn.Params[1] // i32

	entry := fn.NewBlock("entry")

	// Write leading newline
	c.emitWriteNewline(entry, stdout)

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

	// Write " failed\n"
	fSuffixPtr := entry.NewGetElementPtr(failedSuffixGlobal.ContentType, failedSuffixGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	entry.NewCall(c.palWrite, stdout, fSuffixPtr, constant.NewInt(irtypes.I64, 8))

	entry.NewRet(nil)
}
