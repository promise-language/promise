# Promise Language Guide for AI Agents

Concise reference for generating correct Promise code. Load this before writing `.pr` files.

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
}

// Usage
d := Direction.North;
s := Shape.Circle(radius: 3.14);

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

**Critical: `!` = panic. Bare call = auto-propagate.**

```promise
// Failable function: return type has !
read_config(string path) string! {
  string content = io.File.read_content(path);  // bare call: auto-propagates
  return content.trim();
}

// Auto-propagation works in all expression positions:
process_config(string path) string! {
  return transform(io.File.read_content(path));  // call argument: auto-propagates
}

// Void failable function
validate(string input)! {
  if input.is_empty { raise error(message: "empty"); }
}

// Handling errors
main() {
  // Panic on error (ONLY for prototyping)
  string data = read_config("app.cfg")!;

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
| `foo()!` | **Panic** on error | Anywhere |
| `foo() ? e { ... }` | Handle error | Anywhere |
| `foo() ? e is T { ... }` | Handle typed error | Anywhere |

**Rule: In a failable (`!`) function, just call failable functions bare — errors auto-propagate. This works in variable declarations, assignments, and call arguments. Never use `!` to propagate; `!` always means panic.**

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
while item := iter.next() { }  // unwrap binding

// Match (must be exhaustive, commas between arms)
match value {
  1 => print_line("one"),
  2 => print_line("two"),
  _ => print_line("other"),
}

// Type checks and casts
if animal is Dog { animal.breed; }   // narrowed
Dog? dog = animal as Dog;            // safe cast (returns optional)
Dog dog = animal as! Dog;            // forced cast (panics on failure)
```

## Ownership & Borrowing

```promise
// Three parameter modes (declared on the parameter):
read_only(string &s) { }     // shared borrow (read)
mutate(string ~s) { }        // mutable borrow (exclusive write)
consume(string s) { }        // takes ownership (moved)

// At call site — NO markers. Compiler infers borrow from declaration:
read_only(val);               // auto-borrows &val
mutate(val);                  // auto-borrows ~val
consume(val);                 // val is moved, cannot use after

// Copy types (auto-copy, no move): primitives, bool, char, pure value types
// Move types: string, collections, heap types — passed by move unless param is & or ~
```

## Resource Management

```promise
// use binding: auto-calls close()/drop() at scope exit
type Connection {
  int fd;
  close(~this)! { /* cleanup */ }
}
process()! {
  use conn := Connection(fd: 42);
  // conn.close() called automatically when conn goes out of scope
}

// drop: destructor called when owner goes out of scope
type Resource {
  drop(~this) { cleanup(); }
}

// File I/O: use read_content/write_content one-shot helpers, or open/close manually
use io;
main()! {
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

## Concurrency

```promise
// Fire-and-forget goroutine
go { expensive_work(); };

// Task with return value
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
```

## Modules

```promise
// Import a catalog module
use io;
use path;
use os;
use json;

// Import with alias
use json as j;

// Usage
string content = io.File.read_content("data.txt")!;    // panic on error
string dir = path.parent("/usr/local/bin");
os.set_environment_variable("KEY", "value");

// Available modules:
// io      — File, Dir, BufferedReader/Writer, read_line (stdin)
// path    — join, parent, file_name, extension, stem, normalize
// os      — env vars, working_directory, execute (variadic), exit_process
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

// Exclude from platforms
test_file_io() `test(exclude: "wasm32") {
  // ...
}
```

Run tests: `promise test file.pr` or `promise test tests/...` (recursive).

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

## Quick Patterns

```promise
// Read a file (failable main)
use io;
main()! {
  string content = io.File.read_content("input.txt");   // auto-propagates
  print_line(content);
}

// JSON encode/decode (requires `serializable annotation)
// Uses Encodable/Decodable structural interfaces
use json;
type Config `serializable { string name; int port; }
main()! {
  // Encode via JsonEncoder
  json.JsonEncoder enc = json.JsonEncoder();
  Config(name: "app", port: 8080).encode(enc)!;
  string s = enc.to_string();
  // Decode via JsonDecoder
  json.JsonDecoder dec = json.JsonDecoder(data: s);
  Config c = Config.decode(dec)!;
}

// Run a subprocess (variadic — inline args or pre-built string[])
use os;
main()! {
  r := os.execute("ls", "-la", "/tmp");
  print_line(r.standard_output);
}

// File I/O with manual open/close
use io;
main()! {
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

## Common Mistakes

1. **Using `!` to propagate** — `!` always panics. Use bare call for propagation in `!` functions.
2. **`&`/`~` at call site** — Don't write `func(&val)` or `func(~val)`. Just write `func(val)` — the compiler auto-borrows based on the parameter declaration.
3. **Forgetting named args** — constructors require `Type(field: value)`, not positional.
4. **Moving strings twice** — strings are not `Copy`. Use `&s` parameter to borrow, or read into separate variables.
5. **Getter parens** — `vec.len` not `vec.len()`. Getters use property syntax.
6. **Mutable methods** — use `~this` for methods that modify state. `&this` is read-only.
7. **Missing `use`** — `std` is auto-imported, but `io`, `os`, `path`, `json` need explicit `use`.
8. **Optional handler spacing** — Error handler: `expr ? e { ... }` (space before `?`). Optional handler: `expr? _ { ... }` (no space before `?`).
