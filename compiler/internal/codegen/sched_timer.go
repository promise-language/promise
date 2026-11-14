package codegen

import (
	"math"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// Timers: the goroutines parked in std's sleep on wasm32-web (docs/web-apps.md
// "The liveness rule", "Budget and continuation").
//
// A sleeping goroutine parks on a timer node that lives in its own coroutine
// frame — like a select waiter node — so arming a timer allocates nothing. The
// nodes form one list, sorted by deadline, headed at sched.timer_head and
// guarded by sched.timer_lock. An armed timer is a live host registration, so
// arming and expiring one moves promise_web_live_registrations: a program whose
// only pending work is a sleeper is a reactor until the sleeper wakes, and a
// command again afterwards.
//
// Who wakes a sleeper depends on who owns the thread:
//   - the reactor's drains (sched_wasm_web.go) never wait: finding nothing
//     runnable with a timer armed, they ask the host for a pump at the earliest
//     deadline (schedule_pump, kind Timer) and return;
//   - promise_sched_coop_run, which runs every per-test batch to completion,
//     waits for the earliest deadline itself (promise_sched_timer_idle);
//   - code outside a coroutine cannot suspend, so a sleep there steps the
//     scheduler until its own deadline (promise_sched_timer_sleep), and every
//     other wait there steps it until the wait is met: both wait for the
//     earliest timer when nothing is runnable (promise_sched_wait_step), so a
//     goroutine they wait on may sleep.
//
// The monotonic clock, not the host, decides when a timer is due: a host timer
// that fires early, late or twice only costs a drain that finds nothing due.

// Timer node field indices.
const (
	timerFieldDeadline = 0 // i64  monotonic-clock deadline (ns)
	timerFieldG        = 1 // i8*  the parked goroutine
	timerFieldNext     = 2 // i8*  next node in deadline order
)

// timerNoLimit is promise_sched_timer_idle's "no limit". A sleep's deadline
// saturates one below it (timerLatestDeadline), so a sleep that never ends is
// still an armed timer to wait on, never mistaken for having none.
const (
	timerNoLimit        = math.MaxInt64
	timerLatestDeadline = math.MaxInt64 - 1
)

// timerNodeType returns the LLVM struct type of a timer node.
func timerNodeType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I64, irtypes.I8Ptr, irtypes.I8Ptr)
}

// defineSchedTimerFuncs emits the timer runtime. Only called when c.isWasmWeb,
// after promise_sched_coop_step and the reactor globals.
func (c *Compiler) defineSchedTimerFuncs() {
	c.defineSchedTimerInsertFunc()
	c.defineSchedTimerExpireFunc()
	c.defineSchedTimerIdleFunc()
	c.defineSchedWaitStepFunc()
	c.defineSchedTimerSleepFunc()
	c.defineSchedTimerAbandonFunc()
}

// schedTimerField returns a pointer to the scheduler's timer field (head or lock).
func (c *Compiler) schedTimerField(blk *ir.Block, field int) value.Value {
	return blk.NewGetElementPtr(schedStructType(), c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(field)))
}

// timerNodeField returns a pointer to field of the timer node node (an i8*).
func timerNodeField(blk *ir.Block, node value.Value, field int) value.Value {
	ty := timerNodeType()
	typed := blk.NewBitCast(node, irtypes.NewPointer(ty))
	return blk.NewGetElementPtr(ty, typed,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(field)))
}

// addLiveRegistrations adds delta to promise_web_live_registrations.
func (c *Compiler) addLiveRegistrations(blk *ir.Block, delta int64) {
	live := blk.NewLoad(irtypes.I32, c.webLiveRegsGlobal)
	blk.NewStore(blk.NewAdd(live, constant.NewInt(irtypes.I32, delta)), c.webLiveRegsGlobal)
}

// defineSchedTimerInsertFunc emits @promise_sched_timer_insert(i8* node): link
// an armed node (deadline and g already stored) into the list, after every node
// with an equal or earlier deadline so that equal deadlines wake in the order
// they were armed, and count it as a live registration. The caller holds
// sched.timer_lock.
func (c *Compiler) defineSchedTimerInsertFunc() {
	nodeParam := ir.NewParam("node", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_sched_timer_insert", irtypes.Void, nodeParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	loop := fn.NewBlock("loop")
	check := fn.NewBlock("check")
	advance := fn.NewBlock("advance")
	link := fn.NewBlock("link")

	deadline := entry.NewLoad(irtypes.I64, timerNodeField(entry, nodeParam, timerFieldDeadline))
	headField := c.schedTimerField(entry, schedFieldTimerHead)
	entry.NewBr(loop)

	// loop: slot is the link that would point at the new node.
	slot := loop.NewPhi(ir.NewIncoming(headField, entry))
	cur := loop.NewLoad(irtypes.I8Ptr, slot)
	atEnd := loop.NewICmp(enum.IPredEQ, cur, constant.NewNull(irtypes.I8Ptr))
	loop.NewCondBr(atEnd, link, check)

	curDeadline := check.NewLoad(irtypes.I64, timerNodeField(check, cur, timerFieldDeadline))
	later := check.NewICmp(enum.IPredSGT, curDeadline, deadline)
	check.NewCondBr(later, link, advance)

	slot.Incs = append(slot.Incs, ir.NewIncoming(timerNodeField(advance, cur, timerFieldNext), advance))
	advance.NewBr(loop)

	link.NewStore(cur, timerNodeField(link, nodeParam, timerFieldNext))
	link.NewStore(nodeParam, slot)
	c.addLiveRegistrations(link, 1)
	link.NewRet(nil)

	c.funcs["promise_sched_timer_insert"] = fn
}

// defineSchedTimerExpireFunc emits @promise_sched_timer_expire() → i64: make
// every goroutine whose deadline has passed runnable, releasing its live
// registration. Returns 0 if it woke any, otherwise the nanoseconds until the
// earliest deadline (at least 1), or -1 if no timer is armed.
//
// A woken goroutine's node is unlinked before the goroutine is enqueued: the
// node lives in its frame and is reused or gone once it runs.
func (c *Compiler) defineSchedTimerExpireFunc() {
	fn := c.module.NewFunc("promise_sched_timer_expire", irtypes.I64)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	loop := fn.NewBlock("loop")
	check := fn.NewBlock("check")
	pop := fn.NewBlock("pop")
	notDue := fn.NewBlock("not_due")
	empty := fn.NewBlock("empty")

	lock := entry.NewLoad(irtypes.I8Ptr, c.schedTimerField(entry, schedFieldTimerLock))
	entry.NewCall(c.palMutexLock, lock)
	now := c.emitWasmMonotonicNanos(entry)
	entry.NewBr(loop)

	woke := loop.NewPhi(ir.NewIncoming(constant.False, entry))
	headField := c.schedTimerField(loop, schedFieldTimerHead)
	head := loop.NewLoad(irtypes.I8Ptr, headField)
	isEmpty := loop.NewICmp(enum.IPredEQ, head, constant.NewNull(irtypes.I8Ptr))
	loop.NewCondBr(isEmpty, empty, check)

	deadline := check.NewLoad(irtypes.I64, timerNodeField(check, head, timerFieldDeadline))
	due := check.NewICmp(enum.IPredSLE, deadline, now)
	check.NewCondBr(due, pop, notDue)

	next := pop.NewLoad(irtypes.I8Ptr, timerNodeField(pop, head, timerFieldNext))
	g := pop.NewLoad(irtypes.I8Ptr, timerNodeField(pop, head, timerFieldG))
	pop.NewStore(next, headField)
	c.addLiveRegistrations(pop, -1)
	pop.NewCall(c.funcs["promise_sched_enqueue"], g)
	woke.Incs = append(woke.Incs, ir.NewIncoming(constant.True, pop))
	pop.NewBr(loop)

	notDue.NewCall(c.palMutexUnlock, lock)
	wait := notDue.NewSub(deadline, now)
	notDue.NewRet(notDue.NewSelect(woke, constant.NewInt(irtypes.I64, 0), wait))

	empty.NewCall(c.palMutexUnlock, lock)
	empty.NewRet(empty.NewSelect(woke, constant.NewInt(irtypes.I64, 0), constant.NewInt(irtypes.I64, -1)))

	c.funcs["promise_sched_timer_expire"] = fn
}

// defineSchedTimerIdleFunc emits @promise_sched_timer_idle(i64 limit) → i8, the
// wait of a driver that owns the thread and has found nothing runnable. It
// wakes any sleeper already due; otherwise it waits on the monotonic clock
// until the earliest deadline or limit, whichever is first, and then wakes the
// sleepers that are due. Returns:
//   - 1 — it woke a sleeper, or reached limit;
//   - 2 — the per-test deadline (T0680) passed first: it sets the timed-out
//     flag, as promise_sched_coop_step does;
//   - 0 — no timer is armed and limit is timerNoLimit: there is nothing to
//     wait for.
func (c *Compiler) defineSchedTimerIdleFunc() {
	limitParam := ir.NewParam("limit", irtypes.I64)
	fn := c.module.NewFunc("promise_sched_timer_idle", irtypes.I8, limitParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	wokeBlk := fn.NewBlock("woke")
	readHead := fn.NewBlock("read_head")
	armed := fn.NewBlock("armed")
	pickTarget := fn.NewBlock("pick_target")
	nothing := fn.NewBlock("nothing")
	spin := fn.NewBlock("spin")
	checkTest := fn.NewBlock("check_test")
	cmpTest := fn.NewBlock("cmp_test")
	timedOut := fn.NewBlock("timed_out")
	reached := fn.NewBlock("reached")

	w := entry.NewCall(c.funcs["promise_sched_timer_expire"])
	isWoke := entry.NewICmp(enum.IPredEQ, w, constant.NewInt(irtypes.I64, 0))
	entry.NewCondBr(isWoke, wokeBlk, readHead)
	wokeBlk.NewRet(constant.NewInt(irtypes.I8, 1))

	// read_head: the earliest deadline, if any timer is armed.
	lock := readHead.NewLoad(irtypes.I8Ptr, c.schedTimerField(readHead, schedFieldTimerLock))
	readHead.NewCall(c.palMutexLock, lock)
	head := readHead.NewLoad(irtypes.I8Ptr, c.schedTimerField(readHead, schedFieldTimerHead))
	readHead.NewCall(c.palMutexUnlock, lock)
	none := readHead.NewICmp(enum.IPredEQ, head, constant.NewNull(irtypes.I8Ptr))
	readHead.NewCondBr(none, pickTarget, armed)

	earliest := armed.NewLoad(irtypes.I64, timerNodeField(armed, head, timerFieldDeadline))
	armed.NewBr(pickTarget)

	// pick_target: target = min(limit, earliest); nothing to wait for when no
	// timer is armed and the limit is unbounded.
	first := pickTarget.NewPhi(
		ir.NewIncoming(constant.NewInt(irtypes.I64, timerNoLimit), readHead),
		ir.NewIncoming(earliest, armed))
	limitFirst := pickTarget.NewICmp(enum.IPredSLT, limitParam, first)
	target := pickTarget.NewSelect(limitFirst, limitParam, first)
	unbounded := pickTarget.NewICmp(enum.IPredEQ, target, constant.NewInt(irtypes.I64, timerNoLimit))
	pickTarget.NewCondBr(unbounded, nothing, spin)
	nothing.NewRet(constant.NewInt(irtypes.I8, 0))

	now := c.emitWasmMonotonicNanos(spin)
	isReached := spin.NewICmp(enum.IPredSGE, now, target)
	spin.NewCondBr(isReached, reached, checkTest)

	testDeadline := checkTest.NewLoad(irtypes.I64, c.testDeadlineGlobal)
	testArmed := checkTest.NewICmp(enum.IPredNE, testDeadline, constant.NewInt(irtypes.I64, 0))
	checkTest.NewCondBr(testArmed, cmpTest, spin)

	testPassed := cmpTest.NewICmp(enum.IPredSGE, now, testDeadline)
	cmpTest.NewCondBr(testPassed, timedOut, spin)

	timedOut.NewStore(constant.NewInt(irtypes.I8, 1), c.testTimedOutGlobal)
	timedOut.NewRet(constant.NewInt(irtypes.I8, 2))

	reached.NewCall(c.funcs["promise_sched_timer_expire"])
	reached.NewRet(constant.NewInt(irtypes.I8, 1))

	c.funcs["promise_sched_timer_idle"] = fn
}

// defineSchedWaitStepFunc emits @promise_sched_wait_step(i64 limit) → i8, one
// step of a wait in code that cannot suspend — a sleep, or a channel, select,
// mutex or task wait in a plain function body. It runs one goroutine; when none
// is runnable, a sleeper may still become runnable, so it waits for the earliest
// timer, bounded by limit (promise_sched_timer_idle). Returns:
//   - 1 — progress: a goroutine ran, a sleeper woke, or limit was reached;
//     re-check what is waited for;
//   - 2 — the per-test deadline (T0680) passed: give up the wait;
//   - 0 — nothing runnable and no timer armed, with no limit: nothing can ever
//     make progress, so a wait still unmet is a deadlock.
//
// It is the one place that decides; emitWasmWaitStep emits a call to it.
func (c *Compiler) defineSchedWaitStepFunc() {
	limitParam := ir.NewParam("limit", irtypes.I64)
	fn := c.module.NewFunc("promise_sched_wait_step", irtypes.I8, limitParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	stepped := fn.NewBlock("stepped")
	idle := fn.NewBlock("idle")

	r := entry.NewCall(c.funcs["promise_sched_coop_step"])
	ran := entry.NewICmp(enum.IPredNE, r, constant.NewInt(irtypes.I8, 0))
	entry.NewCondBr(ran, stepped, idle)
	stepped.NewRet(r)

	idle.NewRet(idle.NewCall(c.funcs["promise_sched_timer_idle"], limitParam))

	c.funcs["promise_sched_wait_step"] = fn
}

// emitWasmWaitStep emits, in blk, one step of a WASM wait in a plain
// (non-coroutine) body and returns its i8 result, as promise_sched_wait_step
// defines it: 1 = re-check the wait, 2 = the per-test deadline passed, 0 =
// deadlock. Only wasm32-web arms timers; wasm32-wasi steps the scheduler
// directly, whose results mean the same.
func (c *Compiler) emitWasmWaitStep(blk *ir.Block) value.Value {
	if c.isWasmWeb {
		return blk.NewCall(c.funcs["promise_sched_wait_step"], constant.NewInt(irtypes.I64, timerNoLimit))
	}
	return blk.NewCall(c.funcs["promise_sched_coop_step"])
}

// defineSchedTimerSleepFunc emits @promise_sched_timer_sleep(i64 deadline), the
// sleep of code that cannot suspend (a plain function body, reached through a
// call no coroutine variant covers). It steps the wait (promise_sched_wait_step)
// until deadline, which also bounds each wait for a timer, and returns early
// once the per-test deadline has passed. That the wait step runs the scheduler
// is what marks every function that sleeps as blocking (suspendingFuncs), so
// each gets the coroutine variant in which sleep parks instead.
func (c *Compiler) defineSchedTimerSleepFunc() {
	deadlineParam := ir.NewParam("deadline", irtypes.I64)
	fn := c.module.NewFunc("promise_sched_timer_sleep", irtypes.Void, deadlineParam)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	loop := fn.NewBlock("loop")
	step := fn.NewBlock("step")
	done := fn.NewBlock("done")

	entry.NewBr(loop)

	now := c.emitWasmMonotonicNanos(loop)
	isDue := loop.NewICmp(enum.IPredSGE, now, deadlineParam)
	loop.NewCondBr(isDue, done, step)

	// With a limit the step never answers 0: it waits for the clock instead.
	r := step.NewCall(c.funcs["promise_sched_wait_step"], deadlineParam)
	timedOut := step.NewICmp(enum.IPredEQ, r, constant.NewInt(irtypes.I8, 2))
	step.NewCondBr(timedOut, done, loop)

	done.NewRet(nil)

	c.funcs["promise_sched_timer_sleep"] = fn
}

// defineSchedTimerAbandonFunc emits @promise_sched_timer_abandon(), which a
// batch-test binary calls before each test. promise_sched_coop_run waits for
// every armed timer, so the list is empty when a test ends — unless the test
// timed out, leaving its sleepers parked. Those goroutines are abandoned, as a
// timed-out test's goroutines are natively, and are never resumed: their nodes
// are dropped from the list (the frames that hold them stay allocated) and
// their live registrations released, so the next test does not wait out a
// sleep it never started.
func (c *Compiler) defineSchedTimerAbandonFunc() {
	fn := c.module.NewFunc("promise_sched_timer_abandon", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	loop := fn.NewBlock("loop")
	drop := fn.NewBlock("drop")
	done := fn.NewBlock("done")

	lock := entry.NewLoad(irtypes.I8Ptr, c.schedTimerField(entry, schedFieldTimerLock))
	entry.NewCall(c.palMutexLock, lock)
	headField := c.schedTimerField(entry, schedFieldTimerHead)
	entry.NewBr(loop)

	head := loop.NewLoad(irtypes.I8Ptr, headField)
	isEmpty := loop.NewICmp(enum.IPredEQ, head, constant.NewNull(irtypes.I8Ptr))
	loop.NewCondBr(isEmpty, done, drop)

	drop.NewStore(drop.NewLoad(irtypes.I8Ptr, timerNodeField(drop, head, timerFieldNext)), headField)
	c.addLiveRegistrations(drop, -1)
	drop.NewBr(loop)

	done.NewCall(c.palMutexUnlock, lock)
	done.NewRet(nil)

	c.funcs["promise_sched_timer_abandon"] = fn
}

// genWebParkTimer lowers a call to wasmweb.ParkTimerFunc — std's sleep on
// wasm32-web — inline, because the park must suspend the caller's own
// coroutine frame. nsArg is the duration as a raw i64. A duration of zero or
// less returns at once, as nanosleep does; a deadline past the clock's range
// saturates at timerLatestDeadline.
//
// In a coroutine the goroutine parks: its frame-resident node is linked into
// the timer list under sched.timer_lock, which it hands to the scheduler as its
// park mutex (released after the suspend completes, as every park does).
// Outside one it runs promise_sched_timer_sleep.
func (c *Compiler) genWebParkTimer(nsArg value.Value) {
	arm := c.newBlock("sleep.arm")
	cont := c.newBlock("sleep.cont")
	positive := c.block.NewICmp(enum.IPredSGT, nsArg, constant.NewInt(irtypes.I64, 0))
	c.block.NewCondBr(positive, arm, cont)

	c.block = arm
	now := c.emitWasmMonotonicNanos(c.block)
	sum := c.block.NewAdd(now, nsArg)
	latest := constant.NewInt(irtypes.I64, timerLatestDeadline)
	// sum < now: the add wrapped. sum > latest: it reached timerNoLimit.
	wrapped := c.block.NewICmp(enum.IPredSLT, sum, now)
	tooLate := c.block.NewICmp(enum.IPredSGT, sum, latest)
	deadline := c.block.NewSelect(c.block.NewOr(wrapped, tooLate), latest, sum)

	if !c.inCoroutine {
		c.block.NewCall(c.funcs["promise_sched_timer_sleep"], deadline)
		c.block.NewBr(cont)
		c.block = cont
		return
	}

	nodeTy := timerNodeType()
	node := c.createEntryAlloca(nodeTy)
	nodeRaw := c.block.NewBitCast(node, irtypes.I8Ptr)
	currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
	c.block.NewStore(deadline, timerNodeField(c.block, nodeRaw, timerFieldDeadline))
	c.block.NewStore(currentG, timerNodeField(c.block, nodeRaw, timerFieldG))

	lock := c.block.NewLoad(irtypes.I8Ptr, c.schedTimerField(c.block, schedFieldTimerLock))
	c.block.NewCall(c.palMutexLock, lock)
	c.block.NewCall(c.funcs["promise_sched_timer_insert"], nodeRaw)

	gTy := goroutineStructType()
	gPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTy))
	statusField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	c.block.NewStore(constant.NewInt(irtypes.I8, gStatusWaiting), statusField)
	parkMutexField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
	c.block.NewStore(lock, parkMutexField)

	suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
	resume := c.newBlock("sleep.resume")
	c.block.NewSwitch(suspResult, c.coroSuspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), resume),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

	// On resume the timer has expired: the scheduler already released
	// timer_lock (park mutex) and expire unlinked the node.
	c.block = resume
	c.block.NewBr(cont)
	c.block = cont
}
