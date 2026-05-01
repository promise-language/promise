# Standard Library & Runtime Design

This document defines the complete standard library architecture for Promise. It identifies prerequisite language features that must be completed first, specifies all new PAL (Platform Abstraction Layer) extensions needed, and lays out the full API surface organized into implementation phases.

**Design principles** (from CLAUDE.md):
- Implement in Promise (`std/*.pr`) wherever possible — only use `native` for unavoidable low-level operations
- Small, orthogonal, predictable — an AI agent should be able to generate correct code without ecosystem knowledge
- Every effect visible at the call site — no hidden initialization, no implicit state

---

## Table of Contents

1. [Current State](#1-current-state)
2. [Prerequisites — Language Features to Complete](#2-prerequisites--language-features-to-complete)
3. [PAL Extensions](#3-pal-extensions)
4. [Module Design by Phase](#4-module-design-by-phase)
5. [Implementation Details](#5-implementation-details)
6. [Testing Strategy](#6-testing-strategy)

---

## 1. Current State

The stdlib today (20 files, ~730 lines) provides:

| Module | File | What it covers |
|--------|------|---------------|
| Primitives | `int.pr`, `uint.pr`, `float.pr`, `bool.pr`, `char.pr` | Arithmetic, comparison, bitwise, hash operators |
| Strings | `string.pr` | Concatenation, comparison, `contains`, `starts_with`, `ends_with`, `index_of`, `trim`, `split`, `[]`, `[:]` |
| Containers | `vector.pr`, `map.pr` | `Vector[T]` / `T[]` (push/pop/remove/contains/slice), `Map[K,V]` / `map[K,V]` (open-addressing, rehash) |
| I/O | `io.pr` | `println`, `print_int`, `print_f64`, `print_bool` (4 functions total) |
| Math | `math.pr` | `min`, `max`, `abs`, `clamp` (int only) |
| Interfaces | `equal.pr`, `ordered.pr`, `hashable.pr` | `Equal`, `Ordered`, `Hashable` structural types |
| Iterators | `iter.pr` | `Iterator[T]`, `Stream[T]` abstract type stubs (no combinators) |
| Concurrency | `channel.pr`, `task.pr`, `runtime.pr` | `Channel[T]` / `channel[T]` send/close, `Task[T]` / `task[T]` handle, scheduler stats |
| Other | `range.pr`, `hash.pr`, `assert.pr` | `Range` / `..`/`..=`, FNV-1a hash, `assert(bool, string)` |

**What's missing**: file I/O, string conversions (int↔string, parse), math functions (sqrt, sin, floor), error types, sorting, time, OS access, process execution, path manipulation, string builder, set type, stream/iterator combinators.

### Naming Conventions

Promise uses a two-tier naming scheme. Casing tells the reader whether a type is woven into the language itself or lives in library space.

**Lowercase — language-level types.** These have dedicated syntax, LLVM scalar representation, or are fundamental language concepts:

| Type | Why lowercase |
|------|--------------|
| `int`, `i8`..`i64`, `uint`, `u8`..`u64`, `f32`, `f64` | LLVM scalar integers/floats |
| `bool`, `char` | LLVM scalar `i1` / `i32` |
| `string` | Has literal syntax `"..."`, interpolation `${}` |
| `error` | Base type for `?`/`!`/`raise` error system |

**PascalCase — all other types.** Structural interfaces, user-defined types, stdlib types. Some PascalCase types have **syntactic sugar** — a lowercase shorthand the compiler resolves:

| Canonical Name | Sugar Form | Syntax |
|---------------|------------|--------|
| `Vector[T]` | `T[]` | `[1, 2, 3]` literals, `[]`/`[:]` indexing |
| `Map[K,V]` | `map[K,V]` | `[]`/`[]=` indexing |
| `Channel[T]` | `channel[T]` | `<-` send/recv operator |
| `Task[T]` | `task[T]` | `go { }` blocks, `<-` await |
| `Range` | — | `..` / `..=` operator creates Range values |

Types without sugar are always PascalCase: `Iterator[T]`, `Stream[T]`, `Writer`, `Reader`, `Format`, `Parse`, `Equal`, `Ordered`, `Hashable`, `Closer`, `Builder`, `Scanner`, `Set[T]`, `File`, `Duration`, `Instant`, `Random`, etc.

**The principle**: Promise's 4-struct type layout makes everything a full type — there is no primitive/object split. Lowercase signals *"this is part of the language — it has special operators, literals, or hardware representation."* PascalCase signals *"this is a library type."* The sugar forms let programmers write `int[]` and `map[string, int]` instead of `Vector[int]` and `Map[string, int]`, keeping common code concise while maintaining consistent PascalCase canonical names.

**Other naming rules**:

| Construct | Convention | Examples |
|-----------|-----------|---------|
| Methods & functions | `snake_case` | `to_string`, `read_file`, `skip_whitespace` |
| Properties & getters | `snake_case` | `is_empty`, `has_next`, `len` |
| Constants | `SCREAMING_SNAKE_CASE` | `MAX_INT`, `PI`, `NAN` |
| Enum variants | `PascalCase` | `Some`, `None`, `Empty`, `Used` |
| Parameters | `snake_case` | `default_val`, `capacity` |

---

## 2. Prerequisites — Language Features to Complete

These features are incomplete or deferred and must be finished before the stdlib modules that depend on them.

### 2.1 Error Type System — DONE

Fully implemented with inheritance-based error types, typed handlers, and exhaustiveness checking.

**Implementation** (in `std/error.pr`):

```promise
type error {
    string message;
}
```

User-defined error types use inheritance (`is error`):

```promise
type IoError is error {
    int code;
}

type ParseError is error {
    int position;
}

// Inheritance chains supported:
type TimeoutError is DbError is AppError is error { }
```

**What was implemented**:
- `checkRaiseStmt` (stmt.go): validates raised expression inherits from `error` via `InheritsFrom()`
- `checkErrorHandlerExpr` (expr.go): binds `e` to specific error subtype in typed handlers (`? e is IoError { }`), validates exhaustiveness in non-failable contexts, supports else clauses with optional binding
- Three operators: `?` (propagate), `!` (unwrap/panic), `? e { }` / `? e is T { }` (typed handler)
- Generic error types: `type GenericError[T] is error { T val; }`
- Result capture: `(val, err) := failable_call()`
- Error types cannot have `drop` methods (enforced in decl.go)
- 22+ Go unit tests in `sema_test.go`, 5 e2e test files covering construction, inheritance chains, typed handlers, nested handlers, and generic errors

### 2.2 Stream/Iterator Combinators

| Aspect | Current State | Required Change |
|--------|--------------|-----------------|
| `Iterator[T]` | Abstract type with `next() T?` only | Make `structural` interface — any type with `next() T?` satisfies it |
| `Stream[T]` | Abstract type with `iterator() Iterator[T]` only | Make `structural` interface, add combinator methods (lazy intermediate + eager terminal) |
| `for-in` desugaring | Not implemented in codegen | Desugar to `stream.iterator()` + while loop |
| Collection `.iterator()` | Not implemented | Vector, Map, string, range need `iterator()` methods returning `Iterator[T]` |

**Proposed stream combinators** (in `std/iter.pr`):

```promise
type Iterator[T] `structural {
    next() T? `abstract;
}

type Stream[T] `structural {
    iterator() Iterator[T] `abstract;

    // Intermediate (lazy) — return new stream
    map[R](|T| R transform) Stream[R];
    filter(|T| bool predicate) Stream[T];
    take(int n) Stream[T];
    skip(int n) Stream[T];
    enumerate() Stream[(int, T)];
    chain(Stream[T] other) Stream[T];
    zip[U](Stream[U] other) Stream[(T, U)];

    // Terminal (eager) — consume the stream
    fold[R](R init, |R, T| R combine) R;
    reduce(|T, T| T combine) T?;
    collect() T[];
    count() int;
    any(|T| bool predicate) bool;
    every(|T| bool predicate) bool;
    first() T?;
    last() T?;
    find(|T| bool predicate) T?;
    for_each(|T| void action);
}
```

**Implementation**: `Iterator[T]` and `Stream[T]` are structural interfaces — any type with matching methods automatically satisfies them. No `native` codegen needed. Each intermediate combinator returns a concrete internal type (e.g., `_MapStream[T,R]`, `_FilterStream[T]`) that wraps the source stream and structurally satisfies `Stream[R]`. Terminal combinators call `this.iterator()` and loop via `next()`. Collection types (Vector, Map, string, range) implement `iterator()` returning their own concrete iterator type that satisfies `Iterator[T]`.

**Dependency**: Generators (`yield`) are designed but codegen-deferred. Stream combinators do NOT require generators — they can be implemented as wrapper types whose concrete `next()` satisfies `Iterator[T]`. Generators are a future ergonomic improvement.

### 2.3 Numeric Type Conversions

| Aspect | Current State | Required Change |
|--------|--------------|-----------------|
| `as!` (unsafe cast) | Works for numeric widening/narrowing (confirmed in `std/hash.pr`) | No change needed |
| `as` (safe cast) | Works for inheritance downcasts; unclear for numeric | Define numeric `as` as safe widening only (e.g., `int as i64`) |
| Int↔String | Not implemented | Needed for `format()`, `to_string()`, `parse()`, string interpolation |
| Float↔String | `print_f64` uses internal C `snprintf`; no Promise-level access | Need `f64.format()` / `to_string()` and `f64.parse()` |

**Proposed approach**: Add native `format(Writer ~w)` on numeric types (Phase 2a) satisfying `Format`. Add `to_string() string` as a Promise-level method that wraps `format()`. Add native `parse(Reader ~r) Self!` factory methods satisfying `Parse` — each reads from a Reader, consuming only what it needs (e.g., digits for int). Generic `scan[int]("42")!` replaces individual `parse_int`/`parse_f64` free functions. Native codegen uses `snprintf`/`strtol`/`strtod`.

### 2.4 Format & Writer for String Interpolation

| Aspect | Current State | Required Change |
|--------|--------------|-----------------|
| String interpolation | Works for `string`, `int`, `f64`, `bool`, `char` via hardcoded codegen paths | User types need `format(Writer ~w)` support |
| `Writer` interface | Not defined | Define structural type for text output |
| `Format` interface | Not defined | Define structural type with `format(Writer ~w)` |
| `to_string()` | Not available on user types | Synthesized: format into Builder, extract string |

**Proposed types** (in `std/format.pr`):

```promise
type Writer `structural {
    // Required — any type with this method satisfies Writer
    write(~this, u8[] &buf) int! `abstract;

    // Default method — available on any Writer, no need to implement
    write_string(~this, string s) int! {
        return this.write(s.as_bytes());
    }
}

type Format `structural {
    format(Writer ~w) `abstract;
}
```

Writer is byte-oriented (like Go's `io.Writer`), making it usable for files, network, and any I/O — not just formatting. The `write_string` default method provides string convenience on top of the byte interface. Types only need to implement `write(u8[])` to satisfy Writer.

**Language feature: default methods on structural interfaces**. A structural interface can have both `abstract` methods (required for satisfaction) and non-abstract methods (default implementations, available on any satisfying type). This is similar to Rust traits or Java default interface methods.

**How it works**: `Format` types write their string representation into a `Writer` via `write_string()`. The caller controls the buffer, and multiple format calls compose without intermediate allocations.

`to_string()` is synthesized from `format()`: create a Builder (which satisfies `Writer`), call `format(~builder)`, return `builder.to_string()`. No need for types to implement `to_string()` separately.

**String interpolation** `"value: ${x}"` desugars to:
```promise
// compiler-generated:
mut _sb := Builder();
_sb.write_string("value: ");
x.format(~_sb);          // if x implements Format
_sb.to_string()
```

For primitives (`int`, `f64`, `bool`, `char`), the compiler uses existing native paths or built-in `format(Writer ~w)` implementations.

**Stream `join`**: With `Format`, stream combinators can offer `join(string separator)` as a terminal on `Stream[T: Format]` — each element formats into the shared builder with separators between them.

**Example user type**:
```promise
type Point {
    int x;
    int y;

    format(Writer ~w) {
        w.write_string("(");
        this.x.format(~w);
        w.write_string(", ");
        this.y.format(~w);
        w.write_string(")");
    }
}

// Now works in string interpolation:
p := Point(x: 3, y: 4);
println("point: ${p}");   // point: (3, 4)
```

### 2.5 Parse & Reader — Structural Interface on Factory Methods

| Aspect | Current State | Required Change |
|--------|--------------|-----------------|
| Parsing | No generic parsing — would need individual `parse_int`, `parse_f64`, etc. | Define `Parse` structural interface with factory method |
| Byte input | No reader abstraction | Define `Reader` structural interface (byte counterpart to Writer) |
| Structural matching | Matches instance methods and factory methods; abstract factories with implicit Self return | ~~Extend to match factory methods on the type itself~~ **Done** |
| Generic scanning | Not possible (factory infrastructure ready: `T.method()` works in generic context) | `scanner.next[T: Parse]() T!` becomes possible once `Reader` is defined |

**The problem**: `Format` works as a structural interface because it's an instance method — you have a value and call `value.format(~writer)`. Parsing is the inverse: you need to **create** a value by reading from a source. There's no instance to call a method on. The operation lives on the type, not on an instance. Additionally, a parser may not consume all the input — it should read what it needs and leave the rest.

**Proposed types** (in `std/parse.pr`):

```promise
type Reader `structural {
    // Required — any type with this method satisfies Reader
    read(~this, u8[] ~buf) int! `abstract;

    // Default method — reads n bytes and returns as string
    read_string(~this, int n) string! {
        mut buf := u8[](capacity: n);
        bytes_read := this.read(~buf)!;
        return string.from_bytes(buf);
    }

    // Default method — peek without consuming (requires buffered reader)
    // Concrete types may override for efficiency
    peek(~this, int n) u8[]!;
}

type Parse `structural {
    parse(Reader ~r) Self! `factory `abstract;
}
```

Reader is byte-oriented (like Go's `io.Reader`), making it usable for files, network, stdin, and parsing — one interface for everything. The `read_string` default method provides string convenience for parsers. Types only need to implement `read(u8[])` to satisfy Reader.

A type satisfies `Parse` if it has a factory method `parse(Reader ~r) Self!`. The parser reads what it needs from the Reader and stops — remaining content stays available for subsequent parses.

**Scanner** — a buffered Reader for parsing strings:

```promise
type Scanner {
    // Wraps a string source, tracks position. Satisfies Reader.

    new(~this, string source);

    // Reader interface (byte-oriented)
    read(~this, u8[] ~buf) int!;
    peek(~this, int n) u8[]!;

    // Parse the next value of type T (reads from current position)
    next[T: Parse]() T!;

    // Skip whitespace
    skip_whitespace(~this);

    // Remaining input
    rest() string;
    get has_next bool;
    get position int;
}
```

Because Scanner satisfies `Reader`, `next[T: Parse]()` simply calls `T.parse(~this)` — the Scanner passes itself as the Reader. A Scanner can also wrap a File or any other Reader for parsing from I/O sources.

**Generic convenience function**:

```promise
// Parse a full string as T (wraps in a Scanner internally)
scan[T: Parse](string s) T! {
    mut r := Scanner(source: s);
    return T.parse(~r);
}

// Usage:
x := scan[int]("42")!;
y := scan[f64]("3.14")!;
ok := scan[bool]("true")!;
```

**Primitive implementations**:

```promise
type int `native {
    // ... existing ...
    parse(Reader ~r) Self! `factory `native;   // reads digits, stops at non-digit
}

type f64 `native {
    // ... existing ...
    parse(Reader ~r) Self! `factory `native;   // reads float literal, stops at end
}

type bool `native {
    // ... existing ...
    parse(Reader ~r) Self! `factory `native;   // reads "true"/"false"
}
```

**User types** can implement Parse too:

```promise
type Point {
    int x;
    int y;

    parse(Reader ~r) Self! `factory {
        // parse "3,4" format
        px := int.parse(~r)!;
        comma := r.read_string(1)!;
        if comma != "," { raise error(message: "expected comma"); }
        py := int.parse(~r)!;
        return Point(x: px, y: py);
    }
}

p := scan[Point]("3,4")!;
```

Note how Point's parser composes with int's parser — each reads what it needs from the same Reader, advancing the position incrementally. The `read_string` default method makes text parsing convenient while the underlying interface is byte-oriented.

**Multiple values from one source**:

```promise
mut s := Scanner(source: "42 3.14 true");
x := s.next[int]()!;        // reads "42", stops at space
s.skip_whitespace();
y := s.next[f64]()!;        // reads "3.14", stops at space
s.skip_whitespace();
ok := s.next[bool]()!;      // reads "true"
```

**Parsing from a file** — same Reader interface:

```promise
mut f := File.open("data.txt")!;
mut s := Scanner(reader: ~f);    // Scanner wraps any Reader
x := s.next[int]()!;
```

**Language features required**:
1. **Structural factory matching**: extend structural interface matching to factory methods. When `T: Parse` is used, the compiler verifies the concrete type has `parse(Reader ~r) Self! `factory` and dispatches to it. Resolved at monomorphization time.
2. **Default methods on structural interfaces**: non-abstract methods with implementations (like `read_string` on Reader, `write_string` on Writer). Types get these for free when they implement the abstract methods.

**Symmetry with Format**:

| | Format (output) | Parse (input) |
|---|---|---|
| Structural interface | `Format` | `Parse` |
| I/O interface | `Writer` (bytes) | `Reader` (bytes) |
| Method | `format(Writer ~w)` | `parse(Reader ~r) Self!` |
| Method kind | Instance | Factory |
| Direction | Value → Writer | Reader → Value |
| Concrete wrapper | Builder (satisfies Writer) | Scanner (satisfies Reader) |
| String helper | `w.write_string(s)` | `r.read_string(n)` |
| Generic usage | `x.format(~writer)` | `T.parse(~reader)` |
| Convenience | string interpolation | `scan[T](string)` |
| Works with files | File satisfies Writer | File satisfies Reader |

### 2.6 Feature Summary

| Feature | Blocks | Status |
|---------|--------|--------|
| Error type system (2.1) | All failable stdlib APIs | **DONE** — inheritance-based errors, typed handlers, exhaustiveness |
| Stream combinators (2.2) | Sorting, functional patterns | Medium — ~15 wrapper types + methods |
| Numeric conversions (2.3) | String formatting, parsing, math display | Medium — native codegen for format/parse |
| Format & Writer (2.4) | String interpolation, user type display, stream join | Small — structural types + interpolation codegen |
| Parse (2.5) | Generic parsing, Scanner | Small — structural factory matching + native parse for primitives |

---

## 3. PAL Extensions

The PAL (Platform Abstraction Layer) isolates all OS interaction. Currently 20 methods covering memory, threads, mutexes, condvars, and CPU count. New methods needed:

### 3.1 File I/O

```go
// PAL interface additions
EmitFileOpen(module *ir.Module) *ir.Func    // i8* path, i32 flags, i32 mode → i32 (fd or -1)
EmitFileRead(module *ir.Module) *ir.Func    // i32 fd, i8* buf, i64 len → i64 (bytes read or -1)
EmitFileClose(module *ir.Module) *ir.Func   // i32 fd → i32 (0 or -1)
EmitFileStat(module *ir.Module) *ir.Func    // i8* path → {i64 size, i64 mtime_ns, i32 mode, i32 err}
EmitFileRemove(module *ir.Module) *ir.Func  // i8* path → i32 (0 or -1)
EmitFileMkdir(module *ir.Module) *ir.Func   // i8* path, i32 mode → i32 (0 or -1)
```

Note: `EmitFileWrite` already exists in PAL (currently used for stdout/stderr fd 1/2). `EmitFileRead` is new. `EmitFileWrite` works for any fd.

POSIX implementation: direct `open(2)`, `read(2)`, `close(2)`, `stat(2)`, `unlink(2)`, `mkdir(2)` syscall wrappers.

### 3.2 OS / Environment

```go
EmitGetEnv(module *ir.Module) *ir.Func      // i8* name → i8* (value or null)
EmitGetCwd(module *ir.Module) *ir.Func      // i8* buf, i64 len → i8* (path or null)
```

Command-line arguments: captured in `main()` prologue from `argc`/`argv` and stored in a global `string[]`. Exposed via `os_args() string[]`.

### 3.3 Time

```go
EmitNanotime(module *ir.Module) *ir.Func    // void → i64 (monotonic nanoseconds)
EmitWallClock(module *ir.Module) *ir.Func   // void → i64 (unix epoch nanoseconds)
EmitSleep(module *ir.Module) *ir.Func       // i64 nanoseconds → void
```

Note: `promise_nanotime` already exists as a hardcoded function in `io.go:defineNanotimeFunc()` using `clock_gettime(CLOCK_MONOTONIC)`. This should be migrated to a proper PAL method for portability. `EmitWallClock` uses `CLOCK_REALTIME`. `EmitSleep` uses `nanosleep(2)`.

### 3.4 Process Execution

```go
EmitProcessExec(module *ir.Module) *ir.Func // i8* path, i8** argv, i8** envp → i32 (pid or -1)
EmitProcessWait(module *ir.Module) *ir.Func // i32 pid → i32 (exit code)
```

### 3.5 Math (No PAL Needed)

Math functions use LLVM intrinsics directly — no PAL extension required:

```llvm
declare double @llvm.sqrt.f64(double)
declare double @llvm.sin.f64(double)
declare double @llvm.cos.f64(double)
declare double @llvm.pow.f64(double, double)
declare double @llvm.exp.f64(double)
declare double @llvm.log.f64(double)
declare double @llvm.fabs.f64(double)
declare double @llvm.floor.f64(double)
declare double @llvm.ceil.f64(double)
declare double @llvm.round.f64(double)
declare double @llvm.fma.f64(double, double, double)
declare double @llvm.minnum.f64(double, double)
declare double @llvm.maxnum.f64(double, double)
```

These are pure computational intrinsics that map directly to hardware instructions. Declared in codegen, wrapped by Promise functions in `std/math.pr`.

### 3.6 String Utilities

```go
EmitMemcmp(module *ir.Module) *ir.Func      // Already available via libc
EmitMemcpy(module *ir.Module) *ir.Func      // i8* dst, i8* src, i64 len → void
```

### 3.7 PAL Summary

| Category | New Methods | POSIX Backing |
|----------|-------------|---------------|
| File I/O | 6 | `open`, `read`, `close`, `stat`, `unlink`, `mkdir` |
| OS | 2 | `getenv`, `getcwd` |
| Time | 3 | `clock_gettime` (×2), `nanosleep` |
| Process | 2 | `posix_spawn` or `fork`+`exec`, `waitpid` |
| Math | 0 | LLVM intrinsics |
| String | 1 | `memcpy` |
| **Total** | **14** | |

---

## 4. Module Design by Phase

### Phase 0: Language Prerequisites

Complete the features from Section 2 before building stdlib modules.

**0a. Error type system — DONE**
- File: `std/error.pr` — defines `error` with `string message` field
- Sema: `checkRaiseStmt` validates inheritance, `checkErrorHandlerExpr` supports typed handlers with exhaustiveness
- Tests: 22+ sema tests, 5 e2e test files

**0b. Stream combinators**
- File: `std/iter.pr` (extend)
- New internal types for lazy combinators

**0c. Numeric conversions**
- Files: `std/int.pr`, `std/uint.pr`, `std/float.pr` (extend)
- Codegen: native `format(Writer ~w)` methods, parse functions

**0d. Format & Writer**
- File: `std/format.pr` (new) — `Writer` and `Format` structural interfaces
- Codegen: extend string interpolation to desugar `${x}` to `x.format(~builder)`
- Primitives get built-in `format(Writer ~w)` implementations

**0e. Parse & structural factory matching**
- File: `std/parse.pr` (new) — `Parse` structural interface with factory method, `Reader` structural interface, `Scanner` type
- Language feature: extend structural interface matching to factory methods
- Language feature: default methods on structural interfaces (Reader provides `read_string`, `peek`)
- Primitives get built-in `parse(Reader ~r) Self!` factory implementations

---

### Phase 1: Pure Promise Modules (No New PAL)

These modules are implemented entirely in Promise, using only existing native operations.

#### 1a. `std/set.pr` — Set[T]

```promise
type Set[T: Hashable + Equal] {
    // Backed by map[T, bool] internally

    new(~this);

    get len int;
    get is_empty bool;

    add(T elem);
    remove(T elem) bool;
    contains(T elem) bool;
    clear();

    // Set operations
    union(Set[T] &other) Set[T];
    intersect(Set[T] &other) Set[T];
    difference(Set[T] &other) Set[T];
    is_subset(Set[T] &other) bool;

    // Iteration
    to_vector() T[];
    iterator() Iterator[T];
}
```

- **File**: `std/set.pr`
- **Dependencies**: `map.pr`, `hashable.pr`, `equal.pr`
- **Implementation**: Wrapper around `map[T, bool]`
- **Test**: `tests/std/test_set.pr`

#### 1b. `std/sort.pr` — Sorting

```promise
// Sort a vector in-place using natural ordering
sort[T: Ordered](T[] ~vec);

// Sort with custom comparator
sort_by[T](T[] ~vec, |T, T| bool less_than);

// Return sorted copy
sorted[T: Ordered](T[] &vec) T[];

// Return sorted copy with custom comparator
sorted_by[T](T[] &vec, |T, T| bool less_than) T[];

// Binary search (requires sorted input)
binary_search[T: Ordered](T[] &vec, T target) int?;
```

- **File**: `std/sort.pr`
- **Dependencies**: `ordered.pr`, `vector.pr`
- **Implementation**: Introsort (quicksort + heapsort fallback + insertion sort for small partitions). Pure Promise.
- **Test**: `tests/std/test_sort.pr`

#### 1c. `std/string_util.pr` — String Utilities

Additional `string` methods (extend `std/string.pr`):

```promise
type string `native {
    // ... existing methods ...

    // New methods
    repeat(int n) string;
    to_upper() string;
    to_lower() string;
    replace(string old, string new_val) string;
    pad_left(int width, char fill) string;
    pad_right(int width, char fill) string;
    chars() char[];
}
```

Free functions:

```promise
// Join a vector of strings
join(string[] parts, string sep) string;

// Count occurrences of substring
count(string s, string sub) int;
```

- **File**: `std/string_util.pr` (free functions), extend `std/string.pr` (methods)
- **Dependencies**: `string.pr`, `vector.pr`
- **Implementation**: Pure Promise using existing `string.[]`, `string.[:]`, `string.+`
- **Test**: `tests/std/test_string_util.pr`

#### 1d. `std/result.pr` — Result Utilities

```promise
// Unwrap with default value
unwrap_or[T](T! value, T default_val) T;

// Map the success value
map_result[T, R](T! value, |T| R transform) R!;

// Check if result is error
is_error[T](T! value) bool;
```

- **File**: `std/result.pr`
- **Dependencies**: Error type (Phase 0a)
- **Test**: `tests/std/test_result.pr`

---

### Phase 2: Conversion & Formatting

#### 2a. Numeric Formatting & Parsing

Native `format(Writer ~w)` on primitive types (satisfying `Format`), plus Promise-level `to_string()` wrappers:

```promise
type int `native {
    // ... existing ...
    format(Writer ~w) `native;               // writes decimal representation
    parse(Reader ~r) Self! `factory `native;  // reads digits, stops at non-digit
    to_string() string {                     // convenience wrapper
        mut b := Builder();
        this.format(~b);
        return b.to_string();
    }
}

type uint `native {
    // ... existing ...
    format(Writer ~w) `native;
    parse(Reader ~r) Self! `factory `native;
    to_string() string { mut b := Builder(); this.format(~b); return b.to_string(); }
}

type f64 `native {
    // ... existing ...
    format(Writer ~w) `native;               // writes shortest representation
    parse(Reader ~r) Self! `factory `native;  // reads float literal, stops at end
    to_string() string { mut b := Builder(); this.format(~b); return b.to_string(); }
}

type bool `native {
    // ... existing ...
    format(Writer ~w) `native;               // writes "true" or "false"
    parse(Reader ~r) Self! `factory `native;  // reads "true"/"false"
    to_string() string { mut b := Builder(); this.format(~b); return b.to_string(); }
}

type char `native {
    // ... existing ...
    format(Writer ~w) `native;
    to_string() string { mut b := Builder(); this.format(~b); return b.to_string(); }
}
```

Each primitive satisfies both `Format` (instance method) and `Parse` (factory method). `format()` and `parse()` are native; `to_string()` is pure Promise wrapping `format()`. Each `parse()` reads from a Reader, consuming only what it needs — e.g., `int.parse` reads digits and stops at the first non-digit.

- **File**: Extend `std/int.pr`, `std/uint.pr`, `std/float.pr`, `std/bool.pr`, `std/char.pr`
- **Dependencies**: Error type (Phase 0a), Format & Writer (Phase 0d), Parse & Reader (Phase 0e), Builder (Phase 2b)
- **Native codegen**: `snprintf` for formatting into writer, `strtol`/`strtod` for `parse()` factory (reading from Reader)
- **Implementation**: `format()` and `parse()` are native; `to_string()` is Promise wrapping `format()`
- **Test**: `tests/std/test_convert.pr`

#### 2b. `std/builder.pr` — Builder

```promise
type Builder {
    // Efficient string construction via buffer
    // Satisfies Writer — used as the target for Format and string interpolation

    new(~this);
    new(~this, int capacity);

    // Writer interface (satisfies Writer structural type)
    write(~this, u8[] &buf) int!;

    // Convenience methods (from Writer default + additional)
    write_string(~this, string s) int!;
    write_char(char c);

    get len int;
    to_string() string;
    clear();
}
```

- **File**: `std/builder.pr`
- **Dependencies**: `vector.pr` (backed by `u8[]` internally), Writer (Phase 0d)
- **Implementation**: Wraps a `Vector[u8]` with growth strategy. `write(u8[] &buf)` satisfies the `Writer` structural interface, making Builder the primary target for `Format`. `write_string` inherited as default from Writer. `to_string()` copies buffer to new string via native `_sb_to_string`.
- **Test**: `tests/std/test_builder.pr`

#### 2c. `std/fmt.pr` — Runtime Template Formatting

```promise
// Format with positional placeholders: fmt("{} is {} years old", name, age)
// Each {} calls format(~writer) on the next argument
// Escape literal braces with {{  }}
//
// NOTE: Requires variadic generics or overloads for multiple arities.
// Initial implementation: fixed overloads for 1-6 Format arguments.

fmt1[A: Format](string template, A a1) string;
fmt2[A: Format, B: Format](string template, A a1, B a2) string;
fmt3[A: Format, B: Format, C: Format](string template, A a1, B a2, C a3) string;
// ... up to fmt6

// Prefer string interpolation for compile-time templates.
// fmt() is for cases where the template is a runtime value.
```

- **File**: `std/fmt.pr`
- **Dependencies**: `builder.pr`, `Format` (Phase 0d)
- **Implementation**: Pure Promise — scan template for `{}`, call `arg.format(~builder)` for each placeholder
- **Test**: `tests/std/test_fmt.pr`

---

### Phase 3: Math & Time

#### 3a. `std/math.pr` — Extended Math (LLVM Intrinsics)

```promise
// Extend existing std/math.pr which has: min, max, abs, clamp (int only)
// Replace int-only versions with generic versions using Ordered constraint

// Constants
int MAX_INT;    // platform word-size max
int MIN_INT;    // platform word-size min
f64 PI;         // 3.14159265358979323846
f64 E;          // 2.71828182845904523536
f64 INF;        // positive infinity
f64 NEG_INF;    // negative infinity
f64 NAN;        // not-a-number

// Generic comparison (replaces int-only min/max/clamp)
min[T: Ordered](T a, T b) T;
max[T: Ordered](T a, T b) T;
clamp[T: Ordered](T x, T lo, T hi) T;

// Absolute value (int and f64 overloads — f64 uses llvm.fabs intrinsic)
abs(int x) int;
abs(f64 x) f64 `native;

// Floating-point math (backed by LLVM intrinsics)
sqrt(f64 x) f64 `native;
sin(f64 x) f64 `native;
cos(f64 x) f64 `native;
tan(f64 x) f64;              // sin(x) / cos(x)
pow(f64 base, f64 exp) f64 `native;
exp(f64 x) f64 `native;
log(f64 x) f64 `native;      // natural log
log2(f64 x) f64;             // log(x) / log(2)
log10(f64 x) f64;            // log(x) / log(10)

// Rounding
floor(f64 x) f64 `native;
ceil(f64 x) f64 `native;
round(f64 x) f64 `native;
trunc(f64 x) f64;            // floor for positive, ceil for negative

// Classification
is_nan(f64 x) bool;          // x != x
is_inf(f64 x) bool;
is_finite(f64 x) bool;
```

- **File**: `std/math.pr` (extend)
- **Dependencies**: None (LLVM intrinsics)
- **Native codegen**: Declare LLVM intrinsics (`@llvm.sqrt.f64`, etc.), generate wrapper functions
- **Implementation**: `native` functions backed by LLVM intrinsics. `min`, `max`, `clamp` are generic via Ordered constraint (pure Promise). `abs(f64)` uses `llvm.fabs.f64`; `abs(int)` is pure Promise. `tan`, `log2`, `log10`, `trunc`, `is_nan`, `is_inf`, `is_finite` implemented in pure Promise on top of native primitives.
- **Test**: `tests/std/test_math.pr`

#### 3b. `std/random.pr` — Pseudorandom Numbers

```promise
type Random {
    // xoshiro256** PRNG — fast, high-quality, reproducible

    new(~this, uint seed);

    // Core generation
    next_uint() uint;
    next_int() int;
    next_f64() f64;           // [0.0, 1.0)

    // Bounded generation
    next_int_range(int min, int max) int;    // [min, max)
    next_f64_range(f64 min, f64 max) f64;    // [min, max)

    // Utilities
    next_bool() bool;
    shuffle[T](T[] ~vec);
}
```

- **File**: `std/random.pr`
- **Dependencies**: `uint` bitwise operators (exist), `as!` casts (exist)
- **Implementation**: Pure Promise. xoshiro256** state is 4 `uint` fields. Seed expansion via splitmix64. Float conversion: mask top bits, OR into exponent, subtract 1.0.
- **Test**: `tests/std/test_random.pr`

#### 3c. `std/time.pr` — Duration & Instant

```promise
type Duration {
    // Immutable time duration (nanosecond precision)

    int nanos `value;

    // Factory constructors
    from_nanos(int ns) Self `factory;
    from_micros(int us) Self `factory;
    from_millis(int ms) Self `factory;
    from_secs(int s) Self `factory;
    from_mins(int m) Self `factory;
    zero() Self `factory;

    // Accessors
    get as_nanos int;
    get as_micros int;
    get as_millis int;
    get as_secs int;

    // Arithmetic
    +(Duration other) Duration;
    -(Duration other) Duration;
    *(int factor) Duration;

    // Comparison
    ==(Duration other) bool;
    !=(Duration other) bool;
    <(Duration other) bool;
    >(Duration other) bool;
    <=(Duration other) bool;
    >=(Duration other) bool;

    to_string() string;
}

type Instant {
    // Monotonic timestamp for measuring elapsed time

    int nanos;

    // Factory constructors
    now() Self! `factory;

    // Measurement
    elapsed() Duration!;
    duration_since(Instant &earlier) Duration;

    // Comparison
    ==(Instant other) bool;
    <(Instant other) bool;
    >(Instant other) bool;
}

// Utility
sleep(Duration d)!;
```

- **File**: `std/time.pr`
- **Dependencies**: PAL `EmitNanotime` (3.3), PAL `EmitSleep` (3.3), numeric `to_string()` (Phase 2a)
- **Native codegen**: `Instant.now()` calls PAL nanotime, `sleep()` calls PAL sleep
- **Implementation**: `Duration` is pure Promise (arithmetic on nanos). `Instant` needs two native operations (now, sleep).
- **Test**: `tests/std/test_time.pr`

---

### Phase 4: System I/O

#### 4a. `std/io.pr` — Extended I/O (Closer Interface, Utilities)

```promise
// Note: Writer is already defined in std/format.pr (Phase 0d)
// Note: Reader is already defined in std/parse.pr (Phase 0e)

type Closer `structural {
    close(~this)! `abstract;
}

// Convenience functions using Writer (from std/format.pr)
write_line(Writer ~w, string s)!;

// Read utilities using Reader (from std/parse.pr)
read_all(Reader ~r) u8[]!;
read_string(Reader ~r) string!;
```

Extend existing `std/io.pr` with Closer and I/O utilities while keeping existing print functions. `Writer` is reused from `std/format.pr` and `Reader` from `std/parse.pr` — File, TcpStream, etc. all satisfy the same interfaces used for formatting and parsing.

- **File**: `std/io.pr` (extend)
- **Dependencies**: Error type (Phase 0a), Writer (Phase 0d), Reader (Phase 0e), `Vector[u8]`
- **Implementation**: Closer is a pure structural type. Convenience functions are pure Promise.
- **Test**: `tests/std/test_io.pr`

#### 4b. `std/file.pr` — File System Access

```promise
type File {
    // File handle with RAII cleanup

    int fd;

    // Factory constructors
    open(string path) Self! `factory;                  // read-only
    create(string path) Self! `factory;                // write, create/truncate
    open_mode(string path, string mode) Self! `factory; // "r", "w", "a", "rw"

    // Reader/Writer implementation
    read(~this, u8[] ~buf) int!;
    write(~this, u8[] &buf) int!;

    // Convenience methods
    read_all() string!;
    write_string(string s) int!;

    // Resource management
    close(~this)!;
}

// Free functions for one-shot operations
read_file(string path) string!;
write_file(string path, string content)!;
append_file(string path, string content)!;
file_exists(string path) bool;
remove_file(string path)!;
mkdir(string path)!;
```

- **File**: `std/file.pr`
- **Dependencies**: PAL file I/O (3.1), Error type (Phase 0a), `Reader`/`Writer`/`Closer` (4a)
- **Native codegen**: `File.open` → PAL `EmitFileOpen`, `File.read` → PAL `EmitFileRead`, etc.
- **Implementation**: Thin wrapper around PAL calls. Free functions construct/use/close a `File` internally.
- **Test**: `tests/std/test_file.pr`

#### 4c. `std/path.pr` — Path Manipulation

```promise
// Pure string-based path operations (no filesystem access)

path_join(string[] parts) string;
path_dir(string path) string;
path_base(string path) string;
path_ext(string path) string;
path_is_abs(string path) bool;
path_normalize(string path) string;
```

- **File**: `std/path.pr`
- **Dependencies**: `string.pr` methods only
- **Implementation**: Pure Promise string manipulation. Uses `/` as separator (POSIX-first; Windows support deferred).
- **Test**: `tests/std/test_path.pr`

#### 4d. `std/os.pr` — OS Interaction

```promise
// Environment
get_env(string name) string?;
get_cwd() string!;

// Command-line arguments (populated at program start)
os_args() string[];

// Process exit
exit(int code);

// Basic process execution
type ProcessResult {
    int exit_code;
    string stdout;
    string stderr;
}

exec(string program, string[] args) ProcessResult!;
```

- **File**: `std/os.pr`
- **Dependencies**: PAL OS (3.2), PAL process (3.4), Error type (Phase 0a)
- **Native codegen**: `get_env` → PAL `EmitGetEnv`, `os_args` → global captured in main, `exec` → PAL `EmitProcessExec` + `EmitProcessWait`
- **Test**: `tests/std/test_os.pr`

#### 4e. `std/stdin.pr` — Standard Input

```promise
// Read a line from stdin (blocking)
read_line() string!;

// Read all of stdin
read_stdin() string!;
```

- **File**: `std/stdin.pr`
- **Dependencies**: PAL `EmitFileRead` (fd 0)
- **Implementation**: Read into buffer until newline (for `read_line`) or EOF (for `read_stdin`).
- **Test**: `tests/std/test_stdin.pr`

---

### Phase 5: Future Modules (Design Only)

These modules are lower priority. Full API design to be done when dependencies are ready.

#### 5a. `std/json.pr` — JSON Parsing/Serialization

```promise
enum JsonValue {
    Null,
    Bool(bool value),
    Number(f64 value),
    Str(string value),
    Array(JsonValue[] items),
    Object(map[string, JsonValue] entries),
}

parse_json(string input) JsonValue!;
to_json(JsonValue value) string;
```

- **Dependencies**: Phase 2 (string operations), Phase 0a (error types)
- **Implementation**: Recursive descent parser in pure Promise

#### 5b. `std/regex.pr` — Regular Expressions

```promise
type Regex {
    compile(string pattern) Self! `factory;
    is_match(string input) bool;
    find(string input) Match?;
    find_all(string input) Match[];
    replace(string input, string replacement) string;
}

type Match {
    string value;
    int start;
    int end;
}
```

- **Dependencies**: Phase 1 (string utilities)
- **Implementation**: Thompson NFA in pure Promise (no PCRE dependency)

#### 5c. `std/net.pr` — TCP Networking

```promise
type TcpListener {
    bind(string addr, int port) Self! `factory;
    accept() TcpStream!;
    close(~this)!;
}

type TcpStream {
    connect(string addr, int port) Self! `factory;
    read(~this, u8[] ~buf) int!;
    write(~this, u8[] &buf) int!;
    close(~this)!;
}
```

- **Dependencies**: PAL socket extensions, IO reactor (epoll/kqueue)
- **Note**: Requires significant PAL work and potentially goroutine-aware I/O integration

#### 5d. `std/http.pr` — HTTP Client

```promise
type HttpResponse {
    int status;
    map[string, string] headers;
    string body;
}

http_get(string url) HttpResponse!;
http_post(string url, string body, map[string, string] headers) HttpResponse!;
```

- **Dependencies**: `std/net.pr`, `std/json.pr`

#### 5e. `std/crypto.pr` — Cryptographic Hashing

```promise
sha256(u8[] data) u8[];
sha256_hex(u8[] data) string;
```

- **Implementation**: Pure Promise (SHA-256 is ~100 lines of bit manipulation)

---

## 5. Implementation Details

### 5.1 Extern Function Pattern

For each PAL-backed function, the implementation follows this pattern:

**1. Declare in `std/X.pr`:**
```promise
_pal_file_open(string path, int flags, int mode) int `extern("promise_file_open");
```

**2. Define body in `io.go:definePALBodies()`:**
```go
func (c *Compiler) definePromiseFileOpen() {
    fn := c.module.NewFunc("promise_file_open", sretType,
        ir.NewParam("path", i8PtrType),
        ir.NewParam("flags", i8PtrType),
        ir.NewParam("mode", i8PtrType))
    entry := fn.NewBlock("entry")
    // Extract path string data + null-terminate
    // Call pal.EmitFileOpen(...)
    // Pack result into sret
    entry.NewRet(result)
}
```

**3. Add PAL method to interface + POSIX implementation:**
```go
// pal.go
EmitFileOpen(module *ir.Module) *ir.Func

// posix.go
func (p *PosixPAL) EmitFileOpen(module *ir.Module) *ir.Func {
    openFn := module.NewFunc("open", irtypes.I32,
        ir.NewParam("path", irtypes.I8Ptr),
        ir.NewParam("flags", irtypes.I32),
        ir.NewParam("mode", irtypes.I32))
    // ...wrapper...
}
```

**4. Wrap in Promise-level API:**
```promise
type File {
    int fd;
    open(string path) Self! `factory {
        fd := _pal_file_open(path, 0, 0);
        if fd < 0 { raise IoError(msg: "failed to open file", code: fd); }
        return File(fd: fd);
    }
}
```

### 5.2 LLVM Intrinsics Pattern (Math)

For LLVM intrinsic-backed functions:

**1. Declare intrinsic in codegen:**
```go
sqrtF64 := c.module.NewFunc("llvm.sqrt.f64", irtypes.Double,
    ir.NewParam("x", irtypes.Double))
sqrtF64.FuncAttrs = append(sqrtF64.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrReadNone)
```

**2. Mark as native in `std/math.pr`:**
```promise
sqrt(f64 x) f64 `native;
```

**3. Handle in native codegen dispatch** (in `compiler.go` native method/function handling):
```go
case "sqrt":
    raw := c.emitExpr(args[0]) // extract f64 raw
    result := block.NewCall(sqrtF64, raw)
    // wrap back into f64 value struct
```

### 5.3 Testing Pattern

Each stdlib module gets a corresponding test file:

```
std/X.pr          →  tests/std/test_X.pr      (Promise e2e tests)
                  →  internal/codegen/X_test.go (Go IR-shape tests, if native)
```

Promise test files use the `test` keyword:

```promise
test "sort empty vector" {
    mut v := int[]();
    sort(~v);
    assert(v.len == 0, "empty vector should remain empty");
}

test "sort integers" {
    mut v := [3, 1, 4, 1, 5, 9, 2, 6];
    sort(~v);
    assert(v[0] == 1, "first element should be 1");
    assert(v[7] == 9, "last element should be 9");
}
```

Run with:
```bash
promise test tests/std/test_sort.pr        # single file
promise test tests/std/                     # all std tests
bin/e2e.sh                                  # full e2e suite
```

### 5.4 stdAll Sync Requirement

When adding new types or functions to `std/*.pr`, the `stdAll` mini-stdlib in test files must be updated:

- `compiler/internal/codegen/codegen_test.go` — codegen tests
- `compiler/internal/sema/sema_test.go` — sema tests
- `compiler/internal/ownership/ownership_test.go` — ownership tests

Then run `make resources` to embed the updated stdlib.

---

## 6. Testing Strategy

### Test Categories

| Category | Location | What it verifies |
|----------|----------|-----------------|
| Go unit tests | `internal/codegen/*_test.go` | IR shape for native functions |
| Go unit tests | `internal/sema/*_test.go` | Type checking, error messages |
| Promise e2e | `tests/std/test_*.pr` | Runtime correctness |
| Promise e2e | `tests/e2e/*.pr` | Integration across modules |

### Test Coverage Requirements

| Phase | Required Tests |
|-------|---------------|
| Phase 0 (prerequisites) | Sema tests for error type validation, codegen tests for stream wrapper types |
| Phase 1 (pure Promise) | Promise e2e only — no native code involved |
| Phase 2 (conversions) | Codegen tests for native `to_string()` IR + Promise e2e for correctness |
| Phase 3 (math/time) | Codegen tests for LLVM intrinsic wiring + Promise e2e for value correctness |
| Phase 4 (system I/O) | Codegen tests for PAL call wiring + Promise e2e for file/os operations |

### Regression Safety

Before any stdlib change:
```bash
cd compiler && make test              # all Go tests pass
cd .. && bin/e2e.sh                   # all Promise tests pass
```

After:
```bash
cd compiler && make resources && make  # rebuild with new std
cd compiler && make test               # Go tests still pass
cd .. && bin/e2e.sh                    # all Promise tests pass (including new ones)
```

---

## Appendix: Complete Module Inventory

| Phase | File | Type | New PAL | Lines (est.) |
|-------|------|------|---------|-------------|
| 0a | `std/error.pr` | Promise | No | 15 |
| 0b | `std/iter.pr` | Promise | No | 300 |
| 0c | `std/int.pr` etc. | Native | No | 50 |
| 0d | `std/format.pr` | Promise | No | 15 |
| 0e | `std/parse.pr` | Promise | No | 15 |
| 1a | `std/set.pr` | Promise | No | 80 |
| 1b | `std/sort.pr` | Promise | No | 120 |
| 1c | `std/string_util.pr` | Promise | No | 80 |
| 1d | `std/result.pr` | Promise | No | 30 |
| 2a | `std/int.pr` etc. | Native + Promise | No | 60 |
| 2b | `std/builder.pr` | Mostly Promise | No | 60 |
| 2c | `std/fmt.pr` | Promise | No | 50 |
| 3a | `std/math.pr` | Native + Promise | No | 100 |
| 3b | `std/random.pr` | Promise | No | 80 |
| 3c | `std/time.pr` | Promise + Native | 3 | 120 |
| 4a | `std/io.pr` | Promise | No | 40 |
| 4b | `std/file.pr` | Promise + Native | 6 | 100 |
| 4c | `std/path.pr` | Promise | No | 60 |
| 4d | `std/os.pr` | Promise + Native | 2 | 60 |
| 4e | `std/stdin.pr` | Promise + Native | No | 30 |
| 5a | `std/json.pr` | Promise | No | 300 |
| 5b | `std/regex.pr` | Promise | No | 400 |
| 5c | `std/net.pr` | Promise + Native | 6+ | 150 |
| 5d | `std/http.pr` | Promise | No | 200 |
| 5e | `std/crypto.pr` | Promise | No | 150 |
| | **Total (Phases 0-4)** | | **11** | **~1365** |
| | **Total (all phases)** | | **17+** | **~2665** |
