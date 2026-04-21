# Promise Programming Language — Design Proposal

## 1. Overview

**Promise** is a systems-aware, statically-typed programming language with Dart-inspired syntax, Rust-inspired ownership semantics, and a rich type system featuring inheritance, generics, and algebraic error handling. The toolchain (compiler + package manager) is a single Go binary called `promise`, uses ANTLR4 for parsing, and targets LLVM IR for code generation.

---

## 2. Toolchain Architecture

```
Source (.pr)
    │
    ▼
┌──────────────┐
│  ANTLR4 Lexer│   ← Generated from Promise.g4 grammar
│  & Parser    │
└──────┬───────┘
       │  Parse Tree
       ▼
┌──────────────┐
│   AST Builder│   ← Go: converts ANTLR parse tree → Promise AST
└──────┬───────┘
       │  AST
       ▼
┌──────────────┐
│  Semantic    │   ← Type checking, ownership analysis,
│  Analysis    │     generic monomorphization, meta resolution
└──────┬───────┘
       │  Typed AST
       ▼
┌──────────────┐
│  LLVM IR Gen │   ← Go bindings to LLVM (via llir/llvm or CGo)
└──────┬───────┘
       │  LLVM IR
       ▼
┌──────────────┐
│  LLVM Backend│   ← Optimization passes, native code emission
└──────────────┘
       │
       ▼
   Binary / Library
```

The `promise` binary serves as both compiler and package manager:

```
promise build                # Compile the project
promise run                  # Build and run
promise test                 # Run tests
promise init                 # Create new promise.mod
promise add <url>            # Add dependency
promise remove <url>         # Remove dependency
promise update               # Update dependencies
promise fmt                  # Format source code
promise lock                 # Regenerate lockfile
```

---

## 3. File Extensions & Project Layout

```
myproject/
├── promise.mod              # Module definition file
├── promise.lock             # Lockfile (generated)
├── src/
│   ├── main.pr              # Entry point
│   ├── models/
│   │   ├── promise.mod      # Sub-module
│   │   ├── user.pr
│   │   ├── user_test.pr     # Test file alongside source
│   │   └── account.pr
│   └── utils/
│       ├── promise.mod
│       └── strings.pr
```

### 3.1 Testing Convention

Tests live alongside the code they test. Any function annotated with `` `test `` is a test function. There are two approaches:

1. **Inline tests** — write `` `test `` functions directly in the production file:

```promise
// user.pr
type User {
  String name;
  Int age;
}

testUserCreation() `test {
  User u = User(name: "Alice", age: 30);
  assert(u.name == "Alice");
}
```

2. **Separate test files** — create a `<name>_test.pr` file next to the source file (e.g., `user_test.pr` alongside `user.pr`). Test files follow the same convention — all `` `test ``-annotated functions are collected by `promise test`.

The `promise test` command discovers and runs all `` `test ``-annotated functions across the project. Test-annotated functions are excluded from production builds.

---

## 4. Module System

### 4.1 Module Identity

A module is identified by a URL where every path segment is a valid identifier, except segments that are purely numeric, which denote a **version**.

```
github.com/acme/collections/2     → module "collections", version 2
github.com/acme/collections/2/3   → module "collections", version 2.3
github.com/std/io/1               → module "io", version 1
```

Version segments are parsed left-to-right as `major`, `minor`, `patch`.

### 4.2 promise.mod File

The module file declares only the module's own identity:

```
module github.com/acme/myapp/1
```

There is no `require` block. Dependencies are inferred from `use` declarations in source files. This keeps the module file minimal and avoids duplication between the module file and source code.

### 4.3 `use` Declarations

At the top of any `.pr` file, `use` imports a module with a local alias. The URL is a **string literal** and is the single source of truth for that dependency:

```promise
use io "github.com/std/io/1"
use col "github.com/acme/collections/2/1"

main() {
  io.println("hello")
  col.List[Int] list = col.List[Int]()
}
```

The identifier before the URL is **mandatory** and is the only way to reference that module's exports in the file.

**Design rationale:** Each source file is self-contained — you can read a `.pr` file and understand every reference without consulting other files. The full URL next to the alias makes it immediately clear what `io.println` or `col.List` refers to. This optimizes for reading and understanding code locally, which is the most common operation.

### 4.4 Visibility

All declarations are **public** by default. Use `` `public `` meta annotation to explicitly mark something as public (reserved for future use when private-by-default is introduced).

For now, everything is public. In a future revision, declarations will be **private by default**, and `` `public `` will be required to export them.

---

## 5. Type System

### 5.1 Primitive Types — Defined as Regular Types

Promise does **not** have a separate namespace for primitive types. Instead, primitives are defined as regular `type` declarations that contain a `` `raw `` field mapping directly to an LLVM type. This means `Int`, `Float64`, `Bool`, etc. are all types in the standard library, not compiler magic.

#### Raw Fields

A field annotated with `` `raw `` uses an LLVM type identifier directly as its type instead of a Promise type. LLVM type identifiers (`i1`, `i8`, `i16`, `i32`, `i64`, `float`, `double`, etc.) are valid identifiers in Promise and can appear as the type of a `` `raw `` field.

`` `raw `` is only supported on field definitions. Everywhere else — variable declarations, function parameters, return types — you must use a Promise-defined type. This keeps the type system simple: only field definitions need special handling for raw values.

#### Standard Library Primitive Definitions

```promise
type Int {
  i64 `raw `value;
}

type Int8 {
  i8 `raw `value;
}

type Int16 {
  i16 `raw `value;
}

type Int32 {
  i32 `raw `value;
}

type Int64 {
  i64 `raw `value;
}

type UInt {
  i64 `raw `value;
}

type UInt8 {
  i8 `raw `value;
}

type UInt16 {
  i16 `raw `value;
}

type UInt32 {
  i32 `raw `value;
}

type UInt64 {
  i64 `raw `value;
}

type Float32 {
  float `raw `value;
}

type Float64 {
  double `raw `value;
}

type Bool {
  i1 `raw `value;
}
```

The `` `raw `` meta marks a field as mapping directly to its LLVM IR type. The `` `value `` meta places the field in the Value struct (see Section 5.2). These are independent — `i64 `raw;` is valid and places the raw field in the Instance struct (the default). This unifies the type system — there is no distinction between "primitive" and "user-defined" types.

### 5.2 The Four-Struct Model

Every type declaration `T` produces four LLVM structs at compile time. These structs form a chain: **Value → Instance → Variant → Type**.

#### Allocation Model

- **Value struct** — contains only explicitly `` `value ``-annotated fields. Allocated on the stack, embedded in other value/instance structs, or passed as function parameters and return values. Always copied on assignment (value semantics).
- **Instance struct** — always heap-allocated. This is the standard "object" representation.
- **Variant struct** — generated at compile time. One per unique monomorphization. Never dynamically allocated.
- **Type struct** — generated at compile time. One per `type` declaration. Never dynamically allocated.

#### 1. **Value Struct** (`T#v`)
- Contains **only** fields explicitly annotated with `` `value ``. Unannotated fields go to the Instance struct.
- Fields in the value struct can be raw LLVM types (`` `raw `value ``) or Promise types (`` `value ``). When a Promise type is placed in the value struct, its own value struct is embedded (concatenated) inline — no pointer indirection.
- Has a **pointer to the Instance struct** that owns it (may be optimized out later).
- **Always copied** on assignment (value semantics).
- For types with ownership fields, a copy performs a deep clone (or is disallowed if the type is not `Clone`).

#### 2. **Instance Struct** (`T#i`)
- Contains the instance fields (unannotated / default) and a single **pointer to its Variant**.
- Always heap-allocated. This is the default representation when you use a type — the standard "object".
- Does **not** contain a pointer to its Value — values reference the instance, not the other way around.
- Subject to ownership rules.

#### 3. **Variant Struct** (`T#m`)
- Represents **one concrete monomorphization** of a generic type (all generic parameters resolved).
- Contains the vtable, method pointers, resolved generic type info, and a **pointer to the Type struct**.
- Shared across all instances of `T[ConcreteG1, ConcreteG2]`.
- Generated once per unique set of type arguments at compile time.
- Fields annotated with `` `variant `` live here.

#### 4. **Type Struct** (`T#t`)
- Matches the source-code type **declaration** 1:1.
- Contains the unresolved/generic metadata: name, generic parameter descriptors, inheritance chain, field layout info, meta annotations.
- Used for reflection and compile-time meta-programming.
- One per `type` declaration in the source code.
- Fields annotated with `` `type `` live here.

#### The Pointer Chain

```
T#v  ──ptr──▶  T#i  ──ptr──▶  T#m  ──ptr──▶  T#t
```

A value always points to its owning instance. The instance points to its variant. The variant points to its type. Given a value, you can reach all four structs by following pointers. The chain is one-directional — instance does not point back to value.

#### Diagram

```
┌──────────────────────────────────────────────────────────────────┐
│  T#t  (1 per source declaration, compile-time generated)         │
│  - name: "List"                                                  │
│  - generic_params: [E]                                           │
│  - fields: [...]                                                 │
│  - meta: [...]                                                   │
│  - parent_type: nullable                                         │
│  - `type fields live here                                        │
│                                                                  │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │ T#m[Int]  (1 per monomorphization, compile-time gen)       │  │
│  │ - resolved_params: {E: Int}                                │  │
│  │ - vtable: [method_ptrs...]                                 │  │
│  │ - type_ptr: → T#t ◀───────────────────────────────────── │  │
│  │ - `variant fields live here                                │  │
│  └────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────┐
│ T#i (1 per live object, heap-allocated)                       │
│ - variant_ptr: → T#m[Int]                                     │
│ - default (unannotated) instance fields live here             │
└──────────────────────────────────────────────────────────────┘
        ▲
        │ instance_ptr
┌──────────────────────────────────────────────────────────────┐
│ T#v  (stack-allocated, copied on assignment)                  │
│ - instance_ptr: → T#i                                         │
│ - `value fields live here                                     │
│ - items: [1, 2, 3]                                            │
│ - count: 3                                                    │
└──────────────────────────────────────────────────────────────┘
```

#### Field Placement Annotations

When defining a field in a type, the field goes into the **Instance struct** by default. Use meta annotations to place it elsewhere:

```promise
type Player {
  // Instance fields (default — no annotation needed)
  String name;
  Int health;

  // Value field — lives in T#v, copied with the value struct
  Float64 x `value;
  Float64 y `value;

  // Variant field — shared across all instances of this monomorphization
  String spritePath `variant;

  // Type field — shared across all instances of this type declaration
  String typeName `type;
}
```

#### Primitives in the Four-Struct Model

Since primitives are regular types, `Int` works like:

```promise
type Int {
  i64 `raw `value;    // The actual i64 value lives in Int#v
}
```

Resulting LLVM structs:
- `Int#v` = `{ i64, Int#i* }` — the raw i64 plus a pointer to the owning instance
- `Int#i` = `{ Int#m* }` — pointer to variant (no pointer back to value)
- `Int#m` = `{ vtable, Int#t* }` — method dispatch + pointer to type (compile-time generated)
- `Int#t` = `{ metadata }` — name, reflection info (compile-time generated)

For performance, the compiler will optimize away unnecessary indirection for primitives (e.g., `Int` on the stack is just an `i64` in practice, with the instance/variant/type pointers elided when not needed).

### 5.3 Variable Declarations

Variable declarations use **type-first** syntax (Dart/C++ style):

```promise
Int x = 42;
String name = "Alice";
Float64 pi = 3.14159;
List[Int] numbers = [1, 2, 3];
Map[String, Int] scores = {"alice": 100, "bob": 85};
```

Type inference with `var`:

```promise
var x = 42;             // inferred as Int
var name = "Alice";     // inferred as String
```

### 5.4 Inheritance

Single inheritance with `extends`. A type can implement multiple **interfaces** with `impl`.

```promise
type Shape {
  Float64 x;
  Float64 y;

  area() Float64;
}

type Circle extends Shape {
  Float64 radius;

  area() Float64 `instance {
    return 3.14159 * this.radius * this.radius;
  }
}

interface Drawable {
  draw(Canvas &canvas);
}

type Circle extends Shape impl Drawable {
  Float64 radius;

  area() Float64 `instance { ... }

  draw(Canvas &canvas) `instance {
    canvas.drawEllipse(this.x, this.y, this.radius);
  }
}
```

### 5.5 Generics

Generics use **square brackets** `[]`. Constraints are expressed with `where` clauses or inline bounds.

```promise
type Map[K: Hashable + Eq, V] {
  Bucket[K, V][] buckets;

  get(K &key) ?V& `instance { ... }
  set(K key, V value) `instance { ... }
}

// Alternative where-clause syntax:
sort[T](T[] &mut list) where T: Ord {
  ...
}
```

### 5.6 Enums (Algebraic Data Types)

```promise
enum Option[T] {
  Some(T),
  None,
}

enum Result[T, E] {
  Ok(T),
  Err(E),
}

enum Color {
  Red,
  Green,
  Blue,
  Custom(UInt8 r, UInt8 g, UInt8 b),
}
```

---

## 6. Ownership & Memory Management

Promise uses Rust-style ownership with borrowing and lifetimes.

### 6.1 Core Rules

1. Every value has exactly **one owner**.
2. When the owner goes out of scope, the value is **dropped** (destructor called, memory freed).
3. You can have **either** one mutable reference (`&mut T`) **or** any number of shared references (`&T`) — never both simultaneously.
4. References must not outlive their referent.

### 6.2 Syntax

```promise
process(String &data) {              // shared borrow
  io.println(data);
}

modify(String &mut data) {           // mutable borrow
  data.append(" world");
}

consume(String data) {               // takes ownership
  // data is dropped at end of scope
}

main() {
  String s = String("hello");
  process(&s);          // borrow
  modify(&mut s);       // mutable borrow
  consume(s);           // move — s is no longer valid after this line
}
```

### 6.3 Lifetimes

Explicit lifetimes when the compiler cannot infer:

```promise
longest['a](String &'a a, String &'a b) String &'a {
  if a.len() > b.len() { return a; }
  return b;
}
```

### 6.4 Clone and Copy Traits

```promise
interface Copy {}      // Marker — bitwise copy is safe (primitives, small value types)
interface Clone {
  clone() Self;
}
```

Types that are `Copy` are implicitly copied on assignment. Others are moved.

---

## 7. Error Handling

### 7.1 The `!` Return Convention

Functions that can fail use `!` after the return type. Under the hood, this desugars to a result struct — a pair of `(value, error)`.

```promise
readFile(String &path) String! {
  // On success:
  return contents;

  // On failure:
  raise io.FileNotFoundError(path);
}
```

The `!` suffix on the return type means: "this function returns `(String, Error)`".

### 7.2 Calling Failable Functions

```promise
main() {
  // Option 1: Propagate with `?`
  String content = readFile("data.txt")?;   // returns error to caller if failed

  // Option 2: Handle explicitly with `catch`
  String content = readFile("data.txt") catch err {
    io.println("Failed: {err}");
    return;
  };

  // Option 3: Unwrap (panics on error — for prototyping only)
  String content = readFile("data.txt")!;
}
```

### 7.3 Error Types

```promise
interface Error {
  message() String;
}

type FileNotFoundError {
  String path;
}

impl Error for FileNotFoundError {
  message() String {
    return "file not found: {this.path}";
  }
}
```

### 7.4 `raise` Statement

`raise` is used to return an error from a `!`-function. It is **not** an exception — it is sugar for returning the error half of the result struct.

```promise
divide(Float64 a, Float64 b) Float64! {
  if b == 0.0 {
    raise MathError("division by zero");
  }
  return a / b;
}
```

---

## 8. Meta Annotations (Backtick Attributes)

Backtick `` ` `` provides metadata on declarations. Meta annotations are placed **after** the definition name (and any parameters/return type) and **before** the body or statement terminator.

### 8.1 Syntax

```
MetaAnnotation = '`' Identifier [ '(' MetaParams ')' ] ;
MetaParams     = MetaParam ( ',' MetaParam )* ;
MetaParam      = Expression | Identifier ':' Expression ;
```

Meta annotations appear in post-definition position:

- **Types**: `type Foo `meta { ... }`
- **Fields**: `String name `meta;`
- **Methods**: `greet() String `meta { ... }`
- **Functions**: `add(Int a, Int b) Int `meta { ... }`

### 8.2 Examples

```promise
type OldThing `serializable `version(2) `deprecated(since: "1.3", message: "Use newMethod instead") {
  String name `json(name: "user_name") `required;
  Int age `json(name: "user_age");
}

fastAdd(Int a, Int b) Int `inline {
  return a + b;
}

testAddition() `test {
  assert(fastAdd(1, 2) == 3);
}
```

### 8.3 Built-in Metas

| Meta          | Applies To     | Description                                      |
|---------------|----------------|--------------------------------------------------|
| `` `raw ``    | fields         | Field uses an LLVM type identifier directly      |
| `` `value ``  | fields, methods| Place field in Value struct; method receives value as `this` (default for methods) |
| `` `instance ``| methods       | Method receives pointer to Instance struct as `this` |
| `` `variant ``| fields, methods| Place field in Variant struct; method receives variant as `this` |
| `` `type ``   | fields, methods| Place field in Type struct; method is a namespaced function (no `this`) |
| `` `public `` | any decl       | Mark as exported (future: when private-by-default)|
| `` `inline `` | functions      | Hint to inline the function                      |
| `` `deprecated`` | any         | Mark as deprecated with optional message         |
| `` `test ``   | functions      | Mark as a test function                          |
| `` `serializable`` | types     | Auto-generate serialization code                 |
| `` `align(N)``| types, fields  | Memory alignment                                 |
| `` `packed `` | types          | Pack fields without padding                      |
| `` `extern(abi)``| functions   | Foreign function interface                       |
| `` `unsafe `` | functions/blocks| Mark as unsafe code                             |

User-defined metas are available through the type system at compile time for meta-programming and code generation.

---

## 9. Functions & Methods

Functions are declared without a keyword — the name, parameter list, and optional return type are sufficient to identify a function declaration. This follows the Dart/C++/Java convention.

### 9.1 Free Functions

```promise
greet(String &name) String {
  return "Hello, {name}!";
}
```

### 9.2 Methods

Methods are defined inside the type body and correspond to the four struct levels. The method's level determines what `this` refers to and which fields are accessible.

#### Value Methods (default)

By default, `this` is the **value struct**, copied when the method is called. Value methods can access `` `value `` fields only.

```promise
type Point {
  Float64 x `value;
  Float64 y `value;

  distanceTo(this, Point other) Float64 {
    var dx = this.x - other.x;
    var dy = this.y - other.y;
    return math.sqrt(dx * dx + dy * dy);
  }
}
```

#### Instance Methods (`` `instance ``)

Instance methods receive a **pointer to the instance struct**. They can access instance fields but **not** `` `value `` fields (compile error). Use `&this` for shared borrow, `&mut this` for mutable borrow.

```promise
type Counter {
  Int value;

  increment(&mut this) `instance {
    this.value += 1;
  }

  current(&this) Int `instance {
    return this.value;
  }
}
```

#### Variant Methods (`` `variant ``)

Variant methods receive the **variant struct**. One method copy exists per monomorphization. They can access `` `variant `` fields only.

```promise
type Collection[T] {
  String typeName `variant;

  describeType() String `variant {
    return "Collection of {this.typeName}";
  }
}
```

#### Type Methods (`` `type ``)

Type methods are effectively **namespaced functions**. No `this` is passed at call time — the type struct is static and known at compile time. They can access `` `type `` fields only. These replace the `static` keyword.

```promise
type Counter {
  Int value;

  new() Counter `type {
    return Counter(value: 0);
  }
}

// Called as:
Counter c = Counter.new();
```

### 9.4 Lambdas / Closures

```promise
var add = |Int a, Int b| -> Int { return a + b; };
var doubled = list.map(|x| x * 2);
```

Closures capture by reference by default. Use `move` to capture by value:

```promise
String greeting = "hello";
var closure = move |String name| -> String {
  return "{greeting}, {name}";
};
```

### 9.5 Function Types

Function types use arrow syntax instead of a keyword:

```promise
(Int, Int) -> Int                  // function taking two Ints, returning Int
(String&) -> Bool                  // function taking a borrowed String, returning Bool
() -> ()                           // function taking nothing, returning nothing
```

---

## 10. Control Flow

### 10.1 If / Else

```promise
if x > 0 {
  // ...
} else if x == 0 {
  // ...
} else {
  // ...
}

// If as expression
var abs = if x >= 0 { x } else { -x };
```

### 10.2 Match (Pattern Matching)

```promise
match color {
  Color.Red => io.println("red"),
  Color.Green => io.println("green"),
  Color.Custom(r, g, b) => io.println("rgb({r},{g},{b})"),
  _ => io.println("other"),
}

// Match as expression
var label = match status {
  200 => "OK",
  404 => "Not Found",
  _ => "Unknown",
};
```

### 10.3 Loops

```promise
// While
while condition {
  // ...
}

// For-in (iterators)
for item in collection {
  // ...
}

// For-in with index
for i, item in collection {
  // ...
}

// Infinite loop
loop {
  if done { break; }
}

// Classic for
for Int i = 0; i < 10; i += 1 {
  // ...
}
```

---

## 11. Strings & Interpolation

Strings are UTF-8 encoded, owned, and heap-allocated.

```promise
String name = "world";
String msg = "hello, {name}!";           // string interpolation with {}
String raw = r"no \n escape here";        // raw string
String multi = """
  multi-line
  string literal
""";
```

String slices (`&str` equivalent) use `String&` for borrowed string data.

---

## 12. Collections

Built-in collection types with generic support:

```promise
// Array (fixed-size, stack-allocated)
Int[3] arr = [1, 2, 3];

// Slice (dynamic, heap-allocated)
Int[] list = [1, 2, 3, 4, 5];
list.push(6);

// Map
Map[String, Int] scores = {
  "alice": 100,
  "bob": 85,
};

// Tuple
(Int, String) pair = (42, "answer");
var (num, label) = pair;   // destructuring
```

---

## 13. Nullable Types

Promise does not have null. Optional values use `Option[T]`, with sugar `?T`:

```promise
find(Int id) ?User {            // shorthand for Option[User]
  // ...
  return none;                   // Option.None
}

var user = find(42);
if user is Some(u) {
  io.println(u.name);
}

// Or with `?.` chaining
var name = find(42)?.name;
```

---

## 14. Unsafe Code

Promise allows unsafe blocks for low-level operations:

```promise
rawPointer() `unsafe {
  var ptr = unsafe {
    Int* raw = alloc[Int]();
    *raw = 42;
    raw
  };
}
```

---

## 15. Concurrency (Future Direction)

Lightweight coroutines with ownership-safe channels:

```promise
main() {
  Channel[Int] ch = Channel[Int].new(capacity: 10);

  spawn {
    for i in 0..100 {
      ch.send(i);
    }
    ch.close();
  }

  for value in ch {
    io.println("{value}");
  }
}
```

Ownership rules apply across spawned tasks — data is either moved into the task or shared via `Arc[T]` (atomic reference counting).

---

## 16. Complete Example

```promise
use io "github.com/promise-lang/std/io/1"
use json "github.com/promise-lang/std/json/1"

type Todo `serializable {
  Int id `json(name: "id");
  String title `json(name: "title");

  Bool done;

  toggle(&mut this) `instance {
    this.done = !this.done;
  }

  new(Int id, String title) Todo `type {
    return Todo(id: id, title: title, done: false);
  }
}

type TodoList {
  Todo[] items;

  add(&mut this, String title) `instance {
    Int id = this.items.len() + 1;
    this.items.push(Todo.new(id, title));
  }

  pending(&this) Todo&[] `instance {
    return this.items.filter(|t| !t.done);
  }
}

loadFromFile(String &path) TodoList! {
  String content = io.readFile(path)?;
  Todo[] items = json.decode[Todo[]](content)?;
  return TodoList(items: items);
}

main() {
  TodoList todos = loadFromFile("todos.json") catch err {
    io.println("Starting fresh: {err.message()}");
    TodoList(items: []);
  };

  todos.add("Design Promise language");
  todos.add("Build the compiler");

  for i, todo in todos.items {
    var status = if todo.done { "done" } else { "    " };
    io.println("[{status}] {todo.title}");
  }
}
```

---

## 17. Grammar Sketch (ANTLR4)

Key productions (simplified):

```antlr
grammar Promise;

compilationUnit: useDecl* declaration* EOF;

useDecl: 'use' IDENT STRING_LITERAL ';';

declaration
    : typeDecl
    | funcDecl
    | enumDecl
    | interfaceDecl
    ;

metaAnnotation: '`' IDENT ('(' metaParams ')')?;
metaParams: metaParam (',' metaParam)*;
metaParam: expression | IDENT ':' expression;

typeDecl
    : 'type' IDENT typeParams? ('extends' typeRef)? ('impl' typeRef (',' typeRef)*)?
      metaAnnotation* '{' typeMember* '}'
    ;

typeParams: '[' typeParam (',' typeParam)* ']';
typeParam: IDENT (':' typeConstraint)?;
typeConstraint: typeRef ('+' typeRef)*;

typeMember: fieldDecl | methodDecl;
fieldDecl: typeRef metaAnnotation* ';';
methodDecl: IDENT '(' params ')' returnType? metaAnnotation* block;

funcDecl: IDENT typeParams? '(' params ')' returnType? metaAnnotation* whereClause? block;
returnType: typeRef '!'?;
param: typeRef refMod? IDENT;
refMod: '&' 'mut'?;

enumDecl: 'enum' IDENT typeParams? metaAnnotation* '{' enumVariant (',' enumVariant)* ','? '}';
enumVariant: IDENT ('(' enumFields ')')?;
enumFields: enumField (',' enumField)*;
enumField: typeRef IDENT;

interfaceDecl: 'interface' IDENT typeParams? metaAnnotation* '{' interfaceMember* '}';
interfaceMember: IDENT '(' params ')' typeRef? ';';

// Type references
typeRef
    : typeRef '&' 'mut'?                // reference (postfix)
    | typeRef '*'                        // raw pointer (postfix)
    | '?' typeRef                        // optional
    | typeRef '[' ']'                    // slice
    | typeRef '[' INT_LITERAL ']'        // fixed array
    | IDENT typeArgs?                    // named type
    | '(' typeRef (',' typeRef)* ')'     // tuple
    | '(' typeRefList ')' '->' typeRef   // function type (arrow syntax)
    ;

typeArgs: '[' typeRef (',' typeRef)* ']';

lifetime: '\'' IDENT;
```

---

## 18. Compiler Implementation Plan (Go)

Single binary `promise` with the following internal packages:

| Package               | Responsibility                                    |
|-----------------------|---------------------------------------------------|
| `cmd/promise`         | CLI entry point (build, run, test, add, etc.)     |
| `parser/`             | ANTLR4-generated lexer/parser + parse tree visitor|
| `ast/`                | AST node definitions                              |
| `types/`              | Type system: the 4-struct model, generics, traits |
| `sema/`               | Semantic analysis: type checking, ownership check |
| `ownership/`          | Borrow checker implementation                     |
| `codegen/`            | LLVM IR generation via Go LLVM bindings           |
| `module/`             | Module resolution, dependency graph               |
| `meta/`               | Meta annotation processing                        |
| `pkg/`                | Package manager: fetch, resolve, lock, build      |

---

## 19. Package Manager (integrated into `promise` binary)

### Dependency Resolution

- Dependencies are discovered by scanning all `use` declarations in source files — no separate dependency manifest.
- Modules fetched from their URL (git clone / HTTP archive).
- Version segments in the URL are the version specifier — no separate version field.
- Lockfile (`promise.lock`) pins exact commit hashes.
- Diamond dependency resolution: if two deps require different versions of the same module, both are compiled separately (like Go's module system).

---

## 20. Open Design Questions

1. **Lifetime elision rules** — How aggressive should we be with lifetime inference? Rust's rules work well; should we adopt them verbatim or simplify further?

2. **Interface vs trait naming** — The proposal uses `interface` (Dart/Go-like). Should we use `trait` (Rust-like) instead for consistency with the ownership model?

3. **Operator overloading** — Should types be able to define `+`, `-`, `==` etc. via interface implementations?

4. **Async/await vs. goroutine model** — The concurrency section shows a spawn/channel model. Should we also support `async`/`await` syntax?

5. **Macro system** — Should compile-time meta annotations be able to generate code (procedural macros)?

6. **REPL** — Should the toolchain include an interpreter/REPL for rapid prototyping?

7. **Semicolons** — The proposal uses semicolons as statement terminators (Dart/C++ style). Should they be optional (inferred by newline, like Go)?

8. **Reference syntax** — The proposal uses `Type &name` in params and `Type&` as a type modifier. Should references be prefix (`&Type`) like Rust, or postfix (`Type&`) like C++? Current proposal uses postfix for consistency with the type-first declaration style.
