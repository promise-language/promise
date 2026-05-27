# Promise Language Guide

Concise reference for writing correct Promise code.

---

## Basics

```promise
// Entry point (optional — not needed for library code or tests)
main() {
  print_line("Hello, world!");
}

// Variables: type-first or inferred
int x = 42;
string name = "Alice";
bool ok = true;
x := 42;              // inferred as int
name := "Alice";       // inferred as string

// Mutability: variables are mutable by default

// Printing (std auto-imported, no `use` needed)
print("no newline");
print_line("with newline");
print_line("interpolation: {x} + {name}");
```

## Primitive Types

| Type | Description | Literals |
|------|-------------|----------|
| `int` / `i8` `i16` `i32` `i64` | Signed integers | `42`, `-1i8`, `0xFF`, `0b1010`, `1_000` |
| `uint` / `u8` `u16` `u32` `u64` | Unsigned integers | `42u`, `255u8`, `0o77u32` |
| `f32` `f64` | Floats | `3.14`, `1.0f32` |
| `bool` | Boolean | `true`, `false` |
| `char` | Unicode character | `'a'`, `'\n'`, `'\u{1F600}'` |
| `string` | UTF-8 string | `"hello"`, `"val={x}"`, `r"raw\n"` |

Bare numeric literals infer as `int` or `f64`. Use suffixes for specific types.

**String escapes:** `\n` `\t` `\r` `\b` `\\` `\"` `\0` `\{` (literal `{`, suppresses interpolation). Raw strings (`r"..."`) skip all escapes.

## Type Declarations

```promise
// Struct (heap-allocated by default)
type User {
  string name;
  int age;
  string? bio;          // optional field, defaults to none
  int score = 0;        // field with default
  string id `final;     // immutable after construction
}

// Construction uses named arguments
u := User(name: "Alice", age: 30, id: "abc");

// Value type (all fields `value — stack-allocated, auto-copy)
type Vec2 {
  f64 x `value;
  f64 y `value;
}

// Inheritance
type Animal {
  string name;
  speak() string `abstract;   // must be overridden
}

type Dog is Animal {
  string breed;
  speak() string { return "Woof!"; }
}

// Structural interface (auto-satisfied by matching methods)
type Printable `structural {
  to_display() string `abstract;
}
// Any type with to_display() string satisfies Printable — no `is` needed
```

## Enums

```promise
enum Direction { North, South, East, West }

enum Shape {
  Circle(f64 radius),
  Rectangle(f64 width, f64 height),
  Point,

  // Methods declared after variants
  area(&this) f64 {
    match this {
      Shape.Circle(r) => { return 3.14159 * r * r; },
      Shape.Rectangle(w, h) => { return w * h; },
      Shape.Point => { return 0.0; },
    }
  }

  // Getter (accessed as .name, no parens)
  get is_flat bool {
    match this {
      Shape.Point => { return true; },
      _ => { return false; },
    }
  }
}

// Usage
d := Direction.North;
s := Shape.Circle(radius: 3.14);
print_line("{s.area()}");      // method call
print_line("{s.is_flat}");     // getter, no parens

// Pattern matching (must be exhaustive)
match s {
  Shape.Circle(r) => print_line("radius: {r}"),
  Shape.Rectangle(w, h) => print_line("{w}x{h}"),
  Shape.Point => print_line("point"),
}
```

## Methods

Methods are declared inside the type body.

```promise
type Counter {
  int value;

  // Shared borrow (read-only) — default
  current(&this) int { return this.value; }

  // Mutable borrow (can modify)
  increment(~this) { this.value += 1; }

  // Expression body shorthand
  is_zero(&this) bool => this.value == 0;

  // Getter (accessed as .count, no parens)
  get count int => this.value;

  // Factory method (static, called as Counter.zero())
  zero() Self `factory { return Self(value: 0); }
}

c := Counter(value: 0);
c.increment();            // ~this: compiler auto-borrows mutably
print_line("{c.count}");  // getter, no parens
```

## Functions

```promise
add(int a, int b) int {
  return a + b;
}

// With defaults and optional params
greet(string name, string greeting = "Hello", string? title) string {
  if t := title {
    return "{greeting}, {t} {name}!";
  }
  return "{greeting}, {name}!";
}

greet("Alice");                           // "Hello, Alice!"
greet("Bob", greeting: "Hi");             // "Hi, Bob!"
greet("Eve", title: "Dr");                // "Hello, Dr Eve!"

// Variadic parameters
sum(...int nums) int {
  int total = 0;
  for n in nums { total += n; }
  return total;
}
sum(1, 2, 3);   // 6
```

## Error Handling

**Critical: `?!` = panic on error. `!` = optional unwrap. Bare call = auto-propagate.**

```promise
// Failable function: ! after name marks it as failable
read_config!(string path) string {
  string content = io.File.read_content(path);  // bare call: auto-propagates
  return content.trim();
}

// Auto-propagation works in all expression positions:
process_config!(string path) string {
  return transform(io.File.read_content(path));  // call argument: auto-propagates
}

// Void failable function (no return type needed)
validate!(string input) {
  if input.is_empty { raise error(message: "empty"); }
}

// Handling errors
main() {
  // Panic on error (ONLY for prototyping)
  string data = read_config("app.cfg")?!;

  // Handle with recovery value
  string data = read_config("app.cfg") ? {
    "defaults";
  };

  // Handle with error binding
  string data = read_config("app.cfg") ? e {
    print_line("Error: {e.message()}");
    return;
  };

  // Typed handler
  string data = read_config("app.cfg") ? e is IoError {
    "io fallback";
  }!;   // panic on non-IoError
}
```

| Syntax | Behavior | Where |
|--------|----------|-------|
| `foo()` | Auto-propagate error | In `!` functions only |
| `bar(foo())` | Auto-propagate error from argument | In `!` functions only |
| `x = foo()` | Auto-propagate error from assignment | In `!` functions only |
| `foo()?^` | Explicit propagate (same as bare call) | In `!` functions only |
| `foo()?!` | **Panic** on error | Anywhere |
| `foo() ? e { ... }` | Handle error | Anywhere |
| `foo() ? e is T { ... }` | Handle typed error | Anywhere |

**Rule: In a failable (`!`) function, just call failable functions bare — errors auto-propagate. Use `?^` for explicit self-documenting propagation. `?!` always means panic.**

**What NOT to do:**
```promise
// WRONG: there are no failable types — only failable functions
int! x = 5;                    // compile error: ! goes on function name, not type

// WRONG: ?! always panics, even in failable functions (does NOT propagate)
process!() string {
  return read_config("app.cfg")?!;  // panics instead of propagating — just use bare call
}

// RIGHT: bare call auto-propagates in failable functions
process!() string {
  return read_config("app.cfg");    // error auto-propagates to caller
}

// Both are valid — trailing ; is optional on the last expression in a block
string data = read_config("cfg") ? { "default" };
string data = read_config("cfg") ? { "default"; };
```

## Optionals

```promise
string? name = none;
string? greeting = "hello";   // auto-wrapped

// Unwrap with if-binding (preferred)
if n := name {
  print_line(n);    // n is string, not string?
}

// Truthiness narrowing (works for non-bool types)
if name {
  print_line(name);  // narrowed to string
}

// Presence check
if name is present { ... }
if name is absent { ... }

// Force unwrap (panics on none)
string s = name!;

// Default value (elvis operator)
string s = name ?: "unknown";

// Optional handler (note: no space before ?)
string s = name? _ {
  "fallback";
};

// Optional chaining
string? n = user?.name;
```

## Control Flow

```promise
// If / else
if x > 0 {
  print_line("positive");
} else if x == 0 {
  print_line("zero");
} else {
  print_line("negative");
}

// For loops
for i in 0..10 { }           // 0 to 9
for i in 0..=10 { }          // 0 to 10 (inclusive)
for item in collection { }
for i, item in collection { }  // indexed
for i := 0; i < 10; i += 1 { }  // C-style

// While
while condition { }
while item := iter.next() { }  // unwrap binding (loop while non-none)

// While-unwrap with re-borrowing: the condition's borrow expires before the body,
// so the body can freely call methods on the same object.
while key := decoder.next_key()?! {
  string val = decoder.read_string()?!;  // re-borrows decoder — OK
}

// Match (must be exhaustive, commas between arms)
match value {
  1 => print_line("one"),
  2 => print_line("two"),
  _ => print_line("other"),
}

// Match with expression patterns (multi-way conditional dispatch)
match true {
  n % 15 == 0 => "fizzbuzz",
  n % 3 == 0 => "fizz",
  n % 5 == 0 => "buzz",
  _ => n.to_string(),
}

// Type checks and casts
if animal is Dog { animal.breed; }   // narrowed
Dog? dog = animal as Dog;            // safe cast (returns optional)
Dog dog = animal as! Dog;            // forced cast (panics on failure)

// Destructure is-patterns (enum variants and named types)
if shape is Circle(r) {
  print_line("radius: {r}");        // r is bound from Circle's field
}
if opt is Some(val) {
  print_line("value: {val}");       // works with generic enums
}
if animal is Dog(name, breed) {
  print_line("{name} is a {breed}");  // named type field extraction
}
```

## Ownership & Borrowing

```promise
// Three parameter modes (declared on the parameter):
read_only(string &s) { }     // shared borrow (read)
mutate(string ~s) { }        // mutable borrow (exclusive write)
consume(~string s) { }       // takes ownership (moved into callee)
borrow(string s) { }         // shared borrow — caller still owns; callee may not consume

// At call site — NO markers. Compiler infers borrow from declaration:
read_only(val);               // auto-borrows &val
mutate(val);                  // auto-borrows ~val
consume(val);                 // val is moved, cannot use after
borrow(val);                  // val is borrowed, still valid after

// Copy types (auto-copy, no move): primitives, bool, char, pure value types
// Move types: string, collections, heap types — plain `T` param is a borrow;
// add `~T` to consume. Trying to move out of a plain-`T` parameter inside the
// callee (e.g., into a struct field) is a compile-time error.

// Implicit `T& → T` and `T~ → T` decay only happens for Copy types.
// For non-Copy types (string, vectors, heap user types), use `.clone()` for an
// owned copy or declare the local/parameter as `T&` to keep it as a borrow.
//   string s = arc.borrow;          // error: cannot assign string& to string
//   string s = arc.borrow.clone();  // OK: explicit owned copy
//   string& s = arc.borrow;         // OK: kept as borrow
//   int n = arc.borrow;             // OK: int is Copy
```

## Resource Management

```promise
// use binding: auto-calls close()/drop() at scope exit
type Connection {
  int fd;
  close!(~this) { /* cleanup */ }
}
process!() {
  use conn := Connection(fd: 42);
  // conn.close() called automatically when conn goes out of scope
}

// drop: destructor called when owner goes out of scope
type Resource {
  drop(~this) { cleanup(); }
}

// File I/O: use read_content/write_content one-shot helpers, or open/close manually
use io;
main!() {
  // One-shot (preferred for simple reads/writes):
  string content = io.File.read_content("data.txt");
  // Manual open/close for streaming:
  io.File f = io.File.open("data.txt", readonly: true);
  string data = f.read_all();
  f.close();
}
```

## Generics

```promise
type Box[T] {
  T value;

  map[R]((T) -> R fn) Box[R] {
    return Box[R](value: fn(this.value));
  }
}

// Constraints
type SortedList[T: Ordered] { T[] items; }

// Generic functions
max[T: Ordered](T a, T b) T {
  if a > b { return a; }
  return b;
}

// Call with explicit type args
b := Box[int](value: 42);
result := max[int](3, 7);

// Generic functions can call other generic functions
identity[T](T val) T { return val; }
wrap[T](T val) T { return identity[T](val); }
wrap[int](42);   // works: resolves both wrap[int] and identity[int]
```

## Collections (auto-imported from std)

```promise
// Vector (T[])
int[] nums = [1, 2, 3];
nums.push(4);
int? last = nums.pop();
nums[0] = 10;
nums.contains(2);         // bool
nums.remove(0);           // remove at index
nums.len;                 // getter, no parens
nums.is_empty;            // getter, no parens
sub := nums[1:3];         // slice
int[] filled = int[].filled(0, count: 100);

// Map (map[K, V] — keys must be Hashable + Equal)
map[string, int] scores = {"alice": 100, "bob": 85};
scores["charlie"] = 92;
if v := scores["alice"] { print_line("{v}"); }   // subscript returns V?
scores.contains("key");   // bool
scores.remove("key");     // bool
scores.keys();             // K[]
scores.values();           // V[]
for k, v in scores { }     // iterate entries

// Set (Set[T] — T must be Hashable + Equal)
Set[int] s = Set[int]();
s.add(1);
s.contains(1);            // true
s.remove(1);

// String methods (on string type, auto-imported)
s.len; s.is_empty;                                     // getters, no parens
s.contains("sub"); s.starts_with("pre"); s.ends_with("suf");
s.trim(); s.split(","); s.to_upper(); s.to_lower();
s.index_of("x");          // int?
s.replace("old", "new"); s.repeat(3);
s.pad_left(10, ' '); s.pad_right(10, ' ');
```

## Lambdas

```promise
add := |int a, int b| -> int { return a + b; };
double := |int x| -> x * 2;    // expression body

// As function arguments
nums.iter().map(|int x| -> x * 2);

// Capture: copy types auto-captured, move types need `move`
string msg = "hello";
fn := move |string name| -> string { return "{msg}, {name}"; };
// msg is moved into closure, unavailable after
```

## Tuples

```promise
// Tuple type and literal
(int, string) pair = (42, "answer");

// Destructuring
(num, label) := pair;
print_line("{num}: {label}");   // "42: answer"

// As function return
divide(int a, int b) (int, int) {
  return (a / b, a % b);
}
(quotient, remainder) := divide(10, 3);

// Capturing raw failable result (value, error) as tuple
(content, err) := read_file("data.txt");
if err is present {
  print_line("Error: {err.message()}");
}
```

## Concurrency

```promise
// Fire-and-forget goroutine (G struct freed automatically on completion)
go { expensive_work(); };

// go { } block as a task (assigned to variable, awaited with <-)
t := go { expensive_work(); };
<-t;                              // blocks until done, frees G

// Task with return value (go + function call)
t := go fetch_data(url);
string result = <-t;           // blocks until done

// Channels
ch := channel[int](capacity: 10);
ch.send(42);
int? val = <-ch;

// Producer-consumer pattern
ch := channel[int](capacity: 5);
go {
  for i in 0..100 {
    ch.send(i);
  }
  ch.close();
};
for v in ch {
  print_line("{v}");
}

// Select (multiplexes channel operations)
select {
  v := <-ch1:
    print_line("from ch1: {v}");
  v := <-ch2:
    print_line("from ch2: {v}");
  default:
    print_line("no channel ready");
}
```

## Modules

**`std` is auto-imported** — no `use` needed for `print_line`, `Builder`, `Vector`, `Map`, `assert`, etc.

**Catalog modules** require `use` and have different access rules:
- **Types** always need the module prefix: `io.File`, `io.Dir`, `json.JsonValue`
- **Free functions** work without prefix after `use`: `read_line()`, `path.join(...)` are both valid

```promise
// Import a catalog module
use io;
use path;
use os;
use json;

// Import with alias
use json as j;

// Types need module prefix — ALWAYS
io.File f = io.File.open("data.txt", readonly: true);
io.Dir.make("/tmp/out");           // not Dir.make(...)
json.JsonValue v = json.parse_value(data);

// Free functions work after use
string line = read_line();          // io.read_line — prefix optional
string p = path.join("/usr", "bin"); // path functions

// Available modules (use `promise doc <module>` to explore):
// io      — File, Dir, BufferedReader/Writer, read_line (stdin)
// path    — join, parent, file_name, extension, stem, normalize
// os      — env vars, working_dir, execute (variadic), exit_process
// json    — encode_string, decode_string (generic, via Encodable/Decodable)
// math    — lerp, map_range, gcd, lcm, sign, is_even, is_odd
// strings — join, spaces, reverse, is_blank, repeat_join
```

## Tests

```promise
// Test function (discovered by `promise test`)
test_addition() `test {
  assert(1 + 1 == 2, "basic addition");
}

// Test with expected output
test_hello() `test(expected: "hello") {
  print_line("hello");
}

// Per-test timeout (Go duration syntax: "500ms", "2s", "1m")
test_channel_send() `test(timeout: "5s") {
  // times out after 5s instead of the default 60s
}

// Snapshot test with timeout
main() `test(expected: "hello", timeout: "2s") {
  print_line("hello");
}

// Exclude from platforms
test_file_io() `test(exclude: "wasm32") {
  // ...
}
```

Run tests: `promise test file.pr` or `promise test tests/...` (recursive).

**Timeout control:**
```sh
promise test -timeout 30s tests/...                  # per-test default (applies to unannotated tests)
promise test -timeout-scale 2.0 tests/...            # double all timeouts (slow CI)
promise test -timeout-scale 0.5 tests/...            # halve all timeouts (detect sluggish tests)
promise test -timeout-max 5s tests/...               # clamp all timeouts to 5s max
promise test -timeout-min 1s tests/...               # minimum 1s per test
promise test -timeout-min 500ms -timeout-max 10s -timeout-scale 1.5 tests/...
```

Resolution: `final_timeout = clamp((annotation ?: default) × scale, min, max)`

## Generators

```promise
fibonacci() stream[int] {
  int a = 0;
  int b = 1;
  for {
    yield a;
    int temp = a;
    a = b;
    b = temp + b;
  }
}

for n in fibonacci() {
  if n > 1000 { break; }
  print_line("{n}");
}
```

## Fixed-Size Arrays

```promise
// Declaration: Type[size]
int[3] arr = [1, 2, 3];
arr[0] = 10;                  // index assign
arr.len;                      // getter, returns 3
for v in arr { }              // iterate values
for i, v in arr { }           // indexed iteration

// As a field
type Matrix {
  f64[9] data;
}
```

Fixed-size arrays are value types (stack-allocated, auto-copied). Distinct from `T[]` (heap-allocated vector).

## Explicit Constructors

```promise
// new replaces the implicit constructor
type Percentage {
  int value `final;

  new(int value) {
    if value < 0 { this.value = 0; }
    else if value > 100 { this.value = 100; }
    else { this.value = value; }
  }
}
Percentage(value: 120);   // clamped to 100

// Failable new
type Port {
  int value `final;

  new!(int value) {
    if value < 1 || value > 65535 {
      raise error(message: "invalid port");
    }
    this.value = value;
  }
}
Port(value: 80)?!;        // panics on invalid

// raise — returns error from a ! function (not an exception)
divide!(f64 a, f64 b) f64 {
  if b == 0.0 { raise error(message: "division by zero"); }
  return a / b;
}
```

## Quick Patterns

```promise
// Read a file (failable main)
use io;
main!() {
  string content = io.File.read_content("input.txt");   // auto-propagates
  print_line(content);
}

// JSON encode/decode (requires `serializable annotation)
// Uses Encodable/Decodable structural interfaces
use json;
type Config `serializable { string name; int port; }
main!() {
  // Convenience functions (recommended):
  string s = json.encode_string[Config](Config(name: "app", port: 8080));
  Config c = json.decode_string[Config](s);
  // Pretty print:
  string pretty = json.encode_string_pretty[Config](c);

  // Manual encoder/decoder (for streaming or custom formatting):
  json.JsonEncoder enc = json.JsonEncoder();
  Config(name: "app", port: 8080).encode(enc)?!;
  string s2 = enc.to_string();
  json.JsonDecoder dec = json.JsonDecoder(data: s2);
  Config c2 = Config.decode(dec)?!;
}

// Dynamic JSON (JsonValue tree API)
use json;
main!() {
  JsonValue v = json.parse_value("\{\"name\":\"Alice\",\"scores\":[10,20]}");
  if v.is_object {
    string? name = v.get("name")!.as_string();   // "Alice"
    JsonValue? scores = v.get("scores");          // Array
    f64? first = scores!.at(0)!.as_number();      // 10.0
  }
  string out = v.format();                        // compact JSON string
}

// Run a subprocess (variadic — inline args or pre-built string[])
use os;
main!() {
  r := os.execute("ls", "-la", "/tmp");
  print_line(r.standard_output);
}

// File I/O with manual open/close
use io;
main!() {
  io.File f = io.File.open("/tmp/data.txt", readonly: true);
  string content = f.read_all();
  f.close();
  print_line(content);
}

// Iterate map entries
map[string, int] m = {"a": 1, "b": 2};
for k, v in m {
  print_line("{k}={v}");
}
```

## Resource Embedding

Embed files into the binary at compile time using the `` `embed `` annotation on module-level getters:

```promise
// Embed a text file as a string (must be valid UTF-8)
get schema string `embed("schema.sql");

// Embed a binary file as raw bytes
get icon u8[] `embed("icon.png");

// Compress in binary, decompress transparently on access
get large_data string `embed("data.json", compress: true);

// Embed a directory tree as a virtual filesystem
get assets EmbeddedFiles `embed("static/...");

main() {
  print_line(schema);                          // getter access, no parens
  print_line("icon bytes: {icon.len}");
  print_line(large_data);                      // decompressed transparently
  print_line("has index: {assets.contains("index.html")}");
}
```

**Rules:**
- Paths are relative to the source file
- Only on module-level getters (not inside types or functions)
- Return type must be `string`, `u8[]`, or `EmbeddedFiles`
- Absolute paths are a compile error

## Annotations Quick Reference

```promise
type Foo `public { ... }          // exported from module
type Bar `serializable { ... }    // auto-generates encode/decode
type Pt { f64 x `value; }         // field in value struct (stack)
string id `final;                 // immutable after construction
speak() string `abstract;         // must be overridden by child
zero() Self `factory { ... }      // static constructor, returns Self
process() `test { ... }           // test function
process() `test(timeout: "5s") { ... }  // test with per-test timeout
main() `test(expected: "hi") { }  // snapshot test (checks stdout)
type Printable `structural { ... } // auto-satisfied by matching methods
get name string `embed("f.txt");  // compile-time file embed
```

## Discovery Commands

```sh
promise doc std              # browse the auto-imported standard library
promise doc io               # browse the io module (File, Dir, etc.)
promise doc json             # browse the json module
promise doc <module>         # any catalog module
promise doc file.pr          # documentation for a source file
promise -help                # quick start guide with examples
promise doc                  # list all available modules
promise targets              # list supported compile targets
```

## Compilation Targets

By default, `promise build` / `run` / `test` compile for the host platform. The `-target <triple>` flag cross-compiles to one of the supported triples. List them with:

```sh
promise targets         # text output, marks the native target
promise targets -json   # machine-readable
```

Today's supported set:

- **host triple** (default) — the platform `promise` is running on (Linux x86_64/arm64, macOS x86_64/arm64, Windows x86_64/arm64).
- **`wasm32-wasi`** — WebAssembly + WASI; runs in `wasmtime` / `wasmer` / `wasmedge`.
- **`wasm32-web`** — WebAssembly for browsers / Node.js; emits a bootstrap `.js` loader alongside the `.wasm`.

Cross-compile to a non-host OS (e.g. Linux → Windows binary) is not yet wired up — that work is tracked separately.

## Common Mistakes

1. **Using `!` on failable calls** — `!` is for optional unwrap only. Use `?!` to panic on failable errors, or use bare call for propagation in `!` functions.
2. **`&`/`~` at call site** — Don't write `func(&val)` or `func(~val)`. Just write `func(val)` — the compiler auto-borrows based on the parameter declaration.
3. **Forgetting named args** — constructors require `Type(field: value)`, not positional.
4. **Moving strings twice** — strings are not `Copy`. Use `&s` parameter to borrow, or read into separate variables.
5. **Getter parens** — `vec.len` not `vec.len()`. Getters use property syntax.
6. **Mutable methods** — use `~this` for methods that modify state. `&this` is read-only.
7. **Missing `use`** — `std` is auto-imported, but `io`, `os`, `path`, `json` need explicit `use`.
8. **Optional handler spacing** — Error handler: `expr ? e { ... }` (space before `?`). Optional handler: `expr? _ { ... }` (no space before `?`).
9. **Fixed arrays vs vectors** — `int[3]` is a fixed-size array (value type, stack). `u8[]` is a vector (heap, growable). Don't confuse them.
10. **Tuple destructuring** — Use `(a, b) := expr;` not `a, b := expr;`. Tuples need parentheses.
11. **Module type prefix** — Types always need the module prefix: `io.File.open(...)` not `File.open(...)`. Free functions don't: `read_line()` works after `use io;`.
12. **`;` in blocks** — All statements need `;`. The trailing `;` on the last expression before `}` is optional: both `? { "default" }` and `? { "default"; }` work.
13. **`?!` in failable functions** — `?!` always **panics**, even inside a `!` function. For propagation, just use bare call (or `?^` for self-documenting propagation).
14. **API discovery** — Use `promise doc <module>` to explore module APIs instead of guessing. `promise doc std` for the standard library.
