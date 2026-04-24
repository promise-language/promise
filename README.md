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

**Stage 1 (current):** Grammar and scaffolding. The ANTLR4 lexer and parser grammars are complete. The `promise` binary can parse `.pr` files, print parse trees, and report syntax errors. 266 tests cover all language constructs.

Upcoming stages: AST builder, semantic analysis (type checking, ownership analysis), LLVM IR code generation.

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
│   ├── cmd/promise/
│   │   ├── main.go             # CLI entry point
│   │   └── parse_test.go       # Test suite (266 tests)
│   ├── grammar/
│   │   ├── PromiseLexer.g4     # Lexer: keywords, operators, literals, string interpolation
│   │   └── PromiseParser.g4    # Parser: declarations, statements, expressions
│   ├── internal/parser/         # ANTLR4-generated Go code (gitignored)
│   ├── testdata/
│   │   ├── valid/              # 16 valid fixture files by feature area
│   │   └── invalid/            # 15 invalid fixture files for error detection
│   └── tools/                  # ANTLR4 JAR (gitignored)
├── docs/
│   └── language-design.md
└── README.md
```

## Design Document

See [docs/language-design.md](docs/language-design.md) for the full language specification covering types, ownership, error handling, concurrency, generics, and more.

## License

TBD
