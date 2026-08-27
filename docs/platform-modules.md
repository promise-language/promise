# Platform Modules Layout

> **Tag:** `platform-modules` — remaining work to complete this document: `mcp__tracker__list --tag platform-modules`

Design document for Promise's platform-facing standard library: the boundary between `modules/std/`
and other `modules/`, what each platform module contains, and why the layout is what it is. This
supersedes the Phase 4 plan in `standard-library.md` (sections 4b–4e), which was written before
catalog module infrastructure existed.

All modules (including std) live under `modules/`. `std` is special only in that it is
auto-imported via an injected `use std as _;`.

---

## 1. The Core Principle

**`modules/std/` = auto-imported into every file, no `use` needed.**
**`modules/<name>/` = explicitly imported, available only when the program says so.**

The difference is **purely ergonomic** — not architectural. Both live under `modules/` and are
compiled the same way. This is the key insight that determines what belongs where.

### The architecture: `std` as a regular catalog module

`std` is a regular catalog module, identical in treatment to `modules/path` or `modules/io`.
The only special behavior is that every source file automatically receives an injected
`use std as _;` (a glob import that merges all public std symbols into the file's scope, no
prefix needed). This is how Python's builtins work; it is how Rust's prelude works.

**How it works**:
- `modules/std/promise.toml` marks std as a module (`[module] name = "std"`)
- `catalog.toml` registers std as an embedded catalog module (no URL/commit — lives in
  `resources/modules/std/`)
- Every parsed file gets an auto-injected `UseDecl{CatalogName: "std", Alias: "_"}` so std
  symbols appear in scope without any `use` statement
- `mergeGlobImport` in `sema/decl.go` flattens all public std exports into `fileScope`
- Std compiles through the normal `CheckWithTarget` path — no special entry point.
  Non-native types (error, Map, Range, Iterator, Stream) are regular module types;
  `populateUniverseTypes()` in `sema/check.go` sets global identity pointers after
  std's declare pass so compiler features (error handling, for-in, map sugar) can
  identify them.
- No `IsStd` flag on AST nodes, no `stdScope` parent chain, no `mergeStdDecls` — std is
  compiled once via the normal module pipeline and cached in the build cache

### Benefits of std as a regular module

**1. Build cache for std.** Std gets its own cached `.o` file. Since std rarely changes, this is
nearly always a cache hit — a meaningful compile-time improvement, especially noticeable in the
test suite.

**2. Simpler compiler.** Removed `mergeStdDecls`, `parseStdFiles`, `stdScope` parent chain,
`IsStd` flags, and the special cases in `resolveModuleScope` and `loadModuleScopes`. Fewer
moving parts = fewer bugs = easier to understand.

**3. Std/modules boundary is purely ergonomic.** Since std compiles like any module, the only
question is "does this auto-import or require `use`?" — not "does this affect binary size or
compile time?" This makes the design conversation simpler and reduces incentive to game the
std/modules line for performance reasons.

### Binary size: the full picture

With `std` as a proper module compiled to LLVM bitcode (`.bc`), and the linker running LTO
(`--lto-O1` on Linux/macOS/WASM), dead code elimination works at IR level across all modules.
Unused std functions are stripped at link time — a hello-world binary contains only what it
actually calls.

**How it works**: Promise's pipeline is `opt -O1 → .bc → linker --lto-O1`. The linker receives
bitcode from each module and performs whole-program optimization: inlining, constant folding,
and DCE across module boundaries. This replaces the old `--gc-sections` approach (which required
`-function-sections` per llc invocation and could only eliminate whole sections, not functions).
WASM uses `--lto-O2` to fold math intrinsics through indirect call chains in the test trampoline.
Windows is the only exception: it uses `opt → llc → .o → lld-link` (MSVC LTO not yet wired up).

Until LTO was in place, binary size WAS affected by what goes in `std/`. But the
effect is bounded — today std is ~28 files of pure Promise code plus some PAL bindings, which
compiles to perhaps 50–100KB of object code. Not megabytes.

### The `std/` membership test

Given the above, the test is not "does this affect binary size?" but rather:

> **Does auto-importing this symbol benefit almost every Promise program?**
> **Would a program that doesn't use it be annoyed to have it in scope?**

API surface noise is the real cost of over-inclusion in `std/`. An AI agent generating code for
a pure computation task sees fewer valid choices if `File.open`, `get_env`, and `exec` are NOT
in scope. Fewer choices = lower probability of picking the wrong one = more correct outputs.

| Candidate | Verdict | Reason |
|---|---|---|
| `print`, `print_line`, `Writer`, `Closer`, `write_line` | **`std/`** | Every program produces output |
| `Vector`, `Map`, `Set`, `Iterator` | **`std/`** | Data structures are universal |
| `Channel`, `Task`, `error`, `assert` | **`std/`** | Concurrency and errors are language-level |
| `Duration`, `Instant` | **`std/`** | Timing is universal; pure value type; zero API noise |
| `File`, `BufReader` | **`modules/io`** | Not needed by compute-only programs; `use io;` is a meaningful signal |
| `read_line`, `read_stdin` | **`modules/io`** | Same PAL path as File; co-locates all stdin/file I/O |
| `get_env`, `args`, `execute` | **`modules/os`** | Process model — irrelevant to library code and pure computation |
| `path.join`, `path.file_name` | **`modules/path`** | Not every program works with paths |
| Higher-level time ops | **`modules/time`** | Calendar/TZ/formatting builds on std; not universally needed |

**The `use` statement as a semantic signal.** Beyond scope noise, `use io;` at the top of a file
tells any reader — human or agent — that this program interacts with the filesystem. This
information has value. Auto-importing everything strips that signal. The module boundary is
documentation.

---

## 2. `\`target(cond)` — Compile-Time Platform Filtering

Implemented in Stage 8q. See `docs/archive/stages.md` for implementation details.

### The feature

A `\`target(condition)` annotation on a function (or type) causes sema to skip the declaration
entirely when the condition does not match the current build target. Only the matching variant
is compiled. Non-matching variants are as if they were never written.

```promise
// modules/std/platform.pr
_platform_path_separator() string `target(windows) { return "\\"; }
_platform_path_separator() string `target(!windows) { return "/"; }

_platform_line_separator() string `target(windows) { return "\r\n"; }
_platform_line_separator() string `target(!windows) { return "\n"; }

type Platform `public {
    get path_separator string `public `global => _platform_path_separator();
    get line_separator string `public `global => _platform_line_separator();
    is_path_separator(char c) bool `public `global { ... }
}
```

This is exactly the `\`test` pattern: `\`test` includes a function only when compiling tests;
`\`target(windows)` includes a function only when compiling for Windows. Same mechanism, same
implementation location (sema declare pass).

### No grammar changes needed

The annotation grammar already accepts parameterized expressions:

```
metaAnnotation : BACKTICK IDENT (LPAREN metaParams RPAREN)?
metaParam      : expression
```

So `\`target(windows)` already parses — `windows` is an identifier expression. `\`target(!windows)`
already parses — `!windows` is a unary NOT expression. `\`target(linux || macos)` already parses.
Sema just needs to evaluate these as target predicates instead of type-checking them as variable
references.

### Condition language

Target identifiers:

| Name | Matches |
|---|---|
| `windows` | x86_64-pc-windows-msvc |
| `linux` | x86_64-unknown-linux-musl (and other Linux triples) |
| `macos` | x86_64/aarch64-apple-darwin |
| `wasm` | wasm32-wasi |
| `posix` | linux or macos (convenience alias for `linux \|\| macos`) |
| `x86_64` | any x86_64 target |
| `aarch64` | any AArch64 target |

Operators: `!` (not), `\|\|` (or), `&&` (and), `()` grouping.

### Implementation

Entirely in sema (`sema/target.go`):

- `TargetInfo` struct (OS string, Arch string) on `Checker`
- `matchesTarget()` evaluated in declare pass (pass 1) — non-matching declarations are skipped
- `ParseTargetInfo(triple)` derives OS/Arch from LLVM target triple
- 11 codegen sites check `FilteredDecls[decl]` to skip filtered declarations
- Module loader passes target info so dependencies also filter correctly
- `promise doc` uses `HostTargetInfo()` — shows declarations for the current host platform

No grammar changes were needed. `` `target `` is used in production code in `modules/std/platform.pr`
(Platform constants) and in e2e tests (`tests/e2e/test_target_filter.pr`).

### Where `\`target` applies vs where it does not

There are 36 `isWasm`/`isWindows` branches in Go codegen. Most are deep IR-level:
scheduler coroutines (WASM is cooperative, no threads), setjmp/longjmp for panic recovery,
test trampolines, coroutine ABI differences. These cannot move to Promise source — they are
about the shape of the generated IR, not about user-visible API. `\`target` does not touch these.

`\`target` applies to **user-visible platform behavior**: functions whose existence or
implementation differs by target in a way the Promise programmer should see and reason about.

Concrete inventory across std and modules:

| Function/Type | Target variants | Currently handled by |
|---|---|---|
| `Platform.path_separator` | `windows` / `!windows` | `std/platform.pr` global getter |
| `Platform.line_separator` | `windows` / `!windows` | `std/platform.pr` global getter |
| `Platform.is_path_separator(char)` | `windows` / `!windows` | `std/platform.pr` global method |
| `is_absolute(string) bool` | Windows drive letters vs POSIX `/` | `modules/path/path.pr` using `Platform.is_path_separator` |
| `sleep(Duration)` | WASM no-op vs `nanosleep` | Go codegen `if c.isWasm` |
| `File` type | `!wasm` only (no filesystem on WASM) | `modules/io/io.pr` — 4 factory constructors, handle methods, one-shot helpers |
| `read_line/read_stdin` | `!wasm` only | `modules/io/io.pr` — free functions, `read_line` returns `string?!` (optional+failable) |
| `execute(...)`, `Process` | `!wasm` only (no subprocess on WASM) | `modules/os/os.pr` — one-shot `execute`, streaming `Process`/`ProcessInput`/`ProcessOutput` |
| `args` (getter) | different impl on WASM | `modules/os/os.pr` — returns `string[]` from argc/argv globals |
| `get_env_var(string)`, `env` | `!wasm` (WASM env access varies) | `modules/os/os.pr` — `get_env_var(string) string?`, `env` getter |

That's ~18 `\`target` annotation uses across std+modules. This replaces an equal number of
Go codegen special cases with Promise source that is visible, readable, and owned by the module
author rather than the compiler internals.

### The boundary: `\`target` vs PAL

**PAL** = runtime OS syscall differences. `pal_file_open` on POSIX calls `open(2)`;
on Windows calls `CreateFile`. Both exist at runtime, just with different implementations.
PAL handles this in Go codegen — the generated IR is different but the function exists on both.

**`\`target`** = the function should not exist at all on some targets. `os.execute()` is not a
"different implementation on WASM" — it is absent. `File.open` is not "calls a different
syscall on WASM" — there is no filesystem. `\`target(!wasm)` on `File` means WASM programs
that import `modules/io` and try to use `File` get a sema error at compile time, not a runtime
failure. That is strictly better.

### Example: `modules/std/time.pr` sleep

Currently `defineNanotimeSleepFunc` in `io.go` has:

```go
if c.isWasm {
    // emit empty function body
    return
}
// emit nanosleep(2) wrapper
```

With `\`target`:

```promise
// modules/std/time.pr
_sleep_nanos(int ns) `extern("promise_sleep_nanos") `target(!wasm);

sleep(Duration d) `public `target(!wasm) {
    _sleep_nanos(d.as_nanos);
}

sleep(Duration d) `public `target(wasm) {
    // WASM: sleep is a no-op — no blocking syscall available
}
```

The WASM no-op is now explicit and visible in source. The Go codegen `if c.isWasm` branch
for `promise_sleep_nanos` is removed. The information lives where it belongs: in the Promise
std source, not in Go internals.

### Exhaustiveness

The compiler does not verify that `\`target` variants cover all targets. If a function is only
defined with `\`target(windows)` and a Windows program calls it, it works. On Linux, the function
is undefined — caller gets a "function not found" error. This is correct: incomplete coverage
is a programmer error caught at compile time.

A convention: when writing two variants that are intended to be exhaustive, write `!X` and `X`
as the pair. For three targets, write all three explicitly or use a catch-all with no `\`target`
annotation as the default (no annotation = always compiled).

---

## 3. Platform Constants: `Platform` Type

Platform constants live in `modules/std/platform.pr` as global getters and methods on a `Platform`
namespace type. All `` `target `` annotations are confined to `std/` — external modules like
`modules/path/` call `Platform.path_separator`, `Platform.is_path_separator(c)`, etc.

```promise
// modules/std/platform.pr
_platform_path_separator() string `target(windows) { return "\\"; }
_platform_path_separator() string `target(!windows) { return "/"; }

_platform_line_separator() string `target(windows) { return "\r\n"; }
_platform_line_separator() string `target(!windows) { return "\n"; }

_platform_is_path_separator(char c) bool `target(windows) { return c == '/' || c == '\\'; }
_platform_is_path_separator(char c) bool `target(!windows) { return c == '/'; }

type Platform `public {
    get line_separator string `public `global => _platform_line_separator();
    get path_separator string `public `global => _platform_path_separator();
    is_path_separator(char c) bool `public `global { return _platform_is_path_separator(c); }
}
```

`Platform.line_separator` is in `std` (not `modules/path`) because line endings affect all text
output, including programs that never touch the filesystem. `write_line` uses
`Platform.line_separator` rather than hardcoding `"\n"`.

**`print_line` and `\r\n`**: `print_line` always emits `\n`. Modern Windows terminals (Windows Terminal,
VS Code, PowerShell) handle `\n` correctly. `print_line` is for human-readable terminal output.
Use `write_line` when writing to files or pipes where the platform convention matters.

---

## 4. `modules/std/time.pr` — Stays in `std`

`Duration` and `Instant` are already implemented in `modules/std/time.pr` and the question is whether
they should move to `modules/time`.

**They stay in `std`** for two reasons:

1. `Duration` is a pure value type (`int nanos \`value`). It has no PAL dependency and zero binary
   cost beyond the type definition. It is used everywhere: timeouts, retry delays, benchmarking,
   rate limiting.

2. `promise_nanotime` is already emitted in every binary (used by the test runner infrastructure).
   `Instant.now()` calling it costs nothing additional.

`sleep(Duration)` stays in `modules/std/time.pr` with a `\`target` pair — the WASM no-op variant is
explicit in source (see §2 example). The `promise_sleep_nanos` Go codegen branch is removed.

**`modules/time`** then becomes the home for *higher-level* time operations that build on top of
`modules/std/time.pr`: calendar date/time representation, time zone handling, formatted parsing and
serialization (RFC 3339, Unix timestamps). These are not universally needed and are significantly
heavier.

---

## 5. `modules/path` — Path Manipulation

Pure string operations, platform-aware via `Platform.is_path_separator`.

### Current API

```promise
join(string base, string child, ...string rest) string  // join two or more path components
file_name(string path) string                           // last path component
parent(string path) string                              // all but last component
extension(string path) string                           // ".txt" or ""
stem(string path) string                                // file_name without extension
is_absolute(string path) bool
is_relative(string path) bool
split(string path) string[]                             // split into all components
normalize(string path) string                           // collapse ., .., redundant separators
```

Platform awareness is handled via `Platform.is_path_separator(c)` and `Platform.path_separator`
from `std/platform.pr`. All `` `target `` annotations are confined to std — the path module
contains no target-filtered code.

**What NOT to add**: filesystem operations (`exists`, `is_dir`, `stat`). Those require syscalls and
belong in `modules/io`. The module comment "no filesystem access" is correct and intentional.

---

## 6. API Design Principles

Two principles that apply to all module APIs:

### Principle 1: Minimize top-level namespace pollution — group under types

Free functions with a noun prefix (`read_file`, `write_file`, `remove_file`) pollute the module
namespace and repeat the noun in every call. Operations that conceptually belong to a type should
live as type methods:

- **`\`factory`** — construction returning `Self`. Already used for `File.open`, `File.create`.
- **`\`global`** — static method with no receiver, any return type. Already fully implemented.
  Called as `File.read("path")`, `File.exists("path")`, etc.
- **`\`mono`** — same as `\`global` but for generic types.

Result: an AI agent looking for "how do I read a file" searches `File.` and finds everything.
An agent looking for "what's in modules/io" sees three types and two free functions — not fifteen.

The rule: if an operation is logically about a type, it lives on that type. Only operations with
no clear type owner live as module-level free functions.

### Principle 2: Getters over zero-parameter functions

Zero-parameter functions that read a property (not perform an action) should be getters — no `()`.
This reduces visual noise and signals the right semantics to readers.

```promise
// Function: signals "doing something"
Platform.path_separator()   // if it were a function — requires ()

// Getter: signals "reading a property"
Platform.path_separator     // global getter — no () — cleaner, reads like a variable
```

**Where this applies today**:
- `Platform.path_separator`, `Platform.line_separator` — compile-time constants, clearly properties
  (implemented as `` `global `` getters on the `Platform` type)
- `args` — set once at startup, effectively readonly after that

**Implementation state**:
- Type-level getters: fully implemented. `get name type { ... }` inside a type body.
- `` `global `` getters on types: fully implemented. `get name type \`public \`global { ... }`
  inside a type body. No receiver, called as `TypeName.getter_name`.
- **Module-level getters**: fully implemented. `get name type { ... }` at file/module scope.
  Grammar includes `getterDecl | setterDecl` in `topLevelDecl`. Sema declares getters as
  zero-arg functions and setters as one-arg void functions (with `$set` suffix). Codegen emits
  calls on property access and assignment.

`os.args`, `os.executable_path`, and `os.working_dir` are now module-level getters.
Failable getters like `working_dir` use `!` unwrap at the call site (`os.working_dir!`).
`get_env_var(name)` remains a function because it takes a parameter.

---

## 7. `modules/io` — File I/O and Standard Input

`modules/io/io.pr` holds `File`, `BufferedReader`, `BufferedWriter`, `Dir`, `IoError`, and the `read_line()` / `read_stdin()` free functions.

### I/O Architecture: Reactor + Async/Sync Backends

All file and network I/O goes through a **global reactor** (one per process, like Go's netpoller).
The reactor uses the best available async mechanism per platform:

| Platform | Async backend | Fallback |
|---|---|---|
| Linux | io_uring (probed at init via `io_uring_setup` — ENOSYS means unsupported) | epoll → blocking syscalls |
| macOS | kqueue | blocking syscalls |
| Windows | IOCP (truly async file + network I/O) | N/A — IOCP is always available |
| WASM | (TBD — see WASM section) | N/A |

**User-facing API is always synchronous.** `File.read`, `File.write`, etc. look like blocking
calls from Promise code. Under the hood, the goroutine parks (via the existing `coro.suspend` +
`park_mutex` mechanism) and the reactor wakes it when I/O completes. This is identical to how
channel operations work — the goroutine suspends transparently.

**Global reactor** (like Go's `runtime/netpoll`):
- Single poller instance, thread-safe (epoll_wait/kevent are safe from any thread)
- Sysmon calls `reactor_poll(0)` (non-blocking) every tick to harvest ready fds
- Idle Ms call `reactor_poll(block)` in `park_m` instead of just `cond_wait`
- Ready goroutines injected back onto P run queues via existing `enqueue_goroutine`
- No per-P reactors — avoids fd registration migration during work stealing

**Sync fallback**: When async is unavailable (old kernels, unsupported platforms), I/O ops
block the M thread directly. The goroutine parks via the existing `park_mutex` mechanism.
The scheduler can spin up additional Ms to maintain parallelism (same as Go's `entersyscall`).

### Error type

```promise
type IoError is error `public `doc("An operating system I/O error.") {
    int code;  // errno on POSIX, GetLastError on Windows

    `doc("Human-readable description of the error code.")
    message() string `native;  // manual table for top 20 errno values; fallback: "error {code}"
}
```

### `File` — factory constructors and handle methods

Factory constructors are the **complete** set of ways to open a file. No string modes — the
intent is in the constructor name, validated at compile time.

Applying §6 principles: one-shot convenience operations use longer names (prefixed with
`read_content`/`write_content`). The module-level namespace has `File`, `Dir`, `IoError`,
`read_line`, `read_stdin`.

```promise
type File `public `doc("A file handle. Satisfies Reader, Writer, and Closer.") {
    int _fd;

    // ── Factory constructors (short — fundamental) ──────────────────────────
    `doc("Opens an existing file for reading and writing. Fails if not found.")
    open!(string path) Self `factory `public;

    `doc("Opens an existing file for reading only. Fails if not found.")
    read!(string path) Self `factory `public;

    `doc("Creates a new file or truncates an existing one. Opens for reading and writing.")
    create!(string path) Self `factory `public;

    `doc("Opens or creates a file for appending. Read and write, seek position at end.")
    append!(string path) Self `factory `public;

    // ── Handle methods (instance — require an open file) ───────────────────
    read!(~this, u8[] ~buf) int `public; // satisfies Reader
    write!(~this, u8[] &buf) int `public; // satisfies Writer

    `doc("Closes the file handle. Returns error on failure (double-close returns error, does not panic).")
    close!(~this) `public;

    `doc("Reads all remaining content into a string.")
    read_all!(~this) string `public;

    `doc("Writes a string to the file.")
    write_string!(~this, string s) `public;

    `doc("Returns the current byte position.")
    tell!(~this) int `public;

    `doc("Seeks to a byte offset from the start.")
    seek!(~this, int offset) `public;

    // ── One-shot convenience (global — longer names) ────────────────────────
    `doc("Reads the entire contents of a file as a string. Opens, reads, auto-closes.")
    read_content!(string path) string `global `public;

    `doc("Writes content to a file, creating or truncating it. Opens, writes, auto-closes.")
    write_content!(string path, string content) `global `public;

    `doc("Returns true if the path exists (file, directory, or symlink).")
    exists(string path) bool `global `public;

    `doc("Returns the size of the file in bytes.")
    size!(string path) int `global `public;

    `doc("Removes a file. Use Dir.remove for directories.")
    remove!(string path) `global `public;

    drop(~this) { close(~this) ? _ {}; }  // auto-close on scope exit; ignore error
}
```

Usage:

```promise
use io;

// Factory constructors — short, fundamental
f := File.read!("config.json");
string data = f.read_all!();

// One-shot — longer, convenience (auto-closes via drop)
string content = File.read_content!("data.txt");
File.write_content!("out.txt", result);

// Equivalent to read_content (explicit):
f2 := File.read!("data.txt");
string same = f2.read_all!();
// f2 auto-closed by drop
```

**Double-close semantics**: `close()` on an already-closed file returns `IoError`, does NOT
panic. Closing a file multiple times is a logic error but not a catastrophic flow error like
double-closing a channel (which corrupts waiter lists). The `drop` ignores close errors so
auto-close after explicit close is safe.

**Embedded filesystem compatibility**: The instance methods (`read`, `write`, `read_all`,
`close`) work through structural interfaces (`Reader`, `Writer`, `Closer`). A future `EmbedFS`
type would have its own factory (`assets.open!("path") `) but return handles satisfying the same
interfaces. User code taking `Reader &r` works identically with real and embedded files.

### `Dir` — directory operations

Directory operations live on a separate `Dir` namespace type, not on `File`. Directories are
not files — conflating them adds noise to `File` and misleads readers about what `File` does.

```promise
// TODO: Add `namespace annotation to prevent instantiation.
type Dir `public `doc("Directory operations. All methods are global — no Dir instances needed.") {

    `doc("Creates a directory. Parent must exist.")
    make!(string path) `global `public;

    `doc("Creates a directory and all missing parent directories.")
    make_all!(string path) `global `public;

    `doc("Returns the names (not full paths) of entries in a directory.")
    list!(string path) string[] `global `public;

    `doc("Removes an empty directory.")
    remove!(string path) `global `public;

    `doc("Returns true if the path is a directory.")
    exists(string path) bool `global `public;
}
```

Usage:

```promise
use io;

Dir.make_all!("/tmp/myapp/cache");
string[] entries = Dir.list!("/tmp/myapp");
if Dir.exists("output/") {
    Dir.remove!("output/old");
}
```

### `BufReader`

Deferred until real usage drives the design. The structural interface (`Reader`) means a
`BufReader` wrapper can be added later without changing `File` or any code that reads through
`Reader`.

### `read_line()` and `read_stdin()`

These stay as free functions — they have no type to belong to (stdin has no `File` handle in
normal use) and they are the primary interaction pattern for CLI programs.

```promise
`doc("Reads one line from stdin. Returns absent at EOF, raises IoError on read error.")
read_line!() string? `public;

`doc("Reads all of stdin into a string.")
read_stdin!() string `public;
```

**`string?!`** — the function can return `present("line")`, `absent` (EOF), or raise `IoError`.
The `while` loop unwraps the optional automatically:

```promise
use io;

while line := read_line!() {
    print_line(line);
}
```

### WASM file I/O

**Open question.** WASI Preview 1 provides `fd_read`, `fd_write`, `path_open` — basic file I/O
is possible on WASI-capable runtimes (Wasmtime, WasmEdge). Whether to support this depends on:
- Do we target WASI P1 only, or also browser/non-WASI environments?
- WASI P2 (component model) has a different I/O story

For now: `\`target(!wasm)` on `File` and `Dir`. WASM programs that try to use `io.File` get a
compile-time error. This can be relaxed later when WASI support is designed properly.

### PAL additions (implemented)

```
pal_file_open(i8* path, i32 mode) i32                 // fd or -1; mode: 0=rw, 1=ro, 2=create-trunc, 3=append
pal_file_read(i32 fd, i8* buf, i64 len) i64           // bytes read, 0=EOF, -1=error
pal_file_write(i32 fd, i8* buf, i64 len) i64          // bytes written or -1
pal_file_close(i32 fd) i32                            // 0 or -1
pal_file_seek(i32 fd, i64 offset, i32 whence) i64     // new position or -1
pal_file_stat_size(i8* path) i64                      // file size or -1 (uses open+lseek+close)
pal_file_remove(i8* path) i32                         // 0 or -1
pal_file_exists(i8* path) i32                         // 1 = exists, 0 = not found
pal_file_mkdir(i8* path) i32                          // 0 or -1
pal_dir_remove(i8* path) i32                          // 0 or -1
pal_dir_exists(i8* path) i32                          // 1 = is directory, 0 = not
pal_errno() i32                                       // current thread-local errno value
```

**Design decisions:**
- `pal_file_open` takes a mode enum (not raw O_* flags) — the PAL maps to platform-specific constants internally. This avoids leaking POSIX vs Windows flag differences.
- `pal_file_stat_size` uses open+lseek(SEEK_END)+close instead of `stat()` — avoids `struct stat` layout differences between macOS and Linux.
- `pal_file_stat` (D0012) cannot dodge that and GEPs into `struct stat` by byte offset, so it carries a table per platform **and per architecture**: Linux/aarch64 declares a 32-bit `st_mode` before `st_nlink`, where Linux/x86_64 has a 64-bit `st_nlink` first, moving `st_mode`/`st_uid`/`st_gid`. Darwin shares one layout across arches. A wrong table reads zeros rather than failing, so the offsets are pinned by `TestFileStatPosixOffsets`.
- POSIX `pal_dir_exists` uses opendir/closedir instead of `stat()` for the same reason.
- Windows uses UCRT POSIX wrappers (`_open`, `_read`, etc.) with `_O_BINARY` always set. `pal_dir_exists` uses `GetFileAttributesA` since UCRT has no `opendir`.
- WASM stubs return -1 (error) or 0 (not found) for all file ops — no filesystem access yet.
- `pal_errno` uses `__errno_location()` (Linux), `__error()` (macOS), `_errno()` (Windows).

**Dir listing PAL (implemented):**
```
pal_dir_open(i8* path) i8*                            // DIR*/state ptr or null
pal_dir_next_name(i8* handle) i8*                     // d_name ptr or null (end/error)
pal_dir_close(i8* handle) void                        // closedir / FindClose+free
```

- POSIX uses `opendir`/`readdir`/`closedir`. `d_name` offset: 19 bytes (Linux), 21 bytes (macOS).
- Windows uses `FindFirstFileA`/`FindNextFileA`/`FindClose` with a 344-byte heap-allocated state struct (HANDLE + first-entry flag + WIN32_FIND_DATAA). `cFileName` at offset 60 in state.
- WASM stubs return null (dir open) or no-op (close).

**Codegen bridges** (`file_io.go`):
- `promise_io_file_read_line` — byte-by-byte `pal_file_read` loop, 256-byte initial buffer with doubling growth, strips `\r\n`/`\n`, sets errno `0x7FFF0001` on EOF.
- `promise_io_dir_open` — string→cstr, `pal_dir_open`, cast handle to i64, returns `-errno` on null.
- `promise_io_dir_next_name` — cast int→ptr, `pal_dir_next_name`, `strlen`+`promise_string_new`.
- `promise_io_dir_close_handle` — cast int→ptr, `pal_dir_close`.

### Syscall Handoff PAL (Phase 6a)

File IO on POSIX cannot be async via epoll/kqueue (regular files always report "ready").
Instead, goroutines release their P before blocking syscalls so other Gs can run:

```
promise_sched_enter_syscall()   // detach P from M, clear P.current_g, wake idle M
promise_sched_exit_syscall()    // reattach P to M, restore P.current_g
```

These are scheduler functions (not PAL), emitted in `codegen/sched.go`. They wrap every
blocking PAL call in `file_io.go`. On WASM, both are no-ops (single-threaded).

Requires `@__promise_current_m` TLS global so `exit_syscall` can find its M and reattach P.

### Reactor PAL (Phase 6b)

```
// Reactor struct: known LLVM type (not opaque i8*) to prevent codegen drift.
// Fields: platform fd (kqueue fd / epoll fd / IOCP handle), event buffer, count, lock.
pal_reactor_create() %ReactorStruct*
pal_reactor_register(%ReactorStruct* reactor, i32 fd, i32 events) i32  // 0 or -errno
pal_reactor_poll(%ReactorStruct* reactor, %EventStruct* events_buf, i32 max, i64 timeout_ns) i32  // count or -errno
pal_reactor_deregister(%ReactorStruct* reactor, i32 fd) i32  // 0 or -errno
```

The reactor PAL abstracts over epoll/kqueue/IOCP. `timeout_ns = 0` is non-blocking (sysmon),
`timeout_ns = -1` is blocking (idle M). Known struct types prevent silent breakage when
the reactor evolves — all codegen sites see the same typed fields.

**WASM**: Reactor is not applicable (single-threaded, cooperative scheduler). Future: JS
event loop integration for browser-based async IO. The cooperative scheduler would yield
to the browser event loop, and JS callbacks re-enqueue goroutines when IO completes.

---

## 8. `modules/os` — Operating System Interface

Applying §6 principles: `args`, `executable_path`, and `working_dir` are module-level
getters (accessed as `os.args`, `os.executable_path`, `os.working_dir!`). `exit_process`
and `execute` stay as functions (they perform actions). `get_env_var` stays as a function
because it takes a parameter.

Note: `exit_process` instead of `exit` because the bare name collides with libc's `@exit` symbol
when the module is compiled inline (e.g., module tests). The qualified form `os.exit_process(code)`
is clear enough.

```promise
type OsError is error `public `doc("An operating system error.") {
    int code;
}

get_env_var(string name) string? `public
    `doc("Returns the value of the named environment variable.
          Returns none if the variable is not defined.
          An empty string is returned when the variable is set but empty.");

get working_dir! string `public
    `doc("Returns the absolute path of the current working directory.
          Raises an error if the OS call fails.");

exit_process(int code) `public
    `doc("Terminates the process immediately with the given exit code.
          Does not return. Does not run destructors or cleanup.");

get args string[] `public
    `doc("Returns the command-line arguments passed to the program,
          excluding the executable path.");

get executable_path string `public
    `doc("Returns the path to the running executable as provided by the
          operating system (argv[0]).");

// --- Subprocess execution (implemented) ---

type ProcessResult `public `doc("The result of executing a subprocess.
      Contains the exit code and captured standard output and standard error.") {
    int exit_code;
    string stdout;
    string stderr;
}

execute!(string program, ...string args) ProcessResult `public
    `doc("Executes a program with the given arguments and waits for it to complete.
          Accepts arguments inline or as a pre-built string[].
          Returns a ProcessResult containing the exit code and captured standard
          output and standard error. Raises an error if the process could not be
          started. The program is searched using the system PATH. If the program
          is not found, the child process exits with code 127.");

// --- Environment mutation (implemented) ---

set_env_var(string name, string? move value) `public
    `doc("Sets or removes the named environment variable.
          When value is present, sets the variable. When absent, removes it.");

// --- Working directory mutation (implemented) ---

set_working_dir(string path) ! `public
    `doc("Changes the current working directory to the given path.
          Raises an error if the path does not exist or is not a directory.");

// --- Signal handling (implemented) ---

enum Signal `public `doc("A POSIX signal type. Use with setup_signal_handling() and receive_signal() to handle signals.") {
    Interrupt `doc("SIGINT (2) — sent by Ctrl+C."),
    Terminate `doc("SIGTERM (15) — default signal sent by kill command."),
    Hangup `doc("SIGHUP (1) — sent when the terminal is closed."),
}

setup_signal_handling!(...Signal signals) `public
    `doc("Sets up signal handling for the given signals. Creates an internal pipe and
          registers signal handlers on first call. Subsequent calls register additional
          signals. After calling this, use receive_signal() to block until a signal arrives.");

receive_signal!() Signal `public
    `doc("Blocks until a registered signal arrives. Returns the Signal that was received.
          Raises if the signal pipe read fails or the signal number is unrecognized.");

// Signals are delivered over a self-pipe and consumed by an ordinary blocking read, so
// there is no async-signal-unsafe callback: the receiving goroutine parks like any other
// reader. This is why the surface is a blocking receive rather than a registered handler.

// --- Streaming subprocess (implemented, in modules/os) ---
// Q4 below asked whether piped stdin/stdout/stderr belonged in a separate
// modules/process. It did not: Process / ProcessInput / ProcessOutput
// (spawn/wait/kill) live here next to the one-shot execute, because they share
// the same PAL fork/exec bridge.
```

**Implementation notes**:

The first three functions use the **extern bridge** pattern: Promise declares
`_os_func() T \`extern("promise_os_func");` and codegen provides the LLVM IR body in
`os_bridges.go`, bridging Promise types ↔ raw PAL syscall wrappers. Two reusable bridge
infrastructure patterns were introduced:

- **Failable extern bridges** (`failable_bridge.go`): Bridge writes `{i1, T, i8*}` result
  struct matching regular failable functions. `genExternCall` loads the result directly.
  Used by `get_working_dir`.
- **Optional extern bridges**: Bridge writes `{i1, T}` optional struct. `genExternCall`
  detects `*types.Optional` return types and loads directly. Used by `get_env_var`.
- **Error construction** (`constructErrorFromCStr`/`constructErrorFromGlobalStr`): Allocates
  error instances with RTTI and message fields in LLVM IR, for use in bridge error paths.

**`args`/`executable_path` implementation**: The C `main(argc, argv)` stores both values into
globals (`@__promise_argc`, `@__promise_argv`) at the start of the entry point, before scheduler
init. The bridge functions (`defineArgsBody`, `defineExecutableBody`) read these globals and
build the return values. `args` skips `argv[0]` (program name) and returns `argv[1..argc-1]`
as a `Vector[string]`. `executable_path` returns `argv[0]` as a string.
On WASM, `_start` passes `argc=0, argv=null` — both getters return empty results.

**`execute()` implementation**: Three-layer architecture — PAL (raw OS calls) → Bridge (type
conversion in `os_bridges.go`) → Promise wrapper (constructs `ProcessResult`). Decomposed into
fine-grained PAL primitives: `pal_spawn` (fork+exec+pipe, returns pid + fd out-params),
`pal_read_pipe` (read fd to EOF into malloc'd buffer), and `pal_wait_pid` (waitpid with EINTR
retry). The bridge layer uses TLS globals to cache spawn file descriptors between extern calls.
The Promise-level `execute()` calls `_os_spawn`, retrieves fds, reads both pipes concurrently
using goroutines (`go _os_read_pipe(stderr_fd)` for stderr, `_os_read_pipe(stdout_fd)` for stdout),
and waits for exit code. If the program is not found, the child `_exit(127)`.

**PAL functions** (POSIX/Windows/WASM):
```
pal_getenv(i8* name) i8*               // pointer to value or null
pal_getcwd(i8* buf, i64 len) i8*       // fills buf, returns pointer or null on error
pal_spawn(i8* prog, i8** argv, i32* out_stdout_fd, i32* out_stderr_fd) i32
                                           // fork+execvp+pipe; returns pid or -1
pal_read_pipe(i32 fd, i8** out_buf, i64* out_len) void
                                           // read fd to EOF, close fd; caller frees buffer
pal_wait_pid(i32 pid) i32              // waitpid with EINTR retry; returns exit code or -1
```

---

## 9. `modules/time` — Higher-Level Time Operations

`modules/std/time.pr` owns the monotonic primitives — `Duration`, `Instant`, and `sleep`. The `modules/time` catalog module builds wall-clock utilities on top of those: `DateTime.now()`, `Date`, `Time`, `from_unix_secs`, UTC offsets, and ISO-8601 (RFC 3339) format/parse.

```promise
use time;

// Calendar date/time representation
type DateTime `public {
    int year;
    int month;   // 1–12
    int day;     // 1–31
    int hour;    // 0–23
    int minute;  // 0–59
    int second;  // 0–59
    int nano;    // 0–999999999

    now() Self `factory `public;  // current local time (calls get_wall_clock PAL)
    from_unix(int secs) Self `factory `public;
    to_unix() int `public;

    format(string layout) string `public;  // strftime-style, or RFC 3339 subset
    parse!(string s, string layout) Self `factory `public;
}

// Timezone-agnostic conversions
unix_now() int `public;  // seconds since Unix epoch
```

The PAL function for wall clock time (`pal_wall_clock` / `CLOCK_REALTIME`) belongs here, not in
`modules/std/time.pr` (which uses `CLOCK_MONOTONIC` for `Instant.now()`).

`modules/time` depends on `modules/std/time.pr` (for `Duration`, `Instant`) but not vice versa.

---

## 10. Future Platform Modules

One platform-facing module from the original list is still unbuilt, and unlike the planned
entries in `catalog.toml` (`toml`, `yaml`, `mcp`, …) it has no catalog name reserved yet:

| Module | Content | Key dependency |
|---|---|---|
| `modules/fs` | Advanced filesystem: symlinks, watch, temp files | PAL extensions |

`modules/net`, `modules/http`, `modules/json`, and `modules/crypto` were on this list and have
since shipped — see `docs/standard-library.md` §1 for their surfaces. `modules/process` was never
created: streaming child-process I/O landed inside `modules/os` as `Process`/`ProcessInput`/
`ProcessOutput`, next to the one-shot `execute`, because it shares the same PAL fork/exec bridge.

None of this belongs in `std/` — catalog modules are progressively heavier and progressively
less universal, which is exactly why they are imported explicitly rather than auto-imported.

---

## 11. Complete Layout

```
modules/
  std/            (auto-imported via implicit `use std as _;`)
    io.pr         — print, print_line, write_line, Closer
    platform.pr   — Platform (path_separator, line_separator, is_path_separator)
    format.pr     — Writer, Format, Builder
    parse.pr      — Reader, Parse, Scanner, scan[T]
    time.pr       — Duration, Instant, sleep
    math.pr       — PI, sqrt, sin, min, max, ...
    string.pr     — string methods
    vector.pr     — Vector[T]
    map.pr        — Map[K,V]
    iter.pr       — Iterator[T], combinators
    ...

  path/           — join, file_name, parent, extension, stem, split, normalize
  math/           — lerp, map_range, deg_to_rad, sign_f64
  strings/        — join, spaces, reverse, ...
  io/             — File, Dir, IoError, read_line, read_stdin
  os/             — args, get_env_var, working_dir, execute, Process
  time/           — DateTime, Date, Time; now, from_unix_*, parse
  json/           — JsonEncoder, JsonDecoder, JsonValue
  net/            — TcpListener, TcpStream, NetError
  http/           — HTTP/1.1 client and server, http:// and https://
  tls/            — TlsConfig, TlsServerConfig, TlsStream, TlsListener, TlsError
  gzip/           — gzip_encode, gunzip, deflate, inflate, crc32
  crypto/         — Sha256, Digest256, constant_time_equal
  encoding/       — hex_encode, hex_decode, EncodingError
```

**The invariant**: the `std` module is auto-imported everywhere (convenience), but compiled and
cached exactly like any other module. `modules/` are explicit imports. The distinction is about
API surface and the semantic signal of `use`, not about compilation mechanics.

---

## 12. Open Design Questions

These need resolution before implementation:

**Q1: `IoError.message` source**
Use `strerror` (libc, locale-aware, one call) or a manual switch over the top-20 errno values?
**Lean**: manual table — keeps binaries fully self-contained and avoids locale dependency. Fallback
string `"errno {code}"` for unrecognized values.

**Q2: `IoError` vs `OsError` — is this split worth it?**
Both carry an `int code` and a `message` property. One shared `SysError is error` type (in a new thin
`std/sys.pr`) avoids the duplication. But it bleeds OS error types into `std/`.
**Lean**: keep them separate in their respective modules — the type system makes them
distinguishable; programs that handle errors from both can use the base `error` type for shared
handling.

**Q3: `args()` includes program name or not?**
Go: `os.Args[0]` is the program name. Rust: `std::env::args().next()` is the program name.
**Decision**: exclude it — `args()` returns `argv[1..]`. Use `executable_path()` for `argv[0]`.

**Q4: `execute` API — blocking only for now?**
The `ProcessResult` design is synchronous (blocks until the subprocess exits, captures all output in
memory). The open question was whether streaming subprocess I/O (piped stdin/stdout/stderr) should
become a separate `modules/process`.
**Resolved**: no separate module. `modules/os` ships both — the blocking convenience `execute` and
streaming `Process` / `ProcessInput` / `ProcessOutput` (spawn/wait/kill) — because they share the
same PAL fork/exec bridge and splitting them would have duplicated it.

**Q5: `read_line()` — strip trailing `\r\n` or just `\n`?**
**Lean**: strip both `\n` and `\r\n` — returns the line content without any line terminator.
This is the universal expectation and handles files created on Windows read on POSIX and vice versa.

---

## 13. `modules/tls` — Transport Security Backends

`modules/tls` layers TLS over an owned `net.TcpStream`. The design point that makes
it portable is that **no backend ever sees a socket, an fd, or the scheduler**: the
TLS engine is driven through in-memory ciphertext buffers, and Promise code
(`tls.pr`) pumps those buffers over `net.TcpStream`, which already parks on the
reactor. So TLS reads park exactly like plain TCP reads, on one event loop.

This is also a hard constraint, not just a preference. Promise parks by emitting an
inline `coro.suspend` into the Promise frame (`promise_netpoll_wait_read`/`_write`
are intercepted at codegen dispatch and lowered inline), so a coroutine **cannot**
suspend from inside a C stack frame. Any backend callback invoked from within the
TLS library must therefore be a pure buffer operation that never blocks and never
performs I/O.

### Backend availability

| Target | Backend | Protocol | Link surface |
|---|---|---|---|
| Linux | OpenSSL 3.x memory BIOs, vendored musl-static (T1596) | TLS 1.2 + 1.3 | static `libssl.a` / `libcrypto.a` |
| macOS | Secure Transport with buffer queues (T1599) | **TLS 1.2 only** | `Security.framework`, `CoreFoundation.framework` |
| Windows | SChannel with its own buffer queues (T1598) | TLS 1.2 + 1.3 | `secur32` / `crypt32` / `ncrypt` import libraries |
| wasm | none (no sockets) | — | — |

Targets without a backend still compile and link: the `promise_tls_*` bridges become
inert stubs returning a 0 handle, and the constructors raise
`TlsError(kind: unsupported)`. Gating is by handle, not by `` `target() ``.

### The shared PAL surface

Every backend implements the same 25 `pal_tls_*` entry points and the same
backend-neutral status enum — `0 ok, 1 want_read, 2 want_write, <0 fatal` for the
handshake, `>0 bytes / 0 EOF / -1 want_read / -2 want_write / -3 fatal` for read and
write. **No platform-specific status code may cross the PAL boundary**
(`errSSLWouldBlock`, `SSL_ERROR_WANT_READ`, `SEC_E_INCOMPLETE_MESSAGE`, … are all
mapped inside the backend), so every bridge helper in `codegen/tls.go` and all of
`tls.pr` is shared verbatim.

The `bio_read_out` / `bio_write_in` / `bio_pending_out` names are OpenSSL heritage;
on macOS and Windows they operate on the session's own byte queues.

PEM inputs must also behave identically. A multi-block PEM — the ordinary
`fullchain.pem` shape (leaf plus its issuers), or a CA bundle — is accepted by every
backend, and all of them use the leaf and ignore the rest. On macOS that means the
import helper picks the first item of the wanted kind out of `SecItemImport`'s result by
`CFTypeID`: the reported `SecExternalItemType` for a multi-block PEM is
`kSecItemTypeAggregate`, so gating on it would reject every real-world certificate
bundle that Linux accepts.

### macOS: why Secure Transport, and the TLS 1.3 gap

Network.framework was rejected deliberately. It owns the socket *and* its own
libdispatch event loop, so completions arrive on threads the M:N scheduler does not
own and the reactor never sees the fd — TLS would get a wholly separate I/O path
from plain TCP, and everything built on reactor parking (socket deadlines,
cancellation, select) would need reimplementing for TLS alone. Promise already has a
non-blocking reactor; it needs the platform to stay out of the way, not to supply a
second one.

The cost of that choice is protocol coverage: **Secure Transport implements no TLS
1.3.** `kTLSProtocol13` exists in the enum but `SSLSetProtocolVersionMin` rejects it
with `errSSLIllegalParam` (-9830) on both sides, so a macOS connection negotiates at
most TLS 1.2. `TlsConfig.set_min_version(TlsVersion.tls_1_3)` is silently capped
rather than failing, and `TlsStream.version` always reports `tls_1_2` on macOS.

Secure Transport is deprecated by Apple. If it is ever removed, the recorded
successor is **vendoring a static BoringSSL/OpenSSL for macOS** the way T1596 did for
Linux — that preserves the in-memory-buffer architecture instead of dismantling it.

### Zero-dependency linking

A TLS program must build on a macOS host with **no Xcode Command Line Tools**, so TLS
is not the one feature that requires an SDK. `ensureBundledSDK` therefore writes
hand-authored TBD v4 stubs for `Security.framework` and `CoreFoundation.framework`
alongside the existing `libSystem` one, listing exactly the symbols the backend
references. `TestBundledFrameworkTBDsCoverBackendSymbols` derives that symbol list
from the backend itself and fails if a stub falls behind — the failure would
otherwise only appear on machines without an SDK, which is where nobody is looking.
