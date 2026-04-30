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
| Containers | `vector.pr`, `map.pr` | `Vector[T]` (push/pop/remove/contains/slice), `map[K,V]` (open-addressing, rehash) |
| I/O | `io.pr` | `println`, `print_int`, `print_f64`, `print_bool` (4 functions total) |
| Math | `math.pr` | `min`, `max`, `abs`, `clamp` (int only) |
| Interfaces | `equal.pr`, `ordered.pr`, `hashable.pr` | `Equal`, `Ordered`, `Hashable` structural types |
| Iterators | `iter.pr` | `iter[T]`, `stream[T]` abstract type stubs (no combinators) |
| Concurrency | `channel.pr`, `task.pr`, `runtime.pr` | Channel send/close, task handle, scheduler stats |
| Other | `range.pr`, `hash.pr`, `assert.pr` | Range struct, FNV-1a hash, `assert(bool, string)` |

**What's missing**: file I/O, string conversions (int↔string, parse), math functions (sqrt, sin, floor), error types, sorting, time, OS access, process execution, path manipulation, string builder, set type, stream/iterator combinators.

---

## 2. Prerequisites — Language Features to Complete

These features are incomplete or deferred and must be finished before the stdlib modules that depend on them.

### 2.1 Error Type System

| Aspect | Current State | Required Change |
|--------|--------------|-----------------|
| `error` type | Empty stub in `universe.go` (`TypError` = `defNamed("error")`) — no fields, no methods | Define `error` as a structural type with `get message string` |
| `raise` validation | `checkRaiseStmt` only verifies the call is inside a failable function — does not validate the raised value's type | Validate that the raised expression satisfies the `error` structural type |
| Error handler binding | `checkErrorHandlerExpr` binds `e` as `TypError` — but since `error` has no methods, `e` is useless | After `error` becomes structural, `e.message` works |

**Proposed `error` definition** (in `std/error.pr`):

```promise
type error `structural {
    get message string `abstract;
}
```

This makes any type with a `get message string` getter automatically satisfy `error`. User-defined error types:

```promise
type IoError {
    string msg;
    int code;
    get message string => this.msg;
}

type ParseError {
    string msg;
    int position;
    get message string => this.msg;
}
```

**Sema changes needed**:
- `checkRaiseStmt`: validate that the raised expression's type structurally satisfies `error`
- `checkErrorHandlerExpr`: already binds `e` as `TypError` — this becomes useful once `error` has methods

### 2.2 Stream/Iterator Combinators

| Aspect | Current State | Required Change |
|--------|--------------|-----------------|
| `iter[T]` | Abstract type with `next() T?` only | Remains as-is — single-pass cursor |
| `stream[T]` | Abstract type with `iter() iter[T]` only | Add combinator methods (lazy intermediate + eager terminal) |
| `for-in` desugaring | Not implemented in codegen | Desugar to `stream.iter()` + while loop |
| Collection `.iter()` | Not implemented | Vector, Map, string, range need `iter()` methods returning `iter[T]` |

**Proposed stream combinators** (in `std/iter.pr`):

```promise
type iter[T] `native {
    next() T? `abstract;
}

type stream[T] `native {
    iter() iter[T] `abstract;

    // Intermediate (lazy) — return new stream
    map[R](|T| R transform) stream[R];
    filter(|T| bool predicate) stream[T];
    take(int n) stream[T];
    skip(int n) stream[T];
    enumerate() stream[(int, T)];
    chain(stream[T] other) stream[T];
    zip[U](stream[U] other) stream[(T, U)];

    // Terminal (eager) — consume the stream
    fold[R](R init, |R, T| R combine) R;
    reduce(|T, T| T combine) T?;
    collect() T[];
    count() int;
    any(|T| bool predicate) bool;
    every(|T| bool predicate) bool;
    contains(T elem) bool;
    first() T?;
    last() T?;
    find(|T| bool predicate) T?;
    for_each(|T| void action);
    join(string sep) string;
    min() T?;
    max() T?;
}
```

**Implementation**: Each intermediate combinator returns a concrete internal type (e.g., `_MapStream[T,R]`, `_FilterStream[T]`) that wraps the source stream and implements `stream[R]`. Terminal combinators call `this.iter()` and loop via `next()`.

**Dependency**: Generators (`yield`) are designed but codegen-deferred. Stream combinators do NOT require generators — they can be implemented as wrapper types with custom `iter[T]` implementations. Generators are a future ergonomic improvement.

### 2.3 Numeric Type Conversions

| Aspect | Current State | Required Change |
|--------|--------------|-----------------|
| `as!` (unsafe cast) | Works for numeric widening/narrowing (confirmed in `std/hash.pr`) | No change needed |
| `as` (safe cast) | Works for inheritance downcasts; unclear for numeric | Define numeric `as` as safe widening only (e.g., `int as i64`) |
| Int↔String | Not implemented | Needed for `to_string()`, `parse_int()`, string interpolation of numbers |
| Float↔String | `print_f64` uses internal C `snprintf`; no Promise-level access | Need `f64.to_string()` and `parse_f64()` |

**Proposed approach**: Add `to_string()` as a native method on numeric types. Add `parse_int(string) int!` and `parse_f64(string) f64!` as failable free functions. The native implementations in codegen convert via C `snprintf`/`strtol`/`strtod` (these are pure computation, not I/O — acceptable as native).

### 2.4 `toString` for String Interpolation

| Aspect | Current State | Required Change |
|--------|--------------|-----------------|
| String interpolation | Works for `string`, `int`, `f64`, `bool`, `char` via hardcoded codegen paths | User types need `toString()` method support |
| `Printable` interface | Not defined | Define structural type with `to_string() string` |

**Proposed `Printable`** (in `std/string.pr` or `std/printable.pr`):

```promise
type Printable `structural {
    to_string() string `abstract;
}
```

String interpolation `"value: ${x}"` desugars to: if `x` is a primitive, use existing native path; if `x` has `to_string()`, call it; otherwise, compile error.

### 2.5 Feature Summary

| Feature | Blocks | Estimated Effort |
|---------|--------|-----------------|
| Error type system (2.1) | All failable stdlib APIs | Small — structural type + sema validation |
| Stream combinators (2.2) | Sorting, functional patterns | Medium — ~15 wrapper types + methods |
| Numeric conversions (2.3) | String formatting, parsing, math display | Medium — native codegen for to_string/parse |
| `toString` interpolation (2.4) | User-friendly output, debugging | Small — extend interpolation codegen |

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

Note: `EmitWrite` already exists in PAL for stdout/stderr (fd 1/2). `EmitFileRead` is new — the existing `EmitWrite` serves as `EmitFileWrite` when given an arbitrary fd.

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

**0a. Error type system**
- File: `std/error.pr` (new)
- Sema changes: `stmt.go:checkRaiseStmt`, `universe.go`

**0b. Stream combinators**
- File: `std/iter.pr` (extend)
- New internal types for lazy combinators

**0c. Numeric conversions**
- Files: `std/int.pr`, `std/uint.pr`, `std/float.pr` (extend)
- Codegen: native `to_string()` methods

**0d. Printable / toString**
- File: `std/printable.pr` (new) or extend `std/string.pr`
- Codegen: extend string interpolation

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
    iter() iter[T];
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

```promise
// Repeat a string n times
repeat(string s, int n) string;

// Pad string to target length
pad_left(string s, int width, char fill) string;
pad_right(string s, int width, char fill) string;

// Case conversion (ASCII only — Unicode deferred)
to_upper(string s) string;
to_lower(string s) string;

// Join a vector of strings
join(string[] parts, string sep) string;

// Replace all occurrences
replace(string s, string old, string new_val) string;

// Count occurrences of substring
count(string s, string sub) int;
```

Additional `string` methods (extend `std/string.pr`):

```promise
type string `native {
    // ... existing methods ...

    // New methods
    repeat(int n) string;
    to_upper() string;
    to_lower() string;
    replace(string old, string new_val) string;
    chars() char[];
}
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

#### 2a. Numeric to/from String

New native methods on primitive types:

```promise
type int `native {
    // ... existing ...
    to_string() string `native;
}

type uint `native {
    // ... existing ...
    to_string() string `native;
}

type f64 `native {
    // ... existing ...
    to_string() string `native;
    to_string_fixed(int decimals) string `native;
}

type bool `native {
    // ... existing ...
    to_string() string `native;
}

type char `native {
    // ... existing ...
    to_string() string `native;
}
```

Parsing functions (failable — raise on invalid input):

```promise
parse_int(string s) int!;
parse_uint(string s) uint!;
parse_f64(string s) f64!;
parse_bool(string s) bool!;
```

- **File**: Extend `std/int.pr`, `std/uint.pr`, `std/float.pr`, `std/bool.pr`, `std/char.pr`; new `std/parse.pr` for parse functions
- **Dependencies**: Error type (Phase 0a) for parse errors
- **Native codegen**: `snprintf` for int/float→string, `strtol`/`strtod` for string→number
- **Test**: `tests/std/test_convert.pr`

#### 2b. `std/string_builder.pr` — StringBuilder

```promise
type StringBuilder {
    // Efficient string construction via buffer

    new(~this);
    new(~this, int capacity);

    write(string s);
    write_char(char c);
    write_int(int n);
    write_f64(f64 n);

    get len int;
    to_string() string;
    clear();
}
```

- **File**: `std/string_builder.pr`
- **Dependencies**: `vector.pr` (backed by `u8[]` internally), numeric `to_string()` (Phase 2a)
- **Implementation**: Wraps a `Vector[u8]` with growth strategy. `to_string()` copies buffer to new string. Uses native `_sb_to_string` for the final copy.
- **Test**: `tests/std/test_string_builder.pr`

#### 2c. `std/fmt.pr` — Simple Formatting

```promise
// Format with positional placeholders: fmt("{} is {} years old", name, age)
// Each {} calls to_string() on the next argument
// Escape literal braces with {{  }}
//
// NOTE: Requires variadic generics or overloads for multiple arities.
// Initial implementation: fixed overloads for 1-6 arguments.

fmt1(string template, string a1) string;
fmt2(string template, string a2, string a2) string;
fmt3(string template, string a1, string a2, string a3) string;
// ... up to fmt6

// Alternative: use string interpolation directly (preferred for most cases)
// fmt() is for cases where the template is a runtime value
```

- **File**: `std/fmt.pr`
- **Dependencies**: `string_builder.pr`, `Printable` (Phase 0d)
- **Implementation**: Pure Promise — scan template for `{}`, splice arguments
- **Test**: `tests/std/test_fmt.pr`

---

### Phase 3: Math & Time

#### 3a. `std/math.pr` — Extended Math (LLVM Intrinsics)

```promise
// Extend existing std/math.pr which has: min, max, abs, clamp

// Constants
int MAX_INT;    // platform word-size max
int MIN_INT;    // platform word-size min
f64 PI;         // 3.14159265358979323846
f64 E;          // 2.71828182845904523536
f64 INF;        // positive infinity
f64 NEG_INF;    // negative infinity
f64 NAN;        // not-a-number

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

// Comparison
fabs(f64 x) f64 `native;
fmin(f64 a, f64 b) f64 `native;
fmax(f64 a, f64 b) f64 `native;

// Classification
is_nan(f64 x) bool;          // x != x
is_inf(f64 x) bool;
is_finite(f64 x) bool;

// Float min/max (generic overloads of existing int min/max)
min_f64(f64 a, f64 b) f64;
max_f64(f64 a, f64 b) f64;
abs_f64(f64 x) f64;
clamp_f64(f64 x, f64 lo, f64 hi) f64;
```

- **File**: `std/math.pr` (extend)
- **Dependencies**: None (LLVM intrinsics)
- **Native codegen**: Declare LLVM intrinsics (`@llvm.sqrt.f64`, etc.), generate wrapper functions
- **Implementation**: `native` functions backed by LLVM intrinsics. `tan`, `log2`, `log10`, `trunc`, `is_nan`, `is_inf`, `is_finite` implemented in pure Promise on top of native primitives.
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

#### 4a. `std/io.pr` — Extended I/O (Reader/Writer Interfaces)

```promise
// Structural interfaces for I/O
type Reader `structural {
    read(~this, u8[] ~buf) int!;
}

type Writer `structural {
    write(~this, u8[] &buf) int!;
}

type Closer `structural {
    close(~this)!;
}

// Convenience functions using Writer
write_string(Writer ~w, string s) int!;
write_line(Writer ~w, string s) int!;

// Read utilities using Reader
read_all(Reader ~r) u8[]!;
read_string(Reader ~r) string!;
```

Extend existing `std/io.pr` with the structural interfaces while keeping existing print functions.

- **File**: `std/io.pr` (extend)
- **Dependencies**: Error type (Phase 0a), `Vector[u8]`
- **Implementation**: Interfaces are pure structural types. Convenience functions are pure Promise.
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
| 0d | `std/printable.pr` | Promise | No | 10 |
| 1a | `std/set.pr` | Promise | No | 80 |
| 1b | `std/sort.pr` | Promise | No | 120 |
| 1c | `std/string_util.pr` | Promise | No | 80 |
| 1d | `std/result.pr` | Promise | No | 30 |
| 2a | `std/parse.pr` | Native | No | 40 |
| 2b | `std/string_builder.pr` | Mostly Promise | No | 60 |
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
