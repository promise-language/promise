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

**Stage 8h (current):** The compiler pipeline is functional through code generation. Stages 1–8h are complete:

- **Stages 1–2:** ANTLR4 grammar, CST → AST builder
- **Stages 3–4:** Type system, semantic analysis (type checking, name resolution, exhaustiveness)
- **Stage 5:** Generic type substitution, constraint validation, instance tracking
- **Stage 6:** Ownership analysis (move semantics, borrow tracking, return safety)
- **Stage 7:** Meta annotation processing
- **Stage 8a–8h:** LLVM IR codegen — primitives, strings, user types, enums, error handling, generic monomorphization, containers (tuples, optionals, slices, maps, lambdas), optional patterns, string interpolation

The `promise build file.pr` command compiles Promise source to native binaries via LLVM IR + clang. 400+ tests across all packages.

Upcoming stages: module system, full CLI, package manager.

## Building

Prerequisites: Go 1.22+, Java (for ANTLR4 code generation).

```sh
cd compiler
make              # download ANTLR4 JAR, generate parser, build binary
make test         # run tests
make clean        # remove generated code and binary
```

Individual targets:

```sh
make download-antlr   # fetch ANTLR4 JAR to tools/
make generate         # regenerate Go parser from grammars
make build            # compile the promise binary
make fmt              # go fmt
```

## Usage

```sh
./compiler/promise path/to/file.pr
```

Prints the LISP-style parse tree to stdout. Syntax errors are reported to stderr as `file:line:col: message`.

## Project Structure

```
promise_lang/
├── compiler/                    # Go compiler (single binary)
│   ├── go.mod
│   ├── Makefile
│   ├── cmd/promise/             # CLI entry point (build, run, check)
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
