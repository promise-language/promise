# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Promise is a statically-typed programming language with Dart-inspired syntax and Rust-inspired ownership semantics. The compiler is a single Go binary (`promise`) that uses ANTLR4 for parsing and targets LLVM IR (compiled via `opt` + `llc` + `ld.lld` on Linux, clang on other platforms).

**Primary goal**: Promise is designed for AI-agent efficiency — making it easy for LLMs to generate correct, self-contained programs in one shot or use them as tools. Every design decision should optimize for:
- **Self-contained readability**: Looking at a single source file should be enough to understand with certainty what it does. Avoid hidden effects, implicit behaviors, and action-at-a-distance.
- **Explicit over implicit**: Prefer explicit types, explicit error handling (`?`/`!`), explicit ownership (`~`/`&`), explicit mutability. No surprising defaults.
- **Minimal context needed**: An AI agent should be able to generate a correct Promise program without needing to understand a large framework or ecosystem. The standard library should be small, orthogonal, and predictable.
- **One obvious way**: Avoid multiple equivalent ways to do the same thing. Reduce ambiguity in both syntax and semantics so code generation is deterministic.

## Build & Test Commands

**IMPORTANT: Always use `./build` from the repo root to build the compiler. NEVER run `go build` directly — it skips resource embedding and produces a broken binary. The output is always `bin/promise`.**

```bash
# From repo root:
./build                # embed resources + build → bin/promise
bin/verify.sh          # full verify: generate + format + vet + build + go test + promise test
bin/verify.sh --clean  # same but clears caches first
```

The following `make` targets run from `compiler/` and also output to `bin/promise`:

```bash
make                  # download ANTLR4 JAR, generate parser, embed resources, build binary
make release          # release build: embed LLVM tools (~61MB self-contained binary, Linux only)
make test             # run all Go tests (go test ./...)
make generate         # regenerate ANTLR4 parser from grammar
make resources        # copy std/ into embedded resources
make clean            # remove generated code and binary
```

Go tests run from `compiler/`:

```bash
# Run tests for a single package
go test ./internal/codegen/ -count=1
go test ./internal/sema/ -count=1
go test ./internal/ownership/ -count=1

# Run a single test
go test ./internal/codegen/ -run TestDropBasicScopeExit -v -count=1

# Coverage
go test ./internal/codegen/ -coverprofile=/tmp/cov.out -count=1
go tool cover -func=/tmp/cov.out
```

Promise test commands (use `bin/promise` from repo root):

```bash
bin/promise test file.pr                       # default 60s timeout
bin/promise test -timeout 30s file.pr          # custom timeout (Go duration or seconds)
bin/promise exec -timeout 10s 'println("hi")'  # exec with timeout

# Stress testing (flaky test detection)
bin/promise test -stress tests/...                       # run until Ctrl+C
bin/promise test -stress 100 tests/concurrency/...       # run 100 iterations
bin/promise test -stress 30s tests/concurrency/...       # run for 30 seconds
bin/promise test -timeout 10s -stress 50 tests/...       # per-run timeout + stress

# Cache diagnostics
PROMISE_CACHE_DEBUG=1 bin/promise test tests/...         # show cache HIT/MISS/SKIP on stderr
```

**Test output format** — designed for AI-agent tail-friendliness:
```
PASS (0.000s) test_name        # individual results: PASS/FAIL (time) name
FAIL (0.001s) test_broken
20 passed, 1 failed (0.423s)   # summary line (single file)
20 passed, 1 failed, 2 skipped (0.423s)  # with target-excluded tests
FAILED:                         # only appears when failures exist
  test_broken

568 passed, 2 failed (117 files, 30.810s)  # directory summary
568 passed, 2 failed, 3 skipped (117 files, 30.810s)  # with skipped tests
FAILED:
  std/test_vector.pr: test_push
  e2e/basics.pr (timeout)
```
An agent can read the last ~20 lines of output to identify all failures without re-running or grepping.

**Stress test output** — reports flaky tests, timing variance, and adaptive scheduling:
```
=== Stress Test Report ===
50 iterations over 45.2s

FLAKY (2 tests):
  concurrency/stress_unbuffered.pr
    test_channel_send              47/50 (94.0%)  avg: 23.1ms  σ: 15.2ms  min: 2.1ms  max: 89.3ms

HIGH VARIANCE (1 test):
  concurrency/test_channel_basic.pr
    test_buffered_recv             50/50 (100%)  avg: 5.2ms  σ: 8.4ms  CoV: 1.62

STABLE: 45 tests across 12 files
```
Stress mode compiles once and re-runs binaries. Stable files are gradually suppressed (run every 2nd/4th/8th iteration). Exit code 1 if any test has failures.

## Compiler Pipeline

```
.pr source → ANTLR4 (grammar/) → AST (ast/) → Sema 4-pass (sema/) → Ownership (ownership/) → LLVM IR (codegen/) → opt+llc+lld → binary
```

On Linux: `opt -O1` (coroutine lowering) → `llc -filetype=obj` → `ld.lld -static` (link with bundled musl CRT → fully static binaries). On macOS: `opt -O1` → `llc -filetype=obj` → system `ld` (or `ld64.lld`) with `-lSystem -syslibroot`. On other platforms (or `PROMISE_USE_CLANG=1`): `clang -O1`. Requires LLVM 22+. Release builds (`make release`) embed gzip-compressed LLVM tools in the binary (~61MB), extracted lazily to `~/.promise/cache/llvm/`.

Entry point: `cmd/promise/main.go` → `compileFrontend()` orchestrates parse → std merge → sema → ownership.

**Sema 4 passes** (in `sema/check.go`):
1. **Declare** — register all type/enum/func names in scope
2. **Define** — resolve parent types, fields, method signatures
3. **Check** — type-check all function/method bodies
4. **Verify** — return path analysis

**Ownership** runs after sema: move tracking, borrow conflicts, return safety.

**Codegen** (`codegen/compiler.go`): AST + sema info → LLVM IR module. Handles monomorphization of generics, RTTI for inheritance, vtable dispatch, scope cleanup (use/drop bindings).

## Key Architecture Concepts

**Four-struct type layout**: Each user type generates up to 4 LLVM structs — Type (static), Instance (heap fields), Value (`{vtable_ptr, instance_ptr}`), and Variant (enum payload). Value structs are passed around; instance ptrs are what methods receive as `this`. **Pure value types** (all fields `` `value ``): Value struct is `{vtable_ptr, rtti_ptr, field1, field2, ...}` with data embedded directly — no heap allocation. Instance struct is a global RTTI-only singleton. Automatically `` `copy ``, no `is` parents, no `drop()`. See `layout.go:computeValueTypeLayout()`.

**Scope cleanup stack**: `scopeBindings` is a LIFO stack of `bindingClose` (use vars), `bindingDrop` (droppable vars), and `bindingFreeEnv` (closure env structs). Emitted in reverse at scope exit, return, raise, break, continue. Drop bindings have an `i1` drop flag that's cleared at move sites. Env free bindings null-check the env pointer before calling `free()`.

**Closures/lambdas**: All function values use fat pointer `{ i8*, i8* }` (fn ptr + env ptr). Lambda functions always take `i8* %env` as first parameter. Capture analysis in sema (`checkLambdaCapture`) detects outer-scope variable references; `Copy` types auto-captured, non-`Copy` requires `move`. Nested captures propagate through intermediate lambdas. Named function references use thunks (`.thunk.name`) to adapt to env-first ABI. Env structs are heap-allocated via `malloc` and freed via `bindingFreeEnv` at scope exit.

**Monomorphization**: Generic types/functions are specialized at codegen time. `mono.go` handles collecting instances, creating specialized layouts, and defining specialized methods.

**M:N Scheduler** (`codegen/sched.go`): GMP model — G (goroutine/LLVM coroutine), P (processor with 256-slot ring buffer run queue), M (OS thread via `pal_thread_create` with explicit 2MB stack — musl defaults to 128KB). Key concurrency invariants:
- **Park mutex protocol**: Goroutines store the channel/done mutex in `G.park_mutex` before `coro.suspend`. The scheduler unlocks it after `coro.resume` returns. This prevents enqueue-before-suspend races — the waker must acquire the same mutex, blocking until suspend completes.
- **park_mutex = null means yield**: The scheduler re-enqueues the goroutine (cooperative preemption). park_mutex != null means park — the goroutine is on a waiter list and will be woken by another goroutine.
- **Select blocking uses yield-and-retry**: Because select involves multiple channel mutexes but park_mutex can only hold one, blocking select yields (park_mutex=null) and retries the lock→try→unlock cycle until a case is ready. This avoids multi-mutex enqueue-before-suspend and double-wake races.
- **Park_m spurious wakeup protection**: `park_m` loops on `cond_wait` checking `M.spinning` flag (set by `wake_m` before signaling). Prevents spurious wakeups from corrupting the idle M stack.
- **Work stealing lock order**: `steal_work` locks both thief and victim P's in address order (ptrtoint comparison) to prevent ABBA deadlock between concurrent stealers.
- **Waiter lists**: Intrusive linked list via `G.wait_next`. Protected by channel mutex. `promise_waiter_enqueue/dequeue/remove/wake_all` helpers in sched.go.
- **Sysmon**: Background thread sets `G.preempt=1` every 10ms; yield checks at loop back-edges call `coro.suspend`.

**Standard library**: `.pr` files in `std/` are embedded via `go:embed` and merged into user AST before sema. Runtime is codegen-emitted LLVM IR (no C runtime).

## Test Patterns

Tests use helper functions that build a mini standard library (`stdAll`) with all primitive type declarations, then parse + sema + codegen user code:

```go
// codegen tests
ir := generateIR(t, `
    type Foo { int x; }
    main() { f := Foo(x: 1); }
`)
assertContains(t, ir, "call void @Foo.drop")

// sema tests
errs := checkErrs(t, `type Bad { drop(this) {} } main() {}`)
expectError(t, errs, "must take ~this")

// ownership tests
ownerOK(t, `type R { int id; drop(~this){} } test() { r := R(id:1); }`)
errs := ownerErrs(t, `...`)
expectOwnerError(t, errs, "use of moved variable")
```

Methods must be declared inside the type body. Numeric literals infer as `int`/`f64` — use typed parameters for i8/i16/i32/f32 tests.

## Important Files

- `compiler/grammar/PromiseParser.g4` — grammar is the language spec
- `docs/stages.md` — implementation roadmap with deferred bug tracker
- `docs/language-design.md` — full language design proposal
- `compiler/internal/codegen/compiler.go` — codegen entry, type layouts, scope cleanup
- `compiler/internal/codegen/sched.go` — M:N scheduler: GMP structs, sched_loop, park/wake, steal, shutdown
- `compiler/internal/codegen/stmt.go` — statement codegen, drop/close emission, select
- `compiler/internal/codegen/expr.go` — expression codegen, channel send/recv, go blocks
- `compiler/internal/sema/check.go` — sema orchestration
- `compiler/internal/sema/info.go` — sema output (type map, objects, lambda captures)
- `compiler/internal/sema/decl.go` — type/method/func definition passes
- `compiler/internal/types/named.go` — Named type with fields, methods, generics, flags

## Implementation Philosophy

- **Prefer Promise over IR**: When adding new standard library functionality (e.g., container methods, operators), implement in the Promise language (`std/*.pr`) rather than generating custom LLVM IR in codegen. Only use `native` methods when direct memory access or runtime calls are unavoidable (e.g., `Vector.push`, `Vector.[]`, string byte access). Non-native methods written in Promise are type-checked by sema, monomorphized automatically, and far easier to maintain.
- **Test at every level**: Significant changes need both Go unit tests (`codegen_test.go`, `sema_test.go`) AND Promise-level e2e tests (`tests/` directory, run via `promise test`). Go tests verify IR shape; Promise tests verify runtime correctness.
- **No hidden effects**: When designing language features, avoid implicit side effects, hidden control flow, or magic behaviors. Every effect should be visible at the call site. If a function can fail, it must be marked failable (`!`). If a value is consumed, it must be moved (`~`). If a variable is mutable, it must be declared so.
- **Self-contained by default**: Design features so that programs are understandable in isolation. Avoid global state, implicit initialization, and ambient context. A reader (human or AI) should be able to read a `.pr` file top-to-bottom and know exactly what it does without consulting external docs or hidden configuration.

## Conventions

- Compiler errors are accumulated (not fatal on first error) and printed together
- `extractNamed(typ)` unwraps Instance/SharedRef/MutRef to get underlying `*types.Named`
- `needsVtable(named)` returns true if type has children or is abstract → virtual dispatch
- `mangleMethodName(owner, name, failable)` produces LLVM function names like `TypeName.method`
- Move sites must call `clearDropFlag(name)` — there are 8 call variant sites in expr.go plus assignment sites in stmt.go
- All tests must pass after changes. Significant changes need accompanying tests.
- When updating `std/*.pr`, also update the `stdAll` mini standard library in test files (`codegen_test.go`, `sema_test.go`, `ownership_test.go`) to match, then run `make resources` to embed changes.
