# Promise Programming Language — Design Proposal

## 1. Overview

**Promise** is a systems-aware, statically-typed programming language with Dart-inspired syntax, Rust-inspired ownership semantics, and a rich type system featuring inheritance, generics, and algebraic error handling. The toolchain (compiler + package manager) is a single Go binary called `promise`, uses ANTLR4 for parsing, and targets LLVM IR for code generation.

**No macros, no conditional compilation.** All code is fully defined and understandable in the source file — there are no preprocessor directives, procedural macros, or build flags that alter semantics. This is a deliberate design choice: Promise is intended for AI agents, where hidden code generation and flag-dependent behavior create costly inference overhead. It also maximizes compiled unit reuse — a compilation unit always produces the same output, so incremental builds stay fast.

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
├── main.pr                  # Entry point
├── helpers.pr               # Other source files at top level
├── models/
│   ├── promise.mod          # Separate module (excluded from myproject)
│   ├── user.pr
│   ├── user_test.pr         # Test file alongside source
│   └── account.pr
└── utils/
    ├── promise.mod          # Separate module (excluded from myproject)
    └── strings.pr
```

Source files (`.pr`) and directories live directly inside the module root — there is no required `src/` directory. This keeps the layout flat and avoids an extra level of nesting that adds no information. The module root is wherever `promise.mod` is.

### 3.1 Module Boundaries

There is no concept of "sub-modules." Every `promise.mod` file defines a standalone module. When the compiler scans a module's directory tree, any subdirectory that contains its own `promise.mod` is **excluded** — it is a separate module, not part of the parent. Directories without a `promise.mod` are just organizational folders whose `.pr` files belong to the enclosing module.

In the layout above, `myproject/`, `models/`, and `utils/` are three independent modules that happen to be nested on disk. The `myproject` module contains `main.pr` and `helpers.pr`. It does **not** contain anything inside `models/` or `utils/` — those are their own modules with their own identities, dependencies, and compilation scopes.

This means a `promise.mod` file serves exactly one purpose: **it marks the root of a module.** The compiler needs this marker because:

1. **No guessing.** Without it, the compiler cannot distinguish "this directory is a separate module" from "this directory just organizes files." An explicit marker removes ambiguity — you can freely create directories for readability without accidentally splitting your module.
2. **Independent compilation.** Each module is a separate compilation unit with its own dependency graph. The `promise.mod` file tells the compiler "start a new compilation scope here."
3. **Visibility boundaries.** Visibility rules apply at module boundaries. Without a `promise.mod`, a directory has no boundary — its files are part of the parent module and share its namespace.
4. **Tooling clarity.** Tools, IDEs, and AI agents identify module structure by scanning for `promise.mod` files. No heuristics, no configuration — the file system is the source of truth.

### 3.2 Testing Convention

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

The `promise test` command discovers and runs all `` `test ``-annotated functions across the project. Any declaration annotated with `` `test `` — functions, types, or anything else — is excluded from production builds. Entire `_test.pr` files are also excluded from production builds.

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

  +(Int other) Int `native;
  -(Int other) Int `native;
  *(Int other) Int `native;
  /(Int other) Int `native;
  %(Int other) Int `native;
  ==(Int other) Bool `native;
  !=(Int other) Bool `native;
  <(Int other) Bool `native;
  >(Int other) Bool `native;
  <=(Int other) Bool `native;
  >=(Int other) Bool `native;
  -() Int `native;                // unary negation
  toString() String `native;
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

#### Operator Overloading

Operators are ordinary methods whose name is the operator symbol. The compiler does **not** generate any built-in operator code — all operators for all types (including primitives) are defined as methods in the standard library.

Methods marked `` `native `` have no Promise body; the runtime/compiler backend provides the implementation directly (e.g. mapping `Int.+` to an LLVM `add` instruction).

Any user-defined type can define operators the same way:

```promise
type Vec2 {
  Float64 x `value;
  Float64 y `value;

  +(Vec2 other) Vec2 {
    return Vec2(x: this.x + other.x, y: this.y + other.y);
  }

  ==(Vec2 other) Bool {
    return this.x == other.x && this.y == other.y;
  }
}
```

Supported operator method names: `+`, `-`, `*`, `/`, `%`, `==`, `!=`, `<`, `>`, `<=`, `>=`. Unary operators use the same symbol with no parameters (e.g. `-() Int` for negation).

### 5.2 The Four-Struct Model

Every type declaration `T` produces four LLVM structs at compile time. These structs form a chain: **Value → Instance → Variant → Type**.

#### Allocation Model

- **Value struct** — the unit of passing. Contains a vtable pointer, an instance pointer, and explicitly `` `value ``-annotated fields. Allocated on the stack, embedded in other value/instance structs, or passed as function parameters and return values. Always copied on assignment (value semantics).
- **Instance struct** — always heap-allocated. This is the standard "object" representation.
- **Variant struct** — generated at compile time. One per unique monomorphization. Never dynamically allocated.
- **Type struct** — generated at compile time. One per `type` declaration. Never dynamically allocated.

#### 1. **Value Struct** (`T#v`)
- The **unit of passing** — all function parameters, return values, and variable bindings are value structs.
- Contains a **vtable pointer** that determines how fields and methods are dispatched (see Section 5.2.1).
- Contains a **pointer to the Instance struct** that owns it.
- Contains **only** fields explicitly annotated with `` `value ``. Unannotated fields go to the Instance struct.
- Fields in the value struct can be raw LLVM types (`` `raw `value ``) or Promise types (`` `value ``). When a Promise type is placed in the value struct, its own value struct is embedded (concatenated) inline — no pointer indirection.
- **Always copied** on assignment (value semantics).
- For types with ownership fields, a copy performs a deep clone (or is disallowed if the type is not `` `clone ``).

#### 2. **Instance Struct** (`T#i`)
- Contains the instance fields (unannotated / default) and a single **pointer to its Variant**.
- Always heap-allocated. This is the default representation when you use a type — the standard "object".
- Does **not** contain a pointer to its Value — values reference the instance, not the other way around.
- Subject to ownership rules.

#### 3. **Variant Struct** (`T#m`)
- Represents **one concrete monomorphization** of a generic type (all generic parameters resolved).
- Contains resolved generic type info and a **pointer to the Type struct**.
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
 │
 └── vtable_ptr ──▶  view-specific vtable (compile-time generated)
```

A value always points to its owning instance. The instance points to its variant. The variant points to its type. Given a value, you can reach all four structs by following pointers. The chain is one-directional — instance does not point back to value. The vtable pointer in the value struct points to a **view-specific vtable** (see Section 5.2.1) that is separate from the pointer chain.

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
│ - vtable_ptr: → view-specific vtable (see 5.2.1)             │
│ - instance_ptr: → T#i                                         │
│ - `value fields live here                                     │
│ - items: [1, 2, 3]                                            │
│ - count: 3                                                    │
└──────────────────────────────────────────────────────────────┘

    vtable_ptr ──▶
┌──────────────────────────────────────────────────────────────┐
│ Vtable  (compile-time generated, one per view)                │
│ - get_field_0: fn(T#v) -> FieldType                           │
│ - set_field_0: fn(T#v, FieldType)                             │
│ - method_0:    fn(T#v) -> ReturnType                          │
│ - ...                                                         │
└──────────────────────────────────────────────────────────────┘
```

#### 5.2.1 Vtable Dispatch Model

The vtable pointer in the value struct is the **sole mechanism** for field access and method dispatch. Every field produces a getter and setter slot in the vtable. Every method produces a method slot. The call site accesses fields and calls methods exclusively through the vtable — it never directly reads memory from the instance or value struct.

**Vtable structure for a type:**

```promise
type Animal {
  String name;
  Int age;
  speak() String `abstract;
}
```

The compiler generates:

```
Animal_vtable = {
  [0] get_name:  fn(Animal#v) -> String
  [1] set_name:  fn(Animal#v, String)
  [2] get_age:   fn(Animal#v) -> Int
  [3] set_age:   fn(Animal#v, Int)
  [4] speak:     fn(Animal#v) -> String
}
```

At the call site, `animal.name` compiles to `vtable[0](animal_value)` and `animal.speak()` compiles to `vtable[4](animal_value)`. The call site does not know or care whether `name` is a stored field or a computed getter — the vtable function handles it.

**Why fields go through the vtable:**

This decouples the call site from the concrete implementation. A parent type can declare a field `String name`, and a child type can satisfy it with either a stored field or a computed getter — the call site code is identical in both cases. This is what enables interfaces to declare data fields that concrete types can implement however they choose (see Section 5.4).

**Inheritance extends the vtable:**

```promise
type Dog is Animal {
  String breed;
  speak() String { return "Woof!"; }
}
```

Dog's vtable **starts with** Animal's layout and appends new slots:

```
Dog_vtable = {
  // Animal slots (same positions — prefix-compatible)
  [0] get_name:  fn(Dog#v) -> String      → Dog's name field getter
  [1] set_name:  fn(Dog#v, String)        → Dog's name field setter
  [2] get_age:   fn(Dog#v) -> Int         → Dog's age field getter
  [3] set_age:   fn(Dog#v, Int)           → Dog's age field setter
  [4] speak:     fn(Dog#v) -> String      → Dog.speak

  // Dog-specific slots (appended)
  [5] get_breed: fn(Dog#v) -> String      → Dog's breed field getter
  [6] set_breed: fn(Dog#v, String)        → Dog's breed field setter
}
```

For single inheritance, the child's vtable is prefix-compatible with the parent's — a Dog vtable pointer works wherever an Animal vtable pointer is expected, because slots 0–4 are at the same positions.

**Multiple inheritance — per-view vtables:**

When a type has multiple parents, the compiler generates a **separate vtable for each parent view**. This is necessary because different parents have different slot layouts.

```promise
type Named {
  String name;
  greet() String { return "Hi, I'm {this.name}"; }
}

type Audible {
  volume() Int `abstract;
  speak() String `abstract;
}

type Dog is Named, Audible {
  String breed;
  Int loudness;

  speak() String { return "Woof!"; }
  volume() Int { return this.loudness; }
}
```

The compiler generates three vtables:

```
Named_vtable_for_Dog = {
  [0] get_name  → Dog's name getter
  [1] set_name  → Dog's name setter
  [2] greet     → Named.greet (default impl, accesses name through vtable)
}

Audible_vtable_for_Dog = {
  [0] volume    → Dog.volume
  [1] speak     → Dog.speak
}

Dog_vtable = {
  [0] get_name  → ...
  [1] set_name  → ...
  [2] greet     → Named.greet
  [3] volume    → Dog.volume
  [4] speak     → Dog.speak
  [5] get_breed → ...
  [6] set_breed → ...
  [7] get_loudness → ...
  [8] set_loudness → ...
}
```

When a Dog value is passed where `Named` is expected, the vtable pointer in the value struct is set to `Named_vtable_for_Dog`. When passed where `Audible` is expected, it is set to `Audible_vtable_for_Dog`. The call site always sees the vtable layout it expects.

**Interfaces with data fields:**

Because fields are accessed through vtable getter/setter slots, an interface (a type with abstract methods) can also declare data fields. Concrete types satisfy the interface by providing the field — either as stored data or as computed getters/setters:

```promise
type Positioned {
  Float64 x;
  Float64 y;
  distanceTo(Positioned &other) Float64 {
    dx := this.x - other.x;
    dy := this.y - other.y;
    return math.sqrt(dx * dx + dy * dy);
  }
}

type Player is Positioned {
  // Stores x, y as real fields — vtable getters read from instance memory
  Float64 x;
  Float64 y;
  String name;
}

type CameraTarget is Positioned {
  // No stored x, y — computed from tracked entity
  Entity tracked;

  x() Float64 { return this.tracked.position().x; }
  y() Float64 { return this.tracked.position().y; }
}
```

Both satisfy `Positioned`. The call site `pos.x` compiles to `vtable.get_x(pos_value)` — it does not know or care whether `x` is a stored field or a computed property. The `distanceTo` default implementation works for both because it accesses `x` and `y` through the vtable.

**Optimization:**

The vtable dispatch model is the **semantic model** — all access conceptually goes through the vtable. In practice, when the compiler knows the concrete type at a call site, an optimization pass can:

- **Devirtualize**: replace vtable dispatch with direct function calls.
- **Inline field access**: replace `vtable.get_x(value)` with a direct memory load.
- **Elide the vtable pointer**: remove it entirely for non-polymorphic values.

The code structure prioritizes flexibility over performance. Optimizations are layered on top without compromising the model.

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
- `Int#v` = `{ vtable*, Int#i*, i64 }` — vtable pointer, instance pointer, and the raw i64 value
- `Int#i` = `{ Int#m* }` — pointer to variant (no pointer back to value)
- `Int#m` = `{ Int#t* }` — pointer to type (compile-time generated)
- `Int#t` = `{ metadata }` — name, reflection info (compile-time generated)

The Int vtable contains getter/setter for the raw value field plus all operator methods (`+`, `-`, `==`, etc.). For performance, the compiler will optimize away unnecessary indirection for primitives (e.g., `Int` on the stack is just an `i64` in practice, with the vtable/instance/variant/type pointers elided when not needed).

### 5.3 Variable Declarations

Variable declarations use **type-first** syntax (Dart/C++ style):

```promise
Int x = 42;
String name = "Alice";
Float64 pi = 3.14159;
List[Int] numbers = [1, 2, 3];
Map[String, Int] scores = {"alice": 100, "bob": 85};
```

Type inference with `:=`:

```promise
x := 42;                // inferred as Int
name := "Alice";        // inferred as String
```

### 5.4 Inheritance

A type declares its parent types with `is`. There is no distinction between inheritance and interface implementation — both use the same keyword. An interface is simply a type whose methods are all `` `abstract `` — it uses the same `is` keyword and the same vtable machinery.

```promise
type Shape {
  Float64 x;
  Float64 y;

  area() Float64 `abstract;
}

type Circle is Shape {
  Float64 radius;

  area() Float64 { return 3.14159 * this.radius * this.radius; }
}

type Drawable {
  draw(Canvas &canvas) `abstract;
}

type Circle is Shape, Drawable {
  Float64 radius;

  area() Float64 { ... }

  draw(Canvas &canvas) {
    canvas.drawEllipse(this.x, this.y, this.radius);
  }
}
```

#### How Inheritance Maps to Vtables

When a type inherits from a parent, the child's vtable **extends** the parent's layout — the parent's slots appear first at the same positions, and the child appends its own new slots (see Section 5.2.1). This means:

- **Single inheritance**: the child's vtable is prefix-compatible with the parent's. A Circle vtable pointer works wherever a Shape vtable pointer is expected.
- **Multiple inheritance**: the compiler generates a **per-view vtable** for each parent. When a Circle is passed where `Shape` is expected, the value carries a Shape-view vtable. When passed where `Drawable` is expected, it carries a Drawable-view vtable. Each call site sees the slot layout it expects.
- **Field inheritance**: parent fields become getter/setter slots in the vtable. A child can inherit them as stored fields, override them with computed getters/setters, or provide them from a completely different source — the parent's call sites are unaffected.
- **Default method implementations**: a parent type can provide method bodies. These become concrete function pointers in the child's vtable. The child can override them by providing its own implementation, which replaces the function pointer in the vtable slot.

### 5.5 Generics

Generics use **square brackets** `[]`. Constraints are expressed inline in the type parameter list.

```promise
type Map[K: Hashable + Eq, V] {
  Bucket[K, V][] buckets;

  get(K &key) ?V& `instance { ... }
  set(K key, V value) `instance { ... }
}

sort[T: Ord](T[] ~list) {
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
3. You can have **either** one mutable reference (`T~`) **or** any number of shared references (`T&`) — never both simultaneously.
4. References must not outlive their referent.

### 6.2 Syntax

```promise
process(String &data) {              // shared borrow
  io.println(data);
}

modify(String ~data) {               // mutable borrow
  data.append(" world");
}

consume(String data) {               // takes ownership
  // data is dropped at end of scope
}

main() {
  String s = String("hello");
  process(&s);          // borrow
  modify(~s);           // mutable borrow
  consume(s);           // move — s is no longer valid after this line
}
```

### 6.3 Lifetimes

The compiler uses **aggressive lifetime elision** — in practice, explicit lifetime annotations are almost never needed. The elision rules:

1. Each reference parameter gets its own lifetime.
2. If there is exactly one input reference, its lifetime is assigned to all output references.
3. If there is a `&this` or `~this`, its lifetime is assigned to all output references.
4. If multiple input lifetimes exist and none of the above rules apply, the compiler analyzes the function body to infer the relationship. Only when the body is ambiguous (e.g. conditionally returning one of multiple references) does the compiler require an explicit annotation.

```promise
// All of these are inferred — no annotations needed:
first(String &a, String &b) String& { return a; }  // inferred: output borrows from a
name(&this) String& { return this.name; }           // inferred: output borrows from this

// Rare case: compiler cannot determine which input the output borrows from.
// Explicit annotation required:
longest['a](String &'a a, String &'a b) String &'a {
  if a.len() > b.len() { return a; }
  return b;
}
```

### 6.4 Copy and Clone

`` `copy `` and `` `clone `` are built-in meta annotations that control assignment semantics:

```promise
// Bitwise copy — compiler verifies all fields are also `copy
type Point `copy {
  Float64 x `value;
  Float64 y `value;
}

// Auto-generated deep clone — compiler generates clone() Self method
type Document `clone {
  String title;
  String[] pages;
}

// Custom clone — just define the method, no meta needed
type Connection {
  Socket socket;

  clone() Self {
    return Connection(socket: this.socket.duplicate());
  }
}
```

- `` `copy ``: Bitwise copy on assignment (primitives, small value types). The compiler verifies all fields are themselves `` `copy ``. No method generated — the copy is a direct memory copy.
- `` `clone ``: The compiler auto-generates a `clone() Self` method that deep-copies all fields. If the type also defines an explicit `clone() Self` method, the explicit method takes precedence.
- Types that are `` `copy `` are implicitly copied on assignment. Others are moved.

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
type Error {
  message() String `abstract;
}

type FileNotFoundError is Error {
  String path;

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
| `` `abstract ``| methods        | Method has no body; must be implemented by subtypes |
| `` `native `` | methods         | Method has no Promise body; provided by the runtime/compiler backend |
| `` `copy ``  | types           | Bitwise copy on assignment; compiler verifies all fields are `copy |
| `` `clone `` | types           | Auto-generate `clone() Self` method (deep copy)   |
| `` `doc ``   | any             | AST-attached documentation (see Section 8.4)      |

User-defined metas are available through the type system at compile time for meta-programming and code generation.

### 8.4 Documentation (`` `doc ``)

`` `doc `` attaches documentation directly to the AST node. Unlike comments, `` `doc `` is preserved in the parsed tree, unambiguously bound to its declaration, and available to tooling, IDE support, and AI agents at compile time.

```promise
type HttpClient `doc("HTTP client with connection pooling and automatic retry.") {
  Int maxRetries `doc("Maximum number of retry attempts before failing.");
  Duration timeout `doc("Per-request timeout.");

  get(~this, String url) Response! `doc("Perform a GET request. Returns the response or an error.") `instance {
    ...
  }
}

divide(Float64 a, Float64 b) Float64! `doc("Divide a by b. Raises MathError on division by zero.") {
  if b == 0.0 {
    raise MathError("division by zero");
  }
  return a / b;
}
```

The parameter is a plain string. Tooling can extract structured sections (parameters, return values, errors) from the text by convention, but the language itself treats it as an opaque string. This keeps the meta simple while giving AI agents a reliable, parseable documentation source that is always in sync with the code structure.

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
    dx := this.x - other.x;
    dy := this.y - other.y;
    return math.sqrt(dx * dx + dy * dy);
  }
}
```

#### Instance Methods (`` `instance ``)

Instance methods receive a **pointer to the instance struct**. They can access instance fields but **not** `` `value `` fields (compile error). Use `&this` for shared borrow, `~this` for mutable borrow.

```promise
type Counter {
  Int value;

  increment(~this) `instance {
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
add := |Int a, Int b| -> Int { return a + b; };
doubled := list.map(|x| x * 2);
```

Closures capture by reference by default. Use `move` to capture by value:

```promise
String greeting = "hello";
closure := move |String name| -> String {
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
abs := if x >= 0 { x } else { -x };
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
label := match status {
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

// Infinite loop (no condition — Go style)
for {
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
(num, label) := pair;      // destructuring
```

---

## 13. Nullable Types

Promise does not have null. Optional values use `Option[T]`, with sugar `?T`:

```promise
find(Int id) ?User {            // shorthand for Option[User]
  // ...
  return none;                   // Option.None
}

user := find(42);
if user is Some(u) {
  io.println(u.name);
}

// Or with `?.` chaining
name := find(42)?.name;
```

---

## 14. Unsafe Code

Promise allows unsafe blocks for low-level operations:

```promise
rawPointer() `unsafe {
  ptr := unsafe {
    Int* raw = alloc[Int]();
    *raw = 42;
    raw
  };
}
```

---

## 15. Concurrency

Promise uses goroutine-style lightweight coroutines. The runtime multiplexes goroutines onto OS threads and transparently handles I/O scheduling — all blocking I/O calls automatically suspend the current goroutine and resume it when the operation completes. There is **no function coloring**: functions that perform I/O have normal signatures and look identical to pure functions.

### 15.1 Transparent I/O

Functions are never declared as "async". The runtime is the async engine — any function that performs I/O automatically yields the goroutine during the blocking operation:

```promise
// This function does I/O but has a normal signature.
// The runtime suspends the goroutine during httpGet, not the OS thread.
fetchUser(Int id) User! {
  data := httpGet("/users/{id}")?;
  return User.fromJson(data)?;
}

// Callers just call it normally:
user := fetchUser(42)?;
```

### 15.2 Explicit Concurrency with `go`

`go` is an **expression** that launches a goroutine and returns a `Task[T]`, where `T` is the result type of the block or call. The `<-` operator receives the result, suspending the current goroutine until it is ready.

```promise
// Fire-and-forget (Task[Void] result ignored)
go {
  logAnalytics(event);
};

// Value-returning task
task := go fetchUser(42);          // task : Task[User!]
user := <-task;                    // suspends until result ready

// Inline: launch + receive
user := <-go fetchUser(42);        // equivalent to "await"

// Fan out, fan in — structured concurrency
t1 := go fetchUser(id);
t2 := go fetchPosts(id);
t3 := go fetchComments(id);
user := <-t1;                      // all three ran concurrently
posts := <-t2;
comments := <-t3;
```

`Task[T]` is a runtime-internal type returned by `go` expressions — it never appears in function signatures. Concurrency is always a **caller-side decision**.

### 15.3 Channels

Channels are the primary synchronization primitive for streaming data between goroutines:

```promise
main() {
  ch := Channel[Int].new(capacity: 10);

  go {
    for i in 0..100 {
      ch.send(i);
    }
    ch.close();
  };

  for value in ch {
    io.println("{value}");
  }
}
```

The `<-` operator also works on channels: `value := <-ch;` receives the next value.

### 15.4 Ownership Across Goroutines

Ownership rules apply across goroutines — data is either **moved** into the goroutine or shared via `Arc[T]` (atomic reference counting):

```promise
main() {
  data := loadData();

  // data is moved into the goroutine — no longer valid in main
  go {
    process(data);
  };

  // Shared access requires Arc
  Arc[Config] config = Arc.new(loadConfig());
  go {
    serve(config.clone());
  };
}
```

---

## 16. Complete Example

```promise
use io "github.com/promise-lang/std/io/1"
use json "github.com/promise-lang/std/json/1"

type Todo `serializable {
  Int id `json(name: "id");
  String title `json(name: "title");

  Bool done;

  toggle(~this) `instance {
    this.done = !this.done;
  }

  new(Int id, String title) Todo `type {
    return Todo(id: id, title: title, done: false);
  }
}

type TodoList {
  Todo[] items;

  add(~this, String title) `instance {
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
    status := if todo.done { "done" } else { "    " };
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
    ;

metaAnnotation: '`' IDENT ('(' metaParams ')')?;
metaParams: metaParam (',' metaParam)*;
metaParam: expression | IDENT ':' expression;

typeDecl
    : 'type' IDENT typeParams? ('is' typeRef (',' typeRef)*)?
      metaAnnotation* '{' typeMember* '}'
    ;

typeParams: '[' typeParam (',' typeParam)* ']';
typeParam: IDENT (':' typeConstraint)?;
typeConstraint: typeRef ('+' typeRef)*;

typeMember: fieldDecl | methodDecl;
fieldDecl: typeRef metaAnnotation* ';';
methodDecl: IDENT '(' params ')' returnType? metaAnnotation* (block | ';');

funcDecl: IDENT typeParams? '(' params ')' returnType? metaAnnotation* block;
returnType: typeRef '!'?;
param: typeRef refMod? IDENT;
refMod: '&' | '~';

enumDecl: 'enum' IDENT typeParams? metaAnnotation* '{' enumVariant (',' enumVariant)* ','? '}';
enumVariant: IDENT ('(' enumFields ')')?;
enumFields: enumField (',' enumField)*;
enumField: typeRef IDENT;

// Abstract methods use `abstract meta — no separate interface construct
// methodDecl with `abstract has no block: IDENT '(' params ')' returnType? '`abstract' ';'

// Type references
typeRef
    : typeRef '&'                        // shared reference (postfix)
    | typeRef '~'                        // mutable reference (postfix)
    | typeRef '*'                        // raw pointer (postfix)
    | '?' typeRef                        // optional
    | typeRef '[' ']'                    // slice
    | typeRef '[' INT_LITERAL ']'        // fixed array
    | IDENT typeArgs?                    // named type
    | '(' typeRef (',' typeRef)* ')'     // tuple
    | '(' typeRefList ')' '->' typeRef   // function type (arrow syntax)
    ;

typeArgs: '[' typeRef (',' typeRef)* ']';

goExpr: 'go' (block | expression);    // returns Task[T]
receiveExpr: '<-' expression;          // receive from Task[T] or Channel[T]

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

1. **REPL** — Should the toolchain include an interpreter/REPL for rapid prototyping?
