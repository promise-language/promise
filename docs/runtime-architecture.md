# Runtime Architecture

Promise's runtime infrastructure: PAL (platform abstraction), build pipeline (opt/llc/lld/musl), M:N scheduler (GMP model, LLVM coroutines), and multi-platform support.

## Current State

No C runtime files remain. All runtime functions are codegen-emitted LLVM IR or pure Promise (including `promise_test_run`, `promise_test_print_result`, `promise_test_summary`).

**Deleted C files**: `runtime.c`, `runtime_string.c`, `runtime_hash.c`, `runtime_vector.c`, `runtime_test.c`.

**PAL (Platform Abstraction Layer)** — codegen-emitted LLVM IR (`codegen/pal/`):
- `pal_write(fd, buf, len)` — wraps libc `@write` (PosixPAL)
- `pal_exit(code)` — wraps libc `@exit` (PosixPAL)

**Codegen-emitted print/panic functions** (`codegen/io.go`):
- `promise_print_string` — via PAL `pal_write`
- `promise_panic`, `promise_panic_msg` — via PAL `pal_write` + `pal_exit`
- `promise_test_print_result`, `promise_test_summary` — via PAL `pal_write` (test mode)

**Codegen-emitted intrinsic functions** (`codegen/compiler.go`):
- String: `new`, `concat`, `eq`, `trim`, `split`, `next_char`, `eq_string` (map key equality)
- Vector: `with_capacity`, `push`, `pop`, `contains`, `remove`
- RTTI: `type_is`
- Conversion: `int_to_string`, `uint_to_string`, `f64_to_string`, `bool_to_string`, `char_to_string`
- Hash: `hash_string` (FNV-1a for string map keys)

**Pure Promise** (`modules/std/*.pr`):
- Hash: FNV-1a for int/bool/char/float (`modules/std/hash.pr`)
- String methods: `contains`, `starts_with`, `ends_with`, `index_of` (`modules/std/string.pr`)
- Map: full HashMap implementation (`modules/std/map.pr`)

**Libc surface**: `malloc`, `free`, `realloc`, `memcmp`, `snprintf`, `strlen`, `write`, `exit`, `fork`, `waitpid`, `fflush`, `_exit`.

**LLVM intrinsics** (replace libc `memcpy`/`memmove`): `@llvm.memcpy`, `@llvm.memmove`. Note: no `@llvm.memcmp` intrinsic exists.

**LLVM optimizer attributes** on all externs: `noalias`, `nocapture`, `noundef`, `nounwind`, `willreturn`, `readonly`, `argmemonly` (as applicable).

**Build pipeline**: No C compilation — the `.ll` file contains everything. On Linux, `opt -O1` + `llc` + `ld.lld` compile and link with bundled musl CRT (Phase 7b/7b'). On macOS, `opt -O1` + `llc` + system `ld` (or `ld64.lld`) with `-lSystem -syslibroot` (Phase 7c). On WASM, `opt -O1` + `llc` + `wasm-ld` with custom bump allocator and cooperative scheduler — no libc dependency (Phases 4b/5d/7a). On other platforms (or `PROMISE_USE_CLANG=1`), clang acts as driver. Requires LLVM 22+.

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
| **4b** | WASM linear memory allocator (bump on `memory.grow`, no libc) | **Done** |
| **5a** | 1:1 threading MVP (`go`/`<-` with OS threads via PAL) | **Done** |
| **5b** | Channels (`channel[T]`, buffered send/receive) | **Done** |
| **5c** | M:N scheduler (GMP model, LLVM coroutines, work stealing) | **Done** |
| **5d** | Cooperative scheduler for WASM (LLVM coroutines, single-threaded) | **Done** |
| **6** | IO reactor: kqueue + epoll + IOCP | Planned |
| **6b** | JS event loop integration for WASM IO | Planned |
| **7a** | WASM: `opt` + `llc` + `wasm-ld` (no CRT) + `--target wasm32-wasi` | **Done** |
| **7b** | Linux: `opt` + `llc` + `ld.lld` (system glibc CRT) | **Done** |
| **7b'** | Linux: bundled musl CRT (fully static binaries) | **Done** |
| **7c** | macOS: `llc` + system `ld` (SDK sysroot) | **Done** |
| **7d** | Windows: `llc` + `lld-link` (MSVC paths) | Planned |
| **7e** | `--target` flag + cross-compilation | Planned |
| **7f** | Self-contained binary: embed gzip-compressed LLVM tools via `go:embed` | **Done** |
| **8** | Rewrite scheduler in Promise | Planned |

Phases 1-5d, 4b, 7a, 7b, 7b', 7c, and 7f are done. Phase 3 introduced the platform split (PAL). Phase 5a added 1:1 threading (each `go` spawns an OS thread). Phase 5b added typed channels (`channel[T]` with buffered/unbuffered send/receive/for-in and `go { }` block variable capture). Phase 5c replaced 1:1 threading with an M:N scheduler using LLVM coroutine intrinsics — goroutines are cheap coroutine handles multiplexed on OS threads via per-CPU processors and work stealing. Phase 4b added a WASM bump allocator (later replaced by a free-list allocator). Phase 5d added a cooperative scheduler for WASM (single-threaded, no atomics). Phase 7a added the WASM build pipeline (`opt` + `llc` + `wasm-ld`). Phase 7b replaced clang with `opt` + `llc` + `ld.lld` on Linux with system glibc CRT. Phase 7b' bundled musl libc CRT objects via `go:embed`, making fully static binaries the default on Linux — target triple is now `x86_64-unknown-linux-musl`; clang remains as fallback via `PROMISE_USE_CLANG=1`. Phase 7c added macOS opt+llc+system ld pipeline. Phase 7f embeds gzip-compressed LLVM tools (opt, llc, lld, libLLVM.so) in the Go binary for release builds (`go build -tags embed_llvm`), making the promise binary fully self-contained on Linux (~61MB). Phase 6 (IO reactor) and remaining Phase 7 (d-e) are planned. Phase 8 is polish.

---

## Phase 2 — C-to-Codegen Migration (Done)

Completed in 10 steps. All computation-only C functions replaced with codegen-emitted LLVM IR or pure Promise. Only IO/process functions remain in C.

| Step | What moved | Where it went |
|------|-----------|---------------|
| 1 | Bitwise operators | Codegen native emitter table |
| 2 | Hash (FNV-1a) | `modules/std/hash.pr` (int/bool/char/float), codegen LLVM IR (string) |
| 3 | String methods (contains, starts_with, ends_with, index_of) | `modules/std/string.pr` (pure Promise) |
| 4 | Vector contains/remove | Codegen LLVM IR |
| 5 | int/float/bool/char/uint to string | Codegen LLVM IR (float uses libc `snprintf`) |
| 6 | UTF-8 char encode | Included in step 5 (`defineCharToStringFunc`) |
| 7 | String new/concat | Codegen LLVM IR (uses `@llvm.memcpy`) |
| 8 | Vector with_capacity/push/pop | Codegen LLVM IR (uses `@llvm.memcpy`, `@llvm.memmove`) |
| 9 | String trim/split/next_char | Codegen LLVM IR (split uses libc `memcmp`) |
| 10 | RTTI type_is | Codegen LLVM IR (phi-node loop over parent IDs) |

Also done: LLVM intrinsics for all `memcpy`/`memmove`, libc `memcmp` for equality functions (SIMD-accelerated), optimizer attributes on all externs.

**Remaining opportunity**: Pure Promise string methods in `modules/std/string.pr` still use char-by-char comparison. Could add a `memcmp`-backed `string.eq_region` builtin for acceleration on long strings.

---

## Phase 3 — Platform Abstraction Layer (Done)

The remaining C runtime functions all depend on IO or process control. Phase 3 replaces them with a Platform Abstraction Layer (PAL) — codegen-emitted LLVM IR with per-target implementations, eliminating all C runtime files.

### What Remains in C (to be replaced)

| C function | File | Libc calls | What it does |
|------------|------|------------|--------------|
| `promise_panic` | `runtime.c` | `fprintf`, `exit` | Print "panic: msg" to stderr, exit(1) |
| `promise_panic_msg` | `runtime.c` | `fprintf`, `exit` | Print "panic: msg" (Promise string) to stderr, exit(1) |
| `promise_print_string` | `runtime_string.c` | `fwrite`, `putchar` | Print string bytes + newline to stdout |
| `promise_test_run` | `runtime_test.c` | `fork`, `waitpid`, `fflush`, `_exit` | Fork-isolated test execution |
| `promise_test_print_result` | `runtime_test.c` | `printf` | Print "PASS name" / "FAIL name" |
| `promise_test_summary` | `runtime_test.c` | `printf` | Print "N passed, M failed" |

These are called from:
- **std/io.pr**: `_print_string` as `extern` binding
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
// Currently 32 methods: write, exit, alloc, free, realloc, 11 threading/sync,
// num_cpus, and 12 file I/O (open, read, write, close, seek, stat_size,
// remove, exists, mkdir, dir_remove, dir_exists, errno).
type PAL interface {
    EmitWrite(module *ir.Module) *ir.Func     // pal_write(fd, buf, len) → i64
    EmitExit(module *ir.Module) *ir.Func      // pal_exit(code) → void [noreturn]
    EmitAlloc(module *ir.Module) *ir.Func     // pal_alloc(size) → i8*
    EmitFree(module *ir.Module) *ir.Func      // pal_free(ptr) → void
    EmitRealloc(module *ir.Module) *ir.Func   // pal_realloc(ptr, size) → i8*
    // ... threading (11 methods), num_cpus, file I/O (12 methods)
}

// ForTarget returns the PAL implementation for the given target triple.
func ForTarget(triple string) PAL {
    switch {
    case strings.Contains(triple, "windows"):
        return &WindowsPAL{}
    case strings.Contains(triple, "wasm"):
        return &WasmPAL{}
    default:
        return &PosixPAL{target: triple}
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
| `promise_print_string` | **Done.** Codegen: extract `data`/`len` from string instance, call `pal_write(1, data, len)`. Newline handling done at Promise level (`print_line` appends `Platform.line_separator` to the Builder before calling this). |
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

Option 2 is better for performance. The codegen allocates a stack buffer (`alloca [N x i8]` or dynamically sized via alloca), copies the string data + newline byte, calls `pal_write` once. For short strings (most `print_line` calls), this is a single `alloca` + `memcpy` + `store \n` + `pal_write`.

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
│   ├── pal.go          # PAL interface + ForTarget() + shared stubs
│   ├── posix.go        # PosixPAL: libc wrappers (macOS + Linux)
│   ├── windows.go      # WindowsPAL: Win32 API + UCRT wrappers
│   ├── wasm.go         # WasmPAL: WASI imports + stubs
│   └── pal_test.go     # Cross-platform IR shape tests
├── io.go               # definePrintStringFunc, definePrintIntFunc, etc.
├── compiler.go         # PAL field, wiring into declareIntrinsics
└── ...existing files...
```

Three PAL backends: PosixPAL (macOS + Linux via libc), WindowsPAL (Win32 API + UCRT), WasmPAL (WASI imports + stubs). The interface has grown to 32 methods covering memory, threading, sync, CPU count, and file I/O.

### Target Platforms

| Feature | macOS | Linux | Windows | WASM (browser) |
|---------|-------|-------|---------|----------------|
| Syscall style | libSystem.dylib (libc) | musl/glibc (libc) | kernel32.dll + UCRT | WASI imports |
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
- macOS/Linux: pthread_create/join + pthread_mutex + pthread_cond (explicit 2MB thread stack via pthread_attr_setstacksize — musl defaults to 128KB)
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

**Phase 5c — M:N Scheduler** (Done):

Replaces 1:1 threading with Go-style M:N scheduling. Each `go` expression creates a lightweight goroutine (G) — an LLVM coroutine handle plus metadata (~72 bytes) — multiplexed on a pool of OS threads (M) via per-CPU processors (P) with work stealing.

Architecture (GMP model):
- **G (Goroutine)**: LLVM coroutine handle + status + wait data + scheduler linkage. Statuses: idle/runnable/running/waiting/dead.
- **P (Processor)**: Per-CPU logical processor with a 256-slot ring buffer run queue (lock-free single-producer/single-consumer).
- **M (Machine)**: OS thread bound to a P. Pool is elastic — sysmon spawns extra Ms when all are blocked.
- **Sched (Global)**: Singleton with global run queue, idle M list, P array, goroutine counter.

LLVM coroutine pattern: `go` blocks compile as `presplitcoroutine` functions using `llvm.coro.*` intrinsics (id, alloc, begin, suspend, end, free, resume, destroy, done). Each coroutine has an initial suspend (wait to be scheduled) and a final suspend (keep frame alive so scheduler can check `coro.done()`). Clang runs with `-O1` to ensure CoroSplit lowering.

Dual-mode channel operations: inside a coroutine body (`c.inCoroutine`), channel send/receive parks the goroutine on the channel's wait list and calls `coro.suspend`. Outside coroutine context (e.g. `main()` before scheduler wrapping or nested non-coroutine calls), the existing mutex+cond_wait blocking mode is used. Both modes coexist on the same channel.

Task receive (`<-task`): in coroutine mode, parks the current G on the target G's `done_waiters` list and suspends. In thread-blocking mode, polls `G.done` with `usleep(100)`.

Scheduler lifecycle: `main()` is wrapped as goroutine G0. `promise_sched_init` creates P×M worker threads running `promise_sched_loop`. Each M loops: find_runnable (local queue → global queue → work steal) → `coro.resume(G)` → check `coro.done` → goroutine_exit or back to loop. `promise_sched_run_until_main` blocks the initial thread until G0 completes, then `promise_sched_shutdown` signals and joins all Ms.

New files: `codegen/sched.go` (~1100 lines) — all scheduler data structures and functions.

Key implementation details:
- Channel struct extended with 4 waiter list fields (send/recv head/tail) appended to existing 11 fields.
- Thread-local `@__promise_current_g` stores the running G pointer for channel ops.
- `pal_num_cpus()` via `sysconf(_SC_NPROCESSORS_ONLN)` determines P count.
- Sysmon thread with 1ms polling ensures progress when all Ms are blocked in nested calls.
- Void tasks use a sentinel `result_ptr` (0x1) so `goroutine_exit` knows not to free G (caller frees via `<-task`).

Known issue — **enqueue-before-suspend race**: coroutine-mode channel parks and `<-task` done_waiters add the goroutine to a wait list before calling `coro.suspend`. Another thread can dequeue the goroutine and call `coro.resume` before the suspend completes, causing UB (SIGSEGV/SIGBUS). The window is small (~2-3 instructions) but reproducible under contention. Fix requires **deferred parking**: the goroutine stores park info in G fields, suspends, and the scheduler loop completes the park after `coro.resume` returns. Current workaround: use buffered channels with sufficient capacity so coroutine-mode parks are rare; `main()` channel ops use the safe thread-blocking path.

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

## Phase 7 — Replace Clang with `opt` + `llc` + `lld`

### Why

Clang is currently used as a convenience driver — it compiles LLVM IR, runs optimization passes, and links in one command. But it pulls in a full C compiler toolchain that Promise doesn't need, can't cross-compile without a cross-toolchain, and hides platform linking details that become problems when targeting multiple platforms from one host.

`opt` (LLVM optimizer), `llc` (LLVM static compiler), and `lld` (LLVM linker) are standalone tools that handle exactly what Promise needs: optimization, IR-to-object codegen, and linking. `llc` and `lld` support all targets in a single binary, enabling cross-compilation without additional toolchains.

### Phase 7b — Linux Pipeline (Done)

On Linux, `compileAndLink()` dispatches to the LLVM pipeline by default. Non-Linux platforms and `PROMISE_USE_CLANG=1` use the clang fallback.

**Pipeline** (non-Windows):
```
promise.ll → opt -O1 → promise.bc → ld.lld --lto-O1 + CRT → binary
```

Two steps (no `llc` for non-Windows):
1. **`opt -O1`** — run optimization passes including CoroSplit/CoroElide (coroutine lowering), emit LLVM bitcode (`.bc`). `opt` is needed because `llc` alone doesn't run module-level optimization passes.
2. **`ld.lld --lto-O1`** — link bitcode files with link-time optimization. LTO performs cross-module inlining and dead code elimination at IR level across all modules (main + std + user modules). Produces the final ELF binary.

**Windows pipeline** (`opt → llc → .o → lld-link`):
```
promise.ll → opt -O1 → promise.bc → llc -filetype=obj → promise.o → lld-link → binary
```
Windows uses the native object pipeline because MSVC COFF LTO is not yet wired up.

**Code** (`cmd/promise/main.go`):
- `compileAndLink()` — writes `.ll` to temp file, dispatches to LLVM or clang pipeline based on target
- `useClangPipeline(target)` — returns true for non-Linux targets or when `PROMISE_USE_CLANG=1`
- `compileAndLinkLLVM()` — the `opt → .bc → linker --lto-O1` pipeline (non-Windows) or `opt → llc → .o → lld-link` (Windows)
- `compileLLToBC(irText, prefix, optPath)` — runs `opt -O1` on IR text, returns `.bc` temp file path
- `compileAndLinkClang()` — the old clang driver path (fallback)
- `findLLVMTool(name)` — discovers `opt`/`llc`/`ld.lld` (sibling of binary → env override → versioned PATH → unversioned PATH)
- `findCRT(target)` — discovers system glibc CRT objects via `cc -print-file-name` with fallback path probing
- `buildLinuxLinkArgs()` — builds the full `ld.lld` argument list (includes `--lto-O1`)

**LLVM version**: requires LLVM 22+. The generated IR uses `llvm.coro.end → void` (LLVM 22+ signature). All tools (`opt`, `llc`, linker) are version-checked.

### Prerequisites

**Linux** (building from source):
```bash
# Automated (installs LLVM 22+, musl-dev, adds apt.llvm.org if needed):
sudo bin/install-prereqs.sh

# Or manually — Debian/Ubuntu (use highest available LLVM >= 22):
sudo apt install llvm-23 lld-23 musl-dev   # from apt.llvm.org

# Fedora
sudo dnf install llvm lld musl-devel

# Arch
sudo pacman -S llvm lld musl
```

Provides: `opt`, `llc`, `ld.lld` (LLVM 22+ tools) and musl CRT objects (`crt1.o`, `crti.o`, `crtn.o`, `libc.a`). The `musl-dev` package is required — musl CRT objects are embedded in the Go binary via `go:embed` during `make build`. The `build-essential` package is no longer required for building Promise programs (glibc CRT is only used with `PROMISE_USE_CLANG=1`). The LLVM apt repository ([apt.llvm.org](https://apt.llvm.org)) is needed on Ubuntu/Debian since stock repos may not have LLVM 22+.

**macOS** (Phase 7c — opt + llc + system ld):
```bash
brew install llvm    # provides opt + llc (LLVM 22+)
xcode-select --install  # provides macOS SDK + system ld
```

**Windows** (clang fallback, Phase 7d pending):
```bash
choco install llvm   # or winget install llvm
# + Visual Studio Build Tools for CRT libs
```

### Environment Variables

| Variable | Purpose | Default |
|----------|---------|---------|
| `PROMISE_USE_CLANG` | Set to `1` to force the clang pipeline instead of opt+llc+lld | (unset — use LLVM pipeline on Linux) |
| `PROMISE_OPT` | Override path to `opt` binary | Auto-discover |
| `PROMISE_LLC` | Override path to `llc` binary | Auto-discover |
| `PROMISE_LLD` | Override path to `ld.lld` binary | Auto-discover |
| `PROMISE_CLANG` | Override path to `clang` binary (fallback pipeline) | Auto-discover |
| `PROMISE_DUMP_IR` | Write generated LLVM IR to this path (debugging) | (unset — disabled) |

### LLVM Tool Discovery Order

`findLLVMTool(name)` searches in this order:
1. **Sibling of binary**: `{promise_dir}/opt`, `{promise_dir}/llc`, `{promise_dir}/ld.lld`
2. **Environment override**: `$PROMISE_OPT`, `$PROMISE_LLC`, `$PROMISE_LLD`
3. **Homebrew LLVM** (macOS): `/opt/homebrew/opt/llvm/bin`, `/usr/local/opt/llvm/bin`
4. **Versioned PATH**: `opt-25`, `opt-24`, ..., `opt-22` (newest to oldest, LLVM 22+ only)
5. **Unversioned PATH**: `opt`, `llc`, `ld.lld`

Sibling-first means a bundled Promise installation always uses its own tools regardless of system PATH.

### CRT Object Discovery

`findCRT(target)` discovers glibc CRT objects needed by `ld.lld`:
1. **Primary**: `cc -print-file-name=Scrt1.o` (works on all distros)
2. **Fallback**: probe common paths (`/lib/x86_64-linux-gnu/`, `/usr/lib64/`, GCC versioned dirs)

Discovers: `Scrt1.o` (PIE entry), `crti.o`/`crtn.o` (init/fini sections), `crtbeginS.o`/`crtendS.o` (GCC PIC init).

Library search paths (`-L`) are derived from the CRT locations plus standard paths.

### Linux Linker Invocation

**Default: musl static** (Phase 7b') — target triple `x86_64-unknown-linux-musl`:
```bash
ld.lld \
  -m elf_x86_64 -static \
  --build-id --eh-frame-hdr \
  --lto-O1 \
  -o output \
  ~/.promise/cache/crt/x86_64-linux-musl/crt1.o \
  ~/.promise/cache/crt/x86_64-linux-musl/crti.o \
  promise.bc [module.bc ...] \
  ~/.promise/cache/crt/x86_64-linux-musl/libc.a \
  ~/.promise/cache/crt/x86_64-linux-musl/crtn.o
```

`--lto-O1` performs whole-program optimization on the bitcode inputs — inlining, DCE, and dead
code stripping across all modules. No `-lpthread`, `-lgcc`, `-lgcc_s` needed — musl's `libc.a`
includes everything. Produces fully static binaries that run on any Linux kernel ≥2.6.

**Fallback: glibc dynamic** (`PROMISE_USE_CLANG=1`, or internal glibc path) — target triple `x86_64-unknown-linux-gnu`:
```bash
ld.lld \
  -z relro --hash-style=gnu --build-id --eh-frame-hdr \
  --lto-O1 \
  -m elf_x86_64 -pie \
  -dynamic-linker /lib64/ld-linux-x86-64.so.2 \
  -o output \
  Scrt1.o crti.o crtbeginS.o \
  -L/lib/x86_64-linux-gnu -L/usr/lib/gcc/x86_64-linux-gnu/13 \
  promise.bc [module.bc ...] \
  -lpthread -lgcc --as-needed -lgcc_s --no-as-needed -lc \
  -lgcc --as-needed -lgcc_s --no-as-needed \
  crtendS.o crtn.o
```

Both paths support x86_64 and aarch64 (different emulation mode and dynamic linker path).

### Per-Platform Linker Commands

| Target | Pipeline | LTO |
|--------|---------|-----|
| Linux (musl) | `opt → .bc → ld.lld --lto-O1` | **Done** |
| Linux (glibc) | `opt → .bc → ld.lld --lto-O1` | **Done** |
| macOS (ld64.lld) | `opt → .bc → ld64.lld --lto-O1` | **Done** |
| macOS (system ld) | `opt → .bc → llc → .o → system ld` | No LTO (system ld can't process bitcode) |
| WASM | `opt → .bc → wasm-ld --lto-O2` | **Done** (O2 needed for math intrinsic folding) |
| Windows | `opt → .bc → llc → .o → lld-link` | Not yet (MSVC COFF LTO not wired up) |

`lld` supports all four output formats (Mach-O, ELF, PE/COFF, WASM) via different driver modes.

**WASM** (no CRT, LTO O2):
```bash
opt -O1 promise.ll -o promise.bc
wasm-ld --lto-O2 promise.bc -o output.wasm --no-entry --export=_start --allow-undefined
```
WASM uses `--lto-O2` (not O1) to constant-fold math intrinsics (e.g. `sin(0.0)`) through
the test trampoline's indirect call chain.

**Linux** (ELF — bundled musl CRT, fully static):
```bash
llc -O1 -mtriple=x86_64-unknown-linux-musl -filetype=obj promise.ll -o promise.o
ld.lld \
  {promise_lib}/crt/x86_64-linux-musl/crt1.o \
  {promise_lib}/crt/x86_64-linux-musl/crti.o \
  promise.o \
  {promise_lib}/crt/x86_64-linux-musl/crtn.o \
  {promise_lib}/crt/x86_64-linux-musl/libc.a \
  -static \
  -o output
```

CRT objects (`crt1.o`, `crti.o`, `crtn.o`) are the C runtime startup files — they set up `_start`, call `main()`, and handle process init/fini. Promise bundles musl libc's CRT objects rather than depending on the host's glibc. This produces fully static binaries that run on any Linux kernel ≥2.6 regardless of distro. No `-lpthread` needed — musl includes pthreads in `libc.a`. See the Distribution section for details on musl.

**macOS** (Mach-O — needs SDK sysroot):
```bash
llc -O1 -mtriple=arm64-apple-macosx14.0.0 -filetype=obj promise.ll -o promise.o
ld64.lld promise.o -o output \
  -lSystem \
  -syslibroot $(xcrun --show-sdk-path) \
  -arch arm64 \
  -platform_version macos 14.0.0 14.0.0
```

macOS requires the SDK sysroot for `-lSystem` resolution. `xcrun --show-sdk-path` returns it (e.g., `/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk`). The existing `HostTargetTriple()` already calls `sw_vers` for version detection — similar pattern.

Alternative: use Xcode's system `ld` instead of `ld64.lld` — avoids Mach-O compatibility edge cases. Command: `ld -o output promise.o -lSystem -syslibroot $(xcrun --show-sdk-path) -arch arm64`.

**Windows** (PE/COFF — MSVC CRT):
```bash
llc -O1 -mtriple=x86_64-pc-windows-msvc -filetype=obj promise.ll -o promise.obj
lld-link promise.obj /out:output.exe \
  /defaultlib:libcmt \
  /defaultlib:oldnames \
  kernel32.lib \
  /libpath:"C:\Program Files\...\MSVC\lib\x64" \
  /libpath:"C:\Program Files\...\Windows Kits\10\Lib\...\ucrt\x64" \
  /libpath:"C:\Program Files\...\Windows Kits\10\Lib\...\um\x64"
```

Windows is the most complex — MSVC library paths are deeply nested and version-dependent. Discovery: parse `vswhere` output or read `VS*COMNTOOLS` environment variables. Consider keeping clang as fallback on Windows.

### Cross-Compilation

With `llc` + `lld`, cross-compilation requires no cross-toolchain:

```bash
# Build on macOS, target Linux x86_64
promise build --target x86_64-unknown-linux-gnu app.pr

# Build on Linux, target WASM
promise build --target wasm32 app.pr
```

The PAL (Phase 3) already emits platform-specific IR based on the target triple. The codegen selects the right PAL backend via `pal.ForTarget(triple)`. Cross-compilation just means:
1. PAL emits IR for the target (already works)
2. `llc` compiles to target object format (multi-target by design)
3. `lld` links for target (multi-format by design)

**Cross-target difficulty**:
- **WASM**: easiest — no CRT, no system libs. Works from any host.
- **Linux**: easy — bundled musl CRT means no host Linux toolchain needed. `promise build --target x86_64-linux-musl app.pr` works from macOS.
- **macOS**: requires macOS SDK (`-lSystem`). Cross-compiling *to* macOS from Linux/Windows needs the SDK files (legally gray area).
- **Windows**: requires MSVC CRT libs. Cross-compiling *to* Windows needs the Windows SDK.

### Implementation (7b + 7b' Done, rest planned)

**`cmd/promise/main.go` — implemented functions**:

| Function | Purpose |
|----------|---------|
| `compileAndLink()` | Dispatcher: writes `.ll`, calls LLVM or clang pipeline based on target |
| `useClangPipeline(target)` | Returns true for non-Linux or `PROMISE_USE_CLANG=1` |
| `compileAndLinkLLVM()` | `opt -O1` → `llc -filetype=obj` → `ld.lld` (musl static or glibc dynamic) |
| `compileAndLinkClang()` | Old clang driver path (fallback for non-Linux) |
| `findLLVMTool(name)` | Discovers `opt`/`llc`/`ld.lld` — sibling → env → versioned PATH → unversioned PATH |
| `llvmToolVersion(path)` | Parses `LLVM version X` or `LLD X` from `--version` output |
| `checkLLVMToolVersion(path)` | Enforces LLVM 22+ minimum |
| `findMuslCRT(target)` | Locates musl CRT — sibling → installed → cache → extract from embedded |
| `muslCRTValid(dir)` | Validates cached CRT against embedded sizes (detects stale cache) |
| `buildMuslLinkArgs(target, obj, out, crtDir)` | Builds `ld.lld -static` argument list with musl CRT |
| `findCRT(target)` | Discovers system glibc CRT objects via `cc -print-file-name` + fallback probing |
| `tryCRTFallback(info, missing, target)` | Probes `/lib/{arch}-linux-gnu/`, `/usr/lib/gcc/...` for missing CRT |
| `buildLinuxLinkArgs(target, obj, out)` | Builds `ld.lld -pie` argument list for dynamic glibc linking |
| `dynamicLinker(target)` | Returns `/lib64/ld-linux-x86-64.so.2` or aarch64 equivalent |
| `emulationMode(target)` | Returns `elf_x86_64` or `aarch64linux` |
| `findMacOSSDK()` | Discovers macOS SDK sysroot via `xcrun --show-sdk-path` |
| `parseDarwinTriple(target)` | Extracts arch + version from macOS target triple |
| `findDarwinLinker()` | Locates `ld64.lld` or system `ld` for Mach-O linking |
| `buildDarwinLinkArgs(target, obj, out)` | Builds linker args with `-lSystem -syslibroot -platform_version` |
| `linkDarwin(obj, target, out)` | Runs macOS Mach-O linker (ld64.lld or system ld) |
| `linkLinux(obj, target, out)` | Runs Linux ELF linker (ld.lld with glibc or musl) |

**Build-tagged embed files**:

| File | Build constraint | Purpose |
|------|-----------------|---------|
| `crt_linux_amd64.go` | `linux && amd64` | `go:embed resources/crt/x86_64-linux-musl/*` — embeds musl CRT |
| `crt_other.go` | `!(linux && amd64)` | Empty `embed.FS` stub — no musl CRT on other platforms |

**Musl CRT discovery order** (`findMuslCRT`):
1. Sibling of binary: `{exe_dir}/crt/x86_64-linux-musl/`
2. Installed: `~/.promise/lib/crt/x86_64-linux-musl/`
3. Cache: `~/.promise/cache/crt/x86_64-linux-musl/` (validated by size against embedded)
4. Extract embedded CRT to cache (first build only, ~2.5 MB)

**Phased rollout**:

| Step | Work | Status |
|------|------|--------|
| 7b | Linux: `opt` + `llc` + `ld.lld` + system glibc CRT | **Done** |
| 7b' | Linux: bundled musl CRT (fully static binaries) | **Done** |
| 7a | WASM target via `llc` + `wasm-ld` | **Done** |
| 7c | macOS target via `llc` + system `ld` (or `ld64.lld`) | **Done** |
| 7d | Windows target via `llc` + `lld-link` | Planned (high — MSVC paths) |
| 7e | `--target` flag + cross-compilation | Planned (low — plumbing) |
| 7f | Bundle `llc` + `lld` + musl CRT into release tarball | Planned (medium — CI) |
| 7g | Remove clang fallback (optional) | Planned (low — cleanup) |

**Fallback strategy**: `PROMISE_USE_CLANG=1` forces the clang pipeline on any platform. This lets users on platforms without native linker support (Windows) continue working.

### Coroutine Pass Verification (Done)

Verified with LLVM 22 on Linux and macOS:

1. **`llc -O1` does NOT run CoroSplit** — `llc` only runs backend passes (instruction selection, register allocation). CoroSplit is a CGSCC pass in the middle-end optimization pipeline.
2. **`opt -O1 | llc` works** — `opt -O1` runs the full `default<O1>` pipeline including CoroSplit/CoroElide, producing lowered coroutine IR that `llc` can compile without optimization.
3. **LLVM 22+ requires `llvm.coro.end → void`** — LLVM 20-21 used `i1` return type, LLVM 22+ changed to `void`. The generated IR targets LLVM 22+ exclusively.

Validated: `promise test -stress 20 tests/concurrency/...` — 71 tests, 100% pass rate, 0 flaky across 20 iterations with the `opt` + `llc` + `ld.lld` pipeline.

### Distribution

**Goal**: once `promise` is installed, it has zero external dependencies. No system LLVM, no Homebrew, no Xcode CLT (except macOS `-lSystem`). A fresh machine with the Promise tarball can compile and link.

**Phase 7f (Done)**: LLVM tools are gzip-compressed and embedded in the Go binary via `go:embed` for release builds. Platform-specific embed files select the correct tools per OS/arch combination.

**Supported platforms**:
- **Linux x86_64**: Fully self-contained (~61MB). Embeds opt, llc, lld, libLLVM.so + musl CRT. Produces fully static binaries.
- **macOS arm64 + amd64**: Self-contained (~71MB). Embeds opt, llc, lld, libLLVM.dylib + liblld*.dylib + transitive deps (libz3, libzstd). Requires Xcode CLT for macOS SDK sysroot (`-lSystem`).

**Build modes**:
- `make build` / `./build` — dev build (~14-16MB), uses system LLVM tools
- `make release` / `./build --release` — release build, embeds LLVM tools (gzip-compressed)

**Embedded LLVM tools** (gated by `-tags embed_llvm`):
- Platform-specific embed files: `llvm_linux_amd64.go`, `llvm_darwin_arm64.go`, `llvm_darwin_amd64.go`
- Fallback: `llvm_other.go` (build tag `!embed_llvm`) — no embedded tools
- Each file defines: `embeddedLLVMFiles`, `llvmEmbedPrefix`, `llvmCacheSubdir`, `llvmLibEnvKey`
- Extracted lazily on first use to `~/.promise/cache/llvm/<platform>/`
- Symlinks created: `ld.lld → lld`, `ld64.lld → lld`, `lld-link → lld`, `wasm-ld → lld`
- Library path env var set at exec time: `LD_LIBRARY_PATH` (Linux), `DYLD_LIBRARY_PATH` (macOS)

**macOS Mach-O patching** (during extraction):
- `install_name_tool -add_rpath @loader_path` — tools find dylibs in their own directory
- `install_name_tool -change` — rewrites absolute Homebrew paths to `@rpath/<name>`
- `install_name_tool -id @rpath/<name>` — patches dylib install names
- `codesign --force --sign -` — ad-hoc re-signing after modification

**`make llvm-bundle-darwin`** bundles from Homebrew: finds `llvm` and `lld` formulas separately, discovers transitive non-system deps via `otool -L`, gzip-compresses all files.

**Tool discovery order** (implemented in `findLLVMTool()`):
1. **Sibling directory**: `{promise_dir}/opt`, `{promise_dir}/llvm/opt` (install layout)
2. **Env override**: `PROMISE_OPT`, `PROMISE_LLC`, `PROMISE_LLD`, `PROMISE_LD64LLD` (for development/testing)
3. **Embedded cache**: `~/.promise/cache/llvm/<platform>/` (extracted from embedded on first access)
4. **Homebrew LLVM** (macOS): `/opt/homebrew/opt/llvm/bin`, `/usr/local/opt/llvm/bin`, `/opt/homebrew/opt/lld/bin`, `/usr/local/opt/lld/bin`
5. **Versioned PATH**: `opt-25` down to `opt-22`, `llc-25` down to `llc-22`, etc.
6. **Unversioned PATH**: `opt`, `llc`, `ld.lld`

Looking next to the binary first means an installed Promise always finds its own tools, regardless of system PATH changes or Homebrew upgrades. The embedded cache fallback means a release binary works on a fresh machine with zero LLVM installation.

### Bundled musl libc

Linux linking requires CRT startup objects (`crt1.o`, `crti.o`, `crtn.o`) and libc. Rather than depending on the host's glibc (version-specific, path varies by distro), Promise bundles [musl libc](https://musl.libc.org/) — a lightweight, static-linkable, MIT-licensed libc.

**Why musl**:
- **Static linking** — produces fully static binaries with no `.so` dependencies. A Promise binary runs on any Linux kernel ≥2.6, regardless of distro or installed glibc version.
- **Small** — musl's `libc.a` is ~600KB per architecture. CRT objects are ~2KB each.
- **MIT license** — no copyleft concerns (glibc is LGPL, which complicates static linking).
- **Cross-compilation friendly** — bundling musl CRT for x86_64 + aarch64 means Promise can cross-compile to Linux from macOS or Windows without any Linux toolchain installed.
- **Reproducible** — same musl version always produces the same binary. No variance from host libc version.

**Trade-off**: musl has minor behavioral differences from glibc (locale handling, DNS resolution, `dlopen` limitations). These don't affect Promise — the language doesn't expose locale, DNS, or dynamic loading. The PAL's libc surface (`malloc`, `free`, `realloc`, `memcmp`, `snprintf`, `strlen`, `write`, `exit`, `pthread_*`) is fully supported by musl.

**Linux linker command with bundled musl**:
```bash
llc -O1 -mtriple=x86_64-unknown-linux-musl -filetype=obj promise.ll -o promise.o
ld.lld \
  {promise_lib}/crt/x86_64-linux-musl/crt1.o \
  {promise_lib}/crt/x86_64-linux-musl/crti.o \
  promise.o \
  {promise_lib}/crt/x86_64-linux-musl/crtn.o \
  {promise_lib}/crt/x86_64-linux-musl/libc.a \
  -static \
  -o output
```

Note the target triple changes to `x86_64-unknown-linux-musl` (not `linux-gnu`). The `-static` flag produces a fully static ELF binary. No `-lpthread` needed — musl includes pthreads in `libc.a`.

**Building the musl CRT objects** (release engineering):
```bash
# Cross-compile musl for each target architecture
curl -O https://musl.libc.org/releases/musl-1.2.5.tar.gz
tar xf musl-1.2.5.tar.gz && cd musl-1.2.5

# x86_64
CROSS_COMPILE=x86_64-linux-musl- ./configure --prefix=/out/x86_64 --target=x86_64
make install  # produces crt1.o, crti.o, crtn.o, libc.a

# aarch64
CROSS_COMPILE=aarch64-linux-musl- ./configure --prefix=/out/aarch64 --target=aarch64
make install
```

These objects are built once per musl release and checked into the Promise release artifacts (not the source repo — too large). A CI job rebuilds them when the musl version is bumped.

### macOS — SDK sysroot requirement

macOS is the one platform where a full zero-dependency bundle isn't possible. Apple requires linking against `-lSystem` from the macOS SDK, which is part of Xcode or CommandLineTools. This is unavoidable — even Zig and Go require it on macOS.

Mitigation: the Promise installer can check for CommandLineTools and prompt `xcode-select --install` if missing. This is a one-time setup that most Mac developer machines already have.

**macOS linker command**:
```bash
llc -O1 -mtriple=arm64-apple-macosx14.0.0 -filetype=obj promise.ll -o promise.o
ld64.lld promise.o -o output \
  -lSystem \
  -syslibroot $(xcrun --show-sdk-path) \
  -arch arm64 \
  -platform_version macos 14.0.0 14.0.0
```

### Windows — MSVC dependency

Windows requires MSVC CRT libraries (`libcmt.lib`, `kernel32.lib`) from the Visual Studio Build Tools or Windows SDK. Path discovery is complex (`vswhere`, registry, env vars).

Options:
- **Require VS Build Tools** — same as Zig/Rust on Windows. Most Windows dev machines have it.
- **Bundle MinGW CRT** — alternative to MSVC, but adds compatibility concerns.
- **Keep clang fallback** — `PROMISE_USE_CLANG=1` for Windows until native lld support is solid.

Initial plan: require VS Build Tools, with clang fallback. Revisit if bundling a Windows CRT becomes feasible.

### Future — static LLVM (single binary)

The bundled-binaries approach (Phase 7) is the pragmatic first step. The long-term ideal is statically linking LLVM into the `promise` binary via the LLVM C API + CGo:

- **Single binary** (~200MB) — no `llc`/`lld` to ship separately
- **Faster** — no text IR serialization, no temp files, no process spawning. Build IR in memory → run passes → emit object → link, all in-process.
- **Simpler distribution** — one file to download, one file to run

**Why not now**: CGo complicates Go cross-compilation of the Promise compiler itself. Building LLVM static libraries for every host platform requires significant CI infrastructure. The bundled-binaries approach gives the same user experience (zero deps after install) without this build complexity.

**Migration path**: the internal interface (`compileToObject` / `linkBinary`) is the same whether calling `llc`/`lld` as subprocesses or calling LLVM C API functions. The swap is internal — no user-facing changes.

### Dependency summary by platform

**Current state** (Phase 7b + 7b' + 7c done):

| Platform | Build pipeline | External deps (source build) |
|----------|---------------|------|
| **Linux** | `opt` + `llc` + `ld.lld` + bundled musl CRT → **fully static** | LLVM 22+ (`opt`, `llc`, `lld`), `musl-dev` (embedded at compile time) |
| **macOS** | `opt` + `llc` + system `ld` (or `ld64.lld`) | LLVM 22+ (`opt`, `llc`), Xcode CommandLineTools (SDK + `ld`) |
| **Windows** | clang (fallback) | clang 22+ |
| **WASM** | `opt` + `llc` + `wasm-ld` (no CRT, bump allocator, coop scheduler) | LLVM 22+ (`opt`, `llc`, `wasm-ld`) |

**After full Phase 7** (bundled tools + musl):

| Platform | Release install includes | External deps |
|----------|----------------------|---------------|
| **Linux** | `promise` + `opt` + `llc` + `lld` + musl CRT | **None** — fully static binaries |
| **macOS** | `promise` + `opt` + `llc` + `lld` | Xcode CommandLineTools (for `-lSystem`) |
| **Windows** | `promise` + `opt` + `llc` + `lld` | VS Build Tools (for CRT libs) |
| **WASM** | `promise` + `opt` + `llc` + `wasm-ld` | **None** — no CRT needed |

---

## Key Design Decisions

1. **Layer 0 is a PAL, not raw syscalls.** Windows and WASM make raw syscalls impractical as the universal approach. The PAL is a thin abstraction (~25 functions) with per-target codegen.
2. **Two concurrency modes.** WASM can't do threads, so the scheduler supports both M:N (native) and cooperative (WASM). Language semantics stay the same.
3. **WASM is 32-bit pointers.** Codegen needs `ptrSize` abstraction instead of assuming 64-bit.
4. **Test runner can't use fork on Windows/WASM.** Alternatives: threads on Windows, no isolation on WASM.
5. **Cross-compilation comes naturally.** PAL per-target + `llc`/`lld` enables `promise build --target wasm32 app.pr`.
6. **macOS keeps using libSystem.** Apple doesn't guarantee raw syscall ABI stability. Go does the same.
7. **LLVM integration.** Keep text IR (`llir/llvm`) for now; switch to LLVM C API when removing clang becomes a priority.
