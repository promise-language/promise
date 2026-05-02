# Platform Modules Layout

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

### The architecture: `std` as a regular catalog module (Done)

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
- `CheckForStdModule` in `sema/check.go` compiles std itself with `compilingStd=true`
  (prevents std from trying to import itself)
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
| `println`, `Writer`, `Closer`, `write_line` | **`std/`** | Every program produces output |
| `Vector`, `Map`, `Set`, `Iterator` | **`std/`** | Data structures are universal |
| `Channel`, `Task`, `error`, `assert` | **`std/`** | Concurrency and errors are language-level |
| `Duration`, `Instant` | **`std/`** | Timing is universal; pure value type; zero API noise |
| `File`, `BufReader` | **`modules/io`** | Not needed by compute-only programs; `use io;` is a meaningful signal |
| `read_line`, `read_stdin` | **`modules/io`** | Same PAL path as File; co-locates all stdin/file I/O |
| `get_env`, `args`, `exec` | **`modules/os`** | Process model — irrelevant to library code and pure computation |
| `path.join`, `path.file_name` | **`modules/path`** | Not every program works with paths |
| Higher-level time ops | **`modules/time`** | Calendar/TZ/formatting builds on std; not universally needed |

**The `use` statement as a semantic signal.** Beyond scope noise, `use io;` at the top of a file
tells any reader — human or agent — that this program interacts with the filesystem. This
information has value. Auto-importing everything strips that signal. The module boundary is
documentation.

---

## 2. `\`target(cond)` — Compile-Time Platform Filtering (Done)

Implemented in Stage 8q. See `docs/stages.md` for implementation details.

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

### Implementation (Done — Stage 8q)

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
| `File` type | `!wasm` only (no filesystem on WASM) | (not yet implemented) |
| `read_line/read_stdin` | `!wasm` only | (not yet implemented) |
| `exec(...)` | `!wasm` only (no subprocess on WASM) | (not yet implemented) |
| `args()` | different impl on WASM | (not yet implemented) |
| `get_env(string)` | `!wasm` (WASM env access varies) | (not yet implemented) |

That's ~18 `\`target` annotation uses across std+modules. This replaces an equal number of
Go codegen special cases with Promise source that is visible, readable, and owned by the module
author rather than the compiler internals.

### The boundary: `\`target` vs PAL

**PAL** = runtime OS syscall differences. `pal_file_open` on POSIX calls `open(2)`;
on Windows calls `CreateFile`. Both exist at runtime, just with different implementations.
PAL handles this in Go codegen — the generated IR is different but the function exists on both.

**`\`target`** = the function should not exist at all on some targets. `exec()` is not a
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

## 3. Platform Constants: `Platform` Type (Done)

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

**`println` and `\r\n`**: `println` always emits `\n`. Modern Windows terminals (Windows Terminal,
VS Code, PowerShell) handle `\n` correctly. `println` is for human-readable terminal output.
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

**Status**: Implemented. Pure string operations, platform-aware via `Platform.is_path_separator`.

### Current API (Done)

```promise
join(string base, string child, ...string rest) string  // join two or more path components
file_name(string path) string                           // last path component
parent(string path) string                              // all but last component
extension(string path) string                           // ".txt" or ""
stem(string path) string                                // file_name without extension
is_absolute(string path) bool
is_relative(string path) bool
```

Platform awareness is handled via `Platform.is_path_separator(c)` and `Platform.path_separator`
from `std/platform.pr`. All `` `target `` annotations are confined to std — the path module
contains no target-filtered code.

### Additions needed

**`normalize(string path) string`** — collapses `.`, `..`, and redundant separators. Pure Promise,
no syscalls:

```promise
normalize("/home/user/../user/./file.txt") // → "/home/user/file.txt"
normalize("./a/b/../c")                    // → "a/c"
```

**`split_all(string path) string[]`** — splits into all components:

```promise
split_all("/home/user/file.txt")  // → ["/", "home", "user", "file.txt"]
split_all("relative/path")        // → ["relative", "path"]
```

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
- **Module-level getters**: NOT in the grammar. `declaration` only has `typeDecl | enumDecl | funcDecl`.
  Adding `getterDecl` to `declaration` requires a grammar change (ANTLR regen), sema handling
  (declare getter at module scope), and call-site resolution (bare `args` resolves to getter call,
  not variable). Moderate effort — track as a language enhancement.

**For now**: failable operations (`get_cwd() string!`) stay as functions — `()` correctly signals
that something is happening. Pure-value properties (`args`) are candidates for getter syntax once
module-level getters land.

---

## 7. `modules/io` — File I/O and Standard Input

**Status**: Placeholder file (`io.pr` with comment only). Implement from scratch.

### Error type

```promise
type IoError is error `public `doc("An operating system I/O error.") {
    int code;  // errno on POSIX, GetLastError on Windows

    `doc("Human-readable description of the error code.")
    message() string `native;  // manual table for top 20 errno values; fallback: "error {code}"
}
```

### `File` — handle methods and `\`global` one-shot operations

Applying §6 principles: one-shot path operations (`read_file`, `write_file`, etc.) move inside
`File` as `\`global` methods. The module-level namespace then has only `File`, `BufReader`,
`IoError`, `read_line`, `read_stdin`.

```promise
type File `public `doc("A file handle. Satisfies Reader, Writer, and Closer.") {
    int _fd;

    // ── Construction (factory — returns Self) ──────────────────────────────
    `doc("Opens a file for reading.")
    open(string path) Self! `factory `public;

    `doc("Creates or truncates a file for writing.")
    create(string path) Self! `factory `public;

    `doc("Opens with an explicit mode string: `r`, `w`, `a`, `rw`.")
    open_mode(string path, string mode) Self! `factory `public;

    // ── Handle methods (instance — require an open file) ───────────────────
    read(~this, u8[] ~buf) int! `public;          // satisfies Reader
    write(~this, u8[] &buf) int! `public;         // satisfies Writer
    close(~this)! `public;                        // satisfies Closer

    `doc("Reads all remaining content into a string.")
    read_all(~this) string! `public;

    `doc("Writes a string to the file.")
    write_string(~this, string s)! `public;

    `doc("Returns the current byte position.")
    tell(~this) int! `public;

    `doc("Seeks to a byte offset from the start.")
    seek(~this, int offset)! `public;

    // ── One-shot operations (global — no open handle needed) ───────────────
    `doc("Reads the entire contents of a file as a string.")
    read(string path) string! `global `public;

    `doc("Writes content to a file, creating or truncating it.")
    write(string path, string content)! `global `public;

    `doc("Appends content to a file, creating it if needed.")
    append(string path, string content)! `global `public;

    `doc("Returns true if the path exists (file, directory, or symlink).")
    exists(string path) bool `global `public;

    `doc("Returns the size of the file in bytes.")
    size(string path) int! `global `public;

    `doc("Removes a file.")
    remove(string path)! `global `public;

    `doc("Creates a directory. Parent must exist.")
    mkdir(string path)! `global `public;

    `doc("Creates a directory and all missing parent directories.")
    mkdir_all(string path)! `global `public;

    `doc("Returns the names (not full paths) of entries in a directory.")
    list_dir(string path) string[]! `global `public;
}
```

Usage contrast:

```promise
// Before (free functions — 9 names in module namespace):
content := read_file("data.txt")!;
write_file("out.txt", result)!;
if exists("log/") { list_dir("log/")!; }

// After (type methods — 1 name in module namespace):
content := File.read("data.txt")!;
File.write("out.txt", result)!;
if File.exists("log/") { File.list_dir("log/")!; }
```

Note: `\`global` and `\`factory` are mutually exclusive (factory implies variant placement).
The instance `read(~this, u8[] ~buf)` and global `read(string path)` have different signatures
(receiver vs no receiver, different params) — sema distinguishes them correctly.

### `BufReader`

```promise
type BufReader `public `doc("Wraps a Reader with an internal buffer. Efficient for line-by-line reading.") {
    `doc("Creates a BufReader. Default buffer capacity: 4096 bytes.")
    new(~this, Reader ~r, int capacity = 4096) `public;

    read(~this, u8[] ~buf) int! `public;       // satisfies Reader

    `doc("Reads one line. Returns absent at EOF, raises IoError on error.")
    read_line(~this) string?! `public;

    `doc("Reads one byte. Returns absent at EOF.")
    read_byte(~this) u8?! `public;
}
```

### `read_line()` and `read_stdin()`

These stay as free functions — they have no type to belong to (stdin has no `File` handle in
normal use) and they are the primary interaction pattern for CLI programs.

```promise
`doc("Reads one line from stdin. Returns absent at EOF, raises IoError on read error.")
read_line() string?! `public;

`doc("Reads all of stdin into a string.")
read_stdin() string! `public;
```

**`string?!`** — the function can return `present("line")`, `absent` (EOF), or raise `IoError`.
The `while` loop unwraps the optional automatically:

```promise
use io;

while line := read_line()! {
    println(line);
}
```

### PAL additions

```
pal_file_open(i8* path, i32 flags, i32 mode) i32      // fd or -1
pal_file_read(i32 fd, i8* buf, i64 len) i64           // bytes read, 0=EOF, -1=error
pal_file_close(i32 fd) i32                            // 0 or -1
pal_file_stat_size(i8* path) i64                      // -1 = does not exist
pal_file_remove(i8* path) i32                         // 0 or -1
pal_file_mkdir(i8* path) i32                          // 0 or -1
pal_file_readdir(i8* path, i8* buf, i64 len) i32      // null-separated names; returns count
pal_file_seek(i32 fd, i64 offset, i32 whence) i64     // new position or -1
```

`pal_write` already exists. File writes use it (takes arbitrary fd). No `pal_file_write` needed.

---

## 8. `modules/os` — Operating System Interface

**Status**: Placeholder file (`os.pr` with comment only). Implement from scratch.

Applying §6 principles: `args` is a getter candidate (read-once at startup, effectively readonly).
`exit` and `exec` stay as functions (they perform actions). `get_env` and `get_cwd` stay as
functions — the `()` signals that something is read from the OS.

```promise
type OsError is error `public `doc("An operating system error not related to file I/O.") {
    int code;
    message() string `native;
}

`doc("Returns the value of environment variable `name`. Returns absent if not set.")
get_env(string name) string? `public;

`doc("Returns the current working directory.")
get_cwd() string! `public;

`doc("Returns the command-line arguments (index 0 = program name).")
args() string[] `public;     // getter candidate once module-level getters land

`doc("Terminates the process immediately with the given exit code.")
exit(int code) `public;

type ProcessResult `public `doc("The result of a subprocess execution.") {
    int exit_code;
    string stdout;
    string stderr;
}

`doc("Runs a program, captures stdout and stderr, and blocks until it exits.")
exec(string program, string[] args) ProcessResult! `public;
```

**`args()` implementation**: captured in the main prologue. Codegen emits `@__promise_args_data`
(a `string[]`) from `argc`/`argv` before user code runs.

**PAL additions**:
```
pal_getenv(i8* name) i8*               // pointer to value or null
pal_getcwd(i8* buf, i64 len) i8*       // fills buf, returns pointer or null on error
pal_exec_wait(i8** argv) {i32, i8*, i8*}  // captures stdout/stderr as strings
```

---

## 9. `modules/time` — Higher-Level Time Operations

**Status**: Placeholder. `modules/std/time.pr` already provides `Duration`, `Instant`, and `sleep`. The
`modules/time` placeholder should become higher-level time utilities that build on top of those:

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
    parse(string s, string layout) Self! `factory `public;
}

// Timezone-agnostic conversions
unix_now() int `public;  // seconds since Unix epoch
```

The PAL function for wall clock time (`pal_wall_clock` / `CLOCK_REALTIME`) belongs here, not in
`modules/std/time.pr` (which uses `CLOCK_MONOTONIC` for `Instant.now()`).

`modules/time` depends on `modules/std/time.pr` (for `Duration`, `Instant`) but not vice versa.

---

## 10. Future Platform Modules

These are placeholders in the catalog for future design:

| Module | Content | Key dependency |
|---|---|---|
| `modules/net` | TCP/UDP sockets, HTTP client | async I/O PAL, event loop |
| `modules/http` | HTTP server, routing, middleware | `modules/net`, `modules/io` |
| `modules/process` | Child process streams, pipes | PAL fork/exec with pipe capture |
| `modules/fs` | Advanced filesystem: symlinks, watch, temp files | PAL extensions |
| `modules/crypto` | Hashing, HMAC, random bytes | LLVM intrinsics + PAL getrandom |
| `modules/json` | JSON parse/serialize | `modules/io`, `modules/std/error.pr` |

None of these belong in `std/`. They are progressively heavier and progressively less universal.

---

## 11. Complete Layout

```
modules/
  std/            (auto-imported via implicit `use std as _;`)
    io.pr         — println, write_line, Closer
    platform.pr   — Platform (path_separator, line_separator, is_path_separator)
    format.pr     — Writer, Format, Builder
    parse.pr      — Reader, Parse, Scanner, scan[T]
    time.pr       — Duration, Instant, sleep
    math.pr       — PI, sqrt, sin, min, max, ...
    string.pr     — string methods
    vector.pr     — Vector[T]
    map.pr        — Map[K,V]
    iter.pr       — Iterator[T], combinators
    ... (28 files total)

  path/           — join, file_name, parent, extension, stem, is_absolute  (DONE)
  math/           — lerp, map_range, deg_to_rad, sign_f64            (DONE)
  strings/        — join, spaces, reverse, ...                        (DONE)
  io/             — File, BufReader, IoError, read_line, read_stdin   (PLACEHOLDER)
  os/             — args, get_env, get_cwd, exit, exec, OsError       (PLACEHOLDER)
  time/           — DateTime, unix_now, format/parse calendar ops     (PLACEHOLDER)
  http/           — HTTP server/client                                 (PLACEHOLDER)
```

**The invariant**: the `std` module is auto-imported everywhere (convenience), but compiled and
cached exactly like any other module. `modules/` are explicit imports. The distinction is about
API surface and the semantic signal of `use`, not about compilation mechanics.

---

## 12. Open Design Questions

These need resolution before implementation:

**Q1: `IoError.message()` source**
Use `strerror` (libc, locale-aware, one call) or a manual switch over the top-20 errno values?
**Lean**: manual table — keeps binaries fully self-contained and avoids locale dependency. Fallback
string `"errno {code}"` for unrecognized values.

**Q2: `IoError` vs `OsError` — is this split worth it?**
Both carry an `int code` and a `message()`. One shared `SysError is error` type (in a new thin
`std/sys.pr`) avoids the duplication. But it bleeds OS error types into `std/`.
**Lean**: keep them separate in their respective modules — the type system makes them
distinguishable; programs that handle errors from both can use the base `error` type for shared
handling.

**Q3: `args()` includes program name or not?**
Go: `os.Args[0]` is the program name. Rust: `std::env::args().next()` is the program name.
**Lean**: include it at index 0 (consistent with POSIX convention). Add `program_name() string`
as a convenience that returns `args()[0]`.

**Q4: `exec` API — blocking only for now?**
The current `ProcessResult` design is synchronous (blocks until the subprocess exits, captures all
output in memory). Streaming subprocess I/O (piped stdin/stdout) is a separate concern for
`modules/process`. **Lean**: `modules/os` provides the blocking convenience API only; streaming
APIs come later in `modules/process`.

**Q5: `read_line()` — strip trailing `\r\n` or just `\n`?**
**Lean**: strip both `\n` and `\r\n` — returns the line content without any line terminator.
This is the universal expectation and handles files created on Windows read on POSIX and vice versa.

---

## 13. Corrections to `standard-library.md`

The Phase 4 sections should be updated:

| Section | Change |
|---|---|
| 4b `std/file.pr` | Rename to `modules/io` — same API, different location |
| 4c `std/path.pr` | Remove — `modules/path` is the right home; no std/path.pr needed |
| 4d `std/os.pr` | Rename to `modules/os` — same API, different location |
| 4e `std/stdin.pr` | Remove as separate section — `read_line`/`read_stdin` live in `modules/io` |

Add a new Phase 4 header: "Platform Modules — see `docs/platform-modules.md`."

---

## 14. Implementation Order

### ~~Phase A — std-as-module refactor~~ (Done)

1. ~~Add `modules/std/promise.toml` (`[module] name = "std"`)~~ — done
2. ~~In `main.go`: auto-inject `UseDecl{CatalogName: "std", Alias: "_"}` into every file~~ — done
3. ~~In `sema/decl.go`: std resolved like any catalog module via `loadCatalog("std")`~~ — done
4. ~~Remove `IsStd` flag from AST nodes and `isDeclStd` from sema~~ — done
5. ~~Update test infrastructure: compile std as a module~~ — done
6. ~~DCE: superseded by LTO pipeline (`opt → .bc → linker --lto-O1`)~~ — done

Also done since this doc was written:
- ~~`\`target(cond)` compile-time platform filtering~~ — done (Stage 8q, `sema/target.go`)
- ~~Std moved from `std/` to `modules/std/`~~ — done (all modules under `modules/`)
- ~~Build cache with FNV-128a hashing, compiler stamp, persistent module cache~~ — done
- ~~`promise format` code formatter~~ — done

### ~~Phase B — platform constants~~ (Done)

First real use of `` `target `` in production code. Platform constants consolidated into
`Platform` namespace type in `modules/std/platform.pr` with `` `global `` getters and methods.

7. ~~**`Platform.line_separator`** global getter in `modules/std/platform.pr`~~ — done
8. ~~**Update `write_line`** to use `Platform.line_separator`~~ — done
9. ~~**`Platform.path_separator`** global getter in `modules/std/platform.pr`~~ — done
10. ~~**`Platform.is_path_separator(char)`** global method in `modules/std/platform.pr`~~ — done
11. ~~**Windows-aware `join`/`is_absolute`/`file_name`/`parent`**~~ — done (use `Platform.is_path_separator`)
12. ~~**Rename `basename` → `file_name`, `dirname` → `parent`**~~ — done (Rust-style naming)
13. ~~**Enable `` `global `` getters in sema**~~ — done (removed restriction in `decl.go`)
14. ~~**Global getter codegen**~~ — done (handle no-receiver dispatch in `expr.go`)
15. ~~**`HostTargetInfo()` for Go test helpers**~~ — done (`target.go`, all test helpers updated)

### Phase C — path module completion

16. **`normalize` and `split_all`** in `modules/path` — pure Promise, no new PAL
    (`join_all` superseded by variadic `join(base, child, ...rest)`)

### Phase D — file I/O (biggest phase — needs PAL work)

12. **PAL file functions** — add to `codegen/io.go`: open, read, close, stat, remove, mkdir, readdir, seek
13. **`modules/io`** — `IoError`, `File`, `BufReader`, `read_line`, `read_stdin`

### Phase E — OS and process

14. **Args capture in `main` prologue** — emit `@__promise_args_data` global from `argc`/`argv`
15. **`modules/os`** — `get_env`, `get_cwd`, `args`, `exit`, then `exec`

### Phase F — calendar time

16. **`modules/time`** — `DateTime`, wall clock PAL, format/parse utilities
