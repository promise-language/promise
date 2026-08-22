package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// --- Socket deadlines, timeouts and cancellation (T1563) ---
//
// A parked socket operation is bounded in time by an absolute monotonic
// deadline stored in its PollDesc (pd.deadline[dir], 0 = none). PollDescs that
// carry a deadline — or that have been cancelled — are threaded onto a global
// intrusive singly-linked registry through pd.dl_next.
//
// There is no timer source in the scheduler, and none is added here: the reactor
// poller thread already wakes at least once per millisecond (1ms pal_reactor_poll
// timeout, plus a 100us yield on the no-event path), so the expiry scan simply
// piggybacks on that loop. That keeps the whole feature platform-independent —
// no timerfd, no EVFILT_TIMER, no Windows equivalent — at the cost of ~1-2ms of
// overshoot, which is the right granularity for socket deadlines.
//
// Two locks, with a strict order of BATCH LOCK -> REGISTRY LOCK -> pd.lock:
//
//   - pd.lock guards pd.deadline, pd.reason, pd.cancelled, pd.refcount and the
//     two waiter slots.
//   - the registry lock guards the list itself: the head pointer, pd.dl_next
//     and pd.linked.
//
// The reactor calls promise_netpoll_scan_deadlines with the batch lock held, and
// every entry point that needs both takes the registry lock first. arm() is the
// exception that matters for throughput: it needs the registry only when a
// deadline is actually present, so a park with no deadline never touches the
// global lock at all.
//
// Races the design has to survive:
//
//   - Expiry between arm() and the park. arm() releases pd.lock before the
//     inline park code (genNetpollWait) re-takes it to publish the waiter, so a
//     scan can land in between. The scan therefore never clears an expired
//     deadline that had no waiter: it leaves it armed and fires on a later pass,
//     once the goroutine has actually parked. An expired deadline is only ever
//     consumed by the next arm() on that slot, which overwrites it.
//   - Cancel between arm() and the park, which has the same shape. cancel()
//     sets a sticky flag and links the PollDesc onto the registry, so the scan
//     retries the wake until the parked waiter is found.
//
// PollDescs are reference counted (pd.refcount): the socket holds one reference
// and every live CancelHandle holds another, so a handle can safely outlive the
// stream it was created from.

// defineNetpollDeadlineFuncs creates the deadline registry globals and emits
// every promise_netpoll_* function that operates on deadlines, cancellation or
// PollDesc reference counts. Must run before defineNetpollLoopFunc (which calls
// the scan) and defineNetpollCloseFunc (which calls unref).
func (c *Compiler) defineNetpollDeadlineFuncs() {
	c.netpollRegistryLock = c.module.NewGlobalDef("__netpoll_registry_lock", constant.NewNull(irtypes.I8Ptr))
	c.netpollDeadlineHead = c.module.NewGlobalDef("__netpoll_deadline_head", constant.NewNull(irtypes.I8Ptr))

	c.defineNetpollArmFunc()
	c.defineNetpollWakeReasonFunc()
	c.defineNetpollCancelledFunc()
	c.defineNetpollCancelFunc()
	c.defineNetpollRefFunc()
	c.defineNetpollUnrefFunc()
	c.defineNetpollScanDeadlinesFunc()
}

// emitNetpollLink pushes pd onto the deadline registry if it is not already on
// it. Precondition: the registry lock is held. Returns the continuation block.
func (c *Compiler) emitNetpollLink(fn *ir.Func, blk *ir.Block, pdRaw, pdPtr value.Value, prefix string) *ir.Block {
	pdTy := pollDescStructType()

	linkedField := blk.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLinked)))
	linked := blk.NewLoad(irtypes.I32, linkedField)
	isLinked := blk.NewICmp(enum.IPredNE, linked, constant.NewInt(irtypes.I32, 0))

	linkBlk := fn.NewBlock(prefix + ".link")
	contBlk := fn.NewBlock(prefix + ".linked")
	blk.NewCondBr(isLinked, contBlk, linkBlk)

	head := linkBlk.NewLoad(irtypes.I8Ptr, c.netpollDeadlineHead)
	nextField := linkBlk.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldDlNext)))
	linkBlk.NewStore(head, nextField)
	linkBlk.NewStore(pdRaw, c.netpollDeadlineHead)
	linkBlk.NewStore(constant.NewInt(irtypes.I32, 1), linkedField)
	linkBlk.NewBr(contBlk)

	return contBlk
}

// defineNetpollArmFunc emits
// @promise_netpoll_arm(i8* pd, i32 dir, i64 deadline) -> void.
//
// Called immediately before a park. Publishes the absolute deadline for the
// direction (0 clears it), resets the wake reason, and links the PollDesc onto
// the registry when a deadline is present.
func (c *Compiler) defineNetpollArmFunc() {
	pdParam := ir.NewParam("pd", irtypes.I8Ptr)
	dirParam := ir.NewParam("dir", irtypes.I32)
	dlParam := ir.NewParam("deadline", irtypes.I64)
	fn := c.module.NewFunc("promise_netpoll_arm", irtypes.Void, pdParam, dirParam, dlParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	pdTy := pollDescStructType()

	entry := fn.NewBlock(".entry")
	pdPtr := entry.NewBitCast(pdParam, irtypes.NewPointer(pdTy))

	lockField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLock)))
	lock := entry.NewLoad(irtypes.I8Ptr, lockField)
	entry.NewCall(c.palMutexLock, lock)

	dlField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldDeadline)), dirParam)
	entry.NewStore(dlParam, dlField)
	reasonField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldReason)), dirParam)
	entry.NewStore(constant.NewInt(irtypes.I32, netpollReasonReady), reasonField)
	entry.NewCall(c.palMutexUnlock, lock)

	// Only a park that actually carries a deadline touches the global registry.
	// Every other park — the overwhelmingly common case — costs nothing beyond
	// the pd.lock it was already taking. Clearing a deadline does not need the
	// registry either: the next scan sees both slots at 0 and unlinks the node.
	hasDeadline := entry.NewICmp(enum.IPredNE, dlParam, constant.NewInt(irtypes.I64, 0))
	linkPath := fn.NewBlock(".link_path")
	doneBlk := fn.NewBlock(".done")
	entry.NewCondBr(hasDeadline, linkPath, doneBlk)

	registry := linkPath.NewLoad(irtypes.I8Ptr, c.netpollRegistryLock)
	linkPath.NewCall(c.palMutexLock, registry)
	linkedBlk := c.emitNetpollLink(fn, linkPath, pdParam, pdPtr, ".arm")
	linkedBlk.NewCall(c.palMutexUnlock, registry)
	linkedBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)

	c.funcs["promise_netpoll_arm"] = fn
}

// defineNetpollWakeReasonFunc emits
// @promise_netpoll_wake_reason(i8* pd, i32 dir) -> i32, reporting why the last
// park on that direction ended (netpollReasonReady / Timeout / Cancelled).
func (c *Compiler) defineNetpollWakeReasonFunc() {
	pdParam := ir.NewParam("pd", irtypes.I8Ptr)
	dirParam := ir.NewParam("dir", irtypes.I32)
	fn := c.module.NewFunc("promise_netpoll_wake_reason", irtypes.I32, pdParam, dirParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	pdTy := pollDescStructType()

	entry := fn.NewBlock(".entry")
	pdPtr := entry.NewBitCast(pdParam, irtypes.NewPointer(pdTy))
	lockField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLock)))
	lock := entry.NewLoad(irtypes.I8Ptr, lockField)
	entry.NewCall(c.palMutexLock, lock)

	reasonField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldReason)), dirParam)
	reason := entry.NewLoad(irtypes.I32, reasonField)
	entry.NewCall(c.palMutexUnlock, lock)
	entry.NewRet(reason)

	c.funcs["promise_netpoll_wake_reason"] = fn
}

// defineNetpollCancelledFunc emits @promise_netpoll_cancelled(i8* pd) -> i32,
// reading the sticky cancellation flag.
func (c *Compiler) defineNetpollCancelledFunc() {
	pdParam := ir.NewParam("pd", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_netpoll_cancelled", irtypes.I32, pdParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	pdTy := pollDescStructType()

	entry := fn.NewBlock(".entry")
	pdPtr := entry.NewBitCast(pdParam, irtypes.NewPointer(pdTy))
	lockField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLock)))
	lock := entry.NewLoad(irtypes.I8Ptr, lockField)
	entry.NewCall(c.palMutexLock, lock)

	flagField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldCancelled)))
	flag := entry.NewLoad(irtypes.I32, flagField)
	entry.NewCall(c.palMutexUnlock, lock)
	entry.NewRet(flag)

	c.funcs["promise_netpoll_cancelled"] = fn
}

// defineNetpollCancelFunc emits @promise_netpoll_cancel(i8* pd) -> void.
//
// Sets the sticky cancellation flag, wakes whichever waiters are already parked,
// and links the PollDesc onto the registry so the scan can retry the wake for a
// goroutine that parks a moment later.
func (c *Compiler) defineNetpollCancelFunc() {
	pdParam := ir.NewParam("pd", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_netpoll_cancel", irtypes.Void, pdParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	pdTy := pollDescStructType()

	entry := fn.NewBlock(".entry")
	pdPtr := entry.NewBitCast(pdParam, irtypes.NewPointer(pdTy))

	registry := entry.NewLoad(irtypes.I8Ptr, c.netpollRegistryLock)
	entry.NewCall(c.palMutexLock, registry)
	lockField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLock)))
	lock := entry.NewLoad(irtypes.I8Ptr, lockField)
	entry.NewCall(c.palMutexLock, lock)

	flagField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldCancelled)))
	entry.NewStore(constant.NewInt(irtypes.I32, 1), flagField)

	blk := c.emitNetpollWakeSlot(fn, entry, pdPtr, pdFieldReadG, netpollDirRead,
		netpollReasonCancelled, nil, nil, ".cancel_read")
	blk = c.emitNetpollWakeSlot(fn, blk, pdPtr, pdFieldWriteG, netpollDirWrite,
		netpollReasonCancelled, nil, nil, ".cancel_write")
	blk = c.emitNetpollLink(fn, blk, pdParam, pdPtr, ".cancel")

	blk.NewCall(c.palMutexUnlock, lock)
	blk.NewCall(c.palMutexUnlock, registry)
	blk.NewRet(nil)

	c.funcs["promise_netpoll_cancel"] = fn
}

// defineNetpollRefFunc emits @promise_netpoll_ref(i8* pd) -> void.
func (c *Compiler) defineNetpollRefFunc() {
	pdParam := ir.NewParam("pd", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_netpoll_ref", irtypes.Void, pdParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	pdTy := pollDescStructType()

	entry := fn.NewBlock(".entry")
	pdPtr := entry.NewBitCast(pdParam, irtypes.NewPointer(pdTy))
	registry := entry.NewLoad(irtypes.I8Ptr, c.netpollRegistryLock)
	entry.NewCall(c.palMutexLock, registry)
	lockField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLock)))
	lock := entry.NewLoad(irtypes.I8Ptr, lockField)
	entry.NewCall(c.palMutexLock, lock)

	rcField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldRefcount)))
	rc := entry.NewLoad(irtypes.I32, rcField)
	entry.NewStore(entry.NewAdd(rc, constant.NewInt(irtypes.I32, 1)), rcField)

	entry.NewCall(c.palMutexUnlock, lock)
	entry.NewCall(c.palMutexUnlock, registry)
	entry.NewRet(nil)

	c.funcs["promise_netpoll_ref"] = fn
}

// defineNetpollUnrefFunc emits @promise_netpoll_unref(i8* pd) -> void.
//
// Drops one reference. On the last one the PollDesc is unlinked from the
// deadline registry, its mutex and condition variable are destroyed, and the
// allocation is freed. Callers must have already removed the fd from the
// reactor (promise_netpoll_close does this, and drains the in-flight event
// batch, before dropping the socket reference).
func (c *Compiler) defineNetpollUnrefFunc() {
	pdParam := ir.NewParam("pd", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_netpoll_unref", irtypes.Void, pdParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	pdTy := pollDescStructType()

	entry := fn.NewBlock(".entry")
	linkAlloca := entry.NewAlloca(irtypes.NewPointer(irtypes.I8Ptr)) // i8** cursor
	pdPtr := entry.NewBitCast(pdParam, irtypes.NewPointer(pdTy))

	registry := entry.NewLoad(irtypes.I8Ptr, c.netpollRegistryLock)
	entry.NewCall(c.palMutexLock, registry)
	lockField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLock)))
	lock := entry.NewLoad(irtypes.I8Ptr, lockField)
	entry.NewCall(c.palMutexLock, lock)

	rcField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldRefcount)))
	rc := entry.NewLoad(irtypes.I32, rcField)
	rcNext := entry.NewSub(rc, constant.NewInt(irtypes.I32, 1))
	entry.NewStore(rcNext, rcField)
	stillAlive := entry.NewICmp(enum.IPredSGT, rcNext, constant.NewInt(irtypes.I32, 0))

	aliveBlk := fn.NewBlock(".alive")
	freeBlk := fn.NewBlock(".free")
	entry.NewCondBr(stillAlive, aliveBlk, freeBlk)

	aliveBlk.NewCall(c.palMutexUnlock, lock)
	aliveBlk.NewCall(c.palMutexUnlock, registry)
	aliveBlk.NewRet(nil)

	// Last reference: unlink from the registry if present, then destroy + free.
	linkedField := freeBlk.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLinked)))
	linked := freeBlk.NewLoad(irtypes.I32, linkedField)
	isLinked := freeBlk.NewICmp(enum.IPredNE, linked, constant.NewInt(irtypes.I32, 0))
	unlinkInit := fn.NewBlock(".unlink_init")
	destroyBlk := fn.NewBlock(".destroy")
	freeBlk.NewCondBr(isLinked, unlinkInit, destroyBlk)

	unlinkInit.NewStore(c.netpollDeadlineHead, linkAlloca)
	unlinkLoop := fn.NewBlock(".unlink_loop")
	unlinkInit.NewBr(unlinkLoop)

	link := unlinkLoop.NewLoad(irtypes.NewPointer(irtypes.I8Ptr), linkAlloca)
	node := unlinkLoop.NewLoad(irtypes.I8Ptr, link)
	atEnd := unlinkLoop.NewICmp(enum.IPredEQ, node, constant.NewNull(irtypes.I8Ptr))
	unlinkCheck := fn.NewBlock(".unlink_check")
	unlinkLoop.NewCondBr(atEnd, destroyBlk, unlinkCheck)

	isSelf := unlinkCheck.NewICmp(enum.IPredEQ, node, pdParam)
	unlinkRemove := fn.NewBlock(".unlink_remove")
	unlinkNext := fn.NewBlock(".unlink_next")
	unlinkCheck.NewCondBr(isSelf, unlinkRemove, unlinkNext)

	selfNextField := unlinkRemove.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldDlNext)))
	selfNext := unlinkRemove.NewLoad(irtypes.I8Ptr, selfNextField)
	unlinkRemove.NewStore(selfNext, link)
	unlinkRemove.NewStore(constant.NewInt(irtypes.I32, 0), linkedField)
	unlinkRemove.NewBr(destroyBlk)

	nodePtr := unlinkNext.NewBitCast(node, irtypes.NewPointer(pdTy))
	nodeNextField := unlinkNext.NewGetElementPtr(pdTy, nodePtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldDlNext)))
	unlinkNext.NewStore(nodeNextField, linkAlloca)
	unlinkNext.NewBr(unlinkLoop)

	condField := destroyBlk.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldCond)))
	condVal := destroyBlk.NewLoad(irtypes.I8Ptr, condField)
	destroyBlk.NewCall(c.palMutexUnlock, lock)
	destroyBlk.NewCall(c.palMutexDestroy, lock)
	destroyBlk.NewCall(c.palCondDestroy, condVal)
	destroyBlk.NewCall(c.palFree, pdParam)
	destroyBlk.NewCall(c.palMutexUnlock, registry)
	destroyBlk.NewRet(nil)

	c.funcs["promise_netpoll_unref"] = fn
}

// defineNetpollScanDeadlinesFunc emits
// @promise_netpoll_scan_deadlines() -> void, the deadline timer.
//
// Walks the registry, waking any waiter whose deadline has passed (reason
// timeout) or whose PollDesc has been cancelled (reason cancelled), and unlinks
// PollDescs that no longer carry either. Called by the reactor poller thread on
// both the event and no-event paths, with the batch lock held.
func (c *Compiler) defineNetpollScanDeadlinesFunc() {
	fn := c.module.NewFunc("promise_netpoll_scan_deadlines", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	pdTy := pollDescStructType()
	nanotime := c.defineNanotimeFunc()

	entry := fn.NewBlock(".entry")
	linkAlloca := entry.NewAlloca(irtypes.NewPointer(irtypes.I8Ptr)) // i8** cursor
	now := entry.NewCall(nanotime)
	registry := entry.NewLoad(irtypes.I8Ptr, c.netpollRegistryLock)
	entry.NewCall(c.palMutexLock, registry)
	entry.NewStore(c.netpollDeadlineHead, linkAlloca)

	loopHead := fn.NewBlock(".loop")
	entry.NewBr(loopHead)

	link := loopHead.NewLoad(irtypes.NewPointer(irtypes.I8Ptr), linkAlloca)
	node := loopHead.NewLoad(irtypes.I8Ptr, link)
	atEnd := loopHead.NewICmp(enum.IPredEQ, node, constant.NewNull(irtypes.I8Ptr))
	bodyBlk := fn.NewBlock(".body")
	doneBlk := fn.NewBlock(".done")
	loopHead.NewCondBr(atEnd, doneBlk, bodyBlk)

	pdPtr := bodyBlk.NewBitCast(node, irtypes.NewPointer(pdTy))
	lockField := bodyBlk.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLock)))
	lock := bodyBlk.NewLoad(irtypes.I8Ptr, lockField)
	bodyBlk.NewCall(c.palMutexLock, lock)

	cancelledField := bodyBlk.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldCancelled)))
	cancelled := bodyBlk.NewLoad(irtypes.I32, cancelledField)
	isCancelled := bodyBlk.NewICmp(enum.IPredNE, cancelled, constant.NewInt(irtypes.I32, 0))

	// Per-direction deadline state, read once under pd.lock.
	dirs := []int{netpollDirRead, netpollDirWrite}
	gFields := []int{pdFieldReadG, pdFieldWriteG}
	names := []string{".scan_read", ".scan_write"}
	armed := make([]value.Value, len(dirs))
	expired := make([]value.Value, len(dirs))
	for i, dir := range dirs {
		dlField := bodyBlk.NewGetElementPtr(pdTy, pdPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldDeadline)),
			constant.NewInt(irtypes.I32, int64(dir)))
		dl := bodyBlk.NewLoad(irtypes.I64, dlField)
		armed[i] = bodyBlk.NewICmp(enum.IPredNE, dl, constant.NewInt(irtypes.I64, 0))
		due := bodyBlk.NewICmp(enum.IPredSLE, dl, now)
		expired[i] = bodyBlk.NewAnd(armed[i], due)
	}

	// Cancellation wins over expiry: it clears the waiter slot first, so the
	// timeout pass below sees no waiter and leaves the cancelled reason in place.
	blk := bodyBlk
	for i, dir := range dirs {
		blk = c.emitNetpollWakeSlot(fn, blk, pdPtr, gFields[i], dir,
			netpollReasonCancelled, isCancelled, nil, names[i]+"_cancel")
	}
	// An expired deadline is deliberately NOT cleared here. If the goroutine has
	// not published its waiter yet (arm() released pd.lock before the park could
	// take it), leaving the deadline armed makes a later pass fire it instead of
	// silently losing the timeout. The next arm() on that slot overwrites it.
	for i, dir := range dirs {
		blk = c.emitNetpollWakeSlot(fn, blk, pdPtr, gFields[i], dir,
			netpollReasonTimeout, expired[i], nil, names[i]+"_timeout")
	}

	// Keep PollDescs that still carry a deadline or a sticky cancellation;
	// unlink the rest so the registry only holds sockets the scan cares about.
	keep := blk.NewOr(isCancelled, blk.NewOr(armed[0], armed[1]))
	nextField := blk.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldDlNext)))
	next := blk.NewLoad(irtypes.I8Ptr, nextField)
	keepBlk := fn.NewBlock(".keep")
	unlinkBlk := fn.NewBlock(".unlink")
	blk.NewCondBr(keep, keepBlk, unlinkBlk)

	keepBlk.NewStore(nextField, linkAlloca)
	keepBlk.NewCall(c.palMutexUnlock, lock)
	keepBlk.NewBr(loopHead)

	linkedField := unlinkBlk.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLinked)))
	unlinkBlk.NewStore(constant.NewInt(irtypes.I32, 0), linkedField)
	unlinkBlk.NewStore(next, link)
	unlinkBlk.NewCall(c.palMutexUnlock, lock)
	unlinkBlk.NewBr(loopHead)

	doneBlk.NewCall(c.palMutexUnlock, registry)
	doneBlk.NewRet(nil)

	c.funcs["promise_netpoll_scan_deadlines"] = fn
}
