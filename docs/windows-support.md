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

### 3.3 Linking

`lld-link` (LLD's COFF/MSVC mode) links against:

| Library | Source | Purpose |
|---------|--------|---------|
| `libucrt.lib` | Windows SDK | C runtime (malloc, free, printf) |
| `libcmt.lib` | MSVC/VS Build Tools | C runtime startup (mainCRTStartup) |
| `libvcruntime.lib` | MSVC/VS Build Tools | C runtime support (memcpy, etc.) |
| `kernel32.lib` | Windows SDK | Win32 API (CreateThread, GetStdHandle, etc.) |

**Entry point:** `mainCRTStartup` (standard MSVC entry). The CRT calls `main()`, which Promise defines.

### 3.4 Windows SDK Discovery

`findWindowsSDK()` probes in order:

1. **`LIB` env var** — VS Developer Command Prompt sets this
2. **`WindowsSdkDir` + `WindowsSDKVersion` env vars**
3. **`VCToolsInstallDir` env var**
4. **Common SDK paths** — `C:\Program Files (x86)\Windows Kits\10\Lib\` versions
5. **vswhere.exe** — `vswhere -latest -property installationPath`
6. **Common VS paths** — `C:\Program Files\Microsoft Visual Studio\2022\{edition}\VC\Tools\MSVC\`

---

## 4. Platform Abstraction Layer (PAL)

The WindowsPAL in `codegen/pal/windows.go` emits LLVM IR that calls Win32 API functions. All Win32 functions are declared as LLVM externals — the linker resolves them from `kernel32.lib` and UCRT.

### 4.1 Fully Implemented (47/52 methods)

| Category | PAL Functions | Win32 API |
|----------|--------------|-----------|
| Memory | `pal_alloc`, `pal_free`, `pal_realloc` | UCRT malloc/free/realloc |
| I/O | `pal_write` | GetStdHandle + WriteFile |
| Exit | `pal_exit` | ExitProcess |
| Threading | `pal_thread_create` | `_beginthreadex` (2MB stack) + trampoline |
| Threading | `pal_thread_join` | WaitForSingleObject + CloseHandle |
| Mutexes | `pal_mutex_init/lock/unlock/destroy` | CRITICAL_SECTION (40 bytes) |
| Condvars | `pal_cond_init/wait/signal/broadcast/destroy` | CONDITION_VARIABLE (8 bytes) |
| System | `pal_num_cpus` | GetSystemInfo |
| File I/O | `pal_file_open/read/write/close/seek/stat_size/remove/exists` | UCRT _open/_read/_write etc. |
| Dirs | `pal_file_mkdir`, `pal_dir_remove/exists/open/next_name/close` | FindFirstFileW/FindNextFileW |
| Env | `pal_getenv/setenv/unsetenv/getcwd/chdir/get_environ` | UCRT getenv/_putenv_s etc. |
| User | `pal_get_user_info`, `pal_get_hostname` | GetUserNameEx, GetComputerNameEx |

**Thread trampoline:** `_beginthreadex` expects `unsigned (__stdcall *)(void*)` returning `i32`. PAL signature is `i8*(i8*)`. The WindowsPAL emits a trampoline adapter. `_beginthreadex` is used instead of `CreateThread` because the CRT per-thread data must be initialized for `__intrinsic_setjmp`/`longjmp` to work.

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
- **Visual Studio Build Tools required**: Analogous to Xcode CLT on macOS. Provides the Windows SDK and MSVC runtime libraries. Long-term: embed minimal `.lib` files to eliminate this dependency.
- **lld-link, not link.exe**: LLD ships with LLVM, is open-source, and works identically across platforms.
- **CRITICAL_SECTION, not SRWLock**: Safer match for `pthread_mutex_t` semantics (recursive, owning).
- **_beginthreadex, not CreateThread**: CRT per-thread data must be initialized for `__intrinsic_setjmp`/`longjmp` to work correctly on worker threads.
- **`__intrinsic_setjmp`, not `_setjmp`**: MSVC `_setjmp` is a macro that calls `__intrinsic_setjmp(buf, __builtin_frame_address(0))`. We call the intrinsic directly with `@llvm.frameaddress(i32 0)`.
- **`Platform.line_separator`**: `\r\n` on Windows, `\n` elsewhere. Correct for console output. Test harness must normalize.
- **No TLS changes needed**: LLVM `thread_local` compiles to Windows TLS on MSVC targets automatically.
