# Promise Programming Language

Promise is a systems-aware, statically-typed programming language with Dart-inspired syntax, Rust-inspired ownership semantics, and a rich type system featuring inheritance, generics, and algebraic error handling.

The toolchain (compiler + package manager) is a single Go binary called `promise`, uses ANTLR4 for parsing, and targets LLVM IR for code generation.

## Example

```
use io "std/io";

type Circle {
  Float radius;

  area(&this) Float `instance {
    return 3.14159 * this.radius * this.radius;
  }
}

main() {
  Circle c = Circle(radius: 5.0);
  io.println("Area: {c.area()}");

  Int[] numbers = [1, 2, 3, 4, 5];
  Int sum = numbers.filter(|n| -> n > 2).fold(0, |acc, n| -> acc + n);
  io.println("Sum: {sum}");
}
```

## Key Features

- **Ownership & borrowing** -- Rust-inspired move semantics with `&` (shared borrow), `~` (mutable borrow), and `*` (raw pointer)
- **Algebraic error handling** -- `!` suffix for fallible return types, `?` propagation, pattern-matched error handlers
- **Generics** -- type parameters with constraints: `sort[T is Comparable](T[] items)`
- **Enums with data** -- `enum Option[T] { Some(T value), None }`
- **Pattern matching** -- exhaustive `match` with destructuring, guards, and type patterns
- **String interpolation** -- `"Hello, {name}!"` with arbitrary expressions
- **Goroutine-style concurrency** -- `go expr` returns `Task[T]`, `<-task` to receive
- **Generators** -- `yield` and `yield* iter` for lazy `Stream[T]` production
- **Meta annotations** -- `` `instance ``, `` `static ``, `` `abstract `` for declarative metadata
- **No macros, no conditional compilation** -- all code is fully visible in the source file

## Status

**Stage 8f (current):** The compiler pipeline is functional through code generation. Stages 1–8f are complete:

- **Stages 1–2:** ANTLR4 grammar, CST → AST builder
- **Stages 3–4:** Type system, semantic analysis (type checking, name resolution, exhaustiveness)
- **Stage 5:** Generic type substitution, constraint validation, instance tracking
- **Stage 6:** Ownership analysis (move semantics, borrow tracking, return safety)
- **Stage 7:** Meta annotation processing
- **Stage 8a–8f:** LLVM IR codegen — primitives, strings, user types, enums, error handling, generic monomorphization, containers (tuples, optionals, slices, maps, lambdas), optional patterns, string interpolation, promise-first builtin types

The compiler produces native binaries via LLVM IR + clang. 400+ tests across all packages.

Upcoming stages: module system, package manager.

## Building

Prerequisites: Go 1.22+, Java (for ANTLR4 code generation), clang.

```sh
cd compiler
make              # download ANTLR4 JAR, generate parser, build binary
make test         # run tests
make install      # install to ~/.promise/
make clean        # remove generated code and binary
```

## Usage

```sh
promise build file.pr           # compile to executable
promise build -o out file.pr    # compile with custom output name
promise run file.pr             # compile and run
promise test file.pr            # discover and run @test functions
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
promise exec 'print_int(42)'                        # expression (auto-wrapped in main)
promise exec 'main() { println("hello"); }'         # full program
echo 'println("hello")' | promise exec              # from stdin
echo 'println("hello")' | promise                   # bare pipe (auto-detected)
cat program.pr | promise                             # pipe a file
```

Errors in inline mode show the source line with a caret marker:

```
1:10: undefined: foo
    print_int(foo);
              ^
```

### Install

Install Promise system-wide from a built binary:

```sh
promise install
```

Copies the binary to `~/.promise/bin/` and extracts the embedded standard library and runtime to `~/.promise/lib/`. The binary is self-contained — `std/` and `runtime/` files are embedded at build time via `go:embed`.

## Project Structure

```
promise_lang/
├── compiler/                    # Go compiler (single binary)
│   ├── go.mod
│   ├── Makefile
│   ├── cmd/promise/             # CLI entry point (build, run, exec, install)
│   ├── grammar/                 # ANTLR4 lexer/parser grammars
│   ├── internal/
│   │   ├── ast/                 # AST builder (CST → typed AST)
│   │   ├── types/               # Type system (Named, Enum, Scope, generics)
│   │   ├── sema/                # Semantic analysis (type checking, resolution)
│   │   ├── ownership/           # Ownership analysis (moves, borrows)
│   │   ├── codegen/             # LLVM IR code generation
│   │   └── parser/              # ANTLR4-generated Go code (gitignored)
│   ├── testdata/                # Parse test fixtures
│   └── tools/                   # ANTLR4 JAR (gitignored)
├── std/                         # Standard library (.pr files)
├── runtime/                     # C runtime (strings, maps, print, panic)
├── docs/
│   ├── language-design.md       # Full language specification
│   ├── stages.md                # Compiler implementation roadmap
│   └── c-binding-architecture.md
└── README.md
```

## Design Document

See [docs/language-design.md](docs/language-design.md) for the full language specification covering types, ownership, error handling, concurrency, generics, and more.

## License

TBD
