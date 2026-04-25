# Compiler Stages

Implementation stages for the Promise compiler pipeline.

## Overview

| Stage | Package | Description | Status |
|-------|---------|-------------|--------|
| 1 | `compiler/grammar/` | ANTLR4 lexer/parser from grammar | Done |
| 2 | `compiler/internal/ast/` | AST builder: parse tree → typed AST nodes | Done |
| 3 | `compiler/internal/types/` | Type system: Named, Enum, Signature, Scope, Universe | Done |
| 4 | `compiler/internal/sema/` | Semantic analysis: type checking, name resolution, returns, exhaustiveness | Done |
| 5a | `compiler/internal/sema/` | Generic type substitution, constraint validation, instance tracking | Done |
| 5b | `compiler/internal/sema/` | Use declarations, multi-constraint, Iter/Stream, unreachable code | Next |
| 6 | `compiler/internal/ownership/` | Borrow checker: move semantics, lifetime inference | Planned |
| 7 | `compiler/internal/meta/` | Meta annotation processing and validation | Planned |
| 8 | `compiler/internal/codegen/` | LLVM IR generation | Planned |
| 9 | `compiler/internal/module/` | Module resolution, dependency graph | Planned |
| 10 | `cmd/promise/` | CLI entry point (build, run, test, fmt, etc.) | Planned |
| 11 | `pkg/` | Package manager: fetch, resolve, lock | Planned |

---

## Stage 1 — Grammar (Done)

ANTLR4 lexer and parser grammars covering the full language surface.

**Files:** `PromiseLexer.g4` (166 lines), `PromiseParser.g4` (492 lines)

- 25 keywords, all operators including `:=`, `..`, `..=`, `?.`, `?:`, `=>`, `<-`
- Numeric literals with hex/octal/binary/underscore support
- String interpolation (`{expr}`), raw strings, triple-quoted multiline strings
- Type declarations with inheritance (`is`), enums/ADTs, generics
- Pattern matching, error handling (`?`, `!`, error handlers)
- Concurrency (`go`, `<-`), generators (`yield`, `yield*`)
- Meta annotations (backtick syntax)
- 31 test fixtures (16 valid, 15 invalid)

## Stage 2 — AST Builder (Done)

Complete CST → AST conversion via ANTLR visitor pattern.

**Files:** 17 Go files, ~5K LOC, 2163 lines of tests

- Declaration nodes: `TypeDecl`, `EnumDecl`, `FuncDecl`
- Statement nodes: `VarDecl`, `Assignment`, `Return`, `Raise`, `If`, `For`, `While`, `Match`, `Yield`
- Expression nodes: all literals, identifiers, calls, member access, operators, lambdas, go expressions
- Type reference nodes: named, tuple, function, array, slice, optional, reference types
- Pattern nodes: match patterns, is patterns, destructuring
- Source position tracking, AST pretty-printer

## Stage 3 — Type System (Done)

Core type representations and scope management.

**Files:** 14 Go files, ~2.2K LOC, 1100 lines of tests

- `Named` types with fields, methods, multiple inheritance, abstract tracking
- `Enum` types with variants (positional/named fields), generic support
- `Signature` function types with receiver, params, result, error flag
- Container types: `Tuple`, `Array`, `Slice`, `Map`
- Reference types: `Optional` (`T?`), `SharedRef` (`T&`), `MutRef` (`T~`), `Pointer` (`T*`)
- `TypeParam` with constraints, generic instantiation (`Instance`)
- Lexical `Scope` with parent chain, `Universe` scope with built-in primitives and generic types
- Four-struct placement model (`value`, `instance`, `variant`, `type`)

## Stage 4 — Semantic Analysis (Done)

Four-pass analysis: declare → define → check → verify.

**Files:** 12 Go files, ~3.5K LOC, 67 tests

- **Pass 1 (declare):** Registers all type/enum/func names into file scope with forward-reference support
- **Pass 2 (define):** Resolves parent types, fields, method signatures, type parameter constraints; populates `Named` and `Enum` structures
- **Pass 3 (check):** Type-checks function/method bodies
- **Pass 4 (verify):** Missing-return analysis for non-void functions/methods
- Type inference for `:=` declarations
- Expression type checking: literals, identifiers, binary/unary ops, calls, member access, indexing, optional chaining, `is` expressions, casts, error propagation/unwrap/handlers, if/match expressions, lambdas, go expressions, unsafe blocks
- Statement checking: var decls (typed/inferred/destructure), assignments (simple + compound), return, raise, if (regular + unwrap), while (regular + unwrap), for-in, classic for, infinite loops, break/continue validation, yield/yield*
- Constructor calls with named field validation
- Enum variant access and constructor signatures
- Operator dispatch via method lookup on named types
- **Map literals** resolve to `Map[K, V]` type with key/value consistency checking
- **Range operators** (`..`, `..=`) resolve to `Range` type
- **Go expressions** resolve to `Task[T]` with inner type inference
- **Receive operator** (`<-`) extracts `T` from `Task[T]` or `Channel[T]`
- **Map indexing** returns `V?` (optional) for `Map[K, V]`
- **For-in** supports `Slice`, `Array`, `Map`, `Range`, and `String` iteration
- **Match exhaustiveness** checking for enum types (variant coverage) and non-enum types (wildcard required)
- **Missing return** detection across if/else chains, match expressions, and infinite loops
- Error reporting with source positions

---

## Stage 5a — Generic Type Substitution (Done)

Type substitution engine and integration into the semantic checker.

**Files:** `types/subst.go` (new), updates to `sema/expr.go`, `sema/resolve.go`, `sema/stmt.go`, `sema/exhaust.go`, `sema/info.go`

- **Substitution engine** (`types/subst.go`): `Substitute(typ, subst)` recursively replaces TypeParam with concrete types across all type kinds (Named, Enum, Instance, Signature, Optional, Ref, Pointer, Tuple, Array, Slice, Map)
- **Field access on Instance**: `box.value` where `box: Box[int]` resolves `T` → `int` via substitution
- **Method calls on Instance**: method signatures substituted — params and return types use concrete types
- **Constructor calls on Instance**: `Box[int](value: 42)` validates field types with substitution
- **Operator dispatch on Instance**: binary/unary operators resolved through origin type with substitution
- **Constraint validation**: type arguments checked against TypeParam constraints at instantiation
- **Expression-context instantiation**: `Box[int]` in expression context (parsed as IndexExpr) reinterpreted as generic instantiation for single-type-arg generics
- **Instance tracking**: `Info.Instances` records all concrete instantiations for later monomorphization
- **Exhaustiveness for generic enums**: `Option[int]` match checks work via Instance → Enum extraction
- **Optional chaining on Instance**: `box?.value` resolves member through substitution
- **For-in on Iter/Stream instances**: `Iter[T]` iteration yields `T`
- **Known limitation**: multi-arg generics (e.g., `Pair[int, string]`) only work in type annotation position (function params, variable types), not in expression context — grammar allows only single expression inside `[]`

---

## Stage 5b — Sema Completion (Next)

Remaining semantic analysis features.

### Use declarations

AST support exists (`UseDecl` with Alias/Path, `File.Uses`), grammar works (`use io "std/io"`), but sema ignores `file.Uses` entirely.

| Task | Description | Files |
|------|-------------|-------|
| Add `Module` object type | New types.Object representing an imported module alias | `types/object.go` |
| Process `file.Uses` in Pass 1 | Create Module objects and insert aliases into file scope | `sema/decl.go` |
| Module member access | Detect `alias.name` in checkMemberExpr; report "module not loaded" | `sema/expr.go` |
| Duplicate alias detection | Module alias conflicts with other declarations caught by existing insert() | already works |

**Note:** Actual module loading deferred to Stage 9. Stage 5b creates placeholders only.

### Multi-constraint resolution

Currently `resolveTypeParamConstraints` only uses `ap.Constraint[0]`. AST supports `T: A + B` via `TypeParam.Constraint []TypeRef`.

| Task | Description | Files |
|------|-------------|-------|
| Add `constraints []Type` to TypeParam | Store all resolved constraints, not just first | `types/typeparam.go` |
| Resolve all constraints | Loop over `ap.Constraint` slice in define pass | `sema/decl.go` |
| Validate all constraints | Check type arg satisfies ALL constraints at instantiation | `sema/resolve.go` |
| Update AssignableTo | TypeParam assignable to any of its constraints | `types/equal.go` |

### Iter[T] and Stream[T] population

Currently empty generic stubs in Universe. Need abstract methods for interface checking and for-in support.

| Task | Description | Files |
|------|-------------|-------|
| Iter[T].next() T? | Abstract method returning optional element | `types/universe.go` |
| Stream[T].next() Task[T?] | Abstract async next method | `types/universe.go` |
| For-in via Iter | Check `next()` method existence on iterable types | `sema/stmt.go` |

### Unreachable code detection

| Task | Description | Files |
|------|-------------|-------|
| Detect dead code | After return/raise/break/continue, flag following statements | `sema/stmt.go` |
| Report warnings | "unreachable code after return/raise/break/continue" | `sema/stmt.go` |

### Match pattern bindings

Match arm pattern bindings (e.g., `Some(v) => v`) are not inserted into scope — patterns are validated but bindings aren't available in the arm body. This blocks using destructured enum values in match results.

| Task | Description | Files |
|------|-------------|-------|
| Insert pattern bindings | Open scope per arm, insert bindings from ShortDestructure/EnumDestructure patterns | `sema/expr.go` |
| Type bindings from substitution | For generic enum Instance, substitute variant field types for binding types | `sema/expr.go` |

---

## Stage 6 — Borrow Checker (Planned)

Ownership and lifetime analysis.

- Move semantics: track ownership transfers, error on use-after-move
- Borrow rules: at most one `~` (mutable) OR any number of `&` (shared) at a time
- Lifetime inference: determine reference validity scopes
- Lifetime elision rules (function params → result)
- Validate raw pointer (`*`) usage only inside `unsafe` blocks
- Integration with sema pass 3 (ownership errors alongside type errors)

## Stage 7 — Meta Annotations (Planned)

Process and validate meta annotations.

- Built-in metas: `abstract`, `native`, `raw`, `value`, `instance`, `variant`, `type`, `doc`, `deprecated`, `test`, `inline`, `packed`
- Validate meta targets (e.g. `abstract` only on methods, `raw` only on fields)
- Extract `doc` annotations into documentation model
- User-defined meta support (custom annotations that survive to codegen)

## Stage 8 — Code Generation (Planned)

LLVM IR generation from type-checked AST.

- Four-struct memory layout (value/instance/variant/type structs)
- Vtable generation and dispatch
- Ownership-aware memory management (stack vs heap, automatic free)
- Generic monomorphization at IR level
- Error handling codegen (dual-return `(value, error)`)
- Concurrency runtime integration (goroutine scheduling, channels)
- String interpolation lowering
- Operator overloading dispatch

## Stage 9 — Module System (Planned)

Module resolution and dependency management.

- URL-based module identity with version in path
- `use alias "url"` import processing
- Dependency graph construction from source
- Flat directory layout (no required `src/`)
- Cycle detection

## Stage 10 — CLI (Planned)

Full command-line interface.

- `promise build` — compile to binary
- `promise run` — compile and execute
- `promise check` — type-check only (exists as prototype)
- `promise test` — discover and run `test` meta-annotated functions
- `promise fmt` — code formatter

## Stage 11 — Package Manager (Planned)

Dependency fetching and resolution.

- Fetch from git/HTTP URLs
- Lockfile generation and deterministic builds
- Diamond dependency resolution
- Cache management
