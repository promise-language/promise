package codegen

import (
	"fmt"
	"math"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// emitEnumDropCall emits a conditional drop call for an enum variable (T0102).
// Checks drop flag, then passes the alloca pointer (bitcast to i8*) to the drop function.
func (c *Compiler) emitEnumDropCall(b scopeBinding) {
	if b.dropFlag == nil || b.dropFunc == nil {
		return
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	dropBlock := c.newBlock("enum.drop.call")
	skipBlock := c.newBlock("enum.drop.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	ptr := c.block.NewBitCast(b.alloca, irtypes.I8Ptr)
	c.block.NewCall(b.dropFunc, ptr)
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitOptionalLocalValueDrop drops the inner value of an Optional struct held
// by a local variable binding. optVal is the loaded {i1 flag, X} struct;
// elemType is the immediate inner Promise type X. If X is itself Optional
// (nested), walks through layers recursively. At the bottom (non-Optional
// inner), dispatches via b.dropFunc / b.rttiDrop using elemType to choose the
// call shape (T0391). Distinct from compiler.go's emitOptionalValueDrop, which
// derives dispatch from the type alone — this helper reuses the precomputed
// dispatch info on the binding so per-instantiation drops (Arc, Mutex, Weak,
// MutexGuard) and structural-interface RTTI dispatch are handled correctly.
func (c *Compiler) emitOptionalLocalValueDrop(optVal value.Value, elemType types.Type, b scopeBinding) {
	hasVal := c.block.NewExtractValue(optVal, 0)
	dropInnerBlock := c.newBlock("optdrop.inner")
	doneBlock := c.newBlock("optdrop.done")
	c.block.NewCondBr(hasVal, dropInnerBlock, doneBlock)

	c.block = dropInnerBlock
	innerVal := c.block.NewExtractValue(optVal, 1)

	if innerOpt, ok := elemType.(*types.Optional); ok {
		// T0391: Nested Optional — walk into the next layer.
		innerElem := innerOpt.Elem()
		if c.typeSubst != nil {
			innerElem = types.Substitute(innerElem, c.typeSubst)
		}
		c.emitOptionalLocalValueDrop(innerVal, innerElem, b)
	} else if _, isTup := elemType.(*types.Tuple); isTup {
		// T0397: Tuple inner type — walk fields and drop droppable ones via
		// the same helper used by bindingDropTuple.
		typ := elemType
		if c.typeSubst != nil {
			typ = types.Substitute(typ, c.typeSubst)
		}
		c.emitVariantFieldDrop(innerVal, typ)
	} else if _, isSig := elemType.(*types.Signature); isSig {
		// T0814: closure inner — free the fat pointer's env (deep-drop captures).
		typ := elemType
		if c.typeSubst != nil {
			typ = types.Substitute(typ, c.typeSubst)
		}
		c.emitVariantFieldDrop(innerVal, typ)
	} else if b.rttiDrop {
		// B0243: RTTI-based drop dispatch for Optional[StructuralInterface].
		// The concrete type is unknown at compile time — dispatch through typeinfo.
		instance := c.extractInstancePtr(innerVal)
		nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("optdrop.rtti")
		nullSkip := c.newBlock("optdrop.null")
		c.block.NewCondBr(nullCheck, nullSkip, execBlock)

		c.block = execBlock
		c.emitStructuralInstanceDrop(instance)
		c.block.NewBr(nullSkip)

		c.block = nullSkip
	} else if b.dropFunc != nil {
		// Enum inner type: store to temp alloca, bitcast to i8*, call drop.
		if extractEnum(elemType) != nil {
			enumLLVM := c.resolveType(elemType)
			tmpAlloca := c.createEntryAlloca(enumLLVM)
			c.block.NewStore(innerVal, tmpAlloca)
			ptr := c.block.NewBitCast(tmpAlloca, irtypes.I8Ptr)
			c.block.NewCall(b.dropFunc, ptr)
		} else if _, isTask := types.AsAnyTask(elemType); (isTask || types.IsTaskLikeOrigin(b.named)) &&
			c.emitTaskJoinAndFreeByDropFn(innerVal, b.dropFunc) {
			// T0668: `task[T]? o` local — cooperative park-suspend join in a
			// coroutine body (test body / WASM main / go {}) so the
			// single-threaded WASM scheduler can run the pending goroutine;
			// emitTaskJoinAndFreeByDropFn falls back to the legacy spin
			// (returns true) when not in a coroutine.
		} else if isContainerType(elemType) || b.named == types.TypString {
			// String, vector, channel: inner is i8*, call drop directly.
			// T0938: For a vector inner with droppable elements (e.g. string[]?),
			// b.dropFunc is the generic Vector.drop which frees only the buffer.
			// Mirror emitStringDropCall: walk and drop elements first, under the
			// same bit-63 static-vector guard. Static .rodata vectors skip both
			// the element drops and the buffer free.
			resolved := elemType
			if c.typeSubst != nil {
				resolved = types.Substitute(resolved, c.typeSubst)
			}
			if vecElem, isVec := types.AsVector(resolved); isVec {
				headerType := vectorHeaderType()
				headerPtr := c.block.NewBitCast(innerVal, irtypes.NewPointer(headerType))
				rawLen := loadVectorLenRaw(c.block, headerPtr)
				bit63 := c.block.NewAnd(rawLen, constant.NewInt(irtypes.I64, math.MinInt64))
				isStatic := c.block.NewICmp(enum.IPredNE, bit63, constant.NewInt(irtypes.I64, 0))
				vecDoneBlock := c.newBlock("optvecdrop.done")
				nonStaticBlock := c.newBlock("optvecdrop.nonstatic")
				c.block.NewCondBr(isStatic, vecDoneBlock, nonStaticBlock)

				c.block = nonStaticBlock
				c.emitVectorElementDropLoop(innerVal, vecElem)
				c.block.NewCall(b.dropFunc, innerVal)
				c.block.NewBr(vecDoneBlock)

				c.block = vecDoneBlock
			} else {
				c.block.NewCall(b.dropFunc, innerVal)
			}
		} else {
			// User type: inner is value struct {vtable, instance}, extract instance ptr
			instance := c.extractInstancePtr(innerVal)
			nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
			execBlock := c.newBlock("optdrop.exec")
			nullSkip := c.newBlock("optdrop.null")
			c.block.NewCondBr(nullCheck, nullSkip, execBlock)

			c.block = execBlock
			c.block.NewCall(b.dropFunc, instance)
			c.block.NewBr(nullSkip)

			c.block = nullSkip
		}
	}
	c.block.NewBr(doneBlock)

	c.block = doneBlock
}

// emitOptionalDropCall emits a conditional drop for an optional value (T0101).
// Checks: drop flag → has-value flag → drop inner value.
// Layout: optional is {i1 flag, T value} — field 0 is has-value, field 1 is inner.
// T0391: For nested Optionals (T??, T???...), b.valType is the immediate inner
// Optional and emitOptionalLocalValueDrop walks through layers recursively.
func (c *Compiler) emitOptionalDropCall(b scopeBinding) {
	if b.dropFlag == nil {
		return
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	dropBlock := c.newBlock("optdrop.check")
	skipBlock := c.newBlock("optdrop.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	optVal := c.block.NewLoad(b.alloca.ElemType, b.alloca)

	c.emitOptionalLocalValueDrop(optVal, b.valType, b)

	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitTupleDropCall emits a conditional drop for a tuple value variable (T0371).
// Loads the tuple struct from its alloca, then walks fields and drops each
// droppable element via emitVariantFieldDrop (string, vector, channel, user
// types with drop, enums with drop, recursive tuples).
func (c *Compiler) emitTupleDropCall(b scopeBinding) {
	if b.dropFlag == nil {
		return
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	dropBlock := c.newBlock("tupdrop.exec")
	skipBlock := c.newBlock("tupdrop.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	tupVal := c.block.NewLoad(b.alloca.ElemType, b.alloca)
	typ := b.valType
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	c.emitVariantFieldDrop(tupVal, typ)
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitArrayDropCall emits a conditional per-element drop for a fixed-size array
// variable (T0389). Walks each [N x T] slot via GEP, loads the element, and
// calls emitVariantFieldDrop on it (string, vector, channel, user types with
// drop, enums with drop, tuples with droppable fields).
func (c *Compiler) emitArrayDropCall(b scopeBinding) {
	if b.dropFlag == nil {
		return
	}
	arrType, ok := b.valType.(*types.Array)
	if !ok {
		return
	}
	elemType := arrType.Elem()
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	dropBlock := c.newBlock("arrdrop.exec")
	skipBlock := c.newBlock("arrdrop.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	llvmArrType := b.alloca.ElemType
	for i := int64(0); i < arrType.Size(); i++ {
		elemPtr := c.block.NewGetElementPtr(llvmArrType, b.alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, i))
		elemVal := c.block.NewLoad(c.resolveType(elemType), elemPtr)
		c.emitVariantFieldDrop(elemVal, elemType)
	}
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitArrayTempDrop emits a conditional per-element drop for a fixed-array
// statement temp (T1181) — the result of a T[N]-returning call used inline and
// never bound. Mirrors emitArrayDropCall but drives off a stmtTemp whose alloca
// holds the `[N x T]` aggregate. Clears the drop flag after dropping (B0172) so a
// temp reused across loop iterations isn't dropped twice on an already-freed value.
func (c *Compiler) emitArrayTempDrop(temp stmtTemp) {
	elemType := temp.arrType.Elem()
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}

	flag := c.block.NewLoad(irtypes.I1, temp.dropFlag)
	dropBlock := c.newBlock("arrtmp.drop")
	skipBlock := c.newBlock("arrtmp.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	llvmArrType := temp.alloca.ElemType
	for i := int64(0); i < temp.arrType.Size(); i++ {
		elemPtr := c.block.NewGetElementPtr(llvmArrType, temp.alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, i))
		elemVal := c.block.NewLoad(c.resolveType(elemType), elemPtr)
		c.emitVariantFieldDrop(elemVal, elemType)
	}
	c.block.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitInstanceFieldDropsAndFree drops all droppable fields of `named` for the
// instance at `instance` (an i8*), then frees it — WITHOUT running any
// user-defined drop() body. T0967: the use-binding close path uses this when a
// use-bound type also defines drop(); §16.4 suppresses the user drop (use takes
// precedence), but the instance's fields and memory must still be reclaimed
// (zero-leak policy). Mirrors defineSynthesizedDropBody's field-drop + free
// sequence, but operates mid-block on an already-extracted instance pointer.
//
// valType is the concrete value type of the binding. When it is a generic
// instance, `named` is the generic origin whose fields are unbound TypeParams,
// so we reconstruct the mono context (typeSubst + monoCtx) exactly as inside the
// mono drop body — this makes emitFieldDrops resolve the concrete field types
// and select the mono instance layout. Without it, generic fields are skipped
// and leak.
func (c *Compiler) emitInstanceFieldDropsAndFree(named *types.Named, valType types.Type, instance value.Value) {
	savedSubst := c.typeSubst
	savedCtx := c.monoCtx
	if inst, ok := valType.(*types.Instance); ok {
		if subst := c.buildOwnerTypeArgSubst(inst); subst != nil {
			c.typeSubst = subst
		}
		c.monoCtx = &monoContext{inst: inst, origin: inst.Origin(), name: monoName(inst)}
	}

	savedThis, hadThis := c.locals["this"]
	thisAlloca := c.createEntryAlloca(irtypes.I8Ptr)
	c.block.NewStore(instance, thisAlloca)
	c.locals["this"] = thisAlloca // emitFieldDropsFor reads locals["this"]
	c.emitFieldDrops(named)       // own + inherited fields, reverse order
	c.block.NewCall(c.palFree, instance)
	if hadThis {
		c.locals["this"] = savedThis
	} else {
		delete(c.locals, "this")
	}
	c.typeSubst = savedSubst
	c.monoCtx = savedCtx
}

// emitDropSuppressedError drops an error instance (i8*) that is being suppressed.
// T0135: Used when a failable close() error is suppressed (error in flight or
// duplicate close error). Calls error.drop to free the message string and instance.
func (c *Compiler) emitDropSuppressedError(errPtr value.Value) {
	dropName := mangleMethodName("__mod_std_error", "drop", false)
	if dropFn, ok := c.funcs[dropName]; ok {
		c.block.NewCall(dropFn, errPtr)
	}
}

// emitDropCall emits a conditional drop() call for a droppable variable.
// Checks the drop flag; if true (not moved), calls drop().
// Dispatches to emitStringDropCall for bindingDropString bindings.
func (c *Compiler) emitDropCall(b scopeBinding) {
	if b.kind == bindingDropString {
		c.emitStringDropCall(b)
		return
	}
	if b.kind == bindingDropOptional {
		c.emitOptionalDropCall(b)
		return
	}
	if b.kind == bindingDropTuple {
		c.emitTupleDropCall(b)
		return
	}
	if b.kind == bindingDropArray {
		c.emitArrayDropCall(b)
		return
	}
	if b.dropFlag == nil {
		// No drop flag — unconditional drop
		c.emitDropCallDirect(b)
		return
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	dropBlock := c.newBlock("drop.call")
	skipBlock := c.newBlock("drop.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	c.emitDropCallDirect(b)
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitDropCallDirect emits the actual drop() call (direct or virtual dispatch).
// Guards against null instance pointers (e.g., zero-initialized values from
// error handler paths that don't produce a recovery value).
// Container types (Vector, Channel) store raw i8* — not value structs — so we
// load the i8* directly instead of extracting field 1 from a struct.
func (c *Compiler) emitDropCallDirect(b scopeBinding) {
	val := c.block.NewLoad(b.alloca.ElemType, b.alloca)

	// Container types (Vector, Channel) store raw i8* pointers, not value structs.
	// Use the loaded i8* directly — extractInstancePtr would crash on a non-struct.
	var instance value.Value
	if isContainerType(b.valType) {
		instance = val
	} else {
		instance = c.extractInstancePtr(val)
	}

	// Null-check instance pointer: zero-initialized values (from error handler
	// fallthrough) have null instance — skip drop to avoid dereferencing null.
	nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
	dropExecBlock := c.newBlock("drop.exec")
	dropDoneBlock := c.newBlock("drop.done")
	c.block.NewCondBr(nullCheck, dropDoneBlock, dropExecBlock)

	c.block = dropExecBlock
	if b.rttiDrop {
		// B0226: RTTI-based drop dispatch for untyped error catches.
		// Load the drop function pointer from the error instance's typeinfo (field 1)
		// and call it. Synthesized drops handle pal_free internally; explicit user drops
		// use a $wrap function that calls drop + pal_free (B0247).
		c.emitRttiDropDispatch(instance)
	} else if b.dropFunc != nil {
		c.block.NewCall(b.dropFunc, instance)
	} else if b.named != nil {
		vtableRaw := c.extractVtablePtr(val)

		slotIndex := b.named.VirtualMethodIndex("drop", false)
		if slotIndex < 0 {
			panic(fmt.Sprintf("codegen: drop method not in vtable for %s", b.named))
		}
		vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
		fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
			constant.NewInt(irtypes.I32, int64(slotIndex)))
		fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

		funcType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
		fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))
		c.block.NewCall(fnTyped, instance)
	}
	// B0159: Free the instance struct after drop() completes.
	// Only for types with explicit drop — synthesized drops (B0158/B0160/B0202) are
	// deferred until ownership tracking prevents aliasing issues.
	// Container types are excluded — their drop already frees the buffer.
	// B0226: rttiDrop dispatch calls the concrete drop which handles pal_free internally.
	if !b.rttiDrop && !isContainerType(b.valType) && b.named != nil && !b.named.NeedsSynthDrop() && !b.monoSynthDrop {
		c.block.NewCall(c.palFree, instance)
	}
	c.block.NewBr(dropDoneBlock)

	c.block = dropDoneBlock
}

// emitRttiDropDispatch loads the drop function pointer from the error instance's
// typeinfo and calls it. Falls back to base error.drop if the typeinfo drop_fn_ptr
// is null.
// B0226: Enables correct drop for generic error subtypes (e.g., GenericError[Point])
// caught via untyped error handlers (? e { ... }).
func (c *Compiler) emitRttiDropDispatch(instance value.Value) {
	// Load variant pointer from instance (field 0 of instance struct)
	variantPtr := c.loadVariantPtr(instance)

	// Typeinfo struct type (only need first 2 fields for drop_fn_ptr access)
	typeinfoType := irtypes.NewStruct(
		irtypes.I8Ptr, // field 0: vtable_ptr
		irtypes.I8Ptr, // field 1: drop_fn_ptr
	)

	// Load drop_fn_ptr (field 1 of typeinfo)
	typedPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoType))
	dropFnPtr := c.block.NewGetElementPtr(typeinfoType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dropFn := c.block.NewLoad(irtypes.I8Ptr, dropFnPtr)

	// If non-null, call the concrete drop; otherwise fall back to base error.drop
	isNull := c.block.NewICmp(enum.IPredEQ, dropFn, constant.NewNull(irtypes.I8Ptr))
	callBlock := c.newBlock("rtti.drop.call")
	fallbackBlock := c.newBlock("rtti.drop.fallback")
	doneBlock := c.newBlock("rtti.drop.done")
	c.block.NewCondBr(isNull, fallbackBlock, callBlock)

	// Concrete drop via typeinfo
	c.block = callBlock
	dropFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
	typedFn := c.block.NewBitCast(dropFn, irtypes.NewPointer(dropFnType))
	c.block.NewCall(typedFn, instance)
	c.block.NewBr(doneBlock)

	// Fallback: base error.drop
	c.block = fallbackBlock
	baseDropName := mangleMethodName("__mod_std_error", "drop", false)
	if baseDropFn, ok := c.funcs[baseDropName]; ok {
		c.block.NewCall(baseDropFn, instance)
	}
	c.block.NewBr(doneBlock)

	c.block = doneBlock
}

// emitFailableErrorDrop drops a buffered failable error instance (an i8* to a
// heap error) via the same RTTI dispatch used for a caught error at scope exit
// — the concrete error type's drop (which frees message strings and the
// instance) or the base error.drop fallback. Null-guarded. T1379: used by
// FailableTask[T].free_after_done to discharge the error of an un-received
// failable task so it does not leak.
func (c *Compiler) emitFailableErrorDrop(errInstance value.Value) {
	isNull := c.block.NewICmp(enum.IPredEQ, errInstance, constant.NewNull(irtypes.I8Ptr))
	dropBlk := c.newBlock("ftask.err.drop")
	doneBlk := c.newBlock("ftask.err.done")
	c.block.NewCondBr(isNull, doneBlk, dropBlk)
	c.block = dropBlk
	c.emitRttiDropDispatch(errInstance)
	c.block.NewBr(doneBlk)
	c.block = doneBlk
}

// emitStructuralInstanceDrop drops a heap-allocated instance behind a structural interface
// using RTTI-based dispatch (B0243). Loads the typeinfo drop_fn_ptr from the instance's
// variant field. If drop_fn is non-null, calls it — synthesized drops include pal_free;
// explicit user drops use a $wrap function that calls drop + pal_free (B0247).
// If drop_fn is null (type has no drop), calls pal_free directly.
func (c *Compiler) emitStructuralInstanceDrop(instance value.Value) {
	// Load variant pointer from instance (field 0 = typeinfo ptr)
	variantPtr := c.loadVariantPtr(instance)

	// Typeinfo layout: { i8* vtable_ptr, i8* drop_fn_ptr, ... }
	typeinfoType := irtypes.NewStruct(
		irtypes.I8Ptr, // field 0: vtable_ptr
		irtypes.I8Ptr, // field 1: drop_fn_ptr
	)
	typedPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoType))
	dropFnField := c.block.NewGetElementPtr(typeinfoType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dropFn := c.block.NewLoad(irtypes.I8Ptr, dropFnField)

	isNull := c.block.NewICmp(enum.IPredEQ, dropFn, constant.NewNull(irtypes.I8Ptr))
	callBlock := c.newBlock("struct.drop.call")
	freeBlock := c.newBlock("struct.drop.free")
	doneBlock := c.newBlock("struct.drop.done")
	c.block.NewCondBr(isNull, freeBlock, callBlock)

	// Has drop function: call it (synth drops include pal_free; explicit user
	// drops use $wrap which calls drop + pal_free per B0247)
	c.block = callBlock
	dropFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
	typedFn := c.block.NewBitCast(dropFn, irtypes.NewPointer(dropFnType))
	c.block.NewCall(typedFn, instance)
	c.block.NewBr(doneBlock)

	// No drop function: just free the instance
	c.block = freeBlock
	c.block.NewCall(c.palFree, instance)
	c.block.NewBr(doneBlock)

	c.block = doneBlock
}

// dropDiscardedOptional handles B0196/B0208: when an ExprStmt discards an
// Optional result with a droppable inner type (string, vector, channel, user
// type with drop), the inner value must be dropped. trackStringTemp only tracks
// bare i8* values, so {i1, T} optionals slip through.
func (c *Compiler) dropDiscardedOptional(expr ast.Expr, result value.Value) {
	if result == nil || c.block == nil || c.block.Term != nil {
		return
	}
	exprType := c.info.Types[expr]
	if exprType == nil {
		return
	}
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	opt, ok := exprType.(*types.Optional)
	if !ok {
		return
	}
	// T1234: only drop a discarded optional that is an owned statement temp. A
	// discarded *place* expression (bare `o;`, `obj.f;`, `arr[i];`) merely reads
	// (borrows) the inner value — the place's own binding/owner drops it, so
	// dropping it here double-frees (segfault for a capturing closure env whose
	// binding also frees it; use-after-free if the local is used again). A `move`
	// out of a place is a MoveExpr, not a bare place, so it still drops here (and
	// the move site clears the source's drop flag). Mirrors trackHeapUserTypeResult's
	// ident/member-source skips and dropDiscardedHeapType's CallExpr-only guard.
	if isBorrowingPlaceExpr(expr) {
		return
	}
	elem := opt.Elem()
	if c.typeSubst != nil {
		elem = types.Substitute(elem, c.typeSubst)
	}
	innerNamed := extractNamed(elem)

	// T1234: Optional closure `(() -> T)?`. The inner value is a closure fat
	// pointer {fn_ptr, env_ptr}; extractNamed on a function type is nil, so the
	// dropFunc switch below never fires and the heap env leaks on discard. Mirror
	// dropDiscardedAutoPropagate's signature arm: when the tag is set, extract the
	// env pointer and drop-or-free it (presence-guarded — a captureless closure
	// has a null env; a `none` result skips the whole block via the tag branch).
	if _, isSig := elem.(*types.Signature); isSig {
		tag := c.block.NewExtractValue(result, 0)
		dropBlock := c.newBlock("discard.drop")
		skipBlock := c.newBlock("discard.skip")
		c.block.NewCondBr(tag, dropBlock, skipBlock)

		c.block = dropBlock
		innerClosure := c.block.NewExtractValue(result, 1)
		envPtr := c.block.NewExtractValue(innerClosure, 1)
		isNull := c.block.NewICmp(enum.IPredEQ, envPtr, constant.NewNull(irtypes.I8Ptr))
		freeBlock := c.newBlock("discard.env.free")
		c.block.NewCondBr(isNull, skipBlock, freeBlock)

		c.block = freeBlock
		c.emitEnvDropOrFree(envPtr)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
		return
	}

	// Resolve the drop function for the inner type.
	var dropFunc *ir.Func
	var isContainer bool

	switch {
	case innerNamed == types.TypString:
		dropFunc = c.funcs["promise_string_drop"]
	case innerNamed != nil && (func() bool { _, ok := types.AsVector(elem); return ok }() || innerNamed == types.TypVector):
		dropFunc = c.funcs["Vector.drop"]
		isContainer = true
	case innerNamed != nil && (func() bool { _, ok := types.AsChannel(elem); return ok }() || innerNamed == types.TypChannel):
		// T0663: Channel inner drop — per-element-type drop walks buffered items.
		if chanElem, ok := types.AsChannel(elem); ok {
			resolvedChanElem := chanElem
			if c.typeSubst != nil {
				resolvedChanElem = types.Substitute(chanElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateChannelDrop(resolvedChanElem)
			isContainer = true
		}
	case innerNamed != nil && (func() bool { _, ok := types.AsArc(elem); return ok }() || innerNamed == types.TypArc):
		// T0155: Arc inner drop
		if arcElem, ok := types.AsArc(elem); ok {
			resolvedArcElem := arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateArcDrop(resolvedArcElem)
		}
		isContainer = true
	case innerNamed != nil && (func() bool { _, ok := types.AsWeak(elem); return ok }() || innerNamed == types.TypWeak):
		// T0157: Weak inner drop
		if weakElem, ok := types.AsWeak(elem); ok {
			resolvedWeakElem := weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateWeakDrop(resolvedWeakElem)
		}
		isContainer = true
	case innerNamed != nil && (func() bool { _, ok := types.AsMutex(elem); return ok }() || innerNamed == types.TypMutex):
		// T0156: Mutex inner drop
		if mutexElem, ok := types.AsMutex(elem); ok {
			resolvedElem := mutexElem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(mutexElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateMutexDrop(resolvedElem)
		}
		isContainer = true
	case innerNamed != nil && (func() bool { _, ok := types.AsMutexGuard(elem); return ok }() || innerNamed == types.TypMutexGuard):
		// T0156: MutexGuard inner drop
		dropFunc = c.funcs["MutexGuard.drop"]
		isContainer = true
	case innerNamed != nil && (innerNamed.HasDrop() || innerNamed.NeedsSynthDrop()):
		ownerName := innerNamed.Obj().Name()
		resolvedElem := elem
		if c.typeSubst != nil {
			resolvedElem = types.Substitute(elem, c.typeSubst)
		}
		if inst, ok := resolvedElem.(*types.Instance); ok {
			ownerName = monoName(inst)
		} else if innerNamed.HasDrop() && !innerNamed.NeedsSynthDrop() {
			ownerName = c.resolveDropOwner(innerNamed)
		}
		mangledName := mangleMethodName(ownerName, "drop", false)
		dropFunc = c.funcs[mangledName]
	default:
		return // inner type not droppable
	}

	if dropFunc == nil {
		return
	}

	// result is {i1, T} — extract tag and conditionally drop inner value.
	tag := c.block.NewExtractValue(result, 0)
	dropBlock := c.newBlock("discard.drop")
	skipBlock := c.newBlock("discard.skip")
	c.block.NewCondBr(tag, dropBlock, skipBlock)

	c.block = dropBlock
	innerVal := c.block.NewExtractValue(result, 1)

	if innerNamed == types.TypString || isContainer {
		// String and containers store raw i8* — call drop directly.
		c.block.NewCall(dropFunc, innerVal)
	} else {
		// User type: inner is value struct {vtable, instance} — extract instance ptr.
		instance := c.extractInstancePtr(innerVal)
		nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("discard.exec")
		nullSkip := c.newBlock("discard.null")
		c.block.NewCondBr(nullCheck, nullSkip, execBlock)

		c.block = execBlock
		c.block.NewCall(dropFunc, instance)
		// B0159: Free the instance struct after drop() completes.
		if innerNamed != nil && !innerNamed.NeedsSynthDrop() {
			c.block.NewCall(c.palFree, instance)
		}
		c.block.NewBr(nullSkip)

		c.block = nullSkip
	}
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// dropDiscardedTuple handles T1233: when an ExprStmt discards a droppable tuple
// TEMP (a tuple literal or tuple-returning call whose heap fields would otherwise
// orphan — e.g. `(make_str(), 1);`), register a caller tuple stmtTemp so the
// field-wise drop fires at statement end. genTupleLit claims each element temp
// INTO the aggregate, so without this the discarded aggregate's heap fields leak.
// Borrowed/owned-variable sources (an ident with its own bindingDropTuple, or a
// container/field read) are excluded by tupleArgIsCallerOwnedTemp.
func (c *Compiler) dropDiscardedTuple(expr ast.Expr, result value.Value) {
	if result == nil || c.block == nil || c.block.Term != nil {
		return
	}
	exprType := c.info.Types[expr]
	if exprType == nil {
		return
	}
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	tup, ok := exprType.(*types.Tuple)
	if !ok || !c.tupleNeedsDrop(tup) {
		return
	}
	if !c.tupleArgIsCallerOwnedTemp(expr) {
		return
	}
	c.registerTupleStmtTemp(result, tup)
}

// dropDiscardedGenerator handles T1306: when an ExprStmt discards a generator
// factory call result (e.g. `gen(3);`), the raw {handle, slot} coroutine value
// leaks its yield slot and coroutine frame — nothing routes it through any
// cleanup path. Emit the same generator-native cleanup as a consumed generator's
// bindingGenerator (emitGeneratorCleanup): destroy the coroutine handle and free
// the yield slot (+ error slot for the failable {handle, slot, errslot} shape).
//
// NOT __promise_iter_cleanup / __promise_structural_drop: a generator instance has
// a distinct layout from _FnIter (T0088), so those would crash. Every stream[T]
// value is freshly produced by a generator factory (a stream[T]-returning function
// MUST contain yield — sema-enforced), so it is always owned and never an alias:
// unconditionally freeing it at statement end is sound.
func (c *Compiler) dropDiscardedGenerator(expr ast.Expr, result value.Value) {
	if result == nil || c.block == nil || c.block.Term != nil {
		return
	}
	exprType := c.info.Types[expr]
	if c.typeSubst != nil && exprType != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	if exprType == nil {
		return
	}
	if _, ok := types.AsStream(exprType); !ok {
		return
	}
	st, ok := result.Type().(*irtypes.StructType)
	if !ok || (len(st.Fields) != 2 && len(st.Fields) != 3) {
		return
	}
	handle := c.block.NewExtractValue(result, 0)
	slot := c.block.NewExtractValue(result, 1)
	isNull := c.block.NewICmp(enum.IPredEQ, handle, constant.NewNull(irtypes.I8Ptr))
	cleanBlk := c.newBlock("discard.gen.cleanup")
	doneBlk := c.newBlock("discard.gen.done")
	c.block.NewCondBr(isNull, doneBlk, cleanBlk)

	c.block = cleanBlk
	c.block.NewCall(c.genDestroy, handle)
	c.block.NewCall(c.palFree, slot)
	if len(st.Fields) == 3 { // failable generator: also free the error slot (B0023)
		errSlot := c.block.NewExtractValue(result, 2)
		c.block.NewCall(c.palFree, errSlot)
	}
	c.block.NewBr(doneBlk)

	c.block = doneBlk
}

// dropDiscardedHeapType handles B0211: when an ExprStmt discards a heap-allocated
// user type constructor result (e.g., `Foo(x: 1);`), the instance leaks.
// Only handles constructor calls — method/getter returns may share instance
// pointers with existing objects, so freeing them would cause use-after-free.
func (c *Compiler) dropDiscardedHeapType(expr ast.Expr, result value.Value) {
	if result == nil || c.block == nil || c.block.Term != nil {
		return
	}
	// Only handle constructor calls (CallExpr whose callee resolves to a type).
	callExpr, isCall := expr.(*ast.CallExpr)
	if !isCall {
		return
	}
	calleeType := c.info.Types[callExpr.Callee]
	if c.typeSubst != nil && calleeType != nil {
		calleeType = types.Substitute(calleeType, c.typeSubst)
	}
	switch calleeType.(type) {
	case *types.Named, *types.Instance:
		// Constructor call — proceed
	default:
		return // Not a constructor
	}

	exprType := c.info.Types[expr]
	if exprType == nil {
		return
	}
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	// Only handle user types with value struct layout {i8*, i8*}
	named := extractNamed(exprType)
	if named == nil || named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return
	}
	// Skip containers and strings — handled by trackStringTemp
	if isContainerType(exprType) || named == types.TypString {
		return
	}
	// Must be a struct value to extract instance pointer
	if _, ok := result.Type().(*irtypes.StructType); !ok {
		return
	}

	// T0346: Claim any existing heap temp tracking this allocation (e.g.,
	// genConstructorCallMono's palFree track at expr.go:1903) so cleanupHeapTemps
	// doesn't double-free what we're about to free explicitly.
	c.claimHeapTemp(result)

	instance := c.extractInstancePtr(result)
	nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
	freeBlock := c.newBlock("discard.heap.free")
	doneBlock := c.newBlock("discard.heap.done")
	c.block.NewCondBr(nullCheck, doneBlock, freeBlock)

	c.block = freeBlock
	if dropFunc := c.resolveDropFuncForTemp(named, exprType); dropFunc != nil && dropFunc != c.palFree {
		c.block.NewCall(dropFunc, instance)
		// Explicit drop (not synth) doesn't include pal_free
		if named.HasDrop() && !named.NeedsSynthDrop() {
			c.block.NewCall(c.palFree, instance)
		}
	} else {
		c.block.NewCall(c.palFree, instance)
	}
	c.block.NewBr(doneBlock)

	c.block = doneBlock
}

// emitStringDropCall emits a conditional promise_string_drop call for a string variable.
// String allocas store raw i8* (instance pointer), not a value struct — so we load
// the i8* directly and pass it to promise_string_drop (which checks the literal flag).
// B0189: For vectors with droppable elements, emits an element-drop loop before freeing
// the buffer. This handles Vector[string], Vector[Vector[T]], Vector[Channel[T]], and
// vectors of user types with drop().
func (c *Compiler) emitStringDropCall(b scopeBinding) {
	if b.dropFlag == nil {
		panic("codegen: string drop binding must have a drop flag")
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	dropBlock := c.newBlock("strdrop.call")
	skipBlock := c.newBlock("strdrop.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	ptr := c.block.NewLoad(b.alloca.ElemType, b.alloca)

	// Null-check: zero-initialized values from error handler fallthrough
	nullCheck := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
	execBlock := c.newBlock("strdrop.exec")
	doneBlock := c.newBlock("strdrop.done")
	c.block.NewCondBr(nullCheck, doneBlock, execBlock)

	c.block = execBlock

	// B0203: For vectors, check the static flag (bit 63 of len). Passthrough
	// variadic vectors are marked static at the call site to prevent the callee
	// from dropping the caller's vector and its elements. Static .rodata vectors
	// also benefit (Vector.drop already checked bit 63, but element drops did not).
	valType := b.valType
	if c.typeSubst != nil {
		valType = types.Substitute(valType, c.typeSubst)
	}
	if _, isVec := types.AsVector(valType); isVec || (b.named != nil && b.named == types.TypVector) {
		headerType := vectorHeaderType()
		headerPtr := c.block.NewBitCast(ptr, irtypes.NewPointer(headerType))
		rawLen := loadVectorLenRaw(c.block, headerPtr)
		bit63 := c.block.NewAnd(rawLen, constant.NewInt(irtypes.I64, math.MinInt64))
		isStatic := c.block.NewICmp(enum.IPredNE, bit63, constant.NewInt(irtypes.I64, 0))
		nonStaticBlock := c.newBlock("vecdrop.nonstatic")
		c.block.NewCondBr(isStatic, doneBlock, nonStaticBlock)
		c.block = nonStaticBlock
	}

	// B0189: Drop vector elements before freeing the buffer.
	c.emitVectorElementDrops(b, ptr)

	// T0668: a direct `task[T] t = go {…}` binding reuses this bindingDropString
	// path (same i8* alloca + void(i8*) drop shape). In a coroutine body (test
	// body / WASM main / go {}) route the un-awaited-Task scope-exit drop
	// through the cooperative park-suspend join so the single-threaded WASM
	// scheduler can run the pending goroutine instead of livelocking.
	if _, isTask := types.AsAnyTask(valType); (isTask || (b.named != nil && types.IsTaskLikeOrigin(b.named))) &&
		c.emitTaskJoinAndFreeByDropFn(ptr, b.dropFunc) {
		c.block.NewBr(doneBlock)
		c.block = doneBlock
		c.block.NewBr(skipBlock)
		c.block = skipBlock
		return
	}

	c.block.NewCall(b.dropFunc, ptr)
	c.block.NewBr(doneBlock)

	c.block = doneBlock
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitStringDropOldValue conditionally drops the previous string at a non-local
// compound-assignment site. Mirrors the local-var drop pattern from T0357: the
// alias check is a no-op at runtime because promise_string_concat always
// allocates a fresh result (current and result never alias), but it keeps the
// emitted IR shape consistent across compound sites.
func (c *Compiler) emitStringDropOldValue(current, result value.Value) {
	dropFn, ok := c.funcs["promise_string_drop"]
	if !ok {
		return
	}
	diffBlk := c.newBlock("compound.strdrop.diff")
	mergeBlk := c.newBlock("compound.strdrop.merge")
	isSame := c.block.NewICmp(enum.IPredEQ, current, result)
	c.block.NewCondBr(isSame, mergeBlk, diffBlk)
	c.block = diffBlk
	c.block.NewCall(dropFn, current)
	c.block.NewBr(mergeBlk)
	c.block = mergeBlk
}

// emitDropOldCompoundValue drops the getter-returned `current` value of a
// read-modify-write compound assignment after the operator result is computed,
// for the getter-based sites ([] and [:]) where `current` is a freshly-returned
// Value rather than a value stored at a pointer (those sites use
// dropOldUserValueAtPtr instead). A non-`copy` getter must return a freshly
// allocated value (returning a field would move out of borrowed `this`), so the
// old `current` would otherwise leak. Handles string, heap user types, and
// droppable enums; alias-guarded against `result` so a no-op operator returning
// the same value isn't double-freed. Scalars / value types are no-ops. T0714.
//
// Caller caveat (T0987): Map.[] dups its string / enum-payload value but returns
// a heap USER-type value ALIASED to the map's stored instance. Since Map.[]=
// already drops the overwritten value, genMethodCompoundAssign must skip this
// call for the Map-aliased *droppable* heap-user-type case (see
// aliasedMapHeapValue) or the instance is freed twice. The pal_free-only shape
// (isHeapUserNoDropPalFree) still needs this call — for it the direct drop is
// the only thing that frees the aliased old instance.
func (c *Compiler) emitDropOldCompoundValue(current, result value.Value, operandType types.Type) {
	if operandType == nil {
		return
	}
	if c.typeSubst != nil {
		operandType = types.Substitute(operandType, c.typeSubst)
	}
	if c.selfSubst != nil {
		operandType = types.SubstituteSelf(operandType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if extractNamed(operandType) == types.TypString {
		c.emitStringDropOldValue(current, result)
		return
	}
	if isDroppableHeapUserType(operandType) || isHeapUserNoDropPalFree(operandType) {
		oldInstance := c.extractInstancePtr(current)
		newInstance := c.extractInstancePtr(result)
		isNull := c.block.NewICmp(enum.IPredEQ, oldInstance, constant.NewNull(irtypes.I8Ptr))
		isSame := c.block.NewICmp(enum.IPredEQ, oldInstance, newInstance)
		skipDrop := c.block.NewOr(isNull, isSame)
		dropBlock := c.newBlock("compound.userdrop")
		mergeBlock := c.newBlock("compound.userdrop.done")
		c.block.NewCondBr(skipDrop, mergeBlock, dropBlock)
		c.block = dropBlock
		c.emitVariantFieldDrop(current, operandType)
		c.block.NewBr(mergeBlock)
		c.block = mergeBlock
		return
	}
	// Non-`copy` enum operand: the operator returns a fresh value, so drop the
	// old one's variant data. Matches dropOldUserValueAtPtr's enum branch (a
	// copy / fieldless enum emits nothing).
	if extractEnum(operandType) != nil {
		c.emitVariantFieldDrop(current, operandType)
	}
}

// emitFreeCall emits a conditional pal_free call for a heap-allocated user type
// that has no drop method. Checks the drop flag and null-checks the instance pointer.
func (c *Compiler) emitFreeCall(b scopeBinding) {
	if b.dropFlag == nil {
		return
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	freeBlock := c.newBlock("free.call")
	skipBlock := c.newBlock("free.skip")
	c.block.NewCondBr(flag, freeBlock, skipBlock)

	c.block = freeBlock
	val := c.block.NewLoad(b.alloca.ElemType, b.alloca)
	// B0222: Raw i8* instance pointer (from promoted heapTemp). Value struct
	// allocas need extractInstancePtr to get field 1; i8* allocas are the pointer.
	var instance value.Value
	if b.alloca.ElemType == irtypes.I8Ptr {
		instance = val
	} else {
		instance = c.extractInstancePtr(val)
	}

	// Null-check: zero-initialized values from error handler fallthrough
	nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
	execBlock := c.newBlock("free.exec")
	doneBlock := c.newBlock("free.done")
	c.block.NewCondBr(nullCheck, doneBlock, execBlock)

	c.block = execBlock
	if b.dropFunc != nil {
		// T0127: Custom cleanup function (e.g., __promise_iter_cleanup for structural
		// interface variables from iterator chains). The cleanup function frees nested
		// allocations (closure env) and the instance itself.
		c.block.NewCall(b.dropFunc, instance)
	} else if b.rttiDrop {
		// B0272: Structural interface variables whose backing instance has RTTI layout.
		// Use RTTI-based drop dispatch to properly clean up all fields (e.g., string
		// fields) before freeing — raw pal_free would leak nested allocations.
		// Only set for bindings where the instance has standard RTTI (not _FnIter etc.).
		c.emitStructuralInstanceDrop(instance)
	} else {
		c.block.NewCall(c.palFree, instance)
	}
	c.block.NewBr(doneBlock)

	c.block = doneBlock
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitEnvFree frees a closure's env struct at scope exit.
// Checks the drop flag (has the closure been moved?) and null-checks the env pointer.
func (c *Compiler) emitEnvFree(b scopeBinding) {
	if b.dropFlag == nil {
		return
	}
	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	freeBlock := c.newBlock("env.free")
	skipBlock := c.newBlock("env.skip")
	c.block.NewCondBr(flag, freeBlock, skipBlock)

	c.block = freeBlock
	// Load closure, extract env ptr (field 1 of fat pointer)
	closure := c.block.NewLoad(b.alloca.ElemType, b.alloca)
	envPtr := c.block.NewExtractValue(closure, 1)
	// If non-null, free the env struct
	isNull := c.block.NewICmp(enum.IPredEQ, envPtr, constant.NewNull(irtypes.I8Ptr))
	callBlock := c.newBlock("env.free.call")
	c.block.NewCondBr(isNull, skipBlock, callBlock)

	c.block = callBlock
	c.emitEnvDropOrFree(envPtr)
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitEnvDropOrFree loads the env drop function from the env struct header (field 0)
// and calls it if non-null, otherwise calls pal_free. B0221: env structs now store
// a drop function pointer as their first field so captured moved values can be
// properly dropped (not just the env struct freed).
// The env pointer must be non-null (caller is responsible for null-checking).
func (c *Compiler) emitEnvDropOrFree(envPtr value.Value) {
	// Load env drop fn pointer from field 0 (env struct header)
	envHeaderType := irtypes.NewStruct(irtypes.I8Ptr)
	typedHdr := c.block.NewBitCast(envPtr, irtypes.NewPointer(envHeaderType))
	dropFnField := c.block.NewGetElementPtr(envHeaderType, typedHdr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	dropFnRaw := c.block.NewLoad(irtypes.I8Ptr, dropFnField)

	hasDrop := c.block.NewICmp(enum.IPredNE, dropFnRaw, constant.NewNull(irtypes.I8Ptr))
	callDropBlk := c.newBlock("env.deep_drop")
	justFreeBlk := c.newBlock("env.shallow_free")
	mergeBlk := c.newBlock("env.drop_done")
	c.block.NewCondBr(hasDrop, callDropBlk, justFreeBlk)

	// Call env drop function (drops captured values + frees env struct)
	c.block = callDropBlk
	envDropFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
	typedDropFn := c.block.NewBitCast(dropFnRaw, irtypes.NewPointer(envDropFnType))
	c.block.NewCall(typedDropFn, envPtr)
	c.block.NewBr(mergeBlk)

	// No droppable captures — just free the env struct
	c.block = justFreeBlk
	c.block.NewCall(c.palFree, envPtr)
	c.block.NewBr(mergeBlk)

	c.block = mergeBlk
}
