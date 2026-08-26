package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/types"
)

// emitFieldDrops emits drop() calls for all fields of a type that themselves have drop().
// Called at the end of a user-defined drop() method to ensure fields are cleaned up.
// Fields are dropped in reverse declaration order.
func (c *Compiler) emitFieldDrops(named *types.Named) {
	c.emitFieldDropsFor(named, named.AllFields())
}

// emitFieldDropsFor emits drop() calls for the given subset of `named`'s fields.
// Used by emitFieldDrops (all fields, including inherited) and by inherited-drop
// synthesis (T0468) which drops only the child's own fields before delegating to
// the parent's mono drop. Fields are dropped in reverse declaration order.
func (c *Compiler) emitFieldDropsFor(named *types.Named, fields []*types.Field) {
	layout := c.lookupTypeLayout(named)
	if layout == nil {
		return
	}

	// Load the receiver (this is i8* in method context)
	thisAlloca, ok := c.locals["this"]
	if !ok {
		return
	}
	thisPtr := c.block.NewLoad(thisAlloca.ElemType, thisAlloca)
	typedPtr := c.block.NewBitCast(thisPtr, layout.InstancePtrType)

	for i := len(fields) - 1; i >= 0; i-- {
		f := fields[i]

		// T0101: Handle optional fields wrapping droppable types.
		// B0209: Apply type substitution so Optional[TypeParam] resolves to
		// the concrete inner type (e.g., T? → string? when T=string).
		fieldTypeRaw := f.Type()
		if c.typeSubst != nil {
			fieldTypeRaw = types.Substitute(fieldTypeRaw, c.typeSubst)
		}
		if opt, ok := fieldTypeRaw.(*types.Optional); ok {
			c.emitOptionalFieldDrop(opt, f, layout, typedPtr)
			continue
		}

		// B0217: Function-typed fields hold closure fat pointers {fn_ptr, env_ptr}.
		// Free the env pointer if non-null.
		if _, isSig := fieldTypeRaw.(*types.Signature); isSig {
			c.emitFuncFieldEnvFree(f, layout, typedPtr)
			continue
		}

		// T0420: Tuple-typed fields (e.g., generic T resolving to (int, HeapUser))
		// are stored inline. Load the struct value and drop each droppable element
		// via emitVariantFieldDrop, which handles tuple recursion.
		if tup, ok := fieldTypeRaw.(*types.Tuple); ok && c.tupleNeedsDrop(fieldTypeRaw) {
			fieldIdx, ok := layout.InstanceFieldIndex[f.Name()]
			if !ok {
				continue
			}
			fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			fieldVal := c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)
			c.emitVariantFieldDrop(fieldVal, tup)
			continue
		}

		// T0579: Fixed-size array fields with droppable element types. Without this,
		// extractNamed(arrType) returns nil and the field is silently skipped → leak.
		// Delegate per-element drop to emitVariantFieldDrop, which already handles
		// the Array case (T0485).
		if arr, ok := fieldTypeRaw.(*types.Array); ok && c.arrayFieldNeedsDrop(arr) {
			fieldIdx, ok := layout.InstanceFieldIndex[f.Name()]
			if !ok {
				continue
			}
			fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			fieldVal := c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)
			c.emitVariantFieldDrop(fieldVal, arr)
			continue
		}

		// B0192: Apply type substitution before extracting the named type,
		// so generic field types (e.g., T in GenericError[T]) resolve to
		// their concrete types (e.g., Point).
		fieldType := f.Type()
		if c.typeSubst != nil {
			fieldType = types.Substitute(fieldType, c.typeSubst)
		}

		// T0552: Enum-typed field. The enum value is stored inline in the
		// owner's instance struct; pass a pointer to it to the enum's drop
		// function (which switches on tag and drops variant data). Without
		// this branch, the field walk's `extractNamed == nil` skip below
		// silently drops enum fields, leaking their droppable variant data.
		if enumType := extractEnum(fieldType); enumType != nil {
			needsDrop := enumType.HasDrop() || enumType.NeedsSynthDrop()
			if inst, ok := fieldType.(*types.Instance); ok && monoEnumInstNeedsSynthDrop(inst) {
				needsDrop = true
			}
			if !needsDrop {
				continue
			}
			fieldIdx, ok := layout.InstanceFieldIndex[f.Name()]
			if !ok {
				continue
			}
			fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			enumName := enumType.Obj().Name()
			if inst, ok := fieldType.(*types.Instance); ok {
				enumName = monoName(inst)
			}
			mangledName := mangleMethodName(enumName, "drop", false)
			dropFn := c.funcs[mangledName]
			if dropFn == nil && c.moduleInfos != nil {
				dropFn = c.forwardDeclareModuleEnumDrop(enumType, enumName, mangledName)
			}
			if dropFn != nil {
				ptr := c.block.NewBitCast(fieldPtr, irtypes.I8Ptr)
				c.block.NewCall(dropFn, ptr)
			}
			continue
		}

		fieldNamed := extractNamed(fieldType)
		if fieldNamed == nil {
			continue
		}

		// T0460/T1706: Structural interface or polymorphic heap type field
		// (non-value-type). The LLVM slot holds the value struct {i8* vtable,
		// i8* instance}; load it, extract the instance pointer, and dispatch
		// through __promise_structural_drop which reads typeinfo.drop_fn_ptr
		// via RTTI and calls it (or pal_free if no drop is defined). The
		// concrete drop type is unknown at compile time, so RTTI dispatch is
		// required — both for structural interfaces and for concrete bases
		// with children (the runtime type may be a subtype with extra fields).
		if c.needsRttiDrop(fieldNamed) {
			fieldIdx, ok := layout.InstanceFieldIndex[f.Name()]
			if !ok {
				continue
			}
			fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			fieldVal := c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)
			instancePtr := c.extractInstancePtr(fieldVal)
			if c.structuralDrop != nil {
				nullCheck := c.block.NewICmp(enum.IPredEQ, instancePtr, constant.NewNull(irtypes.I8Ptr))
				dropBlk := c.newBlock("sfield.drop")
				contBlk := c.newBlock("sfield.cont")
				c.block.NewCondBr(nullCheck, contBlk, dropBlk)
				c.block = dropBlk
				c.block.NewCall(c.structuralDrop, instancePtr)
				c.block.NewBr(contBlk)
				c.block = contBlk
			}
			continue
		}

		// B0202: Check if the field is a mono instance with a synthesized drop
		// detected at codegen time (TypeParam fields → droppable concrete types).
		// Only match instances that specifically need B0202 mono synth drops — not
		// instances that already have drops via other paths (Vector, Channel, etc.).
		hasMonoSynthDrop := false
		if inst, ok := fieldType.(*types.Instance); ok {
			hasMonoSynthDrop = monoInstNeedsSynthDrop(inst)
		}

		// B0192: Determine if this is a heap-allocated user type that needs pal_free
		// even without a drop method (e.g., Point with only int fields inside
		// GenericError[Point]). Skip value types, copy types, primitive scalars,
		// structural interfaces, and opaque container types (Vector, Channel, Task)
		// which manage their own memory.
		needsFreeOnly := !hasMonoSynthDrop && !fieldNamed.HasDrop() && fieldNamed != types.TypChannel && fieldNamed != types.TypString &&
			!fieldNamed.IsValueType() && !fieldNamed.IsCopy() && !isPrimitiveScalar(fieldNamed) &&
			!fieldNamed.IsStructural() && !isOpaqueContainerType(fieldType)

		// T0560: TypTask has no declared drop() method (per-instantiation drop
		// is synthesized via getOrCreateTaskDrop). Without the explicit Task
		// allowance, the skip below fires and the Task field branch below is
		// unreachable, leaking the G handle. T1379: TypFailableTask is the same
		// (its drop additionally discharges the buffered aggregate).
		if !hasMonoSynthDrop && !fieldNamed.HasDrop() && fieldNamed != types.TypChannel && fieldNamed != types.TypString && !types.IsTaskLikeOrigin(fieldNamed) && !needsFreeOnly {
			continue
		}

		fieldIdx, ok := layout.InstanceFieldIndex[f.Name()]
		if !ok {
			continue
		}

		// Load the field value. Container/opaque types (Vector, Channel, String, Task)
		// are stored as raw i8* pointers, not value structs — use the loaded value directly.
		fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		fieldVal := c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)
		var fieldInstance value.Value
		if isContainerType(fieldType) || isOpaqueContainerType(fieldType) {
			fieldInstance = fieldVal
		} else {
			fieldInstance = c.extractInstancePtr(fieldVal)
		}

		// B0192: Non-droppable heap user types — just free the instance.
		if needsFreeOnly {
			c.block.NewCall(c.palFree, fieldInstance)
			continue
		}

		// String fields: call promise_string_drop directly (T0095).
		// promise_string_drop has internal null check + literal flag check.
		if fieldNamed == types.TypString {
			if dropFn, ok := c.funcs["promise_string_drop"]; ok {
				c.block.NewCall(dropFn, fieldInstance)
			}
			continue
		}

		// T0156: Mutex fields — per-instantiation drop (like Arc).
		if mutexElem, ok := types.AsMutex(fieldType); ok {
			resolvedElem := mutexElem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(mutexElem, c.typeSubst)
			}
			dropFn := c.getOrCreateMutexDrop(resolvedElem)
			c.block.NewCall(dropFn, fieldInstance)
			continue
		}

		// T0156: MutexGuard fields — T-independent drop.
		if types.IsMutexGuard(fieldType) || fieldNamed == types.TypMutexGuard {
			if dropFn, ok := c.funcs["MutexGuard.drop"]; ok {
				c.block.NewCall(dropFn, fieldInstance)
			}
			continue
		}

		// T0393: Arc fields — per-instantiation drop (like Mutex).
		if arcElem, ok := types.AsArc(fieldType); ok {
			resolvedElem := arcElem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(arcElem, c.typeSubst)
			}
			dropFn := c.getOrCreateArcDrop(resolvedElem)
			c.block.NewCall(dropFn, fieldInstance)
			continue
		}

		// T0393: Weak fields — per-instantiation drop (like Arc).
		if weakElem, ok := types.AsWeak(fieldType); ok {
			resolvedElem := weakElem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(weakElem, c.typeSubst)
			}
			dropFn := c.getOrCreateWeakDrop(resolvedElem)
			c.block.NewCall(dropFn, fieldInstance)
			continue
		}

		// T0560: Task fields — per-instantiation drop blocks on goroutine completion,
		// drops the result, frees result_ptr/panic_msg/G. Without this case the field
		// fell through to the heap-user-type path (gated by !isOpaqueContainerType),
		// so plain Task[T] fields silently no-op'd at scope exit and leaked the G.
		if taskElem, ok, taskFail := types.AsAnyTaskFailable(fieldType); ok {
			resolvedElem := taskElem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(taskElem, c.typeSubst)
			}
			dropFn := c.getOrCreateTaskDrop(resolvedElem, taskFail)
			c.block.NewCall(dropFn, fieldInstance)
			continue
		}

		// T0663: Channel fields — per-element-type drop walks any un-received
		// buffered items before freeing the channel. Pre-T0663 this fell through
		// to the resolveDropOwner path which looked up the single "Channel.drop"
		// symbol; that symbol no longer exists (drop is per element type now), so
		// an explicit branch is required or channel fields leak entirely.
		if chanElem, ok := types.AsChannel(fieldType); ok {
			resolvedElem := chanElem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(chanElem, c.typeSubst)
			}
			dropFn := c.getOrCreateChannelDrop(resolvedElem)
			c.block.NewCall(dropFn, fieldInstance)
			continue
		}

		// Resolve and call field type's drop() method.
		// T0132/B0202: For generic field instances with synthesized drops
		// (sema-time or mono-time), use the monomorphized name. Check the resolved
		// fieldType (not just when c.typeSubst is set) so non-generic types
		// containing generic instances also get the right drop name.
		// B0189: Drop string vector elements before freeing the buffer.
		// B0218: Also drop enum elements with synthesized drops, but ONLY when all
		// variant fields are safe to drop (no heap user types that could be aliased
		// via Map.[] / match destructure). String, primitive, value-type, and enum
		// fields are safe. Heap user types are NOT safe — Map.[] returns copies that
		// share instance pointers, so dropping at destruction would double-free.
		if elemType, isVec := types.AsVector(fieldType); isVec {
			// B0232: Drop enum elements with synthesized drops (e.g., Slot[K,V] in Map._buckets).
			// String fields extracted via match are dup'd (see bindEnumDestructure B0232),
			// so the originals in the enum data can be safely freed here.
			c.emitVectorElementDropLoop(fieldInstance, elemType)
		}

		// T0415: Generic user types with body-defined drops have per-instance
		// mangled names (e.g., Box[int].drop). Built-in native container types
		// (Vector, Channel) have a single global drop function and use the
		// origin name. Sema/mono synth drops are always per-instance.
		// Without the explicit-drop check, generic field types with an explicit
		// drop (HasDrop=true, NeedsSynthDrop=false, no mono synth) had their
		// drop silently skipped because the lookup used the origin name.
		ownerName := c.resolveDropOwner(fieldNamed)
		useMonoName := fieldNamed.NeedsSynthDrop() || hasMonoSynthDrop
		if !useMonoName && fieldNamed.HasDrop() {
			if dropMethod := fieldNamed.LookupMethod("drop"); dropMethod != nil && !dropMethod.IsNative() {
				useMonoName = true
			}
		}
		if useMonoName {
			if inst, ok := fieldType.(*types.Instance); ok {
				ownerName = monoName(inst)
			}
		}
		mangledName := mangleMethodName(ownerName, "drop", false)
		if dropFn, ok := c.funcs[mangledName]; ok {
			c.block.NewCall(dropFn, fieldInstance)
		}

		// T0106: Free the field instance after calling its drop().
		// Synthesized drops already include pal_free; explicit drops do not.
		// Container types manage their own memory in their drop functions.
		if !fieldNamed.NeedsSynthDrop() && !hasMonoSynthDrop && !isContainerType(fieldType) && !isOpaqueContainerType(fieldType) {
			c.block.NewCall(c.palFree, fieldInstance)
		}
	}
}

// emitOptionalFieldDrop emits a conditional drop for an optional-typed field (T0101).
// Checks the has-value flag, then drops the inner value if present.
// Layout: optional field is {i1 flag, T value} stored in the instance struct.
func (c *Compiler) emitOptionalFieldDrop(opt *types.Optional, f *types.Field, layout *TypeDeclLayout, typedPtr value.Value) {
	fieldIdx, ok := layout.InstanceFieldIndex[f.Name()]
	if !ok {
		return
	}

	// Load the optional field value.
	fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
	fieldVal := c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)

	c.emitOptionalValueDrop(fieldVal, opt)
}

// emitOptionalValueDrop emits cleanup for an already-loaded {i1 flag, T} optional value.
// Branches on the has-value flag, then drops the inner T according to its kind:
//   - string / Vector / Channel: i8* — call type-specific drop directly
//   - heap user type: {vtable*, instance*} — drop() if present, then pal_free unless
//     the type has a synthesized drop (which already calls pal_free)
//   - nested Optional[U]: recurse on the inner Optional
//
// T0392: Heap user types are now handled here (was previously deferred via stale
// B0181/T0111 comment). Force-unwrap sites neutralize the source's present flag
// (see neutralizeForceUnwrapSource), so the new owner doesn't double-free.
func (c *Compiler) emitOptionalValueDrop(optVal value.Value, opt *types.Optional) {
	elem := opt.Elem()
	innerNamed := extractNamed(elem)

	// T0392: Recurse for nested Optional (T??) — drop the inner Optional's value.
	if innerOpt, isOpt := elem.(*types.Optional); isOpt {
		hasVal := c.block.NewExtractValue(optVal, 0)
		dropBlock := c.newBlock("optfield.drop")
		skipBlock := c.newBlock("optfield.skip")
		c.block.NewCondBr(hasVal, dropBlock, skipBlock)
		c.block = dropBlock
		innerVal := c.block.NewExtractValue(optVal, 1)
		c.emitOptionalValueDrop(innerVal, innerOpt)
		if c.block.Term == nil {
			c.block.NewBr(skipBlock)
		}
		c.block = skipBlock
		return
	}

	// T0485: Tuple inner — Optional<(A, B, ...)>. Branch on has-value, then walk
	// each tuple element via emitVariantFieldDrop (which handles every element kind:
	// string, Vector, Channel, heap user, nested Tuple/Optional/Array).
	if tup, ok := elem.(*types.Tuple); ok {
		hasVal := c.block.NewExtractValue(optVal, 0)
		dropBlock := c.newBlock("optfield.drop")
		skipBlock := c.newBlock("optfield.skip")
		c.block.NewCondBr(hasVal, dropBlock, skipBlock)
		c.block = dropBlock
		innerVal := c.block.NewExtractValue(optVal, 1)
		c.emitVariantFieldDrop(innerVal, tup)
		if c.block.Term == nil {
			c.block.NewBr(skipBlock)
		}
		c.block = skipBlock
		return
	}

	// T0485: Array inner — Optional<[N]T>. Branch on has-value, then walk
	// each array element via emitVariantFieldDrop.
	if arr, ok := elem.(*types.Array); ok {
		hasVal := c.block.NewExtractValue(optVal, 0)
		dropBlock := c.newBlock("optfield.drop")
		skipBlock := c.newBlock("optfield.skip")
		c.block.NewCondBr(hasVal, dropBlock, skipBlock)
		c.block = dropBlock
		innerVal := c.block.NewExtractValue(optVal, 1)
		c.emitVariantFieldDrop(innerVal, arr)
		if c.block.Term == nil {
			c.block.NewBr(skipBlock)
		}
		c.block = skipBlock
		return
	}

	// T0741: Closure inner — Optional<() -> T>. Branch on has-value, then drop
	// the inner closure's env via emitVariantFieldDrop's Signature case (null-
	// checks the env and emitEnvDropOrFree). Covers optional-closure struct
	// fields and optional-closure enum-variant fields.
	if _, ok := elem.(*types.Signature); ok {
		hasVal := c.block.NewExtractValue(optVal, 0)
		dropBlock := c.newBlock("optfield.drop")
		skipBlock := c.newBlock("optfield.skip")
		c.block.NewCondBr(hasVal, dropBlock, skipBlock)
		c.block = dropBlock
		innerVal := c.block.NewExtractValue(optVal, 1)
		c.emitVariantFieldDrop(innerVal, elem)
		if c.block.Term == nil {
			c.block.NewBr(skipBlock)
		}
		c.block = skipBlock
		return
	}

	// T0572: Enum inner — Optional<EnumT>. Branch on has-value, then drop the
	// loaded enum value via emitVariantFieldDrop, which already dispatches to
	// the right drop name (plain / mono / cross-module forward-declared). The
	// `extractNamed` switch below cannot handle enums (extractNamed returns nil
	// for enum types), so without this branch the optional silently skips its
	// inner enum's drop and leaks variant data.
	if enumType := extractEnum(elem); enumType != nil {
		needsDrop := enumType.HasDrop() || enumType.NeedsSynthDrop()
		if inst, ok := elem.(*types.Instance); ok && monoEnumInstNeedsSynthDrop(inst) {
			needsDrop = true
		}
		if !needsDrop {
			return
		}
		hasVal := c.block.NewExtractValue(optVal, 0)
		dropBlock := c.newBlock("optfield.drop")
		skipBlock := c.newBlock("optfield.skip")
		c.block.NewCondBr(hasVal, dropBlock, skipBlock)
		c.block = dropBlock
		innerVal := c.block.NewExtractValue(optVal, 1)
		c.emitVariantFieldDrop(innerVal, elem)
		if c.block.Term == nil {
			c.block.NewBr(skipBlock)
		}
		c.block = skipBlock
		return
	}

	// Determine cleanup needed for the inner type.
	var dropFunc *ir.Func
	// T0668: when set, the inner type is Task[taskJoinElem] and the un-awaited
	// task is dropped via the cooperative join (emitTaskJoinAndFree) instead of
	// the bare legacy spin — fixes the single-threaded WASM scheduler livelock
	// for `task[T]?` fields/locals dropped inside a coroutine body.
	var taskJoinElem types.Type
	haveTaskJoin := false
	taskJoinFailable := false // T1379: inner is FailableTask[taskJoinElem]
	isHeapUser := false

	switch {
	case innerNamed == types.TypString:
		dropFunc = c.funcs["promise_string_drop"]
	case innerNamed == types.TypVector:
		dropFunc = c.funcs["Vector.drop"]
	case types.IsChannel(elem) || innerNamed == types.TypChannel:
		// T0663: Optional[Channel[T]] field scope-exit — per-element-type drop
		// walks any un-received buffered items before freeing the channel.
		var resolvedChanElem types.Type
		if chanElem, ok := types.AsChannel(elem); ok {
			resolvedChanElem = chanElem
			if c.typeSubst != nil {
				resolvedChanElem = types.Substitute(chanElem, c.typeSubst)
			}
		} else if innerNamed == types.TypChannel && c.typeSubst != nil {
			if tp := types.TypChannel.TypeParams(); len(tp) > 0 {
				resolvedChanElem = c.typeSubst[tp[0]]
				if resolvedChanElem != nil {
					resolvedChanElem = types.Substitute(resolvedChanElem, c.typeSubst)
				}
			}
		}
		dropFunc = c.getOrCreateChannelDrop(resolvedChanElem)
	case types.IsAnyTask(elem) || types.IsTaskLikeOrigin(innerNamed):
		// T0560: Optional[Task[T]] field scope-exit — per-instantiation drop
		// blocks on goroutine completion, drops result, frees result_ptr/G.
		// Without this, dispatch fell through to the heap-user-type case which
		// is gated by !isOpaqueContainerType, so this branch was never taken.
		failable := types.IsFailableTask(elem) || innerNamed == types.TypFailableTask
		taskOrigin := types.TypTask
		if failable {
			taskOrigin = types.TypFailableTask
		}
		var resolvedTaskElem types.Type
		if taskElem, ok, _ := types.AsAnyTaskFailable(elem); ok {
			resolvedTaskElem = taskElem
			if c.typeSubst != nil {
				resolvedTaskElem = types.Substitute(taskElem, c.typeSubst)
			}
		} else if innerNamed != nil && c.typeSubst != nil {
			if tp := taskOrigin.TypeParams(); len(tp) > 0 {
				resolvedTaskElem = c.typeSubst[tp[0]]
				if resolvedTaskElem != nil {
					resolvedTaskElem = types.Substitute(resolvedTaskElem, c.typeSubst)
				}
			}
		}
		// T0668: keep dropFunc non-nil so the has-value branch is still reached,
		// but emit the cooperative join at the drop site instead of this spin.
		dropFunc = c.getOrCreateTaskDrop(resolvedTaskElem, failable)
		taskJoinElem = resolvedTaskElem
		taskJoinFailable = failable
		haveTaskJoin = true
	case types.IsArc(elem) || innerNamed == types.TypArc:
		// T0573: Optional[Ref[T]] field scope-exit — per-instantiation drop
		// decrements the refcount, drops the payload + frees the box on last ref.
		// Without this case, Arc fell through to the heap-user-type path (gated
		// by !isOpaqueContainerType) and silently leaked.
		var resolvedArcElem types.Type
		if arcElem, ok := types.AsArc(elem); ok {
			resolvedArcElem = arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
		} else if innerNamed == types.TypArc && c.typeSubst != nil {
			if tp := types.TypArc.TypeParams(); len(tp) > 0 {
				resolvedArcElem = c.typeSubst[tp[0]]
				if resolvedArcElem != nil {
					resolvedArcElem = types.Substitute(resolvedArcElem, c.typeSubst)
				}
			}
		}
		dropFunc = c.getOrCreateArcDrop(resolvedArcElem)
	case types.IsWeak(elem) || innerNamed == types.TypWeak:
		// T0573: Optional[Weak[T]] field scope-exit — per-instantiation drop
		// decrements the weak count and frees the box if both counts reach zero.
		var resolvedWeakElem types.Type
		if weakElem, ok := types.AsWeak(elem); ok {
			resolvedWeakElem = weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
		} else if innerNamed == types.TypWeak && c.typeSubst != nil {
			if tp := types.TypWeak.TypeParams(); len(tp) > 0 {
				resolvedWeakElem = c.typeSubst[tp[0]]
				if resolvedWeakElem != nil {
					resolvedWeakElem = types.Substitute(resolvedWeakElem, c.typeSubst)
				}
			}
		}
		dropFunc = c.getOrCreateWeakDrop(resolvedWeakElem)
	case types.IsMutex(elem) || innerNamed == types.TypMutex:
		// T0573: Optional[Mutex[T]] field scope-exit — per-instantiation drop
		// destroys the mutex, drops the payload, and frees the box.
		var resolvedMutexElem types.Type
		if mutexElem, ok := types.AsMutex(elem); ok {
			resolvedMutexElem = mutexElem
			if c.typeSubst != nil {
				resolvedMutexElem = types.Substitute(mutexElem, c.typeSubst)
			}
		} else if innerNamed == types.TypMutex && c.typeSubst != nil {
			if tp := types.TypMutex.TypeParams(); len(tp) > 0 {
				resolvedMutexElem = c.typeSubst[tp[0]]
				if resolvedMutexElem != nil {
					resolvedMutexElem = types.Substitute(resolvedMutexElem, c.typeSubst)
				}
			}
		}
		dropFunc = c.getOrCreateMutexDrop(resolvedMutexElem)
	case types.IsMutexGuard(elem) || innerNamed == types.TypMutexGuard:
		// T0573: Optional[MutexGuard] field scope-exit — MutexGuard.drop is a
		// single T-independent symbol (T0156), so look it up by name.
		dropFunc = c.funcs["MutexGuard.drop"]
	case innerNamed != nil && innerNamed.IsStructural() && !innerNamed.IsValueType():
		// T0460: Optional[StructuralInterface] field — branch on has-value,
		// then dispatch through __promise_structural_drop via RTTI. The
		// concrete type is unknown at compile time; typeinfo.drop_fn_ptr
		// resolves the right drop (or pal_free when none defined).
		if c.structuralDrop == nil {
			return
		}
		hasVal := c.block.NewExtractValue(optVal, 0)
		dropBlock := c.newBlock("optfield.drop")
		skipBlock := c.newBlock("optfield.skip")
		c.block.NewCondBr(hasVal, dropBlock, skipBlock)

		c.block = dropBlock
		innerVal := c.block.NewExtractValue(optVal, 1)
		instancePtr := c.extractInstancePtr(innerVal)
		nullCheck := c.block.NewICmp(enum.IPredEQ, instancePtr, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("optfield.struct.exec")
		c.block.NewCondBr(nullCheck, skipBlock, execBlock)

		c.block = execBlock
		c.block.NewCall(c.structuralDrop, instancePtr)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
		return
	case innerNamed != nil && !innerNamed.IsValueType() && !innerNamed.IsCopy() &&
		!isPrimitiveScalar(innerNamed) && !innerNamed.IsStructural() &&
		!isOpaqueContainerType(elem):
		// T0392: Heap user type — needs pal_free at minimum, plus drop() if it has one.
		isHeapUser = true
		hasMonoSynthDrop := false
		if inst, ok := elem.(*types.Instance); ok {
			hasMonoSynthDrop = monoInstNeedsSynthDrop(inst)
		}
		if innerNamed.HasDrop() || innerNamed.NeedsSynthDrop() || hasMonoSynthDrop {
			ownerName := c.resolveDropOwner(innerNamed)
			// Generic type methods are mangled per-instance — always use the mono
			// name when elem is an Instance (covers explicit drop, sema synth, and
			// mono synth alike).
			if inst, ok := elem.(*types.Instance); ok {
				ownerName = monoName(inst)
			}
			dropFunc = c.funcs[mangleMethodName(ownerName, "drop", false)]
		}
	default:
		return // value type or primitive — no cleanup needed
	}

	if dropFunc == nil && !isHeapUser {
		return
	}

	hasVal := c.block.NewExtractValue(optVal, 0)
	dropBlock := c.newBlock("optfield.drop")
	skipBlock := c.newBlock("optfield.skip")
	c.block.NewCondBr(hasVal, dropBlock, skipBlock)

	c.block = dropBlock
	innerVal := c.block.NewExtractValue(optVal, 1)

	if isHeapUser {
		// User type: inner is value struct {vtable*, instance*}.
		instancePtr := c.extractInstancePtr(innerVal)
		// Null-check: zero-initialized optionals may have null instance pointers.
		nullCheck := c.block.NewICmp(enum.IPredEQ, instancePtr, constant.NewNull(irtypes.I8Ptr))
		freeBlock := c.newBlock("optfield.free")
		skipFreeBlock := c.newBlock("optfield.free.skip")
		c.block.NewCondBr(nullCheck, skipFreeBlock, freeBlock)

		c.block = freeBlock
		if dropFunc != nil {
			c.block.NewCall(dropFunc, instancePtr)
		}
		// Free the instance — synthesized drops already call pal_free; explicit
		// drops do not. Without an explicit drop, dropFunc is nil and we still pal_free.
		needsFree := !innerNamed.NeedsSynthDrop()
		if inst, ok := elem.(*types.Instance); ok && monoInstNeedsSynthDrop(inst) {
			needsFree = false
		}
		if needsFree {
			c.block.NewCall(c.palFree, instancePtr)
		}
		c.block.NewBr(skipFreeBlock)

		c.block = skipFreeBlock
		c.block.NewBr(skipBlock)
	} else if haveTaskJoin {
		// T0668: Optional[Task[T]] — cooperative park-suspend join in a
		// coroutine body (test body / WASM main / go {}); legacy spin otherwise.
		c.emitTaskJoinAndFree(innerVal, taskJoinElem, taskJoinFailable)
		c.block.NewBr(skipBlock)
	} else {
		// T0354: For Vector inner type, iterate elements and drop heap elements
		// (strings, enums, user types, tuples) before freeing the buffer. Without
		// this, optional `T[]?` fields leak per-element heap allocations.
		if elemType, isVec := types.AsVector(elem); isVec {
			c.emitVectorElementDropLoop(innerVal, elemType)
		}
		// String/container: inner is i8*, call drop directly.
		c.block.NewCall(dropFunc, innerVal)
		c.block.NewBr(skipBlock)
	}

	c.block = skipBlock
}

// emitOptionalFieldReassignDrop emits a conditional drop/free for an optional-typed field
// before reassignment (B0240). When overwriting an optional field (e.g., p.location = none),
// the old inner value must be cleaned up. This handles string, vector, channel, and
// heap user types (with or without drop methods).
// Note: safe for the common case where the optional is overwritten without prior unwrap.
// If the optional was force-unwrapped before reassignment, the unwrapped local also has
// a scope binding (bindingFree) that will free the same instance — a double-free.
// This is tracked by T0111 (dup-on-unwrap); until then, user code that unwraps and then
// overwrites the same optional field has undefined behavior.
func (c *Compiler) emitOptionalFieldReassignDrop(opt *types.Optional, field *types.Field, ownerType types.Type, fieldPtr value.Value) {
	elem := opt.Elem()
	innerNamed := extractNamed(elem)

	// Determine what cleanup is needed for the inner type.
	var dropFunc *ir.Func
	// T0668: Task[taskJoinElem] reassignment → cooperative join (coroutine).
	var taskJoinElem types.Type
	haveTaskJoin := false
	taskJoinFailable := false // T1379: inner is FailableTask[taskJoinElem]
	needsFreeOnly := false
	isStructuralInner := false

	switch {
	case innerNamed == types.TypString:
		dropFunc = c.funcs["promise_string_drop"]
	case types.IsVector(elem):
		dropFunc = c.funcs["Vector.drop"]
	case types.IsChannel(elem) || innerNamed == types.TypChannel:
		// T0663: Optional[Channel[T]] field reassignment — per-element-type drop
		// walks any un-received buffered items before freeing the channel.
		var resolvedChanElem types.Type
		if chanElem, ok := types.AsChannel(elem); ok {
			resolvedChanElem = chanElem
			if c.typeSubst != nil {
				resolvedChanElem = types.Substitute(chanElem, c.typeSubst)
			}
		} else if innerNamed == types.TypChannel && c.typeSubst != nil {
			if tp := types.TypChannel.TypeParams(); len(tp) > 0 {
				resolvedChanElem = c.typeSubst[tp[0]]
				if resolvedChanElem != nil {
					resolvedChanElem = types.Substitute(resolvedChanElem, c.typeSubst)
				}
			}
		}
		dropFunc = c.getOrCreateChannelDrop(resolvedChanElem)
	case types.IsAnyTask(elem) || types.IsTaskLikeOrigin(innerNamed):
		// T0560: Optional[Task[T]] field reassignment — per-instantiation drop
		// blocks on goroutine completion, drops result, frees result_ptr/G.
		// Without this, dispatch fell through to the heap-user-type case which
		// is gated by !isOpaqueContainerType, so this branch was never taken
		// and old tasks leaked on reassignment.
		failable := types.IsFailableTask(elem) || innerNamed == types.TypFailableTask
		taskOrigin := types.TypTask
		if failable {
			taskOrigin = types.TypFailableTask
		}
		var resolvedTaskElem types.Type
		if taskElem, ok, _ := types.AsAnyTaskFailable(elem); ok {
			resolvedTaskElem = taskElem
			if c.typeSubst != nil {
				resolvedTaskElem = types.Substitute(taskElem, c.typeSubst)
			}
		} else if innerNamed != nil && c.typeSubst != nil {
			if tp := taskOrigin.TypeParams(); len(tp) > 0 {
				resolvedTaskElem = c.typeSubst[tp[0]]
				if resolvedTaskElem != nil {
					resolvedTaskElem = types.Substitute(resolvedTaskElem, c.typeSubst)
				}
			}
		}
		dropFunc = c.getOrCreateTaskDrop(resolvedTaskElem, failable)
		taskJoinElem = resolvedTaskElem
		taskJoinFailable = failable
		haveTaskJoin = true
	case types.IsArc(elem) || innerNamed == types.TypArc:
		// T0573: Optional[Ref[T]] field reassignment — per-instantiation drop
		// decrements the refcount on the old value. Without this case, dispatch
		// fell through to the heap-user-type path (gated by !isOpaqueContainerType)
		// and old Arc values leaked on reassignment.
		var resolvedArcElem types.Type
		if arcElem, ok := types.AsArc(elem); ok {
			resolvedArcElem = arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
		} else if innerNamed == types.TypArc && c.typeSubst != nil {
			if tp := types.TypArc.TypeParams(); len(tp) > 0 {
				resolvedArcElem = c.typeSubst[tp[0]]
				if resolvedArcElem != nil {
					resolvedArcElem = types.Substitute(resolvedArcElem, c.typeSubst)
				}
			}
		}
		dropFunc = c.getOrCreateArcDrop(resolvedArcElem)
	case types.IsWeak(elem) || innerNamed == types.TypWeak:
		// T0573: Optional[Weak[T]] field reassignment — per-instantiation drop
		// decrements the weak count on the old value.
		var resolvedWeakElem types.Type
		if weakElem, ok := types.AsWeak(elem); ok {
			resolvedWeakElem = weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
		} else if innerNamed == types.TypWeak && c.typeSubst != nil {
			if tp := types.TypWeak.TypeParams(); len(tp) > 0 {
				resolvedWeakElem = c.typeSubst[tp[0]]
				if resolvedWeakElem != nil {
					resolvedWeakElem = types.Substitute(resolvedWeakElem, c.typeSubst)
				}
			}
		}
		dropFunc = c.getOrCreateWeakDrop(resolvedWeakElem)
	case types.IsMutex(elem) || innerNamed == types.TypMutex:
		// T0573: Optional[Mutex[T]] field reassignment — per-instantiation drop
		// destroys the old mutex, drops its payload, and frees the box.
		var resolvedMutexElem types.Type
		if mutexElem, ok := types.AsMutex(elem); ok {
			resolvedMutexElem = mutexElem
			if c.typeSubst != nil {
				resolvedMutexElem = types.Substitute(mutexElem, c.typeSubst)
			}
		} else if innerNamed == types.TypMutex && c.typeSubst != nil {
			if tp := types.TypMutex.TypeParams(); len(tp) > 0 {
				resolvedMutexElem = c.typeSubst[tp[0]]
				if resolvedMutexElem != nil {
					resolvedMutexElem = types.Substitute(resolvedMutexElem, c.typeSubst)
				}
			}
		}
		dropFunc = c.getOrCreateMutexDrop(resolvedMutexElem)
	case types.IsMutexGuard(elem) || innerNamed == types.TypMutexGuard:
		// T0573: Optional[MutexGuard] field reassignment — MutexGuard.drop is a
		// single T-independent symbol (T0156).
		dropFunc = c.funcs["MutexGuard.drop"]
	case innerNamed != nil && innerNamed.IsStructural() && !innerNamed.IsValueType():
		// T1300: Optional[StructuralInterface] member field reassignment — the old
		// slot holds a {vtable, instance} view box. Drop it through
		// __promise_structural_drop (RTTI: typeinfo.drop_fn_ptr → concrete drop,
		// else pal_free), mirroring the scope-exit path (emitOptionalValueDrop,
		// T0460) and the vector/map overwrite drops (T1287/T1292). Without it the
		// overwritten view box leaks (value-box case: 1 alloc; heap-box case: box +
		// inner instance/string).
		isStructuralInner = true
	case innerNamed != nil && !innerNamed.IsValueType() && !innerNamed.IsCopy() &&
		!isPrimitiveScalar(innerNamed) && !innerNamed.IsStructural() &&
		!isOpaqueContainerType(elem):
		// Heap user type — needs pal_free at minimum, plus drop() if it has one.
		// T0415: Generic type methods are mangled per-instance — always use the
		// mono name when elem is an Instance (covers explicit drop, sema synth,
		// and mono synth alike).
		hasMonoSynthDrop := false
		if inst, ok := elem.(*types.Instance); ok {
			hasMonoSynthDrop = monoInstNeedsSynthDrop(inst)
		}
		if innerNamed.HasDrop() || innerNamed.NeedsSynthDrop() || hasMonoSynthDrop {
			ownerName := c.resolveDropOwner(innerNamed)
			if inst, ok := elem.(*types.Instance); ok {
				ownerName = monoName(inst)
			}
			mangledName := mangleMethodName(ownerName, "drop", false)
			dropFunc = c.funcs[mangledName]
		}
		needsFreeOnly = true
	default:
		return // value type or primitive — no cleanup needed
	}

	if dropFunc == nil && !needsFreeOnly && !isStructuralInner {
		return
	}
	// T1300: structural inner requires the structural-drop runtime; skip if absent
	// (mirrors emitOptionalValueDrop's nil guard).
	if isStructuralInner && c.structuralDrop == nil {
		return
	}

	// Get the LLVM type of the optional field for loading.
	layout := c.lookupTypeLayout(ownerType)
	if layout == nil {
		return
	}
	fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
	if !ok {
		// Value type layout — optional fields in value types are uncommon but handle gracefully.
		if fidx, ok2 := layout.ValueFieldIndex[field.Name()]; ok2 {
			fieldIdx = fidx
		} else {
			return
		}
	}

	var fieldLLVMType irtypes.Type
	if layout.IsValueType {
		fieldLLVMType = layout.Value.Fields[fieldIdx].LLVMType
	} else {
		fieldLLVMType = layout.Instance.Fields[fieldIdx].LLVMType
	}

	// Load old optional value and check has-value flag.
	oldOpt := c.block.NewLoad(fieldLLVMType, fieldPtr)
	hasVal := c.block.NewExtractValue(oldOpt, 0)

	dropBlock := c.newBlock("field.optdrop")
	mergeBlock := c.newBlock("field.optdrop.done")
	c.block.NewCondBr(hasVal, dropBlock, mergeBlock)

	c.block = dropBlock
	innerVal := c.block.NewExtractValue(oldOpt, 1)

	if isStructuralInner {
		// T1300: old slot holds a {vtable, instance} view box — drop through RTTI.
		instancePtr := c.extractInstancePtr(innerVal)
		nullCheck := c.block.NewICmp(enum.IPredEQ, instancePtr, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("field.optdrop.struct.exec")
		afterBlock := c.newBlock("field.optdrop.struct.done")
		c.block.NewCondBr(nullCheck, afterBlock, execBlock)

		c.block = execBlock
		c.block.NewCall(c.structuralDrop, instancePtr)
		c.block.NewBr(afterBlock)

		c.block = afterBlock
	} else if innerNamed == types.TypString || types.IsVector(elem) || types.IsChannel(elem) ||
		types.IsAnyTask(elem) || types.IsTaskLikeOrigin(innerNamed) ||
		types.IsArc(elem) || innerNamed == types.TypArc ||
		types.IsWeak(elem) || innerNamed == types.TypWeak ||
		types.IsMutex(elem) || innerNamed == types.TypMutex ||
		types.IsMutexGuard(elem) || innerNamed == types.TypMutexGuard {
		// T0358 (T0354 follow-up): for Vector inner type, iterate elements and
		// drop heap elements before freeing the buffer.
		if elemType, isVec := types.AsVector(elem); isVec {
			c.emitVectorElementDropLoop(innerVal, elemType)
		}
		if haveTaskJoin {
			// T0668: Optional[Task[T]] reassignment — cooperative join in a
			// coroutine body; legacy spin otherwise.
			c.emitTaskJoinAndFree(innerVal, taskJoinElem, taskJoinFailable)
		} else {
			// String/container/Arc/Weak/Mutex/MutexGuard: inner is i8*, call drop directly.
			c.block.NewCall(dropFunc, innerVal)
		}
	} else {
		// User type: inner is value struct {vtable*, instance*}.
		instancePtr := c.extractInstancePtr(innerVal)
		// Null-check: zero-initialized optionals may have null instance pointers.
		nullCheck := c.block.NewICmp(enum.IPredEQ, instancePtr, constant.NewNull(irtypes.I8Ptr))
		freeBlock := c.newBlock("field.optdrop.free")
		skipFreeBlock := c.newBlock("field.optdrop.free.skip")
		c.block.NewCondBr(nullCheck, skipFreeBlock, freeBlock)

		c.block = freeBlock
		if dropFunc != nil {
			c.block.NewCall(dropFunc, instancePtr)
		}
		// Free the instance (synthesized drops include pal_free; explicit drops do not).
		// T0415: A generic instance whose origin lacks a synth-drop flag may still
		// have a mono-time synth drop that already calls pal_free — don't double-free.
		needsFree := innerNamed != nil && !innerNamed.NeedsSynthDrop()
		if inst, ok := elem.(*types.Instance); ok && monoInstNeedsSynthDrop(inst) {
			needsFree = false
		}
		if needsFree {
			c.block.NewCall(c.palFree, instancePtr)
		}
		c.block.NewBr(skipFreeBlock)

		c.block = skipFreeBlock
	}

	c.block.NewBr(mergeBlock)
	c.block = mergeBlock
}

// emitFuncFieldEnvFree frees the env pointer of a function-typed field (B0217).
// Function values are fat pointers {fn_ptr, env_ptr}. The env_ptr may be a
// heap-allocated capture struct that needs freeing. Null-checks before freeing.
func (c *Compiler) emitFuncFieldEnvFree(f *types.Field, layout *TypeDeclLayout, typedPtr value.Value) {
	fieldIdx, ok := layout.InstanceFieldIndex[f.Name()]
	if !ok {
		return
	}

	// Load the function field value ({i8*, i8*} fat pointer)
	fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
	fieldVal := c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)

	// Extract env pointer (field 1 of fat pointer)
	envPtr := c.block.NewExtractValue(fieldVal, 1)

	// Null-check: only free if env is non-null (no-capture lambdas have null env)
	isNull := c.block.NewICmp(enum.IPredEQ, envPtr, constant.NewNull(irtypes.I8Ptr))
	freeBlock := c.newBlock("funcfield.env.free")
	skipBlock := c.newBlock("funcfield.env.skip")
	c.block.NewCondBr(isNull, skipBlock, freeBlock)

	c.block = freeBlock
	// T0741 Part B: deep-drop the env (drops captured strings/vectors/nested
	// closures via the env's field-0 drop fn) instead of a shallow pal_free,
	// which would leak heap captures. The struct's drop is the sole owner of
	// this field's env, so the deep drop runs exactly once.
	c.emitEnvDropOrFree(envPtr)
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}
