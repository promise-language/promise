# Runtime Architecture Proposal

Proposal for Promise's runtime, syscall layer, concurrency model, and multi-platform support.

## Current State

The runtime today is ~450 lines of C providing:
- **IO**: `printf`/`fprintf`/`fwrite` for print, `snprintf` for int/float/bool/char→string
- **Memory**: `malloc`/`free`/`realloc` for strings, vectors, closures
- **String ops**: new, concat, eq, contains, starts_with, ends_with, index_of, trim, split, UTF-8 decode
- **Vector ops**: with_capacity, push, pop, contains, remove
- **Hash**: FNV-1a for int and string (C)
- **RTTI**: type_is check for inheritance
- **Test runner**: fork/waitpid for crash isolation

The libc surface used: `malloc`, `free`, `realloc`, `memcpy`, `memcmp`, `memmove`, `printf`, `fprintf`, `fwrite`, `snprintf`, `putchar`, `exit`, `fork`, `waitpid`, `fflush`.

Codegen uses `llir/llvm` (pure Go, text IR only). Clang serves as both C compiler (for runtime .c) and linker (.ll + .o → binary).

---

## Target Platforms

| Feature | macOS | Linux | Windows | WASM (browser) |
|---------|-------|-------|---------|----------------|
| Syscall style | libSystem.dylib | raw `syscall` | kernel32.dll (Win32 API) | host imports (JS FFI / WASI) |
| IO multiplex | kqueue | epoll | IOCP | event loop (JS) |
| Memory pages | mmap/munmap | mmap/munmap | VirtualAlloc/VirtualFree | memory.grow |
| Threads | pthread | pthread/clone | CreateThread | **none** (single-threaded) |
| Process spawn | fork/exec | fork/exec | CreateProcess | **none** |
| Linking | Mach-O | ELF | PE/COFF | WASM binary |
| Address size | 64-bit | 64-bit | 64-bit | **32-bit** |

---

## Architecture: 5 Layers

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

### Layer 0: Platform Abstraction Layer (PAL)

A small set of ~25 primitive operations with per-target implementations. The codegen emits calls to PAL functions; each platform provides the LLVM IR for that target.

```
compiler/internal/codegen/pal/
├── pal.go          # PAL function table (abstract interface)
├── pal_darwin.go   # macOS: libSystem.dylib calls
├── pal_linux.go    # Linux: raw syscall instructions
├── pal_windows.go  # Windows: kernel32.dll imports
└── pal_wasm.go     # WASM: host imports (__wasi_* or JS FFI)
```

**IO** (all platforms, different implementations):
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
- WASM: 32-bit pointers, no threads, no filesystem without WASI.

---

### Layer 1: Memory + Intrinsics

**Immediate wins** (no new code needed):
- `memcpy` → `@llvm.memcpy.p0i8.p0i8.i64` (LLVM intrinsic)
- `memset` → `@llvm.memset.p0i8.i64`
- `memmove` → `@llvm.memmove.p0i8.p0i8.i64`

**Memory allocator phasing**:
1. **Now**: Keep `malloc`/`free` but route through `pal_alloc`/`pal_free` wrappers
2. **Later**: mmap-based arena allocator (macOS/Linux), VirtualAlloc-based (Windows), bump allocator on linear memory (WASM)
3. **Much later**: Per-goroutine allocation pools once concurrency lands

**WASM-specific**: Linear memory has a single contiguous address space grown via `memory.grow`. A free-list or bump allocator manages this space.

---

### Layer 2: Core Types in Promise

Move string ops, vector ops, hash, and formatting from C into Promise.

**Blocker**: Promise currently lacks **bitwise operators** (`&`, `|`, `^`, `<<`, `>>`). These are needed for hash, compression, crypto, UTF-8, etc. Adding them is straightforward — new entries in the `native` operator table.

**What can move to Promise today** (no new language features needed):
- `string.contains`, `starts_with`, `ends_with`, `index_of`, `trim`
- `vector.contains`, `vector.remove`
- Map — already done (HashMap is pure Promise)

**What needs bitwise ops first**:
- Hash (FNV-1a uses XOR and multiply)
- Int/float → string conversion (hex formatting needs shifts)
- UTF-8 encode/decode (needs shifts and masks)

**What should stay as builtins** (emitted as LLVM IR by codegen, not C):
- `string.new` (malloc + memcpy — close to allocator)
- `string.concat` (same)
- `vector.push` (realloc path)
- `print` functions (until IO layer is built)
- RTTI `type_is` (accesses raw memory layout)

**Migration order**:
1. Add bitwise operators to the language (`&`, `|`, `^`, `<<`, `>>`, `~`)
2. Move hash to Promise (FNV-1a is ~10 lines)
3. Move string methods (contains, starts_with, etc.) to Promise
4. Move vector.contains/remove to Promise
5. Move int/float/bool→string to Promise
6. Move UTF-8 encode/decode to Promise
7. Replace C string.new/concat with codegen-emitted LLVM IR (calls allocator + memcpy intrinsic)

After this migration, C runtime code drops to near zero.

**WASM pointer size**: WASM uses 32-bit pointers. Codegen needs a `ptrSize` constant instead of hardcoded 8. Struct layouts (vtable_ptr, instance_ptr) shrink to 4 bytes on WASM. The target data layout tells LLVM, but explicit pointer arithmetic in codegen (e.g., vector header offsets) must use `ptrSize`.

---

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

**Implementation language**: Start with C for the scheduler (Option C), migrate to Promise once the language has unsafe pointers, inline assembly, and atomic ops. This is what Go did — early runtimes were mostly C.

---

### Layer 4: Standard Library (Pure Promise)

Everything built on top of Layer 0-3: map (already done), iterators, streams, crypto, compression, networking, etc. All platform-independent.

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

**Alternative**: Keep text IR generation (`llir/llvm`), shell out to `llc` + `lld` instead of `clang`. Ship as companion binaries. `promise install` downloads the right LLVM tools per platform.

---

## Phased Roadmap

| Phase | Work | Targets |
|-------|------|---------|
| **Phase 1** | Bitwise operators (`&`, `\|`, `^`, `<<`, `>>`, `~`) | All |
| **Phase 2** | Move hash, string methods, vector methods to Promise | All |
| **Phase 3** | PAL abstraction — define interface, implement macOS + Linux | macOS, Linux |
| **Phase 3b** | PAL Windows implementation | Windows |
| **Phase 3c** | PAL WASM implementation (WASI imports + JS FFI) | WASM |
| **Phase 4** | Replace memcpy/memset with LLVM intrinsics, centralize allocator | All |
| **Phase 4b** | WASM linear memory allocator (bump/free-list on memory.grow) | WASM |
| **Phase 5** | M:N concurrency scheduler (C initially) | macOS, Linux, Windows |
| **Phase 5b** | Cooperative scheduler for WASM (Asyncify or stack-switching) | WASM |
| **Phase 6** | IO reactor: kqueue + epoll + IOCP | macOS, Linux, Windows |
| **Phase 6b** | JS event loop integration for WASM IO | WASM |
| **Phase 7** | Replace clang with `llc` + `lld`, enable cross-compilation | All |
| **Phase 8** | Rewrite scheduler in Promise | All |

Phases 1-2 are immediate and platform-independent. Phase 3 is where the platform split begins. Phase 5-6 is the big architectural investment. Phase 7-8 is polish.

---

## Key Design Decisions

1. **Layer 0 is a PAL, not raw syscalls.** Windows and WASM make raw syscalls impractical as the universal approach. The PAL is a thin abstraction (~25 functions) with per-target codegen.

2. **Two concurrency modes.** WASM can't do threads, so the scheduler supports both M:N (native) and cooperative (WASM). Language semantics stay the same.

3. **WASM is 32-bit pointers.** Codegen needs `ptrSize` abstraction instead of assuming 64-bit.

4. **Test runner can't use fork on Windows/WASM.** Alternatives: threads on Windows, no isolation on WASM.

5. **Cross-compilation comes naturally.** PAL per-target + `llc`/`lld` enables `promise build --target wasm32 app.pr`.

6. **macOS keeps using libSystem.** Apple doesn't guarantee raw syscall ABI stability. Go does the same.

7. **LLVM integration.** Keep text IR (`llir/llvm`) for now; switch to LLVM C API when removing clang becomes a priority.

8. **int→string without libc.** Implement in Promise once bitwise ops exist.
