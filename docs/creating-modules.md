# Creating New Modules for Promise

A step-by-step guide to proposing, implementing, and shipping new catalog modules.

---

## Table of Contents

1. [Overview](#1-overview)
2. [Phase 1: Write the Proposal](#2-phase-1-write-the-proposal)
3. [Phase 2: Set Up the Module](#3-phase-2-set-up-the-module)
4. [Phase 3: Implement the Module](#4-phase-3-implement-the-module)
5. [Phase 4: Validate](#5-phase-4-validate)
6. [Reference: Module Types](#6-reference-module-types)

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

Before writing code, write a design document in `docs/`. The proposal establishes the API
contract that implementation must satisfy.

### What to include

**1. Motivation** — Why does this module need to exist? What can't be done today without it?
Reference analogous facilities in other languages (Rust, Go, Python, C) to show this is
established, not speculative.

**2. Design principles** — How does the API align with Promise's philosophy? Call out:
- Idiomatic Promise patterns used (`use` binding, enums, value types, `!` functions, channels)
- The abstraction model (e.g., cell-buffer vs. streaming for a TUI module)
- Layering strategy (low-level primitives now, high-level widgets later)
- Cross-platform story (what platform differences are abstracted away)

**3. Quick start** — A minimal, runnable example (10-20 lines) showing the module in action.
This is the first thing an AI agent or new user will read — make it self-explanatory.

**4. Assumed dependencies** — Types from `std` or other modules that the API uses. Call out
any new `std` types that would need to be added (e.g., `Point`, `Size`, `Rect`).

**5. Full API surface** — Every public type, enum, and function with:
- Type signatures using Promise syntax
- `~this` / `&this` annotations (mutability contract)
- Failable markers (`!`, `?!`)
- Default parameter values
- Brief doc comment per method

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
- Performance considerations (buffering, diff-based rendering, etc.)
- Thread safety model
- Cleanup guarantees

**9. Future extensions** — Things explicitly out of scope for v1, but designed to layer on top.
This proves the API won't need breaking changes when these are added.

### Where to put the proposal

Place the proposal inside the module directory: `modules/<name>/proposal.md`. This keeps
the design doc co-located with the code it describes, even before any code exists. Create
the module directory and `promise.toml` first (see Phase 2), then add the proposal there.

For example: `modules/console/proposal.md`.

### Review checklist

Before moving to implementation, verify:

- [ ] Every public function uses full English words (with approved abbreviations from `docs/language-design.md` section 9.3a)
- [ ] Side-effect-free parameterless access uses getters (`get name Type`), not functions
- [ ] All `public` declarations have `\`doc` annotations in the API spec
- [ ] Value types are marked `\`value` where appropriate
- [ ] Error-raising functions are marked `!`
- [ ] Cleanup resources use `use` binding pattern (not manual close)
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
only depend on other catalog modules.

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

Convention: prefix private externs with `_<module>_` (e.g., `_io_file_open`, `_console_enter_raw_mode`).
The `\`extern` string is the LLVM symbol name that codegen will emit.

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

Key conventions:

- **`\`public` + `\`doc` on every exported declaration:**
  ```promise
  type Screen `public `doc("Main terminal handle — owns raw mode and alternate screen buffer.") {
    // ...
  }
  ```

- **Use `\`value` for types that should be stack-allocated and auto-copied:**
  ```promise
  type Style `public `doc("Text styling: foreground, background, attributes.") {
    Color? fg `value;
    Color? bg `value;
    Attribute attr `value;
  }
  ```

- **Use `\`factory` for constructors:**
  ```promise
  open() Self! `factory `public `doc("Opens the terminal in raw mode with alternate screen buffer.");
  ```

- **Use `~this` for mutating methods, `&this` for read-only:**
  ```promise
  write(~this, Point[int] pos, string text, Style style);
  get_cell(&this, Point[int] pos) Cell;
  ```

- **Wrap extern calls in Promise functions** that provide error handling:
  ```promise
  _make_console_error(int rc) ConsoleError {
    int c = 0 - rc;
    return ConsoleError(code: c, message: _console_strerror(c));
  }

  open() Self! `factory {
    int rc = _console_enter_raw_mode();
    if rc < 0 {
      raise _make_console_error(rc);
    }
    // ...
  }
  ```

### 4.3 Write tests incrementally

Add tests to `modules/<name>/<name>_test.pr` as you implement each piece.

**Prefer batch tests** (`` `test `` functions with `assert()`):

```promise
style_default() `test {
  Style s = Style.default();
  assert(s.fg is none, "default fg is none");
  assert(s.bg is none, "default bg is none");
}

style_builder() `test {
  Style s = Style.default().fg(Color.Red).bold();
  assert(s.fg is some, "fg set after builder");
}
```

**Use `` `test(exclude: "wasm32") `` for tests that need real OS interaction** (file I/O,
terminal control, etc.):

```promise
raw_mode_roundtrip() `test(exclude: "wasm32") {
  // This test actually enters/exits raw mode
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
screen_open_close() `test(exclude: "wasm32", allow_leaks: true) {
  use screen := Screen.open();
  screen.clear();
  screen.show();
}
```

### 4.4 Handle language limitations

If you hit a compiler bug, language limitation, or missing feature while implementing:

1. **Stop** — do not hack around it in module code
2. **File a bug** in the `tracker` MCP server (type: `bug`)
3. **Include**: what the bug is, a minimal reproducer, any workaround, and the priority
4. **Wait** for the compiler fix before continuing

This is a hard rule. Module code should never contain workarounds for compiler issues.

### 4.5 Rebuild after changes

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

If the module interacts with other modules or language features in interesting ways, add
integration tests in `tests/catalog/` or `tests/modules/`:

```promise
// tests/catalog/console_basic_test.pr
use console;

console_import() `test {
  // Verify the module loads and basic types are accessible
  Style s = console.Style.default();
  assert(s.fg is none);
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
- [ ] Stress tests show no flakiness (`bin/promise test -stress 100 modules/<name>/`)
- [ ] Coverage is adequate (`bin/promise test -coverage modules/<name>/`)
- [ ] `catalog.toml` has the module entry
- [ ] `promise.toml` exists in the module directory
- [ ] All `\`public` declarations have `\`doc` annotations
- [ ] `docs/standard-library.md` is updated
- [ ] No workarounds for compiler bugs (all filed in tracker)

---

## 6. Reference: Module Types

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
