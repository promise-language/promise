package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/types"
)

// aliasedMapHeapValue reports whether a Map value type V is a *droppable* heap
// user type whose value struct Map.[] returns ALIASED to the stored instance
// (rather than dup'd), AND whose direct drop actually frees the instance. The
// compound-assignment path uses this to skip the direct drop of `current`,
// because for these types both the direct drop and Map.[]='s overwrite-drop free
// the same instance → double free (T0987). Applies the active type/self
// substitutions so it works inside monomorphized bodies too.
//
// NOTE: this deliberately excludes the pal_free-only heap-user shape
// (isHeapUserNoDropPalFree). For those the direct drop is still required: it is
// the path that frees the aliased old instance, and it is alias-guarded so it
// does not double-free with Map.[]=. Suppressing it there leaks the old value
// (regression caught by TestT0987_PalFreeHeapMapCompoundStillDrops and
// pal_free_heap_compound).
func (c *Compiler) aliasedMapHeapValue(valType types.Type) bool {
	if valType == nil {
		return false
	}
	if c.typeSubst != nil {
		valType = types.Substitute(valType, c.typeSubst)
	}
	if c.selfSubst != nil {
		valType = types.SubstituteSelf(valType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	return isDroppableHeapUserType(valType)
}

// hasVectorStringBinding returns true if there's at least one Vector[string]
// binding in the current scope that would trigger element drops.
// B0189: Used to determine if a string return value needs duping.
func (c *Compiler) hasVectorStringBinding() bool {
	for _, b := range c.scopeBindings {
		if b.kind != bindingDropString {
			continue
		}
		valType := b.valType
		if c.typeSubst != nil {
			valType = types.Substitute(valType, c.typeSubst)
		}
		if elemType, isVec := types.AsVector(valType); isVec {
			resolvedElem := elemType
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
			}
			if extractNamed(resolvedElem) == types.TypString {
				return true
			}
		}
	}
	return false
}

// emitVectorElementDrops emits a loop that drops each element in a vector if the
// element type is droppable. Called before Vector.drop frees the buffer.
// B0189: Fixes memory leak where Vector[string] drop didn't free string elements.
func (c *Compiler) emitVectorElementDrops(b scopeBinding, vecPtr value.Value) {
	valType := b.valType
	if c.typeSubst != nil {
		valType = types.Substitute(valType, c.typeSubst)
	}
	elemType, isVec := types.AsVector(valType)
	if !isVec {
		return
	}
	c.emitVectorElementDropLoop(vecPtr, elemType)
}

// emitVectorElementDropLoop emits a loop that iterates vector elements and drops
// each one. Shared by scope-exit drops (emitVectorElementDrops) and field drops
// (emitFieldDrops). The elemType must already have type substitution applied.
// B0189: Fixes memory leak where Vector[string] drop didn't free string elements.
// B0212: Extended to also drop enum elements with synthesized drops.
func (c *Compiler) emitVectorElementDropLoop(vecPtr value.Value, elemType types.Type) {
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	// B0189: String elements are safe to drop (push dups them).
	// B0212: Enum elements stored by value in vectors — each element is an
	// independent copy of the enum internal type, so dropping each is safe.
	// B0245: Heap user type elements (non-value, non-copy, non-primitive, non-structural)
	// are also dropped via pal_free or their drop method. Vector elements are the sole
	// owner of user-type instances — constructors transfer ownership to push, and
	// sort temps clear their drop flags when moved back into the vector.
	// T0741: closure (function value) elements own a heap env struct that
	// emitVariantFieldDrop's Signature case frees.
	_, isSig := elemType.(*types.Signature)
	if extractNamed(elemType) != types.TypString && !isSig {
		if !c.vecElemNeedsEnumDrop(elemType) && !c.vecElemNeedsUserTypeDrop(elemType) && !c.tupleNeedsDrop(elemType) && !c.vecElemNeedsOptionalDrop(elemType) && !c.vecElemNeedsStructuralDrop(elemType) {
			return
		}
	}
	c.emitVectorElementDropLoopBody(vecPtr, elemType)
}

// emitVectorElementDropLoopBody is the shared implementation for vector element drop loops.
func (c *Compiler) emitVectorElementDropLoopBody(vecPtr value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)

	// Load vector length (masked — clears static flag bit 63)
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(vecPtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	// Data starts at offset vectorHeaderSize (16 bytes after buffer start)
	dataBase := c.block.NewGetElementPtr(irtypes.I8, vecPtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))

	// Loop: for i = 0; i < len; i++ { drop(elements[i]); }
	loopHead := c.newBlock("vecdrop.head")
	loopBody := c.newBlock("vecdrop.body")
	loopDone := c.newBlock("vecdrop.done")

	// Initialize counter
	idxAlloca := c.createEntryAlloca(irtypes.I64)
	idxAlloca.SetName(c.uniqueLocalName("vecdrop.idx"))
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)
	c.block.NewBr(loopHead)

	// Loop head: check i < len
	c.block = loopHead
	idx := c.block.NewLoad(irtypes.I64, idxAlloca)
	cond := c.block.NewICmp(enum.IPredULT, idx, length)
	c.block.NewCondBr(cond, loopBody, loopDone)

	// Loop body: drop element[i], increment i
	c.block = loopBody
	idx2 := c.block.NewLoad(irtypes.I64, idxAlloca)
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx2)
	elemVal := c.block.NewLoad(elemLLVM, elemPtr)

	c.emitVariantFieldDrop(elemVal, elemType)

	// Increment counter
	nextIdx := c.block.NewAdd(idx2, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(nextIdx, idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopDone
}

// emitVectorStringDupLoop iterates a vector's string elements and replaces each with
// a deep copy (dupString). Used by Vector.clone() to ensure the cloned vector owns
// independent copies of all string elements. T0154.
func (c *Compiler) emitVectorStringDupLoop(vecPtr value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)

	// Load vector length (masked — clears static flag bit 63)
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(vecPtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	// Data starts at offset vectorHeaderSize (16 bytes after buffer start)
	dataBase := c.block.NewGetElementPtr(irtypes.I8, vecPtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))

	// Loop: for i = 0; i < len; i++ { elements[i] = dupString(elements[i]); }
	loopHead := c.newBlock("vecdup_str.head")
	loopBody := c.newBlock("vecdup_str.body")
	loopDone := c.newBlock("vecdup_str.done")

	idxAlloca := c.createEntryAlloca(irtypes.I64)
	idxAlloca.SetName(c.uniqueLocalName("vecdup_str.idx"))
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopHead
	idx := c.block.NewLoad(irtypes.I64, idxAlloca)
	cond := c.block.NewICmp(enum.IPredULT, idx, length)
	c.block.NewCondBr(cond, loopBody, loopDone)

	c.block = loopBody
	idx2 := c.block.NewLoad(irtypes.I64, idxAlloca)
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx2)
	elemVal := c.block.NewLoad(elemLLVM, elemPtr)
	duped := c.dupString(elemVal)
	c.block.NewStore(duped, elemPtr)

	nextIdx := c.block.NewAdd(idx2, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(nextIdx, idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopDone
}

// emitVectorOptionalDupLoop iterates a cloned vector's Optional[droppable]
// elements and deep-clones each present inner via dupOptionalVectorElem, so the
// cloned vector owns independent inner allocations instead of aliasing the
// source's heap boxes (which the now-active element drop loop would double-free /
// UAF). dupOptionalVectorElem builds its own present/absent split, so the store
// and loop back-edge use c.block after the call (its merge block). T1291.
func (c *Compiler) emitVectorOptionalDupLoop(vecPtr value.Value, elemType types.Type, opt *types.Optional, inner types.Type) {
	elemLLVM := c.resolveType(elemType)

	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(vecPtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	dataBase := c.block.NewGetElementPtr(irtypes.I8, vecPtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))

	loopHead := c.newBlock("vecdup_opt.head")
	loopBody := c.newBlock("vecdup_opt.body")
	loopDone := c.newBlock("vecdup_opt.done")

	idxAlloca := c.createEntryAlloca(irtypes.I64)
	idxAlloca.SetName(c.uniqueLocalName("vecdup_opt.idx"))
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopHead
	idx := c.block.NewLoad(irtypes.I64, idxAlloca)
	cond := c.block.NewICmp(enum.IPredULT, idx, length)
	c.block.NewCondBr(cond, loopBody, loopDone)

	c.block = loopBody
	idx2 := c.block.NewLoad(irtypes.I64, idxAlloca)
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx2)
	elemVal := c.block.NewLoad(elemLLVM, elemPtr)
	duped := c.dupOptionalVectorElem(elemVal, opt, inner)
	// dupOptionalVectorElem advances c.block to its merge block; store and loop
	// back-edge must be emitted there. loopBody dominates that merge (it's the entry
	// to the present/absent diamond), so elemPtr (a loopBody GEP) and idxAlloca (an
	// entry-block alloca) both still dominate — valid across the split.
	c.block.NewStore(duped, elemPtr)
	nextIdx := c.block.NewAdd(idx2, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(nextIdx, idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopDone
}

// emitVectorClosureNullLoop iterates a cloned vector's bare closure elements
// (Vector[() -> int]) and nulls each {fn, env} fat-pointer slot. T1045: a closure
// env CANNOT be deep-cloned (the captured frame is opaque), and dupVector's shallow
// memcpy would alias the source's env between two now-droppable owners →
// double-free at drop (emitVectorElementDropLoop frees each element's env). Nulling
// the cloned slot keeps the source as sole owner (dropped exactly once); the clone
// holds an empty (uncallable) closure. Symmetric with emitVariantFieldDup's
// Signature case for struct-field / enum-variant null-dup.
func (c *Compiler) emitVectorClosureNullLoop(vecPtr value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)

	// Load vector length (masked — clears static flag bit 63)
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(vecPtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	// Data starts at offset vectorHeaderSize (16 bytes after buffer start)
	dataBase := c.block.NewGetElementPtr(irtypes.I8, vecPtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))

	// Loop: for i = 0; i < len; i++ { elements[i] = {null, null}; }
	loopHead := c.newBlock("vecclonenull.head")
	loopBody := c.newBlock("vecclonenull.body")
	loopDone := c.newBlock("vecclonenull.done")

	idxAlloca := c.createEntryAlloca(irtypes.I64)
	idxAlloca.SetName(c.uniqueLocalName("vecclonenull.idx"))
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopHead
	idx := c.block.NewLoad(irtypes.I64, idxAlloca)
	cond := c.block.NewICmp(enum.IPredULT, idx, length)
	c.block.NewCondBr(cond, loopBody, loopDone)

	c.block = loopBody
	idx2 := c.block.NewLoad(irtypes.I64, idxAlloca)
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx2)
	c.block.NewStore(constant.NewZeroInitializer(elemLLVM), elemPtr)

	nextIdx := c.block.NewAdd(idx2, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(nextIdx, idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopDone
}

// emitVectorElementCloneLoopNullable runs emitVectorElementCloneLoop only when
// vecPtr is non-null. T0939: an Optional[Vector] field read on the `none` path
// yields a null inner buffer (field 1 of a zero-initialized optional); the dup of
// that null is null, and emitVectorElementCloneLoop dereferences vecPtr
// (loadVectorLen) unconditionally → segfault. Callers that pass a possibly-null
// vector (the Optional[Vector] inner-buffer dup paths) must use this wrapper.
func (c *Compiler) emitVectorElementCloneLoopNullable(vecPtr value.Value, elemType types.Type) {
	entryBlock := c.block
	isNull := entryBlock.NewICmp(enum.IPredEQ, vecPtr, constant.NewNull(irtypes.I8Ptr))
	cloneBlock := c.newBlock("veccloneopt.do")
	mergeBlock := c.newBlock("veccloneopt.merge")
	entryBlock.NewCondBr(isNull, mergeBlock, cloneBlock)

	c.block = cloneBlock
	c.emitVectorElementCloneLoop(vecPtr, elemType)
	c.block.NewBr(mergeBlock)

	c.block = mergeBlock
}

// emitVectorElementCloneLoop iterates a cloned vector's elements and deep-clones
// each non-copy element so the cloned vector owns independent copies. B0275.
// Handles: strings (dupString), channels (dupChannel), nested vectors (dupVector +
// recursive clone), heap user types (clone method or dupHeapValue fallback),
// enum types with clone methods (B0244), and droppable enums without clone (B0290).
func (c *Compiler) emitVectorElementCloneLoop(vecPtr value.Value, elemType types.Type) {
	named := extractNamed(elemType)
	// B0244: Check for enum types with clone — not caught by extractNamed.
	// B0290: Also detect droppable enums without clone (e.g., Slot[K,V] in Map).
	isCloneableEnum := false
	isDupableEnum := false
	// T1099: Tuple element with droppable fields (e.g., (Ref[int], int)).
	// extractNamed/extractEnum are both nil for tuples, so without this the
	// memcpy'd element was left shallow — the Ref inside the tuple was never
	// refcount-incremented → double-free/UAF at scope exit.
	tup, isTuple := elemType.(*types.Tuple)
	isDupableTuple := isTuple && c.tupleNeedsDrop(elemType)
	if named == nil {
		// T1045: a direct/bare closure element (Vector[() -> int]) is a
		// *types.Signature — extractNamed/extractEnum are both nil, so without
		// this branch the loop early-returns with dupVector's shallow memcpy
		// intact, aliasing each closure's heap env across both vectors →
		// double-free at drop (emitVectorElementDropLoop frees each env).
		// Symmetric with emitVariantFieldDup's Signature case (struct-field /
		// enum-variant null-dup): null each cloned {fn,env} slot so the source
		// keeps sole ownership of the env, the clone holds an empty closure.
		if _, isSig := elemType.(*types.Signature); isSig {
			c.emitVectorClosureNullLoop(vecPtr, elemType)
			return
		}
		// T1291: Optional[droppable] element — extractNamed/extractEnum are both
		// nil for an Optional, so without this the loop early-returns leaving the
		// shallow memcpy intact, aliasing each present inner's heap allocation
		// across both vectors → double-free / UAF once the (now-active) element
		// drop loop frees each. Deep-clone the present inner via
		// dupOptionalVectorElem (present/absent split + per-inner dispatch, incl.
		// the structural case added for T1291). Covers Optional[structural] (the
		// T1291 crash) plus pre-existing Optional[string]/[vector]/[heap-user].
		if opt, inner, ok := c.optionalPushElemNeedsDup(elemType); ok {
			c.emitVectorOptionalDupLoop(vecPtr, elemType, opt, inner)
			return
		}
		if enum := extractEnum(elemType); enum != nil {
			_, isCloneableEnum = c.funcs[c.enumCloneFuncName(enum, elemType)]
			if !isCloneableEnum {
				isDupableEnum = c.vecElemNeedsEnumDrop(elemType)
			}
		}
		if !isCloneableEnum && !isDupableEnum && !isDupableTuple {
			return // primitive/copy type — shallow memcpy is correct
		}
	}

	// String: delegate to existing string dup loop
	if named == types.TypString {
		c.emitVectorStringDupLoop(vecPtr, elemType)
		return
	}

	// T0559 + T0545: single-owner native handles (Task/Mutex/MutexGuard) are
	// move-only i8* handles with no clone semantics. T0545's sema gate rejects
	// clone()/filled()/nesting on containers transitively containing them, so
	// well-formed user code never reaches here. This is the codegen backstop
	// for the residual generic-indirection path (T0616) — sema checks generic
	// bodies with unbound T, so dup[T](Vector[T]) instantiated with T=Task can
	// still reach this. Emit a length-guarded runtime panic (T0559) rather
	// than a silent shallow-copy: empty vectors clone trivially (no
	// double-ownership), non-empty would double-free at drop, so panic with a
	// type-specific message instead of falling through to dupHeapValue (which
	// Go-panics on the i8* → StructType cast).
	unclonableTypeName := ""
	if _, isFTask := types.AsFailableTask(elemType); isFTask || named == types.TypFailableTask {
		unclonableTypeName = "FailableTask"
	} else if _, isTask := types.AsTask(elemType); isTask || named == types.TypTask {
		unclonableTypeName = "Task"
	} else if _, isMutex := types.AsMutex(elemType); isMutex || named == types.TypMutex {
		unclonableTypeName = "Mutex"
	} else if _, isMG := types.AsMutexGuard(elemType); isMG || named == types.TypMutexGuard {
		unclonableTypeName = "MutexGuard"
	}
	if unclonableTypeName != "" {
		headerType := vectorHeaderType()
		headerPtr := c.block.NewBitCast(vecPtr, irtypes.NewPointer(headerType))
		length := loadVectorLen(c.block, headerPtr)
		isEmpty := c.block.NewICmp(enum.IPredEQ, length, constant.NewInt(irtypes.I64, 0))
		okBlock := c.newBlock("vecclone.unsup.ok")
		panicBlock := c.newBlock("vecclone.unsup.panic")
		c.block.NewCondBr(isEmpty, okBlock, panicBlock)

		c.block = panicBlock
		panicMsg := c.makeGlobalString(fmt.Sprintf(
			"Vector[%s[T]].clone() is not supported; %s is move-only",
			unclonableTypeName, unclonableTypeName))
		c.block.NewCall(c.funcs["promise_panic"], panicMsg)
		c.emitPanicReturn()

		c.block = okBlock
		return
	}

	// Determine if element type needs cloning
	_, isCh := types.AsChannel(elemType)
	innerElem, isVec := types.AsVector(elemType)
	arcElem, isArc := types.AsArc(elemType)
	weakElem, isWk := types.AsWeak(elemType)
	isChannel := !isCloneableEnum && !isDupableEnum && (isCh || named == types.TypChannel)
	isVector := !isCloneableEnum && !isDupableEnum && (isVec || named == types.TypVector)
	isArcType := !isCloneableEnum && !isDupableEnum && (isArc || named == types.TypArc)
	isWeakType := !isCloneableEnum && !isDupableEnum && (isWk || named == types.TypWeak)
	isHeapUser := !isCloneableEnum && !isDupableEnum && named != nil && !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural()
	// T1284: structural-interface element — its {vtable, instance} view boxes a
	// heap instance; deep-clone the instance through __promise_structural_clone
	// (RTTI dispatch) so the cloned vector owns an independent box. Without this
	// the shallow memcpy aliases the box across both vectors → double-free once
	// the element drop loop (now structural-aware) frees each.
	isStructuralElem := isStructuralView(named)

	if !isChannel && !isVector && !isArcType && !isWeakType && !isHeapUser && !isStructuralElem && !isCloneableEnum && !isDupableEnum && !isDupableTuple {
		return // value/copy type — shallow memcpy is correct
	}

	// Emit loop: for i = 0; i < len; i++ { elements[i] = clone(elements[i]); }
	elemLLVM := c.resolveType(elemType)

	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(vecPtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	dataBase := c.block.NewGetElementPtr(irtypes.I8, vecPtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))

	loopHead := c.newBlock("vecclone.head")
	loopBody := c.newBlock("vecclone.body")
	loopDone := c.newBlock("vecclone.done")

	idxAlloca := c.createEntryAlloca(irtypes.I64)
	idxAlloca.SetName(c.uniqueLocalName("vecclone.idx"))
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopHead
	idx := c.block.NewLoad(irtypes.I64, idxAlloca)
	cond := c.block.NewICmp(enum.IPredULT, idx, length)
	c.block.NewCondBr(cond, loopBody, loopDone)

	c.block = loopBody
	idx2 := c.block.NewLoad(irtypes.I64, idxAlloca)
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx2)

	if isDupableEnum {
		// B0290: Droppable enum without clone — dup variant fields in place.
		c.dupEnumElementInPlace(elemPtr, elemType)
	} else if isDupableTuple {
		// T1099: Tuple element — deep-clone each droppable field via the shared
		// dupTupleValue helper (handles Ref/Channel/Weak/string/Vector/heap/enum/
		// nested-tuple per field, leaving Copy fields as the memcpy'd value).
		elemVal := c.block.NewLoad(elemLLVM, elemPtr)
		cloned := c.dupTupleValue(elemVal, tup)
		c.block.NewStore(cloned, elemPtr)
	} else {
		elemVal := c.block.NewLoad(elemLLVM, elemPtr)

		var cloned value.Value
		if isCloneableEnum {
			// B0244: Enum with clone — deep-copy via clone method.
			cloned, _ = c.cloneEnumValue(elemVal, elemType)
		} else if isChannel {
			cloned = c.dupChannel(elemVal)
		} else if isArcType {
			resolvedArcElem := arcElem
			if c.typeSubst != nil && resolvedArcElem != nil {
				resolvedArcElem = types.Substitute(resolvedArcElem, c.typeSubst)
			}
			cloned = c.dupArc(elemVal, resolvedArcElem)
		} else if isWeakType {
			resolvedWeakElem := weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
			cloned = c.dupWeak(elemVal, resolvedWeakElem)
		} else if isVector {
			if isVec {
				innerLLVM := c.resolveType(innerElem)
				innerSize := int64(c.typeSize(innerLLVM))
				cloned = c.dupVector(elemVal, innerSize)
				// Recursively clone inner vector's elements
				c.emitVectorElementCloneLoop(cloned, innerElem)
			} else {
				cloned = c.dupVector(elemVal, 0)
			}
		} else if isStructuralElem {
			// T1284: deep-clone the boxed instance via RTTI clone dispatch, then
			// rebuild the {vtable, instance} view with the same view vtable and the
			// freshly-cloned instance pointer.
			cloned = c.cloneStructuralView(elemVal)
		} else {
			// Heap user type: try clone() method, fall back to dupHeapValue
			cloned = c.cloneHeapElement(elemVal, elemType, named)
		}
		c.block.NewStore(cloned, elemPtr)
	}

	nextIdx := c.block.NewAdd(idx2, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(nextIdx, idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopDone
}

// cloneHeapElement clones a single heap user type element by calling its clone()
// method if available, otherwise falling back to dupHeapValue. B0275.
func (c *Compiler) cloneHeapElement(elemVal value.Value, elemType types.Type, named *types.Named) value.Value {
	// T0545 backstop: single-owner native handles have no clone semantics and
	// are i8* (not the {vtable,instance} struct dupHeapValue expects). Return
	// the handle unchanged rather than asserting/panicking.
	if isSingleOwnerHandleType(elemType) {
		return elemVal
	}
	// Resolve clone method name
	ownerName := c.resolveMethodOwner(named, "clone")
	if inst, ok := elemType.(*types.Instance); ok {
		ownerName = monoName(inst)
	}
	mangledName := mangleMethodName(ownerName, "clone", false)
	if cloneFn, ok := c.funcs[mangledName]; ok {
		// B0289: For generic instances, verify all type arguments can be safely
		// handled by the clone's internal match-dup. Container clone methods (Map, Set)
		// iterate elements via match destructure — if any type argument can't be
		// safely match-dup'd, the clone would be shallow → fall back to dupHeapValue.
		if inst, ok := elemType.(*types.Instance); ok {
			for _, arg := range inst.TypeArgs() {
				if c.typeSubst != nil {
					arg = types.Substitute(arg, c.typeSubst)
				}
				if !c.typeArgSafeForCloneDup(arg) {
					return c.dupHeapValue(elemVal, elemType)
				}
			}
		}
		instance := c.extractInstancePtr(elemVal)
		return c.block.NewCall(cloneFn, instance)
	}

	// No clone method — fall back to dupHeapValue (alloc + memcpy + sub-field dup,
	// which is already null-safe internally).
	return c.dupHeapValue(elemVal, elemType)
}

// vecElemNeedsEnumDrop returns true if a vector element type is an enum that has
// a drop function available. Checks both sema-time HasDrop (non-generic enums) and
// codegen-time mono synth drops (generic enum instances like Slot[string, JsonValue]).
// B0212: Enables vector element drop loop to clean up enum elements.
func (c *Compiler) vecElemNeedsEnumDrop(elemType types.Type) bool {
	enum := extractEnum(elemType)
	if enum == nil {
		return false
	}
	// Non-generic enum with sema-detected drop
	if enum.HasDrop() {
		return true
	}
	// Generic enum instance — check if the mono drop function was generated
	if inst, ok := elemType.(*types.Instance); ok {
		mangledName := mangleMethodName(monoName(inst), "drop", false)
		if _, ok := c.funcs[mangledName]; ok {
			return true
		}
	}
	return false
}

// vecElemNeedsUserTypeDrop returns true if a vector element type is a heap user type
// that needs drop or pal_free. Covers: types with explicit/synthesized drops, mono
// instances with codegen-time drops, and plain heap user types (non-value, non-copy,
// non-primitive, non-structural) that need pal_free.
// B0245: Enables vector element drop loop to clean up user-type elements.
func (c *Compiler) vecElemNeedsUserTypeDrop(elemType types.Type) bool {
	named := extractNamed(elemType)
	if named == nil {
		return false
	}
	// Types with explicit or synthesized drop
	if named.HasDrop() || named.NeedsSynthDrop() {
		return true
	}
	// Mono instance with codegen-time synthesized drop
	if inst, ok := elemType.(*types.Instance); ok {
		if n, ok2 := inst.Origin().(*types.Named); ok2 && n.NeedsSynthDrop() {
			return true
		}
		mangledName := mangleMethodName(monoName(inst), "drop", false)
		if _, ok := c.funcs[mangledName]; ok {
			return true
		}
	}
	// Heap user type without any drop — needs pal_free
	if !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
		return true
	}
	return false
}

// cloneStructuralView deep-clones a structural-interface element value (a
// {vtable, instance} fat view): it clones the boxed heap instance through
// __promise_structural_clone (RTTI dispatch on the instance typeinfo's
// clone_fn_ptr) and rebuilds the view with the same view vtable and the fresh
// instance pointer. Shared by the vector element clone loop and the push-element
// dup path so every structural-vector duplication owns an independent box. T1284.
func (c *Compiler) cloneStructuralView(view value.Value) value.Value {
	instancePtr := c.extractInstancePtr(view)
	clonedInst := c.block.NewCall(c.structuralClone, instancePtr)
	vtablePtr := c.extractVtablePtr(view)
	valType := view.Type().(*irtypes.StructType)
	tmp := c.block.NewInsertValue(constant.NewZeroInitializer(valType), vtablePtr, 0)
	return c.block.NewInsertValue(tmp, clonedInst, 1)
}

// isNonValueStructuralType reports whether t resolves to a non-value structural
// interface. Its runtime value is a fat view {vtable, instance} whose instance is
// a heap box (heap user instance / boxed value type / heap string box) that must
// be deep-cloned on read (__promise_structural_clone) and dropped via RTTI
// (__promise_structural_drop). Shared by the vector element drop gate and the
// Optional[structural] dup-on-read gates. T1284/T1291.
//
// The types.Type-taking spelling of isStructuralView (rtti.go), which owns the
// reasoning; kept as one predicate so the two can never drift apart. T1550.
func isNonValueStructuralType(t types.Type) bool {
	return isStructuralView(extractNamed(t))
}

// vecElemNeedsStructuralDrop returns true if a vector element type is a
// non-value-type structural interface. Its runtime value is a fat view
// {vtable, instance} whose instance is a heap box (heap user instance / heap
// string box); the element drop loop must route it through
// __promise_structural_drop (RTTI: typeinfo.drop_fn_ptr → concrete drop, else
// pal_free) rather than treating the view as trivially droppable. T1284.
func (c *Compiler) vecElemNeedsStructuralDrop(elemType types.Type) bool {
	return isNonValueStructuralType(elemType)
}

// vecElemNeedsOptionalDrop returns true if a vector element type is Optional[T]
// where T is a droppable type. Enables emitVectorElementDropLoop to walk Optional
// elements and drop their inner payloads via emitOptionalValueDrop.
// T0620: Closes Gap B — without this, Vector[T?] drop skips inner payload drops.
func (c *Compiler) vecElemNeedsOptionalDrop(elemType types.Type) bool {
	opt, ok := elemType.(*types.Optional)
	if !ok {
		return false
	}
	inner := opt.Elem()
	if c.typeSubst != nil {
		inner = types.Substitute(inner, c.typeSubst)
	}
	return c.typeNeedsFieldDrop(inner)
}

// arrayFieldNeedsDrop returns true if a fixed-size array type has a droppable
// element type. Used by emitFieldDropsFor to skip non-droppable arrays (e.g.
// int[3]) instead of emitting an empty per-element loop. (T0579)
func (c *Compiler) arrayFieldNeedsDrop(arr *types.Array) bool {
	elem := arr.Elem()
	if c.typeSubst != nil {
		elem = types.Substitute(elem, c.typeSubst)
	}
	return c.typeNeedsFieldDrop(elem)
}

// typeNeedsFieldDrop returns true if a single value of typ has any drop work
// (string, vector, channel, heap user type, droppable tuple/array, Optional
// wrapping a droppable inner, enum with drop, etc.). Used by tuple/array field
// drop predicates. (T0579)
func (c *Compiler) typeNeedsFieldDrop(typ types.Type) bool {
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	if tup, ok := typ.(*types.Tuple); ok {
		return c.tupleNeedsDrop(tup)
	}
	if arr, ok := typ.(*types.Array); ok {
		return c.arrayFieldNeedsDrop(arr)
	}
	if opt, ok := typ.(*types.Optional); ok {
		return c.typeNeedsFieldDrop(opt.Elem())
	}
	// T0741: Closure fields own a heap env struct that must be deep-dropped.
	// Covers tuple/array struct fields carrying a closure via tupleNeedsDrop /
	// arrayFieldNeedsDrop.
	if _, ok := typ.(*types.Signature); ok {
		return true
	}
	if named := extractNamed(typ); named != nil {
		if named == types.TypString || named.HasDrop() || named.NeedsSynthDrop() {
			return true
		}
		if _, isVec := types.AsVector(typ); isVec {
			return true
		}
		if _, isCh := types.AsChannel(typ); isCh {
			return true
		}
		if _, isArc := types.AsArc(typ); isArc {
			return true
		}
		if _, isWeak := types.AsWeak(typ); isWeak {
			return true
		}
		if _, isMutex := types.AsMutex(typ); isMutex {
			return true
		}
		if types.IsMutexGuard(typ) || named == types.TypMutexGuard {
			return true
		}
		// T1292: A non-value structural interface field is a heap-boxed view whose
		// instance box must be dropped via __promise_structural_drop. Classify it as
		// droppable so a tuple/array carrying a structural field (e.g. the
		// `(K, Showable)` elements of `Map.entries()`'s result vector) drops each box.
		// T1291: same routing also covers Optional[structural] vector elements (via
		// vecElemNeedsOptionalDrop → here); without it those boxed instances leak.
		if isStructuralView(named) {
			return true
		}
		if !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
			return true
		}
	}
	if c.vecElemNeedsEnumDrop(typ) {
		return true
	}
	return false
}

// tupleNeedsDrop returns true if a tuple type contains any droppable element
// (string, vector, channel, user type with drop, enum with drop, droppable
// Optional/Array, or another droppable tuple).
// B0264: Enables vector element drop loop to clean up tuple elements.
// T0371: Recurses into nested tuples so e.g. ((int, string), int) is droppable.
// T0578: Delegates per-element check to typeNeedsFieldDrop so Optional, Array,
// Mutex, and MutexGuard tuple elements are recognized as droppable.
func (c *Compiler) tupleNeedsDrop(elemType types.Type) bool {
	tup, ok := elemType.(*types.Tuple)
	if !ok {
		return false
	}
	for _, e := range tup.Elems() {
		resolved := e
		if c.typeSubst != nil {
			resolved = types.Substitute(resolved, c.typeSubst)
		}
		if c.typeNeedsFieldDrop(resolved) {
			return true
		}
	}
	return false
}

// isMapOrSetType reports whether typ is the standard-library Map[K,V] or Set[T]
// heap container. These are heap user types deliberately excluded from
// isDroppableHeapUserType / isHeapUserNoDropPalFree (T0440), so callers that
// want to treat them as ordinary heap user types — e.g. the T0732 spawn-side
// deep dup via dupHeapValue — must recognize them explicitly.
func isMapOrSetType(typ types.Type) bool {
	named := extractNamed(typ)
	if named == nil {
		return false
	}
	if named == types.TypMap {
		return true
	}
	if named.Obj() != nil && named.Obj().Name() == "Set" {
		return true
	}
	return false
}

// dupTupleValue creates a deep copy of a tuple value by dup'ing each droppable
// field (strings, vectors, channels, nested tuples, heap user types, enums).
// Non-droppable fields (primitives, value types) are copied by struct value.
// Used when reading a tuple from a container (`t := v[0]`) so the result is
// independently owned and can be safely dropped without affecting the
// container's element. Symmetric with the Vector[string] dup-on-read pattern
// (B0204) and the Vector[user heap type] cloneHeapElement pattern (B0275).
// T0370.
func (c *Compiler) dupTupleValue(tupVal value.Value, tup *types.Tuple) value.Value {
	result := tupVal
	for i, fieldType := range tup.Elems() {
		resolved := fieldType
		if c.typeSubst != nil {
			resolved = types.Substitute(resolved, c.typeSubst)
		}
		elemVal := c.block.NewExtractValue(result, uint64(i))
		var dupped value.Value
		if innerTup, isTup := resolved.(*types.Tuple); isTup {
			if c.tupleNeedsDrop(resolved) {
				dupped = c.dupTupleValue(elemVal, innerTup)
			}
		} else if extractNamed(resolved) == types.TypString && !isRefType(resolved) {
			dupped = c.dupString(elemVal)
		} else {
			// Vectors, channels, heap user types, droppable enums: delegate.
			dupped = c.maybeDupPushElement(elemVal, resolved)
		}
		if dupped != nil {
			result = c.block.NewInsertValue(result, dupped, uint64(i))
		}
	}
	return result
}
