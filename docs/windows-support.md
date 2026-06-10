# Windows Support

> Native Windows support for the Promise compiler. No MinGW — full MSVC-compatible toolchain targeting the Windows SDK and UCRT.

---

## 1. Overview

Promise targets Windows natively via the MSVC ABI (`x86_64-pc-windows-msvc`). The compiler binary (`promise.exe`) is built on Windows using Go, and produces Windows executables by compiling LLVM IR through `opt` → `llc` → `lld-link`, linking against the Windows SDK and UCRT.

**Non-goals:** MinGW/Cygwin, cross-compilation from Linux to Windows.

---

## 2. Current State (2026-03-21)

**Phase 1 complete** — hello world works. Core language, standard library, M:N scheduler, and file I/O all function correctly on Windows.

### Working

- Compiler builds on Windows (`build.ps1`, prerequisite installer `bin/install-prereqs.ps1`)
- Core language: variables, types, enums, generics, match, lambdas, closures
- Standard library: int/float/bool/char/string, Vector, Map, Set, iterators, sorting
- M:N scheduler: goroutines, channels, select, tasks (full GMP model)
- File I/O: open, read, write, close, seek, stat, mkdir, rmdir, dir listing
- Environment: getenv, setenv, getcwd, chdir, hostname, user info
- Monotonic time: QueryPerformanceCounter/Frequency
- Sleep: Win32 Sleep(ms)
- Threading: `_beginthreadex`, CRITICAL_SECTION, CONDITION_VARIABLE
- Panic recovery: `__intrinsic_setjmp`/`longjmp` with 16-byte aligned jmp_buf
- Build cache with `LockFileEx`/`UnlockFileEx` file locking
- Batch tests pass (e.g., `tests/std/int_test.pr`, `tests/concurrency/batch_goroutine_test.pr`)

### Not Working

- Snapshot tests fail due to `\r\n` line endings (`Platform.line_separator` is `\r\n` on Windows, test `expected:` strings use `\n`) — **T0046**
- Process execution stubbed (spawn, streaming spawn, wait_pid, kill all return -1) — **T0053**
- Signal handling stubbed (signal_init, signal_register return -1) — **T0054**
- Stack overflow detection stubbed (no SEH handler) — **T0051**
- No LTO in link pipeline (opt → llc → lld-link, not opt → lld-link --lto) — **T0049**
- Some Go unit tests fail on Windows (path separators, absolute path detection) — **T0047**

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

The WindowsPAL in `codegen/pal/windows.go` emits LLVM IR that calls Win32 API functions. All Win32 functions are declared as LLVM externals — the linker resolves them from `kernel32.lib` and UCRT.

### 4.1 Fully Implemented (47/52 methods)

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

**Thread trampoline:** `CreateThread` expects `DWORD (*)(LPVOID)` (= `i32(i8*)`).
PAL signature is `i8*(i8*)`. The WindowsPAL emits a trampoline adapter. As of
T0772, `pal_thread_create` uses kernel32 **`CreateThread`** rather than the UCRT
`_beginthreadex` — part of the zero-dependency surface (kernel32 is always
present). This is safe because Promise's panic recovery uses **TLS-flag
propagation, not `setjmp`/`longjmp`** (T0146–T0148), so no CRT per-thread state is
needed; the dynamically-linked `ucrtbase.dll` still initializes per-thread errno
via its `DLL_THREAD_ATTACH` callback on bare `CreateThread` threads. (The earlier
`_beginthreadex` rationale below in §8 is obsolete.)

### 4.2 Stubbed (5/52 methods)

| PAL Function | Status | Notes |
|--------------|--------|-------|
| `pal_spawn` | Returns -1 | T0053: needs CreateProcessW |
| `pal_spawn_streaming` | Returns -1 | T0053: needs pipe redirection |
| `pal_read_pipe` | Returns -1 | T0053 |
| `pal_wait_pid` | Returns -1 | T0053: needs WaitForSingleObject + GetExitCodeProcess |
| `pal_kill` | Returns -1 | T0053: needs TerminateProcess |
| `pal_signal_init` | Returns -1 | T0054: needs SetConsoleCtrlHandler |
| `pal_signal_register` | Returns -1 | T0054 |
| `pal_stack_overflow_init` | No-op | T0051: needs VEH handler |
| `pal_stack_overflow_thread_init` | No-op | T0051 |

### 4.3 Windows-Specific Codegen (`codegen/windows.go`)

| Function | Purpose |
|----------|---------|
| `callSetjmp()` | Calls `__intrinsic_setjmp(env, llvm.frameaddress(0))` on Windows (POSIX: `_setjmp(env)`) |
| `defineWindowsUsleep()` | Wraps Win32 `Sleep(ms)` as `usleep(usec)` with 1ms minimum |
| `buildWindowsNanotimeBody()` | QPC/QPF → nanoseconds (two-step to avoid i64 overflow) |
| `buildWindowsSleepNanosBody()` | Nanoseconds → milliseconds → Win32 `Sleep` |

---

## 5. Toolchain & Build System

### 5.1 Prerequisites

- **Go 1.25+** — builds the compiler itself
- **LLVM 22+** — full clang+llvm release (needs `opt.exe`, `llc.exe`, `lld-link.exe`)
- **Visual Studio Build Tools** or **Visual Studio** — provides MSVC libs
- **Windows SDK** — provides `kernel32.lib`, `libucrt.lib`
- **Java 11+** — ANTLR4 parser generation (optional if parser already generated)

Install via: `powershell -ExecutionPolicy Bypass -File bin\install-prereqs.ps1`

### 5.2 Build Script

`build.ps1` at repo root — Windows equivalent of `./build`:

```powershell
.\build.ps1              # embed resources + build → bin\promise.exe
.\build.ps1 -Generate    # also regenerate ANTLR parser (requires Java)
```

Steps: check prerequisites → embed resources (modules, catalog) → `go build` → write hash sidecar → smoke test.

---

## 6. Implementation Plan

### Phase 1: Build Infrastructure — DONE

- [x] `build.ps1` — self-contained Windows build script
- [x] `bin/install-prereqs.ps1` — prerequisite installer
- [x] Windows codegen: setjmp/longjmp, usleep, nanotime, sleep_nanos
- [x] PAL: _beginthreadex, CRITICAL_SECTION, CONDITION_VARIABLE, file I/O
- [x] Module cache file locking (LockFileEx/UnlockFileEx)
- [x] `.exe` extension for run/exec/test temp binaries

### Phase 2: Test Infrastructure

Fix the test harness so the full test suite runs on Windows with meaningful results.

- **T0046**: Snapshot test `\r\n` normalization — strip `\r` from actual output before comparing against `expected:` strings. Unblocks ~100 snapshot tests.
- **T0047**: Fix Go unit tests for Windows paths — path separators, absolute path detection, colon-in-path issues.
- **T0048**: Windows CI pipeline — GitHub Actions `windows-latest`, run `build.ps1` + Go tests + batch tests.

### Phase 3: Link Pipeline & Codegen Cleanup

- **T0049**: Windows LTO support — investigate `lld-link` LTO for COFF targets in LLVM 22. Current: `opt → llc → lld-link` (no LTO). Target: bitcode → `lld-link` with LTO.
- **T0050**: Codegen cleanup — deduplicate B0143 (`@Sleep` declarations) and B0144 (QPC/QPF logic). Ensure all Win32 declarations use `getOrDeclareFunc`.

### Phase 4: Runtime Hardening

- **T0051**: Stack overflow detection (B0141) — register a Vectored Exception Handler (VEH) via `AddVectoredExceptionHandler` at startup. Check for `EXCEPTION_STACK_OVERFLOW` (0xC00000FD). Print "fatal: stack overflow" and `ExitProcess(2)`.
- **T0052**: Snapshot test portability audit — after T0046, check for path separator assumptions in expected output, error messages, etc.

### Phase 5: Process Execution & Signals

- **T0053**: Windows process execution — implement `pal_spawn` via `CreateProcessW`, `pal_wait_pid` via `WaitForSingleObject` + `GetExitCodeProcess`, `pal_read_pipe` via `ReadFile`, `pal_spawn_streaming` with pipe redirection, `pal_kill` via `TerminateProcess`.
- **T0054**: Windows signal handling — `SetConsoleCtrlHandler` for Ctrl+C/Ctrl+Break. Map CTRL_C_EVENT → SIGINT, CTRL_BREAK_EVENT → SIGTERM. Other signals return "not supported" on Windows.

### Phase 6: Full Parity & Polish

- **T0055**: Run full test suite on Windows (`tests/e2e/...`, `tests/std/...`, `tests/concurrency/...`, `tests/modules/...`, `tests/value_types/...`, `tests/arrays/...`). Fix remaining failures. Add Windows-specific tests. Stress test the scheduler.
- **T0056**: Release build support — `build.ps1 -Release` embeds LLVM tools into the binary. Windows-specific embed file (`llvm_windows_amd64.go`). Self-contained ~60-70MB binary.

### Dependencies

```
Phase 1 (DONE)
  |
  v
Phase 2 (Test Infrastructure)  ← unblocks all testing
  |
  +--> Phase 3 (Link Pipeline)  ← independent, improves perf
  |
  +--> Phase 4 (Runtime Hardening)  ← independent, improves reliability
  |
  v
Phase 5 (Process Execution & Signals)  ← needs Phase 2 for testing
  |
  v
Phase 6 (Full Parity & Polish)  ← needs all above
```

---

## 7. Existing Tracker Items

| ID | Title | Phase | Status |
|----|-------|-------|--------|
| B0140 | Go codegen tests assume POSIX PAL (fail on Windows) | Phase 2 | open |
| B0141 | Windows: no stack overflow detection | Phase 4 | open |
| B0142 | Windows PAL tests assert CreateThread but impl uses _beginthreadex | Phase 1 | done |
| B0143 | defineWindowsUsleep may declare duplicate @Sleep | Phase 3 | open |
| B0144 | nanotime QPC/QPF logic duplicated in windows.go and io.go | Phase 3 | open |

---

## 8. Key Design Decisions

- **MSVC ABI, not MinGW**: Native Windows experience. MSVC ABI is what Windows developers and tools expect.
- **No Visual Studio Build Tools / Windows SDK required (T0772)**: Superseded the original "Build Tools required" decision. The link surface is self-generated (own import libs from license-clean `.def` symbol lists + codegen-emitted crt0/TLS/`__chkstk`/`_fltused`), so a fresh machine links runnable `.exe`s with zero local dependencies and no Microsoft `.lib` redistribution. See §3.3.
- **lld-link, not link.exe**: LLD ships with LLVM, is open-source, and works identically across platforms.
- **CRITICAL_SECTION, not SRWLock**: Safer match for `pthread_mutex_t` semantics (recursive, owning).
- **CreateThread, not _beginthreadex (T0772)**: Superseded the original `_beginthreadex` decision. kernel32 `CreateThread` keeps the worker-thread surface on an always-present DLL. Safe because panic recovery uses TLS-flag propagation, not `setjmp`/`longjmp` (T0146–T0148), so no CRT per-thread state is required; ucrtbase's `DLL_THREAD_ATTACH` still initializes per-thread errno on bare `CreateThread` threads.
- **No `setjmp`/`longjmp` (obsolete decision)**: Panic recovery once used `__intrinsic_setjmp`; as of T0146–T0148 it is TLS-flag propagation instead, so no `setjmp`/`longjmp` is emitted on any target. This is what makes the `CreateThread` switch (above) safe.
- **`Platform.line_separator`**: `\r\n` on Windows, `\n` elsewhere. Correct for console output. Test harness must normalize.
- **Self-supplied TLS directory (T0772)**: LLVM `thread_local` compiles to Windows TLS on MSVC targets automatically, but the loader only allocates per-thread storage when the PE has a TLS directory (`_tls_used`). The MSVC CRT (tlssup) normally supplies it; since we link no CRT, codegen emits `_tls_used`/`_tls_index` + the `.tls` start/end markers itself (see `emitWindowsTLSSupport`).
