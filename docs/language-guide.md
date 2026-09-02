# Promise Language Guide

> **Tag:** `language-guide` — remaining work to complete this document: `mcp__tracker__list --tag language-guide`

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

// Standard streams: stdin (Reader), stdout / stderr (Writer)
stderr.write_line("diagnostic — goes to fd 2, not stdout");
stdout.write_line("data — same as print_line, but as a Writer handle");
```

## Primitive Types

| Type | Description | Literals |
|------|-------------|----------|
| `int` / `i8` `i16` `i32` `i64` | Signed integers | `42`, `-1i8`, `0xFF`, `0b1010`, `1_000` |
| `uint` / `u8` `u16` `u32` `u64` | Unsigned integers | `42u`, `255u8`, `0o77u32` |
| `i128` `i256` `i512` / `u128` `u256` `u512` | Wide integers | `0xDEADBEEFu128`, `1_000u256`, `~0u512` |
| `f32` `f64` | Floats | `3.14`, `1.0f32` |
| `bool` | Boolean | `true`, `false` |
| `char` | Unicode character | `'a'`, `'\n'`, `'\u{1F600}'` |
| `string` | UTF-8 string | `"hello"`, `"val={x}"`, `r"raw\n"` |

Bare numeric literals infer as `int` or `f64`. Use suffixes for specific types.

### Wide integers (`i128`/`u128`, `i256`/`u256`, `i512`/`u512`)

Fixed-width integers wider than 64 bits — for cryptography, hashing, UUIDs,
network/IPv6 identifiers, and fixed-point currency. They are native primitives
(LLVM `iN`): pure value types with the full `i64`/`u64` operator set (arithmetic,
comparison, bitwise, shifts, ranges), automatically `` `copy ``, no heap
allocation, no `drop()`. A literal that exceeds `i64` range needs an explicit
wide suffix (`0x...u128`); unsuffixed literals still infer as `int`.

```promise
u128 id  = 0x550e8400e29b41d4a716446655440000u128;  // a UUID as one value
u256 big = 1u256 << 255u256;                          // 2^255
u64  high = (id >> 64u128) as! u64;                   // widths mix only via as!
```

- **Default base is 16.** `to_string()` and `parse()` default to hexadecimal —
  the natural form for hashes/UUIDs, and the only base whose formatting needs no
  division. Decimal is available via `to_string(base: 10)` / `parse(r, base: 10)`.
- **Conversions are explicit.** Mixing widths (or signedness) requires `as!`:
  widening sign-extends (signed) or zero-extends (unsigned); narrowing truncates.
- **Performance.** `add`/`sub`/`mul`/bitwise/shifts lower to inline word-sized
  sequences (carry-flag aware). **Division and remainder are the exception** —
  they lower to soft-arithmetic routines the compiler emits in-IR (tens to
  hundreds of cycles depending on width), so hoist them out of hot loops. Decimal `to_string`
  uses one division per digit; the default base-16 path is division-free.
- **Not constant-time.** LLVM's wide `div`/`mul` are not constant-time; crypto
  needing constant-time must hand-craft routines over `u64[]`/`u32[]`.
  Arbitrary-precision (beyond 512 bits), checked/wrapping/saturating variants, and
  wide-integer atomics are out of scope — see [large-integers.md](large-integers.md).

**String escapes:** `\n` `\t` `\r` `\b` `\\` `\"` `\0` `\{` (literal `{`, suppresses interpolation). Raw strings (`r"..."`) skip all escapes.

**Multi-line strings:** triple-quoted `"""..."""` spans multiple lines with everything preserved verbatim — like a raw string, neither escapes nor `{...}` interpolation are processed.

```promise
string banner = """line one
line two
line three""";
```

**String interpolation:** `{expr}` embeds any expression's value (its `to_string`). The interpolated expression is full-featured: it may contain string literals — including ones that hold `{`, `}`, or escaped quotes (`\"`) — call functions with multiple arguments, nest further interpolations to any depth, and use the explicit error-handling forms `?^` (propagate), `?!` (panic), and `? handler { … }` (handle). A literal `{` outside interpolation must be escaped as `\{`; a literal `}` needs no escape.

```promise
// nested string literals with braces, and a nested interpolation, all valid:
print_line("payload={encode("\{\"id\": {id}\}")}");
print_line("doubled={twice(n)?^}");   // ?^ propagates in a failable function
```

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
  area(this) f64 {
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
  current(this) int { return this.value; }

  // Mutable borrow (can modify)
  increment(~this) { this.value += 1; }

  // Expression body shorthand
  is_zero(this) bool => this.value == 0;

  // Getter (accessed as .count, no parens)
  get count int => this.value;

  // Factory method (static, called as Counter.zero())
  zero() Self `factory { return Self(value: 0); }
}

c := Counter(value: 0);
c.increment();            // ~this: compiler auto-borrows mutably
print_line("{c.count}");  // getter, no parens
```

**Operator overloading** — name a method after the operator to make a type usable with it:

```promise
type Vec2 {
  int x `value;
  int y `value;

  +(Vec2 other) Vec2 => Vec2(x: this.x + other.x, y: this.y + other.y);
  -(Vec2 other) Vec2 => Vec2(x: this.x - other.x, y: this.y - other.y);
  ==(Vec2 other) bool => this.x == other.x && this.y == other.y;
}

Vec2 sum = Vec2(x: 1, y: 2) + Vec2(x: 3, y: 4);   // (4, 6)
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
    print_line("Error: {e.message}");
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

**Auto-propagation does not cross a lambda boundary.** A bare call (or `?^`) inside a lambda body propagates to the *lambda*, not the enclosing function — and a lambda is not itself failable, so this is a compile error. Inside a lambda, handle the error locally with `?!` (panic) or `? { ... }` (recover):

```promise
process!(string path) string {
  // WRONG: read_config inside the lambda can't auto-propagate to process!
  // load := |string p| -> read_config(p);

  // RIGHT: handle the error inside the lambda
  load := |string p| -> string { return read_config(p) ? { "default" }; };
  return load(path);
}
```

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

// Optional handler (whitespace around ? is insignificant)
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

// Match guards — bind the value, then refine an arm with `if <cond>`:
match n {
  val if val > 0 => "positive",
  val if val < 0 => "negative",
  _ => "zero",
}

// Type checks and casts
if animal is Dog { animal.breed; }   // narrowed
Dog? dog = animal as Dog;            // safe cast (returns optional)
Dog dog = animal as! Dog;            // forced cast (panics on failure)

// Non-destructive variant narrowing: `is Variant` exposes the variant's NAMED
// payload members directly (a borrow — the value stays intact). Same narrowing
// as the class-subtype case above.
if shape is Circle {
  print_line("radius: {shape.radius}");  // shape narrowed to the Circle variant
}

// Destructure is-patterns (enum variants and named types) — bind fields to
// fresh names. Required for variants with positional (unnamed) payloads.
if shape is Circle(r) {
  print_line("radius: {r}");        // r is bound from Circle's field
}
if opt is Some(val) {
  print_line("value: {val}");       // works with generic enums
}
if animal is Dog(name, breed) {
  print_line("{name} is a {breed}");  // named type field extraction
}

// match dispatches type-patterns on the runtime subtype (RTTI), binding the
// narrowed subtype to a name:
string label = match shape {
  Circle c => "circle r={c.r}",
  Square s => "square s={s.s}",
  _ => "other",
};
```

## Ownership & Borrowing

```promise
// Three parameter modes (the shared borrow is the unmarked default):
read_only(string s) { }       // shared borrow (read-only) — caller keeps ownership
mutate(string~ s) { }         // mutable borrow (~) — callee may modify; caller keeps ownership
consume(string move s) { }    // move — callee takes ownership; caller can't use s after

// At call site: borrows take NO marker; consuming a named binding takes `move`:
read_only(val);               // shared borrow — val still valid
mutate(val);                  // mutable borrow — val still valid (possibly changed)
consume(move val);            // move — val is gone after this line

// Copy types (auto-copy, no move): primitives, bool, char, pure value types
// Move types: string, collections, heap types — a plain `T s` param is a borrow;
// use `T move s` to consume. A borrow parameter cannot be moved out (e.g., into a
// struct field) — that's a compile-time error.

// No mutation through a shared borrow: a read-only borrow may not mutate the value
// it points at — not a field store, not a property setter, not an index/slice write,
// not a `~this` mutating method (v.push(x), s.add(x)). Use a `~` mutable borrow.
add_one(int[] v)  { v.push(1); }   // error: cannot mutate through a shared borrow
add_one(int[]~ v) { v.push(1); }   // OK: mutable borrow
// Exception: `interior types (Channel/Mutex) allow interior mutation through a
// shared borrow — their internal synchronization makes it safe.

// Self-aliasing: a `~this` (mutable-receiver) method may not take a plain/shared
// borrow of one of the SAME receiver's fields in the same call — the receiver's
// mutable borrow overlaps the field's shared borrow:
//   o.mutate(o.field);  // error: cannot borrow 'o' as mutable — already borrowed
// The method could reassign/drop o.field while the borrow is live. Pass a distinct
// value or a clone (o.field.clone()) instead. Borrow tracking keys on the root
// variable (`o`), not the field: passing ANY field of a non-Copy receiver triggers
// this — including a Copy-typed field (a conservative over-approximation, since a
// Copy field is actually passed by value with no aliasing hazard). The rule lifts
// only when the whole receiver is a Copy/value type, so the call copies rather than
// borrows.

// Reference TYPES (locals and return types; default is owned):
// (borrows can't be stored in fields — use Ref[T] to hold a reference in a struct)
//   string  s = ref.borrow;         // error: can't store a borrow as an owned string
//   string  s = ref.borrow.clone(); // OK: explicit owned copy
//   string& s = ref.borrow;         // OK: kept as a shared reference
//   int     n = ref.borrow;         // OK: int is Copy
// Implicit `T& → T` / `T~ → T` decay only happens for Copy types.
```

## Resource Management

```promise
// use binding: auto-calls close() at scope exit (drop() is suppressed — §16.4)
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
  io.File f = io.File.open_read("data.txt");
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

**Satisfying the std structural interfaces** — a user type participates in generic algorithms (`sort`, `map`/`Set` keys, `clone`) by defining the right methods; no `is` declaration is needed (they are `` `structural ``):

| Interface | Required member | Provided for free |
|-----------|-----------------|-------------------|
| `Equal` | `==(Self other) bool` | `!=` |
| `Ordered` (`is Equal`) | `<(Self other) bool` (plus `==`) | `>`, `<=`, `>=` |
| `Hashable` | `get hash int` (a **getter**, not `hash()`) | — |
| `Cloneable` | `clone() Self` | — |

```promise
type Money {
  int cents;

  ==(Money other) bool => this.cents == other.cents;
  <(Money other) bool  => this.cents < other.cents;   // now Ordered → sortable
  get hash int => this.cents;                          // now a valid map/Set key
}

Money[] xs = [Money(cents: 30), Money(cents: 10)];
xs = sort(move xs);             // works because Money is Ordered (sort consumes xs)
Set[Money] seen = Set[Money]();
seen.add(Money(cents: 10));     // works because Money is Hashable + Equal
```

## Collections (auto-imported from std)

```promise
// Vector (T[])
int[] nums = [1, 2, 3];
int[] empty_vec = [];     // empty-vector literal
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
map[string, int] empty_map = {:};   // empty-map literal — note the colon; `{}` is rejected
scores["charlie"] = 92;
if v := scores["alice"] { print_line("{v}"); }   // subscript returns V?
scores.contains("key");   // bool
scores.remove("key");     // bool
scores.keys();             // K[]
scores.values();           // V[]
for k, v in scores { }     // iterate entries

// Set (Set[T] — T must be Hashable + Equal) — no literal form; construct with Set[T]()
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

## Function Types

```promise
(int, int) -> int f = add;      // takes two ints, returns int
(string&) -> bool p = check;    // borrowed parameter
() -> void a = setup;           // takes nothing, returns nothing
(int) -> void cb = log_value;   // callback returning nothing

// `void` is the only spelling for an empty return — there is no `()` type.
// A function declared with no return type has the same type as one declared
// `void`, so both satisfy `() -> void`.

// Failable function types prefix the `!` producer marker onto the whole type.
// Failability is part of the type — a plain function is not assignable to a
// failable one, or the reverse.
double_or_raise!(int x) int { ... }

!(int) -> int g = double_or_raise;
int r = g(3)?^;                 // handle at the call site: ?^ / ?! / ? e { ... }

// Function types erase parameter names — call through them positionally only.
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
  print_line("Error: {err.message}");
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
t := go fetch_data(url.clone());  // the goroutine must own what it touches (§17.4)
string result = <-t;           // blocks until done

// Failable goroutine: spawn a fallible producer with `go!` → failable_task[T].
// The error surfaces at the receive, which is itself a failable operation.
t := go! fetch_user(42);       // fetch_user!() — t : failable_task[User]
user := <-t;                   // in a failable fn: auto-propagates the error
// in a non-failable fn, handle it at the receive:
user := (<-t)?!;               // panic on the goroutine's error
user := (<-t) ? e { recover(e); };
// `go` on a failable call is rejected (use `go!`); `go!` on a non-failable call
// is rejected (use `go`); discarding a `go! …` is rejected (fire-and-forget must
// be non-failable — handle the error inside a plain `go { f()?!; }`).

// A failable_task[T] is LINEAR (must-use): its error must reach exactly one
// receiver, so it must be received (`<-t`), or moved onward (into a field,
// collection, argument, or `return`), before its owner's scope ends — letting one
// go out of scope undischarged is a compile error. Must-use is transitive: a type
// or collection that owns one (`Holder{ failable_task[T] t }`, `failable_task[T][]`)
// is itself must-use.
handles := build_handles(ids);   // handles : failable_task[User][]
users := <-handles;              // drain: awaits all, consumes handles, yields User[]
// `<-tasks` (drain) is a failable operation: it succeeds only if every task
// succeeded, else fails with the FIRST error by index (remaining errors are
// discharged by the drain, not swallowed).

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

// Sharing a value: borrow for momentary access; Ref[T] to keep or share it.
// Ref[T] is reference-counted shared ownership — clone() to share, drop frees
// the inner value when the last reference goes away.
config := Ref[Config](load_config());
go { use(config.borrow); };      // the goroutine gets its own reference — crossing the
                                 // boundary duplicates the handle (refcount bumped at
                                 // the spawn); `config` stays usable here

// Atomicity of the Ref counter is a compiler decision, not part of the type:
// a Ref that never crosses a go/channel/Task boundary uses a plain counter; one
// that might be shared across goroutines uses an atomic counter. To force the
// fast non-atomic counter, mark the element type `confined — the compiler then
// REJECTS that Ref at any go/channel/Task boundary, which keeps it sound.
type LocalState `confined { int count; }
s := Ref[LocalState](LocalState(count: 0));   // non-atomic counter
// go { s.borrow.count; };  // error: confined Ref can't cross a goroutine
```

> **Common mistake:** trying to store a borrow (`T&`/`T~`) in a struct field.
> Borrows live only in parameters, locals, and return values (stack-bounded).
> To hold or share a reference inside a struct, use `Ref[T]`.

## Modules

**`std` is auto-imported** under `use std as _` (anonymous), which injects its names at the top level — that's why `print_line`, `Builder`, `Vector`, `Map`, `int`, `assert`, etc. work with no prefix and no `use`.

**Catalog modules** require `use`, in one of two forms:
- **Named** (`use io;`) — **both types and functions** are accessed through the module prefix (`io.File`, `io.read_line()`). There is no special unprefixed-function path; naked `File`/`read_line()` is an error.
- **Anonymous** (`use path as _;`) — injects the module's names at the top level, usable with no prefix (exactly how `std` is imported). Name conflicts fall back to the prefix.

**Imports are per-file.** A `use` binds its alias only in the file that declares it — never in the other files of the same module. Each file declares what it uses, so the same `use json;` normally appears in every file that references `json.`, and reading one file tells you where all of its names come from. Declaring an import a file never uses is a warning.

```promise
// Import a catalog module
use io;
use path;
use os;
use json;

// Import with alias
use json as j;

// Named import — types AND functions go through the module prefix
io.File f = io.File.open_read("data.txt");
io.Dir.make("/tmp/out");                 // not Dir.make(...)
string p = path.join("/usr", "bin");     // functions need the prefix too — join(...) alone is an error
json.JsonValue v = json.parse_value(data);

// Anonymous import injects names at the top level (this is how std works):
//   use path as _;  →  join("/usr", "bin")  with no prefix; on a conflict use path.join(...)

// Available modules (use `promise doc <module>` to explore):
// io      — File, Dir, BufferedReader/Writer, read_line (stdin)
// path    — join, parent, file_name, extension, stem, normalize
// os      — env vars, working_dir, execute(program, args[], env?, dir?), exit_process
// json    — encode_string, decode_string (generic, via Encodable/Decodable)
// math    — lerp, map_range, gcd, lcm, sign, is_even, is_odd
// strings — join, spaces, reverse, is_blank, repeat_join
```

## Tests

```promise
// Test function (discovered by `promise test`)
// assert(condition, message) — the message argument is REQUIRED
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

**Output control:** passing tests are not printed. On a terminal the most recent
one is rewritten in place on a single line, so the scrollback holds exactly the
failures followed by the summary; through a pipe or into a file nothing is
printed for them at all. `-progress full` restores a line per test,
`-progress plain`/`tty` force either rendering, and `PROMISE_PROGRESS` sets the
same values from the environment. The summary is identical in every mode.

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

// yield* — delegate to another iterable (generator, range, array, or string),
// re-yielding all of its elements in order:
count_to(int n) stream[int] {
  for i in 1..=n { yield i; }
}

combined() stream[int] {
  yield * count_to(3);   // 1, 2, 3
  yield * 4..=6;         // 4, 5, 6 (delegate to a range)
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

// Repeat literal: [value; count] — bulk-initialize a sized array
u32[64] schedule = [0u32; 64];   // 64 copies of 0u32
u8[256] table    = [7u8; 256];   // 256 copies of 7u8
u8[4][3] grid    = [[0u8; 4]; 3]; // nesting: u8[4][3]
```

Fixed-size arrays are value types (stack-allocated, auto-copied). Distinct from `T[]` (heap-allocated vector).

The **repeat literal** `[value; count]` builds a sized array of `count` copies of `value`. Restrictions: `count` must be a compile-time constant integer literal, and the element type must be `` `copy `` — `value` is evaluated **once** and copied, so a non-`` `copy `` element (e.g. a `string`) is a compile error rather than `count` aliased handles. The `;` is what distinguishes it from a list: `[0u32]` is a one-element array, `[0u32, 0u32]` a two-element array, `[0u32; 64]` a 64-element array.

## Explicit Constructors

```promise
// new replaces the implicit constructor
type Percentage {
  int value `final;

  new(~this, int value) {
    if value < 0 { this.value = 0; }
    else if value > 100 { this.value = 100; }
    else { this.value = value; }
  }
}
Percentage(value: 120);   // clamped to 100

// Failable new
type Port {
  int value `final;

  new!(~this, int value) {
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

// Constructor inheritance — a child's new() calls the parent's new() via super()
type Base {
  int value;
  new(~this, int value) { this.value = value; }
}
type Child is Base {
  new(~this, int v) {
    super(value: v);   // runs Base.new before the rest of Child.new
  }
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

// Run a subprocess: execute(program, args[], env?, working_dir?) — failable
use os;
main!() {
  r := os.execute("ls", ["-la", "/tmp"]);   // env/dir optional; failable — auto-propagates in main!()
  print_line(r.stdout);
}

// File I/O with manual open/close
use io;
main!() {
  io.File f = io.File.open_read("/tmp/data.txt");
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
promise doc .                # documentation for the project in the current dir
promise doc <path>           # documentation for a local project directory
promise                      # concise grouped command index
promise help                 # quick start guide with examples
promise help <cmd>...         # help for any command/subcommand (≡ promise <cmd>... --help)
promise doc                  # list all available modules
promise targets              # list supported compile targets
```

Every command and subcommand accepts `-h`/`-help`/`--help` and prints its help
to stdout (exit 0); `promise help <path...>` routes to any node in the tree
(e.g. `promise help package add`). Usage errors go to stderr with a short
pointer to `--help`.

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
2. **Call-site markers** — borrows take no marker (`func(val)`); the only call-site marker is `move`, required when passing a named binding to a consuming (`move`) parameter: `consume(move val)`.
3. **Named arg before positional** — once an argument is named, every later argument must be named too: `f(x: 1, 2)` is an error, but `f(1, y: 2)` is fine. Constructors, functions, and methods all use the same positional-then-named rule.
4. **Moving strings twice** — strings are not `Copy`. Use a plain `string s` parameter (the unmarked default) to borrow, or `.clone()` for an independent owned copy.
5. **Getter parens** — `vec.len` not `vec.len()`. Getters use property syntax.
6. **Mutable methods** — use `~this` for methods that modify state; bare `this` is read-only.
7. **Missing `use`** — `std` is auto-imported, but `io`, `os`, `path`, `json` need explicit `use` — **in every file that uses them**. Imports are per-file: a sibling file's `use json;` does not carry over, and repeating it is correct, not a redeclaration.
8. **`?` handler spacing is insignificant** — Whitespace around the `?` handler is optional: `expr?{...}`, `expr ?{...}`, `expr? {...}`, and `expr ? {...}` all parse identically (same for the binding forms `expr?e{...}` / `expr? e {...}`). This applies to both the error handler (on failable `!` values) and the optional handler (on `T?` values). Note the companion operators `?^` and `?!` are single tokens — no space is allowed between `?` and `^`/`!`.
9. **Fixed arrays vs vectors** — `int[3]` is a fixed-size array (value type, stack). `u8[]` is a vector (heap, growable). Don't confuse them. The repeat literal `[x; n]` is **sized-array-only** (yields `T[n]`, never a vector); to fill a vector use `u8[].filled(x, n)`. Note `u32[64].filled(...)` does **not** parse (a type in expression position reads as indexing) — use `[0u32; 64]` for a sized array.
10. **Tuple destructuring** — Use `(a, b) := expr;` not `a, b := expr;`. Tuples need parentheses.
11. **Naked catalog names** — under a named `use io;`, both types *and* functions need the `io.` prefix (`io.File`, `io.read_line()`); naked `File`/`read_line()` is an error. To use names without a prefix, import anonymously: `use io as _;`.
12. **`;` in blocks** — All statements need `;`. The trailing `;` on the last expression before `}` is optional: both `? { "default" }` and `? { "default"; }` work.
13. **`?!` in failable functions** — `?!` always **panics**, even inside a `!` function. For propagation, just use bare call (or `?^` for self-documenting propagation).
14. **API discovery** — Use `promise doc <module>` to explore module APIs instead of guessing. `promise doc std` for the standard library.
15. **`{}` is not an empty map** — use `{:}` (empty map — note the colon), `[]` (empty vector), or `Set[T]()` (sets have no literal form). A bare `{}` in value position is rejected with a guiding error.
16. **Mixing `` `value `` and instance fields** — a type's fields must be *all* `` `value `` (a pure value type) or *all* instance; hybrid value+instance types aren't supported yet. This includes inherited fields: a child of a pure value type is itself a value type (a layout-preserving newtype) and may add **methods only** — any field it declares, `` `value `` or not, is rejected. The methods it adds are new ones: value types dispatch statically, so redeclaring an inherited method with the same signature (an override) is rejected, as is any `` `abstract `` method on a value type. Such a child is a distinct compile-time type but carries no runtime identity, so `is`/`as` between two value types is rejected too.
