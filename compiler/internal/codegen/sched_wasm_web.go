package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/promise-language/promise/compiler/internal/codegen/pal"
	"github.com/promise-language/promise/compiler/internal/wasmweb"
)

// This file implements the wasm32-web reactor top level: docs/web-apps.md
// §4 (bounded pumps), §4.1 (the liveness rule), §5 (re-entrancy), §13 (program
// lifetime). It is Phase 1 of §19; delivery (Phase 2, webLiveRegsGlobal's
// actual writers) lives in sched_wasm_web_delivery.go. A program with no live
// subscription still behaves exactly as it always did (§4.1's "no flag day"
// guarantee) — webLiveRegsGlobal only ever leaves 0 once something subscribes.
//
// wasm32-wasi is entirely unaffected: promise_sched_coop_run (sched.go) stays
// the run-to-completion driver for both @main (wasi) and every per-test batch
// execution on ANY wasm target (GenerateTestMain, compiler.go — tests are not
// reactors and always run to completion or per-test deadline). The functions
// here are only reachable from wrapMainWithScheduler's wasm32-web branch, i.e.
// only ordinary (non-test) wasm32-web program binaries.

// defineWebReactorGlobals declares the globals backing the reactor. Only
// called when c.isWasmWeb.
func (c *Compiler) defineWebReactorGlobals() {
	// @promise_web_live_registrations = global i32 0
	// The whole of the liveness rule (§4.1): zero means the program is an
	// ordinary command that exits on drain exactly as it does today, non-zero
	// means it is a reactor and an empty run queue is idle rather than
	// deadlock. Nothing increments this in Phase 1 — Phase 2's subscription
	// table does.
	liveRegs := c.module.NewGlobal(wasmweb.GlobalLiveRegistrations, irtypes.I32)
	liveRegs.Init = constant.NewInt(irtypes.I32, 0)
	c.webLiveRegsGlobal = liveRegs

	// @promise_web_in_pump = global i8 0
	// Re-entrancy guard (§5): nesting is refused, not supported.
	inPump := c.module.NewGlobal(wasmweb.GlobalInPump, irtypes.I8)
	inPump.Init = constant.NewInt(irtypes.I8, 0)
	c.webInPumpGlobal = inPump

	// @promise_web_terminated = global i8 0
	// Set once the instance has exited or faulted (§10); further host entries
	// return without running anything. pal_exit (emitted before this) defines
	// and sets it, so every exit path — a clean drain, an explicit exit, a
	// panic, a deadlock — marks the instance terminated.
	c.webTerminatedGlobal = pal.WebTerminatedGlobal(c.module)
}

// emitWebDrainVerdict ends a reactor drain that found nothing runnable (blk):
// it releases the re-entrancy guard (§5) and applies the liveness rule (§4.1).
// Live registrations gate the decision before main_done is consulted, so a
// reactor whose main() already returned while its consumers park on a live
// subscription is idle, not terminated. It returns the block reached when the
// instance is idle and the one reached when main is done with nothing live;
// the caller terminates both. With neither, the program is deadlocked and
// aborts with promise_sched_coop_run's message and exit code.
func (c *Compiler) emitWebDrainVerdict(fn *ir.Func, blk *ir.Block) (idle, done *ir.Block) {
	idle = fn.NewBlock("idle")
	checkMainDone := fn.NewBlock("check_main_done")
	done = fn.NewBlock("main_done")
	deadlock := fn.NewBlock("deadlock")

	blk.NewStore(constant.NewInt(irtypes.I8, 0), c.webInPumpGlobal)
	live := blk.NewLoad(irtypes.I32, c.webLiveRegsGlobal)
	isLive := blk.NewICmp(enum.IPredNE, live, constant.NewInt(irtypes.I32, 0))
	blk.NewCondBr(isLive, idle, checkMainDone)

	mdField := checkMainDone.NewGetElementPtr(schedStructType(), c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDone)))
	mainDone := checkMainDone.NewLoad(irtypes.I8, mdField)
	mainIsDone := checkMainDone.NewICmp(enum.IPredNE, mainDone, constant.NewInt(irtypes.I8, 0))
	checkMainDone.NewCondBr(mainIsDone, done, deadlock)

	msg := c.getTaskDeadlockMsgGlobal()
	msgPtr := deadlock.NewGetElementPtr(msg.ContentType, msg,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	deadlock.NewCall(c.palWrite, constant.NewInt(irtypes.I32, 2), msgPtr, constant.NewInt(irtypes.I64, 45))
	deadlock.NewCall(c.palExit, constant.NewInt(irtypes.I32, 2))
	deadlock.NewUnreachable()
	return idle, done
}

// defineWebReactorDrainFunc emits @promise_web_reactor_drain() → i8.
//
// This is the UNBOUNDED drain used exactly once, by wrapMainWithScheduler's
// wasm32-web branch, to run `main` (and whatever it spawns/registers) to the
// first point nothing is runnable. It is deliberately unbounded — not the
// budgeted pump of §4.2 — because §4 states plainly that "_initialize... pumps
// until nothing is runnable" versus "promise_web_pump... is bounded". Budgeting
// the very first drain would risk truncating existing long-running programs
// against a host that (pre-Phase-2) has no way to call promise_web_pump again,
// which would violate §4.1's "every existing test is bit-for-bit unaffected".
//
// Returns:
//   - 0 — drained with zero live registrations and main done: the caller
//     (_initialize, via emitWasmStart) must pal_exit(0). Identical outcome to
//     today's unconditional exit.
//   - 1 — the instance is a reactor (live registrations exist) and stays
//     alive: the caller must return instead of exiting.
//
// Deadlock (nothing runnable, zero live registrations, main not done) still
// aborts internally exactly as promise_sched_coop_run does today — it never
// returns.
//
// It is the outermost pump, so it holds the re-entrancy guard (§5) for its
// whole run: a host entry made while main runs — a Promise→JS call that
// dispatches an event synchronously — enqueues and returns without pumping,
// and this drain observes the event.
func (c *Compiler) defineWebReactorDrainFunc() {
	fn := c.module.NewFunc("promise_web_reactor_drain", irtypes.I8)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	loop := fn.NewBlock("loop")
	stoppedBlk := fn.NewBlock("stopped")

	entry.NewStore(constant.NewInt(irtypes.I8, 1), c.webInPumpGlobal)
	entry.NewBr(loop)

	// loop: run one cooperative step. stepR: 0 = nothing runnable, 1 = ran a G
	// (keep going), 2 = per-test deadline (cannot happen here in practice —
	// only GenerateTestMain arms the deadline, and it calls
	// promise_sched_coop_run directly, not this function — but treated
	// defensively the same as "stopped" rather than assumed unreachable).
	stepR := loop.NewCall(c.funcs["promise_sched_coop_step"])
	ranG := loop.NewICmp(enum.IPredEQ, stepR, constant.NewInt(irtypes.I8, 1))
	loop.NewCondBr(ranG, loop, stoppedBlk)

	// stopped: nothing runnable (or defensively, a deadline). Idle — the
	// reactor stays alive, so the caller (_initialize) must return, not exit;
	// main done with nothing live — the caller must pal_exit(0), same outcome
	// as today's unconditional exit.
	idle, done := c.emitWebDrainVerdict(fn, stoppedBlk)
	idle.NewRet(constant.NewInt(irtypes.I8, 1))
	done.NewRet(constant.NewInt(irtypes.I8, 0))

	c.funcs["promise_web_reactor_drain"] = fn
}

// defineWebPumpFunc emits the exported @promise_web_pump() → i32
// (wasmweb.ExportPump). This is the BOUNDED pump of §4.2 — what every JS
// callback calls after delivering an event (Phase 2 wires up callers; nothing
// calls this yet in Phase 1). It stops after wasmweb.StepBudget goroutine
// steps or wasmweb.TimeBudgetNanos of wall clock, whichever comes first
// (wall clock checked every wasmweb.TimeCheckInterval steps so the clock call
// is amortized), and asks the host to schedule a continuation
// (promise_env.schedule_pump) if work remains.
//
// Returns wasmweb.PumpIdle / PumpMore / PumpTerminated. No-op returning
// PumpTerminated immediately if the instance has already faulted (§10), and
// a no-op returning PumpMore immediately if a pump is already running on this
// stack (§5 — nesting is refused, not supported; the outer pump will observe
// any newly queued work on a subsequent iteration).
func (c *Compiler) defineWebPumpFunc() {
	fn := c.module.NewFunc(wasmweb.ExportPump, irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	terminatedRetBlk := fn.NewBlock("terminated_ret")
	checkInPumpBlk := fn.NewBlock("check_in_pump")
	nestedRetBlk := fn.NewBlock("nested_ret")
	startBlk := fn.NewBlock("start")
	loop := fn.NewBlock("loop")
	continueBlk := fn.NewBlock("continue")
	checkTimeBlk := fn.NewBlock("check_time")
	timeCheckBlk := fn.NewBlock("time_check")
	budgetExhaustedBlk := fn.NewBlock("budget_exhausted")
	drainedBlk := fn.NewBlock("drained")

	// entry: faulted instances no-op (§10).
	terminated := entry.NewLoad(irtypes.I8, c.webTerminatedGlobal)
	isTerminated := entry.NewICmp(enum.IPredNE, terminated, constant.NewInt(irtypes.I8, 0))
	entry.NewCondBr(isTerminated, terminatedRetBlk, checkInPumpBlk)
	terminatedRetBlk.NewRet(constant.NewInt(irtypes.I32, wasmweb.PumpTerminated))

	// check_in_pump: refuse nesting (§5).
	inPump := checkInPumpBlk.NewLoad(irtypes.I8, c.webInPumpGlobal)
	isNested := checkInPumpBlk.NewICmp(enum.IPredNE, inPump, constant.NewInt(irtypes.I8, 0))
	checkInPumpBlk.NewCondBr(isNested, nestedRetBlk, startBlk)
	nestedRetBlk.NewRet(constant.NewInt(irtypes.I32, wasmweb.PumpMore))

	// start: claim the guard, record the wall-clock start.
	startBlk.NewStore(constant.NewInt(irtypes.I8, 1), c.webInPumpGlobal)
	startNs := c.emitWasmMonotonicNanos(startBlk)
	startBlk.NewBr(loop)

	// loop: run one cooperative step (phi over the step counter).
	stepPhi := loop.NewPhi(ir.NewIncoming(constant.NewInt(irtypes.I64, 0), startBlk))
	stepR := loop.NewCall(c.funcs["promise_sched_coop_step"])
	isIdle := loop.NewICmp(enum.IPredEQ, stepR, constant.NewInt(irtypes.I8, 0))
	loop.NewCondBr(isIdle, drainedBlk, continueBlk)
	// stepR==2 (test deadline) also falls into continueBlk below and is
	// treated as budget exhaustion once the step counter is bumped; the test
	// harness's own coop_run path is unaffected (see defineWebReactorDrainFunc).

	// continue: bump the step counter, check the step budget.
	stepNext := continueBlk.NewAdd(stepPhi, constant.NewInt(irtypes.I64, 1))
	stepBudgetHit := continueBlk.NewICmp(enum.IPredSGE, stepNext, constant.NewInt(irtypes.I64, wasmweb.StepBudget))
	// A test-deadline step (stepR==2) also stops here regardless of the step
	// budget, so it does not spin the loop further.
	stepR2 := continueBlk.NewICmp(enum.IPredEQ, stepR, constant.NewInt(irtypes.I8, 2))
	stopNow := continueBlk.NewOr(stepBudgetHit, stepR2)
	continueBlk.NewCondBr(stopNow, budgetExhaustedBlk, checkTimeBlk)

	// check_time: only read the clock every TimeCheckInterval steps.
	rem := checkTimeBlk.NewSRem(stepNext, constant.NewInt(irtypes.I64, wasmweb.TimeCheckInterval))
	isCheckpoint := checkTimeBlk.NewICmp(enum.IPredEQ, rem, constant.NewInt(irtypes.I64, 0))
	checkTimeBlk.NewCondBr(isCheckpoint, timeCheckBlk, loop)
	stepPhi.Incs = append(stepPhi.Incs, ir.NewIncoming(stepNext, checkTimeBlk))

	// time_check: wall-clock budget.
	nowNs := c.emitWasmMonotonicNanos(timeCheckBlk)
	elapsed := timeCheckBlk.NewSub(nowNs, startNs)
	timeBudgetHit := timeCheckBlk.NewICmp(enum.IPredSGE, elapsed, constant.NewInt(irtypes.I64, wasmweb.TimeBudgetNanos))
	timeCheckBlk.NewCondBr(timeBudgetHit, budgetExhaustedBlk, loop)
	stepPhi.Incs = append(stepPhi.Incs, ir.NewIncoming(stepNext, timeCheckBlk))

	// budget_exhausted: work remains — ask the host to schedule a
	// continuation and return (§4.2, default kind: ASAP).
	scheduleFn := c.getOrDeclareWasmImport(wasmweb.SchedulePumpSymbol,
		wasmweb.ImportModule, wasmweb.SchedulePumpImport, irtypes.Void,
		ir.NewParam("kind", irtypes.I32), ir.NewParam("delay_ms", irtypes.I32))
	budgetExhaustedBlk.NewCall(scheduleFn,
		constant.NewInt(irtypes.I32, wasmweb.SchedulePumpASAP), constant.NewInt(irtypes.I32, 0))
	budgetExhaustedBlk.NewStore(constant.NewInt(irtypes.I8, 0), c.webInPumpGlobal)
	budgetExhaustedBlk.NewRet(constant.NewInt(irtypes.I32, wasmweb.PumpMore))

	// drained: nothing runnable. Idle — the reactor stays alive, nothing to
	// do until the next host entry. Main done with nothing live — the reactor
	// has drained with nothing left that could ever wake it (§13 — "a program
	// with no live registrations terminates on drain, exactly as today"), so
	// exit(0); pal_exit marks the instance terminated.
	idle, done := c.emitWebDrainVerdict(fn, drainedBlk)
	idle.NewRet(constant.NewInt(irtypes.I32, wasmweb.PumpIdle))
	done.NewCall(c.palExit, constant.NewInt(irtypes.I32, 0))
	done.NewUnreachable()

	c.funcs[wasmweb.ExportPump] = fn
}
