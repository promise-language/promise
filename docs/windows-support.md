# Windows Support

> Native Windows support for the Promise compiler. No MinGW — full MSVC-compatible toolchain targeting the Windows SDK and UCRT.

---

## 1. Overview

Promise targets Windows natively via the MSVC ABI (`x86_64-pc-windows-msvc`). The compiler binary (`promise.exe`) is built on Windows using Go, and produces Windows executables by compiling LLVM IR through `opt` → `llc` → `lld-link`, linking against the Windows SDK and UCRT.

**Non-goals:** MinGW/Cygwin, cross-compilation from Linux to Windows.

---

## 2. Architecture

### 2.1 Target Triple

```
x86_64-pc-windows-msvc    (amd64)
aarch64-pc-windows-msvc   (arm64, future)
```

### 2.2 LLVM Pipeline

Same as Linux/macOS — no clang driver needed:

```
.pr source → frontend → LLVM IR
  → opt -O1 (coroutine lowering)
  → llc -mtriple=x86_64-pc-windows-msvc -filetype=obj (→ COFF .obj)
  → lld-link (MSVC-compatible linker)
```

### 2.3 Linking

`lld-link` (LLD's COFF/MSVC mode) links against:

| Library | Source | Purpose |
|---------|--------|---------|
| `libucrt.lib` | Windows SDK | C runtime (malloc, free, printf) |
| `libcmt.lib` | MSVC/VS Build Tools | C runtime startup (mainCRTStartup) |
| `libvcruntime.lib` | MSVC/VS Build Tools | C runtime support (memcpy, etc.) |
| `kernel32.lib` | Windows SDK | Win32 API (CreateThread, GetStdHandle, etc.) |
| `synchronization.lib` | Windows SDK | WaitOnAddress/WakeByAddress (future, if needed) |

**Entry point:** `mainCRTStartup` (standard MSVC entry). The CRT calls `main()`, which Promise defines.

**Library search paths** (discovered at compile time):
```
C:\Program Files (x86)\Windows Kits\10\Lib\10.0.XXXXX.0\ucrt\x64\
C:\Program Files (x86)\Windows Kits\10\Lib\10.0.XXXXX.0\um\x64\
C:\Program Files\Microsoft Visual Studio\2022\...\lib\x64\
```

Discovery uses `vswhere.exe` (ships with VS Build Tools) and registry/environment probing.

---

## 3. Platform Abstraction Layer (PAL)

The WindowsPAL in `codegen/pal/windows.go` emits LLVM IR that calls Win32 API functions. All Win32 functions are declared as LLVM externals — the linker resolves them from `kernel32.lib` and UCRT.

### 3.1 Already Implemented

| PAL Function | Win32 API | Status |
|--------------|-----------|--------|
| `pal_write` | `GetStdHandle` + `WriteFile` | Done |
| `pal_exit` | `ExitProcess` | Done |
| `pal_alloc` | `malloc` (UCRT) | Done |
| `pal_free` | `free` (UCRT) | Done |
| `pal_realloc` | `realloc` (UCRT) | Done |

### 3.2 Threading (Phase W1 — Linux-preparable)

| PAL Function | Win32 API | Notes |
|--------------|-----------|-------|
| `pal_thread_create` | `CreateThread` | 2MB stack via `dwStackSize` param |
| `pal_thread_join` | `WaitForSingleObject` + `CloseHandle` | INFINITE timeout |
| `pal_mutex_init` | `InitializeCriticalSection` | Alloc 40 bytes for CRITICAL_SECTION |
| `pal_mutex_lock` | `EnterCriticalSection` | |
| `pal_mutex_unlock` | `LeaveCriticalSection` | |
| `pal_mutex_destroy` | `DeleteCriticalSection` + `pal_free` | |
| `pal_cond_init` | `InitializeConditionVariable` | Alloc 8 bytes for CONDITION_VARIABLE |
| `pal_cond_wait` | `SleepConditionVariableCS` | INFINITE timeout |
| `pal_cond_signal` | `WakeConditionVariable` | |
| `pal_cond_broadcast` | `WakeAllConditionVariable` | |
| `pal_cond_destroy` | `pal_free` (no destroy API needed) | CV is just an 8-byte value |
| `pal_num_cpus` | `GetSystemInfo` → `dwNumberOfProcessors` | Offset 32 in SYSTEM_INFO (x64) |

**Win32 struct sizes (x86_64):**
- `CRITICAL_SECTION`: 40 bytes
- `CONDITION_VARIABLE`: 8 bytes
- `SYSTEM_INFO`: 48 bytes (need `dwNumberOfProcessors` at offset 32)

**Thread function adapter:** Win32 `CreateThread` expects `DWORD WINAPI ThreadProc(LPVOID)` (returns `i32`). Our PAL signature is `i8* fn(i8*)` (returns `i8*`). The WindowsPAL emits a small trampoline: calls the real function, discards the `i8*` return, returns `0` (`DWORD`).

### 3.3 Concurrency Model

The M:N scheduler works unchanged on Windows:
- **G** (goroutine): LLVM coroutines — platform-independent
- **P** (processor): Pure data structure — platform-independent
- **M** (OS thread): Created via `pal_thread_create` → `CreateThread`
- **Synchronization**: `pal_mutex_*` → `CRITICAL_SECTION`, `pal_cond_*` → `CONDITION_VARIABLE`
- **Preemption**: sysmon thread uses same mechanism (set `G.preempt=1` flag)

---

## 4. Toolchain & Build System

### 4.1 Prerequisites (Windows)

- **Go 1.25+** — builds the compiler itself
- **LLVM 22+** — `opt.exe`, `llc.exe`, `lld-link.exe` (from LLVM releases or `choco install llvm`)
- **Visual Studio Build Tools** or **Visual Studio** — provides MSVC libs (`libcmt.lib`, `libvcruntime.lib`)
- **Windows SDK** — provides `kernel32.lib`, `libucrt.lib`
- **Java 21+** — ANTLR4 parser generation (same as all platforms)
- **ANTLR4 JAR** — downloaded by Makefile (same as all platforms)

Note: Visual Studio Build Tools is a free ~2GB download. The full Visual Studio IDE is not required.

### 4.2 Build Script

`build.bat` at repo root — Windows equivalent of `./build`:

```batch
@echo off
REM Build the Promise compiler. Output: bin\promise.exe
REM This is the ONLY correct way to build on Windows.
setlocal

cd /d "%~dp0\compiler"
make generate
make resources
cd /d "%~dp0"

mkdir bin 2>nul
cd compiler
go build -buildvcs=false -o ..\bin\promise.exe .\cmd\promise
```

Requires `make` (from Git for Windows, or standalone GNU Make).

### 4.3 LLVM Tool Discovery (Windows)

Same search chain as Linux/macOS, with `.exe` suffix:

1. Sibling of `promise.exe`: `{exe_dir}\opt.exe`, `{exe_dir}\llc.exe`, `{exe_dir}\lld-link.exe`
2. Environment overrides: `PROMISE_OPT`, `PROMISE_LLC`, `PROMISE_LLD`
3. Embedded LLVM cache: `%USERPROFILE%\.promise\cache\llvm\windows-amd64\` (future)
4. `PATH`: `opt.exe`, `llc.exe`, `lld-link.exe` (versioned probing: `opt-25.exe` etc.)

### 4.4 Windows SDK Discovery

`findWindowsSDK()` probes in order (stops when all three lib dirs are found):

1. **`LIB` env var** — VS Developer Command Prompt sets this with semicolon-separated dirs. Scans for dirs containing `libucrt.lib` (UCRT), `kernel32.lib` (UM), `libcmt.lib` (MSVC).
2. **`WindowsSdkDir` + `WindowsSDKVersion` env vars** — Constructs `{dir}\Lib\{ver}\ucrt\x64` and `um\x64`.
3. **`VCToolsInstallDir` env var** — Constructs `{dir}\lib\x64`.
4. **Common SDK paths** — Probes `C:\Program Files (x86)\Windows Kits\10\Lib\` versions in reverse order.
5. **vswhere.exe** — Runs `vswhere -latest -property installationPath` → `VC\Tools\MSVC\{version}\lib\x64\`.
6. **Common VS paths** — `C:\Program Files\Microsoft Visual Studio\2022\{BuildTools,Community,Professional,Enterprise}\VC\Tools\MSVC\`.

### 4.5 Promise Home (Windows)

```
%USERPROFILE%\.promise\
  bin\
    promise.exe
    llvm\              (future: embedded LLVM tools)
  lib\
    std\
  cache\
    build\
    modules\
```

`os.UserHomeDir()` returns `%USERPROFILE%` on Windows — `home.go` already handles this correctly.

---

## 5. Compiler Changes Summary

### 5.1 Files Modified

| File | Change |
|------|--------|
| `pal/windows.go` | Real Win32 threading implementations for all PAL functions |
| `pal/pal_test.go` | Windows-specific tests for every PAL category (threading, mutex, condvar, numCPUs) |
| `cmd/promise/main.go` | `isWindowsTarget()`, `binaryExtension()`, `linkWindows()`, `linkWindowsMulti()`, `findWindowsSDK()`, `.exe` handling, `useClangPipeline()`, `runInstall()`, LLVM tool `.exe` suffix |
| `cmd/promise/stress.go` | `binaryExtension()` for consistent extension handling |

### 5.2 New Files

| File | Purpose |
|------|---------|
| `build.bat` | Windows build script (with git hooks setup) |
| `cmd/promise/crt_windows_amd64.go` | Build tag stub (no embedded CRT on Windows — uses system SDK) |
| `cmd/promise/llvm_windows_amd64.go` | Build tag stub (future: embedded LLVM for Windows release builds) |

---

## 6. Implementation Phases

### Phase W1 — PAL + Linker (Linux-preparable) ✓

All code changes are Go code that generates LLVM IR or invokes tools. Written and compiled on Linux — tested via Go unit tests (IR shape validation), end-to-end testing deferred to Phase W2.

1. **WindowsPAL threading** — Real Win32 API calls for all 17 PAL functions
2. **`linkWindows()` / `linkWindowsMulti()`** — COFF linking via `lld-link`
3. **`findWindowsSDK()`** — Multi-stage Windows SDK + MSVC lib discovery
4. **`isWindowsTarget()` / `binaryExtension()`** — Target detection and `.exe` handling
5. **`useClangPipeline()`** — Add `windows` to LLVM pipeline targets
6. **`.exe` extension** — Output files, temp files, test binaries
7. **`runInstall()`** — Windows PATH instructions, `promise.exe` naming
8. **`build.bat`** — Windows build script (with git hooks setup)
9. **LLVM tool search** — `.exe` suffix probing on `runtime.GOOS == "windows"`
10. **Build tag stubs** — `crt_windows_amd64.go`, `llvm_windows_amd64.go`

**Test coverage** (`pal_test.go`): Every WindowsPAL function has dedicated tests verifying correct Win32 API declarations, struct sizes, calling conventions, and negative assertions (no pthreads). Includes detail tests for the thread trampoline ABI adapter, CRITICAL_SECTION/CONDITION_VARIABLE sizes, SYSTEM_INFO struct access at offset 32, and INFINITE timeout constants.

### Phase W2 — Testing & Validation (Windows machine)

Must be done on Windows:

1. Build the compiler: `build.bat` → `bin\promise.exe`
2. Run Go tests: `cd compiler && go test ./...`
3. Test single-file compilation: `bin\promise build tests\e2e\basics.pr`
4. Verify PAL threading: `bin\promise test tests\concurrency\...`
5. Run full test suite: `bin\promise test tests\... modules\...`
6. Test `promise install` on Windows
7. Fix any Windows-specific issues discovered during testing

### Phase W3 — Self-contained Binary (Windows, future)

1. Build LLVM tools for Windows (`opt.exe`, `llc.exe`, `lld.exe`, `LLVM.dll`)
2. Embed via `go:embed` with `llvm_windows_amd64.go` build tag
3. Bundle into release binary (~70MB estimated)
4. Eliminate need for separate LLVM installation

### Phase W4 — CI (after validation)

Add to GitHub Actions CI matrix:

```yaml
- name: windows-amd64
  runner: windows-latest
```

Add release build job for `promise-windows-amd64.exe`.

---

## 7. Key Design Decisions

- **MSVC ABI, not MinGW**: Native Windows experience. MSVC ABI is what Windows developers and tools expect. MinGW adds complexity (different CRT, different ABI, compatibility issues) without real benefit.
- **Visual Studio Build Tools required**: Analogous to Xcode CLT on macOS. Provides the Windows SDK and MSVC runtime libraries needed for linking. Free download. Long-term: embed the minimal required `.lib` files (like musl CRT on Linux) to eliminate this dependency.
- **lld-link, not link.exe**: LLD ships with LLVM, is open-source, and works identically across platforms. No dependency on MSVC's proprietary linker. `lld-link` is LLD's COFF mode — same binary as `ld.lld`, just a different name/symlink.
- **CRITICAL_SECTION, not SRWLock**: CRITICAL_SECTION is the standard Windows mutex. SRWLock is lighter but non-recursive and non-owning — CRITICAL_SECTION is a safer match for `pthread_mutex_t` semantics.
- **No TLS changes needed**: The scheduler's TLS globals (`@__promise_current_p`, etc.) use LLVM's `thread_local` attribute, which compiles to Windows TLS on MSVC targets automatically.
