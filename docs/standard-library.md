# Standard Library

Promise's standard library design: module inventory, implementation phases, PAL extensions needed, and testing strategy.

**Design principles** (from CLAUDE.md):
- Implement in Promise (`modules/std/*.pr`) wherever possible — only use `native` for unavoidable low-level operations
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

The stdlib today (29 files, ~2,440 lines) provides:

| Category | Files | What it covers |
|----------|-------|---------------|
| Primitives | `int.pr`, `uint.pr`, `float.pr`, `bool.pr`, `char.pr` | Arithmetic, comparison, bitwise, hash, `to_string()`, `format()`, `parse()`, `encode(Encoder)`, `decode(Decoder)` for all numeric/bool/char/string types |
| Strings | `string.pr` | Concatenation, comparison, `contains`, `starts_with`, `ends_with`, `index_of`, `trim`, `split`, `[]`, `[:]`, `bytes()`, `byte_at()`, `from_bytes()`, `to_string()`, `to_upper`, `to_lower`, `repeat`, `replace`, `count`, `chars` |
| Containers | `vector.pr`, `map.pr`, `set.pr` | `Vector[T]` / `T[]` (push/pop/remove/contains/slice/`filled`), `Map[K,V]` / `map[K,V]` (open-addressing, rehash), `Set[T]` |
| Format/Parse | `format.pr`, `builder.pr`, `parse.pr` | `Format` structural interface, `Builder` (string building, satisfies `Writer`), `Parse` structural interface, `Scanner` (string parsing, satisfies `Reader`), `scan[T]()` |
| I/O (std) | `io.pr` | `Reader` (read, read_byte) / `Writer` (write, write_string, write_line) / `Closer` structural interfaces, `print(Format)`, `print_line(Format)` |
| I/O (module) | `modules/io/io.pr` | `File` (open/create/append, read/write bytes, read_line, write_line, read_all, seek), `BufferedReader`, `BufferedWriter` (write_line), `Dir` (make/make_all/list/remove/exists), `IoError`, `read_line()`, `read_stdin()` |
| Path (module) | `modules/path/path.pr` | `path_join`, `path_dir`, `path_base`, `path_ext`, `path_is_abs`, `path_normalize` |
| Math | `math.pr`, `random.pr` | `min`, `max`, `abs`, `clamp`, `sqrt`, `sin`, `cos`, `tan`, `pow`, `exp`, `log`, `floor`, `ceil`, `round`, `Random` PRNG (xoshiro256**) |
| Sorting | `sort.pr` | `sort(T[])` for `Ordered` types (introsort) |
| Interfaces | `equal.pr`, `ordered.pr`, `hashable.pr` | `Equal`, `Ordered`, `Hashable` structural types |
| Iterators | `iter.pr` | `Iterator[T]` structural interface with 20 default combinator methods, `Stream[T]` structural interface, `_FnIter[T]` closure-based iterator, `Generator[T]` coroutine-based iterator, duck-typed for-in |
| Concurrency | `channel.pr`, `task.pr`, `runtime.pr` | `Channel[T]` / `channel[T]` send/close, `Task[T]` / `task[T]` handle, scheduler stats |
| Time | `time.pr` | `Duration` (value type, nanosecond precision), `Instant` (monotonic clock), `sleep()` |
| Platform | `platform.pr` | `Platform` type with `line_separator`, `path_separator`, `is_path_separator()` — compile-time `` `target `` filtering for Windows/POSIX |
| Serialization | `encode.pr` | `Encoder`/`Decoder` (non-structural), `Encodable`/`Decodable` (structural), `DecodeError` |
| Other | `range.pr`, `hash.pr`, `assert.pr`, `error.pr` | `Range` / `..`/`..=`, FNV-1a hash, `assert(bool, string)`, `error` base type |

**Catalog modules** (separate `promise.toml`, imported via `use <name>;`):

| Module | File | Lines | Status |
|--------|------|-------|--------|
| `io` | `modules/io/io.pr` | 501 | **Done** — `File` (open/create/append, read/write bytes, read_line, write_line, read_all, seek), `BufferedReader`, `BufferedWriter`, `Dir` (make/make_all/list/remove/exists), `IoError`, `read_line()`, `read_stdin()`. 69 tests. |
| `path` | `modules/path/path.pr` | 192 | **Done** — `join`, `file_name`, `parent`, `extension`, `is_absolute`, `normalize`. 13 tests. |
| `strings` | `modules/strings/strings.pr` | 65 | **Done** — `join`, `spaces`, `reverse`, `is_blank`, `repeat_join`. 10 tests. |
| `math` | `modules/math/math.pr` | 67 | **Done** — `lerp`, `map_range`, `deg_to_rad`, `rad_to_deg`, `sign`, `sign_f64`, `is_even`, `is_odd`, `gcd`, `lcm`. 26 tests. |
| `json` | `modules/json/json.pr` | ~900 | **Done** — `JsonEncoder` (is Encoder), `JsonDecoder` (is Decoder), generic `encode_string[T]`/`decode_string[T]`/`encode_string_pretty[T]`, `JsonValue` enum with methods (`is_null`..`is_object`, `as_bool`..`as_object`, `get(key)`, `at(index)`, `encode`, `format`, `format_pretty`), `parse_value`. 157 tests. |
| `os` | `modules/os/os.pr` | 4 | **Done** — get_environment_variable, get_working_directory, exit_process, arguments, executable_path, execute, set_environment_variable, set_working_directory, Process/ProcessInput/ProcessOutput (streaming), environment (map), user_name, user_identifier, group_identifier, home_directory, hostname, process_identifier, Signal enum, setup_signal_handling, receive_signal |
| `time` | `modules/time/time.pr` | 4 | **Placeholder** — planned: extended time utilities beyond `std/time.pr` |
| `http` | `modules/http/http.pr` | 4 | **Placeholder** — planned: get, post, Request, Response, Server, Handler |

**What's missing**: Networking, HTTP. OS access (args, env, cwd, execute, set env, set cwd, streaming process, env listing, user/group info, hostname, pid, signal handling) is done.

### Naming Conventions

Promise uses a two-tier naming scheme. Casing tells the reader whether a type is woven into the language itself or lives in library space.

**Lowercase — language-level types.** These have dedicated syntax, LLVM scalar representation, or are fundamental language concepts:

| Type | Why lowercase |
|------|--------------|
| `int`, `i8`..`i64`, `uint`, `u8`..`u64`, `f32`, `f64` | LLVM scalar integers/floats |
| `bool`, `char` | LLVM scalar `i1` / `i32` |
| `string` | Has literal syntax `"..."`, interpolation `{}` |
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

**Implementation** (in `modules/std/error.pr`):

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

### 2.2 Stream/Iterator Combinators — DONE

Fully implemented with structural interfaces, duck-typed for-in, all combinators (lazy intermediate + eager terminal), and generic combinators (map[R], fold[R], zip[U], enumerate, flat_map[R]).

**Implementation** (in `modules/std/iter.pr`):

- `Iterator[T]` — structural interface with `next() T?` abstract method and 20 default combinator methods
- `_FnIter[T]` — closure-based iterator (`is Iterator[T]`) with `() -> T?` function-typed field
- `Stream[T]` — structural interface with `iter() Iterator[T]` abstract method (duck-typed for-in)
- `Generator[T]` — coroutine-based iterator (`is Iterator[T]`)
- Duck-typed for-in: any type with `next() T?` or `iter()` returning iterator works in `for` loops
- `Vector[T].iter()` returns `Iterator[T]`
- All combinators: `filter`, `take`, `skip`, `take_while`, `skip_while`, `chain`, `map[R]`, `zip[U]`, `enumerate`, `flat_map[R]` (lazy), `collect`, `count`, `fold[R]`, `reduce`, `any`, `every`, `first`, `last`, `find`, `for_each` (eager)
- Tests: 104 e2e tests in `tests/std/iter_test.pr`, 9 sema tests, 6 codegen tests

### 2.3 Numeric Type Conversions — DONE

| Aspect | Status |
|--------|--------|
| `as`/`as!` scalar casts | **Done** — all scalar types (int, i8-i64, uint, u8-u64, f32, f64, char, bool) castable via `as`/`as!` |
| char ↔ integer | **Done** — `'A' as int` → 65, `65 as char` → 'A' (zext/trunc) |
| bool ↔ integer/float | **Done** — `true as int` → 1, `42 as bool` → true (icmp ne 0), `0 as bool` → false |
| float → bool | **Done** — `0.0 as bool` → false, any non-zero → true including NaN (fcmp une 0.0) |
| Int↔String | **Done** — `to_string()`, `format(Writer ~w)!`, `int.parse(Reader ~r) int!` |
| Float↔String | **Done** — `to_string()`, `format(Writer ~w)!`, `f64.parse(Reader ~r) f64!` |

**Implemented approach**: `as`/`as!` work identically for scalar types (both return target type directly, no optional). For polymorphic casts (inheritance), `as` returns optional, `as!` panics on mismatch. All primitives have `to_string()` via `"{this}"` and `format(Writer ~w)!`. `int.parse`, `bool.parse`, `uint.parse`, `f64.parse` are pure Promise. No snprintf/strtol needed.

**Key codegen detail**: `int → bool` uses `icmp ne val, 0` (not `trunc`, which would give wrong result for even numbers like 2). `float → bool` uses `fcmp one val, 0.0`. Tests: 32 e2e tests in `tests/e2e/scalar_casts_test.pr`, 9 sema tests, 6 codegen tests.

### 2.4 Format & Writer for String Interpolation — DONE

| Aspect | Current State | Remaining |
|--------|--------------|-----------|
| String interpolation | Works for all primitives (`string`, `int`, `f64`, `bool`, `char`, etc.) and **user-defined types** implementing `format(Writer ~w)!` | — |
| `Writer` interface | **Defined** in `modules/std/format.pr` | — |
| `Format` interface | **Defined** in `modules/std/format.pr` — `format(Writer ~w)!` (failable) | — |
| `to_string()` | **Available** on all primitives via `"{this}"` | — |
| `format()` | **Available** on all primitives — delegates to `w.write_string(to_string())` | — |
| `Builder` | **Implemented** in `modules/std/builder.pr` (pure Promise) | — |
| User type interpolation | **Implemented** — `"{x}"` desugars to Builder + `x.format(~builder)!` + `builder.to_string()` for types implementing `Format` | — |

**Types** (in `modules/std/format.pr`):

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
    format(Writer ~w)! `abstract;
}
```

Writer is byte-oriented (like Go's `io.Writer`), making it usable for files, network, and any I/O — not just formatting. The `write_string` default method provides string convenience on top of the byte interface. Types only need to implement `write(u8[])` to satisfy Writer.

**Language feature: default methods on structural interfaces**. A structural interface can have both `abstract` methods (required for satisfaction) and non-abstract methods (default implementations, available on any satisfying type). This is similar to Rust traits or Java default interface methods.

**How it works**: `Format` types write their string representation into a `Writer` via `write_string()`. The caller controls the buffer, and multiple format calls compose without intermediate allocations. `format()` is failable because underlying `Writer.write()` may fail (e.g., I/O error).

`to_string()` is synthesized from `format()`: create a Builder (which satisfies `Writer`), call `format(~builder)!`, return `builder.to_string()`. No need for types to implement `to_string()` separately. Builder's `write()` never fails, so the `!` is safe.

**String interpolation** `"value: {x}"` desugars to:
```promise
// compiler-generated:
mut _sb := Builder();
_sb.write_string("value: ");
x.format(~_sb)!;          // if x implements Format
_sb.to_string()
```

All primitive types (`int`, `i8`-`i64`, `uint`, `u8`-`u64`, `f32`, `f64`, `bool`, `char`, `string`) implement `format(Writer ~w)!`.

**Stream `join`**: With `Format`, stream combinators can offer `join(string separator)` as a terminal on `Stream[T: Format]` — each element formats into the shared builder with separators between them.

**Example user type**:
```promise
type Point {
    int x;
    int y;

    format(Writer ~w)! {
        w.write_string("(");
        this.x.format(~w);
        w.write_string(", ");
        this.y.format(~w);
        w.write_string(")");
    }
}

// Now works in string interpolation:
p := Point(x: 3, y: 4);
print_line("point: {p}");   // point: (3, 4)
```

### 2.5 Parse & Reader — Structural Interface on Factory Methods — DONE

| Aspect | Status | Details |
|--------|--------|---------|
| Parsing | **Done** | `int.parse`, `bool.parse`, `uint.parse`, `f64.parse` — all pure Promise |
| Byte input | **Done** | `Reader` structural interface in `modules/std/parse.pr` with `read_byte` default method |
| `Scanner` | **Done** | Wraps string, satisfies Reader, tracks position. In `modules/std/parse.pr` |
| `scan[T]()` | **Done** | Generic convenience: `scan[int]("42")!` — wraps string in Scanner, calls `T.parse` |

**The problem**: `Format` works as a structural interface because it's an instance method — you have a value and call `value.format(~writer)`. Parsing is the inverse: you need to **create** a value by reading from a source. There's no instance to call a method on. The operation lives on the type, not on an instance. Additionally, a parser may not consume all the input — it should read what it needs and leave the rest.

**Proposed types** (in `modules/std/parse.pr`):

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
| Method | `format(Writer ~w)!` | `parse(Reader ~r) Self!` |
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
| Stream combinators (2.2) | Sorting, functional patterns | **DONE** — Iterator[T] structural interface with 20 default combinators, _FnIter[T], duck-typed for-in, Vector.iter(), 104 e2e tests |
| Numeric conversions (2.3) | String formatting, parsing, math display | **DONE** — all scalar as/as! casts (numeric, char, bool), `to_string()` + `format()` on all primitives, `int/bool/uint/f64.parse` |
| Format & Writer (2.4) | String interpolation, user type display, stream join | **DONE** — Writer/Format interfaces defined, Builder implemented, `format(Writer ~w)!` on all primitives. User-defined type interpolation via Format now implemented |
| Parse & Reader (2.5) | Generic parsing, Scanner | **DONE** — Reader/Parse interfaces, Scanner, `scan[T]()`, `int/bool/uint/f64.parse` |

---

## 3. PAL Extensions

The PAL (Platform Abstraction Layer) isolates all OS interaction. Currently 47 methods covering memory (5), threads/sync (11), CPU count (1), file I/O (12), OS/environment (5), process execution (5: spawn, read_pipe, wait_pid, spawn_streaming, kill), OS info (3: get_environ, get_user_info, get_hostname), signal handling (2: signal_init, signal_register), and directory listing (3). New methods needed:

### 3.1 File I/O — Done

12 PAL methods implemented in `codegen/pal/` across POSIX, Windows, and WASM:

```go
EmitFileOpen(module *ir.Module) *ir.Func      // i8* path, i32 mode → i32 (fd or -1)
EmitFileRead(module *ir.Module) *ir.Func      // i32 fd, i8* buf, i64 len → i64 (bytes read or -1)
EmitFileWrite(module *ir.Module) *ir.Func     // i32 fd, i8* buf, i64 len → i64 (bytes written or -1)
EmitFileClose(module *ir.Module) *ir.Func     // i32 fd → i32 (0 or -1)
EmitFileSeek(module *ir.Module) *ir.Func      // i32 fd, i64 offset, i32 whence → i64 (position or -1)
EmitFileStatSize(module *ir.Module) *ir.Func  // i8* path → i64 (size or -1, via open+lseek+close)
EmitFileRemove(module *ir.Module) *ir.Func    // i8* path → i32 (0 or -1)
EmitFileExists(module *ir.Module) *ir.Func    // i8* path → i32 (1=yes, 0=no)
EmitFileMkdir(module *ir.Module) *ir.Func     // i8* path → i32 (0 or -1)
EmitDirRemove(module *ir.Module) *ir.Func     // i8* path → i32 (0 or -1)
EmitDirExists(module *ir.Module) *ir.Func     // i8* path → i32 (1=yes, 0=no)
EmitErrno(module *ir.Module) *ir.Func         // → i32 (thread-local errno)
```

`EmitFileOpen` takes a mode enum (0=rw, 1=ro, 2=create-trunc, 3=append) mapped to platform O_* flags internally. `EmitFileStatSize` uses open+lseek(SEEK_END)+close to avoid `struct stat` layout portability issues. POSIX uses libc wrappers; Windows uses UCRT (`_open`, `_read`, etc.) with `_O_BINARY`; WASM stubs return error.

### 3.2 OS / Environment — Done

5 PAL methods:

```go
EmitGetEnv(module *ir.Module) *ir.Func      // i8* name → i8* (value or null)
EmitGetCwd(module *ir.Module) *ir.Func      // i8* buf, i64 len → i8* (path or null)
EmitSetEnv(module *ir.Module) *ir.Func      // i8* name, i8* value → i32 (0 or -1)
EmitUnsetEnv(module *ir.Module) *ir.Func    // i8* name → i32 (0 or -1)
EmitChdir(module *ir.Module) *ir.Func       // i8* path → i32 (0 or -1)
```

Command-line arguments: captured in `main()` prologue from `argc`/`argv` and stored in a global `string[]`. Exposed via `os.arguments` (module-level getter).

### 3.3 Time

```go
EmitNanotime(module *ir.Module) *ir.Func    // void → i64 (monotonic nanoseconds)
EmitWallClock(module *ir.Module) *ir.Func   // void → i64 (unix epoch nanoseconds)
EmitSleep(module *ir.Module) *ir.Func       // i64 nanoseconds → void
```

Note: `promise_nanotime` already exists as a hardcoded function in `io.go:defineNanotimeFunc()` using `clock_gettime(CLOCK_MONOTONIC)`. This should be migrated to a proper PAL method for portability. `EmitWallClock` uses `CLOCK_REALTIME`. `EmitSleep` uses `nanosleep(2)`.

### 3.4 Process Execution — DONE

5 PAL methods (3 original for one-shot execute, 2 new for streaming):

```go
// One-shot (used by execute())
EmitSpawn(module *ir.Module) *ir.Func       // i8* program, i8** argv, i32* out_stdout_fd, i32* out_stderr_fd → i32 (pid or -1)
EmitReadPipe(module *ir.Module) *ir.Func    // i32 fd, i8** out_buf, i64* out_len → void (reads to EOF, closes fd)
EmitWaitPid(module *ir.Module) *ir.Func     // i32 pid → i32 (exit code 0-255, or -1; retries EINTR)
// Streaming (used by Process.spawn())
EmitSpawnStreaming(module *ir.Module) *ir.Func // i8* program, i8** argv, i32* out_stdin_fd, i32* out_stdout_fd, i32* out_stderr_fd → i32 (pid or -1)
EmitKill(module *ir.Module) *ir.Func          // i32 pid, i32 signal → i32 (0 or -1)
// POSIX: fork + execvp + pipe (spawn/spawn_streaming), read loop + close (read_pipe), waitpid (wait_pid), kill(2) (kill)
// Windows/WASM: stubs returning -1
```

`execute()` in `modules/os/os.pr` reads stdout and stderr concurrently using `go _os_read_pipe(stderr_fd)` while the main goroutine reads stdout. This prevents deadlock when a child writes >64KB to stderr.

`Process.spawn()` creates stdin+stdout+stderr pipes. Pipe handles are obtained via `take_standard_input()` (returns `ProcessInput`, satisfies `Writer`), `take_standard_output()`/`take_standard_error()` (returns `ProcessOutput`, satisfies `Reader`). The streaming pipe read/write/close bridges reuse existing `pal_file_read`/`pal_file_write`/`pal_file_close` PAL functions (pipes are just fds).

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

These are pure computational intrinsics that map directly to hardware instructions. Declared in codegen, wrapped by Promise functions in `modules/std/math.pr`.

### 3.6 String Utilities

```go
EmitMemcmp(module *ir.Module) *ir.Func      // Already available via libc
EmitMemcpy(module *ir.Module) *ir.Func      // i8* dst, i8* src, i64 len → void
```

### 3.7 PAL Summary

| Category | New Methods | POSIX Backing |
|----------|-------------|---------------|
| File I/O | 12 (done) | `open`, `read`, `write`, `close`, `seek`, `stat_size`, `remove`, `exists`, `mkdir`, `dir_remove`, `dir_exists`, `errno` |
| OS / Env | 5 (done) | `getenv`, `getcwd`, `setenv`, `unsetenv`, `chdir` |
| Process | 5 (done) | `spawn` (`fork`+`execvp`+`pipe`), `read_pipe` (read+close), `wait_pid` (`waitpid`), `spawn_streaming` (stdin+stdout+stderr pipes), `kill` (`kill(2)`) |
| OS Info | 3 (done) | `get_environ` (environ global), `get_user_info` (`getpwuid`+`getuid`), `get_hostname` (`gethostname`) |
| Dir Listing | 3 (done) | `dir_open` (`opendir`), `dir_next_name` (`readdir`), `dir_close` (`closedir`) |
| Signal | 2 (done) | `signal_init` (pipe + handler), `signal_register` (`signal(2)`) |
| Time | 3 | `clock_gettime` (×2), `nanosleep` |
| Math | 0 | LLVM intrinsics |
| String | 1 | `memcpy` |
| **Total** | **32** (28 done) | |

---

## 4. Module Design by Phase

### Phase 0: Language Prerequisites

Complete the features from Section 2 before building stdlib modules.

**0a. Error type system — DONE**
- File: `modules/std/error.pr` — defines `error` with `string message` field
- Sema: `checkRaiseStmt` validates inheritance, `checkErrorHandlerExpr` supports typed handlers with exhaustiveness
- Tests: 22+ sema tests, 5 e2e test files

**0b. Stream combinators — DONE**
- File: `modules/std/iter.pr` — `Iterator[T]` structural interface with `next() T?` + 20 default combinator methods, `_FnIter[T]` closure-based iterator, `Stream[T]` structural interface, `Generator[T]` coroutine-based iterator
- Duck-typed for-in: `ForInKind` enum (ForInNext/ForInIter) in sema, `genForInCustomIter` in codegen
- `Vector[T].iter()` returns `Iterator[T]`
- All combinators: filter, take, skip, take_while, skip_while, chain, map[R], zip[U], enumerate, flat_map[R] (lazy), collect, count, fold[R], reduce, any, every, first, last, find, for_each (eager)
- Tests: 104 e2e tests in `tests/std/iter_test.pr`

**0c. Numeric conversions — DONE**
- Sema: `isScalarCastType()` extends `isNumericType()` with char and bool for `as`/`as!` casts
- Codegen: `emitScalarCast()` replaces `emitNumericCast()` with `int → bool` (icmp ne 0), `float → bool` (fcmp une 0.0 — NaN is truthy), char ↔ int (zext/trunc)
- All scalar types (int, i8-i64, uint, u8-u64, f32, f64, char, bool) are castable to each other via `as`/`as!`
- Int↔String, Float↔String: `to_string()`, `format(Writer ~w)!`, `int/bool/uint/f64.parse` — all pure Promise
- Tests: 32 e2e tests in `tests/e2e/scalar_casts_test.pr`, 9 sema tests, 6 codegen tests

**0d. Format & Writer — DONE**
- File: `modules/std/format.pr` — `Writer` and `Format` structural interfaces with default `write_string` method
- File: `modules/std/builder.pr` — `Builder` type (pure Promise, wraps `Vector[u8]`, satisfies `Writer`)
- Primitives have `to_string()` via string interpolation (`"{this}"`)
- All primitives (`int`, `i8`-`i64`, `uint`, `u8`-`u64`, `f32`, `f64`, `bool`, `char`, `string`) implement `format(Writer ~w)!`
- String interpolation desugaring to Format — **DONE**. User-defined types implementing `format(Writer ~w)!` are now supported in `{}` interpolation. A Builder is created internally, the type's format method writes to it, and the result is converted to string via `Builder.to_string()`. Both direct dispatch and vtable dispatch (polymorphic) are supported. Value types are also supported.

**0e. Parse & Reader — DONE**
- File: `modules/std/parse.pr` — `Reader` structural interface (with `read_byte` default), `Parse` structural interface with factory method, `Scanner` type, `scan[T]()` convenience function
- `int.parse(Reader ~r) int!`, `bool.parse(Reader ~r) bool!`, `uint.parse(Reader ~r) uint!`, `f64.parse(Reader ~r) f64!` — all pure Promise
- `string.from_bytes(u8[]) string` native factory, `string.bytes() u8[]` and `string.byte_at(int) u8` native methods
- `Vector[T].filled(T, int) T[]` factory for buffer pre-allocation
- Codegen: primitive scalar method receivers (i64 for int, double for f64, etc. instead of i8*)
- Sema: MutRef/SharedRef unwrapping in member access, native factory validation

---

### Phase 1: Pure Promise Modules (No New PAL)

These modules are implemented entirely in Promise, using only existing native operations.

#### 1a. `modules/std/set.pr` — Set[T]

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

- **File**: `modules/std/set.pr`
- **Dependencies**: `map.pr`, `hashable.pr`, `equal.pr`
- **Implementation**: Wrapper around `map[T, bool]`
- **Test**: `tests/std/set_test.pr` (13 tests)

#### 1b. `modules/std/sort.pr` — Sorting

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

- **File**: `modules/std/sort.pr`
- **Dependencies**: `ordered.pr`, `vector.pr`
- **Implementation**: Introsort (quicksort + heapsort fallback + insertion sort for small partitions). Pure Promise.
- **Test**: `tests/std/sort_test.pr` (10 tests)

#### 1c. String Utilities — DONE (split across `std/string.pr` + `modules/strings/`)

String methods (`to_upper`, `to_lower`, `repeat`, `replace`, `count`, `chars`) were added directly to `modules/std/string.pr` rather than creating a separate `string_util.pr` file. Free functions (`join`, `spaces`, `reverse`, `is_blank`, `repeat_join`) live in the `strings` catalog module (`modules/strings/strings.pr`).

- **Files**: `modules/std/string.pr` (methods), `modules/strings/strings.pr` (free functions)
- **Test**: `tests/std/string_test.pr`, `modules/strings/strings_test.pr`

#### 1d. `modules/std/result.pr` — Result Utilities — DEFERRED

Failable types (`T!`) in Promise are a function-level concept — they cannot be used as parameter or variable types. The planned `unwrap_or[T](T! value, T default_val)` and `is_error[T](T! value)` are not expressible. Promise's built-in error handling syntax already covers these use cases concisely:
- `unwrap_or` → `failable_call() ? e { default_val }`
- `is_error` → `bool err = false; failable_call() ? e { err = true; }`

No `modules/std/result.pr` is needed.

---

### Phase 2: Conversion & Formatting

#### 2a. Numeric Formatting & Parsing — DONE

- `to_string()` on all primitives (int, i8-i64, uint, u8-u64, f32, f64, bool, char, string) — uses `"{this}"` string interpolation, zero native codegen needed
- `format(Writer ~w)!` on all primitives — delegates to `w.write_string(this.to_string())` (string uses `w.write_string(this)`)
- `int.parse(Reader ~r) int!` — pure Promise, reads digits with optional leading `-`, stops at first non-digit
- `bool.parse(Reader ~r) bool!` — pure Promise, reads "true"/"false" byte-by-byte
- `uint.parse(Reader ~r) uint!` — pure Promise, reads digits, stops at first non-digit
- `f64.parse(Reader ~r) f64!` — pure Promise, handles sign, integer/fractional parts, scientific notation (e/E)
- Tests: `tests/std/to_string_test.pr` (21 tests), `tests/std/parse_test.pr` (38 tests), `tests/std/format_test.pr` (20 tests)

**String interpolation desugaring to `format()` — DONE.** User-defined types implementing `format(Writer ~w)!` are now supported in `{}` interpolation via Builder. Both direct and vtable (polymorphic) dispatch supported.

**Design change from original plan**: `to_string()` uses string interpolation (`"{this}"`) directly instead of wrapping `format()` through a Builder. This is simpler, has zero native codegen, and works today. `format(Writer ~w)!` is separately implemented for composable output to arbitrary Writers.

- **Files**: `modules/std/int.pr`, `modules/std/uint.pr`, `modules/std/float.pr`, `modules/std/bool.pr`, `modules/std/char.pr`, `modules/std/string.pr`
- **Test**: `tests/std/to_string_test.pr`, `tests/std/parse_test.pr`, `tests/std/format_test.pr`

#### 2b. `modules/std/builder.pr` — Builder — DONE

```promise
type Builder `public {
    u8[] buf;
    new(~this, int capacity = 16) { this.buf = Vector[u8](capacity: capacity); }
    write(~this, u8[] data) int { /* push loop */ }
    write_string(~this, string s) int { /* bytes + push loop */ }
    to_string() string => string.from_bytes(this.buf);
    get len int => this.buf.len;
    clear(~this) { this.buf = Vector[u8](); }
}
```

- **File**: `modules/std/builder.pr` — 100% pure Promise, no native/extern methods
- **Dependencies**: `vector.pr` (backed by `u8[]` internally), `string.from_bytes()` native factory
- **Implementation**: Wraps a `Vector[u8]`. `write()` and `write_string()` push bytes individually. `to_string()` calls `string.from_bytes()` which reads Vector[u8] data+count and calls `promise_string_new`. `write_char` not yet implemented.
- **Test**: `tests/std/builder_test.pr` (9 tests)

#### 2c. `modules/std/fmt.pr` — Runtime Template Formatting — DEFERRED

Runtime template formatting (`fmt1`-`fmt6`) is deferred. String interpolation (`"{x} is {age} years old"`) covers the vast majority of formatting needs at compile time. Runtime template formatting, if needed, belongs in a catalog module (`modules/templates/`) rather than `modules/std/`, since it is not a core primitive.

---

### Phase 3: Math & Time

#### 3a. `modules/std/math.pr` — Extended Math (LLVM Intrinsics) — DONE

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

- **File**: `modules/std/math.pr` (extend)
- **Dependencies**: None (LLVM intrinsics)
- **Native codegen**: Declare LLVM intrinsics (`@llvm.sqrt.f64`, etc.), generate wrapper functions
- **Implementation**: `native` functions backed by LLVM intrinsics. `min`, `max`, `clamp` are generic via Ordered constraint (pure Promise). `abs(f64)` uses `llvm.fabs.f64`; `abs(int)` is pure Promise. `tan`, `log2`, `log10`, `trunc`, `is_nan`, `is_inf`, `is_finite` implemented in pure Promise on top of native primitives.
- **Test**: `tests/std/math_test.pr` (26 tests)

Additionally, the `math` catalog module (`modules/math/math.pr`, 67 lines) provides higher-level pure-Promise helpers: `lerp`, `map_range`, `deg_to_rad`, `rad_to_deg`, `sign`, `sign_f64`, `is_even`, `is_odd`, `gcd`, `lcm`. Tests: `modules/math/math_test.pr` (26 tests).

#### 3b. `modules/std/random.pr` — Pseudorandom Numbers — DONE

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

- **File**: `modules/std/random.pr`
- **Dependencies**: `uint` bitwise operators (exist), `as!` casts (exist)
- **Implementation**: Pure Promise. xoshiro256** state is 4 `uint` fields. Seed expansion via splitmix64. Float conversion: mask top bits, OR into exponent, subtract 1.0.
- **Test**: `tests/std/random_test.pr` (7 tests)

#### 3c. `modules/std/time.pr` — Duration & Instant — DONE

- `Duration` — pure value type (`int nanos `value`). Factory constructors: `from_nanos`, `from_micros`, `from_millis`, `from_secs`, `zero`. Getters: `as_nanos`, `as_micros`, `as_millis`, `as_secs`. Arithmetic: `+`, `-`, `*`. Full comparison operators. `to_string()` with adaptive units (ns/us/ms/s). `format(Writer ~w)!`.
- `Instant` — pure value type. `now()` factory (calls `_nanotime` extern). `elapsed()`, `duration_since()`. Comparison operators.
- `sleep(Duration d)` — free function, calls `_sleep_nanos` extern. WASM: no-op.
- Native codegen: `promise_nanotime` (clock_gettime CLOCK_MONOTONIC), `promise_sleep_nanos` (nanosleep(2)). Bodies in `io.go:definePALBodies`. Test runner uses separate `.promise_nanotime_raw` to avoid ABI conflict.
- **File**: `modules/std/time.pr`
- **Test**: `tests/std/time_test.pr` (24 tests)

---

### Phase 4: System I/O

#### 4a. `modules/std/io.pr` — Extended I/O (Closer Interface, Utilities) — DONE

- `Closer` — structural interface with `close(~this)!` abstract method. Any type with a matching `close` method satisfies it.
- `write_line(Writer ~w, string s)!` — convenience function, writes string + newline.
- `read_all`/`read_string` deferred to Phase 4b (File) when there are concrete Reader sources beyond Scanner.
- **File**: `modules/std/io.pr` (extended)
- **Test**: `tests/std/test_io.pr` (4 tests)

#### 4b. `modules/io/io.pr` — File System Access — DONE

```promise
type File {
    // File handle with RAII cleanup

    int _fd;

    // Factory constructors
    open(string path, bool readonly = false) Self! `factory;  // read-write (or read-only)
    create(string path) Self! `factory;                       // write, create/truncate
    append(string path) Self! `factory;                       // append, create if needed
    ~~open_mode(string path, string mode) Self! `factory;~~   // NOT implementing

    // Byte-level I/O (Reader/Writer interface compliance)
    read(~this, u8[] ~buf) int!;           // reads up to buf.len bytes into buf; returns bytes read (0 = EOF)
    write(~this, u8[] ~buf) int!;          // writes bytes from buf; returns bytes written

    // Convenience methods
    read_all(~this) string!;
    write_string(~this, string s)!;
    read_line(~this) string?!;             // reads one line, absent at EOF

    // Position
    get position int!;
    seek(~this, int offset)!;

    // Resource management
    close(~this)!;
    drop(~this);                           // auto-close on scope exit

    // Global (static) convenience methods
    read_content(string path) string! `global;
    write_content(string path, string content)! `global;
    exists(string path) bool `global;
    size(string path) int! `global;
    remove(string path)! `global;
}

type BufferedReader {
    // Buffered file reader — reduces syscalls by reading in chunks.
    new(~this, File file, int buf_size = 4096);
    read(~this, u8[] ~buf) int!;
    read_line(~this) string?!;
    read_byte(~this) u8?!;
    close(~this)!;
}

type BufferedWriter {
    // Buffered file writer — reduces syscalls by batching writes.
    new(~this, File file, int buf_size = 4096);
    write(~this, u8[] ~buf) int!;
    write_string(~this, string s)!;
    flush(~this)!;
    close(~this)!;
}
```

- **File**: `modules/io/io.pr` (separate `io` module, not part of `std`)
- **Dependencies**: PAL file I/O (3.1), Error type (Phase 0a), `Reader`/`Writer`/`Closer` (4a)
- **Native codegen**: `File.open` → PAL `EmitFileOpen`, `File.read` → PAL `EmitFileRead`, etc.
- **Implementation**: Thin wrapper around PAL calls. `File.read(~this, u8[] ~buf) int!` and `File.write(~this, u8[] ~buf) int!` satisfy the `Reader`/`Writer` structural interfaces. `read_line` is a File instance method (not a free function). `BufferedReader`/`BufferedWriter` are pure Promise wrappers around `File` that reduce syscalls by chunked I/O; both also satisfy `Reader`/`Writer` via their `read`/`write` methods.
- **Test**: `modules/io/io_test.pr` (69 tests)

#### 4c. `modules/path/path.pr` — Path Manipulation — DONE

```promise
// Pure string-based path operations (no filesystem access)

join(string[] parts) string;
file_name(string path) string;
parent(string path) string;
extension(string path) string;
is_absolute(string path) bool;
normalize(string path) string;
```

- **File**: `modules/path/path.pr` (separate `path` module, 192 lines)
- **Dependencies**: `string.pr` methods only
- **Implementation**: Pure Promise string manipulation. Uses `/` as separator (POSIX-first; Windows support deferred).
- **Test**: `modules/path/path_test.pr` (13 tests), `tests/catalog/path_test.pr`

#### 4d. `modules/os/os.pr` — OS Interaction — DONE

```promise
type OsError is error `public { int code; }

type ProcessResult `public {
    int exit_code;
    string standard_output;
    string standard_error;
}

// One-shot execution
get_environment_variable(string name) string?;
get_working_directory() string!;
exit_process(int code);
arguments() string[];
executable_path() string;
execute(string program, ...string arguments) ProcessResult!;
set_environment_variable(string name, string? value);
set_working_directory(string path) !;

// Streaming process execution
type ProcessInput `public { ... }   // satisfies Writer: write, write_string, write_line, close, drop
type ProcessOutput `public { ... }  // satisfies Reader: read, read_all, close, drop
type Process `public {
    spawn(string program, ...string arguments) Self! `factory;
    take_standard_input(~this) ProcessInput!;
    take_standard_output(~this) ProcessOutput!;
    take_standard_error(~this) ProcessOutput!;
    wait(~this) int!;              // closes stdin, returns exit code (cached)
    kill(~this)!;                  // SIGKILL
    get identifier int;            // pid
    drop(~this);                   // close fds + reap zombie
}

// OS info getters
get environment map[string, string];  // all env vars as map
get user_name string;                 // login name (getpwuid)
get user_identifier int;              // uid
get group_identifier int;             // gid
get home_directory string;            // home dir (getpwuid)
get hostname string;                  // machine hostname
get process_identifier int;           // current pid

// Signal handling
enum Signal { Interrupt, Terminate, Hangup }
setup_signal_handling(...Signal signals)!;  // register signals via pipe+handler
receive_signal() Signal!;                   // block until signal arrives
```

- **File**: `modules/os/os.pr` (separate `os` module, not part of `std`)
- **Dependencies**: PAL OS (getenv, getcwd, exit, setenv, unsetenv, chdir, spawn, spawn_streaming, kill, get_environ, get_user_info, get_hostname, signal_init, signal_register), argc/argv globals from main prologue
- **Native codegen**: Extern bridge pattern in `os_bridges.go` — Promise declares `_os_func() T \`extern("promise_os_func");`, codegen provides LLVM IR body bridging Promise types ↔ PAL. `execute` uses three-extern + TLS caching pattern. Streaming process uses six externs. OS info uses six externs. Signal handling uses pipe-based async-signal-safe delivery: `pal_signal_init` creates pipe + defines handler, `pal_signal_register` calls `signal(2)`. The `environment` getter builds `map[string, string]` in pure Promise from the string[] of "KEY=VALUE" entries.
- **Test**: `modules/os/os_test.pr` (103 tests, excluded on WASM)

#### 4e. Standard Input — DONE (merged into `modules/io/io.pr`)

```promise
// Read a line from stdin (blocking) — free function in io module
read_line() string?!;

// Read all of stdin
read_stdin() string!;

// Per-file read_line is a File instance method:
//   file.read_line() string?!;
```

- **File**: `modules/io/io.pr` (stdin functions are free functions in the `io` module)
- **Dependencies**: PAL `EmitFileRead` (fd 0), PAL `EmitFileReadLine`
- **Implementation**: `read_line()` free function reads from fd 0 (stdin). `File.read_line(~this)` is an instance method for reading lines from any open file. Both return `string?!` (absent at EOF, raises `IoError` on error).
- **Test**: `modules/io/io_test.pr`

---

### Phase 5: Future Modules (Design Only)

These modules are lower priority. Full API design to be done when dependencies are ready.

#### 5a. `modules/json/json.pr` — JSON Parsing/Serialization

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

#### 5b. `modules/regex/regex.pr` — Regular Expressions

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

#### 5c. `modules/net/net.pr` — TCP Networking

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

#### 5d. `modules/http/http.pr` — HTTP Client

Currently a 4-line placeholder. Planned API:

```promise
type HttpResponse {
    int status;
    map[string, string] headers;
    string body;
}

http_get(string url) HttpResponse!;
http_post(string url, string body, map[string, string] headers) HttpResponse!;
```

- **Dependencies**: `modules/net/net.pr`, `modules/json/json.pr`

#### 5e. `modules/crypto/crypto.pr` — Cryptographic Hashing

```promise
sha256(u8[] data) u8[];
sha256_hex(u8[] data) string;
```

- **Implementation**: Pure Promise (SHA-256 is ~100 lines of bit manipulation)

#### 5f. `modules/std/embed.pr` — Resource Embedding Types

Types supporting the `` `embed(path) `` compile-time annotation (see [language-design.md](language-design.md#86-resource-embedding-embed)). Single-file embeds use `string` or `u8[]` directly; directory tree embeds use `EmbeddedFiles`.

```promise
type EmbeddedFiles `doc("Virtual read-only filesystem for compile-time embedded directory trees.") {
    EmbeddedFile[] _entries `final;
    u8[] _data `final;

    get files EmbeddedFile[] `doc("List all embedded entries.") => this._entries;

    read(&this, string path) string!
        `doc("Read an embedded file as a UTF-8 string. Raises if path not found.");

    read_bytes(&this, string path) u8[]!
        `doc("Read an embedded file as raw bytes. Raises if path not found.");

    contains(&this, string path) bool
        `doc("Check whether a path exists in the embedded tree.");
}

type EmbeddedFile `value `doc("Metadata for a single entry in an EmbeddedFiles tree.") {
    string name `value `doc("File or directory name (leaf only, no path separators).");
    string path `value `doc("Full relative path from the embed root.");
    int size `value `doc("Size in bytes (0 for directories).");
    bool is_directory `value `doc("True if this entry is a directory.");
}
```

- **Dependencies**: None (pure std types, populated by the compiler at codegen time)
- **Implementation**: `EmbeddedFiles` is constructed by the compiler — codegen reads files at compile time and emits global constant data. `read`/`read_bytes` index into the embedded data blob using offset/size metadata. No PAL needed.

---

## 5. Implementation Details

### 5.1 Extern Function Pattern

For each PAL-backed function, the implementation follows this pattern:

**1. Declare in `modules/std/X.pr`:**
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

**2. Mark as native in `modules/std/math.pr`:**
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
bin/test.sh                                  # full e2e suite
```

### 5.4 Embedding Updated Stdlib

When adding new types or functions to `modules/std/*.pr`, run `./build` from the repo root — it automatically embeds the updated stdlib before compiling.

The `stdAll` mini-stdlib used in Go unit tests (`codegen_test.go`, `sema_test.go`, `ownership_test.go`) is **auto-populated from the embedded std** via `go:embed` at test init time — no manual update needed. `TestResourcesFresh` (in `cmd/promise/resources_test.go`) detects stale embedded copies when `go test ./...` is run.

---

## 6. Testing Strategy

### Test Categories

~2285 test functions across ~207 `.pr` files:

| Category | Location | What it verifies |
|----------|----------|-----------------|
| Go unit tests | `internal/codegen/*_test.go` | IR shape for native functions |
| Go unit tests | `internal/sema/*_test.go` | Type checking, error messages |
| Promise e2e | `tests/e2e/*.pr` | Language features (inheritance, generics, errors, lambdas, etc.) |
| Promise std | `tests/std/*_test.pr` | Standard library runtime correctness |
| Promise concurrency | `tests/concurrency/*.pr` | M:N scheduler, channels, select, tasks, stress |
| Promise modules | `tests/modules/*.pr` | Module system, visibility, transitive deps |
| Catalog tests | `modules/*/*_test.pr` | Catalog module internal tests (io, path, strings, math) |

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
bin/test.sh                            # all tests pass
```

After:
```bash
bin/test.sh                            # rebuild + all tests pass (including new ones)
```

---

## Appendix: Complete Module Inventory

| Phase | File | Type | New PAL | Lines | Status |
|-------|------|------|---------|-------|--------|
| 0a | `modules/std/error.pr` | Promise | No | 3 | **DONE** |
| 0b | `modules/std/iter.pr` | Promise | No | 254 | **DONE** |
| 0c | `modules/std/int.pr` etc. | Native | No | ~930 | **DONE** |
| 0d | `modules/std/format.pr` | Promise | No | 129 | **DONE** |
| 0e | `modules/std/parse.pr` | Promise | No | 43 | **DONE** |
| 1a | `modules/std/set.pr` | Promise | No | 73 | **DONE** |
| 1b | `modules/std/sort.pr` | Promise | No | 92 | **DONE** |
| 1c | `modules/std/string.pr` + `modules/strings/` | Promise | No | 191+65 | **DONE** |
| 1d | `modules/std/result.pr` | ~~Promise~~ | No | — | DEFERRED |
| 2a | (merged into 0c) | — | — | — | **DONE** |
| 2b | `modules/std/builder.pr` | Promise | No | 38 | **DONE** |
| 2c | `modules/std/fmt.pr` | ~~Promise~~ | No | — | DEFERRED |
| 3a | `modules/std/math.pr` + `modules/math/` | Native + Promise | No | 111+67 | **DONE** |
| 3b | `modules/std/random.pr` | Promise | No | 107 | **DONE** |
| 3c | `modules/std/time.pr` | Promise + Native | 3 | 96 | **DONE** |
| 4a | `modules/std/io.pr` | Promise | No | 59 | **DONE** |
| 4b | `modules/io/io.pr` | Promise + Native | 12 | 501 | **DONE** |
| 4c | `modules/path/path.pr` | Promise | No | 192 | **DONE** |
| 4d | `modules/os/os.pr` | Promise + Native | 13 | 449 | **DONE** |
| 4e | (merged into 4b) | — | — | — | **DONE** |
| — | `modules/std/platform.pr` | Promise | No | 33 | **DONE** |
| 5a | `modules/json/json.pr` | Promise | No | ~300 | Future |
| 5b | `modules/regex/regex.pr` | Promise | No | ~400 | Future |
| 5c | `modules/net/net.pr` | Promise + Native | 6+ | ~150 | Future |
| 5d | `modules/http/http.pr` | Promise | No | ~200 | Future |
| 5e | `modules/crypto/crypto.pr` | Promise | No | ~150 | Future |
| 5f | `modules/std/embed.pr` | Promise | No | ~50 | Future (T0012) |
| | **Phases 0-4 (actual)** | | **12** | **~3,055** | **19/20 done** |
| | **Total (all phases)** | | **18+** | **~4,305** | |
