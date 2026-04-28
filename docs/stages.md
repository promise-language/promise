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
| 5b | `compiler/internal/sema/` | Match bindings, unreachable code, multi-constraint, iter/stream, use decls | Done |
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
- **Map literals** resolve to `map[K, V]` type with key/value consistency checking
- **Range operators** (`..`, `..=`) resolve to `range` type
- **Go expressions** resolve to `task[T]` with inner type inference
- **Receive operator** (`<-`) extracts `T` from `task[T]` or `channel[T]`
- **Map indexing** returns `V?` (optional) for `map[K, V]`
- **For-in** supports `slice`, array, `map`, `range`, and `string` iteration
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
- **For-in on iter/stream instances**: `iter[T]` iteration yields `T`
- **Known limitation**: multi-arg generics (e.g., `Pair[int, string]`) only work in type annotation position (function params, variable types), not in expression context — grammar allows only single expression inside `[]`

---

## Stage 5b — Sema Completion (Done)

Completes remaining semantic analysis features before ownership checking.

**Files:** Updates to `sema/expr.go`, `sema/check.go`, `sema/decl.go`, `sema/resolve.go`, `sema/builtins.go`, `types/typeparam.go`, `types/equal.go`, `types/object.go`; ~23 new tests

- **Match pattern bindings**: `Some(v) => v` works — scope opened per arm, bindings inserted from ShortDestructure/EnumDestructure/Name/TypeBinding patterns. For generic enum Instance subjects, variant field types are substituted via `BuildSubstMap`.
- **Unreachable code detection**: `checkBlock` tracks dead-code state — statements after `return`, `raise`, `break`, `continue` flagged as unreachable. Recognizes if/else where both branches exit, exhaustive match with all arms exiting, and infinite loops without break.
- **Multi-constraint resolution**: `T: A + B` fully supported — `TypeParam.constraints []Type` stores all constraints, `resolveTypeParamConstraints` resolves the full constraint list, `validateConstraints` checks type args against ALL constraints, `AssignableTo` allows TypeParam assignment to any of its constraints.
- **iter[T] and stream[T] abstract methods**: `iter[T].next() T?` and `stream[T].next() task[T?]` populated via `populateIterStream()` in builtins — enables iteration protocol interface checking.
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
- Concurrency (go, task, channel, `<-`) → TBD

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
- **Deferred**: Methods (`.len`, `.contains`, etc.), slicing, Unicode normalization. String interpolation completed in Stage 8h.

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
- **Deferred**: Vtable/virtual dispatch, inheritance (parent fields/methods), static method calls (`Type.method()`), operator overloading on user types, non-instance field placements (`value`/`variant`/`type`), default field values. Generic user types handled in Stage 8f.

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

Error handling codegen: failable function declarations, raise statements, error propagation (`?`), forced unwrap (`!`), error handler expressions.

**Files:** Updates to `codegen/compiler.go`, `codegen/expr.go`, `codegen/stmt.go`, `codegen/types.go`; 17 error handling tests

- **Result struct**: Non-void `T!` → `{ i1, T, i8* }` (tag, ok value, error pointer). Void `void!` → `{ i1, i8* }` (tag, error pointer). Tag: `i1 false` = Ok, `i1 true` = Error.
- **Error values are `i8*`**: Same representation as strings. `raise "message"` stores the string's `i8*`.
- **Failable declarations**: Functions/methods with `CanError()` return the result struct. `declareFuncs`/`declareTypeMethods` wrap return type with `computeResultType`.
- **Return wrapping**: `genReturnStmt` wraps the value in an Ok result (`{ false, val, null }`) when inside a failable function.
- **Raise statement**: `genRaiseStmt` wraps the error in an Error result (`{ true, zero, errVal }`) and returns.
- **Error propagation** (`?`): `genErrorPropagateExpr` checks the tag via `condBr`. Error path extracts the `i8*` error, re-wraps in caller's result type, early-returns. Ok path extracts the value.
- **Forced unwrap** (`!`): `genErrorUnwrapExpr` panics on error via `promise_panic(i8*)` + `unreachable`. Ok path extracts the value.
- **Error handler** (`? binding { body }`): `genErrorHandlerExpr` branches to handler block (binds error, generates body) or ok block, merges with phi node.
- **Auto-terminator**: Failable functions without explicit terminator return an Ok-wrapped zero value.
- **Scope**: Failable functions/methods, raise, `?` propagation, `!` unwrap, `? binding { body }` handlers, void failables.
- **Deferred**: Failable extern functions (C ABI for errors). If-unwrap/while-unwrap completed in Stage 8h.

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
- **Deferred**: Type argument inference (explicit type args only), multi-arg generics in expression context (grammar limitation), extern ABI for generic types, C header generation for monomorphic types, container types (Array, Slice, Map, Tuple — Stage 8g).

## Stage 8g — Container Codegen (Done)

Codegen for container types (tuples, optionals, slices, maps) and capturing lambdas.

**Files:** Updates to `codegen/compiler.go`, `codegen/types.go`, `codegen/expr.go`, `codegen/stmt.go`; new `runtime/runtime_map.c` (~205 LOC); 29 new tests (119 total codegen tests)

- **Tuples**: Value type, LLVM struct `{ T0, T1, ... }`. Literals via `insertvalue`, destructuring (`(a, b) := expr`) via `extractvalue`. Mixed-type tuples supported.
- **Optionals**: Value type, `{ i1, T }` struct. `none` = zeroinitializer, some = `{ true, val }`. `targetType` field on Compiler resolves contextual type for `NoneLit` (sema records `TypNone` but codegen needs `Optional(T)`). `lookupLocalType` detects `OptionalTypeRef` annotations and resolves declared types from sema scopes.
- **Elvis operator** (`?:`): `genElvis` extracts flag, condBr → some block (extract value) / none block (evaluate default), phi merge.
- **Optional wrapping**: Assigning `T` to `T?` variable auto-wraps via `wrapOptional` (insertvalue `{ true, val }`).
- **Slices / Array literals**: Heap-allocated `i8*` → `{ i64 len, i64 cap, [data...] }`. 16-byte header + inline elements. `genArrayLit` mallocs, stores header via GEP, stores elements via typed GEP past header. Both `*types.Slice` and `*types.Array` map to `i8Ptr`.
- **Slice indexing**: Bounds-checked with `icmp ult` (unsigned, catches negative indices). Out-of-bounds calls `promise_panic` + `unreachable`. Read via `genSliceIndex`, write via `genSliceIndexAssign` (supports compound assignment like `arr[i] += 1`).
- **Maps**: Type-erased C runtime hash table (`runtime/runtime_map.c`). Open-addressing with FNV-1a hash, 75% load rehash. Entry layout: `[used:1][key_bytes][val_bytes]` inline. Functions: `promise_map_new`, `promise_map_set`, `promise_map_get`, `promise_map_len`, `promise_map_iter_next`.
- **String map keys**: Content-based hashing via `__promise_hash_string` / `__promise_eq_string` (codegen-emitted LLVM IR, dereference `i8*` to read string header). Byte-level hash/compare for primitive keys (NULL function pointers → default). `runtime_hash.c` fully eliminated.
- **Map indexing**: `m["key"]` returns `Optional(V)` — calls `promise_map_get`, checks NULL, wraps in `{ i1, V }` via phi merge. Assignment via `promise_map_set`.
- **For-in iteration**: `genForInStmt` dispatches on iterable type. Slices: counter loop with bounds check per element. Maps: `promise_map_iter_next` loop building `(K, V)` tuple per entry. Ranges: existing `genForInRange` extracted.
- **Lambdas (capturing)**: Anonymous LLVM functions (`.lambda.N`) with `i8* %env` as first parameter (uniform ABI). Fat pointer representation `{ i8*, i8* }` (fn ptr + env ptr) for all function values. Non-capturing lambdas use null env. Compiler state saved/restored (fn, block, locals, canError, scopeBindings, dropFlags). Handles both expression body (`|x| -> x + 1`) and block body (`|x| -> int { return x * 2; }`).
- **Lambda captures**: Sema capture analysis (`checkLambdaCapture`) detects outer-scope variable references via scope chain traversal. `Copy` types auto-captured by copy; non-`Copy` types require explicit `move` keyword. Captures recorded in `info.LambdaCaptures` (deterministic order via sorted names). Nested lambda capture propagation: inner captures from grandparent scopes automatically propagate to intermediate lambdas.
- **Lambda env struct**: Heap-allocated struct holding captured values (`malloc`). Captures loaded from enclosing scope allocas, stored into env fields. Inside lambda body, env is bitcast to typed pointer, fields extracted into local allocas. Move-captured droppable types registered for drop inside lambda body. Env struct freed at scope exit via `bindingFreeEnv` binding (drop-flag-guarded, null-checked `free()`).
- **Lambda calls**: `genCallExpr` detects local variables with `*types.Signature` type before regular function lookup. Loads fat pointer `{ i8*, i8* }`, calls `genIndirectCall` which extracts fn/env, bitcasts to typed function pointer with env-first ABI, calls with env as first arg.
- **Named function references**: When a named function is used as a first-class value (e.g., `f := add`), a thunk with env-first ABI is generated (`.thunk.add`) that forwards to the original function. Fat pointer uses `{ @.thunk.add, null }`.
- **Lambda ownership**: Move captures mark the variable as `Moved` in the enclosing scope. Captured variables are `Owned` inside the lambda body. Copy captures leave the original variable usable.
- **Intrinsics** (`compiler.go`): 7 new map runtime functions declared in `declareIntrinsics`. `lambdaCounter` and `targetType` fields added to Compiler.
- **Scope**: Tuple literals/destructure/return, optional none/some/wrapping/elvis, array literals, slice/array indexing (read/write/compound), for-in over slices/arrays/maps, map literals/indexing/assignment, capturing lambdas (expression/block body, indirect calls, copy/move captures, nested capture propagation, env allocation/cleanup, named function reference thunks).
- **Deferred**: Slice growth (`.push()`), container methods (`.contains`), fixed-size arrays as stack-allocated `[N x T]`, `llvmTypeSize` struct alignment (current implementation sums without padding — correct for primitive elements, under-allocates for struct-typed slice elements). String interpolation, if-unwrap/while-unwrap, optional chaining, and unsafe blocks completed in Stage 8h. Container `.len` completed in Stage 8i.

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
  - **Runtime**: `promise_int_to_string`, `promise_f64_to_string`, `promise_bool_to_string` conversion functions in `runtime_string.c` using `snprintf`.
  - **Codegen**: `genStringLit` split into `genStaticString` (compile-time, no interpolation) and `genInterpolatedString` (runtime). `convertToString` handles all primitive types with sext/zext/fpext as needed. Parts concatenated via `promise_string_concat`.
  - **Intrinsics**: 3 new conversion functions declared in `declareIntrinsics`.
- **Unsafe blocks**: `genUnsafeExpr` trivially generates block contents. Ownership analysis handles the "unsafe" semantics, not codegen.
- **Scope**: If-unwrap (with/without else), while-unwrap (with break/continue), optional chaining on user type fields, string interpolation with identifiers/literals/expressions/multiple parts, unsafe blocks.
- **Deferred**: `is`/`as` expressions (need RTTI), generators (`yield`), concurrency (`go`, `task`, `channel`), container methods (`.push`, `.pop`, `.contains`), user type `toString()` for interpolation. Container `.len` completed in Stage 8i.

## Stage 8i — Char Literals, Container `.len`, String Iteration & Map Compound Assignment (Done)

Codegen for char literals, `.len` property on all containers (string, slice, array, map), for-in over strings, and map compound assignment.

**Files:** Updates to `codegen/expr.go`, `codegen/stmt.go`, `codegen/compiler.go`, `codegen/types.go`, `codegen/native.go`, `sema/expr.go`, `sema/stmt.go`, `runtime/runtime_string.c`, `types/container.go`; 25 new tests (19 codegen → 227 total, 6 sema → 214 total)

- **Char literals**: `genCharLit` parses raw text including escape sequences (`\n`, `\t`, `\r`, `\b`, `\\`, `\'`, `\0`), returns i32 constant. `CatChar` classification added to `types.go` with signed i32 comparisons in `native.go`.
- **Container `.len` property**: Uniform property access across all container types — `arr.len`, `m.len`, `s.len`. Slice/array reads i64 from heap header (GEP index 0). Map calls `promise_map_len`. String reads i64 from instance struct (GEP index 1). Sema extended with `Slice`/`Array`/`Map` cases in `checkMemberExpr` and `TypString` special case in Named handler.
- **For-in over strings**: `genForInString` iterates UTF-8 codepoints via `promise_string_next_char` runtime function. Byte position tracked in i64 alloca, -1 sentinel for end. Index variable (`for i, ch in s`) supported with separate counter.
- **Map compound assignment**: `genMapCompoundAssign` gets current value via `promise_map_get`, NULL-checks with panic on missing key, applies operator, stores back via `promise_map_set`. Sema fix unwraps Optional for operator lookup on map value type.
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
- **Deferred**: Virtual dispatch (vtable), destructure is-patterns (`x is Dog(name)`), generic type RTTI.

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
- **slice[T]**: `len` field, `push(T)`, `pop() → T?`, `contains(T) → bool`, `remove(int)`
- **map[K,V]**: `len` field, `contains(K) → bool`, `remove(K) → bool`, `keys() → K[]`, `values() → V[]`
- **string**: `len` field, `contains(string) → bool`, `starts_with(string) → bool`, `ends_with(string) → bool`, `index_of(string) → int?`, `trim() → string`, `split(string) → string[]`

**Runtime:**
- New `runtime_slice.c`: push (with realloc growth), pop, contains, remove
- Updated `runtime_map.c`: tombstone support (0=empty, 1=used, 2=tombstone), remove, contains, keys, values
- Updated `runtime_string.c`: ~~contains, starts_with, ends_with, index_of~~ (migrated to pure Promise), trim, split

**Sema/Codegen Migration:**
- ~35 `case *types.Slice:` / `case *types.Map:` switch cases migrated to `AsSlice`/`AsMap` helpers
- `.len` removed as special case — now a real field lookup
- Array delegates field/method lookup to TypSlice (rejects mutating methods like `push`/`remove`)

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
- **Factory constructors**: `` `factory `` annotation implies `` `variant `` placement. No `this` receiver (set to nil in `resolveMethodSignature`). Must have return type (typically `Self`). Called as `Type.factoryName()`. `` `final `` field writes restricted to locally-created instances via `factoryLocals` tracking.
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

## Stage 9 — Module System (Planned)

Module resolution and dependency management.

- URL-based module identity with version in path
- `use alias "url"` import processing
- Dependency graph construction from source
- Flat directory layout (no required `src/`)
- Cycle detection

## Stage 10 — CLI

Command-line interface. Core commands implemented; formatter planned.

- `promise build [-o output] <file.pr>` — compile to binary
- `promise run <file.pr>` — compile and execute
- `promise check <file.pr>` — type-check only
- `promise test <file.pr>` — discover and run `test` meta-annotated functions
- `promise test <dir>` — scan directory for `.pr` files and run tests from each
- `promise test <dir>/...` — recursive directory scan (Go-style `...` wildcard)
- `promise ast <file.pr>` — print the AST
- `promise exec <code>` — execute inline code (auto-wraps in `main()` if needed)
- `promise install` — install compiler + std + runtime to `~/.promise/`
- Bare pipe detection: `echo '<code>' | promise` auto-enters exec mode
- Inline error formatting: source line + `^` caret marker, no temp filenames
- Embedded `std/` and `runtime/` in the binary via `go:embed` for self-contained install
- **Standard library test suite** (`tests/std/`): 11 test files, 104 tests covering int, uint, float, bool, char, string, hash, Vector, map, math, and range
- `promise fmt` — code formatter (planned)

## Stage 11 — Package Manager (Planned)

Dependency fetching and resolution.

- Fetch from git/HTTP URLs
- Lockfile generation and deterministic builds
- Diamond dependency resolution
- Cache management

---

## Deferred Work Tracker

Consolidated list of items deferred from completed stages. Items marked ~~strikethrough~~ were completed in later stages.

### Correctness Bugs

| Item | Origin | Priority |
|------|--------|----------|
| ~~`llvmTypeSize` struct alignment — sums field sizes without padding, under-allocates for struct-typed slice elements~~ | 8g | ~~Fixed 8o~~ |
| ~~Evaluation order in compound index assignment — RHS evaluated before LHS target/key~~ | 8i | ~~Fixed 8o~~ |
| Reassignment of droppable variable leaks old value — `x = newVal` overwrites without calling `drop()` on old | 8o | Medium |

### Codegen Gaps

| Item | Origin | Priority |
|------|--------|----------|
| ~~Capturing lambdas/closures~~ | 8g | ~~Done~~ |
| Fixed-size arrays as stack-allocated `[N x T]` | 8g | Medium |
| Destructure is-patterns (`x is Dog(name)`) | 8k | Medium |
| Generic type RTTI | 8k | Medium |
| Failable `close()` error propagation in `use` | 8m | Medium |
| Named enum fields in constructors | 8d | Low |
| Enum methods | 8d | Low |
| Extern ABI pack/unpack for enums | 8d | Low |
| Failable extern functions (C ABI for errors) | 8e | Low |
| Type argument inference (explicit type args only currently) | 8f | Low |
| Multi-arg generics in expression context (grammar limitation) | 8f | Low |
| Extern ABI for generic types | 8f | Low |
| Non-instance field placements (`value`/`variant`/`type`) | 8c | Low |
| ~~Default field values~~ | 8c | ~~Done 8n~~ |
| User type `toString()` for interpolation | 8h | Low |
| Devirtualization optimization (direct call when concrete type known) | 8L | Low |

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

### Runtime Migration (see `docs/runtime-proposal.md`)

| Item | Status |
|------|--------|
| Bitwise operators (`&`, `\|`, `^`, `<<`, `>>`, `~`) on int/uint types | Done |
| Numeric literal type inference (hex/binary/octal literals adopt context type) | Done |
| Primitive casting (`as!` for int↔uint, int↔char, etc.) | Done |
| FNV-1a hash in Promise (`std/hash.pr`) for int/bool/char/float types | Done |
| FNV-1a hash for string (codegen-emitted LLVM IR, no C dependency) | Done |
| Move string methods to Promise (contains, starts_with, ends_with, index_of) | Done |
| Move vector.contains/remove to codegen-emitted LLVM IR | Done |
| Move int/float/bool→string to Promise | Pending |

### Future Stages

| Item | Target |
|------|--------|
| Module system (URL-based imports, dependency graph) | Stage 9 |
| CLI: `promise fmt` code formatter | Stage 10 |
| Package manager (fetch, resolve, lock) | Stage 11 |
| Concurrency (`go`, `task`, `channel`, `<-`) | TBD |
| Generators (`yield`, `yield*`) | TBD |
| String slicing, Unicode normalization | TBD |

### Completed (resolved in later stages)

- ~~String interpolation~~ → 8h
- ~~If-unwrap / while-unwrap~~ → 8h
- ~~Optional chaining, unsafe blocks~~ → 8h
- ~~Container `.len`~~ → 8i
- ~~Container methods `.push`, `.pop`, `.contains`~~ → 8j
- ~~Vtable / virtual dispatch~~ → 8L
- ~~`is` / `as` expressions~~ → 8k
- ~~Slice growth `.push()`~~ → 8j
- ~~`llvmTypeSize` struct alignment~~ → 8o
- ~~Compound index assignment eval order~~ → 8o
