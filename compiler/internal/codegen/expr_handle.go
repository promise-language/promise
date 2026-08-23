package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// genChannelConstructor generates code for channel[T](capacity: n) or channel[T]().
// Calls @promise_channel_new(capacity, elem_size) → i8*.
func (c *Compiler) genChannelConstructor(e *ast.CallExpr, inst *types.Instance) value.Value {
	elemType := inst.TypeArgs()[0]
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	// capacity defaults to 0 (unbuffered) when no argument provided
	var capacity value.Value
	if len(e.Args) > 0 {
		capArg := c.genCallArgExpr(e.Args[0].Value)
		// Argument is int? — unwrap the optional to get the int value.
		// If it's a bare int literal, sema may pass it as int? via AssignableTo.
		argType := c.info.Types[e.Args[0].Value]
		if _, isOpt := argType.(*types.Optional); isOpt {
			// Extract value from { i1, i64 } optional — field 1
			capacity = c.block.NewExtractValue(capArg, 1)
		} else {
			capacity = capArg
		}
	} else {
		capacity = constant.NewInt(irtypes.I64, 0)
	}

	return c.block.NewCall(c.funcs["promise_channel_new"],
		capacity,
		constant.NewInt(irtypes.I64, elemSize))
}

// arcStructType returns the LLVM struct type for Ref[T]: {i64 strong_count, i64 weak_count, T value}.
// T0157: Arc layout includes weak_count for Weak[T] support.
func arcStructType(elemLLVM irtypes.Type) *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I64, irtypes.I64, elemLLVM)
}

// Arc struct field indices — T0157: weak_count added at field 1, value shifted to field 2.
const (
	arcFieldStrong = 0 // i64 strong_count
	arcFieldWeak   = 1 // i64 weak_count
	arcFieldValue  = 2 // T value
)

// genArcConstructor generates Ref[T](value) — allocates {strong_count, weak_count, T}, stores counts=1 and the value.
// T0155: Ref[T] atomic reference counting. T0157: weak_count added.
func (c *Compiler) genArcConstructor(e *ast.CallExpr, inst *types.Instance) value.Value {
	elemType := inst.TypeArgs()[0]
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	elemLLVM := c.resolveType(elemType)
	elemSize := c.typeSize(elemLLVM)
	_, elemIsOpt := elemType.(*types.Optional) // T0853

	// Allocate: 8 bytes strong_count + 8 bytes weak_count + sizeof(T)
	totalSize := 16 + elemSize
	arcPtr := c.block.NewCall(c.palAlloc, constant.NewInt(irtypes.I64, int64(totalSize)))

	// Bitcast to typed struct pointer for GEP
	arcStructTy := arcStructType(elemLLVM)
	typedPtr := c.block.NewBitCast(arcPtr, irtypes.NewPointer(arcStructTy))

	// Store strong_count = 1
	rcField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldStrong))
	c.block.NewStore(constant.NewInt(irtypes.I64, 1), rcField)

	// Store weak_count = 1 (the +1 represents all strong refs collectively)
	wcField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldWeak))
	c.block.NewStore(constant.NewInt(irtypes.I64, 1), wcField)

	// Generate and store value (moved into the Arc)
	// T0853: when the element type is Optional, set targetType so a bare `none`
	// arg lowers to a zero {i1,T} struct via genNoneLit (mirrors Vector.push, T0658).
	savedTarget := c.targetType
	if elemIsOpt {
		c.targetType = elemType
	}
	// T1003: track enum ctor temps created while evaluating the moved-in value so
	// their statement-end drop is suppressed — the value is owned by the Arc now,
	// dropped via the Arc's inner-drop when the last strong ref is released.
	savedEnumTemps := len(c.enumCtorTemps)
	val := c.genCallArgExpr(e.Args[0].Value)
	c.targetType = savedTarget
	c.claimHeapTemp(val)
	// T0273: Clear drop flag — value is moved into Arc, caller must not double-drop.
	if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	// T0784: also clear when the arg is `x as!/as T` over an owned local —
	// the cast is a non-consuming view, so without this the subject and the
	// new Arc both drop the same allocation. T0849: for the conditional `as`
	// form, drop iff the downcast failed.
	if ident := c.castSubjectMovableIdent(e.Args[0].Value); ident != nil {
		c.consumeCastSubjectDropFlag(e.Args[0].Value, ident.Name)
	}
	c.neutralizeForceUnwrapSource(e.Args[0].Value)
	c.claimStringTemp(val)
	c.claimEnvTemp(val)
	// T0853: widen a bare non-optional `T` arg to the `T?` element struct. Done
	// last (after temp-claiming) because stmtTempMap tracks by val-identity,
	// which is lost once val is wrapped. wrapReturnOptional doubles as the
	// constructor-arg widener: it no-ops for `none` (targetType already zeroed
	// it) and for an already-optional arg (types.Identical), else wrapOptional.
	if elemIsOpt {
		val = c.wrapReturnOptional(val, e.Args[0].Value, elemType)
	}
	valField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldValue))
	c.block.NewStore(val, valField)

	// T1003: suppress statement-end drop of enum ctor temps moved into the Arc.
	// T1139: gate on the moved value's static type being an enum — a non-enum
	// arg that merely BORROWS an inline Enum.V(x) temp in a sub-call leaves an
	// intermediate the Arc never owns; it must stay tracked so the caller drops
	// it at statement end, else it leaks.
	argEnumType := c.info.Types[e.Args[0].Value]
	if c.typeSubst != nil {
		argEnumType = types.Substitute(argEnumType, c.typeSubst)
	}
	if extractEnum(argEnumType) != nil {
		for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
		}
		c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
	}

	return arcPtr
}

// genChannelMethodCall dispatches native method calls on channel[T].
func (c *Compiler) genChannelMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	chRaw := c.genExprAutoPropagate(member.Target) // B0323
	chanType := channelStructType()
	chPtr := c.block.NewBitCast(chRaw, irtypes.NewPointer(chanType))
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	switch method {
	case "send":
		return c.genChannelSend(e, chRaw, chPtr, chanType, elemType, elemLLVM, elemSize)
	case "close":
		return c.genChannelClose(chRaw, chPtr, chanType)
	default:
		panic(fmt.Sprintf("codegen: unknown channel method %q", method))
	}
}

// genArcBorrow generates the Arc .borrow getter — loads and returns the inner T value.
// The Arc layout is { i64 strong_count, i64 weak_count, T value }. We GEP to field 2 and load the value.
// T0155: Ref[T] atomic reference counting. T0157: weak_count shifted value to field 2.
func (c *Compiler) genArcBorrow(e *ast.MemberExpr, elemType types.Type) value.Value {
	arcRaw := c.genExprAutoPropagate(e.Target) // B0323
	elemLLVM := c.resolveType(elemType)
	arcStructTy := arcStructType(elemLLVM)
	typedPtr := c.block.NewBitCast(arcRaw, irtypes.NewPointer(arcStructTy))
	valField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldValue))
	return c.block.NewLoad(elemLLVM, valField)
}

// genArcMethodCall dispatches native method calls on Ref[T].
// T0155: Ref[T] atomic reference counting. T0157: downgrade added.
func (c *Compiler) genArcMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	// T0500: suppress the receiver field-read dup — clone()/downgrade() perform
	// their own atomic increment that produces the caller's owning reference.
	// Without this, `owner._arcField.clone()` increments strong_count twice
	// (dup + method body) but the caller registers only one matching drop,
	// leaking +1.
	savedDup := c.dupContainerFieldAccess
	c.dupContainerFieldAccess = false
	arcRaw := c.genExprAutoPropagate(member.Target) // B0323
	c.dupContainerFieldAccess = savedDup

	switch method {
	case "clone":
		// Increment strong_count and return the same pointer (non-atomic when
		// the element type is `confined — T0995).
		rcPtr := c.block.NewBitCast(arcRaw, irtypes.NewPointer(irtypes.I64))
		c.emitRefCountAdd(c.block, rcPtr, 1, irtypes.I64, c.refIsAtomic(elemType))
		// T0499: Return a distinct SSA value so the clone result can be tracked
		// separately from the receiver's stmtTemp. Without this, stmtTemp dedup
		// causes the constructor intermediate to leak when used in a chain
		// (e.g., Ref[int](42).clone()). The ptrtoint+inttoptr is a no-op at
		// runtime — LLVM optimizes it away.
		tmpInt := c.block.NewPtrToInt(arcRaw, c.ptrIntType())
		return c.block.NewIntToPtr(tmpInt, irtypes.I8Ptr)
	case "downgrade":
		// T0157: Atomically increment weak_count, return same pointer as Weak[T]
		return c.genArcDowngrade(arcRaw, elemType)
	default:
		panic(fmt.Sprintf("codegen: unknown arc method %q", method))
	}
}

// genArcDowngrade generates Arc.downgrade() — increments weak_count and returns the pointer as Weak[T].
// T0157: Weak[T] references.
func (c *Compiler) genArcDowngrade(arcRaw value.Value, elemType types.Type) value.Value {
	elemLLVM := c.resolveType(elemType)
	arcStructTy := arcStructType(elemLLVM)
	typedPtr := c.block.NewBitCast(arcRaw, irtypes.NewPointer(arcStructTy))
	wcField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldWeak))
	c.emitRefCountAdd(c.block, wcField, 1, irtypes.I64, c.refIsAtomic(elemType))
	// T0499: fresh SSA value so downgrade result is tracked separately from receiver stmtTemp
	tmpInt := c.block.NewPtrToInt(arcRaw, c.ptrIntType())
	return c.block.NewIntToPtr(tmpInt, irtypes.I8Ptr)
}

// --- Weak[T] codegen (T0157) ---

// genWeakMethodCall dispatches native method calls on Weak[T].
// T0157: Weak[T] references — upgrade and clone.
func (c *Compiler) genWeakMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	// T0500: suppress the receiver field-read dup — clone()/upgrade() perform
	// their own atomic increment that produces the caller's owning reference.
	// Without this, `owner._weakField.clone()` increments weak_count twice
	// (dup + method body) but the caller registers only one matching drop,
	// leaking +1.
	savedDup := c.dupContainerFieldAccess
	c.dupContainerFieldAccess = false
	weakRaw := c.genExprAutoPropagate(member.Target) // B0323
	c.dupContainerFieldAccess = savedDup

	switch method {
	case "clone":
		// Atomically increment weak_count and return the same pointer
		elemLLVM := c.resolveType(elemType)
		arcStructTy := arcStructType(elemLLVM)
		typedPtr := c.block.NewBitCast(weakRaw, irtypes.NewPointer(arcStructTy))
		wcField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldWeak))
		c.emitRefCountAdd(c.block, wcField, 1, irtypes.I64, c.refIsAtomic(elemType))
		// T0499: fresh SSA value so clone result is tracked separately from receiver stmtTemp
		tmpInt := c.block.NewPtrToInt(weakRaw, c.ptrIntType())
		return c.block.NewIntToPtr(tmpInt, irtypes.I8Ptr)
	case "upgrade":
		// CAS loop: atomically try to increment strong_count if > 0
		return c.genWeakUpgrade(weakRaw, elemType)
	default:
		panic(fmt.Sprintf("codegen: unknown weak method %q", method))
	}
}

// genWeakUpgrade generates Weak.upgrade() — CAS loop on strong_count, returns Ref[T]?.
// T0157: Returns {i1, i8*} optional — Some(arc_ptr) if strong_count > 0, none otherwise.
func (c *Compiler) genWeakUpgrade(weakRaw value.Value, elemType types.Type) value.Value {
	elemLLVM := c.resolveType(elemType)
	arcStructTy := arcStructType(elemLLVM)
	optType := irtypes.NewStruct(irtypes.I1, irtypes.I8Ptr)

	typedPtr := c.block.NewBitCast(weakRaw, irtypes.NewPointer(arcStructTy))
	scField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldStrong))

	if c.isWasm || !c.refIsAtomic(elemType) {
		// WASM (single-threaded) or a `confined Ref (T0995): no atomics needed —
		// simple load+compare+store.
		old := c.block.NewLoad(irtypes.I64, scField)
		isZero := c.block.NewICmp(enum.IPredEQ, old, constant.NewInt(irtypes.I64, 0))
		noneBlk := c.newBlock("weak.upgrade.none")
		someBlk := c.newBlock("weak.upgrade.some")
		mergeBlk := c.newBlock("weak.upgrade.merge")
		c.block.NewCondBr(isZero, noneBlk, someBlk)

		c.block = noneBlk
		noneVal := constant.NewZeroInitializer(optType)
		noneBlk.NewBr(mergeBlk)

		c.block = someBlk
		newRC := someBlk.NewAdd(old, constant.NewInt(irtypes.I64, 1))
		someBlk.NewStore(newRC, scField)
		someVal := c.wrapOptional(weakRaw, optType)
		someBlk.NewBr(mergeBlk)

		c.block = mergeBlk
		return mergeBlk.NewPhi(
			ir.NewIncoming(noneVal, noneBlk),
			ir.NewIncoming(someVal, someBlk),
		)
	}

	// Native: CAS loop for thread safety
	//   loop:
	//     old = load atomic i64* strong_count acquire
	//     if old == 0: goto none
	//     new = old + 1
	//     {prev, ok} = cmpxchg i64* strong_count, old, new acq_rel monotonic
	//     if !ok: goto loop
	//     goto some
	loopBlk := c.newBlock("weak.upgrade.loop")
	noneBlk := c.newBlock("weak.upgrade.none")
	someBlk := c.newBlock("weak.upgrade.some")
	mergeBlk := c.newBlock("weak.upgrade.merge")
	c.block.NewBr(loopBlk)

	c.block = loopBlk
	old := loopBlk.NewLoad(irtypes.I64, scField)
	old.Atomic = true
	old.Ordering = enum.AtomicOrderingAcquire
	old.Align = 8 // LLVM requires explicit alignment for atomic load
	isZero := loopBlk.NewICmp(enum.IPredEQ, old, constant.NewInt(irtypes.I64, 0))
	casBlk := c.newBlock("weak.upgrade.cas")
	loopBlk.NewCondBr(isZero, noneBlk, casBlk)

	c.block = casBlk
	newRC := casBlk.NewAdd(old, constant.NewInt(irtypes.I64, 1))
	casResult := casBlk.NewCmpXchg(scField, old, newRC, enum.AtomicOrderingAcquireRelease, enum.AtomicOrderingMonotonic)
	casResult.Weak = false
	ok := casBlk.NewExtractValue(casResult, 1)
	casBlk.NewCondBr(ok, someBlk, loopBlk)

	c.block = noneBlk
	noneVal := constant.NewZeroInitializer(optType)
	noneBlk.NewBr(mergeBlk)

	c.block = someBlk
	someVal := c.wrapOptional(weakRaw, optType)
	someBlk.NewBr(mergeBlk)

	c.block = mergeBlk
	return mergeBlk.NewPhi(
		ir.NewIncoming(noneVal, noneBlk),
		ir.NewIncoming(someVal, someBlk),
	)
}

// --- Mutex[T] / MutexGuard[T] codegen (T0156) ---

// genMutexConstructor generates Mutex[T](value) — allocates scheduler-aware mutex struct, inits fields, stores value.
// Layout: {i8* pal_handle, i8* cond, i8* waiter_head, i8* waiter_tail, i8 held, T value}
// T0285: Scheduler-aware mutex — uses goroutine park/wake instead of blocking OS threads.
func (c *Compiler) genMutexConstructor(e *ast.CallExpr, inst *types.Instance) value.Value {
	elemType := inst.TypeArgs()[0]
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	elemLLVM := c.resolveType(elemType)
	_, elemIsOpt := elemType.(*types.Optional) // T0853

	mutexStructTy := mutexStructType(elemLLVM)
	mutexSize := c.typeSize(mutexStructTy)
	mutexPtr := c.block.NewCall(c.palAlloc, constant.NewInt(irtypes.I64, int64(mutexSize)))

	// Bitcast to typed struct pointer for GEP
	typedPtr := c.block.NewBitCast(mutexPtr, irtypes.NewPointer(mutexStructTy))

	// Field 0: PAL mutex handle (protects metadata only)
	mutexHandle := c.block.NewCall(c.palMutexInit)
	handleField := c.block.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(mutexHandle, handleField)

	// Field 1: condition variable (for non-coroutine waiters)
	condHandle := c.block.NewCall(c.palCondInit)
	condField := c.block.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	c.block.NewStore(condHandle, condField)

	// Field 2: waiter_head = null
	headField := c.block.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	c.block.NewStore(constant.NewNull(irtypes.I8Ptr), headField)

	// Field 3: waiter_tail = null
	tailField := c.block.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 3))
	c.block.NewStore(constant.NewNull(irtypes.I8Ptr), tailField)

	// Field 4: held = 0 (unlocked)
	heldField := c.block.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 4))
	c.block.NewStore(constant.NewInt(irtypes.I8, 0), heldField)

	// Field 5: user value (moved into the Mutex)
	// T0853: when the element type is Optional, set targetType so a bare `none`
	// arg lowers to a zero {i1,T} struct via genNoneLit (mirrors Vector.push, T0658).
	savedTarget := c.targetType
	if elemIsOpt {
		c.targetType = elemType
	}
	// T1003: track enum ctor temps created while evaluating the moved-in value so
	// their statement-end drop is suppressed — the value is owned by the Mutex now,
	// dropped via the Mutex's inner-drop when the Mutex is dropped.
	savedEnumTemps := len(c.enumCtorTemps)
	val := c.genCallArgExpr(e.Args[0].Value)
	c.targetType = savedTarget
	c.claimHeapTemp(val)
	// T0273: Clear drop flag — value is moved into Mutex, caller must not double-drop.
	if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	// T0784: also clear when the arg is `x as!/as T` over an owned local.
	// T0849: for the conditional `as` form, drop iff the downcast failed.
	if ident := c.castSubjectMovableIdent(e.Args[0].Value); ident != nil {
		c.consumeCastSubjectDropFlag(e.Args[0].Value, ident.Name)
	}
	c.neutralizeForceUnwrapSource(e.Args[0].Value)
	c.claimStringTemp(val)
	c.claimEnvTemp(val)
	// T0853: widen a bare non-optional `T` arg to the `T?` element struct. Done
	// last (after temp-claiming) because stmtTempMap tracks by val-identity,
	// which is lost once val is wrapped. wrapReturnOptional doubles as the
	// constructor-arg widener: it no-ops for `none` (targetType already zeroed
	// it) and for an already-optional arg (types.Identical), else wrapOptional.
	if elemIsOpt {
		val = c.wrapReturnOptional(val, e.Args[0].Value, elemType)
	}
	valField := c.block.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldValue)))
	c.block.NewStore(val, valField)

	// T1003: suppress statement-end drop of enum ctor temps moved into the Mutex.
	// T1139: gate on the moved value's static type being an enum — a non-enum
	// arg that merely BORROWS an inline Enum.V(x) temp in a sub-call leaves an
	// intermediate the Mutex never owns; it must stay tracked so the caller drops
	// it at statement end, else it leaks.
	argEnumType := c.info.Types[e.Args[0].Value]
	if c.typeSubst != nil {
		argEnumType = types.Substitute(argEnumType, c.typeSubst)
	}
	if extractEnum(argEnumType) != nil {
		for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
		}
		c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
	}

	return mutexPtr
}

// genMutexMethodCall dispatches native method calls on Mutex[T].
func (c *Compiler) genMutexMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	mutexRaw := c.genExprAutoPropagate(member.Target)

	switch method {
	case "lock":
		// T0655: a single-owner Mutex *temp* receiver would be dropped at
		// statement end before the MutexGuard that borrows it → UAF. Promote
		// it to a scope binding so it outlives the guard, mirroring the
		// already-correct bound-receiver path. No-op for bound receivers
		// (mutexRaw is a fresh load, not a tracked stmt-temp).
		mtxType := c.info.Types[member.Target]
		if c.typeSubst != nil && mtxType != nil {
			mtxType = types.Substitute(mtxType, c.typeSubst)
		}
		c.promoteHandleTempToScopeBinding(mutexRaw, c.getOrCreateMutexDrop(elemType), mtxType)
		return c.genMutexLock(mutexRaw, elemType)
	default:
		panic(fmt.Sprintf("codegen: unknown mutex method %q", method))
	}
}

// genMutexGuardMethodCall dispatches native method calls on MutexGuard[T] (T0839).
// close(~this): unlock the mutex and free the guard via the canonical unlock+free
// body (MutexGuard.drop), then suppress the automatic scope-exit/stmt-temp drop so
// the guard isn't double-freed/double-unlocked.
func (c *Compiler) genMutexGuardMethodCall(e *ast.CallExpr, member *ast.MemberExpr, method string) value.Value {
	switch method {
	case "close":
		guardRaw := c.genExprAutoPropagate(member.Target) // B0323
		// Same body as MutexGuard.drop: scheduler-aware unlock + free guard (T0156).
		// It null-checks internally, so an already-null guard is safe.
		c.block.NewCall(c.funcs["MutexGuard.drop"], guardRaw)
		// The guard is consumed. Suppress later automatic cleanup:
		//  - bound source (`g := m.lock(); g.close();`): clear the drop binding flag.
		//  - temp/chain source (`m.lock().close()`, `(h.mtx!).lock().close()`): release
		//    the stmt-temp tracking. Both calls are no-ops when not applicable.
		if ident, ok := member.Target.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
		c.claimStringTemp(guardRaw)
		return nil // close(~this) returns void (cf. genChannelClose)
	default:
		panic(fmt.Sprintf("codegen: unknown MutexGuard method %q", method))
	}
}

// genMutexLock generates Mutex.lock() — scheduler-aware lock, returns a MutexGuard.
// T0285: Coroutine path uses goroutine park/wake; non-coroutine path uses cond_wait.
// Guard layout: {i8* mutex_alloc_ptr}.
func (c *Compiler) genMutexLock(mutexRaw value.Value, elemType types.Type) value.Value {
	metaTy := mutexMetaType()
	typedPtr := c.block.NewBitCast(mutexRaw, irtypes.NewPointer(metaTy))

	// Load PAL mutex handle and enter critical section
	handleField := c.block.NewGetElementPtr(metaTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldHandle)))
	handle := c.block.NewLoad(irtypes.I8Ptr, handleField)
	c.block.NewCall(c.palMutexLock, handle)

	// Load held flag and waiter head. Acquired iff held==0 AND waiter_head==null.
	// Queuing behind existing waiters prevents newcomer starvation under contention:
	// pthread_mutex is not FIFO, so an arrival that races with a handoff could
	// otherwise win the PAL handle repeatedly and starve parked waiters (T0301).
	heldField := c.block.NewGetElementPtr(metaTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldHeld)))
	held := c.block.NewLoad(irtypes.I8, heldField)
	isHeld := c.block.NewICmp(enum.IPredEQ, held, constant.NewInt(irtypes.I8, 1))

	waiterHeadReadField := c.block.NewGetElementPtr(metaTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldWaiterHead)))
	waiterHeadRead := c.block.NewLoad(irtypes.I8Ptr, waiterHeadReadField)
	hasWaiter := c.block.NewICmp(enum.IPredNE, waiterHeadRead, constant.NewNull(irtypes.I8Ptr))
	mustWait := c.block.NewOr(isHeld, hasWaiter)

	acquiredBlk := c.newBlock("mutex.acquired")
	contestedBlk := c.newBlock("mutex.contested")
	c.block.NewCondBr(mustWait, contestedBlk, acquiredBlk)

	// acquired: held=0 → set held=1, unlock metadata mutex, allocate guard
	c.block = acquiredBlk
	acquiredHeldField := c.block.NewGetElementPtr(metaTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldHeld)))
	c.block.NewStore(constant.NewInt(irtypes.I8, 1), acquiredHeldField)
	c.block.NewCall(c.palMutexUnlock, handle)

	guardBlk := c.newBlock("mutex.guard")
	c.block.NewBr(guardBlk)

	// contested: held=1 → need to wait
	c.block = contestedBlk
	if c.inCoroutine {
		// Goroutine mode: park on mutex waiter list (park-and-wake, not spin-yield).
		// PAL handle is still locked at entry here.
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		waiterHeadField := c.block.NewGetElementPtr(metaTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldWaiterHead)))
		waiterTailField := c.block.NewGetElementPtr(metaTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldWaiterTail)))
		c.block.NewCall(c.funcs["promise_waiter_enqueue"], waiterHeadField, waiterTailField, currentG)
		// Store mutex in G.park_mutex — scheduler releases after coro.suspend completes
		gTyMtx := goroutineStructType()
		mtxGPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTyMtx))
		mtxPmField := c.block.NewGetElementPtr(gTyMtx, mtxGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(handle, mtxPmField)

		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		parkResumeBlk := c.newBlock("mutex.park.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), parkResumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		// Resume: lock was handed off (held=1 already), go directly to guardBlk
		c.block = parkResumeBlk
		c.block.NewBr(guardBlk)
	} else {
		// Non-coroutine mode: wait loop
		waitLoopBlk := c.newBlock("mutex.wait.loop")
		c.block.NewBr(waitLoopBlk)

		c.block = waitLoopBlk
		loopHeldField := c.block.NewGetElementPtr(metaTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldHeld)))
		loopHeld := c.block.NewLoad(irtypes.I8, loopHeldField)
		stillHeld := c.block.NewICmp(enum.IPredEQ, loopHeld, constant.NewInt(irtypes.I8, 1))

		waitBodyBlk := c.newBlock("mutex.wait.body")
		waitDoneBlk := c.newBlock("mutex.wait.done")
		c.block.NewCondBr(stillHeld, waitBodyBlk, waitDoneBlk)

		c.block = waitBodyBlk
		if c.isWasm {
			// T1218: pump the cooperative scheduler instead of the no-op pal_cond_wait
			// so a non-coroutine mutex waiter (e.g. a named fn spawned via `go`) yields
			// to the holder G; on progress branch back to the loop header to recheck
			// `held`. Sibling of the T1200 channel fix.
			c.emitWasmCoopWaitPump(waitLoopBlk)
		} else {
			// Thread-blocking mode: cond_wait (with syscall handoff, T1685), then
			// re-check held.
			condField := c.block.NewGetElementPtr(metaTy, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldCond)))
			cond := c.block.NewLoad(irtypes.I8Ptr, condField)
			c.emitBlockingCondWait(cond, handle)
			c.block.NewBr(waitLoopBlk)
		}

		c.block = waitDoneBlk
		doneHeldField := c.block.NewGetElementPtr(metaTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldHeld)))
		c.block.NewStore(constant.NewInt(irtypes.I8, 1), doneHeldField)
		c.block.NewCall(c.palMutexUnlock, handle)
		c.block.NewBr(guardBlk)
	}

	// Allocate guard: {i8*} — pointer back to the Mutex allocation
	c.block = guardBlk
	guardPtr := c.block.NewCall(c.palAlloc, constant.NewInt(irtypes.I64, 8))
	guardStructTy := irtypes.NewStruct(irtypes.I8Ptr)
	guardTypedPtr := c.block.NewBitCast(guardPtr, irtypes.NewPointer(guardStructTy))
	mutexField := c.block.NewGetElementPtr(guardStructTy, guardTypedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(mutexRaw, mutexField)

	return guardPtr
}

// genMutexGuardBorrow generates the MutexGuard .borrow getter — loads T from the Mutex through the guard.
// Guard layout: {i8* mutex_alloc_ptr}. Mutex layout: {i8* pal_handle, i8* cond, i8* waiter_head, i8* waiter_tail, i8 held, T value}.
func (c *Compiler) genMutexGuardBorrow(e *ast.MemberExpr, elemType types.Type) value.Value {
	guardRaw := c.genExprAutoPropagate(e.Target)
	elemLLVM := c.resolveType(elemType)

	// Load mutex_alloc_ptr from guard (field 0)
	guardStructTy := irtypes.NewStruct(irtypes.I8Ptr)
	guardPtr := c.block.NewBitCast(guardRaw, irtypes.NewPointer(guardStructTy))
	mutexPtrField := c.block.NewGetElementPtr(guardStructTy, guardPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	mutexRaw := c.block.NewLoad(irtypes.I8Ptr, mutexPtrField)

	// Load T from Mutex field 5 (value)
	mutexStructTy := mutexStructType(elemLLVM)
	mutexPtr := c.block.NewBitCast(mutexRaw, irtypes.NewPointer(mutexStructTy))
	valField := c.block.NewGetElementPtr(mutexStructTy, mutexPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldValue)))
	return c.block.NewLoad(elemLLVM, valField)
}

// genMutexGuardBorrowSet generates the MutexGuard .borrow setter — stores T into the Mutex through the guard.
// Handles compound assignment (+=, -=, etc.) by reading the current value first.
// srcExpr (may be nil) is the RHS source AST; used by the T0351 defensive dup
// path to detect a borrow-param string and dup it before store.
func (c *Compiler) genMutexGuardBorrowSet(target *ast.MemberExpr, op ast.AssignOp, val value.Value, elemType types.Type, srcExpr ast.Expr) {
	guardRaw := c.genExpr(target.Target)
	elemLLVM := c.resolveType(elemType)

	// Navigate to the value field: guard → mutex_alloc_ptr → Mutex.value
	guardStructTy := irtypes.NewStruct(irtypes.I8Ptr)
	guardPtr := c.block.NewBitCast(guardRaw, irtypes.NewPointer(guardStructTy))
	mutexPtrField := c.block.NewGetElementPtr(guardStructTy, guardPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	mutexRaw := c.block.NewLoad(irtypes.I8Ptr, mutexPtrField)

	mutexStructTy := mutexStructType(elemLLVM)
	mutexPtr := c.block.NewBitCast(mutexRaw, irtypes.NewPointer(mutexStructTy))
	valField := c.block.NewGetElementPtr(mutexStructTy, mutexPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldValue)))

	// Handle compound assignment
	if op != ast.OpAssign {
		current := c.block.NewLoad(elemLLVM, valField)
		val = c.genCompoundOp(op, elemType, current, val)
	}

	// Drop old value if T is droppable (T0270)
	c.block = c.emitInnerDrop(c.block, mutexPtr, mutexStructTy, elemType, mutexFieldValue)

	// T0351: defensive dup for borrow-param strings. The sema layer already
	// rejects `g.borrow = borrow_param` via tryMoveConsume in checkAssignStmt,
	// but mirror the T0095 string-field setter pattern as a runtime safety net
	// in case a future codegen path bypasses sema.
	if op == ast.OpAssign && srcExpr != nil && extractNamed(elemType) == types.TypString {
		if ident, ok := srcExpr.(*ast.IdentExpr); ok {
			if _, hasFlag := c.dropFlags[ident.Name]; hasFlag {
				c.clearDropFlag(ident.Name)
			} else {
				val = c.dupString(val)
			}
		}
	}

	c.block.NewStore(val, valField)
}

// emitWasmCoopWaitPump emits the WASM non-coroutine blocking-op wait body
// (T1200 channels, T1218 mutex).
//
// On single-threaded WASM pal_cond_wait and pal_mutex_lock/unlock are no-ops, so
// a plain (non-coroutine) function's blocking wait — the classic
// `wait_body: cond_wait; br recheck` loop — busy-spins forever without ever
// yielding to the cooperative scheduler. This happens whenever a blocking op
// (a channel send/recv, or a contended Mutex[T].lock()) runs inside a function
// that is NOT itself a coroutine, e.g. a named top-level function spawned via
// `go worker(c)`: the goroutine coroutine just calls the single, non-coroutine
// `@__user.worker`, whose send/recv/lock takes this thread-blocking branch. The
// partner G (the holder that would release, or the peer that would send) never
// runs (livelock, zero progress) and the per-test deadline (checked in
// promise_sched_coop_step's ran_g) never fires because coop_step is never
// re-entered.
//
// Fix: pump one cooperative step instead of the no-op wait, exactly mirroring the
// Task-receive (genReceiveTask) and Task-drop (defineTaskDropBody) WASM spins
// (T0668/T0687). promise_sched_coop_step returns i8:
//
//	2 = per-test deadline reached → clean early-return from this (non-coroutine)
//	    function, unwinding so the outer coop_step/coop_run regains control and
//	    renders TIMEOUT. The op is abandoned (result discarded, drops skipped —
//	    the test is being torn down; a timed-out test result==2 skips the leak
//	    check, matching the Task-spin "G intentionally not freed" precedent).
//	non-zero = a G ran (progress possible) → re-evaluate the wait condition.
//	0 = no runnable G and the condition is still unmet → nothing can ever change
//	    it → genuine deadlock → terminal message + exit(2) (same as coop_run).
//
// Must be called with c.block == the wait-body block. On progress it branches to
// recheck; the caller resumes building at recheck (which re-tests the condition).
func (c *Compiler) emitWasmCoopWaitPump(recheck *ir.Block) {
	stepR := c.block.NewCall(c.funcs["promise_sched_coop_step"])
	isTimeout := c.block.NewICmp(enum.IPredEQ, stepR, constant.NewInt(irtypes.I8, 2))
	timeoutBlk := c.newBlock("chwait.timeout")
	progressBlk := c.newBlock("chwait.progress")
	c.block.NewCondBr(isTimeout, timeoutBlk, progressBlk)

	// timeout: clean early-return from the non-coroutine function (panicExitBlock
	// is nil here — a plain function, not a coroutine body — so a ret is valid).
	c.block = timeoutBlk
	if _, isVoid := c.fn.Sig.RetType.(*irtypes.VoidType); isVoid {
		c.block.NewRet(nil)
	} else {
		c.block.NewRet(c.zeroValue(c.fn.Sig.RetType))
	}

	// progress vs deadlock
	c.block = progressBlk
	deadlockBlk := c.newBlock("chwait.deadlock")
	madeProgress := c.block.NewICmp(enum.IPredNE, stepR, constant.NewInt(irtypes.I8, 0))
	c.block.NewCondBr(madeProgress, recheck, deadlockBlk)

	// deadlock: no runnable G and condition unmet — terminal (mirrors coop_run).
	c.block = deadlockBlk
	dlMsg := c.getTaskDeadlockMsgGlobal()
	dlMsgPtr := c.block.NewGetElementPtr(dlMsg.ContentType, dlMsg,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewCall(c.palWrite, constant.NewInt(irtypes.I32, 2), dlMsgPtr,
		constant.NewInt(irtypes.I64, 45))
	c.block.NewCall(c.palExit, constant.NewInt(irtypes.I32, 2))
	c.block.NewUnreachable()
}

// genChannelSend generates code for ch.send(value).
// lock → wait-if-full → memcpy to buffer → signal → rendezvous wait if unbuffered → unlock
func (c *Compiler) genChannelSend(e *ast.CallExpr, chRaw value.Value, chPtr value.Value, chanType *irtypes.StructType, elemType types.Type, elemLLVM irtypes.Type, elemSize int64) value.Value {
	// Load mutex
	mtxFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	mtx := c.block.NewLoad(irtypes.I8Ptr, mtxFieldPtr)

	// Load cond vars
	neFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	notEmpty := c.block.NewLoad(irtypes.I8Ptr, neFieldPtr)

	nfFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	notFull := c.block.NewLoad(irtypes.I8Ptr, nfFieldPtr)

	// Lock mutex
	c.block.NewCall(c.palMutexLock, mtx)

	// Check closed before sending — panic if channel is closed
	closedPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
	closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	isClosed := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 1))

	sendClosedPanicBlock := c.newBlock("send.closed.panic")
	sendOkBlock := c.newBlock("send.ok")
	c.block.NewCondBr(isClosed, sendClosedPanicBlock, sendOkBlock)

	c.block = sendClosedPanicBlock
	c.block.NewCall(c.palMutexUnlock, mtx)
	panicMsg := c.makeGlobalString("send on closed channel")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.emitPanicReturn()

	c.block = sendOkBlock

	// Wait while full: while count == capacity
	waitFullBlock := c.newBlock("send.waitfull")
	waitFullClosedBlock := c.newBlock("send.waitfull.closed")
	writeBlock := c.newBlock("send.write")

	c.block.NewBr(waitFullBlock)

	// waitfull: check count == capacity
	c.block = waitFullBlock
	countPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
	count := c.block.NewLoad(irtypes.I64, countPtr)
	capPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
	cap_ := c.block.NewLoad(irtypes.I64, capPtr)
	isFull := c.block.NewICmp(enum.IPredEQ, count, cap_)

	waitFullBodyBlock := c.newBlock("send.waitfull.body")
	c.block.NewCondBr(isFull, waitFullBodyBlock, writeBlock)

	if c.inCoroutine {
		// Goroutine mode: park on send_waiters + coro.suspend
		c.block = waitFullBodyBlock
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		sendHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
		sendTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
		c.block.NewCall(c.funcs["promise_waiter_enqueue"], sendHeadPtr, sendTailPtr, currentG)
		// Store mutex in G.park_mutex — scheduler releases after coro.suspend completes
		gTySend := goroutineStructType()
		sendGPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTySend))
		sendPmField := c.block.NewGetElementPtr(gTySend, sendGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(mtx, sendPmField)

		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock("send.wait.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		// On resume: re-lock and check closed, then retry
		c.block = resumeBlk
		c.block.NewCall(c.palMutexLock, mtx)
		closedAfterWait := c.block.NewLoad(irtypes.I8, closedPtr)
		isClosedAfterWait := c.block.NewICmp(enum.IPredEQ, closedAfterWait, constant.NewInt(irtypes.I8, 1))
		c.block.NewCondBr(isClosedAfterWait, waitFullClosedBlock, waitFullBlock)
	} else if c.isWasm {
		// T1200: pump the cooperative scheduler instead of the no-op cond_wait, then
		// re-check the closed flag (as the cond_wait path does) before re-testing full.
		c.block = waitFullBodyBlock
		wfRecheck := c.newBlock("send.waitfull.recheck")
		c.emitWasmCoopWaitPump(wfRecheck)
		c.block = wfRecheck
		closedAfterWait := c.block.NewLoad(irtypes.I8, closedPtr)
		isClosedAfterWait := c.block.NewICmp(enum.IPredEQ, closedAfterWait, constant.NewInt(irtypes.I8, 1))
		c.block.NewCondBr(isClosedAfterWait, waitFullClosedBlock, waitFullBlock)
	} else {
		// Thread-blocking mode: cond_wait (with syscall handoff, T1685), then
		// re-check closed flag
		c.block = waitFullBodyBlock
		c.emitBlockingCondWait(notFull, mtx)
		closedAfterWait := c.block.NewLoad(irtypes.I8, closedPtr)
		isClosedAfterWait := c.block.NewICmp(enum.IPredEQ, closedAfterWait, constant.NewInt(irtypes.I8, 1))
		c.block.NewCondBr(isClosedAfterWait, waitFullClosedBlock, waitFullBlock)
	}

	// waitfull.closed: channel was closed while we were waiting — panic
	c.block = waitFullClosedBlock
	c.block.NewCall(c.palMutexUnlock, mtx)
	panicMsg2 := c.makeGlobalString("send on closed channel")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg2)
	c.emitPanicReturn()

	// write: memcpy value into buffer[tail * elem_size]
	c.block = writeBlock

	// Alloca value and store (entry-block alloca to avoid stack growth in loops)
	// T1221: send takes ownership (`T move value`) but memcpy's the raw value with
	// no clone. When the arg is a field read on a droppable owner (out.send(this.label)
	// / out.send(b.label)), the buffered pointer aliases the owner's inner buffer — the
	// owner's drop then frees a value the channel still owns → UAF/double-free. Arm the
	// same dup-on-read the general `~`/move-param call path uses (T0366), then clear the
	// flags (mirrors genCallArgsWithMutRef). For a plain owned local this is a no-op, so
	// the existing move-and-clear behavior below is preserved.
	c.maybeEnableDupForMutRefArg(e.Args[0].Value, elemType)
	argVal := c.genCallArgExpr(e.Args[0].Value)
	c.dupStringFieldAccess = false
	c.dupContainerFieldAccess = false
	c.dupHeapUserFieldAccess = false
	// T1174 parity: deep-clone a match-borrowed Optional[heap-user] payload alias.
	argVal, _ = c.dupBorrowedHeapUserPayload(e.Args[0].Value, argVal)
	// Clear drop flag: value is moved into the channel buffer
	if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	// T0784: also clear when the arg is `x as!/as T` over an owned local.
	// T0849: for the conditional `as` form, drop iff the downcast failed.
	if ident := c.castSubjectMovableIdent(e.Args[0].Value); ident != nil {
		c.consumeCastSubjectDropFlag(e.Args[0].Value, ident.Name)
	}
	// B0170: claim string temp — ownership transfers to channel buffer
	c.claimStringTemp(argVal)
	// B0233: claim heap temp — ownership transfers to channel buffer
	c.claimHeapTemp(argVal)
	// T1221: when the arg is an Optional[string]/Optional[container] field dup
	// (out.send(this.maybe_label)), the inner dup pointer is tracked separately —
	// claim it so the caller's statement cleanup doesn't double-free the value the
	// channel now owns. Mirrors genCallArgsWithMutRef's move-param handling (T0522).
	if c.optionalStringDup != nil {
		c.claimStringTemp(c.optionalStringDup)
		c.optionalStringDup = nil
	}
	if c.optionalContainerDup != nil {
		c.claimStringTemp(c.optionalContainerDup)
		c.optionalContainerDup = nil
	}
	argAlloca := c.createEntryAlloca(elemLLVM)
	c.block.NewStore(argVal, argAlloca)
	argAsI8 := c.block.NewBitCast(argAlloca, irtypes.I8Ptr)

	// Calculate dest = buffer + tail * elem_size
	bufPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
	buf := c.block.NewLoad(irtypes.I8Ptr, bufPtr)
	tailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldTail)))
	tail := c.block.NewLoad(irtypes.I64, tailPtr)
	offset := c.block.NewMul(tail, constant.NewInt(irtypes.I64, elemSize))
	dest := c.block.NewGetElementPtr(irtypes.I8, buf, offset)

	// memcpy(dest, &value, elem_size)
	c.block.NewCall(c.funcs["llvm.memcpy"], dest, argAsI8,
		constant.NewInt(irtypes.I64, elemSize), constant.False)

	// tail = (tail + 1) % capacity
	capReload := c.block.NewLoad(irtypes.I64, capPtr)
	tailPlusOne := c.block.NewAdd(tail, constant.NewInt(irtypes.I64, 1))
	newTail := c.block.NewURem(tailPlusOne, capReload)
	c.block.NewStore(newTail, tailPtr)

	// count++
	countReload := c.block.NewLoad(irtypes.I64, countPtr)
	newCount := c.block.NewAdd(countReload, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(newCount, countPtr)

	// Wake a waiting receiver (handles both regular G and select SWN nodes)
	recvHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
	recvTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_one"], recvHeadPtr, recvTailPtr, notEmpty)

	// If unbuffered: wait until receiver picks up the value
	unbufPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldUnbuffered)))
	unbufVal := c.block.NewLoad(irtypes.I8, unbufPtr)
	isUnbuf := c.block.NewICmp(enum.IPredEQ, unbufVal, constant.NewInt(irtypes.I8, 1))

	rendezvousBlock := c.newBlock("send.rendezvous")
	doneBlock := c.newBlock("send.done")
	c.block.NewCondBr(isUnbuf, rendezvousBlock, doneBlock)

	// rendezvous: wait while count > 0 && !closed
	c.block = rendezvousBlock
	rendezvousCheckBlock := c.newBlock("send.rv.check")
	c.block.NewBr(rendezvousCheckBlock)

	c.block = rendezvousCheckBlock
	rvCount := c.block.NewLoad(irtypes.I64, countPtr)
	rvHasItems := c.block.NewICmp(enum.IPredUGT, rvCount, constant.NewInt(irtypes.I64, 0))
	rvClosedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	isOpen := c.block.NewICmp(enum.IPredEQ, rvClosedVal, constant.NewInt(irtypes.I8, 0))
	shouldWait := c.block.NewAnd(rvHasItems, isOpen)

	rendezvousWaitBlock := c.newBlock("send.rv.wait")
	// When rendezvous exits (count==0 or closed), wake one write-waiter from
	// send_waiters so it can write to the now-empty buffer (B0156, T0305).
	rendezvousExitBlock := c.newBlock("send.rv.exit")
	c.block.NewCondBr(shouldWait, rendezvousWaitBlock, rendezvousExitBlock)

	if c.inCoroutine {
		// Goroutine mode rendezvous: park on rv_waiters (T0312).
		// Enqueue G on rv_waiters while ch.mutex is locked, then set park_mutex so
		// the scheduler unlocks ch.mutex after coro.suspend completes. The receiver
		// wakes us (via wake_one(rv_waiters)) only after count-- (count==0), so no
		// re-check is needed on resume — go directly to rendezvousExitBlock.
		c.block = rendezvousWaitBlock
		rvCurrentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		rvHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersHead)))
		rvTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersTail)))
		c.block.NewCall(c.funcs["promise_waiter_enqueue"], rvHeadPtr, rvTailPtr, rvCurrentG)
		rvGTy := goroutineStructType()
		rvGPtr := c.block.NewBitCast(rvCurrentG, irtypes.NewPointer(rvGTy))
		rvPmField := c.block.NewGetElementPtr(rvGTy, rvGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(mtx, rvPmField)
		rvSuspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		rvResumeBlk := c.newBlock("send.rv.resume")
		c.block.NewSwitch(rvSuspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), rvResumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		// Scheduler unlocked ch.mutex via park_mutex; re-lock to proceed.
		c.block = rvResumeBlk
		c.block.NewCall(c.palMutexLock, mtx)
		c.block.NewBr(rendezvousExitBlock)
	} else if c.isWasm {
		// T1200: pump the cooperative scheduler instead of the no-op cond_wait so a
		// non-coroutine sender (e.g. a named fn spawned via `go`) yields to its
		// receiver; on progress recheck the rendezvous condition.
		c.block = rendezvousWaitBlock
		c.emitWasmCoopWaitPump(rendezvousCheckBlock)
	} else {
		// Thread-blocking mode rendezvous: cond_wait (with syscall handoff, T1685)
		c.block = rendezvousWaitBlock
		c.emitBlockingCondWait(notFull, mtx)
		c.block.NewBr(rendezvousCheckBlock)
	}

	// rendezvous exit: wake one write-waiter from send_waiters (T0305/T0312).
	// rv_waiters holds rendezvous-parked senders; send_waiters holds only genuine
	// write-waiters and select SWNs, so waking it here is safe.
	c.block = rendezvousExitBlock
	rvExitSendHead := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
	rvExitSendTail := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
	rvExitNfPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	rvExitNf := c.block.NewLoad(irtypes.I8Ptr, rvExitNfPtr)
	c.block.NewCall(c.funcs["promise_waiter_wake_one"], rvExitSendHead, rvExitSendTail, rvExitNf)
	c.block.NewBr(doneBlock)

	// done: unlock
	c.block = doneBlock
	c.block.NewCall(c.palMutexUnlock, mtx)

	return nil
}

// genChannelClose generates code for ch.close().
// lock → set closed=1 → broadcast both conds → unlock
func (c *Compiler) genChannelClose(chRaw value.Value, chPtr value.Value, chanType *irtypes.StructType) value.Value {
	// Load mutex
	mtxFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	mtx := c.block.NewLoad(irtypes.I8Ptr, mtxFieldPtr)

	// Lock
	c.block.NewCall(c.palMutexLock, mtx)

	// Check if already closed — panic on double-close
	closedPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
	closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	alreadyClosed := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 1))

	doubleClosePanic := c.newBlock("close.panic")
	closeOk := c.newBlock("close.ok")
	c.block.NewCondBr(alreadyClosed, doubleClosePanic, closeOk)

	c.block = doubleClosePanic
	c.block.NewCall(c.palMutexUnlock, mtx)
	panicMsg := c.makeGlobalString("close of closed channel")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.emitPanicReturn()

	c.block = closeOk

	// Set closed = 1
	c.block.NewStore(constant.NewInt(irtypes.I8, 1), closedPtr)

	// Wake all goroutine waiters (send + recv)
	sendHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
	sendTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_all"], sendHeadPtr, sendTailPtr)

	recvHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
	recvTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_all"], recvHeadPtr, recvTailPtr)

	// Wake all rendezvous-parked senders (T0312): channel closed while they waited
	closeRvHead := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersHead)))
	closeRvTail := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_all"], closeRvHead, closeRvTail)

	// Broadcast both cond vars to wake thread-blocked waiters
	neFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	notEmpty := c.block.NewLoad(irtypes.I8Ptr, neFieldPtr)
	c.block.NewCall(c.palCondBroadcast, notEmpty)

	nfFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	notFull := c.block.NewLoad(irtypes.I8Ptr, nfFieldPtr)
	c.block.NewCall(c.palCondBroadcast, notFull)

	// Unlock
	c.block.NewCall(c.palMutexUnlock, mtx)

	return nil
}
