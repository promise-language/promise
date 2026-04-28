# Runtime Architecture Proposal

Proposal for Promise's runtime, syscall layer, concurrency model, and multi-platform support.

## Current State

After completing the C-to-codegen migration (Phases 1-2), the remaining C runtime is minimal:

**Remaining C runtime** (`runtime/runtime.c`, ~15 lines):
- `promise_print_int`, `promise_print_f64`, `promise_print_bool` — IO via `printf`
- `promise_panic`, `promise_panic_msg` — error termination via `fprintf` + `exit`

**Remaining C runtime** (`runtime/runtime_string.c`, ~10 lines):
- `promise_print_string` — IO via `fwrite`

**Remaining C runtime** (`runtime/runtime_test.c`, ~60 lines):
- Test runner — crash isolation via `fork`/`waitpid`

**Everything else is codegen-emitted LLVM IR** (14 intrinsic functions defined in `compiler.go`):
- String: `new`, `concat`, `eq`, `trim`, `split`, `next_char`, `eq_string` (map key equality)
- Vector: `with_capacity`, `push`, `pop`, `contains`, `remove`
- RTTI: `type_is`
- Conversion: `int_to_string`, `uint_to_string`, `f64_to_string`, `bool_to_string`, `char_to_string`
- Hash: `hash_string` (FNV-1a for string map keys)

**Pure Promise** (`std/*.pr`):
- Hash: FNV-1a for int/bool/char/float (`std/hash.pr`)
- String methods: `contains`, `starts_with`, `ends_with`, `index_of` (`std/string.pr`)
- Map: full HashMap implementation (`std/map.pr`)

**Libc surface**: `malloc`, `free`, `realloc`, `memcmp`, `printf`, `fprintf`, `fwrite`, `snprintf`, `putchar`, `exit`, `fork`, `waitpid`, `fflush`.

**LLVM intrinsics** (replace libc `memcpy`/`memmove`): `@llvm.memcpy`, `@llvm.memmove`. Note: no `@llvm.memcmp` intrinsic exists.

**LLVM optimizer attributes** on all externs: `noalias`, `nocapture`, `noundef`, `nounwind`, `willreturn`, `readonly`, `argmemonly` (as applicable).

**Eliminated C files**: `runtime_hash.c`, `runtime_vector.c`, most of `runtime_string.c`, most of `runtime.c`.

---

## Phased Roadmap

| Phase | Work | Status |
|-------|------|--------|
| **1** | Bitwise operators (`&`, `\|`, `^`, `<<`, `>>`, `~`) | Done |
| **2** | Migrate all computation from C to codegen LLVM IR / pure Promise | Done |
| **3** | PAL abstraction — define interface, implement macOS + Linux | **Next** |
| **3b** | PAL Windows | Planned |
| **3c** | PAL WASM (WASI imports + JS FFI) | Planned |
| **4** | Centralize allocator behind PAL | Planned |
| **4b** | WASM linear memory allocator (bump/free-list on `memory.grow`) | Planned |
| **5** | M:N concurrency scheduler (C initially) | Planned |
| **5b** | Cooperative scheduler for WASM (Asyncify or stack-switching) | Planned |
| **6** | IO reactor: kqueue + epoll + IOCP | Planned |
| **6b** | JS event loop integration for WASM IO | Planned |
| **7** | Replace clang with `llc` + `lld`, enable cross-compilation | Planned |
| **8** | Rewrite scheduler in Promise | Planned |

Phases 1-2 are done and platform-independent. Phase 3 is where the platform split begins. Phases 5-6 are the big architectural investment. Phases 7-8 are polish.

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

## Phase 3 — Platform Abstraction Layer (Next)

The remaining C runtime functions all depend on IO or process control. Phase 3 replaces them with a platform abstraction layer (PAL) that provides per-target implementations.

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
│  Layer 0: Platform Abstraction Layer (PAL)   │  write, read, open, mmap, exit, kevent...
└──────────────────────────────────────────────┘
```

### Layer 0: PAL Functions

~25 primitive operations with per-target implementations. The codegen emits calls to PAL functions; each platform provides the LLVM IR.

```
compiler/internal/codegen/pal/
├── pal.go          # PAL function table (abstract interface)
├── pal_darwin.go   # macOS: libSystem.dylib calls
├── pal_linux.go    # Linux: raw syscall instructions
├── pal_windows.go  # Windows: kernel32.dll imports
└── pal_wasm.go     # WASM: host imports (__wasi_* or JS FFI)
```

**IO**:
```
pal_write(fd, buf, len) → bytes_written
pal_read(fd, buf, len) → bytes_read
pal_open(path, flags, mode) → fd
pal_close(fd) → err
pal_stat(path, buf) → err
```
- macOS: `call @write` (libSystem)
- Linux: `syscall` instruction (nr=1 for write)
- Windows: `call @WriteFile` (kernel32), fd→HANDLE mapping
- WASM/WASI: `call @fd_write` (WASI import)
- WASM/browser: `call @__promise_write` (JS-provided import)

**Memory**:
```
pal_alloc(size) → ptr
pal_free(ptr, size)
pal_realloc(ptr, old_size, new_size) → ptr
```
- macOS/Linux: mmap/munmap (or malloc initially)
- Windows: VirtualAlloc/VirtualFree (or HeapAlloc initially)
- WASM: bump allocator on linear memory using `memory.grow`

**Threading** (unavailable on WASM):
```
pal_thread_create(fn, arg) → handle
pal_thread_join(handle)
pal_futex_wait(addr, expected)
pal_futex_wake(addr, count)
```
- macOS: pthread_create + os_unfair_lock
- Linux: clone + futex
- Windows: CreateThread + WaitOnAddress
- WASM: no-op / single-threaded mode

**IO Reactor** (for non-blocking IO):
```
pal_reactor_create() → handle
pal_reactor_add(reactor, fd, events)
pal_reactor_wait(reactor, events_out, timeout) → count
```
- macOS: kqueue + kevent
- Linux: epoll_create + epoll_ctl + epoll_wait
- Windows: CreateIoCompletionPort + GetQueuedCompletionStatus
- WASM: not applicable — use JS event loop integration

**Process** (for test runner):
```
pal_exit(code)
pal_spawn(fn) → status
```
- macOS/Linux: fork + waitpid
- Windows: CreateProcess (or thread-based isolation)
- WASM: call function directly (no isolation, catch panics)

**Platform notes**:
- macOS: Apple discourages raw syscalls and doesn't guarantee ABI stability. Use libSystem.dylib (Go does the same).
- Linux: raw syscalls are stable and preferred.
- Windows: fundamentally different API surface (Win32), requires fd→HANDLE mapping layer.
- WASM: 32-bit pointers, no threads, no filesystem without WASI. Codegen needs a `ptrSize` constant instead of hardcoded 8.

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
