package codegen

import (
	"math"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/types"
)

// defineVectorWithCapacityFunc emits an LLVM IR function that allocates a vector
// with len=0 and the given capacity. Replaces the C runtime promise_vector_with_capacity.
// Vector layout: {i64 len, i64 cap, [data...]} with 16-byte header.
func (c *Compiler) defineVectorWithCapacityFunc() {
	capParam := ir.NewParam("capacity", irtypes.I64)
	elemSizeParam := ir.NewParam("elem_size", irtypes.I64)
	fn := c.module.NewFunc("promise_vector_with_capacity", irtypes.I8Ptr,
		capParam, elemSizeParam)

	headerType := vectorHeaderType() // {i64, i64}
	zero64 := constant.NewInt(irtypes.I64, 0)
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))

	// Entry: clamp negative capacity to 0, compute alloc size
	entry := fn.NewBlock(".entry")
	isNeg := entry.NewICmp(enum.IPredSLT, capParam, zero64)
	clampedCap := entry.NewSelect(isNeg, zero64, capParam)
	dataSize := entry.NewMul(clampedCap, elemSizeParam)
	allocSize := entry.NewAdd(headerSizeConst, dataSize)
	raw := entry.NewCall(c.palAlloc, allocSize)
	isNull := entry.NewICmp(enum.IPredEQ, raw, constant.NewNull(irtypes.I8Ptr))

	oom := fn.NewBlock("oom")
	init := fn.NewBlock("init")
	entry.NewCondBr(isNull, oom, init)

	// OOM: panic
	panicGlobal := c.getCStrGlobal("out of memory")
	msgPtr := oom.NewGetElementPtr(panicGlobal.ContentType, panicGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	oom.NewCall(c.funcs["promise_panic"], msgPtr)
	oom.NewRet(constant.NewNull(irtypes.I8Ptr))

	// Init: store len=0, cap
	hdrPtr := init.NewBitCast(raw, irtypes.NewPointer(headerType))
	lenPtr := init.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	init.NewStore(zero64, lenPtr)
	capPtr := init.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	init.NewStore(clampedCap, capPtr)
	init.NewRet(raw)

	c.funcs["promise_vector_with_capacity"] = fn
}

// defineVectorPushFunc emits an LLVM IR function that appends an element to a vector.
// Returns the (possibly reallocated) vector pointer.
// Replaces the C runtime promise_vector_push.
// Vector layout: {i64 len, i64 cap, [data...]} with 16-byte header.
func (c *Compiler) defineVectorPushFunc() {
	sliceParam := ir.NewParam("slice", irtypes.I8Ptr)
	elemParam := ir.NewParam("elem", irtypes.I8Ptr)
	elemSizeParam := ir.NewParam("elem_size", irtypes.I64)
	fn := c.module.NewFunc("promise_vector_push", irtypes.I8Ptr,
		sliceParam, elemParam, elemSizeParam)

	headerType := vectorHeaderType() // {i64, i64}
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	four64 := constant.NewInt(irtypes.I64, 4)
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))

	// Entry: load len (masked) and cap, check if growth needed
	entry := fn.NewBlock(".entry")
	hdrPtr := entry.NewBitCast(sliceParam, irtypes.NewPointer(headerType))
	vecLen := loadVectorLen(entry, hdrPtr)
	capPtr := entry.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	vecCap := entry.NewLoad(irtypes.I64, capPtr)
	needGrow := entry.NewICmp(enum.IPredSGE, vecLen, vecCap)

	grow := fn.NewBlock("grow")
	copyBlk := fn.NewBlock("copy")
	entry.NewCondBr(needGrow, grow, copyBlk)

	// Grow: realloc with cap*2 (or 4 if cap==0)
	isZeroCap := grow.NewICmp(enum.IPredEQ, vecCap, zero64)
	doubledCap := grow.NewMul(vecCap, constant.NewInt(irtypes.I64, 2))
	newCap := grow.NewSelect(isZeroCap, four64, doubledCap)
	newDataSize := grow.NewMul(newCap, elemSizeParam)
	newAllocSize := grow.NewAdd(headerSizeConst, newDataSize)
	newPtr := grow.NewCall(c.palRealloc, sliceParam, newAllocSize)
	isNull := grow.NewICmp(enum.IPredEQ, newPtr, constant.NewNull(irtypes.I8Ptr))

	oom := fn.NewBlock("oom")
	updateCap := fn.NewBlock("update_cap")
	grow.NewCondBr(isNull, oom, updateCap)

	// OOM: panic
	panicGlobal := c.getCStrGlobal("out of memory")
	msgPtr := oom.NewGetElementPtr(panicGlobal.ContentType, panicGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	oom.NewCall(c.funcs["promise_panic"], msgPtr)
	oom.NewRet(constant.NewNull(irtypes.I8Ptr))

	// Update cap: store new capacity in reallocated header
	newHdrPtr := updateCap.NewBitCast(newPtr, irtypes.NewPointer(headerType))
	newCapPtr := updateCap.NewGetElementPtr(headerType, newHdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	updateCap.NewStore(newCap, newCapPtr)
	updateCap.NewBr(copyBlk)

	// Copy: phi to merge original/reallocated pointers
	vecPtr := copyBlk.NewPhi(
		&ir.Incoming{X: sliceParam, Pred: entry},
		&ir.Incoming{X: newPtr, Pred: updateCap})
	curHdrPtr := copyBlk.NewPhi(
		&ir.Incoming{X: hdrPtr, Pred: entry},
		&ir.Incoming{X: newHdrPtr, Pred: updateCap})

	// Compute destination: data area + len * elem_size
	offset := copyBlk.NewMul(vecLen, elemSizeParam)
	dataOffset := copyBlk.NewAdd(headerSizeConst, offset)
	dest := copyBlk.NewGetElementPtr(irtypes.I8, vecPtr, dataOffset)
	copyBlk.NewCall(c.funcs["llvm.memcpy"], dest, elemParam, elemSizeParam, constant.False)

	// Increment length
	newLen := copyBlk.NewAdd(vecLen, one64)
	curLenPtr := copyBlk.NewGetElementPtr(headerType, curHdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	copyBlk.NewStore(newLen, curLenPtr)
	copyBlk.NewRet(vecPtr)

	c.funcs["promise_vector_push"] = fn
}

// defineVectorPopFunc emits an LLVM IR function that removes and returns the last
// element from a vector. Returns 1 if successful, 0 if empty.
// Replaces the C runtime promise_vector_pop.
// Vector layout: {i64 len, i64 cap, [data...]} with 16-byte header.
func (c *Compiler) defineVectorPopFunc() {
	sliceParam := ir.NewParam("slice", irtypes.I8Ptr)
	outElemParam := ir.NewParam("out_elem", irtypes.I8Ptr)
	elemSizeParam := ir.NewParam("elem_size", irtypes.I64)
	fn := c.module.NewFunc("promise_vector_pop", irtypes.I32,
		sliceParam, outElemParam, elemSizeParam)

	headerType := vectorHeaderType() // {i64, i64}
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))

	// Entry: load len (masked), check if empty
	entry := fn.NewBlock(".entry")
	hdrPtr := entry.NewBitCast(sliceParam, irtypes.NewPointer(headerType))
	vecLen := loadVectorLen(entry, hdrPtr)
	lenPtr := entry.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	isEmpty := entry.NewICmp(enum.IPredEQ, vecLen, zero64)

	emptyBlk := fn.NewBlock("empty")
	doPopBlk := fn.NewBlock("do_pop")
	entry.NewCondBr(isEmpty, emptyBlk, doPopBlk)

	// Empty: return 0
	emptyBlk.NewRet(constant.NewInt(irtypes.I32, 0))

	// Do pop: decrement len, copy last element out
	newLen := doPopBlk.NewSub(vecLen, one64)
	doPopBlk.NewStore(newLen, lenPtr)
	offset := doPopBlk.NewMul(newLen, elemSizeParam)
	dataOffset := doPopBlk.NewAdd(headerSizeConst, offset)
	src := doPopBlk.NewGetElementPtr(irtypes.I8, sliceParam, dataOffset)
	doPopBlk.NewCall(c.funcs["llvm.memcpy"], outElemParam, src, elemSizeParam, constant.False)
	doPopBlk.NewRet(constant.NewInt(irtypes.I32, 1))

	c.funcs["promise_vector_pop"] = fn
}

// defineVectorContainsFunc emits an LLVM IR function that searches a vector for
// an element. Replaces the C runtime promise_vector_contains.
// For string elements, uses the eq_fn comparator (__promise_eq_string).
// For other types, does byte-by-byte comparison (equivalent to memcmp).
// Vector layout: {i64 len, i64 cap, [data...]} with 16-byte header.
func (c *Compiler) defineVectorContainsFunc() {
	sliceParam := ir.NewParam("slice", irtypes.I8Ptr)
	elemParam := ir.NewParam("elem", irtypes.I8Ptr)
	elemSizeParam := ir.NewParam("elem_size", irtypes.I64)
	eqFnParam := ir.NewParam("eq_fn", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_vector_contains", irtypes.I8,
		sliceParam, elemParam, elemSizeParam, eqFnParam)

	headerType := vectorHeaderType() // {i64, i64}

	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	foundVal := constant.NewInt(irtypes.I8, 1)
	notFoundVal := constant.NewInt(irtypes.I8, 0)
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))

	// Entry: load len (masked) from header, init loop counter
	entry := fn.NewBlock(".entry")
	hdrPtr := entry.NewBitCast(sliceParam, irtypes.NewPointer(headerType))
	vecLen := loadVectorLen(entry, hdrPtr)
	iAlloca := entry.NewAlloca(irtypes.I64)
	entry.NewStore(zero64, iAlloca)

	loopHeader := fn.NewBlock("loop.header")
	loopBody := fn.NewBlock("loop.body")
	callEq := fn.NewBlock("call_eq")
	cmpBytes := fn.NewBlock("cmp_bytes")
	loopNext := fn.NewBlock("loop.next")
	found := fn.NewBlock("found")
	notFound := fn.NewBlock("not_found")

	entry.NewBr(loopHeader)

	// loop.header: check i < len
	iVal := loopHeader.NewLoad(irtypes.I64, iAlloca)
	cond := loopHeader.NewICmp(enum.IPredSLT, iVal, vecLen)
	loopHeader.NewCondBr(cond, loopBody, notFound)

	// loop.body: compute element address, check eq_fn
	iCur := loopBody.NewLoad(irtypes.I64, iAlloca)
	offset := loopBody.NewMul(iCur, elemSizeParam)
	dataOffset := loopBody.NewAdd(offset, headerSizeConst)
	curPtr := loopBody.NewGetElementPtr(irtypes.I8, sliceParam, dataOffset)
	isNull := loopBody.NewICmp(enum.IPredEQ, eqFnParam, constant.NewNull(irtypes.I8Ptr))
	loopBody.NewCondBr(isNull, cmpBytes, callEq)

	// call_eq: cast eq_fn to function pointer and call
	eqFnType := irtypes.NewFunc(irtypes.I32, irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I64)
	eqFnCast := callEq.NewBitCast(eqFnParam, irtypes.NewPointer(eqFnType))
	eqResult := callEq.NewCall(eqFnCast, curPtr, elemParam, elemSizeParam)
	eqNonZero := callEq.NewICmp(enum.IPredNE, eqResult, constant.NewInt(irtypes.I32, 0))
	callEq.NewCondBr(eqNonZero, found, loopNext)

	// cmp_bytes: compare via memcmp
	cmpResult := cmpBytes.NewCall(c.funcs["memcmp"], curPtr, elemParam, elemSizeParam)
	isEqual := cmpBytes.NewICmp(enum.IPredEQ, cmpResult, constant.NewInt(irtypes.I32, 0))
	cmpBytes.NewCondBr(isEqual, found, loopNext)

	// loop.next: increment i, loop back
	iNext := loopNext.NewLoad(irtypes.I64, iAlloca)
	iInc := loopNext.NewAdd(iNext, one64)
	loopNext.NewStore(iInc, iAlloca)
	loopNext.NewBr(loopHeader)

	// found / not_found
	found.NewRet(foundVal)
	notFound.NewRet(notFoundVal)

	c.funcs["promise_vector_contains"] = fn
}

// defineVectorRemoveFunc emits an LLVM IR function that removes an element
// from a vector at a given index by shifting subsequent elements left.
// Replaces the C runtime promise_vector_remove.
// Uses memmove for the shift and decrements the length field.
func (c *Compiler) defineVectorRemoveFunc() {
	sliceParam := ir.NewParam("slice", irtypes.I8Ptr)
	indexParam := ir.NewParam("index", irtypes.I64)
	elemSizeParam := ir.NewParam("elem_size", irtypes.I64)
	fn := c.module.NewFunc("promise_vector_remove", irtypes.Void,
		sliceParam, indexParam, elemSizeParam)

	headerType := vectorHeaderType() // {i64, i64}

	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))

	// Entry: load len (masked), bounds check
	entry := fn.NewBlock(".entry")
	hdrPtr := entry.NewBitCast(sliceParam, irtypes.NewPointer(headerType))
	vecLen := loadVectorLen(entry, hdrPtr)
	lenPtr := entry.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

	isNeg := entry.NewICmp(enum.IPredSLT, indexParam, zero64)
	isOver := entry.NewICmp(enum.IPredSGE, indexParam, vecLen)
	oob := entry.NewOr(isNeg, isOver)

	panicBlk := fn.NewBlock("panic")
	checkShift := fn.NewBlock("check_shift")
	entry.NewCondBr(oob, panicBlk, checkShift)

	// panic: call promise_panic with out-of-bounds message
	panicGlobal := c.getCStrGlobal("vector remove: index out of bounds")
	msgPtr := panicBlk.NewGetElementPtr(panicGlobal.ContentType, panicGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	panicBlk.NewCall(c.funcs["promise_panic"], msgPtr)
	panicBlk.NewRet(nil) // void return

	// check_shift: compute data base, check if shift needed
	dataBase := checkShift.NewGetElementPtr(irtypes.I8, sliceParam, headerSizeConst)
	lenMinus1 := checkShift.NewSub(vecLen, one64)
	needsShift := checkShift.NewICmp(enum.IPredSLT, indexParam, lenMinus1)

	doShift := fn.NewBlock("do_shift")
	decLen := fn.NewBlock("dec_len")
	checkShift.NewCondBr(needsShift, doShift, decLen)

	// do_shift: memmove elements left
	dstOffset := doShift.NewMul(indexParam, elemSizeParam)
	dst := doShift.NewGetElementPtr(irtypes.I8, dataBase, dstOffset)
	idxPlus1 := doShift.NewAdd(indexParam, one64)
	srcOffset := doShift.NewMul(idxPlus1, elemSizeParam)
	src := doShift.NewGetElementPtr(irtypes.I8, dataBase, srcOffset)
	remaining := doShift.NewSub(vecLen, idxPlus1)
	moveSize := doShift.NewMul(remaining, elemSizeParam)
	doShift.NewCall(c.funcs["llvm.memmove"], dst, src, moveSize, constant.False)
	doShift.NewBr(decLen)

	// dec_len: decrement length
	newLen := decLen.NewSub(vecLen, one64)
	decLen.NewStore(newLen, lenPtr)
	decLen.NewRet(nil)

	c.funcs["promise_vector_remove"] = fn
}

// defineVectorDropFunc emits an LLVM IR function that frees a vector's heap buffer.
// Vector layout: {i64 len, i64 cap, [data...]} — a single allocation via pal_alloc.
// Drop null-checks, then checks bit 63 of len (static flag) — static .rodata vectors
// must not be freed.
func (c *Compiler) defineVectorDropFunc() {
	thisParam := ir.NewParam("this", irtypes.I8Ptr)
	fn := c.module.NewFunc("Vector.drop", irtypes.Void, thisParam)

	entry := fn.NewBlock(".entry")

	// Null-check: zero-initialized values (from error handler fallthrough) may be null
	isNull := entry.NewICmp(enum.IPredEQ, thisParam, constant.NewNull(irtypes.I8Ptr))
	checkStatic := fn.NewBlock("check_static")
	doneBlk := fn.NewBlock("done")
	entry.NewCondBr(isNull, doneBlk, checkStatic)

	// Check bit 63 of len: if set, this is a static .rodata vector — don't free
	headerType := vectorHeaderType()
	hdrPtr := checkStatic.NewBitCast(thisParam, irtypes.NewPointer(headerType))
	rawLen := loadVectorLenRaw(checkStatic, hdrPtr)
	bit63 := checkStatic.NewAnd(rawLen, constant.NewInt(irtypes.I64, math.MinInt64))
	isStatic := checkStatic.NewICmp(enum.IPredNE, bit63, constant.NewInt(irtypes.I64, 0))
	freeBlk := fn.NewBlock("free")
	checkStatic.NewCondBr(isStatic, doneBlk, freeBlk)

	freeBlk.NewCall(c.palFree, thisParam)
	freeBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)

	c.funcs["Vector.drop"] = fn
}

// getOrCreateChannelDrop lazily creates a per-element-type drop function for
// Channel[T]: @"Channel[<T>].drop"(i8* %this) → void. T0663: the ring buffer
// can still hold un-received, owned items when the channel is dropped
// (refcount→0, no live receivers). The per-element-type body walks the buffered
// items and drops each T before freeing the ring buffer as raw bytes — without
// this, any Channel[T] with a heap T (string/Vector/user-heap/Arc/Weak/Mutex/
// Task/nested-Channel) leaks one allocation per un-received item.
//
// Mirrors getOrCreateArcDrop / getOrCreateMutexDrop / getOrCreateTaskDrop. The
// stub is registered in c.funcs before its body is defined so a self-referential
// type (Channel[Node] where Node has a Channel[Node] field) finds the
// declared-but-empty stub and emits a recursive call instead of recursing
// forever in codegen.
func (c *Compiler) getOrCreateChannelDrop(elemType types.Type) *ir.Func {
	elemName := "void"
	if elemType != nil {
		elemName = typeArgStr(elemType)
	}
	funcName := "Channel[" + elemName + "].drop"

	if fn, ok := c.funcs[funcName]; ok {
		if len(fn.Blocks) > 0 {
			return fn // already defined (or in-progress — recursive type)
		}
		c.defineChannelDropBody(fn, elemType)
		return fn
	}

	thisParam := ir.NewParam("this", irtypes.I8Ptr)
	fn := c.module.NewFunc(funcName, irtypes.Void, thisParam)
	// Register before defining the body (recursion-safety, see doc above).
	c.funcs[funcName] = fn
	c.defineChannelDropBody(fn, elemType)
	return fn
}

// defineChannelDropBody generates the body of a Channel[T].drop function.
// Atomically decrements the channel's reference count. When refcount reaches 0:
// drops any un-received buffered T items (T0663), then frees the ring buffer,
// mutex, 2 cond vars, and the channel struct itself.
// B0163: Channel scope-exit drop with reference counting.
func (c *Compiler) defineChannelDropBody(fn *ir.Func, elemType types.Type) {
	thisParam := fn.Params[0]
	chanType := channelStructType()

	entry := fn.NewBlock(".entry")

	// Null-check: zero-initialized values (from error handler fallthrough) may be null
	isNull := entry.NewICmp(enum.IPredEQ, thisParam, constant.NewNull(irtypes.I8Ptr))
	decrcBlk := fn.NewBlock("decrc")
	doneBlk := fn.NewBlock("done")
	entry.NewCondBr(isNull, doneBlk, decrcBlk)

	// Atomically decrement refcount. Only free when old value was 1 (drops to 0).
	chPtr := decrcBlk.NewBitCast(thisParam, irtypes.NewPointer(chanType))
	rcField := decrcBlk.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRefcount)))
	oldRC := c.emitAtomicAdd(decrcBlk, rcField, constant.NewInt(irtypes.I64, -1), irtypes.I64)
	wasOne := decrcBlk.NewICmp(enum.IPredEQ, oldRC, constant.NewInt(irtypes.I64, 1))
	freeBlk := fn.NewBlock("free")
	decrcBlk.NewCondBr(wasOne, freeBlk, doneBlk)

	// T0663: Drop un-received buffered elements before freeing the ring buffer
	// as raw bytes. Refcount reached 0 → no live receivers → remaining items in
	// [head, head+count) mod capacity are unowned and must be dropped. Skipped
	// entirely for value elements (Channel[int] etc.) — keeps today's codegen
	// for the overwhelmingly common case and avoids a needless loop.
	if elemType != nil && c.variantFieldNeedsDrop(elemType) {
		savedFn, savedEntry, savedBlock := c.fn, c.entryBlock, c.block
		c.fn, c.entryBlock, c.block = fn, entry, freeBlk
		c.emitChannelElementDropLoop(chPtr, chanType, elemType)
		freeBlk = c.block
		c.fn, c.entryBlock, c.block = savedFn, savedEntry, savedBlock
	}

	// Free ring buffer (field 0: i8*)
	bufField := freeBlk.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
	bufPtr := freeBlk.NewLoad(irtypes.I8Ptr, bufField)
	freeBlk.NewCall(c.palFree, bufPtr)

	// Destroy mutex (field 8: i8*)
	mtxField := freeBlk.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	mtxPtr := freeBlk.NewLoad(irtypes.I8Ptr, mtxField)
	freeBlk.NewCall(c.palMutexDestroy, mtxPtr)

	// Destroy not_empty cond var (field 9: i8*)
	neField := freeBlk.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	nePtr := freeBlk.NewLoad(irtypes.I8Ptr, neField)
	freeBlk.NewCall(c.palCondDestroy, nePtr)

	// Destroy not_full cond var (field 10: i8*)
	nfField := freeBlk.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	nfPtr := freeBlk.NewLoad(irtypes.I8Ptr, nfField)
	freeBlk.NewCall(c.palCondDestroy, nfPtr)

	// Free the channel struct itself
	freeBlk.NewCall(c.palFree, thisParam)
	freeBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)
}

// emitChannelElementDropLoop emits a loop over the channel's un-received
// buffered items and drops each one via emitVariantFieldDrop. T0663. The ring
// buffer holds `count` items starting at logical index `head`; item k lives at
// physical slot (head + k) mod capacity, byte offset slot*elem_size — the same
// indexing as the recv read path (stmt.go execRecv). Uses c.block/c.fn/
// c.entryBlock — the caller (defineChannelDropBody) swaps them to the drop
// function's context so emitVariantFieldDrop's sub-blocks land here.
func (c *Compiler) emitChannelElementDropLoop(chPtr value.Value, chanType *irtypes.StructType, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)

	field := func(idx int) value.Value {
		return c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
	}
	// Load buffer geometry once — drop runs on the last reference so these are
	// stable for the duration of the walk.
	buf := c.block.NewLoad(irtypes.I8Ptr, field(chanFieldBuffer))
	elemSize := c.block.NewLoad(irtypes.I64, field(chanFieldElemSize))
	capacity := c.block.NewLoad(irtypes.I64, field(chanFieldCapacity))
	count := c.block.NewLoad(irtypes.I64, field(chanFieldCount))
	head := c.block.NewLoad(irtypes.I64, field(chanFieldHead))

	loopHead := c.newBlock("chdrop.head")
	loopBody := c.newBlock("chdrop.body")
	loopDone := c.newBlock("chdrop.done")

	idxAlloca := c.createEntryAlloca(irtypes.I64)
	idxAlloca.SetName(c.uniqueLocalName("chdrop.idx"))
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)
	c.block.NewBr(loopHead)

	// Loop head: while idx < count
	c.block = loopHead
	idx := c.block.NewLoad(irtypes.I64, idxAlloca)
	cond := c.block.NewICmp(enum.IPredULT, idx, count)
	c.block.NewCondBr(cond, loopBody, loopDone)

	// Loop body: slot = (head + idx) % capacity; drop buffer[slot]
	c.block = loopBody
	idx2 := c.block.NewLoad(irtypes.I64, idxAlloca)
	slot := c.block.NewURem(c.block.NewAdd(head, idx2), capacity)
	byteOff := c.block.NewMul(slot, elemSize)
	elemPtr := c.block.NewGetElementPtr(irtypes.I8, buf, byteOff)
	typedPtr := c.block.NewBitCast(elemPtr, irtypes.NewPointer(elemLLVM))
	elemVal := c.block.NewLoad(elemLLVM, typedPtr)
	c.emitVariantFieldDrop(elemVal, elemType)

	nextIdx := c.block.NewAdd(idx2, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(nextIdx, idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopDone
}

// defineVectorCOWFunc emits promise_vector_cow(i8* slice, i64 elem_size) -> i8*.
// If the vector is static (.rodata, bit 63 of len set), allocates a heap copy
// with the static flag cleared. Otherwise returns the same pointer.
func (c *Compiler) defineVectorCOWFunc() {
	sliceParam := ir.NewParam("slice", irtypes.I8Ptr)
	elemSizeParam := ir.NewParam("elem_size", irtypes.I64)
	fn := c.module.NewFunc("promise_vector_cow", irtypes.I8Ptr,
		sliceParam, elemSizeParam)

	headerType := vectorHeaderType()
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))

	entry := fn.NewBlock(".entry")
	hdrPtr := entry.NewBitCast(sliceParam, irtypes.NewPointer(headerType))
	rawLen := loadVectorLenRaw(entry, hdrPtr)
	bit63 := entry.NewAnd(rawLen, constant.NewInt(irtypes.I64, math.MinInt64))
	isStatic := entry.NewICmp(enum.IPredNE, bit63, constant.NewInt(irtypes.I64, 0))

	copyBlk := fn.NewBlock("cow_copy")
	doneBlk := fn.NewBlock("done")
	entry.NewCondBr(isStatic, copyBlk, doneBlk)

	// COW copy: allocate heap buffer, memcpy data, set len (no bit 63) and cap
	realLen := copyBlk.NewAnd(rawLen, vectorLenMask)
	dataSize := copyBlk.NewMul(realLen, elemSizeParam)
	allocSize := copyBlk.NewAdd(headerSizeConst, dataSize)
	newPtr := copyBlk.NewCall(c.palAlloc, allocSize)
	isNull := copyBlk.NewICmp(enum.IPredEQ, newPtr, constant.NewNull(irtypes.I8Ptr))

	oomBlk := fn.NewBlock("oom")
	initBlk := fn.NewBlock("init")
	copyBlk.NewCondBr(isNull, oomBlk, initBlk)

	panicGlobal := c.getCStrGlobal("out of memory")
	msgPtr := oomBlk.NewGetElementPtr(panicGlobal.ContentType, panicGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	oomBlk.NewCall(c.funcs["promise_panic"], msgPtr)
	oomBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	// Store len (without static flag) and cap, then memcpy data
	newHdrPtr := initBlk.NewBitCast(newPtr, irtypes.NewPointer(headerType))
	newLenPtr := initBlk.NewGetElementPtr(headerType, newHdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	initBlk.NewStore(realLen, newLenPtr)
	newCapPtr := initBlk.NewGetElementPtr(headerType, newHdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	initBlk.NewStore(realLen, newCapPtr)

	// memcpy element data
	srcData := initBlk.NewGetElementPtr(irtypes.I8, sliceParam, headerSizeConst)
	dstData := initBlk.NewGetElementPtr(irtypes.I8, newPtr, headerSizeConst)
	initBlk.NewCall(c.funcs["llvm.memcpy"], dstData, srcData, dataSize, constant.False)
	initBlk.NewBr(doneBlk)

	// Merge: phi between original (heap) and new (cow'd) pointer
	result := doneBlk.NewPhi(
		&ir.Incoming{X: sliceParam, Pred: entry},
		&ir.Incoming{X: newPtr, Pred: initBlk})
	doneBlk.NewRet(result)

	c.funcs["promise_vector_cow"] = fn
}
