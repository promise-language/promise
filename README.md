# Promise Programming Language

Statically-typed language designed for AI-agent efficiency. Explicit ownership (like Rust), goroutine-style concurrency (like Go), algebraic errors, generics, exhaustive pattern matching — zero hidden effects. Native LLVM compilation to Linux, macOS, Windows, and WASM.

**Designed for AI agents.** Read the [Language Guide](docs/language-guide.md) to start writing Promise code. Every design decision optimizes for an AI generating correct, self-contained programs in one shot. Reading a single `.pr` file tells you exactly what it does — no hidden effects, no implicit behaviors, no action-at-a-distance. Types are explicit. Errors are explicit (`?^`/`?!`/`!`). Ownership is explicit (`~`/`&`). Mutability is explicit. There is one obvious way to do things, so code generation is deterministic.

**Mono-versioned catalog.** Promise eliminates dependency hell entirely. There are no per-package versions, no lockfiles, no version resolution. Instead, the entire ecosystem — compiler, standard library, and all catalog modules — ships as a single atomic release called an **epoch** (e.g., `2026.0`). Every module in an epoch is tested together as a unit. Your project declares which epoch it targets in `promise.toml`, and that's it. An AI agent only needs to know the epoch to generate correct imports — no version guessing, no compatibility reasoning, no `package.json` / `Cargo.toml` / `go.mod` boilerplate.

**Self-contained toolchain.** The compiler is a single Go binary (~61MB release build) that embeds everything: LLVM tools, standard library, catalog modules, and runtime. `promise sync` downloads one file. `promise install` extracts it. No system dependencies beyond a linker. Multiple epochs can coexist side-by-side under `~/.promise/epochs/`.

**Modules without ceremony.** Import a catalog module with `use io;` — no URL, no version, no path. The standard library (`std`) is auto-imported into every file. Catalog modules (`io`, `json`, `os`, `net`, `path`, `math`, `strings`, `time`, `term`, `http`) are separate compilation units cached as LLVM bitcode for fast incremental builds.

## Example

```
type Circle {
  f64 radius;

  get area f64 => 3.14159 * this.radius * this.radius;
}

main() {
  c := Circle(radius: 5.0);
  print_line("Area: {c.area}");

  numbers := [1, 2, 3, 4, 5];
  sum := numbers.filter(|n| -> n > 2).fold(0, |acc, n| -> acc + n);
  print_line("Sum: {sum}");
}
```

## Key Features

- **Ownership & borrowing** -- Rust-inspired move semantics with `&` (shared borrow), `~` (mutable borrow), and `*` (raw pointer)
- **Algebraic error handling** -- `!` on function names marks failable, bare calls auto-propagate, `?^` explicit propagate, `?!` panic, `? e { }` pattern-matched handlers
- **Generics** -- type parameters with constraints: `sort[T is Comparable](T[] items)`
- **Enums with data** -- `enum Option[T] { Some(T value), None }`
- **Pattern matching** -- exhaustive `match` with destructuring, guards, and type patterns
- **String interpolation** -- `"Hello, {name}!"` with arbitrary expressions
- **Goroutine-style concurrency** -- `go expr` returns `Task[T]`, `<-task` to receive
- **Generators** -- `yield` and `yield* iter` for lazy `Stream[T]` production
- **Meta annotations** -- `` `public ``, `` `final ``, `` `value ``, `` `abstract ``, `` `structural ``, `` `test `` for declarative metadata
- **No macros, no conditional compilation** -- all code is fully visible in the source file

## Status

**WARNING: Under active development, not for production use.**

The compiler is functional end-to-end: parsing, type checking, ownership analysis, and LLVM IR codegen all work. 3600+ tests across 350+ files. Modules, generics, concurrency, and the standard library are implemented. Active development.

## Building

Prerequisites: Go 1.22+, Java (for ANTLR4 code generation), clang, LLVM 22+.

```sh
./make                # bootstrap: compile all build tools to bin/
bin/build             # generate parser + embed resources + build → bin/promise
bin/build --release   # release build with embedded LLVM tools
bin/test              # build + run all tests (Go + Promise)
bin/verify --wasm     # format + vet + full test suite (pre-commit check)
```

**Important:** Always use `bin/build` to build the compiler. Never run `go build` directly — it skips resource embedding and produces a broken binary.

## Usage

```sh
promise build file.pr           # compile to executable
promise build -o out file.pr    # compile with custom output name
promise run file.pr             # compile and run
promise test file.pr            # discover and run `test functions
promise test tests/...          # recursive directory scan
promise check file.pr           # type-check only
promise ast file.pr             # print the AST
```

### Testing options

```sh
promise test -timeout 30s file.pr                    # per-test timeout (default: 60s)
promise test -stress tests/...                       # stress test until Ctrl+C
promise test -stress 100 tests/concurrency/...       # 100 iterations
promise test -stress 30s tests/concurrency/...       # run for 30 seconds
promise test -timeout 10s -stress 50 tests/...       # combine per-run timeout with stress
```

Stress mode compiles all files once, then repeatedly runs the test binaries. It tracks
per-test pass rates and timing variance to detect flaky tests. Stable tests are gradually
suppressed to focus resources on unreliable ones.

### Inline execution

Run Promise code directly from the command line:

```sh
promise exec 'print_line(42)'                        # expression (auto-wrapped in main)
promise exec 'main() { print_line("hello"); }'       # full program
echo 'print_line("hello")' | promise exec            # from stdin
echo 'print_line("hello")' | promise                 # bare pipe (auto-detected)
cat program.pr | promise                              # pipe a file
```

Errors show the source line with context and a caret marker:

```
1:20: undefined: foo
  > main() { print_line(foo); }
                        ^
```

### Install

Install Promise system-wide from a built binary:

```sh
promise install
```

Copies the binary to `~/.promise/bin/` and extracts the embedded standard library, catalog modules, and LLVM tools to `~/.promise/`. The binary is self-contained — modules and runtime are embedded at build time via `go:embed`.

## Project Structure

```
promise_lang/
├── compiler/                    # Go compiler (single binary)
│   ├── go.mod
│   ├── cmd/promise/             # CLI entry point (build, run, exec, install)
│   ├── grammar/                 # ANTLR4 lexer/parser grammars
│   ├── internal/
│   │   ├── ast/                 # AST builder (CST → typed AST)
│   │   ├── types/               # Type system (Named, Enum, Scope, generics)
│   │   ├── sema/                # Semantic analysis (type checking, resolution)
│   │   ├── ownership/           # Ownership analysis (moves, borrows)
│   │   ├── codegen/             # LLVM IR code generation
│   │   ├── formatter/           # Token-based source formatter
│   │   ├── module/              # Module system
│   │   └── parser/              # ANTLR4-generated Go code (gitignored)
│   ├── testdata/                # Parse test fixtures
│   └── tools/                   # ANTLR4 JAR (gitignored)
├── modules/
│   ├── std/                     # Standard library (auto-imported)
│   ├── io/                      # File I/O
│   ├── json/                    # JSON encode/decode
│   ├── os/                      # OS interaction, processes, signals
│   ├── net/                     # TCP networking
│   ├── path/                    # Path manipulation
│   ├── math/                    # Extended math
│   ├── strings/                 # Extended string utilities
│   ├── time/                    # Extended time utilities
│   ├── term/                    # Terminal UI (raw mode, cell buffer, events)
│   └── http/                    # HTTP client (in progress)
├── tests/                       # Integration and e2e tests
├── examples/                    # Runnable examples
├── bin/                         # Build tools (compiled by ./make)
├── docs/
│   ├── language-design.md       # Full language specification
│   ├── language-guide.md        # Concise language reference
│   ├── standard-library.md      # Standard library design
│   └── ...
└── README.md
```

## Documentation

- [Language Guide](docs/language-guide.md) — concise reference for writing Promise code
- [Language Design](docs/language-design.md) — full language specification (types, ownership, errors, concurrency, generics)
- [Standard Library](docs/standard-library.md) — module inventory and API reference
- [Build Tools](docs/build-tools.md) — build system architecture

## License

Licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](LICENSE-APACHE) or <http://www.apache.org/licenses/LICENSE-2.0>)
- MIT License ([LICENSE-MIT](LICENSE-MIT) or <http://opensource.org/licenses/MIT>)

at your option.
