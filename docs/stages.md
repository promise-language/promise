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
| 5b | `compiler/internal/sema/` | Match bindings, unreachable code, multi-constraint, Iter/Stream, use decls | Done |
| 6a | `compiler/internal/ownership/` | Move semantics, use-after-move, copy exemption, borrow conflicts, unsafe pointer | Done |
| 6b | `compiler/internal/ownership/` | Borrow tracking, implicit coercion, return safety | Done |
| 7 | `compiler/internal/sema/` | Meta annotation processing and validation | Done |
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

## Stage 5b — Sema Completion (Done)

Completes remaining semantic analysis features before ownership checking.

**Files:** Updates to `sema/expr.go`, `sema/check.go`, `sema/decl.go`, `sema/resolve.go`, `sema/builtins.go`, `types/typeparam.go`, `types/equal.go`, `types/object.go`; ~23 new tests

- **Match pattern bindings**: `Some(v) => v` works — scope opened per arm, bindings inserted from ShortDestructure/EnumDestructure/Name/TypeBinding patterns. For generic enum Instance subjects, variant field types are substituted via `BuildSubstMap`.
- **Unreachable code detection**: `checkBlock` tracks dead-code state — statements after `return`, `raise`, `break`, `continue` flagged as unreachable. Recognizes if/else where both branches exit, exhaustive match with all arms exiting, and infinite loops without break.
- **Multi-constraint resolution**: `T: A + B` fully supported — `TypeParam.constraints []Type` stores all constraints, `resolveTypeParamConstraints` resolves the full constraint list, `validateConstraints` checks type args against ALL constraints, `AssignableTo` allows TypeParam assignment to any of its constraints.
- **Iter[T] and Stream[T] abstract methods**: `Iter[T].next() T?` and `Stream[T].next() Task[T?]` populated via `populateIterStream()` in builtins — enables iteration protocol interface checking.
- **Use declaration placeholders**: `Module` object type added, `file.Uses` processed in Pass 1 (alias reserved in scope), bare module reference reports "module not loaded" error. Actual module loading deferred to Stage 9.

---

## Stage 6a — Move Semantics & Ownership (Done)

Separate post-sema pass for ownership analysis.

**Files:** 7 Go files in `compiler/internal/ownership/`, ~790 LOC + 775 LOC tests, 44 tests (81% coverage)

- **Move tracking**: every value has one owner; assignment, function call (value params), constructor args, and return all transfer ownership. Use-after-move reported with source position.
- **Copy exemption**: primitives (`int`, `bool`, `f64`, `char`, `none`, `void`, all sized ints/floats) and references (`&T`, `~T`) are implicitly copied — never moved.
- **Resurrection**: assigning to a moved variable restores it to `Owned` state.
- **Conservative control flow**: if/else uses state merge (moved in either branch = moved after). Loops merge loop-body state with pre-loop state. Match merges all arm states.
- **Borrow conflict detection**: at call sites, detects when the same variable is passed as `~` (mutable borrow) alongside any other borrow. Multiple shared borrows OK.
- **Unsafe pointer validation**: `TypedVarDecl` with pointer type ref (`T*`) outside `unsafe` block reports error.
- **CLI integration**: ownership errors reported after sema in `--check` mode.
- **Known limitations**: pointer value construction isn't sema-supported (pointer tests use direct AST construction). Implicit borrow coercion addressed in Stage 6b.

---

## Stage 6b — Borrow Tracking & Return Safety (Done)

Cross-statement borrow tracking, implicit borrow coercion, and return reference safety.

**Files:** New `ownership/borrow.go` (~155 LOC); updates to `ownership/check.go`, `ownership/expr.go`, `ownership/stmt.go`, `types/equal.go`; 21 new ownership tests, 8 new types tests

- **Implicit borrow coercion**: `AssignableTo` extended with three rules — `T` → `T&` (shared), `T` → `T~` (mutable), `T~` → `T&` (mut-to-shared downgrade). Uses recursive `AssignableTo` for inheritance compatibility.
- **BorrowSet data structure**: tracks active borrows orthogonally to move state. Each `Borrow` records origin variable, kind (shared/mutable), borrower variable (empty for call-scoped), and source position. All methods nil-safe for backward compatibility.
- **Call-scoped borrows**: passing a variable to a borrow parameter (`string &s`, `string ~s`) creates a borrow that expires at statement boundary. Sequential borrows of the same variable work correctly.
- **Variable-scoped borrows**: when a function returning a ref type (`string&`, `string~`) has its result stored in a variable, pending call-scoped borrows are promoted to variable-scoped. The borrow persists until the borrower variable is reassigned or goes out of scope.
- **Move-while-borrowed detection**: `tryMove` checks for active borrows before marking a variable as moved.
- **Assignment-while-borrowed detection**: reassigning a borrowed-from variable reports an error. Reassigning a borrower variable expires its borrows.
- **Method receiver borrows**: calling a `&this` or `~this` method creates a borrow on the receiver variable.
- **Return reference safety**: returning a reference to a local (non-parameter) variable from a function with ref return type is an error.
- **Control flow propagation**: borrows are saved/restored/merged (union) across all control flow constructs (if/else, while, for-in, for, match, lambda, infinite loops) in parallel with move state.
- **Parameter borrow detection**: handles ANTLR grammar ambiguity where `string &s` parses as `typeRef=string&` (not separate refMod) by checking parameter types for `SharedRef`/`MutRef` in addition to `Ref()`.
- **Deferred**: explicit lifetime annotations, stored references in structs, full NLL last-use analysis, drop ordering, disjoint field borrows.

## Stage 7 — Meta Annotation Processing (Done)

Validates and processes built-in meta annotations, wiring them into the type system and ownership checker.

**Files:** New `sema/meta.go` (~170 LOC); updates to `sema/decl.go`, `sema/expr.go`, `sema/error.go`, `sema/info.go`, `types/named.go`, `types/field.go`, `types/enum.go`, `types/object.go`, `ownership/copy.go`; 21 new tests across sema + ownership

- **Meta target validation**: 18 built-in metas (`value`, `instance`, `variant`, `type`, `raw`, `abstract`, `native`, `copy`, `doc`, `deprecated`, `test`, `inline`, `packed`, `align`, `extern`, `serializable`, `public`, `unsafe`) mapped to allowed declaration targets. Unknown metas, wrong targets, and duplicates reported as errors.
- **`copy` meta**: Marks `Named`/`Enum` types as bitwise-copy, exempting them from move semantics. Transitive field validation ensures all fields of a `copy` type are themselves copy types. Ownership checker's `isCopyType` reads `Named.IsCopy()` / `Enum.IsCopy()`.
- **`doc` meta**: Extracts documentation strings from `\`doc("text")` annotations and stores on `Named`, `Field`, `Method`, `Func`, `Enum` types.
- **`deprecated` meta**: Stores deprecation messages. Usage warnings emitted when deprecated types, functions, fields, or methods are referenced in expressions.
- **`test` meta**: Tracks test functions in `Info.Tests` for future `promise test` runner.
- **Warning system**: `warnf` added to checker for non-fatal diagnostic messages (prefixed with "warning:").
- **Deferred metas**: `inline`, `packed`, `align`, `extern`, `serializable`, `public`, `unsafe` are validated for target correctness but processing deferred to later stages (codegen/module system).

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
