package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// --- Mutex[T] field layout (T0285: scheduler-aware) ---

// Mutex struct field indices.
// Layout: {i8* pal_handle, i8* cond, i8* waiter_head, i8* waiter_tail, i8 held, T value}
const (
	mutexFieldHandle     = 0 // i8*  PAL mutex (protects metadata only)
	mutexFieldCond       = 1 // i8*  cond var for non-coroutine waiters
	mutexFieldWaiterHead = 2 // i8*  head of parked goroutine waiter list
	mutexFieldWaiterTail = 3 // i8*  tail of parked goroutine waiter list
	mutexFieldHeld       = 4 // i8   0=unlocked, 1=locked
	mutexFieldValue      = 5 // T    the protected value
)

// mutexStructType returns the LLVM struct type for a Mutex[T] with the given element type.
func mutexStructType(elemLLVM irtypes.Type) *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I8Ptr, // pal_handle
		irtypes.I8Ptr, // cond
		irtypes.I8Ptr, // waiter_head
		irtypes.I8Ptr, // waiter_tail
		irtypes.I8,    // held
		elemLLVM,      // value
	)
}

// mutexMetaType returns the T-independent metadata portion of the Mutex struct.
// Used by MutexGuard unlock which doesn't need to know T.
func mutexMetaType() *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I8Ptr, // pal_handle
		irtypes.I8Ptr, // cond
		irtypes.I8Ptr, // waiter_head
		irtypes.I8Ptr, // waiter_tail
		irtypes.I8,    // held
	)
}

// --- Channel infrastructure ---

// Channel struct field indices.
const (
	chanFieldBuffer     = 0  // i8*  ring buffer
	chanFieldElemSize   = 1  // i64  element size
	chanFieldCapacity   = 2  // i64  ring buffer capacity (always >= 1)
	chanFieldCount      = 3  // i64  current element count
	chanFieldHead       = 4  // i64  read index
	chanFieldTail       = 5  // i64  write index
	chanFieldClosed     = 6  // i8   0=open, 1=closed
	chanFieldUnbuffered = 7  // i8   1 if user requested capacity=0
	chanFieldMutex      = 8  // i8*  PAL mutex handle
	chanFieldNotEmpty   = 9  // i8*  cond var: signaled when items added or closed
	chanFieldNotFull    = 10 // i8*  cond var: signaled when items removed

	// Goroutine waiter lists (Phase 5c: M:N scheduler)
	chanFieldSendWaitersHead = 11 // i8*  head of parked sender Gs
	chanFieldSendWaitersTail = 12 // i8*  tail of parked sender Gs
	chanFieldRecvWaitersHead = 13 // i8*  head of parked receiver Gs
	chanFieldRecvWaitersTail = 14 // i8*  tail of parked receiver Gs

	// Rendezvous waiter list for unbuffered channels (T0312)
	chanFieldRvWaitersHead = 15 // i8*  head of rendezvous-parked senders
	chanFieldRvWaitersTail = 16 // i8*  tail of rendezvous-parked senders

	// Reference count for shared channels (B0163)
	chanFieldRefcount = 17 // i64  atomic reference count (starts at 1)
)

// channelStructType returns the LLVM struct type for a channel.
// Layout: { i8*, i64, i64, i64, i64, i64, i8, i8, i8*, i8*, i8*, i8*, i8*, i8*, i8*, i8*, i8*, i64 } — 18 fields
func channelStructType() *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I8Ptr, // buffer
		irtypes.I64,   // elem_size
		irtypes.I64,   // capacity
		irtypes.I64,   // count
		irtypes.I64,   // head
		irtypes.I64,   // tail
		irtypes.I8,    // closed
		irtypes.I8,    // unbuffered
		irtypes.I8Ptr, // mutex
		irtypes.I8Ptr, // not_empty cond
		irtypes.I8Ptr, // not_full cond
		irtypes.I8Ptr, // send_waiters_head
		irtypes.I8Ptr, // send_waiters_tail
		irtypes.I8Ptr, // recv_waiters_head
		irtypes.I8Ptr, // recv_waiters_tail
		irtypes.I8Ptr, // rv_waiters_head (T0312: rendezvous-parked senders)
		irtypes.I8Ptr, // rv_waiters_tail (T0312)
		irtypes.I64,   // refcount (B0163: atomic reference count)
	)
}

// defineChannelNewFunc emits @promise_channel_new(i64 %capacity, i64 %elem_size) → i8*
// Allocates and initializes a channel struct with ring buffer, mutex, and 2 cond vars.
func (c *Compiler) defineChannelNewFunc() {
	capParam := ir.NewParam("capacity", irtypes.I64)
	elemSzParam := ir.NewParam("elem_size", irtypes.I64)
	fn := c.module.NewFunc("promise_channel_new", irtypes.I8Ptr, capParam, elemSzParam)

	chanType := channelStructType()

	entry := fn.NewBlock(".entry")

	// Allocate channel struct
	structSize := constant.NewInt(irtypes.I64, int64(c.typeSize(chanType)))
	rawPtr := entry.NewCall(c.palAlloc, structSize)
	chPtr := entry.NewBitCast(rawPtr, irtypes.NewPointer(chanType))

	// actual_cap = max(capacity, 1) — even unbuffered channels need 1-slot buffer
	isZero := entry.NewICmp(enum.IPredEQ, capParam, constant.NewInt(irtypes.I64, 0))
	actualCap := entry.NewSelect(isZero, constant.NewInt(irtypes.I64, 1), capParam)

	// Allocate ring buffer: actual_cap * elem_size
	bufSize := entry.NewMul(actualCap, elemSzParam)
	bufPtr := entry.NewCall(c.palAlloc, bufSize)

	// Store buffer
	bufField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
	entry.NewStore(bufPtr, bufField)

	// Store elem_size
	esField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldElemSize)))
	entry.NewStore(elemSzParam, esField)

	// Store capacity = actual_cap
	capField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
	entry.NewStore(actualCap, capField)

	// Store count = 0
	countField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
	entry.NewStore(constant.NewInt(irtypes.I64, 0), countField)

	// Store head = 0
	headField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldHead)))
	entry.NewStore(constant.NewInt(irtypes.I64, 0), headField)

	// Store tail = 0
	tailField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldTail)))
	entry.NewStore(constant.NewInt(irtypes.I64, 0), tailField)

	// Store closed = 0
	closedField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
	entry.NewStore(constant.NewInt(irtypes.I8, 0), closedField)

	// Store unbuffered = (capacity == 0) ? 1 : 0
	unbufVal := entry.NewSelect(isZero, constant.NewInt(irtypes.I8, 1), constant.NewInt(irtypes.I8, 0))
	unbufField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldUnbuffered)))
	entry.NewStore(unbufVal, unbufField)

	// Init mutex
	mtx := entry.NewCall(c.palMutexInit)
	mtxField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	entry.NewStore(mtx, mtxField)

	// Init not_empty cond var
	notEmpty := entry.NewCall(c.palCondInit)
	neField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	entry.NewStore(notEmpty, neField)

	// Init not_full cond var
	notFull := entry.NewCall(c.palCondInit)
	nfField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	entry.NewStore(notFull, nfField)

	// Init goroutine waiter lists to null (send, recv, rv)
	nullPtr := constant.NewNull(irtypes.I8Ptr)
	for _, idx := range []int{chanFieldSendWaitersHead, chanFieldSendWaitersTail,
		chanFieldRecvWaitersHead, chanFieldRecvWaitersTail,
		chanFieldRvWaitersHead, chanFieldRvWaitersTail} {
		field := entry.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
		entry.NewStore(nullPtr, field)
	}

	// Init refcount to 1 (B0163: reference counting for shared channels)
	rcField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRefcount)))
	entry.NewStore(constant.NewInt(irtypes.I64, 1), rcField)

	entry.NewRet(rawPtr)

	c.funcs["promise_channel_new"] = fn
}

// --- LLVM Coroutine Intrinsics (Phase 5c) ---

// declareCoroIntrinsics declares all LLVM coroutine intrinsics needed for the M:N scheduler.
func (c *Compiler) declareCoroIntrinsics() {
	// @llvm.coro.id(i32 align, i8* promise, i8* coroaddr, i8* fnaddrs) → token
	c.coroId = c.module.NewFunc("llvm.coro.id", irtypes.Token,
		ir.NewParam("align", irtypes.I32),
		ir.NewParam("promise", irtypes.I8Ptr),
		ir.NewParam("coroaddr", irtypes.I8Ptr),
		ir.NewParam("fnaddrs", irtypes.I8Ptr))

	// @llvm.coro.alloc(token %id) → i1
	c.coroAlloc = c.module.NewFunc("llvm.coro.alloc", irtypes.I1,
		ir.NewParam("id", irtypes.Token))

	// @llvm.coro.begin(token %id, i8* %mem) → i8*
	c.coroBegin = c.module.NewFunc("llvm.coro.begin", irtypes.I8Ptr,
		ir.NewParam("id", irtypes.Token),
		ir.NewParam("mem", irtypes.I8Ptr))

	// @llvm.coro.size.{i32|i64}() → {i32|i64}
	if c.isWasm {
		c.coroSize = c.module.NewFunc("llvm.coro.size.i32", irtypes.I32)
	} else {
		c.coroSize = c.module.NewFunc("llvm.coro.size.i64", irtypes.I64)
	}

	// @llvm.coro.suspend(token %save, i1 %final) → i8
	c.coroSuspend = c.module.NewFunc("llvm.coro.suspend", irtypes.I8,
		ir.NewParam("save", irtypes.Token),
		ir.NewParam("final", irtypes.I1))

	// @llvm.coro.end(i8* %handle, i1 %unwind, token %bundle) → void (LLVM 22+)
	c.coroEnd = c.module.NewFunc("llvm.coro.end", irtypes.Void,
		ir.NewParam("handle", irtypes.I8Ptr),
		ir.NewParam("unwind", irtypes.I1),
		ir.NewParam("bundle", irtypes.Token))

	// @llvm.coro.free(token %id, i8* %handle) → i8*
	c.coroFree = c.module.NewFunc("llvm.coro.free", irtypes.I8Ptr,
		ir.NewParam("id", irtypes.Token),
		ir.NewParam("handle", irtypes.I8Ptr))

	// @llvm.coro.resume(i8* %handle) → void
	c.coroResume = c.module.NewFunc("llvm.coro.resume", irtypes.Void,
		ir.NewParam("handle", irtypes.I8Ptr))

	// @llvm.coro.destroy(i8* %handle) → void
	c.coroDestroy = c.module.NewFunc("llvm.coro.destroy", irtypes.Void,
		ir.NewParam("handle", irtypes.I8Ptr))

	// @llvm.coro.done(i8* %handle) → i1
	c.coroDone = c.module.NewFunc("llvm.coro.done", irtypes.I1,
		ir.NewParam("handle", irtypes.I8Ptr))

	// noinline wrappers for generator consumer code — prevents coro-elide from
	// seeing the resume/done/destroy pattern and incorrectly stack-allocating frames.
	c.genResume = c.module.NewFunc("__promise_gen_resume", irtypes.Void,
		ir.NewParam("handle", irtypes.I8Ptr))
	c.genResume.FuncAttrs = append(c.genResume.FuncAttrs, rawFuncAttr("noinline"))
	{
		b := c.genResume.NewBlock(".entry")
		b.NewCall(c.coroResume, c.genResume.Params[0])
		b.NewRet(nil)
	}

	c.genDone = c.module.NewFunc("__promise_gen_done", irtypes.I1,
		ir.NewParam("handle", irtypes.I8Ptr))
	c.genDone.FuncAttrs = append(c.genDone.FuncAttrs, rawFuncAttr("noinline"))
	{
		b := c.genDone.NewBlock(".entry")
		v := b.NewCall(c.coroDone, c.genDone.Params[0])
		b.NewRet(v)
	}

	c.genDestroy = c.module.NewFunc("__promise_gen_destroy", irtypes.Void,
		ir.NewParam("handle", irtypes.I8Ptr))
	c.genDestroy.FuncAttrs = append(c.genDestroy.FuncAttrs, rawFuncAttr("noinline"))
	{
		b := c.genDestroy.NewBlock(".entry")
		b.NewCall(c.coroDestroy, c.genDestroy.Params[0])
		b.NewRet(nil)
	}

	// T0088/T0128: Generic cleanup for _FnIter instances — frees parent chain + closure env + instance.
	// _FnIter layout: { i8* variant, { i8* fn_ptr, i8* env_ptr } _next, i64 _parent }
	// T0128: _parent stores ptrtoint of upstream _FnIter instance in combinator chains.
	// If non-zero, recursively calls iterCleanup on the parent before freeing self.
	c.iterCleanup = c.module.NewFunc("__promise_iter_cleanup", irtypes.Void,
		ir.NewParam("inst", irtypes.I8Ptr))
	{
		// Struct type matching _FnIter instance layout
		iterInstType := irtypes.NewStruct(irtypes.I8Ptr, closureType(), irtypes.I64)
		entry := c.iterCleanup.NewBlock(".entry")
		typedPtr := entry.NewBitCast(c.iterCleanup.Params[0], irtypes.NewPointer(iterInstType))

		// Load _parent (field 2: i64)
		parentField := entry.NewGetElementPtr(iterInstType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
		parentInt := entry.NewLoad(irtypes.I64, parentField)
		parentIsZero := entry.NewICmp(enum.IPredEQ, parentInt, constant.NewInt(irtypes.I64, 0))
		cleanParentBlk := c.iterCleanup.NewBlock("clean.parent")
		loadEnvBlk := c.iterCleanup.NewBlock("load.env")
		entry.NewCondBr(parentIsZero, loadEnvBlk, cleanParentBlk)

		// T0128: Recursively clean parent _FnIter
		parentPtr := cleanParentBlk.NewIntToPtr(parentInt, irtypes.I8Ptr)
		cleanParentBlk.NewCall(c.iterCleanup, parentPtr)
		cleanParentBlk.NewBr(loadEnvBlk)

		// Load env_ptr (field 1, sub-field 1)
		envField := loadEnvBlk.NewGetElementPtr(iterInstType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1), constant.NewInt(irtypes.I32, 1))
		envPtr := loadEnvBlk.NewLoad(irtypes.I8Ptr, envField)
		isNull := loadEnvBlk.NewICmp(enum.IPredEQ, envPtr, constant.NewNull(irtypes.I8Ptr))
		checkDropBlk := c.iterCleanup.NewBlock("check.env_drop")
		freeInstBlk := c.iterCleanup.NewBlock("free.inst")
		loadEnvBlk.NewCondBr(isNull, freeInstBlk, checkDropBlk)

		// B0221: Load env drop fn from field 0 of env struct header.
		// If non-null, call it (handles dropping captured values + freeing env).
		// If null, just free the env struct.
		envHeaderType := irtypes.NewStruct(irtypes.I8Ptr)
		typedHdr := checkDropBlk.NewBitCast(envPtr, irtypes.NewPointer(envHeaderType))
		dropFnField := checkDropBlk.NewGetElementPtr(envHeaderType, typedHdr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		dropFnRaw := checkDropBlk.NewLoad(irtypes.I8Ptr, dropFnField)
		hasDrop := checkDropBlk.NewICmp(enum.IPredNE, dropFnRaw, constant.NewNull(irtypes.I8Ptr))
		callDropBlk := c.iterCleanup.NewBlock("env.deep_drop")
		justFreeBlk := c.iterCleanup.NewBlock("env.shallow_free")
		checkDropBlk.NewCondBr(hasDrop, callDropBlk, justFreeBlk)

		envDropFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
		typedDropFn := callDropBlk.NewBitCast(dropFnRaw, irtypes.NewPointer(envDropFnType))
		callDropBlk.NewCall(typedDropFn, envPtr)
		callDropBlk.NewBr(freeInstBlk)

		justFreeBlk.NewCall(c.palFree, envPtr)
		justFreeBlk.NewBr(freeInstBlk)

		freeInstBlk.NewCall(c.palFree, c.iterCleanup.Params[0])
		freeInstBlk.NewRet(nil)
	}

	// B0270: Generic RTTI-based drop for structural interface instances.
	// Unlike __promise_iter_cleanup which assumes _FnIter layout, this function
	// works for ANY concrete type behind a structural interface by dispatching
	// through the typeinfo drop_fn_ptr (field 1 of the variant/typeinfo struct).
	// Instance layout: { typeinfo_ptr*, ... }
	// Typeinfo layout: { i8* vtable_ptr, i8* drop_fn_ptr, ... }
	c.structuralDrop = c.module.NewFunc("__promise_structural_drop", irtypes.Void,
		ir.NewParam("inst", irtypes.I8Ptr))
	{
		entry := c.structuralDrop.NewBlock(".entry")
		inst := c.structuralDrop.Params[0]

		// Load variant/typeinfo pointer from instance field 0
		instanceType := irtypes.NewStruct(irtypes.I8Ptr) // { typeinfo_ptr* }
		typedInst := entry.NewBitCast(inst, irtypes.NewPointer(instanceType))
		variantField := entry.NewGetElementPtr(instanceType, typedInst,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		variantPtr := entry.NewLoad(irtypes.I8Ptr, variantField)

		// Load drop_fn_ptr from typeinfo field 1
		typeinfoType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr) // { vtable, drop_fn }
		typedInfo := entry.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoType))
		dropFnField := entry.NewGetElementPtr(typeinfoType, typedInfo,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		dropFn := entry.NewLoad(irtypes.I8Ptr, dropFnField)

		isNull := entry.NewICmp(enum.IPredEQ, dropFn, constant.NewNull(irtypes.I8Ptr))
		callBlk := c.structuralDrop.NewBlock("drop.call")
		freeBlk := c.structuralDrop.NewBlock("drop.free")
		entry.NewCondBr(isNull, freeBlk, callBlk)

		// Has drop: call it (synth drops include pal_free; $wrap calls drop + pal_free)
		dropFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
		typedFn := callBlk.NewBitCast(dropFn, irtypes.NewPointer(dropFnType))
		callBlk.NewCall(typedFn, inst)
		callBlk.NewRet(nil)

		// No drop: just free the instance
		freeBlk.NewCall(c.palFree, inst)
		freeBlk.NewRet(nil)
	}

	// T1284: Generic RTTI-based deep clone for structural interface instances.
	// Mirror of __promise_structural_drop: dispatch through the typeinfo
	// clone_fn_ptr (field 2) to produce an independently-owned copy of the boxed
	// concrete instance. Returns the input unchanged when clone_fn is null (no
	// eligible clone) — callers must ensure every reachable box carries a real
	// clone_fn so this fallback is never a silent shallow alias.
	c.structuralClone = c.module.NewFunc("__promise_structural_clone", irtypes.I8Ptr,
		ir.NewParam("inst", irtypes.I8Ptr))
	{
		entry := c.structuralClone.NewBlock(".entry")
		inst := c.structuralClone.Params[0]

		isNullInst := entry.NewICmp(enum.IPredEQ, inst, constant.NewNull(irtypes.I8Ptr))
		nullBlk := c.structuralClone.NewBlock("clone.null")
		liveBlk := c.structuralClone.NewBlock("clone.live")
		entry.NewCondBr(isNullInst, nullBlk, liveBlk)

		nullBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

		// Load variant/typeinfo pointer from instance field 0
		instanceType := irtypes.NewStruct(irtypes.I8Ptr)
		typedInst := liveBlk.NewBitCast(inst, irtypes.NewPointer(instanceType))
		variantField := liveBlk.NewGetElementPtr(instanceType, typedInst,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		variantPtr := liveBlk.NewLoad(irtypes.I8Ptr, variantField)

		// Load clone_fn_ptr from typeinfo field 2
		typeinfoType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I8Ptr)
		typedInfo := liveBlk.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoType))
		cloneFnField := liveBlk.NewGetElementPtr(typeinfoType, typedInfo,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
		cloneFn := liveBlk.NewLoad(irtypes.I8Ptr, cloneFnField)

		isNull := liveBlk.NewICmp(enum.IPredEQ, cloneFn, constant.NewNull(irtypes.I8Ptr))
		callBlk := c.structuralClone.NewBlock("clone.call")
		shallowBlk := c.structuralClone.NewBlock("clone.shallow")
		liveBlk.NewCondBr(isNull, shallowBlk, callBlk)

		cloneFnType := irtypes.NewFunc(irtypes.I8Ptr, irtypes.I8Ptr)
		typedFn := callBlk.NewBitCast(cloneFn, irtypes.NewPointer(cloneFnType))
		cloned := callBlk.NewCall(typedFn, inst)
		callBlk.NewRet(cloned)

		shallowBlk.NewRet(inst)
	}
}
