# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Promise is a statically-typed programming language with Dart-inspired syntax and Rust-inspired ownership semantics. The compiler is a single Go binary (`promise`) that uses ANTLR4 for parsing and targets LLVM IR (compiled via `opt` → `.bc` → `ld.lld --lto-O1` on Linux, `opt` → `.bc` → `ld64.lld --lto-O1` on macOS, `opt` → `.bc` → `wasm-ld --lto-O2` on WASM, `opt` → `llc` → `lld-link` on Windows, clang on other platforms).

**Primary goal**: Promise is designed for AI-agent efficiency — making it easy for LLMs to generate correct, self-contained programs in one shot or use them as tools. Every design decision should optimize for:
- **Self-contained readability**: Looking at a single source file should be enough to understand with certainty what it does. Avoid hidden effects, implicit behaviors, and action-at-a-distance.
- **Explicit over implicit**: Prefer explicit types, explicit error handling (`?`/`!`), explicit ownership (`~`/`&`), explicit mutability. No surprising defaults.
- **Minimal context needed**: An AI agent should be able to generate a correct Promise program without needing to understand a large framework or ecosystem. The standard library should be small, orthogonal, and predictable.
- **One obvious way**: Avoid multiple equivalent ways to do the same thing. Reduce ambiguity in both syntax and semantics so code generation is deterministic.

## Build & Test Commands

**IMPORTANT: Always use `./build` from the repo root to build the compiler. NEVER run `go build` directly — it skips resource embedding and produces a broken binary. The output is always `bin/promise`.**

**IMPORTANT: Never commit, push, or create PRs unless the user explicitly asks you to.** Wait for an explicit instruction like "commit", "push", or "create a PR" before performing any git write operations.

**IMPORTANT: Always run `bin/verify.sh --local --wasm` before committing changes.** This formats Go and Promise code, runs `go vet`, and executes the full test suite (including WASM target). The `--local` flag uses a local cache directory to avoid polluting `~/.promise`. Do not commit if verify fails.

**Setup (once per clone):** `bin/setup.sh` — enables git hooks and local dev environment. Also runs automatically on `./build`.

```bash
# From repo root:
./build                    # generate parser + embed resources + build → bin/promise
bin/test.sh                # build + run all tests (go + promise)
bin/test.sh go             # Go unit tests only
bin/test.sh promise        # Promise tests only
bin/test.sh --wasm         # include wasm32-wasi target
bin/test.sh --clean        # clear caches first
bin/verify.sh              # format + vet + all tests (pre-commit check)
bin/verify.sh --wasm       # include wasm target
```

The following `make` targets run from `compiler/` and also output to `bin/promise`:

```bash
make                  # download ANTLR4 JAR, generate parser, embed resources, build binary
make release          # release build: embed LLVM tools (~61-71MB self-contained binary)
make test             # run all Go tests (go test ./...)
make generate         # regenerate ANTLR4 parser from grammar
make resources        # copy modules/ into embedded resources
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
bin/promise test -parallel 4 tests/...         # run up to 4 tests in parallel (default: NumCPU)
bin/promise exec -timeout 10s 'print_line("hi")'  # exec with timeout (failable main, ? works)
bin/promise emit-ir file.pr                    # print LLVM IR to stdout
bin/promise emit-ir file.pr > out.ll           # save IR to file

# Stress testing (flaky test detection)
bin/promise test -stress tests/...                       # run until Ctrl+C
bin/promise test -stress 100 tests/concurrency/...       # run 100 iterations
bin/promise test -stress 30s tests/concurrency/...       # run for 30 seconds
bin/promise test -timeout 10s -stress 50 tests/...       # per-run timeout + stress

# Cache diagnostics
PROMISE_CACHE_DEBUG=1 bin/promise test tests/...         # show cache HIT/MISS/SKIP on stderr
```

**Test output format** — designed for AI-agent tail-friendliness:

Single-file output (verbose — shows every test with timing):
```
PASS (0.001s) test_add
PASS (0.002s) test_sub
FAIL (0.003s) test_broken
  panic: assertion failed: expected 3, got 4   # panic context shown under FAIL
PASS (0.001s) test_other                       # subsequent tests still run after panic

3 passed, 1 failed (0.423s)
FAILED:
  test_broken
```

Multi-file output (compact — one line per file):
```
PASS (0.004s) e2e/basics.pr (3 tests)         # passing file with test count
PASS (0.001s) e2e/hello.pr                    # single-test file (no count)
FAIL (0.005s) e2e/strings.pr (1/3 failed)     # failing file with ratio
  test_split                                   # failed test name
    panic: assertion failed                    # panic context
FAIL (0.000s) broken.pr (compilation error)    # compilation failure
  broken.pr:5:3: type Foo has no field 'bar'   # error context

568 passed, 2 failed (117 files, 30.810s)
FAILED:                                        # summary with failure context
  e2e/strings.pr: test_split
    panic: assertion failed
  broken.pr (compilation error)
    broken.pr:5:3: type Foo has no field 'bar'
```
An agent can read the last ~20 lines of output to identify all failures without re-running or grepping.

**Stress test output** — reports flaky tests, timing variance, and adaptive scheduling. Crash context (signal, stderr) is captured for diagnosis:
```
=== Stress Test Report ===
Target: linux-x86_64
50 iterations over 45.2s

FLAKY (2 tests):
  concurrency/stress_unbuffered.pr
    test_channel_send              47/50 (94.0%)  avg: 23.1ms  σ: 15.2ms  min: 2.1ms  max: 89.3ms
      1 fail  last: SIGSEGV
      | signal: SIGSEGV
      | stderr:
      |   panic: stack overflow at 0x7fff...
      |   goroutine 42 [running]:

HIGH VARIANCE (1 test):
  concurrency/test_channel_basic.pr
    test_buffered_recv             50/50 (100%)  avg: 5.2ms  σ: 8.4ms  CoV: 1.62

STABLE: 45 tests across 12 files
```
Stress mode compiles once and re-runs binaries. Stdout and stderr are captured separately — PASS/FAIL lines parsed from stdout, crash context (signal name, exit code, last 20 lines of stderr) from stderr. Stable files are gradually suppressed (run every 2nd/4th/8th iteration). Exit code 1 if any test has failures.

## Compiler Pipeline

```
.pr source → ANTLR4 (grammar/) → AST (ast/) → Sema 4-pass (sema/) → Ownership (ownership/) → LLVM IR (codegen/) → opt+lld(LTO) → binary
```

On Linux/macOS/WASM: `opt -O1` (coroutine lowering + optimization) → `.bc` (LLVM bitcode) → linker with `--lto-O1` (`--lto-O2` for WASM). LTO performs whole-program inlining and DCE across all modules at link time. On Windows: `opt -O1` → `llc -filetype=obj` → `lld-link` (LTO not yet wired up for MSVC). On other platforms (or `PROMISE_USE_CLANG=1`): `clang -O1`. Requires LLVM 22+. Release builds (`./build --release`) embed gzip-compressed LLVM tools in the binary (~61-71MB), extracted lazily to `~/.promise/cache/llvm/<platform>/`. Platform-specific embed files (`llvm_linux_amd64.go`, `llvm_darwin_arm64.go`, `llvm_darwin_amd64.go`) select the correct tools. On macOS, extracted Mach-O binaries are patched with `install_name_tool` (rpath + dylib references) and re-signed with `codesign`.

Entry point: `cmd/promise/main.go` → `compileFrontend()` orchestrates parse → sema (with auto-injected `use std as _`) → ownership.

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

**Monomorphization** (`codegen/mono.go`): Generic types and functions are specialized at codegen time. The pipeline has three phases for each module compilation:

**1 — Collect**: Gather all concrete instantiations transitively from sema info.
- `collectMonoInstances(info)` → `[]*types.Instance` — deduplicates by mangled name (`monoName(inst)` → `"Box__int__string"`), then transitively expands: (a) walks each instance's substituted field types via `discoverInstances`, (b) walks parent types (e.g., `Range[int] is Stream[T]` → adds `Stream[int]`), (c) resolves _unresolved instances_: sema type-checks generic method bodies once with TypeParams unbound, recording e.g. `_FnIter[T]` (with `TypeParam` embedded) in `info.Types`; `collectUnresolvedInstances` finds these, and `resolveUnresolvedInstances` applies the current concrete substitution to produce `_FnIter[int]`.
- `collectMonoFuncInstances(info, typeInstances)` / `collectMonoMethodInstances(info, typeInstances)` — deduplicate `FuncInstance` and `MethodInstance` records, then resolve _unresolved_ instances (those whose `TypeArgs` contain `TypeParam`). These arise when a generic function/method body calls another generic function/method using the outer's type parameter (e.g., `wrap[T]` calling `identity[T]`). Resolution uses substitution maps from: (a) concrete type instances (for generic type methods calling generic functions), (b) concrete func/method instances of the same kind (transitive self-resolution for chains like `a[T]→b[T]→c[T]`). After both are collected, `crossResolveFuncMethodInstances` performs bidirectional cross-resolution: FuncInstance substs resolve unresolved MethodInstances and vice versa (e.g., `invoke[T]` calling `e.echo[T]`).
- **Cross-module — `collectMonoInstancesWithExtra(modInfo, modFile, extra)`**: seeds the transitive expansion with the module's own `modInfo.SemaInfo.Instances` PLUS any extra `*types.Named`-origin instances from user code whose type name appears in `modFile`'s declarations. Unresolved-instance expansion uses `modInfo.SemaInfo` so method-body type references (e.g., `_FnIter[T]` inside `Vector[T].iter()`) resolve against the module's types. Note: only `*types.Named` extras are filtered in this way — generic enum instances from user code are not propagated via the extra path.
- **`collectMonoFuncInstancesWithExtra` / `collectMonoMethodInstancesWithExtra`**: same pattern — filter extra instances by whether the function/owner-type name is in `modFile`'s declarations. Unresolved extras (containing TypeParams) are skipped.

**2 — Declare**: Create LLVM struct layouts and function stubs.
- `computeMonoLayouts(instances)` — for each `*types.Named` instance with TypeParams, produces 4 LLVM structs with all field types substituted via `types.Substitute(f.Type(), subst)`. The exact layout depends on kind:
  - **Heap user type**: `_t` (Type, empty `{}`), `_m` (Variant, `{T_t* _type}`), `_i` (Instance, `{T_m* _variant, field1, field2, ...}`), `_v` (Value, `{i8* _vtable, T_i* _instance}`)
  - **Value type** (all fields `` `value ``): same `_t`/`_m`, but `_i` is RTTI-only `{T_m* _variant}` with no user fields, and `_v` embeds fields directly: `{i8* _vtable, T_i* _rtti, field1, field2, ...}`
  - For `*types.Enum` instances: `computeMonoEnumLayout` produces `_t`/`_m`/`_i` (RTTI-only) plus `_v` as `{i8* _vtable, T_i* _instance, i32 tag, [maxDataSize x i8] data?}` with per-variant data structs.
- `emitMonoVtableGlobals(instances)` — creates immutable `[N x i8*]` vtable globals (`promise_vtable_Box__int`). Called at two points in the overall compile sequence: first in the main codegen phase **before** `compileModules()` (module-owned functions not yet declared → some slots are `null`), then again **inside** each `compileModule()` call. The second call **updates** the existing global's `Init` field with newly available function pointers rather than skipping — this is the critical fix for module-owned generic types.
- `emitMonoTypeInfoGlobals(instances)` — creates immutable typeinfo globals (`promise_typeinfo_Box__int`) with layout `{i8* vtable_ptr, i32 typeID, i32 numParents, [N x i32] parentIDs}`. TypeID is assigned from `c.nextTypeID++`. For value types, also emits a global RTTI instance (`promise_rtti_Box__int`).
- `declareMonoMethods(file, instances)` — creates LLVM function stubs for non-generic methods on each `*types.Named` instance (e.g., `Box__int.foo`). Skips structural types (their default methods are synthesized separately). Skips generic methods (`len(md.TypeParams) > 0`, handled by `declareMonoMethodInstances`). Also verifies the found TypeDecl matches the exact `*Named` origin to prevent name collisions between user types and std types.
- `declareMonoSynthesizedDefaults(file, instances)` — declares stubs for default methods inherited from structural interfaces. Delegates to `declareStructuralDefaultStubs` which skips methods the concrete type already defines, skips abstract methods, and recurses into transitive structural parents. These stubs are NOT registered in `moduleOwnedFuncs` (they live in the main IR even when declared inside a `compileModule` call).
- `declareMonoFuncs(file, funcInsts)` / `declareMonoMethodInstances(file, methodInsts)` — declare stubs for generic free functions and generic methods. Both register `c.moduleOwnedFuncs[name] = c.compilingModule` when `c.compilingModule != ""`.
- All declare functions guard against re-declaration: `if _, exists := c.funcs[name]; exists { continue }`.

**3 — Define**: Generate function bodies with type substitutions active.
- `defineMonoMethods`: sets `c.typeSubst = subst` and `c.monoCtx = &monoContext{inst, origin, name}`, calls `defineMethodFunc` (or `defineGeneratorMethod` for generator methods). Cleans up via deferred `c.typeSubst = nil; c.monoCtx = nil`.
- `defineMonoSynthesizedDefaults` → `defineStructuralDefaultBodies`: uses `c.saveState()` / `c.restoreState(saved)` for full context save, sets `c.selfSubst = &selfSubstInfo{iface, concrete}` so the interface's `Self` resolves to the concrete type inside the default method body. Recurses into structural parent chains.
- `defineMonoFuncs` / `defineMonoMethodInstances`: set `c.typeSubst` (and optionally `c.monoCtx` for method instances on generic owner types), call `defineFunc` / `defineGeneratorFunc` / `defineMethodFunc`.
- All define functions guard against re-definition with `if !ok || len(fn.Blocks) > 0 { continue }` — essential in the module-internal test path where module sources are merged into the main file, which would otherwise cause both the module phase and main phase to append bodies to the same function → malformed IR.

**Naming**: `monoName(inst)` → `"Map__string__int"` (type args joined by `__`). `monoFuncName(fi)` → `"sort__int"`. `monoMethodInstanceName(mi)` → `"Box__int.transform__string"` (owner mono name + `.` + method name + `__` + method type args). Name encodes the full nested arg tree: `Map[string, Vector[int]]` → `"Map__string__Vector__int"`. Setter methods use a `$set` suffix: `mangleMethodName(name, "[]", true)` → `"Box__int.[]$set"`.

**Type substitution across inheritance**: `mergeParentSubst(named, subst)` augments a substitution map with mappings for inherited type params — resolves parent type args under the current subst, builds a parent sub-map, adds it to `subst`, then recurses for the parent's own parents. `buildMethodInstanceSubst(mi)` starts from owner type-level subst (if `OwnerInst != nil`) + `mergeParentSubst` + method type-level subst, producing a single map that covers all type params visible inside a method body.

**Module separation + caching**: After codegen, `SplitModuleIRs()` assigns functions to module IRs using `moduleOwnedFuncs[name] = moduleName`, registered during the declare phase when `c.compilingModule != ""`. Note that synthesized structural default method stubs are NOT registered (they always stay in the main IR). Module IRs are compiled to `.bc` (LTO) or `.o` and cached. The cache key includes a FNV-128a hash of the **module IR text** (not just source hash), making it sensitive to which mono instances are present — two user programs importing the same module but using different generic instances get separate cache entries. String constants use `LinkagePrivate` so each split `.bc` (module, instance) contains its own copy — no cross-module extern references for strings. Per-module names (`@.str.__mod_std.N`, counter reset to 0 at each `compileModule()` start) remain stable within a module compilation. LTO deduplicates identical private globals at link time.

**Per-instance `.bc` caching**: Each generic type instantiation (`Box__int`, `Vector__string`, etc.) gets its own cached LLVM bitcode file, keyed by `InstanceCacheKey(irPrefix, monoName, typeDeclHash, compilerHash, target)`. The `typeDeclHash` is a FNV-128a hash of the *entire* TypeDecl/EnumDecl AST (fields, method signatures + bodies, annotations, type params, inheritance) computed by `HashTypeDecl`/`HashEnumDecl` in `sema/typehash.go` and stored in `sema.Info.DeclHashes map[*types.TypeName]string`. The cache key changes whenever the type definition changes, but not when unrelated declarations in the same file change. Before codegen, `lookupCachedInstances` checks the build cache for each instance and returns a `map[string]bool`; `CompileWithCache(…, cachedInstances)` skips body generation for cached instances while still registering them in `instanceOwnedFuncs` so `SplitModuleIRs` strips them correctly. After codegen, a pre-cached loop in `compileAndLinkSeparate` loads pre-cached `.bc` files directly (since `InstanceIRs()` only returns instances whose bodies were actually generated). Instance `.bc` files contain only their own function bodies; vtable/typeinfo definitions stay in the main IR (referenced as `extern declare` in instance BCs). `InstanceIRs()` extracts per-instance IR slices by temporarily stripping non-owned function bodies using `saveAndStripNonOwnedInst`.

**M:N Scheduler** (`codegen/sched.go`): GMP model — G (goroutine/LLVM coroutine), P (processor with 256-slot ring buffer run queue), M (OS thread via `pal_thread_create` with explicit 2MB stack — musl defaults to 128KB). Coroutine block structure: `coro.start` (coro.begin + captured-param allocas + entry-block allocas via `createEntryAlloca`) → `br coro.init.suspend` → `coro.init.suspend` (initial coro.suspend + switch to body/cleanup/suspend). The separate `coro.init.suspend` block ensures allocas precede the suspend point so `coro-split` can spill them to the coroutine frame. Key concurrency invariants:
- **Park mutex protocol**: Goroutines store the channel/done mutex in `G.park_mutex` before `coro.suspend`. The scheduler unlocks it after `coro.resume` returns. This prevents enqueue-before-suspend races — the waker must acquire the same mutex, blocking until suspend completes.
- **park_mutex = null means yield**: The scheduler re-enqueues the goroutine (cooperative preemption). park_mutex != null means park — the goroutine is on a waiter list and will be woken by another goroutine.
- **Select blocking uses SelectWaiterNode (SWN) parking**: Each select case gets a stack-allocated SWN (G-layout-compatible at fields 0–4, field 1=0xFF sentinel). SWNs are enqueued on channel waiter lists; a per-select mutex (stored in each SWN and in G.park_mutex) provides wake-once semantics. `select_try_wake` CAS's `G.select_case` under the select mutex. Channel send/recv/close use `promise_waiter_wake_one` which handles both regular G and SWN nodes. On resume, the goroutine locks all channels, removes remaining SWNs, and dispatches on `G.select_case`.
- **Park_m spurious wakeup protection**: `park_m` loops on `cond_wait` checking `M.spinning` flag (set by `wake_m` before signaling). Prevents spurious wakeups from corrupting the idle M stack.
- **Work stealing lock order**: `steal_work` locks both thief and victim P's in address order (ptrtoint comparison) to prevent ABBA deadlock between concurrent stealers.
- **Waiter lists**: Intrusive linked list via `G.wait_next` (field 4). Protected by channel mutex. `promise_waiter_enqueue/dequeue/remove/wake_all/wake_one` helpers in sched.go. Lists can contain both regular G nodes and SelectWaiterNode (SWN) entries — `wake_one` and `wake_all` check field 1 (0xFF sentinel) to distinguish.
- **Sysmon**: Background thread sets `G.preempt=1` every 10ms; yield checks at loop back-edges call `coro.suspend`.
- **Stack overflow detection** (B0010): Guard page via `pthread_attr_setguardsize(4096)` on each M's 2MB stack. SIGSEGV/SIGBUS handler prints "fatal: stack overflow" to stderr and calls `_exit(2)`. macOS uses `sigaction(SA_ONSTACK)` + per-thread `sigaltstack(64KB)` for reliable delivery; Linux uses `signal()` best-effort. Handler registered at startup (`pal_stack_overflow_init`); per-thread alt stack set up at `sched_loop` entry (`pal_stack_overflow_thread_init`).

**Standard library**: 29 `.pr` files in `modules/std/` compiled as a regular embedded catalog module and auto-imported into every file via `use std as _`. Catalog modules (`modules/io/`, `modules/path/`, `modules/math/`, `modules/strings/`, `modules/os/`, `modules/time/`, `modules/http/`) are separate compilation units with their own `promise.toml`. Runtime is codegen-emitted LLVM IR (no C runtime). See `docs/standard-library.md` for the full module inventory, PAL extensions, and implementation phases.

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

Methods must be declared inside the type body (for types) or after variants (for enums). Numeric literals infer as `int`/`f64` — use typed parameters for i8/i16/i32/f32 tests.

## Standard Library & Catalog Modules

The standard library (`modules/std/`, 29 files) is auto-imported via `use std as _` into every file. Catalog modules (`modules/<name>/`) are separate compilation units imported explicitly with `use <name>;`.

**Standard library** (`modules/std/`):

| Category | Files | What it covers |
|----------|-------|---------------|
| Primitives | `int.pr`, `uint.pr`, `float.pr`, `bool.pr`, `char.pr` | Arithmetic, comparison, bitwise, hash, `to_string()`, `format()`, `parse()`, `encode(Encoder)`, `decode(Decoder)` for all numeric/bool/char/string types |
| Strings | `string.pr` | Concatenation, comparison, `contains`, `starts_with`, `ends_with`, `index_of`, `trim`, `split`, `[]`, `[:]`, `bytes()`, `byte_at()`, `from_bytes()`, `to_upper`, `to_lower`, `repeat`, `replace`, `count`, `chars` |
| Containers | `vector.pr`, `map.pr`, `set.pr` | `Vector[T]`/`T[]` (push/pop/remove/contains/slice/`filled`), `Map[K,V]`/`map[K,V]` (open-addressing, rehash), `Set[T]` |
| Format/Parse | `format.pr`, `builder.pr`, `parse.pr` | `Writer`/`Format` structural interfaces, `Builder` (string building, satisfies `Writer`), `Reader`/`Parse` structural interfaces, `Scanner`, `scan[T]()` |
| I/O (std) | `io.pr` | `Reader`/`Writer`/`Closer` structural interfaces, `println`, `print_int`, `print_f64`, `print_bool` |
| Iterators | `iter.pr` | `Iterator[T]` structural interface with 20 default combinator methods, `Stream[T]`, `_FnIter[T]`, `Generator[T]`, duck-typed for-in |
| Math | `math.pr`, `random.pr` | `min`, `max`, `abs`, `clamp`, `sqrt`, `sin`, `cos`, `tan`, `pow`, `exp`, `log`, `floor`, `ceil`, `round`, `Random` PRNG |
| Sorting | `sort.pr` | `sort(T[])` for `Ordered` types |
| Interfaces | `equal.pr`, `ordered.pr`, `hashable.pr` | `Equal`, `Ordered`, `Hashable` structural types |
| Concurrency | `channel.pr`, `task.pr`, `runtime.pr` | `Channel[T]`/`channel[T]` send/close, `Task[T]`/`task[T]` handle, scheduler stats |
| Time | `time.pr` | `Duration` (value type), `Instant`, `sleep()` |
| Serialization | `encode.pr` | `Encoder`/`Decoder` (non-structural), `Encodable`/`Decodable` (structural), `DecodeError` |
| Other | `range.pr`, `hash.pr`, `assert.pr`, `error.pr`, `platform.pr` | `Range`/`..`/`..=`, FNV-1a hash, `assert()`, `error` base type, platform detection |

**Catalog modules** (separate `promise.toml`, imported via `use <name>;`):

| Module | File | What it covers |
|--------|------|---------------|
| `io` | `modules/io/io.pr` | `File` (open/create/append, read/write bytes, read_line, write_line, read_all, seek), `BufferedReader`, `BufferedWriter`, `Dir`, `IoError`, `read_line()`, `read_stdin()` |
| `json` | `modules/json/json.pr` | `JsonEncoder` (is Encoder), `JsonDecoder` (is Decoder), `encode_string[T]`, `decode_string[T]`, `encode_string_pretty[T]`, `JsonValue` enum (with `is_null`..`is_object`, `as_bool`..`as_object`, `get(key)`, `at(index)`, `encode`, `format`, `format_pretty`), `parse_value` |
| `path` | `modules/path/path.pr` | `path_join`, `path_dir`, `path_base`, `path_ext`, `path_is_abs`, `path_normalize` |
| `math` | `modules/math/math.pr` | Extended math functions |
| `strings` | `modules/strings/strings.pr` | Extended string utilities |
| `os` | `modules/os/os.pr` | OS interaction, environment, one-shot `execute`, streaming `Process`/`ProcessInput`/`ProcessOutput`, OS info (user/group/hostname/pid), signal handling (`Signal` enum, `setup_signal_handling`, `receive_signal`) |
| `time` | `modules/time/time.pr` | Extended time utilities |
| `http` | `modules/http/http.pr` | HTTP client (in progress) |

## Test Suite

~2563 test functions across ~215 `.pr` files, organized by category:

| Directory | What it tests | Files |
|-----------|--------------|-------|
| `tests/e2e/` | Language features: inheritance, generics, errors, lambdas, match, enums (incl. enum methods, enum fields), casting, control flow | ~67 |
| `tests/std/` | Standard library: all primitive types, containers, iterators, math, time, formatting, parsing | ~27 |
| `tests/concurrency/` | M:N scheduler, channels, select, tasks, goroutines, panic recovery, stress tests, IO syscall handoff, batch test scheduler init | ~86 |
| `tests/modules/` | Module system: imports, visibility, generics across modules, transitive deps, diamond deps | ~16 |
| `tests/value_types/` | Pure value types: construction, copying, operators, methods, nested, optional | ~9 |
| `tests/arrays/` | Fixed-size arrays: basic, copy, field, loop, OOB, parameters | ~6 |
| `tests/catalog/` | Catalog module integration tests | ~1 |
| `modules/*/` | Catalog module internal tests (`*_test.pr`) | ~6 |

## Important Files

- `compiler/grammar/PromiseParser.g4` — grammar is the language spec
- `docs/stages.md` — implementation roadmap and architecture notes (bugs/tasks/deferred items are tracked in the `tracker` MCP server)
- `docs/language-design.md` — full language design proposal (types, ownership, errors, generics, modules)
- `docs/standard-library.md` — stdlib design: module inventory, PAL extensions, implementation phases, testing strategy
- `compiler/internal/codegen/compiler.go` — codegen entry, type layouts, scope cleanup
- `compiler/internal/codegen/sched.go` — M:N scheduler: GMP structs, sched_loop, park/wake, steal, shutdown
- `compiler/internal/codegen/stmt.go` — statement codegen, drop/close emission, select
- `compiler/internal/codegen/expr.go` — expression codegen, channel send/recv, go blocks
- `compiler/internal/codegen/rtti.go` — RTTI type IDs, `is` checks, view-specific vtables
- `compiler/internal/codegen/mono.go` — monomorphization: collect, declare, define phases
- `compiler/internal/codegen/pal/` — Platform Abstraction Layer (POSIX, Windows, WASM)
- `compiler/internal/sema/check.go` — sema orchestration
- `compiler/internal/sema/info.go` — sema output (type map, objects, lambda captures)
- `compiler/internal/sema/typehash.go` — FNV-128a hash of TypeDecl/EnumDecl AST; populates `Info.DeclHashes` for per-instance cache keys
- `compiler/internal/sema/decl.go` — type/method/func definition passes
- `compiler/internal/types/named.go` — Named type with fields, methods, generics, flags
- `compiler/internal/formatter/formatter.go` — token-based source formatter (lexer + reformatter)
- `compiler/cmd/promise/fmt.go` — `promise format` CLI wiring

## Implementation Philosophy

- **Prefer Promise over IR**: When adding new standard library functionality (e.g., container methods, operators), implement in the Promise language (`modules/std/*.pr`) rather than generating custom LLVM IR in codegen. Only use `native` methods when direct memory access or runtime calls are unavoidable (e.g., `Vector.push`, `Vector.[]`, string byte access). Non-native methods written in Promise are type-checked by sema, monomorphized automatically, and far easier to maintain.
- **Test at every level**: Significant changes need both Go unit tests (`codegen_test.go`, `sema_test.go`) AND Promise-level e2e tests (`tests/` directory, run via `promise test`). Go tests verify IR shape; Promise tests verify runtime correctness.
- **No hidden effects**: When designing language features, avoid implicit side effects, hidden control flow, or magic behaviors. Every effect should be visible at the call site. If a function can fail, it must be marked failable (`!`). If a value is consumed, it must be moved (`~`). If a variable is mutable, it must be declared so.
- **Self-contained by default**: Design features so that programs are understandable in isolation. Avoid global state, implicit initialization, and ambient context. A reader (human or AI) should be able to read a `.pr` file top-to-bottom and know exactly what it does without consulting external docs or hidden configuration.
- **NEVER work around language/compiler/test-infra issues.** When implementing standard library or catalog modules and you hit a language limitation, compiler bug, codegen issue, or test infrastructure problem — **stop and file a bug in the `tracker` MCP server** (type: `bug`). Do NOT hack around the issue in module code (e.g., restructuring code to avoid a parser bug, adding redundant casts to dodge a type-checker gap, duplicating logic because a feature is missing). The language and platform are designed to fully support module implementation — if something doesn't work, that's a real bug that needs a real fix. Let the user implement the compiler/infra fix; your job is to identify and document the blocker clearly.

## Writing Promise Code

- **Always add `` `doc `` annotations on `` `public `` declarations.** Promise's `` `doc `` annotation attaches documentation to types, methods, and functions in the AST. When writing Promise code — especially anything marked `` `public `` — always include a `` `doc `` annotation so AI agents can understand the API without reading implementation details.

- **Prefer batch tests over snapshot tests.** Promise has two test styles:
  - **Batch tests** — functions tagged `` `test `` using `assert()`. All test functions in a file (and across files passed together) compile into a **single binary**, making them significantly faster. This is the preferred style.
    ```
    add() `test {
      assert(1 + 1 == 2);
      assert(3 + 4 == 7);
    }
    ```
  - **Snapshot tests** — `main()` tagged `` `test(expected: "...") ``. Each file compiles into its **own binary** and the stdout output is compared against the expected string. Use only when testing exact program output matters (e.g., print formatting, REPL-style verification).
    ```
    main() `test(expected: "hello world") {
      println("hello world");
    }
    ```
  The cost of running tests is dominated by how many binaries are compiled — test execution itself is practically instant. Batch tests minimize binary count.

- **Co-locate tests with source files.** Place `*_test.pr` files alongside the `.pr` files they test (not in a separate `tests/` tree). This makes tests easier to find and maintains context. The `tests/` directory is for cross-cutting integration and e2e tests that don't belong to a specific module or file.

- **Use full English words in APIs — never abbreviate.** All public names in the standard library and language APIs must use complete, unabbreviated words, even when abbreviations are industry-standard (e.g., `print_line` not `println`, `make_directory` not `mkdir`, `concatenate` not `concat`, `execute` not `exec`, `arguments` not `args`). This optimizes for AI-agent readability — an LLM can always predict the full word but must memorize each abbreviation.

- **Use getters for side-effect-free parameterless access.** Any public function or module-level function that takes no arguments and has no side effects should be a getter (`get name Type`), not a function (`name() Type`). Getters are accessed as properties without parentheses (e.g., `os.hostname`, `os.process_identifier`, `os.environment`). Only use a function when the operation has side effects (e.g., `receive_signal()` blocks on I/O) or takes parameters.

## Bug & Task Tracking

**All bugs, tasks, and deferred items are managed via the `tracker` MCP server.** The tracker auto-assigns IDs by type: `B0001` for bugs, `T0001` for tasks, `D0001` for deferred items. These IDs are stable and can be referenced from code comments, commit messages, and conversations.

- **Filing bugs:** Use `mcp__tracker__create` with `type: "bug"`. Include a clear title, description (what the bug is, any workaround), priority, and relevant tags (e.g., `codegen`, `parser`, `ownership`, `formatter`, `scheduler`).
- **Filing tasks:** Use `type: "task"` for planned work items. Use `type: "deferred"` for items that are not yet scheduled.
- **Querying:** Use `mcp__tracker__list` to filter by type, status, priority, or tag. Use `mcp__tracker__search` for free-text search.
- **Updating:** Use `mcp__tracker__update` to change status (`open` → `in_progress` → `done`/`wontfix`), add notes, or update priority.
- **Reference in code:** Use tracker IDs in code comments (e.g., `// B0030: workaround for optional user type in constructor`) and commit messages.
- **`docs/stages.md`** remains the implementation roadmap and architecture reference. It may reference tracker IDs but is not the source of truth for bug/task status — the tracker is.

## Conventions

- **Document workarounds immediately.** When you encounter a compiler bug, language limitation, or missing feature, file it in the `tracker` MCP server right away (type: `bug`). Include: what the bug is, any workaround, and the priority. Do not leave undocumented workarounds in the code.
- Compiler errors are accumulated (not fatal on first error) and printed together
- `extractNamed(typ)` unwraps Instance/SharedRef/MutRef to get underlying `*types.Named`
- `needsVtable(named)` returns true if type has children or is abstract → virtual dispatch
- `mangleMethodName(owner, name, failable)` produces LLVM function names like `TypeName.method`
- Move sites must call `clearDropFlag(name)` — there are 8 call variant sites in expr.go plus assignment sites in stmt.go
- All tests must pass after changes. Significant changes need accompanying tests.
- When updating `modules/std/*.pr`, run `./build` — it automatically embeds the updated stdlib. The `stdAll` mini-stdlib in test files is auto-populated from the embedded std via `go:embed` — no manual update needed.
