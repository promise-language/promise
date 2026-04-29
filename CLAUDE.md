# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Promise is a statically-typed programming language with Dart-inspired syntax and Rust-inspired ownership semantics. The compiler is a single Go binary (`promise`) that uses ANTLR4 for parsing and targets LLVM IR (linked via clang to native binaries).

## Build & Test Commands

All commands run from `compiler/`:

```bash
make                  # download ANTLR4 JAR, generate parser, embed resources, build binary
make test             # run all Go tests (go test ./...)
make generate         # regenerate ANTLR4 parser from grammar
make resources        # copy std/ and runtime/ into embedded resources
make clean            # remove generated code and binary

# Run tests for a single package
go test ./internal/codegen/ -count=1
go test ./internal/sema/ -count=1
go test ./internal/ownership/ -count=1

# Run a single test
go test ./internal/codegen/ -run TestDropBasicScopeExit -v -count=1

# Coverage
go test ./internal/codegen/ -coverprofile=/tmp/cov.out -count=1
go tool cover -func=/tmp/cov.out

# End-to-end tests (from repo root)
bin/e2e.sh
PROMISE_E2E_TIMEOUT=120 bin/e2e.sh  # custom timeout (default: 60s)

# Promise test with timeout
promise test file.pr                       # default 60s timeout
promise test -timeout 30s file.pr          # custom timeout (Go duration or seconds)
promise exec -timeout 10s 'println("hi")'  # exec with timeout
```

## Compiler Pipeline

```
.pr source → ANTLR4 (grammar/) → AST (ast/) → Sema 4-pass (sema/) → Ownership (ownership/) → LLVM IR (codegen/) → clang → binary
```

Entry point: `cmd/promise/main.go` → `compileFrontend()` orchestrates parse → std merge → sema → ownership.

**Sema 4 passes** (in `sema/check.go`):
1. **Declare** — register all type/enum/func names in scope
2. **Define** — resolve parent types, fields, method signatures
3. **Check** — type-check all function/method bodies
4. **Verify** — return path analysis

**Ownership** runs after sema: move tracking, borrow conflicts, return safety.

**Codegen** (`codegen/compiler.go`): AST + sema info → LLVM IR module. Handles monomorphization of generics, RTTI for inheritance, vtable dispatch, scope cleanup (use/drop bindings).

## Key Architecture Concepts

**Four-struct type layout**: Each user type generates up to 4 LLVM structs — Type (static), Instance (heap fields), Value (`{vtable_ptr, instance_ptr}`), and Variant (enum payload). Value structs are passed around; instance ptrs are what methods receive as `this`.

**Scope cleanup stack**: `scopeBindings` is a LIFO stack of `bindingClose` (use vars), `bindingDrop` (droppable vars), and `bindingFreeEnv` (closure env structs). Emitted in reverse at scope exit, return, raise, break, continue. Drop bindings have an `i1` drop flag that's cleared at move sites. Env free bindings null-check the env pointer before calling `free()`.

**Closures/lambdas**: All function values use fat pointer `{ i8*, i8* }` (fn ptr + env ptr). Lambda functions always take `i8* %env` as first parameter. Capture analysis in sema (`checkLambdaCapture`) detects outer-scope variable references; `Copy` types auto-captured, non-`Copy` requires `move`. Nested captures propagate through intermediate lambdas. Named function references use thunks (`.thunk.name`) to adapt to env-first ABI. Env structs are heap-allocated via `malloc` and freed via `bindingFreeEnv` at scope exit.

**Monomorphization**: Generic types/functions are specialized at codegen time. `mono.go` handles collecting instances, creating specialized layouts, and defining specialized methods.

**Standard library**: `.pr` files in `std/` are embedded via `go:embed` and merged into user AST before sema. Runtime support is C code in `runtime/` linked by clang.

## Test Patterns

Tests use helper functions that build a mini standard library (`stdAll`) with all primitive type declarations, then parse + sema + codegen user code:

```go
// codegen tests
ir := generateIR(t, `
    type Foo { int x; }
    main() { f := Foo(x: 1); }
`)
assertContains(t, ir, "call void @Foo.drop")

// sema tests
errs := checkErrs(t, `type Bad { drop(this) {} } main() {}`)
expectError(t, errs, "must take ~this")

// ownership tests
ownerOK(t, `type R { int id; drop(~this){} } test() { r := R(id:1); }`)
errs := ownerErrs(t, `...`)
expectOwnerError(t, errs, "use of moved variable")
```

Methods must be declared inside the type body. Numeric literals infer as `int`/`f64` — use typed parameters for i8/i16/i32/f32 tests.

## Important Files

- `compiler/grammar/PromiseParser.g4` — grammar is the language spec
- `docs/stages.md` — implementation roadmap with deferred bug tracker
- `docs/language-design.md` — full language design proposal
- `compiler/internal/codegen/compiler.go` — codegen entry, type layouts, scope cleanup
- `compiler/internal/codegen/stmt.go` — statement codegen, drop/close emission
- `compiler/internal/codegen/expr.go` — expression codegen, all call variants
- `compiler/internal/sema/check.go` — sema orchestration
- `compiler/internal/sema/info.go` — sema output (type map, objects, lambda captures)
- `compiler/internal/sema/decl.go` — type/method/func definition passes
- `compiler/internal/types/named.go` — Named type with fields, methods, generics, flags

## Conventions

- Compiler errors are accumulated (not fatal on first error) and printed together
- `extractNamed(typ)` unwraps Instance/SharedRef/MutRef to get underlying `*types.Named`
- `needsVtable(named)` returns true if type has children or is abstract → virtual dispatch
- `mangleMethodName(owner, name, failable)` produces LLVM function names like `TypeName.method`
- Move sites must call `clearDropFlag(name)` — there are 8 call variant sites in expr.go plus assignment sites in stmt.go
- All tests must pass after changes. Significant changes need accompanying tests.
