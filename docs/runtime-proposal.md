# Runtime Architecture Proposal

Proposal for Promise's runtime, syscall layer, concurrency model, and multi-platform support.

## Current State

After completing Phases 1-3, the C runtime is reduced to a single function:

**Remaining C runtime** (`runtime/runtime_test.c`, ~25 lines):
- `promise_test_run` — fork-based crash isolation via `fork`/`waitpid` (test mode only)

**Deleted C files**: `runtime.c`, `runtime_string.c`, `runtime_hash.c`, `runtime_vector.c`.

**PAL (Platform Abstraction Layer)** — codegen-emitted LLVM IR (`codegen/pal/`):
- `pal_write(fd, buf, len)` — wraps libc `@write` (PosixPAL)
- `pal_exit(code)` — wraps libc `@exit` (PosixPAL)

**Codegen-emitted print/panic functions** (`codegen/io.go`):
- `promise_print_string`, `promise_print_int`, `promise_print_f64`, `promise_print_bool` — via PAL `pal_write`
- `promise_panic`, `promise_panic_msg` — via PAL `pal_write` + `pal_exit`
- `promise_test_print_result`, `promise_test_summary` — via PAL `pal_write` (test mode)

**Codegen-emitted intrinsic functions** (`codegen/compiler.go`):
- String: `new`, `concat`, `eq`, `trim`, `split`, `next_char`, `eq_string` (map key equality)
- Vector: `with_capacity`, `push`, `pop`, `contains`, `remove`
- RTTI: `type_is`
- Conversion: `int_to_string`, `uint_to_string`, `f64_to_string`, `bool_to_string`, `char_to_string`
- Hash: `hash_string` (FNV-1a for string map keys)

**Pure Promise** (`std/*.pr`):
- Hash: FNV-1a for int/bool/char/float (`std/hash.pr`)
- String methods: `contains`, `starts_with`, `ends_with`, `index_of` (`std/string.pr`)
- Map: full HashMap implementation (`std/map.pr`)

**Libc surface**: `malloc`, `free`, `realloc`, `memcmp`, `snprintf`, `strlen`, `write`, `exit`, `fork`, `waitpid`, `fflush`, `_exit`.

**LLVM intrinsics** (replace libc `memcpy`/`memmove`): `@llvm.memcpy`, `@llvm.memmove`. Note: no `@llvm.memcmp` intrinsic exists.

**LLVM optimizer attributes** on all externs: `noalias`, `nocapture`, `noundef`, `nounwind`, `willreturn`, `readonly`, `argmemonly` (as applicable).

**Build pipeline**: In non-test mode, no C compilation — the `.ll` file contains everything and clang just links it. In test mode, `runtime_test.c` is compiled and linked for fork isolation.

---

## Phased Roadmap

| Phase | Work | Status |
|-------|------|--------|
| **1** | Bitwise operators (`&`, `\|`, `^`, `<<`, `>>`, `~`) | Done |
| **2** | Migrate all computation from C to codegen LLVM IR / pure Promise | Done |
| **3** | PAL abstraction — define interface, implement macOS + Linux | Done |
| **3b** | PAL Windows | **Done** |
| **3c** | PAL WASM (WASI imports + JS FFI) | **Done** |
| **4** | Centralize allocator behind PAL | **Done** |
| **4b** | WASM linear memory allocator (bump/free-list on `memory.grow`) | Planned |
| **5a** | 1:1 threading MVP (`go`/`<-` with OS threads via PAL) | **Done** |
| **5b** | Channels (`channel[T]`, buffered send/receive) | **Done** |
| **5c** | M:N scheduler (GMP model: goroutines, processors, work stealing) | Planned |
| **5d** | Cooperative scheduler for WASM (Asyncify or stack-switching) | Planned |
| **6** | IO reactor: kqueue + epoll + IOCP | Planned |
| **6b** | JS event loop integration for WASM IO | Planned |
| **7** | Replace clang with `llc` + `lld`, enable cross-compilation | Planned |
| **8** | Rewrite scheduler in Promise | Planned |

Phases 1-5b are done. Phase 3 introduced the platform split (PAL). Phase 5a added 1:1 threading (each `go` spawns an OS thread). Phase 5b added typed channels (`channel[T]` with buffered/unbuffered send/receive/for-in and `go { }` block variable capture). Phases 5c-6 add M:N scheduling and IO. Phases 7-8 are polish.

---

## Phase 2 — C-to-Codegen Migration (Done)

Completed in 10 steps. All computation-only C functions replaced with codegen-emitted LLVM IR or pure Promise. Only IO/process functions remain in C.

| Step | What moved | Where it went |
|------|-----------|---------------|
| 1 | Bitwise operators | Codegen native emitter table |
| 2 | Hash (FNV-1a) | `std/hash.pr` (int/bool/char/float), codegen LLVM IR (string) |
| 3 | String methods (contains, starts_with, ends_with, index_of) | `std/string.pr` (pure Promise) |
| 4 | Vector contains/remove | Codegen LLVM IR |
| 5 | int/float/bool/char/uint to string | Codegen LLVM IR (float uses libc `snprintf`) |
| 6 | UTF-8 char encode | Included in step 5 (`defineCharToStringFunc`) |
| 7 | String new/concat | Codegen LLVM IR (uses `@llvm.memcpy`) |
| 8 | Vector with_capacity/push/pop | Codegen LLVM IR (uses `@llvm.memcpy`, `@llvm.memmove`) |
| 9 | String trim/split/next_char | Codegen LLVM IR (split uses libc `memcmp`) |
| 10 | RTTI type_is | Codegen LLVM IR (phi-node loop over parent IDs) |

Also done: LLVM intrinsics for all `memcpy`/`memmove`, libc `memcmp` for equality functions (SIMD-accelerated), optimizer attributes on all externs.

**Remaining opportunity**: Pure Promise string methods in `std/string.pr` still use char-by-char comparison. Could add a `memcmp`-backed `string.eq_region` builtin for acceleration on long strings.

---

## Phase 3 — Platform Abstraction Layer (Done)

The remaining C runtime functions all depend on IO or process control. Phase 3 replaces them with a Platform Abstraction Layer (PAL) — codegen-emitted LLVM IR with per-target implementations, eliminating all C runtime files.

### What Remains in C (to be replaced)

| C function | File | Libc calls | What it does |
|------------|------|------------|--------------|
| `promise_print_int` | `runtime.c` | `printf` | Print int64 + newline to stdout |
| `promise_print_f64` | `runtime.c` | `printf` | Print f64 + newline to stdout |
| `promise_print_bool` | `runtime.c` | `printf` | Print "true"/"false" + newline to stdout |
| `promise_panic` | `runtime.c` | `fprintf`, `exit` | Print "panic: msg" to stderr, exit(1) |
| `promise_panic_msg` | `runtime.c` | `fprintf`, `exit` | Print "panic: msg" (Promise string) to stderr, exit(1) |
| `promise_print_string` | `runtime_string.c` | `fwrite`, `putchar` | Print string bytes + newline to stdout |
| `promise_test_run` | `runtime_test.c` | `fork`, `waitpid`, `fflush`, `_exit` | Fork-isolated test execution |
| `promise_test_print_result` | `runtime_test.c` | `printf` | Print "PASS name" / "FAIL name" |
| `promise_test_summary` | `runtime_test.c` | `printf` | Print "N passed, M failed" |

These are called from:
- **std/io.pr**: `_print_int`, `_print_f64`, `_print_bool`, `_print_string` as `extern` bindings
- **std/assert.pr**: `_panic_msg` as `extern` binding
- **codegen/compiler.go**: `promise_panic` declared as intrinsic (line 324), used for bounds checks, cast failures, OOM
- **codegen/compiler.go**: `GenerateTestMain()` declares test runner functions (lines 231-244)

### How It Works Today

The build pipeline (`cmd/promise/main.go`) compiles C runtime files via clang, then links them with the generated LLVM IR:

```
promise source → codegen → .ll file
runtime/*.c → clang -c → .o files
clang links .ll + .o files → binary
```

The C functions use `printf` format strings internally, but Promise's codegen already knows the exact types (the value-to-string conversions are already codegen-emitted LLVM IR). The C print functions just unpack the value struct and call `printf`. This is what we replace.

### What PAL Does

PAL replaces libc calls with platform-native equivalents. Instead of `printf("%lld\n", x)` (which internally does formatting + `write(1, ...)`), the codegen formats the value to a buffer (already done by `defineIntToStringFunc` etc.) and calls `pal_write(fd, buf, len)` directly.

The key insight: **all remaining C functions reduce to just two primitives** — `write(fd, buf, len)` and `exit(code)`. The `fork`/`waitpid` in the test runner adds a third (`pal_spawn`), but it's only needed for test mode.

### PAL Interface (Go side)

```go
// compiler/internal/codegen/pal/pal.go

// PAL defines platform-specific function emitters.
// Each method emits an LLVM IR function definition into the module.
type PAL interface {
    // EmitWrite emits pal_write(fd i32, buf i8*, len i64) → i64
    EmitWrite(module *ir.Module) *ir.Func

    // EmitExit emits pal_exit(code i32) → void
    EmitExit(module *ir.Module) *ir.Func

    // EmitSpawn emits pal_spawn(fn i8*) → i32 (0=pass, 1=fail)
    EmitSpawn(module *ir.Module) *ir.Func
}

// ForTarget returns the PAL implementation for the given target triple.
func ForTarget(triple string) PAL {
    switch {
    case strings.Contains(triple, "darwin"):
        return &DarwinPAL{}
    case strings.Contains(triple, "linux"):
        return &LinuxPAL{}
    case strings.Contains(triple, "wasm"):
        return &WasmPAL{}
    default:
        return &LinuxPAL{} // fallback
    }
}
```

Each PAL backend emits a **defined** LLVM IR function (not a `declare` — a full `define` with basic blocks), so no C object files are needed. The function bodies contain platform-specific calls or inline syscalls.

### Per-Platform LLVM IR

**`pal_write(fd, buf, len)` — macOS (libSystem.dylib)**:

```llvm
declare i64 @write(i32, i8*, i64)   ; libSystem.dylib export

define i64 @pal_write(i32 %fd, i8* %buf, i64 %len) {
entry:
    %ret = call i64 @write(i32 %fd, i8* %buf, i64 %len)
    ret i64 %ret
}
```

macOS uses libSystem.dylib function calls. Apple does not guarantee raw syscall ABI stability (Go does the same — uses libSystem). The linker resolves `@write` from libSystem at link time.

**`pal_write(fd, buf, len)` — Linux (raw syscall)**:

```llvm
define i64 @pal_write(i32 %fd, i8* %buf, i64 %len) {
entry:
    %fd64 = sext i32 %fd to i64
    %bufint = ptrtoint i8* %buf to i64
    ; syscall(nr=1, fd, buf, len) — write
    %ret = call i64 asm sideeffect "syscall",
        "={rax},{rax},{rdi},{rsi},{rdx}"
        (i64 1, i64 %fd64, i64 %bufint, i64 %len)
    ret i64 %ret
}
```

Linux uses raw syscall instructions. Syscall numbers are stable ABI. `nr=1` is `write` on x86_64. ARM64 uses `svc #0` instead of `syscall`, with `nr=64` for write.

**`pal_exit(code)` — macOS (libSystem.dylib)**:

```llvm
declare void @exit(i32)   ; libSystem.dylib export

define void @pal_exit(i32 %code) {
entry:
    call void @exit(i32 %code)
    unreachable
}
```

**`pal_exit(code)` — Linux (raw syscall)**:

```llvm
define void @pal_exit(i32 %code) {
entry:
    %code64 = sext i32 %code to i64
    ; syscall(nr=231, code) — exit_group
    call void asm sideeffect "syscall",
        "{rax},{rdi}"(i64 231, i64 %code64)
    unreachable
}
```

**`pal_spawn(fn)` — macOS/Linux (fork + waitpid)**:

Initially keep as a C function or use libSystem/libc calls. Raw `fork` syscall is possible on Linux but the waitpid + status macro logic is nontrivial in pure IR. Can be migrated later or kept as the single remaining C function until Phase 5 (concurrency) provides thread-based isolation.

### Migration: C Functions → PAL + Codegen

Each C print function becomes a codegen-emitted LLVM IR function that formats the value, then calls `pal_write`:

| C function | Replacement strategy |
|------------|---------------------|
| `promise_print_int` | Codegen: call existing `promise_int_to_string`, extract buf/len, call `pal_write(1, buf, len)`, write `\n` |
| `promise_print_f64` | Codegen: call existing `promise_f64_to_string`, extract buf/len, call `pal_write(1, buf, len)`, write `\n` |
| `promise_print_bool` | Codegen: call existing `promise_bool_to_string`, extract buf/len, call `pal_write(1, buf, len)`, write `\n` |
| `promise_print_string` | Codegen: extract `data`/`len` from string instance, call `pal_write(1, data, len)`, write `\n` |
| `promise_panic` | Codegen: write "panic: " + msg to fd 2 (stderr), call `pal_exit(1)` |
| `promise_panic_msg` | Codegen: write "panic: " + string data to fd 2, call `pal_exit(1)` |
| `promise_test_run` | Keep as C initially (fork/waitpid); replace in Phase 5 with thread-based isolation |
| `promise_test_print_result` | Codegen: format "PASS name\n" / "FAIL name\n", call `pal_write(1, ...)` |
| `promise_test_summary` | Codegen: format "N passed, M failed\n", call `pal_write(1, ...)` |

The `_to_string` codegen functions (already implemented in `compiler.go`) produce a string `i8*` with `{len, data}`. The print functions just need to extract these fields and pass them to `pal_write`. The value struct unpacking that currently happens in C (`x->raw`) moves into the codegen-emitted function — the codegen already knows the struct layout.

### Newline Handling

Current C functions append `\n` via `printf` format strings. In PAL, two options:

1. **Two writes**: `pal_write(fd, data, len)` then `pal_write(fd, "\n", 1)` — simple, two syscalls
2. **Buffer copy**: copy string + `\n` into a stack buffer, single `pal_write` — one syscall, small alloca

Option 2 is better for performance. The codegen allocates a stack buffer (`alloca [N x i8]` or dynamically sized via alloca), copies the string data + newline byte, calls `pal_write` once. For short strings (most `println` calls), this is a single `alloca` + `memcpy` + `store \n` + `pal_write`.

### File Descriptors

POSIX and Windows both use integer-like handles for IO. PAL uses `i32` file descriptors:

| Constant | POSIX | Windows | WASM |
|----------|-------|---------|------|
| `fd 0` (stdin) | 0 | `GetStdHandle(STD_INPUT_HANDLE)` | 0 (WASI) |
| `fd 1` (stdout) | 1 | `GetStdHandle(STD_OUTPUT_HANDLE)` | 1 (WASI) |
| `fd 2` (stderr) | 2 | `GetStdHandle(STD_ERROR_HANDLE)` | 2 (WASI) |

On Windows, `pal_write` maps fd 0/1/2 to the corresponding HANDLE, then calls `WriteFile`. On WASM/WASI, fd 1/2 map to WASI's `fd_write` with the same integers. On WASM/browser, a JS-provided import handles the mapping.

### Step-by-Step Implementation Plan

**Step 1: PAL infrastructure + `pal_write` + `pal_exit` (macOS + Linux)**

Create `compiler/internal/codegen/pal/` package:
- `pal.go` — `PAL` interface with `EmitWrite`, `EmitExit`
- `pal_darwin.go` — `DarwinPAL` using libSystem `@write`, `@exit`
- `pal_linux.go` — `LinuxPAL` using raw syscall instructions

Update `compiler.go`:
- Add `pal PAL` field to `Compiler`
- `Compile()` calls `pal.ForTarget(triple)` to select backend
- `declareIntrinsics()` calls `c.pal.EmitWrite(module)`, `c.pal.EmitExit(module)`

**Step 2: Migrate print functions to codegen LLVM IR**

Add to `compiler.go` (or new `io.go`):
- `definePrintStringFunc()` — extract `data`/`len` from string instance, alloca buffer, append `\n`, call `pal_write(1, ...)`
- `definePrintIntFunc()` — call `promise_int_to_string`, then print
- `definePrintF64Func()` — call `promise_f64_to_string`, then print
- `definePrintBoolFunc()` — call `promise_bool_to_string`, then print

These replace the `extern` declarations. The std/io.pr `extern` bindings still work — same function names, same signatures — but the functions are now codegen-defined instead of C-provided.

**Step 3: Migrate panic to codegen LLVM IR**

- `definePanicFunc()` — write "panic: " prefix + msg to fd 2 (stderr), call `pal_exit(1)`
- `definePanicMsgFunc()` — extract string data, write to stderr, call `pal_exit(1)`

**Step 4: Migrate test runner print functions**

- `defineTestPrintResultFunc()` — format + `pal_write`
- `defineTestSummaryFunc()` — format + `pal_write`

**Step 5: Remove C runtime files**

After steps 1-4, the only C function left is `promise_test_run` (fork/waitpid isolation). Options:
- Keep `runtime_test.c` with only `promise_test_run` (3 lines of C)
- Delete all other `.c` files, `promise_bindings.h`
- Update build pipeline to skip clang compilation when no C files remain (test mode excepted)

**Step 6: Migrate test runner (optional, can defer to Phase 5)**

Replace `fork`/`waitpid` with:
- macOS: `posix_spawn` via libSystem (simpler than raw fork)
- Linux: raw `clone` + `wait4` syscalls
- Or defer entirely — thread-based isolation in Phase 5 replaces fork

### Build Pipeline Changes

**Before PAL** (current):
```
.pr → codegen → .ll
runtime/*.c → clang -c → .o files
clang links .ll + .o → binary
```

**After PAL** (Phase 3 complete):
```
.pr → codegen (with PAL) → .ll     (all runtime functions are in the .ll)
clang links .ll → binary            (no .o files, clang just links)
```

The generated `.ll` file contains everything: user code, intrinsic functions, PAL functions. Clang is only needed as a linker (resolves libSystem on macOS). On Linux with raw syscalls, this is the path to replacing clang with `llc` + `lld` (Phase 7).

### Directory Structure

```
compiler/internal/codegen/
├── pal/
│   ├── pal.go          # PAL interface + ForTarget() selector
│   ├── darwin.go       # DarwinPAL: libSystem.dylib (write, exit)
│   └── linux.go        # LinuxPAL: raw syscall (write, exit_group)
├── io.go               # definePrintStringFunc, definePrintIntFunc, etc.
├── compiler.go         # PAL field, wiring into declareIntrinsics
└── ...existing files...
```

Windows and WASM PAL backends added in Phases 3b/3c. The `pal.go` interface grows as needed (Threading, IO Reactor functions added in Phases 5-6).

### Target Platforms

| Feature | macOS | Linux | Windows | WASM (browser) |
|---------|-------|-------|---------|----------------|
| Syscall style | libSystem.dylib | raw `syscall` | kernel32.dll (Win32 API) | host imports (JS FFI / WASI) |
| IO multiplex | kqueue | epoll | IOCP | event loop (JS) |
| Memory pages | mmap/munmap | mmap/munmap | VirtualAlloc/VirtualFree | memory.grow |
| Threads | pthread | pthread/clone | CreateThread | **none** (single-threaded) |
| Process spawn | fork/exec | fork/exec | CreateProcess | **none** |
| Linking | Mach-O | ELF | PE/COFF | WASM binary |
| Address size | 64-bit | 64-bit | 64-bit | **32-bit** |

### Architecture: 5 Layers

```
┌──────────────────────────────────────────────┐
│  Layer 4: Standard Library (Pure Promise)    │  map, iter, streams, crypto, compress...
├──────────────────────────────────────────────┤
│  Layer 3: Concurrency Runtime (Go/LLVM IR)   │  scheduler, green threads, IO reactor
├──────────────────────────────────────────────┤
│  Layer 2: Core Types (Promise + builtins)    │  string, vector, hash, formatting
├──────────────────────────────────────────────┤
│  Layer 1: Memory + Intrinsics (LLVM IR)      │  allocator, memcpy, memset
├──────────────────────────────────────────────┤
│  Layer 0: Platform Abstraction Layer (PAL)   │  write, exit, spawn, (later: mmap, kevent...)
└──────────────────────────────────────────────┘
```

### Platform Notes

- **macOS**: Apple discourages raw syscalls and doesn't guarantee ABI stability. Use libSystem.dylib (Go does the same). `@write`, `@exit`, `@fork`, `@waitpid` are all resolved via libSystem.
- **Linux**: raw syscalls are stable and preferred. `syscall` instruction on x86_64, `svc #0` on ARM64. Syscall numbers differ between architectures.
- **Windows**: fundamentally different API surface (Win32). `WriteFile` instead of `write`, `ExitProcess` instead of `exit`. File descriptors → HANDLE mapping needed.
- **WASM**: 32-bit pointers, no threads, no filesystem without WASI. Codegen needs a `ptrSize` constant instead of hardcoded 8. No fork/process isolation — test runner runs functions directly.

### Layer 0: Future PAL Functions

Phase 3 starts with just `pal_write`, `pal_exit`, and `pal_spawn`. The full PAL grows as later phases need more primitives:

**Phase 4 — Memory** (currently uses libc `malloc`/`free`/`realloc`):
```
pal_alloc(size) → ptr
pal_free(ptr, size)
pal_realloc(ptr, old_size, new_size) → ptr
```
- macOS/Linux: mmap/munmap (or keep malloc initially)
- Windows: VirtualAlloc/VirtualFree (or HeapAlloc initially)
- WASM: bump allocator on linear memory using `memory.grow`

**Phase 5a — Threading** (Done — 1:1 model, WASM stubs run synchronously):
```
pal_thread_create(fn, arg) → handle
pal_thread_join(handle)
pal_mutex_init() → handle
pal_mutex_lock(handle)
pal_mutex_unlock(handle)
pal_mutex_destroy(handle)
pal_cond_init() → handle
pal_cond_wait(cond, mutex)
pal_cond_signal(cond)
pal_cond_broadcast(cond)
pal_cond_destroy(cond)
```
- macOS/Linux: pthread_create/join + pthread_mutex + pthread_cond
- Windows/WASM: synchronous stubs (call fn directly, no-op mutex/cond)

`go expr` spawns an OS thread; `<-task` joins on it. Task struct layout: `{ T result, i1 done, i8* mutex, i8* cond, i8* thread_handle }`. Test runner (`promise_test_run`) migrated from fork/C to thread-based codegen — no C runtime files remain.

**Phase 5b — Channels** (Done):

`channel[T]` is a native type with buffered/unbuffered send/receive. Heap-allocated struct with mutex + 2 cond vars protecting a ring buffer:
```
{ i8* buffer, i64 elem_size, i64 capacity, i64 count, i64 head, i64 tail,
  i8 closed, i8 unbuffered, i8* mutex, i8* not_empty, i8* not_full }
```

Operations:
- `channel[T](capacity: N)` — constructor. N=0 (default) creates unbuffered (rendezvous) channel.
- `ch.send(value)` — lock, wait-if-full, memcpy to ring buffer, signal, rendezvous wait if unbuffered, unlock. Panics on send-to-closed.
- `<-ch` — lock, wait-while-empty, if closed+empty return `none`, else read and return `Some(value)`, unlock. Returns `T?`.
- `ch.close()` — lock, set closed, broadcast both conds, unlock. Panics on double-close.
- `for v in ch { }` — loop receiving until closed+empty.

`go { block }` blocks capture outer local variables by value — the codegen walks the block AST to identify referenced outer locals, packs them into the thread arg struct, and unpacks them as parameters in the thunk function.

**Phase 6 — IO Reactor** (for non-blocking IO):
```
pal_reactor_create() → handle
pal_reactor_add(reactor, fd, events)
pal_reactor_wait(reactor, events_out, timeout) → count
```
- macOS: kqueue + kevent
- Linux: epoll_create + epoll_ctl + epoll_wait
- Windows: CreateIoCompletionPort + GetQueuedCompletionStatus
- WASM: not applicable — use JS event loop integration

**IO** (added when file/network IO is exposed to Promise):
```
pal_read(fd, buf, len) → bytes_read
pal_open(path, flags, mode) → fd
pal_close(fd) → err
pal_stat(path, buf) → err
```

### Layer 1: Memory + Intrinsics

LLVM intrinsics (already done): `@llvm.memcpy`, `@llvm.memmove`, `@llvm.memset`.

Memory allocator phasing:
1. **Now**: Keep `malloc`/`free` with LLVM optimizer attributes
2. **Phase 4**: Route through `pal_alloc`/`pal_free` wrappers
3. **Later**: mmap-based arena allocator (macOS/Linux), VirtualAlloc-based (Windows), bump allocator (WASM)
4. **Much later**: Per-goroutine allocation pools once concurrency lands

### Layer 3: Concurrency — Platform-Adaptive

Two modes because WASM cannot do threads:

**Mode 1: M:N Scheduler** (macOS, Linux, Windows)
```
┌─────────────────────────────┐
│    G (goroutines)           │  Lightweight, stackful coroutines
│    P (processors)           │  Per-CPU run queues + work stealing
│    M (OS threads)           │  Platform thread (pthread / CreateThread)
│    IO Reactor               │  kqueue / epoll / IOCP
└─────────────────────────────┘
```

Go's GMP model. The IO reactor is the only platform-specific piece — abstracted behind PAL.

**Mode 2: Cooperative Scheduler** (WASM)
```
┌─────────────────────────────┐
│    G (goroutines)           │  Stackful coroutines via stack switching
│    Single "M"               │  The browser's main thread
│    JS Event Loop            │  Yields back to browser for IO
└─────────────────────────────┘
```

- No parallelism — all goroutines run cooperatively on one thread
- `go {}` creates a new G, runs when current G yields (at IO, channel ops, or explicit yield points)
- IO through JS interop: scheduler yields to browser event loop, JS calls back when IO completes
- WASM stack switching proposal would make this cleaner; initially use Asyncify or manual CPS transform

**From the Promise programmer's perspective**: `go`, `task`, `channel`, `<-` work identically on all platforms. Native targets run goroutines on multiple cores, WASM runs them cooperatively on one thread. Same semantics, different performance.

**Core components**:
- **G** (goroutine): lightweight context with small stack (2-8KB, growable), saved registers, status
- **M** (machine): OS thread
- **P** (processor): logical CPU with run queue. Count = GOMAXPROCS

**Stack management**: Small initial stack per goroutine. Function entry prologues check remaining space; if insufficient, allocate larger stack and copy (Go's copyable stack approach).

**IO integration**: All IO goes through the scheduler:
1. Goroutine calls `write(fd, ...)`; runtime tries non-blocking write
2. If it would block: register fd with kqueue/epoll/IOCP, park the goroutine, run another G on this M
3. When IO reactor signals readiness, unpark the goroutine

This gives **no function coloring** — every function looks synchronous.

**Implementation language**: Start with C for the scheduler, migrate to Promise once the language has unsafe pointers, inline assembly, and atomic ops.

### Layer 4: Standard Library (Pure Promise)

Everything built on top of Layers 0-3: map (already done), iterators, streams, crypto, compression, networking, etc. All platform-independent.

---

## Toolchain: Removing Clang

| Target | Current | Without Clang |
|--------|---------|---------------|
| macOS | clang (Xcode) | `llc` + `ld64.lld` (or Xcode's ld) |
| Linux | clang | `llc` + `ld.lld` |
| Windows | clang (MSVC/MinGW) | `llc` + `lld-link` |
| WASM | clang --target=wasm32 | `llc` + `wasm-ld` |

`lld` (LLVM's linker) supports all four output formats: Mach-O, ELF, PE/COFF, and WASM.

**Path**:
1. Ship `llc` + `lld` alongside the `promise` binary (or statically link via LLVM C API)
2. Compiler emits `.ll` text → `llc` produces `.o` → `lld` links to final binary
3. Cross-compilation becomes trivial: `promise build --target wasm32 app.pr`

---

## Key Design Decisions

1. **Layer 0 is a PAL, not raw syscalls.** Windows and WASM make raw syscalls impractical as the universal approach. The PAL is a thin abstraction (~25 functions) with per-target codegen.
2. **Two concurrency modes.** WASM can't do threads, so the scheduler supports both M:N (native) and cooperative (WASM). Language semantics stay the same.
3. **WASM is 32-bit pointers.** Codegen needs `ptrSize` abstraction instead of assuming 64-bit.
4. **Test runner can't use fork on Windows/WASM.** Alternatives: threads on Windows, no isolation on WASM.
5. **Cross-compilation comes naturally.** PAL per-target + `llc`/`lld` enables `promise build --target wasm32 app.pr`.
6. **macOS keeps using libSystem.** Apple doesn't guarantee raw syscall ABI stability. Go does the same.
7. **LLVM integration.** Keep text IR (`llir/llvm`) for now; switch to LLVM C API when removing clang becomes a priority.
