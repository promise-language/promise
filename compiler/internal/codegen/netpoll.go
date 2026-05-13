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
//   { i32 fd, i32 _pad, i8* read_g, i8* write_g, i8* lock, i8* cond }
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
)

// pollDescStructType returns the LLVM struct type for PollDesc.
func pollDescStructType() *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I32,   // fd
		irtypes.I32,   // _pad
		irtypes.I8Ptr, // read_g
		irtypes.I8Ptr, // write_g
		irtypes.I8Ptr, // lock
		irtypes.I8Ptr, // cond (for thread-blocking fallback, T0232)
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

	// Define loop first (init references it)
	c.defineNetpollLoopFunc()
	c.defineNetpollInitFunc()
	c.defineNetpollOpenFunc()
	c.defineNetpollCloseFunc()
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

	// Start poller thread
	loopFn := c.funcs["promise_netpoll_loop"]
	loopFnPtr := okBlk.NewBitCast(loopFn, irtypes.I8Ptr)
	handle := okBlk.NewCall(c.palThreadCreate, loopFnPtr, constant.NewNull(irtypes.I8Ptr))

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

	// Register with reactor: pal_reactor_add(reactor_fd, fd, pd_ptr)
	rfdField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReactorFd)))
	rfd := entry.NewLoad(irtypes.I32, rfdField)
	entry.NewCall(c.palReactorAdd, rfd, fdParam, pdRaw)

	entry.NewRet(pdRaw)

	c.funcs["promise_netpoll_open"] = fn
}

// defineNetpollCloseFunc emits @promise_netpoll_close(i8* pd) → void
// Unregisters fd from reactor, wakes any waiting Gs, frees PollDesc.
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

	// Wake read_g if waiting
	readGField := entry.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldReadG)))
	readG := entry.NewLoad(irtypes.I8Ptr, readGField)
	hasReadG := entry.NewICmp(enum.IPredNE, readG, constant.NewNull(irtypes.I8Ptr))
	wakeReadBlk := fn.NewBlock(".wake_read")
	checkWriteBlk := fn.NewBlock(".check_write")
	entry.NewCondBr(hasReadG, wakeReadBlk, checkWriteBlk)

	// Check sentinel vs real G (B0324)
	wakeReadBlk.NewStore(constant.NewNull(irtypes.I8Ptr), readGField)
	closeSentinelR := wakeReadBlk.NewIntToPtr(
		constant.NewInt(irtypes.I64, netpollCondWaiterSentinel), irtypes.I8Ptr)
	closeIsSentinelR := wakeReadBlk.NewICmp(enum.IPredEQ, readG, closeSentinelR)
	closeEnqueueR := fn.NewBlock(".wake_read.enqueue")
	closeCondR := fn.NewBlock(".wake_read.cond")
	wakeReadBlk.NewCondBr(closeIsSentinelR, closeCondR, closeEnqueueR)

	closeEnqueueR.NewCall(c.funcs["promise_sched_enqueue"], readG)
	closeEnqueueR.NewBr(closeCondR)

	condFieldR := closeCondR.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldCond)))
	condR := closeCondR.NewLoad(irtypes.I8Ptr, condFieldR)
	closeCondR.NewCall(c.palCondSignal, condR)
	closeCondR.NewBr(checkWriteBlk)

	// Wake write_g if waiting
	writeGField := checkWriteBlk.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldWriteG)))
	writeG := checkWriteBlk.NewLoad(irtypes.I8Ptr, writeGField)
	hasWriteG := checkWriteBlk.NewICmp(enum.IPredNE, writeG, constant.NewNull(irtypes.I8Ptr))
	wakeWriteBlk := fn.NewBlock(".wake_write")
	cleanupBlk := fn.NewBlock(".cleanup")
	checkWriteBlk.NewCondBr(hasWriteG, wakeWriteBlk, cleanupBlk)

	// Check sentinel vs real G (B0324)
	wakeWriteBlk.NewStore(constant.NewNull(irtypes.I8Ptr), writeGField)
	closeSentinelW := wakeWriteBlk.NewIntToPtr(
		constant.NewInt(irtypes.I64, netpollCondWaiterSentinel), irtypes.I8Ptr)
	closeIsSentinelW := wakeWriteBlk.NewICmp(enum.IPredEQ, writeG, closeSentinelW)
	closeEnqueueW := fn.NewBlock(".wake_write.enqueue")
	closeCondW := fn.NewBlock(".wake_write.cond")
	wakeWriteBlk.NewCondBr(closeIsSentinelW, closeCondW, closeEnqueueW)

	closeEnqueueW.NewCall(c.funcs["promise_sched_enqueue"], writeG)
	closeEnqueueW.NewBr(closeCondW)

	condFieldW := closeCondW.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldCond)))
	condW := closeCondW.NewLoad(irtypes.I8Ptr, condFieldW)
	closeCondW.NewCall(c.palCondSignal, condW)
	closeCondW.NewBr(cleanupBlk)

	// Mark closed (fd = -1) so reactor skips stale events (B0324)
	cleanupBlk.NewStore(constant.NewInt(irtypes.I32, -1), fdField)
	cleanupBlk.NewCall(c.palMutexUnlock, lock)

	// Synchronize with reactor: acquire batch lock to ensure the current event
	// batch is fully processed. After this, no stale references to this PD
	// remain (pal_reactor_del prevents future batches; fd==-1 skips current).
	batchLockVal := cleanupBlk.NewLoad(irtypes.I8Ptr, c.netpollBatchLock)
	cleanupBlk.NewCall(c.palMutexLock, batchLockVal)
	cleanupBlk.NewCall(c.palMutexUnlock, batchLockVal)

	// Now safe to destroy and free — no concurrent access possible
	cleanupBlk.NewCall(c.palMutexDestroy, lock)
	condField := cleanupBlk.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldCond)))
	condVal := cleanupBlk.NewLoad(irtypes.I8Ptr, condField)
	cleanupBlk.NewCall(c.palCondDestroy, condVal)
	cleanupBlk.NewCall(c.palFree, pdParam)
	cleanupBlk.NewRet(nil)

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

	entry := fn.NewBlock(".entry")
	loop := fn.NewBlock("loop")
	processEvents := fn.NewBlock("process_events")
	eventLoop := fn.NewBlock("event_loop")
	eventBody := fn.NewBlock("event_body")
	checkRead := fn.NewBlock("check_read")
	wakeRead := fn.NewBlock("wake_read")
	checkWrite := fn.NewBlock("check_write")
	wakeWrite := fn.NewBlock("wake_write")
	eventNext := fn.NewBlock("event_next")
	eventDone := fn.NewBlock("event_done")
	exitBlk := fn.NewBlock("exit")

	// Heap-allocate event buffer (lives for the duration of the thread)
	eventBufSize := constant.NewInt(irtypes.I64, int64(maxPollEvents*16)) // 16 bytes per PollEvent
	eventBuf := entry.NewCall(c.palAlloc, eventBufSize)
	iAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewBr(loop)

	// loop: check shutdown, then poll
	shutdownField := loop.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldShutdown)))
	shutdownVal := loop.NewLoad(irtypes.I8, shutdownField)
	isShutdown := loop.NewICmp(enum.IPredNE, shutdownVal, constant.NewInt(irtypes.I8, 0))
	loop.NewCondBr(isShutdown, exitBlk, processEvents)

	// processEvents: call pal_reactor_poll
	rfdField := processEvents.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReactorFd)))
	rfd := processEvents.NewLoad(irtypes.I32, rfdField)
	count := processEvents.NewCall(c.palReactorPoll, rfd, eventBuf,
		constant.NewInt(irtypes.I32, maxPollEvents),
		constant.NewInt(irtypes.I32, 10)) // 10ms timeout

	hasEvents := processEvents.NewICmp(enum.IPredSGT, count, constant.NewInt(irtypes.I32, 0))
	processEvents.NewCondBr(hasEvents, eventLoop, loop)

	// eventLoop: lock batch lock, iterate events
	batchLock := eventLoop.NewLoad(irtypes.I8Ptr, c.netpollBatchLock)
	eventLoop.NewCall(c.palMutexLock, batchLock)
	eventLoop.NewStore(constant.NewInt(irtypes.I32, 0), iAlloca)
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

	// Load read_g
	readGField := processRead.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldReadG)))
	readG := processRead.NewLoad(irtypes.I8Ptr, readGField)
	readGNonNull := processRead.NewICmp(enum.IPredNE, readG, constant.NewNull(irtypes.I8Ptr))
	shouldWakeRead := processRead.NewAnd(hasRead, readGNonNull)
	processRead.NewCondBr(shouldWakeRead, wakeRead, checkWrite)

	// wakeRead: clear read_g, check sentinel vs real G (B0324)
	wakeRead.NewStore(constant.NewNull(irtypes.I8Ptr), readGField)
	sentinelR := wakeRead.NewIntToPtr(
		constant.NewInt(irtypes.I64, netpollCondWaiterSentinel), irtypes.I8Ptr)
	isSentinelR := wakeRead.NewICmp(enum.IPredEQ, readG, sentinelR)
	wakeReadEnqueue := fn.NewBlock("wake_read.enqueue")
	wakeReadCond := fn.NewBlock("wake_read.cond")
	wakeRead.NewCondBr(isSentinelR, wakeReadCond, wakeReadEnqueue)

	// Real G: enqueue on run queue
	wakeReadEnqueue.NewCall(c.funcs["promise_sched_enqueue"], readG)
	wakeReadEnqueue.NewBr(wakeReadCond)

	// Signal cond var (wakes thread-blocking waiters; no-op if none waiting)
	pdCondFieldR := wakeReadCond.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldCond)))
	pdCondR := wakeReadCond.NewLoad(irtypes.I8Ptr, pdCondFieldR)
	wakeReadCond.NewCall(c.palCondSignal, pdCondR)
	wakeReadCond.NewBr(checkWrite)

	// checkWrite: check if writable (events & 2) or error
	writeOrErr := checkWrite.NewAnd(events, constant.NewInt(irtypes.I32, pollEventWrite|pollEventError))
	hasWrite := checkWrite.NewICmp(enum.IPredNE, writeOrErr, constant.NewInt(irtypes.I32, 0))

	writeGField := checkWrite.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldWriteG)))
	writeG := checkWrite.NewLoad(irtypes.I8Ptr, writeGField)
	writeGNonNull := checkWrite.NewICmp(enum.IPredNE, writeG, constant.NewNull(irtypes.I8Ptr))
	shouldWakeWrite := checkWrite.NewAnd(hasWrite, writeGNonNull)
	checkWrite.NewCondBr(shouldWakeWrite, wakeWrite, eventNext)

	// wakeWrite: clear write_g, check sentinel vs real G (B0324)
	wakeWrite.NewStore(constant.NewNull(irtypes.I8Ptr), writeGField)
	sentinelW := wakeWrite.NewIntToPtr(
		constant.NewInt(irtypes.I64, netpollCondWaiterSentinel), irtypes.I8Ptr)
	isSentinelW := wakeWrite.NewICmp(enum.IPredEQ, writeG, sentinelW)
	wakeWriteEnqueue := fn.NewBlock("wake_write.enqueue")
	wakeWriteCond := fn.NewBlock("wake_write.cond")
	wakeWrite.NewCondBr(isSentinelW, wakeWriteCond, wakeWriteEnqueue)

	// Real G: enqueue on run queue
	wakeWriteEnqueue.NewCall(c.funcs["promise_sched_enqueue"], writeG)
	wakeWriteEnqueue.NewBr(wakeWriteCond)

	// Signal cond var (wakes thread-blocking waiters; no-op if none waiting)
	pdCondFieldW := wakeWriteCond.NewGetElementPtr(pdTy, pdPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldCond)))
	pdCondW := wakeWriteCond.NewLoad(irtypes.I8Ptr, pdCondFieldW)
	wakeWriteCond.NewCall(c.palCondSignal, pdCondW)
	wakeWriteCond.NewBr(eventNext)

	// eventNext: unlock PollDesc, increment i
	eventNext.NewCall(c.palMutexUnlock, pdLock)
	iNext := eventNext.NewAdd(iVal, constant.NewInt(irtypes.I32, 1))
	eventNext.NewStore(iNext, iAlloca)
	eventNext.NewBr(eventBody)

	// eventDone: unlock batch lock, loop back (B0324)
	eventDone.NewCall(c.palMutexUnlock, batchLock)
	eventDone.NewBr(loop)

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

		// cond_wait on PollDesc's cond var
		condField := c.block.NewGetElementPtr(pdTy, pdPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pdFieldCond)))
		cond := c.block.NewLoad(irtypes.I8Ptr, condField)
		c.block.NewCall(c.palCondWait, cond, lock)

		// Clear sentinel after wake and unlock
		c.block.NewStore(constant.NewNull(irtypes.I8Ptr), waitGField)
		c.block.NewCall(c.palMutexUnlock, lock)
	}
}
