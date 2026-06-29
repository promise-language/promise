# Promise Programming Language — Design Proposal

## 1. Overview

**Promise** is a systems-aware, statically-typed programming language with Dart-inspired syntax, Rust-inspired ownership semantics, and a rich type system featuring inheritance, generics, and algebraic error handling. The toolchain (compiler + package manager) is a single Go binary called `promise`, uses ANTLR4 for parsing, and targets LLVM IR for code generation.

**No macros.** There are no preprocessor directives or procedural macros — code generation is not programmable. Platform differences are handled by `` `target(cond) `` annotations that select which declarations to compile for a given target (see [Platform Filtering](#platform-filtering)). This is a deliberate design choice: Promise is intended for AI agents, where hidden code generation and flag-dependent behavior create costly inference overhead.

---

## 2. Toolchain Architecture

```
Source (.pr)
    │
    ▼
┌──────────────┐
│  ANTLR4 Lexer│   ← Generated from Promise.g4 grammar
│  & Parser    │
└──────┬───────┘
       │  Parse Tree
       ▼
┌──────────────┐
│   AST Builder│   ← Go: converts ANTLR parse tree → Promise AST
└──────┬───────┘
       │  AST
       ▼
┌──────────────┐
│  Semantic    │   ← Type checking, ownership analysis,
│  Analysis    │     generic monomorphization, meta resolution
└──────┬───────┘
       │  Typed AST
       ▼
┌──────────────┐
│  LLVM IR Gen │   ← Go bindings to LLVM (via llir/llvm or CGo)
└──────┬───────┘
       │  LLVM IR
       ▼
┌──────────────┐
│  LLVM Backend│   ← Optimization passes, native code emission
└──────────────┘
       │
       ▼
   Binary / Library
```

The `promise` binary serves as both compiler and package manager. Commands are grouped by purpose. Dependency-management commands live under the `promise package` namespace; the bare toolchain verbs (`install`, `update`, `use`, `remove`) operate on the Promise toolchain itself, not on project dependencies.

```
# Compile & run
promise build file.pr             # Compile to executable
promise run file.pr               # Build and run
promise exec '<code>'             # Execute inline code (failable main, ? works)
echo '<code>' | promise           # Execute from stdin
promise test file.pr              # Run `test functions
promise test dir/...              # Recursive directory scan
promise test -timeout 30s ...     # Per-test timeout (default: 60s)
promise test -parallel 4 ...      # Run up to N tests in parallel (default: NumCPU)
promise test -stress [N|dur] ...  # Stress test (count, duration, or until Ctrl+C)
promise check file.pr             # Type-check only

# Documentation & discovery
promise                           # Concise command index (grouped)
promise help                      # Language overview + quick start
promise guide                     # Full language reference
promise examples                  # Browse and run example programs
promise doc <module>              # Document a catalog module (e.g. promise doc io)
promise doc file.pr               # Generate documentation from `doc annotations
promise doc -signatures file.pr   # Compact signature-only output
promise doc                       # List all available modules
promise targets                   # List supported compile targets

# Project & dependencies
promise init [--module]           # New app project (default), or a library module with --module
promise package add <url> [ref]   # Add an external (git) dependency to promise.toml
promise package remove <url>      # Remove an external dependency from promise.toml
promise package update [url]      # Update dependency pins to latest commits
promise package search <keyword>  # Search the catalog for available modules
promise package pin <url> [ref]   # Resolve + lock a remote dependency to a commit

# Toolchain & cache
promise install [epoch]           # Install Promise + pre-stage its toolchain (this binary, or a named epoch)
promise use <epoch>               # Activate an installed epoch
promise epochs                    # List installed epochs
promise remove <epoch>            # Remove an installed epoch
promise update                    # Update the Promise toolchain (follow the release channel)
promise update channel [stable|next]  # Show or set the release channel (default: stable)
promise update check              # Report whether an update is available (no changes)
promise doctor                    # Check the local environment (--repair also reclaims orphaned cache)
promise clean                     # Remove the build cache

# Tooling
promise format                    # Format source code
promise bind <format> <files>     # Generate bindings from WIT or WebIDL
promise version                   # Print compiler version
```

`promise.toml` is the lockfile — remote dependencies are pinned by commit hash directly in it, so there is no separate lockfile command.

**First-party catalog modules are built into the compiler and require no `add` step** — they are available directly with `use <name>;` (adding one to `[require]` is a compile error), because the epoch itself pins their version. `promise package add`/`remove`/`pin` operate on dependencies recorded in `promise.toml`: **community** modules (name-addressable via the `promise-community/catalog` index) and **ad-hoc** git URLs. `promise package add <name>` resolves the epoch-appropriate, test-verified revision and pins its commit; a bare `use foo;` whose name is neither first-party nor in `[require]` is a compile error that points you to `promise package add foo`. `promise package search` is discovery across the first-party and community catalogs. See `docs/module-system.md` §9.8–§9.10 for the full cross-epoch versioning and compatibility model.

`promise install` pre-stages the host LLVM toolchain into the cache as part of setup, so the first build works offline — there is no separate "fetch"/"warm" command. Re-running `promise install` (or `promise install <epoch>` for an already-installed epoch) re-asserts that the toolchain is staged; `--no-fetch-toolchain` skips the pre-stage for thin installs that fetch lazily on first compile.

Cache reclamation is automatic — there is no separate `gc` command. `promise remove <epoch>` frees that epoch's exclusive blobs (blobs shared with a still-installed epoch are kept), and `promise doctor --repair` sweeps orphaned blobs and stale toolchain-view directories left behind by interrupted installs or compiler-version churn.

The `ast` and `emit-ir` commands are also available for compiler debugging.

---

## 3. File Extensions & Project Layout

```
myproject/
├── promise.toml             # Module definition file
├── main.pr                  # Entry point
├── helpers.pr               # Other source files at top level
├── models/
│   ├── promise.toml         # Separate module (excluded from myproject)
│   ├── user.pr
│   ├── user_test.pr         # Test file alongside source
│   └── account.pr
└── utils/
    ├── promise.toml         # Separate module (excluded from myproject)
    └── strings.pr
```

Source files (`.pr`) and directories live directly inside the module root — there is no required `src/` directory. This keeps the layout flat and avoids an extra level of nesting that adds no information. The module root is wherever `promise.toml` is.

### 3.1 Module Boundaries

There is no concept of "sub-modules." Every `promise.toml` file defines a standalone module. When the compiler scans a module's directory tree, any subdirectory that contains its own `promise.toml` is **excluded** — it is a separate module, not part of the parent. Directories without a `promise.toml` are just organizational folders whose `.pr` files belong to the enclosing module.

In the layout above, `myproject/`, `models/`, and `utils/` are three independent modules that happen to be nested on disk. The `myproject` module contains `main.pr` and `helpers.pr`. It does **not** contain anything inside `models/` or `utils/` — those are their own modules with their own identities, dependencies, and compilation scopes.

This means a `promise.toml` file serves exactly one purpose: **it marks the root of a module.** The compiler needs this marker because:

1. **No guessing.** Without it, the compiler cannot distinguish "this directory is a separate module" from "this directory just organizes files." An explicit marker removes ambiguity — you can freely create directories for readability without accidentally splitting your module.
2. **Independent compilation.** Each module is a separate compilation unit with its own dependency graph. The `promise.toml` file tells the compiler "start a new compilation scope here."
3. **Visibility boundaries.** Visibility rules apply at module boundaries. Without a `promise.toml`, a directory has no boundary — its files are part of the parent module and share its namespace.
4. **Tooling clarity.** Tools, IDEs, and AI agents identify module structure by scanning for `promise.toml` files. No heuristics, no configuration — the file system is the source of truth.

### 3.2 Testing Convention

Tests live alongside the code they test. Any function annotated with `` `test `` is a test function. There are two approaches:

1. **Inline tests** — write `` `test `` functions directly in the production file:

```promise
// user.pr
type User {
  string name;
  int age;
}

testUserCreation() `test {
  User u = User(name: "Alice", age: 30);
  assert(u.name == "Alice", "name should be Alice");
}
```

2. **Separate test files** — create a `<name>_test.pr` file next to the source file (e.g., `user_test.pr` alongside `user.pr`). Test files follow the same convention — all `` `test ``-annotated functions are collected by `promise test`.

The `promise test` command discovers and runs all `` `test ``-annotated functions across the project. Any declaration annotated with `` `test `` — functions, types, or anything else — is excluded from production builds. Entire `_test.pr` files are also excluded from production builds.

**E2E tests** use the `` `test(expected: "output") `` annotation on `main()` to verify program output:

```promise
main() `test(expected: "42") {
  print_line(42);
}
```

**Per-test timeout** — individual tests can declare their own timeout using the `timeout` annotation parameter. The value is a Go-style duration string (`500ms`, `2s`, `1m`). Tests without a timeout annotation use the CLI `-timeout` default (60s). The CLI also provides `-timeout-scale` (multiplier), `-timeout-min`, and `-timeout-max` (clamps). Final timeout: `clamp((annotation ?: default) × scale, min, max)`.

```promise
test_channel_send() `test(timeout: "5s") {
  // times out after 5s instead of the default 60s
}

main() `test(expected: "hello", timeout: "2s") {
  print_line("hello");
}
```

**Directory scanning** runs all `.pr` files in a directory, with Go-style `...` for recursion:

```sh
promise test tests/               # non-recursive
promise test tests/...            # recursive (all subdirectories)
promise test -timeout 30s tests/  # custom per-test default timeout
promise test -timeout-scale 2.0 tests/...   # double all timeouts (slow CI)
promise test -timeout-max 5s tests/...      # clamp all timeouts to 5s
```

**Stress testing** detects flaky tests via repeated execution:

```sh
promise test -stress tests/concurrency/...       # run until Ctrl+C
promise test -stress 100 tests/concurrency/...   # 100 iterations
promise test -stress 30s tests/concurrency/...   # 30-second time limit
```

Stress mode compiles all target files once, then repeatedly runs the binaries. It tracks per-test pass rates and timing variance (coefficient of variation). Tests that always pass are gradually suppressed to focus on unreliable ones. The final report categorizes tests as flaky (any failures), high-variance (timing CoV > 0.5), or stable.

---

## 4. Module System

### 4.1 Module File (`promise.toml`)

Every module has a `promise.toml` at its root (TOML format). A minimal module file:

```toml
[module]
name = "myapp"
epoch = "2026.0"
```

The `epoch` field declares which catalog version this module targets — it determines which catalog modules and standard library APIs are available. It also acts as a compatibility signal: when a remote module's epoch differs from the consuming project's, the compiler warns that catalog APIs may differ.

**External dependencies** are declared in `[require]` and optional `[replace]` sections. Catalog modules (built into the compiler) are **not** listed here — adding a catalog module to `[require]` is a compile error:

```toml
[module]
name = "myapp"
epoch = "2026.0"

[require]
"github.com/acme/parser" = "a1b2c3d4e5f6"   # URL = commit hash (or tag)

[replace]
"github.com/acme/parser" = "../parser"        # local override for development
```

The `[require]` key is the git repository URL; the value is a commit hash or tag that pins an exact revision. Local overrides in `[replace]` map the same URL key to a local directory path.

### 4.2 `use` Declarations

At the top of any `.pr` file, `use` imports a module. There are two forms:

**Catalog import** — reference a built-in catalog module by name:

```promise
use json;
use path;
```

**Sourced import** — give a local alias to any module declared in `[require]`. The URL must match a key in `[require]`:

```promise
use parser "github.com/acme/parser";

main() {
  parser.parse("...")
}
```

The alias is the only way to reference that module's exports in the file. Both forms make every dependency visible at the top of the file — a reader never needs to consult `promise.toml` to understand what `json.parse` or `parser.parse` refers to.

**Catalog vs. external:** Catalog modules are built into the compiler and always available — `use json;` with no `[require]` entry. External modules must be declared in `[require]` before they can be imported with a sourced `use`. Attempting to add a catalog module to `[require]` is a compile error.

### 4.3 Module Identity and Pinning

Remote modules are pinned by commit hash directly in `promise.toml` — the `[require]` value is the exact commit (or tag) to check out. `promise package pin <url> [ref]` resolves one remote dependency's ref (tag, branch, commit, or `HEAD`) to a full commit SHA and writes it back to `promise.toml`. To bump every declared dependency to the latest commit at once, run `promise package update`. The file is checked in and guarantees reproducible builds — `promise.toml` itself is the lockfile.

```bash
promise package pin <url> [ref]   # pin one remote dependency to a resolved commit SHA
promise package update            # update all dependency pins to latest commits
promise clean                     # clear local build cache
promise clean --global            # clear global module + build cache
```

### 4.4 Visibility

Promise uses a two-level visibility model: **module-level** and **member-level**.

#### Module-Level Visibility

Top-level declarations (types, enums, functions) are **private by default**. To export a declaration from a module, annotate it with `` `public ``:

```promise
type User `public {          // exported — visible to `use` consumers
  string name;
  int age;
}

type Cache {                  // private — only visible within this module
  map[string, string] data;
}

greet(string name) string `public {   // exported function
  return "Hello, {name}!";
}

helper() int {               // private — not exported
  return 42;
}
```

The `ExportedScope` of a module contains only `` `public ``-annotated declarations. Consumers using `use mymod;` can only access `mymod.User`, `mymod.greet`, etc. — not `Cache` or `helper`.

#### Member-Level Visibility

Members (fields, methods) of a `` `public `` type are **public by default**. The underscore prefix (`_`) convention marks a member as private:

```promise
type Connection `public {
  string host;              // public — visible to consumers
  int port;                 // public
  int _retry_count;         // private — hidden from doc, conventionally internal

  connect!(~this) {} // public method
  _reset_backoff(~this) {}  // private method
}
```

**Rules:**
- Members without `_` prefix → **public** (shown in `promise doc`, accessible to consumers)
- Members with `_` prefix → **private** (hidden from `promise doc -public`, conventionally internal)
- Explicit `` `public `` on a member is allowed but redundant for members of a public type
- Operators (`+`, `==`, `[]`, etc.) are always public

This model balances explicitness at module boundaries (types/functions need `` `public ``) with practicality inside types (annotating every method would be verbose and add no information). The `_` prefix is a lightweight, visible signal — a reader can immediately tell whether a member is part of the public API just by looking at its name.

---

## 5. Type System

### 5.1 Primitive Types — Defined as Regular Types

Promise does **not** have a separate namespace for primitive types. Instead, primitives are defined as regular `type` declarations annotated with `` `native ``, where the compiler backend provides the underlying representation and operator implementations. This means `int`, `f64`, `bool`, etc. are all types in the standard library, not compiler magic.

#### Raw Fields

A field annotated with `` `raw `` uses an LLVM type identifier directly as its type instead of a Promise type. LLVM type identifiers (`i1`, `i8`, `i16`, `i32`, `i64`, `float`, `double`, etc.) are valid identifiers in Promise and can appear as the type of a `` `raw `` field.

`` `raw `` is only supported on field definitions. Everywhere else — variable declarations, function parameters, return types — you must use a Promise-defined type. This keeps the type system simple: only field definitions need special handling for raw values.

#### Standard Library Primitive Definitions

```promise
type int `native {
  +(int other) int `native;
  -(int other) int `native;
  *(int other) int `native;
  /(int other) int `native;
  %(int other) int `native;
  ==(int other) bool `native;
  !=(int other) bool `native;
  <(int other) bool `native;
  >(int other) bool `native;
  <=(int other) bool `native;
  >=(int other) bool `native;
  -() int `native;                // unary negation
}

type i8 `native { /* same pattern */ }
type i16 `native { /* same pattern */ }
type i32 `native { /* same pattern */ }
type i64 `native { /* same pattern */ }

type uint `native { /* same pattern */ }
type u8 `native { /* same pattern */ }
type u16 `native { /* same pattern */ }
type u32 `native { /* same pattern */ }
type u64 `native { /* same pattern */ }

type f32 `native { /* same pattern */ }
type f64 `native { /* same pattern */ }

type bool `native {
  &&(bool other) bool `native;
  ||(bool other) bool `native;
  ==(bool other) bool `native;
  !=(bool other) bool `native;
  !() bool `native;
}
```

Types annotated with `` `native `` have their underlying representation provided by the compiler backend — there are no user-visible fields. The `` `native `` annotation on methods means the runtime/compiler backend provides the implementation directly. This unifies the type system — there is no distinction between "primitive" and "user-defined" types.

#### Operator Overloading

Operators are ordinary methods whose name is the operator symbol. The compiler does **not** generate any built-in operator code — all operators for all types (including primitives) are defined as methods in the standard library.

Methods marked `` `native `` have no Promise body; the runtime/compiler backend provides the implementation directly (e.g. mapping `int.+` to an LLVM `add` instruction).

Any user-defined type can define operators the same way:

```promise
type Vec2 {
  f64 x `value;
  f64 y `value;

  +(Vec2 other) Vec2 {
    return Vec2(x: this.x + other.x, y: this.y + other.y);
  }

  ==(Vec2 other) bool {
    return this.x == other.x && this.y == other.y;
  }
}
```

Supported operator method names: `+`, `-`, `*`, `/`, `%`, `==`, `!=`, `<`, `>`, `<=`, `>=`. Unary operators use the same symbol with no parameters (e.g. `-() int` for negation). Both unary and binary forms can coexist on the same type — the compiler disambiguates by argument count.

#### Operator Precedence

Operator precedence is fixed by the language and cannot be overridden by user-defined types. From highest to lowest:

| Precedence | Operators | Associativity |
|-----------|-----------|---------------|
| 1 (highest) | `.` `?.` `()` `[]` `[:]` · postfix `?` `?^` `?!` `!` (error handling / optional unwrap) | Left |
| 2 | Unary prefix `-` `!` `~` `<-` | Right |
| 3 | `*` `/` `%` | Left |
| 4 | `<<` `>>` | Left |
| 5 | `+` `-` | Left |
| 6 | `&` `^` `\|` (bitwise) | Left |
| 7 | `..` `..=` | Non-associative |
| 8 | `<` `>` `<=` `>=` | Non-associative |
| 9 | `is` | Non-associative |
| 10 | `as` `as!` | Left |
| 11 | `==` `!=` | Non-associative |
| 12 | `&&` | Left |
| 13 | `\|\|` | Left |
| 14 (lowest) | `?:` (elvis) | Right |

Assignment (`=`, `+=`, `-=`, …) is a **statement**, not an expression, so it does not appear in this table.

### 5.2 The Four-Struct Model

Every type declaration `T` produces four LLVM structs at compile time. These structs form a chain: **Value → Instance → Variant → Type**.

#### Allocation Model

- **Value struct** — the unit of passing. Contains a vtable pointer, an instance pointer, and explicitly `` `value ``-annotated fields. Allocated on the stack, embedded in other value/instance structs, or passed as function parameters and return values. Always copied on assignment (value semantics).
- **Instance struct** — always heap-allocated. This is the standard "object" representation.
- **Variant struct** — generated at compile time. One per unique monomorphization. Never dynamically allocated.
- **Type struct** — generated at compile time. One per `type` declaration. Never dynamically allocated.

#### 1. **Value Struct** (`T#v`)
- The **unit of passing** — all function parameters, return values, and variable bindings are value structs.
- Contains a **vtable pointer** that determines how fields and methods are dispatched (see Section 5.2.1).
- Contains a **pointer to the Instance struct** that owns it.
- Contains **only** fields explicitly annotated with `` `value ``. Unannotated fields go to the Instance struct.
- Fields in the value struct can be raw LLVM types (`` `raw `value ``) or Promise types (`` `value ``). When a Promise type is placed in the value struct, its own value struct is embedded (concatenated) inline — no pointer indirection.
- **Always copied** on assignment (value semantics).
- For types with ownership fields, a copy performs a deep clone (or is disallowed if the type is not `` `clone ``).

#### 2. **Instance Struct** (`T#i`)
- Contains the instance fields (unannotated / default) and a single **pointer to its Variant**.
- Always heap-allocated. This is the default representation when you use a type — the standard "object".
- Does **not** contain a pointer to its Value — values reference the instance, not the other way around.
- Subject to ownership rules.

#### 3. **Variant Struct** (`T#m`)
- Represents **one concrete monomorphization** of a generic type (all generic parameters resolved).
- Contains resolved generic type info and a **pointer to the Type struct**.
- Shared across all instances of `T[ConcreteG1, ConcreteG2]`.
- Generated once per unique set of type arguments at compile time.
- Fields annotated with `` `variant `` live here.

#### 4. **Type Struct** (`T#t`)
- Matches the source-code type **declaration** 1:1.
- Contains the unresolved/generic metadata: name, generic parameter descriptors, inheritance chain, field layout info, meta annotations.
- Used for reflection and compile-time meta-programming.
- One per `type` declaration in the source code.
- Fields annotated with `` `type `` live here.

#### The Pointer Chain

```
T#v  ──ptr──▶  T#i  ──ptr──▶  T#m  ──ptr──▶  T#t
 │
 └── vtable_ptr ──▶  view-specific vtable (compile-time generated)
```

A value always points to its owning instance. The instance points to its variant. The variant points to its type. Given a value, you can reach all four structs by following pointers. The chain is one-directional — instance does not point back to value. The vtable pointer in the value struct points to a **view-specific vtable** (see Section 5.2.1) that is separate from the pointer chain.

A **view** is the perspective through which a value is accessed via a particular type in its inheritance chain. When a `Dog` value is passed where `Animal` is expected, the `Animal` view of `Dog` is used — the vtable pointer is set to the `Animal`-layout vtable generated for `Dog`, exposing only `Animal`'s fields and methods.

#### Diagram

```
┌──────────────────────────────────────────────────────────────────┐
│  T#t  (1 per source declaration, compile-time generated)         │
│  - name: "Vector"                                                │
│  - generic_params: [E]                                           │
│  - fields: [...]                                                 │
│  - meta: [...]                                                   │
│  - parent_type: nullable                                         │
│  - `type fields live here                                        │
│                                                                  │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │ T#m[int]  (1 per monomorphization, compile-time gen)       │  │
│  │ - resolved_params: {E: int}                                │  │
│  │ - type_ptr: → T#t ◀───────────────────────────────────── │  │
│  │ - `variant fields live here                                │  │
│  └────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────┐
│ T#i (1 per live object, heap-allocated)                       │
│ - variant_ptr: → T#m[int]                                     │
│ - default (unannotated) instance fields live here             │
└──────────────────────────────────────────────────────────────┘
        ▲
        │ instance_ptr
┌──────────────────────────────────────────────────────────────┐
│ T#v  (stack-allocated, copied on assignment)                  │
│ - vtable_ptr: → view-specific vtable (see 5.2.1)             │
│ - instance_ptr: → T#i                                         │
│ - `value fields live here                                     │
│ - items: [1, 2, 3]                                            │
│ - count: 3                                                    │
└──────────────────────────────────────────────────────────────┘

    vtable_ptr ──▶
┌──────────────────────────────────────────────────────────────┐
│ Vtable  (compile-time generated, one per view)                │
│ - get_field_0: fn(T#v) -> FieldType                           │
│ - set_field_0: fn(T#v, FieldType)                             │
│ - method_0:    fn(T#v) -> ReturnType                          │
│ - ...                                                         │
└──────────────────────────────────────────────────────────────┘
```

#### 5.2.1 Vtable Dispatch Model

The vtable pointer in the value struct is the **sole mechanism** for field access and method dispatch. Every field produces a getter and setter slot in the vtable. Every method produces a method slot. The call site accesses fields and calls methods exclusively through the vtable — it never directly reads memory from the instance or value struct.

**Vtable structure for a type:**

```promise
type Animal {
  string name;
  int age;
  speak() string `abstract;
}
```

The compiler generates:

```
Animal_vtable = {
  [0] get_name:  fn(Animal#v) -> string
  [1] set_name:  fn(Animal#v, string)
  [2] get_age:   fn(Animal#v) -> int
  [3] set_age:   fn(Animal#v, int)
  [4] speak:     fn(Animal#v) -> string
}
```

At the call site, `animal.name` compiles to `vtable[0](animal_value)` and `animal.speak()` compiles to `vtable[4](animal_value)`. The call site does not know or care whether `name` is a stored field or a computed getter — the vtable function handles it.

**Slot assignment:** Slots are assigned in **declaration order** within each inheritance level, with parent slots always forming a **prefix**. When a child extends a parent, the parent's slots occupy the first N positions (unchanged), and the child's new fields and methods are appended after. This ensures that a parent's vtable layout is a prefix of every child's vtable layout — maximizing vtable reuse across types that share the same parent. Multiple types inheriting from the same parent share identical slot positions for the parent's fields and methods.

**Why fields go through the vtable:**

This decouples the call site from the concrete implementation. A parent type can declare a field `string name`, and a child type can satisfy it with either a stored field or a computed getter — the call site code is identical in both cases. This is what enables interfaces to declare data fields that concrete types can implement however they choose (see Section 5.4).

**Getter/setter syntactic sugar:**

Since all field access goes through vtable getter/setter calls, the language provides syntactic sugar so that field access looks like direct member access, not function calls. The dot operator (`.`) on a field name desugars to the corresponding vtable getter or setter:

```promise
// Reading — dot access desugars to getter call
animal.name                       // → vtable.get_name(animal_value)
player.position.x                 // → vtable.get_x(vtable.get_position(player_value))

// Writing — assignment through dot desugars to setter call
animal.name = "Rex";              // → vtable.set_name(animal_value, "Rex")

// Compound assignment — desugars to get + operator + set
player.health += 10;              // → vtable.set_health(pv, vtable.get_health(pv) + 10)
player.score *= 2;                // → vtable.set_score(pv, vtable.get_score(pv) * 2)

// Chained access
game.world.player.health += 1;   // each dot is a getter; final assignment is a setter
```

This sugar is essential because without it, code would require explicit getter/setter calls (`animal.getName()`, `animal.setName("Rex")`), and operators on fields would be impossible — you cannot write `animal.getName() += 10`. The dot syntax makes vtable-dispatched access indistinguishable from direct field access at the call site.

**Inheritance extends the vtable:**

```promise
type Dog is Animal {
  string breed;
  speak() string { return "Woof!"; }
}
```

Dog's vtable **starts with** Animal's layout and appends new slots:

```
Dog_vtable = {
  // Animal slots (same positions — prefix-compatible)
  [0] get_name:  fn(Dog#v) -> string      → Dog's name field getter
  [1] set_name:  fn(Dog#v, string)        → Dog's name field setter
  [2] get_age:   fn(Dog#v) -> int         → Dog's age field getter
  [3] set_age:   fn(Dog#v, int)           → Dog's age field setter
  [4] speak:     fn(Dog#v) -> string      → Dog.speak

  // Dog-specific slots (appended)
  [5] get_breed: fn(Dog#v) -> string      → Dog's breed field getter
  [6] set_breed: fn(Dog#v, string)        → Dog's breed field setter
}
```

For single inheritance, the child's vtable is prefix-compatible with the parent's — a Dog vtable pointer works wherever an Animal vtable pointer is expected, because slots 0–4 are at the same positions.

**Multiple inheritance — per-view vtables:**

When a type has multiple parents, the compiler generates a **separate vtable for each parent view**. This is necessary because different parents have different slot layouts.

```promise
type Named {
  string name;
  greet() string { return "Hi, I'm {this.name}"; }
}

type Audible {
  volume() int `abstract;
  speak() string `abstract;
}

type Dog is Named, Audible {
  string breed;
  int loudness;

  speak() string { return "Woof!"; }
  volume() int { return this.loudness; }
}
```

The compiler generates three vtables:

```
Named_vtable_for_Dog = {
  [0] get_name  → Dog's name getter
  [1] set_name  → Dog's name setter
  [2] greet     → Named.greet (default impl, accesses name through vtable)
}

Audible_vtable_for_Dog = {
  [0] volume    → Dog.volume
  [1] speak     → Dog.speak
}

Dog_vtable = {
  [0] get_name  → ...
  [1] set_name  → ...
  [2] greet     → Named.greet
  [3] volume    → Dog.volume
  [4] speak     → Dog.speak
  [5] get_breed → ...
  [6] set_breed → ...
  [7] get_loudness → ...
  [8] set_loudness → ...
}
```

When a Dog value is passed where `Named` is expected, the vtable pointer in the value struct is set to `Named_vtable_for_Dog`. When passed where `Audible` is expected, it is set to `Audible_vtable_for_Dog`. The call site always sees the vtable layout it expects.

**Interfaces with data fields:**

Because fields are accessed through vtable getter/setter slots, an interface (a type with abstract methods) can also declare data fields. Concrete types satisfy the interface by providing the field — either as stored data or as computed getters/setters:

```promise
type Positioned {
  f64 x;
  f64 y;
  distanceTo(Positioned other) f64 {
    dx := this.x - other.x;
    dy := this.y - other.y;
    return math.sqrt(dx * dx + dy * dy);
  }
}

type Player is Positioned {
  // Stores x, y as real fields — vtable getters read from instance memory
  f64 x;
  f64 y;
  string name;
}

type CameraTarget is Positioned {
  // No stored x, y — computed from tracked entity
  Entity tracked;

  x() f64 { return this.tracked.position().x; }
  y() f64 { return this.tracked.position().y; }
}
```

Both satisfy `Positioned`. The call site `pos.x` compiles to `vtable.get_x(pos_value)` — it does not know or care whether `x` is a stored field or a computed property. The `distanceTo` default implementation works for both because it accesses `x` and `y` through the vtable.

**Optimization:**

The vtable dispatch model is the **semantic model** — all access conceptually goes through the vtable. In practice, when the compiler knows the concrete type at a call site, an optimization pass can:

- **Devirtualize**: replace vtable dispatch with direct function calls.
- **Inline field access**: replace `vtable.get_x(value)` with a direct memory load.
- **Elide the vtable pointer**: remove it entirely for non-polymorphic values.

The code structure prioritizes flexibility over performance. Optimizations are layered on top without compromising the model.

#### Field Placement Annotations

When defining a field in a type, the field goes into the **Instance struct** by default. Use meta annotations to place it elsewhere:

```promise
type Player {
  // Instance fields (default — no annotation needed)
  string name;
  int health;

  // Value field — lives in T#v, copied with the value struct
  f64 x `value;
  f64 y `value;

  // Variant field — shared across all instances of this monomorphization
  string spritePath `variant;

  // Type field — shared across all instances of this type declaration
  string typeName `type;
}
```

- **Instance fields** (default): per-object, heap-allocated. Each instance has its own copy.
- **`` `value `` fields**: live in the value struct. Copied every time the value is passed or assigned. Best for small, frequently-accessed data (coordinates, flags).
- **`` `variant `` fields**: shared across all instances of the same generic monomorphization. Useful for per-specialization metadata (e.g., a sprite path shared by all `Player[Warrior]` instances). Mutable only at initialization.
- **`` `type `` fields**: shared across all instances of the type declaration regardless of generic parameters. Useful for reflection metadata. Mutable only at initialization.

#### Pure Value Types

When **all** fields in a type have `` `value `` placement, the type is a **pure value type** — it behaves like a primitive:

```promise
type Point {
  int x `value;
  int y `value;

  sum(this) int {
    return this.x + this.y;
  }

  scale(this, int factor) Point {
    return Point(x: this.x * factor, y: this.y * factor);
  }
}

main() {
  p := Point(x: 3, y: 4);
  Point q = p;          // copy, not move — q is independent
  q.x = 10;             // does not affect p
  print_line(p.sum());   // 7
}
```

**Value struct layout**: `{ i8* _vtable, field1, field2, ... }` — fields embedded at indices 1+, no heap allocation. RTTI is accessed via the compile-time-known global `promise_rtti_<T>`, not stored in the value struct. The Instance struct is a global RTTI-only singleton `{ T_m* _variant }`.

**Rules**:
- Automatically `` `copy `` — no explicit annotation needed
- All fields must themselves be copy types (primitives, other value types, bool, char)
- Cannot have `is` parents (leaf types only)
- Cannot have `drop()` methods (nothing to clean up)
- Cannot have failable `new()` methods
- Methods work normally — `this`/`~this` receive a pointer to the value struct alloca

**Use cases**: coordinates (`Point`, `Vec2`), dimensions (`Size`, `Rect`), colors (`Color`), ranges, small fixed-size data.

**Hybrid types** — a type that mixes `` `value `` fields with regular instance (heap) fields — fit the same four-struct model: the value struct embeds the `` `value `` fields directly *and* carries the instance pointer for the heap fields, so a method reaches every field uniformly (`` `value `` fields in the value struct, instance fields through the pointer). This is the natural consequence of treating all types through one layout rather than forking pure-value and pure-instance into separate models. Hybrid types are **not yet implemented**, however: for now a type's fields must be either **all** `` `value `` (a pure value type) or **none** (a regular instance type); mixing the two is rejected at compile time with a clean source-located diagnostic (`type T mixes \`value\` and instance fields; … not yet supported`). Support may be added in the future.

#### Primitives in the Four-Struct Model

Since primitives are regular types, `int` works like any other type in the four-struct model:

- `int#v` = `{ vtable*, int#i*, i64 }` — vtable pointer, instance pointer, and the raw i64 value
- `int#i` = `{ int#m* }` — pointer to variant (no pointer back to value)
- `int#m` = `{ int#t* }` — pointer to type (compile-time generated)
- `int#t` = `{ metadata }` — name, reflection info (compile-time generated)

The `int` vtable contains getter/setter for the raw value plus all operator methods (`+`, `-`, `==`, etc.). For performance, the compiler optimizes away unnecessary indirection for primitives (e.g., `int` on the stack is just an `i64` in practice, with the vtable/instance/variant/type pointers elided when not needed).

### 5.3 Variable Declarations

Variable declarations use **type-first** syntax (Dart/C++ style):

```promise
int x = 42;
string name = "Alice";
f64 pi = 3.14159;
int[] numbers = [1, 2, 3];
map[string, int] scores = {"alice": 100, "bob": 85};
```

Type inference with `:=`:

```promise
x := 42;                // inferred as int
name := "Alice";        // inferred as string
nums := int[]();        // inferred as int[] (Vector[int])
```

#### Typed Numeric Literal Suffixes

Numeric literals can carry a type suffix to specify their exact type without casting. This avoids the need for `as!` casts when working with fixed-width numeric types.

**Integer suffixes:** `u8`, `u16`, `u32`, `u64`, `i8`, `i16`, `i32`, `i64`
**Float suffixes:** `f32`, `f64`

```promise
x := 255u8;             // x is u8
y := 1000u16;           // y is u16
z := 3.14f32;           // z is f32
big := 1_000_000u32;    // underscores work with suffixes
hex := 0xFFu8;          // hex/octal/binary bases work with suffixes
```

The suffix determines the type — it overrides any type hint context. A mismatch between the declared type and the suffix is a compile error:

```promise
u8 a = 42u8;            // OK: suffix matches declared type
u16 b = 10u8;           // ERROR: cannot assign u8 to u16
```

Range validation is performed at compile time. Signed negative minimums like `-128i8` are handled correctly — the compiler recognizes that the unary negation and the literal form a single value:

```promise
i8 min = -128i8;        // OK: -128 is within i8 range
i8 bad = 128i8;         // ERROR: 128 overflows i8 (max 127)
u8 big = 256u8;         // ERROR: 256 overflows u8 (max 255)
```

### 5.4 Inheritance

A type declares its parent types with `is`. There is no distinction between inheritance and interface implementation — both use the same keyword. An interface is simply a type whose methods are all `` `abstract `` — it uses the same `is` keyword and the same vtable machinery.

```promise
type Shape {
  f64 x;
  f64 y;

  area() f64 `abstract;
}

type Circle is Shape {
  f64 radius;

  area() f64 { return 3.14159 * this.radius * this.radius; }
}

type Drawable {
  draw(Canvas canvas) `abstract;
}

type Circle is Shape, Drawable {
  f64 radius;

  area() f64 { ... }

  draw(Canvas canvas) {
    canvas.drawEllipse(this.x, this.y, this.radius);
  }
}
```

#### How Inheritance Maps to Vtables

When a type inherits from a parent, the child's vtable **extends** the parent's layout — the parent's slots appear first at the same positions, and the child appends its own new slots (see Section 5.2.1). This means:

- **Single inheritance**: the child's vtable is prefix-compatible with the parent's. A Circle vtable pointer works wherever a Shape vtable pointer is expected.
- **Multiple inheritance**: the compiler generates a **per-view vtable** for each parent. When a Circle is passed where `Shape` is expected, the value carries a Shape-view vtable. When passed where `Drawable` is expected, it carries a Drawable-view vtable. Each call site sees the slot layout it expects.
- **Field inheritance**: parent fields become getter/setter slots in the vtable. A child can inherit them as stored fields, override them with computed getters/setters, or provide them from a completely different source — the parent's call sites are unaffected.
- **Default method implementations**: a parent type can provide method bodies. These become concrete function pointers in the child's vtable. The child can override them by providing its own implementation, which replaces the function pointer in the vtable slot. `` `abstract `` and a method body are **mutually exclusive**: `` `abstract `` means no body and the child must override; a body means a default implementation that the child may optionally override.
- **Field placement inheritance**: when a child inherits from a parent, field placement annotations (`` `value ``, `` `variant ``, `` `type ``) are inherited as declared. If the parent declares `f64 x `value`, the child's value struct also contains `x`.

#### Structural Interface Satisfaction

By default, a type must declare `is Interface` to be assignable to an interface type. This keeps intent explicit and prevents accidental satisfaction.

For lightweight, widely-satisfied interfaces (e.g. `Printable`, `Hashable`), the `` `structural `` meta tag on the interface enables **structural satisfaction**: any type that provides concrete implementations for all of the interface's abstract methods — with matching signatures — is automatically assignable to that interface, even without `is`.

```promise
type Printable `structural {
  toString() string `abstract;
}

type Point {
  int x;
  int y;

  // No `is Printable` — but satisfies Printable structurally
  toString() string { return "(" + this.x.toString() + ", " + this.y.toString() + ")"; }
}

Printable p = Point(x: 1, y: 2);  // OK — Point has toString() with matching signature
p.toString();                       // dispatches to Point.toString via view-specific vtable
```

Structural satisfaction uses **relaxed signature matching**: the concrete type's method must be compatible with the interface method, but can be more specific:

- **Extra parameters**: the concrete method may have additional parameters beyond those declared in the interface, as long as all extras have default values or are optional types. The compiler generates an **adapter thunk** that supplies defaults/nones for the omitted parameters.
- **Failable**: a non-failable concrete method satisfies a failable interface method (but not vice versa). The adapter wraps the result in a success failable struct.
- **Optional return**: a concrete method returning `T` satisfies an interface requiring `T?`. The adapter wraps the result as `some`.

Structural interfaces can also declare **abstract factory methods** — static constructors that enable generic factory patterns:

```promise
type Parseable `structural {
    parse(string data) `abstract `factory;   // implicit Self return
}

type Json {
    string raw;
    parse(string data) Json `factory { return Json(raw: data); }
    format() string { return "json:{this.raw}"; }
}

// Generic function constrained by factory interface
load[T: Parseable](string data) T {
    return T.parse(data);  // monomorphized: calls Json.parse(data) directly
}

Json j = load[Json]("...");
```

Abstract factory methods declared without a return type get an **implicit `Self` return**. Failable abstract factories (`tryParse!(string data) \`abstract \`factory;`) get implicit `Self` return. Factory methods must match factory-to-factory: an instance method does not satisfy a factory requirement and vice versa.

When a value crosses a type boundary through structural satisfaction (or through a second+ parent), the compiler emits a **view-specific vtable** ordered by the target interface's slot layout. The value struct's vtable pointer is swapped to this view vtable at the coercion point (variable declaration, assignment, function argument, or return statement). For methods with relaxed signature differences, the vtable slot points to an adapter thunk rather than the method directly.

#### Generic Inheritance

A child type can inherit from a **generic parent** by supplying type arguments in the `is` clause. This works for all inheritance patterns: non-generic children, generic children forwarding type params, and partial application of multi-param parents.

```promise
// Non-generic child of generic parent — fixes all type params
type Holder[T] { T value; }
type IntHolder is Holder[int] {}

// Generic child forwarding type params
type Container[T] { T item; get() T { return this.item; } }
type Wrapper[T] is Container[T] { string label; }

// Partial type argument application
type Pair[A, B] { A first; B second; }
type StringPair[V] is Pair[string, V] {}

// Abstract generic parent
type Producer[T] { produce() T `abstract; }
type ConstProducer[T] is Producer[T] {
    T value;
    produce() T { return this.value; }
}
```

**Rules:**

- Type arguments in the `is` clause can be concrete types (`int`, `string`) or the child's own type parameters (`T`, `V`).
- **Transitive chains** work: `type Leaf is Middle[int]` where `type Middle[T] is Base[T]` — `Leaf` inherits all fields and methods from both `Middle` and `Base`, with `T` resolved to `int` throughout.
- **Assignability** follows: a `Wrapper[int]` is assignable to `Container[int]`, and a `Leaf` is assignable to `Base[int]`. This works transitively through generic intermediaries.
- **Method dispatch**: inherited methods have their type parameters substituted. Calling `get()` on a `Wrapper[int]` returns `int`, not `T`.
- All parent type arguments must be valid types (no raw type parameters from an unrelated scope).

### 5.5 Generics

Generics use **square brackets** `[]`. Constraints are expressed inline in the type parameter list.

```promise
type map[K: Hashable + Equal, V] {
  Bucket[K, V][] buckets;

  get(K key) V&? `instance { ... }
  set(K key, V value) `instance { ... }
}

sort[T: Ordered](T[]~ list) {
  ...
}
```

#### Standard Constraint Interfaces

The standard library provides these interfaces for use as generic constraints:

```promise
type Equal {
  ==(Self other) bool `abstract;
  !=(Self other) bool { return !(this == other); }
}

type Ordered is Equal {
  <(Self other) bool `abstract;
  >(Self other) bool { return other < this; }
  <=(Self other) bool { return !(this > other); }
  >=(Self other) bool { return !(this < other); }
}

type Hashable {
  hash() int `abstract;
}
```

All primitive types (`int`, `f64`, `string`, `bool`, etc.) implement `Equal` and `Ordered`. `string` and `int` also implement `Hashable`. User-defined types can implement these interfaces to participate in generic algorithms like `sort`, `map` key lookup, and stream combinators like `distinct()`, `min()`, and `max()`.

#### Method-Level Generics

Methods can have their own type parameters, independent of the enclosing type's generic parameters. This enables transformation methods like `map[R]` where the return type differs from the owner's type parameter.

```promise
type Box[T] {
  T value;

  // Method-level type param R, independent of type-level T
  transform[R]((T) -> R fn) R {
    return fn(this.value);
  }

  // Multiple method-level type params
  zip[A, B]((T) -> A fa, (T) -> B fb) Pair[A, B] {
    return Pair[A, B](first: fa(this.value), second: fb(this.value));
  }
}

main() {
  b := Box[int](value: 42);
  string s = b.transform[string](|int x| -> "value: {x}");
  int doubled = b.transform[int](|int x| -> x * 2);
}
```

**Call syntax:** `obj.method[TypeArgs](args)` — type arguments are provided in square brackets between the method name and the argument list. Type arguments are always explicit (no inference).

**Rules:**

- Method-level type parameters are declared after the method name: `method[R, S](params) ReturnType { ... }`
- Both the method's own type params and the owner's type params are in scope within the method body
- Method-level generics are monomorphized independently — `Box[int].transform[string]` and `Box[int].transform[f64]` produce separate specialized functions
- **Cannot be virtual or abstract** — generic methods use direct dispatch only (same restriction as C++ virtual templates). A generic method is excluded from the vtable
- Generic methods can be inherited — a child type inherits the parent's generic methods and can call them with any type arguments
- Generic methods work on both generic and non-generic owner types

**Interaction with type-level generics:** When a generic method is called on a generic type, substitution composes both levels. Calling `container.map_to[string](fn)` on a `Container[int]` resolves `T=int` (from the type) and `R=string` (from the method call):

```promise
type Container[T] {
  T item;

  map_to[R]((T) -> R fn) R {
    return fn(this.item);
  }
}

c := Container[int](item: 7);
string result = c.map_to[string](|int x| -> "got {x}");
```

### 5.6 Enums (Algebraic Data Types)

```promise
enum Option[T] {
  Some(T),
  None,
}

enum Result[T, E] {
  Ok(T),
  Err(E),
}

enum Color {
  Red,
  Green,
  Blue,
  Custom(u8 r, u8 g, u8 b),
}
```

#### Enum Methods

Enums can declare methods and getters inside the body, after the variant list. Methods receive the enum value as `this` and can pattern-match on it to implement variant-specific behavior.

```promise
enum Shape {
  Circle(f64 radius),
  Rectangle(f64 width, f64 height),
  Point,

  // Method — called as s.area()
  area(this) f64 {
    match this {
      Shape.Circle(r) => { return 3.14159 * r * r; },
      Shape.Rectangle(w, h) => { return w * h; },
      Shape.Point => { return 0.0; },
    }
  }

  // Getter — accessed as s.is_flat (no parens)
  get is_flat bool {
    match this {
      Shape.Point => { return true; },
      _ => { return false; },
    }
  }

  // Expression body shorthand
  is_circle(this) bool => !this.is_flat;

  // Failable method
  validate!(this) string {
    if this.is_flat { raise error(message: "flat shapes not allowed"); }
    return "ok";
  }
}

s := Shape.Circle(radius: 5.0);
f64 a = s.area();        // method call
bool flat = s.is_flat;   // getter (property syntax)
```

**Rules:**

- Methods are declared after all variants, separated by commas from the last variant.
- Methods receive `this` as the enum value (value semantics, not a pointer from the caller's perspective). A bare `this` is a shared borrow (by value for these value-semantic enums); `~this` is a mutable borrow.
- Methods can call other methods on `this` (e.g., `this.rank()` or `this.label` for getters).
- Enum methods **cannot** be `` `abstract ``, `` `native ``, `` `factory ``, `` `global ``, or `` `mono ``.
- Enum methods support expression bodies (`=> expr;`), failable return types (`!`), default parameters, and all standard method features.
- Generic enum methods are not yet supported (deferred to monomorphization support).

### 5.7 Constructors

Promise supports implicit constructors (auto-generated from fields) and explicit `new` constructors (user-defined with validation). Factory constructors provide named alternative construction paths. The `new`/`drop` pair forms a symmetric constructor/destructor lifecycle.

#### Implicit Constructors

For simple types with no validation needs, the compiler generates a constructor from field declarations. All arguments are named:

```promise
type User {
  string name;           // required — no default, not optional
  int age;               // required
  string? bio;           // optional — T? defaults to none
  int score = 0;         // optional — has default
}

User(name: "Alice", age: 30)              // OK: bio=none, score=0
User(name: "Alice", age: 30, bio: "hi")   // OK
User(name: "Alice")                       // ERROR: missing required field 'age'
```

A field is **required** if it is not `T?` and does not have `= default`. All required fields must be provided. Default expressions are evaluated at the call site each time the argument is omitted (see Section 9.4).

#### `Self` Type Alias

`Self` is a compiler-supported type alias meaning "the enclosing type with all generic parameters as-is." After monomorphization, `Self` resolves to the concrete type.

| Context | `Self` resolves to |
|---------|-------------------|
| Inside `type Foo { ... }` | `Foo` |
| Inside `type Box[T] { ... }` | `Box[T]` |
| After monomorphization for `Box[int]` | `Box[int]` |
| Inside `type Dog is Animal { ... }` | `Dog` (not `Animal`) |

`Self` is capitalized because it is a type name, contrasting with `this` which is a value. `Self` is usable in return types, constructor calls, parameter types, field types, and local variable annotations within type bodies:

```promise
type Point {
  f64 x `final;
  f64 y `final;

  offset(f64 dx, f64 dy) Self {
    return Self(x: this.x + dx, y: this.y + dy);
  }
}
```

On generic types, `Self` resolves to the self-instantiation — `Self` inside `Box[T]` is `Box[T]`, not the raw `Box`. This ensures correct type parameter substitution during monomorphization:

```promise
type Box[T] {
  T value;
  new(~this, T move v) { this.value = v; }

  // Factory returning Self — monomorphizes to Box[int] for Box[int].wrap(...)
  wrap(T move v) Self `factory {
    return Self(v: move v);
  }

  // Instance method returning Self
  rewrap(T move v) Self {
    return Self(v: move v);
  }

  // Self as parameter type
  same_as(Self other) bool {
    return true;
  }
}

Box[int] b = Box[int].wrap(v: 42);   // b.value is int, not T
```

#### `` `final `` Fields

A field annotated `` `final `` can be assigned during construction but is frozen afterward. The compiler generates a vtable getter but **no setter**.

```promise
type Token {
  string raw `final;
  int line `final;
  int col `final;
}

main() {
  t := Token(raw: "if", line: 1, col: 0);
  string s = t.raw;       // OK — getter works
  t.raw = "else";         // ERROR: cannot assign to `final field 'raw'
}
```

Write access rules:
- **`new` body**: can assign `` `final `` fields
- **`` `factory `` body**: can assign `` `final `` fields on locally-created instances only (variables initialized from constructor calls within the factory; parameter or external instances cannot have their `` `final `` fields modified)
- **Everywhere else**: assignment is a compile error

Additional rules:
- Child types cannot shadow a `` `final `` field with a mutable one
- `` `final `` fields can have defaults: `int version \`final = 1;`
- `` `final `` + `T?` is valid: `string? tag \`final;` (defaults to `none`, frozen after)
- Defining a custom setter on a `` `final `` field name is a compile error
- Defining a custom getter on a `` `final `` field name is allowed (overrides the generated getter)
- `` `final `` is orthogonal to placement — `` `final \`value ``, `` `final \`variant ``, `` `final \`type `` are all valid
- `` `copy `` types can have `` `final `` fields — bitwise copies get the same frozen values

#### Explicit `new` Constructor

When a type needs validation or computed initialization, define a `new` method. It **replaces** the implicit constructor.

```promise
type Percentage {
  int value `final;

  new(~this, int value) {
    if value < 0 { this.value = 0; }
    else if value > 100 { this.value = 100; }
    else { this.value = value; }
  }
}

Percentage(value: 120)    // calls new(value:), clamps to 100
Percentage(value: 50)     // calls new(value:), stores 50
```

Semantics:
1. Compiler allocates instance (zero-initialized) and stores RTTI
2. `new` body executes with an explicit `~this` (mutable borrow of the fresh instance)
3. Instance returned as value struct

Rules:
- Receiver is written explicitly as `~this` (a mutable borrow of the fresh instance)
- Return type is implicitly `Self` — not written in the signature
- Call site syntax is unchanged: `Type(args...)` — arguments match `new`'s parameter names (not field names)
- When `new` is defined, the implicit constructor is gone — all construction goes through `new`
- Every `` `final `` field must be definitely assigned on all code paths through `new`
- Non-`` `final `` fields not assigned in `new` remain zero-initialized (or use their default)

Parameters can differ from field names, enabling computed construction:

```promise
type Point {
  f64 x `final;
  f64 y `final;

  // Polar constructor — param names differ from field names
  new(~this, f64 radius, f64 angle) {
    this.x = radius * cos(angle);
    this.y = radius * sin(angle);
  }
}

Point(radius: 5.0, angle: 1.57)    // named after new() params
```

#### Failable `new`

Append `!` to make `new` failable. The caller must handle the error using standard error handling (see Section 7).

```promise
type Port {
  int value `final;

  new!(~this, int value) {
    if value < 1 || value > 65535 {
      raise InvalidArgError(msg: "invalid port number");
    }
    this.value = value;
  }
}

Port(value: 80)?!       // calls new(value:), validates, OK
Port(value: -1)?!       // calls new(value:), raises InvalidArgError
```

A failable constructor integrates with standard error handling — auto-propagation in `!` functions, explicit `^` propagation, `?` handling, or `!` to assert-and-panic (see Section 7):

```promise
serve!(int portNum) Server {
  Port p = Port(value: portNum);   // auto-propagates InvalidArgError on failure
  return Server(port: move p);
}
```

When `new` raises, the instance is never returned — no `drop()` runs on the incomplete instance. Fields assigned before the `raise` that hold non-copy values are leaked. Validation should precede field assignment to minimize this.

#### Factory Constructors

A factory is a method annotated `` `factory ``. It provides named alternative construction paths with special privileges:

- Can modify `` `final `` fields on locally-created instances
- Can return child types (return type is `Self` or a type that `is Self`)
- Can be failable (`!`)
- Can be declared `` `abstract `` on `` `structural `` interfaces (see §5.4) to enable generic factory patterns

`` `factory `` implies `` `variant `` placement — per-monomorphization, all generics resolved. This is necessary because a factory on `Box[T]` must know which `T` to create. A factory has **no `this` receiver**. Abstract factories on structural interfaces get an **implicit `Self` return type** when none is specified.

```promise
type Color {
  int r `final;
  int g `final;
  int b `final;

  red() Self `factory {
    return Self(r: 255, g: 0, b: 0);
  }

  hex!(string code) Self `factory {
    if code.len != 7 || code[0] != '#' {
      raise ParseError(msg: "invalid hex color");
    }
    c := Self(r: 0, g: 0, b: 0);
    c.r = parseHex(code[1:3]);     // OK — `factory can set `final
    c.g = parseHex(code[3:5]);
    c.b = parseHex(code[5:7]);
    return c;
  }
}

Color c1 = Color.red();
Color c2 = Color.hex!("#FF8800");
```

Factories can return child types:

```promise
type Shape `abstract {
  string color `final;

  new(~this, string move color) {
    this.color = color;
  }

  circle(string move color, f64 r) Self `factory {
    return Circle(color: move color, radius: r);
  }
}

Shape s = Shape.circle("red", 5.0);   // returns Circle typed as Shape
```

If the type defines `new`, `Self(...)` / `Type(...)` inside the factory routes through `new` — validation always runs.

#### Inheritance and Constructors

When a parent type defines `new`, the child **must** also define `new` and call `super(...)` to initialize the parent.

```promise
type Animal {
  string name `final;
  int age;

  new(~this, string move name, int age) {
    this.name = name;
    this.age = age;
  }
}

type Dog is Animal {
  string breed `final;

  new(~this, string move name, int age, string move breed) {
    super(move name, age);  // calls Animal.new
    this.breed = breed;
  }
}

Dog(name: "Rex", age: 3, breed: "Lab")
```

When the parent has only the implicit constructor, `super(field: value, ...)` uses field-name syntax:

```promise
type Dog is Animal {
  string breed `final;

  new(~this, string move name, string move breed) {
    super(name: move name, age: 0);    // field-name syntax for implicit parent constructor
    this.breed = breed;
  }
}
```

**Relaxed `super()` rule** — `super()` does NOT need to be the first statement. Instead, two invariants apply:

1. **No `this` access before `super()`** — cannot read or write any field (own or inherited) until the parent is initialized
2. **All code paths must call `super()` exactly once** — definite-call analysis (same infrastructure as `` `final `` field definite-assignment)

This enables validation before parent construction, conditional super calls, and computed arguments:

```promise
type SecureConn is Connection {
  string cert `final;

  new!(~this, string rawUrl, string move cert) {
    // Validation before super — no this access, just params + locals
    string normalized = normalizeUrl(rawUrl);
    if !isValid(normalized) {
      raise InvalidUrlError(url: rawUrl.clone());
    }
    super(url: move normalized, timeout: 30);
    this.cert = cert;
  }
}
```

```promise
type Logger is Output {
  new!(~this, string target) {
    if target == "stdout" {
      super(stream: stdout);
    } else if target == "stderr" {
      super(stream: stderr);
    } else {
      Stream f = openFile(target);
      super(stream: move f);
    }
    // all branches called super — OK
  }
}
```

Compile errors for `super()`:

| Rule | Error |
|------|-------|
| `this` access before `super()` on that path | "cannot access 'this' before calling super()" |
| Path exits `new()` without calling `super()` | "all code paths must call super()" |
| `super()` called twice on same path | "super() already called on this path" |
| `super()` inside a loop | "super() cannot be called inside a loop" |
| Child omits `new()` when parent has `new()` | "type Child must define new() because parent Parent defines new()" |
| Parent `new()` is failable but child `new()` is not | "new() on Child must be failable because parent Parent has failable new()" |

Abstract types can define `new` to enforce initialization contracts — children call `super(...)` to satisfy them. Abstract types themselves still cannot be instantiated directly.

#### Construction Lifecycle

```
allocate + zero-init + RTTI
  → super() [if parent has new]
  → new() body [if has new]
  → `final fields frozen
  → instance returned
  → ... use ...
  → drop() [if has drop, at scope exit]
  → field drops [compiler-inserted, reverse order]
  → free
```

#### Definite-Assignment Analysis

The compiler uses flow-sensitive analysis (shared infrastructure) for three checks:
1. Every `` `final `` field is assigned on all paths through `new`
2. `super()` is called on all paths (exactly once, no loops)
3. No `this` access before `super()` on any path

---

## 6. Ownership & Memory Management

Promise uses Rust-style ownership with borrowing and lifetimes.

### 6.1 Core Rules

1. Every value has exactly **one owner**.
2. When the owner goes out of scope **and the value has not been moved**, the value is **dropped** — its `drop()` runs and its memory is freed (see §16.3).
3. A value may be borrowed by **either** one mutable reference (type `T~`) **or** any number of shared references (type `T&`) at a time — never both at once. Borrows of **disjoint fields** of the same value do not conflict (a shared borrow of `v.x` and a shared borrow of `v.y` may coexist).
4. References must not outlive their referent.
5. A moved-from variable is invalid — it may not be read, borrowed, or moved again — **until it is reassigned**, which revives it as a fresh owner (*move resurrection*).

While a value is borrowed it may not be moved, consumed, reassigned, or borrowed in a conflicting mode; the compiler reports "cannot move/use … while it is borrowed". Ownership is tracked **per control-flow path**: when a value is moved on some paths but not others, the compiler inserts a drop flag and drops it only on the paths where it still has an owner (see §16.3).

Throughout this section `T&` and `T~` name the shared- and mutable-reference **types**, used for locals and return types. **Parameters and receivers** default to a shared (read-only) borrow when unmarked (`T name`); a mutable borrow reuses the `~` sigil (`T~ name`), and the keyword `move` transfers ownership. See §6.2.

**Borrows are stack-only — there are no reference fields.** A borrow may live in a parameter, a local, or a return value — all bounded by the call stack, so the compiler checks them one function at a time. A borrow may **not** be stored in a struct field or otherwise escape onto the heap; holding a borrow in heap data would require threading lifetimes through types (the hardest part of a borrow checker) for little gain. To keep or share a reference *inside* a struct, use **`Ref[T]`** — a reference-counted shared-ownership handle (§17.4). The rule is one line: *borrow for momentary access; `Ref[T]` to keep or share.* Whether a `Ref`'s counter is atomic is an implementation detail, not part of the type — it is non-atomic when the value never crosses a `go`/channel/`Task` boundary and atomic when it might.

### 6.2 Borrowing and Moving

A parameter declares how the callee accesses its argument. There are three modes. The **shared (read-only) borrow is the unmarked default**; the two that deviate from read-only access are marked — `~` (the mutable-reference sigil) for write access, and the keyword `move` for taking ownership:

```promise
// shared borrow (read-only) — the default; caller keeps ownership
char_count(string text) int {
  return text.len;
}

// mutable borrow — callee may modify the value; caller keeps ownership
add_zero(int[]~ items) {
  items.push(0);
}

// move (consume) — callee takes ownership; caller may not use the argument afterward
store(string move text) {
  // text is dropped at end of scope, unless moved on again
}

main() {
  string name = "promise";
  int n = char_count(name);   // shared borrow — no marker; name still valid
  print_line("{n} {name}");

  int[] xs = [1, 2];
  add_zero(xs);               // mutable borrow — no marker; xs still valid, now [1, 2, 0]
  print_line("{xs.len}");

  store(move name);           // move — `move` marks the consume; name is gone after this line
}
```

| Parameter | Access | Caller after the call |
|-----------|--------|-----------------------|
| `T name` | shared borrow (read-only) | still owns the value |
| `T~ name` | mutable borrow (read + write) | still owns the value |
| `T move name` | move / consume | value is gone |

For `Copy` types (primitives, `char`, `bool`, pure value types) every mode is a by-value copy, so the distinction is irrelevant; it matters only for move types (`string`, collections, heap user types). A `move` parameter is the only way to consume an argument; a borrow parameter may read (and, if `~`, modify) the value but may **not** move it out (into a field, another `move` callee, a by-value return, or a capture). This prevents double-free bugs where both the caller and a transitive consumer would drop the same allocation.

**The call site marks consumption, and nothing else.** Consuming a named binding as an argument is written `f(move x)`; borrows — shared *or* mutable — carry no marker. The asymmetry is deliberate: a borrow is transient (the value survives the call, and a mutation is observable right there), whereas a move is permanent and its "value is gone" error surfaces *later*, away from the call — the one effect worth announcing.

- **`move` required** — consuming a named binding as a **call or constructor argument**: `f(move x)`, `R(field: move x)`, `Factory.make(move x)`. A constructor (and a factory method) is just a function call; the default constructor's parameters are the owned fields, so they consume.
- **No marker** — a borrow argument (`f(x)`); a plain assignment or `return` (`a = b`, `return b`), which for a non-`Copy` value is *always* a move whose target ownership is visible right there, so a marker would only add noise; a **temporary** argument (`f(build())`, `f(x.clone())`), which has no named binding to invalidate. A redundant `move` on a temporary is rejected.

Invariant: **`move` on an argument ⇔ a named, reusable binding of yours is consumed there.**

#### Receivers (`this`)

A method's receiver uses the same markers, written on `this`:

```promise
type Counter {
  int value;
  current(this) int { return this.value; }            // shared (read-only) borrow of the receiver
  bump(~this) { this.value = this.value + 1; }        // mutable borrow — may mutate fields
  drop(~this) { }                                     // cleanup — also a mutable borrow
}
```

`this` is a shared borrow of the receiver; `~this` is a mutable borrow. A receiver is **never** consumed — a method may mutate `this` but may not move it away whole, even in `drop(~this)`. This is the one asymmetry with parameters, which *can* be declared `move`.

#### Partial moves and captures

A field may be moved out of a value individually. Moving a field leaves the other fields usable (a *partial move*); however a field whose type defines its own `drop()` may **not** be moved out of a still-live aggregate (`cannot move field … — use .clone()`). Closures follow the same rules: `Copy` values are captured by copy, while a non-`Copy` value must be captured with `move`, which transfers ownership into the closure and invalidates the outer binding (see §12).

### 6.3 Lifetimes

The compiler uses **aggressive lifetime elision** — in practice, explicit lifetime annotations are almost never needed. The elision rules:

1. Each reference parameter gets its own lifetime.
2. If there is exactly one input reference, its lifetime is assigned to all output references.
3. If there is a `this` or `~this` receiver, its lifetime is assigned to all output references.
4. If multiple input lifetimes exist and none of the above rules apply, the compiler analyzes the function body to infer the relationship. Only when the body is ambiguous (e.g. conditionally returning one of multiple references) does the compiler require an explicit annotation.

```promise
// All of these are inferred — no annotations needed:
first(string a, string b) string& { return a; }  // inferred: output borrows from a
name(this) string& { return this.name; }          // inferred: output borrows from this

// Rare case: compiler cannot determine which input the output borrows from.
// Explicit annotation required — name a shared lifetime with the `lifetime meta:
longest(string a `lifetime(x), string b `lifetime(x)) string& `lifetime(x) {
  if a.len > b.len { return a; }
  return b;
}
```

**Borrows are statement- and scope-scoped, not last-use.** A borrow created and consumed within a single statement (such as the implicit borrow of a call argument) expires at the end of that statement. A borrow stored in a `T&`/`T~` binding lives until the end of that binding's scope — *not* until its last use — and keeps the value borrowed for that whole region, blocking conflicting moves or mutable borrows there. This is intentionally more conservative than Rust's non-lexical lifetimes; narrow a stored borrow by limiting its binding's scope (e.g. with an inner block).

**Implicit borrow → owned decay is restricted to Copy types.** A `T&` or `T~` is implicitly assignable to a plain owned `T` only when `T` is a Copy type (primitives, value types, references themselves — anything for which `IsCopy(T)` returns true). For non-Copy `T` (string, vectors, heap user types) the decay would silently duplicate ownership of heap data the original owner still holds — a guaranteed double-free at drop time — so it is rejected at every assignment-shaped boundary:

- variable declarations: `string s = a.borrow;`
- field assignment: `obj.field = a.borrow;`
- return statements: `return a.borrow;` from a `string`-returning function
- joined branches (`if`, `match`, parens): when every arm produces a borrow, the joined type stays `T&` and the same boundary rules apply. When arms **mix** borrowed and owned (one arm `T&`, another `T`), the same Copy-only rule applies — silent decay is rejected for non-Copy `T` (the borrow arm's runtime value is the parent's inner pointer, so consuming the joined value as owned `T` would free a pointer the parent still holds). The fix is the same: call `.clone()` on the borrow arm, or change all arms to produce `T&`.

```promise
// Rejected — `if` arms mix `string&` and `string`:
inspect(Ref[string] a, bool cond) {
  string s = if cond { a.borrow } else { "owned" };
  // error: if/match arms mix borrowed and owned non-Copy 'string';
  //        call .clone() on the borrow arm or change all arms to produce 'string&'
}

// Fix (1) — clone the borrow arm:
inspect(Ref[string] a, bool cond) {
  string s = if cond { a.borrow.clone() } else { "owned" };
}

// Fix (2) — make every arm produce a borrow (the joined type stays `string&`):
inspect(Ref[string] a, Ref[string] b, bool cond) {
  string& s = if cond { a.borrow } else { b.borrow };
}
```

To recover an owned value, choose one of:

```promise
// Rejected — implicit `string& → string` decay for a non-Copy type:
get(Ref[string] a) string {
  return a.borrow;          // error: cannot return string& from function returning string
}

// (1) Explicit clone — produces an owned independent copy:
get(Ref[string] a) string {
  return a.borrow.clone();
}

// (2) Keep it as a borrow — declare the local/parameter/return as `T&`:
get(Ref[string] a) string& {
  return a.borrow;
}

// (3) For locals, declare with the borrow type to skip the decay:
inspect(Ref[string] a) {
  string& s = a.borrow;     // OK — no decay, no implicit allocation.
  println(s);
}
```

The Copy carve-out is sound because Copy types are loaded by value at the borrow boundary — the original owner is unaffected:

```promise
n(Ref[int] a) int { return a.borrow; }   // OK: int is Copy.
m(Ref[int] a) {
  int x = a.borrow;                       // OK: int is Copy.
}
```

Rationale: the previous unrestricted decay produced a steady stream of codegen dup-on-read patches around contexts where a borrow leaked into an owned-tracking path (vector index assigns, slice assigns, map index reads, optional fields, etc.). Restricting the decay to Copy at the type level removes the root cause once instead of patching each downstream symptom.

### 6.4 Copy and Clone

`` `copy `` and `` `clone `` are built-in meta annotations that control assignment semantics:

```promise
// Pure value type — automatically `copy, no explicit annotation needed
type Point {
  f64 x `value;
  f64 y `value;
}

// Explicit `copy on a type with instance fields
type Color `copy {
  int r;
  int g;
  int b;
}

// Auto-generated deep clone — compiler generates clone() Self method
type Document `clone {
  string title;
  string[] pages;
}

// Custom clone — just define the method, no meta needed
type Connection {
  Socket socket;

  clone() Self {
    return Connection(socket: this.socket.duplicate());
  }
}
```

- **Pure value types**: Types where ALL fields have `` `value `` placement are automatically `` `copy ``. No heap allocation — all data is embedded directly in the Value struct. Behave like primitives (pass-by-value, no `drop()`). Cannot have `is` parents, non-copy fields, or `drop()` methods.
- `` `copy ``: Bitwise copy on assignment (primitives, small value types). The compiler verifies all fields are themselves `` `copy ``. No method generated — the copy is a direct memory copy.
- `` `clone ``: The compiler auto-generates a `clone() Self` method that deep-copies all fields. If the type also defines an explicit `clone() Self` method, the explicit method takes precedence.
- Types that are `` `copy `` are implicitly copied on assignment. Others are moved.

---

## 7. Error Handling

### 7.1 The `!` Failable Convention

Functions that can fail use `!` after the function name. Under the hood, this desugars to a result struct — a pair of `(value, error)`.

```promise
readFile!(string path) string {
  // On success:
  return contents;

  // On failure:
  raise io.FileNotFoundError(path.clone());
}
```

The `!` after the name means: "this function can fail — it returns `(string, Error)`".

For void failable functions, just use `!` after the name with no return type:

```promise
validate!(string input) {
  if input.is_empty() { raise error("empty input"); }
}
```

### 7.2 Calling Failable Functions

In a **failable function** (name has `!`), a naked call to another failable function **auto-propagates** the error — if the callee fails, the caller immediately returns the error to its own caller. This is the most common case and requires no extra syntax.

Auto-propagation works in **all expression positions** — statements, variable declarations, assignments, and call arguments:

```promise
process!() string {
  string content = readFile("data.txt");    // variable declaration — auto-propagates
  return content.trim();
}

transform!() string {
  return toUpper(readFile("data.txt"));     // call argument — auto-propagates
}

update!() string {
  string content = "";
  content = readFile("data.txt");           // assignment — auto-propagates
  return content;
}
```

When a failable call appears as a call argument, the compiler evaluates the argument, checks the error tag, propagates on failure, and passes the success value to the outer call. Multiple failable arguments are evaluated left-to-right; if any fails, the error propagates immediately and subsequent arguments are not evaluated.

The explicit `^` suffix is allowed for self-documentation but has the same effect:

```promise
  string content = readFile("data.txt")?^;   // same as above — explicit propagation
```

In a **non-failable function**, calling a failable function without handling is a **compile-time error** — there is nowhere to propagate to. The caller must handle the error with `? e { ... }` or unwrap with `!`:

```promise
main() {
  // Handle with ? — block must provide recovery value or diverge (return/panic)
  string content = readFile("data.txt") ? e {
    print_line("Failed: {e.message}");
    return;
  };

  // Handle with ? — error value not needed
  string content = readFile("data.txt") ? {
    return;
  };

  // Panic on error (for prototyping only)
  string content = readFile("data.txt")?!;
}
```

#### Error handler syntax

The `? e { ... }` form handles errors inline. The error binding and type filter are optional:

| Form | Meaning |
|------|---------|
| `expr ? e { ... }` | Handle any error, bind to `e` |
| `expr ? { ... }` | Handle any error, discard error value |
| `expr ? e is IoError { ... }` | Handle only `IoError` (or subtypes), bind to `e` — propagates non-match in `!` function |
| `expr ? _ is IoError { ... }` | Handle only `IoError`, discard binding |
| `expr ? e is IoError { ... } else { ... }` | Handle `IoError`, else catches all other errors |
| `expr ? e is IoError { ... } else e { ... }` | Same with error binding in else clause |
| `expr ? e is IoError { ... }!` | Handle `IoError`, panic on non-matching error |

**Typed handlers** (`? e is T`) perform an RTTI check on the error. In a **failable function**, non-matching errors are **propagated**. In a **non-failable function**, typed handlers must be exhaustive — add an `else { }` clause, a `!` suffix (panic on nomatch), or make the function failable.

The handler block must either produce a **recovery value** of the expected type, or **diverge** (`return`, `break`, `panic`). If it produces a value, that value is used in place of the failed call:

```promise
// Recovery value
string content = readFile("data.txt") ? { "" };    // use empty string on failure

// Diverge
string content = readFile("data.txt") ? e {
  print_line("Error: {e.message}");
  return;
};

// Typed handler — only catches IoError, propagates other errors
process!() string {
  string content = readFile("data.txt") ? e is IoError {
    return "fallback for IO error (code: {e.code})";
  };
  return content;
}

// Typed handler with else — catches IoError specifically, handles all others
handle_all() string {
  string content = readFile("data.txt") ? e is IoError {
    "io fallback";
  } else e {
    return "unexpected: {e.message}";
  };
  return content;
}

// Typed handler with ! — panics on non-matching (explicit opt-in)
handle_or_die() string {
  string content = readFile("data.txt") ? e is IoError {
    "io fallback";
  }!;
  return content;
}
```

#### Capturing the raw result

To inspect both the value and error without propagation, destructure into a tuple:

```promise
(content, err) := readFile("data.txt");
if err is present {
  print_line("Failed: {err.message}");
} else {
  print_line(content);
}
```

#### Summary

| Call form | Behavior | Context |
|-----------|----------|---------|
| `foo()` | Auto-propagate error | `!` function only |
| `bar(foo())` | Auto-propagate error from argument | `!` function only |
| `x = foo()` | Auto-propagate error from assignment | `!` function only |
| `foo()?^` | Explicit propagate (same as bare call) | `!` function only |
| `foo() ? e { ... }` | Handle any error, bind to `e` | Any function |
| `foo() ? { ... }` | Handle any error, discard value | Any function |
| `foo() ? e is T { ... }` | Handle only type `T`, propagate others | `!` function only |
| `foo() ? e is T { ... } else { ... }` | Handle `T`, else catches rest | Any function |
| `foo() ? e is T { ... }!` | Handle `T`, panic on non-match | Any function |
| `foo!()` | Panic on error | Any function |
| `(val, err) := foo()` | Capture raw result | Any function |
| `opt!` | Panic on none (optional unwrap) | Any function |
| `opt ? _ { ... }` | Handle none inline (optional handler) | Any function |

**Critical distinction for AI agents:** `?!` after a failable call **always means "panic on error"** — it does not propagate. `!` is reserved for optional unwrap only. To propagate errors in a failable function, simply call the function with no suffix (bare call). This is the most common pattern and the one AI-generated code should default to:

```promise
// CORRECT — auto-propagation (most common):
wrapper!() string {
  string content = readFile("data.txt");  // bare call → auto-propagates error
  return content;
}

// CORRECT — auto-propagation in call arguments:
wrapper!() string {
  return process(readFile("data.txt"));   // bare call as arg → auto-propagates error
}

// WRONG — panics instead of propagating:
wrapper!() string {
  string content = readFile("data.txt")?!; // ?! → panics on error, does NOT propagate
  return content;
}
```

**Note:** `!` and `? _ { }` also work on `T?` optionals (see Section 14). `?!` targets failable errors; `!` targets optionals. Auto-propagation does not cross lambda boundaries. Inside a non-`!` lambda, failable calls must be handled explicitly with `?^`, `?`, or `?!`.

### 7.3 Error Types

The base `error` type is defined in the standard library with a `message` field:

```promise
type error {
  string message;
}
```

Custom error types inherit from `error` and add domain-specific fields:

```promise
type IoError is error {
  int code;
}

type FileNotFoundError is error {
  string path;
}
```

Error types can be generic:

```promise
type DataError[T] is error {
  T data;
}
```

**Restrictions:**
- Error types **cannot** have `drop()` methods. Error values are passed as raw pointers through the result struct and are not tracked for cleanup. Allowing `drop()` would silently leak resources.
- The `raise` statement validates at compile time that the raised value inherits from `error`.
- Error operators (`?^`, `?!`, `? handler`) validate at compile time that the inner expression is a failable call.

Error inheritance chains work with typed handlers — a handler for a parent type catches all child types:

```promise
type AppError is error { int code; }
type DbError is AppError { string query; }

// Handler for AppError catches DbError too
fail() ? e is AppError { return e.code; };
```

### 7.4 `raise` Statement

`raise` is used to return an error from a `!`-function. It is **not** an exception — it is sugar for returning the error half of the result struct. The raised value must be an `error` or an error subtype.

```promise
divide!(f64 a, f64 b) f64 {
  if b == 0.0 {
    raise error(message: "division by zero");
  }
  return a / b;
}
```

Error construction supports both positional and named arguments:

```promise
raise error("division by zero");                        // positional
raise IoError(message: "disk full", code: 28);          // named
raise IoError("disk full", 28);                         // positional
raise IoError("disk full", code: 28);                   // mixed
```

---

## 8. Meta Annotations (Backtick Attributes)

Backtick `` ` `` provides metadata on declarations. Meta annotations are placed **after** the definition name (and any parameters/return type) and **before** the body or statement terminator.

**Why backtick?** The backtick creates an alternative namespace for compiler directives without polluting the keyword namespace — new meta tags can be added without breaking existing code. The backtick was chosen for its **visual lightness**: it recedes into the background, keeping the reader's focus on the actual code rather than the annotations. Alternatives were considered:

- `@` (Java/Python style) — too visually heavy; meta tags are *modifiers*, not the main event, and `@native` dominates the line far more than `` `native `` does.
- `#` — conflicts with markdown headings in documentation, and with shell comments.
- `$` — conflicts with shell variable expansion; problematic in `promise exec` and bash scripts.
- Non-ASCII characters (e.g., `·` middle dot) — fragile across terminals, SSH, CI systems, and LLM code generation.

The backtick has one known friction point: bash, markdown, and some IDEs treat it as a string delimiter. This is a manageable trade-off — the backtick's visual quietness in source code outweighs the occasional need to escape it in documentation or shell contexts.

### 8.1 Syntax

```
MetaAnnotation = '`' Identifier [ '(' MetaParams ')' ] ;
MetaParams     = MetaParam ( ',' MetaParam )* ;
MetaParam      = Expression | Identifier ':' Expression ;
```

Meta annotations appear in post-definition position:

- **Types**: `type Foo `meta { ... }`
- **Fields**: `string name `meta;`
- **Methods**: `greet() string `meta { ... }`
- **Functions**: `add(int a, int b) int `meta { ... }`
- **Parameters**: `foo(int x `doc("description"), int y `deprecated) { ... }`

### 8.2 Examples

```promise
type OldThing `serializable `version(2) `deprecated(since: "1.3", message: "Use newMethod instead") {
  string name `json(name: "user_name") `required;
  int age `json(name: "user_age");
}

fastAdd(int a, int b) int `inline {
  return a + b;
}

testAddition() `test {
  assert(fastAdd(1, 2) == 3, "fastAdd(1,2) is 3");
}
```

### 8.3 Built-in Metas

| Meta          | Applies To     | Description                                      |
|---------------|----------------|--------------------------------------------------|
| `` `raw ``    | fields         | Field uses an LLVM type identifier directly      |
| `` `value ``  | fields, methods| Place field in Value struct; method receives value as `this` (default for methods) |
| `` `instance ``| methods       | Method receives pointer to Instance struct as `this` |
| `` `variant ``| fields, methods| Place field in Variant struct; method receives variant as `this` |
| `` `type ``   | fields, methods| Place field in Type struct; method is a namespaced function (no `this`) |
| `` `public `` | types, enums, functions | Export from module (see Section 4.4) |
| `` `inline `` | functions      | Hint to inline the function                      |
| `` `deprecated`` | any         | Mark as deprecated with optional message         |
| `` `test ``   | functions      | Mark as a test function                          |
| `` `serializable`` | types     | Auto-generate serialization code                 |
| `` `align(N)``| types, fields  | Memory alignment                                 |
| `` `packed `` | types          | Pack fields without padding                      |
| `` `extern(abi)``| functions   | Foreign function interface                       |
| `` `unsafe `` | functions/blocks| Mark as unsafe code                             |
| `` `abstract ``| methods        | Method has no body; must be implemented by subtypes |
| `` `native `` | methods         | Method has no Promise body; provided by the runtime/compiler backend |
| `` `copy ``  | types           | Bitwise copy on assignment; compiler verifies all fields are also `` `copy `` |
| `` `clone `` | types           | Auto-generate `clone() Self` method (deep copy)   |
| `` `required ``| fields         | Field must be present during deserialization; validation error otherwise |
| `` `final `` | fields          | Immutable after construction; can only be set in `new` or `` `factory `` body (see Section 5.7) |
| `` `factory ``| methods        | Factory constructor with `` `variant `` placement; no `this`, returns `Self` or child (see Section 5.7) |
| `` `doc ``   | any, parameters | AST-attached documentation (see Section 8.4)      |
| `` `target(cond) ``| types, enums, functions | Compile-time platform filtering (see Section 8.5) |
| `` `embed(path) ``| module-level getters | Compile-time resource embedding (see Section 8.6) |

User-defined metas are available through the type system at compile time for meta-programming and code generation.

### 8.4 Documentation (`` `doc ``)

`` `doc `` attaches documentation directly to the AST node. Unlike comments, `` `doc `` is preserved in the parsed tree, unambiguously bound to its declaration, and available to tooling, IDE support, and AI agents at compile time.

```promise
type HttpClient `doc("HTTP client with connection pooling and automatic retry.") {
  int maxRetries `doc("Maximum number of retry attempts before failing.");
  Duration timeout `doc("Per-request timeout.");

  get!(~this, string url `doc("The URL to fetch.")) Response
      `doc("Perform a GET request. Returns the response or an error.") `instance {
    ...
  }
}

divide(f64 a `doc("dividend"), f64 b `doc("divisor"))
    f64! `doc("Divide a by b. Raises MathError on division by zero.") {
  if b == 0.0 {
    raise MathError("division by zero");
  }
  return a / b;
}
```

The parameter is a plain string. Tooling can extract structured sections (parameters, return values, errors) from the text by convention, but the language itself treats it as an opaque string. This keeps the meta simple while giving AI agents a reliable, parseable documentation source that is always in sync with the code structure.

Parameter-level `` `doc `` annotations are placed after the parameter name (and before any default value), providing per-parameter documentation that is structurally bound to each parameter rather than embedded in a free-text block.

### 8.5 Platform Filtering (`` `target ``) {#platform-filtering}

`` `target(cond) `` is a compile-time annotation that includes or excludes a declaration based on the current build target. Only one variant of a filtered pair is compiled per target — the other is invisible to both the type checker and codegen.

```promise
// Exactly one sep() is compiled per platform
sep() `target(linux)   { print_line("linux"); }
sep() `target(!linux)  { print_line("non-linux"); }

// posix = linux || macos
greet() `target(posix)  { print_line("posix"); }
greet() `target(!posix) { print_line("non-posix"); }

// Platform-specific types
type Handle `target(linux)   { int fd; }
type Handle `target(!linux)  { int opaque; }
```

**Supported conditions:**

| Identifier  | Meaning                       |
|-------------|-------------------------------|
| `linux`     | Linux (any triple)            |
| `macos`     | macOS / Darwin                |
| `windows`   | Windows (MSVC ABI)            |
| `wasm`      | WebAssembly (wasm32-wasi)     |
| `posix`     | `linux \|\| macos`            |
| `x86_64`    | x86-64 architecture           |
| `aarch64`   | AArch64 / ARM64               |
| `arm64`     | Alias for `aarch64` (Apple convention) |

Conditions can be combined with `!` (not), `||` (or), and `&&` (and).

**Rules:**
- `` `target `` applies to `type`, `enum`, and `func` declarations. Individual methods cannot be filtered — filter the whole type.
- Without a `` `target `` annotation, a declaration is included on all targets.
- When no explicit `--target` is given, the host platform's triple is used for filtering.
- The `promise doc` command always shows all declarations regardless of target.

This is Promise's only form of platform-specific variation. There are no preprocessor directives, `#ifdef` blocks, or build tags. Platform variants are explicit, structurally separate declarations — visible to the reader and verifiable by the type checker on the appropriate target.

### 8.6 Resource Embedding (`` `embed ``)

`` `embed(path) `` embeds file contents into the compiled binary at compile time. It applies to module-level getters — the compiler reads the file at compile time and generates the getter body. This enables self-contained binaries that bundle assets, templates, schemas, and other resources without runtime file dependencies.

```promise
// Embed a file as a UTF-8 string
get schema string `embed("schema.sql");

// Embed a file as raw bytes
get icon u8[] `embed("icon.png");

// Embed with compression — stored compressed in binary, decompressed on access
get large_dataset string `embed("data.json", compress: true);

// Embed a directory tree as a virtual read-only filesystem
get assets EmbeddedFiles `embed("static/...");
```

The getter has no body — the `` `embed `` annotation tells the compiler to provide the implementation. At the call site, embedded resources are accessed like any other getter (no parentheses). Compressed embeds are transparently decompressed — the caller sees the original data:

```promise
main() {
  print_line(schema);                          // prints the SQL file contents
  print_line("icon bytes: {icon.len}");        // prints byte count
  print_line(large_dataset);                   // decompressed transparently
  print_line("has index: {assets.contains("index.html")}");
}
```

**Supported return types:**

| Type | Behavior |
|------|----------|
| `string` | File contents as UTF-8 string (compile error if not valid UTF-8) |
| `u8[]` | Raw bytes as a byte array |
| `EmbeddedFiles` | Virtual read-only filesystem for directory trees (see `modules/std/embed.pr`) |

**Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| (positional) | `string` | — | Path to the file or directory to embed |
| `compress` | `bool` | `false` | Store compressed in binary, decompress transparently on access |

When `compress: true`, the compiler gzip-compresses the data at compile time. The generated getter decompresses on first access and caches the result — subsequent reads return the cached value with no decompression overhead. This is useful for large text assets (SQL schemas, JSON datasets, HTML templates) where binary size matters more than first-access latency. For `EmbeddedFiles`, compression applies to the concatenated data blob — individual file reads decompress only the requested slice.

**Path rules:**
- Paths are relative to the source file containing the `` `embed `` annotation
- `...` suffix means "embed recursively" (directory trees)
- Absolute paths are a compile error
- Relative paths may reference parent directories (e.g., `"../shared/config.toml"`) — no project-root restriction
- The referenced file or directory must exist at compile time

**Constraints:**
- Only valid on module-level getters (not inside type bodies or functions)
- Embedded getters must not have a body — the compiler generates it
- Embedded file contents are part of the build cache key — if an embedded file changes, the binary is recompiled even if the `.pr` source hasn't changed
- Catalog modules can embed resources (paths relative to the module's source directory)

**Design rationale:** Resource embedding is critical for AI-agent workflows — an LLM can generate a single `.pr` file that compiles to a fully self-contained binary with all required assets. No runtime file dependencies, no deployment scripts, no asset pipelines. Using getters fits Promise's convention that side-effect-free parameterless access uses property syntax (see Section 9.3). The annotation follows the same backtick convention as other meta annotations, keeping the feature discoverable and consistent.

---

## 9. Functions & Methods

Functions are declared without a keyword — the name, parameter list, and optional return type are sufficient to identify a function declaration. This follows the Dart/C++/Java convention.

### 9.1 Free Functions

```promise
greet(string name) string {
  return "Hello, {name}!";
}
```

### 9.2 Methods

Methods are defined inside the type body and correspond to the four struct levels. The method's level determines what `this` refers to and which fields are accessible.

#### Value Methods (default)

By default, `this` is the **value struct**, passed by value. Every method receives the value struct as `this` (see the note under Instance Methods); for a pure value type the value struct embeds the `` `value `` fields directly.

```promise
type Point {
  f64 x `value;
  f64 y `value;

  distanceTo(this, Point other) f64 {
    dx := this.x - other.x;
    dy := this.y - other.y;
    return math.sqrt(dx * dx + dy * dy);
  }
}
```

#### Instance Methods (`` `instance ``)

**Every method — value or instance — receives `this` as the value struct.** The value struct carries the vtable pointer plus: for an instance type, a pointer to the heap instance; for a pure value type, the `` `value `` fields embedded directly (with the instance pointer null or a shared singleton); for a hybrid type with both kinds of field, both. A method accesses **all** of the type's fields uniformly — `` `value `` fields directly in the value struct, instance fields through the instance pointer — so there is **no fundamental difference between pure-value and pure-instance types**; they work the same way. The `` `instance `` annotation marks methods on a type with heap instance state. Use `this` for shared borrow, `~this` for mutable borrow.

```promise
type Counter {
  int value;

  increment(~this) `instance {
    this.value += 1;
  }

  current(this) int `instance {
    return this.value;
  }
}
```

#### Global Methods (`` `global ``)

Global methods are **namespaced functions** — no `this` is passed, no `Self` is available in the body. The type serves purely as a namespace. These replace the `static` keyword from other languages. Cannot be used on generic types (use `` `mono `` instead).

```promise
type Counter {
  int value;

  create(int v) Counter `global {
    return Counter(value: v);
  }

  zero() Counter `global {
    return Counter(value: 0);
  }
}

// Called as:
Counter c = Counter.create(42);
Counter z = Counter.zero();
```

#### Mono Methods (`` `mono ``)

Mono methods are **per-monomorphization namespaced functions**. Like `` `global ``, no `this` is passed, but the type's generic parameters are resolved and `Self` is available in the body. One copy exists per concrete type instantiation.

```promise
type Wrapper[T] {
  T value;

  defaultCount() int `mono {
    return 0;
  }
}

// Called as — each instantiation gets its own copy:
n := Wrapper[int].defaultCount();
m := Wrapper[string].defaultCount();
```

**Note:** The name `new` is reserved for explicit constructors (see Section 5.7). Global/mono methods that create instances should use descriptive names like `create`, `from`, or `empty`. Factory constructors (`` `factory ``) use `` `mono `` (variant) placement instead of `` `global `` — see Section 5.7.

**Future: Data Placement** — `` `global `` and `` `mono `` can also apply to fields, enabling type-scoped or monomorphization-scoped shared data (e.g., `int instanceCount `global` or `string typeName `mono`). This is deferred pending mixed-placement codegen support.

### 9.3 Module-Level Getters and Setters

Functions like `os.get_working_dir()` and `os.set_working_dir("/tmp")` are computed properties of the environment. The call-site parentheses are noise, and the `get_`/`set_` prefixes are boilerplate. Module-level getters and setters extend the same property syntax already used on types (see Section 5.2.1) to file and module scope.

#### Getter Declaration

A module-level getter is declared at the top level of a `.pr` file using the `get` contextual keyword, with no receiver:

```promise
// os.pr — inside the os module
get args string[] `public `doc("Returns the command-line arguments.") {
  return _os_get_args();
}

get working_dir! string `public `doc("Returns the current working directory.") {
  return _os_get_working_dir();
}
```

The syntax mirrors type-level getters but appears at file scope instead of inside a `type { ... }` body. The return type may be failable (`!`) or optional (`?`).

#### Setter Declaration

A module-level setter uses the `set` keyword with a single parameter:

```promise
// os.pr
set working_dir(string path) `public `doc("Changes the current working directory.") {
  _os_set_working_dir(path);
}
```

The setter body receives the assigned value as its parameter. Setters may also be failable.

#### Call-Site Syntax

At the call site, getters are accessed with dot syntax (no parentheses) and setters via assignment:

```promise
use os;

// Getter — reads like a property
string[] args = os.args;
string cwd = os.working_dir!;

// Setter — writes like assignment
os.working_dir = "/tmp";

// Compound assignment — desugars to getter + operator + setter
// os.some_counter += 1  →  os.set_some_counter(os.some_counter + 1)
```

This is identical to how type-level field access works: `animal.name` calls a vtable getter, `animal.name = "Rex"` calls a vtable setter (Section 5.2.1). Module-level getters and setters follow the same principle at module scope.

#### Pairing Rules

Getters and setters are independent declarations — they do not need to be paired:

- **Getter only** → read-only property (e.g., `os.args`). Attempting to assign is a compile error.
- **Setter only** → write-only property. Useful for fire-and-forget configuration.
- **Both** → read-write property (e.g., `os.working_dir`).

#### Visibility

Module-level getters and setters follow the same visibility rules as functions. The `` `public `` annotation exports them to importers. Without it, they are file-private.

#### Relationship to Type-Level Getters

Type-level getters and setters (declared inside `type { ... }`) dispatch through the vtable and receive `this`. Module-level getters and setters have no receiver — they are plain functions with property syntax. The grammar rules are shared; the distinction is whether the declaration appears inside a type body or at file scope.

#### No Module-Level Variables

Promise does **not** support module-level mutable variables (global mutable state). There is no way to declare a variable at file scope — all mutable state must live inside function-scoped locals, type instances, or be threaded explicitly through parameters.

This is a deliberate design decision driven by three of Promise's core goals:

1. **Self-contained readability.** A function that reads or writes a global variable has an invisible dependency on every other function that touches that variable. A reader cannot understand what the function does without tracing all callers and all other mutators — the opposite of "look at one file and know what it does."

2. **Concurrency safety.** With an M:N scheduler running goroutines across OS threads, unsynchronized global mutable state is a data race. Requiring explicit state passing makes sharing intentional and auditable.

3. **AI-agent efficiency.** An LLM generating a Promise program should not need to reason about hidden global state to produce correct code. Every input to a function is visible in its signature.

Module-level getters and setters (Section 9.3) provide property syntax for computed values without requiring mutable file-scope storage. When persistent module-level state is needed, it should be modeled as a type instance passed through the program.

#### No Module Initializers

There are no module-level initializer blocks, `init()` functions, or static constructors. No code runs automatically when a module is imported — code only executes when something explicitly calls it. This eliminates an entire class of ordering bugs (module A's initializer depends on module B's initializer having already run) and makes the program's startup behavior fully predictable from `main()`.

### 9.3a Naming Conventions

#### Full English Words with Approved Exceptions

All public names in the standard library and language APIs must use complete, unabbreviated words — **unless** an approved abbreviation is defined in the dictionary below. This optimizes for AI-agent readability — an LLM can always predict the full word but must memorize each abbreviation. When an abbreviation is approved, it means the short form is so universally recognized that it is *more* readable than the full word.

Examples of the full-word rule: `print_line` not `println`, `make_directory` not `mkdir`, `execute` not `exec`.

#### Approved Abbreviation Dictionary

The following abbreviations are **universally preferred exceptions** to the full-word rule — all identifiers must use the abbreviated form, never the full word. No exceptions.

| Abbreviation | Full word |
|---|---|
| `abs` | `absolute` |
| `arg` | `argument` |
| `attr` | `attribute` |
| `ch` | `character` |
| `config` | `configuration` |
| `dest` | `destination` |
| `dir` | `directory` |
| `env` | `environment` |
| `func` | `function` |
| `id` | `identifier` |
| `info` | `information` |
| `init` | `initialize` |
| `len` | `length` |
| `max` | `maximum` |
| `millis` | `milliseconds` |
| `min` | `minimum` |
| `pos` | `position` |
| `prev` | `previous` |
| `src` | `source` |
| `var` | `variable` |

These abbreviations are so universally recognized that the abbreviated form is more readable than the full word. When a mapping is defined here, the abbreviation is the canonical form — code review should flag uses of the full word.

Human-readable prose (doc strings, error messages, comments) is not subject to this rule — only identifiers (type names, field names, function/method names, parameter names, getter/setter names).

### 9.4 Named Arguments, Defaults & Optional Parameters

#### Definition Syntax

Any parameter can have a **default value** with `= expression`. Parameters whose type uses the `T?` sugar are **optional** — when omitted at the call site, the function receives `none`.

```promise
sendEmail!(
    string to,                    // required
    string subject,               // required
    string body = "",             // has default — skippable
    string? cc,                   // optional — skippable, receives none
    int priority = 3              // has default — skippable
) bool {
  // cc is Option[string] — test with: if cc { ... } (see Section 14.1)
  ...
}
```

There is no ordering constraint on required, defaulted, and optional parameters in the definition — any order is valid. The `this`/`~this` receiver is unaffected: it is never named, never defaulted, and always implicit.

Parameters can carry meta annotations (see Section 8), placed after the parameter name and before any default value:

```promise
connect!(string host `doc("hostname"), int port `doc("port number") = 8080) Connection { ... }
```

**`T?` vs `Option[T]`:** Only the `T?` sugar triggers skippability. If a parameter is declared with `Option[T]` explicitly, it is a required parameter of optional type — the caller must provide it.

#### Call-Site Syntax

At any call site, an argument can be passed by name using `name: expression`, where `name` matches a parameter name from the definition.

**Positional arguments must precede all named arguments.** Once a named argument appears, all subsequent arguments must also be named. Named arguments can appear in any order.

```promise
// All positional
sendEmail("bob@ex.com", "Hi");

// Positional then named (skip optional params)
sendEmail("bob@ex.com", "Hi", priority: 1);

// All named (any order)
sendEmail(to: "bob@ex.com", subject: "Hi", cc: "a@ex.com");
sendEmail(cc: "a@ex.com", subject: "Hi", to: "bob@ex.com");

// ERROR — named before positional:
sendEmail(cc: "a@ex.com", "bob@ex.com", "Hi");

// ERROR — interleaving:
sendEmail("bob@ex.com", cc: "a@ex.com", "Hi");
```

Valid pattern: `[positional...] [named...]` — positional arguments fill parameters left-to-right in declaration order, named arguments fill parameters by name.

#### Argument Matching

The compiler resolves arguments in this order:

1. **Validate structure**: scan the argument list and verify all positional arguments come before any named argument. If a positional argument appears after a named argument, emit a compile error.
2. **Match positional arguments**: fill parameters left-to-right in declaration order. The first positional argument fills the first parameter, the second fills the second, and so on.
3. **Match named arguments**: resolve each named argument by parameter name. Mark each matched parameter as filled. Error on unknown names, duplicate names, or naming a parameter already filled by a positional argument.
4. **Fill defaults and optionals**: for each unfilled parameter — if it has a default value, insert the default expression; if its type is `T?`, insert `none`; otherwise emit a "missing required argument" error.

#### Skipping Parameters

A parameter can be omitted at the call site if it has a default value or its type is `T?`. When using positional arguments, you cannot skip a parameter and provide a later one positionally — you must use named arguments to target specific parameters while skipping others:

```promise
// Skip body and cc, provide priority by name:
sendEmail("bob@ex.com", "Hi", priority: 1);

// Skip body, provide cc by name:
sendEmail("bob@ex.com", "Hi", cc: "alice@ex.com");
```

#### Default Value Evaluation

Default value expressions are evaluated **at the call site** each time the argument is omitted. This avoids the "mutable default" problem — each call gets a fresh evaluation. Default expressions can reference literals, constants, pure function calls, and module-level values. They **cannot** reference sibling parameters from the same function call.

```promise
// VALID defaults:
connect!(string host, int port = 8080) Connection { ... }
createId(string prefix, string id = Uuid.generate()) Thing { ... }

// INVALID — referencing sibling parameter:
range(int start, int end = start + 10) { ... }  // compile error
```

#### Constructor Defaults

Constructor parameters mirror field declarations. Fields with `= expression` defaults become optional constructor parameters (the default is evaluated at the call site each time the argument is omitted). Fields with `T?` type become optional constructor parameters (defaulting to `none`). Fields without defaults and without `T?` type are **required** — omitting them is a compile error.

```promise
type Config {
  string host;           // required — must be provided
  int port = 8080;       // optional — default evaluated at call site
  string? logFile;       // optional — defaults to none
}

Config(host: "localhost");                          // OK: port=8080, logFile=none
Config(host: "localhost", logFile: "/var/log/app"); // OK: port=8080
Config(host: "localhost", port: 9090);              // OK: logFile=none
Config(port: 9090);                                 // ERROR: missing required field 'host'
```

When a type defines an explicit `new` constructor, the implicit constructor is replaced — call site arguments match `new`'s parameter names instead of field names. See Section 5.7 for full constructor design including `new`, failable constructors, factories, `` `final `` fields, and inheritance.

### 9.5 Lambdas / Closures

```promise
add := |int a, int b| -> int { return a + b; };
doubled := list.map(|x| x * 2);
```

Lambdas support default parameter values:

```promise
greet := |string name, string greeting = "Hello"| -> string {
  return "{greeting}, {name}!";
};
greet("Alice");            // greeting uses default
greet("Alice", "Hi");      // greeting = "Hi"
```

Closures capture variables from enclosing scopes. `Copy` types (primitives, refs, types with `copy` meta) are automatically captured by copy. Non-`Copy` types require the `move` keyword, which transfers ownership into the closure:

```promise
string greeting = "hello";
// move required: string is non-Copy
closure := move |string name| -> string {
  return "{greeting}, {name}";
};
// greeting is no longer usable here — it was moved into the closure

int x = 42;
// no move needed: int is Copy
adder := |int y| -> x + y;
// x is still usable here — it was copied into the closure
```

Nested lambdas can capture from grandparent scopes — intermediate lambdas automatically capture forwarded variables:

```promise
int x = 10;
f := |int a| -> int {
  g := |int b| -> x + b;  // x captured through f
  return g(a);
};
```

Named arguments are **not available** when calling through a function-type variable, because function types erase parameter names (see Section 9.6). Named arguments only work when calling a known function or lambda directly.

### 9.6 Function Types

Function types use arrow syntax instead of a keyword:

```promise
(int, int) -> int                  // function taking two ints, returning int
(string&) -> bool                  // function taking a borrowed string, returning bool
() -> ()                           // function taking nothing, returning nothing
```

Function types **erase parameter names and default values**. Only the types and borrow modifiers are part of the function type signature. This means named arguments and default-value skipping are only available when calling a function by its declared name — not when calling through a function-type variable:

```promise
add(int a, int b) int { return a + b; }

add(a: 1, b: 2);          // VALID: calling by name, names available
(int, int) -> int fn = add;
fn(1, 2);                  // VALID: positional through function-type variable
fn(a: 1, b: 2);            // ERROR: function type has no parameter names
```

### 9.7 No Function/Method Overloading

Promise does not support **function or method overloading** — defining multiple functions or methods with the same name but different parameter signatures. Each function name within a scope must be unique.

```promise
// NOT ALLOWED — same name, different signatures:
parse(string s) Document { ... }
parse(Bytes b) Document { ... }   // compile error: 'parse' already defined

// Use distinct names instead:
parseString(string s) Document { ... }
parseBytes(Bytes b) Document { ... }
```

**Rationale:** Overloading interacts poorly with type inference (`:=`), first-class functions (which overload does `callback = parse` refer to?), and error-type resolution. Default parameters and optional parameters (`T?`) already cover the common "fewer arguments" use case, while generics and interfaces handle the "different types" case. Keeping dispatch unambiguous simplifies both the compiler and the developer's mental model.

Note that **operator overloading** (defining `+`, `==`, etc. as methods inside a type body) is supported — see Section 5.7. This is not name-based overloading; each operator symbol has exactly one definition per type.

---

## 10. Control Flow

### 10.1 If / Else

```promise
if x > 0 {
  // ...
} else if x == 0 {
  // ...
} else {
  // ...
}

// If as expression
abs := if x >= 0 { x } else { -x };
```

### 10.2 Match (Pattern Matching)

`match` tests a value against a series of patterns. Arms are checked top-to-bottom; the first match executes. The compiler verifies **exhaustiveness** — all possible cases must be covered (use `_` as a catch-all).

#### Literal patterns

```promise
label := match status {
  200 => "OK",
  404 => "Not Found",
  _ => "Unknown",
};
```

#### Enum destructuring

```promise
match color {
  Color.Red => print_line("red"),
  Color.Green => print_line("green"),
  Color.Custom(r, g, b) => print_line("rgb({r},{g},{b})"),
  _ => print_line("other"),
}
```

#### Type patterns

Match by runtime type. The binding variable is scoped to the arm:

```promise
match shape {
  Circle c => math.pi * c.radius * c.radius,
  Rect r => r.width * r.height,
  _ => 0.0,
}
```

A type pattern without binding just checks the type:

```promise
match animal {
  Dog => print_line("it's a dog"),
  Cat => print_line("it's a cat"),
  _ => print_line("something else"),
}
```

#### Guards

Add `if` after a pattern for an additional condition:

```promise
match n {
  x if x > 0 => "positive",
  0 => "zero",
  _ => "negative",
}
```

Guards can combine with type patterns:

```promise
match animal {
  Dog d if d.age > 10 => "old dog",
  Dog d => "young dog",
  Cat => "cat",
  _ => "other",
}
```

#### Match rules

- **Exhaustive**: every possible value must be covered. Omitting `_` when cases are incomplete is a compile error.
- **First match wins**: arms are checked top-to-bottom.
- **Expression or statement**: `match` can appear as an expression (returns a value) or a statement. When used as an expression, all arms must produce the same type.
- **Bindings are scoped**: a binding like `d` in `Dog d =>` is only valid in that arm's body.
- **No fallthrough**: each arm is independent. There is no implicit fallthrough between arms.

### 10.3 Type Checking with `is` and Casting with `as`

#### `is` — runtime type check with narrowing

The `is` keyword tests whether a value is an instance of a given type. Inside the `if` block, the compiler **narrows** the variable to the checked type:

```promise
Animal animal = getDog();

if animal is Dog {
  animal.bark();              // animal is narrowed to Dog here
}
```

This follows the same narrowing pattern as optional truthiness (`if cc { ... }` narrows `T?` to `T`). The `else` branch does **not** narrow — `animal` remains `Animal`.

`is` works in any boolean context:

```promise
if animal is Dog && animal.age > 5 {
  // animal is Dog here — narrowed before the && guard
}

bool isDog = animal is Dog;   // no narrowing — just a bool test
```

#### `as` — type casting

`as` performs a safe cast, returning an optional. `as!` performs an unsafe cast that panics on failure:

```promise
Dog? dog = animal as Dog;     // safe — returns none if animal is not a Dog
Dog dog = animal as! Dog;     // unsafe — panics if animal is not a Dog
```

`as` composes with optional chaining:

```promise
animal as Dog ?: defaultDog;            // cast or default
(animal as Dog)?.bark();                // cast and chain
```

#### `is` keyword disambiguation

The `is` keyword appears in three contexts. The parser disambiguates by syntactic position:

| Context | Example | How it's recognized |
|---------|---------|---------------------|
| Type declaration | `type Dog is Animal` | After `type IDENT` |
| Optional pattern | `x is present` / `x is absent` | `present`/`absent` are contextual keywords after `is` |
| Type check | `animal is Dog` | Expression context, IDENT after `is` is a type name |

These never conflict — type declarations and expression patterns occupy different syntactic positions.

### 10.4 Loops

```promise
// While
while condition {
  // ...
}

// While with unwrap binding — loops while the expression is not none,
// binding the unwrapped value each iteration (see Section 14.1)
while item := optionalExpr {
  // item is the unwrapped T, not T?
}

// For-in — iterates any stream[T] (see Section 12)
for item in collection {
  // ...
}

// For-in with index — desugars to enumerate() (see Section 12.2)
for i, item in collection {
  // ...
}

// For-in with range
for i in 0..10 {
  // i takes values 0, 1, 2, ..., 9 (exclusive end)
}

for i in 0..=10 {
  // i takes values 0, 1, 2, ..., 10 (inclusive end)
}

// Infinite loop (no condition — Go style)
for {
  if done { break; }
}

// Classic for
for int i = 0; i < 10; i += 1 {
  // ...
}
```

The `for item in expr` loop is **structural** — it works on any value that can yield an iterator. If the value is a `Stream[T]` (the *iterable* interface — has `iter() Iterator[T]`), the loop calls `expr.iter()` to obtain the cursor; if it is already an `Iterator[T]` (has `next() T?`), it iterates directly. Either way it then loops calling `next()` until `none`. (Built-in ranges are iterated via a built-in fast path.) See Section 12 for the full iteration protocol.

---

## 11. strings & Interpolation

strings are UTF-8 encoded, owned, and heap-allocated.

```promise
string name = "world";
string msg = "hello, {name}!";           // string interpolation with {}
string literal_brace = "json: \{key}";   // \{ escapes to literal { (no interpolation)
string raw = r"no \n escape here";        // raw string
string multi = """
  multi-line
  string literal
""";
```

**Escape sequences:** `\n` (newline), `\t` (tab), `\r` (carriage return), `\b` (backspace), `\\` (backslash), `\"` (double quote), `\0` (null), `\{` (literal `{` — suppresses interpolation). Raw strings (`r"..."`) do not process escape sequences.

string slices (`&str` equivalent) use `string&` for borrowed string data.

---

## 12. Streams, Ranges & Generators

Promise provides a unified iteration and streaming abstraction through two core interfaces: `Stream[T]` (a reusable factory that produces cursors) and `Iterator[T]` (a single-pass cursor that yields elements one at a time). Lowercase forms (`stream[T]`, `iter[T]`) are syntactic sugar. Because Promise has no function coloring and uses goroutine-based transparent I/O, a single `Stream[T]` type handles both synchronous and asynchronous data sources — a generator that performs I/O simply suspends its goroutine during the blocking operation, with no change to its type signature.

### 12.1 Core Interfaces

```promise
type Iterator[T] `structural {
  next() T? `abstract;

  // Intermediate combinators — lazy, return a new iterator
  map[R]((T) -> R transform) Iterator[R] { ... }
  filter((T) -> bool predicate) Iterator[T] { ... }
  flat_map[R]((T) -> Iterator[R] transform) Iterator[R] { ... }
  take(int count) Iterator[T] { ... }
  skip(int count) Iterator[T] { ... }
  take_while((T) -> bool predicate) Iterator[T] { ... }
  skip_while((T) -> bool predicate) Iterator[T] { ... }
  zip[U](Iterator[U] other) Iterator[(T, U)] { ... }
  chain(Iterator[T] other) Iterator[T] { ... }
  enumerate() Iterator[(int, T)] { ... }

  // Terminal operations — eager, consume the iterator
  fold[R](R initial, (R, T) -> R accumulate) R { ... }
  reduce((T, T) -> T combine) T? { ... }
  collect() T[] { ... }
  count() int { ... }
  any((T) -> bool predicate) bool { ... }
  every((T) -> bool predicate) bool { ... }
  first() T? { ... }
  last() T? { ... }
  find((T) -> bool predicate) T? { ... }
  for_each((T) -> void action) { ... }
}
```

`Iterator[T]` is the stateful cursor and carries every combinator as a default method. Each call to `next()` returns the next element wrapped in `T?`, or `none` when the sequence is exhausted. This combines the traditional `hasNext()` + `next()` two-method pattern into a single call, leveraging Promise's optional type system. Any type with a `next() T?` method satisfies the structural interface and inherits all the combinators.

```promise
type Stream[T] `structural {
  iter() Iterator[T] `abstract;
}
```

`Stream[T]` is the reusable factory: a single abstract `iter()` that hands out a fresh `Iterator[T]` cursor. Combinators live on `Iterator[T]`, so transform a stream by chaining off `stream.iter()`.

> **Status**: The core interfaces (`Iterator[T]` with `next()`, `Stream[T]` with `iter()`) and all ~20 combinator default methods on `Iterator[T]` are implemented and usable on any type that satisfies them. Combinators not in the list above (`scan`, `chunk`, `distinct`, `contains`, `min`, `max`, `join`) are design targets, not yet implemented.

**Key design properties:**

- **`Stream[T]` is a factory.** Each call to `iter()` produces a fresh, independent cursor. This means streams are reusable — you can iterate the same stream multiple times and get the same elements each time (for deterministic sources).
- **`Iterator[T]` is a single-pass cursor.** Once `next()` returns `none`, the iterator is exhausted. There is no `reset()`.
- **Intermediate operations are lazy.** Calling `stream.map(fn)` does not execute `fn` — it returns a new `stream[T]` that applies `fn` on demand when iterated. Multiple intermediate operations compose into a single pass over the data.
- **Terminal operations are eager.** Calling `stream.collect()` or `stream.count()` consumes the stream and produces a result.
- **Constraint-dependent combinators**: `distinct()` requires `T: Equal`. `min()` and `max()` require `T: Ordered`. These constraints are enforced at the call site via generic bounds.

### 12.2 For-in Desugaring

The `for item in expr` loop desugars into `Stream[T]` and `Iterator[T]` operations:

```promise
for item in stream { body }

// Desugars to:
{
  Iterator[T] _iter = stream.iter();
  while item := _iter.next() {
    body
  }
}
```

This uses `while ... :=` unwrap binding (see Section 14.1): the loop continues as long as `_iter.next()` returns a value, binding the unwrapped element to `item` each iteration.

The indexed form desugars through `enumerate()`:

```promise
for i, item in collection { body }

// Desugars to:
for (i, item) in collection.enumerate() { body }
```

### 12.3 Ranges

The `..` operator constructs a `range` value. `..` produces a half-open (exclusive end) range; `..=` produces an inclusive range:

```promise
0..10       // 0, 1, 2, ..., 9     (exclusive end — half-open)
0..=10      // 0, 1, 2, ..., 10    (inclusive end)
1..1        // empty range
5..=5       // single element: 5
```

`Range[T: Ordered]` is a small **value type** that backs the `..`/`..=` operators (lowercase `range` is sugar). It carries `start`/`end`/`inclusive` and answers two queries; **iteration is built into the language, not a method on the type**:

```promise
type Range[T: Ordered] {
  T start `value;
  T end `value;
  bool inclusive `value;

  contains(T value) bool { ... }   // O(1) membership test
  get is_empty bool { ... }
}
```

A range is **iterable** — `for x in 0..10 { … }` and `yield * 1..=3` work because the compiler lowers a range loop directly to a counting loop. But a `Range` is **not** a `Stream` and has **no `iter()` or combinator methods** (`.filter` / `.map` / `.fold`). To transform a range's values, iterate it into a collection first, then use the `Iterator` combinators (§13).

### 12.4 Generator Functions

A function whose return type is `stream[T]` (or `Stream[T]`) and whose body contains `yield` is a **generator function**. The compiler transforms its body into a coroutine via LLVM `presplitcoroutine` intrinsics that implements `Iterator[T]`. No special modifier keyword is needed — the return type plus the presence of `yield` is sufficient.

```promise
fibonacci() stream[int] {
  int a = 0;
  int b = 1;
  for {
    yield a;
    int temp = a;
    a = b;
    b = temp + b;
  }
}

// Consuming:
for n in fibonacci() {
  if n > 100 { break; }
  print_line("{n}");
}
```

**`yield*` delegates to another iterable** (stream, range, array, vector, string, or iterator), yielding all of its elements inline:

```promise
oneThenTwo() stream[int] {
  yield* 1..=3;     // yields 1, 2, 3
  yield* 7..=9;     // yields 7, 8, 9
}
```

**Generators with I/O** work transparently — no function coloring needed:

```promise
fetchPages(string url) stream[Page] {
  string? nextUrl = url;
  while nextUrl {
    Page page = http.get(nextUrl) ? { break; };  // stop stream on error
    yield page;
    nextUrl = page.nextLink;
  }
}
```

**Restrictions:** `yield` must appear directly in the generator function body — it cannot appear inside a nested closure or lambda. Use a `for` loop instead:

```promise
// ERROR — yield inside a closure
example() stream[int] {
  items.iter().for_each(|item| { yield item; });  // compile error
}

// OK — yield in a for loop
example() stream[int] {
  for item in items { yield item; }
}
```

The compiler transforms each `yield` point into a state in a state machine. Local variables are captured as fields of the generated type. The `next()` method resumes execution from the last yield point and runs until the next `yield` or until the function body completes (returning `none`). This transformation is purely a compiler concern — generators look and feel like ordinary functions to the caller.

### 12.5 Collections as Streams

Built-in collection types implement `Stream[T]`, supporting `for-in` iteration. The combinators live on `Iterator[T]`, so reach them via `.iter().<combinator>()` — e.g. `v.iter().filter(...).collect()`:

| Collection      | Stream Implementation           |
|----------------|---------------------------------|
| `T[]` (Vector)  | `Stream[T]` — iterates elements in order |
| `T[N]` (array)  | `Stream[T]` — iterates elements in order |
| `map[K, V]`     | `Stream[(K, V)]` — iterates key-value pairs |
| `channel[T]`    | `Stream[T]` — receives from channel until closed |
| `range`         | iterable via a built-in counting loop — not a `Stream`; see Section 12.3 |
| `string`        | `Stream[char]` — iterates Unicode scalar values |

`map[K, V]` also provides `.keys() Stream[K]` and `.values() Stream[V]` for iterating only keys or values.

### 12.6 Channels as Streams

A `channel[T]` is iterable: `ch.iter()` returns a `stream[T]`, and from there the standard stream combinators apply (the combinators live on the stream, not the channel itself):

```promise
ch := channel[int](capacity: 10);
go {
  for i in 0..100 { ch.send(i); }
  ch.close();
};

// Use stream combinators on channel
evens := ch.iter().filter(|int n| -> n % 2 == 0);
for n in evens {
  print_line("{n}");
}
```

**Important difference from other streams:** channel iteration is **destructive**. Values are consumed from the shared buffer — there is no way to re-iterate a channel. Calling `iter()` on a channel returns an iterator that receives from the channel; `next()` blocks (suspends the goroutine) until a value is available or the channel is closed. When the channel is closed and empty, `next()` returns `none`.

Calling `ch.iter()` multiple times does **not** produce independent cursors over the same data — each iterator draws from the same underlying channel buffer. This is the expected behavior for a concurrent data-passing primitive.

---

## 13. Collections

Built-in collection types with generic support:

```promise
// Array (fixed-size, stack-allocated)
int[3] arr = [1, 2, 3];

// Vector (dynamic, heap-allocated) — T[] is sugar for Vector[T]
int[] list = [1, 2, 3, 4, 5];
list.push(6);

// Map (hash map with open addressing)
map[string, int] scores = {
  "alice": 100,
  "bob": 85,
};

// Tuple
(int, string) pair = (42, "answer");
(num, label) := pair;      // destructuring
```

All collection types implement `stream[T]` (see Section 12.5), providing lazy combinators like `map`, `filter`, `fold`, and more:

```promise
int[] numbers = [1, 2, 3, 4, 5];
squares := numbers.iter().map[int](|int n| -> n * n).collect();   // [1, 4, 9, 16, 25]
sum := numbers.fold(0, (acc, n) -> acc + n);       // 15
```

### 13.1 Vector (`T[]`)

`T[]` is syntactic sugar for `Vector[T]`, a generic dynamic array backed by a contiguous heap-allocated buffer. Vectors use a doubling growth strategy (0 → 4 → 8 → 16 → ...) and handle millions of elements efficiently.

The `T[]` syntax works in both **type annotation** and **expression** positions. In expression position, it is equivalent to `Vector[T]`, enabling constructor calls and factory method access:

```promise
// Type annotation position
int[] v = [];              // empty vector
int[] v = [1, 2, 3];      // initialized with elements

// Expression position — constructor calls
v := int[]();              // equivalent to Vector[int]()
v := int[](capacity: 1000);  // pre-allocate capacity
v := int[][]();            // nested: Vector[Vector[int]]()

// Expression position — factory methods
v := int[].filled(value: 0, count: 100);
v := string[].filled(value: "x", count: 3);
```

Only type names are valid before `[]` — using a variable or literal (e.g. `x[]()`, `42[]()`) is a compile error.

**Methods and properties:**

| Signature | Description |
|-----------|-------------|
| `get len int` | Number of elements |
| `get is_empty bool` | True when `len == 0` |
| `push(T elem)` | Append element, growing if needed |
| `pop() T?` | Remove and return last element, or `none` if empty |
| `contains(T elem) bool` | Linear search for element equality |
| `remove(int index)` | Remove element at index, shifting subsequent elements left |
| `[](int index) T` | Index access (panics on out-of-bounds) |
| `[]=(int index, T value)` | Index assignment (panics on out-of-bounds) |
| `[:](int? start, int? end) T[]` | Slice — returns a new vector from `start` to `end` (exclusive) |
| `[:]=(int? start, int? end, T[] value)` | Slice assignment — overwrites range with values from `value` |
| `filled(T value, int count) T[]` | Factory: creates a vector of `count` copies of `value` |

```promise
int[] v = [];
for i in 0..5 { v.push(i * i); }     // [0, 1, 4, 9, 16]
v.remove(2);                           // [0, 1, 9, 16]

if val := v.pop() {
    print_line("{val}");                  // 16
}

assert(v.contains(9), "vector contains 9");   // true
v[0] = 42;                            // [42, 1, 9]

sub := v[1:3];                         // [1, 9]
```

Vectors support `for-in` iteration:

```promise
int[] nums = [10, 20, 30];
for n in nums { print_line("{n}"); }          // value iteration
for i, n in nums { print_line("{i}: {n}"); }  // indexed iteration
```

### 13.2 Map (`map[K, V]`)

`map[K, V]` is a generic hash map using open addressing with linear probing. Keys must satisfy the `Hashable + Equal` constraints.

```promise
map[string, int] scores = {"alice": 100, "bob": 85};
scores["charlie"] = 92;
```

**Methods and properties:**

| Signature | Description |
|-----------|-------------|
| `get len int` | Number of entries |
| `get is_empty bool` | True when `len == 0` |
| `[](K key) V?` | Lookup by key, returns `none` if not found |
| `[]=(K key, V value)` | Insert or overwrite entry |
| `contains(K key) bool` | True if key exists |
| `remove(K key) bool` | Remove entry, returns true if found |
| `get_or(K key, V default) V` | Lookup with fallback value |
| `pop(K key) V?` | Remove and return value, or `none` |
| `update(K key, V value) bool` | Update existing entry only, returns true if found |
| `keys() K[]` | All keys as a vector |
| `values() V[]` | All values as a vector |
| `entries() (K, V)[]` | All entries as a vector of tuples |
| `merge(Map[K, V] other)` | Copy all entries from `other` into this map |
| `clear()` | Remove all entries |

```promise
map[string, int] m = {:};
m["x"] = 10;
m["y"] = 20;

if val := m["x"] {
    print_line("{val}");            // 10
}

name := m.get_or("z", 0);       // 0
m.remove("x");                   // true
keys := m.keys();                // ["y"]
```

### 13.3 Tuple

Tuples are fixed-size heterogeneous collections with positional access and destructuring:

```promise
(int, string) pair = (42, "answer");
(num, label) := pair;              // destructuring
```

---

## 14. Nullable Types

Promise does not have null. Optional values use `Option[T]`, with sugar `T?`:

```promise
find(int id) User? {            // shorthand for Option[User]
  // ...
  return none;                   // Option.None
}
```

A value of type `T` is **implicitly convertible** to `T?`. No wrapping syntax is needed:

```promise
string name = "Alice";
string? maybeName = name;       // OK — implicit T → T?
```

### 14.1 Working with Optionals

There are three ways to test and unwrap optional values, from lightest to most explicit.

#### Truthiness narrowing

When a `T?` value appears as an `if` condition, it is treated as a presence check. Inside the block, the compiler **narrows** the type from `T?` to `T`:

```promise
string? cc = getCC();

if cc {
  print_line(cc);              // cc is string here, not string?
}

if !cc {
  print_line("no value");
  print_line(cc);               // ERROR: cc is known to be none here
}
```

Inside the `if cc` block, `cc` is narrowed to `T`. Inside the `if !cc` block, `cc` is known to be `none` — any use of `cc` as type `T` is a **compile-time error**.

This works for any `T?` where `T` is not `bool`. For `bool?`, the compiler emits an error because the intent is ambiguous — use `is present` instead (see below).

#### `is present` / `is absent`

For explicit presence testing that works with **any** `T?` — including `bool?` — use the `is present` and `is absent` patterns. These are contextual keywords: `present` and `absent` are only special after `is` in pattern position; in all other contexts they are normal identifiers.

```promise
bool? verbose = getFlag();

if verbose is present {
  // verbose is bool here — narrowed from bool?
  if verbose { enableLogging(); }
} else {
  print_line(verbose);           // ERROR: verbose is known to be none here
}

if verbose is absent {
  print_line("no flag provided");
  print_line(verbose);           // ERROR: verbose is known to be none here
}
```

As with truthiness narrowing, the inverse blocks enforce negative narrowing: inside `is absent` or the `else` of `is present`, the variable is known to be `none` and any use of it as type `T` is a **compile-time error**.

**Post-divergence narrowing:** When `is absent` (or `!cc`) guards a block that **always diverges** (return, raise, break), the compiler narrows the variable to `T` in all subsequent code — the absent path can never reach it:

```promise
int? x = find(id);
if x is absent {
  raise error(message: "not found");  // diverges
}
// x is T here — narrowed for all subsequent code
int y = x + 1;
```

This also works with `!cc`:

```promise
if !x { return; }
int y = x;           // x is T — guaranteed present
```

Post-divergence narrowing requires: (1) the condition is `is absent` or `!cc`, (2) the then-body diverges on all paths, and (3) there is no `else` clause.

`is present` and `is absent` extend the existing `is` pattern matching keyword (see Section 10.3 for the full `is` keyword disambiguation table). They cannot collide with type names — `present` and `absent` are contextual keywords recognized only after `is` in pattern position.

#### Unwrap binding with `:=`

When you want to unwrap into a **new name**, use `:=` inside an `if` condition:

```promise
User? user = find(42);

if u := user {
  print_line(u.name);          // u is User — unwrapped
}
```

If the right side is `none`, the condition is false. If it holds a value, the left side is bound to the unwrapped value and the block executes. This is useful when you want a shorter or more descriptive name than the original variable.

The `:=` unwrap binding also works in `while` conditions, looping as long as the expression produces a value:

```promise
while item := iter.next() {
  process(item);              // item is T, not T?
}
```

This is the mechanism underlying `for-in` loop desugaring (see Section 12.2).

### 14.2 Force Unwrap (`!`)

The `!` operator on an optional extracts the inner value, panicking at runtime if the optional is `none`. This is symmetric with failable `!` (which panics on error):

```promise
int? x = 42;
int y = x!;           // y is 42

int? z = none;
int w = z!;           // panic: unwrap failed: optional is none
```

Force unwrap is useful in contexts where you know a value is present but the type system hasn't narrowed it:

```promise
int? x = find(42);
int y = x! + 1;                 // use in expressions
string s = "{x!}";              // use in interpolation
process(x!);                    // use as function argument
```

The `as!` operator also works for optional unwrapping: `x as! T` where `x` is `T?` extracts `T` and panics on none. This is equivalent to `x!` but uses cast syntax.

### 14.3 Optional Handler (`? _ { }`)

The `?` operator on an optional handles the `none` case inline, mirroring the error handler syntax. The handler block provides a recovery value or diverges:

```promise
int? x = none;

// Recovery value — handler provides fallback
int y = x ? _ { 0; };                        // y is 0

// Diverge — handler exits the function
int z = x ? _ { return; };                   // never reaches z
```

The `_` binding is required to disambiguate from error propagation (`x?`). Optional handlers do not support typed patterns or `else` clauses — use `is present`/`is absent` for more complex control flow.

**Interaction with failable functions:** When a failable function returns an optional (`int?!`), `?` and `!` compose naturally. The first operator targets the failable layer, the second targets the optional:

```promise
fetch!() int? { ... }

// Chain: error handler then optional handler
int result = fetch() ? e { 0; } ? _ { -1; };

// Chain: error unwrap then optional unwrap
int result = fetch()?!!;
```

The disambiguation rule by operator:

- **Error layer** (act on a failable `!` result): `?^` propagates the error to the caller · `?!` panics on error · `? e { }` handles the error with a block.
- **Optional layer** (act on an `Optional[T]`): `!` force-unwraps (panics on `none`) · `? _ { }` handles `none` with a block · `?:` supplies a default.

The failable layer is always consumed first. When a value is `int?!` (failable returning optional), an error-layer operator binds before an optional-layer one — so `fetch()?!!` reads as `?!` (panic on error) then `!` (unwrap the optional). A bare `!` or `? _ { }` applied to a plain `Optional[T]` acts on the optional layer.

### 14.4 Other Optional Operations

```promise
// `?.` chaining — short-circuits to none if the receiver is absent
name := find(42)?.name;

// `?:` default operator — returns the right side if the left is none
name := find(42)?.name ?: "unknown";

// match for complex enum patterns
match result {
  Ok(value) => process(value),
  Err(e) => log(e),
}
```

### 14.5 Optional Parameters

When `T?` is used as a **function/method parameter type**, the parameter is implicitly optional — the caller may omit it, and the function receives `none` (see Section 9.4). To declare a required parameter of type `Option[T]`, use `Option[T]` explicitly instead of the `T?` sugar. For how `T?` interacts with stream iteration, see Section 12.

---

## 15. Unsafe Code

Promise allows unsafe blocks for low-level operations:

```promise
rawPointer() `unsafe {
  ptr := unsafe {
    int* raw = alloc[int]();
    *raw = 42;
    raw
  };
}
```

---

## 16. Resource Management

Promise provides deterministic, ownership-driven resource cleanup without garbage collector finalizers. Two mechanisms work together: **`use` bindings** for scoped resource lifetime, and **`drop()` methods** for general cleanup when an owner goes out of scope.

### 16.1 I/O Interfaces

The standard library defines a set of structural interfaces for I/O, following the Go model of small, composable abstractions:

```promise
type Reader `structural {
  read!(~this, u8[]~ buf) int `abstract `instance;
}

type Writer `structural {
  write!(~this, u8[] buf) int `abstract `instance;
}

type Closer `structural {
  close!(~this) `abstract `instance;
}

type Seeker `structural {
  seek!(~this, int offset, int whence) int `abstract `instance;
}
```

Compound interfaces compose these via inheritance:

```promise
type ReadCloser `structural is Reader, Closer {}
type WriteCloser `structural is Writer, Closer {}
type ReadWriter `structural is Reader, Writer {}
type ReadWriteCloser `structural is Reader, Writer, Closer {}
```

Because these are `` `structural ``, any type with matching method signatures satisfies them automatically — no explicit `is` declaration needed. A `File` type satisfies `ReadWriteCloser` by having `read`, `write`, and `close` methods with the right signatures:

```promise
type File is ReadWriteCloser {
  int fd;

  open!(string path, bool readonly = false) Self `factory `native;

  read!(~this, u8[]~ buf) int `instance `native;
  write!(~this, u8[] buf) int `instance `native;
  close!(~this) `instance `native;
  seek!(~this, int offset, int whence) int `instance `native;
}
```

User-defined types participate naturally:

```promise
type BufferedWriter is Writer {
  Writer inner;
  u8[] buf;

  write!(~this, u8[] data) int `instance {
    for b in data { this.buf.push(b); }
    if this.buf.len >= 4096 {
      return this.flush()?^;
    }
    return data.len;
  }

  flush!(~this) int `instance {
    n := this.inner.write(this.buf)?^;
    this.buf = [];
    return n;
  }
}
```

### 16.2 `use` Bindings

A `use` binding ties a resource's lifetime to the enclosing scope. When the scope exits — whether by normal fall-through, `return`, `raise`, `break`, or `continue` — the compiler automatically calls `close()` on the bound variable.

```promise
main!() {
  use f := File.open("data.txt", readonly: true)?^;
  string data = f.read_all()?^;
  // f.close() called automatically here
}
```

`use` works in any block scope:

```promise
process!(string path) {
  use f := File.open(path, readonly: true)?^;

  if needsBackup(path) {
    use backup := File.create(path + ".bak")?^;
    copyTo(f, backup)?^;
    // backup.close() called here
  }

  // f.close() called here
}
```

**Rules:**

- The variable's type must have a `close()` method (checked via structural or nominal satisfaction of `Closer`)
- Multiple `use` bindings in the same scope are closed in **reverse declaration order** (LIFO), matching the intuition that resources opened later depend on resources opened earlier
- A `use` variable cannot be moved out of its scope — the compiler must guarantee `close()` is callable
- `use` is a variable declaration modifier, not a statement — it binds a name and a value

**Close error handling:** If the enclosing function is failable (`!` return type), close errors propagate — but only if no other error is already in flight. If the scope is exiting due to a `raise` or error propagation, the close error is suppressed to preserve the original error. If the scope exits normally and `close()` fails, the close error becomes the function's error.

```promise
// In a failable function:
writeData!(string path, u8[] data) {
  use f := File.create(path)?^;
  f.write(data)?^;
  // If f.close() fails here, the error propagates (no prior error)
}

// If write fails:
writeData!(string path, u8[] data) {
  use f := File.create(path)?^;
  f.write(data)?^;  // raises an error
  // f.close() still called, but its error is suppressed — write's error propagates
}
```

In a non-failable function, close errors are silently suppressed (there is nowhere to propagate them).

### 16.3 The `drop()` Method

Any type can define a `drop()` method for general-purpose cleanup. The compiler inserts `drop()` calls when a value's owner goes out of scope and the value has not been moved.

```promise
type TempFile {
  string path;
  File file;

  drop(~this) `instance {
    this.file.close();
    fs.remove(this.path);
  }
}
```

**Drop rules:**

1. When a variable goes out of scope and its type has a `drop()` method, the compiler inserts a `drop()` call
2. If the value was **moved** (ownership transferred), no drop occurs — the new owner is responsible
3. Fields are dropped recursively **after** the parent's `drop()` runs, in reverse declaration order
4. `` `copy `` types cannot define `drop()` — copy semantics are incompatible with unique cleanup (a value that is bitwise-copied would have its `drop()` called multiple times)
5. Drop order within a scope follows reverse declaration order (LIFO), consistent with `use` bindings
6. `drop()` methods must not fail — the signature is `drop(~this)` with no `!` return

**Conditional drops:** When ownership depends on control flow, the compiler inserts **drop flags** — boolean variables that track whether a value still needs dropping:

```promise
transfer(bool condition) {
  Resource r = Resource(id: 0);
  if condition {
    consume(move r);  // r is moved — no drop needed
  }
  // Compiler: if !moved_r { r.drop(); }
}
```

### 16.4 Interaction Between `use` and `drop()`

`use` and `drop()` are complementary:

- `use` calls `close()` at scope exit — for explicit, scoped resource management
- `drop()` calls cleanup code at scope exit — for automatic, ownership-driven cleanup
- A type can have both: `use` calls `close()` explicitly, and `drop()` handles cleanup for non-`use` variables

If a variable is declared with `use`, the compiler calls `close()` at scope exit. If that same type also has a `drop()` method, `drop()` is **not** called — `use` takes precedence. The expectation is that `close()` performs all necessary cleanup (or that `drop()` delegates to `close()` internally).

For variables **not** declared with `use`, normal `drop()` semantics apply — the compiler calls `drop()` when the owner goes out of scope.

```promise
type Connection {
  int socket_fd;

  close!(~this) `instance {
    syscall.close(this.socket_fd)?^;
  }

  drop(~this) `instance {
    // Best-effort close — errors suppressed in drop
    syscall.close(this.socket_fd);
  }
}

main!() {
  // Explicit scoped lifetime — close() called, errors can propagate
  use conn := Connection.connect("localhost:5432")?^;
  conn.query("SELECT 1")?^;
  // conn.close() called here — error propagates if function is !

  // Without use — drop() called at scope exit, errors suppressed
  conn2 := Connection.connect("localhost:5432")?^;
  conn2.query("SELECT 1")?^;
  // conn2.drop() called here — best-effort cleanup
}
```

### 16.5 Summary

| Mechanism | Trigger | Method Called | Error Handling | Use Case |
|-----------|---------|-------------|----------------|----------|
| `use` binding | Scope exit | `close()` | Propagates in `!` functions (unless another error is in flight) | Explicit resource scoping |
| `drop()` method | Owner out of scope (not moved) | `drop()` | Suppressed (drop must not fail) | Automatic cleanup |
| Neither | Owner out of scope | Nothing | N/A | `` `copy `` types, primitives |

---

## 17. Concurrency

Promise uses goroutine-style lightweight coroutines. The runtime multiplexes goroutines onto OS threads and transparently handles I/O scheduling — all blocking I/O calls automatically suspend the current goroutine and resume it when the operation completes. There is **no function coloring**: functions that perform I/O have normal signatures and look identical to pure functions.

### 17.1 Transparent I/O

Functions are never declared as "async". The runtime is the async engine — any function that performs I/O automatically yields the goroutine during the blocking operation:

```promise
// This function does I/O but has a normal signature.
// The runtime suspends the goroutine during httpGet, not the OS thread.
fetchUser!(int id) User {
  data := httpGet("/users/{id}")?^;
  return User.fromJson(data)?^;
}

// Callers just call it normally:
user := fetchUser(42)?^;
```

### 17.2 Explicit Concurrency with `go`

`go` is an **expression** that launches a goroutine. A plain `go` returns a `task[T]`, where `T` is the result type of the block or call; the failable form `go!` returns a `failable_task[T]` (§17.2.1). The `<-` operator receives the result, suspending the current goroutine until it is ready.

The examples in this section spawn **non-failable** producers, so each yields a plain `task[T]`. Spawning a producer that can fail (a name ending in `!`, such as `fetchUser!`) uses `go!` and is covered in §17.2.1.

```promise
// Fire-and-forget (task[Void] result ignored)
go {
  logAnalytics(event);
};

// Value-returning task — `score` cannot fail, so `go` yields a plain task[int]
task := go score(board);           // task : task[int]
points := <-task;                  // suspends until result ready

// Inline: launch + receive
points := <-go score(board);       // equivalent to "await"

// Fan out, fan in — structured concurrency
t1 := go score(boardA);
t2 := go score(boardB);
t3 := go score(boardC);
a := <-t1;                         // all three ran concurrently
b := <-t2;
c := <-t3;
```

`task[T]` is a first-class type returned by `go` expressions. It can be stored in variables, fields, and collections, passed as arguments, and returned from functions. The `<-` operator receives the result from a task, suspending the current goroutine until the task completes. Concurrency is always a **caller-side decision** — the callee does not know or care whether it runs in a goroutine.

`task[T]` is a **single-owner handle** (like `Mutex[T]`/`MutexGuard[T]`): it is move-only and has no `clone()`. It may be a *direct* element of one collection (`Task[T][]` push/iterate/await/drop is supported), but a type that transitively contains a single-owner handle — including one nested inside a user-type field or enum variant (`Holder{Task[T] t}`, `enum E { Held(Task[T] t) }`) — is **non-cloneable**. Every context that would structurally (implicitly) copy such a value is a compile error: `clone()`/`filled()` on such a collection, slicing it (`v[a:b]`), pushing an indexed element of it (`dest.push(src[i])`), destructuring a handle-owning variant field in `match`, and nesting one inside another container (`Vector[Vector[Task[T]]]`, `Map[K, Task[T][]]`). Moving a freshly-constructed value (`dest.push(Holder(t: go …))`) is still allowed. Refcounted handles (`Ref[T]`, `Channel[T]`) are duplicable and unaffected.

### 17.2.1 Failable Goroutines — `go!` and `failable_task[T]`

A goroutine runs asynchronously, so an error it produces **cannot** flow back to the code that spawned it the way a normal failable call propagates to its caller (§7.2). Promise makes this split explicit with two spawn forms, and **which one you use is always written at the spawn site** — a goroutine's failability is never inferred.

| Spawn form | Goroutine kind | Result type | Who handles an error that escapes the body |
|---|---|---|---|
| `go f()` · `go { … }` | non-failable | `task[T]` | the goroutine itself — its body must handle its own errors |
| `go! f()` · `go! { … }` | failable | `failable_task[T]` | whoever **receives** the task, at the `<-` |

In both forms `T` is the **success** type. There is no failable *value* type — `int!` is not a type; `!` is a producer marker (§7.1). `failable_task[T]` is a distinct handle type whose producer is fallible, exactly as a function name ending in `!` marks a fallible function. It is **not** a `task[T!]` and not a `T!` — those notations do not exist.

#### Spawning

`go!` marks the goroutine as failable: its body may `raise`, and an error that escapes the body is **captured into the task**, to be surfaced later at the receive.

```promise
fetchUser!(int id) User { … }       // failable producer (does I/O)

t := go! fetchUser(42);             // t : failable_task[User]
```

Plain `go` is for non-failable work. Spawning a **failable** producer with plain `go` is a compile error — its error would have nowhere to go:

```promise
t := go fetchUser(42);
//   ^ error: `fetchUser` is failable — spawn it with `go!`, or handle the
//            error inside the goroutine (e.g. `go { fetchUser(42)?!; }`)
```

Symmetrically, `go!` on a producer that cannot fail is a compile error — the `!` would be misleading:

```promise
t := go! score(board);              // score() is not failable
//   ^ error: `score` cannot fail; spawn it with plain `go`
```

**Block form.** The marker selects the *failability of the block's scope*, mirroring function bodies:

- `go { … }` is a **non-failable scope**: a failable call inside it must be handled locally (`?!`, `? e { … }`), exactly as in a non-failable function. This is the idiom for fire-and-forget work that performs I/O.
- `go! { … }` is a **failable scope**: a failable call inside it auto-propagates into the task (like a bare call in a failable function), and the escaping error is delivered to whoever receives the task.

```promise
// Non-failable block — handles its own error; safe to fire and forget
go {
  saveMetrics(snapshot)?!;          // panics in the goroutine on failure
};

// Failable block — an escaping error is captured into the task
t := go! {
  user := fetchUser(42);            // auto-propagates into the task
  return enrich(user);              // T = the block's result type
};
```

#### Receiving

Receiving with `<-` is where a failable task's error surfaces. **A `<-` on a `failable_task[T]` is itself a failable operation** that yields `T` — semantically identical to calling a failable function — so it obeys the ordinary error rules of §7.2:

- In a **failable function**, a bare receive auto-propagates:
  ```promise
  loadProfile!(int id) Profile {
    t := go! fetchUser(id);
    user := <-t;                     // auto-propagates the goroutine's error
    return Profile.of(user);
  }
  ```
- In a **non-failable function**, the receive must be handled, exactly like any other failable call:
  ```promise
  main() {
    t := go! fetchUser(42);
    user := (<-t)?!;                 // panic on the goroutine's error
    // or handle it:
    user := (<-t) ? e {
      print_line("fetch failed: {e.message}");
      return;
    };
  }
  ```
  An **unhandled** bare receive from a `failable_task[T]` in a non-failable function is a compile error — the same "failable call must be handled" diagnostic as any unhandled failable call.

Receiving from a plain `task[T]` is unchanged: an ordinary, non-failable receive.

**Operator binding — error operators apply to the receive, not the spawn.** Because the error appears at the receive, the error operators attach there. Writing one on the spawn is a compile error with a fix-it:

```promise
v := go! fetchUser(42)?!;
//                    ^ error: apply the error operator to the receive:
//                             `(<-go! fetchUser(42))?!`
```

So `go! f()?!`, `go! f()?^`, and `go! f() ? e { … }` are all rejected. The inline launch-await-handle form is `(<-go! f())?!` (the failable counterpart of the plain `<-go f()` await).

#### Fire-and-forget must be non-failable

A `failable_task[T]` carries an error that **someone must receive**. Dropping one without ever receiving it would silently swallow that error, so discarding a `failable_task[T]` is a compile error:

```promise
go! fetchUser(42);                   // result discarded
// ^ error: a fire-and-forget goroutine must be non-failable. Handle the error
//          inside it — `go { fetchUser(42)?!; }` — or keep and receive the task.
```

A plain `task[T]` (including `task[Void]`) may be discarded as before.

> **The rule:** a goroutine's error is handled by **exactly one** party — *inside* the goroutine (making it non-failable and freely fire-and-forgettable) or *outside* by whoever receives the `failable_task[T]`. It is never silently dropped.

#### `failable_task[T]` is a single-owner handle

Like `task[T]`, a `failable_task[T]` is a **single-owner handle**: move-only, no `clone()`, and subject to the same non-cloneable-transitivity rules described above for `task[T]`. Its failability is part of its type, so it crosses field, collection, parameter, and return boundaries like any other type — `failable_task[int][]`, `Holder{ failable_task[int] t }`, `process(failable_task[int] t)` — and in every case must still be received (or moved to someone who will receive it) before it goes out of scope.

#### Summary

| Expression | Meaning |
|---|---|
| `go f()` — `f` non-failable | spawn → `task[T]` |
| `go f()` — `f` failable | **compile error** → use `go!`, or handle inside the goroutine |
| `go! f()` — `f` failable | spawn → `failable_task[T]` |
| `go! f()` — `f` non-failable | **compile error** → use plain `go` |
| `go { … }` | non-failable scope → `task[T]` (block handles its own errors) |
| `go! { … }` | failable scope → `failable_task[T]` (escaping error captured) |
| `<-t` — `t : task[T]` | non-failable receive → `T` |
| `<-t` — `t : failable_task[T]` | **failable** receive → `T` (auto-propagate or handle per §7.2) |
| `(<-go! f())?!` | inline launch + await + panic-on-error |
| `go! f()` discarded | **compile error** → fire-and-forget must be non-failable |

### 17.3 Channels

Channels are the primary synchronization primitive for streaming data between goroutines:

```promise
main() {
  ch := channel[int](capacity: 10);

  go {
    for i in 0..100 {
      ch.send(i);
    }
    ch.close();
  };

  for value in ch {
    print_line("{value}");
  }
}
```

The `<-` operator also works on channels: `value := <-ch;` receives the next value.

A `channel[T]` is iterable — `ch.iter()` returns a `stream[T]`, and from there the standard stream combinators (`map`, `filter`, `fold`, …) apply. The combinators belong to the stream, not the channel. See Section 12.6 for details and caveats about destructive iteration.

### 17.4 Ownership Across Goroutines

Ownership rules apply across goroutines — data is either **moved** into the goroutine or shared via `Ref[T]` (reference-counted shared ownership; the counter is atomic precisely because the value crosses a goroutine boundary here):

```promise
main() {
  data := loadData();

  // data is moved into the goroutine — no longer valid in main
  go {
    process(data);
  };

  // Shared access requires Ref
  Ref[Config] config = Ref[Config](loadConfig());
  go {
    serve(config.clone());
  };
}
```

**Atomicity is a transparent implementation detail.** A `Ref[T]` is reference-counted; *whether the counter is atomic is decided by the compiler, not the program.* A `Ref` that never crosses a `go`/channel/`Task` boundary uses a plain non-atomic counter; one that may be shared across goroutines uses an atomic counter. Semantics are identical either way. You can opt a type into the fast non-atomic counter explicitly by marking it `` `confined ``:

```promise
type LocalState `confined { int count; }

s := Ref[LocalState](LocalState(count: 0));  // non-atomic counter
go { s.borrow.count; };                       // error: confined Ref can't cross a goroutine
```

A `` `confined `` type's `Ref`/`Weak` is **rejected at any `go`/channel/`Task` boundary** (reusing the `` `sharable `` enforcement), which is what makes the plain counter sound. `` `confined `` and `` `sharable `` are mutually exclusive. (A later, optional whole-program analysis may infer confinement automatically; being conservative, it can only ever turn an atomic counter into a non-atomic one when the value provably stays on one thread — never a behavior change.)

---

## 18. Complete Example

```promise
use io "github.com/promise-language/std/io/1"
use json "github.com/promise-language/std/json/1"

type Todo `serializable {
  int id `json(name: "id");
  string title `json(name: "title");
  bool done = false;                     // field default — constructor can skip

  toggle(~this) `instance {
    this.done = !this.done;
  }

  new(int id, string move title) Todo `type {
    return Todo(id: id, title: move title);   // done defaults to false
  }
}

type TodoList {
  Todo[] items;

  add(~this, string move title, int priority = 0) `instance {
    int id = this.items.len + 1;
    this.items.push(Todo.new(id, move title));
  }

  pending(this) Todo[] `instance {
    return this.items.iter().filter(|Todo t| -> !t.done).collect();
  }
}

loadFromFile!(string path) TodoList {
  string content = io.readFile(path);          // auto-propagates on error
  Todo[] items = json.decode[Todo[]](content); // auto-propagates on error
  return TodoList(items: move items);
}

main() {
  TodoList todos = loadFromFile("todos.json") ? err {
    print_line("Starting fresh: {err.message}");
    TodoList(items: []);
  };

  todos.add("Design Promise language");
  todos.add("Build the compiler", priority: 1);   // named arg

  for i, todo in todos.items {
    status := if todo.done { "done" } else { "    " };
    print_line("[{status}] {todo.title}");
  }
}
```

---

## 19. Grammar Sketch (ANTLR4)

Key productions (simplified):

```antlr
grammar Promise;

compilationUnit: useDecl* declaration* EOF;

useDecl: 'use' IDENT STRING_LITERAL ';';

declaration
    : typeDecl
    | funcDecl
    | enumDecl
    ;

metaAnnotation: '`' IDENT ('(' metaParams ')')?;
metaParams: metaParam (',' metaParam)*;
metaParam: expression | IDENT ':' expression;

typeDecl
    : 'type' IDENT typeParams? ('is' typeRef (',' typeRef)*)?
      metaAnnotation* '{' typeMember* '}'
    ;

typeParams: '[' typeParam (',' typeParam)* ']';
typeParam: IDENT (':' typeConstraint)?;
typeConstraint: typeRef ('+' typeRef)*;

typeMember: fieldDecl | methodDecl;
fieldDecl: typeRef IDENT metaAnnotation* ('=' expression)? ';';
methodDecl: IDENT '!'? typeParams? '(' params ')' typeRef? metaAnnotation* (block | ';');

funcDecl: IDENT '!'? typeParams? '(' params ')' typeRef? metaAnnotation* block;
// Old syntax (deprecated): returnType: typeRef '!'? | '!';

// Parameters (definition side)
params: paramList?;
paramList: receiverParam (',' param)* | param (',' param)*;
receiverParam: '~'? 'this';                     // this (shared borrow) or ~this (mutable borrow)
param: typeRef 'move' IDENT                      // move — transfer ownership: T move name
     | typeRef IDENT ('=' expression)?;           // borrow — T name (shared) or T~ name (mutable); optional default

// Arguments (call site)
args: (arg (',' arg)*)?;
arg: (IDENT ':')? expression;             // named (IDENT ':') or positional

enumDecl: 'enum' IDENT typeParams? metaAnnotation* '{' enumVariant (',' enumVariant)* ','? '}';
enumVariant: IDENT ('(' enumFields ')')?;
enumFields: enumField (',' enumField)*;
enumField: typeRef IDENT;

// Abstract methods use `abstract meta — no separate interface construct
// methodDecl with `abstract has no block: IDENT '(' params ')' returnType? '`abstract' ';'

// Type references
typeRef
    : typeRef '&'                        // shared reference (postfix)
    | typeRef '~'                        // mutable reference (postfix)
    | typeRef '*'                        // raw pointer (postfix)
    | typeRef '?'                        // optional (postfix)
    | typeRef '[' ']'                    // slice
    | typeRef '[' INT_LITERAL ']'        // fixed array
    | IDENT typeArgs?                    // named type
    | '(' typeRef (',' typeRef)* ')'     // tuple
    | '(' typeRefList ')' '->' typeRef   // function type (arrow syntax)
    ;

typeArgs: '[' typeRef (',' typeRef)* ']';
typeRefList: typeRef (',' typeRef)*;

// Core expression and statement productions (simplified)
block: '{' statement* '}';
statement: expression ';' | varDecl | useDecl | assignment | returnStmt | raiseStmt
         | ifStmt | forStmt | whileStmt | yieldStmt | yieldDelegateStmt;

useDecl: 'use' IDENT ':=' expression ';';     // scoped resource — close() at scope exit

expression: primary | expression binOp expression | unaryOp expression
          | expression '.' IDENT | expression '(' args ')'
          | expression '?' IDENT? block             // error handler
          | expression '?' '^' | expression '?' '!' | expression '!'   // ?^ propagate / ?! panic / ! optional-unwrap
          | expression 'as' '!'? typeRef            // type cast: safe (as) or unsafe (as!)
          | goExpr | receiveExpr | rangeExpr | isExpr
          | ifExpr | matchExpr | '(' expression ')';

// Control flow
ifStmt: 'if' expression block ('else' (ifStmt | block))?;
ifExpr: 'if' expression block 'else' block;
whileStmt: 'while' expression block;
forInStmt: 'for' IDENT (',' IDENT)? 'in' expression block;
classicForStmt: 'for' varDecl ';' expression ';' expression block;
forStmt: forInStmt | classicForStmt | 'for' block;   // infinite loop

goExpr: 'go' '!'? (block | expression);   // 'go' → task[T]; 'go!' → failable_task[T] (§17.2.1)
receiveExpr: '<-' expression;          // receive from task[T] / failable_task[T] / channel[T]
                                       // (<- on a failable_task[T] is a failable operation)

// Error handling (also used for optional unwrap/handler — sema disambiguates by type)
errorPropagate: expression '?' '^';                      // ?^ — propagate error up (failable fn only)
errorPanic: expression '?' '!';                          // ?! — panic on error
errorHandler: expression '?' IDENT? block;               // ? e { ... } (error) or ? _ { ... } (optional)
errorUnwrap: expression '!';                             // ! — unwrap optional (panic on none)
resultDestructure: '(' IDENT ',' IDENT ')' ':=' expression;  // (val, err) := expr

// range expressions
rangeExpr: expression '..' '='? expression;    // 0..10 (exclusive) or 0..=10 (inclusive)

// Yield (only valid inside generator functions returning stream[T])
yieldStmt: 'yield' expression ';';
yieldDelegateStmt: 'yield' '*' expression ';';

// Pattern matching — `is` expression and `match` expression
isExpr: expression 'is' pattern;
matchExpr: 'match' expression '{' matchArm (',' matchArm)* ','? '}';
matchArm: matchPattern ('if' expression)? '=>' (expression | block);

matchPattern
    : '_'                                    // wildcard
    | LITERAL                                // literal (int, string, bool)
    | IDENT '.' IDENT                        // enum variant: Color.Red
    | IDENT '.' IDENT '(' patternFields ')'  // enum destructure: Color.Custom(r, g, b)
    | IDENT IDENT?                           // type pattern with optional binding: Dog d
    | expression                             // expression pattern: n % 15 == 0
    ;

pattern
    : IDENT '(' patternFields ')'       // enum destructuring: Some(u), Ok(v)
    | IDENT                              // type check: Dog, or contextual: present, absent
    ;
patternFields: patternField (',' patternField)*;
patternField: IDENT;

// Unwrap binding in if/while conditions
ifUnwrap: 'if' IDENT ':=' expression block ('else' block)?;
whileUnwrap: 'while' IDENT ':=' expression block;

// Numeric literals with optional type suffixes (lexer fragments)
fragment INT_SUFFIX: [iu] ('8' | '16' | '32' | '64');
fragment FLOAT_SUFFIX: 'f32' | 'f64';
INT_LITERAL: ('0' [xXoObB]? ...)  INT_SUFFIX?;
FLOAT_LITERAL: ... FLOAT_SUFFIX?;
```

---

## 20. Compiler Implementation Plan (Go)

Single binary `promise` with the following internal packages:

| Package               | Responsibility                                    |
|-----------------------|---------------------------------------------------|
| `cmd/promise`         | CLI entry point (build, run, test, add, etc.)     |
| `parser/`             | ANTLR4-generated lexer/parser + parse tree visitor|
| `ast/`                | AST node definitions                              |
| `types/`              | Type system: the 4-struct model, generics, traits |
| `sema/`               | Semantic analysis: type checking, ownership check |
| `ownership/`          | Borrow checker implementation                     |
| `codegen/`            | LLVM IR generation via Go LLVM bindings           |
| `module/`             | Module resolution, dependency graph               |
| `meta/`               | Meta annotation processing                        |
| `pkg/`                | Package manager: fetch, resolve, lock, build      |

---

## 21. Package Manager (integrated into `promise` binary)

### Dependency Resolution

- Dependencies are discovered by scanning all `use` declarations in source files — no separate dependency manifest.
- Modules fetched from their URL (git clone / HTTP archive).
- Version segments in the URL are the version specifier — no separate version field.
- Lockfile (`promise.lock`) pins exact commit hashes.
- Diamond dependency resolution: if two deps require different versions of the same module, both are compiled separately (like Go's module system).

---

## 22. Open Design Questions

1. **REPL** — Should the toolchain include an interpreter/REPL for rapid prototyping?
