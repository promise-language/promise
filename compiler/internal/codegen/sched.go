package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- G (Goroutine) struct ---

// G struct field indices.
const (
	gFieldCoroHandle  = 0  // i8*  LLVM coroutine handle
	gFieldStatus      = 1  // i8   0=idle, 1=runnable, 2=running, 3=waiting, 4=dead
	gFieldWaitData    = 2  // i8*  context: &value for chan send, &result for chan recv
	gFieldSchedNext   = 3  // i8*  next G in run queue (intrusive linked list)
	gFieldWaitNext    = 4  // i8*  next G in channel wait queue
	gFieldID          = 5  // i64  goroutine ID (monotonic counter)
	gFieldResultPtr   = 6  // i8*  for task[T]: heap-allocated result storage
	gFieldDone        = 7  // i8   for task[T]: completion flag (0=running, 1=done)
	gFieldDoneWaiters = 8  // i8*  for task[T]: head of Gs waiting on <-task
	gFieldParkMutex   = 9  // i8*  mutex to release after coro.suspend completes
	gFieldPreempt     = 10 // i8   1 when sysmon requests preemption
	gFieldSelectCase  = 11 // i32  which select case was triggered (-1 = none)
	gFieldPanicked    = 12 // i8   0=not panicked, 1=panicked (.rodata msg), 2=panicked (heap msg)
	gFieldPanicMsg    = 13 // i8*  panic message (null-terminated)
)

// G.panicked field values. promise_panic sets gPanickedRodata (C string, may be .rodata),
// promise_panic_msg sets gPanickedHeapMsg (heap-allocated copy, must free in goroutine_exit).
const (
	gPanickedRodata  = 1 // panic_msg is a C string (may be .rodata — don't free)
	gPanickedHeapMsg = 2 // panic_msg is a heap-allocated copy (must free)
)

// G status values.
const (
	gStatusIdle     = 0
	gStatusRunnable = 1
	gStatusRunning  = 2
	gStatusWaiting  = 3
	gStatusDead     = 4
)

// goroutineStructType returns the LLVM struct type for a goroutine (G).
// Layout: { i8*, i8, i8*, i8*, i8*, i64, i8*, i8, i8*, i8*, i8, i32, i8, i8* } — 14 fields.
func goroutineStructType() *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I8Ptr, // coro_handle
		irtypes.I8,    // status
		irtypes.I8Ptr, // wait_data
		irtypes.I8Ptr, // sched_next
		irtypes.I8Ptr, // wait_next
		irtypes.I64,   // id
		irtypes.I8Ptr, // result_ptr
		irtypes.I8,    // done
		irtypes.I8Ptr, // done_waiters
		irtypes.I8Ptr, // park_mutex
		irtypes.I8,    // preempt
		irtypes.I32,   // select_case
		irtypes.I8,    // panicked
		irtypes.I8Ptr, // panic_msg
	)
}

// --- P (Processor) struct ---

// P run queue size (fixed-size circular buffer).
const pRunQueueSize = 256

// P struct field indices.
const (
	pFieldID        = 0 // i32  processor id
	pFieldRunQueue  = 1 // [256 x i8*]  circular buffer of G pointers
	pFieldRqHead    = 2 // i64  dequeue index
	pFieldRqTail    = 3 // i64  enqueue index
	pFieldCurrentG  = 4 // i8*  currently running G
	pFieldM         = 5 // i8*  associated M (OS thread)
	pFieldLock      = 6 // i8*  mutex for queue overflow
	pFieldSchedTick = 7 // i64  incremented at runG and find_runnable; used by find_runnable's global-first check (% 61 == 0)
)

// processorStructType returns the LLVM struct type for a processor (P).
func processorStructType() *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I32, // id
		irtypes.NewArray(pRunQueueSize, irtypes.I8Ptr), // run_queue
		irtypes.I64,   // rq_head
		irtypes.I64,   // rq_tail
		irtypes.I8Ptr, // current_g
		irtypes.I8Ptr, // m
		irtypes.I8Ptr, // lock
		irtypes.I64,   // sched_tick
	)
}

// --- M (Machine/OS Thread) struct ---

// M struct field indices.
const (
	mFieldP            = 0 // i8*  associated P (null when parked)
	mFieldThreadHandle = 1 // i8*  PAL thread handle
	mFieldParkMutex    = 2 // i8*  mutex for parking
	mFieldParkCond     = 3 // i8*  cond var for waking
	mFieldSpinning     = 4 // i8   1 if looking for work
)

// machineStructType returns the LLVM struct type for an OS thread (M).
func machineStructType() *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I8Ptr, // p
		irtypes.I8Ptr, // thread_handle
		irtypes.I8Ptr, // park_mutex
		irtypes.I8Ptr, // park_cond
		irtypes.I8,    // spinning
	)
}

// --- Sched (Global Scheduler) struct ---

// Sched struct field indices.
const (
	schedFieldGlobalHead       = 0  // i8*  global run queue head
	schedFieldGlobalTail       = 1  // i8*  global run queue tail
	schedFieldGlobalSize       = 2  // i64  number of Gs in global queue
	schedFieldGlobalLock       = 3  // i8*  mutex protecting global queue
	schedFieldPs               = 4  // i8*  pointer to array of P*
	schedFieldNumP             = 5  // i32  number of Ps
	schedFieldIdleMHead        = 6  // i8*  stack of parked Ms
	schedFieldIdleMLock        = 7  // i8*  mutex protecting idle M list
	schedFieldGoroutineCounter = 8  // i64  monotonic G ID counter
	schedFieldShutdown         = 9  // i8   1 when shutting down
	schedFieldMainDone         = 10 // i8   1 when main goroutine completed
	schedFieldMainDoneMutex    = 11 // i8*  mutex for main done signaling
	schedFieldMainDoneCond     = 12 // i8*  cond var for main done signaling
	schedFieldDoneLock         = 13 // i8*  mutex protecting task G.done + G.done_waiters
	schedFieldMaxP             = 14 // i32  max number of Ps (from initial num_cpus)
	schedFieldGsCreated        = 15 // i64  total goroutines created
	schedFieldGsCompleted      = 16 // i64  total goroutines completed
	schedFieldContextSwitches  = 17 // i64  total context switches
	schedFieldSteals           = 18 // i64  total work steals
	schedFieldSysmonHandle     = 19 // i8*  sysmon thread handle (for joining at shutdown)
	schedFieldReadyCount       = 20 // i32  worker threads that completed init (B0165)
	schedFieldReactorFd        = 21 // i32  epoll/kqueue fd (-1 if no reactor)
	schedFieldReactorThread    = 22 // i8*  reactor poller thread handle (null if no reactor)
	schedFieldReactorLock      = 23 // i8*  mutex protecting PollDesc table (null if no reactor)
)

// schedStructType returns the LLVM struct type for the global scheduler.
func schedStructType() *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I8Ptr, // global_head
		irtypes.I8Ptr, // global_tail
		irtypes.I64,   // global_size
		irtypes.I8Ptr, // global_lock
		irtypes.I8Ptr, // ps (pointer to P* array)
		irtypes.I32,   // num_p
		irtypes.I8Ptr, // idle_m_head
		irtypes.I8Ptr, // idle_m_lock
		irtypes.I64,   // goroutine_counter
		irtypes.I8,    // shutdown
		irtypes.I8,    // main_done
		irtypes.I8Ptr, // main_done_mutex
		irtypes.I8Ptr, // main_done_cond
		irtypes.I8Ptr, // done_lock
		irtypes.I32,   // max_p
		irtypes.I64,   // gs_created
		irtypes.I64,   // gs_completed
		irtypes.I64,   // context_switches
		irtypes.I64,   // steals
		irtypes.I8Ptr, // sysmon_handle
		irtypes.I32,   // ready_count (B0165: worker threads that completed init)
		irtypes.I32,   // reactor_fd (T0070: epoll/kqueue fd, -1 if no reactor)
		irtypes.I8Ptr, // reactor_thread (T0070: poller thread handle)
		irtypes.I8Ptr, // reactor_lock (T0070: mutex for PollDesc table)
	)
}

// --- Scheduler globals and initialization ---

// emitAtomicAdd emits an atomic add on native targets, or a regular load+add+store on WASM
// (single-threaded, no atomics needed). Returns the old value.
func (c *Compiler) emitAtomicAdd(blk *ir.Block, ptr value.Value, val value.Value, typ *irtypes.IntType) value.Value {
	if c.isWasm {
		old := blk.NewLoad(typ, ptr)
		blk.NewStore(blk.NewAdd(old, val), ptr)
		return old
	}
	return blk.NewAtomicRMW(enum.AtomicOpAdd, ptr, val, enum.AtomicOrderingMonotonic)
}

// emitAtomicAddRelease is like emitAtomicAdd but uses Release ordering on
// non-WASM targets. The Release barrier ensures all prior stores (e.g.
// alloc_count decrements) are visible to any thread that does an Acquire
// load of the same variable. Used for gs_completed increments (B0320).
func (c *Compiler) emitAtomicAddRelease(blk *ir.Block, ptr value.Value, val value.Value, typ *irtypes.IntType) value.Value {
	if c.isWasm {
		old := blk.NewLoad(typ, ptr)
		blk.NewStore(blk.NewAdd(old, val), ptr)
		return old
	}
	return blk.NewAtomicRMW(enum.AtomicOpAdd, ptr, val, enum.AtomicOrderingRelease)
}

// defineSchedulerGlobals defines the thread-local current-G pointer and the
// global scheduler singleton, and wires them into the compiler.
func (c *Compiler) defineSchedulerGlobals() {
	// @__promise_current_g = [thread_local] global i8* null
	currentG := c.module.NewGlobal("__promise_current_g", irtypes.I8Ptr)
	currentG.Init = constant.NewNull(irtypes.I8Ptr)
	if !c.isWasm {
		currentG.TLSModel = enum.TLSModelGeneric
	}
	c.currentGGlobal = currentG

	// @__promise_current_p = [thread_local] global i8* null
	// Used by local queue operations to find the current P without going through M.
	currentP := c.module.NewGlobal("__promise_current_p", irtypes.I8Ptr)
	currentP.Init = constant.NewNull(irtypes.I8Ptr)
	if !c.isWasm {
		currentP.TLSModel = enum.TLSModelGeneric
	}
	c.currentPGlobal = currentP

	// @__promise_current_m = [thread_local] global i8* null
	// Used by syscall handoff: exit_syscall needs M to reattach P.
	currentM := c.module.NewGlobal("__promise_current_m", irtypes.I8Ptr)
	currentM.Init = constant.NewNull(irtypes.I8Ptr)
	if !c.isWasm {
		currentM.TLSModel = enum.TLSModelGeneric
	}
	c.currentMGlobal = currentM

	// @__promise_sched = global %Sched zeroinitializer
	schedTy := schedStructType()
	sched := c.module.NewGlobal("__promise_sched", schedTy)
	sched.Init = constant.NewZeroInitializer(schedTy)
	c.schedGlobal = sched

	// @__promise_test_panic_msg = global i8* null (non-TLS)
	// Used by per-test panic recovery to pass the panic message from the test
	// thread back to the main thread. Non-TLS because the main thread reads it
	// after pal_thread_join (tests run sequentially, so no race).
	testPanicMsg := c.module.NewGlobal("__promise_test_panic_msg", irtypes.I8Ptr)
	testPanicMsg.Init = constant.NewNull(irtypes.I8Ptr)
	c.testPanicMsgGlobal = testPanicMsg

	// @__promise_test_panic_type = global i8 0 (non-TLS)
	// Mirrors __promise_panic_type (TLS) for the test harness: 0=none, 1=rodata, 2=heap.
	// The batch test main uses this to decide whether to pal_free the panic msg (T0275).
	testPanicType := c.module.NewGlobal("__promise_test_panic_type", irtypes.I8)
	testPanicType.Init = constant.NewInt(irtypes.I8, 0)
	c.testPanicTypeGlobal = testPanicType

	// @__promise_test_done = global i32 0 (non-TLS)
	// Set atomically to 1 by the test trampoline when the test completes
	// (both normal return and panic recovery). The main thread polls this
	// with usleep to enforce per-test timeouts. Non-TLS because the main
	// thread reads it after the test thread sets it (tests run sequentially).
	testDone := c.module.NewGlobal("__promise_test_done", irtypes.I32)
	testDone.Init = constant.NewInt(irtypes.I32, 0)
	c.testDoneGlobal = testDone

	// @__promise_panic_flag = [thread_local] global i8 0
	// Set to 1 when a panic is in flight on this thread.
	panicFlag := c.module.NewGlobal("__promise_panic_flag", irtypes.I8)
	panicFlag.Init = constant.NewInt(irtypes.I8, 0)
	if !c.isWasm {
		panicFlag.TLSModel = enum.TLSModelGeneric
	}
	c.panicFlagGlobal = panicFlag

	// @__promise_panic_msg = [thread_local] global i8* null
	// Points to the C string panic message for the current panic.
	panicMsgTls := c.module.NewGlobal("__promise_panic_msg", irtypes.I8Ptr)
	panicMsgTls.Init = constant.NewNull(irtypes.I8Ptr)
	if !c.isWasm {
		panicMsgTls.TLSModel = enum.TLSModelGeneric
	}
	c.panicMsgTlsGlobal = panicMsgTls

	// @__promise_panic_type = [thread_local] global i8 0
	// Indicates the allocation type of the panic message: 1=.rodata, 2=heap-allocated.
	panicTypeTls := c.module.NewGlobal("__promise_panic_type", irtypes.I8)
	panicTypeTls.Init = constant.NewInt(irtypes.I8, 0)
	if !c.isWasm {
		panicTypeTls.TLSModel = enum.TLSModelGeneric
	}
	c.panicTypeTlsGlobal = panicTypeTls
}

// defineGNewFunc emits @promise_g_new(i8* %coro_handle) → i8*
// Allocates and initializes a G struct with the given coroutine handle.
func (c *Compiler) defineGNewFunc() {
	handleParam := ir.NewParam("coro_handle", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_g_new", irtypes.I8Ptr, handleParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	gType := goroutineStructType()
	entry := fn.NewBlock(".entry")

	// Allocate G struct
	structSize := constant.NewInt(irtypes.I64, int64(c.typeSize(gType)))
	rawPtr := entry.NewCall(c.palAlloc, structSize)
	gPtr := entry.NewBitCast(rawPtr, irtypes.NewPointer(gType))

	// Store coro_handle
	handleField := entry.NewGetElementPtr(gType, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldCoroHandle)))
	entry.NewStore(handleParam, handleField)

	// Store status = runnable (1)
	statusField := entry.NewGetElementPtr(gType, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	entry.NewStore(constant.NewInt(irtypes.I8, gStatusRunnable), statusField)

	// Store wait_data = null
	wdField := entry.NewGetElementPtr(gType, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldWaitData)))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), wdField)

	// Store sched_next = null
	snField := entry.NewGetElementPtr(gType, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldSchedNext)))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), snField)

	// Store wait_next = null
	wnField := entry.NewGetElementPtr(gType, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldWaitNext)))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), wnField)

	// Assign goroutine ID: atomic increment of sched.goroutine_counter.
	// atomicrmw returns the old value, which becomes the new G's unique ID.
	schedTy := schedStructType()
	counterField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGoroutineCounter)))
	counter := c.emitAtomicAdd(entry, counterField, constant.NewInt(irtypes.I64, 1), irtypes.I64)

	// Store id
	idField := entry.NewGetElementPtr(gType, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldID)))
	entry.NewStore(counter, idField)

	// Store result_ptr = null
	rpField := entry.NewGetElementPtr(gType, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), rpField)

	// Store done = 0
	doneField := entry.NewGetElementPtr(gType, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldDone)))
	entry.NewStore(constant.NewInt(irtypes.I8, 0), doneField)

	// Store done_waiters = null
	dwField := entry.NewGetElementPtr(gType, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldDoneWaiters)))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), dwField)

	// Store park_mutex = null
	pmField := entry.NewGetElementPtr(gType, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), pmField)

	// Store select_case = -1 (no select)
	scField := entry.NewGetElementPtr(gType, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldSelectCase)))
	entry.NewStore(constant.NewInt(irtypes.I32, 0xFFFFFFFF), scField) // -1 as unsigned i32

	// Store panicked = 0
	panickedField := entry.NewGetElementPtr(gType, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
	entry.NewStore(constant.NewInt(irtypes.I8, 0), panickedField)

	// Store panic_msg = null
	panicMsgField := entry.NewGetElementPtr(gType, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), panicMsgField)

	// Increment gs_created counter (Release — pairs with Acquire read in
	// the drain spin-wait loop so ARM64 drain checks see the latest count; B0315)
	schedTyLocal := schedStructType()
	gsCreatedField := entry.NewGetElementPtr(schedTyLocal, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGsCreated)))
	c.emitAtomicAddRelease(entry, gsCreatedField, constant.NewInt(irtypes.I64, 1), irtypes.I64)

	entry.NewRet(rawPtr)

	c.funcs["promise_g_new"] = fn
}

// --- Scheduler core functions ---

// defineSchedInitFunc emits @promise_sched_init(i32 %num_cpus) → void
// Allocates P array, creates M worker threads, initializes all scheduler state.
func (c *Compiler) defineSchedInitFunc() {
	numCPUsParam := ir.NewParam("num_cpus", irtypes.I32)
	fn := c.module.NewFunc("promise_sched_init", irtypes.Void, numCPUsParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	schedTy := schedStructType()
	pTy := processorStructType()
	mTy := machineStructType()

	entry := fn.NewBlock(".entry")

	// Init global queue lock
	glLock := entry.NewCall(c.palMutexInit)
	glLockField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalLock)))
	entry.NewStore(glLock, glLockField)

	// Init idle M lock
	imLock := entry.NewCall(c.palMutexInit)
	imLockField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldIdleMLock)))
	entry.NewStore(imLock, imLockField)

	// Init main done mutex + cond
	mdMutex := entry.NewCall(c.palMutexInit)
	mdMutexField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDoneMutex)))
	entry.NewStore(mdMutex, mdMutexField)

	mdCond := entry.NewCall(c.palCondInit)
	mdCondField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDoneCond)))
	entry.NewStore(mdCond, mdCondField)

	// Init done_lock (protects task G.done + G.done_waiters)
	doneLock := entry.NewCall(c.palMutexInit)
	doneLockField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldDoneLock)))
	entry.NewStore(doneLock, doneLockField)

	// Init reactor_fd = -1 (no reactor by default, T0070)
	reactorFdField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReactorFd)))
	entry.NewStore(constant.NewInt(irtypes.I32, -1), reactorFdField)

	// Store num_p and max_p
	numPField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldNumP)))
	entry.NewStore(numCPUsParam, numPField)

	maxPField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMaxP)))
	entry.NewStore(numCPUsParam, maxPField)

	// Allocate P array: num_cpus * sizeof(P)
	numP64 := entry.NewZExt(numCPUsParam, irtypes.I64)
	pSize := constant.NewInt(irtypes.I64, int64(c.typeSize(pTy)))
	totalPSize := entry.NewMul(numP64, pSize)
	psRaw := entry.NewCall(c.palAlloc, totalPSize)

	// Store ps pointer
	psField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldPs)))
	entry.NewStore(psRaw, psField)

	// Two-phase init: first init all P/M structs, then create threads.
	// This prevents work-stealing threads from accessing uninitialized P locks.
	iAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), iAlloca)

	// --- Phase 1: Init all P and M structs ---
	initHeader := fn.NewBlock("init_loop_header")
	initBody := fn.NewBlock("init_loop_body")
	initEnd := fn.NewBlock("init_loop_end")

	entry.NewBr(initHeader)

	iVal := initHeader.NewLoad(irtypes.I32, iAlloca)
	cond := initHeader.NewICmp(enum.IPredSLT, iVal, numCPUsParam)
	initHeader.NewCondBr(cond, initBody, initEnd)

	iVal2 := initBody.NewLoad(irtypes.I32, iAlloca)
	i64Val := initBody.NewZExt(iVal2, irtypes.I64)

	psTyped := initBody.NewBitCast(psRaw, irtypes.NewPointer(pTy))
	pPtr := initBody.NewGetElementPtr(pTy, psTyped, i64Val)

	// P.id = i
	pIdField := initBody.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldID)))
	initBody.NewStore(iVal2, pIdField)

	// P.rq_head = 0
	pHeadField := initBody.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRqHead)))
	initBody.NewStore(constant.NewInt(irtypes.I64, 0), pHeadField)

	// P.rq_tail = 0
	pTailField := initBody.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRqTail)))
	initBody.NewStore(constant.NewInt(irtypes.I64, 0), pTailField)

	// P.current_g = null
	pCurGField := initBody.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldCurrentG)))
	initBody.NewStore(constant.NewNull(irtypes.I8Ptr), pCurGField)

	// P.lock = pal_mutex_init()
	pLock := initBody.NewCall(c.palMutexInit)
	pLockField := initBody.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldLock)))
	initBody.NewStore(pLock, pLockField)

	// Allocate and init M for this P
	mSize := constant.NewInt(irtypes.I64, int64(c.typeSize(mTy)))
	mRaw := initBody.NewCall(c.palAlloc, mSize)
	mPtr := initBody.NewBitCast(mRaw, irtypes.NewPointer(mTy))

	// M.p = pPtr as i8*
	mPField := initBody.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldP)))
	pAsI8 := initBody.NewBitCast(pPtr, irtypes.I8Ptr)
	initBody.NewStore(pAsI8, mPField)

	// M.park_mutex = pal_mutex_init()
	mParkMtx := initBody.NewCall(c.palMutexInit)
	mPmField := initBody.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkMutex)))
	initBody.NewStore(mParkMtx, mPmField)

	// M.park_cond = pal_cond_init()
	mParkCond := initBody.NewCall(c.palCondInit)
	mPcField := initBody.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkCond)))
	initBody.NewStore(mParkCond, mPcField)

	// M.spinning = 0
	mSpinField := initBody.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldSpinning)))
	initBody.NewStore(constant.NewInt(irtypes.I8, 0), mSpinField)

	// P.m = mPtr as i8*
	pMField := initBody.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldM)))
	mAsI8 := initBody.NewBitCast(mPtr, irtypes.I8Ptr)
	initBody.NewStore(mAsI8, pMField)

	// i++
	nextI := initBody.NewAdd(iVal2, constant.NewInt(irtypes.I32, 1))
	initBody.NewStore(nextI, iAlloca)
	initBody.NewBr(initHeader)

	if c.isWasm {
		// WASM: no threads, no sysmon — cooperative scheduler only
		initEnd.NewRet(nil)
	} else {
		// --- Phase 2: Create worker threads (all Ps fully initialized) ---
		threadHeader := fn.NewBlock("thread_loop_header")
		threadBody := fn.NewBlock("thread_loop_body")
		loopEnd := fn.NewBlock("thread_loop_end")

		// Reset counter
		initEnd.NewStore(constant.NewInt(irtypes.I32, 0), iAlloca)
		initEnd.NewBr(threadHeader)

		tVal := threadHeader.NewLoad(irtypes.I32, iAlloca)
		tCond := threadHeader.NewICmp(enum.IPredSLT, tVal, numCPUsParam)
		threadHeader.NewCondBr(tCond, threadBody, loopEnd)

		tVal2 := threadBody.NewLoad(irtypes.I32, iAlloca)
		t64Val := threadBody.NewZExt(tVal2, irtypes.I64)

		// Get P[i].m
		psTyped2 := threadBody.NewBitCast(psRaw, irtypes.NewPointer(pTy))
		tPPtr := threadBody.NewGetElementPtr(pTy, psTyped2, t64Val)
		tMField := threadBody.NewGetElementPtr(pTy, tPPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldM)))
		tMRaw := threadBody.NewLoad(irtypes.I8Ptr, tMField)
		tMPtr := threadBody.NewBitCast(tMRaw, irtypes.NewPointer(mTy))

		// Create thread: pal_thread_create(sched_loop_fn, mRaw)
		schedLoopFn := c.funcs["promise_sched_loop"]
		loopFnPtr := threadBody.NewBitCast(schedLoopFn, irtypes.I8Ptr)
		handle := threadBody.NewCall(c.palThreadCreate, loopFnPtr, tMRaw)

		// M.thread_handle = handle
		mThField := threadBody.NewGetElementPtr(mTy, tMPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldThreadHandle)))
		threadBody.NewStore(handle, mThField)

		// i++
		tNextI := threadBody.NewAdd(tVal2, constant.NewInt(irtypes.I32, 1))
		threadBody.NewStore(tNextI, iAlloca)
		threadBody.NewBr(threadHeader)

		// Start sysmon thread (sets G.preempt on running Gs periodically)
		sysmonFn := c.funcs["promise_sysmon"]
		sysmonFnPtr := loopEnd.NewBitCast(sysmonFn, irtypes.I8Ptr)
		sysmonHandle := loopEnd.NewCall(c.palThreadCreate, sysmonFnPtr, constant.NewNull(irtypes.I8Ptr))

		// Store sysmon thread handle for joining at shutdown
		sysmonField := loopEnd.NewGetElementPtr(schedTy, c.schedGlobal,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldSysmonHandle)))
		loopEnd.NewStore(sysmonHandle, sysmonField)

		// Done
		loopEnd.NewRet(nil)
	}

	c.funcs["promise_sched_init"] = fn
}

// defineSchedLoopFunc emits @promise_sched_loop(i8* %m_raw) → i8*
// This is the main loop for each worker M (OS thread).
func (c *Compiler) defineSchedLoopFunc() {
	mParam := ir.NewParam("m_raw", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_sched_loop", irtypes.I8Ptr, mParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	gTy := goroutineStructType()
	mTy := machineStructType()
	schedTy := schedStructType()

	entry := fn.NewBlock(".entry")
	loop := fn.NewBlock("loop")
	checkShutdown := fn.NewBlock("check_shutdown")
	runG := fn.NewBlock("run_g")
	afterResume := fn.NewBlock("after_resume")
	coroDoneBlk := fn.NewBlock("coro_done")
	coroSuspendedBlk := fn.NewBlock("coro_suspended")
	parkM := fn.NewBlock("park_m")
	exitBlk := fn.NewBlock("exit")

	// Set up per-thread alternate signal stack for stack overflow detection (B0010)
	entry.NewCall(c.palStackOverflowThreadInit)

	// Signal that this worker thread has completed init (B0165).
	// Batch test mode spin-waits on this counter before resetting alloc count,
	// ensuring async pal_stack_overflow_thread_init allocations are excluded
	// from per-test leak detection.
	readyField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReadyCount)))
	c.emitAtomicAdd(entry, readyField, constant.NewInt(irtypes.I32, 1), irtypes.I32)

	// Set TLS current_m once (M is fixed for this thread's lifetime).
	entry.NewStore(mParam, c.currentMGlobal)
	entry.NewBr(loop)

	// loop: find runnable G
	// Get P from M
	mPtr := loop.NewBitCast(mParam, irtypes.NewPointer(mTy))
	pPtrField := loop.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldP)))
	pRaw := loop.NewLoad(irtypes.I8Ptr, pPtrField)

	// Check shutdown
	shutdownField := loop.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldShutdown)))
	shutdownVal := loop.NewLoad(irtypes.I8, shutdownField)
	isShutdown := loop.NewICmp(enum.IPredNE, shutdownVal, constant.NewInt(irtypes.I8, 0))
	loop.NewCondBr(isShutdown, exitBlk, checkShutdown)

	// checkShutdown: try to find work
	gRaw := checkShutdown.NewCall(c.funcs["promise_sched_find_runnable"], pRaw)
	isNull := checkShutdown.NewICmp(enum.IPredEQ, gRaw, constant.NewNull(irtypes.I8Ptr))
	checkShutdown.NewCondBr(isNull, parkM, runG)

	// parkM: park this M until woken
	parkM.NewCall(c.funcs["promise_sched_park_m"], mParam)
	parkM.NewBr(loop)

	// runG: run the goroutine
	// Set @__promise_current_g = gRaw, @__promise_current_p = pRaw
	runG.NewStore(gRaw, c.currentGGlobal)
	runG.NewStore(pRaw, c.currentPGlobal)

	// G.status = running
	gPtr := runG.NewBitCast(gRaw, irtypes.NewPointer(gTy))
	statusField := runG.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	runG.NewStore(constant.NewInt(irtypes.I8, gStatusRunning), statusField)

	// P.current_g = gRaw (so sysmon can find the running G for preemption)
	pTy := processorStructType()
	pPtr := runG.NewBitCast(pRaw, irtypes.NewPointer(pTy))
	curGField := runG.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldCurrentG)))
	runG.NewStore(gRaw, curGField)

	// P.schedTick++ (for find_runnable's global-first check)
	tickField := runG.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldSchedTick)))
	curTick := runG.NewLoad(irtypes.I64, tickField)
	newTick := runG.NewAdd(curTick, constant.NewInt(irtypes.I64, 1))
	runG.NewStore(newTick, tickField)

	// Increment context_switches counter (atomic — called from multiple Ms)
	ctxSwitchField := runG.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldContextSwitches)))
	c.emitAtomicAdd(runG, ctxSwitchField, constant.NewInt(irtypes.I64, 1), irtypes.I64)

	// Load coro handle
	handleField := runG.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldCoroHandle)))
	coroHandle := runG.NewLoad(irtypes.I8Ptr, handleField)

	// Resume coroutine. Panicked goroutines now reach final suspend via TLS
	// panic flag propagation (T0146-T0148), so no setjmp/longjmp recovery needed.
	runG.NewCall(c.coroResume, coroHandle)
	runG.NewBr(afterResume)

	// afterResume: check if coroutine is done
	// Reload gRaw from TLS (may have been changed if G migrated, but for now it's safe)
	gRaw2 := afterResume.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
	gPtr2 := afterResume.NewBitCast(gRaw2, irtypes.NewPointer(gTy))
	handleField2 := afterResume.NewGetElementPtr(gTy, gPtr2,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldCoroHandle)))
	coroHandle2 := afterResume.NewLoad(irtypes.I8Ptr, handleField2)

	isDone := afterResume.NewCall(c.coroDone, coroHandle2)
	afterResume.NewCondBr(isDone, coroDoneBlk, coroSuspendedBlk)

	// coroDone: goroutine finished
	// Clear P.current_g before goroutine_exit (which may enqueue waiters)
	pRaw2 := coroDoneBlk.NewLoad(irtypes.I8Ptr, c.currentPGlobal)
	pPtr2 := coroDoneBlk.NewBitCast(pRaw2, irtypes.NewPointer(pTy))
	pCurGField2 := coroDoneBlk.NewGetElementPtr(pTy, pPtr2,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldCurrentG)))
	coroDoneBlk.NewStore(constant.NewNull(irtypes.I8Ptr), pCurGField2)

	coroDoneBlk.NewCall(c.funcs["promise_goroutine_exit"], gRaw2)
	// Clear current G and P TLS
	coroDoneBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.currentGGlobal)
	coroDoneBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.currentPGlobal)
	coroDoneBlk.NewBr(loop)

	// coroSuspended: G suspended itself. Two cases:
	// 1. park_mutex != null → channel/task wait: release mutex (G is on a waiter list)
	// 2. park_mutex == null → cooperative yield: re-enqueue G (it's now fully suspended)
	pmField := coroSuspendedBlk.NewGetElementPtr(gTy, gPtr2,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
	parkMtx := coroSuspendedBlk.NewLoad(irtypes.I8Ptr, pmField)
	hasParkMtx := coroSuspendedBlk.NewICmp(enum.IPredNE, parkMtx, constant.NewNull(irtypes.I8Ptr))

	releaseMtxBlk := fn.NewBlock("release_park_mutex")
	yieldReenqueueBlk := fn.NewBlock("yield_reenqueue")
	afterReleaseBlk := fn.NewBlock("after_release")
	coroSuspendedBlk.NewCondBr(hasParkMtx, releaseMtxBlk, yieldReenqueueBlk)

	// release_park_mutex: clear the field THEN unlock the mutex.
	// B0249: Must clear park_mutex before unlock to prevent a race where another
	// thread wakes G, G re-parks (storing a new mutex), and our stale NULL write
	// overwrites it — causing the next scheduler to treat park as yield.
	releaseMtxBlk.NewStore(constant.NewNull(irtypes.I8Ptr), pmField)
	releaseMtxBlk.NewCall(c.palMutexUnlock, parkMtx)
	releaseMtxBlk.NewBr(afterReleaseBlk)

	// yield_reenqueue: cooperative yield — G has no park_mutex, so it just
	// wanted to give up the CPU. Re-enqueue it now that coro.suspend has
	// completed and G is safely in a suspended state.
	// Clear P.current_g BEFORE enqueue so sysmon doesn't set preempt on
	// a goroutine that's about to be scheduled by another M.
	pRawY := yieldReenqueueBlk.NewLoad(irtypes.I8Ptr, c.currentPGlobal)
	pPtrY := yieldReenqueueBlk.NewBitCast(pRawY, irtypes.NewPointer(pTy))
	pCurGFieldY := yieldReenqueueBlk.NewGetElementPtr(pTy, pPtrY,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldCurrentG)))
	yieldReenqueueBlk.NewStore(constant.NewNull(irtypes.I8Ptr), pCurGFieldY)
	yieldReenqueueBlk.NewCall(c.funcs["promise_sched_enqueue"], gRaw2)
	yieldReenqueueBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.currentGGlobal)
	yieldReenqueueBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.currentPGlobal)
	yieldReenqueueBlk.NewBr(loop)

	// after_release: channel/task wait — clear P.current_g, current G and P TLS, loop back
	pRaw3 := afterReleaseBlk.NewLoad(irtypes.I8Ptr, c.currentPGlobal)
	pPtr3 := afterReleaseBlk.NewBitCast(pRaw3, irtypes.NewPointer(pTy))
	pCurGField3 := afterReleaseBlk.NewGetElementPtr(pTy, pPtr3,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldCurrentG)))
	afterReleaseBlk.NewStore(constant.NewNull(irtypes.I8Ptr), pCurGField3)

	afterReleaseBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.currentGGlobal)
	afterReleaseBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.currentPGlobal)
	afterReleaseBlk.NewBr(loop)

	// exit: return null
	exitBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	c.funcs["promise_sched_loop"] = fn
}

// --- Sysmon (system monitor) ---

// defineSysmonFunc emits @promise_sysmon(i8* %arg) → i8*
// Background thread that periodically sets G.preempt=1 on all currently running Gs.
// This enables cooperative preemption: goroutines check the flag at yield points
// (loop back-edges) and voluntarily suspend if set.
func (c *Compiler) defineSysmonFunc() {
	argParam := ir.NewParam("arg", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_sysmon", irtypes.I8Ptr, argParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	gTy := goroutineStructType()
	pTy := processorStructType()
	schedTy := schedStructType()

	entry := fn.NewBlock(".entry")
	loop := fn.NewBlock("loop")
	scanHeader := fn.NewBlock("scan_header")
	scanBody := fn.NewBlock("scan_body")
	setPreempt := fn.NewBlock("set_preempt")
	scanNext := fn.NewBlock("scan_next")
	scanDone := fn.NewBlock("scan_done")
	exitBlk := fn.NewBlock("exit")

	iAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewBr(loop)

	// loop: sleep 10ms, then check shutdown flag
	loop.NewCall(c.palUsleep, constant.NewInt(irtypes.I32, 10000)) // 10ms
	shutdownField := loop.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldShutdown)))
	shutdownVal := loop.NewLoad(irtypes.I8, shutdownField)
	isShutdown := loop.NewICmp(enum.IPredNE, shutdownVal, constant.NewInt(irtypes.I8, 0))
	loop.NewCondBr(isShutdown, exitBlk, scanHeader)

	// scanHeader: iterate all Ps
	scanHeader.NewStore(constant.NewInt(irtypes.I32, 0), iAlloca)
	scanHeader.NewBr(scanBody)

	// scanBody: for each P, check current_g
	iVal := scanBody.NewLoad(irtypes.I32, iAlloca)
	numPField := scanBody.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldNumP)))
	numP := scanBody.NewLoad(irtypes.I32, numPField)
	iDone := scanBody.NewICmp(enum.IPredSGE, iVal, numP)
	scanBody.NewCondBr(iDone, scanDone, setPreempt)

	// setPreempt: load P[i].current_g, if non-null → set G.preempt = 1
	i64Val := setPreempt.NewZExt(iVal, irtypes.I64)
	psField := setPreempt.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldPs)))
	psRaw := setPreempt.NewLoad(irtypes.I8Ptr, psField)
	psTyped := setPreempt.NewBitCast(psRaw, irtypes.NewPointer(pTy))
	pPtr := setPreempt.NewGetElementPtr(pTy, psTyped, i64Val)

	curGField := setPreempt.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldCurrentG)))
	curG := setPreempt.NewLoad(irtypes.I8Ptr, curGField)
	hasG := setPreempt.NewICmp(enum.IPredNE, curG, constant.NewNull(irtypes.I8Ptr))

	doSet := fn.NewBlock("do_set_preempt")
	setPreempt.NewCondBr(hasG, doSet, scanNext)

	// do_set_preempt: G.preempt = 1
	gPtr := doSet.NewBitCast(curG, irtypes.NewPointer(gTy))
	preemptField := doSet.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPreempt)))
	doSet.NewStore(constant.NewInt(irtypes.I8, 1), preemptField)
	doSet.NewBr(scanNext)

	// scanNext: i++
	nextI := scanNext.NewAdd(iVal, constant.NewInt(irtypes.I32, 1))
	scanNext.NewStore(nextI, iAlloca)
	scanNext.NewBr(scanBody)

	// scanDone: lost-wakeup safety net (T0352).
	// Read global queue size — if non-zero, wake an idle M. This closes a
	// narrow lost-wakeup race where a non-M thread (e.g. test trampoline)
	// enqueues to the global queue and calls wake_m before any M has pushed
	// itself onto the idle stack. wake_m then no-ops on the empty stack and
	// the work sits unattended until the next enqueue triggers another
	// wake_m. With this safety net, the worst-case stuck time is bounded
	// by sysmon's 10ms tick. wake_m is a no-op when no Ms are idle.
	gsField := scanDone.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalSize)))
	gsVal := scanDone.NewLoad(irtypes.I64, gsField)
	hasGlobalWork := scanDone.NewICmp(enum.IPredNE, gsVal, constant.NewInt(irtypes.I64, 0))
	wakeIdle := fn.NewBlock("sysmon_wake_idle")
	scanDone.NewCondBr(hasGlobalWork, wakeIdle, loop)

	wakeIdle.NewCall(c.funcs["promise_sched_wake_m"])
	wakeIdle.NewBr(loop)

	// exit: return null
	exitBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	c.funcs["promise_sysmon"] = fn
}

// --- P local queue operations ---

// defineLocalEnqueueFunc emits @promise_local_enqueue(i8* %p, i8* %g) → i1
// Attempts to push G into P's 256-slot ring buffer. Returns true on success,
// false if the queue is full (caller should overflow to global queue).
// Caller must be on the M that owns this P (no lock needed).
func (c *Compiler) defineLocalEnqueueFunc() {
	pParam := ir.NewParam("p_raw", irtypes.I8Ptr)
	gParam := ir.NewParam("g_raw", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_local_enqueue", irtypes.I1, pParam, gParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	pTy := processorStructType()

	entry := fn.NewBlock(".entry")
	enqueueBlk := fn.NewBlock("enqueue")
	fullBlk := fn.NewBlock("full")
	retTrue := fn.NewBlock("ret_true")
	retFalse := fn.NewBlock("ret_false")

	pPtr := entry.NewBitCast(pParam, irtypes.NewPointer(pTy))

	// Lock P's mutex to synchronize with steal_work
	lockField := entry.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldLock)))
	pLock := entry.NewLoad(irtypes.I8Ptr, lockField)
	entry.NewCall(c.palMutexLock, pLock)

	// Load head and tail
	headField := entry.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRqHead)))
	head := entry.NewLoad(irtypes.I64, headField)

	tailField := entry.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRqTail)))
	tail := entry.NewLoad(irtypes.I64, tailField)

	// Check if full: (tail - head) >= 256
	diff := entry.NewSub(tail, head)
	isFull := entry.NewICmp(enum.IPredSGE, diff, constant.NewInt(irtypes.I64, pRunQueueSize))
	entry.NewCondBr(isFull, fullBlk, enqueueBlk)

	// enqueue: store G at run_queue[tail % 256], increment tail
	idx := enqueueBlk.NewURem(tail, constant.NewInt(irtypes.I64, pRunQueueSize))
	rqField := enqueueBlk.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRunQueue)),
		idx)
	enqueueBlk.NewStore(gParam, rqField)
	newTail := enqueueBlk.NewAdd(tail, constant.NewInt(irtypes.I64, 1))
	enqueueBlk.NewStore(newTail, tailField)
	enqueueBlk.NewBr(retTrue)

	// retTrue: unlock and return true
	retTrue.NewCall(c.palMutexUnlock, pLock)
	retTrue.NewRet(constant.True)

	// full: unlock and return false
	fullBlk.NewBr(retFalse)
	retFalse.NewCall(c.palMutexUnlock, pLock)
	retFalse.NewRet(constant.False)

	c.funcs["promise_local_enqueue"] = fn
}

// defineLocalDequeueFunc emits @promise_local_dequeue(i8* %p) → i8*
// Pops a G from P's local queue (FIFO: oldest first). Returns null if empty.
// Caller must be on the M that owns this P (no lock needed).
func (c *Compiler) defineLocalDequeueFunc() {
	pParam := ir.NewParam("p_raw", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_local_dequeue", irtypes.I8Ptr, pParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	pTy := processorStructType()

	entry := fn.NewBlock(".entry")
	dequeueBlk := fn.NewBlock("dequeue")
	emptyBlk := fn.NewBlock("empty")
	retG := fn.NewBlock("ret_g")
	retNull := fn.NewBlock("ret_null")

	pPtr := entry.NewBitCast(pParam, irtypes.NewPointer(pTy))

	// Lock P's mutex to synchronize with steal_work
	lockField := entry.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldLock)))
	pLock := entry.NewLoad(irtypes.I8Ptr, lockField)
	entry.NewCall(c.palMutexLock, pLock)

	// Load head and tail
	headField := entry.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRqHead)))
	head := entry.NewLoad(irtypes.I64, headField)

	tailField := entry.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRqTail)))
	tail := entry.NewLoad(irtypes.I64, tailField)

	// Check if empty: head == tail
	isEmpty := entry.NewICmp(enum.IPredEQ, head, tail)
	entry.NewCondBr(isEmpty, emptyBlk, dequeueBlk)

	// dequeue: load G from run_queue[head % 256], increment head
	idx := dequeueBlk.NewURem(head, constant.NewInt(irtypes.I64, pRunQueueSize))
	rqField := dequeueBlk.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRunQueue)),
		idx)
	g := dequeueBlk.NewLoad(irtypes.I8Ptr, rqField)
	newHead := dequeueBlk.NewAdd(head, constant.NewInt(irtypes.I64, 1))
	dequeueBlk.NewStore(newHead, headField)
	dequeueBlk.NewBr(retG)

	// retG: unlock and return G
	retG.NewCall(c.palMutexUnlock, pLock)
	retG.NewRet(g)

	// empty: unlock and return null
	emptyBlk.NewBr(retNull)
	retNull.NewCall(c.palMutexUnlock, pLock)
	retNull.NewRet(constant.NewNull(irtypes.I8Ptr))

	c.funcs["promise_local_dequeue"] = fn
}

// defineStealWorkFunc emits @promise_steal_work(i8* %thief_p, i8* %victim_p) → i8*
// Steals up to half of victim P's local queue into thief P's queue.
// Returns one stolen G (the first one), or null if nothing was stolen.
// Locks BOTH P's during the steal (address-ordered to prevent ABBA deadlock).
func (c *Compiler) defineStealWorkFunc() {
	thiefParam := ir.NewParam("thief_p", irtypes.I8Ptr)
	victimParam := ir.NewParam("victim_p", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_steal_work", irtypes.I8Ptr, thiefParam, victimParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	pTy := processorStructType()

	entry := fn.NewBlock(".entry")
	checkEmpty := fn.NewBlock("check_empty")
	stealLoop := fn.NewBlock("steal_loop")
	stealBody := fn.NewBlock("steal_body")
	stealDone := fn.NewBlock("steal_done")
	emptyBlk := fn.NewBlock("empty")

	// Cast both Ps — allocas and casts BEFORE any terminator
	thiefPtr := entry.NewBitCast(thiefParam, irtypes.NewPointer(pTy))
	victimPtr := entry.NewBitCast(victimParam, irtypes.NewPointer(pTy))

	// Allocas must be in entry block before the terminator
	iAlloca := entry.NewAlloca(irtypes.I64)
	entry.NewStore(constant.NewInt(irtypes.I64, 0), iAlloca)
	firstGAlloca := entry.NewAlloca(irtypes.I8Ptr)
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), firstGAlloca)

	// Get both locks
	thiefLockField := entry.NewGetElementPtr(pTy, thiefPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldLock)))
	thiefLock := entry.NewLoad(irtypes.I8Ptr, thiefLockField)
	victimLockField := entry.NewGetElementPtr(pTy, victimPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldLock)))
	victimLock := entry.NewLoad(irtypes.I8Ptr, victimLockField)

	// Lock both Ps in address order to prevent ABBA deadlock when two Ms
	// steal from each other concurrently (M1: steal P2→P1, M2: steal P1→P2).
	thiefInt := entry.NewPtrToInt(thiefParam, c.ptrIntType())
	victimInt := entry.NewPtrToInt(victimParam, c.ptrIntType())
	thiefFirst := entry.NewICmp(enum.IPredULT, thiefInt, victimInt)

	lockThiefFirst := fn.NewBlock("lock_thief_first")
	lockVictimFirst := fn.NewBlock("lock_victim_first")
	lockDone := fn.NewBlock("lock_done")
	entry.NewCondBr(thiefFirst, lockThiefFirst, lockVictimFirst)

	lockThiefFirst.NewCall(c.palMutexLock, thiefLock)
	lockThiefFirst.NewCall(c.palMutexLock, victimLock)
	lockThiefFirst.NewBr(lockDone)

	lockVictimFirst.NewCall(c.palMutexLock, victimLock)
	lockVictimFirst.NewCall(c.palMutexLock, thiefLock)
	lockVictimFirst.NewBr(lockDone)

	lockDone.NewBr(checkEmpty)

	// Check if victim has work
	vHeadField := checkEmpty.NewGetElementPtr(pTy, victimPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRqHead)))
	vHead := checkEmpty.NewLoad(irtypes.I64, vHeadField)
	vTailField := checkEmpty.NewGetElementPtr(pTy, victimPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRqTail)))
	vTail := checkEmpty.NewLoad(irtypes.I64, vTailField)
	vDiff := checkEmpty.NewSub(vTail, vHead)
	vEmpty := checkEmpty.NewICmp(enum.IPredSLE, vDiff, constant.NewInt(irtypes.I64, 0))
	checkEmpty.NewCondBr(vEmpty, emptyBlk, stealLoop)

	// empty: unlock both and return null
	emptyBlk.NewCall(c.palMutexUnlock, victimLock)
	emptyBlk.NewCall(c.palMutexUnlock, thiefLock)
	emptyBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	// stealLoop: steal up to half = (vTail - vHead) / 2, minimum 1
	half := stealLoop.NewAShr(vDiff, constant.NewInt(irtypes.I64, 1))
	nToSteal := stealLoop.NewCall(c.funcs["promise_i64_max"], half, constant.NewInt(irtypes.I64, 1))

	// Reset counter for the steal loop
	stealLoop.NewStore(constant.NewInt(irtypes.I64, 0), iAlloca)
	stealLoop.NewBr(stealBody)

	// stealBody: steal one G per iteration
	loopI := stealBody.NewLoad(irtypes.I64, iAlloca)
	iDone := stealBody.NewICmp(enum.IPredSGE, loopI, nToSteal)

	afterStealBlk := fn.NewBlock("after_steal_iter")
	stealBody.NewCondBr(iDone, stealDone, afterStealBlk)

	// Pop from victim's head
	curVHead := afterStealBlk.NewLoad(irtypes.I64, vHeadField)
	vIdx := afterStealBlk.NewURem(curVHead, constant.NewInt(irtypes.I64, pRunQueueSize))
	vSlot := afterStealBlk.NewGetElementPtr(pTy, victimPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRunQueue)),
		vIdx)
	stolenG := afterStealBlk.NewLoad(irtypes.I8Ptr, vSlot)
	newVHead := afterStealBlk.NewAdd(curVHead, constant.NewInt(irtypes.I64, 1))
	afterStealBlk.NewStore(newVHead, vHeadField)

	// First stolen G goes to caller; rest go into thief's local queue
	isFirst := afterStealBlk.NewICmp(enum.IPredEQ, loopI, constant.NewInt(irtypes.I64, 0))

	storeFirstBlk := fn.NewBlock("store_first")
	enqueueThiefBlk := fn.NewBlock("enqueue_thief")
	nextIterBlk := fn.NewBlock("next_iter")

	afterStealBlk.NewCondBr(isFirst, storeFirstBlk, enqueueThiefBlk)

	// store_first: save the first G to return
	storeFirstBlk.NewStore(stolenG, firstGAlloca)
	storeFirstBlk.NewBr(nextIterBlk)

	// enqueue_thief: push into thief's local queue
	tTailField := enqueueThiefBlk.NewGetElementPtr(pTy, thiefPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRqTail)))
	tTail := enqueueThiefBlk.NewLoad(irtypes.I64, tTailField)
	tIdx := enqueueThiefBlk.NewURem(tTail, constant.NewInt(irtypes.I64, pRunQueueSize))
	tSlot := enqueueThiefBlk.NewGetElementPtr(pTy, thiefPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRunQueue)),
		tIdx)
	enqueueThiefBlk.NewStore(stolenG, tSlot)
	newTTail := enqueueThiefBlk.NewAdd(tTail, constant.NewInt(irtypes.I64, 1))
	enqueueThiefBlk.NewStore(newTTail, tTailField)
	enqueueThiefBlk.NewBr(nextIterBlk)

	// next_iter: i++
	nextI := nextIterBlk.NewAdd(loopI, constant.NewInt(irtypes.I64, 1))
	nextIterBlk.NewStore(nextI, iAlloca)
	nextIterBlk.NewBr(stealBody)

	// stealDone: unlock both, increment steals counter, return first stolen G
	stealDone.NewCall(c.palMutexUnlock, victimLock)
	stealDone.NewCall(c.palMutexUnlock, thiefLock)

	// Increment steals counter (atomic — called from multiple Ms)
	schedTySteal := schedStructType()
	stealsField := stealDone.NewGetElementPtr(schedTySteal, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldSteals)))
	c.emitAtomicAdd(stealDone, stealsField, constant.NewInt(irtypes.I64, 1), irtypes.I64)

	firstG := stealDone.NewLoad(irtypes.I8Ptr, firstGAlloca)
	stealDone.NewRet(firstG)

	c.funcs["promise_steal_work"] = fn
}

// defineI64MaxFunc emits a simple @promise_i64_max(i64, i64) → i64 helper.
func (c *Compiler) defineI64MaxFunc() {
	aParam := ir.NewParam("a", irtypes.I64)
	bParam := ir.NewParam("b", irtypes.I64)
	fn := c.module.NewFunc("promise_i64_max", irtypes.I64, aParam, bParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	isGT := entry.NewICmp(enum.IPredSGT, aParam, bParam)
	retA := fn.NewBlock("ret_a")
	retB := fn.NewBlock("ret_b")
	entry.NewCondBr(isGT, retA, retB)
	retA.NewRet(aParam)
	retB.NewRet(bParam)

	c.funcs["promise_i64_max"] = fn
}

// defineSchedEnqueueFunc emits @promise_sched_enqueue(i8* %g_raw) → void
// Adds a runnable G to the scheduler. Tries local P queue first, falls back to global.
func (c *Compiler) defineSchedEnqueueFunc() {
	gParam := ir.NewParam("g_raw", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_sched_enqueue", irtypes.Void, gParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	gTy := goroutineStructType()
	schedTy := schedStructType()

	entry := fn.NewBlock(".entry")
	tryLocal := fn.NewBlock("try_local")
	globalEnqueue := fn.NewBlock("global_enqueue")
	wakeAndRet := fn.NewBlock("wake_and_ret")

	// Set G.status = runnable
	gPtr := entry.NewBitCast(gParam, irtypes.NewPointer(gTy))
	statusField := entry.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	entry.NewStore(constant.NewInt(irtypes.I8, gStatusRunnable), statusField)

	// Try local enqueue: load @__promise_current_p
	curP := entry.NewLoad(irtypes.I8Ptr, c.currentPGlobal)
	hasCurP := entry.NewICmp(enum.IPredNE, curP, constant.NewNull(irtypes.I8Ptr))
	entry.NewCondBr(hasCurP, tryLocal, globalEnqueue)

	// tryLocal: attempt to push into P's local queue
	localOk := tryLocal.NewCall(c.funcs["promise_local_enqueue"], curP, gParam)
	tryLocal.NewCondBr(localOk, wakeAndRet, globalEnqueue)

	// globalEnqueue: push to global queue (fallback or no current P)
	// Lock global queue
	glLockField := globalEnqueue.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalLock)))
	glLock := globalEnqueue.NewLoad(irtypes.I8Ptr, glLockField)
	globalEnqueue.NewCall(c.palMutexLock, glLock)

	// G.sched_next = null (tail of list)
	snField := globalEnqueue.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldSchedNext)))
	globalEnqueue.NewStore(constant.NewNull(irtypes.I8Ptr), snField)

	// if sched.global_tail != null: tail.sched_next = gParam
	// else: sched.global_head = gParam
	tailField := globalEnqueue.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalTail)))
	tail := globalEnqueue.NewLoad(irtypes.I8Ptr, tailField)
	tailIsNull := globalEnqueue.NewICmp(enum.IPredEQ, tail, constant.NewNull(irtypes.I8Ptr))

	setHead := fn.NewBlock("set_head")
	setTailNext := fn.NewBlock("set_tail_next")
	updateTail := fn.NewBlock("update_tail")

	globalEnqueue.NewCondBr(tailIsNull, setHead, setTailNext)

	// setHead: empty queue, this G becomes head
	headField := setHead.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalHead)))
	setHead.NewStore(gParam, headField)
	setHead.NewBr(updateTail)

	// setTailNext: queue not empty, append to tail
	tailGPtr := setTailNext.NewBitCast(tail, irtypes.NewPointer(gTy))
	tailSnField := setTailNext.NewGetElementPtr(gTy, tailGPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldSchedNext)))
	setTailNext.NewStore(gParam, tailSnField)
	setTailNext.NewBr(updateTail)

	// updateTail: sched.global_tail = gParam, size++
	tailField2 := updateTail.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalTail)))
	updateTail.NewStore(gParam, tailField2)

	sizeField := updateTail.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalSize)))
	size := updateTail.NewLoad(irtypes.I64, sizeField)
	newSize := updateTail.NewAdd(size, constant.NewInt(irtypes.I64, 1))
	updateTail.NewStore(newSize, sizeField)

	// Unlock global queue
	updateTail.NewCall(c.palMutexUnlock, glLock)
	updateTail.NewBr(wakeAndRet)

	// wakeAndRet: wake an idle M and return
	wakeAndRet.NewCall(c.funcs["promise_sched_wake_m"])
	wakeAndRet.NewRet(nil)

	c.funcs["promise_sched_enqueue"] = fn
}

// defineSchedFindRunnableFunc emits @promise_sched_find_runnable(i8* %p_raw) → i8*
// Searches for a runnable G. Normal order: local queue → global queue → steal.
// Every 61 calls (schedtick % 61 == 0), checks global queue first to prevent
// starvation of Gs enqueued by non-M threads (e.g., test-thread channel ops).
func (c *Compiler) defineSchedFindRunnableFunc() {
	pParam := ir.NewParam("p_raw", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_sched_find_runnable", irtypes.I8Ptr, pParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	gTy := goroutineStructType()
	pTy := processorStructType()
	schedTy := schedStructType()

	entry := fn.NewBlock(".entry")
	checkLocal := fn.NewBlock("check_local")
	tryGlobal := fn.NewBlock("try_global")
	globalEmpty := fn.NewBlock("global_empty")
	globalDequeue := fn.NewBlock("global_dequeue")
	clearTailBlk := fn.NewBlock("clear_tail")
	globalDone := fn.NewBlock("global_done")
	trySteal := fn.NewBlock("try_steal")
	stealLoop := fn.NewBlock("steal_loop")
	stealBody := fn.NewBlock("steal_body")
	stealSkip := fn.NewBlock("steal_skip")
	stealCheck := fn.NewBlock("steal_check")
	notFoundBlk := fn.NewBlock("not_found")

	// Alloca for steal loop counter (must be in entry block)
	iAlloca := entry.NewAlloca(irtypes.I32)
	// Flag alloca: 1 when entered via global-first path (tick % 61 == 0).
	// Read by globalEmpty to fall back to checkLocal on global-first ticks
	// instead of going straight to steal — prevents deadlock on single-P
	// targets like WASM where steal always returns null (T0326).
	globalFirstFlagAlloca := entry.NewAlloca(irtypes.I32)

	// --- Schedtick: every 61 scheduling iterations, check global queue first ---
	// Prevents global queue starvation when the local queue has a yield-spinning G
	// (e.g., unbuffered channel rendezvous). Same mechanism as the Go runtime's
	// "schedtick % 61 == 0 → check global first" (T0326).
	pPtr0 := entry.NewBitCast(pParam, irtypes.NewPointer(pTy))
	tickField0 := entry.NewGetElementPtr(pTy, pPtr0,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldSchedTick)))
	tick0 := entry.NewLoad(irtypes.I64, tickField0)
	tick1 := entry.NewAdd(tick0, constant.NewInt(irtypes.I64, 1))
	entry.NewStore(tick1, tickField0)
	tickMod := entry.NewURem(tick1, constant.NewInt(irtypes.I64, 61))
	globalFirst := entry.NewICmp(enum.IPredEQ, tickMod, constant.NewInt(irtypes.I64, 0))
	flagVal := entry.NewSelect(globalFirst, constant.NewInt(irtypes.I32, 1), constant.NewInt(irtypes.I32, 0))
	entry.NewStore(flagVal, globalFirstFlagAlloca)
	entry.NewCondBr(globalFirst, tryGlobal, checkLocal)

	// --- Step 1: Try local P queue ---
	// Always check local queue first, even for disabled Ps — a disabled P may
	// still have work from before num_p was reduced.
	localG := checkLocal.NewCall(c.funcs["promise_local_dequeue"], pParam)
	localNull := checkLocal.NewICmp(enum.IPredEQ, localG, constant.NewNull(irtypes.I8Ptr))
	localFound := fn.NewBlock("local_found")
	checkLocal.NewCondBr(localNull, tryGlobal, localFound)

	localFound.NewRet(localG)

	// NOTE: Disabled Ps (P.id >= num_p) are NOT short-circuited. They fall
	// through to the global queue and work-stealing phases, helping process
	// any available work before parking. A previous design used cascade-wake
	// (calling wake_m from a disabled M), but this could cycle between two
	// disabled Ms on the LIFO idle stack without ever reaching an active M,
	// causing goroutines to stall on Linux (B0136).

	// --- Step 2: Try global queue ---
	glLockField := tryGlobal.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalLock)))
	glLock := tryGlobal.NewLoad(irtypes.I8Ptr, glLockField)
	tryGlobal.NewCall(c.palMutexLock, glLock)

	headField := tryGlobal.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalHead)))
	head := tryGlobal.NewLoad(irtypes.I8Ptr, headField)
	headIsNull := tryGlobal.NewICmp(enum.IPredEQ, head, constant.NewNull(irtypes.I8Ptr))
	tryGlobal.NewCondBr(headIsNull, globalEmpty, globalDequeue)

	globalEmpty.NewCall(c.palMutexUnlock, glLock)
	// On global-first ticks (flag==1), fall through to checkLocal before
	// stealing — preserves liveness on single-P targets (e.g., WASM) where
	// steal always returns null (T0326).
	gfFlag := globalEmpty.NewLoad(irtypes.I32, globalFirstFlagAlloca)
	globalEmpty.NewStore(constant.NewInt(irtypes.I32, 0), globalFirstFlagAlloca)
	gfIsSet := globalEmpty.NewICmp(enum.IPredEQ, gfFlag, constant.NewInt(irtypes.I32, 1))
	globalEmpty.NewCondBr(gfIsSet, checkLocal, trySteal)

	// globalDequeue: pop head
	gPtr := globalDequeue.NewBitCast(head, irtypes.NewPointer(gTy))
	nextField := globalDequeue.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldSchedNext)))
	next := globalDequeue.NewLoad(irtypes.I8Ptr, nextField)
	globalDequeue.NewStore(next, headField)

	nextIsNull := globalDequeue.NewICmp(enum.IPredEQ, next, constant.NewNull(irtypes.I8Ptr))
	globalDequeue.NewCondBr(nextIsNull, clearTailBlk, globalDone)

	tailField := clearTailBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalTail)))
	clearTailBlk.NewStore(constant.NewNull(irtypes.I8Ptr), tailField)
	clearTailBlk.NewBr(globalDone)

	// globalDone: size--, unlock, clear sched_next, return
	sizeField := globalDone.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalSize)))
	size := globalDone.NewLoad(irtypes.I64, sizeField)
	newSize := globalDone.NewSub(size, constant.NewInt(irtypes.I64, 1))
	globalDone.NewStore(newSize, sizeField)
	globalDone.NewCall(c.palMutexUnlock, glLock)

	nextField2 := globalDone.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldSchedNext)))
	globalDone.NewStore(constant.NewNull(irtypes.I8Ptr), nextField2)
	globalDone.NewRet(head)

	// --- Step 3: Work stealing from other Ps ---
	pPtr := trySteal.NewBitCast(pParam, irtypes.NewPointer(pTy))
	myIdField := trySteal.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldID)))
	myId := trySteal.NewLoad(irtypes.I32, myIdField)

	numPField := trySteal.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldNumP)))
	numP := trySteal.NewLoad(irtypes.I32, numPField)

	psField := trySteal.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldPs)))
	psRaw := trySteal.NewLoad(irtypes.I8Ptr, psField)
	psTyped := trySteal.NewBitCast(psRaw, irtypes.NewPointer(pTy))

	trySteal.NewStore(constant.NewInt(irtypes.I32, 0), iAlloca)
	trySteal.NewBr(stealLoop)

	// stealLoop: for i = 0; i < numP; i++
	iVal := stealLoop.NewLoad(irtypes.I32, iAlloca)
	loopDone := stealLoop.NewICmp(enum.IPredSGE, iVal, numP)
	stealLoop.NewCondBr(loopDone, notFoundBlk, stealBody)

	// stealBody: skip self
	iVal2 := stealBody.NewLoad(irtypes.I32, iAlloca)
	isSelf := stealBody.NewICmp(enum.IPredEQ, iVal2, myId)
	stealBody.NewCondBr(isSelf, stealSkip, stealCheck)

	// stealSkip: i++, continue
	nextI1 := stealSkip.NewLoad(irtypes.I32, iAlloca)
	nextI := stealSkip.NewAdd(nextI1, constant.NewInt(irtypes.I32, 1))
	stealSkip.NewStore(nextI, iAlloca)
	stealSkip.NewBr(stealLoop)

	// stealCheck: try steal from P[i]
	i64Val := stealCheck.NewZExt(iVal2, irtypes.I64)
	victimPtr := stealCheck.NewGetElementPtr(pTy, psTyped, i64Val)
	victimRaw := stealCheck.NewBitCast(victimPtr, irtypes.I8Ptr)
	stolenG := stealCheck.NewCall(c.funcs["promise_steal_work"], pParam, victimRaw)
	stolenNull := stealCheck.NewICmp(enum.IPredEQ, stolenG, constant.NewNull(irtypes.I8Ptr))

	stealFoundBlk := fn.NewBlock("steal_found")
	stealCheck.NewCondBr(stolenNull, stealSkip, stealFoundBlk)

	stealFoundBlk.NewRet(stolenG)

	// notFound: return null
	notFoundBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	c.funcs["promise_sched_find_runnable"] = fn
}

// defineSchedParkMFunc emits @promise_sched_park_m(i8* %m_raw) → void
// Parks an M (OS thread) until woken by promise_sched_wake_m or shutdown.
//
// Protocol: park_mutex is locked BEFORE adding to idle list, then cond_wait
// is called in a loop while still holding it. This prevents the lost-signal
// race where wake_m/shutdown could signal between idle-list push and cond_wait.
// M.p is saved/restored since it's reused as the idle stack next pointer.
//
// Spurious wakeup safety: POSIX allows cond_wait to return spuriously.
// We use M.spinning as a "woken" flag — wake_m sets it to 1 after popping M
// from the idle stack. park_m loops until spinning==1 or shutdown. This
// prevents M from returning prematurely and re-pushing onto the idle stack
// (which would corrupt the intrusive linked list and potentially lose Ms).
//
// Lost-wakeup race (T0375): there is a window between find_runnable() returning
// null and park_m acquiring idle_lock where a non-M enqueuer can complete
// sched_enqueue + wake_m against an empty idle stack. To close this race we
// re-check sched.global_size while still holding idle_lock AFTER pushing self
// onto the idle stack. Both wake_m and this re-check serialize on idle_lock,
// giving the invariant: either wake_m saw us on the stack and signaled (normal
// park works), or we observe global_size > 0 and abort the park (popping self
// off the stack and looping back to find_runnable). pthread mutex unlock/lock
// provides cross-mutex visibility for global_size (which is incremented under
// global_lock in sched_enqueue) without us needing to take global_lock here.
//
// Sysmon (sched.go ~789) still periodically calls wake_m as defense-in-depth
// (T0352), but is no longer the primary mitigation for this race.
func (c *Compiler) defineSchedParkMFunc() {
	mParam := ir.NewParam("m_raw", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_sched_park_m", irtypes.Void, mParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	mTy := machineStructType()
	schedTy := schedStructType()

	entry := fn.NewBlock(".entry")
	abortPark := fn.NewBlock("abort_park")
	continuePark := fn.NewBlock("continue_park")
	waitLoop := fn.NewBlock("wait_loop")
	doWait := fn.NewBlock("do_wait")
	doneBlk := fn.NewBlock("done")

	mPtr := entry.NewBitCast(mParam, irtypes.NewPointer(mTy))

	// Save M.p before we repurpose it as idle-list next pointer
	mPField := entry.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldP)))
	savedP := entry.NewLoad(irtypes.I8Ptr, mPField)

	// Lock park_mutex FIRST — prevents lost-signal race with wake_m/shutdown
	parkMtxField := entry.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkMutex)))
	parkMtx := entry.NewLoad(irtypes.I8Ptr, parkMtxField)
	entry.NewCall(c.palMutexLock, parkMtx)

	// M.spinning = 0 (clear woken flag — wake_m sets it to 1 when deliberately waking)
	spinField := entry.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldSpinning)))
	entry.NewStore(constant.NewInt(irtypes.I8, 0), spinField)

	// Push M onto idle stack: M.p = old idle head, idle_head = M
	imLockField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldIdleMLock)))
	imLock := entry.NewLoad(irtypes.I8Ptr, imLockField)
	entry.NewCall(c.palMutexLock, imLock)

	idleHeadField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldIdleMHead)))
	oldHead := entry.NewLoad(irtypes.I8Ptr, idleHeadField)
	entry.NewStore(oldHead, mPField)      // M.p = old idle head (next pointer)
	entry.NewStore(mParam, idleHeadField) // idle_head = M

	// T0375: re-check global queue size while still holding idle_lock. If a
	// non-M enqueuer raced ahead of us (queued work + ran wake_m on the empty
	// stack), bail out before we commit to cond_wait. Reading global_size
	// without global_lock is safe: we only need to observe enqueues whose
	// wake_m has already run (and missed us); their global_size store is
	// visible because pthread mutex unlock/lock pairs publish all prior
	// writes from the unlocker's perspective.
	globalSizeField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalSize)))
	globalSize := entry.NewLoad(irtypes.I64, globalSizeField)
	hasWork := entry.NewICmp(enum.IPredNE, globalSize, constant.NewInt(irtypes.I64, 0))

	// Pre-load park_cond (defined in entry, dominates all blocks)
	parkCondField := entry.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkCond)))
	parkCond := entry.NewLoad(irtypes.I8Ptr, parkCondField)

	entry.NewCondBr(hasWork, abortPark, continuePark)

	// abort_park: work appeared in the global queue between find_runnable()
	// and our idle-list push. Pop self (head == self, so head = oldHead),
	// restore M.p, release both mutexes, and return so sched_loop loops back
	// to find_runnable.
	abortPark.NewStore(oldHead, idleHeadField) // pop self
	abortPark.NewStore(savedP, mPField)        // restore M.p
	abortPark.NewCall(c.palMutexUnlock, imLock)
	abortPark.NewCall(c.palMutexUnlock, parkMtx)
	abortPark.NewRet(nil)

	// continue_park: normal path — release idle_lock, wait on park_cond.
	continuePark.NewCall(c.palMutexUnlock, imLock)
	continuePark.NewBr(waitLoop)

	// wait_loop: check if deliberately woken (spinning==1) or shutdown.
	// POSIX cond_wait may return spuriously; we must loop to prevent
	// returning early and corrupting the idle stack on re-park.
	spinVal := waitLoop.NewLoad(irtypes.I8, spinField)
	isWoken := waitLoop.NewICmp(enum.IPredNE, spinVal, constant.NewInt(irtypes.I8, 0))

	shutdownField := waitLoop.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldShutdown)))
	shutdownVal := waitLoop.NewLoad(irtypes.I8, shutdownField)
	isShutdown := waitLoop.NewICmp(enum.IPredNE, shutdownVal, constant.NewInt(irtypes.I8, 0))

	shouldExit := waitLoop.NewOr(isWoken, isShutdown)
	waitLoop.NewCondBr(shouldExit, doneBlk, doWait)

	// doWait: cond_wait (park_mutex held — released atomically by cond_wait)
	doWait.NewCall(c.palCondWait, parkCond, parkMtx)
	doWait.NewBr(waitLoop)

	// done: unlock park_mutex, conditionally restore M.p
	doneBlk.NewCall(c.palMutexUnlock, parkMtx)

	// Only restore M.p if we were deliberately woken by wake_m (spinning=1).
	// When woken by shutdown (spinning=0), this M is still on the idle stack
	// and M.p holds the idle-list next pointer. Restoring M.p would corrupt
	// the idle stack, causing concurrent wake_m callers to read garbage →
	// SIGSEGV. The sched_loop checks shutdown before reading M.p, so leaving
	// it as the idle-list link is safe.
	spinAtExit := doneBlk.NewLoad(irtypes.I8, spinField)
	wasWoken := doneBlk.NewICmp(enum.IPredNE, spinAtExit, constant.NewInt(irtypes.I8, 0))
	restoreBlk := fn.NewBlock("restore_p")
	skipRestoreBlk := fn.NewBlock("skip_restore_p")
	doneBlk.NewCondBr(wasWoken, restoreBlk, skipRestoreBlk)

	restoreBlk.NewStore(savedP, mPField)
	restoreBlk.NewRet(nil)

	skipRestoreBlk.NewRet(nil)

	c.funcs["promise_sched_park_m"] = fn
}

// defineSchedWakeMFunc emits @promise_sched_wake_m() → void
// Pops an M from the idle stack and signals its park_cond.
func (c *Compiler) defineSchedWakeMFunc() {
	fn := c.module.NewFunc("promise_sched_wake_m", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	mTy := machineStructType()
	schedTy := schedStructType()

	entry := fn.NewBlock(".entry")

	// Lock idle M list
	imLockField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldIdleMLock)))
	imLock := entry.NewLoad(irtypes.I8Ptr, imLockField)
	entry.NewCall(c.palMutexLock, imLock)

	// Check if idle stack is empty
	idleHeadField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldIdleMHead)))
	head := entry.NewLoad(irtypes.I8Ptr, idleHeadField)
	isEmpty := entry.NewICmp(enum.IPredEQ, head, constant.NewNull(irtypes.I8Ptr))

	emptyBlk := fn.NewBlock("empty")
	wakeBlk := fn.NewBlock("wake")

	entry.NewCondBr(isEmpty, emptyBlk, wakeBlk)

	// empty: nothing to wake
	emptyBlk.NewCall(c.palMutexUnlock, imLock)
	emptyBlk.NewRet(nil)

	// wake: pop head, set new head = head.p (next in idle stack)
	mPtr := wakeBlk.NewBitCast(head, irtypes.NewPointer(mTy))
	mPField := wakeBlk.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldP)))
	nextIdle := wakeBlk.NewLoad(irtypes.I8Ptr, mPField)

	wakeBlk.NewStore(nextIdle, idleHeadField)
	wakeBlk.NewCall(c.palMutexUnlock, imLock)

	// Signal M's park_cond while holding park_mutex (prevents lost-signal race).
	// Set M.spinning = 1 BEFORE signaling — park_m loops on this flag to
	// distinguish real wakeups from spurious cond_wait returns.
	parkMtxField := wakeBlk.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkMutex)))
	parkMtx := wakeBlk.NewLoad(irtypes.I8Ptr, parkMtxField)
	wakeBlk.NewCall(c.palMutexLock, parkMtx)

	// Set woken flag (synchronized by park_mutex)
	spinField := wakeBlk.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldSpinning)))
	wakeBlk.NewStore(constant.NewInt(irtypes.I8, 1), spinField)

	parkCondField := wakeBlk.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkCond)))
	parkCond := wakeBlk.NewLoad(irtypes.I8Ptr, parkCondField)
	wakeBlk.NewCall(c.palCondSignal, parkCond)

	wakeBlk.NewCall(c.palMutexUnlock, parkMtx)

	wakeBlk.NewRet(nil)

	c.funcs["promise_sched_wake_m"] = fn
}

// --- Syscall handoff (Phase 6a) ---

// defineEnterSyscallFunc emits @promise_sched_enter_syscall() → void
// Called before blocking PAL syscalls (file IO). Detaches P from the current
// goroutine so other Ms can steal work from P's run queue. The M keeps its
// P pointer (M.p unchanged) but P.current_g is cleared so sysmon won't try
// to preempt. TLS current_p is cleared to signal "in syscall" state.
//
// On WASM this is a no-op (single-threaded, no M contention).
func (c *Compiler) defineEnterSyscallFunc() {
	fn := c.module.NewFunc("promise_sched_enter_syscall", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")

	if c.isWasm {
		// WASM: single-threaded, no handoff needed
		entry.NewRet(nil)
		c.funcs["promise_sched_enter_syscall"] = fn
		return
	}

	pTy := processorStructType()

	// Load TLS current_p — if null, we're not on a P (shouldn't happen in normal flow)
	pRaw := entry.NewLoad(irtypes.I8Ptr, c.currentPGlobal)
	isNull := entry.NewICmp(enum.IPredEQ, pRaw, constant.NewNull(irtypes.I8Ptr))

	doHandoff := fn.NewBlock("do_handoff")
	retBlk := fn.NewBlock("ret")
	entry.NewCondBr(isNull, retBlk, doHandoff)

	// Clear P.current_g — sysmon won't try to preempt this G
	pPtr := doHandoff.NewBitCast(pRaw, irtypes.NewPointer(pTy))
	curGField := doHandoff.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldCurrentG)))
	doHandoff.NewStore(constant.NewNull(irtypes.I8Ptr), curGField)

	// Clear TLS current_p — signals "in syscall" state
	doHandoff.NewStore(constant.NewNull(irtypes.I8Ptr), c.currentPGlobal)

	// Wake an idle M to steal work from this P's run queue
	doHandoff.NewCall(c.funcs["promise_sched_wake_m"])

	doHandoff.NewBr(retBlk)
	retBlk.NewRet(nil)

	c.funcs["promise_sched_enter_syscall"] = fn
}

// defineExitSyscallFunc emits @promise_sched_exit_syscall() → void
// Called after blocking PAL syscalls return. Reattaches the P to the current
// goroutine. The M still has its P (M.p was never cleared), so we just
// restore P.current_g and TLS current_p.
//
// On WASM this is a no-op.
func (c *Compiler) defineExitSyscallFunc() {
	fn := c.module.NewFunc("promise_sched_exit_syscall", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")

	if c.isWasm {
		entry.NewRet(nil)
		c.funcs["promise_sched_exit_syscall"] = fn
		return
	}

	mTy := machineStructType()
	pTy := processorStructType()

	// Load TLS current_m to find our P (M.p was never cleared)
	mRaw := entry.NewLoad(irtypes.I8Ptr, c.currentMGlobal)
	mIsNull := entry.NewICmp(enum.IPredEQ, mRaw, constant.NewNull(irtypes.I8Ptr))

	restoreBlk := fn.NewBlock("restore")
	retBlk := fn.NewBlock("ret")
	entry.NewCondBr(mIsNull, retBlk, restoreBlk)

	// Get P from M.p
	mPtr := restoreBlk.NewBitCast(mRaw, irtypes.NewPointer(mTy))
	mPField := restoreBlk.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldP)))
	pRaw := restoreBlk.NewLoad(irtypes.I8Ptr, mPField)

	pIsNull := restoreBlk.NewICmp(enum.IPredEQ, pRaw, constant.NewNull(irtypes.I8Ptr))
	doRestore := fn.NewBlock("do_restore")
	restoreBlk.NewCondBr(pIsNull, retBlk, doRestore)

	// Load TLS current_g
	gRaw := doRestore.NewLoad(irtypes.I8Ptr, c.currentGGlobal)

	// Restore P.current_g = G
	pPtr := doRestore.NewBitCast(pRaw, irtypes.NewPointer(pTy))
	curGField := doRestore.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldCurrentG)))
	doRestore.NewStore(gRaw, curGField)

	// Restore TLS current_p = P
	doRestore.NewStore(pRaw, c.currentPGlobal)

	doRestore.NewBr(retBlk)
	retBlk.NewRet(nil)

	c.funcs["promise_sched_exit_syscall"] = fn
}

// defineGoroutineExitFunc emits @promise_goroutine_exit(i8* %g_raw) → void
// Marks G as dead, signals done_waiters, destroys coroutine, frees G.
func (c *Compiler) defineGoroutineExitFunc() {
	gParam := ir.NewParam("g_raw", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_goroutine_exit", irtypes.Void, gParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	gTy := goroutineStructType()

	entry := fn.NewBlock(".entry")

	gPtr := entry.NewBitCast(gParam, irtypes.NewPointer(gTy))

	// G.status = dead
	statusField := entry.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	entry.NewStore(constant.NewInt(irtypes.I8, gStatusDead), statusField)

	// Compute gs_completed field pointer (used at end of function, after all frees).
	// B0234: gs_completed is incremented at the end of goroutine_exit (not the
	// beginning) so that the test harness can spin-wait on gs_created==gs_completed
	// to know all goroutine cleanup (coro.destroy + pal_free(G)) is truly done.
	schedTyLocal := schedStructType()
	gsCompletedField := entry.NewGetElementPtr(schedTyLocal, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGsCompleted)))

	// Pre-load fields we need after unlocking done_lock.
	// For task[T] goroutines, the receiver may free the G struct as soon as
	// done_lock is released, so we must read all G fields beforehand.
	earlyHandleField := entry.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldCoroHandle)))
	earlyCoroHandle := entry.NewLoad(irtypes.I8Ptr, earlyHandleField)
	earlyIdField := entry.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldID)))
	earlyGID := entry.NewLoad(irtypes.I64, earlyIdField)
	earlyRpField := entry.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
	earlyRpVal := entry.NewLoad(irtypes.I8Ptr, earlyRpField)

	// Lock sched.done_lock to protect done + done_waiters atomically.
	// This pairs with genReceiveTask's done_lock acquisition so that
	// no waiter can enqueue-then-suspend between our done=1 and waiter walk.
	schedTyExit := schedStructType()
	doneLockFieldExit := entry.NewGetElementPtr(schedTyExit, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldDoneLock)))
	doneLockExit := entry.NewLoad(irtypes.I8Ptr, doneLockFieldExit)
	entry.NewCall(c.palMutexLock, doneLockExit)

	// G.done = 1
	doneField := entry.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldDone)))
	entry.NewStore(constant.NewInt(irtypes.I8, 1), doneField)

	// Wake all done_waiters: loop through linked list
	dwField := entry.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldDoneWaiters)))
	firstWaiter := entry.NewLoad(irtypes.I8Ptr, dwField)

	// Clear done_waiters list
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), dwField)

	waiterLoop := fn.NewBlock("waiter_loop")
	waiterBody := fn.NewBlock("waiter_body")
	waitersDone := fn.NewBlock("waiters_done")

	entry.NewBr(waiterLoop)

	// waiterLoop: use alloca to track current waiter
	waiterAlloca := entry.NewAlloca(irtypes.I8Ptr)
	entry.NewStore(firstWaiter, waiterAlloca)

	curWaiter := waiterLoop.NewLoad(irtypes.I8Ptr, waiterAlloca)
	waiterIsNull := waiterLoop.NewICmp(enum.IPredEQ, curWaiter, constant.NewNull(irtypes.I8Ptr))
	waiterLoop.NewCondBr(waiterIsNull, waitersDone, waiterBody)

	// waiterBody: set waiter to runnable, enqueue, advance to next
	waiterGPtr := waiterBody.NewBitCast(curWaiter, irtypes.NewPointer(gTy))
	// Save next before enqueue (enqueue may modify sched_next)
	waitNextField := waiterBody.NewGetElementPtr(gTy, waiterGPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldWaitNext)))
	nextWaiter := waiterBody.NewLoad(irtypes.I8Ptr, waitNextField)

	// Enqueue waiter (sets status to runnable)
	waiterBody.NewCall(c.funcs["promise_sched_enqueue"], curWaiter)

	waiterBody.NewStore(nextWaiter, waiterAlloca)
	waiterBody.NewBr(waiterLoop)

	// Unlock sched.done_lock — done + waiters fully processed.
	// After this point, a woken task[T] receiver may free the G struct,
	// so we must NOT access G fields below — use early-loaded values instead.
	waitersDone.NewCall(c.palMutexUnlock, doneLockExit)

	// Pre-load panic fields for cleanup in the non-task free path.
	panickedField := entry.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
	earlyPanicked := entry.NewLoad(irtypes.I8, panickedField)
	earlyPanicMsgField := entry.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
	earlyPanicMsg := entry.NewLoad(irtypes.I8Ptr, earlyPanicMsgField)

	// Destroy coroutine. Panicked goroutines now reach final suspend via TLS
	// panic flag propagation (T0146-T0148), so coro.destroy is always safe.
	waitersDone.NewCall(c.coroDestroy, earlyCoroHandle)

	// Signal main_done if this is G0 (id == 0)
	isG0 := waitersDone.NewICmp(enum.IPredEQ, earlyGID, constant.NewInt(irtypes.I64, 0))

	signalMain := fn.NewBlock("signal_main")
	freeG := fn.NewBlock("free_g")

	waitersDone.NewCondBr(isG0, signalMain, freeG)

	// signalMain: signal main done
	schedTy := schedStructType()
	mdField := signalMain.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDone)))
	signalMain.NewStore(constant.NewInt(irtypes.I8, 1), mdField)

	mdMtxField := signalMain.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDoneMutex)))
	mdMtx := signalMain.NewLoad(irtypes.I8Ptr, mdMtxField)
	mdCondField := signalMain.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDoneCond)))
	mdCond := signalMain.NewLoad(irtypes.I8Ptr, mdCondField)

	signalMain.NewCall(c.palMutexLock, mdMtx)
	signalMain.NewCall(c.palCondBroadcast, mdCond)
	signalMain.NewCall(c.palMutexUnlock, mdMtx)
	signalMain.NewBr(freeG)

	// freeG: free the G struct (don't free if task[T] — caller needs result_ptr)
	// Use early-loaded result_ptr (G struct may be freed by task receiver after unlock)
	isTask := freeG.NewICmp(enum.IPredNE, earlyRpVal, constant.NewNull(irtypes.I8Ptr))

	skipFree := fn.NewBlock("skip_free")
	doFree := fn.NewBlock("do_free")

	freeG.NewCondBr(isTask, skipFree, doFree)

	// skipFree: task[T] — caller will free G when they receive the result.
	// Increment gs_completed after all cleanup (B0234).
	// B0320: Use Release ordering so that all prior stores (alloc_count
	// decrements from pal_free) are visible to any Acquire reader of
	// gs_completed — prevents false-positive LEAK in the drain fast path
	// on ARM64 (weak memory model allows store reordering across variables
	// with Monotonic ordering).
	c.emitAtomicAddRelease(skipFree, gsCompletedField, constant.NewInt(irtypes.I64, 1), irtypes.I64)
	skipFree.NewRet(nil)

	// doFree: free panic_msg if heap-allocated (panicked==2) then free G.
	// panicked=1 (promise_panic) means C string that may be .rodata — don't free.
	// panicked=2 (promise_panic_msg) means heap-allocated copy — must free.
	isHeapMsg := doFree.NewICmp(enum.IPredEQ, earlyPanicked, constant.NewInt(irtypes.I8, int64(gPanickedHeapMsg)))
	freePanicMsgBlk := fn.NewBlock("free_panic_msg")
	doFreeG := fn.NewBlock("do_free_g")
	doFree.NewCondBr(isHeapMsg, freePanicMsgBlk, doFreeG)

	freePanicMsgBlk.NewCall(c.palFree, earlyPanicMsg)
	freePanicMsgBlk.NewBr(doFreeG)

	doFreeG.NewCall(c.palFree, gParam)
	// Increment gs_completed after all cleanup including pal_free(G) (B0234).
	// B0320: Release ordering — see skipFree comment above.
	c.emitAtomicAddRelease(doFreeG, gsCompletedField, constant.NewInt(irtypes.I64, 1), irtypes.I64)
	doFreeG.NewRet(nil)

	c.funcs["promise_goroutine_exit"] = fn
}

// defineSchedShutdownFunc emits @promise_sched_shutdown() → void
// Sets shutdown flag, signals ALL Ms (via Ps) while holding park_mutex, joins threads.
func (c *Compiler) defineSchedShutdownFunc() {
	fn := c.module.NewFunc("promise_sched_shutdown", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	schedTy := schedStructType()
	pTy := processorStructType()
	mTy := machineStructType()

	entry := fn.NewBlock(".entry")

	// Set shutdown flag
	shutdownField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldShutdown)))
	entry.NewStore(constant.NewInt(irtypes.I8, 1), shutdownField)

	// Signal ALL Ms via their Ps (not just idle list).
	// Use max_p (not num_p) so that Ms on disabled Ps (after set_max_procs
	// reduced num_p) are still signaled and joined. Otherwise they get
	// killed mid-execution during process exit → SIGSEGV.
	maxPField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMaxP)))
	maxP := entry.NewLoad(irtypes.I32, maxPField)

	psField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldPs)))
	psRaw := entry.NewLoad(irtypes.I8Ptr, psField)
	psTyped := entry.NewBitCast(psRaw, irtypes.NewPointer(pTy))

	iAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), iAlloca)

	// Signal loop: for each P[i], signal P[i].m's park_cond
	signalLoop := fn.NewBlock("signal_loop")
	signalBody := fn.NewBlock("signal_body")
	joinPhase := fn.NewBlock("join_phase")

	entry.NewBr(signalLoop)

	iVal := signalLoop.NewLoad(irtypes.I32, iAlloca)
	signalCond := signalLoop.NewICmp(enum.IPredSLT, iVal, maxP)
	signalLoop.NewCondBr(signalCond, signalBody, joinPhase)

	// Get P[i].m, lock park_mutex, signal park_cond, unlock
	iVal2 := signalBody.NewLoad(irtypes.I32, iAlloca)
	i64Val := signalBody.NewZExt(iVal2, irtypes.I64)
	pPtr := signalBody.NewGetElementPtr(pTy, psTyped, i64Val)
	mField := signalBody.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldM)))
	mRaw := signalBody.NewLoad(irtypes.I8Ptr, mField)
	mPtr := signalBody.NewBitCast(mRaw, irtypes.NewPointer(mTy))

	parkMtxField := signalBody.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkMutex)))
	parkMtx := signalBody.NewLoad(irtypes.I8Ptr, parkMtxField)
	signalBody.NewCall(c.palMutexLock, parkMtx)

	parkCondField := signalBody.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkCond)))
	parkCond := signalBody.NewLoad(irtypes.I8Ptr, parkCondField)
	signalBody.NewCall(c.palCondSignal, parkCond)

	signalBody.NewCall(c.palMutexUnlock, parkMtx)

	nextI := signalBody.NewAdd(iVal2, constant.NewInt(irtypes.I32, 1))
	signalBody.NewStore(nextI, iAlloca)
	signalBody.NewBr(signalLoop)

	// Join phase: join all M threads by iterating Ps
	joinPhase.NewStore(constant.NewInt(irtypes.I32, 0), iAlloca)

	joinLoop := fn.NewBlock("join_loop")
	joinBody := fn.NewBlock("join_body")
	doneBlk := fn.NewBlock("done")

	joinPhase.NewBr(joinLoop)

	jVal := joinLoop.NewLoad(irtypes.I32, iAlloca)
	joinCond := joinLoop.NewICmp(enum.IPredSLT, jVal, maxP)
	joinLoop.NewCondBr(joinCond, joinBody, doneBlk)

	jVal2 := joinBody.NewLoad(irtypes.I32, iAlloca)
	j64Val := joinBody.NewZExt(jVal2, irtypes.I64)
	jpPtr := joinBody.NewGetElementPtr(pTy, psTyped, j64Val)
	jmField := joinBody.NewGetElementPtr(pTy, jpPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldM)))
	jmRaw := joinBody.NewLoad(irtypes.I8Ptr, jmField)
	jmPtr := joinBody.NewBitCast(jmRaw, irtypes.NewPointer(mTy))
	thField := joinBody.NewGetElementPtr(mTy, jmPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldThreadHandle)))
	th := joinBody.NewLoad(irtypes.I8Ptr, thField)
	joinBody.NewCall(c.palThreadJoin, th)

	jNextI := joinBody.NewAdd(jVal2, constant.NewInt(irtypes.I32, 1))
	joinBody.NewStore(jNextI, iAlloca)
	joinBody.NewBr(joinLoop)

	// Join sysmon thread — it checks the shutdown flag every 10ms and exits
	sysmonField := doneBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldSysmonHandle)))
	sysmonHandle := doneBlk.NewLoad(irtypes.I8Ptr, sysmonField)
	doneBlk.NewCall(c.palThreadJoin, sysmonHandle)

	// Join reactor thread if present (T0070) — it checks shutdown flag every 10ms
	reactorThField := doneBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReactorThread)))
	reactorTh := doneBlk.NewLoad(irtypes.I8Ptr, reactorThField)
	hasReactor := doneBlk.NewICmp(enum.IPredNE, reactorTh, constant.NewNull(irtypes.I8Ptr))
	joinReactorBlk := fn.NewBlock("join_reactor")
	afterReactorBlk := fn.NewBlock("after_reactor")
	doneBlk.NewCondBr(hasReactor, joinReactorBlk, afterReactorBlk)

	joinReactorBlk.NewCall(c.palThreadJoin, reactorTh)
	// Close reactor fd via pal_reactor_close if emitted
	reactorFdField := joinReactorBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReactorFd)))
	reactorFd := joinReactorBlk.NewLoad(irtypes.I32, reactorFdField)
	if c.palReactorClose != nil {
		joinReactorBlk.NewCall(c.palReactorClose, reactorFd)
	}
	// Destroy reactor lock if present
	reactorLockField := joinReactorBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReactorLock)))
	reactorLock := joinReactorBlk.NewLoad(irtypes.I8Ptr, reactorLockField)
	reactorLockNonNull := joinReactorBlk.NewICmp(enum.IPredNE, reactorLock, constant.NewNull(irtypes.I8Ptr))
	destroyReactorLockBlk := fn.NewBlock("destroy_reactor_lock")
	joinReactorBlk.NewCondBr(reactorLockNonNull, destroyReactorLockBlk, afterReactorBlk)

	destroyReactorLockBlk.NewCall(c.palMutexDestroy, reactorLock)
	destroyReactorLockBlk.NewBr(afterReactorBlk)

	// Sysmon and reactor are now joined — safe to destroy per-M resources and free P array
	cleanupLoop := fn.NewBlock("cleanup_loop")
	cleanupBody := fn.NewBlock("cleanup_body")
	freeBlk := fn.NewBlock("free_ps")

	if c.netpollBatchLock != nil {
		// Destroy batch lock if initialized — null when reactor init failed (B0324)
		batchLockVal := afterReactorBlk.NewLoad(irtypes.I8Ptr, c.netpollBatchLock)
		hasBatchLock := afterReactorBlk.NewICmp(enum.IPredNE, batchLockVal, constant.NewNull(irtypes.I8Ptr))
		destroyBatchBlk := fn.NewBlock("destroy_batch_lock")
		afterBatchBlk := fn.NewBlock("after_batch_lock")
		afterReactorBlk.NewCondBr(hasBatchLock, destroyBatchBlk, afterBatchBlk)

		destroyBatchBlk.NewCall(c.palMutexDestroy, batchLockVal)
		destroyBatchBlk.NewBr(afterBatchBlk)

		afterBatchBlk.NewStore(constant.NewInt(irtypes.I32, 0), iAlloca)
		afterBatchBlk.NewBr(cleanupLoop)
	} else {
		afterReactorBlk.NewStore(constant.NewInt(irtypes.I32, 0), iAlloca)
		afterReactorBlk.NewBr(cleanupLoop)
	}

	cVal := cleanupLoop.NewLoad(irtypes.I32, iAlloca)
	cleanupCond := cleanupLoop.NewICmp(enum.IPredSLT, cVal, maxP)
	cleanupLoop.NewCondBr(cleanupCond, cleanupBody, freeBlk)

	// Destroy P's lock, M's park_mutex/park_cond, and free M
	cVal2 := cleanupBody.NewLoad(irtypes.I32, iAlloca)
	c64Val := cleanupBody.NewZExt(cVal2, irtypes.I64)
	cpPtr := cleanupBody.NewGetElementPtr(pTy, psTyped, c64Val)

	// Destroy P.lock
	cpLockField := cleanupBody.NewGetElementPtr(pTy, cpPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldLock)))
	cpLock := cleanupBody.NewLoad(irtypes.I8Ptr, cpLockField)
	cleanupBody.NewCall(c.palMutexDestroy, cpLock)

	// Get M from P
	cmField := cleanupBody.NewGetElementPtr(pTy, cpPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldM)))
	cmRaw := cleanupBody.NewLoad(irtypes.I8Ptr, cmField)
	cmPtr := cleanupBody.NewBitCast(cmRaw, irtypes.NewPointer(mTy))

	// Destroy M.park_mutex
	cmParkMtxField := cleanupBody.NewGetElementPtr(mTy, cmPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkMutex)))
	cmParkMtx := cleanupBody.NewLoad(irtypes.I8Ptr, cmParkMtxField)
	cleanupBody.NewCall(c.palMutexDestroy, cmParkMtx)

	// Destroy M.park_cond
	cmParkCondField := cleanupBody.NewGetElementPtr(mTy, cmPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkCond)))
	cmParkCond := cleanupBody.NewLoad(irtypes.I8Ptr, cmParkCondField)
	cleanupBody.NewCall(c.palCondDestroy, cmParkCond)

	// Free M
	cleanupBody.NewCall(c.palFree, cmRaw)

	cNextI := cleanupBody.NewAdd(cVal2, constant.NewInt(irtypes.I32, 1))
	cleanupBody.NewStore(cNextI, iAlloca)
	cleanupBody.NewBr(cleanupLoop)

	// Destroy scheduler mutexes and conds
	glLockField := freeBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalLock)))
	glLock := freeBlk.NewLoad(irtypes.I8Ptr, glLockField)
	freeBlk.NewCall(c.palMutexDestroy, glLock)

	imLockField := freeBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldIdleMLock)))
	imLock := freeBlk.NewLoad(irtypes.I8Ptr, imLockField)
	freeBlk.NewCall(c.palMutexDestroy, imLock)

	mdMutexField := freeBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDoneMutex)))
	mdMutex := freeBlk.NewLoad(irtypes.I8Ptr, mdMutexField)
	freeBlk.NewCall(c.palMutexDestroy, mdMutex)

	mdCondField := freeBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDoneCond)))
	mdCond := freeBlk.NewLoad(irtypes.I8Ptr, mdCondField)
	freeBlk.NewCall(c.palCondDestroy, mdCond)

	doneLockField := freeBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldDoneLock)))
	doneLock := freeBlk.NewLoad(irtypes.I8Ptr, doneLockField)
	freeBlk.NewCall(c.palMutexDestroy, doneLock)

	// Free P array (each P contains its M inline via pointer)
	freeBlk.NewCall(c.palFree, psRaw)

	freeBlk.NewRet(nil)

	c.funcs["promise_sched_shutdown"] = fn
}

// wrapMainWithScheduler transforms the user's @main into @__promise_user_main
// and creates a new @main that initializes the scheduler, wraps user main as
// goroutine G0, and runs the scheduler until G0 completes.
func (c *Compiler) wrapMainWithScheduler() {
	mainFn := c.funcs["main"]
	if mainFn == nil || c.mainDecl == nil {
		return // no main function (e.g., test-only compilation)
	}

	// If main is failable, compile its body into a separate helper function.
	// raise inside a coroutine would try to return a result struct from an
	// i8*-returning function, causing a type mismatch crash.
	var mainBodyFn *ir.Func
	mainIsFailable := false
	if obj := c.lookupFunc("main"); obj != nil {
		if sig, ok := obj.Type().(*types.Signature); ok && sig.CanError() {
			mainIsFailable = true
			var innerType irtypes.Type = irtypes.Void
			if sig.Result() != nil {
				innerType = c.resolveType(sig.Result())
			}
			resultType := computeResultType(innerType)
			mainBodyFn = c.module.NewFunc("__promise_main_body", resultType)
			c.defineFunc(c.mainDecl, mainBodyFn)
		}
	}

	// mainFn was declared by declareFuncs with i32 return and argc/argv params.
	// Use it directly as the OS entry point — add scheduler setup code.
	entry := mainFn.NewBlock(".entry")

	// Store argc/argv into globals for os.args() / os.executable()
	entry.NewStore(mainFn.Params[0], c.argcGlobal)
	entry.NewStore(mainFn.Params[1], c.argvGlobal)

	// Register stack overflow signal handler before any threads are created (B0010)
	entry.NewCall(c.palStackOverflowInit)

	if c.isWasm {
		// WASM: create @_start entry point that calls __promise_init_heap then @main
		c.emitWasmStart(mainFn)
		// Init scheduler with 1 P (single-threaded cooperative)
		entry.NewCall(c.funcs["promise_sched_init"], constant.NewInt(irtypes.I32, 1))
	} else {
		// Native: num_cpus = pal_num_cpus()
		numCPUs := entry.NewCall(c.palNumCPUs)
		entry.NewCall(c.funcs["promise_sched_init"], numCPUs)
	}

	// Initialize IO reactor if the net module is imported (T0071).
	// Must run after sched_init so the scheduler globals are available.
	if c.needsNetpoll {
		if initFn, ok := c.funcs["promise_netpoll_init"]; ok {
			entry.NewCall(initFn)
		}
	}

	// Create coroutine for user main — compile body inline with inCoroutine=true
	// so that channel ops in main() use coroutine parking instead of thread-blocking.
	coroFn := c.module.NewFunc(".goroutine.main", irtypes.I8Ptr)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))

	// Save compiler state (same pattern as genGoBlock)
	savedFn := c.fn
	savedBlock := c.block
	savedEntryBlock := c.entryBlock
	savedLocals := c.locals
	savedLocalNameCount := c.localNameCount
	savedCanError := c.canError
	savedRetType := c.currentRetType
	savedBlockCounter := c.blockCounter
	savedScopeBindings := c.scopeBindings
	savedDropFlags := c.dropFlags
	savedCastSubjectMatch := c.castSubjectMatch // T0849: function-scoped, like dropFlags
	savedLoopScopeDepth := c.loopScopeDepth
	savedInCoroutine := c.inCoroutine
	savedCoroCleanup := c.coroCleanupBlk
	savedCoroSuspend := c.coroSuspendBlk
	savedPanicExitBlock := c.panicExitBlock
	savedGoExprFF := c.goExprFireAndForget
	c.goExprFireAndForget = false // reset for inner statements (B0109)

	c.fn = coroFn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = nil
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil // T0849: fresh per generated function body
	c.dropBindings = make(map[string]scopeBinding)
	c.stmtTemps = nil                         // T0073
	c.stmtTempMap = make(map[value.Value]int) // T0073
	c.heapTemps = nil                         // T0088
	c.heapTempMap = make(map[value.Value]int) // T0088
	c.envTemps = nil                          // T0100
	c.envTempMap = make(map[value.Value]int)  // T0100
	c.enumCtorTemps = nil                     // B0267
	c.tempTrackingEnabled = true              // T0100: enable temp tracking in main goroutine
	c.loopScopeDepth = 0
	c.inCoroutine = true

	// --- Coroutine preamble ---
	coroEntry := coroFn.NewBlock(".entry")
	c.block = coroEntry

	coroId := coroEntry.NewCall(c.coroId,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr))

	need := coroEntry.NewCall(c.coroAlloc, coroId)
	allocBlk := coroFn.NewBlock("coro.alloc")
	startBlk := coroFn.NewBlock("coro.start")
	coroEntry.NewCondBr(need, allocBlk, startBlk)

	coroSizeVal := allocBlk.NewCall(c.coroSize)
	var coroSizeArg value.Value = coroSizeVal
	if c.isWasm {
		coroSizeArg = allocBlk.NewZExt(coroSizeVal, irtypes.I64)
	}
	mem := allocBlk.NewCall(c.palAlloc, coroSizeArg)
	allocBlk.NewBr(startBlk)

	phiMem := startBlk.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), coroEntry),
		ir.NewIncoming(mem, allocBlk))
	hdl := startBlk.NewCall(c.coroBegin, coroId, phiMem)

	// Initial suspend — in a separate block so that createEntryAlloca can
	// append allocas to startBlk BEFORE the suspend point. coro-split needs
	// allocas to precede coro.suspend to properly spill them to the frame.
	initSuspBlk := coroFn.NewBlock("coro.init.suspend")
	startBlk.NewBr(initSuspBlk)

	initResult := initSuspBlk.NewCall(c.coroSuspend, constant.None, constant.False)

	suspendBlk := coroFn.NewBlock("coro.suspend")
	bodyBlk := coroFn.NewBlock("body")
	cleanupBlk := coroFn.NewBlock("cleanup")
	doneBlk := coroFn.NewBlock("coro.done")

	// T0858: create finalSuspBlk early so an explicit `return;` in the main
	// body can branch here (via c.coroutineReturnBlock) instead of emitting
	// a bare `ret void` against the coroutine's i8* result type.
	finalSuspBlk := coroFn.NewBlock("final.suspend")

	initSuspBlk.NewSwitch(initResult, suspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), bodyBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	suspendBlk.NewRet(hdl)

	// Set cleanup and suspend blocks for mid-body coro.suspend switches
	c.coroCleanupBlk = cleanupBlk
	c.coroSuspendBlk = doneBlk

	// --- Body ---
	c.block = bodyBlk
	c.entryBlock = startBlk // allocas go in startBlk (part of coroutine frame)

	// B0228: Create panic exit block for the main coroutine.
	// When a panic occurs in the main body, emitPanicReturn branches here
	// instead of doing ret (which would bypass the coroutine framework).
	// This block writes the panic message to stderr and exits.
	mainPanicExitBlk := coroFn.NewBlock("main.panic_exit")
	c.panicExitBlock = mainPanicExitBlk

	if mainIsFailable {
		// Call the helper function and check the result
		result := c.block.NewCall(mainBodyFn)
		tag := c.block.NewExtractValue(result, 0) // i1: false=ok, true=error
		errBlk := coroFn.NewBlock("main.error")
		okBlk := coroFn.NewBlock("main.ok")
		c.block.NewCondBr(tag, errBlk, okBlk)

		// Error path: terminal — write message to stderr and exit
		panicStr := constant.NewCharArrayFromString("panic: unhandled error in main\n")
		panicGlobal := c.module.NewGlobalDef(".str.main_error", panicStr)
		panicGlobal.Immutable = true
		panicGlobal.Linkage = enum.LinkagePrivate
		msgPtr := errBlk.NewGetElementPtr(panicGlobal.ContentType, panicGlobal,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		errBlk.NewCall(c.palWrite, constant.NewInt(irtypes.I32, 2), msgPtr, constant.NewInt(irtypes.I64, 30))
		errBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 1))
		errBlk.NewUnreachable()

		c.block = okBlk
	} else {
		c.coroutineReturnBlock = finalSuspBlk // T0858
		c.genBlock(c.mainDecl.Body)
		c.coroutineReturnBlock = nil // T0858
	}

	// Clear panic exit block — main body generation is done
	c.panicExitBlock = nil

	// B0228: After main body, check TLS panic flag. Panics from Promise-level
	// functions (assert, panic_msg) set the flag but don't trigger emitPanicReturn
	// at this level (they just return). Check here before final suspend.
	if c.block != nil && c.block.Term == nil {
		mainBodyFlag := c.block.NewLoad(irtypes.I8, c.panicFlagGlobal)
		mainBodyPanicked := c.block.NewICmp(enum.IPredNE, mainBodyFlag, constant.NewInt(irtypes.I8, 0))
		mainBodyOk := coroFn.NewBlock("main.body_ok")
		c.block.NewCondBr(mainBodyPanicked, mainPanicExitBlk, mainBodyOk)
		c.block = mainBodyOk
	}

	// B0228: Define the panic exit block body.
	// Load the panic message from TLS, write "panic: <msg>\n" to stderr, exit(1).
	{
		stderr := constant.NewInt(irtypes.I32, 2)
		pePrefix := constant.NewCharArrayFromString("panic: ")
		pePrefixGlobal := c.module.NewGlobalDef(".str.panic_exit_prefix", pePrefix)
		pePrefixGlobal.Immutable = true
		pePrefixGlobal.Linkage = enum.LinkagePrivate
		pePrefixPtr := mainPanicExitBlk.NewGetElementPtr(pePrefixGlobal.ContentType, pePrefixGlobal,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		mainPanicExitBlk.NewCall(c.palWrite, stderr, pePrefixPtr, constant.NewInt(irtypes.I64, 7))

		peMsgPtr := mainPanicExitBlk.NewLoad(irtypes.I8Ptr, c.panicMsgTlsGlobal)
		peMsgLen := mainPanicExitBlk.NewCall(c.funcs["strlen"], peMsgPtr)
		mainPanicExitBlk.NewCall(c.palWrite, stderr, peMsgPtr, peMsgLen)
		c.emitWriteNewline(mainPanicExitBlk, stderr)
		mainPanicExitBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 1))
		mainPanicExitBlk.NewUnreachable()
	}

	// Final suspend: yield back to scheduler (finalSuspBlk created earlier, T0858)
	if c.block != nil && c.block.Term == nil {
		c.block.NewBr(finalSuspBlk)
	}

	// --- Cleanup: free coroutine memory (only reached via destroy path) ---
	coroMem := cleanupBlk.NewCall(c.coroFree, coroId, hdl)
	needFree := cleanupBlk.NewICmp(enum.IPredNE, coroMem, constant.NewNull(irtypes.I8Ptr))
	freeBlk := coroFn.NewBlock("coro.free")
	cleanupBlk.NewCondBr(needFree, freeBlk, doneBlk)

	freeBlk.NewCall(c.palFree, coroMem)
	freeBlk.NewBr(doneBlk)

	// Done: single coro.end
	doneBlk.NewCall(c.coroEnd, hdl, constant.False, constant.None)
	doneBlk.NewRet(hdl)

	// Final suspend switch
	finalResult := finalSuspBlk.NewCall(c.coroSuspend, constant.None, constant.True)
	finalSuspBlk.NewSwitch(finalResult, doneBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), doneBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// --- Restore compiler state ---
	c.fn = savedFn
	c.block = savedBlock
	c.entryBlock = savedEntryBlock
	c.locals = savedLocals
	c.localNameCount = savedLocalNameCount
	c.canError = savedCanError
	c.currentRetType = savedRetType
	c.blockCounter = savedBlockCounter
	c.scopeBindings = savedScopeBindings
	c.dropFlags = savedDropFlags
	c.castSubjectMatch = savedCastSubjectMatch // T0849
	c.loopScopeDepth = savedLoopScopeDepth
	c.inCoroutine = savedInCoroutine
	c.coroCleanupBlk = savedCoroCleanup
	c.coroSuspendBlk = savedCoroSuspend
	c.panicExitBlock = savedPanicExitBlock
	c.goExprFireAndForget = savedGoExprFF

	// Back in @main: call the ramp, create G0, enqueue, run, shutdown
	handle := entry.NewCall(coroFn)
	g0 := entry.NewCall(c.funcs["promise_g_new"], handle)
	entry.NewCall(c.funcs["promise_sched_enqueue"], g0)

	if c.isWasm {
		// WASM: cooperative run loop (single-threaded, no M threads)
		entry.NewCall(c.funcs["promise_sched_coop_run"])
	} else {
		entry.NewCall(c.funcs["promise_sched_run_until_main"], g0)
		entry.NewCall(c.funcs["promise_sched_shutdown"])
	}

	// On Windows, call ExitProcess(0) to avoid CRT cleanup crashes during
	// thread teardown (STATUS_ACCESS_VIOLATION in TLS callbacks). B0148.
	if c.isWindows && !c.isWasm {
		entry.NewCall(c.palExit, constant.NewInt(irtypes.I32, 0))
		entry.NewUnreachable()
	} else {
		entry.NewRet(constant.NewInt(irtypes.I32, 0))
	}

	// Windows: emit the self-contained crt0 entry + CRT-replacement runtime
	// support so the linker needs no MSVC/SDK files (T0772).
	if c.isWindows && !c.isWasm {
		c.emitWindowsEntry(mainFn)
	}
}

// defineSchedRunUntilMainFunc emits @promise_sched_run_until_main(i8* %g0) → void
// The initial thread (M0) waits until the main goroutine (G0) completes.
func (c *Compiler) defineSchedRunUntilMainFunc() {
	g0Param := ir.NewParam("g0", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_sched_run_until_main", irtypes.Void, g0Param)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	schedTy := schedStructType()

	entry := fn.NewBlock(".entry")
	waitLoop := fn.NewBlock("wait_loop")
	waitBlk := fn.NewBlock("wait")
	doneBlk := fn.NewBlock("done")

	// Load main_done mutex and cond
	mdMtxField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDoneMutex)))
	mdMtx := entry.NewLoad(irtypes.I8Ptr, mdMtxField)

	mdCondField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDoneCond)))
	mdCond := entry.NewLoad(irtypes.I8Ptr, mdCondField)

	// Lock mutex
	entry.NewCall(c.palMutexLock, mdMtx)
	entry.NewBr(waitLoop)

	// wait_loop: check main_done flag
	mdField := waitLoop.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDone)))
	mainDone := waitLoop.NewLoad(irtypes.I8, mdField)
	isDone := waitLoop.NewICmp(enum.IPredNE, mainDone, constant.NewInt(irtypes.I8, 0))
	waitLoop.NewCondBr(isDone, doneBlk, waitBlk)

	// wait: cond_wait then loop back
	waitBlk.NewCall(c.palCondWait, mdCond, mdMtx)
	waitBlk.NewBr(waitLoop)

	// done: unlock and return
	doneBlk.NewCall(c.palMutexUnlock, mdMtx)
	doneBlk.NewRet(nil)

	c.funcs["promise_sched_run_until_main"] = fn
}

// --- Cooperative scheduler (WASM) ---

// defineSchedCoopStepFunc emits @promise_sched_coop_step() → i8 (T0668)
// One iteration of the single-threaded cooperative scheduler: find a runnable
// G, resume it, and handle its coro.done / park / yield result. Returns 1 if a
// G ran (progress possible), 0 if no runnable G was found. Factored out of
// promise_sched_coop_run so the legacy callable Task[T].drop spin (reached from
// genuinely non-coroutine drop bodies on WASM — synthesized struct/enum/Arc
// field drops, Promise Map[K,Task].drop) can pump the scheduler instead of a
// no-op usleep livelock.
//
// Re-entrancy: this may be called from within a running goroutine's call stack
// (Task[T].drop spin nested under that goroutine), so the incoming TLS
// current_g/current_p are saved on entry and restored on every return path —
// when called from promise_sched_coop_run's loop they are null, so this is a
// no-op there and preserves the original loop semantics exactly.
func (c *Compiler) defineSchedCoopStepFunc() {
	fn := c.module.NewFunc("promise_sched_coop_step", irtypes.I8)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	schedTy := schedStructType()
	gTy := goroutineStructType()
	pTy := processorStructType()

	entry := fn.NewBlock(".entry")
	foundG := fn.NewBlock("found_g")
	noG := fn.NewBlock("no_g")
	afterResume := fn.NewBlock("after_resume")
	coroDoneBlk := fn.NewBlock("coro_done")
	coroSuspendedBlk := fn.NewBlock("coro_suspended")
	releaseMtxBlk := fn.NewBlock("release_park_mutex")
	yieldReenqueueBlk := fn.NewBlock("yield_reenqueue")
	afterReleaseBlk := fn.NewBlock("after_release")
	ranGBlk := fn.NewBlock("ran_g")

	// Save incoming TLS current_g/current_p for re-entrant callers (restored
	// on every return path so a nested pump doesn't clobber the outer G).
	savedG := entry.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
	savedP := entry.NewLoad(irtypes.I8Ptr, c.currentPGlobal)

	// Get P0 from sched.allPs[0]
	psField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldPs)))
	psRaw := entry.NewLoad(irtypes.I8Ptr, psField)
	// P0 is the first element — cast to P* then back to i8*
	psTyped := entry.NewBitCast(psRaw, irtypes.NewPointer(pTy))
	p0 := entry.NewBitCast(psTyped, irtypes.I8Ptr)

	// find runnable G via find_runnable (tries local queue, global queue, steal)
	gRaw := entry.NewCall(c.funcs["promise_sched_find_runnable"], p0)
	gNull := entry.NewICmp(enum.IPredEQ, gRaw, constant.NewNull(irtypes.I8Ptr))
	entry.NewCondBr(gNull, noG, foundG)

	// noG: no runnable G — return 0 (current_g/current_p untouched).
	noG.NewRet(constant.NewInt(irtypes.I8, 0))

	// foundG: set current_g/current_p, set G.status=running, resume coroutine
	foundG.NewStore(gRaw, c.currentGGlobal)
	foundG.NewStore(p0, c.currentPGlobal)

	// G.status = running
	gPtr := foundG.NewBitCast(gRaw, irtypes.NewPointer(gTy))
	statusField := foundG.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	foundG.NewStore(constant.NewInt(irtypes.I8, gStatusRunning), statusField)

	// P.current_g = gRaw
	pPtr := foundG.NewBitCast(p0, irtypes.NewPointer(pTy))
	curGField := foundG.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldCurrentG)))
	foundG.NewStore(gRaw, curGField)

	// Increment context_switches
	ctxField := foundG.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldContextSwitches)))
	ctxVal := foundG.NewLoad(irtypes.I64, ctxField)
	foundG.NewStore(foundG.NewAdd(ctxVal, constant.NewInt(irtypes.I64, 1)), ctxField)

	// Load coro handle and resume
	handleField := foundG.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldCoroHandle)))
	coroHandle := foundG.NewLoad(irtypes.I8Ptr, handleField)
	foundG.NewCall(c.coroResume, coroHandle)
	foundG.NewBr(afterResume)

	// afterResume: reload G from current_g (safety), check coro.done
	gRaw2 := afterResume.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
	gPtr2 := afterResume.NewBitCast(gRaw2, irtypes.NewPointer(gTy))
	handleField2 := afterResume.NewGetElementPtr(gTy, gPtr2,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldCoroHandle)))
	coroHandle2 := afterResume.NewLoad(irtypes.I8Ptr, handleField2)

	isDoneCoro := afterResume.NewCall(c.coroDone, coroHandle2)
	afterResume.NewCondBr(isDoneCoro, coroDoneBlk, coroSuspendedBlk)

	// coroDone: goroutine finished — clear P.current_g, call goroutine_exit
	pRaw2 := coroDoneBlk.NewLoad(irtypes.I8Ptr, c.currentPGlobal)
	pPtr2 := coroDoneBlk.NewBitCast(pRaw2, irtypes.NewPointer(pTy))
	pCurGField2 := coroDoneBlk.NewGetElementPtr(pTy, pPtr2,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldCurrentG)))
	coroDoneBlk.NewStore(constant.NewNull(irtypes.I8Ptr), pCurGField2)
	coroDoneBlk.NewCall(c.funcs["promise_goroutine_exit"], gRaw2)
	coroDoneBlk.NewBr(ranGBlk)

	// coroSuspended: check park_mutex to distinguish park vs yield
	pmField := coroSuspendedBlk.NewGetElementPtr(gTy, gPtr2,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
	parkMtx := coroSuspendedBlk.NewLoad(irtypes.I8Ptr, pmField)
	hasParkMtx := coroSuspendedBlk.NewICmp(enum.IPredNE, parkMtx, constant.NewNull(irtypes.I8Ptr))
	coroSuspendedBlk.NewCondBr(hasParkMtx, releaseMtxBlk, yieldReenqueueBlk)

	// release_park_mutex: goroutine parked (on waiter list) — clear field THEN unlock.
	// B0249: Must clear park_mutex before unlock (see sched_loop comment).
	releaseMtxBlk.NewStore(constant.NewNull(irtypes.I8Ptr), pmField)
	releaseMtxBlk.NewCall(c.palMutexUnlock, parkMtx)
	releaseMtxBlk.NewBr(afterReleaseBlk)

	// yield_reenqueue: cooperative yield — re-enqueue G
	pRawY := yieldReenqueueBlk.NewLoad(irtypes.I8Ptr, c.currentPGlobal)
	pPtrY := yieldReenqueueBlk.NewBitCast(pRawY, irtypes.NewPointer(pTy))
	pCurGFieldY := yieldReenqueueBlk.NewGetElementPtr(pTy, pPtrY,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldCurrentG)))
	yieldReenqueueBlk.NewStore(constant.NewNull(irtypes.I8Ptr), pCurGFieldY)
	yieldReenqueueBlk.NewCall(c.funcs["promise_sched_enqueue"], gRaw2)
	yieldReenqueueBlk.NewBr(ranGBlk)

	// after_release: parked path — clear P.current_g, then ran-a-G return
	pRaw3 := afterReleaseBlk.NewLoad(irtypes.I8Ptr, c.currentPGlobal)
	pPtr3 := afterReleaseBlk.NewBitCast(pRaw3, irtypes.NewPointer(pTy))
	pCurGField3 := afterReleaseBlk.NewGetElementPtr(pTy, pPtr3,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldCurrentG)))
	afterReleaseBlk.NewStore(constant.NewNull(irtypes.I8Ptr), pCurGField3)
	afterReleaseBlk.NewBr(ranGBlk)

	// ran_g: a G ran (done/parked/yielded) — restore the saved TLS context for
	// re-entrant callers (null when called from coop_run's loop) and return 1.
	ranGBlk.NewStore(savedG, c.currentGGlobal)
	ranGBlk.NewStore(savedP, c.currentPGlobal)
	ranGBlk.NewRet(constant.NewInt(irtypes.I8, 1))

	c.funcs["promise_sched_coop_step"] = fn
}

// defineSchedCoopRunFunc emits @promise_sched_coop_run() → void
// Single-threaded cooperative event loop for WASM. No threads, no stealing, no sysmon.
// Runs goroutines until main completes. Deadlocks if no runnable G and main not done.
// T0668: the per-step work is factored into promise_sched_coop_step(); this is
// the thin driver loop. Semantics are unchanged — keep running Gs while one is
// runnable, and only when none is runnable check main_done (exit) vs deadlock.
func (c *Compiler) defineSchedCoopRunFunc() {
	fn := c.module.NewFunc("promise_sched_coop_run", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	schedTy := schedStructType()

	entry := fn.NewBlock(".entry")
	loop := fn.NewBlock("loop")
	noG := fn.NewBlock("no_g")
	doneBlk := fn.NewBlock("done")
	deadlockBlk := fn.NewBlock("deadlock")

	entry.NewBr(loop)

	// loop: run one cooperative step. 1 = ran a G (keep going), 0 = nothing runnable.
	stepR := loop.NewCall(c.funcs["promise_sched_coop_step"])
	ranG := loop.NewICmp(enum.IPredNE, stepR, constant.NewInt(irtypes.I8, 0))
	loop.NewCondBr(ranG, loop, noG)

	// noG: no runnable G — exit if main completed, else deadlock.
	mdField := noG.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDone)))
	mainDone := noG.NewLoad(irtypes.I8, mdField)
	mainIsDone := noG.NewICmp(enum.IPredNE, mainDone, constant.NewInt(irtypes.I8, 0))
	noG.NewCondBr(mainIsDone, doneBlk, deadlockBlk)

	// done: main completed
	doneBlk.NewRet(nil)

	// deadlock: terminal — write message to stderr and exit(2)
	deadlockMsg := constant.NewCharArrayFromString("fatal: all goroutines are asleep - deadlock!\n")
	deadlockGlobal := c.module.NewGlobalDef(".str.deadlock", deadlockMsg)
	deadlockGlobal.Immutable = true
	deadlockGlobal.Linkage = enum.LinkagePrivate
	deadlockPtr := deadlockBlk.NewGetElementPtr(deadlockGlobal.ContentType, deadlockGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	deadlockBlk.NewCall(c.palWrite, constant.NewInt(irtypes.I32, 2), deadlockPtr, constant.NewInt(irtypes.I64, 45))
	deadlockBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 2))
	deadlockBlk.NewUnreachable()

	c.funcs["promise_sched_coop_run"] = fn
}

// --- Waiter list helpers (for channel goroutine parking) ---

// defineWaiterEnqueueFunc emits @promise_waiter_enqueue(i8** %head_ptr, i8** %tail_ptr, i8* %g) → void
// Appends G to a waiter list (FIFO). Sets G.wait_next = null, links at tail.
// Caller must hold the channel mutex.
func (c *Compiler) defineWaiterEnqueueFunc() {
	i8PtrPtr := irtypes.NewPointer(irtypes.I8Ptr)
	headParam := ir.NewParam("head_ptr", i8PtrPtr)
	tailParam := ir.NewParam("tail_ptr", i8PtrPtr)
	gParam := ir.NewParam("g", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_waiter_enqueue", irtypes.Void, headParam, tailParam, gParam)

	gTy := goroutineStructType()
	gPtrTy := irtypes.NewPointer(gTy)

	entry := fn.NewBlock(".entry")
	setHeadBlk := fn.NewBlock("set_head")
	linkTailBlk := fn.NewBlock("link_tail")

	// G.wait_next = null
	gTyped := entry.NewBitCast(gParam, gPtrTy)
	waitNextPtr := entry.NewGetElementPtr(gTy, gTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldWaitNext)))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), waitNextPtr)

	// G.status = waiting
	statusPtr := entry.NewGetElementPtr(gTy, gTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	entry.NewStore(constant.NewInt(irtypes.I8, gStatusWaiting), statusPtr)

	// current_tail = load *tail_ptr
	currentTail := entry.NewLoad(irtypes.I8Ptr, tailParam)
	isEmpty := entry.NewICmp(enum.IPredEQ, currentTail, constant.NewNull(irtypes.I8Ptr))
	entry.NewCondBr(isEmpty, setHeadBlk, linkTailBlk)

	// set_head: list was empty → head = tail = g
	setHeadBlk.NewStore(gParam, headParam)
	setHeadBlk.NewStore(gParam, tailParam)
	setHeadBlk.NewRet(nil)

	// link_tail: tail.wait_next = g, tail = g
	tailTyped := linkTailBlk.NewBitCast(currentTail, gPtrTy)
	tailWaitNext := linkTailBlk.NewGetElementPtr(gTy, tailTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldWaitNext)))
	linkTailBlk.NewStore(gParam, tailWaitNext)
	linkTailBlk.NewStore(gParam, tailParam)
	linkTailBlk.NewRet(nil)

	c.funcs["promise_waiter_enqueue"] = fn
}

// defineWaiterDequeueFunc emits @promise_waiter_dequeue(i8** %head_ptr, i8** %tail_ptr) → i8*
// Removes and returns the head G from a waiter list (FIFO). Returns null if empty.
// Caller must hold the channel mutex.
func (c *Compiler) defineWaiterDequeueFunc() {
	i8PtrPtr := irtypes.NewPointer(irtypes.I8Ptr)
	headParam := ir.NewParam("head_ptr", i8PtrPtr)
	tailParam := ir.NewParam("tail_ptr", i8PtrPtr)
	fn := c.module.NewFunc("promise_waiter_dequeue", irtypes.I8Ptr, headParam, tailParam)

	gTy := goroutineStructType()
	gPtrTy := irtypes.NewPointer(gTy)

	entry := fn.NewBlock(".entry")
	dequeueBlk := fn.NewBlock("dequeue")
	clearTailBlk := fn.NewBlock("clear_tail")
	doneBlk := fn.NewBlock("done")

	// head = load *head_ptr
	head := entry.NewLoad(irtypes.I8Ptr, headParam)
	isEmpty := entry.NewICmp(enum.IPredEQ, head, constant.NewNull(irtypes.I8Ptr))
	entry.NewCondBr(isEmpty, doneBlk, dequeueBlk)

	// dequeue: next = head.wait_next; *head_ptr = next
	headTyped := dequeueBlk.NewBitCast(head, gPtrTy)
	waitNextPtr := dequeueBlk.NewGetElementPtr(gTy, headTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldWaitNext)))
	next := dequeueBlk.NewLoad(irtypes.I8Ptr, waitNextPtr)
	dequeueBlk.NewStore(next, headParam)

	// Clear dequeued G's wait_next
	dequeueBlk.NewStore(constant.NewNull(irtypes.I8Ptr), waitNextPtr)

	// If next is null, clear tail
	nextIsNull := dequeueBlk.NewICmp(enum.IPredEQ, next, constant.NewNull(irtypes.I8Ptr))
	dequeueBlk.NewCondBr(nextIsNull, clearTailBlk, doneBlk)

	clearTailBlk.NewStore(constant.NewNull(irtypes.I8Ptr), tailParam)
	clearTailBlk.NewBr(doneBlk)

	// done: phi returns head or null
	result := doneBlk.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entry),
		ir.NewIncoming(head, dequeueBlk),
		ir.NewIncoming(head, clearTailBlk))
	doneBlk.NewRet(result)

	c.funcs["promise_waiter_dequeue"] = fn
}

// defineWaiterWakeAllFunc emits @promise_waiter_wake_all(i8** %head_ptr, i8** %tail_ptr) → void
// Dequeues all nodes from the waiter list, waking each one.
// Handles both regular G nodes (field 1 = 0-4) and SelectWaiterNodes (field 1 = 0xFF).
// Caller must hold the channel mutex.
func (c *Compiler) defineWaiterWakeAllFunc() {
	i8PtrPtr := irtypes.NewPointer(irtypes.I8Ptr)
	headParam := ir.NewParam("head_ptr", i8PtrPtr)
	tailParam := ir.NewParam("tail_ptr", i8PtrPtr)
	fn := c.module.NewFunc("promise_waiter_wake_all", irtypes.Void, headParam, tailParam)

	gTy := goroutineStructType()
	gPtrTy := irtypes.NewPointer(gTy)

	entry := fn.NewBlock(".entry")
	loopBlk := fn.NewBlock("loop")
	checkKindBlk := fn.NewBlock("check_kind")
	regularWakeBlk := fn.NewBlock("regular_wake")
	selectWakeBlk := fn.NewBlock("select_wake")
	doneBlk := fn.NewBlock("done")

	entry.NewBr(loopBlk)

	// loop: dequeue head
	g := loopBlk.NewCall(c.funcs["promise_waiter_dequeue"], headParam, tailParam)
	isNull := loopBlk.NewICmp(enum.IPredEQ, g, constant.NewNull(irtypes.I8Ptr))
	loopBlk.NewCondBr(isNull, doneBlk, checkKindBlk)

	// check_kind: inspect field 1 (status/kind)
	gTyped := checkKindBlk.NewBitCast(g, gPtrTy)
	statusPtr := checkKindBlk.NewGetElementPtr(gTy, gTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	kind := checkKindBlk.NewLoad(irtypes.I8, statusPtr)
	isSWN := checkKindBlk.NewICmp(enum.IPredEQ, kind, constant.NewInt(irtypes.I8, swnKindSentinel))
	checkKindBlk.NewCondBr(isSWN, selectWakeBlk, regularWakeBlk)

	// regular_wake: set runnable, enqueue, loop
	regularWakeBlk.NewStore(constant.NewInt(irtypes.I8, gStatusRunnable), statusPtr)
	regularWakeBlk.NewCall(c.funcs["promise_sched_enqueue"], g)
	regularWakeBlk.NewBr(loopBlk)

	// select_wake: call select_try_wake (may fail if already woken), then loop
	selectWakeBlk.NewCall(c.funcs["promise_select_try_wake"], g)
	selectWakeBlk.NewBr(loopBlk)

	doneBlk.NewRet(nil)

	c.funcs["promise_waiter_wake_all"] = fn
}

// defineWaiterRemoveFunc emits @promise_waiter_remove(i8** %head_ptr, i8** %tail_ptr, i8* %target) → void
// Removes a specific G from a waiter list. Used by select to clean up after one case triggers.
// Caller must hold the channel mutex.
func (c *Compiler) defineWaiterRemoveFunc() {
	i8PtrPtr := irtypes.NewPointer(irtypes.I8Ptr)
	headParam := ir.NewParam("head_ptr", i8PtrPtr)
	tailParam := ir.NewParam("tail_ptr", i8PtrPtr)
	targetParam := ir.NewParam("target", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_waiter_remove", irtypes.Void, headParam, tailParam, targetParam)

	gTy := goroutineStructType()
	gPtrTy := irtypes.NewPointer(gTy)

	entry := fn.NewBlock(".entry")
	checkHeadBlk := fn.NewBlock("check_head")
	removeHeadBlk := fn.NewBlock("remove_head")
	searchBlk := fn.NewBlock("search")
	foundBlk := fn.NewBlock("found")
	nextBlk := fn.NewBlock("next")
	doneBlk := fn.NewBlock("done")

	// If list is empty, return
	head := entry.NewLoad(irtypes.I8Ptr, headParam)
	isEmpty := entry.NewICmp(enum.IPredEQ, head, constant.NewNull(irtypes.I8Ptr))
	entry.NewCondBr(isEmpty, doneBlk, checkHeadBlk)

	// Check if target is head
	c.block = checkHeadBlk
	isHead := checkHeadBlk.NewICmp(enum.IPredEQ, head, targetParam)
	checkHeadBlk.NewCondBr(isHead, removeHeadBlk, searchBlk)

	// Remove head: *head = head.wait_next
	headTyped := removeHeadBlk.NewBitCast(head, gPtrTy)
	headWaitNext := removeHeadBlk.NewGetElementPtr(gTy, headTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldWaitNext)))
	headNext := removeHeadBlk.NewLoad(irtypes.I8Ptr, headWaitNext)
	removeHeadBlk.NewStore(headNext, headParam)
	removeHeadBlk.NewStore(constant.NewNull(irtypes.I8Ptr), headWaitNext)
	// If new head is null, clear tail
	headNextNull := removeHeadBlk.NewICmp(enum.IPredEQ, headNext, constant.NewNull(irtypes.I8Ptr))
	clearTailBlk := fn.NewBlock("clear_tail")
	removeHeadBlk.NewCondBr(headNextNull, clearTailBlk, doneBlk)
	clearTailBlk.NewStore(constant.NewNull(irtypes.I8Ptr), tailParam)
	clearTailBlk.NewBr(doneBlk)

	// Search: iterate through list, prev starts at head
	prevPhi := searchBlk.NewPhi(ir.NewIncoming(head, checkHeadBlk), ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), nextBlk))
	prevTyped := searchBlk.NewBitCast(prevPhi, gPtrTy)
	prevWaitNext := searchBlk.NewGetElementPtr(gTy, prevTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldWaitNext)))
	curr := searchBlk.NewLoad(irtypes.I8Ptr, prevWaitNext)
	currNull := searchBlk.NewICmp(enum.IPredEQ, curr, constant.NewNull(irtypes.I8Ptr))
	currCheckBlk := fn.NewBlock("curr_check")
	searchBlk.NewCondBr(currNull, doneBlk, currCheckBlk)

	// Check if curr == target
	isTarget := currCheckBlk.NewICmp(enum.IPredEQ, curr, targetParam)
	currCheckBlk.NewCondBr(isTarget, foundBlk, nextBlk)

	// Found: prev.wait_next = curr.wait_next; if curr was tail, update tail
	currTyped := foundBlk.NewBitCast(curr, gPtrTy)
	currWaitNext := foundBlk.NewGetElementPtr(gTy, currTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldWaitNext)))
	currNext := foundBlk.NewLoad(irtypes.I8Ptr, currWaitNext)
	foundBlk.NewStore(currNext, prevWaitNext)
	foundBlk.NewStore(constant.NewNull(irtypes.I8Ptr), currWaitNext)
	// If curr was tail (currNext == null), update tail to prev
	currWasTail := foundBlk.NewICmp(enum.IPredEQ, currNext, constant.NewNull(irtypes.I8Ptr))
	updateTailBlk := fn.NewBlock("update_tail")
	foundBlk.NewCondBr(currWasTail, updateTailBlk, doneBlk)
	prevRaw := updateTailBlk.NewBitCast(prevTyped, irtypes.I8Ptr)
	updateTailBlk.NewStore(prevRaw, tailParam)
	updateTailBlk.NewBr(doneBlk)

	// Next: advance prev = curr, continue search
	nextBlk.NewBr(searchBlk)
	// Fix phi: we need curr as prev for next iteration
	prevPhi.Incs[1] = ir.NewIncoming(curr, nextBlk)

	doneBlk.NewRet(nil)

	c.funcs["promise_waiter_remove"] = fn
}

// --- Select waiter support (B0008) ---
//
// SelectWaiterNode (SWN) is layout-compatible with G at fields 0–4 so that
// the existing waiter_dequeue and waiter_remove functions work on mixed lists
// of G and SWN nodes. Field 1 (i8, same position as G.status) is set to 0xFF
// as a sentinel — valid G status values are 0–4, so after dequeue the caller
// can distinguish SWN from G by checking field 1.
//
// Layout:
//   0: i8*  (padding, aligns with G.coro_handle)
//   1: i8   kind = 0xFF (sentinel, aligns with G.status)
//   2: i8*  (padding, aligns with G.wait_data)
//   3: i8*  (padding, aligns with G.sched_next)
//   4: i8*  next (waiter list linking, aligns with G.wait_next)
//   5: i8*  g (back-pointer to owning G)
//   6: i32  case_index
//   7: i8*  select_mutex (for wake-once protocol; stored here because
//           the scheduler clears G.park_mutex before unlocking it)

const swnKindSentinel = 0xFF

// SWN field indices (fields 0–4 match G layout for waiter list compatibility).
const (
	swnFieldNext        = 4 // same offset as gFieldWaitNext
	swnFieldG           = 5
	swnFieldCaseIndex   = 6
	swnFieldSelectMutex = 7
)

// selectWaiterNodeType returns the LLVM struct type for a SelectWaiterNode.
func selectWaiterNodeType() *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I8Ptr, // pad0 (aligns with G.coro_handle)
		irtypes.I8,    // kind = 0xFF sentinel (aligns with G.status)
		irtypes.I8Ptr, // pad2 (aligns with G.wait_data)
		irtypes.I8Ptr, // pad3 (aligns with G.sched_next)
		irtypes.I8Ptr, // next (waiter list linking, aligns with G.wait_next)
		irtypes.I8Ptr, // g (back-pointer to owning G)
		irtypes.I32,   // case_index
		irtypes.I8Ptr, // select_mutex (for wake-once protocol)
	)
}

// defineSelectWaiterEnqueueFunc emits @promise_select_waiter_enqueue(i8** %head, i8** %tail, i8* %swn).
// Like waiter_enqueue but does NOT set field 1 (kind sentinel is pre-set by caller).
// Uses field 4 (next) for linking — same offset as G.wait_next.
func (c *Compiler) defineSelectWaiterEnqueueFunc() {
	i8PtrPtr := irtypes.NewPointer(irtypes.I8Ptr)
	headParam := ir.NewParam("head_ptr", i8PtrPtr)
	tailParam := ir.NewParam("tail_ptr", i8PtrPtr)
	swnParam := ir.NewParam("swn", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_select_waiter_enqueue", irtypes.Void, headParam, tailParam, swnParam)

	swnTy := selectWaiterNodeType()
	swnPtrTy := irtypes.NewPointer(swnTy)

	entry := fn.NewBlock(".entry")
	setHeadBlk := fn.NewBlock("set_head")
	linkTailBlk := fn.NewBlock("link_tail")

	// SWN.next = null (field 4)
	swnTyped := entry.NewBitCast(swnParam, swnPtrTy)
	nextPtr := entry.NewGetElementPtr(swnTy, swnTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(swnFieldNext)))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), nextPtr)

	// NOTE: we do NOT set field 1 (kind) — caller pre-sets to 0xFF

	// current_tail = load *tail_ptr
	currentTail := entry.NewLoad(irtypes.I8Ptr, tailParam)
	isEmpty := entry.NewICmp(enum.IPredEQ, currentTail, constant.NewNull(irtypes.I8Ptr))
	entry.NewCondBr(isEmpty, setHeadBlk, linkTailBlk)

	// set_head: list was empty → head = tail = swn
	setHeadBlk.NewStore(swnParam, headParam)
	setHeadBlk.NewStore(swnParam, tailParam)
	setHeadBlk.NewRet(nil)

	// link_tail: tail.wait_next = swn, tail = swn
	// The tail could be either a G or an SWN — both have field 4 (wait_next/next)
	// at the same offset. We use G type for GEP since layout is compatible at field 4.
	gTy := goroutineStructType()
	gPtrTy := irtypes.NewPointer(gTy)
	tailTyped := linkTailBlk.NewBitCast(currentTail, gPtrTy)
	tailWaitNext := linkTailBlk.NewGetElementPtr(gTy, tailTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldWaitNext)))
	linkTailBlk.NewStore(swnParam, tailWaitNext)
	linkTailBlk.NewStore(swnParam, tailParam)
	linkTailBlk.NewRet(nil)

	c.funcs["promise_select_waiter_enqueue"] = fn
}

// defineSelectTryWakeFunc emits @promise_select_try_wake(i8* %swn) → i1.
// Implements the wake-once protocol for select waiters. Returns true if this
// call successfully claimed the select (set G.select_case), false if another
// waker already claimed it.
//
//  1. Read SWN.g, SWN.case_index, SWN.select_mutex
//  2. Lock select_mutex
//  3. If G.select_case != -1: already woken → unlock, return false
//  4. Set G.select_case = case_index, mark runnable, enqueue G
//  5. Unlock select_mutex, return true
func (c *Compiler) defineSelectTryWakeFunc() {
	swnParam := ir.NewParam("swn", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_select_try_wake", irtypes.I1, swnParam)

	swnTy := selectWaiterNodeType()
	swnPtrTy := irtypes.NewPointer(swnTy)
	gTy := goroutineStructType()
	gPtrTy := irtypes.NewPointer(gTy)

	entry := fn.NewBlock(".entry")
	alreadyWokenBlk := fn.NewBlock("already_woken")
	doWakeBlk := fn.NewBlock("do_wake")

	// Read SWN.g (field 5), SWN.case_index (field 6), SWN.select_mutex (field 7)
	swnTyped := entry.NewBitCast(swnParam, swnPtrTy)
	gField := entry.NewGetElementPtr(swnTy, swnTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(swnFieldG)))
	gRaw := entry.NewLoad(irtypes.I8Ptr, gField)

	caseField := entry.NewGetElementPtr(swnTy, swnTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(swnFieldCaseIndex)))
	caseIdx := entry.NewLoad(irtypes.I32, caseField)

	// Read select_mutex from SWN (not from G.park_mutex, which the scheduler clears)
	smField := entry.NewGetElementPtr(swnTy, swnTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(swnFieldSelectMutex)))
	selectMtx := entry.NewLoad(irtypes.I8Ptr, smField)

	gTyped := entry.NewBitCast(gRaw, gPtrTy)

	// Lock select_mutex
	entry.NewCall(c.palMutexLock, selectMtx)

	// Check G.select_case: if != -1 (0xFFFFFFFF as u32), already woken
	scField := entry.NewGetElementPtr(gTy, gTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldSelectCase)))
	currentCase := entry.NewLoad(irtypes.I32, scField)
	neg1 := constant.NewInt(irtypes.I32, 0xFFFFFFFF) // -1 as unsigned i32
	isUnclaimed := entry.NewICmp(enum.IPredEQ, currentCase, neg1)
	entry.NewCondBr(isUnclaimed, doWakeBlk, alreadyWokenBlk)

	// already_woken: unlock and return false
	alreadyWokenBlk.NewCall(c.palMutexUnlock, selectMtx)
	alreadyWokenBlk.NewRet(constant.False)

	// do_wake: claim the select, mark runnable, enqueue, return true
	doWakeBlk.NewStore(caseIdx, scField)
	statusPtr := doWakeBlk.NewGetElementPtr(gTy, gTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	doWakeBlk.NewStore(constant.NewInt(irtypes.I8, gStatusRunnable), statusPtr)
	doWakeBlk.NewCall(c.funcs["promise_sched_enqueue"], gRaw)
	doWakeBlk.NewCall(c.palMutexUnlock, selectMtx)
	doWakeBlk.NewRet(constant.True)

	c.funcs["promise_select_try_wake"] = fn
}

// defineWaiterWakeOneFunc emits @promise_waiter_wake_one(i8** %head, i8** %tail, i8* %cond) → void.
// Dequeues waiters from the list until one is successfully woken.
// Handles both regular G nodes (field 1 = 0-4) and SelectWaiterNodes (field 1 = 0xFF).
// For regular G: set status = runnable, enqueue.
// For SWN: call select_try_wake; if it fails (goroutine already woken by another case),
// dequeue the next waiter and retry. If list is exhausted, signal the cond var.
func (c *Compiler) defineWaiterWakeOneFunc() {
	i8PtrPtr := irtypes.NewPointer(irtypes.I8Ptr)
	headParam := ir.NewParam("head_ptr", i8PtrPtr)
	tailParam := ir.NewParam("tail_ptr", i8PtrPtr)
	condParam := ir.NewParam("cond", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_waiter_wake_one", irtypes.Void, headParam, tailParam, condParam)

	gTy := goroutineStructType()
	gPtrTy := irtypes.NewPointer(gTy)

	entry := fn.NewBlock(".entry")
	loopBlk := fn.NewBlock("loop")
	checkKindBlk := fn.NewBlock("check_kind")
	regularWakeBlk := fn.NewBlock("regular_wake")
	selectWakeBlk := fn.NewBlock("select_wake")
	signalBlk := fn.NewBlock("signal")
	doneBlk := fn.NewBlock("done")

	entry.NewBr(loopBlk)

	// loop: dequeue a waiter
	waiter := loopBlk.NewCall(c.funcs["promise_waiter_dequeue"], headParam, tailParam)
	isNull := loopBlk.NewICmp(enum.IPredEQ, waiter, constant.NewNull(irtypes.I8Ptr))
	loopBlk.NewCondBr(isNull, signalBlk, checkKindBlk)

	// check_kind: inspect field 1 (status/kind)
	waiterTyped := checkKindBlk.NewBitCast(waiter, gPtrTy)
	statusPtr := checkKindBlk.NewGetElementPtr(gTy, waiterTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	kind := checkKindBlk.NewLoad(irtypes.I8, statusPtr)
	isSWN := checkKindBlk.NewICmp(enum.IPredEQ, kind, constant.NewInt(irtypes.I8, swnKindSentinel))
	checkKindBlk.NewCondBr(isSWN, selectWakeBlk, regularWakeBlk)

	// regular_wake: set status = runnable, enqueue, done
	regularWakeBlk.NewStore(constant.NewInt(irtypes.I8, gStatusRunnable), statusPtr)
	regularWakeBlk.NewCall(c.funcs["promise_sched_enqueue"], waiter)
	regularWakeBlk.NewBr(doneBlk)

	// select_wake: call try_wake, branch on its i1 return value.
	// true = we claimed the select and woke G → done.
	// false = another waker already claimed it → dequeue next waiter and retry.
	woken := selectWakeBlk.NewCall(c.funcs["promise_select_try_wake"], waiter)
	selectWakeBlk.NewCondBr(woken, doneBlk, loopBlk)

	// signal: no waiter found (or all SWNs stale) → signal cond var
	signalBlk.NewCall(c.palCondSignal, condParam)
	signalBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)

	c.funcs["promise_waiter_wake_one"] = fn
}
