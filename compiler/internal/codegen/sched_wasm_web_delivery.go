package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/types"
	"github.com/promise-language/promise/compiler/internal/wasmweb"
)

// This file implements Phase 2 of docs/wasm-web-callbacks.md §19: delivery —
// the subscription table, queue overflow policies (§8.1), and the exported
// promise_web_enqueue (§14) that JS calls to push one event. It never runs
// Promise code and never allocates on the Promise heap, so it is safe from
// any context, including from inside a pump.
//
// Design note (revised from the first draft of this file): rather than a
// custom ring buffer bridged into Promise code by an invented wait/wake
// primitive, each subscription is backed by a REAL Channel[i32]
// (promise_channel_new — compiler_runtime_types.go), the same one Promise's
// `<-ch` / `for v in ch` already use. promise_web_enqueue writes directly
// into that channel's ring buffer and wakes its recv-waiters exactly the way
// channel send already does (mirroring genReceiveChannel's own send-side wake
// in expr_concurrency.go), reusing promise_waiter_wake_one — a plain,
// non-allocating, non-Promise-code-executing scheduler primitive, so this
// still satisfies "never runs Promise code, never allocates". The consumer
// side is then just ordinary `for e in events` over that channel: zero new
// coroutine-suspend code, zero new grammar, 100% existing, tested machinery.
//
// promise_web_subscribe/unsubscribe/subscription_dropped are internal-only:
// they never cross the wasm/JS boundary (only ExportEnqueue and _initialize
// do — §14's "two exports, one import" is unchanged by this file). Phase 3
// (modules/web/web.pr) calls them via `extern — hand-written, per §14.2, not
// generated. Nothing calls them until a program imports web and subscribes,
// so webLiveRegsGlobal still never leaves 0 for any program that doesn't.

// wasmWebSubSlot field indices. Much smaller than the first draft: the
// channel itself now owns the ring buffer, head/tail/count, mutex, and
// waiter lists — the slot only needs to remember which channel and policy.
const (
	subFieldActive  = 0 // i32  0 = free slot, 1 = in use
	subFieldPolicy  = 1 // i32  wasmweb.QueueDropOldest / DropNewest / Coalesce
	subFieldDropped = 2 // i32  drop count (§8.1 — must stay observable)
	subFieldChannel = 3 // i8*  the subscription's Channel[i32] (promise_channel_new), non-owning
)

// wasmWebMaxSubscriptions bounds the subscription table so that
// promise_web_enqueue — which must be safe to call in any context, including
// from JS with no Promise stack underneath it (§14) — never has to grow it.
const wasmWebMaxSubscriptions = 256

// wasmWebSubChannelElemSize is the element size (bytes) of every
// subscription's backing channel. Fixed at 4 (i32) because the queue entry
// crossing the JS boundary is a plain i32 handle (§14) — web.pr is the one
// and only creator of these channels and always requests Channel[i32].
const wasmWebSubChannelElemSize = 4

func wasmWebSubSlotType() *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I32,   // active
		irtypes.I32,   // policy
		irtypes.I32,   // dropped
		irtypes.I8Ptr, // channel
	)
}

func wasmWebSubTableType() *irtypes.ArrayType {
	return irtypes.NewArray(wasmWebMaxSubscriptions, wasmWebSubSlotType())
}

// wasmWebSubField GEPs to one field of subscription slot subID within the
// fixed global table.
func (c *Compiler) wasmWebSubField(blk *ir.Block, subID value.Value, field int) *ir.InstGetElementPtr {
	return blk.NewGetElementPtr(wasmWebSubTableType(), c.webSubscriptionsGlobal,
		constant.NewInt(irtypes.I32, 0), subID, constant.NewInt(irtypes.I32, int64(field)))
}

// wasmWebChanField GEPs to one field of a channel struct (compiler_runtime_types.go's
// channelStructType/chanField* constants) given its raw i8* instance pointer.
func (c *Compiler) wasmWebChanField(blk *ir.Block, chPtr value.Value, field int) *ir.InstGetElementPtr {
	chanType := channelStructType()
	typed := blk.NewBitCast(chPtr, irtypes.NewPointer(chanType))
	return blk.NewGetElementPtr(chanType, typed,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(field)))
}

// wasmWebUnboxI32Param extracts the raw i32 from a plain `extern parameter.
// Non-wasm_import externs use Promise's generic C-ABI convention — each
// scalar arrives boxed as a pointer to a promise_i32_v value struct
// ({vtable=null, instance=null, raw}, see extern.go's packPrimitive) — not as
// a bare i32. Only `wasm_import declarations get the raw-scalar fast path
// (extern.go's isWasmImport branches), and these internal delivery functions
// deliberately aren't imports (they're Go-defined, not host-provided).
//
// This does not call extern.go's packPrimitive/unpackPrimitive directly —
// those append their extractvalue/insertvalue to c.block (the compiler's
// ambient "current block" field, maintained only during ordinary Promise
// source codegen), not to an explicit block parameter. Every function in this
// file builds IR against its own local blk variables without ever touching
// c.block (matching sched.go/sched_wasm_web.go's established pattern for
// hand-emitted intrinsics), so calling through c.block here would silently
// attach these instructions to whatever block c.block was last left pointing
// at — a different function's block, or none at all. i32 needs no
// coerceToRaw/coerceFromRaw width adjustment (raw and internal are both i32),
// so the field 0/1/2 layout from packPrimitive is reproduced directly instead.
func (c *Compiler) wasmWebUnboxI32Param(blk *ir.Block, param value.Value) value.Value {
	layout := c.layouts[types.TypI32]
	valType := layout.Value.LLVMType
	typed := blk.NewBitCast(param, irtypes.NewPointer(valType))
	loaded := blk.NewLoad(valType, typed)
	return blk.NewExtractValue(loaded, 2)
}

// wasmWebBoxI32Sret writes a raw i32 into the sret pointer a plain `extern
// with an i32 return uses (the mirror of wasmWebUnboxI32Param — see its doc
// for why this hand-rolls packPrimitive's {vtable=null, instance=null, raw}
// layout instead of calling it).
func (c *Compiler) wasmWebBoxI32Sret(blk *ir.Block, sretParam value.Value, raw value.Value) {
	layout := c.layouts[types.TypI32]
	valType := layout.Value.LLVMType
	instancePtrType := layout.Value.Fields[1].LLVMType.(*irtypes.PointerType)
	agg := blk.NewInsertValue(constant.NewUndef(valType), constant.NewNull(irtypes.I8Ptr), 0)
	agg = blk.NewInsertValue(agg, constant.NewNull(instancePtrType), 1)
	agg = blk.NewInsertValue(agg, raw, 2)
	typed := blk.NewBitCast(sretParam, irtypes.NewPointer(valType))
	blk.NewStore(agg, typed)
}

// wasmWebGetOrDeclareFunc returns the Func already registered under name, or
// creates one. A program that `use web;`s pulls in web.pr's `extern("...")
// declarations for these names, and declareExterns (extern.go) creates its
// own *ir.Func for each — independently of these defineWebXxxFunc functions,
// which also always run for every wasm32-web build regardless of whether the
// program imports web. Without this check, both sides call c.module.NewFunc
// for the same name, producing two separate LLVM functions and an "invalid
// redefinition" verifier error. When declareExterns ran first, its Func's
// params (already shaped to match — web.pr's declared signatures are written
// to agree with these bodies) are reused as-is; when it didn't run at all (no
// `use web;` in the program), this creates the Func fresh exactly as before.
func (c *Compiler) wasmWebGetOrDeclareFunc(name string, retType irtypes.Type, params ...*ir.Param) *ir.Func {
	if fn, ok := c.funcs[name]; ok {
		return fn
	}
	fn := c.module.NewFunc(name, retType, params...)
	return fn
}

// defineWebSubscriptionGlobals declares the fixed-size subscription table.
// Only called when c.isWasmWeb.
func (c *Compiler) defineWebSubscriptionGlobals() {
	tableTy := wasmWebSubTableType()
	table := c.module.NewGlobal("promise_web_subscriptions", tableTy)
	table.Init = constant.NewZeroInitializer(tableTy)
	c.webSubscriptionsGlobal = table
}

// defineWebSubscribeFunc emits internal
// @promise_web_subscribe(i32 %capacity, i32 %policy) → i32.
//
// Finds a free slot, allocates a fresh Channel[i32] via the existing
// promise_channel_new (compiler_runtime_types.go — the same allocator every
// `Channel[T](cap)` literal uses), and returns the sub_id, or -1 if the table
// is full. Increments GlobalLiveRegistrations (§4.1): a program with at
// least one live subscription is a reactor.
//
// The slot holds the channel pointer but does not own a reference to it —
// web.pr's subscription wrapper owns the Channel[i32] value it gets back
// (via promise_web_channel) and is responsible for calling
// promise_web_unsubscribe before that value is dropped (§12). Once
// unsubscribe marks the slot inactive, promise_web_enqueue can never touch a
// freed channel.
func (c *Compiler) defineWebSubscribeFunc() {
	fn := c.wasmWebGetOrDeclareFunc(wasmweb.SubscribeFunc, irtypes.Void,
		ir.NewParam("sret", irtypes.I8Ptr),
		ir.NewParam("capacity", irtypes.I8Ptr),
		ir.NewParam("policy", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	sretParam, capParamBoxed, policyParamBoxed := fn.Params[0], fn.Params[1], fn.Params[2]

	entry := fn.NewBlock(".entry")
	loop := fn.NewBlock("loop")
	checkActiveBlk := fn.NewBlock("check_active")
	nextIterBlk := fn.NewBlock("next_iter")
	foundBlk := fn.NewBlock("found")
	notFoundBlk := fn.NewBlock("not_found")

	capParam := c.wasmWebUnboxI32Param(entry, capParamBoxed)
	policyParam := c.wasmWebUnboxI32Param(entry, policyParamBoxed)

	// capacity <= 0 defaults to DefaultQueueCapacity (§8.1).
	capIsZero := entry.NewICmp(enum.IPredSLE, capParam, constant.NewInt(irtypes.I32, 0))
	effCap := entry.NewSelect(capIsZero, constant.NewInt(irtypes.I32, wasmweb.DefaultQueueCapacity), capParam)
	entry.NewBr(loop)

	idxPhi := loop.NewPhi(ir.NewIncoming(constant.NewInt(irtypes.I32, 0), entry))
	inBounds := loop.NewICmp(enum.IPredSLT, idxPhi, constant.NewInt(irtypes.I32, wasmWebMaxSubscriptions))
	loop.NewCondBr(inBounds, checkActiveBlk, notFoundBlk)

	activeField := c.wasmWebSubField(checkActiveBlk, idxPhi, subFieldActive)
	active := checkActiveBlk.NewLoad(irtypes.I32, activeField)
	isFree := checkActiveBlk.NewICmp(enum.IPredEQ, active, constant.NewInt(irtypes.I32, 0))
	checkActiveBlk.NewCondBr(isFree, foundBlk, nextIterBlk)

	idxNext := nextIterBlk.NewAdd(idxPhi, constant.NewInt(irtypes.I32, 1))
	idxPhi.Incs = append(idxPhi.Incs, ir.NewIncoming(idxNext, nextIterBlk))
	nextIterBlk.NewBr(loop)

	// found: allocate the channel, populate the slot, bump live registrations.
	capI64 := foundBlk.NewSExt(effCap, irtypes.I64)
	chPtr := foundBlk.NewCall(c.funcs["promise_channel_new"], capI64,
		constant.NewInt(irtypes.I64, wasmWebSubChannelElemSize))

	foundBlk.NewStore(policyParam, c.wasmWebSubField(foundBlk, idxPhi, subFieldPolicy))
	foundBlk.NewStore(constant.NewInt(irtypes.I32, 0), c.wasmWebSubField(foundBlk, idxPhi, subFieldDropped))
	foundBlk.NewStore(chPtr, c.wasmWebSubField(foundBlk, idxPhi, subFieldChannel))
	foundBlk.NewStore(constant.NewInt(irtypes.I32, 1), c.wasmWebSubField(foundBlk, idxPhi, subFieldActive))

	live := foundBlk.NewLoad(irtypes.I32, c.webLiveRegsGlobal)
	foundBlk.NewStore(foundBlk.NewAdd(live, constant.NewInt(irtypes.I32, 1)), c.webLiveRegsGlobal)

	c.wasmWebBoxI32Sret(foundBlk, sretParam, idxPhi)
	foundBlk.NewRet(nil)

	c.wasmWebBoxI32Sret(notFoundBlk, sretParam, constant.NewInt(irtypes.I32, -1))
	notFoundBlk.NewRet(nil)

	c.funcs[wasmweb.SubscribeFunc] = fn
}

// defineWebChannelFunc emits internal @promise_web_channel(i32 %sub_id) → i8*
// — the channel pointer for a subscription, for web.pr to wrap as a proper
// Channel[i32] value (container types are raw i8* internally — see
// extern.go's isOpaqueContainerType handling — so a plain `extern declared
// to return Channel[i32] receives this pointer directly, no unpacking).
// Out-of-range returns null.
func (c *Compiler) defineWebChannelFunc() {
	fn := c.wasmWebGetOrDeclareFunc(wasmweb.ChannelFunc, irtypes.I8Ptr,
		ir.NewParam("sub_id", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	subParamBoxed := fn.Params[0]

	entry := fn.NewBlock(".entry")
	inBoundsBlk := fn.NewBlock("in_bounds")
	nullRetBlk := fn.NewBlock("null_ret")

	subParam := c.wasmWebUnboxI32Param(entry, subParamBoxed)
	inBounds := entry.NewAnd(
		entry.NewICmp(enum.IPredSGE, subParam, constant.NewInt(irtypes.I32, 0)),
		entry.NewICmp(enum.IPredSLT, subParam, constant.NewInt(irtypes.I32, wasmWebMaxSubscriptions)))
	entry.NewCondBr(inBounds, inBoundsBlk, nullRetBlk)

	ch := inBoundsBlk.NewLoad(irtypes.I8Ptr, c.wasmWebSubField(inBoundsBlk, subParam, subFieldChannel))
	inBoundsBlk.NewRet(ch)

	nullRetBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	c.funcs[wasmweb.ChannelFunc] = fn
}

// defineWebUnsubscribeFunc emits internal
// @promise_web_unsubscribe(i32 %sub_id) → void.
//
// Marks the slot free so promise_web_enqueue stops touching its channel.
// Decrements GlobalLiveRegistrations (§4.1) — closing the last subscription
// lets a reactor program terminate on its next drain, exactly like an
// ordinary command (§13). Does not touch the channel itself: web.pr's
// subscription wrapper calls this before its Channel[i32] field drops (§12),
// and the channel's own drop path (existing, unchanged) frees the buffer,
// mutex, and cond vars once nothing references it anymore. Out-of-range or
// already-inactive sub_id is a silent no-op.
func (c *Compiler) defineWebUnsubscribeFunc() {
	fn := c.wasmWebGetOrDeclareFunc(wasmweb.UnsubscribeFunc, irtypes.Void,
		ir.NewParam("sub_id", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	idParamBoxed := fn.Params[0]

	entry := fn.NewBlock(".entry")
	inBoundsBlk := fn.NewBlock("in_bounds")
	activeBlk := fn.NewBlock("active")
	doneBlk := fn.NewBlock("done")

	idParam := c.wasmWebUnboxI32Param(entry, idParamBoxed)
	inBounds := entry.NewAnd(
		entry.NewICmp(enum.IPredSGE, idParam, constant.NewInt(irtypes.I32, 0)),
		entry.NewICmp(enum.IPredSLT, idParam, constant.NewInt(irtypes.I32, wasmWebMaxSubscriptions)))
	entry.NewCondBr(inBounds, inBoundsBlk, doneBlk)

	activeField := c.wasmWebSubField(inBoundsBlk, idParam, subFieldActive)
	active := inBoundsBlk.NewLoad(irtypes.I32, activeField)
	isActive := inBoundsBlk.NewICmp(enum.IPredNE, active, constant.NewInt(irtypes.I32, 0))
	inBoundsBlk.NewCondBr(isActive, activeBlk, doneBlk)

	activeBlk.NewStore(constant.NewInt(irtypes.I32, 0), activeField)
	activeBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.wasmWebSubField(activeBlk, idParam, subFieldChannel))

	live := activeBlk.NewLoad(irtypes.I32, c.webLiveRegsGlobal)
	activeBlk.NewStore(activeBlk.NewSub(live, constant.NewInt(irtypes.I32, 1)), c.webLiveRegsGlobal)
	activeBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)

	c.funcs[wasmweb.UnsubscribeFunc] = fn
}

// defineWebEnqueueFunc emits the exported
// @promise_web_enqueue(i32 %sub_id, i32 %handle) → i32 (wasmweb.ExportEnqueue).
//
// This is the one export JS calls to push an event (§7, §14): it never runs
// Promise code, never allocates, and is safe from any context including
// inside a pump. It writes directly into the subscription's Channel[i32]
// ring buffer under its own mutex and, on success, wakes a parked receiver
// via promise_waiter_wake_one — the exact same primitive channel recv's own
// send-side wake uses (expr_concurrency.go's genReceiveChannel, which wakes
// send-waiters symmetrically) — so any goroutine blocked in `<-events` or
// `for e in events` resumes through entirely existing, unmodified machinery.
// promise_waiter_wake_one only moves a G pointer between lists and onto a run
// queue; it does not execute Promise code or allocate, so this still holds
// enqueue's contract. Applies the subscription's overflow policy (§8.1) and
// returns EnqueueOK / EnqueueDropped. An out-of-range or inactive sub_id is
// EnqueueDropped, never a trap — JS holds only an untrusted integer id.
func (c *Compiler) defineWebEnqueueFunc() {
	subParam := ir.NewParam("sub_id", irtypes.I32)
	handleParam := ir.NewParam("handle", irtypes.I32)
	fn := c.module.NewFunc(wasmweb.ExportEnqueue, irtypes.I32, subParam, handleParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	inBoundsBlk := fn.NewBlock("in_bounds")
	activeBlk := fn.NewBlock("active")
	droppedRetBlk := fn.NewBlock("dropped_ret")
	lockedBlk := fn.NewBlock("locked")
	hasRoomBlk := fn.NewBlock("has_room")
	pushBlk := fn.NewBlock("push")
	okRetBlk := fn.NewBlock("ok_ret")
	policySwitchBlk := fn.NewBlock("policy_switch")
	dropOldestBlk := fn.NewBlock("drop_oldest")
	dropNewestBlk := fn.NewBlock("drop_newest")
	coalesceBlk := fn.NewBlock("coalesce")

	inBounds := entry.NewAnd(
		entry.NewICmp(enum.IPredSGE, subParam, constant.NewInt(irtypes.I32, 0)),
		entry.NewICmp(enum.IPredSLT, subParam, constant.NewInt(irtypes.I32, wasmWebMaxSubscriptions)))
	entry.NewCondBr(inBounds, inBoundsBlk, droppedRetBlk)

	activeField := c.wasmWebSubField(inBoundsBlk, subParam, subFieldActive)
	active := inBoundsBlk.NewLoad(irtypes.I32, activeField)
	isActive := inBoundsBlk.NewICmp(enum.IPredNE, active, constant.NewInt(irtypes.I32, 0))
	inBoundsBlk.NewCondBr(isActive, activeBlk, droppedRetBlk)

	droppedRetBlk.NewRet(constant.NewInt(irtypes.I32, wasmweb.EnqueueDropped))

	// active: load the channel pointer and lock its mutex before touching
	// any of its fields (matching genReceiveChannel's own locking discipline).
	ch := activeBlk.NewLoad(irtypes.I8Ptr, c.wasmWebSubField(activeBlk, subParam, subFieldChannel))
	mtx := activeBlk.NewLoad(irtypes.I8Ptr, c.wasmWebChanField(activeBlk, ch, chanFieldMutex))
	activeBlk.NewCall(c.palMutexLock, mtx)
	activeBlk.NewBr(lockedBlk)

	// locked: room check against capacity/count.
	capacity := lockedBlk.NewLoad(irtypes.I64, c.wasmWebChanField(lockedBlk, ch, chanFieldCapacity))
	count := lockedBlk.NewLoad(irtypes.I64, c.wasmWebChanField(lockedBlk, ch, chanFieldCount))
	hasRoom := lockedBlk.NewICmp(enum.IPredSLT, count, capacity)
	lockedBlk.NewCondBr(hasRoom, hasRoomBlk, policySwitchBlk)

	// has_room: plain push at tail — the common case, no policy involved.
	tailField := c.wasmWebChanField(hasRoomBlk, ch, chanFieldTail)
	tail := hasRoomBlk.NewLoad(irtypes.I64, tailField)
	buf := hasRoomBlk.NewLoad(irtypes.I8Ptr, c.wasmWebChanField(hasRoomBlk, ch, chanFieldBuffer))
	hasRoomBlk.NewBr(pushBlk)

	slot := pushBlk.NewGetElementPtr(irtypes.I32, pushBlk.NewBitCast(buf, irtypes.NewPointer(irtypes.I32)), tail)
	pushBlk.NewStore(handleParam, slot)
	newTail := pushBlk.NewURem(pushBlk.NewAdd(tail, constant.NewInt(irtypes.I64, 1)), capacity)
	pushBlk.NewStore(newTail, tailField)
	countField := c.wasmWebChanField(pushBlk, ch, chanFieldCount)
	pushBlk.NewStore(pushBlk.NewAdd(count, constant.NewInt(irtypes.I64, 1)), countField)
	// Wake a parked receiver — mirrors genReceiveChannel's send-side wake.
	notEmpty := pushBlk.NewLoad(irtypes.I8Ptr, c.wasmWebChanField(pushBlk, ch, chanFieldNotEmpty))
	pushBlk.NewCall(c.funcs["promise_waiter_wake_one"],
		c.wasmWebChanField(pushBlk, ch, chanFieldRecvWaitersHead),
		c.wasmWebChanField(pushBlk, ch, chanFieldRecvWaitersTail),
		notEmpty)
	pushBlk.NewCall(c.palMutexUnlock, mtx)
	pushBlk.NewBr(okRetBlk)

	okRetBlk.NewRet(constant.NewInt(irtypes.I32, wasmweb.EnqueueOK))

	// policy_switch: full — dispatch on the subscription's overflow policy
	// (§8.1). All three policy blocks are dominated by lockedBlk, so
	// capacity/count/ch/mtx computed there are valid here too.
	headField := c.wasmWebChanField(policySwitchBlk, ch, chanFieldHead)
	head := policySwitchBlk.NewLoad(irtypes.I64, headField)
	tailField2 := c.wasmWebChanField(policySwitchBlk, ch, chanFieldTail)
	tail2 := policySwitchBlk.NewLoad(irtypes.I64, tailField2)
	buf2 := policySwitchBlk.NewLoad(irtypes.I8Ptr, c.wasmWebChanField(policySwitchBlk, ch, chanFieldBuffer))
	buf2I32 := policySwitchBlk.NewBitCast(buf2, irtypes.NewPointer(irtypes.I32))
	droppedField := c.wasmWebSubField(policySwitchBlk, subParam, subFieldDropped)
	dropped := policySwitchBlk.NewLoad(irtypes.I32, droppedField)
	policySwitchBlk.NewStore(policySwitchBlk.NewAdd(dropped, constant.NewInt(irtypes.I32, 1)), droppedField)

	policy := policySwitchBlk.NewLoad(irtypes.I32, c.wasmWebSubField(policySwitchBlk, subParam, subFieldPolicy))
	policySwitchBlk.NewSwitch(policy, dropNewestBlk,
		ir.NewCase(constant.NewInt(irtypes.I32, wasmweb.QueueDropOldest), dropOldestBlk),
		ir.NewCase(constant.NewInt(irtypes.I32, wasmweb.QueueCoalesce), coalesceBlk),
	)

	// drop_oldest (default policy): evict the oldest, push the new arrival at
	// tail. Count is unchanged — the ring stays full. No wake needed: the
	// receiver already knows the channel is non-empty (it was already full).
	newHead := dropOldestBlk.NewURem(dropOldestBlk.NewAdd(head, constant.NewInt(irtypes.I64, 1)), capacity)
	dropOldestBlk.NewStore(newHead, headField)
	dropOldestBlk.NewStore(handleParam, dropOldestBlk.NewGetElementPtr(irtypes.I32, buf2I32, tail2))
	newTail2 := dropOldestBlk.NewURem(dropOldestBlk.NewAdd(tail2, constant.NewInt(irtypes.I64, 1)), capacity)
	dropOldestBlk.NewStore(newTail2, tailField2)
	dropOldestBlk.NewCall(c.palMutexUnlock, mtx)
	dropOldestBlk.NewBr(okRetBlk)

	// drop_newest: reject the arriving event, keep what's already queued.
	dropNewestBlk.NewCall(c.palMutexUnlock, mtx)
	dropNewestBlk.NewBr(droppedRetBlk)

	// coalesce: overwrite the most recently queued slot ((tail - 1) mod cap)
	// in place — the newest still-unread event replaces it rather than
	// growing the backlog. Kind is not distinguished at this layer (the queue
	// entry is opaque per §14); a subscription mixing kinds and opting into
	// Coalesce will coalesce across kinds too — web.pr is expected to
	// restrict Coalesce to single-kind subscriptions.
	lastIdx := coalesceBlk.NewURem(
		coalesceBlk.NewAdd(coalesceBlk.NewAdd(tail2, capacity), constant.NewInt(irtypes.I64, -1)),
		capacity)
	coalesceBlk.NewStore(handleParam, coalesceBlk.NewGetElementPtr(irtypes.I32, buf2I32, lastIdx))
	coalesceBlk.NewCall(c.palMutexUnlock, mtx)
	coalesceBlk.NewBr(okRetBlk)

	c.funcs[wasmweb.ExportEnqueue] = fn
}

// defineWebSubscriptionDroppedFunc emits internal
// @promise_web_subscription_dropped(i32 %sub_id) → i32 — the accessor behind
// web.pr's subscription.dropped (§8.1). Out-of-range returns 0.
func (c *Compiler) defineWebSubscriptionDroppedFunc() {
	fn := c.wasmWebGetOrDeclareFunc(wasmweb.SubscriptionDroppedFunc, irtypes.Void,
		ir.NewParam("sret", irtypes.I8Ptr),
		ir.NewParam("sub_id", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	sretParam, subParamBoxed := fn.Params[0], fn.Params[1]

	entry := fn.NewBlock(".entry")
	inBoundsBlk := fn.NewBlock("in_bounds")
	zeroRetBlk := fn.NewBlock("zero_ret")

	subParam := c.wasmWebUnboxI32Param(entry, subParamBoxed)
	inBounds := entry.NewAnd(
		entry.NewICmp(enum.IPredSGE, subParam, constant.NewInt(irtypes.I32, 0)),
		entry.NewICmp(enum.IPredSLT, subParam, constant.NewInt(irtypes.I32, wasmWebMaxSubscriptions)))
	entry.NewCondBr(inBounds, inBoundsBlk, zeroRetBlk)

	dropped := inBoundsBlk.NewLoad(irtypes.I32, c.wasmWebSubField(inBoundsBlk, subParam, subFieldDropped))
	c.wasmWebBoxI32Sret(inBoundsBlk, sretParam, dropped)
	inBoundsBlk.NewRet(nil)

	c.wasmWebBoxI32Sret(zeroRetBlk, sretParam, constant.NewInt(irtypes.I32, 0))
	zeroRetBlk.NewRet(nil)

	c.funcs[wasmweb.SubscriptionDroppedFunc] = fn
}
