package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// --- G (Goroutine) struct ---

// G struct field indices.
const (
	gFieldCoroHandle  = 0 // i8*  LLVM coroutine handle
	gFieldStatus      = 1 // i8   0=idle, 1=runnable, 2=running, 3=waiting, 4=dead
	gFieldWaitData    = 2 // i8*  context: &value for chan send, &result for chan recv
	gFieldSchedNext   = 3 // i8*  next G in run queue (intrusive linked list)
	gFieldWaitNext    = 4 // i8*  next G in channel wait queue
	gFieldID          = 5 // i64  goroutine ID (monotonic counter)
	gFieldResultPtr   = 6 // i8*  for task[T]: heap-allocated result storage
	gFieldDone        = 7 // i8   for task[T]: completion flag (0=running, 1=done)
	gFieldDoneWaiters = 8 // i8*  for task[T]: head of Gs waiting on <-task
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
// Layout: { i8*, i8, i8*, i8*, i8*, i64, i8*, i8, i8* } — 9 fields, 72 bytes.
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
	)
}

// --- P (Processor) struct ---

// P run queue size (fixed-size circular buffer).
const pRunQueueSize = 256

// P struct field indices.
const (
	pFieldID       = 0 // i32  processor id
	pFieldRunQueue = 1 // [256 x i8*]  circular buffer of G pointers
	pFieldRqHead   = 2 // i64  dequeue index
	pFieldRqTail   = 3 // i64  enqueue index
	pFieldCurrentG = 4 // i8*  currently running G
	pFieldM        = 5 // i8*  associated M (OS thread)
	pFieldLock     = 6 // i8*  mutex for queue overflow
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
	)
}

// --- Scheduler globals and initialization ---

// defineSchedulerGlobals defines the thread-local current-G pointer and the
// global scheduler singleton, and wires them into the compiler.
func (c *Compiler) defineSchedulerGlobals() {
	// @__promise_current_g = thread_local global i8* null
	currentG := c.module.NewGlobal("__promise_current_g", irtypes.I8Ptr)
	currentG.Init = constant.NewNull(irtypes.I8Ptr)
	currentG.TLSModel = enum.TLSModelGeneric
	c.currentGGlobal = currentG

	// @__promise_sched = global %Sched zeroinitializer
	schedTy := schedStructType()
	sched := c.module.NewGlobal("__promise_sched", schedTy)
	sched.Init = constant.NewZeroInitializer(schedTy)
	c.schedGlobal = sched
}

// defineGNewFunc emits @promise_g_new(i8* %coro_handle) → i8*
// Allocates and initializes a G struct with the given coroutine handle.
func (c *Compiler) defineGNewFunc() {
	handleParam := ir.NewParam("coro_handle", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_g_new", irtypes.I8Ptr, handleParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	gType := goroutineStructType()
	entry := fn.NewBlock("entry")

	// Allocate G struct
	structSize := constant.NewInt(irtypes.I64, int64(llvmTypeSize(gType)))
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

	// Assign goroutine ID: atomic increment of sched.goroutine_counter
	// For now, use a non-atomic load/add/store (sufficient for init path).
	schedTy := schedStructType()
	counterField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGoroutineCounter)))
	counter := entry.NewLoad(irtypes.I64, counterField)
	nextCounter := entry.NewAdd(counter, constant.NewInt(irtypes.I64, 1))
	entry.NewStore(nextCounter, counterField)

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

	entry := fn.NewBlock("entry")

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

	// Store num_p
	numPField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldNumP)))
	entry.NewStore(numCPUsParam, numPField)

	// Allocate P array: num_cpus * sizeof(P)
	numP64 := entry.NewZExt(numCPUsParam, irtypes.I64)
	pSize := constant.NewInt(irtypes.I64, int64(llvmTypeSize(pTy)))
	totalPSize := entry.NewMul(numP64, pSize)
	psRaw := entry.NewCall(c.palAlloc, totalPSize)

	// Store ps pointer
	psField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldPs)))
	entry.NewStore(psRaw, psField)

	// Initialize each P in a loop
	// We use a counted loop: for i = 0; i < num_cpus; i++
	loopHeader := fn.NewBlock("p_loop_header")
	loopBody := fn.NewBlock("p_loop_body")
	loopEnd := fn.NewBlock("p_loop_end")

	entry.NewBr(loopHeader)

	// Loop header: phi i = 0 from entry, i+1 from body
	iAlloca := entry.NewAlloca(irtypes.I32) // simplify: use alloca instead of phi
	entry.NewStore(constant.NewInt(irtypes.I32, 0), iAlloca)
	// NOTE: entry already has a br to loopHeader, but we stored to iAlloca before the br.
	// Fix: the alloca and store must be before the br. Let me restructure.

	// Actually, entry block already branched. Let me use a different approach:
	// Use the phi node pattern properly.

	// Remove the alloca approach — need to restructure. Let me use phi properly.
	// The problem is that entry already has a br terminator.

	// Let me redo this: we need entry → setup, then jump to loop header with phi.
	// But llir/llvm doesn't allow modifying terminators. Let me just use a pre-loop block.

	// Actually, I already added alloca in entry block, then added br. The alloca came first
	// so it's fine — but we need to load in the loop header, not use phi.
	// Since we already have entry → br loopHeader, let's use the alloca approach with load/store.

	// Loop header: load i, check i < num_cpus
	iVal := loopHeader.NewLoad(irtypes.I32, iAlloca)
	cond := loopHeader.NewICmp(enum.IPredSLT, iVal, numCPUsParam)
	loopHeader.NewCondBr(cond, loopBody, loopEnd)

	// Loop body: init P[i]
	iVal2 := loopBody.NewLoad(irtypes.I32, iAlloca)
	i64Val := loopBody.NewZExt(iVal2, irtypes.I64)

	// Get pointer to P[i]
	psTyped := loopBody.NewBitCast(psRaw, irtypes.NewPointer(pTy))
	pPtr := loopBody.NewGetElementPtr(pTy, psTyped, i64Val)

	// P.id = i
	pIdField := loopBody.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldID)))
	loopBody.NewStore(iVal2, pIdField)

	// P.rq_head = 0
	pHeadField := loopBody.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRqHead)))
	loopBody.NewStore(constant.NewInt(irtypes.I64, 0), pHeadField)

	// P.rq_tail = 0
	pTailField := loopBody.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldRqTail)))
	loopBody.NewStore(constant.NewInt(irtypes.I64, 0), pTailField)

	// P.current_g = null
	pCurGField := loopBody.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldCurrentG)))
	loopBody.NewStore(constant.NewNull(irtypes.I8Ptr), pCurGField)

	// P.lock = pal_mutex_init()
	pLock := loopBody.NewCall(c.palMutexInit)
	pLockField := loopBody.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldLock)))
	loopBody.NewStore(pLock, pLockField)

	// Allocate and init M for this P
	mSize := constant.NewInt(irtypes.I64, int64(llvmTypeSize(mTy)))
	mRaw := loopBody.NewCall(c.palAlloc, mSize)
	mPtr := loopBody.NewBitCast(mRaw, irtypes.NewPointer(mTy))

	// M.p = pPtr as i8*
	mPField := loopBody.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldP)))
	pAsI8 := loopBody.NewBitCast(pPtr, irtypes.I8Ptr)
	loopBody.NewStore(pAsI8, mPField)

	// M.park_mutex = pal_mutex_init()
	mParkMtx := loopBody.NewCall(c.palMutexInit)
	mPmField := loopBody.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkMutex)))
	loopBody.NewStore(mParkMtx, mPmField)

	// M.park_cond = pal_cond_init()
	mParkCond := loopBody.NewCall(c.palCondInit)
	mPcField := loopBody.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkCond)))
	loopBody.NewStore(mParkCond, mPcField)

	// M.spinning = 0
	mSpinField := loopBody.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldSpinning)))
	loopBody.NewStore(constant.NewInt(irtypes.I8, 0), mSpinField)

	// P.m = mPtr as i8*
	pMField := loopBody.NewGetElementPtr(pTy, pPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pFieldM)))
	mAsI8 := loopBody.NewBitCast(mPtr, irtypes.I8Ptr)
	loopBody.NewStore(mAsI8, pMField)

	// Create thread: pal_thread_create(sched_loop_fn, mRaw)
	// We need the sched_loop function reference. Since it may not exist yet,
	// we store the function ref as a bitcast of i8* to the appropriate fn ptr type.
	schedLoopFn := c.funcs["promise_sched_loop"]
	loopFnPtr := loopBody.NewBitCast(schedLoopFn, irtypes.I8Ptr)
	handle := loopBody.NewCall(c.palThreadCreate, loopFnPtr, mRaw)

	// M.thread_handle = handle
	mThField := loopBody.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldThreadHandle)))
	loopBody.NewStore(handle, mThField)

	// i++
	nextI := loopBody.NewAdd(iVal2, constant.NewInt(irtypes.I32, 1))
	loopBody.NewStore(nextI, iAlloca)
	loopBody.NewBr(loopHeader)

	// Loop end: done
	loopEnd.NewRet(nil)

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

	entry := fn.NewBlock("entry")
	loop := fn.NewBlock("loop")
	checkShutdown := fn.NewBlock("check_shutdown")
	runG := fn.NewBlock("run_g")
	afterResume := fn.NewBlock("after_resume")
	coroDoneBlk := fn.NewBlock("coro_done")
	coroSuspendedBlk := fn.NewBlock("coro_suspended")
	parkM := fn.NewBlock("park_m")
	exitBlk := fn.NewBlock("exit")

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
	// Set @__promise_current_g = gRaw
	runG.NewStore(gRaw, c.currentGGlobal)

	// G.status = running
	gPtr := runG.NewBitCast(gRaw, irtypes.NewPointer(gTy))
	statusField := runG.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	runG.NewStore(constant.NewInt(irtypes.I8, gStatusRunning), statusField)

	// Load coro handle
	handleField := runG.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldCoroHandle)))
	coroHandle := runG.NewLoad(irtypes.I8Ptr, handleField)

	// coro.resume(handle)
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
	coroDoneBlk.NewCall(c.funcs["promise_goroutine_exit"], gRaw2)
	// Clear current G
	coroDoneBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.currentGGlobal)
	coroDoneBlk.NewBr(loop)

	// coroSuspended: G suspended itself (channel op), already in a wait queue
	// Clear current G
	coroSuspendedBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.currentGGlobal)
	coroSuspendedBlk.NewBr(loop)

	// exit: return null
	exitBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	c.funcs["promise_sched_loop"] = fn
}

// defineSchedEnqueueFunc emits @promise_sched_enqueue(i8* %g_raw) → void
// Adds a runnable G to the global run queue and wakes an idle M.
// (Simplified: always uses global queue. Local queue optimization in Step 11.)
func (c *Compiler) defineSchedEnqueueFunc() {
	gParam := ir.NewParam("g_raw", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_sched_enqueue", irtypes.Void, gParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	gTy := goroutineStructType()
	schedTy := schedStructType()

	entry := fn.NewBlock("entry")

	// Set G.status = runnable
	gPtr := entry.NewBitCast(gParam, irtypes.NewPointer(gTy))
	statusField := entry.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	entry.NewStore(constant.NewInt(irtypes.I8, gStatusRunnable), statusField)

	// Lock global queue
	glLockField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalLock)))
	glLock := entry.NewLoad(irtypes.I8Ptr, glLockField)
	entry.NewCall(c.palMutexLock, glLock)

	// G.sched_next = null (tail of list)
	snField := entry.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldSchedNext)))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), snField)

	// if sched.global_tail != null: tail.sched_next = gParam
	// else: sched.global_head = gParam
	tailField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalTail)))
	tail := entry.NewLoad(irtypes.I8Ptr, tailField)
	tailIsNull := entry.NewICmp(enum.IPredEQ, tail, constant.NewNull(irtypes.I8Ptr))

	setHead := fn.NewBlock("set_head")
	setTailNext := fn.NewBlock("set_tail_next")
	updateTail := fn.NewBlock("update_tail")

	entry.NewCondBr(tailIsNull, setHead, setTailNext)

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

	// Wake an idle M
	updateTail.NewCall(c.funcs["promise_sched_wake_m"])

	updateTail.NewRet(nil)

	c.funcs["promise_sched_enqueue"] = fn
}

// defineSchedFindRunnableFunc emits @promise_sched_find_runnable(i8* %p_raw) → i8*
// Tries to dequeue a G from the global run queue (simplified — no local queue / steal yet).
func (c *Compiler) defineSchedFindRunnableFunc() {
	pParam := ir.NewParam("p_raw", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_sched_find_runnable", irtypes.I8Ptr, pParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	gTy := goroutineStructType()
	schedTy := schedStructType()

	entry := fn.NewBlock("entry")

	// Lock global queue
	glLockField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalLock)))
	glLock := entry.NewLoad(irtypes.I8Ptr, glLockField)
	entry.NewCall(c.palMutexLock, glLock)

	// Check if global queue is empty
	headField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalHead)))
	head := entry.NewLoad(irtypes.I8Ptr, headField)
	isEmpty := entry.NewICmp(enum.IPredEQ, head, constant.NewNull(irtypes.I8Ptr))

	emptyBlk := fn.NewBlock("empty")
	dequeueBlk := fn.NewBlock("dequeue")

	entry.NewCondBr(isEmpty, emptyBlk, dequeueBlk)

	// empty: unlock and return null
	emptyBlk.NewCall(c.palMutexUnlock, glLock)
	emptyBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	// dequeue: pop head
	gPtr := dequeueBlk.NewBitCast(head, irtypes.NewPointer(gTy))
	nextField := dequeueBlk.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldSchedNext)))
	next := dequeueBlk.NewLoad(irtypes.I8Ptr, nextField)

	// sched.global_head = next
	dequeueBlk.NewStore(next, headField)

	// if next == null: sched.global_tail = null
	nextIsNull := dequeueBlk.NewICmp(enum.IPredEQ, next, constant.NewNull(irtypes.I8Ptr))
	clearTail := fn.NewBlock("clear_tail")
	doneDq := fn.NewBlock("done_dequeue")

	dequeueBlk.NewCondBr(nextIsNull, clearTail, doneDq)

	tailField := clearTail.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalTail)))
	clearTail.NewStore(constant.NewNull(irtypes.I8Ptr), tailField)
	clearTail.NewBr(doneDq)

	// done_dequeue: size--, unlock, return head
	sizeField := doneDq.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGlobalSize)))
	size := doneDq.NewLoad(irtypes.I64, sizeField)
	newSize := doneDq.NewSub(size, constant.NewInt(irtypes.I64, 1))
	doneDq.NewStore(newSize, sizeField)

	doneDq.NewCall(c.palMutexUnlock, glLock)

	// Clear G.sched_next
	nextField2 := doneDq.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldSchedNext)))
	doneDq.NewStore(constant.NewNull(irtypes.I8Ptr), nextField2)

	doneDq.NewRet(head)

	c.funcs["promise_sched_find_runnable"] = fn
}

// defineSchedParkMFunc emits @promise_sched_park_m(i8* %m_raw) → void
// Parks an M (OS thread) until woken by promise_sched_wake_m or shutdown.
//
// Protocol: park_mutex is locked BEFORE adding to idle list, then cond_wait
// is called while still holding it. This prevents the lost-signal race where
// wake_m/shutdown could signal between idle-list push and cond_wait.
// M.p is saved/restored since it's reused as the idle stack next pointer.
func (c *Compiler) defineSchedParkMFunc() {
	mParam := ir.NewParam("m_raw", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_sched_park_m", irtypes.Void, mParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	mTy := machineStructType()
	schedTy := schedStructType()

	entry := fn.NewBlock("entry")

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

	entry.NewCall(c.palMutexUnlock, imLock)

	// cond_wait (park_mutex already held — released atomically by cond_wait)
	parkCondField := entry.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkCond)))
	parkCond := entry.NewLoad(irtypes.I8Ptr, parkCondField)
	entry.NewCall(c.palCondWait, parkCond, parkMtx)

	entry.NewCall(c.palMutexUnlock, parkMtx)

	// Restore M.p to original P pointer
	entry.NewStore(savedP, mPField)

	entry.NewRet(nil)

	c.funcs["promise_sched_park_m"] = fn
}

// defineSchedWakeMFunc emits @promise_sched_wake_m() → void
// Pops an M from the idle stack and signals its park_cond.
func (c *Compiler) defineSchedWakeMFunc() {
	fn := c.module.NewFunc("promise_sched_wake_m", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	mTy := machineStructType()
	schedTy := schedStructType()

	entry := fn.NewBlock("entry")

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

	// Signal M's park_cond while holding park_mutex (prevents lost-signal race)
	parkMtxField := wakeBlk.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkMutex)))
	parkMtx := wakeBlk.NewLoad(irtypes.I8Ptr, parkMtxField)
	wakeBlk.NewCall(c.palMutexLock, parkMtx)

	parkCondField := wakeBlk.NewGetElementPtr(mTy, mPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mFieldParkCond)))
	parkCond := wakeBlk.NewLoad(irtypes.I8Ptr, parkCondField)
	wakeBlk.NewCall(c.palCondSignal, parkCond)

	wakeBlk.NewCall(c.palMutexUnlock, parkMtx)

	wakeBlk.NewRet(nil)

	c.funcs["promise_sched_wake_m"] = fn
}

// defineGoroutineExitFunc emits @promise_goroutine_exit(i8* %g_raw) → void
// Marks G as dead, signals done_waiters, destroys coroutine, frees G.
func (c *Compiler) defineGoroutineExitFunc() {
	gParam := ir.NewParam("g_raw", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_goroutine_exit", irtypes.Void, gParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	gTy := goroutineStructType()

	entry := fn.NewBlock("entry")

	gPtr := entry.NewBitCast(gParam, irtypes.NewPointer(gTy))

	// G.status = dead
	statusField := entry.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	entry.NewStore(constant.NewInt(irtypes.I8, gStatusDead), statusField)

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

	// waitersDone: destroy coroutine
	handleField := waitersDone.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldCoroHandle)))
	coroHandle := waitersDone.NewLoad(irtypes.I8Ptr, handleField)
	waitersDone.NewCall(c.coroDestroy, coroHandle)

	// Signal main_done if this is G0 (id == 0)
	idField := waitersDone.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldID)))
	gID := waitersDone.NewLoad(irtypes.I64, idField)
	isG0 := waitersDone.NewICmp(enum.IPredEQ, gID, constant.NewInt(irtypes.I64, 0))

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
	// Check if result_ptr is non-null (task[T]) — if so, don't free yet
	rpField := freeG.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
	rpVal := freeG.NewLoad(irtypes.I8Ptr, rpField)
	isTask := freeG.NewICmp(enum.IPredNE, rpVal, constant.NewNull(irtypes.I8Ptr))

	skipFree := fn.NewBlock("skip_free")
	doFree := fn.NewBlock("do_free")

	freeG.NewCondBr(isTask, skipFree, doFree)

	// skipFree: task[T] — caller will free G when they receive the result
	skipFree.NewRet(nil)

	// doFree: free G
	doFree.NewCall(c.palFree, gParam)
	doFree.NewRet(nil)

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

	entry := fn.NewBlock("entry")

	// Set shutdown flag
	shutdownField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldShutdown)))
	entry.NewStore(constant.NewInt(irtypes.I8, 1), shutdownField)

	// Signal ALL Ms via their Ps (not just idle list).
	// Hold each M's park_mutex when signaling to prevent lost-signal race.
	numPField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldNumP)))
	numP := entry.NewLoad(irtypes.I32, numPField)

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
	signalCond := signalLoop.NewICmp(enum.IPredSLT, iVal, numP)
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
	joinCond := joinLoop.NewICmp(enum.IPredSLT, jVal, numP)
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

	// done: cleanup scheduler resources
	doneBlk.NewCall(c.palFree, psRaw)
	doneBlk.NewRet(nil)

	c.funcs["promise_sched_shutdown"] = fn
}

// wrapMainWithScheduler transforms the user's @main into @__promise_user_main
// and creates a new @main that initializes the scheduler, wraps user main as
// goroutine G0, and runs the scheduler until G0 completes.
func (c *Compiler) wrapMainWithScheduler() {
	userMain := c.funcs["main"]
	if userMain == nil {
		return // no main function (e.g., test-only compilation)
	}

	// Rename user main to __promise_user_main
	userMain.SetName("__promise_user_main")
	c.funcs["__promise_user_main"] = userMain

	// Create new @main
	mainFn := c.module.NewFunc("main", irtypes.I32)
	c.funcs["main"] = mainFn

	entry := mainFn.NewBlock("entry")

	// Init scheduler: num_cpus = pal_num_cpus()
	numCPUs := entry.NewCall(c.palNumCPUs)
	entry.NewCall(c.funcs["promise_sched_init"], numCPUs)

	// Create coroutine wrapper for user main
	coroFn := c.module.NewFunc(".goroutine.main", irtypes.I8Ptr)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))

	// Build the coroutine body for user main
	coroEntry := coroFn.NewBlock("entry")

	coroId := coroEntry.NewCall(c.coroId,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr))

	need := coroEntry.NewCall(c.coroAlloc, coroId)
	allocBlk := coroFn.NewBlock("coro.alloc")
	startBlk := coroFn.NewBlock("coro.start")
	coroEntry.NewCondBr(need, allocBlk, startBlk)

	coroSize := allocBlk.NewCall(c.coroSize)
	mem := allocBlk.NewCall(c.palAlloc, coroSize)
	allocBlk.NewBr(startBlk)

	phiMem := startBlk.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), coroEntry),
		ir.NewIncoming(mem, allocBlk))
	hdl := startBlk.NewCall(c.coroBegin, coroId, phiMem)

	// Initial suspend
	initResult := startBlk.NewCall(c.coroSuspend, constant.None, constant.False)

	suspendBlk := coroFn.NewBlock("coro.suspend")
	bodyBlk := coroFn.NewBlock("body")
	cleanupBlk := coroFn.NewBlock("cleanup")

	startBlk.NewSwitch(initResult, suspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), bodyBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	suspendBlk.NewRet(hdl)

	// Body: call __promise_user_main()
	bodyBlk.NewCall(userMain)

	// Final suspend: keep frame alive so scheduler can call coro.done()
	finalSuspBlk := coroFn.NewBlock("final.suspend")
	bodyBlk.NewBr(finalSuspBlk)

	// Cleanup (only reached via destroy path)
	coroMem := cleanupBlk.NewCall(c.coroFree, coroId, hdl)
	needFree := cleanupBlk.NewICmp(enum.IPredNE, coroMem, constant.NewNull(irtypes.I8Ptr))
	freeBlk := coroFn.NewBlock("coro.free")
	doneBlk := coroFn.NewBlock("coro.done")
	cleanupBlk.NewCondBr(needFree, freeBlk, doneBlk)

	freeBlk.NewCall(c.palFree, coroMem)
	freeBlk.NewBr(doneBlk)

	// Done: single coro.end (both final-suspend exit and cleanup converge here)
	doneBlk.NewCall(c.coroEnd, hdl, constant.False, constant.None)
	doneBlk.NewRet(hdl)

	// Final suspend switch: default/i8 0 → doneBlk (skip free, just coro.end+ret)
	// i8 1 (destroy) → cleanup (free frame then coro.end+ret)
	finalResult := finalSuspBlk.NewCall(c.coroSuspend, constant.None, constant.True)
	finalSuspBlk.NewSwitch(finalResult, doneBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), doneBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// Back in @main: call the ramp, create G0, enqueue, wait, shutdown
	handle := entry.NewCall(coroFn)
	g0 := entry.NewCall(c.funcs["promise_g_new"], handle)
	entry.NewCall(c.funcs["promise_sched_enqueue"], g0)
	entry.NewCall(c.funcs["promise_sched_run_until_main"], g0)
	entry.NewCall(c.funcs["promise_sched_shutdown"])
	entry.NewRet(constant.NewInt(irtypes.I32, 0))
}

// defineSchedRunUntilMainFunc emits @promise_sched_run_until_main(i8* %g0) → void
// The initial thread (M0) waits until the main goroutine (G0) completes.
func (c *Compiler) defineSchedRunUntilMainFunc() {
	g0Param := ir.NewParam("g0", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_sched_run_until_main", irtypes.Void, g0Param)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	schedTy := schedStructType()

	entry := fn.NewBlock("entry")
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

	entry := fn.NewBlock("entry")
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

	entry := fn.NewBlock("entry")
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
// Dequeues all Gs from the waiter list, sets each to runnable, and enqueues in scheduler.
// Caller must hold the channel mutex.
func (c *Compiler) defineWaiterWakeAllFunc() {
	i8PtrPtr := irtypes.NewPointer(irtypes.I8Ptr)
	headParam := ir.NewParam("head_ptr", i8PtrPtr)
	tailParam := ir.NewParam("tail_ptr", i8PtrPtr)
	fn := c.module.NewFunc("promise_waiter_wake_all", irtypes.Void, headParam, tailParam)

	gTy := goroutineStructType()
	gPtrTy := irtypes.NewPointer(gTy)

	entry := fn.NewBlock("entry")
	loopBlk := fn.NewBlock("loop")
	wakeBlk := fn.NewBlock("wake")
	doneBlk := fn.NewBlock("done")

	entry.NewBr(loopBlk)

	// loop: dequeue head
	g := loopBlk.NewCall(c.funcs["promise_waiter_dequeue"], headParam, tailParam)
	isNull := loopBlk.NewICmp(enum.IPredEQ, g, constant.NewNull(irtypes.I8Ptr))
	loopBlk.NewCondBr(isNull, doneBlk, wakeBlk)

	// wake: set runnable, enqueue, loop
	gTyped := wakeBlk.NewBitCast(g, gPtrTy)
	statusPtr := wakeBlk.NewGetElementPtr(gTy, gTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	wakeBlk.NewStore(constant.NewInt(irtypes.I8, gStatusRunnable), statusPtr)
	wakeBlk.NewCall(c.funcs["promise_sched_enqueue"], g)
	wakeBlk.NewBr(loopBlk)

	doneBlk.NewRet(nil)

	c.funcs["promise_waiter_wake_all"] = fn
}
