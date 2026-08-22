package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// --- IO reactor (netpoll) integration with M:N scheduler (T0070) ---
//
// The reactor uses a dedicated background poller thread (like sysmon) that calls
// pal_reactor_poll in a loop. When FDs become ready, the poller enqueues parked
// goroutines to the global run queue and wakes idle Ms.
//
// PollDesc struct (per-FD state):
//   { i32 fd, i32 _pad, i8* read_g, i8* write_g, i8* lock, i8* cond,
//     [2 x i64] deadline, [2 x i32] reason, i8* dl_next, i32 linked,
//     i32 refcount, i32 cancelled }
//
// Deadlines and cancellation (T1563) live in netpoll_deadline.go. Lock order is
// BATCH LOCK -> REGISTRY LOCK -> pd.lock; every function that takes more than
// one of them must follow that order or the reactor deadlocks against a
// goroutine arming a deadline.
//
// PollEvent struct (filled by pal_reactor_poll):
//   { i8* userdata, i32 events, i32 _pad }
//
// The wait_read/wait_write operations are generated inline at call sites (like
// channel send/recv) since they require coro.suspend in the calling coroutine.
// They will be implemented when modules/net/ creates native extern wrappers.

const (
	// PollDesc field indices
	pdFieldFd     = 0 // i32 — file descriptor
	pdFieldPad    = 1 // i32 — padding for alignment
	pdFieldReadG  = 2 // i8* — G waiting for read readiness
	pdFieldWriteG = 3 // i8* — G waiting for write readiness
	pdFieldLock   = 4 // i8* — per-PollDesc mutex
	pdFieldCond   = 5 // i8* — condition variable (for thread-blocking fallback)

	// Deadline / cancellation state (T1563). The two-element arrays are indexed
	// by direction (netpollDirRead / netpollDirWrite) so the bridge helpers can
	// select a slot with a runtime index instead of branching on it.
	pdFieldDeadline  = 6  // [2 x i64] — absolute monotonic nanos, 0 = no deadline
	pdFieldReason    = 7  // [2 x i32] — why the last park ended (netpollReason*)
	pdFieldDlNext    = 8  // i8* — intrusive next pointer, deadline registry
	pdFieldLinked    = 9  // i32 — 1 while this PollDesc is on the registry
	pdFieldRefcount  = 10 // i32 — 1 at netpoll_open, +1 per live CancelHandle
	pdFieldCancelled = 11 // i32 — sticky cancellation flag

	// PollEvent field indices (output from pal_reactor_poll)
	peFieldUserdata = 0 // i8* — opaque pointer (PollDesc*)
	peFieldEvents   = 1 // i32 — 1=readable, 2=writable, 4=error
	peFieldPad      = 2 // i32 — padding

	// PollEvent event bits
	pollEventRead  = 1
	pollEventWrite = 2
	pollEventError = 4

	// Max events per poll call
	maxPollEvents = 64

	// Sentinel value stored in pd.read_g/pd.write_g for thread-blocking waiters.
	// The reactor checks for this value: sentinel → signal cond only (no enqueue).
	// Real G pointer → enqueue on run queue. Value 1 is safe because no valid
	// heap pointer is at address 0x1.
	netpollCondWaiterSentinel = 1

	// Park directions — index into pd.deadline / pd.reason.
	netpollDirRead  = 0
	netpollDirWrite = 1

	// Why a parked waiter was woken. Mirrored by _wake_ready/_wake_timeout/
	// _wake_cancelled in modules/net/net.pr.
	netpollReasonReady     = 0
	netpollReasonTimeout   = 1
	netpollReasonCancelled = 2
)

// pollDescStructType returns the LLVM struct type for PollDesc.
func pollDescStructType() *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I32,                      // fd
		irtypes.I32,                      // _pad
		irtypes.I8Ptr,                    // read_g
		irtypes.I8Ptr,                    // write_g
		irtypes.I8Ptr,                    // lock
		irtypes.I8Ptr,                    // cond (for thread-blocking fallback, T0232)
		irtypes.NewArray(2, irtypes.I64), // deadline[read|write] (T1563)
		irtypes.NewArray(2, irtypes.I32), // reason[read|write]   (T1563)
		irtypes.I8Ptr,                    // dl_next
		irtypes.I32,                      // linked
		irtypes.I32,                      // refcount
		irtypes.I32,                      // cancelled
	)
}

// pollEventStructType returns the LLVM struct type for PollEvent output.
func pollEventStructType() *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I8Ptr, // userdata
		irtypes.I32,   // events
		irtypes.I32,   // _pad
	)
}

// defineNetpollFuncs emits all promise_netpoll_* functions.
// Called from defineNetPALBodies when the net module is imported.
func (c *Compiler) defineNetpollFuncs() {
	if c.isWasm {
		return // No reactor on WASM
	}

	// Batch lock (B0324): the reactor holds this lock while processing events.
	// Close acquires it after pal_reactor_del to ensure the current event batch
	// is fully processed before freeing the PollDesc — prevents use-after-free
	// on stale event buffer pointers.
	c.netpollBatchLock = c.module.NewGlobalDef("__netpoll_batch_lock", constant.NewNull(irtypes.I8Ptr))

	// Deadline registry (T1563) — the scan the reactor loop calls lives here,
	// so it must exist before defineNetpollLoopFunc references it.
	c.defineNetpollDeadlineFuncs()

	// Define loop first (init references it)
	c.defineNetpollLoopFunc()
	c.defineNetpollInitFunc()
	c.defineNetpollOpenFunc()
	c.defineNetpollCloseFunc()
	c.defineNetpollShutdownFunc()
}

// defineNetpollShutdownFunc emits @promise_netpoll_shutdown() → void, releasing
// the reactor's two process-wide mutexes (both pal_mutex_init allocations).
//
// This cannot live in promise_sched_shutdown, which is emitted before the
// compiler knows whether the net module is imported and therefore before these
// globals exist. The entry point calls it immediately after
// promise_sched_shutdown has joined the reactor thread, so nothing can still be
// holding either lock.
func (c *Compiler) defineNetpollShutdownFunc() {
	fn := c.module.NewFunc("promise_netpoll_shutdown", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	blk := fn.NewBlock(".entry")
	// Both are null when reactor init failed (B0324, T1563).
	for _, lock := range []struct {
		global *ir.Global
		name   string
	}{
		{c.netpollBatchLock, "batch"},
		{c.netpollRegistryLock, "registry"},
	} {
		val := blk.NewLoad(irtypes.I8Ptr, lock.global)
		nonNull := blk.NewICmp(enum.IPredNE, val, constant.NewNull(irtypes.I8Ptr))
		destroyBlk := fn.NewBlock(".destroy_" + lock.name)
		nextBlk := fn.NewBlock(".after_" + lock.name)
		blk.NewCondBr(nonNull, destroyBlk, nextBlk)

		destroyBlk.NewCall(c.palMutexDestroy, val)
		destroyBlk.NewStore(constant.NewNull(irtypes.I8Ptr), lock.global)
		destroyBlk.NewBr(nextBlk)
		blk = nextBlk
	}
	blk.NewRet(nil)

	c.funcs["promise_netpoll_shutdown"] = fn
}

// emitNetpollShutdown appends the reactor lock teardown call to blk. No-op when
// the net module is not imported (the function is then never emitted).
func (c *Compiler) emitNetpollShutdown(blk *ir.Block) {
	if !c.needsNetpoll {
		return
	}
	if fn, ok := c.funcs["promise_netpoll_shutdown"]; ok {
		blk.NewCall(fn)
	}
}

// defineNetpollInitFunc emits @promise_netpoll_init() → void
// Creates the reactor fd, allocates lock, starts the poller thread.
func (c *Compiler) defineNetpollInitFunc() {
	fn := c.module.NewFunc("promise_netpoll_init", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	schedTy := schedStructType()

	entry := fn.NewBlock(".entry")

	// Create reactor fd
	rfd := entry.NewCall(c.palReactorCreate)
	// If creation failed (rfd < 0), skip reactor setup
	isErr := entry.NewICmp(enum.IPredSLT, rfd, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	errBlk.NewRet(nil) // Silently skip — no reactor available

	// Store reactor fd
	rfdField := okBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReactorFd)))
	okBlk.NewStore(rfd, rfdField)

	// Create reactor lock
	lock := okBlk.NewCall(c.palMutexInit)
	lockField := okBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReactorLock)))
	okBlk.NewStore(lock, lockField)

	// Initialize batch lock — held by reactor during event processing (B0324)
	batchLock := okBlk.NewCall(c.palMutexInit)
	okBlk.NewStore(batchLock, c.netpollBatchLock)

	// Initialize deadline registry lock — same okBlk path as the batch lock, so
	// it is never left null while the poller thread is running (T1563).
	registryLock := okBlk.NewCall(c.palMutexInit)
	okBlk.NewStore(registryLock, c.netpollRegistryLock)

	// Pre-allocate event buffer on the main thread so the allocation is
	// counted before the alloc-count reset — avoids a race where the reactor
	// thread's palAlloc lands inside a test's leak-detection window (B0326).
	eventBufSize := constant.NewInt(irtypes.I64, int64(maxPollEvents*16)) // 16 bytes per PollEvent
	eventBuf := okBlk.NewCall(c.palAlloc, eventBufSize)

	// Start poller thread, passing the pre-allocated event buffer as arg
	loopFn := c.funcs["promise_netpoll_loop"]
	loopFnPtr := okBlk.NewBitCast(loopFn, irtypes.I8Ptr)
	handle := okBlk.NewCall(c.palThreadCreate, loopFnPtr, eventBuf)

	// Store thread handle
	thField := okBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReactorThread)))
	okBlk.NewStore(handle, thField)

	okBlk.NewRet(nil)

	c.funcs["promise_netpoll_init"] = fn
}

// defineNetpollOpenFunc emits @promise_netpoll_open(i32 fd) → i8* (PollDesc*)
// Sets fd non-blocking, allocates PollDesc, registers with reactor.
func (c *Compiler) defineNetpollOpenFunc() {
	fdParam := ir.NewParam("fd", irtypes.I32)
	fn := c.module.NewFunc("promise_netpoll_open", irtypes.I8Ptr, fdParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	pdTy := pollDescStructType()
	schedTy := schedStructType()

	entry := fn.NewBlock(".entry")

	// NOTE: caller is responsible for setting fd non-blocking before calling
	// netpoll_open. This ensures the fd is already in its correct pre-registration
	// state (e.g. "connecting" for connect(), not "writable from new socket") so
	// EPOLLET fires the correct edge transition.

	// Allocate PollDesc
	pdSize := constant.NewInt(irtypes.I64, int64(c.typeSize(pdTy)))
	pdRaw := entry.NewCall(c.palAlloc, pdSize)
	pdPtr := entry.NewBitCast(pdRaw, irtypes.NewPointer(pdTy))

	// Init PollDesc fields
	fdField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldFd)))
	entry.NewStore(fdParam, fdField)

	padField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldPad)))
	entry.NewStore(constant.NewInt(irtypes.I32, 0), padField)

	readGField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldReadG)))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), readGField)

	writeGField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldWriteG)))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), writeGField)

	pdLock := entry.NewCall(c.palMutexInit)
	lockField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLock)))
	entry.NewStore(pdLock, lockField)

	pdCond := entry.NewCall(c.palCondInit)
	condField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldCond)))
	entry.NewStore(pdCond, condField)

	// Deadline / cancellation state starts cleared; the socket itself holds the
	// first PollDesc reference, every live CancelHandle adds one more (T1563).
	for _, dir := range []int{netpollDirRead, netpollDirWrite} {
		dlField := entry.NewGetElementPtr(pdTy, pdPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldDeadline)),
			constant.NewInt(irtypes.I32, int64(dir)))
		entry.NewStore(constant.NewInt(irtypes.I64, 0), dlField)
		reasonField := entry.NewGetElementPtr(pdTy, pdPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldReason)),
			constant.NewInt(irtypes.I32, int64(dir)))
		entry.NewStore(constant.NewInt(irtypes.I32, netpollReasonReady), reasonField)
	}
	dlNextField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldDlNext)))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), dlNextField)
	for _, f := range []struct {
		idx int
		val int64
	}{{pdFieldLinked, 0}, {pdFieldRefcount, 1}, {pdFieldCancelled, 0}} {
		field := entry.NewGetElementPtr(pdTy, pdPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(f.idx)))
		entry.NewStore(constant.NewInt(irtypes.I32, f.val), field)
	}

	// Register with reactor: pal_reactor_add(reactor_fd, fd, pd_ptr)
	rfdField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReactorFd)))
	rfd := entry.NewLoad(irtypes.I32, rfdField)
	entry.NewCall(c.palReactorAdd, rfd, fdParam, pdRaw)

	entry.NewRet(pdRaw)

	c.funcs["promise_netpoll_open"] = fn
}

// emitNetpollWakeSlot emits the shared "wake a parked waiter" sequence for one
// PollDesc direction: clear the waiter slot, record why it was woken, then
// either enqueue the goroutine on the run queue (real G) or signal the
// PollDesc condition variable (thread-blocking sentinel waiter, B0324).
//
// Precondition: pd.lock is held by the caller.
//
// gFieldIdx is pdFieldReadG or pdFieldWriteG, dir the matching netpollDir*, and
// reason the netpollReason* code to store (netpollReasonReady stores nothing —
// arm() already reset the slot). extraCond, when non-nil, is ANDed with the
// "a waiter is present" test. wokenAlloca, when non-nil, is an i32 alloca that
// is incremented on every actual wake. Returns the continuation block, which the
// caller must keep building into.
func (c *Compiler) emitNetpollWakeSlot(fn *ir.Func, blk *ir.Block, pdPtr value.Value,
	gFieldIdx, dir, reason int, extraCond, wokenAlloca value.Value, prefix string) *ir.Block {

	pdTy := pollDescStructType()

	gField := blk.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldIdx)))
	g := blk.NewLoad(irtypes.I8Ptr, gField)
	var cond value.Value = blk.NewICmp(enum.IPredNE, g, constant.NewNull(irtypes.I8Ptr))
	if extraCond != nil {
		cond = blk.NewAnd(extraCond, cond)
	}

	wakeBlk := fn.NewBlock(prefix + ".wake")
	contBlk := fn.NewBlock(prefix + ".cont")
	blk.NewCondBr(cond, wakeBlk, contBlk)

	wakeBlk.NewStore(constant.NewNull(irtypes.I8Ptr), gField)
	if reason != netpollReasonReady {
		reasonField := wakeBlk.NewGetElementPtr(pdTy, pdPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldReason)),
			constant.NewInt(irtypes.I32, int64(dir)))
		wakeBlk.NewStore(constant.NewInt(irtypes.I32, int64(reason)), reasonField)
	}
	if wokenAlloca != nil {
		w := wakeBlk.NewLoad(irtypes.I32, wokenAlloca)
		wakeBlk.NewStore(wakeBlk.NewAdd(w, constant.NewInt(irtypes.I32, 1)), wokenAlloca)
	}
	sentinel := wakeBlk.NewIntToPtr(
		constant.NewInt(irtypes.I64, netpollCondWaiterSentinel), irtypes.I8Ptr)
	isSentinel := wakeBlk.NewICmp(enum.IPredEQ, g, sentinel)
	enqueueBlk := fn.NewBlock(prefix + ".enqueue")
	signalBlk := fn.NewBlock(prefix + ".signal")
	wakeBlk.NewCondBr(isSentinel, signalBlk, enqueueBlk)

	// Real G: enqueue on run queue.
	enqueueBlk.NewCall(c.funcs["promise_sched_enqueue"], g)
	enqueueBlk.NewBr(signalBlk)

	// Signal cond var (wakes thread-blocking waiters; no-op if none waiting).
	pdCondField := signalBlk.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldCond)))
	pdCond := signalBlk.NewLoad(irtypes.I8Ptr, pdCondField)
	signalBlk.NewCall(c.palCondSignal, pdCond)
	signalBlk.NewBr(contBlk)

	return contBlk
}

// defineNetpollCloseFunc emits @promise_netpoll_close(i8* pd) → void
// Unregisters fd from reactor, wakes any waiting Gs, then drops the socket
// reference on the PollDesc (freeing it once no CancelHandle holds one either).
func (c *Compiler) defineNetpollCloseFunc() {
	pdParam := ir.NewParam("pd", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_netpoll_close", irtypes.Void, pdParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	pdTy := pollDescStructType()
	schedTy := schedStructType()

	entry := fn.NewBlock(".entry")
	pdPtr := entry.NewBitCast(pdParam, irtypes.NewPointer(pdTy))

	// Lock PollDesc
	lockField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLock)))
	lock := entry.NewLoad(irtypes.I8Ptr, lockField)
	entry.NewCall(c.palMutexLock, lock)

	// Remove from reactor
	rfdField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReactorFd)))
	rfd := entry.NewLoad(irtypes.I32, rfdField)
	fdField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldFd)))
	fd := entry.NewLoad(irtypes.I32, fdField)
	entry.NewCall(c.palReactorRemove, rfd, fd)

	// Wake both waiter slots. A goroutine parked on a socket that is closed
	// underneath it reports a distinct "cancelled" error rather than looping on
	// a bogus readiness wake (T1563).
	blk := c.emitNetpollWakeSlot(fn, entry, pdPtr, pdFieldReadG, netpollDirRead,
		netpollReasonCancelled, nil, nil, ".close_read")
	blk = c.emitNetpollWakeSlot(fn, blk, pdPtr, pdFieldWriteG, netpollDirWrite,
		netpollReasonCancelled, nil, nil, ".close_write")

	// Mark closed (fd = -1) so reactor skips stale events (B0324)
	blk.NewStore(constant.NewInt(irtypes.I32, -1), fdField)

	// Clear both deadlines. Nothing can arm this PollDesc again, so the next
	// deadline scan unlinks it from the registry instead of walking it forever
	// while a CancelHandle keeps it alive past the close (T1563).
	for _, dir := range []int{netpollDirRead, netpollDirWrite} {
		dlField := blk.NewGetElementPtr(pdTy, pdPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldDeadline)),
			constant.NewInt(irtypes.I32, int64(dir)))
		blk.NewStore(constant.NewInt(irtypes.I64, 0), dlField)
	}
	blk.NewCall(c.palMutexUnlock, lock)

	// Synchronize with reactor: acquire batch lock to ensure the current event
	// batch is fully processed. After this, no stale references to this PD
	// remain (pal_reactor_del prevents future batches; fd==-1 skips current).
	batchLockVal := blk.NewLoad(irtypes.I8Ptr, c.netpollBatchLock)
	blk.NewCall(c.palMutexLock, batchLockVal)
	blk.NewCall(c.palMutexUnlock, batchLockVal)

	// Drop the socket reference. unref unlinks from the deadline registry and
	// destroys + frees the PollDesc once the last reference is gone (T1563).
	blk.NewCall(c.funcs["promise_netpoll_unref"], pdParam)
	blk.NewRet(nil)

	c.funcs["promise_netpoll_close"] = fn
}

// defineNetpollLoopFunc emits @promise_netpoll_loop(i8* arg) → i8*
// Background poller thread that polls the reactor and wakes parked goroutines.
// Follows the sysmon pattern: 10ms poll timeout, checks sched.shutdown to exit.
func (c *Compiler) defineNetpollLoopFunc() {
	argParam := ir.NewParam("arg", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_netpoll_loop", irtypes.I8Ptr, argParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	pdTy := pollDescStructType()
	peTy := pollEventStructType()
	schedTy := schedStructType()
	scanFn := c.funcs["promise_netpoll_scan_deadlines"]

	entry := fn.NewBlock(".entry")
	loop := fn.NewBlock("loop")
	processEvents := fn.NewBlock("process_events")
	noEvents := fn.NewBlock("no_events")
	eventLoop := fn.NewBlock("event_loop")
	eventBody := fn.NewBlock("event_body")
	checkRead := fn.NewBlock("check_read")
	eventNext := fn.NewBlock("event_next")
	eventDone := fn.NewBlock("event_done")
	exitBlk := fn.NewBlock("exit")

	// Use pre-allocated event buffer passed via arg (B0326)
	eventBuf := argParam
	iAlloca := entry.NewAlloca(irtypes.I32)
	// Track whether any goroutines were woken during event processing.
	// Prevents spinning when WSAPoll returns spurious events (e.g., POLLWRNORM
	// on listening sockets on Windows) that do not correspond to waiting goroutines.
	wokenAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewBr(loop)

	// loop: check shutdown, then poll
	shutdownField := loop.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldShutdown)))
	shutdownVal := loop.NewLoad(irtypes.I8, shutdownField)
	isShutdown := loop.NewICmp(enum.IPredNE, shutdownVal, constant.NewInt(irtypes.I8, 0))
	loop.NewCondBr(isShutdown, exitBlk, processEvents)

	// processEvents: lock batch lock, then poll (B0335 — atomic poll+process
	// prevents use-after-free when netpoll_close frees a PollDesc between
	// pal_reactor_poll returning and the reactor processing events)
	batchLock := processEvents.NewLoad(irtypes.I8Ptr, c.netpollBatchLock)
	processEvents.NewCall(c.palMutexLock, batchLock)
	rfdField := processEvents.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReactorFd)))
	rfd := processEvents.NewLoad(irtypes.I32, rfdField)
	count := processEvents.NewCall(c.palReactorPoll, rfd, eventBuf,
		constant.NewInt(irtypes.I32, maxPollEvents),
		constant.NewInt(irtypes.I32, 1)) // 1ms timeout (was 10ms — reduces batch_lock hold time for netpoll_close, B0340)

	hasEvents := processEvents.NewICmp(enum.IPredSGT, count, constant.NewInt(irtypes.I32, 0))
	processEvents.NewCondBr(hasEvents, eventLoop, noEvents)

	// noEvents: expire due deadlines first — a silent socket produces no events
	// at all, so this is the only path on which its deadline can fire (T1563).
	// Then unlock the batch lock and yield briefly to avoid starving
	// netpoll_close waiters (the reactor would otherwise re-lock immediately).
	noEvents.NewCall(scanFn)
	noEvents.NewCall(c.palMutexUnlock, batchLock)
	noEvents.NewCall(c.palUsleep, constant.NewInt(irtypes.I32, 100)) // 100μs
	noEvents.NewBr(loop)

	// eventLoop: iterate events (batch lock held from processEvents)
	eventLoop.NewStore(constant.NewInt(irtypes.I32, 0), iAlloca)
	eventLoop.NewStore(constant.NewInt(irtypes.I32, 0), wokenAlloca)
	eventLoop.NewBr(eventBody)

	// eventBody: check loop condition
	iVal := eventBody.NewLoad(irtypes.I32, iAlloca)
	iDone := eventBody.NewICmp(enum.IPredSGE, iVal, count)
	eventBody.NewCondBr(iDone, eventDone, checkRead)

	// checkRead: extract PollEvent[i], process
	eventBufTyped := checkRead.NewBitCast(eventBuf, irtypes.NewPointer(peTy))
	pe := checkRead.NewGetElementPtr(peTy, eventBufTyped, iVal)

	// Read userdata (PollDesc pointer)
	udataField := checkRead.NewGetElementPtr(peTy, pe,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(peFieldUserdata)))
	udata := checkRead.NewLoad(irtypes.I8Ptr, udataField)

	// Read events
	eventsField := checkRead.NewGetElementPtr(peTy, pe,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(peFieldEvents)))
	events := checkRead.NewLoad(irtypes.I32, eventsField)

	// Cast userdata to PollDesc*
	pdPtr := checkRead.NewBitCast(udata, irtypes.NewPointer(pdTy))

	// Lock PollDesc
	pdLockField := checkRead.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLock)))
	pdLock := checkRead.NewLoad(irtypes.I8Ptr, pdLockField)
	checkRead.NewCall(c.palMutexLock, pdLock)

	// Skip closed PollDescs — fd set to -1 by netpoll_close (B0324)
	pdFdField := checkRead.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldFd)))
	pdFdVal := checkRead.NewLoad(irtypes.I32, pdFdField)
	isClosed := checkRead.NewICmp(enum.IPredEQ, pdFdVal, constant.NewInt(irtypes.I32, -1))
	closedSkip := fn.NewBlock("closed_skip")
	processRead := fn.NewBlock("process_read")
	checkRead.NewCondBr(isClosed, closedSkip, processRead)

	closedSkip.NewCall(c.palMutexUnlock, pdLock)
	closedSkip.NewBr(eventNext)

	// Check if readable (events & 1) or error (events & 4)
	readOrErr := processRead.NewAnd(events, constant.NewInt(irtypes.I32, pollEventRead|pollEventError))
	hasRead := processRead.NewICmp(enum.IPredNE, readOrErr, constant.NewInt(irtypes.I32, 0))
	afterRead := c.emitNetpollWakeSlot(fn, processRead, pdPtr, pdFieldReadG, netpollDirRead,
		netpollReasonReady, hasRead, wokenAlloca, "wake_read")

	// Check if writable (events & 2) or error
	writeOrErr := afterRead.NewAnd(events, constant.NewInt(irtypes.I32, pollEventWrite|pollEventError))
	hasWrite := afterRead.NewICmp(enum.IPredNE, writeOrErr, constant.NewInt(irtypes.I32, 0))
	afterWrite := c.emitNetpollWakeSlot(fn, afterRead, pdPtr, pdFieldWriteG, netpollDirWrite,
		netpollReasonReady, hasWrite, wokenAlloca, "wake_write")
	afterWrite.NewBr(eventNext)

	// eventNext: unlock PollDesc, increment i
	eventNext.NewCall(c.palMutexUnlock, pdLock)
	iNext := eventNext.NewAdd(iVal, constant.NewInt(irtypes.I32, 1))
	eventNext.NewStore(iNext, iAlloca)
	eventNext.NewBr(eventBody)

	// eventDone: expire due deadlines while the batch lock is still held (T1563),
	// then unlock the batch lock (B0324) and check if any goroutines were woken.
	// If no goroutines were woken, sleep 1ms to prevent spinning on spurious
	// poll events (e.g., WSAPoll returning POLLWRNORM for listening sockets
	// on Windows with no waiting goroutines).
	eventDone.NewCall(scanFn)
	eventDone.NewCall(c.palMutexUnlock, batchLock)
	wokenFinal := eventDone.NewLoad(irtypes.I32, wokenAlloca)
	noneWoken := eventDone.NewICmp(enum.IPredEQ, wokenFinal, constant.NewInt(irtypes.I32, 0))
	spuriousBlk := fn.NewBlock("spurious_sleep")
	eventDone.NewCondBr(noneWoken, spuriousBlk, loop)

	spuriousBlk.NewCall(c.palUsleep, constant.NewInt(irtypes.I32, 1000)) // 1ms
	spuriousBlk.NewBr(loop)

	// exit: free event buffer, return
	exitBlk.NewCall(c.palFree, eventBuf)
	exitBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	c.funcs["promise_netpoll_loop"] = fn
}

// extractI64FromIntArg extracts the raw i64 from an int argument value.
// argVal may be a raw i64 (e.g. field access on a heap type) or a full
// value struct {i8*, T_i*, i64} (e.g. local variable load). Returns i64.
func (c *Compiler) extractI64FromIntArg(argVal value.Value) value.Value {
	if _, ok := argVal.Type().(*irtypes.IntType); ok {
		return argVal // already raw i64
	}
	// Value struct — raw i64 is at field index 2
	return c.block.NewExtractValue(argVal, 2)
}

// --- Inline codegen for netpoll wait operations (T0232) ---
//
// These functions emit IR directly into the current function's block stream,
// exactly like genChannelSend/genChannelRecv. They MUST be called from within
// a function being compiled (c.fn, c.block are set), not from defineXXXFunc.
//
// In goroutine mode (c.inCoroutine): emit coro.suspend with park mutex protocol.
// In thread-blocking mode: emit cond_wait on the PollDesc's condition variable.

// genNetpollWaitRead emits inline code to park the current goroutine until
// the PollDesc's fd is readable. pdArg is the PollDesc pointer as a Promise int.
func (c *Compiler) genNetpollWaitRead(pdArg value.Value) {
	c.genNetpollWait(pdArg, pdFieldReadG, "netpoll.wait_read")
}

// genNetpollWaitWrite emits inline code to park the current goroutine until
// the PollDesc's fd is writable. pdArg is the PollDesc pointer as a Promise int.
func (c *Compiler) genNetpollWaitWrite(pdArg value.Value) {
	c.genNetpollWait(pdArg, pdFieldWriteG, "netpoll.wait_write")
}

// genNetpollWait is the shared implementation for wait_read and wait_write.
// gField is pdFieldReadG or pdFieldWriteG.
func (c *Compiler) genNetpollWait(pdArg value.Value, gField int, prefix string) {
	pdTy := pollDescStructType()
	gTy := goroutineStructType()

	// pdArg is a Promise int (i64) holding the PollDesc pointer. Convert to i8*.
	pdRaw := c.block.NewIntToPtr(pdArg, irtypes.I8Ptr)
	pdPtr := c.block.NewBitCast(pdRaw, irtypes.NewPointer(pdTy))

	// Load pd.lock
	lockField := c.block.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldLock)))
	lock := c.block.NewLoad(irtypes.I8Ptr, lockField)

	// Lock PollDesc
	c.block.NewCall(c.palMutexLock, lock)

	waitGField := c.block.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gField)))

	if c.inCoroutine {
		// Goroutine mode: store real G pointer — reactor enqueues it on wake.
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		c.block.NewStore(currentG, waitGField)

		// Set G.park_mutex = pd.lock (park mutex protocol — scheduler unlocks after suspend)
		gPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTy))
		parkMutexField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(lock, parkMutexField)

		// coro.suspend — reactor thread wakes us by enqueuing G
		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock(prefix + ".resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		// On resume: fd is ready. Scheduler already unlocked pd.lock via G.park_mutex.
		c.block = resumeBlk
	} else {
		// Thread-blocking mode: store sentinel (not real G) — reactor signals
		// cond var instead of enqueuing (B0324).
		sentinel := c.block.NewIntToPtr(
			constant.NewInt(irtypes.I64, netpollCondWaiterSentinel), irtypes.I8Ptr)
		c.block.NewStore(sentinel, waitGField)

		// cond_wait on PollDesc's cond var, with syscall handoff (T1685) so this
		// M's P keeps running while we block on the fd becoming ready.
		condField := c.block.NewGetElementPtr(pdTy, pdPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldCond)))
		cond := c.block.NewLoad(irtypes.I8Ptr, condField)
		c.emitBlockingCondWait(cond, lock)

		// Clear sentinel after wake and unlock
		c.block.NewStore(constant.NewNull(irtypes.I8Ptr), waitGField)
		c.block.NewCall(c.palMutexUnlock, lock)
	}
}
