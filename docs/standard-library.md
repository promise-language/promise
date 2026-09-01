# Standard Library

> **Tag:** `standard-library` — remaining work to complete this document: `mcp__tracker__list --tag standard-library`

Promise's standard library design: module inventory, implementation phases, PAL extensions needed, and testing strategy.

**Design principles** (from CLAUDE.md):
- Implement in Promise (`modules/std/*.pr`) wherever possible — only use `native` for unavoidable low-level operations
- Small, orthogonal, predictable — an AI agent should be able to generate correct code without ecosystem knowledge
- Every effect visible at the call site — no hidden initialization, no implicit state

---

## Table of Contents

1. [Module Inventory](#1-module-inventory)
2. [Language Features the Stdlib Depends On](#2-language-features-the-stdlib-depends-on)
3. [PAL Extensions](#3-pal-extensions)
4. [Module Design by Phase](#4-module-design-by-phase)
5. [Implementation Details](#5-implementation-details)
6. [Testing Strategy](#6-testing-strategy)

---

## 1. Module Inventory

The stdlib provides:

| Category | Files | What it covers |
|----------|-------|---------------|
| Primitives | `int.pr`, `uint.pr`, `float.pr`, `bool.pr`, `char.pr` | Arithmetic, comparison, bitwise, hash, `to_string()`, `format()`, `parse()`, `encode(Encoder)`, `decode(Decoder)` for all numeric/bool/char/string types |
| Strings | `string.pr` | Concatenation, comparison, `contains`, `starts_with`, `ends_with`, `index_of`, `trim`, `split`, `[]`, `[:]`, `bytes()`, `byte_at()`, `from_bytes()`, `to_string()`, `to_upper`, `to_lower`, `repeat`, `replace`, `count`, `chars` |
| Containers | `vector.pr`, `map.pr`, `set.pr` | `Vector[T]` / `T[]` (push/pop/remove/contains/slice/`filled`/`clone`/`format`/`to_string`), `Map[K,V]` / `map[K,V]` (open-addressing, rehash, `clone`/`format`/`to_string`), `Set[T]` (`clone`/`format`/`to_string`) |
| Format/Parse | `format.pr`, `builder.pr`, `parse.pr` | `Format` structural interface, `Builder` (string building, satisfies `Writer`), `Parse` structural interface, `Scanner` (string parsing, satisfies `Reader`), `scan[T]()` |
| I/O (std) | `io.pr` | `Reader` (read, read_byte) / `Writer` (write, write_string, write_line) / `Closer` structural interfaces, `print(Format)`, `print_line(Format)`, `stdin` (Reader) / `stdout` / `stderr` (Writer) standard-stream getters |
| I/O (module) | `modules/io/io.pr` | `File` (open/create/append, read/write bytes, read_line, write_line, read_all, seek, `sync`, `rename`, `replace_content`/`replace_bytes`, `lock`/`lock_shared`/`try_lock`/`try_lock_for`/`unlock`), `BufferedReader`, `BufferedWriter` (write_line), `Dir` (make/make_all/list/remove/exists/`sync`), `IoError`, `read_line()`, `read_stdin()` — durability and locking contract in [io.md](io.md) |
| Path (module) | `modules/path/path.pr` | `join`, `parent`, `file_name`, `extension`, `stem`, `split`, `is_absolute`, `is_relative`, `normalize` |
| Math | `math.pr`, `random.pr` | `min`, `max`, `abs`, `clamp`, `sqrt`, `sin`, `cos`, `tan`, `pow`, `exp`, `log`, `floor`, `ceil`, `round`, `Random` PRNG (xoshiro256**) |
| Sorting | `sort.pr` | `sort(T[] move)` for `Ordered` types (consumes and returns; iterative quicksort) |
| Interfaces | `equal.pr`, `ordered.pr`, `hashable.pr`, `clone.pr` | `Equal`, `Ordered`, `Hashable`, `Cloneable` structural types |
| Iterators | `iter.pr` | `Iterator[T]` structural interface with 20 default combinator methods, `Stream[T]` structural interface, `_FnIter[T]` closure-based iterator, `Generator[T]` coroutine-based iterator, duck-typed for-in |
| Concurrency | `channel.pr`, `task.pr`, `runtime.pr`, `ref.pr`, `mutex.pr` | `Channel[T]` / `channel[T]` send/close, `Task[T]` / `task[T]` handle, `Ref[T]` / `Weak[T]` reference-counted shared ownership, `Mutex[T]` / `MutexGuard[T]`, scheduler stats |
| Time | `time.pr` | `Duration` (value type, nanosecond precision), `Instant` (monotonic clock), `sleep()` |
| Platform | `platform.pr` | `Platform` type with `line_separator`, `path_separator`, `is_path_separator()` — compile-time `` `target `` filtering for Windows/POSIX |
| Serialization | `encode.pr` | `Encoder`/`Decoder` (non-structural), `Encodable`/`Decodable` (structural), `DecodeError` |
| Geometry | `geometry.pr` | `Point[T]`, `Size[T]`, `Rect[T]` — generic pure value types for 2D coordinates, sizes, rectangles |
| Unicode | `unicode.pr`, `unicode_ccc.pr`, `unicode_comp.pr`, `unicode_decomp.pr`, `unicode_qc.pr` | `normalize_nfc`, `normalize_nfd`, `is_nfc`, `is_nfd`; generated Unicode 16.0.0 tables (combining class, composition pairs, canonical decomposition, quick-check ranges) |
| Numerics | `ryu.pr`, `wide_int.pr` | Ryū shortest round-trip float→string formatting; `i128`/`u128`, `i256`/`u256`, `i512`/`u512` wide integers |
| Embedding | `embed.pr` | `` `embed(path) `` compile-time file embedding — `EmbeddedFile`, `EmbeddedFiles` |
| Other | `range.pr`, `hash.pr`, `assert.pr`, `error.pr` | `Range` / `..`/`..=`, FNV-1a hash, `assert(bool, string)`, `error` base type |

**Catalog modules** (separate `promise.toml`, imported via `use <name>;`):

| Module | File | Lines | What it covers |
|--------|------|-------|----------------|
| `io` | `modules/io/io.pr` | 809 | `File` (open/create/append, read/write bytes, read_line, write_line, read_all, seek, `sync`, `rename`, `replace_content`/`replace_bytes`, `lock`/`lock_shared`/`try_lock`/`try_lock_for`/`unlock`), `BufferedReader`, `BufferedWriter`, `Dir` (make/make_all/list/remove/exists/`sync`), `IoError`, `read_line()`, `read_stdin()`. Atomic replace, `sync`, and advisory locking are specified by [io.md](io.md). 128 tests. |
| `path` | `modules/path/path.pr` | 192 | `join`, `file_name`, `parent`, `extension`, `stem`, `split`, `is_absolute`, `is_relative`, `normalize`. 9 tests. |
| `strings` | `modules/strings/strings.pr` | 65 | `join`, `spaces`, `reverse`, `is_blank`, `repeat_join`. 15 tests. |
| `math` | `modules/math/math.pr` | 67 | `lerp`, `map_range`, `deg_to_rad`, `rad_to_deg`, `sign`, `sign_f64`, `is_even`, `is_odd`, `gcd`, `lcm`. 8 tests. |
| `json` | `modules/json/json.pr` | 1003 | `JsonEncoder` (is Encoder), `JsonDecoder` (is Decoder), generic `encode_string[T]`/`decode_string[T]`/`encode_string_pretty[T]`, `JsonValue` enum with methods (`is_null`..`is_object`, `as_bool`..`as_object`, `get(key)`, `at(index)`, `encode`, `format`, `format_pretty`), `parse_value`. 164 tests. |
| `os` | `modules/os/os.pr` | 511 | get_env_var, working_dir, exit_process, args, executable_path, execute, set_env_var, set_working_dir, Process/ProcessInput/ProcessOutput (streaming), env (map), user_name, user_id, group_id, home_dir, hostname, process_id, Signal enum, setup_signal_handling, receive_signal. 147 tests. |
| `net` | `modules/net/net.pr` | 398 | `TcpListener` (`bind`, `accept`, `close`, `local_port`), `TcpStream` (`connect`, `read`, `write`, `close`, `shutdown`), `resolve!`, `NetError`, `ResolveError`/`ResolveErrorKind`. Reactor-based non-blocking I/O: sockets are non-blocking and goroutines park on the netpoll reactor rather than blocking an M. `TcpStream.connect` takes a host name or an IPv4/IPv6 literal (T1518); resolution uses the platform resolver behind the scheduler's syscall handoff, not the reactor; an empty host is rejected below the bridge so it fails identically on every target with a resolver (T1726). 41 tests. |
| `time` | `modules/time/time.pr` | 392 | wall-clock `DateTime` (`now`, Unix-epoch conversions, component accessors, `Duration` arithmetic, comparison, UTC offsets, ISO-8601 `to_string`/`parse`/`format_rfc3339`), `Date` (`today`, `add_days`, `at`), `Time` (`midnight`/`noon`, wrapping arithmetic). Native `promise_wallclock` (CLOCK_REALTIME / GetSystemTimePreciseAsFileTime); calendar math in Promise. 53 tests. |
| `http` | `modules/http/http.pr` | 1644 | client + server, http and https — `Request`/`Response`, `Method`, headers, `http_get`/`http_post`/`http_post_json`; `Client` (redirect following with 301/302/303/307/308 method-rewrite policy, keep-alive connection pooling with stale-connection retry, automatic gzip response decoding via the `gzip` module (sends `Accept-Encoding: gzip`, honors `Content-Encoding: gzip`), cross-origin credential stripping, `set_tls_config` for custom CAs / mutual TLS); `Server.bind` (HTTP) and `Server.bind_tls` (HTTPS) with `Handler`, `ServerRequest`, `ServerResponse`, per-connection goroutines with keep-alive and bounded concurrency (`max_connections`, `max_keep_alive_requests`), and draining graceful shutdown. https support (T0079) is a private `_Transport` interface with a plaintext and a TLS implementation, so client framing and the server's keep-alive loop have exactly one implementation; each connection's TLS handshake runs on that connection's own goroutine. Importing `http` links a TLS backend (the vendored static OpenSSL on Linux) even for a program that only speaks http://. 147 tests. |
| `tls` | `modules/tls/tls.pr` | 465 | client + server — `TlsConfig` (`create`/`insecure`, `add_root_certificate`, `set_client_certificate`, `set_min_version`), `TlsServerConfig` (`create` from a PEM certificate chain + key, shared across connections), `TlsVersion`, `TlsStream` (satisfies `Reader`/`Writer`: `read`/`write`/`read_all`/`read_line`/`write_string`/`close`/`close_gracefully`, `version`/`cipher_suite`, plus the `accept` factory that upgrades an already-accepted `net.TcpStream`), `TlsListener` (bind with certificate chain + key, `accept`), `TlsError`/`TlsErrorKind`. Memory-BIO design — all socket I/O and reactor parking stay in Promise over `net.TcpStream`. `TlsListener.accept` handshakes inline; a server that must not let one slow peer stall its accept loop binds a plain `net.TcpListener` and calls `TlsStream.accept` on each connection's own goroutine (what `http.Server.bind_tls` does). Backends: Linux links the vendored musl-static OpenSSL (T1596), macOS uses Secure Transport (T1599), Windows uses SChannel (T1598); WASM raises `unsupported`. 22 tests. |
| `encoding` | `modules/encoding/hex.pr`, `error.pr` | 53 | hex — `hex_encode(u8[]) string`, `hex_decode!(string) u8[]` (upper/lower case, raises on odd length or non-hex digit), `EncodingError` with `at_index`. base64/base64url tracked as T1569. 17 tests. |
| `gzip` | `modules/gzip/` | 956 | RFC 1951 (DEFLATE) and RFC 1952 (gzip) in pure Promise: `gzip_encode`, `gunzip!`, `gunzip_from!(Reader)`, `deflate`, `inflate!`, `crc32`, `GzipWriter` (satisfies `Writer`), `GunzipReader` (satisfies `Reader`), `DecompressError`. 90 tests. |
| `crypto` | `modules/crypto/` | 258 | SHA-256 — `sha256.pr`: `Sha256` streaming context (`update`/`finalize`), `Digest256` (`to_string` hex, `to_bytes`, `^`, `==`, `hash`), one-shot `sha256(u8[]) Digest256`; `constant_time.pr`: `constant_time_equal(u8[], u8[]) bool`; `random.pr`: `random_bytes!(int) u8[]` (CSPRNG via OS syscall — T1571), `CryptoError`. HMAC-SHA-256 (T1567) and PBKDF2 (T1568) remain to be built. 32 tests. |
### Protocol Conformance Is Declared, Not Inferred

The structural interfaces the platform publishes — `Format`, `Parse`, `Reader`, `Writer`, `Closer`, `Encodable`, `Decodable`, `Cloneable`, `Hashable`, `Equal`, `Ordered`, `Iterator` — carry `` `structural(protocol: true) ``, which reserves their requirement names (see §5.4 of `docs/language-design.md`). Two rules follow for every type in `modules/std/` and the catalog:

1. **Every implementation declares `is` explicitly.** `type Duration is Format, Parse { ... }`, never a silent structural match. The `is` clause costs nothing at runtime — for a value type it does not even change the layout — and it converts a signature that drifts from a type that quietly stops satisfying anything into an error naming the exact method.
2. **A method that borrows a reserved name without implementing the protocol says so.** `` `structural(protocol: false) `` on that method, with a comment explaining why the name means something else here. `Channel.close()` is the worked example: channels are refcounted, so closing one is not a consuming operation and `Channel` is deliberately not a `Closer`.

Relying on structural satisfaction is still correct for user code composing with an interface it did not know about. It is not correct for a published platform type, where the conformance is part of the contract and should be checked at the declaration.

### Naming Conventions

Promise uses a two-tier naming scheme. Casing tells the reader whether a type is woven into the language itself or lives in library space.

**Lowercase — language-level types.** These have dedicated syntax, LLVM scalar representation, or are fundamental language concepts:

| Type | Why lowercase |
|------|--------------|
| `int`, `i8`..`i64`, `uint`, `u8`..`u64`, `f32`, `f64` | LLVM scalar integers/floats |
| `bool`, `char` | LLVM scalar `i1` / `i32` |
| `string` | Has literal syntax `"..."`, interpolation `{}` |
| `error` | Base type for `^`/`?`/`!`/`raise` error system |

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

## 2. Language Features the Stdlib Depends On

### 2.1 Error Type System

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
- Three operators: `^` (propagate), `!` (unwrap/panic), `? e { }` / `? e is T { }` (typed handler)
- Generic error types: `type GenericError[T] is error { T val; }`
- Result capture: `(val, err) := failable_call()`
- Error types cannot have `drop` methods (enforced in decl.go)
- 22+ Go unit tests in `sema_test.go`, 5 e2e test files covering construction, inheritance chains, typed handlers, nested handlers, and generic errors

### 2.2 Stream/Iterator Combinators

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

### 2.3 Numeric Type Conversions

| Aspect | Detail |
|--------|--------|
| `as`/`as!` scalar casts | all scalar types (int, i8-i64, uint, u8-u64, f32, f64, char, bool) castable via `as`/`as!` |
| char ↔ integer | `'A' as int` → 65, `65 as char` → 'A' (zext/trunc) |
| bool ↔ integer/float | `true as int` → 1, `42 as bool` → true (icmp ne 0), `0 as bool` → false |
| float → bool | `0.0 as bool` → false, any non-zero → true including NaN (fcmp une 0.0) |
| Int↔String | `to_string()`, `format(Writer ~w)!`, `int.parse(Reader ~r) int!` |
| Float↔String | `to_string()`, `format(Writer ~w)!`, `f64.parse(Reader ~r) f64!` |

**Implemented approach**: `as`/`as!` work identically for scalar types (both return target type directly, no optional). For polymorphic casts (inheritance), `as` returns optional, `as!` panics on mismatch. All primitives have `to_string()` via `"{this}"` and `format!(Writer ~w) `. `int.parse`, `bool.parse`, `uint.parse`, `f64.parse` are pure Promise. No snprintf/strtol needed.

**Key codegen detail**: `int → bool` uses `icmp ne val, 0` (not `trunc`, which would give wrong result for even numbers like 2). `float → bool` uses `fcmp one val, 0.0`. Tests: 32 e2e tests in `tests/e2e/scalar_casts_test.pr`, 9 sema tests, 6 codegen tests.

### 2.4 Format & Writer for String Interpolation

| Aspect | Detail |
|--------|--------|
| String interpolation | Works for all primitives (`string`, `int`, `f64`, `bool`, `char`, etc.) and **user-defined types** implementing `format(Writer ~w)!` |
| `Writer` interface | **Defined** in `modules/std/format.pr` |
| `Format` interface | **Defined** in `modules/std/format.pr` — `format(Writer ~w)!` (failable) |
| `to_string()` | **Available** on all primitives via `"{this}"` |
| `format()` | **Available** on all primitives — delegates to `w.write_string(to_string())` |
| `Builder` | **Implemented** in `modules/std/builder.pr` (pure Promise) |
| User type interpolation | **Implemented** — `"{x}"` desugars to Builder + `x.format(builder)!` + `builder.to_string()` for types implementing `Format` |

**Types** (in `modules/std/format.pr`):

```promise
type Writer `structural {
    // Required — any type with this method satisfies Writer
    write!(~this, u8[] &buf) int `abstract;

    // Default method — available on any Writer, no need to implement
    write_string!(~this, string s) int {
        return this.write(s.as_bytes());
    }
}

type Format `structural {
    format!(Writer ~w) `abstract;
}
```

Writer is byte-oriented (like Go's `io.Writer`), making it usable for files, network, and any I/O — not just formatting. The `write_string` default method provides string convenience on top of the byte interface. Types only need to implement `write(u8[])` to satisfy Writer.

**Language feature: default methods on structural interfaces**. A structural interface can have both `abstract` methods (required for satisfaction) and non-abstract methods (default implementations, available on any satisfying type). This is similar to Rust traits or Java default interface methods.

**How it works**: `Format` types write their string representation into a `Writer` via `write_string()`. The caller controls the buffer, and multiple format calls compose without intermediate allocations. `format()` is failable because underlying `Writer.write()` may fail (e.g., I/O error).

`to_string()` is synthesized from `format()`: create a Builder (which satisfies `Writer`), call `format!(builder) `, return `builder.to_string()`. No need for types to implement `to_string()` separately. Builder's `write()` never fails, so the `!` is safe.

**String interpolation** `"value: {x}"` desugars to:
```promise
// compiler-generated:
mut _sb := Builder();
_sb.write_string("value: ");
x.format!(~_sb); // if x implements Format
_sb.to_string()
```

All primitive types (`int`, `i8`-`i64`, `uint`, `u8`-`u64`, `f32`, `f64`, `bool`, `char`, `string`) implement `format!(Writer ~w) `.

**Stream `join`**: With `Format`, stream combinators can offer `join(string separator)` as a terminal on `Stream[T: Format]` — each element formats into the shared builder with separators between them.

**Example user type**:
```promise
type Point {
    int x;
    int y;

    format!(Writer ~w) {
        w.write_string("(");
        this.x.format(w);
        w.write_string(", ");
        this.y.format(w);
        w.write_string(")");
    }
}

// Now works in string interpolation:
p := Point(x: 3, y: 4);
print_line("point: {p}");   // point: (3, 4)
```

### 2.5 Parse & Reader — Structural Interface on Factory Methods

| Aspect | Details |
|--------|---------|
| Parsing | `int.parse`, `bool.parse`, `uint.parse`, `f64.parse` — all pure Promise |
| Byte input | `Reader` structural interface in `modules/std/parse.pr` with `read_byte` default method |
| `Scanner` | Wraps string, satisfies Reader, tracks position. In `modules/std/parse.pr` |
| `scan[T]()` | Generic convenience: `scan[int]("42")!` — wraps string in Scanner, calls `T.parse` |

**The problem**: `Format` works as a structural interface because it's an instance method — you have a value and call `value.format(writer)`. Parsing is the inverse: you need to **create** a value by reading from a source. There's no instance to call a method on. The operation lives on the type, not on an instance. Additionally, a parser may not consume all the input — it should read what it needs and leave the rest.

**Proposed types** (in `modules/std/parse.pr`):

```promise
type Reader `structural {
    // Required — any type with this method satisfies Reader
    read!(~this, u8[] ~buf) int `abstract;

    // Default method — reads n bytes and returns as string
    read_string!(~this, int n) string {
        mut buf := u8[](capacity: n);
        bytes_read := this.read!(buf);
        return string.from_bytes(buf);
    }

    // Default method — peek without consuming (requires buffered reader)
    // Concrete types may override for efficiency
    peek!(~this, int n) u8[] ;
}

type Parse `structural {
    parse!(Reader ~r) Self `factory `abstract;
}
```

Reader is byte-oriented (like Go's `io.Reader`), making it usable for files, network, stdin, and parsing — one interface for everything. The `read_string` default method provides string convenience for parsers. Types only need to implement `read(u8[])` to satisfy Reader.

A type satisfies `Parse` if it has a factory method `parse!(Reader ~r) Self`. The parser reads what it needs from the Reader and stops — remaining content stays available for subsequent parses.

**Scanner** — a buffered Reader for parsing strings:

```promise
type Scanner {
    // Wraps a string source, tracks position. Satisfies Reader.

    new(~this, string source);

    // Reader interface (byte-oriented)
    read!(~this, u8[] ~buf) int ;
    peek!(~this, int n) u8[] ;

    // Parse the next value of type T (reads from current position)
    next![T: Parse]() T ;

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
scan![T: Parse](string s) T {
    mut r := Scanner(s: s);
    return T.parse(r);
}

// Usage:
x := scan![int]("42");
y := scan![f64]("3.14");
ok := scan![bool]("true");
```

**Primitive implementations**:

```promise
type int `native {
    // ... existing ...
    parse!(Reader ~r) Self `factory `native; // reads digits, stops at non-digit
}

type f64 `native {
    // ... existing ...
    parse!(Reader ~r) Self `factory `native; // reads float literal, stops at end
}

type bool `native {
    // ... existing ...
    parse!(Reader ~r) Self `factory `native; // reads "true"/"false"
}
```

**User types** can implement Parse too:

```promise
type Point {
    int x;
    int y;

    parse!(Reader ~r) Self `factory {
        // parse "3,4" format
        px := int.parse!(r);
        comma := r.read_string!(1);
        if comma != "," { raise error(message: "expected comma"); }
        py := int.parse!(r);
        return Point(x: px, y: py);
    }
}

p := scan![Point]("3,4");
```

Note how Point's parser composes with int's parser — each reads what it needs from the same Reader, advancing the position incrementally. The `read_string` default method makes text parsing convenient while the underlying interface is byte-oriented.

**Multiple values from one source**:

```promise
mut s := Scanner(s: "42 3.14 true");
x := s.next![int](); // reads "42", stops at space
s.skip_whitespace();
y := s.next![f64](); // reads "3.14", stops at space
s.skip_whitespace();
ok := s.next![bool](); // reads "true"
```

**Parsing from a file** — same Reader interface:

```promise
mut f := File.open!("data.txt");
mut s := Scanner(reader: ~f);    // Scanner wraps any Reader
x := s.next![int]();
```

**Language features required**:
1. **Structural factory matching**: extend structural interface matching to factory methods. When `T: Parse` is used, the compiler verifies the concrete type has `parse!(Reader ~r) Self `factory` and dispatches to it. Resolved at monomorphization time.
2. **Default methods on structural interfaces**: non-abstract methods with implementations (like `read_string` on Reader, `write_string` on Writer). Types get these for free when they implement the abstract methods.

**Symmetry with Format**:

| | Format (output) | Parse (input) |
|---|---|---|
| Structural interface | `Format` | `Parse` |
| I/O interface | `Writer` (bytes) | `Reader` (bytes) |
| Method | `format!(Writer ~w)` | `parse!(Reader ~r) Self` |
| Method kind | Instance | Factory |
| Direction | Value → Writer | Reader → Value |
| Concrete wrapper | Builder (satisfies Writer) | Scanner (satisfies Reader) |
| String helper | `w.write_string(s)` | `r.read_string(n)` |
| Generic usage | `x.format(writer)` | `T.parse(reader)` |
| Convenience | string interpolation | `scan[T](string)` |
| Works with files | File satisfies Writer | File satisfies Reader |

---

## 3. PAL Extensions

The PAL (Platform Abstraction Layer) isolates all OS interaction. Currently 47 methods covering memory (5), threads/sync (11), CPU count (1), file I/O (12), OS/environment (5), process execution (5: spawn, read_pipe, wait_pid, spawn_streaming, kill), OS info (3: get_environ, get_user_info, get_hostname), signal handling (2: signal_init, signal_register), and directory listing (3). New methods needed:

### 3.1 File I/O

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

### 3.2 OS / Environment

5 PAL methods:

```go
EmitGetEnv(module *ir.Module) *ir.Func      // i8* name → i8* (value or null)
EmitGetCwd(module *ir.Module) *ir.Func      // i8* buf, i64 len → i8* (path or null)
EmitSetEnv(module *ir.Module) *ir.Func      // i8* name, i8* value → i32 (0 or -1)
EmitUnsetEnv(module *ir.Module) *ir.Func    // i8* name → i32 (0 or -1)
EmitChdir(module *ir.Module) *ir.Func       // i8* path → i32 (0 or -1)
```

Command-line arguments: captured in `main()` prologue from `argc`/`argv` and stored in a global `string[]`. Exposed via `os.args` (module-level getter).

### 3.3 Time

```go
EmitNanotime(module *ir.Module) *ir.Func    // void → i64 (monotonic nanoseconds)
EmitWallClock(module *ir.Module) *ir.Func   // void → i64 (unix epoch nanoseconds)
EmitSleep(module *ir.Module) *ir.Func       // i64 nanoseconds → void
```

Note: `promise_nanotime` already exists as a hardcoded function in `io.go:defineNanotimeFunc()` using `clock_gettime(CLOCK_MONOTONIC)`. This should be migrated to a proper PAL method for portability. `EmitWallClock` uses `CLOCK_REALTIME`. `EmitSleep` uses `nanosleep(2)`.

### 3.4 Process Execution

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
// Windows: CreateProcessA + CreatePipe (spawn), ReadFile (read_pipe), WaitForSingleObject + GetExitCodeProcess (wait_pid), TerminateProcess / GenerateConsoleCtrlEvent (kill) (T0053)
// WASM: stubs returning -1
```

`execute()` in `modules/os/os.pr` reads stdout and stderr concurrently using `go _os_read_pipe(stderr_fd)` while the main goroutine reads stdout. This prevents deadlock when a child writes >64KB to stderr.

`Process.spawn()` creates stdin+stdout+stderr pipes. Pipe handles are obtained via `take_stdin()` (returns `ProcessInput`, satisfies `Writer`), `take_stdout()`/`take_stderr()` (returns `ProcessOutput`, satisfies `Reader`). The streaming pipe read/write/close bridges reuse existing `pal_file_read`/`pal_file_write`/`pal_file_close` PAL functions (pipes are just fds).

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
| File I/O | 12 | `open`, `read`, `write`, `close`, `seek`, `stat_size`, `remove`, `exists`, `mkdir`, `dir_remove`, `dir_exists`, `errno` |
| OS / Env | 5 | `getenv`, `getcwd`, `setenv`, `unsetenv`, `chdir` |
| Process | 9 | `spawn` (`fork`+`execvp`+`pipe`), `read_pipe` (read+close), `wait_pid` (`waitpid`), `spawn_streaming` (stdin+stdout+stderr pipes), `kill` (`kill(2)`), `process_alive` (`kill(0)`), `process_start_time` (`proc_pidinfo`/`/proc/stat`), `kill_group` (`kill(-pgid)`), `spawn_job_handle` |
| OS Info | 3 | `get_environ` (environ global), `get_user_info` (`getpwuid`+`getuid`), `get_hostname` (`gethostname`) |
| Dir Listing | 3 | `dir_open` (`opendir`), `dir_next_name` (`readdir`), `dir_close` (`closedir`) |
| Signal | 2 | `signal_init` (pipe + handler), `signal_register` (`signal(2)`) |
| Time | 3 | `clock_gettime` (×2), `nanosleep` |
| Math | 0 | LLVM intrinsics |
| String | 1 | `memcpy` |
| **Total** | **32** | |

---

## 4. Module Design by Phase

### Phase 0: Language Prerequisites

Complete the features from Section 2 before building stdlib modules.

**0a. Error type system**
- File: `modules/std/error.pr` — defines `error` with `string message` field
- Sema: `checkRaiseStmt` validates inheritance, `checkErrorHandlerExpr` supports typed handlers with exhaustiveness
- Tests: 22+ sema tests, 5 e2e test files

**0b. Stream combinators**
- File: `modules/std/iter.pr` — `Iterator[T]` structural interface with `next() T?` + 20 default combinator methods, `_FnIter[T]` closure-based iterator, `Stream[T]` structural interface, `Generator[T]` coroutine-based iterator
- Duck-typed for-in: `ForInKind` enum (ForInNext/ForInIter) in sema, `genForInCustomIter` in codegen
- `Vector[T].iter()` returns `Iterator[T]`
- All combinators: filter, take, skip, take_while, skip_while, chain, map[R], zip[U], enumerate, flat_map[R] (lazy), collect, count, fold[R], reduce, any, every, first, last, find, for_each (eager)
- Tests: 104 e2e tests in `tests/std/iter_test.pr`

**0c. Numeric conversions**
- Sema: `isScalarCastType()` extends `isNumericType()` with char and bool for `as`/`as!` casts
- Codegen: `emitScalarCast()` replaces `emitNumericCast()` with `int → bool` (icmp ne 0), `float → bool` (fcmp une 0.0 — NaN is truthy), char ↔ int (zext/trunc)
- All scalar types (int, i8-i64, uint, u8-u64, f32, f64, char, bool) are castable to each other via `as`/`as!`
- Int↔String, Float↔String: `to_string()`, `format!(Writer ~w) `, `int/bool/uint/f64.parse` — all pure Promise
- Tests: 32 e2e tests in `tests/e2e/scalar_casts_test.pr`, 9 sema tests, 6 codegen tests

**0d. Format & Writer**
- File: `modules/std/format.pr` — `Writer` and `Format` structural interfaces with default `write_string` method
- File: `modules/std/builder.pr` — `Builder` type (pure Promise, wraps `Vector[u8]`, satisfies `Writer`)
- Primitives have `to_string()` via string interpolation (`"{this}"`)
- All primitives (`int`, `i8`-`i64`, `uint`, `u8`-`u64`, `f32`, `f64`, `bool`, `char`, `string`) implement `format!(Writer ~w) `
- String interpolation desugaring to Format. User-defined types implementing `format!(Writer ~w) ` are now supported in `{}` interpolation. A Builder is created internally, the type's format method writes to it, and the result is converted to string via `Builder.to_string()`. Both direct dispatch and vtable dispatch (polymorphic) are supported. Value types are also supported.

**0e. Parse & Reader**
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
// Sort a vector in place and return it (consumes its argument): v = sort(move v)
sort[T: Ordered](T[] move vec) T[];

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

#### 1c. String Utilities (split across `std/string.pr` + `modules/strings/`)

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

#### 2a. Numeric Formatting & Parsing

- `to_string()` on all primitives (int, i8-i64, uint, u8-u64, f32, f64, bool, char, string) — uses `"{this}"` string interpolation, zero native codegen needed
- `format!(Writer ~w) ` on all primitives — delegates to `w.write_string(this.to_string())` (string uses `w.write_string(this)`)
- `int.parse(Reader ~r) int!` — pure Promise, reads digits with optional leading `-`, stops at first non-digit
- `bool.parse(Reader ~r) bool!` — pure Promise, reads "true"/"false" byte-by-byte
- `uint.parse(Reader ~r) uint!` — pure Promise, reads digits, stops at first non-digit
- `f64.parse(Reader ~r) f64!` — pure Promise, handles sign, integer/fractional parts, scientific notation (e/E)
- Tests: `tests/std/to_string_test.pr` (21 tests), `tests/std/parse_test.pr` (38 tests), `tests/std/format_test.pr` (20 tests)

**String interpolation desugaring to `format()`.** User-defined types implementing `format!(Writer ~w) ` are now supported in `{}` interpolation via Builder. Both direct and vtable (polymorphic) dispatch supported.

**Design change from original plan**: `to_string()` uses string interpolation (`"{this}"`) directly instead of wrapping `format()` through a Builder. This is simpler, has zero native codegen, and works today. `format!(Writer ~w) ` is separately implemented for composable output to arbitrary Writers.

- **Files**: `modules/std/int.pr`, `modules/std/uint.pr`, `modules/std/float.pr`, `modules/std/bool.pr`, `modules/std/char.pr`, `modules/std/string.pr`
- **Test**: `tests/std/to_string_test.pr`, `tests/std/parse_test.pr`, `tests/std/format_test.pr`

#### 2b. `modules/std/builder.pr` — Builder

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

#### 3a. `modules/std/math.pr` — Extended Math (LLVM Intrinsics)

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

#### 3b. `modules/std/random.pr` — Pseudorandom Numbers

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

#### 3c. `modules/std/time.pr` — Duration & Instant

- `Duration` — pure value type (`int nanos `value`). Factory constructors: `from_nanos`, `from_micros`, `from_millis`, `from_secs`, `zero`. Getters: `as_nanos`, `as_micros`, `as_millis`, `as_secs`. Arithmetic: `+`, `-`, `*`. Full comparison operators. `to_string()` with adaptive units (ns/us/ms/s). `format!(Writer ~w) `.
- `Instant` — pure value type. `now()` factory (calls `_nanotime` extern). `elapsed()`, `duration_since()`. Comparison operators.
- `sleep(Duration d)` — free function, calls `_sleep_nanos` extern. WASM: no-op.
- Native codegen: `promise_nanotime` (clock_gettime CLOCK_MONOTONIC), `promise_sleep_nanos` (nanosleep(2)). Bodies in `io.go:definePALBodies`. Test runner uses separate `.promise_nanotime_raw` to avoid ABI conflict.
- **File**: `modules/std/time.pr`
- **Test**: `tests/std/time_test.pr` (24 tests)

---

### Phase 4: System I/O

#### 4a. `modules/std/io.pr` — Extended I/O (Closer Interface, Utilities)

- `Closer` — structural interface with `close!(~this) ` abstract method. Any type with a matching `close` method satisfies it.
- `write_line!(Writer ~w, string s) ` — convenience function, writes string + newline.
- `read_all`/`read_string` deferred to Phase 4b (File) when there are concrete Reader sources beyond Scanner.
- **File**: `modules/std/io.pr` (extended)
- **Test**: `tests/std/test_io.pr` (4 tests)

#### 4b. `modules/io/io.pr` — File System Access

```promise
type File {
    // File handle with RAII cleanup

    int _fd;

    // Factory constructors
    open_read!(string path) Self `factory; // O_RDONLY
    open_write!(string path) Self `factory; // O_WRONLY
    open!(string path) Self `factory; // O_RDWR
    create!(string path) Self `factory; // write-only, create/truncate
    append!(string path) Self `factory; // write-only, append, create if needed
    ~~open_mode!(string path, string mode) Self `factory;~~ // NOT implementing

    // Byte-level I/O (Reader/Writer interface compliance)
    read!(~this, u8[] ~buf) int ; // reads up to buf.len bytes into buf; returns bytes read (0 = EOF)
    write!(~this, u8[] ~buf) int ; // writes bytes from buf; returns bytes written

    // Convenience methods
    read_all!(~this) string ;
    write_string!(~this, string s);
    read_line!(~this) string? ; // reads one line, absent at EOF

    // Position
    get position! int ;
    seek!(~this, int offset);

    // Resource management
    close!(~this);
    drop(~this);                           // auto-close on scope exit

    // Global (static) convenience methods
    read_content!(string path) string `global;
    write_content!(string path, string content) `global;
    exists(string path) bool `global;
    size!(string path) int `global;
    remove!(string path) `global;
}

type BufferedReader {
    // Buffered file reader — reduces syscalls by reading in chunks.
    new(~this, File file, int buf_size = 4096);
    read!(~this, u8[] ~buf) int ;
    read_line!(~this) string? ;
    read_byte!(~this) u8? ;
    close!(~this);
}

type BufferedWriter {
    // Buffered file writer — reduces syscalls by batching writes.
    new(~this, File file, int buf_size = 4096);
    write!(~this, u8[] ~buf) int ;
    write_string!(~this, string s);
    flush!(~this);
    close!(~this);
}
```

- **File**: `modules/io/io.pr` (separate `io` module, not part of `std`)
- **Dependencies**: PAL file I/O (3.1), Error type (Phase 0a), `Reader`/`Writer`/`Closer` (4a)
- **Native codegen**: `File.open` → PAL `EmitFileOpen`, `File.read` → PAL `EmitFileRead`, etc.
- **Implementation**: Thin wrapper around PAL calls. `File.read(~this, u8[] ~buf) int!` and `File.write(~this, u8[] ~buf) int!` satisfy the `Reader`/`Writer` structural interfaces. `read_line` is a File instance method (not a free function). `BufferedReader`/`BufferedWriter` are pure Promise wrappers around `File` that reduce syscalls by chunked I/O; both also satisfy `Reader`/`Writer` via their `read`/`write` methods.
- **Test**: `modules/io/io_test.pr` (69 tests)

#### 4c. `modules/path/path.pr` — Path Manipulation

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

#### 4d. `modules/os/os.pr` — OS Interaction

```promise
type OsError is error `public { int code; }

type ProcessResult `public {
    int exit_code;
    string stdout;
    string stderr;
}

// One-shot execution
get_env_var(string name) string?;
get working_dir! string ;
exit_process(int code);
get args string[];
executable_path() string;
execute!(string program, ...string arguments) ProcessResult ;
set_env_var(string name, string? move value);
set_working_dir(string path) !;

// Streaming process execution
type ProcessInput `public { ... }   // satisfies Writer: write, write_string, write_line, close, drop
type ProcessOutput `public { ... }  // satisfies Reader: read, read_all, close, drop

// Process supervision (T1529): split by provenance — attached vs started
type ProcessRef `public {
    attach!(int pid) Self `factory; // non-invasive: no ptrace/stop/notification
    get id int;
    get start_time int;            // comparable, platform-specific
    get is_alive bool;             // guards against pid reuse via start_time
    signal!(~this, Signal sig);    // graceful signal (SIGTERM/SIGINT/SIGHUP)
}
type Process is ProcessRef `public {
    spawn!(string program, ...string arguments,
           map[string, string]? environment,
           string? working_directory,
           bool new_process_group) Self `factory;
    take_stdin!(~this) ProcessInput ;
    take_stdout!(~this) ProcessOutput ;
    take_stderr!(~this) ProcessOutput ;
    wait!(~this) int ; // closes stdin, returns exit code (cached)
    kill!(~this); // SIGKILL
    signal_group!(~this, Signal sig); // signal child + all descendants (requires new_process_group)
    drop(~this);                   // close fds + reap zombie
}

// OS info getters
get env map[string, string];          // all env vars as map
get user_name string;                 // login name (getpwuid)
get user_id int;                      // uid
get group_id int;                     // gid
get home_dir string;                  // home dir (getpwuid)
get hostname string;                  // machine hostname
get process_id int;                   // current pid

// Signal handling
enum Signal { Interrupt, Terminate, Hangup }
setup_signal_handling!(...Signal signals); // register signals via pipe+handler
receive_signal!() Signal ; // block until signal arrives
```

- **File**: `modules/os/os.pr` (separate `os` module, not part of `std`)
- **Dependencies**: PAL OS (getenv, getcwd, exit, setenv, unsetenv, chdir, spawn, spawn_streaming, kill, process_alive, process_start_time, kill_group, spawn_job_handle, get_environ, get_user_info, get_hostname, signal_init, signal_register), argc/argv globals from main prologue
- **Native codegen**: Extern bridge pattern in `os_bridges.go` — Promise declares `_os_func() T \`extern("promise_os_func");`, codegen provides LLVM IR body bridging Promise types ↔ PAL. `execute` uses three-extern + TLS caching pattern. Streaming process uses six externs. Process supervision uses four externs (T1529). OS info uses six externs. Signal handling uses pipe-based async-signal-safe delivery: `pal_signal_init` creates pipe + defines handler, `pal_signal_register` calls `signal(2)`. The `env` getter builds `map[string, string]` in pure Promise from the string[] of "KEY=VALUE" entries.
- **Test**: `modules/os/os_test.pr` (135 tests, excluded on WASM)

#### 4e. Standard Input (merged into `modules/io/io.pr`)

```promise
// Read a line from stdin (blocking) — free function in io module
read_line!() string? ;

// Read all of stdin
read_stdin!() string ;

// Per-file read_line is a File instance method:
// file.read_line!() string? ;
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

parse_json!(string input) JsonValue ;
to_json(JsonValue value) string;
```

- **Dependencies**: Phase 2 (string operations), Phase 0a (error types)
- **Implementation**: Recursive descent parser in pure Promise

#### 5b. `modules/regex/regex.pr` — Regular Expressions

```promise
type Regex {
    compile!(string pattern) Self `factory;
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
    bind!(string host, int port) Self `factory;   // host must be an IPv4 literal
    accept!(~this) TcpStream;
    set_accept_deadline(~this, Instant? deadline);
    get accept_deadline Instant?;
    create_cancel_handle(this) CancelHandle;
    get local_port int;
    close!(~this);
    get local_port int;
}

type TcpStream {
    connect!(string host, int port, Instant? deadline) Self `factory; // host: name, IPv4 or IPv6 literal
    read!(~this, u8[] ~buf) int;
    write!(~this, u8[] ~data) int;
    set_read_deadline(~this, Instant? deadline);
    set_write_deadline(~this, Instant? deadline);
    set_deadline(~this, Instant? deadline);
    get read_deadline Instant?;
    get write_deadline Instant?;
    create_cancel_handle(this) CancelHandle;
    shutdown!(~this, bool read, bool write);
    close!(~this);
    shutdown!(~this, bool read = false, bool write = false);
}

// Name resolution (T1518)
resolve!(string host) string[];   // A + AAAA, in the resolver's preferred order

enum ResolveErrorKind { not_found, try_again, failed, unsupported }
type ResolveError is NetError { string host; ResolveErrorKind kind; }

// Deadlines and cancellation (T1563)
type CancelHandle {
    cancel(this);
}
```

**Name resolution (T1518).** `resolve` returns every address the platform
resolver reports for a host, in the order it prefers (RFC 6724 destination
ordering), in presentation form — `"127.0.0.1"`, `"::1"`. Numeric literals are
accepted and returned unchanged. `TcpStream.connect` resolves its `host` and
tries the resulting addresses in order, closing each socket and moving to the
next on failure, so a v6-first list on a v4-only network fails fast with
`ENETUNREACH` and falls through to the v4 address. Racing the families in
parallel (Happy Eyeballs) is deliberately out of scope, as is
`TcpListener.bind` by name — binding a name that resolves to several addresses
has no single obvious meaning.

An **empty host always fails**, with `ResolveErrorKind.not_found`, on every
target that has a resolver and for both `resolve` and `TcpStream.connect`. (WASM
has none, and answers `unsupported` for every host, empty or not — see below.)
The platform resolvers do
not agree on it — Windows' `getaddrinfo` succeeds and returns every local
interface address, BSD/macOS succeeds and returns loopback, glibc reports
`EAI_NONAME` — so leaving the decision to them made `connect("", port)` a silent
connection to *this machine* on two of the three. An empty host is what a
mis-parsed URL or an unset config field produces, so it has to fail diagnosably
rather than plausibly succeed. The rejection lives in
`pal_resolve_host`, below the bridge, which is what makes it one behavior rather
than three (T1726).

Resolution runs on the platform resolver (`getaddrinfo`), **not** on the netpoll
reactor. The reactor can only wait on an fd, whereas the resolver owns
`/etc/hosts`, search domains, `resolv.conf` options and mDNS; reimplementing DNS
on the reactor would lose all of that. The scheduler stays unblocked via the
same P-handoff every other thread-blocking call uses (T1685) — the calling M is
released along with its P, so no other goroutine is delayed. The consequence is
that a dead nameserver stalls the *calling goroutine* for as long as the
resolver takes (~10s on musl); `connect`'s deadline (T1563) bounds the
connection wait, not the resolution that precedes it (a caller-visible resolve
timeout is tracked as T1736).

`ResolveError` inherits `NetError`, so code that already catches `NetError`
keeps catching resolution failures unchanged, while callers that need to tell
"no such host" from "connection refused" can catch `ResolveError` and read its
`kind`. `kind` is the only place that reason is recorded — the inherited
`NetError.code` stays `0`, because the normalized resolver codes are not errnos
and `_net_strerror` would mistranslate them. The `EAI_*` numbering is mutually
incompatible across Linux, macOS and Windows, so the PAL normalizes it to one
vocabulary rather than exposing three. On WASM there is no resolver and
`resolve` raises `ResolveErrorKind.unsupported`.

Discriminating the error currently requires the type in scope unqualified
(`use net as _;`): `? e is net.ResolveError` does not parse (T1701) and
`e as net.ResolveError` panics codegen (T1538). Both are pre-existing
cross-module RTTI gaps, not specific to this module.



- **Deadlines** (T1563): every blocking operation can be bounded in time by an
  absolute `Instant` deadline — `Instant.now() + Duration.from_secs(5)` for a
  relative timeout, `none` to clear. `connect` takes its deadline as an optional
  argument; `accept`, `read` and `write` read it from the listener/stream. On
  expiry the operation raises a `NetError` with `is_timeout`, so callers can tell
  "timed out" from "connection refused"/"reset". Expiry is detected by the IO
  reactor on its regular poll pass, so it can overshoot by a millisecond or two —
  the right granularity for socket deadlines, and no platform timer is needed.
  A deadline only ever bounds a wait, so it can only win against an operation
  that waited: an error the syscall itself returns is never rewritten into a
  timeout, and a `connect(2)` the kernel refuses on the call reaches the caller
  as that socket errno even when the deadline had already passed. `is_timeout`
  answers "did this run out of time", not "was the peer reachable".
- **Cancellation** (T1563): `create_cancel_handle` returns a `CancelHandle` that
  can be moved into another goroutine and used to unblock whatever is parked on
  the socket. Cancellation is sticky — once cancelled, later operations on that
  socket raise a `NetError` with `is_cancelled` rather than parking. The handle
  reference-counts the underlying poll descriptor, so it stays valid (and
  harmlessly inert) after the socket it came from is closed or dropped.
- **WASM**: no deadline machinery is emitted at all. WASM has no IO reactor and
  the PAL socket calls it would bound already fail with `ENOSYS`, so there is
  nothing to time out — the deadline setters are inert rather than a separate
  error surface.
- **Dependencies**: PAL socket extensions, IO reactor (epoll/kqueue), PAL
  `getaddrinfo`/`inet_ntop`

#### 5d. `modules/http/http.pr` — HTTP Client & Server

HTTP/1.1 over the `net` module, with https:// carried over the `tls` module
(T0079). Convenience functions plus a reusable `Client` and a `Server`:

```promise
// One-shot helpers (redirect-following + gzip via a transient Client).
http_get!(string url) Response;
http_post!(string url, string body, map[string, string] headers) Response;
http_post_json![T: Encodable](string url, T data) Response;

// Reusable client with redirects, keep-alive pooling, and gzip.
Client client = Client.create(max_redirects: 10, auto_gzip: true);
Response r = client.get("http://host/path")?;   // also post!, send!, close
Response s = client.get("https://host/path")?;  // same client, same call

// A custom TLS configuration — custom CA, mutual TLS, or (testing only)
// verification off. Closes every pooled connection first, so the replacement
// is total: no later request can be answered over a connection negotiated
// under the configuration that was just replaced, and no TLS session outlives
// the backend context it was created from.
client.set_tls_config(tls.TlsConfig.insecure());

// An HTTPS server is a Server like any other; only the bind differs.
Server server = Server.bind_tls("0.0.0.0", 8443, cert_pem, key_pem)?;
server.serve(move handler)?;
```

**Scheme handling.** `_parse_url` accepts `http://` (default port 80) and
`https://` (default port 443) and nothing else. A `_Url` carries a `secure`
flag, which decides the transport, the port omitted from the `Host` header, and
the connection pool's key — pool keys are `scheme://host:port`, so a plaintext
and a TLS connection to the same host:port can never be handed to each other's
requests. A redirect that changes the scheme is a cross-origin hop, so
`Authorization` and `Cookie` are stripped, exactly as for a host or port change.

**Transport.** A private `_Transport` interface (`read`, `write`, `close`,
`graceful_close`) has two implementations: `_PlainTransport` over
`net.TcpStream` and `_TlsTransport` over `tls.TlsStream`. Response framing,
chunked decoding, the server's request loop and the connection pool are written
once against it, so http and https cannot drift apart. A TLS failure surfaces as
`tls.TlsError` rather than `HttpError`, so a caller can still tell a certificate
problem from a network one.

**Server-side handshake placement.** `Server.bind_tls` binds a *plain*
`net.TcpListener` and upgrades each accepted connection inside that connection's
own goroutine. Handshaking in the accept loop would let one slow or non-TLS peer
stall every other connection, and would turn each failed handshake into a
listener error that ends `serve()`. As written, a failed handshake costs exactly
one connection.

**Cost.** Importing `http` links a TLS backend even for a program that only ever
speaks `http://` — on Linux that is the vendored static OpenSSL. Measured on
linux-amd64 for `use http; main() { print_line("hi"); }`:

| program | debug | release |
|---|---|---|
| `use net;` | 0.63 MB | 0.63 MB |
| `use http;` | 13.4 MB | 19.0 MB |

Release exceeding debug is T1707: the LTO link path passes `--lto-O1` but not
`--gc-sections`, so OpenSSL's pre-compiled archive members — invisible to LTO's
own DCE — are never section-stripped.

The alternative (an injectable transport provider, or a separate `https` module)
was rejected: it reintroduces the hidden configuration and "https is not
supported here" action-at-a-distance the language design forbids.

- **`Request`/`Response`**: build requests (`new_get`/`new_post`, `set_header`),
  read responses (`status`, `headers`, `body`, `body_bytes`, `json[T]`,
  `is_success`). `Request.send` performs a single `Connection: close` exchange
  (gzip decoded) with no redirects or pooling.
- **`Client`** (T0447): follows up to `max_redirects` redirects
  (301/302/303/307/308). Method/body policy — 303 → GET (body dropped); 307/308
  preserve method+body; 301/302 preserve GET/HEAD but rewrite other methods to
  GET. `Authorization`/`Cookie` are stripped on cross-host redirects. Idle
  keep-alive connections are pooled per `host:port` (LIFO, stale-connection
  retry). Sends `Accept-Encoding: gzip` and decodes `Content-Encoding: gzip`
  responses when `auto_gzip`.
- **`Server`** (T1519): `bind` + `serve` with a `Handler`, `ServerRequest`/
  `ServerResponse`, and graceful shutdown via `ServerShutdownHandle`. Each
  accepted connection is handled on its own goroutine, so one slow handler
  cannot stall the others. `bind`'s `max_connections` (default 128) bounds how
  many connections are in flight — beyond it the server stops accepting and
  further connections wait in the listen backlog — and
  `max_keep_alive_requests` (default 100) bounds how many requests one
  persistent connection may serve. Both must be at least 1 — `bind` raises
  rather than accept a bound that would leave the server unable to serve
  anything. HTTP/1.1 connections are kept alive by default (HTTP/1.0 only on an
  explicit `Connection: keep-alive`); the server owns response framing, so a
  handler-set `Connection`, `Content-Length` or `Transfer-Encoding` header is
  replaced, a request the server cannot frame (`Transfer-Encoding`, since only
  `Content-Length` framing is implemented) gets `411 Length Required` and loses
  the connection, and a `HEAD` reply keeps its `Content-Length` but carries no
  body. Because one handler is shared across all connection goroutines,
  `Handler.handle` takes a shared `this` receiver: handler state must be
  immutable or guarded by `Mutex[T]`. `serve` returns only after every in-flight
  connection has finished.

- **Dependencies**: `modules/net/net.pr`, `modules/json/json.pr`, `modules/gzip/gzip.pr`

#### 5e. `modules/crypto/` — Cryptographic Primitives

Shipped as three files: `sha256.pr`, `constant_time.pr`, and `random.pr`.

```promise
type Digest256 `doc("A SHA-256 digest: 32 bytes as a big-endian u256, H0 in the most significant 32 bits.") {
    u256 value `value;

    to_string(this) string  `doc("Canonical lowercase 64-character hex, zero-padded.");
    to_bytes(this) u8[]     `doc("The 32-byte big-endian byte string.");
    ^ (Digest256 other) Digest256 `doc("Bitwise XOR (PBKDF2's U1 ^ U2 ^ ... fold).");
    == (Digest256 other) bool;
    get hash int;
}

type Sha256 `doc("Streaming SHA-256 context: update() repeatedly, then finalize() once.") {
    new(~this);
    update(~this, u8[] data);
    finalize(~this) Digest256;
}

sha256(u8[] data) Digest256 `doc("One-shot digest; prefer this when the whole input is in hand.");
constant_time_equal(u8[] a, u8[] b) bool `doc("Data-independent comparison for MACs, signatures, and secret tokens.");

type CryptoError is error `doc("Error raised when the operating system cannot provide random bytes.");
random_bytes!(int count) u8[] `doc("Returns count cryptographically-secure random bytes. Raises on negative count or OS failure.");
```

- **Implementation**: SHA-256 and constant-time comparison are pure Promise, built on the `u256` wide integer in `modules/std/wide_int.pr`. `random_bytes` is the one sanctioned PAL fallback — a syscall (`getentropy` on POSIX, `BCryptGenRandom` on Windows, `random_get` on WASI), not an external library.
- **Remaining**: HMAC-SHA-256 (T1567) and PBKDF2 (T1568) are not yet built.

#### 5f. `modules/std/embed.pr` — Resource Embedding Types

Types supporting the `` `embed(path) `` compile-time annotation (see [language-design.md](language-design.md#86-resource-embedding-embed)). Single-file embeds use `string` or `u8[]` directly; directory tree embeds use `EmbeddedFiles`.

```promise
type EmbeddedFiles `doc("Virtual read-only filesystem for compile-time embedded directory trees.") {
    EmbeddedFile[] _entries `final;
    u8[] _data `final;

    get files EmbeddedFile[] `doc("List all embedded entries.") => this._entries;

    read(this, string path) string!
        `doc("Read an embedded file as a UTF-8 string. Raises if path not found.");

    read_bytes(this, string path) u8[]!
        `doc("Read an embedded file as raw bytes. Raises if path not found.");

    contains(this, string path) bool
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
    open!(string path) Self `factory {
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
    v = sort(move v);
    assert(v.len == 0, "empty vector should remain empty");
}

test "sort integers" {
    mut v := [3, 1, 4, 1, 5, 9, 2, 6];
    v = sort(move v);
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

| Phase | File | Type | New PAL | Lines | Notes |
|-------|------|------|---------|-------|-------|
| 0a | `modules/std/error.pr` | Promise | No | 3 | |
| 0b | `modules/std/iter.pr` | Promise | No | 257 | |
| 0c | `modules/std/int.pr` etc. | Native | No | 909 | |
| 0d | `modules/std/format.pr` | Promise | No | 495 | |
| 0e | `modules/std/parse.pr` | Promise | No | 43 | |
| 1a | `modules/std/set.pr` | Promise | No | 107 | |
| 1b | `modules/std/sort.pr` | Promise | No | 91 | |
| 1c | `modules/std/string.pr` + `modules/strings/` | Promise | No | 199+65 | |
| 1d | `modules/std/result.pr` | ~~Promise~~ | No | — | Not planned — `T!` is function-level, so the helpers are inexpressible (§4) |
| 2a | (merged into 0c) | — | — | — | |
| 2b | `modules/std/builder.pr` | Promise | No | 38 | |
| 2c | `modules/std/fmt.pr` | ~~Promise~~ | No | — | Not planned — interpolation covers it; belongs in a catalog module (§4) |
| 3a | `modules/std/math.pr` + `modules/math/` | Native + Promise | No | 111+67 | |
| 3b | `modules/std/random.pr` | Promise | No | 165 | |
| 3c | `modules/std/time.pr` | Promise + Native | 3 | 96 | |
| 4a | `modules/std/io.pr` | Promise | No | 97 | |
| 4b | `modules/io/io.pr` | Promise + Native | 12 | 548 | |
| 4c | `modules/path/path.pr` | Promise | No | 192 | |
| 4d | `modules/os/os.pr` | Promise + Native | 13 | 511 | |
| 4e | (merged into 4b) | — | — | — | |
| — | `modules/std/platform.pr` | Promise | No | 33 | |
| 5a | `modules/json/json.pr` | Promise | No | 1,003 | |
| 5b | `modules/regex/regex.pr` | Promise | No | ~400 | Design only |
| 5c | `modules/net/net.pr` | Promise + Native | 6+ | 281 | |
| 5d | `modules/http/http.pr` | Promise | No | 1,644 | |
| 5e | `modules/crypto/` | Promise | No | 239 | |
| 5f | `modules/std/embed.pr` | Promise | No | 54 | |
| | **Phases 0-4** | | **28** | **4,027** | `result` and `fmt` not planned |
| | **Total** | | **34+** | **7,248** | |
