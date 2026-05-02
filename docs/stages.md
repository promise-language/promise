# Compiler Stages

Implementation stages for the Promise compiler pipeline. For language design, see [language-design.md](language-design.md).

## Overview

### Compiler Pipeline (Stages 1-8)

| Stage | Package | Description | Status |
|-------|---------|-------------|--------|
| 1 | `compiler/grammar/` | ANTLR4 lexer/parser from grammar | Done |
| 2 | `compiler/internal/ast/` | AST builder: parse tree → typed AST nodes | Done |
| 3 | `compiler/internal/types/` | Type system: Named, Enum, Signature, Scope, Universe | Done |
| 4 | `compiler/internal/sema/` | Semantic analysis: type checking, name resolution, returns, exhaustiveness | Done |
| 5a | `compiler/internal/sema/` | Generic type substitution, constraint validation, instance tracking | Done |
| 5b | `compiler/internal/sema/` | Match bindings, unreachable code, multi-constraint, Iterator/Stream, use decls | Done |
| 6a | `compiler/internal/ownership/` | Move semantics, use-after-move, copy exemption, borrow conflicts, unsafe pointer | Done |
| 6b | `compiler/internal/ownership/` | Borrow tracking, implicit coercion, return safety | Done |
| 7 | `compiler/internal/sema/` | Meta annotation processing and validation | Done |
| 8a | `compiler/internal/codegen/` | LLVM IR: primitives, control flow, functions | Done |
| 8b | `compiler/internal/codegen/` | Strings: representation, literals, concat, extern ABI | Done |
| 8c | `compiler/internal/codegen/` | User types: layout, constructors, fields, methods | Done |
| 8d | `compiler/internal/codegen/` | Enums: tagged unions, pattern matching, destructure bindings | Done |
| 8e | `compiler/internal/codegen/` | Error handling: raise, propagation, unwrap, handlers | Done |
| 8f | `compiler/internal/codegen/` | Generic monomorphization: type-specialized layouts and methods | Done |
| 8g | `compiler/internal/codegen/` | Containers: tuples, optionals, slices, maps, lambdas | Done |
| 8h | `compiler/internal/codegen/` | Optional patterns, string interpolation, expression completeness | Done |
| 8i | `compiler/internal/codegen/` | Char literals, container `.len`, string iteration, map compound assignment | Done |
| 8j | `compiler/internal/types/`, `sema/`, `codegen/`, `runtime/` | Unify compound types with Named types + collection/string methods | Done |
| 8k | `compiler/internal/codegen/`, `sema/`, `types/`, `runtime/` | Inheritance codegen, RTTI, is/as expressions | Done |
| 8L | `compiler/internal/codegen/`, `sema/`, `ast/` | Virtual dispatch (vtable indirect calls) | Done |
| 8m | `compiler/internal/ast/`, `sema/`, `codegen/` | `use` bindings: scoped resource lifetime with automatic `close()` | Done |
| 8n | `compiler/internal/sema/`, `codegen/`, `types/` | Constructors: `new`, `` `final ``, factory, `Self`, `super` | Done |
| 8o | `compiler/internal/sema/`, `codegen/`, `ownership/` | `drop()` methods: ownership-driven cleanup at scope exit | Done |

### Infrastructure (Stages 9-11)

| Stage | Package | Description | Status |
|-------|---------|-------------|--------|
| 9 | `compiler/internal/module/`, `sema/`, `codegen/`, `modules/std/` | Module system: visibility, qualified access, local imports, cross-module codegen, separate/incremental compilation, transitive deps, circular detection, remote git fetching, globally unique module identity | Phase 3 Done + Identity Redesign |
| 10 | `cmd/promise/` | CLI entry point (build, run, test, fmt, etc.) | Done (except `fmt`) |
| 11 | `pkg/` | Package manager: fetch, resolve, lock | Planned |

### Cross-cutting Work

| Area | Description | Status | Design Doc |
|------|-------------|--------|------------|
| Runtime migration | Move C runtime to codegen LLVM IR / pure Promise | Done (Phases 1-4) | [runtime-architecture.md](runtime-architecture.md) |
| Platform abstraction | PAL for macOS/Linux/Windows/WASM | Done (Phase 3). Windows W1 done (threading + linker). | [runtime-architecture.md](runtime-architecture.md), [windows-support.md](windows-support.md) |
| 1:1 Threading | `go`/`<-` with OS threads via PAL | Done (Phase 5a) | [runtime-architecture.md](runtime-architecture.md) |
| Channels | `channel[T]` with buffered/unbuffered send/receive/for-in | Done (Phase 5b) | [runtime-architecture.md](runtime-architecture.md) |
| M:N Scheduler | LLVM coroutines, GMP model, work stealing | Done (Phase 5c) | [runtime-architecture.md](runtime-architecture.md) |
| Operator dispatch | `[]`, `[]=`, `[:]`, `[:]=` as method-dispatched operators | Done | [subscript-slice-operators.md](subscript-slice-operators.md) |
| Naming conventions | PascalCase canonical names for all non-scalar types; lowercase sugar | Done | [standard-library.md](standard-library.md#naming-conventions) |
| C binding | Extern ABI coercion (`extern.go`), C header generation (`headergen.go`) | Done (dormant — header gen implemented but not exposed via CLI; original use case obsolete after C runtime migration) | [c-binding-architecture.md](c-binding-architecture.md) |
| Self-contained binary | Embed gzip-compressed LLVM tools via `go:embed` for release builds | Done (Phase 7f). Linux x86_64: fully static (musl). macOS arm64+amd64: embedded LLVM, requires Xcode CLT for SDK. | [runtime-architecture.md](runtime-architecture.md), [distribution.md](distribution.md#4-macos-notes) |
| Distribution | Release binaries, install script, GitHub Actions CI/release workflows | Planned | [distribution.md](distribution.md) |
| Windows support | Native MSVC ABI, Win32 PAL threading, lld-link, Windows SDK discovery | Phase W1 done (code). W2 pending (testing on Windows). | [windows-support.md](windows-support.md) |
| Yield generators | `stream[T]` functions with `yield`, LLVM presplit coroutines, `for-in` consumption | Done | — |
| Structural interfaces | Relaxed matching (extra optional/default params, non-failable→failable, T→T?), adapter thunks, abstract factory methods with implicit Self, generic factory patterns (`T.parse(data)`) | Done | [language-design.md](language-design.md#structural-interface-satisfaction) |
| Documentation system | `promise doc` command: extract `doc()` meta tags, emit markdown to stdout, `-signatures` compact mode, `-std` for stdlib reference | Phase 1 done (single-file doc, `-public`/`-all`/`-signatures`/`-o` flags, param/variant doc propagation, `DeclareAndDefine` early-exit sema) | [documentation-proposal.md](documentation-proposal.md) |

---

## Stage 1 — Grammar (Done)

ANTLR4 lexer and parser grammars covering the full language surface.

**Files:** `PromiseLexer.g4` (166 lines), `PromiseParser.g4` (492 lines)

- 25 keywords, all operators including `:=`, `..`, `..=`, `?.`, `?:`, `=>`, `<-`
- Numeric literals with hex/octal/binary/underscore support and typed suffixes (`u8`, `i32`, `f64`, etc.)
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
- **Map literals** resolve to `map[K, V]` type with key/value consistency checking
- **Range operators** (`..`, `..=`) resolve to `Range[T]` generic value type
- **Go expressions** resolve to `task[T]` with inner type inference
- **Receive operator** (`<-`) extracts `T` from `task[T]` or `channel[T]`
- **Map indexing** returns `V?` (optional) for `map[K, V]`
- **For-in** supports vector, array, `map` (single binding yields `(K,V)` tuple; `for k, v in m` yields separate key/value), `Range[T]`, `string`, and `channel[T]` iteration
- **Match exhaustiveness** checking for enum types (variant coverage) and non-enum types (wildcard required)
- **Missing return** detection across if/else chains, match expressions, and infinite loops
- Error reporting with source positions
- **Agent-friendly diagnostics**: Property-called-as-method detection (`v.len()` → `'len' is a property on int[], not a method — remove ()`)
- **Method placement**: `` `global `` (namespace-scoped, no `this`/`Self`, non-generic only), `` `mono `` (per-monomorphization, no `this`, `Self` available), `` `factory `` (constructor pattern, implies `` `mono `` placement)

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
- **Expression-context instantiation**: `Box[int]` and `Pair[int, string]` in expression context (parsed as IndexExpr with ExtraIndices) reinterpreted as generic instantiation; works for types, enums, and generic functions with any number of type parameters
- **Instance tracking**: `Info.Instances` records all concrete instantiations for later monomorphization
- **Exhaustiveness for generic enums**: `Option[int]` match checks work via Instance → Enum extraction
- **Optional chaining on Instance**: `box?.value` resolves member through substitution
- **For-in on iter/stream instances**: `iter[T]` iteration yields `T`

---

## Stage 5b — Sema Completion (Done)

Completes remaining semantic analysis features before ownership checking.

**Files:** Updates to `sema/expr.go`, `sema/check.go`, `sema/decl.go`, `sema/resolve.go`, `sema/builtins.go`, `types/typeparam.go`, `types/equal.go`, `types/object.go`; ~23 new tests

- **Match pattern bindings**: `Some(v) => v` works — scope opened per arm, bindings inserted from ShortDestructure/EnumDestructure/Name/TypeBinding patterns. For generic enum Instance subjects, variant field types are substituted via `BuildSubstMap`.
- **Unreachable code detection**: `checkBlock` tracks dead-code state — statements after `return`, `raise`, `break`, `continue` flagged as unreachable. Recognizes if/else where both branches exit, exhaustive match with all arms exiting, and infinite loops without break.
- **Multi-constraint resolution**: `T: A + B` fully supported — `TypeParam.constraints []Type` stores all constraints, `resolveTypeParamConstraints` resolves the full constraint list, `validateConstraints` checks type args against ALL constraints, `AssignableTo` allows TypeParam assignment to any of its constraints.
- **iter[T] and stream[T] abstract methods**: `iter[T].next() T?` and `stream[T].next() task[T?]` populated via `populateIterStream()` in builtins — enables iteration protocol interface checking.
- **Use declarations**: `Module` object type added, `file.Uses` processed in Pass 1 (alias reserved in scope). Module scope resolution, qualified access (`mod.func()`, `mod.Type`), visibility enforcement, and local module loading implemented in Stage 9 Phase 1.

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
- **Deferred metas**: `inline`, `packed`, `align`, `extern`, `serializable`, `unsafe` are validated for target correctness but processing deferred to later stages (codegen/module system). `` `public `` is now processed in Stage 9 (module visibility).

## Stage 8a — LLVM Codegen: Primitives & Control Flow (Done)

Type-system-driven LLVM IR generation for primitive types, arithmetic, control flow, and functions.

**Files:** 6 new Go files in `compiler/internal/codegen/` (~900 LOC), `runtime/runtime.c` (7 LOC); updated `cmd/promise/main.go`, `grammar/PromiseParser.g4`; 32 codegen tests

- **Architecture**: `github.com/llir/llvm` (pure Go) builds IR in-memory, serializes to `.ll` text, compiled to native binary via `clang`.
- **Type-system-driven dispatch**: operators are methods on `*types.Named`, resolved via `LookupMethod(op)` → `IsNative()` → native emitter dispatch table. Codegen never hardcodes "if int and +, emit add."
- **TypeCategory classification** (`types.go`): types grouped into `CatSignedInt`, `CatUnsignedInt`, `CatFloat`, `CatBool`. Single comparison point against universe type singletons.
- **Native emitter table** (`native.go`): maps (TypeCategory, operator) → LLVM instruction emitter. Signed uses `sdiv`/`srem`/`slt`, unsigned uses `udiv`/`urem`/`ult`, float uses `fadd`/`fdiv`/`fcmp`, etc.
- **Short-circuit `&&`/`||`**: intercepted at AST level with phi-node branching (control flow, not single instruction).
- **Variable handling**: alloca + mem2reg strategy — every local gets `alloca`, reads use `load`, writes use `store`. LLVM's `mem2reg` pass promotes to SSA.
- **Two-pass compilation** (`compiler.go`): pass 1 declares all functions, pass 2 generates bodies.
- **Extern functions**: `funcDecl` with `(block | SEMI)` grammar — bodyless functions mapped to runtime print functions.
- **C runtime** (`runtime/runtime.c`): `promise_print_int`, `promise_print_f64`, `promise_print_bool`, `promise_panic`.
- **CLI**: `promise build file.pr [-o output]` and `promise run file.pr` compile through the full pipeline (parse → sema → ownership → codegen → clang).
- **Scope**: `int`/`i8`–`i64`/`uint`/`u8`–`u64`/`f32`/`f64`/`bool`, all arithmetic/comparison/logical ops, if/else, while, for-in (range), classic for, infinite loop, break/continue, function calls, compound assignment.

### Deferred sub-stages

- Ownership-aware memory management (drop) → Stage 8o
- Concurrency: `go`/`<-task` → Done (Phase 5a); `channel[T]` → Done (Phase 5b); M:N scheduler with select, preemption, panic recovery, GOMAXPROCS, sched stats → Done (Phase 5c)

## Stage 8b — Strings (Done)

String type codegen: representation, literals, concatenation, equality, extern ABI packing.

**Files:** Updates to `codegen/layout.go`, `codegen/compiler.go`, `codegen/expr.go`, `codegen/extern.go`, `codegen/types.go`, `codegen/headergen.go`; new `runtime/runtime_string.c`; 18 string-related tests

- **Internal representation**: Strings are `i8*` internally — opaque pointer to heap-allocated `promise_string_i` instance struct. `llvmNamedType(TypString)` returns `i8Ptr`.
- **String layout** (`layout.go`): Four-struct model with flexible array member. Instance struct `{ promise_string_m* _variant, i64 len, [0 x i8] data }` stores length + inline UTF-8 data. Value struct `{ i8* _vtable, promise_string_i* _instance }` is a lightweight handle. No `raw` field.
- **String literals** (`expr.go`): Compile to global constant + `call @promise_string_new(ptr, len)` → `i8*`. Escape sequences resolved at compile time (`\n`, `\t`, `\{`, etc.).
- **String concatenation**: `"a" + "b"` dispatches to `@promise_string_concat(i8*, i8*)` → `i8*` via native string operator path.
- **String equality**: `==` dispatches to `@promise_string_eq(i8*, i8*)` → `i1`. `!=` is `xor(eq, 1)`.
- **Extern ABI packing** (`extern.go`): `packString` wraps `i8*` → `%promise_string_v { null_vtable, bitcast(i8* → string_i*) }` via `insertvalue`. `unpackString` extracts field 1 + bitcasts back to `i8*`.
- **C runtime** (`runtime_string.c`): `promise_string_new` (malloc + memcpy), `promise_string_concat`, `promise_string_eq`, `promise_print_string` (fwrite). No null terminator — uses `len` field exclusively.
- **Header generation**: String layout always emitted (built-in type). Instance struct uses C99 flexible array member `char data[]`.
- **Scope**: Literals, variables, concatenation (`+`), equality (`==`, `!=`), extern passing/returning, empty strings.
- **Deferred**: Slicing, Unicode normalization. Methods (`.len`, `.contains`), string interpolation completed in later stages.

## Stage 8c — User Types (Done)

User-defined type codegen: four-struct layout, constructors, field access/assignment, method declaration/definition/calls, `this` keyword, extern ABI.

**Files:** Updates to `codegen/layout.go`, `codegen/compiler.go`, `codegen/expr.go`, `codegen/stmt.go`, `codegen/extern.go`; 20 user type tests (90 total codegen tests)

- **Internal representation**: User types are `i8*` internally — opaque pointer to heap-allocated `promise_T_i` instance struct. Same as strings. `llvmNamedType` returns `i8Ptr` via the default case.
- **Type layout** (`layout.go`): `computeUserTypeLayout` creates four LLVM struct types. Instance struct holds `{ promise_T_m* _variant, field1, field2, ... }` with field types from `llvmType()` (i64 for int, i8* for strings/user types, etc.). Value struct is `{ i8* _vtable, promise_T_i* _instance }` — no user fields. `InstanceFieldIndex` maps field names to GEP indices (1-indexed, `_variant` at 0). `InstancePtrType` caches the pointer-to-instance type.
- **C header field types**: Primitives use raw C types (`int64_t`, `uint8_t`); strings and user types use `void*` to avoid forward-declaration ordering issues.
- **Constructors** (`expr.go`): Detected when `info.Types[callee]` is `*types.Named`. Heap-allocate via `malloc(sizeof)` using GEP-from-null trick, bitcast to `%T_i*`, zero-initialize `_variant`, store named args by field index, zero-initialize unprovided fields. Returns `i8*`.
- **Field access** (`expr.go`): `d.age` → load `i8*`, bitcast to `%T_i*`, GEP to field index, load field value.
- **Field assignment** (`stmt.go`): `d.age = 5` → same as access but store. Compound assignment (`d.age += 1`) loads current value, applies operator via type system dispatch, stores result.
- **Method declaration** (`compiler.go`): Two-pass. `declareTypeMethods` creates LLVM function stubs with mangled names (`TypeName.methodName`). Receiver (if present) is first `i8*` parameter. `defineTypeMethods` generates bodies with `this` alloca.
- **Method calls** (`expr.go`): `d.getAge()` → resolve method from target type, prepend receiver as first arg, call `@TypeName.methodName`.
- **`this` keyword** (`expr.go`): Inside methods, `this` stored as `c.locals["this"]` alloca for `i8*`. `genThisExpr` loads from it.
- **Extern ABI** (`extern.go`): `packUserType`/`unpackUserType` follow same pattern as strings — `{ null_vtable, bitcast(i8* → T_i*) }` via insertvalue/extractvalue.
- **Compilation order**: `computeLayouts` → `computeUserTypeLayouts` → `declareIntrinsics` → `declareExterns` → `declareTypeMethods` → `declareFuncs` → `defineTypeMethods` → `defineFuncs`.
- **Scope**: Type layout, constructors (named args), field read/write, compound field assignment, methods with receiver (`this`/`&this`/`~this`), method calls, nested user type fields, extern pack/unpack.
- **Deferred**: Vtable/virtual dispatch, inheritance (parent fields/methods), static method calls (`Type.method()`), operator overloading on user types, ~~non-instance field placements (`value`/`variant`/`type`)~~ pure value types done in Stage 8p, mixed placements and `variant`/`type` still deferred, default field values. Generic user types handled in Stage 8f.

## Stage 8d — Enums and Pattern Matching (Done)

Enum type codegen: tagged unions, fieldless enums, variant constructors, pattern matching with switch, destructure bindings.

**Files:** Updates to `codegen/layout.go`, `codegen/compiler.go`, `codegen/expr.go`, `codegen/types.go`; 20 enum-related tests

- **Fieldless enums**: Internal type is `i32` (tag only). Variant values are `i32` constants. `Color.Red` → `i32 0`.
- **Data enums**: Internal type is `{ i32, [N x i8] }` — tag + byte array union sized to the largest variant. Per-variant data structs (e.g., `{ double }` for `Circle(f64)`) are bitcast-overlaid onto the data area.
- **Variant constructors**: `Shape.Circle(3.14)` → alloca enum struct, store tag at index 0, bitcast data area to variant struct pointer, store fields via GEP.
- **Pattern matching**: `match` on enum generates LLVM `switch` on tag. Each arm branches to a dedicated basic block. Wildcard/name patterns use the default target.
- **Destructure bindings**: `Some(v) =>` extracts the data area, bitcasts to variant struct, loads fields into local allocas. Supports `EnumDestructureMatchPattern`, `ShortDestructureMatchPattern`, and `NameMatchPattern` (binding the whole subject).
- **Enum layout** (`layout.go`): `computeEnumLayout` computes tag map, per-variant data struct types, max data size. Four-struct ABI model maintained (type/variant/instance/value structs) for future extern compatibility.
- **Scope**: Fieldless enums, data enums with positional fields, variant values, variant constructors, match with switch, destructure bindings, wildcard/name patterns.
- **Deferred**: Named enum fields in constructors, enum methods, extern ABI pack/unpack for enums.

## Stage 8e — Error Handling (Done)

Error handling codegen: failable function declarations, raise statements, error propagation (`?`), forced unwrap (`!`), error handler expressions, typed error handlers.

**Files:** Updates to `codegen/compiler.go`, `codegen/expr.go`, `codegen/stmt.go`, `codegen/types.go`, `sema/expr.go`, `sema/stmt.go`, `sema/info.go`, `sema/meta.go`, `sema/decl.go`, `types/named.go`, `grammar/PromiseParser.g4`, `ast/expr.go`, `ast/visit_expr.go`, `modules/std/error.pr`; 17 error handling tests + 46 sema tests + 12 codegen tests + 40 e2e tests

- **Error base type**: `type error { string message; }` defined in `modules/std/error.pr`. Universe type reuse merges std fields into `TypError`. All error types inherit from `error` via `is error`.
- **Result struct**: Non-void `T!` → `{ i1, T, i8* }` (tag, ok value, error pointer). Void `void!` → `{ i1, i8* }` (tag, error pointer). Tag: `i1 false` = Ok, `i1 true` = Error.
- **Error values are `i8*`**: Error instance pointers stored in result struct. `raise` on user types extracts the instance pointer from the value struct `{vtable, instance}`.
- **Failable declarations**: Functions/methods with `CanError()` return the result struct. `declareFuncs`/`declareTypeMethods` wrap return type with `computeResultType`.
- **Return wrapping**: `genReturnStmt` wraps the value in an Ok result (`{ false, val, null }`) when inside a failable function.
- **Raise statement**: `genRaiseStmt` wraps the error in an Error result (`{ true, zero, errVal }`) and returns. Validates raised value inherits from `error` at compile time.
- **Error propagation** (`?`): `genErrorPropagateExpr` checks the tag via `condBr`. Error path extracts the `i8*` error, re-wraps in caller's result type, early-returns. Ok path extracts the value.
- **Forced unwrap** (`!`): `genErrorUnwrapExpr` panics on error via `promise_panic(i8*)` + `unreachable`. Ok path extracts the value.
- **Untyped error handler** (`? e { body }`): `genErrorHandlerExpr` branches to handler block, reconstructs error value struct `{vtable_ptr, instance_ptr}` for the binding, generates body, merges with phi node.
- **Typed error handler** (`? e is IoError { body }`): RTTI check via `promise_type_is` on the error instance's `_variant` pointer. Match path: reconstruct typed value struct, generate body. No-match path: propagate error (in failable functions) or panic (in non-failable functions).
- **Failable validation**: `FailableExprs` map in sema `Info` records call expressions with failable signatures. Error operators (`?`, `!`, `? handler`) validate at compile time that their inner expression is failable.
- **Error type restrictions**: Error types cannot have `drop()` methods (error values are not tracked for cleanup; allowing drop would silently leak resources). Generic error types (`type DataError[T] is error { T data; }`) are supported.
- **Auto-terminator**: Failable functions without explicit terminator return an Ok-wrapped zero value.
- **`genBlockValue` helper**: Generates a block and returns the last expression's value without double-generating. Used by `genErrorHandlerExpr` and `genIfExpr`.
- **`!` shorthand**: `foo()!` is equivalent to `foo() void!`. Grammar: `returnType: typeRef BANG? | BANG`. Works for both functions and methods.
- **Failable main**: `main()!` compiles main's body into a separate helper function `__promise_main_body` with the correct result struct return type. The coroutine calls it and converts errors to panics (`"unhandled error in main"`, exit code 1). Direct channel/select ops in failable main use thread-blocking mode (not coroutine parking) — failable main is intended for I/O error handling, not concurrency.
- **Scope**: Failable functions/methods, raise, `?` propagation, `!` unwrap, `? binding { body }` handlers, `? e is T { body }` typed handlers, void failables, generic error types.
- **Auto-propagation**: Naked failable calls in failable functions auto-propagate — codegen emits tag-check + early-return (same as explicit `?`). In non-failable functions, naked failable calls are a compile error: `"failable call must be handled with '?', '!', or an error handler"`. Tracked via `AutoPropagateExprs` map in sema `Info`.
- **Typed handler exhaustiveness**: Typed error handlers (`? e is T { }`) in non-failable functions require explicit handling of non-matching errors. Three options: `else` clause (`? e is T { } else { }`), `!` suffix (`? e is T { }!` — panics on nomatch), or making the function failable (propagates nomatch). In failable functions, unhandled nomatch auto-propagates. `else` supports optional binding: `? e is T { } else e { }`. `reconstructErrorValue` helper extracts vtable+instance from raw `i8*` error pointer.
- **Failable result capture**: `(val, err) := failableCall()` destructures a failable result into the success value and `error?` optional. Error path reconstructs error value struct from instance pointer; ok path yields absent optional. Tracked via `FailableDestructures` map in sema `Info`.
- **Deferred**: Failable extern functions (C ABI for errors). If-unwrap/while-unwrap completed in Stage 8h. Full type expressions in `is` patterns (generics, arrays, module-qualified — see Stage 8k deferred).

## Stage 8f — Generic Monomorphization (Done)

Generic function sema support and type-specialized code generation for all generic instantiations.

**Files:** New `codegen/mono.go` (~475 LOC); updates to `types/signature.go`, `types/subst.go`, `sema/decl.go`, `sema/check.go`, `sema/expr.go`, `sema/info.go`, `codegen/compiler.go`, `codegen/types.go`, `codegen/expr.go`, `codegen/stmt.go`; 6 new sema tests, 20 new codegen tests

**Part A — Sema: Generic functions/methods** (`types/`, `sema/`)
- **TypeParams on Signature** (`types/signature.go`): `typeParams []*TypeParam` field with `TypeParams()` accessor and `SetTypeParams()` setter. `substSignature` strips TypeParams when substituted.
- **Generic function definition** (`sema/decl.go`): `defineFunc` opens type-param scope, creates `TypeParam` objects, resolves constraints, stores on `Signature` via `SetTypeParams`.
- **Generic function body checking** (`sema/check.go`): `checkFuncDecl` opens type-param scope when `sig.TypeParams()` is non-empty, inserting TypeParam type names so `T` resolves during body checking.
- **Generic function calls** (`sema/expr.go`): `checkIndexExpr` detects `*types.Signature` with TypeParams → `instantiateGenericFunc`. Resolves type arg via `resolveTypeRef`, builds substitution map, substitutes signature (stripping TypeParams), records `FuncInstance`.
- **FuncInstance tracking** (`sema/info.go`): `FuncInstance{Func, TypeArgs, Sig}` struct records concrete generic function instantiations for codegen monomorphization.
- **Type reference resolution** (`sema/expr.go`): `resolveTypeRef` resolves expressions as type references (for `int` in `func[int]`), recording type and object on the AST node for codegen consumption.

**Part B — Codegen: Monomorphization** (`codegen/`)
- **Monomorphization, not type erasure**: Each concrete `Instance{Box, [int]}` gets a distinct LLVM layout with correctly-typed fields (`i64` for int, not `i8*`).
- **Naming convention**: `Box__int`, `Pair__int__string`, `Option__int` for types. `identity__int` for generic functions. Double-underscore separator — no collision with Promise identifiers.
- **Core infrastructure** (`mono.go`): `monoContext` struct, `monoName`/`typeArgSuffix`/`monoFuncName` mangling, `collectMonoInstances`/`collectMonoFuncInstances` deduplication. Layout computation: `computeMonoUserTypeLayout` (four-struct ABI with substituted field types), `computeMonoEnumLayout` (tagged union with substituted variant fields). Method/function codegen: `declareMonoMethods`/`defineMonoMethods`/`declareMonoFuncs`/`defineMonoFuncs` with `typeSubst`+`monoCtx` scoping and `defer`-based state cleanup.
- **Compiler integration** (`compiler.go`): New fields (`monoLayouts`, `monoEnumLayouts`, `typeSubst`, `monoCtx`). Dispatch helpers: `lookupTypeLayout`/`lookupEnumLayout` (handle Instance, monoCtx, regular types), `resolveTypeName` (mangled names for method dispatch). Generic origins skipped in existing passes; 14-step compilation pipeline.
- **Type resolution** (`types.go`): `resolveType` applies `typeSubst` first, then handles Instance (enum → mono layout, named → `i8Ptr`), then regular enums with `monoCtx` awareness. `llvmTypeSize` handles `StructType` and `ArrayType` for correct enum data area sizing.
- **Expression codegen** (`expr.go`): Instance constructor detection, `genConstructorCallMono` with `lookupTypeLayout`, `genFieldAccess` with layout-driven field types, `genMethodCall` with `resolveTypeName`, enum operations via `lookupEnumLayout`, `genGenericFuncCall` for `identity[int](42)` dispatch with `typeSubst` application.
- **Statement codegen** (`stmt.go`): `genMemberAssign` uses `lookupTypeLayout` and layout field types for both regular and monomorphic types.
- **Layout-driven field types**: All field load/store/zero-init operations use `layout.Instance.Fields[idx].LLVMType` instead of `llvmType(field.Type())`, which correctly handles TypeParam substitution.
- **Scope**: Generic user type instantiation (layout, constructor, field access/assignment, methods), generic enum instantiation (tagged union, variant values/constructors, pattern matching, destructure bindings), generic functions (single type parameter, void/non-void/failable), multiple instantiations of same generic.
- **Deferred**: Type argument inference (explicit type args only), extern ABI for generic types, C header generation for monomorphic types, container types (Array, Slice, Map, Tuple — Stage 8g).

**Part C — Generic Inheritance** (`types/`, `sema/`, `codegen/`)
- **ParentRef with type args** (`types/named.go`): `ParentRef{Named, TypeArgs}` struct replaces bare `*Named` parent pointers. `AddParent` accepts optional type args. All parent-walking methods (`AllFields`, `AllMethods`, `LookupMethod`, etc.) updated.
- **Assignability** (`types/equal.go`): `isNamedChildOfInstance` and `isInstanceChild` handle generic parent matching — direct type arg comparison and transitive resolution through generic intermediaries via intermediate Instance construction.
- **Sema: define + check** (`sema/decl.go`, `sema/expr.go`): `defineType` resolves `Instance` parents, extracts origin + type args, records instances for monomorphization. `buildParentSubstMap` rewritten as recursive `mergeParentSubstSema` — walks parent chain composing substitutions at each level. Constructor calls use composed substitution for inherited field type validation.
- **Codegen: layout + mono** (`codegen/layout.go`, `codegen/mono.go`): `buildParentFieldSubst` → recursive `mergeParentFieldSubst` for correct LLVM field types. `collectMonoInstances` discovers parent instances transitively. `mergeParentSubst` recursive for transitive chains.
- **Codegen: dispatch + RTTI** (`codegen/compiler.go`, `codegen/expr.go`, `codegen/rtti.go`): `resolveMonoParentName` builds full subst map for method name mangling through generic parents. `genVirtualMethodCall`/`genVirtualGetterCall` apply `vtableSubst` from Instance type args for correct LLVM return/param types. `emitVtableGlobal` falls back to mono parent names for inherited method lookup.
- **Scope**: Non-generic child of generic parent (`IntHolder is Holder[int]`), generic child forwarding params (`Wrapper[T] is Container[T]`), partial type arg application (`StringPair[V] is Pair[string, V]`), abstract generic parents, transitive 3-level chains, assignability (child→parent instance, instance→instance, transitive through generic intermediaries), method override, own+inherited fields/methods.
- **Tests**: 17 e2e tests (`test_generic_inheritance.pr`), 13 sema tests, 2 codegen IR tests.

## Stage 8g — Container Codegen (Done)

Codegen for container types (tuples, optionals, slices, maps) and capturing lambdas.

**Files:** Updates to `codegen/compiler.go`, `codegen/types.go`, `codegen/expr.go`, `codegen/stmt.go`; ~~new `runtime/runtime_map.c` (~205 LOC)~~ (superseded by `modules/std/map.pr`); 29 new tests (119 total codegen tests)

- **Tuples**: Value type, LLVM struct `{ T0, T1, ... }`. Literals via `insertvalue`, destructuring (`(a, b) := expr`) via `extractvalue`. Mixed-type tuples supported.
- **Optionals**: Value type, `{ i1, T }` struct. `none` = zeroinitializer, some = `{ true, val }`. `targetType` field on Compiler resolves contextual type for `NoneLit` (sema records `TypNone` but codegen needs `Optional(T)`). `lookupLocalType` detects `OptionalTypeRef` annotations and resolves declared types from sema scopes.
- **Elvis operator** (`?:`): `genElvis` extracts flag, condBr → some block (extract value) / none block (evaluate default), phi merge.
- **Optional wrapping**: Assigning `T` to `T?` variable auto-wraps via `wrapOptional` (insertvalue `{ true, val }`).
- **Slices / Array literals**: Heap-allocated `i8*` → `{ i64 len, i64 cap, [data...] }`. 16-byte header + inline elements. `genArrayLit` mallocs, stores header via GEP, stores elements via typed GEP past header. Both `*types.Slice` and `*types.Array` map to `i8Ptr`.
- **Slice indexing**: Bounds-checked with `icmp ult` (unsigned, catches negative indices). Out-of-bounds calls `promise_panic` + `unreachable`. Read via `genSliceIndex`, write via `genSliceIndexAssign` (supports compound assignment like `arr[i] += 1`).
- **Maps**: ~~Type-erased C runtime hash table (`runtime/runtime_map.c`)~~ — **Superseded.** Now a pure Promise self-hosted implementation in `modules/std/map.pr`: generic `Map[K: Hashable + Equal, V]` using open-addressing with `Slot[K, V]` enum (Empty/Tombstone/Used), FNV-1a key hashing via `.hash` property, 75% load-factor rehash. Methods: `[]`, `[]=`, `contains`, `remove`, `keys`, `values`, `get_or`, `pop`, `update`, `entries`, `merge`, `clear`. Monomorphized at codegen time.
- **Map indexing**: `m["key"]` returns `V?` (optional). Assignment via `m["key"] = val`. Both compile to monomorphized method calls on the Map instance.
- **For-in iteration**: `genForInStmt` dispatches on iterable type. Vectors: counter loop with bounds check per element. Maps: `genForInMap` calls monomorphized `keys()`/`values()`, iterates both vectors in parallel — single binding yields `(K, V)` tuple, two bindings (`for k, v in m`) yield separate key and value. Ranges: `genForInRange`. Strings: `genForInString` (UTF-8 codepoints). Channels: `genForInChannel`.
- **Lambdas (capturing)**: Anonymous LLVM functions (`.lambda.N`) with `i8* %env` as first parameter (uniform ABI). Fat pointer representation `{ i8*, i8* }` (fn ptr + env ptr) for all function values. Non-capturing lambdas use null env. Compiler state saved/restored (fn, block, locals, canError, scopeBindings, dropFlags). Handles both expression body (`|x| -> x + 1`) and block body (`|x| -> int { return x * 2; }`).
- **Lambda captures**: Sema capture analysis (`checkLambdaCapture`) detects outer-scope variable references via scope chain traversal. `Copy` types auto-captured by copy; non-`Copy` types require explicit `move` keyword. Captures recorded in `info.LambdaCaptures` (deterministic order via sorted names). Nested lambda capture propagation: inner captures from grandparent scopes automatically propagate to intermediate lambdas.
- **Lambda env struct**: Heap-allocated struct holding captured values (`malloc`). Captures loaded from enclosing scope allocas, stored into env fields. Inside lambda body, env is bitcast to typed pointer, fields extracted into local allocas. Move-captured droppable types registered for drop inside lambda body. Env struct freed at scope exit via `bindingFreeEnv` binding (drop-flag-guarded, null-checked `free()`).
- **Lambda calls**: `genCallExpr` detects local variables with `*types.Signature` type before regular function lookup. Loads fat pointer `{ i8*, i8* }`, calls `genIndirectCall` which extracts fn/env, bitcasts to typed function pointer with env-first ABI, calls with env as first arg.
- **Named function references**: When a named function is used as a first-class value (e.g., `f := add`), a thunk with env-first ABI is generated (`.thunk.add`) that forwards to the original function. Fat pointer uses `{ @.thunk.add, null }`.
- **Lambda ownership**: Move captures mark the variable as `Moved` in the enclosing scope. Captured variables are `Owned` inside the lambda body. Copy captures leave the original variable usable.
- **Intrinsics** (`compiler.go`): ~~7 new map runtime functions declared in `declareIntrinsics`~~ (superseded by self-hosted `modules/std/map.pr`). `lambdaCounter` and `targetType` fields added to Compiler.
- **Scope**: Tuple literals/destructure/return, optional none/some/wrapping/elvis, array literals, slice/array indexing (read/write/compound), for-in over slices/arrays/maps, map literals/indexing/assignment, capturing lambdas (expression/block body, indirect calls, copy/move captures, nested capture propagation, env allocation/cleanup, named function reference thunks).
- **Deferred**: ~~Slice growth (`.push()`)~~ done (Vector.push). ~~Container methods (`.contains`)~~ done (Vector.contains, string.contains). ~~`llvmTypeSize` struct alignment~~ fixed (`llvmTypeSizeWithPtr` now handles padding). String interpolation, if-unwrap/while-unwrap, optional chaining, and unsafe blocks completed in Stage 8h. Container `.len` completed in Stage 8i.
- **Fixed-size arrays** (`T[N]`): Stack-allocated `[N x T]` LLVM array type. Hint-based literal inference: `int[3] x = [1,2,3]` types the literal as `*types.Array`, bare `[1,2,3]` remains Vector. `genFixedArrayLit` allocas `[N x T]` and stores elements via GEP. `genArrayIndex`/`genArrayIndexAssign` emit bounds check (`icmp ult idx, N`) and GEP into the array. `.len` returns compile-time constant. `genForInArray` iterates with constant-bound loop. `genArrayBasePtr` returns alloca for identifiers, `genFieldPtr` for struct field access (MemberExpr), temp alloca for computed expressions. Copy semantics: arrays of copy-type elements are copy. Element count mismatch (`int[3] x = [1,2]`) is a sema error. Mutating vector methods (push/pop/remove) rejected on fixed arrays.

## Stage 8h — Optional Patterns, String Interpolation & Expression Completeness (Done)

Codegen for if-unwrap, while-unwrap, optional chaining, string interpolation, and unsafe blocks.

**Files:** Updates to `codegen/expr.go`, `codegen/stmt.go`, `codegen/compiler.go`, `ast/expr.go`, `ast/visit_expr.go`, `sema/expr.go`, `runtime/runtime_string.c`; 12 new tests (131 total codegen tests)

- **If-unwrap**: `if val := optExpr { }` — `genIfUnwrapStmt` extracts flag from `{ i1, T }` optional, condBr to then-block where inner value is extracted and bound to a scoped local. Optional else-block. Binding variable saved/restored to prevent scope leak.
- **While-unwrap**: `while val := optExpr { }` — `genWhileUnwrapStmt` with header/body/exit blocks. Optional re-evaluated each iteration in header. break/continue targets set correctly. Same scope-leak fix as if-unwrap.
- **Optional chaining**: `x?.field` — `genOptionalChainExpr` checks optional flag, accesses field on inner value in some-block, wraps result in `Optional(FieldType)`. None-block produces zeroinitializer. Phi merge at end. `genFieldOnValue` helper extracted for field access on raw values.
- **String interpolation** (cross-cutting):
  - **AST**: `StringInterp` gains `Expr` field (parsed expression from `{expr}` syntax).
  - **AST builder**: `parseInterpolationExpr` re-lexes/re-parses expression text via fresh ANTLR lexer/parser. `offsetExprPositions` recursively adjusts AST node positions to match original source locations.
  - **Sema**: StringLit case extended to type-check interpolation expressions.
  - **Runtime**: ~~`promise_int_to_string`, `promise_f64_to_string`, `promise_bool_to_string` conversion functions in `runtime_string.c` using `snprintf`~~ — now codegen-emitted LLVM IR (`defineIntToStringFunc`, `defineUintToStringFunc`, `defineBoolToStringFunc`, `defineCharToStringFunc` in compiler.go). f64→string uses `_f64_to_str` in `modules/std/format.pr` (pure Promise, no libc dep).
  - **Codegen**: `genStringLit` split into `genStaticString` (compile-time, no interpolation) and `genInterpolatedString` (runtime). `convertToString` handles all primitive types with sext/zext/fpext as needed. Parts concatenated via `promise_string_concat`. Both `promise_string_new` and `promise_string_concat` are codegen-emitted LLVM IR using `@llvm.memcpy` intrinsic.
  - **Intrinsics**: 14 functions defined as codegen LLVM IR in `declareIntrinsics`: `promise_string_new`, `promise_string_concat`, 5 conversion functions (`bool`, `int`, `uint`, `f64`, `char` to string), `promise_vector_with_capacity`, `promise_vector_push`, `promise_vector_pop`, `promise_string_trim`, `promise_string_split`, `promise_string_next_char`, `promise_type_is`.
- **Unsafe blocks**: `genUnsafeExpr` trivially generates block contents. Ownership analysis handles the "unsafe" semantics, not codegen.
- **Scope**: If-unwrap (with/without else), while-unwrap (with break/continue), optional chaining on user type fields, string interpolation with identifiers/literals/expressions/multiple parts, unsafe blocks.
- **All former deferrals resolved**: `is`/`as` expressions completed in Stage 8k. Generators (`yield`) completed in Phase 7c. Container methods (`.push`, `.pop`, `.contains`) completed. Container `.len` completed in Stage 8i. `go`/`<-task` concurrency completed in Phase 5a. `channel[T]` completed in Phase 5b. User type interpolation via `format(Writer ~w)!` now implemented.

## Stage 8i — Char Literals, Container `.len`, String Iteration & Map Compound Assignment (Done)

Codegen for char literals, `.len` property on all containers (string, slice, array, map), for-in over strings, and map compound assignment.

**Files:** Updates to `codegen/expr.go`, `codegen/stmt.go`, `codegen/compiler.go`, `codegen/types.go`, `codegen/native.go`, `sema/expr.go`, `sema/stmt.go`, `runtime/runtime_string.c`, `types/container.go`; 25 new tests (19 codegen → 227 total, 6 sema → 214 total)

- **Char literals**: `genCharLit` parses raw text including escape sequences (`\n`, `\t`, `\r`, `\b`, `\\`, `\'`, `\0`), returns i32 constant. `CatChar` classification added to `types.go` with signed i32 comparisons in `native.go`.
- **Container `.len` property**: Uniform read-only getter across all container types — `arr.len`, `m.len`, `s.len`. Vector reads i64 from heap header (GEP index 0). Map uses `get len` getter (pure Promise). String reads i64 from instance struct (GEP index 1). Both `string.len` and `Vector.len` are declared as `get len int` (read-only getter, no setter).
- **For-in over strings**: `genForInString` iterates UTF-8 codepoints via `promise_string_next_char` runtime function. Byte position tracked in i64 alloca, -1 sentinel for end. Index variable (`for i, ch in s`) supported with separate counter.
- **Map compound assignment**: `genMapCompoundAssign` reads current value via monomorphized `[]` method, unwraps optional with panic on missing key, applies operator, stores back via `[]=`. Sema fix unwraps Optional for operator lookup on map value type.
- **Char interpolation**: `convertToString` extended with `TypChar` case calling `promise_char_to_string` (UTF-8 encode).
- **Deferred**: Evaluation order bug in compound index assignment (RHS evaluated before LHS target/key — see comment in `genMapCompoundAssign`).

## Stage 8k — Inheritance Codegen, RTTI & is/as Expressions (Done)

Codegen for inherited field layouts, static method dispatch through inheritance chains, runtime type information (RTTI), and `is`/`as` expressions.

**Files:** New file `codegen/rtti.go`; updates to `types/named.go`, `codegen/layout.go`, `codegen/mono.go`, `codegen/compiler.go`, `codegen/expr.go`, `sema/expr.go`, `sema/decl.go`, `runtime/runtime.c`; 27 new tests (24 codegen → 251 total, 5 sema → 219 total)

- **Sema validation**: Prevent abstract type instantiation (`IsAbstract()` check in `checkConstructorCall` and `checkInstanceConstructorCall`, including generic instances). Reject multiple concrete parents (more than one parent with fields, checked transitively via `AllFields()`). Allow multiple fieldless/abstract parents. Enum variant names accepted in `is` patterns when subject is an enum type (including generic enum instances).
- **Inherited field layout**: `AllFields()` on Named gathers parent fields first (depth-first, single concrete parent chain), then own fields. Child fields shadow parent fields with the same name. `computeUserTypeLayout` and `computeMonoUserTypeLayout` use `AllFields()` so child instance structs include inherited fields at prefix-compatible indices. Topological ordering ensures parent layouts are computed before children.
- **Static method dispatch**: `resolveMethodOwner` walks the parent chain to find which type actually defines a method. `genMethodCall` uses the defining type's name for mangling (e.g., `Animal.greet` when called on Dog). Monomorphized method names preserved for generic types.
- **RTTI infrastructure**: Each non-generic Named type gets a unique i32 type ID. Type info globals (`@promise_typeinfo_TypeName`) store `{ i32 type_id, i32 num_parents, [N x i32] parent_ids }` with transitive parent IDs. Constructors store the type info pointer in the `_variant` slot (index 0) instead of null. Runtime function `promise_type_is(variant_ptr, expected_id)` checks type ID and parent IDs.
- **is expressions**: `x is present` → `extractvalue` i1 flag from optional. `x is absent` → extract + xor negate. `c is Variant` → extract enum tag, `icmp eq`. `a is Dog` → load `_variant` pointer, call `promise_type_is`, convert i32→i1.
- **as expressions**: `a as Dog` (safe) → RTTI check, branch to `cast.some` (wrap in Optional) or `cast.none` (zeroinitializer), phi merge. `a as! Dog` (force) → RTTI check, branch to `cast.ok` or `cast.panic` (calls `promise_panic`).
- **Deferred**: Virtual dispatch (vtable — completed in Stage 8L), destructure is-patterns (`x is Dog(name)`), generic type RTTI, full type expressions in `is` patterns and typed error handlers. Both `if x is TYPE` and `? e is TYPE` currently only accept bare `IDENT`. Need to support `typeRef` (generics like `DataError[int]`, arrays like `int[]`, optionals like `Foo?`, module-qualified like `std.Error`). The `typeRef` grammar rule already supports these forms — requires updating `pattern` rule, `ErrorHandlerExpr` grammar, AST, sema type resolution, and codegen RTTI for monomorphized type IDs.

## Stage 8j — Unify Compound Types with Named Types + Collection Methods (Done)

Promoted `slice[T]` and `map[K,V]` from structural placeholder types (`*types.Slice`, `*types.Map`) to real Named types in the universe scope, represented as `Instance{TypSlice, [T]}` and `Instance{TypMap, [K, V]}`. Method/field lookup flows through the existing `resolveInstanceMember` → `LookupMethod` → type substitution path — the same path used for user-defined generic types.

**Type System Changes:**
- Added `TypSlice = defGeneric("slice", "T")` to universe scope
- Deleted `Slice` and `Map` structs from `types/container.go`
- `NewSlice(elem)` and `NewMap(key, val)` now return `*Instance`
- Added helper functions `IsSlice`, `AsSlice`, `IsMap`, `AsMap` for clean migration
- `Instance.String()` overridden so `slice[int]` displays as `int[]`
- Deleted `case *Slice:` and `case *Map:` from `equal.go` and `subst.go`

**Native Methods Registered in `builtins.go`:**
- **slice[T]**: `get len` getter, `new(int capacity = 16)`, `push(T)`, `pop() → T?`, `contains(T) → bool`, `remove(int)`
- **map[K,V]**: ~~`get len` getter, `contains(K) → bool`, `remove(K) → bool`, `keys() → K[]`, `values() → V[]`~~ (superseded by self-hosted `modules/std/map.pr` — all methods are pure Promise, only `[]`/`[]=` dispatch through codegen for subscript syntax)
- **string**: `get len` getter, `contains(string) → bool`, `starts_with(string) → bool`, `ends_with(string) → bool`, `index_of(string) → int?`, `trim() → string`, `split(string) → string[]`

**Runtime:**
- New `runtime_slice.c`: push (with realloc growth), pop, contains, remove
- ~~Updated `runtime_map.c`: tombstone support~~ (superseded by self-hosted `modules/std/map.pr` with `Slot[K,V]` enum: Empty/Tombstone/Used)
- Updated `runtime_string.c`: ~~contains, starts_with, ends_with, index_of~~ (migrated to pure Promise), trim, split

**Sema/Codegen Migration:**
- ~35 `case *types.Slice:` / `case *types.Map:` switch cases migrated to `AsSlice`/`AsMap` helpers
- `.len` removed as special case — now a real field lookup
- Array delegates field/method lookup to TypSlice (rejects mutating methods like `push`/`remove`)
- **`T[]` in expression position**: Grammar extended with `expression LBRACKET RBRACKET` postfix rule (`sliceTypeExpr`). New `SliceTypeExpr` AST node desugared to `Vector[T]` in sema via `checkSliceTypeExpr` (mirrors `resolveType` for `SliceTypeRef`). Validates inner expression is a type reference (IdentExpr → TypeName, IndexExpr, MemberExpr, or chained SliceTypeExpr) — rejects variables and literals. Enables `int[]()`, `int[](capacity: N)`, `int[][]()`, `int[].filled(...)` as constructor/factory calls equivalent to `Vector[int]()` etc. Tests in `tests/e2e/test_slice_type_expr.pr`.

## Stage 8L — Virtual Dispatch (Done)

Vtable generation and indirect method calls for inheritance and interface types.

- **Vtable layout**: Each Named type with virtual methods gets a vtable global containing function pointers ordered by `AllVirtualMethods()`. Child types extend parent vtable layout — parent slots at prefix positions, child slots appended. Getter and setter with the same name occupy distinct vtable slots via `methodSlotKey()` — setters keyed as `name$set`, getters/methods keyed by bare name.
- **View-specific vtables**: When a value crosses an interface/parent boundary (assignment, function argument, return), the compiler generates a view vtable with slots ordered by the target type's method layout. The value struct's vtable pointer is swapped at the coercion point.
- **Indirect calls**: Method calls on interface-typed or parent-typed variables use vtable-indexed indirect calls (`call` through GEP'd function pointer) instead of direct `call @TypeName.methodName`. Includes virtual dispatch for getters via `genVirtualGetterCall` and setters via `genVirtualSetterCall`.
- **Setter mangling**: Setter IR functions use `TypeName.methodName$set` to avoid collision with same-name getters in the `c.funcs` map. `mangleMethodName()` helper used consistently across compiler passes (forward decl, body def, monomorphization, vtable emission, call sites).
- **Abstract satisfaction**: `IsAbstract()` and `allAbstractMethods()` use `methodSlotKey` for matching — a concrete setter does not satisfy an abstract getter (and vice versa). Both must be independently implemented.
- **Constructor vtable assignment**: Constructors store the type's vtable pointer into the value struct's slot 0 (currently stores RTTI in `_variant`).
- **Kind-aware method lookup**: All 4 call sites that resolve AST `MethodDecl` to `types.Method` (sema check, sema returns, ownership, codegen) dispatch through getter/setter/method-aware lookup instead of name-only `LookupAnyMethod`.
- **Tests**: 301 codegen tests (including virtual getter dispatch, getter override dispatch, direct getter preservation, same-name getter+setter vtable, view vtable getter+setter, generic getter+setter, compound assignment getter+setter).
- **Deferred**: Devirtualization optimization (when concrete type is known at compile time, use direct call).

## Stage 8m — `use` Bindings: Scoped Resource Lifetime (Done)

Scoped resource management via `use` variable declaration modifier. When the enclosing scope exits, the compiler automatically calls `close()` on the bound variable.

**Grammar:** `useVarDecl: USE IDENT WALRUS expression SEMI;` added as first `statement` alternative.

**AST:** `UseVarDecl` node with `Name string`, `Value Expr` fields. Visitor, printer, and statement dispatch added.

**Sema:** `checkUseVarDecl` type-checks the value expression, extracts `Named` type (handles `Named` and `Instance`), verifies `close()` method exists via `LookupMethod("close")` (structural satisfaction — no `is Closer` required), and inserts the variable into scope. 5 sema tests.

**Codegen:** `useBinding` struct tracks alloca, close function (direct dispatch), named type (virtual dispatch), and value type. `genUseVarDecl` allocates the variable and resolves direct/virtual dispatch for `close()`. `emitUseCloseCalls(fromIdx)` emits close calls in LIFO order at all scope exit points:
- **Fall-through:** `genBlock` emits close calls after generating all statements
- **Return/Raise:** `genReturnStmt` and `genRaiseStmt` emit `emitUseCloseCalls(0)` before exit
- **Break/Continue:** `genBreakStmt` and `genContinueStmt` emit `emitUseCloseCalls(loopUseDepth)` for loop-scoped bindings
- All 8 loop functions save/restore `loopUseDepth`
- Phase 1: close errors are silently suppressed (failable close error propagation deferred to follow-up)
4 codegen tests.

**Ownership:** `pinned map[string]bool` field on `Checker`. `use`-bound variables are marked as pinned — `tryMove` rejects moves of pinned variables. 1 ownership test.

## Stage 8n — Constructors: `new`, `final`, Factory, `Self`, `super` (Done)

Explicit constructors, immutable-after-construction fields, factory constructors, `Self` type alias, and constructor inheritance with `super()` calls.

**Files:** `sema/check.go`, `sema/decl.go`, `sema/expr.go`, `sema/stmt.go`, `sema/meta.go`, `sema/resolve.go`, `sema/info.go`, `types/field.go`, `types/named.go`, `codegen/compiler.go`, `codegen/expr.go`

- **Required field enforcement**: Sema rejects constructor calls that omit required fields. A field is required if it is not `T?` and has no `= default`. All required fields must be provided as named arguments.
- **Default expression evaluation**: Fields with `= expr` defaults have their default expression evaluated at the call site when omitted. Defaults recorded in `Info.FieldDefaults` during sema, evaluated in `genConstructorCallMono` during codegen.
- **`` `final `` fields**: `isFinal` flag on `types.Field`. Can be assigned in `new()` body or on locally-created instances in `` `factory `` methods. No setter generated. Custom setter on `` `final `` field is a compile error.
- **`Self` type alias**: Resolves to `c.curType` in both type-ref context (`resolveNamedType`) and expression context (`checkIdentExpr`). Usable in return types, constructor calls (`Self(...)`), and parameter types within type bodies.
- **Explicit `new` constructor**: `new(~this, params)` replaces the implicit constructor. Implicit `~this` receiver, implicit `Self` return. Sema validates: mutable receiver, no explicit return type, not abstract. `HasNew` flag on `types.Named`. Codegen zero-inits all fields, then calls `Type.new(instancePtr, args...)`.
- **Failable `new`**: `new(~this, params) void!` — codegen wraps result in `{ i1, T, i8* }` result struct with branch/merge/phi for error vs ok paths at the constructor call site.
- **Factory constructors**: `` `factory `` annotation implies `` `variant `` placement. No `this` receiver (set to nil in `resolveMethodSignature`). Must have return type (typically `Self`), except abstract factories on structural interfaces which get implicit `Self` return. Called as `Type.factoryName()`. `` `final `` field writes restricted to locally-created instances via `factoryLocals` tracking. Abstract factories are allowed only on `` `structural `` interfaces and participate in `Implements()` matching (factory must match factory, instance must match instance).
- **`super()` calls**: Parsed as `CallExpr(IdentExpr("super"), args)` — no grammar changes. Intercepted before normal callee resolution in both sema and codegen. When parent has `new()`, calls `ParentType.new(this, args...)`. When parent has implicit constructor, sets parent fields directly via child's layout. Failable parent `new()` propagates errors.
- **Constructor inheritance validation**: Runs as a post-define pass (`validateConstructors`) after all types are defined, ensuring correct validation regardless of declaration order. Child must define `new()` when parent has `new()`. Child `new()` must be failable when parent `new()` is failable.
- **`lookupOwnMethod` helper**: Searches only a type's directly declared methods, not inherited — prevents `LookupMethod("new")` from finding a parent's `new()` and incorrectly setting `HasNew`.
- **Scope**: Required fields, default expressions, `` `final `` fields, `Self` type alias, explicit `new()`, failable `new()`, factory constructors, `super()` with explicit/implicit parent, constructor inheritance validation.
- **Deferred**: Definite-assignment analysis for `` `final `` fields in `new()` body, `super()` position tracking (no `this` before `super`, exactly-once enforcement, no `super` in loops).

## Stage 8o — `drop()` Methods: Ownership-Driven Cleanup (Done)

Compiler-inserted `drop()` calls when a value's owner goes out of scope and the value has not been moved.

**Sema:**
- Validate `drop()` signature: must be `drop(~this)` with no return type and no `!`
- Reject `drop()` on `` `copy `` types (compile error)
- Track which Named types have `drop()` methods in `Info`

**Codegen:**
- At scope exit, for each local variable (reverse declaration order): if type has `drop()` and value was not moved and not declared with `use`, emit `drop()` call
- **Drop flags**: for variables with ownership that depends on control flow (moved in one branch, not another), emit a boolean `alloca` (drop flag) initialized to `true`. Set to `false` when moved. At scope exit, condBr on flag before calling `drop()`.
- **Field dropping**: after parent `drop()` returns, recursively emit `drop()` calls for each field whose type has `drop()`, in reverse declaration order
- **`use` suppression**: variables declared with `use` are excluded from `drop()` insertion — `close()` already handles their cleanup

**Ownership:**
- Integrate with existing move tracking: when a variable is moved, record that it should not be dropped
- Extend control flow merge to track drop eligibility across branches

**Runtime:** No runtime changes — `drop()` is a regular method call.

## Stage 8p — Pure Value Types (Done)

Types where ALL fields have `` `value `` placement behave like primitives: data embedded directly in the Value struct, no heap allocation, automatic `` `copy `` semantics. Enables stack-allocated types like `Point`, `Duration`, `Range`, `Color`.

**Files:** `types/named.go`, `sema/decl.go`, `sema/meta.go`, `codegen/layout.go`, `codegen/compiler.go`, `codegen/rtti.go`, `codegen/types.go`, `codegen/expr.go`, `codegen/stmt.go`; 9 sema tests, 4 codegen tests, 5 e2e tests in `tests/value_types/`

**Type system** (`types/named.go`):
- `IsValueType()` / `SetIsValueType()` flag on `Named` — set when all fields are `` `value `` placement

**Sema** (`sema/decl.go`, `sema/meta.go`):
- `detectValueType()` runs after field/meta processing in `defineType()`. Checks: all fields `` `value ``, not native, no parent types (`is`), all fields are copy types, no `drop()` method. Sets `IsValueType(true)` and auto-enables `Copy`.
- Validation: value types cannot have `is` parents, failable `new()`, `drop()`, or non-copy `` `value `` fields

**Layout** (`codegen/layout.go`):
- `LayoutValueType` enum value. `computeValueTypeLayout()` builds four structs:
  - Type: `{}` (empty, standard)
  - Variant: `{ T_t* _type }` (standard)
  - Instance: `{ T_m* _variant }` — RTTI-only global singleton, no user fields
  - Value: `{ i8* _vtable, T_i* _rtti, field1, field2, ... }` — user data at indices 2+
- `ValueFieldIndex map[string]int` maps field names to Value struct indices
- `instanceFieldLLVMType()` takes `allLayouts` parameter to resolve nested value type field types

**Codegen** (`codegen/compiler.go`, `codegen/rtti.go`):
- `valueTypeRTTI map[*types.Named]*ir.Global` stores global RTTI instance singletons
- `emitTypeInfoGlobals()` creates `promise_rtti_T` globals pointing to variant struct for RTTI chain
- Value type dispatch in `computeUserTypeLayouts()` → `computeValueTypeLayout()`

**Type resolution** (`codegen/types.go`):
- `resolveType()` returns `layout.Value.LLVMType` for value types (full struct, not `{i8*, i8*}`)

**Constructor** (`codegen/expr.go`):
- `genValueTypeConstructor()` builds value via `insertvalue` chain — no `malloc`. Sets vtable (field 0), RTTI pointer (field 1), user fields (2+). Supports implicit constructors and `new()` methods (via alloca + store + call + load).

**Field access** (`codegen/expr.go`):
- Value type fields: `extractvalue` from SSA value, or GEP from `this` pointer
- `genFieldPtr()` updated for value type `this` and variable targets

**Method calls** (`codegen/expr.go`, `codegen/stmt.go`):
- `valueTypeReceiverPtr()` helper: stores value to temp alloca, returns bitcast to `i8*`
- Updated `genMethodCall`, `genGetterCall`, `genSetterCall`, operator dispatch, `genMethodIndex`
- `extractInstancePtrForThis()` helper: extracts RTTI pointer (field 1) from value type `this` for `loadVariantPtr` — prevents reading vtable (field 0) instead of variant pointer

**Rules**:
- No `is` inheritance — value types are leaf types
- All fields must be copy types — no string, vector, or heap-allocated types
- Automatically `` `copy `` — no explicit annotation needed
- No `drop()` method — nothing to clean up
- No failable `new()` — constructor builds inline, no error propagation

**Generic value types**: Supported via monomorphization (`computeMonoValueTypeLayout` in `mono.go`). `Range[T]` is the first generic value type — `Range[int]`, `Range[char]`, `Range[uint]`, etc. are monomorphized with correct field types.

**Deferred**: Structural interface coercion (stack boxing), mixed `value`+instance field placements, `variant`/`type` field placements, `global`/`mono` data placement on fields.

## Yield Generators (Done)

Generator functions: `stream[T]` return type with `yield` statements, compiled to LLVM presplit coroutines, consumed via `for-in`.

**Files:** New `codegen/generator.go` (~350 LOC); updates to `codegen/compiler.go`, `codegen/stmt.go`, `codegen/mono.go`, `sema/check.go`, `sema/stmt.go`, `sema/info.go`, `types/container.go`; 12 e2e tests

**Sema:**
- `AsStream(Type)` / `AsIterator(Type)` type helpers in `types/container.go`
- Generator detection: functions/methods returning `stream[T]` with `yield` recorded in `Info.GeneratorFuncs`
- `yield` outside generator → error; `yield` in lambda → error; `yield` type mismatch → error
- `stream[T]` function with no `yield` → error (catches bare `return` in stream functions)
- Return path analysis skipped for generator functions (terminate by falling off the end)

**Codegen:**
- **Representation**: `{i8* coro_handle, i8* yield_slot_ptr}` — same shape as closure fat pointer. Yield slot is heap-allocated (stable pointer across suspend/resume).
- **Coroutine structure**: LLVM presplit coroutine with initial suspend. Factory function allocates yield slot, calls coroutine ramp, returns `{handle, slot}`.
- **Yield**: Store value to yield slot → `coro.suspend(none, false)` → switch (0=resume, 1=cleanup, default=suspend/done).
- **Consumer (for-in)**: `resume → done? → load value → body → resume → done? → ...`. Loop exit: `coro.destroy(handle)`, `pal_free(yield_slot)`.
- **Coro-elide prevention**: `__promise_gen_resume/done/destroy` noinline wrapper functions hide the resume/done/destroy pattern from LLVM's coro-elide pass, which otherwise incorrectly stack-allocates generator frames.
- **Critical coro-split requirement**: Suspend block (default case of `coro.suspend` switch) must go to a block with `coro.end + ret`, not a bare `ret`. Without `coro.end`, coro-split generates `unreachable` in the resume function's suspend path → optimizer eliminates state saves → use-after-free.
- **Scope cleanup**: `bindingGenerator` kind with handle+slot allocas. On scope exit/break/return: null-check handle → `coro.done?` → `coro.destroy` → `pal_free(slot)`.
- **Monomorphization**: Generator routing in `defineMonoMethods` and `defineMonoFuncs` via `GeneratorFuncs` map lookup.
- **Method generators**: `buildGeneratorCoroutine` shared by top-level functions and type methods. Methods pass receiver as extra coroutine parameter.

**Scope**: Basic generators, infinite generators with break, early return, conditional yield, generator with parameters, empty generators, nested generators (generator calling generator), early break cleanup, index variable in for-in, generator methods on types, recursive generators.

**Deferred**: `yield*` (delegate to sub-iterator), failable generators (`stream[T]!`), stored generator values (first-class generator variables outside for-in), generator closures (capturing lambdas as generators).

## Stage 8q — `\`target(cond)` Compile-Time Platform Filtering (Done)

Compile-time declaration filtering via `` `target(cond) `` annotations. Allows platform-specific function/type/enum variants without preprocessor directives.

**Files:** New `sema/target.go`; updates to `sema/check.go`, `sema/decl.go`, `sema/info.go`, `sema/meta.go`, `sema/sema_test.go`, `codegen/compiler.go` (11 sites), `cmd/promise/main.go`, `cmd/promise/stress.go`, `cmd/promise/doc.go`; 6 sema tests + 1 e2e test file

- **Annotation**: `` `target(cond) `` on `func`, `type`, `enum` declarations. Methods not individually filterable — filter the whole type.
- **Condition expressions**: identifiers (`linux`, `macos`, `windows`, `wasm`, `posix`, `x86_64`, `aarch64`, `arm64`), logical `!`, `||`, `&&`. `posix` is shorthand for `linux || macos`. `arm64` is an alias for `aarch64` (Apple convention).
- **Sema**: `matchesTarget()` evaluated in declare pass (pass 1). Non-matching declarations are skipped (`FilteredDecls[d] = true`) and never inserted into scope. Define (pass 2) and check (pass 3) naturally skip filtered names since `scope.Lookup` returns nil. Zero `TargetInfo` = no filtering (all existing `sema.Check`/`CheckWithModules` callers unchanged).
- **Host triple defaulting**: `compileFrontendForTarget(filename, "")` defaults to `codegen.HostTargetTriple()` when no explicit target is given. Without this, both `sep() \`target(linux)` and `sep() \`target(!linux)` would survive sema → redeclaration error.
- **Codegen guard**: all 11 `for _, decl := range file.Decls` loop sites check `c.info.FilteredDecls[decl]` and skip filtered declarations. Without this, codegen would emit IR for both variants of a filtered pair → LLVM "invalid redefinition of function" error.
- **`ParseTargetInfo(triple)`**: derives `TargetInfo{OS, Arch}` from LLVM target triple. Handles linux-musl, linux-gnu, apple/darwin/macos, windows-msvc, wasm32-wasi.
- **Module loader**: target info flows into `moduleLoader.load()` so dependency modules also filter correctly during their own sema pass.
- **`promise doc`**: uses zero `TargetInfo` intentionally — shows all declarations regardless of target.
- **E2e test**: `tests/e2e/test_target_filter.pr` — function pairs filtered by `linux`/`!linux`, `posix`/`!posix`, `x86_64`/`!x86_64`, `wasm`/`!wasm`. Passes on Linux host and WASM cross-compile target.

## Stage 9 — Module System (Phase 3 Done + Identity Redesign)

Module resolution and dependency management. See [module-system.md](module-system.md) for the full design.

**Phase 1 — Module Boundaries & Local Imports (done):**

**Files:** `grammar/PromiseParser.g4` (use decl alts), `internal/module/config.go`, `internal/module/cache.go`, `internal/sema/decl.go`, `internal/sema/expr.go`, `internal/sema/resolve.go`, `internal/sema/export.go`, `internal/sema/info.go`, `internal/codegen/compiler.go`, `internal/codegen/expr.go`, `internal/codegen/separate.go`, `internal/codegen/mono.go`, `ast/decl.go`, `ast/visit_decl.go`, `types/object.go`, `cmd/promise/main.go`, `modules/std/*.pr` (22 files)

- **Grammar**: `useDecl` has two labeled alternatives — `catalogImport` (`use json;` / `use json as j;` / `use json as _;`) and `sourcedImport` (`use parser "url";` / `use _ "url";`). `qualifiedType` alt in `typeRef` for `mod.Type` references.
- **`promise.toml`**: TOML parser for `[module]`, `[require]`, `[replace]` sections. `promise init` creates the file.
- **`public` visibility**: Explicit `` `public `` meta annotation on types, enums, functions, fields, and methods. `isObjectExported()` checks Func/Named/Enum exported flags. All 22 `modules/std/*.pr` files annotated with explicit `public` on exported symbols.
- **Module scope resolution**: `resolveModuleScope()` handles catalog modules via `loadCatalog()`, local modules via `moduleScopes` map. `mergeGlobImport()` for `as _` with eager conflict detection, filtering by `public` visibility.
- **Qualified access**: `resolveModuleMember()` for `mod.func()` calls with visibility enforcement. `resolveQualifiedType()` for `mod.Type` in type position.
- **Local module loading**: `loadLocalModule()` in `cmd/promise/main.go` — scans use decls for local paths, parses+sema+exports. `ExportedScope()` extracts only `public` symbols. `mergeModuleFiles()` combines multiple `.pr` files in a module dir.
- **Std as catalog module**: `std` is a regular embedded catalog module (`modules/std/promise.toml`, `catalog.toml`). Every file auto-receives an injected `use std as _;` glob import — no special `stdScope` parent chain. `CheckForStdModule` compiles std with `compilingStd=true` to avoid self-import. `use std;` / `use std as s;` still work for qualified access.
- **Cross-module codegen (inline strategy)**: All module declarations compiled into one LLVM IR module via `compileModules()` with context save/restore (`c.info`/`c.file`/`c.compilingModule` swap). Name mangling: `__mod_<module>_<func>` for functions, `__mod_<module>_<Type>.<method>` for methods. `moduleFuncs`/`moduleExterns` maps for qualified call dispatch; plain names registered in `c.funcs` for glob imports. MemberExpr dispatch: std check → module check (function/constructor/enum switch) → enum layout → method call. Coercion uses callee's `*types.Signature` from sema directly.
- **Separate compilation**: Post-codegen IR split — single codegen pass produces unified `ir.Module`, then `SplitModuleIRs()` (in `codegen/separate.go`) toggles function blocks and global initializers to produce per-module `.ll` files. Module IRs contain only module-owned function bodies; all other functions become `declare`, all globals become `external`. Main IR keeps everything except module function bodies. Each `.ll` → `opt -O1 → .bc` (non-Windows; modules compiled in parallel via goroutines). `linkLinuxMulti()`/`linkDarwinMulti()`/`linkWasmMulti()` pass all `.bc` files to the LTO-capable linker. Windows: `.ll` → `opt → llc → .o` → `lld-link`. `moduleOwnedFuncs map[string]string` tracks which IR functions belong to which module (populated during `declareModuleFuncs`, `declareModuleTypeMethods`, `declareMonoMethods`, `declareMonoFuncs`).
- **Incremental compilation**: Content-addressed build cache at `~/.promise/cache/build/` (overridable via `PROMISE_HOME`). **Implementation hash** (FNV-128a of sorted source file contents) determines when a module's `.bc` needs recompilation. **Interface hash** (FNV-128a of public API signatures: function names+signatures, type fields+methods, enum variants) determines when dependents need recompilation. Cache key is FNV-128a of impl hash + compiler hash + target + sorted module paths. Files stored in two-level directory structure (`<hash[:2]>/<hash>.bc`). Atomic writes via temp + rename with rollback. On cache hit, the `opt` pipeline is skipped entirely — the cached `.bc` is passed directly to the LTO linker. Main file always recompiles (most frequent change target). Cache key includes filename separators to prevent hash collisions from file splits.
- **Tests**: 7 std-module sema tests, 26 general module sema tests, 4 ExportedScope tests, 10 module load integration tests, 5 `cmd/promise` integration tests, 6 codegen std tests, 15 cross-module codegen tests (qualified calls, constructors, methods, enums, glob imports, failable functions, externs, multi-module, multi-param generics). 42 e2e tests across 12 files in `tests/modules/` (qualified calls, glob imports with struct types/enums/match, aliases, multi-file modules, generics including multi-param `Pair[int, string]`, failable functions, drop types, closures, visibility, two-module interop, module type as param/return).

**Phase 2 — Transitive Dependencies & Canonical Identity (done):**

**Files:** `cmd/promise/main.go` (moduleLoader refactor), `internal/sema/info.go` (CanonicalName, ModuleOrder), `internal/codegen/compiler.go` (typeGlobalName, moduleCanonical), `internal/codegen/rtti.go` (module-prefixed globals), `internal/codegen/expr.go` (resolveModuleName), `internal/module/cache.go` (BuildCacheKey)

- **Recursive module loading**: `moduleLoader` struct with DFS walk — replaces flat `loadLocalModule()`. Modules are parsed, their `use` declarations scanned, and dependencies recursively loaded via `loadDeps()` before `sema.CheckWithModules()`. Results cached by absolute directory path to handle diamond dependencies (same module loaded once, shared across consumers).
- **Cycle detection**: `visiting` set (maps absDir → import path) tracks in-progress modules. If a module is encountered while it's being loaded, a cycle error with the full path is reported (e.g., `"./a → ./b → ./c → ./a"`). `buildCyclePath()` formats the cycle from the visit stack.
- **Topological ordering**: `depOrder` records modules in post-order DFS — leaf dependencies first. Stored in `Info.ModuleOrder` for codegen. `compileModules()` processes modules in this order so that a module's types and functions are available when its dependents are compiled.
- **Module identity**: Each module has a globally unique identity (`GlobalIdentity`) derived from its source — relative path for local, normalized URL for remote, catalog name for catalog. IR symbols use `__mod_<IRPrefix>_<func>` where `IRPrefix` is derived from GlobalIdentity via `SanitizeIRPrefix()`. This enables cross-project `.o` reuse. `EffectiveIRPrefix()` helper on `ModuleInfo` provides the fallback chain (IRPrefix → CanonicalName → Name) for test compatibility.
- **Duplicate identity detection**: `globalIdentities` map (GlobalIdentity → absDir) in `moduleLoader` detects two different modules resolving to the same identity. Two modules with the same `promise.toml` name but different paths can coexist.
- **Module-prefixed IR globals**: `typeGlobalName()` returns `__mod_<IRPrefix>_<Type>` for typeinfo, vtable, and RTTI globals when compiling a module. Prevents name collisions between module types and std library types (e.g., module `Range` vs std `Range`). `resolveModuleName()` maps consumer aliases to IR prefixes via `moduleCanonical` map.
- **Tests**: 3 new loader tests (transitive, diamond, circular, 3-module circular, canonical name, duplicate canonical name), 4 new codegen tests (prefixed globals, SplitModuleIRs, canonical vs alias IR identity), 8 new cache unit tests (hashing, determinism, round-trip, stale cleanup), 1 new e2e test file (4-level deep transitive chain: test → renderer → geometry → utils). Total: 53 e2e module tests across 15 files, 775 total tests.

**Phase 3 — Remote Module Fetching (done):**

See [phase3-remote-modules.md](phase3-remote-modules.md) for the full design.

**Files:** `internal/module/config.go` (NormalizeURL, SetRequire, IsLocalPath), `internal/module/git.go` (new — git operations), `cmd/promise/main.go` (loadRemote, runPin, runClean --global, epoch warnings)

- **URL normalization**: `NormalizeURL()` strips schemes (`https://`, `http://`, `git://`, `ssh://`), trailing `.git`, trailing slashes, lowercases host (preserves path case). Idempotent.
- **Global cache**: `~/.promise/cache/modules/<normalized-url>/repo.git/` (bare clone) + `<commit12>/` (checkout). `GlobalCacheDir()`, `URLToCachePath()`, `CleanGlobalCache()`.
- **Git operations**: `ensureBareRepo()` (`git clone --bare` or `git fetch`), `ensureCheckout()` (`git archive | tar x`, fallback to `--work-tree`). `ResolveRemoteModule()` orchestrates with fast-path for existing checkouts and filesystem `.lock` (O_CREATE|O_EXCL, 30s timeout, 5-min stale auto-cleanup).
- **Pin resolution**: `PinResolve()` resolves tags/branches/HEAD/"" to full 40-char commit SHA via `git ls-remote`. `IsFullCommitHash()`/`IsCommitHashLike()` validators.
- **`moduleLoader` integration**: `projectCfg` (root config), `remoteResolved` (URL→absDir dedup), `commitPins` (effective pins). `loadRemote()`: normalize → dedup → `[replace]` check → pin lookup → `ResolveRemoteModule()` → `load()`. Both `loadModuleScopes()` and `loadDeps()` dispatch non-local imports to `loadRemote()`.
- **Transitive pin merging**: `mergeTransitivePins()` reads remote module's `promise.toml` [require] and merges into effective pins. Top-level project pins always win (override). Conflicting pins from different modules → error. Called from both `[replace]` and git-fetch paths.
- **`promise pin`**: CLI command resolves tag/branch/HEAD/commit to full SHA via `PinResolve()`, writes to `promise.toml` via `SetRequire()`. Creates `[require]` section if absent; updates existing entries in-place (normalized URL match).
- **Epoch mismatch warnings**: `load()` compares module's epoch vs project epoch, collects warnings in `ml.warnings`. Printed to stderr after all modules loaded.
- **`promise clean`**: removes the build cache (`~/.promise/cache/build/`). `--global` additionally removes the module cache (`~/.promise/cache/modules/`). All cache paths respect `PROMISE_HOME` env var.
- **Tests**: 17 git tests, 2+5 config tests (URL normalization + SetRequire), 7+2 integration tests (remote module + epoch warnings).

**Deferred to Phase 4+:**
- Catalog infrastructure and versioning

**Planned phases:** Phase 4 (catalog infrastructure), Phase 5 (catalog CI), Phase 6 (tooling)

### Stdlib Catalog Modules (Tier 1)

Catalog modules shipped as embedded `.pr` files in `modules/std/`. Registered in `internal/module/catalog.go` via the embedded `catalog.toml`. Available via `use math;`, `use sort;`, `use set;`, `use random;`.

**`modules/std/math.pr`** — Integer math (`min`, `max`, `abs`, `clamp`), constants (`PI`, `E`, `TAU`, `MAX_INT`, `MIN_INT`), f64 math via LLVM intrinsics (`sqrt`, `sin`, `cos`, `pow`, `exp`, `log`, `fabs`, `floor`, `ceil`, `round`), derived f64 functions (`tan`, `log2`, `log10`, `is_nan`, `is_inf`), integer power (`ipow`). LLVM intrinsics declared as `extern` functions mapped to codegen-emitted declarations (`promise_sqrt` → `@llvm.sqrt.f64`, etc). 7 intrinsic functions in `expr.go:genCallExpr`.

**`modules/std/sort.pr`** — Insertion sort for `Vector[int]` and `Vector[string]` (`sort_ints`, `sort_strings`). Comparison-based variants (`sort_ints_by`, `sort_strings_by`) taking `(int, int) bool` / `(string, string) bool` comparators. Pure Promise implementation (no native methods). `reverse_ints`, `reverse_strings`.

**`modules/std/set.pr`** — `Set[T]` wrapper around `Map[T, bool]`. Methods: `add`, `remove`, `contains`, `get len`, `to_vector`, `union`, `intersection`, `difference`. All pure Promise.

**`modules/std/random.pr`** — xorshift64 PRNG seeded from `nanotime()`. Functions: `random_int`, `random_range`, `random_f64`, `random_bool`, `shuffle_ints`. Uses `nanotime() int` extern for seeding.

**Codegen support:** `genCallExpr` handles LLVM math intrinsics via name-based dispatch. `instanceFieldLLVMType` updated to not special-case `Map` (already `i8*`). `genCallExpr` applies `typeSubst` to constructor callee types for generic constructors inside generic methods.

**Tests:** 46 math tests (`tests/std/test_math.pr`), sort/set/random tests in `tests/std/`. Total: 920 native pass.

## Stage 10 — CLI

Command-line interface. Core commands implemented; formatter planned.

- `promise build [-o output] <file.pr>` — compile to binary
- `promise run <file.pr>` — compile and execute
- `promise check <file.pr>` — type-check only
- `promise test <file.pr>` — discover and run `test` meta-annotated functions
- `promise test <dir>` — scan directory for `.pr` files and run tests from each
- `promise test <dir>/...` — recursive directory scan (Go-style `...` wildcard)
- `promise test -timeout <duration>` — per-test timeout (default: 60s, accepts Go durations or plain seconds)
- **Per-test panic recovery** (non-WASM): setjmp/longjmp in test trampoline — a panicking test prints `FAIL` with panic context and continues to the next test, instead of killing the process. Uses separate TLS `@__promise_test_jmpbuf` (not the scheduler's `@__promise_panic_jmpbuf`) so normal program panics are unaffected. On WASM, panics still terminate (no longjmp support).
- **Compact multi-file output**: `promise test <dir>` prints one line per file (`PASS (time) file.pr (N tests)` or `FAIL (time) file.pr (M/N failed)` with indented failure details). Single-file runs remain verbose (every test with timing). End-of-run `FAILED:` summary includes panic/error context for AI-agent tail-friendliness.
- `promise ast <file.pr>` — print the AST
- `promise exec <code>` — execute inline code (auto-wraps in `main()` if needed)
- `promise install` — install compiler + std + runtime to `~/.promise/`
- Bare pipe detection: `echo '<code>' | promise` auto-enters exec mode
- Inline error formatting: source line + `^` caret marker, no temp filenames
- `promise clean` — remove build cache (`~/.promise/cache/build/`), `--global` also removes module cache
- Embedded `modules/std/` and `runtime/` in the binary via `go:embed` for self-contained install
- **Test suite**: 1202 tests across 175 files — `tests/e2e/` (language features), `tests/std/` (standard library), `tests/concurrency/` (scheduler, channels, select, panic recovery, stress tests), `tests/modules/` (module system e2e), `tests/value_types/` (pure value types)
- `promise doc <file.pr>` — generate documentation from `doc()` meta tags (**Phase 1 done**: `cmd/promise/doc.go`)
  - `-public` (default) / `-all` — filter by visibility
  - `-signatures` — compact signature-only output (minimal tokens for AI agents)
  - `-o PATH` — write to file instead of stdout
  - Runs `sema.DeclareAndDefine()` (passes 1+2 only — fast, tolerates incomplete bodies)
  - Shows: types (with inheritance, fields, methods, getters, operators, drop), enums (flat/payload variants with doc), functions (with generic constraints)
  - Param `doc()` and enum variant `doc()` propagated through sema
  - Bug fix: `extractDoc()` now uses `evalStringLit()` for triple-quoted doc strings
  - Phase 2 (planned): directory/recursive docs, `-std`, `-expand`, index generation
  - Phase 3 (planned): `-query "has:drop"`, `-lint`, IDE integration
- `promise format` — code formatter (implemented, see `docs/formatting.md`)
- `promise doctor` — environment diagnostics (planned, see below)

### `promise doctor` (Planned)

Inspired by `flutter doctor`: a single command that checks the entire Promise environment and reports what's working, what's missing, and how to fix it. Designed for both human troubleshooting and CI environment validation.

```
$ promise doctor

Promise doctor — checking your environment

[✓] Promise installation
    Version: epoch 2026.3 (linux-amd64)
    Home:    ~/.promise/
    Binary:  ~/.promise/bin/promise
    Stdlib:  ~/.promise/lib/std/ (48 files)

[✓] LLVM toolchain
    opt:  ~/.promise/bin/llvm/opt (LLVM 22.0.0, embedded)
    llc:  ~/.promise/bin/llvm/llc (LLVM 22.0.0, embedded)
    lld:  ~/.promise/bin/llvm/lld (LLVM 22.0.0, embedded)

[✓] Build cache
    Location: ~/.promise/cache/build/ (142 entries, 38 MB)
    Lock:     ~/.promise/cache/build/.lock (no contention)

[!] Module cache
    Location: ~/.promise/cache/modules/
    Warning:  no modules fetched yet (expected after first `promise build` with catalog deps)

[✗] Java (optional — required for compiler development only)
    Not found on PATH
    Install: https://adoptium.net  or  brew install java

No issues found. 1 optional tool missing.
```

Each check has three states: `[✓]` ok, `[!]` warning (works but suboptimal), `[✗]` error (broken or missing required component).

**Checks performed:**

| Check | What it verifies |
|-------|-----------------|
| Promise installation | Binary path, home dir, stdlib files present, version readable |
| LLVM toolchain | `opt`, `llc`, `lld` (or `ld` on macOS) found and version ≥ 22; embedded vs system |
| musl CRT | `crt1.o`, `crti.o`, `crtn.o`, `libc.a` present (Linux only) |
| Build cache | Directory exists, `.lock` accessible, disk usage |
| Module cache | Directory exists, reachable git hosts (optional network check) |
| `PROMISE_HOME` | If set, verifies it points to a valid install |
| Java | Whether `java` is on PATH (development only — needed to regenerate the ANTLR parser) |
| macOS: Xcode CLT | Whether `xcode-select -p` returns a valid path (macOS only). Required for macOS SDK sysroot even with embedded LLVM. |
| PATH | Whether `~/.promise/bin` is on PATH |

**Implementation notes:**
- All checks are independent — a failure in one doesn't skip others
- `--json` flag emits machine-readable output (for CI scripts and AI agents)
- `--fix` flag attempts automatic fixes where safe (e.g., PATH instructions, `xcode-select --install` hint)
- Fast: no compilation, no network requests by default; `--network` enables the git reachability check

### Stress test mode (`-stress`)

Flaky test detection via repeated execution with adaptive scheduling.

**Files:** `cmd/promise/stress.go` (~560 LOC), `cmd/promise/main.go` (flag parsing)

- `promise test -stress <target>` — run until Ctrl+C
- `promise test -stress N <target>` — run N iterations
- `promise test -stress <duration> <target>` — run for a time limit (e.g. `30s`, `2m`)
- **Compile-once, run-many**: all `.pr` files are compiled to temp binaries once, then re-executed repeatedly. Compilation errors are deterministic and exit immediately.
- **Per-test stats**: pass/fail count, timing mean/stddev/CoV (coefficient of variation), min/max. Timing parsed from test binary output (`PASS (0.003s) test_name`) for unit tests, wall-clock for e2e tests.
- **Flaky detection**: tests with any failure flagged as flaky. Tests with CoV > 0.5 and mean > 1ms flagged as high-variance (timing instability often precedes flakiness).
- **Adaptive file-level scheduling**: stable files gradually suppressed — run every 2nd iteration after 20 runs, every 4th after 50, every 8th after 100. Files with failures or high variance always run every iteration.
- **Live TTY display**: clears and redraws summary each iteration showing flaky/high-variance/stable counts. Non-TTY (piped) mode prints progress every 2 seconds.
- **SIGINT handling**: Ctrl+C stops the loop and prints the final report.
- **Exit code**: 0 if all tests 100% pass rate, 1 if any flaky tests found.

## Stage 11 — Package Manager (Planned)

Dependency fetching and resolution.

- Fetch from git/HTTP URLs
- Lockfile generation and deterministic builds
- Diamond dependency resolution
- Cache management

---

## What's Next

The compiler pipeline (Stages 1-8q) is complete. Runtime is fully codegen-emitted LLVM IR — no C files remain. All major cross-cutting features are done: M:N scheduler (Phase 5c), WASM target (Phases 4b/5d/7a), yield generators, structural interfaces, operator dispatch, naming conventions, pure value types, documentation system (Phase 1), module system (Phase 3 done + identity redesign), stdlib expansion (math, sort, set, random catalog modules), Format/Parse infrastructure (Writer, Reader, Builder, Scanner, `to_string()`, `format(Writer ~w)!` on all primitives, `int/bool/uint/f64.parse`, `string.from_bytes`, `Vector.filled`), time measurement (Duration, Instant, sleep), extended I/O (Closer, write_line), and compile-time platform filtering (`` `target(cond) ``). String pad_left/pad_right methods. Strings module `join` uses variadic params.

Test suite: 1554 native pass, 1549 WASM pass (5 skipped).

### Near-term: Compiler Infrastructure

| Work | Priority |
|------|----------|
| ~~Global build cache~~ — **Done.** `~/.promise/cache/build/` wired into `compileAndLinkSeparate()`. `PROMISE_HOME` override. | ~~High~~ Resolved |
| ~~Self-contained macOS binary~~ — **Done.** `llvm_darwin_arm64.go` + `llvm_darwin_amd64.go`, `make llvm-bundle-darwin`, `./build --release` on macOS. Embeds opt, llc, lld, libLLVM.dylib. Still requires Xcode CLT for macOS SDK sysroot. | ~~High~~ Resolved |
| Fully self-contained macOS binary — Bundle macOS SDK stubs (`libSystem.tbd` + headers) so linking doesn't require Xcode CLT. Go ships its own linker and doesn't need system tools; we could embed the minimal SDK surface needed for `-lSystem`. Would make macOS release binary zero-dependency like Linux. | Medium |
| ~~CLI: `promise format` code formatter~~ — **Done.** Token-based reformatter, 2-space indent, ~130 unit tests. See `docs/formatting.md`. | ~~Medium~~ Resolved |
| Module system Phase 4 (catalog infrastructure) — Stage 9 | Medium |
| Package manager (fetch, resolve, lock) — Stage 11 | Medium |
| Documentation system Phase 2 (directory/recursive docs, `-std`, index generation) | Medium |

### Medium-term: Codegen Gaps

| Work | Priority |
|------|----------|
| Blocking select optimization (waiter-list parking instead of yield-and-retry polling) | Medium |
| Stack overflow detection (guard page + SIGSEGV handler) | Medium |
| ~~Fixed-size arrays as stack-allocated `[N x T]`~~ | ~~Done~~ |
| Destructure is-patterns (`x is Dog(name)`) | Medium |
| `yield*` delegate (forward all values from sub-iterator) | Medium |

### Long-term: Runtime & Platform

| Work | Design Doc | Priority |
|------|-----------|----------|
| IO reactor (kqueue/epoll/IOCP) — Phase 6 | [runtime-architecture.md](runtime-architecture.md) | Low |
| Generic type RTTI | — | Low |
| Value type structural interface coercion (stack boxing) | — | Low |
| ~~Generic value types~~ | — | ~~Done~~ |
| ~~User type `format(Writer ~w)` for interpolation (desugar `"{x}"` to `x.format(~builder)`)~~ | — | ~~Low~~ Done |
| Type argument inference | — | Low |
| Output binary size optimization (rodata literals, dead code) | See [§Output Binary Size](#output-binary-size-optimization) | Medium |

### WASM remaining work

Tests: 1576 pass, 0 fail, 5 skip on `wasm32-wasi` (1581 native pass)

| Item | Skipped tests | Effort | Notes |
|------|--------------|--------|-------|
| ~~f64→string (custom dtoa)~~ | ~~12~~ → 0 | ~~Medium~~ Done | Custom `_f64_to_str` in `modules/std/format.pr` (pure Promise, %g format, 6 sig digits). No snprintf dependency. |
| f64→string full precision (Grisu2/Ryu) | 0 | Medium | Current implementation uses 6 significant digits (matches `%g`). For full round-trip precision (~17 digits), implement Grisu2 or Ryu algorithm. Needed for serialization/deserialization fidelity. |
| Panic recovery (`setjmp`/`longjmp`) | 2 (panic_recovery_basic, panic_recovery_channel) | Medium | WASM has no `setjmp`/`longjmp`. Options: Emscripten-style JS exception handling, or WASM exception handling proposal (`try`/`catch`/`throw` instructions). |
| ~~Free-list allocator~~ | ~~1~~ → 0 | ~~Medium~~ Done | Pre-compiled C free-list allocator (`wasm_alloc.c`) linked via wasm-ld. Size-class buckets (16B–64KB), sbrk via `memory.grow`. Replaces bump allocator. |
| GOMAXPROCS >1 | 1 (gomaxprocs_query) | Low | Single-threaded WASM can't have multiple Ps. Could clamp silently instead of panicking. |
| `clock_time_get` (WASI timing) | 0 | Low | `nanotime()` returns 0. Import `clock_time_get` from WASI for real timing. |
| File IO (`fd_read`, `fd_prestat_*`) | 0 | Low | No tests depend on it yet. Needed for WASI filesystem access. |
| WASM threading (shared memory + atomics) | 0 | Future | `wasm32-unknown-unknown` or threads proposal for true multi-P. |
| Browser/JS FFI | 0 | Future | `wasm32-unknown-unknown` target, JS host bindings. |

---

## Naming Convention Migration (Done)

All non-scalar types now use PascalCase canonical names in the universe, stdlib, codegen, sema, ownership, and tests. Lowercase forms (`map[K,V]`, `channel[T]`, `task[T]`, `iter[T]`, `stream[T]`) are syntactic sugar resolved by the compiler. `Range[T]` is generic (no lowercase alias). See [standard-library.md](standard-library.md#naming-conventions).

---

## Output Binary Size Optimization

The size of compiled Promise programs matters for startup speed, deployment cost, WASM module size (web pages), and embedded/serverless use cases. Currently no work has been done to optimize output binary size — the compiler emits all reachable code and allocates all literals on the heap at runtime.

### Current state

- **Dead code eliminated via LTO**: The `opt → .bc → linker --lto-O1` pipeline performs whole-program DCE across all modules (main + std + user modules). Unused functions, vtables, and RTTI structs are stripped at link time. WASM uses `--lto-O2` for stronger folding.
- **All string literals heap-allocated**: Every string literal (`"hello"`) calls `malloc` at runtime, copies the bytes, and frees on scope exit. The literal bytes exist in the `.rodata` section of the binary *and* are duplicated on the heap.
- **All container literals heap-allocated**: Vector and Map literals allocate heap storage and copy elements at runtime, even for compile-time-known constant data.

### Proposed optimizations (phased)

**~~Phase 1 — LLVM-level dead code elimination~~ (Done via LTO)**

The `opt → .bc → linker --lto-O1` pipeline provides whole-program DCE: unused functions,
globals, and vtable entries are stripped at link time by the LTO-capable linker. This subsumes
the `-internalize`/`-globaldce` approach — LTO is strictly more powerful (cross-module).

Additionally, consider `-Oz` or `-Os` instead of `-O1` for size-optimized builds (e.g.,
`promise build --small`). WASM already uses `--lto-O2` for stronger optimization.

**Phase 2 — Read-only string literals (medium effort, high impact)**

String literals whose content is known at compile time should point directly into the binary's `.rodata` section instead of heap-allocating a copy. Since strings in Promise are immutable, this is semantically transparent.

Implementation sketch:
- Emit string literal bytes as LLVM `@.str.N = private unnamed_addr constant [N x i8] c"..."` globals (already done for the raw bytes passed to `promise_string_new`).
- Change the string value representation: instead of `{i8* heap_ptr, i64 len}` where `heap_ptr` is malloc'd, use `{i8* data_ptr, i64 len}` where `data_ptr` points to the rodata global.
- The string "owns nothing" — no allocation, no free. The `drop`/scope-exit path for string must distinguish rodata-backed strings from heap-backed strings (e.g., a flag bit in the length field, or a separate `i1 owned` field, or comparing the pointer against the rodata range).
- Concatenation and mutation (StringBuilder) still produce heap strings — only literals benefit.
- Substring/slice could return rodata-backed views (no copy) if the source is rodata-backed.

This eliminates malloc+memcpy+free for every string literal in the program — a significant win for programs with many string constants (format strings, error messages, CLI help text).

**Phase 3 — Read-only container literals (medium effort, medium impact)**

Vector and Map literals with compile-time-known constant elements could be placed in `.rodata` and used directly without heap allocation.

- **Vector literals**: `[1, 2, 3]` could emit as a global `[3 x i64]` constant. The Vector header points to rodata with `len=3, cap=3`. Any mutation (push, pop, `[]=`) must COW (copy-on-write) to a heap-allocated buffer first.
- **Map literals**: More complex. Could emit a frozen hash table in rodata. Any mutation triggers COW. May not be worth the complexity initially.
- **Fixed-size arrays**: Already stack-allocated `[N x T]` — these could be rodata globals when all elements are constants and the array is not mutated.

**Phase 4 — Monomorphization pruning (medium effort, medium impact)**

Currently, all methods of a monomorphized generic type are emitted even if only some are called. Track which methods are actually referenced per monomorphization instance and only emit those. This requires a reachability analysis pass after sema (or lazy emission during codegen).

**Phase 5 — Strip and section optimization (low effort, low impact)**

- Strip debug info and symbol tables in release builds (`llc` flags or `llvm-strip`).
- Merge identical constants (LLVM's `-mergefunc` and `-constmerge` passes).
- For WASM: `wasm-opt -Oz` post-processing (Binaryen) for additional size reduction.

### Size budget targets (aspirational)

| Program | Current | Target | Notes |
|---------|---------|--------|-------|
| `main() {}` (empty) | ~20KB native, ~2KB wasm | <5KB native, <500B wasm | Dead code elimination |
| `main() { println("hello") }` | ~25KB native, ~3KB wasm | <8KB native, <1KB wasm | + rodata strings |
| Typical CLI tool | ~100-200KB | ~50-100KB | + mono pruning |

---

## Deferred Work

Known gaps and improvements deferred from completed stages.

### Correctness Bugs

| Item | Origin | Priority |
|------|--------|----------|
| ~~Reassignment of droppable variable leaks old value~~ — **Fixed.** `genAssignStmt` now calls `emitDropCall` on the old value before storing the new one. Drop flag is checked (moved values skipped) and reset after reassignment. Self-assignment short-circuits. | 8o | ~~Medium~~ Resolved |
| ~~Enqueue-before-suspend race~~ — **Fixed.** Goroutine stores the channel/done mutex in `G.park_mutex` before `coro.suspend`; the scheduler loop releases it in `coroSuspendedBlk` after `coro.resume` returns. Since the waker must acquire the same mutex to dequeue, it blocks until the suspend completes. Verified with stress tests in `tests/concurrency/stress_*.pr`. | 5c | ~~High~~ Resolved |
| ~~Park_m spurious wakeup causing deadlocks~~ — **Fixed.** POSIX `cond_wait` can return spuriously. `park_m` now loops checking `M.spinning` flag (set to 1 by `wake_m` before signaling) and shutdown flag. On spurious wakeup, `M.spinning == 0` → re-wait. Previously, spurious wakeup corrupted the idle M stack (M.p used as both next-pointer and real P association). | 5c | ~~Critical~~ Resolved |
| ~~Select blocking deadlock~~ — **Fixed.** `genSelectStmt` set `park_mutex = null` for blocking select, causing the scheduler to treat it as a yield and immediately re-enqueue. After resume, if no case was ready, code fell through to `mergeBlk` skipping the select. Replaced waiter-list parking with yield-and-retry loop (lockStartBlk). Waiter lists had fundamental enqueue-before-suspend and double-wake races with multiple channel mutexes. | 5c | ~~High~~ Resolved |
| ~~Steal_work data race on thief P queue~~ — **Fixed.** `steal_work` wrote stolen goroutines to the thief's P queue without holding the thief's lock. On ARM64, stores could be reordered, causing queue corruption visible to concurrent stealers. Now locks both thief and victim P's in address order (ptrtoint comparison) to prevent ABBA deadlock. | 5c | ~~High~~ Resolved |
| ~~PHI nodes not grouped in failable destructuring~~ — **Fixed.** `genFailableDestructure` interleaved PHI nodes with alloca/store in the merge block. LLVM requires all PHIs at block top. Reordered to emit both PHIs first, then stores. Affected `(val, err) := failable()` patterns. | 8e | ~~Medium~~ Resolved |
| ~~Non-deterministic scope iteration~~ — **Fixed.** Codegen iterated `info.Scopes` (Go map) for type/variable lookups, causing 50/50 test failures due to Go map randomization. Added `ScopeOrder []*types.Scope` to `Info` — insertion-ordered slice appended in `openScope()` and `Check()`. Replaced all 7 `range info.Scopes` sites in codegen with `range info.ScopeOrder`. Also added `StdScope` check in `resolveTypeRefToType` for std-declared types. | 9 | ~~Medium~~ Resolved |
| ~~Dynamic allocas in loop body cause stack overflow~~ — **Fixed.** Temporary allocas in `genVectorMethodCall` (push/pop/contains), `valueTypeReceiverPtr`, and `genExternCall` (sret + args) used `c.block.NewAlloca()`, placing them in the current basic block. Inside loops, each iteration allocated new stack space that was never freed until function return, overflowing the 2MB thread stack at ~131K iterations. Changed to `c.createEntryAlloca()` which places allocas in the function entry block (allocated once, reused across iterations). Regression tests at 200K–10M iterations in `tests/std/test_vector_advanced.pr`. | 5c | ~~High~~ Resolved |
| ~~Pre-existing failable `?` handler phi node type mismatch~~ — **Verified — not reproducible.** 10 e2e tests added in `test_error_handler_phi.pr` covering value-producing handlers (no return), method calls, user types. All pass. | 8k | ~~High~~ Resolved |

### Codegen Gaps

| Item | Origin | Priority |
|------|--------|----------|
| Channel send/recv and select allocas are still dynamic (in-block `alloca`). Non-blocking channel ops in a tight loop accumulate stack per iteration (bounded by channel capacity before a blocking suspend resets the stack). Moving them to entry-block allocas caused LLVM dominator violations in presplit coroutines — needs investigation into why vector method allocas in `coro.start` work but channel allocas don't. Related: the vector/extern/value-type-receiver fix in `createEntryAlloca`. | 5c | Medium |
| Blocking select uses polling (yield-and-retry) instead of waiter-list parking. Correct but spins when no case is ready. Proper fix requires multi-mutex unlock in scheduler or atomic wake-once protocol for select waiters. | 5c | Medium |
| Fire-and-forget goroutine G struct leak: all `go { }` blocks set `result_ptr` to sentinel `0x1`, preventing goroutine_exit from freeing the G. Only `task[T]` should use the sentinel. | 5c | Low |
| Stack overflow detection: deep recursion segfaults with no message. Add guard page (`mprotect` bottom page of M stack) + `SIGSEGV` handler on `sigaltstack` to print "stack overflow" and terminate cleanly. Consider `probe-stack` for large frames. | 5c | Medium |
| ~~Fixed-size arrays as stack-allocated `[N x T]`~~ | ~~8g~~ | ~~Done~~ |
| Destructure is-patterns (`x is Dog(name)`) | 8k | Medium |
| Generic type RTTI | 8k | Medium |
| Failable `close()` error propagation in `use` | 8m | Medium |
| Named enum fields in constructors | 8d | Low |
| Enum methods | 8d | Low |
| Extern ABI pack/unpack for enums | 8d | Low |
| Failable extern functions (C ABI for errors) | 8e | Low |
| Type argument inference (explicit type args only currently) | 8f | Low |
| Extern ABI for generic types | 8f | Low |
| Non-instance field placements: mixed `value`+instance, `variant`/`type` fields, `global`/`mono` data placement | 8c | Low |
| Value type structural interface coercion (stack boxing) | 8p | Low |
| ~~Generic value types~~ — **Done.** `computeMonoValueTypeLayout` in `mono.go`. `Range[T]` is the first generic value type. | 8p | ~~Low~~ Resolved |
| ~~User type `format(Writer ~w)` for interpolation (desugar `"{x}"` to `x.format(~builder)`)~~ — **Done.** User-defined types implementing `format(Writer ~w)!` now work in `{}` interpolation. Compiler creates a Builder, calls `format(~builder)!`, and converts to string via `Builder.to_string()`. Both direct dispatch and vtable dispatch (polymorphic) supported. Value types also supported. | 8h | ~~Low~~ Resolved |
| `yield*` delegate (forward all values from sub-iterator) | Generators | Medium |
| Failable generators (`stream[T]!` with error propagation through yield) | Generators | Low |
| Stored generator values (first-class generator variables outside for-in) | Generators | Low |
| Generator closures (capturing lambdas as generators) | Generators | Low |
| ~~Mono type vtable/RTTI~~: **Fixed.** Added `computeMonoVtableInfo`, `emitMonoVtableGlobals`, `emitMonoTypeInfoGlobals` in `rtti.go` plus unified lookup helpers (`lookupVtableGlobal`, `lookupTypeInfoGlobal`, `lookupValueTypeRTTI`) in `compiler.go`. Constructor codegen (`expr.go`) uses these to resolve vtable/typeinfo for both mono and non-mono types. | 8f | ~~Medium~~ Resolved |
| Devirtualization optimization (direct call when concrete type known) | 8L | Low |
| ~~Factory `Self` return type on generic types resolves to raw `Vector` instead of monomorphized `Vector[T]`~~ — **Resolved**: `selfType()` helper returns self-instantiation (`Instance{curType, [T1, T2, ...]}`) for generic types; used in `resolveNamedType`, `checkIdentExpr`, and implicit abstract factory return | Sema | ~~Low~~ |
| ~~`as!` cast between u8/char crashes (extractInstancePtr on scalar)~~ — **Fixed.** `emitScalarCast()` handles all scalar types (numeric, char, bool). `isScalarCastType()` in sema routes char/bool through the scalar cast path instead of the RTTI path. `int → bool` uses `icmp ne 0` (not trunc). `float → bool` uses `fcmp une 0.0` (NaN is truthy). 156 tests (106 e2e + 22 codegen IR + 28 sema). | Codegen | ~~Low~~ Resolved |
| ~~String comparison operators (`<`, `>`, `<=`, `>=`)~~ — **Resolved**: lexicographic byte comparison via `memcmp` | 8b | ~~Medium~~ |
| ~~Range value type variable binding~~ — resolved: Range is now `Range[T]` generic value type | 8g | ~~Medium~~ |
| ~~Char range iteration~~ — resolved: `Range[char]` uses correct i32 element type | 8g | ~~Medium~~ |
| ~~Uint range iteration~~ — resolved: `Range[uint]` uses correct uint element type | 8g | ~~Low~~ |
| ~~Map for-in tuple handling — `for entry in map` panics in codegen~~ — **Fixed.** Renamed `entry` basic block to `.entry` to avoid LLVM IR name collision. Added `for k, v in map` with proper key/value type bindings in sema+codegen. Added `get_or`, `pop`, `update`, `entries`, `merge` methods. | 8g | ~~Medium~~ Resolved |
| ~~`map[bool, T]` — bool key hashing/lookup is broken~~ — **Fixed.** Bool hash now uses hardcoded constants via `select i1` instead of `fnv1a_hash`. Map literal key types are validated against `Hashable + Equal` constraints via `validateConstraints`. | 8i | ~~Medium~~ Resolved |
| ~~Variable name collisions in repeated `if v := opt { }` blocks~~ — **Fixed.** `uniqueLocalName()` with per-function `localNameCount` appends `.N` suffix to duplicate alloca names in inner scopes. | 8n | ~~Medium~~ Resolved |

### Ownership & Type System

| Item | Origin |
|------|--------|
| Explicit lifetime annotations | 6b |
| Stored references in structs | 6b |
| Full NLL last-use analysis | 6b |
| Drop ordering | 6b |
| Disjoint field borrows | 6b |

### Meta Annotations

| Item | Origin |
|------|--------|
| `inline`, `packed`, `align`, `extern`, `serializable`, `public`, `unsafe` processing | 7 |

### Parameter Handling

| Item | Origin | Priority |
|------|--------|----------|
| ~~Default values for constructor parameters~~ — **Done.** Implemented in Stage 8n. Defaults recorded in `Info.FieldDefaults` during sema, evaluated in `genConstructorCallMono` during codegen. | 5b | ~~Medium~~ Resolved |
| ~~Unified parameter handling for constructors and methods~~ — **Done.** Implemented in Stage 8n. Named args, defaults, optional params, and `Self` all work for constructors. | 5b | ~~Medium~~ Resolved |

### Formatter (`promise format`)

| Item | Priority |
|------|----------|
| `a & b` formats as `a &b` — binary AND (`&`) between two value-producing tokens loses space after `&`. The formatter treats `&` as a unary ref modifier before identifiers because it can't distinguish `a & b` (bitwise AND) from `Reader &r` (ownership modifier) — both are `ident & ident`. The specific case `& !d` (binary AND + unary NOT) is handled, but the general `value & value` case is not. Fix requires either context tracking (expression vs parameter) or accepting the asymmetry. | Medium |
| Own-line comments after `;` pulled up as trailing comments — `x := 1;\n\n// comment` formats as `x := 1; // comment`. The semicolon handler calls `skipNewlines()` (consuming blank lines) then checks `next.kind == tkLineComment` to detect trailing comments, but doesn't check whether blank lines were consumed. Fix: only treat as trailing comment when `pendingNLs <= 1`. | Medium |
| `>` operator method declaration loses space before `(` — `>(Foo other)` instead of `> (Foo other)`. The `needsSpace` rule for `tkLParen` has `if p == tkGT { return false }` to handle generic close (`Foo[T]()`), but this also matches operator method declarations. Other operators like `<`, `==`, `!=` format correctly. Ambiguity: can't distinguish generic close from operator decl without parsing. | Low |
| Import sorting (`use` declarations sorted alphabetically) | Low |
| Trailing comma normalization in match/enum | Low |
| `--check` integration into `bin/verify.sh` | Low |

### Module System (Phase 4+)

| Item | Priority |
|------|----------|
| ~~**Module identity redesign**~~ — **Done.** Two-layer architecture implemented: GlobalIdentity (Layer 1) + SanitizeIRPrefix (Layer 2). See implementation notes below. | ~~High~~ Resolved |
| ~~Global content-addressable build cache~~ — **Done.** `~/.promise/cache/build/` wired into `compileAndLinkSeparate()`. Two-level dirs, atomic writes, `PROMISE_HOME` override. No local `.promise-build/` cache — global only. | ~~High~~ Resolved |
| Catalog infrastructure and versioning (Phase 4) | Medium |
| `promise exec` catalog module access — allow `use math;` etc. without `promise.toml` (catalog is always available) | Medium |
| Epoch-versioned side-by-side installs — multiple Promise epochs coexist (`~/.promise/versions/<epoch>/`), each with its own binary, cache, and catalog. `~/.promise/bin/promise` symlinks to the latest (or user-selected default) epoch. Projects pin their epoch in `promise.toml`; the wrapper dispatches to the correct versioned binary. | Low |
| Std as a regular cacheable module (remove AST-merge special case) | Low |
| Test file structure redesign (see below) | Medium |

**Module identity — two-layer architecture (implemented):**

**Layer 1 — Global identity** (`ModuleInfo.GlobalIdentity`): Every module has a globally unique identity:
- **Local modules**: the import path from project root (e.g. `./libs/parser`). Computed by `GlobalIdentityForLocal()`.
- **Remote modules**: the normalized URL (e.g. `github.com/alice/parser`). Computed by `GlobalIdentityForRemote()` via `NormalizeURL()`. Overrides the local path identity after `load()` returns.
- **Catalog modules**: the catalog-assigned name (e.g. `json`). Computed by `GlobalIdentityForCatalog()`.
- **Std**: not applicable (compiled inline, no module prefix).

Duplicate detection uses GlobalIdentity (not `promise.toml` name): two modules with the same `name` but different GlobalIdentities can coexist without collision.

**Layer 2 — IR prefix** (`ModuleInfo.IRPrefix`): Sanitized prefix for LLVM IR symbols, derived from GlobalIdentity via `SanitizeIRPrefix()`:
- Simple identifiers (matching `[a-zA-Z_][a-zA-Z0-9_]*`) **that required no stripping** pass through unchanged. This covers catalog modules only (e.g. `"json"` → `"json"`).
- All other inputs (local paths, remote URLs) are sanitized: non-alphanumeric characters replaced with `_`, runs collapsed, leading/trailing `_` trimmed. A 6-character FNV-128a hash suffix (of the original globalID) is appended for collision-freedom. This ensures `"json"` (catalog) and `"./json"` (local) never collide.
  - `./mylib` → `mylib_<hash6>` (local path gets hash suffix)
  - `./libs/parser` → `libs_parser_<hash6>`
  - `github.com/alice/parser` → `github_com_alice_parser_<hash6>`
  - `github.com/bob/parser` → `github_com_bob_parser_<hash6>` (different hash, no collision)

IR symbols use the pattern `__mod_<IRPrefix>_<symbol>`. Cache keys use the content-addressed build cache key (FNV-128a of impl hash + compiler hash + target + module paths).

**Files:** `internal/module/identity.go` (SanitizeIRPrefix, GlobalIdentityFor*), `internal/module/cache.go` (BuildCacheKey, HashModuleSources, HashModuleInterface, SaveBuildCache, LookupBuildCache), `internal/module/home.go` (PromiseHome), `internal/sema/info.go` (ModuleInfo.GlobalIdentity, ModuleInfo.IRPrefix), `internal/codegen/compiler.go` (compileModules/compileModule use IRPrefix), `internal/codegen/expr.go` (resolveModuleName returns IRPrefix), `cmd/promise/main.go` (load sets GlobalIdentity/IRPrefix, loadRemote overrides with URL identity, globalIdentities dedup map)

**Global content-addressable build cache (implemented):**

A device-wide build cache at `~/.promise/cache/build/` (overridable via `PROMISE_HOME` env var) stores `.o` files keyed by a content address derived from all inputs that affect the output:
- Compiler binary hash (`CompilerHash()`)
- Target triple
- Module source hash (impl hash — `HashModuleSources()`)
- Sorted list of all module paths in the build

The cache key is an FNV-128a of these inputs (`BuildCacheKey()`). Files are stored in a **two-level directory structure** using the first 2 hex characters of the hash as a subdirectory: `~/.promise/cache/build/a3/a3b4c5d8....bc`. Writes are atomic (`os.CreateTemp` unique temp file + `os.Rename`, with rollback on failure) — concurrent processes writing the same cache key use independent temp files, preventing corruption.

Wired into `compileAndLinkSeparate()` in `main.go`: lookup → cache hit skips `opt+llc` → cache miss compiles and saves. Interface hashes stored alongside `.o` files for incremental dependency tracking.

`PromiseHome()` in `module/home.go` provides the base directory for all Promise data (`~/.promise/` by default, overridable via `PROMISE_HOME` env var). Used by build cache, module cache, LLVM tools, CRT, and `promise install`.

**Concurrent build safety:** Multiple `promise test`/`build` processes use shared `flock(2)` on `build/.lock` — they run concurrently without blocking each other (content-addressed cache is safe for concurrent reads and atomic writes). `promise clean` acquires an exclusive lock via `LockBuildDirExclusive()`, waiting for all running test/build processes to finish. The lock is tied to the file descriptor — automatically released on process exit or crash (no stale lockfiles). `CleanAll()` preserves `.lock` to avoid invalidating locks held by concurrent processes.

**Cache diagnostics:** Set `PROMISE_CACHE_DEBUG=1` to see `[cache HIT/MISS/SKIP]` lines on stderr with cache key components. Useful for debugging cache invalidation issues.

Remaining work:
- **Garbage collection**: LRU eviction by access time, max size limit, or `promise clean --build-cache` manual purge

**Per-instance `.bc` caching (implemented):**

Each generic type instantiation gets its own cached LLVM bitcode file, enabling fine-grained reuse when only some instances change.

**TypeDecl hashing** (`sema/typehash.go`): `HashTypeDecl(td *ast.TypeDecl) string` and `HashEnumDecl(ed *ast.EnumDecl) string` compute a FNV-128a hash (32-char hex, 128 bits) of the full declaration AST — fields, method signatures + bodies, annotations, type params, inheritance parent name. Source positions and comments are excluded. This hash changes when *any* part of the type definition changes, but remains stable when unrelated declarations in the same file change. Hashes are computed in the sema declare pass and stored in `sema.Info.DeclHashes map[*types.TypeName]string`.

**Cache key** (`module.InstanceCacheKey`): `FNV-128a("instance\nprefix:<irPrefix>\nmono:<monoName>\ndecl:<typeDeclHash>\ncompiler:<compilerHash>\ntarget:<target>\n")`. The `monoName` encodes the concrete type args (`Box__int`, `Map__string__Vector__int`). The `typeDeclHash` covers the entire generic type definition. The `irPrefix` scopes it to the owning module.

**Pre-codegen lookup** (`lookupCachedInstances` in `main.go`): Before calling `codegen.Compile`, collects all mono instance names from `sema.Info`, builds their cache keys via `buildInstCacheKeys`, and checks the build cache (`module.LookupBuildCache`). Only active when modules are present and not using the clang pipeline. Returns `map[string]bool` of pre-cached instance names.

**Codegen integration** (`codegen.CompileWithCache`): Takes a `cachedInstances map[string]bool` alongside the normal inputs. Instances in the map are registered in `instanceOwnedFuncs` (so `SplitModuleIRs` knows to strip them from other IRs) but **body generation is skipped** in `defineMonoMethods` and `defineStructuralDefaultBodies`. This means `InstanceIRs()` returns `nil` for those instances (no bodies → nothing to extract).

**Post-codegen pre-cached loop** (`compileAndLinkSeparate` in `main.go`): Before the goroutine loop that compiles fresh instance IRs, a synchronous loop iterates all instance cache keys. For instances missing from `InstanceIRs()` (pre-cached), it calls `module.LookupBuildCache` and appends the cached `.bc` path directly to `instObjs`. If a cache file has vanished concurrently (e.g., `promise clean`), the linker reports an undefined symbol — correct and safe.

**Instance IR extraction** (`InstanceIRs()` in `codegen/separate.go`): Post-codegen, iterates instances that *have* generated bodies. For each, calls `saveAndStripNonOwnedInst` (strips all functions not owned by this instance), `stripGlobals` (strips non-private global definitions), captures the IR string, then restores. The resulting per-instance IR contains only this instance's `define` entries; all other functions appear as `declare` (extern declarations). Vtable and typeinfo global definitions stay in the main IR — instance BCs reference them as extern.

**Files:** `compiler/internal/sema/typehash.go` (HashTypeDecl/HashEnumDecl), `compiler/internal/sema/info.go` (DeclHashes field), `compiler/internal/module/cache.go` (InstanceCacheKey), `compiler/internal/codegen/compiler.go` (CompileWithCache, instanceOwnedFuncs, cachedInstances), `compiler/internal/codegen/separate.go` (InstanceIRs, saveAndStripNonOwnedInst), `compiler/cmd/promise/main.go` (lookupCachedInstances, buildInstCacheKeys, pre-cached loop in compileAndLinkSeparate)

**Tests:** `compiler/internal/sema/typehash_test.go` (27 tests: HashTypeDecl/HashEnumDecl determinism, field/method/annotation/inheritance changes, DeclHashes population via full sema), `compiler/internal/module/cache_test.go` (9 tests: InstanceCacheKey properties), `compiler/internal/codegen/codegen_test.go` (9 tests: InstanceIRs separation, instanceOwnedFuncs registration, CompileWithCache skips/registers)

**Module-internal test files (implemented — Option B):**

`*_test.pr` files in module directories are compiled as **part of the module** during `promise test` (Go-style `_test.go` pattern). All module source files (including all `_test.pr` files) are merged into a single compilation unit, giving test functions access to private declarations without needing `use <self>`.

**Files:** `cmd/promise/main.go` (`isModuleTestFile`, `compileModuleTestFrontend`, `runModuleTestFile`), `internal/module/cache.go` (`CollectModuleSources`, `HashModuleSources` with `includeTests` param, `SaveTestBinaryCache`/`LookupTestBinaryCache`), `cmd/promise/stress.go` (module test dispatch)

- **Detection**: `isModuleTestFile()` walks up the directory tree to find the nearest `promise.toml`, identifying module test files. Non-module `_test.pr` files (e.g., in `tests/`) use existing `compileFrontend` unchanged.
- **Compilation**: `compileModuleTestFrontend(modDir)` collects all `.pr` files (impl + tests) via `CollectModuleSources(modDir, true)`, which walks subdirectories recursively and excludes nested modules (subdirs with their own `promise.toml`). All files are merged into a single AST, then sema + ownership run on the combined unit.
- **Self-import detection**: If a test file contains `use <moduleName>;`, a clear error is emitted.
- **Test binary caching**: Compiled test binaries are cached in the build cache (`~/.promise/cache/build/`). Cache key = `BuildCacheKey(HashModuleSources(modDir, true), compiler, target, nil)`. Second runs skip compilation entirely.
- **Multi-file dedup**: `runTestFiles` and `compileTargets` (stress mode) track which modules have already been tested, avoiding duplicate results when multiple `_test.pr` files from the same module are discovered.
- **File discovery**: `discoverTestFiles()` uses `isInModuleTree()` to correctly handle test files in subdirectories of module roots.

**Non-module test binary caching (implemented):**

All non-module test files (`tests/...`) — both unit tests (`` `test ``) and E2E tests (`` `test(expected=...) ``) — are also cached in the build cache. Cache key = FNV-128a hash of source file content + compiler hash + std library hash + target triple + local module dependency hashes. On cache hit, the compiled binary is reused directly, skipping parse/sema/codegen/linking entirely (~30x speedup).

**Files:** `cmd/promise/main.go` (`cachedStdHash`, `computeTestFileCacheKey`, `executeE2EBinary`), `internal/module/cache.go` (`HashFile`, `HashDir`, `TestCacheMeta`, `SaveTestBinaryMeta`/`LoadTestBinaryMeta`), `cmd/promise/stress.go` (cache lookup in `compileTargets`)

- **Cache key composition**: Source file content (FNV-128a) + `CompilerHash()` (covers compiler logic + embedded catalog modules) + `cachedStdHash()` (covers dev-mode std changes) + target triple + hashes of any local module dependencies from `use X "./path"` declarations.
- **E2E metadata**: Cached E2E test binaries have a `.bin.meta` JSON sidecar storing expected output and exclude targets, so cache hits bypass compilation while preserving output comparison semantics.
- **Stress mode**: `compileTargets()` also checks the test binary cache before compiling, using metadata for test names and exclude lists.
- **Remote imports**: Files with non-local `use` imports (remote URLs) are not cached (too complex to hash; none exist in the test suite currently).
- **macOS first-run overhead**: On macOS, cached test binaries show ~0.1s/file on the first execution after cache population vs ~0.015s/file on subsequent runs (observed pattern: run 1 compile+exec ~0.5s, run 2 cached ~0.1s, run 3 cached ~0.015s). Cause not fully understood — may be macOS Gatekeeper/XProtect scanning unsigned binaries on first exec, though run 1 also executes each binary so the overhead should already be paid. Needs investigation.

**Remaining TODO — file naming**: The `test_*.pr` prefix convention in `tests/` should be renamed to `*_test.pr` for consistency (e.g., `test_arithmetic.pr` → `arithmetic_test.pr`).

### Unscheduled Features

| Item |
|------|
| ~~String slicing~~ — **Done.** `string[:]` operator implemented in `modules/std/string.pr` and codegen. Tested in `tests/std/test_string.pr` (7 slice tests). |
| Unicode normalization |
