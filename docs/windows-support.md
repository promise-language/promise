# Windows Support

> Native Windows support for the Promise compiler. No MinGW — full MSVC-compatible toolchain targeting the Windows SDK and UCRT, with a self-generated link surface that needs no Visual Studio Build Tools.

---

## 1. Overview

Promise targets Windows natively via the MSVC ABI (`x86_64-pc-windows-msvc`). The compiler binary (`promise.exe`) is built on Windows using Go, and produces Windows executables by compiling LLVM IR through `opt` → `llc` → `lld-link`, linking against a self-generated link surface (own import libs + codegen-emitted crt0/TLS/builtins) that resolves only to DLLs shipped with Windows.

**Non-goals:** MinGW/Cygwin, cross-compilation from Linux to Windows.

---

## 2. Current State

Windows is a first-class, fully-supported target. Core language, standard library, M:N scheduler, file I/O, process execution, signals, stack-overflow detection, and release builds all work. `windows-amd64` is a full CI matrix member (built + tested on every PR and `main`/`next` push, see T0774), and the full test suite passes on Windows.

### Working

- **Build & toolchain:** compiler builds on Windows; release builds (`bin/build --release`) embed LLVM tools (T0056); **zero-dependency link surface** — no Visual Studio Build Tools, no Windows SDK, no Microsoft `.lib` redistribution (T0772)
- **Core language:** variables, types, enums, generics, match, lambdas, closures
- **Standard library:** int/float/bool/char/string, Vector, Map, Set, iterators, sorting
- **M:N scheduler:** goroutines, channels, select, tasks (full GMP model)
- **File I/O:** open, read, write, close, seek, stat, mkdir, rmdir, dir listing
- **Process execution:** spawn, streaming spawn, wait_pid, kill, pipe redirection (T0053)
- **Signals:** Ctrl+C → SIGINT, Ctrl+Break → SIGTERM via `SetConsoleCtrlHandler`; self-signaling via `GenerateConsoleCtrlEvent` (T0054)
- **Stack overflow detection:** Vectored Exception Handler catches `STATUS_STACK_OVERFLOW`, prints `fatal: stack overflow`, exits (T0051)
- **Environment:** getenv, setenv, getcwd, chdir, hostname, user info, process id
- **Monotonic time:** QueryPerformanceCounter/Frequency
- **Sleep:** Win32 `Sleep(ms)`
- **Threading:** `CreateThread` (2MB stack), CRITICAL_SECTION, CONDITION_VARIABLE
- **Panic recovery:** TLS-flag propagation (no `setjmp`/`longjmp` on any target)
- **Build cache** with `LockFileEx`/`UnlockFileEx` file locking
- **Tests:** both batch and snapshot tests pass; `\r\n` output is normalized in the harness (T0046)

### Known limitations

- **No LTO in the link pipeline** — Windows uses `opt → llc → lld-link` (no cross-module LTO, unlike Linux/macOS which use bitcode → linker with `--lto-O1`). Deferred — **T0049**.
- **POSIX-only signals are unsupported by design** — only SIGINT/SIGTERM map to Windows console control events; SIGHUP and other POSIX signals return an error.

---

## 3. Architecture

### 3.1 Target Triple

```
x86_64-pc-windows-msvc    (amd64)
aarch64-pc-windows-msvc   (arm64, future)
```

### 3.2 LLVM Pipeline

```
.pr source → frontend → LLVM IR
  → opt -O1 (coroutine lowering)
  → llc -mtriple=x86_64-pc-windows-msvc -filetype=obj (→ COFF .obj)
  → lld-link (MSVC-compatible linker)
```

Note: No LTO yet on Windows (unlike Linux/macOS which use bitcode → linker with `--lto-O1`). See T0049.

### 3.3 Linking — self-generated zero-dependency surface (T0772)

`lld-link` (LLD's COFF/MSVC mode) links against a **self-generated link surface**
that needs **no Visual Studio Build Tools, no Windows SDK, and re-hosts no
Microsoft `.lib`**. The audit of `codegen/pal/windows.go` + `codegen/windows.go`
showed the compiler barely touches the static MSVC CRT, so the entire surface is
self-suppliable:

| Component | Source | Purpose |
|-----------|--------|---------|
| `kernel32.lib` | self-generated import lib (always-present DLL) | Win32 API: CreateThread, GetStdHandle, CRITICAL_SECTION, etc. |
| `advapi32.lib` | self-generated import lib (always-present DLL) | GetUserNameA |
| `ws2_32.lib` | self-generated import lib (always-present DLL) | sockets (net module) |
| `ucrtbase.lib` | self-generated import lib (ships with Windows 10+) | C runtime: malloc/free, _open/_read, math libcalls, app-init |
| `@__promise_start` | codegen-emitted IR | program entry (replaces `mainCRTStartup`) |
| `_tls_used` / `_tls_index` | codegen-emitted IR | TLS directory for `__declspec(thread)` globals (tlssup replacement) |
| `__chkstk` | codegen-emitted IR (naked asm) | stack-probe helper (compiler-rt's Windows lib lacks it; no DLL exports it) |
| `_fltused` | codegen-emitted IR | MSVC floating-point marker |

The import libs are produced from license-clean **symbol-list `.def` files**
(`tools/build/winlink/def/`) via `llvm-dlltool` — symbol→DLL mappings are not
copyrightable, so the generated `.lib`s are freely re-hostable. They are
committed under `compiler/cmd/promise/resources/winlink/windows-amd64/` and
embedded (`go:embed`) into the compiler binary (~21 KiB total), then extracted to
`<PROMISE_HOME>/cache/winlink/<arch>/` at link time (mirroring the embedded musl
CRT objects). Regenerate with `bin/release winlink`.

**Entry point:** `@__promise_start` (codegen-emitted; named in `/entry:`). It runs
the minimal UCRT app-init (`_configure_narrow_argv` + `_initialize_narrow_environment`,
which parse the command line and populate `_environ`), reads argc/argv via
`__p___argc`/`__p___argv`, calls `@main(argc, argv)`, then `ExitProcess`. ucrtbase's
own DllMain initializes the heap and per-thread errno on load and on each
`DLL_THREAD_ATTACH`, so no further app-level CRT init is required — even on bare
`CreateThread` worker threads.

x86_64 only for now (single calling convention, no name decoration); arm64
Windows import-lib generation is a follow-up.

### 3.4 No SDK discovery

The old `findWindowsSDK()` probe (VS env vars, Windows Kits paths, `vswhere.exe`)
is **removed**. A fresh Windows machine needs no probe hit and no Microsoft files
on disk — the surface above is entirely self-contained. `buildWindowsLinkArgs`
points `/libpath:` at the extracted import-lib dir and `/entry:__promise_start`.

---

## 4. Platform Abstraction Layer (PAL)

The WindowsPAL in `codegen/pal/windows.go` emits LLVM IR that calls Win32 API functions. All Win32 functions are declared as LLVM externals — the linker resolves them from the self-generated import libs (kernel32, advapi32, ws2_32, ucrtbase). All PAL methods are implemented; there are no stubs.

### 4.1 Implemented

| Category | PAL Functions | Win32 API |
|----------|--------------|-----------|
| Memory | `pal_alloc`, `pal_free`, `pal_realloc` | UCRT malloc/free/realloc |
| I/O | `pal_write` | GetStdHandle + WriteFile |
| Exit | `pal_exit` | ExitProcess |
| Threading | `pal_thread_create` | `CreateThread` (2MB stack) + trampoline |
| Threading | `pal_thread_join` | WaitForSingleObject + CloseHandle |
| Mutexes | `pal_mutex_init/lock/unlock/destroy` | CRITICAL_SECTION (40 bytes) |
| Condvars | `pal_cond_init/wait/signal/broadcast/destroy` | CONDITION_VARIABLE (8 bytes) |
| System | `pal_num_cpus` | GetSystemInfo |
| File I/O | `pal_file_open/read/write/close/seek/stat_size/remove/exists` | UCRT _open/_read/_write etc. |
| Dirs | `pal_file_mkdir`, `pal_dir_remove/exists/open/next_name/close` | FindFirstFileW/FindNextFileW |
| Env | `pal_getenv/setenv/unsetenv/getcwd/chdir/get_environ` | UCRT getenv/_putenv_s etc. |
| User | `pal_get_user_info`, `pal_get_hostname` | GetUserNameEx, GetComputerNameEx |
| Process | `pal_spawn`, `pal_spawn_streaming`, `pal_read_pipe`, `pal_wait_pid`, `pal_kill` | CreateProcessA + CreatePipe, ReadFile, WaitForSingleObject + GetExitCodeProcess, TerminateProcess / GenerateConsoleCtrlEvent |
| Signals | `pal_signal_init`, `pal_signal_register` | SetConsoleCtrlHandler + UCRT `_pipe` |
| Stack overflow | `pal_stack_overflow_init` | AddVectoredExceptionHandler (process-global; thread init is a no-op) |

**Thread trampoline:** `CreateThread` expects `DWORD (*)(LPVOID)` (= `i32(i8*)`).
PAL signature is `i8*(i8*)`. The WindowsPAL emits a trampoline adapter.
`pal_thread_create` uses kernel32 **`CreateThread`** (T0772) — part of the
zero-dependency surface (kernel32 is always present). This is safe because
Promise's panic recovery uses **TLS-flag propagation, not `setjmp`/`longjmp`**
(T0146–T0148), so no CRT per-thread state is needed; the dynamically-linked
`ucrtbase.dll` still initializes per-thread errno via its `DLL_THREAD_ATTACH`
callback on bare `CreateThread` threads.

### 4.2 Process & signal behavior notes

- **Self-signaling** (`pal_kill` with `pid == GetCurrentProcessId()`): SIGINT →
  `GenerateConsoleCtrlEvent(CTRL_C_EVENT)`, SIGTERM → `CTRL_BREAK_EVENT`. Other
  PIDs use `TerminateProcess`. Unsupported signals (SIGHUP etc.) return -1 (B0154).
- **Console control events:** only SIGINT/SIGTERM have Windows equivalents; per-signal
  enable flags ensure only registered signals are delivered.

### 4.3 Windows-Specific Codegen (`codegen/windows.go`)

| Function | Purpose |
|----------|---------|
| `defineWindowsUsleep()` | Wraps Win32 `Sleep(ms)` as `usleep(usec)` with 1ms minimum |
| `emitWindowsQPCNanos()` | QPC/QPF → nanoseconds (shared by nanotime + sleep_nanos; two-step to avoid i64 overflow) |
| `buildWindowsSleepNanosBody()` | Nanoseconds → milliseconds → Win32 `Sleep` |

---

## 5. Toolchain & Build System

### 5.1 Prerequisites

- **Go 1.25+** — builds the compiler itself
- **LLVM 22+** — full clang+llvm release (needs `opt.exe`, `llc.exe`, `lld-link.exe`)
- **Java 11+** — ANTLR4 parser generation (optional if the parser is already generated; the generated parser is committed)

No Visual Studio Build Tools and no Windows SDK are required — the link surface is
self-generated (T0772, §3.3). Release builds (`bin/build --release`) embed the LLVM
tools into the binary, so an installed LLVM is only needed for non-release builds.

### 5.2 Bootstrap & Build

```cmd
.\make.cmd               :: bootstrap — compile build tools to bin\
bin\build                :: embed resources + build → bin\promise.exe
bin\build --release      :: release build with embedded LLVM tools
bin\test --wasm          :: build + run all tests (incl. wasm32-wasi)
bin\verify --wasm        :: format + vet + all tests (pre-commit check)
```

`bin\build` handles ANTLR generation (when Java is present), resource embedding,
and compilation. `bin\` is gitignored and the forge tools refuse to run stale, so
re-run `.\make.cmd` after changing `tools/` sources.

---

## 6. Remaining Work

Windows has reached full parity for the language, standard library, runtime, and
test suite. The one outstanding item is a performance optimization:

- **T0049** — Windows LTO. Current link pipeline is `opt -O1 → llc -filetype=obj → lld-link`
  (no LTO). Linux/macOS use bitcode → linker with `--lto-O1` for cross-module
  inlining and DCE. Investigate `lld-link` LTO support for COFF targets. Affects
  binary size and performance, not correctness. Deferred, low priority.

---

## 7. Key Design Decisions

- **MSVC ABI, not MinGW**: native Windows experience. MSVC ABI is what Windows developers and tools expect.
- **No Visual Studio Build Tools / Windows SDK (T0772)**: the link surface is self-generated (own import libs from license-clean `.def` symbol lists + codegen-emitted crt0/TLS/`__chkstk`/`_fltused`), so a fresh machine links runnable `.exe`s with zero local dependencies and no Microsoft `.lib` redistribution. See §3.3.
- **lld-link, not link.exe**: LLD ships with LLVM, is open-source, and works identically across platforms.
- **CRITICAL_SECTION, not SRWLock**: safer match for `pthread_mutex_t` semantics (recursive, owning).
- **CreateThread, not _beginthreadex (T0772)**: keeps the worker-thread surface on an always-present DLL. Safe because panic recovery uses TLS-flag propagation, not `setjmp`/`longjmp` (T0146–T0148), so no CRT per-thread state is required; ucrtbase's `DLL_THREAD_ATTACH` still initializes per-thread errno on bare `CreateThread` threads.
- **No `setjmp`/`longjmp`**: panic recovery uses TLS-flag propagation on every target. This is what makes the `CreateThread` choice safe.
- **`Platform.line_separator`**: `\r\n` on Windows, `\n` elsewhere. Correct for console output; the test harness normalizes `\r\n` before comparing snapshot output (T0046).
- **Self-supplied TLS directory (T0772)**: LLVM `thread_local` compiles to Windows TLS on MSVC targets automatically, but the loader only allocates per-thread storage when the PE has a TLS directory (`_tls_used`). The MSVC CRT (tlssup) normally supplies it; since we link no CRT, codegen emits `_tls_used`/`_tls_index` + the `.tls` start/end markers itself (see `emitWindowsTLSSupport`).
