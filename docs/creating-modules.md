# Creating New Modules for Promise

A step-by-step guide to proposing, implementing, and shipping new catalog modules.

---

## Table of Contents

1. [Overview](#1-overview)
2. [Phase 1: Write the Proposal](#2-phase-1-write-the-proposal)
3. [Phase 2: Set Up the Module](#3-phase-2-set-up-the-module)
4. [Phase 3: Implement the Module](#4-phase-3-implement-the-module)
5. [Phase 4: Validate](#5-phase-4-validate)
6. [Reference: Language Constraints for Module Design](#6-reference-language-constraints-for-module-design)
7. [Reference: Module Types](#7-reference-module-types)

---

## 1. Overview

Promise has four kinds of modules:

| Kind | Location | Imported as | Discovery |
|------|----------|-------------|-----------|
| **Embedded catalog module** | `modules/<name>/` | `use <name>;` | Embedded in compiler binary |
| **External catalog module** | Git repo (URL + commit) | `use <name>;` | Fetched once, then cached |
| **Local module** | Any project directory | `use <name> "./path";` | Resolved from project tree |
| **Remote module** | Git repo (URL + commit) | `use <name> "url";` | Fetched once, then cached |

The `std` module is just an embedded catalog module that happens to be auto-imported
(`use std as _`) into every file. Apart from the auto-import, it follows the same rules
as any other catalog module.

**Catalog modules** (both embedded and external) are listed in `catalog.toml` and imported
with bare `use <name>;`. Embedded ones have their source in `modules/<name>/` and are
compiled into the binary. External ones specify a `url` + `commit` in `catalog.toml` and
are fetched from git on first use (then cached locally). From user code, both look identical.

This guide covers creating **embedded catalog modules** — modules whose source lives in the
repo and ships with the compiler binary. The same principles apply to local modules, minus
the catalog registration and embedding steps.

The lifecycle of a new catalog module is:

```
Proposal → Setup → Implement → Validate → Ship
```

---

## 2. Phase 1: Write the Proposal

Before writing code, write a design document. The proposal establishes the API contract
that implementation must satisfy. Place it inside the module directory from the start
(`modules/<name>/proposal.md`) — see [Phase 2](#3-phase-2-set-up-the-module) for setup.

### What to include

**1. Motivation** — Why does this module need to exist? What can't be done today without it?
Reference analogous facilities in other languages (Rust, Go, Python, C) to show this is
established, not speculative.

**2. Design principles** — How does the API align with Promise's philosophy? Call out:
- Idiomatic Promise patterns used (`use` binding, enums, value types, `!` functions, channels)
- The abstraction model (e.g., cell-buffer vs. streaming for a TUI module)
- Layering strategy (low-level primitives now, high-level widgets later)
- Cross-platform story (what platform differences are abstracted away, what is excluded on WASM)

**3. Quick start** — A minimal, runnable example (10-20 lines) showing the module in action.
This is the first thing an AI agent or new user will read — make it self-explanatory.

**4. Assumed dependencies** — Types from `std` that the API uses. Call out any new `std`
types that would need to be added (e.g., `Point`, `Size`, `Rect`). Note: catalog modules
can only depend on `std` (auto-imported) — they cannot import other catalog modules.

**5. Full API surface** — Every public type, enum, and function with:
- Type signatures using Promise syntax
- `~this` / `&this` annotations (mutability contract)
- Failable markers (`!`, `?!`)
- Default parameter values
- `\`doc` annotation per declaration
- `\`target` conditions for platform-specific declarations

Group by type, use Promise code blocks. This is the spec — implementation will match it 1:1.

**6. Usage patterns** — 3-5 realistic examples showing common workflows:
- The simplest case (hello world equivalent)
- The concurrent/async case (channels, goroutines, select)
- Edge cases (error handling, cleanup, subprocess handoff)

**7. Comparison table** — Map each API primitive to its equivalent in 2-3 other languages.
This helps reviewers verify completeness and helps implementors find reference implementations.

**8. Implementation notes** — Platform-specific details:
- What PAL (Platform Abstraction Layer) functions are needed
- What system calls or APIs will be used on each platform (POSIX, Windows, WASM)
- What is excluded on WASM and why (no filesystem, no subprocesses, no real threading)
- Performance considerations (buffering, diff-based rendering, etc.)
- Thread safety model
- Cleanup guarantees (`close()` vs `drop()` — see [section 6.5](#65-resource-cleanup-close-vs-drop))

**9. Future extensions** — Things explicitly out of scope for v1, but designed to layer on top.
This proves the API won't need breaking changes when these are added.

### Where to put the proposal

Place the proposal inside the module directory: `modules/<name>/proposal.md`. This keeps
the design doc co-located with the code it describes, even before any code exists. Create
the module directory and `promise.toml` first (see Phase 2), then add the proposal there.

For example: `modules/console/proposal.md`.

### Review checklist

Before moving to implementation, verify:

- [ ] Every public identifier uses full English words (with approved abbreviations from `docs/language-design.md` section 9.3a)
- [ ] Naming follows conventions: `snake_case` for functions/methods/fields, `PascalCase` for types/enums/variants
- [ ] No function overloading — use default/optional parameters instead (see [section 6.1](#61-no-function-overloading))
- [ ] Side-effect-free parameterless access uses getters (`get name Type`), not functions
- [ ] All `\`public` declarations have `\`doc` annotations
- [ ] Value types have all fields marked `\`value`
- [ ] Error types inherit from `error` with an `int code` field (see [section 4.3](#43-define-error-types))
- [ ] Error-raising functions are marked `!`
- [ ] Cleanup resources use `use` binding with `close()` (not manual close)
- [ ] Platform-specific APIs use `\`target` conditions (see [section 4.4](#44-handle-cross-platform-differences))
- [ ] WASM-incompatible APIs are excluded with `\`target(!wasm)` (see [section 4.5](#45-wasm-considerations))
- [ ] The quick start example compiles (mentally) against the API spec

---

## 3. Phase 2: Set Up the Module

### 3.1 Create the directory

```bash
mkdir modules/<name>
```

This can happen during Phase 1 — create the directory early so the proposal lives here
from the start (`modules/<name>/proposal.md`).

### 3.2 Create `promise.toml`

Every module needs a `promise.toml` in its root directory:

```toml
[module]
name = "<name>"
```

That's the minimum. The `name` field must match the directory name and the name used in
`use <name>;` imports.

Embedded catalog modules **cannot** have an `epoch` field — they are part of the compiler
and always match its epoch. The `epoch` field is only used by local and remote modules
to detect version mismatches with the project.

Catalog modules also **cannot** have `[require]` entries (no remote dependencies). They can
only depend on `std` (which is auto-imported). They cannot import other catalog modules.

### 3.3 Create the source file

Create `modules/<name>/<name>.pr`:

```promise
// <name> module — one-line description.
// Requires: use <name>;
```

Start with just the file header. Implementation comes in Phase 3.

### 3.4 Create the test file

Create `modules/<name>/<name>_test.pr`:

```promise
// Tests for the <name> module.
```

Test files (`*_test.pr`) are compiled as part of the module and have access to all
declarations (public and private).

### 3.5 Register in the catalog

Add an entry to `catalog.toml` in the repo root:

```toml
[modules.<name>]
description = "Short description of what the module provides"
```

This registers the module so the compiler discovers it as an embedded catalog module.

### 3.6 Rebuild

```bash
./build
```

This runs `make resources` (copies modules into the embedded resource directory) and
rebuilds the compiler binary. The new module is now available to any Promise program.

### 3.7 Verify the empty module loads

```bash
bin/promise exec 'use <name>; main() { print_line("ok"); }'
```

If this prints `ok`, the module is correctly registered and loadable.

---

## 4. Phase 3: Implement the Module

### 4.1 Decide what needs native support

Most module code should be written in Promise. Only use `native` / `\`extern` functions
when you need:

- Direct system calls (file I/O, terminal control, networking)
- Memory layout manipulation (raw pointers, unsafe casts)
- Platform-specific behavior that can't be expressed in Promise

For each native function needed:

**a) Declare the extern in your `.pr` file:**

```promise
_console_enter_raw_mode() int `extern("promise_console_enter_raw_mode");
_console_exit_raw_mode() int `extern("promise_console_exit_raw_mode");
```

Convention: prefix private externs with `_<module>_` (e.g., `_io_file_open`,
`_console_enter_raw_mode`). The `\`extern` string is the LLVM symbol name that codegen
will emit.

**b) Implement the PAL function in codegen:**

PAL functions live in `compiler/internal/codegen/pal/`. Each platform has its own file:

| File | Platform |
|------|----------|
| `posix.go` | Linux + macOS |
| `windows.go` | Windows |
| `wasm.go` | WASM (WASI) |
| `pal.go` | Shared dispatch + registration |

Add the LLVM IR implementation for each platform. Follow existing patterns — look at
`promise_io_file_open` in `posix.go` for a complete example.

**c) Register the PAL function** in `pal.go` so codegen emits it.

### 4.2 Write the Promise API

Implement types and functions in `modules/<name>/<name>.pr`, following the proposal spec.

**Naming conventions:**

| Element | Convention | Examples |
|---------|-----------|----------|
| Types, enums | `PascalCase` | `File`, `IoError`, `MouseEvent` |
| Enum variants | `PascalCase` | `Key.Enter`, `Color.BrightRed` |
| Functions, methods | `snake_case` | `read_all`, `poll_event`, `set_cell` |
| Fields | `snake_case` | `exit_code`, `has_ctrl` |
| Getters | `snake_case` | `get size`, `get is_empty` |
| Private members | `_` prefix | `int _fd`, `_make_error()` |
| Extern wrappers | `_<module>_` prefix | `_io_file_open`, `_console_enter_raw_mode` |

Use full English words in all identifiers. The only allowed abbreviations are those in the
approved dictionary (`docs/language-design.md` section 9.3a): `abs`, `arg`, `attr`, `ch`,
`config`, `dest`, `dir`, `env`, `func`, `id`, `info`, `init`, `len`, `max`, `millis`,
`min`, `pos`, `prev`, `src`, `var`.

**Visibility conventions:**

- Top-level declarations are **private by default** — add `\`public` to export
- Members of a `\`public` type are **public by default**
- Use `_` prefix to mark members as private (e.g., `int _fd;`)
- Operators (`+`, `==`, `[]`, etc.) are always public
- Extern wrappers and internal helpers should be private (no `\`public`, `_` prefix)

**Key annotation patterns:**

```promise
// Public type with doc
type Screen `public `doc("Main terminal handle — owns raw mode and alternate screen buffer.") {
  int _fd;               // private field (underscore prefix)
  int _width;

  // Public factory with doc
  open!(ScreenOptions options = ScreenOptions()) Self `factory
      `doc("Opens the terminal in raw mode with alternate screen buffer.") {
    // ...
  }

  // Public getter (side-effect-free, no params — use getter, not function)
  get size Size[int] `doc("Terminal dimensions in character cells.");

  // Mutating method
  write(~this, Point[int] pos, string text, Style style)
      `doc("Write a styled string starting at pos. Clips at screen edge.");

  // Read-only method
  get_cell(&this, Point[int] pos) Cell
      `doc("Read back a cell from the buffer.");
}

// Value type — all fields `value, stack-allocated, auto-copied
type Cell `public `doc("One character cell in the screen buffer.") {
  char ch `value;
  Style style `value;
}
```

### 4.3 Define error types

Every module that can fail needs an error type. Follow the established pattern:

```promise
// Error type inherits from error, includes errno code
type ConsoleError is error `public `doc("A terminal I/O error.") {
  int code;
}

// Helper to wrap PAL return codes (PAL returns -errno on failure)
_make_console_error(int rc) ConsoleError {
  int c = 0 - rc;
  return ConsoleError(code: c, message: _console_strerror(c));
}

// Human-readable messages for common errno values
_console_strerror(int code) string {
  return match code {
    5 => "input/output error",
    9 => "bad file descriptor",
    22 => "invalid argument",
    25 => "inappropriate ioctl for device",  // ENOTTY
    _ => "console error (errno {code})",
  };
}
```

Usage in API methods:

```promise
open!() Self `factory {
  int rc = _console_enter_raw_mode();
  if rc < 0 {
    raise _make_console_error(rc);
  }
  // ...
}
```

### 4.4 Handle cross-platform differences

Promise uses the `\`target` annotation for compile-time platform filtering. Only the
matching variant is compiled — the other is invisible to the type checker and codegen.

**Supported conditions:**

| Identifier | Meaning |
|------------|---------|
| `linux` | Linux (any triple) |
| `macos` | macOS / Darwin |
| `windows` | Windows (MSVC ABI) |
| `wasm` | WebAssembly (wasm32-wasi) |
| `posix` | `linux \|\| macos` |
| `x86_64` | x86-64 architecture |
| `aarch64` | AArch64 / ARM64 |

Conditions combine with `!` (not), `||` (or), `&&` (and), and `()` grouping.

**Pattern: platform-specific function variants**

```promise
// Different implementations per platform — same signature, different target
_get_terminal_size() (int, int) `target(posix) {
  // ioctl(TIOCGWINSZ) on POSIX
  return (_console_ioctl_rows(), _console_ioctl_cols());
}

_get_terminal_size() (int, int) `target(windows) {
  // GetConsoleScreenBufferInfo on Windows
  return (_console_win_rows(), _console_win_cols());
}
```

**Pattern: exclude entire types/functions from a platform**

```promise
// This type does not exist on WASM at all
type Screen `public `target(!wasm) `doc("Main terminal handle.") {
  // ...
}

// This function is POSIX-only
suspend!(~this) `target(posix) `doc("Suspend raw mode for subprocess handoff.");
```

**Rules:**
- `\`target` applies to `type`, `enum`, and `func`/method declarations
- Individual fields cannot be filtered — filter the whole type or use separate types
- Without `\`target`, a declaration is included on all targets
- This is Promise's **only** form of platform-specific variation — no preprocessor, no `#ifdef`

### 4.5 WASM considerations

WASM (wasm32-wasi) has significant limitations that affect module design:

| Feature | WASM status |
|---------|-------------|
| Filesystem I/O | Not available |
| Subprocess execution | Not available |
| Real threading | Not available (cooperative scheduling only) |
| Signal handling | Not available |
| `sleep()` | No-op |
| Terminal / TTY | Not available |
| Environment variables | Limited (runtime-dependent) |

**Design guidance:**

- Exclude WASM-incompatible types/functions with `\`target(!wasm)`
- Pure computation (data structures, algorithms, formatting, parsing) works on all targets
- If the entire module is WASM-incompatible (e.g., `console`), document this in the proposal
- Tests for OS-dependent behavior use `\`test(exclude: "wasm32")`
- The WASM target is always tested in CI (`bin/verify.sh --wasm`) — if your module compiles
  on WASM (even with most APIs excluded), the remaining code must be correct

### 4.6 Write tests incrementally

Add tests to `modules/<name>/<name>_test.pr` as you implement each piece.

**Prefer batch tests** (`` `test `` functions with `assert()`):

```promise
error_strerror_known() `test {
  assert(_console_strerror(22) == "invalid argument", "errno 22");
  assert(_console_strerror(9999) == "console error (errno 9999)", "unknown errno");
}

error_make_error() `test {
  ConsoleError e = _make_console_error(-22);
  assert(e.code == 22, "code from -errno");
  assert(e.message == "invalid argument", "message");
}
```

**Use `` `test(exclude: "wasm32") `` for tests that need real OS interaction:**

```promise
screen_open_close() `test(exclude: "wasm32") {
  use screen := Screen.open();
  assert(screen.size.width > 0, "has width");
}
```

**Use `` `test(timeout: "5s") `` for tests with blocking operations:**

```promise
poll_event_timeout() `test(exclude: "wasm32", timeout: "5s") {
  use screen := Screen.open();
  Event? e = screen.poll_event_timeout(100);
  assert(e is none, "no event within 100ms");
}
```

**Use `` `test(allow_leaks: true) `` for tests with known leaks** (temporary — remove when
leak source is fixed):

```promise
screen_with_mouse() `test(exclude: "wasm32", allow_leaks: true) {
  use screen := Screen.open();
  screen.enable_mouse();
  screen.disable_mouse();
}
```

**Temp file naming convention:** Use `/tmp/pr_<mod>t_<suffix>` (e.g., `/tmp/pr_iot_cer`
for promise_io_test_create_exists_remove). Always clean up temp files at the end of tests.

### 4.7 Handle language limitations

If you hit a compiler bug, language limitation, or missing feature while implementing:

1. **Stop** — do not hack around it in module code
2. **File a bug** in the `tracker` MCP server (type: `bug`)
3. **Include**: what the bug is, a minimal reproducer, any workaround, and the priority
4. **Wait** for the compiler fix before continuing

This is a hard rule. Module code should never contain workarounds for compiler issues.

### 4.8 Multiple source files

Small modules typically use a single `.pr` file. Larger modules may split across multiple
files — all `.pr` files in the module directory (and subdirectories) are merged into a
**single compilation unit**. This means:

- All declarations across files share the same scope (no need to import between files)
- Name collisions between files are compile errors
- File order does not matter — the compiler resolves all names after merging
- Subdirectories are recursively discovered, but a subdirectory with its own `promise.toml`
  is treated as a separate module (not merged)

### 4.9 Rebuild after changes

After every change to module source:

```bash
./build
```

This re-embeds the updated module sources. Then run your tests:

```bash
bin/promise test modules/<name>/
```

---

## 5. Phase 4: Validate

### 5.1 Run module tests

```bash
bin/promise test modules/<name>/
```

All tests must pass. Check for leaks in the output.

### 5.2 Run the full test suite

```bash
bin/verify.sh --local --wasm
```

This runs:
- `go vet` on the compiler
- All Go unit tests
- All Promise tests (including WASM target)
- Code formatting check

**Do not commit if verify fails.**

### 5.3 Write integration tests

If the module interacts with language features in interesting ways, add integration tests
in `tests/catalog/` or `tests/modules/`:

```promise
// tests/catalog/console_basic_test.pr
use console;

console_import() `test {
  // Verify the module loads and basic types are accessible
  ConsoleError e = ConsoleError(code: 1, message: "test");
  assert(e.code == 1);
}
```

### 5.4 Run stress tests (for concurrency-sensitive modules)

```bash
bin/promise test -stress 100 modules/<name>/
```

This runs all module tests 100 times to detect flaky failures, race conditions, and
timing-dependent bugs.

### 5.5 Check test coverage

```bash
bin/promise test -coverage modules/<name>/
```

Review the coverage report. Ensure all public API methods have at least one test. File
tasks in the tracker for any coverage gaps.

### 5.6 Update documentation

Update `docs/standard-library.md` with the new module:
- Add a row to the catalog modules table
- Include file count, line count, test count, and status
- List the key types and functions

### 5.7 Final checklist

Before committing:

- [ ] All module tests pass (`bin/promise test modules/<name>/`)
- [ ] Full verify passes (`bin/verify.sh --local --wasm`)
- [ ] WASM target compiles (even if most APIs are excluded via `\`target`)
- [ ] Stress tests show no flakiness (`bin/promise test -stress 100 modules/<name>/`)
- [ ] Coverage is adequate (`bin/promise test -coverage modules/<name>/`)
- [ ] `catalog.toml` has the module entry
- [ ] `promise.toml` exists in the module directory
- [ ] All `\`public` declarations have `\`doc` annotations
- [ ] Error types follow convention (`is error`, `int code` field, strerror helper)
- [ ] Platform-specific APIs use `\`target` conditions
- [ ] `docs/standard-library.md` is updated
- [ ] No workarounds for compiler bugs (all filed in tracker)

---

## 6. Reference: Language Constraints for Module Design

These constraints from the language design directly affect how modules are structured.

### 6.1 No function overloading

Promise does **not** support function or method overloading. Each function name within a
scope must be unique. Use default parameters and optional parameters instead:

```promise
// WRONG — two functions with same name
open!() Self `factory;
open!(ScreenOptions options) Self `factory;

// RIGHT — single function with default parameter
open!(ScreenOptions options = ScreenOptions()) Self `factory;
```

For fundamentally different operations, use distinct names:

```promise
// WRONG — overloaded parse
parse!(string data) Config ;
parse!(u8[] bytes) Config ;

// RIGHT — distinct names
parse_string!(string data) Config ;
parse_bytes!(u8[] bytes) Config ;
```

### 6.2 No module-level variables

Promise does not support module-level mutable variables (global mutable state). All mutable
state lives in function-scoped locals, type instances, or is threaded through parameters.

This means modules cannot have global singletons, caches, or registries. If persistent
state is needed, model it as a type instance the caller creates and passes around (e.g.,
`Screen` in the console module, `File` in the io module).

### 6.3 No module initializers

No module-level initializer blocks, `init()` functions, or static constructors. No code
runs automatically when a module is imported — code only executes when something explicitly
calls it. This makes startup behavior fully predictable from `main()`.

### 6.4 No transitive re-exports

A module's exported scope contains only its own `\`public` declarations. Symbols from
`std` (auto-imported via `use std as _`) are **not** re-exported. If a consumer needs
a type from `std`, they get it from `std` directly (which is always available).

### 6.5 Resource cleanup: `close()` vs `drop()`

Promise has two cleanup mechanisms. Modules must choose the right one:

| Mechanism | Method | Trigger | Can fail | Use case |
|-----------|--------|---------|----------|----------|
| `use` binding | `close(~this)!` | Scope exit | Yes (propagates in `!` functions) | Explicit resource scoping (files, connections, screens) |
| `drop` | `drop(~this)` | Owner out of scope (not moved) | No (must not fail) | Automatic cleanup (memory, handles) |

**Rules:**
- If a variable is bound with `use`, the compiler calls `close()` at scope exit —
  `drop()` is **not** called (use takes precedence)
- For variables not bound with `use`, normal `drop()` semantics apply
- A type can have both `close()` and `drop()` — `close()` for `use`-bound variables,
  `drop()` for non-`use` variables
- Multiple `use` bindings in the same scope are closed in reverse declaration order (LIFO)
- `drop()` must not fail — its signature is `drop(~this)` with no `!`

**Example:**

```promise
type Screen `public {
  int _fd;

  // Called by `use` binding at scope exit — can propagate errors
  close!(~this) `doc("Restores terminal state. Called automatically by use binding.") {
    int rc = _console_exit_raw_mode(this._fd);
    if rc < 0 { raise _make_console_error(rc); }
  }

  // Called when a non-use Screen goes out of scope — best-effort cleanup
  drop(~this) {
    _console_exit_raw_mode(this._fd);
  }
}
```

### 6.6 Catalog module dependency rules

Embedded catalog modules:
- **Can** depend on `std` (auto-imported, always available)
- **Cannot** import other catalog modules (`use io;` inside a catalog module is an error)
- **Cannot** have `[require]` entries in `promise.toml` (no remote dependencies)
- **Cannot** have an `epoch` field in `promise.toml`

This ensures catalog modules are self-contained and have no circular dependency risk.

---

## 7. Reference: Module Types

### Embedded catalog module

```
modules/<name>/
  promise.toml          # [module] name = "<name>"
  proposal.md           # API proposal (design doc)
  <name>.pr             # implementation
  <name>_test.pr        # tests (compiled into module, access private symbols)
```

Source lives in the repo under `modules/<name>/`. Registered in `catalog.toml` with just
a description (no `url`/`commit`). Embedded in the compiler binary during `./build`.
Imported as `use <name>;`.

```toml
# catalog.toml
[modules.io]
description = "File and directory I/O"
```

### External catalog module

Source lives in an external git repo. Registered in `catalog.toml` with `url` + `commit`.
Fetched once on first use and cached locally. Imported identically to embedded modules
via `use <name>;` — user code cannot tell the difference.

```toml
# catalog.toml
[modules.some_lib]
url = "https://github.com/promise-lang/some-lib"
commit = "a1b2c3d"
description = "Some library"
```

Both `url` and `commit` must be present. The commit pin ensures reproducible builds.

### Local module (project-relative)

```
libs/<name>/
  promise.toml          # [module] name = "<name>"
  <name>.pr             # implementation
  <name>_test.pr        # tests
```

Imported as `use <name> "./libs/<name>";`. Not embedded — lives in the project tree.
Can depend on catalog modules, other local modules, and remote modules.

### Remote module (project-level dependency)

Fetched from a git URL. Requires a commit pin in the project's `promise.toml`:

```toml
[require]
"github.com/user/module-name" = "abc123def"
```

Imported as `use <name> "github.com/user/module-name";`. Cached in `~/.promise/cache/remote/`.

Unlike external catalog modules (which are shared across all Promise projects via
`catalog.toml`), remote modules are per-project dependencies pinned in the project's own
`promise.toml`.

### Directory layout conventions

- One `.pr` file per module is typical for small modules (`path`, `strings`, `math`)
- Larger modules may have multiple `.pr` files — all are merged into a single compilation unit
- Subdirectories are recursively discovered (but nested directories with `promise.toml` are treated as separate modules)
- `*_test.pr` files are included when testing the module but excluded when the module is imported by other code
