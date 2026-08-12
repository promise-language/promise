package codegen

import (
	"fmt"
	"math"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// stringLenMask is 0x7FFFFFFFFFFFFFFF — masks off the literal flag (sign bit).
var stringLenMask = constant.NewInt(irtypes.I64, 0x7FFFFFFFFFFFFFFF)

// defineStringDropFunc emits an LLVM IR function that conditionally frees a
// string instance. Literal strings (sign bit set in len field) are in .rodata
// and must not be freed. Heap-allocated strings (positive len) are freed via pal_free.
func (c *Compiler) defineStringDropFunc() {
	ptrParam := ir.NewParam("ptr", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_string_drop", irtypes.Void, ptrParam)

	instType := strInstanceType()

	entry := fn.NewBlock(".entry")

	// Null check: string fields in types may be null (zero-initialized error paths).
	nullCheck := entry.NewICmp(enum.IPredEQ, ptrParam, constant.NewNull(irtypes.I8Ptr))
	checkBlk := fn.NewBlock("check")
	doneBlk := fn.NewBlock("done")
	entry.NewCondBr(nullCheck, doneBlk, checkBlk)

	typedPtr := checkBlk.NewBitCast(ptrParam, irtypes.NewPointer(instType))
	rawLen := loadStringLenRaw(checkBlk, typedPtr, instType)

	// If bit 63 is set, it's a literal in .rodata — don't free
	bit63 := checkBlk.NewAnd(rawLen, constant.NewInt(irtypes.I64, math.MinInt64))
	isLiteral := checkBlk.NewICmp(enum.IPredNE, bit63, constant.NewInt(irtypes.I64, 0))
	freeBlk := fn.NewBlock("free")
	checkBlk.NewCondBr(isLiteral, doneBlk, freeBlk)

	freeBlk.NewCall(c.palFree, ptrParam)
	freeBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)

	c.funcs["promise_string_drop"] = fn
}

// defineStringNewFunc emits an LLVM IR function that allocates and initializes
// a string instance. Replaces the C runtime promise_string_new.
// Allocates header (16 bytes) + data, copies data via @llvm.memcpy intrinsic.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringNewFunc() {
	dataParam := ir.NewParam("data", irtypes.I8Ptr)
	lenParam := ir.NewParam("len", irtypes.I64)
	fn := c.module.NewFunc("promise_string_new", irtypes.I8Ptr, dataParam, lenParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	headerSize := constant.NewInt(irtypes.I64, int64(c.typeSize(irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64))))

	// OOM panic message
	oomMsg := constant.NewCharArrayFromString("out of memory\x00")
	oomGlobal := c.module.NewGlobalDef(
		fmt.Sprintf(".str.oom.%d", c.strCounter), oomMsg)
	c.strCounter++
	oomGlobal.Immutable = true
	oomGlobal.Linkage = enum.LinkagePrivate

	// entry: allocate and null-check
	entry := fn.NewBlock(".entry")
	allocSize := entry.NewAdd(headerSize, lenParam)
	rawPtr := entry.NewCall(c.palAlloc, allocSize)
	isNull := entry.NewICmp(enum.IPredEQ, rawPtr, constant.NewNull(irtypes.I8Ptr))

	oomBlk := fn.NewBlock("oom")
	initBlk := fn.NewBlock("init")
	entry.NewCondBr(isNull, oomBlk, initBlk)

	// oom: panic
	msgPtr := oomBlk.NewGetElementPtr(oomGlobal.ContentType, oomGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	oomBlk.NewCall(c.funcs["promise_panic"], msgPtr)
	oomBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	// init: store fields and copy data
	typedPtr := initBlk.NewBitCast(rawPtr, irtypes.NewPointer(strInstanceType))

	variantPtr := initBlk.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	initBlk.NewStore(constant.NewNull(irtypes.I8Ptr), variantPtr)

	lenPtr := initBlk.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	initBlk.NewStore(lenParam, lenPtr)

	dataDst := initBlk.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	initBlk.NewCall(c.funcs["llvm.memcpy"], dataDst, dataParam, lenParam, constant.False)

	initBlk.NewRet(rawPtr)

	c.funcs["promise_string_new"] = fn
}

// defineStringConcatFunc emits an LLVM IR function that concatenates two strings.
// Replaces the C runtime promise_string_concat.
// Loads lengths from both inputs, allocates header + total, copies both data regions.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringConcatFunc() {
	aParam := ir.NewParam("a", irtypes.I8Ptr)
	bParam := ir.NewParam("b", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_string_concat", irtypes.I8Ptr, aParam, bParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	headerSize := constant.NewInt(irtypes.I64, int64(c.typeSize(irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64))))

	// OOM panic message
	oomMsg := constant.NewCharArrayFromString("out of memory\x00")
	oomGlobal := c.module.NewGlobalDef(
		fmt.Sprintf(".str.oom.%d", c.strCounter), oomMsg)
	c.strCounter++
	oomGlobal.Immutable = true
	oomGlobal.Linkage = enum.LinkagePrivate

	// entry: load lengths (masking off literal flag), compute total, allocate, null-check
	entry := fn.NewBlock(".entry")

	typedA := entry.NewBitCast(aParam, irtypes.NewPointer(strInstanceType))
	lenA := loadStringLen(entry, typedA, strInstanceType)

	typedB := entry.NewBitCast(bParam, irtypes.NewPointer(strInstanceType))
	lenB := loadStringLen(entry, typedB, strInstanceType)

	total := entry.NewAdd(lenA, lenB)
	allocSize := entry.NewAdd(headerSize, total)
	rawPtr := entry.NewCall(c.palAlloc, allocSize)
	isNull := entry.NewICmp(enum.IPredEQ, rawPtr, constant.NewNull(irtypes.I8Ptr))

	oomBlk := fn.NewBlock("oom")
	initBlk := fn.NewBlock("init")
	entry.NewCondBr(isNull, oomBlk, initBlk)

	// oom: panic
	msgPtr := oomBlk.NewGetElementPtr(oomGlobal.ContentType, oomGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	oomBlk.NewCall(c.funcs["promise_panic"], msgPtr)
	oomBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	// init: store header fields and copy both data regions
	typedNew := initBlk.NewBitCast(rawPtr, irtypes.NewPointer(strInstanceType))

	variantPtr := initBlk.NewGetElementPtr(strInstanceType, typedNew,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	initBlk.NewStore(constant.NewNull(irtypes.I8Ptr), variantPtr)

	lenPtr := initBlk.NewGetElementPtr(strInstanceType, typedNew,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	initBlk.NewStore(total, lenPtr)

	dataDst := initBlk.NewGetElementPtr(strInstanceType, typedNew,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	// Copy a's data
	dataA := initBlk.NewGetElementPtr(strInstanceType, typedA,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	initBlk.NewCall(c.funcs["llvm.memcpy"], dataDst, dataA, lenA, constant.False)

	// Copy b's data after a's
	dstOffset := initBlk.NewGetElementPtr(irtypes.I8, dataDst, lenA)
	dataB := initBlk.NewGetElementPtr(strInstanceType, typedB,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	initBlk.NewCall(c.funcs["llvm.memcpy"], dstOffset, dataB, lenB, constant.False)

	initBlk.NewRet(rawPtr)

	c.funcs["promise_string_concat"] = fn
}

// defineStringDirectEqFunc emits an LLVM IR function that compares two strings
// for equality. Used by the == and != operators. Takes direct i8* string pointers
// (not indirect like defineStringEqFunc which is used by Vector.contains).
// Returns i1 (true if equal). Replaces the C runtime promise_string_eq.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringDirectEqFunc() {
	aParam := ir.NewParam("a", irtypes.I8Ptr)
	bParam := ir.NewParam("b", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_string_eq", irtypes.I1, aParam, bParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	trueVal := constant.NewInt(irtypes.I1, 1)
	falseVal := constant.NewInt(irtypes.I1, 0)

	// Fast path: same pointer → equal
	entry := fn.NewBlock(".entry")
	samePtr := entry.NewICmp(enum.IPredEQ, aParam, bParam)
	samePtrBlk := fn.NewBlock("same_ptr")
	checkLenBlk := fn.NewBlock("check_len")
	entry.NewCondBr(samePtr, samePtrBlk, checkLenBlk)

	samePtrBlk.NewRet(trueVal)

	// Compare lengths (masking off literal flag)
	typedA := checkLenBlk.NewBitCast(aParam, irtypes.NewPointer(strInstanceType))
	lenA := loadStringLen(checkLenBlk, typedA, strInstanceType)

	typedB := checkLenBlk.NewBitCast(bParam, irtypes.NewPointer(strInstanceType))
	lenB := loadStringLen(checkLenBlk, typedB, strInstanceType)

	lenEq := checkLenBlk.NewICmp(enum.IPredEQ, lenA, lenB)
	lenNeqBlk := fn.NewBlock("len_neq")
	cmpDataBlk := fn.NewBlock("cmp_data")
	checkLenBlk.NewCondBr(lenEq, cmpDataBlk, lenNeqBlk)

	lenNeqBlk.NewRet(falseVal)

	// Compare data using memcmp (SIMD-accelerated)
	dataPtrA := cmpDataBlk.NewGetElementPtr(strInstanceType, typedA,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	dataPtrB := cmpDataBlk.NewGetElementPtr(strInstanceType, typedB,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	equalBlk := fn.NewBlock("equal")
	neqBlk := fn.NewBlock("not_equal")

	cmpResult := cmpDataBlk.NewCall(c.funcs["memcmp"], dataPtrA, dataPtrB, lenA)
	isEqual := cmpDataBlk.NewICmp(enum.IPredEQ, cmpResult, constant.NewInt(irtypes.I32, 0))
	cmpDataBlk.NewCondBr(isEqual, equalBlk, neqBlk)

	neqBlk.NewRet(falseVal)
	equalBlk.NewRet(trueVal)

	c.funcs["promise_string_eq"] = fn
}

// defineStringCompareFunc emits an LLVM IR function for lexicographic string
// comparison. Returns i32: negative if a<b, 0 if equal, positive if a>b.
// Uses memcmp on min(len_a, len_b) bytes, then compares lengths.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringCompareFunc() {
	aParam := ir.NewParam("a", irtypes.I8Ptr)
	bParam := ir.NewParam("b", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_string_compare", irtypes.I32, aParam, bParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	ci32 := func(v int64) *constant.Int { return constant.NewInt(irtypes.I32, v) }
	ci64 := func(v int64) *constant.Int { return constant.NewInt(irtypes.I64, v) }

	// Fast path: same pointer → equal
	entry := fn.NewBlock(".entry")
	samePtr := entry.NewICmp(enum.IPredEQ, aParam, bParam)
	samePtrBlk := fn.NewBlock("same_ptr")
	loadBlk := fn.NewBlock("load")
	entry.NewCondBr(samePtr, samePtrBlk, loadBlk)

	samePtrBlk.NewRet(ci32(0))

	// Load lengths (masking off literal flag) and data pointers
	typedA := loadBlk.NewBitCast(aParam, irtypes.NewPointer(strInstanceType))
	lenA := loadStringLen(loadBlk, typedA, strInstanceType)
	dataPtrA := loadBlk.NewGetElementPtr(strInstanceType, typedA, ci32(0), ci32(2), ci32(0))

	typedB := loadBlk.NewBitCast(bParam, irtypes.NewPointer(strInstanceType))
	lenB := loadStringLen(loadBlk, typedB, strInstanceType)
	dataPtrB := loadBlk.NewGetElementPtr(strInstanceType, typedB, ci32(0), ci32(2), ci32(0))

	// minLen = min(lenA, lenB)
	aLess := loadBlk.NewICmp(enum.IPredULT, lenA, lenB)
	minLen := loadBlk.NewSelect(aLess, lenA, lenB)

	// If minLen == 0, skip memcmp (both empty or one is empty)
	minIsZero := loadBlk.NewICmp(enum.IPredEQ, minLen, ci64(0))
	cmpDataBlk := fn.NewBlock("cmp_data")
	cmpLenBlk := fn.NewBlock("cmp_len")
	loadBlk.NewCondBr(minIsZero, cmpLenBlk, cmpDataBlk)

	// cmp_data: memcmp(data_a, data_b, minLen)
	cmpResult := cmpDataBlk.NewCall(c.funcs["memcmp"], dataPtrA, dataPtrB, minLen)
	cmpNonZero := cmpDataBlk.NewICmp(enum.IPredNE, cmpResult, ci32(0))
	retCmpBlk := fn.NewBlock("ret_cmp")
	cmpDataBlk.NewCondBr(cmpNonZero, retCmpBlk, cmpLenBlk)

	// ret_cmp: data differs, return memcmp result
	retCmpBlk.NewRet(cmpResult)

	// cmp_len: data is equal prefix, compare lengths: shorter < longer
	aLess2 := cmpLenBlk.NewICmp(enum.IPredULT, lenA, lenB)
	aGreater := cmpLenBlk.NewICmp(enum.IPredUGT, lenA, lenB)
	r := cmpLenBlk.NewSelect(aGreater, ci32(1), ci32(0))
	r = cmpLenBlk.NewSelect(aLess2, ci32(-1), r)
	cmpLenBlk.NewRet(r)

	c.funcs["promise_string_compare"] = fn
}

// defineStringToUpperFunc emits an LLVM IR function that returns a new string
// with ASCII lowercase letters (a-z) converted to uppercase (A-Z).
// Non-ASCII bytes are left unchanged. O(n) with single allocation.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringToUpperFunc() {
	c.defineStringCaseFunc("promise_string_to_upper", 'a', 'z', -32)
}

// defineStringToLowerFunc emits an LLVM IR function that returns a new string
// with ASCII uppercase letters (A-Z) converted to lowercase (a-z).
// Non-ASCII bytes are left unchanged. O(n) with single allocation.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringToLowerFunc() {
	c.defineStringCaseFunc("promise_string_to_lower", 'A', 'Z', 32)
}

// defineStringCaseFunc emits an LLVM IR function for ASCII case conversion.
// Bytes in [rangeStart, rangeEnd] are shifted by delta; others unchanged.
func (c *Compiler) defineStringCaseFunc(name string, rangeStart, rangeEnd byte, delta int64) {
	sParam := ir.NewParam("s", irtypes.I8Ptr)
	fn := c.module.NewFunc(name, irtypes.I8Ptr, sParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	ci32 := func(v int64) *constant.Int { return constant.NewInt(irtypes.I32, v) }
	ci64 := func(v int64) *constant.Int { return constant.NewInt(irtypes.I64, v) }
	ci8 := func(v int64) *constant.Int { return constant.NewInt(irtypes.I8, v) }

	entry := fn.NewBlock(".entry")

	// Load length (masking off literal flag) and data pointer
	typedS := entry.NewBitCast(sParam, irtypes.NewPointer(strInstanceType))
	sLen := loadStringLen(entry, typedS, strInstanceType)
	sDataPtr := entry.NewGetElementPtr(strInstanceType, typedS, ci32(0), ci32(2), ci32(0))

	// Create new string: promise_string_new(data, len) — copies bytes
	newStr := entry.NewCall(c.funcs["promise_string_new"], sDataPtr, sLen)

	// Get data pointer of new string for in-place modification
	typedNew := entry.NewBitCast(newStr, irtypes.NewPointer(strInstanceType))
	newDataPtr := entry.NewGetElementPtr(strInstanceType, typedNew, ci32(0), ci32(2), ci32(0))

	// If empty, skip loop
	isEmpty := entry.NewICmp(enum.IPredEQ, sLen, ci64(0))
	loopHdr := fn.NewBlock("loop_hdr")
	doneBlk := fn.NewBlock("done")
	entry.NewCondBr(isEmpty, doneBlk, loopHdr)

	// loop_hdr: i = phi(0 from entry, i+1 from loop_update)
	iPhi := loopHdr.NewPhi(ir.NewIncoming(ci64(0), entry))
	bytePtr := loopHdr.NewGetElementPtr(irtypes.I8, newDataPtr, iPhi)
	b := loopHdr.NewLoad(irtypes.I8, bytePtr)

	// Check if byte is in [rangeStart, rangeEnd]
	geStart := loopHdr.NewICmp(enum.IPredUGE, b, ci8(int64(rangeStart)))
	leEnd := loopHdr.NewICmp(enum.IPredULE, b, ci8(int64(rangeEnd)))
	inRange := loopHdr.NewAnd(geStart, leEnd)

	convertBlk := fn.NewBlock("convert")
	updateBlk := fn.NewBlock("update")
	loopHdr.NewCondBr(inRange, convertBlk, updateBlk)

	// convert: shift the byte
	shifted := convertBlk.NewAdd(b, ci8(delta))
	convertBlk.NewStore(shifted, bytePtr)
	convertBlk.NewBr(updateBlk)

	// update: i++, check bound
	iNext := updateBlk.NewAdd(iPhi, ci64(1))
	iPhi.Incs = append(iPhi.Incs, ir.NewIncoming(iNext, updateBlk))
	atEnd := updateBlk.NewICmp(enum.IPredEQ, iNext, sLen)
	updateBlk.NewCondBr(atEnd, doneBlk, loopHdr)

	// done: return new string
	doneBlk.NewRet(newStr)

	c.funcs[name] = fn
}

// defineStringRepeatFunc emits an LLVM IR function that returns a new string
// consisting of the input string repeated n times. O(n*len) with single allocation.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringRepeatFunc() {
	sParam := ir.NewParam("s", irtypes.I8Ptr)
	nParam := ir.NewParam("n", irtypes.I64)
	fn := c.module.NewFunc("promise_string_repeat", irtypes.I8Ptr, sParam, nParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	ci32 := func(v int64) *constant.Int { return constant.NewInt(irtypes.I32, v) }
	ci64 := func(v int64) *constant.Int { return constant.NewInt(irtypes.I64, v) }

	entry := fn.NewBlock(".entry")

	// Load source length (masking off literal flag) and data pointer
	typedS := entry.NewBitCast(sParam, irtypes.NewPointer(strInstanceType))
	sLen := loadStringLen(entry, typedS, strInstanceType)
	sDataPtr := entry.NewGetElementPtr(strInstanceType, typedS, ci32(0), ci32(2), ci32(0))

	// If n <= 0 or sLen == 0, return empty string (reuse promise_string_new with len=0)
	nLeZero := entry.NewICmp(enum.IPredSLE, nParam, ci64(0))
	sLenZero := entry.NewICmp(enum.IPredEQ, sLen, ci64(0))
	skipLoop := entry.NewOr(nLeZero, sLenZero)
	allocBlk := fn.NewBlock("alloc")
	emptyBlk := fn.NewBlock("empty")
	entry.NewCondBr(skipLoop, emptyBlk, allocBlk)

	// empty: return empty string
	emptyStr := emptyBlk.NewCall(c.funcs["promise_string_new"], constant.NewNull(irtypes.I8Ptr), ci64(0))
	emptyBlk.NewRet(emptyStr)

	// alloc: allocate result string: header + sLen*n bytes
	totalLen := allocBlk.NewMul(sLen, nParam)
	headerSize := constant.NewInt(irtypes.I64, int64(c.typeSize(irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64))))
	allocSize := allocBlk.NewAdd(headerSize, totalLen)
	rawPtr := allocBlk.NewCall(c.palAlloc, allocSize)

	// Set up the header: variant = null, len = totalLen
	typedResult := allocBlk.NewBitCast(rawPtr, irtypes.NewPointer(strInstanceType))
	variantPtr := allocBlk.NewGetElementPtr(strInstanceType, typedResult, ci32(0), ci32(0))
	allocBlk.NewStore(constant.NewNull(irtypes.I8Ptr), variantPtr)
	resultLenPtr := allocBlk.NewGetElementPtr(strInstanceType, typedResult, ci32(0), ci32(1))
	allocBlk.NewStore(totalLen, resultLenPtr)
	resultDataPtr := allocBlk.NewGetElementPtr(strInstanceType, typedResult, ci32(0), ci32(2), ci32(0))

	loopHdr := fn.NewBlock("loop_hdr")
	doneBlk := fn.NewBlock("done")
	allocBlk.NewBr(loopHdr)

	// loop_hdr: i = phi(0, i+1), copy sLen bytes at offset i*sLen
	iPhi := loopHdr.NewPhi(ir.NewIncoming(ci64(0), allocBlk))
	offset := loopHdr.NewMul(iPhi, sLen)
	destPtr := loopHdr.NewGetElementPtr(irtypes.I8, resultDataPtr, offset)
	loopHdr.NewCall(c.funcs["llvm.memcpy"], destPtr, sDataPtr, sLen, constant.False)

	iNext := loopHdr.NewAdd(iPhi, ci64(1))
	iPhi.Incs = append(iPhi.Incs, ir.NewIncoming(iNext, loopHdr))
	atEnd := loopHdr.NewICmp(enum.IPredEQ, iNext, nParam)
	loopHdr.NewCondBr(atEnd, doneBlk, loopHdr)

	// done: return result
	doneBlk.NewRet(rawPtr)

	c.funcs["promise_string_repeat"] = fn
}

// defineStringTrimFunc emits an LLVM IR function that returns a new string
// with leading and trailing whitespace removed.
// Whitespace: space (32), tab (9), newline (10), carriage return (13).
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringTrimFunc() {
	sParam := ir.NewParam("s", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_string_trim", irtypes.I8Ptr, sParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)

	// entry: load len (masking off literal flag), get data pointer, alloca start/end
	entry := fn.NewBlock(".entry")
	typedS := entry.NewBitCast(sParam, irtypes.NewPointer(strInstanceType))
	sLen := loadStringLen(entry, typedS, strInstanceType)
	dataPtr := entry.NewGetElementPtr(strInstanceType, typedS,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	startA := entry.NewAlloca(irtypes.I64)
	entry.NewStore(zero64, startA)
	endA := entry.NewAlloca(irtypes.I64)
	entry.NewStore(sLen, endA)

	trimLeftHdr := fn.NewBlock("trim_left_hdr")
	trimLeftChk := fn.NewBlock("trim_left_chk")
	trimLeftAdv := fn.NewBlock("trim_left_adv")
	trimRightHdr := fn.NewBlock("trim_right_hdr")
	trimRightChk := fn.NewBlock("trim_right_chk")
	trimRightAdv := fn.NewBlock("trim_right_adv")
	buildResult := fn.NewBlock("build_result")

	entry.NewBr(trimLeftHdr)

	// trim_left_hdr: start < end?
	start := trimLeftHdr.NewLoad(irtypes.I64, startA)
	end := trimLeftHdr.NewLoad(irtypes.I64, endA)
	leftCond := trimLeftHdr.NewICmp(enum.IPredSLT, start, end)
	trimLeftHdr.NewCondBr(leftCond, trimLeftChk, trimRightHdr)

	// trim_left_chk: is data[start] whitespace?
	startVal := trimLeftChk.NewLoad(irtypes.I64, startA)
	bytePtr := trimLeftChk.NewGetElementPtr(irtypes.I8, dataPtr, startVal)
	b := trimLeftChk.NewLoad(irtypes.I8, bytePtr)
	isSp := trimLeftChk.NewICmp(enum.IPredEQ, b, constant.NewInt(irtypes.I8, 32))
	isTab := trimLeftChk.NewICmp(enum.IPredEQ, b, constant.NewInt(irtypes.I8, 9))
	isNL := trimLeftChk.NewICmp(enum.IPredEQ, b, constant.NewInt(irtypes.I8, 10))
	isCR := trimLeftChk.NewICmp(enum.IPredEQ, b, constant.NewInt(irtypes.I8, 13))
	ws1 := trimLeftChk.NewOr(isSp, isTab)
	ws2 := trimLeftChk.NewOr(ws1, isNL)
	isWs := trimLeftChk.NewOr(ws2, isCR)
	trimLeftChk.NewCondBr(isWs, trimLeftAdv, trimRightHdr)

	// trim_left_adv: start++
	startCur := trimLeftAdv.NewLoad(irtypes.I64, startA)
	nextStart := trimLeftAdv.NewAdd(startCur, one64)
	trimLeftAdv.NewStore(nextStart, startA)
	trimLeftAdv.NewBr(trimLeftHdr)

	// trim_right_hdr: end > start?
	endVal := trimRightHdr.NewLoad(irtypes.I64, endA)
	startVal2 := trimRightHdr.NewLoad(irtypes.I64, startA)
	rightCond := trimRightHdr.NewICmp(enum.IPredSGT, endVal, startVal2)
	trimRightHdr.NewCondBr(rightCond, trimRightChk, buildResult)

	// trim_right_chk: is data[end-1] whitespace?
	endVal2 := trimRightChk.NewLoad(irtypes.I64, endA)
	idxR := trimRightChk.NewSub(endVal2, one64)
	bytePtrR := trimRightChk.NewGetElementPtr(irtypes.I8, dataPtr, idxR)
	bR := trimRightChk.NewLoad(irtypes.I8, bytePtrR)
	isSpR := trimRightChk.NewICmp(enum.IPredEQ, bR, constant.NewInt(irtypes.I8, 32))
	isTabR := trimRightChk.NewICmp(enum.IPredEQ, bR, constant.NewInt(irtypes.I8, 9))
	isNLR := trimRightChk.NewICmp(enum.IPredEQ, bR, constant.NewInt(irtypes.I8, 10))
	isCRR := trimRightChk.NewICmp(enum.IPredEQ, bR, constant.NewInt(irtypes.I8, 13))
	wsR1 := trimRightChk.NewOr(isSpR, isTabR)
	wsR2 := trimRightChk.NewOr(wsR1, isNLR)
	isWsR := trimRightChk.NewOr(wsR2, isCRR)
	trimRightChk.NewCondBr(isWsR, trimRightAdv, buildResult)

	// trim_right_adv: end--
	endCur := trimRightAdv.NewLoad(irtypes.I64, endA)
	prevEnd := trimRightAdv.NewSub(endCur, one64)
	trimRightAdv.NewStore(prevEnd, endA)
	trimRightAdv.NewBr(trimRightHdr)

	// build_result: create new string from data[start..end]
	finalStart := buildResult.NewLoad(irtypes.I64, startA)
	finalEnd := buildResult.NewLoad(irtypes.I64, endA)
	newLen := buildResult.NewSub(finalEnd, finalStart)
	newDataPtr := buildResult.NewGetElementPtr(irtypes.I8, dataPtr, finalStart)
	result := buildResult.NewCall(c.funcs["promise_string_new"], newDataPtr, newLen)
	buildResult.NewRet(result)

	c.funcs["promise_string_trim"] = fn
}

// defineStringSplitFunc emits an LLVM IR function that splits a string by a
// separator and returns a vector (slice) of string pointers.
// Phase 1: count separator occurrences using memcmp.
// Phase 2: allocate vector {i64 len, i64 cap, data...} and fill with substrings.
// Empty separator returns a single-element slice containing the whole string.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringSplitFunc() {
	sParam := ir.NewParam("s", irtypes.I8Ptr)
	sepParam := ir.NewParam("sep", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_string_split", irtypes.I8Ptr, sParam, sepParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	ptrSize := constant.NewInt(irtypes.I64, int64(c.ptrSize()))
	headerSize := constant.NewInt(irtypes.I64, int64(c.typeSize(irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64))))
	vectorHeaderType := irtypes.NewStruct(irtypes.I64, irtypes.I64) // {len, cap}

	// OOM panic message
	oomMsg := constant.NewCharArrayFromString("out of memory\x00")
	oomGlobal := c.module.NewGlobalDef(
		fmt.Sprintf(".str.oom.%d", c.strCounter), oomMsg)
	c.strCounter++
	oomGlobal.Immutable = true
	oomGlobal.Linkage = enum.LinkagePrivate

	// entry: load string fields (masking off literal flag), set up allocas
	entry := fn.NewBlock(".entry")

	typedS := entry.NewBitCast(sParam, irtypes.NewPointer(strInstanceType))
	sLen := loadStringLen(entry, typedS, strInstanceType)
	sDataPtr := entry.NewGetElementPtr(strInstanceType, typedS,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	typedSep := entry.NewBitCast(sepParam, irtypes.NewPointer(strInstanceType))
	sepLen := loadStringLen(entry, typedSep, strInstanceType)
	sepDataPtr := entry.NewGetElementPtr(strInstanceType, typedSep,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	// All allocas in entry block
	countA := entry.NewAlloca(irtypes.I64)
	entry.NewStore(one64, countA) // count starts at 1
	iA := entry.NewAlloca(irtypes.I64)
	entry.NewStore(zero64, iA)
	posA := entry.NewAlloca(irtypes.I64)
	entry.NewStore(zero64, posA)
	idxA := entry.NewAlloca(irtypes.I64)
	entry.NewStore(zero64, idxA)

	sepEmpty := entry.NewICmp(enum.IPredEQ, sepLen, zero64)

	// Create all blocks
	countHdr := fn.NewBlock("count_hdr")
	countBody := fn.NewBlock("count_body")
	countMatch := fn.NewBlock("count_match")
	countNext := fn.NewBlock("count_next")
	allocBlk := fn.NewBlock("alloc")
	oomBlk := fn.NewBlock("oom")
	initHdr := fn.NewBlock("init_hdr")
	emptySep := fn.NewBlock("empty_sep")
	splitInit := fn.NewBlock("split_init")
	splitHdr := fn.NewBlock("split_hdr")
	splitBody := fn.NewBlock("split_body")
	splitMatch := fn.NewBlock("split_match")
	splitNext := fn.NewBlock("split_next")
	splitTail := fn.NewBlock("split_tail")
	doneBlk := fn.NewBlock("done")

	entry.NewCondBr(sepEmpty, allocBlk, countHdr)

	// ===== Phase 1: Count separators =====

	// count_hdr: i <= sLen - sepLen?
	iVal := countHdr.NewLoad(irtypes.I64, iA)
	limit := countHdr.NewSub(sLen, sepLen)
	countCond := countHdr.NewICmp(enum.IPredSLE, iVal, limit)
	countHdr.NewCondBr(countCond, countBody, allocBlk)

	// count_body: memcmp
	iVal2 := countBody.NewLoad(irtypes.I64, iA)
	curPtr := countBody.NewGetElementPtr(irtypes.I8, sDataPtr, iVal2)
	cmpResult := countBody.NewCall(c.funcs["memcmp"], curPtr, sepDataPtr, sepLen)
	isMatch := countBody.NewICmp(enum.IPredEQ, cmpResult, constant.NewInt(irtypes.I32, 0))
	countBody.NewCondBr(isMatch, countMatch, countNext)

	// count_match: count++, i += sepLen - 1
	cnt := countMatch.NewLoad(irtypes.I64, countA)
	cnt1 := countMatch.NewAdd(cnt, one64)
	countMatch.NewStore(cnt1, countA)
	iCur := countMatch.NewLoad(irtypes.I64, iA)
	skipI := countMatch.NewAdd(iCur, sepLen)
	skipIM1 := countMatch.NewSub(skipI, one64)
	countMatch.NewStore(skipIM1, iA)
	countMatch.NewBr(countNext)

	// count_next: i++
	iVal3 := countNext.NewLoad(irtypes.I64, iA)
	iNext := countNext.NewAdd(iVal3, one64)
	countNext.NewStore(iNext, iA)
	countNext.NewBr(countHdr)

	// ===== Phase 2: Allocate slice =====

	// alloc: malloc(16 + count * 8)
	count := allocBlk.NewLoad(irtypes.I64, countA)
	dataSize := allocBlk.NewMul(count, ptrSize)
	totalSize := allocBlk.NewAdd(headerSize, dataSize)
	rawSlice := allocBlk.NewCall(c.palAlloc, totalSize)
	isNull := allocBlk.NewICmp(enum.IPredEQ, rawSlice, constant.NewNull(irtypes.I8Ptr))
	allocBlk.NewCondBr(isNull, oomBlk, initHdr)

	// oom: panic
	msgPtr := oomBlk.NewGetElementPtr(oomGlobal.ContentType, oomGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	oomBlk.NewCall(c.funcs["promise_panic"], msgPtr)
	oomBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	// init_hdr: store len and cap
	hdrPtr := initHdr.NewBitCast(rawSlice, irtypes.NewPointer(vectorHeaderType))
	lenField := initHdr.NewGetElementPtr(vectorHeaderType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	initHdr.NewStore(count, lenField)
	capField := initHdr.NewGetElementPtr(vectorHeaderType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	initHdr.NewStore(count, capField)
	sepEmpty2 := initHdr.NewICmp(enum.IPredEQ, sepLen, zero64)
	initHdr.NewCondBr(sepEmpty2, emptySep, splitInit)

	// ===== Phase 2a: Empty separator → single-element result =====

	elemBase := emptySep.NewGetElementPtr(irtypes.I8, rawSlice, headerSize)
	elemTyped := emptySep.NewBitCast(elemBase, irtypes.NewPointer(irtypes.I8Ptr))
	wholeStr := emptySep.NewCall(c.funcs["promise_string_new"], sDataPtr, sLen)
	emptySep.NewStore(wholeStr, elemTyped)
	emptySep.NewBr(doneBlk)

	// ===== Phase 2b: Split loop =====

	// split_init: reset loop vars
	splitInit.NewStore(zero64, iA)
	splitInit.NewStore(zero64, posA)
	splitInit.NewStore(zero64, idxA)
	splitInit.NewBr(splitHdr)

	// split_hdr: i <= sLen - sepLen?
	siVal := splitHdr.NewLoad(irtypes.I64, iA)
	sLimit := splitHdr.NewSub(sLen, sepLen)
	sCond := splitHdr.NewICmp(enum.IPredSLE, siVal, sLimit)
	splitHdr.NewCondBr(sCond, splitBody, splitTail)

	// split_body: memcmp
	siVal2 := splitBody.NewLoad(irtypes.I64, iA)
	sCurPtr := splitBody.NewGetElementPtr(irtypes.I8, sDataPtr, siVal2)
	sCmpResult := splitBody.NewCall(c.funcs["memcmp"], sCurPtr, sepDataPtr, sepLen)
	sIsMatch := splitBody.NewICmp(enum.IPredEQ, sCmpResult, constant.NewInt(irtypes.I32, 0))
	splitBody.NewCondBr(sIsMatch, splitMatch, splitNext)

	// split_match: create substring, store in result
	pos := splitMatch.NewLoad(irtypes.I64, posA)
	idx := splitMatch.NewLoad(irtypes.I64, idxA)
	matchI := splitMatch.NewLoad(irtypes.I64, iA)
	subLen := splitMatch.NewSub(matchI, pos)
	subPtr := splitMatch.NewGetElementPtr(irtypes.I8, sDataPtr, pos)
	newStr := splitMatch.NewCall(c.funcs["promise_string_new"], subPtr, subLen)
	// Store at rawSlice + 16 + idx * 8
	elemOff := splitMatch.NewMul(idx, ptrSize)
	elemOff2 := splitMatch.NewAdd(headerSize, elemOff)
	elemPtr := splitMatch.NewGetElementPtr(irtypes.I8, rawSlice, elemOff2)
	elemPtrTyped := splitMatch.NewBitCast(elemPtr, irtypes.NewPointer(irtypes.I8Ptr))
	splitMatch.NewStore(newStr, elemPtrTyped)
	// Update pos = i + sepLen, idx++
	newPos := splitMatch.NewAdd(matchI, sepLen)
	splitMatch.NewStore(newPos, posA)
	nextIdx := splitMatch.NewAdd(idx, one64)
	splitMatch.NewStore(nextIdx, idxA)
	// i += sepLen - 1 (split_next adds 1 more)
	skipSI := splitMatch.NewAdd(matchI, sepLen)
	skipSIM1 := splitMatch.NewSub(skipSI, one64)
	splitMatch.NewStore(skipSIM1, iA)
	splitMatch.NewBr(splitNext)

	// split_next: i++
	siVal3 := splitNext.NewLoad(irtypes.I64, iA)
	siNext := splitNext.NewAdd(siVal3, one64)
	splitNext.NewStore(siNext, iA)
	splitNext.NewBr(splitHdr)

	// split_tail: store final substring from pos to sLen
	tailPos := splitTail.NewLoad(irtypes.I64, posA)
	tailIdx := splitTail.NewLoad(irtypes.I64, idxA)
	tailLen := splitTail.NewSub(sLen, tailPos)
	tailPtr := splitTail.NewGetElementPtr(irtypes.I8, sDataPtr, tailPos)
	tailStr := splitTail.NewCall(c.funcs["promise_string_new"], tailPtr, tailLen)
	tailElemOff := splitTail.NewMul(tailIdx, ptrSize)
	tailElemOff2 := splitTail.NewAdd(headerSize, tailElemOff)
	tailElemPtr := splitTail.NewGetElementPtr(irtypes.I8, rawSlice, tailElemOff2)
	tailElemPtrTyped := splitTail.NewBitCast(tailElemPtr, irtypes.NewPointer(irtypes.I8Ptr))
	splitTail.NewStore(tailStr, tailElemPtrTyped)
	splitTail.NewBr(doneBlk)

	// done: return slice
	doneBlk.NewRet(rawSlice)

	c.funcs["promise_string_split"] = fn
}

// defineStringNextCharFunc emits an LLVM IR function that decodes one UTF-8
// codepoint from a string at position *pos, advances *pos past the consumed
// bytes, and returns the codepoint as i32. Returns -1 when *pos >= len (EOF).
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringNextCharFunc() {
	sParam := ir.NewParam("s", irtypes.I8Ptr)
	posParam := ir.NewParam("pos", irtypes.NewPointer(irtypes.I64))
	fn := c.module.NewFunc("promise_string_next_char", irtypes.I32, sParam, posParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	one32 := constant.NewInt(irtypes.I32, 1)

	// entry: load len (masking off literal flag), data pointer, *pos, allocas for cp/n/loopI
	entry := fn.NewBlock(".entry")

	typedS := entry.NewBitCast(sParam, irtypes.NewPointer(strInstanceType))
	sLen := loadStringLen(entry, typedS, strInstanceType)
	sDataPtr := entry.NewGetElementPtr(strInstanceType, typedS,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	posVal := entry.NewLoad(irtypes.I64, posParam)

	// Allocas in entry block
	cpA := entry.NewAlloca(irtypes.I32)
	nA := entry.NewAlloca(irtypes.I32)
	loopIA := entry.NewAlloca(irtypes.I32)

	atEnd := entry.NewICmp(enum.IPredSGE, posVal, sLen)

	retEof := fn.NewBlock("ret_eof")
	decode := fn.NewBlock("decode")
	set1 := fn.NewBlock("set_1byte")
	chk2 := fn.NewBlock("chk_2byte")
	set2 := fn.NewBlock("set_2byte")
	chk3 := fn.NewBlock("chk_3byte")
	set3 := fn.NewBlock("set_3byte")
	set4 := fn.NewBlock("set_4byte")
	contLoop := fn.NewBlock("cont_loop")
	contHdr := fn.NewBlock("cont_hdr")
	contBound := fn.NewBlock("cont_bound")
	contBody := fn.NewBlock("cont_body")
	contDone := fn.NewBlock("cont_done")

	entry.NewCondBr(atEnd, retEof, decode)

	// ret_eof: return -1
	retEof.NewRet(constant.NewInt(irtypes.I32, -1))

	// decode: load first byte, classify
	bytePtr := decode.NewGetElementPtr(irtypes.I8, sDataPtr, posVal)
	b0 := decode.NewLoad(irtypes.I8, bytePtr)
	b0ext := decode.NewZExt(b0, irtypes.I32)
	isAscii := decode.NewICmp(enum.IPredULT, b0ext, constant.NewInt(irtypes.I32, 0x80))
	decode.NewCondBr(isAscii, set1, chk2)

	// set_1byte: cp = b0, n = 1
	set1.NewStore(b0ext, cpA)
	set1.NewStore(one32, nA)
	set1.NewBr(contLoop)

	// chk_2byte: b0 < 0xE0?
	is2byte := chk2.NewICmp(enum.IPredULT, b0ext, constant.NewInt(irtypes.I32, 0xE0))
	chk2.NewCondBr(is2byte, set2, chk3)

	// set_2byte: cp = b0 & 0x1F, n = 2
	masked2 := set2.NewAnd(b0ext, constant.NewInt(irtypes.I32, 0x1F))
	set2.NewStore(masked2, cpA)
	set2.NewStore(constant.NewInt(irtypes.I32, 2), nA)
	set2.NewBr(contLoop)

	// chk_3byte: b0 < 0xF0?
	is3byte := chk3.NewICmp(enum.IPredULT, b0ext, constant.NewInt(irtypes.I32, 0xF0))
	chk3.NewCondBr(is3byte, set3, set4)

	// set_3byte: cp = b0 & 0x0F, n = 3
	masked3 := set3.NewAnd(b0ext, constant.NewInt(irtypes.I32, 0x0F))
	set3.NewStore(masked3, cpA)
	set3.NewStore(constant.NewInt(irtypes.I32, 3), nA)
	set3.NewBr(contLoop)

	// set_4byte: cp = b0 & 0x07, n = 4
	masked4 := set4.NewAnd(b0ext, constant.NewInt(irtypes.I32, 0x07))
	set4.NewStore(masked4, cpA)
	set4.NewStore(constant.NewInt(irtypes.I32, 4), nA)
	set4.NewBr(contLoop)

	// cont_loop: initialize loop index i = 1
	contLoop.NewStore(one32, loopIA)
	contLoop.NewBr(contHdr)

	// cont_hdr: i < n?
	iVal := contHdr.NewLoad(irtypes.I32, loopIA)
	nVal := contHdr.NewLoad(irtypes.I32, nA)
	cond1 := contHdr.NewICmp(enum.IPredSLT, iVal, nVal)
	contHdr.NewCondBr(cond1, contBound, contDone)

	// cont_bound: *pos + i < sLen?
	iVal2 := contBound.NewLoad(irtypes.I32, loopIA)
	iExt := contBound.NewSExt(iVal2, irtypes.I64)
	absPos := contBound.NewAdd(posVal, iExt)
	cond2 := contBound.NewICmp(enum.IPredSLT, absPos, sLen)
	contBound.NewCondBr(cond2, contBody, contDone)

	// cont_body: cp = (cp << 6) | (data[absPos] & 0x3F); i++
	absPosBody := contBody.NewLoad(irtypes.I32, loopIA)
	absPosExt := contBody.NewSExt(absPosBody, irtypes.I64)
	absPosCalc := contBody.NewAdd(posVal, absPosExt)
	contBytePtr := contBody.NewGetElementPtr(irtypes.I8, sDataPtr, absPosCalc)
	contByte := contBody.NewLoad(irtypes.I8, contBytePtr)
	contByteExt := contBody.NewZExt(contByte, irtypes.I32)
	masked := contBody.NewAnd(contByteExt, constant.NewInt(irtypes.I32, 0x3F))
	cp := contBody.NewLoad(irtypes.I32, cpA)
	shifted := contBody.NewShl(cp, constant.NewInt(irtypes.I32, 6))
	newCp := contBody.NewOr(shifted, masked)
	contBody.NewStore(newCp, cpA)
	iCur := contBody.NewLoad(irtypes.I32, loopIA)
	iNext := contBody.NewAdd(iCur, one32)
	contBody.NewStore(iNext, loopIA)
	contBody.NewBr(contHdr)

	// cont_done: *pos += n, return cp
	nFinal := contDone.NewLoad(irtypes.I32, nA)
	nExt := contDone.NewSExt(nFinal, irtypes.I64)
	newPos := contDone.NewAdd(posVal, nExt)
	contDone.NewStore(newPos, posParam)
	cpFinal := contDone.NewLoad(irtypes.I32, cpA)
	contDone.NewRet(cpFinal)

	c.funcs["promise_string_next_char"] = fn
}

// defineStringHashFunc emits an LLVM IR function that computes FNV-1a hash
// over the raw bytes of a string. Replaces the C runtime promise_hash_string_value.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
// over the raw bytes of a string. Replaces the C runtime promise_hash_string_value.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringHashFunc() {
	ptrParam := ir.NewParam("ptr", irtypes.I8Ptr)
	fn := c.module.NewFunc("__promise_hash_string", irtypes.I64, ptrParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	fnvOffset := constant.NewInt(irtypes.I64, -3750763034362895579) // 0xcbf29ce484222325
	fnvPrime := constant.NewInt(irtypes.I64, 1099511628211)         // 0x00000100000001b3
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)

	// Entry: null check
	entry := fn.NewBlock(".entry")
	isNull := entry.NewICmp(enum.IPredEQ, ptrParam, constant.NewNull(irtypes.I8Ptr))
	nullBlk := fn.NewBlock("null")
	initBlk := fn.NewBlock("init")
	entry.NewCondBr(isNull, nullBlk, initBlk)

	// Null → return 0
	nullBlk.NewRet(zero64)

	// Init: load len (masking off literal flag) and data pointer, set up loop variables
	typedPtr := initBlk.NewBitCast(ptrParam, irtypes.NewPointer(strInstanceType))
	strLen := loadStringLen(initBlk, typedPtr, strInstanceType)
	dataPtr := initBlk.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	// Alloca-based loop variables (consistent with rest of codegen)
	iAlloca := initBlk.NewAlloca(irtypes.I64)
	initBlk.NewStore(zero64, iAlloca)
	hAlloca := initBlk.NewAlloca(irtypes.I64)
	initBlk.NewStore(fnvOffset, hAlloca)

	headerBlk := fn.NewBlock("loop.header")
	bodyBlk := fn.NewBlock("loop.body")
	exitBlk := fn.NewBlock("exit")

	initBlk.NewBr(headerBlk)

	// Loop header: check i < len
	iVal := headerBlk.NewLoad(irtypes.I64, iAlloca)
	cond := headerBlk.NewICmp(enum.IPredSLT, iVal, strLen)
	headerBlk.NewCondBr(cond, bodyBlk, exitBlk)

	// Loop body: hash = (hash ^ byte) * prime; i++
	iCur := bodyBlk.NewLoad(irtypes.I64, iAlloca)
	bytePtr := bodyBlk.NewGetElementPtr(irtypes.I8, dataPtr, iCur)
	byteVal := bodyBlk.NewLoad(irtypes.I8, bytePtr)
	byteExt := bodyBlk.NewZExt(byteVal, irtypes.I64)
	hCur := bodyBlk.NewLoad(irtypes.I64, hAlloca)
	xored := bodyBlk.NewXor(hCur, byteExt)
	mulled := bodyBlk.NewMul(xored, fnvPrime)
	bodyBlk.NewStore(mulled, hAlloca)
	nextI := bodyBlk.NewAdd(iCur, one64)
	bodyBlk.NewStore(nextI, iAlloca)
	bodyBlk.NewBr(headerBlk)

	// Exit: return hash
	result := exitBlk.NewLoad(irtypes.I64, hAlloca)
	exitBlk.NewRet(result)

	c.funcs["__promise_hash_string"] = fn
}

// defineStringEqFunc emits an LLVM IR function that compares two string keys
// by content. Replaces the C runtime promise_eq_string. Used by Vector.contains
// for string elements. Parameters are indirect pointers (pointer to slot
// containing a string pointer), matching the generic comparator ABI.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringEqFunc() {
	aParam := ir.NewParam("a", irtypes.I8Ptr)
	bParam := ir.NewParam("b", irtypes.I8Ptr)
	keySizeParam := ir.NewParam("key_size", irtypes.I64)
	fn := c.module.NewFunc("__promise_eq_string", irtypes.I32, aParam, bParam, keySizeParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	zero32 := constant.NewInt(irtypes.I32, 0)
	one32 := constant.NewInt(irtypes.I32, 1)

	// Entry: dereference indirect pointers to get actual string pointers
	entry := fn.NewBlock(".entry")
	ptrPtrA := entry.NewBitCast(aParam, irtypes.NewPointer(irtypes.I8Ptr))
	pa := entry.NewLoad(irtypes.I8Ptr, ptrPtrA)
	ptrPtrB := entry.NewBitCast(bParam, irtypes.NewPointer(irtypes.I8Ptr))
	pb := entry.NewLoad(irtypes.I8Ptr, ptrPtrB)

	// Fast path: same pointer → equal
	samePtr := entry.NewICmp(enum.IPredEQ, pa, pb)
	samePtrBlk := fn.NewBlock("same_ptr")
	checkNullBlk := fn.NewBlock("check_null")
	entry.NewCondBr(samePtr, samePtrBlk, checkNullBlk)

	samePtrBlk.NewRet(one32)

	// Null check: if either is null → not equal
	aNull := checkNullBlk.NewICmp(enum.IPredEQ, pa, constant.NewNull(irtypes.I8Ptr))
	bNull := checkNullBlk.NewICmp(enum.IPredEQ, pb, constant.NewNull(irtypes.I8Ptr))
	eitherNull := checkNullBlk.NewOr(aNull, bNull)
	nullBlk := fn.NewBlock("null")
	checkLenBlk := fn.NewBlock("check_len")
	checkNullBlk.NewCondBr(eitherNull, nullBlk, checkLenBlk)

	nullBlk.NewRet(zero32)

	// Compare lengths (masking off literal flag)
	typedA := checkLenBlk.NewBitCast(pa, irtypes.NewPointer(strInstanceType))
	lenA := loadStringLen(checkLenBlk, typedA, strInstanceType)

	typedB := checkLenBlk.NewBitCast(pb, irtypes.NewPointer(strInstanceType))
	lenB := loadStringLen(checkLenBlk, typedB, strInstanceType)

	lenEq := checkLenBlk.NewICmp(enum.IPredEQ, lenA, lenB)
	lenNeqBlk := fn.NewBlock("len_neq")
	cmpDataBlk := fn.NewBlock("cmp_data")
	checkLenBlk.NewCondBr(lenEq, cmpDataBlk, lenNeqBlk)

	lenNeqBlk.NewRet(zero32)

	// Get data pointers and compare via memcmp
	dataPtrA := cmpDataBlk.NewGetElementPtr(strInstanceType, typedA,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	dataPtrB := cmpDataBlk.NewGetElementPtr(strInstanceType, typedB,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	equalBlk := fn.NewBlock("equal")
	neqBlk := fn.NewBlock("not_equal")

	cmpResult := cmpDataBlk.NewCall(c.funcs["memcmp"], dataPtrA, dataPtrB, lenA)
	isEqual := cmpDataBlk.NewICmp(enum.IPredEQ, cmpResult, constant.NewInt(irtypes.I32, 0))
	cmpDataBlk.NewCondBr(isEqual, equalBlk, neqBlk)

	// Bytes differ → return 0
	neqBlk.NewRet(zero32)

	// All bytes match → return 1
	equalBlk.NewRet(one32)

	c.funcs["__promise_eq_string"] = fn
}
