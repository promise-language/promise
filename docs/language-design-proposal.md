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
│   │   └── account.pr
│   └── utils/
│       ├── promise.mod
│       └── strings.pr
└── test/
    └── models/
        └── user_test.pr
```

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

```
module github.com/acme/myapp/1

require {
  github.com/std/io/1
  github.com/acme/collections/2/1
}
```

### 4.3 `use` Declarations

At the top of any `.pr` file, `use` imports a module with a local alias. The URL is a **string literal**:

```promise
use io "github.com/std/io/1"
use col "github.com/acme/collections/2/1"

fn main() {
  io.println("hello")
  col.List[Int] list = col.List[Int]()
}
```

The identifier before the URL is **mandatory** and is the only way to reference that module's exports in the file.

### 4.4 Visibility

All declarations are **public** by default. Use `` `public `` meta annotation to explicitly mark something as public (reserved for future use when private-by-default is introduced).

For now, everything is public. In a future revision, declarations will be **private by default**, and `` `public `` will be required to export them.

---

## 5. Type System

### 5.1 Primitive Types — Defined as Regular Types

Promise does **not** have a separate namespace for primitive types. Instead, primitives are defined as regular `type` declarations whose fields are annotated with `` `raw `` to map directly to LLVM types. This means `Int`, `Float64`, `Bool`, etc. are all types in the standard library, not compiler magic.

#### Standard Library Primitive Definitions

```promise
type Int {
  `raw("i64") `value
  i Int;
}

type Int8 {
  `raw("i8") `value
  i Int8;
}

type Int16 {
  `raw("i16") `value
  i Int16;
}

type Int32 {
  `raw("i32") `value
  i Int32;
}

type Int64 {
  `raw("i64") `value
  i Int64;
}

type UInt {
  `raw("i64") `value
  i UInt;
}

type UInt8 {
  `raw("i8") `value
  i UInt8;
}

type UInt16 {
  `raw("i16") `value
  i UInt16;
}

type UInt32 {
  `raw("i32") `value
  i UInt32;
}

type UInt64 {
  `raw("i64") `value
  i UInt64;
}

type Float32 {
  `raw("float") `value
  f Float32;
}

type Float64 {
  `raw("double") `value
  f Float64;
}

type Bool {
  `raw("i1") `value
  b Bool;
}
```

The `` `raw("llvm_type") `` meta tells the compiler this field maps directly to the specified LLVM IR type. The `` `value `` meta places the field in the Value struct (see Section 5.2). This unifies the type system — there is no distinction between "primitive" and "user-defined" types.

### 5.2 The Four-Struct Model

Every type declaration `T` produces four LLVM structs at compile time. These structs form a linked chain: **Value → Instance → Variant → Type**.

#### 1. **Value Struct** (`T_val`)
- Contains the data fields (those with no struct annotation, or explicitly annotated `` `value ``).
- **Always has a pointer to the Instance struct** that owns it (may be optimized out later).
- **Always copied** on assignment (value semantics).
- For types with ownership fields, a copy performs a deep clone (or is disallowed if the type is not `Clone`).

#### 2. **Instance Struct** (`T_inst`)
- Contains a **pointer to its Value struct** (heap-allocated data) and a **pointer to its Variant**.
- This is the default representation when you use a type — the standard "object".
- Fields annotated with no meta (default) live here in the instance struct.
- Subject to ownership rules.

#### 3. **Variant Struct** (`T_variant`)
- Represents **one concrete monomorphization** of a generic type (all generic parameters resolved).
- Contains the vtable, method pointers, resolved generic type info, and a **pointer to the Type struct**.
- Shared across all instances of `T[ConcreteG1, ConcreteG2]`.
- Created once per unique set of type arguments (interned at compile time).
- Fields annotated with `` `variant `` live here.

#### 4. **Type Struct** (`T_type`)
- Matches the source-code type **declaration** 1:1.
- Contains the unresolved/generic metadata: name, generic parameter descriptors, inheritance chain, field layout info, meta annotations.
- Used for reflection and compile-time meta-programming.
- One per `type` declaration in the source code.
- Fields annotated with `` `type `` live here.

#### The Pointer Chain

Every struct in the chain has a pointer to the next, making all four always reachable from any starting point:

```
T_val  ──ptr──▶  T_inst  ──ptr──▶  T_variant  ──ptr──▶  T_type
```

Given a value, you can always reach the instance, variant, and type. Given an instance, you can reach its value, variant, and type. And so on.

#### Diagram

```
┌──────────────────────────────────────────────────────────────────┐
│  T_type  (1 per source declaration)                              │
│  - name: "List"                                                  │
│  - generic_params: [E]                                           │
│  - fields: [...]                                                 │
│  - meta: [...]                                                   │
│  - parent_type: nullable                                         │
│  - `type fields live here                                        │
│                                                                  │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │ T_variant[Int]  (1 per monomorphization)                   │  │
│  │ - resolved_params: {E: Int}                                │  │
│  │ - vtable: [method_ptrs...]                                 │  │
│  │ - type_ptr: → T_type ◀──────────────────────────────────── │  │
│  │ - `variant fields live here                                │  │
│  │                                                            │  │
│  │  ┌──────────────────────────────────────────────────────┐  │  │
│  │  │ T_inst (1 per live object)                           │  │  │
│  │  │ - variant_ptr: → T_variant[Int] ◀──────────────────  │  │  │
│  │  │ - value_ptr: → T_val (see below)                     │  │  │
│  │  │ - default (unannotated) fields live here             │  │  │
│  │  │                                                      │  │  │
│  │  │  ┌────────────────────────────────────────────────┐  │  │  │
│  │  │  │ T_val  (the actual data, always copied)        │  │  │  │
│  │  │  │ - instance_ptr: → T_inst ◀──────────────────── │  │  │  │
│  │  │  │ - `value fields live here                      │  │  │  │
│  │  │  │ - items: [1, 2, 3]                             │  │  │  │
│  │  │  │ - count: 3                                     │  │  │  │
│  │  │  └────────────────────────────────────────────────┘  │  │  │
│  │  └──────────────────────────────────────────────────────┘  │  │
│  └────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
```

#### Field Placement Annotations

When defining a field in a type, the field goes into the **Instance struct** by default. Use meta annotations to place it elsewhere:

```promise
type Player {
  // Instance fields (default — no annotation needed)
  String name;
  Int health;

  // Value field — lives in T_val, copied with the value struct
  `value
  Float64 x;
  `value
  Float64 y;

  // Variant field — shared across all instances of this monomorphization
  `variant
  String spritePath;

  // Type field — shared across all instances of this type declaration
  `type
  String typeName;
}
```

#### Primitives in the Four-Struct Model

Since primitives are regular types, `Int` works like:

```promise
type Int {
  `raw("i64") `value
  i Int;    // The actual i64 value lives in Int_val
}
```

Resulting LLVM structs:
- `Int_val` = `{ i64, Int_inst* }` — the raw i64 plus a pointer to the instance
- `Int_inst` = `{ Int_val*, Int_variant* }` — pointers to value and variant
- `Int_variant` = `{ vtable, Int_type* }` — method dispatch + pointer to type
- `Int_type` = `{ metadata }` — name, reflection info

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

  fn area() Float64;
}

type Circle extends Shape {
  Float64 radius;

  fn area() Float64 {
    return 3.14159 * this.radius * this.radius;
  }
}

interface Drawable {
  fn draw(Canvas &canvas);
}

type Circle extends Shape impl Drawable {
  Float64 radius;

  fn area() Float64 { ... }

  fn draw(Canvas &canvas) {
    canvas.drawEllipse(this.x, this.y, this.radius);
  }
}
```

### 5.5 Generics

Generics use **square brackets** `[]`. Constraints are expressed with `where` clauses or inline bounds.

```promise
type Map[K: Hashable + Eq, V] {
  Bucket[K, V][] buckets;

  fn get(K &key) ?V& { ... }
  fn set(K key, V value) { ... }
}

// Alternative where-clause syntax:
fn sort[T](T[] &mut list) where T: Ord {
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
fn process(String &data) {           // shared borrow
  io.println(data);
}

fn modify(String &mut data) {        // mutable borrow
  data.append(" world");
}

fn consume(String data) {            // takes ownership
  // data is dropped at end of scope
}

fn main() {
  String s = String("hello");
  process(&s);          // borrow
  modify(&mut s);       // mutable borrow
  consume(s);           // move — s is no longer valid after this line
}
```

### 6.3 Lifetimes

Explicit lifetimes when the compiler cannot infer:

```promise
fn longest['a](String &'a a, String &'a b) String &'a {
  if a.len() > b.len() { return a; }
  return b;
}
```

### 6.4 Clone and Copy Traits

```promise
interface Copy {}      // Marker — bitwise copy is safe (primitives, small value types)
interface Clone {
  fn clone() Self;
}
```

Types that are `Copy` are implicitly copied on assignment. Others are moved.

---

## 7. Error Handling

### 7.1 The `!` Return Convention

Functions that can fail use `!` after the return type. Under the hood, this desugars to a result struct — a pair of `(value, error)`.

```promise
fn readFile(String &path) String! {
  // On success:
  return contents;

  // On failure:
  raise io.FileNotFoundError(path);
}
```

The `!` suffix on the return type means: "this function returns `(String, Error)`".

### 7.2 Calling Failable Functions

```promise
fn main() {
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
  fn message() String;
}

type FileNotFoundError {
  String path;
}

impl Error for FileNotFoundError {
  fn message() String {
    return "file not found: {this.path}";
  }
}
```

### 7.4 `raise` Statement

`raise` is used to return an error from a `!`-function. It is **not** an exception — it is sugar for returning the error half of the result struct.

```promise
fn divide(Float64 a, Float64 b) Float64! {
  if b == 0.0 {
    raise MathError("division by zero");
  }
  return a / b;
}
```

---

## 8. Meta Annotations (Backtick Attributes)

Backtick `` ` `` provides metadata on declarations.

### 8.1 Syntax

```
MetaAnnotation = '`' Identifier [ '(' MetaParams ')' ] ;
MetaParams     = MetaParam ( ',' MetaParam )* ;
MetaParam      = Expression | Identifier ':' Expression ;
```

### 8.2 Examples

```promise
`serializable
`version(2)
`deprecated(since: "1.3", message: "Use newMethod instead")
type OldThing {
  `json(name: "user_name")
  `required
  String name;

  `json(name: "user_age")
  Int age;
}

`inline
fn fastAdd(Int a, Int b) Int {
  return a + b;
}

`test
fn testAddition() {
  assert(fastAdd(1, 2) == 3);
}
```

### 8.3 Built-in Metas

| Meta          | Applies To     | Description                                      |
|---------------|----------------|--------------------------------------------------|
| `` `raw(t) `` | fields         | Map field directly to LLVM type `t`              |
| `` `value ``  | fields         | Place field in the Value struct                  |
| `` `variant ``| fields         | Place field in the Variant struct                |
| `` `type ``   | fields         | Place field in the Type struct                   |
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

### 9.1 Free Functions

```promise
fn greet(String &name) String {
  return "Hello, {name}!";
}
```

### 9.2 Methods

Methods are defined inside the type body. `this` is the receiver.

```promise
type Counter {
  Int value;

  fn increment(&mut this) {
    this.value += 1;
  }

  fn current(&this) Int {
    return this.value;
  }

  fn intoValue(this) Int {   // consumes self
    return this.value;
  }
}
```

### 9.3 Static Methods

```promise
type Counter {
  Int value;

  static fn new() Counter {
    return Counter(value: 0);
  }
}
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
fn find(Int id) ?User {         // shorthand for Option[User]
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
`unsafe
fn rawPointer() {
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
fn main() {
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

`serializable
type Todo {
  `json(name: "id")
  Int id;

  `json(name: "title")
  String title;

  Bool done;

  fn toggle(&mut this) {
    this.done = !this.done;
  }

  static fn new(Int id, String title) Todo {
    return Todo(id: id, title: title, done: false);
  }
}

type TodoList {
  Todo[] items;

  fn add(&mut this, String title) {
    Int id = this.items.len() + 1;
    this.items.push(Todo.new(id, title));
  }

  fn pending(&this) Todo&[] {
    return this.items.filter(|t| !t.done);
  }
}

fn loadFromFile(String &path) TodoList! {
  String content = io.readFile(path)?;
  Todo[] items = json.decode[Todo[]](content)?;
  return TodoList(items: items);
}

fn main() {
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
    : metaAnnotation* typeDecl
    | metaAnnotation* fnDecl
    | metaAnnotation* enumDecl
    | metaAnnotation* interfaceDecl
    ;

metaAnnotation: '`' IDENT ('(' metaParams ')')?;
metaParams: metaParam (',' metaParam)*;
metaParam: expression | IDENT ':' expression;

typeDecl
    : 'type' IDENT typeParams? ('extends' typeRef)? ('impl' typeRef (',' typeRef)*)?
      '{' typeMember* '}'
    ;

typeParams: '[' typeParam (',' typeParam)* ']';
typeParam: IDENT (':' typeConstraint)?;
typeConstraint: typeRef ('+' typeRef)*;

typeMember: metaAnnotation* (fieldDecl | methodDecl | staticMethodDecl);
fieldDecl: typeRef IDENT ';';
methodDecl: 'fn' IDENT '(' params ')' typeRef? block;
staticMethodDecl: 'static' 'fn' IDENT '(' params ')' typeRef? block;

fnDecl: 'fn' IDENT typeParams? '(' params ')' returnType? whereClause? block;
returnType: typeRef '!'?;
param: typeRef refMod? IDENT;
refMod: '&' 'mut'?;

enumDecl: 'enum' IDENT typeParams? '{' enumVariant (',' enumVariant)* ','? '}';
enumVariant: IDENT ('(' enumFields ')')?;
enumFields: enumField (',' enumField)*;
enumField: typeRef IDENT;

interfaceDecl: 'interface' IDENT typeParams? '{' interfaceMember* '}';
interfaceMember: 'fn' IDENT '(' params ')' typeRef? ';';

// Type references
typeRef
    : typeRef '&' 'mut'?                // reference (postfix)
    | typeRef '*'                        // raw pointer (postfix)
    | '?' typeRef                        // optional
    | typeRef '[' ']'                    // slice
    | typeRef '[' INT_LITERAL ']'        // fixed array
    | IDENT typeArgs?                    // named type
    | '(' typeRef (',' typeRef)* ')'     // tuple
    | 'fn' '(' typeRefList? ')' typeRef? // function type
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
