# Host Callbacks and the `wasm32-web` Reactor Execution Model

> **Status: Design proposal.** No implementation yet. This document decides how a
> browser (or any host that drives the module) calls *into* Promise code on
> `wasm32-web`, and what that requires of the top-level execution model. It is a
> prerequisite for WebIDL `callback` support — see
> [promise-language/promise#25](https://github.com/promise-language/promise/issues/25)
> — but nothing here is WebIDL-specific or bindgen-specific.
>
> Related: [wasm-bindings.md](wasm-bindings.md) owns the IDL→bindings story and
> the `promise bind` pipeline; [runtime-architecture.md](runtime-architecture.md)
> owns the PAL and the M:N scheduler this builds on.

## 1. Problem

Every WASM export the compiler emits today goes one direction: Promise calling
out to the host. There is no way for the host to call in.

Verified against a `wasm32-web` build of a `print_line("hello")` program
(`WebAssembly.Module.exports()`):

```
memory (memory), _initialize (function), cabi_realloc (function), __cabi_retarea (global)
```

That set is fixed. It does not grow with the program — no user function, no
closure, nothing Promise-defined is reachable from JS. So `addEventListener`,
timers, resolved `fetch` promises, and every other host→guest callback are all
unimplementable today, not just WebIDL `callback` types.

The obvious framing — "add an export so JS can invoke a Promise closure" — is
wrong, and the rest of this document is why. A closure answers *what runs*, not
*who runs it*. A closure invoked directly from a browser event handler enters
WASM on the browser's call stack with no G, no P, no scope-cleanup context, and
no ability to park. It cannot behave like Promise code because it is not running
as Promise code. The real work is in the execution model.

## 2. What already works

Two things are already in place and are not the problem.

**Closures.** First-class function types (`(A, B) -> C`), `LambdaExpr`, capture
analysis, and closure codegen with heap-allocated env structs all work today,
on every target. The callback *value* needs no new language work.

**Goroutines on WASM.** Green threads multiplexed onto the single physical
thread are fully functional. Verified:

```
main() `test(expected: "in goroutine\ngot 42\n") {
  t := go { print_line("in goroutine"); 42 };
  v := <-t;
  print_line("got " + v.to_string());
}
```
```
PASS (1.146s) [wasm32-web]
```

G/P structures, run queues, channels, `select`, and park/wake are the same code
as native. Goroutines are LLVM coroutines exactly as they are natively. The
scheduler initializes with one P (`promise_sched_init(1)`, `sched.go:2229`) and
`main` calls `promise_sched_coop_run` instead of the native
`run_until_main` + `shutdown` pair (`sched.go:2471-2477`).

Only the M layer differs:

- M structs are allocated and paired with the P, but **no OS thread is created** —
  the thread-creation phase is skipped on WASM (`sched.go:587-590`) and sysmon
  never starts.
- Atomic RMWs degrade to plain load/store; the G/P/M globals are not
  thread-local.
- Work stealing is compiled but always returns null with a single P.
- **Nothing sets `G.preempt`**, so there is no preemption in practice — see §6,
  where this turns out to be a missing flag-setter rather than a missing
  mechanism.

The substrate to *run* handlers therefore exists. What is missing is a way for
the host to hand work to it, and a top level that can survive being called more
than once.

## 3. The constraint: the browser owns the thread

In a page, WASM is never in charge of the thread. The browser calls in on its
main thread, and that call must return promptly; if it does not, rendering and
input stall and the user gets a "page unresponsive" dialog. Every later callback
— an event listener, a timer, an animation frame, a resolved JS promise — is
another call on that same thread under the same obligation.

Today's `wasm32-web` build assumes the exact opposite, in two places.

**`_initialize` assumes one call contains the whole program.** It calls
`main(0, null)`, then `pal_exit(code)`, then `unreachable`
(`wasm_libc.go:100-117`). The entry point is designed never to return.

**"Nothing runnable" is treated as a fatal bug.** `promise_sched_coop_run`
(`sched.go:2710-2758`) loops on `promise_sched_coop_step`; when no G is
runnable it exits only if `main_done`, and otherwise aborts. Verified:

```
main() `test(expected: "before recv\n") {
  ch := channel[int](0);
  print_line("before recv");
  v := <-ch;
  print_line("never");
}
```
```
actual: before recv | fatal: all goroutines are asleep - deadlock!
exit:   exit status 2
```

Both behaviours are correct for a command-line program and wrong for a page,
where "nothing runnable right now" is the state the program spends most of its
life in.

A browser event is not a second thread and never will be: it is a fresh
synchronous entry into a single-threaded instance. **The hazard to design
against is re-entrancy, not data races.**

## 4. Execution model: a reactor driven by bounded pumps

`wasm32-web` builds as a reactor. The module exposes a small fixed set of entry
points; the host calls them; each does a bounded amount of work and returns.

- **`_initialize`** initializes the runtime, runs `main` (which registers
  whatever handlers the app wants and spawns whatever goroutines it wants), and
  pumps until nothing is runnable.
- **`promise_web_pump`** is what every JS callback calls after delivering an
  event. It pumps and returns.
- **The pump is bounded** (§4.2). If work remains when the budget is spent, it
  asks the host to schedule another pump before returning.
- **"Nothing runnable" is the idle state**, not a deadlock — but only for
  programs that are actually reactors (§4.1). The instance stays alive with its
  heap, registrations, and parked goroutines intact.
- **`main` returning does not end the program.** Termination becomes explicit.

`wasm32-wasi` is unaffected: it keeps `_start`, run-to-completion, and the
deadlock abort exactly as today. This document is `wasm32-web` only.

### 4.1 The liveness rule (and why there is no flag day)

The one decision that keeps this from being a breaking change:

> **An instance stays alive after a drain if and only if it holds at least one
> live host registration** — an event subscription, an armed timer, or a pending
> host operation. Otherwise the program is complete and exits exactly as it does
> today.

So `_initialize` ends with:

```
drain to idle
if live_registrations == 0:
    pal_exit(exit_code)      # identical to today's behaviour
else:
    return                   # reactor: the host will call back in
```

Consequences worth stating plainly:

- **Every existing `wasm32-web` program and test is bit-for-bit unaffected.** A
  program that never registers anything never has a live registration, so it
  exits on drain as it always has. The Node harness keeps getting its exit code.
- **A program becomes a reactor by registering something.** There is no build
  flag, no annotation, no separate "web app" mode. This satisfies "no hidden
  effects": the behaviour follows visibly from what the code does.
- **Deadlock detection survives where it is meaningful.** With zero live
  registrations, "nothing runnable and main not done" is still a genuine
  deadlock and still aborts. It is only reinterpreted as idle when a
  registration exists that could plausibly deliver the wakeup.

This is Node's model — ref'd handles keep the loop alive — and it is the reason
the cost of this change is far smaller than "rewrite the top level".

We do lose one thing: for a *reactor* program, a goroutine waiting on an event
that will never arrive is indistinguishable from one waiting on an event that
will. That is inherent — the host, not the runtime, knows whether a click is
coming. Mitigation is diagnostic, not semantic: an idle drain that leaves
goroutines parked can be reported (behind a debug flag) as "idle: N goroutines
parked, M live registrations", which is the information a developer actually
needs.

### 4.2 Budget and continuation

The pump runs `promise_sched_coop_step` until one of:

1. It returns 0 — nothing runnable. The pump is done; report idle.
2. A **step budget** is exhausted (default 4096 steps).
3. A **wall-clock budget** is exhausted (default 4 ms), checked every 64 steps so
   the clock call is amortized.

`coop_step` is the right granularity to build on: it already returns 0/1/2 for
nothing-runnable / ran-a-G / deadline-reached, and it is already documented as
re-entrant-caller-aware (T0668) — it saves and restores the incoming TLS
`current_g`/`current_p` on every return path. The wall-clock check can reuse the
existing per-test deadline machinery (`c.testDeadlineGlobal`,
`emitWasmMonotonicNanos`), which already implements exactly this pattern.

On budget exhaustion with work remaining, the pump calls a host import to
schedule its own continuation and returns:

```
promise_env.schedule_pump(kind: i32, delay_ms: i32)
```

| `kind` | Host mechanism | Use |
|---|---|---|
| 0 | `MessageChannel` postMessage | Default. Continue as soon as possible without the 4 ms `setTimeout` clamp. |
| 1 | `setTimeout(delay_ms)` | Timers, deliberate delay, backoff. |
| 2 | `requestAnimationFrame` | Work that should land before the next paint. |

Default is kind 0. `requestAnimationFrame` is not the default: it ties progress
to paint, so a background computation would be throttled to 60 Hz and stop
entirely in a hidden tab.

### 4.3 Idle is not deadlock

When the pump drains to nothing-runnable and live registrations exist, it
returns "idle" and the instance simply sits there. No abort, no exit, no
message. The heap, the registered subscriptions, the parked goroutines and the
event queues all persist to the next host entry.

## 5. Re-entrancy

The browser can dispatch an event synchronously from inside a handler —
`element.click()` called from within a click handler is the canonical case.
Without a rule, the pump would nest: a second `coop_step` loop inside a
goroutine that is itself mid-step.

**Decision: nesting is refused, not supported.** The pump entry holds an
`in_pump` flag. If a host entry arrives while the flag is set, the event is
enqueued and the entry returns *immediately without pumping*. The outer pump,
still running, will observe the newly queued event on a subsequent iteration.

This makes nesting unrepresentable rather than merely discouraged, costs one
load and one branch, and needs no re-entrancy reasoning anywhere else in the
design. It is also why delivery and pumping are two separate exports (§9):
enqueueing must remain callable in any context, including from inside a
Promise→JS call that is itself nested inside a pump.

`coop_step`'s existing re-entrancy handling stays as it is — it serves the
legacy `Task[T].drop` spin path, which is a different caller and unaffected.

## 6. Preemption on WASM

The review of #25 states that with no sysmon a long handler freezes the page and
"nothing in the runtime can stop it". That is true today but overstates the
limit: **the yield check is already compiled in on WASM; only the flag-setter is
missing.**

`emitYieldCheck` (`stmt_loop.go:408`) emits an inline check of `G.preempt` at
every loop back-edge whenever `c.inCoroutine` — there is no `isWasm` guard. On
WASM that check runs and always sees 0, because sysmon (the only writer) never
starts.

So software preemption is available for the cost of a writer. The cheap form is
a budget counter rather than a clock: have the WASM yield check decrement a
global and yield when it reaches zero, which is one load/store/branch per
back-edge — the same order of cost as the check already emitted. When a
goroutine yields at a back-edge, the pump regains control *between* steps, can
observe its budget is spent, schedule a continuation, and return to the browser
with the goroutine re-enqueued. The page stays responsive.

This covers handlers that loop, which is the overwhelming majority of the
freezing cases in practice. It does **not** cover a long straight-line
computation or a long-running native call, which contain no back-edge; those
remain a documented contract, not an enforced one.

**Decision: not in the first implementation.** It changes hot-loop codegen for
all WASM programs and deserves its own benchmark and its own review. The v1
contract is "a single handler that runs long freezes the page"; this is the
designed path to lifting it, recorded here so that v1 does not foreclose it.
Nothing in §4 depends on which choice is made.

## 7. Delivery: JS enqueues, Promise code consumes

Signal handling is the precedent, and it already solves the same shape of
problem — an event arriving in a foreign context that must be handled by
ordinary user code.

- `promise_signal_handler` runs in the foreign context and does the minimum:
  one `write` to a pipe. It never runs user code, never allocates, never touches
  the scheduler.
- The handler *logic* runs on a normal goroutine blocked in `receive_signal()` —
  a plain pipe read wrapped in `enter_syscall`/`exit_syscall`. Full ownership and
  drop semantics; nothing special about the context.
- The pipe decouples delivery from handling, and registration is explicit.

**The browser analogue: the JS event listener is the signal handler.** It stores
the event in the existing `_refs` handle table, pushes `(subscription_id,
handle)` onto a queue, calls the pump, and returns. It never calls a Promise
closure directly.

This is what keeps the export surface to two fixed-signature entries instead of
a generated trampoline per callback signature — and therefore removes almost all
of the generated naming that #25's Bug-1 lesson warns about. There is very
little left that *can* drift (§9).

`net`'s reactor is not a reusable precedent here: it is explicitly skipped on
WASM because it needs a dedicated poller thread. In the browser the host *is*
the poller, which is why the signal shape fits and the netpoll shape does not.

## 8. The delivery surface: event channels

This is the highest-leverage API decision in the design. Two candidates:

```
// (a) registration
main() {
  web.on(canvas, "mousedown", (MouseEvent e) { ... });
}

// (b) event channel
main() {
  events := web.events(canvas, ["mousedown", "mousemove"]);
  for e in events { dispatch(e); }
}
```

**Decision: (b) is the Layer 1 primitive. (a) is sugar implemented on top of
it.**

The case for the channel is that four separate problems stop being event-system
problems and become uses of machinery the language already has:

- **Buffering** is channel capacity.
- **Backpressure** is channel semantics.
- **Composition** with timers, network I/O, and app-internal channels is
  `select`, for free.
- **Shutdown** is channel close, with its existing well-defined semantics.

None of this is aspirational: `for v in ch { ... }` over a `Channel[T]` already
compiles, runs, and terminates on `close()` today, on every target. The
`for e in events` loop is existing language machinery, not something this design
has to build.

The fourth point is the one that matters most, and it is worth spelling out
because it dissolves the classic use-after-free in every event system.

> **In the channel model, no Promise closure is ever reachable from JS.** The JS
> side holds only an integer subscription id. The closure — if there is one at
> all — is owned by the consuming goroutine on the Promise side.

So "unregister the currently running handler", the case that requires a stated
rule in every registration-based design, cannot corrupt anything here: closing a
subscription removes the JS listener and closes the channel; a handler that is
mid-execution is ordinary Promise code on an ordinary goroutine, holding
ordinary references, and it finishes normally. The closure and its captured env
cannot be freed under a live frame because JS never had a reference to free.

In (b) the loop is *logical*: it reads like a loop but is spread across many
host entries, holding the browser's thread for none of them. The consuming
goroutine parks when the channel is empty, the pump finds nothing runnable, and
the WASM call returns.

`web.on` remains available as sugar for the common single-event case, defined
as: subscribe, spawn a goroutine that consumes the channel and calls the closure
per event. Because it is defined in terms of (b), it inherits (b)'s lifetime
properties instead of introducing its own.

### 8.1 Queue semantics and overflow

The host push happens outside any G and must never block, so "channel full"
needs a defined behaviour. Growing without bound is not acceptable: a
`mousemove` flood would consume memory until the tab dies.

**Decision: bounded queue with an explicit per-subscription policy, and a
visible drop count.**

| Policy | Behaviour | For |
|---|---|---|
| `DropOldest` (default) | Evict the oldest queued event | Positional/state events — the newest is the truth |
| `DropNewest` | Reject the arriving event | When early events matter more than late ones |
| `Coalesce` | Merge into the newest queued event of the same kind | `mousemove`, `resize`, `scroll` |

Default capacity 256 with `DropOldest`. Drops are counted per subscription and
readable (`subscription.dropped`) — a silent drop that no one can observe is the
failure mode to avoid, and 256 buffered clicks already means the app is broken.
`Coalesce` is opt-in because it is right for `mousemove` and wrong for `click`;
the runtime cannot infer which.

Ordering is FIFO per subscription. There is no global ordering guarantee across
subscriptions, and the design does not pretend to offer one.

## 9. Dispatch policy

Within the loop, the remaining choice is how each event is handled:

```
for e in events {
  match e.kind {
    MouseMove => dispatch(e),             // inline: cheap, strictly ordered
    Click     => { go { dispatch(e); } }  // own G: may block without stalling delivery
  }
}
```

**Inline is the default** — cheapest, strictly ordered, least to reason about.
Handing an event to its own G buys exactly one thing: a handler that blocks no
longer delays the events behind it. Both are supported, selectable per event
type, and the guarantees differ and must be stated rather than discovered:

- **Inline**: handlers run to completion in arrival order.
- **Spawned G**: *start* order only. Two invocations of the same handler can
  interleave at suspend points.

Unbounded per-event spawn is the weakest form of this — one slow handler at a
high event rate grows Gs without bound, each pinning an event handle. A bounded
consumer pool is the recommended shape when concurrency is actually wanted.

Note the interaction with unsubscription: a spawned G can still be mid-handler
after the loop has moved on, so "unsubscribed" means "no new events will be
delivered", not "no handler is running". Callers that need the stronger property
join their consumers.

## 10. Failure inside a handler

There is no enclosing Promise frame to propagate to — the logical caller is the
browser.

- **Raised errors are unaffected.** They are values, and the failability rules
  already force a handler to deal with its own. A subscription loop is ordinary
  code; the compiler already rejects an unhandled `!` in it.
- **A panic terminates the instance**, the same way it terminates a native
  program, with the message routed to `console.error` before the instance is
  marked faulted and pumping stops. Continuing to pump after a panic means
  running further handlers over a heap the runtime has already declared
  inconsistent, which converts one reproducible crash into an unbounded number
  of unreproducible ones.

Subsequent host entries into a faulted instance return immediately without
pumping.

## 11. Blocking that the host must resolve

Any wait a handler performs must be satisfiable by some *future host entry* — an
event, a timer, a resolved fetch. A wait that could only be satisfied by
continuing to run right now cannot be satisfied at all, because the pump has to
return.

In practice this is not a new restriction so much as a new failure mode for an
old one: it is the same discipline as "don't block the event loop" in JS, and
the same operations are involved. The channel surface makes the legitimate cases
natural (`select` across events, timers, and network) and gives the illegitimate
ones nowhere to hide, since there is no synchronous host call to reach for.

Per §4.1, a wait that no host entry will ever satisfy is a silent idle rather
than a reported deadlock, with the debug-flag diagnostic as the mitigation.

## 12. Resource lifetime across the boundary

Event objects are resources and reuse the existing machinery unchanged: the JS
listener calls `_refStore(event)` to get a handle, and the handle travels in the
queue entry. On the Promise side it is wrapped as the generated resource type,
and the existing `[resource-drop]` path releases the JS-side reference when the
value is dropped.

Two cases follow from that and need no new mechanism:

- **Events queued at close.** Closing a subscription drops the channel, which
  drops its queued elements, which releases their handles. No leak, no special
  case.
- **A DOM element removed while its events are in flight.** The handle keeps the
  JS object alive until the event value is dropped, which is the correct
  behaviour — the handler sees the event it was sent, and the object becomes
  collectable afterwards.

## 13. Program lifetime and termination

- **`main` returning does not terminate a reactor program.** It means setup is
  finished.
- **Termination is explicit:** `web.terminate(code)` closes all subscriptions,
  runs the normal drop paths, and calls `pal_exit(code)`.
- **A program with no live registrations terminates on drain**, exactly as today
  (§4.1).

**Leak accounting** is evaluated at termination, which is where it is evaluated
today — `pal_exit` is reached in both the command-style and the explicit-
`terminate` paths, so the existing check applies unchanged to both. A reactor
that never terminates is never checked, which is correct: parked goroutines and
queued events are live data, not leaks. Browser integration tests therefore
drive their scenario and then call `web.terminate(0)`, which is what makes the
zero-leak policy enforceable for this feature rather than merely aspirational.

## 14. Layer 1: the exported surface

Two exports, one import. Nothing per-callback, nothing per-signature.

**Exports (host → guest):**

| Name | Signature | Contract |
|---|---|---|
| `_initialize` | `() -> void` | Existing. Init, run `main`, drain, then exit-or-return per §4.1. |
| `promise_web_enqueue` | `(sub_id: i32, handle: i32) -> i32` | Push one event. Never runs Promise code, never allocates on the Promise heap, safe in any context including inside a pump. Returns 0 = queued, 1 = dropped per policy. |
| `promise_web_pump` | `() -> i32` | Drain within budget. Returns 0 = idle, 1 = work remains (a continuation has been scheduled), 2 = terminated. No-op returning immediately if already pumping (§5) or faulted (§10). |

**Import (guest → host):**

| Name | Signature |
|---|---|
| `promise_env.schedule_pump` | `(kind: i32, delay_ms: i32) -> void` |

### 14.1 One name, one definition

#25's Bug 1 was caused by two generators independently inventing names for the
same logical thing and drifting apart. The structural fix here is that there is
almost nothing left to name — but the little that remains gets exactly one
definition.

**Decision: a new Go package `compiler/internal/wasmweb` holds these names as
constants**, together with the small helpers that derive anything name-shaped.
It is imported by `internal/codegen` (which emits the exports), by
`internal/bindgen/codegen.go` (which emits `.pr`), and by
`internal/bindgen/jsglue.go` (which emits `.js`). No string literal for any of
these names appears anywhere else, and a test asserts that.

The package is deliberately tiny and dependency-free so that both the compiler
backend and the binding generators can depend on it without a cycle.

### 14.2 It must be usable without bindgen

Layer 1 is a language capability, not bindgen plumbing that happens to be
reachable. The acceptance test is a hand-written program with no generated
bindings anywhere in it:

```
use web;

main() {
  clicks := web.events(web.document.body, ["click"]);
  mut count := 0;
  for e in clicks {
    count = count + 1;
    web.console.log("click " + count.to_string());
  }
}
```

This program registers one subscription, so by §4.1 it is a reactor: `main`'s
goroutine parks on an empty channel, `_initialize` returns, and each browser
click enqueues, pumps, advances the loop by one iteration, and returns.

## 15. Layer 2: WebIDL `callback` lowering

With Layer 1 in place, the WebIDL side needs **no new export at all** — which is
the payoff of §7 and the main reason the export surface is safe from drift.

- `bindgen/ir.go`: add a function-type case to `TypeRefKind`, which currently has
  no way to represent "this parameter is a callback".
- `webidl_to_ir.go:59-60`: stop dropping `file.Callbacks` on the floor. Lower
  each `Callback` to that new IR case using its already-parsed `Return`/`Params`
  — the WebIDL parser is complete and needs no changes.
- `codegen.go`: emit the parameter as a Promise `FunctionTypeRef`, and implement
  the method in terms of Layer 1 — subscribe, spawn a consumer goroutine that
  invokes the closure per event. This is precisely the `web.on` sugar of §8.
- `jsglue.go`: emit the listener that stores the event handle and calls
  `promise_web_enqueue` + `promise_web_pump`, using the shared names from §14.1.

`Element.addEventListener(DOMString, EventListener)` then works end-to-end with
no callback-specific machinery below the bindgen layer.

WIT gets this close to free: `wit_to_ir.go` shares the same IR, so once the
function-type case exists, mapping an equivalent WIT construct is a small
isolated change. Worth checking after Layer 2 lands; not a requirement.

## 16. Bind-time self-check

`promise bind webidl` currently exits 0 on output that cannot compile — the
`undefined type: EventListener` failure in #25 surfaces one command later, at
`promise build`. That is a separate defect from callbacks, and fixing it is what
stops the next such gap from shipping silently.

**Decision: `bind` type-checks what it just wrote, in-process, and exits
non-zero if it does not resolve.** Frontend only — no subprocess, no LLVM, no
linker.

Both halves already exist: `compileProjectFrontend` (`main.go:5583`) merges
every non-test `.pr` in a project directory, resolves module dependencies from
`promise.toml`, and runs sema → embeds → ownership; and `bind` already writes a
`promise.toml` into its output directory (`bind.go:194`, `bind.go:353`), so what
it generates is already a well-formed project. This is a call at the end of
`runBindWebIdl`/`runBindWit`, not new frontend plumbing.

Two scoping decisions:

- **Always on, both build modes.** Debug/release in this repo is about embedded
  LLVM tools and the backend pipeline, not a frontend mode. Validating in one
  build and not the other means broken output is caught locally and waved
  through in CI, or the reverse. `-no-check` is the escape hatch for anyone who
  wants the raw output; `bind` already takes `-target`, so it checks for the
  target it generated for.
- **`promise check` accepting only a single file is a separate bug** (`main.go:296-303`
  calls `compileFrontend(os.Args[2])`, parsing exactly one file, which fails on
  the first sibling reference in any multi-file project). It should resolve its
  input the way `build` does. Tracked separately; the bind self-check does not
  wait on it, and once both land the self-check is the same shared entry point
  called in-process.

## 17. Testing

The Node harness cannot validate this. It stubs every unrecognized `promise_env`
import as a no-op, so it would happily pass a build whose JS→WASM direction is
entirely wrong — the same blind spot that hid the four prior WebIDL bugs.

Three layers, all required:

1. **Go unit / IR-shape tests** — the exports exist with the right signatures;
   `_initialize` has no trailing `pal_exit` when a registration is live; the
   pump respects its budget; names come from `internal/wasmweb`.
2. **Promise-level tests** for the queue, overflow policies, and subscription
   close semantics — these are ordinary channel behaviour and testable natively.
3. **A real headless browser test** — Playwright driving system Chromium
   (`/usr/bin/chromium-browser`, already proven in this environment): dispatch a
   genuine `mousedown`/`mousemove` pair, then read back observable state
   (`getImageData` pixels, or an exported counter). Compiling and not crashing is
   not evidence.

Per the review of #25, the real surface area lives in
[promise-language/web](https://github.com/promise-language/web): the acceptance
criterion is that the #25 repro plus a regression test for each of the four
already-fixed WebIDL bugs exist there and pass against a compiler carrying this
change. Wiring out-of-repo modules into this repo's CI is a separate concern
with its own version-pinning question (`bin/verify` runs only `tests/ modules/
examples/ tools/stub/`, so nothing consumed by reference is exercised); it is
tracked separately and this work neither carries it nor waits on it.

## 18. Non-goals

- **`wasm32-wasi`.** Unchanged: `_start`, run-to-completion, deadlock abort.
- **Multi-argument and non-void-return callbacks.** The queue entry is
  `(sub_id, handle)`; richer signatures are a Layer 2 extension. Layer 1 does not
  preclude them.
- **`callback interface`** — a distinct WebIDL construct, currently parsed as a
  regular interface and discarded. Separate concern.
- **Threads.** `wasm32-web` remains single-threaded. No SharedArrayBuffer, no
  cross-origin isolation requirement.
- **A full `web` catalog API.** Layer 1 needs only enough public surface to prove
  it is usable outside bindgen (§14.2); the rest belongs to
  [#17](https://github.com/promise-language/promise/issues/17).

## 19. Implementation phases

1. **Reactor top level.** Liveness rule, `_initialize` exit-or-return, bounded
   pump, `schedule_pump` import, idle-vs-deadlock. No callbacks yet — verifiable
   on its own, and every existing test must stay green by construction (§4.1).
2. **Delivery.** `promise_web_enqueue`, the subscription table, queue policies,
   the `in_pump` re-entrancy guard, handle marshalling.
3. **Promise surface.** `web.events` / subscription type / `web.on` sugar, plus
   the hand-written no-bindgen acceptance program from §14.2.
4. **WebIDL lowering.** IR function-type case, `webidl_to_ir` callbacks,
   `codegen.go`/`jsglue.go` branches on the shared names.
5. **Bind-time self-check** (§16) — independent of 1–4 and landable in any order.
6. **Browser integration tests**, in this repo for the primitive and in `web`
   for the real surface.

Deferred, recorded so v1 does not foreclose them: software preemption (§6),
richer callback signatures, and out-of-repo CI plumbing (§17).
