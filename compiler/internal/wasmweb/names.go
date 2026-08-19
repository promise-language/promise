// Package wasmweb holds the single definition of every symbol name shared
// across the wasm32-web host boundary.
//
// Three separate generators have to agree on these strings: the compiler
// backend (internal/codegen) emits the exports and the import, while the
// binding generators emit Promise source (internal/bindgen/codegen.go) and the
// JavaScript that calls them (internal/bindgen/jsglue.go). GitHub #10 was
// caused by exactly that kind of split — two generators independently invented
// a name for the same logical thing and drifted apart — so the names live here
// and nowhere else.
//
// The package is deliberately tiny and dependency-free so both the backend and
// the binding generators can import it without a cycle.
//
// See docs/wasm-web-callbacks.md §14.
package wasmweb

// ImportModule is the WASM import module name the JS glue supplies. Every
// promise_env.* import the runtime declares uses this as its module.
const ImportModule = "promise_env"

// Exports called by the host (JS → WASM). Both are registered with the linker
// via --export= in buildWasmLinkArgs; adding a name here is not enough on its
// own.
const (
	// ExportPump drains the scheduler within a budget and returns.
	//
	//	promise_web_pump() -> i32
	//
	// Returns PumpIdle, PumpMore, or PumpTerminated. It is a no-op returning
	// PumpMore when a pump is already running on this stack (§5: nesting is
	// refused, never nested) and PumpTerminated once the instance has exited.
	ExportPump = "promise_web_pump"

	// ExportEnqueue delivers one event to a subscription.
	//
	//	promise_web_enqueue(sub_id: i32, handle: i32) -> i32
	//
	// It never runs Promise code and never allocates on the Promise heap, so
	// it is safe to call in any context — including from inside a Promise→JS
	// call that is itself nested inside a pump. Returns EnqueueOK or
	// EnqueueDropped.
	ExportEnqueue = "promise_web_enqueue"
)

// Pump return codes (ExportPump).
const (
	PumpIdle       = 0 // nothing runnable; instance is idle and waiting on the host
	PumpMore       = 1 // work remains; a continuation has been scheduled
	PumpTerminated = 2 // the instance has exited; further pumps do nothing
)

// Enqueue return codes (ExportEnqueue).
const (
	EnqueueOK      = 0 // queued
	EnqueueDropped = 1 // dropped per the subscription's overflow policy
)

// SchedulePumpImport is the host function the runtime calls to schedule its own
// continuation when a pump exhausts its budget with work still pending.
//
//	promise_env.schedule_pump(kind: i32, delay_ms: i32) -> void
//
// SchedulePumpSymbol is the corresponding LLVM symbol; the wasm-import-module /
// wasm-import-name attribute pair maps it onto ImportModule.SchedulePumpImport.
const (
	SchedulePumpImport = "schedule_pump"
	SchedulePumpSymbol = "promise_env_schedule_pump"
)

// schedule_pump kinds. The host picks the mechanism; the guest picks the kind.
const (
	// SchedulePumpASAP continues as soon as possible without the ~4ms clamp
	// setTimeout(0) is subject to — a MessageChannel postMessage in practice.
	// This is the default for ordinary continuation.
	SchedulePumpASAP = 0
	// SchedulePumpTimer continues after delay_ms via setTimeout.
	SchedulePumpTimer = 1
	// SchedulePumpFrame continues before the next paint via
	// requestAnimationFrame. Never the default: it ties progress to paint, so
	// background work would be throttled to the display refresh rate and stop
	// entirely in a hidden tab.
	SchedulePumpFrame = 2
)

// Pump budget. A pump stops after StepBudget goroutine steps or TimeBudgetNanos
// of wall clock, whichever comes first, and schedules a continuation if work
// remains. The clock is only read every TimeCheckInterval steps so the cost is
// amortized.
//
// With no preemption on WASM these bound work only *between* goroutine steps: a
// single handler that runs long still blocks the page, and nothing in the
// runtime can stop it (§6).
const (
	StepBudget        = 4096
	TimeBudgetNanos   = 4_000_000 // 4ms
	TimeCheckInterval = 64
)

// Runtime globals backing the reactor. Named here because the IR-shape tests
// and the binding generators both refer to them.
const (
	// GlobalLiveRegistrations counts live host registrations — subscriptions,
	// armed timers, pending host operations. It is the whole of the liveness
	// rule (§4.1): zero means the program is an ordinary command that exits on
	// drain exactly as it does today, non-zero means it is a reactor and an
	// empty run queue is idle rather than deadlock.
	GlobalLiveRegistrations = "promise_web_live_registrations"

	// GlobalInPump is the re-entrancy guard (§5).
	GlobalInPump = "promise_web_in_pump"

	// GlobalTerminated is set once the instance has exited or faulted; further
	// host entries return without running anything.
	GlobalTerminated = "promise_web_terminated"

	// GlobalExitCode holds main's return code until the liveness check decides
	// whether to exit with it.
	GlobalExitCode = "promise_web_exit_code"
)
