package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- Index assignment ---

// genIndexAssign handles assignment to a container element: arr[i] = val, m[k] = val.
func (c *Compiler) genIndexAssign(target *ast.IndexExpr, op ast.AssignOp, val value.Value, srcExpr ast.Expr) {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	// Unwrap MutRef/SharedRef for index assignment (auto-deref through borrows)
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
	}

	// Fixed-size array index assignment
	if arr, ok := targetType.(*types.Array); ok {
		c.genArrayIndexAssign(target, arr, op, val)
		return
	}

	named := extractNamed(targetType)
	if named != nil {
		if m := named.LookupMethod("[]="); m != nil {
			if m.IsNative() {
				c.genNativeIndexAssign(target, targetType, op, val, srcExpr)
				return
			}
			c.genMethodIndexAssign(target, targetType, val)
			return
		}
	}
	panic(fmt.Sprintf("codegen: cannot assign to index of type %s", targetType))
}

// genArrayIndexAssign handles arr[i] = val for fixed-size arrays with bounds checking.
func (c *Compiler) genArrayIndexAssign(target *ast.IndexExpr, arr *types.Array, op ast.AssignOp, val value.Value) {
	basePtr := c.genArrayBasePtr(target.Target, arr)
	idx := c.genExpr(target.Index)
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)

	// Bounds check
	size := constant.NewInt(irtypes.I64, arr.Size())
	inBounds := c.block.NewICmp(enum.IPredULT, idx, size)
	okBlock := c.newBlock("arrassign.ok")
	panicBlock := c.newBlock("arrassign.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	c.block = panicBlock
	oobMsg := c.makeGlobalString("array index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.emitPanicReturn()

	c.block = okBlock
	elemPtr := c.block.NewGetElementPtr(arrType, basePtr,
		constant.NewInt(irtypes.I32, 0), idx)

	elemType := arr.Elem()
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}

	if op == ast.OpAssign {
		// T0583: Drop the previous element before storing the new value. Without
		// this, overwriting a droppable element (string, Vector, Channel, Arc,
		// heap user, Optional<droppable>, droppable tuple/nested array) leaks
		// the previous allocation. Mirrors genVectorIndexAssign and
		// genMemberAssign drop-on-overwrite patterns.
		if c.typeNeedsFieldDrop(elemType) {
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		}
		c.block.NewStore(val, elemPtr)
		return
	}

	// Compound assignment
	current := c.block.NewLoad(elemLLVM, elemPtr)
	result := c.genCompoundOp(op, arr.Elem(), current, val)
	// T0583: Drop old string before storing the concat result. Numeric/bool/char
	// compound ops produce values, not new allocations — only string applies.
	if extractNamed(elemType) == types.TypString {
		c.emitStringDropOldValue(current, result)
	} else {
		// T0715: a non-native operator on a heap user type (or droppable enum)
		// returns a FRESH value, so the old element leaks unless dropped. The
		// alias-guarded helper is a no-op for value types/scalars.
		c.dropOldUserValueAtPtr(elemPtr, elemType, result)
	}
	c.block.NewStore(result, elemPtr)
}

// genNativeIndexAssign dispatches native []= implementations for built-in types.
func (c *Compiler) genNativeIndexAssign(target *ast.IndexExpr, targetType types.Type, op ast.AssignOp, val value.Value, srcExpr ast.Expr) {
	if elem, ok := types.AsVector(targetType); ok {
		c.genVectorIndexAssign(target, elem, op, val, srcExpr)
		return
	}
	// Inside monomorphized method body: targetType is Named(Vector) not Instance(Vector[T]).
	named := extractNamed(targetType)
	if named == types.TypVector && c.typeSubst != nil {
		tp := named.TypeParams()[0]
		if elem, ok := c.typeSubst[tp]; ok {
			c.genVectorIndexAssign(target, elem, op, val, srcExpr)
			return
		}
	}
	panic(fmt.Sprintf("codegen: no native []= implementation for type %s", targetType))
}

// genMethodIndexAssign calls the monomorphized []= method on a user type.
func (c *Compiler) genMethodIndexAssign(target *ast.IndexExpr, targetType types.Type, val value.Value) {
	mangledName := mangleMethodName(c.resolveTypeName(targetType), "[]=", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared []= method %s", mangledName))
	}

	targetVal := c.genExpr(target.Target)
	keyVal := c.genExpr(target.Index)

	// B0343: Map []= takes ~K key (move). Handle string key ownership so the
	// map holds an independent copy. If the key source has a drop flag, clear
	// it (ownership transferred). If no drop flag (borrow param), dup so the
	// map doesn't hold a pointer that the caller will free.
	if keyType, _, isMap := types.AsMap(targetType); isMap {
		resolvedKey := keyType
		if c.typeSubst != nil {
			resolvedKey = types.Substitute(resolvedKey, c.typeSubst)
		}
		if extractNamed(resolvedKey) == types.TypString {
			if ident, ok := target.Index.(*ast.IdentExpr); ok {
				if _, hasFlag := c.dropFlags[ident.Name]; hasFlag {
					c.clearDropFlag(ident.Name)
				} else {
					keyVal = c.dupString(keyVal)
				}
			} else if isStringBorrowExpr(target.Index) {
				// B0355: non-ident borrow expr (field access, container element) as map key —
				// the source still owns the pointer; dup so map holds an independent copy.
				keyVal = c.dupString(keyVal)
			}
		}
	}

	var instancePtr value.Value
	switch {
	case isThisReceiver(target.Target):
		// T0745: `this` (incl. paren-wrapped) is already the i8* receiver ptr.
		instancePtr = targetVal
	case isContainerType(targetType):
		instancePtr = targetVal
	default:
		instancePtr = c.extractInstancePtr(targetVal)
	}

	call := c.block.NewCall(fn, instancePtr, keyVal, val)
	c.propagateIfFailable(call) // T0708
	// B0232: Claim string/heap temps for the key — ownership transfers to the []= method.
	// Without this, temporary keys (e.g., "a".repeat(2)) are freed at statement end
	// while still stored in the container, causing dangling pointers.
	c.claimStringTemp(keyVal)
	c.claimHeapTemp(keyVal)
}

// genVectorIndexAssign handles vec[i] = val with bounds check.
func (c *Compiler) genVectorIndexAssign(target *ast.IndexExpr, elemType types.Type, op ast.AssignOp, val value.Value, srcExpr ast.Expr) {
	slicePtr := c.genExpr(target.Target)
	idx := c.genExpr(target.Index)
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	// COW: if static (.rodata), copy to heap first (T0062)
	cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
		slicePtr, constant.NewInt(irtypes.I64, elemSize))
	c.storeBackSlicePtr(target.Target, cowSlice)

	// Bounds check (masked len)
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(cowSlice, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	c.emitIndexBoundsCheck(idx, length, "indexassign", "index out of bounds")
	dataBase := c.block.NewGetElementPtr(irtypes.I8, cowSlice,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)

	if op == ast.OpAssign {
		// B0195: New value is dup'd at the call site (like push, B0189) so the
		// vector owns an independent copy.
		// B0204: Drop old string element before storing new value. This is safe
		// because dup-on-read (B0204 in genVectorIndex) ensures any local variable
		// that captured the old value via vec[i] owns an independent copy.
		if extractNamed(elemType) == types.TypString {
			if dropFn, ok := c.funcs["promise_string_drop"]; ok {
				oldVal := c.block.NewLoad(elemLLVM, elemPtr)
				c.block.NewCall(dropFn, oldVal)
			}
		} else if c.vecElemNeedsEnumDrop(elemType) {
			// B0235: Drop old enum element before overwriting. Enum elements are
			// stored by value in vector buffers, so each element is an independent
			// copy. emitVariantFieldDrop allocas the old value, bitcasts to i8*,
			// and calls the synthesized enum drop function.
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		} else if types.IsVector(elemType) || types.IsChannel(elemType) ||
			types.IsArc(elemType) || types.IsWeak(elemType) {
			// T0383: Drop old element before overwriting for nested heap container
			// types (Vector, Channel, Arc, Weak). Without this, overwriting via
			// vec[i] = newVal leaks the old element. Safe because genVectorIndex
			// dups these on read (T0383 dup-on-read), so any aliased local owns
			// an independent copy. Mirrors the Vector[string] B0204 pattern.
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		} else if isDroppableHeapUserType(elemType) || isHeapUserNoDropPalFree(elemType) {
			// T0398: Drop old heap user-type element before overwriting. Without this,
			// `vec[i] = X` leaks vec[i]'s previous instance. Safe because dup-on-read
			// (T0398 in genVectorIndex, set above in genAssignStmt) ensures any RHS
			// vec reads return independent clones — no live alias to the freed instance.
			// T0908: also cover heap user types with NO drop (isHeapUserNoDropPalFree);
			// emitVariantFieldDrop's B0218 branch pal_frees the old no-drop instance.
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		} else if c.tupleNeedsDrop(elemType) {
			// T0412: Drop old tuple element before overwriting. Without this,
			// `vec[i] = X` for Vector[(droppable, ...)] leaks vec[i]'s previous
			// tuple's heap fields (vector buffers, strings, channels, nested
			// user types). Safe because the dup-on-read flag set in genAssignStmt
			// ensures vec-to-vec writes produce independent clones — no live
			// alias to the freed tuple's fields. emitVariantFieldDrop's tuple
			// branch walks each element via ExtractValue + recursive drop.
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		} else if c.vecElemNeedsOptionalDrop(elemType) {
			// T0620: Drop old Optional[droppable] element before overwriting.
			// Safe because: Gap A (genArrayLit clearDropFlag) ensures the vector
			// is the sole owner, and dup-on-read (genVectorIndex) ensures any
			// local variable that read via v[i] holds an independent copy.
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		} else if isMapOrSetType(elemType) {
			// T1167: Drop old Map/Set element before overwriting. Map/Set are 2-word
			// {i8*,i8*} value-struct containers deliberately excluded from
			// isDroppableHeapUserType (T0440), so the heap-user branch above skips
			// them, leaking the old container (struct + backing buffer). Mirrors the
			// heap-user branch (T0398) and the genMemberAssign Map/Set branch (T1167).
			// Safe because genVectorIndex dups Map/Set on read, so any aliased local
			// owns an independent copy.
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		} else if c.vecElemNeedsStructuralDrop(elemType) {
			// T1287: Drop the old structural-interface element box before overwriting.
			// The old {vtable, instance} view boxes a heap instance (heap user / string /
			// primitive / value box) that emitVariantFieldDrop's structural branch (T0765)
			// routes through __promise_structural_drop (RTTI: typeinfo.drop_fn_ptr →
			// concrete drop, else pal_free). Without this branch the overwritten box leaks.
			// Safe because reads of v[i] into a local deep-clone the box (cloneStructuralView
			// dup-on-read, T1284) — no live alias to the freed box. Mirrors the vector
			// element drop loop (T1284) and the T1291/T1292 structural drop routing.
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		} else if _, isSig := elemType.(*types.Signature); isSig {
			// T1226: Drop old closure element before overwriting. A capturing
			// closure element owns a heap env struct; overwriting via v[i] = newFn
			// leaks the old env. emitVariantFieldDrop's Signature case (T0739)
			// null-checks the env ptr and deep-drops it. Skip when old env == new
			// env (self-alias). Mirrors the genMemberAssign closure branch and is
			// paired with claimEnvTemp on the RHS in genAssignStmt.
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			if st, ok := oldVal.Type().(*irtypes.StructType); ok && len(st.Fields) == 2 {
				oldEnv := c.block.NewExtractValue(oldVal, 1)
				newEnv := c.block.NewExtractValue(val, 1)
				isNull := c.block.NewICmp(enum.IPredEQ, oldEnv, constant.NewNull(irtypes.I8Ptr))
				isSame := c.block.NewICmp(enum.IPredEQ, oldEnv, newEnv)
				skipDrop := c.block.NewOr(isNull, isSame)
				dropBlock := c.newBlock("elem.closuredrop")
				mergeBlock := c.newBlock("elem.closuredrop.done")
				c.block.NewCondBr(skipDrop, mergeBlock, dropBlock)
				c.block = dropBlock
				c.emitVariantFieldDrop(oldVal, elemType)
				c.block.NewBr(mergeBlock)
				c.block = mergeBlock
			}
		}
		c.block.NewStore(val, elemPtr)
		// T0909: When RHS is a method/operator whose body is `return this`,
		// the returned value aliases the receiver. Clear the receiver's drop
		// flag so scope-exit doesn't double-free the instance now owned by
		// this element slot.
		if srcExpr != nil {
			var aliasOrigin ast.Expr
			if call, ok := srcExpr.(*ast.CallExpr); ok {
				aliasOrigin = chainOriginExpr(call)
			} else {
				aliasOrigin = operatorReceiverOrigin(srcExpr)
			}
			if id, ok := aliasOrigin.(*ast.IdentExpr); ok {
				c.maybeClearReceiverDropFlag(val, id.Name, elemType)
			}
			// ThisExpr origin: `this` has no per-variable drop flag, no clear needed.
		}
		return
	}

	// Compound assignment
	current := c.block.NewLoad(elemLLVM, elemPtr)
	result := c.genCompoundOp(op, elemType, current, val)
	// T0363: Drop the old element before storing the new one. Without this,
	// heap-allocated old values leak.
	if extractNamed(elemType) == types.TypString {
		c.emitStringDropOldValue(current, result)
	} else {
		// T0715: drop the old heap user-type / droppable-enum element (the
		// non-native operator returns a fresh value). Alias-guarded; no-op for
		// value types/scalars.
		c.dropOldUserValueAtPtr(elemPtr, elemType, result)
	}
	c.block.NewStore(result, elemPtr)
}

// genCompoundIndexAssign handles compound index assignments (arr[i] += val, m[k] += val)
// with the canonical evaluation order: target → key → RHS → read → modify → write.
func (c *Compiler) genCompoundIndexAssign(target *ast.IndexExpr, op ast.AssignOp, valueExpr ast.Expr) {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	// Fixed-size array compound assignment.
	// Evaluation order (RHS before target) is safe: arrays are stack-local copy types, no aliasing.
	if arr, ok := targetType.(*types.Array); ok {
		val := c.genExpr(valueExpr)
		if c.info.AutoPropagateExprs[valueExpr] {
			val = c.genAutoPropagateValue(val)
		}
		c.genArrayIndexAssign(target, arr, op, val)
		return
	}

	named := extractNamed(targetType)
	if named != nil {
		if m := named.LookupMethod("[]="); m != nil {
			if m.IsNative() {
				// Native compound assign (vectors)
				elem, ok := types.AsVector(targetType)
				if !ok && named == types.TypVector && c.typeSubst != nil {
					tp := named.TypeParams()[0]
					elem, ok = c.typeSubst[tp], c.typeSubst[tp] != nil
				}
				if ok {
					slicePtr := c.genExpr(target.Target)
					idx := c.genExpr(target.Index)
					val := c.genExpr(valueExpr)
					if c.info.AutoPropagateExprs[valueExpr] {
						val = c.genAutoPropagateValue(val)
					}
					// COW: if static (.rodata), copy to heap first (T0062)
					elemLLVM := c.resolveType(elem)
					elemSize := int64(c.typeSize(elemLLVM))
					cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
						slicePtr, constant.NewInt(irtypes.I64, elemSize))
					c.storeBackSlicePtr(target.Target, cowSlice)
					c.genVectorCompoundAssign(cowSlice, idx, elem, op, val)
					return
				}
			} else {
				// Non-native: read via [], apply op, write via []=
				c.genMethodCompoundAssign(target, targetType, op, valueExpr)
				return
			}
		}
	}
	panic(fmt.Sprintf("codegen: cannot compound-assign to index of type %s", targetType))
}

// genMethodCompoundAssign handles compound assignment (e.g. m[k] += v) on non-native types
// by calling [] to read, applying the operator, then calling []= to write.
// The RHS is evaluated before the [] read, per the canonical compound-assign
// order target → key → RHS → read → op → write (T1090).
func (c *Compiler) genMethodCompoundAssign(target *ast.IndexExpr, targetType types.Type, op ast.AssignOp, valueExpr ast.Expr) {
	typeName := c.resolveTypeName(targetType)

	getFnName := mangleMethodName(typeName, "[]", false)
	getFn, ok := c.funcs[getFnName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared [] method %s", getFnName))
	}
	setFnName := mangleMethodName(typeName, "[]=", false)
	setFn, ok := c.funcs[setFnName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared []= method %s", setFnName))
	}

	targetVal := c.genExpr(target.Target)
	keyVal := c.genExpr(target.Index)
	val := c.genExpr(valueExpr)
	if c.info.AutoPropagateExprs[valueExpr] {
		val = c.genAutoPropagateValue(val)
	}

	var instancePtr value.Value
	if isContainerType(targetType) {
		instancePtr = targetVal
	} else {
		instancePtr = c.extractInstancePtr(targetVal)
	}

	// Call [] to get current value (returns V? for maps)
	var optVal value.Value = c.block.NewCall(getFn, instancePtr, keyVal)

	// T0709: a failable [] read propagates its error before the value is used.
	var indexMethod *types.Method
	if n := extractNamed(targetType); n != nil {
		indexMethod = n.LookupMethod("[]")
	}
	isOpt := true // existing behavior: [] returns V? (optional presence)
	if indexMethod != nil {
		if indexMethod.Sig().CanError() {
			optVal = c.genAutoPropagateValue(optVal)
		}
		_, isOpt = indexMethod.Sig().Result().(*types.Optional)
	}

	var current value.Value
	if isOpt {
		// Check has_value flag (field 0 of optional struct)
		hasVal := c.block.NewExtractValue(optVal, 0)
		okBlock := c.newBlock("mapcomp.ok")
		panicBlock := c.newBlock("mapcomp.panic")
		c.block.NewCondBr(hasVal, okBlock, panicBlock)

		c.block = panicBlock
		panicMsg := c.makeGlobalString("compound assignment on missing key")
		c.block.NewCall(c.funcs["promise_panic"], panicMsg)
		c.emitPanicReturn()

		c.block = okBlock
		current = c.block.NewExtractValue(optVal, 1)
	} else {
		current = optVal
	}
	// Compound op operates on V (the unwrapped element type from V?). For maps,
	// V is the second type argument; for other containers, derive from the []=
	// method's value parameter.
	operandType := c.compoundElemType(targetType)
	result := c.genCompoundOp(op, operandType, current, val)

	// T0363/T0714: drop the old value `current` after computing `result`.
	//   - Map.[] returns V? and dups the inner string, so a string `current` is a
	//     heap dup that would otherwise leak; the value stored in the map is freed
	//     separately by Map.[]='s drop-old-on-overwrite logic.
	//   - A non-map user container ([]/[]= on a plain user type) returns a fresh
	//     value from its getter (returning a field would move out of borrowed
	//     `this`), so a heap user-type / droppable-enum `current` would also leak.
	// emitDropOldCompoundValue is alias-guarded and a no-op for scalars/value
	// types.
	//
	// T0987: unlike strings/enum-payloads (which Map.[] dups) and plain user
	// containers (whose getter returns a fresh value), Map.[] returns a heap
	// user-type value ALIASED to the map's stored instance — no dup. Map.[]=
	// already drops the old stored value on overwrite, so dropping `current`
	// here for that case would free the same instance twice (double-free /
	// SEGV). Suppress the direct drop only for the Map-aliased heap-user-type
	// case; strings, enums, and non-map containers still need it.
	_, mapVal, isMap := types.AsMap(targetType)
	if isMap && c.aliasedMapHeapValue(mapVal) {
		// Map.[]= owns the drop of the overwritten value; do nothing here.
	} else {
		c.emitDropOldCompoundValue(current, result, operandType)
	}

	call := c.block.NewCall(setFn, instancePtr, keyVal, result)
	c.propagateIfFailable(call) // T0708
}

// compoundElemType returns the element type that compound assignment on a
// container operates on (V for Map[K, V], element for Vector, etc.). Falls
// back to the []= method's value parameter when the container isn't a known
// builtin.
func (c *Compiler) compoundElemType(containerType types.Type) types.Type {
	if _, v, ok := types.AsMap(containerType); ok {
		return v
	}
	if elem, ok := types.AsVector(containerType); ok {
		return elem
	}
	if named := extractNamed(containerType); named != nil {
		if m := named.LookupMethod("[]="); m != nil {
			params := m.Sig().Params()
			if len(params) >= 2 {
				return params[1].Type()
			}
		}
	}
	return nil
}

// genVectorCompoundAssign handles vec[i] += val with bounds check and pre-evaluated operands.
func (c *Compiler) genVectorCompoundAssign(slicePtr, idx value.Value, elemType types.Type, op ast.AssignOp, val value.Value) {
	elemLLVM := c.resolveType(elemType)

	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
	okBlock := c.newBlock("slicecomp.ok")
	panicBlock := c.newBlock("slicecomp.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	c.block = panicBlock
	oobMsg := c.makeGlobalString("index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.emitPanicReturn()

	c.block = okBlock
	dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)

	current := c.block.NewLoad(elemLLVM, elemPtr)
	result := c.genCompoundOp(op, elemType, current, val)
	// T0363: Drop the old element before storing the new one. Without this,
	// heap-allocated old values leak.
	if extractNamed(elemType) == types.TypString {
		c.emitStringDropOldValue(current, result)
	} else {
		// T0715: drop the old heap user-type / droppable-enum element (the
		// non-native operator returns a fresh value). Alias-guarded; no-op for
		// value types/scalars.
		c.dropOldUserValueAtPtr(elemPtr, elemType, result)
	}
	c.block.NewStore(result, elemPtr)
}

// --- Slice assignment ---

// genSliceAssign handles assignment to a slice target: v[a:b] = val.
func (c *Compiler) genSliceAssign(target *ast.SliceExpr, val value.Value) {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot slice-assign to type %s", targetType))
	}
	m := named.LookupMethod("[:]=")
	if m == nil {
		panic(fmt.Sprintf("codegen: no [:]=  method on type %s", named))
	}

	targetVal := c.genExpr(target.Target)

	// Generate optional int arguments for low and high bounds
	optIntType := irtypes.NewStruct(irtypes.I1, irtypes.I64)
	low := c.genSliceBound(target.Low, optIntType)
	high := c.genSliceBound(target.High, optIntType)

	mangledName := mangleMethodName(c.resolveTypeName(targetType), "[:]=", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared [:]=  method %s", mangledName))
	}

	var instancePtr value.Value
	switch {
	case isThisReceiver(target.Target):
		// T0745: `this` (incl. paren-wrapped) is already the i8* receiver ptr.
		instancePtr = targetVal
	case isContainerType(targetType):
		instancePtr = targetVal
	default:
		instancePtr = c.extractInstancePtr(targetVal)
	}

	// COW: if vector is static (.rodata), copy to heap first (T0062).
	// Must be done at the call site because [:]=  modifies this in-place
	// and the method's COW on individual element writes won't propagate back.
	if vecElem, isVec := types.AsVector(targetType); isVec {
		elemLLVM := c.resolveType(vecElem)
		elemSize := int64(c.typeSize(elemLLVM))
		instancePtr = c.block.NewCall(c.funcs["promise_vector_cow"],
			instancePtr, constant.NewInt(irtypes.I64, elemSize))
		c.storeBackSlicePtr(target.Target, instancePtr)
	}

	call := c.block.NewCall(fn, instancePtr, low, high, val)
	c.propagateIfFailable(call) // T0708
}

// genSliceCompoundAssign handles compound assignment to a slice target
// (v[a:b] += x) by reading the current value via [:], applying the operator,
// then writing via [:]=. Mirrors genMethodCompoundAssign (the [] path) for the
// read/op/write structure and genSliceAssign for receiver-ptr resolution and
// bound generation. T0714. Follows the canonical compound-assign order
// target → bounds → RHS → read → op → write (T1090): the RHS is evaluated
// before the [:] read, consistent with the []/native index paths.
func (c *Compiler) genSliceCompoundAssign(target *ast.SliceExpr, op ast.AssignOp, valueExpr ast.Expr) {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	// Unwrap MutRef/SharedRef for slicing (auto-deref through borrows), matching
	// genSliceExpr.
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot slice-compound-assign to type %s", targetType))
	}
	getM := named.LookupMethod("[:]")
	if getM == nil {
		panic(fmt.Sprintf("codegen: no [:] method on type %s", named))
	}
	setM := named.LookupMethod("[:]=")
	if setM == nil {
		panic(fmt.Sprintf("codegen: no [:]= method on type %s", named))
	}
	// Native slices (Vector/string) are unreachable: sema's checkOperator rejects
	// the binary operator on those element shapes before this path runs. The guard
	// documents that invariant.
	if getM.IsNative() || setM.IsNative() {
		panic(fmt.Sprintf("codegen: native slice compound assignment unsupported on type %s", named))
	}

	typeName := c.resolveTypeName(targetType)
	getFnName := mangleMethodName(typeName, "[:]", false)
	getFn, ok := c.funcs[getFnName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared [:] method %s", getFnName))
	}
	setFnName := mangleMethodName(typeName, "[:]=", false)
	setFn, ok := c.funcs[setFnName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared [:]= method %s", setFnName))
	}

	targetVal := c.genExpr(target.Target)

	var instancePtr value.Value
	switch {
	case isThisReceiver(target.Target):
		// T0745: `this` (incl. paren-wrapped) is already the i8* receiver ptr.
		instancePtr = targetVal
	case isContainerType(targetType):
		instancePtr = targetVal
	default:
		instancePtr = c.extractInstancePtr(targetVal)
	}

	// Generate bounds once — they are reused for both the [:] read and the [:]=
	// write, so side-effecting bound expressions evaluate exactly once.
	optIntType := irtypes.NewStruct(irtypes.I1, irtypes.I64)
	low := c.genSliceBound(target.Low, optIntType)
	high := c.genSliceBound(target.High, optIntType)

	// T1090: evaluate the RHS before the [:] read, matching the canonical order
	// (target → bounds → RHS → read → op → write) shared with the []/native
	// index paths. This is observable only when the RHS side-effects the slice
	// target, in which case the read combines the post-RHS value.
	val := c.genExpr(valueExpr)
	if c.info.AutoPropagateExprs[valueExpr] {
		val = c.genAutoPropagateValue(val)
	}

	// Read the current value via [:].
	var current value.Value = c.block.NewCall(getFn, instancePtr, low, high)
	// T0709 (reopened for slices by T0714): a failable [:] read propagates its
	// error before the value is used.
	if getM.Sig().CanError() {
		current = c.genAutoPropagateValue(current)
	}

	operandType := getM.Sig().Result()
	if c.typeSubst != nil {
		operandType = types.Substitute(operandType, c.typeSubst)
	}
	result := c.genCompoundOp(op, operandType, current, val)

	// T0363/T0714: drop the old heap-allocated value the getter returned before
	// it is overwritten, so heap operands (string, heap user types, droppable
	// enums) don't leak. The getter must return a fresh value (returning a field
	// would move out of borrowed `this`), so `current` is uniquely owned here.
	// Native vectors are excluded above, so no COW handling is needed.
	c.emitDropOldCompoundValue(current, result, operandType)

	call := c.block.NewCall(setFn, instancePtr, low, high, result)
	c.propagateIfFailable(call) // T0708
}
