package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/types"
)

// strInstanceType returns the canonical string instance struct type.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func strInstanceType() *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len (sign bit = literal flag)
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)
}

// loadStringLenRaw loads the raw i64 length field from a typed string instance
// pointer, with the literal flag bit intact.
func loadStringLenRaw(b *ir.Block, typedPtr value.Value, instType irtypes.Type) value.Value {
	lenPtr := b.NewGetElementPtr(instType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	return b.NewLoad(irtypes.I64, lenPtr)
}

// loadStringLen loads the string length with the literal flag (sign bit) masked off.
func loadStringLen(b *ir.Block, typedPtr value.Value, instType irtypes.Type) value.Value {
	raw := loadStringLenRaw(b, typedPtr, instType)
	return b.NewAnd(raw, stringLenMask)
}

// dupString duplicates a string instance (i8* → i8*) by loading its length and data,
// then calling promise_string_new to create a fresh heap copy. Used to prevent
// double-free when a string field is read from a type with synthesized drop (T0095).
// Null-safe: returns null for null input (zero-initialized fields from error paths).
// The caller owns the returned copy independently from the original.
func (c *Compiler) dupString(ptr value.Value) value.Value {
	// Null check: string fields may be null (error handler fallthrough, optional fields).
	entryBlock := c.block
	nullCheck := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
	dupBlock := c.newBlock("strdup.copy")
	mergeBlock := c.newBlock("strdup.merge")
	entryBlock.NewCondBr(nullCheck, mergeBlock, dupBlock)

	c.block = dupBlock
	instType := strInstanceType()
	typedPtr := c.block.NewBitCast(ptr, irtypes.NewPointer(instType))
	strLen := loadStringLen(c.block, typedPtr, instType)
	dataPtr := c.block.NewGetElementPtr(instType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	newPtr := c.block.NewCall(c.funcs["promise_string_new"], dataPtr, strLen)
	dupBlock.NewBr(mergeBlock)

	c.block = mergeBlock
	return c.block.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entryBlock),
		ir.NewIncoming(newPtr, dupBlock),
	)
}

// dupVector duplicates a vector instance (i8* → i8*) by allocating a fresh heap copy.
// Handles null (returns null) and static vectors (clears bit 63 in copy).
// B0219: Used to prevent use-after-free when a vector field is read from a type with
// drop and later the field is reassigned (dropping the old value).
func (c *Compiler) dupVector(ptr value.Value, elemSize int64) value.Value {
	entryBlock := c.block
	nullCheck := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
	dupBlock := c.newBlock("vecdup.copy")
	mergeBlock := c.newBlock("vecdup.merge")
	entryBlock.NewCondBr(nullCheck, mergeBlock, dupBlock)

	c.block = dupBlock
	headerType := vectorHeaderType()
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))
	elemSizeConst := constant.NewInt(irtypes.I64, elemSize)

	hdrPtr := c.block.NewBitCast(ptr, irtypes.NewPointer(headerType))
	rawLen := loadVectorLenRaw(c.block, hdrPtr)
	realLen := c.block.NewAnd(rawLen, vectorLenMask)

	// Load capacity
	capPtr := c.block.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	cap := c.block.NewLoad(irtypes.I64, capPtr)

	// Allocate: header + cap * elem_size
	dataSize := c.block.NewMul(cap, elemSizeConst)
	allocSize := c.block.NewAdd(headerSizeConst, dataSize)
	newPtr := c.block.NewCall(c.palAlloc, allocSize)

	// OOM check
	isOOM := c.block.NewICmp(enum.IPredEQ, newPtr, constant.NewNull(irtypes.I8Ptr))
	oomBlock := c.newBlock("vecdup.oom")
	initBlock := c.newBlock("vecdup.init")
	c.block.NewCondBr(isOOM, oomBlock, initBlock)

	c.block = oomBlock
	panicGlobal := c.getCStrGlobal("out of memory")
	msgPtr := c.block.NewGetElementPtr(panicGlobal.ContentType, panicGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewCall(c.funcs["promise_panic"], msgPtr)
	// OOM: branch to merge with null (panic flag is set, caller will detect)
	oomBlock.NewBr(mergeBlock)

	// Init: store len (without static flag), cap, memcpy data
	c.block = initBlock
	newHdrPtr := c.block.NewBitCast(newPtr, irtypes.NewPointer(headerType))
	newLenPtr := c.block.NewGetElementPtr(headerType, newHdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(realLen, newLenPtr)
	newCapPtr := c.block.NewGetElementPtr(headerType, newHdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	c.block.NewStore(cap, newCapPtr)

	// memcpy element data (only len elements, not full cap)
	copySize := c.block.NewMul(realLen, elemSizeConst)
	srcData := c.block.NewGetElementPtr(irtypes.I8, ptr, headerSizeConst)
	dstData := c.block.NewGetElementPtr(irtypes.I8, newPtr, headerSizeConst)
	c.block.NewCall(c.funcs["llvm.memcpy"], dstData, srcData, copySize, constant.False)
	initBlock.NewBr(mergeBlock)

	c.block = mergeBlock
	return c.block.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entryBlock),
		ir.NewIncoming(newPtr, initBlock),
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), oomBlock),
	)
}

// dupChannel duplicates a channel reference (i8* → i8*) by atomically incrementing
// the reference count. B0219: Used to prevent use-after-free when a channel field
// is read from a type with drop and later the field is reassigned.
func (c *Compiler) dupChannel(ptr value.Value) value.Value {
	entryBlock := c.block
	nullCheck := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
	incBlock := c.newBlock("chdup.inc")
	mergeBlock := c.newBlock("chdup.merge")
	entryBlock.NewCondBr(nullCheck, mergeBlock, incBlock)

	c.block = incBlock
	chanType := channelStructType()
	chPtr := c.block.NewBitCast(ptr, irtypes.NewPointer(chanType))
	rcField := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRefcount)))
	c.emitAtomicAdd(c.block, rcField, constant.NewInt(irtypes.I64, 1), irtypes.I64)
	incBlock.NewBr(mergeBlock)

	c.block = mergeBlock
	return c.block.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entryBlock),
		ir.NewIncoming(ptr, incBlock),
	)
}

// dupOptionalVectorElem duplicates the inner value of an Optional loaded from a
// vector element. Branches on has-value: if present, extracts the inner value,
// deep-dups it based on type, and rebuilds the Optional; if absent, passes
// through. No temp tracking — the variable's Optional drop binding owns cleanup.
// T0620: Prevents double-free between variable's Optional drop and vector's
// element drop loop (enabled by Gap B fix).
// isSignatureElem reports whether t is a closure (function value) type. Used to
// route Optional[closure] inners to the null-the-fat-pointer path (T1227) instead
// of the shallow default that would alias the captured env.
func isSignatureElem(t types.Type) bool {
	_, ok := t.(*types.Signature)
	return ok
}

func (c *Compiler) dupOptionalVectorElem(optVal value.Value, opt *types.Optional, innerElem types.Type) value.Value {
	optLLVM := optVal.Type()

	hasVal := c.block.NewExtractValue(optVal, 0)
	dupBlock := c.newBlock("optdup.dup")
	skipBlock := c.newBlock("optdup.skip")
	mergeBlock := c.newBlock("optdup.merge")
	c.block.NewCondBr(hasVal, dupBlock, skipBlock)

	// Absent path: use original value as-is
	c.block = skipBlock
	skipBlock.NewBr(mergeBlock)

	// Present path: extract inner, dup, rebuild Optional
	c.block = dupBlock
	innerVal := c.block.NewExtractValue(optVal, 1)

	var dupedInner value.Value
	named := extractNamed(innerElem)
	switch {
	case isSignatureElem(innerElem):
		// T1227: a closure inner (Optional[() -> int]) cannot be deep-cloned — the
		// captured frame is opaque. Mirror maybeDupPushElement's T1045 case: null
		// the {fn,env} fat pointer so the source keeps sole ownership of the env and
		// the "clone" holds an empty closure. Without this the default fell through
		// to a shallow `dupedInner = innerVal` that aliases the env → double-free.
		dupedInner = constant.NewZeroInitializer(innerVal.Type())
	case named == types.TypString:
		dupedInner = c.dupString(innerVal)
	case types.IsVector(innerElem):
		vecElem, _ := types.AsVector(innerElem)
		innerLLVM := c.resolveType(vecElem)
		innerSize := int64(c.typeSize(innerLLVM))
		dupedInner = c.dupVector(innerVal, innerSize)
		c.emitVectorElementCloneLoop(dupedInner, vecElem)
	case named == types.TypVector:
		dupedInner = c.dupVector(innerVal, 0)
	case types.IsChannel(innerElem) || named == types.TypChannel:
		dupedInner = c.dupChannel(innerVal)
	case types.IsArc(innerElem):
		arcElem, _ := types.AsArc(innerElem)
		dupedInner = c.dupArc(innerVal, arcElem)
	case types.IsWeak(innerElem):
		weakElem, _ := types.AsWeak(innerElem)
		dupedInner = c.dupWeak(innerVal, weakElem)
	case named != nil && named.IsStructural() && !named.IsValueType():
		// T1291: structural-interface inner — the {vtable, instance} view boxes a
		// heap instance; deep-clone it via RTTI (__promise_structural_clone) so the
		// cloned optional owns an independent box. Without this the shallow alias is
		// double-freed once the structural-aware element drop frees each box.
		dupedInner = c.cloneStructuralView(innerVal)
	case isDroppableHeapUserType(innerElem) || isHeapUserNoDropPalFree(innerElem):
		if namedInner := extractNamed(innerElem); namedInner != nil {
			dupedInner = c.cloneHeapElement(innerVal, innerElem, namedInner)
		}
	default:
		if optInner, ok := innerElem.(*types.Optional); ok {
			// Nested Optional (e.g. T?? whose element is T?): recurse so the
			// clone owns an independent deep copy at every level. Without this
			// the inner Optional — and any heap value it carries — is shared
			// with the original, causing a double free when both are dropped.
			deeper := optInner.Elem()
			if isTypeDroppable(deeper) {
				dupedInner = c.dupOptionalVectorElem(innerVal, optInner, deeper)
			}
		} else if tup, ok := innerElem.(*types.Tuple); ok {
			dupedInner = c.dupTupleValue(innerVal, tup)
		} else if en := extractEnum(innerElem); en != nil {
			// T1183: Optional[droppable-enum] inner. optionalPushElemNeedsDup
			// recognizes this (via pushElemNeedsDup's enum branch), so without a
			// clone here the Some payload would fall through to the shallow
			// pass-through below and alias the source enum's variant fields — a
			// double-free/UAF when both drop. Mirror maybeDupPushElement's enum
			// clone (clone-fn if available, else in-place variant-field dup).
			if _, ok := c.funcs[c.enumCloneFuncName(en, innerElem)]; ok {
				cloned, _ := c.cloneEnumValue(innerVal, innerElem)
				dupedInner = cloned
			} else if c.vecElemNeedsEnumDrop(innerElem) {
				alloca := c.createEntryAlloca(innerVal.Type())
				c.block.NewStore(innerVal, alloca)
				c.dupEnumElementInPlace(alloca, innerElem)
				dupedInner = c.block.NewLoad(innerVal.Type(), alloca)
			}
		}
	}

	if dupedInner == nil {
		dupedInner = innerVal
	}

	// Rebuild Optional: {i1 true, T dupedInner}
	result := c.block.NewInsertValue(constant.NewUndef(optLLVM), constant.NewInt(irtypes.I1, 1), 0)
	result = c.block.NewInsertValue(result, dupedInner, 1)
	exitDupBlock := c.block
	exitDupBlock.NewBr(mergeBlock)

	c.block = mergeBlock
	return c.block.NewPhi(
		ir.NewIncoming(optVal, skipBlock),
		ir.NewIncoming(result, exitDupBlock),
	)
}

// getOrCreateArcDrop lazily creates a per-element-type drop function for Ref[T].
// T0155: Ref[T] atomic reference counting.
// The drop function atomically decrements the refcount. When it reaches zero,
// drops the inner T value (if T needs dropping) then frees the allocation.
func (c *Compiler) getOrCreateArcDrop(elemType types.Type) *ir.Func {
	elemName := typeArgStr(elemType)
	funcName := "Ref[" + elemName + "].drop"

	if fn, ok := c.funcs[funcName]; ok {
		if len(fn.Blocks) > 0 {
			return fn // already defined
		}
		// Stub declared by declareMonoMethods — fill body below
		c.defineArcDropBody(fn, elemType)
		return fn
	}

	thisParam := ir.NewParam("this", irtypes.I8Ptr)
	fn := c.module.NewFunc(funcName, irtypes.Void, thisParam)

	c.defineArcDropBody(fn, elemType)

	c.funcs[funcName] = fn
	return fn
}

// defineArcDropBody generates the body of an Ref[T] drop function.
// Null-checks, atomically decrements strong_count. When strong_count reaches 0:
// drops inner T, then decrements weak_count. When weak_count reaches 0: frees allocation.
// T0157: Two-stage deallocation — strong_count controls T lifetime, weak_count controls allocation.
func (c *Compiler) defineArcDropBody(fn *ir.Func, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)
	arcStructTy := arcStructType(elemLLVM)
	thisParam := fn.Params[0]
	atomic := c.refIsAtomic(elemType) // T0995: `confined element → non-atomic counter

	entry := fn.NewBlock(".entry")

	// Null check: zero-initialized values (from error handler fallthrough) may be null
	isNull := entry.NewICmp(enum.IPredEQ, thisParam, constant.NewNull(irtypes.I8Ptr))
	decrcBlk := fn.NewBlock("decrc")
	doneBlk := fn.NewBlock("done")
	entry.NewCondBr(isNull, doneBlk, decrcBlk)

	// Atomically decrement strong_count
	typedPtr := decrcBlk.NewBitCast(thisParam, irtypes.NewPointer(arcStructTy))
	rcField := decrcBlk.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldStrong))
	oldRC := c.emitRefCountAdd(decrcBlk, rcField, -1, irtypes.I64, atomic)
	wasOne := decrcBlk.NewICmp(enum.IPredEQ, oldRC, constant.NewInt(irtypes.I64, 1))
	dropBlk := fn.NewBlock("drop_value")
	decrcBlk.NewCondBr(wasOne, dropBlk, doneBlk)

	// Drop inner T value, then decrement weak_count
	dropBlk = c.emitArcInnerDrop(dropBlk, typedPtr, arcStructTy, elemType)
	// Decrement weak_count (the +1 that represents all strong refs)
	wcField := dropBlk.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldWeak))
	oldWC := c.emitRefCountAdd(dropBlk, wcField, -1, irtypes.I64, atomic)
	wcWasOne := dropBlk.NewICmp(enum.IPredEQ, oldWC, constant.NewInt(irtypes.I64, 1))
	freeBlk := fn.NewBlock("free")
	dropBlk.NewCondBr(wcWasOne, freeBlk, doneBlk)

	// Free allocation only when both strong and weak counts are zero
	freeBlk.NewCall(c.palFree, thisParam)
	freeBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)
}

// emitArcInnerDrop emits the type-specific drop logic for the value stored inside an Arc.
// T0155: handles primitives (no-op), strings, vectors, channels, nested arcs, and user types.
// T0157: delegates to emitInnerDrop with arcFieldValue index.
// T0358: returns the (possibly new) block — Vector inner drop emits a loop that
// changes the active block, so callers must thread the returned block.
func (c *Compiler) emitArcInnerDrop(blk *ir.Block, typedPtr value.Value, arcStructTy *irtypes.StructType, elemType types.Type) *ir.Block {
	return c.emitInnerDrop(blk, typedPtr, arcStructTy, elemType, arcFieldValue)
}

// emitInnerDrop emits type-specific drop logic for a value at the given field index
// in a container struct. Used by both Arc and Mutex drop bodies.
// T0358: returns the (possibly new) block. The Vector case emits an element drop
// loop that creates new blocks; callers must continue from the returned block.
func (c *Compiler) emitInnerDrop(blk *ir.Block, typedPtr value.Value, structTy *irtypes.StructType, elemType types.Type, fieldIdx int) *ir.Block {
	named := extractNamed(elemType)
	fi := constant.NewInt(irtypes.I32, int64(fieldIdx))

	// T0389: Tuple element — load the tuple value and drop each droppable element.
	// emitVariantFieldDrop's tuple branch walks elements via ExtractValue. Save/restore
	// c.fn/c.entryBlock/c.block so any sub-blocks (e.g., Vector inside the tuple) land
	// in this drop function's entry, then thread the returned block back.
	if tup, ok := elemType.(*types.Tuple); ok {
		valField := blk.NewGetElementPtr(structTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), fi)
		tupVal := blk.NewLoad(c.resolveType(tup), valField)
		savedFn, savedEntry, savedBlock := c.fn, c.entryBlock, c.block
		c.fn = blk.Parent
		c.entryBlock = blk.Parent.Blocks[0]
		c.block = blk
		c.emitVariantFieldDrop(tupVal, tup)
		blk = c.block
		c.fn, c.entryBlock, c.block = savedFn, savedEntry, savedBlock
		return blk
	}

	// T0850: Optional element (`Ref[T?]` / `Mutex[T?]`) — load the `{i1, inner}`
	// optional value and delegate to emitVariantFieldDrop, whose Optional branch
	// (emitOptionalValueDrop) drops the present inner. Without this, extractNamed
	// returns nil for an Optional, no case below fires, and the inner heap payload
	// leaks. Same save/restore pattern as the Tuple case — emitOptionalValueDrop
	// creates present/none sub-blocks.
	if opt, ok := elemType.(*types.Optional); ok {
		valField := blk.NewGetElementPtr(structTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), fi)
		optVal := blk.NewLoad(c.resolveType(opt), valField)
		savedFn, savedEntry, savedBlock := c.fn, c.entryBlock, c.block
		c.fn = blk.Parent
		c.entryBlock = blk.Parent.Blocks[0]
		c.block = blk
		c.emitVariantFieldDrop(optVal, opt)
		blk = c.block
		c.fn, c.entryBlock, c.block = savedFn, savedEntry, savedBlock
		return blk
	}

	// T1003: Enum element (`Ref[E]` / `Mutex[E]`) — the enum value is embedded
	// directly in the container struct (field is the enum internal type, not a
	// {vtable,instance} value). extractNamed returns nil for an enum, so none of
	// the switch cases below fire; without this the inner enum's droppable variant
	// fields (e.g. a string payload) leak, and the constructor's enum-ctor-temp
	// suppression would otherwise free them early (use-after-free through .borrow).
	if enum := extractEnum(elemType); enum != nil {
		if c.enumInstanceHasDrop(elemType, enum) {
			valField := blk.NewGetElementPtr(structTy, typedPtr,
				constant.NewInt(irtypes.I32, 0), fi)
			enumPtr := blk.NewBitCast(valField, irtypes.I8Ptr)
			enumName := enum.Obj().Name()
			if inst, ok := elemType.(*types.Instance); ok {
				enumName = monoName(inst)
			}
			if dropFn, ok := c.funcs[mangleMethodName(enumName, "drop", false)]; ok {
				blk.NewCall(dropFn, enumPtr)
			}
		}
		return blk
	}

	switch {
	case named != nil && isPrimitiveScalar(named):
		// Copy type — no inner drop needed
		return blk
	case named == types.TypString:
		valField := blk.NewGetElementPtr(structTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), fi)
		strVal := blk.NewLoad(irtypes.I8Ptr, valField)
		if dropFn, ok := c.funcs["promise_string_drop"]; ok {
			blk.NewCall(dropFn, strVal)
		}
	case named != nil && (types.IsVector(elemType) || named == types.TypVector):
		valField := blk.NewGetElementPtr(structTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), fi)
		vecVal := blk.NewLoad(irtypes.I8Ptr, valField)
		// T0358 (T0354 follow-up): drop vector elements before freeing buffer.
		// emitVectorElementDropLoop uses c.block/c.fn/c.entryBlock — temporarily
		// swap them so allocas land in this drop fn's entry block, then restore.
		if innerVecElem, isVec := types.AsVector(elemType); isVec {
			savedFn, savedEntry, savedBlock := c.fn, c.entryBlock, c.block
			c.fn = blk.Parent
			c.entryBlock = blk.Parent.Blocks[0]
			c.block = blk
			c.emitVectorElementDropLoop(vecVal, innerVecElem)
			blk = c.block
			c.fn, c.entryBlock, c.block = savedFn, savedEntry, savedBlock
		}
		if dropFn, ok := c.funcs["Vector.drop"]; ok {
			blk.NewCall(dropFn, vecVal)
		}
	case named != nil && (types.IsChannel(elemType) || named == types.TypChannel):
		valField := blk.NewGetElementPtr(structTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), fi)
		chVal := blk.NewLoad(irtypes.I8Ptr, valField)
		if chElem, ok := types.AsChannel(elemType); ok {
			// T0663: per-element-type drop walks any un-received buffered items.
			innerDropFn := c.getOrCreateChannelDrop(chElem)
			blk.NewCall(innerDropFn, chVal)
		}
	case named != nil && (types.IsArc(elemType) || named == types.TypArc):
		// Nested Arc: load i8* and call the inner Arc's drop
		valField := blk.NewGetElementPtr(structTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), fi)
		innerArc := blk.NewLoad(irtypes.I8Ptr, valField)
		if innerElem, ok := types.AsArc(elemType); ok {
			innerDropFn := c.getOrCreateArcDrop(innerElem)
			blk.NewCall(innerDropFn, innerArc)
		}
	case named != nil && (types.IsWeak(elemType) || named == types.TypWeak):
		// T0157: Nested Weak: load i8* and call the inner Weak's drop
		valField := blk.NewGetElementPtr(structTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), fi)
		innerWeak := blk.NewLoad(irtypes.I8Ptr, valField)
		if innerElem, ok := types.AsWeak(elemType); ok {
			innerDropFn := c.getOrCreateWeakDrop(innerElem)
			blk.NewCall(innerDropFn, innerWeak)
		}
	case named != nil && (types.IsMutex(elemType) || named == types.TypMutex):
		// T0156: Mutex inner drop — resolve element type and get per-instantiation drop
		valField := blk.NewGetElementPtr(structTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), fi)
		innerMutex := blk.NewLoad(irtypes.I8Ptr, valField)
		if mutexElem, ok := types.AsMutex(elemType); ok {
			innerDropFn := c.getOrCreateMutexDrop(mutexElem)
			blk.NewCall(innerDropFn, innerMutex)
		}
	case named != nil && (types.IsMutexGuard(elemType) || named == types.TypMutexGuard):
		// T0156: MutexGuard inner drop
		valField := blk.NewGetElementPtr(structTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), fi)
		innerGuard := blk.NewLoad(irtypes.I8Ptr, valField)
		if dropFn, ok := c.funcs["MutexGuard.drop"]; ok {
			blk.NewCall(dropFn, innerGuard)
		}
	case named != nil && (types.IsAnyTask(elemType) || types.IsTaskLikeOrigin(named)):
		// T0546: Task is an opaque container — slot type is i8*, not userValueType.
		// Load the G handle and join+free the un-awaited task.
		valField := blk.NewGetElementPtr(structTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), fi)
		innerTask := blk.NewLoad(irtypes.I8Ptr, valField)
		if taskElem, ok, taskFail := types.AsAnyTaskFailable(elemType); ok {
			// T0668: route through emitTaskJoinAndFree. emitInnerDrop runs in a
			// synthesized struct/enum field-drop body (c.inCoroutine == false),
			// so this takes the legacy callable Task[T].drop path — identical
			// IR to before; WASM coverage for these non-coroutine bodies comes
			// from defineTaskDropBody's cooperative-step spin pump (Part 2).
			savedFn, savedEntry, savedBlock := c.fn, c.entryBlock, c.block
			c.fn = blk.Parent
			c.entryBlock = blk.Parent.Blocks[0]
			c.block = blk
			c.emitTaskJoinAndFree(innerTask, taskElem, taskFail)
			blk = c.block
			c.fn, c.entryBlock, c.block = savedFn, savedEntry, savedBlock
		}
	case named != nil && (named.HasDrop() || named.NeedsSynthDrop()):
		// User type with explicit or synthesized drop
		valField := blk.NewGetElementPtr(structTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), fi)
		// User types stored as {i8* vtable, i8* instance} — extract instance ptr
		valStruct := blk.NewLoad(userValueType(), valField)
		instancePtr := blk.NewExtractValue(valStruct, 1)
		ownerName := named.Obj().Name()
		if inst, ok := elemType.(*types.Instance); ok {
			ownerName = monoName(inst)
		} else if named.HasDrop() && !named.NeedsSynthDrop() {
			ownerName = c.resolveDropOwner(named)
		}
		mangledName := mangleMethodName(ownerName, "drop", false)
		if dropFn, ok := c.funcs[mangledName]; ok {
			blk.NewCall(dropFn, instancePtr)
		}
		// Free the instance after explicit drop (user drop doesn't include deallocation;
		// synthesized drops already call pal_free internally)
		if !named.IsValueType() && !named.NeedsSynthDrop() {
			blk.NewCall(c.palFree, instancePtr)
		}
	case named != nil && !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural():
		// Heap user type without drop — free the instance pointer
		valField := blk.NewGetElementPtr(structTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), fi)
		valStruct := blk.NewLoad(userValueType(), valField)
		instancePtr := blk.NewExtractValue(valStruct, 1)
		blk.NewCall(c.palFree, instancePtr)
	}
	return blk
}

// dupArc duplicates a Ref reference (i8* → i8*) by incrementing the reference
// count. T0155: Used to prevent use-after-free when a Ref field is read from a
// type with drop. T0995: the increment is non-atomic when elemType is `confined.
func (c *Compiler) dupArc(ptr value.Value, elemType types.Type) value.Value {
	entryBlock := c.block
	nullCheck := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
	incBlock := c.newBlock("arcdup.inc")
	mergeBlock := c.newBlock("arcdup.merge")
	entryBlock.NewCondBr(nullCheck, mergeBlock, incBlock)

	c.block = incBlock
	// Refcount is at offset 0 (i64)
	rcPtr := c.block.NewBitCast(ptr, irtypes.NewPointer(irtypes.I64))
	c.emitRefCountAdd(c.block, rcPtr, 1, irtypes.I64, c.refIsAtomic(elemType))
	incBlock.NewBr(mergeBlock)

	c.block = mergeBlock
	return c.block.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entryBlock),
		ir.NewIncoming(ptr, incBlock),
	)
}

// dupWeak duplicates a Weak reference (i8* → i8*) by atomically incrementing
// the weak reference count. T0157: Used to prevent use-after-free when a Weak field
// is read from a type with drop.
func (c *Compiler) dupWeak(ptr value.Value, elemType types.Type) value.Value {
	entryBlock := c.block
	nullCheck := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
	incBlock := c.newBlock("weakdup.inc")
	mergeBlock := c.newBlock("weakdup.merge")
	entryBlock.NewCondBr(nullCheck, mergeBlock, incBlock)

	c.block = incBlock
	// Weak_count is at offset 1 (i64) — need GEP through Arc struct
	elemLLVM := c.resolveType(elemType)
	arcStructTy := arcStructType(elemLLVM)
	typedPtr := incBlock.NewBitCast(ptr, irtypes.NewPointer(arcStructTy))
	wcField := incBlock.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldWeak))
	c.emitRefCountAdd(c.block, wcField, 1, irtypes.I64, c.refIsAtomic(elemType))
	incBlock.NewBr(mergeBlock)

	c.block = mergeBlock
	return c.block.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entryBlock),
		ir.NewIncoming(ptr, incBlock),
	)
}

// --- Weak[T] codegen (T0157) ---

// getOrCreateWeakDrop lazily creates a per-element-type drop function for Weak[T].
// T0157: Weak[T] references. The drop function atomically decrements weak_count.
// When weak_count reaches zero, frees the allocation (T already dropped by last Arc).
func (c *Compiler) getOrCreateWeakDrop(elemType types.Type) *ir.Func {
	elemName := typeArgStr(elemType)
	funcName := "Weak[" + elemName + "].drop"

	if fn, ok := c.funcs[funcName]; ok {
		if len(fn.Blocks) > 0 {
			return fn // already defined
		}
		c.defineWeakDropBody(fn, elemType)
		return fn
	}

	thisParam := ir.NewParam("this", irtypes.I8Ptr)
	fn := c.module.NewFunc(funcName, irtypes.Void, thisParam)

	c.defineWeakDropBody(fn, elemType)

	c.funcs[funcName] = fn
	return fn
}

// defineWeakDropBody generates the body of a Weak[T] drop function.
// Null-checks, atomically decrements weak_count. When weak_count reaches 0, frees allocation.
// T0157: Weak drop does NOT drop T — that's Arc's responsibility when strong_count reaches 0.
func (c *Compiler) defineWeakDropBody(fn *ir.Func, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)
	arcStructTy := arcStructType(elemLLVM)
	thisParam := fn.Params[0]

	entry := fn.NewBlock(".entry")

	// Null check
	isNull := entry.NewICmp(enum.IPredEQ, thisParam, constant.NewNull(irtypes.I8Ptr))
	decrcBlk := fn.NewBlock("decwc")
	doneBlk := fn.NewBlock("done")
	entry.NewCondBr(isNull, doneBlk, decrcBlk)

	// Atomically decrement weak_count
	typedPtr := decrcBlk.NewBitCast(thisParam, irtypes.NewPointer(arcStructTy))
	wcField := decrcBlk.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldWeak))
	oldWC := c.emitRefCountAdd(decrcBlk, wcField, -1, irtypes.I64, c.refIsAtomic(elemType))
	wasOne := decrcBlk.NewICmp(enum.IPredEQ, oldWC, constant.NewInt(irtypes.I64, 1))
	freeBlk := fn.NewBlock("free")
	decrcBlk.NewCondBr(wasOne, freeBlk, doneBlk)

	// Free allocation (T already dropped by last Arc)
	freeBlk.NewCall(c.palFree, thisParam)
	freeBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)
}

// --- Mutex[T] / MutexGuard[T] codegen (T0156) ---

// defineMutexGuardCloseFunc defines MutexGuard.close(i8* this) -> void.
// Unlocks the mutex and frees the guard allocation. Used by `use` bindings.
func (c *Compiler) defineMutexGuardCloseFunc() {
	thisParam := ir.NewParam("this", irtypes.I8Ptr)
	fn := c.module.NewFunc("MutexGuard.close", irtypes.Void, thisParam)
	c.defineMutexGuardUnlockFreeBody(fn)
	c.funcs["MutexGuard.close"] = fn
}

// defineMutexGuardDropFunc defines MutexGuard.drop(i8* this) -> void.
// Same behavior as close: unlocks the mutex and frees the guard allocation.
func (c *Compiler) defineMutexGuardDropFunc() {
	thisParam := ir.NewParam("this", irtypes.I8Ptr)
	fn := c.module.NewFunc("MutexGuard.drop", irtypes.Void, thisParam)
	c.defineMutexGuardUnlockFreeBody(fn)
	c.funcs["MutexGuard.drop"] = fn
}

// defineMutexGuardUnlockFreeBody generates the body for MutexGuard close/drop:
// null-check, load mutex pointer from guard, scheduler-aware unlock, free guard.
// T0285 added waiter_head/waiter_tail fields. T0291 implements park-and-wake: dequeues a
// waiting goroutine (if any) and hands off the lock (held stays 1), or clears held=0 + signals cond.
// Guard layout: {i8* mutex_alloc_ptr}. Mutex layout: {i8* pal_handle, i8* cond, i8* waiter_head, i8* waiter_tail, i8 held, T value}.
func (c *Compiler) defineMutexGuardUnlockFreeBody(fn *ir.Func) {
	thisParam := fn.Params[0]
	entry := fn.NewBlock(".entry")
	metaTy := mutexMetaType()

	// Null check
	isNull := entry.NewICmp(enum.IPredEQ, thisParam, constant.NewNull(irtypes.I8Ptr))
	unlockBlk := fn.NewBlock("unlock")
	doneBlk := fn.NewBlock("done")
	entry.NewCondBr(isNull, doneBlk, unlockBlk)

	// Load mutex_alloc_ptr from guard (field 0)
	guardStructTy := irtypes.NewStruct(irtypes.I8Ptr)
	guardPtr := unlockBlk.NewBitCast(thisParam, irtypes.NewPointer(guardStructTy))
	mutexField := unlockBlk.NewGetElementPtr(guardStructTy, guardPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	mutexRaw := unlockBlk.NewLoad(irtypes.I8Ptr, mutexField)

	// Bitcast to metadata struct
	typedPtr := unlockBlk.NewBitCast(mutexRaw, irtypes.NewPointer(metaTy))

	// Lock metadata mutex
	handleField := unlockBlk.NewGetElementPtr(metaTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldHandle)))
	handle := unlockBlk.NewLoad(irtypes.I8Ptr, handleField)
	unlockBlk.NewCall(c.palMutexLock, handle)

	// Check for a waiting goroutine on the waiter list
	waiterHeadField := unlockBlk.NewGetElementPtr(metaTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldWaiterHead)))
	waiterTailField := unlockBlk.NewGetElementPtr(metaTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldWaiterTail)))
	waitG := unlockBlk.NewCall(c.funcs["promise_waiter_dequeue"], waiterHeadField, waiterTailField)
	hasWaiter := unlockBlk.NewICmp(enum.IPredNE, waitG, constant.NewNull(irtypes.I8Ptr))
	handoffBlk := fn.NewBlock("unlock.handoff")
	noWaiterBlk := fn.NewBlock("unlock.no_waiter")
	freeGuardBlk := fn.NewBlock("unlock.free_guard")
	unlockBlk.NewCondBr(hasWaiter, handoffBlk, noWaiterBlk)

	// handoff: held stays 1 — hand lock directly to the woken goroutine
	gTy := goroutineStructType()
	waitGTyped := handoffBlk.NewBitCast(waitG, irtypes.NewPointer(gTy))
	waitStatusField := handoffBlk.NewGetElementPtr(gTy, waitGTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	handoffBlk.NewStore(constant.NewInt(irtypes.I8, gStatusRunnable), waitStatusField)
	handoffBlk.NewCall(c.funcs["promise_sched_enqueue"], waitG)
	handoffBlk.NewCall(c.palMutexUnlock, handle)
	handoffBlk.NewBr(freeGuardBlk)

	// no_waiter: clear held=0, signal cond for thread-blocked waiters, unlock.
	// Signal cond BEFORE unlocking so the waking thread (if any) sees a coherent
	// state on re-acquire. T0301: defensive ordering; POSIX allows either order
	// but signal-before-unlock avoids a theoretical window where the waiter
	// re-checks held, races with another thread setting held=1, and misses.
	heldField := noWaiterBlk.NewGetElementPtr(metaTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldHeld)))
	noWaiterBlk.NewStore(constant.NewInt(irtypes.I8, 0), heldField)
	condField := noWaiterBlk.NewGetElementPtr(metaTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldCond)))
	cond := noWaiterBlk.NewLoad(irtypes.I8Ptr, condField)
	noWaiterBlk.NewCall(c.palCondSignal, cond)
	noWaiterBlk.NewCall(c.palMutexUnlock, handle)
	noWaiterBlk.NewBr(freeGuardBlk)

	// Free guard
	freeGuardBlk.NewCall(c.palFree, thisParam)
	freeGuardBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)
}

// getOrCreateMutexDrop returns or creates the per-element-type Mutex[T].drop function.
// Drops inner T, destroys cond var + PAL mutex, frees.
func (c *Compiler) getOrCreateMutexDrop(elemType types.Type) *ir.Func {
	elemName := typeArgStr(elemType)
	funcName := "Mutex[" + elemName + "].drop"

	if fn, ok := c.funcs[funcName]; ok {
		if len(fn.Blocks) > 0 {
			return fn
		}
		c.defineMutexDropBody(fn, elemType)
		return fn
	}

	thisParam := ir.NewParam("this", irtypes.I8Ptr)
	fn := c.module.NewFunc(funcName, irtypes.Void, thisParam)

	c.defineMutexDropBody(fn, elemType)

	c.funcs[funcName] = fn
	return fn
}

// defineMutexDropBody generates the Mutex[T].drop body:
// null-check, drop inner T, destroy cond var, destroy PAL mutex, free allocation.
// T0285: Updated for new layout {pal_handle, cond, waiter_head, waiter_tail, held, T}.
func (c *Compiler) defineMutexDropBody(fn *ir.Func, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)
	mutexStructTy := mutexStructType(elemLLVM)
	thisParam := fn.Params[0]

	entry := fn.NewBlock(".entry")

	// Null check
	isNull := entry.NewICmp(enum.IPredEQ, thisParam, constant.NewNull(irtypes.I8Ptr))
	dropBlk := fn.NewBlock("drop")
	doneBlk := fn.NewBlock("done")
	entry.NewCondBr(isNull, doneBlk, dropBlk)

	typedPtr := dropBlk.NewBitCast(thisParam, irtypes.NewPointer(mutexStructTy))

	// Drop inner T value — Mutex has value at field 5
	dropBlk = c.emitInnerDrop(dropBlk, typedPtr, mutexStructTy, elemType, mutexFieldValue)

	// Destroy cond var (field 1)
	condField := dropBlk.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldCond)))
	condHandle := dropBlk.NewLoad(irtypes.I8Ptr, condField)
	dropBlk.NewCall(c.palCondDestroy, condHandle)

	// Destroy PAL mutex (field 0)
	handleField := dropBlk.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldHandle)))
	handle := dropBlk.NewLoad(irtypes.I8Ptr, handleField)
	dropBlk.NewCall(c.palMutexDestroy, handle)

	// Free allocation
	dropBlk.NewCall(c.palFree, thisParam)
	dropBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)
}

// --- Task[T] codegen (T0503) ---

// getOrCreateTaskDrop returns or creates the per-element-type Task[T].drop function.
// T0503: When a task handle is never awaited via <-t, scope-exit drop blocks
// until the goroutine finishes, drops the result value (if any), then frees
// G.result_ptr (skipping the void sentinel 0x1), panic_msg (if heap-allocated),
// and the G struct itself.
//
// T0668: Task[T].drop is split into a spin-wait shell + Task[T].free_after_done
// (the spin-free post-done cleanup). Coroutine-reachable drop sites instead use
// the cooperative park-suspend join (emitTaskJoinAndFree), mirroring the proven
// `<-t` await path so the single-threaded WASM scheduler can run the pending
// goroutine. This legacy callable Task[T].drop remains the fallback for
// genuinely non-coroutine drop bodies (synthesized struct/enum/Arc field drops,
// monomorphized Promise Map[K,Task].drop); on WASM its spin pumps the
// cooperative scheduler one step per iteration instead of a no-op usleep.
func (c *Compiler) getOrCreateTaskDrop(elemType types.Type, failable bool) *ir.Func {
	elemName := "void"
	if elemType != nil {
		elemName = typeArgStr(elemType)
	}
	// T1379: a FailableTask[T] drop differs only in free_after_done, which
	// discharges the buffered {ok,value,err} aggregate (drops the success value
	// or frees the error) before freeing the buffer. Distinct name so the two
	// per-element-type drops never collide.
	handleName := "Task"
	if failable {
		handleName = "FailableTask"
	}
	funcName := handleName + "[" + elemName + "].drop"

	if fn, ok := c.funcs[funcName]; ok {
		if len(fn.Blocks) == 0 {
			c.defineTaskDropBody(fn, elemType, failable)
		}
		// T0668: keep the drop→free_after_done mapping populated even on the
		// already-defined / declared-only fast paths (used by the by-drop-fn
		// temp/binding join route).
		c.taskFreeAfterDone[fn] = c.getOrCreateTaskFreeAfterDone(elemType, failable)
		return fn
	}

	thisParam := ir.NewParam("this", irtypes.I8Ptr)
	fn := c.module.NewFunc(funcName, irtypes.Void, thisParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	c.defineTaskDropBody(fn, elemType, failable)

	c.funcs[funcName] = fn
	c.taskFreeAfterDone[fn] = c.getOrCreateTaskFreeAfterDone(elemType, failable)
	return fn
}

// defineTaskDropBody generates the legacy callable Task[T].drop body:
// null-check → spin-wait on G.done → Task[T].free_after_done(this).
//
// T0668: On the single-threaded WASM cooperative scheduler a busy usleep spin
// never lets the pending goroutine run (livelock). WASM therefore pumps one
// promise_sched_coop_step() per spin iteration; if a step makes no progress and
// G is still not done the program is genuinely deadlocked → terminal message +
// exit(2), matching promise_sched_coop_run's deadlock block. The host path is
// unchanged (usleep; another M runs the awaited G).
func (c *Compiler) defineTaskDropBody(fn *ir.Func, elemType types.Type, failable bool) {
	gTy := goroutineStructType()
	thisParam := fn.Params[0]

	entry := fn.NewBlock(".entry")

	// Null check
	isNull := entry.NewICmp(enum.IPredEQ, thisParam, constant.NewNull(irtypes.I8Ptr))
	checkBlk := fn.NewBlock("task.drop.check")
	spinBlk := fn.NewBlock("task.drop.spin")
	readyBlk := fn.NewBlock("task.drop.ready")
	doneBlk := fn.NewBlock("done")
	entry.NewCondBr(isNull, doneBlk, checkBlk)

	gPtr := checkBlk.NewBitCast(thisParam, irtypes.NewPointer(gTy))

	// Spin-wait loop: re-load G.done atomically (acquire) so the LLVM optimizer
	// cannot hoist or cache the load across iterations (T0669).
	doneField := checkBlk.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldDone)))
	doneLoad := checkBlk.NewLoad(irtypes.I8, doneField)
	doneLoad.Atomic = true
	doneLoad.Ordering = enum.AtomicOrderingAcquire
	doneLoad.Align = 1
	doneVal := doneLoad
	isDone := checkBlk.NewICmp(enum.IPredNE, doneVal, constant.NewInt(irtypes.I8, 0))
	checkBlk.NewCondBr(isDone, readyBlk, spinBlk)

	if c.isWasm {
		// T0668: pump the cooperative scheduler one step instead of a no-op
		// usleep. promise_sched_coop_step() returns i8: non-zero = ran/advanced
		// a G (progress possible), 0 = no runnable G.
		stepFn := c.funcs["promise_sched_coop_step"]
		stepR := spinBlk.NewCall(stepFn)
		coopRecheckBlk := fn.NewBlock("task.drop.coop_recheck")
		deadlockBlk := fn.NewBlock("task.drop.deadlock")
		spinProgressBlk := fn.NewBlock("task.drop.progress")
		// T0680 Part 2: stepR==2 = per-test deadline reached. Stop spinning and go
		// straight to done, skipping Task[T].free_after_done — the awaited G may
		// still be running (a leak), but a timed-out test (result==2) skips the
		// leak check, so this cannot surface a false LEAK. Prevents a livelock
		// nested under this drop-join from spinning coop_step→2 forever.
		isTimeout := spinBlk.NewICmp(enum.IPredEQ, stepR, constant.NewInt(irtypes.I8, 2))
		spinBlk.NewCondBr(isTimeout, doneBlk, spinProgressBlk)

		madeProgress := spinProgressBlk.NewICmp(enum.IPredNE, stepR, constant.NewInt(irtypes.I8, 0))
		spinProgressBlk.NewCondBr(madeProgress, checkBlk, coopRecheckBlk)

		// No runnable G — re-check G.done. If the awaited G is still not done it
		// can never complete (nothing left to run) → genuine deadlock.
		rdLoad := coopRecheckBlk.NewLoad(irtypes.I8, doneField)
		rdLoad.Atomic = true
		rdLoad.Ordering = enum.AtomicOrderingAcquire
		rdLoad.Align = 1
		rdDone := coopRecheckBlk.NewICmp(enum.IPredNE, rdLoad, constant.NewInt(irtypes.I8, 0))
		coopRecheckBlk.NewCondBr(rdDone, checkBlk, deadlockBlk)

		msg := c.getTaskDeadlockMsgGlobal()
		msgPtr := deadlockBlk.NewGetElementPtr(msg.ContentType, msg,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		deadlockBlk.NewCall(c.palWrite, constant.NewInt(irtypes.I32, 2), msgPtr,
			constant.NewInt(irtypes.I64, 45))
		deadlockBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 2))
		deadlockBlk.NewUnreachable()
	} else {
		spinBlk.NewCall(c.palUsleep, constant.NewInt(irtypes.I32, 100))
		spinBlk.NewBr(checkBlk)
	}

	// ready: G is done — defer the post-done cleanup to Task[T].free_after_done.
	readyBlk.NewCall(c.getOrCreateTaskFreeAfterDone(elemType, failable), thisParam)
	readyBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)
}

// getOrCreateTaskFreeAfterDone returns or creates Task[T].free_after_done — the
// spin-free post-done cleanup extracted from the old Task[T].drop (T0668).
// Assumes the goroutine G is already done. Used by both the legacy spin shell
// (after the wait) and the cooperative park-suspend join (emitTaskJoinAndFree),
// so the post-done IR exists in exactly one place.
func (c *Compiler) getOrCreateTaskFreeAfterDone(elemType types.Type, failable bool) *ir.Func {
	elemName := "void"
	if elemType != nil {
		elemName = typeArgStr(elemType)
	}
	handleName := "Task"
	if failable {
		handleName = "FailableTask"
	}
	funcName := handleName + "[" + elemName + "].free_after_done"

	if fn, ok := c.funcs[funcName]; ok {
		if len(fn.Blocks) == 0 {
			c.defineTaskFreeAfterDoneBody(fn, elemType, failable)
		}
		return fn
	}

	thisParam := ir.NewParam("this", irtypes.I8Ptr)
	fn := c.module.NewFunc(funcName, irtypes.Void, thisParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	c.defineTaskFreeAfterDoneBody(fn, elemType, failable)

	c.funcs[funcName] = fn
	return fn
}

// defineTaskFreeAfterDoneBody generates the Task[T].free_after_done body
// (T0668): null-check → drop result T (if any, and not the void sentinel 0x1)
// → free result_ptr (when non-sentinel) → free panic_msg if heap-allocated →
// free the G struct. This is the old Task[T].drop post-done logic moved
// verbatim; the only structural change is the leading null check (callers may
// pass a consumed / zero-initialized handle) and the entry→ready wiring.
func (c *Compiler) defineTaskFreeAfterDoneBody(fn *ir.Func, elemType types.Type, failable bool) {
	gTy := goroutineStructType()
	thisParam := fn.Params[0]
	// T1379: a FailableTask[T] result buffer holds the failable aggregate
	// {i1 ok, T value, i8* err} (never bare T). It is therefore never "void" as
	// far as the buffer is concerned — even T = void stores {i1, i8*}.
	isVoid := (elemType == nil || elemType == types.TypVoid) && !failable

	entry := fn.NewBlock(".entry")
	readyBlk := fn.NewBlock("task.fad.ready")
	doneBlk := fn.NewBlock("done")

	// Null check — callers (Task[T].drop ready path, the cooperative join, and
	// consumed/zero-initialized slots) may pass a null handle.
	isNull := entry.NewICmp(enum.IPredEQ, thisParam, constant.NewNull(irtypes.I8Ptr))
	entry.NewCondBr(isNull, doneBlk, readyBlk)

	// ready: drop result, then free result_ptr, panic_msg, G.
	// emitVariantFieldDrop uses c.fn/c.entryBlock/c.block; save and restore so
	// caller state isn't disturbed (same pattern as defineArcDropBody's vector path).
	// T1150: also neutralize the coroutine context. This plain (non-coroutine)
	// function may be generated lazily while compiling a coroutine body. If the
	// result type T is itself a Task[U], emitVariantFieldDrop routes to
	// emitTaskJoinAndFree, which — seeing a stale c.inCoroutine/c.coroSuspendBlk —
	// would emit a coro.suspend referencing the *enclosing* coroutine's cleanup
	// block (which doesn't exist here → "use of undefined value '%cleanup'").
	// Clearing inCoroutine routes the nested task-field drop through the legacy
	// callable Task[U].drop (spin shell on host, coop-step pump on WASM), which is
	// valid inside a plain function.
	savedFn, savedEntry, savedBlock := c.fn, c.entryBlock, c.block
	savedInCoroutine, savedCoroSuspend, savedCoroCleanup := c.inCoroutine, c.coroSuspendBlk, c.coroCleanupBlk
	c.fn = fn
	c.entryBlock = entry
	c.block = readyBlk
	c.inCoroutine = false
	c.coroSuspendBlk = nil
	c.coroCleanupBlk = nil

	gPtr := c.block.NewBitCast(thisParam, irtypes.NewPointer(gTy))

	// T0594: hoist G.panicked load so the non-void result path can skip loading
	// uninitialized result_ptr contents when the goroutine panicked before writing.
	panickedField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
	panickedVal := c.block.NewLoad(irtypes.I8, panickedField)
	isPanicked := c.block.NewICmp(enum.IPredNE, panickedVal, constant.NewInt(irtypes.I8, 0))

	rpField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
	rpVal := c.block.NewLoad(irtypes.I8Ptr, rpField)

	if failable {
		// T1379: FailableTask[T] — result_ptr holds the {ok,value,err} aggregate.
		// An un-received (dropped) failable task must discharge its buffered
		// payload so neither the success value nor the error instance leaks:
		//   ok==true  → drop the success value T (if any);
		//   ok==false → drop the buffered error instance.
		// Then free the aggregate buffer. Skip entirely if the goroutine panicked
		// (aggregate never written) or the buffer is null/sentinel.
		tVoid := (elemType == nil || elemType == types.TypVoid)
		var innerLLVM irtypes.Type = irtypes.Void
		if !tVoid {
			innerLLVM = c.resolveType(elemType)
		}
		aggType := computeResultType(innerLLVM)

		sentinelInt := c.block.NewPtrToInt(rpVal, c.ptrIntType())
		isSentinel := c.block.NewICmp(enum.IPredULE, sentinelInt,
			constant.NewInt(c.ptrIntType(), 1))
		notSentinelBlk := c.newBlock("ftask.drop.not_sentinel")
		freeBufOnlyBlk := c.newBlock("ftask.drop.free_buf_only")
		dischargeBlk := c.newBlock("ftask.drop.discharge")
		afterResultBlk := c.newBlock("ftask.drop.after_result")
		c.block.NewCondBr(isSentinel, afterResultBlk, notSentinelBlk)

		// Panicked goroutine never wrote the aggregate → free the buffer only.
		c.block = notSentinelBlk
		c.block.NewCondBr(isPanicked, freeBufOnlyBlk, dischargeBlk)

		c.block = freeBufOnlyBlk
		c.block.NewCall(c.palFree, rpVal)
		c.block.NewBr(afterResultBlk)

		// discharge: load the aggregate, then drop success value or free error.
		c.block = dischargeBlk
		typedRP := c.block.NewBitCast(rpVal, irtypes.NewPointer(aggType))
		agg := c.block.NewLoad(aggType, typedRP)
		tag := c.block.NewExtractValue(agg, 0) // i1: 0 = ok, 1 = error
		errIdx := resultErrIdx(aggType)
		okBlk := c.newBlock("ftask.drop.ok")
		errBlk := c.newBlock("ftask.drop.err")
		freeBlk2 := c.newBlock("ftask.drop.free_buf")
		c.block.NewCondBr(tag, errBlk, okBlk)

		// ok path: drop the success value T (if non-void).
		c.block = okBlk
		if !tVoid {
			okVal := c.block.NewExtractValue(agg, 1)
			c.emitVariantFieldDrop(okVal, elemType)
		}
		c.block.NewBr(freeBlk2)

		// err path: free the buffered error instance via runtime drop dispatch.
		c.block = errBlk
		errVal := c.block.NewExtractValue(agg, errIdx)
		c.emitFailableErrorDrop(errVal)
		c.block.NewBr(freeBlk2)

		c.block = freeBlk2
		c.block.NewCall(c.palFree, rpVal)
		c.block.NewBr(afterResultBlk)

		c.block = afterResultBlk
	} else if !isVoid {
		// Non-void task: result_ptr is a heap allocation holding T.
		// Defensive sentinel guard: skip if result_ptr is null or the 0x1 sentinel.
		resultLLVM := c.resolveType(elemType)
		sentinelInt := c.block.NewPtrToInt(rpVal, c.ptrIntType())
		isSentinel := c.block.NewICmp(enum.IPredULE, sentinelInt,
			constant.NewInt(c.ptrIntType(), 1))
		notSentinelBlk := c.newBlock("task.drop.not_sentinel")
		freeBufOnlyBlk := c.newBlock("task.drop.free_buf_only")
		dropAndFreeBlk := c.newBlock("task.drop.drop_and_free")
		afterResultBlk := c.newBlock("task.drop.after_result")
		c.block.NewCondBr(isSentinel, afterResultBlk, notSentinelBlk)

		// T0594: panicked goroutine never wrote result_ptr — split into free-only
		// (panicked) vs drop-then-free (normal) to avoid walking uninitialized memory.
		c.block = notSentinelBlk
		c.block.NewCondBr(isPanicked, freeBufOnlyBlk, dropAndFreeBlk)

		c.block = freeBufOnlyBlk
		c.block.NewCall(c.palFree, rpVal)
		c.block.NewBr(afterResultBlk)

		c.block = dropAndFreeBlk
		typedRP := c.block.NewBitCast(rpVal, irtypes.NewPointer(resultLLVM))
		loadedVal := c.block.NewLoad(resultLLVM, typedRP)
		c.emitVariantFieldDrop(loadedVal, elemType)
		c.block.NewCall(c.palFree, rpVal)
		c.block.NewBr(afterResultBlk)

		c.block = afterResultBlk
	}
	// Void task: result_ptr is the sentinel 0x1 — never freed.

	// Free panic_msg if heap-allocated (panicked == 2).
	// T0594: reuse the hoisted panickedVal rather than reloading G.panicked.
	isHeapMsg := c.block.NewICmp(enum.IPredEQ, panickedVal,
		constant.NewInt(irtypes.I8, int64(gPanickedHeapMsg)))
	freePanicBlk := c.newBlock("task.drop.free_panic")
	freeGBlk := c.newBlock("task.drop.free_g")
	c.block.NewCondBr(isHeapMsg, freePanicBlk, freeGBlk)

	panicMsgField := freePanicBlk.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
	panicMsg := freePanicBlk.NewLoad(irtypes.I8Ptr, panicMsgField)
	freePanicBlk.NewCall(c.palFree, panicMsg)
	freePanicBlk.NewBr(freeGBlk)

	freeGBlk.NewCall(c.palFree, thisParam)
	freeGBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)

	c.fn, c.entryBlock, c.block = savedFn, savedEntry, savedBlock
	c.inCoroutine, c.coroSuspendBlk, c.coroCleanupBlk = savedInCoroutine, savedCoroSuspend, savedCoroCleanup
}

// getTaskDeadlockMsgGlobal returns the shared private deadlock-message global
// used by the WASM Task[T].drop cooperative-step spin pump (T0668). Created
// once and reused across all Task[T].drop instantiations to avoid emitting one
// duplicate 45-byte global per element type.
func (c *Compiler) getTaskDeadlockMsgGlobal() *ir.Global {
	if c.taskDeadlockMsgGlobal != nil {
		return c.taskDeadlockMsgGlobal
	}
	msg := constant.NewCharArrayFromString("fatal: all goroutines are asleep - deadlock!\n")
	g := c.module.NewGlobalDef(".str.deadlock.taskdrop", msg)
	g.Immutable = true
	g.Linkage = enum.LinkagePrivate
	c.taskDeadlockMsgGlobal = g
	return g
}

// dupHeapValue duplicates a heap user type value struct by allocating a new
// instance, memcpy'ing the original, and dup'ing any droppable sub-fields.
// B0236: Used for match destructure of droppable enums where extracted heap
// user type values would otherwise share instance pointers with enum data.
// T0387: For polymorphic types (those needing a vtable), dispatches to a
// per-concrete-type clone fn via typeinfo's clone_fn_ptr so the runtime concrete
// subtype is duplicated correctly (right size + sub-field dup matching the
// concrete layout). The static path is used otherwise.
func (c *Compiler) dupHeapValue(val value.Value, resolvedType types.Type) value.Value {
	// T0545 backstop: single-owner native handles (Task/Mutex/MutexGuard) are
	// raw i8* handles, not the `{vtable,instance}` value struct this function
	// assumes (val.Type().(*StructType) would panic). They have no dup
	// semantics; return unchanged. Sema rejects the user-reachable paths.
	if isSingleOwnerHandleType(resolvedType) {
		return val
	}
	named := extractNamed(resolvedType)
	layout := c.lookupTypeLayout(resolvedType)
	if layout == nil || layout.Instance == nil {
		// No layout found — return as-is (shouldn't happen for heap user types)
		return val
	}

	// Extract instance pointer from value struct (field 1)
	instancePtr := c.extractInstancePtr(val)

	// Null check
	entryBlock := c.block
	nullCheck := c.block.NewICmp(enum.IPredEQ, instancePtr, constant.NewNull(irtypes.I8Ptr))
	dupBlock := c.newBlock("heapdup.copy")
	mergeBlock := c.newBlock("heapdup.merge")
	entryBlock.NewCondBr(nullCheck, mergeBlock, dupBlock)

	c.block = dupBlock

	var newPtr value.Value
	if named != nil && c.needsVtable(named) {
		// T0387: Polymorphic dispatch via typeinfo's clone_fn_ptr.
		// For polymorphic types, the runtime concrete subtype may have a different
		// size and additional droppable fields beyond the static layout. Loading
		// the clone fn from typeinfo (set up by maybeSynthesizeCloneFn) ensures we
		// allocate + memcpy the right size and dup the right fields.
		variantPtr := c.loadVariantPtr(instancePtr)
		typeinfoType := irtypes.NewStruct(
			irtypes.I8Ptr, // 0: vtable_ptr
			irtypes.I8Ptr, // 1: drop_fn_ptr
			irtypes.I8Ptr, // 2: clone_fn_ptr
		)
		typedTI := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoType))
		cloneFnField := c.block.NewGetElementPtr(typeinfoType, typedTI,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
		cloneFn := c.block.NewLoad(irtypes.I8Ptr, cloneFnField)

		// If clone_fn is null (e.g., abstract base or types not eligible for synth
		// clone), fall back to the static path so dupHeapValue still produces a copy.
		isNull := c.block.NewICmp(enum.IPredEQ, cloneFn, constant.NewNull(irtypes.I8Ptr))
		dispatchBlk := c.newBlock("heapdup.dispatch")
		staticBlk := c.newBlock("heapdup.static")
		dynMergeBlk := c.newBlock("heapdup.dyn_merge")
		c.block.NewCondBr(isNull, staticBlk, dispatchBlk)

		// Dynamic dispatch through the typeinfo clone fn pointer.
		c.block = dispatchBlk
		cloneFnType := irtypes.NewFunc(irtypes.I8Ptr, irtypes.I8Ptr)
		typedFn := c.block.NewBitCast(cloneFn, irtypes.NewPointer(cloneFnType))
		dynNewPtr := c.block.NewCall(typedFn, instancePtr)
		dispatchEnd := c.block
		dispatchEnd.NewBr(dynMergeBlk)

		// Static fallback for null clone fn pointer.
		c.block = staticBlk
		staticNewPtr := c.dupHeapValueStatic(instancePtr, resolvedType, layout)
		staticEnd := c.block
		staticEnd.NewBr(dynMergeBlk)

		c.block = dynMergeBlk
		newPtr = c.block.NewPhi(
			ir.NewIncoming(dynNewPtr, dispatchEnd),
			ir.NewIncoming(staticNewPtr, staticEnd),
		)
	} else {
		newPtr = c.dupHeapValueStatic(instancePtr, resolvedType, layout)
	}

	// Build new value struct: same vtable, new instance pointer
	vtablePtr := c.extractVtablePtr(val)
	valType := val.Type().(*irtypes.StructType)
	newVal := c.block.NewInsertValue(constant.NewZeroInitializer(valType), vtablePtr, 0)
	newVal2 := c.block.NewInsertValue(newVal, newPtr, 1)
	dupEnd := c.block
	dupEnd.NewBr(mergeBlock)

	// Merge: null → original val, non-null → dup'd val
	c.block = mergeBlock
	return c.block.NewPhi(
		ir.NewIncoming(val, entryBlock),
		ir.NewIncoming(newVal2, dupEnd),
	)
}

// dupHeapValueStatic emits the static (non-polymorphic) duplication path:
// allocate using the static layout's size, memcpy from the source instance,
// and dup droppable sub-fields. Used directly for non-polymorphic types and as
// a fallback when typeinfo's clone_fn_ptr is null (T0387).
func (c *Compiler) dupHeapValueStatic(instancePtr value.Value, resolvedType types.Type, layout *TypeDeclLayout) value.Value {
	named := extractNamed(resolvedType)
	instanceStructType := layout.Instance.LLVMType
	instancePtrType := layout.InstancePtrType

	nullPtr := constant.NewNull(instancePtrType)
	sizePtr := c.block.NewGetElementPtr(instanceStructType, nullPtr,
		constant.NewInt(irtypes.I32, 1))
	sizeRaw := c.block.NewPtrToInt(sizePtr, c.ptrIntType())
	var size value.Value = sizeRaw
	if c.isWasm {
		size = c.block.NewZExt(sizeRaw, irtypes.I64)
	}

	newPtr := c.block.NewCall(c.palAlloc, size)
	c.block.NewCall(c.funcs["llvm.memcpy"], newPtr, instancePtr, size, constant.False)

	typedNewPtr := c.block.NewBitCast(newPtr, instancePtrType)
	c.dupHeapValueFields(named, resolvedType, layout, typedNewPtr)
	return newPtr
}

// emitSingleOwnerHandleDupPanic is the T1113 defense-in-depth backstop. A
// single-owner native handle (Task/Mutex/MutexGuard) is a bare i8* owning one
// allocation and has NO dup semantics, so a structural-copy path that reaches one
// must panic rather than silently shallow-copy — a shallow copy aliases the
// source handle and double-frees / UAFs at drop (the original T1109 bug). Every
// reachable user path is rejected earlier: the by-value container read
// (`container[k]!` of a struct/enum nesting a handle) by the ownership pass
// (rejectIndexExprSingleOwnerMove via FirstFieldNestedSingleOwnerHandle), and
// .clone()/.filled() of handle-bearing aggregates by sema (validateCloneType /
// checkContainerNotCloneable). So this is unreachable in well-formed programs; it
// only fires if a future path slips through, converting silent corruption into a
// clear panic (mirrors the T0559 vector / T0813 closure backstops). After the
// panic return terminates the block, c.block is moved to a fresh (dead,
// unreachable) block so the caller's subsequent codegen appends to valid IR.
func (c *Compiler) emitSingleOwnerHandleDupPanic(handleName string) {
	panicMsg := c.makeGlobalString(fmt.Sprintf(
		"internal: cannot duplicate single-owner handle %s", handleName))
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.emitPanicReturn()
	c.block = c.newBlock("handle.dup.unreachable")
}

// singleOwnerHandleName returns "Task"/"Mutex"/"MutexGuard" if typ is one of the
// single-owner native handles, else "". named is typ's extractNamed result.
func singleOwnerHandleName(typ types.Type, named *types.Named) string {
	if _, ok := types.AsMutex(typ); ok || named == types.TypMutex {
		return "Mutex"
	}
	if _, ok := types.AsMutexGuard(typ); ok || named == types.TypMutexGuard {
		return "MutexGuard"
	}
	if _, ok := types.AsFailableTask(typ); ok || named == types.TypFailableTask {
		return "FailableTask"
	}
	if _, ok := types.AsTask(typ); ok || named == types.TypTask {
		return "Task"
	}
	return ""
}

// dupHeapValueFields walks the fields of a heap user type instance and dups
// any droppable sub-fields (strings, vectors, channels, nested heap types).
// B0236: Called after memcpy to fix up shared pointers in the new copy.
func (c *Compiler) dupHeapValueFields(named *types.Named, resolvedType types.Type, layout *TypeDeclLayout, typedNewPtr value.Value) {
	// Build substitution for generic instances
	var subst map[*types.TypeParam]types.Type
	if inst, ok := resolvedType.(*types.Instance); ok && len(named.TypeParams()) > 0 {
		subst = types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
	} else if c.typeSubst != nil {
		subst = c.typeSubst
	}

	instanceStructType := layout.Instance.LLVMType
	for _, f := range named.AllFields() {
		fieldIdx, ok := layout.InstanceFieldIndex[f.Name()]
		if !ok {
			continue
		}

		fType := f.Type()
		if subst != nil {
			fType = types.Substitute(fType, subst)
		}

		fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedNewPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		fieldLLVMType := layout.Instance.Fields[fieldIdx].LLVMType

		if _, isSig := fType.(*types.Signature); isSig {
			// T0813: a closure env cannot be deep-cloned (the captured frame is
			// opaque); null the cloned slot so the source keeps sole ownership of
			// the env (dropped exactly once), mirroring emitVariantFieldDup's
			// Signature case. Defense-in-depth — sema now rejects clone()/filled()
			// of closure-containing aggregates, but residual implicit-dup paths
			// (polymorphic slice, etc.) still reach here and would otherwise alias
			// the env pointer between two droppable owners → double-free.
			c.block.NewStore(constant.NewZeroInitializer(fieldLLVMType), fieldPtr)
			continue
		}

		fNamed := extractNamed(fType)
		if fNamed == nil {
			continue
		}

		fieldVal := c.block.NewLoad(fieldLLVMType, fieldPtr)

		if fNamed == types.TypString {
			dup := c.dupString(fieldVal)
			c.block.NewStore(dup, fieldPtr)
		} else if elemType, isVec := types.AsVector(fType); isVec {
			elemLLVM := c.resolveType(elemType)
			elemSize := int64(c.typeSize(elemLLVM))
			dup := c.dupVector(fieldVal, elemSize)
			// B0276: Deep-clone droppable elements (strings, heap types, etc.)
			// to prevent double-free when both original and dup are dropped.
			c.emitVectorElementCloneLoop(dup, elemType)
			c.block.NewStore(dup, fieldPtr)
		} else if _, isChan := types.AsChannel(fType); isChan || fNamed == types.TypChannel {
			dup := c.dupChannel(fieldVal)
			c.block.NewStore(dup, fieldPtr)
		} else if arcElem, isArc := types.AsArc(fType); isArc || fNamed == types.TypArc {
			dup := c.dupArc(fieldVal, arcElem)
			c.block.NewStore(dup, fieldPtr)
		} else if _, isWeak := types.AsWeak(fType); isWeak || fNamed == types.TypWeak {
			elemType := fType
			if w, ok := types.AsWeak(fType); ok {
				elemType = w
			}
			dup := c.dupWeak(fieldVal, elemType)
			c.block.NewStore(dup, fieldPtr)
		} else if h := singleOwnerHandleName(fType, fNamed); h != "" {
			// T1113: The single-owner native handles (Task/Mutex/MutexGuard —
			// Vector/Channel/Arc/Weak are handled above, and all satisfy
			// isOpaqueContainerType) have NO dup semantics. The old T0387 no-op
			// shallow copy aliased the source's handle and double-freed / UAF'd at
			// drop. Panic backstop — see emitSingleOwnerHandleDupPanic.
			c.emitSingleOwnerHandleDupPanic(h)
		} else if !fNamed.IsValueType() && !fNamed.IsCopy() && !isPrimitiveScalar(fNamed) && !fNamed.IsStructural() {
			// Nested heap user type — recursive dup
			dup := c.dupHeapValue(fieldVal, fType)
			c.block.NewStore(dup, fieldPtr)
		}
	}
}

// dupEnumElementInPlace modifies an already-memcpy'd enum element in place,
// duping any droppable variant fields so the copy owns independent data.
// B0290: Used for vector-of-enum elements during dupHeapValue when the enum
// has droppable variant data but no clone method (e.g., Slot[K,V] in Map).
// Uses c.enumDupInProgress to detect recursive types and prevent infinite codegen.
func (c *Compiler) dupEnumElementInPlace(elemPtr value.Value, elemType types.Type) {
	enum := extractEnum(elemType)
	if enum == nil {
		return
	}

	// Cycle detection: recursive types (e.g., JsonValue containing Vector[JsonValue])
	// would cause infinite codegen. Track which enums are being processed and skip
	// if we encounter one already in progress — the shallow memcpy from dupVector
	// is sufficient for the recursive level since the outer level handles depth-1 dup.
	if c.enumDupInProgress == nil {
		c.enumDupInProgress = make(map[*types.Enum]bool)
	}
	if c.enumDupInProgress[enum] {
		return // cycle detected — shallow copy from dupVector is sufficient
	}
	c.enumDupInProgress[enum] = true
	defer func() { delete(c.enumDupInProgress, enum) }()

	layout := c.lookupEnumLayout(elemType)
	if layout == nil {
		return
	}

	internalType, ok := layout.EnumInternalType.(*irtypes.StructType)
	if !ok {
		return // fieldless enum — nothing to dup
	}

	// Build substitution for generic instances
	var subst map[*types.TypeParam]types.Type
	if inst, ok := elemType.(*types.Instance); ok {
		subst = types.BuildSubstMap(enum.TypeParams(), inst.TypeArgs())
	}

	// elemPtr points to the enum internal type {i32 tag, [N x i8] data}
	typedPtr := c.block.NewBitCast(elemPtr, irtypes.NewPointer(internalType))

	// Load tag (index 0)
	tagPtr := c.block.NewGetElementPtr(internalType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	tag := c.block.NewLoad(irtypes.I32, tagPtr)

	// Data area pointer (index 1)
	dataPtr := c.block.NewGetElementPtr(internalType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))

	// Collect variants that need dup
	type variantDup struct {
		tag      int
		name     string
		variant  *types.Variant
		dataType *irtypes.StructType
	}
	var duppableVariants []variantDup
	for _, v := range enum.Variants() {
		if v.NumFields() == 0 {
			continue
		}
		dt := layout.VariantDataTypes[v.Name()]
		if dt == nil {
			continue
		}
		hasDroppable := false
		for _, f := range v.Fields() {
			fType := f.Type()
			if subst != nil {
				fType = types.Substitute(fType, subst)
			}
			if c.variantFieldNeedsDrop(fType) {
				hasDroppable = true
				break
			}
		}
		if hasDroppable {
			duppableVariants = append(duppableVariants, variantDup{
				tag:      layout.VariantTag[v.Name()],
				name:     v.Name(),
				variant:  v,
				dataType: dt,
			})
		}
	}

	if len(duppableVariants) == 0 {
		return
	}

	switchBlock := c.block
	doneBlock := c.newBlock("enumdup.done")
	var cases []*ir.Case
	for _, vd := range duppableVariants {
		varBlock := c.newBlock(fmt.Sprintf("enumdup.%s", vd.name))
		cases = append(cases, &ir.Case{X: constant.NewInt(irtypes.I32, int64(vd.tag)), Target: varBlock})

		c.block = varBlock
		typedDataPtr := c.block.NewBitCast(dataPtr, irtypes.NewPointer(vd.dataType))

		for i, f := range vd.variant.Fields() {
			fType := f.Type()
			if subst != nil {
				fType = types.Substitute(fType, subst)
			}
			if !c.variantFieldNeedsDrop(fType) {
				continue
			}
			fieldPtr := c.block.NewGetElementPtr(vd.dataType, typedDataPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			fieldVal := c.block.NewLoad(vd.dataType.Fields[i], fieldPtr)

			c.emitVariantFieldDup(fieldVal, fieldPtr, fType)
		}
		c.block.NewBr(doneBlock)
	}
	switchBlock.NewSwitch(tag, doneBlock, cases...)

	c.block = doneBlock
}

// emitVariantFieldDup dups a single variant field value (string, vector, channel,
// or heap user type) and stores the duped copy back to fieldPtr.
// B0290: Mirrors emitVariantFieldDrop but performs dup instead of drop.
func (c *Compiler) emitVariantFieldDup(fieldVal value.Value, fieldPtr value.Value, typ types.Type) {
	named := extractNamed(typ)
	if named != nil {
		if named == types.TypString {
			dup := c.dupString(fieldVal)
			c.block.NewStore(dup, fieldPtr)
			return
		}
		if elemType, isVec := types.AsVector(typ); isVec {
			elemLLVM := c.resolveType(elemType)
			elemSize := int64(c.typeSize(elemLLVM))
			dup := c.dupVector(fieldVal, elemSize)
			c.emitVectorElementCloneLoop(dup, elemType)
			c.block.NewStore(dup, fieldPtr)
			return
		}
		if _, isCh := types.AsChannel(typ); isCh || named == types.TypChannel {
			dup := c.dupChannel(fieldVal)
			c.block.NewStore(dup, fieldPtr)
			return
		}
		// T1109: Ref/Arc[T] variant field — strong-count increment (non-atomic when
		// `confined). Native handle: LLVM value is a bare i8*, so it must NOT reach
		// dupHeapValue (which assumes a value struct and panics on a *PointerType).
		// Mirrors maybeDupPushElement (expr.go) and emitVariantFieldDrop's Arc branch.
		if arcElem, isArc := types.AsArc(typ); isArc || named == types.TypArc {
			if c.typeSubst != nil && arcElem != nil {
				arcElem = types.Substitute(arcElem, c.typeSubst)
			}
			dup := c.dupArc(fieldVal, arcElem)
			c.block.NewStore(dup, fieldPtr)
			return
		}
		// T1109: Weak[T] variant field — atomic weak-count increment.
		if weakElem, isWeak := types.AsWeak(typ); isWeak {
			if c.typeSubst != nil {
				weakElem = types.Substitute(weakElem, c.typeSubst)
			}
			dup := c.dupWeak(fieldVal, weakElem)
			c.block.NewStore(dup, fieldPtr)
			return
		}
		// T1113: Single-owner native handles (Mutex/MutexGuard/Task) have NO dup
		// semantics — the LLVM value is a bare i8* owning one allocation. A shallow
		// copy (the old T1109 no-op) aliases the container's slot, so moving the
		// read-back copy out and dropping it double-frees / UAFs the slot the
		// container still references. The reachable user paths (`container[k]!` of an
		// enum/struct nesting such a handle) are now rejected at the ownership pass
		// (rejectIndexExprSingleOwnerMove via FirstFieldNestedSingleOwnerHandle), and
		// .clone()/.filled() of handle-bearing aggregates by validateCloneType /
		// checkContainerNotCloneable. Panic backstop — see
		// emitSingleOwnerHandleDupPanic.
		if h := singleOwnerHandleName(typ, named); h != "" {
			c.emitSingleOwnerHandleDupPanic(h)
			return
		}
		// T1292: A non-value structural interface variant field is a heap-boxed view
		// ({vtable, instance}). A shallow copy would alias the box between two now-
		// droppable owners → double-free at drop. Deep-clone the box via
		// cloneStructuralView (T1284), mirroring maybeDupPushElement's structural arm.
		if named.IsStructural() && !named.IsValueType() {
			dup := c.cloneStructuralView(fieldVal)
			c.block.NewStore(dup, fieldPtr)
			return
		}
		if !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
			// Use cloneHeapElement to try clone() first (a named function that handles
			// recursive types safely), falling back to dupHeapValue only for non-recursive
			// types without clone. This prevents infinite codegen for recursive types
			// like Map[K, JsonValue] where JsonValue.Object contains Map[K, JsonValue].
			dup := c.cloneHeapElement(fieldVal, typ, named)
			c.block.NewStore(dup, fieldPtr)
			return
		}
		return
	}

	// T0741: Closure (function value) field. variantFieldNeedsDrop now returns
	// true for closures (for the drop path), so the dup/clone gate reaches here.
	// A closure env CANNOT be deep-cloned (the captured frame is opaque), and a
	// shallow copy would alias the source's env between two now-droppable owners
	// → double-free at drop (the leak→double-free hazard from this ticket's
	// CAUTION, surfacing via native container clone, e.g. Vector[EnumWithClosure]
	// .clone()). Null the cloned slot instead: the source keeps sole ownership of
	// its env (dropped exactly once), the clone holds an empty (uncallable)
	// closure. T0813 makes sema reject the explicit clone()/filled() repro
	// outright; this remains defense-in-depth for residual implicit dup paths
	// (polymorphic slice, vector element clone loop) that still reach here.
	if _, ok := typ.(*types.Signature); ok {
		c.block.NewStore(constant.NewZeroInitializer(fieldVal.Type()), fieldPtr)
		return
	}

	// Nested enum field: dup in place
	if extractEnum(typ) != nil {
		c.dupEnumElementInPlace(fieldPtr, typ)
		return
	}

	// T1099: Tuple variant field (e.g., enum Holder { Pair((Ref[int], int)) }).
	// emitVariantFieldDrop walks tuple elements (B0264), so the symmetric dup must
	// too — otherwise a tuple-bearing variant cloned in a container (Vector/Map
	// Slot) leaves the inner Ref/Channel/string shallow-copied → double-free at
	// drop. dupTupleValue deep-clones each droppable field; store the result back.
	if tup, isTup := typ.(*types.Tuple); isTup {
		if c.tupleNeedsDrop(typ) {
			dup := c.dupTupleValue(fieldVal, tup)
			c.block.NewStore(dup, fieldPtr)
		}
		return
	}
}
