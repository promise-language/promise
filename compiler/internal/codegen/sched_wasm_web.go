package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/promise-language/promise/compiler/internal/wasmweb"
)

// This file implements the wasm32-web reactor top level: docs/wasm-web-callbacks.md
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
	// return without running anything. Nothing sets this to 1 yet in Phase 1
	// except the drain-to-clean-termination path itself, immediately before
	// pal_exit.
	terminated := c.module.NewGlobal(wasmweb.GlobalTerminated, irtypes.I8)
	terminated.Init = constant.NewInt(irtypes.I8, 0)
	c.webTerminatedGlobal = terminated
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
func (c *Compiler) defineWebReactorDrainFunc() {
	fn := c.module.NewFunc("promise_web_reactor_drain", irtypes.I8)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	schedTy := schedStructType()

	entry := fn.NewBlock(".entry")
	loop := fn.NewBlock("loop")
	stoppedBlk := fn.NewBlock("stopped")
	liveBlk := fn.NewBlock("live")
	checkMainDoneBlk := fn.NewBlock("check_main_done")
	terminateCleanBlk := fn.NewBlock("terminate_clean")
	deadlockBlk := fn.NewBlock("deadlock")

	entry.NewBr(loop)

	// loop: run one cooperative step. stepR: 0 = nothing runnable, 1 = ran a G
	// (keep going), 2 = per-test deadline (cannot happen here in practice —
	// only GenerateTestMain arms the deadline, and it calls
	// promise_sched_coop_run directly, not this function — but treated
	// defensively the same as "stopped" rather than assumed unreachable).
	stepR := loop.NewCall(c.funcs["promise_sched_coop_step"])
	ranG := loop.NewICmp(enum.IPredEQ, stepR, constant.NewInt(irtypes.I8, 1))
	loop.NewCondBr(ranG, loop, stoppedBlk)

	// stopped: nothing runnable (or defensively, a deadline). Apply the
	// liveness rule (§4.1) — live registrations gate the decision before
	// main_done is even consulted, so a reactor whose main() already
	// returned but whose consumer goroutines are parked on a live
	// subscription is correctly reported idle, not terminated.
	live := stoppedBlk.NewLoad(irtypes.I32, c.webLiveRegsGlobal)
	isLive := stoppedBlk.NewICmp(enum.IPredNE, live, constant.NewInt(irtypes.I32, 0))
	stoppedBlk.NewCondBr(isLive, liveBlk, checkMainDoneBlk)

	// live: reactor stays alive — caller (_initialize) must return, not exit.
	liveBlk.NewRet(constant.NewInt(irtypes.I8, 1))

	// check_main_done: zero live registrations — either clean completion
	// (identical to today) or a genuine deadlock (identical to today).
	mdField := checkMainDoneBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDone)))
	mainDone := checkMainDoneBlk.NewLoad(irtypes.I8, mdField)
	mainIsDone := checkMainDoneBlk.NewICmp(enum.IPredNE, mainDone, constant.NewInt(irtypes.I8, 0))
	checkMainDoneBlk.NewCondBr(mainIsDone, terminateCleanBlk, deadlockBlk)

	// terminate_clean: main completed, no live registrations — caller must
	// pal_exit(0), same outcome as today's unconditional exit.
	terminateCleanBlk.NewRet(constant.NewInt(irtypes.I8, 0))

	// deadlock: terminal — write message to stderr and exit(2). Same message
	// and exit code as promise_sched_coop_run's deadlock path; a distinct
	// .rodata global because both functions are emitted in the same module
	// on wasm32-web.
	deadlockMsg := constant.NewCharArrayFromString("fatal: all goroutines are asleep - deadlock!\n")
	deadlockGlobal := c.module.NewGlobalDef(".str.web_reactor_deadlock", deadlockMsg)
	deadlockGlobal.Immutable = true
	deadlockGlobal.Linkage = enum.LinkagePrivate
	deadlockPtr := deadlockBlk.NewGetElementPtr(deadlockGlobal.ContentType, deadlockGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	deadlockBlk.NewCall(c.palWrite, constant.NewInt(irtypes.I32, 2), deadlockPtr, constant.NewInt(irtypes.I64, 45))
	deadlockBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 2))
	deadlockBlk.NewUnreachable()

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

	schedTy := schedStructType()

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
	idleRetBlk := fn.NewBlock("idle_ret")
	checkMainDoneBlk := fn.NewBlock("check_main_done")
	terminateCleanBlk := fn.NewBlock("terminate_clean")
	deadlockBlk := fn.NewBlock("deadlock")

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

	// drained: nothing runnable — apply the liveness rule (§4.1), same
	// decision as defineWebReactorDrainFunc's "stopped" block.
	live := drainedBlk.NewLoad(irtypes.I32, c.webLiveRegsGlobal)
	isLive := drainedBlk.NewICmp(enum.IPredNE, live, constant.NewInt(irtypes.I32, 0))
	drainedBlk.NewCondBr(isLive, idleRetBlk, checkMainDoneBlk)

	// idle_ret: reactor stays alive, nothing to do until the next host entry.
	idleRetBlk.NewStore(constant.NewInt(irtypes.I8, 0), c.webInPumpGlobal)
	idleRetBlk.NewRet(constant.NewInt(irtypes.I32, wasmweb.PumpIdle))

	// check_main_done: zero live registrations.
	mdField := checkMainDoneBlk.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDone)))
	mainDone := checkMainDoneBlk.NewLoad(irtypes.I8, mdField)
	mainIsDone := checkMainDoneBlk.NewICmp(enum.IPredNE, mainDone, constant.NewInt(irtypes.I8, 0))
	checkMainDoneBlk.NewCondBr(mainIsDone, terminateCleanBlk, deadlockBlk)

	// terminate_clean: the reactor has drained with nothing left that could
	// ever wake it (§13 — "a program with no live registrations terminates on
	// drain, exactly as today"). Mark terminated so any further host entry
	// (racing with pal_exit, which does not return) is a safe no-op, then
	// exit(0).
	terminateCleanBlk.NewStore(constant.NewInt(irtypes.I8, 1), c.webTerminatedGlobal)
	terminateCleanBlk.NewStore(constant.NewInt(irtypes.I8, 0), c.webInPumpGlobal)
	terminateCleanBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 0))
	terminateCleanBlk.NewUnreachable()

	// deadlock: nothing runnable, zero live registrations, main not done.
	deadlockMsg := constant.NewCharArrayFromString("fatal: all goroutines are asleep - deadlock!\n")
	deadlockGlobal := c.module.NewGlobalDef(".str.web_pump_deadlock", deadlockMsg)
	deadlockGlobal.Immutable = true
	deadlockGlobal.Linkage = enum.LinkagePrivate
	deadlockPtr := deadlockBlk.NewGetElementPtr(deadlockGlobal.ContentType, deadlockGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	deadlockBlk.NewCall(c.palWrite, constant.NewInt(irtypes.I32, 2), deadlockPtr, constant.NewInt(irtypes.I64, 45))
	deadlockBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 2))
	deadlockBlk.NewUnreachable()

	c.funcs[wasmweb.ExportPump] = fn
}
